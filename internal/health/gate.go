// Package health tracks whether Redash is reachable and gates the wire servers on
// the answer. A proxy that cannot reach Redash cannot serve anything, so it says
// so at connect time instead of accepting a session that fails one query later.
package health

import (
	"context"
	"net"
	"sync"
	"time"
)

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

// Gate is the shared answer to "can we serve right now?". It is a state machine
// plus a broadcast: transitions are decided by the Checker, and sessions learn
// about them by selecting on Down rather than polling.
type Gate struct {
	mu     sync.Mutex
	up     bool
	kind   Kind
	reason string

	// Closed for as long as the gate is down, and replaced with a fresh open
	// channel on recovery. A session that starts while the gate is already down
	// therefore sees a channel that is closed from the start.
	downCh chan struct{}

	// Depth 1: a burst of failing queries only needs to wake the checker once,
	// and Suspect must never block the session goroutine that calls it.
	suspect chan struct{}
}

// NewGate returns a gate that starts up. Nothing observes it before the first
// probe (serve runs one before it binds any listener), and starting up is what
// lets a server constructed without a checker serve unconditionally.
func NewGate() *Gate {
	return &Gate{
		up:      true,
		kind:    KindOK,
		downCh:  make(chan struct{}),
		suspect: make(chan struct{}, 1),
	}
}

// Down fires when the gate goes down, and is already closed if it is down now.
func (g *Gate) Down() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.downCh
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
	close(g.downCh)
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
	g.downCh = make(chan struct{})
	return true
}

// Suspect asks the checker to probe now rather than at its next tick. A session
// whose query just died on an infrastructure error has better evidence than the
// timer does, and calling this is how the gate trips in seconds instead of a
// minute. Never blocks.
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

// Interrupt watches the gate on behalf of one session.
type Interrupt struct {
	down <-chan struct{}
	done chan struct{}
}

// InterruptOnDown arms conn's read deadline when the gate goes down, so a session
// goroutine blocked reading from its client wakes up and can react to the drop.
// It deliberately never writes: the session goroutine is the only writer on that
// connection, and a second one would interleave with an in-flight result.
//
// Stop must be called when the session ends.
func InterruptOnDown(ctx context.Context, conn net.Conn, g *Gate) *Interrupt {
	i := &Interrupt{down: g.Down(), done: make(chan struct{})}

	go func() {
		select {
		case <-ctx.Done():
		case <-i.done:
		case <-i.down:
			_ = conn.SetReadDeadline(time.Now())
		}
	}()

	return i
}

// Dropped reports whether the gate closed at any point during this session.
//
// It asks about this session's own history rather than the gate's current state.
// A read that fails here failed because of a deadline this watch armed, and the
// only thing that arms it is the gate closing; asking the gate instead would let
// a recovery in the intervening microseconds turn a deliberate drop into an
// unexplained "receiving message" error with nothing sent to the client.
func (i *Interrupt) Dropped() bool {
	select {
	case <-i.down:
		return true
	default:
		return false
	}
}

func (i *Interrupt) Stop() { close(i.done) }
