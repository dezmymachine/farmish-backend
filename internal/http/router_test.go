package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dezmymachine/farmish-backend/internal/config"
	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/internal/http/middleware"
	"github.com/dezmymachine/farmish-backend/pkg/logger"
)

func testRouter(t *testing.T) http.Handler {
	t.Helper()
	cfg := config.Config{Env: config.EnvTest, CORSOrigins: []string{"https://farmish.gh"}}
	return NewRouter(cfg, logger.New(&bytes.Buffer{}, "error"))
}

func TestHealthz(t *testing.T) {
	w := httptest.NewRecorder()
	testRouter(t).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if w.Body.String() != `{"status":"ok"}` {
		t.Errorf("body = %s", w.Body.String())
	}
	if w.Header().Get(middleware.RequestIDHeader) == "" {
		t.Error("missing request ID header")
	}
}

func TestErrorEnvelopes(t *testing.T) {
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
			w := httptest.NewRecorder()
			testRouter(t).ServeHTTP(w, httptest.NewRequest(tt.method, tt.path, nil))
			if w.Code != tt.status {
				t.Fatalf("status = %d, want %d", w.Code, tt.status)
			}
			var env apierror.Envelope
			if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil || env.Error.Code != tt.code || env.Error.Message == "" {
				t.Errorf("body %s is not a %s envelope (%v)", w.Body.String(), tt.code, err)
			}
		})
	}
}
