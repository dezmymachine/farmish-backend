package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/auth/authtest"
	"github.com/dezmymachine/farmish-backend/internal/catalog"
	"github.com/dezmymachine/farmish-backend/internal/config"
	"github.com/dezmymachine/farmish-backend/internal/crypto"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/internal/jobs"
	"github.com/dezmymachine/farmish-backend/internal/messaging"
	"github.com/dezmymachine/farmish-backend/internal/messaging/realtime"
	"github.com/dezmymachine/farmish-backend/internal/notify"
	"github.com/dezmymachine/farmish-backend/internal/sellers"
	"github.com/dezmymachine/farmish-backend/internal/users"
)

// messageFixture wires the messaging endpoints with the Auth emulator.
type messageFixture struct {
	router *gin.Engine
	pool   *pgxpool.Pool
	msgs   *messaging.Service
	hub    *realtime.Hub
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
	sellersSvc := sellers.New(pool, crypter, nil)
	msgs := messaging.New(pool, users.New(pool), sellersSvc, nil)
	log := slog.New(slog.DiscardHandler)
	msgs.AttachLogger(log)
	reg := jobs.NewRegistry()
	notify.Register(reg, notify.NewWorker(notify.LogOnly{Log: log}, log, pool))
	client, err := jobs.NewClient(pool, reg, log, jobs.Options{Work: false})
	if err != nil {
		t.Fatal(err)
	}
	msgs.AttachJobClient(client)
	hub := realtime.NewHub(realtime.DefaultOptions(), log)
	fb := authtest.Firebase(t)
	router := newTestRouterWithConfig(t, Deps{
		DB: fakePinger{}, Verifier: fb, Users: users.New(pool),
		Sellers: sellersSvc, Messages: msgs, Hub: hub,
	}, config.Config{
		Env: config.EnvTest, CORSOrigins: []string{"https://farmish.gh"},
	})
	return &messageFixture{router: router, pool: pool, msgs: msgs, hub: hub}
}

// sellerWithListing profiles a fresh emulator user and gives them an active
// listing, returning the token, user id and listing id.
func (f *messageFixture) sellerWithListing(t *testing.T, uid, title string) (string, uuid.UUID, uuid.UUID) {
	t.Helper()
	token := withProfile(t, f.pool, uid)
	sellerID := userIDFor(t, f.pool, token)
	var categoryID uuid.UUID
	if err := f.pool.QueryRow(context.Background(),
		`SELECT id FROM categories WHERE parent_id IS NULL LIMIT 1`).Scan(&categoryID); err != nil {
		t.Fatal(err)
	}
	var listingID uuid.UUID
	if err := f.pool.QueryRow(context.Background(),
		`INSERT INTO listings (seller_id, category_id, title, slug, description, price_pesewas,
		                       unit, quantity_available, item_state, status, region, district,
		                       published_at, expires_at)
		 VALUES ($1, $2, $3, $4, 'Good maize harvested this week here.', 5000,
		         'bags_50kg', 10, 'grade_a', 'active', 'Ashanti', 'Kumasi Metro',
		         now(), now() + interval '30 days')
		 RETURNING id`,
		sellerID, categoryID, title, "msg-"+uuid.NewString()[:8]).Scan(&listingID); err != nil {
		t.Fatal(err)
	}
	return token, sellerID, listingID
}

// buyerToken makes a plain emulator user.
func (f *messageFixture) buyerToken(t *testing.T) string {
	t.Helper()
	eu := authtest.EmailUser(t)
	req := meRequest(http.MethodGet, eu.Token, "")
	if w := serve(t, f.router, req); w.Code != http.StatusOK {
		t.Fatalf("buyer setup: %d %s", w.Code, w.Body.String())
	}
	return eu.Token
}

