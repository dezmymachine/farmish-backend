package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gorilla/websocket"

	"github.com/dezmymachine/farmish-backend/internal/auth"
	"github.com/dezmymachine/farmish-backend/internal/users"
)

// errSocketCap means the user already holds MaxSocketsPerUser sockets.
var errSocketCap = errors.New("socket cap hit")

// Authenticator verifies the first-frame token and resolves it to a user,
// through the same Verifier + Users path as the HTTP auth middleware.
type Authenticator interface {
	Verify(ctx context.Context, token string) (auth.Identity, error)
	Resolve(ctx context.Context, id auth.Identity) (users.User, error)
}

// clientFrame is what a socket may send: auth only.
type clientFrame struct {
	Type  string `json:"type"`
	Token string `json:"token"`
}

// Serve runs one socket from upgrade to disconnect: the auth phase, then the
// read and write pumps. It blocks until the socket is gone.
func (h *Hub) Serve(ctx context.Context, conn *websocket.Conn, authn Authenticator) {
	c := &Client{
		conn: conn, send: make(chan []byte, h.opts.SendBuffer),
		closeWith: make(chan closeRequest, 1), done: make(chan struct{}), hub: h,
	}
	go c.writePump()
	c.readPump(ctx, authn)
	h.remove(c)
}

// readPump authenticates the socket, registers it, then enforces the
// auth-only rule and the token-expiry timer until the socket dies.
func (c *Client) readPump(ctx context.Context, authn Authenticator) {
	h := c.hub
	exp, code, err := c.authenticate(ctx, authn)
	if err != nil {
		c.requestClose(code, "authentication required")
		c.waitFlushed()
		return
	}
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(nextDeadline(h.now(), h.opts.PongWait, exp))
	})
	for {
		_ = c.conn.SetReadDeadline(nextDeadline(h.now(), h.opts.PongWait, exp))
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			if expired(h.now(), exp) {
				c.requestClose(CloseTokenExpired, "token expired, re-authenticate")
				c.waitFlushed()
			}
			return
		}
		var frame clientFrame
		if err := json.Unmarshal(raw, &frame); err != nil || frame.Type != "auth" || frame.Token == "" {
			c.requestClose(CloseBadFrame, "auth frames only")
			c.waitFlushed()
			return
		}
		id, _, err := verify(ctx, authn, frame.Token)
		if err != nil {
			c.requestClose(CloseUnauthorized, "authentication required")
			c.waitFlushed()
			return
		}
		exp = id.Expires
		if expired(h.now(), exp) {
			c.requestClose(CloseTokenExpired, "token expired, re-authenticate")
			c.waitFlushed()
			return
		}
	}
}

// expired reports whether the token passed exp. Tokens without an exp
// never expire this way.
func expired(now, exp time.Time) bool {
	return !exp.IsZero() && !now.Before(exp)
}

// nextDeadline returns the earlier of now+PongWait and the token expiry (an
// idle socket wakes at exp even with no traffic).
func nextDeadline(now time.Time, pongWait time.Duration, exp time.Time) time.Time {
	deadline := now.Add(pongWait)
	if !exp.IsZero() && exp.Before(deadline) {
		return exp
	}
	return deadline
}

// waitFlushed gives the writer a moment to flush a close frame before the
// deferred remove tears the connection down.
func (c *Client) waitFlushed() {
	select {
	case <-c.done:
	case <-time.After(c.hub.opts.WriteTimeout):
	}
}

// authenticate reads the first frame within the auth window, verifies it,
// registers the socket and answers ready. It returns the token expiry and,
// on failure, the close code to send.
func (c *Client) authenticate(ctx context.Context, authn Authenticator) (time.Time, int, error) {
	h := c.hub
	if err := c.conn.SetReadDeadline(h.now().Add(h.opts.AuthTimeout)); err != nil {
		return time.Time{}, CloseUnauthorized, err
	}
	_, raw, err := c.conn.ReadMessage()
	if err != nil {
		return time.Time{}, CloseUnauthorized, err
	}
	var frame clientFrame
	if err := json.Unmarshal(raw, &frame); err != nil || frame.Type != "auth" || frame.Token == "" {
		return time.Time{}, CloseUnauthorized, fmt.Errorf("first frame is not auth")
	}
	id, user, err := verify(ctx, authn, frame.Token)
	if err != nil {
		return time.Time{}, CloseUnauthorized, err
	}
	c.userID = user.ID
	if !h.add(c) {
		return time.Time{}, CloseTooManySockets, fmt.Errorf("%w: user %s", errSocketCap, user.ID)
	}
	c.sendFrame(Frame{Type: "ready"})
	return id.Expires, 0, nil
}

// verify checks the token and resolves its user.
func verify(ctx context.Context, authn Authenticator, token string) (auth.Identity, users.User, error) {
	id, err := authn.Verify(ctx, token)
	if err != nil {
		return auth.Identity{}, users.User{}, err
	}
	user, err := authn.Resolve(ctx, id)
	if err != nil {
		return auth.Identity{}, users.User{}, err
	}
	return id, user, nil
}

// sendFrame queues a frame for the writer, dropping it when the client is
// already gone.
func (c *Client) sendFrame(frame Frame) {
	raw, err := json.Marshal(frame)
	if err != nil {
		return
	}
	select {
	case c.send <- raw:
	case <-c.done:
	default:
	}
}

// writePump is the socket's only writer: queued frames, heartbeats and the
// close handshake all serialize here (gorilla forbids concurrent writers).
func (c *Client) writePump() {
	h := c.hub
	ticker := time.NewTicker(h.opts.PingInterval)
	defer ticker.Stop()
	for {
		select {
		case raw := <-c.send:
			if err := c.write(websocket.TextMessage, raw); err != nil {
				h.remove(c)
				return
			}
		case <-ticker.C:
			if err := c.write(websocket.PingMessage, nil); err != nil {
				h.remove(c)
				return
			}
		case req := <-c.closeWith:
			_ = c.conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(req.code, req.reason),
				h.now().Add(h.opts.WriteTimeout))
			h.remove(c)
			return
		case <-c.done:
			return
		}
	}
}

// write sends one frame with a deadline.
func (c *Client) write(messageType int, raw []byte) error {
	h := c.hub
	if err := c.conn.SetWriteDeadline(h.now().Add(h.opts.WriteTimeout)); err != nil {
		return err
	}
	return c.conn.WriteMessage(messageType, raw)
}

// now is the hub clock, for tests.
func (h *Hub) now() time.Time {
	if h.opts.Now == nil {
		return time.Now()
	}
	return h.opts.Now()
}
