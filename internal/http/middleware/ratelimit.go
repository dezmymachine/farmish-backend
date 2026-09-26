package middleware

import (
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/gin-gonic/gin"

	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/internal/ratelimit"
	"github.com/dezmymachine/farmish-backend/internal/users"
	"github.com/dezmymachine/farmish-backend/pkg/logger"
)

// rateLimitExtension names a stricter per-operation policy, e.g.
// `x-farmish-rate-limit: sensitive`.
const rateLimitExtension = "x-farmish-rate-limit"

// RateLimits are the limits the router applies.
type RateLimits struct {
	// IP applies to every request except the health probes, keyed by client
	// address (IPv6 by /64). Generous: mobile carriers put many subscribers
	// behind one address (CGNAT).
	IP ratelimit.Rule
	// User applies to authenticated requests, keyed by user ID.
	User ratelimit.Rule
	// Operation holds named policies for x-farmish-rate-limit, keyed by the
	// user when signed in, else by client address.
	Operation map[string]ratelimit.Rule
}

// DefaultRateLimits are the production limits.
func DefaultRateLimits() RateLimits {
	return RateLimits{
		IP:   ratelimit.Rule{Name: "ip", Limit: 300, Period: time.Minute, Burst: 100},
		User: ratelimit.Rule{Name: "user", Limit: 120, Period: time.Minute, Burst: 60},
		Operation: map[string]ratelimit.Rule{
			// Writes that cost money or notify people, and anonymous forms.
			"sensitive": {Name: "sensitive", Limit: 10, Period: time.Minute, Burst: 5},
		},
	}
}

// Validate checks every rule.
func (r RateLimits) Validate() error {
	rules := []ratelimit.Rule{r.IP, r.User}
	for _, rule := range r.Operation {
		rules = append(rules, rule)
	}
	for _, rule := range rules {
		if err := rule.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// RateLimitIP limits every request by client address, except paths in skip.
func RateLimitIP(l ratelimit.Limiter, rule ratelimit.Rule, skip ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if slices.Contains(skip, c.Request.URL.Path) {
			c.Next()
			return
		}
		if !limit(c, l, rule, ratelimit.ClientKey(GetClientIP(c))) {
			return
		}
		c.Next()
	}
}

// RateLimitOperations must run after Authenticate. It limits signed-in users
// by limits.User, and applies the operation's x-farmish-rate-limit policy.
func RateLimitOperations(spec *openapi3.T, l ratelimit.Limiter, limits RateLimits) (gin.HandlerFunc, error) {
	if err := forEachOperation(spec, func(method, path string, op *openapi3.Operation) error {
		raw, present := op.Extensions[rateLimitExtension]
		if !present {
			return nil
		}
		name, _ := raw.(string)
		if _, ok := limits.Operation[name]; !ok {
			return fmt.Errorf("%s %s: unknown %s policy %v", method, path, rateLimitExtension, raw)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	router, err := newSpecRouter(spec)
	if err != nil {
		return nil, err
	}

	return func(c *gin.Context) {
		u, signedIn := users.FromContext(c.Request.Context())
		key := ratelimit.ClientKey(GetClientIP(c))
		if signedIn {
			key = u.ID.String()
			if !limit(c, l, limits.User, key) {
				return
			}
		}
		if route, _, err := router.FindRoute(c.Request); err == nil {
			if name, ok := route.Operation.Extensions[rateLimitExtension].(string); ok {
				if !limit(c, l, limits.Operation[name], key) {
					return
				}
			}
		}
		c.Next()
	}, nil
}

// limit applies rule to key, sets the X-RateLimit-* headers and, when the
// limit is exceeded, aborts with 429 and Retry-After. Limiter backend errors
// fail open (logged): an outage of a future shared backend shouldn't take the
// API down.
func limit(c *gin.Context, l ratelimit.Limiter, rule ratelimit.Rule, key string) bool {
	d, err := l.Allow(c.Request.Context(), rule, key)
	if err != nil {
		logger.FromContext(c.Request.Context()).Error("rate limiter failed; allowing request",
			slog.String("rule", rule.Name), slog.String("error", err.Error()))
		return true
	}
	c.Header("X-RateLimit-Limit", strconv.Itoa(d.Limit))
	c.Header("X-RateLimit-Remaining", strconv.Itoa(d.Remaining))
	c.Header("X-RateLimit-Reset", ceilSeconds(d.Reset))
	if d.Allowed {
		return true
	}
	c.Header("Retry-After", ceilSeconds(d.RetryAfter))
	logger.FromContext(c.Request.Context()).Info("rate limited", slog.String("rule", rule.Name))
	apierror.Abort(c, http.StatusTooManyRequests, apierror.CodeRateLimited, "Too many requests; slow down and retry later")
	return false
}

func ceilSeconds(d time.Duration) string {
	return strconv.Itoa(int(math.Ceil(d.Seconds())))
}
