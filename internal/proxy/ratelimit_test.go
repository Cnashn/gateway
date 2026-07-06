package proxy_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cnashn/gateway/internal/proxy"
	"github.com/cnashn/gateway/internal/ratelimit"
)

type fakeLimiter struct {
	decision ratelimit.Decision
	err      error
	gotRoute string
	gotKey   string
	calls    int
}

func (f *fakeLimiter) Allow(_ context.Context, route, key string) (ratelimit.Decision, error) {
	f.calls++
	f.gotRoute = route
	f.gotKey = key
	return f.decision, f.err
}

func serveRateLimited(t *testing.T, limiter ratelimit.Limiter, onFailOpen func(), headers map[string]string) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	handlerCalled := false
	h := proxy.RateLimit(limiter, "/api/orders/", discardLogger(), onFailOpen)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handlerCalled = true
			w.WriteHeader(http.StatusOK)
		}))

	req := httptest.NewRequest(http.MethodGet, "/api/orders/1", nil)
	req.RemoteAddr = "10.0.0.9:1234"
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, handlerCalled
}

func TestRateLimitAllowedSetsHeaders(t *testing.T) {
	limiter := &fakeLimiter{decision: ratelimit.Decision{
		Allowed:    true,
		Limit:      10,
		Remaining:  7,
		ResetAfter: 2500 * time.Millisecond,
	}}

	rec, handlerCalled := serveRateLimited(t, limiter, nil, nil)

	if !handlerCalled {
		t.Fatal("allowed request did not reach the handler")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	want := map[string]string{
		"X-RateLimit-Limit":     "10",
		"X-RateLimit-Remaining": "7",
		"X-RateLimit-Reset":     "3",
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("header %s = %q, want %q", k, got, v)
		}
	}
}

func TestRateLimitDeniedReturns429(t *testing.T) {
	limiter := &fakeLimiter{decision: ratelimit.Decision{
		Allowed:    false,
		Limit:      10,
		Remaining:  0,
		RetryAfter: 300 * time.Millisecond,
	}}

	rec, handlerCalled := serveRateLimited(t, limiter, nil, map[string]string{"X-Request-Id": "rid-1"})

	if handlerCalled {
		t.Fatal("denied request reached the handler")
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want %q (sub-second waits round up)", got, "1")
	}
	if got := rec.Header().Get("X-RateLimit-Remaining"); got != "0" {
		t.Errorf("X-RateLimit-Remaining = %q, want 0", got)
	}

	var payload map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("body %q is not JSON: %v", rec.Body.String(), err)
	}
	if payload["error"] != "rate_limited" || payload["request_id"] != "rid-1" {
		t.Errorf("body = %v, want error=rate_limited request_id=rid-1", payload)
	}
}

func TestRateLimitErrorFailsOpen(t *testing.T) {
	limiter := &fakeLimiter{err: errors.New("redis: connection refused")}
	failOpens := 0

	rec, handlerCalled := serveRateLimited(t, limiter, func() { failOpens++ }, nil)

	if !handlerCalled {
		t.Fatal("request was blocked on limiter error, want fail-open")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if failOpens != 1 {
		t.Errorf("fail-open counter = %d, want 1", failOpens)
	}
	if rec.Header().Get("X-RateLimit-Limit") != "" {
		t.Error("rate limit headers set on fail-open, want none")
	}
}

func TestRateLimitPassesRouteAndClientKey(t *testing.T) {
	limiter := &fakeLimiter{decision: ratelimit.Decision{Allowed: true, Limit: 10, Remaining: 9}}

	serveRateLimited(t, limiter, nil, map[string]string{"X-API-Key": "key-abc"})

	if limiter.gotRoute != "/api/orders/" {
		t.Errorf("route = %q, want /api/orders/", limiter.gotRoute)
	}
	if limiter.gotKey != "key-abc" {
		t.Errorf("client key = %q, want key-abc", limiter.gotKey)
	}
}

func TestRateLimitNilLimiterPassesThrough(t *testing.T) {
	rec, handlerCalled := serveRateLimited(t, nil, nil, nil)

	if !handlerCalled {
		t.Fatal("request did not reach the handler with nil limiter")
	}
	if rec.Header().Get("X-RateLimit-Limit") != "" {
		t.Error("rate limit headers set with nil limiter, want none")
	}
}
