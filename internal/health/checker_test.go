package health_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lhpalacio/redash-wire/internal/health"
	"github.com/lhpalacio/redash-wire/internal/redash"
)

// stubLister stands in for the Redash API. Each call pops the next outcome, so a
// test can spell out a sequence of failures and recoveries.
type stubLister struct {
	results []listResult
	calls   atomic.Int64
	// Signalled on every call, so a test waiting for an out-of-band probe can
	// block on the event instead of polling a counter across two goroutines.
	probed chan struct{}
}

type listResult struct {
	sources []redash.DataSource
	err     error
}

func (s *stubLister) ListDataSources(context.Context) ([]redash.DataSource, error) {
	n := int(s.calls.Add(1)) - 1
	if s.probed != nil {
		select {
		case s.probed <- struct{}{}:
		default:
		}
	}
	return s.results[min(n, len(s.results)-1)].sources, s.results[min(n, len(s.results)-1)].err
}

// captured collects the JSON log stream, which is the contract the menu bar app
// consumes: asserting on the fields here is asserting on what the app will read.
type captured struct{ buf *bytes.Buffer }

func newCapture() (*slog.Logger, *captured) {
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return logger, &captured{buf: buf}
}

func (c *captured) events(name string) []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(c.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			continue
		}
		if record["event"] == name {
			out = append(out, record)
		}
	}
	return out
}

func sources() []redash.DataSource {
	return []redash.DataSource{{ID: 1, Name: "Warehouse", Type: "pg"}}
}

func TestSecondFailureTripsTheGate(t *testing.T) {
	// A proxy that has been working: the first failure is absorbed, the second is
	// believed. Tearing down every live session over one dropped packet is the
	// failure mode the threshold exists to prevent.
	lister := &stubLister{results: []listResult{
		{sources: sources()},
		{err: errors.New("dial tcp: i/o timeout")},
	}}
	gate := health.NewGate()
	logger, log := newCapture()
	checker := health.NewChecker(lister, redash.NewSwappableRegistry(sources()), gate, logger)

	checker.Probe(context.Background()) // succeeds, so there is now a state to defend
	checker.Probe(context.Background())

	if !gate.Up() {
		t.Fatal("the gate dropped on the first failure after a success, want it to survive one blip")
	}
	if got := log.events(health.EventRedashDown); len(got) != 0 {
		t.Errorf("emitted %d redash_down events below the threshold, want 0", len(got))
	}

	checker.Probe(context.Background())

	if gate.Up() {
		t.Fatal("the gate is still up after two consecutive failures")
	}
	if got := gate.Status().Kind; got != health.KindUnreachable {
		t.Errorf("Kind = %q, want %q: a transport error is not the user's key being wrong", got, health.KindUnreachable)
	}

	events := log.events(health.EventRedashDown)
	if len(events) != 1 {
		t.Fatalf("emitted %d redash_down events, want exactly 1 (the edge, not every probe)", len(events))
	}
	if events[0]["kind"] != string(health.KindUnreachable) {
		t.Errorf("redash_down carried kind %v, want %q", events[0]["kind"], health.KindUnreachable)
	}
}

func TestOneSuccessClears(t *testing.T) {
	lister := &stubLister{results: []listResult{
		{err: errors.New("down")},
		{err: errors.New("down")},
		{sources: sources()},
	}}
	gate := health.NewGate()
	registry := redash.NewSwappableRegistry(nil)
	logger, log := newCapture()
	checker := health.NewChecker(lister, registry, gate, logger)

	checker.Probe(context.Background())
	checker.Probe(context.Background())
	if err := checker.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}

	if !gate.Up() {
		t.Fatal("the gate is still down after a successful probe")
	}
	if len(log.events(health.EventRedashUp)) != 1 {
		t.Error("recovery emitted no redash_up event, so the app would still be showing amber")
	}
	// The probe that proves reachability is the same call that lists the sources,
	// which is what makes the registry refresh free.
	if _, ok := registry.Lookup("Warehouse"); !ok {
		t.Error("the registry was not refreshed by the recovering probe")
	}
}

