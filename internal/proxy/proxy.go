// Package proxy contains the reverse proxy core and middleware chain.
package proxy

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/cnashn/gateway/internal/config"
	"github.com/cnashn/gateway/internal/ratelimit"
)

// failOpenTotal counts rate-limiter fail-open events until Step 5 replaces
// it with a Prometheus counter.
var failOpenTotal atomic.Int64

func FailOpenTotal() int64 { return failOpenTotal.Load() }

// New builds the routing mux. Each route serves its upstream through the
// middleware chain, outermost first:
//
//	recovery -> request-id -> logging -> ratelimit -> breaker -> reverse proxy
//
// Recovery is outermost so it catches panics from every later stage;
// request-id runs before logging so every log line carries an id; ratelimit
// rejects before the breaker so denied requests never count against upstream
// health.
func New(cfg *config.Config, logger *slog.Logger, limiter ratelimit.Limiter) (*http.ServeMux, error) {
	upstreams := make(map[string]config.Upstream, len(cfg.Upstreams))
	for _, u := range cfg.Upstreams {
		upstreams[u.Name] = u
	}

	mux := http.NewServeMux()
	for _, route := range cfg.Routes {
		u := upstreams[route.Upstream]
		target, err := url.Parse(u.URL)
		if err != nil {
			return nil, fmt.Errorf("parsing upstream %q url: %w", u.Name, err)
		}

		handler := Chain(newReverseProxy(target, u.Timeout, logger),
			Recovery(logger),
			RequestID(),
			Logging(logger, route.PathPrefix, u.Name),
			RateLimit(limiter, route.PathPrefix, logger, func() { failOpenTotal.Add(1) }),
			CircuitBreak(nil),
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
