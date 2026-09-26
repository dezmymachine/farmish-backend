package auth

import "testing"

func TestSignupMethod(t *testing.T) {
	tests := []struct {
		provider, method string
		ok               bool
	}{
		{"phone", SignupPhone, true},
		{"password", SignupEmail, true},
		{"emailLink", SignupEmail, true},
		{"google.com", SignupSocial, true},
		{"apple.com", SignupSocial, true},
		{"facebook.com", SignupSocial, true},
		{"oidc.example", SignupSocial, true},
		{"custom", "", false},
		{"anonymous", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		m, ok := SignupMethod(tt.provider)
		if m != tt.method || ok != tt.ok {
			t.Errorf("SignupMethod(%q) = %q, %t; want %q, %t", tt.provider, m, ok, tt.method, tt.ok)
		}
	}
}
