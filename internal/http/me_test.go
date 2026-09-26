package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/auth/authtest"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/internal/ratelimit"
	"github.com/dezmymachine/farmish-backend/internal/users"
)

// e2eRouter wires the real router to the Auth emulator and a fresh database.
func e2eRouter(t *testing.T) (*gin.Engine, *pgxpool.Pool) {
	t.Helper()
	fb := authtest.Firebase(t)
	pool := dbtest.Pool(t)
	return newTestRouter(t, Deps{DB: fakePinger{}, Verifier: fb, Users: users.New(pool)}), pool
}

func meRequest(method, token, body string) *http.Request {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, "/v1/me", nil)
	} else {
		req = httptest.NewRequest(method, "/v1/me", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

func decodeMe(t *testing.T, w *httptest.ResponseRecorder) api.Me {
	t.Helper()
	var me api.Me
	if err := json.Unmarshal(w.Body.Bytes(), &me); err != nil {
		t.Fatalf("body %s: %v", w.Body.String(), err)
	}
	return me
}

func TestGetMe_EmailAndPhoneSignIns(t *testing.T) {
	r, pool := e2eRouter(t)

	eu := authtest.EmailUser(t)
	req := meRequest(http.MethodGet, eu.Token, "")
	w := serve(t, r, req)
	if w.Code != http.StatusOK {
		t.Fatalf("email: status %d, body %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	me := decodeMe(t, w)
	if me.SignupMethod != api.MeSignupMethodEmail || me.Email == nil || *me.Email != eu.Email || me.Phone != nil ||
		me.Role != api.MeRoleUser || me.SellerVerified {
		t.Errorf("email me = %+v", me)
	}

	pu := authtest.PhoneUser(t)
	req = meRequest(http.MethodGet, pu.Token, "")
	w = serve(t, r, req)
	if w.Code != http.StatusOK {
		t.Fatalf("phone: status %d, body %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	me = decodeMe(t, w)
	if me.SignupMethod != api.MeSignupMethodPhone || me.Phone == nil || *me.Phone != pu.Phone || me.Email != nil {
		t.Errorf("phone me = %+v", me)
	}

	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM users`).Scan(&n); err != nil || n != 2 {
		t.Errorf("users rows = %d, err %v", n, err)
	}
}

func TestGetMe_ConcurrentFirstRequests(t *testing.T) {
	r, pool := e2eRouter(t)
	eu := authtest.EmailUser(t)

	var wg sync.WaitGroup
	codes := make([]int, 10)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = serve(t, r, meRequest(http.MethodGet, eu.Token, "")).Code
		}()
	}
	wg.Wait()
	for _, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("codes = %v", codes)
		}
	}
	var n int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM users WHERE firebase_uid = $1`, eu.UID).Scan(&n)
	if n != 1 {
		t.Errorf("%d rows for one account", n)
	}
}

func TestMe_UnauthorizedIsGeneric(t *testing.T) {
	r, _ := e2eRouter(t)
	eu := authtest.EmailUser(t)
	expired := authtest.UnsignedToken(map[string]any{"sub": eu.UID, "user_id": eu.UID, "iat": 1_700_000_000, "exp": 1_700_000_060})
	otherProject := authtest.UnsignedToken(map[string]any{
		"sub": eu.UID, "user_id": eu.UID, "aud": "other-project",
		"iss": "https://securetoken.google.com/other-project",
	})

	var first string
	for name, req := range map[string]*http.Request{
		"missing":          meRequest(http.MethodGet, "", ""),
		"garbage":          meRequest(http.MethodGet, "garbage", ""),
		"expired":          meRequest(http.MethodGet, expired, ""),
		"other project":    meRequest(http.MethodGet, otherProject, ""),
		"tampered":         meRequest(http.MethodGet, strings.Replace(eu.Token, ".", ".x", 1), ""),
		"patch no token":   meRequest(http.MethodPatch, "", `{"displayName":""}`), // auth before validation
		"basic credential": func() *http.Request { q := meRequest(http.MethodGet, "", ""); q.SetBasicAuth("a", "b"); return q }(),
	} {
		t.Run(name, func(t *testing.T) {
			w := serve(t, r, req)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status %d, body %s", w.Code, w.Body.String())
			}
			assertErrorEnvelope(t, w.Body.Bytes(), apierror.CodeUnauthorized)
			assertContract(t, req, w)
			if first == "" {
				first = w.Body.String()
			} else if w.Body.String() != first {
				t.Errorf("401 body %s differs from %s", w.Body.String(), first)
			}
		})
	}
}

func TestUpdateMe(t *testing.T) {
	r, _ := e2eRouter(t)
	pu := authtest.PhoneUser(t)

	req := meRequest(http.MethodPatch, pu.Token, `{"displayName":"  Akosua Farms  "}`)
	w := serve(t, r, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	if me := decodeMe(t, w); me.DisplayName == nil || *me.DisplayName != "Akosua Farms" {
		t.Errorf("displayName = %v", me.DisplayName)
	}
	if me := decodeMe(t, serve(t, r, meRequest(http.MethodGet, pu.Token, ""))); *me.DisplayName != "Akosua Farms" {
		t.Error("update not persisted")
	}

	for name, body := range map[string]string{
		"empty":         `{"displayName":""}`,
		"blank":         `{"displayName":"   "}`,
		"too long":      `{"displayName":"` + strings.Repeat("a", 81) + `"}`,
		"unknown field": `{"displayName":"ok","role":"admin"}`,
		"missing field": `{}`,
		"not json":      `{"displayName":`,
	} {
		t.Run(name, func(t *testing.T) {
			w := serve(t, r, meRequest(http.MethodPatch, pu.Token, body))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status %d, body %s", w.Code, w.Body.String())
			}
			assertErrorEnvelope(t, w.Body.Bytes(), apierror.CodeValidationFailed)
		})
	}
}

// countingLimiter wraps a limiter and counts calls.
type countingLimiter struct {
	inner ratelimit.Limiter
	calls atomic.Int32
}

func (c *countingLimiter) Allow(ctx context.Context, r ratelimit.Rule, key string) (ratelimit.Decision, error) {
	c.calls.Add(1)
	return c.inner.Allow(ctx, r, key)
}

// Hybrid limiting: anonymous traffic (e.g. a flood) only ever touches the
// in-process IP limiter, never the metered shared (Upstash) one; signed-in
// requests use the shared limiter for their per-user budget.
func TestRateLimit_HybridBackends(t *testing.T) {
	fb := authtest.Firebase(t)
	ip := &countingLimiter{inner: ratelimit.NewMemory()}
	shared := &countingLimiter{inner: ratelimit.NewMemory()}
	r := newTestRouter(t, Deps{
		DB: fakePinger{}, Verifier: fb, Users: users.New(dbtest.Pool(t)),
		IPLimiter: ip, SharedLimiter: shared,
	})

	for range 20 {
		serve(t, r, meRequest(http.MethodGet, "", ""))
	}
	if ip.calls.Load() != 20 || shared.calls.Load() != 0 {
		t.Fatalf("anonymous: ip %d, shared %d (want 20, 0)", ip.calls.Load(), shared.calls.Load())
	}

	eu := authtest.EmailUser(t)
	if w := serve(t, r, meRequest(http.MethodGet, eu.Token, "")); w.Code != http.StatusOK {
		t.Fatalf("signed in: %d", w.Code)
	}
	if shared.calls.Load() != 1 {
		t.Errorf("signed-in request: shared limiter calls = %d, want 1", shared.calls.Load())
	}
}
