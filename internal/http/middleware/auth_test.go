package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/dezmymachine/farmish-backend/internal/auth"
	"github.com/dezmymachine/farmish-backend/internal/auth/authtest"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/users"
)

type fakeVerifier struct {
	calls  atomic.Int32
	tokens map[string]auth.Identity
}

func (f *fakeVerifier) Verify(_ context.Context, token string) (auth.Identity, error) {
	f.calls.Add(1)
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
