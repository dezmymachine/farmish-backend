// Package orders owns the order state machine (DOMAIN §4) and the buyer and
// seller order reads. Phase 16 completes the table; the actors are enforced
// per move, and every transition returns the side effects the caller must
// execute in the same transaction.
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

// AllStatuses is every status, for the full-table test.
var AllStatuses = []string{
	StatusPendingPayment, StatusPaid, StatusAccepted, StatusShipped, StatusDelivered,
	StatusCompleted, StatusCancelled, StatusDisputed, StatusRefunded, StatusExpired,
}

// Escrow states stored in orders.escrow_state.
const (
	EscrowNone              = "none"
	EscrowHeld              = "held"
	EscrowReleased          = "released"
	EscrowRefundPending     = "refund_pending"
	EscrowRefunded          = "refunded"
	EscrowPartiallyRefunded = "partially_refunded"
)

// Actor types (order_events.actor_type).
const (
	ActorBuyer  = "buyer"
	ActorSeller = "seller"
	ActorAdmin  = "admin"
	ActorSystem = "system"
)

// AllActorTypes is every actor type, for the full-table test.
var AllActorTypes = []string{ActorBuyer, ActorSeller, ActorAdmin, ActorSystem}

// Refund reasons stored in refunds.reason (Phase 17a schema). dispute_refund
// and dispute_partial are Phase 17b's; they exist here only because the CHECK
// constraint and the schema are this phase's.
const (
	RefundReasonSellerRejected   = "seller_rejected"
	RefundReasonBuyerCancelled   = "buyer_cancelled"
	RefundReasonSellerTimeout    = "seller_timeout"
	RefundReasonStockUnavailable = "stock_unavailable"
	RefundReasonDisputeRefund    = "dispute_refund"
	RefundReasonDisputePartial   = "dispute_partial"
)

// Refund statuses stored in refunds.status (Phase 17a schema).
const (
	RefundStatusQueued    = "queued"
	RefundStatusPending   = "pending"
	RefundStatusProcessed = "processed"
	RefundStatusFailed    = "failed"
)

var (
	// ErrNotFound means no order matches, or the caller is not a party to it.
	// Both read as 404 so order ids cannot be probed.
	ErrNotFound = errors.New("order not found")
	// ErrInvalidTransition means the move is not in DOMAIN §4.
	ErrInvalidTransition = errors.New("invalid order status transition")
	// ErrForbidden means the actor type may make this move in general, but
	// this particular actor may not: a buyer calling a seller move on their
	// own purchase, or a stranger on any order.
	ErrForbidden = errors.New("actor may not perform this transition")
	// ErrDisputeExists means the order already has a dispute.
	ErrDisputeExists = errors.New("order already has a dispute")
)

// InvalidTransitionError carries the from and to the contract's 409 details
// report, so the client can say what it tried and what the order was.
type InvalidTransitionError struct {
	From, To string
}

func (e *InvalidTransitionError) Error() string {
	return ErrInvalidTransition.Error() + ": " + e.From + " -> " + e.To
}

func (e *InvalidTransitionError) Is(target error) bool {
	return target == ErrInvalidTransition
}

// ForbiddenError identifies the actor the table allowed, but on the wrong
// order: a buyer calling a seller move on their own purchase.
type ForbiddenError struct {
	ActorType string
}

func (e *ForbiddenError) Error() string {
	return ErrForbidden.Error() + ": " + e.ActorType
}

func (e *ForbiddenError) Is(target error) bool {
	return target == ErrForbidden
}

// Actor is who is performing a transition. ID is nil for system actions.
type Actor struct {
	Type string
	ID   *uuid.UUID
}

// Buyer builds the buyer actor for a user id.
func Buyer(id uuid.UUID) Actor { return Actor{Type: ActorBuyer, ID: &id} }

// Seller builds the seller actor for a user id.
func Seller(id uuid.UUID) Actor { return Actor{Type: ActorSeller, ID: &id} }

// System builds the system actor.
func System() Actor { return Actor{Type: ActorSystem} }

// Order is a row of the orders table.
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

// EffectKind is what the caller must do in the same transaction.
type EffectKind int

const (
	// EffectRestoreStock returns the order's reserved units to the listings.
	EffectRestoreStock EffectKind = iota
	// EffectEnqueueRefund queues the buyer's money back. The worker only
	// logs until Phase 17a replaces it with the real refund.
	EffectEnqueueRefund
	// EffectEnqueueRelease queues the escrow release to the seller. The
	// worker only logs until Phase 17a replaces it.
	EffectEnqueueRelease
	// EffectNotify sends the other party an SMS.
	EffectNotify
)

// SideEffect is one instruction from DOMAIN §4's rightmost column.
type SideEffect struct {
	Kind EffectKind
	// Template names a notify template (EffectNotify only).
	Template string
	// Recipient is the user to notify (EffectNotify only).
	Recipient uuid.UUID
	// Reason is the refunds.reason value to record (EffectEnqueueRefund only).
	Reason string
}

