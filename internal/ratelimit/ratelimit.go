// Package ratelimit will contain the Redis-backed token bucket rate limiter.
// For now it only defines the interface the proxy middleware depends on.
package ratelimit

import (
	"context"
	"time"
)

type Decision struct {
	Allowed    bool
	Limit      int
	Remaining  int
	ResetAfter time.Duration
	RetryAfter time.Duration
}

type Limiter interface {
	Allow(ctx context.Context, route, key string) (Decision, error)
}
