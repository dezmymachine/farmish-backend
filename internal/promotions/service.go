package promotions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dezmymachine/farmish-backend/internal/database"
	"github.com/dezmymachine/farmish-backend/internal/db"
	"github.com/dezmymachine/farmish-backend/internal/ledger"
	"github.com/dezmymachine/farmish-backend/internal/payments"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

// ApplyInput promotes one listing to one tier.
type ApplyInput struct {
	ListingID uuid.UUID
	Tier      string
}

// Configs returns the active promotion packages in display order.
func (s *Service) Configs(ctx context.Context) ([]Config, error) {
	rows, err := db.New(s.pool).ListActivePromotionConfigs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list promotion configs: %w", err)
	}
	configs := make([]Config, 0, len(rows))
	for _, row := range rows {
		configs = append(configs, configFromRow(row))
	}
	return configs, nil
}

// Tier returns one promotion package. An inactive or unknown tier is the same
// 404: whether a tier exists but is not for sale is not public information.
func (s *Service) Tier(ctx context.Context, tier string) (Config, error) {
	if strings.TrimSpace(tier) == "" {
		return Config{}, fmt.Errorf("%w: %q", ErrTierNotFound, tier)
	}
	row, err := db.New(s.pool).GetPromotionConfig(ctx, tier)
	if errors.Is(err, pgx.ErrNoRows) {
		return Config{}, fmt.Errorf("%w: %q", ErrTierNotFound, tier)
	}
	if err != nil {
		return Config{}, fmt.Errorf("get promotion tier %q: %w", tier, err)
	}
	if !row.IsActive {
		return Config{}, fmt.Errorf("%w: %q", ErrTierNotFound, tier)
	}
	return configFromRow(row), nil
}

// Purchase starts a promotion-package charge for any authenticated buyer. The
// tier's price and credit count are snapshotted before Paystack is called: the
// payment row stores the amounts and the tier's credits in its metadata.
func (s *Service) Purchase(ctx context.Context, buyerID uuid.UUID, email, tier string) (Purchase, error) {
	var verr validation.Error
	if buyerID == uuid.Nil {
		verr.Add("buyerId", "is required")
	}
	if strings.TrimSpace(tier) == "" {
		verr.Add("tier", "is required")
	}
	if err := verr.OrNil(); err != nil {
		return Purchase{}, err
	}

	config, err := s.Tier(ctx, tier)
	if err != nil {
		return Purchase{}, err
	}
	payment, err := s.payments.Initialize(ctx, payments.CreateInput{
		UserID: buyerID, Email: email, Purpose: payments.PurposePromotion,
		PurposeRef: config.Tier, BasePesewas: config.PricePesewas,
		Metadata: map[string]any{MetadataCreditsKey: int(config.Credits)},
	})
	if err != nil {
		return Purchase{}, err
	}
	if payment.AuthorizationURL == nil || *payment.AuthorizationURL == "" {
		return Purchase{}, fmt.Errorf("promotion purchase %s has no authorization URL", payment.Reference)
	}
	return Purchase{Payment: payment, Tier: config}, nil
}

// HandlePromotionPaid grants the purchased credits. It runs in the
// payments.succeeded job's transaction and is idempotent on the payment
// reference: a retry merely finds the ledger transaction already posted.
func (s *Service) HandlePromotionPaid(ctx context.Context, tx pgx.Tx, payment payments.Payment) error {
	if payment.Purpose != payments.PurposePromotion {
		return fmt.Errorf("promotion handler called for purpose %q", payment.Purpose)
	}
	if payment.Status != payments.StatusSuccess {
		return fmt.Errorf("promotion handler called for payment status %q", payment.Status)
	}
	if payment.ProviderFee == nil {
		return fmt.Errorf("promotion payment %s is missing its actual provider fee", payment.Reference)
	}
	credits, err := promotionCredits(payment.Metadata)
	if err != nil {
		return fmt.Errorf("promotion payment %s: %w", payment.Reference, err)
	}
	err = s.ledger.Post(ctx, tx, ledger.KindPromotionPaid, payment.Reference,
		ledger.PromotionPaid(payment.Base, payment.Charge, *payment.ProviderFee, int64(credits), payment.UserID)...,
	)
	if errors.Is(err, ledger.ErrDuplicate) {
		return nil
	}
	return err
}

