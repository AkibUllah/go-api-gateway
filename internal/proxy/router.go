// Package proxy routes requests to upstream pools and forwards them with
// load balancing, retries and circuit breaking.
package proxy

import (
	"net"
	"net/http"
	"sort"
	"strings"

	"github.com/AkibUllah/go-api-gateway/internal/httpx"
)

// Route is a compiled routing rule.
type Route struct {
	Name        string
	Host        string // "", "api.example.com" or "*.example.com"
	Prefix      string
	Methods     []string
	StripPrefix bool
	Handler     http.Handler
}

func (r *Route) hostScore() int {
	switch {
	case r.Host == "":
		return 0
	case strings.HasPrefix(r.Host, "*."):
		return 1
	default:
		return 2
	}
}

func (r *Route) matchHost(host string) bool {
	switch {
	case r.Host == "":
		return true
	case strings.HasPrefix(r.Host, "*."):
		suffix := r.Host[1:] // ".example.com"
		return strings.HasSuffix(host, suffix) && len(host) > len(suffix)
	default:
		return host == r.Host
	}
}

// matchPath matches at path-segment boundaries: "/api/users" matches
// "/api/users" and "/api/users/42" but not "/api/usersettings".
func (r *Route) matchPath(path string) bool {
	if r.Prefix == "/" {
		return true
	}
	return path == r.Prefix || strings.HasPrefix(path, r.Prefix+"/")
}

func (r *Route) allowsMethod(m string) bool {
	if len(r.Methods) == 0 {
		return true
	}
	for _, x := range r.Methods {
		if x == m {
			return true
		}
	}
	return false
}

// Router picks the most specific matching route: exact host beats wildcard
// host beats any host, then the longest path prefix wins.
type Router struct {
	routes []*Route
}

// NewRouter sorts routes by specificity. Input order breaks ties.
func NewRouter(routes []*Route) *Router {
	rs := make([]*Route, len(routes))
	copy(rs, routes)
	for _, r := range rs {
		r.Host = strings.ToLower(r.Host)
		if r.Prefix != "/" {
			r.Prefix = strings.TrimRight(r.Prefix, "/")
		}
	}
	sort.SliceStable(rs, func(i, j int) bool {
		if a, b := rs[i].hostScore(), rs[j].hostScore(); a != b {
			return a > b
		}
		return len(rs[i].Prefix) > len(rs[j].Prefix)
	})
	return &Router{routes: rs}
}

// Match returns the route for a request. When a path matched but no route
// accepted the method, allowed lists the methods that would have matched.
func (rt *Router) Match(r *http.Request) (route *Route, allowed []string) {
	host := strings.ToLower(r.Host)
	if strings.IndexByte(host, ':') >= 0 { // avoid SplitHostPort's error allocation for bare hosts
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
	}
	for _, rr := range rt.routes {
		if !rr.matchHost(host) || !rr.matchPath(r.URL.Path) {
			continue
		}
		if rr.allowsMethod(r.Method) {
			return rr, nil
		}
		allowed = append(allowed, rr.Methods...)
	}
	return nil, allowed
}

// ServeHTTP dispatches to the matched route's handler.
func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	route, allowed := rt.Match(r)
	if route == nil {
		if len(allowed) > 0 {
			w.Header().Set("Allow", strings.Join(dedupe(allowed), ", "))
			httpx.WriteError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed for this route")
			return
		}
		httpx.WriteError(w, r, http.StatusNotFound, "route_not_found", "no route matches this request")
		return
	}
	httpx.GetInfo(r.Context()).Route = route.Name
	if route.StripPrefix && route.Prefix != "/" {
		r = stripPrefix(r, route.Prefix)
	}
	route.Handler.ServeHTTP(w, r)
}

func stripPrefix(r *http.Request, prefix string) *http.Request {
	r2 := new(http.Request)
	*r2 = *r
	u := *r.URL
	u.Path = ensureSlash(strings.TrimPrefix(u.Path, prefix))
	if u.RawPath != "" {
		u.RawPath = ensureSlash(strings.TrimPrefix(u.RawPath, prefix))
	}
	r2.URL = &u
	return r2
}

func ensureSlash(p string) string {
	if p == "" || p[0] != '/' {
		return "/" + p
	}
	return p
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
