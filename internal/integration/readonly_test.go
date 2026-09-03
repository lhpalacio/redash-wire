package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lhpalacio/redash-wire/internal/mysqlwire"
	"github.com/lhpalacio/redash-wire/internal/proxy"
	"github.com/lhpalacio/redash-wire/internal/redash"
	"github.com/lhpalacio/redash-wire/internal/testutil"
)

// countingMock records how many statements reached Redash, which is the whole
// point of read-only mode: a refused write must never have left the proxy.
func countingMock(t *testing.T) (*testutil.MockRedashAPI, *testutil.MockSourceRegistry, *atomic.Int32) {
	mock, registry := defaultMockAndRegistry(t)
	var calls atomic.Int32
	mock.ExecuteQueryFunc = func(ctx context.Context, sql string, dataSourceID int) (*redash.QueryResult, error) {
		calls.Add(1)
		return &redash.QueryResult{
			Columns: []redash.Column{{Name: "id", Type: "integer"}},
			Rows:    []map[string]any{{"id": 1}},
		}, nil
	}
	return mock, registry, &calls
}

func TestPG_ReadOnlyRefusesWritesBeforeRedash(t *testing.T) {
	mock, registry, calls := countingMock(t)
	addr := startPGServer(t, mock, registry, proxy.WithReadOnly(true))
	conn := connectPG(t, addr, "Production PG")

	// The mode is announced at connect time, the way PostgreSQL 14 does.
	if got := conn.PgConn().ParameterStatus("default_transaction_read_only"); got != "on" {
		t.Errorf("default_transaction_read_only parameter = %q, want on", got)
	}

	refused := []struct {
		name string
		sql  string
		verb string
	}{
		{"update", "UPDATE customers SET name = 'x' WHERE id = 1", "UPDATE"},
		{"delete", "DELETE FROM customers", "DELETE"},
		{"insert", "INSERT INTO customers (id) VALUES (1)", "INSERT"},
		{"drop", "DROP TABLE customers", "DROP"},
		{"data-modifying cte", "WITH d AS (DELETE FROM customers RETURNING *) SELECT * FROM d", "DELETE"},
		{"select into", "SELECT * INTO backup FROM customers", "INTO"},
		{"explain analyze update", "EXPLAIN ANALYZE UPDATE customers SET name = 'x'", "UPDATE"},
	}
	for _, tt := range refused {
		t.Run(tt.name, func(t *testing.T) {
			_, err := conn.Exec(context.Background(), tt.sql)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) {
				t.Fatalf("expected a PgError, got %v", err)
			}
			if pgErr.Code != "25006" {
				t.Errorf("SQLSTATE = %s, want 25006 (read_only_sql_transaction)", pgErr.Code)
			}
			if want := "cannot execute " + tt.verb + " in a read-only transaction"; pgErr.Message != want {
				t.Errorf("message = %q, want %q", pgErr.Message, want)
			}
			if !strings.Contains(pgErr.Hint, "read-only mode") {
				t.Errorf("hint should say the proxy is read-only, got %q", pgErr.Hint)
			}
		})
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("%d statements reached Redash; a refused write must never leave the proxy", n)
	}

	// Reads, and the local answers, are untouched.
	rows, err := conn.Query(context.Background(), "SELECT id FROM customers")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	rows.Close()
	if n := calls.Load(); n != 1 {
		t.Fatalf("the select should have reached Redash once, got %d calls", n)
	}
	for _, sql := range []string{"BEGIN", "SET search_path TO public", "COMMIT", "SELECT datname FROM pg_database"} {
		if _, err := conn.Exec(context.Background(), sql); err != nil {
			t.Errorf("%q must still work in read-only mode: %v", sql, err)
		}
	}
}

// A session cannot switch the proxy's read-only mode off, and the proxy says so
// rather than answering SET with a silent OK.
func TestPG_ReadOnlyIsAdvertisedAndCannotBeEscaped(t *testing.T) {
	mock, registry, _ := countingMock(t)
	addr := startPGServer(t, mock, registry, proxy.WithReadOnly(true))
	conn := connectPG(t, addr, "Production PG")

	for _, param := range []string{"transaction_read_only", "default_transaction_read_only"} {
		var got string
		if err := conn.QueryRow(context.Background(), "SHOW "+param).Scan(&got); err != nil {
			t.Fatalf("SHOW %s: %v", param, err)
		}
		if got != "on" {
			t.Errorf("SHOW %s = %q, want on", param, got)
		}
	}

	escapes := []string{
		"SET transaction_read_only = off",
		"SET default_transaction_read_only TO off",
		"SET SESSION CHARACTERISTICS AS TRANSACTION READ WRITE",
		"BEGIN READ WRITE",
		"START TRANSACTION READ WRITE",
	}
	for _, sql := range escapes {
		_, err := conn.Exec(context.Background(), sql)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "25006" {
			t.Errorf("%q should be refused with 25006, got %v", sql, err)
		}
	}
	// Still a usable session afterwards.
	if _, err := conn.Exec(context.Background(), "SET transaction_read_only = on"); err != nil {
		t.Errorf("asking for read-only is allowed: %v", err)
	}
}

