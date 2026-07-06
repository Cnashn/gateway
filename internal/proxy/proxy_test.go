package proxy_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cnashn/gateway/internal/config"
	"github.com/cnashn/gateway/internal/proxy"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

type upstreamCall struct {
	name      string
	path      string
	requestID string
	xff       string
}

func echoUpstream(t *testing.T, name string, calls *[]upstreamCall) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			*calls = append(*calls, upstreamCall{
				name:      name,
				path:      r.URL.Path,
				requestID: r.Header.Get("X-Request-Id"),
				xff:       r.Header.Get("X-Forwarded-For"),
			})
		}
		fmt.Fprintf(w, "%s:%s", name, r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func gatewayConfig(ordersURL, usersURL string, timeout time.Duration) *config.Config {
	return &config.Config{
		Listen:   ":0",
		RedisURL: "redis://localhost:6379",
		Upstreams: []config.Upstream{
			{Name: "orders", URL: ordersURL, Timeout: timeout},
			{Name: "users", URL: usersURL, Timeout: timeout},
		},
		Routes: []config.Route{
			{PathPrefix: "/api/orders/", Upstream: "orders", RateLimit: config.RateLimit{RequestsPerSecond: 5, Burst: 10}},
			{PathPrefix: "/api/users/", Upstream: "users", RateLimit: config.RateLimit{RequestsPerSecond: 5, Burst: 10}},
		},
		Breaker: config.Breaker{FailureThreshold: 0.5, WindowSize: 100, OpenDuration: 30 * time.Second, HalfOpenMaxRequests: 3},
	}
}

func newGateway(t *testing.T, cfg *config.Config, logger *slog.Logger) *httptest.Server {
	t.Helper()
	mux, err := proxy.New(cfg, logger, nil)
	if err != nil {
		t.Fatalf("proxy.New() error = %v", err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, url string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(body)
}

func TestRoutingToConfiguredUpstream(t *testing.T) {
	var calls []upstreamCall
	orders := echoUpstream(t, "orders", &calls)
	users := echoUpstream(t, "users", &calls)
	gw := newGateway(t, gatewayConfig(orders.URL, users.URL, 2*time.Second), discardLogger())

	resp, body := get(t, gw.URL+"/api/orders/123", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if body != "orders:/api/orders/123" {
		t.Errorf("body = %q, want %q (path must be forwarded unstripped)", body, "orders:/api/orders/123")
	}

	resp, body = get(t, gw.URL+"/api/users/me", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if body != "users:/api/users/me" {
		t.Errorf("body = %q, want %q", body, "users:/api/users/me")
	}

	if len(calls) != 2 || calls[0].name != "orders" || calls[1].name != "users" {
		t.Errorf("upstream calls = %+v, want one to orders then one to users", calls)
	}
}

func TestUnmatchedPathReturns404(t *testing.T) {
	orders := echoUpstream(t, "orders", nil)
	users := echoUpstream(t, "users", nil)
	gw := newGateway(t, gatewayConfig(orders.URL, users.URL, 2*time.Second), discardLogger())

	resp, _ := get(t, gw.URL+"/api/payments/1", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestDeadUpstreamReturns502(t *testing.T) {
	orders := echoUpstream(t, "orders", nil)
	users := echoUpstream(t, "users", nil)
	deadURL := users.URL
	users.Close()
	gw := newGateway(t, gatewayConfig(orders.URL, deadURL, 2*time.Second), discardLogger())

	resp, body := get(t, gw.URL+"/api/users/me", nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}

	var payload map[string]string
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("body %q is not JSON: %v", body, err)
	}
	if payload["error"] != "bad_gateway" {
		t.Errorf("error = %q, want %q", payload["error"], "bad_gateway")
	}
	if payload["request_id"] == "" {
		t.Error("request_id missing from 502 body")
	}
}

func TestSlowUpstreamTimesOut(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
	}))
	t.Cleanup(slow.Close)
	orders := echoUpstream(t, "orders", nil)
	gw := newGateway(t, gatewayConfig(orders.URL, slow.URL, 50*time.Millisecond), discardLogger())

	resp, _ := get(t, gw.URL+"/api/users/me", nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 on upstream timeout", resp.StatusCode)
	}
}

func TestRequestIDPropagation(t *testing.T) {
	var calls []upstreamCall
	orders := echoUpstream(t, "orders", &calls)
	users := echoUpstream(t, "users", nil)
	gw := newGateway(t, gatewayConfig(orders.URL, users.URL, 2*time.Second), discardLogger())

	t.Run("generated when absent", func(t *testing.T) {
		calls = nil
		resp, _ := get(t, gw.URL+"/api/orders/1", nil)
		id := resp.Header.Get("X-Request-Id")
		if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(id) {
			t.Errorf("generated request id %q is not 32 hex chars", id)
		}
		if len(calls) != 1 || calls[0].requestID != id {
			t.Errorf("upstream saw request id %q, response header has %q", calls[0].requestID, id)
		}
	})

	t.Run("preserved when present", func(t *testing.T) {
		calls = nil
		resp, _ := get(t, gw.URL+"/api/orders/1", map[string]string{"X-Request-Id": "client-id-42"})
		if got := resp.Header.Get("X-Request-Id"); got != "client-id-42" {
			t.Errorf("response request id = %q, want client-id-42", got)
		}
		if len(calls) != 1 || calls[0].requestID != "client-id-42" {
			t.Errorf("upstream saw request id %q, want client-id-42", calls[0].requestID)
		}
	})
}

func TestXForwardedForAppended(t *testing.T) {
	var calls []upstreamCall
	orders := echoUpstream(t, "orders", &calls)
	users := echoUpstream(t, "users", nil)
	gw := newGateway(t, gatewayConfig(orders.URL, users.URL, 2*time.Second), discardLogger())

	get(t, gw.URL+"/api/orders/1", map[string]string{"X-Forwarded-For": "203.0.113.7"})
	if len(calls) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(calls))
	}
	if !strings.HasPrefix(calls[0].xff, "203.0.113.7, ") {
		t.Errorf("X-Forwarded-For = %q, want existing entry preserved with client IP appended", calls[0].xff)
	}
}

func TestMiddlewareChainOrder(t *testing.T) {
	var order []string
	record := func(name string) proxy.Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		order = append(order, "handler")
	})

	h := proxy.Chain(final, record("recovery"), record("request-id"), record("logging"), record("ratelimit"), record("breaker"))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	want := []string{"recovery", "request-id", "logging", "ratelimit", "breaker", "handler"}
	if !slices.Equal(order, want) {
		t.Errorf("middleware order = %v, want %v", order, want)
	}
}

