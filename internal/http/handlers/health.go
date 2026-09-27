// Package handlers implements the generated api.StrictServerInterface.
package handlers

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/internal/listings"
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
	DB       Pinger
	Users    UserStore
	Sellers  SellerStore
	Catalog  CatalogStore
	Media    MediaStore
	Listings ListingStore
	// PublicListings is the public read side of the same service. It is a
	// separate field so the seller write surface and the public browse
	// surface stay separate contracts.
	PublicListings PublicListingStore
	// Payments settles provider webhooks and serves payment status. It is nil
	// when Paystack is not configured, and the two operations then answer 503.
	Payments PaymentStore
	// Promotions sells packages, reports balances and applies promotions.
	Promotions PromotionStore
	// Checkout prices carts, creates checkouts and serves the buyer poll.
	Checkout CheckoutStore
	// Orders reads buyer and seller orders.
	Orders OrderStore
	// OrderActions moves an order through DOMAIN §4's table.
	OrderActions OrderActions
	// Views enqueues the listing view count. Nil disables counting, which
	// keeps the read path working without a job queue.
	Views listings.ViewCounter
	// ViewerHash turns a caller's address into a stable, one-way id for view
	// counting. Nil disables counting, since an unhashed address must never
	// reach the job queue.
	ViewerHash func(ctx context.Context, ip string) uuid.UUID
	Log        *slog.Logger
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
