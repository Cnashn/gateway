package proxy_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/cnashn/gateway/internal/breaker"
	"github.com/cnashn/gateway/internal/observability"
	"github.com/cnashn/gateway/internal/proxy"
	"github.com/cnashn/gateway/internal/ratelimit"
)

func TestMetricsMiddlewareCountsRequests(t *testing.T) {
	m := observability.NewMetrics()
	h := proxy.Metrics(m, "/api/orders/", "orders")(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
		}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/orders/1", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/orders/1", nil))

	got := testutil.ToFloat64(m.RequestsTotal.WithLabelValues("/api/orders/", "orders", "4xx"))
	if got != 2 {
		t.Errorf("requests_total{4xx} = %v, want 2", got)
	}
	if n := testutil.CollectAndCount(m.RequestDuration); n != 1 {
		t.Errorf("request_duration series = %d, want 1", n)
	}
	if inflight := testutil.ToFloat64(m.InflightRequests); inflight != 0 {
		t.Errorf("inflight after requests done = %v, want 0", inflight)
	}
}

func TestFailOpenIncrementsDecisionCounter(t *testing.T) {
	m := observability.NewMetrics()
	limiter := &fakeLimiter{err: errors.New("redis down")}

	h := proxy.RateLimit(limiter, "/api/orders/", discardLogger(),
		func(decision string) { m.OnRatelimitDecision("/api/orders/", decision) })(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/orders/1", nil))

	got := testutil.ToFloat64(m.RatelimitDecisions.WithLabelValues("/api/orders/", "failopen"))
	if got != 1 {
		t.Errorf("ratelimit_decisions_total{failopen} = %v, want 1", got)
	}
}

func TestDeniedIncrementsDecisionCounter(t *testing.T) {
	m := observability.NewMetrics()
	limiter := &fakeLimiter{decision: ratelimit.Decision{Allowed: false, Limit: 5, RetryAfter: time.Second}}

	h := proxy.RateLimit(limiter, "/api/orders/", discardLogger(),
		func(decision string) { m.OnRatelimitDecision("/api/orders/", decision) })(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/orders/1", nil))

	got := testutil.ToFloat64(m.RatelimitDecisions.WithLabelValues("/api/orders/", "denied"))
	if got != 1 {
		t.Errorf("ratelimit_decisions_total{denied} = %v, want 1", got)
	}
}

func TestBreakerStateChangeSetsGauge(t *testing.T) {
	m := observability.NewMetrics()

	m.OnBreakerStateChange("orders", breaker.Closed, breaker.Open)
	if got := testutil.ToFloat64(m.BreakerState.WithLabelValues("orders")); got != 2 {
		t.Errorf("breaker_state after open = %v, want 2", got)
	}

	m.OnBreakerStateChange("orders", breaker.Open, breaker.HalfOpen)
	if got := testutil.ToFloat64(m.BreakerState.WithLabelValues("orders")); got != 1 {
		t.Errorf("breaker_state after half-open = %v, want 1", got)
	}
}
