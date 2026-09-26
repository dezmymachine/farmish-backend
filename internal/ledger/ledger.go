// Package ledger is Farmish's append-only, double-entry bookkeeping. Every
// business event that moves value is recorded as ledger entries whose amounts
// sum to zero separately for each currency. Balances are always derived by
// summing entries; there is no mutable balance column.
package ledger

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/dezmymachine/farmish-backend/internal/db"
)

// Currencies used by the ledger.
const (
	CurrencyGHS = "GHS"
	CurrencyCRD = "CRD"
)

// Fixed account codes from DOMAIN §5.2.
const (
	PaystackClearing    = "paystack_clearing"
	Escrow              = "escrow"
	PayoutClearing      = "payout_clearing"
	PlatformCommission  = "platform_commission"
	PromotionRevenue    = "promotion_revenue"
	ProcessingFeeIncome = "processing_fee_income"
	PaystackFees        = "paystack_fees"
	TransferFees        = "transfer_fees"
	PromoCreditsIssued  = "promo_credits_issued"
)

// Account types stored in ledger_accounts.type.
const (
	AccountAsset     = "asset"
	AccountLiability = "liability"
	AccountRevenue   = "revenue"
	AccountExpense   = "expense"
	AccountEquity    = "equity"
)

var (
	// ErrDuplicate means (kind, reference) was already posted. Callers treat it
	// as "already done": it is the ledger's idempotency contract.
	ErrDuplicate = errors.New("ledger transaction already posted")
	// ErrUnbalanced means the entries do not sum to zero for every currency in
	// the transaction.
	ErrUnbalanced = errors.New("ledger entries do not balance per currency")
)

// Entry is one side of a ledger transaction. Positive amounts are debits and
// negative amounts are credits.
type Entry struct {
	// Account is an account code, such as ledger.Escrow or
	// ledger.SellerPayable(sellerID).
	Account string
	// Amount is an integer count in the entry's currency: pesewas for GHS and
	// credits for CRD. No floats are ever used for ledger amounts.
	Amount   int64
	Currency string
	// OrderID links escrow entries to an order. Orders do not exist until
	// Phase 15a, so callers that do not have one leave it nil.
	OrderID *uuid.UUID
}

// SellerPayable returns the liability account for money owed to a seller.
func SellerPayable(id uuid.UUID) string {
	return "seller_payable:" + id.String()
}

// PromoCredits returns the liability account for a user's promotion credits.
func PromoCredits(id uuid.UUID) string {
	return "promo_credits:" + id.String()
}

// Ledger posts transactions and reads derived balances.
type Ledger struct{}

// New returns a Ledger. Post and Balance take the caller's database handle;
// the ledger owns no connection because every posting must join the business
// transaction that caused it.
func New() *Ledger {
	return &Ledger{}
}

// accountSpec is the row ledger.Post must find or create for an account code.
type accountSpec struct {
	Type     string
	Currency string
	Owner    uuid.UUID
	HasOwner bool
}

// fixedAccounts is the metadata for DOMAIN §5.2's fixed accounts. The rows
// themselves are seeded by the ledger migration; this table tells Post how to
// recognize a code that must already exist.
var fixedAccounts = map[string]accountSpec{
	PaystackClearing:    {Type: AccountAsset, Currency: CurrencyGHS},
	Escrow:              {Type: AccountLiability, Currency: CurrencyGHS},
	PayoutClearing:      {Type: AccountLiability, Currency: CurrencyGHS},
	PlatformCommission:  {Type: AccountRevenue, Currency: CurrencyGHS},
	PromotionRevenue:    {Type: AccountRevenue, Currency: CurrencyGHS},
	ProcessingFeeIncome: {Type: AccountRevenue, Currency: CurrencyGHS},
	PaystackFees:        {Type: AccountExpense, Currency: CurrencyGHS},
	TransferFees:        {Type: AccountExpense, Currency: CurrencyGHS},
	PromoCreditsIssued:  {Type: AccountEquity, Currency: CurrencyCRD},
}

// accountSpecFor resolves an account code without touching the database. The
// account type and currency are implied by the code: a fixed code has DOMAIN
// metadata, while a dynamic prefix determines its type, currency and owner.
func accountSpecFor(code string) (accountSpec, error) {
	if spec, ok := fixedAccounts[code]; ok {
		return spec, nil
	}
	if rest, ok := strings.CutPrefix(code, "seller_payable:"); ok {
		id, err := uuid.Parse(rest)
		if err != nil {
			return accountSpec{}, fmt.Errorf("unknown ledger account %q: %w", code, err)
		}
		return accountSpec{Type: AccountLiability, Currency: CurrencyGHS, Owner: id, HasOwner: true}, nil
	}
	if rest, ok := strings.CutPrefix(code, "promo_credits:"); ok {
		id, err := uuid.Parse(rest)
		if err != nil {
			return accountSpec{}, fmt.Errorf("unknown ledger account %q: %w", code, err)
		}
		return accountSpec{Type: AccountLiability, Currency: CurrencyCRD, Owner: id, HasOwner: true}, nil
	}
	return accountSpec{}, fmt.Errorf("unknown ledger account %q", code)
}

