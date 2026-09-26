package ledger

import (
	"github.com/google/uuid"
)

// Transaction kinds from DOMAIN §5.3.
const (
	KindCheckoutPaid     = "checkout_paid"
	KindEscrowRelease    = "escrow_release"
	KindOrderRefund      = "order_refund"
	KindPromotionPaid    = "promotion_paid"
	KindPromotionApplied = "promotion_applied"
	KindPayoutInitiated  = "payout_initiated"
	KindPayoutSucceeded  = "payout_succeeded"
	KindPayoutFailed     = "payout_failed"
)

// CheckoutOrder is one order's share of a checkout payment.
type CheckoutOrder struct {
	OrderID uuid.UUID
	// Base is the order's base: its subtotal plus its delivery fee.
	Base int64
}

// CheckoutPaid records a successful checkout (DOMAIN §5.3.1). Callers must
// pass the orders whose bases add up to base: Post rejects anything else.
func CheckoutPaid(charge, base, paystackFee int64, orders ...CheckoutOrder) []Entry {
	entries := make([]Entry, 0, len(orders)+3)
	entries = append(entries,
		Entry{Account: PaystackClearing, Amount: charge - paystackFee, Currency: CurrencyGHS},
		Entry{Account: PaystackFees, Amount: paystackFee, Currency: CurrencyGHS},
	)
	for _, order := range orders {
		orderID := order.OrderID
		entries = append(entries, Entry{
			Account:  Escrow,
			Amount:   -order.Base,
			Currency: CurrencyGHS,
			OrderID:  &orderID,
		})
	}
	return append(entries, Entry{
		Account:  ProcessingFeeIncome,
		Amount:   -(charge - base),
		Currency: CurrencyGHS,
	})
}

// EscrowRelease records an order's release (DOMAIN §5.3.2). The seller keeps
// the whole base minus the snapshotted commission. The commission itself is
// calculated from the order's subtotal by the caller.
func EscrowRelease(orderID, sellerID uuid.UUID, base, commission int64) []Entry {
	return []Entry{
		{Account: Escrow, Amount: base, Currency: CurrencyGHS, OrderID: &orderID},
		{Account: SellerPayable(sellerID), Amount: -(base - commission), Currency: CurrencyGHS, OrderID: &orderID},
		{Account: PlatformCommission, Amount: -commission, Currency: CurrencyGHS, OrderID: &orderID},
	}
}

// OrderRefund returns money from escrow to Paystack's clearing balance
// (DOMAIN §5.3.3). The caller owns the phase-17 rules about what may still be
// refunded; this records the movement.
func OrderRefund(orderID uuid.UUID, refund int64) []Entry {
	return []Entry{
		{Account: Escrow, Amount: refund, Currency: CurrencyGHS, OrderID: &orderID},
		{Account: PaystackClearing, Amount: -refund, Currency: CurrencyGHS, OrderID: &orderID},
	}
}

// PromotionPaid records a promotion purchase (DOMAIN §5.3.4): GHS entries for
// the cash charge, plus CRD entries for the credits issued. paystackFee is the
// actual fee from the webhook, which may differ from the estimated processing
// fee included in charge.
func PromotionPaid(price, charge, paystackFee int64, credits int64, userID uuid.UUID) []Entry {
	return []Entry{
		{Account: PaystackClearing, Amount: charge - paystackFee, Currency: CurrencyGHS},
		{Account: PaystackFees, Amount: paystackFee, Currency: CurrencyGHS},
		{Account: PromotionRevenue, Amount: -price, Currency: CurrencyGHS},
		{Account: ProcessingFeeIncome, Amount: -(charge - price), Currency: CurrencyGHS},
		{Account: PromoCreditsIssued, Amount: credits, Currency: CurrencyCRD},
		{Account: PromoCredits(userID), Amount: -credits, Currency: CurrencyCRD},
	}
}

// PromotionApplied spends promotion credits on a listing promotion (DOMAIN
// §5.3.5). The caller's transaction must lock the user's credit row and refuse
// a negative balance; this only describes the balanced movement.
func PromotionApplied(userID uuid.UUID, credits int64) []Entry {
	return []Entry{
		{Account: PromoCredits(userID), Amount: credits, Currency: CurrencyCRD},
		{Account: PromoCreditsIssued, Amount: -credits, Currency: CurrencyCRD},
	}
}

// PayoutInitiated moves released seller money into payout clearing (DOMAIN
// §5.3.6).
func PayoutInitiated(sellerID uuid.UUID, amount int64) []Entry {
	return []Entry{
		{Account: SellerPayable(sellerID), Amount: amount, Currency: CurrencyGHS},
		{Account: PayoutClearing, Amount: -amount, Currency: CurrencyGHS},
	}
}

// PayoutSucceeded records a confirmed transfer (DOMAIN §5.3.7). The platform
// absorbs the transfer fee when it is positive.
func PayoutSucceeded(amount, transferFee int64) []Entry {
	entries := []Entry{
		{Account: PayoutClearing, Amount: amount, Currency: CurrencyGHS},
		{Account: PaystackClearing, Amount: -amount, Currency: CurrencyGHS},
	}
	if transferFee > 0 {
		entries = append(entries,
			Entry{Account: TransferFees, Amount: transferFee, Currency: CurrencyGHS},
			Entry{Account: PaystackClearing, Amount: -transferFee, Currency: CurrencyGHS},
		)
	}
	return entries
}

// PayoutFailed returns an initiated transfer to the seller's payable balance
// (DOMAIN §5.3.8).
func PayoutFailed(sellerID uuid.UUID, amount int64) []Entry {
	return []Entry{
		{Account: PayoutClearing, Amount: amount, Currency: CurrencyGHS},
		{Account: SellerPayable(sellerID), Amount: -amount, Currency: CurrencyGHS},
	}
}
