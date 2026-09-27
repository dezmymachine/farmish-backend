package checkout_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/dezmymachine/farmish-backend/internal/auth"
	"github.com/dezmymachine/farmish-backend/internal/catalog"
	"github.com/dezmymachine/farmish-backend/internal/checkout"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/delivery"
	"github.com/dezmymachine/farmish-backend/internal/jobs"
	"github.com/dezmymachine/farmish-backend/internal/ledger"
	"github.com/dezmymachine/farmish-backend/internal/listings"
	"github.com/dezmymachine/farmish-backend/internal/media"
	"github.com/dezmymachine/farmish-backend/internal/media/mediatest"
	"github.com/dezmymachine/farmish-backend/internal/money"
	"github.com/dezmymachine/farmish-backend/internal/orders"
	"github.com/dezmymachine/farmish-backend/internal/payments"
	"github.com/dezmymachine/farmish-backend/internal/payments/fake"
	"github.com/dezmymachine/farmish-backend/internal/payments/paystacktest"
	"github.com/dezmymachine/farmish-backend/internal/sellers"
	"github.com/dezmymachine/farmish-backend/internal/users"
)

const feeBps = 195

// fixture is a full production-shaped checkout stack over real Postgres: real
// listings, a real payments service, River running the purpose handler, and a
// fake Paystack.
type fixture struct {
	pool     *pgxpool.Pool
	store    *media.R2
	listings *listings.Service
	payments *payments.Service
	provider *fake.Provider
	svc      *checkout.Service
	sellerA  uuid.UUID
	sellerB  uuid.UUID
	buyer    uuid.UUID
	other    uuid.UUID
	now      time.Time
	events   <-chan *river.Event
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Pool(t)
	if err := catalog.Seed(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := mediatest.R2(t)
	sellersSvc := sellers.New(pool, nil, nil)
	mk := func(uid string) uuid.UUID {
		user, err := users.New(pool).Resolve(ctx, auth.Identity{
			UID: uid, Email: uid + "@farmish.test", Provider: "password",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sellersSvc.UpsertMine(ctx, user.ID, sellers.ProfileInput{
			BusinessName: uid + " farms", Region: "Ashanti", District: "Kumasi Metro",
		}); err != nil {
			t.Fatal(err)
		}
		return user.ID
	}
	f := &fixture{
		pool: pool, store: store,
		sellerA: mk("checkout-seller-a"), sellerB: mk("checkout-seller-b"),
		buyer: mk("checkout-buyer"), other: mk("checkout-other"),
	}
	f.now = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	f.listings = listings.New(pool, catalog.New(pool), media.New(pool, store), sellersSvc)
	f.listings.Now = func() time.Time { return f.now }
	log := slog.New(slog.DiscardHandler)
	f.provider = fake.New()
	f.payments = payments.New(pool, f.provider, log, feeBps, "https://farmish.gh/payments/status")
	f.svc = checkout.New(pool, f.payments, f.provider, delivery.Manual{}, ledger.New(), log, feeBps, 30*time.Minute)
	f.svc.Now = func() time.Time { return f.now }
	f.payments.RegisterPurpose(payments.PurposeCheckout, f.svc.HandleCheckoutPaid)

	reg := jobs.NewRegistry()
	payments.RegisterSucceeded(reg, f.payments, log)
	checkout.RegisterJobs(reg, f.svc, log)
	client, err := jobs.NewClient(pool, reg, log, jobs.Options{Work: true, FetchPollInterval: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	events, cancel := client.Subscribe(river.EventKindJobCompleted)
	t.Cleanup(cancel)
	if err := client.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.events = events
	f.payments.AttachJobClient(client)
	f.svc.AttachJobClient(client)
	t.Cleanup(func() {
		if err := jobs.Stop(client, 5*time.Second, 2*time.Second, log); err != nil {
			t.Errorf("stop jobs: %v", err)
		}
	})
	return f
}

var uploadClient = &http.Client{Timeout: 10 * time.Second}

// activeListing publishes a listing the fixture controls. Price and stock are
// the caller's, and seller delivery can be turned on with a fee.
func (f *fixture) activeListing(t *testing.T, owner uuid.UUID, title string, price int64, stock int32, deliveryFee *int64) listings.View {
	t.Helper()
	ctx := context.Background()
	in := listings.Input{
		CategorySlug: "fresh-produce-grains-cereals", Title: title,
		Description:  "Maize stored in a silo, available for pickup at the farm.",
		PricePesewas: price, Unit: "kg", QuantityAvailable: stock, MinOrderQty: 1,
		IsNegotiable: true, ItemState: "grade_a", Region: "Ashanti", District: "Kumasi Metro",
		Delivery: listings.Delivery{Pickup: true, SellerDelivery: deliveryFee != nil, FeePesewas: deliveryFee},
	}
	view, err := f.listings.Create(ctx, owner, in, false)
	if err != nil {
		t.Fatal(err)
	}
	up, err := media.New(f.pool, f.store).CreateUpload(ctx, owner, media.PurposeListingImage, "image/jpeg", 64)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("a", 64)
	req, err := http.NewRequest(http.MethodPut, up.URL, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(body))
	for key, value := range up.Headers {
		req.Header.Set(key, value)
	}
	resp, err := uploadClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload: %d", resp.StatusCode)
	}
	if _, err := f.listings.Update(ctx, owner, view.ID, listings.Patch{
		ImageMediaIDs: []uuid.UUID{up.ID},
	}); err != nil {
		t.Fatal(err)
	}
	active, err := f.listings.Publish(ctx, owner, view.ID)
	if err != nil {
		t.Fatal(err)
	}
	return active
}

func (f *fixture) cart(t *testing.T, lines []checkout.CartLine) checkout.QuoteInput {
	t.Helper()
	return f.cartFor(t, lines, f.sellerA)
}

// cartFor builds a pickup cart for exactly the sellers involved. A delivery
// entry for a seller who has no line would be a validation error, which is
// the right contract behaviour for a client that ignored its own quote.
func (f *fixture) cartFor(t *testing.T, lines []checkout.CartLine, sellers ...uuid.UUID) checkout.QuoteInput {
	t.Helper()
	choices := make(map[uuid.UUID]checkout.DeliveryChoice, len(sellers))
	for _, seller := range sellers {
		choices[seller] = checkout.DeliveryChoice{Method: delivery.MethodPickup}
	}
	return checkout.QuoteInput{Lines: lines, Delivery: choices}
}

// deliveredCart is the seller-delivery variant, for orders that must carry a
// recipient. The listing must offer seller delivery.
func (f *fixture) deliveredCart(t *testing.T, lines []checkout.CartLine) checkout.QuoteInput {
	t.Helper()
	return checkout.QuoteInput{Lines: lines, Delivery: map[uuid.UUID]checkout.DeliveryChoice{
		f.sellerA: {
			Method: delivery.MethodSellerDelivery, Address: "12 Market Road",
			RecipientName: "Ama Serwaa", RecipientPhone: "+233241234567",
		},
	}}
}

func (f *fixture) stock(t *testing.T, listingID uuid.UUID) int32 {
	t.Helper()
	var stock int32
	if err := f.pool.QueryRow(context.Background(),
		`SELECT quantity_available FROM listings WHERE id = $1`, listingID).Scan(&stock); err != nil {
		t.Fatal(err)
	}
	return stock
}

func (f *fixture) orderStatuses(t *testing.T, checkoutID uuid.UUID) []string {
	t.Helper()
	rows, err := f.pool.Query(context.Background(),
		`SELECT status FROM orders WHERE checkout_id = $1 ORDER BY seller_id`, checkoutID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var status string
		if err := rows.Scan(&status); err != nil {
			t.Fatal(err)
		}
		out = append(out, status)
	}
	return out
}

func (f *fixture) escrowBalances(t *testing.T) (int64, int64) {
	t.Helper()
	var escrow, heldOrders int64
	if err := f.pool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(le.amount), 0) FROM ledger_entries le
		 JOIN ledger_accounts la ON la.id = le.account_id WHERE la.code = 'escrow'`).Scan(&escrow); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(base_pesewas), 0) FROM orders
		 WHERE escrow_state IN ('held','refund_pending','partially_refunded')`).Scan(&heldOrders); err != nil {
		t.Fatal(err)
	}
	return escrow, heldOrders
}

// settle posts a signed charge.success for a created checkout and waits for the
// purpose job to finish.
func (f *fixture) settle(t *testing.T, created checkout.Created, providerFee int64) {
	t.Helper()
	body := paystacktest.ChargeSuccessBody(created.Payment.Reference, created.Quote.ChargePesewas, time.Now().UnixNano())
	if err := f.payments.HandleWebhook(context.Background(), body); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(20 * time.Second)
	for {
		select {
		case event := <-f.events:
			if event.Kind == river.EventKindJobCompleted && event.Job.Kind == "payments.succeeded" {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for payments.succeeded")
		}
	}
}

func TestCheckout_TwoSellersTwoOrdersOneCharge(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	listingA := f.activeListing(t, f.sellerA, "Checkout Maize A", 1000, 10, nil)
	listingB := f.activeListing(t, f.sellerB, "Checkout Maize B", 2500, 10, nil)

	created, err := f.svc.Create(ctx, f.buyer, "buyer@farmish.test", uuid.New(),
		f.cartFor(t, []checkout.CartLine{
			{ListingID: listingA.ID, Quantity: 2},
			{ListingID: listingB.ID, Quantity: 3},
		}, f.sellerA, f.sellerB))
	if err != nil {
		t.Fatal(err)
	}
	if created.Replayed {
		t.Error("a fresh create reported itself as a replay")
	}
	if len(created.Quote.Orders) != 2 || len(created.Quote.Orders[0].Items)+len(created.Quote.Orders[1].Items) != 2 {
		t.Errorf("quote orders = %+v, want two orders with two items", created.Quote.Orders)
	}
	charge, fee, err := money.GrossUp(9500, feeBps)
	if err != nil {
		t.Fatal(err)
	}
	if created.Quote.BasePesewas != 9500 || created.Quote.ChargePesewas != charge || created.Quote.ProcessingFeePesewas != fee {
		t.Errorf("quote = base %d fee %d charge %d, want 9500/%d/%d",
			created.Quote.BasePesewas, created.Quote.ProcessingFeePesewas, created.Quote.ChargePesewas, fee, charge)
	}
	// Exactly one provider call for the whole basket, with the grossed-up total.
	if got := f.provider.CallCount("InitializeTransaction"); got != 1 {
		t.Fatalf("provider initialize calls = %d, want 1", got)
	}
	if f.provider.Initialized[0].AmountPesewas != charge {
		t.Errorf("provider amount = %d, want the grossed-up charge %d",
			f.provider.Initialized[0].AmountPesewas, charge)
	}
	if f.provider.Initialized[0].Reference != created.Payment.Reference {
		t.Errorf("provider reference = %q, want %q", f.provider.Initialized[0].Reference, created.Payment.Reference)
	}
	// Stock is reserved.
	if got := f.stock(t, listingA.ID); got != 8 {
		t.Errorf("listing A stock = %d, want 8", got)
	}
	if got := f.stock(t, listingB.ID); got != 7 {
		t.Errorf("listing B stock = %d, want 7", got)
	}

	// The buyer can poll their checkout.
	checkoutRow, orderRows, err := f.svc.Get(ctx, f.buyer, created.CheckoutID)
	if err != nil {
		t.Fatal(err)
	}
	if checkoutRow.Status != "pending_payment" || len(orderRows) != 2 {
		t.Errorf("poll = %s with %d orders", checkoutRow.Status, len(orderRows))
	}
	if _, _, err := f.svc.Get(ctx, f.other, created.CheckoutID); !errors.Is(err, orders.ErrNotFound) {
		t.Errorf("another user's poll = %v, want ErrNotFound", err)
	}
}

func TestCheckout_Idempotent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	listing := f.activeListing(t, f.sellerA, "Idempotent Maize", 1000, 10, nil)
	key := uuid.New()
	in := f.cart(t, []checkout.CartLine{{ListingID: listing.ID, Quantity: 1}})

	first, err := f.svc.Create(ctx, f.buyer, "buyer@farmish.test", key, in)
	if err != nil {
		t.Fatal(err)
	}
	calls := f.provider.CallCount("InitializeTransaction")
	second, err := f.svc.Create(ctx, f.buyer, "buyer@farmish.test", key, in)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Replayed || second.CheckoutID != first.CheckoutID || second.Payment.Reference != first.Payment.Reference {
		t.Errorf("replay = %+v, want the stored answer %+v", second, first)
	}
	if got := f.provider.CallCount("InitializeTransaction"); got != calls {
		t.Errorf("replay made another provider call: %d -> %d", calls, got)
	}
	if got := f.stock(t, listing.ID); got != 9 {
		t.Errorf("replay changed stock to %d, want 9", got)
	}

	// The same key with a different payload is refused.
	changed := f.cart(t, []checkout.CartLine{{ListingID: listing.ID, Quantity: 2}})
	if _, err := f.svc.Create(ctx, f.buyer, "buyer@farmish.test", key, changed); !errors.Is(err, checkout.ErrIdempotencyKeyReused) {
		t.Errorf("changed payload err = %v, want ErrIdempotencyKeyReused", err)
	}
	if got := f.stock(t, listing.ID); got != 9 {
		t.Errorf("conflict changed stock to %d, want 9", got)
	}
}

func TestCheckout_ProviderFailureRollsBackStock(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	listing := f.activeListing(t, f.sellerA, "Failure Maize", 1000, 5, nil)
	f.provider.InitErr = payments.ErrProviderUnavailable
	defer func() { f.provider.InitErr = nil }()

	_, err := f.svc.Create(ctx, f.buyer, "buyer@farmish.test", uuid.New(), f.cart(t, []checkout.CartLine{
		{ListingID: listing.ID, Quantity: 2},
	}))
	if !errors.Is(err, payments.ErrProviderUnavailable) {
		t.Fatalf("err = %v, want ErrProviderUnavailable", err)
	}
	if got := f.stock(t, listing.ID); got != 5 {
		t.Errorf("stock after failure = %d, want 5 restored", got)
	}
	var status string
	if err := f.pool.QueryRow(ctx,
		`SELECT status FROM checkouts ORDER BY created_at DESC LIMIT 1`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Errorf("checkout status = %q, want failed", status)
	}
	var orderStatus string
	if err := f.pool.QueryRow(ctx,
		`SELECT status FROM orders ORDER BY created_at DESC LIMIT 1`).Scan(&orderStatus); err != nil {
		t.Fatal(err)
	}
	if orderStatus != orders.StatusExpired {
		t.Errorf("order status = %q, want expired", orderStatus)
	}
}

func TestCheckout_WebhookReplayNoDoubleEscrow(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	listing := f.activeListing(t, f.sellerA, "Replay Maize", 1000, 10, nil)
	created, err := f.svc.Create(ctx, f.buyer, "buyer@farmish.test", uuid.New(), f.cart(t, []checkout.CartLine{
		{ListingID: listing.ID, Quantity: 2},
	}))
	if err != nil {
		t.Fatal(err)
	}
	f.settle(t, created, 20)

	escrow, held := f.escrowBalances(t)
	if escrow != -2000 || held != 2000 {
		t.Fatalf("after settle: escrow %d, held bases %d, want -2000 and 2000", escrow, held)
	}
	for _, status := range f.orderStatuses(t, created.CheckoutID) {
		if status != orders.StatusPaid {
			t.Errorf("order status = %q, want paid", status)
		}
	}
	var escrowState string
	if err := f.pool.QueryRow(ctx,
		`SELECT escrow_state FROM orders WHERE checkout_id = $1`, created.CheckoutID).Scan(&escrowState); err != nil {
		t.Fatal(err)
	}
	if escrowState != orders.EscrowHeld {
		t.Errorf("escrow_state = %q, want held", escrowState)
	}

	// The identical webhook body is a duplicate and changes nothing.
	body := paystacktest.ChargeSuccessBody(created.Payment.Reference, created.Quote.ChargePesewas, time.Now().UnixNano())
	if err := f.payments.HandleWebhook(ctx, body); err != nil {
		t.Fatal(err)
	}
	afterEscrow, afterHeld := f.escrowBalances(t)
	if afterEscrow != escrow || afterHeld != held {
		t.Errorf("replay moved the ledger: escrow %d -> %d, held %d -> %d",
			escrow, afterEscrow, held, afterHeld)
	}
	var postings int64
	if err := f.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM ledger_transactions WHERE kind = 'checkout_paid' AND reference = $1`,
		created.Payment.Reference).Scan(&postings); err != nil {
		t.Fatal(err)
	}
	if postings != 1 {
		t.Errorf("checkout_paid postings = %d, want exactly 1", postings)
	}
	// Every escrow credit carries the order id.
	var unnamed int64
	if err := f.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM ledger_entries le
		 JOIN ledger_accounts la ON la.id = le.account_id
		 JOIN ledger_transactions lt ON lt.id = le.transaction_id
		 WHERE la.code = 'escrow' AND lt.kind = 'checkout_paid' AND le.order_id IS NULL`).Scan(&unnamed); err != nil {
		t.Fatal(err)
	}
	if unnamed != 0 {
		t.Errorf("%d escrow credits without an order id", unnamed)
	}
}

func TestCheckout_AmountMismatchLeavesUnpaid(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	listing := f.activeListing(t, f.sellerA, "Mismatch Maize", 1000, 10, nil)
	created, err := f.svc.Create(ctx, f.buyer, "buyer@farmish.test", uuid.New(), f.cart(t, []checkout.CartLine{
		{ListingID: listing.ID, Quantity: 2},
	}))
	if err != nil {
		t.Fatal(err)
	}
	// One pesewa off: 13a rejects it, the checkout stays pending.
	body := paystacktest.ChargeSuccessBody(created.Payment.Reference, created.Quote.ChargePesewas+1, 777)
	if err := f.payments.HandleWebhook(ctx, body); err != nil {
		t.Fatal(err)
	}
	checkoutRow, _, err := f.svc.Get(ctx, f.buyer, created.CheckoutID)
	if err != nil {
		t.Fatal(err)
	}
	if checkoutRow.Status != "pending_payment" {
		t.Errorf("checkout status = %q, want pending_payment", checkoutRow.Status)
	}
	for _, status := range f.orderStatuses(t, created.CheckoutID) {
		if status != orders.StatusPendingPayment {
			t.Errorf("order status = %q, want pending_payment", status)
		}
	}
	escrow, held := f.escrowBalances(t)
	if escrow != 0 || held != 0 {
		t.Errorf("mismatch posted to the ledger: escrow %d, held %d", escrow, held)
	}
	var auditRows int64
	if err := f.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM audit_events WHERE action = 'payment.amount_mismatch'`).Scan(&auditRows); err != nil {
		t.Fatal(err)
	}
	if auditRows != 1 {
		t.Errorf("audit rows = %d, want 1", auditRows)
	}
}

func TestCheckout_ExpiresAndRestoresStock(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	listing := f.activeListing(t, f.sellerA, "Expiring Maize", 1000, 5, nil)
	created, err := f.svc.Create(ctx, f.buyer, "buyer@farmish.test", uuid.New(), f.cart(t, []checkout.CartLine{
		{ListingID: listing.ID, Quantity: 3},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.stock(t, listing.ID); got != 2 {
		t.Fatalf("reserved stock = %d, want 2", got)
	}

	// The clock passes the 30-minute window; the sweep expires everything.
	f.now = f.now.Add(31 * time.Minute)
	expired, err := f.svc.ExpireUnpaid(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if expired != 1 {
		t.Fatalf("expired = %d, want 1", expired)
	}
	if got := f.stock(t, listing.ID); got != 5 {
		t.Errorf("stock after expiry = %d, want 5 restored exactly", got)
	}
	checkoutRow, _, err := f.svc.Get(ctx, f.buyer, created.CheckoutID)
	if err != nil {
		t.Fatal(err)
	}
	if checkoutRow.Status != "expired" {
		t.Errorf("checkout status = %q, want expired", checkoutRow.Status)
	}
	for _, status := range f.orderStatuses(t, created.CheckoutID) {
		if status != orders.StatusExpired {
			t.Errorf("order status = %q, want expired", status)
		}
	}
	var paymentStatus string
	if err := f.pool.QueryRow(ctx,
		`SELECT status FROM payments WHERE id = $1`, created.Payment.ID).Scan(&paymentStatus); err != nil {
		t.Fatal(err)
	}
	if paymentStatus != payments.StatusAbandoned {
		t.Errorf("payment status = %q, want abandoned", paymentStatus)
	}
	// The expired checkout has an expiry event on its order.
	var events int64
	if err := f.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM order_events oe JOIN orders o ON o.id = oe.order_id
		 WHERE o.checkout_id = $1 AND oe.to_status = 'expired'`, created.CheckoutID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Errorf("expiry events = %d, want 1", events)
	}
}

