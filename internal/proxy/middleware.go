package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/cnashn/gateway/internal/breaker"
	"github.com/cnashn/gateway/internal/observability"
	"github.com/cnashn/gateway/internal/ratelimit"
)

type Middleware func(http.Handler) http.Handler

// Chain applies middlewares so that the first argument is the outermost:
// Chain(h, a, b, c) serves requests through a -> b -> c -> h.
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

func Recovery(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				// http.ErrAbortHandler is the server's own abort signal
				// (ReverseProxy panics with it mid-response); it must
				// propagate, not turn into a 500.
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				logger.Error("panic recovered",
					"panic", rec,
					"stack", string(debug.Stack()),
					"request_id", r.Header.Get("X-Request-Id"),
				)
				writeJSONError(w, http.StatusInternalServerError, "internal_error", r.Header.Get("X-Request-Id"))
			}()
			next.ServeHTTP(w, r)
		})
	}
}

func RequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get("X-Request-Id")
			if id == "" {
				id = newRequestID()
				r.Header.Set("X-Request-Id", id)
			}
			w.Header().Set("X-Request-Id", id)
			next.ServeHTTP(w, r)
		})
	}
}

func newRequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func Logging(logger *slog.Logger, route, upstream string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			logger.Info("request",
				"method", r.Method,
				"path", r.URL.Path,
				"route", route,
				"upstream", upstream,
				"status", rec.status,
				"duration_ms", float64(time.Since(start).Microseconds())/1000,
				"bytes", rec.bytes,
				"client_key", ClientKey(r),
				"request_id", r.Header.Get("X-Request-Id"),
			)
		})
	}
}

// RateLimit enforces the route's rate limit. On allow it sets the standard
// X-RateLimit-* headers; on deny it responds 429 with Retry-After.
//
// FAIL OPEN by design: if the limiter errors or exceeds its 50ms budget, the
// request is allowed through rather than turning a Redis outage into a full
// gateway outage. The tradeoff is that clients are effectively unlimited
// while Redis is down. That is the right default for availability-focused
// routing; for quota billing or auth throttling, failing closed would be the
// safer choice.
func RateLimit(limiter ratelimit.Limiter, route string, logger *slog.Logger, onDecision func(decision string)) Middleware {
	if onDecision == nil {
		onDecision = func(string) {}
	}
	return func(next http.Handler) http.Handler {
		if limiter == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), 50*time.Millisecond)
			d, err := limiter.Allow(ctx, route, ClientKey(r))
			cancel()

			if err != nil {
				logger.Warn("rate limiter unavailable, failing open",
					"route", route,
					"error", err,
					"request_id", r.Header.Get("X-Request-Id"),
				)
				onDecision("failopen")
				next.ServeHTTP(w, r)
				return
			}

			w.Header().Set("X-RateLimit-Limit", strconv.Itoa(d.Limit))
			w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(d.Remaining))

			if !d.Allowed {
				onDecision("denied")
				w.Header().Set("Retry-After", strconv.Itoa(ceilSeconds(d.RetryAfter)))
				writeJSONError(w, http.StatusTooManyRequests, "rate_limited", r.Header.Get("X-Request-Id"))
				return
			}

			onDecision("allowed")
			w.Header().Set("X-RateLimit-Reset", strconv.Itoa(ceilSeconds(d.ResetAfter)))
			next.ServeHTTP(w, r)
		})
	}
}

// Metrics records request count, duration and in-flight gauge. It sits right
// after request-id so denied (429/503) responses are counted too.
func Metrics(m *observability.Metrics, route, upstream string) Middleware {
	return func(next http.Handler) http.Handler {
		if m == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			m.InflightRequests.Inc()
			defer m.InflightRequests.Dec()

			start := time.Now()
			rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)

			m.RequestsTotal.WithLabelValues(route, upstream, statusClass(rec.status)).Inc()
			m.RequestDuration.WithLabelValues(route, upstream).Observe(time.Since(start).Seconds())
		})
	}
}

func statusClass(status int) string {
	switch {
	case status >= 500:
		return "5xx"
	case status >= 400:
		return "4xx"
	case status >= 300:
		return "3xx"
	default:
		return "2xx"
	}
}

func ceilSeconds(d time.Duration) int {
	s := int((d + time.Second - 1) / time.Second)
	if s < 1 {
		s = 1
	}
	return s
}

// CircuitBreak short-circuits requests to an unhealthy upstream with 503.
// A failure is a transport error, timeout, or 5xx from the upstream (the
// gateway's own 502 covers the first two); 4xx is the client's fault, not
// the upstream's, so it never trips the breaker.
func CircuitBreak(b breaker.Breaker, upstream string) Middleware {
	return func(next http.Handler) http.Handler {
		if b == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			done, err := b.Allow()
			if err != nil {
				var oe *breaker.OpenError
				if errors.As(err, &oe) && oe.RetryAfter > 0 {
					w.Header().Set("Retry-After", strconv.Itoa(ceilSeconds(oe.RetryAfter)))
				}
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{
					"error":      "upstream_unavailable",
					"upstream":   upstream,
					"request_id": r.Header.Get("X-Request-Id"),
				})
				return
			}

			rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
			defer func() { done(rec.status < 500) }()
			next.ServeHTTP(rec, r)
		})
	}
}

// ClientKey identifies the caller for rate limiting and logs: the X-API-Key
// header if present, otherwise the client IP honoring X-Forwarded-For.
func ClientKey(r *http.Request) string {
	if k := r.Header.Get("X-API-Key"); k != "" {
		return k
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first, _, _ := strings.Cut(xff, ",")
		return strings.TrimSpace(first)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

type responseRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (r *responseRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	r.wroteHeader = true
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *responseRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func writeJSONError(w http.ResponseWriter, status int, code, requestID string) {
	writeJSON(w, status, map[string]string{
		"error":      code,
		"request_id": requestID,
	})
}

func writeJSON(w http.ResponseWriter, status int, payload map[string]string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
