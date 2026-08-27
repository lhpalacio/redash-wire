package health_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
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

func TestProbeBelowThresholdKeepsServing(t *testing.T) {
	lister := &stubLister{results: []listResult{{err: errors.New("dial tcp: i/o timeout")}}}
	gate := health.NewGate()
	logger, log := newCapture()
	checker := health.NewChecker(lister, redash.NewSwappableRegistry(sources()), gate, logger)

	// One failure is a blip. Tearing down every live session over a single dropped
	// packet is the failure mode the threshold exists to prevent.
	if err := checker.Probe(context.Background()); err == nil {
		t.Fatal("Probe returned nil for a failing lister")
	}
	if !gate.Up() {
		t.Error("the gate dropped on the first failure, want it to survive one blip")
	}
	if got := log.events(health.EventRedashDown); len(got) != 0 {
		t.Errorf("emitted %d redash_down events below the threshold, want 0", len(got))
	}
}

func TestSecondFailureTripsTheGate(t *testing.T) {
	lister := &stubLister{results: []listResult{{err: errors.New("dial tcp: i/o timeout")}}}
	gate := health.NewGate()
	logger, log := newCapture()
	checker := health.NewChecker(lister, redash.NewSwappableRegistry(sources()), gate, logger)

	checker.Probe(context.Background())
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
	checker := health.NewChecker(redash.NewClient(server.URL, "bad-key"), redash.NewSwappableRegistry(nil), gate, logger)

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
	// the user needs just changed, so the app has to hear about it.
	rejected := health.NewChecker(redash.NewClient(unauthorized.URL, "bad-key"), redash.NewSwappableRegistry(nil), gate, logger)
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
