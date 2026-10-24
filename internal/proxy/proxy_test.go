package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/AkibUllah/go-api-gateway/internal/config"
	"github.com/AkibUllah/go-api-gateway/internal/httpx"
	"github.com/AkibUllah/go-api-gateway/internal/metrics"
	"github.com/AkibUllah/go-api-gateway/internal/middleware"
	"github.com/AkibUllah/go-api-gateway/internal/upstream"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func named(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, name) })
}

func TestRouterSpecificity(t *testing.T) {
	rt := NewRouter([]*Route{
		{Name: "root", Prefix: "/", Handler: named("root")},
		{Name: "api", Prefix: "/api", Handler: named("api")},
		{Name: "users", Prefix: "/api/users/", Handler: named("users")},
		{Name: "wild", Host: "*.dokan.com.bd", Prefix: "/api", Handler: named("wild")},
		{Name: "seller", Host: "Seller.Dokan.com.bd", Prefix: "/", Handler: named("seller")},
	})
	cases := []struct{ host, path, want string }{
		{"gw.local", "/", "root"},
		{"gw.local", "/api", "api"},
		{"gw.local", "/api/", "api"},
		{"gw.local", "/api/users", "users"},
		{"gw.local", "/api/users/42", "users"},
		{"gw.local", "/api/usersettings", "api"}, // segment boundary
		{"gw.local", "/apix", "root"},
		{"shop.dokan.com.bd", "/api/x", "wild"},
		{"shop.dokan.com.bd:8080", "/other", "root"},
		{"seller.dokan.com.bd", "/api/x", "seller"}, // exact host beats wildcard host
		{"dokan.com.bd", "/api", "api"},             // wildcard needs a subdomain
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", c.path, nil)
		r.Host = c.host
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, r)
		if rec.Body.String() != c.want {
			t.Errorf("%s%s -> %q, want %q", c.host, c.path, rec.Body.String(), c.want)
		}
	}
}

func TestRouterMethodsAndNotFound(t *testing.T) {
	rt := NewRouter([]*Route{
		{Name: "read", Prefix: "/items", Methods: []string{"GET", "HEAD"}, Handler: named("read")},
		{Name: "write", Prefix: "/items", Methods: []string{"POST"}, Handler: named("write")},
	})
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest("POST", "/items", nil))
	if rec.Body.String() != "write" {
		t.Fatalf("POST went to %q", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest("DELETE", "/items/1", nil))
	if rec.Code != 405 || rec.Header().Get("Allow") != "GET, HEAD, POST" {
		t.Fatalf("DELETE: %d Allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
	rec = httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest("GET", "/nope", nil))
	if rec.Code != 404 {
		t.Fatalf("unmatched path: %d", rec.Code)
	}
}

func TestStripPrefix(t *testing.T) {
	var gotPath, gotRaw string
	h := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { gotPath, gotRaw = r.URL.Path, r.URL.RawPath })
	rt := NewRouter([]*Route{{Name: "x", Prefix: "/api/catalog", StripPrefix: true, Handler: h}})
	for _, c := range [][2]string{
		{"/api/catalog", "/"},
		{"/api/catalog/", "/"},
		{"/api/catalog/products/7", "/products/7"},
		{"/api/catalog/a%2Fb?x=1", "/a/b"},
	} {
		rt.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", c[0], nil))
		if gotPath != c[1] {
			t.Errorf("%s stripped to %q, want %q", c[0], gotPath, c[1])
		}
	}
	if gotRaw != "/a%2Fb" {
		t.Errorf("escaped path not preserved: %q", gotRaw)
	}
}

// backend is an httptest upstream that records hits and can be told to fail.
type backend struct {
	*httptest.Server
	hits   atomic.Int32
	status atomic.Int32
	delay  atomic.Int64
}

