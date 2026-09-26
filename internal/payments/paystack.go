// Package payments holds the Paystack client and the payment service:
// creating a charge with the processing fee grossed up onto the buyer
// (DOMAIN §2.2), settling it from a signed webhook, and the verify
// fallback for when the webhook never arrives.
//
// Amounts cross the wire in pesewas, the subunit Paystack also uses, so
// nothing is multiplied by 100 anywhere in this package.
package payments

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Provider is the payment gateway this package talks to. The service depends
// on the interface, never on the HTTP client, so tests of *other* packages
// (checkout, promotions) can use payments/fake.
type Provider interface {
	InitializeTransaction(ctx context.Context, in InitializeInput) (InitializeResult, error)
	VerifyTransaction(ctx context.Context, reference string) (Transaction, error)
	CreateRefund(ctx context.Context, in RefundInput) (RefundResult, error)
	ResolveAccount(ctx context.Context, in ResolveInput) (Account, error)
	ListBanks(ctx context.Context, in ListBanksInput) ([]Bank, error)
	CreateTransferRecipient(ctx context.Context, in TransferRecipientInput) (TransferRecipient, error)
	InitiateTransfer(ctx context.Context, in TransferInput) (TransferResult, error)
	VerifyTransfer(ctx context.Context, reference string) (Transfer, error)
}

// DefaultTimeout bounds every call. Paystack is a third party: a hung request
// must not hold a database transaction or a job worker open.
const DefaultTimeout = 10 * time.Second

// maxResponseBytes caps a response body. A payment API answering with
// megabytes is either broken or hostile.
const maxResponseBytes = 1 << 20

// ErrProviderUnavailable means Paystack could not be reached or answered with
// a non-2xx status. Handlers answer 502: the buyer should retry, and no money
// moved.
var ErrProviderUnavailable = errors.New("paystack: unavailable")

// ErrRejected means Paystack answered, but with status:false. The message is
// Paystack's own and is safe to log (it never contains the secret).
var ErrRejected = errors.New("paystack: request rejected")

// InitializeInput starts a charge.
type InitializeInput struct {
	Email string
	// AmountPesewas is the charge in pesewas: base plus the grossed-up
	// processing fee.
	AmountPesewas int64
	// Reference is Farmish's own reference; Paystack echoes it back on every
	// later call and in the webhook.
	Reference   string
	CallbackURL string
	Metadata    map[string]any
	// Channels empty means Paystack's default. Farmish wants card and
	// mobile money.
	Channels []string
	// Currency is always GHS.
	Currency string
}

// InitializeResult is what the buyer needs to pay: the URL to send them to and
// the access code for the inline widget.
type InitializeResult struct {
	AuthorizationURL string
	AccessCode       string
	Reference        string
}

// Transaction is a verified charge.
type Transaction struct {
	// Status is Paystack's own: "success", "failed", "abandoned", "pending".
	Status string
	// AmountPesewas is the gross amount the buyer was charged.
	AmountPesewas int64
	Currency      string
	// FeesPesewas is what Paystack actually took.
	FeesPesewas int64
	Channel     string
	PaidAt      *time.Time
	Reference   string
	// ID is Paystack's numeric transaction id, used in the webhook event key.
	ID int64
}

// Successful reports whether Paystack says the money arrived.
func (t Transaction) Successful() bool { return t.Status == StatusSuccess }

// RefundInput returns money to a buyer. Used by Phase 17a.
type RefundInput struct {
	TransactionReference string
	AmountPesewas        int64
}

// RefundResult is Paystack's answer to a refund.
type RefundResult struct {
	RefundID int64
	Status   string
}

// ResolveInput looks up a payout account. Used by Phase 18a.
type ResolveInput struct {
	AccountNumber string
	BankCode      string
}

// Account is a resolved payout account.
type Account struct {
	AccountName string
}

// ListBanksInput lists payout banks. Used by Phase 18a.
type ListBanksInput struct {
	Currency string
	// Type is "mobile_money" or "ghipss".
	Type string
}

// Bank is one payout bank.
type Bank struct {
	Name string
	Code string
	Type string
}

// TransferRecipientInput creates a reusable payout destination. Phase 18a.
type TransferRecipientInput struct {
	// Type is "mobile_money" or "basilisk" (bank account).
	Type          string
	Name          string
	AccountNumber string
	BankCode      string
	Currency      string
}

// TransferRecipient is a created payout destination.
type TransferRecipient struct {
	RecipientCode string
}

// TransferInput sends money to a payout destination. Phase 18b.
type TransferInput struct {
	AmountPesewas int64
	RecipientCode string
	Reference     string
	Reason        string
}

// TransferResult is Paystack's answer to a transfer.
type TransferResult struct {
	TransferCode string
	Status       string
	// Reference is the reference Paystack recorded, which may differ from the
	// one sent if the provider was given a duplicate.
	Reference string
}

// Transfer is a verified transfer. Phase 18b.
type Transfer struct {
	Status         string
	TransferCode   string
	FailureReason  string
	Reference      string
	AmountPesewas  int64
	RecipientsName string
}

