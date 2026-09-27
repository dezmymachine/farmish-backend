// Package jobs wires River: a Postgres-native job queue in the same database,
// so jobs are enqueued in the same transaction as the state change that
// causes them (REDEVELOPMENT_PLAN.md §5).
//
// Each feature defines job args + a worker and registers them on a Registry;
// cmd/api builds one Client from it. Enqueue with client.InsertTx(ctx, tx,
// args, opts) inside the business transaction: if the transaction rolls back,
// the job never exists.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

// Defaults applied to every job unless its InsertOpts override them.
const (
	// DefaultMaxAttempts caps retries. With River's backoff (attempt^4
	// seconds: 1s, 16s, 81s, 4m16s, ...) 10 attempts span roughly 7 hours.
	DefaultMaxAttempts = 10
	// DefaultJobTimeout cancels a job's context if it runs longer.
	DefaultJobTimeout = time.Minute
	// DefaultMaxWorkers is the default queue's concurrency (JOBS_MAX_WORKERS).
	DefaultMaxWorkers = 10
)

// Client is the River client, bound to pgx transactions.
type Client = river.Client[pgx.Tx]

// Registry collects the workers and periodic jobs of every feature.
type Registry struct {
	workers  *river.Workers
	periodic []*river.PeriodicJob
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{workers: river.NewWorkers()}
}

// Register adds a worker for job args of type T.
func Register[T river.JobArgs](r *Registry, w river.Worker[T]) {
	river.AddWorker(r.workers, w)
}

// Every schedules newArgs to be enqueued at a fixed interval. With runOnStart
// the first job is enqueued as soon as this process becomes the leader. Only
// the elected leader schedules, so a job fires once per interval across all
// worker processes.
func (r *Registry) Every(interval time.Duration, newArgs func() river.JobArgs, runOnStart bool) {
	r.periodic = append(r.periodic, river.NewPeriodicJob(
		river.PeriodicInterval(interval),
		func() (river.JobArgs, *river.InsertOpts) { return newArgs(), nil },
		&river.PeriodicJobOpts{RunOnStart: runOnStart},
	))
}

// Schedule enqueues newArgs on a custom schedule, such as DailyAt. Only the
// elected leader schedules, like Every.
func (r *Registry) Schedule(schedule river.PeriodicSchedule, newArgs func() river.JobArgs, runOnStart bool) {
	r.periodic = append(r.periodic, river.NewPeriodicJob(
		schedule,
		func() (river.JobArgs, *river.InsertOpts) { return newArgs(), nil },
		&river.PeriodicJobOpts{RunOnStart: runOnStart},
	))
}

// DailyAt runs once a day at the given wall-clock time in Loc.
type DailyAt struct {
	Hour, Min int
	Loc       *time.Location
}

// Next returns the next occurrence after current.
func (d DailyAt) Next(current time.Time) time.Time {
	inLoc := current.In(d.Loc)
	next := time.Date(inLoc.Year(), inLoc.Month(), inLoc.Day(), d.Hour, d.Min, 0, 0, d.Loc)
	if !next.After(current) {
		next = next.Add(24 * time.Hour)
	}
	return next
}

// Options configures a Client.
type Options struct {
	// Work makes the client fetch and run jobs (RUN_MODE all|worker). When
	// false it is insert-only (RUN_MODE api).
	Work bool
	// MaxWorkers is the default queue's concurrency; 0 means DefaultMaxWorkers.
	MaxWorkers int
	// RetryPolicy overrides River's exponential backoff (tests use a fast one).
	RetryPolicy river.ClientRetryPolicy
	// FetchPollInterval overrides how often idle workers poll (tests only;
	// River also wakes on LISTEN/NOTIFY).
	FetchPollInterval time.Duration
}

// NewClient builds the River client over pool.
func NewClient(pool *pgxpool.Pool, reg *Registry, log *slog.Logger, opts Options) (*Client, error) {
	cfg := &river.Config{
		Logger:            log.With(slog.String("component", "river")),
		MaxAttempts:       DefaultMaxAttempts,
		JobTimeout:        DefaultJobTimeout,
		RetryPolicy:       opts.RetryPolicy,
		Workers:           reg.workers, // also validates job kinds on insert
		FetchPollInterval: opts.FetchPollInterval,
	}
	if opts.FetchPollInterval > 0 {
		cfg.FetchCooldown = min(opts.FetchPollInterval, river.FetchCooldownDefault)
	}
	if opts.Work {
		maxWorkers := opts.MaxWorkers
		if maxWorkers <= 0 {
			maxWorkers = DefaultMaxWorkers
		}
		cfg.Queues = map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: maxWorkers}}
		cfg.PeriodicJobs = reg.periodic
	}
	client, err := river.NewClient(riverpgxv5.New(pool), cfg)
	if err != nil {
		return nil, fmt.Errorf("river client: %w", err)
	}
	return client, nil
}

// Unique makes an insert a no-op while another job of the same kind with the
// same args exists (pending, scheduled, available, running, retryable or
// completed and not yet pruned). The result's UniqueSkippedAsDuplicate
// reports a skip.
//
// Job uniqueness is a guard against double enqueues, not the idempotency
// mechanism: completed jobs are pruned after a day, so money-moving work must
// also be idempotent in its own tables (unique references, state checks).
func Unique() *river.InsertOpts {
	return &river.InsertOpts{UniqueOpts: river.UniqueOpts{ByArgs: true}}
}

// UniqueWithin is Unique limited to a time window, e.g. at most one
// reconciliation per hour: UniqueWithin(time.Hour).
func UniqueWithin(period time.Duration) *river.InsertOpts {
	return &river.InsertOpts{UniqueOpts: river.UniqueOpts{ByArgs: true, ByPeriod: period}}
}

// Stop stops fetching new jobs and waits up to soft for running jobs to
// finish, then cancels their contexts and waits up to hard more.
func Stop(client *Client, soft, hard time.Duration, log *slog.Logger) error {
	softCtx, cancel := context.WithTimeout(context.Background(), soft)
	defer cancel()
	err := client.Stop(softCtx)
	if err == nil {
		return nil
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("river stop: %w", err)
	}
	log.Warn("jobs still running after soft stop timeout; cancelling them", slog.Duration("timeout", soft))
	hardCtx, cancelHard := context.WithTimeout(context.Background(), hard)
	defer cancelHard()
	if err := client.StopAndCancel(hardCtx); err != nil {
		return fmt.Errorf("river stop and cancel: %w", err)
	}
	return nil
}

// AccraLocation is Africa/Accra for daily schedules. Accra stays on UTC+0
// with no daylight saving, so the fixed zone is exact even where the tz
// database is missing; LoadLocation wins when it exists.
func AccraLocation() *time.Location {
	if loc, err := time.LoadLocation("Africa/Accra"); err == nil {
		return loc
	}
	return time.FixedZone("Africa/Accra", 0)
}
