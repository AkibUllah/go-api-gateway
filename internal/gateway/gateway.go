// Package gateway assembles routes, upstream pools and middleware from a
// config and swaps the whole assembly atomically on reload.
package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"

	"github.com/AkibUllah/go-api-gateway/internal/auth"
	"github.com/AkibUllah/go-api-gateway/internal/config"
	"github.com/AkibUllah/go-api-gateway/internal/httpx"
	"github.com/AkibUllah/go-api-gateway/internal/metrics"
	mw "github.com/AkibUllah/go-api-gateway/internal/middleware"
	"github.com/AkibUllah/go-api-gateway/internal/proxy"
	"github.com/AkibUllah/go-api-gateway/internal/ratelimit"
	"github.com/AkibUllah/go-api-gateway/internal/upstream"
)

// gzipMinSize is the smallest body worth compressing.
const gzipMinSize = 1024

// Gateway is the root http.Handler. It is safe to call Reload concurrently
// with ServeHTTP: requests already in progress finish on the old runtime.
type Gateway struct {
	log     *slog.Logger
	metrics *metrics.Metrics
	now     func() time.Time

	current  atomic.Pointer[runtime]
	draining atomic.Bool
	reloadMu sync.Mutex
	// RetireGrace is how long a replaced runtime keeps its connections and
	// Redis client open for requests that started before the swap.
	RetireGrace time.Duration
}

// runtime is everything derived from one config.
type runtime struct {
	cfg        *config.Config
	handler    http.Handler
	pools      map[string]*upstream.Pool
	transports []*http.Transport
	redis      *redis.Client
	stopChecks context.CancelFunc
	checks     []*sync.WaitGroup
}

// New builds a gateway from cfg and starts upstream health checks.
func New(cfg *config.Config, log *slog.Logger, m *metrics.Metrics) (*Gateway, error) {
	if m == nil {
		m = metrics.New()
	}
	g := &Gateway{log: log, metrics: m, now: time.Now, RetireGrace: 30 * time.Second}
	rt, err := g.build(cfg)
	if err != nil {
		return nil, err
	}
	g.current.Store(rt)
	return g, nil
}

// Config returns the active configuration.
func (g *Gateway) Config() *config.Config { return g.current.Load().cfg }

// Metrics returns the collectors.
func (g *Gateway) Metrics() *metrics.Metrics { return g.metrics }

// Reload swaps in a new configuration. On error the running config is kept.
func (g *Gateway) Reload(cfg *config.Config) error {
	g.reloadMu.Lock()
	defer g.reloadMu.Unlock()
	rt, err := g.build(cfg)
	if err != nil {
		g.metrics.ConfigReloads.WithLabelValues("error").Inc()
		return err
	}
	old := g.current.Swap(rt)
	g.metrics.ConfigReloads.WithLabelValues("success").Inc()
	if old.cfg.Server.Listen != cfg.Server.Listen || old.cfg.Admin.Listen != cfg.Admin.Listen {
		g.log.Warn("listen address changes take effect on restart",
			"running", old.cfg.Server.Listen, "configured", cfg.Server.Listen)
	}
	old.stopChecks()
	time.AfterFunc(g.RetireGrace, old.close)
	return nil
}

// SetDraining makes /readyz fail so load balancers stop sending traffic
// before shutdown, while in-flight and late requests are still served.
func (g *Gateway) SetDraining(v bool) { g.draining.Store(v) }

// Close stops health checks and releases connections.
func (g *Gateway) Close() {
	rt := g.current.Load()
	rt.stopChecks()
	rt.close()
}

func (rt *runtime) close() {
	for _, wg := range rt.checks {
		wg.Wait()
	}
	for _, t := range rt.transports {
		t.CloseIdleConnections()
	}
	if rt.redis != nil {
		_ = rt.redis.Close()
	}
}

