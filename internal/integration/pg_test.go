package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/lhpalacio/redash-wire/internal/redash"
)

func TestPG_ConnectionAndAuth(t *testing.T) {
	mock, registry := defaultMockAndRegistry(t)
	addr := startPGServer(t, mock, registry)

	t.Run("valid credentials", func(t *testing.T) {
		conn := connectPG(t, addr, "Production PG")
		var result string
		err := conn.QueryRow(context.Background(), "SELECT version()").Scan(&result)
		if err != nil {
			t.Fatalf("query failed: %v", err)
		}
		if !strings.Contains(result, "redash-wire") {
			t.Fatalf("expected version to contain 'redash-wire', got %q", result)
		}
	})

	t.Run("wrong password", func(t *testing.T) {
		host, port, _ := net.SplitHostPort(addr)
		connStr := fmt.Sprintf("host=%s port=%s user=%s password=wrong sslmode=disable", host, port, testUser)
		cfg, err := pgx.ParseConfig(connStr)
		if err != nil {
			t.Fatalf("parse config: %v", err)
		}
		cfg.Database = "test"
		cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol

		conn, err := pgx.ConnectConfig(context.Background(), cfg)
		if err == nil {
			conn.Close(context.Background())
			t.Fatal("expected auth error, got nil")
		}
	})

	t.Run("wrong username", func(t *testing.T) {
		host, port, _ := net.SplitHostPort(addr)
		connStr := fmt.Sprintf("host=%s port=%s user=wronguser password=%s sslmode=disable", host, port, testPass)
		cfg, err := pgx.ParseConfig(connStr)
		if err != nil {
			t.Fatalf("parse config: %v", err)
		}
		cfg.Database = "test"
		cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol

		conn, err := pgx.ConnectConfig(context.Background(), cfg)
		if err == nil {
			conn.Close(context.Background())
			t.Fatal("expected auth error, got nil")
		}
	})
}

func TestPG_LocalQueries(t *testing.T) {
	mock, registry := defaultMockAndRegistry(t)
	addr := startPGServer(t, mock, registry)
	conn := connectPG(t, addr, "Production PG")

	tests := []struct {
		name string
		sql  string
	}{
		{"SET", "SET client_encoding = 'UTF8'"},
		{"SHOW server_version", "SHOW server_version"},
		{"BEGIN", "BEGIN"},
		{"COMMIT", "COMMIT"},
		{"ROLLBACK", "ROLLBACK"},
		{"DEALLOCATE", "DEALLOCATE ALL"},
		{"CLOSE", "CLOSE ALL"},
		{"DISCARD", "DISCARD ALL"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := conn.Exec(context.Background(), tt.sql)
			if err != nil {
				t.Fatalf("exec %q failed: %v", tt.sql, err)
			}
		})
	}

	t.Run("SHOW server_version value", func(t *testing.T) {
		var val string
		err := conn.QueryRow(context.Background(), "SHOW server_version").Scan(&val)
		if err != nil {
			t.Fatalf("query failed: %v", err)
		}
		if val != "14.0" {
			t.Fatalf("expected 14.0, got %q", val)
		}
	})
}

