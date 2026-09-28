package realtime_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/dezmymachine/farmish-backend/internal/messaging/realtime"
)

// TestRealtime_AuthFirstFrame proves the gate: silence, a bad token or a
// token only in the query all close with 4401, while a good first frame
// gets ready.
func TestRealtime_AuthFirstFrame(t *testing.T) {
	h := newHarness(t, testOptions())
	user := uuid.New()
	h.auth.users["good"] = user

	silent, err := dialWS(wsURL(h.server), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer silent.Close()
	if code := closeCode(t, silent); code != realtime.CloseUnauthorized {
		t.Errorf("silent socket code = %d, want 4401", code)
	}

	bad, err := dialWS(wsURL(h.server), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer bad.Close()
	if err := bad.WriteJSON(map[string]string{"type": "auth", "token": "nope"}); err != nil {
		t.Fatal(err)
	}
	if code := closeCode(t, bad); code != realtime.CloseUnauthorized {
		t.Errorf("bad token code = %d, want 4401", code)
	}

	// A token smuggled in the query string is ignored: without an auth
	// frame the socket still dies unauthenticated.
	query, err := dialWS(wsURL(h.server)+"?token=good", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer query.Close()
	if code := closeCode(t, query); code != realtime.CloseUnauthorized {
		t.Errorf("query-token code = %d, want 4401", code)
	}

	conn := h.dial(t, "good")
	if n := h.hub.Sockets(user); n != 1 {
		t.Errorf("sockets = %d, want 1", n)
	}
	_ = conn
}

// TestRealtime_UnknownClientFrame proves anything but auth closes 4400.
func TestRealtime_UnknownClientFrame(t *testing.T) {
	h := newHarness(t, testOptions())
	h.auth.users["good"] = uuid.New()
	conn := h.dial(t, "good")
	if err := conn.WriteJSON(map[string]string{"type": "ping"}); err != nil {
		t.Fatal(err)
	}
	if code := closeCode(t, conn); code != realtime.CloseBadFrame {
		t.Errorf("code = %d, want 4400", code)
	}
}

// TestRealtime_ConnectionCap proves the 6th socket for one user is refused
// with 4429 while the first five stay open.
func TestRealtime_ConnectionCap(t *testing.T) {
	h := newHarness(t, testOptions())
	user := uuid.New()
	h.auth.users["good"] = user
	for i := 0; i < 5; i++ {
		h.dial(t, "good")
	}
	sixth, err := dialWS(wsURL(h.server), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer sixth.Close()
	if err := sixth.WriteJSON(map[string]string{"type": "auth", "token": "good"}); err != nil {
		t.Fatal(err)
	}
	if code := closeCode(t, sixth); code != realtime.CloseTooManySockets {
		t.Errorf("6th socket code = %d, want 4429", code)
	}
	if n := h.hub.Sockets(user); n != 5 {
		t.Errorf("sockets = %d, want 5", n)
	}
}

// TestRealtime_DeliveredToParticipantsOnly proves a publish reaches exactly
// the named users.
func TestRealtime_DeliveredToParticipantsOnly(t *testing.T) {
	h := newHarness(t, testOptions())
	buyer, seller, stranger := uuid.New(), uuid.New(), uuid.New()
	h.auth.users["buyer"] = buyer
	h.auth.users["seller"] = seller
	h.auth.users["stranger"] = stranger
	buyerConn := h.dial(t, "buyer")
	sellerConn := h.dial(t, "seller")
	strangerConn := h.dial(t, "stranger")

	conversationID := uuid.New()
	messageID := uuid.New()
	h.hub.Publish([]uuid.UUID{buyer, seller}, realtime.Frame{
		Type: "message.created", ConversationID: &conversationID,
		Message: &realtime.Message{
			ID: messageID, ConversationID: conversationID, SenderID: buyer,
			Body: "Is this still available?", CreatedAt: time.Now().UTC(),
		},
	})
	for name, conn := range map[string]*websocket.Conn{"buyer": buyerConn, "seller": sellerConn} {
		var frame map[string]any
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if err := conn.ReadJSON(&frame); err != nil {
			t.Fatalf("%s read: %v", name, err)
		}
		if frame["type"] != "message.created" {
			t.Errorf("%s frame = %v", name, frame)
		}
		raw, _ := json.Marshal(frame)
		var decoded realtime.Frame
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("%s decode: %v", name, err)
		}
		if decoded.Message == nil || decoded.Message.ID != messageID || decoded.Message.Body != "Is this still available?" {
			t.Errorf("%s message = %+v", name, decoded.Message)
		}
	}
	_ = strangerConn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, _, err := strangerConn.ReadMessage(); err == nil {
		t.Error("stranger received a frame")
	}
}

// TestRealtime_SlowConsumerDropped proves a full buffer drops only that
// client with 1013, while a healthy client keeps receiving.
func TestRealtime_SlowConsumerDropped(t *testing.T) {
	opts := testOptions()
	opts.SendBuffer = 1
	h := newHarness(t, opts)
	buyer, seller := uuid.New(), uuid.New()
	h.auth.users["buyer"] = buyer
	h.auth.users["seller"] = seller
	slow := h.dial(t, "buyer") // never reads again

	conversationID := uuid.New()
	dropped := false
	for i := 0; i < 5000 && !dropped; i++ {
		h.hub.Publish([]uuid.UUID{buyer}, realtime.Frame{
			Type: "message.created", ConversationID: &conversationID,
			Message: &realtime.Message{ID: uuid.New(), ConversationID: conversationID},
		})
		dropped = h.hub.Sockets(buyer) == 0
	}
	if !dropped {
		t.Fatal("slow buyer never dropped")
	}
	// The drop unregisters first; the 1013 frame follows from the writer.
	_ = slow.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, _, err := slow.ReadMessage()
		if err == nil {
			continue
		}
		var closeErr *websocket.CloseError
		if !errors.As(err, &closeErr) || closeErr.Code != realtime.CloseTryAgain {
			t.Errorf("drop code = %v, want 1013", err)
		}
		break
	}

	// A healthy client is unaffected by another's drop: publish and read in
	// lockstep so its unit buffer never fills.
	healthy := h.dial(t, "seller")
	_ = healthy.SetReadDeadline(time.Now().Add(5 * time.Second))
	for i := 0; i < 5; i++ {
		h.hub.Publish([]uuid.UUID{seller}, realtime.Frame{
			Type: "message.created", ConversationID: &conversationID,
			Message: &realtime.Message{ID: uuid.New(), ConversationID: conversationID, Body: "hello"},
		})
		var frame map[string]any
		if err := healthy.ReadJSON(&frame); err != nil {
			t.Fatalf("healthy read %d: %v", i, err)
		}
		if frame["type"] != "message.created" {
			t.Fatalf("healthy frame = %v", frame)
		}
	}
}

// TestRealtime_TokenExpiry proves a passed exp closes with 4001, while a
// fresh auth frame beforehand keeps the socket open.
func TestRealtime_TokenExpiry(t *testing.T) {
	opts := testOptions()
	opts.PongWait = 5 * time.Second
	h := newHarness(t, opts)
	user := uuid.New()
	h.auth.users["short"] = user
	h.auth.exps["short"] = time.Now().Add(100 * time.Millisecond)
	expiring := h.dial(t, "short")
	if code := closeCode(t, expiring); code != realtime.CloseTokenExpired {
		t.Errorf("expired code = %d, want 4001", code)
	}

	h.auth.exps["short"] = time.Now().Add(100 * time.Millisecond)
	kept := h.dial(t, "short")
	h.auth.exps["short"] = time.Now().Add(time.Hour)
	if err := kept.WriteJSON(map[string]string{"type": "auth", "token": "short"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if n := h.hub.Sockets(user); n != 1 {
		t.Errorf("sockets = %d, want the refreshed one still open", n)
	}
}

// TestRealtime_ShutdownCloses1001 proves hub.Close delivers 1001 to every
// socket and returns once all writers observed it.
func TestRealtime_ShutdownCloses1001(t *testing.T) {
	h := newHarness(t, testOptions())
	user := uuid.New()
	h.auth.users["good"] = user
	first := h.dial(t, "good")
	second := h.dial(t, "good")
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.hub.Close(realtime.CloseGoingAway, "server shutting down")
	}()
	for name, conn := range map[string]*websocket.Conn{"first": first, "second": second} {
		if code := closeCode(t, conn); code != realtime.CloseGoingAway {
			t.Errorf("%s code = %d, want 1001", name, code)
		}
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("hub.Close did not return")
	}
	if n := h.hub.Sockets(user); n != 0 {
		t.Errorf("sockets = %d, want 0", n)
	}
}
