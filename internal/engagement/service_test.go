package engagement_test

import (
	"context"
	"crypto/rand"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/dezmymachine/farmish-backend/internal/auth"
	"github.com/dezmymachine/farmish-backend/internal/catalog"
	"github.com/dezmymachine/farmish-backend/internal/crypto"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/engagement"
	"github.com/dezmymachine/farmish-backend/internal/orders"
	"github.com/dezmymachine/farmish-backend/internal/sellers"
	"github.com/dezmymachine/farmish-backend/internal/users"

	"github.com/jackc/pgx/v5/pgxpool"
)

// reviewFixture is an engagement service over a real database with a
// completed order carrying one listing.
type reviewFixture struct {
	pool     *pgxpool.Pool
	svc      *engagement.Service
	now      time.Time
	buyer    uuid.UUID
	seller   uuid.UUID
	stranger uuid.UUID
	listing  uuid.UUID
	order    uuid.UUID
}

func newReviewFixture(t *testing.T) *reviewFixture {
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
	mk := func(uid string) uuid.UUID {
		user, err := users.New(pool).Resolve(ctx, auth.Identity{
			UID: uid, Email: uid + "@farmish.test", Provider: "password",
		})
		if err != nil {
			t.Fatal(err)
		}
		return user.ID
	}
	buyer, seller, stranger := mk("review-buyer"), mk("review-seller"), mk("review-stranger")
	sellersSvc := sellers.New(pool, crypter, nil)
	if _, err := sellersSvc.UpsertMine(ctx, seller, sellers.ProfileInput{
		BusinessName: "Review Farm", Region: "Ashanti", District: "Kumasi Metro",
	}); err != nil {
		t.Fatalf("create profile: %v", err)
	}
	var categoryID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM categories WHERE parent_id IS NULL LIMIT 1`).Scan(&categoryID); err != nil {
		t.Fatal(err)
	}
	var listingID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO listings (seller_id, category_id, title, slug, description, price_pesewas,
		                       unit, quantity_available, item_state, status, region, district,
		                       published_at, expires_at)
		 VALUES ($1, $2, 'Review maize bags', $3, 'Good maize harvested this week here.', 5000,
		         'bags_50kg', 10, 'grade_a', 'active', 'Ashanti', 'Kumasi Metro',
		         now(), now() + interval '30 days')
		 RETURNING id`,
		seller, categoryID, "review-maize-"+uuid.NewString()[:8]).Scan(&listingID); err != nil {
		t.Fatal(err)
	}
	f := &reviewFixture{
		pool: pool, now: time.Now().UTC().Truncate(time.Second),
		buyer: buyer, seller: seller, stranger: stranger, listing: listingID,
	}
	ordersSvc := orders.NewService(pool, 48*time.Hour, 3*24*time.Hour)
	svc := engagement.New(pool, ordersSvc, users.New(pool), nil)
	svc.Now = func() time.Time { return f.now }
	f.svc = svc
	f.order = f.completedOrder(t, buyer, seller, listingID, 5000)
	return f
}

