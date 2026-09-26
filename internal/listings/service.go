package listings

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/catalog"
	"github.com/dezmymachine/farmish-backend/internal/database"
	"github.com/dezmymachine/farmish-backend/internal/db"
	"github.com/dezmymachine/farmish-backend/internal/geo"
	"github.com/dezmymachine/farmish-backend/internal/media"
	"github.com/dezmymachine/farmish-backend/internal/text"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

// Catalog is the part of catalog.Service listings validates against.
type Catalog interface {
	// Detail resolves a category by slug, with its inherited group and the
	// effective attributes.
	Detail(ctx context.Context, slug string) (catalog.Detail, error)
	// Resolved is Detail by id, for a listing's stored category.
	Resolved(ctx context.Context, id uuid.UUID) (catalog.Detail, error)
	// IsLeaf reports whether a category has no children (a listing needs a
	// leaf, or a parent without children such as irrigation).
	IsLeaf(ctx context.Context, id uuid.UUID) (bool, error)
	// IDsForFilter expands a search filter on a category slug to the ids to
	// match: a parent includes its children, a child is itself (DOMAIN §9).
	IDsForFilter(ctx context.Context, slug string) ([]uuid.UUID, error)
}

// Media is the part of media.Service listings attaches images through.
type Media interface {
	Attach(ctx context.Context, tx pgx.Tx, ownerID, mediaID uuid.UUID) (media.Object, error)
	PublicURL(key string) string
}

// Sellers reads whether a user has a seller profile (sellers.Service).
type Sellers interface {
	Exists(ctx context.Context, userID uuid.UUID) (bool, error)
}

// Service owns listings.
type Service struct {
	pool    *pgxpool.Pool
	catalog Catalog
	media   Media
	sellers Sellers
	// Now is the clock, injectable so tests can age listings for the sweep.
	Now func() time.Time
}

// New returns a Service. sellers may be nil in tests whose callers always
// have a profile.
func New(pool *pgxpool.Pool, c Catalog, m Media, s Sellers) *Service {
	return &Service{pool: pool, catalog: c, media: m, sellers: s, Now: time.Now}
}

// Create stores a new listing. publish: true publishes it in the same
// transaction, so an incomplete listing is never visible.
func (s *Service) Create(ctx context.Context, sellerID uuid.UUID, in Input, publish bool) (View, error) {
	if err := s.requireSeller(ctx, sellerID); err != nil {
		return View{}, err
	}
	cat, err := s.validate(ctx, in)
	if err != nil {
		return View{}, err
	}

	var out View
	if err := s.insertWithSlug(ctx, sellerID, in, cat, publish, &out); err != nil {
		return View{}, err
	}
	return out, nil
}

// insertWithSlug retries on a slug collision (another seller won the race),
// so concurrent creates never 500.
func (s *Service) insertWithSlug(ctx context.Context, sellerID uuid.UUID, in Input, cat catalog.Detail, publish bool, out *View) error {
	base := text.Slugify(in.Title)
	if base == "" {
		return fieldError("title", "yields an empty slug")
	}
	for attempt := range SlugCollisionTries + 1 {
		slug := slugFor(base, attempt)
		err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
			q := db.New(tx)
			row, err := q.InsertListing(ctx, db.InsertListingParams{
				SellerID: sellerID, CategoryID: cat.ID, Title: in.Title, Slug: slug,
				Description: in.Description, PricePesewas: in.PricePesewas, Unit: in.Unit,
				QuantityAvailable: in.QuantityAvailable, MinOrderQty: in.MinOrderQty,
				IsNegotiable: in.IsNegotiable, ItemState: in.ItemState,
				Region: in.Region, District: in.District, Area: in.Area,
				OffersPickup: in.Delivery.Pickup, OffersSellerDelivery: in.Delivery.SellerDelivery,
				SellerDeliveryFeePesewas: in.Delivery.FeePesewas,
			})
			if err != nil {
				return fmt.Errorf("insert listing: %w", err)
			}
			if err := s.writeChildren(ctx, tx, q, row, in, cat); err != nil {
				return err
			}
			if publish {
				if err := s.publishTx(ctx, q, row, cat); err != nil {
					return err
				}
				row, err = q.GetListingByID(ctx, row.ID)
				if err != nil {
					return fmt.Errorf("reload listing: %w", err)
				}
			}
			view, err := s.view(ctx, q, row, cat.Slug)
			if err != nil {
				return err
			}
			*out = view
			return nil
		})
		if err == nil {
			return nil
		}
		if isUniqueViolation(err) {
			continue // slug taken: try the next suffix
		}
		return err
	}
	return fmt.Errorf("slug %q is taken after %d tries", base, SlugCollisionTries)
}

