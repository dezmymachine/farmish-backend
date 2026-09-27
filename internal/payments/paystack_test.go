package payments_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dezmymachine/farmish-backend/internal/payments"
)

// recorded is what the fake Paystack saw: enough to assert the request shape.
type recorded struct {
	Method string
	Path   string
	// Escaped is the path as it went on the wire: Path is the decoded form,
	// so only Escaped shows whether a reference was escaped.
	Escaped string
	Query   string
	Auth    string
	Accept  string
	Body    map[string]any
	Raw     string
}

// fakePaystack serves one canned envelope and records the request.
type fakePaystack struct {
	t        *testing.T
	status   int
	envelope string
	got      recorded
}

func newFake(t *testing.T, data string) *fakePaystack {
	t.Helper()
	return &fakePaystack{t: t, status: http.StatusOK, envelope: `{"status":true,"message":"ok","data":` + data + `}`}
}

func (f *fakePaystack) start() *httptest.Server {
	f.t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.got = recorded{
			Method: r.Method, Path: r.URL.Path, Escaped: r.URL.EscapedPath(),
			Query: r.URL.RawQuery, Auth: r.Header.Get("Authorization"),
			Accept: r.Header.Get("Accept"),
		}
		if r.Body != nil {
			raw := make([]byte, r.ContentLength)
			if r.ContentLength > 0 {
				_, _ = r.Body.Read(raw)
			}
			f.got.Raw = string(raw)
			_ = json.Unmarshal(raw, &f.got.Body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(f.envelope))
	}))
	f.t.Cleanup(srv.Close)
	return srv
}

func (f *fakePaystack) client(baseURL string) *payments.PaystackClient {
	return payments.NewPaystackClient("sk_test_secret", baseURL)
}

