package handlers

import (
	"context"
	"errors"

	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/users"
)

// errNoUser means an authenticated operation ran without the auth middleware
// having set a user: a wiring bug, answered with a generic 500.
var errNoUser = errors.New("authenticated operation reached without a user in context")

// GetMe returns the signed-in user. The auth middleware has already resolved
// (or created) the users row.
func (Server) GetMe(ctx context.Context, _ api.GetMeRequestObject) (api.GetMeResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	return api.GetMe200JSONResponse(toMe(u)), nil
}

// UpdateMe updates the signed-in user's editable fields. The request body was
// validated against the spec (1-80 chars, not blank) before reaching here.
func (s Server) UpdateMe(ctx context.Context, req api.UpdateMeRequestObject) (api.UpdateMeResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	updated, err := s.Users.UpdateDisplayName(ctx, u.ID, req.Body.DisplayName)
	if err != nil {
		return nil, err
	}
	return api.UpdateMe200JSONResponse(toMe(updated)), nil
}

func toMe(u users.User) api.Me {
	return api.Me{
		Id:             u.ID,
		Email:          u.Email,
		EmailVerified:  u.EmailVerified,
		Phone:          u.Phone,
		DisplayName:    u.DisplayName,
		SignupMethod:   api.MeSignupMethod(u.SignupMethod),
		Role:           api.MeRole(u.Role),
		SellerVerified: u.SellerVerified,
		CreatedAt:      u.CreatedAt,
		UpdatedAt:      u.UpdatedAt,
	}
}
