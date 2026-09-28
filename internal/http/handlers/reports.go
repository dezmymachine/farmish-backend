package handlers

import (
	"context"
	"errors"

	"github.com/dezmymachine/farmish-backend/internal/engagement"
	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/internal/users"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

// CreateReport records a report on exactly one target.
func (s Server) CreateReport(ctx context.Context, req api.CreateReportRequestObject) (api.CreateReportResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	if req.Body == nil {
		return api.CreateReport400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(apierror.New(apierror.CodeBadRequest, "Malformed request")),
		}, nil
	}
	description := ""
	if req.Body.Description != nil {
		description = *req.Body.Description
	}
	report, err := s.Engagement.CreateReport(ctx, u.ID, req.Body.ListingId, req.Body.UserId, string(req.Body.Reason), description)
	var verr *validation.Error
	switch {
	case err == nil:
		return api.CreateReport201JSONResponse(toReport(report)), nil
	case errors.As(err, &verr):
		return api.CreateReport400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(validationFailed(verr)),
		}, nil
	case errors.Is(err, engagement.ErrNotFound):
		return api.CreateReport404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse(apierror.New(apierror.CodeNotFound, "Report target not found")),
		}, nil
	case errors.Is(err, engagement.ErrAlreadyReported):
		return api.CreateReport409JSONResponse{
			ConflictJSONResponse: api.ConflictJSONResponse(apierror.New(apierror.CodeConflict, "This target is already reported")),
		}, nil
	default:
		return nil, err
	}
}

// ListAdminReports returns the moderation queue, oldest first.
func (s Server) ListAdminReports(ctx context.Context, req api.ListAdminReportsRequestObject) (api.ListAdminReportsResponseObject, error) {
	if _, ok := users.FromContext(ctx); !ok {
		return nil, errNoUser
	}
	page, limit := pageParams(req.Params.Page, req.Params.Limit)
	status := ""
	if req.Params.Status != nil {
		status = string(*req.Params.Status)
	}
	views, total, err := s.Engagement.ListReports(ctx, status, limit, (page-1)*limit)
	var verr *validation.Error
	switch {
	case err == nil:
	case errors.As(err, &verr):
		return api.ListAdminReports400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(validationFailed(verr)),
		}, nil
	default:
		return nil, err
	}
	out := make([]api.ReportListItem, 0, len(views))
	for _, view := range views {
		out = append(out, toReportListItem(view))
	}
	return api.ListAdminReports200JSONResponse(api.ReportList{
		Items: out, Meta: api.PageMeta{Page: page, Limit: limit, Total: total},
	}), nil
}

// ResolveReport decides an open report, suspending listings or hiding named
// reviews where asked.
func (s Server) ResolveReport(ctx context.Context, req api.ResolveReportRequestObject) (api.ResolveReportResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	if req.Body == nil {
		return api.ResolveReport400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(apierror.New(apierror.CodeBadRequest, "Malformed request")),
		}, nil
	}
	report, err := s.Engagement.ResolveReport(ctx, u.ID, req.Id, string(req.Body.Status), string(req.Body.Action), req.Body.Note, req.Body.ReviewId, s.Listings)
	var verr *validation.Error
	switch {
	case err == nil:
		return api.ResolveReport200JSONResponse(toReport(report)), nil
	case errors.As(err, &verr):
		return api.ResolveReport400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(validationFailed(verr)),
		}, nil
	case errors.Is(err, engagement.ErrNotFound):
		return api.ResolveReport404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse(apierror.New(apierror.CodeNotFound, "Report not found")),
		}, nil
	case errors.Is(err, engagement.ErrReportNotOpen), errors.Is(err, engagement.ErrAlreadyHidden):
		return api.ResolveReport409JSONResponse{
			ConflictJSONResponse: api.ConflictJSONResponse(apierror.New(apierror.CodeConflict, "This report cannot be resolved as asked")),
		}, nil
	default:
		return nil, err
	}
}

// toReport maps a report onto the contract.
func toReport(report engagement.Report) api.Report {
	out := api.Report{
		Id: report.ID, ReporterId: report.ReporterID, Reason: api.ReportReason(report.Reason),
		Status: api.ReportStatus(report.Status), CreatedAt: report.CreatedAt,
		Description: report.Description, ListingId: report.ListingID, ReportedUserId: report.ReportedUserID,
		ResolvedAt: report.ResolvedAt,
	}
	if report.Action != nil {
		action := api.ReportAction(*report.Action)
		out.Action = &action
	}
	out.ResolutionNote = report.ResolutionNote
	return out
}

// toReportListItem maps a report view with its target summary.
func toReportListItem(view engagement.ReportView) api.ReportListItem {
	out := api.ReportListItem{
		Id: view.Report.ID, Reason: api.ReportReason(view.Report.Reason),
		Status: api.ReportStatus(view.Report.Status), CreatedAt: view.Report.CreatedAt,
	}
	if view.Target.Listing != nil {
		out.Listing = &api.ReportTargetListing{
			Id: view.Target.Listing.ID, Title: view.Target.Listing.Title, Slug: view.Target.Listing.Slug,
		}
	}
	if view.Target.User != nil {
		out.ReportedUser = &api.ReportTargetUser{
			UserId: view.Target.User.ID, DisplayName: view.Target.User.DisplayName,
		}
	}
	return out
}
