package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/auth/authtest"
	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/media"
	"github.com/dezmymachine/farmish-backend/internal/sellers"
)

// publishOne creates an active listing through the real API and returns its id
// and slug. Public browse tests need something actually browsable. extra is
// spliced into the request body, for the odd field a test needs.
func publishOne(t *testing.T, r http.Handler, pool *pgxpool.Pool, store *media.R2, token, title, extra string) (id, slug string) {
	t.Helper()
	mediaID := uploadImage(t, pool, store, token)
	body := strings.TrimSuffix(listingBody, "}") +
		`, "imageMediaIds": ["` + mediaID + `"], "title": "` + title + `"` + extra + `}`
	req := jsonRequest(http.MethodPost, "/v1/listings", token, body)
	w := serve(t, r, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create %q: %d %s", title, w.Code, w.Body.String())
	}
	var created api.SellerListing
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	req = jsonRequest(http.MethodPost, "/v1/me/listings/"+created.Id.String()+"/publish", token, "")
	w = serve(t, r, req)
	if w.Code != http.StatusOK {
		t.Fatalf("publish %q: %d %s", title, w.Code, w.Body.String())
	}
	return created.Id.String(), created.Slug
}

func TestSearch_EndpointContract(t *testing.T) {
	r, pool, store := listingRouter(t)
	tok := withProfile(t, pool, "search-seller")
	_, slug := publishOne(t, r, pool, store, tok, "Bulk Maize Consignment", "")

	cases := map[string]struct {
		path string
		bad  bool
	}{
		"plain":         {"/v1/listings", false},
		"with q":        {"/v1/listings?q=maize", false},
		"filtered":      {"/v1/listings?category=fresh-produce&region=Ashanti&itemState=grade_a", false},
		"price range":   {"/v1/listings?minPrice=100&maxPrice=900000000&sort=price_asc", false},
		"paged":         {"/v1/listings?page=1&limit=5&sort=newest", false},
		"relevance":     {"/v1/listings?q=maize&sort=relevance", false},
		"unknown sort":  {"/v1/listings?sort=nonsense", true},
		"limit too big": {"/v1/listings?limit=51", true},
		"inverted":      {"/v1/listings?minPrice=500&maxPrice=100", true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// No token: browse is public.
			req := jsonRequest(http.MethodGet, tc.path, "", "")
			w := serve(t, r, req)
			if tc.bad {
				if w.Code != http.StatusBadRequest {
					t.Fatalf("%s = %d %s, want 400", tc.path, w.Code, w.Body.String())
				}
				assertContract(t, req, w)
				return
			}
			if w.Code != http.StatusOK {
				t.Fatalf("%s = %d %s", tc.path, w.Code, w.Body.String())
			}
			assertContract(t, req, w)
			if got := w.Header().Get("Cache-Control"); got != "public, max-age=30" {
				t.Errorf("Cache-Control = %q", got)
			}
			if etag := w.Header().Get("ETag"); !strings.HasPrefix(etag, `W/"`) {
				t.Errorf("ETag = %q, want a weak validator", etag)
			}
			var body api.ListingSearchResult
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(tc.path, "limit=5") && body.Meta.Limit != 5 {
				t.Errorf("meta.limit = %d, want 5", body.Meta.Limit)
			}
			if strings.Contains(tc.path, "q=maize") {
				if len(body.Items) == 0 {
					t.Errorf("q=maize returned nothing, want %q", slug)
				} else if body.Items[0].Title != "Bulk Maize Consignment" {
					t.Errorf("first item = %q", body.Items[0].Title)
				}
			}
			if len(body.Items) > 0 {
				if body.Items[0].Price.Currency != api.MoneyCurrencyGHS {
					t.Errorf("price = %+v, want GHS pesewas", body.Items[0].Price)
				}
				if body.Items[0].Promoted != nil {
					t.Errorf("promoted = %+v, want null for an unpromoted listing", body.Items[0].Promoted)
				}
			}
		})
	}
}

