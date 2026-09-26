// Package media tracks images uploaded by clients. The bytes go straight to
// S3-compatible storage (Cloudflare R2, or the local S3 stand-in) with
// short-lived presigned PUTs; this package owns the database side and the
// orphan sweep.
package media

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Purposes stored in media_objects.purpose.
const (
	PurposeListingImage = "listing_image"
)

// Statuses stored in media_objects.status.
const (
	StatusPending  = "pending"
	StatusAttached = "attached"
)

// PresignTTL is how long an upload URL stays valid. Presigned URLs are
// secrets: never log them.
const PresignTTL = 10 * time.Minute

// Limits from DOMAIN §7: images only, at most 5 MB.
const (
	MaxSizeBytes  = 5 * 1024 * 1024
	MaxCleanupAge = 24 * time.Hour
	cleanupBatch  = 100
)

var (
	// ErrNotFound means no media_objects row matches.
	ErrNotFound = errors.New("media object not found")
	// ErrForbidden means the object belongs to another user.
	ErrForbidden = errors.New("not the owner of this media object")
	// ErrNotUploaded means the presigned upload never happened.
	ErrNotUploaded = errors.New("media object was never uploaded")
	// ErrUploadMismatch means the stored object differs from what was
	// declared when the URL was signed (size or content type).
	ErrUploadMismatch = errors.New("uploaded object does not match the signed request")
	// ErrObjectNotFound is returned by Storage.Head and Delete when the
	// object is absent from storage.
	ErrObjectNotFound = errors.New("object not found in storage")
	// ErrInvalidMedia is returned by Storage for content types outside the
	// allowlist.
	ErrInvalidMedia = errors.New("unsupported media type")
)

// ObjectInfo is what storage knows about a stored object.
type ObjectInfo struct {
	Size        int64
	ContentType string
}

// Storage is the object store the service needs. R2 is the real
// implementation; tests run it against the local S3 stand-in.
type Storage interface {
	// PresignPut returns a URL the client PUTs to, plus the headers it must
	// send exactly. Content type and length are signed.
	PresignPut(ctx context.Context, key, contentType string, size int64, ttl time.Duration) (url string, headers map[string]string, err error)
	Head(ctx context.Context, key string) (ObjectInfo, error)
	Delete(ctx context.Context, key string) error
}

// Object is a media_objects row.
type Object struct {
	ID          uuid.UUID
	OwnerID     uuid.UUID
	Key         string
	Purpose     string
	ContentType string
	SizeBytes   int64
	Status      string
	CreatedAt   time.Time
	AttachedAt  *time.Time
}

// Upload is what CreateUpload returns to the caller.
type Upload struct {
	Object
	// URL, Method and Headers describe the presigned upload; ExpiresAt is
	// when the signature dies; PublicURL is where the object is served from.
	URL       string
	Method    string
	Headers   map[string]string
	ExpiresAt time.Time
	PublicURL string
}
