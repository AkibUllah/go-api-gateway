// Package config loads, expands and validates the gateway's YAML configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root of the gateway configuration file.
type Config struct {
	Server     Server                     `yaml:"server"`
	Admin      Admin                      `yaml:"admin"`
	Logging    Logging                    `yaml:"logging"`
	Redis      Redis                      `yaml:"redis"`
	Upstreams  map[string]Upstream        `yaml:"upstreams"`
	JWT        map[string]JWTProvider     `yaml:"jwt"`
	APIKeys    map[string]APIKeySet       `yaml:"api_keys"`
	RateLimits map[string]RateLimitPolicy `yaml:"rate_limits"`
	CORS       map[string]CORSPolicy      `yaml:"cors"`
	Routes     []Route                    `yaml:"routes"`
}

// Server configures the public listener.
type Server struct {
	Listen            string   `yaml:"listen"`
	ReadHeaderTimeout Duration `yaml:"read_header_timeout"`
	ReadTimeout       Duration `yaml:"read_timeout"`
	WriteTimeout      Duration `yaml:"write_timeout"`
	IdleTimeout       Duration `yaml:"idle_timeout"`
	ShutdownTimeout   Duration `yaml:"shutdown_timeout"`
	// DefaultTimeout applies to routes that do not set their own timeout.
	DefaultTimeout Duration `yaml:"default_timeout"`
	MaxBodyBytes   ByteSize `yaml:"max_body_bytes"`
	// TrustedProxies lists CIDRs whose X-Forwarded-For header is believed when
	// determining the client IP (rate limiting, logs).
	TrustedProxies []string `yaml:"trusted_proxies"`
	HealthPath     string   `yaml:"health_path"`
	ReadyPath      string   `yaml:"ready_path"`

	trusted []*net.IPNet
}

// TrustedNets returns the parsed trusted proxy CIDRs.
func (s Server) TrustedNets() []*net.IPNet { return s.trusted }

// Admin configures the optional admin listener serving /metrics, /healthz and /readyz.
// When Listen is empty those endpoints are served on the public listener instead.
type Admin struct {
	Listen      string `yaml:"listen"`
	MetricsPath string `yaml:"metrics_path"`
}

