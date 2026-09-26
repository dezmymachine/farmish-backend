package middleware

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/gin-gonic/gin"

	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/internal/turnstile"
	"github.com/dezmymachine/farmish-backend/pkg/logger"
)

const (
	// TurnstileHeader carries the Turnstile widget's token.
	TurnstileHeader = "X-Turnstile-Token"
	// turnstileExtension marks operations that require a Turnstile token.
	turnstileExtension = "x-farmish-turnstile"
)

var errNoTurnstileToken = errors.New("missing turnstile token")

// TurnstileFromSpec requires a valid Turnstile token on operations marked
// `x-farmish-turnstile: true`. Run it after rate limiting (so floods don't
// reach Cloudflare) and before request validation.
func TurnstileFromSpec(spec *openapi3.T, v turnstile.Verifier) (gin.HandlerFunc, error) {
	if err := forEachOperation(spec, func(method, path string, op *openapi3.Operation) error {
		if raw, present := op.Extensions[turnstileExtension]; present {
			if _, ok := raw.(bool); !ok {
				return fmt.Errorf("%s %s: %s must be a boolean, got %v", method, path, turnstileExtension, raw)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	router, err := newSpecRouter(spec)
	if err != nil {
		return nil, err
	}
	require := RequireTurnstile(v)
	return func(c *gin.Context) {
		route, _, err := router.FindRoute(c.Request)
		if err != nil {
			c.Next()
			return
		}
		if on, _ := route.Operation.Extensions[turnstileExtension].(bool); !on {
			c.Next()
			return
		}
		if require(c); c.IsAborted() {
			return
		}
		c.Next()
	}, nil
}

// RequireTurnstile verifies the X-Turnstile-Token header with Cloudflare.
// A missing or rejected token gets 400 turnstile_failed; if Cloudflare can't
// be reached the request fails closed with 503.
func RequireTurnstile(v turnstile.Verifier) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := logger.FromContext(c.Request.Context())
		token := c.GetHeader(TurnstileHeader)
		err := errNoTurnstileToken
		if token != "" {
			err = v.Verify(c.Request.Context(), token, GetClientIP(c))
		}
		switch {
		case err == nil:
		case errors.Is(err, errNoTurnstileToken), errors.Is(err, turnstile.ErrFailed):
			log.Info("turnstile check failed", slog.String("reason", err.Error()))
			apierror.Abort(c, http.StatusBadRequest, apierror.CodeTurnstileFailed, "Bot check failed; please complete the challenge and retry")
		default:
			log.Error("turnstile verification unavailable", slog.String("error", err.Error()))
			apierror.Abort(c, http.StatusServiceUnavailable, apierror.CodeUnavailable, "Verification is temporarily unavailable; please retry")
		}
	}
}
