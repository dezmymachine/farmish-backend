package engagement_test

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/dezmymachine/farmish-backend/internal/auth"
	"github.com/dezmymachine/farmish-backend/internal/catalog"
	"github.com/dezmymachine/farmish-backend/internal/crypto"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/engagement"
	"github.com/dezmymachine/farmish-backend/internal/listings"
	"github.com/dezmymachine/farmish-backend/internal/orders"
	"github.com/dezmymachine/farmish-backend/internal/sellers"
	"github.com/dezmymachine/farmish-backend/internal/users"
	"github.com/dezmymachine/farmish-backend/internal/validation"

	"github.com/jackc/pgx/v5/pgxpool"
)

// reportFixture is an engagement service with listings suspend wired.
type reportFixture struct {
	pool     *pgxpool.Pool
	svc      *engagement.Service
	listings *listings.Service
	reporter uuid.UUID
	seller   uuid.UUID
	listing  uuid.UUID
	admin    uuid.UUID
}

func newReportFixture(t *testing.T) *reportFixture {
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
	reporter, seller := mk("report-reporter"), mk("report-seller")
	sellersSvc := sellers.New(pool, crypter, nil)
	if _, err := sellersSvc.UpsertMine(ctx, seller, sellers.ProfileInput{
		BusinessName: "Report Farm", Region: "Ashanti", District: "Kumasi Metro",
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
		 VALUES ($1, $2, 'Report maize bags', $3, 'Good maize harvested this week here.', 5000,
		         'bags_50kg', 10, 'grade_a', 'active', 'Ashanti', 'Kumasi Metro',
		         now(), now() + interval '30 days')
		 RETURNING id`,
		seller, categoryID, "report-maize-"+uuid.NewString()[:8]).Scan(&listingID); err != nil {
		t.Fatal(err)
	}
	listingsSvc := listings.New(pool, catalog.New(pool), nil, sellersSvc)
	ordersSvc := orders.NewService(pool, 48*time.Hour, 3*24*time.Hour)
	svc := engagement.New(pool, ordersSvc, users.New(pool), nil)
	log := slog.New(slog.DiscardHandler)
	svc.AttachLogger(log)
	admin, err := users.New(pool).Resolve(ctx, auth.Identity{
		UID: "report-admin", Email: "report-admin@farmish.test", Provider: "password",
	})
	if err != nil {
		t.Fatal(err)
	}
	return &reportFixture{pool: pool, svc: svc, listings: listingsSvc, reporter: reporter, seller: seller, listing: listingID, admin: admin.ID}
}

// TestReport_OneOpenPerTarget proves a second open report is a 409 while a
// dismissed one frees the target.
func TestReport_OneOpenPerTarget(t *testing.T) {
	f := newReportFixture(t)
	ctx := context.Background()
	if _, err := f.svc.CreateReport(ctx, f.reporter, &f.listing, nil, engagement.ReportReasonSpam, "Spam listing."); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := f.svc.CreateReport(ctx, f.reporter, &f.listing, nil, engagement.ReportReasonFraud, "Also bad."); !errors.Is(err, engagement.ErrAlreadyReported) {
		t.Errorf("second err = %v, want ErrAlreadyReported", err)
	}
	// Dismissing frees the target: the same reporter may report again.
	views, _, err := f.svc.ListReports(ctx, engagement.ReportStatusOpen, 20, 0)
	if err != nil || len(views) != 1 {
		t.Fatalf("list = %d, %v", len(views), err)
	}
	if _, err := f.svc.ResolveReport(ctx, f.admin, views[0].Report.ID, engagement.ReportStatusDismissed, engagement.ReportActionNone, "Not spam.", nil, f.listings); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	if _, err := f.svc.CreateReport(ctx, views[0].Report.ReporterID, &f.listing, nil, engagement.ReportReasonSpam, "Again."); err != nil {
		t.Errorf("report after dismiss: %v", err)
	}
	// Another reporter on the same target is fine.
	other, err := users.New(f.pool).Resolve(ctx, auth.Identity{
		UID: "report-other", Email: "report-other@farmish.test", Provider: "password",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CreateReport(ctx, other.ID, &f.listing, nil, engagement.ReportReasonSpam, "Spam too."); err != nil {
		t.Errorf("other reporter: %v", err)
	}
}

// TestReport_ExactlyOneTarget proves both-or-neither targets are 400s.
func TestReport_ExactlyOneTarget(t *testing.T) {
	f := newReportFixture(t)
	ctx := context.Background()
	for name, tc := range map[string]struct {
		listing, user *uuid.UUID
	}{
		"both":    {&f.listing, &f.seller},
		"neither": {nil, nil},
	} {
		if _, err := f.svc.CreateReport(ctx, f.reporter, tc.listing, tc.user, engagement.ReportReasonSpam, "x"); err == nil {
			t.Errorf("%s: want a validation error", name)
		} else {
			var invalid *validation.Error
			if !errors.As(err, &invalid) {
				t.Errorf("%s err = %v, want validation", name, err)
			}
		}
	}
	if _, err := f.svc.CreateReport(ctx, f.reporter, nil, &f.seller, engagement.ReportReasonOther, ""); err == nil {
		t.Error("other without description: want a validation error")
	} else {
		var invalid *validation.Error
		if !errors.As(err, &invalid) {
			t.Errorf("err = %v, want validation", err)
		}
	}
	if _, err := f.svc.CreateReport(ctx, f.reporter, &f.listing, nil, engagement.ReportReasonOther, "Looks off."); err != nil {
		t.Errorf("other with description: %v", err)
	}
	if _, err := f.svc.CreateReport(ctx, f.reporter, uuidPtr(uuid.New()), nil, engagement.ReportReasonSpam, "x"); !errors.Is(err, engagement.ErrNotFound) {
		t.Errorf("missing listing err = %v, want ErrNotFound", err)
	}
}

func uuidPtr(id uuid.UUID) *uuid.UUID { return &id }

// TestReport_ResolveSuspendsListing proves an actioned suspension flips the
// listing in the same transaction with an audit row, and a second resolve
// is refused.
func TestReport_ResolveSuspendsListing(t *testing.T) {
	f := newReportFixture(t)
	ctx := context.Background()
	created, err := f.svc.CreateReport(ctx, f.reporter, &f.listing, nil, engagement.ReportReasonProhibitedItem, "Banned goods.")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := f.svc.ResolveReport(ctx, f.admin, created.ID, engagement.ReportStatusActioned, engagement.ReportActionSuspendListing, "Confirmed.", nil, f.listings)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.Status != engagement.ReportStatusActioned {
		t.Errorf("status = %s", resolved.Status)
	}
	var status string
	if err := f.pool.QueryRow(ctx, `SELECT status FROM listings WHERE id = $1`, f.listing).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "suspended" {
		t.Errorf("listing status = %s, want suspended", status)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM audit_events WHERE action = 'report.resolve'`); n != 1 {
		t.Errorf("resolve audits = %d, want 1", n)
	}
	if _, err := f.svc.ResolveReport(ctx, f.admin, created.ID, engagement.ReportStatusDismissed, engagement.ReportActionNone, "Again.", nil, f.listings); !errors.Is(err, engagement.ErrReportNotOpen) {
		t.Errorf("second resolve err = %v, want ErrReportNotOpen", err)
	}
	if _, err := f.svc.ResolveReport(ctx, f.admin, uuid.New(), engagement.ReportStatusDismissed, engagement.ReportActionNone, "Missing.", nil, f.listings); !errors.Is(err, engagement.ErrNotFound) {
		t.Errorf("missing resolve err = %v, want ErrNotFound", err)
	}
}

// TestReport_ResolveValidation proves the resolve matrix: suspending a user
// report, hiding without (or with a stray) review id, and bad enums are all
// refused before anything writes.
func TestReport_ResolveValidation(t *testing.T) {
	f := newReportFixture(t)
	ctx := context.Background()
	userReport, err := f.svc.CreateReport(ctx, f.reporter, nil, &f.seller, engagement.ReportReasonFraud, "Scammer.")
	if err != nil {
		t.Fatal(err)
	}
	listingReport, err := f.svc.CreateReport(ctx, f.reporter, &f.listing, nil, engagement.ReportReasonSpam, "Spam.")
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		reportID uuid.UUID
		status   string
		action   string
		note     string
		review   *uuid.UUID
		field    string
	}{
		"suspend user report": {userReport.ID, engagement.ReportStatusActioned, engagement.ReportActionSuspendListing, "No.", nil, "action"},
		"hide without review": {listingReport.ID, engagement.ReportStatusActioned, engagement.ReportActionHideReview, "No.", nil, "reviewId"},
		"bad status":          {listingReport.ID, "archived", engagement.ReportActionNone, "No.", nil, "status"},
		"bad action":          {listingReport.ID, engagement.ReportStatusDismissed, "ban", "No.", nil, "action"},
		"blank note":          {listingReport.ID, engagement.ReportStatusDismissed, engagement.ReportActionNone, "  ", nil, "note"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := f.svc.ResolveReport(ctx, f.admin, tc.reportID, tc.status, tc.action, tc.note, tc.review, f.listings)
			var invalid *validation.Error
			if !errors.As(err, &invalid) {
				t.Fatalf("err = %v, want validation", err)
			}
			found := false
			for _, field := range invalid.Fields {
				if field.Name == tc.field {
					found = true
				}
			}
			if !found {
				t.Errorf("fields = %+v, want %q", invalid.Fields, tc.field)
			}
		})
	}
	// A stray reviewId on a plain dismissal is refused too.
	someReview := uuid.New()
	if _, err := f.svc.ResolveReport(ctx, f.admin, listingReport.ID, engagement.ReportStatusDismissed, engagement.ReportActionNone, "No.", &someReview, f.listings); err == nil {
		t.Error("stray reviewId: want a validation error")
	}
	// hide_review with a named review hides it and resolves in one tx.
	review := f.reviewFor(t, f.listing)
	resolved, err := f.svc.ResolveReport(ctx, f.admin, listingReport.ID, engagement.ReportStatusActioned, engagement.ReportActionHideReview, "Offensive review.", &review, f.listings)
	if err != nil {
		t.Fatalf("hide resolve: %v", err)
	}
	if resolved.Action == nil || *resolved.Action != engagement.ReportActionHideReview {
		t.Errorf("action = %v", resolved.Action)
	}
	var hiddenAt *time.Time
	if err := f.pool.QueryRow(ctx, `SELECT hidden_at FROM reviews WHERE id = $1`, review).Scan(&hiddenAt); err != nil || hiddenAt == nil {
		t.Errorf("review hidden_at = %v, %v", hiddenAt, err)
	}
}

