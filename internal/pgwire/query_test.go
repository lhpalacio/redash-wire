package pgwire

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/lhpalacio/redash-wire/internal/redash"
)

func TestFormatValue(t *testing.T) {
	tests := []struct {
		name       string
		val        any
		redashType string
		want       string
	}{
		{name: "bool true", val: true, redashType: "boolean", want: "t"},
		{name: "bool false", val: false, redashType: "boolean", want: "f"},
		{name: "string true", val: "true", redashType: "boolean", want: "t"},
		{name: "string false", val: "false", redashType: "boolean", want: "f"},
		{name: "string 1 as bool", val: "1", redashType: "boolean", want: "t"},

		{name: "json.Number int", val: json.Number("42"), redashType: "integer", want: "42"},
		{name: "float64 int", val: float64(42), redashType: "integer", want: "42"},

		{name: "json.Number float", val: json.Number("3.14"), redashType: "float", want: "3.14"},

		// Complex values (map/slice) are always JSON regardless of redashType.
		{name: "map value", val: map[string]any{"key": "val"}, redashType: "string", want: `{"key":"val"}`},
		{name: "slice value", val: []any{1, 2}, redashType: "string", want: `[1,2]`},

		{name: "plain string", val: "hello", redashType: "string", want: "hello"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatValue(tt.val, tt.redashType)
			if got != tt.want {
				t.Errorf("formatValue(%v, %q) = %q, want %q", tt.val, tt.redashType, got, tt.want)
			}
		})
	}
}

func TestCommandTag(t *testing.T) {
	tests := []struct {
		name     string
		sql      string
		rowCount int
		want     string
	}{
		{name: "SELECT", sql: "SELECT * FROM users", rowCount: 5, want: "SELECT 5"},
		{name: "INSERT", sql: "INSERT INTO users VALUES (1)", rowCount: 1, want: "INSERT 0 1"},
		{name: "UPDATE", sql: "UPDATE users SET name='x'", rowCount: 3, want: "UPDATE 3"},
		{name: "DELETE", sql: "DELETE FROM users WHERE id=1", rowCount: 1, want: "DELETE 1"},
		{name: "CREATE TABLE", sql: "CREATE TABLE foo (id int)", rowCount: 0, want: "CREATE TABLE"},
		{name: "DROP TABLE", sql: "DROP TABLE foo", rowCount: 0, want: "DROP TABLE"},
		{name: "ALTER TABLE", sql: "ALTER TABLE foo ADD col int", rowCount: 0, want: "ALTER TABLE"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := commandTag(tt.sql, tt.rowCount)
			if got != tt.want {
				t.Errorf("commandTag(%q, %d) = %q, want %q", tt.sql, tt.rowCount, got, tt.want)
			}
		})
	}
}

func TestIsLocalQuery(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want bool
	}{
		{name: "SET", sql: "SET search_path TO public", want: true},
		{name: "SET with newline after keyword", sql: "SET\nsearch_path TO public", want: true},
		{name: "SHOW", sql: "SHOW server_version", want: true},
		{name: "SHOW with tab after keyword", sql: "SHOW\tserver_version;", want: true},
		{name: "SELECT with newline before function", sql: "SELECT\nversion()", want: true},
		{name: "pg_database with function in WHERE", sql: "SELECT d.datname FROM pg_catalog.pg_database d WHERE has_database_privilege(current_user, d.datname, 'CONNECT')", want: true},
		{name: "BEGIN", sql: "BEGIN", want: true},
		{name: "COMMIT", sql: "COMMIT", want: true},
		{name: "ROLLBACK", sql: "ROLLBACK", want: true},
		{name: "DEALLOCATE", sql: "DEALLOCATE ALL", want: true},
		{name: "CLOSE", sql: "CLOSE cursor_name", want: true},
		{name: "DISCARD", sql: "DISCARD ALL", want: true},
		{name: "SELECT version()", sql: "SELECT version()", want: true},
		{name: "SELECT current_database()", sql: "SELECT current_database()", want: true},
		{name: "pg_database reference", sql: "SELECT * FROM pg_database", want: true},
		{name: "regular table", sql: "SELECT * FROM users", want: false},
		{name: "INSERT", sql: "INSERT INTO logs VALUES (1)", want: false},
		{name: "case insensitive set", sql: "set timezone='UTC'", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsLocalQuery(tt.sql)
			if got != tt.want {
				t.Errorf("IsLocalQuery(%q) = %v, want %v", tt.sql, got, tt.want)
			}
		})
	}
}

