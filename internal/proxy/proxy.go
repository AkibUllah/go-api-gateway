package proxy

import (
	"context"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"sync"
	"time"

	"github.com/AkibUllah/go-api-gateway/internal/httpx"
	"github.com/AkibUllah/go-api-gateway/internal/metrics"
	"github.com/AkibUllah/go-api-gateway/internal/upstream"
)

// NewTransport returns the shared HTTP transport for one upstream pool.
func NewTransport(dialTimeout time.Duration, maxIdlePerHost int) *http.Transport {
	return &http.Transport{
		Proxy: nil, // never route upstream traffic through HTTP(S)_PROXY env vars
		DialContext: (&net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          1024,
		MaxIdleConnsPerHost:   maxIdlePerHost,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// Options configures a proxy handler for one route.
type Options struct {
	Pool      *upstream.Pool
	Transport http.RoundTripper
	Metrics   *metrics.Metrics
	Log       *slog.Logger
	// TrustedPeer reports whether the direct peer is a trusted proxy whose
	// X-Forwarded-For chain should be preserved.
	TrustedPeer func(*http.Request) bool
}

// New returns an http.Handler that forwards to the pool.
func New(o Options) http.Handler {
	rt := &retryTransport{pool: o.Pool, base: o.Transport, metrics: o.Metrics}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// Rewrite starts from a copy with X-Forwarded-* removed; keep the
			// inbound chain only when it came from a proxy we trust.
			if o.TrustedPeer != nil && o.TrustedPeer(pr.In) {
				if xff := pr.In.Header.Values("X-Forwarded-For"); len(xff) > 0 {
					pr.Out.Header["X-Forwarded-For"] = append([]string(nil), xff...)
				}
			}
			pr.SetXForwarded()
			// Scheme and host are filled in per attempt by retryTransport.
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = o.Pool.Name
			pr.Out.Host = ""
		},
		Transport:    rt,
		BufferPool:   bufferPool{},
		ErrorLog:     log.New(slogWriter{o.Log}, "", 0),
		ErrorHandler: errorHandler(o.Log),
	}
}

func errorHandler(lg *slog.Logger) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		var maxBytes *http.MaxBytesError
		switch {
		case errors.As(err, &maxBytes):
			httpx.WriteError(w, r, http.StatusRequestEntityTooLarge, "body_too_large", "request body too large")
		case errors.Is(err, upstream.ErrNoTarget):
			httpx.WriteError(w, r, http.StatusServiceUnavailable, "no_healthy_upstream", "no healthy upstream available")
		case errors.Is(err, context.DeadlineExceeded) || errors.Is(context.Cause(r.Context()), context.DeadlineExceeded) ||
			r.Context().Err() == context.DeadlineExceeded:
			httpx.WriteError(w, r, http.StatusGatewayTimeout, "upstream_timeout", "upstream did not respond in time")
		case errors.Is(r.Context().Err(), context.Canceled):
			// Client went away; nobody is listening for the response.
			w.WriteHeader(499)
		default:
			lg.Warn("upstream error", "request_id", httpx.RequestID(r.Context()), "error", err)
			httpx.WriteError(w, r, http.StatusBadGateway, "bad_gateway", "upstream request failed")
		}
	}
}

// retryTransport selects a target per attempt and retries idempotent,
// bodiless requests on a different target after connection errors or a
// retryable status (502/503/504 by default).
type retryTransport struct {
	pool    *upstream.Pool
	base    http.RoundTripper
	metrics *metrics.Metrics
}

