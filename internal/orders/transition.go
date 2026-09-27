package orders

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/db"
	"github.com/dezmymachine/farmish-backend/internal/jobs"
	"github.com/dezmymachine/farmish-backend/internal/notify"
)

// Service owns the order state machine, its side effects and the order reads.
type Service struct {
	// pool backs the reads; the writes always take the caller's transaction.
	pool *pgxpool.Pool
	// autoComplete is how long after delivery an undisputed order completes
	// (DOMAIN §3: 3 days).
	autoComplete time.Duration
	// sellerAcceptTimeout is how long a paid order waits for acceptance
	// before the system cancels it (DOMAIN §3: 48 hours).
	sellerAcceptTimeout time.Duration
	// jobs enqueues refund, release and notify jobs inside the caller's
	// transaction. Nil disables enqueuing (unit tests of the table alone).
	jobs *jobs.Client
	// Now is the clock, injectable so the timer tests never sleep.
	Now func() time.Time
}

// NewService returns the state machine with its timers over a pool
// (DOMAIN §3: 48 hours to accept, 3 days to auto-complete).
func NewService(pool *pgxpool.Pool, sellerAcceptTimeout, autoComplete time.Duration) *Service {
	return &Service{
		pool: pool, autoComplete: autoComplete, sellerAcceptTimeout: sellerAcceptTimeout,
		Now: time.Now,
	}
}

// AttachJobClient gives the service the River client it needs to enqueue
// refund, release and notify jobs inside the business transaction. cmd/api
// calls it once, after the registry exists.
func (s *Service) AttachJobClient(client *jobs.Client) { s.jobs = client }

// RefundNeededArgs queues the buyer's money back. Phase 17a replaces the
// log-only worker with the real Paystack refund.
type RefundNeededArgs struct {
	OrderID       uuid.UUID `json:"orderId"`
	AmountPesewas int64     `json:"amountPesewas"`
}

// Kind implements river.JobArgs.
func (RefundNeededArgs) Kind() string { return "orders.refund_needed" }

// ReleaseNeededArgs queues the escrow release to the seller. Phase 17a
// replaces the log-only worker with the real release posting.
type ReleaseNeededArgs struct {
	OrderID       uuid.UUID `json:"orderId"`
	AmountPesewas int64     `json:"amountPesewas"`
}

// Kind implements river.JobArgs.
func (ReleaseNeededArgs) Kind() string { return "orders.release_needed" }

// Transition moves one order, inside the caller's transaction.
//
// It locks the row, validates (from, to, actor) against DOMAIN §4, updates the
// status and the target status's timestamp, writes the order event, and
// returns the side effects the caller must execute in the same transaction.
// ErrInvalidTransition means the move is not in the table; ErrForbidden means
// the actor type is right but this actor is the wrong party.
func (s *Service) Transition(ctx context.Context, tx pgx.Tx, orderID uuid.UUID, to string, actor Actor, note string) (Order, []SideEffect, error) {
	q := db.New(tx)
	current, err := q.GetOrderForUpdate(ctx, orderID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, nil, fmt.Errorf("%w: %s", ErrNotFound, orderID)
	}
	if err != nil {
		return Order{}, nil, fmt.Errorf("lock order: %w", err)
	}
	order := fromRow(current)
	allowed, inTable := transitionTable[order.Status][to]
	switch {
	case !inTable:
		return Order{}, nil, &InvalidTransitionError{From: order.Status, To: to}
	case !actorAllowed(allowed, actor, order):
		if typeAllowed(allowed, actor) {
			// The move exists in DOMAIN §4 but not for this party: the order
			// is real for the caller, so this is a 403, not a 404.
			return Order{}, nil, &ForbiddenError{ActorType: actor.Type}
		}
		return Order{}, nil, &InvalidTransitionError{From: order.Status, To: to}
	}

	updated, err := q.SetOrderStatus(ctx, db.SetOrderStatusParams{
		ID: orderID, Status: to, Status_2: order.Status,
	})
	if err != nil {
		return Order{}, nil, fmt.Errorf("set order status: %w", err)
	}
	now := s.Now()
	if err := s.stampStatus(ctx, tx, orderID, to, note == StatusShipped, now); err != nil {
		return Order{}, nil, err
	}
	if err := writeEvent(ctx, q, orderID, &order.Status, to, actor, note); err != nil {
		return Order{}, nil, err
	}
	moved := fromRow(updated)
	return moved, effectsFor(moved, to, actor.Type), nil
}

// stampStatus writes the target status's timestamp. Delivered also starts the
// auto-complete clock, because both are columns of the same row: doing it
// here keeps the deadline atomic with the move.
func (s *Service) stampStatus(ctx context.Context, tx pgx.Tx, orderID uuid.UUID, to string, _ bool, now time.Time) error {
	q := db.New(tx)
	switch to {
	case StatusAccepted:
		if err := q.SetOrderAcceptedAt(ctx, db.SetOrderAcceptedAtParams{ID: orderID, AcceptedAt: &now}); err != nil {
			return fmt.Errorf("set accepted_at: %w", err)
		}
	case StatusShipped:
		if err := q.SetOrderShipped(ctx, db.SetOrderShippedParams{ID: orderID, ShippedAt: &now}); err != nil {
			return fmt.Errorf("set shipped_at: %w", err)
		}
	case StatusDelivered:
		deadline := now.Add(s.autoComplete)
		if err := q.SetOrderDeliveredAt(ctx, db.SetOrderDeliveredAtParams{
			ID: orderID, DeliveredAt: &now, AutoCompleteAt: &deadline,
		}); err != nil {
			return fmt.Errorf("set delivered_at: %w", err)
		}
	case StatusCompleted:
		if err := q.SetOrderCompletedAt(ctx, db.SetOrderCompletedAtParams{ID: orderID, CompletedAt: &now}); err != nil {
			return fmt.Errorf("set completed_at: %w", err)
		}
	case StatusCancelled:
		if err := q.SetOrderCancelledAt(ctx, db.SetOrderCancelledAtParams{ID: orderID, CancelledAt: &now}); err != nil {
			return fmt.Errorf("set cancelled_at: %w", err)
		}
	}
	return nil
}