func TestIsCatalogQuery(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want bool
	}{
		{name: "pg_catalog.pg_type", sql: "SELECT * FROM pg_catalog.pg_type", want: true},
		{name: "pg_class", sql: "SELECT * FROM pg_class", want: true},
		{name: "information_schema", sql: "SELECT * FROM information_schema.tables", want: true},
		{name: "regular table", sql: "SELECT * FROM users", want: false},
		{name: "simple literal", sql: "SELECT 1", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsCatalogQuery(tt.sql)
			if got != tt.want {
				t.Errorf("IsCatalogQuery(%q) = %v, want %v", tt.sql, got, tt.want)
			}
		})
	}
}

func TestIsSimpleSelect(t *testing.T) {
	tests := []struct {
		name  string
		lower string
		want  bool
	}{
		{name: "function call", lower: "select version()", want: true},
		{name: "has FROM", lower: "select * from users", want: false},
		{name: "has FROM after newline", lower: "select version()\nfrom dual", want: false},
		{name: "newline after select", lower: "select\nversion()", want: true},
		{name: "not select", lower: "show something", want: false},
		{name: "selected is not select", lower: "selected version()", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isSimpleSelect(tt.lower)
			if got != tt.want {
				t.Errorf("isSimpleSelect(%q) = %v, want %v", tt.lower, got, tt.want)
			}
		})
	}
}

func newFrontend(buf *bytes.Buffer) *pgproto3.Frontend {
	return pgproto3.NewFrontend(buf, nil)
}

func receiveAll(t *testing.T, fe *pgproto3.Frontend) []pgproto3.BackendMessage {
	t.Helper()
	var msgs []pgproto3.BackendMessage
	for {
		msg, err := fe.Receive()
		if err != nil {
			break
		}
		msgs = append(msgs, msg)
	}
	return msgs
}

func TestSendQueryResult(t *testing.T) {
	var buf bytes.Buffer

	result := &redash.QueryResult{
		Columns: []redash.Column{
			{Name: "id", Type: "integer"},
			{Name: "name", Type: "string"},
		},
		Rows: []map[string]any{
			{"id": json.Number("1"), "name": "alice"},
			{"id": json.Number("2"), "name": "bob"},
		},
	}

	if err := SendQueryResult(&buf, "SELECT * FROM users", result); err != nil {
		t.Fatalf("SendQueryResult: %v", err)
	}

	fe := newFrontend(&buf)
	msgs := receiveAll(t, fe)

	if len(msgs) != 5 {
		t.Fatalf("got %d messages, want 5", len(msgs))
	}

	assertMsgType[*pgproto3.RowDescription](t, msgs[0], "msgs[0]")
	assertMsgType[*pgproto3.DataRow](t, msgs[1], "msgs[1]")
	assertMsgType[*pgproto3.DataRow](t, msgs[2], "msgs[2]")
	assertMsgType[*pgproto3.CommandComplete](t, msgs[3], "msgs[3]")
	assertMsgType[*pgproto3.ReadyForQuery](t, msgs[4], "msgs[4]")

	rd := msgs[0].(*pgproto3.RowDescription)
	if len(rd.Fields) != 2 {
		t.Errorf("RowDescription has %d fields, want 2", len(rd.Fields))
	}

	cc := msgs[3].(*pgproto3.CommandComplete)
	if string(cc.CommandTag) != "SELECT 2" {
		t.Errorf("CommandTag = %q, want %q", cc.CommandTag, "SELECT 2")
	}
}

