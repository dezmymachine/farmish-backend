package handlers

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/internal/orders"
	"github.com/dezmymachine/farmish-backend/internal/users"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

// OrderActions is the transition surface these handlers use. The actions
// already enforce the actor's relation to the order, so the handlers only map
// errors onto the contract.
type OrderActions interface {
	Accept(ctx context.Context, sellerID, orderID uuid.UUID) (orders.Order, error)
	Reject(ctx context.Context, sellerID, orderID uuid.UUID, reason string) (orders.Order, error)
	Ship(ctx context.Context, sellerID, orderID uuid.UUID, trackingRef string) (orders.Order, error)
	MarkDelivered(ctx context.Context, sellerID, orderID uuid.UUID) (orders.Order, error)
	Cancel(ctx context.Context, buyerID, orderID uuid.UUID, reason string) (orders.Order, error)
	ConfirmReceipt(ctx context.Context, buyerID, orderID uuid.UUID) (orders.Order, error)
	Dispute(ctx context.Context, buyerID, orderID uuid.UUID, reason, description string) (orders.Order, error)
}

// orderError classifies a transition error once, so every endpoint answers
// the same cause with the same shape.
type orderError struct {
	status int
	body   api.Error
}

func (s Server) classifyOrderError(err error) (orderError, bool) {
	var invalid *validation.Error
	var transition *orders.InvalidTransitionError
	var forbidden *orders.ForbiddenError
	switch {
	case errors.As(err, &invalid):
		return orderError{status: 400, body: validationFailed(invalid)}, true
	case errors.As(err, &transition):
		// The 409 carries what the client tried against what the order was.
		from, to := transition.From, transition.To
		return orderError{status: 409, body: apierror.WithDetails(apierror.CodeInvalidTransition,
			"That action is not allowed in this state",
			[]api.ErrorDetail{
				{Location: api.ErrorDetailLocationBody, Field: &from, Message: "current status"},
				{Location: api.ErrorDetailLocationBody, Field: &to, Message: "requested status"},
			})}, true
	case errors.As(err, &forbidden):
		return orderError{status: 403, body: api.Error{Error: api.ErrorBody{
			Code: apierror.CodeForbidden, Message: "You may not perform this action on this order",
		}}}, true
	case errors.Is(err, orders.ErrNotFound):
		return orderError{status: 404, body: api.Error{Error: api.ErrorBody{
			Code: apierror.CodeNotFound, Message: "Order not found",
		}}}, true
	case errors.Is(err, orders.ErrDisputeExists):
		return orderError{status: 409, body: api.Error{Error: api.ErrorBody{
			Code: apierror.CodeConflict, Message: "This order already has a dispute",
		}}}, true
	}
	return orderError{}, false
}

// accept answers with the moved order shaped for the seller, who just acted.
func (s Server) sellerDetail(ctx context.Context, callerID, orderID uuid.UUID) (api.OrderDetail, error) {
	detail, _, err := s.Orders.Get(ctx, callerID, orderID)
	if err != nil {
		return api.OrderDetail{}, err
	}
	return toOrderDetail(detail, true), nil
}

// buyerDetail answers with the moved order shaped for the buyer, who just
// acted. A read failure here is a programming error, not a client error.
func (s Server) buyerDetail(ctx context.Context, callerID, orderID uuid.UUID) api.OrderDetail {
	detail, _, err := s.Orders.Get(ctx, callerID, orderID)
	if err != nil {
		return api.OrderDetail{}
	}
	return toOrderDetail(detail, false)
}

// AcceptOrder is the seller accepting a paid order.
func (s Server) AcceptOrder(ctx context.Context, req api.AcceptOrderRequestObject) (api.AcceptOrderResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	order, err := s.OrderActions.Accept(ctx, u.ID, req.Id)
	if classified, handled := s.classifyOrderError(err); handled {
		return api.AcceptOrderdefaultJSONResponse{Body: classified.body, StatusCode: classified.status}, nil
	}
	if err != nil {
		return nil, err
	}
	detail, err := s.sellerDetail(ctx, u.ID, order.ID)
	if err != nil {
		return nil, err
	}
	return api.AcceptOrder200JSONResponse(detail), nil
}

