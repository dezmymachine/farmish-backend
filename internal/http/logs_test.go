package httpapi

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/dezmymachine/farmish-backend/internal/auth/authtest"
	"github.com/dezmymachine/farmish-backend/internal/payments/paystacktest"
)

// TestLogs_NoSecretsOrPII is the Phase 21 log guarantee: a phone sign-in, a
// listing, a checkout, signed webhooks, a dispute and refund, a payout account
// and a payout run with the logs captured (from the raw handler, with no
// redaction in the way), and none of the credentials or personal data those
// flows touch may appear in what was written.
func TestLogs_NoSecretsOrPII(t *testing.T) {
	logs := captureLogs(t)

	f := newAdminFixture(t)
	phoneUser := authtest.PhoneUser(t)
	me := serve(t, f.router, meRequest(http.MethodGet, phoneUser.Token, ""))
	if me.Code != http.StatusOK {
		t.Fatalf("phone sign-in: %d %s", me.Code, me.Body.String())
	}
	// Failure paths log too: a rejected token and a forged webhook signature.
	serve(t, f.router, meRequest(http.MethodGet, "eyJhbGciOiJub25lIn0.eyJzdWIiOiJ4In0.forged", ""))
	forged := paystacktest.ChargeSuccessBody("ref-forged", 1000, 1)
	const forgedSig = "deadbeef"
	if w := postWebhook(f.router, forged, forgedSig); w.Code != http.StatusUnauthorized {
		t.Fatalf("forged webhook: %d", w.Code)
	}

	money := driveOrderMoney(t, f)
	waitJobsIdle(t, f.pool)
	payout := drivePayouts(t, newExecPayoutFixture(t))

	out := logs.String()
	for _, line := range []string{`"msg":"http_request"`, `"msg":"payment settled"`, `"msg":"sms (log-only)"`, `"msg":"payout failed; payable re-credited"`} {
		if !strings.Contains(out, line) {
			t.Fatalf("the capture is missing %s: the scenario did not exercise the logging it is meant to police (%d bytes captured)", line, len(out))
		}
	}

	national := strings.TrimPrefix(phoneUser.Phone, "+233")
	secrets := map[string]string{
		"phone (E.164)":         phoneUser.Phone,
		"phone (national)":      national,
		"phone (local)":         "0" + national,
		"phone token":           phoneUser.Token,
		"account number":        payout.accountNumber,
		"paystack secret":       paystackSecret,
		"forged signature":      forgedSig,
		"forged token":          "forged",
		"phone user's password": phoneUser.Password,
	}
	for i, tok := range money.tokens {
		secrets["token "+string(rune('a'+i))] = tok
	}
	for i, phone := range money.phones {
		secrets["order phone "+string(rune('a'+i))] = phone
		secrets["order phone digits "+string(rune('a'+i))] = strings.TrimPrefix(phone, "+233")
	}
	for name, value := range secrets {
		if value != "" && strings.Contains(out, value) {
			t.Errorf("%s appears in the logs", name)
		}
	}

	for name, pattern := range map[string]*regexp.Regexp{
		"a JWT":                     regexp.MustCompile(`eyJ[A-Za-z0-9_-]{6,}`),
		"a webhook signature":       regexp.MustCompile(`\b[0-9a-f]{128}\b`),
		"a Ghana E.164 number":      regexp.MustCompile(`\+233[0-9]{9}`),
		"a Ghana local number":      regexp.MustCompile(`\b0[235][0-9]{8}\b`),
		"a Paystack key":            regexp.MustCompile(`\b[sp]k_(test|live)_[A-Za-z0-9]+`),
		"an email address":          regexp.MustCompile(`[A-Za-z0-9._%+-]+@(farmish\.test|example\.com)`),
		"a bearer credential":       regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._-]{8,}`),
		"an Authorization header":   regexp.MustCompile(`(?i)"authorization"`),
		"a paystack signature name": regexp.MustCompile(`(?i)x-paystack-signature`),
	} {
		if m := pattern.FindString(out); m != "" {
			t.Errorf("the logs contain %s: %q", name, m)
		}
	}
}
