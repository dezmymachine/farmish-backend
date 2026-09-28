package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
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
	"github.com/dezmymachine/farmish-backend/internal/http/middleware"
	"github.com/dezmymachine/farmish-backend/internal/jobs"
	"github.com/dezmymachine/farmish-backend/internal/listings"
	"github.com/dezmymachine/farmish-backend/internal/notify"
	"github.com/dezmymachine/farmish-backend/internal/orders"
	"github.com/dezmymachine/farmish-backend/internal/ratelimit"
	"github.com/dezmymachine/farmish-backend/internal/sellers"
	"github.com/dezmymachine/farmish-backend/internal/users"
)

// engagementFixture wires the engagement endpoints with the Auth emulator.
type engagementFixture struct {
	router *gin.Engine
	pool   *pgxpool.Pool
	engage *engagement.Service
	orders *orders.Service
}

func newEngagementFixture(t *testing.T) *engagementFixture {
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
	listingsSvc := listings.New(pool, catalog.New(pool), nil, sellersSvc)
	ordersSvc := orders.NewService(pool, 48*time.Hour, 3*24*time.Hour)
	engage := engagement.New(pool, ordersSvc, usersSvc, nil)
	log := slog.New(slog.DiscardHandler)
	engage.AttachLogger(log)
	reg := jobs.NewRegistry()
	notify.Register(reg, notify.NewWorker(notify.LogOnly{Log: log}, log, pool))
	client, err := jobs.NewClient(pool, reg, log, jobs.Options{Work: false})
	if err != nil {
		t.Fatal(err)
	}
	_ = client
	_ = client
	fb := authtest.Firebase(t)
	limits := middleware.DefaultRateLimits()
	limits.Operation["sensitive"] = ratelimit.Rule{Name: "sensitive", Limit: 100, Period: time.Minute, Burst: 50}
	router := newTestRouter(t, Deps{
		DB: fakePinger{}, Verifier: fb, Users: usersSvc,
		Sellers: sellersSvc, Listings: listingsSvc, PublicListings: listingsSvc,
		Orders: ordersSvc, OrderActions: ordersSvc, Engagement: engage,
		RateLimits: &limits, SharedLimiter: ratelimit.NewMemory(),
	})
	return &engagementFixture{router: router, pool: pool, engage: engage, orders: ordersSvc}
}

// sellerWithActiveListing profiles a fresh emulator user and inserts an
// active listing for them.
func (f *engagementFixture) sellerWithActiveListing(t *testing.T, uid, title string) (string, uuid.UUID, uuid.UUID) {
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
		sellerID, categoryID, title, "engage-"+uuid.NewString()[:8]).Scan(&listingID); err != nil {
		t.Fatal(err)
	}
	return token, sellerID, listingID
}

