// Package middleware holds the cross-cutting Gin middleware: request ID,
// access logging, panic recovery and CORS.
package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"regexp"

	"github.com/gin-gonic/gin"

	"github.com/dezmymachine/farmish-backend/pkg/logger"
)

// RequestIDHeader carries the request ID in both directions.
const RequestIDHeader = "X-Request-ID"

const requestIDKey = "request_id"

// Inbound IDs are echoed only if they look safe to log and return.
var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// RequestID assigns every request an ID, reusing a well-formed inbound
// X-Request-ID, and attaches a logger tagged with it to the request context.
func RequestID(base *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(RequestIDHeader)
		if !validRequestID.MatchString(id) {
			id = newRequestID()
		}
		c.Set(requestIDKey, id)
		c.Header(RequestIDHeader, id)

		l := base.With(slog.String("request_id", id))
		c.Request = c.Request.WithContext(logger.WithContext(c.Request.Context(), l))
		c.Next()
	}
}

// GetRequestID returns the ID assigned by RequestID, or "".
func GetRequestID(c *gin.Context) string {
	return c.GetString(requestIDKey)
}

func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error
	return hex.EncodeToString(b[:])
}
