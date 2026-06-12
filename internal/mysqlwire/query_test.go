package mysqlwire

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/lhpalacio/redash-wire/internal/redash"
)

// parseTextRows decodes RowDatas back into FieldValues: BuildSimpleResultset
// fills only RowDatas and Fields, leaving Values for the client packet parser.
func parseTextRows(t *testing.T, rs *mysql.Resultset) [][]mysql.FieldValue {
	t.Helper()
	rows := make([][]mysql.FieldValue, len(rs.RowDatas))
	for i, rd := range rs.RowDatas {
		fvs, err := rd.ParseText(rs.Fields, nil)
		if err != nil {
			t.Fatalf("ParseText row %d: %v", i, err)
		}
		rows[i] = fvs
	}
	return rows
}

func fieldValueString(fv mysql.FieldValue) string {
	return string(fv.AsString())
}

func TestIsLocalQuery(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want bool
	}{
		{name: "SET NAMES", sql: "SET NAMES utf8mb4", want: true},
		{name: "SET lowercase", sql: "set names utf8mb4", want: true},
		{name: "SET mixed case", sql: "Set Character_Set_Results = NULL", want: true},

		{name: "BEGIN", sql: "BEGIN", want: true},
		{name: "START TRANSACTION", sql: "START TRANSACTION", want: true},
		{name: "COMMIT", sql: "COMMIT", want: true},
		{name: "ROLLBACK", sql: "ROLLBACK", want: true},
		{name: "begin lowercase", sql: "begin", want: true},
		{name: "commit lowercase", sql: "commit", want: true},

		{name: "SHOW DATABASES", sql: "SHOW DATABASES", want: true},
		{name: "SHOW SCHEMAS", sql: "SHOW SCHEMAS", want: true},
		{name: "show databases lowercase", sql: "show databases", want: true},

		{name: "SHOW TABLES", sql: "SHOW TABLES", want: true},
		{name: "SHOW FULL TABLES", sql: "SHOW FULL TABLES", want: true},
		{name: "show tables lowercase", sql: "show tables", want: true},
		{name: "show full tables lowercase", sql: "show full tables", want: true},

		{name: "SHOW VARIABLES", sql: "SHOW VARIABLES", want: true},
		{name: "SHOW SESSION VARIABLES", sql: "SHOW SESSION VARIABLES", want: true},
		{name: "SHOW GLOBAL VARIABLES", sql: "SHOW GLOBAL VARIABLES", want: true},
		{name: "show variables lowercase", sql: "show variables", want: true},

		{name: "SHOW SESSION STATUS", sql: "SHOW SESSION STATUS", want: true},
		{name: "SHOW STATUS", sql: "SHOW STATUS", want: true},

		{name: "information_schema.tables", sql: "SELECT * FROM information_schema.tables", want: true},
		{name: "information_schema uppercase", sql: "SELECT * FROM INFORMATION_SCHEMA.TABLES", want: true},

		{name: "SELECT @@version", sql: "SELECT @@version", want: true},
		{name: "SELECT version()", sql: "SELECT version()", want: true},
		{name: "SELECT database()", sql: "SELECT database()", want: true},
		{name: "SELECT @@version_comment", sql: "SELECT @@version_comment", want: true},
		{name: "SELECT @@max_allowed_packet", sql: "SELECT @@max_allowed_packet", want: true},
		{name: "select @@version lowercase", sql: "select @@version", want: true},

		{name: "SELECT FROM table", sql: "SELECT * FROM users", want: false},
		{name: "INSERT", sql: "INSERT INTO logs (msg) VALUES ('hello')", want: false},
		{name: "UPDATE", sql: "UPDATE users SET name='x' WHERE id=1", want: false},
		{name: "DELETE", sql: "DELETE FROM users WHERE id=1", want: false},

		{name: "leading space SET", sql: "  SET NAMES utf8mb4", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isLocalQuery(tt.sql)
			if got != tt.want {
				t.Errorf("isLocalQuery(%q) = %v, want %v", tt.sql, got, tt.want)
			}
		})
	}
}

