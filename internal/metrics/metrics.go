// Package metrics defines the gateway's Prometheus collectors.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/AkibUllah/go-api-gateway/internal/breaker"
)

// Metrics groups every collector. It is created once per process and survives
// config reloads so counters are not reset.
type Metrics struct {
	Registry *prometheus.Registry

	Requests         *prometheus.CounterVec
	Duration         *prometheus.HistogramVec
	InFlight         prometheus.Gauge
	UpstreamRequests *prometheus.CounterVec
	Retries          *prometheus.CounterVec
	RateLimited      *prometheus.CounterVec
	AuthFailures     *prometheus.CounterVec
	TargetHealthy    *prometheus.GaugeVec
	CircuitState     *prometheus.GaugeVec
	ConfigReloads    *prometheus.CounterVec
}

// New registers all collectors on a fresh registry (plus Go and process collectors).
func New() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		Registry: reg,
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_requests_total",
			Help: "Requests handled, by route, method and status code.",
		}, []string{"route", "method", "code"}),
		Duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gateway_request_duration_seconds",
			Help:    "End-to-end request latency including upstream time.",
			Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}, []string{"route"}),
		InFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gateway_in_flight_requests",
			Help: "Requests currently being served.",
		}),
		UpstreamRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_upstream_requests_total",
			Help: "Upstream attempts by pool, target and outcome (2xx..5xx, error, circuit_open).",
		}, []string{"upstream", "target", "outcome"}),
		Retries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_upstream_retries_total",
			Help: "Retries of idempotent requests on another target.",
		}, []string{"upstream"}),
		RateLimited: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_rate_limited_total",
			Help: "Requests rejected by a rate limit policy.",
		}, []string{"route", "policy"}),
		AuthFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_auth_failures_total",
			Help: "Requests rejected by JWT or API key authentication.",
		}, []string{"route", "method"}),
		TargetHealthy: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gateway_upstream_target_healthy",
			Help: "1 when the target (host:port) passes active health checks.",
		}, []string{"upstream", "target"}),
		CircuitState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gateway_circuit_state",
			Help: "Circuit breaker state per target: 0 closed, 1 open, 2 half-open.",
		}, []string{"upstream", "target"}),
		ConfigReloads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_config_reloads_total",
			Help: "Configuration reload attempts by result.",
		}, []string{"result"}),
	}
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.Requests, m.Duration, m.InFlight, m.UpstreamRequests, m.Retries,
		m.RateLimited, m.AuthFailures, m.TargetHealthy, m.CircuitState, m.ConfigReloads,
	)
	return m
}

// TargetHealth implements upstream.Observer.
func (m *Metrics) TargetHealth(pool, target string, healthy bool) {
	v := 0.0
	if healthy {
		v = 1
	}
	m.TargetHealthy.WithLabelValues(pool, target).Set(v)
}

// BreakerState implements upstream.Observer.
func (m *Metrics) BreakerState(pool, target string, s breaker.State) {
	m.CircuitState.WithLabelValues(pool, target).Set(float64(s))
}
