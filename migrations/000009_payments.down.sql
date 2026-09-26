-- Phase 13a: payments and webhook events.
DROP TABLE IF EXISTS webhook_events;
DROP TRIGGER IF EXISTS set_updated_at ON payments;
DROP TABLE IF EXISTS payments;
