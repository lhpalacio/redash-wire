package pgwire

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
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

		{name: "ISO 8601 datetime", val: "2024-01-15T10:30:00Z", redashType: "datetime", want: "2024-01-15 10:30:00+00"},
		{name: "already spaced datetime", val: "2024-01-15 10:30:00+00", redashType: "datetime", want: "2024-01-15 10:30:00+00"},

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
		{name: "SHOW", sql: "SHOW server_version", want: true},
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
		{name: "not select", lower: "show something", want: false},
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

			if err := HandleLocalQuery(&buf, tt.sql, startupParams, sources, listenAddr); err != nil {
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
