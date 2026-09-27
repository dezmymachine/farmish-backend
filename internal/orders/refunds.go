package orders

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dezmymachine/farmish-backend/internal/audit"
	"github.com/dezmymachine/farmish-backend/internal/database"
	"github.com/dezmymachine/farmish-backend/internal/db"
	"github.com/dezmymachine/farmish-backend/internal/jobs"
	"github.com/dezmymachine/farmish-backend/internal/ledger"
	"github.com/dezmymachine/farmish-backend/internal/money"
	"github.com/dezmymachine/farmish-backend/internal/notify"
	"github.com/dezmymachine/farmish-backend/internal/payments"
)

// Refund reconciliation timings (ADR-0027). A refund whose Paystack call had
// an ambiguous outcome stays pending and is reconciled against Paystack's
// refund list; only once RefundRetryGrace has passed with no matching refund
// at Paystack may the call be sent again. A settled-looking refund is polled
// until Paystack reports it processed or failed, and polling stops after
// RefundGiveUpAfter (an admin takes over).
const (
	RefundRetryGrace  = 15 * time.Minute
	RefundRecheckSoon = 2 * time.Minute
	RefundRecheckPoll = 10 * time.Minute
	RefundGiveUpAfter = 7 * 24 * time.Hour
	// refundRecheckNow asks the job to run again immediately (River snoozes
	// need a positive duration to mean "later"; this is effectively now).
	refundRecheckNow = time.Second
	// listRefundsPages bounds one reconciliation's walk of Paystack's list.
	listRefundsPages   = 10
	listRefundsPerPage = 100
	// listRefundsLookback widens the list window around the attempt, for clock
	// skew between us and Paystack.
	listRefundsLookback = 5 * time.Minute
)

var (
	// ErrRefundExceedsBase means a new refund would take an order's refunds
	// (all but failed ones) above what the buyer paid for it.
	ErrRefundExceedsBase = errors.New("refunds would exceed the order base")
	// ErrAmbiguousRefundMatch means a refund webhook without a known Paystack
	// id matches more than one in-flight refund (a multi-seller checkout shares
	// one payment reference). The webhook is rolled back and answered 500, so
	// Paystack retries after the job has stored the ids.
	ErrAmbiguousRefundMatch = errors.New("refund webhook matches more than one in-flight refund")
)

// RemainingRelease returns what escrow release should pay for an order's
// currently-held remainder (DOMAIN §4.1). A partial refund is taken from the
// delivery fee first, then from the subtotal, and commission is charged only
// on the subtotal that remains. remainingBase is 0 when the order was
// refunded in full: there is nothing left to release.
func RemainingRelease(order Order) (remainingBase, commission int64, err error) {
	remainingBase = order.BasePesewas - order.RefundedPesewas
	if remainingBase <= 0 {
		return 0, 0, nil
	}
	refundedSubtotal := order.RefundedPesewas - order.DeliveryFeePesewas
	if refundedSubtotal < 0 {
		refundedSubtotal = 0
	}
	remainingSubtotal := order.SubtotalPesewas - refundedSubtotal
	if remainingSubtotal < 0 {
		remainingSubtotal = 0
	}
	commission, err = money.Commission(remainingSubtotal, int(order.CommissionRateBps))
	if err != nil {
		return 0, 0, fmt.Errorf("compute remaining commission: %w", err)
	}
	return remainingBase, commission, nil
}

