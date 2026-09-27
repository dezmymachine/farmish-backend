package orders

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/dezmymachine/farmish-backend/internal/audit"
	"github.com/dezmymachine/farmish-backend/internal/database"
	"github.com/dezmymachine/farmish-backend/internal/db"
	"github.com/dezmymachine/farmish-backend/internal/jobs"
	"github.com/dezmymachine/farmish-backend/internal/notify"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

// validDisputeOutcomes mirrors the disputes CHECK.
var validDisputeOutcomes = map[string]bool{
	DisputeOutcomeRefundBuyer: true, DisputeOutcomeReleaseSeller: true, DisputeOutcomePartial: true,
}

// Dispute is one row of the disputes table.
type Dispute struct {
	ID             uuid.UUID
	OrderID        uuid.UUID
	OpenedBy       uuid.UUID
	Reason         string
	Description    string
	Status         string
	Outcome        *string
	RefundPesewas  *int64
	ResolutionNote *string
	ResolvedBy     *uuid.UUID
	ResolvedAt     *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// DisputeView is a dispute with its order, for the admin surfaces.
type DisputeView struct {
	Dispute Dispute
	Order   Detail
}

// PartyContact is one order party's contact fields, admin only.
type PartyContact struct {
	UserID      uuid.UUID
	DisplayName *string
	Email       *string
	Phone       *string
}

// LedgerEntryView is one ledger entry posted for an order.
type LedgerEntryView struct {
	Account              string
	Amount               int64
	Currency             string
	TransactionKind      string
	TransactionReference string
	CreatedAt            time.Time
}

// AdminOrder is an order with both parties' contacts and its ledger entries.
type AdminOrder struct {
	Order   Detail
	Buyer   PartyContact
	Seller  PartyContact
	Entries []LedgerEntryView
}

// ListDisputes returns the admin review queue, oldest first. An empty status
// lists every dispute.
func (s *Service) ListDisputes(ctx context.Context, status string, limit, offset int32) ([]DisputeView, int64, error) {
	var filter *string
	if status != "" {
		if status != DisputeStatusOpen && status != DisputeStatusResolved {
			var invalid validation.Error
			invalid.Add("status", "must be open or resolved")
			return nil, 0, invalid.OrNil()
		}
		filter = &status
	}
	rows, err := db.New(s.pool).ListDisputes(ctx, db.ListDisputesParams{
		Status: filter, Limit: limit, Offset: offset,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("list disputes: %w", err)
	}
	total, err := db.New(s.pool).CountDisputes(ctx, filter)
	if err != nil {
		return nil, 0, fmt.Errorf("count disputes: %w", err)
	}
	views := make([]DisputeView, 0, len(rows))
	for _, row := range rows {
		detail, err := s.detailUnchecked(ctx, row.OrderID)
		if err != nil {
			return nil, 0, err
		}
		views = append(views, DisputeView{Dispute: fromDisputeRow(row), Order: detail})
	}
	return views, total, nil
}

// GetDispute returns one dispute with its order.
func (s *Service) GetDispute(ctx context.Context, disputeID uuid.UUID) (DisputeView, error) {
	row, err := db.New(s.pool).GetDisputeByID(ctx, disputeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return DisputeView{}, fmt.Errorf("%w: %s", ErrDisputeNotFound, disputeID)
	}
	if err != nil {
		return DisputeView{}, fmt.Errorf("get dispute: %w", err)
	}
	detail, err := s.detailUnchecked(ctx, row.OrderID)
	if err != nil {
		return DisputeView{}, err
	}
	return DisputeView{Dispute: fromDisputeRow(row), Order: detail}, nil
}

// Resolve moves a disputed order to its outcome in one transaction: the state
// change, the refund row, the release and notify enqueues, the dispute's
// resolution and the audit event all commit together.
//
//   - refund_buyer: disputed → refunded, a dispute_refund of the whole
//     remaining escrow, escrow refund_pending.
//   - release_seller: disputed → completed, the held escrow released.
//   - partial: disputed → completed, a dispute_partial of refundAmount, then
//     the release, which waits (snoozes) until the refund is processed.
//
// refundAmount is required for partial and forbidden otherwise; nil means the
// client sent none.
func (s *Service) Resolve(ctx context.Context, adminID, disputeID uuid.UUID, outcome string, refundAmount *int64, note string) (DisputeView, error) {
	var invalid validation.Error
	if !validDisputeOutcomes[outcome] {
		invalid.Add("outcome", "must be refund_buyer, release_seller or partial")
	}
	trimmed := strings.TrimSpace(note)
	if len(trimmed) < 1 || len(trimmed) > 2000 {
		invalid.Add("note", "must be between 1 and 2000 characters")
	}
	if outcome == DisputeOutcomePartial && refundAmount == nil {
		invalid.Add("refundAmount", "is required for a partial resolution")
	}
	if outcome != DisputeOutcomePartial && refundAmount != nil {
		invalid.Add("refundAmount", "is only allowed for a partial resolution")
	}
	if err := invalid.OrNil(); err != nil {
		return DisputeView{}, err
	}
	if s.jobs == nil {
		return DisputeView{}, fmt.Errorf("orders: resolve dispute: job client is not wired")
	}

	var resolved db.Dispute
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		dispute, err := q.GetDisputeForUpdate(ctx, disputeID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrDisputeNotFound, disputeID)
		}
		if err != nil {
			return fmt.Errorf("lock dispute: %w", err)
		}
		if dispute.Status != DisputeStatusOpen {
			return ErrDisputeNotOpen
		}
		order, err := q.GetOrderForUpdate(ctx, dispute.OrderID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrNotFound, dispute.OrderID)
		}
		if err != nil {
			return fmt.Errorf("lock order: %w", err)
		}
		if order.Status != StatusDisputed {
			return &InvalidTransitionError{From: order.Status, To: resolveTarget(outcome)}
		}
		remaining := order.BasePesewas - order.RefundedPesewas
		amount := remaining
		reason := RefundReasonDisputeRefund
		if outcome == DisputeOutcomePartial {
			amount = *refundAmount
			reason = RefundReasonDisputePartial
			var outOfRange validation.Error
			if amount <= 0 || amount >= remaining {
				outOfRange.Add("refundAmount", "must be between 1 and the order's remaining escrow")
				return outOfRange.OrNil()
			}
		} else if outcome == DisputeOutcomeRefundBuyer && remaining <= 0 {
			return fmt.Errorf("%w: order %s holds nothing", ErrRefundExceedsBase, order.ID)
		}

		target := resolveTarget(outcome)
		moved, _, err := s.Transition(ctx, tx, order.ID, target, Admin(adminID), "dispute resolved: "+outcome)
		if err != nil {
			return err
		}
		switch outcome {
		case DisputeOutcomeRefundBuyer:
			if err := s.CreateRefund(ctx, tx, order.ID, amount, reason); err != nil {
				return err
			}
			if err := s.SetEscrowState(ctx, tx, order.ID, EscrowRefundPending); err != nil {
				return err
			}
		case DisputeOutcomeReleaseSeller:
			if _, err := s.jobs.InsertTx(ctx, tx, ReleaseEscrowArgs{OrderID: order.ID}, jobs.Unique()); err != nil {
				return fmt.Errorf("enqueue release escrow: %w", err)
			}
		case DisputeOutcomePartial:
			// The refund first, so the release the next line enqueues sees
			// the in-flight refund and snoozes until it settles.
			if err := s.CreateRefund(ctx, tx, order.ID, amount, reason); err != nil {
				return err
			}
			if _, err := s.jobs.InsertTx(ctx, tx, ReleaseEscrowArgs{OrderID: order.ID}, jobs.Unique()); err != nil {
				return fmt.Errorf("enqueue release escrow: %w", err)
			}
		}
		now := s.Now()
		updated, err := q.ResolveDispute(ctx, db.ResolveDisputeParams{
			ID: dispute.ID, Outcome: &outcome, RefundPesewas: &amount,
			ResolutionNote: &trimmed, ResolvedBy: pgtype.UUID{Bytes: adminID, Valid: true}, ResolvedAt: &now,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrDisputeNotOpen
		}
		if err != nil {
			return fmt.Errorf("resolve dispute: %w", err)
		}
		resolved = updated
		if err := audit.Record(ctx, tx, audit.Event{
			ActorID: &adminID, Action: "dispute.resolve", TargetType: "dispute", TargetID: dispute.ID.String(),
			Metadata: map[string]any{
				"order_id": order.ID.String(), "outcome": outcome,
				"refund_pesewas": amount, "note": trimmed,
			},
		}); err != nil {
			return err
		}
		for _, recipient := range []struct {
			id       uuid.UUID
			template string
		}{
			{order.BuyerID, notify.TemplateDisputeResolvedBuyer},
			{order.SellerID, notify.TemplateDisputeResolvedSeller},
		} {
			if _, err := s.jobs.InsertTx(ctx, tx, notify.SMSArgs{
				UserID: recipient.id, Template: recipient.template,
				Params: map[string]string{
					"orderId":      order.ID.String(),
					"orderIdShort": order.ID.String()[:8],
					"outcome":      friendlyOutcome(outcome),
				},
			}, nil); err != nil {
				return fmt.Errorf("enqueue resolution notification: %w", err)
			}
		}
		_ = moved
		return nil
	})
	if err != nil {
		return DisputeView{}, err
	}
	detail, err := s.detailUnchecked(ctx, resolved.OrderID)
	if err != nil {
		return DisputeView{}, err
	}
	return DisputeView{Dispute: fromDisputeRow(resolved), Order: detail}, nil
}