// GetOwn returns one of the caller's listings.
func (s *Service) GetOwn(ctx context.Context, sellerID, id uuid.UUID) (View, error) {
	return s.viewByID(ctx, sellerID, id)
}

// ListOwn returns the caller's listings, newest first, with a total.
func (s *Service) ListOwn(ctx context.Context, sellerID uuid.UUID, status string, limit, offset int32) ([]Summary, int64, error) {
	var st *string
	if status != "" {
		st = &status
	}
	q := db.New(s.pool)
	total, err := q.CountSellerListings(ctx, db.CountSellerListingsParams{SellerID: sellerID, Status: st})
	if err != nil {
		return nil, 0, fmt.Errorf("count listings: %w", err)
	}
	rows, err := q.ListSellerListings(ctx, db.ListSellerListingsParams{
		SellerID: sellerID, Status: st, Limit: limit, Offset: offset,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("list listings: %w", err)
	}
	out := make([]Summary, 0, len(rows))
	for _, r := range rows {
		images, err := q.CountListingImages(ctx, r.ID)
		if err != nil {
			return nil, 0, fmt.Errorf("count listing images: %w", err)
		}
		if images > math.MaxInt32 {
			return nil, 0, errors.New("too many images on a listing")
		}
		cat, err := s.catalog.Resolved(ctx, r.CategoryID)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, Summary{
			ID: r.ID, Title: r.Title, Slug: r.Slug, CategorySlug: cat.Slug,
			PricePesewas: r.PricePesewas, QuantityAvailable: r.QuantityAvailable,
			Status: r.Status, ImageCount: int32(images), ViewCount: r.ViewCount, //nolint:gosec // bounded above
			FavoriteCount: r.FavoriteCount, ExpiresAt: r.ExpiresAt,
			CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		})
	}
	return out, total, nil
}

// Update edits a listing the caller owns. A nil patch field keeps the stored
// value; the merged result is validated as a whole. Changing the category
// re-validates the attributes and drops the ones the new category does not
// define. An active listing stays active.
func (s *Service) Update(ctx context.Context, sellerID, id uuid.UUID, patch Patch) (View, error) {
	var out View
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		row, err := ownedListing(ctx, q, sellerID, id)
		if err != nil {
			return err
		}
		if row.Status == StatusSuspended {
			return ErrSuspended
		}
		cur, err := s.catalog.Resolved(ctx, row.CategoryID)
		if err != nil {
			return err
		}
		merged := applyPatch(row, patch, cur.Slug)
		// A patch may move the listing to another category; that category's
		// group and attributes govern from then on, so re-validate.
		cat := cur
		if merged.CategorySlug != cur.Slug {
			if cat, err = s.category(ctx, merged.CategorySlug); err != nil {
				return err
			}
			merged.CategorySlug = cat.Slug
		}
		if verr := s.validateFields(ctx, merged, cat); verr != nil {
			return verr
		}
		updated, err := q.UpdateListing(ctx, db.UpdateListingParams{
			CategoryID: pgUUID(cat.ID), Title: &merged.Title, Description: &merged.Description,
			PricePesewas: &merged.PricePesewas, Unit: &merged.Unit,
			QuantityAvailable: &merged.QuantityAvailable, MinOrderQty: &merged.MinOrderQty,
			IsNegotiable: &merged.IsNegotiable, ItemState: &merged.ItemState,
			Region: &merged.Region, District: &merged.District, Area: merged.Area,
			OffersPickup: &merged.Delivery.Pickup, OffersSellerDelivery: &merged.Delivery.SellerDelivery,
			SellerDeliveryFeePesewas: merged.Delivery.FeePesewas, ID: id,
		})
		if err != nil {
			return fmt.Errorf("update listing: %w", err)
		}
		// Attributes are replaced whenever they are supplied or the category
		// changed, so another category's values never survive.
		if patch.Attributes != nil || patch.CategorySlug != nil {
			if err := s.replaceAttributes(ctx, q, id, merged.Attributes, cat); err != nil {
				return err
			}
		}
		if patch.ImageMediaIDs != nil {
			if err := s.replaceImages(ctx, tx, q, sellerID, id, merged.ImageMediaIDs); err != nil {
				return err
			}
		}
		view, err := s.view(ctx, q, updated, cat.Slug)
		if err != nil {
			return err
		}
		out = view
		return nil
	})
	if err != nil {
		return View{}, err
	}
	return out, nil
}

