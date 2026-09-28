package logger

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestNew_WritesJSONAtLevel(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, "warn")
	l.Info("dropped")
	l.Warn("kept", "k", "v")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("want exactly one JSON line, got %q: %v", buf.String(), err)
	}
	if rec["msg"] != "kept" || rec["k"] != "v" || rec["level"] != "WARN" {
		t.Errorf("unexpected record: %v", rec)
	}
}

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{
		"debug": slog.LevelDebug, "INFO": slog.LevelInfo, "warn": slog.LevelWarn,
		"error": slog.LevelError, "": slog.LevelInfo, "nope": slog.LevelInfo,
	} {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestContextRoundTrip(t *testing.T) {
	if FromContext(context.Background()) != slog.Default() {
		t.Error("empty context should yield slog.Default()")
	}
	l := New(&bytes.Buffer{}, "info")
	if FromContext(WithContext(context.Background(), l)) != l {
		t.Error("logger not recovered from context")
	}
}

func TestNew_RedactsSecrets(t *testing.T) {
	var buf bytes.Buffer
	New(&buf, "info").Info("sign-in", "phone", "+233241234567", "note", "call +233241234567")
	if s := buf.String(); strings.Contains(s, "241234567") {
		t.Errorf("phone leaked into log: %s", s)
	}
}
