// Package httpapi builds the Gin router and runs the HTTP server.
package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/dezmymachine/farmish-backend/internal/config"
	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/internal/http/handlers"
	"github.com/dezmymachine/farmish-backend/internal/http/middleware"
)

// Deps are the dependencies handlers need.
type Deps struct {
	DB handlers.Pinger
}

// NewRouter returns the Gin engine with middleware and routes registered.
func NewRouter(cfg config.Config, log *slog.Logger, deps Deps) *gin.Engine {
	// Release mode everywhere: debug mode prints non-JSON banners to stdout.
	gin.SetMode(gin.ReleaseMode)

	r := gin.New()
	// Don't trust X-Forwarded-For until Phase 6 wires CF-Connecting-IP.
	_ = r.SetTrustedProxies(nil)
	r.HandleMethodNotAllowed = true

	// AccessLog sits outside Recovery so recovered panics are logged as 500s.
	r.Use(
		middleware.RequestID(log),
		middleware.AccessLog(),
		middleware.Recovery(),
		middleware.CORS(cfg.CORSOrigins),
	)

	r.NoRoute(func(c *gin.Context) {
		apierror.Abort(c, http.StatusNotFound, apierror.CodeNotFound, "Resource not found")
	})
	r.NoMethod(func(c *gin.Context) {
		apierror.Abort(c, http.StatusMethodNotAllowed, apierror.CodeMethodNotAllowed, "Method not allowed")
	})

	health := handlers.Health{DB: deps.DB}
	r.GET("/healthz", health.Healthz)
	r.GET("/readyz", health.Readyz)

	return r
}
