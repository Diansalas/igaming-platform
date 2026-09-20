-- Stage 4I Phase D: jurisdiction policy configuration & operational
-- semantics (architect design ruling, "Stage 4I Phase D: Jurisdiction
-- Policy Configuration & Operational Semantics"; recorded in full at
-- docs/decisions/0043-jurisdiction-evaluation-policy-configuration.md).
-- Implements PC-GAP-4 (canonical-model §14.6) by EXTENDING
-- `jurisdiction_precedence_configs` (migration 0071) rather than creating
-- a sibling table (ADR 0043 Decision 1): the table's role widens from
-- "source-precedence ordering, shape only" to "the effective-dated
-- evaluation-policy configuration for a licensing jurisdiction and
-- operation class, of which the source-precedence ordering is one
-- (currently unset) component."
--
-- Still keyed on (licensing_jurisdiction_id, operation_class,
-- effective_from) - NOT tenant_id (ADR 0043 Decision 2/canonical-model
-- §3.4's bootstrap-circularity finding). The table remains platform-wide
-- reference configuration read by every tenant sharing a licensing
-- jurisdiction.
--
-- PC-GAP-1 (location requirement, HDR-J-8) and PC-GAP-2 (location
-- staleness, HDR-J-9) CONTENT remain BLOCKED on those human decisions
-- (see the new human-decision-register document this phase adds under
-- docs/decisions/). This migration adds the MECHANISM only: the storage,
-- the four-state fail-closed distinction, validation, append-only
-- provenance, and RLS. It inserts ZERO rows - no seed, no fixture, no
-- example jurisdiction.

-- ======================================================================
-- 1. New columns
-- ======================================================================
-- The table is empty (zero Go references anywhere prior to this phase),
-- so NOT NULL columns with no DEFAULT are safe to add directly.

ALTER TABLE jurisdiction_precedence_configs
    ADD COLUMN status TEXT NOT NULL DEFAULT 'draft'
        CHECK (status IN ('draft', 'active', 'withdrawn')),
    ADD COLUMN location_requirement TEXT NOT NULL DEFAULT 'unset'
        CHECK (location_requirement IN ('unset', 'required', 'advisory')),
    ADD COLUMN max_location_signal_age_seconds INTEGER
        CHECK (max_location_signal_age_seconds IS NULL OR max_location_signal_age_seconds > 0),
    ADD COLUMN precedence_status TEXT NOT NULL DEFAULT 'unset'
        CHECK (precedence_status IN ('unset', 'configured')),
    ADD COLUMN precedence_policy_version TEXT NOT NULL,
    ADD COLUMN legal_review_reference TEXT
        CHECK (legal_review_reference IS NULL OR btrim(legal_review_reference) <> ''),
    ADD COLUMN reason_code TEXT NOT NULL
        CHECK (btrim(reason_code) <> '');

COMMENT ON COLUMN jurisdiction_precedence_configs.status IS 'Row-level activation. draft = authored, governs nothing. active = governs. withdrawn = tombstone returning the (licensing_jurisdiction_id, operation_class) key to "no policy". Stage 4I Phase D.';
COMMENT ON COLUMN jurisdiction_precedence_configs.location_requirement IS 'PC-GAP-1 value (unset/required/advisory) - content blocked on HDR-J-8. Stage 4I Phase D.';
COMMENT ON COLUMN jurisdiction_precedence_configs.max_location_signal_age_seconds IS 'PC-GAP-2 value, whole seconds, NULL = unset - content blocked on HDR-J-9. Stage 4I Phase D. No upper bound is imposed at the database - see the Phase D ADR for the residual-risk note handed to security.';
COMMENT ON COLUMN jurisdiction_precedence_configs.precedence_status IS 'Makes HDR-J-2''s blocked precedence content an explicit state (unset/configured) instead of an ambiguous empty JSONB array. Stage 4I Phase D.';
COMMENT ON COLUMN jurisdiction_precedence_configs.precedence_policy_version IS 'The value of jurisdiction.PrecedencePolicyVersion (Phase C''s precedence ALGORITHM version) at authoring time - distinct from resolver_policy_version, which carries jurisdiction.PolicyVersion (the tenant/brand-subject resolver''s own logic version, resolver.go). Both are written from compiled-in constants, never from a caller. Stage 4I Phase D.';
COMMENT ON COLUMN jurisdiction_precedence_configs.legal_review_reference IS 'The recorded legal/compliance validation reference for this policy version (HDR-J-2 NOTES: "subject to legal/compliance validation for each operating jurisdiction"). Required on active rows. Stage 4I Phase D.';
COMMENT ON COLUMN jurisdiction_precedence_configs.reason_code IS 'CLAUDE.md''s audit rule: makes this row''s own history self-describing without a join to audit_log. Stage 4I Phase D.';

-- ======================================================================
-- 2. Table-level CHECK constraints
-- ======================================================================

ALTER TABLE jurisdiction_precedence_configs
    ADD CONSTRAINT jurisdiction_precedence_configs_effective_to_after_from
        CHECK (effective_to IS NULL OR effective_to > effective_from),
    ADD CONSTRAINT jurisdiction_precedence_configs_precedence_status_matches
        CHECK ((precedence_status = 'unset') = (precedence = '[]'::jsonb)),
    ADD CONSTRAINT jurisdiction_precedence_configs_withdrawn_no_content
        CHECK (status <> 'withdrawn' OR (
            location_requirement = 'unset'
            AND max_location_signal_age_seconds IS NULL
            AND precedence_status = 'unset'
        )),
    ADD CONSTRAINT jurisdiction_precedence_configs_active_requires_legal_review
        CHECK (status <> 'active' OR legal_review_reference IS NOT NULL);

-- ======================================================================
-- 3. Indexes
-- ======================================================================
-- The partial unique index is the AUTHORITATIVE concurrency control: at
-- most one open (effective_to IS NULL) version per
-- (licensing_jurisdiction_id, operation_class), enforced by the database,
-- not by application check-then-insert.

CREATE UNIQUE INDEX uq_jurisdiction_precedence_configs_open
    ON jurisdiction_precedence_configs (licensing_jurisdiction_id, operation_class)
    WHERE effective_to IS NULL;

CREATE INDEX idx_jurisdiction_precedence_configs_lookup
    ON jurisdiction_precedence_configs (licensing_jurisdiction_id, operation_class, effective_from DESC);

-- The pre-existing UNIQUE (licensing_jurisdiction_id, operation_class,
-- effective_from) from migration 0071 is retained, untouched.

-- ======================================================================
-- 4. Triggers
-- ======================================================================
-- Mirrors migration 0071's jurisdiction_resolutions_deny_mutation
-- precedent: a trigger is not bypassed by table ownership, unlike a
-- missing RLS policy.

-- 4a. Forge-proof effective_from/created_at/effective_to on INSERT. This
-- is what makes a forged effective_from structurally impossible: the
-- column is DB-set unconditionally, no write API accepts one, and
-- backdating/future-dating are both unavailable. (Scheduled future-dated
-- activation is a real capability someone will eventually want - it is
-- deliberately DEFERRED, not built here.)
CREATE FUNCTION jurisdiction_precedence_configs_stamp_times() RETURNS TRIGGER AS $$
BEGIN
    NEW.effective_from := now();
    NEW.created_at := now();
    NEW.effective_to := NULL;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER jurisdiction_precedence_configs_stamp_times
    BEFORE INSERT ON jurisdiction_precedence_configs
    FOR EACH ROW EXECUTE FUNCTION jurisdiction_precedence_configs_stamp_times();

-- 4b. Append-only: DELETE is denied outright; UPDATE may only move
-- effective_to from NULL to a real timestamp, exactly once, on a row that
-- is not already closed. The `to_jsonb(NEW) - 'effective_to' IS DISTINCT
-- FROM to_jsonb(OLD) - 'effective_to'` comparison is deliberate so a
-- future added column is immutable BY DEFAULT rather than by remembering
-- to extend a column list.
--
-- This function also backstops TRUNCATE (row-level triggers do not fire
-- on TRUNCATE - migration 0016's own finding, restated at migration
-- 0071 lines 134-138) via a separate FOR EACH STATEMENT trigger below;
-- NEW/OLD are not available in that statement-level context, so TG_OP is
-- checked before any reference to either.
--
-- Forge-proof effective_to, exactly mirroring how the BEFORE INSERT
-- trigger above forges effective_from: whenever an UPDATE transitions
-- effective_to from NULL to a non-NULL value, this trigger forcibly sets
-- it to now() regardless of what value the caller supplied. This makes it
-- impossible for a caller-supplied value (backdated or future-dated) to
-- ever land in effective_to - the sanctioned Go path
-- (evaluation_policy_admin.go) already always closes with the SAME
-- transaction's now() used for its own successor's effective_from, so it
-- is unaffected and remains exactly contiguous.
--
-- NOT eliminated by this trigger alone (security re-verification,
-- Stage 4I Phase D fix round): two SEPARATE raw-platform-admin-SQL
-- transactions - one holding a bare close (no successor insert) open,
-- the other inserting a new row for the same key under an
-- earlier-pinned transaction timestamp - can still produce a brief
-- window gap or, in the narrower case of an in-flight bare close
-- racing an overlapping insert, an actual overlap, because each
-- transaction's now() is fixed at its own BEGIN, not at statement time.
-- This is NOT reachable via CreateEvaluationPolicyVersion (which always
-- closes and inserts in ONE transaction) and requires deliberate raw SQL
-- outside the sanctioned write path. ResolveEvaluationPolicy's selection
-- query additionally orders and limits its result (evaluation_policy.go)
-- so that even in this narrow, non-sanctioned scenario, resolution stays
-- deterministic rather than picking an arbitrary row. Eliminating the
-- underlying possibility entirely would require a range-exclusion
-- constraint (e.g. btree_gist's EXCLUDE USING gist), deferred as a named
-- future hardening item since it adds a new extension dependency for a
-- gap only reachable by bypassing every sanctioned write path.
CREATE FUNCTION jurisdiction_precedence_configs_enforce_append_only() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'TRUNCATE' THEN
        RAISE EXCEPTION 'jurisdiction_precedence_configs is append-only: TRUNCATE is not permitted';
    END IF;
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'jurisdiction_precedence_configs is append-only: DELETE is not permitted';
    END IF;
    IF OLD.effective_to IS NULL AND NEW.effective_to IS NOT NULL THEN
        NEW.effective_to := now();
    END IF;
    IF (to_jsonb(NEW) - 'effective_to') IS DISTINCT FROM (to_jsonb(OLD) - 'effective_to') THEN
        RAISE EXCEPTION 'jurisdiction_precedence_configs: only effective_to may change after insert';
    END IF;
    IF OLD.effective_to IS NOT NULL THEN
        RAISE EXCEPTION 'jurisdiction_precedence_configs: a closed version may not be reopened or re-closed';
    END IF;
    IF NEW.effective_to IS NULL THEN
        RAISE EXCEPTION 'jurisdiction_precedence_configs: effective_to may not be cleared';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER jurisdiction_precedence_configs_immutable
    BEFORE UPDATE OR DELETE ON jurisdiction_precedence_configs
    FOR EACH ROW EXECUTE FUNCTION jurisdiction_precedence_configs_enforce_append_only();

CREATE TRIGGER jurisdiction_precedence_configs_deny_truncate
    BEFORE TRUNCATE ON jurisdiction_precedence_configs
    FOR EACH STATEMENT EXECUTE FUNCTION jurisdiction_precedence_configs_enforce_append_only();

-- ======================================================================
-- 5. Row-level security — CHANGED (added where there was none), hardening
--    direction only
-- ======================================================================
-- Migration 0071 shipped this table with NO RLS at all (the same posture
-- as jurisdictions/licences). This phase adds it: permissive read (this
-- remains platform-wide reference configuration every tenant-scoped
-- transaction must be able to read), platform-admin-scope-only write. No
-- DELETE policy. No FOR ALL policy (canonical-model §6.1).

ALTER TABLE jurisdiction_precedence_configs ENABLE ROW LEVEL SECURITY;
ALTER TABLE jurisdiction_precedence_configs FORCE ROW LEVEL SECURITY;

CREATE POLICY jurisdiction_precedence_configs_read ON jurisdiction_precedence_configs
    FOR SELECT USING (true);

CREATE POLICY jurisdiction_precedence_configs_platform_insert ON jurisdiction_precedence_configs
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE POLICY jurisdiction_precedence_configs_platform_close ON jurisdiction_precedence_configs
    FOR UPDATE
    USING (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

-- No DELETE policy. No FOR ALL policy.

-- ======================================================================
-- 6. Table comment — rewritten for the widened role
-- ======================================================================

COMMENT ON TABLE jurisdiction_precedence_configs IS 'The effective-dated evaluation-policy configuration for a licensing jurisdiction and operation class, of which the source-precedence ordering is one (currently unset) component (Stage 4I Phase D, widening migration 0071''s original shape-only table). Keyed on (licensing_jurisdiction_id, operation_class, effective_from) - NEVER tenant_id (canonical-model Sec 3.4''s bootstrap-circularity finding: selecting a per-jurisdiction rule cannot depend on the jurisdiction that rule determines, but the tenant''s own licensing jurisdiction is knowable via tenants.licence_id -> licences.jurisdiction_id before any player-side resolution runs). Append-only: effective_to is the ONLY column that may change after insert, exactly once, from NULL to a real timestamp (jurisdiction_precedence_configs_enforce_append_only). precedence content remains blocked on HDR-J-2; location_requirement/max_location_signal_age_seconds content remains blocked on HDR-J-8/HDR-J-9 respectively (docs/decisions/, Stage 4I Phase D human-decision-register document). Zero rows exist as of this migration.';
