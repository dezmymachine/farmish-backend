// Package promotions sells promotion packages, grants CRD credits when their
// payments succeed, and spends those credits to promote sellers' listings.
// Credits are the CRD unit from DOMAIN §5.1: they have no cash value and are
// never mixed with GHS in an entry.
package promotions

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/ledger"
	"github.com/dezmymachine/farmish-backend/internal/payments"
)

// MetadataCreditsKey snapshots a purchased tier's credit count on the payment
// row. Later edits to promotion_configs cannot change what a paid buyer
// receives.
const MetadataCreditsKey = "promotion_credits"

// Tiers sold through the API.
const (
	TierTop        = "top"
	TierVIP        = "vip"
	TierDiamond    = "diamond"
	TierEnterprise = "enterprise"
)

var (
	// ErrTierNotFound means the requested tier does not exist or is not for
	// sale. The response is 404 either way, so no unavailable tier is leaked.
	ErrTierNotFound = errors.New("promotion tier not found")
	// ErrInsufficientCredits means the buyer's CRD balance is below the tier
	// cost at the moment the application is applied.
	ErrInsufficientCredits = errors.New("insufficient promotion credits")
	// ErrPromotionDowngrade means a lower tier was requested while a higher
	// tier is still active. DOMAIN §6 forfeits remaining time on replacement,
	// but never downgrades an active promotion.
	ErrPromotionDowngrade = errors.New("promotion downgrade is not allowed")
)

// Config is one promotion package as buyers see it.
type Config struct {
	Tier         string
	Name         string
	PricePesewas int64
	Credits      int32
	DurationDays int32
	TierRank     int32
	Featured     bool
	Description  string
	Features     []string
	SortOrder    int32
	Active       bool
}

// Purchase is a newly created promotion charge.
type Purchase struct {
	Payment payments.Payment
	Tier    Config
}

// Application is one row in listing_promotions.
type Application struct {
	ID                uuid.UUID
	ListingID         uuid.UUID
	Tier              string
	StartsAt          time.Time
	EndsAt            time.Time
	CreditsSpent      int32
	ReplacedRemaining bool
}

// PaymentInitializer creates a provider-backed payment. The production
// payments.Service implements it; tests use payments/fake.
type PaymentInitializer interface {
	Initialize(ctx context.Context, in payments.CreateInput) (payments.Payment, error)
}

// ListingChecker locks a listing inside the caller's transaction and checks
// what the caller may do with it. The production listings.Service implements
// it; tests use a fake.
type ListingChecker interface {
	RequireOwner(ctx context.Context, tx pgx.Tx, sellerID, id uuid.UUID) error
	RequireAdvertisable(ctx context.Context, tx pgx.Tx, sellerID, id uuid.UUID, now time.Time) error
}

// Service owns promotion packages, purchases, credit grants and applications.
type Service struct {
	pool     *pgxpool.Pool
	payments PaymentInitializer
	listings ListingChecker
	ledger   *ledger.Ledger
	// Now is the clock, injectable so tests can control promotion windows.
	Now func() time.Time
}

// New returns a Service. The same instance registers its promotion purpose
// handler with payments, so the grant path and the HTTP paths cannot drift.
func New(pool *pgxpool.Pool, initializer PaymentInitializer, listings ListingChecker, books *ledger.Ledger) *Service {
	return &Service{pool: pool, payments: initializer, listings: listings, ledger: books, Now: time.Now}
}
