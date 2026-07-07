package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"

	"github.com/cnashn/gateway/internal/config"
)

// startDemoUpstreams runs one echo handler per configured upstream inside
// this process, on loopback ports the kernel picks, and rewrites each
// upstream URL to point at its handler. It exists so a single-service
// free-tier deploy has something to proxy to; nothing else changes, the
// gateway still reaches these "upstreams" over HTTP like real ones.
func startDemoUpstreams(cfg *config.Config, logger *slog.Logger) error {
	for i := range cfg.Upstreams {
		u := &cfg.Upstreams[i]

		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return fmt.Errorf("demo upstream %q: %w", u.Name, err)
		}

		name := u.Name
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{
				"upstream": name,
				"method":   r.Method,
				"path":     r.URL.Path,
			})
		})}
		go srv.Serve(ln)

		u.URL = "http://" + ln.Addr().String()
		logger.Info("demo upstream started", "upstream", u.Name, "addr", u.URL)
	}
	return nil
}
