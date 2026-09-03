package integration_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/lhpalacio/redash-wire/internal/redash"
)

// TestMySQL_Connection: valid credentials get a session. The refusals live in
// auth_test.go.
func TestMySQL_Connection(t *testing.T) {
	mock, registry := defaultMockAndRegistry(t)
	addr := startMySQLServer(t, mock, registry)

	db := connectMySQL(t, addr, "")
	if err := db.Ping(); err != nil {
		t.Fatalf("ping failed: %v", err)
	}
}

func TestMySQL_UseDatabase(t *testing.T) {
	mock, registry := defaultMockAndRegistry(t)
	addr := startMySQLServer(t, mock, registry)
	db := connectMySQL(t, addr, "")

	t.Run("valid MySQL source", func(t *testing.T) {
		_, err := db.Exec("USE `Analytics MySQL`")
		if err != nil {
			t.Fatalf("USE failed: %v", err)
		}
	})

	t.Run("unknown database", func(t *testing.T) {
		_, err := db.Exec("USE nonexistent")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "Unknown database") {
			t.Fatalf("expected 'Unknown database' error, got: %v", err)
		}
	})

	t.Run("non-MySQL source", func(t *testing.T) {
		_, err := db.Exec("USE `Production PG`")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "not a MySQL-compatible") {
			t.Fatalf("expected non-MySQL-compatible error, got: %v", err)
		}
	})
}

func TestMySQL_ShowDatabases(t *testing.T) {
	mock, registry := defaultMockAndRegistry(t)
	addr := startMySQLServer(t, mock, registry)
	db := connectMySQL(t, addr, "")

	rows, err := db.Query("SHOW DATABASES")
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		names = append(names, name)
	}

	found := map[string]bool{}
	for _, n := range names {
		found[n] = true
	}
	if !found["Analytics MySQL"] {
		t.Error("expected 'Analytics MySQL' in SHOW DATABASES")
	}
	if found["Production PG"] {
		t.Error("did not expect 'Production PG' in SHOW DATABASES")
	}
}

func TestMySQL_ShowTables(t *testing.T) {
	mock, registry := defaultMockAndRegistry(t)
	addr := startMySQLServer(t, mock, registry)
	db := connectMySQL(t, addr, "")

	_, err := db.Exec("USE `Analytics MySQL`")
	if err != nil {
		t.Fatalf("USE failed: %v", err)
	}

	t.Run("SHOW TABLES", func(t *testing.T) {
		rows, err := db.Query("SHOW TABLES")
		if err != nil {
			t.Fatalf("query failed: %v", err)
		}
		defer rows.Close()

		var tables []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatalf("scan: %v", err)
			}
			tables = append(tables, name)
		}

		found := map[string]bool{}
		for _, n := range tables {
			found[n] = true
		}
		if !found["users"] || !found["orders"] {
			t.Fatalf("expected 'users' and 'orders', got %v", tables)
		}
	})

	t.Run("SHOW FULL TABLES", func(t *testing.T) {
		rows, err := db.Query("SHOW FULL TABLES")
		if err != nil {
			t.Fatalf("query failed: %v", err)
		}
		defer rows.Close()

		types := map[string]string{}
		for rows.Next() {
			var name, tableType string
			if err := rows.Scan(&name, &tableType); err != nil {
				t.Fatalf("scan: %v", err)
			}
			types[name] = tableType
		}
		for _, name := range []string{"users", "orders"} {
			if types[name] != "BASE TABLE" {
				t.Errorf("table %q: type = %q, want BASE TABLE (tables listed: %v)", name, types[name], types)
			}
		}
	})
}

func TestMySQL_ShowVariables(t *testing.T) {
	mock, registry := defaultMockAndRegistry(t)
	addr := startMySQLServer(t, mock, registry)
	db := connectMySQL(t, addr, "")

	rows, err := db.Query("SHOW VARIABLES")
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	defer rows.Close()

	vars := map[string]string{}
	for rows.Next() {
		var name string
		var value sql.NullString
		if err := rows.Scan(&name, &value); err != nil {
			t.Fatalf("scan: %v", err)
		}
		vars[name] = value.String
	}

	if v, ok := vars["version"]; !ok || v != "8.0.0-redash-wire" {
		t.Errorf("expected version '8.0.0-redash-wire', got %q", v)
	}
	if v, ok := vars["character_set_client"]; !ok || v != "utf8mb4" {
		t.Errorf("expected character_set_client 'utf8mb4', got %q", v)
	}
}

