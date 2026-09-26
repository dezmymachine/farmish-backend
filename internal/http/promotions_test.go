package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/dezmymachine/farmish-backend/internal/auth/authtest"
	"github.com/dezmymachine/farmish-backend/internal/catalog"
	"github.com/dezmymachine/farmish-backend/internal/config"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/http/middleware"
	"github.com/dezmymachine/farmish-backend/internal/jobs"
	"github.com/dezmymachine/farmish-backend/internal/ledger"
	"github.com/dezmymachine/farmish-backend/internal/listings"
	"github.com/dezmymachine/farmish-backend/internal/media"
	"github.com/dezmymachine/farmish-backend/internal/media/mediatest"
	"github.com/dezmymachine/farmish-backend/internal/money"
	"github.com/dezmymachine/farmish-backend/internal/payments"
	"github.com/dezmymachine/farmish-backend/internal/payments/fake"
	"github.com/dezmymachine/farmish-backend/internal/payments/paystacktest"
	"github.com/dezmymachine/farmish-backend/internal/promotions"
	"github.com/dezmymachine/farmish-backend/internal/ratelimit"
	"github.com/dezmymachine/farmish-backend/internal/sellers"
	"github.com/dezmymachine/farmish-backend/internal/users"
)

type promotionHTTPFixture struct {
	router   *gin.Engine
	pool     *pgxpool.Pool
	store    *media.R2
	provider *fake.Provider
	events   <-chan *river.Event
}

// newPromotionHTTPFixture wires listings, payments, promotions, River and the
// router, so the end-to-end test exercises the same registration production
// uses.
func newPromotionHTTPFixture(t *testing.T) *promotionHTTPFixture {
	t.Helper()
	return newPromotionHTTPFixtureWithLimits(t, nil)
}

