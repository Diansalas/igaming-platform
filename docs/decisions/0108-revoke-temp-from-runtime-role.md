# ADR 0108 — Revoke TEMPORARY from the runtime role and from PUBLIC (PRH-2 R2, TRIGGER-SEARCH-PATH-1)

- **Status:** IMPLEMENTED on branch `prh2-r2-temp-revoke` (migration `0116_revoke_temp_from_runtime`); not merged,
  not yet independently re-reviewed by `security` / `code-reviewer`. Owner-authorized fix (smallest safe change).
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

- It closes the exploitation path for ALL older unpinned functions at once, because the shadow cannot be created.
- It does **not** replace pinning. TRIGGER-SEARCH-PATH-1 **residual**: the unpinned 0026..0113 functions remain
  latent defence in depth. They are still shadowable by any principal that holds `TEMPORARY` or CREATE on a schema
  earlier in the path (a future grant, an owner-role session, a different database role). Pinning them
  (`SET search_path = pg_catalog, public, pg_temp` or schema-qualifying) stays as follow-up work and was not done
  here by instruction. The K2 four-eyes mechanism is not rewritten.
- The acting_* RLS read exposure (security finding 2) is closed in practice by the same revoke and remains under the
  pinning residual.
- ADR 0101 section 27.5 residual (K3 (d)/(c2) findings trust executed K2 compensating debits) is closed with the
  shadowing path; the pinning residual still applies.

## 5. Deployment notes

- **The migration role must own the database** (or hold `TEMPORARY WITH GRANT OPTION`). Otherwise `REVOKE` only
  warns and the migration fails on its assertion, by design. On RDS the master user that creates the database owns
  it; if the database was created by another role, transfer ownership first or run the revoke as the owner.
- Revoking `PUBLIC` affects every non-owner role on the database. Any other non-owner role that legitimately needs
  TEMP must be granted it explicitly and its use reviewed against this invariant. None exists in the repository.
- Production code needs no TEMP: `git grep` finds no non-test use. A static test
  (`TestNoRuntimeTempObjectsInProductionCode`) fails if a non-test `.go` file under `cmd/` or `internal/` gains
  `CREATE TEMP`, `SELECT ... INTO TEMP` or a `pg_temp.` object, so nobody reintroduces it without revisiting this
  ADR.
- Large sorts and hash spills use temp FILES, not the TEMPORARY privilege, and are unaffected.
- A database restored or recreated from scratch starts with the PostgreSQL default until 0116 or the provisioning
  script is applied (runbook section 7, step 5).

## 6. Tests

All runtime-role probes assert `NOT rolsuper AND NOT rolbypassrls` first.

- Catalogue: runtime and `PUBLIC` have no TEMP; the owner keeps it.
- Every form fails with 42501 for the runtime role: `CREATE TEMP TABLE`, `CREATE TEMP TABLE AS`,
  `SELECT INTO TEMP`, `CREATE TEMP VIEW`, `CREATE TEMP SEQUENCE`, `CREATE TABLE pg_temp.x`,
  `CREATE FUNCTION pg_temp.f()`, direct, inside a `DO` block, and with `SET LOCAL search_path = pg_temp, public`;
  the same statements succeed for the owner.
- The K2 attack from the security review, rebuilt as a test, fails at TEMP creation, and no compensating entry is posted.
- A genuine two-person adjustment, driven as the runtime role, still submits, is approved by the second person and
  executes exactly once; self-approval and a replayed decision are refused.
- Migration: up on a fresh database, down, up again, `migrate verify` clean; the assertion fires when the migration
  runs as a non-owner role, and the runtime-holds-TEMP assertion fires by simulation; a direct runtime grant is also
  removed.
- Static guard on production code; provisioning scripts carry the revoke.
- Two older tests that created TEMP tables as the runtime role (`kyc_worker_catalogue_gate`, kyc outbox L-1) now
  probe on the owner pool, with a comment.

Mutation evidence: `docs/plans/payment-readiness/evidence/prh2-r2-temp-revoke-mutation-kill.txt`.
