package redact

import (
	"context"
	"log/slog"
	"strings"
)

// sensitiveKeys are attribute keys whose values are replaced outright,
// matched case-insensitively against the last path segment (so "req.token"
// and "Token" both match). Account and ID numbers, credentials and emails
// have no safe partial form in a log line.
var sensitiveKeys = map[string]struct{}{
	"phone":                 {},
	"phone_e164":            {},
	"account_number":        {},
	"token":                 {},
	"id_token":              {},
	"authorization":         {},
	"id_number":             {},
	"email":                 {},
	"secret":                {},
	"password":              {},
	"signature":             {},
	"x-paystack-signature":  {},
	"paystack_secret_key":   {},
	"data_encryption_key":   {},
	"firebase_credentials":  {},
	"recipient_code":        {},
	"authorization_code":    {},
	"account_number_cipher": {},
}

// Handler wraps next so no attribute reaches it unredacted. Attributes are
// redacted by key (sensitiveKeys) and every string value is scanned for JWTs,
// bearer credentials and Ghana phone numbers. Group attributes are walked.
//
// Errors are redacted too: an error attribute is rendered to its message
// first, because a wrapped error can carry a URL or body fragment.
type Handler struct {
	next slog.Handler
}

// NewHandler returns a redacting wrapper around next.
func NewHandler(next slog.Handler) *Handler { return &Handler{next: next} }

var _ slog.Handler = (*Handler)(nil)

// Enabled implements slog.Handler.
func (h *Handler) Enabled(ctx context.Context, l slog.Level) bool { return h.next.Enabled(ctx, l) }

// Handle implements slog.Handler. It rebuilds the record so the original,
// which callers may reuse, is never mutated.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, String(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(redactAttr(a))
		return true
	})
	return h.next.Handle(ctx, out)
}

// WithAttrs implements slog.Handler.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	red := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		red[i] = redactAttr(a)
	}
	return &Handler{next: h.next.WithAttrs(red)}
}

// WithGroup implements slog.Handler.
func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{next: h.next.WithGroup(name)}
}

func redactAttr(a slog.Attr) slog.Attr {
	a.Value = a.Value.Resolve()
	if isSensitiveKey(a.Key) && a.Value.Kind() != slog.KindGroup {
		if a.Value.Kind() == slog.KindString && a.Value.String() == "" {
			return a
		}
		return slog.String(a.Key, Placeholder)
	}
	switch a.Value.Kind() {
	case slog.KindString:
		return slog.String(a.Key, String(a.Value.String()))
	case slog.KindGroup:
		group := a.Value.Group()
		red := make([]slog.Attr, len(group))
		for i, g := range group {
			red[i] = redactAttr(g)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(red...)}
	case slog.KindAny:
		if err, ok := a.Value.Any().(error); ok {
			return slog.String(a.Key, String(err.Error()))
		}
		if s, ok := a.Value.Any().(interface{ String() string }); ok {
			return slog.String(a.Key, String(s.String()))
		}
	default:
	}
	return a
}

func isSensitiveKey(key string) bool {
	k := strings.ToLower(key)
	if i := strings.LastIndexByte(k, '.'); i >= 0 {
		k = k[i+1:]
	}
	_, ok := sensitiveKeys[k]
	return ok
}
