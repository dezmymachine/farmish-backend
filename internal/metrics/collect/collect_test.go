package collect

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/jobs"
	"github.com/dezmymachine/farmish-backend/internal/metrics"
	"github.com/dezmymachine/farmish-backend/pkg/logger"
)

var discard = logger.New(io.Discard, "error")

type fastRetry struct{}

func (fastRetry) NextRetry(*rivertype.JobRow) time.Time { return time.Now().Add(10 * time.Millisecond) }

type probeArgs struct{}

func (probeArgs) Kind() string { return "metrics_probe" }

// probeWorker fails its first attempt, then succeeds.
type probeWorker struct {
	river.WorkerDefaults[probeArgs]
	attempts atomic.Int32
}

func (w *probeWorker) Work(context.Context, *river.Job[probeArgs]) error {
	if w.attempts.Add(1) == 1 {
		return errors.New("transient")
	}
	return nil
}

func gather(t *testing.T, c prometheus.Collector) map[string]float64 {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(c)
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, f := range families {
		for _, m := range f.Metric {
			key := f.GetName()
			for _, l := range m.Label {
				key += "{" + l.GetName() + "=" + l.GetValue() + "}"
			}
			switch {
			case m.Gauge != nil:
				out[key] = m.Gauge.GetValue()
			case m.Counter != nil:
				out[key] = m.Counter.GetValue()
			}
		}
	}
	return out
}

func TestPoolCollector_ExposesPoolStats(t *testing.T) {
	pool := dbtest.Pool(t)
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := gather(t, NewPoolCollector(pool))
	for _, name := range []string{
		"db_pool_acquired_conns", "db_pool_idle_conns", "db_pool_total_conns",
		"db_pool_acquire_duration_seconds", "db_pool_acquires_total", "db_pool_empty_acquires_total",
	} {
		if _, ok := got[name]; !ok {
			t.Errorf("%s missing from %v", name, got)
		}
	}
	if got["db_pool_total_conns"] < 1 || got["db_pool_acquires_total"] < 1 {
		t.Errorf("a pinged pool must show connections and acquires: %v", got)
	}
}

func TestQueueCollector_CountsByStateAndCaches(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	reg := jobs.NewRegistry()
	jobs.Register(reg, &probeWorker{})
	client, err := jobs.NewClient(pool, reg, discard, jobs.Options{Work: false})
	if err != nil {
		t.Fatal(err)
	}
	c := NewQueueCollector(pool, discard)
	clock := time.Now()
	c.now = func() time.Time { return clock }

	got := gather(t, c)
	for _, state := range jobStates {
		if v, ok := got["jobs_queue_depth{state="+state+"}"]; !ok || v != 0 {
			t.Errorf("empty queue: state %s = %v (present %v), want an explicit 0", state, v, ok)
		}
	}

	if _, err := client.Insert(ctx, probeArgs{}, nil); err != nil {
		t.Fatal(err)
	}
	if got := gather(t, c); got["jobs_queue_depth{state=available}"] != 0 {
		t.Errorf("a scrape inside the TTL must serve the cache, got %v", got)
	}
	clock = clock.Add(queueDepthTTL + time.Second)
	if got := gather(t, c); got["jobs_queue_depth{state=available}"] != 1 {
		t.Errorf("after the TTL the count refreshes, got %v", got)
	}
}

func TestQueueCollector_DatabaseErrorServesLastKnown(t *testing.T) {
	pool := dbtest.Pool(t)
	c := NewQueueCollector(pool, discard)
	clock := time.Now()
	c.now = func() time.Time { return clock }
	gather(t, c)
	pool.Close()
	clock = clock.Add(time.Minute)
	got := gather(t, c)
	if _, ok := got["jobs_queue_depth{state=available}"]; !ok {
		t.Errorf("a database error must keep the last counts, got %v", got)
	}
}

func TestWatchJobs_CountsCompletedAndFailedAttempts(t *testing.T) {
	pool := dbtest.Pool(t)
	reg := jobs.NewRegistry()
	jobs.Register(reg, &probeWorker{})
	client, err := jobs.NewClient(pool, reg, discard, jobs.Options{
		Work: true, RetryPolicy: fastRetry{}, FetchPollInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := WatchJobs(ctx, client)
	defer stop()
	if err := client.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = jobs.Stop(client, 5*time.Second, 2*time.Second, discard) }()

	if _, err := client.Insert(context.Background(), probeArgs{}, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for testutil.ToFloat64(metrics.JobsCompleted.WithLabelValues("metrics_probe")) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("completed counter never moved")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := testutil.ToFloat64(metrics.JobsFailed.WithLabelValues("metrics_probe")); got != 1 {
		t.Errorf("failed attempts = %v, want 1 (the first attempt)", got)
	}
}
