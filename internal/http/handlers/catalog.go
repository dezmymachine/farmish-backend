package handlers

import (
	"context"
	"errors"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/dezmymachine/farmish-backend/internal/catalog"
	"github.com/dezmymachine/farmish-backend/internal/geo"
	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/internal/users"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

// Cache lifetimes for public catalog reads.
const (
	catalogCacheControl   = "public, max-age=300"
	locationsCacheControl = "public, max-age=86400"
)

// CatalogStore is the part of catalog.Service the handlers use.
type CatalogStore interface {
	Tree(ctx context.Context) ([]catalog.Node, error)
	Detail(ctx context.Context, slug string) (catalog.Detail, error)
	CreateCategory(ctx context.Context, in catalog.CreateInput) (catalog.Detail, error)
	UpdateCategory(ctx context.Context, id uuid.UUID, in catalog.UpdateInput) (catalog.Detail, error)
	AddAttribute(ctx context.Context, categoryID uuid.UUID, in catalog.AttributeInput) (catalog.Attribute, error)
	UpdateAttribute(ctx context.Context, categoryID, attributeID uuid.UUID, in catalog.AttributePatch) (catalog.Attribute, error)
	DeleteAttribute(ctx context.Context, categoryID, attributeID uuid.UUID) error
}

// ListCategories serves the public category tree (Cache-Control: 5 min).
func (s Server) ListCategories(ctx context.Context, _ api.ListCategoriesRequestObject) (api.ListCategoriesResponseObject, error) {
	nodes, err := s.Catalog.Tree(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]api.CategoryNode, 0, len(nodes))
	for _, n := range nodes {
		items = append(items, toCategoryNode(n))
	}
	return api.ListCategories200JSONResponse{
		Body:    api.CategoryList{Items: items},
		Headers: api.ListCategories200ResponseHeaders{CacheControl: strptr(catalogCacheControl)},
	}, nil
}

// GetCategory serves one category by slug (Cache-Control: 5 min).
func (s Server) GetCategory(ctx context.Context, req api.GetCategoryRequestObject) (api.GetCategoryResponseObject, error) {
	d, err := s.Catalog.Detail(ctx, req.Slug)
	switch {
	case errors.Is(err, catalog.ErrNotFound):
		return api.GetCategory404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse(apierror.New(apierror.CodeNotFound, "Category not found")),
		}, nil
	case err != nil:
		return nil, err
	}
	return api.GetCategory200JSONResponse{
		Body:    toCategoryDetail(d),
		Headers: api.GetCategory200ResponseHeaders{CacheControl: strptr(catalogCacheControl)},
	}, nil
}

// GetLocations serves the 16 regions with district suggestions
// (Cache-Control: 1 day).
func (s Server) GetLocations(ctx context.Context, _ api.GetLocationsRequestObject) (api.GetLocationsResponseObject, error) {
	regions := make([]api.RegionDistricts, 0, len(geo.RegionDistricts))
	for _, rd := range geo.RegionDistricts {
		regions = append(regions, api.RegionDistricts{
			Name:      rd.Name,
			Districts: append([]string{}, rd.Districts...),
		})
	}
	return api.GetLocations200JSONResponse{
		Body:    api.Locations{Regions: regions},
		Headers: api.GetLocations200ResponseHeaders{CacheControl: strptr(locationsCacheControl)},
	}, nil
}

// CreateCategory creates a parent or child category.
func (s Server) CreateCategory(ctx context.Context, req api.CreateCategoryRequestObject) (api.CreateCategoryResponseObject, error) {
	if _, ok := users.FromContext(ctx); !ok {
		return nil, errNoUser
	}
	if req.Body == nil {
		return api.CreateCategory400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(apierror.New(apierror.CodeBadRequest, "Malformed request")),
		}, nil
	}
	d, err := s.Catalog.CreateCategory(ctx, toCreateInput(*req.Body))
	var verr *validation.Error
	switch {
	case errors.As(err, &verr):
		return api.CreateCategory400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(validationFailed(verr)),
		}, nil
	case errors.Is(err, catalog.ErrSlugTaken):
		return api.CreateCategory409JSONResponse{
			ConflictJSONResponse: api.ConflictJSONResponse(apierror.New(apierror.CodeConflict, "Category slug is taken")),
		}, nil
	case err != nil:
		return nil, err
	}
	return api.CreateCategory201JSONResponse(toCategoryDetail(d)), nil
}

