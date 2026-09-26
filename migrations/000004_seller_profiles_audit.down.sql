DROP TRIGGER audit_events_append_only ON audit_events;
DROP TABLE audit_events;
DROP TRIGGER set_updated_at ON seller_profiles;
DROP TABLE seller_profiles;
DROP FUNCTION forbid_mutation();
