package promotions_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/auth"
	"github.com/dezmymachine/farmish-backend/internal/catalog"
	"github.com/dezmymachine/farmish-backend/internal/database"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/ledger"
	"github.com/dezmymachine/farmish-backend/internal/listings"
	"github.com/dezmymachine/farmish-backend/internal/media"
	"github.com/dezmymachine/farmish-backend/internal/media/mediatest"
	"github.com/dezmymachine/farmish-backend/internal/money"
	"github.com/dezmymachine/farmish-backend/internal/payments"
	"github.com/dezmymachine/farmish-backend/internal/payments/fake"
	"github.com/dezmymachine/farmish-backend/internal/promotions"
	"github.com/dezmymachine/farmish-backend/internal/sellers"
	"github.com/dezmymachine/farmish-backend/internal/users"
)

const feeBps = 195

// fixture wires promotions to real catalog, listings, media, payments and the
// ledger. The provider is fake; everything else is the production path.
type fixture struct {
	pool     *pgxpool.Pool
	payments *payments.Service
	provider *fake.Provider
	svc      *promotions.Service
	listings *listings.Service
	store    *media.R2
	seller   uuid.UUID
	other    uuid.UUID
	buyer    uuid.UUID
	now      time.Time
}

func newFixture(t *testing.T) *fixture {
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
	mk := func(uid string) uuid.UUID {
		u, err := users.New(pool).Resolve(ctx, auth.Identity{UID: uid, Email: uid + "@farmish.test", Provider: "password"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sellersSvc.UpsertMine(ctx, u.ID, sellers.ProfileInput{
			BusinessName: uid + " farms", Region: "Ashanti", District: "Kumasi Metro",
		}); err != nil {
			t.Fatal(err)
		}
		return u.ID
	}
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	listingsSvc := listings.New(pool, catalog.New(pool), media.New(pool, store), sellersSvc)
	listingsSvc.Now = func() time.Time { return now }
	provider := fake.New()
	log := slog.New(slog.DiscardHandler)
	paymentsSvc := payments.New(pool, provider, log, feeBps, "https://farmish.gh/payments/status")
	svc := promotions.New(pool, paymentsSvc, listingsSvc, ledger.New())
	svc.Now = func() time.Time { return now }
	return &fixture{
		pool: pool, payments: paymentsSvc, provider: provider, svc: svc, listings: listingsSvc,
		store: store, seller: mk("promotion-seller"), other: mk("promotion-other"),
		buyer: mk("promotion-buyer"), now: now,
	}
}

var uploadClient = &http.Client{Timeout: 10 * time.Second}

func uploadImage(t *testing.T, pool *pgxpool.Pool, store *media.R2, owner uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	up, err := media.New(pool, store).CreateUpload(ctx, owner, media.PurposeListingImage, "image/jpeg", 64)
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
	resp, err := uploadClient.Do(req)
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

func newActiveListing(t *testing.T, pool *pgxpool.Pool, listingsSvc *listings.Service, store *media.R2, owner uuid.UUID, title string) listings.View {
	t.Helper()
	ctx := context.Background()
	fee := int64(500)
	in := listings.Input{
		CategorySlug: "livestock-poultry-cattle", Title: title,
		Description:  "Well-fed heifer, vaccinated and ready for sale on the farm.",
		PricePesewas: 850000, Unit: "heads", QuantityAvailable: 12, MinOrderQty: 1,
		IsNegotiable: true, ItemState: "adult", Region: "Ashanti", District: "Kumasi Metro",
		Delivery: listings.Delivery{Pickup: true, SellerDelivery: true, FeePesewas: &fee},
	}
	view, err := listingsSvc.Create(ctx, owner, in, false)
	if err != nil {
		t.Fatal(err)
	}
	image := uploadImage(t, pool, store, owner)
	if _, err := listingsSvc.Update(ctx, owner, view.ID, listings.Patch{
		ImageMediaIDs: []uuid.UUID{image},
	}); err != nil {
		t.Fatal(err)
	}
	active, err := listingsSvc.Publish(ctx, owner, view.ID)
	if err != nil {
		t.Fatal(err)
	}
	return active
}

// grant settles a synthetic promotion payment through the real purpose handler
// and ledger.
func (f *fixture) grant(t *testing.T, user uuid.UUID, reference string, price, charge, providerFee int64, credits int32) payments.Payment {
	t.Helper()
	payment := payments.Payment{
		ID: uuid.New(), Reference: reference, UserID: user, Purpose: payments.PurposePromotion,
		PurposeRef: "vip", Base: price, Fee: charge - price, Charge: charge,
		Currency: "GHS", Status: payments.StatusSuccess, ProviderFee: &providerFee,
		Metadata: map[string]any{promotions.MetadataCreditsKey: float64(credits)},
	}
	err := database.InTx(context.Background(), f.pool, func(tx pgx.Tx) error {
		return f.svc.HandlePromotionPaid(context.Background(), tx, payment)
	})
	if err != nil {
		t.Fatal(err)
	}
	return payment
}

func mustCredits(t *testing.T, f *fixture, user uuid.UUID, want int32) {
	t.Helper()
	got, err := f.svc.Credits(context.Background(), user)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("credits = %d, want %d", got, want)
	}
}

func TestPromotionConfigs_Seeded(t *testing.T) {
	f := newFixture(t)
	configs, err := f.svc.Configs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []promotions.Config{
		{Tier: "top", Name: "Starter", PricePesewas: 5500, Credits: 55, DurationDays: 7, TierRank: 1, SortOrder: 1, Active: true},
		{Tier: "vip", Name: "Growth", PricePesewas: 7500, Credits: 75, DurationDays: 14, TierRank: 2, SortOrder: 2, Active: true},
		{Tier: "diamond", Name: "Premium", PricePesewas: 10000, Credits: 100, DurationDays: 21, TierRank: 3, Featured: true, SortOrder: 3, Active: true},
		{Tier: "enterprise", Name: "Enterprise", PricePesewas: 15000, Credits: 150, DurationDays: 30, TierRank: 4, Featured: true, SortOrder: 4, Active: true},
	}
	if len(configs) != len(want) {
		t.Fatalf("configs = %+v, want four tiers", configs)
	}
	for i := range want {
		got := configs[i]
		if got.Tier != want[i].Tier || got.Name != want[i].Name || got.PricePesewas != want[i].PricePesewas ||
			got.Credits != want[i].Credits || got.DurationDays != want[i].DurationDays ||
			got.TierRank != want[i].TierRank || got.Featured != want[i].Featured ||
			got.SortOrder != want[i].SortOrder || !got.Active || got.Description == "" || len(got.Features) == 0 {
			t.Errorf("config %d = %+v, want %+v with marketing copy", i, got, want[i])
		}
	}
	before := configs[0]
	if err := promotions.Seed(context.Background(), f.pool); err != nil {
		t.Fatal(err)
	}
	after, err := f.svc.Tier(context.Background(), before.Tier)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Errorf("reseed changed the tier: before %+v, after %+v", before, after)
	}
}

func TestPurchase_InitializesGrossedUpCharge(t *testing.T) {
	f := newFixture(t)
	purchase, err := f.svc.Purchase(context.Background(), f.buyer, "buyer@farmish.test", promotions.TierVIP)
	if err != nil {
		t.Fatal(err)
	}
	charge, fee, err := money.GrossUp(7500, feeBps)
	if err != nil {
		t.Fatal(err)
	}
	if purchase.Payment.Base != 7500 || purchase.Payment.Charge != charge || purchase.Payment.Fee != fee {
		t.Errorf("payment = base %d fee %d charge %d, want 7500/%d/%d",
			purchase.Payment.Base, purchase.Payment.Fee, purchase.Payment.Charge, fee, charge)
	}
	if purchase.Payment.Purpose != payments.PurposePromotion || purchase.Payment.PurposeRef != promotions.TierVIP {
		t.Errorf("payment = %+v", purchase.Payment)
	}
	if purchase.Tier.Credits != 75 {
		t.Errorf("tier credits = %d, want 75", purchase.Tier.Credits)
	}
	if purchase.Payment.AuthorizationURL == nil || *purchase.Payment.AuthorizationURL == "" {
		t.Error("purchase has no authorization URL")
	}
	if len(f.provider.Initialized) != 1 {
		t.Fatalf("provider calls = %d", len(f.provider.Initialized))
	}
	if got := f.provider.Initialized[0].AmountPesewas; got != charge {
		t.Errorf("provider amount = %d, want the grossed-up charge %d", got, charge)
	}
	if got := f.provider.Initialized[0].Metadata[promotions.MetadataCreditsKey]; got != 75 {
		t.Errorf("provider metadata credits = %v, want the purchase-time snapshot", got)
	}
}

func TestPurchase_UnknownOrInactiveTier404(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.Purchase(context.Background(), f.buyer, "buyer@farmish.test", ""); err == nil {
		t.Error("empty tier was accepted")
	}
	if _, err := f.svc.Purchase(context.Background(), f.buyer, "buyer@farmish.test", "no-such-tier"); !errors.Is(err, promotions.ErrTierNotFound) {
		t.Errorf("unknown tier err = %v, want ErrTierNotFound", err)
	}
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE promotion_configs SET is_active = false WHERE tier = 'vip'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Purchase(context.Background(), f.buyer, "buyer@farmish.test", promotions.TierVIP); !errors.Is(err, promotions.ErrTierNotFound) {
		t.Errorf("inactive tier err = %v, want ErrTierNotFound", err)
	}
}

func TestHandlePromotionPaid_GrantsSnapshotCreditsOnce(t *testing.T) {
	f := newFixture(t)
	payment := f.grant(t, f.buyer, "snapshot-grant", 7500, 7650, 198, 75)
	mustCredits(t, f, f.buyer, 75)

	// The tier is edited after the purchase. A retry still grants the snapshot,
	// and the posted ledger row makes the second call a no-op.
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE promotion_configs SET credits = 1000 WHERE tier = 'vip'`); err != nil {
		t.Fatal(err)
	}
	err := database.InTx(context.Background(), f.pool, func(tx pgx.Tx) error {
		return f.svc.HandlePromotionPaid(context.Background(), tx, payment)
	})
	if !errors.Is(err, nil) && err != nil {
		t.Fatal(err)
	}
	mustCredits(t, f, f.buyer, 75)

	var transactions, entries int64
	if err := f.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM ledger_transactions WHERE kind = 'promotion_paid' AND reference = 'snapshot-grant'`).Scan(&transactions); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM ledger_entries le JOIN ledger_transactions lt ON lt.id = le.transaction_id
		 WHERE lt.kind = 'promotion_paid' AND lt.reference = 'snapshot-grant'`).Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if transactions != 1 || entries != 6 {
		t.Errorf("ledger rows = %d transactions and %d entries, want 1 and 6", transactions, entries)
	}
}

func TestApply_DeductsCreditsAndRanks(t *testing.T) {
	f := newFixture(t)
	view := newActiveListing(t, f.pool, f.listings, f.store, f.seller, "Promotable Heifer")
	f.grant(t, f.seller, "apply-grant", 5500, 5610, 110, 55)
	applied, err := f.svc.Apply(context.Background(), f.seller, promotions.ApplyInput{ListingID: view.ID, Tier: promotions.TierTop})
	if err != nil {
		t.Fatal(err)
	}
	if applied.Tier != promotions.TierTop || applied.CreditsSpent != 55 || !applied.StartsAt.Equal(f.now) ||
		!applied.EndsAt.Equal(f.now.AddDate(0, 0, 7)) || applied.ReplacedRemaining {
		t.Errorf("application = %+v", applied)
	}
	mustCredits(t, f, f.seller, 0)
}

func TestApply_ValidationOwnershipAndState(t *testing.T) {
	f := newFixture(t)
	view := newActiveListing(t, f.pool, f.listings, f.store, f.seller, "Owned Heifer")
	f.grant(t, f.seller, "ownership-grant", 5500, 5610, 110, 55)
	f.grant(t, f.other, "other-grant", 5500, 5610, 110, 55)

	if _, err := f.svc.Apply(context.Background(), f.other, promotions.ApplyInput{ListingID: view.ID, Tier: promotions.TierTop}); !errors.Is(err, listings.ErrForbidden) {
		t.Errorf("another seller err = %v, want listings.ErrForbidden", err)
	}
	if _, err := f.svc.Apply(context.Background(), f.seller, promotions.ApplyInput{ListingID: uuid.New(), Tier: promotions.TierTop}); !errors.Is(err, listings.ErrNotFound) {
		t.Errorf("unknown listing err = %v, want listings.ErrNotFound", err)
	}
	if _, err := f.svc.Apply(context.Background(), f.seller, promotions.ApplyInput{ListingID: view.ID, Tier: "no-such-tier"}); !errors.Is(err, promotions.ErrTierNotFound) {
		t.Errorf("unknown tier err = %v, want ErrTierNotFound", err)
	}
	if _, err := f.listings.Archive(context.Background(), f.seller, view.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Apply(context.Background(), f.seller, promotions.ApplyInput{ListingID: view.ID, Tier: promotions.TierTop}); !errors.Is(err, listings.ErrListingNotActive) {
		t.Errorf("inactive listing err = %v, want listings.ErrListingNotActive", err)
	}
	mustCredits(t, f, f.seller, 55)
}

func TestApply_InsufficientCredits(t *testing.T) {
	f := newFixture(t)
	view := newActiveListing(t, f.pool, f.listings, f.store, f.seller, "Unfunded Heifer")
	if _, err := f.svc.Apply(context.Background(), f.seller, promotions.ApplyInput{ListingID: view.ID, Tier: promotions.TierTop}); !errors.Is(err, promotions.ErrInsufficientCredits) {
		t.Fatalf("err = %v, want ErrInsufficientCredits", err)
	}
	var promotionsCount int64
	if err := f.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM listing_promotions WHERE listing_id = $1`, view.ID).Scan(&promotionsCount); err != nil {
		t.Fatal(err)
	}
	if promotionsCount != 0 {
		t.Errorf("listing promotions = %d, want 0", promotionsCount)
	}
}

