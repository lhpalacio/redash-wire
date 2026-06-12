package pgwire

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
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
	return strings.ToLower(strings.TrimSpace(sqltext.Redact(sql)))
}

func BuildRowDescription(columns []redash.Column, rows []map[string]any) *pgproto3.RowDescription {
	fields := make([]pgproto3.FieldDescription, len(columns))
	for i, col := range columns {
		oid := RedashTypeToPgOID(col.Type)
		size := RedashTypeToPgSize(col.Type)

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
	dataRows := make([]pgproto3.DataRow, len(rows))
	for i, row := range rows {
		values := make([][]byte, len(columns))
		for j, col := range columns {
			val, ok := row[col.Name]
			if !ok || val == nil {
				values[j] = nil
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

	case "datetime":
		s := fmt.Sprintf("%v", val)
		s = strings.Replace(s, "T", " ", 1)
		if strings.HasSuffix(s, "Z") {
			s = s[:len(s)-1] + "+00"
		}
		return s

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
	buf, err := encode((&pgproto3.ErrorResponse{
		Severity: "ERROR",
		Code:     "XX000",
		Message:  msg,
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

	if strings.HasPrefix(lower, "set ") {
		return true
	}
	if strings.HasPrefix(lower, "show ") {
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

func isSimpleSelect(lower string) bool {
	if !strings.HasPrefix(lower, "select ") {
		return false
	}
	return !strings.Contains(lower, " from ")
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

func HandleLocalQuery(conn io.Writer, sql string, startupParams map[string]string, sources []redash.DataSource, listenAddr string) error {
	lower := normalize(sql)

	if strings.HasPrefix(lower, "set ") {
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
	if strings.HasPrefix(lower, "show ") {
		param := strings.TrimSpace(lower[5:])
		return handleShowCommand(conn, param)
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
		return SendSingleRowResult(conn, "pg_backend_pid", "1")
	}
	if strings.Contains(lower, "pg_postmaster_start_time") {
		return SendSingleRowResult(conn, "pg_postmaster_start_time", time.Now().UTC().Format("2006-01-02 15:04:05+00"))
	}

	// Use the same token match as IsLocalQuery so any query it routed here as local
	// (e.g. "FROM\npg_database") is dispatched consistently rather than dropped.
	if sqltext.ContainsToken(lower, "pg_database") {
		return handlePgDatabaseQuery(conn, sources)
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

func handleShowCommand(conn io.Writer, param string) error {
	values := map[string]string{
		"server_version":              "14.0",
		"server_encoding":             "UTF8",
		"client_encoding":             "UTF8",
		"lc_collate":                  "en_US.UTF-8",
		"lc_ctype":                    "en_US.UTF-8",
		"is_superuser":                "on",
		"session_authorization":       "redash",
		"standard_conforming_strings": "on",
		"timezone":                    "UTC",
		"datestyle":                   "ISO, MDY",
		"integer_datetimes":           "on",
		"max_identifier_length":       "63",
		"transaction_isolation":       "read committed",
		"search_path":                 "\"$user\", public",
	}

	if val, ok := values[param]; ok {
		return SendSingleRowResult(conn, param, val)
	}
	return SendSingleRowResult(conn, param, "")
}
