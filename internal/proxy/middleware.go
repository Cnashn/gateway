package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
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

// RateLimit is a pass-through slot in the chain until Step 3 implements the
// ratelimit.Limiter it will consult.
func RateLimit(_ ratelimit.Limiter) Middleware {
	return func(next http.Handler) http.Handler {
		return next
	}
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
