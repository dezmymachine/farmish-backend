package listings

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dezmymachine/farmish-backend/internal/db"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

// Sort orders for the public search.
const (
	SortRelevance = "relevance"
	SortNewest    = "newest"
	SortPriceAsc  = "price_asc"
	SortPriceDesc = "price_desc"
)

// MaxPageSize is the API's per-page limit.
const MaxPageSize = 50

var (
	// ErrListingNotActive means the listing exists but is not browsable
	// (draft, sold, archived, suspended or expired).
	ErrListingNotActive = errors.New("listing is not active")
	// ErrOwnListing means a seller tried to reveal their own contact details.
	ErrOwnListing = errors.New("this is your own listing")
	// ErrNoContact means the seller has not opted into contact details.
	ErrNoContact = errors.New("seller has not shared contact details")
)

// SearchInput is one public search request. Nil filters are ignored.
type SearchInput struct {
	Q         *string
	Category  *string
	Region    *string
	District  *string
	MinPrice  *int64
	MaxPrice  *int64
	ItemState *string
	Sort      string
	Page      int32
	Limit     int32
}

// CategoryRef names a category in a response.
type CategoryRef struct {
	Slug string
	Name string
}

// SellerRef is the public seller summary inside a listing (never contact data).
type SellerRef struct {
	Name     string
	Verified bool
}

// PromoRef is an active promotion on a listing.
type PromoRef struct {
	Tier string
}

// PublicSummary is one row of the public search/browse response.
type PublicSummary struct {
	ID           uuid.UUID
	Slug         string
	Title        string
	PricePesewas int64
	Unit         string
	Region       string
	District     string
	ItemState    string
	Category     CategoryRef
	CoverURL     *string
	Seller       SellerRef
	Promo        *PromoRef
	PublishedAt  time.Time
}

// SearchResult is a page of results plus the total.
type SearchResult struct {
	Items []PublicSummary
	Total int64
	Page  int32
	Limit int32
	// Query is the effective free-text query ("" when none).
	Query string
	// Sort is the effective sort, after the relevance-without-q fallback.
	Sort string
}

// DetailImage is one public image.
type DetailImage struct {
	MediaID uuid.UUID
	URL     string
	Order   int32
}

// PublicDetail is the public listing page: the summary plus the fields only a
// signed-in buyer needs, and a safe seller projection (DOMAIN §7: no phone,
// email or identity data).
type PublicDetail struct {
	PublicSummary
	Description          string
	QuantityAvailable    int32
	MinOrderQty          int32
	IsNegotiable         bool
	Area                 *string
	OffersPickup         bool
	OffersSellerDelivery bool
	SellerDeliveryFee    *int64
	Images               []DetailImage
	Attributes           []PublicAttribute
	ExpiresAt            *time.Time
	FavoriteCount        int32
	// SellerPublic is the Phase 8 PublicSeller projection.
	SellerPublic SellerPublic
	// SellerID is internal: used to refuse a contact reveal on your own
	// listing. It is never rendered in a response.
	SellerID uuid.UUID
}

// PublicAttribute is one attribute value with its label.
type PublicAttribute struct {
	Key   string
	Label string
	Value string
}

// SellerPublic is the safe seller projection (Phase 8).
type SellerPublic struct {
	UserID      uuid.UUID
	Name        string
	Region      string
	District    string
	Bio         *string
	Verified    bool
	MemberSince time.Time
}

// Contact is the contact reveal: only the fields the seller opted into.
type Contact struct {
	Phone    *string
	Whatsapp *string
}

