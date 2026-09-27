package orders

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/dezmymachine/farmish-backend/internal/db"
)

// SideEffects are the actions DOMAIN §4 attaches to a transition. The caller
// executes them in the same transaction, because only it knows whether the
// transition came from a job, an endpoint or a recovery path.
type SideEffects struct {
	// RestoreStock is true when the order's reserved units go back on sale.
	RestoreStock bool
	// RefundNeeded is true when the buyer's money must come back.
	RefundNeeded bool
	// NotifySeller is the Phase 16 call site: the transition should tell the
	// seller. Phase 15b has no notification channel yet, so it is a no-op.
	NotifySeller bool
}

// Transition moves one order to a new status, inside the caller's transaction.
//
// It locks the row, checks DOMAIN §4, writes the status (and the target
// status's timestamp), appends the order event, and returns the side effects
// the caller must apply in the same transaction. Anything not in the table is
// ErrInvalidTransition, which the endpoint maps to 409.
func Transition(ctx context.Context, tx pgx.Tx, orderID uuid.UUID, to, actorType string, actorID *uuid.UUID, note string) (Order, SideEffects, error) {
	q := db.New(tx)
	current, err := q.GetOrderForUpdate(ctx, orderID)
	if err != nil {
		return Order{}, SideEffects{}, fmt.Errorf("lock order: %w", err)
	}
	if !allowedTransitions[current.Status][to] {
		return Order{}, SideEffects{}, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, current.Status, to)
	}
	updated, err := q.SetOrderStatus(ctx, db.SetOrderStatusParams{
		ID: orderID, Status: to, Status_2: current.Status,
	})
	if err != nil {
		return Order{}, SideEffects{}, fmt.Errorf("set order status: %w", err)
	}
	if err := writeEvent(ctx, q, orderID, &current.Status, to, actorType, actorID, note); err != nil {
		return Order{}, SideEffects{}, err
	}
	return fromRow(updated), sideEffectsFor(to), nil
}

// MarkPaidAt stamps the escrow and payment columns on an order that just became
// paid. Transition itself only owns status and events; escrow_state depends on
// why the order moved (DOMAIN §4 lists it as a side effect).
func MarkPaidAt(ctx context.Context, tx pgx.Tx, orderID uuid.UUID, now time.Time) error {
	if err := db.New(tx).SetOrderPaid(ctx, db.SetOrderPaidParams{
		ID: orderID, PaidAt: &now,
	}); err != nil {
		return fmt.Errorf("mark order paid: %w", err)
	}
	return nil
}

// MarkCancelledAt stamps when an order was cancelled, after Transition moved it.
func MarkCancelledAt(ctx context.Context, tx pgx.Tx, orderID uuid.UUID, now time.Time) error {
	if err := db.New(tx).SetOrderCancelledAt(ctx, db.SetOrderCancelledAtParams{
		ID: orderID, CancelledAt: &now,
	}); err != nil {
		return fmt.Errorf("mark order cancelled: %w", err)
	}
	return nil
}

// SetEscrowState records the escrow side of a transition the caller has already
// applied, such as refund_pending on a cancelled order whose payment arrived.
func SetEscrowState(ctx context.Context, tx pgx.Tx, orderID uuid.UUID, state string) error {
	if err := db.New(tx).SetOrderEscrowState(ctx, db.SetOrderEscrowStateParams{
		ID: orderID, EscrowState: state,
	}); err != nil {
		return fmt.Errorf("set escrow state: %w", err)
	}
	return nil
}

// NotifySeller is the Phase 16 call site. It is deliberately empty: 15b has no
// notification channel, and 16 will implement SMS + in-app notices here.
func NotifySeller(ctx context.Context, orderID uuid.UUID) {
	_ = ctx
	_ = orderID
}

// sideEffectsFor returns the DOMAIN §4 side effects of entering a status.
func sideEffectsFor(to string) SideEffects {
	switch to {
	case StatusPaid:
		return SideEffects{NotifySeller: true}
	case StatusExpired:
		return SideEffects{RestoreStock: true}
	case StatusCancelled:
		return SideEffects{RefundNeeded: true}
	default:
		return SideEffects{}
	}
}

func writeEvent(ctx context.Context, q *db.Queries, orderID uuid.UUID, from *string, to, actorType string, actorID *uuid.UUID, note string) error {
	var fromStatus *string
	if from != nil {
		value := *from
		fromStatus = &value
	}
	if err := q.InsertOrderEvent(ctx, db.InsertOrderEventParams{
		OrderID: orderID, FromStatus: fromStatus, ToStatus: to,
		ActorType: actorType, ActorID: actorUUID(actorID), Note: noteOrNil(note),
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
