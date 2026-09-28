package handlers

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/dezmymachine/farmish-backend/internal/checkout"
	"github.com/dezmymachine/farmish-backend/internal/db"
	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/internal/orders"
	"github.com/dezmymachine/farmish-backend/internal/payments"
	"github.com/dezmymachine/farmish-backend/internal/users"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

// CheckoutStore is the part of checkout.Service these handlers use.
type CheckoutStore interface {
	Quote(ctx context.Context, buyerID uuid.UUID, in checkout.QuoteInput) (checkout.CheckoutQuote, error)
	Create(ctx context.Context, buyerID uuid.UUID, email string, idempotencyKey uuid.UUID, in checkout.QuoteInput) (checkout.Created, error)
	Get(ctx context.Context, buyerID, checkoutID uuid.UUID) (db.Checkout, []db.ListCheckoutOrdersRow, error)
}

// OrderStore reads orders for buyers and sellers.
type OrderStore interface {
	ListForBuyer(ctx context.Context, buyerID uuid.UUID, status string, limit, offset int32) ([]orders.Summary, int64, error)
	ListForSeller(ctx context.Context, sellerID uuid.UUID, status string, limit, offset int32) ([]orders.Summary, int64, error)
	Get(ctx context.Context, callerID, orderID uuid.UUID) (orders.Detail, bool, error)
}

// QuoteCheckout prices the caller's cart from server-side snapshots.
func (s Server) QuoteCheckout(ctx context.Context, req api.QuoteCheckoutRequestObject) (api.QuoteCheckoutResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	if req.Body == nil {
		return api.QuoteCheckout400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(apierror.New(apierror.CodeBadRequest, "Malformed request")),
		}, nil
	}
	in, requestInvalid := toCheckoutInput(req.Body.Lines, req.Body.Delivery)
	if err := requestInvalid.OrNil(); err != nil {
		return api.QuoteCheckout400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(validationFailed(&requestInvalid)),
		}, nil
	}
	quote, err := s.Checkout.Quote(ctx, u.ID, in)
	var verr *validation.Error
	switch {
	case err == nil:
		return api.QuoteCheckout200JSONResponse(toCheckoutQuote(quote)), nil
	case errors.As(err, &verr):
		return api.QuoteCheckout400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(validationFailed(verr)),
		}, nil
	default:
		return nil, err
	}
}

// CreateCheckout is POST /v1/checkout.
func (s Server) CreateCheckout(ctx context.Context, req api.CreateCheckoutRequestObject) (api.CreateCheckoutResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	if req.Body == nil {
		return api.CreateCheckout400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(apierror.New(apierror.CodeBadRequest, "Malformed request")),
		}, nil
	}
	if req.Params.IdempotencyKey == uuid.Nil {
		return api.CreateCheckout400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(apierror.New(apierror.CodeBadRequest, "A uuid Idempotency-Key header is required")),
		}, nil
	}
	in, requestInvalid := toCheckoutInput(req.Body.Lines, req.Body.Delivery)
	if err := requestInvalid.OrNil(); err != nil {
		return api.CreateCheckout400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(validationFailed(&requestInvalid)),
		}, nil
	}

	email := ""
	if u.Email != nil {
		email = *u.Email
	}
	created, err := s.Checkout.Create(ctx, u.ID, email, req.Params.IdempotencyKey, in)
	var verr *validation.Error
	switch {
	case err == nil && !created.Replayed:
		return api.CreateCheckout201JSONResponse(toCheckoutCreated(created)), nil
	case err == nil:
		return api.CreateCheckout200JSONResponse(toCheckoutCreated(created)), nil
	case errors.As(err, &verr):
		return api.CreateCheckout400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(validationFailed(verr)),
		}, nil
	case errors.Is(err, checkout.ErrIdempotencyKeyReused):
		return api.CreateCheckout409JSONResponse{
			ConflictJSONResponse: api.ConflictJSONResponse(apierror.New("idempotency_key_reused", "This key was used with a different cart")),
		}, nil
	case errors.Is(err, checkout.ErrInsufficientStock):
		return api.CreateCheckout409JSONResponse{
			ConflictJSONResponse: api.ConflictJSONResponse(apierror.New("insufficient_stock", "Another buyer took the remaining stock")),
		}, nil
	case errors.Is(err, payments.ErrProviderUnavailable), errors.Is(err, payments.ErrRejected):
		return api.CreateCheckout502JSONResponse{
			PaymentProviderErrorJSONResponse: api.PaymentProviderErrorJSONResponse(
				apierror.New(apierror.CodePaymentProvider, "The payment provider is unavailable; try again")),
		}, nil
	default:
		return nil, err
	}
}

