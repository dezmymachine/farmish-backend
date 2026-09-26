// Command seed loads reference data (catalog categories, attributes and
// promotion tiers) into DATABASE_URL. It is idempotent: a repeat run changes
// nothing.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/dezmymachine/farmish-backend/internal/catalog"
	"github.com/dezmymachine/farmish-backend/internal/config"
	"github.com/dezmymachine/farmish-backend/internal/database"
	"github.com/dezmymachine/farmish-backend/internal/promotions"
	"github.com/dezmymachine/farmish-backend/pkg/logger"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		os.Exit(1)
	}
}

func run() error {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	level := os.Getenv("LOG_LEVEL")
	if level == "" {
		level = "info"
	}
	log := logger.New(os.Stdout, level)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, err := database.Open(ctx, config.DB{URL: dbURL, MaxConns: 5, StatementTimeout: 30 * time.Second})
	if err != nil {
		return err
	}
	defer database.Close(pool, 5*time.Second)
	if err := catalog.Seed(ctx, pool); err != nil {
		return err
	}
	log.Info("catalog seeded")
	if err := promotions.Seed(ctx, pool); err != nil {
		return err
	}
	log.Info("promotion tiers seeded")
	return nil
}
