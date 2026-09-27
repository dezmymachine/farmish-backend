package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/dezmymachine/farmish-backend/internal/payments/paystacktest"
)

// refundWebhook builds a signed-ready refund event body.
func refundWebhook(t *testing.T, event string, data map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"event": event, "data": data})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// rejectAndAwaitRefund rejects a paid order through the real endpoint and
// waits for the real River worker to make the refund's Paystack call, so the
// refund is pending with Paystack's id stored.
func rejectAndAwaitRefund(t *testing.T, f *fulfilmentFixture, sellerTok string, orderID uuid.UUID) (refundID uuid.UUID, paystackID int64, reference string, amount int64) {
	t.Helper()
	ctx := context.Background()
	req := jsonRequest(http.MethodPost, "/v1/seller/orders/"+orderID.String()+"/reject", sellerTok,
		`{"reason": "The goods spoiled before the buyer could collect."}`)
	if w := serve(t, f.router, req); w.Code != http.StatusOK {
		t.Fatalf("reject: %d %s", w.Code, w.Body.String())
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		var ps *string
		err := f.pool.QueryRow(ctx,
			`SELECT r.id, r.paystack_refund_id, p.reference, r.amount_pesewas
			 FROM refunds r JOIN orders o ON o.id = r.order_id
			 JOIN checkouts c ON c.id = o.checkout_id JOIN payments p ON p.id = c.payment_id
			 WHERE r.order_id = $1`, orderID).Scan(&refundID, &ps, &reference, &amount)
		if err == nil && ps != nil {
			if err := json.Unmarshal([]byte(*ps), &paystackID); err != nil {
				t.Fatal(err)
			}
			return refundID, paystackID, reference, amount
		}
		if time.Now().After(deadline) {
			t.Fatalf("refund job never called Paystack (err %v)", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (f *fulfilmentFixture) scalar(t *testing.T, query string, args ...any) string {
	t.Helper()
	var v string
	if err := f.pool.QueryRow(context.Background(), query, args...).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestWebhook_RefundEventsRouted is Major 5: the four refund events travel
// the real signed webhook endpoint, with the production registration, and a
// replay is deduped by webhook_events.
func TestWebhook_RefundEventsRouted(t *testing.T) {
	f := newFulfilmentFixture(t)
	sellerTok, sellerID := f.userWithProfile(t, "refund-route-seller@example.com")
	buyerTok, _ := f.userWithProfile(t, "refund-route-buyer@example.com")

	// refund.processed, with Paystack's id: settles once.
	orderID, _ := f.createPaidOrder(t, sellerTok, buyerTok, sellerID)
	refundID, paystackID, reference, amount := rejectAndAwaitRefund(t, f, sellerTok, orderID)
	processed := refundWebhook(t, "refund.processed", map[string]any{
		"id": paystackID, "amount": amount, "currency": "GHS", "status": "processed",
		"transaction_reference": reference,
	})
	sig := paystacktest.Sign(paystackSecret, processed)
	if w := postWebhook(f.router, processed, sig); w.Code != http.StatusOK {
		t.Fatalf("refund.processed: %d %s", w.Code, w.Body.String())
	}
	if got := f.scalar(t, `SELECT status FROM refunds WHERE id = $1`, refundID); got != "processed" {
		t.Fatalf("refund = %s, want processed", got)
	}
	if got := f.scalar(t, `SELECT escrow_state FROM orders WHERE id = $1`, orderID); got != "refunded" {
		t.Errorf("escrow = %s, want refunded", got)
	}
	if got := f.scalar(t, `SELECT outcome FROM webhook_events WHERE event_type = 'refund.processed'`); got != "processed" {
		t.Errorf("webhook outcome = %s, want processed", got)
	}
	// Replay: deduped before any handler runs.
	if w := postWebhook(f.router, processed, sig); w.Code != http.StatusOK {
		t.Fatalf("replay: %d %s", w.Code, w.Body.String())
	}
	if got := f.scalar(t, `SELECT COUNT(*)::text FROM webhook_events WHERE event_type = 'refund.processed'`); got != "1" {
		t.Errorf("webhook_events rows = %s, want 1 after the replay", got)
	}
	if got := f.scalar(t, `SELECT COUNT(*)::text FROM ledger_transactions WHERE kind = 'order_refund'`); got != "1" {
		t.Errorf("order_refund postings = %s, want 1", got)
	}

	// refund.pending with no top-level id (Paystack's refund payload shape):
	// keyed by refund_reference, matched by the single-candidate fallback.
	second, _ := f.createPaidOrder(t, sellerTok, buyerTok, sellerID)
	secondRefund, secondPaystackID, secondRef, secondAmount := rejectAndAwaitRefund(t, f, sellerTok, second)
	pending := refundWebhook(t, "refund.pending", map[string]any{
		"refund_reference": "RF-SECOND", "amount": secondAmount, "currency": "GHS", "status": "pending",
		"transaction_reference": secondRef,
	})
	if w := postWebhook(f.router, pending, paystacktest.Sign(paystackSecret, pending)); w.Code != http.StatusOK {
		t.Fatalf("refund.pending: %d %s", w.Code, w.Body.String())
	}
	if got := f.scalar(t, `SELECT event_key FROM webhook_events WHERE event_type = 'refund.pending'`); got != "refund.pending:ref:RF-SECOND" {
		t.Errorf("event key = %s, want refund.pending:ref:RF-SECOND", got)
	}
	if got := f.scalar(t, `SELECT status FROM refunds WHERE id = $1`, secondRefund); got != "pending" {
		t.Errorf("after refund.pending = %s, want pending", got)
	}

	// refund.failed: routed, fails the refund, escrow untouched.
	failed := refundWebhook(t, "refund.failed", map[string]any{
		"id": secondPaystackID, "amount": secondAmount, "currency": "GHS", "status": "failed",
		"transaction_reference": secondRef,
	})
	if w := postWebhook(f.router, failed, paystacktest.Sign(paystackSecret, failed)); w.Code != http.StatusOK {
		t.Fatalf("refund.failed: %d %s", w.Code, w.Body.String())
	}
	if got := f.scalar(t, `SELECT status FROM refunds WHERE id = $1`, secondRefund); got != "failed" {
		t.Errorf("after refund.failed = %s, want failed", got)
	}
	if got := f.scalar(t, `SELECT escrow_state FROM orders WHERE id = $1`, second); got != "refund_pending" {
		t.Errorf("escrow after failure = %s, want refund_pending", got)
	}

	// refund.processing is routed to the informational handler too.
	processing := refundWebhook(t, "refund.processing", map[string]any{
		"id": secondPaystackID, "amount": secondAmount, "currency": "GHS", "status": "processing",
		"transaction_reference": secondRef,
	})
	if w := postWebhook(f.router, processing, paystacktest.Sign(paystackSecret, processing)); w.Code != http.StatusOK {
		t.Fatalf("refund.processing: %d %s", w.Code, w.Body.String())
	}
	if got := f.scalar(t, `SELECT outcome FROM webhook_events WHERE event_type = 'refund.processing'`); got != "ignored" {
		t.Errorf("refund.processing on a failed refund = %s, want ignored", got)
	}
}
