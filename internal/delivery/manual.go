package delivery

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

var (
	// ErrDeliveryNotOffered means at least one item in the seller-order does
	// not offer the requested method, or its stored fee is missing.
	ErrDeliveryNotOffered = errors.New("delivery method not offered")
	// ErrNotSupported means the method needs a provider that does not exist
	// yet. Courier integration is explicitly backlog, not a buyer error.
	ErrNotSupported = errors.New("delivery method not supported")
)

// Manual implements the delivery sellers already offer themselves.
type Manual struct{}

// Quote prices one seller-order. Pickup is free. Seller delivery charges once
// per order, using the highest item fee: the seller makes one trip, and the
// buyer must not pay a per-item fee on top of it.
func (Manual) Quote(_ context.Context, in QuoteInput) (Quote, error) {
	switch in.Method {
	case MethodPickup:
		return Quote{FeePesewas: 0, Method: MethodPickup}, nil
	case MethodSellerDelivery:
		var fee int64
		for _, item := range in.Items {
			if !item.OffersSellerDelivery || item.SellerDeliveryFee == nil {
				return Quote{}, ErrDeliveryNotOffered
			}
			if *item.SellerDeliveryFee > fee {
				fee = *item.SellerDeliveryFee
			}
		}
		return Quote{FeePesewas: fee, Method: MethodSellerDelivery}, nil
	default:
		return Quote{}, ErrNotSupported
	}
}

// CreateShipment is unsupported: manual deliveries have no carrier to create.
func (Manual) CreateShipment(_ context.Context, _ uuid.UUID) (string, error) {
	return "", ErrNotSupported
}

// Track is unsupported: the seller records tracking manually in Phase 16.
func (Manual) Track(_ context.Context, _ string) (Status, error) {
	return "", ErrNotSupported
}