// Publish moves a draft, archived or expired listing to active. It must be
// complete: at least one image and every required attribute.
func (s *Service) Publish(ctx context.Context, sellerID, id uuid.UUID) (View, error) {
	return s.transition(ctx, sellerID, id, "publish", func(ctx context.Context, q *db.Queries, row db.Listing, cat catalog.Detail) error {
		return s.publishTx(ctx, q, row, cat)
	})
}

// Renew extends an active (or expired) listing by another 30 days.
func (s *Service) Renew(ctx context.Context, sellerID, id uuid.UUID) (View, error) {
	return s.transition(ctx, sellerID, id, "renew", func(_ context.Context, q *db.Queries, _ db.Listing, _ catalog.Detail) error {
		expiresAt := s.Now().Add(ExpiryWindow)
		_, err := q.SetListingStatus(ctx, db.SetListingStatusParams{ID: id, Status: StatusActive, ExpiresAt: &expiresAt})
		return err
	})
}

// MarkSold marks an active listing sold.
func (s *Service) MarkSold(ctx context.Context, sellerID, id uuid.UUID) (View, error) {
	return s.transition(ctx, sellerID, id, "sold", func(_ context.Context, q *db.Queries, _ db.Listing, _ catalog.Detail) error {
		_, err := q.SetListingStatus(ctx, db.SetListingStatusParams{ID: id, Status: StatusSold})
		return err
	})
}

// Archive hides an active or expired listing. Renew or publish brings it back.
func (s *Service) Archive(ctx context.Context, sellerID, id uuid.UUID) (View, error) {
	return s.transition(ctx, sellerID, id, "archive", func(_ context.Context, q *db.Queries, _ db.Listing, _ catalog.Detail) error {
		_, err := q.SetListingStatus(ctx, db.SetListingStatusParams{ID: id, Status: StatusArchived})
		return err
	})
}

// Delete removes a draft. Anything else must be archived, so its history and
// counters survive.
func (s *Service) Delete(ctx context.Context, sellerID, id uuid.UUID) error {
	return database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		row, err := ownedListing(ctx, q, sellerID, id)
		if err != nil {
			return err
		}
		if row.Status == StatusSuspended {
			return ErrSuspended
		}
		if row.Status != StatusDraft {
			return fmt.Errorf("%w: only drafts can be deleted (status %s)", ErrInvalidTransition, row.Status)
		}
		if _, err := q.DeleteListing(ctx, id); err != nil {
			return fmt.Errorf("delete listing: %w", err)
		}
		return nil
	})
}

// ExpireDue flips active listings past their expiry to expired and returns
// how many it changed.
func (s *Service) ExpireDue(ctx context.Context) (int, error) {
	now := s.Now()
	rows, err := db.New(s.pool).ExpireDueListings(ctx, &now)
	if err != nil {
		return 0, fmt.Errorf("expire due listings: %w", err)
	}
	return len(rows), nil
}

// transition runs one owner-driven status change under a row lock.
func (s *Service) transition(ctx context.Context, sellerID, id uuid.UUID, action string,
	apply func(context.Context, *db.Queries, db.Listing, catalog.Detail) error,
) (View, error) {
	var out View
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		row, err := ownedListing(ctx, q, sellerID, id)
		if err != nil {
			return err
		}
		if row.Status == StatusSuspended {
			return ErrSuspended
		}
		if !allowedTransitions[action][row.Status] {
			return fmt.Errorf("%w: cannot %s a %s listing", ErrInvalidTransition, action, row.Status)
		}
		cat, err := s.catalog.Resolved(ctx, row.CategoryID)
		if err != nil {
			return err
		}
		if err := apply(ctx, q, row, cat); err != nil {
			return err
		}
		updated, err := q.GetListingByID(ctx, id)
		if err != nil {
			return fmt.Errorf("reload listing: %w", err)
		}
		view, err := s.view(ctx, q, updated, cat.Slug)
		if err != nil {
			return err
		}
		out = view
		return nil
	})
	if err != nil {
		return View{}, err
	}
	return out, nil
}

