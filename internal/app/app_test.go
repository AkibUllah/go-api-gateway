package app

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckValidatesBundledConfig(t *testing.T) {
	if code := Run([]string{"-config", filepath.Join("..", "..", "configs", "gateway.yaml"), "-check"}); code != 0 {
		t.Fatalf("exit code %d for the bundled config", code)
	}
}

func TestCheckRejectsInvalidConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(path, []byte("routes: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{"-config", path, "-check"}); code != 2 {
		t.Fatalf("exit code %d, want 2", code)
	}
	if code := Run([]string{"-no-such-flag"}); code != 2 {
		t.Fatalf("unknown flag exit code %d, want 2", code)
	}
}

func TestHealthcheckProbe(t *testing.T) {
	okSrv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer okSrv.Close()
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
	defer badSrv.Close()
	if code := Run([]string{"-healthcheck", okSrv.URL}); code != 0 {
		t.Fatalf("healthy probe exit %d", code)
	}
	if code := Run([]string{"-healthcheck", badSrv.URL}); code != 1 {
		t.Fatalf("unhealthy probe exit %d", code)
	}
	if code := Run([]string{"-version"}); code != 0 {
		t.Fatalf("version exit %d", code)
	}
}
