package payouts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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

// Send timings (the refund lifecycle's shape, ADR-0027, applied to sends).
const (
	// sendRetrySoon re-runs a send whose last attempt proved nothing.
	sendRetrySoon = 2 * time.Minute
	// sendRecheckPoll re-checks a transfer Paystack holds.
	sendRecheckPoll = 10 * time.Minute
	// stuckAfter is how long a pending payout may wait for its webhook
	// before the reconciler asks Paystack directly.
	stuckAfter = 24 * time.Hour
	// sweepBatch bounds one timer pass.
	sweepBatch = 100
)

// ExecuteSeller queues one payout for the seller's full payable balance, or
// returns nil when there is nothing payable: no verified account, an active
// cooldown, a payout already in flight, or a balance below the floor. The
// state change, its ledger posting and the send enqueue commit in one
// transaction, under the seller's advisory lock.
func (s *Service) ExecuteSeller(ctx context.Context, sellerID uuid.UUID) (*Payout, error) {
	if s.ledger == nil {
		return nil, fmt.Errorf("payouts: execute: ledger is not wired")
	}
	if s.jobs == nil {
		return nil, fmt.Errorf("payouts: execute: job client is not wired")
	}
	var queued *Payout
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		if _, err := q.LockPayoutSeller(ctx, sellerID.String()); err != nil {
			return fmt.Errorf("lock payout seller: %w", err)
		}
		account, err := q.GetPayoutAccountBySeller(ctx, sellerID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("get payout account: %w", err)
		}
		if account.Status != StatusVerified {
			return nil
		}
		if account.CooldownUntil != nil && s.Now().Before(*account.CooldownUntil) {
			return nil
		}
		available, err := s.payableBalance(ctx, tx, sellerID)
		if err != nil {
			return err
		}
		if available < s.minPesewas {
			return nil
		}
		reference, err := newReference()
		if err != nil {
			return err
		}
		row, err := q.InsertPayout(ctx, db.InsertPayoutParams{
			SellerID: sellerID, AmountPesewas: available,
			Reference: reference, RecipientCode: account.RecipientCode,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if isUniqueViolation(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("insert payout: %w", err)
		}
		postErr := s.ledger.Post(ctx, tx, ledger.KindPayoutInitiated, reference,
			ledger.PayoutInitiated(sellerID, available)...)
		if errors.Is(postErr, ledger.ErrDuplicate) {
			return nil
		}
		if postErr != nil {
			return fmt.Errorf("post payout initiated: %w", postErr)
		}
		if _, err := s.jobs.InsertTx(ctx, tx, SendArgs{PayoutID: row.ID}, jobs.Unique()); err != nil {
			return fmt.Errorf("enqueue payout send: %w", err)
		}
		payout := fromPayoutRow(row)
		queued = &payout
		return nil
	})
	if err != nil {
		return nil, err
	}
	return queued, nil
}

// ExecuteAll queues one payout per payable seller. One seller's failure
// never stops the rest; the advisory lock per seller keeps concurrent sweeps
// from double-paying.
func (s *Service) ExecuteAll(ctx context.Context) (int, error) {
	now := s.Now()
	sellers, err := db.New(s.pool).ListPayoutCandidates(ctx, &now)
	if err != nil {
		return 0, fmt.Errorf("list payout candidates: %w", err)
	}
	queued := 0
	for _, sellerID := range sellers {
		payout, err := s.ExecuteSeller(ctx, sellerID)
		if err != nil {
			return queued, err
		}
		if payout != nil {
			queued++
		}
	}
	return queued, nil
}