func TestSendError(t *testing.T) {
	var buf bytes.Buffer

	if err := SendError(&buf, "something broke"); err != nil {
		t.Fatalf("SendError: %v", err)
	}

	fe := newFrontend(&buf)
	msgs := receiveAll(t, fe)

	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}

	assertMsgType[*pgproto3.ErrorResponse](t, msgs[0], "msgs[0]")
	assertMsgType[*pgproto3.ReadyForQuery](t, msgs[1], "msgs[1]")

	er := msgs[0].(*pgproto3.ErrorResponse)
	if er.Message != "something broke" {
		t.Errorf("ErrorResponse.Message = %q, want %q", er.Message, "something broke")
	}
}

func TestSendEmptyQuery(t *testing.T) {
	var buf bytes.Buffer

	if err := SendEmptyQuery(&buf); err != nil {
		t.Fatalf("SendEmptyQuery: %v", err)
	}

	fe := newFrontend(&buf)
	msgs := receiveAll(t, fe)

	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}

	assertMsgType[*pgproto3.EmptyQueryResponse](t, msgs[0], "msgs[0]")
	assertMsgType[*pgproto3.ReadyForQuery](t, msgs[1], "msgs[1]")
}

func TestHandleLocalQuery(t *testing.T) {
	startupParams := map[string]string{
		"user":     "testuser",
		"database": "mydb",
	}
	sources := []redash.DataSource{
		{ID: 1, Name: "mydb", Type: "pg"},
	}
	listenAddr := "127.0.0.1:15432"

	tests := []struct {
		name     string
		sql      string
		wantMsgs []string
	}{
		{
			name: "SET command",
			sql:  "SET client_encoding='UTF8'",
			wantMsgs: []string{
				"CommandComplete",
				"ReadyForQuery",
			},
		},
		{
			name: "SELECT version()",
			sql:  "SELECT version()",
			wantMsgs: []string{
				"RowDescription",
				"DataRow",
				"CommandComplete",
				"ReadyForQuery",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer

			if err := HandleLocalQuery(&buf, tt.sql, LocalSession{StartupParams: startupParams, Sources: sources, ListenAddr: listenAddr}); err != nil {
				t.Fatalf("HandleLocalQuery(%q): %v", tt.sql, err)
			}

			fe := newFrontend(&buf)
			msgs := receiveAll(t, fe)

			if len(msgs) != len(tt.wantMsgs) {
				names := make([]string, len(msgs))
				for i, m := range msgs {
					names[i] = msgTypeName(m)
				}
				t.Fatalf("got %d messages %v, want %d %v", len(msgs), names, len(tt.wantMsgs), tt.wantMsgs)
			}

			for i, wantName := range tt.wantMsgs {
				got := msgTypeName(msgs[i])
				if got != wantName {
					t.Errorf("msgs[%d] type = %s, want %s", i, got, wantName)
				}
			}

			if tt.name == "SET command" {
				cc := msgs[0].(*pgproto3.CommandComplete)
				if string(cc.CommandTag) != "SET" {
					t.Errorf("CommandTag = %q, want %q", cc.CommandTag, "SET")
				}
			}
		})
	}
}

func assertMsgType[T pgproto3.BackendMessage](t *testing.T, msg pgproto3.BackendMessage, label string) {
	t.Helper()
	if _, ok := msg.(T); !ok {
		t.Errorf("%s: got %T, want %T", label, msg, *new(T))
	}
}

func msgTypeName(msg pgproto3.BackendMessage) string {
	switch msg.(type) {
	case *pgproto3.RowDescription:
		return "RowDescription"
	case *pgproto3.DataRow:
		return "DataRow"
	case *pgproto3.CommandComplete:
		return "CommandComplete"
	case *pgproto3.ReadyForQuery:
		return "ReadyForQuery"
	case *pgproto3.ErrorResponse:
		return "ErrorResponse"
	case *pgproto3.EmptyQueryResponse:
		return "EmptyQueryResponse"
	default:
		return "Unknown"
	}
}

