package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

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

type Server struct {
	listenAddr   string
	resolvedAddr string
	redashClient redash.RedashAPI
	registry     redash.SourceRegistry
	logger       *slog.Logger
	username     string
	password     string
	connSeq      atomic.Int64
}

func NewServer(listenAddr string, logger *slog.Logger, redashClient redash.RedashAPI, registry redash.SourceRegistry, username, password string) *Server {
	return &Server{
		listenAddr:   listenAddr,
		redashClient: redashClient,
		registry:     registry,
		logger:       logger,
		username:     username,
		password:     password,
	}
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.listenAddr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", s.listenAddr, err)
	}

	s.logger.Info("listening (postgres)", "addr", ln.Addr().String())

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

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				wg.Wait()
				return nil
			default:
				s.logger.Error("accepting connection", "error", err)
				continue
			}
		}

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

	logger := s.logger.With("remote_addr", conn.RemoteAddr().String(), "session_id", s.connSeq.Add(1))
	session := newSession(conn, logger, s.redashClient, s.registry, s.resolvedAddr, s.username, s.password)
	// Drive the session with the per-connection context so a server shutdown (and,
	// once the session returns, a client disconnect) cancels in-flight Redash polling.
	session.serve(connCtx)
}
