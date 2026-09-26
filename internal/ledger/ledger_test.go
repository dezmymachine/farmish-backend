package ledger_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/auth"
	"github.com/dezmymachine/farmish-backend/internal/database"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/db"
	"github.com/dezmymachine/farmish-backend/internal/ledger"
	"github.com/dezmymachine/farmish-backend/internal/users"
)

func mustUser(t *testing.T, pool *pgxpool.Pool, uid string) uuid.UUID {
	t.Helper()
	user, err := users.New(pool).Resolve(context.Background(), auth.Identity{
		UID: uid, Email: uid + "@farmish.test", Provider: "password",
	})
	if err != nil {
		t.Fatal(err)
	}
	return user.ID
}

func postInTx(t *testing.T, pool *pgxpool.Pool, kind, reference string, entries ...ledger.Entry) {
	t.Helper()
	err := database.InTx(context.Background(), pool, func(tx pgx.Tx) error {
		return ledger.New().Post(context.Background(), tx, kind, reference, entries...)
	})
	if err != nil {
		t.Fatalf("post %s/%s: %v", kind, reference, err)
	}
}

func countRows(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func mustBalance(t *testing.T, pool *pgxpool.Pool, account string, want int64) {
	t.Helper()
	got, err := ledger.New().Balance(context.Background(), pool, account)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("balance of %s = %d, want %d", account, got, want)
	}
}

func mustAppendOnlyError(t *testing.T, pool *pgxpool.Pool, query string, args ...any) {
	t.Helper()
	_, err := pool.Exec(context.Background(), query, args...)
	if err == nil || !strings.Contains(err.Error(), "is not allowed (append-only)") {
		t.Errorf("query %q: err = %v, want the append-only trigger error", query, err)
	}
}

func TestLedger_SeededFixedAccounts(t *testing.T) {
	pool := dbtest.Pool(t)
	want := map[string]struct {
		accountType string
		currency    string
	}{
		ledger.PaystackClearing:    {ledger.AccountAsset, ledger.CurrencyGHS},
		ledger.Escrow:              {ledger.AccountLiability, ledger.CurrencyGHS},
		ledger.PayoutClearing:      {ledger.AccountLiability, ledger.CurrencyGHS},
		ledger.PlatformCommission:  {ledger.AccountRevenue, ledger.CurrencyGHS},
		ledger.PromotionRevenue:    {ledger.AccountRevenue, ledger.CurrencyGHS},
		ledger.ProcessingFeeIncome: {ledger.AccountRevenue, ledger.CurrencyGHS},
		ledger.PaystackFees:        {ledger.AccountExpense, ledger.CurrencyGHS},
		ledger.TransferFees:        {ledger.AccountExpense, ledger.CurrencyGHS},
		ledger.PromoCreditsIssued:  {ledger.AccountEquity, ledger.CurrencyCRD},
	}
	for code, account := range want {
		row, err := db.New(pool).GetLedgerAccountByCode(context.Background(), code)
		if err != nil {
			t.Fatalf("get %s: %v", code, err)
		}
		if row.Type != account.accountType || row.Currency != account.currency || row.OwnerID.Valid {
			t.Errorf("%s = type %s currency %s owner valid %t, want %s %s no owner",
				code, row.Type, row.Currency, row.OwnerID.Valid, account.accountType, account.currency)
		}
	}
}

