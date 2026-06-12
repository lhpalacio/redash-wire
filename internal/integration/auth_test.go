package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestMySQLRejectsWrongPassword guards against the auth-bypass regression where
// the MySQL listener accepted any password for a known username.
func TestMySQLRejectsWrongPassword(t *testing.T) {
	mock, registry := defaultMockAndRegistry()
	addr := startMySQLServer(t, mock, registry)

	dsn := fmt.Sprintf("%s:%s@tcp(%s)/?allowNativePasswords=true", testUser, "WRONG_PASSWORD", addr)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err == nil {
		t.Fatal("expected authentication to fail with a wrong password, but Ping succeeded")
	}
}

func TestMySQLRejectsWrongUser(t *testing.T) {
	mock, registry := defaultMockAndRegistry()
	addr := startMySQLServer(t, mock, registry)

	dsn := fmt.Sprintf("%s:%s@tcp(%s)/?allowNativePasswords=true", "wronguser", testPass, addr)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err == nil {
		t.Fatal("expected authentication to fail with an unknown user, but Ping succeeded")
	}
}

func TestPGRejectsWrongPassword(t *testing.T) {
	mock, registry := defaultMockAndRegistry()
	addr := startPGServer(t, mock, registry)

	host, port, _ := net.SplitHostPort(addr)
	connStr := fmt.Sprintf("host=%s port=%s user=%s password=%s sslmode=disable", host, port, testUser, "WRONG_PASSWORD")
	cfg, err := pgx.ParseConfig(connStr)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.Database = "Sample PostgreSQL"
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol

	conn, err := pgx.ConnectConfig(context.Background(), cfg)
	if err == nil {
		conn.Close(context.Background())
		t.Fatal("expected authentication to fail with a wrong password, but connect succeeded")
	}
}
