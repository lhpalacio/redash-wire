package mysqlwire

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	drivermysql "github.com/go-sql-driver/mysql"
	"github.com/lhpalacio/redash-wire/internal/health"
	"github.com/lhpalacio/redash-wire/internal/redash"
	"github.com/lhpalacio/redash-wire/internal/testutil"
)

const (
	wireUser = "testuser"
	wirePass = "testpass"
)

func wireSources() []redash.DataSource {
	return []redash.DataSource{
		{ID: 1, Name: "prod_pg", Type: "pg"},
		{ID: 2, Name: "prod_mysql", Type: "mysql"},
	}
}

// startWireServer serves a Server on a loopback port for the rest of the test.
func startWireServer(t *testing.T, mock *testutil.MockRedashAPI, gate *health.Gate) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := NewServer(ln.Addr().String(), testLogger, mock, testutil.NewMockSourceRegistry(wireSources()), wireUser, wirePass, WithGate(gate))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ctx, ln)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return ln.Addr().String()
}

func openWire(t *testing.T, addr, user, pass, db string) *sql.DB {
	t.Helper()
	dsn := fmt.Sprintf("%s:%s@tcp(%s)/%s?allowNativePasswords=true", user, pass, addr, db)
	dbh, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { dbh.Close() })
	return dbh
}

func mysqlErrNumber(t *testing.T, err error) uint16 {
	t.Helper()
	var me *drivermysql.MySQLError
	if !errors.As(err, &me) {
		t.Fatalf("err = %v (%T), want a MySQL server error", err, err)
	}
	return me.Number
}

func TestLoginErrors(t *testing.T) {
	addr := startWireServer(t, &testutil.MockRedashAPI{}, health.NewGate())

	tests := []struct {
		name, user, pass, db string
		code                 uint16
		msg                  string
	}{
		{"wrong password", wireUser, "nope", "", 1045, "Access denied"},
		{"unknown user", "nobody", wirePass, "", 1045, "Access denied"},
		{"unknown user naming a database", "nobody", wirePass, "prod_mysql", 1045, "Access denied"},
		{"wrong password naming an unknown database", wireUser, "nope", "nonexistent", 1045, "Access denied"},
		{"unknown database once authenticated", wireUser, wirePass, "nonexistent", 1049, "Unknown database 'nonexistent'"},
		{"non-MySQL data source once authenticated", wireUser, wirePass, "prod_pg", 1049, "not a MySQL-compatible"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := openWire(t, addr, tt.user, tt.pass, tt.db).Ping()
			if err == nil {
				t.Fatal("connected, want a refusal")
			}
			if code := mysqlErrNumber(t, err); code != tt.code {
				t.Errorf("error code = %d, want %d (%v)", code, tt.code, err)
			}
			if !strings.Contains(err.Error(), tt.msg) {
				t.Errorf("error = %q, want it to contain %q", err, tt.msg)
			}
		})
	}

	t.Run("database named in the handshake is selected", func(t *testing.T) {
		db := openWire(t, addr, wireUser, wirePass, "prod_mysql")
		var name string
		if err := db.QueryRow("SELECT DATABASE()").Scan(&name); err != nil {
			t.Fatalf("SELECT DATABASE(): %v", err)
		}
		if name != "prod_mysql" {
			t.Errorf("DATABASE() = %q, want prod_mysql", name)
		}
	})
}

func TestLoginWhileRedashIsDown(t *testing.T) {
	gate := health.NewGate()
	gate.Fail(health.KindUnreachable, "dial tcp: i/o timeout")
	addr := startWireServer(t, &testutil.MockRedashAPI{}, gate)

	t.Run("a wrong password learns nothing about the outage", func(t *testing.T) {
		err := openWire(t, addr, wireUser, "nope", "prod_mysql").Ping()
		if code := mysqlErrNumber(t, err); code != 1045 {
			t.Errorf("error code = %d, want 1045 (%v)", code, err)
		}
		if strings.Contains(err.Error(), "unreachable") {
			t.Errorf("error = %q discloses the outage before authentication", err)
		}
	})

	for _, db := range []string{"", "prod_mysql"} {
		t.Run(fmt.Sprintf("valid login with database %q is refused with the reason", db), func(t *testing.T) {
			err := openWire(t, addr, wireUser, wirePass, db).Ping()
			if err == nil {
				t.Fatal("connected while Redash was unreachable, want a refusal")
			}
			if !strings.Contains(err.Error(), "unreachable") {
				t.Errorf("error = %q, want it to name Redash as the cause", err)
			}
		})
	}
}

