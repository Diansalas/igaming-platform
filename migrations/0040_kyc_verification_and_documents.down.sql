COMMENT ON COLUMN player_accounts.verified_at IS 'Hook for the Stage 4 KYC/AML subsystem - not enforced or interpreted by anything in Stage 2.';

DROP TRIGGER IF EXISTS player_credential_tokens_no_truncate ON player_credential_tokens;
DROP TRIGGER IF EXISTS player_credential_tokens_no_delete ON player_credential_tokens;
DROP FUNCTION IF EXISTS player_credential_tokens_deny_delete();
DROP TABLE IF EXISTS player_credential_tokens;

DROP TRIGGER IF EXISTS kyc_documents_no_truncate ON kyc_documents;
DROP TRIGGER IF EXISTS kyc_documents_deny_delete ON kyc_documents;
DROP TRIGGER IF EXISTS kyc_documents_immutable_core ON kyc_documents;
DROP FUNCTION IF EXISTS kyc_documents_enforce_immutability();
DROP TABLE IF EXISTS kyc_documents;

DROP TRIGGER IF EXISTS kyc_verifications_no_truncate ON kyc_verifications;
DROP TRIGGER IF EXISTS kyc_verifications_no_delete ON kyc_verifications;
DROP FUNCTION IF EXISTS kyc_verifications_deny_delete();
DROP TABLE IF EXISTS kyc_verifications;

ALTER TABLE staff_users DROP CONSTRAINT IF EXISTS staff_users_id_tenant_unique;
ALTER TABLE player_accounts DROP CONSTRAINT IF EXISTS player_accounts_id_tenant_unique;
