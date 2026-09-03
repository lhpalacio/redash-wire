package mysqlwire

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/server"
	"github.com/lhpalacio/redash-wire/internal/health"
	"github.com/lhpalacio/redash-wire/internal/redash"
	"github.com/lhpalacio/redash-wire/internal/sqltext"
)

type handler struct {
	ctx          context.Context
	redashClient redash.RedashAPI
	registry     redash.SourceRegistry
	gate         *health.Gate
	conns        *connTable
	readOnly     bool
	dataSourceID int
	dbName       string
	logger       *slog.Logger

	// authenticated flips in login, once go-mysql has verified the password.
	// Until then UseDB only records pendingDB, so nothing about data sources or
	// the gate reaches a client that has not proved who it is.
	authenticated bool
	pendingDB     string
	connID        uint32

	schemaCache *redash.SchemaCache

	// cancelQuery aborts the Redash query in flight on this session, if any. It
	// is set for the duration of ExecuteQuery and called by KILL QUERY from
	// another session, so it needs the mutex.
	mu          sync.Mutex
	cancelQuery context.CancelFunc
}

var _ server.Handler = (*handler)(nil)

func newHandler(ctx context.Context, logger *slog.Logger, redashClient redash.RedashAPI, registry redash.SourceRegistry, gate *health.Gate, conns *connTable, readOnly bool) *handler {
	return &handler{
		ctx:          ctx,
		redashClient: redashClient,
		registry:     registry,
		gate:         gate,
		conns:        conns,
		readOnly:     readOnly,
		logger:       logger,
		schemaCache:  redash.NewSchemaCache(),
	}
}

// readOnlyMessage is MySQL's own wording for error 1290 under --read-only, with
// the proxy's reason appended so a client (or an agent driving one) learns the
// mode is the proxy's and not something the session can switch off.
const readOnlyMessage = "The MySQL server is running with the --read-only option so it cannot execute this statement (redash-wire is in read-only mode for this profile; only reads reach Redash. Set read_only: false in the config to allow writes.)"

// login is the post-authentication half of the handshake; go-mysql runs it
// after the password check and before the OK packet, and sends its error to
// the client instead of the OK. This is where the connect-time refusals live:
// the gate, so a client that connects without -D is turned away while Redash
// is down rather than learning it on its first query; and the database named
// in the handshake, which go-mysql handed to UseDB before it had verified the
// password and which is validated only now, giving `mysql -D nonexistent` the
// same 1049 the real server sends once authentication has succeeded.
func (h *handler) login(connID uint32) error {
	h.authenticated = true
	h.connID = connID

	if !h.gate.Up() {
		h.logger.Info("refusing login", "reason", "redash unavailable", "kind", h.gate.Status().Kind)
		return mysql.NewError(mysql.ER_UNKNOWN_ERROR, h.gate.ClientMessage())
	}

	if h.pendingDB == "" {
		return nil
	}
	dbName := h.pendingDB
	h.pendingDB = ""
	return h.UseDB(dbName)
}

