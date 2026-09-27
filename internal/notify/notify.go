// Package notify sends transactional SMS. Messages are short, carry no URLs
// beyond the site domain, and never contain money-adjacent personal data
// beyond what the recipient already knows: their own order.
package notify

import (
	"context"
	"errors"
)

// SMS sends one message to one E.164 recipient. Implementations must never
// log the recipient's number in full.
type SMS interface {
	Send(ctx context.Context, toE164, message string) error
}

// MaxMessageLen is mNotify's single-SMS ceiling. A message is truncated rather
// than split, because the second half of a split SMS can arrive out of order.
const MaxMessageLen = 160

// ErrUnknownTemplate means the job asked for a template that does not exist:
// a programming error, never a user error.
var ErrUnknownTemplate = errors.New("notify: unknown template")
