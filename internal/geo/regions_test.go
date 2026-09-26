package geo

import "testing"

func TestIsRegion(t *testing.T) {
	if len(Regions) != 16 {
		t.Fatalf("len(Regions) = %d, want 16", len(Regions))
	}
	for _, r := range Regions {
		if !IsRegion(r) {
			t.Errorf("IsRegion(%q) = false", r)
		}
	}
	for _, s := range []string{"Accra", "", "ashanti", "ASHANTI", "Greater accra", " Volta"} {
		if IsRegion(s) {
			t.Errorf("IsRegion(%q) = true, want false", s)
		}
	}
}
