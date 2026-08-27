package proxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/lhpalacio/redash-wire/internal/health"
	"github.com/lhpalacio/redash-wire/internal/pgwire"
	"github.com/lhpalacio/redash-wire/internal/redash"
	"github.com/lhpalacio/redash-wire/internal/sqltext"
)

type Session struct {
	conn         net.Conn
	backend      *pgproto3.Backend
	redashClient redash.RedashAPI
	registry     redash.SourceRegistry
	gate         *health.Gate
	listenAddr   string
	username     string
	password     string
	dataSourceID int
	dsType       string
	logger       *slog.Logger
	params       map[string]string

	schemaCache *redash.SchemaCache
}

func newSession(conn net.Conn, logger *slog.Logger, redashClient redash.RedashAPI, registry redash.SourceRegistry, gate *health.Gate, listenAddr, username, password string) *Session {
	backend := pgproto3.NewBackend(conn, conn)
	backend.SetMaxBodyLen(pgwire.MaxClientMessageBytes)
	return &Session{
		conn:         conn,
		backend:      backend,
		redashClient: redashClient,
		registry:     registry,
		gate:         gate,
		listenAddr:   listenAddr,
		username:     username,
		password:     password,
		logger:       logger,
		schemaCache:  redash.NewSchemaCache(),
	}
}

func (s *Session) serve(ctx context.Context) {
	params, err := pgwire.HandleStartup(s.backend, s.conn, s.username, s.password, s.admit)
	if err != nil {
		switch {
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

	// Interrupts the blocked Receive below when Redash goes away, so this
	// goroutine regains control and sends the FATAL itself.
	watch := health.InterruptOnDown(ctx, s.conn, s.gate)
	defer watch.Stop()

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
			// Checked after the disconnect cases so a client that left on its own
			// is not reported as a drop. What lands here after the gate closed is
			// the read deadline the watch armed to interrupt this very call.
			if watch.Dropped() {
				s.logger.Info("dropping session", "reason", "redash unavailable", "kind", s.gate.Status().Kind)
				if err := pgwire.SendFatal(s.conn, pgwire.SQLStateConnectionFailure, s.gate.ClientMessage()); err != nil {
					s.logger.Debug("sending drop notice to client", "error", err)
				}
				return
			}
			s.logger.Error("receiving message", "error", err)
			return
		}

		switch msg := msg.(type) {
		case *pgproto3.Query:
			s.handleQuery(ctx, msg.String)

		case *pgproto3.Terminate:
			s.logger.Info("client terminated")
			return

		case *pgproto3.Parse:
			if err := s.sendExtendedNotSupported(); err != nil {
				s.logger.Error("sending extended query error", "error", err)
				return
			}

		case *pgproto3.Sync:
			if err := pgwire.SendReadyForQuery(s.conn); err != nil {
				s.logger.Error("writing sync response", "error", err)
				return
			}

		default:
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
	if sqltext.IsMultiStatement(sql) {
		if err := pgwire.SendError(s.conn, "multi-statement queries are not supported; send one statement per request"); err != nil {
			s.logger.Error("sending error to client", "error", err)
		}
		return
	}

	s.logger.Debug("query received", "sql", sql)

	// Ahead of the catalog answers served from the cached registry and the
	// no-data-source reply. All of those are true and none of them is the reason
	// the query cannot run. Only reachable in the window between the gate closing
	// and the read being interrupted, since a gated proxy refuses at login.
	if !s.gate.Up() {
		if err := pgwire.SendError(s.conn, s.gate.ClientMessage()); err != nil {
			s.logger.Error("sending error to client", "error", err)
		}
		return
	}

	if pgwire.IsLocalQuery(sql) {
		sources := redash.FilterByType(s.registry.All(), redash.IsPostgresCompatible)
		if err := pgwire.HandleLocalQuery(s.conn, sql, s.params, sources, s.listenAddr); err != nil {
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

	if s.isPostgresBackend() {
		sql = pgwire.RewriteTimestampComparisons(sql)
	}

	start := time.Now()
	result, err := s.redashClient.ExecuteQuery(ctx, sql, s.dataSourceID)
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

func (s *Session) sendExtendedNotSupported() error {
	return pgwire.SendError(s.conn, "extended query protocol is not supported")
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
