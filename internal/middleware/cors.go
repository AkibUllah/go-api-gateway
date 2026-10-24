package middleware

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/AkibUllah/go-api-gateway/internal/config"
)

// CORS answers preflight requests itself and decorates actual responses.
// Origins may be exact ("https://shop.example.com"), a subdomain wildcard
// ("https://*.example.com") or "*". With credentials enabled the matched
// origin is echoed instead of "*", as browsers require.
func CORS(p config.CORSPolicy) Middleware {
	methods := strings.Join(p.AllowedMethods, ", ")
	headers := strings.Join(p.AllowedHeaders, ", ")
	exposed := strings.Join(p.ExposedHeaders, ", ")
	maxAge := ""
	if p.MaxAge > 0 {
		maxAge = strconv.Itoa(int(p.MaxAge.D().Seconds()))
	}
	anyOrigin := false
	for _, o := range p.AllowedOrigins {
		if o == "*" {
			anyOrigin = true
		}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}
			h := w.Header()
			h.Add("Vary", "Origin")
			allowed := anyOrigin || originAllowed(origin, p.AllowedOrigins)
			preflight := r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""

			if !allowed {
				if preflight {
					// Answer without CORS headers; the browser blocks the call.
					w.WriteHeader(http.StatusNoContent)
					return
				}
				next.ServeHTTP(w, r)
				return
			}
			if anyOrigin && !p.AllowCredentials {
				h.Set("Access-Control-Allow-Origin", "*")
			} else {
				h.Set("Access-Control-Allow-Origin", origin)
			}
			if p.AllowCredentials {
				h.Set("Access-Control-Allow-Credentials", "true")
			}
			if preflight {
				h.Add("Vary", "Access-Control-Request-Method")
				h.Add("Vary", "Access-Control-Request-Headers")
				h.Set("Access-Control-Allow-Methods", methods)
				h.Set("Access-Control-Allow-Headers", headers)
				if maxAge != "" {
					h.Set("Access-Control-Max-Age", maxAge)
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if exposed != "" {
				h.Set("Access-Control-Expose-Headers", exposed)
			}
			next.ServeHTTP(w, r)
		})
	}
}

func originAllowed(origin string, allowed []string) bool {
	for _, a := range allowed {
		if strings.EqualFold(a, origin) {
			return true
		}
		// "https://*.example.com" matches "https://api.example.com" but not
		// "https://example.com" or "https://evil-example.com".
		if scheme, rest, ok := strings.Cut(a, "://*."); ok {
			prefix := scheme + "://"
			if strings.HasPrefix(origin, prefix) {
				host := origin[len(prefix):]
				if strings.HasSuffix(host, "."+rest) && len(host) > len(rest)+1 {
					return true
				}
			}
		}
	}
	return false
}
