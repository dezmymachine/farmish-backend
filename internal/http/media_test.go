package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/auth"
	"github.com/dezmymachine/farmish-backend/internal/auth/authtest"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/internal/http/middleware"
	"github.com/dezmymachine/farmish-backend/internal/media"
	"github.com/dezmymachine/farmish-backend/internal/media/mediatest"
	"github.com/dezmymachine/farmish-backend/internal/ratelimit"
	"github.com/dezmymachine/farmish-backend/internal/users"
)

// mediaRouter builds the router with a raised `sensitive` budget so a test
// can make many upload requests; the policy itself is asserted separately.
func mediaRouter(t *testing.T) (*gin.Engine, *pgxpool.Pool, *auth.Firebase, *media.R2) {
	t.Helper()
	r, pool, fb, store := mediaRouterWithLimits(t, nil)
	return r, pool, fb, store
}

func mediaRouterWithLimits(t *testing.T, limits *middleware.RateLimits) (*gin.Engine, *pgxpool.Pool, *auth.Firebase, *media.R2) {
	t.Helper()
	fb := authtest.Firebase(t)
	pool := dbtest.Pool(t)
	store := mediatest.R2(t)
	deps := Deps{DB: fakePinger{}, Verifier: fb, Users: users.New(pool), Media: media.New(pool, store)}
	if limits != nil {
		deps.RateLimits = limits
		deps.SharedLimiter = ratelimit.NewMemory()
	}
	r := newTestRouter(t, deps)
	return r, pool, fb, store
}

func uploadURLBody(t *testing.T, w *httptest.ResponseRecorder) api.MediaUpload {
	t.Helper()
	var up api.MediaUpload
	if err := json.Unmarshal(w.Body.Bytes(), &up); err != nil {
		t.Fatalf("body %s: %v", w.Body.String(), err)
	}
	return up
}

func TestCreateMediaUploadUrl_EndToEnd(t *testing.T) {
	r, pool, fb, _ := mediaRouter(t)
	_, tok := authUser(t, pool, fb)

	req := jsonRequest(http.MethodPost, "/v1/media/upload-url", tok,
		`{"purpose":"listing_image","contentType":"image/jpeg","sizeBytes":2048}`)
	w := serve(t, r, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	up := uploadURLBody(t, w)
	if up.Method != api.MediaUploadMethodPUT || up.Headers["Content-Type"] != "image/jpeg" ||
		up.Headers["Content-Length"] != "2048" || up.ExpiresAt.IsZero() {
		t.Fatalf("upload = %+v", up)
	}
	if !strings.Contains(up.UploadUrl, "X-Amz-Signature=") || !strings.Contains(up.PublicUrl, "listings/") {
		t.Errorf("upload = %+v", up)
	}
	// The presigned PUT works with exactly those headers.
	body := strings.Repeat("a", 2048)
	putReq, err := http.NewRequest(http.MethodPut, up.UploadUrl, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	putReq.ContentLength = int64(len(body))
	for k, v := range up.Headers {
		putReq.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(putReq)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("presigned PUT: %d", resp.StatusCode)
	}
	// One row per upload, owned by the caller.
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM media_objects WHERE id = $1`, up.MediaId).Scan(&n); err != nil || n != 1 {
		t.Errorf("media_objects row for %s: n=%d err=%v", up.MediaId, n, err)
	}
}

func TestCreateMediaUploadUrl_RejectsTypeAndSize(t *testing.T) {
	// Raised budget: this test makes six requests, and the sensitive policy
	// is asserted on its own below.
	limits := middleware.DefaultRateLimits()
	limits.Operation["sensitive"] = ratelimit.Rule{Name: "sensitive", Limit: 100, Period: time.Minute, Burst: 50}
	r, pool, fb, _ := mediaRouterWithLimits(t, &limits)
	_, tok := authUser(t, pool, fb)

	for name, body := range map[string]string{
		"gif":           `{"purpose":"listing_image","contentType":"image/gif","sizeBytes":100}`,
		"pdf":           `{"purpose":"listing_image","contentType":"application/pdf","sizeBytes":100}`,
		"zero":          `{"purpose":"listing_image","contentType":"image/jpeg","sizeBytes":0}`,
		"over 5MB":      `{"purpose":"listing_image","contentType":"image/jpeg","sizeBytes":5242881}`,
		"bad purpose":   `{"purpose":"avatar","contentType":"image/jpeg","sizeBytes":100}`,
		"unknown field": `{"purpose":"listing_image","contentType":"image/jpeg","sizeBytes":100,"acl":"public-read"}`,
	} {
		t.Run(name, func(t *testing.T) {
			req := jsonRequest(http.MethodPost, "/v1/media/upload-url", tok, body)
			w := serve(t, r, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status %d, body %s, headers %v", w.Code, w.Body.String(), w.Header())
			}
			assertErrorEnvelope(t, w.Body.Bytes(), apierror.CodeValidationFailed)
			assertContract(t, req, w)
		})
	}
}

func TestCreateMediaUploadUrl_Unauthenticated401(t *testing.T) {
	r, _, _, _ := mediaRouter(t)
	req := jsonRequest(http.MethodPost, "/v1/media/upload-url", "",
		`{"purpose":"listing_image","contentType":"image/jpeg","sizeBytes":100}`)
	w := serve(t, r, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, body %s", w.Code, w.Body.String())
	}
	assertErrorEnvelope(t, w.Body.Bytes(), apierror.CodeUnauthorized)
	assertContract(t, req, w)

	// A garbage token is the same generic 401.
	req = jsonRequest(http.MethodPost, "/v1/media/upload-url", "garbage",
		`{"purpose":"listing_image","contentType":"image/jpeg","sizeBytes":100}`)
	if w := serve(t, r, req); w.Code != http.StatusUnauthorized {
		t.Errorf("garbage token: %d", w.Code)
	}
}

// The upload endpoint is marked `sensitive`, so the per-operation budget
// applies (5/min burst by default).
func TestCreateMediaUploadUrl_SensitiveRateLimit(t *testing.T) {
	r, pool, fb, _ := mediaRouter(t)
	_, tok := authUser(t, pool, fb)
	body := `{"purpose":"listing_image","contentType":"image/jpeg","sizeBytes":100}`

	var limited *httptest.ResponseRecorder
	for i := range 7 {
		w := serve(t, r, jsonRequest(http.MethodPost, "/v1/media/upload-url", tok, body))
		if w.Code == http.StatusTooManyRequests {
			limited = w
			break
		}
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	if limited == nil {
		t.Fatal("sensitive policy never limited 7 upload requests")
	}
	assertErrorEnvelope(t, limited.Body.Bytes(), apierror.CodeRateLimited)
	if limited.Header().Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}
}
