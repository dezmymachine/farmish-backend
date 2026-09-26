package handlers

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/internal/media"
	"github.com/dezmymachine/farmish-backend/internal/users"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

// MediaStore is the part of media.Service the handlers use.
type MediaStore interface {
	CreateUpload(ctx context.Context, ownerID uuid.UUID, purpose, contentType string, size int64) (media.Upload, error)
}

// CreateMediaUploadUrl returns a presigned PUT for one image. The caller is
// the owner: identity comes from the verified token only.
func (s Server) CreateMediaUploadUrl(ctx context.Context, req api.CreateMediaUploadUrlRequestObject) (api.CreateMediaUploadUrlResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	if req.Body == nil {
		return api.CreateMediaUploadUrl400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(apierror.New(apierror.CodeBadRequest, "Malformed request")),
		}, nil
	}
	// Presign makes no outgoing call, but the service is IO-bound: unwrap so
	// nothing can hold gin's pooled context (see requestContext).
	up, err := s.Media.CreateUpload(requestContext(ctx), u.ID, string(req.Body.Purpose), string(req.Body.ContentType), req.Body.SizeBytes)
	var verr *validation.Error
	switch {
	case errors.As(err, &verr):
		return api.CreateMediaUploadUrl400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(validationFailed(verr)),
		}, nil
	case err != nil:
		return nil, err
	}
	return api.CreateMediaUploadUrl200JSONResponse(api.MediaUpload{
		MediaId: up.ID, UploadUrl: up.URL, Method: api.MediaUploadMethod(up.Method),
		Headers: up.Headers, ExpiresAt: up.ExpiresAt, PublicUrl: up.PublicURL,
	}), nil
}
