-- Phase 14 rollback. promotion_configs rows disappear with their table.
ALTER TABLE payments DROP COLUMN metadata;
ALTER TABLE listing_promotions DROP COLUMN credits_spent;
DROP TABLE promotion_configs;