// CreateRefund records that money is owed for an order and queues the
// external call, inside the caller's transaction (the side effect of a
// cancellation, or the checkout expiry recovery path).
//
// It is idempotent: the partial unique index on refunds stops a second full
// refund on the same order, which this treats as "already recorded". Under the
// order's row lock it refuses a refund that would take the order's refunds
// (all but failed ones) above its base.
func (s *Service) CreateRefund(ctx context.Context, tx pgx.Tx, orderID uuid.UUID, amount int64, reason string) error {
	if s.jobs == nil {
		// Money must never be recorded without the job that moves it.
		return fmt.Errorf("orders: create refund: job client is not wired")
	}
	q := db.New(tx)
	order, err := q.GetOrderForUpdate(ctx, orderID)
	if err != nil {
		return fmt.Errorf("lock order: %w", err)
	}
	refund, err := q.InsertRefund(ctx, db.InsertRefundParams{
		OrderID: orderID, AmountPesewas: amount, Reason: reason,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("insert refund: %w", err)
	}
	outstanding, err := q.SumOutstandingRefundsForOrder(ctx, orderID)
	if err != nil {
		return fmt.Errorf("sum refunds: %w", err)
	}
	if outstanding > order.BasePesewas {
		return fmt.Errorf("%w: order %s base %d, refunds would total %d",
			ErrRefundExceedsBase, orderID, order.BasePesewas, outstanding)
	}
	if _, err := s.jobs.InsertTx(ctx, tx, RefundArgs{RefundID: refund.ID}, jobs.Unique()); err != nil {
		return fmt.Errorf("enqueue refund: %w", err)
	}
	return nil
}

// ReleaseEscrow posts the seller's earnings and the platform's commission for
// an order's currently-held remainder (DOMAIN §5.3.2, §4.1), then marks the
// escrow released. It is idempotent: the order's state is re-checked under
// its row lock, and the ledger's UNIQUE(kind, reference) stops a second
// posting for the same order. posted is false for every no-op path, so the
// caller can log why nothing happened.
func (s *Service) ReleaseEscrow(ctx context.Context, orderID uuid.UUID) (posted bool, err error) {
	if s.ledger == nil {
		return false, fmt.Errorf("orders: release escrow: ledger is not wired")
	}
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		row, err := q.GetOrderForUpdate(ctx, orderID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock order: %w", err)
		}
		order := fromRow(row)
		if order.Status != StatusCompleted {
			return nil
		}
		switch order.EscrowState {
		case EscrowHeld, EscrowPartiallyRefunded, EscrowRefundPending:
		default:
			return nil
		}
		inFlight, err := q.CountInFlightRefundsForOrder(ctx, orderID)
		if err != nil {
			return fmt.Errorf("count in-flight refunds: %w", err)
		}
		if inFlight > 0 {
			// A partial dispute resolution enqueues the refund before the
			// release: the release waits until the refund settles, and the
			// worker turns this into a snooze. Returning the sentinel (rather
			// than a silent no-op) keeps a stuck release visible.
			return fmt.Errorf("%w: order %s has %d refund(s) in flight", ErrReleaseDeferred, orderID, inFlight)
		}
		if order.EscrowState == EscrowRefundPending {
			return nil
		}
		remainingBase, commission, err := RemainingRelease(order)
		if err != nil {
			return err
		}
		if remainingBase <= 0 {
			// A full refund left nothing to release (DOMAIN §4.1): no
			// commission is earned, and there is no ledger entry to post.
			return nil
		}
		postErr := s.ledger.Post(ctx, tx, ledger.KindEscrowRelease, order.ID.String(),
			ledger.EscrowRelease(order.ID, order.SellerID, remainingBase, commission)...)
		if errors.Is(postErr, ledger.ErrDuplicate) {
			return nil
		}
		if postErr != nil {
			return fmt.Errorf("post escrow release: %w", postErr)
		}
		if err := q.SetOrderEscrowState(ctx, db.SetOrderEscrowStateParams{
			ID: order.ID, EscrowState: EscrowReleased,
		}); err != nil {
			return fmt.Errorf("set escrow released: %w", err)
		}
		posted = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return posted, nil
}

// refundStep is what one run of the refund job decided to do, inside its
// first transaction, before any network call.
type refundStep struct {
	action    string // "", "call" or "reconcile"
	refund    db.Refund
	reference string
}

// ProcessRefund runs one step of a refund's life (ADR-0027) and returns how
// long the job should wait before running again (0: the refund needs nothing
// more from the job).
//
//   - queued: re-check "no refund after release", mark pending (committed
//     before the call), call Paystack. A definite rejection fails the refund;
//     an ambiguous failure (timeout, 5xx, lost response) leaves it pending,
//     because Paystack may have created it and a blind retry would refund the
//     buyer twice.
//   - pending: reconcile. With Paystack's id known, fetch the refund and
//     settle or fail it. Without it, look for our refund in Paystack's list
//     and adopt it; only after RefundRetryGrace with no match at Paystack does
//     the refund go back to queued for another call.
//   - processed / failed: nothing.
func (s *Service) ProcessRefund(ctx context.Context, refundID uuid.UUID) (time.Duration, error) {
	if s.paystack == nil {
		return 0, fmt.Errorf("orders: process refund: paystack is not wired")
	}
	var step refundStep
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		refund, err := q.GetRefundForUpdate(ctx, refundID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock refund: %w", err)
		}
		switch refund.Status {
		case RefundStatusQueued:
			order, err := q.GetOrderForUpdate(ctx, refund.OrderID)
			if err != nil {
				return fmt.Errorf("lock order: %w", err)
			}
			if order.EscrowState == EscrowReleased {
				// The manual path: an admin handles it outside the system.
				return s.failRefund(ctx, tx, refund, "refund_after_release", "refund.after_release")
			}
			ref, err := q.GetPaymentReferenceForOrder(ctx, order.ID)
			if err != nil {
				return fmt.Errorf("get payment reference: %w", err)
			}
			now := s.Now()
			if err := q.SetRefundPending(ctx, db.SetRefundPendingParams{ID: refund.ID, AttemptedAt: &now}); err != nil {
				return fmt.Errorf("mark refund pending: %w", err)
			}
			refund.AttemptedAt = &now
			step = refundStep{action: "call", refund: refund, reference: ref}
		case RefundStatusPending:
			ref, err := q.GetPaymentReferenceForOrder(ctx, refund.OrderID)
			if err != nil {
				return fmt.Errorf("get payment reference: %w", err)
			}
			step = refundStep{action: "reconcile", refund: refund, reference: ref}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	switch step.action {
	case "call":
		return s.callRefund(ctx, step)
	case "reconcile":
		return s.reconcileRefund(ctx, step)
	default:
		return 0, nil
	}
}

// callRefund makes the Paystack call, with no transaction open.
func (s *Service) callRefund(ctx context.Context, step refundStep) (time.Duration, error) {
	log := s.logger().With(slog.String("refund_id", step.refund.ID.String()),
		slog.String("order_id", step.refund.OrderID.String()))
	result, callErr := s.paystack.CreateRefund(ctx, payments.RefundInput{
		TransactionReference: step.reference, AmountPesewas: step.refund.AmountPesewas,
	})
	switch {
	case callErr == nil:
		if err := s.storePaystackRefundID(ctx, step.refund.ID, result.RefundID); err != nil {
			// The refund exists at Paystack; keep its id for reconciliation.
			log.Error("refund created at paystack but its id could not be stored",
				slog.Int64("paystack_refund_id", result.RefundID), slog.String("error", err.Error()))
			return RefundRecheckSoon, nil
		}
		return RefundRecheckPoll, nil
	case payments.IsDefiniteRejection(callErr):
		err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
			refund, err := db.New(tx).GetRefundForUpdate(ctx, step.refund.ID)
			if err != nil {
				return err
			}
			return s.failRefund(ctx, tx, refund, truncate("paystack_rejected: "+callErr.Error(), 500), "refund.failed")
		})
		return 0, err
	default:
		// Ambiguous: Paystack may have created the refund. Stay pending and
		// reconcile before anything is sent again.
		log.Warn("refund call had an ambiguous outcome; reconciling before any retry",
			slog.String("error", callErr.Error()))
		return RefundRecheckSoon, nil
	}
}

