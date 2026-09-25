package database

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dezmymachine/farmish-backend/internal/config"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
)

func TestPoolConfig(t *testing.T) {
	pc, err := PoolConfig(config.DB{
		URL:              "postgres://u:p@localhost:5432/farmish",
		MaxConns:         7,
		StatementTimeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pc.MaxConns != 7 || pc.ConnConfig.ConnectTimeout != connectTimeout ||
		pc.MaxConnLifetime != maxConnLifetime || pc.MaxConnIdleTime != maxConnIdleTime ||
		pc.HealthCheckPeriod != healthCheckPeriod {
		t.Errorf("pool settings not applied: %+v", pc)
	}
	rp := pc.ConnConfig.RuntimeParams
	if rp["statement_timeout"] != "3000" || rp["application_name"] != "farmish-api" {
		t.Errorf("runtime params = %v", rp)
	}
}

func TestPoolConfig_URLParamsWin(t *testing.T) {
	pc, err := PoolConfig(config.DB{
		URL:              "postgres://u:p@localhost/farmish?statement_timeout=500&application_name=migrator",
		MaxConns:         1,
		StatementTimeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	rp := pc.ConnConfig.RuntimeParams
	if rp["statement_timeout"] != "500" || rp["application_name"] != "migrator" {
		t.Errorf("URL params overridden: %v", rp)
	}
}

func TestPoolConfig_InvalidURLHidesSecret(t *testing.T) {
	_, err := PoolConfig(config.DB{URL: "postgres://u:hunter2@localhost:notaport/db", MaxConns: 1})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error leaks password: %v", err)
	}
}

func TestOpen(t *testing.T) {
	ctx := context.Background()

	t.Run("reachable", func(t *testing.T) {
		pool, err := Open(ctx, config.DB{URL: dbtest.EmptyURL(t), MaxConns: 2, StatementTimeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		defer pool.Close()
		var timeout string
		if err := pool.QueryRow(ctx, "SHOW statement_timeout").Scan(&timeout); err != nil {
			t.Fatal(err)
		}
		if timeout != "1s" {
			t.Errorf("statement_timeout = %q, want 1s", timeout)
		}
	})

	t.Run("unreachable fails fast", func(t *testing.T) {
		start := time.Now()
		_, err := Open(ctx, config.DB{URL: "postgres://u:p@127.0.0.1:1/db", MaxConns: 1})
		if err == nil {
			t.Fatal("expected error")
		}
		if d := time.Since(start); d > startupPingWait+time.Second {
			t.Errorf("took %v", d)
		}
	})
}

func TestClose(t *testing.T) {
	pool, err := Open(context.Background(), config.DB{URL: dbtest.EmptyURL(t), MaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !Close(pool, 5*time.Second) {
		t.Error("healthy pool did not close in time")
	}
}
