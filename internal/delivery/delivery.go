// Package delivery quotes and tracks order delivery. Phase 15a only needs the
// manual options sellers already offer: buyer pickup and seller delivery. A
// courier provider can implement Provider later without changing checkout.
package delivery

import (
	"context"

	"github.com/google/uuid"
)

// Delivery methods stored in orders.delivery_method.
const (
	MethodPickup         = "pickup"
	MethodSellerDelivery = "seller_delivery"
	MethodCourier        = "courier"
)

// QuoteInput is one seller-order's delivery request.
type QuoteInput struct {
	Method string
	Items  []QuoteItem
}

// QuoteItem is the delivery-relevant part of a listing snapshot.
type QuoteItem struct {
	ListingID            uuid.UUID
	OffersSellerDelivery bool
	SellerDeliveryFee    *int64
}

// Quote is the provider's answer for one seller-order.
type Quote struct {
	FeePesewas int64
	Method     string
}

// Status is a tracking answer. Only manual delivery exists in Phase 15a, so
// its provider always reports unsupported; the shape is here for Phase 16.
type Status string

// Provider quotes, creates and tracks shipments.
type Provider interface {
	// Quote prices one seller-order's delivery.
	Quote(ctx context.Context, in QuoteInput) (Quote, error)
	// CreateShipment creates a carrier shipment for an order.
	CreateShipment(ctx context.Context, orderID uuid.UUID) (ref string, err error)
	// Track reports a shipment's status.
	Track(ctx context.Context, ref string) (Status, error)
}
