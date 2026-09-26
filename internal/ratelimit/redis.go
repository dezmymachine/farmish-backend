package ratelimit

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// tokenBucket atomically refills and takes one token. State is a hash
// {tokens, ts}; ts uses the Redis server clock (TIME), so every API instance
// agrees. The key expires once it would be full again, so idle clients cost
// no memory and no sweeper is needed. Times are in microseconds.
//
// KEYS[1] bucket key; ARGV[1] tokens per microsecond; ARGV[2] burst.
// Returns {allowed (0/1), remaining (whole tokens), retry_us, reset_us}.
var tokenBucket = redis.NewScript(`
local rate = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000000 + tonumber(t[2])

local state = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local tokens = tonumber(state[1])
local ts = tonumber(state[2])
if tokens == nil or ts == nil then
  tokens = burst
  ts = now
end
local elapsed = now - ts
if elapsed < 0 then elapsed = 0 end
tokens = math.min(burst, tokens + elapsed * rate)

local allowed = 0
local retry = 0
if tokens >= 1 then
  tokens = tokens - 1
  allowed = 1
else
  retry = math.ceil((1 - tokens) / rate)
end
local reset = math.ceil((burst - tokens) / rate)

redis.call('HSET', KEYS[1], 'tokens', tostring(tokens), 'ts', tostring(now))
redis.call('PEXPIRE', KEYS[1], math.ceil(reset / 1000) + 1000)
return {allowed, math.floor(tokens), retry, reset}
`)

// Redis is a Limiter shared by every API instance using the same Redis.
type Redis struct {
	client redis.Scripter
	prefix string
}

var _ Limiter = (*Redis)(nil)

// NewRedis returns a limiter storing buckets under prefix, e.g.
// "farmish:production:rl:" (the environment keeps dev and prod apart if they
// ever share a database).
func NewRedis(client redis.Scripter, prefix string) *Redis {
	return &Redis{client: client, prefix: prefix}
}

// Allow takes one token from rule's bucket for key. One round trip
// (EVALSHA; EVAL the first time a server sees the script).
func (r *Redis) Allow(ctx context.Context, rule Rule, key string) (Decision, error) {
	ratePerMicro := rule.ratePerSec() / 1e6
	res, err := tokenBucket.Run(ctx, r.client, []string{r.prefix + rule.Name + ":" + key},
		strconv.FormatFloat(ratePerMicro, 'g', -1, 64), rule.Burst).Int64Slice()
	if err != nil {
		return Decision{}, fmt.Errorf("redis rate limit: %w", err)
	}
	if len(res) != 4 {
		return Decision{}, fmt.Errorf("redis rate limit: unexpected reply %v", res)
	}
	return Decision{
		Allowed:    res[0] == 1,
		Limit:      rule.Burst,
		Remaining:  int(res[1]),
		RetryAfter: time.Duration(res[2]) * time.Microsecond,
		Reset:      time.Duration(res[3]) * time.Microsecond,
	}, nil
}
