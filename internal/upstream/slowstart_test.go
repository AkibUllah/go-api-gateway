package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AkibUllah/go-api-gateway/internal/config"
)

// fakeClock is a settable clock shared by the pool, its breakers and slow-start.
type fakeClock struct{ ns atomic.Int64 }

func newFakeClock() *fakeClock {
	c := &fakeClock{}
	c.ns.Store(time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC).UnixNano())
	return c
}

func (c *fakeClock) Now() time.Time          { return time.Unix(0, c.ns.Load()) }
func (c *fakeClock) Advance(d time.Duration) { c.ns.Add(int64(d)) }

// slowStartPool has targets a and b (weights wa, wb) with a 10s slow-start and
// health checks against srv that flip state after a single probe.
func slowStartPool(t *testing.T, clock *fakeClock, balancer string, wa, wb int) (*Pool, *httptest.Server, *atomic.Bool) {
	t.Helper()
	var up atomic.Bool
	up.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !up.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(srv.Close)
	p, err := NewPool("slow", config.Upstream{
		Balancer:  balancer,
		Targets:   []config.Target{{URL: "http://a:1", Weight: wa}, {URL: srv.URL, Weight: wb}},
		SlowStart: config.Duration(10 * time.Second),
		HealthCheck: config.HealthCheck{
			Path: "/health", Timeout: config.Duration(time.Second),
			HealthyThreshold: 1, UnhealthyThreshold: 1,
		},
	}, nil, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	return p, srv, &up
}

// recover takes target b down and back up through real health probes.
func recoverTarget(t *testing.T, p *Pool, srv *httptest.Server, up *atomic.Bool) *Target {
	t.Helper()
	b := p.Targets[1]
	up.Store(false)
	p.CheckOnce(context.Background(), srv.Client(), b, nil)
	if b.Healthy() {
		t.Fatal("b should be unhealthy")
	}
	up.Store(true)
	p.CheckOnce(context.Background(), srv.Client(), b, nil)
	if !b.Healthy() {
		t.Fatal("b should have recovered")
	}
	return b
}

func TestSlowStartRampsLinearly(t *testing.T) {
	clock := newFakeClock()
	p, srv, up := slowStartPool(t, clock, "round_robin", 1, 2)
	b := p.Targets[1]
	if w := p.effectiveWeight(b, clock.Now().UnixNano()); w != 2000 {
		t.Fatalf("a target healthy from the start must not ramp, weight = %d", w)
	}
	recoverTarget(t, p, srv, up)

	steps := []struct {
		after time.Duration
		want  int64
	}{
		{0, 200},                       // 10% of 2000
		{2500 * time.Millisecond, 650}, // 200 + 1800/4
		{5 * time.Second, 1100},
		{9 * time.Second, 1820},
		{10 * time.Second, 2000},
		{time.Minute, 2000},
	}
	start := clock.Now()
	for _, s := range steps {
		now := start.Add(s.after).UnixNano()
		if got := p.effectiveWeight(b, now); got != s.want {
			t.Errorf("after %v: effective weight %d, want %d", s.after, got, s.want)
		}
	}
	if b.recoveredAt.Load() != 0 {
		t.Fatal("a finished ramp should be cleared")
	}
}

func TestSlowStartShiftsTrafficWithTheClock(t *testing.T) {
	clock := newFakeClock()
	p, srv, up := slowStartPool(t, clock, "round_robin", 1, 1)
	recoverTarget(t, p, srv, up)

	share := func(n int) int {
		got := 0
		for i := 0; i < n; i++ {
			if pick(t, p) == p.Targets[1] {
				got++
			}
		}
		return got
	}
	// Just recovered: b weighs 100 against a's 1000, so 1 pick in 11.
	if got := share(1100); got != 100 {
		t.Fatalf("right after recovery b got %d of 1100 picks, want 100", got)
	}
	clock.Advance(5 * time.Second) // b at 550 of 1000
	if got := share(1550); got != 550 {
		t.Fatalf("half way b got %d of 1550 picks, want 550", got)
	}
	clock.Advance(5 * time.Second) // ramp over: plain round-robin again
	if got := share(1000); got != 500 {
		t.Fatalf("after the window b got %d of 1000 picks, want 500", got)
	}
}

func TestSlowStartWithLeastConn(t *testing.T) {
	clock := newFakeClock()
	p, srv, up := slowStartPool(t, clock, "least_conn", 1, 1)
	a := p.Targets[0]
	b := recoverTarget(t, p, srv, up)

	// One request in flight on b at 10% weight is ten times the load of one
	// on a, so a takes the next requests until it has more than ten.
	rb := b.Acquire()
	var releases []func()
	for i := 0; i < 10; i++ {
		if got := pick(t, p); got != a {
			t.Fatalf("pick %d went to the ramping target", i)
		}
		releases = append(releases, a.Acquire())
	}
	releases = append(releases, a.Acquire())
	if got := pick(t, p); got != b {
		t.Fatal("with 11 in flight on a, b should take the next request")
	}
	rb()
	for _, r := range releases {
		r()
	}
}

func TestSlowStartRestartsOnEachRecovery(t *testing.T) {
	clock := newFakeClock()
	p, srv, up := slowStartPool(t, clock, "round_robin", 1, 1)
	b := recoverTarget(t, p, srv, up)
	clock.Advance(8 * time.Second)
	recoverTarget(t, p, srv, up)
	if got := p.effectiveWeight(b, clock.Now().UnixNano()); got != 100 {
		t.Fatalf("second recovery should restart the ramp at 10%%, got %d", got)
	}
}

func TestSlowStartClockSteppingBackStaysAtFloor(t *testing.T) {
	clock := newFakeClock()
	p, srv, up := slowStartPool(t, clock, "round_robin", 1, 1)
	b := recoverTarget(t, p, srv, up)
	if got := p.effectiveWeight(b, clock.Now().Add(-time.Hour).UnixNano()); got != 100 {
		t.Fatalf("weight before the recovery time = %d, want the 10%% floor", got)
	}
}

func TestNoSlowStartMeansFullWeightOnRecovery(t *testing.T) {
	clock := newFakeClock()
	p, srv, up := slowStartPool(t, clock, "round_robin", 1, 1)
	p.slowStart = 0
	b := recoverTarget(t, p, srv, up)
	if got := p.effectiveWeight(b, clock.Now().UnixNano()); got != 1000 {
		t.Fatalf("without slow-start a recovered target weighs %d, want 1000", got)
	}
	if b.recoveredAt.Load() != 0 {
		t.Fatal("no ramp should be recorded when slow-start is off")
	}
}

func TestStatusReportsEffectiveWeights(t *testing.T) {
	clock := newFakeClock()
	p, srv, up := slowStartPool(t, clock, "round_robin", 3, 2)
	recoverTarget(t, p, srv, up)
	clock.Advance(5 * time.Second)

	st := p.Status()
	if len(st) != 2 {
		t.Fatalf("status has %d targets", len(st))
	}
	a, b := st[0], st[1]
	if a.URL != "http://a:1" || !a.Healthy || a.Circuit != "closed" || a.Weight != 3 || a.EffectiveWeight != 3 || a.SlowStartEnds != nil {
		t.Fatalf("steady target status = %+v", a)
	}
	if b.Weight != 2 || b.EffectiveWeight != 1.1 { // 0.2 + 1.8/2
		t.Fatalf("ramping target weight %d, effective %v; want 2 and 1.1", b.Weight, b.EffectiveWeight)
	}
	if b.SlowStartEnds == nil || !b.SlowStartEnds.Equal(clock.Now().Add(5*time.Second)) {
		t.Fatalf("slow_start_ends = %v, want %v", b.SlowStartEnds, clock.Now().Add(5*time.Second))
	}

	p.Targets[0].healthy.Store(false)
	if w := p.Status()[0].EffectiveWeight; w != 0 {
		t.Fatalf("unhealthy target effective weight = %v, want 0", w)
	}
}

func TestStatusRedactsCredentialsAndZeroesOpenCircuits(t *testing.T) {
	p, err := NewPool("creds", config.Upstream{
		Targets:        []config.Target{{URL: "http://user:secret@a:1", Weight: 4}},
		CircuitBreaker: config.CircuitBreaker{FailureThreshold: 1, OpenTimeout: config.Duration(time.Hour)},
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	tg := p.Targets[0]
	if !tg.Breaker().Allow() {
		t.Fatal("closed breaker rejected")
	}
	tg.Breaker().Done(false)
	st := p.Status()[0]
	if st.URL != "http://user:xxxxx@a:1" {
		t.Fatalf("url = %q, credentials must be redacted", st.URL)
	}
	if st.Circuit != "open" || st.EffectiveWeight != 0 || st.Weight != 4 {
		t.Fatalf("open circuit status = %+v", st)
	}
}
