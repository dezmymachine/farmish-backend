package handlers

import (
	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

// validationFailed maps a domain *validation.Error to a 400 envelope with
// per-field details (location "body"), matching the validator middleware's
// shape for spec-level violations.
func validationFailed(err *validation.Error) api.Error {
	details := make([]api.ErrorDetail, 0, len(err.Fields))
	for i := range err.Fields {
		details = append(details, api.ErrorDetail{
			Location: api.ErrorDetailLocationBody,
			Field:    &err.Fields[i].Name,
			Message:  err.Fields[i].Message,
		})
	}
	return apierror.WithDetails(apierror.CodeValidationFailed, "Request validation failed", details)
}
