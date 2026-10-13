-- ADR 0116 SLICE S1: AML holds and enforcement foundation. MONEY-ADJACENT / COMPLIANCE / SECURITY MIGRATION: FOR
-- `security` (blocking), `ledger-finance` (blocking), `payments`, `casino`, `sportsbook`, `identity-compliance`, `qa`,
-- `code-reviewer` REVIEW BEFORE MERGE.
--
-- NUMBERING: 0141 (0137-0139 reserved for ADR 0119; see 0140). Depends on 0140 (aml_authorize and friends).
--
-- NO LEGAL VALUE and NO AML VENDOR: nothing here screens anybody. A hold exists only because an authorised aml_officer
-- placed it through the proof-bound definer function below (the automatic screening holds are slice S2 and are
-- NOT IMPLEMENTED). No row is seeded.
--
-- What it adds, and only this (ADR 0116 s4.3 C2, s5.1, s5.2, s8 S1):
--   0. Pre-flight (AM098): no AML hold table exists; kyc_enforcement_decisions_outcome_check is 0100's.
--   1. aml_holds, aml_hold_lifts (append-only; lifts are rows, UNIQUE per hold) and aml_enforcement_decisions
--      (append-only; the AML-scoped true outcome). ENABLE + FORCE ROW LEVEL SECURITY; NO privilege and NO policy for
--      igaming_runtime; the only policy is for the table OWNER (CURRENT_USER at migration time: the role that owns the
--      SECURITY DEFINER functions) and is bound to the session tenant, a second independent denial. All FKs to
--      persons, player_accounts and tenants are ON DELETE RESTRICT (C9).
--   2. C2: the composite FK (player_account_id, tenant_id) -> player_accounts(id, tenant_id) and the BEFORE INSERT
--      trigger that requires person_id = player_accounts.person_id in the same tenant (and, for a person-level hold, that
--      the person has an account in the tenant); the owner scope is forced from the tenant's licensing model; the actor
--      columns are forced from the proven session.
--   3. The definer API (every function calls aml_authorize FIRST, 0140): aml_hold_place (needs the officer quorum),
--      aml_hold_lift (single officer; a confirmed_sanctions hold needs the proof-bound four-eyes approval of 0142 and is
--      refused here), aml_hold_list.
--   4. The ENFORCEMENT read path (no proof: the player's own gate calls it): aml_hold_active(tenant, person, account,
--      operation) -> boolean, aml_hold_match(...) -> uuid, and aml_record_enforcement_decision(...). They are SECURITY
--      DEFINER, search_path pinned, STABLE reads, refuse a tenant other than the session tenant (AM026), and are the ONLY
--      way a tenant session learns anything about holds. No tenant session can read aml_holds.
--   5. kyc_enforcement_decisions.outcome gains `review` (the PUBLIC outcome recorded for both a KYC `pending` and an AML
--      `held`, ADR 0116 s5.1 C4). Historical rows are untouched; readers map legacy `pending` to `review`.
--   6. The alert Kind aml.hold_eval_failed (seeded under alert_kinds' FORCE RLS through a temporary literal single-Kind
--      FOR INSERT policy created and dropped in one DO block, the 0114 pattern). Its payload carries the operation only:
--      no case id, person id or match category (ADR 0116 s5.2).
--
-- Enforcement of `held` lives in Go (internal/aml EvaluateHold composed into kyc.EvaluateEnforcement); the DB side is the
-- authority on WHAT a hold is. A held operation gates only new value entering play or leaving the platform; it never
-- gates settlements, reversals or releases (ADR 0116 s5.1 D9). It DOES refuse every MANUAL ADJUSTMENT of the held person's
-- wallet, including a correcting one (a held balance is never seized, zeroed or refunded): the ledger-finance consequence
-- (an erroneous credit cannot be reversed, nor an M2 Step B compensating credit posted, while any hold is active) is recorded
-- for the owner and legal in ADR 0116 s10 (Q7 / HD-AML-9) and is NOT built around here.
--
-- SQLSTATE class AM: see 0140. This file uses AM002, AM003, AM004, AM005, AM006, AM007, AM026, AM040, AM098, AM099.

SET LOCAL lock_timeout = '5s';

-- =========================================================================
-- 0. Pre-flight
-- =========================================================================
DO $$
DECLARE
    v_def text;
