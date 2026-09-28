package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/riverqueue/river"

	"github.com/dezmymachine/farmish-backend/internal/jobs"
	"github.com/dezmymachine/farmish-backend/internal/metrics"
)

// ErrBalanceUnavailable means mNotify answered, but not with a readable
// balance (a bad key, a changed response shape).
var ErrBalanceUnavailable = errors.New("mnotify: balance unavailable")

// Balance returns the SMS credits left on the account.
//
// The endpoint is GET /api/balance/sms?key=<key>. It was probed live on
// 2026-09-28 (ADR-0034): with an invalid key it answers 401 with a JSON
// {"error": ...} envelope, where a wrong path returns an HTML page, which
// confirms the route and the key parameter. The success body is decoded
// tolerantly (a JSON number or a numeric string under "balance"), because no
// live key was available to pin its exact shape; the test pins both.
func (m *MNotify) Balance(ctx context.Context) (float64, error) {
	endpoint := m.baseURL + "/api/balance/sms?key=" + url.QueryEscape(m.apiKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, fmt.Errorf("mnotify: build balance request: %w", err)
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("mnotify: balance: %w", withoutURL(err))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return 0, fmt.Errorf("mnotify: read balance: %w", err)
	}
	var envelope struct {
		Status  string          `json:"status"`
		Error   string          `json:"error"`
		Message string          `json:"message"`
		Balance json.RawMessage `json:"balance"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return 0, fmt.Errorf("%w: unreadable response (http %d)", ErrBalanceUnavailable, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		// The gateway's message names the problem ("invalid api key") and
		// never echoes the key.
		return 0, fmt.Errorf("%w: http %d: %s", ErrBalanceUnavailable, resp.StatusCode, firstNonEmpty(envelope.Error, envelope.Message))
	}
	balance, ok := parseBalance(envelope.Balance)
	if !ok {
		return 0, fmt.Errorf("%w: response carried no numeric balance", ErrBalanceUnavailable)
	}
	return balance, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return "no message"
}

// parseBalance reads a JSON number or a numeric string.
func parseBalance(raw json.RawMessage) (float64, bool) {
	text := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if text == "" || text == "null" {
		return 0, false
	}
	v, err := strconv.ParseFloat(text, 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return v, true
}

// BalanceChecker reads the remaining SMS credits. *MNotify implements it.
type BalanceChecker interface {
	Balance(ctx context.Context) (float64, error)
}

// CheckBalanceArgs is the daily balance check.
type CheckBalanceArgs struct{}

// Kind implements river.JobArgs.
func (CheckBalanceArgs) Kind() string { return "notify.check_balance" }

// CheckBalanceWorker reads the balance, publishes it as mnotify_balance and
// logs an Error when it has fallen below LowBelow: that log line and the
// gauge are the two signals docs/runbooks/alerts.md alerts on. A gateway
// failure returns an error, so River retries and jobs_failed_total counts it.
type CheckBalanceWorker struct {
	river.WorkerDefaults[CheckBalanceArgs]
	Checker  BalanceChecker
	LowBelow float64
	Log      *slog.Logger
}

// Work implements river.Worker.
func (w *CheckBalanceWorker) Work(ctx context.Context, _ *river.Job[CheckBalanceArgs]) error {
	if w.Checker == nil {
		return errors.New("notify.check_balance worker is not wired")
	}
	balance, err := w.Checker.Balance(ctx)
	if err != nil {
		return err
	}
	metrics.SMSBalance.Set(balance)
	if balance < w.LowBelow {
		w.Log.Error("mnotify balance is low; top up to keep SMS flowing",
			slog.Float64("balance", balance), slog.Float64("threshold", w.LowBelow))
		return nil
	}
	w.Log.Info("mnotify balance checked", slog.Float64("balance", balance))
	return nil
}

// RegisterBalanceCheck adds the worker and a daily 08:00 Africa/Accra run.
// The first run waits for 08:00 rather than firing at every deploy.
func RegisterBalanceCheck(r *jobs.Registry, w *CheckBalanceWorker) {
	jobs.Register(r, w)
	r.Schedule(jobs.DailyAt{Hour: 8, Min: 0, Loc: jobs.AccraLocation()}, func() river.JobArgs { return CheckBalanceArgs{} }, false)
}
