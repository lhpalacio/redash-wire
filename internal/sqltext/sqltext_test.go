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
			got := Postgres.Statements(tt.sql)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Statements(%q) = %#v, want %#v", tt.sql, got, tt.want)
			}
		})
	}
}

func TestStatementsEdgeCases(t *testing.T) {
	tests := []struct {
		name string
		d    Dialect
		sql  string
		want []string
	}{
		{"dollar quote with tag", Postgres, "SELECT $tag$a;b$tag$ ; SELECT 2", []string{"SELECT $tag$a;b$tag$", "SELECT 2"}},
		{"unterminated single quote", Postgres, "SELECT 'abc; SELECT 2", []string{"SELECT 'abc; SELECT 2"}},
		{"unterminated block comment", Postgres, "SELECT 1 /* ; SELECT 2", []string{"SELECT 1 /* ; SELECT 2"}},
		{"backslash escaped quote (mysql single)", MySQL, `SELECT 'a\'; b'; SELECT 2`, []string{`SELECT 'a\'; b'`, "SELECT 2"}},
		{"backslash escaped quote (mysql double)", MySQL, `SELECT "a\"; b"; SELECT 2`, []string{`SELECT "a\"; b"`, "SELECT 2"}},
		{"backtick identifier with semicolon", MySQL, "SELECT `a;b`; SELECT 2", []string{"SELECT `a;b`", "SELECT 2"}},
		{"only comments", Postgres, "-- just a comment\n", nil},

		// Postgres: a backslash is an ordinary byte, so 'C:\' is a complete
		// literal and the second statement is visible to the splitter.
		{"backslash ends a postgres literal", Postgres, `SELECT 'C:\'; DELETE FROM users --'`, []string{`SELECT 'C:\'`, "DELETE FROM users --'"}},
		{"backslash inside postgres literal is plain", Postgres, `SELECT 'a\b'; SELECT 2`, []string{`SELECT 'a\b'`, "SELECT 2"}},
		{"postgres E-string honors backslash", Postgres, `SELECT E'it\'s; here'; SELECT 2`, []string{`SELECT E'it\'s; here'`, "SELECT 2"}},
		{"identifier ending in e is not an E-string", Postgres, `SELECT type'\'; SELECT 2`, []string{`SELECT type'\'`, "SELECT 2"}},
		{"mysql backslash-terminated literal swallows the rest", MySQL, `SELECT 'C:\'; SELECT 2`, []string{`SELECT 'C:\'; SELECT 2`}},

		// Comments.
		{"postgres nested block comment", Postgres, "SELECT 1 /* a /* b */ ; DROP TABLE x */", []string{"SELECT 1 /* a /* b */ ; DROP TABLE x */"}},
		{"mysql block comment does not nest", MySQL, "SELECT 1 /* a /* b */ ; SELECT 2 */", []string{"SELECT 1 /* a /* b */", "SELECT 2 */"}},
		{"mysql hash comment", MySQL, "SELECT 1; # trailing note", []string{"SELECT 1"}},
		{"mysql hash comment hides a quote", MySQL, "SELECT 1; # it's fine\nSELECT 2", []string{"SELECT 1", "# it's fine\nSELECT 2"}},
		{"postgres hash is code", Postgres, "SELECT 1 # 2; SELECT 3", []string{"SELECT 1 # 2", "SELECT 3"}},

		// MySQL has no dollar quoting; $ is an identifier byte.
		{"mysql dollar in identifier", MySQL, "SELECT a$b$c; SELECT 2", []string{"SELECT a$b$c", "SELECT 2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.d.Statements(tt.sql)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("%v.Statements(%q) = %#v, want %#v", tt.d, tt.sql, got, tt.want)
			}
		})
	}
}

