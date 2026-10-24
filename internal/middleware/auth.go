package middleware

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/AkibUllah/go-api-gateway/internal/auth"
	"github.com/AkibUllah/go-api-gateway/internal/httpx"
	"github.com/AkibUllah/go-api-gateway/internal/metrics"
	"github.com/AkibUllah/go-api-gateway/internal/ratelimit"
)

// ConsumerHeader is set on upstream requests to the authenticated consumer.
// Any client-supplied value is removed first so it cannot be spoofed.
const ConsumerHeader = "X-Consumer-ID"

type principalKey struct{}

// PrincipalFrom returns the authenticated principal, if any.
func PrincipalFrom(ctx context.Context) *auth.Principal {
	p, _ := ctx.Value(principalKey{}).(*auth.Principal)
	return p
}

func withPrincipal(r *http.Request, p *auth.Principal) *http.Request {
	httpx.GetInfo(r.Context()).Consumer = p.Consumer
	r.Header.Set(ConsumerHeader, p.Consumer)
	return r.WithContext(context.WithValue(r.Context(), principalKey{}, p))
}

// JWT requires a valid bearer token. Configured claims are copied into
// request headers for the upstream after stripping client-sent copies.
func JWT(v *auth.JWTVerifier, route string, m *metrics.Metrics, log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Header.Del(ConsumerHeader)
			for _, header := range v.ForwardClaims() {
				r.Header.Del(header)
			}
			raw, ok := auth.BearerToken(r.Header.Get("Authorization"))
			if !ok {
				reject(w, r, m, route, "jwt", `Bearer realm="gateway"`, "missing bearer token")
				return
			}
			p, err := v.Verify(raw)
			if err != nil {
				log.Debug("jwt rejected", "request_id", httpx.RequestID(r.Context()), "error", err)
				reject(w, r, m, route, "jwt", `Bearer realm="gateway", error="invalid_token"`, "invalid or expired token")
				return
			}
			for claim, header := range v.ForwardClaims() {
				if val, ok := auth.ClaimString(p.Claims[claim]); ok {
					r.Header.Set(header, val)
				}
			}
			next.ServeHTTP(w, withPrincipal(r, p))
		})
	}
}

// APIKey requires a valid key in the configured header. The key itself is
// removed before the request is forwarded so upstreams never see it.
func APIKey(v *auth.APIKeyVerifier, route string, m *metrics.Metrics) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Header.Del(ConsumerHeader)
			presented := r.Header.Get(v.Header)
			r.Header.Del(v.Header)
			p, err := v.Verify(presented)
			if err != nil {
				reject(w, r, m, route, "api_key", "", "missing or invalid API key")
				return
			}
			next.ServeHTTP(w, withPrincipal(r, p))
		})
	}
}

func reject(w http.ResponseWriter, r *http.Request, m *metrics.Metrics, route, method, challenge, msg string) {
	if m != nil {
		m.AuthFailures.WithLabelValues(route, method).Inc()
	}
	if challenge != "" {
		w.Header().Set("WWW-Authenticate", challenge)
	}
	httpx.WriteError(w, r, http.StatusUnauthorized, "unauthorized", msg)
}

// RateLimitOptions configures the RateLimit middleware.
type RateLimitOptions struct {
	Route    string
	Policy   string
	Key      string // "ip", "consumer" or "header:<Name>"
	FailOpen bool
	ClientIP ClientIPFunc
	Metrics  *metrics.Metrics
	Log      *slog.Logger
}

// RateLimit enforces a token bucket per key and sets the IETF draft
// RateLimit-Limit / RateLimit-Remaining headers plus Retry-After on 429.
func RateLimit(l ratelimit.Limiter, o RateLimitOptions) Middleware {
	header, byHeader := strings.CutPrefix(o.Key, "header:")
	keyFor := func(r *http.Request) string {
		switch {
		case o.Key == "consumer":
			if p := PrincipalFrom(r.Context()); p != nil {
				return "c:" + p.Consumer
			}
		case byHeader:
			if v := r.Header.Get(header); v != "" {
				return "h:" + v
			}
		}
		return "ip:" + o.ClientIP(r)
	}
	prefix := o.Route + ":" + o.Policy + ":"

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			d, err := l.Allow(r.Context(), prefix+keyFor(r))
			if err != nil {
				o.Log.Warn("rate limiter unavailable", "route", o.Route, "policy", o.Policy,
					"fail_open", o.FailOpen, "error", err)
				if o.FailOpen {
					next.ServeHTTP(w, r)
					return
				}
				httpx.WriteError(w, r, http.StatusServiceUnavailable, "rate_limiter_unavailable",
					"rate limiter unavailable")
				return
			}
			h := w.Header()
			h.Set("RateLimit-Limit", strconv.Itoa(d.Limit))
			h.Set("RateLimit-Remaining", strconv.Itoa(d.Remaining))
			if !d.Allowed {
				if o.Metrics != nil {
					o.Metrics.RateLimited.WithLabelValues(o.Route, o.Policy).Inc()
				}
				secs := int(math.Ceil(d.RetryAfter.Seconds()))
				if secs < 1 {
					secs = 1
				}
				h.Set("Retry-After", strconv.Itoa(secs))
				httpx.WriteError(w, r, http.StatusTooManyRequests, "rate_limited", "too many requests")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Timeout bounds the whole upstream exchange (all retries included). The
// proxy maps the expired context to 504 Gateway Timeout.
func Timeout(d time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeoutCause(r.Context(), d, ErrRouteTimeout)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ErrRouteTimeout is the context cause when a route timeout fires.
var ErrRouteTimeout = errors.New("route timeout exceeded")
