// Package breaker implements a consecutive-failure circuit breaker.
//
// Closed: requests flow; FailureThreshold consecutive failures open the circuit.
// Open: requests are rejected until OpenTimeout has elapsed.
// Half-open: up to HalfOpenRequests probes are admitted; one success closes the
// circuit, one failure re-opens it for another OpenTimeout.
package breaker

import (
	"sync"
	"time"
)

// State is the breaker state.
type State int

// Breaker states.
const (
	Closed State = iota
	Open
	HalfOpen
)

func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case Open:
		return "open"
	case HalfOpen:
		return "half_open"
	default:
		return "unknown"
	}
}

// Settings configures a Breaker.
type Settings struct {
	FailureThreshold int
	OpenTimeout      time.Duration
	HalfOpenRequests int
	// Now is injectable for tests; defaults to time.Now.
	Now func() time.Time
	// OnStateChange is called (outside the lock) after every transition.
	OnStateChange func(from, to State)
}

// Breaker is safe for concurrent use.
type Breaker struct {
	mu       sync.Mutex
	s        Settings
	state    State
	failures int
	openedAt time.Time
	inFlight int // half-open probes currently admitted
}

// New returns a closed breaker. A FailureThreshold <= 0 yields a breaker that never opens.
func New(s Settings) *Breaker {
	if s.Now == nil {
		s.Now = time.Now
	}
	if s.HalfOpenRequests <= 0 {
		s.HalfOpenRequests = 1
	}
	return &Breaker{s: s}
}

// State returns the current state, promoting Open to HalfOpen when the timeout has passed.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == Open && b.s.Now().Sub(b.openedAt) >= b.s.OpenTimeout {
		return HalfOpen
	}
	return b.state
}

// Allow reports whether a request may proceed. Every allowed request must be
// followed by exactly one call to Done.
func (b *Breaker) Allow() bool {
	if b == nil || b.s.FailureThreshold <= 0 {
		return true
	}
	var from, to State
	b.mu.Lock()
	switch b.state {
	case Closed:
		b.mu.Unlock()
		return true
	case Open:
		if b.s.Now().Sub(b.openedAt) < b.s.OpenTimeout {
			b.mu.Unlock()
			return false
		}
		from, to = Open, HalfOpen
		b.state = HalfOpen
		b.inFlight = 0
	}
	// Half-open.
	if b.inFlight >= b.s.HalfOpenRequests {
		b.mu.Unlock()
		b.notify(from, to)
		return false
	}
	b.inFlight++
	b.mu.Unlock()
	b.notify(from, to)
	return true
}

// Done records the outcome of a request admitted by Allow.
func (b *Breaker) Done(success bool) {
	if b == nil || b.s.FailureThreshold <= 0 {
		return
	}
	var from, to State
	b.mu.Lock()
	switch b.state {
	case Closed:
		if success {
			b.failures = 0
		} else {
			b.failures++
			if b.failures >= b.s.FailureThreshold {
				from, to = Closed, Open
				b.trip()
			}
		}
	case HalfOpen:
		if b.inFlight > 0 {
			b.inFlight--
		}
		if success {
			from, to = HalfOpen, Closed
			b.state = Closed
			b.failures = 0
		} else {
			from, to = HalfOpen, Open
			b.trip()
		}
	case Open:
		// A request admitted before the circuit opened finished late; ignore it.
	}
	b.mu.Unlock()
	b.notify(from, to)
}

// Ignore releases a request admitted by Allow without recording an outcome,
// e.g. when the client cancelled before the upstream answered.
func (b *Breaker) Ignore() {
	if b == nil || b.s.FailureThreshold <= 0 {
		return
	}
	b.mu.Lock()
	if b.state == HalfOpen && b.inFlight > 0 {
		b.inFlight--
	}
	b.mu.Unlock()
}

func (b *Breaker) trip() {
	b.state = Open
	b.openedAt = b.s.Now()
	b.failures = 0
	b.inFlight = 0
}

func (b *Breaker) notify(from, to State) {
	if from != to && b.s.OnStateChange != nil {
		b.s.OnStateChange(from, to)
	}
}
