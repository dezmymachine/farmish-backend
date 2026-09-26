package media_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dezmymachine/farmish-backend/internal/database"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/media"
	"github.com/dezmymachine/farmish-backend/internal/media/mediatest"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

// put uploads body to url with the headers a presign returned and returns the
// response status.
func put(t *testing.T, url string, headers map[string]string, body string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func TestUploadURL_PresignedPutWorks(t *testing.T) {
	store := mediatest.R2(t)
	pool := dbtest.Pool(t)
	ctx := context.Background()
	svc := media.New(pool, store)
	uid := newOwner(t, pool, "media-put")

	up, err := svc.CreateUpload(ctx, uid, media.PurposeListingImage, "image/jpeg", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if up.Method != "PUT" || up.Headers["Content-Type"] != "image/jpeg" ||
		up.Headers["Content-Length"] != "1000" {
		t.Errorf("presign = %+v", up)
	}
	if up.ExpiresAt.IsZero() || !strings.Contains(up.PublicURL, up.Key) {
		t.Errorf("upload = %+v", up)
	}
	// The row starts pending.
	if up.Status != media.StatusPending {
		t.Errorf("status = %q", up.Status)
	}

	if code := put(t, up.URL, up.Headers, strings.Repeat("a", 1000)); code != http.StatusOK {
		t.Fatalf("PUT status %d", code)
	}
	info, err := store.Head(ctx, up.Key)
	if err != nil || info.Size != 1000 || info.ContentType != "image/jpeg" {
		t.Fatalf("head = %+v, %v", info, err)
	}
}

func TestUploadURL_RejectsTypeAndSize(t *testing.T) {
	store := mediatest.R2(t)
	pool := dbtest.Pool(t)
	svc := media.New(pool, store)
	uid := newOwner(t, pool, "media-reject")
	ctx := context.Background()

	for name, args := range map[string]struct {
		contentType string
		size        int64
	}{
		"gif":        {"image/gif", 1000},
		"pdf":        {"application/pdf", 1000},
		"zero bytes": {"image/jpeg", 0},
		"negative":   {"image/png", -1},
		"over 5MB":   {"image/jpeg", media.MaxSizeBytes + 1},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.CreateUpload(context.Background(), uid, media.PurposeListingImage, args.contentType, args.size)
			var verr *validation.Error
			if !errors.As(err, &verr) || len(verr.Fields) == 0 {
				t.Fatalf("err = %v, want *validation.Error", err)
			}
		})
	}
	// A bad purpose is refused too.
	if _, err := svc.CreateUpload(ctx, uid, "avatar", "image/jpeg", 10); err == nil {
		t.Error("bad purpose accepted")
	}
}

