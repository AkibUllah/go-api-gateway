package ratelimit

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// BenchmarkMemoryAllowParallel spreads 10k client keys over all goroutines,
// the realistic shape for a per-IP limit.
func BenchmarkMemoryAllowParallel(b *testing.B) {
	m := NewMemory(Rule{Rate: 1000, Period: time.Second, Burst: 1000}, nil)
	keys := make([]string, 10_000)
	for i := range keys {
		keys[i] = "ip:10.0." + strconv.Itoa(i/256) + "." + strconv.Itoa(i%256)
	}
	var n atomic.Uint64
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _ = m.Allow(ctx, keys[n.Add(1)%uint64(len(keys))])
		}
	})
}

// BenchmarkMemoryAllowHotKey is the worst case: every goroutine hits one bucket.
func BenchmarkMemoryAllowHotKey(b *testing.B) {
	m := NewMemory(Rule{Rate: 1000, Period: time.Second, Burst: 1000}, nil)
	ctx := context.Background()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _ = m.Allow(ctx, "consumer:key:pathao")
		}
	})
}
