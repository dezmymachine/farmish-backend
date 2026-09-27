package payments

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dezmymachine/farmish-backend/internal/database"
	"github.com/dezmymachine/farmish-backend/internal/db"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

// CreatePendingInTx inserts a pending payment inside the caller's transaction.
//
// Checkout uses this so the payment row commits atomically with the checkout,
// its orders and the stock reservations (Phase 15b); Initialize, by contrast,
// owns its own transactions because a promotion purchase has nothing to keep
// atomic (ADR-0020). The gross-up uses the service's configured fee rate, so
// the row satisfies the charge = base + fee CHECK.
//
// The provider is never called here: that happens after the transaction
// commits, through the returned payment's reference.
func (s *Service) CreatePendingInTx(ctx context.Context, tx pgx.Tx, in CreateInput) (Payment, error) {
	var verr validation.Error
	if in.UserID == uuid.Nil {
		verr.Add("userId", "is required")
	}
	if !validPurpose(in.Purpose) {
		verr.Add("purpose", "must be promotion or checkout")
	}
	if in.PurposeRef == "" {
		verr.Add("purposeRef", "is required")
	}
	if in.BasePesewas <= 0 {
		verr.Add("baseAmount", "must be more than 0")
	}
	if err := verr.OrNil(); err != nil {
		return Payment{}, err
	}
	charge, fee, err := grossUp(in.BasePesewas, s.feeBps)
	if err != nil {
		return Payment{}, err
	}
	reference, err := newReference()
	if err != nil {
		return Payment{}, err
	}
	metadata, err := marshalMetadata(in.Metadata)
	if err != nil {
		return Payment{}, err
	}
	row, err := db.New(tx).InsertPayment(ctx, db.InsertPaymentParams{
		Reference: reference, UserID: in.UserID, Purpose: in.Purpose,
		PurposeRef: in.PurposeRef, BasePesewas: in.BasePesewas,
		ProcessingFeePesewas: fee, ChargePesewas: charge, Metadata: metadata,
	})
	if err != nil {
		return Payment{}, fmt.Errorf("insert payment: %w", err)
	}
	return fromRow(row), nil
}

// MarkFailedInTx marks a pending payment failed inside the caller's
// transaction. A payment that already reached another state is left alone: a
// webhook or the verify fallback may have won the race.
func (s *Service) MarkFailedInTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, reason string) error {
	if _, err := db.New(tx).MarkPaymentFailed(ctx, db.MarkPaymentFailedParams{
		ID: id, FailureReason: optionalString(reason),
	}); err != nil {
		return fmt.Errorf("mark payment failed: %w", err)
	}
	return nil
}

// MarkAbandonedInTx marks a pending payment abandoned inside the caller's
// transaction, for the checkout expiry sweep.
func (s *Service) MarkAbandonedInTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, reason string) error {
	if _, err := db.New(tx).MarkPaymentAbandoned(ctx, db.MarkPaymentAbandonedParams{
		ID: id, FailureReason: optionalString(reason),
	}); err != nil {
		return fmt.Errorf("mark payment abandoned: %w", err)
	}
	return nil
}

// SetAuthorizationURL stores where the buyer completes the payment. It runs in
// its own transaction because the provider call happened outside one.
func (s *Service) SetAuthorizationURL(ctx context.Context, id uuid.UUID, url string) error {
	return database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := db.New(tx).SetPaymentAuthorizationURL(ctx, db.SetPaymentAuthorizationURLParams{
			ID: id, AuthorizationUrl: optionalString(url),
		}); err != nil {
			return fmt.Errorf("store authorization url: %w", err)
		}
		return nil
	})
}

// GetByReference loads a payment by its reference, for callers that need the
// row without ownership checks (the checkout confirm path already knows the
// buyer from the checkout row).
func (s *Service) GetByReference(ctx context.Context, reference string) (Payment, error) {
	row, err := db.New(s.pool).GetPaymentByReference(ctx, reference)
	if errors.Is(err, pgx.ErrNoRows) {
		return Payment{}, fmt.Errorf("%w: %s", ErrNotFound, reference)
	}
	if err != nil {
		return Payment{}, fmt.Errorf("get payment: %w", err)
	}
	return fromRow(row), nil
}
