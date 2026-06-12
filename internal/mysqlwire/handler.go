package mysqlwire

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/server"
	"github.com/lhpalacio/redash-wire/internal/redash"
	"github.com/lhpalacio/redash-wire/internal/sqltext"
)

type handler struct {
	ctx          context.Context
	redashClient redash.RedashAPI
	registry     redash.SourceRegistry
	dataSourceID int
	dbName       string
	logger       *slog.Logger

	schemaCache *redash.SchemaCache
}

var _ server.Handler = (*handler)(nil)

func newHandler(ctx context.Context, logger *slog.Logger, redashClient redash.RedashAPI, registry redash.SourceRegistry) *handler {
	return &handler{
		ctx:          ctx,
		redashClient: redashClient,
		registry:     registry,
		logger:       logger,
		schemaCache:  redash.NewSchemaCache(),
	}
}

func (h *handler) UseDB(dbName string) error {
	ds, ok := h.registry.Lookup(dbName)
	if !ok {
		return mysql.NewError(mysql.ER_BAD_DB_ERROR, fmt.Sprintf("Unknown database '%s'", dbName))
	}
	if !redash.IsMySQLCompatible(ds.Type) {
		return mysql.NewError(mysql.ER_BAD_DB_ERROR, fmt.Sprintf("database '%s' is not a MySQL-compatible data source", dbName))
	}
	h.dataSourceID = ds.ID
	h.dbName = ds.Name
	h.schemaCache.Reset()
	h.logger.Info("switched database", "database", ds.Name, "data_source_id", ds.ID)
	return nil
}

func (h *handler) HandleQuery(query string) (*mysql.Result, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}

	// We can only faithfully execute one statement per Redash call, so reject
	// batches explicitly instead of silently dropping all but the first.
	if sqltext.IsMultiStatement(query) {
		return nil, mysql.NewError(mysql.ER_UNKNOWN_ERROR, "multi-statement queries are not supported; send one statement per request")
	}

	h.logger.Debug("query received", "sql", query)

	if lower := strings.ToLower(query); strings.HasPrefix(lower, "use ") {
		dbName := strings.TrimSpace(query[4:])
		dbName = strings.Trim(dbName, "`;\"'")
		if err := h.UseDB(dbName); err != nil {
			return nil, err
		}
		return nil, nil
	}

	if isLocalQuery(query) {
		sources := redash.FilterByType(h.registry.All(), redash.IsMySQLCompatible)
		return handleLocalQuery(query, h.dbName, sources, h.getSchema())
	}

	if h.dataSourceID == 0 {
		return nil, mysql.NewError(
			mysql.ER_NO_DB_ERROR,
			"No database selected; use USE <database> (SHOW DATABASES to list available data sources)",
		)
	}

	query = h.stripDBQualifier(query)

	start := time.Now()
	result, err := h.redashClient.ExecuteQuery(h.ctx, query, h.dataSourceID)
	if err != nil {
		// SQL text is Debug-only (logged above); keep it out of the Error line.
		h.logger.Error("query execution failed", "error", err, "data_source_id", h.dataSourceID, "duration_ms", time.Since(start).Milliseconds())
		// Only surface genuine query errors; infrastructure errors may leak internal
		// hostnames/credentials, so replace them with a generic message.
		msg := "query execution failed (see proxy logs for details)"
		var qe *redash.QueryError
		if errors.As(err, &qe) {
			msg = qe.Message
		}
		return nil, mysql.NewError(mysql.ER_UNKNOWN_ERROR, msg)
	}

	h.logger.Info("query executed", "data_source_id", h.dataSourceID, "rows", len(result.Rows), "duration_ms", time.Since(start).Milliseconds())
	return buildResult(query, result)
}

func (h *handler) getSchema() []redash.SchemaTable {
	if h.dataSourceID == 0 {
		return nil
	}

	schema, err := h.schemaCache.Get(func() ([]redash.SchemaTable, error) {
		return h.redashClient.GetSchema(h.ctx, h.dataSourceID)
	})
	if err != nil {
		h.logger.Warn("fetching schema from Redash", "error", err)
		return nil
	}
	return schema
}

func (h *handler) stripDBQualifier(query string) string {
	if h.dbName == "" {
		return query
	}
	// The qualifier is itself a quoted identifier (`db`. or "db".), so identifier
	// quotes must be eligible for replacement; only single-quoted string values
	// are protected.
	query = sqltext.ReplaceOutsideStrings(query, "`"+h.dbName+"`.", "")
	query = sqltext.ReplaceOutsideStrings(query, "\""+h.dbName+"\".", "")
	return query
}

func (h *handler) HandleFieldList(table string, fieldWildcard string) ([]*mysql.Field, error) {
	return nil, mysql.NewError(mysql.ER_UNKNOWN_ERROR, "COM_FIELD_LIST is not supported")
}

func (h *handler) HandleStmtPrepare(query string) (int, int, any, error) {
	return 0, 0, nil, mysql.NewError(mysql.ER_UNKNOWN_ERROR, "prepared statements are not supported")
}

func (h *handler) HandleStmtExecute(context any, query string, args []any) (*mysql.Result, error) {
	return nil, mysql.NewError(mysql.ER_UNKNOWN_ERROR, "prepared statements are not supported")
}

func (h *handler) HandleStmtClose(context any) error {
	return nil
}

func (h *handler) HandleOtherCommand(cmd byte, data []byte) error {
	return mysql.NewError(mysql.ER_UNKNOWN_COM_ERROR, fmt.Sprintf("unsupported command: %d", cmd))
}
