-- Extensions used across the schema.
CREATE EXTENSION IF NOT EXISTS pgcrypto; -- gen_random_uuid(), digest()
CREATE EXTENSION IF NOT EXISTS citext;   -- case-insensitive emails/slugs

-- Attach to any table with an updated_at column:
--   CREATE TRIGGER set_updated_at BEFORE UPDATE ON <table>
--     FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  NEW.updated_at = now();
  RETURN NEW;
END;
$$;
