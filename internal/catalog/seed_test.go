package catalog_test

import (
	"context"
	"testing"

	"github.com/dezmymachine/farmish-backend/internal/catalog"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/db"
)

// wantAttrs maps parent slug to its DOMAIN §9 attribute count.
var wantAttrs = map[string]int{
	"seeds-seedlings": 3, "fertilizers-chemicals": 3, "farm-machinery": 3,
	"livestock-poultry": 3, "feeds-supplements": 2, "fresh-produce": 3,
	"processed-foods": 3, "farm-labour": 3, "land-leasing": 3,
	"storage-packaging": 2, "veterinary": 2, "irrigation": 2,
}

// TestSeed_MatchesDomainAndIsIdempotent proves the seed ports DOMAIN §9
// exactly (12 parents, 72 children, per-parent attributes) and that a second
// run changes nothing, not even updated_at.
func TestSeed_MatchesDomainAndIsIdempotent(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	if err := catalog.Seed(ctx, pool); err != nil {
		t.Fatal(err)
	}
	q := db.New(pool)

	var parents, children int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM categories WHERE parent_id IS NULL`).Scan(&parents); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM categories WHERE parent_id IS NOT NULL`).Scan(&children); err != nil {
		t.Fatal(err)
	}
	if parents != 12 || children != 72 {
		t.Fatalf("parents = %d, children = %d (want 12 and 72)", parents, children)
	}
	for slug, want := range wantAttrs {
		c, err := q.GetCategoryBySlug(ctx, slug)
		if err != nil {
			t.Fatalf("%s: %v", slug, err)
		}
		attrs, err := q.ListAttributesByCategory(ctx, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(attrs) != want {
			t.Errorf("%s: %d attributes, want %d", slug, len(attrs), want)
		}
	}
	// Spot-checks: fixed slugs, prefixed child slug, groups, required key.
	land, err := q.GetCategoryBySlug(ctx, "land-leasing")
	if err != nil || land.ListingGroup == nil || *land.ListingGroup != "land" {
		t.Errorf("land-leasing = %+v, %v", land, err)
	}
	attrs, _ := q.ListAttributesByCategory(ctx, land.ID)
	found := false
	for _, a := range attrs {
		if a.Key == "land_size_acres" && a.Required {
			found = true
		}
	}
	if !found {
		t.Error("land_size_acres required attribute missing")
	}
	flower, err := q.GetCategoryBySlug(ctx, "seeds-seedlings-flowers-ornamentals")
	if err != nil {
		t.Fatalf("child slug: %v", err)
	}
	parent, err := q.GetCategoryByID(ctx, flower.ParentID.Bytes)
	if err != nil || parent.Slug != "seeds-seedlings" {
		t.Errorf("child parent = %+v, %v", parent, err)
	}

	var before string
	if err := pool.QueryRow(ctx, `SELECT max(updated_at)::text FROM categories`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Seed(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var after string
	var nParents, nChildren, nAttrs int
	if err := pool.QueryRow(ctx, `SELECT max(updated_at)::text FROM categories`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Errorf("second seed touched updated_at: %s -> %s", before, after)
	}
	_ = pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM categories WHERE parent_id IS NULL),
		(SELECT count(*) FROM categories WHERE parent_id IS NOT NULL),
		(SELECT count(*) FROM category_attributes)`).Scan(&nParents, &nChildren, &nAttrs)
	if nParents != 12 || nChildren != 72 || nAttrs != 32 {
		t.Errorf("after reseed: parents=%d children=%d attrs=%d", nParents, nChildren, nAttrs)
	}
}

// TestChildAttributes_OverrideAndMerge proves admin attributes on a child
// merge with the parent's: same keys override, new keys append.
func TestChildAttributes_OverrideAndMerge(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	if err := catalog.Seed(ctx, pool); err != nil {
		t.Fatal(err)
	}
	s := catalog.New(pool)
	child, err := s.Detail(ctx, "livestock-poultry-cattle")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddAttribute(ctx, child.ID, catalog.AttributeInput{
		Key: "breed", Label: "Breed (local)", Type: "text",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddAttribute(ctx, child.ID, catalog.AttributeInput{
		Key: "horn_status", Label: "Horn Status", Type: "select", Options: []string{"Horned", "Polled"},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Detail(ctx, "livestock-poultry-cattle")
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]catalog.Attribute{}
	for _, a := range got.Attributes {
		byKey[a.Key] = a
	}
	if byKey["breed"].Label != "Breed (local)" {
		t.Errorf("breed not overridden: %+v", byKey["breed"])
	}
	if byKey["horn_status"].Label != "Horn Status" || byKey["vaccinated"].Label != "Vaccinated" {
		t.Errorf("merged attributes = %+v", got.Attributes)
	}
	if len(got.Attributes) != 4 {
		t.Errorf("len(attributes) = %d, want 3 parent + 1 new", len(got.Attributes))
	}
}
