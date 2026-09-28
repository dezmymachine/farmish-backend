package middleware

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/dezmymachine/farmish-backend/internal/metrics"
)

// unmatchedRoute labels requests no route matched (404, 405, scanners). It
// is a constant so probing random paths cannot mint new label values.
const unmatchedRoute = "unmatched"

// Metrics records http_requests_total and http_request_duration_seconds. The
// route label is the matched template (c.FullPath), never the raw path: raw
// paths carry ids and slugs, and one series per id is a cardinality explosion.
// Probes are skipped, since orchestrator health checks would otherwise
// dominate every ratio (the 5xx rate especially).
func Metrics(skip ...string) gin.HandlerFunc {
	skipped := make(map[string]struct{}, len(skip))
	for _, s := range skip {
		skipped[s] = struct{}{}
	}
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		route := c.FullPath()
		if route == "" {
			route = unmatchedRoute
		}
		if _, ok := skipped[route]; ok {
			return
		}
		metrics.HTTPRequests.WithLabelValues(c.Request.Method, route, strconv.Itoa(c.Writer.Status())).Inc()
		metrics.HTTPDuration.WithLabelValues(c.Request.Method, route).Observe(time.Since(start).Seconds())
	}
}
