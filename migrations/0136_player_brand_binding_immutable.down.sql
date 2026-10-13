-- Reverses 0136 (ADR 0118; DEV/STAGING ONLY, per the migrate-down policy).
--
-- WARNING: after this, igaming_runtime can again re-home a player into another brand of the same tenant with one
-- statement (BRAND-REHOME-HOLD-1, ADR 0118 s1), re-point a wallet to another player or asset, and re-point a KYC
-- verification. Removing this control in any persistent environment requires a new forward migration and an ADR
-- amending ADR 0118; this down is a development tool.
--
-- No data pre-flight: removing a transition guard cannot make an existing row invalid, and this file touches no row.
-- 0136 changed no existing object, so the schema returns to 0135 exactly (whole-schema snapshot test:
-- internal/tenant/migration_0136_integration_test.go).

DROP TRIGGER kyc_verifications_identity_immutable ON kyc_verifications;
DROP FUNCTION kyc_verifications_enforce_identity_immutable();
DROP TRIGGER wallets_identity_immutable ON wallets;
DROP FUNCTION wallets_enforce_identity_immutable();
DROP TRIGGER player_accounts_identity_immutable ON player_accounts;
DROP FUNCTION player_accounts_enforce_identity_immutable();