// publishTx sets the publish timestamps. Completeness (an image and every
// required attribute) is enforced here, so Create(publish) and Publish agree.
func (s *Service) publishTx(ctx context.Context, q *db.Queries, row db.Listing, cat catalog.Detail) error {
	images, err := q.CountListingImages(ctx, row.ID)
	if err != nil {
		return fmt.Errorf("count listing images: %w", err)
	}
	if images == 0 {
		return fieldError("imageMediaIds", "at least one image is required to publish")
	}
	values, err := listAttributeValues(ctx, q, row.ID)
	if err != nil {
		return err
	}
	present := map[string]string{}
	for _, v := range values {
		present[v.Key] = v.Value
	}
	for _, def := range cat.Attributes {
		if def.Required && strings.TrimSpace(present[def.Key]) == "" {
			return fieldError("attributes/"+def.Key, "is required to publish")
		}
	}
	now := s.Now()
	published := row.PublishedAt
	if published == nil {
		published = &now
	}
	expiresAt := now.Add(ExpiryWindow)
	_, err = q.SetListingStatus(ctx, db.SetListingStatusParams{
		ID: row.ID, Status: StatusActive, PublishedAt: published, ExpiresAt: &expiresAt,
	})
	return err
}

// RequireAdvertisable locks the caller's listing inside tx and refuses one
// that cannot be promoted. Unknown listings and other sellers' listings keep
// their existing 404/403 errors; anything that is not active and unexpired is
// a promotion-specific 409 in the calling package.
func (s *Service) RequireAdvertisable(ctx context.Context, tx pgx.Tx, sellerID, id uuid.UUID, now time.Time) error {
	row, err := ownedListing(ctx, db.New(tx), sellerID, id)
	if err != nil {
		return err
	}
	if row.Status != StatusActive || row.ExpiresAt == nil || !row.ExpiresAt.After(now) {
		return fmt.Errorf("%w: %s", ErrListingNotActive, id)
	}
	return nil
}

// ownedListing loads a listing FOR UPDATE and checks the seller.
func ownedListing(ctx context.Context, q *db.Queries, sellerID, id uuid.UUID) (db.Listing, error) {
	row, err := q.GetListingByIDForUpdate(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Listing{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return db.Listing{}, fmt.Errorf("get listing: %w", err)
	}
	if row.SellerID != sellerID {
		return db.Listing{}, fmt.Errorf("%w: %s", ErrForbidden, id)
	}
	return row, nil
}

// viewByID loads a listing the caller owns, with images and attributes.
func (s *Service) viewByID(ctx context.Context, sellerID, id uuid.UUID) (View, error) {
	q := db.New(s.pool)
	row, err := q.GetListingByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return View{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return View{}, fmt.Errorf("get listing: %w", err)
	}
	if row.SellerID != sellerID {
		return View{}, fmt.Errorf("%w: %s", ErrForbidden, id)
	}
	cat, err := s.catalog.Resolved(ctx, row.CategoryID)
	if err != nil {
		return View{}, err
	}
	return s.view(ctx, q, row, cat.Slug)
}

// view assembles the seller's own listing view.
func (s *Service) view(ctx context.Context, q *db.Queries, row db.Listing, categorySlug string) (View, error) {
	images, err := q.ListListingImages(ctx, row.ID)
	if err != nil {
		return View{}, fmt.Errorf("list listing images: %w", err)
	}
	out := View{Listing: fromRow(row), CategorySlug: categorySlug}
	for _, img := range images {
		out.Images = append(out.Images, Image{
			MediaID: img.MediaID, URL: s.media.PublicURL(img.Key), SortOrder: img.SortOrder,
		})
	}
	if out.Images == nil {
		out.Images = []Image{}
	}
	values, err := listAttributeValues(ctx, q, row.ID)
	if err != nil {
		return View{}, err
	}
	if values == nil {
		values = []AttributeValue{}
	}
	out.Attributes = values
	return out, nil
}

func listAttributeValues(ctx context.Context, q *db.Queries, listingID uuid.UUID) ([]AttributeValue, error) {
	rows, err := q.ListListingAttributes(ctx, listingID)
	if err != nil {
		return nil, fmt.Errorf("list listing attributes: %w", err)
	}
	out := make([]AttributeValue, 0, len(rows))
	for _, r := range rows {
		out = append(out, AttributeValue{Key: r.Key, Value: r.Value, Type: r.Type, Required: r.Required})
	}
	return out, nil
}

// writeChildren attaches the images and stores the attributes of a new row.
func (s *Service) writeChildren(ctx context.Context, tx pgx.Tx, q *db.Queries, row db.Listing, in Input, cat catalog.Detail) error {
	if err := s.replaceImages(ctx, tx, q, row.SellerID, row.ID, in.ImageMediaIDs); err != nil {
		return err
	}
	return s.replaceAttributes(ctx, q, row.ID, in.Attributes, cat)
}

// replaceImages attaches each media object to the listing. The media row
// becomes attached inside this transaction, so a rolled-back listing leaves
// the object pending and re-usable.
func (s *Service) replaceImages(ctx context.Context, tx pgx.Tx, q *db.Queries, sellerID, listingID uuid.UUID, mediaIDs []uuid.UUID) error {
	if err := q.DeleteListingImages(ctx, listingID); err != nil {
		return fmt.Errorf("clear listing images: %w", err)
	}
	seen := map[uuid.UUID]bool{}
	for i, mediaID := range mediaIDs {
		if seen[mediaID] {
			return fieldError("imageMediaIds", "must not repeat an image")
		}
		seen[mediaID] = true
		obj, err := s.media.Attach(ctx, tx, sellerID, mediaID)
		if errors.Is(err, media.ErrNotUploaded) || errors.Is(err, media.ErrForbidden) ||
			errors.Is(err, media.ErrNotFound) || errors.Is(err, media.ErrUploadMismatch) {
			return fieldError(fmt.Sprintf("imageMediaIds/%d", i), err.Error())
		}
		if err != nil {
			return err
		}
		if err := q.InsertListingImage(ctx, db.InsertListingImageParams{
			ListingID: listingID, MediaID: obj.ID, SortOrder: int32(i),
		}); err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: %s", ErrImageInUse, obj.ID)
			}
			return fmt.Errorf("insert listing image: %w", err)
		}
	}
	return nil
}

