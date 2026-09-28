// Package messaging owns buyer↔seller conversations about listings
// (Phase 19): one conversation per (listing, buyer), participant-only
// reads and writes, cursor history, read markers and throttled SMS nudges.
// Realtime push lives in messaging/realtime; this package is the REST
// source of truth both transports share.
package messaging

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Message body bounds (DOMAIN §11, messages.body CHECK).
const (
	MinBodyLen = 1
	MaxBodyLen = 2000
	// SummaryBodyLen truncates the last message in conversation summaries.
	SummaryBodyLen = 120
)

var (
	// ErrNotFound means no conversation matches, or the caller is not a
	// participant: both read as 404/403 without leaking which.
	ErrNotFound = errors.New("conversation not found")
	// ErrForbidden means the caller is authenticated but not a participant.
	ErrForbidden = errors.New("not a participant of this conversation")
	// ErrOwnListing means the seller tried to message themselves.
	ErrOwnListing = errors.New("cannot message your own listing")
	// ErrListingInactive means the listing is not active or is expired.
	ErrListingInactive = errors.New("listing is not active")
	// ErrBadCursor means a history cursor does not decode.
	ErrBadCursor = errors.New("invalid history cursor")
)

// Conversation is one row of the conversations table.
type Conversation struct {
	ID             uuid.UUID
	ListingID      uuid.UUID
	BuyerID        uuid.UUID
	SellerID       uuid.UUID
	OrderID        *uuid.UUID
	LastMessageAt  *time.Time
	BuyerLastRead  *time.Time
	SellerLastRead *time.Time
	CreatedAt      time.Time
}

// Message is one row of the messages table.
type Message struct {
	ID             uuid.UUID
	ConversationID uuid.UUID
	SenderID       uuid.UUID
	Body           string
	CreatedAt      time.Time
}

// ListingRef is the listing half of a conversation summary.
type ListingRef struct {
	ID            uuid.UUID
	Slug          string
	Title         string
	CoverImageURL *string
}

// LastMessage is the truncated tail of a conversation summary.
type LastMessage struct {
	Body      string
	CreatedAt time.Time
	FromMe    bool
}

// Summary is one conversation row for the list endpoint.
type Summary struct {
	ConversationID uuid.UUID
	Listing        ListingRef
	Counterpart    Counterpart
	LastMessage    *LastMessage
	UnreadCount    int64
}

// Counterpart is the other participant, safe-projected: a name only, never
// contact fields.
type Counterpart struct {
	UserID uuid.UUID
	Name   string
}

// participantRole reports which side callerID is on.
func participantRole(c Conversation, callerID uuid.UUID) (isBuyer, ok bool) {
	switch callerID {
	case c.BuyerID:
		return true, true
	case c.SellerID:
		return false, true
	}
	return false, false
}

// readMarker returns the caller's read marker.
func readMarker(c Conversation, callerID uuid.UUID) *time.Time {
	if callerID == c.BuyerID {
		return c.BuyerLastRead
	}
	return c.SellerLastRead
}

// EncodeCursor renders an opaque history cursor over (created_at, id).
func EncodeCursor(createdAt time.Time, id uuid.UUID) string {
	raw := createdAt.UTC().Format(time.RFC3339Nano) + "|" + id.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor parses a cursor from EncodeCursor.
func DecodeCursor(cursor string) (time.Time, uuid.UUID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, uuid.UUID{}, fmt.Errorf("%w: %v", ErrBadCursor, err)
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return time.Time{}, uuid.UUID{}, fmt.Errorf("%w: want created_at|id", ErrBadCursor)
	}
	at, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, uuid.UUID{}, fmt.Errorf("%w: %v", ErrBadCursor, err)
	}
	id, err := uuid.Parse(parts[1])
	if err != nil {
		return time.Time{}, uuid.UUID{}, fmt.Errorf("%w: %v", ErrBadCursor, err)
	}
	return at, id, nil
}

// Truncate shortens a message body for summaries.
func Truncate(body string) string {
	if len(body) <= SummaryBodyLen {
		return body
	}
	return body[:SummaryBodyLen]
}
