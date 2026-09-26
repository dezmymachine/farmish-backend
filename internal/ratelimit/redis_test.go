package ratelimit_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dezmymachine/farmish-backend/internal/ratelimit"
	"github.com/dezmymachine/farmish-backend/internal/ratelimit/redistest"
	"github.com/dezmymachine/farmish-backend/internal/redisx"
	"github.com/dezmymachine/farmish-backend/pkg/logger"
)

// 10/s refill, bursts of 3: easy to exhaust, refills within a test.
var fast = ratelimit.Rule{Name: "fast", Limit: 10, Period: time.Second, Burst: 3}

func allowR(t *testing.T, l ratelimit.Limiter, r ratelimit.Rule, key string) ratelimit.Decision {
	t.Helper()
	d, err := l.Allow(context.Background(), r, key)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestRedis_BurstThenDeny(t *testing.T) {
	c, prefix := redistest.Client(t)
	l := ratelimit.NewRedis(c, prefix)

	for i, want := range []int{2, 1, 0} {
		d := allowR(t, l, fast, "k")
		if !d.Allowed || d.Remaining != want || d.Limit != 3 {
			t.Fatalf("request %d: %+v", i, d)
		}
	}
	d := allowR(t, l, fast, "k")
	if d.Allowed || d.Remaining != 0 {
		t.Fatalf("over limit: %+v", d)
	}
	// One token takes 100ms at 10/s; a full bucket 300ms (minus elapsed time).
	if d.RetryAfter <= 0 || d.RetryAfter > 100*time.Millisecond || d.Reset <= 200*time.Millisecond || d.Reset > 300*time.Millisecond {
		t.Errorf("retry %v, reset %v", d.RetryAfter, d.Reset)
	}
}

func TestRedis_RefillNeverAboveBurst(t *testing.T) {
	c, prefix := redistest.Client(t)
	l := ratelimit.NewRedis(c, prefix)
	for range 3 {
		allowR(t, l, fast, "k")
	}
	time.Sleep(150 * time.Millisecond) // ~1.5 tokens
	if !allowR(t, l, fast, "k").Allowed {
		t.Fatal("not refilled after 150ms")
	}
	time.Sleep(time.Second) // far more than a full refill
	for i := range 3 {
		if !allowR(t, l, fast, "k").Allowed {
			t.Fatalf("request %d denied after idle", i)
		}
	}
	if allowR(t, l, fast, "k").Allowed {
		t.Error("bucket overfilled past Burst")
	}
}

func TestRedis_Independence(t *testing.T) {
	c, prefix := redistest.Client(t)
	dev := ratelimit.NewRedis(c, prefix+"development:")
	prod := ratelimit.NewRedis(c, prefix+"production:")
	other := ratelimit.Rule{Name: "other", Limit: 10, Period: time.Second, Burst: 1}

	for range 3 {
		allowR(t, dev, fast, "a")
	}
	if !allowR(t, dev, fast, "b").Allowed {
		t.Error("key b limited by key a")
	}
	if !allowR(t, dev, other, "a").Allowed {
		t.Error("rule other limited by rule fast")
	}
	if !allowR(t, prod, fast, "a").Allowed {
		t.Error("production prefix limited by development")
	}
}

// Two "instances" sharing Redis share one budget: the point of a shared backend.
func TestRedis_SharedAcrossInstances(t *testing.T) {
	c, prefix := redistest.Client(t)
	a, b := ratelimit.NewRedis(c, prefix), ratelimit.NewRedis(c, prefix)
	allowR(t, a, fast, "u")
	allowR(t, b, fast, "u")
	allowR(t, a, fast, "u")
	if allowR(t, b, fast, "u").Allowed {
		t.Error("instances did not share the bucket")
	}
}

// The Lua script is atomic: concurrent takes never exceed the burst.
func TestRedis_ConcurrentExactlyBurst(t *testing.T) {
	c, prefix := redistest.Client(t)
	l := ratelimit.NewRedis(c, prefix)
	slow := ratelimit.Rule{Name: "slow", Limit: 1, Period: time.Minute, Burst: 50} // no meaningful refill during the test

	var allowed atomic.Int32
	var wg sync.WaitGroup
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := l.Allow(context.Background(), slow, "k")
			if err != nil {
				t.Error(err)
				return
			}
			if d.Allowed {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := allowed.Load(); n != 50 {
		t.Errorf("allowed %d, want exactly 50", n)
	}
}

func TestRedis_KeysExpire(t *testing.T) {
	c, prefix := redistest.Client(t)
	l := ratelimit.NewRedis(c, prefix)
	allowR(t, l, fast, "k")
	ttl, err := c.PTTL(context.Background(), prefix+"fast:k").Result()
	if err != nil {
		t.Fatal(err)
	}
	// Refill time (100ms for one token) plus the 1s safety margin.
	if ttl <= 0 || ttl > 1200*time.Millisecond {
		t.Errorf("PTTL = %v", ttl)
	}
}

type failing struct{ calls atomic.Int32 }

func (f *failing) Allow(context.Context, ratelimit.Rule, string) (ratelimit.Decision, error) {
	f.calls.Add(1)
	return ratelimit.Decision{}, errors.New("upstash down")
}

func TestFallback_UsesSecondaryOnError(t *testing.T) {
	primary := &failing{}
	f := &ratelimit.Fallback{
		Primary: primary, Secondary: ratelimit.NewMemory(), Timeout: 200 * time.Millisecond,
		Log: logger.New(io.Discard, "error"),
	}
	one := ratelimit.Rule{Name: "one", Limit: 1, Period: time.Minute, Burst: 1}
	if d := allowR(t, f, one, "k"); !d.Allowed {
		t.Fatalf("first: %+v", d)
	}
	if d := allowR(t, f, one, "k"); d.Allowed {
		t.Error("fallback did not limit (it must degrade, not switch off)")
	}
	if primary.calls.Load() != 2 {
		t.Errorf("primary tried %d times, want every call", primary.calls.Load())
	}
}

// A dead Redis endpoint costs at most the timeout per call, then falls back.
func TestFallback_DeadRedisIsFast(t *testing.T) {
	dead, err := redisx.New("redis://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dead.Close() }()
	f := &ratelimit.Fallback{
		Primary: ratelimit.NewRedis(dead, "x:"), Secondary: ratelimit.NewMemory(),
		Timeout: 200 * time.Millisecond, Log: logger.New(io.Discard, "error"),
	}

	start := time.Now()
	for range 3 {
		if !allowR(t, f, fast, "k").Allowed {
			t.Fatal("fallback denied within burst")
		}
	}
	if d := time.Since(start); d > 700*time.Millisecond {
		t.Errorf("3 calls took %v; each must be bounded by the timeout", d)
	}
}

func TestFallback_RecoversToPrimary(t *testing.T) {
	c, prefix := redistest.Client(t)
	f := &ratelimit.Fallback{
		Primary: ratelimit.NewRedis(c, prefix), Secondary: ratelimit.NewMemory(),
		Timeout: 200 * time.Millisecond, Log: logger.New(io.Discard, "error"),
	}
	allowR(t, f, fast, "k")
	if n, _ := c.Exists(context.Background(), prefix+"fast:k").Result(); n != 1 {
		t.Error("healthy primary not used")
	}
}
