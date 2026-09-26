package payments

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/dezmymachine/farmish-backend/internal/db"
	"github.com/dezmymachine/farmish-backend/internal/money"
)

// Purposes a payment can be for. Each has a handler registered in the purpose
// registry: 'promotion' lands in Phase 14, 'checkout' in Phase 15b.
const (
	PurposePromotion = "promotion"
	PurposeCheckout  = "checkout"
)

// Payment statuses, mirroring what Paystack reports.
const (
	StatusPending   = "pending"
	StatusSuccess   = "success"
	StatusFailed    = "failed"
	StatusAbandoned = "abandoned"
)

// Provider event types Farmish handles.
const (
	EventChargeSuccess = "charge.success"
	EventChargeFailed  = "charge.failed"
)

// Webhook outcomes recorded in webhook_events.outcome. A rejection is a
// business decision (the money does not match what we expected); only an
// unhandled error rolls the transaction back and asks Paystack to retry.
const (
	OutcomeProcessed = "processed"
	OutcomeIgnored   = "ignored"

	OutcomeUnknownReference = "rejected:unknown_reference"
	OutcomeAmountMismatch   = "rejected:amount_mismatch"
	OutcomeCurrencyMismatch = "rejected:currency_mismatch"
)

// ErrNotFound is returned for a reference that does not exist, or belongs to
// someone else. The two cases are deliberately indistinguishable: a 404 must
// not reveal that a reference is real.
var ErrNotFound = errors.New("payment not found")

// ErrInvalidInput is a validation failure the caller can fix.
var ErrInvalidInput = errors.New("invalid payment input")

// ErrAmountOutOfRange is a gross-up that could not be represented.
var ErrAmountOutOfRange = errors.New("payment amount out of range")

// verifyFallbackDelay is how long a pending payment must be before the verify
// fallback asks Paystack about it. Below that, the buyer has not even reached
// Paystack yet, and a verify call would be wasted.
const verifyFallbackDelay = 10 * time.Second

// Payment is one charge, as this package exposes it. Every amount is pesewas.
type Payment struct {
	ID          uuid.UUID
	Reference   string
	UserID      uuid.UUID
	Purpose     string
	PurposeRef  string
	Base        int64
	Fee         int64
	Charge      int64
	Currency    string
	Status      string
	ProviderFee *int64
	Channel     *string
	// AuthorizationURL is where the buyer goes to pay. It is set after the
	// provider call, so it is empty for a moment after Initialize.
	AuthorizationURL *string
	PaidAt           *time.Time
	FailureReason    *string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Succeeded reports whether the money arrived.
func (p Payment) Succeeded() bool { return p.Status == StatusSuccess }

// Settled reports whether the payment has reached a final state.
func (p Payment) Settled() bool {
	return p.Status == StatusSuccess || p.Status == StatusFailed || p.Status == StatusAbandoned
}

// fromRow maps the generated row onto the domain type.
func fromRow(r db.Payment) Payment {
	return Payment{
		ID: r.ID, Reference: r.Reference, UserID: r.UserID,
		Purpose: r.Purpose, PurposeRef: r.PurposeRef,
		Base: r.BasePesewas, Fee: r.ProcessingFeePesewas, Charge: r.ChargePesewas,
		Currency: r.Currency, Status: r.Status,
		ProviderFee: r.PaystackFeePesewas, Channel: r.Channel,
		AuthorizationURL: r.AuthorizationUrl, PaidAt: r.PaidAt,
		FailureReason: r.FailureReason, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

// newReference returns the reference for a new payment: 'FMS-' followed by 20
// base32 characters, which is 100 bits of entropy from crypto/rand. It is
// generated server-side and never reused.
func newReference() (string, error) {
	// 20 base32 characters carry 100 bits; 13 bytes is 104, masked to 100.
	var buf [13]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate payment reference: %w", err)
	}
	buf[12] &= 0x0f // keep the encoding inside 20 characters
	return "FMS-" + base32Encode(buf[:]), nil
}

// base32Alphabet is RFC 4648 base32, lowercased: references travel in URLs and
// the lowercase form avoids case-sensitivity surprises in logs and queries.
const base32Alphabet = "abcdefghijklmnopqrstuvwxyz234567"

func base32Encode(b []byte) string {
	var out [20]byte
	// Treat the input as a bit stream, 5 bits at a time.
	var (
		acc  uint16
		bits uint
		pos  int
	)
	for _, c := range b {
		acc = acc<<8 | uint16(c)
		bits += 8
		for bits >= 5 {
			bits -= 5
			out[pos] = base32Alphabet[(acc>>bits)&0x1f]
			pos++
		}
	}
	for bits > 0 && pos < len(out) { // pad the tail with zeroes
		out[pos] = base32Alphabet[(acc<<(5-bits))&0x1f]
		pos++
		bits -= 5
	}
	return string(out[:pos])
}

// placeholderEmail is the address Paystack requires for an account that signed
// in with a phone and has no email. It is deterministic in the user id, so the
// same account always presents the same address, and it is not deliverable.
func placeholderEmail(userID uuid.UUID) string {
	id := userID.String()
	hexPart := strings.ReplaceAll(id, "-", "")[:12]
	return "u" + hexPart + "@users.farmish.gh"
}

// grossUp computes what the buyer pays for base at the configured fee rate.
func grossUp(base int64, feeBps int) (charge, fee int64, err error) {
	charge, fee, err = money.GrossUp(base, feeBps)
	if err != nil {
		// An unrepresentable amount is a validation failure for the caller, not
		// a provider problem.
		return 0, 0, fmt.Errorf("%w: %w", ErrAmountOutOfRange, err)
	}
	return charge, fee, nil
}

// validatePurpose keeps the CHECK constraint's list in one place.
func validPurpose(purpose string) bool {
	return purpose == PurposePromotion || purpose == PurposeCheckout
}
