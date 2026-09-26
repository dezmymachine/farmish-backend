package handlers

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/internal/listings"
	"github.com/dezmymachine/farmish-backend/internal/payments"
	"github.com/dezmymachine/farmish-backend/internal/promotions"
	"github.com/dezmymachine/farmish-backend/internal/users"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

// promotionCacheControl is the public package list's lifetime.
const promotionCacheControl = "public, max-age=300"

// PromotionStore is the part of promotions.Service these handlers use.
type PromotionStore interface {
	Configs(ctx context.Context) ([]promotions.Config, error)
	Purchase(ctx context.Context, buyerID uuid.UUID, email, tier string) (promotions.Purchase, error)
	Credits(ctx context.Context, userID uuid.UUID) (int32, error)
	Apply(ctx context.Context, sellerID uuid.UUID, in promotions.ApplyInput) (promotions.Application, error)
	Applications(ctx context.Context, sellerID, listingID uuid.UUID) ([]promotions.Application, error)
}

// ListPromotionConfigs serves the public package list.
func (s Server) ListPromotionConfigs(ctx context.Context, _ api.ListPromotionConfigsRequestObject) (api.ListPromotionConfigsResponseObject, error) {
	configs, err := s.Promotions.Configs(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]api.PromotionConfig, 0, len(configs))
	for _, config := range configs {
		items = append(items, toPromotionConfig(config))
	}
	return api.ListPromotionConfigs200JSONResponse{
		Body:    api.PromotionConfigList{Items: items},
		Headers: api.ListPromotionConfigs200ResponseHeaders{CacheControl: strptr(promotionCacheControl)},
	}, nil
}

// PurchasePromotion starts a promotion-package charge for the caller.
func (s Server) PurchasePromotion(ctx context.Context, req api.PurchasePromotionRequestObject) (api.PurchasePromotionResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	if req.Body == nil {
		return api.PurchasePromotion400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(apierror.New(apierror.CodeBadRequest, "Malformed request")),
		}, nil
	}
	email := ""
	if u.Email != nil {
		email = *u.Email
	}
	purchase, err := s.Promotions.Purchase(ctx, u.ID, email, string(req.Body.Tier))
	var verr *validation.Error
	switch {
	case err == nil:
		return api.PurchasePromotion201JSONResponse(toPromotionPurchase(purchase)), nil
	case errors.As(err, &verr):
		return api.PurchasePromotion400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(validationFailed(verr)),
		}, nil
	case errors.Is(err, promotions.ErrTierNotFound):
		return api.PurchasePromotion404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse(apierror.New(apierror.CodeNotFound, "Promotion tier not found")),
		}, nil
	case errors.Is(err, payments.ErrProviderUnavailable), errors.Is(err, payments.ErrRejected):
		return api.PurchasePromotion502JSONResponse{
			PaymentProviderErrorJSONResponse: api.PaymentProviderErrorJSONResponse(
				apierror.New(apierror.CodePaymentProvider, "The payment provider is unavailable; try again")),
		}, nil
	default:
		return nil, err
	}
}

// GetMyPromotionCredits serves the caller's CRD balance.
func (s Server) GetMyPromotionCredits(ctx context.Context, _ api.GetMyPromotionCreditsRequestObject) (api.GetMyPromotionCreditsResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	balance, err := s.Promotions.Credits(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	return api.GetMyPromotionCredits200JSONResponse(api.PromotionBalance{Balance: balance}), nil
}

// ApplyPromotion spends a tier's credits on one of the caller's own active
// listings.
func (s Server) ApplyPromotion(ctx context.Context, req api.ApplyPromotionRequestObject) (api.ApplyPromotionResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	if req.Body == nil {
		return api.ApplyPromotion400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(apierror.New(apierror.CodeBadRequest, "Malformed request")),
		}, nil
	}
	application, err := s.Promotions.Apply(ctx, u.ID, promotions.ApplyInput{
		ListingID: req.Body.ListingId, Tier: string(req.Body.Tier),
	})
	var verr *validation.Error
	switch {
	case err == nil:
		return api.ApplyPromotion201JSONResponse(toPromotionApplication(application)), nil
	case errors.As(err, &verr):
		return api.ApplyPromotion400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(validationFailed(verr)),
		}, nil
	case errors.Is(err, listings.ErrForbidden):
		return api.ApplyPromotion403JSONResponse{
			ForbiddenJSONResponse: api.ForbiddenJSONResponse(
				apierror.New(apierror.CodeForbidden, "Only the listing owner can promote it")),
		}, nil
	case errors.Is(err, listings.ErrNotFound), errors.Is(err, promotions.ErrTierNotFound):
		return api.ApplyPromotion404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse(apierror.New(apierror.CodeNotFound, "Listing or promotion tier not found")),
		}, nil
	case errors.Is(err, promotions.ErrInsufficientCredits),
		errors.Is(err, promotions.ErrPromotionDowngrade),
		errors.Is(err, listings.ErrListingNotActive):
		return api.ApplyPromotion409JSONResponse{
			ConflictJSONResponse: api.ConflictJSONResponse(promotionConflict(err)),
		}, nil
	default:
		return nil, err
	}
}

