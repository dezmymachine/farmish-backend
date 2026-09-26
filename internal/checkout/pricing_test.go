package checkout_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/dezmymachine/farmish-backend/internal/checkout"
	"github.com/dezmymachine/farmish-backend/internal/delivery"
	"github.com/dezmymachine/farmish-backend/internal/money"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

func mustUUID(value string) uuid.UUID {
	return uuid.MustParse(value)
}

var (
	sellerA = mustUUID("11111111-1111-1111-1111-111111111111")
	sellerB = mustUUID("22222222-2222-2222-2222-222222222222")
	buyer   = mustUUID("33333333-3333-3333-3333-333333333333")
)

func activeSnapshot(id, seller uuid.UUID, category uuid.UUID) checkout.ListingSnapshot {
	return checkout.ListingSnapshot{
		ID: id, SellerID: seller, SellerName: "Seller " + seller.String()[:8],
		Title: "Listing " + id.String()[:8], Unit: "kg", UnitPricePesewas: 1000,
		QuantityAvailable: 10, MinOrderQty: 1, CategoryID: category, Active: true,
		OffersPickup: true,
	}
}

func sellerDeliveryChoice() checkout.DeliveryChoice {
	return checkout.DeliveryChoice{
		Method: delivery.MethodSellerDelivery, Address: "12 Market Road",
		RecipientName: "Ama Serwaa", RecipientPhone: "+233241234567",
	}
}

func fieldNames(t *testing.T, err error) []string {
	t.Helper()
	var invalid *validation.Error
	if !errors.As(err, &invalid) {
		t.Fatalf("err = %v (%T), want *validation.Error", err, err)
	}
	names := make([]string, 0, len(invalid.Fields))
	for _, field := range invalid.Fields {
		names = append(names, field.Name)
	}
	return names
}