func TestPG_ServerInfoFunctions(t *testing.T) {
	mock, registry := defaultMockAndRegistry(t)
	addr := startPGServer(t, mock, registry)
	conn := connectPG(t, addr, "Production PG")

	_, port, _ := net.SplitHostPort(addr)

	tests := []struct {
		name     string
		sql      string
		contains string
	}{
		{"version()", "SELECT version()", "redash-wire"},
		{"current_database()", "SELECT current_database()", "Production PG"},
		{"current_schema()", "SELECT current_schema()", "public"},
		{"current_user", "SELECT current_user", testUser},
		{"session_user", "SELECT session_user", testUser},
		{"inet_server_addr()", "SELECT inet_server_addr()", "127.0.0.1"},
		{"inet_server_port()", "SELECT inet_server_port()", port},
		{"pg_is_in_recovery()", "SELECT pg_is_in_recovery()", "f"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var val string
			err := conn.QueryRow(context.Background(), tt.sql).Scan(&val)
			if err != nil {
				t.Fatalf("query %q failed: %v", tt.sql, err)
			}
			if !strings.Contains(val, tt.contains) {
				t.Fatalf("expected %q to contain %q, got %q", tt.sql, tt.contains, val)
			}
		})
	}

	t.Run("pg_backend_pid()", func(t *testing.T) {
		// Every column the proxy sends is text, so the pid is parsed here.
		pid := func(c *pgx.Conn) int64 {
			t.Helper()
			var text string
			if err := c.QueryRow(context.Background(), "SELECT pg_backend_pid()").Scan(&text); err != nil {
				t.Fatalf("query failed: %v", err)
			}
			id, err := strconv.ParseInt(text, 10, 64)
			if err != nil {
				t.Fatalf("pg_backend_pid() = %q, want an integer", text)
			}
			return id
		}
		id := pid(conn)
		if id <= 0 {
			t.Fatalf("pg_backend_pid() = %d, want a positive id", id)
		}
		if again := pid(conn); again != id {
			t.Errorf("pg_backend_pid() changed from %d to %d on the same connection", id, again)
		}
		// The proxy answers every session with the same pid rather than the
		// ProcessID it sent in BackendKeyData, so two connections are not yet
		// told apart here.
	})
}

func TestPG_PgDatabaseListing(t *testing.T) {
	mock, registry := defaultMockAndRegistry(t)
	addr := startPGServer(t, mock, registry)
	conn := connectPG(t, addr, "Production PG")

	rows, err := conn.Query(context.Background(), "SELECT datname, oid, datistemplate, datallowconn FROM pg_database")
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name, oid, isTemplate, allowConn string
		if err := rows.Scan(&name, &oid, &isTemplate, &allowConn); err != nil {
			t.Fatalf("scan: %v", err)
		}
		names = append(names, name)
	}

	// Only PG-compatible sources should be listed; the MySQL source is filtered out.
	found := map[string]bool{}
	for _, n := range names {
		found[n] = true
	}
	if !found["Production PG"] {
		t.Error("expected 'Production PG' in pg_database listing")
	}
	if !found["Redshift DW"] {
		t.Error("expected 'Redshift DW' in pg_database listing")
	}
	if found["Analytics MySQL"] {
		t.Error("did not expect 'Analytics MySQL' in pg_database listing")
	}
}

func TestPG_CatalogQueries(t *testing.T) {
	mock, registry := defaultMockAndRegistry(t)
	addr := startPGServer(t, mock, registry)
	conn := connectPG(t, addr, "Production PG")

	t.Run("pg_namespace", func(t *testing.T) {
		var nspname string
		err := conn.QueryRow(context.Background(), "SELECT nspname FROM pg_namespace").Scan(&nspname)
		if err != nil {
			t.Fatalf("query failed: %v", err)
		}
		if nspname != "public" {
			t.Fatalf("expected 'public', got %q", nspname)
		}
	})

	t.Run("pg_type", func(t *testing.T) {
		rows, err := conn.Query(context.Background(), "SELECT oid, typname FROM pg_type")
		if err != nil {
			t.Fatalf("query failed: %v", err)
		}
		defer rows.Close()
		var cols []string
		for _, f := range rows.FieldDescriptions() {
			cols = append(cols, f.Name)
		}
		if strings.Join(cols, ",") != "oid,typname" {
			t.Errorf("columns = %v, want [oid typname]", cols)
		}
		if rows.Next() {
			t.Error("pg_type returned a row; the proxy knows no types")
		}
	})

	t.Run("pg_class", func(t *testing.T) {
		rows, err := conn.Query(context.Background(), "SELECT oid, table_name, table_schema FROM pg_class")
		if err != nil {
			t.Fatalf("query failed: %v", err)
		}
		defer rows.Close()

		var tables []string
		for rows.Next() {
			var oid, name, schema string
			if err := rows.Scan(&oid, &name, &schema); err != nil {
				t.Fatalf("scan: %v", err)
			}
			tables = append(tables, name)
		}
		found := map[string]bool{}
		for _, n := range tables {
			found[n] = true
		}
		if !found["users"] || !found["orders"] {
			t.Fatalf("expected 'users' and 'orders' in pg_class, got %v", tables)
		}
	})

	t.Run("pg_statio_user_tables", func(t *testing.T) {
		rows, err := conn.Query(context.Background(), "SELECT name, comment, total_size, data_size, index_size FROM pg_statio_user_tables")
		if err != nil {
			t.Fatalf("query failed: %v", err)
		}
		defer rows.Close()

		var tables []string
		for rows.Next() {
			var name string
			var comment, totalSize, dataSize, indexSize *string
			if err := rows.Scan(&name, &comment, &totalSize, &dataSize, &indexSize); err != nil {
				t.Fatalf("scan: %v", err)
			}
			tables = append(tables, name)
		}
		if len(tables) == 0 {
			t.Fatal("expected at least one table in pg_statio_user_tables")
		}
	})
}