// reconcileRefund settles, fails, adopts or (only when provably safe) requeues
// a pending refund by asking Paystack what it holds.
func (s *Service) reconcileRefund(ctx context.Context, step refundStep) (time.Duration, error) {
	refund := step.refund
	log := s.logger().With(slog.String("refund_id", refund.ID.String()),
		slog.String("order_id", refund.OrderID.String()))
	attempted := refund.CreatedAt
	if refund.AttemptedAt != nil {
		attempted = *refund.AttemptedAt
	}
	now := s.Now()
	if now.Sub(attempted) >= RefundGiveUpAfter {
		err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
			return audit.Record(ctx, tx, audit.Event{
				Action: "refund.stuck", TargetType: "refund", TargetID: refund.ID.String(),
				Metadata: map[string]any{"order_id": refund.OrderID.String(), "amount_pesewas": refund.AmountPesewas},
			})
		})
		log.Error("refund still unsettled after the give-up window; needs an admin",
			slog.Duration("since_attempt", now.Sub(attempted)))
		return 0, err
	}

	if refund.PaystackRefundID != nil {
		id, err := strconv.ParseInt(*refund.PaystackRefundID, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("stored paystack refund id %q: %w", *refund.PaystackRefundID, err)
		}
		remote, err := s.paystack.FetchRefund(ctx, id)
		if err != nil {
			log.Warn("fetch refund failed; will re-check", slog.String("error", err.Error()))
			return RefundRecheckPoll, nil
		}
		switch remote.Status {
		case "processed":
			err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
				locked, err := db.New(tx).GetRefundForUpdate(ctx, refund.ID)
				if err != nil {
					return err
				}
				_, err = s.settleRefund(ctx, tx, locked, remote.AmountPesewas, remote.Currency)
				return err
			})
			return 0, err
		case "failed":
			err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
				locked, err := db.New(tx).GetRefundForUpdate(ctx, refund.ID)
				if err != nil {
					return err
				}
				return s.failRefund(ctx, tx, locked, "paystack_refund_failed", "refund.failed")
			})
			return 0, err
		default:
			return RefundRecheckPoll, nil
		}
	}

	// No Paystack id: did our ambiguous call create a refund anyway?
	candidates, err := s.findPaystackRefunds(ctx, step.reference, refund.AmountPesewas, attempted.Add(-listRefundsLookback))
	if err != nil {
		log.Warn("list refunds failed; will re-check", slog.String("error", err.Error()))
		return RefundRecheckSoon, nil
	}
	switch len(candidates) {
	case 1:
		if err := s.storePaystackRefundID(ctx, refund.ID, candidates[0].ID); err != nil {
			return 0, err
		}
		log.Info("adopted the refund paystack already created", slog.Int64("paystack_refund_id", candidates[0].ID))
		return refundRecheckNow, nil
	case 0:
		if now.Sub(attempted) < RefundRetryGrace {
			return RefundRecheckSoon, nil
		}
		// Proven: Paystack holds no refund for this attempt, so sending again
		// cannot refund the buyer twice.
		err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
			return db.New(tx).SetRefundQueuedForRetry(ctx, refund.ID)
		})
		if err != nil {
			return 0, err
		}
		log.Warn("no refund at paystack after the grace period; retrying the call")
		return refundRecheckNow, nil
	default:
		log.Error("several unlinked paystack refunds match this refund; needs an admin",
			slog.Int("candidates", len(candidates)))
		return RefundRecheckPoll, nil
	}
}

