// Package fake provides a programmable payments.Provider for tests of the
// packages that depend on payments: checkout and promotions. Payments' own
// tests use a real httptest Paystack instead, so the wire format is covered
// there.
package fake

import (
	"context"
	"sync"
	"time"

	"github.com/dezmymachine/farmish-backend/internal/payments"
)

// Provider is a scripted Paystack. Every call is recorded, and the answer for
// each method can be set or made to fail.
type Provider struct {
	mu sync.Mutex

	// InitResult and InitErr answer InitializeTransaction.
	InitResult payments.InitializeResult
	InitErr    error
	// VerifyResult and VerifyErr answer VerifyTransaction.
	VerifyResult payments.Transaction
	VerifyErr    error
	// RefundResult and RefundErr answer CreateRefund.
	RefundResult payments.RefundResult
	RefundErr    error
	// Refunded keeps every RefundInput passed to CreateRefund.
	Refunded []payments.RefundInput
	// RefundErrAfterRecord makes CreateRefund create the refund at "Paystack"
	// and then fail anyway, the ambiguous case: a lost response to a request
	// that took effect (ADR-0027).
	RefundErrAfterRecord error
	// Refunds is the fake Paystack's refund store, filled by CreateRefund and
	// read by FetchRefund/ListRefunds. Tests may edit statuses with
	// SetRefundStatus.
	Refunds []payments.Refund
	// FetchErr and ListErr make FetchRefund/ListRefunds fail.
	FetchErr error
	ListErr  error
	// Now stamps created refunds; nil means time.Now.
	Now func() time.Time

	// Calls records the order methods were called in.
	Calls []string
	// Initialized keeps every InitializeInput, so a test can assert on the
	// amount, reference and metadata that were sent.
	Initialized []payments.InitializeInput
	// Verified keeps every reference passed to VerifyTransaction.
	Verified []string
}

// New returns a provider that initializes successfully.
func New() *Provider {
	return &Provider{
		InitResult: payments.InitializeResult{
			AuthorizationURL: "https://checkout.paystack.com/fake",
			AccessCode:       "fake_access",
			Reference:        "FMS-FAKE",
		},
	}
}

// InitializeTransaction implements payments.Provider.
func (p *Provider) InitializeTransaction(ctx context.Context, in payments.InitializeInput) (payments.InitializeResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Calls = append(p.Calls, "InitializeTransaction")
	p.Initialized = append(p.Initialized, in)
	if p.InitErr != nil {
		return payments.InitializeResult{}, p.InitErr
	}
	out := p.InitResult
	if out.Reference == "" {
		out.Reference = in.Reference
	}
	return out, nil
}

// VerifyTransaction implements payments.Provider.
func (p *Provider) VerifyTransaction(ctx context.Context, reference string) (payments.Transaction, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Calls = append(p.Calls, "VerifyTransaction")
	p.Verified = append(p.Verified, reference)
	if p.VerifyErr != nil {
		return payments.Transaction{}, p.VerifyErr
	}
	out := p.VerifyResult
	if out.Reference == "" {
		out.Reference = reference
	}
	return out, nil
}

// CreateRefund implements payments.Provider.
func (p *Provider) CreateRefund(ctx context.Context, in payments.RefundInput) (payments.RefundResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Calls = append(p.Calls, "CreateRefund")
	p.Refunded = append(p.Refunded, in)
	if p.RefundErr != nil {
		return payments.RefundResult{}, p.RefundErr
	}
	out := p.RefundResult
	if out.RefundID == 0 && out.Status == "" {
		// A distinct id per call, so multiple refunds in one test never collide
		// on the real schema's UNIQUE(paystack_refund_id).
		out = payments.RefundResult{RefundID: int64(len(p.Refunded)), Status: "pending"}
	}
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	p.Refunds = append(p.Refunds, payments.Refund{
		ID: out.RefundID, TransactionReference: in.TransactionReference, AmountPesewas: in.AmountPesewas,
		Currency: "GHS", Status: out.Status, CreatedAt: now(),
	})
	if p.RefundErrAfterRecord != nil {
		return payments.RefundResult{}, p.RefundErrAfterRecord
	}
	return out, nil
}

// FetchRefund implements payments.Provider.
func (p *Provider) FetchRefund(ctx context.Context, id int64) (payments.Refund, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Calls = append(p.Calls, "FetchRefund")
	if p.FetchErr != nil {
		return payments.Refund{}, p.FetchErr
	}
	for _, r := range p.Refunds {
		if r.ID == id {
			return r, nil
		}
	}
	return payments.Refund{}, payments.ErrRejected
}

// ListRefunds implements payments.Provider: one page holding every refund
// created on or after in.From.
func (p *Provider) ListRefunds(ctx context.Context, in payments.ListRefundsInput) ([]payments.Refund, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Calls = append(p.Calls, "ListRefunds")
	if p.ListErr != nil {
		return nil, p.ListErr
	}
	if in.Page > 1 {
		return nil, nil
	}
	var out []payments.Refund
	for _, r := range p.Refunds {
		if !r.CreatedAt.Before(in.From) {
			out = append(out, r)
		}
	}
	return out, nil
}

// SetRefundStatus changes a stored refund's status, as Paystack settling it
// would.
func (p *Provider) SetRefundStatus(id int64, status string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.Refunds {
		if p.Refunds[i].ID == id {
			p.Refunds[i].Status = status
		}
	}
}

// ResolveAccount implements payments.Provider.
func (p *Provider) ResolveAccount(ctx context.Context, in payments.ResolveInput) (payments.Account, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Calls = append(p.Calls, "ResolveAccount")
	return payments.Account{AccountName: "FAKE HOLDER"}, nil
}

// ListBanks implements payments.Provider.
func (p *Provider) ListBanks(ctx context.Context, in payments.ListBanksInput) ([]payments.Bank, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Calls = append(p.Calls, "ListBanks")
	return []payments.Bank{{Name: "Fake Bank", Code: "fake", Type: in.Type}}, nil
}

// CreateTransferRecipient implements payments.Provider.
func (p *Provider) CreateTransferRecipient(ctx context.Context, in payments.TransferRecipientInput) (payments.TransferRecipient, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Calls = append(p.Calls, "CreateTransferRecipient")
	return payments.TransferRecipient{RecipientCode: "RCP_FAKE"}, nil
}

// InitiateTransfer implements payments.Provider.
func (p *Provider) InitiateTransfer(ctx context.Context, in payments.TransferInput) (payments.TransferResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Calls = append(p.Calls, "InitiateTransfer")
	return payments.TransferResult{TransferCode: "TRF_FAKE", Status: "queued", Reference: in.Reference}, nil
}

// VerifyTransfer implements payments.Provider.
func (p *Provider) VerifyTransfer(ctx context.Context, reference string) (payments.Transfer, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Calls = append(p.Calls, "VerifyTransfer")
	return payments.Transfer{Status: "success", Reference: reference}, nil
}

// CallCount returns how many times a method was called.
func (p *Provider) CallCount(method string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.Calls {
		if c == method {
			n++
		}
	}
	return n
}
