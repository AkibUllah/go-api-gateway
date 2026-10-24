// Command gateway-demo runs the gateway together with three in-process echo
// upstreams on 127.0.0.1:9001-9003, so a single container (Render, a quick
// local try-out) shows routing, balancing and auth without docker compose.
// It accepts the same flags as the gateway; configs/gateway.yaml already
// points at those addresses by default.
package main

import (
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/AkibUllah/go-api-gateway/internal/app"
	"github.com/AkibUllah/go-api-gateway/internal/echo"
)

func main() {
	upstreams := []struct{ addr, service, instance string }{
		{"127.0.0.1:9001", "catalog", "catalog-1"},
		{"127.0.0.1:9002", "catalog", "catalog-2"},
		{"127.0.0.1:9003", "orders", "orders-1"},
	}
	for _, u := range upstreams {
		srv := &http.Server{Addr: u.addr, Handler: echo.Handler(u.service, u.instance), ReadHeaderTimeout: 5 * time.Second}
		go func(addr string) {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("demo upstream stopped", "addr", addr, "error", err)
				os.Exit(1)
			}
		}(u.addr)
	}
	os.Exit(app.Run(os.Args[1:]))
}
