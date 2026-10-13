-- ADR 0119 slice S4b (step-up enforcement; ADR 0119 s6.6, s9.5(b), s13.4). IDENTITY / AUTHENTICATION INTEGRITY
-- CONTROL: FOR `security` (live review), `architect` (the RLS policy pair), `ledger-finance` (s9.5 / ADR 0082 A9),
-- `code-reviewer`, `qa` REVIEW BEFORE MERGE (gate G).
--
-- NUMBERING: 0151, allocated by the orchestrator for S4b. 0139 stays reserved for the staff lifecycle (S5 + S6) and
-- 0143-0150 for ADR 0116 AML and other in-flight slices; the runner applies versions in order and tolerates gaps
-- (0139 is already a gap at 0142).
--
-- What it adds, and only this:
--   0. SET LOCAL lock_timeout = '5s' (0133 / 0136 / 0137 / 0138 precedent), for the whole file.
--   1. sessions: the runtime role regains column-level UPDATE of exactly mfa_at, assurance_kid and assurance_seal
--      (ADR 0119 s13.4; withheld by 0138, review F9, until the slice that first writes them). The step-up writes ONLY
--      the chain-head row of the caller's own session (Go: auth.StepUpStaffSession). The SF040 trigger of 0138 still
--      binds every role: mfa_at never moves backwards or to NULL, the seal / kid never return to NULL, and aal, amr,
--      auth_time, absolute_expires_at, expires_at and the identity columns stay immutable. The keyed seal (format v2
--      since S4b, binding refresh_token_hash and expires_at: condition S3-R10) defeats a SQL edit of mfa_at: an
--      unsealed mfa_at fails the seal check on liveness and on refresh. Mirrored by deploy/init-app-role.sql.
--   2. staff_users: one narrow PERMISSIVE policy pair keyed on the transaction-local GUC app.actor_staff_id (ADR 0119
--      s9.5(b)). Only db.SetActorStaffIDForCurrentTx sets it, from the verified token subject, right before a governed
--      transaction's in-transaction staff re-assertion (a static test pins the single setter). It makes the ACTOR'S
--      OWN row visible and FOR SHARE-lockable under WithTenant(target) / WithPlatformActingInTenant, where the
--      dual-scope policy of migration 0011 hides a platform actor's row. staff_actor_self_lock has WITH CHECK (false):
--      it can never admit an actual UPDATE. Every RESTRICTIVE fence on staff_users (the 0112 acting fence, the 0114
--      KYC-worker fence) still applies unchanged. No new exposure under arbitrary SQL as the runtime role: that role
--      can already read platform rows through an empty app.tenant_id.
--
-- Not here: staff_users.staff_version (0139, S5 + S6), any lifecycle object. No role, password or attribute change.
SET LOCAL lock_timeout = '5s';

-- 1. sessions: the step-up columns.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        EXECUTE 'GRANT UPDATE (mfa_at, assurance_kid, assurance_seal) ON sessions TO igaming_runtime';
    END IF;
END
$$;

-- 2. staff_users: the actor self-select / self-lock pair.
CREATE POLICY staff_actor_self_select ON staff_users FOR SELECT
    USING (id = NULLIF(current_setting('app.actor_staff_id', true), '')::uuid);
CREATE POLICY staff_actor_self_lock ON staff_users FOR UPDATE
    USING (id = NULLIF(current_setting('app.actor_staff_id', true), '')::uuid) WITH CHECK (false);
