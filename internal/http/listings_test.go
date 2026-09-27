package httpapi

import (
	"context"
	"encoding/json"
	"io"
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
	"github.com/dezmymachine/farmish-backend/internal/checkout"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/delivery"
	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/internal/http/middleware"
	"github.com/dezmymachine/farmish-backend/internal/ledger"
	"github.com/dezmymachine/farmish-backend/internal/listings"
	"github.com/dezmymachine/farmish-backend/internal/media"
	"github.com/dezmymachine/farmish-backend/internal/media/mediatest"
	"github.com/dezmymachine/farmish-backend/internal/orders"
	"github.com/dezmymachine/farmish-backend/internal/payments"
	"github.com/dezmymachine/farmish-backend/internal/payments/fake"
	"github.com/dezmymachine/farmish-backend/internal/ratelimit"
	"github.com/dezmymachine/farmish-backend/internal/sellers"
	"github.com/dezmymachine/farmish-backend/internal/users"
)

// listingRouter wires the real router with catalog, media and listings. The
// storage is returned so tests upload through the same bucket the service
// reads.
func listingRouter(t *testing.T) (*gin.Engine, *pgxpool.Pool, *media.R2) {
	return listingRouterWithLimits(t, nil)
}

func listingRouterWithLimits(t *testing.T, limits *middleware.RateLimits) (*gin.Engine, *pgxpool.Pool, *media.R2) {
	t.Helper()
	fb := authtest.Firebase(t)
	pool := dbtest.Pool(t)
	if err := catalog.Seed(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	store := mediatest.R2(t)
	sellersSvc := sellers.New(pool, nil, nil)
	listingsSvc := listings.New(pool, catalog.New(pool), media.New(pool, store), sellersSvc)
	ordersSvc := orders.NewService(pool, 48*time.Hour, 3*24*time.Hour)
	deps := Deps{
		DB: fakePinger{}, Verifier: fb, Users: users.New(pool),
		Sellers: sellersSvc, Media: media.New(pool, store),
		Listings: listingsSvc, PublicListings: listingsSvc,
		Checkout: checkout.New(pool,
			payments.New(pool, fake.New(), slog.New(slog.DiscardHandler), 195, "https://farmish.gh/payments/status"),
			fake.New(), delivery.Manual{}, ledger.New(), ordersSvc,
			slog.New(slog.DiscardHandler), 195, 30*time.Minute),
		Orders: ordersSvc,
	}
	if limits != nil {
		deps.RateLimits = limits
		deps.SharedLimiter = ratelimit.NewMemory()
	}
	return newTestRouter(t, deps), pool, store
}

// withProfile creates a seller profile for a fresh emulator user and returns
// its token.
func withProfile(t *testing.T, pool *pgxpool.Pool, uid string) string {
	t.Helper()
	ctx := context.Background()
	fb := authtest.Firebase(t)
	eu := authtest.EmailUser(t)
	id, err := fb.Verify(ctx, eu.Token)
	if err != nil {
		t.Fatal(err)
	}
	u, err := users.New(pool).Resolve(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sellers.New(pool, nil, nil).UpsertMine(ctx, u.ID, sellers.ProfileInput{
		BusinessName: uid + " farms", Region: "Ashanti", District: "Kumasi Metro",
	}); err != nil {
		t.Fatal(err)
	}
	return eu.Token
}

// userIDFor resolves a token to its users row id.
func userIDFor(t *testing.T, pool *pgxpool.Pool, token string) uuid.UUID {
	t.Helper()
	fb := authtest.Firebase(t)
	id, err := fb.Verify(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	u, err := users.New(pool).Resolve(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

const listingBody = `{
  "categorySlug": "livestock-poultry-cattle",
  "title": "Healthy Friesian Heifer",
  "description": "Well-fed heifer, vaccinated and ready for sale on the farm.",
  "price": {"amount": 850000, "currency": "GHS"},
  "unit": "heads",
  "quantityAvailable": 12,
  "minOrderQty": 1,
  "itemState": "adult",
  "region": "Ashanti",
  "district": "Kumasi Metro",
  "deliveryOptions": {"pickup": true, "sellerDelivery": true, "sellerDeliveryFee": {"amount": 500, "currency": "GHS"}}
}`

// createListing posts a draft and returns its id.
func createListing(t *testing.T, r *gin.Engine, token string) uuid.UUID {
	t.Helper()
	req := jsonRequest(http.MethodPost, "/v1/listings", token, listingBody)
	w := serve(t, r, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created api.SellerListing
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	return created.Id
}

// uploadImage asks media for an upload URL and PUTs the bytes into the
// router's bucket.
func uploadImage(t *testing.T, pool *pgxpool.Pool, store *media.R2, token string) string {
	t.Helper()
	ctx := context.Background()
	owner := userIDFor(t, pool, token)
	svc := media.New(pool, store)
	up, err := svc.CreateUpload(ctx, owner, media.PurposeListingImage, "image/jpeg", 64)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("a", 64)
	req, err := http.NewRequest(http.MethodPut, up.URL, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(body))
	for k, v := range up.Headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload: %d", resp.StatusCode)
	}
	return up.ID.String()
}

func TestListings_ContractValid(t *testing.T) {
	r, pool, store := listingRouter(t)
	tok := withProfile(t, pool, "contract-seller")

	// Create (201) with an image, then read, list and patch it: every
	// response must satisfy the spec.
	mediaID := uploadImage(t, pool, store, tok)
	body := strings.TrimSuffix(listingBody, "}") + `, "imageMediaIds": ["` + mediaID + `"]}`
	req := jsonRequest(http.MethodPost, "/v1/listings", tok, body)
	w := serve(t, r, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var created api.SellerListing
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Status != api.SellerListingStatusDraft || created.Slug != "healthy-friesian-heifer" {
		t.Fatalf("created = %+v", created)
	}
	if len(created.Images) != 1 || created.Images[0].Url == "" {
		t.Errorf("images = %+v", created.Images)
	}

	path := "/v1/me/listings/" + created.Id.String()
	req = jsonRequest(http.MethodGet, path, tok, "")
	w = serve(t, r, req)
	if w.Code != http.StatusOK {
		t.Fatalf("get: %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)

	req = jsonRequest(http.MethodGet, "/v1/me/listings?page=1&limit=20", tok, "")
	w = serve(t, r, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var list api.SellerListingSummaryList
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Items) != 1 {
		t.Fatalf("list = %+v, %v", list, err)
	}

	req = jsonRequest(http.MethodPatch, "/v1/me/listings/"+created.Id.String(), tok,
		`{"price": {"amount": 900000, "currency": "GHS"}}`)
	w = serve(t, r, req)
	if w.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var patched api.SellerListing
	if err := json.Unmarshal(w.Body.Bytes(), &patched); err != nil || patched.Price.Amount != 900000 {
		t.Errorf("patched price = %+v, %v", patched.Price, err)
	}
}

func TestListings_Lifecycle(t *testing.T) {
	r, pool, store := listingRouter(t)
	tok := withProfile(t, pool, "lifecycle-seller")
	mediaID := uploadImage(t, pool, store, tok)
	body := strings.TrimSuffix(listingBody, "}") + `, "imageMediaIds": ["` + mediaID + `"]}`

	// Create and publish in one request.
	req := jsonRequest(http.MethodPost, "/v1/listings", tok, strings.TrimSuffix(body, "}")+`, "publish": true}`)
	w := serve(t, r, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create+publish: %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var listing api.SellerListing
	if err := json.Unmarshal(w.Body.Bytes(), &listing); err != nil || listing.Status != api.SellerListingStatusActive {
		t.Fatalf("listing = %+v, %v", listing, err)
	}
	if listing.ExpiresAt == nil || listing.PublishedAt == nil {
		t.Errorf("published listing without timestamps: %+v", listing)
	}
	id := listing.Id

	// Archive, then publish again, then mark sold, then renew (409: sold).
	for _, step := range []struct {
		path string
		want int
		code string
	}{
		{"/archive", http.StatusOK, ""},
		{"/publish", http.StatusOK, ""},
		{"/mark-sold", http.StatusOK, ""},
		{"/renew", http.StatusConflict, apierror.CodeInvalidTransition},
		{"/archive", http.StatusConflict, apierror.CodeInvalidTransition},
		{"/publish", http.StatusConflict, apierror.CodeInvalidTransition},
	} {
		req := jsonRequest(http.MethodPost, "/v1/me/listings/"+id.String()+step.path, tok, "")
		w := serve(t, r, req)
		if w.Code != step.want {
			t.Fatalf("%s: %d %s", step.path, w.Code, w.Body.String())
		}
		assertContract(t, req, w)
		if step.code != "" {
			assertErrorEnvelope(t, w.Body.Bytes(), step.code)
		}
	}

	// A draft can be deleted; a sold listing cannot.
	draft := createListing(t, r, tok)
	req = plainRequest(http.MethodDelete, "/v1/me/listings/"+draft.String(), tok)
	w = serve(t, r, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete draft: %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	req = plainRequest(http.MethodDelete, "/v1/me/listings/"+id.String(), tok)
	w = serve(t, r, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("delete sold: %d %s", w.Code, w.Body.String())
	}
	assertErrorEnvelope(t, w.Body.Bytes(), apierror.CodeInvalidTransition)
	assertContract(t, req, w)
}

func TestListings_OwnershipAndProfileGuards(t *testing.T) {
	r, pool, _ := listingRouter(t)
	mine := withProfile(t, pool, "owner-seller")
	theirs := withProfile(t, pool, "other-seller")
	id := createListing(t, r, mine)

	// Another seller is forbidden on every mutation and on the read.
	for _, req := range []*http.Request{
		jsonRequest(http.MethodGet, "/v1/me/listings/"+id.String(), theirs, ""),
		jsonRequest(http.MethodPatch, "/v1/me/listings/"+id.String(), theirs, `{"title":"Hijacked Heifer"}`),
		jsonRequest(http.MethodPost, "/v1/me/listings/"+id.String()+"/publish", theirs, ""),
		jsonRequest(http.MethodPost, "/v1/me/listings/"+id.String()+"/archive", theirs, ""),
		plainRequest(http.MethodDelete, "/v1/me/listings/"+id.String(), theirs),
	} {
		w := serve(t, r, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s by another seller: %d %s", req.Method, req.URL.Path, w.Code, w.Body.String())
		}
		assertContract(t, req, w)
	}
	// Unknown id: 404.
	req := jsonRequest(http.MethodGet, "/v1/me/listings/"+uuid.NewString(), mine, "")
	w := serve(t, r, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown listing: %d", w.Code)
	}
	assertErrorEnvelope(t, w.Body.Bytes(), apierror.CodeNotFound)
	assertContract(t, req, w)

	// A user without a seller profile cannot list: 403
	// seller_profile_required.
	_, plainTok := authUser(t, pool, authtest.Firebase(t))
	req = jsonRequest(http.MethodPost, "/v1/listings", plainTok, listingBody)
	w = serve(t, r, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("no profile: %d %s", w.Code, w.Body.String())
	}
	assertErrorEnvelope(t, w.Body.Bytes(), apierror.CodeSellerProfileRequired)
	assertContract(t, req, w)

	// Unauthenticated: 401.
	req = jsonRequest(http.MethodPost, "/v1/listings", "", listingBody)
	if w := serve(t, r, req); w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous create: %d", w.Code)
	}
}

func TestListings_SuspendedIsFrozen(t *testing.T) {
	r, pool, store := listingRouter(t)
	tok := withProfile(t, pool, "suspended-seller")
	mediaID := uploadImage(t, pool, store, tok)
	body := strings.TrimSuffix(listingBody, "}") + `, "imageMediaIds": ["` + mediaID + `"], "publish": true}`
	req := jsonRequest(http.MethodPost, "/v1/listings", tok, body)
	w := serve(t, r, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var listing api.SellerListing
	if err := json.Unmarshal(w.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	// An admin suspended it (direct SQL: moderation is Phase 20b).
	if _, err := pool.Exec(context.Background(),
		`UPDATE listings SET status = 'suspended' WHERE id = $1`, listing.Id); err != nil {
		t.Fatal(err)
	}

	req = jsonRequest(http.MethodPatch, "/v1/me/listings/"+listing.Id.String(), tok,
		`{"title": "New Title Here"}`)
	w = serve(t, r, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("patch suspended: %d %s", w.Code, w.Body.String())
	}
	assertErrorEnvelope(t, w.Body.Bytes(), apierror.CodeListingSuspended)
	assertContract(t, req, w)

	req = jsonRequest(http.MethodPost, "/v1/me/listings/"+listing.Id.String()+"/archive", tok, "")
	w = serve(t, r, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("archive suspended: %d %s", w.Code, w.Body.String())
	}
	assertErrorEnvelope(t, w.Body.Bytes(), apierror.CodeInvalidTransition)
}

func TestListings_ValidationErrors(t *testing.T) {
	// A raised `sensitive` budget: this test makes several create calls (the
	// policy itself is covered by the media tests).
	limits := middleware.DefaultRateLimits()
	limits.Operation["sensitive"] = ratelimit.Rule{Name: "sensitive", Limit: 100, Period: time.Minute, Burst: 50}
	r, pool, _ := listingRouterWithLimits(t, &limits)
	tok := withProfile(t, pool, "validation-seller")

	for name, body := range map[string]string{
		"bad unit": `{"categorySlug":"livestock-poultry-cattle","title":"Healthy Friesian Heifer",
			"description":"Well-fed heifer, vaccinated and ready for sale on the farm.",
			"price":{"amount":850000,"currency":"GHS"},"unit":"metric_ton","quantityAvailable":1,
			"itemState":"adult","region":"Ashanti","district":"Kumasi Metro",
			"deliveryOptions":{"pickup":true,"sellerDelivery":false}}`,
		"parent category": `{"categorySlug":"livestock-poultry","title":"Healthy Friesian Heifer",
			"description":"Well-fed heifer, vaccinated and ready for sale on the farm.",
			"price":{"amount":850000,"currency":"GHS"},"unit":"heads","quantityAvailable":1,
			"itemState":"adult","region":"Ashanti","district":"Kumasi Metro",
			"deliveryOptions":{"pickup":true,"sellerDelivery":false}}`,
		"missing required attribute": `{"categorySlug":"land-leasing-farmland-for-rent",
			"title":"Irrigated Farmland For Rent",
			"description":"Twelve acres of irrigated farmland available for rent yearly.",
			"price":{"amount":5000000,"currency":"GHS"},"unit":"acres","quantityAvailable":1,
			"itemState":"developed","region":"Ashanti","district":"Kumasi Metro",
			"deliveryOptions":{"pickup":true,"sellerDelivery":false}}`,
		"unknown attribute": `{"categorySlug":"livestock-poultry-cattle","title":"Healthy Friesian Heifer",
			"description":"Well-fed heifer, vaccinated and ready for sale on the farm.",
			"price":{"amount":850000,"currency":"GHS"},"unit":"heads","quantityAvailable":1,
			"itemState":"adult","region":"Ashanti","district":"Kumasi Metro",
			"deliveryOptions":{"pickup":true,"sellerDelivery":false},
			"attributes":{"land_size_acres":"5"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			req := jsonRequest(http.MethodPost, "/v1/listings", tok, body)
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

	// A wrong currency never reaches the service: the spec enum rejects it.
	req := jsonRequest(http.MethodPost, "/v1/listings", tok,
		strings.Replace(listingBody, `"currency": "GHS"`, `"currency": "USD"`, 1))
	w := serve(t, r, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("USD price: %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)

	// An empty patch is rejected by the spec (minProperties: 1).
	req = jsonRequest(http.MethodPatch, "/v1/me/listings/"+uuid.NewString(), tok, `{}`)
	if w := serve(t, r, req); w.Code != http.StatusBadRequest {
		t.Errorf("empty patch: %d", w.Code)
	}
}
