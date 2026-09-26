// Package httpapi builds the Gin router and runs the HTTP server.
package httpapi

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/dezmymachine/farmish-backend/internal/auth"
	"github.com/dezmymachine/farmish-backend/internal/config"
	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/internal/http/handlers"
	"github.com/dezmymachine/farmish-backend/internal/http/middleware"
	"github.com/dezmymachine/farmish-backend/internal/listings"
	"github.com/dezmymachine/farmish-backend/internal/ratelimit"
	"github.com/dezmymachine/farmish-backend/internal/turnstile"
	"github.com/dezmymachine/farmish-backend/pkg/logger"
)

// Deps are the dependencies handlers need.
type Deps struct {
	DB       handlers.Pinger
	Verifier auth.Verifier
	Users    UserService
	Sellers  handlers.SellerStore
	Catalog  handlers.CatalogStore
	Media    handlers.MediaStore
	Listings handlers.ListingStore
	// PublicListings is the public browse surface of the same service.
	PublicListings handlers.PublicListingStore
	Turnstile      turnstile.Verifier
	// Views enqueues listing view counts; nil turns counting off.
	Views listings.ViewCounter
	// ViewerHash turns a caller's address into a one-way id for view
	// counting; nil turns counting off.
	ViewerHash func(ctx context.Context, ip string) uuid.UUID
	// IPLimiter backs the per-IP flood limit. It stays in-process on purpose:
	// free, instant, and a flood can't burn the metered Redis quota.
	// Defaults to ratelimit.Memory.
	IPLimiter ratelimit.Limiter
	// SharedLimiter backs per-user and per-operation limits: Upstash Redis
	// (with in-process fallback) when REDIS_URL is set. Defaults to
	// ratelimit.Memory.
	SharedLimiter ratelimit.Limiter
	// RateLimits defaults to middleware.DefaultRateLimits().
	RateLimits *middleware.RateLimits
}

// UserService resolves authenticated users and serves the /v1/me handlers
// (users.Service).
type UserService interface {
	middleware.UserResolver
	handlers.UserStore
}

// NewRouter returns the Gin engine with middleware and every operation in
// api/openapi.yaml registered through the generated strict server.
func NewRouter(cfg config.Config, log *slog.Logger, deps Deps) (*gin.Engine, error) {
	spec, err := api.GetSpec()
	if err != nil {
		return nil, fmt.Errorf("load openapi spec: %w", err)
	}
	validate, err := middleware.OpenAPIValidator(spec)
	if err != nil {
		return nil, err
	}
	// Separate spec copy: each middleware adjusts its own for routing.
	authSpec, err := api.GetSpec()
	if err != nil {
		return nil, fmt.Errorf("load openapi spec: %w", err)
	}
	authenticate, err := middleware.Authenticate(authSpec, deps.Verifier, deps.Users)
	if err != nil {
		return nil, err
	}

	limits := middleware.DefaultRateLimits()
	if deps.RateLimits != nil {
		limits = *deps.RateLimits
	}
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	ipLimiter, sharedLimiter := deps.IPLimiter, deps.SharedLimiter
	if ipLimiter == nil {
		ipLimiter = ratelimit.NewMemory()
	}
	if sharedLimiter == nil {
		sharedLimiter = ratelimit.NewMemory()
	}
	limitSpec, err := api.GetSpec()
	if err != nil {
		return nil, fmt.Errorf("load openapi spec: %w", err)
	}
	limitOperations, err := middleware.RateLimitOperations(limitSpec, sharedLimiter, limits)
	if err != nil {
		return nil, err
	}
	turnstileSpec, err := api.GetSpec()
	if err != nil {
		return nil, fmt.Errorf("load openapi spec: %w", err)
	}
	requireTurnstile, err := middleware.TurnstileFromSpec(turnstileSpec, deps.Turnstile)
	if err != nil {
		return nil, err
	}

	r := newEngine(log, clientIPResolver(cfg))
	// Order matters:
	//   - CORS answers preflights first, and its headers reach 429s too.
	//   - The per-IP limit runs before any expensive work (probes exempt).
	//   - Auth resolves the user for per-user limits.
	//   - Turnstile runs after limits, so floods don't reach Cloudflare.
	//   - Validation runs last, so anonymous callers learn nothing about request shapes.
	r.Use(
		middleware.CORS(cfg.CORSOrigins),
		middleware.RateLimitIP(ipLimiter, limits.IP, "/healthz", "/readyz"),
		authenticate,
		limitOperations,
		requireTurnstile,
		validate,
	)

	server := handlers.Server{
		DB: deps.DB, Users: deps.Users, Sellers: deps.Sellers, Catalog: deps.Catalog,
		Media: deps.Media, Listings: deps.Listings, PublicListings: deps.PublicListings,
		Views: deps.Views, ViewerHash: deps.ViewerHash, Log: log,
	}
	api.RegisterHandlersWithOptions(r, strictServer(server), api.GinServerOptions{
		ErrorHandler: func(c *gin.Context, err error, _ int) { requestError(c, err) },
	})
	return r, nil
}

