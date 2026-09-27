package payouts_test

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/auth"
	"github.com/dezmymachine/farmish-backend/internal/crypto"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/jobs"
	"github.com/dezmymachine/farmish-backend/internal/notify"
	"github.com/dezmymachine/farmish-backend/internal/payments"
	"github.com/dezmymachine/farmish-backend/internal/payments/fake"
	"github.com/dezmymachine/farmish-backend/internal/payouts"
	"github.com/dezmymachine/farmish-backend/internal/sellers"
	"github.com/dezmymachine/farmish-backend/internal/users"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

// payoutFixture is a payouts service over a real database with a scripted
// Paystack, a controllable clock and an insert-only job client (tests drive
// the service directly and read the enqueued SMS rows themselves).
type payoutFixture struct {
	svc      *payouts.Service
	pool     *pgxpool.Pool
	provider *fake.Provider
	now      time.Time
	seller   uuid.UUID
}

func newPayoutFixture(t *testing.T) *payoutFixture {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Pool(t)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	crypter, err := crypto.New(key)
	if err != nil {
		t.Fatal(err)
	}
	user, err := users.New(pool).Resolve(ctx, auth.Identity{
		UID: "payout-seller", Email: "payout-seller@farmish.test", Provider: "password",
	})
	if err != nil {
		t.Fatal(err)
	}
	sellersSvc := sellers.New(pool, crypter, nil)
	if _, err := sellersSvc.UpsertMine(ctx, user.ID, sellers.ProfileInput{
		BusinessName: "Akosua Farms", Region: "Ashanti", District: "Kumasi Metro",
	}); err != nil {
		t.Fatalf("create profile: %v", err)
	}
	provider := fake.New()
	provider.BanksResult = []payments.Bank{{Name: "MTN Mobile Money", Code: "MTN", Type: "mobile_money"}}
	provider.ResolveResult = payments.Account{AccountName: "AKOSUA MENSAH"}
	provider.RecipientResult = payments.TransferRecipient{RecipientCode: "RCP_1"}
	f := &payoutFixture{
		pool: pool, provider: provider, now: time.Now().UTC().Truncate(time.Second), seller: user.ID,
	}
	svc := payouts.New(pool, crypter, provider, sellersSvc)
	svc.Now = func() time.Time { return f.now }
	log := slog.New(slog.DiscardHandler)
	svc.AttachLogger(log)
	reg := jobs.NewRegistry()
	notify.Register(reg, notify.NewWorker(notify.LogOnly{Log: log}, log, pool))
	client, err := jobs.NewClient(pool, reg, log, jobs.Options{Work: false})
	if err != nil {
		t.Fatal(err)
	}
	svc.AttachJobClient(client)
	f.svc = svc
	return f
}

func (f *payoutFixture) advance(d time.Duration) { f.now = f.now.Add(d) }