// SendPayout runs one step of a payout's send (the 17a refund shape applied
// to transfers) and returns how long the job should wait before running
// again (0: the payout needs nothing more from the job).
//
//   - queued: call InitiateTransfer with no transaction open. A definite
//     rejection fails the payout and re-credits the payable; an ambiguous
//     outcome stays queued and reconciles first, because Paystack may hold
//     the transfer under our reference and a blind resend is safe only once
//     proven absent.
//   - pending: the webhook owns settlement; the job only polls the
//     reconciliation path. Anything else: nothing.
func (s *Service) SendPayout(ctx context.Context, payoutID uuid.UUID) (time.Duration, error) {
	if s.paystack == nil {
		return 0, fmt.Errorf("payouts: send: paystack is not wired")
	}
	var recipientCode, reference string
	var amount int64
	var pending bool
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		payout, err := db.New(tx).GetPayoutForUpdate(ctx, payoutID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock payout: %w", err)
		}
		switch payout.Status {
		case PayoutQueued:
			recipientCode, reference, amount = payout.RecipientCode, payout.Reference, payout.AmountPesewas
		case PayoutPending:
			// The webhook owns settlement, but a missed webhook must not
			// stall the payout until the daily check: reconcile on every
			// poll, like the refund job does.
			pending = true
		default:
			return nil
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	log := s.logger().With(slog.String("payout_id", payoutID.String()))
	if pending {
		return s.reconcileSend(ctx, payoutID, log)
	}
	log = log.With(slog.String("reference", reference))
	result, callErr := s.paystack.InitiateTransfer(ctx, payments.TransferInput{
		AmountPesewas: amount, RecipientCode: recipientCode, Reference: reference, Reason: "Farmish payout",
	})
	switch {
	case callErr == nil:
		if err := s.markPending(ctx, payoutID, result.TransferCode); err != nil {
			log.Error("transfer created at paystack but could not be marked pending", slog.String("error", err.Error()))
			return sendRecheckPoll, nil
		}
		return sendRecheckPoll, nil
	case payments.IsDefiniteRejection(callErr):
		err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
			payout, err := db.New(tx).GetPayoutForUpdate(ctx, payoutID)
			if err != nil {
				return err
			}
			if payout.Status != PayoutQueued {
				// A webhook settled it while the call was out: the
				// webhook owns the payout now.
				return nil
			}
			return s.failPayout(ctx, tx, payout, PayoutFailed, truncate("paystack_rejected: "+callErr.Error(), 500))
		})
		return 0, err
	default:
		log.Warn("transfer call had an ambiguous outcome; reconciling before any retry",
			slog.String("error", callErr.Error()))
		return s.reconcileSend(ctx, payoutID, log)
	}
}

// reconcileSend adopts a transfer Paystack already holds, or proves the send
// never took effect. It runs after every ambiguous send and on every poll of
// a payout the webhook has not settled yet.
func (s *Service) reconcileSend(ctx context.Context, payoutID uuid.UUID, log *slog.Logger) (time.Duration, error) {
	var reference string
	if err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		payout, err := db.New(tx).GetPayoutForUpdate(ctx, payoutID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock payout: %w", err)
		}
		if payout.Status != PayoutQueued && payout.Status != PayoutPending {
			return nil
		}
		reference = payout.Reference
		return nil
	}); err != nil {
		return 0, err
	}
	if reference == "" {
		return 0, nil
	}
	remote, err := s.paystack.VerifyTransfer(ctx, reference)
	if payments.IsDefiniteRejection(err) {
		// Paystack holds nothing for this reference: sending again cannot
		// pay twice, because the reference is unique per payout.
		log.Warn("no transfer at paystack for this reference; the send may run again")
		return sendRetrySoon, nil
	}
	if err != nil {
		log.Warn("verify transfer failed; will re-check", slog.String("error", err.Error()))
		return sendRecheckPoll, nil
	}
	switch remote.Status {
	case "success":
		err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
			payout, err := db.New(tx).GetPayoutForUpdate(ctx, payoutID)
			if err != nil {
				return err
			}
			if payout.TransferCode == nil && remote.TransferCode != "" {
				payout.TransferCode = &remote.TransferCode
			}
			_, err = s.settleSuccess(ctx, tx, payout)
			return err
		})
		return 0, err
	case "failed", "reversed":
		err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
			payout, err := db.New(tx).GetPayoutForUpdate(ctx, payoutID)
			if err != nil {
				return err
			}
			return s.failPayout(ctx, tx, payout, remote.Status, "paystack_transfer_"+remote.Status)
		})
		return 0, err
	default:
		if err := s.markPending(ctx, payoutID, remote.TransferCode); err != nil {
			return 0, err
		}
		log.Info("adopted the transfer paystack already holds", slog.String("transfer_code", remote.TransferCode))
		return sendRecheckPoll, nil
	}
}

// markPending records a transfer Paystack holds. The update only fires from
// queued, so a payout the webhook settled first keeps its state (and its
// original sent_at, which the stuck check reads).
func (s *Service) markPending(ctx context.Context, payoutID uuid.UUID, transferCode string) error {
	return database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		now := s.Now()
		code := transferCode
		if err := db.New(tx).SetPayoutPending(ctx, db.SetPayoutPendingParams{
			ID: payoutID, TransferCode: &code, SentAt: &now,
		}); err != nil {
			return fmt.Errorf("mark payout pending: %w", err)
		}
		return nil
	})
}

