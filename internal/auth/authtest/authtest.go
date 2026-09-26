// Package authtest creates real users and ID tokens in the Firebase Auth
// emulator for tests.
//
// Set FIREBASE_AUTH_EMULATOR_HOST (make test does). Without it, auth tests are
// skipped, unless AUTHTEST_REQUIRED=1, in which case they fail instead.
package authtest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/dezmymachine/farmish-backend/internal/auth"
	"github.com/dezmymachine/farmish-backend/internal/config"
)

// ProjectID is the emulator project. demo-* IDs never reach a real project.
const ProjectID = "demo-farmish"

// User is an emulator account with a fresh ID token.
type User struct {
	UID      string
	Email    string
	Password string
	Phone    string
	Token    string
}

// Host returns the emulator host, skipping (or failing) the test without one.
func Host(t testing.TB) string {
	t.Helper()
	h := os.Getenv("FIREBASE_AUTH_EMULATOR_HOST")
	if h != "" {
		return h
	}
	if os.Getenv("AUTHTEST_REQUIRED") == "1" {
		t.Fatal("authtest: FIREBASE_AUTH_EMULATOR_HOST is not set (AUTHTEST_REQUIRED=1)")
	}
	t.Skip("authtest: FIREBASE_AUTH_EMULATOR_HOST not set; skipping auth test (run `make test`)")
	return ""
}

// Firebase returns an Admin SDK client bound to the emulator.
func Firebase(t testing.TB) *auth.Firebase {
	t.Helper()
	Host(t)
	f, err := auth.NewFirebase(context.Background(), config.Firebase{ProjectID: ProjectID})
	if err != nil {
		t.Fatalf("authtest: %v", err)
	}
	return f
}

// EmailUser signs up a new email/password account.
func EmailUser(t testing.TB) User {
	t.Helper()
	email := "user-" + randHex(6) + "@farmish.test"
	password := "secret-" + randHex(4)
	var out struct {
		IDToken string `json:"idToken"`
		LocalID string `json:"localId"`
	}
	call(t, "accounts:signUp", map[string]any{"email": email, "password": password, "returnSecureToken": true}, &out)
	return User{UID: out.LocalID, Email: email, Password: password, Token: out.IDToken}
}

// PhoneUser signs in a new phone account through the emulator's SMS flow.
func PhoneUser(t testing.TB) User {
	t.Helper()
	n, _ := rand.Int(rand.Reader, big.NewInt(9_000_000))
	phone := fmt.Sprintf("+23324%07d", n.Int64()+1_000_000)

	var sent struct {
		SessionInfo string `json:"sessionInfo"`
	}
	call(t, "accounts:sendVerificationCode", map[string]any{"phoneNumber": phone, "recaptchaToken": "emulator"}, &sent)

	var codes struct {
		VerificationCodes []struct {
			Code        string `json:"code"`
			SessionInfo string `json:"sessionInfo"`
		} `json:"verificationCodes"`
	}
	get(t, "/emulator/v1/projects/"+ProjectID+"/verificationCodes", &codes)
	code := ""
	for _, c := range codes.VerificationCodes {
		if c.SessionInfo == sent.SessionInfo {
			code = c.Code
		}
	}
	if code == "" {
		t.Fatal("authtest: verification code not found in emulator")
	}

	var out struct {
		IDToken string `json:"idToken"`
		LocalID string `json:"localId"`
	}
	call(t, "accounts:signInWithPhoneNumber", map[string]any{"sessionInfo": sent.SessionInfo, "code": code}, &out)
	return User{UID: out.LocalID, Phone: phone, Token: out.IDToken}
}

// Revoke revokes every session for uid, so the user's existing ID tokens
// fail VerifyStrict (they still pass Verify until they expire).
func Revoke(t testing.TB, fb *auth.Firebase, uid string) {
	t.Helper()
	if err := fb.RevokeSessions(context.Background(), uid); err != nil {
		t.Fatalf("authtest: revoke: %v", err)
	}
}

// SignIn returns a fresh ID token for an existing email user (e.g. to pick up
// new custom claims).
func SignIn(t testing.TB, email, password string) string {
	t.Helper()
	var out struct {
		IDToken string `json:"idToken"`
	}
	call(t, "accounts:signInWithPassword", map[string]any{"email": email, "password": password, "returnSecureToken": true}, &out)
	return out.IDToken
}

// SetCustomClaims replaces a user's custom claims directly in the emulator
// (bypassing our code), e.g. to seed claims another feature would own.
func SetCustomClaims(t testing.TB, uid string, claims map[string]any) {
	t.Helper()
	attrs, _ := json.Marshal(claims)
	b, _ := json.Marshal(map[string]any{"localId": uid, "customAttributes": string(attrs)})
	url := "http://" + Host(t) + "/identitytoolkit.googleapis.com/v1/projects/" + ProjectID + "/accounts:update"
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer owner") // emulator admin credential
	var out map[string]any
	do(t, req, &out)
}

// UnsignedToken builds an emulator-style (alg "none") token from claims, with
// sensible defaults for this project that claims can override. Use it to
// craft expired or wrong-audience tokens.
func UnsignedToken(claims map[string]any) string {
	now := time.Now().Unix()
	uid := "crafted-" + randHex(6)
	c := map[string]any{
		"iss": "https://securetoken.google.com/" + ProjectID, "aud": ProjectID,
		"sub": uid, "user_id": uid, "iat": now, "exp": now + 3600, "auth_time": now,
		"firebase": map[string]any{"sign_in_provider": "password", "identities": map[string]any{}},
	}
	for k, v := range claims {
		c[k] = v
	}
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]string{"alg": "none", "typ": "JWT"}) + "." + enc(c) + "."
}

func call(t testing.TB, method string, body, out any) {
	t.Helper()
	b, _ := json.Marshal(body)
	url := "http://" + Host(t) + "/identitytoolkit.googleapis.com/v1/" + method + "?key=emulator"
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	do(t, req, out)
}

func get(t testing.TB, path string, out any) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+Host(t)+path, nil)
	do(t, req, out)
}

func do(t testing.TB, req *http.Request, out any) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("authtest: %s: %v", req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		var e bytes.Buffer
		_, _ = e.ReadFrom(resp.Body)
		t.Fatalf("authtest: %s: status %d: %s", req.URL.Path, resp.StatusCode, e.String())
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("authtest: %s: decode: %v", req.URL.Path, err)
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