// Search runs a public search over active, unexpired listings. Promoted
// listings come first (DOMAIN §6), then the requested sort.
func (s *Service) Search(ctx context.Context, in SearchInput) (SearchResult, error) {
	var verr validation.Error
	if in.Limit < 1 || in.Limit > MaxPageSize {
		verr.Add("limit", fmt.Sprintf("must be between 1 and %d", MaxPageSize))
	}
	if in.Page < 1 {
		verr.Add("page", "must be at least 1")
	}
	if in.MinPrice != nil && *in.MinPrice < 0 {
		verr.Add("minPrice", "must be 0 or more")
	}
	if in.MaxPrice != nil && *in.MaxPrice < 0 {
		verr.Add("maxPrice", "must be 0 or more")
	}
	if in.MinPrice != nil && in.MaxPrice != nil && *in.MinPrice > *in.MaxPrice {
		verr.Add("minPrice", "must not be greater than maxPrice")
	}
	if err := verr.OrNil(); err != nil {
		return SearchResult{}, err
	}

	sort := in.Sort
	if sort == "" {
		sort = SortNewest
	}
	// Relevance without a query has nothing to rank by: fall back to newest.
	if sort == SortRelevance && (in.Q == nil || *in.Q == "") {
		sort = SortNewest
	}
	switch sort {
	case SortNewest, SortPriceAsc, SortPriceDesc, SortRelevance:
	default:
		var verr validation.Error
		verr.Add("sort", "must be relevance, newest, price_asc or price_desc")
		return SearchResult{}, verr.OrNil()
	}

	var categoryIDs []uuid.UUID
	if in.Category != nil && *in.Category != "" {
		ids, err := s.catalog.IDsForFilter(ctx, *in.Category)
		if err != nil {
			return SearchResult{}, err
		}
		categoryIDs = ids
	}

	now := s.Now()
	params := dbSearchParams(in, categoryIDs, now, sort)
	q := db.New(s.pool)
	rows, err := q.SearchListings(ctx, params)
	if err != nil {
		return SearchResult{}, fmt.Errorf("search listings: %w", err)
	}
	total, err := q.CountSearchListings(ctx, dbCountSearchParams(in, categoryIDs, now))
	if err != nil {
		return SearchResult{}, fmt.Errorf("count search listings: %w", err)
	}

	out := SearchResult{
		Items: make([]PublicSummary, 0, len(rows)), Total: total,
		Page: in.Page, Limit: in.Limit, Sort: sort,
	}
	if in.Q != nil {
		out.Query = *in.Q
	}
	for _, r := range rows {
		item := PublicSummary{
			ID: r.ID, Slug: r.Slug, Title: r.Title, PricePesewas: r.PricePesewas,
			Unit: r.Unit, Region: r.Region, District: r.District, ItemState: r.ItemState,
			Category:    CategoryRef{Slug: r.CategorySlug, Name: r.CategoryName},
			CoverURL:    optURL(s.media.PublicURL(r.CoverKey)),
			Seller:      SellerRef{Name: r.SellerName, Verified: r.SellerVerified},
			PublishedAt: derefTime(r.PublishedAt),
		}
		if r.PromoTier != nil {
			item.Promo = &PromoRef{Tier: *r.PromoTier}
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}

// PublicDetail returns one browsable listing with its images, attributes and
// the safe seller projection. Draft, sold, archived, suspended and expired
// listings are not browsable (ErrListingNotActive).
func (s *Service) PublicDetail(ctx context.Context, slug string) (PublicDetail, error) {
	q := db.New(s.pool)
	now := s.Now()
	row, err := q.GetPublicListingBySlug(ctx, slug)
	if errors.Is(err, pgx.ErrNoRows) {
		return PublicDetail{}, fmt.Errorf("%w: %s", ErrNotFound, slug)
	}
	if err != nil {
		return PublicDetail{}, fmt.Errorf("get public listing: %w", err)
	}
	if row.Status != StatusActive || row.ExpiresAt == nil || !row.ExpiresAt.After(now) {
		return PublicDetail{}, fmt.Errorf("%w: %s", ErrListingNotActive, slug)
	}

	images, err := q.ListListingImages(ctx, row.ID)
	if err != nil {
		return PublicDetail{}, fmt.Errorf("list listing images: %w", err)
	}
	detail := PublicDetail{
		PublicSummary: PublicSummary{
			ID: row.ID, Slug: row.Slug, Title: row.Title, PricePesewas: row.PricePesewas,
			Unit: row.Unit, Region: row.Region, District: row.District, ItemState: row.ItemState,
			Category:    CategoryRef{Slug: row.CategorySlug, Name: row.CategoryName},
			Seller:      SellerRef{Name: row.BusinessName, Verified: row.SellerVerified},
			PublishedAt: derefTime(row.PublishedAt),
		},
		Description:          row.Description,
		QuantityAvailable:    row.QuantityAvailable,
		MinOrderQty:          row.MinOrderQty,
		IsNegotiable:         row.IsNegotiable,
		Area:                 row.Area,
		OffersPickup:         row.OffersPickup,
		OffersSellerDelivery: row.OffersSellerDelivery,
		SellerDeliveryFee:    row.SellerDeliveryFeePesewas,
		ExpiresAt:            row.ExpiresAt,
		FavoriteCount:        row.FavoriteCount,
		SellerID:             row.SellerID,
		Images:               make([]DetailImage, 0, len(images)),
		SellerPublic: SellerPublic{
			UserID: row.SellerID, Name: row.BusinessName, Region: row.SellerRegion,
			District: row.SellerDistrict, Bio: row.SellerBio, Verified: row.SellerVerified,
			MemberSince: row.SellerCreatedAt,
		},
	}
	for _, img := range images {
		image := DetailImage{
			MediaID: img.MediaID, URL: s.media.PublicURL(img.Key), Order: img.SortOrder,
		}
		detail.Images = append(detail.Images, image)
		// The first image is the cover, and its URL is already public.
		if len(detail.Images) == 1 {
			detail.CoverURL = optURL(image.URL)
		}
	}
	values, err := listAttributeValues(ctx, q, row.ID)
	if err != nil {
		return PublicDetail{}, err
	}
	labels, err := s.attributeLabels(ctx, row.CategoryID, values)
	if err != nil {
		return PublicDetail{}, err
	}
	detail.Attributes = make([]PublicAttribute, 0, len(values))
	for _, v := range values {
		detail.Attributes = append(detail.Attributes, PublicAttribute{Key: v.Key, Label: labels[v.Key], Value: v.Value})
	}
	return detail, nil
}

// Contact returns the seller's contact details, but only the fields they
// opted into (DOMAIN §7). Asking about your own listing is refused.
func (s *Service) Contact(ctx context.Context, callerID, listingID uuid.UUID) (Contact, error) {
	q := db.New(s.pool)
	details, err := q.GetListingContactDetails(ctx, listingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Contact{}, fmt.Errorf("%w: %s", ErrNotFound, listingID)
	}
	if err != nil {
		return Contact{}, fmt.Errorf("get contact details: %w", err)
	}
	if details.SellerID == callerID {
		return Contact{}, fmt.Errorf("%w: %s", ErrOwnListing, listingID)
	}
	now := s.Now()
	if details.Status != StatusActive || details.ExpiresAt == nil || !details.ExpiresAt.After(now) {
		return Contact{}, fmt.Errorf("%w: %s", ErrListingNotActive, listingID)
	}
	// show_phone / show_whatsapp decide what leaves the building; nothing
	// else is ever returned.
	phone, whatsapp := details.PhoneE164, details.WhatsappE164
	out := Contact{}
	if details.ShowPhone {
		out.Phone = phone
	}
	if details.ShowWhatsapp {
		out.Whatsapp = whatsapp
	}
	if err := q.IncrementListingContactCount(ctx, listingID); err != nil {
		return Contact{}, fmt.Errorf("increment contact count: %w", err)
	}
	return out, nil
}

// CountView bumps the view counter. It is called by the view job, never in
// the request path.
func (s *Service) CountView(ctx context.Context, listingID uuid.UUID) error {
	if err := db.New(s.pool).IncrementListingViewCount(ctx, listingID); err != nil {
		return fmt.Errorf("increment view count: %w", err)
	}
	return nil
}

// dbSearchParams and dbCountSearchParams build the sqlc arguments. `now`
// always comes from the service clock, never SQL now(), so tests are
// deterministic.
func dbSearchParams(in SearchInput, categoryIDs []uuid.UUID, now time.Time, sort string) db.SearchListingsParams {
	return db.SearchListingsParams{
		Now: &now, Q: in.Q, CategoryIds: categoryIDs, Region: in.Region,
		District: in.District, MinPrice: in.MinPrice, MaxPrice: in.MaxPrice,
		ItemState: in.ItemState, Sort: sort,
		Limit: in.Limit, Offset: (in.Page - 1) * in.Limit,
	}
}

func dbCountSearchParams(in SearchInput, categoryIDs []uuid.UUID, now time.Time) db.CountSearchListingsParams {
	return db.CountSearchListingsParams{
		Now: &now, Q: in.Q, CategoryIds: categoryIDs, Region: in.Region,
		District: in.District, MinPrice: in.MinPrice, MaxPrice: in.MaxPrice,
		ItemState: in.ItemState,
	}
}

// attributeLabels resolves display labels for stored attribute values from
// the category's effective definitions, falling back to the key.
func (s *Service) attributeLabels(ctx context.Context, categoryID uuid.UUID, values []AttributeValue) (map[string]string, error) {
	labels := map[string]string{}
	if len(values) == 0 {
		return labels, nil
	}
	cat, err := s.catalog.Resolved(ctx, categoryID)
	if err != nil {
		return nil, err
	}
	for _, d := range cat.Attributes {
		labels[d.Key] = d.Label
	}
	for _, v := range values {
		if _, ok := labels[v.Key]; !ok {
			labels[v.Key] = v.Key
		}
	}
	return labels, nil
}

// optURL keeps a cover image only when storage can actually serve it.
func optURL(url string) *string {
	if url == "" {
		return nil
	}
	return &url
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}