func TestPost_Balanced(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	seller := mustUser(t, pool, "balanced-seller")

	// The DOMAIN §2.2 example: base 10,000 grosses up to a 10,199 charge with a
	// 199 processing fee.
	first := uuid.New()
	second := uuid.New()
	postInTx(t, pool, ledger.KindCheckoutPaid, "balanced-checkout",
		ledger.Entry{Account: ledger.PaystackClearing, Amount: 10000, Currency: ledger.CurrencyGHS},
		ledger.Entry{Account: ledger.PaystackFees, Amount: 199, Currency: ledger.CurrencyGHS},
		ledger.Entry{Account: ledger.Escrow, Amount: -6000, Currency: ledger.CurrencyGHS, OrderID: &first},
		ledger.Entry{Account: ledger.Escrow, Amount: -4000, Currency: ledger.CurrencyGHS, OrderID: &second},
		ledger.Entry{Account: ledger.ProcessingFeeIncome, Amount: -199, Currency: ledger.CurrencyGHS},
	)
	postInTx(t, pool, ledger.KindEscrowRelease, "balanced-release",
		ledger.EscrowRelease(first, seller, 10000, 500)...,
	)

	mustBalance(t, pool, ledger.PaystackClearing, 10000)
	mustBalance(t, pool, ledger.PaystackFees, 199)
	mustBalance(t, pool, ledger.Escrow, 0)
	mustBalance(t, pool, ledger.ProcessingFeeIncome, -199)
	mustBalance(t, pool, ledger.SellerPayable(seller), -9500)
	mustBalance(t, pool, ledger.PlatformCommission, -500)

	account, err := db.New(pool).GetLedgerAccountByCode(ctx, ledger.SellerPayable(seller))
	if err != nil {
		t.Fatal(err)
	}
	if account.Type != ledger.AccountLiability || account.Currency != ledger.CurrencyGHS ||
		!account.OwnerID.Valid || uuid.UUID(account.OwnerID.Bytes) != seller {
		t.Errorf("lazy seller account = %+v, want a GHS liability owned by the seller", account)
	}
	if n := countRows(t, pool, `SELECT COUNT(*) FROM ledger_transactions`); n != 2 {
		t.Errorf("ledger_transactions = %d, want 2", n)
	}
	if n := countRows(t, pool, `SELECT COUNT(*) FROM ledger_entries`); n != 8 {
		t.Errorf("ledger_entries = %d, want 8", n)
	}
}

func TestPost_UnbalancedRejected(t *testing.T) {
	pool := dbtest.Pool(t)
	err := database.InTx(context.Background(), pool, func(tx pgx.Tx) error {
		return ledger.New().Post(context.Background(), tx, "unbalanced", "short",
			ledger.Entry{Account: ledger.PaystackClearing, Amount: 100, Currency: ledger.CurrencyGHS},
			ledger.Entry{Account: ledger.Escrow, Amount: -90, Currency: ledger.CurrencyGHS},
		)
	})
	if !errors.Is(err, ledger.ErrUnbalanced) {
		t.Fatalf("err = %v, want ErrUnbalanced", err)
	}
	if n := countRows(t, pool, `SELECT COUNT(*) FROM ledger_transactions WHERE kind = 'unbalanced'`); n != 0 {
		t.Errorf("unbalanced transaction rows = %d, want 0", n)
	}
	if n := countRows(t, pool, `SELECT COUNT(*) FROM ledger_entries`); n != 0 {
		t.Errorf("ledger_entries = %d, want 0", n)
	}
}

func TestPost_PerCurrencyBalance(t *testing.T) {
	pool := dbtest.Pool(t)
	user := mustUser(t, pool, "per-currency-user")
	postInTx(t, pool, "per-currency", "both",
		ledger.Entry{Account: ledger.PaystackClearing, Amount: 100, Currency: ledger.CurrencyGHS},
		ledger.Entry{Account: ledger.Escrow, Amount: -100, Currency: ledger.CurrencyGHS},
		ledger.Entry{Account: ledger.PromoCreditsIssued, Amount: 5, Currency: ledger.CurrencyCRD},
		ledger.Entry{Account: ledger.PromoCredits(user), Amount: -5, Currency: ledger.CurrencyCRD},
	)
	mustBalance(t, pool, ledger.PaystackClearing, 100)
	mustBalance(t, pool, ledger.Escrow, -100)
	mustBalance(t, pool, ledger.PromoCreditsIssued, 5)
	mustBalance(t, pool, ledger.PromoCredits(user), -5)

	err := database.InTx(context.Background(), pool, func(tx pgx.Tx) error {
		return ledger.New().Post(context.Background(), tx, "per-currency", "mixed",
			ledger.Entry{Account: ledger.PaystackClearing, Amount: 100, Currency: ledger.CurrencyGHS},
			ledger.Entry{Account: ledger.PromoCredits(user), Amount: -100, Currency: ledger.CurrencyCRD},
		)
	})
	if !errors.Is(err, ledger.ErrUnbalanced) {
		t.Fatalf("err = %v, want ErrUnbalanced", err)
	}
	if n := countRows(t, pool, `SELECT COUNT(*) FROM ledger_transactions WHERE reference = 'mixed'`); n != 0 {
		t.Errorf("mixed transaction rows = %d, want 0", n)
	}
}