// settleSuccess books a confirmed transfer (DOMAIN §5.3.7): the clearing
// balance pays out, plus the absorbed transfer fee when configured. A payout
// already successful is a replay.
func (s *Service) settleSuccess(ctx context.Context, tx pgx.Tx, payout db.Payout) (string, error) {
	if s.ledger == nil {
		return "", fmt.Errorf("payouts: settle success: ledger is not wired")
	}
	if payout.Status == PayoutSuccess {
		return payments.OutcomeIgnored, nil
	}
	q := db.New(tx)
	now := s.Now()
	settled, err := q.SetPayoutSuccess(ctx, db.SetPayoutSuccessParams{ID: payout.ID, CompletedAt: &now})
	if errors.Is(err, pgx.ErrNoRows) {
		return payments.OutcomeIgnored, nil
	}
	if err != nil {
		return "", fmt.Errorf("settle payout: %w", err)
	}
	postErr := s.ledger.Post(ctx, tx, ledger.KindPayoutSucceeded, settled.Reference,
		ledger.PayoutSucceeded(settled.AmountPesewas, s.transferFeePesewas)...)
	if postErr != nil && !errors.Is(postErr, ledger.ErrDuplicate) {
		return "", fmt.Errorf("post payout succeeded: %w", postErr)
	}
	amountText, err := money.FormatGHS(settled.AmountPesewas)
	if err != nil {
		return "", fmt.Errorf("format payout amount: %w", err)
	}
	if s.jobs != nil {
		if _, err := s.jobs.InsertTx(ctx, tx, notify.SMSArgs{
			UserID: settled.SellerID, Template: notify.TemplatePayoutSent,
			Params: map[string]string{"amount": amountText},
		}, nil); err != nil {
			return "", fmt.Errorf("enqueue payout notification: %w", err)
		}
	}
	return payments.OutcomeProcessed, nil
}

