package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/cnashn/gateway/internal/config"
	"github.com/cnashn/gateway/internal/observability"
	"github.com/cnashn/gateway/internal/proxy"
	"github.com/cnashn/gateway/internal/ratelimit"
)

func main() {
	start := time.Now()
	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	if cfg.Demo {
		if err := startDemoUpstreams(cfg, logger); err != nil {
			logger.Error("failed to start demo upstreams", "error", err)
			os.Exit(1)
		}
	}

	// ParseURL handles rediss:// (Upstash) and sets up TLS from the URL.
	redisOpts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		logger.Error("invalid redis url", "error", err)
		os.Exit(1)
	}
	redisClient := redis.NewClient(redisOpts)
	defer redisClient.Close()

	// go-redis dials lazily on first use, so a cold or slow Redis never
	// blocks startup or /healthz. This warm-up ping just reports when the
	// connection is actually usable; the limiter fails open until then.
	go func() {
		for attempt := 1; ; attempt++ {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := redisClient.Ping(ctx).Err()
			cancel()
			if err == nil {
				logger.Info("redis reachable", "attempt", attempt, "since_start", time.Since(start).String())
				return
			}
			logger.Warn("redis not reachable yet", "attempt", attempt, "error", err)
			time.Sleep(min(time.Duration(attempt)*time.Second, 10*time.Second))
		}
	}()

	metrics := observability.NewMetrics()

	limits := make(map[string]ratelimit.Limit, len(cfg.Routes))
	for _, rt := range cfg.Routes {
		limits[rt.PathPrefix] = ratelimit.Limit{
			Rate:  rt.RateLimit.RequestsPerSecond,
			Burst: rt.RateLimit.Burst,
		}
	}
	limiter := ratelimit.NewRedisLimiter(redisClient, limits,
		ratelimit.WithOpObserver(metrics.ObserveRedisOp))

	mux, err := proxy.New(cfg, logger, limiter, metrics)
	if err != nil {
		logger.Error("failed to build proxy", "error", err)
		os.Exit(1)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})

	// Landing page for the bare root ("/{$}" matches only "/", so unknown
	// paths still 404). Lists the routes so a clicked link shows the gateway
	// exists rather than a bare 404.
	routePaths := make([]string, 0, len(cfg.Routes))
	for _, rt := range cfg.Routes {
		routePaths = append(routePaths, rt.PathPrefix)
	}
	index, _ := json.Marshal(map[string]any{
		"service": "gateway",
		"status":  "ok",
		"routes":  routePaths,
		"health":  "/healthz",
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(index)
	})

	server := &http.Server{
		Addr:    cfg.Listen,
		Handler: mux,
	}

	errCh := make(chan error, 1)

	// The metrics endpoint lives on its own port for Prometheus to scrape in
	// the compose stack. Single-service hosts like Render expect exactly one
	// open port and destabilize routing if they see a second, so the deploy
	// sets GATEWAY_METRICS_LISTEN=off to skip it.
	var metricsServer *http.Server
	if cfg.MetricsListen != "" {
		metricsMux := http.NewServeMux()
		metricsMux.Handle("GET /metrics", metrics.Handler())
		metricsServer = &http.Server{
			Addr:    cfg.MetricsListen,
			Handler: metricsMux,
		}
		go func() {
			logger.Info("metrics listening", "addr", cfg.MetricsListen)
			if err := metricsServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
	}
	go func() {
		logger.Info("gateway listening", "addr", cfg.Listen, "startup", time.Since(start).String())
		errCh <- server.ListenAndServe()
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		logger.Error("server failed", "error", err)
		os.Exit(1)
	case sig := <-stop:
		logger.Info("shutting down", "signal", sig.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if metricsServer != nil {
		_ = metricsServer.Shutdown(ctx)
	}
	if err := server.Shutdown(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		logger.Error("graceful shutdown failed", "error", err)
		os.Exit(1)
	}
	logger.Info("shutdown complete")
}
