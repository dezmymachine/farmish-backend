// Package redact masks personal data and secrets before they reach a log.
//
// The string helpers (Phone, AccountNumber, Email) keep just enough to tell
// two values apart while debugging. Handler wraps a slog.Handler so the
// guarantee holds even when a call site forgets to mask.
package redact

import (
	"regexp"
	"strings"
)

// Placeholder replaces values that must never be logged, even in part.
const Placeholder = "[redacted]"

// JWTPlaceholder replaces anything that looks like a JWT.
const JWTPlaceholder = "[redacted-jwt]"

// Phone hides the middle digits: +233241234567 becomes +233*****4567.
// Input too short to mask meaningfully returns a fixed mask.
func Phone(s string) string {
	if len(s) < 8 {
		return "****"
	}
	return s[:4] + "*****" + s[len(s)-4:]
}

// AccountNumber keeps only the last four characters: 0123456789 becomes
// ******6789. Input of four characters or fewer is fully masked.
func AccountNumber(s string) string {
	if len(s) <= 4 {
		return strings.Repeat("*", len(s))
	}
	return strings.Repeat("*", len(s)-4) + s[len(s)-4:]
}

// Email keeps the first character of the local part and the domain:
// ama.mensah@example.com becomes a***@example.com. Input without an "@"
// is fully masked.
func Email(s string) string {
	at := strings.LastIndexByte(s, '@')
	if at <= 0 {
		return Placeholder
	}
	return s[:1] + "***" + s[at:]
}

var (
	// e164GH matches a Ghana E.164 number, the only phone shape the API stores.
	e164GH = regexp.MustCompile(`\+233[0-9]{9}`)
	// jwt matches three base64url segments, the first starting "eyJ" (a JSON
	// header), which is how every Firebase ID token and custom token begins.
	jwt = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*`)
	// bearer matches an Authorization value.
	bearer = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]+`)
)

// String masks secrets and phone numbers found inside free text: JWTs,
// bearer credentials and Ghana E.164 numbers. It is the value-pattern pass
// Handler applies to every string attribute.
func String(s string) string {
	if s == "" {
		return s
	}
	if strings.Contains(s, "eyJ") {
		s = jwt.ReplaceAllString(s, JWTPlaceholder)
	}
	if strings.Contains(s, "+233") {
		s = e164GH.ReplaceAllStringFunc(s, Phone)
	}
	if len(s) > 6 && strings.Contains(strings.ToLower(s), "bearer") {
		s = bearer.ReplaceAllString(s, "Bearer "+Placeholder)
	}
	return s
}
