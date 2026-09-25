package httpapi

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/dezmymachine/farmish-backend/pkg/logger"
)

// Cancelling the context (as SIGTERM does via signal.NotifyContext) must let an
// in-flight request finish before Serve returns cleanly.
func TestServe_GracefulShutdown(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "done")
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		served <- Serve(ctx, NewServer(ln.Addr().String(), h), ln, 5*time.Second, logger.New(&bytes.Buffer{}, "error"))
	}()

	type result struct {
		body string
		err  error
	}
	resCh := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String())
		if err != nil {
			resCh <- result{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		resCh <- result{string(b), err}
	}()

	<-started
	cancel()
	select {
	case err := <-served:
		t.Fatalf("Serve returned before in-flight request finished: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)

	if res := <-resCh; res.err != nil || res.body != "done" {
		t.Fatalf("in-flight request: body %q, err %v", res.body, res.err)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after shutdown")
	}
}
