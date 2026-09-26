// Package auth verifies Firebase ID tokens and manages Firebase custom claims.
// It knows nothing about our users table; internal/users maps an Identity to
// a users row.
package auth

import (
	"context"
	"errors"
	"time"
)

// ErrInvalidToken wraps every token verification failure. Callers answer with
// a generic 401 and log the wrapped cause.
var ErrInvalidToken = errors.New("invalid token")

// Identity is what a verified ID token says about the caller.
type Identity struct {
	UID           string
	Email         string
	EmailVerified bool
	Phone         string // E.164, set for phone sign-ins
	Name          string
	Provider      string // firebase.sign_in_provider, e.g. "phone", "password", "google.com"
	AuthTime      time.Time
}

// Verifier verifies a raw ID token.
//
// Verify checks signature, issuer, audience and expiry locally. VerifyStrict
// additionally checks revocation with Firebase (a network call): use it only
// for step-up operations on sensitive endpoints.
type Verifier interface {
	Verify(ctx context.Context, idToken string) (Identity, error)
	VerifyStrict(ctx context.Context, idToken string) (Identity, error)
}

// ClaimsSetter mirrors a user's role into Firebase custom claims.
type ClaimsSetter interface {
	SetRoleClaim(ctx context.Context, uid, role string) error
}

// Signup methods stored in users.signup_method.
const (
	SignupSocial = "social"
	SignupEmail  = "email"
	SignupPhone  = "phone"
)

// SignupMethod maps a sign_in_provider to users.signup_method. ok is false
// for providers we never accept: custom tokens (we don't mint any) and
// anonymous sessions.
func SignupMethod(provider string) (method string, ok bool) {
	switch provider {
	case "phone":
		return SignupPhone, true
	case "password", "emailLink":
		return SignupEmail, true
	case "", "custom", "anonymous":
		return "", false
	default: // google.com, apple.com, facebook.com, oidc.*, saml.*, ...
		return SignupSocial, true
	}
}
