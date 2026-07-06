package ratelimit_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	testcontainers "github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/cnashn/gateway/internal/ratelimit"
)

func setupLimiter(t *testing.T, limits map[string]ratelimit.Limit) (*ratelimit.RedisLimiter, *redis.Client) {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test needs docker, skipped with -short")
	}

	ctx := context.Background()
	container, err := tcredis.Run(ctx, "redis:7-alpine")
	testcontainers.CleanupContainer(t, container)
	if err != nil {
		t.Fatalf("starting redis container: %v", err)
	}

	uri, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("redis connection string: %v", err)
	}
	opts, err := redis.ParseURL(uri)
	if err != nil {
		t.Fatalf("parsing redis url: %v", err)
	}
	client := redis.NewClient(opts)
	t.Cleanup(func() { client.Close() })

	return ratelimit.NewRedisLimiter(client, limits), client
}

func TestBurstHonoredExactly(t *testing.T) {
	limiter, _ := setupLimiter(t, map[string]ratelimit.Limit{
		"/api/orders/": {Rate: 1, Burst: 5},
	})
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		d, err := limiter.Allow(ctx, "/api/orders/", "client-1")
		if err != nil {
			t.Fatalf("Allow() error = %v", err)
		}
		if !d.Allowed {
			t.Fatalf("request %d denied, want all %d burst requests allowed", i+1, 5)
		}
		if d.Remaining != 4-i {
			t.Errorf("request %d: Remaining = %d, want %d", i+1, d.Remaining, 4-i)
		}
		if d.Limit != 5 {
			t.Errorf("Limit = %d, want 5", d.Limit)
		}
	}

	d, err := limiter.Allow(ctx, "/api/orders/", "client-1")
	if err != nil {
		t.Fatalf("Allow() error = %v", err)
	}
	if d.Allowed {
		t.Fatal("request 6 allowed, want denied after burst exhausted")
	}
	if d.RetryAfter <= 0 || d.RetryAfter > 1200*time.Millisecond {
		t.Errorf("RetryAfter = %v, want in (0, 1.2s] at 1 token/sec", d.RetryAfter)
	}
}

func TestTokensRefillAtConfiguredRate(t *testing.T) {
	limiter, _ := setupLimiter(t, map[string]ratelimit.Limit{
		"/api/orders/": {Rate: 2, Burst: 2},
	})
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if d, err := limiter.Allow(ctx, "/api/orders/", "client-1"); err != nil || !d.Allowed {
			t.Fatalf("burst request %d: allowed=%v err=%v", i+1, d.Allowed, err)
		}
	}
	if d, err := limiter.Allow(ctx, "/api/orders/", "client-1"); err != nil || d.Allowed {
		t.Fatalf("bucket drained but request allowed=%v err=%v", d.Allowed, err)
	}

	time.Sleep(1200 * time.Millisecond)

	for i := 0; i < 2; i++ {
		d, err := limiter.Allow(ctx, "/api/orders/", "client-1")
		if err != nil {
			t.Fatalf("Allow() error = %v", err)
		}
		if !d.Allowed {
			t.Fatalf("request %d after 1.2s refill denied, want ~2 tokens back at 2/sec", i+1)
		}
	}
}

func TestKeysDoNotShareBuckets(t *testing.T) {
	limiter, _ := setupLimiter(t, map[string]ratelimit.Limit{
		"/api/orders/": {Rate: 1, Burst: 1},
	})
	ctx := context.Background()

	if d, _ := limiter.Allow(ctx, "/api/orders/", "client-a"); !d.Allowed {
		t.Fatal("client-a first request denied")
	}
	if d, _ := limiter.Allow(ctx, "/api/orders/", "client-a"); d.Allowed {
		t.Fatal("client-a second request allowed, bucket should be empty")
	}
	if d, _ := limiter.Allow(ctx, "/api/orders/", "client-b"); !d.Allowed {
		t.Fatal("client-b denied, buckets must be per-key")
	}
}

func TestBucketKeyHasTTL(t *testing.T) {
	limiter, client := setupLimiter(t, map[string]ratelimit.Limit{
		"/api/orders/": {Rate: 1, Burst: 5},
	})
	ctx := context.Background()

	if _, err := limiter.Allow(ctx, "/api/orders/", "client-1"); err != nil {
		t.Fatalf("Allow() error = %v", err)
	}

	ttl, err := client.TTL(ctx, "ratelimit:/api/orders/:client-1").Result()
	if err != nil {
		t.Fatalf("TTL error = %v", err)
	}
	max := 5*time.Second + time.Minute
	if ttl <= 0 || ttl > max {
		t.Errorf("TTL = %v, want in (0, %v] (full refill time + 60s)", ttl, max)
	}
}

func TestConcurrentRequestsRespectBurst(t *testing.T) {
	limiter, _ := setupLimiter(t, map[string]ratelimit.Limit{
		"/api/orders/": {Rate: 0.5, Burst: 10},
	})
	ctx := context.Background()

	var allowed, failed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := limiter.Allow(ctx, "/api/orders/", "client-1")
			if err != nil {
				failed.Add(1)
				return
			}
			if d.Allowed {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()

	if failed.Load() != 0 {
		t.Fatalf("%d Allow calls errored", failed.Load())
	}
	if allowed.Load() != 10 {
		t.Errorf("allowed = %d, want exactly 10: the lua script must make refill+consume atomic", allowed.Load())
	}
}

func TestUnconfiguredRouteAllowed(t *testing.T) {
	limiter, _ := setupLimiter(t, map[string]ratelimit.Limit{})

	d, err := limiter.Allow(context.Background(), "/api/unknown/", "client-1")
	if err != nil {
		t.Fatalf("Allow() error = %v", err)
	}
	if !d.Allowed {
		t.Error("route without a configured limit denied, want allowed")
	}
}