// completedOrder writes a checkout plus a completed order with one item.
func (f *reviewFixture) completedOrder(t *testing.T, buyer, seller, listingID uuid.UUID, subtotal int64) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var checkoutID, orderID uuid.UUID
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO checkouts (buyer_id, idempotency_key, request_hash, base_pesewas,
		                        processing_fee_pesewas, charge_pesewas, status, expires_at)
		 VALUES ($1, $2, 'review-test', $3, 0, $3, 'paid', now() + interval '30 minutes')
		 RETURNING id`, buyer, uuid.New(), subtotal).Scan(&checkoutID); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO orders (checkout_id, buyer_id, seller_id, status, escrow_state,
		                     subtotal_pesewas, delivery_fee_pesewas, base_pesewas,
		                     commission_rate_bps, commission_pesewas, delivery_method)
		 VALUES ($1, $2, $3, 'completed', 'released', $4, 0, $4, 500, $5, 'pickup')
		 RETURNING id`,
		checkoutID, buyer, seller, subtotal, (subtotal*500+5000)/10000).Scan(&orderID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO order_items (order_id, listing_id, title, unit, unit_price_pesewas, quantity, line_total_pesewas)
		 VALUES ($1, $2, 'Review maize bags', 'bags_50kg', $3, 1, $3)`,
		orderID, listingID, subtotal); err != nil {
		t.Fatal(err)
	}
	return orderID
}

func (f *reviewFixture) count(t *testing.T, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := f.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestReview_RequiresCompletedOrder proves paid/shipped/delivered refuse
// with 409 while completed records.
func TestReview_RequiresCompletedOrder(t *testing.T) {
	f := newReviewFixture(t)
	ctx := context.Background()
	for _, status := range []string{"paid", "shipped", "delivered"} {
		if _, err := f.pool.Exec(ctx, `UPDATE orders SET status = $1 WHERE id = $2`, status, f.order); err != nil {
			t.Fatal(err)
		}
		if _, err := f.svc.CreateReview(ctx, f.buyer, f.order, f.listing, 5, "Great."); !errors.Is(err, engagement.ErrOrderNotCompleted) {
			t.Errorf("%s err = %v, want ErrOrderNotCompleted", status, err)
		}
	}
	if _, err := f.pool.Exec(ctx, `UPDATE orders SET status = 'completed' WHERE id = $1`, f.order); err != nil {
		t.Fatal(err)
	}
	review, err := f.svc.CreateReview(ctx, f.buyer, f.order, f.listing, 5, "Great maize.")
	if err != nil {
		t.Fatalf("completed: %v", err)
	}
	if review.Rating != 5 || review.Comment == nil || *review.Comment != "Great maize." {
		t.Errorf("review = %+v", review)
	}
}

// TestReview_UniquePerOrderListingReviewer proves the second review of the
// same triple is a 409, while another order's review is fine.
func TestReview_UniquePerOrderListingReviewer(t *testing.T) {
	f := newReviewFixture(t)
	ctx := context.Background()
	if _, err := f.svc.CreateReview(ctx, f.buyer, f.order, f.listing, 5, "First."); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CreateReview(ctx, f.buyer, f.order, f.listing, 4, "Second."); !errors.Is(err, engagement.ErrAlreadyReviewed) {
		t.Errorf("second err = %v, want ErrAlreadyReviewed", err)
	}
	other := f.completedOrder(t, f.buyer, f.seller, f.listing, 3000)
	if _, err := f.svc.CreateReview(ctx, f.buyer, other, f.listing, 4, "Other order."); err != nil {
		t.Errorf("other order: %v", err)
	}
}

// TestReview_OnlyBuyerAndListingInOrder proves strangers are forbidden and
// foreign listings are refused.
func TestReview_OnlyBuyerAndListingInOrder(t *testing.T) {
	f := newReviewFixture(t)
	ctx := context.Background()
	if _, err := f.svc.CreateReview(ctx, f.stranger, f.order, f.listing, 5, "Mine."); !errors.Is(err, engagement.ErrForbidden) {
		t.Errorf("stranger err = %v, want ErrForbidden", err)
	}
	if _, err := f.svc.CreateReview(ctx, f.seller, f.order, f.listing, 5, "Mine."); !errors.Is(err, engagement.ErrForbidden) {
		t.Errorf("seller err = %v, want ErrForbidden", err)
	}
	if _, err := f.svc.CreateReview(ctx, f.buyer, uuid.New(), f.listing, 5, "Mine."); !errors.Is(err, engagement.ErrNotFound) {
		t.Errorf("unknown order err = %v, want ErrNotFound", err)
	}
	otherListing := uuid.New()
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO listings (id, seller_id, category_id, title, slug, description, price_pesewas,
		                       unit, quantity_available, item_state, status, region, district,
		                       published_at, expires_at)
		 SELECT $1, seller_id, category_id, 'Other maize', 'other-maize-x', description, price_pesewas,
		        unit, quantity_available, item_state, status, region, district, published_at, expires_at
		 FROM listings WHERE id = $2`, otherListing, f.listing); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CreateReview(ctx, f.buyer, f.order, otherListing, 5, "Mine."); !errors.Is(err, engagement.ErrListingNotInOrder) {
		t.Errorf("foreign listing err = %v, want ErrListingNotInOrder", err)
	}
	for name, tc := range map[string]struct {
		rating  int16
		comment string
	}{
		"zero rating": {0, ""}, "six rating": {6, ""}, "long comment": {5, string(make([]byte, 1001))},
	} {
		if _, err := f.svc.CreateReview(ctx, f.buyer, f.order, f.listing, tc.rating, tc.comment); err == nil {
			t.Errorf("%s: want a validation error", name)
		}
	}
}

