package health

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/lhpalacio/redash-wire/internal/redash"
)

// Event names carried in the "event" field of every JSON log line the menu bar
// app reads. The app switches on these rather than on the human-readable message,
// so the wording of a log line stays free to change; these strings do not.
const (
	EventListenerReady = "listener_ready"
	EventRedashUp      = "redash_up"
	EventRedashDown    = "redash_down"
	EventDataSources   = "datasources_refreshed"
)

const (
	// The interval bounds how long a dropped VPN can go unnoticed, since the
	// timer is what finds it when no query is running. A data source list is a
	// small GET, so ten seconds costs Redash little and keeps the menu honest.
	DefaultInterval = 10 * time.Second
	DefaultTimeout  = 5 * time.Second

	// The startup probe is authoritative on one failure, so it gets the patience
	// the steady-state probes give up: a Redash that is slow to answer once at
	// launch should not stop a CLI start that would have worked a moment later.
	DefaultStartupTimeout = 10 * time.Second

	// How long after a first failure the confirming probe runs. Waiting a full
	// interval was most of the time between a VPN dropping and the menu saying
	// so; one second still separates the two probes enough that a single lost
	// packet does not fail both.
	DefaultConfirmDelay = time.Second

	// A rejected key does not fix itself, so back off hard instead of hammering a
	// wall every ten seconds for the rest of the session.
	DefaultRejectedInterval = 5 * time.Minute

	// Two failures to trip, one success to clear. The asymmetry is deliberate:
	// being wrong about "up" costs one query, being wrong about "down" costs every
	// live session. Two also absorbs the 403-serving captive portals and proxies
	// that a half-connected VPN puts in the way, which look fatal but are not.
	DefaultFailureThreshold = 2
)

// Checker is the only thing in the process that asks Redash whether it is alive.
// It owns the gate's transitions, and refreshes the registry as a side effect of
// the same call: proving reachability and listing data sources are one request.
type Checker struct {
	lister   redash.DataSourceLister
	registry *redash.SwappableRegistry
	gate     *Gate
	logger   *slog.Logger

	interval         time.Duration
	confirmDelay     time.Duration
	rejectedInterval time.Duration
	timeout          time.Duration
	startupTimeout   time.Duration
	threshold        int

	failures    int
	everUp      bool
	lastKind    Kind
	fingerprint string
}

type Option func(*Checker)

func WithInterval(d time.Duration) Option { return func(c *Checker) { c.interval = d } }
func WithTimeout(d time.Duration) Option  { return func(c *Checker) { c.timeout = d } }
func WithFailureThreshold(n int) Option   { return func(c *Checker) { c.threshold = n } }
func WithRejectedInterval(d time.Duration) Option {
	return func(c *Checker) { c.rejectedInterval = d }
}
func WithConfirmDelay(d time.Duration) Option   { return func(c *Checker) { c.confirmDelay = d } }
func WithStartupTimeout(d time.Duration) Option { return func(c *Checker) { c.startupTimeout = d } }

func NewChecker(lister redash.DataSourceLister, registry *redash.SwappableRegistry, gate *Gate, logger *slog.Logger, opts ...Option) *Checker {
	c := &Checker{
		lister:           lister,
		registry:         registry,
		gate:             gate,
		logger:           logger,
		interval:         DefaultInterval,
		confirmDelay:     DefaultConfirmDelay,
		rejectedInterval: DefaultRejectedInterval,
		timeout:          DefaultTimeout,
		startupTimeout:   DefaultStartupTimeout,
		threshold:        DefaultFailureThreshold,
		lastKind:         KindOK,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Run probes on a timer until ctx is cancelled, and out of band whenever a
// session reports a suspicious query failure. A first failure shortens the
// timer to the confirm delay, so the threshold is met in seconds rather than an
// interval later.
func (c *Checker) Run(ctx context.Context) {
	timer := time.NewTimer(c.nextInterval())
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-c.gate.Suspicions():
			// A session's query just died on an infrastructure error. That is
			// better evidence than the timer has, so check now.
		}

		_ = c.Probe(ctx)
		timer.Reset(c.nextInterval())
	}
}