func TestFormatValue(t *testing.T) {
	tests := []struct {
		name       string
		val        any
		redashType string
		want       any
	}{
		{name: "bool true", val: true, redashType: "boolean", want: int64(1)},
		{name: "bool false", val: false, redashType: "boolean", want: int64(0)},
		{name: "string true", val: "true", redashType: "boolean", want: int64(1)},
		{name: "string 1", val: "1", redashType: "boolean", want: int64(1)},
		{name: "string t", val: "t", redashType: "boolean", want: int64(1)},
		{name: "string false", val: "false", redashType: "boolean", want: int64(0)},
		{name: "string 0", val: "0", redashType: "boolean", want: int64(0)},

		{name: "json.Number int", val: json.Number("42"), redashType: "integer", want: int64(42)},
		{name: "float64 int", val: float64(42), redashType: "integer", want: int64(42)},
		{name: "json.Number negative int", val: json.Number("-7"), redashType: "integer", want: int64(-7)},

		// Integer overflowing int64 is kept exactly as text rather than corrupted.
		{name: "json.Number overflow int", val: json.Number("18446744073709551615"), redashType: "integer", want: "18446744073709551615"},

		{name: "json.Number float", val: json.Number("3.14"), redashType: "float", want: float64(3.14)},
		// High-precision DECIMAL is kept exactly as text instead of corrupted via float64.
		{name: "high-precision decimal", val: json.Number("1234567890.0987654321"), redashType: "float", want: "1234567890.0987654321"},

		{name: "datetime ISO", val: "2024-01-15T10:30:00Z", redashType: "datetime", want: "2024-01-15 10:30:00"},
		{name: "datetime no Z", val: "2024-01-15T10:30:00", redashType: "datetime", want: "2024-01-15 10:30:00"},

		// Complex types (map, slice) are always JSON regardless of redashType.
		{name: "map to JSON", val: map[string]any{"key": "val"}, redashType: "string", want: `{"key":"val"}`},
		{name: "slice to JSON", val: []any{1, 2, 3}, redashType: "string", want: `[1,2,3]`},

		{name: "plain string", val: "hello", redashType: "string", want: "hello"},
		{name: "number as string type", val: 99, redashType: "string", want: fmt.Sprintf("%v", 99)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatOne(tt.val, tt.redashType)
			if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", tt.want) {
				t.Errorf("formatOne(%v, %q) = %v (%T), want %v (%T)",
					tt.val, tt.redashType, got, got, tt.want, tt.want)
			}
		})
	}
}

// formatOne resolves a single value the way buildResult does for a one-row column
// of the given Redash type, so per-column type decisions are exercised end to end.
func formatOne(val any, redashType string) any {
	col := redash.Column{Name: "c", Type: redashType}
	rows := []map[string]any{{"c": val}}
	return convertValue(val, redashType, columnKind(col, rows))
}

// TestBuildResultMixedColumnTypes guards against the "row types aren't consistent"
// crash: a column whose values would map to different Go types must still build.
func TestBuildResultMixedColumnTypes(t *testing.T) {
	qr := &redash.QueryResult{
		Columns: []redash.Column{{Name: "id", Type: "integer"}},
		Rows: []map[string]any{
			{"id": json.Number("1")},
			{"id": json.Number("18446744073709551615")}, // overflows int64
		},
	}
	result, err := buildResult("SELECT id FROM events", qr)
	if err != nil {
		t.Fatalf("buildResult should not fail on mixed/overflow values: %v", err)
	}
	rows := parseTextRows(t, result.Resultset)
	if got := fieldValueString(rows[1][0]); got != "18446744073709551615" {
		t.Errorf("overflow value = %q, want exact text", got)
	}
}

// TestBuildResultBooleanWithJSON guards against the "row types aren't consistent"
// crash for a boolean column that also holds a nested JSON value.
func TestBuildResultBooleanWithJSON(t *testing.T) {
	qr := &redash.QueryResult{
		Columns: []redash.Column{{Name: "flag", Type: "boolean"}},
		Rows: []map[string]any{
			{"flag": true},
			{"flag": map[string]any{"unexpected": "object"}},
		},
	}
	result, err := buildResult("SELECT flag FROM t", qr)
	if err != nil {
		t.Fatalf("buildResult should not fail on boolean+JSON mix: %v", err)
	}
	if got := len(result.RowDatas); got != 2 {
		t.Errorf("row count = %d, want 2", got)
	}
}

