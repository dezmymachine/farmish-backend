package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/database"
	"github.com/dezmymachine/farmish-backend/internal/db"
	"github.com/dezmymachine/farmish-backend/internal/jobs"
	"github.com/dezmymachine/farmish-backend/internal/notify"
	"github.com/dezmymachine/farmish-backend/internal/sellers"
	"github.com/dezmymachine/farmish-backend/internal/users"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

// UserStore reads display names for counterpart labels.
type UserStore interface {
	Get(ctx context.Context, id uuid.UUID) (users.User, error)
}

// SellerStore reads business names for counterpart labels.
type SellerStore interface {
	GetPublic(ctx context.Context, userID uuid.UUID) (sellers.PublicProfile, error)
}

// CoverStore turns a media key into a public image URL. *media.Service
// satisfies it, nil-safely.
type CoverStore interface {
	PublicURL(key string) string
}

// Service owns conversations, messages, read markers and the nudge fan-out.
type Service struct {
	pool    *pgxpool.Pool
	users   UserStore
	sellers SellerStore
	media   CoverStore
	jobs    *jobs.Client
	log     *slog.Logger
	// Now is the clock, injectable so timer tests never sleep.
	Now func() time.Time
}

// New returns the service. Media may be nil (no cover URLs then).
func New(pool *pgxpool.Pool, users UserStore, sellers SellerStore, media CoverStore) *Service {
	return &Service{pool: pool, users: users, sellers: sellers, media: media, Now: time.Now}
}

// AttachJobClient gives the service the client it needs to enqueue SMS
// nudges inside the write transaction. cmd/api calls it once, after the
// registry exists.
func (s *Service) AttachJobClient(client *jobs.Client) { s.jobs = client }

// AttachLogger gives the service its logger.
func (s *Service) AttachLogger(l *slog.Logger) { s.log = l }

// StartConversation opens (or reuses) the buyer's conversation for a listing
// and appends the first message. created reports whether the row is new
// (201) or existing (200).
func (s *Service) StartConversation(ctx context.Context, buyerID, listingID uuid.UUID, message string) (Conversation, Message, bool, error) {
	if invalid := checkBody(message); invalid != nil {
		return Conversation{}, Message{}, false, invalid
	}
	listing, err := db.New(s.pool).GetListingForMessaging(ctx, db.GetListingForMessagingParams{
		ID: listingID, Now: s.Now(),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Conversation{}, Message{}, false, fmt.Errorf("%w: %s", ErrListingInactive, listingID)
	}
	if err != nil {
		return Conversation{}, Message{}, false, fmt.Errorf("get listing: %w", err)
	}
	if !listing.Active {
		return Conversation{}, Message{}, false, fmt.Errorf("%w: %s", ErrListingInactive, listingID)
	}
	if listing.SellerID == buyerID {
		return Conversation{}, Message{}, false, ErrOwnListing
	}
	var convo Conversation
	var sent Message
	created := false
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		row, err := q.InsertConversation(ctx, db.InsertConversationParams{
			ListingID: listingID, BuyerID: buyerID, SellerID: listing.SellerID,
		})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("insert conversation: %w", err)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			// The unique index already held this conversation: append.
			row, err = q.GetConversationByListingBuyer(ctx, db.GetConversationByListingBuyerParams{
				ListingID: listingID, BuyerID: buyerID,
			})
			if err != nil {
				return fmt.Errorf("get existing conversation: %w", err)
			}
		} else {
			created = true
		}
		convo = fromConversationRow(row)
		m, err := s.appendMessage(ctx, tx, convo, buyerID, message, listing.Title)
		if err != nil {
			return err
		}
		sent = m
		return nil
	})
	if err != nil {
		return Conversation{}, Message{}, false, err
	}
	return convo, sent, created, nil
}

