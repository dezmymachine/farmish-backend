package handlers

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/dezmymachine/farmish-backend/internal/checkout"
	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/internal/users"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

// CheckoutStore is the part of checkout.Service the quote handler uses.
type CheckoutStore interface {
	Quote(ctx context.Context, buyerID uuid.UUID, in checkout.QuoteInput) (checkout.CheckoutQuote, error)
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
	lines := make([]checkout.CartLine, 0, len(req.Body.Lines))
	for _, line := range req.Body.Lines {
		lines = append(lines, checkout.CartLine{ListingID: line.ListingId, Quantity: int(line.Quantity)})
	}
	var requestInvalid validation.Error
	delivery := make(map[uuid.UUID]checkout.DeliveryChoice, len(req.Body.Delivery))
	for i, choice := range req.Body.Delivery {
		if _, duplicate := delivery[choice.SellerId]; duplicate {
			requestInvalid.Add(fmt.Sprintf("delivery[%d].sellerId", i), "each seller may appear only once")
			continue
		}
		delivery[choice.SellerId] = checkout.DeliveryChoice{
			Method: string(choice.Method), Address: orEmpty(choice.Address), Region: orEmpty(choice.Region),
			District: orEmpty(choice.District), RecipientName: orEmpty(choice.RecipientName),
			RecipientPhone: orEmpty(choice.RecipientPhone),
		}
	}
	if err := requestInvalid.OrNil(); err != nil {
		return api.QuoteCheckout400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(validationFailed(&requestInvalid)),
		}, nil
	}

	quote, err := s.Checkout.Quote(ctx, u.ID, checkout.QuoteInput{Lines: lines, Delivery: delivery})
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

func toCheckoutQuote(quote checkout.CheckoutQuote) api.CheckoutQuote {
	orders := make([]api.CheckoutOrderQuote, 0, len(quote.Orders))
	for _, order := range quote.Orders {
		items := make([]api.CheckoutQuoteItem, 0, len(order.Items))
		for _, item := range order.Items {
			items = append(items, api.CheckoutQuoteItem{
				ListingId: item.ListingID, Title: item.Title, Unit: item.Unit,
				UnitPrice: ghs(item.UnitPricePesewas), Quantity: int32(item.Quantity), //nolint:gosec // G115: validated quantities cannot exceed int32 availability
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

func orEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
