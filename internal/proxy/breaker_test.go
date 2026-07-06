package proxy_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnashn/gateway/internal/breaker"
	"github.com/cnashn/gateway/internal/config"
	"github.com/cnashn/gateway/internal/proxy"
)

func breakerGatewayConfig(upstreamURL string) *config.Config {
	return &config.Config{
		Listen:   ":0",
		RedisURL: "redis://localhost:6379",
		Upstreams: []config.Upstream{
			{Name: "orders", URL: upstreamURL, Timeout: 2 * time.Second},
		},
		Routes: []config.Route{
			{PathPrefix: "/api/orders/", Upstream: "orders", RateLimit: config.RateLimit{RequestsPerSecond: 1000, Burst: 1000}},
		},
		Breaker: config.Breaker{FailureThreshold: 0.5, WindowSize: 4, OpenDuration: 30 * time.Second, HalfOpenMaxRequests: 2},
	}
}

func TestBreakerOpensAndShortCircuits(t *testing.T) {
	var hits atomic.Int64
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(failing.Close)

	gw := newGateway(t, breakerGatewayConfig(failing.URL), discardLogger())

	for i := 0; i < 4; i++ {
		resp, _ := get(t, gw.URL+"/api/orders/1", nil)
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("request %d: status = %d, want 500 passed through while closed", i+1, resp.StatusCode)
		}
	}

	resp, body := get(t, gw.URL+"/api/orders/1", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 short-circuit after breaker opened", resp.StatusCode)
	}
	if got := hits.Load(); got != 4 {
		t.Errorf("upstream hits = %d, want 4: open breaker must not call the upstream", got)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("503 short-circuit missing Retry-After header")
	}

	var payload map[string]string
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("body %q is not JSON: %v", body, err)
	}
	if payload["error"] != "upstream_unavailable" || payload["upstream"] != "orders" {
		t.Errorf("body = %v, want error=upstream_unavailable upstream=orders", payload)
	}
}

func TestClientErrorsDoNotTripBreaker(t *testing.T) {
	var hits atomic.Int64
	notFound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(notFound.Close)

	gw := newGateway(t, breakerGatewayConfig(notFound.URL), discardLogger())

	for i := 0; i < 10; i++ {
		resp, _ := get(t, gw.URL+"/api/orders/1", nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("request %d: status = %d, want 404 passed through: 4xx must not trip the breaker", i+1, resp.StatusCode)
		}
	}
	if hits.Load() != 10 {
		t.Errorf("upstream hits = %d, want 10", hits.Load())
	}
}

func TestTransportErrorsTripBreaker(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	gw := newGateway(t, breakerGatewayConfig(deadURL), discardLogger())

	for i := 0; i < 4; i++ {
		resp, _ := get(t, gw.URL+"/api/orders/1", nil)
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("request %d: status = %d, want 502 while closed", i+1, resp.StatusCode)
		}
	}
	resp, _ := get(t, gw.URL+"/api/orders/1", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503: transport errors must count as failures", resp.StatusCode)
	}
}

type fakeBreaker struct {
	allow       bool
	retryAfter  time.Duration
	gotOutcomes []bool
}

func (f *fakeBreaker) Allow() (func(bool), error) {
	if !f.allow {
		return nil, &breaker.OpenError{RetryAfter: f.retryAfter}
	}
	return func(success bool) { f.gotOutcomes = append(f.gotOutcomes, success) }, nil
}

func (f *fakeBreaker) State() breaker.State {
	if f.allow {
		return breaker.Closed
	}
	return breaker.Open
}

func TestCircuitBreakReportsOutcome(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		wantSuccess bool
	}{
		{name: "2xx is success", status: http.StatusOK, wantSuccess: true},
		{name: "4xx is success", status: http.StatusTeapot, wantSuccess: true},
		{name: "5xx is failure", status: http.StatusBadGateway, wantSuccess: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fb := &fakeBreaker{allow: true}
			h := proxy.CircuitBreak(fb, "orders")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
			}))
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

			if len(fb.gotOutcomes) != 1 || fb.gotOutcomes[0] != tt.wantSuccess {
				t.Errorf("outcomes = %v, want [%v]", fb.gotOutcomes, tt.wantSuccess)
			}
		})
	}
}
