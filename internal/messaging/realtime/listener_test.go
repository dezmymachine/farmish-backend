package realtime_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/catalog"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/messaging/realtime"
)

// dialAs registers a token for a user on this harness and opens an
// authenticated socket for them.
func (h *harness) dialAs(t *testing.T, token string, userID uuid.UUID) *websocket.Conn {
	t.Helper()
	h.auth.mu.Lock()
	h.auth.users[token] = userID
	h.auth.mu.Unlock()
	return h.dial(t, token)
}

// listenWorld is one database with two hubs, each behind its own server,
// and a seeded conversation.
type listenWorld struct {
	pool         *pgxpool.Pool
	ctx          context.Context
	cancel       context.CancelFunc
	h1, h2       *harness
	buyer        uuid.UUID
	seller       uuid.UUID
	conversation uuid.UUID
}

func newListenWorld(t *testing.T) *listenWorld {
	t.Helper()
	pool := dbtest.Pool(t)
	// Cancel before the pool closes (cleanup LIFO): the listeners must
	// release their connections first, or Pool.Close hangs.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := catalog.Seed(ctx, pool); err != nil {
		t.Fatal(err)
	}
	mk := func(uid string) uuid.UUID {
		var id uuid.UUID
		if err := pool.QueryRow(ctx,
			`INSERT INTO users (firebase_uid, signup_method) VALUES ($1, 'email') RETURNING id`,
			uid).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	buyer, seller := mk("listen-buyer"), mk("listen-seller")
	if _, err := pool.Exec(ctx,
		`INSERT INTO seller_profiles (user_id, business_name, region, district)
		 VALUES ($1, 'Listen Farm', 'Ashanti', 'Kumasi Metro')`, seller); err != nil {
		t.Fatal(err)
	}
	var categoryID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM categories WHERE parent_id IS NULL LIMIT 1`).Scan(&categoryID); err != nil {
		t.Fatal(err)
	}
	var listingID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO listings (seller_id, category_id, title, slug, description, price_pesewas,
		                       unit, quantity_available, item_state, status, region, district,
		                       published_at, expires_at)
		 VALUES ($1, $2, 'Listen maize', $3, 'Good maize harvested this week here.', 5000,
		         'bags_50kg', 10, 'grade_a', 'active', 'Ashanti', 'Kumasi Metro',
		         now(), now() + interval '30 days')
		 RETURNING id`,
		seller, categoryID, "listen-maize-"+uuid.NewString()[:8]).Scan(&listingID); err != nil {
		t.Fatal(err)
	}
	var conversationID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO conversations (listing_id, buyer_id, seller_id) VALUES ($1, $2, $3) RETURNING id`,
		listingID, buyer, seller).Scan(&conversationID); err != nil {
		t.Fatal(err)
	}
	return &listenWorld{
		pool: pool, ctx: ctx, cancel: cancel,
		h1: newHarness(t, testOptions()), h2: newHarness(t, testOptions()),
		buyer: buyer, seller: seller, conversation: conversationID,
	}
}

// notify publishes one messaging event the way the service does: inside a
// committed transaction.
func (w *listenWorld) notify(t *testing.T, payload map[string]any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.pool.Exec(w.ctx, `SELECT pg_notify('messaging_events', $1)`, string(raw)); err != nil {
		t.Fatal(err)
	}
}

// drainConn reads a socket forever: gorilla only answers server pings
// while the app reads, so an idle test socket would die on the pong
// deadline. Frames land on ch for assertions with timeouts.
type drainConn struct {
	ch chan map[string]any
}

func drain(t *testing.T, conn *websocket.Conn) *drainConn {
	t.Helper()
	d := &drainConn{ch: make(chan map[string]any, 64)}
	go func() {
		for {
			var frame map[string]any
			if err := conn.ReadJSON(&frame); err != nil {
				return
			}
			select {
			case d.ch <- frame:
			default:
			}
		}
	}()
	return d
}

// next returns the next frame within timeout, or nil on silence.
func (d *drainConn) next(timeout time.Duration) map[string]any {
	select {
	case frame := <-d.ch:
		return frame
	case <-time.After(timeout):
		return nil
	}
}

// TestRealtime_CrossInstance proves two hubs on one DB: a committed write
// handled anywhere reaches a socket on the other hub.
func TestRealtime_CrossInstance(t *testing.T) {
	w := newListenWorld(t)
	go runListener(t, w.pool, w.h1.hub, w.ctx)
	go runListener(t, w.pool, w.h2.hub, w.ctx)
	buyer := drain(t, w.h2.dialAs(t, "buyer", w.buyer))
	seller := drain(t, w.h2.dialAs(t, "seller", w.seller))

	var messageID uuid.UUID
	if err := w.pool.QueryRow(w.ctx,
		`INSERT INTO messages (conversation_id, sender_id, body) VALUES ($1, $2, 'Is this still available?') RETURNING id`,
		w.conversation, w.buyer).Scan(&messageID); err != nil {
		t.Fatal(err)
	}
	w.notify(t, map[string]any{
		"kind": "message.created", "conversationId": w.conversation, "messageId": messageID,
		"participantIds": []uuid.UUID{w.buyer, w.seller},
	})
	for name, d := range map[string]*drainConn{"buyer": buyer, "seller": seller} {
		frame := d.next(5 * time.Second)
		if frame == nil {
			t.Fatalf("%s got no frame", name)
		}
		if frame["type"] != "message.created" {
			t.Fatalf("%s frame = %v", name, frame)
		}
		message, _ := frame["message"].(map[string]any)
		if message["id"] != messageID.String() || message["body"] != "Is this still available?" {
			t.Errorf("%s message = %v", name, message)
		}
	}
}

// TestRealtime_RollbackEmitsNothing proves an aborted transaction emits no
// event, while the committed write right after does.
func TestRealtime_RollbackEmitsNothing(t *testing.T) {
	w := newListenWorld(t)
	go runListener(t, w.pool, w.h1.hub, w.ctx)
	seller := drain(t, w.h1.dialAs(t, "seller", w.seller))

	tx, err := w.pool.Begin(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var messageID uuid.UUID
	if err := tx.QueryRow(w.ctx,
		`INSERT INTO messages (conversation_id, sender_id, body) VALUES ($1, $2, 'Lost.') RETURNING id`,
		w.conversation, w.buyer).Scan(&messageID); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{
		"kind": "message.created", "conversationId": w.conversation, "messageId": messageID,
		"participantIds": []uuid.UUID{w.buyer, w.seller},
	})
	if _, err := tx.Exec(w.ctx, `SELECT pg_notify('messaging_events', $1)`, string(raw)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(w.ctx); err != nil {
		t.Fatal(err)
	}
	if frame := seller.next(500 * time.Millisecond); frame != nil {
		t.Fatalf("rolled-back write produced a frame: %v", frame)
	}

	var committedID uuid.UUID
	if err := w.pool.QueryRow(w.ctx,
		`INSERT INTO messages (conversation_id, sender_id, body) VALUES ($1, $2, 'Kept.') RETURNING id`,
		w.conversation, w.buyer).Scan(&committedID); err != nil {
		t.Fatal(err)
	}
	w.notify(t, map[string]any{
		"kind": "message.created", "conversationId": w.conversation, "messageId": committedID,
		"participantIds": []uuid.UUID{w.buyer, w.seller},
	})
	frame := seller.next(5 * time.Second)
	if frame == nil {
		t.Fatal("committed write produced no frame")
	}
	if message, _ := frame["message"].(map[string]any); message["id"] != committedID.String() {
		t.Errorf("frame = %v", frame)
	}
}

// TestRealtime_ReadEvent proves mark-read publishes per-recipient frames
// with the right byMe.
func TestRealtime_ReadEvent(t *testing.T) {
	w := newListenWorld(t)
	go runListener(t, w.pool, w.h1.hub, w.ctx)
	buyer := drain(t, w.h1.dialAs(t, "buyer", w.buyer))
	seller := drain(t, w.h1.dialAs(t, "seller", w.seller))

	now := time.Now().UTC().Truncate(time.Second)
	w.notify(t, map[string]any{
		"kind": "conversation.read", "conversationId": w.conversation,
		"readerId": w.buyer, "readAt": now.Format(time.RFC3339Nano),
		"participantIds": []uuid.UUID{w.buyer, w.seller},
	})
	buyerFrame := buyer.next(5 * time.Second)
	sellerFrame := seller.next(5 * time.Second)
	if buyerFrame == nil || buyerFrame["type"] != "conversation.read" || buyerFrame["byMe"] != true {
		t.Errorf("buyer frame = %v", buyerFrame)
	}
	if sellerFrame == nil || sellerFrame["type"] != "conversation.read" || sellerFrame["byMe"] != false {
		t.Errorf("seller frame = %v", sellerFrame)
	}
}

// TestRealtime_ListenerReconnectResync proves a severed listener reconnects
// and tells every local socket to refetch.
func TestRealtime_ListenerReconnectResync(t *testing.T) {
	w := newListenWorld(t)
	go runListener(t, w.pool, w.h1.hub, w.ctx)
	seller := drain(t, w.h1.dialAs(t, "seller", w.seller))

	var pid int32
	if err := w.pool.QueryRow(w.ctx,
		`SELECT pid FROM pg_stat_activity WHERE query = 'LISTEN messaging_events' AND datname = current_database() LIMIT 1`).Scan(&pid); err != nil {
		t.Fatalf("find listener backend: %v", err)
	}
	if _, err := w.pool.Exec(w.ctx, `SELECT pg_terminate_backend($1)`, pid); err != nil {
		t.Fatal(err)
	}
	frame := seller.next(10 * time.Second)
	if frame == nil || frame["type"] != "resync" {
		t.Errorf("frame = %v, want resync", frame)
	}
}

// runListener runs a LISTEN listener until ctx ends, failing the test on an
// unexpected (non-cancellation) error.
func runListener(t *testing.T, pool *pgxpool.Pool, hub *realtime.Hub, ctx context.Context) {
	t.Helper()
	go func() {
		if err := realtime.NewListener(pool, hub, nil).Run(ctx); err != nil {
			t.Error(err)
		}
	}()
}
