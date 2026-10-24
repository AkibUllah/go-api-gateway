// Command echo-upstream is a minimal demo upstream that reflects each request as JSON.
// Configure with SERVICE_NAME, INSTANCE and PORT.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/AkibUllah/go-api-gateway/internal/echo"
)

func main() {
	service := envOr("SERVICE_NAME", "echo")
	instance := envOr("INSTANCE", hostname())
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", service, "instance", instance)
	if err := run(":"+envOr("PORT", "9001"), echo.Handler(service, instance), log); err != nil {
		log.Error("serve", "error", err)
		os.Exit(1)
	}
}

func run(addr string, h http.Handler, log *slog.Logger) error {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Info("echo upstream listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "local"
	}
	return h
}
