package notify

import (
	"context"
	"log/slog"

	"github.com/dezmymachine/farmish-backend/internal/geo"
)

// LogOnly is the dev and test sender: it logs a masked number and the
// template name, and never sends anything. NOTIFY_SMS_ENABLED=false uses it.
type LogOnly struct {
	Log *slog.Logger
}

// Send logs instead of sending.
func (l LogOnly) Send(_ context.Context, toE164, message string) error {
	l.Log.Info("sms (log-only)",
		slog.String("to", geo.MaskPhone(toE164)),
		slog.String("message", message))
	return nil
}