func TestPost_CurrencyMismatchWithAccount(t *testing.T) {
	pool := dbtest.Pool(t)
	err := database.InTx(context.Background(), pool, func(tx pgx.Tx) error {
		return ledger.New().Post(context.Background(), tx, "currency-mismatch", "escrow-crd",
			ledger.Entry{Account: ledger.Escrow, Amount: 100, Currency: ledger.CurrencyGHS},
			ledger.Entry{Account: ledger.PaystackClearing, Amount: -100, Currency: ledger.CurrencyGHS},
			ledger.Entry{Account: ledger.PaystackClearing, Amount: 5, Currency: ledger.CurrencyCRD},
			ledger.Entry{Account: ledger.PaystackClearing, Amount: -5, Currency: ledger.CurrencyCRD},
		)
	})
	if err == nil || !strings.Contains(err.Error(), "does not match account") {
		t.Fatalf("err = %v, want the entry/account currency mismatch", err)
	}
	if n := countRows(t, pool, `SELECT COUNT(*) FROM ledger_transactions WHERE reference = 'escrow-crd'`); n != 0 {
		t.Errorf("mismatched transaction rows = %d, want 0", n)
	}
}

func TestPost_UnknownAccountRejected(t *testing.T) {
	pool := dbtest.Pool(t)
	err := database.InTx(context.Background(), pool, func(tx pgx.Tx) error {
		return ledger.New().Post(context.Background(), tx, "unknown-account", "no-such-code",
			ledger.Entry{Account: ledger.Escrow, Amount: 100, Currency: ledger.CurrencyGHS},
			ledger.Entry{Account: ledger.PaystackClearing, Amount: -100, Currency: ledger.CurrencyGHS},
			ledger.Entry{Account: "treasury", Amount: 50, Currency: ledger.CurrencyGHS},
			ledger.Entry{Account: "treasury", Amount: -50, Currency: ledger.CurrencyGHS},
		)
	})
	if err == nil || !strings.Contains(err.Error(), `unknown ledger account "treasury"`) {
		t.Fatalf("err = %v, want the unknown account", err)
	}
	if n := countRows(t, pool, `SELECT COUNT(*) FROM ledger_transactions WHERE reference = 'no-such-code'`); n != 0 {
		t.Errorf("unknown-account transaction rows = %d, want 0", n)
	}
}

func TestPost_Idempotent(t *testing.T) {
	pool := dbtest.Pool(t)
	postInTx(t, pool, ledger.KindCheckoutPaid, "duplicate-checkout",
		ledger.Entry{Account: ledger.PaystackClearing, Amount: 10, Currency: ledger.CurrencyGHS},
		ledger.Entry{Account: ledger.Escrow, Amount: -10, Currency: ledger.CurrencyGHS},
	)
	beforeTransactions := countRows(t, pool, `SELECT COUNT(*) FROM ledger_transactions`)
	beforeEntries := countRows(t, pool, `SELECT COUNT(*) FROM ledger_entries`)
	beforeClearing, err := ledger.New().Balance(context.Background(), pool, ledger.PaystackClearing)
	if err != nil {
		t.Fatal(err)
	}

	err = database.InTx(context.Background(), pool, func(tx pgx.Tx) error {
		return ledger.New().Post(context.Background(), tx, ledger.KindCheckoutPaid, "duplicate-checkout",
			ledger.Entry{Account: ledger.PaystackClearing, Amount: 10, Currency: ledger.CurrencyGHS},
			ledger.Entry{Account: ledger.Escrow, Amount: -10, Currency: ledger.CurrencyGHS},
		)
	})
	if !errors.Is(err, ledger.ErrDuplicate) {
		t.Fatalf("second post err = %v, want ErrDuplicate", err)
	}
	if got := countRows(t, pool, `SELECT COUNT(*) FROM ledger_transactions`); got != beforeTransactions {
		t.Errorf("ledger_transactions = %d, want %d", got, beforeTransactions)
	}
	if got := countRows(t, pool, `SELECT COUNT(*) FROM ledger_entries`); got != beforeEntries {
		t.Errorf("ledger_entries = %d, want %d", got, beforeEntries)
	}
	mustBalance(t, pool, ledger.PaystackClearing, beforeClearing)
}

