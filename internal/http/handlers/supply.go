package handlers

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/oapi-codegen/runtime/types"

	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/internal/supply"
	"github.com/dezmymachine/farmish-backend/internal/users"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

// SupplyStore is the part of supply.Service these handlers use.
type SupplyStore interface {
	Create(ctx context.Context, userID uuid.UUID, in supply.Input) (supply.Request, error)
	Get(ctx context.Context, callerID, requestID uuid.UUID) (supply.Request, error)
	List(ctx context.Context, callerID uuid.UUID, status string, limit, offset int32) ([]supply.Request, int64, error)
	AdminList(ctx context.Context, status string, limit, offset int32) ([]supply.Request, int64, error)
	Transition(ctx context.Context, actorID uuid.UUID, admin bool, requestID uuid.UUID, to, note string) (supply.Request, error)
}

// CreateSupplyRequest records a bulk supply request.
func (s Server) CreateSupplyRequest(ctx context.Context, req api.CreateSupplyRequestRequestObject) (api.CreateSupplyRequestResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	if req.Body == nil {
		return api.CreateSupplyRequest400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(apierror.New(apierror.CodeBadRequest, "Malformed request")),
		}, nil
	}
	created, err := s.Supply.Create(ctx, u.ID, toSupplyInput(req.Body))
	var verr *validation.Error
	switch {
	case err == nil:
		return api.CreateSupplyRequest201JSONResponse(toSupplyRequest(created)), nil
	case errors.As(err, &verr):
		return api.CreateSupplyRequest400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(validationFailed(verr)),
		}, nil
	default:
		return nil, err
	}
}

// ListMySupplyRequests returns the caller's requests, newest first.
func (s Server) ListMySupplyRequests(ctx context.Context, req api.ListMySupplyRequestsRequestObject) (api.ListMySupplyRequestsResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	page, limit := pageParams(req.Params.Page, req.Params.Limit)
	status := ""
	if req.Params.Status != nil {
		status = string(*req.Params.Status)
	}
	items, total, err := s.Supply.List(ctx, u.ID, status, limit, (page-1)*limit)
	var verr *validation.Error
	switch {
	case err == nil:
	case errors.As(err, &verr):
		return api.ListMySupplyRequests400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(validationFailed(verr)),
		}, nil
	default:
		return nil, err
	}
	out := make([]api.SupplyRequestSummary, 0, len(items))
	for _, item := range items {
		out = append(out, toSupplyRequestSummary(item))
	}
	return api.ListMySupplyRequests200JSONResponse(api.SupplyRequestSummaryList{
		Items: out, Meta: api.PageMeta{Page: page, Limit: limit, Total: total},
	}), nil
}

// GetSupplyRequest returns one of the caller's requests with its events.
func (s Server) GetSupplyRequest(ctx context.Context, req api.GetSupplyRequestRequestObject) (api.GetSupplyRequestResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	request, err := s.Supply.Get(ctx, u.ID, req.Id)
	switch {
	case errors.Is(err, supply.ErrNotFound):
		return api.GetSupplyRequest404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse(apierror.New(apierror.CodeNotFound, "Supply request not found")),
		}, nil
	case err != nil:
		return nil, err
	}
	return api.GetSupplyRequest200JSONResponse(toSupplyRequest(request)), nil
}

// CancelSupplyRequest cancels the caller's pending request.
func (s Server) CancelSupplyRequest(ctx context.Context, req api.CancelSupplyRequestRequestObject) (api.CancelSupplyRequestResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	reason := ""
	if req.Body != nil && req.Body.Reason != nil {
		reason = *req.Body.Reason
	}
	request, err := s.Supply.Transition(ctx, u.ID, false, req.Id, supply.StatusCancelled, reason)
	var verr *validation.Error
	switch {
	case err == nil:
		return api.CancelSupplyRequest200JSONResponse(toSupplyRequest(request)), nil
	case errors.As(err, &verr):
		return api.CancelSupplyRequest400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(validationFailed(verr)),
		}, nil
	case errors.Is(err, supply.ErrNotFound):
		return api.CancelSupplyRequest404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse(apierror.New(apierror.CodeNotFound, "Supply request not found")),
		}, nil
	case errors.Is(err, supply.ErrInvalidTransition):
		return api.CancelSupplyRequest409JSONResponse{
			ConflictJSONResponse: api.ConflictJSONResponse(apierror.New(apierror.CodeInvalidTransition, "Only a pending request may be cancelled")),
		}, nil
	default:
		return nil, err
	}
}

