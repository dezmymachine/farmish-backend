package checkout

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/dezmymachine/farmish-backend/internal/audit"
	"github.com/dezmymachine/farmish-backend/internal/database"
	"github.com/dezmymachine/farmish-backend/internal/db"
	"github.com/dezmymachine/farmish-backend/internal/ledger"
	"github.com/dezmymachine/farmish-backend/internal/orders"
	"github.com/dezmymachine/farmish-backend/internal/payments"
)

// refundNeededNote marks an order whose buyer must be paid back because their
// payment arrived after expiry and the stock was gone.
const refundNeededNote = "stock_unavailable_after_expiry"

// HandleCheckoutPaid is the `checkout` purpose handler. It runs inside the
// payments.succeeded job's transaction and is idempotent on the payment
// reference: a replay finds the ledger row already posted.
func (s *Service) HandleCheckoutPaid(ctx context.Context, tx pgx.Tx, payment payments.Payment) error {
	if payment.Purpose != payments.PurposeCheckout {
		return fmt.Errorf("checkout handler called for purpose %q", payment.Purpose)
	}
	if payment.Status != payments.StatusSuccess {
		return fmt.Errorf("checkout handler called for payment status %q", payment.Status)
	}
	checkoutID, err := uuid.Parse(payment.PurposeRef)
	if err != nil {
		return fmt.Errorf("checkout payment %s has a malformed purpose ref: %w", payment.Reference, err)
	}
	q := db.New(tx)
	checkout, err := q.GetCheckoutForUpdate(ctx, checkoutID)
	if err != nil {
		return fmt.Errorf("lock checkout: %w", err)
	}
	switch checkout.Status {
	case "paid":
		// Already confirmed; the ledger's ErrDuplicate below is the other
		// half of this idempotency.
		return nil
	case "pending_payment":
		return s.confirmPending(ctx, tx, q, checkout, payment)
	case "expired":
		return s.confirmExpired(ctx, tx, q, checkout, payment)
	default:
		return fmt.Errorf("checkout %s is %s; nothing to confirm", checkout.ID, checkout.Status)
	}
}

// confirmPending is the normal path: the money arrived within the window.
func (s *Service) confirmPending(ctx context.Context, tx pgx.Tx, q *db.Queries, checkout db.Checkout, payment payments.Payment) error {
	if err := s.markOrdersPaid(ctx, tx, q, checkout.ID); err != nil {
		return err
	}
	if err := q.SetCheckoutStatus(ctx, db.SetCheckoutStatusParams{
		ID: checkout.ID, Status: "paid", Status_2: "pending_payment",
	}); err != nil {
		return fmt.Errorf("mark checkout paid: %w", err)
	}
	return s.postCheckoutPaid(ctx, tx, checkout, payment)
}

// confirmExpired is the late-payment recovery the owner decided on 2026-09-26:
// re-reserve the stock, and when that is impossible cancel for a full refund.
func (s *Service) confirmExpired(ctx context.Context, tx pgx.Tx, q *db.Queries, checkout db.Checkout, payment payments.Payment) error {
	if err := s.reReserveStock(ctx, q, checkout.ID); err != nil {
		if !errors.Is(err, ErrInsufficientStock) {
			return err
		}
		// The stock is gone: the buyer gets their money back. The ledger
		// still posts checkout_paid, because the money did arrive; the refund
		// job (Phase 17a) will Dr escrow and Cr paystack_clearing.
		ordersRows, err := q.ListOrdersByCheckout(ctx, checkout.ID)
		if err != nil {
			return fmt.Errorf("list orders: %w", err)
		}
		for _, order := range ordersRows {
			_, _, err := s.orders.Transition(ctx, tx, order.ID, orders.StatusCancelled,
				orders.System(), refundNeededNote)
			if errors.Is(err, orders.ErrInvalidTransition) {
				continue // a concurrent transition already moved it
			}
			if err != nil {
				return err
			}
			if err := s.orders.SetEscrowState(ctx, tx, order.ID, orders.EscrowRefundPending); err != nil {
				return err
			}
			if s.jobs != nil {
				if _, err := s.jobs.InsertTx(ctx, tx, orders.RefundNeededArgs{
					OrderID: order.ID, AmountPesewas: order.BasePesewas,
				}, jobsUnique()); err != nil {
					return fmt.Errorf("enqueue refund needed: %w", err)
				}
			}
			if err := audit.Record(ctx, tx, audit.Event{
				Action: "order.refund_needed", TargetType: "order", TargetID: order.ID.String(),
				Metadata: map[string]any{
					"checkout_id": checkout.ID.String(),
					"reason":      refundNeededNote,
					"pesewas":     order.BasePesewas,
				},
			}); err != nil {
				return err
			}
			s.log.Warn("late payment for an out-of-stock order; refund needed",
				"order_id", order.ID.String(), "checkout_id", checkout.ID.String())
		}
		if err := q.SetCheckoutStatus(ctx, db.SetCheckoutStatusParams{
			ID: checkout.ID, Status: "paid", Status_2: "expired",
		}); err != nil {
			return fmt.Errorf("mark checkout paid: %w", err)
		}
		return s.postCheckoutPaid(ctx, tx, checkout, payment)
	}

	// The stock came back: the orders revive and escrow holds as usual.
	if err := s.markOrdersPaid(ctx, tx, q, checkout.ID); err != nil {
		return err
	}
	if err := q.SetCheckoutStatus(ctx, db.SetCheckoutStatusParams{
		ID: checkout.ID, Status: "paid", Status_2: "expired",
	}); err != nil {
		return fmt.Errorf("mark checkout paid: %w", err)
	}
	return s.postCheckoutPaid(ctx, tx, checkout, payment)
}