// RejectOrder is the seller refusing the order.
func (s Server) RejectOrder(ctx context.Context, req api.RejectOrderRequestObject) (api.RejectOrderResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	if req.Body == nil {
		return api.RejectOrder400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(apierror.New(apierror.CodeBadRequest, "Malformed request")),
		}, nil
	}
	order, err := s.OrderActions.Reject(ctx, u.ID, req.Id, req.Body.Reason)
	if classified, handled := s.classifyOrderError(err); handled {
		return api.RejectOrderdefaultJSONResponse{Body: classified.body, StatusCode: classified.status}, nil
	}
	if err != nil {
		return nil, err
	}
	detail, err := s.sellerDetail(ctx, u.ID, order.ID)
	if err != nil {
		return nil, err
	}
	return api.RejectOrder200JSONResponse(detail), nil
}

// ShipOrder is the seller marking the order on its way.
func (s Server) ShipOrder(ctx context.Context, req api.ShipOrderRequestObject) (api.ShipOrderResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	trackingRef := ""
	if req.Body != nil && req.Body.TrackingRef != nil {
		trackingRef = *req.Body.TrackingRef
	}
	order, err := s.OrderActions.Ship(ctx, u.ID, req.Id, trackingRef)
	if classified, handled := s.classifyOrderError(err); handled {
		return api.ShipOrderdefaultJSONResponse{Body: classified.body, StatusCode: classified.status}, nil
	}
	if err != nil {
		return nil, err
	}
	detail, err := s.sellerDetail(ctx, u.ID, order.ID)
	if err != nil {
		return nil, err
	}
	return api.ShipOrder200JSONResponse(detail), nil
}

// MarkOrderDelivered is the seller handing the order over.
func (s Server) MarkOrderDelivered(ctx context.Context, req api.MarkOrderDeliveredRequestObject) (api.MarkOrderDeliveredResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	order, err := s.OrderActions.MarkDelivered(ctx, u.ID, req.Id)
	if classified, handled := s.classifyOrderError(err); handled {
		return api.MarkOrderDelivereddefaultJSONResponse{Body: classified.body, StatusCode: classified.status}, nil
	}
	if err != nil {
		return nil, err
	}
	detail, err := s.sellerDetail(ctx, u.ID, order.ID)
	if err != nil {
		return nil, err
	}
	return api.MarkOrderDelivered200JSONResponse(detail), nil
}

// CancelOrder is the buyer backing out before acceptance.
func (s Server) CancelOrder(ctx context.Context, req api.CancelOrderRequestObject) (api.CancelOrderResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	reason := ""
	if req.Body != nil {
		reason = req.Body.Reason
	}
	order, err := s.OrderActions.Cancel(ctx, u.ID, req.Id, reason)
	if classified, handled := s.classifyOrderError(err); handled {
		return api.CancelOrderdefaultJSONResponse{Body: classified.body, StatusCode: classified.status}, nil
	}
	if err != nil {
		return nil, err
	}
	return api.CancelOrder200JSONResponse(s.buyerDetail(ctx, u.ID, order.ID)), nil
}

// ConfirmOrderReceipt is the buyer closing a shipped or delivered order.
func (s Server) ConfirmOrderReceipt(ctx context.Context, req api.ConfirmOrderReceiptRequestObject) (api.ConfirmOrderReceiptResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	order, err := s.OrderActions.ConfirmReceipt(ctx, u.ID, req.Id)
	if classified, handled := s.classifyOrderError(err); handled {
		return api.ConfirmOrderReceiptdefaultJSONResponse{Body: classified.body, StatusCode: classified.status}, nil
	}
	if err != nil {
		return nil, err
	}
	return api.ConfirmOrderReceipt200JSONResponse(s.buyerDetail(ctx, u.ID, order.ID)), nil
}

// DisputeOrder opens a case and stops the auto-complete clock.
func (s Server) DisputeOrder(ctx context.Context, req api.DisputeOrderRequestObject) (api.DisputeOrderResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	if req.Body == nil {
		return api.DisputeOrder400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(apierror.New(apierror.CodeBadRequest, "Malformed request")),
		}, nil
	}
	order, err := s.OrderActions.Dispute(ctx, u.ID, req.Id, string(req.Body.Reason), req.Body.Description)
	if classified, handled := s.classifyOrderError(err); handled {
		return api.DisputeOrderdefaultJSONResponse{Body: classified.body, StatusCode: classified.status}, nil
	}
	if err != nil {
		return nil, err
	}
	return api.DisputeOrder200JSONResponse(s.buyerDetail(ctx, u.ID, order.ID)), nil
}
