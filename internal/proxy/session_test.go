package proxy_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/lhpalacio/redash-wire/internal/proxy"
	"github.com/lhpalacio/redash-wire/internal/redash"
	"github.com/lhpalacio/redash-wire/internal/testutil"
)

const (
	testUser = "testuser"
	testPass = "testpass"
	testDB   = "Production PG"
)

var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

func startTestPGServer(t *testing.T, mock *testutil.MockRedashAPI, registry *testutil.MockSourceRegistry) string {
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

func connectPG(t *testing.T, addr string) *pgx.Conn {
	t.Helper()
	host, port, _ := net.SplitHostPort(addr)
	cfg, err := pgx.ParseConfig(fmt.Sprintf("host=%s port=%s user=%s password=%s sslmode=disable", host, port, testUser, testPass))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.Database = testDB
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol

	conn, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// blockingMock returns a mock whose ExecuteQuery blocks until its context is
// cancelled, signalling when a query has started and when the context was
// cancelled. It stands in for a long-running Redash job.
func blockingMock() (*testutil.MockRedashAPI, <-chan struct{}, <-chan struct{}) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	var startOnce, cancelOnce sync.Once
	mock := &testutil.MockRedashAPI{
		ExecuteQueryFunc: func(ctx context.Context, sql string, dataSourceID int) (*redash.QueryResult, error) {
			startOnce.Do(func() { close(started) })
			<-ctx.Done()
			cancelOnce.Do(func() { close(cancelled) })
			return nil, ctx.Err()
		},
	}
	return mock, started, cancelled
}

// TestClientDisconnectCancelsInFlightJob covers the case where the client's socket
// dies while a query is blocked in ExecuteQuery. The per-query context must be
// cancelled so the poll loop stops and the Redash job is cancelled, rather than
// running to completion against a dead socket while holding a semaphore slot.
func TestClientDisconnectCancelsInFlightJob(t *testing.T) {
	mock, started, cancelled := blockingMock()
	registry := testutil.NewMockSourceRegistry(testutil.SampleDataSources())
	addr := startTestPGServer(t, mock, registry)

	conn := connectPG(t, addr)
	errc := make(chan error, 1)
	go func() {
		_, err := conn.Exec(context.Background(), "SELECT * FROM big_table")
		errc <- err
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the query never reached ExecuteQuery")
	}

	// Hard-close the underlying socket mid-query, the way a crashed client would.
	if err := conn.PgConn().Conn().Close(); err != nil {
		t.Fatalf("closing client socket: %v", err)
	}

	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("the Redash job was not cancelled within 1s of the client disconnecting")
	}
	<-errc // the Exec goroutine unwinds
}

// TestCancelRequestStopsInFlightQuery covers the PostgreSQL CancelRequest path: a
// throwaway connection naming this session's ProcessID/SecretKey must cancel its
// in-flight query without dropping the session.
func TestCancelRequestStopsInFlightQuery(t *testing.T) {
	mock, started, cancelled := blockingMock()
	registry := testutil.NewMockSourceRegistry(testutil.SampleDataSources())
	addr := startTestPGServer(t, mock, registry)

	conn := connectPG(t, addr)
	errc := make(chan error, 1)
	go func() {
		_, err := conn.Exec(context.Background(), "SELECT * FROM big_table")
		errc <- err
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the query never reached ExecuteQuery")
	}

	// pgx opens a fresh connection and sends CancelRequest with the key it was
	// handed in BackendKeyData at startup.
	if err := conn.PgConn().CancelRequest(context.Background()); err != nil {
		t.Fatalf("CancelRequest: %v", err)
	}

	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("CancelRequest did not cancel the in-flight query within 1s")
	}
	if err := <-errc; err == nil {
		t.Fatal("the cancelled query returned no error to the client")
	}
}

// TestExtendedProtocolEmitsOneErrorAndOneReadyForQuery covers the pipeline desync:
// a Parse/Bind/Describe/Execute/Sync sequence must draw exactly one ErrorResponse
// and exactly one ReadyForQuery, not four of the former and five of the latter.
func TestExtendedProtocolEmitsOneErrorAndOneReadyForQuery(t *testing.T) {
	mock := &testutil.MockRedashAPI{}
	registry := testutil.NewMockSourceRegistry(testutil.SampleDataSources())
	addr := startTestPGServer(t, mock, registry)

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	fe := pgproto3.NewFrontend(c, c)

	fe.Send(&pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersion30,
		Parameters:      map[string]string{"user": testUser, "database": testDB},
	})
	if err := fe.Flush(); err != nil {
		t.Fatalf("sending startup: %v", err)
	}

	// Complete auth and drain the startup bundle up to the first ReadyForQuery.
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("startup receive: %v", err)
		}
		if _, ok := msg.(*pgproto3.AuthenticationCleartextPassword); ok {
			fe.Send(&pgproto3.PasswordMessage{Password: testPass})
			if err := fe.Flush(); err != nil {
				t.Fatalf("sending password: %v", err)
			}
			continue
		}
		if _, ok := msg.(*pgproto3.ReadyForQuery); ok {
			break
		}
	}

	// A full extended-protocol pipeline for an unsupported feature.
	fe.SendParse(&pgproto3.Parse{Query: "SELECT 1"})
	fe.SendBind(&pgproto3.Bind{})
	fe.SendDescribe(&pgproto3.Describe{ObjectType: 'P'})
	fe.SendExecute(&pgproto3.Execute{})
	fe.SendSync(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatalf("sending pipeline: %v", err)
	}

	var errors, readys int
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("pipeline receive: %v", err)
		}
		switch msg.(type) {
		case *pgproto3.ErrorResponse:
			errors++
		case *pgproto3.ReadyForQuery:
			readys++
		}
		if _, ok := msg.(*pgproto3.ReadyForQuery); ok {
			break // the pipeline is answered by exactly one ReadyForQuery
		}
	}

	if errors != 1 {
		t.Errorf("got %d ErrorResponse messages, want exactly 1", errors)
	}
	if readys != 1 {
		t.Errorf("got %d ReadyForQuery messages, want exactly 1", readys)
	}
}
