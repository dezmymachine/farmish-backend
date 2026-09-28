package metrics_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/metrics"
	"github.com/dezmymachine/farmish-backend/internal/metrics/collect"
	"github.com/dezmymachine/farmish-backend/pkg/logger"
)

func scrape(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url) //nolint:gosec // test URL, loopback
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("scrape: status %d, %v", resp.StatusCode, err)
	}
	return string(body)
}

// TestMetrics_ExposesEverySpecifiedSeries is the Phase 21 "metrics are
// exposed" check: every name the spec lists appears on /metrics.
func TestMetrics_ExposesEverySpecifiedSeries(t *testing.T) {
	pool := dbtest.Pool(t)
	log := logger.New(io.Discard, "error")
	if err := collect.RegisterDatabase(pool, log); err != nil {
		t.Fatal(err)
	}
	// Vector metrics only exist once a label set has been seen.
	metrics.HTTPRequests.WithLabelValues("GET", "/x", "200").Inc()
	metrics.HTTPDuration.WithLabelValues("GET", "/x").Observe(0.01)
	metrics.JobsCompleted.WithLabelValues("k").Inc()
	metrics.JobsFailed.WithLabelValues("k").Inc()
	metrics.PaymentsTotal.WithLabelValues("checkout", metrics.StatusSucceeded).Inc()
	metrics.PayoutsTotal.WithLabelValues(metrics.StatusSucceeded).Inc()
	metrics.RefundsTotal.WithLabelValues(metrics.StatusProcessed).Inc()
	metrics.RateLimitDenied.WithLabelValues("user").Inc()

	srv := httptest.NewServer(metrics.Handler())
	defer srv.Close()
	body := scrape(t, srv.URL)
	for _, name := range []string{
		"http_requests_total", "http_request_duration_seconds_bucket",
		"db_pool_acquired_conns", "db_pool_idle_conns", "db_pool_total_conns", "db_pool_acquire_duration_seconds",
		"jobs_queue_depth", "jobs_completed_total", "jobs_failed_total",
		"payments_total", "payment_amount_mismatch_total",
		"payouts_total", "payout_failures_total", "refunds_total", "ledger_reconcile_violations",
		"ratelimit_denied_total", "ratelimit_fallback_total",
		"go_goroutines",
	} {
		if !strings.Contains(body, "\n"+name) && !strings.HasPrefix(body, name) {
			t.Errorf("%s is not exposed", name)
		}
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func TestStart_ServesOnItsOwnListenerAndStops(t *testing.T) {
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	done, err := metrics.Start(ctx, addr, 2*time.Second, logger.New(io.Discard, "error"))
	if err != nil {
		t.Fatal(err)
	}
	if body := scrape(t, "http://"+addr+"/metrics"); !strings.Contains(body, "go_goroutines") {
		t.Errorf("no runtime metrics in %.200s", body)
	}
	resp, err := http.Post("http://"+addr+"/metrics", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /metrics = %d, want 405", resp.StatusCode)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("listener did not stop after the context ended")
	}
	if resp, err := http.Get("http://" + addr + "/metrics"); err == nil { //nolint:gosec // loopback test
		_ = resp.Body.Close()
		t.Error("the listener is still accepting after shutdown")
	}
}

func TestStart_TakenPortFailsBoot(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := metrics.Start(ctx, ln.Addr().String(), time.Second, logger.New(io.Discard, "error")); err == nil {
		t.Fatal("binding a taken port must fail startup, not lose metrics silently")
	}
}

func TestStart_EmptyAddrDisables(t *testing.T) {
	done, err := metrics.Start(context.Background(), "", time.Second, logger.New(io.Discard, "error"))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	default:
		t.Fatal("a disabled listener's done channel must be closed already")
	}
}
