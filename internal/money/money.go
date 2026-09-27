// Package money holds Farmish's arithmetic on integer pesewas. Every amount in
// the system is an int64 count of pesewas (GHS 1.00 = 100 pesewas) and the
// currency is always GHS (DOMAIN §1). No float ever touches an amount.
//
// Each division has one explicit rounding rule, named after it:
//
//   - RoundHalfUp for commission: the platform must not lose half a pesewa to
//     truncation, and must not gain one either.
//   - CeilDiv for the processing-fee gross-up: the platform must never receive
//     less than it needs to cover Paystack's cut.
//
// The helpers bound their inputs to MaxAmount and return ErrOverflow rather
// than wrapping, because a wrapped amount is a wrong charge.
package money

import (
	"errors"
	"fmt"
	"math/bits"
)

// BpsDenominator is the denominator for basis points: 10,000 bps is 100%.
const BpsDenominator = 10_000

// MaxAmount is the largest amount any helper accepts: 10,000,000,000.00 GHS in
// pesewas. It is far below the int64 ceiling, so the products below have room
// before they need 128 bits.
const MaxAmount = int64(1e12)

// ErrOverflow is returned when an input is negative or above MaxAmount, or
// when a result would leave the supported range.
var ErrOverflow = errors.New("money: amount out of range")

// RoundHalfUp returns a*b/d rounded half up: (a*b + d/2) / d. This is the
// commission rule (DOMAIN §2.1).
func RoundHalfUp(a, b, d int64) (int64, error) {
	if err := check(a, b); err != nil {
		return 0, err
	}
	if d <= 0 || d > MaxAmount {
		return 0, fmt.Errorf("%w: divisor %d", ErrOverflow, d)
	}
	// a*b can exceed int64 (up to 1e24), so the product, the half and the
	// division are done in 128 bits.
	// check() has already rejected negatives and anything above MaxAmount, so
	// these conversions cannot wrap.
	hi, lo := bits.Mul64(uint64(a), uint64(b)) //nolint:gosec // G115: validated non-negative by check()
	lo, carry := bits.Add64(lo, uint64(d)/2, 0)
	hi, _ = bits.Add64(hi, 0, carry)
	return div128(hi, lo, uint64(d))
}

// CeilDiv returns a/d rounded up: (a + d - 1) / d. This is the gross-up rule
// (DOMAIN §2.2), where under-charging would leave the platform short.
func CeilDiv(a, d int64) (int64, error) {
	if err := check(a); err != nil {
		return 0, err
	}
	if d <= 0 || d > MaxAmount {
		return 0, fmt.Errorf("%w: divisor %d", ErrOverflow, d)
	}
	// a + d is at most 2*MaxAmount, well inside int64.
	return ceilDivRaw(a, d), nil
}

// ceilDivRaw is CeilDiv without the input bound, for callers that have already
// checked their inputs and whose numerator is a product rather than an amount.
func ceilDivRaw(a, d int64) int64 { return (a + d - 1) / d }

// GrossUp grosses up base so Paystack's percentage cut of the charge still
// leaves the platform with the full base (DOMAIN §2.2):
//
//	charge = ceil(base * 10000 / (10000 - feeBps))
//
// feeBps is Paystack's fee in basis points (PAYSTACK_FEE_BPS, 195 today). The
// returned fee is the difference and is never negative: at 0 bps the charge
// equals the base.
//
// A feeBps of 10,000 or more is rejected: the denominator would be zero or
// negative, and no such rate is real.
func GrossUp(base int64, feeBps int) (charge, fee int64, err error) {
	if err := check(base); err != nil {
		return 0, 0, err
	}
	if feeBps < 0 || feeBps >= BpsDenominator {
		return 0, 0, fmt.Errorf("%w: feeBps %d must be 0..%d", ErrOverflow, feeBps, BpsDenominator-1)
	}
	denominator := int64(BpsDenominator - feeBps)
	// base * 10,000 is at most 1e16: inside int64, so no 128-bit needed. The
	// numerator is a product, not an amount, hence ceilDivRaw.
	numerator := base * BpsDenominator
	charge = ceilDivRaw(numerator, denominator)
	if err := check(charge); err != nil {
		return 0, 0, err
	}
	if charge < base {
		// Unreachable for a non-negative fee, but a fee larger than the charge
		// would be a bug worth failing loudly on.
		return 0, 0, fmt.Errorf("%w: charge %d below base %d", ErrOverflow, charge, base)
	}
	return charge, charge - base, nil
}

// Commission returns the platform's cut of subtotal at rateBps, rounded half
// up (DOMAIN §2.1). Delivery fees are never included — the seller keeps those
// in full — so callers pass the item subtotal only.
func Commission(subtotal int64, rateBps int) (int64, error) {
	if err := check(subtotal); err != nil {
		return 0, err
	}
	if rateBps < 0 || rateBps > BpsDenominator {
		return 0, fmt.Errorf("%w: rateBps %d must be 0..%d", ErrOverflow, rateBps, BpsDenominator)
	}
	return RoundHalfUp(subtotal, int64(rateBps), BpsDenominator)
}

// div128 divides the 128-bit value (hi, lo) by y and returns the quotient,
// refusing a quotient that would not fit an amount.
func div128(hi, lo, y uint64) (int64, error) {
	if y == 0 {
		return 0, fmt.Errorf("%w: division by zero", ErrOverflow)
	}
	if hi >= y {
		// The quotient needs more than 64 bits: far outside any real amount.
		return 0, fmt.Errorf("%w: quotient exceeds 64 bits", ErrOverflow)
	}
	q, _ := bits.Div64(hi, lo, y)
	if q > uint64(MaxAmount) {
		return 0, fmt.Errorf("%w: result %d above %d", ErrOverflow, q, MaxAmount)
	}
	return int64(q), nil
}

// check validates every amount argument.
func check(amounts ...int64) error {
	for _, a := range amounts {
		if a < 0 || a > MaxAmount {
			return fmt.Errorf("%w: %d is not in 0..%d", ErrOverflow, a, MaxAmount)
		}
	}
	return nil
}

// FormatGHS renders pesewas as cedis with two decimals ("123.45"), for text
// channels like SMS that cannot use the Money shape. Callers prefix "GHS "
// themselves. No float is involved: it is integer division and remainder.
func FormatGHS(pesewas int64) (string, error) {
	if err := check(pesewas); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d.%02d", pesewas/100, pesewas%100), nil
}
