// Package upstream manages pools of backend targets: load balancing, active
// health checks and per-target circuit breakers.
package upstream

import (
	"errors"
	"fmt"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/AkibUllah/go-api-gateway/internal/breaker"
	"github.com/AkibUllah/go-api-gateway/internal/config"
)

// ErrNoTarget is returned when every target is unhealthy, has an open circuit
// or was already tried for this request.
var ErrNoTarget = errors.New("no healthy upstream target available")

// Target is one backend instance.
type Target struct {
	URL     *url.URL
	healthy atomic.Bool
	active  atomic.Int64
	breaker *breaker.Breaker

	// health-check streaks, only touched by the checker goroutine
	okStreak, failStreak int
}

// Healthy reports the active health-check verdict (true when checks are disabled).
func (t *Target) Healthy() bool { return t.healthy.Load() }

// Active returns the number of in-flight requests.
func (t *Target) Active() int64 { return t.active.Load() }

// Breaker returns the target's circuit breaker (never nil).
func (t *Target) Breaker() *breaker.Breaker { return t.breaker }

// Acquire increments the in-flight counter; call the returned func when the
// response body has been fully consumed.
func (t *Target) Acquire() (release func()) {
	t.active.Add(1)
	var once atomic.Bool
	return func() {
		if once.CompareAndSwap(false, true) {
			t.active.Add(-1)
		}
	}
}

// Observer receives pool events; used to drive Prometheus gauges.
type Observer interface {
	TargetHealth(pool, target string, healthy bool)
	BreakerState(pool, target string, state breaker.State)
}

type nopObserver struct{}

func (nopObserver) TargetHealth(string, string, bool)          {}
func (nopObserver) BreakerState(string, string, breaker.State) {}

// Pool is a named set of targets with a balancing strategy.
type Pool struct {
	Name     string
	Config   config.Upstream
	Targets  []*Target
	balancer Balancer
	observer Observer
	retryOn  map[int]bool
}

// NewPool builds a pool from configuration. All targets start healthy so the
// gateway serves traffic immediately; active checks demote them if needed.
func NewPool(name string, cfg config.Upstream, obs Observer, now func() time.Time) (*Pool, error) {
	if obs == nil {
		obs = nopObserver{}
	}
	p := &Pool{Name: name, Config: cfg, observer: obs, retryOn: map[int]bool{}}
	for _, code := range cfg.Retries.OnStatus {
		p.retryOn[code] = true
	}
	for _, raw := range cfg.Targets {
		u, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("upstream %s: %w", name, err)
		}
		t := &Target{URL: u}
		t.healthy.Store(true)
		target := u.String()
		t.breaker = breaker.New(breaker.Settings{
			FailureThreshold: cfg.CircuitBreaker.FailureThreshold,
			OpenTimeout:      cfg.CircuitBreaker.OpenTimeout.D(),
			HalfOpenRequests: cfg.CircuitBreaker.HalfOpenRequests,
			Now:              now,
			OnStateChange: func(_, to breaker.State) {
				obs.BreakerState(name, target, to)
			},
		})
		obs.TargetHealth(name, target, true)
		obs.BreakerState(name, target, breaker.Closed)
		p.Targets = append(p.Targets, t)
	}
	switch cfg.Balancer {
	case "least_conn":
		p.balancer = &LeastConn{}
	default:
		p.balancer = &RoundRobin{}
	}
	return p, nil
}

// RetryableStatus reports whether a response status should be retried on another target.
func (p *Pool) RetryableStatus(code int) bool { return p.retryOn[code] }

// Pick selects a healthy target whose breaker admits the request, skipping any
// in exclude. The caller must call target.Breaker().Done exactly once.
func (p *Pool) Pick(exclude map[*Target]bool) (*Target, error) {
	candidates := make([]*Target, 0, len(p.Targets))
	for _, t := range p.Targets {
		if !exclude[t] && t.Healthy() && t.breaker.State() != breaker.Open {
			candidates = append(candidates, t)
		}
	}
	for len(candidates) > 0 {
		t := p.balancer.Next(candidates)
		if t.breaker.Allow() {
			return t, nil
		}
		// Half-open with probes exhausted: drop it and try the rest.
		candidates = remove(candidates, t)
	}
	return nil, ErrNoTarget
}

// HealthyCount returns the number of targets currently passing health checks.
func (p *Pool) HealthyCount() int {
	n := 0
	for _, t := range p.Targets {
		if t.Healthy() {
			n++
		}
	}
	return n
}

func remove(ts []*Target, t *Target) []*Target {
	out := ts[:0]
	for _, x := range ts {
		if x != t {
			out = append(out, x)
		}
	}
	return out
}
