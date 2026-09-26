// Package handlers implements the generated api.StrictServerInterface.
package handlers

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/internal/users"
	"github.com/dezmymachine/farmish-backend/pkg/logger"
)

// readyTimeout bounds each dependency check so a hung DB can't hang probes.
const readyTimeout = 2 * time.Second

// Pinger is a dependency that can report whether it's reachable
// (*pgxpool.Pool satisfies it).
type Pinger interface {
	Ping(ctx context.Context) error
}

// UserStore is the part of users.Service the handlers use.
type UserStore interface {
	UpdateDisplayName(ctx context.Context, id uuid.UUID, name string) (users.User, error)
}

// Server implements every operation in api/openapi.yaml.
type Server struct {
	DB    Pinger
	Users UserStore
}

var _ api.StrictServerInterface = Server{}

// GetHealthz is the liveness probe. It checks only that the process serves HTTP.
func (Server) GetHealthz(context.Context, api.GetHealthzRequestObject) (api.GetHealthzResponseObject, error) {
	return api.GetHealthz200JSONResponse{Status: api.HealthStatusStatusOk}, nil
}

// GetReadyz is the readiness probe: 200 when the database answers a ping
// within readyTimeout, otherwise a generic 503 (the cause is only logged).
func (s Server) GetReadyz(ctx context.Context, _ api.GetReadyzRequestObject) (api.GetReadyzResponseObject, error) {
	pingCtx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	if err := s.DB.Ping(pingCtx); err != nil {
		logger.FromContext(ctx).Warn("readiness check failed",
			slog.String("dependency", "postgres"), slog.String("error", err.Error()))
		return api.GetReadyz503JSONResponse{
			UnavailableJSONResponse: api.UnavailableJSONResponse(apierror.New(apierror.CodeUnavailable, "Service not ready")),
		}, nil
	}
	return api.GetReadyz200JSONResponse{Status: api.HealthStatusStatusOk}, nil
}
