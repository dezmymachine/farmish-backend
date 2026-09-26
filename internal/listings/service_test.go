package listings_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/auth"
	"github.com/dezmymachine/farmish-backend/internal/catalog"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/listings"
	"github.com/dezmymachine/farmish-backend/internal/media"
	"github.com/dezmymachine/farmish-backend/internal/media/mediatest"
	"github.com/dezmymachine/farmish-backend/internal/sellers"
	"github.com/dezmymachine/farmish-backend/internal/users"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

// fixture is a seeded database with one seller profile and local storage.
type fixture struct {
	pool   *pgxpool.Pool
	store  *media.R2
	svc    *listings.Service
	seller uuid.UUID
	other  uuid.UUID
}

// newFixture seeds the catalog, creates two sellers (both with profiles) and
// returns a service wired to real storage.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Pool(t)
	if err := catalog.Seed(ctx, pool); err != nil {
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
	f := &fixture{pool: pool, store: store, seller: mk("listing-seller-1"), other: mk("listing-seller-2")}
	f.svc = listings.New(pool, catalog.New(pool), media.New(pool, store), sellersSvc)
	return f
}

// httpClient is the plain client used for the test's own presigned uploads.
var httpClient = &http.Client{Timeout: 10 * time.Second}

// newPutRequest builds the PUT a presigned upload expects.
func newPutRequest(url string, headers map[string]string, body string) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodPut, url, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.ContentLength = int64(len(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

// upload creates a pending media object and PUTs its bytes, returning its id.
func (f *fixture) upload(t *testing.T, owner uuid.UUID, contentType string, size int) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	up, err := media.New(f.pool, f.store).CreateUpload(ctx, owner, media.PurposeListingImage, contentType, int64(size))
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("a", size)
	req, err := newPutRequest(up.URL, up.Headers, body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != 200 {
		t.Fatalf("upload: %d", resp.StatusCode)
	}
	return up.ID
}

// validInput is a complete, valid listing in a leaf category.
func validInput() listings.Input {
	fee := int64(500)
	return listings.Input{
		CategorySlug: "livestock-poultry-cattle", Title: "Healthy Friesian Heifer",
		Description:  "Well-fed heifer, vaccinated and ready for sale on the farm.",
		PricePesewas: 850000, Unit: "heads", QuantityAvailable: 12, MinOrderQty: 1,
		IsNegotiable: true, ItemState: "adult", Region: "Ashanti", District: "Kumasi Metro",
		Delivery: listings.Delivery{Pickup: true, SellerDelivery: true, FeePesewas: &fee},
	}
}

func fieldNames(t *testing.T, err error) []string {
	t.Helper()
	var ve *validation.Error
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v (%T), want *validation.Error", err, err)
	}
	out := make([]string, 0, len(ve.Fields))
	for _, f := range ve.Fields {
		out = append(out, f.Name)
	}
	return out
}

func hasField(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

func TestCreateListing_DraftAndPublish(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// A draft needs no image.
	draft, err := f.svc.Create(ctx, f.seller, validInput(), false)
	if err != nil {
		t.Fatal(err)
	}
	if draft.Status != listings.StatusDraft || draft.Slug != "healthy-friesian-heifer" ||
		draft.PublishedAt != nil || draft.ExpiresAt != nil {
		t.Fatalf("draft = %+v", draft)
	}

	// Publishing without an image fails with a field error.
	_, err = f.svc.Publish(ctx, f.seller, draft.ID)
	if names := fieldNames(t, err); !hasField(names, "imageMediaIds") {
		t.Errorf("publish without image: %v", names)
	}

	// Add an image, then publish.
	in := validInput()
	in.ImageMediaIDs = []uuid.UUID{f.upload(t, f.seller, "image/jpeg", 64)}
	updated, err := f.svc.Update(ctx, f.seller, draft.ID, listings.Patch{
		ImageMediaIDs: in.ImageMediaIDs, Attributes: in.Attributes,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Images) != 1 || updated.Images[0].URL == "" {
		t.Fatalf("images = %+v", updated.Images)
	}
	active, err := f.svc.Publish(ctx, f.seller, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if active.Status != listings.StatusActive || active.PublishedAt == nil || active.ExpiresAt == nil {
		t.Fatalf("published = %+v", active)
	}
	want := active.PublishedAt.Add(30 * 24 * time.Hour)
	if diff := active.ExpiresAt.Sub(want); diff > time.Second || diff < -time.Second {
		t.Errorf("expires_at = %v, want ~%v", active.ExpiresAt, want)
	}
}

func TestCreateListing_Validation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	negativeFee := int64(-5)
	tooMany := make([]uuid.UUID, 11)
	for i := range tooMany {
		tooMany[i] = uuid.New()
	}

	tests := map[string]struct {
		mutate func(*listings.Input)
		field  string
	}{
		"title too short":   {func(i *listings.Input) { i.Title = "Cows" }, "title"},
		"title too long":    {func(i *listings.Input) { i.Title = strings.Repeat("x", 101) }, "title"},
		"description short": {func(i *listings.Input) { i.Description = "short" }, "description"},
		"price zero":        {func(i *listings.Input) { i.PricePesewas = 0 }, "price"},
		"price too high":    {func(i *listings.Input) { i.PricePesewas = listings.MaxPricePesewas + 1 }, "price"},
		"wrong unit":        {func(i *listings.Input) { i.Unit = "metric_ton" }, "unit"},
		"wrong item state":  {func(i *listings.Input) { i.ItemState = "brand_new" }, "itemState"},
		"min above qty":     {func(i *listings.Input) { i.QuantityAvailable = 2; i.MinOrderQty = 5 }, "minOrderQty"},
		"bad region":        {func(i *listings.Input) { i.Region = "Accra" }, "region"},
		"no delivery":       {func(i *listings.Input) { i.Delivery = listings.Delivery{} }, "deliveryOptions"},
		"missing delivery fee": {func(i *listings.Input) {
			i.Delivery = listings.Delivery{Pickup: true, SellerDelivery: true}
		}, "deliveryOptions/sellerDeliveryFee"},
		"negative fee":     {func(i *listings.Input) { i.Delivery.FeePesewas = &negativeFee }, "deliveryOptions/sellerDeliveryFee"},
		"too many images":  {func(i *listings.Input) { i.ImageMediaIDs = tooMany }, "imageMediaIds"},
		"unknown category": {func(i *listings.Input) { i.CategorySlug = "nope" }, "categorySlug"},
		"parent category":  {func(i *listings.Input) { i.CategorySlug = "livestock-poultry" }, "categorySlug"},
		"bad quantity":     {func(i *listings.Input) { i.QuantityAvailable = -1; i.MinOrderQty = 1 }, "quantityAvailable"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			in := validInput()
			tt.mutate(&in)
			_, err := f.svc.Create(ctx, f.seller, in, false)
			names := fieldNames(t, err)
			if !hasField(names, tt.field) {
				t.Errorf("fields = %v, want %q (err %v)", names, tt.field, err)
			}
		})
	}
}

func TestCreateListing_Attributes(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	land := validInput()
	land.CategorySlug = "land-leasing-farmland-for-rent"
	land.Unit = "acres"
	land.ItemState = "developed"

	// Required attribute missing.
	_, err := f.svc.Create(ctx, f.seller, land, false)
	if names := fieldNames(t, err); !hasField(names, "attributes/land_size_acres") {
		t.Errorf("missing required: %v", names)
	}

	// Unknown key, bad select option, bad number, bad date, bad boolean.
	for name, attrs := range map[string]map[string]string{
		"unknown key":     {"not_an_attribute": "x"},
		"bad select":      {"land_size_acres": "5", "soil_type": "Volcanic"},
		"bad number":      {"land_size_acres": "five"},
		"number too big":  {"land_size_acres": "20000000000"},
		"date not a date": {"harvest_date": "26/09/2026"},
		"bad boolean":     {"organic": "yes"},
		"text too long":   {"packaging": strings.Repeat("x", 201)},
		"empty value":     {"organic": "  "},
	} {
		t.Run(name, func(t *testing.T) {
			in := land
			in.Attributes = attrs
			_, err := f.svc.Create(ctx, f.seller, in, false)
			names := fieldNames(t, err)
			if len(names) == 0 {
				t.Errorf("expected attribute field errors, got none")
			}
		})
	}

	// A valid set is stored.
	good := land
	good.Title = "Irrigated Farmland For Rent"
	good.Attributes = map[string]string{
		"land_size_acres": "12.5", "soil_type": "Loamy", "water_source": "Borehole",
	}
	created, err := f.svc.Create(ctx, f.seller, good, false)
	if err != nil {
		t.Fatal(err)
	}
	stored := map[string]string{}
	for _, a := range created.Attributes {
		stored[a.Key] = a.Value
	}
	if stored["land_size_acres"] != "12.5" || stored["soil_type"] != "Loamy" || len(stored) != 3 {
		t.Errorf("stored attributes = %v", stored)
	}

	// Editing a livestock listing with a land attribute is rejected.
	cattle := validInput()
	cattle.Attributes = map[string]string{"land_size_acres": "5"}
	if _, err := f.svc.Create(ctx, f.seller, cattle, false); err == nil {
		t.Error("land attribute accepted on a livestock listing")
	}
}

func TestCreateListing_RequiresSellerProfile(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u, err := users.New(f.pool).Resolve(ctx, auth.Identity{UID: "no-profile", Email: "np@farmish.test", Provider: "password"})
	if err != nil {
		t.Fatal(err)
	}

	_, err = f.svc.Create(ctx, u.ID, validInput(), false)
	if !errors.Is(err, listings.ErrSellerProfileRequired) {
		t.Fatalf("err = %v, want ErrSellerProfileRequired", err)
	}
}

func TestListing_OwnershipEnforced(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	created, err := f.svc.Create(ctx, f.seller, validInput(), false)
	if err != nil {
		t.Fatal(err)
	}

	// The owner can read it.
	if _, err := f.svc.GetOwn(ctx, f.seller, created.ID); err != nil {
		t.Errorf("owner read: %v", err)
	}
	// Another seller cannot.
	if _, err := f.svc.GetOwn(ctx, f.other, created.ID); !errors.Is(err, listings.ErrForbidden) {
		t.Errorf("other read = %v", err)
	}
	price := int64(100)
	for name, call := range map[string]func() error{
		"update": func() error {
			_, err := f.svc.Update(ctx, f.other, created.ID, listings.Patch{PricePesewas: &price})
			return err
		},
		"publish":  func() error { _, err := f.svc.Publish(ctx, f.other, created.ID); return err },
		"archive":  func() error { _, err := f.svc.Archive(ctx, f.other, created.ID); return err },
		"renew":    func() error { _, err := f.svc.Renew(ctx, f.other, created.ID); return err },
		"markSold": func() error { _, err := f.svc.MarkSold(ctx, f.other, created.ID); return err },
		"delete":   func() error { return f.svc.Delete(ctx, f.other, created.ID) },
	} {
		if err := call(); !errors.Is(err, listings.ErrForbidden) {
			t.Errorf("%s by another seller = %v, want ErrForbidden", name, err)
		}
	}
	// Unknown id: not found.
	ghost := uuid.New()
	if _, err := f.svc.GetOwn(ctx, f.seller, ghost); !errors.Is(err, listings.ErrNotFound) {
		t.Errorf("unknown read = %v", err)
	}
	if err := f.svc.Delete(ctx, f.seller, ghost); !errors.Is(err, listings.ErrNotFound) {
		t.Errorf("unknown delete = %v", err)
	}
}

func TestListing_SlugUniqueness(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	slugs := make([]string, 0, 3)
	for range 3 {
		created, err := f.svc.Create(ctx, f.seller, validInput(), false)
		if err != nil {
			t.Fatal(err)
		}
		slugs = append(slugs, created.Slug)
	}
	want := []string{"healthy-friesian-heifer", "healthy-friesian-heifer-2", "healthy-friesian-heifer-3"}
	for i, w := range want {
		if slugs[i] != w {
			t.Errorf("slug %d = %q, want %q", i, slugs[i], w)
		}
	}

	// Concurrent creates of the same title all succeed with distinct slugs.
	before := len(slugs)
	var wg sync.WaitGroup
	got := make([]string, 5)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			created, err := f.svc.Create(ctx, f.seller, validInput(), false)
			if err != nil {
				t.Errorf("concurrent create: %v", err)
				return
			}
			got[i] = created.Slug
		}()
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, s := range slugs {
		seen[s] = true
	}
	for _, s := range got {
		if s == "" {
			t.Fatal("a concurrent create produced no slug")
		}
		if seen[s] {
			t.Errorf("duplicate slug %q", s)
		}
		seen[s] = true
	}
	if len(seen) != before+len(got) {
		t.Errorf("distinct slugs = %d, want %d", len(seen), before+len(got))
	}
}

func TestListing_StatusTransitions(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// A published listing with an image, so publish and renew are possible.
	published := f.published(t, f.seller)

	// Allowed transitions.
	if _, err := f.svc.Archive(ctx, f.seller, published.ID); err != nil {
		t.Errorf("active -> archived: %v", err)
	}
	if _, err := f.svc.Publish(ctx, f.seller, published.ID); err != nil {
		t.Errorf("archived -> active: %v", err)
	}
	if _, err := f.svc.MarkSold(ctx, f.seller, published.ID); err != nil {
		t.Errorf("active -> sold: %v", err)
	}
	// sold is terminal for the owner.
	if _, err := f.svc.Publish(ctx, f.seller, published.ID); !errors.Is(err, listings.ErrInvalidTransition) {
		t.Errorf("sold -> active = %v", err)
	}

	// Draft cannot be sold, archived or renewed.
	draft, err := f.svc.Create(ctx, f.seller, validInput(), false)
	if err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func() error{
		"draft -> sold":     func() error { _, e := f.svc.MarkSold(ctx, f.seller, draft.ID); return e },
		"draft -> archived": func() error { _, e := f.svc.Archive(ctx, f.seller, draft.ID); return e },
		"draft -> renewed":  func() error { _, e := f.svc.Renew(ctx, f.seller, draft.ID); return e },
	} {
		if err := call(); !errors.Is(err, listings.ErrInvalidTransition) {
			t.Errorf("%s = %v, want ErrInvalidTransition", name, err)
		}
	}
	// Renew extends an active listing.
	before := *published.ExpiresAt
	renewed, err := f.svc.Renew(ctx, f.seller, published.ID)
	if err == nil {
		if !renewed.ExpiresAt.After(before) {
			t.Errorf("renew did not extend: %v -> %v", before, renewed.ExpiresAt)
		}
	}
	// An expired listing can be renewed or published; the sweep sets expired.
	expired := f.expired(t, f.seller)
	if _, err := f.svc.Renew(ctx, f.seller, expired.ID); err != nil {
		t.Errorf("expired -> active (renew): %v", err)
	}
	expired2 := f.expired(t, f.seller)
	if _, err := f.svc.Publish(ctx, f.seller, expired2.ID); err != nil {
		t.Errorf("expired -> active (publish): %v", err)
	}
}

func TestListing_SuspendedIsFrozen(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	created := f.published(t, f.seller)
	if _, err := f.pool.Exec(ctx, `UPDATE listings SET status = 'suspended' WHERE id = $1`, created.ID); err != nil {
		t.Fatal(err)
	}
	price := int64(1)
	if _, err := f.svc.Update(ctx, f.seller, created.ID, listings.Patch{PricePesewas: &price}); !errors.Is(err, listings.ErrSuspended) {
		t.Errorf("update suspended = %v", err)
	}
	for name, call := range map[string]func() error{
		"publish": func() error { _, e := f.svc.Publish(ctx, f.seller, created.ID); return e },
		"renew":   func() error { _, e := f.svc.Renew(ctx, f.seller, created.ID); return e },
		"sold":    func() error { _, e := f.svc.MarkSold(ctx, f.seller, created.ID); return e },
		"archive": func() error { _, e := f.svc.Archive(ctx, f.seller, created.ID); return e },
		"delete":  func() error { return f.svc.Delete(ctx, f.seller, created.ID) },
	} {
		if err := call(); !errors.Is(err, listings.ErrSuspended) {
			t.Errorf("%s suspended = %v, want ErrSuspended", name, err)
		}
	}
}

func TestListing_ImagesAttachOnce(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	mediaID := f.upload(t, f.seller, "image/jpeg", 64)

	first, err := f.svc.Create(ctx, f.seller, validInput(), false)
	if err != nil {
		t.Fatal(err)
	}
	withImage, err := f.svc.Update(ctx, f.seller, first.ID, listings.Patch{ImageMediaIDs: []uuid.UUID{mediaID}})
	if err != nil {
		t.Fatal(err)
	}
	if len(withImage.Images) != 1 {
		t.Fatalf("images = %+v", withImage.Images)
	}

	// The same media object cannot attach to a second listing.
	second, err := f.svc.Create(ctx, f.seller, validInput(), false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.Update(ctx, f.seller, second.ID, listings.Patch{ImageMediaIDs: []uuid.UUID{mediaID}})
	if !errors.Is(err, listings.ErrImageInUse) {
		t.Errorf("second listing with the same media = %v, want ErrImageInUse", err)
	}
	// The first listing still owns it.
	again, err := f.svc.GetOwn(ctx, f.seller, first.ID)
	if err != nil || len(again.Images) != 1 {
		t.Errorf("first listing images = %+v, %v", again.Images, err)
	}
	// Another seller's media is refused.
	foreign := f.upload(t, f.other, "image/jpeg", 64)
	if _, err := f.svc.Update(ctx, f.seller, first.ID, listings.Patch{
		ImageMediaIDs: []uuid.UUID{foreign},
	}); err == nil {
		t.Error("another seller's media accepted")
	}
}

func TestExpireDueListings(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// The clock is injected before creating anything, so "fresh" listings
	// really are fresh relative to it.
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	f.svc.Now = func() time.Time { return now }

	active := f.published(t, f.seller) // expires now+30d
	sold := f.published(t, f.seller)   // then marked sold
	if _, err := f.svc.MarkSold(ctx, f.seller, sold.ID); err != nil {
		t.Fatal(err)
	}
	draft, err := f.svc.Create(ctx, f.seller, validInput(), false)
	if err != nil {
		t.Fatal(err)
	}
	// An active listing already past its expiry.
	overdue := f.published(t, f.seller)
	if _, err := f.pool.Exec(ctx, `UPDATE listings SET expires_at = $2 WHERE id = $1`,
		overdue.ID, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	// An hour later the sweep expires exactly the due listing.
	f.svc.Now = func() time.Time { return now.Add(time.Hour) }
	n, err := f.svc.ExpireDue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expired %d, want 1", n)
	}
	status := func(id uuid.UUID) string {
		t.Helper()
		var s string
		if err := f.pool.QueryRow(ctx, `SELECT status FROM listings WHERE id = $1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if got := status(overdue.ID); got != listings.StatusExpired {
		t.Errorf("overdue = %q", got)
	}
	if got := status(active.ID); got != listings.StatusActive {
		t.Errorf("fresh active = %q", got)
	}
	if got := status(sold.ID); got != listings.StatusSold {
		t.Errorf("sold = %q", got)
	}
	if got := status(draft.ID); got != listings.StatusDraft {
		t.Errorf("draft = %q", got)
	}
	// The expired listing can be published again.
	if _, err := f.svc.Publish(ctx, f.seller, overdue.ID); err != nil {
		t.Errorf("publish an expired listing: %v", err)
	}
}

func TestListOwn_FiltersAndCounts(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	first := f.published(t, f.seller)
	if _, err := f.svc.Create(ctx, f.seller, validInput(), false); err != nil {
		t.Fatal(err)
	}
	// Another seller's listing never shows up.
	if _, err := f.svc.Create(ctx, f.other, validInput(), false); err != nil {
		t.Fatal(err)
	}

	items, total, err := f.svc.ListOwn(ctx, f.seller, "", 20, 0)
	if err != nil || total != 2 || len(items) != 2 {
		t.Fatalf("all: %d items, total %d, err %v", len(items), total, err)
	}
	if items[0].ID != first.ID && items[1].ID != first.ID {
		t.Error("published listing missing from the list")
	}
	active, total, err := f.svc.ListOwn(ctx, f.seller, listings.StatusActive, 20, 0)
	if err != nil || total != 1 || len(active) != 1 || active[0].ID != first.ID {
		t.Fatalf("active: %+v, total %d, err %v", active, total, err)
	}
	if active[0].CategorySlug != "livestock-poultry-cattle" || active[0].ImageCount != 1 {
		t.Errorf("summary = %+v", active[0])
	}
}

func TestUpdate_KeepsActiveAndDropsForeignAttributes(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	created := f.published(t, f.seller)

	// Editing an active listing keeps it active.
	price := int64(900000)
	edited, err := f.svc.Update(ctx, f.seller, created.ID, listings.Patch{PricePesewas: &price})
	if err != nil {
		t.Fatal(err)
	}
	if edited.Status != listings.StatusActive || edited.PricePesewas != price {
		t.Fatalf("edited = %+v", edited.Listing)
	}
	if edited.Slug != created.Slug {
		t.Error("slug changed on edit: URLs would break")
	}

	// Moving to another category re-validates and drops old attributes.
	moved, err := f.svc.Update(ctx, f.seller, created.ID, listings.Patch{
		CategorySlug: ptr("fresh-produce-vegetables"),
		Unit:         ptr("kg"),
		ItemState:    ptr("grade_a"),
		Attributes:   map[string]string{"organic": "true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if moved.CategorySlug != "fresh-produce-vegetables" {
		t.Fatalf("category = %q", moved.CategorySlug)
	}
	keys := map[string]bool{}
	for _, a := range moved.Attributes {
		keys[a.Key] = true
	}
	if len(keys) != 1 || !keys["organic"] {
		t.Errorf("attributes after category change = %v", keys)
	}
}

// helpers

// published creates a listing with one image and publishes it.
func (f *fixture) published(t *testing.T, owner uuid.UUID) listings.View {
	t.Helper()
	created, err := f.svc.Create(context.Background(), owner, validInput(), false)
	if err != nil {
		t.Fatal(err)
	}
	image := f.upload(t, owner, "image/jpeg", 64)
	updated, err := f.svc.Update(context.Background(), owner, created.ID, listings.Patch{
		ImageMediaIDs: []uuid.UUID{image},
	})
	if err != nil {
		t.Fatal(err)
	}
	active, err := f.svc.Publish(context.Background(), owner, updated.ID)
	if err != nil {
		t.Fatal(err)
	}
	return active
}

// expired creates a published listing and backdates its expiry, so the
// sweep can act on it.
func (f *fixture) expired(t *testing.T, owner uuid.UUID) listings.View {
	t.Helper()
	active := f.published(t, owner)
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE listings SET expires_at = now() - interval '1 hour' WHERE id = $1`, active.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ExpireDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	return active
}

func ptr[T any](v T) *T { return &v }
