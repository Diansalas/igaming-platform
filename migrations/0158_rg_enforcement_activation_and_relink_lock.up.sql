-- ADR 0115 SLICE S4-min (completion audit A6): two database controls the enforcement needs.
--
-- NUMBERING: 0158 (0153-0157 are held by other in-flight work; 0159 is reserved and unused here).
--
-- 1. rg_enforcement_activations: the per-tenant / per-brand ENABLE of the RG LIMIT enforcement
--    (deposit / loss / wager limits and the rg_config_missing / rg_limit_required fail-closed answers
--    of ADR 0115 section 6). OWNER DECISION 2026-10-15: the enforcement ships DISABLED by default,
--    because under the OQ-1 default a brand with no compliance-supplied floor (OQ-2) could neither
--    take a deposit nor a wager. Self-exclusion and time-out are NOT behind this switch: they need
--    no configuration and are always enforced (internal/rg EvaluateEligibility).
--
--    The switch is a ONE-WAY RATCHET and that is the point: the table is append-only (UPDATE,
--    DELETE and TRUNCATE are refused, RL009), a row can only ENABLE, and there is no disabling row.
--    Turning a player-protection control off is therefore not a software operation: it needs a
--    recorded decision and a migration. (CLAUDE.md: enforcement is not optional and a quiet change
--    must be impossible.) One row per (tenant, brand) scope: a tenant-wide row (brand_id NULL)
--    covers every brand of the tenant; a duplicate is refused by the unique key (23505), which the
--    Go layer reads as "already enabled".
--
--    Written by an ACTIVE STAFF PRINCIPAL OF THE SAME TENANT only (rg_actor_session, the operator
--    configuration rule of 0135: a platform principal, an acting session, a player and a service
--    session are refused, RL001); id, actor, person and time are set by the trigger, never by the
--    caller. Read by the tenant only (RLS). FORCE RLS; the runtime role gets SELECT, INSERT only.
--
-- 2. player_accounts person re-link lock (ADR 0115 S3 hard condition (d)). player_accounts.person_id
--    is the key of every Person-scope RG control (self-exclusion, time-out, tenant_person limit
--    aggregation) and stays mutable (PERSON-RELINK-PIN-1 is an open identity-owner decision). No Go
--    code re-links today, but the runtime role can UPDATE the column. A re-link now takes the L0.4
--    person advisory locks of the OLD and the NEW Person (the exact key internal/rg lockPerson uses,
--    lower hash first so two re-links cannot invert), held to the end of the re-linking transaction.
--    So a re-link waits for every in-flight RG evaluation of either Person and every later
--    evaluation of either Person waits for the re-link: an account can no longer move between
--    Persons between an evaluation's account read and its decision (the Go side re-reads the Person
--    after its lock and follows a move). It does not pin person_id; that decision stays open.
--
-- Every function pins search_path (ADR 0108); none is SECURITY DEFINER.

CREATE TABLE rg_enforcement_activations (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    brand_id              UUID NULL,
    reason_code           TEXT NOT NULL CHECK (octet_length(reason_code) BETWEEN 1 AND 64 AND reason_code ~ '^[ -~]+$'),
    attestation_ref       TEXT NOT NULL CHECK (octet_length(attestation_ref) BETWEEN 1 AND 200 AND attestation_ref ~ '^[ -~]+$'),
    created_by_actor_type TEXT NOT NULL CHECK (created_by_actor_type = 'staff'),
    created_by_actor_id   UUID NOT NULL,
    created_by_person_id  UUID NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL,
    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id) ON DELETE RESTRICT,
    CONSTRAINT rg_enforcement_activations_scope_key UNIQUE NULLS NOT DISTINCT (tenant_id, brand_id)
);

CREATE FUNCTION rg_enforcement_activations_10_actor() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_actor RECORD;
BEGIN
    SELECT * INTO v_actor FROM public.rg_actor_session();
    IF v_actor.scope <> 'tenant' OR v_actor.tenant IS DISTINCT FROM NEW.tenant_id THEN
        RAISE EXCEPTION 'rg_enforcement_activations: written by an active staff principal of the same tenant only' USING ERRCODE = 'RL001';
    END IF;
    NEW.id := pg_catalog.gen_random_uuid(); -- never a caller value
    NEW.created_by_actor_type := 'staff';
    NEW.created_by_actor_id := v_actor.actor;
    NEW.created_by_person_id := v_actor.person_id;
    NEW.created_at := pg_catalog.clock_timestamp();
    RETURN NEW;
END
$$;

CREATE TRIGGER rg_enforcement_activations_10_actor BEFORE INSERT ON rg_enforcement_activations
    FOR EACH ROW EXECUTE FUNCTION rg_enforcement_activations_10_actor();
CREATE TRIGGER rg_enforcement_activations_immutable BEFORE UPDATE OR DELETE ON rg_enforcement_activations
    FOR EACH ROW EXECUTE FUNCTION rg_deny_mutation();
CREATE TRIGGER rg_enforcement_activations_no_truncate BEFORE TRUNCATE ON rg_enforcement_activations
    FOR EACH STATEMENT EXECUTE FUNCTION rg_deny_mutation();

ALTER TABLE rg_enforcement_activations ENABLE ROW LEVEL SECURITY;
ALTER TABLE rg_enforcement_activations FORCE ROW LEVEL SECURITY;
CREATE POLICY rg_enforcement_activations_read ON rg_enforcement_activations
    FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY rg_enforcement_activations_insert ON rg_enforcement_activations
    FOR INSERT
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

-- The tenant-status and brand-status reads of the gameplay paths do not need this index; the lookup
-- is by the unique key (tenant_id, brand_id) above.

-- -------------------------------------------------------------------------------------------------
-- 2. The person re-link lock.
-- -------------------------------------------------------------------------------------------------
CREATE FUNCTION player_accounts_person_relink_lock() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_old text := OLD.person_id::text;
    v_new text := NEW.person_id::text;
BEGIN
    -- The same key as internal/rg lockPerson: pg_advisory_xact_lock(hashtext('player_restrictions'),
    -- hashtext(<person id>)). Lower hash first, so two concurrent re-links cannot take the pair in
    -- opposite orders.
    IF pg_catalog.hashtext(v_old) <= pg_catalog.hashtext(v_new) THEN
        PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtext('player_restrictions'), pg_catalog.hashtext(v_old));
        PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtext('player_restrictions'), pg_catalog.hashtext(v_new));
    ELSE
        PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtext('player_restrictions'), pg_catalog.hashtext(v_new));
        PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtext('player_restrictions'), pg_catalog.hashtext(v_old));
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER player_accounts_person_relink_lock BEFORE UPDATE OF person_id ON player_accounts
    FOR EACH ROW WHEN (OLD.person_id IS DISTINCT FROM NEW.person_id)
    EXECUTE FUNCTION player_accounts_person_relink_lock();

-- -------------------------------------------------------------------------------------------------
-- 3. Grants to igaming_runtime (mirrored by deploy/init-app-role.sql). RLS + triggers bind.
-- -------------------------------------------------------------------------------------------------
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        EXECUTE 'REVOKE ALL ON rg_enforcement_activations FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON rg_enforcement_activations TO igaming_runtime';
    END IF;
END $$;