func TestIsMultiStatement(t *testing.T) {
	if Postgres.IsMultiStatement("SELECT 1;") {
		t.Error("single statement with trailing ; should not be multi")
	}
	if !Postgres.IsMultiStatement("BEGIN; SELECT 1; COMMIT") {
		t.Error("expected multi-statement")
	}
	if Postgres.IsMultiStatement("SELECT ';;;'") {
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
		r := Postgres.Redact(tt.sql)
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
		got := Postgres.SplitTopLevelCommas(tt.sql)
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
			if got := MySQL.ReplaceOutsideStrings(tt.sql, tt.old, ""); got != tt.want {
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

func TestWriteVerb(t *testing.T) {
	tests := []struct {
		name string
		d    Dialect
		sql  string
		want string
	}{
		// Plain reads.
		{"select", Postgres, "SELECT * FROM users", ""},
		{"select with trailing semicolon and comment", Postgres, "SELECT 1; -- delete me\n", ""},
		{"leading comment", Postgres, "/* update */ SELECT 1", ""},
		{"parenthesised union", Postgres, "(SELECT 1) UNION (SELECT 2)", ""},
		{"with cte", Postgres, "WITH x AS (SELECT 1) SELECT * FROM x", ""},
		{"values", Postgres, "VALUES (1), (2)", ""},
		{"table", Postgres, "TABLE users", ""},
		{"show mysql", MySQL, "SHOW CREATE TABLE users", ""},
		{"describe", MySQL, "DESCRIBE users", ""},
		{"desc", MySQL, "DESC users", ""},
		{"keyword inside a literal", Postgres, "SELECT 'DELETE FROM users' AS note", ""},
		{"keyword inside a quoted identifier", Postgres, `SELECT "delete" FROM t`, ""},
		{"keyword inside a comment", MySQL, "SELECT 1 # update later", ""},
		{"keyword as a longer identifier", Postgres, "SELECT updated_at, deleted, inserted_by FROM t", ""},
		{"replace function is not a write", MySQL, "SELECT REPLACE(name, 'a', 'b') FROM t", ""},
		{"insert function is not a write", MySQL, "SELECT INSERT('abc', 1, 1, 'x')", ""},
		{"in list is not into", Postgres, "SELECT 1 WHERE id IN (1, 2)", ""},
		{"explain select", Postgres, "EXPLAIN SELECT * FROM users", ""},
		{"explain analyze select", Postgres, "EXPLAIN ANALYZE SELECT * FROM users", ""},
		{"explain with option list", Postgres, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) SELECT 1", ""},
		{"explain format=json mysql", MySQL, "EXPLAIN FORMAT=JSON SELECT 1", ""},
		{"explain table is describe on mysql", MySQL, "EXPLAIN users", ""},
		{"explain for connection", MySQL, "EXPLAIN FOR CONNECTION 5", ""},
		{"empty", Postgres, "   ;  ", ""},

		// Writes by lead keyword.
		{"insert", Postgres, "INSERT INTO t (id) VALUES (1)", "INSERT"},
		{"update", Postgres, "UPDATE t SET x = 1", "UPDATE"},
		{"delete", MySQL, "DELETE FROM t WHERE x = 1", "DELETE"},
		{"delete lowercase with leading whitespace", Postgres, "\n  delete from t", "DELETE"},
		{"drop", Postgres, "DROP TABLE users", "DROP"},
		{"create", MySQL, "CREATE TABLE t (id int)", "CREATE"},
		{"alter", Postgres, "ALTER TABLE t ADD COLUMN x int", "ALTER"},
		{"truncate", MySQL, "TRUNCATE TABLE t", "TRUNCATE"},
		{"replace into", MySQL, "REPLACE INTO t VALUES (1)", "REPLACE"},
		{"merge", Postgres, "MERGE INTO t USING s ON t.id = s.id WHEN MATCHED THEN DELETE", "MERGE"},
		{"grant", Postgres, "GRANT ALL ON t TO alice", "GRANT"},
		{"call", MySQL, "CALL cleanup()", "CALL"},
		{"do block", Postgres, "DO $$ BEGIN DELETE FROM t; END $$", "DO"},
		{"copy", Postgres, "COPY t FROM STDIN", "COPY"},
		{"lock", Postgres, "LOCK TABLE t", "LOCK"},
		{"load data", MySQL, "LOAD DATA INFILE '/tmp/x' INTO TABLE t", "LOAD"},
		{"vacuum", Postgres, "VACUUM t", "VACUUM"},
		{"refresh materialized view", Postgres, "REFRESH MATERIALIZED VIEW mv", "REFRESH"},
		{"unknown lead", Postgres, "FROBNICATE t", "FROBNICATE"},
		{"explain table on postgres is unknown", Postgres, "EXPLAIN users", "USERS"},

		// Writes hidden inside a read.
		{"data-modifying cte delete", Postgres, "WITH d AS (DELETE FROM t RETURNING *) SELECT * FROM d", "DELETE"},
		{"data-modifying cte insert", Postgres, "WITH i AS (INSERT INTO t VALUES (1) RETURNING id) SELECT * FROM i", "INSERT"},
		{"cte then update", Postgres, "WITH x AS (SELECT 1) UPDATE t SET a = 1", "UPDATE"},
		{"select into", Postgres, "SELECT * INTO new_table FROM t", "INTO"},
		{"select into outfile", MySQL, "SELECT * FROM t INTO OUTFILE '/tmp/x'", "INTO"},
		{"select for update", Postgres, "SELECT * FROM t FOR UPDATE", "UPDATE"},
		{"explain analyze update", Postgres, "EXPLAIN ANALYZE UPDATE t SET x = 1", "UPDATE"},
		{"explain delete mysql", MySQL, "EXPLAIN DELETE FROM t", "DELETE"},
		{"explain with option list before insert", Postgres, "EXPLAIN (ANALYZE) INSERT INTO t VALUES (1)", "INSERT"},
		{"nested explain", Postgres, "EXPLAIN EXPLAIN DELETE FROM t", "DELETE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.d.WriteVerb(tt.sql); got != tt.want {
				t.Errorf("%v.WriteVerb(%q) = %q, want %q", tt.d, tt.sql, got, tt.want)
			}
		})
	}
}