// TestListingDetail_SafeProjection is the privacy guard for the public page:
// the decoded JSON must not carry any contact or identity field.
func TestListingDetail_SafeProjection(t *testing.T) {
	r, pool, store := listingRouter(t)
	tok := withProfile(t, pool, "detail-seller")
	id, slug := publishOne(t, r, pool, store, tok, "Healthy Friesian Heifer",
		`, "attributes": {"breed": "Friesian", "vaccinated": "true"}`)
	ctx := context.Background()

	// The seller's own contact details exist in the database; none of them may
	// surface on the public page.
	if _, err := pool.Exec(ctx, `UPDATE users
		SET phone_e164 = '+233201234567', email = 'seller@farmish.test'
		WHERE firebase_uid = $1`, "detail-seller"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE seller_profiles
		SET show_phone = true, show_whatsapp = true, whatsapp_e164 = '+233209876543'
		WHERE user_id = $1`, userIDFor(t, pool, tok)); err != nil {
		t.Fatal(err)
	}

	req := jsonRequest(http.MethodGet, "/v1/listings/"+slug, "", "")
	w := serve(t, r, req)
	if w.Code != http.StatusOK {
		t.Fatalf("detail: %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)

	body := w.Body.String()
	for _, forbidden := range []string{
		"phone", "email", "whatsapp", "firebaseUid", "firebase_uid", "role",
		"idNumber", "id_number", "sellerId", "phoneE164", "233201234567", "233209876543",
		"@farmish.test",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("public detail body contains %q:\n%s", forbidden, body)
		}
	}

	var detail api.ListingDetail
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Slug != slug || detail.Id.String() != id {
		t.Errorf("detail = %s / %s, want %s / %s", detail.Id, detail.Slug, id, slug)
	}
	// The seller projection is the Phase 8 one: a user id and a business name.
	if detail.Seller.UserId != userIDFor(t, pool, tok) || detail.Seller.BusinessName == "" {
		t.Errorf("seller = %+v", detail.Seller)
	}
	if len(detail.Images) == 0 || detail.Images[0].Url == "" {
		t.Fatalf("images = %+v", detail.Images)
	}
	if detail.CoverImageUrl == nil || *detail.CoverImageUrl != detail.Images[0].Url {
		t.Errorf("coverImageUrl = %v, want the first image", detail.CoverImageUrl)
	}
	if len(detail.Attributes) == 0 {
		t.Error("attributes = none, want the category's attributes")
	}
	for _, attr := range detail.Attributes {
		if attr.Label == "" || attr.Label == attr.Key {
			t.Errorf("attribute %q has no display label", attr.Key)
		}
	}
	if detail.ExpiresAt.Before(time.Now()) {
		t.Errorf("expiresAt = %s, want a future expiry", detail.ExpiresAt)
	}
	if got := w.Header().Get("Cache-Control"); got != "public, max-age=60" {
		t.Errorf("Cache-Control = %q", got)
	}

	// A listing that is no longer browsable is a 404, not a dead page.
	req = jsonRequest(http.MethodPost, "/v1/me/listings/"+id+"/archive", tok, "")
	if w := serve(t, r, req); w.Code != http.StatusOK {
		t.Fatalf("archive: %d %s", w.Code, w.Body.String())
	}
	req = jsonRequest(http.MethodGet, "/v1/listings/"+slug, "", "")
	w = serve(t, r, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("archived listing: %d, want 404", w.Code)
	}
	assertContract(t, req, w)
}

func TestListing_ETag304(t *testing.T) {
	r, pool, store := listingRouter(t)
	tok := withProfile(t, pool, "etag-seller")
	_, slug := publishOne(t, r, pool, store, tok, "Cached Cassava Tubers", "")

	for _, path := range []string{"/v1/listings/" + slug, "/v1/listings?limit=5"} {
		t.Run(path, func(t *testing.T) {
			req := jsonRequest(http.MethodGet, path, "", "")
			w := serve(t, r, req)
			if w.Code != http.StatusOK {
				t.Fatalf("first: %d %s", w.Code, w.Body.String())
			}
			etag := w.Header().Get("ETag")
			if etag == "" {
				t.Fatal("no ETag on the first response")
			}

			// A matching validator gets 304 and no body.
			req = jsonRequest(http.MethodGet, path, "", "")
			req.Header.Set("If-None-Match", etag)
			w = serve(t, r, req)
			if w.Code != http.StatusNotModified {
				t.Fatalf("revalidate: %d %s, want 304", w.Code, w.Body.String())
			}
			if w.Body.Len() != 0 {
				t.Errorf("304 body = %q, want empty", w.Body.String())
			}
			if got := w.Header().Get("ETag"); got != etag {
				t.Errorf("304 ETag = %q, want %q", got, etag)
			}

			// A stale validator gets the body again.
			req = jsonRequest(http.MethodGet, path, "", "")
			req.Header.Set("If-None-Match", `W/"0000000000000000"`)
			if w := serve(t, r, req); w.Code != http.StatusOK {
				t.Errorf("stale validator: %d, want 200", w.Code)
			}
			// `*` means "I already have a copy", so it is fresh too.
			req = jsonRequest(http.MethodGet, path, "", "")
			req.Header.Set("If-None-Match", "*")
			if w := serve(t, r, req); w.Code != http.StatusNotModified {
				t.Errorf("star validator: %d, want 304", w.Code)
			}
		})
	}
}

// ensureProfile gives an existing user a seller profile, for tests that signed
// in with a phone token rather than an email one.
func ensureProfile(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, name string) {
	t.Helper()
	if _, err := sellers.New(pool, nil, nil).UpsertMine(context.Background(), userID, sellers.ProfileInput{
		BusinessName: name + " farms", Region: "Ashanti", District: "Kumasi Metro",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestContactReveal_OptInOnly(t *testing.T) {
	r, pool, store := listingRouter(t)
	// A phone-authenticated seller, because the revealed number is the one
	// Firebase holds for the account: users.Service mirrors that claim, and
	// a token without one would clear it again on the next request.
	phoneAcct := authtest.PhoneUser(t)
	sellerID := userIDFor(t, pool, phoneAcct.Token)
	ensureProfile(t, pool, sellerID, "contact-seller")
	sellerTok := phoneAcct.Token
	buyerTok := withProfile(t, pool, "contact-buyer")
	id, _ := publishOne(t, r, pool, store, sellerTok, "Contactable Heifer", "")
	ctx := context.Background()

	phone := phoneAcct.Phone
	optIn := func(showPhone, showWhatsapp bool) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE seller_profiles
			SET show_phone = $2, show_whatsapp = $3, whatsapp_e164 = '+233209876543'
			WHERE user_id = $1`, sellerID, showPhone, showWhatsapp); err != nil {
			t.Fatal(err)
		}
	}
	call := func(token, listingID string) *httptest.ResponseRecorder {
		t.Helper()
		req := jsonRequest(http.MethodPost, "/v1/listings/"+listingID+"/contact", token, "")
		w := serve(t, r, req)
		assertContract(t, req, w)
		return w
	}
	contactCount := func() int32 {
		t.Helper()
		var n int32
		if err := pool.QueryRow(ctx,
			`SELECT contact_count FROM listings WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Anonymous is 401.
	if w := call("", id); w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous: %d %s, want 401", w.Code, w.Body.String())
	}
	// Your own listing is 403, even though you would pass the opt-in check.
	optIn(true, true)
	if w := call(sellerTok, id); w.Code != http.StatusForbidden {
		t.Errorf("own listing: %d %s, want 403", w.Code, w.Body.String())
	}

	// Opted out of everything: {} and the reveal is still counted.
	optIn(false, false)
	w := call(buyerTok, id)
	if w.Code != http.StatusOK {
		t.Fatalf("opted out: %d %s", w.Code, w.Body.String())
	}
	if got := strings.TrimSpace(w.Body.String()); got != "{}" {
		t.Errorf("opted out body = %q, want {}", got)
	}
	if n := contactCount(); n != 1 {
		t.Errorf("contact_count = %d, want 1", n)
	}

	// Opted in: the phone appears, WhatsApp does not.
	optIn(true, false)
	w = call(buyerTok, id)
	if w.Code != http.StatusOK {
		t.Fatalf("opted in: %d %s", w.Code, w.Body.String())
	}
	var contact api.ListingContact
	if err := json.Unmarshal(w.Body.Bytes(), &contact); err != nil {
		t.Fatal(err)
	}
	if contact.Phone == nil || *contact.Phone != phone {
		t.Errorf("phone = %v, want %s", contact.Phone, phone)
	}
	if contact.Whatsapp != nil {
		t.Errorf("whatsapp = %q, want withheld", *contact.Whatsapp)
	}
	if n := contactCount(); n != 2 {
		t.Errorf("contact_count = %d, want 2", n)
	}

	// A listing that is not browsable is a 404, and so is one that never
	// existed: neither may reveal anything.
	if _, err := pool.Exec(ctx, `UPDATE listings SET status = 'sold' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if w := call(buyerTok, id); w.Code != http.StatusNotFound {
		t.Errorf("sold listing: %d, want 404", w.Code)
	}
	if w := call(buyerTok, uuid.NewString()); w.Code != http.StatusNotFound {
		t.Errorf("unknown listing: %d, want 404", w.Code)
	}
	if n := contactCount(); n != 2 {
		t.Errorf("contact_count = %d, want 2: a refused reveal is not counted", n)
	}
}