// ListAdminSupplyRequests returns every request, newest first.
func (s Server) ListAdminSupplyRequests(ctx context.Context, req api.ListAdminSupplyRequestsRequestObject) (api.ListAdminSupplyRequestsResponseObject, error) {
	if _, ok := users.FromContext(ctx); !ok {
		return nil, errNoUser
	}
	page, limit := pageParams(req.Params.Page, req.Params.Limit)
	status := ""
	if req.Params.Status != nil {
		status = string(*req.Params.Status)
	}
	items, total, err := s.Supply.AdminList(ctx, status, limit, (page-1)*limit)
	var verr *validation.Error
	switch {
	case err == nil:
	case errors.As(err, &verr):
		return api.ListAdminSupplyRequests400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(validationFailed(verr)),
		}, nil
	default:
		return nil, err
	}
	out := make([]api.SupplyRequestSummary, 0, len(items))
	for _, item := range items {
		out = append(out, toSupplyRequestSummary(item))
	}
	return api.ListAdminSupplyRequests200JSONResponse(api.SupplyRequestSummaryList{
		Items: out, Meta: api.PageMeta{Page: page, Limit: limit, Total: total},
	}), nil
}

// SetSupplyRequestStatus moves a request through an admin transition.
func (s Server) SetSupplyRequestStatus(ctx context.Context, req api.SetSupplyRequestStatusRequestObject) (api.SetSupplyRequestStatusResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	if req.Body == nil {
		return api.SetSupplyRequestStatus400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(apierror.New(apierror.CodeBadRequest, "Malformed request")),
		}, nil
	}
	note := ""
	if req.Body.Note != nil {
		note = *req.Body.Note
	}
	request, err := s.Supply.Transition(ctx, u.ID, true, req.Id, string(req.Body.Status), note)
	var verr *validation.Error
	switch {
	case err == nil:
		return api.SetSupplyRequestStatus200JSONResponse(toSupplyRequest(request)), nil
	case errors.As(err, &verr):
		return api.SetSupplyRequestStatus400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(validationFailed(verr)),
		}, nil
	case errors.Is(err, supply.ErrNotFound):
		return api.SetSupplyRequestStatus404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse(apierror.New(apierror.CodeNotFound, "Supply request not found")),
		}, nil
	case errors.Is(err, supply.ErrInvalidTransition):
		return api.SetSupplyRequestStatus409JSONResponse{
			ConflictJSONResponse: api.ConflictJSONResponse(apierror.New(apierror.CodeInvalidTransition, "That move is not allowed from this status")),
		}, nil
	default:
		return nil, err
	}
}

// toSupplyInput converts the contract body. Item semantics belong to the
// service, which reports them as field errors.
func toSupplyInput(body *api.CreateSupplyRequest) supply.Input {
	in := supply.Input{
		DeliveryName: orEmpty(body.DeliveryName), DeliveryPhone: orEmpty(body.DeliveryPhone),
		DeliveryAddress: orEmpty(body.DeliveryAddress), Notes: orEmpty(body.Notes),
	}
	if body.ExpectedDate != nil {
		in.ExpectedDate = body.ExpectedDate.Format("2006-01-02")
	}
	for _, line := range body.Items {
		in.Items = append(in.Items, supply.ItemInput{
			CategorySlug: line.CategorySlug, ProductName: line.ProductName,
			Quantity: line.Quantity, Unit: line.Unit,
		})
	}
	return in
}

// toSupplyRequest maps a full request with its events.
func toSupplyRequest(request supply.Request) api.SupplyRequest {
	out := api.SupplyRequest{
		Id: request.ID, RequestNumber: request.RequestNumber,
		Status: api.SupplyStatus(request.Status), CreatedAt: request.CreatedAt,
		DeliveryName: request.DeliveryName, DeliveryPhone: request.DeliveryPhone,
		DeliveryAddress: request.DeliveryAddress, Notes: request.Notes,
	}
	if request.ExpectedDate != nil {
		day := types.Date{Time: *request.ExpectedDate}
		out.ExpectedDate = &day
	}
	out.Items = make([]api.SupplyItem, 0, len(request.Items))
	for _, item := range request.Items {
		out.Items = append(out.Items, api.SupplyItem{
			CategorySlug: item.CategorySlug, ProductName: item.ProductName,
			Quantity: item.Quantity, Unit: item.Unit,
		})
	}
	out.Events = make([]api.SupplyEvent, 0, len(request.Events))
	for _, event := range request.Events {
		out.Events = append(out.Events, api.SupplyEvent{
			ToStatus: api.SupplyStatus(event.To), Note: event.Note, CreatedAt: event.CreatedAt,
		})
		if event.From != nil {
			from := api.SupplyStatus(*event.From)
			out.Events[len(out.Events)-1].FromStatus = &from
		}
	}
	return out
}

// toSupplyRequestSummary maps a request without its events.
func toSupplyRequestSummary(request supply.Request) api.SupplyRequestSummary {
	full := toSupplyRequest(request)
	return api.SupplyRequestSummary{
		Id: full.Id, RequestNumber: full.RequestNumber, Status: full.Status,
		Items: full.Items, CreatedAt: full.CreatedAt,
	}
}