// PaystackStatus* are the payment statuses Farmish stores.
const (
	StatusPending   = "pending"
	StatusSuccess   = "success"
	StatusFailed    = "failed"
	StatusAbandoned = "abandoned"
)

// PaystackClient talks to Paystack's REST API.
//
// Paystack wraps every response in {"status":bool,"message":string,"data":…}.
// A non-2xx code or status:false is an error carrying Paystack's message; the
// secret key never appears in an error, because it only ever travels in a
// header.
type PaystackClient struct {
	baseURL string
	secret  string
	http    *http.Client
}

// Option customises a client. Tests use WithHTTPClient for timeouts and
// WithBaseURL to point at an httptest server.
type Option func(*PaystackClient)

// WithHTTPClient replaces the HTTP client.
func WithHTTPClient(c *http.Client) Option {
	return func(p *PaystackClient) {
		if c != nil {
			p.http = c
		}
	}
}

// NewPaystackClient returns a client for the API at baseURL (empty means
// Paystack's own). secret is the API key; it is never logged or returned.
func NewPaystackClient(secret, baseURL string, opts ...Option) *PaystackClient {
	if baseURL == "" {
		baseURL = "https://api.paystack.co"
	}
	p := &PaystackClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		secret:  secret,
		http:    &http.Client{Timeout: DefaultTimeout},
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// InitializeTransaction creates a charge and returns where to send the buyer.
func (p *PaystackClient) InitializeTransaction(ctx context.Context, in InitializeInput) (InitializeResult, error) {
	channels := in.Channels
	if len(channels) == 0 {
		channels = []string{"card", "mobile_money"}
	}
	currency := in.Currency
	if currency == "" {
		currency = "GHS"
	}
	body := map[string]any{
		"email":        in.Email,
		"amount":       in.AmountPesewas,
		"reference":    in.Reference,
		"callback_url": in.CallbackURL,
		"channels":     channels,
		"currency":     currency,
	}
	if len(in.Metadata) > 0 {
		body["metadata"] = in.Metadata
	}
	var data struct {
		AuthorizationURL string `json:"authorization_url"`
		AccessCode       string `json:"access_code"`
		Reference        string `json:"reference"`
	}
	if err := p.call(ctx, http.MethodPost, "/transaction/initialize", body, &data); err != nil {
		return InitializeResult{}, err
	}
	return InitializeResult{
		AuthorizationURL: data.AuthorizationURL,
		AccessCode:       data.AccessCode,
		Reference:        data.Reference,
	}, nil
}

// VerifyTransaction asks Paystack whether a charge succeeded. This is the
// fallback when no webhook arrived.
func (p *PaystackClient) VerifyTransaction(ctx context.Context, reference string) (Transaction, error) {
	var data struct {
		ID        int64      `json:"id"`
		Status    string     `json:"status"`
		Reference string     `json:"reference"`
		Amount    int64      `json:"amount"`
		Currency  string     `json:"currency"`
		Fees      int64      `json:"fees"`
		Channel   string     `json:"channel"`
		PaidAt    *time.Time `json:"paid_at"`
	}
	path := "/transaction/verify/" + url.PathEscape(reference)
	if err := p.call(ctx, http.MethodGet, path, nil, &data); err != nil {
		return Transaction{}, err
	}
	return Transaction{
		Status:        data.Status,
		AmountPesewas: data.Amount,
		Currency:      data.Currency,
		FeesPesewas:   data.Fees,
		Channel:       data.Channel,
		PaidAt:        data.PaidAt,
		Reference:     data.Reference,
		ID:            data.ID,
	}, nil
}

// CreateRefund returns money to a buyer. Phase 17a.
func (p *PaystackClient) CreateRefund(ctx context.Context, in RefundInput) (RefundResult, error) {
	var data struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
		Amount int64  `json:"amount"`
	}
	body := map[string]any{"reference": in.TransactionReference}
	if in.AmountPesewas > 0 {
		body["amount"] = in.AmountPesewas
	}
	if err := p.call(ctx, http.MethodPost, "/refund", body, &data); err != nil {
		return RefundResult{}, err
	}
	return RefundResult{RefundID: data.ID, Status: data.Status}, nil
}

// ResolveAccount validates a payout account and returns its holder's name.
// Phase 18a.
func (p *PaystackClient) ResolveAccount(ctx context.Context, in ResolveInput) (Account, error) {
	var data struct {
		AccountName string `json:"account_name"`
	}
	query := url.Values{"account_number": {in.AccountNumber}, "bank_code": {in.BankCode}}
	if err := p.call(ctx, http.MethodGet, "/bank/resolve?"+query.Encode(), nil, &data); err != nil {
		return Account{}, err
	}
	return Account{AccountName: data.AccountName}, nil
}