// GetCheckout is the buyer's status poll after the Paystack redirect.
func (s Server) GetCheckout(ctx context.Context, req api.GetCheckoutRequestObject) (api.GetCheckoutResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	checkoutRow, orderRows, err := s.Checkout.Get(ctx, u.ID, req.Id)
	switch {
	case errors.Is(err, orders.ErrNotFound):
		return api.GetCheckout404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse(apierror.New(apierror.CodeNotFound, "Checkout not found")),
		}, nil
	case err != nil:
		return nil, err
	}
	items := make([]api.OrderSummary, 0, len(orderRows))
	for _, row := range orderRows {
		items = append(items, toOrderSummary(orders.Summary{
			Order: orders.Order{
				ID: row.ID, CheckoutID: row.CheckoutID, SellerID: row.SellerID,
				Status: row.Status, EscrowState: row.EscrowState,
				SubtotalPesewas: row.SubtotalPesewas, DeliveryFeePesewas: row.DeliveryFeePesewas,
				BasePesewas: row.BasePesewas, DeliveryMethod: row.DeliveryMethod,
				CreatedAt: row.CreatedAt, PaidAt: row.PaidAt,
			},
			SellerName: row.SellerName,
		}))
	}
	return api.GetCheckout200JSONResponse(api.CheckoutStatus{
		Id: checkoutRow.ID, Status: api.CheckoutStatusStatus(checkoutRow.Status),
		Charge: ghs(checkoutRow.ChargePesewas), ExpiresAt: checkoutRow.ExpiresAt, Orders: items,
	}), nil
}

// ListMyOrders is the buyer's purchase list.
func (s Server) ListMyOrders(ctx context.Context, req api.ListMyOrdersRequestObject) (api.ListMyOrdersResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	page, limit := pageParams(req.Params.Page, req.Params.Limit)
	status := statusString(req.Params.Status)
	items, total, err := s.Orders.ListForBuyer(ctx, u.ID, status, limit, (page-1)*limit)
	if err != nil {
		return nil, err
	}
	summaries := make([]api.OrderSummary, 0, len(items))
	for _, order := range items {
		summaries = append(summaries, toOrderSummary(order))
	}
	return api.ListMyOrders200JSONResponse(api.OrderSummaryList{
		Items: summaries, Meta: api.PageMeta{Page: page, Limit: limit, Total: total},
	}), nil
}

// ListMySales is the seller's sale list.
func (s Server) ListMySales(ctx context.Context, req api.ListMySalesRequestObject) (api.ListMySalesResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	page, limit := pageParams(req.Params.Page, req.Params.Limit)
	status := statusString(req.Params.Status)
	items, total, err := s.Orders.ListForSeller(ctx, u.ID, status, limit, (page-1)*limit)
	if err != nil {
		return nil, err
	}
	summaries := make([]api.OrderSummary, 0, len(items))
	for _, order := range items {
		summaries = append(summaries, toOrderSummary(order))
	}
	return api.ListMySales200JSONResponse(api.OrderSummaryList{
		Items: summaries, Meta: api.PageMeta{Page: page, Limit: limit, Total: total},
	}), nil
}