// findPaystackRefunds returns Paystack refunds for reference and amount,
// created on or after from, that no refund row has claimed yet.
func (s *Service) findPaystackRefunds(ctx context.Context, reference string, amount int64, from time.Time) ([]payments.Refund, error) {
	var out []payments.Refund
	q := db.New(s.pool)
	for page := 1; page <= listRefundsPages; page++ {
		batch, err := s.paystack.ListRefunds(ctx, payments.ListRefundsInput{From: from, Page: page, PerPage: listRefundsPerPage})
		if err != nil {
			return nil, err
		}
		for _, r := range batch {
			if r.TransactionReference != reference || r.AmountPesewas != amount {
				continue
			}
			id := strconv.FormatInt(r.ID, 10)
			linked, err := q.PaystackRefundIDLinked(ctx, &id)
			if err != nil {
				return nil, fmt.Errorf("check refund link: %w", err)
			}
			if !linked {
				out = append(out, r)
			}
		}
		if len(batch) < listRefundsPerPage {
			break
		}
	}
	return out, nil
}

func (s *Service) storePaystackRefundID(ctx context.Context, refundID uuid.UUID, paystackID int64) error {
	return database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		id := strconv.FormatInt(paystackID, 10)
		return db.New(tx).SetRefundPaystackID(ctx, db.SetRefundPaystackIDParams{ID: refundID, PaystackRefundID: &id})
	})
}

