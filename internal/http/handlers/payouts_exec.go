package handlers

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/internal/payouts"
	"github.com/dezmymachine/farmish-backend/internal/users"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

// GetMyBalance returns the caller's money split: payable, held, in-flight
// and paid out.
func (s Server) GetMyBalance(ctx context.Context, _ api.GetMyBalanceRequestObject) (api.GetMyBalanceResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	balance, err := s.Payouts.Balance(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	return api.GetMyBalance200JSONResponse(api.SellerBalance{
		Available: ghs(balance.Available), InEscrow: ghs(balance.InEscrow),
		InFlight: ghs(balance.InFlight), PaidOut: ghs(balance.PaidOut),
	}), nil
}

// ListMyPayouts returns the caller's payouts, newest first.
func (s Server) ListMyPayouts(ctx context.Context, req api.ListMyPayoutsRequestObject) (api.ListMyPayoutsResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	page, limit := pageParams(req.Params.Page, req.Params.Limit)
	items, total, err := s.Payouts.History(ctx, u.ID, limit, (page-1)*limit)
	if err != nil {
		return nil, err
	}
	return api.ListMyPayouts200JSONResponse(api.PayoutList{
		Items: toPayouts(items), Meta: api.PageMeta{Page: page, Limit: limit, Total: total},
	}), nil
}

// ListAdminPayouts returns every payout, newest first, with optional status
// and seller filters.
func (s Server) ListAdminPayouts(ctx context.Context, req api.ListAdminPayoutsRequestObject) (api.ListAdminPayoutsResponseObject, error) {
	if _, ok := users.FromContext(ctx); !ok {
		return nil, errNoUser
	}
	page, limit := pageParams(req.Params.Page, req.Params.Limit)
	status := ""
	if req.Params.Status != nil {
		status = string(*req.Params.Status)
	}
	var sellerID *uuid.UUID
	if req.Params.SellerId != nil {
		id := *req.Params.SellerId
		sellerID = &id
	}
	items, total, err := s.Payouts.AdminList(ctx, status, sellerID, limit, (page-1)*limit)
	var verr *validation.Error
	switch {
	case err == nil:
	case errors.As(err, &verr):
		return api.ListAdminPayouts400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(validationFailed(verr)),
		}, nil
	default:
		return nil, err
	}
	return api.ListAdminPayouts200JSONResponse(api.PayoutList{
		Items: toPayouts(items), Meta: api.PageMeta{Page: page, Limit: limit, Total: total},
	}), nil
}

// RetryPayout queues a fresh payout for a failed or reversed one.
func (s Server) RetryPayout(ctx context.Context, req api.RetryPayoutRequestObject) (api.RetryPayoutResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	payout, err := s.Payouts.RetryPayout(ctx, u.ID, req.Id)
	switch {
	case err == nil:
		return api.RetryPayout200JSONResponse(toPayout(payout)), nil
	case errors.Is(err, payouts.ErrPayoutNotFound):
		return api.RetryPayout404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse(apierror.New(apierror.CodeNotFound, "Payout not found")),
		}, nil
	case errors.Is(err, payouts.ErrRetryNotAllowed):
		return api.RetryPayout409JSONResponse{
			ConflictJSONResponse: api.ConflictJSONResponse(apierror.New(apierror.CodeConflict, "Only a failed or reversed payout may be retried")),
		}, nil
	default:
		return nil, err
	}
}

// toPayouts maps payout rows onto the contract.
func toPayouts(items []payouts.Payout) []api.Payout {
	out := make([]api.Payout, 0, len(items))
	for _, item := range items {
		out = append(out, toPayout(item))
	}
	return out
}

// toPayout maps one payout onto the contract. Account numbers never appear:
// the row holds none, and neither does this mapper.
func toPayout(payout payouts.Payout) api.Payout {
	out := api.Payout{
		Id: payout.ID, SellerId: payout.SellerID, Amount: ghs(payout.AmountPesewas),
		Status: api.PayoutStatus(payout.Status), Reference: payout.Reference,
		CreatedAt: payout.CreatedAt, FailureReason: payout.FailureReason, CompletedAt: payout.CompletedAt,
	}
	return out
}