func (g *Gateway) build(cfg *config.Config) (rt *runtime, err error) {
	rt = &runtime{cfg: cfg, pools: map[string]*upstream.Pool{}}
	ctx, cancel := context.WithCancel(context.Background())
	rt.stopChecks = cancel
	defer func() {
		if err != nil {
			cancel()
			rt.close()
		}
	}()

	trusted := cfg.Server.TrustedNets()
	clientIP := func(r *http.Request) string { return httpx.ClientIP(r, trusted) }
	trustedPeer := func(r *http.Request) bool {
		host, _, splitErr := net.SplitHostPort(r.RemoteAddr)
		if splitErr != nil {
			return false
		}
		ip := net.ParseIP(host)
		for _, n := range trusted {
			if n.Contains(ip) {
				return true
			}
		}
		return false
	}

	if cfg.Redis.Addr != "" {
		rt.redis = redis.NewClient(&redis.Options{
			Addr:         cfg.Redis.Addr,
			Password:     cfg.Redis.Password,
			DB:           cfg.Redis.DB,
			DialTimeout:  2 * time.Second,
			ReadTimeout:  250 * time.Millisecond,
			WriteTimeout: 250 * time.Millisecond,
		})
	}

	handlers := map[string]http.Handler{}
	for _, name := range sortedNames(cfg.Upstreams) {
		ucfg := cfg.Upstreams[name]
		pool, perr := upstream.NewPool(name, ucfg, g.metrics, g.now)
		if perr != nil {
			return nil, perr
		}
		tr := proxy.NewTransport(ucfg.DialTimeout.D(), ucfg.MaxIdleConns)
		rt.transports = append(rt.transports, tr)
		rt.pools[name] = pool
		handlers[name] = proxy.New(proxy.Options{
			Pool: pool, Transport: tr, Metrics: g.metrics, Log: g.log, TrustedPeer: trustedPeer,
		})
		checkTr := proxy.NewTransport(ucfg.HealthCheck.Timeout.D(), 2)
		rt.transports = append(rt.transports, checkTr)
		checkClient := &http.Client{Transport: checkTr}
		rt.checks = append(rt.checks, pool.StartHealthChecks(ctx, checkClient, g.log))
	}

	jwts := map[string]*auth.JWTVerifier{}
	for name, jc := range cfg.JWT {
		var jwks *auth.JWKS
		if jc.JWKSURL != "" {
			jwks = auth.NewJWKS(jc.JWKSURL, jc.JWKSCacheTTL.D())
		}
		v, verr := auth.NewJWTVerifier(jc, jwks)
		if verr != nil {
			return nil, fmt.Errorf("jwt.%s: %w", name, verr)
		}
		jwts[name] = v
	}
	keys := map[string]*auth.APIKeyVerifier{}
	for name, kc := range cfg.APIKeys {
		v, kerr := auth.NewAPIKeyVerifier(kc)
		if kerr != nil {
			return nil, fmt.Errorf("api_keys.%s: %w", name, kerr)
		}
		keys[name] = v
	}

	var routes []*proxy.Route
	for _, rc := range cfg.Routes {
		chain := []mw.Middleware{}
		if rc.CORS != "" {
			chain = append(chain, mw.CORS(cfg.CORS[rc.CORS]))
		}
		chain = append(chain, mw.BodyLimit(int64(rc.MaxBodyBytes)))
		if rc.JWT != "" {
			chain = append(chain, mw.JWT(jwts[rc.JWT], rc.Name, g.metrics, g.log))
		}
		if rc.APIKeys != "" {
			chain = append(chain, mw.APIKey(keys[rc.APIKeys], rc.Name, g.metrics))
		}
		if rc.RateLimit != "" {
			pol := cfg.RateLimits[rc.RateLimit]
			rule := ratelimit.Rule{Rate: pol.Rate, Period: pol.Period.D(), Burst: pol.Burst}
			var lim ratelimit.Limiter
			if pol.Backend == "redis" {
				lim = ratelimit.NewRedis(rt.redis, "gw:rl:", rule, g.now)
			} else {
				lim = ratelimit.NewMemory(rule, g.now)
			}
			chain = append(chain, mw.RateLimit(lim, mw.RateLimitOptions{
				Route: rc.Name, Policy: rc.RateLimit, Key: pol.Key, FailOpen: cfg.Redis.FailOpen,
				ClientIP: clientIP, Metrics: g.metrics, Log: g.log,
			}))
		}
		chain = append(chain, mw.Timeout(rc.Timeout.D()))
		if rc.Gzip {
			chain = append(chain, mw.Gzip(gzipMinSize))
		}
		routes = append(routes, &proxy.Route{
			Name:        rc.Name,
			Host:        rc.Host,
			Prefix:      rc.PathPrefix,
			Methods:     rc.Methods,
			StripPrefix: rc.StripPrefix,
			Handler:     mw.Chain(handlers[rc.Upstream], chain...),
		})
	}

	rt.handler = mw.Chain(proxy.NewRouter(routes),
		mw.RequestID(),
		mw.Observe(g.log, g.metrics, clientIP),
		mw.Recover(g.log),
	)
	return rt, nil
}

// ServeHTTP serves health endpoints and proxies everything else. When the
// admin listener is disabled the metrics endpoint is served here as well.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rt := g.current.Load()
	switch r.URL.Path {
	case rt.cfg.Server.HealthPath:
		g.Healthz(w, r)
		return
	case rt.cfg.Server.ReadyPath:
		g.Readyz(w, r)
		return
	case rt.cfg.Admin.MetricsPath:
		if rt.cfg.Admin.Listen == "" {
			g.MetricsHandler().ServeHTTP(w, r)
			return
		}
	}
	rt.handler.ServeHTTP(w, r)
}

// AdminHandler serves /metrics, /healthz and /readyz on the admin listener.
func (g *Gateway) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	cfg := g.Config()
	mux.Handle(cfg.Admin.MetricsPath, g.MetricsHandler())
	mux.HandleFunc(cfg.Server.HealthPath, g.Healthz)
	mux.HandleFunc(cfg.Server.ReadyPath, g.Readyz)
	return mux
}

// MetricsHandler exposes the Prometheus registry.
func (g *Gateway) MetricsHandler() http.Handler {
	return promhttp.HandlerFor(g.metrics.Registry, promhttp.HandlerOpts{Registry: g.metrics.Registry})
}

// Healthz is the liveness probe: the process is up and serving.
func (g *Gateway) Healthz(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type poolStatus struct {
	Healthy int `json:"healthy"`
	Total   int `json:"total"`
}

// Readyz is the readiness probe: every upstream has at least one healthy
// target and the gateway is not draining for shutdown.
func (g *Gateway) Readyz(w http.ResponseWriter, _ *http.Request) {
	rt := g.current.Load()
	pools := make(map[string]poolStatus, len(rt.pools))
	ready := !g.draining.Load()
	for name, p := range rt.pools {
		st := poolStatus{Healthy: p.HealthyCount(), Total: len(p.Targets)}
		if st.Healthy == 0 {
			ready = false
		}
		pools[name] = st
	}
	status, code := "ready", http.StatusOK
	switch {
	case g.draining.Load():
		status, code = "draining", http.StatusServiceUnavailable
	case !ready:
		status, code = "degraded", http.StatusServiceUnavailable
	}
	httpx.WriteJSON(w, code, map[string]any{"status": status, "upstreams": pools})
}

// Pool returns a pool of the active runtime (tests and diagnostics).
func (g *Gateway) Pool(name string) *upstream.Pool { return g.current.Load().pools[name] }

func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