// replaceAttributes validates the values against the category's definitions
// and stores them, dropping anything the category no longer defines.
func (s *Service) replaceAttributes(ctx context.Context, q *db.Queries, listingID uuid.UUID, values map[string]string, cat catalog.Detail) error {
	valid, err := validateAttributes(values, cat)
	if err != nil {
		return err
	}
	if err := q.DeleteListingAttributes(ctx, listingID); err != nil {
		return fmt.Errorf("clear listing attributes: %w", err)
	}
	for _, v := range valid {
		if err := q.InsertListingAttribute(ctx, db.InsertListingAttributeParams{
			ListingID: listingID, AttributeID: v.ID, Value: v.Value,
		}); err != nil {
			return fmt.Errorf("insert listing attribute: %w", err)
		}
	}
	return nil
}

// attrValue pairs an attribute definition with the value to store.
type attrValue struct {
	ID    uuid.UUID
	Value string
}

// validate resolves the category and runs every DOMAIN §7 rule.
func (s *Service) validate(ctx context.Context, in Input) (catalog.Detail, error) {
	cat, err := s.category(ctx, in.CategorySlug)
	if err != nil {
		return catalog.Detail{}, err
	}
	return cat, s.validateFields(ctx, in, cat)
}

// category resolves a category slug, mapping "unknown" to a field error.
func (s *Service) category(ctx context.Context, slug string) (catalog.Detail, error) {
	cat, err := s.catalog.Detail(ctx, slug)
	if errors.Is(err, catalog.ErrNotFound) {
		return catalog.Detail{}, fieldError("categorySlug", "unknown category")
	}
	if err != nil {
		return catalog.Detail{}, err
	}
	return cat, nil
}

