package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	legacyrouter "github.com/getkin/kin-openapi/routers/legacy"
	"github.com/gin-gonic/gin"

	"github.com/dezmymachine/farmish-backend/internal/config"
	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/internal/http/handlers"
	"github.com/dezmymachine/farmish-backend/internal/http/middleware"
	"github.com/dezmymachine/farmish-backend/pkg/logger"
)

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

func testRouter(t *testing.T) http.Handler {
	t.Helper()
	return testRouterWith(t, fakePinger{})
}

func testRouterWith(t *testing.T, db handlers.Pinger) *gin.Engine {
	t.Helper()
	cfg := config.Config{Env: config.EnvTest, CORSOrigins: []string{"https://farmish.gh"}}
	r, err := NewRouter(cfg, logger.New(io.Discard, "error"), Deps{DB: db})
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
	spec, err := api.GetSpec()
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
	spec, err := api.GetSpec()
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