func TestBuildResult(t *testing.T) {
	t.Run("SELECT with columns and rows", func(t *testing.T) {
		qr := &redash.QueryResult{
			Columns: []redash.Column{
				{Name: "id", Type: "integer"},
				{Name: "name", Type: "string"},
			},
			Rows: []map[string]any{
				{"id": json.Number("1"), "name": "Alice"},
				{"id": json.Number("2"), "name": "Bob"},
			},
		}

		result, err := buildResult("SELECT id, name FROM users", qr)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result.Resultset == nil {
			t.Fatal("expected Resultset, got nil")
		}
		if got := len(result.RowDatas); got != 2 {
			t.Errorf("row count = %d, want 2", got)
		}
		if got := result.ColumnNumber(); got != 2 {
			t.Errorf("column count = %d, want 2", got)
		}

		if got := string(result.Resultset.Fields[0].Name); got != "id" {
			t.Errorf("field 0 name = %q, want %q", got, "id")
		}
		if got := string(result.Resultset.Fields[1].Name); got != "name" {
			t.Errorf("field 1 name = %q, want %q", got, "name")
		}

		rows := parseTextRows(t, result.Resultset)
		if got := fieldValueString(rows[1][1]); got != "Bob" {
			t.Errorf("row 1 name = %q, want %q", got, "Bob")
		}
	})

	t.Run("INSERT returns affected rows", func(t *testing.T) {
		qr := &redash.QueryResult{
			Rows: []map[string]any{{}, {}, {}},
		}

		result, err := buildResult("INSERT INTO users (name) VALUES ('x')", qr)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result.AffectedRows != 3 {
			t.Errorf("AffectedRows = %d, want 3", result.AffectedRows)
		}
	})

	t.Run("UPDATE returns affected rows", func(t *testing.T) {
		qr := &redash.QueryResult{
			Rows: []map[string]any{{}},
		}

		result, err := buildResult("UPDATE users SET name='y' WHERE id=1", qr)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result.AffectedRows != 1 {
			t.Errorf("AffectedRows = %d, want 1", result.AffectedRows)
		}
	})

	t.Run("DELETE returns affected rows", func(t *testing.T) {
		qr := &redash.QueryResult{
			Rows: []map[string]any{{}, {}},
		}

		result, err := buildResult("DELETE FROM logs WHERE id < 10", qr)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result.AffectedRows != 2 {
			t.Errorf("AffectedRows = %d, want 2", result.AffectedRows)
		}
	})

	t.Run("empty columns returns empty result", func(t *testing.T) {
		qr := &redash.QueryResult{
			Columns: nil,
			Rows:    nil,
		}

		result, err := buildResult("SELECT 1", qr)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result.Resultset != nil {
			t.Error("expected nil Resultset for empty columns")
		}
	})
}

func TestHandleShowDatabases(t *testing.T) {
	sources := []redash.DataSource{
		{ID: 1, Name: "prod_mysql", Type: "mysql"},
		{ID: 2, Name: "staging_mysql", Type: "mysql"},
	}

	result, err := handleShowDatabases(sources)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rs := result.Resultset
	if rs == nil {
		t.Fatal("expected Resultset, got nil")
	}
	if got := len(rs.RowDatas); got != 2 {
		t.Errorf("row count = %d, want 2", got)
	}
	if got := rs.ColumnNumber(); got != 1 {
		t.Errorf("column count = %d, want 1", got)
	}

	if got := string(rs.Fields[0].Name); got != "Database" {
		t.Errorf("field name = %q, want %q", got, "Database")
	}

	rows := parseTextRows(t, rs)
	if got := fieldValueString(rows[0][0]); got != "prod_mysql" {
		t.Errorf("row 0 Database = %q, want %q", got, "prod_mysql")
	}
	if got := fieldValueString(rows[1][0]); got != "staging_mysql" {
		t.Errorf("row 1 Database = %q, want %q", got, "staging_mysql")
	}
}

func TestHandleShowTables(t *testing.T) {
	schema := []redash.SchemaTable{
		{Name: "users", Columns: []string{"id", "name"}},
		{Name: "orders", Columns: []string{"id", "total"}},
	}

	t.Run("show tables", func(t *testing.T) {
		result, err := handleShowTables("show tables", schema)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		rs := result.Resultset
		if rs == nil {
			t.Fatal("expected Resultset, got nil")
		}
		if got := rs.ColumnNumber(); got != 1 {
			t.Errorf("column count = %d, want 1", got)
		}
		if got := len(rs.RowDatas); got != 2 {
			t.Errorf("row count = %d, want 2", got)
		}

		if got := string(rs.Fields[0].Name); got != "Tables" {
			t.Errorf("field name = %q, want %q", got, "Tables")
		}

		rows := parseTextRows(t, rs)
		if got := fieldValueString(rows[0][0]); got != "users" {
			t.Errorf("row 0 Tables = %q, want %q", got, "users")
		}
	})

	t.Run("show full tables", func(t *testing.T) {
		result, err := handleShowTables("show full tables", schema)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		rs := result.Resultset
		if rs == nil {
			t.Fatal("expected Resultset, got nil")
		}
		if got := rs.ColumnNumber(); got != 2 {
			t.Errorf("column count = %d, want 2", got)
		}
		if got := len(rs.RowDatas); got != 2 {
			t.Errorf("row count = %d, want 2", got)
		}

		if got := string(rs.Fields[0].Name); got != "Tables" {
			t.Errorf("field 0 name = %q, want %q", got, "Tables")
		}
		if got := string(rs.Fields[1].Name); got != "Table_type" {
			t.Errorf("field 1 name = %q, want %q", got, "Table_type")
		}

		rows := parseTextRows(t, rs)
		if got := fieldValueString(rows[0][1]); got != "BASE TABLE" {
			t.Errorf("row 0 Table_type = %q, want %q", got, "BASE TABLE")
		}
	})
}

