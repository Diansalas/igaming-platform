# ADR 0108 — Revoke TEMPORARY from the runtime role and from PUBLIC (PRH-2 R2, TRIGGER-SEARCH-PATH-1)

- **Status:** IMPLEMENTED on branch `prh2-r2-temp-revoke` (migration `0116_revoke_temp_from_runtime`); not merged.
  Reviewed by `security`, `ledger-finance` and `code-reviewer` (ACCEPT / READY WITH CONDITIONS); the conditions are
  applied in the fix batch recorded in section 7. Owner-authorized fix (smallest safe change).
- **Decision type:** database privilege hardening; no new service boundary, no change to how tenant isolation is
  enforced, no change to the ledger account model.
- **Registry:** TRIGGER-SEARCH-PATH-1 and TRIGGER-SEARCH-PATH-1-UPGRADE (exploitation path closed; pinning residual open).
- **Source:** `docs/plans/prh2-hardening-round/reviews/k3-delta-security.md` finding 1.

## 1. Invariant

**The runtime role (`igaming_runtime`) cannot create temporary objects** (TEMP tables, views, sequences, functions,
anything in `pg_temp`), by any statement form, in any session setting.

## 2. Root cause

PostgreSQL grants `TEMPORARY` on every database to `PUBLIC` by default, so `igaming_runtime` inherited it. The
0026..0113 guard and helper functions (`ledger_adjustment_requests_guard`, `ledger_adjustment_approvals_guard`,
`financial_actor_session`, `financial_acting_session_valid`, `staff_capability_grant_in_force`,
`ledger_adjustment_eligible_grant`, ...) have no pinned `search_path` and look up tables without a schema prefix.
Relations are resolved with `pg_temp` first, so a session that creates TEMP tables named `staff_users`,
`staff_capability_grants` and `staff_capability_grant_requests` replaces what those functions read. Security
reproduced this as the runtime role: two invented staff, a Submit and a Decide, and a real compensating debit
executed with no real human approver. Anyone able to run arbitrary SQL as the runtime role (a compromised
application, SQL injection) could do this. K3's own functions are pinned and were not affected.

## 3. Decision

Migration `0116_revoke_temp_from_runtime`:

1. `EXECUTE format('REVOKE TEMPORARY ON DATABASE %I FROM PUBLIC', current_database())`. The database name is never
   hard-coded.
2. The same revoke from `igaming_runtime`, only if that role exists (`pg_roles` check), so the migration is
   deterministic where the role is created later. Revoking from the role alone is a no-op while `PUBLIC` holds the
   privilege, because the role inherits it; this is proven by a test (a role-only revoke leaves
   `has_database_privilege` true) and by the mutation run.
3. ASSERTS the end state and RAISES (`TEMP-REVOKE-1`, SQLSTATE 42501) if `PUBLIC` still holds `TEMPORARY`
   (`aclexplode` on `pg_database.datacl`, with `acldefault` for a NULL ACL) or `igaming_runtime` still holds it
   (`has_database_privilege`, which also covers membership). A silent no-op (for example a migration role that does
   not own the database, where `REVOKE` only warns) is impossible.

Down: `GRANT TEMPORARY ON DATABASE <current> TO PUBLIC`, restoring the PostgreSQL default. Dev/CI only; the file
header carries the warning.

The same two revokes are added to `deploy/init-app-role.sql` and `deploy/aws/sql/init-runtime-role.rds.sql`, so a
freshly provisioned database is safe before migrations run. No role, password or attribute is changed. Terraform is
untouched.

The database owner (the migration role, `igaming`) keeps `TEMPORARY` as the owner; tests that probe with TEMP tables
run on the owner pool.

## 4. Scope and what this does NOT do

- It closes the exploitation path **for the runtime role and other non-owner roles**, for ALL older unpinned functions
  at once, because the shadow cannot be created. **The database owner (the migration role) deliberately keeps
  TEMPORARY** (as the database owner it cannot be revoked in any useful way, and the migration and probing tests
  need it); any session that connects as the owner can still shadow the unpinned functions.
