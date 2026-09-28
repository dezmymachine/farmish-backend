package redact_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/dezmymachine/farmish-backend/pkg/redact"
)

const testJWT = "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.c2lnbmF0dXJl"

func TestMasks(t *testing.T) {
	for _, tc := range []struct{ name, got, want string }{
		{"phone", redact.Phone("+233241234567"), "+233*****4567"},
		{"short phone", redact.Phone("123"), "****"},
		{"account", redact.AccountNumber("0123456789"), "******6789"},
		{"short account", redact.AccountNumber("123"), "***"},
		{"email", redact.Email("ama.mensah@example.com"), "a***@example.com"},
		{"not an email", redact.Email("nope"), redact.Placeholder},
		{"empty email", redact.Email(""), redact.Placeholder},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

func TestString_Patterns(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"empty", "", ""},
		{"plain", "order paid", "order paid"},
		{"phone in text", "sms to +233241234567 failed", "sms to +233*****4567 failed"},
		{"jwt", "token " + testJWT + " rejected", "token [redacted-jwt] rejected"},
		{"bearer", "Authorization: Bearer abc.def-ghi", "Authorization: Bearer [redacted]"},
		{"lowercase bearer", "got bearer abc123", "got Bearer [redacted]"},
		{"foreign number untouched", "+441234567890", "+441234567890"},
	} {
		if got := redact.String(tc.in); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func logLine(t *testing.T, fn func(l *slog.Logger)) (map[string]any, string) {
	t.Helper()
	var buf bytes.Buffer
	fn(slog.New(redact.NewHandler(slog.NewJSONHandler(&buf, nil))))
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("want one JSON line, got %q: %v", buf.String(), err)
	}
	return rec, buf.String()
}

func TestHandler_RedactsByKey(t *testing.T) {
	rec, _ := logLine(t, func(l *slog.Logger) {
		l.Info("x",
			"phone", "+233241234567", "phone_e164", "+233241234567", "account_number", "0123456789",
			"token", "abc", "Authorization", "Bearer abc", "id_number", "GHA-123", "email", "a@b.co",
			"secret", "s", "reference", "ref-1")
	})
	for _, k := range []string{"phone", "phone_e164", "account_number", "token", "Authorization", "id_number", "email", "secret"} {
		if rec[k] != redact.Placeholder {
			t.Errorf("%s = %v, want %q", k, rec[k], redact.Placeholder)
		}
	}
	if rec["reference"] != "ref-1" {
		t.Errorf("reference = %v, want untouched", rec["reference"])
	}
}

func TestHandler_RedactsByValue(t *testing.T) {
	rec, line := logLine(t, func(l *slog.Logger) {
		l.Warn("call for +233241234567",
			"note", "user +233241234567 sent "+testJWT, "err", errors.New("boom "+testJWT))
	})
	if strings.Contains(line, "241234567") || strings.Contains(line, "eyJ") {
		t.Errorf("secret survived: %s", line)
	}
	if rec["msg"] != "call for +233*****4567" {
		t.Errorf("msg = %v", rec["msg"])
	}
	if rec["err"] != "boom [redacted-jwt]" {
		t.Errorf("err = %v", rec["err"])
	}
}

func TestHandler_GroupsAndWith(t *testing.T) {
	_, line := logLine(t, func(l *slog.Logger) {
		l.With("phone", "+233241234567").WithGroup("req").Info("x",
			slog.Group("user", slog.String("email", "a@b.co"), slog.String("n", "+233241234567")),
			slog.String("token", "t"))
	})
	for _, leak := range []string{"241234567", "a@b.co", `"t"`} {
		if strings.Contains(line, leak) {
			t.Errorf("%q leaked: %s", leak, line)
		}
	}
}

func TestHandler_StringerValue(t *testing.T) {
	_, line := logLine(t, func(l *slog.Logger) { l.Info("x", "who", fmt.Stringer(phoneStringer{})) })
	if strings.Contains(line, "241234567") {
		t.Errorf("stringer leaked: %s", line)
	}
}

type phoneStringer struct{}

func (phoneStringer) String() string { return "+233241234567" }
