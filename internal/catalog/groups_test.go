package catalog

import (
	"slices"
	"testing"
)

func TestGroups(t *testing.T) {
	if len(Groups) != 5 {
		t.Fatalf("len(Groups) = %d, want 5", len(Groups))
	}
	for _, g := range []string{"equipment", "quality", "livestock", "land", "service"} {
		if !ValidGroup(g) {
			t.Errorf("ValidGroup(%q) = false", g)
		}
	}
	if ValidGroup("vehicles") {
		t.Error("ValidGroup(vehicles) = true")
	}
	if g := Groups["livestock"]; !slices.Contains(g.Units, "heads") || !slices.Contains(g.ItemStates, "breeding_stock") {
		t.Errorf("livestock group = %+v", g)
	}
	if g := Groups["quality"]; !slices.Contains(g.Units, "kg") || !slices.Contains(g.Units, "pieces") {
		t.Errorf("quality group = %+v", g)
	}
}
