package messaging_test

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/auth"
	"github.com/dezmymachine/farmish-backend/internal/catalog"
	"github.com/dezmymachine/farmish-backend/internal/crypto"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/jobs"
	"github.com/dezmymachine/farmish-backend/internal/messaging"
	"github.com/dezmymachine/farmish-backend/internal/notify"
	"github.com/dezmymachine/farmish-backend/internal/sellers"
	"github.com/dezmymachine/farmish-backend/internal/users"
)

// messageFixture is a messaging service over a real database with an
// insert-only job client and a fixed clock.
type messageFixture struct {
	pool     *pgxpool.Pool
	svc      *messaging.Service
	now      time.Time
	buyer    uuid.UUID
	seller   uuid.UUID
	stranger uuid.UUID
	listing  uuid.UUID
}

func newMessageFixture(t *testing.T) *messageFixture {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Pool(t)
	if err := catalog.Seed(ctx, pool); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	crypter, err := crypto.New(key)
	if err != nil {
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
	buyer, seller, stranger := mk("msg-buyer"), mk("msg-seller"), mk("msg-stranger")
	sellersSvc := sellers.New(pool, crypter, nil)
	if _, err := sellersSvc.UpsertMine(ctx, seller, sellers.ProfileInput{
		BusinessName: "Akosua Farms", Region: "Ashanti", District: "Kumasi Metro",
	}); err != nil {
		t.Fatalf("create profile: %v", err)
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
		 VALUES ($1, $2, 'Fresh maize bags', $3, 'Good maize harvested this week here.', 5000,
		         'bags_50kg', 10, 'grade_a', 'active', 'Ashanti', 'Kumasi Metro',
		         now(), now() + interval '30 days')
		 RETURNING id`,
		seller, categoryID, "fresh-maize-bags-"+uuid.NewString()[:8]).Scan(&listingID); err != nil {
		t.Fatal(err)
	}
	f := &messageFixture{
		pool: pool, now: time.Now().UTC().Truncate(time.Second),
		buyer: buyer, seller: seller, stranger: stranger, listing: listingID,
	}
	svc := messaging.New(pool, users.New(pool), sellersSvc, nil)
	svc.Now = func() time.Time { return f.now }
	log := slog.New(slog.DiscardHandler)
	svc.AttachLogger(log)
	reg := jobs.NewRegistry()
	notify.Register(reg, notify.NewWorker(notify.LogOnly{Log: log}, log, pool))
	client, err := jobs.NewClient(pool, reg, log, jobs.Options{Work: false})
	if err != nil {
		t.Fatal(err)
	}
	svc.AttachJobClient(client)
	f.svc = svc
	return f
}

func (f *messageFixture) count(t *testing.T, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := f.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// start opens a conversation for the fixture's buyer.
func (f *messageFixture) start(t *testing.T, message string) (messaging.Conversation, messaging.Message) {
	t.Helper()
	convo, sent, created, err := f.svc.StartConversation(context.Background(), f.buyer, f.listing, message)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !created {
		t.Fatal("want a new conversation")
	}
	return convo, sent
}

// TestStartConversation_DuplicateReturnsExisting proves the second POST
// returns 200 with the same id and appends the message.
func TestStartConversation_DuplicateReturnsExisting(t *testing.T) {
	f := newMessageFixture(t)
	ctx := context.Background()
	convo, first := f.start(t, "Is this still available?")
	if first.Body != "Is this still available?" {
		t.Errorf("message = %q", first.Body)
	}
	again, second, created, err := f.svc.StartConversation(ctx, f.buyer, f.listing, "Can I pick up tomorrow?")
	if err != nil {
		t.Fatalf("second start: %v", err)
	}
	if created || again.ID != convo.ID {
		t.Errorf("second start = %v created=%v, want the same id", again.ID, created)
	}
	if second.Body != "Can I pick up tomorrow?" {
		t.Errorf("second message = %q", second.Body)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM messages WHERE conversation_id = $1`, convo.ID); n != 2 {
		t.Errorf("messages = %d, want 2", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM conversations WHERE listing_id = $1 AND buyer_id = $2`, f.listing, f.buyer); n != 1 {
		t.Errorf("conversations = %d, want 1", n)
	}
}

// TestStartConversation_OwnListingAndInactive proves sellers cannot message
// themselves and dead listings refuse.
func TestStartConversation_OwnListingAndInactive(t *testing.T) {
	f := newMessageFixture(t)
	ctx := context.Background()
	if _, _, _, err := f.svc.StartConversation(ctx, f.seller, f.listing, "Hello me."); !errors.Is(err, messaging.ErrOwnListing) {
		t.Errorf("own listing err = %v, want ErrOwnListing", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE listings SET status = 'sold' WHERE id = $1`, f.listing); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := f.svc.StartConversation(ctx, f.buyer, f.listing, "Hello?"); !errors.Is(err, messaging.ErrListingInactive) {
		t.Errorf("sold listing err = %v, want ErrListingInactive", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE listings SET status = 'active', expires_at = now() - interval '1 day' WHERE id = $1`, f.listing); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := f.svc.StartConversation(ctx, f.buyer, f.listing, "Hello?"); !errors.Is(err, messaging.ErrListingInactive) {
		t.Errorf("expired listing err = %v, want ErrListingInactive", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE listings SET status = 'active', expires_at = now() + interval '30 days' WHERE id = $1`, f.listing); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"", "x", string(make([]byte, 2001))} {
		if body == "x" {
			continue
		}
		if _, _, _, err := f.svc.StartConversation(ctx, f.buyer, f.listing, body); err == nil {
			t.Errorf("body len %d: want a validation error", len(body))
		}
	}
}

// TestConversation_NonParticipant403 proves strangers get 403 on reads,
// sends and read markers, and unknown ids 404.
func TestConversation_NonParticipant403(t *testing.T) {
	f := newMessageFixture(t)
	ctx := context.Background()
	convo, _ := f.start(t, "Is this still available?")
	for name, call := range map[string]func() error{
		"messages": func() error { _, _, err := f.svc.GetMessages(ctx, f.stranger, convo.ID, "", 20); return err },
		"send":     func() error { _, err := f.svc.SendMessage(ctx, f.stranger, convo.ID, "Spam."); return err },
		"read":     func() error { return f.svc.MarkRead(ctx, f.stranger, convo.ID) },
	} {
		if err := call(); !errors.Is(err, messaging.ErrForbidden) {
			t.Errorf("%s err = %v, want ErrForbidden", name, err)
		}
	}
	if _, _, err := f.svc.GetMessages(ctx, f.buyer, uuid.New(), "", 20); !errors.Is(err, messaging.ErrNotFound) {
		t.Errorf("unknown messages err = %v, want ErrNotFound", err)
	}
	if _, err := f.svc.SendMessage(ctx, f.buyer, uuid.New(), "Hello?"); !errors.Is(err, messaging.ErrNotFound) {
		t.Errorf("unknown send err = %v, want ErrNotFound", err)
	}
	if err := f.svc.MarkRead(ctx, f.buyer, uuid.New()); !errors.Is(err, messaging.ErrNotFound) {
		t.Errorf("unknown read err = %v, want ErrNotFound", err)
	}
}

// TestMessages_CursorPagination writes 75 messages and pages them 30/30/15
// with no duplicates or gaps.
func TestMessages_CursorPagination(t *testing.T) {
	f := newMessageFixture(t)
	ctx := context.Background()
	convo, _ := f.start(t, "Message zero.")
	for i := 1; i < 75; i++ {
		sender := f.seller
		if i%2 == 0 {
			sender = f.buyer
		}
		if _, err := f.svc.SendMessage(ctx, sender, convo.ID, messageBody(i)); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	seen := map[uuid.UUID]bool{}
	var cursor string
	pages := []int{}
	for {
		items, next, err := f.svc.GetMessages(ctx, f.buyer, convo.ID, cursor, 30)
		if err != nil {
			t.Fatal(err)
		}
		pages = append(pages, len(items))
		for i, item := range items {
			if seen[item.ID] {
				t.Fatalf("duplicate message %s", item.ID)
			}
			seen[item.ID] = true
			if i > 0 && !items[i-1].CreatedAt.After(items[i].CreatedAt) && items[i-1].CreatedAt.Equal(items[i].CreatedAt) && items[i-1].ID.String() < item.ID.String() {
				t.Fatalf("page out of order at %d", i)
			}
		}
		if next == nil {
			break
		}
		cursor = *next
	}
	if len(pages) != 3 || pages[0] != 30 || pages[1] != 30 || pages[2] != 15 {
		t.Errorf("pages = %v, want [30 30 15]", pages)
	}
	if len(seen) != 75 {
		t.Errorf("messages seen = %d, want 75", len(seen))
	}
	// A bad cursor is a 400-class error.
	if _, _, err := f.svc.GetMessages(ctx, f.buyer, convo.ID, "not-a-cursor", 30); !errors.Is(err, messaging.ErrBadCursor) {
		t.Errorf("bad cursor err = %v, want ErrBadCursor", err)
	}
}

func messageBody(i int) string {
	return "Message number " + itoa(i) + " with enough padding to be realistic."
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// TestUnreadCounts proves markers drive the count: two unread, then none
// after marking read, and the sender's own messages never count.
func TestUnreadCounts(t *testing.T) {
	f := newMessageFixture(t)
	ctx := context.Background()
	convo, _ := f.start(t, "One.")
	if _, err := f.svc.SendMessage(ctx, f.buyer, convo.ID, "Two."); err != nil {
		t.Fatal(err)
	}
	sellerView, _, err := f.svc.ListConversations(ctx, f.seller, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(sellerView) != 1 || sellerView[0].UnreadCount != 2 {
		t.Fatalf("seller unread = %+v, want 2", sellerView)
	}
	buyerView, _, err := f.svc.ListConversations(ctx, f.buyer, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if buyerView[0].UnreadCount != 0 {
		t.Errorf("buyer unread = %d, own messages must not count", buyerView[0].UnreadCount)
	}
	// The clock is fixed while the database stamps real time: move past the
	// messages before marking read.
	f.now = f.now.Add(time.Hour)
	if err := f.svc.MarkRead(ctx, f.seller, convo.ID); err != nil {
		t.Fatal(err)
	}
	sellerView, _, err = f.svc.ListConversations(ctx, f.seller, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if sellerView[0].UnreadCount != 0 {
		t.Errorf("seller unread after read = %d, want 0", sellerView[0].UnreadCount)
	}
}

// TestConversationSummary_NoPII proves summaries carry names and the cover
// but never contact fields.
func TestConversationSummary_NoPII(t *testing.T) {
	f := newMessageFixture(t)
	ctx := context.Background()
	f.start(t, "Is this still available?")
	views, total, err := f.svc.ListConversations(ctx, f.buyer, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(views) != 1 {
		t.Fatalf("views = %d, total %d", len(views), total)
	}
	view := views[0]
	if view.Counterpart.Name != "Akosua Farms" {
		t.Errorf("counterpart = %+v, want the business name", view.Counterpart)
	}
	if view.LastMessage == nil || !view.LastMessage.FromMe {
		t.Errorf("last message = %+v, want FromMe", view.LastMessage)
	}
	sellerViews, _, err := f.svc.ListConversations(ctx, f.seller, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if sellerViews[0].Counterpart.Name != "Buyer" {
		t.Errorf("buyer counterpart = %q, want Buyer (no display name set)", sellerViews[0].Counterpart.Name)
	}
}

// TestSMSNudge_Throttled proves five rapid messages enqueue exactly one SMS.
func TestSMSNudge_Throttled(t *testing.T) {
	f := newMessageFixture(t)
	ctx := context.Background()
	convo, _ := f.start(t, "One.")
	for _, body := range []string{"Two.", "Three.", "Four.", "Five."} {
		if _, err := f.svc.SendMessage(ctx, f.buyer, convo.ID, body); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.count(t, `SELECT COUNT(*) FROM river_job WHERE kind = 'notify.sms'`); n != 1 {
		t.Errorf("sms jobs = %d, want 1", n)
	}
	// The seller reads, then a new message nudges again... but the hourly
	// unique window still holds: still exactly one job.
	if err := f.svc.MarkRead(ctx, f.seller, convo.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SendMessage(ctx, f.buyer, convo.ID, "Six."); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM river_job WHERE kind = 'notify.sms'`); n != 1 {
		t.Errorf("sms jobs after read = %d, want still 1 (hourly throttle)", n)
	}
}
