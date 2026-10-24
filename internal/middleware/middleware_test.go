package middleware

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/AkibUllah/go-api-gateway/internal/auth"
	"github.com/AkibUllah/go-api-gateway/internal/config"
	"github.com/AkibUllah/go-api-gateway/internal/httpx"
	"github.com/AkibUllah/go-api-gateway/internal/metrics"
	"github.com/AkibUllah/go-api-gateway/internal/ratelimit"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func ok(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }

func serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body httpx.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body is not JSON: %q", rec.Body.String())
	}
	return body.Error.Code
}

func TestChainOrder(t *testing.T) {
	var order []string
	mk := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}
	serve(Chain(http.HandlerFunc(ok), mk("a"), nil, mk("b"), mk("c")), httptest.NewRequest("GET", "/", nil))
	if strings.Join(order, "") != "abc" {
		t.Fatalf("order = %v", order)
	}
}

func TestRequestID(t *testing.T) {
	var seen string
	h := RequestID()(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = httpx.RequestID(r.Context())
		if r.Header.Get(RequestIDHeader) != seen {
			t.Error("request header not set for upstream")
		}
	}))

	rec := serve(h, httptest.NewRequest("GET", "/", nil))
	if len(seen) != 32 || rec.Header().Get(RequestIDHeader) != seen {
		t.Fatalf("generated id %q, response header %q", seen, rec.Header().Get(RequestIDHeader))
	}

	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set(RequestIDHeader, "edge-lb-123.abc")
	serve(h, r)
	if seen != "edge-lb-123.abc" {
		t.Fatalf("valid incoming id not kept: %q", seen)
	}

	r.Header.Set(RequestIDHeader, "bad id\r\nX-Injected: 1")
	serve(h, r)
	if strings.ContainsAny(seen, " \r\n") || len(seen) != 32 {
		t.Fatalf("unsafe incoming id not replaced: %q", seen)
	}
}

func TestRecoverReturnsJSON500(t *testing.T) {
	h := RequestID()(Recover(discard)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })))
	rec := serve(h, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 500 || errorCode(t, rec) != "internal_error" {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestObserveRecordsMetricsAndLogs(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	m := metrics.New()
	h := Observe(log, m, func(*http.Request) string { return "203.0.113.9" })(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			info := httpx.GetInfo(r.Context())
			info.Route, info.Target, info.Consumer, info.Attempts = "orders", "10.0.0.5:9003", "jwt:c-1", 2
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "created")
		}))
	serve(h, httptest.NewRequest("POST", "/api/orders", nil))

	if got := testutil.ToFloat64(m.Requests.WithLabelValues("orders", "POST", "201")); got != 1 {
		t.Fatalf("request counter = %v", got)
	}
	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("log line not JSON: %q", buf.String())
	}
	for k, want := range map[string]any{
		"route": "orders", "status": float64(201), "bytes": float64(7), "upstream": "10.0.0.5:9003",
		"consumer": "jwt:c-1", "client_ip": "203.0.113.9", "attempts": float64(2),
	} {
		if line[k] != want {
			t.Errorf("log %s = %v, want %v", k, line[k], want)
		}
	}
}

func TestBodyLimit(t *testing.T) {
	var readErr error
	h := BodyLimit(10)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, readErr = io.ReadAll(r.Body)
	}))

	rec := serve(h, httptest.NewRequest("POST", "/", strings.NewReader(strings.Repeat("x", 11))))
	if rec.Code != 413 || errorCode(t, rec) != "body_too_large" {
		t.Fatalf("declared oversize body: %d", rec.Code)
	}

	r := httptest.NewRequest("POST", "/", io.NopCloser(strings.NewReader(strings.Repeat("x", 50))))
	r.ContentLength = -1 // chunked
	serve(h, r)
	var mbe *http.MaxBytesError
	if !errors.As(readErr, &mbe) {
		t.Fatalf("chunked oversize body read err = %v, want MaxBytesError", readErr)
	}

	serve(h, httptest.NewRequest("POST", "/", strings.NewReader("small")))
	if readErr != nil {
		t.Fatalf("small body rejected: %v", readErr)
	}
}

func corsPolicy() config.CORSPolicy {
	return config.CORSPolicy{
		AllowedOrigins:   []string{"https://dokan.com.bd", "https://*.dokan.com.bd"},
		AllowedMethods:   []string{"GET", "POST"},
		AllowedHeaders:   []string{"Authorization", "Content-Type"},
		ExposedHeaders:   []string{"X-Request-ID"},
		AllowCredentials: true,
		MaxAge:           config.Duration(10 * time.Minute),
	}
}

