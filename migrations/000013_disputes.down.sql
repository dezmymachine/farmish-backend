-- Phase 16 rollback.
DROP TRIGGER IF EXISTS disputes_set_updated_at ON disputes;
DROP TABLE IF EXISTS disputes;
