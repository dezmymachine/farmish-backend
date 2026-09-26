// Package redisx creates the shared Redis client (Upstash in hosted
// environments, docker-compose Redis locally). It's used for shared rate
// limits today and is the place to hang Redis-backed caching later.
package redisx

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// New builds a client for url (redis:// or rediss://). It doesn't connect;
// call Ping to check reachability.
//
// Options are tuned for a metered, remote Redis (Upstash bills per command):
// no automatic retries (they double commands and latency; callers fall back
// instead), no CLIENT SETINFO / HELLO handshakes on each new connection, and
// context deadlines are honoured so callers can bound every call.
func New(url string) (*redis.Client, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		// The URL embeds the password: never echo it.
		return nil, errors.New("REDIS_URL: invalid redis URL")
	}
	opts.DialTimeout = 2 * time.Second
	opts.ReadTimeout = 500 * time.Millisecond
	opts.WriteTimeout = 500 * time.Millisecond
	opts.ContextTimeoutEnabled = true
	opts.MaxRetries = -1 // -1 disables retries (0 would mean the default of 3)
	opts.PoolSize = 10
	opts.MinIdleConns = 0
	opts.ConnMaxIdleTime = 5 * time.Minute
	opts.Protocol = 2           // RESP2: no HELLO round trip per connection
	opts.DisableIdentity = true // no CLIENT SETINFO per connection
	return redis.NewClient(opts), nil
}

// Ping checks reachability and returns the round-trip time.
func Ping(ctx context.Context, c *redis.Client) (time.Duration, error) {
	start := time.Now()
	if err := c.Ping(ctx).Err(); err != nil {
		return 0, fmt.Errorf("redis ping: %w", err)
	}
	return time.Since(start), nil
}