func TestMySQL_LocalSelects(t *testing.T) {
	mock, registry := defaultMockAndRegistry(t)
	addr := startMySQLServer(t, mock, registry)
	db := connectMySQL(t, addr, "")

	tests := []struct {
		name     string
		sql      string
		expected string
	}{
		{"@@version", "SELECT @@version", "8.0.0-redash-wire"},
		{"@@version_comment", "SELECT @@version_comment", "redash-wire MySQL proxy"},
		{"version()", "SELECT version()", "8.0.0-redash-wire"},
		{"user()", "SELECT user()", "redash@localhost"},
		{"@@sql_mode", "SELECT @@sql_mode", "ONLY_FULL_GROUP_BY"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var val string
			err := db.QueryRow(tt.sql).Scan(&val)
			if err != nil {
				t.Fatalf("query %q failed: %v", tt.sql, err)
			}
			if !strings.Contains(val, tt.expected) {
				t.Fatalf("expected %q to contain %q, got %q", tt.sql, tt.expected, val)
			}
		})
	}

	t.Run("connection_id()", func(t *testing.T) {
		ctx := context.Background()
		connID := func(c *sql.Conn) int64 {
			t.Helper()
			var id int64
			if err := c.QueryRowContext(ctx, "SELECT connection_id()").Scan(&id); err != nil {
				t.Fatalf("query failed: %v", err)
			}
			return id
		}
		first, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer first.Close()
		id := connID(first)
		if id <= 0 {
			t.Fatalf("connection_id() = %d, want a positive id", id)
		}
		if again := connID(first); again != id {
			t.Errorf("connection_id() changed from %d to %d on the same connection", id, again)
		}
		second, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer second.Close()
		if other := connID(second); other == id {
			t.Errorf("two connections share connection_id() %d", id)
		}
	})

	t.Run("database() without USE", func(t *testing.T) {
		var val sql.NullString
		err := db.QueryRow("SELECT database()").Scan(&val)
		if err != nil {
			t.Fatalf("query failed: %v", err)
		}
		if val.Valid && val.String != "" {
			t.Fatalf("database() = %q with no database selected, want NULL", val.String)
		}
	})

	t.Run("database() after USE", func(t *testing.T) {
		_, err := db.Exec("USE `Analytics MySQL`")
		if err != nil {
			t.Fatalf("USE failed: %v", err)
		}
		var val string
		err = db.QueryRow("SELECT database()").Scan(&val)
		if err != nil {
			t.Fatalf("query failed: %v", err)
		}
		if val != "Analytics MySQL" {
			t.Fatalf("expected 'Analytics MySQL', got %q", val)
		}
	})
}

func TestMySQL_RemoteQueryExecution(t *testing.T) {
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
					"id":         json.Number("42"),
					"name":       "Alice",
					"score":      json.Number("95.5"),
					"active":     true,
					"created_at": "2024-01-15T10:30:00Z",
					"metadata":   map[string]any{"role": "admin"},
				},
			},
		}, nil
	}

	addr := startMySQLServer(t, mock, registry)
	db := connectMySQL(t, addr, "")

	_, err := db.Exec("USE `Analytics MySQL`")
	if err != nil {
		t.Fatalf("USE failed: %v", err)
	}

	rows, err := db.Query("SELECT * FROM some_table")
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	defer rows.Close()

	if capturedSQL != "SELECT * FROM some_table" {
		t.Fatalf("expected SQL 'SELECT * FROM some_table', got %q", capturedSQL)
	}
	if capturedDSID != 2 {
		t.Fatalf("expected data source ID 2, got %d", capturedDSID)
	}

	if !rows.Next() {
		t.Fatal("expected 1 row")
	}

	var id int64
	var name, score, active, createdAt, metadata string
	if err := rows.Scan(&id, &name, &score, &active, &createdAt, &metadata); err != nil {
		t.Fatalf("scan: %v", err)
	}

	if id != 42 {
		t.Errorf("id: expected 42, got %d", id)
	}
	if name != "Alice" {
		t.Errorf("name: expected 'Alice', got %q", name)
	}
	if active != "1" {
		t.Errorf("active: expected '1', got %q", active)
	}
	if !strings.Contains(createdAt, "2024-01-15") {
		t.Errorf("created_at: expected to contain '2024-01-15', got %q", createdAt)
	}
	if !strings.Contains(metadata, "admin") {
		t.Errorf("metadata: expected to contain 'admin', got %q", metadata)
	}
}

