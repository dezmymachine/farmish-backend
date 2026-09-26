package handlers

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/internal/listings"
	"github.com/dezmymachine/farmish-backend/internal/users"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

// ListingStore is the part of listings.Service the handlers use.
type ListingStore interface {
	Create(ctx context.Context, sellerID uuid.UUID, in listings.Input, publish bool) (listings.View, error)
	GetOwn(ctx context.Context, sellerID, id uuid.UUID) (listings.View, error)
	ListOwn(ctx context.Context, sellerID uuid.UUID, status string, limit, offset int32) ([]listings.Summary, int64, error)
	Update(ctx context.Context, sellerID, id uuid.UUID, patch listings.Patch) (listings.View, error)
	Delete(ctx context.Context, sellerID, id uuid.UUID) error
	Publish(ctx context.Context, sellerID, id uuid.UUID) (listings.View, error)
	Renew(ctx context.Context, sellerID, id uuid.UUID) (listings.View, error)
	MarkSold(ctx context.Context, sellerID, id uuid.UUID) (listings.View, error)
	Archive(ctx context.Context, sellerID, id uuid.UUID) (listings.View, error)
}

// CreateListing creates a listing for the signed-in seller.
func (s Server) CreateListing(ctx context.Context, req api.CreateListingRequestObject) (api.CreateListingResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	if req.Body == nil {
		return api.CreateListing400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(apierror.New(apierror.CodeBadRequest, "Malformed request")),
		}, nil
	}
	publish := req.Body.Publish != nil && *req.Body.Publish
	view, err := s.Listings.Create(requestContext(ctx), u.ID, toListingInput(*req.Body), publish)
	var verr *validation.Error
	switch {
	case err == nil:
		return api.CreateListing201JSONResponse(toSellerListing(view)), nil
	case errors.Is(err, listings.ErrSellerProfileRequired):
		return api.CreateListing403JSONResponse{
			ForbiddenJSONResponse: api.ForbiddenJSONResponse(
				apierror.New(apierror.CodeSellerProfileRequired, "Create a seller profile before listing")),
		}, nil
	case errors.As(err, &verr):
		return api.CreateListing400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(validationFailed(verr)),
		}, nil
	default:
		return nil, err
	}
}

// ListMyListings returns the caller's listings.
func (s Server) ListMyListings(ctx context.Context, req api.ListMyListingsRequestObject) (api.ListMyListingsResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	page, limit := int32(1), int32(20)
	if req.Params.Page != nil {
		page = *req.Params.Page
	}
	if req.Params.Limit != nil {
		limit = *req.Params.Limit
	}
	var status string
	if req.Params.Status != nil {
		status = string(*req.Params.Status)
	}
	items, total, err := s.Listings.ListOwn(ctx, u.ID, status, limit, (page-1)*limit)
	if err != nil {
		return nil, err
	}
	out := make([]api.SellerListingSummary, 0, len(items))
	for _, item := range items {
		out = append(out, toListingSummary(item))
	}
	return api.ListMyListings200JSONResponse{
		Items: out,
		Meta:  api.PageMeta{Page: page, Limit: limit, Total: total},
	}, nil
}

// GetMyListing returns one of the caller's listings.
func (s Server) GetMyListing(ctx context.Context, req api.GetMyListingRequestObject) (api.GetMyListingResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	view, err := s.Listings.GetOwn(ctx, u.ID, req.Id)
	switch {
	case errors.Is(err, listings.ErrNotFound):
		return api.GetMyListing404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse(apierror.New(apierror.CodeNotFound, "Listing not found")),
		}, nil
	case errors.Is(err, listings.ErrForbidden):
		return api.GetMyListing403JSONResponse{
			ForbiddenJSONResponse: api.ForbiddenJSONResponse(apierror.New(apierror.CodeForbidden, "You do not have access to this resource")),
		}, nil
	case err != nil:
		return nil, err
	}
	return api.GetMyListing200JSONResponse(toSellerListing(view)), nil
}

