package handlers

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/gin-gonic/gin"

	"github.com/dezmymachine/farmish-backend/internal/http/api"

	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/internal/http/middleware"
	"github.com/dezmymachine/farmish-backend/internal/payments"
	"github.com/dezmymachine/farmish-backend/internal/users"
)

// PaymentStore is the part of payments.Service the HTTP layer uses.
type PaymentStore interface {
	HandleWebhook(ctx context.Context, raw []byte) error
	Verify(ctx context.Context, userID uuid.UUID, reference string) (payments.Payment, error)
}

// PaystackWebhook settles a payment event from Paystack.
//
// The signature was already verified by middleware.PaystackSignature over the
// raw bytes, which is why the handler reads them from there rather than from
// the generated body: only the exact bytes Paystack sent can be trusted.
//
// A malformed body is a 400 (Paystack will not fix it by retrying); anything
// else is a 500, which is what makes Paystack retry. The service has already
// decided which events are duplicates, ignored or rejected, and answers 200
// for all of those.
func (s Server) PaystackWebhook(ctx context.Context, req api.PaystackWebhookRequestObject) (api.PaystackWebhookResponseObject, error) {
	ginCtx, ok := ctx.(*gin.Context)
	if !ok {
		return nil, errors.New("paystack webhook reached without a gin context")
	}
	if s.Payments == nil {
		return nil, errors.New("paystack is not configured")
	}
	raw, found := middleware.RawPaystackBody(ginCtx)
	if !found {
		// Unreachable: the route is only reachable through the signature
		// middleware, which refuses first. Answering 500 rather than acting on
		// an unverified body is the safe failure.
		return nil, errors.New("paystack webhook reached without a verified body")
	}
	if err := s.Payments.HandleWebhook(ctx, raw); err != nil {
		if errors.Is(err, payments.ErrMalformedEvent) {
			return api.PaystackWebhook400JSONResponse{
				BadRequestJSONResponse: api.BadRequestJSONResponse(
					apierror.New(apierror.CodeBadRequest, "Malformed webhook body")),
			}, nil
		}
		// A transient failure: 500 so Paystack retries, and the webhook_events
		// row was rolled back with it.
		return nil, err
	}
	return api.PaystackWebhook200Response{}, nil
}

// GetPaymentStatus returns one of the caller's payments, verifying with
// Paystack when the local row is still pending (the lost-webhook fallback).
func (s Server) GetPaymentStatus(ctx context.Context, req api.GetPaymentStatusRequestObject) (api.GetPaymentStatusResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	if s.Payments == nil {
		return nil, errors.New("paystack is not configured")
	}
	payment, err := s.Payments.Verify(ctx, u.ID, req.Reference)
	switch {
	case err == nil:
	case errors.Is(err, payments.ErrNotFound):
		// Not ours and not theirs look the same, so a 404 leaks nothing.
		return api.GetPaymentStatus404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse(apierror.New(apierror.CodeNotFound, "Payment not found")),
		}, nil
	case errors.Is(err, payments.ErrProviderUnavailable):
		return api.GetPaymentStatus502JSONResponse{
			PaymentProviderErrorJSONResponse: api.PaymentProviderErrorJSONResponse(
				apierror.New(apierror.CodePaymentProvider, "The payment provider is unavailable; try again")),
		}, nil
	default:
		return nil, err
	}
	return api.GetPaymentStatus200JSONResponse(toPaymentStatus(payment)), nil
}

// toPaymentStatus maps a payment onto the contract. It carries no provider id,
// no authorization URL and nothing else a buyer does not need.
func toPaymentStatus(p payments.Payment) api.PaymentStatus {
	out := api.PaymentStatus{
		Reference: p.Reference,
		Status:    api.PaymentStatusStatus(p.Status),
		Base:      ghs(p.Base),
		Charge:    ghs(p.Charge),
		// The processing fee is non-refundable (DOMAIN §2.2).
		ProcessingFee: ghs(p.Fee),
		PaidAt:        p.PaidAt,
	}
	if p.FailureReason != nil {
		out.FailureReason = p.FailureReason
	}
	return out
}
