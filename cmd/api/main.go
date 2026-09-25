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

	"github.com/dezmymachine/farmish-backend/internal/config"
	httpapi "github.com/dezmymachine/farmish-backend/internal/http"
	"github.com/dezmymachine/farmish-backend/pkg/logger"
)

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

	router := httpapi.NewRouter(cfg, log)
	addr := net.JoinHostPort("", strconv.Itoa(cfg.Port))
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	return httpapi.Serve(ctx, httpapi.NewServer(addr, router), ln, cfg.ShutdownTimeout, log)
}