func (h *handler) UseDB(dbName string) error {
	// go-mysql calls UseDB while reading the handshake, before it has verified
	// the password. Answering then would let anyone enumerate data source names
	// and learn whether Redash is down; real MySQL says nothing but "Access
	// denied" until authentication succeeds. Record the name and let login
	// validate it.
	if !h.authenticated {
		h.pendingDB = dbName
		return nil
	}

	if !h.gate.Up() {
		h.logger.Info("refusing database selection", "reason", "redash unavailable", "kind", h.gate.Status().Kind)
		return mysql.NewError(mysql.ER_UNKNOWN_ERROR, h.gate.ClientMessage())
	}

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
	if sqltext.MySQL.IsMultiStatement(query) {
		return nil, mysql.NewError(mysql.ER_UNKNOWN_ERROR, "multi-statement queries are not supported; send one statement per request")
	}

	h.logger.Debug("query received", "sql", query)

	// KILL names one of this proxy's own connections and never goes to Redash,
	// where the id would pick out an unrelated thread on the real server. It is
	// answered even while the gate is closed: the query it aborts may be the one
	// stuck on the outage.
	if id, queryOnly, ok, err := parseKill(query); ok {
		if err != nil {
			return nil, err
		}
		return nil, h.handleKill(id, queryOnly)
	}

	// Ahead of everything else, including the no-database-selected reply and the
	// catalog answers served from the cached registry. All of those are true and
	// none of them is the reason the query cannot run.
	if !h.gate.Up() {
		return nil, mysql.NewError(mysql.ER_UNKNOWN_ERROR, h.gate.ClientMessage())
	}

	if lower := strings.ToLower(query); strings.HasPrefix(lower, "use ") {
		dbName := strings.TrimSpace(query[4:])
		dbName = strings.Trim(dbName, "`;\"'")
		if err := h.UseDB(dbName); err != nil {
			return nil, err
		}
		return nil, nil
	}

	if isLocalQuery(query) {
		return handleLocalQuery(query, localSession{
			dbName:   h.dbName,
			connID:   h.connID,
			sources:  redash.FilterByType(h.registry.All(), redash.IsMySQLCompatible),
			schema:   h.getSchema(),
			readOnly: h.readOnly,
		})
	}

	if h.dataSourceID == 0 {
		return nil, mysql.NewError(
			mysql.ER_NO_DB_ERROR,
			"No database selected; use USE <database> (SHOW DATABASES to list available data sources)",
		)
	}

	// Last before the call to Redash, so the local answers above are untouched
	// and nothing that would write ever leaves the proxy. The refusal is the one
	// a MySQL server started with --read-only gives, code included.
	if h.readOnly {
		if verb := sqltext.MySQL.WriteVerb(query); verb != "" {
			h.logger.Info("refused write in read-only mode", "verb", verb, "data_source_id", h.dataSourceID)
			return nil, mysql.NewError(mysql.ER_OPTION_PREVENTS_STATEMENT, readOnlyMessage)
		}
	}

	query = h.stripDBQualifier(query)

	start := time.Now()
	result, err := h.executeRemote(query)
	if err != nil {
		if errors.Is(err, errInterrupted) {
			h.logger.Info("query interrupted", "data_source_id", h.dataSourceID, "duration_ms", time.Since(start).Milliseconds())
			return nil, mysql.NewDefaultError(mysql.ER_QUERY_INTERRUPTED)
		}
		// SQL text is Debug-only (logged above); keep it out of the Error line.
		h.logger.Error("query execution failed", "error", err, "data_source_id", h.dataSourceID, "duration_ms", time.Since(start).Milliseconds())
		// A query that died on infrastructure is better evidence than the health
		// timer has. Ask for a probe now; the probe, not this query, decides.
		if health.Suspicious(err) {
			h.gate.Suspect()
		}
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

// errInterrupted is how executeRemote reports that KILL QUERY, not the client
// going away or the proxy shutting down, ended the query.
var errInterrupted = errors.New("query interrupted")

// executeRemote runs the query on Redash under a context that KILL QUERY from
// another session can cancel. The Redash client cancels the job on its side
// when the context ends, so the interrupted query stops costing anything.
func (h *handler) executeRemote(query string) (*redash.QueryResult, error) {
	qctx, cancel := context.WithCancel(h.ctx)
	defer cancel()

	h.mu.Lock()
	h.cancelQuery = cancel
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.cancelQuery = nil
		h.mu.Unlock()
	}()

	result, err := h.redashClient.ExecuteQuery(qctx, query, h.dataSourceID)
	if err != nil && qctx.Err() != nil && h.ctx.Err() == nil {
		return nil, errInterrupted
	}
	return result, err
}

// interrupt cancels the Redash query this session is waiting on, if there is
// one, and reports whether there was. KILL QUERY from another session lands here.
func (h *handler) interrupt() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cancelQuery == nil {
		return false
	}
	h.cancelQuery()
	return true
}

