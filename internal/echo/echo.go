// Package echo is a tiny demo upstream used by docker compose, the all-in-one
// demo binary and the tests. It reflects the request it received so you can
// see exactly what the gateway forwarded (stripped prefixes, forwarded
// claims, request IDs, X-Forwarded-* headers).
//
// Query parameters simulate upstream behaviour:
//
//	?status=503     respond with that status code
//	?delay=300ms    sleep before responding (max 30s)
//	?size=4096      pad the response body to roughly N bytes (max 1MiB)
package echo

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Response is the JSON body returned for every non-health request.
type Response struct {
	Service   string            `json:"service"`
	Instance  string            `json:"instance"`
	Method    string            `json:"method"`
	Path      string            `json:"path"`
	Query     string            `json:"query,omitempty"`
	Host      string            `json:"host"`
	Headers   map[string]string `json:"headers"`
	BodyBytes int64             `json:"body_bytes"`
	Padding   string            `json:"padding,omitempty"`
}

// Handler returns the echo handler.
func Handler(service, instance string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": service, "instance": instance})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if d, err := time.ParseDuration(q.Get("delay")); err == nil && d > 0 {
			if d > 30*time.Second {
				d = 30 * time.Second
			}
			select {
			case <-time.After(d):
			case <-r.Context().Done():
				return
			}
		}
		n, _ := io.Copy(io.Discard, io.LimitReader(r.Body, 10<<20))
		resp := Response{
			Service:   service,
			Instance:  instance,
			Method:    r.Method,
			Path:      r.URL.Path,
			Query:     r.URL.RawQuery,
			Host:      r.Host,
			Headers:   flatten(r.Header),
			BodyBytes: n,
		}
		if size, err := strconv.Atoi(q.Get("size")); err == nil && size > 0 {
			if size > 1<<20 {
				size = 1 << 20
			}
			resp.Padding = strings.Repeat("a", size)
		}
		status := http.StatusOK
		if s, err := strconv.Atoi(q.Get("status")); err == nil && s >= 200 && s <= 599 {
			status = s
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Instance", instance)
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(resp)
	})
	return mux
}

func flatten(h http.Header) map[string]string {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		out[k] = strings.Join(h[k], ", ")
	}
	return out
}
