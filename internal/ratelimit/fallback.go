package ratelimit

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// Fallback asks Primary (a shared Redis limiter) within Timeout, and on any
// error (timeout, outage, quota throttling) answers from Secondary (the
// in-process limiter). Limiting degrades to per-instance rather than
// switching off, and a slow Redis never adds more than Timeout to a request.
type Fallback struct {
	Primary   Limiter
	Secondary Limiter
	Timeout   time.Duration
	Log       *slog.Logger

	lastLog atomic.Int64 // unix nanos of the last fallback warning
}

var _ Limiter = (*Fallback)(nil)

// fallbackLogEvery throttles the "falling back" warning during an outage.
const fallbackLogEvery = 30 * time.Second

// Allow implements Limiter. It never returns an error from Primary.
func (f *Fallback) Allow(ctx context.Context, rule Rule, key string) (Decision, error) {
	pctx, cancel := context.WithTimeout(ctx, f.Timeout)
	d, err := f.Primary.Allow(pctx, rule, key)
	cancel()
	if err == nil {
		return d, nil
	}
	if now := time.Now().UnixNano(); now-f.lastLog.Load() >= int64(fallbackLogEvery) {
		f.lastLog.Store(now)
		f.Log.Warn("shared rate limiter unavailable; using in-process limits",
			slog.String("rule", rule.Name), slog.String("error", err.Error()))
	}
	return f.Secondary.Allow(ctx, rule, key)
}
