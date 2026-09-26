-- Phase 13b rollback. forbid_mutation() belongs to migration 000004 and
-- is intentionally left in place.
DROP TRIGGER IF EXISTS ledger_entries_append_only ON ledger_entries;
DROP TRIGGER IF EXISTS ledger_transactions_append_only ON ledger_transactions;
DROP TRIGGER IF EXISTS ledger_accounts_append_only ON ledger_accounts;
DROP TRIGGER IF EXISTS ledger_entries_balanced ON ledger_entries;
DROP TABLE IF EXISTS ledger_entries;
DROP TABLE IF EXISTS ledger_transactions;
DROP TABLE IF EXISTS ledger_accounts;
DROP FUNCTION IF EXISTS ledger_check_balanced();
