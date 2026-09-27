package notify_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/dezmymachine/farmish-backend/internal/notify"
)

func TestMNotify_Client(t *testing.T) {
	type request struct {
		method string
		path   string
		key    string
		body   map[string]any
	}
	var got request
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		got = request{method: r.Method, path: r.URL.Path, key: r.URL.Query().Get("key"), body: body}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "message": "Sent"})
	}))
	defer server.Close()

	client := notify.NewMNotify("secret-key", "FARMISH", server.URL)
	if err := client.Send(context.Background(), "+233241234567", "Farmish: your order is on its way."); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got.method != http.MethodPost || got.path != "/api/sms/quick" || got.key != "secret-key" {
		t.Errorf("request = %s %s key=%q, want POST /api/sms/quick key=secret-key", got.method, got.path, got.key)
	}
	// mNotify expects the local format, not E.164.
	recipients, _ := got.body["recipient"].([]any)
	if len(recipients) != 1 || recipients[0] != "0241234567" {
		t.Errorf("recipient = %v, want [\"0241234567\"]", got.body["recipient"])
	}
	if got.body["sender"] != "FARMISH" || got.body["is_schedule"] != false {
		t.Errorf("body = %v", got.body)
	}
	if got.body["message"] != "Farmish: your order is on its way." {
		t.Errorf("message = %v", got.body["message"])
	}
}

func TestMNotify_Rejections(t *testing.T) {
	t.Run("gateway answers failure", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "message": "Insufficient balance"})
		}))
		defer server.Close()
		err := notify.NewMNotify("k", "FARMISH", server.URL).Send(context.Background(), "+233241234567", "x")
		if !errors.Is(err, notify.ErrGatewayRejected) || !strings.Contains(err.Error(), "Insufficient balance") {
			t.Errorf("err = %v, want the gateway's message wrapped in ErrGatewayRejected", err)
		}
	})
	t.Run("non-2xx without json", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("proxy on fire"))
		}))
		defer server.Close()
		err := notify.NewMNotify("k", "FARMISH", server.URL).Send(context.Background(), "+233241234567", "x")
		if err == nil || strings.Contains(err.Error(), "+233241234567") {
			t.Errorf("err = %v, want a failure without the number", err)
		}
	})
	t.Run("unreachable gateway", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := server.URL
		server.Close()
		err := notify.NewMNotify("k", "FARMISH", url).Send(context.Background(), "+233241234567", "x")
		if err == nil {
			t.Error("an unreachable gateway reported success")
		}
	})
}

func TestRender(t *testing.T) {
	for name, tt := range map[string]struct {
		template string
		params   map[string]string
		want     string
		wantErr  bool
	}{
		"paid seller": {
			notify.TemplateOrderPaidSeller,
			map[string]string{"orderIdShort": "ab12cd"},
			"Farmish: you have a new paid order ab12cd. Accept it within 48 hours or it is cancelled and refunded.",
			false,
		},
		"delivered buyer": {
			notify.TemplateOrderDeliveredBuyer,
			map[string]string{"orderIdShort": "ab12cd"},
			"Farmish: order ab12cd was delivered. Confirm receipt within 3 days or open a dispute.",
			false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := notify.Render(tt.template, tt.params)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Render(%s) = %q, want an error", tt.template, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("Render = %q, want %q", got, tt.want)
			}
			if len(got) > notify.MaxMessageLen {
				t.Errorf("message is %d characters, over the single-SMS limit", len(got))
			}
		})
	}
	// Every template renders inside the SMS limit with typical ids.
	for _, name := range []string{
		notify.TemplateOrderPaidSeller, notify.TemplateOrderAcceptedBuyer,
		notify.TemplateOrderRejectedBuyer, notify.TemplateOrderShippedBuyer,
		notify.TemplateOrderDeliveredBuyer, notify.TemplateOrderCompletedSeller,
		notify.TemplateOrderCancelledBuyer, notify.TemplateOrderCancelledSeller,
		notify.TemplateOrderDisputedSeller,
	} {
		got, err := notify.Render(name, map[string]string{"orderIdShort": "a1b2c3d4"})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(got) == 0 || len(got) > notify.MaxMessageLen {
			t.Errorf("%s = %d characters, want 1..%d", name, len(got), notify.MaxMessageLen)
		}
	}
	if _, err := notify.Render("no-such-template", nil); !errors.Is(err, notify.ErrUnknownTemplate) {
		t.Errorf("unknown template err = %v, want ErrUnknownTemplate", err)
	}
}

// captureLog keeps log lines for assertions without a buffer global.
type captureHandler struct {
	mu    sync.Mutex
	lines []string
}

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lines = append(h.lines, r.Message)
	return nil
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *captureHandler) WithGroup(string) slog.Handler            { return h }

func TestLogOnly_MasksNumber(t *testing.T) {
	handler := &captureHandler{}
	sender := notify.LogOnly{Log: slog.New(handler)}
	if err := sender.Send(context.Background(), "+233241234567", "Farmish: hi"); err != nil {
		t.Fatal(err)
	}
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if len(handler.lines) != 1 {
		t.Fatalf("log lines = %v", handler.lines)
	}
	// The full number must never appear.
	if strings.Contains(strings.Join(handler.lines, "\n"), "+233241234567") {
		t.Error("LogOnly wrote the unmasked number")
	}
}
