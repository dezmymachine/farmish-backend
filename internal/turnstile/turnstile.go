// Package turnstile verifies Cloudflare Turnstile tokens server-side.
// Turnstile is Cloudflare's low-friction bot check: the browser widget
// produces a single-use token that the API verifies with Cloudflare before
// accepting a sensitive anonymous request.
package turnstile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// SiteverifyURL is Cloudflare's verification endpoint.
const SiteverifyURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"

// ErrFailed means Cloudflare rejected the token: missing, invalid, expired or
// already used. Callers answer 400.
var ErrFailed = errors.New("turnstile verification failed")

// Verifier verifies a Turnstile token.
type Verifier interface {
	Verify(ctx context.Context, token string, remoteIP netip.Addr) error
}

// Client calls Cloudflare's siteverify API.
type Client struct {
	Secret string
	URL    string       // defaults to SiteverifyURL
	HTTP   *http.Client // defaults to a client with a 5s timeout
}

var _ Verifier = (*Client)(nil)

// New returns a Client for secret.
func New(secret string) *Client {
	return &Client{Secret: secret, URL: SiteverifyURL, HTTP: &http.Client{Timeout: 5 * time.Second}}
}

type siteverifyResponse struct {
	Success    bool     `json:"success"`
	ErrorCodes []string `json:"error-codes"`
}

// Verify returns nil if Cloudflare accepts token, an error wrapping ErrFailed
// if it rejects it, and any other error if Cloudflare couldn't be asked
// (callers fail closed).
func (c *Client) Verify(ctx context.Context, token string, remoteIP netip.Addr) error {
	token = strings.TrimSpace(token)
	if token == "" || len(token) > 2048 { // Cloudflare tokens are at most 2048 chars
		return fmt.Errorf("%w: missing or oversized token", ErrFailed)
	}

	form := url.Values{"secret": {c.Secret}, "response": {token}}
	if remoteIP.IsValid() {
		form.Set("remoteip", remoteIP.String())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("turnstile request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("turnstile siteverify: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("turnstile siteverify: status %d", resp.StatusCode)
	}
	var out siteverifyResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("turnstile siteverify: decode: %w", err)
	}
	if !out.Success {
		// Misconfiguration on our side isn't the client's fault.
		for _, code := range out.ErrorCodes {
			if code == "missing-input-secret" || code == "invalid-input-secret" {
				return fmt.Errorf("turnstile siteverify: server misconfigured: %v", out.ErrorCodes)
			}
		}
		return fmt.Errorf("%w: %v", ErrFailed, out.ErrorCodes)
	}
	return nil
}

// Cloudflare's published test secrets
// (https://developers.cloudflare.com/turnstile/troubleshooting/testing/).
// They must never be used when deployed.
var testSecrets = []string{
	"1x0000000000000000000000000000000AA", // always passes
	"2x0000000000000000000000000000000AA", // always fails
	"3x0000000000000000000000000000000AA", // "token already spent"
}

// IsTestSecret reports whether secret is one of Cloudflare's test secrets.
func IsTestSecret(secret string) bool {
	for _, s := range testSecrets {
		if secret == s {
			return true
		}
	}
	return false
}