// settleRefund books a refund Paystack reports processed, for both the
// webhook and reconciliation (DOMAIN §5.3.3). Under the order's row lock it
// requires Paystack's amount and currency to match ours and the order's
// refunds to stay within its base; otherwise nothing is settled and the
// rejection is logged and audited for an admin. A refund already processed is
// a replay.
func (s *Service) settleRefund(ctx context.Context, tx pgx.Tx, refund db.Refund, amount int64, currency string) (string, error) {
	if s.ledger == nil {
		return "", fmt.Errorf("orders: settle refund: ledger is not wired")
	}
	if refund.Status == RefundStatusProcessed {
		return payments.OutcomeIgnored, nil
	}
	q := db.New(tx)
	order, err := q.GetOrderForUpdate(ctx, refund.OrderID)
	if err != nil {
		return "", fmt.Errorf("lock order: %w", err)
	}
	reject := func(outcome string) (string, error) {
		s.logger().Error("refund settlement rejected",
			slog.String("refund_id", refund.ID.String()), slog.String("order_id", refund.OrderID.String()),
			slog.String("outcome", outcome), slog.Int64("expected_pesewas", refund.AmountPesewas),
			slog.Int64("reported_pesewas", amount), slog.String("reported_currency", currency))
		if err := audit.Record(ctx, tx, audit.Event{
			Action: "refund.settlement_rejected", TargetType: "refund", TargetID: refund.ID.String(),
			Metadata: map[string]any{
				"order_id": refund.OrderID.String(), "outcome": outcome,
				"expected_pesewas": refund.AmountPesewas, "reported_pesewas": amount, "reported_currency": currency,
			},
		}); err != nil {
			return "", err
		}
		return outcome, nil
	}
	switch {
	case currency != ledger.CurrencyGHS:
		return reject(payments.OutcomeCurrencyMismatch)
	case amount != refund.AmountPesewas:
		return reject(payments.OutcomeAmountMismatch)
	case order.RefundedPesewas+amount > order.BasePesewas:
		return reject(payments.OutcomeExceedsBase)
	}

	settled, err := q.SetRefundProcessed(ctx, refund.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return payments.OutcomeIgnored, nil
	}
	if err != nil {
		return "", fmt.Errorf("settle refund: %w", err)
	}
	postErr := s.ledger.Post(ctx, tx, ledger.KindOrderRefund, settled.ID.String(),
		ledger.OrderRefund(settled.OrderID, settled.AmountPesewas)...)
	if postErr != nil && !errors.Is(postErr, ledger.ErrDuplicate) {
		return "", fmt.Errorf("post order refund: %w", postErr)
	}
	updated, err := q.AddOrderRefundedPesewas(ctx, db.AddOrderRefundedPesewasParams{
		ID: settled.OrderID, RefundedPesewas: settled.AmountPesewas,
	})
	if err != nil {
		return "", fmt.Errorf("add refunded pesewas: %w", err)
	}
	state := EscrowPartiallyRefunded
	if updated.RefundedPesewas >= updated.BasePesewas {
		state = EscrowRefunded
	}
	if err := q.SetOrderEscrowState(ctx, db.SetOrderEscrowStateParams{ID: updated.ID, EscrowState: state}); err != nil {
		return "", fmt.Errorf("set escrow state: %w", err)
	}
	if s.jobs != nil {
		if _, err := s.jobs.InsertTx(ctx, tx, notify.SMSArgs{
			UserID: updated.BuyerID, Template: notify.TemplateOrderRefundedBuyer,
			Params: map[string]string{"orderIdShort": updated.ID.String()[:8]},
		}, nil); err != nil {
			return "", fmt.Errorf("enqueue refund notification: %w", err)
		}
	}
	return payments.OutcomeProcessed, nil
}

