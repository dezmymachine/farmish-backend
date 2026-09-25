// Package handlers holds HTTP handlers.
package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// HealthResponse is the liveness payload.
type HealthResponse struct {
	Status string `json:"status"`
}

// Healthz is the liveness probe. It checks only that the process serves HTTP;
// dependency checks belong in /readyz (Phase 2).
func Healthz(c *gin.Context) {
	c.JSON(http.StatusOK, HealthResponse{Status: "ok"})
}