// markOrdersPaid transitions every order of the checkout to paid, marks escrow
// held and stamps paid_at. An already-paid order is skipped.
func (s *Service) markOrdersPaid(ctx context.Context, tx pgx.Tx, q *db.Queries, checkoutID uuid.UUID) error {
	rows, err := q.ListOrdersByCheckout(ctx, checkoutID)
	if err != nil {
		return fmt.Errorf("list orders: %w", err)
	}
	for _, order := range rows {
		if order.Status != orders.StatusPendingPayment && order.Status != orders.StatusExpired {
			continue
		}
		moved, effects, err := s.orders.Transition(ctx, tx, order.ID, orders.StatusPaid, orders.System(), "")
		if errors.Is(err, orders.ErrInvalidTransition) {
			continue // a concurrent path already confirmed this order
		}
		if err != nil {
			return err
		}
		if err := s.orders.MarkPaidAt(ctx, tx, order.ID); err != nil {
			return err
		}
		if err := s.orders.ApplyEffects(ctx, tx, moved, effects); err != nil {
			return err
		}
	}
	return nil
}

// postCheckoutPaid records the money in the ledger: one escrow credit per
// order, the processing fee income and Paystack's actual fee (DOMAIN §5.3.1).
func (s *Service) postCheckoutPaid(ctx context.Context, tx pgx.Tx, checkout db.Checkout, payment payments.Payment) error {
	fee := int64(0)
	if payment.ProviderFee != nil {
		fee = *payment.ProviderFee
	}
	rows, err := db.New(tx).ListOrdersByCheckout(ctx, checkout.ID)
	if err != nil {
		return fmt.Errorf("list orders: %w", err)
	}
	ledgerOrders := make([]ledger.CheckoutOrder, 0, len(rows))
	for _, order := range rows {
		ledgerOrders = append(ledgerOrders, ledger.CheckoutOrder{OrderID: order.ID, Base: order.BasePesewas})
	}
	err = s.ledger.Post(ctx, tx, ledger.KindCheckoutPaid, payment.Reference,
		ledger.CheckoutPaid(payment.Charge, payment.Base, fee, ledgerOrders...)...)
	if errors.Is(err, ledger.ErrDuplicate) {
		return nil
	}
	return err
}

// reReserveStock takes the units back for a late payment. The locks and the
// reservations follow ascending listing id, the same order as creation, so two
// late confirmations cannot deadlock.
func (s *Service) reReserveStock(ctx context.Context, q *db.Queries, checkoutID uuid.UUID) error {
	items, err := q.ListOrderItemsByCheckout(ctx, checkoutID)
	if err != nil {
		return fmt.Errorf("list order items: %w", err)
	}
	ids := make([]uuid.UUID, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ListingID)
	}
	if _, err := q.GetQuoteListingForUpdate(ctx, db.GetQuoteListingForUpdateParams{
		Now: s.Now(), ListingIds: sortedUUIDs(ids),
	}); err != nil {
		return fmt.Errorf("lock listings: %w", err)
	}
	for _, item := range items {
		affected, err := q.ReserveListingStock(ctx, db.ReserveListingStockParams{
			ID: item.ListingID, QuantityAvailable: item.Quantity,
		})
		if err != nil {
			return fmt.Errorf("reserve stock: %w", err)
		}
		if affected == 0 {
			return fmt.Errorf("%w: listing %s", ErrInsufficientStock, item.ListingID)
		}
	}
	return nil
}

// expireOrdersInTx moves a checkout's pending orders to expired. The stock
// restore is the caller's, once, from the stored items.
func (s *Service) expireOrdersInTx(ctx context.Context, tx pgx.Tx, checkoutID uuid.UUID, note string) error {
	q := db.New(tx)
	rows, err := q.ListOrdersByCheckout(ctx, checkoutID)
	if err != nil {
		return fmt.Errorf("list orders: %w", err)
	}
	for _, order := range rows {
		if _, _, err := s.orders.Transition(ctx, tx, order.ID, orders.StatusExpired, orders.System(), note); err != nil {
			if errors.Is(err, orders.ErrInvalidTransition) {
				continue
			}
			return err
		}
	}
	return nil
}

// restoreStockForCheckout returns exactly the reserved units, from the stored
// order items rather than the request.
func restoreStockForCheckout(ctx context.Context, q *db.Queries, checkoutID uuid.UUID) error {
	items, err := q.ListOrderItemsByCheckout(ctx, checkoutID)
	if err != nil {
		return fmt.Errorf("list order items: %w", err)
	}
	for _, item := range items {
		if err := q.RestoreListingStock(ctx, db.RestoreListingStockParams{
			ID: item.ListingID, QuantityAvailable: item.Quantity,
		}); err != nil {
			return fmt.Errorf("restore stock: %w", err)
		}
	}
	return nil
}