func (f *payoutFixture) count(t *testing.T, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := f.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestNormalizeNumber proves the Ghanaian prefix handling: +233, 233 and
// local 0 forms all land on 0XXXXXXXXX, anything else is refused.
func TestNormalizeNumber(t *testing.T) {
	for _, tc := range []struct {
		typ, raw, want string
		ok             bool
	}{
		{"mobile_money", "0241234567", "0241234567", true},
		{"mobile_money", "+233241234567", "0241234567", true},
		{"mobile_money", "233241234567", "0241234567", true},
		{"mobile_money", "024 123 4567", "0241234567", true},
		{"mobile_money", "024-123-4567", "0241234567", true},
		{"mobile_money", "02412345", "", false},
		{"mobile_money", "1241234567", "", false},
		{"mobile_money", "024123456a", "", false},
		{"mobile_money", "", "", false},
		{"ghipss", "1234567890", "1234567890", true},
		{"ghipss", "12345678901234567890", "12345678901234567890", true},
		{"ghipss", "123456789", "", false},
		{"ghipss", "123456789012345678901", "", false},
	} {
		got, ok := payouts.NormalizeNumber(tc.typ, tc.raw)
		if got != tc.want || ok != tc.ok {
			t.Errorf("NormalizeNumber(%s, %q) = (%q, %v), want (%q, %v)",
				tc.typ, tc.raw, got, ok, tc.want, tc.ok)
		}
	}
}

// TestNameMatches proves the token rule: one shared token of 3+ characters
// verifies, short tokens and disjoint names do not.
func TestNameMatches(t *testing.T) {
	for _, tc := range []struct {
		resolved, business, display string
		want                        bool
	}{
		{"AKOSUA MENSAH", "Akosua Farms", "", true},
		{"akosua-mensah", "AKOSUA FARMS!", "", true},
		{"K. MENSAH", "Akosua Farms", "Kwame Mensah", true},
		{"KWAME OWUSU", "Akosua Farms", "Ama Serwaa", false},
		{"AM", "Am Farms", "", false},
		{"", "Akosua Farms", "", false},
		{"!!!", "Akosua Farms", "", false},
	} {
		if got := payouts.NameMatches(tc.resolved, tc.business, tc.display); got != tc.want {
			t.Errorf("NameMatches(%q, %q, %q) = %v, want %v",
				tc.resolved, tc.business, tc.display, got, tc.want)
		}
	}
}

// TestSetAccount_FirstSetupVerified drives the whole first setup: Paystack
// resolution, the name check, encryption, the masked response and the audit
// plus security SMS in the same transaction.
func TestSetAccount_FirstSetupVerified(t *testing.T) {
	f := newPayoutFixture(t)
	ctx := context.Background()

	account, err := f.svc.SetAccount(ctx, f.seller, "", payouts.Input{
		Type: "mobile_money", BankCode: "MTN", AccountNumber: "+233241234567",
	})
	if err != nil {
		t.Fatalf("set account: %v", err)
	}
	if account.Status != payouts.StatusVerified || account.NumberMask != "******4567" {
		t.Errorf("account = %+v, want verified/******4567", account)
	}
	if account.BankName != "MTN Mobile Money" || account.AccountName != "AKOSUA MENSAH" {
		t.Errorf("account = %+v, want the fake bank and holder", account)
	}
	if account.CooldownUntil != nil {
		t.Errorf("first setup has a cooldown until %v, want none", account.CooldownUntil)
	}
	if account.RecipientCode == "" {
		t.Error("no recipient code stored for 18b (the contract mapper drops it, the service keeps it)")
	}
	if len(f.provider.Resolved) != 1 || f.provider.Resolved[0].AccountNumber != "0241234567" {
		t.Errorf("resolved = %+v, want the normalized number", f.provider.Resolved)
	}
	if len(f.provider.Recipients) != 1 || f.provider.Recipients[0].Type != "mobile_money" {
		t.Errorf("recipients = %+v, want one mobile_money recipient", f.provider.Recipients)
	}

	// The stored number is ciphertext, never the digits.
	var enc, mask string
	if err := f.pool.QueryRow(ctx,
		`SELECT account_number_enc, account_number_mask FROM seller_payout_accounts WHERE seller_id = $1`,
		f.seller).Scan(&enc, &mask); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc, "v1:") || strings.Contains(enc, "0241234567") {
		t.Errorf("stored ciphertext %q is not ciphertext", enc)
	}
	if mask != "******4567" {
		t.Errorf("stored mask = %q", mask)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM audit_events WHERE action = 'payout_account.set'`); n != 1 {
		t.Errorf("audit rows = %d, want 1", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM river_job WHERE kind = 'notify.sms' AND args->>'template' = 'payout_account_changed'`); n != 1 {
		t.Errorf("security SMS jobs = %d, want 1", n)
	}

	// GET returns the same masked view.
	got, err := f.svc.Get(ctx, f.seller)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.NumberMask != "******4567" || got.Status != payouts.StatusVerified {
		t.Errorf("get = %+v", got)
	}
}