func TestHandleShowVariables(t *testing.T) {
	result, err := handleShowVariables()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rs := result.Resultset
	if rs == nil {
		t.Fatal("expected Resultset, got nil")
	}
	if got := rs.ColumnNumber(); got != 2 {
		t.Errorf("column count = %d, want 2", got)
	}

	if got := string(rs.Fields[0].Name); got != "Variable_name" {
		t.Errorf("field 0 name = %q, want %q", got, "Variable_name")
	}
	if got := string(rs.Fields[1].Name); got != "Value" {
		t.Errorf("field 1 name = %q, want %q", got, "Value")
	}

	if got := len(rs.RowDatas); got < 10 {
		t.Errorf("row count = %d, want at least 10", got)
	}

	rows := parseTextRows(t, rs)
	found := false
	for _, row := range rows {
		if fieldValueString(row[0]) == "version" {
			if got := fieldValueString(row[1]); got != "8.0.0-redash-wire" {
				t.Errorf("version value = %q, want %q", got, "8.0.0-redash-wire")
			}
			found = true
			break
		}
	}
	if !found {
		t.Error("expected to find 'version' variable in SHOW VARIABLES result")
	}
}

func TestHandleLocalQuery(t *testing.T) {
	sources := []redash.DataSource{
		{ID: 1, Name: "prod_mysql", Type: "mysql"},
		{ID: 2, Name: "staging_mysql", Type: "mysql"},
	}
	schema := []redash.SchemaTable{
		{Name: "users"},
	}

	t.Run("SET returns nil result", func(t *testing.T) {
		result, err := handleLocalQuery("SET NAMES utf8mb4", "prod_mysql", sources, schema)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result != nil {
			t.Errorf("expected nil result for SET command, got %v", result)
		}
	})

	t.Run("SHOW DATABASES returns databases", func(t *testing.T) {
		result, err := handleLocalQuery("SHOW DATABASES", "prod_mysql", sources, schema)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result == nil || result.Resultset == nil {
			t.Fatal("expected non-nil result with Resultset for SHOW DATABASES")
		}
		if got := len(result.RowDatas); got != len(sources) {
			t.Errorf("row count = %d, want %d", got, len(sources))
		}
	})

	t.Run("SELECT @@version returns version", func(t *testing.T) {
		result, err := handleLocalQuery("SELECT @@version", "prod_mysql", sources, schema)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result == nil || result.Resultset == nil {
			t.Fatal("expected non-nil result with Resultset for SELECT @@version")
		}
		rows := parseTextRows(t, result.Resultset)
		if len(rows) != 1 {
			t.Fatalf("row count = %d, want 1", len(rows))
		}
		if got := fieldValueString(rows[0][0]); got != "8.0.0-redash-wire" {
			t.Errorf("@@version = %q, want %q", got, "8.0.0-redash-wire")
		}
	})

	t.Run("BEGIN returns nil result", func(t *testing.T) {
		result, err := handleLocalQuery("BEGIN", "prod_mysql", sources, schema)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result != nil {
			t.Errorf("expected nil result for BEGIN, got %v", result)
		}
	})

	t.Run("SELECT database() with dbName", func(t *testing.T) {
		result, err := handleLocalQuery("SELECT database()", "prod_mysql", sources, schema)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result == nil || result.Resultset == nil {
			t.Fatal("expected result with Resultset")
		}
		rows := parseTextRows(t, result.Resultset)
		if len(rows) != 1 {
			t.Fatalf("row count = %d, want 1", len(rows))
		}
		if got := fieldValueString(rows[0][0]); got != "prod_mysql" {
			t.Errorf("database() = %q, want %q", got, "prod_mysql")
		}
	})
}

func TestSingleResult(t *testing.T) {
	t.Run("string value", func(t *testing.T) {
		result, err := singleResult("col", "hello")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		rs := result.Resultset
		if rs == nil {
			t.Fatal("expected Resultset, got nil")
		}
		if got := len(rs.RowDatas); got != 1 {
			t.Errorf("row count = %d, want 1", got)
		}
		if got := rs.ColumnNumber(); got != 1 {
			t.Errorf("column count = %d, want 1", got)
		}
		rows := parseTextRows(t, rs)
		if got := fieldValueString(rows[0][0]); got != "hello" {
			t.Errorf("value = %q, want %q", got, "hello")
		}
	})

	t.Run("int value", func(t *testing.T) {
		result, err := singleResult("num", 42)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		rs := result.Resultset
		if rs == nil {
			t.Fatal("expected Resultset, got nil")
		}
		rows := parseTextRows(t, rs)
		got := fmt.Sprintf("%v", rows[0][0].Value())
		if got != "42" {
			t.Errorf("value = %s, want 42", got)
		}
	})
}