func TestPost_RollsBackWithCallerTx(t *testing.T) {
	pool := dbtest.Pool(t)
	seller := mustUser(t, pool, "rollback-seller")
	sentinel := errors.New("caller rolled back")
	err := database.InTx(context.Background(), pool, func(tx pgx.Tx) error {
		if err := ledger.New().Post(context.Background(), tx, "rolled-back", "caller-aborted",
			ledger.Entry{Account: ledger.PaystackClearing, Amount: 100, Currency: ledger.CurrencyGHS},
			ledger.Entry{Account: ledger.SellerPayable(seller), Amount: -100, Currency: ledger.CurrencyGHS},
		); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the caller's rollback error", err)
	}
	if n := countRows(t, pool, `SELECT COUNT(*) FROM ledger_transactions WHERE reference = 'caller-aborted'`); n != 0 {
		t.Errorf("rolled-back transaction rows = %d, want 0", n)
	}
	if n := countRows(t, pool, `SELECT COUNT(*) FROM ledger_entries`); n != 0 {
		t.Errorf("ledger_entries = %d, want 0", n)
	}
	if _, err := db.New(pool).GetLedgerAccountByCode(context.Background(), ledger.SellerPayable(seller)); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("lazy account survived the rollback: err = %v", err)
	}
}

func TestLedger_AppendOnly(t *testing.T) {
	pool := dbtest.Pool(t)
	postInTx(t, pool, "append-only", "protected",
		ledger.Entry{Account: ledger.PaystackClearing, Amount: 100, Currency: ledger.CurrencyGHS},
		ledger.Entry{Account: ledger.PaystackFees, Amount: -100, Currency: ledger.CurrencyGHS},
	)
	mustAppendOnlyError(t, pool, `UPDATE ledger_accounts SET code = code WHERE code = $1`, ledger.PaystackClearing)
	mustAppendOnlyError(t, pool, `DELETE FROM ledger_accounts WHERE code = $1`, ledger.PaystackClearing)
	mustAppendOnlyError(t, pool, `UPDATE ledger_transactions SET kind = kind WHERE reference = 'protected'`)
	mustAppendOnlyError(t, pool, `DELETE FROM ledger_transactions WHERE reference = 'protected'`)
	mustAppendOnlyError(t, pool, `UPDATE ledger_entries SET amount = amount WHERE amount = 100`)
	mustAppendOnlyError(t, pool, `DELETE FROM ledger_entries WHERE amount = 100`)
}

func TestLedger_DeferredBalanceTrigger(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var transaction int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO ledger_transactions (kind, reference) VALUES ('unbalanced-raw', 'commit-check') RETURNING id`,
	).Scan(&transaction); err != nil {
		t.Fatal(err)
	}
	// Go validation is bypassed deliberately: this proves the database guard
	// fires even if a future caller reaches SQL another way.
	if _, err := tx.Exec(ctx,
		`INSERT INTO ledger_entries (transaction_id, account_id, amount, currency)
		 SELECT $1, id, $2, 'GHS' FROM ledger_accounts WHERE code = 'paystack_clearing'`,
		transaction, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO ledger_entries (transaction_id, account_id, amount, currency)
		 SELECT $1, id, $2, 'GHS' FROM ledger_accounts WHERE code = 'escrow'`,
		transaction, -90); err != nil {
		t.Fatal(err)
	}
	err = tx.Commit(ctx)
	if err == nil || !strings.Contains(err.Error(), "is unbalanced") {
		t.Fatalf("commit err = %v, want the deferred balance error", err)
	}
	if n := countRows(t, pool, `SELECT COUNT(*) FROM ledger_transactions WHERE reference = 'commit-check'`); n != 0 {
		t.Errorf("unbalanced transaction rows = %d, want 0", n)
	}
}
