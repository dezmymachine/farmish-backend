package checkout

import (
	"github.com/google/uuid"
)

// CartLine is one requested quantity. It deliberately carries no price: all
// prices come from the server-side listing snapshot, making client tampering
// impossible by construction.
type CartLine struct {
	ListingID uuid.UUID
	Quantity  int
}

// DeliveryChoice is one seller-order's delivery request.
type DeliveryChoice struct {
	Method         string
	Address        string
	Region         string
	District       string
	RecipientName  string
	RecipientPhone string
}

// Cart is a checkout request in domain terms.
type Cart struct {
	BuyerID  uuid.UUID
	Lines    []CartLine
	Delivery map[uuid.UUID]DeliveryChoice
}

// ListingSnapshot is the server-side data PriceCart may use. Active is
// calculated by the loader from status and expiry, keeping the pricing core
// pure and deterministic.
type ListingSnapshot struct {
	ID                   uuid.UUID
	SellerID             uuid.UUID
	SellerName           string
	Title                string
	Unit                 string
	UnitPricePesewas     int64
	QuantityAvailable    int32
	MinOrderQty          int32
	CategoryID           uuid.UUID
	ParentCategoryID     *uuid.UUID
	Active               bool
	OffersPickup         bool
	OffersSellerDelivery bool
	SellerDeliveryFee    *int64
}

// ItemQuote is one priced line.
type ItemQuote struct {
	ListingID        uuid.UUID
	Title            string
	Unit             string
	UnitPricePesewas int64
	Quantity         int
	LineTotalPesewas int64
}

// OrderQuote is one seller's order. CommissionRateBps and CommissionPesewas
// are internal: the buyer sees amounts, not the platform's cut.
type OrderQuote struct {
	SellerID           uuid.UUID
	SellerName         string
	Items              []ItemQuote
	SubtotalPesewas    int64
	DeliveryFeePesewas int64
	BasePesewas        int64
	CommissionRateBps  int
	CommissionPesewas  int64
}

// CheckoutQuote is the priced cart.
type CheckoutQuote struct {
	Orders               []OrderQuote
	BasePesewas          int64
	ProcessingFeePesewas int64
	ChargePesewas        int64
}

// CommissionRates holds DOMAIN §2.1's configured rates.
type CommissionRates struct {
	// Default is used when neither a listing's category nor its parent has an
	// override.
	Default int
	// Overrides maps category IDs to basis-point rates.
	Overrides map[uuid.UUID]int
}
