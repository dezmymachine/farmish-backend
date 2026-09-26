package httpapi

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/media"
)

func publishQuoteListing(t *testing.T, r *gin.Engine, pool *pgxpool.Pool, store *media.R2, token, title string) uuid.UUID {
	t.Helper()
	id := createListing(t, r, token)
	mediaID := uploadImage(t, pool, store, token)
	req := jsonRequest(http.MethodPatch, "/v1/me/listings/"+id.String(), token,
		`{"title":`+strconv.Quote(title)+`,"imageMediaIds":["`+mediaID+`"]}`)
	w := serve(t, r, req)
	if w.Code != http.StatusOK {
		t.Fatalf("attach image: %d %s", w.Code, w.Body.String())
	}
	req = jsonRequest(http.MethodPost, "/v1/me/listings/"+id.String()+"/publish", token, "")
	w = serve(t, r, req)
	if w.Code != http.StatusOK {
		t.Fatalf("publish: %d %s", w.Code, w.Body.String())
	}
	return id
}

func TestQuoteEndpoint_Contract(t *testing.T) {
	r, pool, store := listingRouter(t)
	seller := withProfile(t, pool, "quote-seller")
	buyer := withProfile(t, pool, "quote-buyer")
	listing := publishQuoteListing(t, r, pool, store, seller, "Quotable Heifer")

	req := jsonRequest(http.MethodPost, "/v1/checkout/quote", buyer, `{
		"lines": [{"listingId": "`+listing.String()+`", "quantity": 2}],
		"delivery": [{"sellerId": "`+userIDFor(t, pool, seller).String()+`", "method": "pickup"}]
	}`)
	w := serve(t, r, req)
	if w.Code != http.StatusOK {
		t.Fatalf("quote: %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var quote api.CheckoutQuote
	if err := json.Unmarshal(w.Body.Bytes(), &quote); err != nil {
		t.Fatal(err)
	}
	if len(quote.Orders) != 1 || len(quote.Orders[0].Items) != 1 ||
		quote.Orders[0].Items[0].Quantity != 2 || quote.Orders[0].Items[0].LineTotal.Amount != 1700000 {
		t.Errorf("quote = %+v", quote)
	}

	raw := map[string]any{}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	order, _ := raw["orders"].([]any)[0].(map[string]any)
	for _, forbidden := range []string{"commission", "commissionRateBps", "commissionPesewas"} {
		if _, ok := order[forbidden]; ok {
			t.Errorf("quote exposes %q: %v", forbidden, order)
		}
	}

	req = jsonRequest(http.MethodPost, "/v1/checkout/quote", "", `{"lines": [], "delivery": []}`)
	w = serve(t, r, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous quote = %d %s, want 401", w.Code, w.Body.String())
	}
	assertContract(t, req, w)

	req = jsonRequest(http.MethodPost, "/v1/checkout/quote", buyer, `{
		"lines": [
			{"listingId": "`+listing.String()+`", "quantity": 1},
			{"listingId": "`+listing.String()+`", "quantity": 1}
		],
		"delivery": [{"sellerId": "`+userIDFor(t, pool, seller).String()+`", "method": "pickup"}]
	}`)
	w = serve(t, r, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("duplicate lines = %d %s, want 400", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
}

func TestPriceCart_NoClientPrices(t *testing.T) {
	fields := map[string]bool{}
	line := reflect.TypeOf(api.CheckoutCartLine{})
	for i := range line.NumField() {
		fields[line.Field(i).Tag.Get("json")] = true
	}
	if len(fields) != 2 || !fields["listingId"] || !fields["quantity"] {
		t.Errorf("CheckoutCartLine JSON fields = %v, want only listingId and quantity", fields)
	}
}
