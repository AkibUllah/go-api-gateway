package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/AkibUllah/go-api-gateway/internal/auth"
	"github.com/AkibUllah/go-api-gateway/internal/config"
	"github.com/AkibUllah/go-api-gateway/internal/echo"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

const secret = "gateway-test-secret-0123456789abcdef"

func echoServer(t testing.TB, service string) *httptest.Server {
	s := httptest.NewServer(echo.Handler(service, service))
	t.Cleanup(s.Close)
	return s
}

func testConfig(t testing.TB, catalog, orders string, extra string) *config.Config {
	t.Helper()
	yaml := fmt.Sprintf(`
admin:
  listen: ""
upstreams:
  catalog:
    targets: [%q]
  orders:
    targets: [%q]
jwt:
  customers:
    secret: %q
    forward_claims: {sub: X-User-ID}
api_keys:
  partners:
    keys: [{name: swift, key: partner-key}]
rate_limits:
  tight: {rate: 1, period: 1m, burst: 2}
routes:
  - name: catalog
    path_prefix: /api/catalog
    strip_prefix: true
    upstream: catalog
    rate_limit: tight
  - name: orders
    path_prefix: /api/orders
    strip_prefix: true
    upstream: orders
    jwt: customers
  - name: partner
    path_prefix: /partner
    upstream: orders
    api_keys: partners
%s`, catalog, orders, secret, extra)
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func newGateway(t testing.TB, cfg *config.Config) *Gateway {
	t.Helper()
	g, err := New(cfg, discard, nil)
	if err != nil {
		t.Fatal(err)
	}
	g.RetireGrace = 0
	t.Cleanup(g.Close)
	return g
}

func get(t *testing.T, h http.Handler, path string, hdr ...string) (*httptest.ResponseRecorder, echo.Response) {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	var body echo.Response
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec, body
}

func TestEndToEndRouting(t *testing.T) {
	cat, ord := echoServer(t, "catalog"), echoServer(t, "orders")
	g := newGateway(t, testConfig(t, cat.URL, ord.URL, ""))

	rec, body := get(t, g, "/api/catalog/products/42")
	if rec.Code != 200 || body.Service != "catalog" || body.Path != "/products/42" {
		t.Fatalf("catalog: %d %+v", rec.Code, body)
	}
	if rec.Header().Get("X-Request-ID") == "" || body.Headers["X-Request-Id"] != rec.Header().Get("X-Request-ID") {
		t.Fatal("request id not propagated to upstream and client")
	}
	if rec, _ := get(t, g, "/nothing-here"); rec.Code != 404 {
		t.Fatalf("unknown route: %d", rec.Code)
	}
}

func TestEndToEndAuth(t *testing.T) {
	cat, ord := echoServer(t, "catalog"), echoServer(t, "orders")
	g := newGateway(t, testConfig(t, cat.URL, ord.URL, ""))

	if rec, _ := get(t, g, "/api/orders/mine"); rec.Code != 401 {
		t.Fatalf("orders without token: %d", rec.Code)
	}
	tok, _ := auth.SignHS256(secret, jwt.MapClaims{"sub": "customer-42"}, time.Hour)
	rec, body := get(t, g, "/api/orders/mine", "Authorization", "Bearer "+tok, "X-User-ID", "admin")
	if rec.Code != 200 || body.Headers["X-User-Id"] != "customer-42" || body.Headers["X-Consumer-Id"] != "jwt:customer-42" {
		t.Fatalf("orders with token: %d %v", rec.Code, body.Headers)
	}

	rec, body = get(t, g, "/partner/delivery", "X-API-Key", "partner-key")
	if rec.Code != 200 || body.Headers["X-Api-Key"] != "" || body.Headers["X-Consumer-Id"] != "key:swift" {
		t.Fatalf("partner: %d %v", rec.Code, body.Headers)
	}
}

func TestEndToEndRateLimit(t *testing.T) {
	cat, ord := echoServer(t, "catalog"), echoServer(t, "orders")
	g := newGateway(t, testConfig(t, cat.URL, ord.URL, ""))
	codes := []int{}
	for i := 0; i < 3; i++ {
		rec, _ := get(t, g, "/api/catalog/")
		codes = append(codes, rec.Code)
	}
	if fmt.Sprint(codes) != "[200 200 429]" {
		t.Fatalf("codes = %v", codes)
	}
}

func TestHealthReadyAndMetricsEndpoints(t *testing.T) {
	cat, ord := echoServer(t, "catalog"), echoServer(t, "orders")
	g := newGateway(t, testConfig(t, cat.URL, ord.URL, ""))

	if rec, _ := get(t, g, "/healthz"); rec.Code != 200 {
		t.Fatalf("healthz %d", rec.Code)
	}
	rec, _ := get(t, g, "/readyz")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"ready"`) {
		t.Fatalf("readyz %d %s", rec.Code, rec.Body)
	}
	get(t, g, "/api/catalog/x")
	rec, _ = get(t, g, "/metrics")
	for _, want := range []string{
		`gateway_requests_total{code="200",method="GET",route="catalog"} 1`,
		`gateway_upstream_target_healthy{target=`,
		`gateway_request_duration_seconds_bucket`,
		`go_goroutines`,
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("metrics missing %q", want)
		}
	}

	g.SetDraining(true)
	rec, _ = get(t, g, "/readyz")
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "draining") {
		t.Fatalf("draining readyz %d %s", rec.Code, rec.Body)
	}
}

func TestReadyzDegradedWhenAPoolHasNoHealthyTarget(t *testing.T) {
	cat := echoServer(t, "catalog")
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := "http://" + ln.Addr().String()
	ln.Close()
	cfg := testConfig(t, cat.URL, dead, "")
	u := cfg.Upstreams["orders"]
	u.HealthCheck = config.HealthCheck{
		Path: "/health", Interval: config.Duration(10 * time.Millisecond), Timeout: config.Duration(100 * time.Millisecond),
		HealthyThreshold: 1, UnhealthyThreshold: 1,
	}
	cfg.Upstreams["orders"] = u
	g := newGateway(t, cfg)

	deadline := time.Now().Add(3 * time.Second)
	for g.Pool("orders").HealthyCount() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	rec, _ := get(t, g, "/readyz")
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), `"degraded"`) {
		t.Fatalf("readyz %d %s", rec.Code, rec.Body)
	}
}

func TestReloadSwapsRoutesAtomically(t *testing.T) {
	cat, ord := echoServer(t, "catalog"), echoServer(t, "orders")
	g := newGateway(t, testConfig(t, cat.URL, ord.URL, ""))
	if rec, _ := get(t, g, "/api/v2/catalog/x"); rec.Code != 404 {
		t.Fatalf("route exists before reload: %d", rec.Code)
	}
	next := testConfig(t, cat.URL, ord.URL, `  - name: catalog-v2
    path_prefix: /api/v2/catalog
    strip_prefix: true
    upstream: catalog
`)
	if err := g.Reload(next); err != nil {
		t.Fatal(err)
	}
	rec, body := get(t, g, "/api/v2/catalog/x")
	if rec.Code != 200 || body.Path != "/x" {
		t.Fatalf("new route after reload: %d %+v", rec.Code, body)
	}
	if g.Config() != next {
		t.Fatal("Config() does not return the reloaded config")
	}
}

func TestServeReloadsAndShutsDownGracefully(t *testing.T) {
	cat, ord := echoServer(t, "catalog"), echoServer(t, "orders")
	cfg := testConfig(t, cat.URL, ord.URL, "")
	g := newGateway(t, cfg)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + ln.Addr().String()
	reload := make(chan struct{})
	loads := 0
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- g.Serve(ctx, ServeOptions{
			Public: ln,
			Reload: reload,
			Load: func() (*config.Config, error) {
				loads++
				if loads == 1 {
					return nil, fmt.Errorf("broken yaml")
				}
				return testConfig(t, cat.URL, ord.URL, `  - name: late
    path_prefix: /late
    upstream: catalog
`), nil
			},
		})
	}()

	waitFor(t, func() bool { resp, err := http.Get(base + "/healthz"); return err == nil && resp.Body.Close() == nil })
	reload <- struct{}{} // fails: old config must keep serving
	if resp, _ := http.Get(base + "/api/catalog/ok"); resp == nil || resp.StatusCode != 200 {
		t.Fatal("gateway stopped serving after a failed reload")
	}
	reload <- struct{}{}
	waitFor(t, func() bool { resp, err := http.Get(base + "/late"); return err == nil && resp.StatusCode == 200 })

	// Start a slow request, then shut down: it must complete.
	slow := make(chan int, 1)
	go func() {
		resp, err := http.Get(base + "/api/catalog/slow?delay=300ms")
		if err != nil {
			slow <- -1
			return
		}
		resp.Body.Close()
		slow <- resp.StatusCode
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if code := <-slow; code != 200 {
		t.Fatalf("in-flight request got %d during shutdown", code)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := http.Get(base + "/healthz"); err == nil {
		t.Fatal("listener still accepting after shutdown")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 3s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestNewLoggerLevels(t *testing.T) {
	var buf strings.Builder
	lv := new(slog.LevelVar)
	log := NewLogger(config.Logging{Level: "warn", Format: "text"}, &buf, lv)
	log.Info("hidden")
	log.Warn("shown")
	SetLevel(lv, "debug")
	log.Debug("now shown")
	out := buf.String()
	if strings.Contains(out, "hidden") || !strings.Contains(out, "shown") || !strings.Contains(out, "now shown") {
		t.Fatalf("log output:\n%s", out)
	}
}

// BenchmarkGatewayEndToEnd measures the full stack over real TCP: client ->
// gateway (request id, access log, metrics, rate limit, timeout, proxy) -> echo upstream.
func BenchmarkGatewayEndToEnd(b *testing.B) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer up.Close()
	cfg := testConfig(b, up.URL, up.URL, "")
	r := cfg.RateLimits["tight"]
	r.Rate, r.Burst = 1<<30, 1<<30
	cfg.RateLimits["tight"] = r
	g, err := New(cfg, discard, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer g.Close()
	srv := httptest.NewServer(g)
	defer srv.Close()
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 256}}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			resp, err := client.Get(srv.URL + "/api/catalog/products")
			if err != nil {
				b.Error(err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				b.Errorf("status %d", resp.StatusCode)
				return
			}
		}
	})
}
