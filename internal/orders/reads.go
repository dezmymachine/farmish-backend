package orders

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/dezmymachine/farmish-backend/internal/db"
)

// DefaultPageSize bounds order lists.
const DefaultPageSize = 20

// Summary is one row of an order list or a checkout poll.
type Summary struct {
	Order
	SellerName string
}

// Detail is one order with its items, events and the seller's public
// projection. Which of these the caller may see is the handler's call.
type Detail struct {
	Summary
	SellerBio          *string
	SellerVerification string
	SellerMemberSince  time.Time
	Items              []Item
	Events             []Event
}

// ListForBuyer returns the caller's orders, newest first.
func (s *Service) ListForBuyer(ctx context.Context, buyerID uuid.UUID, status string, limit, offset int32) ([]Summary, int64, error) {
	rows, err := db.New(s.pool).ListOrdersByBuyer(ctx, db.ListOrdersByBuyerParams{
		BuyerID: buyerID, Status: nilString(status), Limit: limit, Offset: offset,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("list buyer orders: %w", err)
	}
	total, err := db.New(s.pool).CountOrdersByBuyer(ctx, db.CountOrdersByBuyerParams{
		BuyerID: buyerID, Status: nilString(status),
	})
	if err != nil {
		return nil, 0, fmt.Errorf("count buyer orders: %w", err)
	}
	return listSummaries(rows), total, nil
}

// ListForSeller returns the caller's sale orders, newest first.
func (s *Service) ListForSeller(ctx context.Context, sellerID uuid.UUID, status string, limit, offset int32) ([]Summary, int64, error) {
	rows, err := db.New(s.pool).ListOrdersBySeller(ctx, db.ListOrdersBySellerParams{
		SellerID: sellerID, Status: nilString(status), Limit: limit, Offset: offset,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("list seller orders: %w", err)
	}
	total, err := db.New(s.pool).CountOrdersBySeller(ctx, db.CountOrdersBySellerParams{
		SellerID: sellerID, Status: nilString(status),
	})
	if err != nil {
		return nil, 0, fmt.Errorf("count seller orders: %w", err)
	}
	return listSellerSummaries(rows), total, nil
}

// Get returns one order's detail to its buyer or its seller, and reports which
// role the caller is in. Anyone else gets ErrNotFound, so ids cannot be probed.
func (s *Service) Get(ctx context.Context, callerID, orderID uuid.UUID) (Detail, bool, error) {
	row, err := db.New(s.pool).GetOrderDetail(ctx, orderID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Detail{}, false, fmt.Errorf("%w: %s", ErrNotFound, orderID)
	}
	if err != nil {
		return Detail{}, false, fmt.Errorf("get order detail: %w", err)
	}
	if row.BuyerID != callerID && row.SellerID != callerID {
		return Detail{}, false, fmt.Errorf("%w: %s", ErrNotFound, orderID)
	}
	items, err := db.New(s.pool).ListOrderItemsByOrder(ctx, orderID)
	if err != nil {
		return Detail{}, false, fmt.Errorf("list order items: %w", err)
	}
	events, err := db.New(s.pool).ListOrderEvents(ctx, orderID)
	if err != nil {
		return Detail{}, false, fmt.Errorf("list order events: %w", err)
	}
	detail := Detail{
		Summary: Summary{
			Order: Order{
				ID: row.ID, CheckoutID: row.CheckoutID, BuyerID: row.BuyerID,
				SellerID: row.SellerID, Status: row.Status, EscrowState: row.EscrowState,
				SubtotalPesewas: row.SubtotalPesewas, DeliveryFeePesewas: row.DeliveryFeePesewas,
				BasePesewas: row.BasePesewas, CommissionRateBps: row.CommissionRateBps,
				CommissionPesewas: row.CommissionPesewas, RefundedPesewas: row.RefundedPesewas,
				DeliveryMethod: row.DeliveryMethod, DeliveryAddress: row.DeliveryAddress,
				DeliveryRegion: row.DeliveryRegion, DeliveryDistrict: row.DeliveryDistrict,
				RecipientName: row.RecipientName, RecipientPhone: row.RecipientPhone,
				TrackingRef: row.TrackingRef, PaidAt: row.PaidAt, AcceptedAt: row.AcceptedAt,
				ShippedAt: row.ShippedAt, DeliveredAt: row.DeliveredAt, CompletedAt: row.CompletedAt,
				CancelledAt: row.CancelledAt, AutoCompleteAt: row.AutoCompleteAt,
				CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
			},
			SellerName: row.SellerName,
		},
		SellerBio:          row.SellerBio,
		SellerVerification: row.SellerVerification,
		SellerMemberSince:  row.SellerMemberSince,
		Items:              make([]Item, 0, len(items)),
		Events:             make([]Event, 0, len(events)),
	}
	for _, item := range items {
		detail.Items = append(detail.Items, Item{
			ID: item.ID, OrderID: item.OrderID, ListingID: item.ListingID, Title: item.Title,
			Unit: item.Unit, UnitPricePesewas: item.UnitPricePesewas, Quantity: item.Quantity,
			LineTotalPesewas: item.LineTotalPesewas,
		})
	}
	for _, event := range events {
		detail.Events = append(detail.Events, Event{
			ID: event.ID, OrderID: event.OrderID, FromStatus: event.FromStatus,
			ToStatus: event.ToStatus, ActorType: event.ActorType, ActorID: eventActorID(event.ActorID),
			Note: event.Note, CreatedAt: event.CreatedAt,
		})
	}
	return detail, row.SellerID == callerID, nil
}

// listSummaries maps the buyer-list rows onto the domain summary.
func listSummaries(rows []db.ListOrdersByBuyerRow) []Summary {
	out := make([]Summary, 0, len(rows))
	for _, row := range rows {
		out = append(out, Summary{
			Order: Order{
				ID: row.ID, CheckoutID: row.CheckoutID, BuyerID: row.BuyerID,
				SellerID: row.SellerID, Status: row.Status, EscrowState: row.EscrowState,
				SubtotalPesewas: row.SubtotalPesewas, DeliveryFeePesewas: row.DeliveryFeePesewas,
				BasePesewas: row.BasePesewas, CommissionRateBps: row.CommissionRateBps,
				CommissionPesewas: row.CommissionPesewas, RefundedPesewas: row.RefundedPesewas,
				DeliveryMethod: row.DeliveryMethod, DeliveryAddress: row.DeliveryAddress,
				DeliveryRegion: row.DeliveryRegion, DeliveryDistrict: row.DeliveryDistrict,
				RecipientName: row.RecipientName, RecipientPhone: row.RecipientPhone,
				TrackingRef: row.TrackingRef, PaidAt: row.PaidAt, AcceptedAt: row.AcceptedAt,
				ShippedAt: row.ShippedAt, DeliveredAt: row.DeliveredAt, CompletedAt: row.CompletedAt,
				CancelledAt: row.CancelledAt, AutoCompleteAt: row.AutoCompleteAt,
				CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
			},
			SellerName: row.SellerName,
		})
	}
	return out
}

// listSellerSummaries maps the seller-list rows onto the domain summary.
func listSellerSummaries(rows []db.ListOrdersBySellerRow) []Summary {
	out := make([]Summary, 0, len(rows))
	for _, row := range rows {
		out = append(out, Summary{
			Order: Order{
				ID: row.ID, CheckoutID: row.CheckoutID, BuyerID: row.BuyerID,
				SellerID: row.SellerID, Status: row.Status, EscrowState: row.EscrowState,
				SubtotalPesewas: row.SubtotalPesewas, DeliveryFeePesewas: row.DeliveryFeePesewas,
				BasePesewas: row.BasePesewas, CommissionRateBps: row.CommissionRateBps,
				CommissionPesewas: row.CommissionPesewas, RefundedPesewas: row.RefundedPesewas,
				DeliveryMethod: row.DeliveryMethod, DeliveryAddress: row.DeliveryAddress,
				DeliveryRegion: row.DeliveryRegion, DeliveryDistrict: row.DeliveryDistrict,
				RecipientName: row.RecipientName, RecipientPhone: row.RecipientPhone,
				TrackingRef: row.TrackingRef, PaidAt: row.PaidAt, AcceptedAt: row.AcceptedAt,
				ShippedAt: row.ShippedAt, DeliveredAt: row.DeliveredAt, CompletedAt: row.CompletedAt,
				CancelledAt: row.CancelledAt, AutoCompleteAt: row.AutoCompleteAt,
				CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
			},
			SellerName: row.SellerName,
		})
	}
	return out
}

func nilString(status string) *string {
	if status == "" {
		return nil
	}
	return &status
}

// eventActorID widens the generated nullable actor id for the domain type.
func eventActorID(id pgtype.UUID) *uuid.UUID {
	if !id.Valid {
		return nil
	}
	value := uuid.UUID(id.Bytes)
	return &value
}
