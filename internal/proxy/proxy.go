// Package proxy contains the reverse proxy core and middleware chain.
package proxy

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/cnashn/gateway/internal/breaker"
	"github.com/cnashn/gateway/internal/config"
	"github.com/cnashn/gateway/internal/observability"
	"github.com/cnashn/gateway/internal/ratelimit"
)

// New builds the routing mux. Each route serves its upstream through the
// middleware chain, outermost first:
//
//	recovery -> request-id -> metrics -> logging -> ratelimit -> breaker -> reverse proxy
//
// Recovery is outermost so it catches panics from every later stage;
// request-id runs before logging so every log line carries an id; metrics
// and logging sit before ratelimit so 429s and 503s are observed; ratelimit
// rejects before the breaker so denied requests never count against upstream
// health.
func New(cfg *config.Config, logger *slog.Logger, limiter ratelimit.Limiter, metrics *observability.Metrics) (*http.ServeMux, error) {
	upstreams := make(map[string]config.Upstream, len(cfg.Upstreams))
	breakers := make(map[string]*breaker.CircuitBreaker, len(cfg.Upstreams))
	for _, u := range cfg.Upstreams {
		upstreams[u.Name] = u
		opts := []breaker.Option{breaker.WithLogger(logger)}
		if metrics != nil {
			opts = append(opts, breaker.WithOnStateChange(metrics.OnBreakerStateChange))
			metrics.BreakerState.WithLabelValues(u.Name).Set(float64(breaker.Closed))
		}
		breakers[u.Name] = breaker.New(u.Name, breaker.Config{
			FailureThreshold:    cfg.Breaker.FailureThreshold,
			WindowSize:          cfg.Breaker.WindowSize,
			OpenDuration:        cfg.Breaker.OpenDuration,
			HalfOpenMaxRequests: cfg.Breaker.HalfOpenMaxRequests,
		}, opts...)
	}

	mux := http.NewServeMux()
	for _, route := range cfg.Routes {
		u := upstreams[route.Upstream]
		target, err := url.Parse(u.URL)
		if err != nil {
			return nil, fmt.Errorf("parsing upstream %q url: %w", u.Name, err)
		}

		var onDecision func(string)
		if metrics != nil {
			prefix := route.PathPrefix
			onDecision = func(decision string) { metrics.OnRatelimitDecision(prefix, decision) }
		}
		handler := Chain(newReverseProxy(target, u.Timeout, logger),
			Recovery(logger),
			RequestID(),
			Metrics(metrics, route.PathPrefix, u.Name),
			Logging(logger, route.PathPrefix, u.Name),
			RateLimit(limiter, route.PathPrefix, logger, onDecision),
			CircuitBreak(breakers[route.Upstream], u.Name),
		)
		mux.Handle(route.PathPrefix, handler)
	}
	return mux, nil
}

func newReverseProxy(target *url.URL, timeout time.Duration, logger *slog.Logger) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.Header["X-Forwarded-For"] = pr.In.Header["X-Forwarded-For"]
			pr.SetXForwarded()
			pr.SetURL(target)
		},
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: timeout}).DialContext,
			ResponseHeaderTimeout: timeout,
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			requestID := r.Header.Get("X-Request-Id")
			logger.Warn("upstream request failed",
				"upstream", target.Host,
				"error", err,
				"request_id", requestID,
			)
			writeJSONError(w, http.StatusBadGateway, "bad_gateway", requestID)
		},
	}
}