// TestSetAccount_NeedsReviewAndApprove proves a nameless match waits for
// review until an admin approves it, with audit on both steps.
func TestSetAccount_NeedsReviewAndApprove(t *testing.T) {
	f := newPayoutFixture(t)
	ctx := context.Background()
	f.provider.ResolveResult = payments.Account{AccountName: "KWAME OWUSU"}

	account, err := f.svc.SetAccount(ctx, f.seller, "", payouts.Input{
		Type: "mobile_money", BankCode: "MTN", AccountNumber: "0241234567",
	})
	if err != nil {
		t.Fatalf("set account: %v", err)
	}
	if account.Status != payouts.StatusNeedsReview || account.VerifiedAt != nil {
		t.Errorf("account = %+v, want needs_review without a verified_at", account)
	}

	admin, err := users.New(f.pool).Resolve(ctx, auth.Identity{
		UID: "payout-admin", Email: "payout-admin@farmish.test", Provider: "password",
	})
	if err != nil {
		t.Fatal(err)
	}
	approved, err := f.svc.Approve(ctx, admin.ID, f.seller)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if approved.Status != payouts.StatusVerified || approved.VerifiedAt == nil {
		t.Errorf("approved = %+v, want verified with a timestamp", approved)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM audit_events WHERE action = 'payout_account.approve'`); n != 1 {
		t.Errorf("approve audits = %d, want 1", n)
	}
	if _, err := f.svc.Approve(ctx, admin.ID, f.seller); !errors.Is(err, payouts.ErrAlreadyVerified) {
		t.Errorf("second approve err = %v, want ErrAlreadyVerified", err)
	}
	if _, err := f.svc.Approve(ctx, admin.ID, uuid.New()); !errors.Is(err, payouts.ErrNotFound) {
		t.Errorf("approve missing err = %v, want ErrNotFound", err)
	}
}

// TestSetAccount_ChangeStartsCooldown proves the anti-takeover rule: the
// first setup has no cooldown, a changed account waits 48h, and the audit
// carries both masked numbers.
func TestSetAccount_ChangeStartsCooldown(t *testing.T) {
	f := newPayoutFixture(t)
	ctx := context.Background()

	if _, err := f.svc.SetAccount(ctx, f.seller, "", payouts.Input{
		Type: "mobile_money", BankCode: "MTN", AccountNumber: "0241234567",
	}); err != nil {
		t.Fatal(err)
	}
	f.advance(time.Hour)
	changed, err := f.svc.SetAccount(ctx, f.seller, "", payouts.Input{
		Type: "mobile_money", BankCode: "MTN", AccountNumber: "0247654321",
	})
	if err != nil {
		t.Fatalf("change: %v", err)
	}
	if changed.CooldownUntil == nil || !changed.CooldownUntil.Equal(f.now.Add(48*time.Hour)) {
		t.Errorf("cooldown = %v, want now+48h (%v)", changed.CooldownUntil, f.now.Add(48*time.Hour))
	}
	if changed.NumberMask != "******4321" {
		t.Errorf("mask = %q, want ******4321", changed.NumberMask)
	}
	var meta string
	if err := f.pool.QueryRow(ctx,
		`SELECT (metadata->>'old_mask') || '/' || (metadata->>'new_mask') FROM audit_events
		 WHERE action = 'payout_account.set' ORDER BY id DESC LIMIT 1`).Scan(&meta); err != nil {
		t.Fatal(err)
	}
	if meta != "******4567/******4321" {
		t.Errorf("audit masks = %q", meta)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM river_job WHERE kind = 'notify.sms' AND args->>'template' = 'payout_account_changed'`); n != 2 {
		t.Errorf("security SMS jobs = %d, want 2", n)
	}
}

// TestSetAccount_Validation proves bad input never reaches Paystack: unknown
// types, malformed numbers and unknown bank codes are 400s.
func TestSetAccount_Validation(t *testing.T) {
	f := newPayoutFixture(t)
	ctx := context.Background()

	for name, in := range map[string]payouts.Input{
		"unknown type": {Type: "crypto", BankCode: "MTN", AccountNumber: "0241234567"},
		"short number": {Type: "mobile_money", BankCode: "MTN", AccountNumber: "02412345"},
		"letters":      {Type: "mobile_money", BankCode: "MTN", AccountNumber: "024123456a"},
		"unknown bank": {Type: "mobile_money", BankCode: "NOPE", AccountNumber: "0241234567"},
		"short ghipss": {Type: "ghipss", BankCode: "MTN", AccountNumber: "123456789"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := f.svc.SetAccount(ctx, f.seller, "", in); err == nil {
				t.Fatal("want an error")
			} else {
				var invalid *validation.Error
				if !errors.As(err, &invalid) {
					t.Fatalf("err = %v, want a validation error", err)
				}
			}
			if n := f.provider.CallCount("ResolveAccount"); n != 0 {
				t.Errorf("Paystack calls = %d, want 0", n)
			}
		})
	}
}

