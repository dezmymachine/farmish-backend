package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dezmymachine/farmish-backend/internal/config"
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

func testRouterWith(t *testing.T, db handlers.Pinger) http.Handler {
	t.Helper()
	cfg := config.Config{Env: config.EnvTest, CORSOrigins: []string{"https://farmish.gh"}}
	return NewRouter(cfg, logger.New(&bytes.Buffer{}, "error"), Deps{DB: db})
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

func TestReadyz(t *testing.T) {
	t.Run("db up", func(t *testing.T) {
		w := httptest.NewRecorder()
		testRouterWith(t, fakePinger{}).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if w.Code != http.StatusOK || w.Body.String() != `{"status":"ok"}` {
			t.Errorf("status %d, body %s", w.Code, w.Body.String())
		}
	})

	t.Run("db down", func(t *testing.T) {
		w := httptest.NewRecorder()
		secret := errors.New("dial tcp 10.0.0.5:5432: password authentication failed for user farmish")
		testRouterWith(t, fakePinger{err: secret}).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d", w.Code)
		}
		var env apierror.Envelope
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil || env.Error.Code != apierror.CodeUnavailable {
			t.Errorf("body %s is not an unavailable envelope (%v)", w.Body.String(), err)
		}
		if bytes.Contains(w.Body.Bytes(), []byte("10.0.0.5")) {
			t.Error("dependency error leaked to the client")
		}
	})
}