// buyerWithCompletedOrder makes a plain emulator user plus a completed
// order with one item of the listing.
func (f *engagementFixture) buyerWithCompletedOrder(t *testing.T, sellerID, listingID uuid.UUID, subtotal int64) (string, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	eu := authtest.EmailUser(t)
	req := meRequest(http.MethodGet, eu.Token, "")
	if w := serve(t, f.router, req); w.Code != http.StatusOK {
		t.Fatalf("buyer setup: %d %s", w.Code, w.Body.String())
	}
	var buyerID uuid.UUID
	if err := f.pool.QueryRow(ctx, `SELECT id FROM users WHERE firebase_uid = $1`, eu.UID).Scan(&buyerID); err != nil {
		t.Fatal(err)
	}
	var checkoutID, orderID uuid.UUID
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO checkouts (buyer_id, idempotency_key, request_hash, base_pesewas,
		                        processing_fee_pesewas, charge_pesewas, status, expires_at)
		 VALUES ($1, $2, 'engage-test', $3, 0, $3, 'paid', now() + interval '30 minutes')
		 RETURNING id`, buyerID, uuid.New(), subtotal).Scan(&checkoutID); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO orders (checkout_id, buyer_id, seller_id, status, escrow_state,
		                     subtotal_pesewas, delivery_fee_pesewas, base_pesewas,
		                     commission_rate_bps, commission_pesewas, delivery_method)
		 VALUES ($1, $2, $3, 'completed', 'released', $4, 0, $4, 500, $5, 'pickup')
		 RETURNING id`,
		checkoutID, buyerID, sellerID, subtotal, (subtotal*500+5000)/10000).Scan(&orderID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO order_items (order_id, listing_id, title, unit, unit_price_pesewas, quantity, line_total_pesewas)
		 VALUES ($1, $2, 'Engage maize', 'bags_50kg', $3, 1, $3)`,
		orderID, listingID, subtotal); err != nil {
		t.Fatal(err)
	}
	return eu.Token, orderID
}

// TestEngagementEndpoints_ReviewFlow walks create → duplicate 409 → list
// with summary → hide → excluded, all contract-valid.
func TestEngagementEndpoints_ReviewFlow(t *testing.T) {
	f := newEngagementFixture(t)
	sellerTok, _, listingID := f.sellerWithActiveListing(t, "engage-flow-seller@example.com", "Flow maize")
	_ = sellerTok
	buyerTok, orderID := f.buyerWithCompletedOrder(t, f.sellerOf(t, listingID), listingID, 5000)

	req := jsonRequest(http.MethodPost, "/v1/orders/"+orderID.String()+"/reviews", buyerTok,
		`{"listingId": "`+listingID.String()+`", "rating": 5, "comment": "Great maize."}`)
	w := serve(t, f.router, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)

	req = jsonRequest(http.MethodPost, "/v1/orders/"+orderID.String()+"/reviews", buyerTok,
		`{"listingId": "`+listingID.String()+`", "rating": 4}`)
	if w := serve(t, f.router, req); w.Code != http.StatusConflict {
		t.Errorf("duplicate = %d, want 409", w.Code)
	} else {
		assertErrorEnvelope(t, w.Body.Bytes(), "already_reviewed")
		assertContract(t, req, w)
	}

	var slug string
	if err := f.pool.QueryRow(context.Background(), `SELECT slug FROM listings WHERE id = $1`, listingID).Scan(&slug); err != nil {
		t.Fatal(err)
	}
	req = jsonRequest(http.MethodGet, "/v1/listings/"+slug+"/reviews", "", "")
	w = serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var list api.PublicReviewList
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Summary.Count != 1 || list.Summary.Average != 5.0 {
		t.Errorf("list = %+v", list)
	}
	if list.Items[0].ReviewerName == "" {
		t.Error("reviewer name missing")
	}
	for _, key := range []string{"reviewerId", "reviewer_id", "email", "phone", "firebaseUid"} {
		if strings.Contains(w.Body.String(), `"`+key+`"`) {
			t.Errorf("public review contains %q", key)
		}
	}

	_, adminTok := authAdmin(t, f.pool, authtest.Firebase(t))
	var reviewID uuid.UUID
	if err := f.pool.QueryRow(context.Background(), `SELECT id FROM reviews`).Scan(&reviewID); err != nil {
		t.Fatal(err)
	}
	req = jsonRequest(http.MethodPost, "/v1/admin/reviews/"+reviewID.String()+"/hide", adminTok, `{"reason": "Spam."}`)
	w = serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("hide = %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	req = jsonRequest(http.MethodGet, "/v1/listings/"+slug+"/reviews", "", "")
	w = serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list after hide = %d", w.Code)
	}
	var after api.PublicReviewList
	if err := json.Unmarshal(w.Body.Bytes(), &after); err != nil {
		t.Fatal(err)
	}
	if len(after.Items) != 0 || after.Summary.Count != 0 {
		t.Errorf("after hide = %+v, want empty", after)
	}
	if w := serve(t, f.router, jsonRequest(http.MethodPost, "/v1/admin/reviews/"+reviewID.String()+"/hide", adminTok, `{"reason": "Again."}`)); w.Code != http.StatusConflict {
		t.Errorf("second hide = %d, want 409", w.Code)
	}
}

// sellerOf looks up a listing's seller.
func (f *engagementFixture) sellerOf(t *testing.T, listingID uuid.UUID) uuid.UUID {
	t.Helper()
	var sellerID uuid.UUID
	if err := f.pool.QueryRow(context.Background(), `SELECT seller_id FROM listings WHERE id = $1`, listingID).Scan(&sellerID); err != nil {
		t.Fatal(err)
	}
	return sellerID
}

// TestEngagementEndpoints_ReviewFailures covers unpaid orders, strangers,
// foreign listings and bad bodies.
func TestEngagementEndpoints_ReviewFailures(t *testing.T) {
	f := newEngagementFixture(t)
	_, sellerID, listingID := f.sellerWithActiveListing(t, "engage-fail-seller@example.com", "Fail maize")
	buyerTok, orderID := f.buyerWithCompletedOrder(t, sellerID, listingID, 5000)
	strangerTok := f.buyerToken(t)

	if _, err := f.pool.Exec(context.Background(), `UPDATE orders SET status = 'paid' WHERE id = $1`, orderID); err != nil {
		t.Fatal(err)
	}
	req := jsonRequest(http.MethodPost, "/v1/orders/"+orderID.String()+"/reviews", buyerTok,
		`{"listingId": "`+listingID.String()+`", "rating": 5}`)
	if w := serve(t, f.router, req); w.Code != http.StatusConflict {
		t.Errorf("unpaid = %d, want 409", w.Code)
	} else {
		assertErrorEnvelope(t, w.Body.Bytes(), "order_not_completed")
		assertContract(t, req, w)
	}
	if _, err := f.pool.Exec(context.Background(), `UPDATE orders SET status = 'completed' WHERE id = $1`, orderID); err != nil {
		t.Fatal(err)
	}

	req = jsonRequest(http.MethodPost, "/v1/orders/"+orderID.String()+"/reviews", strangerTok,
		`{"listingId": "`+listingID.String()+`", "rating": 5}`)
	if w := serve(t, f.router, req); w.Code != http.StatusForbidden {
		t.Errorf("stranger = %d, want 403", w.Code)
	} else {
		assertContract(t, req, w)
	}
	req = jsonRequest(http.MethodPost, "/v1/orders/"+orderID.String()+"/reviews", buyerTok,
		`{"listingId": "`+uuid.NewString()+`", "rating": 5}`)
	if w := serve(t, f.router, req); w.Code != http.StatusBadRequest {
		t.Errorf("foreign listing = %d, want 400", w.Code)
	} else {
		assertContract(t, req, w)
	}
	for _, bad := range []string{
		`{"listingId": "` + listingID.String() + `", "rating": 0}`,
		`{"listingId": "` + listingID.String() + `", "rating": 6}`,
		`{"listingId": "nope", "rating": 5}`,
		`{}`,
	} {
		req := jsonRequest(http.MethodPost, "/v1/orders/"+orderID.String()+"/reviews", buyerTok, bad)
		if w := serve(t, f.router, req); w.Code != http.StatusBadRequest {
			t.Errorf("body %q = %d, want 400", bad, w.Code)
		}
	}
	req = jsonRequest(http.MethodPost, "/v1/orders/"+uuid.NewString()+"/reviews", buyerTok,
		`{"listingId": "`+listingID.String()+`", "rating": 5}`)
	if w := serve(t, f.router, req); w.Code != http.StatusNotFound {
		t.Errorf("unknown order = %d, want 404", w.Code)
	} else {
		assertContract(t, req, w)
	}
}

// buyerToken makes a plain emulator user.
func (f *engagementFixture) buyerToken(t *testing.T) string {
	t.Helper()
	eu := authtest.EmailUser(t)
	req := meRequest(http.MethodGet, eu.Token, "")
	if w := serve(t, f.router, req); w.Code != http.StatusOK {
		t.Fatalf("buyer setup: %d %s", w.Code, w.Body.String())
	}
	return eu.Token
}

// TestEngagementEndpoints_Favorites proves idempotent PUT/DELETE with an
// exact counter, inactive flagging and rating display.
func TestEngagementEndpoints_Favorites(t *testing.T) {
	f := newEngagementFixture(t)
	_, _, listingID := f.sellerWithActiveListing(t, "engage-fav-seller@example.com", "Fav maize")
	buyerTok := f.buyerToken(t)

	put := func() int {
		req := jsonRequest(http.MethodPut, "/v1/me/favorites/"+listingID.String(), buyerTok, "")
		w := serve(t, f.router, req)
		assertContract(t, req, w)
		return w.Code
	}
	if code := put(); code != http.StatusNoContent {
		t.Fatalf("put = %d", code)
	}
	if code := put(); code != http.StatusNoContent {
		t.Fatalf("second put = %d, want idempotent 204", code)
	}
	var counter int64
	if err := f.pool.QueryRow(context.Background(), `SELECT favorite_count FROM listings WHERE id = $1`, listingID).Scan(&counter); err != nil {
		t.Fatal(err)
	}
	if counter != 1 {
		t.Errorf("counter = %d, want 1", counter)
	}

	req := jsonRequest(http.MethodGet, "/v1/me/favorites", buyerTok, "")
	w := serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("favorites = %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var list api.FavoriteList
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || !list.Items[0].Available {
		t.Errorf("favorites = %+v, want one available", list)
	}

	// Sold listings stay listed, flagged unavailable.
	if _, err := f.pool.Exec(context.Background(), `UPDATE listings SET status = 'sold' WHERE id = $1`, listingID); err != nil {
		t.Fatal(err)
	}
	req = jsonRequest(http.MethodGet, "/v1/me/favorites", buyerTok, "")
	w = serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("favorites sold = %d", w.Code)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].Available {
		t.Errorf("sold favorite = %+v, want available=false", list)
	}

	remove := func() int {
		req := jsonRequest(http.MethodDelete, "/v1/me/favorites/"+listingID.String(), buyerTok, "")
		w := serve(t, f.router, req)
		assertContract(t, req, w)
		return w.Code
	}
	if code := remove(); code != http.StatusNoContent {
		t.Fatalf("delete = %d", code)
	}
	if code := remove(); code != http.StatusNoContent {
		t.Fatalf("second delete = %d, want idempotent 204", code)
	}
	if err := f.pool.QueryRow(context.Background(), `SELECT favorite_count FROM listings WHERE id = $1`, listingID).Scan(&counter); err != nil {
		t.Fatal(err)
	}
	if counter != 0 {
		t.Errorf("counter = %d, want 0", counter)
	}

	req = jsonRequest(http.MethodPut, "/v1/me/favorites/"+uuid.NewString(), buyerTok, "")
	if w := serve(t, f.router, req); w.Code != http.StatusNotFound {
		t.Errorf("favorite missing = %d, want 404", w.Code)
	} else {
		assertContract(t, req, w)
	}
	if w := serve(t, f.router, jsonRequest(http.MethodPut, "/v1/me/favorites/"+listingID.String(), "", "")); w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous put = %d, want 401", w.Code)
	}
}

// TestEngagementEndpoints_RatingDisplayed proves reviews surface as the
// seller aggregate on the seller profile and the listing detail.
func TestEngagementEndpoints_RatingDisplayed(t *testing.T) {
	f := newEngagementFixture(t)
	sellerTok, sellerID, listingID := f.sellerWithActiveListing(t, "engage-rate-seller@example.com", "Rate maize")
	_ = sellerTok
	buyerTok, orderID := f.buyerWithCompletedOrder(t, sellerID, listingID, 5000)
	buyer2Tok, order2ID := f.buyerWithCompletedOrder(t, sellerID, listingID, 3000)
	_ = buyer2Tok
	_ = order2ID

	for _, tc := range []struct {
		token   string
		orderID uuid.UUID
		rating  int
	}{
		{buyerTok, orderID, 5},
		{buyer2Tok, order2ID, 4},
	} {
		req := jsonRequest(http.MethodPost, "/v1/orders/"+tc.orderID.String()+"/reviews", tc.token,
			`{"listingId": "`+listingID.String()+`", "rating": `+strconv.Itoa(tc.rating)+`}`)
		if w := serve(t, f.router, req); w.Code != http.StatusCreated {
			t.Fatalf("review = %d %s", w.Code, w.Body.String())
		}
	}

	req := jsonRequest(http.MethodGet, "/v1/sellers/"+sellerID.String(), "", "")
	w := serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("seller = %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var seller api.PublicSeller
	if err := json.Unmarshal(w.Body.Bytes(), &seller); err != nil {
		t.Fatal(err)
	}
	if seller.Rating.Average != 4.5 || seller.Rating.Count != 2 {
		t.Errorf("seller rating = %+v, want 4.5 x 2", seller.Rating)
	}

	var slug string
	if err := f.pool.QueryRow(context.Background(), `SELECT slug FROM listings WHERE id = $1`, listingID).Scan(&slug); err != nil {
		t.Fatal(err)
	}
	req = jsonRequest(http.MethodGet, "/v1/listings/"+slug, "", "")
	w = serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("listing = %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var detail api.ListingDetail
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Rating.Average != 4.5 || detail.Rating.Count != 2 {
		t.Errorf("listing rating = %+v, want 4.5 x 2", detail.Rating)
	}
	if detail.Seller.Rating.Average != 4.5 {
		t.Errorf("embedded seller rating = %+v", detail.Seller.Rating)
	}
}
