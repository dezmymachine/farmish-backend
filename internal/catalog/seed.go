package catalog

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/database"
	"github.com/dezmymachine/farmish-backend/internal/db"
	"github.com/dezmymachine/farmish-backend/internal/text"
)

// Seed upserts the DOMAIN §9 tree: parents, children and attributes.
// Idempotent: the upsert queries only write when something changed, so a
// repeat run changes nothing (not even updated_at).
func Seed(ctx context.Context, pool *pgxpool.Pool) error {
	return database.InTx(ctx, pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		for i, parent := range SeedCategories {
			parentID, err := upsertCategory(ctx, q, parent.Name, parent.Slug, nil, &parent.Icon, &parent.Group, int32(i+1))
			if err != nil {
				return fmt.Errorf("seed parent %s: %w", parent.Slug, err)
			}
			for j, child := range parent.Children {
				slug := parent.Slug + "-" + text.Slugify(child)
				if _, err := upsertCategory(ctx, q, child, slug, &parentID, nil, nil, int32(j+1)); err != nil {
					return fmt.Errorf("seed child %s: %w", slug, err)
				}
			}
			for k, attr := range parent.Attributes {
				options := attr.Options
				if options == nil {
					options = []string{}
				}
				params := db.UpsertCategoryAttributeParams{
					CategoryID: parentID, Key: attr.Key, Label: attr.Label, Type: attr.Type,
					Options: options, Required: attr.Required, SortOrder: int32(k + 1),
				}
				if _, err := q.UpsertCategoryAttribute(ctx, params); err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return fmt.Errorf("seed attribute %s.%s: %w", parent.Slug, attr.Key, err)
				}
			}
		}
		return nil
	})
}

// upsertCategory upserts one row and returns its id, fetching the existing
// row when the no-change guard suppressed the write.
func upsertCategory(ctx context.Context, q *db.Queries, name, slug string, parentID *uuid.UUID, icon, group *string, sortOrder int32) (uuid.UUID, error) {
	row, err := q.UpsertCategoryBySlug(ctx, db.UpsertCategoryBySlugParams{
		Name: name, Slug: slug, ParentID: pgUUIDPtr(parentID),
		Icon: icon, ListingGroup: group, SortOrder: sortOrder,
	})
	if err == nil {
		return row.ID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.UUID{}, err
	}
	existing, err := q.GetCategoryBySlug(ctx, slug)
	if err != nil {
		return uuid.UUID{}, fmt.Errorf("fetch unchanged category: %w", err)
	}
	return existing.ID, nil
}
