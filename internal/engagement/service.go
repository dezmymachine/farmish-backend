// Package engagement owns reviews, favorites and reports (Phase 20).
package engagement

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/audit"
	"github.com/dezmymachine/farmish-backend/internal/database"
	"github.com/dezmymachine/farmish-backend/internal/db"
	"github.com/dezmymachine/farmish-backend/internal/listings"
	"github.com/dezmymachine/farmish-backend/internal/orders"
	"github.com/dezmymachine/farmish-backend/internal/users"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

var (
	// ErrNotFound means no review, favorite or order matches for the caller.
	ErrNotFound = errors.New("not found")
	// ErrForbidden means the caller is not the order's buyer.
	ErrForbidden = errors.New("only the buyer may review this order")
	// ErrOrderNotCompleted means the order is not completed yet.
	ErrOrderNotCompleted = errors.New("only a completed order may be reviewed")
	// ErrAlreadyReviewed means this reviewer already reviewed this listing
	// in this order.
	ErrAlreadyReviewed = errors.New("this listing was already reviewed in this order")
	// ErrListingNotInOrder means the listing is not one of the order's items.
	ErrListingNotInOrder = errors.New("listing is not part of this order")
)

// OrderStore is the part of the order surface reviews need: the buyer's
// completed order with its items, plus the buyer check that keeps 403s from
// leaking into 404s.
type OrderStore interface {
	BuyerOf(ctx context.Context, orderID uuid.UUID) (uuid.UUID, error)
	Get(ctx context.Context, callerID, orderID uuid.UUID) (orders.Detail, bool, error)
}

// UserStore reads display names for reviewer labels.
type UserStore interface {
	Get(ctx context.Context, id uuid.UUID) (users.User, error)
}

// CoverStore turns a media key into a public image URL.
type CoverStore interface {
	PublicURL(key string) string
}

// Review is one row of the reviews table.
type Review struct {
	ID         uuid.UUID
	OrderID    uuid.UUID
	ListingID  uuid.UUID
	SellerID   uuid.UUID
	ReviewerID uuid.UUID
	Rating     int16
	Comment    *string
	HiddenAt   *time.Time
	CreatedAt  time.Time
}

// Rating is a seller aggregate: the one-decimal average and the count.
type Rating struct {
	Average float64
	Count   int64
}

// FavoriteListing is one favourited listing with its browsability flag.
type FavoriteListing struct {
	Listing   listings.PublicSummary
	Available bool
}

// Service owns reviews and favorites.
type Service struct {
	pool   *pgxpool.Pool
	orders OrderStore
	users  UserStore
	media  CoverStore
	log    *slog.Logger
	// Now is the clock, injectable so tests never sleep.
	Now func() time.Time
}

// New returns the service. Media may be nil (no cover URLs then).
func New(pool *pgxpool.Pool, orders OrderStore, users UserStore, media CoverStore) *Service {
	return &Service{pool: pool, orders: orders, users: users, media: media, Now: time.Now}
}

// AttachLogger gives the service its logger.
func (s *Service) AttachLogger(l *slog.Logger) { s.log = l }

