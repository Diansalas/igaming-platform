ALTER TABLE bonus_campaigns DROP CONSTRAINT IF EXISTS bonus_campaigns_current_version_fk;
DROP TABLE IF EXISTS bonus_campaign_versions;
DROP TABLE IF EXISTS bonus_campaigns;
