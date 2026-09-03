package pgwire

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/lhpalacio/redash-wire/internal/redash"
	"github.com/lhpalacio/redash-wire/internal/sqltext"
)

// normalize lowercases a statement after redacting string-literal and comment
// contents, so classification keyword/identifier matching can never fire inside
// a literal or comment.
func normalize(sql string) string {
	return strings.ToLower(strings.TrimSpace(sqltext.Postgres.Redact(sql)))
}

func BuildRowDescription(columns []redash.Column, rows []map[string]any) *pgproto3.RowDescription {
	fields := make([]pgproto3.FieldDescription, len(columns))
	for i, col := range columns {
		oid := RedashTypeToPgOID(col.Type)
		size := RedashTypeToPgSize(col.Type)

		// Redash's "datetime" says nothing about zones; the values do. A column
		// whose values all carry an offset is timestamptz, one whose values are
		// all naive is timestamp, and one the proxy cannot classify stays text.
		if col.Type == "datetime" {
			switch datetimeColumnKind(rows, col.Name) {
			case datetimeAware:
				oid = OidTimestampTZ
			case datetimeText:
				oid, size = OidText, -1
			}
		}

		// Promote a column to JSONB when EVERY non-null value is a JSON object/array,
		// so the advertised OID matches the JSON text BuildDataRows emits for those
		// values (regardless of the Redash-declared type). Sampling row 0 alone was
		// order-dependent, and a column mixing objects with scalars cannot be valid JSONB.
		if columnIsJSON(rows, col.Name) {
			oid = OidJSONB
			size = -1
		}

		fields[i] = pgproto3.FieldDescription{
			Name:                 []byte(col.Name),
			TableOID:             0,
			TableAttributeNumber: 0,
			DataTypeOID:          oid,
			DataTypeSize:         size,
			TypeModifier:         -1,
			Format:               0,
		}
	}
	return &pgproto3.RowDescription{Fields: fields}
}

func BuildDataRows(columns []redash.Column, rows []map[string]any) []pgproto3.DataRow {
	// Decided once per column so every value is rendered in the form the
	// advertised OID promises (see BuildRowDescription).
	kinds := make([]datetimeKind, len(columns))
	for j, col := range columns {
		if col.Type == "datetime" {
			kinds[j] = datetimeColumnKind(rows, col.Name)
		}
	}

	dataRows := make([]pgproto3.DataRow, len(rows))
	for i, row := range rows {
		values := make([][]byte, len(columns))
		for j, col := range columns {
			val, ok := row[col.Name]
			if !ok || val == nil {
				values[j] = nil
				continue
			}
			if col.Type == "datetime" {
				values[j] = []byte(formatDatetime(val, kinds[j]))
				continue
			}
			values[j] = []byte(formatValue(val, col.Type))
		}
		dataRows[i] = pgproto3.DataRow{Values: values}
	}
	return dataRows
}

func columnIsJSON(rows []map[string]any, name string) bool {
	anyComplex := false
	for _, row := range rows {
		v, ok := row[name]
		if !ok || v == nil {
			continue
		}
		switch v.(type) {
		case map[string]any, []any:
			anyComplex = true
		default:
			return false
		}
	}
	return anyComplex
}

func formatValue(val any, redashType string) string {
	switch val.(type) {
	case map[string]any, []any:
		b, err := json.Marshal(val)
		if err != nil {
			return fmt.Sprintf("%v", val)
		}
		return string(b)
	}

	switch redashType {
	case "boolean":
		switch v := val.(type) {
		case bool:
			if v {
				return "t"
			}
			return "f"
		case string:
			lower := strings.ToLower(v)
			if lower == "true" || lower == "1" || lower == "t" {
				return "t"
			}
			return "f"
		default:
			return fmt.Sprintf("%v", val)
		}

	case "integer":
		switch v := val.(type) {
		case json.Number:
			return v.String()
		case float64:
			return fmt.Sprintf("%.0f", v)
		default:
			return fmt.Sprintf("%v", val)
		}

	case "float":
		switch v := val.(type) {
		case json.Number:
			return v.String()
		default:
			return fmt.Sprintf("%v", val)
		}

	case "date":
		// A date OID (1082) must carry only the calendar date; strip any time or
		// timezone suffix Redash may include (e.g. "2024-01-15T00:00:00Z").
		s := fmt.Sprintf("%v", val)
		if i := strings.IndexAny(s, "T "); i >= 0 {
			s = s[:i]
		}
		return s

	default:
		return fmt.Sprintf("%v", val)
	}
}

