package echo

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEchoReflectsRequest(t *testing.T) {
	h := Handler("catalog", "catalog-1")
	r := httptest.NewRequest("POST", "/products?lang=bn", strings.NewReader("hello"))
	r.Header.Set("X-User-ID", "customer-42")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	var got Response
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Service != "catalog" || got.Method != "POST" || got.Path != "/products" || got.Query != "lang=bn" ||
		got.BodyBytes != 5 || got.Headers["X-User-Id"] != "customer-42" {
		t.Fatalf("echo = %+v", got)
	}
	if rec.Header().Get("X-Upstream-Instance") != "catalog-1" {
		t.Fatal("instance header missing")
	}
}

func TestEchoSimulationParams(t *testing.T) {
	h := Handler("orders", "orders-1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x?status=503&size=2048", nil))
	if rec.Code != 503 || rec.Body.Len() < 2048 {
		t.Fatalf("status=%d len=%d", rec.Code, rec.Body.Len())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/health", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("health: %d %s", rec.Code, rec.Body)
	}
}
