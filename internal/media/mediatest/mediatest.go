// Package mediatest provides a real S3-compatible storage for media tests
// (the local stand-in from docker-compose), created per test run.
//
// Set MEDIA_TEST_S3_ENDPOINT to its URL (make test does). Without it, media
// tests are skipped, unless MEDIA_TEST_S3_REQUIRED=1, in which case they
// fail instead.
package mediatest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/dezmymachine/farmish-backend/internal/config"
	"github.com/dezmymachine/farmish-backend/internal/media"
)

// BucketPrefix namespaces buckets this package creates; they're removed when
// the test ends.
const BucketPrefix = "farmish-test-"

// endpoint returns the stand-in URL, skipping (or failing) without one.
func endpoint(t testing.TB) string {
	t.Helper()
	if e := os.Getenv("MEDIA_TEST_S3_ENDPOINT"); e != "" {
		return e
	}
	if os.Getenv("MEDIA_TEST_S3_REQUIRED") == "1" {
		t.Fatal("mediatest: MEDIA_TEST_S3_ENDPOINT is not set (MEDIA_TEST_S3_REQUIRED=1)")
	}
	t.Skip("mediatest: MEDIA_TEST_S3_ENDPOINT not set; skipping media test (run `make test`)")
	return ""
}

// R2 returns a media.Storage backed by the local S3 stand-in, with a fresh
// bucket that is removed when the test ends.
func R2(t testing.TB) *media.R2 {
	t.Helper()
	ctx := context.Background()
	bucket := BucketPrefix + randomHex(t, 6)
	s, err := media.NewR2(config.R2{
		AccessKeyID:   envOr("R2_ACCESS_KEY_ID", "farmish"),
		SecretKey:     envOr("R2_SECRET_ACCESS_KEY", "farmish-secret"),
		Bucket:        bucket,
		PublicBaseURL: "http://" + endpoint(t) + "/" + bucket,
		Endpoint:      endpoint(t),
	})
	if err != nil {
		t.Fatalf("mediatest: %v", err)
	}
	if err := s.CreateBucket(ctx); err != nil {
		t.Fatalf("mediatest: %v", err)
	}
	t.Cleanup(func() { s.DeleteBucket(context.Background()) })
	return s
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func randomHex(t testing.TB, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}