// validateFields runs the field rules for a resolved category.
func (s *Service) validateFields(ctx context.Context, in Input, cat catalog.Detail) error {
	var verr validation.Error

	leaf, err := s.catalog.IsLeaf(ctx, cat.ID)
	if err != nil {
		return err
	}
	if !leaf {
		verr.Add("categorySlug", "must be a category with no subcategories")
	}
	if n := utf8.RuneCountInString(in.Title); n < 5 || n > 100 {
		verr.Add("title", "must be 5-100 characters")
	}
	if n := utf8.RuneCountInString(in.Description); n < 20 || n > 5000 {
		verr.Add("description", "must be 20-5000 characters")
	}
	if in.PricePesewas < 1 || in.PricePesewas > MaxPricePesewas {
		verr.Add("price", "must be between 1 and 1000000000 pesewas")
	}
	if in.QuantityAvailable < 0 {
		verr.Add("quantityAvailable", "must be 0 or more")
	}
	if in.MinOrderQty < 1 {
		verr.Add("minOrderQty", "must be at least 1")
	}
	if in.QuantityAvailable > 0 && in.MinOrderQty > in.QuantityAvailable {
		verr.Add("minOrderQty", "must not exceed quantityAvailable")
	}
	if !contains(cat.Units, in.Unit) {
		verr.Add("unit", "must be one of the category's units")
	}
	if !contains(cat.ItemStates, in.ItemState) {
		verr.Add("itemState", "must be one of the category's item states")
	}
	if !geo.IsRegion(in.Region) {
		verr.Add("region", "must be one of the 16 regions")
	}
	if n := utf8.RuneCountInString(in.District); n < 2 || n > 80 {
		verr.Add("district", "must be 2-80 characters")
	}
	if in.Area != nil && utf8.RuneCountInString(*in.Area) > 120 {
		verr.Add("area", "must be at most 120 characters")
	}
	if !in.Delivery.Pickup && !in.Delivery.SellerDelivery {
		verr.Add("deliveryOptions", "must offer pickup or seller delivery")
	}
	if in.Delivery.SellerDelivery && in.Delivery.FeePesewas == nil {
		verr.Add("deliveryOptions/sellerDeliveryFee", "is required when seller delivery is offered")
	}
	if in.Delivery.FeePesewas != nil && *in.Delivery.FeePesewas < 0 {
		verr.Add("deliveryOptions/sellerDeliveryFee", "must be 0 or more")
	}
	if len(in.ImageMediaIDs) > MaxImagesPerListing {
		verr.Add("imageMediaIds", "must have at most 10 images")
	}
	if _, err := validateAttributes(in.Attributes, cat); err != nil {
		var ve *validation.Error
		if errors.As(err, &ve) {
			verr.Fields = append(verr.Fields, ve.Fields...)
		}
	}
	return verr.OrNil()
}

// validateAttributes checks values against the category's definitions
// (DOMAIN §7): unknown keys, select options, number and date formats and
// missing required attributes. Failures name `attributes/<key>`.
func validateAttributes(values map[string]string, cat catalog.Detail) ([]attrValue, error) {
	var verr validation.Error
	defs := map[string]catalog.Attribute{}
	for _, d := range cat.Attributes {
		defs[d.Key] = d
	}
	// Sorted by key so inserts (and error order) are deterministic.
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var valid []attrValue
	for _, key := range keys {
		value := values[key]
		def, ok := defs[key]
		if !ok {
			verr.Add("attributes/"+key, "is not an attribute of this category")
			continue
		}
		if err := validateAttributeValue(def.Type, def.Options, value); err != nil {
			verr.Add("attributes/"+key, err.Error())
			continue
		}
		valid = append(valid, attrValue{ID: def.ID, Value: strings.TrimSpace(value)})
	}
	for _, d := range cat.Attributes {
		if d.Required && strings.TrimSpace(values[d.Key]) == "" {
			verr.Add("attributes/"+d.Key, "is required")
		}
	}
	if err := verr.OrNil(); err != nil {
		return nil, err
	}
	return valid, nil
}

// validateAttributeValue applies the DOMAIN §7 per-type rules.
func validateAttributeValue(attrType string, options []string, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("must not be empty")
	}
	if utf8.RuneCountInString(value) > MaxAttributeValueLen {
		return fmt.Errorf("must be at most %d characters", MaxAttributeValueLen)
	}
	switch attrType {
	case catalog.AttrSelect:
		if !contains(options, value) {
			return fmt.Errorf("must be one of: %s", strings.Join(options, ", "))
		}
	case catalog.AttrBoolean:
		if value != "true" && value != "false" {
			return errors.New("must be true or false")
		}
	case catalog.AttrNumber:
		f, err := strconv.ParseFloat(value, 64)
		if err != nil || f > 1e9 || f < -1e9 {
			return errors.New("must be a number within +/-1e9")
		}
	case catalog.AttrDate:
		if _, err := time.Parse("2006-01-02", value); err != nil {
			return errors.New("must be a date in YYYY-MM-DD")
		}
	case catalog.AttrText:
		// length already checked above
	}
	return nil
}