// RetryRefund re-queues a failed refund's Paystack call. A refund refused
// after escrow release stays failed: an admin handles it outside the system.
func (s *Service) RetryRefund(ctx context.Context, adminID, refundID uuid.UUID) (db.Refund, error) {
	if s.jobs == nil {
		return db.Refund{}, fmt.Errorf("orders: retry refund: job client is not wired")
	}
	var retried db.Refund
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		refund, err := q.GetRefundForUpdate(ctx, refundID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrRefundNotFound, refundID)
		}
		if err != nil {
			return fmt.Errorf("lock refund: %w", err)
		}
		if refund.Status != RefundStatusFailed {
			return ErrRefundNotFailed
		}
		if refund.FailureReason != nil && *refund.FailureReason == "refund_after_release" {
			return ErrRefundAfterRelease
		}
		order, err := q.GetOrderForUpdate(ctx, refund.OrderID)
		if err != nil {
			return fmt.Errorf("lock order: %w", err)
		}
		if order.EscrowState == EscrowReleased {
			return ErrRefundAfterRelease
		}
		retried, err = q.SetRefundQueuedFromFailed(ctx, refund.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRefundNotFailed
		}
		if err != nil {
			return fmt.Errorf("requeue refund: %w", err)
		}
		if err := audit.Record(ctx, tx, audit.Event{
			ActorID: &adminID, Action: "refund.retry", TargetType: "refund", TargetID: refund.ID.String(),
			Metadata: map[string]any{"order_id": refund.OrderID.String(), "amount_pesewas": refund.AmountPesewas},
		}); err != nil {
			return err
		}
		// No uniqueness guard: the refund's own queued check is the
		// idempotency mechanism, and a completed or failed job row for the
		// same args would swallow a unique insert, leaving the retry
		// queued forever with no worker coming for it.
		if _, err := s.jobs.InsertTx(ctx, tx, RefundArgs{RefundID: refund.ID}, nil); err != nil {
			return fmt.Errorf("enqueue refund: %w", err)
		}
		return nil
	})
	if err != nil {
		return db.Refund{}, err
	}
	return retried, nil
}

