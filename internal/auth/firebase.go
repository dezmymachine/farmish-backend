package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	firebase "firebase.google.com/go/v4"
	fbauth "firebase.google.com/go/v4/auth"
	"google.golang.org/api/option"

	"github.com/dezmymachine/farmish-backend/internal/config"
)

// roleClaim is the custom claim mirroring users.role for the frontend. It is
// informational only: authorization always reads users.role.
const roleClaim = "role"

// Firebase implements Verifier and ClaimsSetter with the Firebase Admin SDK.
//
// When FIREBASE_AUTH_EMULATOR_HOST is set (dev/test only; config refuses it
// when deployed) the SDK talks to the emulator and skips signature checks.
type Firebase struct {
	client *fbauth.Client
}

var (
	_ Verifier     = (*Firebase)(nil)
	_ ClaimsSetter = (*Firebase)(nil)
)

// NewFirebase creates the Admin SDK client. Without credentials it can still
// verify ID tokens (public Google certs); setting claims then fails.
func NewFirebase(ctx context.Context, cfg config.Firebase) (*Firebase, error) {
	opts := []option.ClientOption{option.WithoutAuthentication()}
	if len(cfg.Credentials) > 0 {
		opts = []option.ClientOption{option.WithAuthCredentialsJSON(option.ServiceAccount, cfg.Credentials)}
	}
	app, err := firebase.NewApp(ctx, &firebase.Config{ProjectID: cfg.ProjectID}, opts...)
	if err != nil {
		return nil, fmt.Errorf("firebase app: %w", err)
	}
	client, err := app.Auth(ctx)
	if err != nil {
		return nil, fmt.Errorf("firebase auth client: %w", err)
	}
	return &Firebase{client: client}, nil
}

// Verify checks the token's signature, issuer, audience and expiry locally
// (no per-request network call outside emulator mode). Revocation is not
// checked here; sensitive operations use VerifyStrict.
func (f *Firebase) Verify(ctx context.Context, idToken string) (Identity, error) {
	tok, err := f.client.VerifyIDToken(ctx, idToken)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	return identityFromToken(tok), nil
}

// VerifyStrict additionally checks with Firebase that the token hasn't been
// revoked and the account isn't disabled. It makes a network call, so callers
// must use it only for step-up operations on sensitive endpoints.
func (f *Firebase) VerifyStrict(ctx context.Context, idToken string) (Identity, error) {
	tok, err := f.client.VerifyIDTokenAndCheckRevoked(ctx, idToken)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	return identityFromToken(tok), nil
}

// RevokeSessions revokes every refresh token for uid, so existing ID tokens
// fail VerifyStrict (until they expire they still pass Verify). It is used in
// tests and will back admin tooling that signs a user out everywhere.
func (f *Firebase) RevokeSessions(ctx context.Context, uid string) error {
	if err := f.client.RevokeRefreshTokens(ctx, uid); err != nil {
		return fmt.Errorf("revoke refresh tokens: %w", err)
	}
	return nil
}

func identityFromToken(tok *fbauth.Token) Identity {
	str := func(k string) string { s, _ := tok.Claims[k].(string); return s }
	verified, _ := tok.Claims["email_verified"].(bool)
	return Identity{
		UID:           tok.UID,
		Email:         str("email"),
		EmailVerified: verified,
		Phone:         str("phone_number"),
		Name:          str("name"),
		Provider:      tok.Firebase.SignInProvider,
		AuthTime:      time.Unix(tok.AuthTime, 0),
	}
}

// SetRoleClaim sets the role custom claim ("user" removes it), preserving any
// other custom claims on the account.
func (f *Firebase) SetRoleClaim(ctx context.Context, uid, role string) error {
	u, err := f.client.GetUser(ctx, uid)
	if err != nil {
		return fmt.Errorf("get firebase user: %w", err)
	}
	claims := map[string]any{}
	for k, v := range u.CustomClaims {
		claims[k] = v
	}
	if role == "" || role == "user" {
		delete(claims, roleClaim)
	} else {
		claims[roleClaim] = role
	}
	if err := f.client.SetCustomUserClaims(ctx, uid, claims); err != nil {
		return fmt.Errorf("set custom claims: %w", err)
	}
	return nil
}

// CustomClaims returns a user's custom claims (used by tests and tooling).
func (f *Firebase) CustomClaims(ctx context.Context, uid string) (map[string]any, error) {
	u, err := f.client.GetUser(ctx, uid)
	if err != nil {
		if fbauth.IsUserNotFound(err) {
			return nil, errors.New("firebase user not found")
		}
		return nil, fmt.Errorf("get firebase user: %w", err)
	}
	return u.CustomClaims, nil
}
