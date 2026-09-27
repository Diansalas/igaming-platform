-- PRH-I3 fix round 4 (security F3, DR-PRHI3-07, docs/decisions/0096-kyc-
-- enforcement-boundary.md §17.3): DB-level closure of "withdrawing an
-- ACTIVE kyc_enforcement_policies row must not be able to leave the key
-- with no active successor, as a single actor's decision".
--
-- Migration 0100 is already applied and its up.sql is checksummed - this
-- migration NEVER edits 0100's file. It extends 0100's own lifecycle
-- trigger function in place (CREATE OR REPLACE FUNCTION, same function
-- name, same triggers already attached to it in 0100 - no new trigger
-- object is bound for the BEFORE-UPDATE check), and adds one new AFTER,
-- DEFERRED constraint trigger for the part of the check that can only be
-- evaluated once the rest of the transaction has run.
--
-- Design (as proposed in ADR 0096 §17.3):
--   1. A nullable superseded_by_policy_id column, set exactly once, only
--      on an active -> withdrawn transition, naming the replacement row.
--   2. The existing BEFORE-UPDATE lifecycle trigger (unchanged four-eyes
--      and transition-legality checks) additionally requires
--      superseded_by_policy_id to be set on active -> withdrawn, and
--      forbids it being set on any other transition.
--   3. A NEW deferred (checked at COMMIT, not at statement time) AFTER
--      trigger confirms, once the whole transaction has run its course,
--      that the named successor: exists, is 'active', is NOT the row
--      being withdrawn itself, and shares the withdrawn row's exact
--      enforcement key (licensing_jurisdiction_id, trigger_type,
--      play_operation, asset_code) - so it cannot be satisfied by
--      pointing at an arbitrary row, an unrelated policy, or a row that
--      is still 'draft' by commit time. Deferred checking is required
--      because kyc.SupersedeEnforcementPolicy's own sanctioned ordering
--      (create the replacement as draft, THEN withdraw the old row
--      naming it, THEN activate the replacement) means the successor is
--      genuinely not yet 'active' at the moment the withdrawal statement
--      itself runs - only by the time the transaction commits.
--
-- Net effect: kyc.WithdrawEnforcementPolicy called standalone against an
-- ACTIVE row (never setting superseded_by_policy_id) is refused by the
-- database itself, not by convention. kyc.SupersedeEnforcementPolicy's
-- atomic withdraw+create+activate sequence, using the ordering above,
-- satisfies both the immediate and the deferred check.
--
-- Approver != requester (four-eyes) for both the withdrawal step and the
-- replacement's own creation/activation is UNCHANGED - already enforced
-- by 0100's existing acting-principal-vs-created_by_actor_id check,
-- itself unchanged by this migration, and it continues to be derived
-- only from the app.platform_admin_principal_id session GUC, never an
-- application-supplied column.

ALTER TABLE kyc_enforcement_policies
    ADD COLUMN superseded_by_policy_id UUID REFERENCES kyc_enforcement_policies (id);

