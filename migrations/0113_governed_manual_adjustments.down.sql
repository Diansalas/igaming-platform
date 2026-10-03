-- MUST RUN INSIDE A SINGLE TRANSACTION (db.MigrateDown does this). Run by
-- hand under psql autocommit, a MA099 refusal below would leave FORCE ROW
-- LEVEL SECURITY lifted for the owner on the checked tables. By hand, use
-- `psql --single-transaction -v ON_ERROR_STOP=1 -f <this file>`.
--
-- Reverses 0113 (ADR 0100 §10.9). Refuses (MA099) while any row exists in
-- the requests, approvals, policies, policy changes or profile tables, or
-- while any ledger_unlinked_manual_adjustment mismatch exists - so once a
-- governed adjustment or a detector finding exists, 0113 is effectively
-- irreversible (accepted by ledger-finance, confirmation §3). Otherwise it
-- restores 0107's kind CHECK exactly, drops the fences, the acting
-- policies, the new tables and functions, and the FK added to 0112's
-- catalogue. The pre-existing ledger policies are left byte-identical
-- (0113 only ADDED policies and triggers on them).

-- Every one of these tables is FORCE ROW LEVEL SECURITY, which binds the
-- owning migration role too: with no session GUCs the EXISTS checks below
-- would see ZERO rows and the down would silently drop live governed data.
-- Lift FORCE first (the owner then bypasses RLS) so the refusal is real;
-- if it fires, the whole down rolls back and FORCE is restored with it.
ALTER TABLE ledger_adjustment_requests NO FORCE ROW LEVEL SECURITY;
ALTER TABLE ledger_adjustment_approvals NO FORCE ROW LEVEL SECURITY;
ALTER TABLE financial_approval_policies NO FORCE ROW LEVEL SECURITY;
ALTER TABLE financial_approval_policy_changes NO FORCE ROW LEVEL SECURITY;
ALTER TABLE financial_approval_policy_change_approvals NO FORCE ROW LEVEL SECURITY;
ALTER TABLE tenant_financial_policy_profiles NO FORCE ROW LEVEL SECURITY;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM ledger_adjustment_requests)
       OR EXISTS (SELECT 1 FROM ledger_adjustment_approvals)
       OR EXISTS (SELECT 1 FROM financial_approval_policies)
       OR EXISTS (SELECT 1 FROM financial_approval_policy_changes)
       OR EXISTS (SELECT 1 FROM financial_approval_policy_change_approvals)
       OR EXISTS (SELECT 1 FROM tenant_financial_policy_profiles) THEN
        RAISE EXCEPTION '0113 down refused: governed manual adjustment / policy rows exist' USING ERRCODE = 'MA099';
    END IF;
END $$;

-- The mismatch check cannot be RLS-hidden from the owner: reconciliation_
-- mismatches is FORCE RLS (tenant-scoped), so the count below would be 0
-- for a migration session with no app.tenant_id. Temporarily lifting FORCE
-- for the owner makes the refusal real.
ALTER TABLE reconciliation_mismatches NO FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM reconciliation_mismatches WHERE mismatch_kind = 'ledger_unlinked_manual_adjustment') THEN
        RAISE EXCEPTION '0113 down refused: ledger_unlinked_manual_adjustment mismatches exist' USING ERRCODE = 'MA099';
    END IF;
END $$;
ALTER TABLE reconciliation_mismatches FORCE ROW LEVEL SECURITY;

ALTER TABLE reconciliation_mismatches DROP CONSTRAINT reconciliation_mismatches_mismatch_kind_check;
ALTER TABLE reconciliation_mismatches ADD CONSTRAINT reconciliation_mismatches_mismatch_kind_check
    CHECK (mismatch_kind IN (
        'missing_projection', 'balance_mismatch',
        'sb_locked_mismatch', 'sb_bet_net_mismatch', 'sb_orphan_ledger', 'sb_orphan_history',
        'sb_status_mismatch', 'sb_mock_statement_mismatch',
        'cas_round_binding_mismatch', 'cas_posting_shape_mismatch', 'cas_orphan_win',
        'cas_rollback_linkage_mismatch', 'cas_tombstone_conflict', 'cas_unposted_provider_event',
        'cas_tombstone_late_original', 'cas_mock_statement_mismatch',
        'pay_missing_platform_record', 'pay_missing_provider_record', 'pay_amount_mismatch',
        'pay_asset_mismatch', 'pay_reference_mismatch', 'pay_status_mismatch', 'pay_duplicate',
        'pay_unresolved', 'pay_captured_unposted'
    ));

