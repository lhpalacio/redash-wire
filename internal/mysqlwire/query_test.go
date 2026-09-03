package mysqlwire

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
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

// fieldValueString renders a parsed value the way a client would print it:
// text as-is, numbers formatted, NULL as "".
func fieldValueString(fv mysql.FieldValue) string {
	switch v := fv.Value().(type) {
	case []byte:
		return string(v)
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
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
		{name: "FROM on its own line with database()", sql: "SELECT id, DATABASE() AS db\nFROM users\nLIMIT 5", want: false},
		{name: "FROM after a tab", sql: "SELECT @@version\tFROM dual", want: false},
		{name: "from inside a literal", sql: "SELECT 'from', @@version", want: true},
		{name: "Connector/J startup probe", sql: connectorJProbe, want: true},
		{name: "three session functions", sql: "SELECT DATABASE(), USER(), VERSION()", want: true},
		{name: "select on its own line", sql: "SELECT\n@@version", want: true},
		{name: "plain SELECT 1 goes to Redash", sql: "SELECT 1", want: false},
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
		{Name: "users", Columns: []redash.SchemaColumn{{Name: "id"}, {Name: "name"}}},
		{Name: "orders", Columns: []redash.SchemaColumn{{Name: "id"}, {Name: "total"}}},
		{Name: "user_roles"},
	}
	sess := localSession{
		dbName:  "prod_mysql",
		sources: []redash.DataSource{{ID: 1, Name: "prod_mysql", Type: "mysql"}, {ID: 2, Name: "staging_mysql", Type: "mysql"}},
		schema:  schema,
	}

	tableNames := func(t *testing.T, result *mysql.Result) []string {
		t.Helper()
		var names []string
		for _, row := range parseTextRows(t, result.Resultset) {
			names = append(names, fieldValueString(row[0]))
		}
		return names
	}

	t.Run("show tables", func(t *testing.T) {
		result, err := handleShowTables("show tables", sess)
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
		if got := string(rs.Fields[0].Name); got != "Tables_in_prod_mysql" {
			t.Errorf("field name = %q, want %q", got, "Tables_in_prod_mysql")
		}
		if got := tableNames(t, result); !slices.Equal(got, []string{"users", "orders", "user_roles"}) {
			t.Errorf("tables = %v, want every table", got)
		}
	})

	t.Run("show full tables", func(t *testing.T) {
		result, err := handleShowTables("show full tables", sess)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		rs := result.Resultset
		if got := rs.ColumnNumber(); got != 2 {
			t.Errorf("column count = %d, want 2", got)
		}
		if got := len(rs.RowDatas); got != 3 {
			t.Errorf("row count = %d, want 3", got)
		}
		if got := string(rs.Fields[1].Name); got != "Table_type" {
			t.Errorf("field 1 name = %q, want %q", got, "Table_type")
		}
		rows := parseTextRows(t, rs)
		if got := fieldValueString(rows[0][1]); got != "BASE TABLE" {
			t.Errorf("row 0 Table_type = %q, want %q", got, "BASE TABLE")
		}
	})

	t.Run("no database selected lists nothing", func(t *testing.T) {
		result, err := handleShowTables("SHOW TABLES", localSession{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := len(result.RowDatas); got != 0 {
			t.Errorf("row count = %d, want 0", got)
		}
		if got := string(result.Resultset.Fields[0].Name); got != "Tables" {
			t.Errorf("field name = %q, want %q", got, "Tables")
		}
	})

	filters := []struct {
		name string
		sql  string
		want []string
	}{
		{"LIKE prefix", "SHOW TABLES LIKE 'user%'", []string{"users", "user_roles"}},
		{"LIKE is case-insensitive", "SHOW TABLES LIKE 'USER%'", []string{"users", "user_roles"}},
		{"LIKE underscore matches one character", "SHOW TABLES LIKE 'user_'", []string{"users"}},
		{"LIKE escaped underscore is literal", `SHOW TABLES LIKE 'user\_%'`, []string{"user_roles"}},
		{"LIKE exact", "SHOW TABLES LIKE 'orders'", []string{"orders"}},
		{"LIKE nothing", "SHOW TABLES LIKE 'zzz%'", nil},
		{"LIKE double-quoted pattern", `SHOW TABLES LIKE "%roles"`, []string{"user_roles"}},
		{"FULL with LIKE", "SHOW FULL TABLES LIKE 'orders'", []string{"orders"}},
		{"FROM current database", "SHOW TABLES FROM prod_mysql", []string{"users", "orders", "user_roles"}},
		{"IN current database, quoted, other case", "SHOW TABLES IN `PROD_MYSQL` LIKE 'o%'", []string{"orders"}},
		{"WHERE equals", "SHOW TABLES WHERE Tables_in_prod_mysql = 'users'", []string{"users"}},
		{"WHERE quoted column", "SHOW TABLES WHERE `Tables_in_prod_mysql` LIKE '%s'", []string{"users", "orders", "user_roles"}},
		{"WHERE not equals", "SHOW TABLES WHERE Tables_in_prod_mysql <> 'users'", []string{"orders", "user_roles"}},
		{"WHERE NOT LIKE", "SHOW TABLES WHERE Tables_in_prod_mysql NOT LIKE 'user%'", []string{"orders"}},
		{"WHERE AND", "SHOW FULL TABLES WHERE Table_type = 'BASE TABLE' AND Tables_in_prod_mysql LIKE 'u%'", []string{"users", "user_roles"}},
		{"WHERE Table_type VIEW", "SHOW FULL TABLES WHERE Table_type = 'VIEW'", nil},
		{"trailing semicolon", "SHOW TABLES LIKE 'orders';", []string{"orders"}},
	}
	for _, tt := range filters {
		t.Run(tt.name, func(t *testing.T) {
			result, err := handleShowTables(tt.sql, sess)
			if err != nil {
				t.Fatalf("handleShowTables(%q): %v", tt.sql, err)
			}
			if got := tableNames(t, result); !slices.Equal(got, tt.want) {
				t.Errorf("handleShowTables(%q) = %v, want %v", tt.sql, got, tt.want)
			}
		})
	}

	t.Run("LIKE names the pattern in the column", func(t *testing.T) {
		result, err := handleShowTables("SHOW TABLES LIKE 'user%'", sess)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := string(result.Resultset.Fields[0].Name); got != "Tables_in_prod_mysql (user%)" {
			t.Errorf("field name = %q, want %q", got, "Tables_in_prod_mysql (user%)")
		}
	})

	refusals := []struct {
		name string
		sql  string
		code uint16
	}{
		{"FROM unknown database", "SHOW TABLES FROM nope", mysql.ER_BAD_DB_ERROR},
		{"FROM another data source", "SHOW TABLES FROM staging_mysql", mysql.ER_NOT_SUPPORTED_YET},
		{"WHERE on an unknown column", "SHOW TABLES WHERE Engine = 'InnoDB'", mysql.ER_NOT_SUPPORTED_YET},
		{"WHERE with OR", "SHOW TABLES WHERE Tables_in_prod_mysql = 'a' OR Tables_in_prod_mysql = 'b'", mysql.ER_NOT_SUPPORTED_YET},
		{"WHERE with a function", "SHOW TABLES WHERE LENGTH(Tables_in_prod_mysql) > 3", mysql.ER_NOT_SUPPORTED_YET},
		{"WHERE with nothing after it", "SHOW TABLES WHERE", mysql.ER_PARSE_ERROR},
		{"LIKE without a pattern", "SHOW TABLES LIKE", mysql.ER_PARSE_ERROR},
		{"trailing garbage", "SHOW TABLES LIKE 'a' extra", mysql.ER_PARSE_ERROR},
	}
	for _, tt := range refusals {
		t.Run(tt.name, func(t *testing.T) {
			_, err := handleShowTables(tt.sql, sess)
			var myErr *mysql.MyError
			if !errors.As(err, &myErr) {
				t.Fatalf("handleShowTables(%q) err = %v, want a MySQL error", tt.sql, err)
			}
			if myErr.Code != tt.code {
				t.Errorf("handleShowTables(%q) code = %d, want %d (%s)", tt.sql, myErr.Code, tt.code, myErr.Message)
			}
		})
	}
}

func TestLikeMatcher(t *testing.T) {
	tests := []struct {
		pattern, s string
		want       bool
	}{
		{"user%", "users", true},
		{"user%", "orders", false},
		{"%", "", true},
		{"_", "", false},
		{"u_er%", "USER_ROLES", true},
		{`a\%b`, "a%b", true},
		{`a\%b`, "axb", false},
		{`a\_b`, "a_b", true},
		{`a\_b`, "axb", false},
		{"a.b", "axb", false},
		{"caf%", "café", true},
		{"caf_", "café", true},
	}
	for _, tt := range tests {
		if got := likeMatcher(tt.pattern)(tt.s); got != tt.want {
			t.Errorf("likeMatcher(%q)(%q) = %v, want %v", tt.pattern, tt.s, got, tt.want)
		}
	}
}

func TestHandleShowVariables(t *testing.T) {
	result, err := handleShowVariables("SHOW VARIABLES", false)
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
	sess := localSession{dbName: "prod_mysql", connID: 42, sources: sources, schema: schema}

	t.Run("SET returns nil result", func(t *testing.T) {
		result, err := handleLocalQuery("SET NAMES utf8mb4", sess)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result != nil {
			t.Errorf("expected nil result for SET command, got %v", result)
		}
	})

	t.Run("SHOW DATABASES returns databases", func(t *testing.T) {
		result, err := handleLocalQuery("SHOW DATABASES", sess)
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
		result, err := handleLocalQuery("SELECT @@version", sess)
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
		result, err := handleLocalQuery("BEGIN", sess)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result != nil {
			t.Errorf("expected nil result for BEGIN, got %v", result)
		}
	})

	t.Run("SELECT database() with dbName", func(t *testing.T) {
		result, err := handleLocalQuery("SELECT database()", sess)
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

// connectorJProbe is the 19-variable query MySQL Connector/J sends on connect.
const connectorJProbe = "SELECT  @@session.auto_increment_increment AS auto_increment_increment, @@character_set_client AS character_set_client, @@character_set_connection AS character_set_connection, @@character_set_results AS character_set_results, @@character_set_server AS character_set_server, @@collation_server AS collation_server, @@collation_connection AS collation_connection, @@init_connect AS init_connect, @@interactive_timeout AS interactive_timeout, @@license AS license, @@lower_case_table_names AS lower_case_table_names, @@max_allowed_packet AS max_allowed_packet, @@net_write_timeout AS net_write_timeout, @@performance_schema AS performance_schema, @@sql_mode AS sql_mode, @@system_time_zone AS system_time_zone, @@time_zone AS time_zone, @@transaction_isolation AS transaction_isolation, @@wait_timeout AS wait_timeout"

func TestHandleLocalSelect(t *testing.T) {
	sess := localSession{dbName: "prod_mysql", connID: 10007}

	columns := func(t *testing.T, sql string) ([]string, []mysql.FieldValue) {
		t.Helper()
		result, err := handleLocalQuery(sql, sess)
		if err != nil {
			t.Fatalf("handleLocalQuery(%q): %v", sql, err)
		}
		if result == nil || result.Resultset == nil {
			t.Fatalf("handleLocalQuery(%q): no result set", sql)
		}
		names := make([]string, len(result.Fields))
		for i, f := range result.Fields {
			names[i] = string(f.Name)
		}
		rows := parseTextRows(t, result.Resultset)
		if len(rows) != 1 {
			t.Fatalf("handleLocalQuery(%q): %d rows, want 1", sql, len(rows))
		}
		return names, rows[0]
	}

	t.Run("Connector/J probe gets one column per variable", func(t *testing.T) {
		names, row := columns(t, connectorJProbe)
		want := []string{
			"auto_increment_increment", "character_set_client", "character_set_connection",
			"character_set_results", "character_set_server", "collation_server", "collation_connection",
			"init_connect", "interactive_timeout", "license", "lower_case_table_names", "max_allowed_packet",
			"net_write_timeout", "performance_schema", "sql_mode", "system_time_zone", "time_zone",
			"transaction_isolation", "wait_timeout",
		}
		if !slices.Equal(names, want) {
			t.Fatalf("columns = %v, want %v", names, want)
		}
		if got := fieldValueString(row[0]); got != "1" {
			t.Errorf("auto_increment_increment = %q, want 1", got)
		}
		if got := fieldValueString(row[11]); got != "67108864" {
			t.Errorf("max_allowed_packet = %q, want 67108864", got)
		}
		if got := fieldValueString(row[17]); got != "REPEATABLE-READ" {
			t.Errorf("transaction_isolation = %q, want REPEATABLE-READ", got)
		}
	})

	t.Run("several functions keep their own columns", func(t *testing.T) {
		names, row := columns(t, "SELECT DATABASE(), USER(), VERSION()")
		if want := []string{"DATABASE()", "USER()", "VERSION()"}; !slices.Equal(names, want) {
			t.Fatalf("columns = %v, want %v", names, want)
		}
		if got := fieldValueString(row[0]); got != "prod_mysql" {
			t.Errorf("DATABASE() = %q, want prod_mysql", got)
		}
		if got := fieldValueString(row[1]); got != "redash@localhost" {
			t.Errorf("USER() = %q, want redash@localhost", got)
		}
		if got := fieldValueString(row[2]); got != serverVersion {
			t.Errorf("VERSION() = %q, want %q", got, serverVersion)
		}
	})

	t.Run("mysql CLI banner query", func(t *testing.T) {
		names, row := columns(t, "select @@version_comment limit 1")
		if want := []string{"@@version_comment"}; !slices.Equal(names, want) {
			t.Fatalf("columns = %v, want %v", names, want)
		}
		if got := fieldValueString(row[0]); got != "redash-wire MySQL proxy" {
			t.Errorf("@@version_comment = %q", got)
		}
	})

	t.Run("aliases", func(t *testing.T) {
		names, row := columns(t, "SELECT @@version v, @@session.time_zone AS `tz`, 'x' AS \"lit\", version() AS 'ver', 1 one")
		if want := []string{"v", "tz", "lit", "ver", "one"}; !slices.Equal(names, want) {
			t.Fatalf("columns = %v, want %v", names, want)
		}
		if got := fieldValueString(row[1]); got != "SYSTEM" {
			t.Errorf("@@session.time_zone = %q, want SYSTEM", got)
		}
		if got := fieldValueString(row[2]); got != "x" {
			t.Errorf("'x' = %q", got)
		}
		if got := fieldValueString(row[4]); got != "1" {
			t.Errorf("1 = %q", got)
		}
	})

	t.Run("scopes and unknown variables", func(t *testing.T) {
		names, row := columns(t, "SELECT @@GLOBAL.max_allowed_packet, @@local.wait_timeout, @@no_such_variable, @@session.no_such_variable")
		if want := []string{"@@GLOBAL.max_allowed_packet", "@@local.wait_timeout", "@@no_such_variable", "@@session.no_such_variable"}; !slices.Equal(names, want) {
			t.Fatalf("columns = %v, want %v", names, want)
		}
		if got := fieldValueString(row[0]); got != "67108864" {
			t.Errorf("@@GLOBAL.max_allowed_packet = %q", got)
		}
		if got := fieldValueString(row[1]); got != "28800" {
			t.Errorf("@@local.wait_timeout = %q", got)
		}
		if row[2].Type != mysql.FieldValueTypeNull || row[3].Type != mysql.FieldValueTypeNull {
			t.Errorf("unknown variables should be NULL, got %v and %v", row[2].Value(), row[3].Value())
		}
	})

	t.Run("connection id is the handshake id", func(t *testing.T) {
		_, row := columns(t, "SELECT CONNECTION_ID()")
		if got := fieldValueString(row[0]); got != "10007" {
			t.Errorf("CONNECTION_ID() = %q, want 10007", got)
		}
	})

	t.Run("database() is NULL without a database", func(t *testing.T) {
		result, err := handleLocalQuery("SELECT database()", localSession{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		row := parseTextRows(t, result.Resultset)[0]
		if row[0].Type != mysql.FieldValueTypeNull {
			t.Errorf("database() = %v, want NULL", row[0].Value())
		}
	})

	t.Run("literal containing a comma and a keyword stays one item", func(t *testing.T) {
		names, row := columns(t, "SELECT 'a, from b' AS s, @@version")
		if want := []string{"s", "@@version"}; !slices.Equal(names, want) {
			t.Fatalf("columns = %v, want %v", names, want)
		}
		if got := fieldValueString(row[0]); got != "a, from b" {
			t.Errorf("s = %q", got)
		}
	})
}

// tablePlusColumnsQuery is what TablePlus sends to fill a table's structure
// view on MySQL, with the data source name as the schema.
const tablePlusColumnsQuery = "SELECT ordinal_position as ordinal_position,column_name as column_name,column_type AS data_type,character_set_name as character_set,collation_name as collation,is_nullable as is_nullable,column_default as column_default,extra as extra,column_name AS foreign_key,column_comment AS comment FROM information_schema.columns WHERE table_schema='Analytics MySQL' AND table_name='users' ORDER BY ordinal_position"

func TestHandleInfoSchemaColumns(t *testing.T) {
	sess := localSession{
		dbName: "Analytics MySQL",
		schema: []redash.SchemaTable{
			{Name: "users", Columns: []redash.SchemaColumn{
				{Name: "id", Type: "int"},
				{Name: "name", Type: "varchar", Comment: "display name"},
				{Name: "email"},
			}},
			{Name: "orders", Columns: []redash.SchemaColumn{{Name: "id", Type: "int"}, {Name: "user_id", Type: "int"}}},
		},
	}

	// run answers sql and renders the result as column names and rows of text,
	// NULL for a null.
	run := func(t *testing.T, sess localSession, sql string) ([]string, [][]string) {
		t.Helper()
		result, err := handleLocalQuery(sql, sess)
		if err != nil {
			t.Fatalf("handleLocalQuery(%q): %v", sql, err)
		}
		if result == nil || result.Resultset == nil {
			t.Fatalf("handleLocalQuery(%q): no result set", sql)
		}
		names := make([]string, len(result.Fields))
		for i, f := range result.Fields {
			names[i] = string(f.Name)
		}
		var rows [][]string
		for _, r := range parseTextRows(t, result.Resultset) {
			row := make([]string, len(r))
			for i, v := range r {
				if v.Value() == nil {
					row[i] = "NULL"
				} else {
					row[i] = fieldValueString(v)
				}
			}
			rows = append(rows, row)
		}
		return names, rows
	}
	join := func(ss []string) string { return strings.Join(ss, "|") }

	t.Run("TablePlus structure view", func(t *testing.T) {
		names, rows := run(t, sess, tablePlusColumnsQuery)
		if got, want := join(names), "ordinal_position|column_name|data_type|character_set|collation|is_nullable|column_default|extra|foreign_key|comment"; got != want {
			t.Errorf("columns = %s\nwant %s", got, want)
		}
		want := []string{
			"1|id|int|NULL|NULL|NULL|NULL|NULL|id|",
			"2|name|varchar|NULL|NULL|NULL|NULL|NULL|name|display name",
			"3|email|NULL|NULL|NULL|NULL|NULL|NULL|email|",
		}
		if len(rows) != len(want) {
			t.Fatalf("got %d rows, want %d: %v", len(rows), len(want), rows)
		}
		for i, row := range rows {
			if join(row) != want[i] {
				t.Errorf("row %d = %s\nwant %s", i, join(row), want[i])
			}
		}
	})

	t.Run("DBeaver SELECT * in upper case", func(t *testing.T) {
		names, rows := run(t, sess, "SELECT * FROM information_schema.COLUMNS WHERE TABLE_SCHEMA='analytics mysql' AND TABLE_NAME='orders' ORDER BY ORDINAL_POSITION")
		// MySQL 8's information_schema.columns has 22 columns; a client reading
		// SELECT * by position relies on the real order.
		if len(names) != 22 || names[3] != "COLUMN_NAME" || names[7] != "DATA_TYPE" || names[15] != "COLUMN_TYPE" {
			t.Errorf("columns = %v", names)
		}
		if len(rows) != 2 {
			t.Fatalf("got %d rows, want 2: %v", len(rows), rows)
		}
		if rows[1][0] != "def" || rows[1][1] != "Analytics MySQL" || rows[1][2] != "orders" || rows[1][3] != "user_id" || rows[1][4] != "2" || rows[1][7] != "int" {
			t.Errorf("row = %v", rows[1])
		}
	})

	t.Run("qualified columns, IN and LIKE", func(t *testing.T) {
		names, rows := run(t, sess, "SELECT c.column_name, `c`.`TABLE_NAME` FROM information_schema.columns c WHERE c.table_name IN ('users', 'nothing') AND c.column_name LIKE '%id%'")
		if got := join(names); got != "c.column_name|`c`.`TABLE_NAME`" {
			t.Errorf("columns = %s", got)
		}
		if len(rows) != 1 || join(rows[0]) != "id|users" {
			t.Errorf("rows = %v, want [[id users]]", rows)
		}
	})

	t.Run("no WHERE lists every column of every table", func(t *testing.T) {
		_, rows := run(t, sess, "SELECT table_name, column_name FROM information_schema.columns")
		if len(rows) != 5 {
			t.Errorf("got %d rows, want 5", len(rows))
		}
	})

	t.Run("another schema has nothing", func(t *testing.T) {
		names, rows := run(t, sess, "SELECT column_name FROM information_schema.columns WHERE table_schema = 'other' AND table_name = 'users'")
		if len(rows) != 0 || join(names) != "column_name" {
			t.Errorf("rows = %v, columns = %v", rows, names)
		}
	})

	t.Run("OR across two tables admits both", func(t *testing.T) {
		_, rows := run(t, sess, "SELECT column_name FROM information_schema.columns WHERE table_name = 'users' OR table_name = 'orders'")
		if len(rows) != 5 {
			t.Errorf("got %d rows, want 5", len(rows))
		}
	})

	t.Run("literals keep their value and other expressions keep their place", func(t *testing.T) {
		names, rows := run(t, sess, "SELECT 'x' AS lit, 7 AS n, CASE column_key WHEN 'PRI' THEN 1 ELSE 0 END AS pk, UPPER(column_name) FROM information_schema.columns WHERE table_name = 'orders'")
		if got := join(names); got != "lit|n|pk|UPPER(column_name)" {
			t.Errorf("columns = %s", got)
		}
		if len(rows) != 2 || len(rows[0]) != 4 || rows[0][0] != "x" || rows[0][1] != "7" {
			t.Errorf("rows = %v", rows)
		}
	})

	t.Run("statistics is empty with the requested columns", func(t *testing.T) {
		names, rows := run(t, sess, "SELECT sub_part as index_length,index_name as index_name,index_type AS index_algorithm,CASE non_unique WHEN 0 THEN'TRUE'ELSE'FALSE'END AS is_unique,column_name as column_name FROM information_schema.statistics WHERE table_schema='Analytics MySQL' AND table_name='users'")
		if got := join(names); got != "index_length|index_name|index_algorithm|is_unique|column_name" {
			t.Errorf("columns = %s", got)
		}
		if len(rows) != 0 {
			t.Errorf("rows = %v, want none", rows)
		}
	})

	t.Run("no database selected", func(t *testing.T) {
		_, rows := run(t, localSession{}, tablePlusColumnsQuery)
		if len(rows) != 0 {
			t.Errorf("rows = %v, want none", rows)
		}
	})
}

// TestEmptyStringIsNotNull: go-mysql sends a Go "" as SQL NULL, so an empty
// text value from Redash used to reach the client as NULL. It must arrive as a
// zero-length string, distinct from a real NULL in the same column.
func TestEmptyStringIsNotNull(t *testing.T) {
	result, err := buildResult("SELECT s FROM t", &redash.QueryResult{
		Columns: []redash.Column{{Name: "s", Type: "string"}},
		Rows:    []map[string]any{{"s": ""}, {"s": nil}, {"s": "x"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := parseTextRows(t, result.Resultset)
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	if v := rows[0][0].Value(); v == nil || len(v.([]byte)) != 0 {
		t.Errorf("empty string came back as %#v, want a zero-length string", v)
	}
	if v := rows[1][0].Value(); v != nil {
		t.Errorf("NULL came back as %#v", v)
	}

	// The same through a locally answered query: init_connect is "".
	vars, err := handleLocalQuery("SHOW VARIABLES", localSession{})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range parseTextRows(t, vars.Resultset) {
		if fieldValueString(row[0]) == "init_connect" && row[1].Value() == nil {
			t.Error("init_connect came back as NULL, want an empty string")
		}
	}
}

// TestShowVariablesFilter: SHOW VARIABLES honours LIKE and WHERE, since a
// client asking for one variable reads whatever row comes first.
func TestShowVariablesFilter(t *testing.T) {
	tests := []struct {
		sql  string
		want []string
	}{
		{"SHOW VARIABLES LIKE 'lower_case%'", []string{"lower_case_table_names"}},
		{"show session variables like 'SQL_MODE'", []string{"sql_mode"}},
		{"SHOW GLOBAL VARIABLES WHERE Variable_name = 'version'", []string{"version"}},
		{"SHOW VARIABLES WHERE `Variable_name` IN ('autocommit', 'time_zone');", []string{"autocommit", "time_zone"}},
		{"SHOW VARIABLES LIKE 'no_such_variable'", nil},
	}
	for _, tt := range tests {
		t.Run(tt.sql, func(t *testing.T) {
			result, err := handleLocalQuery(tt.sql, localSession{})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, row := range parseTextRows(t, result.Resultset) {
				got = append(got, fieldValueString(row[0]))
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}

	// Without a filter, nothing the filtered forms found may be missing.
	result, err := handleLocalQuery("SHOW VARIABLES", localSession{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, row := range parseTextRows(t, result.Resultset) {
		got[fieldValueString(row[0])] = true
	}
	for _, tt := range tests {
		for _, name := range tt.want {
			if !got[name] {
				t.Errorf("unfiltered SHOW VARIABLES is missing %s", name)
			}
		}
	}
}
