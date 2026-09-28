package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/auth/authtest"
	"github.com/dezmymachine/farmish-backend/internal/catalog"
	"github.com/dezmymachine/farmish-backend/internal/crypto"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/engagement"
	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/internal/http/middleware"
	"github.com/dezmymachine/farmish-backend/internal/jobs"
	"github.com/dezmymachine/farmish-backend/internal/listings"
	"github.com/dezmymachine/farmish-backend/internal/notify"
	"github.com/dezmymachine/farmish-backend/internal/orders"
	"github.com/dezmymachine/farmish-backend/internal/ratelimit"
	"github.com/dezmymachine/farmish-backend/internal/sellers"
	"github.com/dezmymachine/farmish-backend/internal/supply"
	"github.com/dezmymachine/farmish-backend/internal/users"
)

// reportSupplyFixture wires reports and supply endpoints with the Auth
// emulator.
type reportSupplyFixture struct {
	router   *gin.Engine
	pool     *pgxpool.Pool
	engage   *engagement.Service
	supply   *supply.Service
	listings *listings.Service
}

func newReportSupplyFixture(t *testing.T) *reportSupplyFixture {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Pool(t)
	if err := catalog.Seed(ctx, pool); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	crypter, err := crypto.New(key)
	if err != nil {
		t.Fatal(err)
	}
	sellersSvc := sellers.New(pool, crypter, nil)
	usersSvc := users.New(pool)
	ordersSvc := orders.NewService(pool, 48*time.Hour, 3*24*time.Hour)
	engage := engagement.New(pool, ordersSvc, usersSvc, nil)
	supplySvc := supply.New(pool, usersSvc)
	log := slog.New(slog.DiscardHandler)
	engage.AttachLogger(log)
	supplySvc.AttachLogger(log)
	reg := jobs.NewRegistry()
	notify.Register(reg, notify.NewWorker(notify.LogOnly{Log: log}, log, pool))
	client, err := jobs.NewClient(pool, reg, log, jobs.Options{Work: false})
	if err != nil {
		t.Fatal(err)
	}
	supplySvc.AttachJobClient(client)
	listingsSvc := listings.New(pool, catalog.New(pool), nil, sellersSvc)
	fb := authtest.Firebase(t)
	limits := middleware.DefaultRateLimits()
	limits.Operation["sensitive"] = ratelimit.Rule{Name: "sensitive", Limit: 100, Period: time.Minute, Burst: 50}
	router := newTestRouter(t, Deps{
		DB: fakePinger{}, Verifier: fb, Users: usersSvc,
		Sellers: sellersSvc, Listings: listingsSvc, PublicListings: listingsSvc,
		Orders: ordersSvc, OrderActions: ordersSvc, Engagement: engage, Supply: supplySvc,
		RateLimits: &limits, SharedLimiter: ratelimit.NewMemory(),
	})
	return &reportSupplyFixture{router: router, pool: pool, engage: engage, supply: supplySvc, listings: listingsSvc}
}

// activeListing profiles a fresh emulator user and inserts an active
// listing for them.
func (f *reportSupplyFixture) activeListing(t *testing.T, uid, title string) (string, uuid.UUID, uuid.UUID) {
	t.Helper()
	token := withProfile(t, f.pool, uid)
	sellerID := userIDFor(t, f.pool, token)
	var categoryID uuid.UUID
	if err := f.pool.QueryRow(context.Background(),
		`SELECT id FROM categories WHERE parent_id IS NULL LIMIT 1`).Scan(&categoryID); err != nil {
		t.Fatal(err)
	}
	var listingID uuid.UUID
	if err := f.pool.QueryRow(context.Background(),
		`INSERT INTO listings (seller_id, category_id, title, slug, description, price_pesewas,
		                       unit, quantity_available, item_state, status, region, district,
		                       published_at, expires_at)
		 VALUES ($1, $2, $3, $4, 'Good maize harvested this week here.', 5000,
		         'bags_50kg', 10, 'grade_a', 'active', 'Ashanti', 'Kumasi Metro',
		         now(), now() + interval '30 days')
		 RETURNING id`,
		sellerID, categoryID, title, "rep-"+uuid.NewString()[:8]).Scan(&listingID); err != nil {
		t.Fatal(err)
	}
	return token, sellerID, listingID
}

// plainToken makes an emulator user without a profile.
func (f *reportSupplyFixture) plainToken(t *testing.T) (string, uuid.UUID) {
	t.Helper()
	eu := authtest.EmailUser(t)
	req := meRequest(http.MethodGet, eu.Token, "")
	if w := serve(t, f.router, req); w.Code != http.StatusOK {
		t.Fatalf("user setup: %d", w.Code)
	}
	return eu.Token, userIDFor(t, f.pool, eu.Token)
}

