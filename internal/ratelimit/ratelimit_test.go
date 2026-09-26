package ratelimit

import (
	"context"
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }
func newClock() *clock               { return &clock{t: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)} }

func allow(t *testing.T, m *Memory, r Rule, key string) Decision {
	t.Helper()
	d, err := m.Allow(context.Background(), r, key)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// 60/min = 1 token per second, bursts of 3.
var perSec = Rule{Name: "t", Limit: 60, Period: time.Minute, Burst: 3}

func TestBurstThenDeny(t *testing.T) {
	c := newClock()
	m := NewMemory(WithClock(c.now))

	for i, wantRemaining := range []int{2, 1, 0} {
		d := allow(t, m, perSec, "k")
		if !d.Allowed || d.Remaining != wantRemaining || d.Limit != 3 {
			t.Fatalf("request %d: %+v", i, d)
		}
	}
	d := allow(t, m, perSec, "k")
	if d.Allowed || d.Remaining != 0 || d.RetryAfter != time.Second || d.Reset != 3*time.Second {
		t.Fatalf("over limit: %+v", d)
	}
}

func TestRefill(t *testing.T) {
	c := newClock()
	m := NewMemory(WithClock(c.now))
	for range 3 {
		allow(t, m, perSec, "k")
	}
	c.add(500 * time.Millisecond)
	if d := allow(t, m, perSec, "k"); d.Allowed || d.RetryAfter != 500*time.Millisecond {
		t.Fatalf("half a token: %+v", d)
	}
	c.add(500 * time.Millisecond)
	if d := allow(t, m, perSec, "k"); !d.Allowed {
		t.Fatalf("after 1s: %+v", d)
	}
	c.add(time.Hour) // refills to Burst, never above
	for i := range 3 {
		if d := allow(t, m, perSec, "k"); !d.Allowed {
			t.Fatalf("after idle, request %d denied", i)
		}
	}
	if d := allow(t, m, perSec, "k"); d.Allowed {
		t.Fatal("bucket overfilled past Burst")
	}
}

func TestKeysAndRulesAreIndependent(t *testing.T) {
	m := NewMemory(WithClock(newClock().now))
	other := Rule{Name: "other", Limit: 60, Period: time.Minute, Burst: 1}
	for range 3 {
		allow(t, m, perSec, "a")
	}
	if !allow(t, m, perSec, "b").Allowed {
		t.Error("key b limited by key a")
	}
	if !allow(t, m, other, "a").Allowed {
		t.Error("rule other limited by rule t")
	}
}

func TestConcurrentAllowsNeverExceedBurst(t *testing.T) {
	m := NewMemory(WithClock(newClock().now)) // frozen clock: no refill
	r := Rule{Name: "c", Limit: 60, Period: time.Minute, Burst: 50}
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d, _ := m.Allow(context.Background(), r, "k"); d.Allowed {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed != 50 {
		t.Errorf("allowed %d, want exactly 50", allowed)
	}
}

func TestSweepAndKeyCap(t *testing.T) {
	c := newClock()
	m := NewMemory(WithClock(c.now), WithMaxKeys(3))
	r := Rule{Name: "cap", Limit: 60, Period: time.Minute, Burst: 1}

	for i := range 3 {
		allow(t, m, r, fmt.Sprint("k", i))
	}
	// Cap reached, nothing idle: new keys share one overflow bucket.
	if !allow(t, m, r, "new-1").Allowed {
		t.Fatal("first overflow request denied")
	}
	if allow(t, m, r, "new-2").Allowed {
		t.Error("overflow keys not limited as a group")
	}
	if m.Len() != 4 {
		t.Errorf("tracked %d buckets, want cap 3 + overflow", m.Len())
	}

	c.add(idleAfter)
	m.Sweep()
	if m.Len() != 0 {
		t.Errorf("sweep left %d idle buckets", m.Len())
	}
	if !allow(t, m, r, "new-2").Allowed {
		t.Error("key not tracked individually after sweep")
	}
}

func TestRuleValidate(t *testing.T) {
	if err := perSec.Validate(); err != nil {
		t.Error(err)
	}
	for _, bad := range []Rule{
		{Name: "", Limit: 1, Period: time.Second, Burst: 1},
		{Name: "x", Limit: 0, Period: time.Second, Burst: 1},
		{Name: "x", Limit: 1, Period: 0, Burst: 1},
		{Name: "x", Limit: 1, Period: time.Second, Burst: 0},
		{Name: "slow", Limit: 1, Period: time.Hour, Burst: 1}, // refills slower than the sweep threshold
	} {
		if bad.Validate() == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestClientKey(t *testing.T) {
	for in, want := range map[string]string{
		"196.201.214.10":        "196.201.214.10",
		"::ffff:196.201.214.10": "196.201.214.10",
		"2c0f:f248:1:2:3:4:5:6": "2c0f:f248:1:2::/64",
		"2c0f:f248:1:2:ffff::1": "2c0f:f248:1:2::/64",
	} {
		if got := ClientKey(netip.MustParseAddr(in)); got != want {
			t.Errorf("ClientKey(%s) = %s, want %s", in, got, want)
		}
	}
}
