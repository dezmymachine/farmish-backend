package ledger_test

import (
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/ledger"
)

func mustPostTemplate(t *testing.T, kind, reference string, entries []ledger.Entry) {
	t.Helper()
	pool := dbtest.Pool(t)
	buyer := mustUser(t, pool, reference+"-buyer")
	seller := mustUser(t, pool, reference+"-seller")
	posted := make([]ledger.Entry, 0, len(entries))
	for _, entry := range entries {
		if entry.OrderID != nil {
			orderID := mustOrder(t, pool, buyer, seller)
			entry.OrderID = &orderID
		}
		posted = append(posted, entry)
	}
	postInTx(t, pool, kind, reference, posted...)
}

func TestPostingTemplates_CheckoutPaidAndEscrowRelease(t *testing.T) {
	first, second := uuid.New(), uuid.New()

	// The DOMAIN §2.2 example: a 10,000 base grosses up to a 10,199 charge with
	// a 199 processing fee, here split across two orders.
	got := ledger.CheckoutPaid(10199, 10000, 199,
		ledger.CheckoutOrder{OrderID: first, Base: 6000},
		ledger.CheckoutOrder{OrderID: second, Base: 4000},
	)
	want := []ledger.Entry{
		{Account: ledger.PaystackClearing, Amount: 10000, Currency: ledger.CurrencyGHS},
		{Account: ledger.PaystackFees, Amount: 199, Currency: ledger.CurrencyGHS},
		{Account: ledger.Escrow, Amount: -6000, Currency: ledger.CurrencyGHS, OrderID: &first},
		{Account: ledger.Escrow, Amount: -4000, Currency: ledger.CurrencyGHS, OrderID: &second},
		{Account: ledger.ProcessingFeeIncome, Amount: -199, Currency: ledger.CurrencyGHS},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CheckoutPaid() = %+v, want %+v", got, want)
	}
	mustPostTemplate(t, ledger.KindCheckoutPaid, "template-checkout", got)

	// The DOMAIN §2.1 example: a 12,345 subtotal at 500bps earns 617.
	pool := dbtest.Pool(t)
	buyer := mustUser(t, pool, "template-buyer")
	seller := mustUser(t, pool, "template-seller")
	order := mustOrder(t, pool, buyer, seller)
	got = ledger.EscrowRelease(order, seller, 12345, 617)
	want = []ledger.Entry{
		{Account: ledger.Escrow, Amount: 12345, Currency: ledger.CurrencyGHS, OrderID: &order},
		{Account: ledger.SellerPayable(seller), Amount: -11728, Currency: ledger.CurrencyGHS, OrderID: &order},
		{Account: ledger.PlatformCommission, Amount: -617, Currency: ledger.CurrencyGHS, OrderID: &order},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("EscrowRelease() = %+v, want %+v", got, want)
	}
	postInTx(t, pool, ledger.KindEscrowRelease, "template-release", got...)
}

func TestPostingTemplates_RefundsAndPromotions(t *testing.T) {
	pool := dbtest.Pool(t)
	order := uuid.New()
	user := mustUser(t, pool, "template-buyer")

	// The DOMAIN §4.1 partial-refund amount: 2,500 moves back to clearing.
	got := ledger.OrderRefund(order, 2500)
	want := []ledger.Entry{
		{Account: ledger.Escrow, Amount: 2500, Currency: ledger.CurrencyGHS, OrderID: &order},
		{Account: ledger.PaystackClearing, Amount: -2500, Currency: ledger.CurrencyGHS, OrderID: &order},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("OrderRefund() = %+v, want %+v", got, want)
	}
	mustPostTemplate(t, ledger.KindOrderRefund, "template-refund", got)

	// DOMAIN §6's vip tier: 7,500 price buys 75 credits. The 7,650 charge leaves
	// a 150 processing fee, while the 198 actual webhook fee is booked to Paystack.
	got = ledger.PromotionPaid(7500, 7650, 198, 75, user)
	want = []ledger.Entry{
		{Account: ledger.PaystackClearing, Amount: 7452, Currency: ledger.CurrencyGHS},
		{Account: ledger.PaystackFees, Amount: 198, Currency: ledger.CurrencyGHS},
		{Account: ledger.PromotionRevenue, Amount: -7500, Currency: ledger.CurrencyGHS},
		{Account: ledger.ProcessingFeeIncome, Amount: -150, Currency: ledger.CurrencyGHS},
		{Account: ledger.PromoCreditsIssued, Amount: 75, Currency: ledger.CurrencyCRD},
		{Account: ledger.PromoCredits(user), Amount: -75, Currency: ledger.CurrencyCRD},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PromotionPaid() = %+v, want %+v", got, want)
	}
	postInTx(t, pool, ledger.KindPromotionPaid, "template-promotion-paid", got...)

	got = ledger.PromotionApplied(user, 75)
	want = []ledger.Entry{
		{Account: ledger.PromoCredits(user), Amount: 75, Currency: ledger.CurrencyCRD},
		{Account: ledger.PromoCreditsIssued, Amount: -75, Currency: ledger.CurrencyCRD},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PromotionApplied() = %+v, want %+v", got, want)
	}
	postInTx(t, pool, ledger.KindPromotionApplied, "template-promotion-applied", got...)
}

func TestPostingTemplates_Payouts(t *testing.T) {
	pool := dbtest.Pool(t)
	seller := mustUser(t, pool, "template-payout-seller")

	// The DOMAIN §4.1 seller net: 7,600 payable after a 2,500 partial refund.
	got := ledger.PayoutInitiated(seller, 7600)
	want := []ledger.Entry{
		{Account: ledger.SellerPayable(seller), Amount: 7600, Currency: ledger.CurrencyGHS},
		{Account: ledger.PayoutClearing, Amount: -7600, Currency: ledger.CurrencyGHS},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PayoutInitiated() = %+v, want %+v", got, want)
	}
	postInTx(t, pool, ledger.KindPayoutInitiated, "template-payout-initiated", got...)

	got = ledger.PayoutSucceeded(7600, 0)
	want = []ledger.Entry{
		{Account: ledger.PayoutClearing, Amount: 7600, Currency: ledger.CurrencyGHS},
		{Account: ledger.PaystackClearing, Amount: -7600, Currency: ledger.CurrencyGHS},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PayoutSucceeded() without a fee = %+v, want %+v", got, want)
	}
	mustPostTemplate(t, ledger.KindPayoutSucceeded, "template-payout-no-fee", got)

	got = ledger.PayoutSucceeded(7600, 25)
	want = []ledger.Entry{
		{Account: ledger.PayoutClearing, Amount: 7600, Currency: ledger.CurrencyGHS},
		{Account: ledger.PaystackClearing, Amount: -7600, Currency: ledger.CurrencyGHS},
		{Account: ledger.TransferFees, Amount: 25, Currency: ledger.CurrencyGHS},
		{Account: ledger.PaystackClearing, Amount: -25, Currency: ledger.CurrencyGHS},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PayoutSucceeded() with a fee = %+v, want %+v", got, want)
	}
	mustPostTemplate(t, ledger.KindPayoutSucceeded, "template-payout-fee", got)

	got = ledger.PayoutFailed(seller, 7600)
	want = []ledger.Entry{
		{Account: ledger.PayoutClearing, Amount: 7600, Currency: ledger.CurrencyGHS},
		{Account: ledger.SellerPayable(seller), Amount: -7600, Currency: ledger.CurrencyGHS},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PayoutFailed() = %+v, want %+v", got, want)
	}
	postInTx(t, pool, ledger.KindPayoutFailed, "template-payout-failed", got...)
}
