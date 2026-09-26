package handlers

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestNewViewerHasher(t *testing.T) {
	hash := NewViewerHasher([]byte("a-deployment-key"))
	ctx := context.Background()

	first := hash(ctx, "203.0.113.7")
	if first.String() == "" || first.String() == "00000000-0000-0000-0000-000000000000" {
		t.Fatalf("hash = %s, want a real uuid", first)
	}
	// Stable for the same address and key, so a viewer still dedupes.
	if again := hash(ctx, "203.0.113.7"); again != first {
		t.Errorf("hash is not stable: %s then %s", first, again)
	}
	// Different addresses, different viewers.
	if other := hash(ctx, "203.0.113.8"); other == first {
		t.Error("two addresses produced the same viewer id")
	}
	// A different key gives a different digest, so the id is not portable
	// between deployments and cannot be correlated across them.
	if other := NewViewerHasher([]byte("another-key"))(ctx, "203.0.113.7"); other == first {
		t.Error("the digest does not depend on the key")
	}
	// No address means no viewer, rather than a shared "unknown" id that would
	// collapse every such request into one.
	if got := hash(ctx, ""); got.String() != "00000000-0000-0000-0000-000000000000" {
		t.Errorf("empty address = %s, want the nil uuid", got)
	}
	// The address must not survive anywhere in the digest.
	if strings.Contains(first.String(), "203") {
		t.Errorf("digest %s leaks the address", first)
	}
}

func TestETagMatches(t *testing.T) {
	const etag = `W/"0123456789abcdef"`
	for _, tc := range []struct {
		header string
		want   bool
	}{
		{etag, true},
		{`"0123456789abcdef"`, true},        // weak comparison ignores W/
		{`W/"0123456789abcdef", "x"`, true}, // a list with a match
		{"*", true},
		{``, false},
		{`W/"fedcba9876543210"`, false},
		{`"other"`, false},
	} {
		if got := etagMatches(tc.header, etag); got != tc.want {
			t.Errorf("etagMatches(%q) = %v, want %v", tc.header, got, tc.want)
		}
	}
}

// TestWeakETag_IsStableOverTheSameBody guards the property the cache depends
// on: the same body always yields the same validator, and a different body
// never does.
func TestWeakETag_IsStableOverTheSameBody(t *testing.T) {
	type body struct {
		Items  []string `json:"items"`
		Total  int64    `json:"total"`
		Nested struct {
			Flag bool `json:"flag"`
		} `json:"nested"`
	}
	base := body{Items: []string{"a", "b"}, Total: 2}
	same := body{Items: []string{"a", "b"}, Total: 2}

	first, fresh := weakETag(base, nil)
	if fresh {
		t.Error("a nil If-None-Match must never be fresh")
	}
	again, _ := weakETag(same, nil)
	if first != again {
		t.Errorf("same body, different etags: %s vs %s", first, again)
	}
	if _, fresh := weakETag(same, &first); !fresh {
		t.Error("an identical body must revalidate as fresh")
	}

	// A change anywhere in the body, however deep, changes the validator.
	changed := same
	changed.Total = 3
	if _, fresh := weakETag(changed, &first); fresh {
		t.Error("a changed field must not revalidate as fresh")
	}
	deeper := same
	deeper.Nested.Flag = true
	if _, fresh := weakETag(deeper, &first); fresh {
		t.Error("a change in a nested field must not revalidate as fresh")
	}

	// The digest covers the actual JSON, not the Go value: a round trip
	// through the wire format is stable.
	raw, err := json.Marshal(same)
	if err != nil {
		t.Fatal(err)
	}
	var decoded body
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Items[0] != "a" {
		t.Errorf("round trip lost data: %+v", decoded)
	}
}
