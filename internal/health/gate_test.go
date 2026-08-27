package health

import (
	"context"
	"net"
	"strings"
	"testing"
)

func TestGateStartsUp(t *testing.T) {
	// Servers built without a checker must serve unconditionally, so an unprobed
	// gate has to be open rather than "unknown".
	g := NewGate()

	if !g.Up() {
		t.Fatal("a new gate is down, want up")
	}
	select {
	case <-g.Down():
		t.Fatal("the down channel of a new gate is closed, want open")
	default:
	}
}

func TestFailClosesTheDownChannel(t *testing.T) {
	g := NewGate()
	down := g.Down()

	if !g.Fail(KindUnreachable, "dial tcp: i/o timeout") {
		t.Fatal("Fail reported no transition on the first failure")
	}

	select {
	case <-down:
	default:
		t.Fatal("the down channel captured before Fail did not close")
	}

	got := g.Status()
	if got.Up || got.Kind != KindUnreachable || got.Reason != "dial tcp: i/o timeout" {
		t.Fatalf("Status() = %+v, want a down/unreachable status carrying the reason", got)
	}
}

func TestDownIsAlreadyClosedForASessionStartedWhileDown(t *testing.T) {
	g := NewGate()
	g.Fail(KindUnreachable, "down")

	// A session that connects after the gate has already dropped must not wait for
	// the *next* transition; it has to see the current one.
	select {
	case <-g.Down():
	default:
		t.Fatal("Down() returned an open channel while the gate was down")
	}
}

func TestRecoverReopensTheDownChannel(t *testing.T) {
	g := NewGate()
	g.Fail(KindUnreachable, "down")

	if !g.Recover() {
		t.Fatal("Recover reported no transition after a failure")
	}
	if !g.Up() || g.Status().Kind != KindOK {
		t.Fatalf("Status() = %+v, want an up/ok status", g.Status())
	}

	select {
	case <-g.Down():
		t.Fatal("the down channel is still closed after Recover; a later session would drop immediately")
	default:
	}
}

func TestOnlyTheEdgeIsReportedButTheReasonStillUpdates(t *testing.T) {
	g := NewGate()

	g.Fail(KindUnreachable, "first")
	if g.Fail(KindRejected, "second") {
		t.Error("Fail reported a transition while already down")
	}
	// Repeating the log line every probe would be noise, but the app is still
	// showing the old sentence until the state itself changes.
	if got := g.Status(); got.Kind != KindRejected || got.Reason != "second" {
		t.Errorf("Status() = %+v, want the refreshed kind and reason", got)
	}

	g.Recover()
	if g.Recover() {
		t.Error("Recover reported a transition while already up")
	}
}

func TestSuspectNeverBlocksAndCoalesces(t *testing.T) {
	g := NewGate()

	// Called from session goroutines on every failing query: a burst must not
	// block any of them, and the checker only needs to wake once.
	for range 100 {
		g.Suspect()
	}

	select {
	case <-g.Suspicions():
	default:
		t.Fatal("Suspect delivered nothing to the checker")
	}
	select {
	case <-g.Suspicions():
		t.Fatal("Suspect queued more than one wakeup")
	default:
	}
}

func TestClientMessageNamesTheCause(t *testing.T) {
	g := NewGate()

	g.Fail(KindUnreachable, "i/o timeout")
	if got := g.ClientMessage(); !strings.Contains(got, "unreachable") {
		t.Errorf("ClientMessage() = %q, want it to say Redash is unreachable", got)
	}

	g.Fail(KindRejected, "status 401")
	if got := g.ClientMessage(); !strings.Contains(got, "api_key") {
		t.Errorf("ClientMessage() = %q, want it to point at the API key", got)
	}
}

func TestAnInterruptRemembersTheDropAfterRecovery(t *testing.T) {
	// The session's read was interrupted because the gate closed. If the gate
	// reopens before the session goroutine gets to look — a recovery probe
	// landing in the microseconds in between — asking the gate for its current
	// state would call a deliberate drop an unexplained read error and send the
	// client nothing at all.
	g := NewGate()
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	watch := InterruptOnDown(context.Background(), server, g)
	defer watch.Stop()

	if watch.Dropped() {
		t.Fatal("Dropped() is true before the gate ever closed")
	}

	g.Fail(KindUnreachable, "down")
	g.Recover()

	if !g.Up() {
		t.Fatal("the gate did not recover")
	}
	if !watch.Dropped() {
		t.Error("Dropped() forgot the drop as soon as the gate recovered")
	}
}

func TestASessionStartedAfterRecoveryDoesNotInheritTheDrop(t *testing.T) {
	g := NewGate()
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	g.Fail(KindUnreachable, "down")
	g.Recover()

	watch := InterruptOnDown(context.Background(), server, g)
	defer watch.Stop()

	if watch.Dropped() {
		t.Error("a session that connected after recovery reports the earlier drop as its own")
	}
}