// datetimeKind is how a Redash "datetime" column is presented. Redash's type
// says nothing about zones, but its values do: Python serializes a naive
// timestamp as 2024-01-15T10:30:00 and an aware one with an offset or Z.
type datetimeKind int

const (
	datetimeText  datetimeKind = iota // unparseable or mixed values: verbatim, as text
	datetimeNaive                     // no value carries a zone: timestamp (1114)
	datetimeAware                     // every value carries a zone: timestamptz (1184)
)

var (
	naiveLayouts = []string{"2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"}
	awareLayouts = []string{
		"2006-01-02T15:04:05Z07:00", "2006-01-02 15:04:05Z07:00",
		"2006-01-02T15:04:05Z0700", "2006-01-02 15:04:05Z0700",
		"2006-01-02T15:04:05Z07", "2006-01-02 15:04:05Z07",
	}
)

// parseDatetime classifies one value. Every layout also accepts fractional
// seconds after the seconds field.
func parseDatetime(val any) (time.Time, datetimeKind) {
	s, ok := val.(string)
	if !ok {
		return time.Time{}, datetimeText
	}
	for _, layout := range naiveLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, datetimeNaive
		}
	}
	for _, layout := range awareLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, datetimeAware
		}
	}
	return time.Time{}, datetimeText
}

// datetimeColumnKind classifies a column by all of its non-null values. A
// column that mixes naive and aware values, or holds something that is not a
// timestamp, cannot be typed honestly and is sent as text. With no values to
// judge by it is a plain timestamp.
func datetimeColumnKind(rows []map[string]any, name string) datetimeKind {
	kind := datetimeNaive
	seen := false
	for _, row := range rows {
		v, ok := row[name]
		if !ok || v == nil {
			continue
		}
		_, k := parseDatetime(v)
		if k == datetimeText || (seen && k != kind) {
			return datetimeText
		}
		kind, seen = k, true
	}
	return kind
}

// formatDatetime renders a value the way the server would under the TimeZone
// the proxy advertises (UTC): a timestamp as "2006-01-02 15:04:05[.ffffff]", a
// timestamptz normalized to UTC with the "+00" suffix. A text column passes
// its values through untouched.
func formatDatetime(val any, kind datetimeKind) string {
	t, k := parseDatetime(val)
	if kind == datetimeText || k == datetimeText {
		return formatValue(val, "")
	}
	if kind == datetimeAware {
		return t.UTC().Format("2006-01-02 15:04:05.999999") + "+00"
	}
	return t.Format("2006-01-02 15:04:05.999999")
}

func SendQueryResult(conn io.Writer, sql string, result *redash.QueryResult) error {
	tag := commandTag(sql, len(result.Rows))

	var buf []byte
	var err error

	if len(result.Columns) > 0 {
		buf, err = encode(BuildRowDescription(result.Columns, result.Rows).Encode(buf))
		if err != nil {
			return err
		}

		dataRows := BuildDataRows(result.Columns, result.Rows)
		for _, dr := range dataRows {
			buf, err = encode(dr.Encode(buf))
			if err != nil {
				return err
			}
		}
	}

	// Redash does not return an affected-row count, so for a write without
	// RETURNING the command tag's count is 0 regardless of what actually changed.
	// Surface that explicitly via a NOTICE rather than letting a client read "0"
	// as "no rows matched".
	if isWriteWithoutResultRows(sql, result) {
		buf, err = encode((&pgproto3.NoticeResponse{
			Severity: "NOTICE",
			Code:     "01000",
			Message:  "redash-wire: Redash does not report affected-row counts; the command tag count is not accurate for writes",
		}).Encode(buf))
		if err != nil {
			return err
		}
	}

	buf, err = encode((&pgproto3.CommandComplete{
		CommandTag: []byte(tag),
	}).Encode(buf))
	if err != nil {
		return err
	}

	buf, err = encode((&pgproto3.ReadyForQuery{TxStatus: 'I'}).Encode(buf))
	if err != nil {
		return err
	}

	_, err = conn.Write(buf)
	return err
}