- It does **not** replace pinning. TRIGGER-SEARCH-PATH-1 **residual**: the unpinned 0026..0113 functions remain
  latent defence in depth. They are still shadowable by any principal that holds `TEMPORARY` or CREATE on a schema
  earlier in the path (the owner, a future grant, a different database role). Pinning them
  (`SET search_path = pg_catalog, public, pg_temp` or schema-qualifying) stays as follow-up work and was not done
  here by instruction. The K2 four-eyes mechanism is not rewritten.
- The acting_* RLS read exposure (security finding 2 of the K3 delta review) is closed in practice by the same
  revoke and remains under the pinning residual.
- **ADR 0101 section 27.5 residual** (K3 (d)/(c2) findings trust executed K2 compensating debits): the dependency is
  satisfied **only after this branch is merged, and only while conditions C1-C3 hold**:
  - **C1** the platform binary connects as `igaming_runtime`, never as the database owner (PLAT-ROLESPLIT-1; a real-money
    precondition; `make run` and `.env.example` still run the app as the owner `igaming` in development, which keeps
    TEMP, so development is not covered by this closure);
  - **C2** 0116's end state is verified on the serving database, including after any restore or recreate (runbook section 7
    step 5; production startup also fails closed, section 5);
  - **C3** no other principal that can write K2 rows is granted TEMPORARY, or CREATE on any schema.
  The pinning residual still applies in all cases.

## 5. Deployment notes

- **The migration role must own the database** (or hold `TEMPORARY WITH GRANT OPTION`). Otherwise `REVOKE` only
  warns and the migration fails on its assertion, by design. On RDS the master user that creates the database owns
  it; if the database was created by another role, transfer ownership first or run the revoke as the owner.
- **Apply 0116 BEFORE runtime traffic, or recycle the runtime sessions.** `REVOKE` stops new TEMP creation, but a
  runtime backend that created a TEMP table BEFORE the revoke keeps it (reproduced by security: the table stays
  writable and still shadows the unqualified name for the life of that backend). After applying 0116 (or the
  provisioning revoke) to a database that has live runtime sessions, recycle the runtime connection pools or
  terminate the `igaming_runtime` backends (`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE
  usename = 'igaming_runtime' AND pid <> pg_backend_pid()`). The AWS flow applies it before the service starts.
