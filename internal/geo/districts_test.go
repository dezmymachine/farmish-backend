package geo

import (
	"sort"
	"testing"
)

func TestRegionDistricts(t *testing.T) {
	if len(RegionDistricts) != 16 {
		t.Fatalf("len(RegionDistricts) = %d, want 16", len(RegionDistricts))
	}
	// Exactly the 16 DOMAIN regions, sorted by name.
	names := make([]string, 0, len(RegionDistricts))
	seen := map[string]bool{}
	for _, rd := range RegionDistricts {
		names = append(names, rd.Name)
		seen[rd.Name] = true
		if len(rd.Districts) == 0 {
			t.Errorf("%s has no districts", rd.Name)
		}
		dupes := map[string]bool{}
		for _, d := range rd.Districts {
			if dupes[d] {
				t.Errorf("%s lists %q twice", rd.Name, d)
			}
			dupes[d] = true
		}
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("regions not sorted by name: %v", names)
	}
	for _, r := range Regions {
		if !seen[r] {
			t.Errorf("region %q missing from districts", r)
		}
	}
	// Spot-checks from the legacy list, including a shared district.
	byName := map[string][]string{}
	for _, rd := range RegionDistricts {
		byName[rd.Name] = rd.Districts
	}
	if len(byName["Greater Accra"]) == 0 {
		t.Error("Greater Accra has no districts")
	}
	for _, region := range []string{"Volta", "Oti"} {
		found := false
		for _, d := range byName[region] {
			if d == "Krachi East" {
				found = true
			}
		}
		if !found {
			t.Errorf("Krachi East missing from %s (legacy lists it under both)", region)
		}
	}
}