// transitionTable is DOMAIN §4, verbatim: from → to → the actor types that
// may perform the move. Anything absent is illegal. TestTransition_FullTable
// checks this literal against a hand-copied table, so a typo cannot survive.
var transitionTable = map[string]map[string][]string{
	StatusPendingPayment: {
		StatusPaid:    {ActorSystem},
		StatusExpired: {ActorSystem},
	},
	StatusExpired: {
		StatusPaid:      {ActorSystem},
		StatusCancelled: {ActorSystem},
	},
	StatusPaid: {
		StatusAccepted:  {ActorSeller},
		StatusCancelled: {ActorSeller, ActorBuyer, ActorSystem},
	},
	StatusAccepted: {
		StatusCancelled: {ActorSeller},
		StatusShipped:   {ActorSeller},
	},
	StatusShipped: {
		StatusDelivered: {ActorSeller},
		StatusCompleted: {ActorBuyer},
		StatusDisputed:  {ActorBuyer},
	},
	StatusDelivered: {
		StatusCompleted: {ActorBuyer, ActorSystem},
		StatusDisputed:  {ActorBuyer},
	},
	StatusDisputed: {
		StatusRefunded:  {ActorAdmin},
		StatusCompleted: {ActorAdmin},
	},
}

// actorAllowed reports whether the actor may make this move: the type must be
// in the table, and a buyer or seller must be this order's buyer or seller.
func actorAllowed(actorTypes []string, actor Actor, order Order) bool {
	for _, actorType := range actorTypes {
		if actor.Type != actorType {
			continue
		}
		switch actor.Type {
		case ActorBuyer:
			return actor.ID != nil && order.BuyerID == *actor.ID
		case ActorSeller:
			return actor.ID != nil && order.SellerID == *actor.ID
		default:
			// Admin and system moves are not tied to a party.
			return true
		}
	}
	return false
}

// typeAllowed reports whether the actor type appears in the table for this
// move, without the party check: the difference is ErrForbidden (right type,
// wrong party) versus ErrInvalidTransition (move not in the table).
func typeAllowed(actorTypes []string, actor Actor) bool {
	for _, actorType := range actorTypes {
		if actor.Type == actorType {
			return true
		}
	}
	return false
}

// effectsFor returns DOMAIN §4's side effects for a move. The *other* party is
// the one who gets an SMS; the system tells both.
func effectsFor(order Order, to, actorType string) []SideEffect {
	switch to {
	case StatusPaid:
		return []SideEffect{{Kind: EffectNotify, Template: "order_paid_seller", Recipient: order.SellerID}}
	case StatusAccepted:
		return []SideEffect{{Kind: EffectNotify, Template: "order_accepted_buyer", Recipient: order.BuyerID}}
	case StatusShipped:
		return []SideEffect{{Kind: EffectNotify, Template: "order_shipped_buyer", Recipient: order.BuyerID}}
	case StatusDelivered:
		return []SideEffect{{Kind: EffectNotify, Template: "order_delivered_buyer", Recipient: order.BuyerID}}
	case StatusCompleted:
		return []SideEffect{
			{Kind: EffectEnqueueRelease},
			{Kind: EffectNotify, Template: "order_completed_seller", Recipient: order.SellerID},
		}
	case StatusDisputed:
		return []SideEffect{{Kind: EffectNotify, Template: "order_disputed_seller", Recipient: order.SellerID}}
	case StatusCancelled:
		return cancelEffects(order, actorType)
	default:
		return nil
	}
}

// cancelEffects shape a death: stock back, money back, and whoever did not
// cancel it is told. The refund's reason records who caused it (DOMAIN's
// refunds.reason, Phase 17a).
func cancelEffects(order Order, actorType string) []SideEffect {
	effects := []SideEffect{{Kind: EffectRestoreStock}, {Kind: EffectEnqueueRefund, Reason: refundReasonFor(actorType)}}
	switch actorType {
	case ActorSeller:
		return append(effects, SideEffect{Kind: EffectNotify, Template: "order_rejected_buyer", Recipient: order.BuyerID})
	case ActorBuyer:
		return append(effects,
			SideEffect{Kind: EffectNotify, Template: "order_cancelled_seller", Recipient: order.SellerID},
			SideEffect{Kind: EffectNotify, Template: "order_cancelled_buyer", Recipient: order.BuyerID})
	default:
		return append(effects,
			SideEffect{Kind: EffectNotify, Template: "order_cancelled_seller", Recipient: order.SellerID},
			SideEffect{Kind: EffectNotify, Template: "order_cancelled_buyer", Recipient: order.BuyerID})
	}
}

// refundReasonFor maps the actor who cancelled the order to the refund's
// stored reason. A system cancellation here is always the 48h accept timeout
// (DOMAIN §3): the checkout-expiry recovery path records its own reason
// directly, bypassing this table.
func refundReasonFor(actorType string) string {
	switch actorType {
	case ActorSeller:
		return RefundReasonSellerRejected
	case ActorBuyer:
		return RefundReasonBuyerCancelled
	default:
		return RefundReasonSellerTimeout
	}
}

// ExposeTransitionTable returns the transition table for tests to compare
// against a hand-written copy of DOMAIN §4. Production code must call
// Transition, never read this.
func ExposeTransitionTable() map[string]map[string][]string {
	out := make(map[string]map[string][]string, len(transitionTable))
	for from, tos := range transitionTable {
		copied := make(map[string][]string, len(tos))
		for to, actors := range tos {
			copied[to] = append([]string(nil), actors...)
		}
		out[from] = copied
	}
	return out
}

// IsForbidden reports whether err is the wrong-party refusal.
func IsForbidden(err error) bool {
	var forbidden *ForbiddenError
	return errors.As(err, &forbidden)
}

// IsInvalidTransition reports whether err is the not-in-table refusal.
func IsInvalidTransition(err error) bool {
	var invalid *InvalidTransitionError
	return errors.As(err, &invalid)
}
