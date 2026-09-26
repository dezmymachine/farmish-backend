# Phase 10: Media uploads (Cloudflare R2)

**Depends on:** 4 · **Size:** medium

## Goal

Clients upload images straight to R2 with short-lived presigned PUT URLs. The API tracks each object (`media_objects`) until a listing attaches it (Phase 11), and a periodic job deletes orphans.

## Design

- Use the **S3-compatible API**: `github.com/aws/aws-sdk-go-v2` (`service/s3` plus `s3.NewPresignClient`). Pin versions.
- R2 endpoint: `https://<R2_ACCOUNT_ID>.r2.cloudflarestorage.com`, region `auto`. `R2_ENDPOINT` overrides it for local **MinIO**.
- **Local/test:** add a `minio` service to `docker-compose.yml` (`minio/minio`, pinned tag, `127.0.0.1:9000`, console :9001, credentials `farmish` / `farmish-secret`) plus a bucket-creation step (an `mc` one-shot container or the SDK `CreateBucket` in tests).
- **`media.Storage` interface:**

  ```go
  type Storage interface {
      PresignPut(ctx context.Context, key, contentType string, size int64, ttl time.Duration) (url string, headers map[string]string, err error)
      Head(ctx context.Context, key string) (ObjectInfo, error)   // size, content type; ErrObjectNotFound
      Delete(ctx context.Context, key string) error
  }
  ```

  The real implementation is `media.R2`. Tests run the real implementation against MinIO; there's no fake needed for this package.
- **Key format:** `listings/<owner_user_id>/<uuid>.<ext>`, where the ext comes from the content type: jpeg → `jpg`, png, webp.
- **Allowlist:** `image/jpeg`, `image/png`, `image/webp`. Max **5 MB** (5 × 1024 × 1024), both from DOMAIN §7.
- **Presign** with `ContentType` **and** `ContentLength` set, so both are signed headers. The client must send exactly those or the upload is rejected. **TTL: 10 minutes.**

## Schema: `migrations/000006_media.up.sql`

```sql
CREATE TABLE media_objects (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  owner_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  key           text NOT NULL UNIQUE,
  purpose       text NOT NULL CHECK (purpose IN ('listing_image')),
  content_type  text NOT NULL CHECK (content_type IN ('image/jpeg','image/png','image/webp')),
  size_bytes    bigint NOT NULL CHECK (size_bytes BETWEEN 1 AND 5242880),
  status        text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','attached')),
  created_at    timestamptz NOT NULL DEFAULT now(),
  attached_at   timestamptz
);
CREATE INDEX media_objects_pending_idx ON media_objects (created_at) WHERE status = 'pending';
CREATE INDEX media_objects_owner_idx ON media_objects (owner_id);
```

## API (tag `media`)

`POST /v1/media/upload-url`: bearer, `x-farmish-rate-limit: sensitive`.
- **Request:** `{purpose: "listing_image", contentType: enum, sizeBytes: 1..5242880}`, with `additionalProperties: false`.
- **200 response:** `{mediaId, uploadUrl, method: "PUT", headers: {Content-Type, Content-Length}, expiresAt, publicUrl}`, where `publicUrl = R2_PUBLIC_BASE_URL + "/" + key`.
- **400** for a disallowed type or size. The spec enum and max give the validator this, with a service-level re-check.

## Service (`internal/media`)

- `CreateUpload(ctx, ownerID, purpose, contentType, size)`: inserts a `pending` row and returns the presign.
- `Attach(ctx, tx, ownerID, mediaID) (Object, error)`, used by Phase 11 inside its tx. It checks:
  - the row exists
  - `owner_id == ownerID` (else `ErrForbidden`)
  - `status == pending`, or already attached to **this** listing (idempotent). Pass a listing ID, or let Phase 11 handle it through `listing_images` uniqueness.
  - `Storage.Head` shows the object exists (else `ErrNotUploaded`) with a matching size and content type (else `ErrUploadMismatch`)

  Then it sets `status='attached'` and `attached_at`. Note: `Head` is a network call inside a DB tx. That's acceptable here because it's a fast read against our own storage, but **keep it before** any row locks. Document this exception in the ADR.
- `CleanupOrphans(ctx)`: for `pending` rows older than **24h**, `Storage.Delete` then delete the row. Batches of 100. A not-found object counts as success.

## Jobs

`media.cleanup_orphans`: periodic, **hourly** (`reg.Every(time.Hour, …, false)`).

## Config

- `R2_ACCOUNT_ID`, `R2_ACCESS_KEY_ID`, `R2_SECRET_ACCESS_KEY`, `R2_BUCKET`, `R2_PUBLIC_BASE_URL`: required when deployed.
- `R2_ENDPOINT`: optional override (MinIO).
- In development and test, the `.env.example` defaults point at compose MinIO (`R2_ENDPOINT=http://127.0.0.1:9000`, bucket `farmish-dev`, public base `http://127.0.0.1:9000/farmish-dev`).
- **Never log the secret.**
- **R2 bucket CORS:** allow `PUT` from the frontend origins with the `Content-Type` header. Document this in `docs/runbooks/r2.md`, and do it in Phase 22.

## Tests (against MinIO)

| Test | Proves |
|---|---|
| `TestUploadURL_PresignedPutWorks` | Request a URL, `PUT` the exact bytes with the returned headers → 200; `Head` sees it |
| `TestUploadURL_RejectsTypeAndSize` | `image/gif`, `application/pdf`, 0 bytes, 5 MB + 1 → 400 `validation_failed` (`assertContract`) |
| `TestUploadURL_WrongSizeUploadRejected` | Presign for 1000 bytes, PUT 2000 → storage rejects (signed content-length) |
| `TestUploadURL_Unauthenticated401` | |
| `TestAttach_OwnershipAndState` | Another user's media → `ErrForbidden`; not uploaded → `ErrNotUploaded`; success sets attached; second attach is idempotent |
| `TestCleanupOrphans` | Clock at +25h: pending rows and objects deleted, attached untouched, missing object tolerated |
| `TestMediaKey_Format` | `listings/<uuid>/<uuid>.jpg` |

## Manual QA

`make minio-up` (add the target), then `make run`. Call `curl` for the upload URL, then `curl -X PUT -H 'Content-Type: image/jpeg' --data-binary @test.jpg "<url>"` → 200. Open the `publicUrl` in a browser (the MinIO bucket needs a public-read policy for dev; document that).

## Pitfalls

- The presign **must** include `Content-Length`, or anyone can upload a 5 GB file to our bucket.
- Presigned URLs are secrets for 10 minutes. Don't log them.
- R2 has no ACLs. Public reading is configured via the bucket's public domain (R2 dev URL or a custom domain), not per object.
