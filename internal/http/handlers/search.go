package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/gin-gonic/gin"

	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/internal/http/middleware"
	"github.com/dezmymachine/farmish-backend/internal/listings"
	"github.com/dezmymachine/farmish-backend/internal/users"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

// Cache lifetimes for public listing reads. Deliberately short: prices and
// stock move, and a stale page costs a buyer money.
const (
	searchCacheControl = "public, max-age=30"
	detailCacheControl = "public, max-age=60"
)

// defaultSearchPage and defaultSearchLimit are what the contract's defaults
// say, restated here because the service takes plain ints.
const (
	defaultSearchPage  = 1
	defaultSearchLimit = 20
)

// PublicListingStore is the read side of listings.Service the public handlers
// use, plus the contact reveal.
type PublicListingStore interface {
	Search(ctx context.Context, in listings.SearchInput) (listings.SearchResult, error)
	PublicDetail(ctx context.Context, slug string) (listings.PublicDetail, error)
	Contact(ctx context.Context, callerID, listingID uuid.UUID) (listings.Contact, error)
}

// SearchListings serves public search and browse. Anonymous callers are fine:
// nothing in the response identifies a seller beyond their business name.
func (s Server) SearchListings(ctx context.Context, req api.SearchListingsRequestObject) (api.SearchListingsResponseObject, error) {
	in := listings.SearchInput{Page: defaultSearchPage, Limit: defaultSearchLimit}
	p := req.Params
	in.Q, in.Category, in.District = p.Q, p.Category, p.District
	if p.Region != nil {
		region := string(*p.Region)
		in.Region = &region
	}
	in.MinPrice, in.MaxPrice, in.ItemState = p.MinPrice, p.MaxPrice, p.ItemState
	if p.Sort != nil {
		in.Sort = string(*p.Sort)
	}
	if p.Page != nil {
		in.Page = *p.Page
	}
	if p.Limit != nil {
		in.Limit = *p.Limit
	}

	res, err := s.PublicListings.Search(ctx, in)
	var verr *validation.Error
	switch {
	case err == nil:
	case errors.As(err, &verr):
		return api.SearchListings400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(validationFailed(verr)),
		}, nil
	default:
		return nil, err
	}

	body := api.ListingSearchResult{
		Items: make([]api.ListingSummary, 0, len(res.Items)),
		Meta:  api.PageMeta{Page: res.Page, Limit: res.Limit, Total: res.Total},
	}
	for _, item := range res.Items {
		body.Items = append(body.Items, toPublicSummary(item))
	}

	etag, fresh := weakETag(body, req.Params.IfNoneMatch)
	if fresh {
		return api.SearchListings304Response{Headers: api.NotModifiedResponseHeaders{
			CacheControl: strptr(searchCacheControl), ETag: &etag,
		}}, nil
	}
	return api.SearchListings200JSONResponse{
		Body: body,
		Headers: api.SearchListings200ResponseHeaders{
			CacheControl: strptr(searchCacheControl), ETag: &etag,
		},
	}, nil
}

// GetPublicListing serves one listing to a buyer. A listing that is not
// browsable is a 404, so a stale link never shows a dead page, and the view is
// counted through a job so the read stays cheap.
func (s Server) GetPublicListing(ctx context.Context, req api.GetPublicListingRequestObject) (api.GetPublicListingResponseObject, error) {
	detail, err := s.PublicListings.PublicDetail(ctx, req.Slug)
	switch {
	case errors.Is(err, listings.ErrNotFound), errors.Is(err, listings.ErrListingNotActive):
		return api.GetPublicListing404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse(apierror.New(apierror.CodeNotFound, "Listing not found")),
		}, nil
	case err != nil:
		return nil, err
	}

	s.countView(ctx, detail.ID)

	body := toListingDetail(detail)
	etag, fresh := weakETag(body, req.Params.IfNoneMatch)
	if fresh {
		return api.GetPublicListing304Response{Headers: api.NotModifiedResponseHeaders{
			CacheControl: strptr(detailCacheControl), ETag: &etag,
		}}, nil
	}
	return api.GetPublicListing200JSONResponse{
		Body: body,
		Headers: api.GetPublicListing200ResponseHeaders{
			CacheControl: strptr(detailCacheControl), ETag: &etag,
		},
	}, nil
}

// RevealListingContact shows a seller's contact details, and only the fields
// they opted into. A seller asking about their own listing is a 403.
func (s Server) RevealListingContact(ctx context.Context, req api.RevealListingContactRequestObject) (api.RevealListingContactResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	contact, err := s.PublicListings.Contact(ctx, u.ID, req.Id)
	switch {
	case errors.Is(err, listings.ErrOwnListing):
		return api.RevealListingContact403JSONResponse{
			ForbiddenJSONResponse: api.ForbiddenJSONResponse(
				apierror.New(apierror.CodeForbidden, "This is your own listing")),
		}, nil
	case errors.Is(err, listings.ErrNotFound), errors.Is(err, listings.ErrListingNotActive):
		return api.RevealListingContact404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse(apierror.New(apierror.CodeNotFound, "Listing not found")),
		}, nil
	case err != nil:
		return nil, err
	}
	// Absent fields are omitted, so a seller who shares neither yields {}.
	body := api.ListingContact{}
	if contact.Phone != nil {
		body.Phone = contact.Phone
	}
	if contact.Whatsapp != nil {
		body.Whatsapp = contact.Whatsapp
	}
	return api.RevealListingContact200JSONResponse(body), nil
}

