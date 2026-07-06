// Package ratelimit implements a Redis-backed token bucket rate limiter,
// atomic via a Lua script.
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
