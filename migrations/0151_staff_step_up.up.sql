-- ADR 0119 slice S4b (step-up enforcement; ADR 0119 s6.6, s13.4). IDENTITY / AUTHENTICATION INTEGRITY CONTROL: FOR
-- `security` (live review), `architect`, `code-reviewer`, `qa` REVIEW BEFORE MERGE (gate G).
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
--
-- NOT here: the staff_actor_self_select / staff_actor_self_lock policy pair of ADR 0119 s9.5(b). It is not required by
-- any flow S4b re-asserts (K2 / K3 / HSEC run in a tenant session for a tenant actor or an acting session for the
-- acting platform principal, and withdrawal approve is tenant-scoped: the actor's own row is already visible in each);
-- it lands with the first flow that needs it (K1 platform-scope approvals, the staff lifecycle; ADR 0119 S4b-R3).
-- Not here either: staff_users.staff_version (0139, S5 + S6). No role, password or attribute change.
SET LOCAL lock_timeout = '5s';

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        EXECUTE 'GRANT UPDATE (mfa_at, assurance_kid, assurance_seal) ON sessions TO igaming_runtime';
    END IF;
END
$$;
