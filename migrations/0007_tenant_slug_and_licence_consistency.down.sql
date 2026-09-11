ALTER TABLE tenants DROP CONSTRAINT IF EXISTS tenants_licence_matches_model;
ALTER TABLE tenants DROP COLUMN IF EXISTS expected_licensee;
ALTER TABLE licences DROP CONSTRAINT IF EXISTS licences_id_licensee_key;
