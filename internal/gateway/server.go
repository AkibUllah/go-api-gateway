package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/AkibUllah/go-api-gateway/internal/config"
)

// NewLogger returns a slog logger for the logging config. The level is held
// in lv so a reload can change verbosity without rebuilding handlers.
func NewLogger(cfg config.Logging, w io.Writer, lv *slog.LevelVar) *slog.Logger {
	SetLevel(lv, cfg.Level)
	opts := &slog.HandlerOptions{Level: lv}
	if cfg.Format == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

// SetLevel parses a level name into lv (unknown names leave lv unchanged).
func SetLevel(lv *slog.LevelVar, name string) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(strings.ToUpper(name))); err == nil {
		lv.Set(l)
	}
}

// ServeOptions configures Serve.
type ServeOptions struct {
	Public net.Listener
	// Admin may be nil when admin endpoints are served on the public listener.
	Admin net.Listener
	// Reload receives a value for every reload request (SIGHUP in main).
	Reload <-chan struct{}
	// Load reads the configuration again for a reload.
	Load func() (*config.Config, error)
	// OnReload is called after a successful reload (e.g. to update log level).
	OnReload func(*config.Config)
	// DrainDelay keeps serving with /readyz failing before closing listeners,
	// giving load balancers time to notice.
	DrainDelay time.Duration
}

// Serve runs the HTTP servers until ctx is cancelled, then shuts down
// gracefully: readiness fails, listeners close, in-flight requests get up to
// server.shutdown_timeout to finish.
func (g *Gateway) Serve(ctx context.Context, o ServeOptions) error {
	cfg := g.Config()
	newServer := func(h http.Handler) *http.Server {
		return &http.Server{
			Handler:           h,
			ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.D(),
			ReadTimeout:       cfg.Server.ReadTimeout.D(),
			WriteTimeout:      cfg.Server.WriteTimeout.D(),
			IdleTimeout:       cfg.Server.IdleTimeout.D(),
			ErrorLog:          slog.NewLogLogger(g.log.Handler(), slog.LevelWarn),
		}
	}
	servers := []*http.Server{newServer(g)}
	listeners := []net.Listener{o.Public}
	if o.Admin != nil {
		servers = append(servers, newServer(g.AdminHandler()))
		listeners = append(listeners, o.Admin)
	}

	errc := make(chan error, len(servers))
	for i, s := range servers {
		g.log.Info("listening", "addr", listeners[i].Addr().String(), "admin", i == 1)
		go func(s *http.Server, ln net.Listener) {
			if err := s.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}(s, listeners[i])
	}

loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case err := <-errc:
			_ = g.shutdown(servers, cfg.Server.ShutdownTimeout.D())
			return err
		case <-o.Reload:
			g.reload(o)
		}
	}

	g.log.Info("shutting down", "drain_delay", o.DrainDelay.String())
	g.SetDraining(true)
	if o.DrainDelay > 0 {
		time.Sleep(o.DrainDelay)
	}
	return g.shutdown(servers, g.Config().Server.ShutdownTimeout.D())
}

func (g *Gateway) reload(o ServeOptions) {
	if o.Load == nil {
		return
	}
	cfg, err := o.Load()
	if err == nil {
		err = g.Reload(cfg)
	} else {
		g.metrics.ConfigReloads.WithLabelValues("error").Inc()
	}
	if err != nil {
		g.log.Error("config reload failed; keeping current config", "error", err)
		return
	}
	if o.OnReload != nil {
		o.OnReload(cfg)
	}
	g.log.Info("config reloaded", "routes", len(cfg.Routes), "upstreams", len(cfg.Upstreams))
}

func (g *Gateway) shutdown(servers []*http.Server, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var errs []error
	for _, s := range servers {
		if err := s.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	g.Close()
	if err := errors.Join(errs...); err != nil {
		g.log.Error("shutdown incomplete", "error", err)
		return err
	}
	g.log.Info("shutdown complete")
	return nil
}