// GetOrder returns one order shaped by the caller's role.
func (s Server) GetOrder(ctx context.Context, req api.GetOrderRequestObject) (api.GetOrderResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	detail, isSeller, err := s.Orders.Get(ctx, u.ID, req.Id)
	switch {
	case errors.Is(err, orders.ErrNotFound):
		return api.GetOrder404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse(apierror.New(apierror.CodeNotFound, "Order not found")),
		}, nil
	case err != nil:
		return nil, err
	}
	rating, err := s.ratingOf(ctx, detail.SellerID)
	if err != nil {
		return nil, err
	}
	return api.GetOrder200JSONResponse(toOrderDetail(detail, isSeller, rating)), nil
}

// toCheckoutInput converts the contract body to the domain cart, rejecting
// duplicate seller delivery entries before pricing.
func toCheckoutInput(lines []api.CheckoutCartLine, delivery []api.CheckoutDelivery) (checkout.QuoteInput, validation.Error) {
	var invalid validation.Error
	cartLines := make([]checkout.CartLine, 0, len(lines))
	for _, line := range lines {
		cartLines = append(cartLines, checkout.CartLine{ListingID: line.ListingId, Quantity: int(line.Quantity)})
	}
	choices := make(map[uuid.UUID]checkout.DeliveryChoice, len(delivery))
	for i, choice := range delivery {
		if _, duplicate := choices[choice.SellerId]; duplicate {
			invalid.Add(fmt.Sprintf("delivery[%d].sellerId", i), "each seller may appear only once")
			continue
		}
		choices[choice.SellerId] = checkout.DeliveryChoice{
			Method: string(choice.Method), Address: orEmpty(choice.Address), Region: orEmpty(choice.Region),
			District: orEmpty(choice.District), RecipientName: orEmpty(choice.RecipientName),
			RecipientPhone: orEmpty(choice.RecipientPhone),
		}
	}
	return checkout.QuoteInput{Lines: cartLines, Delivery: choices}, invalid
}

func toCheckoutCreated(created checkout.Created) api.CheckoutCreated {
	return api.CheckoutCreated{
		CheckoutId: created.CheckoutID, Reference: created.Payment.Reference,
		AuthorizationUrl: orEmpty(created.Payment.AuthorizationURL),
		Quote:            toCheckoutQuote(created.Quote), ExpiresAt: created.ExpiresAt,
	}
}

func toOrderSummary(order orders.Summary) api.OrderSummary {
	return api.OrderSummary{
		Id: order.ID, CheckoutId: order.CheckoutID, SellerId: order.SellerID,
		SellerName: order.SellerName, Status: api.OrderStatus(order.Status),
		EscrowState: api.EscrowState(order.EscrowState),
		Subtotal:    ghs(order.SubtotalPesewas), DeliveryFee: ghs(order.DeliveryFeePesewas),
		Base:           ghs(order.BasePesewas),
		DeliveryMethod: api.OrderSummaryDeliveryMethod(order.DeliveryMethod),
		CreatedAt:      order.CreatedAt, PaidAt: order.PaidAt,
	}
}