BEGIN
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_class WHERE relnamespace = 'public'::regnamespace
                AND relname IN ('aml_holds', 'aml_hold_lifts', 'aml_enforcement_decisions')) THEN
        RAISE EXCEPTION 'migration 0141 pre-flight: an AML hold table already exists' USING ERRCODE = 'AM098';
    END IF;
    IF to_regprocedure('public.aml_authorize(uuid, text, text, text)') IS NULL THEN
        RAISE EXCEPTION 'migration 0141 pre-flight: migration 0140 (aml_authorize) is not applied' USING ERRCODE = 'AM098';
    END IF;
    SELECT pg_catalog.pg_get_constraintdef(oid) INTO v_def FROM pg_catalog.pg_constraint
     WHERE conrelid = 'public.kyc_enforcement_decisions'::regclass AND conname = 'kyc_enforcement_decisions_outcome_check';
    IF v_def IS DISTINCT FROM 'CHECK ((outcome = ANY (ARRAY[''not_required''::text, ''passed''::text, ''pending''::text, ''failed''::text, ''unavailable''::text])))' THEN
        RAISE EXCEPTION 'migration 0141 pre-flight: kyc_enforcement_decisions_outcome_check is not the 0100 definition (%)', v_def USING ERRCODE = 'AM098';
    END IF;
    IF EXISTS (SELECT 1 FROM public.alert_kinds WHERE kind = 'aml.hold_eval_failed') THEN
        RAISE EXCEPTION 'migration 0141 pre-flight: alert kind aml.hold_eval_failed already exists' USING ERRCODE = 'AM098';
    END IF;
END
$$;

-- =========================================================================
-- 1. Public outcome `review` on the tenant-visible decision log
-- =========================================================================
ALTER TABLE kyc_enforcement_decisions DROP CONSTRAINT kyc_enforcement_decisions_outcome_check;
-- NOT VALID then VALIDATE (security F11): the ACCESS EXCLUSIVE lock of the ADD is held only for the catalogue change; the
-- full-table scan of the (large, append-only) decision log runs under the weaker SHARE UPDATE EXCLUSIVE lock of VALIDATE.
-- The new CHECK only ADMITS one more value, so VALIDATE cannot fail on existing rows.
ALTER TABLE kyc_enforcement_decisions ADD CONSTRAINT kyc_enforcement_decisions_outcome_check
    CHECK (outcome IN ('not_required', 'passed', 'pending', 'failed', 'unavailable', 'review')) NOT VALID;
ALTER TABLE kyc_enforcement_decisions VALIDATE CONSTRAINT kyc_enforcement_decisions_outcome_check;

-- =========================================================================
-- 2. Shared helpers
-- =========================================================================

-- The tenant a session is bound to: app.tenant_id, or the acting tenant ONLY when the session has exactly the acting shape
-- (acting tenant AND acting platform principal present, no other principal setting: financial_acting_gucs_exact(), 0112). A
-- bare app.acting_tenant_id (a forged setting without the acting principal) binds nothing (security F5). The officer functions
-- additionally validate the acting principal's grant in aml_authorize. NULL when neither.
CREATE FUNCTION aml_session_tenant() RETURNS uuid
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT COALESCE(NULLIF(pg_catalog.current_setting('app.tenant_id', true), ''),
                    CASE WHEN public.financial_acting_gucs_exact()
                         THEN NULLIF(pg_catalog.current_setting('app.acting_tenant_id', true), '') END)::uuid
$$;

-- The per-person serialisation point of hold changes and four-eyes proposals / approvals (security F8): a hold placement, a
-- lift, a proposal and an approval for the same (tenant, person) never interleave, so "no hold changed since the proposal" is
-- decided against a stable hold set. Transaction-scoped advisory lock; the key is derived from the ids only.
CREATE FUNCTION aml_person_lock(p_tenant uuid, p_person uuid) RETURNS void
    LANGUAGE sql VOLATILE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended('aml_person:' || p_tenant::text || ':' || p_person::text, 116))
$$;

-- The monotonic hold-change mark of a person: the highest change_seq of any hold or lift of (tenant, person), 0 when none. A
-- proposal records the mark; an approval is invalid when the mark has moved. change_seq comes from ONE sequence, assigned
-- under aml_person_lock, so it orders hold changes of a person independent of clocks and transaction start times.
CREATE FUNCTION aml_person_hold_mark(p_tenant uuid, p_person uuid) RETURNS bigint
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    RETURN COALESCE(GREATEST(
        (SELECT max(h.change_seq) FROM public.aml_holds h WHERE h.tenant_id = p_tenant AND h.person_id = p_person),
        (SELECT max(l.change_seq) FROM public.aml_hold_lifts l JOIN public.aml_holds h ON h.id = l.hold_id
          WHERE h.tenant_id = p_tenant AND h.person_id = p_person)), 0);
