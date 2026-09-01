package health

import (
	"strings"
	"testing"
)

func TestGateStartsUp(t *testing.T) {
	// Servers built without a checker must serve unconditionally, so an unprobed
	// gate has to be open rather than "unknown".
	if !NewGate().Up() {
		t.Fatal("a new gate is down, want up")
	}
}

func TestFailIsATransitionThatCarriesTheReason(t *testing.T) {
	g := NewGate()

	if !g.Fail(KindUnreachable, "dial tcp: i/o timeout") {
		t.Fatal("Fail reported no transition on the first failure")
	}
	got := g.Status()
	if got.Up || got.Kind != KindUnreachable || got.Reason != "dial tcp: i/o timeout" {
		t.Fatalf("Status() = %+v, want a down/unreachable status carrying the reason", got)
	}
}

func TestRecoverIsATransitionBackToOK(t *testing.T) {
	g := NewGate()
	g.Fail(KindUnreachable, "down")

	if !g.Recover() {
		t.Fatal("Recover reported no transition after a failure")
	}
	if !g.Up() || g.Status().Kind != KindOK {
		t.Fatalf("Status() = %+v, want an up/ok status", g.Status())
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
