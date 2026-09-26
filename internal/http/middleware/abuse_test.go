package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/gin-gonic/gin"

	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/internal/ratelimit"
	"github.com/dezmymachine/farmish-backend/internal/turnstile"
)

// tinyLimits make limits easy to hit: bursts of 2/3/1, refilling slowly.
func tinyLimits() RateLimits {
	return RateLimits{
		IP:        ratelimit.Rule{Name: "ip", Limit: 1, Period: time.Minute, Burst: 3},
		User:      ratelimit.Rule{Name: "user", Limit: 1, Period: time.Minute, Burst: 2},
		Operation: map[string]ratelimit.Rule{"sensitive": {Name: "sensitive", Limit: 1, Period: time.Minute, Burst: 1}},
	}
}

type fakeTurnstile struct {
	calls atomic.Int32
	err   error
	gotIP netip.Addr
}

func (f *fakeTurnstile) Verify(_ context.Context, token string, ip netip.Addr) error {
	f.calls.Add(1)
	f.gotIP = ip
	if f.err != nil {
		return f.err
	}
	if token != "good" {
		return turnstile.ErrFailed
	}
	return nil
}

// abuseRouter mirrors the production chain: client IP, per-IP limit, auth,
// per-user/operation limits, Turnstile.
func abuseRouter(t *testing.T, limits RateLimits, ts turnstile.Verifier) *gin.Engine {
	t.Helper()
	l := ratelimit.NewMemory()
	authMw, err := Authenticate(loadSpec(t, "testdata/abuse.yaml"), &fakeVerifier{tokens: fixtureTokens}, fakeResolver{})
	if err != nil {
		t.Fatal(err)
	}
	ops, err := RateLimitOperations(loadSpec(t, "testdata/abuse.yaml"), l, limits)
	if err != nil {
		t.Fatal(err)
	}
	tsMw, err := TurnstileFromSpec(loadSpec(t, "testdata/abuse.yaml"), ts)
	if err != nil {
		t.Fatal(err)
	}
	e := gin.New()
	e.Use(ClientIP(ClientIPResolver{}), RateLimitIP(l, limits.IP, "/healthz"), authMw, ops, tsMw)
	ok := func(c *gin.Context) { c.Status(http.StatusOK) }
	e.GET("/healthz", ok)
	e.GET("/plain", ok)
	e.POST("/form", ok)
	e.GET("/private", ok)
	return e
}

func req(method, path, ip, authz, turnstileToken string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.RemoteAddr = ip + ":1234"
	if authz != "" {
		r.Header.Set("Authorization", authz)
	}
	if turnstileToken != "" {
		r.Header.Set(TurnstileHeader, turnstileToken)
	}
	return r
}

func do(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func errCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var env api.Error
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("body %s: %v", w.Body.String(), err)
	}
	return env.Error.Code
}

