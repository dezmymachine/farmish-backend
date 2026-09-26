# ADR-0006: API contract, codegen and request validation

- **Status:** Accepted
- **Date:** 2026-09-26
- **Phase:** 3

## Decisions

1. **OpenAPI 3.1.0 with oapi-codegen v2.8.0.** v2.8.0 is the first release with (initial) 3.1 support, built on kin-openapi's 3.1 support. The spec at `api/openapi.yaml` is the source of truth.

2. **Generated code lives in `internal/http/api/api.gen.go`** (package `api`), not `internal/http/api.gen.go` as §4 said. `handlers` implements the generated `StrictServerInterface`, and `httpapi` imports both. Putting the generated code in `httpapi` would create an import cycle.

3. **Codegen config** (`api/oapi-codegen.yaml`): Gin server + strict server + models + embedded spec, plus:
   - `skip-prune`, so shared schemas like Money and PageMeta exist before any path uses them
   - `always-prefix-enum-values`, giving `ErrorDetailLocationBody` rather than a bare `Body` that would collide later

4. **The generated default error handlers are always overridden.** Their defaults return `{"msg": err.Error()}`, which leaks internals and breaks the envelope.
   - Handler/response errors: log, then a generic 500 `internal_error`.
   - Binding errors: a 400 `bad_request`.

5. **API naming conventions:**
   - camelCase JSON properties and query parameters, e.g. `minPrice`
   - snake_case error `code`s, which are stable and may never be renamed
   - `message` is for humans only

   Error codes so far: `bad_request`, `validation_failed`, `not_found`, `method_not_allowed`, `internal_error`, `unavailable`.

6. **The error envelope uses generated types.** `apierror` builds `api.Error`, so the Go envelope can't drift from the spec's `Error` schema. Tests validate real responses against the spec.

7. **Request validation** (`middleware.OpenAPIValidator`, kin-openapi `openapi3filter`):
   - Checks path, query, header and cookie params, content type and body schema, reporting **all** problems.
   - The response is 400 `validation_failed` with `details[]` of `{location, field, message}`. `field` is the parameter name, or a JSON pointer for body fields.
   - With 3.1 specs, kin nests per-field errors under a single `SchemaError.Origin`. The middleware walks down to the leaf errors and strips the validator's `error at "…": at '…': ` prefixes.
   - Paths and methods not in the spec pass through to Gin's 404/405 envelopes.
   - Security requirements are **not** enforced here (`NoopAuthenticationFunc`). Auth middleware owns 401/403 (Phase 4).
   - Spec `servers` are ignored when routing, because the API answers on several hosts.
   - Middleware order: CORS runs first, so preflights never hit validation.

8. **Spec lint uses Redocly CLI, pinned and run from Docker**, with `recommended-strict`. Two rules are off: `no-unused-components` (shared components come before their users) and `operation-4xx-response` (the probes have no 4xx outcomes). `localhost` was dropped from `servers`, as the rule requires.

9. **The drift check** regenerates into a temp dir and diffs against the committed file. It fails both when the spec changes without a regenerate and when generated code is hand-edited. It runs in `make ci` (ADR-0004), not a GitHub workflow.

10. **`ContextWithFallback` is on**, so strict handlers, which get `*gin.Context` as their `context.Context`, see values that middleware put on the request context (the request-scoped logger).

## Consequences

- To add an endpoint: edit `api/openapi.yaml`, run `make generate`, implement the new method on `handlers.Server`. The compiler catches missing methods.
- "An invalid request returns 400" is proven with a fixture spec (`internal/http/middleware/testdata/validate.yaml`), because the real spec has no inputs yet. Real endpoints get validated automatically once they're added.
- Strict JSON responses end with a newline (`json.Encoder`); Gin-written envelopes don't. Clients must not compare bodies byte-for-byte.
