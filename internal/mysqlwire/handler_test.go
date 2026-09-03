package mysqlwire

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/lhpalacio/redash-wire/internal/health"
	"github.com/lhpalacio/redash-wire/internal/redash"
	"github.com/lhpalacio/redash-wire/internal/testutil"
)

var testLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// newRawHandler builds a handler that has not authenticated yet, as go-mysql
// sees it while it is still reading the handshake.
func newRawHandler(t *testing.T, sources []redash.DataSource, mock *testutil.MockRedashAPI, gate *health.Gate, conns *connTable) *handler {
	t.Helper()
	return newHandler(context.Background(), testLogger, mock, testutil.NewMockSourceRegistry(sources), gate, conns, false)
}

// newTestHandler builds a handler past a successful login, with connection id 1.
func newTestHandler(t *testing.T, sources []redash.DataSource, mock *testutil.MockRedashAPI) *handler {
	t.Helper()
	h := newRawHandler(t, sources, mock, health.NewGate(), newConnTable())
	if err := h.login(1); err != nil {
		t.Fatalf("login: %v", err)
	}
	return h
}

func errorCode(t *testing.T, err error) uint16 {
	t.Helper()
	var myErr *mysql.MyError
	if !errors.As(err, &myErr) {
		t.Fatalf("err = %v, want a MySQL error", err)
	}
	return myErr.Code
}

// localValue runs a locally answered single-value statement on h and returns
// the value as a client prints it, "" for NULL. It is how a test observes the
// session's state: SELECT DATABASE() for the selected source, SELECT
// CONNECTION_ID() for the id, SHOW TABLES for the schema in use.
func localValue(t *testing.T, h *handler, sql string) string {
	t.Helper()
	result, err := h.HandleQuery(sql)
	if err != nil {
		t.Fatalf("HandleQuery(%q): %v", sql, err)
	}
	if result == nil || result.Resultset == nil {
		t.Fatalf("HandleQuery(%q): no result set", sql)
	}
	rows := parseTextRows(t, result.Resultset)
	if len(rows) != 1 || len(rows[0]) != 1 {
		t.Fatalf("HandleQuery(%q): want one value, got %d rows", sql, len(rows))
	}
	return fieldValueString(rows[0][0])
}

// capturingMock is a Redash mock that records the data source id of every
// remote query in dsID, which is how a test observes which source a session
// has selected.
func capturingMock(dsID *int) *testutil.MockRedashAPI {
	return &testutil.MockRedashAPI{
		ExecuteQueryFunc: func(_ context.Context, _ string, id int) (*redash.QueryResult, error) {
			*dsID = id
			return &redash.QueryResult{}, nil
		},
	}
}

// selectedSource runs a remote query on h and reports the data source id it
// reached, through a mock built by capturingMock over dsID.
func selectedSource(t *testing.T, h *handler, dsID *int) int {
	t.Helper()
	*dsID = 0
	if _, err := h.HandleQuery("SELECT 1 FROM t"); err != nil {
		t.Fatalf("remote query: %v", err)
	}
	return *dsID
}

