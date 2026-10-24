// Package httpx holds small HTTP helpers shared by the middleware and proxy
// packages: request-scoped values, JSON errors, client IP resolution and a
// status-capturing ResponseWriter.
package httpx

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
)

type ctxKey int

const (
	keyRequestID ctxKey = iota
	keyInfo
)

// Info carries per-request facts that middleware fills in and the access log
// reads at the end (route, upstream target, consumer).
type Info struct {
	Route    string
	Target   string
	Consumer string
	Attempts int
}

// WithRequestID stores the request ID in the context.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyRequestID, id)
}

// RequestID returns the request ID or "".
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(keyRequestID).(string)
	return id
}

// WithInfo attaches a mutable Info to the context.
func WithInfo(ctx context.Context, i *Info) context.Context {
	return context.WithValue(ctx, keyInfo, i)
}

// GetInfo returns the request's Info. It never returns nil, so callers can
// write to it unconditionally (writes are dropped if no Info was attached).
func GetInfo(ctx context.Context) *Info {
	if i, ok := ctx.Value(keyInfo).(*Info); ok {
		return i
	}
	return &Info{}
}

// ErrorBody is the JSON envelope for every error the gateway itself produces.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail describes a gateway error.
type ErrorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

// WriteError writes a JSON error response.
func WriteError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ErrorBody{Error: ErrorDetail{
		Code: code, Message: message, RequestID: RequestID(r.Context()),
	}})
}

// WriteJSON writes v as a JSON response.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ClientIP returns the caller's IP. X-Forwarded-For is honoured only when the
// direct peer is a trusted proxy; the header is then walked right-to-left and
// the first untrusted hop wins, which is the address a client cannot forge.
func ClientIP(r *http.Request, trusted []*net.IPNet) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if len(trusted) == 0 || !inNets(net.ParseIP(host), trusted) {
		return host
	}
	xff := r.Header.Values("X-Forwarded-For")
	hops := strings.Split(strings.Join(xff, ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		h := strings.TrimSpace(hops[i])
		ip := net.ParseIP(h)
		if ip == nil {
			break
		}
		if !inNets(ip, trusted) {
			return h
		}
	}
	return host
}

func inNets(ip net.IP, nets []*net.IPNet) bool {
	if ip == nil {
		return false
	}
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// StatusRecorder wraps a ResponseWriter to capture the status code and the
// number of body bytes written. It forwards Flush, Hijack and Unwrap so
// streaming and protocol upgrades keep working through middleware.
type StatusRecorder struct {
	http.ResponseWriter
	Status      int
	Bytes       int64
	wroteHeader bool
}

// NewStatusRecorder wraps w.
func NewStatusRecorder(w http.ResponseWriter) *StatusRecorder {
	return &StatusRecorder{ResponseWriter: w, Status: http.StatusOK}
}

// WriteHeader implements http.ResponseWriter.
func (s *StatusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.Status = code
		// 1xx informational responses are not final.
		if code >= 200 {
			s.wroteHeader = true
		}
	}
	s.ResponseWriter.WriteHeader(code)
}

// Write implements http.ResponseWriter.
func (s *StatusRecorder) Write(b []byte) (int, error) {
	s.wroteHeader = true
	n, err := s.ResponseWriter.Write(b)
	s.Bytes += int64(n)
	return n, err
}

// WroteHeader reports whether a final status was sent.
func (s *StatusRecorder) WroteHeader() bool { return s.wroteHeader }

// Flush implements http.Flusher.
func (s *StatusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack implements http.Hijacker.
func (s *StatusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := s.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("hijack not supported")
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (s *StatusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
