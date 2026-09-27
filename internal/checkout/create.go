package checkout

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/database"
	"github.com/dezmymachine/farmish-backend/internal/db"
	"github.com/dezmymachine/farmish-backend/internal/delivery"
	"github.com/dezmymachine/farmish-backend/internal/jobs"
	"github.com/dezmymachine/farmish-backend/internal/ledger"
	"github.com/dezmymachine/farmish-backend/internal/orders"
	"github.com/dezmymachine/farmish-backend/internal/payments"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

var (
	// ErrIdempotencyKeyReused means the same key came back with a different
	// payload. The endpoint answers 409: the client must pick a new key.
	ErrIdempotencyKeyReused = errors.New("idempotency key reused with a different payload")
	// ErrInsufficientStock means another buyer took the remaining units first.
	// The endpoint answers 409; nothing was written.
	ErrInsufficientStock = errors.New("insufficient stock")
)

// Provider is the Paystack seam. checkout calls it only outside a transaction.
type Provider = payments.Provider

// Service creates checkouts, confirms their payments and expires the unpaid
// ones. Stock is reserved at creation and restored on failure and expiry, so a
// listed item can never be oversold.
type Service struct {
	pool     *pgxpool.Pool
	payments *payments.Service
	provider Provider
	delivery delivery.Provider
	ledger   *ledger.Ledger
	jobs     *jobs.Client
	log      *slog.Logger
	feeBps   int
	// expiry bounds how long a checkout may sit unpaid (DOMAIN §3).
	expiry time.Duration
	// Now is the clock, injectable so tests can age checkouts.
	Now func() time.Time
}

// New returns a checkout Service. jobs may be nil in tests that do not run the
// refund-needed enqueue.
func New(pool *pgxpool.Pool, ps *payments.Service, provider Provider, deliv delivery.Provider, books *ledger.Ledger, log *slog.Logger, feeBps int, expiry time.Duration) *Service {
	return &Service{
		pool: pool, payments: ps, provider: provider, delivery: deliv,
		ledger: books, log: log, feeBps: feeBps, expiry: expiry, Now: time.Now,
	}
}

// AttachJobClient gives the service the River client it needs to enqueue
// refund-needed jobs. cmd/api calls it once, after the registry exists.
func (s *Service) AttachJobClient(client *jobs.Client) { s.jobs = client }

// Created is the answer to a successful POST /v1/checkout, replay or not.
type Created struct {
	CheckoutID uuid.UUID
	Payment    payments.Payment
	Quote      CheckoutQuote
	ExpiresAt  time.Time
	Replayed   bool
}

// Create is POST /v1/checkout: idempotent, stock-reserving, one order per
// seller, one Paystack charge for the whole basket.
//
// The database work commits before the provider is called, so a network
// failure never leaves a transaction open and the buyer never sees a checkout
// whose stock was not theirs. On a provider failure everything unwinds in a
// fresh transaction and the buyer may retry.
func (s *Service) Create(ctx context.Context, buyerID uuid.UUID, email string, idempotencyKey uuid.UUID, in QuoteInput) (Created, error) {
	if buyerID == uuid.Nil {
		return Created{}, errors.New("checkout requires a buyer")
	}
	if idempotencyKey == uuid.Nil {
		return Created{}, errors.New("checkout requires an idempotency key")
	}
	hash, err := requestHash(in)
	if err != nil {
		return Created{}, err
	}

	created, err := s.createInTx(ctx, buyerID, email, idempotencyKey, hash, in)
	if err != nil {
		return Created{}, err
	}
	if created.Replayed {
		// The stored answer already carries its authorization URL; calling
		// the provider again would double-charge the buyer.
		return created, nil
	}

	// The provider call happens with no transaction open (ADR-0020 applies to
	// checkout for the same reason it applies to promotions).
	result, err := s.provider.InitializeTransaction(ctx, payments.InitializeInput{
		Email: email, AmountPesewas: created.Quote.ChargePesewas,
		Reference: created.Payment.Reference,
		Metadata: map[string]any{
			"payment_id":  created.Payment.ID.String(),
			"purpose":     payments.PurposeCheckout,
			"checkout_id": created.CheckoutID.String(),
		},
	})
	if err != nil {
		if failErr := s.failCheckout(ctx, created.CheckoutID); failErr != nil {
			s.log.Error("rollback after provider failure failed",
				slog.String("checkout_id", created.CheckoutID.String()), slog.String("error", failErr.Error()))
		}
		return Created{}, fmt.Errorf("initialize checkout charge: %w", err)
	}
	if err := s.payments.SetAuthorizationURL(ctx, created.Payment.ID, result.AuthorizationURL); err != nil {
		return Created{}, err
	}
	created.Payment.AuthorizationURL = &result.AuthorizationURL
	return created, nil
}

