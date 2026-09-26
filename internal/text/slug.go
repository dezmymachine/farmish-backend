// Package text holds shared string helpers: slugs for categories and
// listings (DOMAIN §7).
package text

import (
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// nonAlnum collapses every run of non-ASCII-alphanumerics into one dash.
var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// Slugify lowercases, folds accents to ASCII, turns non-alphanumerics into
// dashes and trims them, capped at 80 characters. "&" disappears (the legacy
// app kept it, producing broken slugs): "Flowers & Ornamentals" becomes
// "flowers-ornamentals".
func Slugify(s string) string {
	slug := nonAlnum.ReplaceAllString(strings.ToLower(stripMarks(s)), "-")
	slug = strings.Trim(slug, "-")
	if len(slug) > 80 {
		slug = strings.Trim(slug[:80], "-")
	}
	return slug
}

// stripMarks decomposes accents (NFD) and drops the non-spacing marks, so
// "Café" folds to "Cafe" before lowercasing.
func stripMarks(s string) string {
	out, _, _ := transform.String(transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC), s)
	return out
}