// ListBanks lists payout banks. Phase 18a.
func (p *PaystackClient) ListBanks(ctx context.Context, in ListBanksInput) ([]Bank, error) {
	currency := in.Currency
	if currency == "" {
		currency = "GHS"
	}
	query := url.Values{"currency": {currency}, "type": {in.Type}, "perPage": {"100"}}
	var data []struct {
		Name string `json:"name"`
		Code string `json:"code"`
		Type string `json:"type"`
	}
	if err := p.call(ctx, http.MethodGet, "/bank?"+query.Encode(), nil, &data); err != nil {
		return nil, err
	}
	out := make([]Bank, 0, len(data))
	for _, b := range data {
		out = append(out, Bank{Name: b.Name, Code: b.Code, Type: b.Type})
	}
	return out, nil
}

// CreateTransferRecipient creates a reusable payout destination. Phase 18a.
func (p *PaystackClient) CreateTransferRecipient(ctx context.Context, in TransferRecipientInput) (TransferRecipient, error) {
	currency := in.Currency
	if currency == "" {
		currency = "GHS"
	}
	body := map[string]any{
		"type":           in.Type,
		"name":           in.Name,
		"account_number": in.AccountNumber,
		"bank_code":      in.BankCode,
		"currency":       currency,
	}
	var data struct {
		RecipientCode string `json:"recipient_code"`
	}
	if err := p.call(ctx, http.MethodPost, "/transferrecipient", body, &data); err != nil {
		return TransferRecipient{}, err
	}
	return TransferRecipient{RecipientCode: data.RecipientCode}, nil
}

// InitiateTransfer sends money to a payout destination. Phase 18b.
func (p *PaystackClient) InitiateTransfer(ctx context.Context, in TransferInput) (TransferResult, error) {
	body := map[string]any{
		"amount":         in.AmountPesewas,
		"recipient_code": in.RecipientCode,
		"reference":      in.Reference,
		"reason":         in.Reason,
	}
	var data struct {
		TransferCode string `json:"transfer_code"`
		Status       string `json:"status"`
		Reference    string `json:"reference"`
	}
	if err := p.call(ctx, http.MethodPost, "/transfer", body, &data); err != nil {
		return TransferResult{}, err
	}
	return TransferResult{TransferCode: data.TransferCode, Status: data.Status, Reference: data.Reference}, nil
}

// VerifyTransfer asks Paystack about a transfer. Phase 18b.
func (p *PaystackClient) VerifyTransfer(ctx context.Context, reference string) (Transfer, error) {
	var data struct {
		Status         string `json:"status"`
		TransferCode   string `json:"transfer_code"`
		Reference      string `json:"reference"`
		Amount         int64  `json:"amount"`
		FailureReason  string `json:"reason"`
		RecipientsName string `json:"recipient_name"`
	}
	path := "/transfer/verify/" + url.PathEscape(reference)
	if err := p.call(ctx, http.MethodGet, path, nil, &data); err != nil {
		return Transfer{}, err
	}
	return Transfer{
		Status: data.Status, TransferCode: data.TransferCode, Reference: data.Reference,
		AmountPesewas: data.Amount, FailureReason: data.FailureReason,
		RecipientsName: data.RecipientsName,
	}, nil
}

// call performs one request and unwraps Paystack's envelope into out.
//
// Every failure path returns an error that names the operation and, when
// Paystack supplied one, its message. The secret key is only ever a header, so
// it cannot leak into an error.
func (p *PaystackClient) call(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("paystack: encode request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("paystack: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.secret)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := p.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %s %s: %w", ErrProviderUnavailable, method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("paystack: read response: %w", err)
	}

	var envelope struct {
		Status  bool            `json:"status"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	unmarshalErr := json.Unmarshal(raw, &envelope)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%w: %s %s: http %d: %s",
			ErrProviderUnavailable, method, path, resp.StatusCode, paystackMessage(envelope.Message, raw))
	}
	if unmarshalErr != nil {
		return fmt.Errorf("paystack: %s %s: unreadable response: %w", method, path, unmarshalErr)
	}
	if !envelope.Status {
		return fmt.Errorf("%w: %s %s: %s", ErrRejected, method, path, envelope.Message)
	}
	if out == nil {
		return nil
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return fmt.Errorf("paystack: %s %s: response carried no data", method, path)
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("paystack: %s %s: unreadable data: %w", method, path, err)
	}
	return nil
}

// paystackMessage prefers the parsed message and falls back to a truncated
// body, so an HTML error page from a proxy is still diagnosable in a log.
func paystackMessage(message string, raw []byte) string {
	if message != "" {
		return message
	}
	const limit = 200
	body := strings.TrimSpace(string(raw))
	if len(body) > limit {
		return body[:limit] + "…"
	}
	return body
}

// BaseURL returns the API root the client talks to. Exported for logging and
// tests; it is never a secret.
func (p *PaystackClient) BaseURL() string { return p.baseURL }

// Timeout returns the per-request timeout.
func (p *PaystackClient) Timeout() time.Duration {
	if p.http == nil {
		return 0
	}
	return p.http.Timeout
}