// ListMyListingPromotions serves one owned listing's promotion history.
func (s Server) ListMyListingPromotions(ctx context.Context, req api.ListMyListingPromotionsRequestObject) (api.ListMyListingPromotionsResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	applications, err := s.Promotions.Applications(ctx, u.ID, req.Id)
	switch {
	case err == nil:
		items := make([]api.PromotionApplication, 0, len(applications))
		for _, application := range applications {
			items = append(items, toPromotionApplication(application))
		}
		return api.ListMyListingPromotions200JSONResponse(api.PromotionApplicationList{Items: items}), nil
	case errors.Is(err, listings.ErrForbidden):
		return api.ListMyListingPromotions403JSONResponse{
			ForbiddenJSONResponse: api.ForbiddenJSONResponse(
				apierror.New(apierror.CodeForbidden, "Only the listing owner can see its promotions")),
		}, nil
	case errors.Is(err, listings.ErrNotFound):
		return api.ListMyListingPromotions404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse(apierror.New(apierror.CodeNotFound, "Listing not found")),
		}, nil
	default:
		return nil, err
	}
}

func toPromotionConfig(config promotions.Config) api.PromotionConfig {
	return api.PromotionConfig{
		Tier: api.PromotionTier(config.Tier), Name: config.Name, Price: ghs(config.PricePesewas),
		Credits: config.Credits, DurationDays: config.DurationDays, Featured: config.Featured,
		Description: config.Description, Features: config.Features,
	}
}

func toPromotionPurchase(purchase promotions.Purchase) api.PromotionPurchase {
	return api.PromotionPurchase{
		Reference: purchase.Payment.Reference,
		// The service guarantees a URL; the pointer is dereferenced here so a
		// missing URL is a loud programming error rather than a blank checkout.
		AuthorizationUrl: *purchase.Payment.AuthorizationURL,
		Price:            ghs(purchase.Payment.Base),
		ProcessingFee:    ghs(purchase.Payment.Fee),
		Charge:           ghs(purchase.Payment.Charge),
		Credits:          purchase.Tier.Credits,
	}
}

func toPromotionApplication(application promotions.Application) api.PromotionApplication {
	out := api.PromotionApplication{
		Id: application.ID, ListingId: application.ListingID, Tier: api.PromotionTier(application.Tier),
		StartsAt: application.StartsAt, EndsAt: application.EndsAt, CreditsSpent: application.CreditsSpent,
	}
	if application.ReplacedRemaining {
		out.ReplacedRemaining = &application.ReplacedRemaining
	}
	return out
}

// promotionConflict keeps the three apply-time business conflicts distinct in
// prose while sharing the contract's generic 409 envelope.
func promotionConflict(err error) api.Error {
	switch {
	case errors.Is(err, promotions.ErrInsufficientCredits):
		return apierror.New(apierror.CodeConflict, "Not enough promotion credits")
	case errors.Is(err, promotions.ErrPromotionDowngrade):
		return apierror.New(apierror.CodeConflict, "A lower tier cannot replace the active tier")
	default:
		return apierror.New(apierror.CodeConflict, "Only an active listing can be promoted")
	}
}
