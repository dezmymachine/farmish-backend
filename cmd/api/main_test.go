package main

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/jobs"
	"github.com/dezmymachine/farmish-backend/internal/notify"
	"github.com/dezmymachine/farmish-backend/pkg/logger"
)

func TestShutdownBudget(t *testing.T) {
	for _, total := range []time.Duration{3 * time.Second, 9 * time.Second, 30 * time.Second} {
		drain, soft, hard, db := shutdownBudget(total)
		// HTTP drain and job stop run concurrently, so the worst case is
		// drain + db (jobs get soft + hard == drain).
		if drain+db != total || soft+hard != drain || soft <= 0 || hard <= 0 || db <= 0 {
			t.Errorf("total %v: drain %v soft %v hard %v db %v", total, drain, soft, hard, db)
		}
	}
}

// TestRegistry_BalanceCheckOnlyWithTheRealGateway proves the daily mNotify
// balance check is registered when SMS goes through mNotify and not when the
// log-only sender is in use: River rejects an insert whose kind has no worker.
func TestRegistry_BalanceCheckOnlyWithTheRealGateway(t *testing.T) {
	pool := dbtest.Pool(t)
	log := logger.New(io.Discard, "error")
	for name, tt := range map[string]struct {
		sender notify.SMS
		want   bool
	}{
		"mnotify":  {notify.NewMNotify("k", "FARMISH", "http://127.0.0.1:1"), true},
		"log only": {notify.LogOnly{Log: log}, false},
	} {
		t.Run(name, func(t *testing.T) {
			client, err := jobs.NewClient(pool, registry(log, nil, nil, nil, nil, nil, nil, tt.sender, 200, pool), log, jobs.Options{})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Insert(context.Background(), notify.CheckBalanceArgs{}, nil)
			if (err == nil) != tt.want {
				t.Errorf("insert notify.check_balance: err = %v, registered want %v", err, tt.want)
			}
		})
	}
}
