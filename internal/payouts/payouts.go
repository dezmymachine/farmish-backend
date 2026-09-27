// Package payouts owns seller payout accounts (Phase 18a): the Paystack-
// resolved, name-checked, encrypted account a seller's earnings go to.
// Payout execution itself is Phase 18b.
package payouts

import (
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

// Account types stored in seller_payout_accounts.type. They match Paystack's
// ListBanks types.
const (
	TypeMobileMoney = "mobile_money"
	TypeGhipss      = "ghipss"
)

// Account statuses stored in seller_payout_accounts.status. Payouts only go
// to verified accounts; an admin approves needs_review ones.
const (
	StatusVerified    = "verified"
	StatusNeedsReview = "needs_review"
)

// CooldownAfterChange is how long after an account change no payout may run
// to the new account (DOMAIN §3: anti-takeover).
const CooldownAfterChange = 48 * time.Hour

var (
	// ErrNotFound means the seller never set a payout account.
	ErrNotFound = errors.New("payout account not found")
	// ErrSellerProfileRequired means the caller has no seller profile.
	ErrSellerProfileRequired = errors.New("seller profile is required")
	// ErrUnresolvable means Paystack could not resolve the account.
	ErrUnresolvable = errors.New("payout account could not be resolved")
	// ErrAlreadyVerified means an approve found nothing waiting for review.
	ErrAlreadyVerified = errors.New("payout account is already verified")
)

// validTypes mirrors the seller_payout_accounts CHECK.
var validTypes = map[string]bool{TypeMobileMoney: true, TypeGhipss: true}

// Account is a seller's payout account. The number never appears here: only
// its mask leaves the service.
type Account struct {
	SellerID      uuid.UUID
	Type          string
	BankCode      string
	BankName      string
	NumberMask    string
	AccountName   string
	RecipientCode string
	Status        string
	VerifiedAt    *time.Time
	CooldownUntil *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Input is a seller's raw payout details.
type Input struct {
	Type          string
	BankCode      string
	AccountNumber string
}

// recipientType maps our account type onto Paystack's transfer recipient
// types ("basilisk" is Paystack's bank-account type).
func recipientType(accountType string) string {
	if accountType == TypeGhipss {
		return "basilisk"
	}
	return TypeMobileMoney
}

// NormalizeNumber strips separators and unifies Ghanaian prefixes to the
// local format Paystack resolves (0XXXXXXXXX for MoMo). It reports whether
// the result is well-formed for the type.
func NormalizeNumber(accountType, raw string) (string, bool) {
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		if r == '+' {
			return -1
		}
		if unicode.IsSpace(r) || r == '-' {
			return -1
		}
		return r
	}, raw)
	// Any other character (letters, slashes) survives the map: reject it.
	for _, r := range digits {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	if accountType == TypeMobileMoney {
		digits = strings.TrimPrefix(strings.TrimPrefix(digits, "233"), "0")
		digits = "0" + digits
		if len(digits) != 10 || digits[0] != '0' {
			return "", false
		}
		return digits, true
	}
	if len(digits) < 10 || len(digits) > 20 {
		return "", false
	}
	return digits, true
}

// Mask returns the number as it may leave the service: stars plus the last 4
// (e.g. 0241234567 becomes ******4567).
func Mask(normalized string) string {
	if len(normalized) <= 4 {
		return "****"
	}
	return strings.Repeat("*", len(normalized)-4) + normalized[len(normalized)-4:]
}

// NameMatches reports whether the Paystack-resolved holder name plausibly
// belongs to the seller: after uppercasing, punctuation stripping and token
// splitting, it shares at least one token of at least 3 characters with the
// seller's business name or display name.
func NameMatches(resolved, businessName, displayName string) bool {
	got := tokens(resolved)
	if len(got) == 0 {
		return false
	}
	for _, want := range append(tokens(businessName), tokens(displayName)...) {
		if len(want) < 3 {
			continue
		}
		for _, have := range got {
			if have == want {
				return true
			}
		}
	}
	return false
}

// tokens uppercases a name, turns every non-letter/non-digit into a space,
// and splits it.
func tokens(name string) []string {
	cleaned := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToUpper(r)
		}
		return ' '
	}, name)
	return strings.Fields(cleaned)
}