// failPayout marks a transfer failed or reversed, re-credits the payable
// (DOMAIN §5.3.8), audits and alerts. Success is final: it never fails.
func (s *Service) failPayout(ctx context.Context, tx pgx.Tx, payout db.Payout, status, reason string) error {
	if s.ledger == nil {
		return fmt.Errorf("payouts: fail payout: ledger is not wired")
	}
	if payout.Status == PayoutSuccess {
		return nil
	}
	now := s.Now()
	settled, err := db.New(tx).SetPayoutFailed(ctx, db.SetPayoutFailedParams{
		ID: payout.ID, Status: status, FailureReason: &reason, CompletedAt: &now,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("mark payout failed: %w", err)
	}
	postErr := s.ledger.Post(ctx, tx, ledger.KindPayoutFailed, settled.Reference,
		ledger.PayoutFailed(settled.SellerID, settled.AmountPesewas)...)
	if postErr != nil && !errors.Is(postErr, ledger.ErrDuplicate) {
		return fmt.Errorf("post payout failed: %w", postErr)
	}
	if err := audit.Record(ctx, tx, audit.Event{
		Action: "payout.failed", TargetType: "payout", TargetID: payout.ID.String(),
		Metadata: map[string]any{
			"seller_id": payout.SellerID.String(), "amount_pesewas": payout.AmountPesewas,
			"status": status, "reason": reason,
		},
	}); err != nil {
		return err
	}
	amountText, err := money.FormatGHS(settled.AmountPesewas)
	if err != nil {
		return fmt.Errorf("format payout amount: %w", err)
	}
	if s.jobs != nil {
		if _, err := s.jobs.InsertTx(ctx, tx, notify.SMSArgs{
			UserID: settled.SellerID, Template: notify.TemplatePayoutFailed,
			Params: map[string]string{"amount": amountText},
		}, nil); err != nil {
			return fmt.Errorf("enqueue payout notification: %w", err)
		}
	}
	s.logger().Error("payout failed; payable re-credited",
		slog.String("payout_id", payout.ID.String()), slog.String("seller_id", payout.SellerID.String()),
		slog.Int64("amount_pesewas", payout.AmountPesewas), slog.String("reason", reason))
	return nil
}

// transferData is the part of a transfer webhook this phase acts on.
type transferData struct {
	Reference    string `json:"reference"`
	Amount       int64  `json:"amount"`
	Currency     string `json:"currency"`
	TransferCode string `json:"transfer_code"`
}

func parseTransferEvent(data json.RawMessage) (transferData, error) {
	var d transferData
	if err := json.Unmarshal(data, &d); err != nil {
		return d, fmt.Errorf("%w: transfer data: %w", payments.ErrMalformedEvent, err)
	}
	return d, nil
}

// settleByReference settles the payout Paystack names, for the webhooks and
// the reconciler alike. Amount and currency mismatches settle nothing: the
// rejection is logged and audited for an admin.
func (s *Service) settleByReference(ctx context.Context, tx pgx.Tx, d transferData, terminal string) (string, error) {
	q := db.New(tx)
	payout, err := q.GetPayoutForUpdateByReference(ctx, d.Reference)
	if errors.Is(err, pgx.ErrNoRows) {
		return payments.OutcomeUnknownReference, nil
	}
	if err != nil {
		return "", fmt.Errorf("match payout: %w", err)
	}
	if d.TransferCode != "" && payout.TransferCode == nil {
		payout.TransferCode = &d.TransferCode
	}
	switch {
	case d.Currency != "" && d.Currency != ledger.CurrencyGHS:
		return s.rejectSettlement(ctx, tx, payout, payments.OutcomeCurrencyMismatch, d)
	case d.Amount != 0 && d.Amount != payout.AmountPesewas:
		return s.rejectSettlement(ctx, tx, payout, payments.OutcomeAmountMismatch, d)
	}
	switch terminal {
	case PayoutSuccess:
		switch payout.Status {
		case PayoutSuccess:
			return payments.OutcomeIgnored, nil
		case PayoutFailed, PayoutReversed:
			// A late success after we failed it: an admin's call, never an
			// automatic post (the payable was already re-credited).
			s.logger().Error("late transfer success after failure; needs an admin",
				slog.String("payout_id", payout.ID.String()), slog.String("reference", payout.Reference))
			if err := audit.Record(ctx, tx, audit.Event{
				Action: "payout.late_success", TargetType: "payout", TargetID: payout.ID.String(),
				Metadata: map[string]any{"seller_id": payout.SellerID.String(), "amount_pesewas": payout.AmountPesewas},
			}); err != nil {
				return "", err
			}
			return payments.OutcomeProcessed, nil
		default:
			return s.settleSuccess(ctx, tx, payout)
		}
	default:
		switch payout.Status {
		case PayoutSuccess:
			s.logger().Warn("transfer failure arrived after success; ignoring",
				slog.String("payout_id", payout.ID.String()), slog.String("reference", payout.Reference))
			return payments.OutcomeIgnored, nil
		case PayoutFailed, PayoutReversed:
			return payments.OutcomeIgnored, nil
		default:
			if err := s.failPayout(ctx, tx, payout, terminal, "paystack_transfer_"+terminal); err != nil {
				return "", err
			}
			return payments.OutcomeProcessed, nil
		}
	}
}

// rejectSettlement logs and audits a transfer event whose money does not
// match the payout, settling nothing.
func (s *Service) rejectSettlement(ctx context.Context, tx pgx.Tx, payout db.Payout, outcome string, d transferData) (string, error) {
	s.logger().Error("payout settlement rejected",
		slog.String("payout_id", payout.ID.String()), slog.String("outcome", outcome),
		slog.Int64("expected_pesewas", payout.AmountPesewas), slog.Int64("reported_pesewas", d.Amount),
		slog.String("reported_currency", d.Currency))
	if err := audit.Record(ctx, tx, audit.Event{
		Action: "payout.settlement_rejected", TargetType: "payout", TargetID: payout.ID.String(),
		Metadata: map[string]any{
			"seller_id": payout.SellerID.String(), "outcome": outcome,
			"expected_pesewas": payout.AmountPesewas, "reported_pesewas": d.Amount, "reported_currency": d.Currency,
		},
	}); err != nil {
		return "", err
	}
	return outcome, nil
}

// OnTransferSuccess is the transfer.success webhook.
func (s *Service) OnTransferSuccess(ctx context.Context, tx pgx.Tx, data json.RawMessage) (string, error) {
	d, err := parseTransferEvent(data)
	if err != nil {
		return "", err
	}
	if d.Reference == "" {
		return payments.OutcomeUnknownReference, nil
	}
	return s.settleByReference(ctx, tx, d, PayoutSuccess)
}

// OnTransferFailed is the transfer.failed webhook.
func (s *Service) OnTransferFailed(ctx context.Context, tx pgx.Tx, data json.RawMessage) (string, error) {
	d, err := parseTransferEvent(data)
	if err != nil {
		return "", err
	}
	if d.Reference == "" {
		return payments.OutcomeUnknownReference, nil
	}
	return s.settleByReference(ctx, tx, d, PayoutFailed)
}

// OnTransferReversed is the transfer.reversed webhook.
func (s *Service) OnTransferReversed(ctx context.Context, tx pgx.Tx, data json.RawMessage) (string, error) {
	d, err := parseTransferEvent(data)
	if err != nil {
		return "", err
	}
	if d.Reference == "" {
		return payments.OutcomeUnknownReference, nil
	}
	return s.settleByReference(ctx, tx, d, PayoutReversed)
}

// RegisterTransferEvents wires the transfer webhooks into the payments event
// dispatch. cmd/api and the routing test both call it, so the test proves
// the production registration.
func RegisterTransferEvents(p *payments.Service, s *Service) {
	p.RegisterEvent(payments.EventTransferSuccess, s.OnTransferSuccess)
	p.RegisterEvent(payments.EventTransferFailed, s.OnTransferFailed)
	p.RegisterEvent(payments.EventTransferReversed, s.OnTransferReversed)
}

// ReconcileStuck settles pending payouts older than a day through
// VerifyTransfer. Transfers still unsettled get an audit row for an admin;
// the job returns nil either way.
func (s *Service) ReconcileStuck(ctx context.Context) (int, error) {
	cutoff := s.Now().Add(-stuckAfter)
	rows, err := db.New(s.pool).ListStuckPendingPayouts(ctx, db.ListStuckPendingPayoutsParams{
		SentAt: &cutoff, Limit: sweepBatch,
	})
	if err != nil {
		return 0, fmt.Errorf("list stuck payouts: %w", err)
	}
	settled := 0
	for _, row := range rows {
		remote, err := s.paystack.VerifyTransfer(ctx, row.Reference)
		if err != nil {
			s.logger().Warn("reconcile verify failed; will re-check",
				slog.String("payout_id", row.ID.String()), slog.String("error", err.Error()))
			continue
		}
		switch remote.Status {
		case "success":
			err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
				payout, err := db.New(tx).GetPayoutForUpdate(ctx, row.ID)
				if err != nil {
					return err
				}
				_, err = s.settleSuccess(ctx, tx, payout)
				return err
			})
			if err != nil {
				return settled, err
			}
			settled++
		case "failed", "reversed":
			err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
				payout, err := db.New(tx).GetPayoutForUpdate(ctx, row.ID)
				if err != nil {
					return err
				}
				return s.failPayout(ctx, tx, payout, remote.Status, "paystack_transfer_"+remote.Status)
			})
			if err != nil {
				return settled, err
			}
			settled++
		default:
			s.logger().Error("payout still unsettled after a day; needs an admin",
				slog.String("payout_id", row.ID.String()), slog.String("reference", row.Reference))
			err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
				return audit.Record(ctx, tx, audit.Event{
					Action: "payout.stuck", TargetType: "payout", TargetID: row.ID.String(),
					Metadata: map[string]any{"seller_id": row.SellerID.String(), "amount_pesewas": row.AmountPesewas},
				})
			})
			if err != nil {
				return settled, err
			}
		}
	}
	return settled, nil
}