// replay rebuilds the stored answer for an idempotent retry: the quote from
// the persisted orders and items, the payment with its authorization URL.
func (s *Service) replay(ctx context.Context, checkout db.Checkout) (Created, error) {
	q := db.New(s.pool)
	ordersRows, err := q.ListCheckoutOrders(ctx, checkout.ID)
	if err != nil {
		return Created{}, fmt.Errorf("list checkout orders: %w", err)
	}
	items, err := q.ListOrderItemsByCheckout(ctx, checkout.ID)
	if err != nil {
		return Created{}, fmt.Errorf("list checkout items: %w", err)
	}
	itemsByOrder := make(map[uuid.UUID][]ItemQuote, len(ordersRows))
	for _, item := range items {
		itemsByOrder[item.OrderID] = append(itemsByOrder[item.OrderID], ItemQuote{
			ListingID: item.ListingID, Title: item.Title, Unit: item.Unit,
			UnitPricePesewas: item.UnitPricePesewas, Quantity: int(item.Quantity),
			LineTotalPesewas: item.LineTotalPesewas,
		})
	}
	quote := CheckoutQuote{
		BasePesewas:          checkout.BasePesewas,
		ProcessingFeePesewas: checkout.ProcessingFeePesewas,
		ChargePesewas:        checkout.ChargePesewas,
	}
	for _, order := range ordersRows {
		quote.Orders = append(quote.Orders, OrderQuote{
			SellerID: order.SellerID, SellerName: order.SellerName,
			Items:              itemsByOrder[order.ID],
			SubtotalPesewas:    order.SubtotalPesewas,
			DeliveryFeePesewas: order.DeliveryFeePesewas,
			BasePesewas:        order.BasePesewas,
		})
	}
	payment, err := s.payments.PaymentForLookup(ctx, pgUUID(checkout.PaymentID))
	if err != nil {
		return Created{}, err
	}
	return Created{
		CheckoutID: checkout.ID, Payment: payment, Quote: quote,
		ExpiresAt: checkout.ExpiresAt, Replayed: true,
	}, nil
}

// Get returns a buyer's own checkout for the status poll. A checkout that is
// not the caller's reads as not found, so ids cannot be probed.
func (s *Service) Get(ctx context.Context, buyerID, checkoutID uuid.UUID) (db.Checkout, []db.ListCheckoutOrdersRow, error) {
	q := db.New(s.pool)
	checkout, err := q.GetCheckoutByID(ctx, checkoutID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && checkout.BuyerID != buyerID) {
		return db.Checkout{}, nil, fmt.Errorf("%w: %s", orders.ErrNotFound, checkoutID)
	}
	if err != nil {
		return db.Checkout{}, nil, fmt.Errorf("get checkout: %w", err)
	}
	rows, err := q.ListCheckoutOrders(ctx, checkoutID)
	if err != nil {
		return db.Checkout{}, nil, fmt.Errorf("list checkout orders: %w", err)
	}
	return checkout, rows, nil
}

// ExpireUnpaid is the periodic sweep: unpaid checkouts past their window lose
// their stock reservation and their payment is abandoned.
func (s *Service) ExpireUnpaid(ctx context.Context) (int, error) {
	q := db.New(s.pool)
	rows, err := q.ListExpiredPendingCheckouts(ctx, db.ListExpiredPendingCheckoutsParams{
		ExpiresAt: s.Now(), Limit: 100,
	})
	if err != nil {
		return 0, fmt.Errorf("list expired checkouts: %w", err)
	}
	expired := 0
	for _, checkout := range rows {
		err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
			inner := db.New(tx)
			current, err := inner.GetCheckoutForUpdate(ctx, checkout.ID)
			if err != nil {
				return fmt.Errorf("lock checkout: %w", err)
			}
			if current.Status != "pending_payment" {
				return nil // the sweep raced a payment or another worker
			}
			if err := inner.SetCheckoutStatus(ctx, db.SetCheckoutStatusParams{
				ID: current.ID, Status: "expired", Status_2: "pending_payment",
			}); err != nil {
				return fmt.Errorf("expire checkout: %w", err)
			}
			if err := s.expireOrdersInTx(ctx, tx, current.ID, "checkout expired unpaid"); err != nil {
				return err
			}
			if err := restoreStockForCheckout(ctx, inner, current.ID); err != nil {
				return err
			}
			if err := s.payments.MarkAbandonedInTx(ctx, tx, pgUUID(current.PaymentID), "the checkout expired unpaid"); err != nil {
				return err
			}
			expired++
			return nil
		})
		if err != nil {
			return expired, err
		}
	}
	return expired, nil
}

// pgUUID converts the generated nullable column for a payments call. A
// checkout always has a payment: the row is written in the same transaction.
func pgUUID(id pgtype.UUID) uuid.UUID {
	return uuid.UUID(id.Bytes)
}