// UpdateCategory patches a category.
func (s Server) UpdateCategory(ctx context.Context, req api.UpdateCategoryRequestObject) (api.UpdateCategoryResponseObject, error) {
	if _, ok := users.FromContext(ctx); !ok {
		return nil, errNoUser
	}
	if req.Body == nil {
		return api.UpdateCategory400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(apierror.New(apierror.CodeBadRequest, "Malformed request")),
		}, nil
	}
	d, err := s.Catalog.UpdateCategory(ctx, req.Id, toUpdateInput(*req.Body))
	var verr *validation.Error
	switch {
	case errors.As(err, &verr):
		return api.UpdateCategory400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(validationFailed(verr)),
		}, nil
	case errors.Is(err, catalog.ErrNotFound):
		return api.UpdateCategory404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse(apierror.New(apierror.CodeNotFound, "Category not found")),
		}, nil
	case err != nil:
		return nil, err
	}
	return api.UpdateCategory200JSONResponse(toCategoryDetail(d)), nil
}

// CreateCategoryAttribute adds an attribute to a category.
func (s Server) CreateCategoryAttribute(ctx context.Context, req api.CreateCategoryAttributeRequestObject) (api.CreateCategoryAttributeResponseObject, error) {
	if _, ok := users.FromContext(ctx); !ok {
		return nil, errNoUser
	}
	if req.Body == nil {
		return api.CreateCategoryAttribute400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(apierror.New(apierror.CodeBadRequest, "Malformed request")),
		}, nil
	}
	a, err := s.Catalog.AddAttribute(ctx, req.Id, toAttributeInput(*req.Body))
	var verr *validation.Error
	switch {
	case errors.As(err, &verr):
		return api.CreateCategoryAttribute400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(validationFailed(verr)),
		}, nil
	case errors.Is(err, catalog.ErrNotFound):
		return api.CreateCategoryAttribute404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse(apierror.New(apierror.CodeNotFound, "Category not found")),
		}, nil
	case errors.Is(err, catalog.ErrAttributeKeyTaken):
		return api.CreateCategoryAttribute409JSONResponse{
			ConflictJSONResponse: api.ConflictJSONResponse(apierror.New(apierror.CodeConflict, "Attribute key is taken on this category")),
		}, nil
	case err != nil:
		return nil, err
	}
	return api.CreateCategoryAttribute201JSONResponse(toCategoryAttribute(a)), nil
}

// UpdateCategoryAttribute patches an attribute scoped to its category.
func (s Server) UpdateCategoryAttribute(ctx context.Context, req api.UpdateCategoryAttributeRequestObject) (api.UpdateCategoryAttributeResponseObject, error) {
	if _, ok := users.FromContext(ctx); !ok {
		return nil, errNoUser
	}
	if req.Body == nil {
		return api.UpdateCategoryAttribute400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(apierror.New(apierror.CodeBadRequest, "Malformed request")),
		}, nil
	}
	a, err := s.Catalog.UpdateAttribute(ctx, req.Id, req.AttributeId, toAttributePatch(*req.Body))
	var verr *validation.Error
	switch {
	case errors.As(err, &verr):
		return api.UpdateCategoryAttribute400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(validationFailed(verr)),
		}, nil
	case errors.Is(err, catalog.ErrNotFound):
		return api.UpdateCategoryAttribute404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse(apierror.New(apierror.CodeNotFound, "Attribute not found")),
		}, nil
	case err != nil:
		return nil, err
	}
	return api.UpdateCategoryAttribute200JSONResponse(toCategoryAttribute(a)), nil
}

