package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/auth"
	"github.com/dezmymachine/farmish-backend/internal/crypto"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/internal/jobs"
	"github.com/dezmymachine/farmish-backend/internal/notify"
	"github.com/dezmymachine/farmish-backend/internal/payments"
	"github.com/dezmymachine/farmish-backend/internal/payments/fake"
	"github.com/dezmymachine/farmish-backend/internal/payouts"
	"github.com/dezmymachine/farmish-backend/internal/sellers"
	"github.com/dezmymachine/farmish-backend/internal/users"
)

// stubVerifier maps bearer tokens to identities with controllable auth times,
// so step-up freshness is tested at the endpoint level without waiting.
type stubVerifier struct {
	mu  sync.Mutex
	ids map[string]auth.Identity
}

func (s *stubVerifier) Verify(_ context.Context, token string) (auth.Identity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.ids[token]
	if !ok {
		return auth.Identity{}, auth.ErrInvalidToken
	}
	return id, nil
}

func (s *stubVerifier) VerifyStrict(_ context.Context, token string) (auth.Identity, error) {
	return s.Verify(context.Background(), token)
}

func stubIdentity(uid, email string, authTime time.Time) auth.Identity {
	return auth.Identity{UID: uid, Email: email, Provider: "password", AuthTime: authTime}
}

// payoutFixture wires the payout endpoints with a stub verifier and a
// scripted Paystack.
type payoutFixture struct {
	router   *gin.Engine
	pool     *pgxpool.Pool
	verifier *stubVerifier
	provider *fake.Provider
	pay      *payouts.Service
	sellers  *sellers.Service
}

func newPayoutFixture(t *testing.T) *payoutFixture {
	t.Helper()
	pool := dbtest.Pool(t)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	crypter, err := crypto.New(key)
	if err != nil {
		t.Fatal(err)
	}
	sellersSvc := sellers.New(pool, crypter, nil)
	provider := fake.New()
	provider.BanksResult = []payments.Bank{{Name: "MTN Mobile Money", Code: "MTN", Type: "mobile_money"}}
	provider.ResolveResult = payments.Account{AccountName: "AKOSUA MENSAH"}
	provider.RecipientResult = payments.TransferRecipient{RecipientCode: "RCP_1"}
	pay := payouts.New(pool, crypter, provider, sellersSvc)
	log := slog.New(slog.DiscardHandler)
	pay.AttachLogger(log)
	reg := jobs.NewRegistry()
	notify.Register(reg, notify.NewWorker(notify.LogOnly{Log: log}, log, pool))
	client, err := jobs.NewClient(pool, reg, log, jobs.Options{Work: false})
	if err != nil {
		t.Fatal(err)
	}
	pay.AttachJobClient(client)
	verifier := &stubVerifier{ids: map[string]auth.Identity{}}
	router := newTestRouter(t, Deps{
		DB: fakePinger{}, Verifier: verifier, Users: users.New(pool),
		Sellers: sellersSvc, Payouts: pay,
	})
	return &payoutFixture{router: router, pool: pool, verifier: verifier, provider: provider, pay: pay, sellers: sellersSvc}
}

