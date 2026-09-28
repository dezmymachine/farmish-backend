package notify_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/riverqueue/river"

	"github.com/dezmymachine/farmish-backend/internal/metrics"
	"github.com/dezmymachine/farmish-backend/internal/notify"
)

func balanceServer(t *testing.T, status int, contentType, body string, seen *http.Request) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			*seen = *r.Clone(context.Background())
		}
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestMNotify_Balance(t *testing.T) {
	var seen http.Request
	for name, tt := range map[string]struct {
		status int
		ctype  string
		body   string
		want   float64
		fail   bool
	}{
		"number":            {200, "application/json", `{"status":"success","balance":1240,"bonus":"0"}`, 1240, false},
		"numeric string":    {200, "application/json", `{"status":"success","balance":"37.5"}`, 37.5, false},
		"zero is a balance": {200, "application/json", `{"status":"success","balance":0}`, 0, false},
		"invalid key":       {401, "application/json", `{"error":"invalid api key. please make sure your api key is valid and enabled"}`, 0, true},
		"html error page":   {200, "text/html", `<!DOCTYPE html><html></html>`, 0, true},
		"no balance field":  {200, "application/json", `{"status":"success"}`, 0, true},
		"non numeric":       {200, "application/json", `{"status":"success","balance":"lots"}`, 0, true},
		"negative":          {200, "application/json", `{"status":"success","balance":-5}`, 0, true},
		"server error":      {500, "application/json", `{"message":"boom"}`, 0, true},
	} {
		t.Run(name, func(t *testing.T) {
			server := balanceServer(t, tt.status, tt.ctype, tt.body, &seen)
			got, err := notify.NewMNotify("secret-key", "FARMISH", server.URL).Balance(context.Background())
			if tt.fail {
				if !errors.Is(err, notify.ErrBalanceUnavailable) {
					t.Fatalf("err = %v, want ErrBalanceUnavailable", err)
				}
				if strings.Contains(err.Error(), "secret-key") {
					t.Errorf("error leaks the API key: %v", err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("balance = %v, %v; want %v", got, err, tt.want)
			}
			if seen.Method != http.MethodGet || seen.URL.Path != "/api/balance/sms" || seen.URL.Query().Get("key") != "secret-key" {
				t.Errorf("request = %s %s key=%q, want GET /api/balance/sms key=secret-key",
					seen.Method, seen.URL.Path, seen.URL.Query().Get("key"))
			}
		})
	}
}

// TestMNotify_TransportErrorsDoNotLeakTheKey: the key rides in the query
// string, and net/http prints the URL in its errors.
func TestMNotify_TransportErrorsDoNotLeakTheKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL
	server.Close()
	client := notify.NewMNotify("secret-key", "FARMISH", url)

	_, balanceErr := client.Balance(context.Background())
	sendErr := client.Send(context.Background(), "+233241234567", "x")
	for name, err := range map[string]error{"balance": balanceErr, "send": sendErr} {
		if err == nil {
			t.Fatalf("%s: an unreachable gateway reported success", name)
		}
		if strings.Contains(err.Error(), "secret-key") || strings.Contains(err.Error(), "key=") {
			t.Errorf("%s: error carries the request URL: %v", name, err)
		}
	}
}

type fakeChecker struct {
	balance float64
	err     error
}

func (f fakeChecker) Balance(context.Context) (float64, error) { return f.balance, f.err }

func TestCheckBalanceWorker(t *testing.T) {
	work := func(c notify.BalanceChecker) (string, error) {
		var buf bytes.Buffer
		w := &notify.CheckBalanceWorker{
			Checker: c, LowBelow: 200,
			Log: slog.New(slog.NewJSONHandler(&buf, nil)),
		}
		err := w.Work(context.Background(), &river.Job[notify.CheckBalanceArgs]{})
		return buf.String(), err
	}

	t.Run("healthy balance publishes the gauge and stays quiet", func(t *testing.T) {
		out, err := work(fakeChecker{balance: 500})
		if err != nil {
			t.Fatal(err)
		}
		if got := testutil.ToFloat64(metrics.SMSBalance); got != 500 {
			t.Errorf("mnotify_balance = %v, want 500", got)
		}
		if strings.Contains(out, `"level":"ERROR"`) {
			t.Errorf("a healthy balance logged an error: %s", out)
		}
	})

	t.Run("low balance logs an Error naming the threshold", func(t *testing.T) {
		out, err := work(fakeChecker{balance: 150})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, `"level":"ERROR"`) || !strings.Contains(out, "balance is low") ||
			!strings.Contains(out, `"balance":150`) || !strings.Contains(out, `"threshold":200`) {
			t.Errorf("low balance log = %s", out)
		}
		if got := testutil.ToFloat64(metrics.SMSBalance); got != 150 {
			t.Errorf("mnotify_balance = %v, want 150", got)
		}
	})

	t.Run("exactly the threshold is not low", func(t *testing.T) {
		out, err := work(fakeChecker{balance: 200})
		if err != nil || strings.Contains(out, `"level":"ERROR"`) {
			t.Errorf("at threshold: %v, %s", err, out)
		}
	})

	t.Run("a gateway failure is returned so River retries", func(t *testing.T) {
		if _, err := work(fakeChecker{err: notify.ErrBalanceUnavailable}); !errors.Is(err, notify.ErrBalanceUnavailable) {
			t.Errorf("err = %v, want it returned", err)
		}
	})

	t.Run("an unwired worker fails loudly", func(t *testing.T) {
		w := &notify.CheckBalanceWorker{Log: slog.New(slog.DiscardHandler)}
		if err := w.Work(context.Background(), &river.Job[notify.CheckBalanceArgs]{}); err == nil {
			t.Error("a worker with no checker reported success")
		}
	})
}
