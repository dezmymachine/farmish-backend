package text

import (
	"strings"
	"testing"
)

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{
		"Flowers & Ornamentals": "flowers-ornamentals",
		"Sheep & Goats":         "sheep-goats",
		"Café":                  "cafe",
		"Tractors":              "tractors",
		"  Hello World  ":       "hello-world",
		"--Dried & Smoked Fish--": "dried-smoked-fish",
		"Sefwi-Bibiani":           "sefwi-bibiani",
	} {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
	long := Slugify(strings.Repeat("a", 100) + " " + strings.Repeat("b", 10))
	if len(long) > 80 || strings.HasPrefix(long, "-") || strings.HasSuffix(long, "-") {
		t.Errorf("long slug = %q", long)
	}
	if got := Slugify(""); got != "" {
		t.Errorf("Slugify(empty) = %q", got)
	}
}