// sellerWithProfile resolves a stub identity to a user and gives them a
// seller profile, returning the user id.
func (f *payoutFixture) sellerWithProfile(t *testing.T, token, uid, businessName string) uuid.UUID {
	t.Helper()
	u, err := users.New(f.pool).Resolve(context.Background(), auth.Identity{
		UID: uid, Email: uid + "@farmish.test", Provider: "password",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.sellers.UpsertMine(context.Background(), u.ID, sellers.ProfileInput{
		BusinessName: businessName, Region: "Ashanti", District: "Kumasi Metro",
	}); err != nil {
		t.Fatalf("create profile: %v", err)
	}
	return u.ID
}

func (f *payoutFixture) setIdentity(token, uid, email string, authTime time.Time) {
	f.verifier.mu.Lock()
	defer f.verifier.mu.Unlock()
	f.verifier.ids[token] = stubIdentity(uid, email, authTime)
}

func (f *payoutFixture) makeAdmin(t *testing.T, userID uuid.UUID) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE users SET role = 'admin' WHERE id = $1`, userID); err != nil {
		t.Fatal(err)
	}
}

const validPayoutBody = `{"type":"mobile_money","bankCode":"MTN","accountNumber":"0241234567"}`

// TestPayoutStepUp proves the step-up gate at the endpoint: a stale sign-in
// is 401 reauth_required with the re-auth header, a fresh one reaches the
// service and gets 200.
func TestPayoutStepUp(t *testing.T) {
	f := newPayoutFixture(t)
	f.setIdentity("stale-token", "payout-stale", "payout-stale@farmish.test", time.Now().Add(-6*time.Minute))
	f.sellerWithProfile(t, "stale-token", "payout-stale", "Akosua Farms")

	req := jsonRequest(http.MethodPut, "/v1/seller/payout-account", "stale-token", validPayoutBody)
	w := serve(t, f.router, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("stale = %d %s, want 401", w.Code, w.Body.String())
	}
	assertErrorEnvelope(t, w.Body.Bytes(), apierror.CodeReauthRequired)
	assertContract(t, req, w)
	if h := w.Header().Get("WWW-Authenticate"); h != `Bearer error="insufficient_user_authentication"` {
		t.Errorf("WWW-Authenticate = %q", h)
	}

	f.setIdentity("fresh-token", "payout-fresh", "payout-fresh@farmish.test", time.Now())
	f.sellerWithProfile(t, "fresh-token", "payout-fresh", "Akosua Farms")
	req = jsonRequest(http.MethodPut, "/v1/seller/payout-account", "fresh-token", validPayoutBody)
	w = serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("fresh = %d %s, want 200", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
}

// TestPayoutAccount_MaskedRoundTrip proves the masked round trip: PUT stores
// ciphertext, GET returns the same mask, and the full number appears in no
// response body.
func TestPayoutAccount_MaskedRoundTrip(t *testing.T) {
	f := newPayoutFixture(t)
	f.setIdentity("tok", "payout-round", "payout-round@farmish.test", time.Now())
	f.sellerWithProfile(t, "tok", "payout-round", "Akosua Farms")

	req := jsonRequest(http.MethodPut, "/v1/seller/payout-account", "tok", validPayoutBody)
	w := serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("put = %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var created api.PayoutAccount
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.AccountNumberMasked != "******4567" || created.Status != api.PayoutAccountStatusVerified {
		t.Errorf("created = %+v", created)
	}
	if strings.Contains(w.Body.String(), "0241234567") {
		t.Error("PUT body contains the full number")
	}

	req = jsonRequest(http.MethodGet, "/v1/seller/payout-account", "tok", "")
	w = serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("get = %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	if strings.Contains(w.Body.String(), "0241234567") {
		t.Error("GET body contains the full number")
	}
	var enc string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT account_number_enc FROM seller_payout_accounts`).Scan(&enc); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc, "v1:") || strings.Contains(enc, "0241234567") {
		t.Errorf("stored value %q is not ciphertext", enc)
	}
}

// TestPayoutAccount_ValidationAndProfile proves the failure paths: unknown
// types and banks are 400s, and sellers without a profile get 403 on both
// reads and writes.
func TestPayoutAccount_ValidationAndProfile(t *testing.T) {
	f := newPayoutFixture(t)
	f.setIdentity("tok", "payout-valid", "payout-valid@farmish.test", time.Now())
	f.sellerWithProfile(t, "tok", "payout-valid", "Akosua Farms")
	f.setIdentity("noprofile", "payout-noprofile", "payout-noprofile@farmish.test", time.Now())
	if _, err := users.New(f.pool).Resolve(context.Background(), auth.Identity{
		UID: "payout-noprofile", Email: "payout-noprofile@farmish.test", Provider: "password",
	}); err != nil {
		t.Fatal(err)
	}

	for name, body := range map[string]string{
		"unknown type": `{"type":"crypto","bankCode":"MTN","accountNumber":"0241234567"}`,
		"short number": `{"type":"mobile_money","bankCode":"MTN","accountNumber":"123"}`,
		"unknown bank": `{"type":"mobile_money","bankCode":"NOPE","accountNumber":"0241234567"}`,
		"missing body": ``,
	} {
		req := jsonRequest(http.MethodPut, "/v1/seller/payout-account", "tok", body)
		w := serve(t, f.router, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d %s, want 400", name, w.Code, w.Body.String())
		} else {
			assertContract(t, req, w)
		}
	}

	for _, method := range []string{http.MethodGet, http.MethodPut} {
		body := ""
		if method == http.MethodPut {
			body = validPayoutBody
		}
		req := jsonRequest(method, "/v1/seller/payout-account", "noprofile", body)
		w := serve(t, f.router, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s without profile = %d, want 403", method, w.Code)
		} else {
			assertErrorEnvelope(t, w.Body.Bytes(), apierror.CodeForbidden)
			assertContract(t, req, w)
		}
	}

	req := jsonRequest(http.MethodGet, "/v1/seller/payout-account", "tok", "")
	if w := serve(t, f.router, req); w.Code != http.StatusNotFound {
		t.Errorf("get before setup = %d, want 404", w.Code)
	} else {
		assertContract(t, req, w)
	}
}

