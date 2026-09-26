package catalog

// GroupInfo is what GET /v1/categories/{slug} reports for a category's
// listing_group: the allowed item states and units (DOMAIN §8).
type GroupInfo struct {
	ItemStates []string
	Units      []string
}

// Groups maps each parent-held listing_group to its states and units, in
// DOMAIN §8 order. Children inherit their parent's group.
var Groups = map[string]GroupInfo{
	"equipment": {
		ItemStates: []string{"brand_new", "used", "refurbished"},
		Units:      []string{"pieces", "units", "crates", "baskets", "bundles", "dozen", "pack", "box"},
	},
	"quality": {
		ItemStates: []string{"grade_a", "grade_b", "organic", "premium", "standard"},
		Units: []string{
			"kg", "grams", "metric_ton", "pounds", "bags_50kg", "bags_25kg",
			"pieces", "units", "crates", "baskets", "bundles", "dozen", "pack", "box",
		},
	},
	"livestock": {
		ItemStates: []string{"young", "adult", "mature", "breeding_stock"},
		Units:      []string{"pieces", "units", "crates", "baskets", "bundles", "dozen", "pack", "box", "heads"},
	},
	"land": {
		ItemStates: []string{"developed", "partially_developed", "undeveloped"},
		Units:      []string{"acres", "hectares", "square_meters"},
	},
	"service": {
		ItemStates: []string{"experienced", "certified", "trained"},
		Units:      []string{"hour", "day", "week", "month"},
	},
}

// ValidGroup reports whether group is one of the five listing groups.
func ValidGroup(group string) bool {
	_, ok := Groups[group]
	return ok
}
