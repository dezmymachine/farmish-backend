package orders_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/auth"
	"github.com/dezmymachine/farmish-backend/internal/catalog"
	"github.com/dezmymachine/farmish-backend/internal/database"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/orders"
	"github.com/dezmymachine/farmish-backend/internal/users"
)

// handWrittenTable is DOMAIN §4 typed out by hand from the doc. The code's
// table is compared against it, so a typo in either one fails the test.
var handWrittenTable = map[string]map[string][]string{
	"pending_payment": {
		"paid":    {"system"},
		"expired": {"system"},
	},
	"expired": {
		"paid":      {"system"},
		"cancelled": {"system"},
	},
	"paid": {
		"accepted":  {"seller"},
		"cancelled": {"seller", "buyer", "system"},
	},
	"accepted": {
		"cancelled": {"seller"},
		"shipped":   {"seller"},
	},
	"shipped": {
		"delivered": {"seller"},
		"completed": {"buyer"},
		"disputed":  {"buyer"},
	},
	"delivered": {
		"completed": {"buyer", "system"},
		"disputed":  {"buyer"},
	},
	"disputed": {
		"refunded":  {"admin"},
		"completed": {"admin"},
	},
}

type tableFixture struct {
	pool     *pgxpool.Pool
	svc      *orders.Service
	buyer    uuid.UUID
	seller   uuid.UUID
	stranger uuid.UUID
	orderID  uuid.UUID
}

// newTableFixture creates one real order row. The order is seeded directly
// because the full-table test only exercises Transition's validation, not how
// an order came to exist; production code has exactly one status writer.
func newTableFixture(t *testing.T) *tableFixture {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Pool(t)
	if err := catalog.Seed(ctx, pool); err != nil {
		t.Fatal(err)
	}
	mk := func(uid string) uuid.UUID {
		user, err := users.New(pool).Resolve(ctx, auth.Identity{
			UID: uid, Email: uid + "@farmish.test", Provider: "password",
		})
		if err != nil {
			t.Fatal(err)
		}
		return user.ID
	}
	f := &tableFixture{
		pool: pool, svc: orders.NewService(pool, 48*time.Hour, 3*24*time.Hour),
		buyer: mk("table-buyer"), seller: mk("table-seller"), stranger: mk("table-stranger"),
	}
	f.orderID = seedOrder(t, f)
	return f
}

// seedOrder writes the minimum checkout/order rows, as the ledger tests do.
func seedOrder(t *testing.T, f *tableFixture) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var checkoutID, orderID uuid.UUID
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO checkouts (buyer_id, idempotency_key, request_hash, base_pesewas,
		                        processing_fee_pesewas, charge_pesewas, expires_at)
		 VALUES ($1, $2, 'table-test', 1000, 20, 1020, now() + interval '30 minutes')
		 RETURNING id`, f.buyer, uuid.New()).Scan(&checkoutID); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO orders (checkout_id, buyer_id, seller_id, status, escrow_state,
		                     subtotal_pesewas, delivery_fee_pesewas, base_pesewas,
		                     commission_rate_bps, commission_pesewas, delivery_method)
		 VALUES ($1, $2, $3, 'paid', 'held', 1000, 0, 1000, 500, 50, 'pickup')
		 RETURNING id`, checkoutID, f.buyer, f.seller).Scan(&orderID); err != nil {
		t.Fatal(err)
	}
	return orderID
}

// seedStatus puts the order into `from` for one attempt. Tests own this; the
// app has exactly one status writer.
func (f *tableFixture) seedStatus(t *testing.T, status string) {
	t.Helper()
	if _, err := f.pool.Exec(t.Context(),
		`UPDATE orders SET status = $2,
		   escrow_state = CASE WHEN $2 IN ('paid','accepted','shipped','delivered') THEN 'held' ELSE 'none' END,
		   paid_at = now(), accepted_at = now(), shipped_at = now(), delivered_at = now(),
		   auto_complete_at = now() + interval '3 days'
		 WHERE id = $1`, f.orderID, status); err != nil {
		t.Fatalf("seed %s: %v", status, err)
	}
}

// attempt runs one transition inside a transaction that is always rolled
// back, so each combination starts from the seeded state untouched.
func (f *tableFixture) attempt(ctx context.Context, to string, actor orders.Actor) error {
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, _, err = f.svc.Transition(ctx, tx, f.orderID, to, actor, "table-test")
	return err
}

// TestTransition_TableMatchesDomain compares the code's table with a
// hand-written copy of DOMAIN §4, cell by cell.
func TestTransition_TableMatchesDomain(t *testing.T) {
	code := orders.ExposeTransitionTable()
	for _, from := range orders.AllStatuses {
		for _, to := range orders.AllStatuses {
			want := handWrittenTable[from][to]
			got := code[from][to]
			if len(want) != len(got) {
				t.Errorf("%s -> %s: code allows %v, DOMAIN §4 says %v", from, to, got, want)
				continue
			}
			for i, actor := range want {
				if got[i] != actor {
					t.Errorf("%s -> %s: code allows %v, DOMAIN §4 says %v", from, to, got, want)
				}
			}
		}
	}
}