// TestMessagingEndpoints_HappyPath walks start → send → history → read →
// list, every response contract-valid.
func TestMessagingEndpoints_HappyPath(t *testing.T) {
	f := newMessageFixture(t)
	sellerTok, _, listingID := f.sellerWithListing(t, "msg-happy-seller@example.com", "Happy maize")
	buyerTok := f.buyerToken(t)

	req := jsonRequest(http.MethodPost, "/v1/conversations", buyerTok,
		`{"listingId": "`+listingID.String()+`", "message": "Is this still available?"}`)
	w := serve(t, f.router, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("start = %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var convo api.Conversation
	if err := json.Unmarshal(w.Body.Bytes(), &convo); err != nil {
		t.Fatal(err)
	}

	req = jsonRequest(http.MethodPost, "/v1/conversations/"+convo.Id.String()+"/messages", sellerTok,
		`{"body": "Yes, pickup tomorrow."}`)
	w = serve(t, f.router, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("send = %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)

	req = jsonRequest(http.MethodGet, "/v1/conversations/"+convo.Id.String()+"/messages?limit=30", buyerTok, "")
	w = serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("history = %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var history api.MessageList
	if err := json.Unmarshal(w.Body.Bytes(), &history); err != nil {
		t.Fatal(err)
	}
	if len(history.Items) != 2 || history.NextCursor != nil {
		t.Errorf("history = %+v", history)
	}

	req = jsonRequest(http.MethodPost, "/v1/conversations/"+convo.Id.String()+"/read", buyerTok, "")
	if w := serve(t, f.router, req); w.Code != http.StatusNoContent {
		t.Fatalf("read = %d %s", w.Code, w.Body.String())
	} else {
		assertContract(t, req, w)
	}

	req = jsonRequest(http.MethodGet, "/v1/conversations", buyerTok, "")
	w = serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var list api.ConversationList
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].UnreadCount != 0 {
		t.Errorf("list = %+v, want one read conversation", list)
	}
	if list.Items[0].Counterpart.Name == "" {
		t.Errorf("counterpart = %+v, want the seller business name", list.Items[0].Counterpart)
	}
	for _, body := range [][]byte{w.Body.Bytes()} {
		for _, key := range []string{"phone", "email", "firebaseUid"} {
			if strings.Contains(string(body), `"`+key+`"`) {
				t.Errorf("response contains %q", key)
			}
		}
	}
}

// TestMessagingEndpoints_DuplicateOwnInactiveStranger covers the failure
// paths: duplicate start (200), own listing (403), dead listing (404),
// stranger (403), bad bodies (400), anonymous (401).
func TestMessagingEndpoints_DuplicateOwnInactiveStranger(t *testing.T) {
	f := newMessageFixture(t)
	sellerTok, _, listingID := f.sellerWithListing(t, "msg-fail-seller@example.com", "Fail maize")
	buyerTok := f.buyerToken(t)
	strangerTok := f.buyerToken(t)

	start := func(token, body string) (int, *http.Request) {
		req := jsonRequest(http.MethodPost, "/v1/conversations", token, body)
		w := serve(t, f.router, req)
		return w.Code, req
	}
	body := `{"listingId": "` + listingID.String() + `", "message": "Hello."}`
	if code, _ := start(buyerTok, body); code != http.StatusCreated {
		t.Fatalf("start = %d", code)
	}
	req := jsonRequest(http.MethodPost, "/v1/conversations", buyerTok, body)
	w := serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Errorf("duplicate start = %d, want 200", w.Code)
	} else {
		assertContract(t, req, w)
	}
	// Own listing.
	req = jsonRequest(http.MethodPost, "/v1/conversations", sellerTok, body)
	if w := serve(t, f.router, req); w.Code != http.StatusForbidden {
		t.Errorf("own listing = %d, want 403", w.Code)
	} else {
		assertErrorEnvelope(t, w.Body.Bytes(), apierror.CodeForbidden)
		assertContract(t, req, w)
	}
	// Dead listing.
	if _, err := f.pool.Exec(context.Background(), `UPDATE listings SET status = 'sold' WHERE id = $1`, listingID); err != nil {
		t.Fatal(err)
	}
	req = jsonRequest(http.MethodPost, "/v1/conversations", strangerTok, body)
	if w := serve(t, f.router, req); w.Code != http.StatusNotFound {
		t.Errorf("sold listing = %d, want 404", w.Code)
	} else {
		assertContract(t, req, w)
	}
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE listings SET status = 'active', expires_at = now() + interval '30 days' WHERE id = $1`, listingID); err != nil {
		t.Fatal(err)
	}

	// Stranger on every conversation route.
	var convoID uuid.UUID
	if err := f.pool.QueryRow(context.Background(),
		`SELECT id FROM conversations WHERE listing_id = $1`, listingID).Scan(&convoID); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path, body string
	}{
		{http.MethodGet, "/v1/conversations/" + convoID.String() + "/messages", ""},
		{http.MethodPost, "/v1/conversations/" + convoID.String() + "/messages", `{"body":"Spam."}`},
		{http.MethodPost, "/v1/conversations/" + convoID.String() + "/read", ""},
	} {
		req := jsonRequest(tc.method, tc.path, strangerTok, tc.body)
		w := serve(t, f.router, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s = %d, want 403", tc.method, tc.path, w.Code)
		} else {
			assertContract(t, req, w)
		}
	}
	// Bad bodies.
	for _, bad := range []string{`{"listingId": "` + listingID.String() + `"}`, `{"listingId": "nope", "message": "Hi."}`, ``} {
		req := jsonRequest(http.MethodPost, "/v1/conversations", buyerTok, bad)
		if w := serve(t, f.router, req); w.Code != http.StatusBadRequest {
			t.Errorf("body %q = %d, want 400", bad, w.Code)
		}
	}
	// Anonymous.
	req = jsonRequest(http.MethodGet, "/v1/conversations", "", "")
	if w := serve(t, f.router, req); w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous = %d, want 401", w.Code)
	} else {
		assertContract(t, req, w)
	}
}

// TestMessagingEndpoints_RateLimit proves the 11th rapid send is a 429 with
// headers.
func TestMessagingEndpoints_RateLimit(t *testing.T) {
	f := newMessageFixture(t)
	_, _, listingID := f.sellerWithListing(t, "msg-limit-seller@example.com", "Limit maize")
	buyerTok := f.buyerToken(t)

	req := jsonRequest(http.MethodPost, "/v1/conversations", buyerTok,
		`{"listingId": "`+listingID.String()+`", "message": "Hello."}`)
	w := serve(t, f.router, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("start = %d %s", w.Code, w.Body.String())
	}
	var convo api.Conversation
	if err := json.Unmarshal(w.Body.Bytes(), &convo); err != nil {
		t.Fatal(err)
	}
	var limited *httptest.ResponseRecorder
	var limitedReq *http.Request
	for i := 0; i < 11; i++ {
		req := jsonRequest(http.MethodPost, "/v1/conversations/"+convo.Id.String()+"/messages", buyerTok,
			`{"body": "Ping."}`)
		w := serve(t, f.router, req)
		if w.Code == http.StatusTooManyRequests {
			limited, limitedReq = w, req
			break
		}
		if w.Code != http.StatusCreated {
			t.Fatalf("send %d = %d %s", i, w.Code, w.Body.String())
		}
	}
	if limited == nil {
		t.Fatal("messaging policy never limited 11 sends")
	}
	assertErrorEnvelope(t, limited.Body.Bytes(), apierror.CodeRateLimited)
	if limited.Header().Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}
	assertContract(t, limitedReq, limited)
}

// TestMessagingEndpoints_WebSocket proves the upgrade route: an authenticated
// socket gets ready and receives a REST send live; a bad origin is refused
// and silence is closed.
func TestMessagingEndpoints_WebSocket(t *testing.T) {
	f := newMessageFixture(t)
	sellerTok, _, listingID := f.sellerWithListing(t, "msg-ws-seller@example.com", "WS maize")
	buyerTok := f.buyerToken(t)

	server := httptest.NewServer(f.router)
	defer server.Close()
	wsURL := "ws" + server.URL[len("http"):] + "/v1/ws"

	dial := func(origin, token string) (*websocket.Conn, error) {
		header := http.Header{}
		if origin != "" {
			header.Set("Origin", origin)
		}
		conn, resp, err := websocket.DefaultDialer.Dial(wsURL, header)
		if resp != nil && resp.Body != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		if err != nil {
			return nil, err
		}
		if token != "" {
			if err := conn.WriteJSON(map[string]string{"type": "auth", "token": token}); err != nil {
				_ = conn.Close()
				return nil, err
			}
			var frame map[string]any
			_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
			if err := conn.ReadJSON(&frame); err != nil {
				_ = conn.Close()
				return nil, err
			}
			if frame["type"] != "ready" {
				_ = conn.Close()
				return nil, fmt.Errorf("first frame = %v", frame)
			}
		}
		return conn, nil
	}

	// Bad origin is refused at upgrade.
	if _, err := dial("https://evil.test", ""); err == nil {
		t.Error("evil origin upgraded")
	}
	buyerConn, err := dial("https://farmish.gh", buyerTok)
	if err != nil {
		t.Fatalf("buyer dial: %v", err)
	}
	defer buyerConn.Close()
	sellerConn, err := dial("https://farmish.gh", sellerTok)
	if err != nil {
		t.Fatalf("seller dial: %v", err)
	}
	defer sellerConn.Close()

	// Run the production listener: a REST send must reach both sockets live.
	listenCtx, stopListener := context.WithCancel(context.Background())
	defer stopListener()
	go func() {
		if err := realtime.NewListener(f.pool, f.hub, nil).Run(listenCtx); err != nil {
			t.Error(err)
		}
	}()

	req := jsonRequest(http.MethodPost, "/v1/conversations", buyerTok,
		`{"listingId": "`+listingID.String()+`", "message": "Real time?"}`)
	w := serve(t, f.router, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("start = %d %s", w.Code, w.Body.String())
	}
	for name, conn := range map[string]*websocket.Conn{"buyer": buyerConn, "seller": sellerConn} {
		var frame map[string]any
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		if err := conn.ReadJSON(&frame); err != nil {
			t.Fatalf("%s read: %v", name, err)
		}
		if frame["type"] != "message.created" {
			t.Fatalf("%s frame = %v", name, frame)
		}
		raw, _ := json.Marshal(frame)
		assertRealtimeContract(t, raw)
	}
}

// assertRealtimeContract validates a realtime frame against the RealtimeEvent
// oneOf in api/openapi.yaml: the codegen server cannot serve 101 upgrades,
// so frames are checked as raw JSON instead of HTTP responses.
func assertRealtimeContract(t *testing.T, raw []byte) {
	t.Helper()
	spec, err := api.GetSpec()
	if err != nil {
		t.Fatal(err)
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("frame %s is not JSON: %v", raw, err)
	}
	if err := spec.Components.Schemas["RealtimeEvent"].Value.VisitJSON(decoded); err != nil {
		t.Errorf("frame %s violates RealtimeEvent: %v", raw, err)
	}
}