// ApplyEffects executes a transition's side effects inside the same
// transaction. Phase 15b's confirm path and the endpoints share it, so the
// effects can never drift from the table.
func (s *Service) ApplyEffects(ctx context.Context, tx pgx.Tx, order Order, effects []SideEffect) error {
	q := db.New(tx)
	for _, effect := range effects {
		switch effect.Kind {
		case EffectRestoreStock:
			if err := restoreStock(ctx, q, order.ID); err != nil {
				return err
			}
		case EffectEnqueueRefund:
			if s.jobs == nil {
				continue
			}
			if _, err := s.jobs.InsertTx(ctx, tx, RefundNeededArgs{
				OrderID: order.ID, AmountPesewas: order.BasePesewas,
			}, jobs.Unique()); err != nil {
				return fmt.Errorf("enqueue refund needed: %w", err)
			}
		case EffectEnqueueRelease:
			if s.jobs == nil {
				continue
			}
			if _, err := s.jobs.InsertTx(ctx, tx, ReleaseNeededArgs{
				OrderID: order.ID, AmountPesewas: order.BasePesewas,
			}, jobs.Unique()); err != nil {
				return fmt.Errorf("enqueue release needed: %w", err)
			}
		case EffectNotify:
			if s.jobs == nil {
				continue
			}
			if _, err := s.jobs.InsertTx(ctx, tx, notify.SMSArgs{
				UserID: effect.Recipient, Template: effect.Template,
				Params: map[string]string{
					"orderId":      order.ID.String(),
					"orderIdShort": order.ID.String()[:8],
				},
			}, nil); err != nil {
				return fmt.Errorf("enqueue notification: %w", err)
			}
		}
	}
	return nil
}

// restoreStock returns exactly the units the order took, from the stored items
// rather than the request, in ascending listing order.
func restoreStock(ctx context.Context, q *db.Queries, orderID uuid.UUID) error {
	items, err := q.ListOrderItemsByOrder(ctx, orderID)
	if err != nil {
		return fmt.Errorf("list order items: %w", err)
	}
	for _, item := range items {
		if err := q.RestoreListingStock(ctx, db.RestoreListingStockParams{
			ID: item.ListingID, QuantityAvailable: item.Quantity,
		}); err != nil {
			return fmt.Errorf("restore stock: %w", err)
		}
	}
	return nil
}

// MarkPaidAt stamps escrow and payment on an order Transition just moved to
// paid. Escrow is a side effect the caller owns: 15b's confirm path sets held,
// and the admin dispute resolution in 17b will set its own states.
func (s *Service) MarkPaidAt(ctx context.Context, tx pgx.Tx, orderID uuid.UUID) error {
	now := s.Now()
	if err := db.New(tx).SetOrderPaid(ctx, db.SetOrderPaidParams{
		ID: orderID, PaidAt: &now,
	}); err != nil {
		return fmt.Errorf("mark order paid: %w", err)
	}
	return nil
}

// SetEscrowState records the escrow side of a move the caller has applied.
func (s *Service) SetEscrowState(ctx context.Context, tx pgx.Tx, orderID uuid.UUID, state string) error {
	if err := db.New(tx).SetOrderEscrowState(ctx, db.SetOrderEscrowStateParams{
		ID: orderID, EscrowState: state,
	}); err != nil {
		return fmt.Errorf("set escrow state: %w", err)
	}
	return nil
}

func writeEvent(ctx context.Context, q *db.Queries, orderID uuid.UUID, from *string, to string, actor Actor, note string) error {
	if err := q.InsertOrderEvent(ctx, db.InsertOrderEventParams{
		OrderID: orderID, FromStatus: from, ToStatus: to,
		ActorType: actor.Type, ActorID: actorUUID(actor.ID), Note: noteOrNil(note),
	}); err != nil {
		return fmt.Errorf("insert order event: %w", err)
	}
	return nil
}

func noteOrNil(note string) *string {
	if note == "" {
		return nil
	}
	return &note
}

func actorUUID(id *uuid.UUID) pgtype.UUID {
	if id == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: *id, Valid: true}
}

// fromRow maps the generated row onto the domain type.
func fromRow(r db.Order) Order {
	return Order{
		ID: r.ID, CheckoutID: r.CheckoutID, BuyerID: r.BuyerID, SellerID: r.SellerID,
		Status: r.Status, EscrowState: r.EscrowState,
		SubtotalPesewas: r.SubtotalPesewas, DeliveryFeePesewas: r.DeliveryFeePesewas,
		BasePesewas: r.BasePesewas, CommissionRateBps: r.CommissionRateBps,
		CommissionPesewas: r.CommissionPesewas, RefundedPesewas: r.RefundedPesewas,
		DeliveryMethod: r.DeliveryMethod, DeliveryAddress: r.DeliveryAddress,
		DeliveryRegion: r.DeliveryRegion, DeliveryDistrict: r.DeliveryDistrict,
		RecipientName: r.RecipientName, RecipientPhone: r.RecipientPhone,
		TrackingRef: r.TrackingRef, PaidAt: r.PaidAt, AcceptedAt: r.AcceptedAt,
		ShippedAt: r.ShippedAt, DeliveredAt: r.DeliveredAt, CompletedAt: r.CompletedAt,
		CancelledAt: r.CancelledAt, AutoCompleteAt: r.AutoCompleteAt,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}
