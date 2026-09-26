package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/auth"
	"github.com/dezmymachine/farmish-backend/internal/auth/authtest"
	"github.com/dezmymachine/farmish-backend/internal/catalog"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/internal/users"
)

func catalogRouter(t *testing.T) (*gin.Engine, *pgxpool.Pool, *auth.Firebase) {
	t.Helper()
	fb := authtest.Firebase(t)
	pool := dbtest.Pool(t)
	if err := catalog.Seed(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	r := newTestRouter(t, Deps{DB: fakePinger{}, Verifier: fb, Users: users.New(pool), Catalog: catalog.New(pool)})
	return r, pool, fb
}

func TestCategoriesTree_MatchesSeed(t *testing.T) {
	r, _, _ := catalogRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/categories", nil)
	w := serve(t, r, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=300" {
		t.Errorf("Cache-Control = %q", cc)
	}
	var tree api.CategoryList
	if err := json.Unmarshal(w.Body.Bytes(), &tree); err != nil {
		t.Fatal(err)
	}
	if len(tree.Items) != 12 || tree.Items[0].Slug != "seeds-seedlings" || tree.Items[0].Name != "Seeds & Seedlings" {
		t.Fatalf("tree head = %+v", tree.Items)
	}
	seeds := tree.Items[0]
	if len(seeds.Children) != 7 || seeds.Children[0].Name != "Cereal Seeds" {
		t.Fatalf("seeds children = %+v", seeds.Children)
	}
	found := false
	for _, c := range seeds.Children {
		if c.Slug == "seeds-seedlings-flowers-ornamentals" {
			found = true
		}
		if len(c.Children) != 0 {
			t.Errorf("child %s has grandchildren", c.Slug)
		}
	}
	if !found {
		t.Error("prefixed child slug missing (legacy & bug back?)")
	}
	last := tree.Items[len(tree.Items)-1]
	if last.Slug != "irrigation" || len(last.Children) != 0 {
		t.Errorf("tree tail = %+v", last)
	}
}

func TestCategoryDetail_ChildInheritsGroupAndAttributes(t *testing.T) {
	r, _, _ := catalogRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/categories/livestock-poultry-cattle", nil)
	w := serve(t, r, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=300" {
		t.Errorf("Cache-Control = %q", cc)
	}
	var d api.CategoryDetail
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Parent == nil || d.Parent.Slug != "livestock-poultry" {
		t.Errorf("parent = %+v", d.Parent)
	}
	if d.Group != api.CategoryDetailGroupLivestock {
		t.Errorf("group = %q", d.Group)
	}
	has := func(ss []string, want string) bool {
		for _, s := range ss {
			if s == want {
				return true
			}
		}
		return false
	}
	if !has(d.ItemStates, "breeding_stock") {
		t.Errorf("itemStates = %v", d.ItemStates)
	}
	if !has(d.Units, "heads") || !has(d.Units, "pieces") {
		t.Errorf("units = %v", d.Units)
	}
	keys := map[string]bool{}
	for _, a := range d.Attributes {
		keys[a.Key] = true
	}
	for _, k := range []string{"breed", "vaccinated", "weight_kg"} {
		if !keys[k] {
			t.Errorf("parent attribute %q missing: %+v", k, d.Attributes)
		}
	}
}

func TestCategoryDetail_UnknownSlug404(t *testing.T) {
	r, _, _ := catalogRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/categories/no-such-slug", nil)
	w := serve(t, r, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d", w.Code)
	}
	assertErrorEnvelope(t, w.Body.Bytes(), apierror.CodeNotFound)
	assertContract(t, req, w)
}

func TestAdminCategories_CRUDAndGuards(t *testing.T) {
	r, pool, fb := catalogRouter(t)
	_, userTok := authUser(t, pool, fb)
	_, adminTok := authAdmin(t, pool, fb)

	// Non-admin: 403.
	w := serve(t, r, jsonRequest(http.MethodPost, "/v1/admin/categories", userTok, `{"name":"Test Parent","listingGroup":"equipment"}`))
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin create: %d %s", w.Code, w.Body.String())
	}

	// Create a parent: 201, generated slug.
	req := jsonRequest(http.MethodPost, "/v1/admin/categories", adminTok,
		`{"name":"Test Equipment","listingGroup":"equipment","icon":"🚜"}`)
	w = serve(t, r, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create parent: %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var parent api.CategoryDetail
	if err := json.Unmarshal(w.Body.Bytes(), &parent); err != nil || parent.Slug != "test-equipment" {
		t.Fatalf("parent = %+v, %v", parent, err)
	}

	// Create a child: prefixed slug, inherits group in detail.
	req = jsonRequest(http.MethodPost, "/v1/admin/categories", adminTok,
		`{"name":"Test Tillers","parentId":"`+parent.Id.String()+`"}`)
	w = serve(t, r, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create child: %d %s", w.Code, w.Body.String())
	}
	var child api.CategoryDetail
	if err := json.Unmarshal(w.Body.Bytes(), &child); err != nil ||
		child.Slug != "test-equipment-test-tillers" || child.Group != api.CategoryDetailGroupEquipment {
		t.Fatalf("child = %+v, %v", child, err)
	}

	// Child of a child: 400. Duplicate slug: 409.
	req = jsonRequest(http.MethodPost, "/v1/admin/categories", adminTok,
		`{"name":"Too Deep","parentId":"`+child.Id.String()+`"}`)
	w = serve(t, r, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("child of child: %d %s", w.Code, w.Body.String())
	}
	assertErrorEnvelope(t, w.Body.Bytes(), apierror.CodeValidationFailed)
	assertContract(t, req, w)

	req = jsonRequest(http.MethodPost, "/v1/admin/categories", adminTok,
		`{"name":"Irrigation","listingGroup":"equipment"}`)
	w = serve(t, r, req)
	if w.Code != http.StatusConflict {
		t.Errorf("duplicate slug: %d %s", w.Code, w.Body.String())
	}
	assertErrorEnvelope(t, w.Body.Bytes(), apierror.CodeConflict)
	assertContract(t, req, w)

	// PATCH rename + deactivate: hidden from the tree.
	req = jsonRequest(http.MethodPatch, "/v1/admin/categories/"+parent.Id.String(), adminTok,
		`{"name":"Renamed Equipment","isActive":false}`)
	w = serve(t, r, req)
	if w.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var patched api.CategoryDetail
	if err := json.Unmarshal(w.Body.Bytes(), &patched); err != nil ||
		patched.Slug != "test-equipment" || patched.Name != "Renamed Equipment" {
		t.Errorf("patched = %+v, %v", patched, err)
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/categories", nil)
	w = serve(t, r, req)
	var tree api.CategoryList
	if err := json.Unmarshal(w.Body.Bytes(), &tree); err != nil {
		t.Fatal(err)
	}
	for _, n := range tree.Items {
		if n.Slug == "test-equipment" {
			t.Error("deactivated parent still in the tree")
		}
	}

	// PATCH unknown id: 404.
	req = jsonRequest(http.MethodPatch, "/v1/admin/categories/"+uuid.NewString(), adminTok, `{"name":"Ghost"}`)
	w = serve(t, r, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("patch unknown: %d", w.Code)
	}
	assertErrorEnvelope(t, w.Body.Bytes(), apierror.CodeNotFound)
	assertContract(t, req, w)
}

func TestAdminAttributes_CRUD(t *testing.T) {
	r, pool, fb := catalogRouter(t)
	_, adminTok := authAdmin(t, pool, fb)

	// Child id from the seeded detail.
	w := serve(t, r, httptest.NewRequest(http.MethodGet, "/v1/categories/livestock-poultry-cattle", nil))
	var detail api.CategoryDetail
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	attrPath := "/v1/admin/categories/" + detail.Id.String() + "/attributes"

	// Create: 201. Duplicate key: 409.
	req := jsonRequest(http.MethodPost, attrPath, adminTok,
		`{"key":"horn_status","label":"Horn Status","type":"select","options":["Horned","Polled"]}`)
	w = serve(t, r, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create attr: %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var created api.CategoryAttribute
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	req = jsonRequest(http.MethodPost, attrPath, adminTok,
		`{"key":"horn_status","label":"Dupe","type":"text"}`)
	w = serve(t, r, req)
	if w.Code != http.StatusConflict {
		t.Errorf("duplicate key: %d %s", w.Code, w.Body.String())
	}
	assertErrorEnvelope(t, w.Body.Bytes(), apierror.CodeConflict)
	assertContract(t, req, w)

	// Bad definition: 400 with details.
	req = jsonRequest(http.MethodPost, attrPath, adminTok, `{"key":"x","label":"X","type":"select"}`)
	w = serve(t, r, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("select without options: %d %s", w.Code, w.Body.String())
	}
	assertErrorEnvelope(t, w.Body.Bytes(), apierror.CodeValidationFailed)

	// Patch the label: 200, and the detail merges it.
	onePath := attrPath + "/" + created.Id.String()
	req = jsonRequest(http.MethodPatch, onePath, adminTok, `{"label":"Horns"}`)
	w = serve(t, r, req)
	if w.Code != http.StatusOK {
		t.Fatalf("patch attr: %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	w = serve(t, r, httptest.NewRequest(http.MethodGet, "/v1/categories/livestock-poultry-cattle", nil))
	var merged api.CategoryDetail
	if err := json.Unmarshal(w.Body.Bytes(), &merged); err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{}
	for _, a := range merged.Attributes {
		labels[a.Key] = a.Label
	}
	if labels["horn_status"] != "Horns" || labels["breed"] != "Breed" {
		t.Errorf("merged = %+v", merged.Attributes)
	}

	// Scoped to the category: another category's attribute reads as missing.
	w = serve(t, r, httptest.NewRequest(http.MethodGet, "/v1/categories/fresh-produce-vegetables", nil))
	var veg api.CategoryDetail
	if err := json.Unmarshal(w.Body.Bytes(), &veg); err != nil {
		t.Fatal(err)
	}
	req = jsonRequest(http.MethodPatch, "/v1/admin/categories/"+veg.Id.String()+"/attributes/"+created.Id.String(), adminTok,
		`{"label":"Hijack"}`)
	if w := serve(t, r, req); w.Code != http.StatusNotFound {
		t.Errorf("cross-category patch: %d %s", w.Code, w.Body.String())
	}

	// Delete: 204, gone from the detail, second delete 404.
	req = plainRequest(http.MethodDelete, onePath, adminTok)
	w = serve(t, r, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	w = serve(t, r, httptest.NewRequest(http.MethodGet, "/v1/categories/livestock-poultry-cattle", nil))
	var after api.CategoryDetail
	if err := json.Unmarshal(w.Body.Bytes(), &after); err != nil {
		t.Fatal(err)
	}
	for _, a := range after.Attributes {
		if a.Key == "horn_status" {
			t.Errorf("deleted attribute still present: %+v", after.Attributes)
		}
	}
	req = plainRequest(http.MethodDelete, onePath, adminTok)
	if w := serve(t, r, req); w.Code != http.StatusNotFound {
		t.Errorf("second delete: %d", w.Code)
	}
}

// plainRequest builds a bodiless request (for DELETEs, which take no body).
func plainRequest(method, path, token string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

func TestLocations_SixteenRegions(t *testing.T) {
	r, _, _ := catalogRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/locations", nil)
	w := serve(t, r, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=86400" {
		t.Errorf("Cache-Control = %q", cc)
	}
	var loc api.Locations
	if err := json.Unmarshal(w.Body.Bytes(), &loc); err != nil {
		t.Fatal(err)
	}
	if len(loc.Regions) != 16 {
		t.Fatalf("regions = %d", len(loc.Regions))
	}
	byName := map[string][]string{}
	for _, rd := range loc.Regions {
		byName[rd.Name] = rd.Districts
	}
	if len(byName["Greater Accra"]) == 0 {
		t.Error("Greater Accra has no districts")
	}
	found := false
	for _, d := range byName["Ashanti"] {
		if d == "Asokwa" {
			found = true
		}
	}
	if !found {
		t.Error("Asokwa missing from Ashanti")
	}
}
