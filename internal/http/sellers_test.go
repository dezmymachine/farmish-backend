package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/auth"
	"github.com/dezmymachine/farmish-backend/internal/auth/authtest"
	"github.com/dezmymachine/farmish-backend/internal/crypto"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/internal/http/handlers"
	"github.com/dezmymachine/farmish-backend/internal/sellers"
	"github.com/dezmymachine/farmish-backend/internal/users"
)

// ginLeakStore records whether any service call received gin's pooled
// *gin.Context, which must never escape the handler (see requestContext).
type ginLeakStore struct {
	ginSeen atomic.Bool
}

func (f *ginLeakStore) check(ctx context.Context) {
	if _, ok := ctx.(*gin.Context); ok {
		f.ginSeen.Store(true)
	}
}

func (f *ginLeakStore) GetMine(ctx context.Context, id uuid.UUID) (sellers.Profile, error) {
	f.check(ctx)
	return sellers.Profile{UserID: id}, nil
}

func (f *ginLeakStore) UpsertMine(ctx context.Context, id uuid.UUID, _ sellers.ProfileInput) (sellers.Profile, error) {
	f.check(ctx)
	return sellers.Profile{UserID: id}, nil
}

func (f *ginLeakStore) GetPublic(ctx context.Context, id uuid.UUID) (sellers.PublicProfile, error) {
	f.check(ctx)
	return sellers.PublicProfile{UserID: id}, nil
}

func (f *ginLeakStore) ListByStatus(ctx context.Context, _ string, _, _ int32) ([]sellers.AdminProfile, int64, error) {
	f.check(ctx)
	return nil, 0, nil
}

func (f *ginLeakStore) Verify(ctx context.Context, _, _ uuid.UUID, _, _ string) (sellers.AdminProfile, error) {
	f.check(ctx)
	return sellers.AdminProfile{}, nil
}

var _ handlers.SellerStore = (*ginLeakStore)(nil)

func TestSellersHandlers_DoNotLeakGinContext(t *testing.T) {
	fb := authtest.Firebase(t)
	pool := dbtest.Pool(t)
	store := &ginLeakStore{}
	r := newTestRouter(t, Deps{DB: fakePinger{}, Verifier: fb, Users: users.New(pool), Sellers: store})
	u, tok := authUser(t, pool, fb)
	_, adminTok := authAdmin(t, pool, fb)

	serve(t, r, jsonRequest(http.MethodPut, "/v1/me/seller-profile", tok, validProfileBody))
	serve(t, r, jsonRequest(http.MethodPost, "/v1/admin/sellers/"+u.ID.String()+"/verification", adminTok,
		`{"decision":"approve"}`))
	if store.ginSeen.Load() {
		t.Error("a sellers handler passed gin's pooled *gin.Context to the service")
	}
}

func sellersRouter(t *testing.T) (*gin.Engine, *pgxpool.Pool, *auth.Firebase) {
	t.Helper()
	fb := authtest.Firebase(t)
	pool := dbtest.Pool(t)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	c, err := crypto.New(key)
	if err != nil {
		t.Fatal(err)
	}
	r := newTestRouter(t, Deps{
		DB: fakePinger{}, Verifier: fb, Users: users.New(pool),
		Sellers: sellers.New(pool, c, fb),
	})
	return r, pool, fb
}