func TestUseDBBeforeLogin(t *testing.T) {
	sources := []redash.DataSource{
		{ID: 1, Name: "prod_mysql", Type: "mysql"},
		{ID: 2, Name: "prod_pg", Type: "pg"},
	}

	t.Run("nothing is validated before the password is verified", func(t *testing.T) {
		gate := health.NewGate()
		gate.Fail(health.KindUnreachable, "dial tcp: i/o timeout")
		h := newRawHandler(t, sources, &testutil.MockRedashAPI{}, gate, nil)
		for _, db := range []string{"nonexistent", "prod_pg", "prod_mysql"} {
			if err := h.UseDB(db); err != nil {
				t.Errorf("UseDB(%q) before login = %v, want nil: it discloses the registry or the outage", db, err)
			}
		}
	})

	t.Run("unknown database is refused at login with 1049", func(t *testing.T) {
		h := newRawHandler(t, sources, &testutil.MockRedashAPI{}, health.NewGate(), nil)
		if err := h.UseDB("nonexistent"); err != nil {
			t.Fatalf("UseDB before login: %v", err)
		}
		err := h.login(5)
		if code := errorCode(t, err); code != mysql.ER_BAD_DB_ERROR {
			t.Errorf("login code = %d, want %d (%v)", code, mysql.ER_BAD_DB_ERROR, err)
		}
		if !strings.Contains(err.Error(), "Unknown database 'nonexistent'") {
			t.Errorf("login error = %q, want the MySQL wording", err)
		}
	})

	t.Run("non-MySQL data source is refused at login", func(t *testing.T) {
		h := newRawHandler(t, sources, &testutil.MockRedashAPI{}, health.NewGate(), nil)
		_ = h.UseDB("prod_pg")
		if code := errorCode(t, h.login(5)); code != mysql.ER_BAD_DB_ERROR {
			t.Errorf("login code = %d, want %d", code, mysql.ER_BAD_DB_ERROR)
		}
	})

	t.Run("valid database is selected at login", func(t *testing.T) {
		var dsID int
		h := newRawHandler(t, sources, capturingMock(&dsID), health.NewGate(), nil)
		_ = h.UseDB("prod_mysql")
		if err := h.login(5); err != nil {
			t.Fatalf("login: %v", err)
		}
		if got := localValue(t, h, "SELECT DATABASE()"); got != "prod_mysql" {
			t.Errorf("DATABASE() after login = %q, want prod_mysql", got)
		}
		if got := selectedSource(t, h, &dsID); got != 1 {
			t.Errorf("queries reach data source %d, want 1", got)
		}
		if got := localValue(t, h, "SELECT CONNECTION_ID()"); got != "5" {
			t.Errorf("CONNECTION_ID() = %q, want the id go-mysql assigned (5)", got)
		}
		// Once authenticated, USE validates right away.
		if code := errorCode(t, h.UseDB("nonexistent")); code != mysql.ER_BAD_DB_ERROR {
			t.Errorf("UseDB after login code = %d, want %d", code, mysql.ER_BAD_DB_ERROR)
		}
	})

	t.Run("gate refuses the login before the database is looked at", func(t *testing.T) {
		gate := health.NewGate()
		gate.Fail(health.KindUnreachable, "dial tcp: i/o timeout")
		h := newRawHandler(t, sources, &testutil.MockRedashAPI{}, gate, nil)
		_ = h.UseDB("nonexistent")
		err := h.login(5)
		if code := errorCode(t, err); code != mysql.ER_UNKNOWN_ERROR {
			t.Errorf("login code = %d, want %d", code, mysql.ER_UNKNOWN_ERROR)
		}
		if !strings.Contains(err.Error(), "unreachable") {
			t.Errorf("login error = %q, want it to name Redash as the cause", err)
		}
	})

	t.Run("gate refuses a login without a database", func(t *testing.T) {
		gate := health.NewGate()
		gate.Fail(health.KindRejected, "401")
		h := newRawHandler(t, sources, &testutil.MockRedashAPI{}, gate, nil)
		if err := h.login(5); err == nil || !strings.Contains(err.Error(), "rejected our credentials") {
			t.Errorf("login = %v, want the gate's message", err)
		}
	})
}

