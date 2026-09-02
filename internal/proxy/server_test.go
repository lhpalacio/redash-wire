package proxy_test

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/lhpalacio/redash-wire/internal/proxy"
	"github.com/lhpalacio/redash-wire/internal/testutil"
)

// tempError is a temporary net.Error, the shape fd exhaustion (EMFILE) takes.
type tempError struct{}

func (tempError) Error() string   { return "temporary accept failure" }
func (tempError) Timeout() bool   { return false }
func (tempError) Temporary() bool { return true }

// scriptedListener returns a fixed number of Accept errors and then behaves like
// an idle listener (blocking until Close), so a test can watch the accept loop's
// backoff without needing real fd exhaustion.
type scriptedListener struct {
	mu          sync.Mutex
	failsLeft   int
	acceptTimes []time.Time
	closed      chan struct{}
}

func (l *scriptedListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	l.acceptTimes = append(l.acceptTimes, time.Now())
	if l.failsLeft > 0 {
		l.failsLeft--
		l.mu.Unlock()
		return nil, tempError{}
	}
	l.mu.Unlock()
	<-l.closed
	return nil, net.ErrClosed
}

func (l *scriptedListener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	select {
	case <-l.closed:
	default:
		close(l.closed)
	}
	return nil
}

func (l *scriptedListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}
}

func (l *scriptedListener) accepts() []time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]time.Time(nil), l.acceptTimes...)
}

// TestServeBacksOffOnAcceptErrors verifies that a run of Accept errors is retried
// with a growing delay (not spun on at 100% CPU) and that a shutdown exits the
// loop cleanly.
func TestServeBacksOffOnAcceptErrors(t *testing.T) {
	ln := &scriptedListener{failsLeft: 3, closed: make(chan struct{})}
	mock := &testutil.MockRedashAPI{}
	registry := testutil.NewMockSourceRegistry(nil)
	srv := proxy.NewServer("127.0.0.1:0", discardLogger, mock, registry, testUser, testPass)

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ctx, ln) }()

	// Wait until the three failures are consumed and the loop has reached the
	// blocking fourth Accept.
	deadline := time.Now().Add(3 * time.Second)
	for len(ln.accepts()) < 4 {
		if time.Now().After(deadline) {
			t.Fatalf("accept loop consumed only %d attempts, want 4", len(ln.accepts()))
		}
		time.Sleep(2 * time.Millisecond)
	}

	// The shutdown goroutine closes the listener on ctx cancel, which unblocks the
	// fourth Accept with ErrClosed.
	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("Serve returned %v, want nil on shutdown", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after ctx cancel")
	}

	times := ln.accepts()
	// The retries must be spaced by the doubling backoff (~5ms, ~10ms, ~20ms): a
	// bounded, growing delay rather than a tight spin.
	gap1 := times[1].Sub(times[0])
	gap2 := times[2].Sub(times[1])
	gap3 := times[3].Sub(times[2])
	if gap1 < 4*time.Millisecond {
		t.Errorf("first retry gap = %v, want the loop to back off ~5ms rather than spin", gap1)
	}
	if gap2 < gap1 || gap3 < gap2 {
		t.Errorf("backoff did not grow across retries: %v, %v, %v", gap1, gap2, gap3)
	}
}
