package upstream

import "sync/atomic"

// Balancer chooses one target from a non-empty candidate list.
type Balancer interface {
	Next(candidates []Candidate) *Target
}

// weightScale is the fixed-point unit of effective weights: a configured
// weight of 1 is 1000, so slow-start can ramp in steps finer than one unit.
const weightScale = 1000

// Candidate is a target eligible for one pick together with its effective
// weight in thousandths of a configured weight unit.
type Candidate struct {
	Target *Target
	Weight int64
}

// RoundRobin cycles through candidates with an atomic counter.
type RoundRobin struct {
	n atomic.Uint64
}

// Next implements Balancer.
func (r *RoundRobin) Next(c []Candidate) *Target {
	i := r.n.Add(1) - 1
	return c[i%uint64(len(c))].Target
}

// LeastConn picks the candidate with the fewest in-flight requests; ties are
// broken round-robin so equal targets share load instead of the first one
// taking every request.
type LeastConn struct {
	n atomic.Uint64
}

// Next implements Balancer.
func (l *LeastConn) Next(c []Candidate) *Target {
	start := int((l.n.Add(1) - 1) % uint64(len(c))) //nolint:gosec // result is < len(c)
	best := c[start].Target
	bestActive := best.Active()
	for i := 1; i < len(c); i++ {
		t := c[(start+i)%len(c)].Target
		if a := t.Active(); a < bestActive {
			best, bestActive = t, a
		}
	}
	return best
}