// TestSetAccount_Unresolvable proves a Paystack refusal becomes
// ErrUnresolvable with nothing stored.
func TestSetAccount_Unresolvable(t *testing.T) {
	f := newPayoutFixture(t)
	ctx := context.Background()
	f.provider.ResolveErr = errors.New("account not found at bank")

	if _, err := f.svc.SetAccount(ctx, f.seller, "", payouts.Input{
		Type: "mobile_money", BankCode: "MTN", AccountNumber: "0241234567",
	}); !errors.Is(err, payouts.ErrUnresolvable) {
		t.Fatalf("err = %v, want ErrUnresolvable", err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM seller_payout_accounts WHERE seller_id = $1`, f.seller); n != 0 {
		t.Errorf("account rows = %d, want 0", n)
	}
}

// TestSetAccount_RequiresProfile proves sellers without a profile cannot
// read or set an account.
func TestSetAccount_RequiresProfile(t *testing.T) {
	f := newPayoutFixture(t)
	ctx := context.Background()
	stranger, err := users.New(f.pool).Resolve(ctx, auth.Identity{
		UID: "payout-stranger", Email: "payout-stranger@farmish.test", Provider: "password",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Get(ctx, stranger.ID); !errors.Is(err, payouts.ErrSellerProfileRequired) {
		t.Errorf("get err = %v, want ErrSellerProfileRequired", err)
	}
	if _, err := f.svc.SetAccount(ctx, stranger.ID, "", payouts.Input{
		Type: "mobile_money", BankCode: "MTN", AccountNumber: "0241234567",
	}); !errors.Is(err, payouts.ErrSellerProfileRequired) {
		t.Errorf("set err = %v, want ErrSellerProfileRequired", err)
	}
	if _, err := f.svc.Get(ctx, f.seller); !errors.Is(err, payouts.ErrNotFound) {
		t.Errorf("get missing err = %v, want ErrNotFound", err)
	}
}

// TestBanks_CachedList proves one Paystack call serves two list calls, and a
// third past the hour fetches again.
func TestBanks_CachedList(t *testing.T) {
	f := newPayoutFixture(t)
	ctx := context.Background()

	for range 2 {
		banks, err := f.svc.ListBanks(ctx, "mobile_money")
		if err != nil {
			t.Fatal(err)
		}
		if len(banks) != 1 || banks[0].Code != "MTN" {
			t.Fatalf("banks = %+v", banks)
		}
	}
	if n := f.provider.CallCount("ListBanks"); n != 1 {
		t.Errorf("Paystack calls = %d, want 1", n)
	}
	f.advance(61 * time.Minute)
	if _, err := f.svc.ListBanks(ctx, "mobile_money"); err != nil {
		t.Fatal(err)
	}
	if n := f.provider.CallCount("ListBanks"); n != 2 {
		t.Errorf("Paystack calls = %d, want 2 after the TTL", n)
	}
	if _, err := f.svc.ListBanks(ctx, "bogus"); err == nil {
		t.Error("bad type: want a validation error")
	} else {
		var invalid *validation.Error
		if !errors.As(err, &invalid) {
			t.Errorf("err = %v, want validation", err)
		}
	}
}

// TestSetAccount_GhipssRecipientType proves bank accounts go out as Paystack
// basilisk recipients.
func TestSetAccount_GhipssRecipientType(t *testing.T) {
	f := newPayoutFixture(t)
	ctx := context.Background()
	f.provider.BanksResult = []payments.Bank{{Name: "Test Bank", Code: "TST", Type: "ghipss"}}

	if _, err := f.svc.SetAccount(ctx, f.seller, "", payouts.Input{
		Type: "ghipss", BankCode: "TST", AccountNumber: "1234567890",
	}); err != nil {
		t.Fatalf("set account: %v", err)
	}
	if len(f.provider.Recipients) != 1 || f.provider.Recipients[0].Type != "basilisk" {
		t.Errorf("recipients = %+v, want one basilisk recipient", f.provider.Recipients)
	}
}
