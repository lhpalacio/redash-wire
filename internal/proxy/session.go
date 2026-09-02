package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/lhpalacio/redash-wire/internal/health"
	"github.com/lhpalacio/redash-wire/internal/pgwire"
	"github.com/lhpalacio/redash-wire/internal/redash"
	"github.com/lhpalacio/redash-wire/internal/sqltext"
)

type Session struct {
	conn         *peekConn
	backend      *pgproto3.Backend
	redashClient redash.RedashAPI
	registry     redash.SourceRegistry
	gate         *health.Gate
	listenAddr   string
	username     string
	password     string
	readOnly     bool
	dataSourceID int
	dsType       string
	logger       *slog.Logger
	params       map[string]string

	// backendKey is the ProcessID/SecretKey the server assigned this session and
	// sends in BackendKeyData; onCancel lets a CancelRequest on another connection
	// reach the server's session directory.
	backendKey pgproto3.BackendKeyData
	onCancel   pgwire.CancelFunc

	// extendedErr is set once an extended-protocol message has been rejected, so
	// the rest of the pipeline up to the next Sync is discarded rather than
	// answered with an error apiece (which desynced pgjdbc/pgx).
	extendedErr bool

	// queryCancel cancels the query currently in flight (nil when idle). It is set
	// under mu so a CancelRequest or a mid-query disconnect can reach it from
	// another goroutine.
	mu          sync.Mutex
	queryCancel context.CancelFunc

	schemaCache *redash.SchemaCache
}

func newSession(conn net.Conn, logger *slog.Logger, redashClient redash.RedashAPI, registry redash.SourceRegistry, gate *health.Gate, listenAddr, username, password string, readOnly bool) *Session {
	pc := newPeekConn(conn)
	backend := pgproto3.NewBackend(pc, pc)
	backend.SetMaxBodyLen(pgwire.MaxClientMessageBytes)
	return &Session{
		conn:         pc,
		backend:      backend,
		redashClient: redashClient,
		registry:     registry,
		gate:         gate,
		listenAddr:   listenAddr,
		username:     username,
		password:     password,
		readOnly:     readOnly,
		logger:       logger,
		schemaCache:  redash.NewSchemaCache(),
	}
}

func (s *Session) serve(ctx context.Context) {
	params, err := pgwire.HandleStartup(s.backend, s.conn, s.username, s.password, s.admit, s.backendKey, s.onCancel, s.readOnly)
	if err != nil {
		switch {
		case errors.Is(err, pgwire.ErrCancelRequest):
			// A throwaway cancel connection; the request was already dispatched
			// inside HandleStartup. Nothing more to do but close.
			s.logger.Debug("handled cancel request")
		case errors.Is(err, pgwire.ErrAuthFailed):
			// Routine: a wrong password or a port scan. Not an operational error.
			s.logger.Info("authentication failed")
		case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), isConnClosed(err):
			s.logger.Debug("client closed connection during handshake")
		default:
			s.logger.Error("startup handshake failed", "error", err)
		}
		return
	}
	// Clear the handshake read deadline so an idle but valid client
	// connection is not torn down between queries.
	_ = s.conn.SetReadDeadline(time.Time{})
	s.params = params

	dbName := params["database"]
	if dbName == "" {
		dbName = params["user"]
	}

	if ds, ok := s.registry.Lookup(dbName); ok && redash.IsPostgresCompatible(ds.Type) {
		s.dataSourceID = ds.ID
		s.dsType = ds.Type
	}

	s.logger.Info("client connected",
		"user", params["user"],
		"database", dbName,
		"data_source_id", s.dataSourceID,
		"application_name", params["application_name"],
	)

	for {
		msg, err := s.backend.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || isConnClosed(err) {
				s.logger.Info("client disconnected")
				return
			}
			s.logger.Error("receiving message", "error", err)
			return
		}

		switch msg := msg.(type) {
		case *pgproto3.Query:
			s.extendedErr = false
			s.handleQuery(ctx, msg.String)

		case *pgproto3.Terminate:
			s.logger.Info("client terminated")
			return

		// The extended query protocol is not supported. Per the protocol, the
		// first offending message gets a single ErrorResponse; everything after it
		// is discarded until Sync, which then gets exactly one ReadyForQuery. The
		// old code answered each of Parse/Bind/Describe/Execute AND Sync, so a
		// pgjdbc/pgx pipeline saw four errors and five ReadyForQuery and desynced.
		case *pgproto3.Parse, *pgproto3.Bind, *pgproto3.Describe, *pgproto3.Execute, *pgproto3.Close, *pgproto3.Flush:
			if !s.extendedErr {
				s.extendedErr = true
				if err := s.sendExtendedError(); err != nil {
					s.logger.Error("sending extended query error", "error", err)
					return
				}
			}
			// else: still discarding this pipeline until its Sync.

		case *pgproto3.Sync:
			// Sync ends the pipeline: send exactly one ReadyForQuery and clear the
			// error latch, whether or not we rejected anything before it.
			s.extendedErr = false
			if err := pgwire.SendReadyForQuery(s.conn); err != nil {
				s.logger.Error("writing sync response", "error", err)
				return
			}

		default:
			if s.extendedErr {
				// Mid-pipeline after an error: swallow until Sync rather than
				// answering, which would add a stray ReadyForQuery.
				continue
			}
			s.logger.Warn("unsupported message type", "type", msg)
			if err := pgwire.SendError(s.conn, "unsupported message type"); err != nil {
				s.logger.Error("sending error to client", "error", err)
				return
			}
		}
	}
}

