-- Exact inverse of 0082_stage9_db_hardening.up.sql, in reverse order.
-- Every statement here restores the pre-0082 schema state precisely: the
-- three indexes are dropped, each replaced foreign key is restored to its
-- original auto-generated name and definition (so a subsequent re-run of
-- the up migration finds exactly what it expects to DROP), and every
-- trigger/function this migration created is dropped. Nothing in this
-- migration created or destroyed a row, so there is no data to restore.
--
-- ledger_deny_mutation() itself is NOT dropped: it is migration 0021's
-- function, shared by a dozen other tables, and dropping it here would
-- destroy their guards too.

-- 3. Pagination indexes
DROP INDEX idx_ledger_transactions_tenant_time;
DROP INDEX idx_sportsbook_bets_tenant_time;
DROP INDEX idx_casino_launch_sessions_tenant_time;

-- 2. ARCH-DB-3 composite brand pinning
ALTER TABLE jurisdiction_resolutions DROP CONSTRAINT jurisdiction_resolutions_player_tenant_brand_fkey;

ALTER TABLE kyc_documents DROP CONSTRAINT kyc_documents_player_tenant_brand_fkey;
ALTER TABLE kyc_documents
    ADD CONSTRAINT kyc_documents_brand_id_tenant_id_fkey
    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id);

ALTER TABLE kyc_verifications DROP CONSTRAINT kyc_verifications_player_tenant_brand_fkey;
ALTER TABLE kyc_verifications
    ADD CONSTRAINT kyc_verifications_brand_id_tenant_id_fkey
    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id);

ALTER TABLE bonus_grant_progress DROP CONSTRAINT bonus_grant_progress_player_tenant_brand_fkey;
ALTER TABLE bonus_grant_progress
    ADD CONSTRAINT bonus_grant_progress_brand_id_tenant_id_fkey
    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id);

ALTER TABLE bonus_held_dispositions DROP CONSTRAINT bonus_held_dispositions_player_tenant_brand_fkey;
ALTER TABLE bonus_held_dispositions
    ADD CONSTRAINT bonus_held_dispositions_brand_id_tenant_id_fkey
    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id);

ALTER TABLE deposit_intents DROP CONSTRAINT deposit_intents_player_tenant_brand_fkey;
ALTER TABLE deposit_intents
    ADD CONSTRAINT deposit_intents_brand_id_tenant_id_fkey
    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id);

ALTER TABLE withdrawal_requests DROP CONSTRAINT withdrawal_requests_player_tenant_brand_fkey;
ALTER TABLE withdrawal_requests
    ADD CONSTRAINT withdrawal_requests_brand_id_tenant_id_fkey
    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id);

-- 1. Immutability + TRUNCATE-deny triggers
DROP TRIGGER ledger_accounts_no_truncate ON ledger_accounts;
DROP TRIGGER ledger_accounts_immutable_fields ON ledger_accounts;
DROP FUNCTION ledger_accounts_enforce_immutable_fields();

DROP TRIGGER login_attempts_no_truncate ON login_attempts;
DROP TRIGGER login_attempts_immutable ON login_attempts;

DROP TRIGGER reconciliation_mismatches_no_truncate ON reconciliation_mismatches;
DROP TRIGGER reconciliation_mismatches_immutable_fields ON reconciliation_mismatches;
DROP FUNCTION reconciliation_mismatches_enforce_immutable_fields();

DROP TRIGGER reconciliation_runs_no_truncate ON reconciliation_runs;
DROP TRIGGER reconciliation_runs_immutable ON reconciliation_runs;

DROP TRIGGER deposit_intents_no_truncate ON deposit_intents;
DROP TRIGGER deposit_intents_immutable_fields ON deposit_intents;
DROP FUNCTION deposit_intents_enforce_immutable_fields();

DROP TRIGGER sportsbook_bets_no_truncate ON sportsbook_bets;
DROP TRIGGER sportsbook_bets_immutable_fields ON sportsbook_bets;
DROP FUNCTION sportsbook_bets_enforce_immutable_fields();
