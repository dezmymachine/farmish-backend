package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	legacyrouter "github.com/getkin/kin-openapi/routers/legacy"
	"github.com/gin-gonic/gin"

	"github.com/dezmymachine/farmish-backend/internal/auth"
	"github.com/dezmymachine/farmish-backend/internal/config"
	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/internal/http/handlers"
	"github.com/dezmymachine/farmish-backend/internal/http/middleware"
	"github.com/dezmymachine/farmish-backend/internal/ratelimit"
	"github.com/dezmymachine/farmish-backend/internal/turnstile"
	"github.com/dezmymachine/farmish-backend/internal/users"
	"github.com/dezmymachine/farmish-backend/pkg/logger"
)

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

func testRouter(t *testing.T) http.Handler {
	t.Helper()
	return testRouterWith(t, fakePinger{})
}

// rejectAll is a Verifier for tests that never authenticate anyone.
type rejectAll struct{}

func (rejectAll) Verify(context.Context, string) (auth.Identity, error) {
	return auth.Identity{}, auth.ErrInvalidToken
}

func (rejectAll) VerifyStrict(context.Context, string) (auth.Identity, error) {
	return auth.Identity{}, auth.ErrInvalidToken
}

// noTurnstile rejects every token (no production operation uses Turnstile yet).
type noTurnstile struct{}

func (noTurnstile) Verify(context.Context, string, netip.Addr) error { return turnstile.ErrFailed }

func testRouterWith(t *testing.T, db handlers.Pinger) *gin.Engine {
	t.Helper()
	return newTestRouter(t, Deps{DB: db, Verifier: rejectAll{}, Users: users.New(nil)})
}

func newTestRouter(t *testing.T, deps Deps) *gin.Engine {
	t.Helper()
	return newTestRouterWithConfig(t, deps,
		config.Config{Env: config.EnvTest, CORSOrigins: []string{"https://farmish.gh"}})
}

// newTestRouterWithConfig is newTestRouter with a custom config, for the tests
// that need a setting the default test config leaves empty (a Paystack secret,
// for the webhook signature).
func newTestRouterWithConfig(t *testing.T, deps Deps, cfg config.Config) *gin.Engine {
	t.Helper()
	if cfg.CORSOrigins == nil {
		cfg.CORSOrigins = []string{"https://farmish.gh"}
	}
	if deps.Turnstile == nil {
		deps.Turnstile = noTurnstile{}
	}
	out := io.Discard
	if os.Getenv("QA_DEBUG_LOG") != "" {
		out = os.Stdout
	}
	r, err := NewRouter(cfg, logger.New(out, "debug"), deps)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func serve(t *testing.T, h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// assertContract validates a recorded response against api/openapi.yaml.
func assertContract(t *testing.T, req *http.Request, w *httptest.ResponseRecorder) {
	t.Helper()
	spec, err := api.CachedSpec()
	if err != nil {
		t.Fatal(err)
	}
	spec.Servers = nil
	router, err := legacyrouter.NewRouter(spec)
	if err != nil {
		t.Fatal(err)
	}
	route, params, err := router.FindRoute(req)
	if err != nil {
		t.Fatalf("%s %s not in spec: %v", req.Method, req.URL.Path, err)
	}
	err = openapi3filter.ValidateResponse(context.Background(), &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route},
		Status:                 w.Code,
		Header:                 w.Header(),
		Body:                   io.NopCloser(bytes.NewReader(w.Body.Bytes())),
		Options:                &openapi3filter.Options{IncludeResponseStatus: true},
	})
	if err != nil {
		t.Errorf("response violates contract: %v\nbody: %s", err, w.Body.String())
	}
}

// assertErrorEnvelope checks body against the spec's Error schema and code.
func assertErrorEnvelope(t *testing.T, body []byte, code string) api.Error {
	t.Helper()
	spec, err := api.CachedSpec()
	if err != nil {
		t.Fatal(err)
	}
	var raw any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("body %s is not JSON: %v", body, err)
	}
	if err := spec.Components.Schemas["Error"].Value.VisitJSON(raw, openapi3.MultiErrors()); err != nil {
		t.Errorf("body %s violates the Error schema: %v", body, err)
	}
	var env api.Error
	_ = json.Unmarshal(body, &env)
	if env.Error.Code != code {
		t.Errorf("code = %q, want %q (body %s)", env.Error.Code, code, body)
	}
	return env
}

func TestHealthz(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := serve(t, testRouter(t), req)

	if w.Code != http.StatusOK || w.Body.String() != "{\"status\":\"ok\"}\n" {
		t.Fatalf("status %d, body %q", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if w.Header().Get(middleware.RequestIDHeader) == "" {
		t.Error("missing request ID header")
	}
	assertContract(t, req, w)
}

func TestReadyz(t *testing.T) {
	t.Run("db up", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
		w := serve(t, testRouterWith(t, fakePinger{}), req)
		if w.Code != http.StatusOK {
			t.Fatalf("status %d, body %s", w.Code, w.Body.String())
		}
		assertContract(t, req, w)
	})

	t.Run("db down", func(t *testing.T) {
		secret := errors.New("dial tcp 10.0.0.5:5432: password authentication failed for user farmish")
		req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
		w := serve(t, testRouterWith(t, fakePinger{err: secret}), req)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d", w.Code)
		}
		assertErrorEnvelope(t, w.Body.Bytes(), apierror.CodeUnavailable)
		if bytes.Contains(w.Body.Bytes(), []byte("10.0.0.5")) {
			t.Error("dependency error leaked to the client")
		}
		assertContract(t, req, w)
	})
}

