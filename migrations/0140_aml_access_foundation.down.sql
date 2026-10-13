-- Reverses 0140 (ADR 0116 S3 part 1; DEV/STAGING ONLY, per the migrate-down policy).
--
-- The down REFUSES (AM099) while any AML object of a LATER migration (0141, 0142) still exists, or while any
-- aml_officer grant request / grant exists (a grant is append-only compliance history; it is never deleted to make a
-- down pass). Otherwise it removes exactly what 0140 created and returns the schema to 0136 exactly (whole-schema
-- snapshot test: internal/aml/migration_0140_integration_test.go).
--
-- FORCE ROW LEVEL SECURITY is lifted on the two reference tables only for the row removal and restored in this same
-- transaction (the 0124 pattern); a non-owner migration role cannot lift it and fails closed.

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_class WHERE relnamespace = 'public'::regnamespace AND relname LIKE 'aml\_%') THEN
        RAISE EXCEPTION 'migration 0140 down refused: AML tables of a later migration still exist; roll those back first' USING ERRCODE = 'AM099';
    END IF;
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_proc WHERE pronamespace = 'public'::regnamespace AND proname LIKE 'aml\_%'
                  AND proname NOT IN ('aml_officer_grant_request_guard', 'aml_owner_scope_of_tenant', 'aml_scope_compatible',
                                      'aml_officer_quorum', 'aml_assert_quorum', 'aml_authorize', 'aml_assert_row_scope')) THEN
        RAISE EXCEPTION 'migration 0140 down refused: AML functions of a later migration still exist; roll those back first' USING ERRCODE = 'AM099';
    END IF;
    -- Request / grant rows are visible only through their own RLS; count them as the owner with RLS lifted for this
    -- transaction (the FORCE flag is restored below in all paths because a raise rolls the whole transaction back).
    ALTER TABLE staff_capability_grant_requests NO FORCE ROW LEVEL SECURITY;
    ALTER TABLE staff_capability_grants NO FORCE ROW LEVEL SECURITY;
    IF EXISTS (SELECT 1 FROM staff_capability_grant_requests WHERE capability = 'aml_officer')
       OR EXISTS (SELECT 1 FROM staff_capability_grants WHERE capability = 'aml_officer') THEN
        RAISE EXCEPTION 'migration 0140 down refused: aml_officer grant requests or grants exist (append-only compliance history)' USING ERRCODE = 'AM099';
    END IF;
    ALTER TABLE staff_capability_grant_requests FORCE ROW LEVEL SECURITY;
    ALTER TABLE staff_capability_grants FORCE ROW LEVEL SECURITY;
END
$$;

DROP TRIGGER staff_capability_grant_requests_u_aml_officer ON staff_capability_grant_requests;
DROP FUNCTION aml_officer_grant_request_guard();
DROP FUNCTION aml_assert_row_scope(text, text);
DROP FUNCTION aml_authorize(uuid, text, text, text);
DROP FUNCTION aml_assert_quorum(uuid, text);
DROP FUNCTION aml_officer_quorum(uuid, text);
DROP FUNCTION aml_scope_compatible(text, text);
DROP FUNCTION aml_owner_scope_of_tenant(uuid);

ALTER TABLE financial_control_classifications NO FORCE ROW LEVEL SECURITY;
ALTER TABLE financial_capability_catalogue NO FORCE ROW LEVEL SECURITY;
ALTER TABLE financial_control_classifications DISABLE TRIGGER financial_control_classifications_no_delete;
DELETE FROM financial_capability_catalogue WHERE capability = 'aml_officer';
DELETE FROM financial_control_classifications WHERE operation_kind = 'aml_officer';
ALTER TABLE financial_control_classifications ENABLE TRIGGER financial_control_classifications_no_delete;
ALTER TABLE financial_control_classifications FORCE ROW LEVEL SECURITY;
ALTER TABLE financial_capability_catalogue FORCE ROW LEVEL SECURITY;

ALTER TABLE financial_capability_catalogue DROP CONSTRAINT financial_capability_catalogue_operation_kind_check;
ALTER TABLE financial_capability_catalogue ADD CONSTRAINT financial_capability_catalogue_operation_kind_check
    CHECK (operation_kind IN ('ledger_adjustment', 'payment_force_resolve', 'withdrawal_hold_resolution'));
ALTER TABLE financial_capability_catalogue DROP CONSTRAINT financial_capability_catalogue_action_check;
ALTER TABLE financial_capability_catalogue ADD CONSTRAINT financial_capability_catalogue_action_check
    CHECK (action IN ('initiate', 'request', 'approve'));
