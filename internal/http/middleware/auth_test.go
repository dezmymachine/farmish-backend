package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/dezmymachine/farmish-backend/internal/auth"
	"github.com/dezmymachine/farmish-backend/internal/auth/authtest"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/internal/users"
)

type fakeVerifier struct {
	calls       atomic.Int32
	strictCalls atomic.Int32
	tokens      map[string]auth.Identity
}

func (f *fakeVerifier) Verify(_ context.Context, token string) (auth.Identity, error) {
	f.calls.Add(1)
	id, ok := f.tokens[token]
	if !ok {
		return auth.Identity{}, auth.ErrInvalidToken
	}
	return id, nil
}

func (f *fakeVerifier) VerifyStrict(_ context.Context, token string) (auth.Identity, error) {
	f.strictCalls.Add(1)
	id, ok := f.tokens[token]
	if !ok {
		return auth.Identity{}, auth.ErrInvalidToken
	}
	return id, nil
}

type fakeResolver struct{ err error }

func (f fakeResolver) Resolve(_ context.Context, id auth.Identity) (users.User, error) {
	if f.err != nil {
		return users.User{}, f.err
	}
	if _, ok := auth.SignupMethod(id.Provider); !ok {
		return users.User{}, users.ErrUnsupportedProvider
	}
	role := users.RoleUser
	if id.UID == "admin-uid" {
		role = users.RoleAdmin
	}
	// Stable per account, like a real users row.
	return users.User{ID: uuid.NewSHA1(uuid.NameSpaceOID, []byte(id.UID)), FirebaseUID: id.UID, Role: role}, nil
}

