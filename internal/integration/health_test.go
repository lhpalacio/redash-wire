package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lhpalacio/redash-wire/internal/health"
	"github.com/lhpalacio/redash-wire/internal/mysqlwire"
	"github.com/lhpalacio/redash-wire/internal/proxy"
)

const pgDatabase = "Production PG"
const mysqlDatabase = "Analytics MySQL"

func startGatedPGServer(t *testing.T, gate *health.Gate) string {
	t.Helper()
	mock, registry := defaultMockAndRegistry()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := proxy.NewServer(ln.Addr().String(), discardLogger, mock, registry, testUser, testPass, proxy.WithGate(gate))

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

func startGatedMySQLServer(t *testing.T, gate *health.Gate) string {
	t.Helper()
	mock, registry := defaultMockAndRegistry()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := mysqlwire.NewServer(ln.Addr().String(), discardLogger, mock, registry, testUser, testPass, mysqlwire.WithGate(gate))

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

// dialPG connects without failing the test, so a refusal can be inspected rather
// than aborting the run.
func dialPG(addr, dbName string) (*pgx.Conn, error) {
	host, port, _ := net.SplitHostPort(addr)
	cfg, err := pgx.ParseConfig(fmt.Sprintf("host=%s port=%s user=%s password=%s sslmode=disable",
		host, port, testUser, testPass))
	if err != nil {
		return nil, err
	}
	cfg.Database = dbName
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return pgx.ConnectConfig(ctx, cfg)
}

func TestPGRefusesConnectionsWhileRedashIsUnreachable(t *testing.T) {
	gate := health.NewGate()
	gate.Fail(health.KindUnreachable, "dial tcp: i/o timeout")
	addr := startGatedPGServer(t, gate)

	conn, err := dialPG(addr, pgDatabase)
	if err == nil {
		conn.Close(context.Background())
		t.Fatal("connected while Redash was unreachable, want a refusal")
	}
	// The whole point of refusing after the handshake rather than at accept: the
	// person is in a terminal, and this is where they find out it was the VPN.
	if !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("connect error = %q, want it to name Redash as the cause", err)
	}
}

func TestPGRefusalPointsAtTheKeyWhenRedashRejectedUs(t *testing.T) {
	gate := health.NewGate()
	gate.Fail(health.KindRejected, "data sources request failed (status 401)")
	addr := startGatedPGServer(t, gate)

	conn, err := dialPG(addr, pgDatabase)
	if err == nil {
		conn.Close(context.Background())
		t.Fatal("connected while Redash was rejecting us, want a refusal")
	}
	// A dead key and a dead network need different things from the user, so the
	// two refusals must not read the same.
	if !strings.Contains(err.Error(), "api_key") {
		t.Errorf("connect error = %q, want it to point at the API key", err)
	}
}

func TestPGKeepsALiveSessionWhenRedashGoesAway(t *testing.T) {
	gate := health.NewGate()
	addr := startGatedPGServer(t, gate)

	conn, err := dialPG(addr, pgDatabase)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())
	ctx := context.Background()
	if _, err := conn.Exec(ctx, "SELECT * FROM users"); err != nil {
		t.Fatalf("query before the outage: %v", err)
	}

	gate.Fail(health.KindUnreachable, "the vpn went away")

	// The socket survives. A wrong call by the checker then costs one query,
	// not a reconnect dialog in every client that was open; the query itself
	// names the cause, which is where the person is looking.
	_, err = conn.Exec(ctx, "SELECT * FROM users")
	if err == nil {
		t.Fatal("a query succeeded while Redash was unreachable")
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("query error = %q, want it to name Redash as the cause", err)
	}
	if conn.IsClosed() {
		t.Fatal("the live session was dropped; the client would have to reconnect")
	}

	gate.Recover()
	if _, err := conn.Exec(ctx, "SELECT * FROM users"); err != nil {
		t.Fatalf("query on the same session after recovery: %v", err)
	}
}

