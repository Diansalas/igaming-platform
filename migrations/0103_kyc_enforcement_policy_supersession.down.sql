-- Reverses migration 0103 ONLY while no kyc_enforcement_policies row has
-- ever recorded a supersession (superseded_by_policy_id IS NOT NULL) -
-- dropping the column would otherwise silently destroy that provenance.
-- Uses the same ADD-CONSTRAINT-CHECK(false) technique as migration
-- 0102's own down guard: constraint validation performs a full heap scan
-- that is NOT filtered by kyc_enforcement_policies' FORCE ROW LEVEL
-- SECURITY (unlike a plain SELECT executed under a restrictive policy),
-- so this guard cannot be silently defeated by RLS the way a naive
-- EXISTS-via-SELECT check was for kyc_enforcement_decisions in migration
-- 0100's own down.sql (see that file's history / ADR 0096 §16.1 B2).
DO $$
BEGIN
    ALTER TABLE kyc_enforcement_policies
        ADD CONSTRAINT kyc_enforcement_policies_down_guard
        CHECK (superseded_by_policy_id IS NULL);
    ALTER TABLE kyc_enforcement_policies DROP CONSTRAINT kyc_enforcement_policies_down_guard;
EXCEPTION WHEN check_violation THEN
    RAISE EXCEPTION 'migration 0103 (down): kyc_enforcement_policies has rows with superseded_by_policy_id set - rolling back would destroy that supersession history. Roll forward instead.';
END $$;

DROP TRIGGER kyc_enforcement_policies_check_supersession ON kyc_enforcement_policies;
DROP FUNCTION kyc_enforcement_policies_check_supersession();

-- Restore migration 0100's lifecycle trigger function body verbatim.
CREATE OR REPLACE FUNCTION kyc_enforcement_policies_enforce_lifecycle() RETURNS TRIGGER AS $$
DECLARE
    acting_principal UUID;
BEGIN
    IF TG_OP = 'TRUNCATE' THEN
        RAISE EXCEPTION 'kyc_enforcement_policies is append-only: TRUNCATE is not permitted';
    END IF;
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'kyc_enforcement_policies is append-only: DELETE is not permitted';
    END IF;
    IF (to_jsonb(NEW) - 'status') IS DISTINCT FROM (to_jsonb(OLD) - 'status') THEN
        RAISE EXCEPTION 'kyc_enforcement_policies: only status may change after insert';
    END IF;
    IF NOT (
        (OLD.status = 'draft' AND NEW.status IN ('active', 'withdrawn'))
        OR (OLD.status = 'active' AND NEW.status = 'withdrawn')
    ) THEN
        RAISE EXCEPTION 'kyc_enforcement_policies: illegal status transition % -> %', OLD.status, NEW.status;
    END IF;
    IF NEW.status = 'active' AND NEW.trigger_type NOT IN ('cumulative_deposit', 'play') THEN
        RAISE EXCEPTION 'kyc_enforcement_policies: trigger_type % is not yet evaluator-wired and may not be activated', NEW.trigger_type;
    END IF;
    acting_principal := NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid;
    IF acting_principal IS NULL THEN
        RAISE EXCEPTION 'kyc_enforcement_policies: a platform-admin principal is required to change status';
    END IF;
    IF acting_principal = OLD.created_by_actor_id THEN
        RAISE EXCEPTION 'kyc_enforcement_policies: four-eyes required - the activating/withdrawing principal must differ from the row''s creator';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

ALTER TABLE kyc_enforcement_policies DROP COLUMN superseded_by_policy_id;
