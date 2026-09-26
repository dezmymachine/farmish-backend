// Package paystacktest signs webhook bodies the way Paystack does, so tests
// and the manual QA script produce a body the endpoint will accept.
//
// Paystack computes HMAC-SHA512 over the exact request body with the secret
// key and sends the hex digest in x-paystack-signature.
package paystacktest

import (
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
)

// SignatureHeader is the header Paystack signs with.
const SignatureHeader = "x-paystack-signature"

// Sign returns the hex HMAC-SHA512 of body under secret.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha512.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// ChargeSuccessBody builds a charge.success event body. The fields are the ones
// the service reads; Paystack sends many more, and a real payload carries
// personal data, so this one deliberately does not.
func ChargeSuccessBody(reference string, amountPesewas int64, transactionID int64) []byte {
	return mustBody(map[string]any{
		"event": "charge.success",
		"data": map[string]any{
			"id": transactionID, "reference": reference,
			"amount": amountPesewas, "currency": "GHS", "fees": 199,
			"channel": "card", "status": "success", "paid_at": "2026-09-26T10:00:00Z",
		},
	})
}

// ChargeSuccessBodyWithCurrency is ChargeSuccessBody with an explicit currency,
// for the mismatch tests.
func ChargeSuccessBodyWithCurrency(reference string, amountPesewas int64, transactionID int64, currency string) []byte {
	return mustBody(map[string]any{
		"event": "charge.success",
		"data": map[string]any{
			"id": transactionID, "reference": reference,
			"amount": amountPesewas, "currency": currency, "fees": 199,
			"channel": "card", "status": "success", "paid_at": "2026-09-26T10:00:00Z",
		},
	})
}

// ChargeFailedBody builds a charge.failed event body.
func ChargeFailedBody(reference string, transactionID int64, reason string) []byte {
	return mustBody(map[string]any{
		"event": "charge.failed",
		"data": map[string]any{
			"id": transactionID, "reference": reference, "status": "failed",
			"reason": reason, "currency": "GHS",
		},
	})
}

// EventBody builds an arbitrary event with a data id, for the unknown-event
// and duplicate-delivery tests.
func EventBody(event string, dataID int64) []byte {
	return mustBody(map[string]any{
		"event": event,
		"data":  map[string]any{"id": dataID},
	})
}

func mustBody(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		// A map of scalars always marshals.
		panic("paystacktest: marshal body: " + err.Error())
	}
	return raw
}