// failRefund marks a refund failed with an audit row and an Error log: the
// alert hook for an admin (escrow stays as it is, so the money is still held).
func (s *Service) failRefund(ctx context.Context, tx pgx.Tx, refund db.Refund, reason, action string) error {
	if err := db.New(tx).SetRefundFailed(ctx, db.SetRefundFailedParams{ID: refund.ID, FailureReason: &reason}); err != nil {
		return fmt.Errorf("mark refund failed: %w", err)
	}
	if err := audit.Record(ctx, tx, audit.Event{
		Action: action, TargetType: "refund", TargetID: refund.ID.String(),
		Metadata: map[string]any{"order_id": refund.OrderID.String(), "amount_pesewas": refund.AmountPesewas, "reason": reason},
	}); err != nil {
		return err
	}
	s.logger().Error("refund failed; escrow stays held for an admin",
		slog.String("refund_id", refund.ID.String()), slog.String("order_id", refund.OrderID.String()),
		slog.Int64("amount_pesewas", refund.AmountPesewas), slog.String("reason", reason))
	return nil
}

// refundEventData is the part of a refund webhook this phase acts on
// (ADR-0027). Paystack's refund payload may carry no top-level id; matching
// then falls back to the transaction reference plus amount.
type refundEventData struct {
	ID                   int64  `json:"id"`
	Amount               int64  `json:"amount"`
	Status               string `json:"status"`
	Currency             string `json:"currency"`
	TransactionReference string `json:"transaction_reference"`
}

func parseRefundEvent(data json.RawMessage) (refundEventData, error) {
	var d refundEventData
	if err := json.Unmarshal(data, &d); err != nil {
		return d, fmt.Errorf("%w: refund data: %w", payments.ErrMalformedEvent, err)
	}
	return d, nil
}

// matchRefund finds the refund a webhook event describes: by Paystack's own
// id once stored, otherwise by the order's payment reference plus the amount,
// and then only when exactly one in-flight refund matches (ADR-0027).
func (s *Service) matchRefund(ctx context.Context, q *db.Queries, d refundEventData) (db.Refund, error) {
	if d.ID != 0 {
		id := strconv.FormatInt(d.ID, 10)
		refund, err := q.GetRefundForUpdateByPaystackRefundID(ctx, &id)
		if err == nil {
			return refund, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return db.Refund{}, err
		}
	}
	if d.TransactionReference == "" || d.Amount == 0 {
		return db.Refund{}, pgx.ErrNoRows
	}
	candidates, err := q.ListInFlightRefundsByReference(ctx, db.ListInFlightRefundsByReferenceParams{
		Reference: d.TransactionReference, AmountPesewas: d.Amount,
	})
	if err != nil {
		return db.Refund{}, err
	}
	switch len(candidates) {
	case 0:
		return db.Refund{}, pgx.ErrNoRows
	case 1:
		refund := candidates[0]
		if d.ID != 0 && refund.PaystackRefundID == nil {
			// Remember Paystack's id so later events for this refund match directly.
			id := strconv.FormatInt(d.ID, 10)
			if err := q.SetRefundPaystackID(ctx, db.SetRefundPaystackIDParams{ID: refund.ID, PaystackRefundID: &id}); err != nil {
				return db.Refund{}, fmt.Errorf("store paystack refund id: %w", err)
			}
			refund.PaystackRefundID = &id
		}
		return refund, nil
	default:
		s.logger().Warn("refund webhook is ambiguous; deferring until the job stores paystack ids",
			slog.String("reference", d.TransactionReference), slog.Int("candidates", len(candidates)))
		return db.Refund{}, fmt.Errorf("%w (%d candidates)", ErrAmbiguousRefundMatch, len(candidates))
	}
}