func TestRejectedIsDistinguishedFromUnreachable(t *testing.T) {
	// A real 401 through the real client, so the classification is exercised end
	// to end rather than against a hand-built error.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	gate := health.NewGate()
	logger, log := newCapture()
	// WithRejectedThreshold(1) confirms the rejected verdict on the first probe;
	// the confirmation window itself is covered by
	// TestRejectedNeedsConfirmationBeforeTheLongBackoff.
	checker := health.NewChecker(redash.NewClient(server.URL, "bad-key"), redash.NewSwappableRegistry(nil), gate, logger,
		health.WithRejectedThreshold(1))

	checker.Probe(context.Background())
	checker.Probe(context.Background())

	if got := gate.Status().Kind; got != health.KindRejected {
		t.Fatalf("Kind = %q, want %q: 401 will not fix itself on a retry", got, health.KindRejected)
	}
	if !strings.Contains(gate.ClientMessage(), "api_key") {
		t.Errorf("ClientMessage() = %q, want it to point at the key rather than the network", gate.ClientMessage())
	}
	events := log.events(health.EventRedashDown)
	if len(events) != 1 || events[0]["kind"] != string(health.KindRejected) {
		t.Errorf("redash_down events = %v, want one carrying kind=rejected", events)
	}
}

func TestKindChangeIsReAnnouncedWhileStillDown(t *testing.T) {
	unauthorized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer unauthorized.Close()

	gate := health.NewGate()
	logger, log := newCapture()

	// First take the gate down as unreachable...
	transport := &stubLister{results: []listResult{{err: errors.New("dial tcp: i/o timeout")}}}
	down := health.NewChecker(transport, redash.NewSwappableRegistry(nil), gate, logger)
	down.Probe(context.Background())
	down.Probe(context.Background())

	// ...then let it start answering 401. The gate never came back up, but the fix
	// the user needs just changed, so the app has to hear about it. This checker
	// confirms the rejected verdict on the first probe so the test stays about the
	// re-announcement, not the confirmation window.
	rejected := health.NewChecker(redash.NewClient(unauthorized.URL, "bad-key"), redash.NewSwappableRegistry(nil), gate, logger,
		health.WithRejectedThreshold(1))
	rejected.Probe(context.Background())
	rejected.Probe(context.Background())

	kinds := make([]any, 0, 2)
	for _, e := range log.events(health.EventRedashDown) {
		kinds = append(kinds, e["kind"])
	}
	if len(kinds) != 2 || kinds[0] != "unreachable" || kinds[1] != "rejected" {
		t.Errorf("redash_down kinds = %v, want [unreachable rejected]", kinds)
	}
}

func TestDataSourcesArePublishedOnlyWhenTheyChange(t *testing.T) {
	lister := &stubLister{results: []listResult{{sources: sources()}}}
	gate := health.NewGate()
	logger, log := newCapture()
	checker := health.NewChecker(lister, redash.NewSwappableRegistry(nil), gate, logger)

	checker.Probe(context.Background())
	checker.Probe(context.Background())

	events := log.events(health.EventDataSources)
	if len(events) != 2 {
		t.Fatalf("emitted %d datasources_refreshed events, want 2 (one per probe)", len(events))
	}
	// The first carries the list the app renders; the second is a steady-state
	// heartbeat, so it must not repeat twelve names into the log window.
	if _, ok := events[0]["sources"]; !ok {
		t.Error("the first datasources_refreshed carried no sources payload")
	}
	if _, ok := events[1]["sources"]; ok {
		t.Error("an unchanged list repeated its full payload")
	}
	if events[1]["level"] != "DEBUG" {
		t.Errorf("the unchanged event logged at %v, want DEBUG so it stays out of the way", events[1]["level"])
	}

	var decoded []redash.DataSourceView
	if err := json.Unmarshal([]byte(events[0]["sources"].(string)), &decoded); err != nil {
		t.Fatalf("the sources payload does not decode as the app would read it: %v", err)
	}
	if len(decoded) != 1 || decoded[0].Wire != "postgres" {
		t.Errorf("decoded = %+v, want one source resolved to the postgres wire", decoded)
	}
}