func TestKillQueryOverTheWire(t *testing.T) {
	started := make(chan struct{})
	var once sync.Once
	mock := &testutil.MockRedashAPI{
		ExecuteQueryFunc: func(ctx context.Context, _ string, _ int) (*redash.QueryResult, error) {
			once.Do(func() { close(started) })
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	addr := startWireServer(t, mock, health.NewGate())
	db := openWire(t, addr, wireUser, wirePass, "prod_mysql")
	ctx := context.Background()

	victim, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer victim.Close()
	var id uint32
	if err := victim.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&id); err != nil {
		t.Fatalf("SELECT CONNECTION_ID(): %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := victim.ExecContext(ctx, "SELECT * FROM users")
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the query never reached Redash")
	}

	// What the mysql CLI does on Ctrl-C: a fresh connection sends KILL QUERY.
	killer, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer killer.Close()
	if _, err := killer.ExecContext(ctx, fmt.Sprintf("KILL QUERY %d", id)); err != nil {
		t.Fatalf("KILL QUERY: %v", err)
	}

	select {
	case err := <-done:
		if code := mysqlErrNumber(t, err); code != 1317 {
			t.Errorf("interrupted query code = %d, want 1317 (%v)", code, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the query was not interrupted")
	}

	// The interrupted session is still usable.
	var again uint32
	if err := victim.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&again); err != nil || again != id {
		t.Errorf("victim after KILL QUERY: id %d, err %v; want id %d and no error", again, err, id)
	}
}

// flakyListener fails Accept with a temporary error a number of times, then
// blocks like a real listener until it is closed.
type flakyListener struct {
	failures  int
	calls     int
	recovered chan struct{} // closed when Accept first blocks
	closed    chan struct{}
	once      sync.Once
}

type temporaryErr struct{}

func (temporaryErr) Error() string   { return "accept tcp: too many open files" }
func (temporaryErr) Temporary() bool { return true }
func (temporaryErr) Timeout() bool   { return false }

func newFlakyListener(failures int) *flakyListener {
	return &flakyListener{failures: failures, recovered: make(chan struct{}), closed: make(chan struct{})}
}

func (l *flakyListener) Accept() (net.Conn, error) {
	l.calls++
	if l.calls <= l.failures {
		return nil, temporaryErr{}
	}
	if l.calls == l.failures+1 {
		close(l.recovered)
	}
	<-l.closed
	return nil, net.ErrClosed
}

func (l *flakyListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *flakyListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

func newIdleServer() *Server {
	return NewServer("", testLogger, &testutil.MockRedashAPI{}, testutil.NewMockSourceRegistry(nil), wireUser, wirePass)
}

func TestServeBacksOffOnAcceptErrors(t *testing.T) {
	ln := newFlakyListener(4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- newIdleServer().Serve(ctx, ln) }()

	select {
	case <-ln.recovered:
	case <-time.After(10 * time.Second):
		t.Fatal("Serve never got past the failing Accepts")
	}
	elapsed := time.Since(start)
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Serve after cancel = %v, want nil", err)
	}

	// 5ms, 10ms, 20ms, 40ms: four failures cost at least 75ms of backoff.
	if elapsed < 75*time.Millisecond {
		t.Errorf("four failing Accepts were retried in %v; want at least 75ms of backoff, not a spin", elapsed)
	}
}

func TestServeReturnsWhenListenerIsClosedExternally(t *testing.T) {
	ln := newFlakyListener(0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- newIdleServer().Serve(ctx, ln) }()

	<-ln.recovered
	ln.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Error("Serve = nil for a listener closed out from under it, want an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve kept looping on a closed listener")
	}
}