// Probe runs one health check and applies the result: it swaps the registry,
// moves the gate, and emits the events the app listens for. serve calls it once
// before binding, so a cold start and a mid-session drop travel the same path.
func (c *Checker) Probe(ctx context.Context) error {
	timeout := c.timeout
	if !c.everUp {
		timeout = c.startupTimeout
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	sources, err := c.lister.ListDataSources(probeCtx)
	if err != nil {
		// A cancelled parent means we are shutting down, not that Redash is gone.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.recordFailure(err)
		return err
	}

	c.recordSuccess(sources)
	return nil
}

func (c *Checker) recordSuccess(sources []redash.DataSource) {
	c.failures = 0
	c.everUp = true
	c.lastKind = KindOK

	// Swap before recovering, never after: for the instant in between, the gate
	// would be open over a registry that is still the stale one.
	c.registry.Replace(sources)
	c.publish(sources)

	if c.gate.Recover() {
		c.logger.Info("redash is reachable again", "event", EventRedashUp, "data_sources", len(sources))
	}
}

func (c *Checker) recordFailure(err error) {
	c.failures++
	kind := classify(err)

	// Below the threshold and still serving: record the blip where someone can
	// find it, but do not move the menu bar over a single dropped packet.
	//
	// The threshold defends a state that was working. Before the first success
	// there is no such state — no session to tear down, and no evidence Redash
	// was ever reachable — so the startup probe is authoritative. Absorbing it
	// would leave a cold start that cannot reach Redash looking healthy, which is
	// the exact failure this checker exists to make visible.
	if c.everUp && c.failures < c.threshold && c.gate.Up() {
		c.logger.Debug("redash health probe failed", "kind", kind, "attempt", c.failures, "error", err)
		return
	}

	// Re-announce when the kind changes even though the gate was already down: an
	// unreachable Redash that starts answering 401 needs a different fix, and the
	// app is showing the old sentence until it hears otherwise.
	if c.gate.Fail(kind, err.Error()) || kind != c.lastKind {
		c.logger.Error("redash is unreachable", "event", EventRedashDown, "kind", kind, "error", err)
	} else {
		c.logger.Debug("redash still unreachable", "kind", kind, "error", err)
	}
	c.lastKind = kind
}

// publish emits the data source list the app renders. The full list goes out only
// when it changes; a steady state logs a count at Debug, so a menu bar left open
// for a day does not bury its log window in the same twelve names.
func (c *Checker) publish(sources []redash.DataSource) {
	views := redash.NewDataSourceViews(sources)
	encoded, err := json.Marshal(views)
	if err != nil {
		c.logger.Warn("encoding data sources for the health event", "error", err)
		return
	}

	if string(encoded) == c.fingerprint {
		c.logger.Debug("data sources unchanged", "event", EventDataSources, "count", len(views))
		return
	}

	c.fingerprint = string(encoded)
	c.logger.Info("data sources refreshed", "event", EventDataSources, "count", len(views), "sources", string(encoded))
}

func (c *Checker) nextInterval() time.Duration {
	s := c.gate.Status()
	if !s.Up && s.Kind == KindRejected {
		return c.rejectedInterval
	}
	// A failure the threshold absorbed. The gate is still up over a Redash that
	// just did not answer, and every live session is betting on that; settle it
	// now rather than at the next tick.
	if s.Up && c.failures > 0 {
		return c.confirmDelay
	}
	return c.interval
}

// classify draws the same line redash.isFatalStatus does: a response that says
// "not you" will keep saying it until the config changes, while anything else is
// worth retrying at the normal cadence.
func classify(err error) Kind {
	status, ok := redash.HTTPStatus(err)
	if !ok {
		return KindUnreachable
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return KindRejected
	default:
		return KindUnreachable
	}
}

// Suspicious reports whether a failed query is evidence about Redash rather than
// about the SQL. A false positive is cheap: it costs one extra probe, and the
// probe, not the query, decides whether the gate moves.
func Suspicious(err error) bool {
	if err == nil {
		return false
	}
	// The client went away or the caller gave up; that says nothing about Redash.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var qe *redash.QueryError
	return !errors.As(err, &qe)
}
