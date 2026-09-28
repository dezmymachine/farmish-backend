package middleware

import (
	"github.com/gin-gonic/gin"
)

const hstsValue = "max-age=31536000; includeSubDomains"

// SecurityHeaders sets the response hardening headers on every API response,
// error and preflight included, because they are set before the handler runs:
//
//   - X-Content-Type-Options: nosniff
//   - Referrer-Policy: no-referrer
//   - X-Frame-Options: DENY
//   - Strict-Transport-Security, only when hsts is set (staging and
//     production; a browser that saw it on http://localhost would pin it)
//   - Cache-Control: no-store on requests that carry credentials, so a shared
//     cache never keeps a private response. A handler that sets its own
//     Cache-Control later simply replaces it.
func SecurityHeaders(hsts bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		if hsts {
			h.Set("Strict-Transport-Security", hstsValue)
		}
		if c.GetHeader("Authorization") != "" {
			h.Set("Cache-Control", "no-store")
		}
		c.Next()
	}
}
