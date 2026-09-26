// Package ratelimit implements token-bucket rate limiting behind a Limiter
// interface. Memory is the in-process implementation used in v1; a shared
// backend (Redis/Postgres) can replace it when the API runs on several
// instances (REDEVELOPMENT_PLAN.md §10).
package ratelimit

import (
	"context"
	"fmt"
	"math"
	"net/netip"
	"sync"
	"time"
)

// Rule allows Limit events per Period, with bursts up to Burst.
// Tokens refill continuously at Limit/Period.
type Rule struct {
	Name   string // namespaces keys, e.g. "ip", "user", "sensitive"
	Limit  int
	Period time.Duration
	Burst  int
}

func (r Rule) ratePerSec() float64 { return float64(r.Limit) / r.Period.Seconds() }

// Validate rejects rules that are malformed or that refill slower than the
// sweeper's idle threshold (a swept bucket restarts full, so that would hand
// out free tokens).
func (r Rule) Validate() error {
	if r.Name == "" || r.Limit < 1 || r.Period <= 0 || r.Burst < 1 {
		return fmt.Errorf("rate limit rule %q: name, limit >= 1, period > 0 and burst >= 1 are required", r.Name)
	}
	if refill := seconds(float64(r.Burst) / r.ratePerSec()); refill > idleAfter {
		return fmt.Errorf("rate limit rule %q refills in %v, longer than the %v sweep threshold", r.Name, refill, idleAfter)
	}
	return nil
}

// Decision is the outcome of one Allow call.
type Decision struct {
	Allowed   bool
	Limit     int           // bucket capacity (Rule.Burst)
	Remaining int           // whole tokens left after this call
	Reset     time.Duration // until the bucket is full again
	// RetryAfter is how long until one token is available (0 if Allowed).
	RetryAfter time.Duration
}

// Limiter decides whether an event for key is allowed under rule.
type Limiter interface {
	Allow(ctx context.Context, rule Rule, key string) (Decision, error)
}

type bucket struct {
	tokens float64
	last   time.Time
}

// Memory is an in-process Limiter. It's safe for concurrent use.
type Memory struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	maxKeys int
	now     func() time.Time
}

// Option configures Memory.
type Option func(*Memory)

// WithClock injects a clock (tests).
func WithClock(now func() time.Time) Option { return func(m *Memory) { m.now = now } }

// WithMaxKeys caps tracked keys (default 100,000). Beyond the cap, and after
// sweeping idle buckets, new keys share one overflow bucket per rule: memory
// stays bounded and a flood of fresh keys (e.g. rotating addresses) is still
// limited as a group.
func WithMaxKeys(n int) Option { return func(m *Memory) { m.maxKeys = n } }

// NewMemory returns an in-process limiter.
func NewMemory(opts ...Option) *Memory {
	m := &Memory{buckets: map[string]*bucket{}, maxKeys: 100_000, now: time.Now}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Allow takes one token from rule's bucket for key.
func (m *Memory) Allow(_ context.Context, rule Rule, key string) (Decision, error) {
	now := m.now()
	id := rule.Name + ":" + key

	m.mu.Lock()
	defer m.mu.Unlock()

	b, ok := m.buckets[id]
	if !ok {
		if len(m.buckets) >= m.maxKeys {
			m.sweepLocked(now)
		}
		if len(m.buckets) >= m.maxKeys {
			id = rule.Name + ":\x00overflow"
			b, ok = m.buckets[id]
		}
		if !ok {
			b = &bucket{tokens: float64(rule.Burst), last: now}
			m.buckets[id] = b
		}
	}

	rate := rule.ratePerSec()
	b.tokens = math.Min(float64(rule.Burst), b.tokens+now.Sub(b.last).Seconds()*rate)
	b.last = now

	d := Decision{Limit: rule.Burst}
	if b.tokens >= 1 {
		b.tokens--
		d.Allowed = true
	} else {
		d.RetryAfter = seconds((1 - b.tokens) / rate)
	}
	d.Remaining = int(math.Floor(b.tokens))
	d.Reset = seconds((float64(rule.Burst) - b.tokens) / rate)
	return d, nil
}

// Sweep drops buckets that have refilled completely (idle clients). Call it
// periodically; Allow also sweeps when the key cap is reached.
func (m *Memory) Sweep() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepLocked(m.now())
}

// Len reports tracked buckets (tests, metrics).
func (m *Memory) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.buckets)
}

// sweepLocked removes buckets idle long enough to be full again. Without the
// rule at hand it uses a conservative idle threshold instead.
func (m *Memory) sweepLocked(now time.Time) {
	for id, b := range m.buckets {
		if now.Sub(b.last) >= idleAfter {
			delete(m.buckets, id)
		}
	}
}

// idleAfter must exceed the longest time any Rule needs to refill from empty;
// a swept bucket restarts full, so sweeping earlier would grant free tokens.
const idleAfter = 15 * time.Minute

// RunSweeper sweeps every interval until ctx is done.
func (m *Memory) RunSweeper(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Sweep()
		}
	}
}

// ClientKey normalises a client address for per-IP limiting. IPv6 clients
// get a /64 (typically one subscriber), so rotating addresses within it
// doesn't evade the limit.
func ClientKey(addr netip.Addr) string {
	addr = addr.Unmap()
	if addr.Is6() {
		p, _ := addr.Prefix(64)
		return p.String()
	}
	return addr.String()
}

func seconds(s float64) time.Duration {
	return time.Duration(math.Ceil(s * float64(time.Second)))
}