func TestCheckout_ConcurrentLastUnit(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	listing := f.activeListing(t, f.sellerA, "Last Unit Maize", 1000, 1, nil)
	cart := f.cart(t, []checkout.CartLine{{ListingID: listing.ID, Quantity: 1}})

	const buyers = 5
	var wg sync.WaitGroup
	results := make([]struct {
		err error
		id  uuid.UUID
	}, buyers)
	for i := range buyers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			created, err := f.svc.Create(ctx, f.buyer, "buyer@farmish.test", uuid.New(), cart)
			results[i].err = err
			results[i].id = created.CheckoutID
		}()
	}
	wg.Wait()

	succeeded := 0
	for _, result := range results {
		switch {
		case result.err == nil:
			succeeded++
		case errors.Is(result.err, checkout.ErrInsufficientStock):
		default:
			t.Fatalf("concurrent create err = %v", result.err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("succeeded = %d, want exactly 1", succeeded)
	}
	if got := f.stock(t, listing.ID); got != 0 {
		t.Errorf("final stock = %d, want 0", got)
	}
}

func TestCheckout_PaidAfterExpiry(t *testing.T) {
	t.Run("stock still there revives the order", func(t *testing.T) {
		f := newFixture(t)
		ctx := context.Background()
		listing := f.activeListing(t, f.sellerA, "Late Maize", 1000, 5, nil)
		created, err := f.svc.Create(ctx, f.buyer, "buyer@farmish.test", uuid.New(), f.cart(t, []checkout.CartLine{
			{ListingID: listing.ID, Quantity: 2},
		}))
		if err != nil {
			t.Fatal(err)
		}
		f.now = f.now.Add(31 * time.Minute)
		if _, err := f.svc.ExpireUnpaid(ctx); err != nil {
			t.Fatal(err)
		}
		f.settle(t, created, 20)

		for _, status := range f.orderStatuses(t, created.CheckoutID) {
			if status != orders.StatusPaid {
				t.Errorf("order status = %q, want paid", status)
			}
		}
		checkoutRow, _, err := f.svc.Get(ctx, f.buyer, created.CheckoutID)
		if err != nil {
			t.Fatal(err)
		}
		if checkoutRow.Status != "paid" {
			t.Errorf("checkout status = %q, want paid", checkoutRow.Status)
		}
		// Late revive re-reserves: 5 restored, then 3 held again.
		if got := f.stock(t, listing.ID); got != 3 {
			t.Errorf("stock = %d, want 3", got)
		}
		escrow, held := f.escrowBalances(t)
		if escrow != -2000 || held != 2000 {
			t.Errorf("escrow %d, held %d, want -2000 and 2000", escrow, held)
		}
	})

	t.Run("stock gone cancels and records the refund", func(t *testing.T) {
		f := newFixture(t)
		ctx := context.Background()
		listing := f.activeListing(t, f.sellerA, "Gone Maize", 1000, 1, nil)
		late, err := f.svc.Create(ctx, f.buyer, "buyer@farmish.test", uuid.New(), f.cart(t, []checkout.CartLine{
			{ListingID: listing.ID, Quantity: 1},
		}))
		if err != nil {
			t.Fatal(err)
		}
		f.now = f.now.Add(31 * time.Minute)
		if _, err := f.svc.ExpireUnpaid(ctx); err != nil {
			t.Fatal(err)
		}
		// Another buyer takes the restored unit.
		stealer, err := f.svc.Create(ctx, f.other, "other@farmish.test", uuid.New(), f.cart(t, []checkout.CartLine{
			{ListingID: listing.ID, Quantity: 1},
		}))
		if err != nil {
			t.Fatal(err)
		}
		f.settle(t, stealer, 10)

		f.settle(t, late, 20)
		for _, status := range f.orderStatuses(t, late.CheckoutID) {
			if status != orders.StatusCancelled {
				t.Errorf("order status = %q, want cancelled", status)
			}
		}
		var escrowState, note string
		if err := f.pool.QueryRow(ctx,
			`SELECT escrow_state, note FROM orders o
			 JOIN order_events oe ON oe.order_id = o.id AND oe.to_status = 'cancelled'
			 WHERE o.checkout_id = $1`, late.CheckoutID).Scan(&escrowState, &note); err != nil {
			t.Fatal(err)
		}
		if escrowState != orders.EscrowRefundPending {
			t.Errorf("escrow_state = %q, want refund_pending", escrowState)
		}
		if note != "stock_unavailable_after_expiry" {
			t.Errorf("note = %q", note)
		}
		var refundJobs int64
		if err := f.pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM river_job WHERE kind = 'orders.refund_needed'`).Scan(&refundJobs); err != nil {
			t.Fatal(err)
		}
		if refundJobs != 1 {
			t.Errorf("refund-needed jobs = %d, want 1", refundJobs)
		}
		var audits int64
		if err := f.pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM audit_events WHERE action = 'order.refund_needed'`).Scan(&audits); err != nil {
			t.Fatal(err)
		}
		if audits != 1 {
			t.Errorf("refund-needed audits = %d, want 1", audits)
		}
		// Escrow still balances: the money arrived, and it is refund-pending.
		escrow, held := f.escrowBalances(t)
		if escrow != -2000 || held != 2000 {
			t.Errorf("escrow %d, held %d, want -2000 and 2000 (late + stealer)", escrow, held)
		}
		checkoutRow, _, err := f.svc.Get(ctx, f.buyer, late.CheckoutID)
		if err != nil {
			t.Fatal(err)
		}
		if checkoutRow.Status != "paid" {
			t.Errorf("checkout status = %q, want paid (the money arrived)", checkoutRow.Status)
		}
	})
}