// TestMySQL_Writes: Redash runs a write but reports no rows for it, so the
// client sees success and, as the README says, no affected-row count.
func TestMySQL_Writes(t *testing.T) {
	mock, registry := defaultMockAndRegistry(t)
	mock.ExecuteQueryFunc = redashRunsAnything

	addr := startMySQLServer(t, mock, registry)
	db := connectMySQL(t, addr, "Analytics MySQL")

	for _, stmt := range []string{
		"INSERT INTO t (id) VALUES (1)",
		"UPDATE customers SET name = 'test' WHERE id = 1",
		"DELETE FROM t WHERE x = 1",
	} {
		t.Run(stmt, func(t *testing.T) {
			result, err := db.Exec(stmt)
			if err != nil {
				t.Fatalf("exec failed: %v", err)
			}
			affected, err := result.RowsAffected()
			if err != nil {
				t.Fatalf("rows affected: %v", err)
			}
			if affected != 0 {
				t.Errorf("RowsAffected = %d, want 0: Redash reports no count", affected)
			}
		})
	}
}

func TestMySQL_NoDatabaseSelected(t *testing.T) {
	mock, registry := defaultMockAndRegistry(t)
	addr := startMySQLServer(t, mock, registry)
	db := connectMySQL(t, addr, "")

	_, err := db.Exec("SELECT * FROM users")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "No database selected") {
		t.Fatalf("expected 'No database selected' error, got: %v", err)
	}
}

func TestMySQL_InformationSchema(t *testing.T) {
	mock, registry := defaultMockAndRegistry(t)
	addr := startMySQLServer(t, mock, registry)
	db := connectMySQL(t, addr, "")

	_, err := db.Exec("USE `Analytics MySQL`")
	if err != nil {
		t.Fatalf("USE failed: %v", err)
	}

	t.Run("information_schema.routines", func(t *testing.T) {
		rows, err := db.Query("SELECT * FROM information_schema.routines")
		if err != nil {
			t.Fatalf("query failed: %v", err)
		}
		defer rows.Close()

		count := 0
		for rows.Next() {
			count++
		}
		if count != 0 {
			t.Fatalf("expected 0 rows for routines, got %d", count)
		}
	})

	t.Run("information_schema.tables", func(t *testing.T) {
		rows, err := db.Query("SELECT table_name, table_type FROM information_schema.tables")
		if err != nil {
			t.Fatalf("query failed: %v", err)
		}
		defer rows.Close()

		var tables []string
		for rows.Next() {
			var name, ttype string
			if err := rows.Scan(&name, &ttype); err != nil {
				t.Fatalf("scan: %v", err)
			}
			tables = append(tables, name)
			if ttype != "BASE TABLE" {
				t.Errorf("expected table type 'BASE TABLE', got %q", ttype)
			}
		}
		if len(tables) == 0 {
			t.Error("expected at least one table in information_schema.tables")
		}
	})

	t.Run("information_schema.collation_character_set_applicability", func(t *testing.T) {
		rows, err := db.Query("SELECT * FROM information_schema.collation_character_set_applicability")
		if err != nil {
			t.Fatalf("query failed: %v", err)
		}
		defer rows.Close()

		// One row per table, carrying the table's name, is what TablePlus
		// reads from this query; the rest of the row is padding.
		var names []string
		for rows.Next() {
			var charset, collation, engine, name string
			var estimatedRow int64
			if err := rows.Scan(&charset, &collation, &engine, &name, &estimatedRow); err != nil {
				t.Fatalf("scan: %v", err)
			}
			names = append(names, name)
		}
		if got := strings.Join(names, ","); got != "users,orders" {
			t.Errorf("tables = %q, want users,orders", got)
		}
	})
}

// TestMySQL_TableStructure: the query TablePlus sends for a table's structure
// view is answered from the Redash schema, through a real client connection.
func TestMySQL_TableStructure(t *testing.T) {
	mock, registry := defaultMockAndRegistry(t)
	addr := startMySQLServer(t, mock, registry)
	db := connectMySQL(t, addr, "Analytics MySQL")

	rows, err := db.Query("SELECT ordinal_position as ordinal_position,column_name as column_name,column_type AS data_type,is_nullable as is_nullable,column_comment AS comment FROM information_schema.columns WHERE table_schema='Analytics MySQL' AND table_name='orders' ORDER BY ordinal_position")
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var ordinal int
		var name, dataType, comment string
		var nullable sql.NullString
		if err := rows.Scan(&ordinal, &name, &dataType, &nullable, &comment); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if nullable.Valid {
			t.Errorf("is_nullable for %s = %q, want NULL (Redash does not report it)", name, nullable.String)
		}
		got = append(got, fmt.Sprintf("%d:%s:%s", ordinal, name, dataType))
	}
	want := "1:id:int,2:user_id:int,3:total:decimal"
	if strings.Join(got, ",") != want {
		t.Errorf("structure = %v, want %s", got, want)
	}
}
