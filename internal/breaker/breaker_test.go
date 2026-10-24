package breaker

import (
	"sync"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTest(threshold int) (*Breaker, *clock, *[]string) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	var transitions []string
	b := New(Settings{
		FailureThreshold: threshold,
		OpenTimeout:      10 * time.Second,
		HalfOpenRequests: 1,
		Now:              c.Now,
		OnStateChange:    func(from, to State) { transitions = append(transitions, from.String()+"->"+to.String()) },
	})
	return b, c, &transitions
}

func fail(b *Breaker, n int) {
	for i := 0; i < n; i++ {
		if b.Allow() {
			b.Done(false)
		}
	}
}

func TestOpensAfterConsecutiveFailures(t *testing.T) {
	b, _, tr := newTest(3)
	fail(b, 2)
	b.Allow()
	b.Done(true) // success resets the streak
	fail(b, 2)
	if b.State() != Closed {
		t.Fatal("breaker opened without 3 consecutive failures")
	}
	fail(b, 1)
	if b.State() != Open {
		t.Fatalf("state = %v, want open", b.State())
	}
	if b.Allow() {
		t.Fatal("open breaker admitted a request")
	}
	if len(*tr) != 1 || (*tr)[0] != "closed->open" {
		t.Fatalf("transitions = %v", *tr)
	}
}

func TestHalfOpenAdmitsLimitedProbesThenCloses(t *testing.T) {
	b, c, tr := newTest(1)
	fail(b, 1)
	c.Add(10 * time.Second)
	if b.State() != HalfOpen {
		t.Fatalf("state = %v, want half_open after timeout", b.State())
	}
	if !b.Allow() {
		t.Fatal("half-open breaker rejected the first probe")
	}
	if b.Allow() {
		t.Fatal("half-open breaker admitted a second concurrent probe")
	}
	b.Done(true)
	if b.State() != Closed || !b.Allow() {
		t.Fatal("successful probe did not close the breaker")
	}
	want := []string{"closed->open", "open->half_open", "half_open->closed"}
	if len(*tr) != len(want) {
		t.Fatalf("transitions = %v, want %v", *tr, want)
	}
}

func TestHalfOpenFailureReopens(t *testing.T) {
	b, c, _ := newTest(1)
	fail(b, 1)
	c.Add(11 * time.Second)
	if !b.Allow() {
		t.Fatal("probe rejected")
	}
	b.Done(false)
	if b.State() != Open {
		t.Fatalf("state = %v, want open", b.State())
	}
	c.Add(9 * time.Second)
	if b.Allow() {
		t.Fatal("re-opened breaker must wait a full timeout")
	}
}

func TestIgnoreReleasesProbeSlot(t *testing.T) {
	b, c, _ := newTest(1)
	fail(b, 1)
	c.Add(10 * time.Second)
	if !b.Allow() {
		t.Fatal("probe rejected")
	}
	b.Ignore()
	if b.State() != HalfOpen || !b.Allow() {
		t.Fatal("ignored probe should free the slot without changing state")
	}
}

func TestDisabledBreakerNeverOpens(t *testing.T) {
	b := New(Settings{})
	for i := 0; i < 100; i++ {
		if !b.Allow() {
			t.Fatal("disabled breaker rejected")
		}
		b.Done(false)
	}
	var nilBreaker *Breaker
	if !nilBreaker.Allow() {
		t.Fatal("nil breaker must allow")
	}
}

func TestConcurrentUse(t *testing.T) {
	b, _, _ := newTest(1000)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if b.Allow() {
					b.Done((i+j)%2 == 0)
				}
				_ = b.State()
			}
		}(i)
	}
	wg.Wait()
}