// createInTx runs the idempotency lookup and, on a miss, the full
// stock-reserving insert. A lost (buyer, key) insert race retries the lookup,
// which is the spec's step 2.
func (s *Service) createInTx(ctx context.Context, buyerID uuid.UUID, email string, idempotencyKey uuid.UUID, hash string, in QuoteInput) (Created, error) {
	for {
		existing, found, err := s.findExisting(ctx, buyerID, idempotencyKey, hash)
		if err != nil {
			return Created{}, err
		}
		if found {
			return existing, nil
		}
		created, err := s.insertCheckout(ctx, buyerID, email, idempotencyKey, hash, in)
		if err != nil {
			if isUniqueViolation(err, "checkouts_buyer_id_idempotency_key_key") {
				// Another request with the same key won the race: its payload
				// decides whether this is a replay or a conflict.
				continue
			}
			return Created{}, err
		}
		return created, nil
	}
}

// findExisting returns the stored answer when the key was seen before. A hash
// mismatch is ErrIdempotencyKeyReused; the caller turns that into a 409.
func (s *Service) findExisting(ctx context.Context, buyerID, idempotencyKey uuid.UUID, hash string) (Created, bool, error) {
	row, err := db.New(s.pool).GetCheckoutByIdempotencyKey(ctx, db.GetCheckoutByIdempotencyKeyParams{
		BuyerID: buyerID, IdempotencyKey: idempotencyKey,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Created{}, false, nil
	}
	if err != nil {
		return Created{}, false, fmt.Errorf("lookup idempotency key: %w", err)
	}
	if row.RequestHash != hash {
		return Created{}, true, ErrIdempotencyKeyReused
	}
	// Any stored status replays its stored answer, exactly as the spec
	// describes: a retry of a failed checkout learns it failed; the buyer
	// retries with a fresh key.
	replayed, err := s.replay(ctx, row)
	if err != nil {
		return Created{}, true, err
	}
	return replayed, true, nil
}

// insertCheckout is the spec's step 3: one transaction that locks snapshots in
// listing-id order, prices, reserves stock and writes every row.
func (s *Service) insertCheckout(ctx context.Context, buyerID uuid.UUID, email string, idempotencyKey uuid.UUID, hash string, in QuoteInput) (Created, error) {
	now := s.Now()
	expiresAt := now.Add(s.expiry)
	var created Created
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		ids := sortedListingIDs(in.Lines)
		rows, err := q.GetQuoteListingForUpdate(ctx, db.GetQuoteListingForUpdateParams{
			Now: now, ListingIds: ids,
		})
		if err != nil {
			return fmt.Errorf("lock listing snapshots: %w", err)
		}
		snapshots := make(map[uuid.UUID]ListingSnapshot, len(rows))
		for _, row := range rows {
			snapshots[row.ID] = snapshotForUpdate(row)
		}
		rates, err := s.ratesFrom(ctx, tx)
		if err != nil {
			return err
		}
		quote, err := PriceCart(ctx, snapshots, Cart{
			BuyerID: buyerID, Lines: in.Lines, Delivery: in.Delivery,
		}, rates, s.feeBps, s.delivery)
		if err != nil {
			// The snapshots were locked a moment ago, so an
			// above-available quantity means another buyer took the units
			// between the quote and this request: a 409, not a 400.
			return translateStockRace(err)
		}

		// Reserve stock before writing any rows, in the same id order the
		// locks were taken, so the failure path touches nothing else.
		for _, line := range in.Lines {
			affected, err := q.ReserveListingStock(ctx, db.ReserveListingStockParams{
				ID: line.ListingID, QuantityAvailable: int32(line.Quantity), //nolint:gosec // G115: quantity is validated against int32 availability
			})
			if err != nil {
				return fmt.Errorf("reserve stock: %w", err)
			}
			if affected == 0 {
				return fmt.Errorf("%w: listing %s", ErrInsufficientStock, line.ListingID)
			}
		}

		checkout, err := q.InsertCheckout(ctx, db.InsertCheckoutParams{
			BuyerID: buyerID, IdempotencyKey: idempotencyKey, RequestHash: hash,
			BasePesewas: quote.BasePesewas, ProcessingFeePesewas: quote.ProcessingFeePesewas,
			ChargePesewas: quote.ChargePesewas, ExpiresAt: expiresAt,
		})
		if err != nil {
			return fmt.Errorf("insert checkout: %w", err)
		}
		payment, err := s.payments.CreatePendingInTx(ctx, tx, payments.CreateInput{
			UserID: buyerID, Email: email, Purpose: payments.PurposeCheckout,
			PurposeRef: checkout.ID.String(), BasePesewas: quote.BasePesewas,
		})
		if err != nil {
			return err
		}
		if err := q.SetCheckoutPayment(ctx, db.SetCheckoutPaymentParams{
			ID: checkout.ID, PaymentID: paymentUUID(payment.ID),
		}); err != nil {
			return fmt.Errorf("link payment: %w", err)
		}

		if err := s.insertOrders(ctx, tx, q, buyerID, checkout.ID, quote, in); err != nil {
			return err
		}
		created = Created{
			CheckoutID: checkout.ID, Payment: payment, Quote: quote,
			ExpiresAt: checkout.ExpiresAt,
		}
		return nil
	})
	if err != nil {
		return Created{}, err
	}
	return created, nil
}