func TestASuspicionWakesTheChecker(t *testing.T) {
	lister := &stubLister{results: []listResult{{sources: sources()}}, probed: make(chan struct{}, 4)}
	gate := health.NewGate()
	logger, _ := newCapture()
	// An interval long enough that a probe arriving within the deadline can only
	// have come from the suspicion, never from the timer.
	checker := health.NewChecker(lister, redash.NewSwappableRegistry(nil), gate, logger,
		health.WithInterval(time.Hour))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go checker.Run(ctx)

	gate.Suspect()

	select {
	case <-lister.probed:
	case <-time.After(5 * time.Second):
		t.Fatal("a suspicion did not trigger an out-of-band probe: the gate would wait a full interval to notice")
	}
}

func TestShutdownIsNotAHealthSignal(t *testing.T) {
	lister := &stubLister{results: []listResult{{err: context.Canceled}}}
	gate := health.NewGate()
	logger, log := newCapture()
	checker := health.NewChecker(lister, redash.NewSwappableRegistry(nil), gate, logger,
		health.WithFailureThreshold(1))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	checker.Probe(ctx)

	if !gate.Up() {
		t.Error("a cancelled context took the gate down; shutting down is not Redash being unreachable")
	}
	if len(log.events(health.EventRedashDown)) != 0 {
		t.Error("shutdown emitted a redash_down event")
	}
}

func TestTheStartupProbeIsAuthoritative(t *testing.T) {
	// Launch-at-login before the VPN is up: the very first probe fails. There is
	// no session to protect and nothing has ever proved Redash was reachable, so
	// absorbing this as a blip would leave the menu bar green over a proxy that
	// cannot serve a single query.
	lister := &stubLister{results: []listResult{{err: errors.New("dial tcp: connection refused")}}}
	gate := health.NewGate()
	logger, log := newCapture()
	checker := health.NewChecker(lister, redash.NewSwappableRegistry(nil), gate, logger)

	checker.Probe(context.Background())

	if gate.Up() {
		t.Fatal("the gate is up after a failed startup probe, so the proxy would accept connections it cannot serve")
	}
	if len(log.events(health.EventRedashDown)) != 1 {
		t.Error("a failed startup probe emitted no redash_down event, so the app would show green")
	}
}

func TestAFirstFailureIsConfirmedWithinTheConfirmDelay(t *testing.T) {
	// A working proxy sees one failure. Waiting a whole interval to confirm it
	// used to be most of the time between a VPN dropping and the menu saying so,
	// so the checker must come back for the second look almost at once.
	lister := &stubLister{results: []listResult{
		{sources: sources()},
		{err: errors.New("dial tcp: i/o timeout")},
	}, probed: make(chan struct{}, 4)}
	gate := health.NewGate()
	logger, _ := newCapture()
	// An interval long enough that a second probe arriving within the deadline
	// can only have come from the confirm delay, never from the timer.
	checker := health.NewChecker(lister, redash.NewSwappableRegistry(nil), gate, logger,
		health.WithInterval(time.Hour), health.WithConfirmDelay(10*time.Millisecond))

	checker.Probe(context.Background()) // the success that makes the next failure a blip
	<-lister.probed

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go checker.Run(ctx)

	gate.Suspect() // first failure
	<-lister.probed
	if !gate.Up() {
		t.Fatal("the gate dropped on the first failure, so the confirm delay had nothing to confirm")
	}

	select {
	case <-lister.probed: // the confirming probe
	case <-time.After(5 * time.Second):
		t.Fatal("no confirming probe within 5s of the first failure: the gate would wait a full interval")
	}
	// Run applies the result after the lister returns; give it a moment to do so.
	deadline := time.Now().Add(time.Second)
	for gate.Up() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if gate.Up() {
		t.Fatal("the gate is still up after the confirming probe failed")
	}
}

// deadlineLister records how much time each probe was given, which is the
// deadline on the context the checker hands it.
type deadlineLister struct {
	budgets []time.Duration
}

func (d *deadlineLister) ListDataSources(ctx context.Context) ([]redash.DataSource, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		d.budgets = append(d.budgets, 0)
	} else {
		d.budgets = append(d.budgets, time.Until(deadline))
	}
	return sources(), nil
}