func (s *Service) requireSeller(ctx context.Context, sellerID uuid.UUID) error {
	if s.sellers == nil {
		return nil
	}
	ok, err := s.sellers.Exists(ctx, sellerID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrSellerProfileRequired
	}
	return nil
}

// applyPatch merges a patch over the stored row, producing the Input the
// validators run against. Attribute values come from storage when the patch
// does not supply them, so a category change can be validated as a whole.
func applyPatch(row db.Listing, patch Patch, categorySlug string) Input {
	in := Input{
		CategorySlug:      categorySlug,
		Title:             row.Title,
		Description:       row.Description,
		PricePesewas:      row.PricePesewas,
		Unit:              row.Unit,
		QuantityAvailable: row.QuantityAvailable,
		MinOrderQty:       row.MinOrderQty,
		IsNegotiable:      row.IsNegotiable,
		ItemState:         row.ItemState,
		Region:            row.Region,
		District:          row.District,
		Area:              row.Area,
		Delivery: Delivery{
			Pickup:         row.OffersPickup,
			SellerDelivery: row.OffersSellerDelivery,
			FeePesewas:     row.SellerDeliveryFeePesewas,
		},
	}
	setStr(&in.Title, patch.Title)
	setStr(&in.Description, patch.Description)
	setInt64(&in.PricePesewas, patch.PricePesewas)
	setStr(&in.Unit, patch.Unit)
	setInt32(&in.QuantityAvailable, patch.QuantityAvailable)
	setInt32(&in.MinOrderQty, patch.MinOrderQty)
	setBool(&in.IsNegotiable, patch.IsNegotiable)
	setStr(&in.ItemState, patch.ItemState)
	setStr(&in.Region, patch.Region)
	setStr(&in.District, patch.District)
	setStrPtr(&in.Area, patch.Area)
	if patch.CategorySlug != nil {
		in.CategorySlug = *patch.CategorySlug
	}
	if patch.Delivery != nil {
		in.Delivery = patch.Delivery.Merge(in.Delivery)
	}
	in.Attributes = patch.Attributes
	in.ImageMediaIDs = patch.ImageMediaIDs
	return in
}

func fromRow(r db.Listing) Listing {
	return Listing{
		ID: r.ID, SellerID: r.SellerID, CategoryID: r.CategoryID, Title: r.Title,
		Slug: r.Slug, Description: r.Description, PricePesewas: r.PricePesewas,
		Unit: r.Unit, QuantityAvailable: r.QuantityAvailable, MinOrderQty: r.MinOrderQty,
		IsNegotiable: r.IsNegotiable, ItemState: r.ItemState, Status: r.Status,
		Region: r.Region, District: r.District, Area: r.Area,
		OffersPickup: r.OffersPickup, OffersSellerDelivery: r.OffersSellerDelivery,
		SellerDeliveryFeePesewas: r.SellerDeliveryFeePesewas,
		PublishedAt:              r.PublishedAt, ExpiresAt: r.ExpiresAt,
		ViewCount: r.ViewCount, FavoriteCount: r.FavoriteCount, ContactCount: r.ContactCount,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

// Patch merge helpers: a nil pointer keeps the stored value.
func setStr(dst *string, p *string) {
	if p != nil {
		*dst = *p
	}
}

func setInt64(dst *int64, p *int64) {
	if p != nil {
		*dst = *p
	}
}

func setInt32(dst *int32, p *int32) {
	if p != nil {
		*dst = *p
	}
}

func setBool(dst *bool, p *bool) {
	if p != nil {
		*dst = *p
	}
}

func setStrPtr(dst **string, p *string) {
	if p != nil {
		*dst = p
	}
}

func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

// fieldError builds a one-field *validation.Error.
func fieldError(name, msg string) error {
	e := &validation.Error{}
	e.Add(name, msg)
	return e
}

// isUniqueViolation reports a Postgres unique-constraint error.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// slugFor returns base for the first try, then base-2, base-3 … and finally
// base-<6 random base36 chars> (DOMAIN §7).
func slugFor(base string, attempt int) string {
	switch {
	case attempt == 0:
		return base
	case attempt <= SlugCollisionTries:
		return base + "-" + strconv.Itoa(attempt+1)
	default:
		return base + "-" + randomSuffix()
	}
}

func randomSuffix() string {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	b := make([]byte, 6)
	for i := range b {
		b[i] = alphabet[rand.Intn(len(alphabet))] //nolint:gosec // slug cosmetics, not security
	}
	return string(b)
}
