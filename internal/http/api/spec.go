package api

import (
	"sync"

	"github.com/getkin/kin-openapi/openapi3"
)

// The parsed contract, memoized: parsing the whole spec costs ~a second
// under the race detector, and the router builds plus every contract
// assertion would otherwise re-parse it hundreds of times per test run.
// The spec is immutable at runtime, so sharing it is safe.
var (
	specOnce sync.Once
	spec     *openapi3.T
	specErr  error
)

// CachedSpec returns GetSpec's result, parsed once per process.
func CachedSpec() (*openapi3.T, error) {
	specOnce.Do(func() {
		spec, specErr = GetSpec()
	})
	return spec, specErr
}
