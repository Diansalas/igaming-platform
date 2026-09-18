ALTER TABLE bonus_offers DROP CONSTRAINT IF EXISTS bonus_offers_current_version_fk;
DROP TABLE IF EXISTS bonus_offer_versions;
DROP TABLE IF EXISTS bonus_offers;
DROP FUNCTION IF EXISTS bonus_offers_enforce_immutable_fields();
