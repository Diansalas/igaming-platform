-- Reverses migration 0045.

DROP TRIGGER IF EXISTS asset_operation_eligibility_narrowing ON asset_operation_eligibility;
DROP FUNCTION IF EXISTS asset_operation_eligibility_enforce_narrowing();
DROP TRIGGER IF EXISTS asset_authorizations_narrowing ON asset_authorizations;
DROP FUNCTION IF EXISTS asset_authorizations_enforce_narrowing();

DROP TABLE IF EXISTS asset_operation_eligibility;
DROP TABLE IF EXISTS asset_authorizations;
DROP TABLE IF EXISTS platform_products;