END
$$;

-- A WRITE reaches only rows of the actor's OWN owner scope (ADR 0116 s4.3 "only the hold's owner_scope may lift"): a
-- tenant officer owns 'tenant' rows, a platform officer owns 'platform' rows. A mismatch is reported as not found.
CREATE FUNCTION aml_actor_owner_scope(p_actor_scope text) RETURNS text
    LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT CASE p_actor_scope WHEN 'tenant' THEN 'tenant' WHEN 'platform_acting' THEN 'platform' ELSE NULL END
$$;

CREATE FUNCTION aml_assert_write_scope(p_actor_scope text, p_owner_scope text) RETURNS void
    LANGUAGE plpgsql IMMUTABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF p_owner_scope IS NULL OR public.aml_actor_owner_scope(p_actor_scope) IS DISTINCT FROM p_owner_scope THEN
        RAISE EXCEPTION 'aml: not found' USING ERRCODE = 'AM040';
    END IF;
END
$$;

-- Append-only guard shared by every AML table (UPDATE, DELETE and TRUNCATE all refused, binding on the owner).
CREATE FUNCTION aml_deny_mutation() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    RAISE EXCEPTION 'aml: % is append-only; % is not permitted', TG_TABLE_NAME, TG_OP USING ERRCODE = 'AM005';
END
$$;

-- The operations a hold can gate (ADR 0116 s5.1) plus manual_adjustment (ADR 0116 s5.1 "held balances": no manual
-- adjustment may seize, zero or refund a held balance; ANY active hold refuses an adjustment).
CREATE FUNCTION aml_valid_operation(p_operation text) RETURNS boolean
    LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT p_operation IN ('deposit', 'withdrawal', 'payout', 'casino_stake', 'sportsbook_stake', 'manual_adjustment')
$$;

-- =========================================================================
-- 3. aml_holds
-- =========================================================================
CREATE SEQUENCE aml_hold_change_seq;

CREATE TABLE aml_holds (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    owner_scope          TEXT NOT NULL CHECK (owner_scope IN ('platform', 'tenant')),
    person_id            UUID NOT NULL REFERENCES persons (id) ON DELETE RESTRICT,
    player_account_id    UUID NULL,
    hold_scope           TEXT NOT NULL CHECK (hold_scope IN ('all_value_movement', 'withdrawal_and_payout')),
    -- Slice S1 ships the officer-placed classes only; S2 extends this CHECK with the screening classes.
    reason_class         TEXT NOT NULL CHECK (reason_class IN ('manual', 'confirmed_sanctions')),
    reason_code          TEXT NOT NULL CHECK (reason_code ~ '^[a-z0-9_]{1,64}$'),
    placed_by            UUID NOT NULL,
    placed_by_person_id  UUID NOT NULL,
    placed_by_scope      TEXT NOT NULL CHECK (placed_by_scope IN ('tenant', 'platform_acting')),
    placed_at            TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    change_seq           BIGINT NOT NULL DEFAULT nextval('aml_hold_change_seq'),
    UNIQUE (tenant_id, id),
    FOREIGN KEY (player_account_id, tenant_id) REFERENCES player_accounts (id, tenant_id) ON DELETE RESTRICT,
    CHECK (reason_class <> 'confirmed_sanctions' OR hold_scope = 'all_value_movement'),
    CHECK ((placed_by_scope = 'tenant') = (owner_scope = 'tenant'))
);
CREATE INDEX aml_holds_person ON aml_holds (tenant_id, person_id);

CREATE TABLE aml_hold_lifts (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    hold_id              UUID NOT NULL UNIQUE REFERENCES aml_holds (id) ON DELETE RESTRICT,
    tenant_id            UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    owner_scope          TEXT NOT NULL CHECK (owner_scope IN ('platform', 'tenant')),
    lifted_by            UUID NOT NULL,
    lifted_by_person_id  UUID NOT NULL,
    lifted_by_scope      TEXT NOT NULL CHECK (lifted_by_scope IN ('tenant', 'platform_acting')),
    lifted_at            TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    change_seq           BIGINT NOT NULL DEFAULT nextval('aml_hold_change_seq'),
    reason_code          TEXT NOT NULL CHECK (reason_code ~ '^[a-z0-9_]{1,64}$'),
    -- The approved four-eyes event that authorises a confirmed_sanctions lift (0142 adds the FK and the event checks).
    approval_event_id    UUID NULL,
    FOREIGN KEY (tenant_id, hold_id) REFERENCES aml_holds (tenant_id, id) ON DELETE RESTRICT,
    CHECK ((lifted_by_scope = 'tenant') = (owner_scope = 'tenant'))
);