func TestCORSPreflight(t *testing.T) {
	called := false
	h := CORS(corsPolicy())(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	r := httptest.NewRequest("OPTIONS", "/api", nil)
	r.Header.Set("Origin", "https://seller.dokan.com.bd")
	r.Header.Set("Access-Control-Request-Method", "POST")
	rec := serve(h, r)
	if called {
		t.Fatal("preflight reached the upstream")
	}
	hd := rec.Header()
	if rec.Code != 204 || hd.Get("Access-Control-Allow-Origin") != "https://seller.dokan.com.bd" ||
		hd.Get("Access-Control-Allow-Credentials") != "true" || hd.Get("Access-Control-Max-Age") != "600" ||
		hd.Get("Access-Control-Allow-Methods") != "GET, POST" {
		t.Fatalf("preflight response %d %v", rec.Code, hd)
	}
}

func TestCORSOrigins(t *testing.T) {
	h := CORS(corsPolicy())(http.HandlerFunc(ok))
	cases := map[string]bool{
		"https://dokan.com.bd":         true,
		"https://api.dokan.com.bd":     true,
		"https://evil-dokan.com.bd":    false,
		"http://api.dokan.com.bd":      false,
		"https://dokan.com.bd.evil.io": false,
	}
	for origin, allowed := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Origin", origin)
		rec := serve(h, r)
		got := rec.Header().Get("Access-Control-Allow-Origin") == origin
		if got != allowed {
			t.Errorf("origin %s allowed=%v, want %v", origin, got, allowed)
		}
		if rec.Body.String() != "ok" {
			t.Errorf("origin %s: simple request not passed through", origin)
		}
		if allowed && rec.Header().Get("Access-Control-Expose-Headers") != "X-Request-ID" {
			t.Errorf("origin %s: exposed headers missing", origin)
		}
	}

	star := CORS(config.CORSPolicy{AllowedOrigins: []string{"*"}})(http.HandlerFunc(ok))
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Origin", "https://anything.example")
	if got := serve(star, r).Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("wildcard without credentials should answer *, got %q", got)
	}
}

func gunzip(t *testing.T, b []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestGzipCompressesLargeCompressibleBodies(t *testing.T) {
	payload := `{"items":"` + strings.Repeat("shari-", 500) + `"}`
	h := Gzip(1024)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "99999") // upstream length must be dropped
		// Write in small pieces to exercise the buffering path.
		for i := 0; i < len(payload); i += 100 {
			_, _ = io.WriteString(w, payload[i:min(i+100, len(payload))])
		}
	}))
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Accept-Encoding", "br, gzip;q=0.8")
	rec := serve(h, r)
	if rec.Header().Get("Content-Encoding") != "gzip" || rec.Header().Get("Content-Length") != "" {
		t.Fatalf("headers = %v", rec.Header())
	}
	if !strings.Contains(rec.Header().Get("Vary"), "Accept-Encoding") {
		t.Fatal("Vary: Accept-Encoding missing")
	}
	if got := gunzip(t, rec.Body.Bytes()); got != payload {
		t.Fatal("round trip mismatch")
	}
	if rec.Body.Len() >= len(payload)/4 {
		t.Fatalf("compressed %d -> %d bytes", len(payload), rec.Body.Len())
	}
}

func TestGzipSkips(t *testing.T) {
	big := strings.Repeat("a", 4096)
	cases := []struct {
		name, accept, ctype, encoding, body string
	}{
		{"small body", "gzip", "application/json", "", `{"ok":true}`},
		{"client refuses", "gzip;q=0", "application/json", "", big},
		{"no accept", "", "application/json", "", big},
		{"already encoded", "gzip", "application/json", "br", big},
		{"binary type", "gzip", "image/png", "", big},
		{"event stream", "gzip", "text/event-stream", "", big},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := Gzip(1024)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", c.ctype)
				if c.encoding != "" {
					w.Header().Set("Content-Encoding", c.encoding)
				}
				_, _ = io.WriteString(w, c.body)
			}))
			r := httptest.NewRequest("GET", "/", nil)
			if c.accept != "" {
				r.Header.Set("Accept-Encoding", c.accept)
			}
			rec := serve(h, r)
			if rec.Header().Get("Content-Encoding") == "gzip" || rec.Body.String() != c.body {
				t.Fatalf("body modified: encoding=%q len=%d", rec.Header().Get("Content-Encoding"), rec.Body.Len())
			}
		})
	}
}

func TestGzipPreservesStatusAndEmptyBodies(t *testing.T) {
	h := Gzip(10)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
	}))
	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	rec := serve(h, r)
	if rec.Code != http.StatusAccepted || rec.Body.Len() != 0 || rec.Header().Get("Content-Encoding") != "" {
		t.Fatalf("got %d len=%d enc=%q", rec.Code, rec.Body.Len(), rec.Header().Get("Content-Encoding"))
	}
}

