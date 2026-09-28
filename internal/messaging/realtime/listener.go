package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Listener tails the messaging_events channel and routes events to local
// sockets. One runs per api/all process, on its own dedicated connection
// (a pool connection must never LISTEN: the pool would recycle it).
type Listener struct {
	pool    *pgxpool.Pool
	hub     *Hub
	queries *messagingQueries
	log     *slog.Logger
	backoff time.Duration
}

// messagingQueries loads the rows events name. It is a seam for tests.
type messagingQueries struct {
	pool *pgxpool.Pool
}

// NewListener returns a listener over pool.
func NewListener(pool *pgxpool.Pool, hub *Hub, log *slog.Logger) *Listener {
	if log == nil {
		log = slog.Default()
	}
	return &Listener{pool: pool, hub: hub, queries: &messagingQueries{pool: pool}, log: log, backoff: time.Second}
}

// Run blocks until ctx is cancelled, reconnecting with backoff on connection
// loss. After every reconnect it broadcasts resync: events published while
// deaf may be missing, and sockets refetch over REST.
func (l *Listener) Run(ctx context.Context) error {
	for {
		err := l.listen(ctx)
		if ctx.Err() != nil {
			return nil
		}
		l.log.Warn("messaging listener lost its connection; reconnecting",
			slog.String("error", errText(err)), slog.Duration("backoff", l.backoff))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(l.backoff):
		}
		if l.backoff < time.Minute {
			l.backoff *= 2
		}
		l.hub.Publish(nil, Frame{Type: "resync"})
		l.backoff = time.Second
	}
}

// listen runs one LISTEN session.
func (l *Listener) listen(ctx context.Context) error {
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire listener connection: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `LISTEN messaging_events`); err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	for {
		notification, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return fmt.Errorf("wait for notification: %w", err)
		}
		l.route(ctx, notification.Payload)
	}
}

// route loads the named row (only when a participant holds a socket here)
// and publishes its frame.
func (l *Listener) route(ctx context.Context, payload string) {
	var event struct {
		Kind           string      `json:"kind"`
		ConversationID uuid.UUID   `json:"conversationId"`
		MessageID      *uuid.UUID  `json:"messageId,omitempty"`
		ParticipantIDs []uuid.UUID `json:"participantIds"`
		ReaderID       *uuid.UUID  `json:"readerId,omitempty"`
		ReadAt         *time.Time  `json:"readAt,omitempty"`
	}
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		l.log.Error("bad messaging event payload", slog.String("error", err.Error()))
		return
	}
	switch event.Kind {
	case "message.created":
		if event.MessageID == nil {
			return
		}
		if !l.hasSockets(event.ParticipantIDs) {
			return
		}
		msg, err := l.queries.getMessage(ctx, *event.MessageID)
		if errors.Is(err, pgx.ErrNoRows) {
			return
		}
		if err != nil {
			l.log.Error("load realtime message", slog.String("error", err.Error()))
			return
		}
		l.hub.Publish(event.ParticipantIDs, Frame{
			Type: event.Kind, ConversationID: &event.ConversationID,
			Message: &Message{
				ID: msg.ID, ConversationID: msg.ConversationID, SenderID: msg.SenderID,
				Body: msg.Body, CreatedAt: msg.CreatedAt,
			},
		})
	case "conversation.read":
		for _, id := range event.ParticipantIDs {
			byMe := id == uuidOrZero(event.ReaderID)
			frame := Frame{
				Type: event.Kind, ConversationID: &event.ConversationID,
				ReadAt: event.ReadAt, ByMe: &byMe,
			}
			l.hub.Publish([]uuid.UUID{id}, frame)
		}
	default:
		l.log.Warn("unknown messaging event kind", slog.String("kind", event.Kind))
	}
}

// messageRow is the row the listener loads for message.created.
type messageRow struct {
	ID             uuid.UUID
	ConversationID uuid.UUID
	SenderID       uuid.UUID
	Body           string
	CreatedAt      time.Time
}

// getMessage loads one message row.
func (q *messagingQueries) getMessage(ctx context.Context, id uuid.UUID) (messageRow, error) {
	var row messageRow
	err := q.pool.QueryRow(ctx,
		`SELECT id, conversation_id, sender_id, body, created_at FROM messages WHERE id = $1`, id).
		Scan(&row.ID, &row.ConversationID, &row.SenderID, &row.Body, &row.CreatedAt)
	if err != nil {
		return messageRow{}, err
	}
	return row, nil
}

// hasSockets reports whether any participant holds a socket here, so the
// row load above only happens when it can reach someone.
func (l *Listener) hasSockets(ids []uuid.UUID) bool {
	for _, id := range ids {
		if l.hub.Sockets(id) > 0 {
			return true
		}
	}
	return false
}

func uuidOrZero(id *uuid.UUID) uuid.UUID {
	if id == nil {
		return uuid.UUID{}
	}
	return *id
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
