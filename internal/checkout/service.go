package checkout

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/dezmymachine/farmish-backend/internal/db"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

// QuoteInput is a quote request in domain terms.
type QuoteInput struct {
	Lines    []CartLine
	Delivery map[uuid.UUID]DeliveryChoice
}

// Rates loads DOMAIN §2.1's commission configuration.
func (s *Service) Rates(ctx context.Context) (CommissionRates, error) {
	return s.ratesFrom(ctx, s.pool)
}

// ratesFrom reads the commission configuration through the caller's handle, so
// a checkout prices and snapshots inside one transaction.
func (s *Service) ratesFrom(ctx context.Context, q db.DBTX) (CommissionRates, error) {
	rows, err := db.New(q).ListCommissionConfigs(ctx)
	if err != nil {
		return CommissionRates{}, fmt.Errorf("list commission configs: %w", err)
	}
	return RatesFromRows(rows)
}

// Quote prices a buyer's cart. Prices, availability, activity and category
// relationships all come from the database; the request carries only listing
// IDs, quantities and delivery choices.
func (s *Service) Quote(ctx context.Context, buyerID uuid.UUID, in QuoteInput) (CheckoutQuote, error) {
	if buyerID == uuid.Nil {
		var invalid validation.Error
		invalid.Add("buyerId", "is required")
		return CheckoutQuote{}, invalid.OrNil()
	}
	ids := make([]uuid.UUID, 0, len(in.Lines))
	seen := make(map[uuid.UUID]bool, len(in.Lines))
	for _, line := range in.Lines {
		if line.ListingID == uuid.Nil || seen[line.ListingID] {
			continue
		}
		seen[line.ListingID] = true
		ids = append(ids, line.ListingID)
	}
	rows, err := db.New(s.pool).ListQuoteListings(ctx, db.ListQuoteListingsParams{
		Now: s.Now(), ListingIds: ids,
	})
	if err != nil {
		return CheckoutQuote{}, fmt.Errorf("list quote listings: %w", err)
	}
	listings := make(map[uuid.UUID]ListingSnapshot, len(rows))
	for _, row := range rows {
		listings[row.ID] = snapshotFromRow(row)
	}
	rates, err := s.Rates(ctx)
	if err != nil {
		return CheckoutQuote{}, err
	}
	return PriceCart(ctx, listings, Cart{BuyerID: buyerID, Lines: in.Lines, Delivery: in.Delivery}, rates, s.feeBps, s.delivery)
}

// RatesFromRows converts commission rows into resolution order. Exactly one
// default row is required: DOMAIN §2.1's fallback must always exist.
func RatesFromRows(rows []db.ListCommissionConfigsRow) (CommissionRates, error) {
	rates := CommissionRates{Overrides: make(map[uuid.UUID]int, len(rows))}
	defaults := 0
	for _, row := range rows {
		if !row.CategoryID.Valid {
			defaults++
			rates.Default = int(row.RateBps)
			continue
		}
		rates.Overrides[uuid.UUID(row.CategoryID.Bytes)] = int(row.RateBps)
	}
	if defaults != 1 {
		return CommissionRates{}, fmt.Errorf("commission configs contain %d default rows, want exactly one", defaults)
	}
	return rates, nil
}

func snapshotFromRow(row db.ListQuoteListingsRow) ListingSnapshot {
	var parent *uuid.UUID
	if row.CategoryParentID.Valid {
		id := uuid.UUID(row.CategoryParentID.Bytes)
		parent = &id
	}
	return ListingSnapshot{
		ID: row.ID, SellerID: row.SellerID, SellerName: row.SellerName, Title: row.Title,
		Unit: row.Unit, UnitPricePesewas: row.UnitPrice, QuantityAvailable: row.QuantityAvailable,
		MinOrderQty: row.MinOrderQty, CategoryID: row.CategoryID, ParentCategoryID: parent,
		Active: row.Active, OffersPickup: row.OffersPickup, OffersSellerDelivery: row.OffersSellerDelivery,
		SellerDeliveryFee: row.SellerDeliveryFeePesewas,
	}
}