func TestPG_RemoteQueryExecution(t *testing.T) {
	mock, registry := defaultMockAndRegistry(t)

	var capturedSQL string
	var capturedDSID int
	mock.ExecuteQueryFunc = func(ctx context.Context, sql string, dataSourceID int) (*redash.QueryResult, error) {
		capturedSQL = sql
		capturedDSID = dataSourceID
		return &redash.QueryResult{
			Columns: []redash.Column{
				{Name: "id", Type: "integer"},
				{Name: "name", Type: "string"},
				{Name: "score", Type: "float"},
				{Name: "active", Type: "boolean"},
				{Name: "created_at", Type: "datetime"},
				{Name: "metadata", Type: "string"},
			},
			Rows: []map[string]any{
				{
					"id":         json.Number("1"),
					"name":       "Alice",
					"score":      json.Number("95.5"),
					"active":     true,
					"created_at": "2024-01-15T10:30:00Z",
					"metadata":   map[string]any{"role": "admin"},
				},
				{
					"id":         json.Number("2"),
					"name":       "Bob",
					"score":      json.Number("87.3"),
					"active":     false,
					"created_at": "2024-02-20T14:00:00Z",
					"metadata":   map[string]any{"role": "user"},
				},
			},
		}, nil
	}

	addr := startPGServer(t, mock, registry)
	conn := connectPG(t, addr, "Production PG")

	rows, err := conn.Query(context.Background(), "SELECT * FROM some_table")
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	defer rows.Close()

	if capturedSQL != "SELECT * FROM some_table" {
		t.Fatalf("expected SQL 'SELECT * FROM some_table', got %q", capturedSQL)
	}
	if capturedDSID != 1 {
		t.Fatalf("expected data source ID 1, got %d", capturedDSID)
	}

	type row struct {
		ID        string
		Name      string
		Score     string
		Active    string
		CreatedAt string
		Metadata  string
	}

	var results []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.ID, &r.Name, &r.Score, &r.Active, &r.CreatedAt, &r.Metadata); err != nil {
			t.Fatalf("scan: %v", err)
		}
		results = append(results, r)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(results))
	}

	if results[0].ID != "1" {
		t.Errorf("row 0 id: expected '1', got %q", results[0].ID)
	}
	if results[0].Name != "Alice" {
		t.Errorf("row 0 name: expected 'Alice', got %q", results[0].Name)
	}
	if results[0].Active != "t" {
		t.Errorf("row 0 active: expected 't', got %q", results[0].Active)
	}
	if !strings.Contains(results[0].CreatedAt, "2024-01-15") {
		t.Errorf("row 0 created_at: expected to contain '2024-01-15', got %q", results[0].CreatedAt)
	}
	if !strings.Contains(results[0].Metadata, "admin") {
		t.Errorf("row 0 metadata: expected to contain 'admin', got %q", results[0].Metadata)
	}

	if results[1].Active != "f" {
		t.Errorf("row 1 active: expected 'f', got %q", results[1].Active)
	}
}

