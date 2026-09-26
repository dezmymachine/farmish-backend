package middleware

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	legacyrouter "github.com/getkin/kin-openapi/routers/legacy"
	"github.com/gin-gonic/gin"

	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
)

// OpenAPIValidator rejects requests that don't match spec (path/query/header
// parameters, content type and body schema) with a 400 validation_failed
// envelope listing every problem.
//
// Requests for paths or methods the spec doesn't define pass through, so Gin's
// 404/405 handlers answer them. Security requirements are not checked here;
// the auth middleware owns 401/403.
func OpenAPIValidator(spec *openapi3.T) (gin.HandlerFunc, error) {
	// Match on path only: the API is reachable under several hosts (Railway,
	// Cloudflare, localhost), so the spec's server URLs must not constrain routing.
	spec.Servers = nil
	router, err := legacyrouter.NewRouter(spec)
	if err != nil {
		return nil, fmt.Errorf("openapi router: %w", err)
	}
	opts := &openapi3filter.Options{
		AuthenticationFunc: openapi3filter.NoopAuthenticationFunc,
		MultiError:         true,
	}

	return func(c *gin.Context) {
		route, pathParams, err := router.FindRoute(c.Request)
		if err != nil {
			if !errors.Is(err, routers.ErrPathNotFound) && !errors.Is(err, routers.ErrMethodNotAllowed) {
				_ = c.Error(err)
			}
			c.Next()
			return
		}
		err = openapi3filter.ValidateRequest(c.Request.Context(), &openapi3filter.RequestValidationInput{
			Request:    c.Request,
			PathParams: pathParams,
			Route:      route,
			Options:    opts,
		})
		if err != nil {
			apierror.AbortWithDetails(c, http.StatusBadRequest, apierror.CodeValidationFailed,
				"Request validation failed", validationDetails(err))
			return
		}
		c.Next()
	}, nil
}

// validationDetails flattens kin-openapi's nested errors into ErrorDetails:
// one per violated parameter or body field.
func validationDetails(err error) []api.ErrorDetail {
	var out []api.ErrorDetail
	for _, e := range flattenMulti(err) {
		var reqErr *openapi3filter.RequestError
		if !errors.As(e, &reqErr) {
			out = append(out, api.ErrorDetail{Location: api.ErrorDetailLocationBody, Message: "invalid request"})
			continue
		}
		out = append(out, requestErrorDetails(reqErr)...)
	}
	return out
}

func flattenMulti(err error) []error {
	multi, ok := err.(openapi3.MultiError) //nolint:errorlint // only the top-level list, not wrapped ones
	if !ok {
		return []error{err}
	}
	var out []error
	for _, e := range multi {
		out = append(out, flattenMulti(e)...)
	}
	return out
}

func requestErrorDetails(re *openapi3filter.RequestError) []api.ErrorDetail {
	loc, name := api.ErrorDetailLocationBody, ""
	if p := re.Parameter; p != nil {
		loc, name = api.ErrorDetailLocation(p.In), p.Name
	}

	leaves := schemaLeaves(re.Err)
	if len(leaves) == 0 {
		return []api.ErrorDetail{{Location: loc, Field: optional(name), Message: fallbackReason(re)}}
	}
	out := make([]api.ErrorDetail, 0, len(leaves))
	for _, se := range leaves {
		msg, ptr := describe(se)
		field := name
		if loc == api.ErrorDetailLocationBody {
			field = ptr
		}
		out = append(out, api.ErrorDetail{Location: loc, Field: optional(field), Message: msg})
	}
	return out
}

// schemaLeaves returns the innermost schema errors. With OpenAPI 3.1, kin
// reports one SchemaError whose Origin wraps a MultiError of per-field ones.
func schemaLeaves(err error) []*openapi3.SchemaError {
	switch e := err.(type) { //nolint:errorlint // walking the chain by hand
	case nil:
		return nil
	case openapi3.MultiError:
		var out []*openapi3.SchemaError
		for _, x := range e {
			out = append(out, schemaLeaves(x)...)
		}
		return out
	case *openapi3.SchemaError:
		if inner := schemaLeaves(e.Origin); len(inner) > 0 {
			return inner
		}
		return []*openapi3.SchemaError{e}
	default:
		return schemaLeaves(errors.Unwrap(err))
	}
}

// Reason prefixes carrying the failing JSON pointer, outermost first:
// `error at "/qty": at '/qty': minimum: got 0, want 1`.
var pointerPrefixes = []*regexp.Regexp{
	regexp.MustCompile(`^error at "([^"]*)": `),
	regexp.MustCompile(`^at '([^']*)': `),
}

// describe returns a client-facing message and JSON pointer for a leaf error.
func describe(se *openapi3.SchemaError) (msg, ptr string) {
	msg = strings.TrimPrefix(se.Reason, "validation failed due to: ")
	if p := se.JSONPointer(); len(p) > 0 {
		ptr = "/" + strings.Join(p, "/")
	}
	for _, re := range pointerPrefixes {
		if m := re.FindStringSubmatch(msg); m != nil {
			if ptr == "" {
				ptr = m[1]
			}
			msg = msg[len(m[0]):]
		}
	}
	return msg, ptr
}

// fallbackReason is used when there is no schema error to describe, e.g.
// malformed JSON, a wrong content type or a missing required value.
func fallbackReason(e *openapi3filter.RequestError) string {
	var pe *openapi3filter.ParseError
	switch {
	case errors.As(e.Err, &pe):
		return "malformed value"
	case errors.Is(e.Err, openapi3filter.ErrInvalidRequired):
		return "is required"
	case e.Reason != "":
		return e.Reason
	default:
		return "invalid value"
	}
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
