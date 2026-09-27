package handlers

import (
	"context"
	"errors"

	"github.com/dezmymachine/farmish-backend/internal/db"
	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/internal/ledger"
	"github.com/dezmymachine/farmish-backend/internal/orders"
	"github.com/dezmymachine/farmish-backend/internal/users"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

// ListAdminDisputes returns the dispute review queue, oldest first.
func (s Server) ListAdminDisputes(ctx context.Context, req api.ListAdminDisputesRequestObject) (api.ListAdminDisputesResponseObject, error) {
	if _, ok := users.FromContext(ctx); !ok {
		return nil, errNoUser
	}
	page, limit := pageParams(req.Params.Page, req.Params.Limit)
	status := ""
	if req.Params.Status != nil {
		status = string(*req.Params.Status)
	}
	views, total, err := s.OrderActions.ListDisputes(ctx, status, limit, (page-1)*limit)
	var verr *validation.Error
	switch {
	case err == nil:
	case errors.As(err, &verr):
		return api.ListAdminDisputes400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(validationFailed(verr)),
		}, nil
	default:
		return nil, err
	}
	out := make([]api.DisputeAdmin, 0, len(views))
	for _, view := range views {
		out = append(out, toDisputeAdmin(view))
	}
	return api.ListAdminDisputes200JSONResponse{
		Items: out,
		Meta:  api.PageMeta{Page: page, Limit: limit, Total: total},
	}, nil
}

// GetAdminDispute returns one dispute with its order detail and events.
func (s Server) GetAdminDispute(ctx context.Context, req api.GetAdminDisputeRequestObject) (api.GetAdminDisputeResponseObject, error) {
	if _, ok := users.FromContext(ctx); !ok {
		return nil, errNoUser
	}
	view, err := s.OrderActions.GetDispute(ctx, req.Id)
	if classified, handled := s.classifyOrderError(err); handled {
		return api.GetAdminDispute404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse(classified.body),
		}, nil
	}
	if err != nil {
		return nil, err
	}
	return api.GetAdminDispute200JSONResponse(toDisputeAdmin(view)), nil
}

// ResolveDispute decides an open dispute: full refund, release, or split.
func (s Server) ResolveDispute(ctx context.Context, req api.ResolveDisputeRequestObject) (api.ResolveDisputeResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	if req.Body == nil {
		return api.ResolveDispute400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(apierror.New(apierror.CodeBadRequest, "Malformed request")),
		}, nil
	}
	var refundAmount *int64
	if req.Body.RefundAmount != nil {
		if string(req.Body.RefundAmount.Currency) != ledger.CurrencyGHS {
			var invalid validation.Error
			invalid.Add("refundAmount.currency", "must be GHS")
			return api.ResolveDispute400JSONResponse{
				BadRequestJSONResponse: api.BadRequestJSONResponse(validationFailed(&invalid)),
			}, nil
		}
		amount := req.Body.RefundAmount.Amount
		refundAmount = &amount
	}
	view, err := s.OrderActions.Resolve(ctx, u.ID, req.Id, string(req.Body.Outcome), refundAmount, req.Body.Note)
	if classified, handled := s.classifyOrderError(err); handled {
		switch classified.status {
		case 400:
			return api.ResolveDispute400JSONResponse{
				BadRequestJSONResponse: api.BadRequestJSONResponse(classified.body),
			}, nil
		case 404:
			return api.ResolveDispute404JSONResponse{
				NotFoundJSONResponse: api.NotFoundJSONResponse(classified.body),
			}, nil
		default:
			return api.ResolveDispute409JSONResponse{
				ConflictJSONResponse: api.ConflictJSONResponse(classified.body),
			}, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return api.ResolveDispute200JSONResponse(toDisputeAdmin(view)), nil
}

// GetAdminOrder returns an order with both parties' contacts and its ledger
// entries.
func (s Server) GetAdminOrder(ctx context.Context, req api.GetAdminOrderRequestObject) (api.GetAdminOrderResponseObject, error) {
	if _, ok := users.FromContext(ctx); !ok {
		return nil, errNoUser
	}
	order, err := s.OrderActions.GetAdminOrder(ctx, req.Id)
	if classified, handled := s.classifyOrderError(err); handled {
		return api.GetAdminOrder404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse(classified.body),
		}, nil
	}
	if err != nil {
		return nil, err
	}
	return api.GetAdminOrder200JSONResponse(toAdminOrderDetail(order)), nil
}

