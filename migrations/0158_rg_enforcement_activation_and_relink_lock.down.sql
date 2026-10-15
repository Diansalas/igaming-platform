-- Down of 0158 (ADR 0115 S4-min). Refuses (RL099), BEFORE dropping anything, while any RG enforcement
-- activation exists: an activation is a recorded compliance decision and is never dropped by a
-- migration (a player-protection control is not silently switched off). The FORCE RLS of the table is
-- lifted first, inside this same transaction, so the owner's count is not RLS-blind.
ALTER TABLE rg_enforcement_activations NO FORCE ROW LEVEL SECURITY;
DO $$
DECLARE
    v_n bigint;
BEGIN
    SELECT count(*) INTO v_n FROM rg_enforcement_activations;
    IF v_n > 0 THEN
        RAISE EXCEPTION 'migration 0158 down refused: % RG enforcement activation(s) exist', v_n USING ERRCODE = 'RL099';
    END IF;
END
$$;

DROP TRIGGER player_accounts_person_relink_lock ON player_accounts;
DROP FUNCTION player_accounts_person_relink_lock();

DROP TABLE rg_enforcement_activations;
DROP FUNCTION rg_enforcement_activations_10_actor();