// UpdateListing edits one of the caller's listings.
func (s Server) UpdateListing(ctx context.Context, req api.UpdateListingRequestObject) (api.UpdateListingResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	if req.Body == nil {
		return api.UpdateListing400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(apierror.New(apierror.CodeBadRequest, "Malformed request")),
		}, nil
	}
	view, err := s.Listings.Update(requestContext(ctx), u.ID, req.Id, toListingPatch(*req.Body))
	switch {
	case errors.Is(err, listings.ErrNotFound):
		return api.UpdateListing404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse(apierror.New(apierror.CodeNotFound, "Listing not found")),
		}, nil
	case errors.Is(err, listings.ErrForbidden):
		return api.UpdateListing403JSONResponse{
			ForbiddenJSONResponse: api.ForbiddenJSONResponse(apierror.New(apierror.CodeForbidden, "You do not have access to this resource")),
		}, nil
	case errors.Is(err, listings.ErrSuspended):
		return api.UpdateListing409JSONResponse{
			ConflictJSONResponse: api.ConflictJSONResponse(apierror.New(apierror.CodeListingSuspended, "This listing is suspended; contact support")),
		}, nil
	case errors.Is(err, listings.ErrInvalidTransition), errors.Is(err, listings.ErrImageInUse):
		return api.UpdateListing409JSONResponse{
			ConflictJSONResponse: api.ConflictJSONResponse(apierror.New(apierror.CodeInvalidTransition, "That action is not allowed in this state")),
		}, nil
	}
	var verr *validation.Error
	switch {
	case errors.As(err, &verr):
		return api.UpdateListing400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(validationFailed(verr)),
		}, nil
	case err != nil:
		return nil, err
	}
	return api.UpdateListing200JSONResponse(toSellerListing(view)), nil
}

// DeleteListing removes a draft listing.
func (s Server) DeleteListing(ctx context.Context, req api.DeleteListingRequestObject) (api.DeleteListingResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	err := s.Listings.Delete(requestContext(ctx), u.ID, req.Id)
	switch {
	case errors.Is(err, listings.ErrNotFound):
		return api.DeleteListing404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse(apierror.New(apierror.CodeNotFound, "Listing not found")),
		}, nil
	case errors.Is(err, listings.ErrForbidden):
		return api.DeleteListing403JSONResponse{
			ForbiddenJSONResponse: api.ForbiddenJSONResponse(apierror.New(apierror.CodeForbidden, "You do not have access to this resource")),
		}, nil
	case errors.Is(err, listings.ErrInvalidTransition), errors.Is(err, listings.ErrSuspended):
		return api.DeleteListing409JSONResponse{
			ConflictJSONResponse: api.ConflictJSONResponse(apierror.New(apierror.CodeInvalidTransition, "Only a draft can be deleted; archive it instead")),
		}, nil
	case err != nil:
		return nil, err
	}
	return api.DeleteListing204Response{}, nil
}

// PublishListing publishes a draft, archived or expired listing.
func (s Server) PublishListing(ctx context.Context, req api.PublishListingRequestObject) (api.PublishListingResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	view, err := s.Listings.Publish(requestContext(ctx), u.ID, req.Id)
	var verr *validation.Error
	switch {
	case err == nil:
		return api.PublishListing200JSONResponse(toSellerListing(view)), nil
	case errors.Is(err, listings.ErrNotFound):
		return api.PublishListing404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse(apierror.New(apierror.CodeNotFound, "Listing not found")),
		}, nil
	case errors.Is(err, listings.ErrForbidden):
		return api.PublishListing403JSONResponse{
			ForbiddenJSONResponse: api.ForbiddenJSONResponse(apierror.New(apierror.CodeForbidden, "You do not have access to this resource")),
		}, nil
	case errors.Is(err, listings.ErrInvalidTransition), errors.Is(err, listings.ErrSuspended):
		return api.PublishListing409JSONResponse{
			ConflictJSONResponse: api.ConflictJSONResponse(apierror.New(apierror.CodeInvalidTransition, "That action is not allowed in this state")),
		}, nil
	case errors.As(err, &verr):
		return api.PublishListing400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(validationFailed(verr)),
		}, nil
	default:
		return nil, err
	}
}