// parseKill recognises KILL [CONNECTION | QUERY] <id>. ok is false when the
// statement is not a KILL at all; err reports a KILL that does not parse.
func parseKill(query string) (id uint32, queryOnly bool, ok bool, err error) {
	fields := strings.Fields(strings.TrimSuffix(normalize(query), ";"))
	if len(fields) == 0 || fields[0] != "kill" {
		return 0, false, false, nil
	}
	args := fields[1:]
	if len(args) > 0 && (args[0] == "query" || args[0] == "connection") {
		queryOnly = args[0] == "query"
		args = args[1:]
	}
	if len(args) != 1 {
		return 0, false, true, mysql.NewError(mysql.ER_PARSE_ERROR, "You have an error in your SQL syntax; expected KILL [CONNECTION | QUERY] <id>")
	}
	n, perr := strconv.ParseUint(args[0], 10, 32)
	if perr != nil {
		return 0, false, true, mysql.NewError(mysql.ER_PARSE_ERROR, fmt.Sprintf("You have an error in your SQL syntax; KILL expects a connection id, got '%s'", args[0]))
	}
	return uint32(n), queryOnly, true, nil
}

// handleKill is what the mysql CLI's Ctrl-C reaches: it opens a fresh
// connection and sends KILL QUERY <its own id>. The id is one go-mysql
// assigned, so it is looked up among this proxy's sessions.
func (h *handler) handleKill(id uint32, queryOnly bool) error {
	target, ok := h.conns.lookup(id)
	if !ok {
		return mysql.NewDefaultError(mysql.ER_NO_SUCH_THREAD, id)
	}
	interrupted := target.h.interrupt()
	if queryOnly {
		h.logger.Info("kill query", "target_connection_id", id, "interrupted", interrupted)
		return nil
	}
	target.disconnect()
	h.logger.Info("kill connection", "target_connection_id", id, "interrupted", interrupted)
	return nil
}

