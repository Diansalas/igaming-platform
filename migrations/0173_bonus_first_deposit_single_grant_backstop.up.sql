-- 0173 (BONUS-FIRST-DEPOSIT-DOUBLE-GRANT-1, ledger-finance review condition C1): database backstop for "one
-- FirstDepositOnly Grant per (tenant, offer, player)". The application rule lives in matchesDepositEligibility
-- (internal/bonus/deposit_sweep.go); this makes a second one structurally impossible even if that rule is bypassed or
-- raced. Runtime inserts are UNCHANGED: bonus_grants.first_deposit_only is DERIVED by a BEFORE INSERT trigger from the
-- Grant's own offer version (never supplied or trusted from the caller) and a partial UNIQUE INDEX enforces the rule.
-- Status is irrelevant: a cancelled/forfeited/expired Grant still occupies the slot (no retroactive swap).
--
-- Refuses to apply (SQLSTATE BG173) while existing data would violate the index: the operator must resolve the
-- duplicate Grants explicitly (they are financial records and are never deleted or rewritten by a migration).
DO $$
DECLARE
    v_dupes bigint;
BEGIN
    -- FORCE RLS would hide every row from a session without app.tenant_id: lift it for the owner's pre-check only.
    ALTER TABLE bonus_grants NO FORCE ROW LEVEL SECURITY;
    ALTER TABLE bonus_offer_versions NO FORCE ROW LEVEL SECURITY;
    SELECT count(*) INTO v_dupes FROM (
        SELECT g.tenant_id, g.offer_id, g.player_account_id
          FROM bonus_grants g
          JOIN bonus_offer_versions ov ON ov.id = g.offer_version_id AND ov.tenant_id = g.tenant_id
         WHERE ov.first_deposit_only
         GROUP BY g.tenant_id, g.offer_id, g.player_account_id
        HAVING count(*) > 1
    ) d;
    ALTER TABLE bonus_grants FORCE ROW LEVEL SECURITY;
    ALTER TABLE bonus_offer_versions FORCE ROW LEVEL SECURITY;
    IF v_dupes > 0 THEN
        RAISE EXCEPTION 'migration 0173 refused: % (tenant, offer, player) combination(s) already hold more than one FirstDepositOnly bonus grant; resolve them explicitly first', v_dupes
            USING ERRCODE = 'BG173';
    END IF;
END
$$;

ALTER TABLE bonus_grants ADD COLUMN first_deposit_only BOOLEAN NOT NULL DEFAULT false;

ALTER TABLE bonus_grants NO FORCE ROW LEVEL SECURITY;
ALTER TABLE bonus_offer_versions NO FORCE ROW LEVEL SECURITY;
UPDATE bonus_grants g SET first_deposit_only = true
  FROM bonus_offer_versions ov
 WHERE ov.id = g.offer_version_id AND ov.tenant_id = g.tenant_id AND ov.first_deposit_only;
ALTER TABLE bonus_grants FORCE ROW LEVEL SECURITY;
ALTER TABLE bonus_offer_versions FORCE ROW LEVEL SECURITY;

CREATE FUNCTION bonus_grants_derive_first_deposit_only() RETURNS TRIGGER AS $$
BEGIN
    SELECT ov.first_deposit_only INTO NEW.first_deposit_only
      FROM bonus_offer_versions ov
     WHERE ov.id = NEW.offer_version_id AND ov.tenant_id = NEW.tenant_id;
    NEW.first_deposit_only := COALESCE(NEW.first_deposit_only, false);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER bonus_grants_derive_first_deposit_only
    BEFORE INSERT ON bonus_grants
    FOR EACH ROW EXECUTE FUNCTION bonus_grants_derive_first_deposit_only();

CREATE FUNCTION bonus_grants_first_deposit_only_immutable() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.first_deposit_only IS DISTINCT FROM OLD.first_deposit_only THEN
        RAISE EXCEPTION 'bonus_grants.first_deposit_only is derived at insert and immutable';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER bonus_grants_first_deposit_only_immutable
    BEFORE UPDATE ON bonus_grants
    FOR EACH ROW EXECUTE FUNCTION bonus_grants_first_deposit_only_immutable();

CREATE UNIQUE INDEX bonus_grants_one_first_deposit_per_player_offer
    ON bonus_grants (tenant_id, offer_id, player_account_id)
    WHERE first_deposit_only;
