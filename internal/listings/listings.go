// Package listings owns seller listings: creation, edits, the status machine
// and the hourly expiry sweep (DOMAIN §7). Prices are integer pesewas and
// every rule (unit, item state, attributes) is validated server-side against
// the category's listing group.
package listings

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Statuses stored in listings.status.
const (
	StatusDraft     = "draft"
	StatusActive    = "active"
	StatusSold      = "sold"
	StatusExpired   = "expired"
	StatusArchived  = "archived"
	StatusSuspended = "suspended"
)

// DOMAIN §7 limits.
const (
	// ExpiryWindow is how long a published listing stays active.
	ExpiryWindow = 30 * 24 * time.Hour
	// MaxImagesPerListing is the DOMAIN §7 cap.
	MaxImagesPerListing = 10
	// MaxPricePesewas is the per-unit ceiling (GHS 10,000,000).
	MaxPricePesewas = 1_000_000_000
	// SlugCollisionTries is how many -2, -3 … suffixes to try before falling
	// back to random characters.
	SlugCollisionTries = 50
	// MaxAttributeValueLen bounds a stored attribute value.
	MaxAttributeValueLen = 200
)

var (
	// ErrNotFound means no listing matches.
	ErrNotFound = errors.New("listing not found")
	// ErrForbidden means the listing belongs to another seller.
	ErrForbidden = errors.New("not the owner of this listing")
	// ErrInvalidTransition means the requested status change is not allowed
	// from the current status (DOMAIN §7).
	ErrInvalidTransition = errors.New("invalid listing status transition")
	// ErrSellerProfileRequired means the caller has no seller profile.
	ErrSellerProfileRequired = errors.New("seller profile required")
	// ErrSuspended means an admin suspended the listing; the owner is frozen
	// out of edits and transitions.
	ErrSuspended = errors.New("listing is suspended")
	// ErrImageInUse means the media object already belongs to another
	// listing.
	ErrImageInUse = errors.New("media object is already attached to a listing")
	// ErrIncomplete means publishing needs images and required attributes.
	ErrIncomplete = errors.New("listing is not complete enough to publish")
)

// allowedTransitions is DOMAIN §7, owner-driven only: admin transitions
// (any → suspended, suspended → active/archived) are Phase 20b.
var allowedTransitions = map[string]map[string]bool{
	"publish": {StatusDraft: true, StatusArchived: true, StatusExpired: true},
	"renew":   {StatusActive: true, StatusExpired: true},
	"sold":    {StatusActive: true},
	"archive": {StatusActive: true, StatusExpired: true},
}

// Listing is a row of the listings table.
type Listing struct {
	ID                       uuid.UUID
	SellerID                 uuid.UUID
	CategoryID               uuid.UUID
	Title                    string
	Slug                     string
	Description              string
	PricePesewas             int64
	Unit                     string
	QuantityAvailable        int32
	MinOrderQty              int32
	IsNegotiable             bool
	ItemState                string
	Status                   string
	Region                   string
	District                 string
	Area                     *string
	OffersPickup             bool
	OffersSellerDelivery     bool
	SellerDeliveryFeePesewas *int64
	PublishedAt              *time.Time
	ExpiresAt                *time.Time
	ViewCount                int32
	FavoriteCount            int32
	ContactCount             int32
	CreatedAt                time.Time
	UpdatedAt                time.Time
}

// Image is one attached image with its public URL.
type Image struct {
	MediaID   uuid.UUID
	URL       string
	SortOrder int32
}

// AttributeValue is a stored attribute value with its definition.
type AttributeValue struct {
	Key      string
	Value    string
	Type     string
	Required bool
}

// View is the seller's own listing: the row plus images, attributes and the
// category slug.
type View struct {
	Listing
	CategorySlug string
	Images       []Image
	Attributes   []AttributeValue
}

// Summary is one row in the seller's listing list.
type Summary struct {
	ID                uuid.UUID
	Title             string
	Slug              string
	CategorySlug      string
	PricePesewas      int64
	QuantityAvailable int32
	Status            string
	ImageCount        int32
	ViewCount         int32
	FavoriteCount     int32
	ExpiresAt         *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// Delivery describes the delivery options a listing offers.
type Delivery struct {
	Pickup         bool
	SellerDelivery bool
	// FeePesewas is required exactly when SellerDelivery is true.
	FeePesewas *int64
}

// Merge applies a delivery patch over the stored options. A nil fee in the
// patch keeps the stored one; ClearFee drops it (used when switching
// seller delivery off).
func (p DeliveryPatch) Merge(cur Delivery) Delivery {
	out := cur
	if p.Pickup != nil {
		out.Pickup = *p.Pickup
	}
	if p.SellerDelivery != nil {
		out.SellerDelivery = *p.SellerDelivery
	}
	switch {
	case p.FeePesewas != nil:
		out.FeePesewas = p.FeePesewas
	case p.ClearFee:
		out.FeePesewas = nil
	}
	return out
}

// Input is the create write model: every field is set.
type Input struct {
	CategorySlug      string
	Title             string
	Description       string
	PricePesewas      int64
	Unit              string
	QuantityAvailable int32
	MinOrderQty       int32
	IsNegotiable      bool
	ItemState         string
	Region            string
	District          string
	Area              *string
	Delivery          Delivery
	Attributes        map[string]string
	ImageMediaIDs     []uuid.UUID
}

// Patch is the update write model: a nil field keeps the stored value.
// Attributes and images are replaced only when supplied (a non-nil map or
// slice), except that a category change always re-validates attributes.
type Patch struct {
	CategorySlug      *string
	Title             *string
	Description       *string
	PricePesewas      *int64
	Unit              *string
	QuantityAvailable *int32
	MinOrderQty       *int32
	IsNegotiable      *bool
	ItemState         *string
	Region            *string
	District          *string
	Area              *string
	Delivery          *DeliveryPatch
	Attributes        map[string]string
	ImageMediaIDs     []uuid.UUID
}

// DeliveryPatch is the partial delivery model for updates.
type DeliveryPatch struct {
	Pickup         *bool
	SellerDelivery *bool
	FeePesewas     *int64
	// ClearFee drops a stored fee (switching seller delivery off).
	ClearFee bool
}