// toOrderDetail shapes one order for the caller's role: the seller sees the
// recipient and the commission; the buyer sees the seller's public profile.
// Both see the seller's rating.
func toOrderDetail(detail orders.Detail, isSeller bool, rating api.SellerRating) api.OrderDetail {
	out := api.OrderDetail{
		Id: detail.ID, CheckoutId: detail.CheckoutID, BuyerId: detail.BuyerID,
		SellerId: detail.SellerID, SellerName: detail.SellerName,
		Status: api.OrderStatus(detail.Status), EscrowState: api.EscrowState(detail.EscrowState),
		Subtotal: ghs(detail.SubtotalPesewas), DeliveryFee: ghs(detail.DeliveryFeePesewas),
		Base:           ghs(detail.BasePesewas),
		DeliveryMethod: api.OrderDetailDeliveryMethod(detail.DeliveryMethod),
		CreatedAt:      detail.CreatedAt, PaidAt: detail.PaidAt, CancelledAt: detail.CancelledAt,
		Events: make([]api.OrderEvent, 0, len(detail.Events)),
	}
	out.Items = make([]api.CheckoutQuoteItem, 0, len(detail.Items))
	for _, item := range detail.Items {
		out.Items = append(out.Items, api.CheckoutQuoteItem{
			ListingId: item.ListingID, Title: item.Title, Unit: item.Unit,
			UnitPrice: ghs(item.UnitPricePesewas), Quantity: item.Quantity,
			LineTotal: ghs(item.LineTotalPesewas),
		})
	}
	for _, event := range detail.Events {
		out.Events = append(out.Events, api.OrderEvent{
			ActorType: api.OrderEventActorType(event.ActorType), CreatedAt: event.CreatedAt,
			FromStatus: fromStatus(event.FromStatus), ToStatus: api.OrderStatus(event.ToStatus),
			Note: event.Note,
		})
	}
	if isSeller {
		commission := ghs(detail.CommissionPesewas)
		out.Commission = &commission
		net := ghs(detail.BasePesewas - detail.CommissionPesewas)
		out.SellerNet = &net
		out.Delivery.Address = detail.DeliveryAddress
		out.Delivery.Region = detail.DeliveryRegion
		out.Delivery.District = detail.DeliveryDistrict
		out.Delivery.RecipientName = detail.RecipientName
		out.Delivery.RecipientPhone = detail.RecipientPhone
		out.Delivery.TrackingRef = detail.TrackingRef
		return out
	}
	seller := api.PublicSeller{
		UserId: detail.SellerID, BusinessName: detail.SellerName,
		Bio: detail.SellerBio, Verified: detail.SellerVerification == "verified",
		MemberSince: detail.SellerMemberSince,
	}
	if region := detail.DeliveryRegion; region != nil {
		seller.Region = *region
	}
	if district := detail.DeliveryDistrict; district != nil {
		seller.District = *district
	}
	out.Seller = &seller
	return out
}

// fromStatus converts an optional previous status for an event row.
func fromStatus(status *string) *api.OrderStatus {
	if status == nil {
		return nil
	}
	value := api.OrderStatus(*status)
	return &value
}

// toCheckoutQuote maps the domain quote onto the contract. Commission is
// deliberately absent: buyers see amounts, not the platform's cut.
func toCheckoutQuote(quote checkout.CheckoutQuote) api.CheckoutQuote {
	orders := make([]api.CheckoutOrderQuote, 0, len(quote.Orders))
	for _, order := range quote.Orders {
		items := make([]api.CheckoutQuoteItem, 0, len(order.Items))
		for _, item := range order.Items {
			items = append(items, api.CheckoutQuoteItem{
				ListingId: item.ListingID, Title: item.Title, Unit: item.Unit,
				UnitPrice: ghs(item.UnitPricePesewas), Quantity: int32(item.Quantity), //nolint:gosec // G115: quantity is validated against int32 availability
				LineTotal: ghs(item.LineTotalPesewas),
			})
		}
		orders = append(orders, api.CheckoutOrderQuote{
			SellerId: order.SellerID, SellerName: order.SellerName, Items: items,
			Subtotal: ghs(order.SubtotalPesewas), DeliveryFee: ghs(order.DeliveryFeePesewas),
			Base: ghs(order.BasePesewas),
		})
	}
	return api.CheckoutQuote{
		Orders: orders, Base: ghs(quote.BasePesewas),
		ProcessingFee: ghs(quote.ProcessingFeePesewas), Charge: ghs(quote.ChargePesewas),
	}
}

// orEmpty flattens an optional string.
func orEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func pageParams(page, limit *int32) (int32, int32) {
	p, l := int32(1), int32(20)
	if page != nil {
		p = *page
	}
	if limit != nil {
		l = *limit
	}
	return p, l
}

func statusString[T ~string](status *T) string {
	if status == nil {
		return ""
	}
	return string(*status)
}