func TestLedger_EscrowEqualsHeldOrders(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	first := f.activeListing(t, f.sellerA, "Invariant Maize A", 1200, 20, nil)
	second := f.activeListing(t, f.sellerB, "Invariant Maize B", 800, 20, nil)

	for i := range 3 {
		created, err := f.svc.Create(ctx, f.buyer, "buyer@farmish.test", uuid.New(),
			f.cartFor(t, []checkout.CartLine{
				{ListingID: first.ID, Quantity: i + 1},
				{ListingID: second.ID, Quantity: 1},
			}, f.sellerA, f.sellerB))
		if err != nil {
			t.Fatal(err)
		}
		f.settle(t, created, 20)
	}

	escrow, held := f.escrowBalances(t)
	if escrow != -held || held == 0 {
		t.Errorf("escrow %d vs held bases %d, want escrow = -held", escrow, held)
	}
	var global int64
	if err := f.pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(amount), 0) FROM ledger_entries WHERE currency = 'GHS'`).Scan(&global); err != nil {
		t.Fatal(err)
	}
	if global != 0 {
		t.Errorf("global GHS ledger sum = %d, want 0", global)
	}
}

func TestOrders_AccessControl(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	fee := int64(500)
	listing := f.activeListing(t, f.sellerA, "Access Maize", 1000, 10, &fee)
	created, err := f.svc.Create(ctx, f.buyer, "buyer@farmish.test", uuid.New(), f.deliveredCart(t, []checkout.CartLine{
		{ListingID: listing.ID, Quantity: 2},
	}))
	if err != nil {
		t.Fatal(err)
	}
	f.settle(t, created, 20)

	ordersSvc := orders.NewReadService(f.pool)
	rows, err := f.pool.Query(ctx,
		`SELECT id FROM orders WHERE checkout_id = $1`, created.CheckoutID)
	if err != nil {
		t.Fatal(err)
	}
	var orderID uuid.UUID
	if !rows.Next() {
		t.Fatal("no order rows")
	}
	if err := rows.Scan(&orderID); err != nil {
		t.Fatal(err)
	}
	rows.Close()

	// The buyer and the seller can both read it; a stranger cannot.
	if _, _, err := ordersSvc.Get(ctx, f.buyer, orderID); err != nil {
		t.Errorf("buyer read: %v", err)
	}
	sellerDetail, isSeller, err := ordersSvc.Get(ctx, f.sellerA, orderID)
	if err != nil || !isSeller {
		t.Fatalf("seller read: detail %+v, isSeller %t, err %v", sellerDetail, isSeller, err)
	}
	if sellerDetail.RecipientPhone == nil || *sellerDetail.RecipientPhone == "" {
		t.Error("seller cannot see the recipient phone")
	}
	if sellerDetail.CommissionPesewas <= 0 {
		t.Errorf("seller commission = %d, want a positive snapshot", sellerDetail.CommissionPesewas)
	}
	if _, _, err := ordersSvc.Get(ctx, f.other, orderID); !errors.Is(err, orders.ErrNotFound) {
		t.Errorf("stranger read = %v, want ErrNotFound", err)
	}

	// The lists see the order from each side.
	buyerItems, total, err := ordersSvc.ListForBuyer(ctx, f.buyer, "", 20, 0)
	if err != nil || total != 1 || len(buyerItems) != 1 {
		t.Errorf("buyer list = %d items, total %d, err %v", len(buyerItems), total, err)
	}
	sellerItems, total, err := ordersSvc.ListForSeller(ctx, f.sellerA, "paid", 20, 0)
	if err != nil || total != 1 || len(sellerItems) != 1 {
		t.Errorf("seller list = %d items, total %d, err %v", len(sellerItems), total, err)
	}
	if _, total, err := ordersSvc.ListForSeller(ctx, f.sellerB, "", 20, 0); err != nil || total != 0 {
		t.Errorf("wrong seller's list total = %d, err %v", total, err)
	}
}