func TestStripDBQualifier(t *testing.T) {
	h := &handler{dbName: "orders"}
	tests := []struct {
		name, in, want string
	}{
		{"unquoted FROM", "SELECT * FROM orders.users", "SELECT * FROM users"},
		{"backtick FROM", "SELECT * FROM `orders`.`users`", "SELECT * FROM `users`"},
		{"double-quoted FROM", `SELECT * FROM "orders".users`, "SELECT * FROM users"},
		{"database name case", "SELECT * FROM ORDERS.users", "SELECT * FROM users"},
		{"FROM on its own line", "SELECT id\nFROM\n  orders.users\nLIMIT 5", "SELECT id\nFROM\n  users\nLIMIT 5"},
		{"JOIN", "SELECT * FROM orders.a JOIN `orders`.b ON a.id = b.a_id", "SELECT * FROM a JOIN b ON a.id = b.a_id"},
		{"LEFT JOIN", "SELECT * FROM x LEFT JOIN orders.y USING (id)", "SELECT * FROM x LEFT JOIN y USING (id)"},
		{"INSERT INTO", "INSERT INTO orders.t (a) VALUES (1)", "INSERT INTO t (a) VALUES (1)"},
		{"UPDATE", "UPDATE `orders`.t SET a = 1", "UPDATE t SET a = 1"},
		{"DELETE FROM", "DELETE FROM orders.t WHERE id = 1", "DELETE FROM t WHERE id = 1"},
		{
			"column qualifiers that happen to match the database survive",
			"SELECT `orders`.id, `customers`.id FROM orders JOIN customers ON `orders`.customer_id = `customers`.id",
			"SELECT `orders`.id, `customers`.id FROM orders JOIN customers ON `orders`.customer_id = `customers`.id",
		},
		{"table named like the database", "SELECT orders.id FROM orders.orders WHERE orders.total > 1", "SELECT orders.id FROM orders WHERE orders.total > 1"},
		{"ON DUPLICATE KEY UPDATE names a column, not a table", "INSERT INTO orders.t (a) VALUES (1) ON DUPLICATE KEY UPDATE orders.a = 1", "INSERT INTO t (a) VALUES (1) ON DUPLICATE KEY UPDATE orders.a = 1"},
		{"another database is left alone", "SELECT * FROM other.users", "SELECT * FROM other.users"},
		{"inside a literal", "SELECT * FROM orders.t WHERE s = 'from orders.users'", "SELECT * FROM t WHERE s = 'from orders.users'"},
		{"inside a comment", "SELECT * FROM orders.t -- from orders.x", "SELECT * FROM t -- from orders.x"},
		{"no qualifier", "SELECT * FROM users", "SELECT * FROM users"},

		// Statements a client sends to read a table's structure.
		{"SHOW CREATE TABLE", "SHOW CREATE TABLE `orders`.`users`", "SHOW CREATE TABLE `users`"},
		{"SHOW COLUMNS FROM db.table", "SHOW FULL COLUMNS FROM `orders`.`users`", "SHOW FULL COLUMNS FROM `users`"},
		{"SHOW COLUMNS FROM table FROM db", "SHOW COLUMNS FROM `users` FROM `orders`", "SHOW COLUMNS FROM `users`"},
		{"SHOW COLUMNS FROM table IN db LIKE", "SHOW COLUMNS FROM users IN orders LIKE 'id%'", "SHOW COLUMNS FROM users LIKE 'id%'"},
		{"SHOW INDEX FROM db.table", "SHOW INDEX FROM orders.users", "SHOW INDEX FROM users"},
		{"SHOW KEYS FROM table FROM db", "SHOW KEYS FROM users FROM orders", "SHOW KEYS FROM users"},
		{"SHOW TABLE STATUS FROM db", "SHOW TABLE STATUS FROM `orders` LIKE 'users'", "SHOW TABLE STATUS LIKE 'users'"},
		{"SHOW TABLE STATUS IN db", "SHOW TABLE STATUS IN orders", "SHOW TABLE STATUS"},
		{"SHOW TRIGGERS FROM db", "SHOW TRIGGERS FROM orders", "SHOW TRIGGERS"},
		{"DESCRIBE", "DESCRIBE `orders`.`users`", "DESCRIBE `users`"},
		{"DESC", "DESC orders.users", "DESC users"},
		{"EXPLAIN table", "EXPLAIN orders.users", "EXPLAIN users"},
		{"EXPLAIN query", "EXPLAIN SELECT * FROM orders.users", "EXPLAIN SELECT * FROM users"},
		{"CREATE TABLE", "CREATE TABLE orders.t (id INT)", "CREATE TABLE t (id INT)"},
		{"ALTER TABLE", "ALTER TABLE `orders`.`t` ADD COLUMN a INT", "ALTER TABLE `t` ADD COLUMN a INT"},
		{"DROP TABLE", "DROP TABLE orders.t", "DROP TABLE t"},
		{"TRUNCATE TABLE", "TRUNCATE TABLE orders.t", "TRUNCATE TABLE t"},

		// Positions that only look like the forms above.
		{"ORDER BY DESC is not DESCRIBE", "SELECT * FROM orders.t ORDER BY a DESC", "SELECT * FROM t ORDER BY a DESC"},
		{"IN outside SHOW is a set", "SELECT * FROM orders.t WHERE a IN (1, 2)", "SELECT * FROM t WHERE a IN (1, 2)"},
		{"IN list naming the database outside SHOW survives", "SELECT * FROM t WHERE name IN orders", "SELECT * FROM t WHERE name IN orders"},
		{"SHOW COLUMNS FROM a table named like the database", "SHOW COLUMNS FROM orders", "SHOW COLUMNS FROM orders"},
		{"SHOW INDEX FROM a table named like the database, FROM db", "SHOW INDEX FROM orders FROM orders", "SHOW INDEX FROM orders"},
		{"SHOW ... FROM another database survives", "SHOW TABLE STATUS FROM other", "SHOW TABLE STATUS FROM other"},
		{"SHOW ... FROM db inside a literal", "SHOW TABLE STATUS LIKE 'from orders'", "SHOW TABLE STATUS LIKE 'from orders'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := h.stripDBQualifier(tt.in); got != tt.want {
				t.Errorf("stripDBQualifier(%q)\n got %q\nwant %q", tt.in, got, tt.want)
			}
		})
	}

	t.Run("no database selected", func(t *testing.T) {
		if got := (&handler{}).stripDBQualifier("SELECT * FROM orders.users"); got != "SELECT * FROM orders.users" {
			t.Errorf("got %q, want the query untouched", got)
		}
	})
}