// GetAdminOrder returns an order with both parties' contacts and its ledger
// entries. Unlike Get, any existing order is visible: admins may probe ids.
func (s *Service) GetAdminOrder(ctx context.Context, orderID uuid.UUID) (AdminOrder, error) {
	detail, err := s.detailUnchecked(ctx, orderID)
	if err != nil {
		return AdminOrder{}, err
	}
	q := db.New(s.pool)
	buyer, err := q.GetUserByID(ctx, detail.BuyerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminOrder{}, fmt.Errorf("%w: %s", ErrNotFound, orderID)
	}
	if err != nil {
		return AdminOrder{}, fmt.Errorf("get buyer: %w", err)
	}
	seller, err := q.GetUserByID(ctx, detail.SellerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminOrder{}, fmt.Errorf("%w: %s", ErrNotFound, orderID)
	}
	if err != nil {
		return AdminOrder{}, fmt.Errorf("get seller: %w", err)
	}
	rows, err := q.ListLedgerEntriesForOrder(ctx, pgtype.UUID{Bytes: orderID, Valid: true})
	if err != nil {
		return AdminOrder{}, fmt.Errorf("list ledger entries: %w", err)
	}
	entries := make([]LedgerEntryView, 0, len(rows))
	for _, row := range rows {
		entries = append(entries, LedgerEntryView{
			Account: row.Account, Amount: row.Amount, Currency: row.Currency,
			TransactionKind: row.TransactionKind, TransactionReference: row.TransactionReference,
			CreatedAt: row.CreatedAt,
		})
	}
	return AdminOrder{
		Order:   detail,
		Buyer:   PartyContact{UserID: buyer.ID, DisplayName: buyer.DisplayName, Email: buyer.Email, Phone: buyer.PhoneE164},
		Seller:  PartyContact{UserID: seller.ID, DisplayName: seller.DisplayName, Email: seller.Email, Phone: seller.PhoneE164},
		Entries: entries,
	}, nil
}

// resolveTarget maps a dispute outcome onto DOMAIN §4's target status.
func resolveTarget(outcome string) string {
	if outcome == DisputeOutcomeRefundBuyer {
		return StatusRefunded
	}
	return StatusCompleted
}

// friendlyOutcome renders an outcome for the resolution SMS.
func friendlyOutcome(outcome string) string {
	switch outcome {
	case DisputeOutcomeRefundBuyer:
		return "refund"
	case DisputeOutcomeReleaseSeller:
		return "release"
	default:
		return "partial refund"
	}
}

// fromDisputeRow maps the generated row onto the domain type.
func fromDisputeRow(r db.Dispute) Dispute {
	d := Dispute{
		ID: r.ID, OrderID: r.OrderID, OpenedBy: r.OpenedBy, Reason: r.Reason,
		Description: r.Description, Status: r.Status, Outcome: r.Outcome,
		RefundPesewas: r.RefundPesewas, ResolutionNote: r.ResolutionNote,
		ResolvedAt: r.ResolvedAt, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
	if r.ResolvedBy.Valid {
		id := uuid.UUID(r.ResolvedBy.Bytes)
		d.ResolvedBy = &id
	}
	return d
}