- **A restore or recreate loses the ACL, and `migrate up` will NOT re-apply it.** Database-level privileges are not
  carried by `CREATE DATABASE` or by a logical restore without `--create`, but `schema_migrations` still lists 0116,
  so `migrate up` is a no-op. Run the 0116 up SQL (or the environment's role-provisioning script) directly as the
  database OWNER, then run the verification query in the runbook (section 7 step 5). Production startup now
  FAILS CLOSED when the connecting role holds TEMP (`db.VerifyRuntimeRoleInProduction`, `ConnectingRoleHoldsTemp`),
  so a restored database that was not repaired refuses to serve rather than silently reopening the hole. Physical
  snapshot / PITR restores are expected to keep the ACL but are not tested.
- **`down` is not runnable in deployed environments.** `cmd/migrate down` refuses unless `APP_ENV` is explicitly
  `development` or `staging` (production and unset are refused, mirroring `GuardEnvironment`); the library function
  `MigrateDown` that tests call is not guarded. The 0116 down file keeps its dev/CI-only warning.
- Revoking `PUBLIC` affects every non-owner role on the database. Any other non-owner role that legitimately needs
  TEMP must be granted it explicitly and its use reviewed against this invariant. None exists in the repository.
- Production code needs no TEMP: `git grep` finds no non-test use. A static test
  (`TestNoRuntimeTempObjectsInProductionCode`) fails if a non-test `.go` file under `cmd/` or `internal/` gains
  `CREATE TEMP`, `SELECT ... INTO TEMP` or a `pg_temp` / `pg_temp_N` qualified object (comments are ignored), so nobody
  reintroduces it without revisiting this ADR.
- Large sorts and hash spills use temp FILES, not the TEMPORARY privilege, and are unaffected (security verified a
  13.9 MB external merge sort and a 17.5 MB hash aggregate as the runtime role).
- Recorded, not done here: (I1) on RDS consider `REVOKE CONNECT ON DATABASE postgres FROM PUBLIC` (the runtime role
  can connect to, and hold TEMP in, other databases on the server; there is no cross-database access, so no tenant
  data is reachable); (I2) a future PostgreSQL 16 membership of `igaming_runtime` with INHERIT FALSE, SET TRUE in a role
  holding TEMP would not be seen by `has_database_privilege` but would allow SET ROLE; any new membership needs review
  against this ADR; (I3) the shared local CI database `igaming_platform_ci_local` predates 0116 and still has PUBLIC TEMP;
  apply 0116 there by running `migrate up` as its owner when the orchestrator says so.

## 6. Tests

All runtime-role probes assert `NOT rolsuper AND NOT rolbypassrls` first.

- Catalogue: runtime and `PUBLIC` have no TEMP; the owner keeps it.
- Every form fails with 42501 for the runtime role: `CREATE TEMP TABLE`, `CREATE TEMP TABLE AS`,
  `SELECT INTO TEMP`, `CREATE TEMP VIEW`, `CREATE TEMP SEQUENCE`, `CREATE TABLE pg_temp.x`,
  `CREATE FUNCTION pg_temp.f()`, direct, inside a `DO` block, and with `SET LOCAL search_path = pg_temp, public`;
  the same statements succeed for the owner.
- The K2 attack from the security review, rebuilt as a test, fails at TEMP creation. The "no compensating entry"
  check runs **without any shadow in place** (the shadows cannot be created), so it is defence in depth: the forged
  identities are refused with CG001, an ungranted real staff member with MA003, and nothing is posted.
  `assertInvariants` (debits equal credits) guards ledger integrity but cannot detect a forged approval.
- Non-vacuity in the repo: on a THROWAWAY scratch database, PUBLIC's TEMP is granted back by the owner and the very
  same forgery executes a real compensating debit (`TestTempRevoke_K2ShadowAttack_SucceedsWhenTempIsGrantedBack`).
- A genuine two-person adjustment, driven as the runtime role, still submits, is approved by the second person and
  executes exactly once; self-approval (MA031), an ungranted approver (MA003) and a replayed decision are refused.
- Migration: staged through 0116 (stable when later migrations land): up on a fresh database, down, up again,
  `migrate verify` clean; the assertion fires when the migration runs as a non-owner role, and the
  runtime-holds-TEMP assertion fires by simulation; a direct runtime grant is also removed.
- Production gate: `VerifyRuntimeRoleInProduction` fails closed when the connecting role holds TEMP (unit and real DB);
  `cmd/migrate` refuses `down` outside development and staging.
- Static guard on production code; provisioning scripts carry the revoke.
- Older tests that created TEMP tables as the runtime role (`kyc_worker_catalogue_gate`, kyc outbox L-1) now probe
  on the owner pool. The catalogue gate enforces the equivalence premise (no role-specific policy, no policy
  expression depending on `current_user` / `session_user` / `current_role` / `pg_has_role`) as a tripwire.

Mutation evidence: `docs/plans/payment-readiness/evidence/prh2-r2-temp-revoke-mutation-kill.txt`.

## 7. Fix batch after review (security, ledger-finance, code-reviewer)

Applied: staged-migration round-trip test (merge hazard with 0117); production startup TEMP check; `down` guard in
`cmd/migrate`; live-session and restore documentation and the exact verification SQL in the runbook; catalogue-gate
equivalence tripwire; real error codes in the defence-in-depth block plus the in-repo non-vacuity test; static-test
regex widened and comments ignored; this ADR's wording (sections 4 and 5).
