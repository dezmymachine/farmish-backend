package middleware

import (
	"fmt"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/routers"
	legacyrouter "github.com/getkin/kin-openapi/routers/legacy"
)

// newSpecRouter builds an OpenAPI router that matches on path only: the API
// answers on several hosts (Railway, Cloudflare, localhost), so the spec's
// server URLs must not constrain routing. It mutates spec.Servers.
func newSpecRouter(spec *openapi3.T) (routers.Router, error) {
	spec.Servers = nil
	r, err := legacyrouter.NewRouter(spec)
	if err != nil {
		return nil, fmt.Errorf("openapi router: %w", err)
	}
	return r, nil
}

// forEachOperation calls fn for every operation in spec.
func forEachOperation(spec *openapi3.T, fn func(method, path string, op *openapi3.Operation) error) error {
	for path, item := range spec.Paths.Map() {
		for method, op := range item.Operations() {
			if err := fn(method, path, op); err != nil {
				return err
			}
		}
	}
	return nil
}
