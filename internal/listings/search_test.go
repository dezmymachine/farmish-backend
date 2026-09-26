package listings_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/dezmymachine/farmish-backend/internal/listings"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

// searchFixture adds published listings with controlled fields.
type searchFixture struct {
	*fixture
}

// active creates a published listing with the given title, price, region,
// district, category, unit and item state.
func (f *searchFixture) active(t *testing.T, owner uuid.UUID, mut func(*listings.Input)) listings.View {
	t.Helper()
	in := validInput()
	if mut != nil {
		mut(&in)
	}
	view, err := f.svc.Create(context.Background(), owner, in, false)
	if err != nil {
		t.Fatalf("create %q: %v", in.Title, err)
	}
	image := f.upload(t, owner, "image/jpeg", 64)
	if _, err := f.svc.Update(context.Background(), owner, view.ID, listings.Patch{
		ImageMediaIDs: []uuid.UUID{image},
	}); err != nil {
		t.Fatal(err)
	}
	active, err := f.svc.Publish(context.Background(), owner, view.ID)
	if err != nil {
		t.Fatalf("publish %q: %v", in.Title, err)
	}
	return active
}

func newSearchFixture(t *testing.T) *searchFixture {
	t.Helper()
	return &searchFixture{newFixture(t)}
}

func ids(items []listings.PublicSummary) []string {
	out := make([]string, 0, len(items))
	for _, i := range items {
		out = append(out, i.Title)
	}
	return out
}

func TestSearch_Filters(t *testing.T) {
	f := newSearchFixture(t)
	ctx := context.Background()
	seller, other := f.seller, f.other

	maize := f.active(t, seller, func(i *listings.Input) {
		i.Title = "Yellow Maize Bulk"
		i.Description = "Freshly harvested yellow maize, one tonne bags."
		i.CategorySlug = "fresh-produce-grains-cereals"
		i.Unit = "kg"
		i.ItemState = "grade_a"
		i.PricePesewas = 40000
		i.Region, i.District = "Ashanti", "Kumasi Metro"
	})
	cassava := f.active(t, seller, func(i *listings.Input) {
		i.Title = "Cassava Tubers"
		i.Description = "Sweet cassava tubers, harvested this week."
		i.CategorySlug = "fresh-produce-root-crops"
		i.Unit = "kg"
		i.ItemState = "grade_b"
		i.PricePesewas = 15000
		i.Region, i.District = "Ashanti", "Ejisu"
	})
	tractor := f.active(t, other, func(i *listings.Input) {
		i.Title = "Used Tractor"
		i.Description = "A 2015 four wheel drive tractor, ready for work."
		i.CategorySlug = "farm-machinery-tractors"
		i.Unit = "pieces"
		i.ItemState = "used"
		i.PricePesewas = 8500000
		i.Region, i.District = "Greater Accra", "Accra Metropolitan"
	})
	f.active(t, seller, func(i *listings.Input) {
		i.Title = "Friesian Heifer Calf"
		i.Description = "A young Friesian heifer calf for the farm."
		i.CategorySlug, i.Unit, i.ItemState = "livestock-poultry-cattle", "heads", "young"
		i.PricePesewas = 400000
		i.Region, i.District = "Ashanti", "Kumasi Metro"
	})
	_ = maize

	search := func(t *testing.T, in listings.SearchInput) []listings.PublicSummary {
		t.Helper()
		if in.Limit == 0 {
			in.Limit = 50
		}
		if in.Page == 0 {
			in.Page = 1
		}
		res, err := f.svc.Search(ctx, in)
		if err != nil {
			t.Fatalf("search %+v: %v", in, err)
		}
		return res.Items
	}

	tests := map[string]struct {
		in   listings.SearchInput
		want []string
	}{
		"free text":         {listings.SearchInput{Q: ptr("maize")}, []string{"Yellow Maize Bulk"}},
		"trigram near miss": {listings.SearchInput{Q: ptr("Friesn Heifer")}, []string{"Friesian Heifer Calf"}},
		"no match":          {listings.SearchInput{Q: ptr("zzzznothing")}, []string{}},
		"child category":    {listings.SearchInput{Category: ptr("fresh-produce-root-crops")}, []string{"Cassava Tubers"}},
		"parent category expands": {
			listings.SearchInput{Category: ptr("fresh-produce")},
			[]string{"Cassava Tubers", "Yellow Maize Bulk"}, // newest first
		},
		"unknown category matches nothing": {listings.SearchInput{Category: ptr("no-such-category")}, []string{}},
		"region":                           {listings.SearchInput{Region: ptr("Greater Accra")}, []string{"Used Tractor"}},
		"district":                         {listings.SearchInput{District: ptr("Ejisu")}, []string{"Cassava Tubers"}},
		"item state":                       {listings.SearchInput{ItemState: ptr("young")}, []string{"Friesian Heifer Calf"}},
		"price range":                      {listings.SearchInput{MinPrice: ptr(int64(20000)), MaxPrice: ptr(int64(100000))}, []string{"Yellow Maize Bulk"}},
		"q + category":                     {listings.SearchInput{Q: ptr("cassava"), Category: ptr("fresh-produce")}, []string{"Cassava Tubers"}},
		"q + wrong category":               {listings.SearchInput{Q: ptr("cassava"), Category: ptr("livestock-poultry")}, []string{}},
		"region + price": {
			listings.SearchInput{Region: ptr("Ashanti"), MaxPrice: ptr(int64(20000))},
			[]string{"Cassava Tubers"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := search(t, tt.in)
			if strings.Join(ids(got), "|") != strings.Join(tt.want, "|") {
				t.Errorf("got %v, want %v", ids(got), tt.want)
			}
		})
	}
	_ = cassava
	_ = tractor
}

