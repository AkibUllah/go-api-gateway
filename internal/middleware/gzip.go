package middleware

import (
	"bufio"
	"compress/gzip"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// Gzip compresses responses when the client accepts gzip, the upstream did not
// already encode the body, the content type is compressible and the body is at
// least minSize bytes. Bodies of unknown length are buffered up to minSize
// before deciding, so tiny JSON replies are not inflated by gzip framing.
func Gzip(minSize int) Middleware {
	pool := &sync.Pool{New: func() any {
		w, _ := gzip.NewWriterLevel(nil, gzip.DefaultCompression)
		return w
	}}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodHead || !acceptsGzip(r.Header.Get("Accept-Encoding")) {
				next.ServeHTTP(w, r)
				return
			}
			gw := &gzipWriter{ResponseWriter: w, minSize: minSize, pool: pool, status: http.StatusOK}
			defer gw.finish()
			next.ServeHTTP(gw, r)
		})
	}
}

// acceptsGzip parses Accept-Encoding, honouring "gzip;q=0" as a refusal.
func acceptsGzip(h string) bool {
	for _, part := range strings.Split(h, ",") {
		enc, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		enc = strings.TrimSpace(enc)
		if enc != "gzip" && enc != "*" {
			continue
		}
		params = strings.ReplaceAll(params, " ", "")
		if q, ok := strings.CutPrefix(params, "q="); ok {
			if v, err := strconv.ParseFloat(q, 64); err == nil && v == 0 {
				return false
			}
		}
		return true
	}
	return false
}

func compressibleType(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0]))
	switch {
	case ct == "":
		return false
	case ct == "text/event-stream":
		return false // streaming; compression would buffer events
	case strings.HasPrefix(ct, "text/"):
		return true
	case strings.HasSuffix(ct, "+json"), strings.HasSuffix(ct, "+xml"):
		return true
	}
	switch ct {
	case "application/json", "application/javascript", "application/xml",
		"application/x-ndjson", "image/svg+xml", "application/graphql-response+json":
		return true
	}
	return false
}

type gzipWriter struct {
	http.ResponseWriter
	minSize int
	pool    *sync.Pool

	status       int
	headerCalled bool
	decided      bool
	compress     bool
	buf          []byte
	gz           *gzip.Writer
}

func (g *gzipWriter) eligible() bool {
	h := g.Header()
	if h.Get("Content-Encoding") != "" || g.status < 200 ||
		g.status == http.StatusNoContent || g.status == http.StatusNotModified {
		return false
	}
	return compressibleType(h.Get("Content-Type"))
}

func (g *gzipWriter) WriteHeader(code int) {
	if g.decided || g.headerCalled {
		if g.decided && code < 200 {
			g.ResponseWriter.WriteHeader(code)
		}
		return
	}
	if code < 200 {
		g.ResponseWriter.WriteHeader(code) // 1xx is not the final header
		return
	}
	g.status = code
	g.headerCalled = true
	if !g.eligible() {
		g.decide(false)
		return
	}
	if cl := g.Header().Get("Content-Length"); cl != "" {
		n, err := strconv.Atoi(cl)
		g.decide(err == nil && n >= g.minSize)
	}
}

func (g *gzipWriter) Write(b []byte) (int, error) {
	if !g.headerCalled {
		if g.Header().Get("Content-Type") == "" {
			g.Header().Set("Content-Type", http.DetectContentType(b))
		}
		g.WriteHeader(http.StatusOK)
	}
	if g.decided {
		if g.compress {
			return g.gz.Write(b)
		}
		return g.ResponseWriter.Write(b)
	}
	g.buf = append(g.buf, b...)
	if len(g.buf) >= g.minSize {
		g.decide(true)
		if err := g.flushBuf(); err != nil {
			return 0, err
		}
	}
	return len(b), nil
}

func (g *gzipWriter) decide(compress bool) {
	g.decided = true
	g.compress = compress
	h := g.Header()
	if compressibleType(h.Get("Content-Type")) && h.Get("Content-Encoding") == "" {
		h.Add("Vary", "Accept-Encoding")
	}
	if compress {
		h.Del("Content-Length")
		h.Set("Content-Encoding", "gzip")
		g.gz = g.pool.Get().(*gzip.Writer)
		g.gz.Reset(g.ResponseWriter)
	}
	g.ResponseWriter.WriteHeader(g.status)
}

func (g *gzipWriter) flushBuf() error {
	if len(g.buf) == 0 {
		return nil
	}
	var err error
	if g.compress {
		_, err = g.gz.Write(g.buf)
	} else {
		_, err = g.ResponseWriter.Write(g.buf)
	}
	g.buf = nil
	return err
}

func (g *gzipWriter) finish() {
	if !g.decided && (g.headerCalled || len(g.buf) > 0) {
		g.decide(false)
		_ = g.flushBuf()
	}
	if g.gz != nil {
		_ = g.gz.Close()
		g.gz.Reset(nil)
		g.pool.Put(g.gz)
		g.gz = nil
	}
}

func (g *gzipWriter) Flush() {
	if !g.decided && g.headerCalled {
		g.decide(g.eligible())
		_ = g.flushBuf()
	}
	if g.gz != nil {
		_ = g.gz.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (g *gzipWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := g.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("hijack not supported")
}

func (g *gzipWriter) Unwrap() http.ResponseWriter { return g.ResponseWriter }
