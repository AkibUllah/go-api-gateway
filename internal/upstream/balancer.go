package upstream

import (
	"sync"
	"sync/atomic"
)

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

// RoundRobin is smooth weighted round-robin, the nginx algorithm: every pick
// adds each candidate's weight to its running score, takes the highest score
// and subtracts the total from the winner, so weights 5:1:1 interleave as
// a a b a c a a instead of five a's in a row. When all candidates weigh the
// same (the common case) it uses a lock-free counter instead.
type RoundRobin struct {
	n  atomic.Uint64
	sw smoothWeighted
}

// Next implements Balancer.
func (r *RoundRobin) Next(c []Candidate) *Target {
	if uniform(c) {
		i := r.n.Add(1) - 1
		return c[i%uint64(len(c))].Target
	}
	return r.sw.next(c)
}

// LeastConn picks the candidate with the fewest in-flight requests per unit
// of weight. Ties between equal weights are broken round-robin so equal
// targets share load instead of the first one taking every request; ties
// between different weights (typically several idle targets) go through
// smooth weighted round-robin so low traffic is still split by weight.
type LeastConn struct {
	n  atomic.Uint64
	sw smoothWeighted
}

// Next implements Balancer.
func (l *LeastConn) Next(c []Candidate) *Target {
	start := int((l.n.Add(1) - 1) % uint64(len(c))) //nolint:gosec // result is < len(c)
	best := c[start]
	bestActive := best.Target.Active()
	tied, mixed := 1, false
	for i := 1; i < len(c); i++ {
		x := c[(start+i)%len(c)]
		a := x.Target.Active()
		// active/weight compared by cross-multiplying to stay in integers.
		switch d := a*best.Weight - bestActive*x.Weight; {
		case d < 0:
			best, bestActive, tied, mixed = x, a, 1, false
		case d == 0:
			tied++
			mixed = mixed || x.Weight != best.Weight
		}
	}
	if tied == 1 || !mixed {
		return best.Target
	}
	var buf [8]Candidate
	ties := buf[:0]
	for _, x := range c {
		if x.Target.Active()*best.Weight == bestActive*x.Weight {
			ties = append(ties, x)
		}
	}
	if len(ties) == 0 { // in-flight counts moved under us
		return best.Target
	}
	return l.sw.next(ties)
}

// smoothWeighted holds the lock for the per-target scores of smooth weighted
// round-robin. Scores live on the targets; a target belongs to one pool and a
// pool has one balancer, so one mutex guards them all.
type smoothWeighted struct {
	mu sync.Mutex
}

func (s *smoothWeighted) next(c []Candidate) *Target {
	s.mu.Lock()
	defer s.mu.Unlock()
	var total int64
	var best *Target
	for _, x := range c {
		x.Target.score += x.Weight
		total += x.Weight
		if best == nil || x.Target.score > best.score {
			best = x.Target
		}
	}
	best.score -= total
	return best
}

func uniform(c []Candidate) bool {
	for _, x := range c[1:] {
		if x.Weight != c[0].Weight {
			return false
		}
	}
	return true
}
