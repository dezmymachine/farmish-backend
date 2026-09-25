// Package apierror writes the single JSON error envelope used by every
// endpoint: {"error": {"code", "message", "details?"}}.
package apierror

import "github.com/gin-gonic/gin"

// Envelope is the top-level error response body.
type Envelope struct {
	Error Body `json:"error"`
}

// Body describes a single error.
type Body struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

// Stable error codes shared across handlers.
const (
	CodeNotFound         = "not_found"
	CodeMethodNotAllowed = "method_not_allowed"
	CodeInternal         = "internal_error"
	CodeUnavailable      = "unavailable"
)

// Abort writes the envelope with status and stops the handler chain.
func Abort(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, Envelope{Error: Body{Code: code, Message: message}})
}
