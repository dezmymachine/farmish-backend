package realtime_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/dezmymachine/farmish-backend/internal/auth"
	"github.com/dezmymachine/farmish-backend/internal/messaging/realtime"
	"github.com/dezmymachine/farmish-backend/internal/users"
)

// stubAuth maps tokens to users with controllable expiries.
type stubAuth struct {
	mu    sync.Mutex
	users map[string]uuid.UUID
	exps  map[string]time.Time
}

func (s *stubAuth) Verify(_ context.Context, token string) (auth.Identity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.users[token]
	if !ok {
		return auth.Identity{}, auth.ErrInvalidToken
	}
	return auth.Identity{UID: id.String(), Expires: s.exps[token]}, nil
}

func (s *stubAuth) Resolve(_ context.Context, id auth.Identity) (users.User, error) {
	parsed, err := uuid.Parse(id.UID)
	if err != nil {
		return users.User{}, err
	}
	return users.User{ID: parsed}, nil
}

// harness is a hub behind a real TCP server plus the stub auth.
type harness struct {
	hub    *realtime.Hub
	server *httptest.Server
	auth   *stubAuth
}

func testOptions() realtime.Options {
	opts := realtime.DefaultOptions()
	opts.AuthTimeout = 300 * time.Millisecond
	opts.PingInterval = 50 * time.Millisecond
	opts.PongWait = 200 * time.Millisecond
	opts.WriteTimeout = 2 * time.Second
	return opts
}

func newHarness(t *testing.T, opts realtime.Options) *harness {
	t.Helper()
	auth := &stubAuth{users: map[string]uuid.UUID{}, exps: map[string]time.Time{}}
	hub := realtime.NewHub(opts, nil)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		hub.Serve(r.Context(), conn, auth)
	}))
	t.Cleanup(server.Close)
	return &harness{hub: hub, server: server, auth: auth}
}

func wsURL(server *httptest.Server) string {
	return "ws" + strings.TrimPrefix(server.URL, "http")
}

// dialWS opens a socket, always draining the handshake body whether the
// upgrade succeeds or not.
func dialWS(url string, header http.Header) (*websocket.Conn, error) {
	conn, resp, err := websocket.DefaultDialer.Dial(url, header)
	if resp != nil && resp.Body != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	return conn, err
}

// dial opens a socket and auts it, returning the connection. The caller
// closes it.
func (h *harness) dial(t *testing.T, token string) *websocket.Conn {
	t.Helper()
	conn, err := dialWS(wsURL(h.server), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetPingHandler(func(appData string) error {
		return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(5*time.Second))
	})
	if token != "" {
		if err := conn.WriteJSON(map[string]string{"type": "auth", "token": token}); err != nil {
			t.Fatalf("auth frame: %v", err)
		}
		var frame map[string]any
		if err := conn.ReadJSON(&frame); err != nil {
			t.Fatalf("ready: %v", err)
		}
		if frame["type"] != "ready" {
			t.Fatalf("first frame = %v, want ready", frame)
		}
	}
	return conn
}

// closeCode reads until the server closes and returns the code.
func closeCode(t *testing.T, conn *websocket.Conn) int {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, _, err := conn.ReadMessage()
		if err == nil {
			continue
		}
		var closeErr *websocket.CloseError
		if errors.As(err, &closeErr) {
			return closeErr.Code
		}
		t.Fatalf("read: %v", err)
		return 0
	}
}