func TestRecoveryMiddleware(t *testing.T) {
	h := proxy.Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}), proxy.Recovery(discardLogger()))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	var payload map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("body %q is not JSON: %v", rec.Body.String(), err)
	}
	if payload["error"] != "internal_error" {
		t.Errorf("error = %q, want internal_error", payload["error"])
	}
}

func TestRecoveryRepanicsOnErrAbortHandler(t *testing.T) {
	h := proxy.Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	}), proxy.Recovery(discardLogger()))

	defer func() {
		if recover() != http.ErrAbortHandler {
			t.Error("http.ErrAbortHandler was swallowed, want it re-panicked")
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	t.Error("expected panic")
}

func TestClientKey(t *testing.T) {
	tests := []struct {
		name       string
		apiKey     string
		xff        string
		remoteAddr string
		want       string
	}{
		{name: "api key wins", apiKey: "key-1", xff: "1.2.3.4", remoteAddr: "5.6.7.8:1234", want: "key-1"},
		{name: "first xff entry", xff: "1.2.3.4, 9.9.9.9", remoteAddr: "5.6.7.8:1234", want: "1.2.3.4"},
		{name: "remote addr fallback", remoteAddr: "5.6.7.8:1234", want: "5.6.7.8"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tt.remoteAddr
			if tt.apiKey != "" {
				r.Header.Set("X-API-Key", tt.apiKey)
			}
			if tt.xff != "" {
				r.Header.Set("X-Forwarded-For", tt.xff)
			}
			if got := proxy.ClientKey(r); got != tt.want {
				t.Errorf("ClientKey() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRequestLogFields(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	orders := echoUpstream(t, "orders", nil)
	users := echoUpstream(t, "users", nil)
	gw := newGateway(t, gatewayConfig(orders.URL, users.URL, 2*time.Second), logger)

	get(t, gw.URL+"/api/orders/42", map[string]string{"X-API-Key": "test-key"})

	var line map[string]any
	found := false
	for _, raw := range bytes.Split(buf.Bytes(), []byte("\n")) {
		if len(raw) == 0 {
			continue
		}
		if err := json.Unmarshal(raw, &line); err != nil {
			t.Fatalf("log line %q is not JSON: %v", raw, err)
		}
		if line["msg"] == "request" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no request log line in output: %s", buf.String())
	}

	want := map[string]any{
		"method":     "GET",
		"path":       "/api/orders/42",
		"route":      "/api/orders/",
		"upstream":   "orders",
		"status":     float64(200),
		"client_key": "test-key",
	}
	for k, v := range want {
		if line[k] != v {
			t.Errorf("log field %s = %v, want %v", k, line[k], v)
		}
	}
	for _, k := range []string{"duration_ms", "bytes", "request_id"} {
		if _, ok := line[k]; !ok {
			t.Errorf("log line missing field %s", k)
		}
	}
}
