package ledger

import (
	"context"
	"fmt"
	"time"

	"github.com/dezmymachine/farmish-backend/internal/db"
)

// completedGrace is how long after completion an order may wait for its
// escrow_release posting before the reconciler calls it a violation: River
// enqueues the release in the completing transaction, but the worker posts it
// asynchronously.
const completedGrace = time.Hour

// Violation is one failed DOMAIN §5.4 invariant.
type Violation struct {
	// Check names the invariant: entries_balance, escrow_balance,
	// non_negative_balances, completed_have_release or processed_refunds_posted.
	Check string
	// Detail identifies the offending rows and amounts.
	Detail string
}

// Report is the reconciler's answer. Clean means no violation was found. The
// reconciler never mutates the ledger: it only detects.
type Report struct {
	CheckedAt time.Time
	// EscrowBalance is the raw signed sum of the escrow account.
	EscrowBalance int64
	// ExpectedEscrow is what the orders say is still held, negated to the
	// ledger's sign convention (credits are negative).
	ExpectedEscrow int64
	Violations     []Violation
}

// Clean reports whether the report holds no violation.
func (r Report) Clean() bool { return len(r.Violations) == 0 }

// Reconcile checks the DOMAIN §5.4 invariants against the current rows:
//
//  1. the signed entries sum to zero per currency
//  2. the escrow balance equals minus the held remainder over orders whose
//     escrow is held, refund-pending or partially refunded
//  3. no seller_payable or promo_credits account shows a negative (displayed)
//     balance
//  4. every completed order older than an hour has an escrow_release posting,
//     and every processed refund has an order_refund posting
//
// now is the clock, so tests never sleep for the completion grace window.
func (l *Ledger) Reconcile(ctx context.Context, q db.DBTX, now time.Time) (Report, error) {
	queries := db.New(q)
	report := Report{CheckedAt: now}

	sums, err := queries.SumLedgerEntriesByCurrency(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("sum ledger entries: %w", err)
	}
	for _, sum := range sums {
		if sum.Total != 0 {
			report.Violations = append(report.Violations, Violation{
				Check:  "entries_balance",
				Detail: fmt.Sprintf("currency %s sums to %d, want 0", sum.Currency, sum.Total),
			})
		}
	}

	balance, err := queries.SumLedgerAccountBalance(ctx, Escrow)
	if err != nil {
		return Report{}, fmt.Errorf("sum escrow balance: %w", err)
	}
	held, err := queries.SumHeldOrdersRemainder(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("sum held orders: %w", err)
	}
	report.EscrowBalance = balance
	report.ExpectedEscrow = -held
	if balance != -held {
		report.Violations = append(report.Violations, Violation{
			Check:  "escrow_balance",
			Detail: fmt.Sprintf("escrow %d, orders hold %d, want %d", balance, held, -held),
		})
	}

	positives, err := queries.ListPositiveDynamicBalances(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("list dynamic balances: %w", err)
	}
	for _, row := range positives {
		report.Violations = append(report.Violations, Violation{
			Check:  "non_negative_balances",
			Detail: fmt.Sprintf("account %s sums to %d, want <= 0", row.Code, row.Balance),
		})
	}

	cutoff := now.Add(-completedGrace)
	stale, err := queries.ListCompletedWithoutRelease(ctx, &cutoff)
	if err != nil {
		return Report{}, fmt.Errorf("list completed without release: %w", err)
	}
	for _, row := range stale {
		report.Violations = append(report.Violations, Violation{
			Check:  "completed_have_release",
			Detail: fmt.Sprintf("order %s completed without an escrow_release posting", row.ID),
		})
	}

	unposted, err := queries.ListProcessedRefundsWithoutPosting(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("list refunds without posting: %w", err)
	}
	for _, id := range unposted {
		report.Violations = append(report.Violations, Violation{
			Check:  "processed_refunds_posted",
			Detail: fmt.Sprintf("refund %s processed without an order_refund posting", id),
		})
	}

	return report, nil
}
