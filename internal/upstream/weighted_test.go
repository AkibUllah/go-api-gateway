package upstream

import (
	"strings"
	"sync"
	"testing"

	"github.com/AkibUllah/go-api-gateway/internal/config"
)

func weightedPool(t testing.TB, balancer string, weights ...int) *Pool {
	t.Helper()
	cfg := config.Upstream{Balancer: balancer}
	for i, w := range weights {
		cfg.Targets = append(cfg.Targets, config.Target{URL: "http://" + string(rune('a'+i)) + ":1", Weight: w})
	}
	p, err := NewPool("weighted", cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func countPicks(t *testing.T, p *Pool, n int) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for i := 0; i < n; i++ {
		counts[pick(t, p).URL.Hostname()]++
	}
	return counts
}

func TestWeightedRoundRobinDistribution(t *testing.T) {
	p := weightedPool(t, "round_robin", 3, 1)
	counts := countPicks(t, p, 1000)
	if counts["a"] != 750 || counts["b"] != 250 {
		t.Fatalf("weights 3:1 gave %v, want a=750 b=250", counts)
	}
}

func TestWeightedRoundRobinIsSmooth(t *testing.T) {
	p := weightedPool(t, "round_robin", 5, 1, 1)
	var seq strings.Builder
	for i := 0; i < 14; i++ {
		seq.WriteString(pick(t, p).URL.Hostname())
	}
	// The nginx sequence for 5:1:1 is a a b a c a a, repeating: the light
	// targets are spread through the cycle instead of trailing a burst of a's.
	if got, want := seq.String(), "aabacaaaabacaa"; got != want {
		t.Fatalf("sequence = %s, want %s", got, want)
	}
}

func TestWeightedRoundRobinSkipsExcludedTargets(t *testing.T) {
	p := weightedPool(t, "round_robin", 3, 2, 1)
	a := p.Targets[0]
	counts := map[string]int{}
	for i := 0; i < 300; i++ {
		tg, err := p.Pick(map[*Target]bool{a: true})
		if err != nil {
			t.Fatal(err)
		}
		tg.Breaker().Done(true)
		counts[tg.URL.Hostname()]++
	}
	if counts["a"] != 0 || counts["b"] != 200 || counts["c"] != 100 {
		t.Fatalf("with a excluded, 2:1 gave %v", counts)
	}
	// Once a is back it gets its share again without starving the others.
	counts = countPicks(t, p, 600)
	if counts["a"] < 290 || counts["b"] < 190 || counts["c"] < 90 {
		t.Fatalf("after re-inclusion 3:2:1 gave %v", counts)
	}
}

func TestWeightedRoundRobinConcurrent(t *testing.T) {
	p := weightedPool(t, "round_robin", 3, 1)
	var mu sync.Mutex
	counts := map[string]int{}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				tg, err := p.Pick(nil)
				if err != nil {
					t.Error(err)
					return
				}
				tg.Breaker().Done(true)
				mu.Lock()
				counts[tg.URL.Hostname()]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if counts["a"] != 3000 || counts["b"] != 1000 {
		t.Fatalf("concurrent 3:1 gave %v, want a=3000 b=1000", counts)
	}
}

func TestLeastConnScalesActiveByWeight(t *testing.T) {
	p := weightedPool(t, "least_conn", 3, 1)
	a, b := p.Targets[0], p.Targets[1]

	// a: 2 in flight / weight 3 is less loaded than b: 1 / weight 1.
	ra1, ra2, rb := a.Acquire(), a.Acquire(), b.Acquire()
	for i := 0; i < 5; i++ {
		if got := pick(t, p); got != a {
			t.Fatalf("picked %s, a has the lower load per weight", got.URL.Host)
		}
	}
	// a: 3 / 3 = 1 ties with b: 1 / 1; ties split by weight, so both appear.
	ra3 := a.Acquire()
	seen := map[*Target]int{}
	for i := 0; i < 8; i++ {
		seen[pick(t, p)]++
	}
	if seen[a] != 6 || seen[b] != 2 {
		t.Fatalf("equal load per weight should split 3:1, got a=%d b=%d", seen[a], seen[b])
	}
	// a: 4 / 3 is now above b: 1 / 1.
	ra4 := a.Acquire()
	if got := pick(t, p); got != b {
		t.Fatalf("picked %s, b has the lower load per weight", got.URL.Host)
	}
	for _, r := range []func(){ra1, ra2, ra3, ra4, rb} {
		r()
	}
}

func TestLeastConnIdleTargetsShareByWeight(t *testing.T) {
	p := weightedPool(t, "least_conn", 3, 1)
	counts := countPicks(t, p, 1000)
	if counts["a"] != 750 || counts["b"] != 250 {
		t.Fatalf("idle 3:1 least_conn gave %v, want a=750 b=250", counts)
	}
}

func BenchmarkPickRoundRobin(b *testing.B) {
	benchPick(b, weightedPool(b, "round_robin", 1, 1, 1, 1))
}

func BenchmarkPickWeightedRoundRobin(b *testing.B) {
	benchPick(b, weightedPool(b, "round_robin", 4, 2, 1, 1))
}

func benchPick(b *testing.B, p *Pool) {
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			t, err := p.Pick(nil)
			if err != nil {
				b.Error(err)
				return
			}
			t.Breaker().Done(true)
		}
	})
}