func (s *Session) handleQuery(ctx context.Context, sql string) {
	sql = strings.TrimSpace(sql)

	if sql == "" {
		if err := pgwire.SendEmptyQuery(s.conn); err != nil {
			s.logger.Error("sending empty query response", "error", err)
		}
		return
	}

	// A single simple-Query message may legally carry multiple statements. We can
	// only faithfully execute one per Redash call, so reject batches explicitly
	// rather than silently answering the first and dropping the rest.
	if sqltext.Postgres.IsMultiStatement(sql) {
		if err := pgwire.SendError(s.conn, "multi-statement queries are not supported; send one statement per request"); err != nil {
			s.logger.Error("sending error to client", "error", err)
		}
		return
	}

	s.logger.Debug("query received", "sql", sql)

	// Ahead of the catalog answers served from the cached registry and the
	// no-data-source reply. All of those are true and none of them is the reason
	// the query cannot run. This is also how a session that was open when Redash
	// went away learns about it: the socket stays, and every query is answered
	// with the reason until the gate reopens, so a false alarm costs one query
	// rather than a reconnect.
	if !s.gate.Up() {
		// A client actively trying to run a query while the gate is closed is a
		// reason to re-check now: it can shorten a long (rejected-key) backoff to
		// the next probe instead of the next timer tick.
		s.gate.Suspect()
		if err := pgwire.SendError(s.conn, s.gate.ClientMessage()); err != nil {
			s.logger.Error("sending error to client", "error", err)
		}
		return
	}

	if pgwire.IsLocalQuery(sql) {
		sources := redash.FilterByType(s.registry.All(), redash.IsPostgresCompatible)
		local := pgwire.LocalSession{StartupParams: s.params, Sources: sources, ListenAddr: s.listenAddr, ReadOnly: s.readOnly}
		if err := pgwire.HandleLocalQuery(s.conn, sql, local); err != nil {
			s.logger.Error("handling local query", "error", err)
		}
		return
	}

	if pgwire.IsCatalogQuery(sql) {
		sources := redash.FilterByType(s.registry.All(), redash.IsPostgresCompatible)
		schema := s.getSchema(ctx)
		if err := pgwire.HandleCatalogQuery(s.conn, sql, schema, sources); err != nil {
			s.logger.Error("handling catalog query", "error", err)
		}
		return
	}

	if s.dataSourceID == 0 {
		if err := pgwire.SendError(s.conn, "no data source selected; connect with a valid database name (use SELECT datname FROM pg_database to list available data sources)"); err != nil {
			s.logger.Error("sending error to client", "error", err)
		}
		return
	}

	// Last before the call to Redash, so the local answers above are untouched
	// and nothing that would write ever leaves the proxy. The refusal is the one
	// PostgreSQL gives under default_transaction_read_only, code included.
	if s.readOnly {
		if verb := sqltext.Postgres.WriteVerb(sql); verb != "" {
			s.logger.Info("refused write in read-only mode", "verb", verb, "data_source_id", s.dataSourceID)
			msg := fmt.Sprintf("cannot execute %s in a read-only transaction", verb)
			if err := pgwire.SendErrorCode(s.conn, pgwire.SQLStateReadOnly, msg, pgwire.ReadOnlyHint); err != nil {
				s.logger.Error("sending error to client", "error", err)
			}
			return
		}
	}

	if s.isPostgresBackend() {
		sql = pgwire.RewriteTimestampComparisons(sql)
	}

	start := time.Now()
	result, err := s.executeQuery(ctx, sql)
	if err != nil {
		// SQL text is intentionally Debug-only (logged above); the Error line carries
		// no query text, only what operators need to triage.
		s.logger.Error("query execution failed", "error", err, "data_source_id", s.dataSourceID, "duration_ms", time.Since(start).Milliseconds())
		// A query that died on infrastructure is better evidence than the health
		// timer has. Ask for a probe now; the probe, not this query, decides.
		if health.Suspicious(err) {
			s.gate.Suspect()
		}
		// Only surface genuine query errors to the client; infrastructure errors may
		// contain internal hostnames/credentials, so replace them with a generic message.
		msg := "query execution failed (see proxy logs for details)"
		var qe *redash.QueryError
		if errors.As(err, &qe) {
			msg = qe.Message
		}
		if sendErr := pgwire.SendError(s.conn, msg); sendErr != nil {
			s.logger.Error("sending error to client", "error", sendErr)
		}
		return
	}

	s.logger.Info("query executed", "data_source_id", s.dataSourceID, "rows", len(result.Rows), "duration_ms", time.Since(start).Milliseconds())

	if err := pgwire.SendQueryResult(s.conn, sql, result); err != nil {
		s.logger.Error("sending query result", "error", err)
	}
}