func TestRateLimitIP_429WithHeaders(t *testing.T) {
	r := abuseRouter(t, tinyLimits(), &fakeTurnstile{})
	for i, remaining := range []string{"2", "1", "0"} {
		w := do(r, req(http.MethodGet, "/plain", "196.201.214.10", "", ""))
		if w.Code != http.StatusOK || w.Header().Get("X-RateLimit-Limit") != "3" || w.Header().Get("X-RateLimit-Remaining") != remaining {
			t.Fatalf("request %d: %d %v", i, w.Code, w.Header())
		}
	}
	w := do(r, req(http.MethodGet, "/plain", "196.201.214.10", "", ""))
	if w.Code != http.StatusTooManyRequests || errCode(t, w) != apierror.CodeRateLimited {
		t.Fatalf("over limit: %d %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") != "60" || w.Header().Get("X-RateLimit-Remaining") != "0" || w.Header().Get("X-RateLimit-Reset") != "180" {
		t.Errorf("429 headers: %v", w.Header())
	}

	// Other clients and the health probe are unaffected.
	if w := do(r, req(http.MethodGet, "/plain", "196.201.214.11", "", "")); w.Code != http.StatusOK {
		t.Errorf("other IP limited: %d", w.Code)
	}
	for range 10 {
		if w := do(r, req(http.MethodGet, "/healthz", "196.201.214.10", "", "")); w.Code != http.StatusOK {
			t.Fatalf("probe limited: %d", w.Code)
		}
	}
}

// Users behind one shared (CGNAT) address get their own per-user budgets.
func TestRateLimitOperations_PerUser(t *testing.T) {
	limits := tinyLimits()
	limits.IP.Burst = 100 // isolate the per-user limit
	r := abuseRouter(t, limits, &fakeTurnstile{})
	const sharedIP = "41.66.1.1"

	for i := range 2 {
		if w := do(r, req(http.MethodGet, "/private", sharedIP, "Bearer user-token", "")); w.Code != http.StatusOK {
			t.Fatalf("user request %d: %d", i, w.Code)
		}
	}
	w := do(r, req(http.MethodGet, "/private", sharedIP, "Bearer user-token", ""))
	if w.Code != http.StatusTooManyRequests || w.Header().Get("X-RateLimit-Limit") != "2" {
		t.Fatalf("user over limit: %d %v", w.Code, w.Header())
	}
	if w := do(r, req(http.MethodGet, "/private", sharedIP, "Bearer admin-token", "")); w.Code != http.StatusOK {
		t.Errorf("second user on the same IP limited: %d", w.Code)
	}
}

func TestSensitivePolicyAndTurnstile(t *testing.T) {
	ts := &fakeTurnstile{}
	limits := tinyLimits()
	limits.IP.Burst = 100
	r := abuseRouter(t, limits, ts)

	// Missing or bad token: 400, whatever else is fine.
	w := do(r, req(http.MethodPost, "/form", "196.201.214.20", "", ""))
	if w.Code != http.StatusBadRequest || errCode(t, w) != apierror.CodeTurnstileFailed {
		t.Fatalf("no token: %d %s", w.Code, w.Body.String())
	}
	if ts.calls.Load() != 0 {
		t.Error("Cloudflare called without a token")
	}

	// A fresh client: good token passes, and the verifier sees the client IP.
	if w := do(r, req(http.MethodPost, "/form", "196.201.214.21", "", "good")); w.Code != http.StatusOK {
		t.Fatalf("good token: %d %s", w.Code, w.Body.String())
	}
	if ts.gotIP != netip.MustParseAddr("196.201.214.21") {
		t.Errorf("verifier got IP %v", ts.gotIP)
	}
	// The sensitive policy (burst 1) now limits that client, before Cloudflare.
	before := ts.calls.Load()
	if w := do(r, req(http.MethodPost, "/form", "196.201.214.21", "", "good")); w.Code != http.StatusTooManyRequests {
		t.Errorf("sensitive policy not applied: %d", w.Code)
	}
	if ts.calls.Load() != before {
		t.Error("rate-limited request still reached Cloudflare")
	}

	w = do(r, req(http.MethodPost, "/form", "196.201.214.22", "", "bad"))
	if w.Code != http.StatusBadRequest || errCode(t, w) != apierror.CodeTurnstileFailed {
		t.Errorf("bad token: %d %s", w.Code, w.Body.String())
	}
	// Unmarked operations never call Cloudflare.
	before = ts.calls.Load()
	do(r, req(http.MethodGet, "/plain", "196.201.214.23", "", ""))
	if ts.calls.Load() != before {
		t.Error("unmarked operation called Cloudflare")
	}
}

func TestTurnstile_UnavailableFailsClosed(t *testing.T) {
	r := abuseRouter(t, tinyLimits(), &fakeTurnstile{err: errors.New("cloudflare timeout")})
	w := do(r, req(http.MethodPost, "/form", "196.201.214.30", "", "good"))
	if w.Code != http.StatusServiceUnavailable || errCode(t, w) != apierror.CodeUnavailable {
		t.Errorf("%d %s", w.Code, w.Body.String())
	}
}

type brokenLimiter struct{}

func (brokenLimiter) Allow(context.Context, ratelimit.Rule, string) (ratelimit.Decision, error) {
	return ratelimit.Decision{}, errors.New("redis down")
}

func TestRateLimit_BackendFailureFailsOpen(t *testing.T) {
	e := gin.New()
	e.Use(RateLimitIP(brokenLimiter{}, tinyLimits().IP))
	e.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })
	if w := do(e, req(http.MethodGet, "/", "196.201.214.40", "", "")); w.Code != http.StatusOK {
		t.Errorf("status %d", w.Code)
	}
}

func TestAbuseExtensions_RejectedAtStartup(t *testing.T) {
	l := ratelimit.NewMemory()
	tests := map[string]func(*openapi3.T) error{
		"unknown rate-limit policy": func(s *openapi3.T) error {
			s.Paths.Value("/form").Post.Extensions[rateLimitExtension] = "strictest"
			_, err := RateLimitOperations(s, l, tinyLimits())
			return err
		},
		"non-boolean turnstile": func(s *openapi3.T) error {
			s.Paths.Value("/form").Post.Extensions[turnstileExtension] = "yes"
			_, err := TurnstileFromSpec(s, &fakeTurnstile{})
			return err
		},
	}
	for name, fn := range tests {
		t.Run(name, func(t *testing.T) {
			if err := fn(loadSpec(t, "testdata/abuse.yaml")); err == nil {
				t.Error("expected startup error")
			}
		})
	}
	if err := DefaultRateLimits().Validate(); err != nil {
		t.Errorf("default limits invalid: %v", err)
	}
	bad := tinyLimits()
	bad.Operation["sensitive"] = ratelimit.Rule{Name: "sensitive"}
	if bad.Validate() == nil {
		t.Error("invalid rule accepted")
	}
}