// SendMessage appends one message to a conversation the sender participates
// in.
func (s *Service) SendMessage(ctx context.Context, senderID, conversationID uuid.UUID, body string) (Message, error) {
	if invalid := checkBody(body); invalid != nil {
		return Message{}, invalid
	}
	var sent Message
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		row, err := q.GetConversationForUpdate(ctx, conversationID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrNotFound, conversationID)
		}
		if err != nil {
			return fmt.Errorf("lock conversation: %w", err)
		}
		convo := fromConversationRow(row)
		if _, ok := participantRole(convo, senderID); !ok {
			return ErrForbidden
		}
		listing, err := q.GetListingForMessaging(ctx, db.GetListingForMessagingParams{
			ID: convo.ListingID, Now: s.Now(),
		})
		if err != nil {
			return fmt.Errorf("get listing: %w", err)
		}
		m, err := s.appendMessage(ctx, tx, convo, senderID, body, listing.Title)
		if err != nil {
			return err
		}
		sent = m
		return nil
	})
	if err != nil {
		return Message{}, err
	}
	return sent, nil
}

// appendMessage inserts the message, bumps last_message_at, nudges the
// recipient by SMS when they have unread, and publishes the realtime event,
// all inside the caller's transaction.
func (s *Service) appendMessage(ctx context.Context, tx pgx.Tx, convo Conversation, senderID uuid.UUID, body, listingTitle string) (Message, error) {
	if s.jobs == nil {
		return Message{}, fmt.Errorf("messaging: job client is not wired")
	}
	q := db.New(tx)
	previous, err := q.GetLastMessage(ctx, convo.ID)
	previousExists := true
	if errors.Is(err, pgx.ErrNoRows) {
		previousExists = false
	} else if err != nil {
		return Message{}, fmt.Errorf("get last message: %w", err)
	}
	row, err := q.InsertMessage(ctx, db.InsertMessageParams{
		ConversationID: convo.ID, SenderID: senderID, Body: body,
	})
	if err != nil {
		return Message{}, fmt.Errorf("insert message: %w", err)
	}
	sent := fromMessageRow(row)
	if err := q.SetConversationLastMessage(ctx, db.SetConversationLastMessageParams{
		ID: convo.ID, LastMessageAt: &sent.CreatedAt,
	}); err != nil {
		return Message{}, fmt.Errorf("bump last message: %w", err)
	}
	recipient := convo.BuyerID
	if senderID == convo.BuyerID {
		recipient = convo.SellerID
	}
	marker := readMarker(convo, recipient)
	if marker == nil || (previousExists && !marker.After(previous.CreatedAt)) {
		if _, err := s.jobs.InsertTx(ctx, tx, notify.SMSArgs{
			UserID: recipient, Template: notify.TemplateMessageReceived,
			Params: map[string]string{"title": listingTitle},
		}, jobs.UniqueWithin(time.Hour)); err != nil {
			return Message{}, fmt.Errorf("enqueue nudge: %w", err)
		}
	}
	if err := publishEvent(ctx, q, eventPayload{
		Kind: "message.created", ConversationID: convo.ID, MessageID: &sent.ID,
		ParticipantIDs: []uuid.UUID{convo.BuyerID, convo.SellerID},
	}); err != nil {
		return Message{}, err
	}
	return sent, nil
}