func TestPaystackClient_InitializeTransaction(t *testing.T) {
	f := newFake(t, `{"authorization_url":"https://checkout.paystack.com/abc","access_code":"acc_1","reference":"FMS-XYZ"}`)
	srv := f.start()
	got, err := f.client(srv.URL).InitializeTransaction(context.Background(), payments.InitializeInput{
		Email: "buyer@farmish.gh", AmountPesewas: 10199, Reference: "FMS-XYZ",
		CallbackURL: "https://farmish.gh/payments/status",
		Metadata:    map[string]any{"payment_id": "p1", "purpose": "checkout"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.AuthorizationURL != "https://checkout.paystack.com/abc" || got.AccessCode != "acc_1" || got.Reference != "FMS-XYZ" {
		t.Errorf("result = %+v", got)
	}
	if f.got.Method != http.MethodPost || f.got.Path != "/transaction/initialize" {
		t.Errorf("request = %s %s", f.got.Method, f.got.Path)
	}
	if f.got.Auth != "Bearer sk_test_secret" {
		t.Errorf("Authorization = %q, want the bearer secret", f.got.Auth)
	}
	if f.got.Accept != "application/json" {
		t.Errorf("Accept = %q", f.got.Accept)
	}
	// Amounts go over the wire in pesewas, unscaled (the spec's pitfall list).
	if f.got.Body["amount"] != float64(10199) {
		t.Errorf("amount = %v, want 10199 pesewas", f.got.Body["amount"])
	}
	if f.got.Body["email"] != "buyer@farmish.gh" || f.got.Body["reference"] != "FMS-XYZ" {
		t.Errorf("body = %v", f.got.Body)
	}
	if f.got.Body["callback_url"] != "https://farmish.gh/payments/status" {
		t.Errorf("callback_url = %v", f.got.Body["callback_url"])
	}
	if f.got.Body["currency"] != "GHS" {
		t.Errorf("currency = %v, want GHS", f.got.Body["currency"])
	}
	// Farmish wants card and mobile money by default.
	channels, _ := f.got.Body["channels"].([]any)
	if len(channels) != 2 || channels[0] != "card" || channels[1] != "mobile_money" {
		t.Errorf("channels = %v, want card and mobile_money", f.got.Body["channels"])
	}
	meta, _ := f.got.Body["metadata"].(map[string]any)
	if meta["purpose"] != "checkout" || meta["payment_id"] != "p1" {
		t.Errorf("metadata = %v", f.got.Body["metadata"])
	}
}

func TestPaystackClient_InitializeTransaction_ExplicitChannels(t *testing.T) {
	f := newFake(t, `{"authorization_url":"u","access_code":"a","reference":"r"}`)
	srv := f.start()
	if _, err := f.client(srv.URL).InitializeTransaction(context.Background(), payments.InitializeInput{
		AmountPesewas: 100, Channels: []string{"card"},
	}); err != nil {
		t.Fatal(err)
	}
	channels, _ := f.got.Body["channels"].([]any)
	if len(channels) != 1 || channels[0] != "card" {
		t.Errorf("channels = %v, want the caller's list", f.got.Body["channels"])
	}
	// An empty email and callback are the caller's business, not the client's.
	if _, ok := f.got.Body["email"]; !ok {
		t.Error("email missing from the body")
	}
}

func TestPaystackClient_VerifyTransaction(t *testing.T) {
	paidAt := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	f := newFake(t, `{"id":302961,"status":"success","reference":"FMS-XYZ","amount":10199,
		"currency":"GHS","fees":199,"channel":"card","paid_at":"`+paidAt.Format(time.RFC3339)+`"}`)
	srv := f.start()
	got, err := f.client(srv.URL).VerifyTransaction(context.Background(), "FMS-XYZ")
	if err != nil {
		t.Fatal(err)
	}
	if f.got.Method != http.MethodGet || f.got.Path != "/transaction/verify/FMS-XYZ" {
		t.Errorf("request = %s %s", f.got.Method, f.got.Path)
	}
	if got.ID != 302961 || !got.Successful() || got.AmountPesewas != 10199 || got.FeesPesewas != 199 {
		t.Errorf("transaction = %+v", got)
	}
	if got.Currency != "GHS" || got.Channel != "card" || got.Reference != "FMS-XYZ" {
		t.Errorf("transaction = %+v", got)
	}
	if got.PaidAt == nil || !got.PaidAt.Equal(paidAt) {
		t.Errorf("paidAt = %v, want %s", got.PaidAt, paidAt)
	}
}

func TestPaystackClient_VerifyTransaction_NotPaid(t *testing.T) {
	f := newFake(t, `{"id":1,"status":"failed","reference":"FMS-XYZ","amount":10199,
		"currency":"GHS","fees":0,"channel":"mobile_money"}`)
	srv := f.start()
	got, err := f.client(srv.URL).VerifyTransaction(context.Background(), "FMS-XYZ")
	if err != nil {
		t.Fatal(err)
	}
	if got.Successful() {
		t.Error("a failed transaction reported as successful")
	}
	if got.PaidAt != nil {
		t.Errorf("paidAt = %v, want nil for a failed charge", got.PaidAt)
	}
}

func TestPaystackClient_VerifyTransaction_EscapesReference(t *testing.T) {
	f := newFake(t, `{"id":1,"status":"pending","reference":"x","amount":1,"currency":"GHS"}`)
	srv := f.start()
	if _, err := f.client(srv.URL).VerifyTransaction(context.Background(), "FMS/../admin"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.got.Escaped, "FMS%2F..%2Fadmin") {
		t.Errorf("escaped path = %q, want the reference's slashes escaped", f.got.Escaped)
	}
}

func TestPaystackClient_CreateRefund(t *testing.T) {
	f := newFake(t, `{"id":900,"status":"queued","amount":5000}`)
	srv := f.start()
	got, err := f.client(srv.URL).CreateRefund(context.Background(), payments.RefundInput{
		TransactionReference: "FMS-XYZ", AmountPesewas: 5000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.got.Method != http.MethodPost || f.got.Path != "/refund" {
		t.Errorf("request = %s %s", f.got.Method, f.got.Path)
	}
	// Paystack's Create Refund names the transaction "transaction" (ADR-0027).
	if f.got.Body["transaction"] != "FMS-XYZ" || f.got.Body["amount"] != float64(5000) {
		t.Errorf("body = %v", f.got.Body)
	}
	if got.RefundID != 900 || got.Status != "queued" {
		t.Errorf("result = %+v", got)
	}

	// A full refund sends no amount at all.
	if _, err := f.client(srv.URL).CreateRefund(context.Background(),
		payments.RefundInput{TransactionReference: "FMS-XYZ"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.got.Body["amount"]; ok {
		t.Errorf("full refund sent amount = %v, want it omitted", f.got.Body["amount"])
	}
}

func TestPaystackClient_ResolveAccount(t *testing.T) {
	f := newFake(t, `{"account_name":"AMA SERWA","account_number":"1234567890"}`)
	srv := f.start()
	got, err := f.client(srv.URL).ResolveAccount(context.Background(), payments.ResolveInput{
		AccountNumber: "1234567890", BankCode: "mtn",
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.got.Method != http.MethodGet || f.got.Path != "/bank/resolve" {
		t.Errorf("request = %s %s", f.got.Method, f.got.Path)
	}
	if !strings.Contains(f.got.Query, "account_number=1234567890") || !strings.Contains(f.got.Query, "bank_code=mtn") {
		t.Errorf("query = %q", f.got.Query)
	}
	if got.AccountName != "AMA SERWA" {
		t.Errorf("account = %+v", got)
	}
}

func TestPaystackClient_ListBanks(t *testing.T) {
	f := newFake(t, `[{"name":"MTN Mobile Money","code":"mtn","type":"mobile_money"},
		{"name":"GCB Bank","code":"gcb","type":"ghipss"}]`)
	srv := f.start()
	got, err := f.client(srv.URL).ListBanks(context.Background(), payments.ListBanksInput{Type: "mobile_money"})
	if err != nil {
		t.Fatal(err)
	}
	if f.got.Method != http.MethodGet || f.got.Path != "/bank" {
		t.Errorf("request = %s %s", f.got.Method, f.got.Path)
	}
	if !strings.Contains(f.got.Query, "currency=GHS") || !strings.Contains(f.got.Query, "type=mobile_money") {
		t.Errorf("query = %q, want the GHS currency and the requested type", f.got.Query)
	}
	if len(got) != 2 || got[0].Name != "MTN Mobile Money" || got[1].Code != "gcb" || got[1].Type != "ghipss" {
		t.Errorf("banks = %+v", got)
	}
}

func TestPaystackClient_CreateTransferRecipient(t *testing.T) {
	f := newFake(t, `{"recipient_code":"RCP_1","type":"mobile_money"}`)
	srv := f.start()
	got, err := f.client(srv.URL).CreateTransferRecipient(context.Background(), payments.TransferRecipientInput{
		Type: "mobile_money", Name: "Ama Serwa", AccountNumber: "+233241234567", BankCode: "mtn",
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.got.Method != http.MethodPost || f.got.Path != "/transferrecipient" {
		t.Errorf("request = %s %s", f.got.Method, f.got.Path)
	}
	if f.got.Body["name"] != "Ama Serwa" || f.got.Body["bank_code"] != "mtn" || f.got.Body["currency"] != "GHS" {
		t.Errorf("body = %v", f.got.Body)
	}
	if got.RecipientCode != "RCP_1" {
		t.Errorf("recipient = %+v", got)
	}
}

func TestPaystackClient_InitiateTransfer(t *testing.T) {
	f := newFake(t, `{"transfer_code":"TRF_1","status":"queued","reference":"PO-1"}`)
	srv := f.start()
	got, err := f.client(srv.URL).InitiateTransfer(context.Background(), payments.TransferInput{
		AmountPesewas: 2000, RecipientCode: "RCP_1", Reference: "PO-1", Reason: "Payout for order 1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.got.Method != http.MethodPost || f.got.Path != "/transfer" {
		t.Errorf("request = %s %s", f.got.Method, f.got.Path)
	}
	if f.got.Body["amount"] != float64(2000) || f.got.Body["recipient_code"] != "RCP_1" {
		t.Errorf("body = %v", f.got.Body)
	}
	if got.TransferCode != "TRF_1" || got.Status != "queued" || got.Reference != "PO-1" {
		t.Errorf("result = %+v", got)
	}
}

func TestPaystackClient_VerifyTransfer(t *testing.T) {
	f := newFake(t, `{"status":"success","transfer_code":"TRF_1","reference":"PO-1",
		"amount":2000,"recipient_name":"Ama Serwa"}`)
	srv := f.start()
	got, err := f.client(srv.URL).VerifyTransfer(context.Background(), "PO-1")
	if err != nil {
		t.Fatal(err)
	}
	if f.got.Method != http.MethodGet || f.got.Path != "/transfer/verify/PO-1" {
		t.Errorf("request = %s %s", f.got.Method, f.got.Path)
	}
	if got.Status != "success" || got.TransferCode != "TRF_1" || got.AmountPesewas != 2000 {
		t.Errorf("transfer = %+v", got)
	}
}

func TestPaystackClient_StatusFalseIsAnError(t *testing.T) {
	for _, tt := range []struct {
		name     string
		status   int
		envelope string
		wantErr  error
	}{
		{"status false with 200", http.StatusOK, `{"status":false,"message":"Invalid key","data":null}`, payments.ErrRejected},
		{"status false on initialize", http.StatusOK, `{"status":false,"message":"Invalid reference","data":null}`, payments.ErrRejected},
		{"401 from the provider", http.StatusUnauthorized, `{"status":false,"message":"Invalid key","data":null}`, payments.ErrProviderUnavailable},
		{"500 from the provider", http.StatusInternalServerError, `{"message":"boom","data":null}`, payments.ErrProviderUnavailable},
		{"502 from a proxy", http.StatusBadGateway, `<html>bad gateway</html>`, payments.ErrProviderUnavailable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t, "{}")
			f.status, f.envelope = tt.status, tt.envelope
			srv := f.start()
			_, err := f.client(srv.URL).InitializeTransaction(context.Background(),
				payments.InitializeInput{AmountPesewas: 100, Reference: "FMS-XYZ"})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			// Paystack's own message helps diagnosis; the secret never appears.
			if tt.status == http.StatusOK && !strings.Contains(err.Error(), "Invalid") {
				t.Errorf("err = %v, want Paystack's message", err)
			}
			if strings.Contains(err.Error(), "sk_test_secret") {
				t.Errorf("err leaked the secret key: %v", err)
			}
		})
	}
}

func TestPaystackClient_UnreadableResponses(t *testing.T) {
	t.Run("not json", func(t *testing.T) {
		f := newFake(t, "{}")
		f.envelope = "not json at all"
		srv := f.start()
		if _, err := f.client(srv.URL).VerifyTransaction(context.Background(), "FMS-XYZ"); err == nil {
			t.Fatal("a non-JSON 200 was accepted")
		}
	})
	t.Run("no data field", func(t *testing.T) {
		f := newFake(t, "{}")
		f.envelope = `{"status":true,"message":"ok"}`
		srv := f.start()
		if _, err := f.client(srv.URL).VerifyTransaction(context.Background(), "FMS-XYZ"); err == nil {
			t.Fatal("a response with no data was accepted")
		}
	})
	t.Run("data of the wrong shape", func(t *testing.T) {
		f := newFake(t, "{}")
		f.envelope = `{"status":true,"message":"ok","data":"a string"}`
		srv := f.start()
		if _, err := f.client(srv.URL).VerifyTransaction(context.Background(), "FMS-XYZ"); err == nil {
			t.Fatal("a string in data was accepted as a transaction")
		}
	})
}

func TestPaystackClient_UnreachableProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now
	_, err := payments.NewPaystackClient("sk_test_secret", url).
		VerifyTransaction(context.Background(), "FMS-XYZ")
	if !errors.Is(err, payments.ErrProviderUnavailable) {
		t.Fatalf("err = %v, want ErrProviderUnavailable", err)
	}
}

// TestNewPaystackClient_Defaults pins the two defaults a test could otherwise
// silently rely on: Paystack's own host and a bounded timeout.
func TestNewPaystackClient_Defaults(t *testing.T) {
	c := payments.NewPaystackClient("sk_test_secret", "")
	if c.BaseURL() != "https://api.paystack.co" {
		t.Errorf("BaseURL = %q, want Paystack's API root", c.BaseURL())
	}
	if c.Timeout() != payments.DefaultTimeout {
		t.Errorf("Timeout = %s, want %s", c.Timeout(), payments.DefaultTimeout)
	}
	// A trailing slash on an override must not double up in request paths.
	trailing := payments.NewPaystackClient("sk_test_secret", "http://127.0.0.1:1/")
	if trailing.BaseURL() != "http://127.0.0.1:1" {
		t.Errorf("BaseURL = %q, want the trailing slash trimmed", trailing.BaseURL())
	}
}

func TestPaystackClient_FetchRefund(t *testing.T) {
	f := newFake(t, `{"id":900,"transaction_reference":"FMS-XYZ","amount":5000,"currency":"GHS","status":"processed","createdAt":"2026-09-27T10:00:00.000Z"}`)
	srv := f.start()
	got, err := f.client(srv.URL).FetchRefund(context.Background(), 900)
	if err != nil {
		t.Fatal(err)
	}
	if f.got.Method != http.MethodGet || f.got.Path != "/refund/900" {
		t.Errorf("request = %s %s", f.got.Method, f.got.Path)
	}
	if got.ID != 900 || got.TransactionReference != "FMS-XYZ" || got.AmountPesewas != 5000 ||
		got.Currency != "GHS" || got.Status != "processed" || got.CreatedAt.IsZero() {
		t.Errorf("refund = %+v", got)
	}
}

func TestPaystackClient_ListRefunds(t *testing.T) {
	f := newFake(t, `[{"id":1,"transaction_reference":"FMS-A","amount":100,"currency":"GHS","status":"pending","createdAt":"2026-09-27T10:00:00.000Z"},
	                  {"id":2,"transaction_reference":"FMS-B","amount":200,"currency":"GHS","status":"processed","createdAt":"2026-09-27T11:00:00.000Z"}]`)
	srv := f.start()
	from := time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC)
	got, err := f.client(srv.URL).ListRefunds(context.Background(), payments.ListRefundsInput{From: from, Page: 2, PerPage: 100})
	if err != nil {
		t.Fatal(err)
	}
	if f.got.Method != http.MethodGet || f.got.Path != "/refund" {
		t.Errorf("request = %s %s", f.got.Method, f.got.Path)
	}
	for _, want := range []string{"from=2026-09-27T09%3A00%3A00Z", "page=2", "perPage=100"} {
		if !strings.Contains(f.got.Query, want) {
			t.Errorf("query %q lacks %q", f.got.Query, want)
		}
	}
	if len(got) != 2 || got[1].ID != 2 || got[1].TransactionReference != "FMS-B" || got[1].AmountPesewas != 200 {
		t.Errorf("refunds = %+v", got)
	}
}

// A definite rejection proves nothing happened; everything else is ambiguous
// (ADR-0027). Refunds retry only after proving nothing happened.
func TestIsDefiniteRejection(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		envelope string
		want     bool
	}{
		{"status false", http.StatusOK, `{"status":false,"message":"Transaction has been fully reversed"}`, true},
		{"400", http.StatusBadRequest, `{"status":false,"message":"Invalid transaction"}`, true},
		{"404", http.StatusNotFound, `{"status":false,"message":"not found"}`, true},
		{"408 is ambiguous", http.StatusRequestTimeout, `{}`, false},
		{"429 is ambiguous", http.StatusTooManyRequests, `{}`, false},
		{"500 is ambiguous", http.StatusInternalServerError, `oops`, false},
		{"502 is ambiguous", http.StatusBadGateway, `<html>`, false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t, `{}`)
			f.status, f.envelope = tt.status, tt.envelope
			srv := f.start()
			_, err := f.client(srv.URL).CreateRefund(context.Background(), payments.RefundInput{TransactionReference: "FMS-X"})
			if err == nil {
				t.Fatal("want an error")
			}
			if got := payments.IsDefiniteRejection(err); got != tt.want {
				t.Errorf("IsDefiniteRejection(%v) = %v, want %v", err, got, tt.want)
			}
			if tt.status >= 300 && !errors.Is(err, payments.ErrProviderUnavailable) {
				t.Errorf("non-2xx must still wrap ErrProviderUnavailable for existing callers: %v", err)
			}
		})
	}
	if payments.IsDefiniteRejection(context.DeadlineExceeded) {
		t.Error("a timeout must be ambiguous")
	}
	dead := payments.NewPaystackClient("sk_test_secret", "http://127.0.0.1:1")
	if _, err := dead.CreateRefund(context.Background(), payments.RefundInput{TransactionReference: "FMS-X"}); err == nil || payments.IsDefiniteRejection(err) {
		t.Errorf("a transport error must be ambiguous: %v", err)
	}
}