// TestReportEndpoints_Flow walks create → duplicate 409 → admin list →
// suspend resolve → listing suspended and gone from search.
func TestReportEndpoints_Flow(t *testing.T) {
	f := newReportSupplyFixture(t)
	_, _, listingID := f.activeListing(t, "rep-flow-seller@example.com", "Flow maize")
	reporterTok, _ := f.plainToken(t)
	_, adminTok := authAdmin(t, f.pool, authtest.Firebase(t))

	req := jsonRequest(http.MethodPost, "/v1/reports", reporterTok,
		`{"listingId": "`+listingID.String()+`", "reason": "prohibited_item", "description": "Banned goods."}`)
	w := serve(t, f.router, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)

	req = jsonRequest(http.MethodPost, "/v1/reports", reporterTok,
		`{"listingId": "`+listingID.String()+`", "reason": "spam"}`)
	if w := serve(t, f.router, req); w.Code != http.StatusConflict {
		t.Errorf("duplicate = %d, want 409", w.Code)
	} else {
		assertErrorEnvelope(t, w.Body.Bytes(), apierror.CodeConflict)
		assertContract(t, req, w)
	}
	// Both targets at once is a 400.
	req = jsonRequest(http.MethodPost, "/v1/reports", reporterTok,
		`{"listingId": "`+listingID.String()+`", "userId": "`+uuid.NewString()+`", "reason": "spam"}`)
	if w := serve(t, f.router, req); w.Code != http.StatusBadRequest {
		t.Errorf("both targets = %d, want 400", w.Code)
	} else {
		assertContract(t, req, w)
	}

	req = jsonRequest(http.MethodGet, "/v1/admin/reports?status=open", adminTok, "")
	w = serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("admin list = %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var list api.ReportList
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].Listing == nil {
		t.Fatalf("list = %+v", list)
	}
	reportID := list.Items[0].Id

	req = jsonRequest(http.MethodPost, "/v1/admin/reports/"+reportID.String()+"/resolve", adminTok,
		`{"status": "actioned", "action": "suspend_listing", "note": "Confirmed."}`)
	w = serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("resolve = %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)

	var status string
	if err := f.pool.QueryRow(context.Background(), `SELECT status FROM listings WHERE id = $1`, listingID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "suspended" {
		t.Errorf("listing = %s, want suspended", status)
	}
	// Suspended listings vanish from public search.
	searchReq := jsonRequest(http.MethodGet, "/v1/listings?q=maize", "", "")
	if w := serve(t, f.router, searchReq); w.Code == http.StatusOK {
		var result api.ListingSearchResult
		if err := json.Unmarshal(w.Body.Bytes(), &result); err == nil {
			for _, item := range result.Items {
				if item.Id == listingID {
					t.Error("suspended listing still searchable")
				}
			}
		}
	}
	// Second resolve is a 409.
	req = jsonRequest(http.MethodPost, "/v1/admin/reports/"+reportID.String()+"/resolve", adminTok,
		`{"status": "dismissed", "action": "none", "note": "Again."}`)
	if w := serve(t, f.router, req); w.Code != http.StatusConflict {
		t.Errorf("second resolve = %d, want 409", w.Code)
	} else {
		assertContract(t, req, w)
	}
	// Non-admins cannot touch the queue.
	req = jsonRequest(http.MethodGet, "/v1/admin/reports?status=open", reporterTok, "")
	if w := serve(t, f.router, req); w.Code != http.StatusForbidden {
		t.Errorf("reporter list = %d, want 403", w.Code)
	}
}

// TestSupplyEndpoints_Flow walks create → get → owner cancel, then the
// admin machine to delivered with an SMS per move.
func TestSupplyEndpoints_Flow(t *testing.T) {
	f := newReportSupplyFixture(t)
	userTok, userID := f.plainToken(t)
	_, adminTok := authAdmin(t, f.pool, authtest.Firebase(t))
	_ = userID

	body := `{"items": [
		{"categorySlug": "fresh-produce", "productName": "Tomatoes", "quantity": 10, "unit": "kg"},
		{"categorySlug": "seeds-seedlings", "productName": "Maize seed", "quantity": 2, "unit": "bags_50kg"}],
		"deliveryAddress": "Plot 12, Kumasi", "expectedDate": "2099-01-01", "notes": "Call me."}`
	req := jsonRequest(http.MethodPost, "/v1/supply-requests", userTok, body)
	w := serve(t, f.router, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var created api.SupplyRequest
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if len(created.Items) != 2 || len(created.Events) != 1 || created.Status != api.SupplyStatusPending {
		t.Errorf("created = %+v", created)
	}
	if !strings.HasPrefix(created.RequestNumber, "SUP-") {
		t.Errorf("number = %q", created.RequestNumber)
	}

	req = jsonRequest(http.MethodGet, "/v1/supply-requests/"+created.Id.String(), userTok, "")
	w = serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("get = %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)

	// Owner cancels from pending.
	req = jsonRequest(http.MethodPost, "/v1/supply-requests/"+created.Id.String()+"/cancel", userTok, `{"reason": "Found some."}`)
	w = serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("cancel = %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)

	// A fresh request rides the admin machine to delivered.
	req = jsonRequest(http.MethodPost, "/v1/supply-requests", userTok, body)
	w = serve(t, f.router, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("second create = %d", w.Code)
	}
	var second api.SupplyRequest
	if err := json.Unmarshal(w.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	for _, to := range []api.SupplyStatus{api.SupplyStatusConfirmed, api.SupplyStatusProcessing, api.SupplyStatusDelivered} {
		req := jsonRequest(http.MethodPost, "/v1/admin/supply-requests/"+second.Id.String()+"/status", adminTok,
			`{"status": "`+string(to)+`", "note": "Moving."}`)
		w := serve(t, f.router, req)
		if w.Code != http.StatusOK {
			t.Fatalf("admin to %s = %d %s", to, w.Code, w.Body.String())
		}
		assertContract(t, req, w)
	}
	var sms int64
	if err := f.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM river_job WHERE kind = 'notify.sms'`).Scan(&sms); err != nil {
		t.Fatal(err)
	}
	// create×2 + owner cancel + 3 admin moves = 6 notifications.
	if sms != 6 {
		t.Errorf("sms jobs = %d, want 6", sms)
	}

	// Strangers read others as missing; owners cannot skip or revive.
	strangerTok, _ := f.plainToken(t)
	req = jsonRequest(http.MethodGet, "/v1/supply-requests/"+second.Id.String(), strangerTok, "")
	if w := serve(t, f.router, req); w.Code != http.StatusNotFound {
		t.Errorf("stranger get = %d, want 404", w.Code)
	} else {
		assertContract(t, req, w)
	}
	req = jsonRequest(http.MethodPost, "/v1/supply-requests/"+second.Id.String()+"/cancel", userTok, `{}`)
	if w := serve(t, f.router, req); w.Code != http.StatusConflict {
		t.Errorf("cancel delivered = %d, want 409", w.Code)
	} else {
		assertContract(t, req, w)
	}
	req = jsonRequest(http.MethodPost, "/v1/admin/supply-requests/"+created.Id.String()+"/status", adminTok,
		`{"status": "confirmed"}`)
	if w := serve(t, f.router, req); w.Code != http.StatusConflict {
		t.Errorf("confirm cancelled = %d, want 409", w.Code)
	}
}

// TestSupplyEndpoints_CreateValidationTable drives the 400 matrix through
// the contract.
func TestSupplyEndpoints_CreateValidationTable(t *testing.T) {
	f := newReportSupplyFixture(t)
	userTok, _ := f.plainToken(t)
	good := `{"items": [{"categorySlug": "fresh-produce", "productName": "Tomatoes", "quantity": 1, "unit": "kg"}]}`
	cases := map[string]string{
		"no items":     `{"items": []}`,
		"child slug":   `{"items": [{"categorySlug": "fresh-produce-vegetables", "productName": "Tomatoes", "quantity": 1, "unit": "kg"}]}`,
		"unknown slug": `{"items": [{"categorySlug": "nope", "productName": "Tomatoes", "quantity": 1, "unit": "kg"}]}`,
		"bad unit":     `{"items": [{"categorySlug": "fresh-produce", "productName": "Tomatoes", "quantity": 1, "unit": "truckloads"}]}`,
		"past date":    `{"items": [{"categorySlug": "fresh-produce", "productName": "Tomatoes", "quantity": 1, "unit": "kg"}], "expectedDate": "2000-01-01"}`,
		"bad phone":    `{"items": [{"categorySlug": "fresh-produce", "productName": "Tomatoes", "quantity": 1, "unit": "kg"}], "deliveryPhone": "123"}`,
		"not json":     `{`,
	}
	for name, body := range cases {
		req := jsonRequest(http.MethodPost, "/v1/supply-requests", userTok, body)
		if w := serve(t, f.router, req); w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", name, w.Code)
		} else if name != "not json" {
			assertContract(t, req, w)
		}
	}
	if w := serve(t, f.router, jsonRequest(http.MethodPost, "/v1/supply-requests", userTok, good)); w.Code != http.StatusCreated {
		t.Errorf("good = %d, want 201", w.Code)
	}
	req := jsonRequest(http.MethodGet, "/v1/supply-requests?status=bogus", userTok, "")
	if w := serve(t, f.router, req); w.Code != http.StatusBadRequest {
		t.Errorf("bad filter = %d, want 400", w.Code)
	}
}

// TestUniqueness_Constraints pins the plan's Done-when across 20a and 20b:
// review, favorite and report uniqueness each refuse the second write.
func TestUniqueness_Constraints(t *testing.T) {
	f := newReportSupplyFixture(t)
	_, _, listingID := f.activeListing(t, "rep-unique-seller@example.com", "Unique maize")
	reporterTok, _ := f.plainToken(t)

	for i := 0; i < 2; i++ {
		req := jsonRequest(http.MethodPost, "/v1/reports", reporterTok,
			`{"listingId": "`+listingID.String()+`", "reason": "spam"}`)
		w := serve(t, f.router, req)
		want := http.StatusCreated
		if i == 1 {
			want = http.StatusConflict
		}
		if w.Code != want {
			t.Errorf("report %d = %d, want %d", i, w.Code, want)
		}
	}
}