// DeleteCategoryAttribute removes an attribute scoped to its category.
func (s Server) DeleteCategoryAttribute(ctx context.Context, req api.DeleteCategoryAttributeRequestObject) (api.DeleteCategoryAttributeResponseObject, error) {
	if _, ok := users.FromContext(ctx); !ok {
		return nil, errNoUser
	}
	if err := s.Catalog.DeleteAttribute(ctx, req.Id, req.AttributeId); err != nil {
		if errors.Is(err, catalog.ErrNotFound) {
			return api.DeleteCategoryAttribute404JSONResponse{
				NotFoundJSONResponse: api.NotFoundJSONResponse(apierror.New(apierror.CodeNotFound, "Attribute not found")),
			}, nil
		}
		return nil, err
	}
	return api.DeleteCategoryAttribute204Response{}, nil
}

func toCategoryNode(n catalog.Node) api.CategoryNode {
	children := make([]api.CategoryNode, 0, len(n.Children))
	for _, c := range n.Children {
		children = append(children, api.CategoryNode{
			Id: c.ID, Name: c.Name, Slug: c.Slug, Icon: c.Icon,
			SortOrder: c.SortOrder, Children: []api.CategoryNode{},
		})
	}
	return api.CategoryNode{
		Id: n.ID, Name: n.Name, Slug: n.Slug, Icon: n.Icon,
		SortOrder: n.SortOrder, Children: children,
	}
}

func toCategoryDetail(d catalog.Detail) api.CategoryDetail {
	// Group is always set: parents carry it, children inherit it, and the
	// service errors on an unknown group.
	group := ""
	if d.Group != nil {
		group = *d.Group
	}
	out := api.CategoryDetail{
		Id: d.ID, Name: d.Name, Slug: d.Slug, Icon: d.Icon,
		Group: api.CategoryDetailGroup(group), ItemStates: d.ItemStates, Units: d.Units,
		Attributes: make([]api.CategoryAttribute, 0, len(d.Attributes)),
	}
	if d.Parent != nil {
		out.Parent = &struct {
			Id   openapi_types.UUID `json:"id"`
			Name string             `json:"name"`
			Slug string             `json:"slug"`
		}{Id: d.Parent.ID, Name: d.Parent.Name, Slug: d.Parent.Slug}
	}
	for _, a := range d.Attributes {
		out.Attributes = append(out.Attributes, toCategoryAttribute(a))
	}
	return out
}

func toCategoryAttribute(a catalog.Attribute) api.CategoryAttribute {
	return api.CategoryAttribute{
		Id: a.ID, Key: a.Key, Label: a.Label, Type: api.CategoryAttributeType(a.Type),
		Options: append([]string{}, a.Options...), Required: a.Required,
	}
}

func toCreateInput(body api.CreateCategoryJSONRequestBody) catalog.CreateInput {
	in := catalog.CreateInput{
		Name: body.Name, Slug: body.Slug, Icon: body.Icon,
		ParentID: body.ParentId, SortOrder: 0,
	}
	if body.ListingGroup != nil {
		g := string(*body.ListingGroup)
		in.ListingGroup = &g
	}
	if body.SortOrder != nil {
		in.SortOrder = *body.SortOrder
	}
	return in
}

func toUpdateInput(body api.UpdateCategoryJSONRequestBody) catalog.UpdateInput {
	return catalog.UpdateInput{
		Name: body.Name, Icon: body.Icon,
		SortOrder: body.SortOrder, IsActive: body.IsActive,
	}
}

func toAttributeInput(body api.CreateCategoryAttributeJSONRequestBody) catalog.AttributeInput {
	in := catalog.AttributeInput{
		Key: body.Key, Label: body.Label, Type: string(body.Type),
		Required: false,
	}
	if body.Options != nil {
		in.Options = *body.Options
	}
	if body.Required != nil {
		in.Required = *body.Required
	}
	return in
}

func toAttributePatch(body api.UpdateCategoryAttributeJSONRequestBody) catalog.AttributePatch {
	p := catalog.AttributePatch{
		Label: body.Label, Required: body.Required, SortOrder: body.SortOrder,
	}
	if body.Type != nil {
		t := string(*body.Type)
		p.Type = &t
	}
	if body.Options != nil {
		p.Options = *body.Options
		p.HasOptions = true
	}
	return p
}

func strptr(s string) *string { return &s }
