DROP TABLE listing_promotions;
DROP INDEX listings_published_idx;
DROP INDEX listings_price_idx;
DROP INDEX listings_title_trgm_idx;
DROP INDEX listings_search_idx;
ALTER TABLE listings DROP COLUMN search_vector;
-- Last: other objects may use the extension.
DROP EXTENSION IF EXISTS pg_trgm;
