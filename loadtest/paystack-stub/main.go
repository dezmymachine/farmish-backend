// Command paystack-stub is a stand-in for the Paystack API, for load tests
// only. It answers the two calls a checkout makes (initialize and verify)
// with instant successes, so a run measures this API and not Paystack's
// latency or rate limits.
//
// It is never wired into a deploy: config refuses any PAYSTACK_BASE_URL but
// https://api.paystack.co when APP_ENV=production.
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"strings"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9999", "listen address")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /transaction/initialize", initialize)
	mux.HandleFunc("GET /transaction/verify/{reference}", verify)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		reply(w, http.StatusNotFound, false, "paystack-stub: "+r.Method+" "+r.URL.Path+" is not stubbed", nil)
	})

	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("paystack-stub listening on %s", *addr)
	log.Fatal(srv.ListenAndServe())
}

func initialize(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Reference string `json:"reference"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil || strings.TrimSpace(in.Reference) == "" {
		reply(w, http.StatusBadRequest, false, "reference is required", nil)
		return
	}
	reply(w, http.StatusOK, true, "Authorization URL created", map[string]any{
		"authorization_url": "https://checkout.paystack.test/" + in.Reference,
		"access_code":       "ac_" + in.Reference,
		"reference":         in.Reference,
	})
}

// verify reports every charge as still pending: the load test settles
// payments through signed webhooks, never through the verify fallback.
func verify(w http.ResponseWriter, r *http.Request) {
	reply(w, http.StatusOK, true, "Verification successful", map[string]any{
		"status": "pending", "reference": r.PathValue("reference"), "currency": "GHS",
	})
}

func reply(w http.ResponseWriter, status int, ok bool, message string, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]any{"status": ok, "message": message, "data": data}); err != nil {
		log.Printf("write response: %v", err)
	}
}
