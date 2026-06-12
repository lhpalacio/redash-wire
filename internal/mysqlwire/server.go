package mysqlwire

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/server"
	"github.com/lhpalacio/redash-wire/internal/redash"
)

const (
	maxConcurrentConns = 200
	handshakeTimeout   = 30 * time.Second
)

type Server struct {
	listenAddr   string
	redashClient redash.RedashAPI
	registry     redash.SourceRegistry
	logger       *slog.Logger
	username     string
	password     string
	mysqlServer  *server.Server
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
		// NewServer wires go-mysql's DefaultAuthenticationProvider, which performs
		// real mysql_native_password verification against the credential returned
		// by credentialAuthHandler.GetCredential (see auth.go).
		mysqlServer: server.NewServer(
			"8.0.0-redash-wire",
			mysql.DEFAULT_COLLATION_ID,
			mysql.AUTH_NATIVE_PASSWORD,
			nil, nil,
		),
	}
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.listenAddr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", s.listenAddr, err)
	}

	s.logger.Info("listening (mysql)", "addr", ln.Addr().String())

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

	logger := s.logger.With("remote_addr", conn.RemoteAddr().String(), "session_id", s.connSeq.Add(1))

	// Use the per-connection context so server shutdown cancels in-flight Redash polling.
	h := newHandler(connCtx, logger, s.redashClient, s.registry)

	// Bound the handshake/auth phase, then clear the deadline so an established but
	// idle connection is not torn down between queries.
	_ = conn.SetReadDeadline(time.Now().Add(handshakeTimeout))
	mysqlConn, err := s.mysqlServer.NewCustomizedConn(conn, &credentialAuthHandler{username: s.username, password: s.password}, h)
	if err != nil {
		logger.Error("mysql handshake failed", "error", err)
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	logger.Info("mysql client connected", "user", mysqlConn.GetUser())

	for !mysqlConn.Closed() {
		if err := mysqlConn.HandleCommand(); err != nil {
			logger.Info("mysql client disconnected")
			return
		}
	}
}