// admit turns away a valid login while Redash is unreachable. It runs inside the
// startup exchange rather than after it, because a connection the client already
// believes is open cannot be refused any more, only broken.
func (s *Session) admit() error {
	if s.gate.Up() {
		return nil
	}
	// Someone is trying to connect while the gate is closed. Ask for a probe now so
	// a long (rejected-key) backoff is shortened to the next check rather than the
	// next timer tick.
	s.gate.Suspect()
	return errors.New(s.gate.ClientMessage())
}

func (s *Session) isPostgresBackend() bool {
	return redash.IsPostgresCompatible(s.dsType)
}

func (s *Session) getSchema(ctx context.Context) []redash.SchemaTable {
	if s.dataSourceID == 0 {
		return nil
	}
	// Redash is known to be unreachable, so a fetch would sit on the HTTP client's
	// timeout before returning the empty schema we can hand back right now.
	if !s.gate.Up() {
		return nil
	}

	schema, err := s.schemaCache.Get(func() ([]redash.SchemaTable, error) {
		s.logger.Debug("fetching schema from Redash", "data_source_id", s.dataSourceID, "ds_type", s.dsType)
		return s.redashClient.GetSchema(ctx, s.dataSourceID)
	})
	if err != nil {
		s.logger.Warn("fetching schema from Redash failed", "error", err)
		return nil
	}
	return schema
}

