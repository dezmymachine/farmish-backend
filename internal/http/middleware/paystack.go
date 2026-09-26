package middleware

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
)

// PaystackPath is the one path PaystackSignature acts on.
const PaystackPath = "/v1/webhooks/paystack"

// paystackSignatureHeader is the header Paystack signs with.
const paystackSignatureHeader = "x-paystack-signature"

// maxWebhookBody caps the body we will read and hash. Paystack events are a
// few kilobytes; a megabyte is generous, and the cap is what stops this from
// being an unbounded read on a public endpoint.
const maxWebhookBody = 1 << 20 // 1 MB

// paystackRawBodyKey is where the verified bytes are stashed for the handler.
const paystackRawBodyKey = "farmish.paystack.raw_body"

// PaystackSignature verifies the HMAC-SHA512 of the raw request body under the
// secret key and compares it to x-paystack-signature in constant time.
//
// It must be mounted before the OpenAPI validator, which parses and re-reads
// the body: the signature is only meaningful over the exact bytes Paystack
// sent, so the body is read here, hashed, and put back for the validator.
//
// Only PaystackPath is touched; every other request passes through untouched.
// A missing or wrong signature is a 401, and the body is neither parsed nor
// stored.
func PaystackSignature(secret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.URL.Path != PaystackPath || c.Request.Method != http.MethodPost {
			c.Next()
			return
		}
		if secret == "" {
			// Refusing is the fail-closed choice: without a secret nothing can
			// be verified, and a webhook we cannot authenticate is a webhook we
			// must not act on.
			apierror.Abort(c, http.StatusServiceUnavailable, apierror.CodeUnavailable,
				"Webhook verification is not configured")
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxWebhookBody))
		if err != nil {
			// Too large, or the client went away. Nothing is hashed or stored.
			apierror.Abort(c, http.StatusBadRequest, apierror.CodeBadRequest, "Malformed request")
			return
		}
		// The validator and the handler both need to read the body.
		c.Request.Body = io.NopCloser(bytes.NewReader(body))

		signature := c.GetHeader(paystackSignatureHeader)
		if !validPaystackSignature(secret, body, signature) {
			// Deliberately says nothing about why: a wrong signature and a
			// missing one are the same answer.
			apierror.Abort(c, http.StatusUnauthorized, apierror.CodeUnauthorized, "Invalid signature")
			return
		}
		c.Set(paystackRawBodyKey, body)
		c.Next()
	}
}

// validPaystackSignature compares the header to the expected digest in
// constant time. Paystack sends the hex digest; a W/ prefix or surrounding
// whitespace is not part of it and is not accepted.
func validPaystackSignature(secret string, body []byte, header string) bool {
	if header == "" {
		return false
	}
	got, err := hex.DecodeString(header)
	if err != nil {
		return false
	}
	mac := hmac.New(sha512.New, []byte(secret))
	mac.Write(body)
	// hmac.Equal is the constant-time comparison. A plain == would leak how
	// much of a forged signature was correct.
	return hmac.Equal(got, mac.Sum(nil))
}

// RawPaystackBody returns the verified body the signature middleware read. It
// is only set on a request that passed verification.
func RawPaystackBody(c *gin.Context) ([]byte, bool) {
	v, ok := c.Get(paystackRawBodyKey)
	if !ok {
		return nil, false
	}
	body, ok := v.([]byte)
	return body, ok
}
