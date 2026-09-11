ALTER TABLE assets DROP CONSTRAINT IF EXISTS assets_network_required_for_crypto;
ALTER TABLE assets DROP COLUMN IF EXISTS network;
