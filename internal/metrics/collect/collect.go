// Package collect holds the metrics that read from the database and River. It
// is separate from package metrics so packages that only record counters
// (rate limiting, middleware) do not depend on the database layer.
package collect

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/riverqueue/river"

	"github.com/dezmymachine/farmish-backend/internal/db"
	"github.com/dezmymachine/farmish-backend/internal/jobs"
	"github.com/dezmymachine/farmish-backend/internal/metrics"
)

// queueDepthTTL caches the river_job count between scrapes. The count is a
// full GROUP BY over the queue table, and several scrapers (or a tight scrape
// interval) must not multiply it.
const queueDepthTTL = 15 * time.Second

// jobStates are River's job states. Each is always exported, so a state that
// empties reads 0 instead of disappearing.
var jobStates = []string{"available", "cancelled", "completed", "discarded", "pending", "retryable", "running", "scheduled"}

// PoolCollector exposes pgxpool.Stat() on every scrape.
type PoolCollector struct {
	pool                                   *pgxpool.Pool
	acquired, idle, total                  *prometheus.Desc
	acquireDuration, acquireCount, emptied *prometheus.Desc
}

// NewPoolCollector returns a collector for pool.
func NewPoolCollector(pool *pgxpool.Pool) *PoolCollector {
	d := func(name, help string) *prometheus.Desc { return prometheus.NewDesc(name, help, nil, nil) }
	return &PoolCollector{
		pool:            pool,
		acquired:        d("db_pool_acquired_conns", "Connections currently checked out of the pool."),
		idle:            d("db_pool_idle_conns", "Idle connections in the pool."),
		total:           d("db_pool_total_conns", "Connections in the pool, in any state."),
		acquireDuration: d("db_pool_acquire_duration_seconds", "Cumulative time spent acquiring connections; divide its rate by the acquire count's."),
		acquireCount:    d("db_pool_acquires_total", "Successful connection acquires."),
		emptied:         d("db_pool_empty_acquires_total", "Acquires that had to wait because the pool had no idle connection."),
	}
}

// Describe implements prometheus.Collector.
func (c *PoolCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.acquired, c.idle, c.total, c.acquireDuration, c.acquireCount, c.emptied} {
		ch <- d
	}
}

// Collect implements prometheus.Collector.
func (c *PoolCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.pool.Stat()
	ch <- prometheus.MustNewConstMetric(c.acquired, prometheus.GaugeValue, float64(s.AcquiredConns()))
	ch <- prometheus.MustNewConstMetric(c.idle, prometheus.GaugeValue, float64(s.IdleConns()))
	ch <- prometheus.MustNewConstMetric(c.total, prometheus.GaugeValue, float64(s.TotalConns()))
	ch <- prometheus.MustNewConstMetric(c.acquireDuration, prometheus.CounterValue, s.AcquireDuration().Seconds())
	ch <- prometheus.MustNewConstMetric(c.acquireCount, prometheus.CounterValue, float64(s.AcquireCount()))
	ch <- prometheus.MustNewConstMetric(c.emptied, prometheus.CounterValue, float64(s.EmptyAcquireCount()))
}

// QueueCollector exposes jobs_queue_depth{state} from a cached count over the
// river_job table.
type QueueCollector struct {
	pool *pgxpool.Pool
	log  *slog.Logger
	desc *prometheus.Desc
	now  func() time.Time

	mu      sync.Mutex
	fetched time.Time
	depth   map[string]float64
}

// NewQueueCollector returns a collector counting jobs in pool's database.
func NewQueueCollector(pool *pgxpool.Pool, log *slog.Logger) *QueueCollector {
	return &QueueCollector{
		pool: pool, log: log, now: time.Now,
		desc: prometheus.NewDesc("jobs_queue_depth", "River jobs by state.", []string{"state"}, nil),
	}
}

// Describe implements prometheus.Collector.
func (c *QueueCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

// Collect implements prometheus.Collector. On a database error it serves the
// last known counts (or nothing before the first success) and logs.
func (c *QueueCollector) Collect(ch chan<- prometheus.Metric) {
	for state, n := range c.counts() {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, n, state)
	}
}

func (c *QueueCollector) counts() map[string]float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.depth != nil && c.now().Sub(c.fetched) < queueDepthTTL {
		return c.depth
	}
	// A scrape must not hang on a sick database.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rows, err := db.New(c.pool).CountJobsByState(ctx)
	if err != nil {
		c.log.Warn("job queue depth unavailable", slog.String("error", err.Error()))
		return c.depth
	}
	depth := make(map[string]float64, len(jobStates))
	for _, s := range jobStates {
		depth[s] = 0
	}
	for _, r := range rows {
		depth[r.State] = float64(r.Jobs)
	}
	c.depth, c.fetched = depth, c.now()
	return c.depth
}

// WatchJobs feeds jobs_completed_total and jobs_failed_total from River's
// event stream until ctx ends. Subscribe before the client starts, so no
// early event is missed; the returned stop function cancels the subscription.
func WatchJobs(ctx context.Context, client *jobs.Client) (stop func()) {
	events, cancel := client.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-events:
				if !ok {
					return
				}
				if ev == nil || ev.Job == nil {
					continue
				}
				switch ev.Kind {
				case river.EventKindJobCompleted:
					metrics.JobsCompleted.WithLabelValues(ev.Job.Kind).Inc()
				case river.EventKindJobFailed:
					metrics.JobsFailed.WithLabelValues(ev.Job.Kind).Inc()
				default:
				}
			}
		}
	}()
	return cancel
}

// RegisterDatabase registers the pool and queue collectors.
func RegisterDatabase(pool *pgxpool.Pool, log *slog.Logger) error {
	if err := metrics.Registry.Register(NewPoolCollector(pool)); err != nil {
		return err
	}
	return metrics.Registry.Register(NewQueueCollector(pool, log))
}
