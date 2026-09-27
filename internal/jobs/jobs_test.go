package jobs_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/jobs"
	"github.com/dezmymachine/farmish-backend/pkg/logger"
)

var discard = logger.New(io.Discard, "error")

// fastRetry retries failed jobs almost immediately so tests don't wait out
// River's exponential backoff.
type fastRetry struct{}

func (fastRetry) NextRetry(*rivertype.JobRow) time.Time { return time.Now().Add(10 * time.Millisecond) }

// countArgs/countWorker count runs per key.
type countArgs struct {
	Key string `json:"key"`
}

func (countArgs) Kind() string { return "test_count" }

type countWorker struct {
	river.WorkerDefaults[countArgs]
	mu   sync.Mutex
	runs map[string]int
}

func (w *countWorker) Work(_ context.Context, job *river.Job[countArgs]) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.runs[job.Args.Key]++
	return nil
}

func (w *countWorker) count(key string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.runs[key]
}

func newRegistry() (*jobs.Registry, *countWorker) {
	reg := jobs.NewRegistry()
	w := &countWorker{runs: map[string]int{}}
	jobs.Register(reg, w)
	return reg, w
}

func newClient(t *testing.T, pool *pgxpool.Pool, reg *jobs.Registry, work bool) *jobs.Client {
	t.Helper()
	c, err := jobs.NewClient(pool, reg, discard, jobs.Options{
		Work: work, RetryPolicy: fastRetry{}, FetchPollInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func start(t *testing.T, c *jobs.Client) {
	t.Helper()
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := jobs.Stop(c, 5*time.Second, 2*time.Second, discard); err != nil {
			t.Errorf("stop: %v", err)
		}
	})
}

// waitCompleted waits for n completion events of kind.
func waitCompleted(t *testing.T, events <-chan *river.Event, kind string, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for got := 0; got < n; {
		select {
		case e := <-events:
			if e.Kind == river.EventKindJobCompleted && e.Job.Kind == kind {
				got++
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %d completed %q jobs", n, kind)
		}
	}
}

func jobState(t *testing.T, pool *pgxpool.Pool, id int64) (state string, attempt int) {
	t.Helper()
	err := pool.QueryRow(context.Background(), `SELECT state::text, attempt FROM river_job WHERE id = $1`, id).Scan(&state, &attempt)
	if err != nil {
		t.Fatal(err)
	}
	return state, attempt
}

func countRows(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The core guarantee: a job enqueued in a transaction that rolls back never
// exists and never runs, together with the business write it belonged to.
func TestInsertTx_RolledBackNeverRuns(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `CREATE TABLE demo_orders (id int PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	reg, w := newRegistry()
	c := newClient(t, pool, reg, true)
	start(t, c)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO demo_orders VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.InsertTx(ctx, tx, countArgs{Key: "rolled-back"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	time.Sleep(time.Second) // ~20 poll intervals
	if n := w.count("rolled-back"); n != 0 {
		t.Errorf("job ran %d times after rollback", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM river_job`); n != 0 {
		t.Errorf("%d job rows after rollback", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM demo_orders`); n != 0 {
		t.Errorf("business row survived rollback")
	}
}

func TestInsertTx_CommittedRunsExactlyOnce(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `CREATE TABLE demo_orders (id int PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	reg, w := newRegistry()
	c := newClient(t, pool, reg, true)
	events, cancel := c.Subscribe(river.EventKindJobCompleted)
	defer cancel()
	start(t, c)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO demo_orders VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	res, err := c.InsertTx(ctx, tx, countArgs{Key: "committed"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	waitCompleted(t, events, "test_count", 1, 10*time.Second)
	time.Sleep(time.Second) // a duplicate run would show up here
	if n := w.count("committed"); n != 1 {
		t.Errorf("job ran %d times, want exactly 1", n)
	}
	if state, attempt := jobState(t, pool, res.Job.ID); state != "completed" || attempt != 1 {
		t.Errorf("job state %s, attempt %d", state, attempt)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM demo_orders`); n != 1 {
		t.Errorf("business row missing after commit")
	}
}

func TestNoopJob(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	reg := jobs.NewRegistry()
	jobs.Register(reg, &jobs.NoopWorker{Log: discard})
	c := newClient(t, pool, reg, true)
	events, cancel := c.Subscribe(river.EventKindJobCompleted)
	defer cancel()
	start(t, c)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.InsertTx(ctx, tx, jobs.NoopArgs{Note: "hello"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	waitCompleted(t, events, "noop", 1, 10*time.Second)
}

// RUN_MODE=api: an insert-only client enqueues but never works jobs; a worker
// process picks them up.
func TestInsertOnlyClient(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	reg, w := newRegistry()

	apiClient := newClient(t, pool, reg, false)
	res, err := apiClient.Insert(ctx, countArgs{Key: "split"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if state, _ := jobState(t, pool, res.Job.ID); state != "available" || w.count("split") != 0 {
		t.Fatalf("insert-only client worked the job: state %s", state)
	}

	worker := newClient(t, pool, reg, true)
	events, cancel := worker.Subscribe(river.EventKindJobCompleted)
	defer cancel()
	start(t, worker)
	waitCompleted(t, events, "test_count", 1, 10*time.Second)
	if n := w.count("split"); n != 1 {
		t.Errorf("ran %d times", n)
	}
}

type flakyArgs struct{}

func (flakyArgs) Kind() string { return "test_flaky" }

type flakyWorker struct {
	river.WorkerDefaults[flakyArgs]
	attempts atomic.Int32
}

func (w *flakyWorker) Work(context.Context, *river.Job[flakyArgs]) error {
	if w.attempts.Add(1) == 1 {
		return errors.New("transient failure")
	}
	return nil
}

func TestRetriesFailedJobs(t *testing.T) {
	pool := dbtest.Pool(t)
	reg := jobs.NewRegistry()
	w := &flakyWorker{}
	jobs.Register(reg, w)
	c := newClient(t, pool, reg, true)
	events, cancel := c.Subscribe(river.EventKindJobCompleted)
	defer cancel()
	start(t, c)

	res, err := c.Insert(context.Background(), flakyArgs{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Job.MaxAttempts != jobs.DefaultMaxAttempts {
		t.Errorf("max attempts = %d, want default %d", res.Job.MaxAttempts, jobs.DefaultMaxAttempts)
	}
	waitCompleted(t, events, "test_flaky", 1, 20*time.Second)
	state, attempt := jobState(t, pool, res.Job.ID)
	if state != "completed" || attempt != 2 || w.attempts.Load() != 2 {
		t.Errorf("state %s, attempt %d, worker attempts %d", state, attempt, w.attempts.Load())
	}
}

func TestUnique(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	reg, _ := newRegistry()
	c := newClient(t, pool, reg, false)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	first, err := c.InsertTx(ctx, tx, countArgs{Key: "ref-123"}, jobs.Unique())
	if err != nil || first.UniqueSkippedAsDuplicate {
		t.Fatalf("first: %+v, err %v", first, err)
	}
	dup, err := c.InsertTx(ctx, tx, countArgs{Key: "ref-123"}, jobs.Unique())
	if err != nil || !dup.UniqueSkippedAsDuplicate || dup.Job.ID != first.Job.ID {
		t.Errorf("duplicate not skipped: %+v, err %v", dup, err)
	}
	other, err := c.InsertTx(ctx, tx, countArgs{Key: "ref-456"}, jobs.Unique())
	if err != nil || other.UniqueSkippedAsDuplicate {
		t.Errorf("different args skipped: %+v, err %v", other, err)
	}
	windowed, err := c.InsertTx(ctx, tx, countArgs{Key: "ref-789"}, jobs.UniqueWithin(time.Hour))
	if err != nil || windowed.UniqueSkippedAsDuplicate {
		t.Fatalf("windowed: %+v, err %v", windowed, err)
	}
	if again, _ := c.InsertTx(ctx, tx, countArgs{Key: "ref-789"}, jobs.UniqueWithin(time.Hour)); !again.UniqueSkippedAsDuplicate {
		t.Error("windowed duplicate not skipped")
	}
}

type tickArgs struct{}

func (tickArgs) Kind() string { return "test_tick" }

type tickWorker struct {
	river.WorkerDefaults[tickArgs]
}

func (tickWorker) Work(context.Context, *river.Job[tickArgs]) error { return nil }

func TestPeriodicJobFires(t *testing.T) {
	pool := dbtest.Pool(t)
	reg := jobs.NewRegistry()
	jobs.Register(reg, tickWorker{})
	reg.Every(500*time.Millisecond, func() river.JobArgs { return tickArgs{} }, true)
	c := newClient(t, pool, reg, true)
	events, cancel := c.Subscribe(river.EventKindJobCompleted)
	defer cancel()
	start(t, c)

	// Once on start (after leader election), then again on the interval.
	waitCompleted(t, events, "test_tick", 2, 30*time.Second)
}

type blockArgs struct{}

func (blockArgs) Kind() string { return "test_block" }

type blockWorker struct {
	river.WorkerDefaults[blockArgs]
	started   chan struct{}
	cancelled chan struct{}
}

func (w *blockWorker) Work(ctx context.Context, _ *river.Job[blockArgs]) error {
	close(w.started)
	<-ctx.Done()
	close(w.cancelled)
	return ctx.Err()
}

// Shutdown waits for running jobs up to the soft timeout, then cancels them.
func TestStop_CancelsJobsAfterSoftTimeout(t *testing.T) {
	pool := dbtest.Pool(t)
	reg := jobs.NewRegistry()
	w := &blockWorker{started: make(chan struct{}), cancelled: make(chan struct{})}
	jobs.Register(reg, w)
	c := newClient(t, pool, reg, true)
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Insert(context.Background(), blockArgs{}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-w.started:
	case <-time.After(10 * time.Second):
		t.Fatal("job never started")
	}

	begin := time.Now()
	if err := jobs.Stop(c, 300*time.Millisecond, 5*time.Second, discard); err != nil {
		t.Fatalf("stop: %v", err)
	}
	select {
	case <-w.cancelled:
	default:
		t.Error("running job's context was not cancelled")
	}
	if d := time.Since(begin); d > 5*time.Second {
		t.Errorf("stop took %v", d)
	}
}

// TestDailyAt_Next proves the daily schedule fires at the wall-clock time:
// later the same day before the hour, the next day after it.
func TestDailyAt_Next(t *testing.T) {
	accra := time.FixedZone("Africa/Accra", 0)
	schedule := jobs.DailyAt{Hour: 3, Min: 0, Loc: accra}

	before := time.Date(2026, 9, 27, 2, 0, 0, 0, accra)
	if next := schedule.Next(before); next != time.Date(2026, 9, 27, 3, 0, 0, 0, accra) {
		t.Errorf("before the hour: next = %v, want 03:00 the same day", next)
	}
	after := time.Date(2026, 9, 27, 4, 0, 0, 0, accra)
	if next := schedule.Next(after); next != time.Date(2026, 9, 28, 3, 0, 0, 0, accra) {
		t.Errorf("after the hour: next = %v, want 03:00 the next day", next)
	}
	exact := time.Date(2026, 9, 27, 3, 0, 0, 0, accra)
	if next := schedule.Next(exact); next != time.Date(2026, 9, 28, 3, 0, 0, 0, accra) {
		t.Errorf("on the hour: next = %v, want 03:00 the next day", next)
	}
}
