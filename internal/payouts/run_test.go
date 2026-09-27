package payouts_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/auth"
	"github.com/dezmymachine/farmish-backend/internal/crypto"
	"github.com/dezmymachine/farmish-backend/internal/database"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/jobs"
	"github.com/dezmymachine/farmish-backend/internal/ledger"
	"github.com/dezmymachine/farmish-backend/internal/money"
	"github.com/dezmymachine/farmish-backend/internal/notify"
	"github.com/dezmymachine/farmish-backend/internal/payments"
	"github.com/dezmymachine/farmish-backend/internal/payments/fake"
	"github.com/dezmymachine/farmish-backend/internal/payouts"
	"github.com/dezmymachine/farmish-backend/internal/sellers"
	"github.com/dezmymachine/farmish-backend/internal/users"
)

// execFixture is a payouts service with a real ledger, a scripted Paystack,
// a controllable clock, a captured log and an insert-only job client. Tests
// drive ExecuteSeller/SendPayout/the webhook handlers directly, so each step
// is asserted without a River worker racing it.
type execFixture struct {
	pool     *pgxpool.Pool
	svc      *payouts.Service
	ledger   *ledger.Ledger
	provider *fake.Provider
	logs     *bytes.Buffer
	now      time.Time
	seller   uuid.UUID
	buyer    uuid.UUID
}

func newExecFixture(t *testing.T) *execFixture {
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
	mk := func(uid string) uuid.UUID {
		user, err := users.New(pool).Resolve(ctx, auth.Identity{
			UID: uid, Email: uid + "@farmish.test", Provider: "password",
		})
		if err != nil {
			t.Fatal(err)
		}
		return user.ID
	}
	seller, buyer := mk("exec-seller"), mk("exec-buyer")
	sellersSvc := sellers.New(pool, crypter, nil)
	if _, err := sellersSvc.UpsertMine(ctx, seller, sellers.ProfileInput{
		BusinessName: "Akosua Farms", Region: "Ashanti", District: "Kumasi Metro",
	}); err != nil {
		t.Fatalf("create profile: %v", err)
	}
	provider := fake.New()
	provider.BanksResult = []payments.Bank{{Name: "MTN Mobile Money", Code: "MTN", Type: "mobile_money"}}
	provider.ResolveResult = payments.Account{AccountName: "AKOSUA MENSAH"}
	provider.RecipientResult = payments.TransferRecipient{RecipientCode: "RCP_EXEC"}
	f := &execFixture{
		pool: pool, ledger: ledger.New(), provider: provider, logs: &bytes.Buffer{},
		now: time.Now().UTC().Truncate(time.Second), seller: seller, buyer: buyer,
	}
	provider.Now = func() time.Time { return f.now }
	log := slog.New(slog.NewJSONHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	svc := payouts.New(pool, crypter, provider, sellersSvc)
	svc.Now = func() time.Time { return f.now }
	svc.AttachLedger(f.ledger)
	svc.AttachLogger(log)
	svc.Configure(2000, 0)
	reg := jobs.NewRegistry()
	payouts.RegisterJobs(reg, svc, log)
	notify.Register(reg, notify.NewWorker(notify.LogOnly{Log: log}, log, pool))
	client, err := jobs.NewClient(pool, reg, log, jobs.Options{Work: false})
	if err != nil {
		t.Fatal(err)
	}
	svc.AttachJobClient(client)
	f.svc = svc
	// Every execution test needs a verified account; individual tests change
	// it (cooldown, needs_review) through SetAccount again.
	if _, err := svc.SetAccount(ctx, seller, "", payouts.Input{
		Type: "mobile_money", BankCode: "MTN", AccountNumber: "0241234567",
	}); err != nil {
		t.Fatalf("set account: %v", err)
	}
	return f
}

func (f *execFixture) advance(d time.Duration) { f.now = f.now.Add(d) }

func (f *execFixture) count(t *testing.T, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := f.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// releaseOrder writes a checkout plus a completed order and posts its
// checkout_paid and escrow_release legs, so the seller's payable grows by
// base minus commission.
func (f *execFixture) releaseOrder(t *testing.T, subtotal, delivery int64) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	base := subtotal + delivery
	commission, err := money.Commission(subtotal, 500)
	if err != nil {
		t.Fatal(err)
	}
	var checkoutID uuid.UUID
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO checkouts (buyer_id, idempotency_key, request_hash, base_pesewas,
		                        processing_fee_pesewas, charge_pesewas, status, expires_at)
		 VALUES ($1, $2, 'exec-test', $3, 0, $3, 'paid', now() + interval '30 minutes')
		 RETURNING id`, f.buyer, uuid.New(), base).Scan(&checkoutID); err != nil {
		t.Fatal(err)
	}
	var orderID uuid.UUID
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO orders (checkout_id, buyer_id, seller_id, status, escrow_state,
		                     subtotal_pesewas, delivery_fee_pesewas, base_pesewas,
		                     commission_rate_bps, commission_pesewas, delivery_method)
		 VALUES ($1, $2, $3, 'completed', 'released', $4, $5, $6, 500, $7, 'pickup')
		 RETURNING id`,
		checkoutID, f.buyer, f.seller, subtotal, delivery, base, commission).Scan(&orderID); err != nil {
		t.Fatal(err)
	}
	if err := database.InTx(ctx, f.pool, func(tx pgx.Tx) error {
		// A realistic charge with a nonzero Paystack fee (the builder
		// refuses zero legs).
		if err := f.ledger.Post(ctx, tx, "checkout_paid", orderID.String(),
			ledger.CheckoutPaid(base+150, base, 100, ledger.CheckoutOrder{OrderID: orderID, Base: base})...); err != nil {
			return err
		}
		return f.ledger.Post(ctx, tx, "escrow_release", orderID.String(),
			ledger.EscrowRelease(orderID, f.seller, base, commission)...)
	}); err != nil {
		t.Fatal(err)
	}
	return orderID
}

