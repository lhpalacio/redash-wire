package mysqlwire

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/server"
	"github.com/lhpalacio/redash-wire/internal/health"
	"github.com/lhpalacio/redash-wire/internal/redash"
)

const (
	maxConcurrentConns = 200
	handshakeTimeout   = 30 * time.Second
	// Accept backoff bounds, the same ones net/http uses.
	acceptBackoffMin = 5 * time.Millisecond
	acceptBackoffMax = time.Second
)

type Server struct {
	listenAddr   string
	redashClient redash.RedashAPI
	registry     redash.SourceRegistry
	logger       *slog.Logger
	username     string
	password     string
	mysqlServer  *server.Server
	gate         *health.Gate
	conns        *connTable
	connSeq      atomic.Int64
}

type ServerOption func(*Server)

// WithGate makes the server refuse logins and answer queries with the reason
// while Redash is unreachable.
// Without it the server serves unconditionally, which is what the wire-protocol
// tests want.
func WithGate(g *health.Gate) ServerOption {
	return func(s *Server) { s.gate = g }
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
		conns:        newConnTable(),
		// NewServer wires go-mysql's DefaultAuthenticationProvider, which performs
		// real mysql_native_password verification against the credential returned
		// by credentialAuthHandler.GetCredential (see auth.go).
		mysqlServer: server.NewServer(
			serverVersion,
			mysql.DEFAULT_COLLATION_ID,
			mysql.AUTH_NATIVE_PASSWORD,
			nil, nil,
		),
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

	s.logger.Info("listening (mysql)", "event", health.EventListenerReady, "wire", "mysql", "addr", ln.Addr().String())

	return s.Serve(ctx, ln)
}

func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
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
			// loop flat out. Back off the way net/http does; the listener is still
			// open, so a later Accept can succeed once the condition clears.
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

	logger := s.logger.With("remote_addr", conn.RemoteAddr().String(), "session_id", s.connSeq.Add(1))

	// Use the per-connection context so server shutdown cancels in-flight Redash polling.
	h := newHandler(connCtx, logger, s.redashClient, s.registry, s.gate, s.conns)

	// The gate and the database named in the handshake are only checked once the
	// password has been verified (see handler.login), so an unauthenticated
	// client learns nothing but "Access denied".
	auth := newCredentialAuthHandler(s.username, s.password, func(c *server.Conn) error {
		return h.login(c.ConnectionID())
	})

	// Bound the handshake/auth phase, then clear the deadline so an established but
	// idle connection is not torn down between queries.
	_ = conn.SetReadDeadline(time.Now().Add(handshakeTimeout))
	mysqlConn, err := s.mysqlServer.NewCustomizedConn(conn, auth, h)
	if err != nil {
		var myErr *mysql.MyError
		if errors.As(err, &myErr) {
			// A refusal the proxy chose to send (wrong password, unknown database,
			// Redash down) is an event, not a fault.
			logger.Info("mysql login refused", "code", myErr.Code, "error", myErr.Message)
		} else {
			logger.Error("mysql handshake failed", "error", err)
		}
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	connID := mysqlConn.ConnectionID()
	s.conns.add(connID, h, connCancel)
	defer s.conns.remove(connID)

	logger.Info("mysql client connected", "user", mysqlConn.GetUser(), "connection_id", connID)

	// A session that is open when Redash goes away keeps its socket: the handler
	// answers every command with the reason until the gate reopens, which is the
	// only place go-mysql lets the proxy speak on an established connection.
	for !mysqlConn.Closed() {
		if err := mysqlConn.HandleCommand(); err != nil {
			logger.Info("mysql client disconnected")
			return
		}
	}
}

// connTable is the proxy's answer to KILL: the sessions currently open, keyed
// by the connection id go-mysql handed each client in its handshake. Without
// it a KILL would be forwarded to Redash, where the id names some unrelated
// thread on the real server.
type connTable struct {
	mu    sync.Mutex
	conns map[uint32]liveConn
}

type liveConn struct {
	h *handler
	// disconnect cancels the connection's context, which closes its socket.
	disconnect context.CancelFunc
}

func newConnTable() *connTable {
	return &connTable{conns: make(map[uint32]liveConn)}
}

func (t *connTable) add(id uint32, h *handler, disconnect context.CancelFunc) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.conns[id] = liveConn{h: h, disconnect: disconnect}
}

func (t *connTable) remove(id uint32) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.conns, id)
}

func (t *connTable) lookup(id uint32) (liveConn, bool) {
	if t == nil {
		return liveConn{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	c, ok := t.conns[id]
	return c, ok
}
