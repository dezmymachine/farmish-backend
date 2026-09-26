-- Phase 15a rollback. forbid_mutation() belongs to migration 000004 and is
-- intentionally left in place.
ALTER TABLE ledger_entries DROP CONSTRAINT IF EXISTS ledger_entries_order_fk;
DROP TRIGGER IF EXISTS order_events_append_only ON order_events;
DROP TRIGGER IF EXISTS orders_set_updated_at ON orders;
DROP TRIGGER IF EXISTS checkouts_set_updated_at ON checkouts;
DROP TRIGGER IF EXISTS commission_configs_set_updated_at ON commission_configs;
DROP TABLE IF EXISTS order_events;
DROP TABLE IF EXISTS order_items;
DROP TABLE IF EXISTS orders;
DROP TABLE IF EXISTS checkouts;
DROP TABLE IF EXISTS commission_configs;
