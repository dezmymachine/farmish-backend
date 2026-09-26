package main

import (
	"testing"
	"time"
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