CREATE TABLE aml_enforcement_decisions (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    player_account_id  UUID NOT NULL,
    person_id          UUID NOT NULL REFERENCES persons (id) ON DELETE RESTRICT,
    operation          TEXT NOT NULL CHECK (operation IN ('deposit', 'withdrawal_hold', 'withdrawal_payout', 'casino_play', 'sportsbook_play', 'manual_adjustment')),
    -- 'not_evaluated': a manual adjustment is refused on the hold alone; no KYC evaluation took part (security F4, option b).
    kyc_outcome        TEXT NOT NULL CHECK (kyc_outcome IN ('not_required', 'passed', 'pending', 'failed', 'unavailable', 'not_evaluated')),
    hold_outcome       TEXT NOT NULL CHECK (hold_outcome IN ('none', 'held', 'eval_error')),
    public_outcome     TEXT NOT NULL CHECK (public_outcome IN ('not_required', 'passed', 'review', 'failed', 'unavailable')),
    allowed            BOOLEAN NOT NULL,
    hold_id            UUID NULL,
    correlation_id     UUID NOT NULL,
    decided_at         TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (player_account_id, tenant_id) REFERENCES player_accounts (id, tenant_id) ON DELETE RESTRICT,
    FOREIGN KEY (tenant_id, hold_id) REFERENCES aml_holds (tenant_id, id) ON DELETE RESTRICT,
    CHECK (hold_outcome = 'held' OR hold_id IS NULL),
    CHECK (hold_outcome = 'none' OR NOT allowed)
);
CREATE INDEX aml_enforcement_decisions_player ON aml_enforcement_decisions (tenant_id, player_account_id, decided_at DESC);