func TestUnroutedRequests(t *testing.T) {
	tests := []struct {
		method, path string
		status       int
		code         string
	}{
		{http.MethodGet, "/nope", http.StatusNotFound, apierror.CodeNotFound},
		{http.MethodPost, "/healthz", http.StatusMethodNotAllowed, apierror.CodeMethodNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			w := serve(t, testRouter(t), httptest.NewRequest(tt.method, tt.path, nil))
			if w.Code != tt.status {
				t.Fatalf("status = %d, want %d", w.Code, tt.status)
			}
			assertErrorEnvelope(t, w.Body.Bytes(), tt.code)
		})
	}
}

// Probes are public: no Authorization header is needed, and a garbage one is
// ignored (auth middleware, not the validator, owns 401s).
func TestProbesArePublic(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Authorization", "Bearer garbage")
	if w := serve(t, testRouter(t), req); w.Code != http.StatusOK {
		t.Errorf("status = %d", w.Code)
	}
}

func TestInternalErrorHandlerHidesError(t *testing.T) {
	r := gin.New()
	r.GET("/", func(c *gin.Context) { internalError("boom")(c, errors.New("pq: secret table missing")) })
	w := serve(t, r, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", w.Code)
	}
	assertErrorEnvelope(t, w.Body.Bytes(), apierror.CodeInternal)
	if bytes.Contains(w.Body.Bytes(), []byte("secret")) {
		t.Error("handler error leaked to the client")
	}
}

func TestRequestErrorHandler(t *testing.T) {
	r := gin.New()
	r.GET("/", func(c *gin.Context) { requestError(c, errors.New("json: cannot unmarshal")) })
	w := serve(t, r, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", w.Code)
	}
	assertErrorEnvelope(t, w.Body.Bytes(), apierror.CodeBadRequest)
}

// RUN_MODE=worker serves health probes only; API routes don't exist there.
func TestProbeRouter(t *testing.T) {
	probeCfg := config.Config{Env: config.EnvTest}
	r := NewProbeRouter(probeCfg, logger.New(io.Discard, "error"), fakePinger{})
	for _, path := range []string{"/healthz", "/readyz"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := serve(t, r, req)
		if w.Code != http.StatusOK {
			t.Errorf("%s: status %d", path, w.Code)
		}
		assertContract(t, req, w)
	}
	w := serve(t, r, httptest.NewRequest(http.MethodGet, "/v1/me", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("/v1/me on probe router: status %d", w.Code)
	}
	assertErrorEnvelope(t, w.Body.Bytes(), apierror.CodeNotFound)

	down := NewProbeRouter(probeCfg, logger.New(io.Discard, "error"), fakePinger{err: errors.New("db down")})
	if w := serve(t, down, httptest.NewRequest(http.MethodGet, "/readyz", nil)); w.Code != http.StatusServiceUnavailable {
		t.Errorf("readyz with db down: %d", w.Code)
	}
}

// Through the real router: the per-IP limit returns a contract-valid 429,
// and the health probes are never limited.
func TestRouter_RateLimited(t *testing.T) {
	limits := middleware.DefaultRateLimits()
	limits.IP = ratelimit.Rule{Name: "ip", Limit: 1, Period: time.Minute, Burst: 2}
	r := newTestRouter(t, Deps{DB: fakePinger{}, Verifier: rejectAll{}, Users: users.New(nil), RateLimits: &limits})

	for range 2 {
		if w := serve(t, r, httptest.NewRequest(http.MethodGet, "/v1/me", nil)); w.Code != http.StatusUnauthorized {
			t.Fatalf("status %d", w.Code)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	req.Header.Set("Origin", "https://farmish.gh")
	w := serve(t, r, req)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("status %d, headers %v", w.Code, w.Header())
	}
	assertErrorEnvelope(t, w.Body.Bytes(), apierror.CodeRateLimited)
	assertContract(t, req, w)
	// Browsers can only read the 429 (and Retry-After) with CORS headers.
	if w.Header().Get("Access-Control-Allow-Origin") != "https://farmish.gh" ||
		!strings.Contains(w.Header().Get("Access-Control-Expose-Headers"), "Retry-After") {
		t.Errorf("429 lacks CORS headers: %v", w.Header())
	}

	for range 5 {
		if w := serve(t, r, httptest.NewRequest(http.MethodGet, "/healthz", nil)); w.Code != http.StatusOK {
			t.Fatalf("probe limited: %d", w.Code)
		}
	}
}