// payoutRow reads a payout's status and failure reason.
func (f *execFixture) payoutRow(t *testing.T, payoutID uuid.UUID) (status string, failure *string) {
	t.Helper()
	if err := f.pool.QueryRow(context.Background(),
		`SELECT status, failure_reason FROM payouts WHERE id = $1`, payoutID).Scan(&status, &failure); err != nil {
		t.Fatal(err)
	}
	return status, failure
}

// deliverTransfer runs a transfer webhook handler in its own transaction,
// the way payments.Service.HandleWebhook would.
func (f *execFixture) deliver(t *testing.T, h func(context.Context, pgx.Tx, json.RawMessage) (string, error), body []byte) string {
	t.Helper()
	var outcome string
	if err := database.InTx(context.Background(), f.pool, func(tx pgx.Tx) error {
		out, err := h(context.Background(), tx, json.RawMessage(body))
		outcome = out
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return outcome
}

func transferEvent(reference string, amount int64, fields map[string]any) []byte {
	base := map[string]any{"reference": reference, "amount": amount, "currency": "GHS"}
	for k, v := range fields {
		base[k] = v
	}
	raw, err := json.Marshal(base)
	if err != nil {
		panic(err)
	}
	return raw
}

// executeNow queues one payout for the seller and fails the test on error.
// A nil return means nothing was payable (a skip the test did not expect).
func (f *execFixture) executeNow(t *testing.T) payouts.Payout {
	t.Helper()
	payout, err := f.svc.ExecuteSeller(context.Background(), f.seller)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if payout == nil {
		t.Fatal("execute queued nothing, want a payout")
	}
	return *payout
}

// TestPayout_EndToEnd walks a completed order to a settled payout: payable
// → execute_all → queued → send → pending → transfer.success → success,
// with every ledger leg balanced.
func TestPayout_EndToEnd(t *testing.T) {
	f := newExecFixture(t)
	ctx := context.Background()
	f.releaseOrder(t, 5000, 0) // payable 4750 after the 250 commission

	if n, err := f.svc.ExecuteAll(ctx); err != nil || n != 1 {
		t.Fatalf("execute_all = %d, %v; want 1", n, err)
	}
	var queued payouts.Payout
	if err := f.pool.QueryRow(ctx,
		`SELECT id FROM payouts WHERE seller_id = $1 AND status = 'queued'`, f.seller).Scan(&queued.ID); err != nil {
		t.Fatal(err)
	}
	// Re-run while queued: the one-in-flight guard skips.
	if again, err := f.svc.ExecuteSeller(ctx, f.seller); err != nil || again != nil {
		t.Fatalf("second execute = %v, %v; want a skip", again, err)
	}

	payoutID := queued.ID
	if next, err := f.svc.SendPayout(ctx, payoutID); err != nil || next <= 0 {
		t.Fatalf("send = %v, %v; want a re-check duration", next, err)
	}
	if f.provider.CallCount("InitiateTransfer") != 1 {
		t.Fatalf("transfer calls = %d, want 1", f.provider.CallCount("InitiateTransfer"))
	}
	in := f.provider.Transferred[0]
	if in.AmountPesewas != 4750 || in.RecipientCode != "RCP_EXEC" || in.Reason != "Farmish payout" {
		t.Errorf("transfer input = %+v", in)
	}
	if status, _ := f.payoutRow(t, payoutID); status != payouts.PayoutPending {
		t.Fatalf("payout = %s, want pending", status)
	}

	body := transferEvent(f.referenceOf(t, payoutID), 4750, nil)
	if outcome := f.deliver(t, f.svc.OnTransferSuccess, body); outcome != payments.OutcomeProcessed {
		t.Fatalf("webhook outcome = %s, want processed", outcome)
	}
	if status, _ := f.payoutRow(t, payoutID); status != payouts.PayoutSuccess {
		t.Errorf("payout = %s, want success", status)
	}
	// A replay settles nothing twice.
	if outcome := f.deliver(t, f.svc.OnTransferSuccess, body); outcome != payments.OutcomeIgnored {
		t.Errorf("replay outcome = %s, want ignored", outcome)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM ledger_transactions WHERE kind = 'payout_succeeded'`); n != 1 {
		t.Errorf("payout_succeeded postings = %d, want 1", n)
	}

	balance, err := f.svc.Balance(ctx, f.seller)
	if err != nil {
		t.Fatal(err)
	}
	if balance.Available != 0 || balance.InFlight != 0 || balance.PaidOut != 4750 {
		t.Errorf("balance = %+v, want available 0, in-flight 0, paid out 4750", balance)
	}
	var escrow, clearing int64
	if err := f.pool.QueryRow(ctx, `SELECT COALESCE(SUM(amount),0) FROM ledger_entries le
		JOIN ledger_accounts la ON la.id = le.account_id WHERE la.code = 'escrow'`).Scan(&escrow); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT COALESCE(SUM(amount),0) FROM ledger_entries le
		JOIN ledger_accounts la ON la.id = le.account_id WHERE la.code = 'payout_clearing'`).Scan(&clearing); err != nil {
		t.Fatal(err)
	}
	if escrow != 0 || clearing != 0 {
		t.Errorf("escrow = %d, clearing = %d; want 0 and 0 (released and paid out)", escrow, clearing)
	}
}

func (f *execFixture) referenceOf(t *testing.T, payoutID uuid.UUID) string {
	t.Helper()
	var reference string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT reference FROM payouts WHERE id = $1`, payoutID).Scan(&reference); err != nil {
		t.Fatal(err)
	}
	return reference
}

// TestPayout_FailedRestoresBalance proves a failed transfer re-credits the
// payable, fails the payout and alerts.
func TestPayout_FailedRestoresBalance(t *testing.T) {
	f := newExecFixture(t)
	ctx := context.Background()
	f.releaseOrder(t, 5000, 0)
	payout := f.executeNow(t)
	if _, err := f.svc.SendPayout(ctx, payout.ID); err != nil {
		t.Fatal(err)
	}

	body := transferEvent(payout.Reference, 4750, nil)
	if outcome := f.deliver(t, f.svc.OnTransferFailed, body); outcome != payments.OutcomeProcessed {
		t.Fatalf("webhook outcome = %s, want processed", outcome)
	}
	if status, reason := f.payoutRow(t, payout.ID); status != payouts.PayoutFailed || reason == nil {
		t.Errorf("payout = %s/%v, want failed with a reason", status, reason)
	}
	balance, err := f.svc.Balance(ctx, f.seller)
	if err != nil {
		t.Fatal(err)
	}
	if balance.Available != 4750 {
		t.Errorf("available = %d, want the full 4750 back", balance.Available)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM ledger_transactions WHERE kind = 'payout_failed' AND reference = $1`, payout.Reference); n != 1 {
		t.Errorf("payout_failed postings = %d, want 1", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM audit_events WHERE action = 'payout.failed' AND target_id = $1`, payout.ID.String()); n != 1 {
		t.Errorf("failure audits = %d, want 1", n)
	}
	if !strings.Contains(f.logs.String(), `"level":"ERROR"`) {
		t.Errorf("no Error log for the failed payout: %s", f.logs.String())
	}
	// A replay of the same failure changes nothing.
	if outcome := f.deliver(t, f.svc.OnTransferFailed, body); outcome != payments.OutcomeIgnored {
		t.Errorf("replay outcome = %s, want ignored", outcome)
	}

	// A late success after the failure goes to an admin, never to the ledger.
	late := transferEvent(payout.Reference, 4750, nil)
	if outcome := f.deliver(t, f.svc.OnTransferSuccess, late); outcome != payments.OutcomeProcessed {
		t.Errorf("late success outcome = %s, want processed", outcome)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM ledger_transactions WHERE kind = 'payout_succeeded'`); n != 0 {
		t.Errorf("success postings = %d, want 0 after a failure", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM audit_events WHERE action = 'payout.late_success'`); n != 1 {
		t.Errorf("late-success audits = %d, want 1", n)
	}
}

// TestPayout_NoPayoutDuringCooldown proves a changed account waits 48h.
func TestPayout_NoPayoutDuringCooldown(t *testing.T) {
	f := newExecFixture(t)
	ctx := context.Background()
	f.releaseOrder(t, 5000, 0)

	// Change the account: the cooldown starts now.
	if _, err := f.svc.SetAccount(ctx, f.seller, "", payouts.Input{
		Type: "mobile_money", BankCode: "MTN", AccountNumber: "0247654321",
	}); err != nil {
		t.Fatal(err)
	}
	if payout, err := f.svc.ExecuteSeller(ctx, f.seller); err != nil || payout != nil {
		t.Fatalf("execute in cooldown = %v, %v; want a skip", payout, err)
	}
	f.advance(49 * time.Hour)
	if payout, err := f.svc.ExecuteSeller(ctx, f.seller); err != nil || payout == nil {
		t.Fatalf("execute after cooldown = %v, %v; want a payout", payout, err)
	}
}

// TestPayout_BelowMinimumSkipped proves 1999 never pays while 2000 does.
func TestPayout_BelowMinimumSkipped(t *testing.T) {
	f := newExecFixture(t)
	ctx := context.Background()
	// 1999 at 500 bps: commission 100, payable 1899.
	f.releaseOrder(t, 1999, 0)
	if payout, err := f.svc.ExecuteSeller(ctx, f.seller); err != nil || payout != nil {
		t.Fatalf("execute at 1899 = %v, %v; want a skip", payout, err)
	}
	// One more release takes the payable to 2000+.
	f.releaseOrder(t, 200, 0) // commission 10, payable +190 = 2089
	payout := f.executeNow(t)
	if payout.AmountPesewas != 2089 {
		t.Errorf("payout amount = %d, want the full 2089", payout.AmountPesewas)
	}
}

// TestPayout_OneInFlight proves concurrent executions queue exactly one
// payout for the same balance.
func TestPayout_OneInFlight(t *testing.T) {
	f := newExecFixture(t)
	ctx := context.Background()
	f.releaseOrder(t, 5000, 0)

	const runners = 5
	results := make([]*payouts.Payout, runners)
	errs := make([]error, runners)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = f.svc.ExecuteSeller(ctx, f.seller)
		}(i)
	}
	wg.Wait()
	queued := 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("runner %d: %v", i, err)
		}
		if results[i] != nil {
			queued++
		}
	}
	if queued != 1 {
		t.Errorf("queued payouts = %d, want exactly 1", queued)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM payouts WHERE seller_id = $1`, f.seller); n != 1 {
		t.Errorf("payout rows = %d, want 1", n)
	}
}

// TestPayout_NeedsReviewAccountSkipped proves unverified accounts never pay.
func TestPayout_NeedsReviewAccountSkipped(t *testing.T) {
	f := newExecFixture(t)
	ctx := context.Background()
	f.releaseOrder(t, 5000, 0)
	f.provider.ResolveResult = payments.Account{AccountName: "SOMEONE ELSE"}
	if _, err := f.svc.SetAccount(ctx, f.seller, "", payouts.Input{
		Type: "mobile_money", BankCode: "MTN", AccountNumber: "0241234567",
	}); err != nil {
		t.Fatal(err)
	}
	if payout, err := f.svc.ExecuteSeller(ctx, f.seller); err != nil || payout != nil {
		t.Fatalf("execute needs_review = %v, %v; want a skip", payout, err)
	}
}

// TestPayout_SendRetryAdoptsExisting proves the ambiguous send never pays
// twice: Paystack created the transfer but the response was lost, so the
// retry adopts it via verify.
func TestPayout_SendRetryAdoptsExisting(t *testing.T) {
	f := newExecFixture(t)
	ctx := context.Background()
	f.releaseOrder(t, 5000, 0)
	payout := f.executeNow(t)

	f.provider.TransferErrAfterRecord = errors.New("timeout after record")
	next, err := f.svc.SendPayout(ctx, payout.ID)
	f.provider.TransferErrAfterRecord = nil
	if err != nil || next <= 0 {
		t.Fatalf("send = %v, %v; want a re-check", next, err)
	}
	// Well past any grace: the retry still sends nothing new.
	f.advance(3 * time.Hour)
	if next, err := f.svc.SendPayout(ctx, payout.ID); err != nil || next <= 0 {
		t.Fatalf("retry = %v, %v; want a re-check", next, err)
	}
	if n := f.provider.CallCount("InitiateTransfer"); n != 1 {
		t.Fatalf("transfer calls = %d, want exactly 1", n)
	}
	if status, _ := f.payoutRow(t, payout.ID); status != payouts.PayoutPending {
		t.Fatalf("payout = %s, want pending with the adopted transfer", status)
	}

	// Paystack settles it; the next poll books it.
	f.provider.SetTransferStatus(payout.Reference, "success")
	if next, err := f.svc.SendPayout(ctx, payout.ID); err != nil || next != 0 {
		t.Errorf("after settlement next = %v, err = %v; want 0", next, err)
	}
	if status, _ := f.payoutRow(t, payout.ID); status != payouts.PayoutSuccess {
		t.Errorf("payout = %s, want success", status)
	}
}

// TestPayout_ReconcileStuck proves the daily check settles a webhook-less
// pending payout, and audits one Paystack never heard of.
func TestPayout_ReconcileStuck(t *testing.T) {
	f := newExecFixture(t)
	ctx := context.Background()
	f.releaseOrder(t, 5000, 0)
	payout := f.executeNow(t)
	if _, err := f.svc.SendPayout(ctx, payout.ID); err != nil {
		t.Fatal(err)
	}

	f.advance(25 * time.Hour)
	f.provider.SetTransferStatus(payout.Reference, "success")
	if n, err := f.svc.ReconcileStuck(ctx); err != nil || n != 1 {
		t.Fatalf("reconcile = %d, %v; want 1", n, err)
	}
	if status, _ := f.payoutRow(t, payout.ID); status != payouts.PayoutSuccess {
		t.Errorf("payout = %s, want success", status)
	}

	// A pending transfer Paystack never settles stays pending, with an audit
	// row for an admin.
	f.releaseOrder(t, 5000, 0)
	stuck := f.executeNow(t)
	if _, err := f.svc.SendPayout(ctx, stuck.ID); err != nil {
		t.Fatal(err)
	}
	f.provider.SetTransferStatus(stuck.Reference, "pending")
	f.advance(25 * time.Hour)
	if n, err := f.svc.ReconcileStuck(ctx); err != nil || n != 0 {
		t.Fatalf("reconcile = %d, %v; want 0 settled", n, err)
	}
	if status, _ := f.payoutRow(t, stuck.ID); status != payouts.PayoutPending {
		t.Errorf("payout = %s, want still pending", status)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM audit_events WHERE action = 'payout.stuck' AND target_id = $1`, stuck.ID.String()); n != 1 {
		t.Errorf("stuck audits = %d, want 1", n)
	}
}

// TestPayout_RetryFlow proves failed → a fresh payout row for the
// re-credited balance, and that live or unknown payouts refuse.
func TestPayout_RetryFlow(t *testing.T) {
	f := newExecFixture(t)
	ctx := context.Background()
	f.releaseOrder(t, 5000, 0)
	payout := f.executeNow(t)
	if _, err := f.svc.SendPayout(ctx, payout.ID); err != nil {
		t.Fatal(err)
	}
	body := transferEvent(payout.Reference, 4750, nil)
	if outcome := f.deliver(t, f.svc.OnTransferFailed, body); outcome != payments.OutcomeProcessed {
		t.Fatalf("fail webhook = %s", outcome)
	}

	admin, err := users.New(f.pool).Resolve(ctx, auth.Identity{
		UID: "payout-retry-admin", Email: "payout-retry-admin@farmish.test", Provider: "password",
	})
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := f.svc.RetryPayout(ctx, admin.ID, payout.ID)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if fresh.ID == payout.ID || fresh.Reference == payout.Reference {
		t.Errorf("retry reused the failed row %+v", fresh)
	}
	if fresh.AmountPesewas != 4750 || fresh.Status != payouts.PayoutQueued {
		t.Errorf("retry = %+v, want a queued 4750", fresh)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM audit_events WHERE action = 'payout.retry'`); n != 1 {
		t.Errorf("retry audits = %d, want 1", n)
	}

	if _, err := f.svc.RetryPayout(ctx, admin.ID, fresh.ID); !errors.Is(err, payouts.ErrRetryNotAllowed) {
		t.Errorf("retry live err = %v, want ErrRetryNotAllowed", err)
	}
	if _, err := f.svc.RetryPayout(ctx, admin.ID, uuid.New()); !errors.Is(err, payouts.ErrPayoutNotFound) {
		t.Errorf("retry unknown err = %v, want ErrPayoutNotFound", err)
	}
}

// TestPayout_ReversedSettles proves a reversal re-credits like a failure and
// can be retried.
func TestPayout_ReversedSettles(t *testing.T) {
	f := newExecFixture(t)
	ctx := context.Background()
	f.releaseOrder(t, 5000, 0)
	payout := f.executeNow(t)
	if _, err := f.svc.SendPayout(ctx, payout.ID); err != nil {
		t.Fatal(err)
	}
	body := transferEvent(payout.Reference, 4750, nil)
	if outcome := f.deliver(t, f.svc.OnTransferReversed, body); outcome != payments.OutcomeProcessed {
		t.Fatalf("reversed webhook = %s", outcome)
	}
	if status, _ := f.payoutRow(t, payout.ID); status != payouts.PayoutReversed {
		t.Errorf("payout = %s, want reversed", status)
	}
	balance, err := f.svc.Balance(ctx, f.seller)
	if err != nil {
		t.Fatal(err)
	}
	if balance.Available != 4750 {
		t.Errorf("available = %d, want 4750", balance.Available)
	}
}

// TestPayout_SendDefiniteRejection proves a 4xx fails fast with no retry.
func TestPayout_SendDefiniteRejection(t *testing.T) {
	f := newExecFixture(t)
	ctx := context.Background()
	f.releaseOrder(t, 5000, 0)
	payout := f.executeNow(t)

	f.provider.TransferErr = fmt.Errorf("%w: invalid recipient", payments.ErrRejected)
	if next, err := f.svc.SendPayout(ctx, payout.ID); err != nil || next != 0 {
		t.Errorf("rejected send next = %v, err = %v; want 0", next, err)
	}
	if status, reason := f.payoutRow(t, payout.ID); status != payouts.PayoutFailed || reason == nil {
		t.Errorf("payout = %s/%v, want failed with a reason", status, reason)
	}
	if n := f.provider.CallCount("InitiateTransfer"); n != 1 {
		t.Errorf("transfer calls = %d, want 1 (no retry)", n)
	}
}