-- RLS: enabled and FORCED; no privilege and no policy for the runtime role. The single policy is for the OWNER role and is
-- bound to the session tenant (defence in depth: the definer functions below also filter by tenant explicitly).
DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['aml_holds', 'aml_hold_lifts', 'aml_enforcement_decisions'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('REVOKE ALL ON %I FROM PUBLIC', t);
        EXECUTE format($f$CREATE POLICY aml_owner_session ON %I FOR ALL TO CURRENT_USER
                          USING (tenant_id = public.aml_session_tenant()) WITH CHECK (tenant_id = public.aml_session_tenant())$f$, t);
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
            EXECUTE format('REVOKE ALL ON %I FROM igaming_runtime', t);
        END IF;
        EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION aml_deny_mutation()', t || '_append_only', t);
        EXECUTE format('CREATE TRIGGER %I BEFORE TRUNCATE ON %I FOR EACH STATEMENT EXECUTE FUNCTION aml_deny_mutation()', t || '_no_truncate', t);
    END LOOP;
END
$$;

-- aml_holds BEFORE INSERT: owner scope forced from the tenant's licensing model; C2 person binding; actor columns equal the
-- proven session (the definer function sets them from aml_authorize; this is the second, independent check).
CREATE FUNCTION aml_holds_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_actor record;
    v_acct  uuid;
BEGIN
    -- The monotonic hold-change mark (security F8) is FORCED here, under the per-person lock, so the invariant does not depend on
    -- which function (or owner-level statement) inserts the row.
    PERFORM public.aml_person_lock(NEW.tenant_id, NEW.person_id);
    NEW.change_seq := pg_catalog.nextval('public.aml_hold_change_seq');
    IF NEW.owner_scope IS DISTINCT FROM public.aml_owner_scope_of_tenant(NEW.tenant_id) THEN
        RAISE EXCEPTION 'aml: the hold owner scope must be the tenant''s licensing owner scope' USING ERRCODE = 'AM002';
    END IF;
    IF NEW.player_account_id IS NOT NULL THEN
        SELECT pa.person_id INTO v_acct FROM public.player_accounts pa
         WHERE pa.id = NEW.player_account_id AND pa.tenant_id = NEW.tenant_id;
        IF v_acct IS NULL OR v_acct <> NEW.person_id THEN
            RAISE EXCEPTION 'aml: the hold person is not the person of the named player account' USING ERRCODE = 'AM007';
        END IF;
    ELSIF NOT EXISTS (SELECT 1 FROM public.player_accounts pa WHERE pa.tenant_id = NEW.tenant_id AND pa.person_id = NEW.person_id) THEN
        RAISE EXCEPTION 'aml: the person has no account in this tenant' USING ERRCODE = 'AM007';
    END IF;
    SELECT * INTO v_actor FROM public.financial_actor_session();
    IF NEW.placed_by IS DISTINCT FROM v_actor.actor OR NEW.placed_by_scope IS DISTINCT FROM v_actor.scope
       OR NEW.placed_by_person_id IS DISTINCT FROM v_actor.person_id THEN
        RAISE EXCEPTION 'aml: the placing actor columns are not the proven session actor' USING ERRCODE = 'AM001';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER aml_holds_10_guard BEFORE INSERT ON aml_holds FOR EACH ROW EXECUTE FUNCTION aml_holds_guard();

-- aml_hold_lifts BEFORE INSERT: the lift mirrors the hold's tenant and owner scope; only the hold's owner scope may lift;
-- a confirmed_sanctions hold needs an approval event (0142 validates it); the actor is the proven session actor.
CREATE FUNCTION aml_hold_lifts_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_hold  record;
    v_actor record;
BEGIN
    SELECT h.tenant_id, h.owner_scope, h.reason_class, h.person_id INTO v_hold FROM public.aml_holds h WHERE h.id = NEW.hold_id;
    IF NOT FOUND OR v_hold.tenant_id <> NEW.tenant_id THEN
        RAISE EXCEPTION 'aml: not found' USING ERRCODE = 'AM040';
    END IF;
    -- The monotonic hold-change mark (security F8) is FORCED here, under the per-person lock, so the invariant does not depend on
    -- which function (or owner-level statement) inserts the row.
    PERFORM public.aml_person_lock(NEW.tenant_id, v_hold.person_id);
    NEW.change_seq := pg_catalog.nextval('public.aml_hold_change_seq');
    IF NEW.owner_scope <> v_hold.owner_scope THEN
        RAISE EXCEPTION 'aml: only the hold''s owner scope may lift it' USING ERRCODE = 'AM006';
    END IF;
    IF v_hold.reason_class = 'confirmed_sanctions' AND NEW.approval_event_id IS NULL THEN
        RAISE EXCEPTION 'aml: lifting a confirmed sanctions hold needs a proof-bound four-eyes approval' USING ERRCODE = 'AM006';
    END IF;
    IF v_hold.reason_class <> 'confirmed_sanctions' AND NEW.approval_event_id IS NOT NULL THEN
        RAISE EXCEPTION 'aml: an approval event is only valid for a confirmed sanctions lift' USING ERRCODE = 'AM006';
    END IF;
    SELECT * INTO v_actor FROM public.financial_actor_session();
    IF NEW.lifted_by IS DISTINCT FROM v_actor.actor OR NEW.lifted_by_scope IS DISTINCT FROM v_actor.scope
       OR NEW.lifted_by_person_id IS DISTINCT FROM v_actor.person_id THEN
        RAISE EXCEPTION 'aml: the lifting actor columns are not the proven session actor' USING ERRCODE = 'AM001';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER aml_hold_lifts_10_guard BEFORE INSERT ON aml_hold_lifts FOR EACH ROW EXECUTE FUNCTION aml_hold_lifts_guard();

-- =========================================================================
-- 4. The enforcement read path (callable by the player/gate session, no proof)
-- =========================================================================

-- aml_hold_match: the oldest ACTIVE hold of (tenant, person) that gates p_operation, or NULL. A hold applies to the person in
-- the tenant (player_account_id NULL) or to one account. all_value_movement gates every operation; withdrawal_and_payout
-- gates withdrawal and payout; ANY hold gates manual_adjustment. The session tenant must be the requested tenant (AM026):
-- the answer is never a silently RLS-filtered `false`.
CREATE FUNCTION aml_hold_match(p_tenant uuid, p_person uuid, p_account uuid, p_operation text) RETURNS uuid
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_id uuid;
BEGIN
    IF p_tenant IS NULL OR p_person IS NULL OR p_operation IS NULL OR NOT public.aml_valid_operation(p_operation) THEN
        RAISE EXCEPTION 'aml: invalid hold evaluation arguments' USING ERRCODE = 'AM004';
    END IF;
    IF p_tenant IS DISTINCT FROM public.aml_session_tenant() THEN
        RAISE EXCEPTION 'aml: the session tenant is not the requested tenant' USING ERRCODE = 'AM026';
    END IF;
    SELECT h.id INTO v_id
      FROM public.aml_holds h
     WHERE h.tenant_id = p_tenant AND h.person_id = p_person
       AND (h.player_account_id IS NULL OR h.player_account_id = p_account)
       AND (h.hold_scope = 'all_value_movement' OR p_operation IN ('withdrawal', 'payout', 'manual_adjustment'))
       AND NOT EXISTS (SELECT 1 FROM public.aml_hold_lifts l WHERE l.hold_id = h.id)
     ORDER BY h.placed_at, h.id
     LIMIT 1;
    RETURN v_id;
END
$$;

CREATE FUNCTION aml_hold_active(p_tenant uuid, p_person uuid, p_account uuid, p_operation text) RETURNS boolean
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF p_tenant IS NULL OR p_person IS NULL OR p_operation IS NULL OR NOT public.aml_valid_operation(p_operation) THEN
        RAISE EXCEPTION 'aml: invalid hold evaluation arguments' USING ERRCODE = 'AM004';
    END IF;
    IF p_tenant IS DISTINCT FROM public.aml_session_tenant() THEN
        RAISE EXCEPTION 'aml: the session tenant is not the requested tenant' USING ERRCODE = 'AM026';
    END IF;
    RETURN EXISTS (
        SELECT 1 FROM public.aml_holds h
         WHERE h.tenant_id = p_tenant AND h.person_id = p_person
           AND (h.player_account_id IS NULL OR h.player_account_id = p_account)
           AND (h.hold_scope = 'all_value_movement' OR p_operation IN ('withdrawal', 'payout', 'manual_adjustment'))
           AND NOT EXISTS (SELECT 1 FROM public.aml_hold_lifts l WHERE l.hold_id = h.id)
    );
END
$$;

-- aml_record_enforcement_decision: writes the AML-scoped true outcome for a held / evaluator-error evaluation, in the
-- caller's (gate's) transaction. Validates the tenant against the session and the account/person pairing. NOT proof-bound
-- (a gate is not an officer): a runtime-credential holder can forge a row here, exactly as it can already forge a
-- kyc_enforcement_decisions row; the table is an evidence log for officers, not an enforcement input.
CREATE FUNCTION aml_record_enforcement_decision(
    p_tenant uuid, p_account uuid, p_person uuid, p_operation text,
    p_kyc_outcome text, p_hold_outcome text, p_public_outcome text, p_allowed boolean, p_hold_id uuid, p_correlation uuid
) RETURNS uuid
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_id uuid;
BEGIN
    IF p_tenant IS NULL OR p_account IS NULL OR p_person IS NULL OR p_correlation IS NULL THEN
        RAISE EXCEPTION 'aml: invalid decision arguments' USING ERRCODE = 'AM004';
    END IF;
    IF p_tenant IS DISTINCT FROM public.aml_session_tenant() THEN
        RAISE EXCEPTION 'aml: the session tenant is not the requested tenant' USING ERRCODE = 'AM026';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM public.player_accounts pa WHERE pa.id = p_account AND pa.tenant_id = p_tenant AND pa.person_id = p_person) THEN
        RAISE EXCEPTION 'aml: the account does not belong to this tenant and person' USING ERRCODE = 'AM007';
    END IF;
    -- A named hold must be a hold of THIS person in THIS tenant (security F7): a decision row never points at another
    -- person's hold, so an officer reading the evidence log for one person never sees another person's hold id.
    IF p_hold_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM public.aml_holds h WHERE h.id = p_hold_id AND h.tenant_id = p_tenant AND h.person_id = p_person) THEN
        RAISE EXCEPTION 'aml: the named hold is not a hold of this person' USING ERRCODE = 'AM007';
    END IF;
    INSERT INTO public.aml_enforcement_decisions
        (tenant_id, player_account_id, person_id, operation, kyc_outcome, hold_outcome, public_outcome, allowed, hold_id, correlation_id)
    VALUES (p_tenant, p_account, p_person, p_operation, p_kyc_outcome, p_hold_outcome, p_public_outcome, p_allowed, p_hold_id, p_correlation)
    RETURNING id INTO v_id;
    RETURN v_id;
