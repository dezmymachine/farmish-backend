package httpapi

import (
	"bytes"
	"io"
	"log/slog"
	"sync"
	"testing"
)

// testLogOut, when set, receives every log line the fixtures produce. It is
// nil (discard) except inside captureLogs. Tests in this package don't run in
// parallel, so a package variable is safe.
var testLogOut io.Writer

// syncBuffer is a bytes.Buffer that job workers and the request goroutines
// can write to concurrently.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// captureLogs routes the fixtures' logs into a buffer for the rest of the
// test. The buffer receives the raw JSON handler's output, with no redaction
// in between, so it proves the call sites themselves are clean rather than
// relying on the redacting handler to hide a leak.
func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	testLogOut = buf
	t.Cleanup(func() { testLogOut = nil })
	return buf
}

// testLogger is the logger fixtures hand to services: discarded normally,
// the capture buffer inside captureLogs.
func testLogger() *slog.Logger {
	if testLogOut == nil {
		return slog.New(slog.DiscardHandler)
	}
	return slog.New(slog.NewJSONHandler(testLogOut, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