// CreateReview records the buyer's rating of a listing bought in a
// completed order. It is idempotent on (order, listing, reviewer): a replay
// returns the existing row as already_reviewed.
func (s *Service) CreateReview(ctx context.Context, buyerID, orderID, listingID uuid.UUID, rating int16, comment string) (Review, error) {
	if rating < 1 || rating > 5 {
		var invalid validation.Error
		invalid.Add("rating", "must be between 1 and 5")
		return Review{}, invalid.OrNil()
	}
	if len(comment) > 1000 {
		var invalid validation.Error
		invalid.Add("comment", "must be at most 1000 characters")
		return Review{}, invalid.OrNil()
	}
	buyer, err := s.orders.BuyerOf(ctx, orderID)
	if errors.Is(err, orders.ErrNotFound) {
		return Review{}, fmt.Errorf("%w: %s", ErrNotFound, orderID)
	}
	if err != nil {
		return Review{}, fmt.Errorf("get order buyer: %w", err)
	}
	if buyer != buyerID {
		return Review{}, ErrForbidden
	}
	detail, _, err := s.orders.Get(ctx, buyerID, orderID)
	if err != nil {
		return Review{}, fmt.Errorf("get order: %w", err)
	}
	if detail.Status != orders.StatusCompleted {
		return Review{}, ErrOrderNotCompleted
	}
	inOrder := false
	for _, item := range detail.Items {
		if item.ListingID == listingID {
			inOrder = true
			break
		}
	}
	if !inOrder {
		return Review{}, ErrListingNotInOrder
	}
	row, err := db.New(s.pool).InsertReview(ctx, db.InsertReviewParams{
		OrderID: orderID, ListingID: listingID, SellerID: detail.SellerID,
		ReviewerID: buyerID, Rating: rating, Comment: orNil(comment),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Review{}, ErrAlreadyReviewed
	}
	if err != nil {
		return Review{}, fmt.Errorf("insert review: %w", err)
	}
	return fromReviewRow(row), nil
}

// ListReviews returns one listing's visible reviews, newest first, plus the
// seller aggregate over the same visible set.
func (s *Service) ListReviews(ctx context.Context, listingID uuid.UUID, limit, offset int32) ([]Review, int64, Rating, error) {
	q := db.New(s.pool)
	rows, err := q.ListVisibleReviewsByListing(ctx, db.ListVisibleReviewsByListingParams{
		ListingID: listingID, Limit: limit, Offset: offset,
	})
	if err != nil {
		return nil, 0, Rating{}, fmt.Errorf("list reviews: %w", err)
	}
	total, err := q.CountVisibleReviewsByListing(ctx, listingID)
	if err != nil {
		return nil, 0, Rating{}, fmt.Errorf("count reviews: %w", err)
	}
	out := make([]Review, 0, len(rows))
	sellerID := uuid.Nil
	for _, row := range rows {
		out = append(out, fromReviewRow(row))
		sellerID = row.SellerID
	}
	rating := Rating{}
	if total > 0 {
		agg, err := q.SellerRating(ctx, sellerID)
		if err != nil {
			return nil, 0, Rating{}, fmt.Errorf("seller rating: %w", err)
		}
		rating = Rating{Average: agg.Average, Count: agg.Count}
	}
	return out, total, rating, nil
}

// SellerRating returns a seller's aggregate over visible reviews.
func (s *Service) SellerRating(ctx context.Context, sellerID uuid.UUID) (Rating, error) {
	agg, err := db.New(s.pool).SellerRating(ctx, sellerID)
	if err != nil {
		return Rating{}, fmt.Errorf("seller rating: %w", err)
	}
	return Rating{Average: agg.Average, Count: agg.Count}, nil
}

// HideReview hides a review for moderation, with an audit event. An already
// hidden review cannot be hidden again.
func (s *Service) HideReview(ctx context.Context, adminID, reviewID uuid.UUID, reason string) (Review, error) {
	trimmed := strings.TrimSpace(reason)
	if len(trimmed) < 1 || len(trimmed) > 500 {
		var invalid validation.Error
		invalid.Add("reason", "must be between 1 and 500 characters")
		return Review{}, invalid.OrNil()
	}
	var hidden db.Review
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		now := s.Now()
		row, err := db.New(tx).SetReviewHidden(ctx, db.SetReviewHiddenParams{
			ID: reviewID, HiddenAt: &now,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrNotFound, reviewID)
		}
		if err != nil {
			return fmt.Errorf("hide review: %w", err)
		}
		hidden = row
		return audit.Record(ctx, tx, audit.Event{
			ActorID: &adminID, Action: "review.hide", TargetType: "review", TargetID: reviewID.String(),
			Metadata: map[string]any{"reason": trimmed},
		})
	})
	if err != nil {
		return Review{}, err
	}
	return fromReviewRow(hidden), nil
}