END
$$;

-- =========================================================================
-- 5. The officer API (proof-bound, ADR 0116 s3.1)
-- =========================================================================

-- aml_hold_place: needs an in-force aml_officer grant, a signed proof bound to the exact arguments (target `new`), and the
-- officer quorum of the owner scope (ADR 0116 s6: a hold cannot be ACTIVATED for a scope without a quorum). Owner scope =
-- the tenant's licensing owner scope; the placing actor must own that scope (a platform officer places holds only in
-- platform-licensed tenants, a tenant officer only in own-licence tenants).
CREATE FUNCTION aml_hold_place(
    p_tenant uuid, p_person uuid, p_account uuid, p_hold_scope text, p_reason_class text, p_reason_code text
) RETURNS uuid
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    a       record;
    v_owner text;
    v_id    uuid;
BEGIN
    SELECT * INTO a FROM public.aml_authorize(p_tenant, 'aml_hold:place', 'new',
        public.k2_sha256_hex(public.k2_canonical(p_tenant::text, p_person::text, p_account::text, p_hold_scope, p_reason_class, p_reason_code)));
    v_owner := public.aml_owner_scope_of_tenant(p_tenant);
    IF public.aml_actor_owner_scope(a.o_scope) IS DISTINCT FROM v_owner THEN
        RAISE EXCEPTION 'aml: the actor does not own this tenant''s AML scope' USING ERRCODE = 'AM002';
    END IF;
    PERFORM public.aml_assert_not_subject(a.o_person_id, p_person);
    PERFORM public.aml_assert_quorum(p_tenant, v_owner);
    PERFORM public.aml_person_lock(p_tenant, p_person);
    INSERT INTO public.aml_holds (tenant_id, owner_scope, person_id, player_account_id, hold_scope, reason_class, reason_code,
                                  placed_by, placed_by_person_id, placed_by_scope)
    VALUES (p_tenant, v_owner, p_person, p_account, p_hold_scope, p_reason_class, p_reason_code, a.o_actor, a.o_person_id, a.o_scope)
    RETURNING id INTO v_id;
    RETURN v_id;