func isWriteWithoutResultRows(sql string, result *redash.QueryResult) bool {
	lower := strings.ToLower(strings.TrimSpace(sql))
	isWrite := strings.HasPrefix(lower, "insert") ||
		strings.HasPrefix(lower, "update") ||
		strings.HasPrefix(lower, "delete")
	return isWrite && len(result.Rows) == 0
}

func commandTag(sql string, rowCount int) string {
	lower := strings.ToLower(strings.TrimSpace(sql))

	switch {
	case strings.HasPrefix(lower, "insert"):
		return fmt.Sprintf("INSERT 0 %d", rowCount)
	case strings.HasPrefix(lower, "update"):
		return fmt.Sprintf("UPDATE %d", rowCount)
	case strings.HasPrefix(lower, "delete"):
		return fmt.Sprintf("DELETE %d", rowCount)
	case strings.HasPrefix(lower, "create"):
		return "CREATE TABLE"
	case strings.HasPrefix(lower, "drop"):
		return "DROP TABLE"
	case strings.HasPrefix(lower, "alter"):
		return "ALTER TABLE"
	default:
		return fmt.Sprintf("SELECT %d", rowCount)
	}
}

func SendError(conn io.Writer, msg string) error {
	return SendErrorCode(conn, "XX000", msg, "")
}

// SendErrorCode writes an ErrorResponse with the given SQLSTATE and an optional
// hint, then the ReadyForQuery that ends the simple-query cycle.
func SendErrorCode(conn io.Writer, code, msg, hint string) error {
	buf, err := encode((&pgproto3.ErrorResponse{
		Severity: "ERROR",
		Code:     code,
		Message:  msg,
		Hint:     hint,
	}).Encode(nil))
	if err != nil {
		return err
	}

	buf, err = encode((&pgproto3.ReadyForQuery{TxStatus: 'I'}).Encode(buf))
	if err != nil {
		return err
	}

	_, err = conn.Write(buf)
	return err
}

// SendFatal writes a FATAL ErrorResponse and deliberately no ReadyForQuery: the
// connection is about to close, and telling the client it may send another query
// would be a lie. This is what a real Postgres server does when it shuts down
// under an established session.
func SendFatal(conn io.Writer, code, msg string) error {
	buf, err := encode((&pgproto3.ErrorResponse{
		Severity: "FATAL",
		Code:     code,
		Message:  msg,
	}).Encode(nil))
	if err != nil {
		return err
	}

	_, err = conn.Write(buf)
	return err
}

func SendEmptyQuery(conn io.Writer) error {
	buf, err := encode((&pgproto3.EmptyQueryResponse{}).Encode(nil))
	if err != nil {
		return err
	}
	buf, err = encode((&pgproto3.ReadyForQuery{TxStatus: 'I'}).Encode(buf))
	if err != nil {
		return err
	}
	_, err = conn.Write(buf)
	return err
}

func SendCommandComplete(conn io.Writer, tag string) error {
	buf, err := encode((&pgproto3.CommandComplete{CommandTag: []byte(tag)}).Encode(nil))
	if err != nil {
		return err
	}
	buf, err = encode((&pgproto3.ReadyForQuery{TxStatus: 'I'}).Encode(buf))
	if err != nil {
		return err
	}
	_, err = conn.Write(buf)
	return err
}

