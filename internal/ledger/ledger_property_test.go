package ledger_test

import (
	"context"
	"fmt"
	"math/rand"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/database"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/ledger"
)

type propertyAccount struct {
	code     string
	currency string
}

// TestLedger_PropertyBalancesConsistent posts hundreds of randomly generated
// but always balanced transactions, then checks both global invariants: every
// currency still sums to zero, and every account balance equals the generated
// movement for that account.
func TestLedger_PropertyBalancesConsistent(t *testing.T) {
	const seed = 20260926
	rng := rand.New(rand.NewSource(seed))
	t.Logf("property-test seed: %d", seed)

	pool := dbtest.Pool(t)
	ctx := context.Background()
	accounts := propertyAccounts(t, pool)
	expected := make(map[string]int64, len(accounts))

	for i := range 500 {
		reference := fmt.Sprintf("property-%04d", i)
		first := accounts[i%len(accounts)]
		entries := randomBalancedEntries(rng, append([]propertyAccount{first}, accounts...))
		err := database.InTx(ctx, pool, func(tx pgx.Tx) error {
			return ledger.New().Post(ctx, tx, "property", reference, entries...)
		})
		if err != nil {
			t.Fatalf("post %s: %v", reference, err)
		}
		for _, entry := range entries {
			expected[entry.Account] += entry.Amount
		}
	}

	rows, err := pool.Query(ctx,
		`SELECT currency, COALESCE(SUM(amount), 0)::bigint
		 FROM ledger_entries GROUP BY currency`)
	if err != nil {
		t.Fatal(err)
	}
	totals := map[string]int64{}
	for rows.Next() {
		var currency string
		var total int64
		if err := rows.Scan(&currency, &total); err != nil {
			t.Fatal(err)
		}
		totals[currency] = total
	}
	rows.Close()
	if totals[ledger.CurrencyGHS] != 0 || totals[ledger.CurrencyCRD] != 0 {
		t.Errorf("global currency totals = %+v, want GHS and CRD both zero", totals)
	}
	for _, account := range accounts {
		got, err := ledger.New().Balance(ctx, pool, account.code)
		if err != nil {
			t.Fatal(err)
		}
		if got != expected[account.code] {
			t.Errorf("balance of %s = %d, want %d", account.code, got, expected[account.code])
		}
	}
}

// propertyAccounts returns 20 accounts: the nine fixed accounts, plus enough
// seller and promotion-credit owners to exercise lazy account creation.
func propertyAccounts(t *testing.T, pool *pgxpool.Pool) []propertyAccount {
	t.Helper()
	accounts := []propertyAccount{
		{ledger.PaystackClearing, ledger.CurrencyGHS},
		{ledger.Escrow, ledger.CurrencyGHS},
		{ledger.PayoutClearing, ledger.CurrencyGHS},
		{ledger.PlatformCommission, ledger.CurrencyGHS},
		{ledger.PromotionRevenue, ledger.CurrencyGHS},
		{ledger.ProcessingFeeIncome, ledger.CurrencyGHS},
		{ledger.PaystackFees, ledger.CurrencyGHS},
		{ledger.TransferFees, ledger.CurrencyGHS},
		{ledger.PromoCreditsIssued, ledger.CurrencyCRD},
	}
	for i := range 6 {
		user := mustUser(t, pool, fmt.Sprintf("ledger-property-seller-%02d", i))
		accounts = append(accounts, propertyAccount{ledger.SellerPayable(user), ledger.CurrencyGHS})
	}
	for i := range 5 {
		user := mustUser(t, pool, fmt.Sprintf("ledger-property-buyer-%02d", i))
		accounts = append(accounts, propertyAccount{ledger.PromoCredits(user), ledger.CurrencyCRD})
	}
	return accounts
}

// randomBalancedEntries returns two to four nonzero amounts over accounts of
// one currency. The first n-1 amounts are positive; the final amount closes
// the transaction, so every generated posting is balanced by construction.
func randomBalancedEntries(rng *rand.Rand, candidates []propertyAccount) []ledger.Entry {
	currency := candidates[0].currency
	eligible := make([]propertyAccount, 0, len(candidates))
	for _, account := range candidates {
		if account.currency == currency {
			eligible = append(eligible, account)
		}
	}
	n := 2 + rng.Intn(3)
	selected := append([]propertyAccount(nil), eligible...)
	rng.Shuffle(len(selected), func(i, j int) { selected[i], selected[j] = selected[j], selected[i] })
	selected = selected[:n]

	ceiling := 1000
	if currency == ledger.CurrencyCRD {
		ceiling = 100
	}
	entries := make([]ledger.Entry, 0, n)
	var running int64
	for i := range n - 1 {
		amount := int64(1 + rng.Intn(ceiling))
		running += amount
		entries = append(entries, ledger.Entry{
			Account: selected[i].code, Amount: amount, Currency: currency,
		})
	}
	return append(entries, ledger.Entry{
		Account: selected[n-1].code, Amount: -running, Currency: currency,
	})
}