// Credits returns the buyer's CRD balance as a positive display number. The
// ledger stores the corresponding liability as a negative raw sum.
func (s *Service) Credits(ctx context.Context, userID uuid.UUID) (int32, error) {
	if userID == uuid.Nil {
		return 0, fmt.Errorf("promotion credits require a user")
	}
	raw, err := s.ledger.Balance(ctx, s.pool, ledger.PromoCredits(userID))
	if err != nil {
		return 0, err
	}
	if raw > 0 {
		return 0, fmt.Errorf("promotion credit balance is unexpectedly positive: %d", raw)
	}
	if raw < -math.MaxInt32 {
		return 0, fmt.Errorf("promotion credit balance is out of range: %d", raw)
	}
	// The range check above makes this narrowing conversion safe.
	return int32(-raw), nil //nolint:gosec // G115: raw is within ±MaxInt32 here
}

// Apply spends a tier's credits to promote the caller's own active listing. The
// ownership check, advisory lock, balance check, promotion insert and ledger
// posting all happen in one transaction.
func (s *Service) Apply(ctx context.Context, sellerID uuid.UUID, in ApplyInput) (Application, error) {
	var verr validation.Error
	if sellerID == uuid.Nil {
		verr.Add("sellerId", "is required")
	}
	if in.ListingID == uuid.Nil {
		verr.Add("listingId", "is required")
	}
	if strings.TrimSpace(in.Tier) == "" {
		verr.Add("tier", "is required")
	}
	if err := verr.OrNil(); err != nil {
		return Application{}, err
	}

	now := s.Now()
	var application Application
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		row, err := q.GetPromotionConfig(ctx, in.Tier)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !row.IsActive) {
			return fmt.Errorf("%w: %q", ErrTierNotFound, in.Tier)
		}
		if err != nil {
			return fmt.Errorf("get promotion tier %q: %w", in.Tier, err)
		}
		config := configFromRow(row)

		if err := s.listings.RequireAdvertisable(ctx, tx, sellerID, in.ListingID, now); err != nil {
			return err
		}
		if err := lockPromotionCredits(ctx, q, sellerID); err != nil {
			return err
		}
		balance, err := s.ledger.Balance(ctx, tx, ledger.PromoCredits(sellerID))
		if err != nil {
			return err
		}
		if balance > -int64(config.Credits) {
			return fmt.Errorf("%w: need %d credits", ErrInsufficientCredits, config.Credits)
		}

		startsAt := now
		replaced := false
		current, err := q.GetActiveListingPromotionForUpdate(ctx, db.GetActiveListingPromotionForUpdateParams{
			ListingID: in.ListingID, StartsAt: now,
		})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("get active promotion: %w", err)
		}
		if err == nil {
			if config.TierRank < current.TierRank {
				return fmt.Errorf("%w: %q is below the active %q tier", ErrPromotionDowngrade, config.Tier, current.Tier)
			}
			if config.TierRank == current.TierRank {
				// A same-tier application queues behind the current window
				// rather than updating history: the new row starts when the
				// current row ends.
				startsAt = current.EndsAt
			} else {
				// A higher tier replaces the current window immediately. Ending
				// the old row forfeits its remaining time.
				if err := q.EndActiveListingPromotions(ctx, db.EndActiveListingPromotionsParams{
					ListingID: in.ListingID, EndsAt: now,
				}); err != nil {
					return fmt.Errorf("end active promotion: %w", err)
				}
				replaced = now.Before(current.EndsAt)
			}
		}

		inserted, err := q.InsertListingPromotion(ctx, db.InsertListingPromotionParams{
			ListingID: in.ListingID, SellerID: sellerID, Tier: config.Tier,
			TierRank: config.TierRank, StartsAt: startsAt,
			EndsAt:       startsAt.AddDate(0, 0, int(config.DurationDays)),
			CreditsSpent: config.Credits,
		})
		if err != nil {
			return fmt.Errorf("insert listing promotion: %w", err)
		}
		if err := s.ledger.Post(ctx, tx, ledger.KindPromotionApplied, inserted.ID.String(),
			ledger.PromotionApplied(sellerID, int64(config.Credits))...,
		); err != nil {
			return err
		}
		application = Application{
			ID: inserted.ID, ListingID: inserted.ListingID, Tier: inserted.Tier,
			StartsAt: inserted.StartsAt, EndsAt: inserted.EndsAt,
			CreditsSpent: inserted.CreditsSpent, ReplacedRemaining: replaced,
		}
		return nil
	})
	if err != nil {
		return Application{}, err
	}
	return application, nil
}

