package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/dezmymachine/farmish-backend/internal/auth/authtest"
	"github.com/dezmymachine/farmish-backend/internal/catalog"
	"github.com/dezmymachine/farmish-backend/internal/config"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/metrics"
	"github.com/dezmymachine/farmish-backend/internal/users"
)

var wantSecurityHeaders = map[string]string{
	"X-Content-Type-Options": "nosniff",
	"Referrer-Policy":        "no-referrer",
	"X-Frame-Options":        "DENY",
}

func assertSecurityHeaders(t *testing.T, label string, h http.Header) {
	t.Helper()
	for k, want := range wantSecurityHeaders {
		if got := h.Get(k); got != want {
			t.Errorf("%s: %s = %q, want %q", label, k, got, want)
		}
	}
}

func TestSecurityHeaders_OnEveryKindOfResponse(t *testing.T) {
	r := testRouter(t)
	preflight := httptest.NewRequest(http.MethodOptions, "/v1/me", nil)
	preflight.Header.Set("Origin", "https://farmish.gh")
	preflight.Header.Set("Access-Control-Request-Method", "GET")
	for name, req := range map[string]*http.Request{
		"probe 200":      httptest.NewRequest(http.MethodGet, "/healthz", nil),
		"404":            httptest.NewRequest(http.MethodGet, "/nowhere", nil),
		"401":            httptest.NewRequest(http.MethodGet, "/v1/me", nil),
		"405":            httptest.NewRequest(http.MethodDelete, "/healthz", nil),
		"CORS preflight": preflight,
	} {
		assertSecurityHeaders(t, name, serve(t, r, req).Header())
	}
}

func TestSecurityHeaders_HSTSOnlyWhenDeployed(t *testing.T) {
	for env, want := range map[config.Env]string{
		config.EnvTest:        "",
		config.EnvDevelopment: "",
		config.EnvStaging:     "max-age=31536000; includeSubDomains",
		config.EnvProduction:  "max-age=31536000; includeSubDomains",
	} {
		r := newTestRouterWithConfig(t, Deps{DB: fakePinger{}, Verifier: rejectAll{}, Users: users.New(nil)},
			config.Config{Env: env})
		w := serve(t, r, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if got := w.Header().Get("Strict-Transport-Security"); got != want {
			t.Errorf("%s: Strict-Transport-Security = %q, want %q", env, got, want)
		}
	}
}

func TestSecurityHeaders_CacheControl(t *testing.T) {
	r, _, _ := catalogRouter(t)
	eu := authtest.EmailUser(t)

	authed := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	authed.Header.Set("Authorization", "Bearer "+eu.Token)
	if got := serve(t, r, authed).Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("authenticated /v1/me: Cache-Control = %q, want no-store", got)
	}

	// A rejected credential is still a credentialed request: its error must
	// not be cached either.
	bad := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	bad.Header.Set("Authorization", "Bearer not-a-token")
	w := serve(t, r, bad)
	if w.Code != http.StatusUnauthorized || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("bad token: %d, Cache-Control = %q", w.Code, w.Header().Get("Cache-Control"))
	}

	// A handler's own policy replaces the default, even for a signed-in caller.
	pub := httptest.NewRequest(http.MethodGet, "/v1/categories", nil)
	pub.Header.Set("Authorization", "Bearer "+eu.Token)
	if got := serve(t, r, pub).Header().Get("Cache-Control"); got != "public, max-age=300" {
		t.Errorf("public catalog with a token: Cache-Control = %q, want the handler's policy", got)
	}

	// No credentials, no default: the header stays the handler's business.
	if got := serve(t, r, httptest.NewRequest(http.MethodGet, "/healthz", nil)).Header().Get("Cache-Control"); got != "" {
		t.Errorf("anonymous /healthz: Cache-Control = %q, want none", got)
	}
}

// catalogPool is a fresh database with the catalog seeded.
func catalogPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := dbtest.Pool(t)
	if err := catalog.Seed(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestHTTPMetrics_RouteTemplateNotRawPath(t *testing.T) {
	pool := catalogPool(t)
	r := newTestRouter(t, Deps{DB: fakePinger{}, Verifier: rejectAll{}, Users: users.New(pool), Catalog: catalog.New(pool)})

	tmpl := metrics.HTTPRequests.WithLabelValues("GET", "/v1/categories/:slug", "200")
	before := testutil.ToFloat64(tmpl)
	for _, slug := range []string{"livestock-poultry-cattle", "seeds-seedlings", "irrigation"} {
		w := serve(t, r, httptest.NewRequest(http.MethodGet, "/v1/categories/"+slug, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d", slug, w.Code)
		}
	}
	if got := testutil.ToFloat64(tmpl) - before; got != 3 {
		t.Errorf("route-template counter moved by %v, want 3", got)
	}
	// One series for the template. A series per slug would be a cardinality bomb.
	for _, slug := range []string{"livestock-poultry-cattle", "seeds-seedlings", "irrigation"} {
		if testutil.ToFloat64(metrics.HTTPRequests.WithLabelValues("GET", "/v1/categories/"+slug, "200")) != 0 {
			t.Errorf("raw path /v1/categories/%s leaked into the route label", slug)
		}
	}

	unmatched := metrics.HTTPRequests.WithLabelValues("GET", "unmatched", "404")
	before = testutil.ToFloat64(unmatched)
	serve(t, r, httptest.NewRequest(http.MethodGet, "/scanner/probe/1", nil))
	serve(t, r, httptest.NewRequest(http.MethodGet, "/scanner/probe/2", nil))
	if got := testutil.ToFloat64(unmatched) - before; got != 2 {
		t.Errorf("unmatched counter moved by %v, want 2 (random paths share one label)", got)
	}
}

func TestHTTPMetrics_ProbesAreNotCounted(t *testing.T) {
	r := testRouter(t)
	probe := metrics.HTTPRequests.WithLabelValues("GET", "/healthz", "200")
	before := testutil.ToFloat64(probe)
	for range 5 {
		serve(t, r, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	}
	if got := testutil.ToFloat64(probe) - before; got != 0 {
		t.Errorf("health probes moved http_requests_total by %v; they would drown the 5xx ratio", got)
	}
}

func TestMetricsRoute_NotOnThePublicRouter(t *testing.T) {
	r := testRouter(t)
	for _, path := range []string{"/metrics", "/v1/metrics", "/debug/metrics"} {
		w := serve(t, r, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404: metrics live on the private listener only", path, w.Code)
		}
	}
}
