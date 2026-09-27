-- Phase 17a rollback.
ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_refunded_within_base;
DROP TRIGGER IF EXISTS refunds_set_updated_at ON refunds;
DROP TABLE IF EXISTS refunds;
