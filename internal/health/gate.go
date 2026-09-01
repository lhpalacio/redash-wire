// Package health tracks whether Redash is reachable and gates the wire servers on
// the answer. A proxy that cannot reach Redash cannot serve anything, so it says
// so at login and on every query while that lasts, instead of accepting a session
// that fails one query later.
package health

import "sync"

// Kind separates a failure that should clear on its own from one that needs
// someone to edit the config. They deserve different words in the UI and
// different retry cadences, so the distinction is carried rather than flattened
// into a single "down".
type Kind string

const (
	KindOK          Kind = "ok"
	KindUnreachable Kind = "unreachable"
	KindRejected    Kind = "rejected"
)

// Status is a consistent snapshot. Reading Up and Reason through separate calls
// could straddle a transition and describe a state that never existed.
type Status struct {
	Up     bool
	Kind   Kind
	Reason string
}

// Gate is the shared answer to "can we serve right now?". Transitions are
// decided by the Checker; sessions ask at login and before every query.
//
// It used to broadcast a drop so that open sessions could be torn down the
// moment it closed. That made a wrong call cost every client a reconnect, and
// with detection now a matter of seconds a wrong call is the risk worth
// designing for. An open session keeps its socket and is answered with the
// reason on each query until the gate reopens, so a false alarm costs one query.
type Gate struct {
	mu     sync.Mutex
	up     bool
	kind   Kind
	reason string

	// Depth 1: a burst of failing queries only needs to wake the checker once,
	// and Suspect must never block the session goroutine that calls it.
	suspect chan struct{}
}

// NewGate returns a gate that starts up. Starting up is what lets a server
// constructed without a checker serve unconditionally; serve closes it by hand
// before binding under -wait-for-redash, where nothing has been proved yet.
func NewGate() *Gate {
	return &Gate{
		up:      true,
		kind:    KindOK,
		suspect: make(chan struct{}, 1),
	}
}

func (g *Gate) Status() Status {
	g.mu.Lock()
	defer g.mu.Unlock()
	return Status{Up: g.up, Kind: g.kind, Reason: g.reason}
}

func (g *Gate) Up() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.up
}

// Fail closes the gate. It reports whether this was a transition, so the caller
// can log the edge instead of repeating itself every probe. The kind and reason
// are refreshed either way: an unreachable Redash that starts returning 401 has
// changed in a way worth showing, even though it was already down.
func (g *Gate) Fail(kind Kind, reason string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.kind = kind
	g.reason = reason
	if !g.up {
		return false
	}
	g.up = false
	return true
}

// Recover reopens the gate, reporting whether this was a transition.
func (g *Gate) Recover() bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.kind = KindOK
	g.reason = ""
	if g.up {
		return false
	}
	g.up = true
	return true
}

// Suspect asks the checker to probe now rather than at its next tick. A session
// whose query just died on an infrastructure error has better evidence than the
// timer does, and calling this is how the gate trips in seconds instead of an
// interval. Never blocks.
func (g *Gate) Suspect() {
	select {
	case g.suspect <- struct{}{}:
	default:
	}
}

// Suspicions is the checker's side of Suspect.
func (g *Gate) Suspicions() <-chan struct{} { return g.suspect }

// ClientMessage is the sentence handed to a psql or mysql client. It names the
// cause, because the terminal is where the person actually is when a query
// fails; the menu bar is somewhere they have to think to look.
func (g *Gate) ClientMessage() string {
	switch g.Status().Kind {
	case KindRejected:
		return "redash-wire: Redash rejected our credentials; check the profile's api_key and redash_url"
	default:
		return "redash-wire: Redash is unreachable; the proxy will serve queries again once it recovers"
	}
}
