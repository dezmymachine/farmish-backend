// Package orders owns the order state machine (DOMAIN §4) and the buyer and
// seller order reads. Phase 15b implements only the transitions checkout needs;
// Phase 16 builds the full actor-driven table.
package orders

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Statuses stored in orders.status (DOMAIN §4).
const (
	StatusPendingPayment = "pending_payment"
	StatusPaid           = "paid"
	StatusAccepted       = "accepted"
	StatusShipped        = "shipped"
	StatusDelivered      = "delivered"
	StatusCompleted      = "completed"
	StatusCancelled      = "cancelled"
	StatusDisputed       = "disputed"
	StatusRefunded       = "refunded"
	StatusExpired        = "expired"
)

// Escrow states stored in orders.escrow_state.
const (
	EscrowNone              = "none"
	EscrowHeld              = "held"
	EscrowReleased          = "released"
	EscrowRefundPending     = "refund_pending"
	EscrowRefunded          = "refunded"
	EscrowPartiallyRefunded = "partially_refunded"
)

// Actors that may drive a transition (order_events.actor_type).
const (
	ActorBuyer  = "buyer"
	ActorSeller = "seller"
	ActorAdmin  = "admin"
	ActorSystem = "system"
)

var (
	// ErrNotFound means no order matches, or the caller has no business
	// knowing it exists. Both cases read as 404.
	ErrNotFound = errors.New("order not found")
	// ErrInvalidTransition means the requested move is not in DOMAIN §4.
	ErrInvalidTransition = errors.New("invalid order status transition")
)

// Order is a row of the orders table with its delivery details.
type Order struct {
	ID                 uuid.UUID
	CheckoutID         uuid.UUID
	BuyerID            uuid.UUID
	SellerID           uuid.UUID
	Status             string
	EscrowState        string
	SubtotalPesewas    int64
	DeliveryFeePesewas int64
	BasePesewas        int64
	CommissionRateBps  int32
	CommissionPesewas  int64
	RefundedPesewas    int64
	DeliveryMethod     string
	DeliveryAddress    *string
	DeliveryRegion     *string
	DeliveryDistrict   *string
	RecipientName      *string
	RecipientPhone     *string
	TrackingRef        *string
	PaidAt             *time.Time
	AcceptedAt         *time.Time
	ShippedAt          *time.Time
	DeliveredAt        *time.Time
	CompletedAt        *time.Time
	CancelledAt        *time.Time
	AutoCompleteAt     *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// Item is one priced line of an order, snapshotted at checkout.
type Item struct {
	ID               uuid.UUID
	OrderID          uuid.UUID
	ListingID        uuid.UUID
	Title            string
	Unit             string
	UnitPricePesewas int64
	Quantity         int32
	LineTotalPesewas int64
}

// Event is one append-only order_events row.
type Event struct {
	ID         int64
	OrderID    uuid.UUID
	FromStatus *string
	ToStatus   string
	ActorType  string
	ActorID    *uuid.UUID
	Note       *string
	CreatedAt  time.Time
}

// allowedTransitions is DOMAIN §4, restricted to the system transitions Phase
// 15b needs. Phase 16 replaces this with the full table and its actor checks.
var allowedTransitions = map[string]map[string]bool{
	StatusPendingPayment: {
		StatusPaid:      true,
		StatusExpired:   true,
		StatusCancelled: true,
	},
	// Late payment recovery (DOMAIN §4, owner decision 2026-09-26): a
	// charge.success for an expired checkout either revives the orders, when
	// the stock could be re-reserved, or cancels them for a full refund.
	StatusExpired: {
		StatusPaid:      true,
		StatusCancelled: true,
	},
}
