package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/cnashn/gateway/internal/breaker"
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
func RateLimit(limiter ratelimit.Limiter, route string, logger *slog.Logger, onFailOpen func()) Middleware {
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
				if onFailOpen != nil {
					onFailOpen()
				}
				next.ServeHTTP(w, r)
				return
			}

			w.Header().Set("X-RateLimit-Limit", strconv.Itoa(d.Limit))
			w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(d.Remaining))

			if !d.Allowed {
				w.Header().Set("Retry-After", strconv.Itoa(ceilSeconds(d.RetryAfter)))
				writeJSONError(w, http.StatusTooManyRequests, "rate_limited", r.Header.Get("X-Request-Id"))
				return
			}

			w.Header().Set("X-RateLimit-Reset", strconv.Itoa(ceilSeconds(d.ResetAfter)))
			next.ServeHTTP(w, r)
		})
	}
}

func ceilSeconds(d time.Duration) int {
	s := int((d + time.Second - 1) / time.Second)
	if s < 1 {
		s = 1
	}
	return s
}

// CircuitBreak is a pass-through slot in the chain until Step 4 implements
// the breaker.Breaker it will consult.
func CircuitBreak(_ breaker.Breaker) Middleware {
	return func(next http.Handler) http.Handler {
		return next
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
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":      code,
		"request_id": requestID,
	})
}
