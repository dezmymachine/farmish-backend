package media

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/db"
	"github.com/dezmymachine/farmish-backend/internal/validation"
	"github.com/dezmymachine/farmish-backend/pkg/logger"
)

// allowedTypes are the only content types we accept (DOMAIN §7).
var allowedTypes = map[string]string{
	"image/jpeg": "jpg",
	"image/png":  "png",
	"image/webp": "webp",
}

// publicURLs is served by Storage; only R2 has a public domain.
type publicURLer interface{ PublicURL(key string) string }

// Service records uploads and attaches them to listings (Phase 11).
type Service struct {
	pool    *pgxpool.Pool
	storage Storage
	// Now is the clock, injectable so tests can age rows for the sweep.
	Now func() time.Time
}

// New returns a Service over pool and storage.
func New(pool *pgxpool.Pool, storage Storage) *Service {
	return &Service{pool: pool, storage: storage, Now: time.Now}
}

// MediaKey is the object key for a listing image: the owner is in the path
// so objects can be traced (and swept) per user.
func MediaKey(ownerID uuid.UUID, contentType string) string {
	ext, ok := allowedTypes[contentType]
	if !ok {
		ext = "bin"
	}
	return fmt.Sprintf("listings/%s/%s.%s", ownerID, uuid.NewString(), ext)
}

// CreateUpload validates the declaration, records a pending row and returns
// the presigned PUT. The client must send exactly the returned headers.
func (s *Service) CreateUpload(ctx context.Context, ownerID uuid.UUID, purpose, contentType string, size int64) (Upload, error) {
	var verr validation.Error
	if purpose != PurposeListingImage {
		verr.Add("purpose", "must be listing_image")
	}
	if _, ok := allowedTypes[contentType]; !ok {
		verr.Add("contentType", "must be image/jpeg, image/png or image/webp")
	}
	if size < 1 || size > MaxSizeBytes {
		verr.Add("sizeBytes", fmt.Sprintf("must be between 1 and %d bytes", MaxSizeBytes))
	}
	if err := verr.OrNil(); err != nil {
		return Upload{}, err
	}

	key := MediaKey(ownerID, contentType)
	url, headers, err := s.storage.PresignPut(ctx, key, contentType, size, PresignTTL)
	if err != nil {
		return Upload{}, err
	}
	row, err := db.New(s.pool).InsertMediaObject(ctx, db.InsertMediaObjectParams{
		OwnerID: ownerID, Key: key, Purpose: purpose, ContentType: contentType, SizeBytes: size,
	})
	if err != nil {
		return Upload{}, fmt.Errorf("insert media object: %w", err)
	}
	up := Upload{Object: fromRow(row), URL: url, Method: "PUT", Headers: headers, ExpiresAt: s.Now().Add(PresignTTL)}
	if p, ok := s.storage.(publicURLer); ok {
		up.PublicURL = p.PublicURL(key)
	}
	// The URL is a bearer secret for 10 minutes: log the key, never the URL.
	logger.FromContext(ctx).Info("media upload signed",
		slog.String("media_id", row.ID.String()), slog.String("key", key))
	return up, nil
}

// PublicURL is where an attached object is served from. It is the way the
// listing view turns a media key into an image URL.
func (s *Service) PublicURL(key string) string {
	if p, ok := s.storage.(publicURLer); ok {
		return p.PublicURL(key)
	}
	return ""
}

// Attach marks a pending object as attached, after checking ownership and
// that the bytes really are in storage with the declared size and type.
// Phase 11 calls it inside its own transaction; Head is a fast read against
// our own storage and runs before any row locks (ADR-0017).
func (s *Service) Attach(ctx context.Context, tx pgx.Tx, ownerID, mediaID uuid.UUID) (Object, error) {
	q := db.New(tx)
	row, err := q.GetMediaObject(ctx, mediaID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Object{}, fmt.Errorf("%w: %s", ErrNotFound, mediaID)
	}
	if err != nil {
		return Object{}, fmt.Errorf("get media object: %w", err)
	}
	if row.OwnerID != ownerID {
		return Object{}, fmt.Errorf("%w: %s", ErrForbidden, mediaID)
	}
	if row.Status == StatusAttached {
		return fromRow(row), nil // idempotent: Phase 11 owns the listing link
	}

	info, err := s.storage.Head(ctx, row.Key)
	if errors.Is(err, ErrObjectNotFound) {
		return Object{}, fmt.Errorf("%w: %s", ErrNotUploaded, mediaID)
	}
	if err != nil {
		return Object{}, err
	}
	if info.Size != row.SizeBytes || info.ContentType != row.ContentType {
		return Object{}, fmt.Errorf("%w: %s", ErrUploadMismatch, mediaID)
	}

	attached, err := q.AttachMediaObject(ctx, mediaID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Someone attached it while we were checking: treat as done.
		latest, gerr := q.GetMediaObject(ctx, mediaID)
		if gerr != nil {
			return Object{}, fmt.Errorf("get media object: %w", gerr)
		}
		return fromRow(latest), nil
	}
	if err != nil {
		return Object{}, fmt.Errorf("attach media object: %w", err)
	}
	return fromRow(attached), nil
}

// CleanupOrphans deletes pending objects (and their rows) older than
// MaxCleanupAge, in batches. A missing object counts as success; attached
// rows are never touched. It reports how many it removed.
func (s *Service) CleanupOrphans(ctx context.Context) (int, error) {
	cutoff := s.Now().Add(-MaxCleanupAge)
	log := logger.FromContext(ctx)
	removed := 0
	for {
		rows, err := db.New(s.pool).ListPendingMediaBefore(ctx, db.ListPendingMediaBeforeParams{
			CreatedAt: cutoff, Limit: cleanupBatch,
		})
		if err != nil {
			return removed, fmt.Errorf("list pending media: %w", err)
		}
		if len(rows) == 0 {
			return removed, nil
		}
		for _, row := range rows {
			if err := s.storage.Delete(ctx, row.Key); err != nil && !errors.Is(err, ErrObjectNotFound) {
				log.Warn("delete orphan failed; keeping the row for the next sweep",
					slog.String("media_id", row.ID.String()), slog.String("error", err.Error()))
				continue
			}
			if _, err := db.New(s.pool).DeleteMediaObject(ctx, row.ID); err != nil {
				log.Warn("delete orphan row failed", slog.String("media_id", row.ID.String()),
					slog.String("error", err.Error()))
				continue
			}
			removed++
		}
		if len(rows) < cleanupBatch {
			return removed, nil
		}
	}
}

func fromRow(r db.MediaObject) Object {
	return Object{
		ID: r.ID, OwnerID: r.OwnerID, Key: r.Key, Purpose: r.Purpose,
		ContentType: r.ContentType, SizeBytes: r.SizeBytes, Status: r.Status,
		CreatedAt: r.CreatedAt, AttachedAt: r.AttachedAt,
	}
}