func TestTheStartupProbeGetsTheLongerTimeout(t *testing.T) {
	// The startup probe is authoritative on one failure, so it keeps the patience
	// the steady-state probes give up. One slow first answer must not stop a
	// start that would have worked a moment later.
	lister := &deadlineLister{}
	gate := health.NewGate()
	logger, _ := newCapture()
	checker := health.NewChecker(lister, redash.NewSwappableRegistry(nil), gate, logger,
		health.WithTimeout(5*time.Second), health.WithStartupTimeout(10*time.Second))

	checker.Probe(context.Background())
	checker.Probe(context.Background())

	if len(lister.budgets) != 2 {
		t.Fatalf("recorded %d probes, want 2", len(lister.budgets))
	}
	within := func(got, want time.Duration) bool { return got > want-time.Second && got <= want }
	if !within(lister.budgets[0], 10*time.Second) {
		t.Errorf("startup probe budget = %s, want about 10s", lister.budgets[0])
	}
	if !within(lister.budgets[1], 5*time.Second) {
		t.Errorf("steady-state probe budget = %s, want about 5s", lister.budgets[1])
	}
}

func TestFailedProbesSayWhenTheNextOneIs(t *testing.T) {
	// The menu counts down to the next probe, so every failed probe has to say
	// how far away that is: the edge that closes the gate, and each one after.
	lister := &stubLister{results: []listResult{{err: errors.New("dial tcp: i/o timeout")}}}
	gate := health.NewGate()
	logger, log := newCapture()
	checker := health.NewChecker(lister, redash.NewSwappableRegistry(nil), gate, logger,
		health.WithInterval(10*time.Second))

	checker.Probe(context.Background()) // the startup probe is authoritative: gate closes
	checker.Probe(context.Background()) // still down

	down := log.events(health.EventRedashDown)
	if len(down) != 1 || down[0]["retry_in_seconds"] != float64(10) {
		t.Errorf("redash_down events = %v, want one carrying retry_in_seconds=10", down)
	}
	retry := log.events(health.EventRedashRetry)
	if len(retry) != 1 || retry[0]["retry_in_seconds"] != float64(10) {
		t.Fatalf("redash_retry events = %v, want one carrying retry_in_seconds=10", retry)
	}
	// The app runs without -debug, so a Debug line would never reach it.
	if retry[0]["level"] != "INFO" {
		t.Errorf("redash_retry logged at %v, want INFO so the app sees it", retry[0]["level"])
	}
}

func TestARejectedKeyCountsDownToTheLongBackoff(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	gate := health.NewGate()
	logger, log := newCapture()
	checker := health.NewChecker(redash.NewClient(server.URL, "bad-key"), redash.NewSwappableRegistry(nil), gate, logger,
		health.WithRejectedInterval(5*time.Minute), health.WithRejectedThreshold(1))

	checker.Probe(context.Background())

	down := log.events(health.EventRedashDown)
	if len(down) != 1 || down[0]["retry_in_seconds"] != float64(300) {
		t.Errorf("redash_down events = %v, want one carrying retry_in_seconds=300: a rejected key backs off", down)
	}
}

