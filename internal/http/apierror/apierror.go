// Package apierror builds and writes the single JSON error envelope used by
// every endpoint: {"error": {"code", "message", "details?"}}. The types are
// generated from api/openapi.yaml, so the envelope can't drift from the spec.
package apierror

import (
	"github.com/gin-gonic/gin"

	"github.com/dezmymachine/farmish-backend/internal/http/api"
)

// Stable error codes. Clients may switch on these; never rename one.
const (
	CodeBadRequest            = "bad_request"
	CodeValidationFailed      = "validation_failed"
	CodeInvalidTransition     = "invalid_transition"
	CodeSellerProfileRequired = "seller_profile_required"
	CodeListingSuspended      = "listing_suspended"
	CodeConflict              = "conflict"
	CodeUnauthorized          = "unauthorized"
	CodeReauthRequired        = "reauth_required"
	CodeForbidden             = "forbidden"
	CodeNotFound              = "not_found"
	CodeMethodNotAllowed      = "method_not_allowed"
	CodeRateLimited           = "rate_limited"
	CodeTurnstileFailed       = "turnstile_failed"
	CodeInternal              = "internal_error"
	CodeUnavailable           = "unavailable"
	// CodePaymentProvider is a 502: the payment provider could not be reached
	// or refused. No money moved, and the buyer can retry.
	CodePaymentProvider = "payment_provider_error"
)

// New returns an envelope with code and message. Strict handlers use it to
// build typed error responses, e.g. api.GetReadyz503JSONResponse{...}.
func New(code, message string) api.Error {
	return api.Error{Error: api.ErrorBody{Code: code, Message: message}}
}

// WithDetails returns an envelope carrying per-field details.
func WithDetails(code, message string, details []api.ErrorDetail) api.Error {
	e := New(code, message)
	if len(details) > 0 {
		e.Error.Details = &details
	}
	return e
}

// Abort writes the envelope with status and stops the handler chain.
func Abort(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, New(code, message))
}

// AbortWithDetails writes an envelope with per-field details and stops the
// handler chain.
func AbortWithDetails(c *gin.Context, status int, code, message string, details []api.ErrorDetail) {
	c.AbortWithStatusJSON(status, WithDetails(code, message, details))
}
