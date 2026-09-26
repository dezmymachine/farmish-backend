package catalog

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/db"
	"github.com/dezmymachine/farmish-backend/internal/text"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

// slugPattern mirrors the categories.slug CHECK.
var slugPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// keyPattern mirrors the category_attributes.key CHECK.
var keyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{1,40}$`)

// Service reads and writes the catalog.
type Service struct {
	pool *pgxpool.Pool
}

// New returns a Service over pool.
func New(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// Tree returns active parents with their active children, each ordered by
// (sort_order, name). Children of an inactive parent are hidden too.
func (s *Service) Tree(ctx context.Context) ([]Node, error) {
	rows, err := db.New(s.pool).ListActiveCategories(ctx)
	if err != nil {
		return nil, fmt.Errorf("list categories: %w", err)
	}
	active := map[uuid.UUID]bool{}
	for _, r := range rows {
		active[r.ID] = true
	}
	byParent := map[uuid.UUID][]Category{}
	var parents []Category
	for _, r := range rows {
		c := fromRow(r)
		switch {
		case c.ParentID == nil:
			parents = append(parents, c)
		case active[*c.ParentID]:
			byParent[*c.ParentID] = append(byParent[*c.ParentID], c)
		}
	}
	sort.Slice(parents, func(i, j int) bool { return lessCategory(parents[i], parents[j]) })
	out := make([]Node, 0, len(parents))
	for _, p := range parents {
		children := byParent[p.ID]
		sort.Slice(children, func(i, j int) bool { return lessCategory(children[i], children[j]) })
		if children == nil {
			children = []Category{}
		}
		out = append(out, Node{Category: p, Children: children})
	}
	return out, nil
}

// Detail returns one category by slug. For a child, group, item states,
// units and attributes resolve through the parent: the child's own
// attributes override same-keyed parent ones, the rest append.
func (s *Service) Detail(ctx context.Context, slug string) (Detail, error) {
	q := db.New(s.pool)
	row, err := q.GetCategoryBySlug(ctx, slug)
	if errors.Is(err, pgx.ErrNoRows) {
		return Detail{}, fmt.Errorf("%w: %s", ErrNotFound, slug)
	}
	if err != nil {
		return Detail{}, fmt.Errorf("get category: %w", err)
	}
	c := fromRow(row)
	holder := c
	var ref *ParentRef
	if c.ParentID != nil {
		prow, err := q.GetCategoryByID(ctx, *c.ParentID)
		if errors.Is(err, pgx.ErrNoRows) {
			return Detail{}, fmt.Errorf("parent of %s: %w", slug, ErrNotFound)
		}
		if err != nil {
			return Detail{}, fmt.Errorf("get parent category: %w", err)
		}
		pc := fromRow(prow)
		holder = pc
		ref = &ParentRef{ID: pc.ID, Name: pc.Name, Slug: pc.Slug}
	}
	group := ""
	if holder.Group != nil {
		group = *holder.Group
	}
	info, ok := Groups[group]
	if !ok {
		return Detail{}, fmt.Errorf("category %s has unknown group %q", slug, group)
	}
	attrs, err := s.resolvedAttributes(ctx, q, c, holder)
	if err != nil {
		return Detail{}, err
	}
	return Detail{
		Category: c, Parent: ref,
		ItemStates: append([]string{}, info.ItemStates...),
		Units:      append([]string{}, info.Units...),
		Attributes: attrs,
	}, nil
}

// resolvedAttributes merges holder (parent) attributes with the category's
// own, child keys winning in place, new child keys appended.
func (s *Service) resolvedAttributes(ctx context.Context, q *db.Queries, c, holder Category) ([]Attribute, error) {
	prows, err := q.ListAttributesByCategory(ctx, holder.ID)
	if err != nil {
		return nil, fmt.Errorf("list parent attributes: %w", err)
	}
	merged := make([]Attribute, 0, len(prows))
	at := map[string]int{}
	for _, r := range prows {
		at[r.Key] = len(merged)
		merged = append(merged, fromAttrRow(r))
	}
	if c.ID != holder.ID {
		orows, err := q.ListAttributesByCategory(ctx, c.ID)
		if err != nil {
			return nil, fmt.Errorf("list category attributes: %w", err)
		}
		for _, r := range orows {
			a := fromAttrRow(r)
			if i, ok := at[a.Key]; ok {
				merged[i] = a
			} else {
				at[a.Key] = len(merged)
				merged = append(merged, a)
			}
		}
	}
	return merged, nil
}

// CreateCategory creates a parent (listingGroup required) or a child
// (no group; the parent must itself be top-level). The slug is generated
// from the name with a parent prefix for children, unless given explicitly.
func (s *Service) CreateCategory(ctx context.Context, in CreateInput) (Detail, error) {
	q := db.New(s.pool)
	var verr validation.Error
	if n := utf8.RuneCountInString(in.Name); n < 2 || n > 80 {
		verr.Add("name", "must be 2-80 characters")
	}
	var parent *Category
	var group *string
	switch {
	case in.ParentID == nil:
		if in.ListingGroup == nil || !ValidGroup(*in.ListingGroup) {
			verr.Add("listingGroup", "is required for a parent category (equipment, quality, livestock, land, service)")
		} else {
			group = in.ListingGroup
		}
	default:
		if in.ListingGroup != nil {
			verr.Add("listingGroup", "must not be set on a child category (inherited from the parent)")
		}
		prow, err := q.GetCategoryByID(ctx, *in.ParentID)
		if errors.Is(err, pgx.ErrNoRows) {
			verr.Add("parentId", "parent category not found")
		} else if err != nil {
			return Detail{}, fmt.Errorf("get parent category: %w", err)
		} else if prow.ParentID.Valid {
			verr.Add("parentId", "parent must be a top-level category (two levels only)")
		} else {
			pc := fromRow(prow)
			parent = &pc
		}
	}
	slug := ""
	if in.Slug != nil {
		slug = *in.Slug
		if !slugPattern.MatchString(slug) {
			verr.Add("slug", "must match ^[a-z0-9]+(-[a-z0-9]+)*$")
		}
	} else if parent != nil {
		slug = parent.Slug + "-" + text.Slugify(in.Name)
	} else {
		slug = text.Slugify(in.Name)
	}
	if slug == "" {
		verr.Add("name", "yields an empty slug")
	}
	if err := verr.OrNil(); err != nil {
		return Detail{}, err
	}
	row, err := q.InsertCategory(ctx, db.InsertCategoryParams{
		Name: in.Name, Slug: slug, ParentID: pgUUIDPtr(in.ParentID),
		Icon: in.Icon, ListingGroup: group, SortOrder: in.SortOrder,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Detail{}, fmt.Errorf("%w: %s", ErrSlugTaken, slug)
	}
	if err != nil {
		return Detail{}, fmt.Errorf("insert category: %w", err)
	}
	return s.Detail(ctx, row.Slug)
}

// UpdateCategory patches name, icon, sort order and the active flag. The
// slug and group never change here. Deactivating hides the category (and its
// children) from public reads.
func (s *Service) UpdateCategory(ctx context.Context, id uuid.UUID, in UpdateInput) (Detail, error) {
	q := db.New(s.pool)
	var verr validation.Error
	if in.Name != nil {
		if n := utf8.RuneCountInString(*in.Name); n < 2 || n > 80 {
			verr.Add("name", "must be 2-80 characters")
		}
	}
	if err := verr.OrNil(); err != nil {
		return Detail{}, err
	}
	row, err := q.UpdateCategory(ctx, db.UpdateCategoryParams{
		Name: in.Name, Icon: in.Icon, SortOrder: in.SortOrder, IsActive: in.IsActive, ID: id,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Detail{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return Detail{}, fmt.Errorf("update category: %w", err)
	}
	return s.Detail(ctx, row.Slug)
}

// AddAttribute defines an attribute on any category (parents in the seed,
// children via the admin API). A taken key on the same category is a 409.
func (s *Service) AddAttribute(ctx context.Context, categoryID uuid.UUID, in AttributeInput) (Attribute, error) {
	q := db.New(s.pool)
	if _, err := q.GetCategoryByID(ctx, categoryID); errors.Is(err, pgx.ErrNoRows) {
		return Attribute{}, fmt.Errorf("%w: %s", ErrNotFound, categoryID)
	} else if err != nil {
		return Attribute{}, fmt.Errorf("get category: %w", err)
	}
	if err := validateAttribute(in.Key, in.Label, in.Type, in.Options); err != nil {
		return Attribute{}, err
	}
	options := in.Options
	if options == nil {
		options = []string{}
	}
	row, err := q.InsertAttribute(ctx, db.InsertAttributeParams{
		CategoryID: categoryID, Key: in.Key, Label: in.Label, Type: in.Type,
		Options: options, Required: in.Required, SortOrder: in.SortOrder,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Attribute{}, fmt.Errorf("%w: %s", ErrAttributeKeyTaken, in.Key)
	}
	if err != nil {
		return Attribute{}, fmt.Errorf("insert attribute: %w", err)
	}
	return fromAttrRow(row), nil
}

// UpdateAttribute patches an attribute scoped to its category. Switching the
// type away from select clears stale options; switching to select needs
// options (kept ones count).
func (s *Service) UpdateAttribute(ctx context.Context, categoryID, attributeID uuid.UUID, in AttributePatch) (Attribute, error) {
	q := db.New(s.pool)
	existing, err := q.GetCategoryAttribute(ctx, db.GetCategoryAttributeParams{ID: attributeID, CategoryID: categoryID})
	if errors.Is(err, pgx.ErrNoRows) {
		return Attribute{}, fmt.Errorf("%w: %s", ErrNotFound, attributeID)
	}
	if err != nil {
		return Attribute{}, fmt.Errorf("get attribute: %w", err)
	}
	attrType := existing.Type
	if in.Type != nil {
		attrType = *in.Type
	}
	options := existing.Options
	if in.HasOptions {
		options = in.Options
	} else if attrType != AttrSelect {
		options = []string{}
	}
	label := existing.Label
	if in.Label != nil {
		label = *in.Label
	}
	if err := validateAttribute(existing.Key, label, attrType, options); err != nil {
		return Attribute{}, err
	}
	required := existing.Required
	if in.Required != nil {
		required = *in.Required
	}
	sortOrder := existing.SortOrder
	if in.SortOrder != nil {
		sortOrder = *in.SortOrder
	}
	row, err := q.UpdateAttribute(ctx, db.UpdateAttributeParams{
		Label: &label, Type: &attrType, Options: options, Required: &required,
		SortOrder: &sortOrder, ID: attributeID, CategoryID: categoryID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Attribute{}, fmt.Errorf("%w: %s", ErrNotFound, attributeID)
	}
	if err != nil {
		return Attribute{}, fmt.Errorf("update attribute: %w", err)
	}
	return fromAttrRow(row), nil
}

// DeleteAttribute removes an attribute scoped to its category.
func (s *Service) DeleteAttribute(ctx context.Context, categoryID, attributeID uuid.UUID) error {
	_, err := db.New(s.pool).DeleteAttribute(ctx, db.DeleteAttributeParams{ID: attributeID, CategoryID: categoryID})
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrNotFound, attributeID)
	}
	if err != nil {
		return fmt.Errorf("delete attribute: %w", err)
	}
	return nil
}

// validateAttribute checks one attribute definition; failures name camelCase
// API fields.
func validateAttribute(key, label, attrType string, options []string) error {
	var verr validation.Error
	if !keyPattern.MatchString(key) {
		verr.Add("key", "must match ^[a-z][a-z0-9_]{1,40}$")
	}
	if n := utf8.RuneCountInString(label); n < 1 || n > 80 {
		verr.Add("label", "must be 1-80 characters")
	}
	switch attrType {
	case AttrText, AttrNumber, AttrBoolean, AttrSelect, AttrDate:
	default:
		verr.Add("type", "must be one of text, number, boolean, select, date")
	}
	if (attrType == AttrSelect) != (len(options) > 0) {
		verr.Add("options", "select needs options, other types must have none")
	}
	return verr.OrNil()
}

func lessCategory(a, b Category) bool {
	if a.SortOrder != b.SortOrder {
		return a.SortOrder < b.SortOrder
	}
	return a.Name < b.Name
}

func fromRow(r db.Category) Category {
	var parentID *uuid.UUID
	if r.ParentID.Valid {
		id := uuid.UUID(r.ParentID.Bytes)
		parentID = &id
	}
	return Category{
		ID: r.ID, ParentID: parentID,
		Name: r.Name, Slug: r.Slug, Icon: r.Icon, Group: r.ListingGroup,
		SortOrder: r.SortOrder, IsActive: r.IsActive,
	}
}

func fromAttrRow(r db.CategoryAttribute) Attribute {
	return Attribute{
		ID: r.ID, CategoryID: r.CategoryID, Key: r.Key, Label: r.Label,
		Type: r.Type, Options: append([]string{}, r.Options...),
		Required: r.Required, SortOrder: r.SortOrder,
	}
}

// pgUUIDPtr lifts an optional UUID into a nullable pgtype value.
func pgUUIDPtr(id *uuid.UUID) pgtype.UUID {
	if id == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: *id, Valid: true}
}