// GetMessages returns one page of history, newest first. before limits to
// messages strictly older than the cursor; the returned cursor pages further
// when a full page came back.
func (s *Service) GetMessages(ctx context.Context, callerID, conversationID uuid.UUID, before string, limit int32) ([]Message, *string, error) {
	row, err := db.New(s.pool).GetConversationByID(ctx, conversationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, fmt.Errorf("%w: %s", ErrNotFound, conversationID)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("get conversation: %w", err)
	}
	if _, ok := participantRole(fromConversationRow(row), callerID); !ok {
		return nil, nil, ErrForbidden
	}
	var beforeAt *time.Time
	var beforeID *uuid.UUID
	if before != "" {
		at, id, err := DecodeCursor(before)
		if err != nil {
			return nil, nil, err
		}
		beforeAt, beforeID = &at, &id
	}
	rows, err := db.New(s.pool).ListMessages(ctx, db.ListMessagesParams{
		ConversationID: conversationID, BeforeAt: beforeAt, BeforeID: beforeIDUUID(beforeID), Limit: limit + 1,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("list messages: %w", err)
	}
	out := make([]Message, 0, len(rows))
	for _, r := range rows {
		if len(out) >= int(limit) {
			break
		}
		out = append(out, fromMessageRow(r))
	}
	var next *string
	if len(rows) > int(limit) && len(out) > 0 {
		last := out[len(out)-1]
		cursor := EncodeCursor(last.CreatedAt, last.ID)
		next = &cursor
	}
	return out, next, nil
}

// MarkRead sets the caller's read marker to now and publishes the read
// event, in one transaction.
func (s *Service) MarkRead(ctx context.Context, callerID, conversationID uuid.UUID) error {
	if s.jobs == nil {
		return fmt.Errorf("messaging: job client is not wired")
	}
	return database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		row, err := q.GetConversationForUpdate(ctx, conversationID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrNotFound, conversationID)
		}
		if err != nil {
			return fmt.Errorf("lock conversation: %w", err)
		}
		convo := fromConversationRow(row)
		isBuyer, ok := participantRole(convo, callerID)
		if !ok {
			return ErrForbidden
		}
		now := s.Now()
		if isBuyer {
			if err := q.SetConversationRead(ctx, db.SetConversationReadParams{ID: convo.ID, BuyerLastReadAt: &now}); err != nil {
				return fmt.Errorf("mark read: %w", err)
			}
		} else {
			if err := q.SetConversationReadSeller(ctx, db.SetConversationReadSellerParams{ID: convo.ID, SellerLastReadAt: &now}); err != nil {
				return fmt.Errorf("mark read: %w", err)
			}
		}
		return publishEvent(ctx, q, eventPayload{
			Kind: "conversation.read", ConversationID: convo.ID, ReaderID: &callerID, ReadAt: &now,
			ParticipantIDs: []uuid.UUID{convo.BuyerID, convo.SellerID},
		})
	})
}

// ListConversations returns the caller's conversation summaries, newest
// activity first.
func (s *Service) ListConversations(ctx context.Context, callerID uuid.UUID, limit, offset int32) ([]Summary, int64, error) {
	q := db.New(s.pool)
	rows, err := q.ListConversationsByParticipant(ctx, db.ListConversationsByParticipantParams{
		BuyerID: callerID, Limit: limit, Offset: offset,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("list conversations: %w", err)
	}
	total, err := q.CountConversationsByParticipant(ctx, callerID)
	if err != nil {
		return nil, 0, fmt.Errorf("count conversations: %w", err)
	}
	out := make([]Summary, 0, len(rows))
	for _, row := range rows {
		summary, err := s.summarize(ctx, fromConversationRow(row), callerID)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, summary)
	}
	return out, total, nil
}

