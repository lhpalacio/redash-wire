package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/lhpalacio/redash-wire/internal/mysqlwire"
	"github.com/lhpalacio/redash-wire/internal/proxy"
	"github.com/lhpalacio/redash-wire/internal/redash"
	"github.com/lhpalacio/redash-wire/internal/testutil"
)

const (
	testUser = "testuser"
	testPass = "testpass"
)

var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

func defaultMockAndRegistry() (*testutil.MockRedashAPI, *testutil.MockSourceRegistry) {
	sources := testutil.SampleDataSources()
	mock := &testutil.MockRedashAPI{
		GetSchemaFunc: func(ctx context.Context, dataSourceID int) ([]redash.SchemaTable, error) {
			return testutil.SampleSchema(), nil
		},
	}
	registry := testutil.NewMockSourceRegistry(sources)
	return mock, registry
}

func startPGServer(t *testing.T, mock *testutil.MockRedashAPI, registry *testutil.MockSourceRegistry) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := proxy.NewServer(ln.Addr().String(), discardLogger, mock, registry, testUser, testPass)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.Serve(ctx, ln)
	}()

	t.Cleanup(func() {
		cancel()
		<-done
	})

	return ln.Addr().String()
}

func startMySQLServer(t *testing.T, mock *testutil.MockRedashAPI, registry *testutil.MockSourceRegistry) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := mysqlwire.NewServer(ln.Addr().String(), discardLogger, mock, registry, testUser, testPass)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.Serve(ctx, ln)
	}()

	t.Cleanup(func() {
		cancel()
		<-done
	})

	return ln.Addr().String()
}

func connectPG(t *testing.T, addr, dbName string) *pgx.Conn {
	t.Helper()

	host, port, _ := net.SplitHostPort(addr)
	connStr := fmt.Sprintf("host=%s port=%s user=%s password=%s sslmode=disable", host, port, testUser, testPass)

	cfg, err := pgx.ParseConfig(connStr)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.Database = dbName
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol

	conn, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	t.Cleanup(func() {
		conn.Close(context.Background())
	})

	return conn
}

func connectMySQL(t *testing.T, addr, dbName string) *sql.DB {
	t.Helper()

	dsn := fmt.Sprintf("%s:%s@tcp(%s)/%s?allowNativePasswords=true", testUser, testPass, addr, dbName)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	if err := db.Ping(); err != nil {
		db.Close()
		t.Fatalf("ping: %v", err)
	}

	t.Cleanup(func() {
		db.Close()
	})

	return db
}
