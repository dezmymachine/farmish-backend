package checkout_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/auth"
	"github.com/dezmymachine/farmish-backend/internal/catalog"
	"github.com/dezmymachine/farmish-backend/internal/checkout"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/delivery"
	"github.com/dezmymachine/farmish-backend/internal/listings"
	"github.com/dezmymachine/farmish-backend/internal/media"
	"github.com/dezmymachine/farmish-backend/internal/media/mediatest"
	"github.com/dezmymachine/farmish-backend/internal/sellers"
	"github.com/dezmymachine/farmish-backend/internal/users"
)

type serviceFixture struct {
	pool     *pgxpool.Pool
	listings *listings.Service
	store    *media.R2
	svc      *checkout.Service
	now      time.Time
}

func newServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Pool(t)
	if err := catalog.Seed(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := mediatest.R2(t)
	sellersSvc := sellers.New(pool, nil, nil)
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	listingsSvc := listings.New(pool, catalog.New(pool), media.New(pool, store), sellersSvc)
	listingsSvc.Now = func() time.Time { return now }
	svc := checkout.New(pool, delivery.Manual{}, 195)
	svc.Now = func() time.Time { return now }
	return &serviceFixture{pool: pool, listings: listingsSvc, store: store, svc: svc, now: now}
}