// AddFavorite idempotently favourites a listing, bumping its counter only
// on the first add, in one transaction.
func (s *Service) AddFavorite(ctx context.Context, userID, listingID uuid.UUID) error {
	return database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		if _, err := q.GetListingForFavorite(ctx, listingID); errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrNotFound, listingID)
		} else if err != nil {
			return fmt.Errorf("get listing: %w", err)
		}
		row, err := q.InsertFavorite(ctx, db.InsertFavoriteParams{UserID: userID, ListingID: listingID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("insert favorite: %w", err)
		}
		_ = row
		return q.BumpFavoriteCount(ctx, db.BumpFavoriteCountParams{ID: listingID, FavoriteCount: 1})
	})
}

// RemoveFavorite idempotently unfavourites, dropping the counter only when
// a row actually went away.
func (s *Service) RemoveFavorite(ctx context.Context, userID, listingID uuid.UUID) error {
	return database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		removed, err := q.DeleteFavorite(ctx, db.DeleteFavoriteParams{UserID: userID, ListingID: listingID})
		if err != nil {
			return fmt.Errorf("delete favorite: %w", err)
		}
		if removed == 0 {
			return nil
		}
		return q.BumpFavoriteCount(ctx, db.BumpFavoriteCountParams{ID: listingID, FavoriteCount: -1})
	})
}

// ListFavorites returns the caller's favourited listings with summaries,
// newest favourited first, inactive ones flagged unavailable.
func (s *Service) ListFavorites(ctx context.Context, userID uuid.UUID, limit, offset int32) ([]FavoriteListing, int64, error) {
	q := db.New(s.pool)
	now := s.Now()
	rows, err := q.ListFavoriteListings(ctx, db.ListFavoriteListingsParams{
		UserID: userID, Now: &now, Limit: limit, Offset: offset,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("list favorites: %w", err)
	}
	total, err := q.CountFavoritesByUser(ctx, userID)
	if err != nil {
		return nil, 0, fmt.Errorf("count favorites: %w", err)
	}
	out := make([]FavoriteListing, 0, len(rows))
	for _, row := range rows {
		out = append(out, FavoriteListing{Listing: toPublicSummary(row, s.media), Available: row.Available})
	}
	return out, total, nil
}

// ReviewerName renders the public reviewer label: the display name's first
// word, or "Buyer".
func (s *Service) ReviewerName(ctx context.Context, reviewerID uuid.UUID) string {
	user, err := s.users.Get(ctx, reviewerID)
	if err != nil || user.DisplayName == nil {
		return "Buyer"
	}
	if first, _, _ := strings.Cut(strings.TrimSpace(*user.DisplayName), " "); first != "" {
		return first
	}
	return "Buyer"
}

// orNil blanks an empty comment to NULL.
func orNil(comment string) *string {
	if comment == "" {
		return nil
	}
	return &comment
}

// fromReviewRow maps the generated row onto the domain type.
func fromReviewRow(r db.Review) Review {
	return Review{
		ID: r.ID, OrderID: r.OrderID, ListingID: r.ListingID, SellerID: r.SellerID,
		ReviewerID: r.ReviewerID, Rating: r.Rating, Comment: r.Comment,
		HiddenAt: r.HiddenAt, CreatedAt: r.CreatedAt,
	}
}

// toPublicSummary maps a favorite row onto the public listing summary,
// sharing the search shape (cover, promo, safe seller).
func toPublicSummary(row db.ListFavoriteListingsRow, media CoverStore) listings.PublicSummary {
	out := listings.PublicSummary{
		ID: row.ID, Slug: row.Slug, Title: row.Title, PricePesewas: row.PricePesewas,
		Unit: row.Unit, Region: row.Region, District: row.District, ItemState: row.ItemState,
		Category: listings.CategoryRef{Slug: row.CategorySlug, Name: row.CategoryName},
		Seller:   listings.SellerRef{Name: row.SellerName, Verified: row.SellerVerified},
		PublishedAt: derefTime(row.PublishedAt),
	}
	if key, ok := row.CoverKey.(string); ok && key != "" && media != nil {
		if url := media.PublicURL(key); url != "" {
			out.CoverURL = &url
		}
	}
	if row.PromoTier != nil {
		out.Promo = &listings.PromoRef{Tier: *row.PromoTier}
	}
	return out
}

// derefTime zeroes a missing timestamp.
func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}
