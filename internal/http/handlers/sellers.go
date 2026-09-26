package handlers

import (
	"context"
	"errors"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/internal/sellers"
	"github.com/dezmymachine/farmish-backend/internal/users"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

// requestContext unwraps the *gin.Context strict handlers receive to the
// underlying request context. All middleware values (user, logger, request
// ID) live on the request context, so nothing is lost. The unwrap matters:
// gin pools Context objects, and the wrapper must not escape the handler.
// Anything that outlives the request (an HTTP client's keep-alive read loop,
// a background job) would race on the recycled wrapper. Synchronous database
// calls are unaffected either way.
func requestContext(ctx context.Context) context.Context {
	if gc, ok := ctx.(*gin.Context); ok && gc.Request != nil {
		return gc.Request.Context()
	}
	return ctx
}

// SellerStore is the part of sellers.Service the handlers use.
type SellerStore interface {
	GetMine(ctx context.Context, userID uuid.UUID) (sellers.Profile, error)
	UpsertMine(ctx context.Context, userID uuid.UUID, in sellers.ProfileInput) (sellers.Profile, error)
	GetPublic(ctx context.Context, userID uuid.UUID) (sellers.PublicProfile, error)
	ListByStatus(ctx context.Context, status string, limit, offset int32) ([]sellers.AdminProfile, int64, error)
	Verify(ctx context.Context, adminID, targetID uuid.UUID, decision, reason string) (sellers.AdminProfile, error)
}

// GetMySellerProfile returns the caller's seller profile, or 404 when they
// never created one.
func (s Server) GetMySellerProfile(ctx context.Context, _ api.GetMySellerProfileRequestObject) (api.GetMySellerProfileResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	p, err := s.Sellers.GetMine(ctx, u.ID)
	switch {
	case errors.Is(err, sellers.ErrNotFound):
		return api.GetMySellerProfile404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse(apierror.New(apierror.CodeNotFound, "Seller profile not found")),
		}, nil
	case err != nil:
		return nil, err
	}
	return api.GetMySellerProfile200JSONResponse(toSellerProfile(p)), nil
}

// UpdateMySellerProfile creates or edits the caller's seller profile. The
// request body was validated against the spec before reaching here;
// cross-field rules (idType/idNumber together, WhatsApp normalisation)
// return 400 with field details.
func (s Server) UpdateMySellerProfile(ctx context.Context, req api.UpdateMySellerProfileRequestObject) (api.UpdateMySellerProfileResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	if req.Body == nil {
		return api.UpdateMySellerProfile400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(apierror.New(apierror.CodeBadRequest, "Malformed request")),
		}, nil
	}
	// Unwrap: persisting may call Firebase (claim mirror) over HTTP.
	p, err := s.Sellers.UpsertMine(requestContext(ctx), u.ID, toProfileInput(*req.Body))
	var verr *validation.Error
	switch {
	case errors.As(err, &verr):
		return api.UpdateMySellerProfile400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(validationFailed(verr)),
		}, nil
	case err != nil:
		return nil, err
	}
	return api.UpdateMySellerProfile200JSONResponse(toSellerProfile(p)), nil
}

// GetPublicSeller returns the safe projection of a seller. It is public and
// never contains contact, identity or account fields.
func (s Server) GetPublicSeller(ctx context.Context, req api.GetPublicSellerRequestObject) (api.GetPublicSellerResponseObject, error) {
	p, err := s.Sellers.GetPublic(ctx, req.UserId)
	switch {
	case errors.Is(err, sellers.ErrNotFound):
		return api.GetPublicSeller404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse(apierror.New(apierror.CodeNotFound, "Seller not found")),
		}, nil
	case err != nil:
		return nil, err
	}
	return api.GetPublicSeller200JSONResponse(toPublicSeller(p)), nil
}

// ListAdminSellers returns the verification review queue for one status.
func (s Server) ListAdminSellers(ctx context.Context, req api.ListAdminSellersRequestObject) (api.ListAdminSellersResponseObject, error) {
	page, limit := int32(1), int32(20)
	if req.Params.Page != nil {
		page = *req.Params.Page
	}
	if req.Params.Limit != nil {
		limit = *req.Params.Limit
	}
	items, total, err := s.Sellers.ListByStatus(ctx, string(req.Params.Status), limit, (page-1)*limit)
	var verr *validation.Error
	switch {
	case errors.As(err, &verr):
		return api.ListAdminSellers400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(validationFailed(verr)),
		}, nil
	case err != nil:
		return nil, err
	}
	out := make([]api.SellerProfileAdmin, 0, len(items))
	for _, item := range items {
		out = append(out, toAdminProfile(item))
	}
	return api.ListAdminSellers200JSONResponse{
		Items: out,
		Meta:  api.PageMeta{Page: page, Limit: limit, Total: total},
	}, nil
}

