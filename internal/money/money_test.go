package money

import (
	"errors"
	"testing"
)

func TestRoundHalfUp(t *testing.T) {
	tests := []struct {
		name    string
		a, b, d int64
		want    int64
		wantErr bool
	}{
		// The DOMAIN §2.1 commission example, spelled out.
		{"commission example 12345 x 500bps", 12345, 500, BpsDenominator, 617, false},
		{"exact division", 100, 1, 2, 50, false},
		{"half rounds up", 1, 1, 2, 1, false},
		{"just under half rounds down", 1, 1, 3, 0, false},
		{"exactly half of 3", 3, 1, 2, 2, false},
		{"zero", 0, 1, 2, 0, false},
		{"one pesewa at 1%", 1, 100, BpsDenominator, 0, false},
		{"one pesewa at 50%", 1, 5000, BpsDenominator, 1, false},
		{"99 at 500bps", 99, 500, BpsDenominator, 5, false},
		{"100 at 500bps", 100, 500, BpsDenominator, 5, false},
		{"10001 at 500bps", 10001, 500, BpsDenominator, 500, false},
		{"10001 at 333bps", 10001, 333, BpsDenominator, 333, false},
		{"zero divisor", 100, 1, 0, 0, true},
		{"negative divisor", 100, 1, -2, 0, true},
		{"negative amount", -1, 1, 2, 0, true},
		{"amount above MaxAmount", MaxAmount + 1, 1, 2, 0, true},
		{"product overflows int64 but result is fine", MaxAmount, MaxAmount, MaxAmount, MaxAmount, false},
		{"result above MaxAmount", MaxAmount, 2, 1, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RoundHalfUp(tt.a, tt.b, tt.d)
			if tt.wantErr {
				if !errors.Is(err, ErrOverflow) {
					t.Fatalf("RoundHalfUp(%d, %d, %d) = %d, %v; want ErrOverflow", tt.a, tt.b, tt.d, got, err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("RoundHalfUp(%d, %d, %d) = %d, %v; want %d", tt.a, tt.b, tt.d, got, err, tt.want)
			}
		})
	}
}

func TestCeilDiv(t *testing.T) {
	tests := []struct {
		name    string
		a, d    int64
		want    int64
		wantErr bool
	}{
		{"exact", 100, 10, 10, false},
		{"rounds up", 101, 10, 11, false},
		{"one over", 1, 2, 1, false},
		{"zero", 0, 10, 0, false},
		{"99 over 100", 99, 100, 1, false},
		{"10001 over 3", 10001, 3, 3334, false},
		{"max amount", MaxAmount, 1, MaxAmount, false},
		{"zero divisor", 100, 0, 0, true},
		{"negative divisor", 100, -10, 0, true},
		{"negative amount", -100, 10, 0, true},
		{"above MaxAmount", MaxAmount + 1, 10, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CeilDiv(tt.a, tt.d)
			if tt.wantErr {
				if !errors.Is(err, ErrOverflow) {
					t.Fatalf("CeilDiv(%d, %d) = %d, %v; want ErrOverflow", tt.a, tt.d, got, err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("CeilDiv(%d, %d) = %d, %v; want %d", tt.a, tt.d, got, err, tt.want)
			}
		})
	}
}

func TestGrossUp(t *testing.T) {
	// 195 bps is PAYSTACK_FEE_BPS (DOMAIN §2.2).
	const feeBps = 195
	tests := []struct {
		name       string
		base       int64
		feeBps     int
		wantCharge int64
		wantFee    int64
		wantErr    bool
	}{
		// The DOMAIN §2.2 example: base 10,000 -> charge 10,199, fee 199.
		{"dom example base 10000", 10_000, feeBps, 10_199, 199, false},
		{"one pesewa still covers the fee", 1, feeBps, 2, 1, false},
		{"zero base", 0, feeBps, 0, 0, false},
		{"99", 99, feeBps, 101, 2, false},
		{"100", 100, feeBps, 102, 2, false},
		{"10001", 10001, feeBps, 10_200, 199, false},
		{"no fee at all", 10_000, 0, 10_000, 0, false},
		{"a fee of 9999bps leaves a denominator of 1", 10_000, 9_999, 100_000_000, 99_990_000, false},
		{"negative base", -1, feeBps, 0, 0, true},
		{"fee of exactly 100%", 10_000, 10_000, 0, 0, true},
		{"fee above 100%", 10_000, 10_001, 0, 0, true},
		{"negative fee", 10_000, -1, 0, 0, true},
		{"base above MaxAmount", MaxAmount + 1, feeBps, 0, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			charge, fee, err := GrossUp(tt.base, tt.feeBps)
			if tt.wantErr {
				if !errors.Is(err, ErrOverflow) {
					t.Fatalf("GrossUp(%d, %d) = %d, %d, %v; want ErrOverflow", tt.base, tt.feeBps, charge, fee, err)
				}
				return
			}
			if err != nil || charge != tt.wantCharge || fee != tt.wantFee {
				t.Errorf("GrossUp(%d, %d) = %d, %d, %v; want %d, %d",
					tt.base, tt.feeBps, charge, fee, err, tt.wantCharge, tt.wantFee)
			}
		})
	}
}

// TestGrossUp_NeverUnderCovers is the property that matters: whatever Paystack
// takes as a percentage of the charge, the platform must still net the base.
func TestGrossUp_NeverUnderCovers(t *testing.T) {
	bases := []int64{0, 1, 99, 100, 101, 999, 1000, 10001, 123_456, 1_000_000, MaxAmount / 2}
	for _, bps := range []int{0, 1, 100, 195, 500, 1_000, 2_500, 5_000, 9_999} {
		for _, base := range bases {
			charge, fee, err := GrossUp(base, bps)
			if errors.Is(err, ErrOverflow) {
				// A rate like 9999bps grosses a large base past MaxAmount.
				// Refusing is the guard working, not a rounding failure.
				continue
			}
			if err != nil {
				t.Fatalf("GrossUp(%d, %d): %v", base, bps, err)
			}
			if charge != base+fee {
				t.Fatalf("GrossUp(%d, %d) = charge %d, fee %d; want charge == base+fee", base, bps, charge, fee)
			}
			if fee < 0 {
				t.Fatalf("GrossUp(%d, %d) gave a negative fee %d", base, bps, fee)
			}
			// Paystack's cut of the charge, rounded down as they would, must
			// leave the platform with at least the base.
			net := charge - ceilDivRaw(charge*int64(bps), BpsDenominator)
			if net < base {
				t.Errorf("GrossUp(%d, %d) = %d: net after Paystack's cut is %d, below the base %d",
					base, bps, charge, net, base)
			}
		}
	}
}

// TestGrossUp_TopOfRangeIsRefused documents the boundary: a base at MaxAmount
// grosses up above MaxAmount, so the guard rejects it rather than returning an
// amount outside the supported range.
func TestGrossUp_TopOfRangeIsRefused(t *testing.T) {
	if _, _, err := GrossUp(MaxAmount, 0); err != nil {
		t.Fatalf("GrossUp(MaxAmount, 0) = %v, want the base itself to be allowed", err)
	}
	if _, _, err := GrossUp(MaxAmount, 1); !errors.Is(err, ErrOverflow) {
		t.Errorf("GrossUp(MaxAmount, 1) = %v, want ErrOverflow", err)
	}
	// A base with room to gross up still works, and its charge equals
	// base+fee. At 1bps the multiplier is 10000/9999, so a base a little under
	// the ceiling still fits.
	charge, fee, err := GrossUp(900_000_000_000, 1)
	if err != nil {
		t.Fatalf("GrossUp(9e11, 1) = %v", err)
	}
	if charge != 900_000_000_000+fee {
		t.Errorf("charge %d != base 900000000000 + fee %d", charge, fee)
	}
}

func TestCommission(t *testing.T) {
	tests := []struct {
		name     string
		subtotal int64
		bps      int
		want     int64
		wantErr  bool
	}{
		// The DOMAIN §2.1 example: 12,345 at 500 bps -> 617.
		{"dom example 12345 at 500bps", 12_345, 500, 617, false},
		{"default rate on 10000", 10_000, 500, 500, false},
		{"zero rate", 10_000, 0, 0, false},
		{"100% rate", 10_000, 10_000, 10_000, false},
		{"rounds half up", 1, 5_000, 1, false},
		{"rounds down below half", 1, 4_999, 0, false},
		{"99 at 500bps", 99, 500, 5, false},
		{"10001 at 500bps", 10001, 500, 500, false},
		{"one pesewa at 195bps", 1, 195, 0, false},
		{"negative subtotal", -1, 500, 0, true},
		{"rate above 100%", 10_000, 10_001, 0, true},
		{"negative rate", 10_000, -1, 0, true},
		{"subtotal above MaxAmount", MaxAmount + 1, 500, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Commission(tt.subtotal, tt.bps)
			if tt.wantErr {
				if !errors.Is(err, ErrOverflow) {
					t.Fatalf("Commission(%d, %d) = %d, %v; want ErrOverflow", tt.subtotal, tt.bps, got, err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("Commission(%d, %d) = %d, %v; want %d", tt.subtotal, tt.bps, got, err, tt.want)
			}
		})
	}
}

// TestNoFloats is a guard rather than a test of behaviour: the package must
// not grow a float-based helper. It documents the rule next to the tests.
func TestNoFloats(t *testing.T) {
	if BpsDenominator != 10_000 {
		t.Errorf("BpsDenominator = %d, want 10000", BpsDenominator)
	}
	if MaxAmount != 1_000_000_000_000 {
		t.Errorf("MaxAmount = %d, want 1e12", MaxAmount)
	}
}

func TestFormatGHS(t *testing.T) {
	for _, tc := range []struct {
		pesewas int64
		want    string
	}{
		{0, "0.00"},
		{1, "0.01"},
		{20, "0.20"},
		{2000, "20.00"},
		{12345, "123.45"},
		{1000000000000, "10000000000.00"},
	} {
		if got, err := FormatGHS(tc.pesewas); err != nil || got != tc.want {
			t.Errorf("FormatGHS(%d) = (%q, %v), want (%q, nil)", tc.pesewas, got, err, tc.want)
		}
	}
	if _, err := FormatGHS(-1); err == nil {
		t.Error("negative amount: want an error")
	}
}
