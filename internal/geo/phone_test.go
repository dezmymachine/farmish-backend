package geo

import (
	"errors"
	"testing"
)

func TestNormalizeGhanaPhone(t *testing.T) {
	valid := map[string]string{
		"0241234567":       "+233241234567",
		"233241234567":     "+233241234567",
		"+233241234567":    "+233241234567",
		"+233 24 123 4567": "+233241234567",
		"024-123-4567":     "+233241234567",
		"024 123 4567":     "+233241234567",
		"(024)1234567":     "+233241234567",
	}
	for in, want := range valid {
		if got, err := NormalizeGhanaPhone(in); err != nil || got != want {
			t.Errorf("NormalizeGhanaPhone(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"+23324123456",   // 8 digits
		"+2332412345678", // 10 digits
		"+234241234567",
		"abc",
		"",
		"024123456", // too short
		"00233241234567",
	} {
		if got, err := NormalizeGhanaPhone(in); !errors.Is(err, ErrInvalidPhone) {
			t.Errorf("NormalizeGhanaPhone(%q) = %q, %v; want ErrInvalidPhone", in, got, err)
		}
	}
}

func TestMaskPhone(t *testing.T) {
	if got := MaskPhone("+233241234567"); got != "+233*****4567" {
		t.Errorf("MaskPhone = %q", got)
	}
	for _, in := range []string{"", "x", "1234567", "+233"} {
		func() {
			defer func() {
				if recover() != nil {
					t.Errorf("MaskPhone(%q) panicked", in)
				}
			}()
			_ = MaskPhone(in)
		}()
	}
}