// VerifySeller approves or rejects a pending seller profile.
func (s Server) VerifySeller(ctx context.Context, req api.VerifySellerRequestObject) (api.VerifySellerResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	if req.Body == nil {
		return api.VerifySeller400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(apierror.New(apierror.CodeBadRequest, "Malformed request")),
		}, nil
	}
	reason := ""
	if req.Body.Reason != nil {
		reason = *req.Body.Reason
	}
	// Unwrap: deciding mirrors the Firebase claim over HTTP.
	p, err := s.Sellers.Verify(requestContext(ctx), u.ID, req.UserId, string(req.Body.Decision), reason)
	var verr *validation.Error
	switch {
	case errors.As(err, &verr):
		return api.VerifySeller400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(validationFailed(verr)),
		}, nil
	case errors.Is(err, sellers.ErrNotFound):
		return api.VerifySeller404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse(apierror.New(apierror.CodeNotFound, "Seller not found")),
		}, nil
	case errors.Is(err, sellers.ErrInvalidTransition):
		return api.VerifySeller409JSONResponse{
			ConflictJSONResponse: api.ConflictJSONResponse(apierror.New(apierror.CodeInvalidTransition, "Only a pending seller can be decided")),
		}, nil
	case err != nil:
		return nil, err
	}
	return api.VerifySeller200JSONResponse(toAdminProfile(p)), nil
}

func toProfileInput(body api.UpdateMySellerProfileJSONRequestBody) sellers.ProfileInput {
	in := sellers.ProfileInput{
		BusinessName: body.BusinessName,
		Region:       string(body.Region),
		District:     body.District,
		Bio:          body.Bio,
		Whatsapp:     body.Whatsapp,
		IDNumber:     body.IdNumber,
	}
	if body.ShowPhone != nil {
		in.ShowPhone = *body.ShowPhone
	}
	if body.ShowWhatsapp != nil {
		in.ShowWhatsapp = *body.ShowWhatsapp
	}
	if body.IdType != nil {
		t := string(*body.IdType)
		in.IDType = &t
	}
	return in
}

func toSellerProfile(p sellers.Profile) api.SellerProfile {
	out := api.SellerProfile{
		BusinessName: p.BusinessName, Region: p.Region, District: p.District,
		Bio: p.Bio, ShowPhone: p.ShowPhone, ShowWhatsapp: p.ShowWhatsapp,
		Whatsapp: p.Whatsapp, VerificationStatus: api.SellerProfileVerificationStatus(p.VerificationStatus),
		IdNumberLast4: p.IDNumberLast4, SubmittedAt: p.SubmittedAt,
		ReviewedAt: p.ReviewedAt, RejectionReason: p.RejectionReason,
	}
	if p.IDType != nil {
		t := api.SellerProfileIdType(*p.IDType)
		out.IdType = &t
	}
	return out
}

func toPublicSeller(p sellers.PublicProfile) api.PublicSeller {
	return api.PublicSeller{
		UserId: p.UserID, BusinessName: p.BusinessName, Region: p.Region,
		District: p.District, Bio: p.Bio, Verified: p.Verified, MemberSince: p.MemberSince,
	}
}

func toAdminProfile(p sellers.AdminProfile) api.SellerProfileAdmin {
	out := api.SellerProfileAdmin{
		UserId: p.UserID, DisplayName: p.DisplayName, Email: p.Email, Phone: p.Phone,
		BusinessName: p.BusinessName, Region: p.Region, District: p.District,
		Bio: p.Bio, ShowPhone: p.ShowPhone, ShowWhatsapp: p.ShowWhatsapp,
		Whatsapp: p.Whatsapp, VerificationStatus: api.SellerProfileAdminVerificationStatus(p.VerificationStatus),
		IdNumberLast4: p.IDNumberLast4, SubmittedAt: p.SubmittedAt,
		ReviewedAt: p.ReviewedAt, RejectionReason: p.RejectionReason,
	}
	if p.IDType != nil {
		t := api.SellerProfileAdminIdType(*p.IDType)
		out.IdType = &t
	}
	return out
}
