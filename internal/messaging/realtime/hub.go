// Package realtime delivers messaging events over a push-only WebSocket
// (Phase 19): the server writes frames, clients never send anything but auth
// frames. REST stays the only write path and the source of truth; sockets
// are hints the client reconciles over REST.
package realtime

import (
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// Close codes the server sends. 4400–4429 are application codes (RFC 6455
// reserves 4000–4999 for applications); 1001/1013 are protocol codes.
const (
	// CloseBadFrame means the client sent anything but an auth frame.
	CloseBadFrame = 4400
	// CloseUnauthorized means no auth frame in time, or a bad token.
	CloseUnauthorized = 4401
	// CloseTokenExpired means the token passed exp without a fresh auth.
	CloseTokenExpired = 4001
	// CloseTooManySockets means the user already holds the cap.
	CloseTooManySockets = 4429
	// CloseGoingAway is the shutdown close every socket receives.
	CloseGoingAway = 1001
	// CloseTryAgain is the slow-consumer drop: reconnect and refetch.
	CloseTryAgain = 1013
)

// Options tunes the hub and its clients. Tests override timeouts and the
// clock through it.
type Options struct {
	// MaxSocketsPerUser caps one user's concurrent sockets.
	MaxSocketsPerUser int
	// SendBuffer is each client's outbound queue.
	SendBuffer int
	// AuthTimeout bounds the wait for the first auth frame.
	AuthTimeout time.Duration
	// PingInterval is the server heartbeat; PongWait bounds the reply.
	PingInterval time.Duration
	PongWait     time.Duration
	// WriteTimeout bounds every frame write.
	WriteTimeout time.Duration
	// ReadLimit caps one incoming frame.
	ReadLimit int64
	// Now is the clock.
	Now func() time.Time
}

// DefaultOptions returns the production tuning.
func DefaultOptions() Options {
	return Options{
		MaxSocketsPerUser: 5,
		SendBuffer:        32,
		AuthTimeout:       5 * time.Second,
		PingInterval:      25 * time.Second,
		PongWait:          60 * time.Second,
		WriteTimeout:      10 * time.Second,
		ReadLimit:         4096,
		Now:               time.Now,
	}
}

// Frame is one server → client event. The shapes mirror the RealtimeEvent
// schemas in api/openapi.yaml.
type Frame struct {
	Type           string     `json:"type"`
	ConversationID *uuid.UUID `json:"conversationId,omitempty"`
	Message        *Message   `json:"message,omitempty"`
	ReadAt         *time.Time `json:"readAt,omitempty"`
	ByMe           *bool      `json:"byMe,omitempty"`
}

// Message is the pushed message projection (the REST shape).
type Message struct {
	ID             uuid.UUID `json:"id"`
	ConversationID uuid.UUID `json:"conversationId"`
	SenderID       uuid.UUID `json:"senderId"`
	Body           string    `json:"body"`
	CreatedAt      time.Time `json:"createdAt"`
}

// closeRequest asks the writer goroutine to send a close frame and stop.
// Only the writer ever writes to the connection.
type closeRequest struct {
	code   int
	reason string
}

// Hub routes frames to connected users. It is safe for concurrent use.
type Hub struct {
	mu      sync.Mutex
	clients map[uuid.UUID]map[*Client]struct{}
	opts    Options
	log     *slog.Logger
	closed  bool
	wg      sync.WaitGroup
}

// NewHub returns a hub. Log may be nil (slog.Default then).
func NewHub(opts Options, log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	return &Hub{clients: map[uuid.UUID]map[*Client]struct{}{}, opts: opts, log: log}
}

// Client is one authenticated socket.
type Client struct {
	conn      *websocket.Conn
	send      chan []byte
	closeWith chan closeRequest
	done      chan struct{}
	once      sync.Once
	userID    uuid.UUID
	hub       *Hub
	// registered reports whether add() counted this client in the
	// shutdown WaitGroup. Guarded by the hub mutex.
	registered bool
}

// add registers a client, enforcing the per-user cap. False means the cap
// is hit (or the hub is closed) and the caller must refuse the socket.
func (h *Hub) add(c *Client) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	set := h.clients[c.userID]
	if len(set) >= h.opts.MaxSocketsPerUser {
		return false
	}
	if set == nil {
		set = map[*Client]struct{}{}
		h.clients[c.userID] = set
	}
	set[c] = struct{}{}
	c.registered = true
	h.wg.Add(1)
	return true
}

// remove unregisters a client and releases it exactly once: its done
// channel wakes the writer, and the closed connection unblocks the reader.
func (h *Hub) remove(c *Client) {
	h.mu.Lock()
	registered := c.registered
	c.registered = false
	if set := h.clients[c.userID]; set != nil {
		delete(set, c)
		if len(set) == 0 {
			delete(h.clients, c.userID)
		}
	}
	h.mu.Unlock()
	c.once.Do(func() {
		close(c.done)
		_ = c.conn.Close()
		if registered {
			h.wg.Done()
		}
	})
}

// Sockets reports how many sockets a user holds, for tests.
func (h *Hub) Sockets(userID uuid.UUID) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients[userID])
}

// Publish sends a frame to every socket of the given users. A client whose
// buffer is full is dropped alone (it reconnects and refetches); collection
// happens under the lock, drops after it.
func (h *Hub) Publish(userIDs []uuid.UUID, frame Frame) {
	raw, err := json.Marshal(frame)
	if err != nil {
		h.log.Error("marshal realtime frame", slog.String("error", err.Error()))
		return
	}
	var slow []*Client
	h.mu.Lock()
	if !h.closed {
		for _, id := range userIDs {
			for c := range h.clients[id] {
				select {
				case c.send <- raw:
				default:
					slow = append(slow, c)
				}
			}
		}
	}
	h.mu.Unlock()
	for _, c := range slow {
		c.requestClose(CloseTryAgain, "slow consumer, reconnect")
		h.remove(c)
	}
}

// Broadcast sends a frame to every connected socket (resync): it is the
// only publish path that is not addressed to named users.
func (h *Hub) Broadcast(frame Frame) {
	raw, err := json.Marshal(frame)
	if err != nil {
		h.log.Error("marshal realtime frame", slog.String("error", err.Error()))
		return
	}
	var slow []*Client
	h.mu.Lock()
	if !h.closed {
		for _, set := range h.clients {
			for c := range set {
				select {
				case c.send <- raw:
				default:
					slow = append(slow, c)
				}
			}
		}
	}
	h.mu.Unlock()
	for _, c := range slow {
		c.requestClose(CloseTryAgain, "slow consumer, reconnect")
		h.remove(c)
	}
}

// requestClose asks the writer to send a close frame, without blocking.
func (c *Client) requestClose(code int, reason string) {
	select {
	case c.closeWith <- closeRequest{code: code, reason: reason}:
	default:
	}
}

// Close shuts the hub down: every socket gets the code, and Close blocks
// until every writer observed it. Shutdown calls it through
// RegisterOnShutdown, whose context timeout bounds the wait.
func (h *Hub) Close(code int, reason string) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	var clients []*Client
	for _, set := range h.clients {
		for c := range set {
			clients = append(clients, c)
		}
	}
	h.mu.Unlock()
	for _, c := range clients {
		c.requestClose(code, reason)
	}
	h.wg.Wait()
}
