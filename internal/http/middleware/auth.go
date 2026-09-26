package middleware

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	legacyrouter "github.com/getkin/kin-openapi/routers/legacy"
	"github.com/gin-gonic/gin"

	"github.com/dezmymachine/farmish-backend/internal/auth"
	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/internal/users"
	"github.com/dezmymachine/farmish-backend/pkg/logger"
)

const (
	// bearerScheme is the security scheme name in api/openapi.yaml.
	bearerScheme = "bearerAuth"
	// roleExtension on an operation names the role it requires.
	roleExtension = "x-farmish-role"
)

// UserResolver maps a verified identity to a users row (users.Service).
type UserResolver interface {
	Resolve(ctx context.Context, id auth.Identity) (users.User, error)
}

// Authenticate enforces what api/openapi.yaml declares for each operation:
//   - security includes bearerAuth: RequireAuth (verified token + users row)
//   - x-farmish-role: admin: additionally RequireAdmin
//   - security: [] (public): nothing
//
// Paths the spec doesn't define pass through to Gin's 404/405.
func Authenticate(spec *openapi3.T, v auth.Verifier, r UserResolver) (gin.HandlerFunc, error) {
	if err := checkRoleExtensions(spec); err != nil {
		return nil, err
	}
	spec.Servers = nil // match on path only, as in OpenAPIValidator
	router, err := legacyrouter.NewRouter(spec)
	if err != nil {
		return nil, fmt.Errorf("openapi router: %w", err)
	}
	requireAuth := RequireAuth(v, r)
	requireAdmin := RequireAdmin()

	return func(c *gin.Context) {
		route, _, err := router.FindRoute(c.Request)
		if err != nil || !needsBearer(spec, route.Operation) {
			c.Next()
			return
		}
		if requireAuth(c); c.IsAborted() {
			return
		}
		if roleOf(route.Operation) == users.RoleAdmin {
			if requireAdmin(c); c.IsAborted() {
				return
			}
		}
		c.Next()
	}, nil
}

// RequireAuth verifies the Bearer ID token, resolves (or creates) the users
// row and stores it in the request context. Every failure gets the same
// generic 401; the cause is only logged.
func RequireAuth(v auth.Verifier, r UserResolver) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		log := logger.FromContext(ctx)

		token, ok := bearerToken(c.GetHeader("Authorization"))
		if !ok {
			abortUnauthorized(c, log, "missing or malformed Authorization header")
			return
		}
		id, err := v.Verify(ctx, token)
		if err != nil {
			abortUnauthorized(c, log, err.Error())
			return
		}
		u, err := r.Resolve(ctx, id)
		if errors.Is(err, users.ErrUnsupportedProvider) {
			abortUnauthorized(c, log, err.Error())
			return
		}
		if err != nil {
			log.Error("resolve user failed", slog.String("error", err.Error()))
			apierror.Abort(c, http.StatusInternalServerError, apierror.CodeInternal, "Internal server error")
			return
		}

		ctx = users.WithContext(ctx, u)
		ctx = logger.WithContext(ctx, log.With(slog.String("user_id", u.ID.String())))
		c.Request = c.Request.WithContext(ctx)
	}
}

// RequireAdmin allows only users whose users.role is admin. It must run after
// RequireAuth.
func RequireAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		u, ok := users.FromContext(c.Request.Context())
		if !ok {
			abortUnauthorized(c, logger.FromContext(c.Request.Context()), "RequireAdmin without an authenticated user")
			return
		}
		if !u.IsAdmin() {
			apierror.Abort(c, http.StatusForbidden, apierror.CodeForbidden, "You do not have access to this resource")
		}
	}
}

func abortUnauthorized(c *gin.Context, log *slog.Logger, reason string) {
	log.Info("authentication failed", slog.String("reason", reason))
	c.Header("WWW-Authenticate", "Bearer")
	apierror.Abort(c, http.StatusUnauthorized, apierror.CodeUnauthorized, "Authentication required")
}

// bearerToken extracts the token from "Bearer <token>" (scheme is
// case-insensitive per RFC 7235).
func bearerToken(header string) (string, bool) {
	scheme, token, ok := strings.Cut(header, " ")
	token = strings.TrimSpace(token)
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", false
	}
	return token, true
}

// needsBearer reports whether op requires bearerAuth. An operation without
// its own security block inherits the document-level one; `security: []`
// makes it public.
func needsBearer(spec *openapi3.T, op *openapi3.Operation) bool {
	reqs := spec.Security
	if op.Security != nil {
		reqs = *op.Security
	}
	for _, req := range reqs {
		if _, ok := req[bearerScheme]; ok {
			return true
		}
	}
	return false
}

func roleOf(op *openapi3.Operation) string {
	role, _ := op.Extensions[roleExtension].(string)
	return role
}

// checkRoleExtensions fails fast on a typo'd role, which would otherwise
// silently leave an admin route open to every signed-in user.
func checkRoleExtensions(spec *openapi3.T) error {
	for path, item := range spec.Paths.Map() {
		for method, op := range item.Operations() {
			raw, present := op.Extensions[roleExtension]
			if !present {
				continue
			}
			role, _ := raw.(string)
			if role != users.RoleAdmin {
				return fmt.Errorf("%s %s: %s must be %q, got %v", method, path, roleExtension, users.RoleAdmin, raw)
			}
			if !needsBearer(spec, op) {
				return fmt.Errorf("%s %s: %s requires bearerAuth security", method, path, roleExtension)
			}
		}
	}
	return nil
}