// Logging configures slog output.
type Logging struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// Redis is only required when a rate limit policy uses the redis backend.
type Redis struct {
	Addr     string `yaml:"addr"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
	// FailOpen lets requests through when Redis is unreachable instead of rejecting them.
	FailOpen bool `yaml:"fail_open"`
}

// Upstream is a pool of interchangeable backend targets.
type Upstream struct {
	Targets        []string       `yaml:"targets"`
	Balancer       string         `yaml:"balancer"`
	HealthCheck    HealthCheck    `yaml:"health_check"`
	CircuitBreaker CircuitBreaker `yaml:"circuit_breaker"`
	Retries        Retries        `yaml:"retries"`
	DialTimeout    Duration       `yaml:"dial_timeout"`
	MaxIdleConns   int            `yaml:"max_idle_conns_per_host"`
}

// HealthCheck configures active probing of every target in a pool.
type HealthCheck struct {
	Path               string   `yaml:"path"`
	Interval           Duration `yaml:"interval"`
	Timeout            Duration `yaml:"timeout"`
	HealthyThreshold   int      `yaml:"healthy_threshold"`
	UnhealthyThreshold int      `yaml:"unhealthy_threshold"`
}

// Enabled reports whether active health checks are configured.
func (h HealthCheck) Enabled() bool { return h.Path != "" }

// CircuitBreaker configures the per-target breaker.
type CircuitBreaker struct {
	FailureThreshold int      `yaml:"failure_threshold"`
	OpenTimeout      Duration `yaml:"open_timeout"`
	HalfOpenRequests int      `yaml:"half_open_requests"`
}

// Enabled reports whether the breaker is configured.
func (c CircuitBreaker) Enabled() bool { return c.FailureThreshold > 0 }

// Retries configures retries of idempotent requests on another target.
type Retries struct {
	Attempts int      `yaml:"attempts"`
	Backoff  Duration `yaml:"backoff"`
	OnStatus []int    `yaml:"on_status"`
}

// JWTProvider validates bearer tokens.
type JWTProvider struct {
	Algorithms    []string          `yaml:"algorithms"`
	Secret        string            `yaml:"secret"`
	PublicKeyFile string            `yaml:"public_key_file"`
	JWKSURL       string            `yaml:"jwks_url"`
	JWKSCacheTTL  Duration          `yaml:"jwks_cache_ttl"`
	Issuer        string            `yaml:"issuer"`
	Audience      string            `yaml:"audience"`
	Leeway        Duration          `yaml:"leeway"`
	ForwardClaims map[string]string `yaml:"forward_claims"`
}

// APIKeySet is a named collection of API keys checked from a header.
type APIKeySet struct {
	Header string   `yaml:"header"`
	Keys   []APIKey `yaml:"keys"`
}

// APIKey is one consumer credential. Exactly one of Key (plain, usually from an
// env var) or SHA256 (hex digest) must be set.
type APIKey struct {
	Name   string `yaml:"name"`
	Key    string `yaml:"key"`
	SHA256 string `yaml:"sha256"`
}

// RateLimitPolicy is a token bucket: Rate tokens per Period, up to Burst.
type RateLimitPolicy struct {
	Rate    int      `yaml:"rate"`
	Period  Duration `yaml:"period"`
	Burst   int      `yaml:"burst"`
	Key     string   `yaml:"key"`
	Backend string   `yaml:"backend"`
}

// CORSPolicy configures cross-origin access.
type CORSPolicy struct {
	AllowedOrigins   []string `yaml:"allowed_origins"`
	AllowedMethods   []string `yaml:"allowed_methods"`
	AllowedHeaders   []string `yaml:"allowed_headers"`
	ExposedHeaders   []string `yaml:"exposed_headers"`
	AllowCredentials bool     `yaml:"allow_credentials"`
	MaxAge           Duration `yaml:"max_age"`
}

// Route maps matching requests to an upstream with an optional middleware set.
// The middleware order is fixed by the gateway (see README); routes only choose
// which ones are active.
type Route struct {
	Name         string   `yaml:"name"`
	Host         string   `yaml:"host"`
	PathPrefix   string   `yaml:"path_prefix"`
	Methods      []string `yaml:"methods"`
	StripPrefix  bool     `yaml:"strip_prefix"`
	Upstream     string   `yaml:"upstream"`
	Timeout      Duration `yaml:"timeout"`
	MaxBodyBytes ByteSize `yaml:"max_body_bytes"`
	JWT          string   `yaml:"jwt"`
	APIKeys      string   `yaml:"api_keys"`
	RateLimit    string   `yaml:"rate_limit"`
	CORS         string   `yaml:"cors"`
	Gzip         bool     `yaml:"gzip"`
}

// Load reads, expands, parses, defaults and validates a config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the path is the operator's own -config flag
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(raw)
}

// Parse expands ${VAR} / ${VAR:-default} references, decodes YAML strictly
// (unknown keys are errors), applies defaults and validates.
func Parse(raw []byte) (*Config, error) {
	expanded, err := ExpandEnv(string(raw), os.LookupEnv)
	if err != nil {
		return nil, err
	}
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader([]byte(expanded)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	s := &c.Server
	setStr(&s.Listen, ":8080")
	setDur(&s.ReadHeaderTimeout, 5*time.Second)
	setDur(&s.ReadTimeout, 30*time.Second)
	setDur(&s.WriteTimeout, 60*time.Second)
	setDur(&s.IdleTimeout, 120*time.Second)
	setDur(&s.ShutdownTimeout, 20*time.Second)
	setDur(&s.DefaultTimeout, 30*time.Second)
	if s.MaxBodyBytes == 0 {
		s.MaxBodyBytes = 10 << 20
	}
	setStr(&s.HealthPath, "/healthz")
	setStr(&s.ReadyPath, "/readyz")
	setStr(&c.Admin.MetricsPath, "/metrics")
	setStr(&c.Logging.Level, "info")
	setStr(&c.Logging.Format, "json")

	for name, u := range c.Upstreams {
		setStr(&u.Balancer, "round_robin")
		setDur(&u.DialTimeout, 5*time.Second)
		if u.MaxIdleConns == 0 {
			u.MaxIdleConns = 64
		}
		if u.HealthCheck.Enabled() {
			setDur(&u.HealthCheck.Interval, 10*time.Second)
			setDur(&u.HealthCheck.Timeout, 2*time.Second)
			setInt(&u.HealthCheck.HealthyThreshold, 2)
			setInt(&u.HealthCheck.UnhealthyThreshold, 3)
		}
		if u.CircuitBreaker.Enabled() {
			setDur(&u.CircuitBreaker.OpenTimeout, 30*time.Second)
			setInt(&u.CircuitBreaker.HalfOpenRequests, 1)
		}
		if u.Retries.Attempts > 0 {
			setDur(&u.Retries.Backoff, 25*time.Millisecond)
			if len(u.Retries.OnStatus) == 0 {
				u.Retries.OnStatus = []int{502, 503, 504}
			}
		}
		c.Upstreams[name] = u
	}
	for name, j := range c.JWT {
		if len(j.Algorithms) == 0 {
			switch {
			case j.Secret != "":
				j.Algorithms = []string{"HS256"}
			default:
				j.Algorithms = []string{"RS256"}
			}
		}
		setDur(&j.JWKSCacheTTL, 10*time.Minute)
		c.JWT[name] = j
	}
	for name, k := range c.APIKeys {
		setStr(&k.Header, "X-API-Key")
		c.APIKeys[name] = k
	}
	for name, rl := range c.RateLimits {
		setDur(&rl.Period, time.Second)
		if rl.Burst == 0 {
			rl.Burst = rl.Rate
		}
		setStr(&rl.Key, "ip")
		setStr(&rl.Backend, "memory")
		c.RateLimits[name] = rl
	}
	for name, cp := range c.CORS {
		if len(cp.AllowedMethods) == 0 {
			cp.AllowedMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}
		}
		if len(cp.AllowedHeaders) == 0 {
			cp.AllowedHeaders = []string{"Authorization", "Content-Type", "X-Request-ID"}
		}
		c.CORS[name] = cp
	}
	for i := range c.Routes {
		r := &c.Routes[i]
		if r.Name == "" {
			r.Name = fmt.Sprintf("route-%d", i)
		}
		setStr(&r.PathPrefix, "/")
		if r.Timeout == 0 {
			r.Timeout = s.DefaultTimeout
		}
		if r.MaxBodyBytes == 0 {
			r.MaxBodyBytes = s.MaxBodyBytes
		}
		for j, m := range r.Methods {
			r.Methods[j] = strings.ToUpper(m)
		}
	}
}

// Validate checks cross references and value ranges and returns every problem
// found, joined, so an operator can fix a config file in one pass.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	c.Server.trusted = c.Server.trusted[:0]
	for _, cidr := range c.Server.TrustedProxies {
		if !strings.Contains(cidr, "/") {
			if ip := net.ParseIP(cidr); ip != nil && ip.To4() != nil {
				cidr += "/32"
			} else {
				cidr += "/128"
			}
		}
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			add("server.trusted_proxies: invalid CIDR %q", cidr)
			continue
		}
		c.Server.trusted = append(c.Server.trusted, n)
	}
	switch strings.ToLower(c.Logging.Level) {
	case "debug", "info", "warn", "error":
	default:
		add("logging.level: must be debug, info, warn or error, got %q", c.Logging.Level)
	}
	switch c.Logging.Format {
	case "json", "text":
	default:
		add("logging.format: must be json or text, got %q", c.Logging.Format)
	}

	usesRedis := false
	for _, name := range sortedKeys(c.Upstreams) {
		u := c.Upstreams[name]
		if len(u.Targets) == 0 {
			add("upstreams.%s: at least one target is required", name)
		}
		for _, t := range u.Targets {
			pu, err := url.Parse(t)
			if err != nil || (pu.Scheme != "http" && pu.Scheme != "https") || pu.Host == "" {
				add("upstreams.%s: target %q must be an absolute http(s) URL", name, t)
			}
		}
		if u.Balancer != "round_robin" && u.Balancer != "least_conn" {
			add("upstreams.%s.balancer: must be round_robin or least_conn, got %q", name, u.Balancer)
		}
		if u.HealthCheck.Enabled() && !strings.HasPrefix(u.HealthCheck.Path, "/") {
			add("upstreams.%s.health_check.path: must start with /", name)
		}
		if u.Retries.Attempts < 0 || u.Retries.Attempts > 5 {
			add("upstreams.%s.retries.attempts: must be between 0 and 5", name)
		}
	}
	for _, name := range sortedKeys(c.JWT) {
		j := c.JWT[name]
		sources := 0
		for _, s := range []string{j.Secret, j.PublicKeyFile, j.JWKSURL} {
			if s != "" {
				sources++
			}
		}
		if sources != 1 {
			add("jwt.%s: exactly one of secret, public_key_file or jwks_url is required", name)
		}
		for _, alg := range j.Algorithms {
			switch alg {
			case "HS256", "HS384", "HS512":
				if j.Secret == "" {
					add("jwt.%s: %s requires secret", name, alg)
				}
			case "RS256", "RS384", "RS512":
				if j.Secret != "" {
					add("jwt.%s: %s requires public_key_file or jwks_url", name, alg)
				}
			default:
				add("jwt.%s: unsupported algorithm %q", name, alg)
			}
		}
		if j.Secret != "" && len(j.Secret) < 32 {
			add("jwt.%s: secret must be at least 32 bytes", name)
		}
	}
	for _, name := range sortedKeys(c.APIKeys) {
		set := c.APIKeys[name]
		if len(set.Keys) == 0 {
			add("api_keys.%s: at least one key is required", name)
		}
		for i, k := range set.Keys {
			if k.Name == "" {
				add("api_keys.%s.keys[%d]: name is required", name, i)
			}
			if (k.Key == "") == (k.SHA256 == "") {
				add("api_keys.%s.keys[%d]: exactly one of key or sha256 is required", name, i)
			}
			if k.SHA256 != "" && len(k.SHA256) != 64 {
				add("api_keys.%s.keys[%d]: sha256 must be 64 hex characters", name, i)
			}
		}
	}
	for _, name := range sortedKeys(c.RateLimits) {
		rl := c.RateLimits[name]
		if rl.Rate <= 0 {
			add("rate_limits.%s.rate: must be > 0", name)
		}
		if rl.Burst < 1 {
			add("rate_limits.%s.burst: must be >= 1", name)
		}
		if !validRateKey(rl.Key) {
			add("rate_limits.%s.key: must be ip, consumer or header:<Name>, got %q", name, rl.Key)
		}
		switch rl.Backend {
		case "memory":
		case "redis":
			usesRedis = true
		default:
			add("rate_limits.%s.backend: must be memory or redis, got %q", name, rl.Backend)
		}
	}
	if usesRedis && c.Redis.Addr == "" {
		add("redis.addr: required because a rate limit uses the redis backend")
	}

	if len(c.Routes) == 0 {
		add("routes: at least one route is required")
	}
	seen := map[string]bool{}
	for i, r := range c.Routes {
		where := fmt.Sprintf("routes[%d] (%s)", i, r.Name)
		if seen[r.Name] {
			add("%s: duplicate route name", where)
		}
		seen[r.Name] = true
		if !strings.HasPrefix(r.PathPrefix, "/") {
			add("%s.path_prefix: must start with /", where)
		}
		if r.PathPrefix == c.Server.HealthPath || r.PathPrefix == c.Server.ReadyPath {
			add("%s.path_prefix: %q is reserved", where, r.PathPrefix)
		}
		if _, ok := c.Upstreams[r.Upstream]; !ok {
			add("%s.upstream: unknown upstream %q", where, r.Upstream)
		}
		ref := func(field, val string, ok bool) {
			if val != "" && !ok {
				add("%s.%s: unknown %s %q", where, field, field, val)
			}
		}
		_, ok := c.JWT[r.JWT]
		ref("jwt", r.JWT, ok)
		_, ok = c.APIKeys[r.APIKeys]
		ref("api_keys", r.APIKeys, ok)
		rl, ok := c.RateLimits[r.RateLimit]
		ref("rate_limit", r.RateLimit, ok)
		_, ok = c.CORS[r.CORS]
		ref("cors", r.CORS, ok)
		if r.JWT != "" && r.APIKeys != "" {
			add("%s: set either jwt or api_keys, not both", where)
		}
		if r.RateLimit != "" && rl.Key == "consumer" && r.JWT == "" && r.APIKeys == "" {
			add("%s.rate_limit: key \"consumer\" needs jwt or api_keys on the route", where)
		}
		if r.Timeout <= 0 {
			add("%s.timeout: must be > 0", where)
		}
	}
	return errors.Join(errs...)
}

func validRateKey(k string) bool {
	if k == "ip" || k == "consumer" {
		return true
	}
	name, ok := strings.CutPrefix(k, "header:")
	return ok && name != ""
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func setStr(p *string, v string) {
	if *p == "" {
		*p = v
	}
}

func setDur(p *Duration, v time.Duration) {
	if *p == 0 {
		*p = Duration(v)
	}
}

func setInt(p *int, v int) {
	if *p == 0 {
		*p = v
	}
}