// The signed Content-Length is what stops a 5 GB upload to our bucket.
func TestUploadURL_WrongSizeUploadRejected(t *testing.T) {
	store := mediatest.R2(t)
	pool := dbtest.Pool(t)
	svc := media.New(pool, store)
	uid := newOwner(t, pool, "media-wrongsize")

	up, err := svc.CreateUpload(context.Background(), uid, media.PurposeListingImage, "image/png", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if code := put(t, up.URL, up.Headers, strings.Repeat("a", 2000)); code == http.StatusOK {
		t.Fatalf("storage accepted a 2000-byte body signed for 1000: %d", code)
	}
	// Nothing was stored.
	if _, err := store.Head(context.Background(), up.Key); !errors.Is(err, media.ErrObjectNotFound) {
		t.Errorf("head after rejected upload = %v", err)
	}
}

func TestAttach_OwnershipAndState(t *testing.T) {
	store := mediatest.R2(t)
	pool := dbtest.Pool(t)
	ctx := context.Background()
	svc := media.New(pool, store)
	mine := newOwner(t, pool, "media-attach-mine")
	other := newOwner(t, pool, "media-attach-other")

	inTx := func(fn func(tx pgx.Tx) error) error {
		return database.InTx(ctx, pool, fn)
	}

	// Not uploaded yet.
	up, err := svc.CreateUpload(ctx, mine, media.PurposeListingImage, "image/webp", 512)
	if err != nil {
		t.Fatal(err)
	}
	var got media.Object
	err = inTx(func(tx pgx.Tx) error {
		var aerr error
		got, aerr = svc.Attach(ctx, tx, mine, up.ID)
		return aerr
	})
	if !errors.Is(err, media.ErrNotUploaded) {
		t.Fatalf("attach before upload = %v", err)
	}

	// Another user's object.
	err = inTx(func(tx pgx.Tx) error {
		_, aerr := svc.Attach(ctx, tx, other, up.ID)
		return aerr
	})
	if !errors.Is(err, media.ErrForbidden) {
		t.Fatalf("cross-user attach = %v", err)
	}

	// The stored object disagrees with the declared size: mismatch. The
	// presign itself prevents this, so the row is edited to simulate a
	// mismatch (e.g. an object replaced out of band).
	bad, err := svc.CreateUpload(ctx, mine, media.PurposeListingImage, "image/jpeg", 100)
	if err != nil {
		t.Fatal(err)
	}
	if code := put(t, bad.URL, bad.Headers, strings.Repeat("a", 100)); code != http.StatusOK {
		t.Fatalf("upload: %d", code)
	}
	if _, err := pool.Exec(ctx, `UPDATE media_objects SET size_bytes = 999 WHERE id = $1`, bad.ID); err != nil {
		t.Fatal(err)
	}
	err = inTx(func(tx pgx.Tx) error {
		_, aerr := svc.Attach(ctx, tx, mine, bad.ID)
		return aerr
	})
	if !errors.Is(err, media.ErrUploadMismatch) {
		t.Fatalf("attach with size mismatch = %v", err)
	}

	// The happy path, then idempotency.
	if code := put(t, up.URL, up.Headers, strings.Repeat("a", 512)); code != http.StatusOK {
		t.Fatalf("upload: %d", code)
	}
	err = inTx(func(tx pgx.Tx) error {
		var aerr error
		got, aerr = svc.Attach(ctx, tx, mine, up.ID)
		return aerr
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != media.StatusAttached || got.AttachedAt == nil {
		t.Fatalf("attached = %+v", got)
	}
	err = inTx(func(tx pgx.Tx) error {
		_, aerr := svc.Attach(ctx, tx, mine, up.ID)
		return aerr
	})
	if err != nil {
		t.Fatalf("second attach = %v", err)
	}
	// Unknown id.
	err = inTx(func(tx pgx.Tx) error {
		_, aerr := svc.Attach(ctx, tx, mine, uuid.New())
		return aerr
	})
	if !errors.Is(err, media.ErrNotFound) {
		t.Errorf("attach unknown = %v", err)
	}
}

func TestCleanupOrphans(t *testing.T) {
	store := mediatest.R2(t)
	pool := dbtest.Pool(t)
	ctx := context.Background()
	svc := media.New(pool, store)
	uid := newOwner(t, pool, "media-cleanup")

	now := time.Now()
	svc.Now = func() time.Time { return now }

	// Uploaded and old: swept.
	old, err := svc.CreateUpload(ctx, uid, media.PurposeListingImage, "image/jpeg", 64)
	if err != nil {
		t.Fatal(err)
	}
	if code := put(t, old.URL, old.Headers, strings.Repeat("a", 64)); code != http.StatusOK {
		t.Fatalf("upload: %d", code)
	}
	// Old but attached: untouched.
	attached, err := svc.CreateUpload(ctx, uid, media.PurposeListingImage, "image/png", 64)
	if err != nil {
		t.Fatal(err)
	}
	if code := put(t, attached.URL, attached.Headers, strings.Repeat("a", 64)); code != http.StatusOK {
		t.Fatalf("upload: %d", code)
	}
	if err := database.InTx(ctx, pool, func(tx pgx.Tx) error {
		_, aerr := svc.Attach(ctx, tx, uid, attached.ID)
		return aerr
	}); err != nil {
		t.Fatal(err)
	}
	// Old, pending, but the object is already gone: tolerated.
	missing, err := svc.CreateUpload(ctx, uid, media.PurposeListingImage, "image/webp", 64)
	if err != nil {
		t.Fatal(err)
	}
	// Young: not swept.
	fresh, err := svc.CreateUpload(ctx, uid, media.PurposeListingImage, "image/jpeg", 64)
	if err != nil {
		t.Fatal(err)
	}
	if code := put(t, fresh.URL, fresh.Headers, strings.Repeat("a", 64)); code != http.StatusOK {
		t.Fatalf("upload: %d", code)
	}

	ageRows(t, pool, now.Add(-25*time.Hour), old.ID, attached.ID, missing.ID)

	removed, err := svc.CleanupOrphans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2 (old + missing)", removed)
	}
	if _, err := store.Head(ctx, old.Key); !errors.Is(err, media.ErrObjectNotFound) {
		t.Errorf("orphan object still in storage: %v", err)
	}
	if _, err := store.Head(ctx, attached.Key); err != nil {
		t.Errorf("attached object was swept: %v", err)
	}
	if _, err := store.Head(ctx, fresh.Key); err != nil {
		t.Errorf("young object was swept: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_objects WHERE id = ANY($1)`, []uuid.UUID{old.ID, attached.ID, missing.ID, fresh.ID}).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("rows left = %d, want 2 (attached + fresh)", n)
	}
}

func TestMediaKey_Format(t *testing.T) {
	uid := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	key := media.MediaKey(uid, "image/jpeg")
	pattern := regexp.MustCompile(`^listings/11111111-2222-3333-4444-555555555555/[0-9a-f-]{36}\.jpg$`)
	if !pattern.MatchString(key) {
		t.Errorf("key = %q", key)
	}
	for contentType, ext := range map[string]string{"image/jpeg": "jpg", "image/png": "png", "image/webp": "webp"} {
		if got := media.MediaKey(uid, contentType); !strings.HasSuffix(got, "."+ext) {
			t.Errorf("%s key = %q, want .%s", contentType, got, ext)
		}
	}
	// Keys are unique per call.
	first, second := media.MediaKey(uid, "image/png"), media.MediaKey(uid, "image/png")
	if first == second {
		t.Error("MediaKey is not unique")
	}
}

// Presigned URLs expire; check the TTL is the documented 10 minutes.
func TestPresignTTL(t *testing.T) {
	store := mediatest.R2(t)
	url, _, err := store.PresignPut(context.Background(), "listings/u/t.jpg", "image/jpeg", 10, media.PresignTTL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(url, "X-Amz-Expires="+strconv.Itoa(int(media.PresignTTL.Seconds()))) {
		t.Errorf("url does not carry the 10-minute expiry: %s", url)
	}
	if media.PresignTTL != 10*time.Minute {
		t.Errorf("PresignTTL = %v", media.PresignTTL)
	}
}