COMMENT ON COLUMN kyc_enforcement_policies.superseded_by_policy_id IS 'Set exactly once, only on an active -> withdrawn transition (enforced by kyc_enforcement_policies_enforce_lifecycle), naming the replacement policy that must be ''active'', for the same enforcement key, by commit time (enforced by the deferred kyc_enforcement_policies_check_supersession constraint trigger). NULL on every draft/active row and on any withdrawal of a still-draft row.';

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
    IF (to_jsonb(NEW) - 'status' - 'superseded_by_policy_id') IS DISTINCT FROM (to_jsonb(OLD) - 'status' - 'superseded_by_policy_id') THEN
        RAISE EXCEPTION 'kyc_enforcement_policies: only status and superseded_by_policy_id may change after insert';
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
    -- Security F3 (ADR 0096 §17.3): superseded_by_policy_id may be set
    -- (NULL -> non-null) if and only if this is specifically the
    -- active -> withdrawn transition. It is immutable in every other
    -- direction (in particular, a draft row being withdrawn - never
    -- having been enforced - carries no successor requirement).
    IF NEW.superseded_by_policy_id IS DISTINCT FROM OLD.superseded_by_policy_id THEN
        IF NOT (OLD.status = 'active' AND NEW.status = 'withdrawn') THEN
            RAISE EXCEPTION 'kyc_enforcement_policies: superseded_by_policy_id may only be set on an active -> withdrawn transition';
        END IF;
        IF OLD.superseded_by_policy_id IS NOT NULL THEN
            RAISE EXCEPTION 'kyc_enforcement_policies: superseded_by_policy_id is already set and may not change';
        END IF;
    END IF;
    IF OLD.status = 'active' AND NEW.status = 'withdrawn' THEN
        IF NEW.superseded_by_policy_id IS NULL THEN
            RAISE EXCEPTION 'kyc_enforcement_policies: withdrawing an ACTIVE policy requires supersession - set superseded_by_policy_id to an approved, activating replacement in the same transaction (see kyc.SupersedeEnforcementPolicy)';
        END IF;
        IF NEW.superseded_by_policy_id = NEW.id THEN
            RAISE EXCEPTION 'kyc_enforcement_policies: a policy cannot supersede itself';
        END IF;
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

-- Deferred check: by COMMIT time, the named successor must exist, be
-- 'active', not be the withdrawn row itself, and share the withdrawn
-- row's exact enforcement key. Deferred (not BEFORE-statement-time)
-- because kyc.SupersedeEnforcementPolicy authors the replacement as
-- 'draft' BEFORE withdrawing the old row (the unique partial index
-- kyc_enforcement_policies_one_active forbids two simultaneously-active
-- rows for the same key, so the replacement cannot already be 'active'
-- at the moment the old row is withdrawn) and only activates it
-- afterward, later in the same transaction.
CREATE FUNCTION kyc_enforcement_policies_check_supersession() RETURNS TRIGGER AS $$
DECLARE
    successor kyc_enforcement_policies%ROWTYPE;
BEGIN
    IF NEW.superseded_by_policy_id IS NULL THEN
        RETURN NULL;
    END IF;
    SELECT * INTO successor FROM kyc_enforcement_policies WHERE id = NEW.superseded_by_policy_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'kyc_enforcement_policies: superseding policy % does not exist by commit', NEW.superseded_by_policy_id;
    END IF;
    IF successor.status <> 'active' THEN
        RAISE EXCEPTION 'kyc_enforcement_policies: superseding policy % must be active by commit (is %)', successor.id, successor.status;
    END IF;
    IF successor.licensing_jurisdiction_id IS DISTINCT FROM NEW.licensing_jurisdiction_id
        OR successor.trigger_type IS DISTINCT FROM NEW.trigger_type
        OR COALESCE(successor.play_operation, '') IS DISTINCT FROM COALESCE(NEW.play_operation, '')
        OR COALESCE(successor.asset_code, '') IS DISTINCT FROM COALESCE(NEW.asset_code, '') THEN
        RAISE EXCEPTION 'kyc_enforcement_policies: superseding policy % does not share the withdrawn policy''s enforcement key', successor.id;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE CONSTRAINT TRIGGER kyc_enforcement_policies_check_supersession
    AFTER UPDATE ON kyc_enforcement_policies
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
    WHEN (OLD.status = 'active' AND NEW.status = 'withdrawn')
    EXECUTE FUNCTION kyc_enforcement_policies_check_supersession();

COMMENT ON FUNCTION kyc_enforcement_policies_check_supersession() IS 'ADR 0096 §17.3 / security F3: deferred (commit-time) half of the supersession check - the BEFORE-UPDATE lifecycle trigger requires superseded_by_policy_id to be SET; this confirms, once the transaction has finished, that it names a genuinely active, key-matching, non-self replacement, closing the "point at an arbitrary/draft row" gap.';
