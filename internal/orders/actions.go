package orders

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dezmymachine/farmish-backend/internal/database"
	"github.com/dezmymachine/farmish-backend/internal/db"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

// Dispute reasons stored in disputes.reason.
const (
	DisputeNotReceived    = "not_received"
	DisputeNotAsDescribed = "not_as_described"
	DisputeDamaged        = "damaged"
	DisputeWrongItem      = "wrong_item"
	DisputeOther          = "other"
)

// validDisputeReasons mirrors the disputes CHECK.
var validDisputeReasons = map[string]bool{
	DisputeNotReceived: true, DisputeNotAsDescribed: true, DisputeDamaged: true,
	DisputeWrongItem: true, DisputeOther: true,
}

// Accept moves a paid order to accepted. DOMAIN §4: only the seller.
func (s *Service) Accept(ctx context.Context, sellerID, orderID uuid.UUID) (Order, error) {
	return s.act(ctx, Seller(sellerID), orderID, StatusAccepted, "")
}

// Reject refuses a paid or accepted order: the buyer's money comes back and
// the reserved stock is restored. A reason is required, for the event trail.
func (s *Service) Reject(ctx context.Context, sellerID, orderID uuid.UUID, reason string) (Order, error) {
	if strings.TrimSpace(reason) == "" {
		var invalid validation.Error
		invalid.Add("reason", "is required")
		return Order{}, invalid.OrNil()
	}
	if len(reason) > 500 {
		var invalid validation.Error
		invalid.Add("reason", "must be at most 500 characters")
		return Order{}, invalid.OrNil()
	}
	return s.act(ctx, Seller(sellerID), orderID, StatusCancelled, reason)
}

// Ship moves an accepted order to shipped, optionally recording a tracking
// reference.
func (s *Service) Ship(ctx context.Context, sellerID, orderID uuid.UUID, trackingRef string) (Order, error) {
	if len(trackingRef) > 120 {
		var invalid validation.Error
		invalid.Add("trackingRef", "must be at most 120 characters")
		return Order{}, invalid.OrNil()
	}
	moved, err := s.act(ctx, Seller(sellerID), orderID, StatusShipped, "")
	if err != nil {
		return moved, err
	}
	if trackingRef == "" {
		return moved, nil
	}
	return moved, database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := db.New(tx).SetOrderTrackingRef(ctx, db.SetOrderTrackingRefParams{
			ID: orderID, TrackingRef: &trackingRef,
		}); err != nil {
			return fmt.Errorf("set tracking ref: %w", err)
		}
		return nil
	})
}

// MarkDelivered moves a shipped order to delivered and starts the 3-day
// auto-complete clock.
func (s *Service) MarkDelivered(ctx context.Context, sellerID, orderID uuid.UUID) (Order, error) {
	return s.act(ctx, Seller(sellerID), orderID, StatusDelivered, "")
}

// Cancel is the buyer backing out before acceptance: the stock goes back and
// the seller is told the sale is off.
func (s *Service) Cancel(ctx context.Context, buyerID, orderID uuid.UUID, reason string) (Order, error) {
	if len(reason) > 500 {
		var invalid validation.Error
		invalid.Add("reason", "must be at most 500 characters")
		return Order{}, invalid.OrNil()
	}
	return s.act(ctx, Buyer(buyerID), orderID, StatusCancelled, reason)
}

// ConfirmReceipt is the buyer closing a shipped or delivered order, which
// queues the escrow release.
func (s *Service) ConfirmReceipt(ctx context.Context, buyerID, orderID uuid.UUID) (Order, error) {
	return s.act(ctx, Buyer(buyerID), orderID, StatusCompleted, "")
}

// Dispute opens a case on a shipped or delivered order. The disputes row stops
// the auto-complete timer; escrow stays held until 17b resolves the case.
func (s *Service) Dispute(ctx context.Context, buyerID, orderID uuid.UUID, reason, description string) (Order, error) {
	var invalid validation.Error
	if !validDisputeReasons[reason] {
		invalid.Add("reason", "must be not_received, not_as_described, damaged, wrong_item or other")
	}
	trimmed := strings.TrimSpace(description)
	if len(trimmed) < 10 || len(trimmed) > 2000 {
		invalid.Add("description", "must be between 10 and 2000 characters")
	}
	if err := invalid.OrNil(); err != nil {
		return Order{}, err
	}
	moved, err := s.act(ctx, Buyer(buyerID), orderID, StatusDisputed, reason+": "+trimmed)
	if err != nil {
		return moved, err
	}
	return moved, database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := db.New(tx).InsertDispute(ctx, db.InsertDisputeParams{
			OrderID: orderID, OpenedBy: buyerID, Reason: reason, Description: trimmed,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrDisputeExists
			}
			return fmt.Errorf("insert dispute: %w", err)
		}
		return nil
	})
}

// act runs one transition with its effects in a single transaction, then
// applies the escrow column the transition implies for a cancellation:
// refund-pending, so the refund job can post the money back. A completed
// order's escrow stays held until the release job (Phase 17a) actually posts
// the ledger entries; only then does escrow_state become released.
func (s *Service) act(ctx context.Context, actor Actor, orderID uuid.UUID, to, note string) (Order, error) {
	// Role before state: the caller must be this order's buyer or seller for
	// the move they attempt. The other party gets 403, a stranger gets 404,
	// and only then does the table decide 409. System moves skip the check.
	// The parties come from the bare order row, not the detail read: the
	// detail joins the seller profile, which is a read concern, not a guard.
	if actor.ID != nil && (actor.Type == ActorBuyer || actor.Type == ActorSeller) {
		row, err := db.New(s.pool).GetOrderByID(ctx, orderID)
		if errors.Is(err, pgx.ErrNoRows) {
			return Order{}, fmt.Errorf("%w: %s", ErrNotFound, orderID)
		}
		if err != nil {
			return Order{}, fmt.Errorf("get order parties: %w", err)
		}
		if row.BuyerID != *actor.ID && row.SellerID != *actor.ID {
			return Order{}, fmt.Errorf("%w: %s", ErrNotFound, orderID)
		}
		if (row.SellerID == *actor.ID) != (actor.Type == ActorSeller) {
			return Order{}, &ForbiddenError{ActorType: actor.Type}
		}
	}
	var moved Order
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		order, effects, err := s.Transition(ctx, tx, orderID, to, actor, note)
		if err != nil {
			return err
		}
		moved = order
		if to == StatusCancelled {
			if err := s.SetEscrowState(ctx, tx, order.ID, EscrowRefundPending); err != nil {
				return err
			}
		}
		return s.ApplyEffects(ctx, tx, order, effects)
	})
	if err != nil {
		return Order{}, err
	}
	return moved, nil
}