func TestRateLimitMiddleware(t *testing.T) {
	m := metrics.New()
	lim := ratelimit.NewMemory(ratelimit.Rule{Rate: 1, Period: time.Minute, Burst: 2}, nil)
	h := RateLimit(lim, RateLimitOptions{
		Route: "catalog", Policy: "per_ip", Key: "ip", Metrics: m, Log: discard,
		ClientIP: func(r *http.Request) string { return r.Header.Get("X-Test-IP") },
	})(http.HandlerFunc(ok))

	req := func(ip string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("X-Test-IP", ip)
		return serve(h, r)
	}
	if rec := req("1.1.1.1"); rec.Code != 200 || rec.Header().Get("RateLimit-Remaining") != "1" || rec.Header().Get("RateLimit-Limit") != "2" {
		t.Fatalf("first request: %d %v", rec.Code, rec.Header())
	}
	req("1.1.1.1")
	rec := req("1.1.1.1")
	if rec.Code != 429 || errorCode(t, rec) != "rate_limited" || rec.Header().Get("Retry-After") != "60" {
		t.Fatalf("third request: %d retry-after=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if rec := req("2.2.2.2"); rec.Code != 200 {
		t.Fatal("other client limited")
	}
	if got := testutil.ToFloat64(m.RateLimited.WithLabelValues("catalog", "per_ip")); got != 1 {
		t.Fatalf("rate limited counter = %v", got)
	}
}

func TestRateLimitByConsumerAndHeader(t *testing.T) {
	var keys []string
	lim := limiterFunc(func(_ context.Context, key string) (ratelimit.Decision, error) {
		keys = append(keys, key)
		return ratelimit.Decision{Allowed: true, Limit: 1}, nil
	})
	ip := func(*http.Request) string { return "9.9.9.9" }
	byConsumer := RateLimit(lim, RateLimitOptions{Route: "r", Policy: "p", Key: "consumer", ClientIP: ip, Log: discard})(http.HandlerFunc(ok))
	byHeader := RateLimit(lim, RateLimitOptions{Route: "r", Policy: "p", Key: "header:X-Tenant", ClientIP: ip, Log: discard})(http.HandlerFunc(ok))

	r := httptest.NewRequest("GET", "/", nil)
	r = r.WithContext(context.WithValue(r.Context(), principalKey{}, &auth.Principal{Consumer: "key:pathao"}))
	serve(byConsumer, r)
	r2 := httptest.NewRequest("GET", "/", nil)
	r2.Header.Set("X-Tenant", "shop-7")
	serve(byHeader, r2)
	serve(byHeader, httptest.NewRequest("GET", "/", nil)) // header missing -> IP
	want := []string{"r:p:c:key:pathao", "r:p:h:shop-7", "r:p:ip:9.9.9.9"}
	if strings.Join(keys, " ") != strings.Join(want, " ") {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
}

type limiterFunc func(context.Context, string) (ratelimit.Decision, error)

func (f limiterFunc) Allow(ctx context.Context, k string) (ratelimit.Decision, error) {
	return f(ctx, k)
}

func TestRateLimitBackendFailure(t *testing.T) {
	broken := limiterFunc(func(context.Context, string) (ratelimit.Decision, error) {
		return ratelimit.Decision{}, errors.New("redis: connection refused")
	})
	ip := func(*http.Request) string { return "1.1.1.1" }
	open := RateLimit(broken, RateLimitOptions{Key: "ip", FailOpen: true, ClientIP: ip, Log: discard})(http.HandlerFunc(ok))
	closed := RateLimit(broken, RateLimitOptions{Key: "ip", FailOpen: false, ClientIP: ip, Log: discard})(http.HandlerFunc(ok))
	if rec := serve(open, httptest.NewRequest("GET", "/", nil)); rec.Code != 200 {
		t.Fatalf("fail-open returned %d", rec.Code)
	}
	if rec := serve(closed, httptest.NewRequest("GET", "/", nil)); rec.Code != 503 {
		t.Fatalf("fail-closed returned %d", rec.Code)
	}
}

const jwtSecret = "middleware-test-secret-0123456789abcdef"

func TestJWTMiddleware(t *testing.T) {
	v, err := auth.NewJWTVerifier(config.JWTProvider{
		Algorithms: []string{"HS256"}, Secret: jwtSecret,
		ForwardClaims: map[string]string{"sub": "X-User-ID", "role": "X-User-Role"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := metrics.New()
	var got http.Header
	var principal *auth.Principal
	h := JWT(v, "orders", m, discard)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, principal = r.Header.Clone(), PrincipalFrom(r.Context())
	}))

	tok, _ := auth.SignHS256(jwtSecret, jwt.MapClaims{"sub": "customer-42"}, time.Hour)
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	r.Header.Set("X-User-Role", "admin") // spoof attempt; token has no role claim
	r.Header.Set(ConsumerHeader, "key:someone-else")
	if rec := serve(h, r); rec.Code != 200 {
		t.Fatalf("valid token rejected: %d", rec.Code)
	}
	if got.Get("X-User-ID") != "customer-42" || got.Get("X-User-Role") != "" || got.Get(ConsumerHeader) != "jwt:customer-42" {
		t.Fatalf("forwarded headers = %v", got)
	}
	if principal == nil || principal.Subject != "customer-42" {
		t.Fatalf("principal = %+v", principal)
	}

	for _, authz := range []string{"", "Bearer nope", "Basic dXNlcjpwYXNz"} {
		r := httptest.NewRequest("GET", "/", nil)
		if authz != "" {
			r.Header.Set("Authorization", authz)
		}
		rec := serve(h, r)
		if rec.Code != 401 || !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Bearer") {
			t.Fatalf("authz %q: %d %v", authz, rec.Code, rec.Header())
		}
	}
	if got := testutil.ToFloat64(m.AuthFailures.WithLabelValues("orders", "jwt")); got != 3 {
		t.Fatalf("auth failure counter = %v", got)
	}
}

func TestAPIKeyMiddlewareStripsKey(t *testing.T) {
	v, _ := auth.NewAPIKeyVerifier(config.APIKeySet{Header: "X-API-Key", Keys: []config.APIKey{{Name: "pathao", Key: "k-123"}}})
	var got http.Header
	h := APIKey(v, "partner", nil)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.Header.Clone() }))

	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("X-API-Key", "k-123")
	if rec := serve(h, r); rec.Code != 200 {
		t.Fatalf("valid key rejected: %d", rec.Code)
	}
	if got.Get("X-API-Key") != "" || got.Get(ConsumerHeader) != "key:pathao" {
		t.Fatalf("upstream headers = %v", got)
	}
	r = httptest.NewRequest("POST", "/", nil)
	r.Header.Set("X-API-Key", "wrong")
	if rec := serve(h, r); rec.Code != 401 {
		t.Fatalf("wrong key: %d", rec.Code)
	}
}