// RenewListing extends an active or expired listing.
func (s Server) RenewListing(ctx context.Context, req api.RenewListingRequestObject) (api.RenewListingResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	view, err := s.Listings.Renew(requestContext(ctx), u.ID, req.Id)
	switch {
	case err == nil:
		return api.RenewListing200JSONResponse(toSellerListing(view)), nil
	case errors.Is(err, listings.ErrNotFound):
		return api.RenewListing404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse(apierror.New(apierror.CodeNotFound, "Listing not found")),
		}, nil
	case errors.Is(err, listings.ErrForbidden):
		return api.RenewListing403JSONResponse{
			ForbiddenJSONResponse: api.ForbiddenJSONResponse(apierror.New(apierror.CodeForbidden, "You do not have access to this resource")),
		}, nil
	case errors.Is(err, listings.ErrInvalidTransition), errors.Is(err, listings.ErrSuspended):
		return api.RenewListing409JSONResponse{
			ConflictJSONResponse: api.ConflictJSONResponse(apierror.New(apierror.CodeInvalidTransition, "That action is not allowed in this state")),
		}, nil
	default:
		return nil, err
	}
}

// MarkListingSold marks an active listing sold.
func (s Server) MarkListingSold(ctx context.Context, req api.MarkListingSoldRequestObject) (api.MarkListingSoldResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	view, err := s.Listings.MarkSold(requestContext(ctx), u.ID, req.Id)
	switch {
	case err == nil:
		return api.MarkListingSold200JSONResponse(toSellerListing(view)), nil
	case errors.Is(err, listings.ErrNotFound):
		return api.MarkListingSold404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse(apierror.New(apierror.CodeNotFound, "Listing not found")),
		}, nil
	case errors.Is(err, listings.ErrForbidden):
		return api.MarkListingSold403JSONResponse{
			ForbiddenJSONResponse: api.ForbiddenJSONResponse(apierror.New(apierror.CodeForbidden, "You do not have access to this resource")),
		}, nil
	case errors.Is(err, listings.ErrInvalidTransition), errors.Is(err, listings.ErrSuspended):
		return api.MarkListingSold409JSONResponse{
			ConflictJSONResponse: api.ConflictJSONResponse(apierror.New(apierror.CodeInvalidTransition, "That action is not allowed in this state")),
		}, nil
	default:
		return nil, err
	}
}

// ArchiveListing hides an active or expired listing.
func (s Server) ArchiveListing(ctx context.Context, req api.ArchiveListingRequestObject) (api.ArchiveListingResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	view, err := s.Listings.Archive(requestContext(ctx), u.ID, req.Id)
	switch {
	case err == nil:
		return api.ArchiveListing200JSONResponse(toSellerListing(view)), nil
	case errors.Is(err, listings.ErrNotFound):
		return api.ArchiveListing404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse(apierror.New(apierror.CodeNotFound, "Listing not found")),
		}, nil
	case errors.Is(err, listings.ErrForbidden):
		return api.ArchiveListing403JSONResponse{
			ForbiddenJSONResponse: api.ForbiddenJSONResponse(apierror.New(apierror.CodeForbidden, "You do not have access to this resource")),
		}, nil
	case errors.Is(err, listings.ErrInvalidTransition), errors.Is(err, listings.ErrSuspended):
		return api.ArchiveListing409JSONResponse{
			ConflictJSONResponse: api.ConflictJSONResponse(apierror.New(apierror.CodeInvalidTransition, "That action is not allowed in this state")),
		}, nil
	default:
		return nil, err
	}
}

func toListingInput(body api.CreateListingJSONRequestBody) listings.Input {
	in := listings.Input{
		CategorySlug: body.CategorySlug, Title: body.Title, Description: body.Description,
		PricePesewas: body.Price.Amount, Unit: body.Unit, QuantityAvailable: body.QuantityAvailable,
		MinOrderQty: 1, IsNegotiable: true, ItemState: body.ItemState,
		Region: string(body.Region), District: body.District, Area: body.Area,
		Delivery: listings.Delivery{
			Pickup:         body.DeliveryOptions.Pickup,
			SellerDelivery: body.DeliveryOptions.SellerDelivery,
			FeePesewas:     moneyAmount(body.DeliveryOptions.SellerDeliveryFee),
		},
	}
	if body.MinOrderQty != nil {
		in.MinOrderQty = *body.MinOrderQty
	}
	if body.IsNegotiable != nil {
		in.IsNegotiable = *body.IsNegotiable
	}
	if body.Attributes != nil {
		in.Attributes = *body.Attributes
	}
	if body.ImageMediaIds != nil {
		in.ImageMediaIDs = *body.ImageMediaIds
	}
	return in
}