// TestReview_HiddenExcludedFromAggregates proves hiding removes the review
// from lists and aggregates, with an audit row.
func TestReview_HiddenExcludedFromAggregates(t *testing.T) {
	f := newReviewFixture(t)
	ctx := context.Background()
	one, err := f.svc.CreateReview(ctx, f.buyer, f.order, f.listing, 5, "Great.")
	if err != nil {
		t.Fatal(err)
	}
	other := f.completedOrder(t, f.buyer, f.seller, f.listing, 3000)
	two, err := f.svc.CreateReview(ctx, f.buyer, other, f.listing, 3, "Fine.")
	if err != nil {
		t.Fatal(err)
	}
	items, total, summary, err := f.svc.ListReviews(ctx, f.listing, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(items) != 2 || summary.Count != 2 || summary.Average != 4.0 {
		t.Fatalf("list = %d %+v, want 2 items at 4.0", total, summary)
	}
	admin, err := users.New(f.pool).Resolve(ctx, auth.Identity{
		UID: "review-admin", Email: "review-admin@farmish.test", Provider: "password",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.HideReview(ctx, admin.ID, one.ID, "Spam."); err != nil {
		t.Fatalf("hide: %v", err)
	}
	items, total, summary, err = f.svc.ListReviews(ctx, f.listing, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(items) != 1 || items[0].ID != two.ID {
		t.Errorf("after hide: %d items, want only the second", len(items))
	}
	if summary.Count != 1 || summary.Average != 3.0 {
		t.Errorf("after hide summary = %+v, want 3.0 x 1", summary)
	}
	if _, err := f.svc.HideReview(ctx, admin.ID, one.ID, "Again."); !errors.Is(err, engagement.ErrAlreadyHidden) {
		t.Errorf("second hide err = %v, want ErrAlreadyHidden", err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM audit_events WHERE action = 'review.hide'`); n != 1 {
		t.Errorf("hide audits = %d, want 1", n)
	}
	if _, err := f.svc.HideReview(ctx, admin.ID, uuid.New(), "Missing."); !errors.Is(err, engagement.ErrNotFound) {
		t.Errorf("hide missing err = %v, want ErrNotFound", err)
	}
}

// TestFavorites_IdempotentAndCounter proves PUT×2 keeps one row and one
// count, DELETE×2 keeps zero, and concurrent adds from many users count
// exactly.
func TestFavorites_IdempotentAndCounter(t *testing.T) {
	f := newReviewFixture(t)
	ctx := context.Background()
	put := func() {
		t.Helper()
		if err := f.svc.AddFavorite(ctx, f.buyer, f.listing); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	put()
	put()
	if n := f.count(t, `SELECT COUNT(*) FROM favorites WHERE user_id = $1`, f.buyer); n != 1 {
		t.Fatalf("favorite rows = %d, want 1", n)
	}
	if n := f.count(t, `SELECT favorite_count FROM listings WHERE id = $1`, f.listing); n != 1 {
		t.Fatalf("counter = %d, want 1", n)
	}
	remove := func() {
		t.Helper()
		if err := f.svc.RemoveFavorite(ctx, f.buyer, f.listing); err != nil {
			t.Fatalf("remove: %v", err)
		}
	}
	remove()
	remove()
	if n := f.count(t, `SELECT favorite_count FROM listings WHERE id = $1`, f.listing); n != 0 {
		t.Fatalf("counter = %d, want 0", n)
	}

	const fans = 8
	var wg sync.WaitGroup
	for i := 0; i < fans; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			user, err := users.New(f.pool).Resolve(ctx, auth.Identity{
				UID: string(rune('a'+i)) + "-fan", Email: string(rune('a'+i)) + "-fan@farmish.test", Provider: "password",
			})
			if err != nil {
				t.Errorf("resolve %d: %v", i, err)
				return
			}
			if err := f.svc.AddFavorite(ctx, user.ID, f.listing); err != nil {
				t.Errorf("add %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if n := f.count(t, `SELECT favorite_count FROM listings WHERE id = $1`, f.listing); n != fans {
		t.Errorf("counter = %d, want %d", n, fans)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM favorites WHERE listing_id = $1`, f.listing); n != fans {
		t.Errorf("favorite rows = %d, want %d", n, fans)
	}

	// Unknown listings refuse.
	if err := f.svc.AddFavorite(ctx, f.buyer, uuid.New()); !errors.Is(err, engagement.ErrNotFound) {
		t.Errorf("add missing err = %v, want ErrNotFound", err)
	}
	// Removing a never-favourited listing is a silent no-op.
	if err := f.svc.RemoveFavorite(ctx, f.stranger, f.listing); err != nil {
		t.Errorf("remove missing: %v", err)
	}
	if n := f.count(t, `SELECT favorite_count FROM listings WHERE id = $1`, f.listing); n != fans {
		t.Errorf("counter after no-op remove = %d, want %d", n, fans)
	}
}