// countView enqueues the view count. It is best effort on purpose: a queue
// problem must not turn a cached read into a 500, and the next request will
// enqueue again.
func (s Server) countView(ctx context.Context, listingID uuid.UUID) {
	if s.Views == nil {
		return
	}
	ginCtx, ok := ctx.(*gin.Context)
	if !ok {
		return
	}
	viewer := s.ViewerHash(ginCtx, middleware.GetClientIP(ginCtx).String())
	if err := s.Views.CountView(ctx, listingID, viewer); err != nil {
		s.Log.Warn("enqueue view count failed",
			slog.String("listing_id", listingID.String()), slog.String("error", err.Error()))
	}
}

// NewViewerHasher returns the viewer hash used to count a read once per hour.
// The address is HMAC'd with the deployment's data key and a domain-separating
// prefix, so the digest cannot be brute-forced back to an IP and is not
// interchangeable with anything else made from the same key. The raw address
// never leaves this function, and it is never logged.
func NewViewerHasher(key []byte) func(ctx context.Context, ip string) uuid.UUID {
	return func(_ context.Context, ip string) uuid.UUID {
		if ip == "" {
			return uuid.Nil
		}
		mac := hmac.New(sha256.New, key)
		fmt.Fprintf(mac, "view:%s", ip)
		var out uuid.UUID
		copy(out[:], mac.Sum(nil)[:16])
		return out
	}
}

// weakETag returns a weak ETag over the exact body bytes the generated
// response sends, and reports whether the client's validator still matches.
// A nil or absent If-None-Match is never a match.
func weakETag(body any, ifNoneMatch *string) (etag string, fresh bool) {
	// These DTOs are plain structs of scalars, pointers and slices, so
	// marshalling cannot fail.
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	etag = `W/"` + hex.EncodeToString(sum[:8]) + `"`
	if ifNoneMatch == nil {
		return etag, false
	}
	return etag, etagMatches(*ifNoneMatch, etag)
}

// etagMatches implements If-None-Match for a GET: weak comparison, so `*` and
// a list of validators both work and the W/ prefix is not significant.
func etagMatches(header, etag string) bool {
	header = strings.TrimSpace(header)
	if header == "" {
		return false
	}
	if header == "*" {
		return true
	}
	want := strings.TrimPrefix(etag, "W/")
	for _, candidate := range strings.Split(header, ",") {
		if strings.TrimPrefix(strings.TrimSpace(candidate), "W/") == want {
			return true
		}
	}
	return false
}

// toListingSummary maps one search row onto the public contract. It is a
// safe projection by construction: there is no field here for a phone, an
// email or a seller's user id.
func toPublicSummary(item listings.PublicSummary) api.ListingSummary {
	out := api.ListingSummary{
		Id:            item.ID,
		Slug:          item.Slug,
		Title:         item.Title,
		Price:         ghs(item.PricePesewas),
		Unit:          item.Unit,
		Region:        api.GhanaRegion(item.Region),
		District:      item.District,
		ItemState:     item.ItemState,
		Category:      api.ListingCategoryRef{Slug: item.Category.Slug, Name: item.Category.Name},
		CoverImageUrl: item.CoverURL,
		Seller:        api.ListingSellerRef{Name: item.Seller.Name, Verified: item.Seller.Verified},
		PublishedAt:   item.PublishedAt,
	}
	if item.Promo != nil {
		out.Promoted = &api.ListingPromotion{Tier: api.ListingPromotionTier(item.Promo.Tier)}
	}
	return out
}

// derefTime flattens an optional timestamp: a browsable listing always has an
// expiry (the service refuses one without), so the zero value is unreachable.
func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// toListingDetail maps the public listing page. seller is the Phase 8
// PublicSeller projection, which carries a user id and nothing else private.
func toListingDetail(d listings.PublicDetail) api.ListingDetail {
	delivery := api.ListingDeliveryOptions{
		Pickup: d.OffersPickup, SellerDelivery: d.OffersSellerDelivery,
	}
	if d.SellerDeliveryFee != nil {
		fee := ghs(*d.SellerDeliveryFee)
		delivery.Fee = &fee
	}
	images := make([]api.ListingImage, 0, len(d.Images))
	for _, img := range d.Images {
		images = append(images, api.ListingImage{Url: img.URL, Order: img.Order})
	}
	attributes := make([]api.ListingAttribute, 0, len(d.Attributes))
	for _, attr := range d.Attributes {
		attributes = append(attributes, api.ListingAttribute{
			Key: attr.Key, Label: attr.Label, Value: attr.Value,
		})
	}
	out := api.ListingDetail{
		Id: d.ID, Slug: d.Slug, Title: d.Title, Price: ghs(d.PricePesewas),
		Unit: d.Unit, Region: api.GhanaRegion(d.Region), District: d.District,
		ItemState:     d.ItemState,
		Category:      api.ListingCategoryRef{Slug: d.Category.Slug, Name: d.Category.Name},
		CoverImageUrl: d.CoverURL,
		Seller: api.PublicSeller{
			UserId: d.SellerPublic.UserID, BusinessName: d.SellerPublic.Name,
			Region: d.SellerPublic.Region, District: d.SellerPublic.District,
			Bio: d.SellerPublic.Bio, Verified: d.SellerPublic.Verified,
			MemberSince: d.SellerPublic.MemberSince,
		},
		PublishedAt: d.PublishedAt, Description: d.Description,
		QuantityAvailable: d.QuantityAvailable, MinOrderQty: d.MinOrderQty,
		IsNegotiable: d.IsNegotiable, Area: d.Area, DeliveryOptions: delivery,
		Images: images, Attributes: attributes, ExpiresAt: derefTime(d.ExpiresAt),
		FavoriteCount: d.FavoriteCount,
	}
	if d.Promo != nil {
		out.Promoted = &api.ListingPromotion{Tier: api.ListingPromotionTier(d.Promo.Tier)}
	}
	return out
}