// RetryRefund re-queues a failed refund's Paystack call.
func (s Server) RetryRefund(ctx context.Context, req api.RetryRefundRequestObject) (api.RetryRefundResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	refund, err := s.OrderActions.RetryRefund(ctx, u.ID, req.Id)
	if classified, handled := s.classifyOrderError(err); handled {
		switch classified.status {
		case 404:
			return api.RetryRefund404JSONResponse{
				NotFoundJSONResponse: api.NotFoundJSONResponse(classified.body),
			}, nil
		default:
			return api.RetryRefund409JSONResponse{
				ConflictJSONResponse: api.ConflictJSONResponse(classified.body),
			}, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return api.RetryRefund200JSONResponse(toRefund(refund)), nil
}

// toDisputeAdmin maps a dispute view onto the contract. The embedded order is
// the seller-shaped detail: it carries the commission, the recipient and the
// events, which is what a resolver needs.
func toDisputeAdmin(view orders.DisputeView) api.DisputeAdmin {
	out := api.DisputeAdmin{
		Id: view.Dispute.ID, OrderId: view.Dispute.OrderID,
		Status:      api.DisputeStatus(view.Dispute.Status),
		Reason:      api.DisputeAdminReason(view.Dispute.Reason),
		Description: view.Dispute.Description, OpenedBy: view.Dispute.OpenedBy,
		CreatedAt: view.Dispute.CreatedAt, UpdatedAt: &view.Dispute.UpdatedAt,
		Order: toOrderDetail(view.Order, true),
	}
	if view.Dispute.Outcome != nil {
		outcome := api.DisputeOutcome(*view.Dispute.Outcome)
		out.Outcome = &outcome
	}
	if view.Dispute.RefundPesewas != nil {
		amount := ghs(*view.Dispute.RefundPesewas)
		out.RefundAmount = &amount
	}
	out.ResolutionNote = view.Dispute.ResolutionNote
	out.ResolvedBy = view.Dispute.ResolvedBy
	out.ResolvedAt = view.Dispute.ResolvedAt
	return out
}

// toAdminOrderDetail maps an admin order onto the contract.
func toAdminOrderDetail(order orders.AdminOrder) api.AdminOrderDetail {
	entries := make([]api.LedgerEntryView, 0, len(order.Entries))
	for _, entry := range order.Entries {
		entries = append(entries, api.LedgerEntryView{
			Account: entry.Account, Amount: ghs(entry.Amount),
			TransactionKind: entry.TransactionKind, TransactionReference: entry.TransactionReference,
			CreatedAt: entry.CreatedAt,
		})
	}
	return api.AdminOrderDetail{
		Order: toOrderDetail(order.Order, true),
		Buyer: api.OrderPartyContact{
			UserId: order.Buyer.UserID, DisplayName: order.Buyer.DisplayName,
			Email: order.Buyer.Email, Phone: order.Buyer.Phone,
		},
		Seller: api.OrderPartyContact{
			UserId: order.Seller.UserID, DisplayName: order.Seller.DisplayName,
			Email: order.Seller.Email, Phone: order.Seller.Phone,
		},
		LedgerEntries: entries,
	}
}

// toRefund maps a refund row onto the contract.
func toRefund(refund db.Refund) api.Refund {
	return api.Refund{
		Id: refund.ID, OrderId: refund.OrderID, Amount: ghs(refund.AmountPesewas),
		Reason: api.RefundReason(refund.Reason), Status: api.RefundStatus(refund.Status),
		CreatedAt: refund.CreatedAt, UpdatedAt: &refund.UpdatedAt,
		FailureReason: refund.FailureReason, PaystackRefundId: refund.PaystackRefundID,
	}
}