// TestTransition_FullTable exercises every (from, to, actor type) pair
// against the real database: each type is tried as the right party (the
// order's own buyer or seller, or a party-unchecked system/admin), as a
// stranger wearing that type, and against moves the table does not contain.
// Allowed pairs succeed; the wrong party is Forbidden; everything else is
// InvalidTransition.
func TestTransition_FullTable(t *testing.T) {
	f := newTableFixture(t)
	ctx := t.Context()

	for _, from := range orders.AllStatuses {
		for _, to := range orders.AllStatuses {
			inTable := map[string]bool{}
			for _, actorType := range handWrittenTable[from][to] {
				inTable[actorType] = true
			}
			for _, actorType := range orders.AllActorTypes {
				// Each actor type is tried as the right party and, for the
				// party-checked types, as a stranger wearing the same type.
				rightParty := orders.Actor{Type: actorType, ID: &f.stranger}
				switch actorType {
				case orders.ActorBuyer:
					rightParty = orders.Buyer(f.buyer)
				case orders.ActorSeller:
					rightParty = orders.Seller(f.seller)
				case orders.ActorSystem:
					rightParty = orders.System()
				}
				stranger := orders.Actor{Type: actorType, ID: &f.stranger}
				for _, attempt := range []struct {
					name    string
					actor   orders.Actor
					allowed bool
					wantErr string // "", "forbidden", "invalid"
				}{
					{
						name:    actorType + "/right-party",
						actor:   rightParty,
						allowed: inTable[actorType],
						wantErr: boolToWant(inTable[actorType]),
					},
					// A stranger matters only where the type is party-checked:
					// buyer and seller moves must come from this order's party.
					{
						name:    actorType + "/stranger",
						actor:   stranger,
						allowed: inTable[actorType] && !partyChecked(actorType),
						wantErr: wantFor(inTable[actorType], partyChecked(actorType)),
					},
				} {
					label := from + " -> " + to + " as " + attempt.name
					f.seedStatus(t, from)
					err := f.attempt(ctx, to, attempt.actor)
					switch {
					case attempt.allowed && err != nil:
						t.Errorf("%s: err = %v, want success", label, err)
					case attempt.allowed:
						continue
					case err == nil:
						t.Errorf("%s: succeeded, want refused", label)
					case attempt.wantErr == "forbidden" && !orders.IsForbidden(err):
						t.Errorf("%s: err = %v, want Forbidden", label, err)
					case attempt.wantErr == "invalid" && !orders.IsInvalidTransition(err):
						t.Errorf("%s: err = %v, want InvalidTransition", label, err)
					}
				}
			}
		}
	}
}

// partyChecked reports whether the type must be this order's party.
func partyChecked(actorType string) bool {
	return actorType == orders.ActorBuyer || actorType == orders.ActorSeller
}

func boolToWant(allowed bool) string {
	if allowed {
		return ""
	}
	return "invalid"
}

func wantFor(inTable, checked bool) string {
	switch {
	case inTable && checked:
		return "forbidden"
	case inTable:
		return "" // party-unchecked type: the stranger pass is a valid pass
	default:
		return "invalid"
	}
}

// TestTransition_WritesEventAndTimestamp proves a legal move writes the event
// row and stamps the target status's timestamp.
func TestTransition_WritesEventAndTimestamp(t *testing.T) {
	f := newTableFixture(t)
	ctx := t.Context()
	f.seedStatus(t, orders.StatusPaid)

	var accepted orders.Order
	err := database.InTx(ctx, f.pool, func(tx pgx.Tx) error {
		moved, effects, err := f.svc.Transition(ctx, tx, f.orderID, orders.StatusAccepted, orders.Seller(f.seller), "")
		if err != nil {
			return err
		}
		accepted = moved
		// The notify effect enqueues nothing here (no jobs client), which is
		// fine: the full-table test covers validation, not enqueuing.
		_ = effects
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Status != orders.StatusAccepted || accepted.AcceptedAt == nil {
		t.Errorf("accepted = %+v, want the timestamp stamped", accepted)
	}
	var from, to, actorType string
	if err := f.pool.QueryRow(ctx,
		`SELECT coalesce(from_status,''), to_status, actor_type FROM order_events
		 WHERE order_id = $1 ORDER BY id DESC LIMIT 1`, f.orderID).Scan(&from, &to, &actorType); err != nil {
		t.Fatal(err)
	}
	if from != orders.StatusPaid || to != orders.StatusAccepted || actorType != orders.ActorSeller {
		t.Errorf("event = %s -> %s by %s, want paid -> accepted by seller", from, to, actorType)
	}
}