// summarize builds one safe-projected summary: listing ref with cover,
// counterpart name only, truncated last message and the unread count.
func (s *Service) summarize(ctx context.Context, convo Conversation, callerID uuid.UUID) (Summary, error) {
	q := db.New(s.pool)
	listing, err := q.GetListingForMessaging(ctx, db.GetListingForMessagingParams{
		ID: convo.ListingID, Now: s.Now(),
	})
	if err != nil {
		return Summary{}, fmt.Errorf("get listing: %w", err)
	}
	summary := Summary{
		ConversationID: convo.ID,
		Listing: ListingRef{
			ID: convo.ListingID, Slug: listing.Slug, Title: listing.Title,
		},
	}
	coverKey, _ := listing.CoverKey.(string)
	if coverKey != "" && s.media != nil {
		if url := s.media.PublicURL(coverKey); url != "" {
			summary.Listing.CoverImageURL = &url
		}
	}
	if callerID == convo.BuyerID {
		profile, err := s.sellers.GetPublic(ctx, convo.SellerID)
		if err != nil {
			return Summary{}, fmt.Errorf("get seller profile: %w", err)
		}
		summary.Counterpart = Counterpart{UserID: convo.SellerID, Name: profile.BusinessName}
	} else {
		name := "Buyer"
		if user, err := s.users.Get(ctx, convo.BuyerID); err == nil && user.DisplayName != nil && *user.DisplayName != "" {
			name = *user.DisplayName
		} else if err != nil {
			return Summary{}, fmt.Errorf("get buyer: %w", err)
		}
		summary.Counterpart = Counterpart{UserID: convo.BuyerID, Name: name}
	}
	last, err := q.GetLastMessage(ctx, convo.ID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		last = db.Message{}
	case err != nil:
		return Summary{}, fmt.Errorf("get last message: %w", err)
	default:
		summary.LastMessage = &LastMessage{
			Body: Truncate(last.Body), CreatedAt: last.CreatedAt, FromMe: last.SenderID == callerID,
		}
	}
	unread, err := q.CountUnread(ctx, db.CountUnreadParams{
		ConversationID: convo.ID, SenderID: callerID, ReadAt: readMarker(convo, callerID),
	})
	if err != nil {
		return Summary{}, fmt.Errorf("count unread: %w", err)
	}
	summary.UnreadCount = unread
	return summary, nil
}

// checkBody validates a message body (DOMAIN §11: 1–2000 chars).
func checkBody(body string) error {
	if len(body) < MinBodyLen || len(body) > MaxBodyLen {
		var invalid validation.Error
		invalid.Add("body", "must be between 1 and 2000 characters")
		return invalid.OrNil()
	}
	return nil
}

// beforeIDUUID widens an optional cursor id for the generated query.
func beforeIDUUID(id *uuid.UUID) pgtype.UUID {
	if id == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: *id, Valid: true}
}

// eventPayload is the ids-only realtime payload published with pg_notify.
// Bodies never travel here: the listener loads the row, and the 8000-byte
// NOTIFY limit stays far away even so.
type eventPayload struct {
	Kind           string      `json:"kind"`
	ConversationID uuid.UUID   `json:"conversationId"`
	MessageID      *uuid.UUID  `json:"messageId,omitempty"`
	ParticipantIDs []uuid.UUID `json:"participantIds"`
	ReaderID       *uuid.UUID  `json:"readerId,omitempty"`
	ReadAt         *time.Time  `json:"readAt,omitempty"`
}

// publishEvent notifies the realtime listener inside the caller's
// transaction: it fires at commit, so a rolled-back write emits nothing.
func publishEvent(ctx context.Context, q *db.Queries, payload eventPayload) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal realtime event: %w", err)
	}
	if _, err := q.NotifyMessagingEvent(ctx, string(raw)); err != nil {
		return fmt.Errorf("notify realtime event: %w", err)
	}
	return nil
}

// fromConversationRow maps the generated row onto the domain type.
func fromConversationRow(r db.Conversation) Conversation {
	return Conversation{
		ID: r.ID, ListingID: r.ListingID, BuyerID: r.BuyerID, SellerID: r.SellerID,
		OrderID: uuidOrNil(r.OrderID), LastMessageAt: r.LastMessageAt,
		BuyerLastRead: r.BuyerLastReadAt, SellerLastRead: r.SellerLastReadAt,
		CreatedAt: r.CreatedAt,
	}
}

// fromMessageRow maps the generated row onto the domain type.
func fromMessageRow(r db.Message) Message {
	return Message{
		ID: r.ID, ConversationID: r.ConversationID, SenderID: r.SenderID,
		Body: r.Body, CreatedAt: r.CreatedAt,
	}
}

// uuidOrNil widens a nullable id for the domain type.
func uuidOrNil(id pgtype.UUID) *uuid.UUID {
	if !id.Valid {
		return nil
	}
	value := uuid.UUID(id.Bytes)
	return &value
}
