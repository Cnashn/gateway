package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validYAML = `
listen: ":8080"
redis_url: "redis://localhost:6379"
upstreams:
  - name: orders
    url: "http://orders:8081"
    timeout: 5s
  - name: users
    url: "http://users:8082"
routes:
  - path_prefix: /api/orders/
    upstream: orders
    rate_limit:
      requests_per_second: 5
      burst: 10
  - path_prefix: /api/users/
    upstream: users
    rate_limit:
      requests_per_second: 10
      burst: 20
breaker:
  failure_threshold: 0.5
  open_duration: 30s
  half_open_max_requests: 3
`

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadValid(t *testing.T) {
	cfg, err := Load(writeConfig(t, validYAML))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Listen != ":8080" {
		t.Errorf("Listen = %q, want %q", cfg.Listen, ":8080")
	}
	if cfg.RedisURL != "redis://localhost:6379" {
		t.Errorf("RedisURL = %q, want %q", cfg.RedisURL, "redis://localhost:6379")
	}
	if len(cfg.Upstreams) != 2 {
		t.Fatalf("len(Upstreams) = %d, want 2", len(cfg.Upstreams))
	}
	if cfg.Upstreams[0].Name != "orders" || cfg.Upstreams[0].URL != "http://orders:8081" {
		t.Errorf("Upstreams[0] = %+v", cfg.Upstreams[0])
	}
	if cfg.Upstreams[0].Timeout != 5*time.Second {
		t.Errorf("Upstreams[0].Timeout = %v, want 5s", cfg.Upstreams[0].Timeout)
	}
	if cfg.Upstreams[1].Timeout != 10*time.Second {
		t.Errorf("Upstreams[1].Timeout = %v, want 10s default", cfg.Upstreams[1].Timeout)
	}
	if len(cfg.Routes) != 2 {
		t.Fatalf("len(Routes) = %d, want 2", len(cfg.Routes))
	}
	r := cfg.Routes[0]
	if r.PathPrefix != "/api/orders/" || r.Upstream != "orders" || r.RateLimit.RequestsPerSecond != 5 || r.RateLimit.Burst != 10 {
		t.Errorf("Routes[0] = %+v", r)
	}
	if cfg.Breaker.FailureThreshold != 0.5 || cfg.Breaker.OpenDuration != 30*time.Second || cfg.Breaker.HalfOpenMaxRequests != 3 {
		t.Errorf("Breaker = %+v", cfg.Breaker)
	}
}

func TestEnvOverrides(t *testing.T) {
	tests := []struct {
		name       string
		env        map[string]string
		wantListen string
		wantRedis  string
	}{
		{
			name:       "no overrides",
			env:        nil,
			wantListen: ":8080",
			wantRedis:  "redis://localhost:6379",
		},
		{
			name:       "listen override",
			env:        map[string]string{"GATEWAY_LISTEN": ":9999"},
			wantListen: ":9999",
			wantRedis:  "redis://localhost:6379",
		},
		{
			name:       "redis override",
			env:        map[string]string{"GATEWAY_REDIS_URL": "redis://other:6380"},
			wantListen: ":8080",
			wantRedis:  "redis://other:6380",
		},
		{
			name: "both overrides",
			env: map[string]string{
				"GATEWAY_LISTEN":    ":7777",
				"GATEWAY_REDIS_URL": "rediss://upstash:6379",
			},
			wantListen: ":7777",
			wantRedis:  "rediss://upstash:6379",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			cfg, err := Load(writeConfig(t, validYAML))
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if cfg.Listen != tt.wantListen {
				t.Errorf("Listen = %q, want %q", cfg.Listen, tt.wantListen)
			}
			if cfg.RedisURL != tt.wantRedis {
				t.Errorf("RedisURL = %q, want %q", cfg.RedisURL, tt.wantRedis)
			}
		})
	}
}

func TestLoadInvalid(t *testing.T) {
	tests := []struct {
		name     string
		yaml     string
		wantErrs []string
	}{
		{
			name: "missing listen and redis",
			yaml: `
upstreams:
  - name: orders
    url: "http://orders:8081"
routes:
  - path_prefix: /api/orders/
    upstream: orders
    rate_limit:
      requests_per_second: 5
      burst: 10
breaker:
  failure_threshold: 0.5
  open_duration: 30s
  half_open_max_requests: 3
`,
			wantErrs: []string{"listen: must not be empty", "redis_url: must not be empty"},
		},
		{
			name: "route references unknown upstream",
			yaml: `
listen: ":8080"
redis_url: "redis://localhost:6379"
upstreams:
  - name: orders
    url: "http://orders:8081"
routes:
  - path_prefix: /api/users/
    upstream: users
    rate_limit:
      requests_per_second: 5
      burst: 10
breaker:
  failure_threshold: 0.5
  open_duration: 30s
  half_open_max_requests: 3
`,
			wantErrs: []string{`routes[0].upstream: "users" does not match any configured upstream`},
		},
		{
			name: "bad upstream url and bad rate limit",
			yaml: `
listen: ":8080"
redis_url: "redis://localhost:6379"
upstreams:
  - name: orders
    url: "not-a-url"
routes:
  - path_prefix: /api/orders/
    upstream: orders
    rate_limit:
      requests_per_second: 0
      burst: -1
breaker:
  failure_threshold: 0.5
  open_duration: 30s
  half_open_max_requests: 3
`,
			wantErrs: []string{
				`upstreams[0].url: "not-a-url" is not a valid absolute URL`,
				"routes[0].rate_limit.requests_per_second: must be greater than 0",
				"routes[0].rate_limit.burst: must be greater than 0",
			},
		},
		{
			name: "duplicate upstream names and path prefix without slash",
			yaml: `
listen: ":8080"
redis_url: "redis://localhost:6379"
upstreams:
  - name: orders
    url: "http://a:8081"
  - name: orders
    url: "http://b:8082"
routes:
  - path_prefix: api/orders/
    upstream: orders
    rate_limit:
      requests_per_second: 5
      burst: 10
breaker:
  failure_threshold: 0.5
  open_duration: 30s
  half_open_max_requests: 3
`,
			wantErrs: []string{
				`upstreams[1].name: duplicate name "orders"`,
				`routes[0].path_prefix: "api/orders/" must start with /`,
			},
		},
		{
			name: "invalid breaker settings",
			yaml: `
listen: ":8080"
redis_url: "redis://localhost:6379"
upstreams:
  - name: orders
    url: "http://orders:8081"
routes:
  - path_prefix: /api/orders/
    upstream: orders
    rate_limit:
      requests_per_second: 5
      burst: 10
breaker:
  failure_threshold: 1.5
  open_duration: 0s
  half_open_max_requests: 0
`,
			wantErrs: []string{
				"breaker.failure_threshold: must be in (0, 1]",
				"breaker.open_duration: must be greater than 0",
				"breaker.half_open_max_requests: must be greater than 0",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tt.yaml))
			if err == nil {
				t.Fatal("Load() succeeded, want validation error")
			}
			for _, want := range tt.wantErrs {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error missing %q\ngot: %v", want, err)
				}
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("Load() succeeded, want error for missing file")
	}
}

func TestLoadUnknownField(t *testing.T) {
	if _, err := Load(writeConfig(t, validYAML+"\nbogus_field: true\n")); err == nil {
		t.Fatal("Load() succeeded, want error for unknown field")
	}
}
