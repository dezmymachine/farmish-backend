// Package redistest connects tests to a real Redis (docker-compose).
//
// Set REDIS_TEST_URL (make test does). Without it, Redis tests are skipped,
// unless REDISTEST_REQUIRED=1, in which case they fail instead. Tests never
// touch Upstash.
package redistest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/dezmymachine/farmish-backend/internal/redisx"
)

// Client returns a client for REDIS_TEST_URL and a key prefix unique to this
// test; keys under it are deleted when the test ends.
func Client(t testing.TB) (*redis.Client, string) {
	t.Helper()
	url := os.Getenv("REDIS_TEST_URL")
	if url == "" {
		if os.Getenv("REDISTEST_REQUIRED") == "1" {
			t.Fatal("redistest: REDIS_TEST_URL is not set (REDISTEST_REQUIRED=1)")
		}
		t.Skip("redistest: REDIS_TEST_URL not set; skipping Redis test (run `make test`)")
	}
	c, err := redisx.New(url)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := redisx.Ping(context.Background(), c); err != nil {
		t.Fatalf("redistest: %v", err)
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	prefix := "test:" + hex.EncodeToString(b) + ":"

	t.Cleanup(func() {
		ctx := context.Background()
		iter := c.Scan(ctx, 0, prefix+"*", 100).Iterator()
		for iter.Next(ctx) {
			c.Del(ctx, iter.Val())
		}
		_ = c.Close()
	})
	return c, prefix
}
