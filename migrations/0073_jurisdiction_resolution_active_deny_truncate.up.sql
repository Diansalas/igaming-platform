-- Stage 4I final independent security/compliance certification
-- (`security`, SEC-4I-F8): add the missing BEFORE TRUNCATE deny trigger
-- to jurisdiction_resolution_active.
--
-- WHAT WAS MISSING. The canonical model's §6.1 ("Universal schema
-- rules", adopted verbatim from the Stage 4I security model §S-3.1) is
-- stated as binding on BOTH new tables -- its opening line reads
-- "Binding on `jurisdiction_resolutions` and on
-- `jurisdiction_resolution_active`" -- and its per-command-policy bullet
-- ends: "Plus a `BEFORE TRUNCATE ... FOR EACH STATEMENT` deny trigger."
-- Migration 0071 gave jurisdiction_resolutions both halves
-- (jurisdiction_resolutions_immutable + jurisdiction_resolutions_deny_
-- truncate) and gave jurisdiction_resolution_active neither. Migration
-- 0072 (SEC-4I-F4) closed the FOR ALL / DELETE-policy half of the same
-- bullet for that table but did not close this half.
--
-- WHY IT MATTERS CONCRETELY, AND WHY RLS DOES NOT ALREADY COVER IT.
-- PostgreSQL row-level security does NOT apply to TRUNCATE at all -- the
-- command is governed only by the TRUNCATE privilege, which the
-- application role holds implicitly because it OWNS these tables
-- (deploy/init-app-role.sql: NOSUPERUSER, NOBYPASSRLS, but owner of the
-- application database and its schema). So every control 0071 and 0072
-- put on this table is bypassed by one statement:
--
--   verified live, as the application role, against the dev database:
--     BEGIN; SET app.tenant_id = '<any tenant>';
--     TRUNCATE jurisdiction_resolution_active;   -- succeeds
--
-- It succeeds from an ordinary tenant-scoped connection (db.Pool.
-- WithTenant -- every admin handler in the platform) and even from a
-- player-scoped one (WithPlayerScope), which the RLS predicates
-- otherwise exclude from this table entirely.
--
-- This is SEC-4I-F4's own hazard reopened through a different command,
-- with a LARGER blast radius. F4's finding was that a FOR ALL policy let
-- a tenant-scoped transaction DELETE its own resolution-active rows --
-- "an UNAUDITED way to change a control's state, plus a lasting
-- divergence between audit_log (which still says the fact was set
-- active) and the table (where the row no longer exists)". TRUNCATE
-- produces exactly that divergence for EVERY tenant at once, not just
-- the caller's own. SetResolutionActive(active = false) writes an
-- audit_log "jurisdiction_resolution_active.changed" row carrying
-- before/after and a reason code in the same transaction; TRUNCATE
-- writes nothing.
--
-- No application code path issues a TRUNCATE against this table, and
-- that is precisely the point: the trigger is the structural backstop
-- for that fact, exactly as the absence of a DELETE policy is. CLAUDE.md
-- requires tenant-owned state to be protected "by PostgreSQL row-level
-- security... not by discipline in application code"; RLS demonstrably
-- does not reach TRUNCATE, so a trigger is the only mechanism that does.
-- A trigger is also not bypassed by table ownership, which is the
-- identical reason audit_log_deny_truncate (migration 0016),
-- ledger_transactions_no_truncate (0021), ledger_entries_no_truncate
-- (0022) and asset_authorizations_no_truncate (0047) all exist.
--
-- The shape below follows asset_authorizations_no_truncate (0047)
-- exactly -- the same table whose per-command policy shape migration
-- 0072 adopted for this table.

CREATE FUNCTION jurisdiction_resolution_active_deny_truncate() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '%: TRUNCATE is not permitted - a resolution-active fact is turned off with SetResolutionActive(active = false), which is audited with before/after and a reason code, never erased', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER jurisdiction_resolution_active_no_truncate
    BEFORE TRUNCATE ON jurisdiction_resolution_active
    FOR EACH STATEMENT EXECUTE FUNCTION jurisdiction_resolution_active_deny_truncate();
