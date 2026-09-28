package handlers

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/dezmymachine/farmish-backend/internal/engagement"
	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/internal/listings"
	"github.com/dezmymachine/farmish-backend/internal/users"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

// EngagementStore is the part of engagement.Service these handlers use.
type EngagementStore interface {
	CreateReview(ctx context.Context, buyerID, orderID, listingID uuid.UUID, rating int16, comment string) (engagement.Review, error)
	ListReviews(ctx context.Context, listingID uuid.UUID, limit, offset int32) ([]engagement.Review, int64, engagement.Rating, error)
	SellerRating(ctx context.Context, sellerID uuid.UUID) (engagement.Rating, error)
	HideReview(ctx context.Context, adminID, reviewID uuid.UUID, reason string) (engagement.Review, error)
	AddFavorite(ctx context.Context, userID, listingID uuid.UUID) error
	RemoveFavorite(ctx context.Context, userID, listingID uuid.UUID) error
	ListFavorites(ctx context.Context, userID uuid.UUID, limit, offset int32) ([]engagement.FavoriteListing, int64, error)
	ReviewerName(ctx context.Context, reviewerID uuid.UUID) string
}

// CreateReview records the buyer's rating of a bought listing.
func (s Server) CreateReview(ctx context.Context, req api.CreateReviewRequestObject) (api.CreateReviewResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	if req.Body == nil {
		return api.CreateReview400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(apierror.New(apierror.CodeBadRequest, "Malformed request")),
		}, nil
	}
	comment := ""
	if req.Body.Comment != nil {
		comment = *req.Body.Comment
	}
	// The validator enforces 1–5 before reaching here, so the narrowing is safe.
	rating := int16(req.Body.Rating) //nolint:gosec // G115: validated 1-5 by the contract
	review, err := s.Engagement.CreateReview(ctx, u.ID, req.Id, req.Body.ListingId, rating, comment)
	var verr *validation.Error
	switch {
	case err == nil:
		return api.CreateReview201JSONResponse(toReview(review)), nil
	case errors.As(err, &verr):
		return api.CreateReview400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(validationFailed(verr)),
		}, nil
	case errors.Is(err, engagement.ErrForbidden):
		return api.CreateReview403JSONResponse{
			ForbiddenJSONResponse: api.ForbiddenJSONResponse(apierror.New(apierror.CodeForbidden, "Only the buyer may review this order")),
		}, nil
	case errors.Is(err, engagement.ErrNotFound):
		return api.CreateReview404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse(apierror.New(apierror.CodeNotFound, "Order not found")),
		}, nil
	case errors.Is(err, engagement.ErrOrderNotCompleted):
		return api.CreateReview409JSONResponse{
			ConflictJSONResponse: api.ConflictJSONResponse(apierror.New("order_not_completed", "Only a completed order may be reviewed")),
		}, nil
	case errors.Is(err, engagement.ErrAlreadyReviewed):
		return api.CreateReview409JSONResponse{
			ConflictJSONResponse: api.ConflictJSONResponse(apierror.New("already_reviewed", "This listing was already reviewed in this order")),
		}, nil
	case errors.Is(err, engagement.ErrListingNotInOrder):
		return api.CreateReview400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(apierror.New(apierror.CodeBadRequest, "Listing is not part of this order")),
		}, nil
	default:
		return nil, err
	}
}

// ListListingReviews returns a listing's visible reviews with the seller
// aggregate. The listing check is browsability: inactive reads as missing.
func (s Server) ListListingReviews(ctx context.Context, req api.ListListingReviewsRequestObject) (api.ListListingReviewsResponseObject, error) {
	detail, err := s.PublicListings.PublicDetail(ctx, req.Slug)
	switch {
	case errors.Is(err, listings.ErrNotFound):
		return api.ListListingReviews404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse(apierror.New(apierror.CodeNotFound, "Listing not found")),
		}, nil
	case err != nil:
		return nil, err
	}
	page, limit := pageParams(req.Params.Page, req.Params.Limit)
	items, total, summary, err := s.Engagement.ListReviews(ctx, detail.ID, limit, (page-1)*limit)
	if err != nil {
		return nil, err
	}
	out := make([]api.PublicReview, 0, len(items))
	for _, item := range items {
		out = append(out, api.PublicReview{
			Id: item.ID, Rating: int32(item.Rating), //nolint:gosec // G115: rating is 1-5 by CHECK
			Comment:      item.Comment,
			ReviewerName: s.Engagement.ReviewerName(ctx, item.ReviewerID), CreatedAt: item.CreatedAt,
		})
	}
	return api.ListListingReviews200JSONResponse(api.PublicReviewList{
		Items: out, Meta: api.PageMeta{Page: page, Limit: limit, Total: total},
		Summary: api.SellerRating{Average: float32(summary.Average), Count: summary.Count},
	}), nil
}