// insertOrders writes one order per seller, the snapshotted items and the
// creation event, in the seller order PriceCart already sorted.
func (s *Service) insertOrders(ctx context.Context, tx pgx.Tx, q *db.Queries, buyerID, checkoutID uuid.UUID, quote CheckoutQuote, in QuoteInput) error {
	itemsByListing := make(map[uuid.UUID]ItemQuote, len(quote.Orders))
	for _, order := range quote.Orders {
		for _, item := range order.Items {
			itemsByListing[item.ListingID] = item
		}
	}
	for _, orderQuote := range quote.Orders {
		choice := in.Delivery[orderQuote.SellerID]
		order, err := q.InsertOrder(ctx, db.InsertOrderParams{
			CheckoutID: checkoutID, BuyerID: buyerID, SellerID: orderQuote.SellerID,
			SubtotalPesewas: orderQuote.SubtotalPesewas, DeliveryFeePesewas: orderQuote.DeliveryFeePesewas,
			BasePesewas: orderQuote.BasePesewas, CommissionRateBps: int32(orderQuote.CommissionRateBps), //nolint:gosec // G115: config rates are bounded 0..3000
			CommissionPesewas: orderQuote.CommissionPesewas, DeliveryMethod: choice.Method,
			DeliveryAddress:  textOrNil(choice.Address, choice.Method != delivery.MethodPickup),
			DeliveryRegion:   textOrNil(choice.Region, choice.Method != delivery.MethodPickup),
			DeliveryDistrict: textOrNil(choice.District, choice.Method != delivery.MethodPickup),
			RecipientName:    textOrNil(choice.RecipientName, choice.Method != delivery.MethodPickup),
			RecipientPhone:   textOrNil(choice.RecipientPhone, choice.Method != delivery.MethodPickup),
		})
		if err != nil {
			return fmt.Errorf("insert order: %w", err)
		}
		for _, item := range orderQuote.Items {
			if err := q.InsertOrderItem(ctx, db.InsertOrderItemParams{
				OrderID: order.ID, ListingID: item.ListingID, Title: item.Title,
				Unit: item.Unit, UnitPricePesewas: item.UnitPricePesewas,
				Quantity:         int32(item.Quantity), //nolint:gosec // G115: quantity is validated against int32 availability
				LineTotalPesewas: item.LineTotalPesewas,
			}); err != nil {
				return fmt.Errorf("insert order item: %w", err)
			}
		}
		if err := q.InsertOrderEvent(ctx, db.InsertOrderEventParams{
			OrderID: order.ID, ToStatus: orders.StatusPendingPayment,
			ActorType: orders.ActorBuyer, ActorID: actorUUID(&buyerID),
		}); err != nil {
			return fmt.Errorf("insert creation event: %w", err)
		}
	}
	return nil
}