func TestSearch_Sorts(t *testing.T) {
	f := newSearchFixture(t)
	seller := f.seller
	f.active(t, seller, func(i *listings.Input) {
		i.Title = "Cheap Maize"
		i.Description = "Maize at a low price."
		i.CategorySlug, i.Unit, i.ItemState = "fresh-produce-grains-cereals", "kg", "grade_a"
		i.PricePesewas = 10000
	})
	f.active(t, seller, func(i *listings.Input) {
		i.Title = "Mid Grade Produce"
		i.Description = "An assorted maize shipment in the middle price range."
		i.CategorySlug, i.Unit, i.ItemState = "fresh-produce-grains-cereals", "kg", "grade_b"
		i.PricePesewas = 20000
	})
	f.active(t, seller, func(i *listings.Input) {
		i.Title = "Pricey Rice"
		i.Description = "Rice, quite expensive this season."
		i.CategorySlug, i.Unit, i.ItemState = "fresh-produce-grains-cereals", "kg", "grade_a"
		i.PricePesewas = 90000
	})

	for _, tt := range []struct {
		sort string
		want []string
	}{
		{listings.SortPriceAsc, []string{"Cheap Maize", "Mid Grade Produce"}},
		{listings.SortPriceDesc, []string{"Mid Grade Produce", "Cheap Maize"}},
		// Relevance ranks the title match first: "Cheap Maize" has the term in
		// its title (weight A) while "Mid Grade Produce" only mentions it in
		// the description (weight B).
		{listings.SortRelevance, []string{"Cheap Maize", "Mid Grade Produce"}},
	} {
		t.Run(tt.sort, func(t *testing.T) {
			res, err := f.svc.Search(context.Background(), listings.SearchInput{
				Q: ptr("maize"), Sort: tt.sort, Limit: 50, Page: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			got := ids(res.Items)
			// Only the two maize listings match "maize".
			if strings.Join(got, "|") != strings.Join(tt.want, "|") {
				t.Errorf("%s: got %v, want %v", tt.sort, got, tt.want)
			}
		})
	}

	// Relevance without a query falls back to newest.
	res, err := f.svc.Search(context.Background(), listings.SearchInput{Sort: listings.SortRelevance, Limit: 50, Page: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.Sort != listings.SortNewest {
		t.Errorf("sort = %q, want newest fallback", res.Sort)
	}
	if len(res.Items) != 3 {
		t.Errorf("newest: got %v", ids(res.Items))
	}
}

func TestSearch_ExcludesInactiveAndExpired(t *testing.T) {
	f := newSearchFixture(t)
	ctx := context.Background()
	seller := f.seller
	active := f.active(t, seller, func(i *listings.Input) { i.Title = "Visible Maize" })

	// A draft, a sold, an archived and an expired listing must never show.
	draft, err := f.svc.Create(ctx, seller, validInput(), false)
	if err != nil {
		t.Fatal(err)
	}
	sold := f.active(t, seller, func(i *listings.Input) { i.Title = "Sold Maize" })
	if _, err := f.svc.MarkSold(ctx, seller, sold.ID); err != nil {
		t.Fatal(err)
	}
	archived := f.active(t, seller, func(i *listings.Input) { i.Title = "Archived Maize" })
	if _, err := f.svc.Archive(ctx, seller, archived.ID); err != nil {
		t.Fatal(err)
	}
	overdue := f.active(t, seller, func(i *listings.Input) { i.Title = "Expired Maize" })
	if _, err := f.pool.Exec(ctx,
		`UPDATE listings SET expires_at = $2 WHERE id = $1`, overdue.ID, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	// A suspended listing, set the way Phase 20b will.
	suspended := f.active(t, seller, func(i *listings.Input) { i.Title = "Suspended Maize" })
	if _, err := f.pool.Exec(ctx, `UPDATE listings SET status = 'suspended' WHERE id = $1`, suspended.ID); err != nil {
		t.Fatal(err)
	}

	res, err := f.svc.Search(ctx, listings.SearchInput{Limit: 50, Page: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 1 || res.Items[0].ID != active.ID {
		t.Fatalf("search returned %v (total %d), want only the active listing", ids(res.Items), res.Total)
	}
	// The same holds for the detail endpoint.
	for _, slug := range []string{draft.Slug, sold.Slug, archived.Slug, overdue.Slug, suspended.Slug} {
		if _, err := f.svc.PublicDetail(ctx, slug); err == nil {
			t.Errorf("detail for a %s listing succeeded", slug)
		}
	}
	if _, err := f.svc.PublicDetail(ctx, active.Slug); err != nil {
		t.Errorf("active detail: %v", err)
	}
}

func TestSearch_PromotedFirst(t *testing.T) {
	f := newSearchFixture(t)
	ctx := context.Background()
	seller := f.seller

	plain := f.active(t, seller, func(i *listings.Input) { i.Title = "Plain Maize" })
	vip := f.active(t, seller, func(i *listings.Input) { i.Title = "Vip Maize" })
	enterprise := f.active(t, seller, func(i *listings.Input) { i.Title = "Enterprise Maize" })
	expiredPromo := f.active(t, seller, func(i *listings.Input) { i.Title = "Expired Promo Maize" })

	now := time.Now()
	promo := func(t *testing.T, l listings.View, tier string, rank int, from, to time.Time) {
		t.Helper()
		if _, err := f.pool.Exec(ctx, `INSERT INTO listing_promotions
			(listing_id, seller_id, tier, tier_rank, starts_at, ends_at)
			VALUES ($1, $2, $3, $4, $5, $6)`, l.ID, l.SellerID, tier, rank, from, to); err != nil {
			t.Fatal(err)
		}
	}
	promo(t, vip, "vip", 2, now.Add(-time.Hour), now.Add(time.Hour))
	promo(t, enterprise, "enterprise", 4, now.Add(-time.Hour), now.Add(time.Hour))
	// Already over: must rank as unpromoted (DOMAIN §6).
	promo(t, expiredPromo, "diamond", 3, now.Add(-48*time.Hour), now.Add(-24*time.Hour))

	res, err := f.svc.Search(ctx, listings.SearchInput{Limit: 50, Page: 1})
	if err != nil {
		t.Fatal(err)
	}
	got := ids(res.Items)
	if len(got) != 4 {
		t.Fatalf("items = %v", got)
	}
	if got[0] != "Enterprise Maize" || got[1] != "Vip Maize" {
		t.Errorf("promoted first: %v", got)
	}
	if res.Items[0].Promo == nil || res.Items[0].Promo.Tier != "enterprise" {
		t.Errorf("promo = %+v", res.Items[0].Promo)
	}
	// The expired promotion is reported as no promotion at all.
	for _, item := range res.Items {
		if item.ID == expiredPromo.ID && item.Promo != nil {
			t.Errorf("expired promo reported as %+v", item.Promo)
		}
		if item.ID == plain.ID && item.Promo != nil {
			t.Errorf("unpromoted listing has %+v", item.Promo)
		}
	}
}

func TestSearch_Pagination(t *testing.T) {
	f := newSearchFixture(t)
	seller := f.seller
	for i := range 7 {
		lot := i
		f.active(t, seller, func(in *listings.Input) {
			in.Title = fmt.Sprintf("Maize Lot %d", lot)
		})
	}

	page1, err := f.svc.Search(context.Background(), listings.SearchInput{Limit: 3, Page: 1})
	if err != nil {
		t.Fatal(err)
	}
	page2, err := f.svc.Search(context.Background(), listings.SearchInput{Limit: 3, Page: 2})
	if err != nil {
		t.Fatal(err)
	}
	page3, err := f.svc.Search(context.Background(), listings.SearchInput{Limit: 3, Page: 3})
	if err != nil {
		t.Fatal(err)
	}
	if page1.Total != 7 || page2.Total != 7 {
		t.Errorf("totals = %d, %d (want 7)", page1.Total, page2.Total)
	}
	if len(page1.Items) != 3 || len(page2.Items) != 3 || len(page3.Items) != 1 {
		t.Fatalf("page sizes = %d, %d, %d", len(page1.Items), len(page2.Items), len(page3.Items))
	}
	seen := map[uuid.UUID]bool{}
	for _, page := range []listings.SearchResult{page1, page2, page3} {
		for _, item := range page.Items {
			if seen[item.ID] {
				t.Errorf("listing %s appeared on two pages", item.Title)
			}
			seen[item.ID] = true
		}
	}

	// The service refuses an over-large page even if a caller skips the spec.
	_, err = f.svc.Search(context.Background(), listings.SearchInput{Limit: listings.MaxPageSize + 1, Page: 1})
	var verr *validation.Error
	if !errors.As(err, &verr) {
		t.Errorf("limit 51 = %v, want *validation.Error", err)
	}
	if _, err := f.svc.Search(context.Background(), listings.SearchInput{Limit: 50, Page: 0}); !errors.As(err, &verr) {
		t.Errorf("page 0 = %v, want *validation.Error", err)
	}
}

// TestSearch_UsesIndexes is the "does the browse path scale" guard. It seeds a
// broad active catalogue with a handful of rare terms and then asserts that the
// three production query shapes reach for an index instead of scanning.
// TestSearch_UsesIndexes is the "does browse scale" guard. It seeds a broad
// catalogue with production-shaped rows (wide descriptions, 20k listings) and
// asserts the real query shapes reach for the search indexes instead of
// scanning. Row width matters: a GIN tsvector index only beats a sequential
// scan once the heap is wider than the index, so the fixture has to look like
// real listings rather than stub rows.
func TestSearch_UsesIndexes(t *testing.T) {
	f := newSearchFixture(t)
	ctx := context.Background()

	var categoryID uuid.UUID
	if err := f.pool.QueryRow(ctx,
		`SELECT id FROM categories WHERE parent_id IS NOT NULL LIMIT 1`).Scan(&categoryID); err != nil {
		t.Fatal(err)
	}
	// body approximates a typical seller description: the schema allows 20 to
	// 5000 characters and real listings sit in the middle of that range, so
	// the heap is much wider than the tsvector index.
	const body = "Ghanaian farm produce of the highest grade, carefully sorted, bagged and ready for collection from the farm gate. "
	full := strings.Repeat(body, 5)
	if _, err := f.pool.Exec(ctx, `INSERT INTO listings
		(seller_id, category_id, title, slug, description, price_pesewas, unit,
		 quantity_available, min_order_qty, is_negotiable, item_state, status,
		 region, district, offers_pickup, offers_seller_delivery,
		 published_at, expires_at, created_at)
		SELECT $1, $2,
			'Bulk consignment ' || g,
			'bulk-consignment-' || g,
			'Produce shipment ' || g || ' ' || $3,
			10000 + g, 'kg', 500, 1, true, 'grade_a', 'active',
			CASE WHEN g <= 2 THEN 'Ashanti' ELSE 'Greater Accra' END,
			CASE WHEN g <= 2 THEN 'Kumasi Metro' ELSE 'Accra Metropolitan' END,
			true, false,
			now() - (g || ' minutes')::interval,
			now() + interval '30 days',
			now() - (g || ' minutes')::interval
		FROM generate_series(1, 20000) g`, f.seller, categoryID, full); err != nil {
		t.Fatal(err)
	}
	// Two rows carry a term nobody else has, so a search for it is highly
	// selective against a large active table.
	if _, err := f.pool.Exec(ctx, `UPDATE listings SET description = $1
		WHERE slug IN ('bulk-consignment-1', 'bulk-consignment-2')`,
		"Sorghum shipment, white grain, available now. "+full); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `ANALYZE listings`); err != nil {
		t.Fatal(err)
	}

	// Known limitation, recorded in docs/reviews/phase-12.md: a long fuzzy
	// string ("Bulk consignmnt 5") gets a ~1.0 selectivity estimate for
	// `title % $1`, so the planner scans instead of using the trigram index.
	// Short queries do use it, which is why the free-text case above expects
	// both indexes. Fixing it means giving the trigram branch a bounded
	// estimate, tracked in the phase 12 backlog row.

	// plan runs EXPLAIN (FORMAT JSON) and returns the index names and node
	// types the plan touches.
	plan := func(t *testing.T, query string, args ...any) (indexes, nodes []string) {
		t.Helper()
		var raw []byte
		if err := f.pool.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+query, args...).Scan(&raw); err != nil {
			t.Fatalf("explain: %v", err)
		}
		var out []struct {
			Plan map[string]any `json:"Plan"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("plan json: %v (%s)", err, raw)
		}
		var walk func(map[string]any)
		walk = func(node map[string]any) {
			if name, ok := node["Node Type"].(string); ok {
				nodes = append(nodes, name)
			}
			if idx, ok := node["Index Name"].(string); ok {
				indexes = append(indexes, idx)
			}
			children, _ := node["Plans"].([]any)
			for _, child := range children {
				if m, ok := child.(map[string]any); ok {
					walk(m)
				}
			}
		}
		walk(out[0].Plan)
		return indexes, nodes
	}

	for _, tt := range []struct {
		name   string
		query  string
		args   []any
		wantIn []string
	}{
		{
			// The production free-text predicate, verbatim: the tsvector
			// branch and the trigram branch combine into a BitmapOr.
			name: "free text",
			query: `SELECT id FROM listings WHERE status = 'active' AND expires_at > $1
			        AND (search_vector @@ websearch_to_tsquery('simple', $2) OR title % $2)`,
			args:   []any{time.Now(), "sorghum"},
			wantIn: []string{"listings_search_idx", "listings_title_trgm_idx"},
		},
		{
			name:   "region filter",
			query:  `SELECT id FROM listings WHERE status = 'active' AND expires_at > $1 AND region = $2`,
			args:   []any{time.Now(), "Ashanti"},
			wantIn: []string{"listings_region_idx"},
		},
		{
			name: "newest browse",
			query: `SELECT id FROM listings WHERE status = 'active' AND expires_at > $1
			        ORDER BY published_at DESC LIMIT 20`,
			args:   []any{time.Now()},
			wantIn: []string{"listings_published_idx"},
		},
		{
			name: "price sort",
			query: `SELECT id FROM listings WHERE status = 'active' AND expires_at > $1 AND region = $2
			        ORDER BY price_pesewas ASC LIMIT 20`,
			args:   []any{time.Now(), "Ashanti"},
			wantIn: []string{"listings_region_idx"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			indexes, nodes := plan(t, tt.query, tt.args...)
			if containsAny(nodes, "Seq Scan") {
				t.Errorf("plan fell back to a sequential scan: nodes %v", nodes)
			}
			for _, want := range tt.wantIn {
				if !containsAny(indexes, want) {
					t.Errorf("plan used %v, want %s (nodes %v)", indexes, want, nodes)
				}
			}
		})
	}
}

func containsAny(hay []string, want string) bool {
	for _, h := range hay {
		if h == want {
			return true
		}
	}
	return false
}

func TestPublicDetail(t *testing.T) {
	f := newSearchFixture(t)
	ctx := context.Background()
	seller := f.seller

	in := validInput()
	in.Title = "Healthy Friesian Heifer"
	in.Area = ptr("Adom farm")
	in.Attributes = map[string]string{"breed": "Friesian", "vaccinated": "true"}
	view, err := f.svc.Create(ctx, seller, in, false)
	if err != nil {
		t.Fatal(err)
	}
	first := f.upload(t, seller, "image/jpeg", 64)
	second := f.upload(t, seller, "image/jpeg", 64)
	if _, err := f.svc.Update(ctx, seller, view.ID, listings.Patch{
		ImageMediaIDs: []uuid.UUID{first, second},
	}); err != nil {
		t.Fatal(err)
	}
	view, err = f.svc.Publish(ctx, seller, view.ID)
	if err != nil {
		t.Fatal(err)
	}

	d, err := f.svc.PublicDetail(ctx, view.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if d.ID != view.ID || d.Slug != view.Slug {
		t.Errorf("id/slug = %s/%s, want %s/%s", d.ID, d.Slug, view.ID, view.Slug)
	}
	if d.Title != in.Title || d.Description != in.Description {
		t.Errorf("title/description mismatch: %q / %q", d.Title, d.Description)
	}
	if d.PricePesewas != in.PricePesewas || d.QuantityAvailable != in.QuantityAvailable ||
		d.MinOrderQty != in.MinOrderQty || !d.IsNegotiable {
		t.Errorf("commercial fields = %+v", d.PublicSummary)
	}
	if d.Area == nil || *d.Area != "Adom farm" {
		t.Errorf("area = %v", d.Area)
	}
	if !d.OffersPickup || !d.OffersSellerDelivery || d.SellerDeliveryFee == nil ||
		*d.SellerDeliveryFee != *in.Delivery.FeePesewas {
		t.Errorf("delivery = %v/%v/%v", d.OffersPickup, d.OffersSellerDelivery, d.SellerDeliveryFee)
	}
	if d.ExpiresAt == nil {
		t.Error("expiresAt = nil, want the active expiry")
	}
	if len(d.Images) != 2 {
		t.Fatalf("images = %d, want 2", len(d.Images))
	}
	if d.Images[0].MediaID != first || d.Images[1].MediaID != second {
		t.Errorf("image order = %s, %s", d.Images[0].MediaID, d.Images[1].MediaID)
	}
	if d.Images[0].URL == "" || d.Images[0].Order != 0 || d.Images[1].Order != 1 {
		t.Errorf("images = %+v", d.Images)
	}
	// The cover is the first image, exposed as a public URL, never a storage key.
	if d.CoverURL == nil || *d.CoverURL != d.Images[0].URL {
		t.Errorf("coverURL = %v, want the first image url %q", d.CoverURL, d.Images[0].URL)
	}
	// Attribute values carry the category's display label, not the raw key.
	if len(d.Attributes) != 2 {
		t.Fatalf("attributes = %+v", d.Attributes)
	}
	byKey := map[string]listings.PublicAttribute{}
	for _, a := range d.Attributes {
		byKey[a.Key] = a
	}
	// The label comes from the category definition ("Breed"), not the raw key.
	if got := byKey["breed"]; got.Value != "Friesian" || got.Label != "Breed" {
		t.Errorf("breed attribute = %+v, want value Friesian and label Breed", got)
	}
	if got := byKey["vaccinated"]; got.Value != "true" || got.Label != "Vaccinated" {
		t.Errorf("vaccinated attribute = %+v", got)
	}
	// The seller projection is the safe one: a name and a place, nothing else.
	if d.SellerPublic.UserID != seller || d.SellerPublic.Name == "" ||
		d.SellerPublic.Region != "Ashanti" || d.SellerPublic.District != "Kumasi Metro" {
		t.Errorf("seller = %+v", d.SellerPublic)
	}
	if d.SellerPublic.Verified {
		t.Error("seller reported verified without a verification record")
	}
	if d.SellerPublic.MemberSince.IsZero() {
		t.Error("memberSince = zero")
	}

	// An unknown slug is a 404, not an empty page.
	if _, err := f.svc.PublicDetail(ctx, "no-such-listing"); !errors.Is(err, listings.ErrNotFound) {
		t.Errorf("unknown slug = %v, want ErrNotFound", err)
	}
}

func TestContact(t *testing.T) {
	f := newSearchFixture(t)
	ctx := context.Background()
	seller, buyer := f.seller, f.other

	view := f.active(t, seller, nil)

	// The seller's own contact details, and their opt-ins.
	const phone, whatsapp = "+233201234567", "+233209876543"
	if _, err := f.pool.Exec(ctx, `UPDATE users SET phone_e164 = $2 WHERE id = $1`, seller, phone); err != nil {
		t.Fatal(err)
	}
	optIn := func(t *testing.T, showPhone, showWhatsapp bool, wa *string) {
		t.Helper()
		if _, err := f.pool.Exec(ctx, `UPDATE seller_profiles
			SET show_phone = $2, show_whatsapp = $3, whatsapp_e164 = $4 WHERE user_id = $1`,
			seller, showPhone, showWhatsapp, wa); err != nil {
			t.Fatal(err)
		}
	}

	// Your own listing is refused, so the endpoint cannot be used to read back
	// your own number through an odd path.
	if _, err := f.svc.Contact(ctx, seller, view.ID); !errors.Is(err, listings.ErrOwnListing) {
		t.Errorf("own listing = %v, want ErrOwnListing", err)
	}
	if _, err := f.svc.Contact(ctx, buyer, uuid.New()); !errors.Is(err, listings.ErrNotFound) {
		t.Errorf("unknown listing = %v, want ErrNotFound", err)
	}

	countOf := func(t *testing.T) int32 {
		t.Helper()
		var n int32
		if err := f.pool.QueryRow(ctx,
			`SELECT contact_count FROM listings WHERE id = $1`, view.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("both opted in", func(t *testing.T) {
		wa := whatsapp
		optIn(t, true, true, &wa)
		got, err := f.svc.Contact(ctx, buyer, view.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Phone == nil || *got.Phone != phone || got.Whatsapp == nil || *got.Whatsapp != whatsapp {
			t.Errorf("contact = %+v, want both numbers", got)
		}
		if n := countOf(t); n != 1 {
			t.Errorf("contact_count = %d, want 1", n)
		}
	})

	t.Run("phone only", func(t *testing.T) {
		wa := whatsapp
		optIn(t, true, false, &wa)
		got, err := f.svc.Contact(ctx, buyer, view.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Phone == nil || *got.Phone != phone {
			t.Errorf("phone = %v, want %s", got.Phone, phone)
		}
		if got.Whatsapp != nil {
			t.Errorf("whatsapp = %v, want withheld", *got.Whatsapp)
		}
	})

	t.Run("neither opted in", func(t *testing.T) {
		optIn(t, false, false, nil)
		got, err := f.svc.Contact(ctx, buyer, view.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Phone != nil || got.Whatsapp != nil {
			t.Errorf("contact = %+v, want nothing revealed", got)
		}
		// The reveal was still counted: the seller asked for it.
		if n := countOf(t); n != 3 {
			t.Errorf("contact_count = %d, want 3", n)
		}
	})

	// A listing that is not browsable is a 404 even for a valid buyer.
	for _, status := range []string{"sold", "archived", "suspended"} {
		other := f.active(t, seller, func(i *listings.Input) { i.Title = "Hidden " + status })
		if _, err := f.pool.Exec(ctx,
			`UPDATE listings SET status = $2 WHERE id = $1`, other.ID, status); err != nil {
			t.Fatal(err)
		}
		if _, err := f.svc.Contact(ctx, buyer, other.ID); !errors.Is(err, listings.ErrListingNotActive) {
			t.Errorf("%s listing = %v, want ErrListingNotActive", status, err)
		}
	}
	expired := f.active(t, seller, func(i *listings.Input) { i.Title = "Expired Contact" })
	if _, err := f.pool.Exec(ctx, `UPDATE listings SET expires_at = $2 WHERE id = $1`,
		expired.ID, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Contact(ctx, buyer, expired.ID); !errors.Is(err, listings.ErrListingNotActive) {
		t.Errorf("expired listing = %v, want ErrListingNotActive", err)
	}
}

func TestCountView(t *testing.T) {
	f := newSearchFixture(t)
	ctx := context.Background()
	seller := f.seller
	view := f.active(t, seller, nil)
	other := f.active(t, seller, func(i *listings.Input) { i.Title = "Second Lot" })

	views := func(t *testing.T, id uuid.UUID) int32 {
		t.Helper()
		var n int32
		if err := f.pool.QueryRow(ctx,
			`SELECT view_count FROM listings WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	for range 3 {
		if err := f.svc.CountView(ctx, view.ID); err != nil {
			t.Fatal(err)
		}
	}
	if n := views(t, view.ID); n != 3 {
		t.Errorf("view_count = %d, want 3", n)
	}
	if n := views(t, other.ID); n != 0 {
		t.Errorf("other listing view_count = %d, want 0", n)
	}
	// The counter is a counter: repeating the same viewer is the job's
	// problem, not an error here.
	if err := f.svc.CountView(ctx, view.ID); err != nil {
		t.Fatal(err)
	}
	if n := views(t, view.ID); n != 4 {
		t.Errorf("view_count = %d, want 4", n)
	}
}