func serviceUser(t *testing.T, pool *pgxpool.Pool, uid string) uuid.UUID {
	t.Helper()
	user, err := users.New(pool).Resolve(context.Background(), auth.Identity{
		UID: uid, Email: uid + "@farmish.test", Provider: "password",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sellers.New(pool, nil, nil).UpsertMine(context.Background(), user.ID, sellers.ProfileInput{
		BusinessName: uid + " farms", Region: "Ashanti", District: "Kumasi Metro",
	}); err != nil {
		t.Fatal(err)
	}
	return user.ID
}

var serviceHTTPClient = &http.Client{Timeout: 10 * time.Second}

func serviceImage(t *testing.T, pool *pgxpool.Pool, store *media.R2, owner uuid.UUID) uuid.UUID {
	t.Helper()
	up, err := media.New(pool, store).CreateUpload(context.Background(), owner, media.PurposeListingImage, "image/jpeg", 64)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("a", 64)
	req, err := http.NewRequest(http.MethodPut, up.URL, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(body))
	for key, value := range up.Headers {
		req.Header.Set(key, value)
	}
	resp, err := serviceHTTPClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload: %d", resp.StatusCode)
	}
	return up.ID
}

func serviceListing(t *testing.T, f *serviceFixture, owner uuid.UUID, modify func(*listings.Input)) listings.View {
	t.Helper()
	ctx := context.Background()
	fee := int64(500)
	in := listings.Input{
		CategorySlug: "livestock-poultry-cattle", Title: "Service Fixture Heifer",
		Description:  "Well-fed heifer, vaccinated and ready for sale on the farm.",
		PricePesewas: 850000, Unit: "heads", QuantityAvailable: 12, MinOrderQty: 1,
		IsNegotiable: true, ItemState: "adult", Region: "Ashanti", District: "Kumasi Metro",
		Delivery: listings.Delivery{Pickup: true, SellerDelivery: true, FeePesewas: &fee},
	}
	if modify != nil {
		modify(&in)
	}
	view, err := f.listings.Create(ctx, owner, in, false)
	if err != nil {
		t.Fatal(err)
	}
	image := serviceImage(t, f.pool, f.store, owner)
	if _, err := f.listings.Update(ctx, owner, view.ID, listings.Patch{
		ImageMediaIDs: []uuid.UUID{image},
	}); err != nil {
		t.Fatal(err)
	}
	active, err := f.listings.Publish(ctx, owner, view.ID)
	if err != nil {
		t.Fatal(err)
	}
	return active
}

func setCommissionOverride(t *testing.T, pool *pgxpool.Pool, category uuid.UUID, rate int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO commission_configs (category_id, rate_bps) VALUES ($1, $2)
		 ON CONFLICT (category_id) DO UPDATE SET rate_bps = EXCLUDED.rate_bps`,
		category, rate); err != nil {
		t.Fatal(err)
	}
}

func categoryIDs(t *testing.T, pool *pgxpool.Pool, slug string) (uuid.UUID, *uuid.UUID) {
	t.Helper()
	detail, err := catalog.New(pool).Detail(context.Background(), slug)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Parent == nil {
		t.Fatalf("category %s has no parent", slug)
	}
	parent := detail.Parent.ID
	return detail.ID, &parent
}

func TestService_Rates(t *testing.T) {
	f := newServiceFixture(t)
	rates, err := f.svc.Rates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rates.Default != 500 || len(rates.Overrides) != 0 {
		t.Errorf("rates = %+v, want the seeded 500bps default", rates)
	}
}

func TestService_QuoteTwoSellers(t *testing.T) {
	f := newServiceFixture(t)
	ctx := context.Background()
	sellerA := serviceUser(t, f.pool, "quote-seller-a")
	sellerB := serviceUser(t, f.pool, "quote-seller-b")
	buyer := serviceUser(t, f.pool, "quote-buyer")

	cattleID, cattleParent := categoryIDs(t, f.pool, "livestock-poultry-cattle")
	grainID, grainParent := categoryIDs(t, f.pool, "fresh-produce-grains-cereals")
	setCommissionOverride(t, f.pool, cattleID, 300)
	setCommissionOverride(t, f.pool, *cattleParent, 400)
	setCommissionOverride(t, f.pool, grainID, 700)
	_ = grainParent

	first := serviceListing(t, f, sellerA, func(in *listings.Input) {
		in.Title = "Quote Cattle"
		in.PricePesewas = 1000
		in.QuantityAvailable = 10
		in.MinOrderQty = 1
	})
	second := serviceListing(t, f, sellerA, func(in *listings.Input) {
		in.CategorySlug = "fresh-produce-grains-cereals"
		in.Title = "Quote Grain"
		in.Description = "Grain stored in a silo and available for pickup."
		in.PricePesewas = 2000
		in.Unit = "kg"
		in.QuantityAvailable = 10
		in.MinOrderQty = 2
		in.ItemState = "grade_a"
	})
	third := serviceListing(t, f, sellerB, func(in *listings.Input) {
		in.CategorySlug = "fresh-produce-grains-cereals"
		in.Title = "Other Seller Grain"
		in.Description = "Grain stored in a silo and available for pickup."
		in.PricePesewas = 5000
		in.Unit = "kg"
		in.QuantityAvailable = 2
		in.MinOrderQty = 1
		in.ItemState = "grade_b"
	})

	got, err := f.svc.Quote(ctx, buyer, checkout.QuoteInput{
		Lines: []checkout.CartLine{
			{ListingID: first.ID, Quantity: 2},
			{ListingID: second.ID, Quantity: 2},
			{ListingID: third.ID, Quantity: 1},
		},
		Delivery: map[uuid.UUID]checkout.DeliveryChoice{
			sellerA: {
				Method: delivery.MethodSellerDelivery, Address: "12 Market Road",
				RecipientName: "Ama Serwaa", RecipientPhone: "+233241234567",
			},
			sellerB: {Method: delivery.MethodPickup},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Orders) != 2 {
		t.Fatalf("orders = %+v, want one order per seller", got.Orders)
	}
	ordersBySeller := make(map[uuid.UUID]checkout.OrderQuote, len(got.Orders))
	for _, order := range got.Orders {
		ordersBySeller[order.SellerID] = order
	}
	firstOrder, secondOrder := ordersBySeller[sellerA], ordersBySeller[sellerB]
	if firstOrder.SubtotalPesewas != 6000 || firstOrder.DeliveryFeePesewas != 500 ||
		firstOrder.BasePesewas != 6500 || firstOrder.CommissionRateBps != 700 {
		t.Errorf("first order = %+v, want 6000/500/6500 at the highest category rate", firstOrder)
	}
	if secondOrder.CommissionRateBps != 700 || secondOrder.BasePesewas != 5000 {
		t.Errorf("second order = %+v, want the child override", secondOrder)
	}
	if got.BasePesewas != 11500 {
		t.Errorf("checkout base = %d, want 11500", got.BasePesewas)
	}
}