func TestPGServesAgainAfterRecoveryWithoutARestart(t *testing.T) {
	gate := health.NewGate()
	gate.Fail(health.KindUnreachable, "down")
	addr := startGatedPGServer(t, gate)

	if conn, err := dialPG(addr, pgDatabase); err == nil {
		conn.Close(context.Background())
		t.Fatal("connected while the gate was down")
	}

	// The listener stayed bound throughout, which is the reason recovery needs no
	// click in the menu bar: the VPN comes back and the next connect just works.
	gate.Recover()

	conn, err := dialPG(addr, pgDatabase)
	if err != nil {
		t.Fatalf("connect after recovery: %v", err)
	}
	defer conn.Close(context.Background())

	if _, err := conn.Exec(context.Background(), "SELECT * FROM users"); err != nil {
		t.Fatalf("query after recovery: %v", err)
	}
}

func TestMySQLRefusesWhileRedashIsUnreachable(t *testing.T) {
	gate := health.NewGate()
	gate.Fail(health.KindUnreachable, "dial tcp: i/o timeout")
	addr := startGatedMySQLServer(t, gate)

	// go-mysql calls UseDB during the handshake when the DSN names a database, so
	// the refusal reaches the client before it ever gets a prompt.
	db, err := sql.Open("mysql", fmt.Sprintf("%s:%s@tcp(%s)/%s?allowNativePasswords=true",
		testUser, testPass, addr, mysqlDatabase))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	err = db.Ping()
	if err == nil {
		t.Fatal("connected while Redash was unreachable, want a refusal")
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("ping error = %q, want it to name Redash as the cause", err)
	}
}

func TestMySQLKeepsALiveSessionWhenRedashGoesAway(t *testing.T) {
	gate := health.NewGate()
	addr := startGatedMySQLServer(t, gate)

	db := connectMySQL(t, addr, mysqlDatabase)
	ctx := context.Background()
	// One pinned connection, so the pool cannot hide a dropped socket behind a
	// fresh dial.
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "SELECT * FROM users"); err != nil {
		t.Fatalf("query before the outage: %v", err)
	}

	gate.Fail(health.KindUnreachable, "the vpn went away")

	// go-mysql only lets the proxy speak in answer to a command, and keeping the
	// socket is what makes that enough: the reason reaches the client on the
	// query, where before the session was cut and the client saw a broken pipe.
	_, err = conn.ExecContext(ctx, "SELECT * FROM users")
	if err == nil {
		t.Fatal("a query succeeded while Redash was unreachable")
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("query error = %q, want it to name Redash as the cause", err)
	}

	gate.Recover()
	if _, err := conn.ExecContext(ctx, "SELECT * FROM users"); err != nil {
		t.Fatalf("query on the same session after recovery: %v", err)
	}
}

func TestMySQLWithNoDatabaseIsToldWhyWhileRedashIsUnreachable(t *testing.T) {
	// `mysql -u ... -p` with no -D never reaches UseDB during the handshake, so
	// nothing refuses the connection. It has to learn the reason from its first
	// command, which means the socket must survive long enough to send one and
	// the gate has to answer before "No database selected" does.
	gate := health.NewGate()
	gate.Fail(health.KindUnreachable, "dial tcp: i/o timeout")
	addr := startGatedMySQLServer(t, gate)

	db, err := sql.Open("mysql", fmt.Sprintf("%s:%s@tcp(%s)/?allowNativePasswords=true",
		testUser, testPass, addr))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	_, err = db.Exec("SELECT * FROM users")
	if err == nil {
		t.Fatal("a query succeeded while Redash was unreachable")
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("query error = %q, want it to name Redash rather than the missing database", err)
	}
}

func TestServersWithoutAGateServeUnconditionally(t *testing.T) {
	// The wire-protocol tests construct servers with no gate at all; that must
	// keep meaning "always serve", not "never serve".
	mock, registry := defaultMockAndRegistry()
	addr := startPGServer(t, mock, registry)

	conn, err := dialPG(addr, pgDatabase)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())

	if _, err := conn.Exec(context.Background(), "SELECT * FROM users"); err != nil {
		t.Fatalf("query: %v", err)
	}
}
