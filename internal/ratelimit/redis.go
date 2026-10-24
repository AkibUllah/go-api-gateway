package ratelimit

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/redis/go-redis/v9"
)

// tokenBucketScript refills and takes one token atomically inside Redis, so
// every gateway replica shares the same bucket. Time comes from the caller in
// milliseconds to keep the script deterministic (required for replication).
//
// KEYS[1] bucket key; ARGV: rate per ms, burst, now ms, ttl ms
// returns {allowed (0/1), tokens * 1000 (integer), retry_after_ms}
var tokenBucketScript = redis.NewScript(`
local rate  = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local now   = tonumber(ARGV[3])
local ttl   = tonumber(ARGV[4])

local state  = redis.call("HMGET", KEYS[1], "t", "ts")
local tokens = tonumber(state[1])
local ts     = tonumber(state[2])
if tokens == nil then
  tokens = burst
  ts = now
end
if now > ts then
  tokens = math.min(burst, tokens + (now - ts) * rate)
  ts = now
end
local allowed = 0
local retry = 0
if tokens >= 1 then
  tokens = tokens - 1
  allowed = 1
else
  retry = math.ceil((1 - tokens) / rate)
end
redis.call("HSET", KEYS[1], "t", tokens, "ts", ts)
redis.call("PEXPIRE", KEYS[1], ttl)
return {allowed, math.floor(tokens * 1000), retry}
`)

// Redis is a distributed token bucket limiter.
type Redis struct {
	client redis.Scripter
	rule   Rule
	prefix string
	now    func() time.Time
	ttl    time.Duration
}

// NewRedis returns a Redis-backed limiter. prefix namespaces keys per policy.
func NewRedis(client redis.Scripter, prefix string, rule Rule, now func() time.Time) *Redis {
	if now == nil {
		now = time.Now
	}
	refill := time.Duration(float64(rule.Burst) / rule.perSecond() * float64(time.Second))
	return &Redis{client: client, rule: rule, prefix: prefix, now: now, ttl: refill + time.Minute}
}

// Allow implements Limiter.
func (r *Redis) Allow(ctx context.Context, key string) (Decision, error) {
	perMs := r.rule.perSecond() / 1000
	res, err := tokenBucketScript.Run(ctx, r.client, []string{r.prefix + key},
		perMs, r.rule.Burst, r.now().UnixMilli(), r.ttl.Milliseconds()).Int64Slice()
	if err != nil {
		return Decision{}, fmt.Errorf("redis rate limit: %w", err)
	}
	if len(res) != 3 {
		return Decision{}, fmt.Errorf("redis rate limit: unexpected reply %v", res)
	}
	return Decision{
		Allowed:    res[0] == 1,
		Limit:      r.rule.Burst,
		Remaining:  int(math.Floor(float64(res[1]) / 1000)),
		RetryAfter: time.Duration(res[2]) * time.Millisecond,
	}, nil
}