// RetryPayout triggers a fresh execute for the seller of a failed or
// reversed payout: a new row with a new reference for the (re-credited)
// balance. Anything else is a 409-class refusal.
func (s *Service) RetryPayout(ctx context.Context, adminID, payoutID uuid.UUID) (Payout, error) {
	if s.jobs == nil {
		return Payout{}, fmt.Errorf("payouts: retry: job client is not wired")
	}
	row, err := db.New(s.pool).GetPayoutByID(ctx, payoutID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Payout{}, fmt.Errorf("%w: %s", ErrPayoutNotFound, payoutID)
	}
	if err != nil {
		return Payout{}, fmt.Errorf("get payout: %w", err)
	}
	if row.Status != PayoutFailed && row.Status != PayoutReversed {
		return Payout{}, ErrRetryNotAllowed
	}
	fresh, err := s.ExecuteSeller(ctx, row.SellerID)
	if err != nil {
		return Payout{}, err
	}
	if fresh == nil {
		return Payout{}, ErrRetryNotAllowed
	}
	if err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		return audit.Record(ctx, tx, audit.Event{
			ActorID: &adminID, Action: "payout.retry", TargetType: "payout", TargetID: fresh.ID.String(),
			Metadata: map[string]any{
				"seller_id": fresh.SellerID.String(), "amount_pesewas": fresh.AmountPesewas,
				"retried_payout_id": payoutID.String(),
			},
		})
	}); err != nil {
		return Payout{}, err
	}
	return *fresh, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
