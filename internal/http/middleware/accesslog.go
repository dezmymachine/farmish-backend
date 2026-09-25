package middleware

import (
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/dezmymachine/farmish-backend/pkg/logger"
)

// AccessLog logs one line per request after it completes. It logs the matched
// route and path only, never the query string or headers, so tokens and phone
// numbers can't leak into logs.
func AccessLog() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		status := c.Writer.Status()
		level := slog.LevelInfo
		switch {
		case status >= 500:
			level = slog.LevelError
		case status >= 400:
			level = slog.LevelWarn
		}

		logger.FromContext(c.Request.Context()).LogAttrs(c.Request.Context(), level, "http_request",
			slog.String("method", c.Request.Method),
			slog.String("route", c.FullPath()),
			slog.String("path", c.Request.URL.Path),
			slog.Int("status", status),
			slog.Int("bytes", c.Writer.Size()),
			slog.Duration("duration", time.Since(start)),
			slog.String("client_ip", c.ClientIP()),
			slog.String("user_agent", c.Request.UserAgent()),
		)
	}
}
