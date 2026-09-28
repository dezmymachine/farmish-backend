package metrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Handler serves the registry in the Prometheus text format.
func Handler() http.Handler {
	return promhttp.HandlerFor(Registry, promhttp.HandlerOpts{Registry: Registry})
}

// Start binds addr and serves /metrics in the background until ctx ends, then
// shuts down within timeout. The returned channel closes once the server has
// stopped. Binding is synchronous, so a taken port fails startup instead of
// silently losing metrics.
//
// It is a separate listener from the public API on purpose: the public router
// carries no metrics route, and Railway scrapes the private address. An empty
// addr disables it (the channel is already closed).
func Start(ctx context.Context, addr string, timeout time.Duration, log *slog.Logger) (<-chan struct{}, error) {
	done := make(chan struct{})
	if addr == "" {
		close(done)
		return done, nil
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("metrics listen on %s: %w", addr, err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", Handler())
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second,
	}
	go func() {
		defer close(done)
		errCh := make(chan error, 1)
		go func() {
			log.Info("metrics listening", slog.String("addr", ln.Addr().String()))
			errCh <- srv.Serve(ln)
		}()
		select {
		case err := <-errCh:
			log.Error("metrics server stopped", slog.String("error", err.Error()))
			return
		case <-ctx.Done():
		}
		// The context is already cancelled here; shutdown needs its own deadline.
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Warn("metrics shutdown", slog.String("error", err.Error()))
		}
		if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Warn("metrics server", slog.String("error", err.Error()))
		}
	}()
	return done, nil
}