func loadSpec(t *testing.T, file string) *openapi3.T {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := openapi3.NewLoader().LoadFromData(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := spec.Validate(t.Context()); err != nil {
		t.Fatalf("fixture invalid: %v", err)
	}
	return spec
}

func authRouter(t *testing.T, v auth.Verifier, r UserResolver) *gin.Engine {
	t.Helper()
	mw, err := Authenticate(loadSpec(t, "testdata/auth.yaml"), v, r)
	if err != nil {
		t.Fatal(err)
	}
	e := gin.New()
	e.Use(mw)
	ok := func(c *gin.Context) {
		u, has := users.FromContext(c.Request.Context())
		if has {
			c.String(http.StatusOK, u.FirebaseUID)
			return
		}
		c.String(http.StatusOK, "anonymous")
	}
	e.GET("/public", ok)
	e.GET("/private", ok)
	e.GET("/admin", ok)
	e.GET("/step-up", ok)
	e.GET("/not-in-spec", ok)
	return e
}

func get(t *testing.T, h http.Handler, path, authz string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

var fixtureTokens = map[string]auth.Identity{
	"user-token":   {UID: "user-uid", Provider: "password"},
	"admin-token":  {UID: "admin-uid", Provider: "google.com"},
	"custom-token": {UID: "custom-uid", Provider: "custom"},
}

func TestAuthenticate_PublicAndUnknownRoutesSkipAuth(t *testing.T) {
	v := &fakeVerifier{tokens: fixtureTokens}
	r := authRouter(t, v, fakeResolver{})
	for _, path := range []string{"/public", "/not-in-spec"} {
		w := get(t, r, path, "Bearer garbage")
		if w.Code != http.StatusOK || w.Body.String() != "anonymous" {
			t.Errorf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	if n := v.calls.Load(); n != 0 {
		t.Errorf("verifier called %d times on public routes", n)
	}
}

func TestAuthenticate_GenericUnauthorized(t *testing.T) {
	r := authRouter(t, &fakeVerifier{tokens: fixtureTokens}, fakeResolver{})
	var first string
	for name, authz := range map[string]string{
		"missing":         "",
		"basic scheme":    "Basic dXNlcjpwYXNz",
		"bearer no token": "Bearer ",
		"invalid token":   "Bearer nope",
		"custom provider": "Bearer custom-token",
	} {
		t.Run(name, func(t *testing.T) {
			for _, path := range []string{"/private", "/admin"} {
				w := get(t, r, path, authz)
				if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") != "Bearer" {
					t.Fatalf("%s: status %d, WWW-Authenticate %q", path, w.Code, w.Header().Get("WWW-Authenticate"))
				}
				if first == "" {
					first = w.Body.String()
				}
				if w.Body.String() != first {
					t.Errorf("401 bodies differ: %s vs %s", w.Body.String(), first)
				}
			}
		})
	}
	if first != `{"error":{"code":"unauthorized","message":"Authentication required"}}` {
		t.Errorf("401 body = %s", first)
	}
}

func TestAuthenticate_UserAndAdmin(t *testing.T) {
	r := authRouter(t, &fakeVerifier{tokens: fixtureTokens}, fakeResolver{})

	if w := get(t, r, "/private", "bearer user-token"); w.Code != http.StatusOK || w.Body.String() != "user-uid" {
		t.Errorf("user on /private: %d %s", w.Code, w.Body.String())
	}
	w := get(t, r, "/admin", "Bearer user-token")
	if w.Code != http.StatusForbidden || w.Body.String() != `{"error":{"code":"forbidden","message":"You do not have access to this resource"}}` {
		t.Errorf("user on /admin: %d %s", w.Code, w.Body.String())
	}
	if w := get(t, r, "/admin", "Bearer admin-token"); w.Code != http.StatusOK || w.Body.String() != "admin-uid" {
		t.Errorf("admin on /admin: %d %s", w.Code, w.Body.String())
	}
}

func TestAuthenticate_ResolverFailureIs500(t *testing.T) {
	r := authRouter(t, &fakeVerifier{tokens: fixtureTokens}, fakeResolver{err: errors.New("db down")})
	w := get(t, r, "/private", "Bearer user-token")
	if w.Code != http.StatusInternalServerError || w.Body.String() != `{"error":{"code":"internal_error","message":"Internal server error"}}` {
		t.Errorf("%d %s", w.Code, w.Body.String())
	}
}

func TestAuthenticate_RejectsBadRoleExtensions(t *testing.T) {
	for name, mutate := range map[string]func(*openapi3.T){
		"typo'd role": func(s *openapi3.T) {
			s.Paths.Value("/admin").Get.Extensions[roleExtension] = "admn"
		},
		"role on a public operation": func(s *openapi3.T) {
			op := s.Paths.Value("/public").Get
			op.Extensions = map[string]any{roleExtension: "admin"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			spec := loadSpec(t, "testdata/auth.yaml")
			mutate(spec)
			if _, err := Authenticate(spec, &fakeVerifier{}, fakeResolver{}); err == nil {
				t.Error("expected startup error")
			}
		})
	}
}

func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var env api.Error
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("body %s is not JSON: %v", w.Body.String(), err)
	}
	return env.Error.Code
}

// An emulator user signed in just now passes the step-up fixture op.
func TestStepUp_FreshTokenPasses(t *testing.T) {
	fb := authtest.Firebase(t)
	r := authRouter(t, fb, users.New(dbtest.Pool(t)))
	eu := authtest.EmailUser(t)

	w := get(t, r, "/step-up", "Bearer "+eu.Token)
	if w.Code != http.StatusOK {
		t.Fatalf("fresh token on step-up op: status %d, body %s", w.Code, w.Body.String())
	}
	// A missing token on a step-up op still gets the generic 401.
	w = get(t, r, "/step-up", "")
	if w.Code != http.StatusUnauthorized || errorCode(t, w) != apierror.CodeUnauthorized {
		t.Errorf("missing token on step-up op: %d %s", w.Code, w.Body.String())
	}
}

// With the clock 6 minutes ahead the same fresh token is stale: 401
// reauth_required plus the WWW-Authenticate error param.
func TestStepUp_StaleTokenRejected(t *testing.T) {
	fb := authtest.Firebase(t)
	r := authRouter(t, fb, users.New(dbtest.Pool(t)))
	eu := authtest.EmailUser(t)

	if w := get(t, r, "/step-up", "Bearer "+eu.Token); w.Code != http.StatusOK {
		t.Fatalf("fresh token: status %d, body %s", w.Code, w.Body.String())
	}

	old := stepUpNow
	stepUpNow = func() time.Time { return old().Add(6 * time.Minute) }
	defer func() { stepUpNow = old }()

	w := get(t, r, "/step-up", "Bearer "+eu.Token)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("stale token: status %d, body %s", w.Code, w.Body.String())
	}
	if got := errorCode(t, w); got != apierror.CodeReauthRequired {
		t.Errorf("stale token code = %q, want %q (body %s)", got, apierror.CodeReauthRequired, w.Body.String())
	}
	if wa := w.Header().Get("WWW-Authenticate"); wa != `Bearer error="insufficient_user_authentication"` {
		t.Errorf("WWW-Authenticate = %q", wa)
	}
}