// Post writes one balanced transaction atomically in the caller's transaction.
//
// It validates the entries before anything is inserted, resolves each account,
// checks that every entry uses its account's currency, then inserts the
// transaction and its entries. If (kind, reference) was already posted, Post
// changes nothing and returns ErrDuplicate.
func (l *Ledger) Post(ctx context.Context, tx pgx.Tx, kind, reference string, entries ...Entry) error {
	if strings.TrimSpace(kind) == "" || strings.TrimSpace(reference) == "" {
		return fmt.Errorf("ledger transaction requires a kind and reference")
	}
	if err := checkEntries(entries); err != nil {
		return err
	}
	accounts, err := l.resolveAccounts(ctx, tx, entries)
	if err != nil {
		return err
	}
	for i, entry := range entries {
		if entry.Currency != accounts[entry.Account].Currency {
			return fmt.Errorf("ledger entry %d: currency %q does not match account %q",
				i, entry.Currency, entry.Account)
		}
	}

	q := db.New(tx)
	transaction, err := q.CreateLedgerTransaction(ctx, db.CreateLedgerTransactionParams{
		Kind: kind, Reference: reference,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: (%s, %s)", ErrDuplicate, kind, reference)
	}
	if err != nil {
		return fmt.Errorf("create ledger transaction: %w", err)
	}
	for i, entry := range entries {
		if _, err := q.CreateLedgerEntry(ctx, db.CreateLedgerEntryParams{
			TransactionID: transaction.ID,
			AccountID:     accounts[entry.Account].ID,
			Amount:        entry.Amount,
			Currency:      entry.Currency,
			OrderID:       optionalUUID(entry.OrderID),
		}); err != nil {
			return fmt.Errorf("create ledger entry %d: %w", i, err)
		}
	}
	return nil
}

// Balance returns an account's balance as the raw signed sum of its entries.
// It does not negate liability, revenue or equity accounts: callers that
// display a balance choose the sign convention.
func (l *Ledger) Balance(ctx context.Context, q db.DBTX, account string) (int64, error) {
	if strings.TrimSpace(account) == "" {
		return 0, fmt.Errorf("ledger balance requires an account code")
	}
	balance, err := db.New(q).SumLedgerAccountBalance(ctx, account)
	if err != nil {
		return 0, fmt.Errorf("sum ledger balance for %q: %w", account, err)
	}
	return balance, nil
}

// checkEntries validates one transaction before any row is written. Sums use
// arbitrary precision so a hostile or mistaken amount cannot wrap around and
// look balanced.
func checkEntries(entries []Entry) error {
	if len(entries) < 2 {
		return fmt.Errorf("ledger transaction requires at least two entries, got %d", len(entries))
	}
	sums := make(map[string]*big.Int)
	for i, entry := range entries {
		if strings.TrimSpace(entry.Account) == "" {
			return fmt.Errorf("ledger entry %d: account code is required", i)
		}
		if entry.Currency != CurrencyGHS && entry.Currency != CurrencyCRD {
			return fmt.Errorf("ledger entry %d: unknown currency %q", i, entry.Currency)
		}
		if entry.Amount == 0 {
			return fmt.Errorf("ledger entry %d: amount must not be zero", i)
		}
		sum, ok := sums[entry.Currency]
		if !ok {
			sum = big.NewInt(0)
			sums[entry.Currency] = sum
		}
		sum.Add(sum, big.NewInt(entry.Amount))
	}
	for currency, sum := range sums {
		if sum.Sign() != 0 {
			return fmt.Errorf("%w: currency %s sums to %s", ErrUnbalanced, currency, sum.String())
		}
	}
	return nil
}

// resolveAccounts returns every distinct account used by entries, creating
// dynamic accounts in the caller's transaction when they do not exist yet.
func (l *Ledger) resolveAccounts(ctx context.Context, tx pgx.Tx, entries []Entry) (map[string]db.LedgerAccount, error) {
	q := db.New(tx)
	accounts := make(map[string]db.LedgerAccount, len(entries))
	for _, entry := range entries {
		if _, ok := accounts[entry.Account]; ok {
			continue
		}
		spec, err := accountSpecFor(entry.Account)
		if err != nil {
			return nil, err
		}
		account, err := q.GetLedgerAccountByCode(ctx, entry.Account)
		if err == nil {
			accounts[entry.Account] = account
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("get ledger account %q: %w", entry.Account, err)
		}
		account, err = q.CreateLedgerAccount(ctx, db.CreateLedgerAccountParams{
			Code: entry.Account, Type: spec.Type, Currency: spec.Currency, OwnerID: optionalOwner(spec),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			// Another transaction created the account first.
			account, err = q.GetLedgerAccountByCode(ctx, entry.Account)
		}
		if err != nil {
			return nil, fmt.Errorf("create ledger account %q: %w", entry.Account, err)
		}
		accounts[entry.Account] = account
	}
	return accounts, nil
}

func optionalOwner(spec accountSpec) pgtype.UUID {
	if !spec.HasOwner {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: spec.Owner, Valid: true}
}

func optionalUUID(id *uuid.UUID) pgtype.UUID {
	if id == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: *id, Valid: true}
}
