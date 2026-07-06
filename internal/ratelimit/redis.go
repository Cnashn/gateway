package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

type Limit struct {
	Rate  float64
	Burst int
}

// Refill and consume happen in one script execution, so two concurrent
// requests can never both spend the same token; a plain HMGET+HSET pair
// would race between the read and the write.
//
// Clock source: redis TIME instead of a caller-supplied timestamp, so every
// gateway instance shares Redis's single clock. Horizontally scaled gateways
// with skewed local clocks would otherwise refill the same bucket
// inconsistently or move last_refill_ms backward.
//
// KEYS[1] bucket hash key
// ARGV[1] refill rate in tokens/second
// ARGV[2] burst (bucket capacity)
// ARGV[3] key TTL in milliseconds
//
// Returns {1, remaining, reset_ms} when allowed,
//
//	{0, remaining, retry_after_ms} when denied.
const tokenBucketScript = `
local rate = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local ttl_ms = tonumber(ARGV[3])

local time = redis.call('TIME')
local now_ms = time[1] * 1000 + math.floor(time[2] / 1000)

local bucket = redis.call('HMGET', KEYS[1], 'tokens', 'last_refill_ms')
local tokens = tonumber(bucket[1])
local last_refill_ms = tonumber(bucket[2])

if tokens == nil then
  tokens = burst
else
  local elapsed_ms = math.max(0, now_ms - last_refill_ms)
  tokens = math.min(burst, tokens + elapsed_ms * rate / 1000)
end

local allowed = 0
local wait_ms = 0
if tokens >= 1 then
  allowed = 1
  tokens = tokens - 1
else
  wait_ms = math.ceil((1 - tokens) * 1000 / rate)
end

redis.call('HSET', KEYS[1], 'tokens', tokens, 'last_refill_ms', now_ms)
redis.call('PEXPIRE', KEYS[1], ttl_ms)

if allowed == 1 then
  local reset_ms = math.ceil((burst - tokens) * 1000 / rate)
  return {1, math.floor(tokens), reset_ms}
end
return {0, math.floor(tokens), wait_ms}
`

type RedisLimiter struct {
	client redis.UniversalClient
	limits map[string]Limit
	// redis.Script invokes via EVALSHA and reloads the script on NOSCRIPT
	// (e.g. after a Redis restart or failover flushes the script cache).
	script *redis.Script
}

func NewRedisLimiter(client redis.UniversalClient, limits map[string]Limit) *RedisLimiter {
	return &RedisLimiter{
		client: client,
		limits: limits,
		script: redis.NewScript(tokenBucketScript),
	}
}

func (l *RedisLimiter) Allow(ctx context.Context, route, key string) (Decision, error) {
	limit, ok := l.limits[route]
	if !ok {
		return Decision{Allowed: true}, nil
	}

	// Idle buckets expire once fully refilled; the extra minute avoids
	// churning keys for clients that pause briefly between bursts.
	ttl := time.Duration(float64(limit.Burst)/limit.Rate)*time.Second + time.Minute

	res, err := l.script.Run(ctx, l.client,
		[]string{"ratelimit:" + route + ":" + key},
		limit.Rate, limit.Burst, ttl.Milliseconds(),
	).Int64Slice()
	if err != nil {
		return Decision{}, fmt.Errorf("rate limit script: %w", err)
	}
	if len(res) != 3 {
		return Decision{}, fmt.Errorf("rate limit script returned %d values, want 3", len(res))
	}

	d := Decision{
		Allowed:   res[0] == 1,
		Limit:     limit.Burst,
		Remaining: int(res[1]),
	}
	if d.Allowed {
		d.ResetAfter = time.Duration(res[2]) * time.Millisecond
	} else {
		d.RetryAfter = time.Duration(res[2]) * time.Millisecond
	}
	return d, nil
}
