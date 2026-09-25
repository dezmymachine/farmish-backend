package middleware

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/pkg/logger"
)

func init() { gin.SetMode(gin.TestMode) }

// logLines decodes each JSON log line written to buf.
func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad log line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func TestRequestID(t *testing.T) {
	var buf bytes.Buffer
	r := gin.New()
	r.Use(RequestID(logger.New(&buf, "info")))
	r.GET("/", func(c *gin.Context) {
		logger.FromContext(c.Request.Context()).Info("inside")
		c.String(http.StatusOK, GetRequestID(c))
	})

	t.Run("generates when absent", func(t *testing.T) {
		buf.Reset()
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
		id := w.Header().Get(RequestIDHeader)
		if len(id) != 32 || w.Body.String() != id {
			t.Fatalf("header %q, body %q", id, w.Body.String())
		}
		if got := logLines(t, &buf)[0]["request_id"]; got != id {
			t.Errorf("logger request_id = %v, want %s", got, id)
		}
	})

	t.Run("reuses well-formed inbound", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set(RequestIDHeader, "abc-123")
		r.ServeHTTP(w, req)
		if got := w.Header().Get(RequestIDHeader); got != "abc-123" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("replaces malformed inbound", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set(RequestIDHeader, "bad id\nwith newline")
		r.ServeHTTP(w, req)
		if got := w.Header().Get(RequestIDHeader); len(got) != 32 {
			t.Errorf("got %q", got)
		}
	})
}

func TestRecoveryAndAccessLog(t *testing.T) {
	var buf bytes.Buffer
	r := gin.New()
	r.Use(RequestID(logger.New(&buf, "info")), AccessLog(), Recovery())
	r.GET("/boom", func(*gin.Context) { panic("kaboom") })
	r.GET("/ok", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/boom?phone=0240000000", nil))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", w.Code)
	}
	var env apierror.Envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil || env.Error.Code != apierror.CodeInternal {
		t.Fatalf("body %q not an internal_error envelope (%v)", w.Body.String(), err)
	}
	if strings.Contains(w.Body.String(), "kaboom") {
		t.Error("panic value leaked to the client")
	}

	lines := logLines(t, &buf)
	if len(lines) != 2 {
		t.Fatalf("want panic + access log lines, got %d: %s", len(lines), buf.String())
	}
	if lines[0]["msg"] != "panic recovered" || lines[0]["level"] != "ERROR" {
		t.Errorf("unexpected panic log: %v", lines[0])
	}
	access := lines[1]
	if access["msg"] != "http_request" || access["status"] != float64(500) || access["route"] != "/boom" || access["level"] != "ERROR" {
		t.Errorf("unexpected access log: %v", access)
	}
	if strings.Contains(buf.String(), "0240000000") {
		t.Error("query string leaked into logs")
	}

	buf.Reset()
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ok", nil))
	if l := logLines(t, &buf); len(l) != 1 || l[0]["status"] != float64(204) || l[0]["level"] != "INFO" {
		t.Errorf("unexpected access log: %v", l)
	}
}

func TestRecovery_RepanicsAbortHandler(t *testing.T) {
	r := gin.New()
	r.Use(Recovery())
	r.GET("/", func(*gin.Context) { panic(http.ErrAbortHandler) })

	defer func() {
		if err, _ := recover().(error); !errors.Is(err, http.ErrAbortHandler) {
			t.Errorf("recovered %v, want http.ErrAbortHandler", err)
		}
	}()
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	t.Error("expected re-panic")
}

func TestCORS(t *testing.T) {
	r := gin.New()
	r.Use(CORS([]string{"https://farmish.gh"}))
	r.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })

	preflight := func(origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodOptions, "/", nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", http.MethodGet)
		req.Header.Set("Access-Control-Request-Headers", "Authorization")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	w := preflight("https://farmish.gh")
	if w.Code != http.StatusNoContent || w.Header().Get("Access-Control-Allow-Origin") != "https://farmish.gh" {
		t.Errorf("allowed origin: status %d, headers %v", w.Code, w.Header())
	}
	if w.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Error("credentials must not be allowed")
	}

	w = preflight("https://evil.example")
	if w.Code != http.StatusForbidden || w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("disallowed origin: status %d, headers %v", w.Code, w.Header())
	}
}