func TestKill(t *testing.T) {
	sources := []redash.DataSource{{ID: 1, Name: "prod_mysql", Type: "mysql"}}

	t.Run("parse", func(t *testing.T) {
		tests := []struct {
			sql       string
			id        uint32
			queryOnly bool
			ok        bool
			code      uint16
		}{
			{"KILL QUERY 10002", 10002, true, true, 0},
			{"kill connection 7", 7, false, true, 0},
			{"KILL 7;", 7, false, true, 0},
			{"KILL", 0, false, true, mysql.ER_PARSE_ERROR},
			{"KILL QUERY abc", 0, false, true, mysql.ER_PARSE_ERROR},
			{"KILL QUERY 1 2", 0, false, true, mysql.ER_PARSE_ERROR},
			{"KILLER 1", 0, false, false, 0},
			{"SELECT 1", 0, false, false, 0},
		}
		for _, tt := range tests {
			id, queryOnly, ok, err := parseKill(tt.sql)
			if ok != tt.ok {
				t.Errorf("parseKill(%q) ok = %v, want %v", tt.sql, ok, tt.ok)
				continue
			}
			if !ok {
				continue
			}
			if tt.code != 0 {
				if code := errorCode(t, err); code != tt.code {
					t.Errorf("parseKill(%q) code = %d, want %d", tt.sql, code, tt.code)
				}
				continue
			}
			if err != nil || id != tt.id || queryOnly != tt.queryOnly {
				t.Errorf("parseKill(%q) = (%d, %v, %v), want (%d, %v, nil)", tt.sql, id, queryOnly, err, tt.id, tt.queryOnly)
			}
		}
	})

	t.Run("unknown id is 1094 and never reaches Redash", func(t *testing.T) {
		called := false
		mock := &testutil.MockRedashAPI{
			ExecuteQueryFunc: func(_ context.Context, _ string, _ int) (*redash.QueryResult, error) {
				called = true
				return &redash.QueryResult{}, nil
			},
		}
		h := newTestHandler(t, sources, mock)
		if err := h.UseDB("prod_mysql"); err != nil {
			t.Fatal(err)
		}
		_, err := h.HandleQuery("KILL QUERY 99999")
		if code := errorCode(t, err); code != mysql.ER_NO_SUCH_THREAD {
			t.Errorf("code = %d, want %d", code, mysql.ER_NO_SUCH_THREAD)
		}
		if called {
			t.Error("KILL was forwarded to Redash")
		}
	})

	t.Run("KILL QUERY interrupts the target's query", func(t *testing.T) {
		conns := newConnTable()
		started := make(chan struct{})
		var once sync.Once
		var executions atomic.Int32
		mock := &testutil.MockRedashAPI{
			ExecuteQueryFunc: func(ctx context.Context, _ string, _ int) (*redash.QueryResult, error) {
				executions.Add(1)
				once.Do(func() { close(started) })
				<-ctx.Done()
				return nil, ctx.Err()
			},
		}
		victim := newRawHandler(t, sources, mock, health.NewGate(), conns)
		_ = victim.UseDB("prod_mysql")
		if err := victim.login(10001); err != nil {
			t.Fatal(err)
		}
		disconnected := false
		conns.add(10001, victim, func() { disconnected = true })

		killer := newRawHandler(t, sources, mock, health.NewGate(), conns)
		if err := killer.login(10002); err != nil {
			t.Fatal(err)
		}

		done := make(chan error, 1)
		go func() {
			_, err := victim.HandleQuery("SELECT * FROM users")
			done <- err
		}()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("the victim's query never reached Redash")
		}

		if _, err := killer.HandleQuery("KILL QUERY 10001"); err != nil {
			t.Fatalf("KILL QUERY: %v", err)
		}
		select {
		case err := <-done:
			if code := errorCode(t, err); code != mysql.ER_QUERY_INTERRUPTED {
				t.Errorf("victim's query code = %d, want %d (%v)", code, mysql.ER_QUERY_INTERRUPTED, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the victim's query was not interrupted")
		}
		if disconnected {
			t.Error("KILL QUERY closed the connection")
		}
		if got := executions.Load(); got != 1 {
			t.Errorf("Redash saw %d queries, want 1: KILL must not be forwarded", got)
		}

		// KILL CONNECTION on an idle session just closes it.
		if _, err := killer.HandleQuery("KILL 10001"); err != nil {
			t.Fatalf("KILL: %v", err)
		}
		if !disconnected {
			t.Error("KILL CONNECTION did not close the connection")
		}
	})

	t.Run("KILL QUERY on an idle session is a no-op", func(t *testing.T) {
		conns := newConnTable()
		idle := newRawHandler(t, sources, &testutil.MockRedashAPI{}, health.NewGate(), conns)
		_ = idle.login(10003)
		conns.add(10003, idle, func() {})
		h := newRawHandler(t, sources, &testutil.MockRedashAPI{}, health.NewGate(), conns)
		_ = h.login(10004)
		if _, err := h.HandleQuery("KILL QUERY 10003"); err != nil {
			t.Errorf("KILL QUERY on an idle session = %v, want OK", err)
		}
	})
}

func TestConnectionID(t *testing.T) {
	h := newRawHandler(t, nil, &testutil.MockRedashAPI{}, health.NewGate(), nil)
	if err := h.login(10042); err != nil {
		t.Fatal(err)
	}
	result, err := h.HandleQuery("SELECT CONNECTION_ID()")
	if err != nil {
		t.Fatal(err)
	}
	rows := parseTextRows(t, result.Resultset)
	if got := fieldValueString(rows[0][0]); got != "10042" {
		t.Errorf("CONNECTION_ID() = %q, want the id go-mysql assigned (10042)", got)
	}
}

func TestUseDB(t *testing.T) {
	sources := []redash.DataSource{
		{ID: 1, Name: "prod_mysql", Type: "mysql"},
		{ID: 2, Name: "prod_pg", Type: "pg"},
	}

	t.Run("valid MySQL source", func(t *testing.T) {
		var dsID int
		h := newTestHandler(t, sources, capturingMock(&dsID))
		if err := h.UseDB("prod_mysql"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := localValue(t, h, "SELECT DATABASE()"); got != "prod_mysql" {
			t.Errorf("DATABASE() = %q, want prod_mysql", got)
		}
		if got := selectedSource(t, h, &dsID); got != 1 {
			t.Errorf("queries reach data source %d, want 1", got)
		}
	})

	t.Run("non-MySQL source returns error", func(t *testing.T) {
		h := newTestHandler(t, sources, &testutil.MockRedashAPI{})
		err := h.UseDB("prod_pg")
		if err == nil {
			t.Fatal("expected error for non-MySQL source, got nil")
		}
	})

	t.Run("nonexistent source returns error", func(t *testing.T) {
		h := newTestHandler(t, sources, &testutil.MockRedashAPI{})
		err := h.UseDB("nonexistent")
		if err == nil {
			t.Fatal("expected error for nonexistent source, got nil")
		}
	})

	t.Run("USE switches the table list to the new source", func(t *testing.T) {
		two := []redash.DataSource{
			{ID: 1, Name: "prod_mysql", Type: "mysql"},
			{ID: 3, Name: "staging_mysql", Type: "mysql"},
		}
		mock := &testutil.MockRedashAPI{
			GetSchemaFunc: func(_ context.Context, dsID int) ([]redash.SchemaTable, error) {
				return []redash.SchemaTable{{Name: fmt.Sprintf("table_of_%d", dsID)}}, nil
			},
		}
		h := newTestHandler(t, two, mock)

		if err := h.UseDB("prod_mysql"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := localValue(t, h, "SHOW TABLES"); got != "table_of_1" {
			t.Fatalf("SHOW TABLES = %q, want table_of_1", got)
		}

		// The schema is cached for the session; switching source must not keep
		// serving the old source's tables.
		if err := h.UseDB("staging_mysql"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := localValue(t, h, "SHOW TABLES"); got != "table_of_3" {
			t.Errorf("after USE staging_mysql, SHOW TABLES = %q, want table_of_3", got)
		}

		// Re-selecting the current source keeps answering for it.
		if err := h.UseDB("staging_mysql"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := localValue(t, h, "SHOW TABLES"); got != "table_of_3" {
			t.Errorf("after a repeated USE, SHOW TABLES = %q, want table_of_3", got)
		}
	})

	t.Run("case insensitive lookup", func(t *testing.T) {
		var dsID int
		h := newTestHandler(t, sources, capturingMock(&dsID))
		if err := h.UseDB("PROD_MYSQL"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := selectedSource(t, h, &dsID); got != 1 {
			t.Errorf("queries reach data source %d, want 1", got)
		}
		// The session reports the source's own name, not the client's spelling.
		if got := localValue(t, h, "SELECT DATABASE()"); got != "prod_mysql" {
			t.Errorf("DATABASE() = %q, want prod_mysql", got)
		}
	})
}

func TestHandleQuery_Routing(t *testing.T) {
	sources := []redash.DataSource{
		{ID: 1, Name: "prod_mysql", Type: "mysql"},
		{ID: 2, Name: "staging_mysql", Type: "mysql"},
	}

	t.Run("local query does not call ExecuteQuery", func(t *testing.T) {
		called := false
		mock := &testutil.MockRedashAPI{
			ExecuteQueryFunc: func(_ context.Context, _ string, _ int) (*redash.QueryResult, error) {
				called = true
				return &redash.QueryResult{}, nil
			},
		}
		h := newTestHandler(t, sources, mock)

		_, err := h.HandleQuery("SET NAMES utf8mb4")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if called {
			t.Error("ExecuteQuery should not be called for local queries")
		}
	})

	t.Run("remote query with data source calls ExecuteQuery", func(t *testing.T) {
		var capturedSQL string
		var capturedDSID int
		mock := &testutil.MockRedashAPI{
			ExecuteQueryFunc: func(_ context.Context, sql string, dsID int) (*redash.QueryResult, error) {
				capturedSQL = sql
				capturedDSID = dsID
				return &redash.QueryResult{
					Columns: []redash.Column{{Name: "id", Type: "integer"}},
					Rows:    []map[string]any{{"id": 1}},
				}, nil
			},
		}
		h := newTestHandler(t, sources, mock)

		if err := h.UseDB("prod_mysql"); err != nil {
			t.Fatalf("UseDB: %v", err)
		}

		result, err := h.HandleQuery("SELECT * FROM users")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if capturedSQL != "SELECT * FROM users" {
			t.Errorf("ExecuteQuery SQL = %q, want %q", capturedSQL, "SELECT * FROM users")
		}
		if capturedDSID != 1 {
			t.Errorf("ExecuteQuery dataSourceID = %d, want 1", capturedDSID)
		}
		if result == nil || result.Resultset == nil {
			t.Fatal("expected result with Resultset")
		}
	})

	t.Run("remote query without data source returns error", func(t *testing.T) {
		mock := &testutil.MockRedashAPI{}
		h := newTestHandler(t, sources, mock)

		_, err := h.HandleQuery("SELECT * FROM users")
		if err == nil {
			t.Fatal("expected error when no database selected, got nil")
		}
	})

	t.Run("USE command switches database", func(t *testing.T) {
		var dsID int
		h := newTestHandler(t, sources, capturingMock(&dsID))

		if _, err := h.HandleQuery("USE prod_mysql"); err != nil {
			t.Fatalf("HandleQuery(USE): %v", err)
		}
		if got := selectedSource(t, h, &dsID); got != 1 {
			t.Errorf("after USE, queries reach data source %d, want 1", got)
		}

		if _, err := h.HandleQuery("USE staging_mysql"); err != nil {
			t.Fatalf("HandleQuery(USE staging): %v", err)
		}
		if got := selectedSource(t, h, &dsID); got != 2 {
			t.Errorf("after the second USE, queries reach data source %d, want 2", got)
		}
		if got := localValue(t, h, "SELECT DATABASE()"); got != "staging_mysql" {
			t.Errorf("DATABASE() = %q, want staging_mysql", got)
		}
	})

	t.Run("USE with backticks and semicolons", func(t *testing.T) {
		h := newTestHandler(t, sources, &testutil.MockRedashAPI{})

		if _, err := h.HandleQuery("USE `prod_mysql`;"); err != nil {
			t.Fatalf("HandleQuery(USE with backticks): %v", err)
		}
		if got := localValue(t, h, "SELECT DATABASE()"); got != "prod_mysql" {
			t.Errorf("DATABASE() = %q, want prod_mysql", got)
		}
	})

	t.Run("empty query returns nil", func(t *testing.T) {
		mock := &testutil.MockRedashAPI{}
		h := newTestHandler(t, sources, mock)

		result, err := h.HandleQuery("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result != nil {
			t.Errorf("expected nil result for empty query, got %v", result)
		}
	})

	t.Run("whitespace-only query returns nil", func(t *testing.T) {
		mock := &testutil.MockRedashAPI{}
		h := newTestHandler(t, sources, mock)

		result, err := h.HandleQuery("   ")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result != nil {
			t.Errorf("expected nil result for whitespace query, got %v", result)
		}
	})
}