func TestFormatDatetime(t *testing.T) {
	tests := []struct {
		name string
		val  any
		kind datetimeKind
		want string
	}{
		{name: "Z becomes +00", val: "2024-01-15T10:30:00Z", kind: datetimeAware, want: "2024-01-15 10:30:00+00"},
		{name: "already server form", val: "2024-01-15 10:30:00+00", kind: datetimeAware, want: "2024-01-15 10:30:00+00"},
		{name: "offset normalized to UTC", val: "2024-01-15T13:30:00.250+03:00", kind: datetimeAware, want: "2024-01-15 10:30:00.25+00"},
		{name: "naive isoformat", val: "2024-01-15T10:30:00", kind: datetimeNaive, want: "2024-01-15 10:30:00"},
		{name: "naive with microseconds", val: "2024-01-15T10:30:00.123456", kind: datetimeNaive, want: "2024-01-15 10:30:00.123456"},
		{name: "date only", val: "2024-01-15", kind: datetimeNaive, want: "2024-01-15 00:00:00"},
		{name: "text column passes through", val: "2024-01-15T10:30:00Z", kind: datetimeText, want: "2024-01-15T10:30:00Z"},
		{name: "unparseable passes through", val: json.Number("1700000000"), kind: datetimeNaive, want: "1700000000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatDatetime(tt.val, tt.kind); got != tt.want {
				t.Errorf("formatDatetime(%v, %d) = %q, want %q", tt.val, tt.kind, got, tt.want)
			}
		})
	}
}

// collectTyped decodes a result into its field descriptions and raw values
// (nil for SQL NULL). Values are copied as each message arrives, because the
// frontend reuses its buffer for the next one.
func collectTyped(t *testing.T, buf *bytes.Buffer) ([]pgproto3.FieldDescription, [][][]byte) {
	t.Helper()
	var fields []pgproto3.FieldDescription
	var rows [][][]byte
	fe := newFrontend(buf)
	for {
		msg, err := fe.Receive()
		if err != nil {
			break
		}
		switch m := msg.(type) {
		case *pgproto3.RowDescription:
			fields = append(fields, m.Fields...)
		case *pgproto3.DataRow:
			row := make([][]byte, len(m.Values))
			for i, v := range m.Values {
				if v != nil {
					row[i] = append([]byte(nil), v...)
				}
			}
			rows = append(rows, row)
		}
	}
	return fields, rows
}