-- Acting permissive policies on existing tables.
DROP POLICY acting_read ON player_accounts;
DROP POLICY acting_read ON wallets;
DROP POLICY acting_read ON ledger_accounts;
DROP POLICY acting_insert ON ledger_accounts;
DROP POLICY acting_read ON ledger_transactions;
DROP POLICY acting_insert ON ledger_transactions;
DROP POLICY acting_lock ON ledger_transactions;
DROP POLICY acting_read ON ledger_entries;
DROP POLICY acting_insert ON ledger_entries;
DROP POLICY acting_read ON wallet_balance_projection;
DROP POLICY acting_insert ON wallet_balance_projection;
DROP POLICY acting_update ON wallet_balance_projection;
DROP POLICY acting_read ON payment_attempts;
DROP POLICY acting_read ON deposit_intents;
DROP POLICY acting_read_own_licence ON licences;
DROP POLICY acting_read ON asset_authorizations;
DROP POLICY acting_read ON staff_capability_grant_requests;

-- Fences.
DROP TRIGGER ledger_transactions_governed_fence ON ledger_transactions;
DROP TRIGGER ledger_entries_governed_fence ON ledger_entries;
DROP TRIGGER wallet_balance_projection_acting_fence ON wallet_balance_projection;
DROP FUNCTION ledger_transactions_governed_fence();
DROP FUNCTION ledger_entries_governed_fence();
DROP FUNCTION wallet_balance_projection_acting_fence();
DROP FUNCTION ledger_governed_fence_allows(uuid, text, text, uuid, text, text);

-- K2-G1 restrictive fences.
DROP POLICY acting_fence_select ON asset_operation_eligibility;
DROP POLICY acting_fence_select ON open_bet_self_exclusion_policies;

-- Functions taking a new table's row type depend on it: drop them first.
DROP FUNCTION ledger_adjustment_verify_link(ledger_adjustment_requests);
DROP FUNCTION ledger_adjustment_payload_hash(ledger_adjustment_requests);
DROP FUNCTION financial_approval_policy_change_tightens(financial_approval_policy_changes);
DROP FUNCTION financial_approval_policy_change_content_hash(financial_approval_policy_changes);

-- New tables (dependency order) and their functions.
DROP TABLE ledger_adjustment_approvals;
DROP TABLE ledger_adjustment_requests;
DROP TABLE tenant_financial_policy_profiles;
DROP TABLE financial_approval_policies;
DROP TABLE financial_approval_policy_change_approvals;
DROP TABLE financial_approval_policy_changes;

DROP FUNCTION ledger_adjustment_execution_status(uuid);
DROP FUNCTION ledger_adjustment_approvals_apply_reject();
DROP FUNCTION ledger_adjustment_approvals_beneficiary_guard();
DROP FUNCTION ledger_adjustment_approvals_guard();
DROP FUNCTION ledger_adjustment_requests_no_executing_commit();
DROP FUNCTION ledger_adjustment_requests_beneficiary_guard();
DROP FUNCTION ledger_adjustment_requests_guard();
DROP FUNCTION ledger_adjustment_live_person(uuid, uuid);
DROP FUNCTION ledger_adjustment_invisible_platform_grant(uuid, uuid, text, uuid);
DROP FUNCTION ledger_adjustment_eligible_grant(uuid, uuid, text);
DROP FUNCTION ledger_adjustment_payload_refusal(uuid, uuid, uuid, uuid, text, text, numeric, text, uuid, text, text);
DROP FUNCTION ledger_adjustment_asset_suspended(uuid, text);
DROP FUNCTION player_open_payment_exposure(uuid, uuid);
DROP FUNCTION financial_policy_author_persons(uuid[]);
DROP FUNCTION financial_policy_required_approvals(text, uuid, uuid, text, numeric, timestamptz);
DROP FUNCTION financial_approval_policy_change_approvals_apply();
DROP FUNCTION tenant_financial_policy_profiles_guard();
DROP FUNCTION financial_approval_policies_guard();
DROP FUNCTION financial_approval_policy_change_approvals_guard();
DROP FUNCTION financial_approval_policy_changes_guard();
DROP FUNCTION financial_approval_policy_predecessor(text, text, uuid, uuid, uuid, text, text, timestamptz);
DROP FUNCTION financial_policy_actor_has_permission(text, text);
DROP FUNCTION financial_policy_is_tightening(int, numeric, int, int, numeric, int);
DROP FUNCTION ledger_adjustment_note_hash(text);
DROP FUNCTION k2_sha256_hex(text);
DROP FUNCTION k2_canonical(text[]);

ALTER TABLE financial_capability_catalogue DROP CONSTRAINT financial_capability_catalogue_operation_kind_fkey;
DROP TABLE ledger_adjustment_reason_codes;
DROP TABLE financial_control_classifications;
