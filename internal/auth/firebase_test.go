package auth_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/dezmymachine/farmish-backend/internal/auth"
	"github.com/dezmymachine/farmish-backend/internal/auth/authtest"
	"github.com/dezmymachine/farmish-backend/internal/config"
)

func TestVerify_EmailAndPhone(t *testing.T) {
	fb := authtest.Firebase(t)
	ctx := context.Background()

	eu := authtest.EmailUser(t)
	id, err := fb.Verify(ctx, eu.Token)
	if err != nil {
		t.Fatal(err)
	}
	if id.UID != eu.UID || id.Email != eu.Email || id.Provider != "password" || id.EmailVerified {
		t.Errorf("email identity = %+v", id)
	}
	if time.Since(id.AuthTime) > time.Minute {
		t.Errorf("auth time %v", id.AuthTime)
	}

	pu := authtest.PhoneUser(t)
	id, err = fb.Verify(ctx, pu.Token)
	if err != nil {
		t.Fatal(err)
	}
	if id.UID != pu.UID || id.Phone != pu.Phone || id.Provider != "phone" || id.Email != "" {
		t.Errorf("phone identity = %+v", id)
	}
}

func TestVerify_RejectsBadTokens(t *testing.T) {
	fb := authtest.Firebase(t)
	eu := authtest.EmailUser(t)
	past := time.Now().Add(-2 * time.Hour).Unix()

	for name, token := range map[string]string{
		"empty":            "",
		"garbage":          "not-a-jwt",
		"expired":          authtest.UnsignedToken(map[string]any{"sub": eu.UID, "user_id": eu.UID, "iat": past, "exp": past + 60}),
		"wrong audience":   authtest.UnsignedToken(map[string]any{"sub": eu.UID, "user_id": eu.UID, "aud": "someone-else"}),
		"wrong issuer":     authtest.UnsignedToken(map[string]any{"sub": eu.UID, "user_id": eu.UID, "iss": "https://evil.example"}),
		"nonexistent user": authtest.UnsignedToken(nil),
		"issued in future": authtest.UnsignedToken(map[string]any{"sub": eu.UID, "user_id": eu.UID, "iat": time.Now().Add(time.Hour).Unix()}),
		"tampered payload": eu.Token[:len(eu.Token)-5] + "xxxxx",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := fb.Verify(context.Background(), token); !errors.Is(err, auth.ErrInvalidToken) {
				t.Errorf("err = %v, want ErrInvalidToken", err)
			}
		})
	}
}

// Outside emulator mode, a well-formed RS256 token signed by a key Google
// never issued must fail signature verification. (The SDK fetches Google's
// public certs; offline, verification fails closed, which also rejects.)
func TestVerify_RejectsForgedSignature(t *testing.T) {
	authtest.Host(t) // run with the rest of the auth suite
	t.Setenv("FIREBASE_AUTH_EMULATOR_HOST", "")

	fb, err := auth.NewFirebase(context.Background(), config.Firebase{ProjectID: authtest.ProjectID})
	if err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	signingInput := enc(map[string]string{"alg": "RS256", "kid": "forged-kid", "typ": "JWT"}) + "." + enc(map[string]any{
		"iss": "https://securetoken.google.com/" + authtest.ProjectID, "aud": authtest.ProjectID,
		"sub": "forged-uid", "iat": now, "exp": now + 3600, "auth_time": now,
		"firebase": map[string]any{"sign_in_provider": "password"},
	})
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	forged := signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)

	if _, err := fb.Verify(context.Background(), forged); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("forged token accepted or wrong error: %v", err)
	} else {
		t.Logf("rejected: %v", err)
	}
	// An unsigned (alg none) token must also fail outside emulator mode.
	if _, err := fb.Verify(context.Background(), authtest.UnsignedToken(nil)); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("unsigned token accepted outside emulator mode: %v", err)
	}
}

func TestSetClaim_SellerVerified(t *testing.T) {
	fb := authtest.Firebase(t)
	ctx := context.Background()
	u := authtest.EmailUser(t)
	authtest.SetCustomClaims(t, u.UID, map[string]any{"role": "admin"})

	if err := fb.SetClaim(ctx, u.UID, "seller_verified", true); err != nil {
		t.Fatal(err)
	}
	claims, err := fb.CustomClaims(ctx, u.UID)
	if err != nil || claims["seller_verified"] != true || claims["role"] != "admin" {
		t.Fatalf("after set: claims = %v, err %v", claims, err)
	}

	if err := fb.SetClaim(ctx, u.UID, "seller_verified", false); err != nil {
		t.Fatal(err)
	}
	claims, _ = fb.CustomClaims(ctx, u.UID)
	if claims["seller_verified"] != false || claims["role"] != "admin" {
		t.Errorf("after revoke: claims = %v (want seller_verified=false, role kept)", claims)
	}
}

func TestSetRoleClaim_MergesClaims(t *testing.T) {
	fb := authtest.Firebase(t)
	ctx := context.Background()
	u := authtest.EmailUser(t)
	// A claim owned by another feature (seller verification, Phase 8).
	authtest.SetCustomClaims(t, u.UID, map[string]any{"seller_verified": true})

	if err := fb.SetRoleClaim(ctx, u.UID, "admin"); err != nil {
		t.Fatal(err)
	}
	claims, err := fb.CustomClaims(ctx, u.UID)
	if err != nil || claims["role"] != "admin" || claims["seller_verified"] != true {
		t.Fatalf("after grant: claims = %v, err %v", claims, err)
	}

	if err := fb.SetRoleClaim(ctx, u.UID, "user"); err != nil {
		t.Fatal(err)
	}
	claims, _ = fb.CustomClaims(ctx, u.UID)
	if _, ok := claims["role"]; ok || claims["seller_verified"] != true {
		t.Errorf("after revoke: claims = %v (want role removed, seller_verified kept)", claims)
	}
}