// After RevokeRefreshTokens the same token fails the step-up op with 401.
//
// Deviation from the phase spec (see ADR-0014): the spec expects the same
// token to still pass a normal op until expiry. Against production Firebase
// that holds, because only VerifyStrict checks revocation. But the Admin SDK
// also checks revocation on the fast path when FIREBASE_AUTH_EMULATOR_HOST
// is set (`if c.isEmulator || checkRevokedOrDisabled` in auth.go), so in
// tests both paths reject the revoked token. Production behaviour (normal
// path never calls VerifyStrict) is proved by
// TestStepUp_NonStepUpUsesFastVerify instead.
func TestStepUp_RevokedTokenRejected(t *testing.T) {
	fb := authtest.Firebase(t)
	r := authRouter(t, fb, users.New(dbtest.Pool(t)))
	eu := authtest.EmailUser(t)

	if w := get(t, r, "/step-up", "Bearer "+eu.Token); w.Code != http.StatusOK {
		t.Fatalf("before revoke step-up: %d %s", w.Code, w.Body.String())
	}
	// The emulator tracks revocation at one-second granularity (validSince):
	// revoking in the same second as sign-in is a no-op, so cross into the
	// next second first. This sleep works around emulator granularity, not
	// a business timer.
	time.Sleep(1200 * time.Millisecond)
	authtest.Revoke(t, fb, eu.UID)

	if w := get(t, r, "/step-up", "Bearer "+eu.Token); w.Code != http.StatusUnauthorized {
		t.Errorf("revoked token on step-up op: status %d, body %s", w.Code, w.Body.String())
	}
}

// A normal operation verifies with the fast path and never touches
// VerifyStrict; the step-up operation uses only VerifyStrict.
func TestStepUp_NonStepUpUsesFastVerify(t *testing.T) {
	fresh := map[string]auth.Identity{
		"fresh-token": {UID: "user-uid", Provider: "password", AuthTime: time.Now()},
	}
	v := &fakeVerifier{tokens: fresh}
	r := authRouter(t, v, fakeResolver{})

	if w := get(t, r, "/private", "Bearer fresh-token"); w.Code != http.StatusOK {
		t.Fatalf("normal op: %d %s", w.Code, w.Body.String())
	}
	if n := v.strictCalls.Load(); n != 0 {
		t.Errorf("normal op called VerifyStrict %d times", n)
	}
	if n := v.calls.Load(); n != 1 {
		t.Errorf("normal op called Verify %d times, want 1", n)
	}

	if w := get(t, r, "/step-up", "Bearer fresh-token"); w.Code != http.StatusOK {
		t.Fatalf("step-up op: %d %s", w.Code, w.Body.String())
	}
	if n := v.strictCalls.Load(); n != 1 {
		t.Errorf("step-up op called VerifyStrict %d times, want 1", n)
	}
	if n := v.calls.Load(); n != 1 {
		t.Errorf("step-up op called Verify %d times, want no extra call", n)
	}
}

func TestStepUp_BadExtensionRejectedAtStartup(t *testing.T) {
	for name, mutate := range map[string]func(*openapi3.T){
		"non-boolean value": func(s *openapi3.T) {
			s.Paths.Value("/step-up").Get.Extensions[stepUpExtension] = "yes"
		},
		"step-up on a public operation": func(s *openapi3.T) {
			op := s.Paths.Value("/public").Get
			if op.Extensions == nil {
				op.Extensions = map[string]any{}
			}
			op.Extensions[stepUpExtension] = true
		},
	} {
		t.Run(name, func(t *testing.T) {
			spec := loadSpec(t, "testdata/auth.yaml")
			mutate(spec)
			if _, err := Authenticate(spec, &fakeVerifier{}, fakeResolver{}); err == nil {
				t.Error("expected startup error")
			}
		})
	}
}

// End to end: real emulator tokens, real users table, real role change.
func TestAuthenticate_AdminEndToEnd(t *testing.T) {
	fb := authtest.Firebase(t)
	svc := users.New(dbtest.Pool(t))
	r := authRouter(t, fb, svc)
	eu := authtest.EmailUser(t)

	if w := get(t, r, "/admin", "Bearer "+eu.Token); w.Code != http.StatusForbidden {
		t.Fatalf("non-admin: status %d, body %s", w.Code, w.Body.String())
	}

	u, err := svc.GetByFirebaseUID(context.Background(), eu.UID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetRole(context.Background(), u.ID, users.RoleAdmin, fb); err != nil {
		t.Fatal(err)
	}
	// The DB role takes effect immediately, even on the old token.
	if w := get(t, r, "/admin", "Bearer "+eu.Token); w.Code != http.StatusOK {
		t.Fatalf("admin: status %d, body %s", w.Code, w.Body.String())
	}
	claims, err := fb.CustomClaims(context.Background(), eu.UID)
	if err != nil || claims["role"] != "admin" {
		t.Errorf("custom claims = %v, err %v", claims, err)
	}
	// A fresh sign-in carries the claim for the frontend.
	fresh := authtest.SignIn(t, eu.Email, eu.Password)
	if w := get(t, r, "/admin", "Bearer "+fresh); w.Code != http.StatusOK {
		t.Errorf("fresh token: status %d", w.Code)
	}
}