func newBackend(t *testing.T, name string) *backend {
	b := &backend{}
	b.status.Store(200)
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.hits.Add(1)
		if d := time.Duration(b.delay.Load()); d > 0 {
			select {
			case <-time.After(d):
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(b.status.Load()))
		_ = json.NewEncoder(w).Encode(map[string]string{
			"backend": name, "path": r.URL.Path, "query": r.URL.RawQuery, "host": r.Host,
			"xff": r.Header.Get("X-Forwarded-For"), "xfh": r.Header.Get("X-Forwarded-Host"),
		})
	}))
	t.Cleanup(b.Close)
	return b
}

type harness struct {
	handler http.Handler
	pool    *upstream.Pool
	metrics *metrics.Metrics
}

func newHarness(t *testing.T, ucfg config.Upstream, targets ...string) *harness {
	t.Helper()
	ucfg.Targets = targets
	if ucfg.Balancer == "" {
		ucfg.Balancer = "round_robin"
	}
	if ucfg.Retries.Attempts > 0 && len(ucfg.Retries.OnStatus) == 0 {
		ucfg.Retries.OnStatus = []int{502, 503, 504}
	}
	m := metrics.New()
	p, err := upstream.NewPool("svc", ucfg, m, nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := NewTransport(time.Second, 8)
	t.Cleanup(tr.CloseIdleConnections)
	h := New(Options{Pool: p, Transport: tr, Metrics: m, Log: discard,
		TrustedPeer: func(r *http.Request) bool { return strings.HasPrefix(r.RemoteAddr, "10.") }})
	// Attach an Info like the Observe middleware does.
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(httpx.WithInfo(r.Context(), &httpx.Info{})))
	})
	return &harness{handler: wrapped, pool: p, metrics: m}
}

func (h *harness) do(t *testing.T, method, path string, body io.Reader) (*httptest.ResponseRecorder, map[string]string) {
	t.Helper()
	r := httptest.NewRequest(method, path, body)
	r.RemoteAddr = "203.0.113.50:41000"
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, r)
	var m map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return rec, m
}

func TestProxyForwardsPathQueryAndForwardedHeaders(t *testing.T) {
	b := newBackend(t, "a")
	h := newHarness(t, config.Upstream{}, b.URL+"/v1")
	rec, got := h.do(t, "GET", "/products/7?lang=bn", nil)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	host := strings.TrimPrefix(b.URL, "http://")
	if got["path"] != "/v1/products/7" || got["query"] != "lang=bn" || got["host"] != host {
		t.Fatalf("upstream saw %v", got)
	}
	if got["xff"] != "203.0.113.50" || got["xfh"] != "example.com" {
		t.Fatalf("forwarded headers %v", got)
	}
}

func TestProxyDropsUntrustedForwardedFor(t *testing.T) {
	b := newBackend(t, "a")
	h := newHarness(t, config.Upstream{}, b.URL)
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.50:41000"
	r.Header.Set("X-Forwarded-For", "6.6.6.6")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, r)
	var got map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["xff"] != "203.0.113.50" {
		t.Fatalf("spoofed XFF forwarded: %q", got["xff"])
	}

	r = httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.1.2.3:41000" // trusted proxy
	r.Header.Set("X-Forwarded-For", "198.51.100.4")
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, r)
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["xff"] != "198.51.100.4, 10.1.2.3" {
		t.Fatalf("trusted chain not preserved: %q", got["xff"])
	}
}

func TestRetriesIdempotentRequestOnAnotherTarget(t *testing.T) {
	bad, good := newBackend(t, "bad"), newBackend(t, "good")
	bad.status.Store(503)
	h := newHarness(t, config.Upstream{Retries: config.Retries{Attempts: 1}}, bad.URL, good.URL)
	for i := 0; i < 4; i++ {
		rec, got := h.do(t, "GET", "/", nil)
		if rec.Code != 200 || got["backend"] != "good" {
			t.Fatalf("request %d: %d from %s", i, rec.Code, got["backend"])
		}
	}
	// Round-robin lands on "bad" first for every request here (the retry's
	// pick advances the counter too), so each request costs exactly one retry.
	if bad.hits.Load() != 4 || good.hits.Load() != 4 {
		t.Fatalf("hits bad=%d good=%d, want 4/4", bad.hits.Load(), good.hits.Load())
	}
	if got := testutil.ToFloat64(h.metrics.Retries.WithLabelValues("svc")); got != 4 {
		t.Fatalf("retry counter = %v", got)
	}
}