// executeQuery runs one remote query under a cancellable context, watching the
// client socket for the duration. A mid-query disconnect (or a CancelRequest)
// cancels that context, so the poll loop stops and the Redash job is DELETEd
// instead of the proxy running the job to completion, decoding the result into a
// dead socket, and holding a semaphore slot the whole time.
func (s *Session) executeQuery(ctx context.Context, sql string) (*redash.QueryResult, error) {
	queryCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.queryCancel = cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.queryCancel = nil
		s.mu.Unlock()
		cancel()
	}()

	stop := s.watchDisconnect(cancel)
	defer stop()

	return s.redashClient.ExecuteQuery(queryCtx, sql, s.dataSourceID)
}

// cancelInFlight cancels the query currently in flight, if any. It is what a
// CancelRequest reaches through the server's session directory; a no-op when the
// session is idle.
func (s *Session) cancelInFlight() {
	s.mu.Lock()
	cancel := s.queryCancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// watchDisconnect reads from the client socket while a query is in flight. In the
// simple query protocol the client sends nothing until it has the result, so a
// read that ends in EOF/reset means the peer is gone: cancel the query. Any bytes
// that do arrive (an early Terminate, say) are pushed back for the main loop
// rather than consumed, so a normal client waiting for its result is undisturbed.
//
// stop() unblocks the read with a read deadline and waits for the goroutine, so it
// never leaks and never mistakes a query that simply finished for a disconnect.
func (s *Session) watchDisconnect(cancel context.CancelFunc) (stop func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 512)
		for {
			n, err := s.conn.readUnderlying(buf)
			if n > 0 {
				// Not ours to consume: hand it back to the protocol reader.
				s.conn.pushBack(buf[:n])
			}
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					return // stop() woke us; the query finished on its own.
				}
				cancel()
				return
			}
		}
	}()
	return func() {
		_ = s.conn.SetReadDeadline(time.Now().Add(-time.Second))
		<-done
		_ = s.conn.SetReadDeadline(time.Time{})
	}
}

// sendExtendedError writes a single ErrorResponse with no trailing ReadyForQuery.
// In the extended protocol the ReadyForQuery is owed to the pipeline's Sync, not
// to the message that failed, so emitting one here (as SendError does) is what
// produced the extra ReadyForQuery that desynced clients.
func (s *Session) sendExtendedError() error {
	buf, err := (&pgproto3.ErrorResponse{
		Severity: "ERROR",
		Code:     "0A000", // feature_not_supported
		Message:  "extended query protocol is not supported",
	}).Encode(nil)
	if err != nil {
		return err
	}
	_, err = s.conn.Write(buf)
	return err
}

// peekConn wraps a net.Conn so the disconnect watcher can read the socket
// (through readUnderlying) to notice the peer leaving, and push back any protocol
// bytes it reads so the pgproto3 backend still sees them on its next read.
type peekConn struct {
	net.Conn
	mu     sync.Mutex
	unread []byte
}

func newPeekConn(c net.Conn) *peekConn { return &peekConn{Conn: c} }

func (c *peekConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	if len(c.unread) > 0 {
		n := copy(p, c.unread)
		c.unread = c.unread[n:]
		if len(c.unread) == 0 {
			c.unread = nil
		}
		c.mu.Unlock()
		return n, nil
	}
	c.mu.Unlock()
	return c.Conn.Read(p)
}

// readUnderlying bypasses the pushed-back buffer, so the watcher never re-reads
// bytes it just handed back.
func (c *peekConn) readUnderlying(p []byte) (int, error) { return c.Conn.Read(p) }

func (c *peekConn) pushBack(b []byte) {
	c.mu.Lock()
	c.unread = append(append([]byte(nil), b...), c.unread...)
	c.mu.Unlock()
}

func isConnClosed(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, net.ErrClosed) {
		return true
	}
	return strings.Contains(err.Error(), "connection reset by peer")
}
