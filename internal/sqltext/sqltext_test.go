package sqltext

import (
	"reflect"
	"testing"
)

func TestStatements(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want []string
	}{
		{"single", "SELECT 1", []string{"SELECT 1"}},
		{"trailing semicolon", "SELECT 1;", []string{"SELECT 1"}},
		{"two statements", "BEGIN; SELECT 1", []string{"BEGIN", "SELECT 1"}},
		{"three with commit", "BEGIN; UPDATE t SET x=1 WHERE id=5; COMMIT", []string{"BEGIN", "UPDATE t SET x=1 WHERE id=5", "COMMIT"}},
		{"semicolon in string", "SELECT ';'", []string{"SELECT ';'"}},
		{"semicolon in line comment", "SELECT 1 -- a; b\n", []string{"SELECT 1 -- a; b"}},
		{"semicolon in block comment", "SELECT 1 /* a; b */", []string{"SELECT 1 /* a; b */"}},
		{"semicolon in dollar quote", "SELECT $$a;b$$", []string{"SELECT $$a;b$$"}},
		{"empty", "   ;  ;  ", nil},
		{"doubled quote escape", "SELECT 'it''s; here'", []string{"SELECT 'it''s; here'"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Statements(tt.sql)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Statements(%q) = %#v, want %#v", tt.sql, got, tt.want)
			}
		})
	}
}

func TestStatementsEdgeCases(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want []string
	}{
		{"dollar quote with tag", "SELECT $tag$a;b$tag$ ; SELECT 2", []string{"SELECT $tag$a;b$tag$", "SELECT 2"}},
		{"unterminated single quote", "SELECT 'abc; SELECT 2", []string{"SELECT 'abc; SELECT 2"}},
		{"unterminated block comment", "SELECT 1 /* ; SELECT 2", []string{"SELECT 1 /* ; SELECT 2"}},
		{"backslash escaped quote (mysql single)", `SELECT 'a\'; b'; SELECT 2`, []string{`SELECT 'a\'; b'`, "SELECT 2"}},
		{"backslash escaped quote (mysql double)", `SELECT "a\"; b"; SELECT 2`, []string{`SELECT "a\"; b"`, "SELECT 2"}},
		{"backtick identifier with semicolon", "SELECT `a;b`; SELECT 2", []string{"SELECT `a;b`", "SELECT 2"}},
		{"only comments", "-- just a comment\n", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Statements(tt.sql)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Statements(%q) = %#v, want %#v", tt.sql, got, tt.want)
			}
		})
	}
}

func TestIsMultiStatement(t *testing.T) {
	if IsMultiStatement("SELECT 1;") {
		t.Error("single statement with trailing ; should not be multi")
	}
	if !IsMultiStatement("BEGIN; SELECT 1; COMMIT") {
		t.Error("expected multi-statement")
	}
	if IsMultiStatement("SELECT ';;;'") {
		t.Error("semicolons inside a literal must not count")
	}
}

func TestRedact(t *testing.T) {
	tests := []struct {
		sql      string
		contains string // a token that should NOT survive redaction
		survives string // a token that SHOULD survive
	}{
		{"SELECT * FROM sales WHERE d LIKE '%information_schema.%'", "information_schema.", "sales"},
		{"SELECT 1 /* pg_type */ FROM t", "pg_type", "from t"},
		{"SELECT col FROM information_schema.tables", "", "information_schema.tables"},
	}
	for _, tt := range tests {
		r := Redact(tt.sql)
		if len(r) != len(tt.sql) {
			t.Errorf("Redact changed length: %d != %d", len(r), len(tt.sql))
		}
		if tt.contains != "" && containsLower(r, tt.contains) {
			t.Errorf("Redact(%q) should have hidden %q, got %q", tt.sql, tt.contains, r)
		}
		if tt.survives != "" && !containsLower(r, tt.survives) {
			t.Errorf("Redact(%q) should have kept %q, got %q", tt.sql, tt.survives, r)
		}
	}
}

func TestSplitTopLevelCommas(t *testing.T) {
	tests := []struct {
		sql  string
		want []string
	}{
		{"a, b, c", []string{"a", " b", " c"}},
		{"attname, format_type(atttypid, atttypmod) as data_type", []string{"attname", " format_type(atttypid, atttypmod) as data_type"}},
		{"'a,b', c", []string{"'a,b'", " c"}},
		{`"my, col", id`, []string{`"my, col"`, " id"}},
		{"coalesce(a, b, c)", []string{"coalesce(a, b, c)"}},
	}
	for _, tt := range tests {
		got := SplitTopLevelCommas(tt.sql)
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("SplitTopLevelCommas(%q) = %#v, want %#v", tt.sql, got, tt.want)
		}
	}
}

func TestReplaceOutsideStrings(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		old  string
		want string
	}{
		{
			name: "strips backtick-qualified db name with spaces",
			sql:  "SELECT * FROM `Covercheck - Write`.`account`",
			old:  "`Covercheck - Write`.",
			want: "SELECT * FROM `account`",
		},
		{
			name: "strips double-quoted qualifier",
			sql:  `SELECT * FROM "mydb"."account"`,
			old:  `"mydb".`,
			want: `SELECT * FROM "account"`,
		},
		{
			name: "leaves qualifier inside a string literal untouched",
			sql:  "SELECT '`mydb`.x' AS note FROM `mydb`.t",
			old:  "`mydb`.",
			want: "SELECT '`mydb`.x' AS note FROM t",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ReplaceOutsideStrings(tt.sql, tt.old, ""); got != tt.want {
				t.Errorf("ReplaceOutsideStrings(%q, %q) = %q, want %q", tt.sql, tt.old, got, tt.want)
			}
		})
	}
}

func TestContainsToken(t *testing.T) {
	if !ContainsToken("select * from pg_class c", "pg_class") {
		t.Error("expected whole-word pg_class match")
	}
	if ContainsToken("select * from pg_classification_rules", "pg_class") {
		t.Error("pg_class must not match inside pg_classification_rules")
	}
	if !ContainsToken("select pg_catalog.pg_type x", "pg_catalog.") {
		t.Error("token ending in '.' should match")
	}
	if !ContainsToken("select version()", "version(") {
		t.Error("token ending in '(' should match")
	}
}

func containsLower(s, sub string) bool {
	return len(s) >= len(sub) && indexLower(s, sub) >= 0
}

func indexLower(s, sub string) int {
	ls, lsub := toLower(s), toLower(sub)
	for i := 0; i+len(lsub) <= len(ls); i++ {
		if ls[i:i+len(lsub)] == lsub {
			return i
		}
	}
	return -1
}

func toLower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}
