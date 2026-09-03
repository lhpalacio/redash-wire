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

// defaultMockAndRegistry is a Redash that serves the sample schema and fails
// the test if any statement reaches it. A test that means a statement to run
// on Redash sets ExecuteQueryFunc; every other test thereby also proves the
// proxy answered its statements itself.
func defaultMockAndRegistry(t *testing.T) (*testutil.MockRedashAPI, *testutil.MockSourceRegistry) {
	t.Helper()
	sources := testutil.SampleDataSources()
	mock := &testutil.MockRedashAPI{
		GetSchemaFunc: func(ctx context.Context, dataSourceID int) ([]redash.SchemaTable, error) {
			return testutil.SampleSchema(), nil
		},
		ExecuteQueryFunc: func(ctx context.Context, sql string, dataSourceID int) (*redash.QueryResult, error) {
			t.Errorf("unexpected query to Redash: %s", sql)
			return &redash.QueryResult{}, nil
		},
	}
	registry := testutil.NewMockSourceRegistry(sources)
	return mock, registry
}

// redashRunsAnything stands in for a Redash that runs any statement and
// returns no rows, for tests about what happens around a query rather than
// about its result.
func redashRunsAnything(context.Context, string, int) (*redash.QueryResult, error) {
	return &redash.QueryResult{}, nil
}

func startPGServer(t *testing.T, mock *testutil.MockRedashAPI, registry *testutil.MockSourceRegistry, opts ...proxy.ServerOption) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := proxy.NewServer(ln.Addr().String(), discardLogger, mock, registry, testUser, testPass, opts...)

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

func startMySQLServer(t *testing.T, mock *testutil.MockRedashAPI, registry *testutil.MockSourceRegistry, opts ...mysqlwire.ServerOption) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := mysqlwire.NewServer(ln.Addr().String(), discardLogger, mock, registry, testUser, testPass, opts...)

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