func TestApply_ExtendUpgradeDowngrade(t *testing.T) {
	f := newFixture(t)
	view := newActiveListing(t, f.pool, f.listings, f.store, f.seller, "Tiered Heifer")
	f.grant(t, f.seller, "tier-grant", 7500, 7650, 198, 75)
	f.grant(t, f.seller, "second-tier-grant", 5500, 5610, 110, 55)
	f.grant(t, f.seller, "third-tier-grant", 7500, 7650, 198, 75)

	first, err := f.svc.Apply(context.Background(), f.seller, promotions.ApplyInput{ListingID: view.ID, Tier: promotions.TierTop})
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.svc.Apply(context.Background(), f.seller, promotions.ApplyInput{ListingID: view.ID, Tier: promotions.TierTop})
	if err != nil {
		t.Fatal(err)
	}
	if !second.StartsAt.Equal(first.EndsAt) || !second.EndsAt.Equal(first.EndsAt.AddDate(0, 0, 7)) || second.ReplacedRemaining {
		t.Errorf("extended application = %+v, want it to queue behind %+v", second, first)
	}
	mustCredits(t, f, f.seller, 95)

	upgrade, err := f.svc.Apply(context.Background(), f.seller, promotions.ApplyInput{ListingID: view.ID, Tier: promotions.TierVIP})
	if err != nil {
		t.Fatal(err)
	}
	if !upgrade.StartsAt.Equal(f.now) || !upgrade.EndsAt.Equal(f.now.AddDate(0, 0, 14)) || !upgrade.ReplacedRemaining {
		t.Errorf("upgrade = %+v, want an immediate replacement with forfeited remainder", upgrade)
	}
	mustCredits(t, f, f.seller, 20)
	var active, rank int64
	if err := f.pool.QueryRow(context.Background(),
		`SELECT COUNT(*), MAX(tier_rank) FROM listing_promotions
		 WHERE listing_id = $1 AND starts_at <= $2 AND ends_at > $2`,
		view.ID, f.now).Scan(&active, &rank); err != nil {
		t.Fatal(err)
	}
	// The forfeited row keeps a one-microsecond bridge because the schema
	// requires ends_at > starts_at. Rank decides the replacement.
	if active != 2 || rank != 2 {
		t.Errorf("active promotions = %d with highest rank %d, want 2 and 2", active, rank)
	}
	var forfeited time.Time
	if err := f.pool.QueryRow(context.Background(),
		`SELECT ends_at FROM listing_promotions WHERE id = $1`, first.ID).Scan(&forfeited); err != nil {
		t.Fatal(err)
	}
	if !forfeited.Equal(f.now.Add(time.Microsecond)) {
		t.Errorf("forfeited ends_at = %s, want one microsecond after replacement", forfeited)
	}

	f.grant(t, f.seller, "downgrade-balance-grant", 5500, 5610, 110, 55)
	mustCredits(t, f, f.seller, 75)
	if _, err := f.svc.Apply(context.Background(), f.seller, promotions.ApplyInput{ListingID: view.ID, Tier: promotions.TierTop}); !errors.Is(err, promotions.ErrPromotionDowngrade) {
		t.Errorf("downgrade err = %v, want ErrPromotionDowngrade", err)
	}
	mustCredits(t, f, f.seller, 75)
}

func TestApply_ConcurrentNeverNegative(t *testing.T) {
	f := newFixture(t)
	view := newActiveListing(t, f.pool, f.listings, f.store, f.seller, "Contended Heifer")
	f.grant(t, f.seller, "contention-grant", 7500, 7650, 198, 75)

	const applicants = 5
	var wg sync.WaitGroup
	errs := make([]error, applicants)
	for i := range applicants {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = f.svc.Apply(context.Background(), f.seller, promotions.ApplyInput{ListingID: view.ID, Tier: promotions.TierTop})
		}()
	}
	wg.Wait()

	succeeded := 0
	for _, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, promotions.ErrInsufficientCredits):
		default:
			t.Fatalf("concurrent apply err = %v", err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("succeeded = %d, want exactly 1", succeeded)
	}
	mustCredits(t, f, f.seller, 20)
	var balance int64
	if err := f.pool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(amount), 0) FROM ledger_entries le
		 JOIN ledger_accounts la ON la.id = le.account_id
		 WHERE la.code = $1`, ledger.PromoCredits(f.seller)).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if balance != -20 {
		t.Errorf("raw credit balance = %d, want -20", balance)
	}
}