// TestSendQueryResult_DatetimeColumns: a Redash datetime column is advertised
// as timestamp or timestamptz according to its values, in a text form pgx's
// codec for that OID scans back to the same instant; anything else is text.
func TestSendQueryResult_DatetimeColumns(t *testing.T) {
	utc := func(h, m, s, ns int) time.Time { return time.Date(2024, 1, 15, h, m, s, ns, time.UTC) }

	tests := []struct {
		name    string
		values  []any
		wantOID uint32
		wantRaw []string
		wantAt  []time.Time // per value; zero when the value is NULL
	}{
		{
			name:    "naive values are timestamp",
			values:  []any{"2024-01-15T10:30:00", "2024-01-15 10:30:00.123456"},
			wantOID: OidTimestamp,
			wantRaw: []string{"2024-01-15 10:30:00", "2024-01-15 10:30:00.123456"},
			wantAt:  []time.Time{utc(10, 30, 0, 0), utc(10, 30, 0, 123456000)},
		},
		{
			name:    "aware values are timestamptz rendered in UTC",
			values:  []any{"2024-01-15T10:30:00Z", "2024-01-15T13:30:00.5+03:00", "2024-01-15 10:30:00+00"},
			wantOID: OidTimestampTZ,
			wantRaw: []string{"2024-01-15 10:30:00+00", "2024-01-15 10:30:00.5+00", "2024-01-15 10:30:00+00"},
			wantAt:  []time.Time{utc(10, 30, 0, 0), utc(10, 30, 0, 500000000), utc(10, 30, 0, 0)},
		},
		{
			name:    "nulls do not decide the kind",
			values:  []any{nil, "2024-01-15T10:30:00Z"},
			wantOID: OidTimestampTZ,
			wantRaw: []string{"", "2024-01-15 10:30:00+00"},
			wantAt:  []time.Time{{}, utc(10, 30, 0, 0)},
		},
		{
			name:    "mixed naive and aware values fall back to text verbatim",
			values:  []any{"2024-01-15T10:30:00", "2024-01-15T10:30:00Z"},
			wantOID: OidText,
			wantRaw: []string{"2024-01-15T10:30:00", "2024-01-15T10:30:00Z"},
		},
		{
			name:    "unparseable value keeps the column text",
			values:  []any{"2024-01-15T10:30:00", "yesterday"},
			wantOID: OidText,
			wantRaw: []string{"2024-01-15T10:30:00", "yesterday"},
		},
		{
			name:    "no rows defaults to timestamp",
			wantOID: OidTimestamp,
		},
	}

	m := pgtype.NewMap()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := &redash.QueryResult{Columns: []redash.Column{{Name: "ts", Type: "datetime"}}}
			for _, v := range tt.values {
				result.Rows = append(result.Rows, map[string]any{"ts": v})
			}
			var buf bytes.Buffer
			if err := SendQueryResult(&buf, "SELECT ts FROM t", result); err != nil {
				t.Fatal(err)
			}
			fields, rows := collectTyped(t, &buf)
			if len(fields) != 1 || fields[0].DataTypeOID != tt.wantOID {
				t.Fatalf("fields = %+v, want one column with OID %d", fields, tt.wantOID)
			}
			if len(rows) != len(tt.values) {
				t.Fatalf("got %d rows, want %d", len(rows), len(tt.values))
			}
			for i, row := range rows {
				if got := string(row[0]); got != tt.wantRaw[i] {
					t.Errorf("row %d = %q, want %q", i, got, tt.wantRaw[i])
				}
				if tt.wantAt == nil || row[0] == nil {
					continue
				}
				var got time.Time
				if err := m.Scan(tt.wantOID, pgtype.TextFormatCode, row[0], &got); err != nil {
					t.Fatalf("row %d: pgtype cannot scan %q as OID %d: %v", i, row[0], tt.wantOID, err)
				}
				if !got.Equal(tt.wantAt[i]) {
					t.Errorf("row %d scanned to %v, want %v", i, got, tt.wantAt[i])
				}
			}
		})
	}
}

// TestHandleLocalQuery_ShowStripsTerminator: psql sends "SHOW x;" verbatim; the
// terminator is not part of the parameter name.
func TestHandleLocalQuery_ShowStripsTerminator(t *testing.T) {
	for _, sql := range []string{"SHOW server_version;", "SHOW server_version ;", "SHOW\nserver_version"} {
		t.Run(sql, func(t *testing.T) {
			if !IsLocalQuery(sql) {
				t.Fatalf("IsLocalQuery(%q) = false", sql)
			}
			var buf bytes.Buffer
			if err := HandleLocalQuery(&buf, sql, LocalSession{StartupParams: nil, Sources: nil, ListenAddr: "127.0.0.1:15432"}); err != nil {
				t.Fatal(err)
			}
			cols, rows := collectResult(t, &buf)
			if len(cols) != 1 || cols[0] != "server_version" {
				t.Errorf("columns = %v, want [server_version]", cols)
			}
			if len(rows) != 1 || rows[0][0] != "14.0" {
				t.Errorf("rows = %v, want [[14.0]]", rows)
			}
		})
	}
}