func (h *handler) getSchema() []redash.SchemaTable {
	if h.dataSourceID == 0 {
		return nil
	}
	// Redash is known to be unreachable, so a fetch would sit on the HTTP client's
	// timeout before returning the empty schema we can hand back right now.
	if !h.gate.Up() {
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

// stripDBQualifier removes the current database's qualifier from table
// references, so `SELECT * FROM orders.users` reaches Redash as `SELECT * FROM
// users`: the data source name is not a schema the real server knows.
//
// Without a parser, only the positions that unambiguously introduce a table
// reference are rewritten: the identifier (quoted or not) right after FROM,
// JOIN, INTO and TABLE, and a leading UPDATE, DESCRIBE, DESC or EXPLAIN, which
// covers SELECT ... FROM, every JOIN form, INSERT/REPLACE INTO, UPDATE, DELETE
// FROM, CREATE/ALTER/DROP/TRUNCATE TABLE, SHOW CREATE TABLE, SHOW COLUMNS and
// SHOW INDEX FROM, and DESCRIBE. A qualifier anywhere else is left alone, in
// particular `x.col` column references: with a data source named orders,
// `orders`.id in a select list or ON clause is the table orders, and stripping
// it would change the query's meaning. Known limitations: the second and later
// tables of a comma-separated FROM list keep their qualifier, as does a
// three-part db.table.col column reference; and a qualifier right after the
// FROM inside EXTRACT/SUBSTRING/TRIM is stripped like a table's.
//
// A SHOW statement may also name the database on its own, as in SHOW TABLE
// STATUS FROM db or SHOW COLUMNS FROM t FROM db, which the real server would
// refuse as an unknown database. That FROM db (or IN db) is dropped: Redash's
// connection is already in the right database. The first FROM of SHOW COLUMNS,
// FIELDS, INDEX, INDEXES and KEYS names a table, so it is only ever rewritten
// as db.table.
func (h *handler) stripDBQualifier(query string) string {
	if h.dbName == "" {
		return query
	}
	// Keywords are located in the redacted text so one inside a literal or
	// comment is never acted on; the identifier itself is read from the original,
	// where a quoted one is still visible. Both share positions.
	red := strings.ToLower(sqltext.MySQL.Redact(query))
	first := len(red) - len(strings.TrimLeft(red, " \t\r\n"))
	isShow := tokenAt(red, first, "show")
	tableFirst := isShow && showNamesTableFirst(red, first+len("show"))
	fromSeen := 0

	var out strings.Builder
	copied := 0
	for i := 0; i < len(red); {
		kw := tableRefKeywordAt(red, i, first, isShow)
		if kw == "" {
			i++
			continue
		}
		kwStart := i
		i += len(kw)
		namesDatabase := false
		if kw == "from" || kw == "in" {
			fromSeen++
			namesDatabase = isShow && (!tableFirst || fromSeen != 1)
		}
		start := i
		for start < len(query) && strings.IndexByte(" \t\r\n", query[start]) >= 0 {
			start++
		}
		name, end := identifierAt(query, start)
		if !strings.EqualFold(name, h.dbName) {
			continue
		}
		switch {
		case end < len(query) && query[end] == '.':
			out.WriteString(query[copied:start])
			copied = end + 1
			i = end + 1
		case namesDatabase:
			// Drop the keyword, the name and the blank before them, so SHOW
			// TABLE STATUS FROM db ends where the real server's statement does.
			cut := kwStart
			for cut > copied && strings.IndexByte(" \t\r\n", query[cut-1]) >= 0 {
				cut--
			}
			out.WriteString(query[copied:cut])
			copied = end
			i = end
		}
	}
	out.WriteString(query[copied:])
	return out.String()
}

// showNamesTableFirst reports whether the SHOW statement whose keyword ends at
// red[i] is one of the forms whose first FROM (or IN) names a table rather
// than a database: SHOW [FULL] {COLUMNS | FIELDS} FROM tbl and SHOW {INDEX |
// INDEXES | KEYS} FROM tbl. Both take an optional second FROM db.
func showNamesTableFirst(red string, i int) bool {
	for _, w := range strings.Fields(red[i:]) {
		switch w {
		case "full", "extended":
			continue
		case "columns", "fields", "index", "indexes", "keys":
			return true
		}
		return false
	}
	return false
}

// tableRefKeywordAt returns the table-introducing keyword that starts at
// red[i], or "". UPDATE, DESCRIBE, DESC and EXPLAIN only count at the start of
// the statement (first), so the column after ON DUPLICATE KEY UPDATE and the
// DESC of an ORDER BY are not mistaken for a table. IN only counts in a SHOW
// statement, where it is a synonym for FROM.
func tableRefKeywordAt(red string, i, first int, isShow bool) string {
	for _, kw := range [...]string{"from", "join", "into", "table", "update", "describe", "desc", "explain", "in"} {
		if !tokenAt(red, i, kw) {
			continue
		}
		switch kw {
		case "update", "describe", "desc", "explain":
			if i != first {
				continue
			}
		case "in":
			if !isShow {
				continue
			}
		}
		return kw
	}
	return ""
}

// identifierAt reads the identifier at s[i]: a `quoted` or "quoted" one, or a
// run of identifier bytes. It returns the unquoted name and the index just past
// it; an empty name means there was no identifier there.
func identifierAt(s string, i int) (string, int) {
	if i >= len(s) {
		return "", i
	}
	if q := s[i]; q == '`' || q == '"' {
		if close := strings.IndexByte(s[i+1:], q); close >= 0 {
			return s[i+1 : i+1+close], i + 1 + close + 1
		}
		return "", i
	}
	end := i
	for end < len(s) && isIdentByte(s[end]) {
		end++
	}
	return s[i:end], end
}

func isIdentByte(b byte) bool {
	return b == '_' || b == '$' ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
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