func replayable(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete, http.MethodTrace:
	default:
		return false
	}
	return r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0 && r.Header.Get("Transfer-Encoding") == ""
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	info := httpx.GetInfo(ctx)
	attempts := 1
	if replayable(req) {
		attempts += t.pool.Config.Retries.Attempts
	}
	backoff := t.pool.Config.Retries.Backoff.D()
	tried := make(map[*upstream.Target]bool, attempts)

	var lastResp *http.Response
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		target, err := t.pool.Pick(tried)
		if err != nil {
			if lastResp != nil {
				return lastResp, nil
			}
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, err
		}
		if lastResp != nil {
			drainClose(lastResp.Body)
			lastResp = nil
		}
		if attempt > 1 && t.metrics != nil {
			t.metrics.Retries.WithLabelValues(t.pool.Name).Inc()
		}
		tried[target] = true
		info.Target, info.Attempts = target.URL.Host, attempt

		out := req.Clone(ctx)
		out.URL.Scheme = target.URL.Scheme
		out.URL.Host = target.URL.Host
		out.URL.Path = joinPath(target.URL.Path, req.URL.Path)
		if req.URL.RawPath != "" {
			out.URL.RawPath = joinPath(target.URL.EscapedPath(), req.URL.RawPath)
		}
		out.Host = ""

		release := target.Acquire()
		resp, err := t.base.RoundTrip(out)
		if err != nil && errors.Is(ctx.Err(), context.Canceled) {
			target.Breaker().Ignore() // the client left; says nothing about the upstream
		} else {
			target.Breaker().Done(err == nil && resp.StatusCode < 500)
		}
		t.observe(target, resp, err)

		if err != nil {
			release()
			lastErr = err
			if ctx.Err() != nil || attempt == attempts {
				return nil, err
			}
			if !sleep(ctx, backoff*time.Duration(attempt)) {
				return nil, ctx.Err()
			}
			continue
		}
		resp.Body = wrapBody(resp.Body, release)
		if t.pool.RetryableStatus(resp.StatusCode) && attempt < attempts {
			lastResp = resp
			if !sleep(ctx, backoff*time.Duration(attempt)) {
				drainClose(resp.Body)
				return nil, ctx.Err()
			}
			continue
		}
		return resp, nil
	}
	if lastResp != nil {
		return lastResp, nil
	}
	return nil, lastErr
}

func (t *retryTransport) observe(target *upstream.Target, resp *http.Response, err error) {
	if t.metrics == nil {
		return
	}
	outcome := "error"
	if err == nil {
		outcome = strconv.Itoa(resp.StatusCode/100) + "xx"
	}
	t.metrics.UpstreamRequests.WithLabelValues(t.pool.Name, target.URL.Host, outcome).Inc()
}

func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func drainClose(b io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(b, 64<<10))
	_ = b.Close()
}

func joinPath(base, p string) string {
	switch {
	case base == "" || base == "/":
		return p
	case p == "" || p == "/":
		return base
	}
	if base[len(base)-1] == '/' {
		base = base[:len(base)-1]
	}
	return base + p
}

// releaseBody decrements the target's in-flight counter when the response
// body is closed, which is what least-connections balancing needs.
type releaseBody struct {
	io.ReadCloser
	release func()
}

func (b *releaseBody) Close() error {
	err := b.ReadCloser.Close()
	b.release()
	return err
}

// releaseRWBody preserves io.Writer for 101 Switching Protocols responses,
// which ReverseProxy requires to tunnel WebSocket connections.
type releaseRWBody struct {
	io.ReadWriteCloser
	release func()
}

func (b *releaseRWBody) Close() error {
	err := b.ReadWriteCloser.Close()
	b.release()
	return err
}

func wrapBody(body io.ReadCloser, release func()) io.ReadCloser {
	if rwc, ok := body.(io.ReadWriteCloser); ok {
		return &releaseRWBody{ReadWriteCloser: rwc, release: release}
	}
	return &releaseBody{ReadCloser: body, release: release}
}

var buffers = sync.Pool{New: func() any { b := make([]byte, 32<<10); return &b }}

type bufferPool struct{}

func (bufferPool) Get() []byte  { return *(buffers.Get().(*[]byte)) }
func (bufferPool) Put(b []byte) { buffers.Put(&b) }

type slogWriter struct{ l *slog.Logger }

func (s slogWriter) Write(p []byte) (int, error) {
	msg := string(p)
	if n := len(msg); n > 0 && msg[n-1] == '\n' {
		msg = msg[:n-1]
	}
	s.l.Warn("reverse proxy", "detail", msg)
	return len(p), nil
}
