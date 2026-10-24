package upstream

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AkibUllah/go-api-gateway/internal/breaker"
	"github.com/AkibUllah/go-api-gateway/internal/config"
)

func pool(t *testing.T, balancer string, targets ...string) *Pool {
	t.Helper()
	p, err := NewPool("test", config.Upstream{Targets: targets, Balancer: balancer}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func pick(t *testing.T, p *Pool) *Target {
	t.Helper()
	tg, err := p.Pick(nil)
	if err != nil {
		t.Fatal(err)
	}
	tg.Breaker().Done(true)
	return tg
}

func TestRoundRobinCyclesTargets(t *testing.T) {
	p := pool(t, "round_robin", "http://a:1", "http://b:1", "http://c:1")
	counts := map[string]int{}
	for i := 0; i < 300; i++ {
		counts[pick(t, p).URL.Host]++
	}
	for host, n := range counts {
		if n != 100 {
			t.Errorf("%s got %d picks, want 100", host, n)
		}
	}
}

func TestLeastConnPrefersIdleTarget(t *testing.T) {
	p := pool(t, "least_conn", "http://a:1", "http://b:1")
	busy := p.Targets[0]
	release := busy.Acquire()
	for i := 0; i < 10; i++ {
		if got := pick(t, p); got == busy {
			t.Fatal("least_conn picked the busy target")
		}
	}
	release()
	release() // idempotent
	if busy.Active() != 0 {
		t.Fatalf("active = %d after release", busy.Active())
	}
	seen := map[*Target]bool{}
	for i := 0; i < 4; i++ {
		seen[pick(t, p)] = true
	}
	if len(seen) != 2 {
		t.Fatal("ties should be spread over both targets")
	}
}

func TestPickSkipsUnhealthyExcludedAndOpenTargets(t *testing.T) {
	cfg := config.Upstream{
		Targets:        []string{"http://a:1", "http://b:1", "http://c:1"},
		CircuitBreaker: config.CircuitBreaker{FailureThreshold: 1, OpenTimeout: config.Duration(time.Hour)},
	}
	p, err := NewPool("test", cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, b, c := p.Targets[0], p.Targets[1], p.Targets[2]
	a.healthy.Store(false)
	// Trip c's breaker.
	if !c.Breaker().Allow() {
		t.Fatal("closed breaker rejected")
	}
	c.Breaker().Done(false)
	for i := 0; i < 5; i++ {
		if got := pick(t, p); got != b {
			t.Fatalf("picked %s, only b is eligible", got.URL.Host)
		}
	}
	if _, err := p.Pick(map[*Target]bool{b: true}); !errors.Is(err, ErrNoTarget) {
		t.Fatalf("err = %v, want ErrNoTarget", err)
	}
	if p.HealthyCount() != 2 {
		t.Fatalf("healthy = %d, want 2 (breaker state is separate from health)", p.HealthyCount())
	}
}

type recordingObserver struct {
	health []bool
	states []breaker.State
}

func (r *recordingObserver) TargetHealth(_, _ string, h bool) { r.health = append(r.health, h) }
func (r *recordingObserver) BreakerState(_, _ string, s breaker.State) {
	r.states = append(r.states, s)
}

func TestHealthCheckThresholds(t *testing.T) {
	var up atomic.Bool
	up.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/base/health" {
			t.Errorf("probe path = %q", r.URL.Path)
		}
		if !up.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()

	obs := &recordingObserver{}
	p, err := NewPool("svc", config.Upstream{
		Targets: []string{srv.URL + "/base/"},
		HealthCheck: config.HealthCheck{
			Path: "/health", Timeout: config.Duration(time.Second),
			HealthyThreshold: 2, UnhealthyThreshold: 2,
		},
	}, obs, nil)
	if err != nil {
		t.Fatal(err)
	}
	tg := p.Targets[0]
	ctx := context.Background()

	up.Store(false)
	p.CheckOnce(ctx, srv.Client(), tg, nil)
	if !tg.Healthy() {
		t.Fatal("one failure must not flip a target below unhealthy_threshold=2")
	}
	p.CheckOnce(ctx, srv.Client(), tg, nil)
	if tg.Healthy() {
		t.Fatal("two failures should mark the target unhealthy")
	}
	up.Store(true)
	p.CheckOnce(ctx, srv.Client(), tg, nil)
	if tg.Healthy() {
		t.Fatal("one success must not restore a target below healthy_threshold=2")
	}
	p.CheckOnce(ctx, srv.Client(), tg, nil)
	if !tg.Healthy() {
		t.Fatal("two successes should restore the target")
	}
	want := []bool{true, false, true}
	if len(obs.health) != len(want) {
		t.Fatalf("observer saw %v, want %v", obs.health, want)
	}
}

func TestHealthCheckLoopStopsOnCancel(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()
	p, _ := NewPool("svc", config.Upstream{
		Targets: []string{srv.URL},
		HealthCheck: config.HealthCheck{
			Path: "/health", Interval: config.Duration(5 * time.Millisecond), Timeout: config.Duration(time.Second),
			HealthyThreshold: 1, UnhealthyThreshold: 1,
		},
	}, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	wg := p.StartHealthChecks(ctx, srv.Client(), nil)
	deadline := time.Now().Add(2 * time.Second)
	for hits.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	wg.Wait()
	if hits.Load() < 3 {
		t.Fatalf("only %d probes in 2s", hits.Load())
	}
}

func TestSingleJoin(t *testing.T) {
	cases := [][3]string{
		{"", "/health", "/health"},
		{"/base", "/health", "/base/health"},
		{"/base/", "/health", "/base/health"},
		{"/base", "health", "/base/health"},
		{"/base/", "", "/base/"},
	}
	for _, c := range cases {
		if got := singleJoin(c[0], c[1]); got != c[2] {
			t.Errorf("singleJoin(%q,%q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
}
