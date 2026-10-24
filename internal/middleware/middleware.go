// Package middleware contains the gateway's HTTP middleware. Each middleware
// is a plain func(http.Handler) http.Handler so they compose with the standard
// library and can be unit-tested with httptest in isolation.
package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/AkibUllah/go-api-gateway/internal/httpx"
	"github.com/AkibUllah/go-api-gateway/internal/metrics"
)

// Middleware wraps a handler.
type Middleware func(http.Handler) http.Handler

// Chain applies middleware so that the first one listed is the outermost.
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		if mws[i] != nil {
			h = mws[i](h)
		}
	}
	return h
}

// RequestIDHeader is the header used to propagate request IDs.
const RequestIDHeader = "X-Request-ID"

// RequestID accepts a well-formed incoming X-Request-ID (so IDs from an edge
// load balancer survive) or generates a new one, then sets it on the request
// (forwarded upstream), the response and the context.
func RequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(RequestIDHeader)
			if !validRequestID(id) {
				id = NewRequestID()
			}
			r.Header.Set(RequestIDHeader, id)
			w.Header().Set(RequestIDHeader, id)
			next.ServeHTTP(w, r.WithContext(httpx.WithRequestID(r.Context(), id)))
		})
	}
}

// NewRequestID returns 16 random bytes, hex encoded.
func NewRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func validRequestID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if !isIDChar(id[i]) {
			return false
		}
	}
	return true
}

func isIDChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return c == '-' || c == '_' || c == '.'
}

// Recover converts a panic into a 500 and logs the stack. http.ErrAbortHandler
// is re-panicked because net/http uses it to abort a response deliberately.
func Recover(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
						panic(v)
					}
					log.Error("panic serving request",
						"request_id", httpx.RequestID(r.Context()),
						"panic", fmt.Sprint(v),
						"stack", string(debug.Stack()))
					httpx.WriteError(w, r, http.StatusInternalServerError, "internal_error", "internal server error")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// Observe writes one structured access log line per request and records the
// request counter and latency histogram. It attaches an httpx.Info to the
// context which inner layers fill with route, target and consumer.
func Observe(log *slog.Logger, m *metrics.Metrics, trusted ClientIPFunc) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			info := &httpx.Info{}
			rec := httpx.NewStatusRecorder(w)
			if m != nil {
				m.InFlight.Inc()
				defer m.InFlight.Dec()
			}
			next.ServeHTTP(rec, r.WithContext(httpx.WithInfo(r.Context(), info)))

			elapsed := time.Since(start)
			route := info.Route
			if route == "" {
				route = "unmatched"
			}
			if m != nil {
				m.Requests.WithLabelValues(route, r.Method, fmt.Sprint(rec.Status)).Inc()
				m.Duration.WithLabelValues(route).Observe(elapsed.Seconds())
			}
			level := slog.LevelInfo
			if rec.Status >= 500 {
				level = slog.LevelError
			}
			attrs := []slog.Attr{
				slog.String("request_id", httpx.RequestID(r.Context())),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.Status),
				slog.Int64("bytes", rec.Bytes),
				slog.Float64("duration_ms", float64(elapsed.Microseconds())/1000),
				slog.String("route", route),
				slog.String("client_ip", trusted(r)),
			}
			if info.Target != "" {
				attrs = append(attrs, slog.String("upstream", info.Target), slog.Int("attempts", info.Attempts))
			}
			if info.Consumer != "" {
				attrs = append(attrs, slog.String("consumer", info.Consumer))
			}
			log.LogAttrs(r.Context(), level, "request", attrs...)
		})
	}
}

// ClientIPFunc resolves the client IP for a request.
type ClientIPFunc func(*http.Request) string

// BodyLimit rejects bodies larger than max bytes with 413. A declared
// Content-Length is checked up front; chunked bodies are capped while the
// upstream reads them (the proxy maps the resulting error to 413).
func BodyLimit(max int64) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > max {
				httpx.WriteError(w, r, http.StatusRequestEntityTooLarge, "body_too_large",
					fmt.Sprintf("request body exceeds %d bytes", max))
				return
			}
			if r.Body != nil && r.Body != http.NoBody {
				r.Body = http.MaxBytesReader(w, r.Body, max)
			}
			next.ServeHTTP(w, r)
		})
	}
}