func TestRejectedNeedsConfirmationBeforeTheLongBackoff(t *testing.T) {
	// A captive portal or a half-connected VPN serves 403 for a stretch before it
	// lets real traffic through. A single such response must not park every
	// session on the key message and back off for five minutes; the rejected
	// verdict has to persist across the confirmation window first. Until then the
	// outage reads as plain unreachable and keeps probing at the normal interval.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	gate := health.NewGate()
	logger, log := newCapture()
	checker := health.NewChecker(redash.NewClient(server.URL, "bad-key"), redash.NewSwappableRegistry(nil), gate, logger,
		health.WithInterval(10*time.Second), health.WithRejectedInterval(5*time.Minute),
		health.WithRejectedThreshold(3))

	// First probe: the gate closes (the startup probe is authoritative), but a
	// single 403 is not yet the key being wrong, so it reads as unreachable and
	// the countdown is the normal interval, not the long backoff.
	checker.Probe(context.Background())
	if got := gate.Status().Kind; got != health.KindUnreachable {
		t.Fatalf("after one 403, Kind = %q, want %q: the rejected verdict is not yet confirmed", got, health.KindUnreachable)
	}
	down := log.events(health.EventRedashDown)
	if len(down) != 1 || down[0]["kind"] != string(health.KindUnreachable) || down[0]["retry_in_seconds"] != float64(10) {
		t.Fatalf("first redash_down = %v, want one unreachable event retrying in 10s", down)
	}

	// Second probe stays inside the window: still unreachable, still the normal
	// interval.
	checker.Probe(context.Background())
	if got := gate.Status().Kind; got != health.KindUnreachable {
		t.Fatalf("after two 403s, Kind = %q, want %q: still confirming", got, health.KindUnreachable)
	}

	// Third probe crosses the threshold: now it is the key, and the countdown
	// becomes the five-minute backoff.
	checker.Probe(context.Background())
	if got := gate.Status().Kind; got != health.KindRejected {
		t.Fatalf("after three 403s, Kind = %q, want %q: the verdict is confirmed", got, health.KindRejected)
	}
	if !strings.Contains(gate.ClientMessage(), "api_key") {
		t.Errorf("ClientMessage() = %q, want it to point at the key once confirmed", gate.ClientMessage())
	}
	down = log.events(health.EventRedashDown)
	last := down[len(down)-1]
	if last["kind"] != string(health.KindRejected) || last["retry_in_seconds"] != float64(300) {
		t.Errorf("final redash_down = %v, want a rejected event backing off for 300s", last)
	}
}

func TestRunProbesAtOnceWhenNothingHasProbedYet(t *testing.T) {
	// Under -wait-for-redash the listeners bind before anything asks Redash, so
	// the checker's loop is the first probe and must not wait an interval. The
	// gate starts closed in that mode, so a success here is also the edge that
	// tells the app it can go green.
	lister := &stubLister{results: []listResult{{sources: sources()}}, probed: make(chan struct{}, 4)}
	gate := health.NewGate()
	gate.Fail(health.KindUnreachable, "waiting for the first check")
	logger, log := newCapture()
	checker := health.NewChecker(lister, redash.NewSwappableRegistry(nil), gate, logger,
		health.WithInterval(time.Hour))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go checker.Run(ctx)

	select {
	case <-lister.probed:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not probe on entry: the app would sit at 'checking' for a whole interval")
	}
	deadline := time.Now().Add(time.Second)
	for !gate.Up() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !gate.Up() {
		t.Fatal("the gate did not open after the first successful probe")
	}
	if len(log.events(health.EventRedashUp)) != 1 {
		t.Error("opening a gate that started closed emitted no redash_up, so the app would never leave 'checking'")
	}
}

func TestRunDoesNotRepeatAProbeAlreadyMade(t *testing.T) {
	// Without -wait-for-redash serve probes before binding; the loop must not
	// ask again the instant it starts, or every start costs two calls.
	lister := &stubLister{results: []listResult{{sources: sources()}}, probed: make(chan struct{}, 4)}
	gate := health.NewGate()
	logger, _ := newCapture()
	checker := health.NewChecker(lister, redash.NewSwappableRegistry(nil), gate, logger,
		health.WithInterval(time.Hour))

	checker.Probe(context.Background())
	<-lister.probed

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go checker.Run(ctx)

	select {
	case <-lister.probed:
		t.Fatal("Run repeated the probe serve had just made")
	case <-time.After(300 * time.Millisecond):
	}
}

// TestSuspicious pins the rule for when a failed query is allowed to wake the
// checker: a failure of the SQL itself, or of the caller (a client that went
// away, a deadline that passed), says nothing about Redash; anything else,
// such as a transport failure or an unexpected status, is worth one probe.
func TestSuspicious(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"no error", nil, false},
		{"the SQL failed on the data source", &redash.QueryError{Message: "syntax error"}, false},
		{"a wrapped SQL failure", fmt.Errorf("running: %w", &redash.QueryError{Message: "no such table"}), false},
		{"the client went away", context.Canceled, false},
		{"the client went away, wrapped", fmt.Errorf("executing request: %w", context.Canceled), false},
		{"the caller gave up", fmt.Errorf("polling: %w", context.DeadlineExceeded), false},
		{"a transport failure", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, true},
		{"an unexpected status", errors.New("redash API error (status 502): bad gateway"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := health.Suspicious(tt.err); got != tt.want {
				t.Errorf("Suspicious(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