// failCheckout unwinds a provider failure: the checkout and payment fail, the
// orders expire and the reserved stock goes back on sale.
func (s *Service) failCheckout(ctx context.Context, checkoutID uuid.UUID) error {
	return database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		checkout, err := q.GetCheckoutForUpdate(ctx, checkoutID)
		if err != nil {
			return fmt.Errorf("lock checkout: %w", err)
		}
		if checkout.Status != "pending_payment" {
			// A webhook or the sweep already moved it; nothing to unwind.
			return nil
		}
		if err := q.SetCheckoutStatus(ctx, db.SetCheckoutStatusParams{
			ID: checkout.ID, Status: "failed", Status_2: "pending_payment",
		}); err != nil {
			return fmt.Errorf("fail checkout: %w", err)
		}
		if err := s.payments.MarkFailedInTx(ctx, tx, pgUUID(checkout.PaymentID), "paystack was unavailable"); err != nil {
			return err
		}
		if err := s.expireOrdersInTx(ctx, tx, checkout.ID, "payment provider failure"); err != nil {
			return err
		}
		return restoreStockForCheckout(ctx, q, checkout.ID)
	})
}

// requestHash canonicalises the cart so the same key with a byte-different
// body still replays, while a genuinely different payload is refused.
func requestHash(in QuoteInput) (string, error) {
	canonical := struct {
		Lines    []CartLine       `json:"lines"`
		Delivery []DeliveryChoice `json:"delivery"`
	}{Lines: in.Lines}
	for _, seller := range sortedSellerIDs(in.Delivery) {
		canonical.Delivery = append(canonical.Delivery, in.Delivery[seller])
	}
	sort.Slice(canonical.Lines, func(i, j int) bool {
		return canonical.Lines[i].ListingID.String() < canonical.Lines[j].ListingID.String()
	})
	raw, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("canonicalise checkout request: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func sortedListingIDs(lines []CartLine) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(lines))
	for _, line := range lines {
		ids = append(ids, line.ListingID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	return ids
}

func sortedSellerIDs(choices map[uuid.UUID]DeliveryChoice) []uuid.UUID {
	sellers := make([]uuid.UUID, 0, len(choices))
	for seller := range choices {
		sellers = append(sellers, seller)
	}
	sort.Slice(sellers, func(i, j int) bool { return sellers[i].String() < sellers[j].String() })
	return sellers
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}

func textOrNil(value string, keep bool) *string {
	if !keep || value == "" {
		return nil
	}
	return &value
}

func actorUUID(id *uuid.UUID) pgtype.UUID {
	if id == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: *id, Valid: true}
}

// paymentUUID converts a payments id for the checkout row's nullable column.
func paymentUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

// snapshotForUpdate maps the locked snapshot row onto the pricing input.
func snapshotForUpdate(row db.GetQuoteListingForUpdateRow) ListingSnapshot {
	var parent *uuid.UUID
	if row.CategoryParentID.Valid {
		id := uuid.UUID(row.CategoryParentID.Bytes)
		parent = &id
	}
	return ListingSnapshot{
		ID: row.ID, SellerID: row.SellerID, SellerName: row.SellerName, Title: row.Title,
		Unit: row.Unit, UnitPricePesewas: row.UnitPrice, QuantityAvailable: row.QuantityAvailable,
		MinOrderQty: row.MinOrderQty, CategoryID: row.CategoryID, ParentCategoryID: parent,
		Active: row.Active, OffersPickup: row.OffersPickup, OffersSellerDelivery: row.OffersSellerDelivery,
		SellerDeliveryFee: row.SellerDeliveryFeePesewas,
	}
}

// translateStockRace maps the quantity-availability validation error, and only
// that error, onto ErrInsufficientStock. Other validation problems were wrong
// when the client loaded the page and stay 400s.
func translateStockRace(err error) error {
	var verr *validation.Error
	if !errors.As(err, &verr) {
		return err
	}
	for _, field := range verr.Fields {
		if strings.HasSuffix(field.Name, ".quantity") && strings.Contains(field.Message, "available") {
			return ErrInsufficientStock
		}
	}
	return err
}