func TestHandleLocalQuery_SetWithNewline(t *testing.T) {
	sql := "SET\nsearch_path TO public"
	if !IsLocalQuery(sql) {
		t.Fatalf("IsLocalQuery(%q) = false", sql)
	}
	var buf bytes.Buffer
	if err := HandleLocalQuery(&buf, sql, LocalSession{StartupParams: nil, Sources: nil, ListenAddr: "127.0.0.1:15432"}); err != nil {
		t.Fatal(err)
	}
	msgs := receiveAll(t, newFrontend(&buf))
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want CommandComplete + ReadyForQuery", len(msgs))
	}
	cc, ok := msgs[0].(*pgproto3.CommandComplete)
	if !ok || string(cc.CommandTag) != "SET" {
		t.Errorf("msgs[0] = %#v, want CommandComplete SET", msgs[0])
	}
}

// TestHandleLocalQuery_PgDatabaseBeforeScalarShortcuts: a pg_database listing
// whose WHERE clause mentions current_user (DBeaver and pgAdmin filter on
// has_database_privilege) must return the data sources, not the user name.
func TestHandleLocalQuery_PgDatabaseBeforeScalarShortcuts(t *testing.T) {
	sources := []redash.DataSource{
		{ID: 1, Name: "a", Type: "pg"},
		{ID: 2, Name: "b", Type: "pg"},
	}
	sql := "SELECT d.datname FROM pg_catalog.pg_database d WHERE has_database_privilege(current_user, d.datname, 'CONNECT') ORDER BY 1"
	var buf bytes.Buffer
	if err := HandleLocalQuery(&buf, sql, LocalSession{StartupParams: map[string]string{"user": "alice"}, Sources: sources, ListenAddr: "127.0.0.1:15432"}); err != nil {
		t.Fatal(err)
	}
	cols, rows := collectResult(t, &buf)
	if len(cols) == 0 || cols[0] != "datname" {
		t.Fatalf("columns = %v, want datname first", cols)
	}
	if len(rows) != 2 || rows[0][0] != "a" || rows[1][0] != "b" {
		t.Errorf("rows = %v, want the two data sources", rows)
	}

	// The scalar shortcut still answers a bare function call.
	buf.Reset()
	if err := HandleLocalQuery(&buf, "SELECT current_user", LocalSession{StartupParams: map[string]string{"user": "alice"}, Sources: sources, ListenAddr: "127.0.0.1:15432"}); err != nil {
		t.Fatal(err)
	}
	if _, rows := collectResult(t, &buf); len(rows) != 1 || rows[0][0] != "alice" {
		t.Errorf("SELECT current_user rows = %v, want [[alice]]", rows)
	}
}

// A read-only proxy must not answer SET with a silent OK when the SET asks for
// the one thing it cannot grant; but it must keep answering every other SET,
// and the plain read-only forms, so a client's usual session setup still works.
func TestAsksForReadWrite(t *testing.T) {
	tests := []struct {
		sql  string
		want bool
	}{
		{"SET transaction_read_only = off", true},
		{"SET transaction_read_only TO off", true},
		{"set session transaction_read_only = false", true},
		{"SET default_transaction_read_only = 0", true},
		{"SET default_transaction_read_only TO 'off'", true},
		{"SET SESSION CHARACTERISTICS AS TRANSACTION READ WRITE", true},
		{"BEGIN READ WRITE", true},
		{"START TRANSACTION ISOLATION LEVEL SERIALIZABLE READ WRITE", true},
		{"BEGIN", false},
		{"BEGIN READ ONLY", false},
		{"SET transaction_read_only = on", false},
		{"SET default_transaction_read_only TO on", false},
		{"SET search_path TO public", false},
		{"SET application_name = 'read write'", false},
		{"SELECT 1", false},
	}
	for _, tt := range tests {
		if got := asksForReadWrite(normalize(tt.sql), tt.sql); got != tt.want {
			t.Errorf("asksForReadWrite(%q) = %v, want %v", tt.sql, got, tt.want)
		}
	}
}