func TestTimeoutSetsDeadlineWithCause(t *testing.T) {
	var cause error
	h := Timeout(time.Millisecond)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		cause = context.Cause(r.Context())
	}))
	serve(h, httptest.NewRequest("GET", "/", nil))
	if !errors.Is(cause, ErrRouteTimeout) {
		t.Fatalf("cause = %v", cause)
	}
}

func TestClientIP(t *testing.T) {
	_, trusted, _ := net.ParseCIDR("10.0.0.0/8")
	nets := []*net.IPNet{trusted}
	cases := []struct {
		remote, xff, want string
		nets              []*net.IPNet
	}{
		{"203.0.113.7:5000", "", "203.0.113.7", nets},
		{"203.0.113.7:5000", "1.2.3.4", "203.0.113.7", nets}, // untrusted peer: XFF ignored
		{"10.0.0.2:5000", "1.2.3.4", "1.2.3.4", nets},
		{"10.0.0.2:5000", "6.6.6.6, 1.2.3.4, 10.0.0.9", "1.2.3.4", nets}, // right-most untrusted hop
		{"10.0.0.2:5000", "garbage", "10.0.0.2", nets},
		{"10.0.0.2:5000", "1.2.3.4", "10.0.0.2", nil},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := httpx.ClientIP(r, c.nets); got != c.want {
			t.Errorf("remote=%s xff=%q: got %s, want %s", c.remote, c.xff, got, c.want)
		}
	}
}

func BenchmarkGzipJSON(b *testing.B) {
	payload := []byte(`{"items":"` + strings.Repeat("hilsa-", 2000) + `"}`)
	h := Gzip(1024)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)
	}))
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	for i := 0; i < b.N; i++ {
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
}

func BenchmarkMiddlewareStack(b *testing.B) {
	lim := ratelimit.NewMemory(ratelimit.Rule{Rate: 1 << 30, Period: time.Second, Burst: 1 << 30}, nil)
	h := Chain(http.HandlerFunc(ok),
		RequestID(),
		Observe(discard, metrics.New(), func(*http.Request) string { return "1.1.1.1" }),
		Recover(discard),
		CORS(corsPolicy()),
		BodyLimit(1<<20),
		RateLimit(lim, RateLimitOptions{Route: "r", Policy: "p", Key: "ip", Log: discard,
			ClientIP: func(*http.Request) string { return "1.1.1.1" }}),
		Timeout(time.Second),
	)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			r := httptest.NewRequest("GET", "/api/catalog/products", nil)
			r.Header.Set("Origin", "https://dokan.com.bd")
			h.ServeHTTP(httptest.NewRecorder(), r)
		}
	})
}
