-- Reverses 0137 (ADR 0119 S2; DEV/STAGING ONLY, per the migrate-down policy).
--
-- Refuses (SF097), BEFORE dropping anything, while any MFA row exists (a factor, a recovery code, a login challenge
-- or an enrolment token): enrolled second factors and their audit trail are never dropped by a migration. The FORCE
-- ROW LEVEL SECURITY of the four tables is lifted first, inside this same transaction, so the owner's counts are not
-- RLS-blind (the tables are dropped right after; a refusal rolls the lift back).
--
-- WARNING: after this, staff MFA has no storage. Nothing in the running platform reads these tables before S3, so on
-- the S2 tree this is harmless; once S3 has merged, the down must not be run against an environment whose staff
-- have enrolled. Removing this control in any persistent environment needs a new forward migration and an ADR
-- amendment.
--
-- 0137 changed no existing object, so the schema returns to 0136 exactly (whole-schema snapshot test:
-- internal/staffmfa/store/migration_0137_integration_test.go).
ALTER TABLE staff_mfa_factors NO FORCE ROW LEVEL SECURITY;
ALTER TABLE staff_mfa_recovery_codes NO FORCE ROW LEVEL SECURITY;
ALTER TABLE staff_auth_challenges NO FORCE ROW LEVEL SECURITY;
ALTER TABLE staff_enrolment_tokens NO FORCE ROW LEVEL SECURITY;
DO $$
DECLARE
    v_factors bigint; v_codes bigint; v_challenges bigint; v_tokens bigint;
BEGIN
    SELECT count(*) INTO v_factors FROM staff_mfa_factors;
    SELECT count(*) INTO v_codes FROM staff_mfa_recovery_codes;
    SELECT count(*) INTO v_challenges FROM staff_auth_challenges;
    SELECT count(*) INTO v_tokens FROM staff_enrolment_tokens;
    IF v_factors + v_codes + v_challenges + v_tokens > 0 THEN
        RAISE EXCEPTION 'migration 0137 down refused: staff MFA data exists (factors=%, recovery_codes=%, challenges=%, enrolment_tokens=%). Nothing was changed; staff MFA data is never dropped by a migration (ADR 0119); on a local / dev database rebuild from migrations.',
            v_factors, v_codes, v_challenges, v_tokens USING ERRCODE = 'SF097';
    END IF;
END
$$;

DROP TABLE staff_enrolment_tokens;
DROP TABLE staff_auth_challenges;
DROP TABLE staff_mfa_recovery_codes;
DROP TABLE staff_mfa_factors;
DROP FUNCTION sf_enrolment_token_guard();
DROP FUNCTION sf_challenge_guard();
DROP FUNCTION sf_recovery_guard();
DROP FUNCTION sf_factor_guard();
DROP FUNCTION sf_deny_truncate();
DROP FUNCTION sf_deny_delete();
DROP FUNCTION sf_force_tenant_from_staff();