// reviewFor writes a completed order plus a visible review on it.
func (f *reportFixture) reviewFor(t *testing.T, listingID uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var checkoutID, orderID uuid.UUID
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO checkouts (buyer_id, idempotency_key, request_hash, base_pesewas,
		                        processing_fee_pesewas, charge_pesewas, status, expires_at)
		 VALUES ($1, $2, 'report-test', 5000, 0, 5000, 'paid', now() + interval '30 minutes')
		 RETURNING id`, f.reporter, uuid.New()).Scan(&checkoutID); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO orders (checkout_id, buyer_id, seller_id, status, escrow_state,
		                     subtotal_pesewas, delivery_fee_pesewas, base_pesewas,
		                     commission_rate_bps, commission_pesewas, delivery_method)
		 VALUES ($1, $2, $3, 'completed', 'released', 5000, 0, 5000, 500, 250, 'pickup')
		 RETURNING id`,
		checkoutID, f.reporter, f.seller).Scan(&orderID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO order_items (order_id, listing_id, title, unit, unit_price_pesewas, quantity, line_total_pesewas)
		 VALUES ($1, $2, 'Report maize', 'bags_50kg', 5000, 1, 5000)`,
		orderID, listingID); err != nil {
		t.Fatal(err)
	}
	review, err := f.svc.CreateReview(ctx, f.reporter, orderID, listingID, 1, "Awful.")
	if err != nil {
		t.Fatalf("seed review: %v", err)
	}
	return review.ID
}

func (f *reportFixture) count(t *testing.T, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := f.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
