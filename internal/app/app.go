// Package app is the gateway's command-line entry point, shared by
// cmd/gateway and the all-in-one cmd/gateway-demo.
package app

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/AkibUllah/go-api-gateway/internal/config"
	"github.com/AkibUllah/go-api-gateway/internal/gateway"
)

// Version is set at build time with -ldflags "-X .../internal/app.Version=...".
var Version = "dev"

// Run parses args (without the program name) and runs the gateway until
// SIGINT/SIGTERM. SIGHUP reloads the config file. It returns the exit code.
func Run(args []string) int {
	fs := flag.NewFlagSet("gateway", flag.ContinueOnError)
	cfgPath := fs.String("config", envOr("GATEWAY_CONFIG", "configs/gateway.yaml"), "path to the YAML config")
	check := fs.Bool("check", false, "validate the config and exit")
	probe := fs.String("healthcheck", "", "GET this URL, exit 0 on 2xx (container health checks)")
	drain := fs.Duration("drain-delay", durationEnv("GATEWAY_DRAIN_DELAY", 0), "keep serving with /readyz failing for this long before shutdown")
	showVersion := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	switch {
	case *showVersion:
		fmt.Println(Version)
		return 0
	case *probe != "":
		return healthcheck(*probe)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if *check {
		fmt.Printf("config OK: %d routes, %d upstreams\n", len(cfg.Routes), len(cfg.Upstreams))
		return 0
	}

	level := new(slog.LevelVar)
	log := gateway.NewLogger(cfg.Logging, os.Stdout, level)
	slog.SetDefault(log)

	gw, err := gateway.New(cfg, log, nil)
	if err != nil {
		log.Error("build gateway", "error", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var lc net.ListenConfig
	public, err := lc.Listen(ctx, "tcp", cfg.Server.Listen)
	if err != nil {
		log.Error("listen", "addr", cfg.Server.Listen, "error", err)
		return 1
	}
	var admin net.Listener
	if cfg.Admin.Listen != "" {
		if admin, err = lc.Listen(ctx, "tcp", cfg.Admin.Listen); err != nil {
			log.Error("listen admin", "addr", cfg.Admin.Listen, "error", err)
			return 1
		}
	}

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	reload := make(chan struct{}, 1)
	go func() {
		for range hup {
			select {
			case reload <- struct{}{}:
			default: // a reload is already pending
			}
		}
	}()

	log.Info("gateway starting", "version", Version, "config", *cfgPath,
		"routes", len(cfg.Routes), "upstreams", len(cfg.Upstreams))
	err = gw.Serve(ctx, gateway.ServeOptions{
		Public:     public,
		Admin:      admin,
		Reload:     reload,
		Load:       func() (*config.Config, error) { return config.Load(*cfgPath) },
		OnReload:   func(c *config.Config) { gateway.SetLevel(level, c.Logging.Level) },
		DrainDelay: *drain,
	})
	if err != nil {
		log.Error("server stopped with error", "error", err)
		return 1
	}
	return 0
}

func healthcheck(url string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		fmt.Fprintln(os.Stderr, "status", resp.StatusCode)
		return 1
	}
	return 0
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func durationEnv(key string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(key)); err == nil {
		return d
	}
	return def
}