func TestDoesNotRetryRequestsWithBodies(t *testing.T) {
	bad, good := newBackend(t, "bad"), newBackend(t, "good")
	bad.status.Store(503)
	h := newHarness(t, config.Upstream{Retries: config.Retries{Attempts: 2}}, bad.URL, good.URL)
	codes := map[int]int{}
	for i := 0; i < 4; i++ {
		rec, _ := h.do(t, "POST", "/orders", strings.NewReader(`{"sku":"rice-5kg"}`))
		codes[rec.Code]++
	}
	if codes[503] != 2 || codes[200] != 2 {
		t.Fatalf("POST outcomes %v: a 503 must be returned as-is, not retried", codes)
	}
}

func TestRetriesConnectionErrors(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := "http://" + ln.Addr().String()
	ln.Close() // connection refused from now on
	good := newBackend(t, "good")
	h := newHarness(t, config.Upstream{Retries: config.Retries{Attempts: 1}}, dead, good.URL)
	for i := 0; i < 4; i++ {
		if rec, _ := h.do(t, "GET", "/", nil); rec.Code != 200 {
			t.Fatalf("request %d: %d", i, rec.Code)
		}
	}
	if got := testutil.ToFloat64(h.metrics.UpstreamRequests.WithLabelValues("svc", ln.Addr().String(), "error")); got != 4 {
		t.Fatalf("error outcome counter = %v", got)
	}
}

func TestReturnsLastResponseWhenRetriesExhausted(t *testing.T) {
	a, b := newBackend(t, "a"), newBackend(t, "b")
	a.status.Store(502)
	b.status.Store(503)
	h := newHarness(t, config.Upstream{Retries: config.Retries{Attempts: 3}}, a.URL, b.URL)
	rec, got := h.do(t, "GET", "/", nil)
	if rec.Code != 503 || got["backend"] != "b" {
		t.Fatalf("got %d from %s, want the second target's 503", rec.Code, got["backend"])
	}
	if a.hits.Load()+b.hits.Load() != 2 {
		t.Fatal("each target should be tried at most once per request")
	}
}

func TestBadGatewayAndNoHealthyTarget(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := "http://" + ln.Addr().String()
	ln.Close()
	h := newHarness(t, config.Upstream{}, dead)
	rec, _ := h.do(t, "GET", "/", nil)
	if rec.Code != 502 {
		t.Fatalf("dead upstream: %d", rec.Code)
	}

	b := newBackend(t, "a")
	h = newHarness(t, config.Upstream{HealthCheck: config.HealthCheck{Path: "/health"}}, b.URL)
	markUnhealthy(t, h.pool)
	rec, _ = h.do(t, "GET", "/", nil)
	var body httpx.ErrorBody
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != 503 || body.Error.Code != "no_healthy_upstream" {
		t.Fatalf("no healthy target: %d %s", rec.Code, rec.Body)
	}
}

// markUnhealthy drives the real health checker against a failing endpoint.
func markUnhealthy(t *testing.T, p *upstream.Pool) {
	t.Helper()
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	defer failing.Close()
	client := &http.Client{Transport: rewriteTo(failing.URL)}
	p.Config.HealthCheck.Timeout = config.Duration(time.Second)
	p.Config.HealthCheck.UnhealthyThreshold = 1
	p.CheckOnce(context.Background(), client, p.Targets[0], nil)
	if p.Targets[0].Healthy() {
		t.Fatal("target still healthy")
	}
}

type rewriteTo string

