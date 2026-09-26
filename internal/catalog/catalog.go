// Package catalog owns the two-level category tree, its typed attributes
// and the reference-data seed (DOMAIN §8–9).
package catalog

import (
	"errors"

	"github.com/google/uuid"
)

var (
	// ErrNotFound means no category (or attribute) matches.
	ErrNotFound = errors.New("category not found")
	// ErrSlugTaken means the slug is already used by another category.
	ErrSlugTaken = errors.New("category slug taken")
	// ErrAttributeKeyTaken means the key is already used on this category.
	ErrAttributeKeyTaken = errors.New("attribute key taken")
)

// Attribute types stored in category_attributes.type.
const (
	AttrText    = "text"
	AttrNumber  = "number"
	AttrBoolean = "boolean"
	AttrSelect  = "select"
	AttrDate    = "date"
)

// Category is a row of the categories table. Group (listing_group) is set on
// parents only; children inherit it.
type Category struct {
	ID        uuid.UUID
	ParentID  *uuid.UUID
	Name      string
	Slug      string
	Icon      *string
	Group     *string
	SortOrder int32
	IsActive  bool
}

// Attribute is a row of the category_attributes table.
type Attribute struct {
	ID         uuid.UUID
	CategoryID uuid.UUID
	Key        string
	Label      string
	Type       string
	Options    []string
	Required   bool
	SortOrder  int32
}

// Node is a tree node for the public list: parents carry children, children
// carry an empty slice.
type Node struct {
	Category
	Children []Category
}

// ParentRef identifies a child's parent in the detail view.
type ParentRef struct {
	ID   uuid.UUID
	Name string
	Slug string
}

// Detail is the single-category view. For a child, Group, ItemStates, Units
// and Attributes resolve through the parent (a child's own attributes
// override same-keyed parent ones).
type Detail struct {
	Category
	Parent     *ParentRef
	ItemStates []string
	Units      []string
	Attributes []Attribute
}

// CreateInput is the admin write model for a category.
type CreateInput struct {
	Name         string
	Slug         *string
	ParentID     *uuid.UUID
	Icon         *string
	ListingGroup *string
	SortOrder    int32
}

// UpdateInput is the admin patch model. Nil means unchanged.
type UpdateInput struct {
	Name      *string
	Icon      *string
	SortOrder *int32
	IsActive  *bool
}

// AttributeInput is the admin write model for an attribute.
type AttributeInput struct {
	Key       string
	Label     string
	Type      string
	Options   []string
	Required  bool
	SortOrder int32
}

// AttributePatch is the admin patch model. A nil Options keeps the column;
// a non-nil (even empty) Options replaces it.
type AttributePatch struct {
	Label     *string
	Type      *string
	Options   []string
	HasOptions bool
	Required  *bool
	SortOrder *int32
}
