package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/cnashn/gateway/internal/config"
)

func TestStartDemoUpstreams(t *testing.T) {
	cfg := &config.Config{
		Upstreams: []config.Upstream{
			{Name: "orders", URL: "http://orders:8081"},
			{Name: "users", URL: "http://users:8082"},
		},
	}
	logger := slog.New(slog.NewTextHandler(&strings.Builder{}, nil))

	if err := startDemoUpstreams(cfg, logger); err != nil {
		t.Fatalf("startDemoUpstreams() error = %v", err)
	}

	for _, u := range cfg.Upstreams {
		if !strings.HasPrefix(u.URL, "http://127.0.0.1:") {
			t.Fatalf("upstream %q URL = %q, want rewritten to loopback", u.Name, u.URL)
		}
		resp, err := http.Get(u.URL + "/some/path")
		if err != nil {
			t.Fatalf("GET %s: %v", u.URL, err)
		}
		defer resp.Body.Close()

		var body map[string]string
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decoding response from %q: %v", u.Name, err)
		}
		if body["upstream"] != u.Name || body["path"] != "/some/path" {
			t.Errorf("response from %q = %v", u.Name, body)
		}
	}
}
