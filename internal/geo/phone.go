// Package geo holds Ghana reference data: regions, district suggestions
// (Phase 9) and phone helpers shared by auth, profiles and notifications.
package geo

import (
	"errors"
	"regexp"
	"strings"
)

// ErrInvalidPhone is returned when input is not a Ghanaian E.164 number.
var ErrInvalidPhone = errors.New("invalid Ghana phone number")

// ghanaE164 is +233 followed by exactly 9 digits (DOMAIN §10).
var ghanaE164 = regexp.MustCompile(`^\+233[0-9]{9}$`)

// NormalizeGhanaPhone normalises user input to E.164: it strips spaces,
// dashes and parentheses, turns a leading 0 or 233 into +233, and validates
// ^\+233[0-9]{9}$. Firebase phone sign-in already yields E.164, so that
// passes through unchanged.
func NormalizeGhanaPhone(s string) (string, error) {
	cleaned := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '-', '(', ')':
			return -1
		default:
			return r
		}
	}, s)
	switch {
	case strings.HasPrefix(cleaned, "0"):
		cleaned = "+233" + cleaned[1:]
	case strings.HasPrefix(cleaned, "233"):
		cleaned = "+" + cleaned
	}
	if !ghanaE164.MatchString(cleaned) {
		return "", ErrInvalidPhone
	}
	return cleaned, nil
}

// MaskPhone hides the middle digits for logs: +233241234567 becomes
// +233*****4567. It never panics; short input returns a fixed mask.
func MaskPhone(e164 string) string {
	if len(e164) < 8 {
		return "****"
	}
	return e164[:4] + "*****" + e164[len(e164)-4:]
}
