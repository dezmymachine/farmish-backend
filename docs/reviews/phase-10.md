# Review packet: Phase 10 Media uploads (R2)

## Summary

Clients upload listing images straight to object storage: the API tracks
each upload in `media_objects`, hands back a 10-minute presigned PUT with
signed `Content-Type` and `Content-Length`, and an hourly River job deletes
uploads nobody claimed within 24 hours. Production is Cloudflare R2 over its
S3 API; locally and in tests the compose **rustfs** service stands in
(owner decision, ADR-0016: MinIO's images are no longer pullable). The
signed-length check is what stops a 5 GB upload to our bucket, and it is
tested end to end (403 on a wrong body size).

## Commits

`git log --oneline b7352d5..HEAD` output (plus this packet commit):

```
10a9b5e Phase 10: media migration, queries and local S3 stand-in
5d6662f Phase 10: R2 media config
3274960 Phase 10: media storage client and service
d586aa6 Phase 10: upload-url endpoint, handler and wiring
9a379b9 Phase 10: hourly media.cleanup_orphans job
428fce1 Phase 10: media runbook and ADRs
7149b89 Phase 10: tidy go.mod (AWS SDK now a direct dependency)
bf8270f Phase 10: lint fixes in media tests and helper
fcc7e10 Phase 10: config fixtures gain R2 values
2c10752 Phase 10: smoke script reads container logs once
d0256bd Phase 10: plan and handbook updates
(plus this packet commit; verify with git log --oneline b7352d5..HEAD)
```

## Done-when checklist

From REDEVELOPMENT_PLAN.md §6 (Phase 10) and the spec test table:

- [x] A presigned PUT works against the local S3 stand-in in compose: `TestUploadURL_PresignedPutWorks` (`internal/media/service_test.go`) and `TestCreateMediaUploadUrl_EndToEnd` (`internal/http/media_test.go`) both PUT the exact bytes and assert 200 plus a matching `Head`
- [x] A disallowed type or size is rejected: `TestUploadURL_RejectsTypeAndSize` (gif, pdf, 0, negative, 5 MB + 1 → `validation.Error`) and `TestCreateMediaUploadUrl_RejectsTypeAndSize` (400 `validation_failed` + `assertContract`)
- [x] A signed content-length mismatch is rejected: `TestUploadURL_WrongSizeUploadRejected` — 2000 bytes against a 1000-byte presign → 403, nothing stored
- [x] Orphans are cleaned in the job test: `TestCleanupOrphans` (rows + objects, attached untouched, missing object tolerated) and `TestCleanupOrphans_JobEndToEnd` (enqueued through River, waits for completion)
- [x] `TestUploadURL_Unauthenticated401` → `TestCreateMediaUploadUrl_Unauthenticated401` (missing and garbage token, same generic 401)
- [x] `TestAttach_OwnershipAndState` (`internal/media/service_test.go`): another user's media → `ErrForbidden`, never uploaded → `ErrNotUploaded`, size mismatch → `ErrUploadMismatch`, success sets `attached`, second attach idempotent
- [x] `TestMediaKey_Format`: `listings/<uuid>/<uuid>.jpg` (+ png/webp, unique per call)
- [x] `TestPresignTTL`: the URL carries a 600 s expiry

Extras: `TestCreateMediaUploadUrl_SensitiveRateLimit` (the `sensitive`
extension is applied), `TestGroups`-style `TestConfig_R2`,
`TestRegionDistricts` unchanged.

## make ci

Last 25 lines of `make ci` output, ending in `ci: all checks passed`:

```

#12 [build 4/6] RUN --mount=type=cache,target=/go/pkg/mod go mod download
#12 CACHED

#13 [build 5/6] COPY . .
#13 DONE 0.1s

#14 [build 6/6] RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build     CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/api ./cmd/migrate
#14 DONE 3.8s

#15 [stage-1 2/2] COPY --from=build /out/api /out/migrate /
#15 CACHED

#16 exporting to image
#16 exporting layers done
#16 exporting manifest sha256:0e6d9b7ebe86b58541e177657c9d8cb0a9cbc65a9bb90b1973101f37bd6730f9 done
#16 exporting config sha256:9a554a4fabc3328098298e885642b145e69ab51127f690852788adc8820e1a65 done
#16 exporting attestation manifest sha256:2ce0f7f0887556d3cee03a70f1ea9b49d300bfde298cf004358d84e39d88bf6b 0.0s done
#16 exporting manifest list sha256:b6ece45f3f246f54c24f7464a02d30ba665d3c22cb459af9d15b2688cc88e70e 0.0s done
#16 naming to docker.io/library/farmish-backend:dev done
#16 unpacking to docker.io/library/farmish-backend:dev done
#16 DONE 0.1s
./scripts/smoke.sh farmish-backend:dev
smoke: migrate ok, /healthz ok, redis limits ok, /v1/me 401 -> 200 with token, /readyz 200 -> 503 on DB loss, graceful shutdown ok
ci: all checks passed
```

## Manual QA

Against `make run` with the compose stand-in (`make rustfs-up`), dev
credentials, bucket `farmish-dev` created once via the S3 API.

1. Request an upload URL:

```
POST /v1/media/upload-url  {"purpose":"listing_image","contentType":"image/jpeg","sizeBytes":2048}
→ {"expiresAt":"2026-09-26T14:01:40.389848562Z",
   "headers":{"Content-Length":"2048","Content-Type":"image/jpeg"},
   "mediaId":"a15526b9-5af5-43bf-985d-27bbf840ccc1","method":"PUT",
   "publicUrl":"http://127.0.0.1:9000/farmish-dev/listings/ff0392b8-.../fbad4c57-....jpg",
   "uploadUrl":"http://127.0.0.1:9000/farmish-dev/listings/ff0392b8-.../fbad4c57-....jpg?X-Amz-..."}
```

2. `curl -X PUT` with exactly the returned headers → **200**; a 100-byte
   body against a 2048-byte presign → **403** (control: the 2048-byte body
   → 200):

```
--- signed 2048, PUT 100 bytes
status=403
--- signed 2048, PUT 2048 bytes (control)
status=200
```

3. Tracked row, and no presigned URL in the log:

```
pending|image/jpeg|2048|listings/32656042-.../2d649e85-....jpg
grep -c "X-Amz-Signature" /tmp/api10.log → 0
```

The API server was stopped afterwards; QA rows stay in the local dev
database (media rows are safe to delete; the `audit_events` trigger still
refuses DELETE, as designed).

## Files changed

`git diff --stat b7352d5..HEAD` (34 files, +2256/-124). New files:

- `migrations/000006_media.{up,down}.sql`: `media_objects`
- `db/queries/media.sql` → generated `internal/db/media.sql.go`
- `internal/media/{media,r2,service,jobs}.go`: domain, S3 client, service, job
- `internal/media/mediatest/`: per-test bucket on the local stand-in
- `internal/media/{service,jobs,helpers}_test.go`
- `internal/http/handlers/media.go`, `internal/http/media_test.go`
- `docs/runbooks/r2.md`, `docs/adr/0016-rustfs-local-s3.md`,
  `docs/adr/0017-media-head-in-transaction.md`, this packet
- `docker-compose.yml` (rustfs), `Makefile` (`rustfs-up`, `test` now starts
  it and exports `MEDIA_TEST_S3_*`), `scripts/smoke.sh` (log read)
- `go.mod`/`go.sum`: aws-sdk-go-v2, credentials, s3, smithy-go (pinned)

## Schema changes

New migration `000006_media` (`media_objects` with the image/5 MB CHECKs
and a partial index for pending rows). Down drops the table.
`migrations_test` up→down→up passes under `make test`.

## API changes

Tag `media`: `createMediaUploadUrl` (bearer, `sensitive` rate limit) with
`CreateUploadUrlRequest` and `MediaUpload` schemas, including the signed
header map. `make api-lint` and `generate-check` pass.

## Deviations from the spec

- **ADR-0016 (owner decision):** the compose stand-in is **rustfs**, not
  MinIO — `minio/minio`, `bitnami/minio`, `quay.io/minio` and
  `dl.min.io/server/minio` are all unreachable from this machine, while
  rustfs pulls fine. The tests still run the real S3 client: presigned PUT,
  signed-length rejection (403), Head, Delete and bucket creation all
  verified. Production R2 is unchanged.
- **ADR-0017:** the storage `Head` inside `Attach` is a documented
  exception to "no external calls in a transaction" (fast read against our
  own bucket, before any row lock).
- `make minio-up` became `make rustfs-up`; `make test` starts the stand-in
  and sets `MEDIA_TEST_S3_ENDPOINT` / `MEDIA_TEST_S3_REQUIRED=1`, so media
  tests cannot silently skip.
- The stand-in has no web console, so the spec's "open publicUrl in a
  browser" QA step is replaced by an S3-API check (documented in the
  runbook).

## Open questions / risks

- Reviewer: `Attach` is written for Phase 11 (it takes a `pgx.Tx` and does
  not yet link an object to a listing; the listing-side uniqueness lives
  in `listing_images`). Nothing calls it yet outside tests.
- Reviewer: the smoke script previously used `docker logs | grep -q`,
  which trips `pipefail` once the startup log outgrows the pipe buffer.
  That is a latent bug the media startup log exposed; fixed by capturing
  the log once. The assertions are unchanged.
- The R2 console work (bucket, token, public domain, CORS, lifecycle) needs
  owner access and is Phase 22; the checklist is in `docs/runbooks/r2.md`.

## Backlog additions

None.
