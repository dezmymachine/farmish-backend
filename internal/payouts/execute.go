package payouts

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/dezmymachine/farmish-backend/internal/db"
	"github.com/dezmymachine/farmish-backend/internal/ledger"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

// Payout statuses stored in payouts.status.
const (
	PayoutQueued   = "queued"
	PayoutPending  = "pending"
	PayoutSuccess  = "success"
	PayoutFailed   = "failed"
	PayoutReversed = "reversed"
)

var (
	// ErrPayoutNotFound means no payout matches, for admin lookups.
	ErrPayoutNotFound = errors.New("payout not found")
	// ErrAlreadyInFlight means the seller already has a payout running.
	ErrAlreadyInFlight = errors.New("a payout is already in flight for this seller")
	// ErrRetryNotAllowed means a payout cannot be retried: it never failed,
	// or there is nothing payable for a fresh attempt.
	ErrRetryNotAllowed = errors.New("only a failed or reversed payout may be retried")
)

// Payout is one row of the payouts table.
type Payout struct {
	ID            uuid.UUID
	SellerID      uuid.UUID
	AmountPesewas int64
	Reference     string
	RecipientCode string
	TransferCode  *string
	Status        string
	FailureReason *string
	SentAt        *time.Time
	CompletedAt   *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Balance is a seller's money split for the balance endpoint. All amounts
// are non-negative display values in pesewas.
type Balance struct {
	// Available is the payable balance: -Σ(seller_payable), floored at 0.
	Available int64
	// InEscrow is Σ over the seller's held orders of (base − refunded).
	InEscrow int64
	// InFlight is Σ over queued and pending payouts.
	InFlight int64
	// PaidOut is Σ over successful payouts.
	PaidOut int64
}

// newReference returns a PO- payout reference: 20 lowercase base32
// characters (100 bits) after the prefix. The UNIQUE(reference) constraint
// is the final guard against a collision.
func newReference() (string, error) {
	var buf [13]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate payout reference: %w", err)
	}
	encoded := strings.ToLower(base32.StdEncoding.EncodeToString(buf[:]))
	return "PO-" + encoded[:20], nil
}

// AttachLedger gives the service the ledger postings settle through.
// cmd/api calls it once, before any job can run.
func (s *Service) AttachLedger(l *ledger.Ledger) { s.ledger = l }

// Configure sets the payout floor and the absorbed transfer fee from
// config. cmd/api calls it once at startup.
func (s *Service) Configure(minPesewas, transferFeePesewas int64) {
	s.minPesewas = minPesewas
	s.transferFeePesewas = transferFeePesewas
}

// payableBalance returns the seller's payable display balance: the negated
// raw sum of seller_payable, floored at 0. A positive raw sum means the
// account was debited past zero, which the reconciler flags; it is never
// paid out.
func (s *Service) payableBalance(ctx context.Context, q db.DBTX, sellerID uuid.UUID) (int64, error) {
	raw, err := s.ledger.Balance(ctx, q, ledger.SellerPayable(sellerID))
	if err != nil {
		return 0, fmt.Errorf("sum seller payable: %w", err)
	}
	if raw >= 0 {
		return 0, nil
	}
	return -raw, nil
}

// Balance splits a seller's money for the balance endpoint.
func (s *Service) Balance(ctx context.Context, sellerID uuid.UUID) (Balance, error) {
	if s.ledger == nil {
		return Balance{}, fmt.Errorf("payouts: balance: ledger is not wired")
	}
	q := db.New(s.pool)
	available, err := s.payableBalance(ctx, s.pool, sellerID)
	if err != nil {
		return Balance{}, err
	}
	held, err := q.SumHeldRemainderBySeller(ctx, sellerID)
	if err != nil {
		return Balance{}, fmt.Errorf("sum held remainder: %w", err)
	}
	inFlight, err := q.SumPayoutsBySellerStatus(ctx, db.SumPayoutsBySellerStatusParams{
		SellerID: sellerID, Statuses: []string{PayoutQueued, PayoutPending},
	})
	if err != nil {
		return Balance{}, fmt.Errorf("sum in-flight payouts: %w", err)
	}
	paidOut, err := q.SumPayoutsBySellerStatus(ctx, db.SumPayoutsBySellerStatusParams{
		SellerID: sellerID, Statuses: []string{PayoutSuccess},
	})
	if err != nil {
		return Balance{}, fmt.Errorf("sum paid-out payouts: %w", err)
	}
	return Balance{Available: available, InEscrow: held, InFlight: inFlight, PaidOut: paidOut}, nil
}

// History returns the seller's payouts, newest first.
func (s *Service) History(ctx context.Context, sellerID uuid.UUID, limit, offset int32) ([]Payout, int64, error) {
	rows, err := db.New(s.pool).ListPayoutsBySeller(ctx, db.ListPayoutsBySellerParams{
		SellerID: sellerID, Limit: limit, Offset: offset,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("list payouts: %w", err)
	}
	total, err := db.New(s.pool).CountPayoutsBySeller(ctx, sellerID)
	if err != nil {
		return nil, 0, fmt.Errorf("count payouts: %w", err)
	}
	out := make([]Payout, 0, len(rows))
	for _, row := range rows {
		out = append(out, fromPayoutRow(row))
	}
	return out, total, nil
}

// AdminList returns every payout, newest first, with optional filters. Empty
// status lists all; nil seller lists all sellers.
func (s *Service) AdminList(ctx context.Context, status string, sellerID *uuid.UUID, limit, offset int32) ([]Payout, int64, error) {
	var statusFilter *string
	if status != "" {
		if !validPayoutStatus(status) {
			var invalid validation.Error
			invalid.Add("status", "must be queued, pending, success, failed or reversed")
			return nil, 0, invalid.OrNil()
		}
		statusFilter = &status
	}
	var sellerFilter pgtype.UUID
	if sellerID != nil {
		sellerFilter = pgtype.UUID{Bytes: *sellerID, Valid: true}
	}
	q := db.New(s.pool)
	rows, err := q.ListAdminPayouts(ctx, db.ListAdminPayoutsParams{
		Status: statusFilter, SellerID: sellerFilter, Limit: limit, Offset: offset,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("list payouts: %w", err)
	}
	total, err := q.CountAdminPayouts(ctx, db.CountAdminPayoutsParams{
		Status: statusFilter, SellerID: sellerFilter,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("count payouts: %w", err)
	}
	out := make([]Payout, 0, len(rows))
	for _, row := range rows {
		out = append(out, fromPayoutRow(row))
	}
	return out, total, nil
}

// validPayoutStatus mirrors the payouts CHECK.
func validPayoutStatus(status string) bool {
	switch status {
	case PayoutQueued, PayoutPending, PayoutSuccess, PayoutFailed, PayoutReversed:
		return true
	}
	return false
}

// fromPayoutRow maps the generated row onto the domain type.
func fromPayoutRow(r db.Payout) Payout {
	return Payout{
		ID: r.ID, SellerID: r.SellerID, AmountPesewas: r.AmountPesewas,
		Reference: r.Reference, RecipientCode: r.RecipientCode, TransferCode: r.TransferCode,
		Status: r.Status, FailureReason: r.FailureReason,
		SentAt: r.SentAt, CompletedAt: r.CompletedAt,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

// isUniqueViolation reports a PostgreSQL unique-constraint violation: a lost
// race against a concurrent execution inserting the same guarded row.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
