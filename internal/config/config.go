// Package config loads and validates the gateway configuration from YAML,
// with environment variable overrides for deployment-specific values.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen    string     `yaml:"listen"`
	RedisURL  string     `yaml:"redis_url"`
	Upstreams []Upstream `yaml:"upstreams"`
	Routes    []Route    `yaml:"routes"`
	Breaker   Breaker    `yaml:"breaker"`
}

type Upstream struct {
	Name    string        `yaml:"name"`
	URL     string        `yaml:"url"`
	Timeout time.Duration `yaml:"timeout"`
}

const defaultUpstreamTimeout = 10 * time.Second

type Route struct {
	PathPrefix string    `yaml:"path_prefix"`
	Upstream   string    `yaml:"upstream"`
	RateLimit  RateLimit `yaml:"rate_limit"`
}

type RateLimit struct {
	RequestsPerSecond float64 `yaml:"requests_per_second"`
	Burst             int     `yaml:"burst"`
}

type Breaker struct {
	FailureThreshold    float64       `yaml:"failure_threshold"`
	OpenDuration        time.Duration `yaml:"open_duration"`
	HalfOpenMaxRequests int           `yaml:"half_open_max_requests"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parsing config file %s: %w", path, err)
	}

	applyEnvOverrides(&cfg)
	for i := range cfg.Upstreams {
		if cfg.Upstreams[i].Timeout == 0 {
			cfg.Upstreams[i].Timeout = defaultUpstreamTimeout
		}
	}

	if errs := cfg.validate(); len(errs) > 0 {
		return nil, fmt.Errorf("invalid config:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return &cfg, nil
}

func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("GATEWAY_LISTEN"); v != "" {
		cfg.Listen = v
	}
	if v := os.Getenv("GATEWAY_REDIS_URL"); v != "" {
		cfg.RedisURL = v
	}
}

func (c *Config) validate() []string {
	var errs []string

	if c.Listen == "" {
		errs = append(errs, "listen: must not be empty")
	}
	if c.RedisURL == "" {
		errs = append(errs, "redis_url: must not be empty")
	}

	if len(c.Upstreams) == 0 {
		errs = append(errs, "upstreams: at least one upstream is required")
	}
	upstreamNames := make(map[string]bool, len(c.Upstreams))
	for i, u := range c.Upstreams {
		if u.Name == "" {
			errs = append(errs, fmt.Sprintf("upstreams[%d].name: must not be empty", i))
		} else if upstreamNames[u.Name] {
			errs = append(errs, fmt.Sprintf("upstreams[%d].name: duplicate name %q", i, u.Name))
		}
		upstreamNames[u.Name] = true

		if u.URL == "" {
			errs = append(errs, fmt.Sprintf("upstreams[%d].url: must not be empty", i))
		} else if parsed, err := url.Parse(u.URL); err != nil || parsed.Scheme == "" || parsed.Host == "" {
			errs = append(errs, fmt.Sprintf("upstreams[%d].url: %q is not a valid absolute URL", i, u.URL))
		}

		if u.Timeout < 0 {
			errs = append(errs, fmt.Sprintf("upstreams[%d].timeout: must not be negative", i))
		}
	}

	if len(c.Routes) == 0 {
		errs = append(errs, "routes: at least one route is required")
	}
	prefixes := make(map[string]bool, len(c.Routes))
	for i, r := range c.Routes {
		if r.PathPrefix == "" {
			errs = append(errs, fmt.Sprintf("routes[%d].path_prefix: must not be empty", i))
		} else if !strings.HasPrefix(r.PathPrefix, "/") {
			errs = append(errs, fmt.Sprintf("routes[%d].path_prefix: %q must start with /", i, r.PathPrefix))
		} else if prefixes[r.PathPrefix] {
			errs = append(errs, fmt.Sprintf("routes[%d].path_prefix: duplicate prefix %q", i, r.PathPrefix))
		}
		prefixes[r.PathPrefix] = true

		if r.Upstream == "" {
			errs = append(errs, fmt.Sprintf("routes[%d].upstream: must not be empty", i))
		} else if !upstreamNames[r.Upstream] {
			errs = append(errs, fmt.Sprintf("routes[%d].upstream: %q does not match any configured upstream", i, r.Upstream))
		}

		if r.RateLimit.RequestsPerSecond <= 0 {
			errs = append(errs, fmt.Sprintf("routes[%d].rate_limit.requests_per_second: must be greater than 0", i))
		}
		if r.RateLimit.Burst <= 0 {
			errs = append(errs, fmt.Sprintf("routes[%d].rate_limit.burst: must be greater than 0", i))
		}
	}

	if c.Breaker.FailureThreshold <= 0 || c.Breaker.FailureThreshold > 1 {
		errs = append(errs, "breaker.failure_threshold: must be in (0, 1]")
	}
	if c.Breaker.OpenDuration <= 0 {
		errs = append(errs, "breaker.open_duration: must be greater than 0")
	}
	if c.Breaker.HalfOpenMaxRequests <= 0 {
		errs = append(errs, "breaker.half_open_max_requests: must be greater than 0")
	}

	return errs
}
