-- Down of 0173: drops exactly what the up created. No data is lost (the column is derived from the offer version).
DROP INDEX bonus_grants_one_first_deposit_per_player_offer;
DROP TRIGGER bonus_grants_first_deposit_only_immutable ON bonus_grants;
DROP FUNCTION bonus_grants_first_deposit_only_immutable();
DROP TRIGGER bonus_grants_derive_first_deposit_only ON bonus_grants;
DROP FUNCTION bonus_grants_derive_first_deposit_only();
ALTER TABLE bonus_grants DROP COLUMN first_deposit_only;
