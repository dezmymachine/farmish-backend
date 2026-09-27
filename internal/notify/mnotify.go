package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultTimeout bounds one mNotify call. An SMS gateway must never hold a
// worker open.
const DefaultTimeout = 10 * time.Second

// maxResponseBytes caps a gateway answer: megabytes mean the wrong endpoint.
const maxResponseBytes = 1 << 20

// ErrGatewayRejected means mNotify answered, but not with success.
var ErrGatewayRejected = errors.New("mnotify: message rejected")

// MNotify sends through mNotify's quick-send API. The endpoint and request
// shape were probed live on 2026-09-27 (see ADR-0026): an invalid-key request
// to api.mnotify.com/api/sms/quick returns a JSON envelope, which confirmed
// the method, path, key parameter and body fields.
type MNotify struct {
	apiKey  string
	sender  string
	baseURL string
	http    *http.Client
}

// NewMNotify returns a client. Empty baseURL uses mNotify's own host; tests
// point it at an httptest server.
func NewMNotify(apiKey, sender, baseURL string) *MNotify {
	if baseURL == "" {
		baseURL = "https://api.mnotify.com"
	}
	return &MNotify{
		apiKey: apiKey, sender: sender, baseURL: strings.TrimRight(baseURL, "/"),
		http: &http.Client{Timeout: DefaultTimeout},
	}
}

// Send delivers one message. mNotify expects a local Ghanaian number
// (0XXXXXXXXX) rather than E.164, so the conversion happens here and only
// here.
func (m *MNotify) Send(ctx context.Context, toE164, message string) error {
	endpoint := m.baseURL + "/api/sms/quick?key=" + url.QueryEscape(m.apiKey)
	body, err := json.Marshal(map[string]any{
		"recipient":     []string{localGhanaNumber(toE164)},
		"sender":        m.sender,
		"message":       message,
		"is_schedule":   false,
		"schedule_date": "",
	})
	if err != nil {
		return fmt.Errorf("mnotify: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("mnotify: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.http.Do(req)
	if err != nil {
		return fmt.Errorf("mnotify: send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("mnotify: read response: %w", err)
	}
	var envelope struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("mnotify: unreadable response (http %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK || envelope.Status != "success" {
		// The gateway's own message is safe to surface: it never echoes the
		// recipient's number.
		return fmt.Errorf("%w: http %d: %s", ErrGatewayRejected, resp.StatusCode, envelope.Message)
	}
	return nil
}

// localGhanaNumber converts +233XXXXXXXXX to 0XXXXXXXXX. Anything already
// local passes through; anything else is refused rather than guessed.
func localGhanaNumber(e164 string) string {
	if strings.HasPrefix(e164, "+233") {
		return "0" + strings.TrimPrefix(e164, "+233")
	}
	return e164
}
