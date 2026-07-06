// Package observability holds the Prometheus registry and metric
// definitions. The ratelimit and breaker packages stay dependency-clean:
// they report through callbacks wired up here instead of importing
// prometheus themselves.
package observability

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/cnashn/gateway/internal/breaker"
)

type Metrics struct {
	registry *prometheus.Registry

	RequestsTotal      *prometheus.CounterVec
	RequestDuration    *prometheus.HistogramVec
	RatelimitDecisions *prometheus.CounterVec
	BreakerState       *prometheus.GaugeVec
	RedisOpDuration    prometheus.Histogram
	InflightRequests   prometheus.Gauge
}

func NewMetrics() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		RequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_requests_total",
			Help: "Requests handled, by route, upstream and status class.",
		}, []string{"route", "upstream", "status_class"}),
		RequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gateway_request_duration_seconds",
			Help:    "End-to-end request duration through the gateway.",
			Buckets: prometheus.ExponentialBucketsRange(0.001, 10, 12),
		}, []string{"route", "upstream"}),
		RatelimitDecisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_ratelimit_decisions_total",
			Help: "Rate limit outcomes per route: allowed, denied or failopen.",
		}, []string{"route", "decision"}),
		BreakerState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gateway_breaker_state",
			Help: "Circuit breaker state per upstream: 0 closed, 1 half-open, 2 open.",
		}, []string{"upstream"}),
		RedisOpDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "gateway_redis_op_duration_seconds",
			Help:    "Latency of rate limiter Redis script calls.",
			Buckets: prometheus.ExponentialBucketsRange(0.0001, 1, 12),
		}),
		InflightRequests: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gateway_inflight_requests",
			Help: "Requests currently being handled.",
		}),
	}

	m.registry.MustRegister(
		m.RequestsTotal,
		m.RequestDuration,
		m.RatelimitDecisions,
		m.BreakerState,
		m.RedisOpDuration,
		m.InflightRequests,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

func (m *Metrics) ObserveRedisOp(d time.Duration) {
	m.RedisOpDuration.Observe(d.Seconds())
}

func (m *Metrics) OnBreakerStateChange(upstream string, _, to breaker.State) {
	m.BreakerState.WithLabelValues(upstream).Set(float64(to))
}

func (m *Metrics) OnRatelimitDecision(route, decision string) {
	m.RatelimitDecisions.WithLabelValues(route, decision).Inc()
}
