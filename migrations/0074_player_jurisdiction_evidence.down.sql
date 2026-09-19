-- Reverses migration 0074.

DROP TRIGGER IF EXISTS jurisdiction_evidence_collection_active_no_truncate ON jurisdiction_evidence_collection_active;
DROP FUNCTION IF EXISTS jurisdiction_evidence_collection_active_deny_truncate();

DROP TRIGGER IF EXISTS jurisdiction_evidence_collection_active_updated_at ON jurisdiction_evidence_collection_active;
DROP FUNCTION IF EXISTS jurisdiction_evidence_collection_active_touch_updated_at();

DROP POLICY IF EXISTS jurisdiction_evidence_collection_active_tenant_isolation_update ON jurisdiction_evidence_collection_active;
DROP POLICY IF EXISTS jurisdiction_evidence_collection_active_tenant_isolation_insert ON jurisdiction_evidence_collection_active;
DROP POLICY IF EXISTS jurisdiction_evidence_collection_active_tenant_isolation_read ON jurisdiction_evidence_collection_active;

DROP TABLE IF EXISTS jurisdiction_evidence_collection_active;

ALTER TABLE kyc_verifications
    DROP CONSTRAINT IF EXISTS kyc_verifications_verified_residence_set_by_fk,
    DROP CONSTRAINT IF EXISTS kyc_verifications_verified_residence_source_pair,
    DROP CONSTRAINT IF EXISTS kyc_verifications_verified_residence_setby_pair,
    DROP CONSTRAINT IF EXISTS kyc_verifications_verified_residence_country_pair;

ALTER TABLE kyc_verifications
    DROP COLUMN IF EXISTS verified_residence_set_at,
    DROP COLUMN IF EXISTS verified_residence_set_by,
    DROP COLUMN IF EXISTS verified_residence_source,
    DROP COLUMN IF EXISTS verified_residence_country;

ALTER TABLE player_accounts
    DROP CONSTRAINT IF EXISTS player_accounts_declared_residence_pair;

ALTER TABLE player_accounts
    DROP COLUMN IF EXISTS declared_residence_captured_at,
    DROP COLUMN IF EXISTS declared_residence_country;
