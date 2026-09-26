# ADR-0016: rustfs replaces MinIO as the local S3 stand-in

- **Status:** Accepted (Phase 10). Owner decision: "Use rustfs as the local
  S3 stand-in".
- **Date:** 2026-09-26

## Context

Phase 10 tests the real `media.R2` client against a local S3-compatible
server; the spec names MinIO. On this machine MinIO cannot be installed:

```
$ docker pull minio/minio:latest
Error response from daemon: pull access denied for minio/minio,
repository does not exist or may require 'docker login'
$ docker manifest inspect bitnami/minio:latest   # also blocked
$ docker manifest inspect quay.io/minio/minio   # 401 Unauthorized
```

MinIO has withdrawn its public images from Docker Hub, and the registries
that still host them are unreachable here. Other images (postgres, redis,
hello-world) pull fine, so this is MinIO availability, not a broken network.

## Decision

Use **rustfs** (`rustfs/rustfs`, pinned `1.0.0-alpha.44`) as the local
stand-in, exposed as the `rustfs` compose service on `127.0.0.1:9000` with
dev credentials (`farmish` / `farmish-secret`) and `make rustfs-up`.

## Consequences

- The tests still exercise the real `aws-sdk-go-v2` S3 client end to end:
  presigned PUT, signed `Content-Type`/`Content-Length` enforcement (a wrong
  body size is rejected with 403, exactly like MinIO), `HeadObject`,
  `DeleteObject` and bucket creation.
- Production is unchanged: Cloudflare R2 over the same S3 API, selected by
  leaving `R2_ENDPOINT` unset (the API refuses it when deployed).
- The compose service has no web console, so the spec's "open the publicUrl
  in a browser" QA step needs a public-read bucket policy; the runbook says
  how to check the object instead.