func toListingPatch(body api.UpdateListingJSONRequestBody) listings.Patch {
	patch := listings.Patch{
		CategorySlug: strPtr(body.CategorySlug), Title: body.Title, Description: body.Description,
		Unit: body.Unit, QuantityAvailable: body.QuantityAvailable, MinOrderQty: body.MinOrderQty,
		IsNegotiable: body.IsNegotiable, ItemState: body.ItemState,
		District: body.District, Area: body.Area,
	}
	if body.Region != nil {
		region := string(*body.Region)
		patch.Region = &region
	}
	if body.Price != nil {
		amount := body.Price.Amount
		patch.PricePesewas = &amount
	}
	if body.DeliveryOptions != nil {
		patch.Delivery = &listings.DeliveryPatch{
			Pickup:         &body.DeliveryOptions.Pickup,
			SellerDelivery: &body.DeliveryOptions.SellerDelivery,
			FeePesewas:     moneyAmount(body.DeliveryOptions.SellerDeliveryFee),
		}
	}
	if body.Attributes != nil {
		patch.Attributes = *body.Attributes
	}
	if body.ImageMediaIds != nil {
		patch.ImageMediaIDs = *body.ImageMediaIds
	}
	return patch
}

func toSellerListing(v listings.View) api.SellerListing {
	out := api.SellerListing{
		Id: v.ID, CategorySlug: v.CategorySlug, Title: v.Title, Slug: v.Slug,
		Description: v.Description, Price: ghs(v.PricePesewas), Unit: v.Unit,
		QuantityAvailable: v.QuantityAvailable, MinOrderQty: v.MinOrderQty,
		IsNegotiable: v.IsNegotiable, ItemState: v.ItemState,
		Status: api.SellerListingStatus(v.Status),
		Region: v.Region, District: v.District, Area: v.Area,
		OffersPickup: v.OffersPickup, OffersSellerDelivery: v.OffersSellerDelivery,
		SellerDeliveryFee: moneyView(v.SellerDeliveryFeePesewas),
		PublishedAt:       v.PublishedAt, ExpiresAt: v.ExpiresAt,
		ViewCount: v.ViewCount, FavoriteCount: v.FavoriteCount, ContactCount: v.ContactCount,
		CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt,
		Images:     make([]api.SellerListingImage, 0, len(v.Images)),
		Attributes: make([]api.SellerListingAttribute, 0, len(v.Attributes)),
	}
	for _, img := range v.Images {
		out.Images = append(out.Images, api.SellerListingImage{
			MediaId: img.MediaID, Url: img.URL, SortOrder: img.SortOrder,
		})
	}
	for _, a := range v.Attributes {
		attr := api.SellerListingAttribute{Key: a.Key, Value: a.Value}
		if a.Type != "" {
			t := api.SellerListingAttributeType(a.Type)
			attr.Type = &t
		}
		out.Attributes = append(out.Attributes, attr)
	}
	return out
}

func toListingSummary(s listings.Summary) api.SellerListingSummary {
	return api.SellerListingSummary{
		Id: s.ID, Title: s.Title, Slug: s.Slug, CategorySlug: s.CategorySlug,
		Price: ghs(s.PricePesewas), QuantityAvailable: s.QuantityAvailable,
		Status: api.SellerListingSummaryStatus(s.Status), ImageCount: s.ImageCount,
		ViewCount: s.ViewCount, FavoriteCount: s.FavoriteCount, ExpiresAt: s.ExpiresAt,
		CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt,
	}
}

func ghs(pesewas int64) api.Money {
	return api.Money{Amount: pesewas, Currency: api.MoneyCurrencyGHS}
}

func moneyAmount(m *api.Money) *int64 {
	if m == nil {
		return nil
	}
	amount := m.Amount
	return &amount
}

func moneyView(pesewas *int64) *api.Money {
	if pesewas == nil {
		return nil
	}
	money := ghs(*pesewas)
	return &money
}

func strPtr(s *string) *string { return s }