END
$$;

-- aml_hold_lift: lifts a `manual` hold. A confirmed_sanctions hold is lifted only by the four-eyes approval of 0142.
CREATE FUNCTION aml_hold_lift(p_tenant uuid, p_hold uuid, p_reason_code text) RETURNS uuid
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    a       record;
    v_hold  record;
    v_id    uuid;
BEGIN
    SELECT * INTO a FROM public.aml_authorize(p_tenant, 'aml_hold:lift', p_hold::text,
        public.k2_sha256_hex(public.k2_canonical(p_tenant::text, p_hold::text, p_reason_code)));
    SELECT h.owner_scope, h.reason_class, h.person_id INTO v_hold FROM public.aml_holds h WHERE h.id = p_hold AND h.tenant_id = p_tenant;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'aml: not found' USING ERRCODE = 'AM040';
    END IF;
    PERFORM public.aml_assert_write_scope(a.o_scope, v_hold.owner_scope);
    PERFORM public.aml_assert_not_subject(a.o_person_id, v_hold.person_id);
    PERFORM public.aml_person_lock(p_tenant, v_hold.person_id);
    IF v_hold.reason_class = 'confirmed_sanctions' THEN
        RAISE EXCEPTION 'aml: a confirmed sanctions hold is lifted only by a proof-bound four-eyes approval' USING ERRCODE = 'AM006';
    END IF;
    INSERT INTO public.aml_hold_lifts (hold_id, tenant_id, owner_scope, lifted_by, lifted_by_person_id, lifted_by_scope, reason_code)
    VALUES (p_hold, p_tenant, v_hold.owner_scope, a.o_actor, a.o_person_id, a.o_scope, p_reason_code)
    RETURNING id INTO v_id;
    RETURN v_id;
END
$$;

-- aml_hold_list: the holds of one person in one tenant that the actor's scope may read (a platform officer reads both owner
-- scopes of the tenant it acts in; a tenant officer reads tenant-owned rows only). Proof target = the person.
CREATE FUNCTION aml_hold_list(p_tenant uuid, p_person uuid)
RETURNS TABLE (hold_id uuid, owner_scope text, player_account_id uuid, hold_scope text, reason_class text, reason_code text,
               placed_at timestamptz, lifted boolean, lifted_at timestamptz)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    a record;
BEGIN
    SELECT * INTO a FROM public.aml_authorize(p_tenant, 'aml_hold:read', p_person::text,
        public.k2_sha256_hex(public.k2_canonical(p_tenant::text, p_person::text)));
    PERFORM public.aml_assert_not_subject(a.o_person_id, p_person);
    RETURN QUERY
        SELECT h.id, h.owner_scope, h.player_account_id, h.hold_scope, h.reason_class, h.reason_code, h.placed_at,
               (l.id IS NOT NULL), l.lifted_at
          FROM public.aml_holds h LEFT JOIN public.aml_hold_lifts l ON l.hold_id = h.id
         WHERE h.tenant_id = p_tenant AND h.person_id = p_person AND public.aml_scope_compatible(a.o_scope, h.owner_scope)
         ORDER BY h.placed_at, h.id;
END
$$;