// Applications returns one listing's promotion history, newest window first,
// after checking that the caller owns the listing.
func (s *Service) Applications(ctx context.Context, sellerID, listingID uuid.UUID) ([]Application, error) {
	if sellerID == uuid.Nil || listingID == uuid.Nil {
		return nil, fmt.Errorf("promotion history requires a seller and listing")
	}
	now := s.Now()
	var applications []Application
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.listings.RequireAdvertisable(ctx, tx, sellerID, listingID, now); err != nil {
			return err
		}
		rows, err := db.New(tx).ListListingPromotions(ctx, listingID)
		if err != nil {
			return fmt.Errorf("list listing promotions: %w", err)
		}
		applications = make([]Application, 0, len(rows))
		for _, row := range rows {
			applications = append(applications, Application{
				ID: row.ID, ListingID: row.ListingID, Tier: row.Tier,
				StartsAt: row.StartsAt, EndsAt: row.EndsAt, CreditsSpent: row.CreditsSpent,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return applications, nil
}

// lockPromotionCredits serializes one buyer's credit balance changes for the
// transaction. The ledger has no mutable balance row to lock, so the advisory
// lock is the concurrency control DOMAIN §5.3.5 requires.
func lockPromotionCredits(ctx context.Context, q *db.Queries, userID uuid.UUID) error {
	if _, err := q.LockPromotionCredits(ctx, userID.String()); err != nil {
		return fmt.Errorf("lock promotion credits: %w", err)
	}
	return nil
}

// promotionCredits reads the purchase-time credit snapshot from a payment. A
// later config edit changes what future buyers receive, never what a settled
// payment already bought.
func promotionCredits(metadata map[string]any) (int32, error) {
	value, ok := metadata[MetadataCreditsKey]
	if !ok {
		return 0, fmt.Errorf("payment metadata is missing %q", MetadataCreditsKey)
	}
	switch credits := value.(type) {
	case int:
		return checkedCredits(int64(credits))
	case int8:
		return checkedCredits(int64(credits))
	case int16:
		return checkedCredits(int64(credits))
	case int32:
		return checkedCredits(int64(credits))
	case int64:
		return checkedCredits(credits)
	case float64:
		if credits != math.Trunc(credits) {
			return 0, fmt.Errorf("payment metadata %q is not a whole number", MetadataCreditsKey)
		}
		return checkedCredits(int64(credits))
	case json.Number:
		parsed, err := credits.Int64()
		if err != nil {
			return 0, fmt.Errorf("payment metadata %q is not a whole number", MetadataCreditsKey)
		}
		return checkedCredits(parsed)
	default:
		return 0, fmt.Errorf("payment metadata %q has the wrong type %T", MetadataCreditsKey, value)
	}
}

func checkedCredits(credits int64) (int32, error) {
	if credits <= 0 || credits > math.MaxInt32 {
		return 0, fmt.Errorf("payment metadata %q is out of range: %d", MetadataCreditsKey, credits)
	}
	return int32(credits), nil
}
