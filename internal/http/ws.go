// Package httpapi wires the realtime WebSocket endpoint.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/dezmymachine/farmish-backend/internal/auth"
	"github.com/dezmymachine/farmish-backend/internal/messaging/realtime"
	"github.com/dezmymachine/farmish-backend/internal/users"
	"github.com/dezmymachine/farmish-backend/pkg/logger"
)

// wsAuth verifies first-frame tokens through the same Verifier + Users path
// as the HTTP auth middleware.
type wsAuth struct {
	verifier auth.Verifier
	users    middlewareUserResolver
}

// middlewareUserResolver is the subset of users.Service the socket needs.
type middlewareUserResolver interface {
	Resolve(ctx context.Context, id auth.Identity) (users.User, error)
}

// Verify checks the token's signature, issuer, audience and expiry.
func (a *wsAuth) Verify(ctx context.Context, token string) (auth.Identity, error) {
	return a.verifier.Verify(ctx, token)
}

// Resolve maps a verified identity to its users row.
func (a *wsAuth) Resolve(ctx context.Context, id auth.Identity) (users.User, error) {
	return a.users.Resolve(ctx, id)
}

// realtimeRoute upgrades GET /v1/ws to a push-only messaging socket. It is
// registered directly on Gin, outside the generated strict server: a 101
// upgrade has no OpenAPI response shape (the deviation is recorded in the
// Phase 19 ADR). The per-IP limit already ran; auth happens in the first
// frame, never in the query string.
func realtimeRoute(hub *realtime.Hub, verifier auth.Verifier, users middlewareUserResolver, origins []string, log *slog.Logger) gin.HandlerFunc {
	upgrader := websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		CheckOrigin:     originCheck(origins),
	}
	return func(c *gin.Context) {
		conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			logger.FromContext(c.Request.Context()).Info("websocket upgrade refused",
				slog.String("error", err.Error()))
			return
		}
		hub.Serve(c.Request.Context(), conn, &wsAuth{verifier: verifier, users: users})
	}
}

// originCheck mirrors CORSOrigins for the handshake. Browsers always send
// Origin, so a missing one cannot be a cross-site hijack attempt and stays
// allowed (mobile apps and QA tools omit it); any other value must match.
func originCheck(origins []string) func(r *http.Request) bool {
	allowed := map[string]bool{}
	for _, origin := range origins {
		allowed[origin] = true
	}
	return func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		return allowed[origin]
	}
}
