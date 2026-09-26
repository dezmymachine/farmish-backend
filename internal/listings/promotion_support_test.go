package listings_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dezmymachine/farmish-backend/internal/database"
	"github.com/dezmymachine/farmish-backend/internal/listings"
)

// TestRequireAdvertisable covers the promotion support check: the caller's
// active, unexpired listing passes under a row lock, while unknown, borrowed
// and inactive listings keep their existing errors.
func TestRequireAdvertisable(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	in := validInput()
	view, err := f.svc.Create(ctx, f.seller, in, false)
	if err != nil {
		t.Fatal(err)
	}
	image := f.upload(t, f.seller, "image/jpeg", 64)
	if _, err := f.svc.Update(ctx, f.seller, view.ID, listings.Patch{
		ImageMediaIDs: []uuid.UUID{image},
	}); err != nil {
		t.Fatal(err)
	}
	active, err := f.svc.Publish(ctx, f.seller, view.ID)
	if err != nil {
		t.Fatal(err)
	}
	check := func(sellerID, id uuid.UUID, now time.Time) error {
		return database.InTx(ctx, f.pool, func(tx pgx.Tx) error {
			return f.svc.RequireAdvertisable(ctx, tx, sellerID, id, now)
		})
	}

	if err := check(f.seller, active.ID, time.Now()); err != nil {
		t.Errorf("active listing: %v", err)
	}
	if err := check(f.seller, uuid.New(), time.Now()); !errors.Is(err, listings.ErrNotFound) {
		t.Errorf("unknown listing err = %v, want ErrNotFound", err)
	}
	if err := check(f.other, active.ID, time.Now()); !errors.Is(err, listings.ErrForbidden) {
		t.Errorf("another seller err = %v, want ErrForbidden", err)
	}
	if _, err := f.svc.Archive(ctx, f.seller, active.ID); err != nil {
		t.Fatal(err)
	}
	if err := check(f.seller, active.ID, time.Now()); !errors.Is(err, listings.ErrListingNotActive) {
		t.Errorf("archived listing err = %v, want ErrListingNotActive", err)
	}
	if _, err := f.pool.Exec(ctx,
		`UPDATE listings SET status = 'active', expires_at = $2 WHERE id = $1`,
		active.ID, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := check(f.seller, active.ID, time.Now()); !errors.Is(err, listings.ErrListingNotActive) {
		t.Errorf("expired listing err = %v, want ErrListingNotActive", err)
	}
}
