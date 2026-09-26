-- Media objects (Phase 10): one row per client upload to R2 (or the local
-- S3 stand-in). Rows start pending and become attached when a listing uses
-- the object (Phase 11); CleanupOrphans deletes stale pending ones.
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
-- The orphan sweep only ever looks at pending rows.
CREATE INDEX media_objects_pending_idx ON media_objects (created_at) WHERE status = 'pending';
CREATE INDEX media_objects_owner_idx ON media_objects (owner_id);