func SendSingleRowResult(conn io.Writer, colName, value string) error {
	buf, err := encode((&pgproto3.RowDescription{
		Fields: []pgproto3.FieldDescription{{
			Name:         []byte(colName),
			DataTypeOID:  OidText,
			DataTypeSize: -1,
			TypeModifier: -1,
			Format:       0,
		}},
	}).Encode(nil))
	if err != nil {
		return err
	}

	buf, err = encode((&pgproto3.DataRow{
		Values: [][]byte{[]byte(value)},
	}).Encode(buf))
	if err != nil {
		return err
	}

	buf, err = encode((&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")}).Encode(buf))
	if err != nil {
		return err
	}
	buf, err = encode((&pgproto3.ReadyForQuery{TxStatus: 'I'}).Encode(buf))
	if err != nil {
		return err
	}

	_, err = conn.Write(buf)
	return err
}

func SendReadyForQuery(conn io.Writer) error {
	buf, err := encode((&pgproto3.ReadyForQuery{TxStatus: 'I'}).Encode(nil))
	if err != nil {
		return err
	}
	_, err = conn.Write(buf)
	return err
}

func SendEmptyResult(conn io.Writer, columns []string) error {
	fields := make([]pgproto3.FieldDescription, len(columns))
	for i, name := range columns {
		fields[i] = pgproto3.FieldDescription{
			Name:         []byte(name),
			DataTypeOID:  OidText,
			DataTypeSize: -1,
			TypeModifier: -1,
			Format:       0,
		}
	}

	buf, err := encode((&pgproto3.RowDescription{Fields: fields}).Encode(nil))
	if err != nil {
		return err
	}
	buf, err = encode((&pgproto3.CommandComplete{CommandTag: []byte("SELECT 0")}).Encode(buf))
	if err != nil {
		return err
	}
	buf, err = encode((&pgproto3.ReadyForQuery{TxStatus: 'I'}).Encode(buf))
	if err != nil {
		return err
	}

	_, err = conn.Write(buf)
	return err
}

func IsLocalQuery(sql string) bool {
	lower := normalize(sql)

	if hasKeyword(lower, "set") || hasKeyword(lower, "show") {
		return true
	}
	if strings.HasPrefix(lower, "begin") || strings.HasPrefix(lower, "commit") || strings.HasPrefix(lower, "rollback") {
		return true
	}
	if strings.HasPrefix(lower, "deallocate") || strings.HasPrefix(lower, "close") || strings.HasPrefix(lower, "discard") {
		return true
	}

	if isSimpleSelect(lower) {
		functionPatterns := []string{
			"version()",
			"current_database()",
			"current_schema()",
			"current_schemas(",
			"current_user",
			"session_user",
			"inet_server_addr()",
			"inet_server_port()",
			"pg_is_in_recovery()",
			"pg_backend_pid()",
			"pg_postmaster_start_time()",
		}
		for _, pattern := range functionPatterns {
			if sqltext.ContainsToken(lower, pattern) {
				return true
			}
		}
	}

	if sqltext.ContainsToken(lower, "pg_database") {
		return true
	}

	return false
}

func IsCatalogQuery(sql string) bool {
	return containsPgCatalogRef(normalize(sql))
}

// isSimpleSelect reports a SELECT with no FROM anywhere: the shape of a scalar
// function call such as SELECT version(). Keywords are matched on any
// whitespace, so a newline after SELECT or before FROM is not special.
func isSimpleSelect(lower string) bool {
	return hasKeyword(lower, "select") && !sqltext.ContainsToken(lower, "from")
}

// hasKeyword reports whether lower opens with kw as a whole word followed by
// whitespace, so "set\nsearch_path" matches like "set search_path" and
// "settings" does not.
func hasKeyword(lower, kw string) bool {
	return strings.HasPrefix(lower, kw) && len(lower) > len(kw) && isSpace(lower[len(kw)])
}

func containsPgCatalogRef(lower string) bool {
	if sqltext.ContainsToken(lower, "pg_catalog.") || sqltext.ContainsToken(lower, "information_schema.") {
		return true
	}

	pgTables := []string{
		"pg_type", "pg_class", "pg_namespace", "pg_proc",
		"pg_attribute", "pg_attrdef", "pg_constraint", "pg_index",
		"pg_trigger", "pg_description", "pg_depend", "pg_extension",
		"pg_enum", "pg_inherits", "pg_sequence", "pg_collation",
		"pg_am", "pg_roles", "pg_user", "pg_views", "pg_tables",
		"pg_indexes", "pg_settings", "pg_database",
		"pg_available_extensions",
	}
	for _, t := range pgTables {
		if sqltext.ContainsToken(lower, t) {
			return true
		}
	}

	for _, prefix := range []string{"pg_statio_", "pg_stat_"} {
		if strings.Contains(lower, prefix) {
			return true
		}
	}
	return false
}

// LocalSession is what a locally answered query can see of the connection.
type LocalSession struct {
	StartupParams map[string]string
	Sources       []redash.DataSource
	ListenAddr    string
	// ReadOnly makes SHOW transaction_read_only answer on, and refuses the SET
	// and BEGIN forms that would ask for a read-write transaction.
	ReadOnly bool
	// BackendPID is the ProcessID the session sent in BackendKeyData, so
	// pg_backend_pid() names the same session a CancelRequest would.
	BackendPID uint32
}

// ReadOnlyHint is the hint attached to every read-only refusal, so a client (or
// an agent driving one) learns the mode belongs to the proxy, not the session.
const ReadOnlyHint = "redash-wire is running in read-only mode for this profile; only reads reach Redash. Set read_only: false in the config to allow writes."

func HandleLocalQuery(conn io.Writer, sql string, sess LocalSession) error {
	lower := normalize(sql)
	startupParams, sources, listenAddr := sess.StartupParams, sess.Sources, sess.ListenAddr

	// Real PostgreSQL lets a session switch a read-only default off. The proxy's
	// mode is the operator's, so the request is refused the way a hot standby
	// refuses it, rather than swallowed as SET usually is here: a silent OK would
	// have the next write fail for a reason the client believes it just removed.
	if sess.ReadOnly && asksForReadWrite(lower, sql) {
		return SendErrorCode(conn, SQLStateReadOnly, "cannot set transaction read-write mode while redash-wire is in read-only mode", ReadOnlyHint)
	}

	if hasKeyword(lower, "set") {
		return SendCommandComplete(conn, "SET")
	}
	if strings.HasPrefix(lower, "begin") {
		return SendCommandComplete(conn, "BEGIN")
	}
	if strings.HasPrefix(lower, "commit") {
		return SendCommandComplete(conn, "COMMIT")
	}
	if strings.HasPrefix(lower, "rollback") {
		return SendCommandComplete(conn, "ROLLBACK")
	}
	if strings.HasPrefix(lower, "deallocate") {
		return SendCommandComplete(conn, "DEALLOCATE")
	}
	if strings.HasPrefix(lower, "close") {
		return SendCommandComplete(conn, "CLOSE CURSOR")
	}
	if strings.HasPrefix(lower, "discard") {
		return SendCommandComplete(conn, "DISCARD ALL")
	}
	if hasKeyword(lower, "show") {
		// The statement arrives verbatim, so "SHOW x;" still carries its
		// terminator; it is not part of the parameter name.
		param := strings.TrimSpace(strings.TrimRight(lower[len("show"):], "; \t\r\n"))
		return handleShowCommand(conn, param, sess.ReadOnly)
	}

	// Catalog tables come before the scalar-function shortcuts: a pg_database
	// listing may mention current_user in its WHERE clause (has_database_privilege
	// does) and must still return the data sources, not the user. The token
	// match is the one IsLocalQuery uses, so any query it routed here (e.g.
	// "FROM\npg_database") is dispatched consistently rather than dropped.
	if sqltext.ContainsToken(lower, "pg_database") {
		return handlePgDatabaseQuery(conn, sources)
	}

	// The shortcuts answer a bare function call; a query that reads from a
	// relation is not one, whatever functions it applies.
	if !isSimpleSelect(lower) {
		return SendEmptyResult(conn, []string{"result"})
	}

	if strings.Contains(lower, "version()") {
		return SendSingleRowResult(conn, "version",
			"PostgreSQL 14.0 on redash-wire, compiled by redash-wire")
	}
	if strings.Contains(lower, "current_database()") {
		db := startupParams["database"]
		if db == "" {
			db = "redash"
		}
		return SendSingleRowResult(conn, "current_database", db)
	}
	if strings.Contains(lower, "current_schema()") {
		return SendSingleRowResult(conn, "current_schema", "public")
	}
	if strings.Contains(lower, "current_schemas(") {
		return SendSingleRowResult(conn, "current_schemas", "{public}")
	}
	if strings.Contains(lower, "current_user") {
		user := startupParams["user"]
		if user == "" {
			user = "redash"
		}
		return SendSingleRowResult(conn, "current_user", user)
	}
	if strings.Contains(lower, "session_user") {
		user := startupParams["user"]
		if user == "" {
			user = "redash"
		}
		return SendSingleRowResult(conn, "session_user", user)
	}
	if strings.Contains(lower, "inet_server_addr") {
		host, _, _ := net.SplitHostPort(listenAddr)
		if host == "" || host == "0.0.0.0" {
			host = "127.0.0.1"
		}
		return SendSingleRowResult(conn, "inet_server_addr", host)
	}
	if strings.Contains(lower, "inet_server_port") {
		_, port, _ := net.SplitHostPort(listenAddr)
		if port == "" {
			port = "15432"
		}
		return SendSingleRowResult(conn, "inet_server_port", port)
	}
	if strings.Contains(lower, "pg_is_in_recovery") {
		return SendSingleRowResult(conn, "pg_is_in_recovery", "f")
	}
	if strings.Contains(lower, "pg_backend_pid") {
		return SendSingleRowResult(conn, "pg_backend_pid", strconv.FormatUint(uint64(sess.BackendPID), 10))
	}
	if strings.Contains(lower, "pg_postmaster_start_time") {
		return SendSingleRowResult(conn, "pg_postmaster_start_time", time.Now().UTC().Format("2006-01-02 15:04:05+00"))
	}

	return SendEmptyResult(conn, []string{"result"})
}

func handlePgDatabaseQuery(conn io.Writer, sources []redash.DataSource) error {
	fields := []pgproto3.FieldDescription{
		{Name: []byte("datname"), DataTypeOID: OidText, DataTypeSize: -1, TypeModifier: -1, Format: 0},
		{Name: []byte("oid"), DataTypeOID: OidInt4, DataTypeSize: 4, TypeModifier: -1, Format: 0},
		{Name: []byte("datistemplate"), DataTypeOID: OidBool, DataTypeSize: 1, TypeModifier: -1, Format: 0},
		{Name: []byte("datallowconn"), DataTypeOID: OidBool, DataTypeSize: 1, TypeModifier: -1, Format: 0},
	}

	buf, err := encode((&pgproto3.RowDescription{Fields: fields}).Encode(nil))
	if err != nil {
		return err
	}

	for _, ds := range sources {
		buf, err = encode((&pgproto3.DataRow{
			Values: [][]byte{
				[]byte(ds.Name),
				[]byte(strconv.Itoa(ds.ID)),
				[]byte("f"),
				[]byte("t"),
			},
		}).Encode(buf))
		if err != nil {
			return err
		}
	}

	buf, err = encode((&pgproto3.CommandComplete{
		CommandTag: []byte(fmt.Sprintf("SELECT %d", len(sources))),
	}).Encode(buf))
	if err != nil {
		return err
	}
	buf, err = encode((&pgproto3.ReadyForQuery{TxStatus: 'I'}).Encode(buf))
	if err != nil {
		return err
	}

	_, err = conn.Write(buf)
	return err
}

// asksForReadWrite recognises the statements that would turn a read-only
// default off for the session: SET [SESSION] transaction_read_only /
// default_transaction_read_only to off, SET SESSION CHARACTERISTICS AS
// TRANSACTION READ WRITE, and BEGIN / START TRANSACTION READ WRITE. lower is
// the redacted, lowercased statement, which settles its shape; the value of a
// SET is read from raw, since it may be a quoted literal the redaction blanked.
func asksForReadWrite(lower, raw string) bool {
	fields := strings.Fields(strings.ReplaceAll(strings.ReplaceAll(lower, "=", " "), ";", " "))
	if len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case "set":
		if m := readOnlySetting.FindStringSubmatch(strings.ToLower(raw)); m != nil {
			switch m[1] {
			case "off", "false", "0", "no":
				return true
			}
		}
		return hasReadWrite(fields)
	case "begin", "start":
		return hasReadWrite(fields)
	}
	return false
}

// readOnlySetting matches the value given to transaction_read_only or
// default_transaction_read_only in a SET, with either "=" or TO and optional
// quotes: the forms psql, pgx and pgjdbc send.
var readOnlySetting = regexp.MustCompile(`(?:^|[^a-z0-9_])(?:default_)?transaction_read_only\s*(?:=|\bto\b)\s*'?([a-z0-9]+)'?`)

func hasReadWrite(fields []string) bool {
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == "read" && fields[i+1] == "write" {
			return true
		}
	}
	return false
}

func handleShowCommand(conn io.Writer, param string, readOnly bool) error {
	values := map[string]string{
		"server_version":                "14.0",
		"server_encoding":               "UTF8",
		"client_encoding":               "UTF8",
		"lc_collate":                    "en_US.UTF-8",
		"lc_ctype":                      "en_US.UTF-8",
		"is_superuser":                  "on",
		"session_authorization":         "redash",
		"standard_conforming_strings":   "on",
		"timezone":                      "UTC",
		"datestyle":                     "ISO, MDY",
		"integer_datetimes":             "on",
		"max_identifier_length":         "63",
		"transaction_isolation":         "read committed",
		"search_path":                   "\"$user\", public",
		"transaction_read_only":         onOff(readOnly),
		"default_transaction_read_only": onOff(readOnly),
	}

	if val, ok := values[param]; ok {
		return SendSingleRowResult(conn, param, val)
	}
	return SendSingleRowResult(conn, param, "")
}
