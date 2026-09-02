package proxy

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/lhpalacio/redash-wire/internal/health"
	"github.com/lhpalacio/redash-wire/internal/redash"
)

// maxConcurrentConns bounds the number of simultaneously-handled connections per
// listener, so a flood of connections cannot exhaust goroutines/file descriptors.
// Excess connections wait in the kernel accept queue (backpressure) rather than
// being dropped.
const maxConcurrentConns = 200

// handshakeTimeout bounds how long a client may take to complete startup/auth.
// It is cleared once the session is established, so legitimate long-lived idle
// connections are not affected; only slow/stalled handshakes are cut off.
const handshakeTimeout = 30 * time.Second

// Accept backoff bounds, the same ones net/http uses: a persistent Accept error
// such as fd exhaustion would otherwise spin the accept loop flat out.
const (
	acceptBackoffMin = 5 * time.Millisecond
	acceptBackoffMax = time.Second
)

type Server struct {
	listenAddr   string
	resolvedAddr string
	redashClient redash.RedashAPI
	registry     redash.SourceRegistry
	logger       *slog.Logger
	username     string
	password     string
	gate         *health.Gate
	readOnly     bool
	connSeq      atomic.Uint32

	// cancelers maps each session's ProcessID to the secret it was handed and a
	// hook that cancels its in-flight query. A PostgreSQL CancelRequest arrives on
	// a separate connection naming the (ProcessID, SecretKey) pair, so the server
	// needs a directory to find the session it refers to.
	mu        sync.Mutex
	cancelers map[uint32]sessionCanceler
}

type sessionCanceler struct {
	secret []byte
	cancel func()
}

type ServerOption func(*Server)

// WithGate makes the server refuse and drop sessions while Redash is unreachable.
// Without it the server serves unconditionally, which is what the tests that care
// about the wire protocol rather than about health want.
func WithGate(g *health.Gate) ServerOption {
	return func(s *Server) { s.gate = g }
}

// WithReadOnly makes every session refuse statements that are not reads before
// they reach Redash, and report the mode to clients that ask.
func WithReadOnly(on bool) ServerOption {
	return func(s *Server) { s.readOnly = on }
}

func NewServer(listenAddr string, logger *slog.Logger, redashClient redash.RedashAPI, registry redash.SourceRegistry, username, password string, opts ...ServerOption) *Server {
	s := &Server{
		listenAddr:   listenAddr,
		redashClient: redashClient,
		registry:     registry,
		logger:       logger,
		username:     username,
		password:     password,
		gate:         health.NewGate(),
		cancelers:    make(map[uint32]sessionCanceler),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.listenAddr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", s.listenAddr, err)
	}

	s.logger.Info("listening (postgres)", "event", health.EventListenerReady, "wire", "postgres", "addr", ln.Addr().String())

	return s.Serve(ctx, ln)
}

func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	s.resolvedAddr = ln.Addr().String()

	var wg sync.WaitGroup
	sem := make(chan struct{}, maxConcurrentConns)

	go func() {
		<-ctx.Done()
		if err := ln.Close(); err != nil {
			s.logger.Error("closing listener", "error", err)
		}
	}()

	var backoff time.Duration
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				wg.Wait()
				if ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("accepting connection: %w", err)
			}
			// A persistent Accept error such as EMFILE would otherwise spin this
			// loop flat out and flood the log. Back off the way net/http does; the
			// listener is still open, so a later Accept can succeed once the
			// condition clears.
			if backoff == 0 {
				backoff = acceptBackoffMin
			} else {
				backoff = min(2*backoff, acceptBackoffMax)
			}
			s.logger.Error("accepting connection", "error", err, "retry_in", backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
			}
			continue
		}
		backoff = 0

		sem <- struct{}{} // backpressure: blocks once maxConcurrentConns are in flight
		wg.Go(func() {
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					s.logger.Error("session panic", "error", r, "remote_addr", conn.RemoteAddr())
				}
			}()
			s.handleConn(ctx, conn)
		})
	}
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel()

	go func() {
		<-connCtx.Done()
		conn.Close()
	}()

	// Bound the handshake/auth phase so a client that connects and stalls cannot
	// hold the connection (and its goroutine) indefinitely. serve clears the
	// deadline once startup completes.
	_ = conn.SetReadDeadline(time.Now().Add(handshakeTimeout))

	// A per-session ProcessID/SecretKey. The ProcessID doubles as the session id in
	// the logs; the secret makes a CancelRequest unforgeable by another client.
	pid := s.connSeq.Add(1)
	secret := make([]byte, 4)
	_, _ = rand.Read(secret)

	logger := s.logger.With("remote_addr", conn.RemoteAddr().String(), "session_id", pid)
	session := newSession(conn, logger, s.redashClient, s.registry, s.gate, s.resolvedAddr, s.username, s.password, s.readOnly)
	session.backendKey = pgproto3.BackendKeyData{ProcessID: pid, SecretKey: secret}
	session.onCancel = s.cancelByKey

	s.registerCanceler(pid, secret, session.cancelInFlight)
	defer s.deregisterCanceler(pid)

	// Drive the session with the per-connection context so a server shutdown (and,
	// once the session returns, a client disconnect) cancels in-flight Redash polling.
	session.serve(connCtx)
}

func (s *Server) registerCanceler(pid uint32, secret []byte, cancel func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelers[pid] = sessionCanceler{secret: secret, cancel: cancel}
}

func (s *Server) deregisterCanceler(pid uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cancelers, pid)
}

// cancelByKey cancels the in-flight query of the session named by a CancelRequest,
// but only when the secret matches, so one client cannot cancel another's query by
// guessing a ProcessID. It is a no-op when nothing is in flight.
func (s *Server) cancelByKey(pid uint32, secret []byte) {
	s.mu.Lock()
	c, ok := s.cancelers[pid]
	s.mu.Unlock()
	if !ok {
		return
	}
	if subtle.ConstantTimeCompare(c.secret, secret) != 1 {
		s.logger.Info("ignoring cancel request with a bad secret", "session_id", pid)
		return
	}
	s.logger.Info("cancel request", "session_id", pid)
	c.cancel()
}
