package upstream

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// StartHealthChecks probes every target on the configured interval until ctx
// is cancelled. It returns immediately when health checks are disabled. The
// returned WaitGroup completes once all checker goroutines have exited.
func (p *Pool) StartHealthChecks(ctx context.Context, client *http.Client, log *slog.Logger) *sync.WaitGroup {
	var wg sync.WaitGroup
	hc := p.Config.HealthCheck
	if !hc.Enabled() {
		return &wg
	}
	if client == nil {
		client = &http.Client{}
	}
	for _, t := range p.Targets {
		wg.Add(1)
		go func(t *Target) {
			defer wg.Done()
			ticker := time.NewTicker(hc.Interval.D())
			defer ticker.Stop()
			for {
				p.CheckOnce(ctx, client, t, log)
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}(t)
	}
	return &wg
}

// CheckOnce performs a single probe and applies the healthy/unhealthy thresholds.
// It is exported so tests can drive the state machine deterministically.
func (p *Pool) CheckOnce(ctx context.Context, client *http.Client, t *Target, log *slog.Logger) {
	hc := p.Config.HealthCheck
	cctx, cancel := context.WithTimeout(ctx, hc.Timeout.D())
	defer cancel()

	ok := false
	u := *t.URL
	u.Path = singleJoin(u.Path, hc.Path)
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, u.String(), nil)
	if err == nil {
		req.Header.Set("User-Agent", "go-api-gateway-healthcheck")
		var resp *http.Response
		resp, err = client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			ok = resp.StatusCode >= 200 && resp.StatusCode < 400
		}
	}
	if ctx.Err() != nil {
		return // shutting down; do not flip state on a cancelled probe
	}

	if ok {
		t.okStreak++
		t.failStreak = 0
		if !t.Healthy() && t.okStreak >= hc.HealthyThreshold {
			t.healthy.Store(true)
			p.observer.TargetHealth(p.Name, t.URL.Host, true)
			if log != nil {
				log.Info("upstream target healthy", "upstream", p.Name, "target", t.URL.String())
			}
		}
		return
	}
	t.failStreak++
	t.okStreak = 0
	if t.Healthy() && t.failStreak >= hc.UnhealthyThreshold {
		t.healthy.Store(false)
		p.observer.TargetHealth(p.Name, t.URL.Host, false)
		if log != nil {
			log.Warn("upstream target unhealthy", "upstream", p.Name, "target", t.URL.String(), "error", errString(err))
		}
	}
}

func errString(err error) string {
	if err == nil {
		return "bad status"
	}
	return err.Error()
}

// singleJoin joins two URL paths with exactly one slash between them.
func singleJoin(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	aSlash := a[len(a)-1] == '/'
	bSlash := b[0] == '/'
	switch {
	case aSlash && bSlash:
		return a + b[1:]
	case !aSlash && !bSlash:
		return a + "/" + b
	}
	return a + b
}