func newPromotionHTTPFixtureWithLimits(t *testing.T, limits *middleware.RateLimits) *promotionHTTPFixture {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Pool(t)
	if err := catalog.Seed(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := promotions.Seed(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := mediatest.R2(t)
	sellersSvc := sellers.New(pool, nil, nil)
	listingsSvc := listings.New(pool, catalog.New(pool), media.New(pool, store), sellersSvc)
	provider := fake.New()
	log := slog.New(slog.DiscardHandler)
	paymentsSvc := payments.New(pool, provider, log, paystackFeeBps, "https://farmish.gh/payments/status")
	promoSvc := promotions.New(pool, paymentsSvc, listingsSvc, ledger.New())
	reg := jobs.NewRegistry()
	payments.RegisterSucceeded(reg, paymentsSvc, log)
	client, err := jobs.NewClient(pool, reg, log, jobs.Options{Work: true, FetchPollInterval: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	events, cancel := client.Subscribe(river.EventKindJobCompleted)
	t.Cleanup(cancel)
	if err := client.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	paymentsSvc.AttachJobClient(client)
	paymentsSvc.RegisterPurpose(payments.PurposePromotion, promoSvc.HandlePromotionPaid)
	t.Cleanup(func() {
		if err := jobs.Stop(client, 5*time.Second, 2*time.Second, log); err != nil {
			t.Errorf("stop jobs: %v", err)
		}
	})

	fb := authtest.Firebase(t)
	deps := Deps{
		DB: fakePinger{}, Verifier: fb, Users: users.New(pool),
		Sellers: sellersSvc, Media: media.New(pool, store),
		Listings: listingsSvc, PublicListings: listingsSvc,
		Payments: paymentsSvc, Promotions: promoSvc,
	}
	if limits != nil {
		deps.RateLimits = limits
		deps.SharedLimiter = ratelimit.NewMemory()
	}
	router := newTestRouterWithConfig(t, deps, config.Config{
		Env: config.EnvTest, CORSOrigins: []string{"https://farmish.gh"},
		Paystack: config.Paystack{SecretKey: paystackSecret},
	})
	return &promotionHTTPFixture{router: router, pool: pool, store: store, provider: provider, events: events}
}

func publishPromotionListing(t *testing.T, f *promotionHTTPFixture, token, title string) uuid.UUID {
	t.Helper()
	id := createListing(t, f.router, token)
	mediaID := uploadImage(t, f.pool, f.store, token)
	req := jsonRequest(http.MethodPatch, "/v1/me/listings/"+id.String(), token,
		`{"title":`+strconv.Quote(title)+`,"imageMediaIds":["`+mediaID+`"]}`)
	w := serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("attach image: %d %s", w.Code, w.Body.String())
	}
	req = jsonRequest(http.MethodPost, "/v1/me/listings/"+id.String()+"/publish", token, "")
	w = serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("publish: %d %s", w.Code, w.Body.String())
	}
	return id
}

func TestPromotionConfigs_SeededAndPublic(t *testing.T) {
	f := newPromotionHTTPFixture(t)
	req := jsonRequest(http.MethodGet, "/v1/promotions/configs", "", "")
	w := serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("configs: %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	if got := w.Header().Get("Cache-Control"); got != "public, max-age=300" {
		t.Errorf("Cache-Control = %q", got)
	}
	var body api.PromotionConfigList
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	want := []struct {
		tier    api.PromotionTier
		price   int64
		credits int32
	}{
		{"top", 5500, 55}, {"vip", 7500, 75}, {"diamond", 10000, 100}, {"enterprise", 15000, 150},
	}
	if len(body.Items) != len(want) {
		t.Fatalf("configs = %+v, want four tiers", body.Items)
	}
	for i := range want {
		got := body.Items[i]
		if got.Tier != want[i].tier || got.Price.Amount != want[i].price || got.Credits != want[i].credits ||
			got.Price.Currency != api.MoneyCurrencyGHS || got.Description == "" || len(got.Features) == 0 {
			t.Errorf("config %d = %+v", i, got)
		}
	}
}

func TestEndToEnd_PurchaseWebhookApplyRank(t *testing.T) {
	f := newPromotionHTTPFixture(t)
	seller := withProfile(t, f.pool, "promotion-seller")
	buyer := userToken(t, f.pool, authtest.Firebase(t), "promotion-buyer")
	promotedID := publishPromotionListing(t, f, seller, "Promoted Friesian Heifer")
	plainID := publishPromotionListing(t, f, seller, "Plain Friesian Heifer")

	// Any authenticated buyer can purchase. The webhook grants the snapshot
	// credits, and its replay cannot grant them again.
	buyerPurchase := purchasePromotion(t, f, buyer, "vip", 7500, 75)
	grantPromotionPayment(t, f, buyerPurchase, 424242)
	if got := getPromotionBalance(t, f, buyer); got != 75 {
		t.Fatalf("buyer balance = %d, want 75", got)
	}

	// The seller buys the package actually spent below.
	sellerPurchase := purchasePromotion(t, f, seller, "top", 5500, 55)
	grantPromotionPayment(t, f, sellerPurchase, 424243)
	if got := getPromotionBalance(t, f, seller); got != 55 {
		t.Fatalf("seller balance = %d, want 55", got)
	}

	// Spending on the seller's own listing ranks it first.
	req := jsonRequest(http.MethodPost, "/v1/promotions/applications", seller,
		`{"listingId":"`+promotedID.String()+`","tier":"top"}`)
	w := serve(t, f.router, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("apply: %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var application api.PromotionApplication
	if err := json.Unmarshal(w.Body.Bytes(), &application); err != nil {
		t.Fatal(err)
	}
	if application.ListingId != promotedID || application.Tier != "top" || application.CreditsSpent != 55 {
		t.Errorf("application = %+v", application)
	}
	if got := getPromotionBalance(t, f, seller); got != 0 {
		t.Errorf("seller balance = %d, want 0 after spending top", got)
	}

	req = jsonRequest(http.MethodGet, "/v1/listings?q=Friesian&limit=20", "", "")
	w = serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("search: %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var results api.ListingSearchResult
	if err := json.Unmarshal(w.Body.Bytes(), &results); err != nil {
		t.Fatal(err)
	}
	if len(results.Items) != 2 || results.Items[0].Id != promotedID || results.Items[1].Id != plainID {
		t.Fatalf("search order = %+v, want the promoted listing first", results.Items)
	}
	if results.Items[0].Promoted == nil || results.Items[1].Promoted != nil {
		t.Errorf("promoted flags = %+v and %+v", results.Items[0].Promoted, results.Items[1].Promoted)
	}
}

func purchasePromotion(t *testing.T, f *promotionHTTPFixture, token, tier string, price, credits int64) api.PromotionPurchase {
	t.Helper()
	req := jsonRequest(http.MethodPost, "/v1/promotions/purchases", token, `{"tier":"`+tier+`"}`)
	w := serve(t, f.router, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("purchase %s: %d %s", tier, w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var purchase api.PromotionPurchase
	if err := json.Unmarshal(w.Body.Bytes(), &purchase); err != nil {
		t.Fatal(err)
	}
	charge, fee, err := money.GrossUp(price, paystackFeeBps)
	if err != nil {
		t.Fatal(err)
	}
	if purchase.Price.Amount != price || purchase.Charge.Amount != charge || purchase.ProcessingFee.Amount != fee ||
		purchase.Credits != int32(credits) || purchase.Reference == "" || purchase.AuthorizationUrl == "" {
		t.Fatalf("purchase = %+v, want price %d and charge %d", purchase, price, charge)
	}
	return purchase
}

func grantPromotionPayment(t *testing.T, f *promotionHTTPFixture, purchase api.PromotionPurchase, eventID int64) {
	t.Helper()
	body := paystacktest.ChargeSuccessBody(purchase.Reference, purchase.Charge.Amount, eventID)
	w := postWebhook(f.router, body, paystacktest.Sign(paystackSecret, body))
	if w.Code != http.StatusOK {
		t.Fatalf("webhook: %d %s", w.Code, w.Body.String())
	}
	waitForPromotionJob(t, f.events)
	before := creditBalanceRows(t, f)
	// A duplicate event is recorded without enqueueing another job, so there is
	// nothing new to wait for; the stored balances must not move.
	w = postWebhook(f.router, body, paystacktest.Sign(paystackSecret, body))
	if w.Code != http.StatusOK {
		t.Fatalf("replay: %d %s", w.Code, w.Body.String())
	}
	if after := creditBalanceRows(t, f); after != before {
		t.Fatalf("ledger credit balances moved on replay: before %d, after %d", before, after)
	}
}

func creditBalanceRows(t *testing.T, f *promotionHTTPFixture) int64 {
	t.Helper()
	var total int64
	if err := f.pool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(amount), 0) FROM ledger_entries le
		 JOIN ledger_accounts la ON la.id = le.account_id
		 WHERE la.currency = 'CRD'`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	return total
}

func TestPromotions_EndpointFailures(t *testing.T) {
	// A raised sensitive budget: this test exercises the mapped failures,
	// while the policy itself is covered elsewhere.
	limits := middleware.DefaultRateLimits()
	limits.Operation["sensitive"] = ratelimit.Rule{Name: "sensitive", Limit: 100, Period: time.Minute, Burst: 50}
	f := newPromotionHTTPFixtureWithLimits(t, &limits)
	seller := withProfile(t, f.pool, "promotion-failure-seller")
	buyer := userToken(t, f.pool, authtest.Firebase(t), "promotion-failure-buyer")
	other := userToken(t, f.pool, authtest.Firebase(t), "promotion-failure-other")
	activeID := publishPromotionListing(t, f, seller, "Failure-Path Heifer")
	inactiveID := publishPromotionListing(t, f, seller, "Retired Heifer")
	archive := jsonRequest(http.MethodPost, "/v1/me/listings/"+inactiveID.String()+"/archive", seller, "")
	if w := serve(t, f.router, archive); w.Code != http.StatusOK {
		t.Fatalf("archive: %d %s", w.Code, w.Body.String())
	}

	check := func(name string, req *http.Request, want int) {
		t.Helper()
		w := serve(t, f.router, req)
		if w.Code != want {
			t.Errorf("%s: got %d %s, want %d", name, w.Code, w.Body.String(), want)
		}
		assertContract(t, req, w)
	}
	check("anonymous purchase", jsonRequest(http.MethodPost, "/v1/promotions/purchases", "", `{"tier":"top"}`), http.StatusUnauthorized)
	check("unknown tier", jsonRequest(http.MethodPost, "/v1/promotions/purchases", buyer, `{"tier":"no-such-tier"}`), http.StatusBadRequest)
	check("empty tier", jsonRequest(http.MethodPost, "/v1/promotions/purchases", buyer, `{"tier":""}`), http.StatusBadRequest)
	if _, err := f.pool.Exec(context.Background(), `UPDATE promotion_configs SET is_active = false WHERE tier = 'vip'`); err != nil {
		t.Fatal(err)
	}
	check("inactive tier", jsonRequest(http.MethodPost, "/v1/promotions/purchases", buyer, `{"tier":"vip"}`), http.StatusNotFound)

	f.provider.InitErr = payments.ErrProviderUnavailable
	check("provider unavailable", jsonRequest(http.MethodPost, "/v1/promotions/purchases", buyer, `{"tier":"top"}`), http.StatusBadGateway)
	f.provider.InitErr = nil

	check("anonymous credits", jsonRequest(http.MethodGet, "/v1/me/promotion-credits", "", ""), http.StatusUnauthorized)
	check("anonymous apply", jsonRequest(http.MethodPost, "/v1/promotions/applications", "", `{"listingId":"`+activeID.String()+`","tier":"top"}`), http.StatusUnauthorized)
	check("empty application", jsonRequest(http.MethodPost, "/v1/promotions/applications", seller, `{"listingId":"00000000-0000-0000-0000-000000000000","tier":"top"}`), http.StatusBadRequest)
	check("unknown listing", jsonRequest(http.MethodPost, "/v1/promotions/applications", seller, `{"listingId":"`+uuid.NewString()+`","tier":"top"}`), http.StatusNotFound)
	check("another owner's listing", jsonRequest(http.MethodPost, "/v1/promotions/applications", other, `{"listingId":"`+activeID.String()+`","tier":"top"}`), http.StatusForbidden)
	check("inactive listing", jsonRequest(http.MethodPost, "/v1/promotions/applications", seller, `{"listingId":"`+inactiveID.String()+`","tier":"top"}`), http.StatusConflict)
	check("insufficient credits", jsonRequest(http.MethodPost, "/v1/promotions/applications", seller, `{"listingId":"`+activeID.String()+`","tier":"top"}`), http.StatusConflict)
	check("anonymous promotion history", jsonRequest(http.MethodGet, "/v1/me/listings/"+activeID.String()+"/promotions", "", ""), http.StatusUnauthorized)
	check("unknown promotion history", jsonRequest(http.MethodGet, "/v1/me/listings/"+uuid.NewString()+"/promotions", seller, ""), http.StatusNotFound)
	check("another owner's history", jsonRequest(http.MethodGet, "/v1/me/listings/"+activeID.String()+"/promotions", other, ""), http.StatusForbidden)

	req := jsonRequest(http.MethodGet, "/v1/me/listings/"+activeID.String()+"/promotions", seller, "")
	w := serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("empty history: %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var history api.PromotionApplicationList
	if err := json.Unmarshal(w.Body.Bytes(), &history); err != nil {
		t.Fatal(err)
	}
	if len(history.Items) != 0 {
		t.Errorf("history = %+v, want no rows", history.Items)
	}
}

func getPromotionBalance(t *testing.T, f *promotionHTTPFixture, token string) int32 {
	t.Helper()
	req := jsonRequest(http.MethodGet, "/v1/me/promotion-credits", token, "")
	w := serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("credits: %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var body api.PromotionBalance
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Balance
}

func waitForPromotionJob(t *testing.T, events <-chan *river.Event) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case event := <-events:
			if event.Kind == river.EventKindJobCompleted && event.Job.Kind == "payments.succeeded" {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for payments.succeeded")
		}
	}
}
