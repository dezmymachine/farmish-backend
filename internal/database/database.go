// Package database opens and configures the pgx connection pool.
package database

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/config"
)

// Pool defaults. Neon closes idle connections server-side, so recycle them
// well before that and health-check regularly.
const (
	connectTimeout    = 5 * time.Second
	maxConnLifetime   = 30 * time.Minute
	maxConnIdleTime   = 5 * time.Minute
	healthCheckPeriod = 30 * time.Second
	startupPingWait   = 10 * time.Second
)

// PoolConfig builds a pgxpool config from cfg, applying timeouts and the
// server-side statement_timeout unless the URL already sets one.
func PoolConfig(cfg config.DB) (*pgxpool.Config, error) {
	pc, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		// pgx errors can echo the URL (and password); keep the message generic.
		return nil, fmt.Errorf("parse DATABASE_URL: invalid connection string")
	}
	pc.MaxConns = cfg.MaxConns
	pc.MaxConnLifetime = maxConnLifetime
	pc.MaxConnLifetimeJitter = maxConnLifetime / 10
	pc.MaxConnIdleTime = maxConnIdleTime
	pc.HealthCheckPeriod = healthCheckPeriod
	pc.ConnConfig.ConnectTimeout = connectTimeout

	rp := pc.ConnConfig.RuntimeParams
	if _, ok := rp["statement_timeout"]; !ok && cfg.StatementTimeout > 0 {
		rp["statement_timeout"] = strconv.FormatInt(cfg.StatementTimeout.Milliseconds(), 10)
	}
	if _, ok := rp["application_name"]; !ok {
		rp["application_name"] = "farmish-api"
	}
	return pc, nil
}

// Open creates the pool and verifies connectivity, failing fast on a bad URL
// or unreachable database.
func Open(ctx context.Context, cfg config.DB) (*pgxpool.Pool, error) {
	pc, err := PoolConfig(cfg)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, startupPingWait)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

// Close closes pool, giving up after timeout. pgxpool.Close waits for every
// connection, and one stuck on an unreachable database can block it until the
// OS gives up on the socket, long past a container's SIGTERM grace period.
// It reports whether the pool closed in time.
func Close(pool *pgxpool.Pool, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		pool.Close()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}
