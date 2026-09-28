package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/auth"
	"github.com/dezmymachine/farmish-backend/internal/auth/authtest"
	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/payments"
	"github.com/dezmymachine/farmish-backend/internal/payments/paystacktest"
	"github.com/dezmymachine/farmish-backend/internal/sellers"
	"github.com/dezmymachine/farmish-backend/internal/users"
)

// Scenarios shared by the Phase 21 observability tests: the audit coverage
// test asserts what they leave in audit_events, the log test asserts what
// they leave in the logs. Each drives the real router, the real webhook
// route and the real workers.

// awaitRefundCall waits for the refund job's Paystack call, then returns
// Paystack's refund id with the payment reference and amount a refund
// webhook needs.
func (f *adminFixture) awaitRefundCall(t *testing.T, orderID uuid.UUID) (paystackID int64, reference string, amount int64) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var raw *string
		err := f.pool.QueryRow(context.Background(),
			`SELECT r.paystack_refund_id, p.reference, r.amount_pesewas
			 FROM refunds r JOIN orders o ON o.id = r.order_id
			 JOIN checkouts c ON c.id = o.checkout_id JOIN payments p ON p.id = c.payment_id
			 WHERE r.order_id = $1`, orderID).Scan(&raw, &reference, &amount)
		if err == nil && raw != nil {
			if err := json.Unmarshal([]byte(*raw), &paystackID); err != nil {
				t.Fatal(err)
			}
			return paystackID, reference, amount
		}
		if time.Now().After(deadline) {
			t.Fatalf("the refund job never called Paystack for order %s (err %v)", orderID, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// disputeIDFor returns the dispute opened on orderID.
func (f *adminFixture) disputeIDFor(t *testing.T, orderID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := f.pool.QueryRow(context.Background(),
		`SELECT id FROM disputes WHERE order_id = $1`, orderID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// resolveRefundBuyer has the admin refund the buyer in full.
func (f *adminFixture) resolveRefundBuyer(t *testing.T, adminTok string, orderID uuid.UUID) {
	t.Helper()
	req := jsonRequest(http.MethodPost, "/v1/admin/disputes/"+f.disputeIDFor(t, orderID).String()+"/resolve", adminTok,
		`{"outcome": "refund_buyer", "note": "The courier confirmed the loss."}`)
	if w := serve(t, f.router, req); w.Code != http.StatusOK {
		t.Fatalf("resolve: %d %s", w.Code, w.Body.String())
	}
}

// refundEvent posts a signed refund webhook and requires a 200.
func (f *adminFixture) refundEvent(t *testing.T, event string, paystackID int64, reference string, amount int64) {
	t.Helper()
	body := refundWebhook(t, event, map[string]any{
		"id": paystackID, "amount": amount, "currency": "GHS", "status": "processed",
		"transaction_reference": reference,
	})
	if w := postWebhook(f.router, body, paystacktest.Sign(paystackSecret, body)); w.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", event, w.Code, w.Body.String())
	}
}

// orderMoneyCreds are the credentials the order scenario used. The log test
// checks none of them reached a log line.
type orderMoneyCreds struct {
	tokens []string
	phones []string
}

// phoneUserWithProfile signs a fresh user in by phone and gives them a
// seller profile, returning their token, id and number.
func (f *adminFixture) phoneUserWithProfile(t *testing.T, name string) (token string, id uuid.UUID, phone string) {
	t.Helper()
	ctx := context.Background()
	pu := authtest.PhoneUser(t)
	identity, err := authtest.Firebase(t).Verify(ctx, pu.Token)
	if err != nil {
		t.Fatal(err)
	}
	u, err := users.New(f.pool).Resolve(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sellers.New(f.pool, nil, nil).UpsertMine(ctx, u.ID, sellers.ProfileInput{
		BusinessName: name + " farms", Region: "Ashanti", District: "Kumasi Metro",
	}); err != nil {
		t.Fatal(err)
	}
	return pu.Token, u.ID, pu.Phone
}

// driveOrderMoney runs, on f:
//   - a mismatched charge (rejected and audited)
//   - a paid order that is disputed and refunded, settled by webhook
//   - a second disputed order whose refund fails by webhook
//
// The seller and buyer sign in by phone, so the notifications their orders
// trigger reach a recipient with a number to mask.
func driveOrderMoney(t *testing.T, f *adminFixture) orderMoneyCreds {
	t.Helper()
	fb := authtest.Firebase(t)
	_, adminTok := authAdmin(t, f.pool, fb)
	sellerTok, sellerID, sellerPhone := f.phoneUserWithProfile(t, "money-seller")
	buyerTok, _, buyerPhone := f.phoneUserWithProfile(t, "money-buyer")
	phones := []string{sellerPhone, buyerPhone}

	// A charge that does not match the payment is never settled.
	listingID := publishQuoteListing(t, f.router, f.pool, f.store, sellerTok, "Mismatch Cowpea")
	req := jsonRequest(http.MethodPost, "/v1/checkout", buyerTok,
		`{"lines": [{"listingId": "`+listingID.String()+`", "quantity": 1}],
		  "delivery": [{"sellerId": "`+sellerID.String()+`", "method": "pickup"}]}`)
	req.Header.Set("Idempotency-Key", uuid.NewString())
	w := serve(t, f.router, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("checkout: %d %s", w.Code, w.Body.String())
	}
	var created api.CheckoutCreated
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	wrong := paystacktest.ChargeSuccessBody(created.Reference, created.Quote.Charge.Amount+1, nextPaystackEventID())
	if w := postWebhook(f.router, wrong, paystacktest.Sign(paystackSecret, wrong)); w.Code != http.StatusOK {
		t.Fatalf("mismatch webhook: %d %s", w.Code, w.Body.String())
	}

	// Refund settled.
	first := f.disputedOrder(t, sellerTok, buyerTok, sellerID)
	f.resolveRefundBuyer(t, adminTok, first)
	id, reference, amount := f.awaitRefundCall(t, first)
	f.refundEvent(t, "refund.processed", id, reference, amount)

	// Refund failed.
	second := f.disputedOrder(t, sellerTok, buyerTok, sellerID)
	f.resolveRefundBuyer(t, adminTok, second)
	id, reference, amount = f.awaitRefundCall(t, second)
	f.refundEvent(t, "refund.failed", id, reference, amount)

	return orderMoneyCreds{
		tokens: []string{adminTok, sellerTok, buyerTok},
		phones: phones,
	}
}

// payoutCreds are the values the payout scenario must keep out of logs.
type payoutCreds struct {
	accountNumber string
}

// drivePayouts runs, on f:
//   - a payout account that needs review, then is approved
//   - a verified account, a payout that succeeds by webhook
//   - a second payout that fails by webhook
func drivePayouts(t *testing.T, f *execPayoutFixture) payoutCreds {
	t.Helper()
	ctx := context.Background()

	// needs_review: the resolved name does not match the business name.
	f.provider.ResolveResult = payments.Account{AccountName: "KWAME OWUSU"}
	reviewed := f.sellerWithAccount(t, "reviewed", "cov-reviewed-seller")
	f.verifier.mu.Lock()
	f.verifier.ids["admin"] = stubIdentity("cov-admin", "cov-admin@farmish.test", time.Now())
	f.verifier.mu.Unlock()
	admin, err := users.New(f.pool).Resolve(ctx, auth.Identity{
		UID: "cov-admin", Email: "cov-admin@farmish.test", Provider: "password",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pay.Approve(ctx, admin.ID, reviewed); err != nil {
		t.Fatalf("approve: %v", err)
	}

	f.provider.ResolveResult = payments.Account{AccountName: "AKOSUA MENSAH"}
	sellerID := f.sellerWithAccount(t, "seller", "cov-seller")
	transfer := func(event, reference string, amount int64) {
		t.Helper()
		raw, sig := signTransfer(paystackSecret, event, map[string]any{
			"id": nextPaystackEventID(), "reference": reference, "amount": amount, "currency": "GHS",
		})
		req := jsonRequest(http.MethodPost, "/v1/webhooks/paystack", "", string(raw))
		req.Header.Set("x-paystack-signature", sig)
		if w := serve(t, f.router, req); w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", event, w.Code, w.Body.String())
		}
	}

	f.releaseOrder(t, sellerID, 5000)
	first, err := f.pay.ExecuteSeller(ctx, sellerID)
	if err != nil || first == nil {
		t.Fatalf("execute: %v, %v", first, err)
	}
	if _, err := f.pay.SendPayout(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	transfer("transfer.success", first.Reference, first.AmountPesewas)

	f.releaseOrder(t, sellerID, 5000)
	second, err := f.pay.ExecuteSeller(ctx, sellerID)
	if err != nil || second == nil {
		t.Fatalf("second execute: %v, %v", second, err)
	}
	if _, err := f.pay.SendPayout(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	transfer("transfer.failed", second.Reference, second.AmountPesewas)

	return payoutCreds{accountNumber: "0241234567"}
}

// waitJobsIdle waits until no job is available or running, so the
// notifications a scenario enqueued have been sent (and logged) before a test
// inspects the result. Scheduled jobs are not waited for: a refund job
// snoozes until its webhook arrives.
func waitJobsIdle(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var live int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM river_job WHERE state IN ('available', 'running')`).Scan(&live); err != nil {
			t.Fatal(err)
		}
		if live == 0 {
			return
		}
		if time.Now().After(deadline) {
			rows, _ := pool.Query(context.Background(),
				`SELECT kind, state::text, scheduled_at::text, coalesce(errors::text, '') FROM river_job
				 WHERE state IN ('available', 'running')`)
			var dump string
			for rows != nil && rows.Next() {
				var kind, state, at, errs string
				_ = rows.Scan(&kind, &state, &at, &errs)
				dump += fmt.Sprintf("\n  %s %s at %s %.300s", kind, state, at, errs)
			}
			t.Fatalf("%d jobs still live after 30s:%s", live, dump)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
