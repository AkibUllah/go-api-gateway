package ratelimit

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Unix(1_700_000_000, 0)} }

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// limiterSuite runs the same behavioural tests against both backends.
func limiterSuite(t *testing.T, mk func(Rule, func() time.Time) Limiter) {
	ctx := context.Background()

	t.Run("burst then reject with retry-after", func(t *testing.T) {
		c := newClock()
		l := mk(Rule{Rate: 2, Period: time.Second, Burst: 3}, c.Now)
		for i := 0; i < 3; i++ {
			d, err := l.Allow(ctx, "k")
			if err != nil || !d.Allowed {
				t.Fatalf("request %d rejected: %+v %v", i, d, err)
			}
			if d.Remaining != 2-i || d.Limit != 3 {
				t.Fatalf("request %d: remaining=%d limit=%d", i, d.Remaining, d.Limit)
			}
		}
		d, _ := l.Allow(ctx, "k")
		if d.Allowed {
			t.Fatal("4th request allowed with burst 3")
		}
		if d.RetryAfter <= 0 || d.RetryAfter > 500*time.Millisecond {
			t.Fatalf("retry after = %v, want (0, 500ms] at 2 tokens/s", d.RetryAfter)
		}
	})

	t.Run("refills at the configured rate", func(t *testing.T) {
		c := newClock()
		l := mk(Rule{Rate: 10, Period: time.Second, Burst: 1}, c.Now)
		if d, _ := l.Allow(ctx, "k"); !d.Allowed {
			t.Fatal("first request rejected")
		}
		if d, _ := l.Allow(ctx, "k"); d.Allowed {
			t.Fatal("second request allowed before refill")
		}
		c.Add(100 * time.Millisecond)
		if d, _ := l.Allow(ctx, "k"); !d.Allowed {
			t.Fatal("request rejected after one token refilled")
		}
	})

	t.Run("refill is capped at burst", func(t *testing.T) {
		c := newClock()
		l := mk(Rule{Rate: 100, Period: time.Second, Burst: 2}, c.Now)
		c.Add(time.Hour)
		allowed := 0
		for i := 0; i < 5; i++ {
			if d, _ := l.Allow(ctx, "k"); d.Allowed {
				allowed++
			}
		}
		if allowed != 2 {
			t.Fatalf("allowed %d after a long idle period, want burst=2", allowed)
		}
	})

	t.Run("keys are independent", func(t *testing.T) {
		c := newClock()
		l := mk(Rule{Rate: 1, Period: time.Minute, Burst: 1}, c.Now)
		for _, k := range []string{"a", "b", "c"} {
			if d, _ := l.Allow(ctx, k); !d.Allowed {
				t.Fatalf("key %s rejected on first request", k)
			}
		}
	})
}

func TestMemoryLimiter(t *testing.T) {
	limiterSuite(t, func(r Rule, now func() time.Time) Limiter { return NewMemory(r, now) })
}

func TestMemoryLimiterEvictsIdleBuckets(t *testing.T) {
	c := newClock()
	m := NewMemory(Rule{Rate: 10, Period: time.Second, Burst: 10}, c.Now)
	for i := 0; i < 500; i++ {
		_, _ = m.Allow(context.Background(), fmt.Sprintf("client-%d", i))
	}
	if m.Len() != 500 {
		t.Fatalf("tracked %d buckets, want 500", m.Len())
	}
	c.Add(2 * time.Minute)
	// 2000 fresh keys touch every one of the 64 shards, so each one sweeps.
	for i := 0; i < 2000; i++ {
		_, _ = m.Allow(context.Background(), fmt.Sprintf("new-%d", i))
	}
	if n := m.Len(); n != 2000 {
		t.Fatalf("tracked %d buckets, want only the 2000 fresh ones", n)
	}
}

func TestMemoryLimiterIsExactUnderConcurrency(t *testing.T) {
	m := NewMemory(Rule{Rate: 1, Period: time.Hour, Burst: 100}, nil)
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < 20; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if d, _ := m.Allow(context.Background(), "hot-key"); d.Allowed {
					allowed.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if allowed.Load() != 100 {
		t.Fatalf("allowed %d of 1000 concurrent requests, want exactly burst=100", allowed.Load())
	}
}

func newRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return mr, client
}

func TestRedisLimiter(t *testing.T) {
	_, client := newRedis(t)
	n := 0
	limiterSuite(t, func(r Rule, now func() time.Time) Limiter {
		n++ // fresh key prefix per subtest
		return NewRedis(client, fmt.Sprintf("test%d:", n), r, now)
	})
}

func TestRedisLimiterSharesStateAcrossReplicas(t *testing.T) {
	_, client := newRedis(t)
	c := newClock()
	rule := Rule{Rate: 1, Period: time.Minute, Burst: 2}
	a := NewRedis(client, "rl:", rule, c.Now)
	b := NewRedis(client, "rl:", rule, c.Now)
	ctx := context.Background()
	d1, _ := a.Allow(ctx, "user")
	d2, _ := b.Allow(ctx, "user")
	d3, _ := a.Allow(ctx, "user")
	if !d1.Allowed || !d2.Allowed || d3.Allowed {
		t.Fatalf("two replicas did not share one bucket: %v %v %v", d1.Allowed, d2.Allowed, d3.Allowed)
	}
}

func TestRedisLimiterSetsExpiry(t *testing.T) {
	mr, client := newRedis(t)
	l := NewRedis(client, "rl:", Rule{Rate: 10, Period: time.Second, Burst: 10}, nil)
	if _, err := l.Allow(context.Background(), "k"); err != nil {
		t.Fatal(err)
	}
	if ttl := mr.TTL("rl:k"); ttl <= 0 || ttl > 2*time.Minute {
		t.Fatalf("bucket TTL = %v, want a bounded expiry", ttl)
	}
}

func TestRedisLimiterReportsBackendErrors(t *testing.T) {
	mr, client := newRedis(t)
	l := NewRedis(client, "rl:", Rule{Rate: 1, Period: time.Second, Burst: 1}, nil)
	mr.Close()
	if _, err := l.Allow(context.Background(), "k"); err == nil {
		t.Fatal("expected an error with Redis down")
	}
}