// NewProbeRouter serves only /healthz and /readyz, for RUN_MODE=worker
// processes that need platform health checks but must not expose the API.
func NewProbeRouter(cfg config.Config, log *slog.Logger, db handlers.Pinger) *gin.Engine {
	r := newEngine(log, clientIPResolver(cfg))
	s := strictServer(handlers.Server{DB: db})
	r.GET("/healthz", s.GetHealthz)
	r.GET("/readyz", s.GetReadyz)
	return r
}

// newEngine returns a Gin engine with the middleware and fallbacks every
// router shares.
func newEngine(log *slog.Logger, ips middleware.ClientIPResolver) *gin.Engine {
	// Release mode everywhere: debug mode prints non-JSON banners to stdout.
	gin.SetMode(gin.ReleaseMode)

	r := gin.New()
	// Gin's own ClientIP() is unused: middleware.ClientIP resolves addresses.
	_ = r.SetTrustedProxies(nil)
	r.HandleMethodNotAllowed = true
	// Strict handlers receive *gin.Context as context.Context; fall back to the
	// request context so values set by middleware (request logger) are visible.
	r.ContextWithFallback = true

	// AccessLog sits outside Recovery so recovered panics are logged as 500s.
	r.Use(middleware.RequestID(log), middleware.ClientIP(ips), middleware.AccessLog(), middleware.Recovery())

	r.NoRoute(func(c *gin.Context) {
		apierror.Abort(c, http.StatusNotFound, apierror.CodeNotFound, "Resource not found")
	})
	r.NoMethod(func(c *gin.Context) {
		apierror.Abort(c, http.StatusMethodNotAllowed, apierror.CodeMethodNotAllowed, "Method not allowed")
	})
	return r
}

func clientIPResolver(cfg config.Config) middleware.ClientIPResolver {
	return middleware.ClientIPResolver{
		TrustedProxies:  cfg.ClientIP.TrustedProxies,
		TrustCloudflare: cfg.ClientIP.TrustCloudflare,
	}
}

func strictServer(s handlers.Server) api.ServerInterface {
	return api.NewStrictHandlerWithOptions(s, nil, api.StrictGinServerOptions{
		RequestErrorHandlerFunc:  requestError,
		HandlerErrorFunc:         internalError("handler error"),
		ResponseErrorHandlerFunc: internalError("response error"),
	})
}

// requestError answers requests the generated code couldn't bind (bad JSON,
// bad parameter format). The validator normally catches these first.
func requestError(c *gin.Context, err error) {
	logger.FromContext(c.Request.Context()).Info("request binding failed", slog.String("error", err.Error()))
	apierror.Abort(c, http.StatusBadRequest, apierror.CodeBadRequest, "Malformed request")
}

// internalError logs err and answers with a generic 500, never the error text.
func internalError(what string) func(*gin.Context, error) {
	return func(c *gin.Context, err error) {
		logger.FromContext(c.Request.Context()).Error(what, slog.String("error", err.Error()))
		if c.Writer.Written() {
			c.Abort()
			return
		}
		apierror.Abort(c, http.StatusInternalServerError, apierror.CodeInternal, "Internal server error")
	}
}