// AddFavorite idempotently favourites a listing.
func (s Server) AddFavorite(ctx context.Context, req api.AddFavoriteRequestObject) (api.AddFavoriteResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	if err := s.Engagement.AddFavorite(ctx, u.ID, req.ListingId); err != nil {
		if errors.Is(err, engagement.ErrNotFound) {
			return api.AddFavorite404JSONResponse{
				NotFoundJSONResponse: api.NotFoundJSONResponse(apierror.New(apierror.CodeNotFound, "Listing not found")),
			}, nil
		}
		return nil, err
	}
	return api.AddFavorite204Response{}, nil
}

// RemoveFavorite idempotently unfavourites a listing.
func (s Server) RemoveFavorite(ctx context.Context, req api.RemoveFavoriteRequestObject) (api.RemoveFavoriteResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	if err := s.Engagement.RemoveFavorite(ctx, u.ID, req.ListingId); err != nil {
		return nil, err
	}
	return api.RemoveFavorite204Response{}, nil
}

// ListMyFavorites returns the caller's favourites, inactive flagged.
func (s Server) ListMyFavorites(ctx context.Context, req api.ListMyFavoritesRequestObject) (api.ListMyFavoritesResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	page, limit := pageParams(req.Params.Page, req.Params.Limit)
	items, total, err := s.Engagement.ListFavorites(ctx, u.ID, limit, (page-1)*limit)
	if err != nil {
		return nil, err
	}
	out := make([]api.FavoriteListing, 0, len(items))
	for _, item := range items {
		out = append(out, toFavoriteListing(item.Listing, item.Available))
	}
	return api.ListMyFavorites200JSONResponse(api.FavoriteList{
		Items: out, Meta: api.PageMeta{Page: page, Limit: limit, Total: total},
	}), nil
}

// HideReview hides a review for moderation, audited.
func (s Server) HideReview(ctx context.Context, req api.HideReviewRequestObject) (api.HideReviewResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	if req.Body == nil {
		return api.HideReview400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(apierror.New(apierror.CodeBadRequest, "Malformed request")),
		}, nil
	}
	review, err := s.Engagement.HideReview(ctx, u.ID, req.Id, req.Body.Reason)
	var verr *validation.Error
	switch {
	case err == nil:
		return api.HideReview200JSONResponse(toReview(review)), nil
	case errors.As(err, &verr):
		return api.HideReview400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(validationFailed(verr)),
		}, nil
	case errors.Is(err, engagement.ErrNotFound):
		return api.HideReview404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse(apierror.New(apierror.CodeNotFound, "Review not found")),
		}, nil
	case errors.Is(err, engagement.ErrAlreadyHidden):
		return api.HideReview409JSONResponse{
			ConflictJSONResponse: api.ConflictJSONResponse(apierror.New(apierror.CodeConflict, "Review is already hidden")),
		}, nil
	default:
		return nil, err
	}
}

// ratingOf loads a seller aggregate for response mappers. Routers built
// before engagement existed (older tests) get zeros.
func (s Server) ratingOf(ctx context.Context, sellerID uuid.UUID) (api.SellerRating, error) {
	if s.Engagement == nil {
		return api.SellerRating{}, nil
	}
	rating, err := s.Engagement.SellerRating(ctx, sellerID)
	if err != nil {
		return api.SellerRating{}, err
	}
	return api.SellerRating{Average: float32(rating.Average), Count: rating.Count}, nil //nolint:gosec // G115: one-decimal average fits float32
}

// toReview maps a review onto the contract.
func toReview(review engagement.Review) api.Review {
	return api.Review{
		Id: review.ID, OrderId: review.OrderID, ListingId: review.ListingID,
		// The CHECK constraint holds 1–5, so the widening is safe.
		Rating: int32(review.Rating), Comment: review.Comment, CreatedAt: review.CreatedAt, //nolint:gosec // G115: rating is 1-5 by CHECK
	}
}

// toFavoriteListing maps a favourited summary plus its availability flag.
func toFavoriteListing(item listings.PublicSummary, available bool) api.FavoriteListing {
	summary := toPublicSummary(item)
	return api.FavoriteListing{
		Id: summary.Id, Slug: summary.Slug, Title: summary.Title, Price: summary.Price,
		Unit: summary.Unit, Region: summary.Region, District: summary.District,
		ItemState: summary.ItemState, Category: summary.Category, CoverImageUrl: summary.CoverImageUrl,
		Promoted: summary.Promoted, Seller: summary.Seller, PublishedAt: summary.PublishedAt,
		Available: available,
	}
}