// OnRefundProcessed is the refund.processed webhook: settle through the same
// checks as reconciliation.
func (s *Service) OnRefundProcessed(ctx context.Context, tx pgx.Tx, data json.RawMessage) (string, error) {
	d, err := parseRefundEvent(data)
	if err != nil {
		return "", err
	}
	q := db.New(tx)
	refund, err := s.matchRefund(ctx, q, d)
	if errors.Is(err, pgx.ErrNoRows) {
		return payments.OutcomeUnknownReference, nil
	}
	if err != nil {
		return "", fmt.Errorf("match refund: %w", err)
	}
	return s.settleRefund(ctx, tx, refund, d.Amount, d.Currency)
}

// OnRefundFailed is the refund.failed webhook: the refund fails, with an
// audit row and an Error log, and escrow stays held so an admin can retry.
func (s *Service) OnRefundFailed(ctx context.Context, tx pgx.Tx, data json.RawMessage) (string, error) {
	d, err := parseRefundEvent(data)
	if err != nil {
		return "", err
	}
	q := db.New(tx)
	refund, err := s.matchRefund(ctx, q, d)
	if errors.Is(err, pgx.ErrNoRows) {
		return payments.OutcomeUnknownReference, nil
	}
	if err != nil {
		return "", fmt.Errorf("match refund: %w", err)
	}
	if refund.Status == RefundStatusProcessed || refund.Status == RefundStatusFailed {
		return payments.OutcomeIgnored, nil
	}
	if err := s.failRefund(ctx, tx, refund, "paystack_refund_failed", "refund.failed"); err != nil {
		return "", err
	}
	return payments.OutcomeProcessed, nil
}

// OnRefundPending is the informational refund.pending / refund.processing
// webhook. The refund is already pending; the only effect is remembering
// Paystack's id when matched by the fallback.
func (s *Service) OnRefundPending(ctx context.Context, tx pgx.Tx, data json.RawMessage) (string, error) {
	d, err := parseRefundEvent(data)
	if err != nil {
		return "", err
	}
	q := db.New(tx)
	refund, err := s.matchRefund(ctx, q, d)
	if errors.Is(err, pgx.ErrNoRows) {
		return payments.OutcomeUnknownReference, nil
	}
	if err != nil {
		return "", fmt.Errorf("match refund: %w", err)
	}
	if refund.Status == RefundStatusProcessed || refund.Status == RefundStatusFailed {
		return payments.OutcomeIgnored, nil
	}
	return payments.OutcomeProcessed, nil
}

// RegisterRefundEvents wires the refund webhooks into the payments event
// dispatch. cmd/api and the routing test both call it, so the test proves the
// production registration.
func RegisterRefundEvents(p *payments.Service, s *Service) {
	p.RegisterEvent(payments.EventRefundProcessed, s.OnRefundProcessed)
	p.RegisterEvent(payments.EventRefundFailed, s.OnRefundFailed)
	p.RegisterEvent(payments.EventRefundPending, s.OnRefundPending)
	p.RegisterEvent(payments.EventRefundProcessing, s.OnRefundPending)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