func TestPG_WritableReportsReadOnlyOff(t *testing.T) {
	mock, registry, _ := countingMock(t)
	addr := startPGServer(t, mock, registry)
	conn := connectPG(t, addr, "Production PG")

	if got := conn.PgConn().ParameterStatus("default_transaction_read_only"); got != "off" {
		t.Errorf("default_transaction_read_only parameter = %q, want off", got)
	}
	var got string
	if err := conn.QueryRow(context.Background(), "SHOW transaction_read_only").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "off" {
		t.Errorf("SHOW transaction_read_only = %q, want off", got)
	}
	if _, err := conn.Exec(context.Background(), "SET transaction_read_only = off"); err != nil {
		t.Errorf("a writable proxy accepts the SET as before: %v", err)
	}
}

func TestMySQL_ReadOnlyRefusesWritesBeforeRedash(t *testing.T) {
	mock, registry, calls := countingMock(t)
	addr := startMySQLServer(t, mock, registry, mysqlwire.WithReadOnly(true))
	db := connectMySQL(t, addr, "Analytics MySQL")

	refused := []struct {
		name string
		sql  string
	}{
		{"update", "UPDATE customers SET name = 'x' WHERE id = 1"},
		{"delete", "DELETE FROM customers"},
		{"insert", "INSERT INTO customers (id) VALUES (1)"},
		{"replace", "REPLACE INTO customers (id) VALUES (1)"},
		{"truncate", "TRUNCATE TABLE customers"},
		{"select into outfile", "SELECT * FROM customers INTO OUTFILE '/tmp/x'"},
		{"explain delete", "EXPLAIN DELETE FROM customers"},
	}
	for _, tt := range refused {
		t.Run(tt.name, func(t *testing.T) {
			_, err := db.Exec(tt.sql)
			var myErr *mysqldriver.MySQLError
			if !errors.As(err, &myErr) {
				t.Fatalf("expected a MySQLError, got %v", err)
			}
			if myErr.Number != 1290 {
				t.Errorf("error number = %d, want 1290 (ER_OPTION_PREVENTS_STATEMENT)", myErr.Number)
			}
			if !strings.Contains(myErr.Message, "--read-only") || !strings.Contains(myErr.Message, "read-only mode") {
				t.Errorf("message should read like MySQL's and name the proxy, got %q", myErr.Message)
			}
		})
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("%d statements reached Redash; a refused write must never leave the proxy", n)
	}

	rows, err := db.Query("SELECT id FROM customers")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	rows.Close()
	if n := calls.Load(); n != 1 {
		t.Fatalf("the select should have reached Redash once, got %d calls", n)
	}
	for _, sql := range []string{"SET NAMES utf8mb4", "BEGIN", "COMMIT", "SHOW DATABASES", "USE `Analytics MySQL`"} {
		if _, err := db.Exec(sql); err != nil {
			t.Errorf("%q must still work in read-only mode: %v", sql, err)
		}
	}
}

func TestMySQL_ReadOnlyIsAdvertised(t *testing.T) {
	mock, registry, _ := countingMock(t)

	check := func(t *testing.T, addr string, want int) {
		t.Helper()
		db := connectMySQL(t, addr, "")
		var ro, superRO, txRO, legacy int
		row := db.QueryRow("SELECT @@read_only, @@global.super_read_only, @@session.transaction_read_only, @@tx_read_only")
		if err := row.Scan(&ro, &superRO, &txRO, &legacy); err != nil {
			t.Fatal(err)
		}
		for name, got := range map[string]int{"read_only": ro, "super_read_only": superRO, "transaction_read_only": txRO, "tx_read_only": legacy} {
			if got != want {
				t.Errorf("@@%s = %d, want %d", name, got, want)
			}
		}

		rows, err := db.Query("SHOW VARIABLES")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		found := map[string]string{}
		for rows.Next() {
			var name string
			var value sql.NullString
			if err := rows.Scan(&name, &value); err != nil {
				t.Fatal(err)
			}
			found[name] = value.String
		}
		if got := found["read_only"]; got != strconv.Itoa(want) {
			t.Errorf("SHOW VARIABLES read_only = %q, want %d", got, want)
		}
	}

	t.Run("read-only", func(t *testing.T) {
		check(t, startMySQLServer(t, mock, registry, mysqlwire.WithReadOnly(true)), 1)
	})
	t.Run("writable", func(t *testing.T) {
		check(t, startMySQLServer(t, mock, registry), 0)
	})
}