-- =========================================================================
-- 6. Grants (mirrored by deploy/init-app-role.sql)
-- =========================================================================
REVOKE ALL ON FUNCTION aml_session_tenant() FROM PUBLIC;
REVOKE ALL ON FUNCTION aml_actor_owner_scope(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION aml_assert_write_scope(text, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION aml_deny_mutation() FROM PUBLIC;
REVOKE ALL ON FUNCTION aml_valid_operation(text) FROM PUBLIC;
REVOKE ALL ON SEQUENCE aml_hold_change_seq FROM PUBLIC;
REVOKE ALL ON FUNCTION aml_person_lock(uuid, uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION aml_person_hold_mark(uuid, uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION aml_holds_guard() FROM PUBLIC;
REVOKE ALL ON FUNCTION aml_hold_lifts_guard() FROM PUBLIC;
REVOKE ALL ON FUNCTION aml_hold_match(uuid, uuid, uuid, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION aml_hold_active(uuid, uuid, uuid, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION aml_record_enforcement_decision(uuid, uuid, uuid, text, text, text, text, boolean, uuid, uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION aml_hold_place(uuid, uuid, uuid, text, text, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION aml_hold_lift(uuid, uuid, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION aml_hold_list(uuid, uuid) FROM PUBLIC;
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        REVOKE ALL ON FUNCTION aml_session_tenant() FROM igaming_runtime;
        REVOKE ALL ON FUNCTION aml_actor_owner_scope(text) FROM igaming_runtime;
        REVOKE ALL ON FUNCTION aml_assert_write_scope(text, text) FROM igaming_runtime;
        REVOKE ALL ON FUNCTION aml_deny_mutation() FROM igaming_runtime;
        REVOKE ALL ON FUNCTION aml_valid_operation(text) FROM igaming_runtime;
        -- The default privileges of the schema grant the runtime role USAGE on every new sequence: take it back (nextval is the owner's).
        REVOKE ALL ON SEQUENCE aml_hold_change_seq FROM igaming_runtime;
        REVOKE ALL ON FUNCTION aml_person_lock(uuid, uuid) FROM igaming_runtime;
        REVOKE ALL ON FUNCTION aml_person_hold_mark(uuid, uuid) FROM igaming_runtime;
        REVOKE ALL ON FUNCTION aml_holds_guard() FROM igaming_runtime;
        REVOKE ALL ON FUNCTION aml_hold_lifts_guard() FROM igaming_runtime;
        GRANT EXECUTE ON FUNCTION aml_hold_match(uuid, uuid, uuid, text) TO igaming_runtime;
        GRANT EXECUTE ON FUNCTION aml_hold_active(uuid, uuid, uuid, text) TO igaming_runtime;
        GRANT EXECUTE ON FUNCTION aml_record_enforcement_decision(uuid, uuid, uuid, text, text, text, text, boolean, uuid, uuid) TO igaming_runtime;
        GRANT EXECUTE ON FUNCTION aml_hold_place(uuid, uuid, uuid, text, text, text) TO igaming_runtime;
        GRANT EXECUTE ON FUNCTION aml_hold_lift(uuid, uuid, text) TO igaming_runtime;
        GRANT EXECUTE ON FUNCTION aml_hold_list(uuid, uuid) TO igaming_runtime;
    END IF;
END
$$;

-- =========================================================================
-- 7. The alert Kind (0114 pattern: temporary literal single-Kind FOR INSERT policy, created and dropped in one DO block)
-- =========================================================================
-- severity p2 is an ENGINEERING DEFAULT (ALERT-DELIVERY-1 OPEN: nobody is paged). The payload allows the operation only.
DO $$
BEGIN
    CREATE POLICY alert_kinds_seed_0141 ON alert_kinds
        FOR INSERT TO CURRENT_USER
        WITH CHECK (kind = 'aml.hold_eval_failed');
    INSERT INTO alert_kinds (kind, severity, scope, simulation, requires_subject,
                             in_tx_raisable_by_tenant, allowed_keys, raise_mode)
    VALUES ('aml.hold_eval_failed', 'p2', 'platform', false, true, true, ARRAY['operation'], 'in_tx');
    DROP POLICY alert_kinds_seed_0141 ON alert_kinds;
END
$$;

COMMENT ON TABLE aml_holds IS 'ADR 0116 s4.3: append-only AML holds. No tenant session reads this table; the gates call aml_hold_active().';
COMMENT ON FUNCTION aml_hold_active(uuid, uuid, uuid, text) IS 'ADR 0116 s5.1: the ONLY hold read a gate performs. SECURITY DEFINER, STABLE, search_path pinned, session-tenant bound (AM026).';
