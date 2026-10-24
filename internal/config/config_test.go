package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const minimal = `
upstreams:
  users:
    targets: ["http://127.0.0.1:9001"]
routes:
  - name: users
    path_prefix: /api/users
    upstream: users
`

func TestParseAppliesDefaults(t *testing.T) {
	cfg, err := Parse([]byte(minimal + `
    rate_limit: rl
rate_limits:
  rl: {rate: 5}
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != ":8080" || cfg.Server.HealthPath != "/healthz" || cfg.Admin.MetricsPath != "/metrics" {
		t.Fatalf("server defaults not applied: %+v", cfg.Server)
	}
	if got := cfg.Routes[0].Timeout.D(); got != 30*time.Second {
		t.Fatalf("route timeout default = %v", got)
	}
	if cfg.Routes[0].MaxBodyBytes != 10<<20 {
		t.Fatalf("route body limit should inherit server default, got %d", cfg.Routes[0].MaxBodyBytes)
	}
	if u := cfg.Upstreams["users"]; u.Balancer != "round_robin" || u.MaxIdleConns != 64 {
		t.Fatalf("upstream defaults: %+v", u)
	}
	rl := cfg.RateLimits["rl"]
	if rl.Burst != 5 || rl.Period.D() != time.Second || rl.Key != "ip" || rl.Backend != "memory" {
		t.Fatalf("rate limit defaults: %+v", rl)
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	_, err := Parse([]byte(minimal + "\n    stripprefix: true\n"))
	if err == nil || !strings.Contains(err.Error(), "stripprefix") {
		t.Fatalf("expected unknown field error, got %v", err)
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	_, err := Parse([]byte(`
upstreams:
  a:
    targets: ["ftp://nope"]
    balancer: random
routes:
  - name: r
    path_prefix: api
    upstream: missing
    jwt: nobody
`))
	if err == nil {
		t.Fatal("expected validation error")
	}
	for _, want := range []string{
		"must be an absolute http(s) URL",
		"must be round_robin or least_conn",
		"path_prefix: must start with /",
		`unknown upstream "missing"`,
		`unknown jwt "nobody"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

func TestValidateCrossFieldRules(t *testing.T) {
	cases := map[string]struct{ yaml, want string }{
		"redis backend needs addr": {minimal + `    rate_limit: rl
rate_limits:
  rl: {rate: 1, backend: redis}
`, "redis.addr: required"},
		"consumer key needs auth": {minimal + `    rate_limit: rl
rate_limits:
  rl: {rate: 1, key: consumer}
`, `key "consumer" needs jwt or api_keys`},
		"short hmac secret": {minimal + `    jwt: j
jwt:
  j: {secret: short}
`, "at least 32 bytes"},
		"jwt and api key together": {minimal + `    jwt: j
    api_keys: k
jwt:
  j: {secret: "0123456789abcdef0123456789abcdef"}
api_keys:
  k: {keys: [{name: a, key: b}]}
`, "either jwt or api_keys"},
		"api key needs exactly one form": {minimal + `    api_keys: k
api_keys:
  k: {keys: [{name: a, key: b, sha256: c}]}
`, "exactly one of key or sha256"},
		"reserved path": {strings.Replace(minimal, "/api/users", "/healthz", 1), "is reserved"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestExpandEnv(t *testing.T) {
	env := map[string]string{"HOST": "users", "EMPTY": ""}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	cases := []struct {
		in, want, err string
	}{
		{"http://${HOST}:80", "http://users:80", ""},
		{"${PORT:-8080}", "8080", ""},
		{"${EMPTY:-fallback}", "fallback", ""},
		{"cost $$5 and $HOME", "cost $5 and $HOME", ""},
		{"${SECRET}", "", "SECRET"},
		{"${UNCLOSED", "", "unterminated"},
	}
	for _, c := range cases {
		got, err := ExpandEnv(c.in, lookup)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("ExpandEnv(%q) error = %v, want %q", c.in, err, c.err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("ExpandEnv(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

func TestParseByteSize(t *testing.T) {
	cases := map[string]ByteSize{
		"512": 512, "1KB": 1000, "1KiB": 1024, "10MB": 10_000_000, "2MiB": 2 << 20, "1 gib": 1 << 30, "64B": 64,
	}
	for in, want := range cases {
		got, err := ParseByteSize(in)
		if err != nil || got != want {
			t.Errorf("ParseByteSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "MB", "-1KB", "1.5MB", "ten"} {
		if _, err := ParseByteSize(bad); err == nil {
			t.Errorf("ParseByteSize(%q) should fail", bad)
		}
	}
}

func TestDurationMustBeValid(t *testing.T) {
	_, err := Parse([]byte(minimal + "    timeout: soon\n"))
	if err == nil || !strings.Contains(err.Error(), "invalid duration") {
		t.Fatalf("expected duration error, got %v", err)
	}
}

func TestTrustedProxiesAcceptBareIPsAndCIDRs(t *testing.T) {
	cfg, err := Parse([]byte(minimal + `server:
  trusted_proxies: ["10.0.0.0/8", "192.168.1.10", "::1"]
`))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(cfg.Server.TrustedNets()); n != 3 {
		t.Fatalf("parsed %d nets, want 3", n)
	}
	if _, err := Parse([]byte(minimal + "server:\n  trusted_proxies: [\"not-an-ip\"]\n")); err == nil {
		t.Fatal("invalid CIDR accepted")
	}
}

func TestBundledConfigLoadsWithoutEnvironment(t *testing.T) {
	for _, k := range []string{"PORT", "REDIS_ADDR", "RATE_LIMIT_BACKEND", "JWT_SECRET", "ADMIN_LISTEN"} {
		t.Setenv(k, "")
	}
	cfg, err := Load(filepath.Join("..", "..", "configs", "gateway.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Routes) != 3 || len(cfg.Upstreams) != 2 {
		t.Fatalf("unexpected bundled config shape: %d routes, %d upstreams", len(cfg.Routes), len(cfg.Upstreams))
	}
	if cfg.Routes[1].MaxBodyBytes != 256_000 {
		t.Fatalf("orders body limit = %d", cfg.Routes[1].MaxBodyBytes)
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil || !os.IsNotExist(unwrapAll(err)) {
		t.Fatalf("expected not-exist error, got %v", err)
	}
}

func unwrapAll(err error) error {
	for {
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return err
		}
		err = u.Unwrap()
	}
}
