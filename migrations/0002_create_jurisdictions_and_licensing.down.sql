DROP TABLE IF EXISTS tenant_jurisdiction_configs;
ALTER TABLE tenants DROP COLUMN IF EXISTS licence_id;
DROP TABLE IF EXISTS licences;
DROP TABLE IF EXISTS jurisdictions;
