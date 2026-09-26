package geo

// Regions are exactly the 16 Ghanaian regions (DOMAIN §10). Stored as text
// and validated with IsRegion; matching is case-sensitive and exact.
var Regions = []string{
	"Ahafo",
	"Ashanti",
	"Bono",
	"Bono East",
	"Central",
	"Eastern",
	"Greater Accra",
	"North East",
	"Northern",
	"Oti",
	"Savannah",
	"Upper East",
	"Upper West",
	"Volta",
	"Western",
	"Western North",
}

var regionSet = func() map[string]struct{} {
	m := make(map[string]struct{}, len(Regions))
	for _, r := range Regions {
		m[r] = struct{}{}
	}
	return m
}()

// IsRegion reports whether s is one of the 16 regions, exact match.
func IsRegion(s string) bool {
	_, ok := regionSet[s]
	return ok
}
