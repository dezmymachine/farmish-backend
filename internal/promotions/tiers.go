package promotions

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/database"
	"github.com/dezmymachine/farmish-backend/internal/db"
)

// SeedTier is one DOMAIN §6 package. Prices are pesewas; descriptions and
// feature bullets are copied from the legacy seed data.
type SeedTier struct {
	Tier         string
	Name         string
	PricePesewas int64
	Credits      int32
	DurationDays int32
	TierRank     int32
	Featured     bool
	Description  string
	Features     []string
	SortOrder    int32
	Active       bool
}

// SeedTiers is the four packages buyers can purchase.
var SeedTiers = []SeedTier{
	{
		Tier: TierTop, Name: "Starter", PricePesewas: 5500, Credits: 55,
		DurationDays: 7, TierRank: 1, Featured: false,
		Description: "Perfect for beginners - get your first ads noticed",
		Features: []string{
			"55 promotion credits",
			"Top placement in search",
			"7 days duration",
			"Increased visibility",
		},
		SortOrder: 1, Active: true,
	},
	{
		Tier: TierVIP, Name: "Growth", PricePesewas: 7500, Credits: 75,
		DurationDays: 14, TierRank: 2, Featured: false,
		Description: "Most popular choice for growing sellers",
		Features: []string{
			"75 promotion credits",
			"VIP badge on listings",
			"Highlighted in orange",
			"14 days duration",
			"Priority in search",
			"3x more views",
		},
		SortOrder: 2, Active: true,
	},
	{
		Tier: TierDiamond, Name: "Premium", PricePesewas: 10000, Credits: 100,
		DurationDays: 21, TierRank: 3, Featured: true,
		Description: "Maximum exposure for serious sellers",
		Features: []string{
			"100 promotion credits",
			"Diamond badge",
			"Featured on homepage",
			"Category spotlight",
			"21 days duration",
			"5x more views",
			"Premium support",
		},
		SortOrder: 3, Active: true,
	},
	{
		Tier: TierEnterprise, Name: "Enterprise", PricePesewas: 15000, Credits: 150,
		DurationDays: 30, TierRank: 4, Featured: true,
		Description: "Full business package with maximum benefits",
		Features: []string{
			"150 promotion credits",
			"Enterprise badge",
			"Featured on homepage",
			"Unlimited promotions",
			"30 days duration",
			"10x more views",
			"Verified business status",
			"Analytics dashboard",
			"Priority support",
		},
		SortOrder: 4, Active: true,
	},
}

// Seed upserts the DOMAIN §6 tiers. Idempotent: the no-change guard leaves an
// unchanged row untouched, including updated_at.
func Seed(ctx context.Context, pool *pgxpool.Pool) error {
	return database.InTx(ctx, pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		for _, tier := range SeedTiers {
			row, err := q.UpsertPromotionConfig(ctx, db.UpsertPromotionConfigParams{
				Tier: tier.Tier, Name: tier.Name, PricePesewas: tier.PricePesewas,
				Credits: tier.Credits, DurationDays: tier.DurationDays, TierRank: tier.TierRank,
				Featured: tier.Featured, Description: tier.Description, Features: tier.Features,
				SortOrder: tier.SortOrder, IsActive: tier.Active,
			})
			if err == nil {
				_ = row
				continue
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("seed promotion tier %s: %w", tier.Tier, err)
			}
			if _, err := q.GetPromotionConfig(ctx, tier.Tier); err != nil {
				return fmt.Errorf("fetch unchanged promotion tier %s: %w", tier.Tier, err)
			}
		}
		return nil
	})
}

// configFromRow maps a generated tier onto the API-facing shape.
func configFromRow(row db.PromotionConfig) Config {
	return Config{
		Tier: row.Tier, Name: row.Name, PricePesewas: row.PricePesewas,
		Credits: row.Credits, DurationDays: row.DurationDays, TierRank: row.TierRank,
		Featured: row.Featured, Description: row.Description, Features: row.Features,
		SortOrder: row.SortOrder, Active: row.IsActive,
	}
}