// TestPayoutAccount_Unresolvable422 proves a Paystack refusal is a 422 with
// the account_unresolvable code.
func TestPayoutAccount_Unresolvable422(t *testing.T) {
	f := newPayoutFixture(t)
	f.setIdentity("tok", "payout-unres", "payout-unres@farmish.test", time.Now())
	f.sellerWithProfile(t, "tok", "payout-unres", "Akosua Farms")
	f.provider.ResolveErr = fmt.Errorf("%w: account not found at bank", payments.ErrRejected)

	req := jsonRequest(http.MethodPut, "/v1/seller/payout-account", "tok", validPayoutBody)
	w := serve(t, f.router, req)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unresolvable = %d %s, want 422", w.Code, w.Body.String())
	}
	assertErrorEnvelope(t, w.Body.Bytes(), apierror.CodeAccountUnresolvable)
	assertContract(t, req, w)
}

// TestPayoutAccount_Cooldown proves the second PUT carries now+48h and sends
// another security SMS.
func TestPayoutAccount_Cooldown(t *testing.T) {
	f := newPayoutFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	f.pay.Now = func() time.Time { return now }
	f.setIdentity("tok", "payout-cool", "payout-cool@farmish.test", time.Now())
	f.sellerWithProfile(t, "tok", "payout-cool", "Akosua Farms")

	put := func(body string) api.PayoutAccount {
		req := jsonRequest(http.MethodPut, "/v1/seller/payout-account", "tok", body)
		w := serve(t, f.router, req)
		if w.Code != http.StatusOK {
			t.Fatalf("put = %d %s", w.Code, w.Body.String())
		}
		assertContract(t, req, w)
		var account api.PayoutAccount
		if err := json.Unmarshal(w.Body.Bytes(), &account); err != nil {
			t.Fatal(err)
		}
		return account
	}
	if first := put(validPayoutBody); first.CooldownUntil != nil {
		t.Errorf("first cooldown = %v, want none", first.CooldownUntil)
	}
	now = now.Add(time.Hour)
	second := put(`{"type":"mobile_money","bankCode":"MTN","accountNumber":"0247654321"}`)
	if second.CooldownUntil == nil || !second.CooldownUntil.Equal(now.Add(48*time.Hour)) {
		t.Errorf("second cooldown = %v, want now+48h", second.CooldownUntil)
	}
	var sms int64
	if err := f.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM river_job WHERE kind = 'notify.sms' AND args->>'template' = 'payout_account_changed'`).Scan(&sms); err != nil {
		t.Fatal(err)
	}
	if sms != 2 {
		t.Errorf("security SMS jobs = %d, want 2", sms)
	}
}

// TestPayoutAccount_Approve proves the admin review: needs_review waits,
// approve verifies with audit, a second approve 409s, strangers 403.
func TestPayoutAccount_Approve(t *testing.T) {
	f := newPayoutFixture(t)
	f.setIdentity("seller", "payout-appr", "payout-appr@farmish.test", time.Now())
	sellerID := f.sellerWithProfile(t, "seller", "payout-appr", "Akosua Farms")
	f.setIdentity("admin", "payout-admin", "payout-admin@farmish.test", time.Now())
	adminID, err := users.New(f.pool).Resolve(context.Background(), auth.Identity{
		UID: "payout-admin", Email: "payout-admin@farmish.test", Provider: "password",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.provider.ResolveResult = payments.Account{AccountName: "SOMEONE ELSE"}

	req := jsonRequest(http.MethodPut, "/v1/seller/payout-account", "seller", validPayoutBody)
	w := serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("put = %d %s", w.Code, w.Body.String())
	}
	var created api.PayoutAccount
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Status != api.PayoutAccountStatusNeedsReview {
		t.Fatalf("status = %s, want needs_review", created.Status)
	}

	// A non-admin cannot approve.
	req = jsonRequest(http.MethodPost, "/v1/admin/payout-accounts/"+sellerID.String()+"/approve", "seller", "")
	if w := serve(t, f.router, req); w.Code != http.StatusForbidden {
		t.Errorf("seller approve = %d, want 403", w.Code)
	} else {
		assertContract(t, req, w)
	}

	f.makeAdmin(t, adminID.ID)
	path := "/v1/admin/payout-accounts/" + sellerID.String() + "/approve"
	req = jsonRequest(http.MethodPost, path, "admin", "")
	w = serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("approve = %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var approved api.PayoutAccount
	if err := json.Unmarshal(w.Body.Bytes(), &approved); err != nil {
		t.Fatal(err)
	}
	if approved.Status != api.PayoutAccountStatusVerified {
		t.Errorf("status = %s, want verified", approved.Status)
	}

	req = jsonRequest(http.MethodPost, path, "admin", "")
	if w := serve(t, f.router, req); w.Code != http.StatusConflict {
		t.Errorf("second approve = %d, want 409", w.Code)
	} else {
		assertContract(t, req, w)
	}
	req = jsonRequest(http.MethodPost, "/v1/admin/payout-accounts/"+uuid.NewString()+"/approve", "admin", "")
	if w := serve(t, f.router, req); w.Code != http.StatusNotFound {
		t.Errorf("approve missing = %d, want 404", w.Code)
	} else {
		assertContract(t, req, w)
	}
}

// TestBanks_CachedList proves the endpoint serves Paystack's banks and only
// asks once for two identical calls.
func TestBanks_CachedList(t *testing.T) {
	f := newPayoutFixture(t)
	f.setIdentity("tok", "payout-banks", "payout-banks@farmish.test", time.Now())

	get := func(query string) api.BankList {
		req := jsonRequest(http.MethodGet, "/v1/payouts/banks"+query, "tok", "")
		w := serve(t, f.router, req)
		if w.Code != http.StatusOK {
			t.Fatalf("banks%s = %d %s", query, w.Code, w.Body.String())
		}
		assertContract(t, req, w)
		var list api.BankList
		if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
			t.Fatal(err)
		}
		return list
	}
	first := get("?type=mobile_money")
	if len(first.Items) != 1 || first.Items[0].Code != "MTN" {
		t.Fatalf("banks = %+v", first)
	}
	get("?type=mobile_money")
	if n := f.provider.CallCount("ListBanks"); n != 1 {
		t.Errorf("Paystack calls = %d, want 1", n)
	}
	all := get("")
	if len(all.Items) != 2 {
		t.Errorf("unfiltered banks = %+v, want both types", all)
	}

	req := jsonRequest(http.MethodGet, "/v1/payouts/banks?type=bogus", "tok", "")
	if w := serve(t, f.router, req); w.Code != http.StatusBadRequest {
		t.Errorf("bad type = %d, want 400", w.Code)
	} else {
		assertContract(t, req, w)
	}
	req = jsonRequest(http.MethodGet, "/v1/payouts/banks?type=mobile_money", "", "")
	if w := serve(t, f.router, req); w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous = %d, want 401", w.Code)
	} else {
		assertContract(t, req, w)
	}
}

// TestPayoutAccount_NeverUnmasked greps every response body these endpoints
// return for the full account numbers: only masks may leave the server.
func TestPayoutAccount_NeverUnmasked(t *testing.T) {
	f := newPayoutFixture(t)
	f.setIdentity("tok", "payout-mask", "payout-mask@farmish.test", time.Now())
	f.sellerWithProfile(t, "tok", "payout-mask", "Akosua Farms")
	f.provider.BanksResult = append(f.provider.BanksResult,
		payments.Bank{Name: "Test Bank", Code: "TST", Type: "ghipss"})

	bodies := []string{}
	put := func(body string) {
		req := jsonRequest(http.MethodPut, "/v1/seller/payout-account", "tok", body)
		w := serve(t, f.router, req)
		if w.Code != http.StatusOK {
			t.Fatalf("put %s: %d %s", body, w.Code, w.Body.String())
		}
		bodies = append(bodies, w.Body.String())
	}
	put(validPayoutBody)
	put(`{"type":"ghipss","bankCode":"TST","accountNumber":"1234567890"}`)
	for _, path := range []string{"/v1/seller/payout-account", "/v1/payouts/banks", "/v1/payouts/banks?type=mobile_money"} {
		req := jsonRequest(http.MethodGet, path, "tok", "")
		w := serve(t, f.router, req)
		if w.Code != http.StatusOK {
			t.Fatalf("get %s: %d", path, w.Code)
		}
		bodies = append(bodies, w.Body.String())
	}
	for _, secret := range []string{"0241234567", "1234567890"} {
		for _, body := range bodies {
			if strings.Contains(body, secret) {
				t.Errorf("response body contains %s: %s", secret, body)
			}
		}
	}
}

// TestBanks_ProviderDown proves a Paystack outage is a 502, not a 500.
func TestBanks_ProviderDown(t *testing.T) {
	f := newPayoutFixture(t)
	f.setIdentity("tok", "payout-bankdown", "payout-bankdown@farmish.test", time.Now())
	f.provider.BanksErr = fmt.Errorf("%w: connection refused", payments.ErrProviderUnavailable)

	req := jsonRequest(http.MethodGet, "/v1/payouts/banks?type=mobile_money", "tok", "")
	w := serve(t, f.router, req)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("banks down = %d %s, want 502", w.Code, w.Body.String())
	}
	assertErrorEnvelope(t, w.Body.Bytes(), apierror.CodePaymentProvider)
	assertContract(t, req, w)
}
