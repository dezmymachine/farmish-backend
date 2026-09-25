package middleware

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/gin-gonic/gin"

	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/pkg/logger"
)

// Recovery turns a handler panic into a logged error and a generic 500
// envelope. http.ErrAbortHandler is re-panicked so net/http can abort the
// connection as intended.
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(rec)
			}
			logger.FromContext(c.Request.Context()).Error("panic recovered",
				slog.String("panic", fmt.Sprint(rec)),
				slog.String("stack", string(debug.Stack())),
			)
			if c.Writer.Written() {
				c.Abort()
				return
			}
			apierror.Abort(c, http.StatusInternalServerError, apierror.CodeInternal, "Internal server error")
		}()
		c.Next()
	}
}
