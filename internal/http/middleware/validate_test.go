package middleware

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/gin-gonic/gin"

	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
)

func validatorRouter(t *testing.T) *gin.Engine {
	t.Helper()
	data, err := os.ReadFile("testdata/validate.yaml")
	if err != nil {
		t.Fatal(err)
	}
	spec, err := openapi3.NewLoader().LoadFromData(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := spec.Validate(t.Context()); err != nil {
		t.Fatalf("fixture spec invalid: %v", err)
	}
	v, err := OpenAPIValidator(spec)
	if err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	r.HandleMethodNotAllowed = true
	r.Use(v)
	// Echo the body so tests can prove the validator left it readable.
	r.POST("/things/:id", func(c *gin.Context) {
		b, _ := io.ReadAll(c.Request.Body)
		c.Data(http.StatusOK, "application/json", b)
	})
	r.NoRoute(func(c *gin.Context) { c.Status(http.StatusNotFound) })
	r.NoMethod(func(c *gin.Context) { c.Status(http.StatusMethodNotAllowed) })
	return r
}

type call struct {
	method, target, body, contentType string
	headers                           map[string]string
}

func (tc call) do(t *testing.T, r http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(tc.method, tc.target, strings.NewReader(tc.body))
	ct := tc.contentType
	if ct == "" && tc.body != "" {
		ct = "application/json"
	}
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	for k, v := range tc.headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestOpenAPIValidator_Valid(t *testing.T) {
	body := `{"name":"maize","qty":3,"note":null}`
	w := call{method: http.MethodPost, target: "/things/7?limit=10", body: body}.do(t, validatorRouter(t))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", w.Code, w.Body.String())
	}
	if w.Body.String() != body {
		t.Errorf("handler saw body %q, want %q", w.Body.String(), body)
	}
}

func TestOpenAPIValidator_NoAuthEnforcement(t *testing.T) {
	// The fixture requires bearerAuth globally; the validator must not 401.
	w := call{method: http.MethodPost, target: "/things/7?limit=10", body: `{"name":"a","qty":1}`}.do(t, validatorRouter(t))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", w.Code, w.Body.String())
	}
}

func TestOpenAPIValidator_Rejects(t *testing.T) {
	type detail struct{ location, field string }
	tests := []struct {
		name string
		call call
		want []detail // every detail that must be present
	}{
		{
			"bad path param",
			call{method: http.MethodPost, target: "/things/0?limit=10", body: `{"name":"a","qty":1}`},
			[]detail{{"path", "id"}},
		},
		{
			"non-integer path param",
			call{method: http.MethodPost, target: "/things/abc?limit=10", body: `{"name":"a","qty":1}`},
			[]detail{{"path", "id"}},
		},
		{
			"missing required query",
			call{method: http.MethodPost, target: "/things/1", body: `{"name":"a","qty":1}`},
			[]detail{{"query", "limit"}},
		},
		{
			"query over max",
			call{method: http.MethodPost, target: "/things/1?limit=51", body: `{"name":"a","qty":1}`},
			[]detail{{"query", "limit"}},
		},
		{
			"header too long",
			call{method: http.MethodPost, target: "/things/1?limit=1", body: `{"name":"a","qty":1}`, headers: map[string]string{"X-Trace": "toolong"}},
			[]detail{{"header", "X-Trace"}},
		},
		{
			"body: all problems reported",
			call{method: http.MethodPost, target: "/things/1?limit=1", body: `{"name":"","qty":0,"extra":true}`},
			[]detail{{"body", "/name"}, {"body", "/qty"}},
		},
		{
			"body: missing required",
			call{method: http.MethodPost, target: "/things/1?limit=1", body: `{"name":"a"}`},
			[]detail{{"body", ""}},
		},
		{
			"body: missing entirely",
			call{method: http.MethodPost, target: "/things/1?limit=1"},
			[]detail{{"body", ""}},
		},
		{
			"body: malformed JSON",
			call{method: http.MethodPost, target: "/things/1?limit=1", body: `{"name":`},
			[]detail{{"body", ""}},
		},
		{
			"body: wrong content type",
			call{method: http.MethodPost, target: "/things/1?limit=1", body: "name=a", contentType: "application/x-www-form-urlencoded"},
			[]detail{{"body", ""}},
		},
		{
			"path and body together",
			call{method: http.MethodPost, target: "/things/0?limit=1", body: `{"name":"a","qty":0}`},
			[]detail{{"path", "id"}, {"body", "/qty"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := tt.call.do(t, validatorRouter(t))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status %d, body %s", w.Code, w.Body.String())
			}
			var env api.Error
			if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
				t.Fatalf("body %s: %v", w.Body.String(), err)
			}
			if env.Error.Code != apierror.CodeValidationFailed || env.Error.Details == nil {
				t.Fatalf("not a validation_failed envelope with details: %s", w.Body.String())
			}
			var got []detail
			for _, d := range *env.Error.Details {
				if d.Message == "" || strings.Contains(d.Message, "at '") || strings.Contains(d.Message, "error at") ||
					strings.Contains(d.Message, "Schema:") || strings.Contains(d.Message, "\n") {
					t.Errorf("message %q is empty or carries validator noise", d.Message)
				}
				if !d.Location.Valid() {
					t.Errorf("location %q not in the spec enum", d.Location)
				}
				f := ""
				if d.Field != nil {
					f = *d.Field
				}
				got = append(got, detail{string(d.Location), f})
			}
			for _, want := range tt.want {
				if want.field == "" {
					if !slices.ContainsFunc(got, func(g detail) bool { return g.location == want.location }) {
						t.Errorf("no %s detail in %v", want.location, got)
					}
					continue
				}
				if !slices.Contains(got, want) {
					t.Errorf("missing detail %v in %v (%s)", want, got, w.Body.String())
				}
			}
		})
	}
}

func TestOpenAPIValidator_PassesThroughUnknownRoutes(t *testing.T) {
	r := validatorRouter(t)
	if w := (call{method: http.MethodGet, target: "/unknown"}).do(t, r); w.Code != http.StatusNotFound {
		t.Errorf("unknown path: status %d", w.Code)
	}
	if w := (call{method: http.MethodDelete, target: "/things/1"}).do(t, r); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("unknown method: status %d", w.Code)
	}
}