func TestPG_DMLCommandTags(t *testing.T) {
	mock, registry := defaultMockAndRegistry(t)
	mock.ExecuteQueryFunc = func(ctx context.Context, sql string, dataSourceID int) (*redash.QueryResult, error) {
		return &redash.QueryResult{
			Columns: []redash.Column{{Name: "id", Type: "integer"}},
			Rows: []map[string]any{
				{"id": json.Number("1")},
				{"id": json.Number("2")},
				{"id": json.Number("3")},
			},
		}, nil
	}

	addr := startPGServer(t, mock, registry)
	conn := connectPG(t, addr, "Production PG")

	tests := []struct {
		name        string
		sql         string
		expectedTag string
	}{
		{"INSERT", "INSERT INTO t (id) VALUES (1)", "INSERT 0 3"},
		{"UPDATE", "UPDATE t SET x = 1", "UPDATE 3"},
		{"DELETE", "DELETE FROM t WHERE x = 1", "DELETE 3"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tag, err := conn.Exec(context.Background(), tt.sql)
			if err != nil {
				t.Fatalf("exec %q failed: %v", tt.sql, err)
			}
			if tag.String() != tt.expectedTag {
				t.Fatalf("expected tag %q, got %q", tt.expectedTag, tag.String())
			}
		})
	}
}

func TestPG_DMLNoData(t *testing.T) {
	mock, registry := defaultMockAndRegistry(t)
	// Simulate Redash's "query completed but returned no data": the query
	// executed successfully but Redash does not return affected row counts.
	mock.ExecuteQueryFunc = func(ctx context.Context, sql string, dataSourceID int) (*redash.QueryResult, error) {
		return &redash.QueryResult{}, nil
	}

	addr := startPGServer(t, mock, registry)
	conn := connectPG(t, addr, "Production PG")

	tag, err := conn.Exec(context.Background(), "UPDATE customers SET name = 'test' WHERE id = 1")
	if err != nil {
		t.Fatalf("exec failed: %v", err)
	}
	if tag.String() != "UPDATE 0" {
		t.Errorf("command tag = %q, want UPDATE 0: Redash reports no count", tag.String())
	}
}

func TestPG_NoDataSourceSelected(t *testing.T) {
	mock, registry := defaultMockAndRegistry(t)
	addr := startPGServer(t, mock, registry)

	conn := connectPG(t, addr, "nonexistent")

	_, err := conn.Exec(context.Background(), "SELECT * FROM users")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no data source selected") {
		t.Fatalf("expected 'no data source selected' error, got: %v", err)
	}
}

func TestPG_QueryExecutionError(t *testing.T) {
	mock, registry := defaultMockAndRegistry(t)
	// A real SQL error surfaces as a *redash.QueryError (a failed Redash job), which
	// is safe to forward verbatim to the client.
	mock.ExecuteQueryFunc = func(ctx context.Context, sql string, dataSourceID int) (*redash.QueryResult, error) {
		return nil, &redash.QueryError{Message: `relation "bad_table" does not exist`}
	}

	addr := startPGServer(t, mock, registry)
	conn := connectPG(t, addr, "Production PG")

	_, err := conn.Exec(context.Background(), "SELECT * FROM bad_table")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "bad_table") {
		t.Fatalf("expected error about bad_table, got: %v", err)
	}
}

func TestPG_InfraErrorIsMasked(t *testing.T) {
	mock, registry := defaultMockAndRegistry(t)
	// A non-QueryError (e.g. an internal Redash/network failure) must NOT be leaked
	// verbatim to the client, since it can contain internal hostnames/credentials.
	mock.ExecuteQueryFunc = func(ctx context.Context, sql string, dataSourceID int) (*redash.QueryResult, error) {
		return nil, fmt.Errorf(`could not translate host name "warehouse.internal.example" to address`)
	}

	addr := startPGServer(t, mock, registry)
	conn := connectPG(t, addr, "Production PG")

	_, err := conn.Exec(context.Background(), "SELECT 1 FROM t")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if strings.Contains(err.Error(), "warehouse.internal.example") {
		t.Fatalf("internal hostname leaked to client: %v", err)
	}
}
