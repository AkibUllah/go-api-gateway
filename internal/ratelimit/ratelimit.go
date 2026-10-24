// Package ratelimit provides token-bucket rate limiters backed by process
// memory or Redis.
package ratelimit

import (
	"context"
	"hash/maphash"
	"math"
	"sync"
	"time"
)

// Decision is the outcome of a rate limit check.
type Decision struct {
	Allowed    bool
	Limit      int           // bucket capacity (burst)
	Remaining  int           // whole tokens left after this request
	RetryAfter time.Duration // time until one token is available, when rejected
}

// Limiter decides whether a request identified by key may proceed.
type Limiter interface {
	Allow(ctx context.Context, key string) (Decision, error)
}

// Rule is a token bucket: Rate tokens are added every Period, up to Burst.
type Rule struct {
	Rate   int
	Period time.Duration
	Burst  int
}

func (r Rule) perSecond() float64 { return float64(r.Rate) / r.Period.Seconds() }

const shardCount = 64

// Memory is an in-process token bucket limiter. Keys are spread over shards so
// that concurrent requests for different clients rarely contend on a lock.
// Idle buckets are evicted lazily during Allow to bound memory.
type Memory struct {
	rule   Rule
	now    func() time.Time
	seed   maphash.Seed
	shards [shardCount]shard
	idle   time.Duration
}

type shard struct {
	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewMemory returns a memory limiter. now may be nil (time.Now).
func NewMemory(rule Rule, now func() time.Time) *Memory {
	if now == nil {
		now = time.Now
	}
	m := &Memory{rule: rule, now: now, seed: maphash.MakeSeed()}
	// A bucket that has been idle long enough to refill completely carries no
	// state worth keeping.
	m.idle = time.Duration(float64(rule.Burst)/rule.perSecond()*float64(time.Second)) + time.Minute
	for i := range m.shards {
		m.shards[i].buckets = make(map[string]*bucket)
	}
	return m
}

// Allow implements Limiter. It never returns an error.
func (m *Memory) Allow(_ context.Context, key string) (Decision, error) {
	s := &m.shards[maphash.String(m.seed, key)%shardCount]
	now := m.now()
	rate := m.rule.perSecond()
	burst := float64(m.rule.Burst)

	s.mu.Lock()
	defer s.mu.Unlock()
	if now.Sub(s.lastSweep) > m.idle {
		for k, b := range s.buckets {
			if now.Sub(b.last) > m.idle {
				delete(s.buckets, k)
			}
		}
		s.lastSweep = now
	}
	b, ok := s.buckets[key]
	if !ok {
		b = &bucket{tokens: burst, last: now}
		s.buckets[key] = b
	} else if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(burst, b.tokens+elapsed*rate)
		b.last = now
	}
	d := Decision{Limit: m.rule.Burst}
	if b.tokens >= 1 {
		b.tokens--
		d.Allowed = true
	} else {
		d.RetryAfter = time.Duration((1 - b.tokens) / rate * float64(time.Second))
	}
	d.Remaining = int(math.Floor(b.tokens))
	return d, nil
}

// Len returns the number of tracked buckets (for tests and metrics).
func (m *Memory) Len() int {
	n := 0
	for i := range m.shards {
		s := &m.shards[i]
		s.mu.Lock()
		n += len(s.buckets)
		s.mu.Unlock()
	}
	return n
}
