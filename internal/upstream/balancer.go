package upstream

import "sync/atomic"

// Balancer chooses one target from a non-empty candidate list.
type Balancer interface {
	Next(candidates []*Target) *Target
}

// RoundRobin cycles through candidates with an atomic counter.
type RoundRobin struct {
	n atomic.Uint64
}

// Next implements Balancer.
func (r *RoundRobin) Next(c []*Target) *Target {
	i := r.n.Add(1) - 1
	return c[i%uint64(len(c))]
}

// LeastConn picks the candidate with the fewest in-flight requests; ties are
// broken round-robin so equal targets share load instead of the first one
// taking every request.
type LeastConn struct {
	n atomic.Uint64
}

// Next implements Balancer.
func (l *LeastConn) Next(c []*Target) *Target {
	start := int((l.n.Add(1) - 1) % uint64(len(c))) //nolint:gosec // result is < len(c)
	best := c[start]
	bestActive := best.Active()
	for i := 1; i < len(c); i++ {
		t := c[(start+i)%len(c)]
		if a := t.Active(); a < bestActive {
			best, bestActive = t, a
		}
	}
	return best
}
