package turnstile

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// fakeSiteverify answers like Cloudflare and records the last form posted.
func fakeSiteverify(t *testing.T, status int, body string) (*Client, *http.Request) {
	t.Helper()
	var got http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		got = *r
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &Client{Secret: "sekret", URL: srv.URL, HTTP: srv.Client()}, &got
}

func TestVerify_Success(t *testing.T) {
	c, got := fakeSiteverify(t, http.StatusOK, `{"success":true}`)
	if err := c.Verify(context.Background(), "tok", netip.MustParseAddr("196.201.214.10")); err != nil {
		t.Fatal(err)
	}
	if got.Method != http.MethodPost || got.PostForm.Get("secret") != "sekret" ||
		got.PostForm.Get("response") != "tok" || got.PostForm.Get("remoteip") != "196.201.214.10" {
		t.Errorf("posted %s %v", got.Method, got.PostForm)
	}
}

func TestVerify_Rejected(t *testing.T) {
	for name, body := range map[string]string{
		"invalid":   `{"success":false,"error-codes":["invalid-input-response"]}`,
		"duplicate": `{"success":false,"error-codes":["timeout-or-duplicate"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := fakeSiteverify(t, http.StatusOK, body)
			if err := c.Verify(context.Background(), "tok", netip.Addr{}); !errors.Is(err, ErrFailed) {
				t.Errorf("err = %v, want ErrFailed", err)
			}
		})
	}
	c, _ := fakeSiteverify(t, http.StatusOK, `{"success":true}`)
	for _, tok := range []string{"", "   ", strings.Repeat("x", 2049)} {
		if err := c.Verify(context.Background(), tok, netip.Addr{}); !errors.Is(err, ErrFailed) {
			t.Errorf("token %q: err = %v", tok[:min(len(tok), 8)], err)
		}
	}
}

// Our misconfiguration or Cloudflare trouble is not the client's fault:
// these must NOT be ErrFailed (callers fail closed with 503).
func TestVerify_Unavailable(t *testing.T) {
	cases := map[string]*Client{}
	c1, _ := fakeSiteverify(t, http.StatusOK, `{"success":false,"error-codes":["invalid-input-secret"]}`)
	cases["bad secret"] = c1
	c2, _ := fakeSiteverify(t, http.StatusInternalServerError, `oops`)
	cases["server error"] = c2
	c3, _ := fakeSiteverify(t, http.StatusOK, `not json`)
	cases["bad json"] = c3
	cases["unreachable"] = &Client{Secret: "s", URL: "http://127.0.0.1:1", HTTP: &http.Client{Timeout: time.Second}}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := c.Verify(context.Background(), "tok", netip.Addr{})
			if err == nil || errors.Is(err, ErrFailed) {
				t.Errorf("err = %v, want a non-ErrFailed error", err)
			}
		})
	}
}

// Live check against Cloudflare with its published test secrets (needs
// network, like govulncheck; skipped with -short).
func TestVerify_CloudflareTestSecrets(t *testing.T) {
	if testing.Short() {
		t.Skip("network test")
	}
	ctx := context.Background()
	const dummyToken = "XXXX.DUMMY.TOKEN.XXXX"
	if err := New("1x0000000000000000000000000000000AA").Verify(ctx, dummyToken, netip.Addr{}); err != nil {
		t.Errorf("always-pass secret: %v", err)
	}
	if err := New("2x0000000000000000000000000000000AA").Verify(ctx, dummyToken, netip.Addr{}); !errors.Is(err, ErrFailed) {
		t.Errorf("always-fail secret: %v", err)
	}
}

func TestIsTestSecret(t *testing.T) {
	if !IsTestSecret("1x0000000000000000000000000000000AA") || IsTestSecret("0x4AAAAAAA-real") {
		t.Error("IsTestSecret wrong")
	}
}
