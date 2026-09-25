// Package handlers holds HTTP handlers.
package handlers

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/pkg/logger"
)

// readyTimeout bounds each dependency check so a hung DB can't hang probes.
const readyTimeout = 2 * time.Second

// HealthResponse is the liveness/readiness payload.
type HealthResponse struct {
	Status string `json:"status"`
}

// Pinger is a dependency that can report whether it's reachable
// (*pgxpool.Pool satisfies it).
type Pinger interface {
	Ping(ctx context.Context) error
}

// Health serves the liveness and readiness probes.
type Health struct {
	DB Pinger
}

// Healthz is the liveness probe. It checks only that the process serves HTTP.
func (Health) Healthz(c *gin.Context) {
	c.JSON(http.StatusOK, HealthResponse{Status: "ok"})
}

// Readyz is the readiness probe: 200 when the database answers a ping within
// readyTimeout, otherwise a generic 503 envelope (the cause is only logged).
func (h Health) Readyz(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), readyTimeout)
	defer cancel()
	if err := h.DB.Ping(ctx); err != nil {
		logger.FromContext(c.Request.Context()).Warn("readiness check failed",
			slog.String("dependency", "postgres"), slog.String("error", err.Error()))
		apierror.Abort(c, http.StatusServiceUnavailable, apierror.CodeUnavailable, "Service not ready")
		return
	}
	c.JSON(http.StatusOK, HealthResponse{Status: "ok"})
}