func (u rewriteTo) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Host = strings.TrimPrefix(string(u), "http://")
	return http.DefaultTransport.RoundTrip(r)
}

func TestRouteTimeoutReturns504(t *testing.T) {
	b := newBackend(t, "slow")
	b.delay.Store(int64(500 * time.Millisecond))
	h := newHarness(t, config.Upstream{}, b.URL)
	handler := middleware.Timeout(30 * time.Millisecond)(h.handler)
	r := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	start := time.Now()
	handler.ServeHTTP(rec, r)
	if rec.Code != 504 {
		t.Fatalf("status %d, want 504", rec.Code)
	}
	if time.Since(start) > 400*time.Millisecond {
		t.Fatal("timeout did not cut the request short")
	}
}

func TestCircuitOpensAndShedsLoad(t *testing.T) {
	b := newBackend(t, "flaky")
	b.status.Store(500)
	h := newHarness(t, config.Upstream{CircuitBreaker: config.CircuitBreaker{
		FailureThreshold: 3, OpenTimeout: config.Duration(time.Hour), HalfOpenRequests: 1,
	}}, b.URL)
	for i := 0; i < 3; i++ {
		if rec, _ := h.do(t, "GET", "/", nil); rec.Code != 500 {
			t.Fatalf("request %d: %d", i, rec.Code)
		}
	}
	for i := 0; i < 5; i++ {
		if rec, _ := h.do(t, "GET", "/", nil); rec.Code != 503 {
			t.Fatalf("open circuit returned %d", rec.Code)
		}
	}
	if b.hits.Load() != 3 {
		t.Fatalf("upstream received %d requests, the open circuit should have shed the last 5", b.hits.Load())
	}
}

func TestClientCancelDoesNotTripBreaker(t *testing.T) {
	b := newBackend(t, "slow")
	b.delay.Store(int64(time.Second))
	h := newHarness(t, config.Upstream{CircuitBreaker: config.CircuitBreaker{
		FailureThreshold: 1, OpenTimeout: config.Duration(time.Hour),
	}}, b.URL)
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
	time.AfterFunc(20*time.Millisecond, cancel)
	h.handler.ServeHTTP(httptest.NewRecorder(), r)
	if st := h.pool.Targets[0].Breaker().State().String(); st != "closed" {
		t.Fatalf("breaker %s after a client cancel", st)
	}
}

func TestLeastConnCountsInFlightUntilBodyClosed(t *testing.T) {
	b := newBackend(t, "a")
	h := newHarness(t, config.Upstream{Balancer: "least_conn"}, b.URL)
	h.do(t, "GET", "/", nil)
	if n := h.pool.Targets[0].Active(); n != 0 {
		t.Fatalf("active = %d after the response completed", n)
	}
}

func BenchmarkRouterMatch(b *testing.B) {
	var routes []*Route
	for _, p := range []string{"/api/catalog", "/api/orders", "/api/users", "/api/payments/bkash", "/api/payments",
		"/partner", "/static", "/api/search", "/api/cart", "/"} {
		routes = append(routes, &Route{Name: p, Prefix: p, Handler: named(p)})
	}
	rt := NewRouter(routes)
	r := httptest.NewRequest("GET", "/api/payments/bkash/callback", nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if route, _ := rt.Match(r); route == nil {
			b.Fatal("no match")
		}
	}
}

func BenchmarkProxyRoundTrip(b *testing.B) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstreamSrv.Close()
	p, _ := upstream.NewPool("bench", config.Upstream{Targets: []string{upstreamSrv.URL}, Balancer: "round_robin"}, nil, nil)
	tr := NewTransport(time.Second, 256)
	defer tr.CloseIdleConnections()
	gw := httptest.NewServer(New(Options{Pool: p, Transport: tr, Log: discard}))
	defer gw.Close()
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 256}}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			resp, err := client.Get(gw.URL + "/products")
			if err != nil {
				b.Error(err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	})
}