// authUser creates an emulator account plus its users row.
func authUser(t *testing.T, pool *pgxpool.Pool, fb *auth.Firebase) (users.User, string) {
	t.Helper()
	ctx := context.Background()
	eu := authtest.EmailUser(t)
	id, err := fb.Verify(ctx, eu.Token)
	if err != nil {
		t.Fatal(err)
	}
	u, err := users.New(pool).Resolve(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return u, eu.Token
}

func authAdmin(t *testing.T, pool *pgxpool.Pool, fb *auth.Firebase) (users.User, string) {
	t.Helper()
	u, tok := authUser(t, pool, fb)
	if _, err := users.New(pool).SetRole(context.Background(), u.ID, users.RoleAdmin, fb); err != nil {
		t.Fatal(err)
	}
	return u, tok
}

func jsonRequest(method, path, token, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

func errorDetails(t *testing.T, w *httptest.ResponseRecorder) []api.ErrorDetail {
	t.Helper()
	var env api.Error
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("body %s is not an error envelope: %v", w.Body.String(), err)
	}
	if env.Error.Details == nil {
		t.Fatalf("body %s has no details", w.Body.String())
	}
	return *env.Error.Details
}

const validProfileBody = `{"businessName":"Akosua Farms","region":"Ashanti","district":"Kumasi Metro",` +
	`"bio":"Maize and cassava.","showPhone":true,"whatsapp":"0241234567",` +
	`"idType":"ghana_card","idNumber":"GHA-123456789-0"}`

func TestMySellerProfile_CRUD(t *testing.T) {
	r, pool, fb := sellersRouter(t)
	u, tok := authUser(t, pool, fb)

	// No profile yet: 404.
	req := jsonRequest(http.MethodGet, "/v1/me/seller-profile", tok, "")
	w := serve(t, r, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("get before create: %d %s", w.Code, w.Body.String())
	}
	assertErrorEnvelope(t, w.Body.Bytes(), apierror.CodeNotFound)
	assertContract(t, req, w)

	// Anonymous: 401.
	if w := serve(t, r, jsonRequest(http.MethodGet, "/v1/me/seller-profile", "", "")); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", w.Code)
	}

	// Create with an ID: pending, last4 only.
	req = jsonRequest(http.MethodPut, "/v1/me/seller-profile", tok, validProfileBody)
	w = serve(t, r, req)
	if w.Code != http.StatusOK {
		t.Fatalf("put: %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if raw["verificationStatus"] != "pending" || raw["idNumberLast4"] != "89-0" {
		t.Errorf("profile = %v", raw)
	}
	for _, k := range []string{"idNumber", "userId"} {
		if _, ok := raw[k]; ok {
			t.Errorf("owner response leaks %q", k)
		}
	}

	// Read back: same profile.
	req = jsonRequest(http.MethodGet, "/v1/me/seller-profile", tok, "")
	w = serve(t, r, req)
	if w.Code != http.StatusOK {
		t.Fatalf("get: %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var me api.SellerProfile
	if err := json.Unmarshal(w.Body.Bytes(), &me); err != nil || me.BusinessName != "Akosua Farms" {
		t.Errorf("get = %+v, %v", me, err)
	}
	if u.ID == uuid.Nil {
		t.Error("missing user id")
	}
}

func TestSellerProfile_Validation(t *testing.T) {
	cases := map[string]string{
		"bad region":              `{"businessName":"Akosua Farms","region":"Accra","district":"Kumasi Metro"}`,
		"idType without idNumber": `{"businessName":"Akosua Farms","region":"Ashanti","district":"Kumasi Metro","idType":"ghana_card"}`,
		"bad whatsapp":            `{"businessName":"Akosua Farms","region":"Ashanti","district":"Kumasi Metro","whatsapp":"+2348012345678"}`,
		"unknown field":           `{"businessName":"Akosua Farms","region":"Ashanti","district":"Kumasi Metro","role":"admin"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			r, pool, fb := sellersRouter(t)
			_, tok := authUser(t, pool, fb)
			req := jsonRequest(http.MethodPut, "/v1/me/seller-profile", tok, body)
			w := serve(t, r, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status %d, body %s", w.Code, w.Body.String())
			}
			assertErrorEnvelope(t, w.Body.Bytes(), apierror.CodeValidationFailed)
			if len(errorDetails(t, w)) == 0 {
				t.Error("no field details")
			}
			assertContract(t, req, w)
		})
	}
}

func TestPublicSeller_SafeProjection(t *testing.T) {
	r, pool, fb := sellersRouter(t)
	u, tok := authUser(t, pool, fb)
	_, adminTok := authAdmin(t, pool, fb)

	req := jsonRequest(http.MethodPut, "/v1/me/seller-profile", tok, validProfileBody)
	if w := serve(t, r, req); w.Code != http.StatusOK {
		t.Fatalf("put: %d %s", w.Code, w.Body.String())
	}

	path := "/v1/sellers/" + u.ID.String()
	req = httptest.NewRequest(http.MethodGet, path, nil)
	w := serve(t, r, req)
	if w.Code != http.StatusOK {
		t.Fatalf("public get: %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"idNumber", "idNumberLast4", "idType", "email", "phone", "whatsapp", "firebaseUid", "role"} {
		if _, ok := raw[k]; ok {
			t.Errorf("public response leaks %q: %v", k, raw)
		}
	}
	if raw["verified"] != false || raw["businessName"] != "Akosua Farms" || raw["memberSince"] == nil {
		t.Errorf("public = %v", raw)
	}

	// After approval the public view reports verified.
	verifyReq := jsonRequest(http.MethodPost, "/v1/admin/sellers/"+u.ID.String()+"/verification", adminTok,
		`{"decision":"approve"}`)
	if w := serve(t, r, verifyReq); w.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, path, nil)
	w = serve(t, r, req)
	var again map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &again); err != nil || again["verified"] != true {
		t.Errorf("public after approve = %v, %v", again, err)
	}

	// Unknown seller: 404. Malformed UUID: 400.
	req = httptest.NewRequest(http.MethodGet, "/v1/sellers/"+uuid.NewString(), nil)
	w = serve(t, r, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown seller: %d", w.Code)
	}
	assertErrorEnvelope(t, w.Body.Bytes(), apierror.CodeNotFound)
	assertContract(t, req, w)

	req = httptest.NewRequest(http.MethodGet, "/v1/sellers/not-a-uuid", nil)
	w = serve(t, r, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad uuid: %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
}

func TestAdminSellers_List(t *testing.T) {
	r, pool, fb := sellersRouter(t)
	for range 2 {
		u, tok := authUser(t, pool, fb)
		_ = u
		req := jsonRequest(http.MethodPut, "/v1/me/seller-profile", tok, validProfileBody)
		if w := serve(t, r, req); w.Code != http.StatusOK {
			t.Fatalf("put: %d %s", w.Code, w.Body.String())
		}
	}
	_, adminTok := authAdmin(t, pool, fb)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/sellers?status=pending&page=1&limit=20", nil)
	req.Header.Set("Authorization", "Bearer "+adminTok)
	w := serve(t, r, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var list api.SellerProfileAdminList
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 2 || list.Meta.Total != 2 || list.Meta.Page != 1 || list.Meta.Limit != 20 {
		t.Errorf("list = %+v", list)
	}
	if list.Items[0].Email == nil {
		t.Error("admin item lacks owner email")
	}

	// Non-admin: 403. Bad status: 400.
	_, userTok := authUser(t, pool, fb)
	req = httptest.NewRequest(http.MethodGet, "/v1/admin/sellers?status=pending", nil)
	req.Header.Set("Authorization", "Bearer "+userTok)
	if w := serve(t, r, req); w.Code != http.StatusForbidden {
		t.Errorf("non-admin list: %d", w.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/admin/sellers?status=bogus", nil)
	req.Header.Set("Authorization", "Bearer "+adminTok)
	w = serve(t, r, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad status: %d %s", w.Code, w.Body.String())
	}
	assertErrorEnvelope(t, w.Body.Bytes(), apierror.CodeValidationFailed)
}

func TestAdminVerify_Endpoint(t *testing.T) {
	r, pool, fb := sellersRouter(t)
	u, userTok := authUser(t, pool, fb)
	_, adminTok := authAdmin(t, pool, fb)

	put := jsonRequest(http.MethodPut, "/v1/me/seller-profile", userTok, validProfileBody)
	if w := serve(t, r, put); w.Code != http.StatusOK {
		t.Fatalf("put: %d %s", w.Code, w.Body.String())
	}
	verifyPath := "/v1/admin/sellers/" + u.ID.String() + "/verification"

	// Non-admin: 403.
	w := serve(t, r, jsonRequest(http.MethodPost, verifyPath, userTok, `{"decision":"approve"}`))
	if w.Code != http.StatusForbidden {
		t.Errorf("non-admin verify: %d %s", w.Code, w.Body.String())
	}
	assertErrorEnvelope(t, w.Body.Bytes(), apierror.CodeForbidden)

	// Reject without reason: 400 with details.
	req := jsonRequest(http.MethodPost, verifyPath, adminTok, `{"decision":"reject"}`)
	w = serve(t, r, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("reject without reason: %d %s", w.Code, w.Body.String())
	}
	assertErrorEnvelope(t, w.Body.Bytes(), apierror.CodeValidationFailed)
	assertContract(t, req, w)

	// Reject with reason: 200, rejected.
	req = jsonRequest(http.MethodPost, verifyPath, adminTok, `{"decision":"reject","reason":"Blurry photo"}`)
	w = serve(t, r, req)
	if w.Code != http.StatusOK {
		t.Fatalf("reject: %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var rejected api.SellerProfileAdmin
	if err := json.Unmarshal(w.Body.Bytes(), &rejected); err != nil ||
		rejected.VerificationStatus != api.SellerProfileAdminVerificationStatusRejected ||
		rejected.RejectionReason == nil || *rejected.RejectionReason != "Blurry photo" {
		t.Errorf("rejected = %+v, %v", rejected, err)
	}

	// Deciding twice: 409 invalid_transition.
	req = jsonRequest(http.MethodPost, verifyPath, adminTok, `{"decision":"approve"}`)
	w = serve(t, r, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("second decision: %d %s", w.Code, w.Body.String())
	}
	assertErrorEnvelope(t, w.Body.Bytes(), apierror.CodeInvalidTransition)
	assertContract(t, req, w)

	// Unknown seller: 404.
	req = jsonRequest(http.MethodPost, "/v1/admin/sellers/"+uuid.NewString()+"/verification", adminTok, `{"decision":"approve"}`)
	w = serve(t, r, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown seller: %d", w.Code)
	}
	assertErrorEnvelope(t, w.Body.Bytes(), apierror.CodeNotFound)
	assertContract(t, req, w)
}
