// Package metrics defines the service's Prometheus metrics (Phase 21) and
// serves them on a private listener.
//
// The collectors are package-level so a domain package records an event with
// one line (metrics.PayoutsTotal.WithLabelValues("success").Inc()) instead of
// threading a recorder through every constructor. They live on their own
// registry, not the default one, so /metrics exposes exactly this list plus
// the Go runtime and process collectors.
//
// Label values are always drawn from a fixed set (route templates, status
// enums, rule names): a user-controlled value must never become a label.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Registry is the registry /metrics serves.
var Registry = prometheus.NewRegistry()

// Statuses recorded by the money counters. They mirror the domain statuses.
const (
	StatusQueued    = "queued"
	StatusSucceeded = "succeeded"
	StatusProcessed = "processed"
	StatusFailed    = "failed"
	StatusAbandoned = "abandoned"
	StatusReversed  = "reversed"
)

var (
	// HTTPRequests counts finished requests by route template, never raw path.
	HTTPRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "HTTP requests by method, route template and status code.",
	}, []string{"method", "route", "status"})

	// HTTPDuration is the request latency histogram.
	HTTPDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "HTTP request latency by method and route template.",
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
	}, []string{"method", "route"})

	// JobsCompleted and JobsFailed come from River's event stream. A failure
	// is one failed attempt: the job may still retry.
	JobsCompleted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "jobs_completed_total",
		Help: "River jobs completed successfully, by kind.",
	}, []string{"kind"})
	JobsFailed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "jobs_failed_total",
		Help: "River job attempts that failed, by kind.",
	}, []string{"kind"})

	// PaymentsTotal counts payments reaching a terminal status.
	PaymentsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "payments_total",
		Help: "Payments reaching a terminal status, by purpose and status.",
	}, []string{"purpose", "status"})

	// PaymentAmountMismatch counts webhooks whose charge did not match the
	// payment. Any increase is an alert.
	PaymentAmountMismatch = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "payment_amount_mismatch_total",
		Help: "Charge webhooks rejected for an amount or currency mismatch.",
	})

	// PayoutsTotal counts payout state changes; PayoutFailures counts the
	// failed and reversed ones on their own, for a one-line alert.
	PayoutsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "payouts_total",
		Help: "Payout state changes, by status.",
	}, []string{"status"})
	PayoutFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "payout_failures_total",
		Help: "Payouts that failed or were reversed.",
	})

	// RefundsTotal counts refund state changes.
	RefundsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "refunds_total",
		Help: "Refund state changes, by status.",
	}, []string{"status"})

	// LedgerReconcileViolations is the violation count of the latest
	// reconciliation run. Anything above zero is an alert.
	LedgerReconcileViolations = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "ledger_reconcile_violations",
		Help: "Violations found by the latest ledger reconciliation run.",
	})

	// SMSBalance is the mNotify SMS credit balance from the daily check. It
	// stays unset (absent from a scrape) until the first check has run.
	SMSBalance = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "mnotify_balance",
		Help: "SMS credits left on the mNotify account, from the daily check.",
	})

	// RateLimitDenied counts requests refused by a rate-limit rule.
	RateLimitDenied = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ratelimit_denied_total",
		Help: "Requests refused by a rate-limit rule.",
	}, []string{"rule"})

	// RateLimitFallback counts checks answered by the in-process limiter
	// because the shared (Redis) limiter failed.
	RateLimitFallback = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "ratelimit_fallback_total",
		Help: "Rate-limit checks answered by the in-process fallback.",
	})
)

func init() {
	Registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		HTTPRequests, HTTPDuration,
		JobsCompleted, JobsFailed,
		PaymentsTotal, PaymentAmountMismatch,
		PayoutsTotal, PayoutFailures, RefundsTotal,
		LedgerReconcileViolations, SMSBalance,
		RateLimitDenied, RateLimitFallback,
	)
	// Counters without an event yet still need a zero sample: an alert on
	// increase() and a dashboard both need the series to exist before the
	// first incident.
	PaymentAmountMismatch.Add(0)
	PayoutFailures.Add(0)
	RateLimitFallback.Add(0)
	LedgerReconcileViolations.Set(0)
}
