// Command api runs the Farmish HTTP API.
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/dezmymachine/farmish-backend/internal/config"
	"github.com/dezmymachine/farmish-backend/internal/database"
	httpapi "github.com/dezmymachine/farmish-backend/internal/http"
	"github.com/dezmymachine/farmish-backend/pkg/logger"
)

// dbCloseTimeout bounds pool shutdown so SIGTERM exits within the platform's
// grace period even when the database is unreachable.
const dbCloseTimeout = 5 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := logger.New(os.Stdout, cfg.LogLevel).With("service", "farmish-api", "env", string(cfg.Env))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := database.Open(ctx, cfg.DB)
	if err != nil {
		return err
	}
	defer func() {
		if !database.Close(pool, dbCloseTimeout) {
			log.Warn("database pool did not close in time; exiting anyway", "timeout", dbCloseTimeout)
		}
	}()
	log.Info("database connected", "max_conns", cfg.DB.MaxConns)

	router := httpapi.NewRouter(cfg, log, httpapi.Deps{DB: pool})
	addr := net.JoinHostPort("", strconv.Itoa(cfg.Port))
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	return httpapi.Serve(ctx, httpapi.NewServer(addr, router), ln, cfg.ShutdownTimeout, log)
}