func hasField(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

func TestPriceCart_TwoSellersTwoOrders(t *testing.T) {
	listingA1 := mustUUID("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaa1")
	listingA2 := mustUUID("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaa2")
	listingB := mustUUID("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbb1")
	category := mustUUID("cccccccc-cccc-cccc-cccc-ccccccccccc1")
	feeA1 := int64(500)
	feeA2 := int64(800)
	snapshots := map[uuid.UUID]checkout.ListingSnapshot{
		listingA1: {
			ID: listingA1, SellerID: sellerA, SellerName: "Seller A", Title: "First A",
			Unit: "kg", UnitPricePesewas: 1000, QuantityAvailable: 10, MinOrderQty: 1,
			CategoryID: category, Active: true, OffersPickup: true,
			OffersSellerDelivery: true, SellerDeliveryFee: &feeA1,
		},
		listingA2: {
			ID: listingA2, SellerID: sellerA, SellerName: "Seller A", Title: "Second A",
			Unit: "kg", UnitPricePesewas: 2000, QuantityAvailable: 10, MinOrderQty: 2,
			CategoryID: category, Active: true, OffersPickup: true,
			OffersSellerDelivery: true, SellerDeliveryFee: &feeA2,
		},
		listingB: {
			ID: listingB, SellerID: sellerB, SellerName: "Seller B", Title: "Only B",
			Unit: "kg", UnitPricePesewas: 5000, QuantityAvailable: 2, MinOrderQty: 1,
			CategoryID: category, Active: true, OffersPickup: true,
		},
	}
	cart := checkout.Cart{
		BuyerID: buyer,
		Lines: []checkout.CartLine{
			{ListingID: listingA2, Quantity: 2},
			{ListingID: listingB, Quantity: 1},
			{ListingID: listingA1, Quantity: 2},
		},
		Delivery: map[uuid.UUID]checkout.DeliveryChoice{
			sellerA: sellerDeliveryChoice(),
			sellerB: {Method: delivery.MethodPickup},
		},
	}
	got, err := checkout.PriceCart(context.Background(), snapshots, cart,
		checkout.CommissionRates{Default: 500}, 195, delivery.Manual{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Orders) != 2 || got.Orders[0].SellerID != sellerA || got.Orders[1].SellerID != sellerB {
		t.Fatalf("orders = %+v, want seller A then seller B", got.Orders)
	}
	first, second := got.Orders[0], got.Orders[1]
	if len(first.Items) != 2 || first.Items[0].ListingID != listingA1 || first.Items[1].ListingID != listingA2 {
		t.Errorf("first order items = %+v, want listing order", first.Items)
	}
	if first.SubtotalPesewas != 6000 || first.DeliveryFeePesewas != 800 || first.BasePesewas != 6800 ||
		first.CommissionRateBps != 500 || first.CommissionPesewas != 300 {
		t.Errorf("first order = %+v, want 6000/800/6800 and 500bps/300", first)
	}
	if second.SubtotalPesewas != 5000 || second.DeliveryFeePesewas != 0 || second.BasePesewas != 5000 ||
		second.CommissionRateBps != 500 || second.CommissionPesewas != 250 {
		t.Errorf("second order = %+v, want 5000/0/5000 and 500bps/250", second)
	}
	if got.BasePesewas != 11800 || got.ProcessingFeePesewas != 235 || got.ChargePesewas != 12035 {
		t.Errorf("checkout = base %d fee %d charge %d, want 11800/235/12035",
			got.BasePesewas, got.ProcessingFeePesewas, got.ChargePesewas)
	}
}

func TestPriceCart_Validation(t *testing.T) {
	other := mustUUID("44444444-4444-4444-4444-444444444444")
	category := mustUUID("cccccccc-cccc-cccc-cccc-ccccccccccc2")
	fee := int64(500)
	newListings := func() (uuid.UUID, map[uuid.UUID]checkout.ListingSnapshot) {
		id := uuid.New()
		snapshot := activeSnapshot(id, sellerA, category)
		snapshot.UnitPricePesewas = 1000
		snapshot.QuantityAvailable = 5
		snapshot.MinOrderQty = 2
		snapshot.OffersSellerDelivery = true
		snapshot.SellerDeliveryFee = &fee
		return id, map[uuid.UUID]checkout.ListingSnapshot{id: snapshot}
	}
	newCart := func(id uuid.UUID) checkout.Cart {
		return checkout.Cart{
			BuyerID: buyer,
			Lines:   []checkout.CartLine{{ListingID: id, Quantity: 2}},
			Delivery: map[uuid.UUID]checkout.DeliveryChoice{
				sellerA: sellerDeliveryChoice(),
			},
		}
	}

	t.Run("duplicate line", func(t *testing.T) {
		id, listings := newListings()
		cart := newCart(id)
		cart.Lines = append(cart.Lines, checkout.CartLine{ListingID: id, Quantity: 2})
		_, err := checkout.PriceCart(context.Background(), listings, cart,
			checkout.CommissionRates{Default: 500}, 195, delivery.Manual{})
		if names := fieldNames(t, err); !hasField(names, "lines[1].listingId") {
			t.Errorf("fields = %v, want the duplicate line", names)
		}
	})

	t.Run("unknown listing", func(t *testing.T) {
		_, listings := newListings()
		cart := checkout.Cart{
			BuyerID: buyer,
			Lines:   []checkout.CartLine{{ListingID: other, Quantity: 2}},
			Delivery: map[uuid.UUID]checkout.DeliveryChoice{
				sellerA: sellerDeliveryChoice(),
			},
		}
		_, err := checkout.PriceCart(context.Background(), listings, cart,
			checkout.CommissionRates{Default: 500}, 195, delivery.Manual{})
		if names := fieldNames(t, err); !hasField(names, "lines[0].listingId") {
			t.Errorf("fields = %v, want the unknown listing", names)
		}
	})

	t.Run("inactive listing", func(t *testing.T) {
		id, listings := newListings()
		snapshot := listings[id]
		snapshot.Active = false
		listings[id] = snapshot
		_, err := checkout.PriceCart(context.Background(), listings, newCart(id),
			checkout.CommissionRates{Default: 500}, 195, delivery.Manual{})
		if names := fieldNames(t, err); !hasField(names, "lines[0].listingId") {
			t.Errorf("fields = %v, want the inactive listing", names)
		}
	})

	t.Run("own listing", func(t *testing.T) {
		id, listings := newListings()
		cart := newCart(id)
		cart.BuyerID = sellerA
		_, err := checkout.PriceCart(context.Background(), listings, cart,
			checkout.CommissionRates{Default: 500}, 195, delivery.Manual{})
		if names := fieldNames(t, err); !hasField(names, "lines[0].listingId") {
			t.Errorf("fields = %v, want the own listing", names)
		}
	})

	t.Run("quantity below minimum", func(t *testing.T) {
		id, listings := newListings()
		cart := newCart(id)
		cart.Lines[0].Quantity = 1
		_, err := checkout.PriceCart(context.Background(), listings, cart,
			checkout.CommissionRates{Default: 500}, 195, delivery.Manual{})
		if names := fieldNames(t, err); !hasField(names, "lines[0].quantity") {
			t.Errorf("fields = %v, want the small quantity", names)
		}
	})

	t.Run("quantity above available", func(t *testing.T) {
		id, listings := newListings()
		cart := newCart(id)
		cart.Lines[0].Quantity = 6
		_, err := checkout.PriceCart(context.Background(), listings, cart,
			checkout.CommissionRates{Default: 500}, 195, delivery.Manual{})
		if names := fieldNames(t, err); !hasField(names, "lines[0].quantity") {
			t.Errorf("fields = %v, want the large quantity", names)
		}
	})

	t.Run("delivery not offered", func(t *testing.T) {
		id, listings := newListings()
		snapshot := listings[id]
		snapshot.OffersSellerDelivery = false
		snapshot.SellerDeliveryFee = nil
		listings[id] = snapshot
		_, err := checkout.PriceCart(context.Background(), listings, newCart(id),
			checkout.CommissionRates{Default: 500}, 195, delivery.Manual{})
		if names := fieldNames(t, err); !hasField(names, "delivery."+sellerA.String()+".method") {
			t.Errorf("fields = %v, want the delivery method", names)
		}
	})

	t.Run("missing address", func(t *testing.T) {
		id, listings := newListings()
		cart := newCart(id)
		choice := sellerDeliveryChoice()
		choice.Address = ""
		cart.Delivery[sellerA] = choice
		_, err := checkout.PriceCart(context.Background(), listings, cart,
			checkout.CommissionRates{Default: 500}, 195, delivery.Manual{})
		if names := fieldNames(t, err); !hasField(names, "delivery."+sellerA.String()+".address") {
			t.Errorf("fields = %v, want the address", names)
		}
	})

	t.Run("too many lines", func(t *testing.T) {
		listings := make(map[uuid.UUID]checkout.ListingSnapshot, 51)
		cart := checkout.Cart{BuyerID: buyer, Delivery: map[uuid.UUID]checkout.DeliveryChoice{}}
		for range 51 {
			id := uuid.New()
			snapshot := activeSnapshot(id, sellerA, category)
			listings[id] = snapshot
			cart.Lines = append(cart.Lines, checkout.CartLine{ListingID: id, Quantity: 1})
		}
		_, err := checkout.PriceCart(context.Background(), listings, cart,
			checkout.CommissionRates{Default: 500}, 195, delivery.Manual{})
		if names := fieldNames(t, err); !hasField(names, "lines") {
			t.Errorf("fields = %v, want the line limit", names)
		}
	})
}

func TestPriceCart_DeliveryFeeMax(t *testing.T) {
	low, high := int64(500), int64(800)
	listingLow := mustUUID("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaab1")
	listingHigh := mustUUID("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaab2")
	category := mustUUID("cccccccc-cccc-cccc-cccc-ccccccccccc3")
	lowSnapshot := activeSnapshot(listingLow, sellerA, category)
	lowSnapshot.SellerDeliveryFee = &low
	lowSnapshot.OffersSellerDelivery = true
	highSnapshot := activeSnapshot(listingHigh, sellerA, category)
	highSnapshot.SellerDeliveryFee = &high
	highSnapshot.OffersSellerDelivery = true
	got, err := checkout.PriceCart(context.Background(),
		map[uuid.UUID]checkout.ListingSnapshot{listingLow: lowSnapshot, listingHigh: highSnapshot},
		checkout.Cart{
			BuyerID: buyer,
			Lines: []checkout.CartLine{
				{ListingID: listingLow, Quantity: 1},
				{ListingID: listingHigh, Quantity: 1},
			},
			Delivery: map[uuid.UUID]checkout.DeliveryChoice{sellerA: sellerDeliveryChoice()},
		}, checkout.CommissionRates{Default: 500}, 195, delivery.Manual{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Orders) != 1 || got.Orders[0].DeliveryFeePesewas != high {
		t.Errorf("delivery fee = %+v, want one 800 charge for the order", got.Orders)
	}
}

func TestPriceCart_CommissionResolution(t *testing.T) {
	listing := mustUUID("dddddddd-dddd-dddd-dddd-dddddddddddd")
	parent := mustUUID("eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee")
	child := mustUUID("ffffffff-ffff-ffff-ffff-ffffffffffff")
	other := mustUUID("00000000-0000-0000-0000-000000000001")
	base := activeSnapshot(listing, sellerA, child)
	base.ParentCategoryID = &parent
	base.UnitPricePesewas = 1000
	cart := checkout.Cart{
		BuyerID:  buyer,
		Lines:    []checkout.CartLine{{ListingID: listing, Quantity: 1}},
		Delivery: map[uuid.UUID]checkout.DeliveryChoice{sellerA: {Method: delivery.MethodPickup}},
	}
	listings := map[uuid.UUID]checkout.ListingSnapshot{listing: base}
	for _, tt := range []struct {
		name  string
		rates checkout.CommissionRates
		want  int
	}{
		{"default", checkout.CommissionRates{Default: 500}, 500},
		{"parent override", checkout.CommissionRates{Default: 500, Overrides: map[uuid.UUID]int{parent: 400}}, 400},
		{"child override wins", checkout.CommissionRates{Default: 500, Overrides: map[uuid.UUID]int{parent: 400, child: 300}}, 300},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := checkout.PriceCart(context.Background(), listings, cart, tt.rates, 195, delivery.Manual{})
			if err != nil {
				t.Fatal(err)
			}
			if got.Orders[0].CommissionRateBps != tt.want {
				t.Errorf("rate = %d, want %d", got.Orders[0].CommissionRateBps, tt.want)
			}
		})
	}

	mixedChild := mustUUID("00000000-0000-0000-0000-000000000002")
	mixed := base
	mixed.CategoryID = mixedChild
	second := mustUUID("00000000-0000-0000-0000-000000000003")
	secondSnapshot := activeSnapshot(second, sellerA, other)
	mixedCart := checkout.Cart{
		BuyerID: buyer,
		Lines: []checkout.CartLine{
			{ListingID: listing, Quantity: 1},
			{ListingID: second, Quantity: 1},
		},
		Delivery: map[uuid.UUID]checkout.DeliveryChoice{sellerA: {Method: delivery.MethodPickup}},
	}
	got, err := checkout.PriceCart(context.Background(),
		map[uuid.UUID]checkout.ListingSnapshot{listing: mixed, second: secondSnapshot},
		mixedCart,
		checkout.CommissionRates{Default: 100, Overrides: map[uuid.UUID]int{child: 300, other: 700}},
		195, delivery.Manual{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Orders[0].CommissionRateBps != 700 {
		t.Errorf("mixed-category rate = %d, want the highest applicable rate", got.Orders[0].CommissionRateBps)
	}
}

func TestMoney_ExamplesFromDomain(t *testing.T) {
	listing := mustUUID("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaac1")
	snapshot := activeSnapshot(listing, sellerA, mustUUID("cccccccc-cccc-cccc-cccc-ccccccccccc4"))
	snapshot.UnitPricePesewas = 10000
	cart := checkout.Cart{
		BuyerID:  buyer,
		Lines:    []checkout.CartLine{{ListingID: listing, Quantity: 1}},
		Delivery: map[uuid.UUID]checkout.DeliveryChoice{sellerA: {Method: delivery.MethodPickup}},
	}
	got, err := checkout.PriceCart(context.Background(),
		map[uuid.UUID]checkout.ListingSnapshot{listing: snapshot}, cart,
		checkout.CommissionRates{Default: 500}, 195, delivery.Manual{})
	if err != nil {
		t.Fatal(err)
	}
	if got.BasePesewas != 10000 || got.ChargePesewas != 10199 || got.ProcessingFeePesewas != 199 {
		t.Errorf("checkout = base %d fee %d charge %d, want 10000/199/10199",
			got.BasePesewas, got.ProcessingFeePesewas, got.ChargePesewas)
	}

	snapshot.UnitPricePesewas = 12345
	got, err = checkout.PriceCart(context.Background(),
		map[uuid.UUID]checkout.ListingSnapshot{listing: snapshot}, cart,
		checkout.CommissionRates{Default: 500}, 195, delivery.Manual{})
	if err != nil {
		t.Fatal(err)
	}
	charge, _, err := money.GrossUp(12345, 195)
	if err != nil {
		t.Fatal(err)
	}
	if got.Orders[0].CommissionPesewas != 617 || got.ChargePesewas != charge {
		t.Errorf("commission = %d charge = %d, want 617 and %d",
			got.Orders[0].CommissionPesewas, got.ChargePesewas, charge)
	}
}
