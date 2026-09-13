DROP TABLE IF EXISTS wallets;
ALTER TABLE player_accounts DROP CONSTRAINT IF EXISTS player_accounts_id_tenant_brand_key;
