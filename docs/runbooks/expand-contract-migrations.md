# Expand / contract migrations: policy and operator procedure

Status: policy and procedure document (no code). It consolidates rules that already exist in
`docs/architecture/38-deployment-architecture.md` section 3, `docs/HANDOVER.md` section 22,
`operational-runbooks.md` sections 1, 2 and 17, and `cmd/migrate`. Where this document adds a rule it says so.

## 1. Facts about the tooling (verified at this commit)

- `cmd/migrate` has `up`, `down`, `status`, `verify`. Migrations are `migrations/NNNN_name.up.sql` / `.down.sql`, numbered
  gap-free; **numbers are allocated only by the orchestrator** (never pick one).
- `up` records each applied version with the SHA-256 of its up-file. `verify` re-hashes the on-disk up-files and checks
  version gaps. It does not compare the live schema to the SQL and does not look at down-files.
- `down` **refuses** unless `APP_ENV` is explicitly `development` or `staging` (unset or `production` is refused, checked
  before any connection). Some downs refuse data-bearing states. CI proves reversibility only for the last 4 steps.
- Rolling deploys run two application versions against one schema (section 3 of the deployment architecture).
- A restore or `CREATE DATABASE` does not carry the database ACL (TEMPORARY revoke, migration 0116); `migrate up` will not
  re-apply it (`restore-from-backup.md`).

## 2. Forward-fix versus restore (policy)

| Situation | Action |
|---|---|
| Migration failed and rolled back (transactional DDL), nothing applied | Fix the cause (data or migration), re-run. Migrations are single-transaction; a refusal changes nothing |
| Migration applied, new application version misbehaves | Roll the **application** back (previous build must still work on the new schema: that is what expand-first guarantees), or roll forward with a fix. Do **not** run `down` |
| Migration applied and itself is wrong (bad constraint, wrong default, missing index) | **Forward-fix** with a new migration (additive first). The wrong migration stays in the history |
| Migration damaged or lost data | Declared incident. Compensating entries first; restore (`restore-from-backup.md`) only by human decision, accepting loss of everything after the restore point |
| Development or staging database in a bad state | `down`/rebuild from migrations is fine (synthetic data), with `APP_ENV=development|staging` |
| Production | Never `down`. Never hand-edit `schema_migrations`. Never edit an applied up-file (it breaks `verify` and diverges environments) |

Rationale (CLAUDE.md): the ledger is append-only, corrections are compensating entries, and a database restore discards
committed financial history. Restore is the exception, not the rollback mechanism.

## 3. Expand / migrate / contract

Every schema change that the previous application version depends on is split across releases (this restates the
standing rule in the deployment architecture as an operator checklist):

1. **Expand (release N).** Add the new shape only: new nullable column or one with a default, new table, new index
   (`CREATE INDEX CONCURRENTLY` is not available inside a migration transaction: schedule large index builds
   deliberately and keep them in a write-quiet window), new constraint added `NOT VALID` where supported, new trigger or
   function that is a no-op for old writers. The old binary must keep working unchanged.
2. **Migrate (release N+1).** Deploy code that uses the new shape exclusively and backfill in bounded batches. Backfills
   that touch financial tables never `UPDATE` historical ledger rows: they add rows or fill derived columns that the
   immutability triggers allow.
3. **Contract (release N+2 at the earliest, after every old replica has drained).** Drop the old shape, validate the
   `NOT VALID` constraint, tighten nullability.

Additional rules for this codebase:

- A new `NOT NULL` column needs a default or a backfill step first (old INSERTs do not know it exists).
- RLS policy, trigger and function changes are schema changes: they must be in a migration, and the fingerprint tooling
  (`deploy/drill/verify.sql`) is how you compare environments.
- New guard triggers that can refuse existing data (the 0132 model) **must pre-flight first** (section 4).
- Functions pin `search_path`; a new `SECURITY DEFINER` function needs `security` review.
- Grants to the runtime role are explicit in the migration (the CI harness and `init-app-role.sql` default grants are not
  a substitute). A grant to a differently named runtime role needs its own reviewed migration.

## 4. Pre-flight refusals (the WD098 model)

Migration 0132 is the reference. Before creating anything it scans every tenant (per-tenant `set_config` loop, because the
tables are FORCE RLS and a plain `SELECT` is blind) and raises `WD098` with per-class counts if existing rows violate the
new rules. Nothing is changed and the transaction rolls back. Operator procedure:

1. Read the counts in the error. Do not patch rows to make the migration pass (append-only ledger).
2. Classify the database: throwaway/dev (rebuild from migrations; synthetic data), or staging/production-like (STOP and
   escalate to ledger-finance and a human; any remedy is a compensating entry committed **before** the migration).
   The full table is `operational-runbooks.md` section 17.
3. Re-run only when the remedy is committed. A refusal is safe to repeat.

When authoring a migration with a pre-flight: use a distinct SQLSTATE class per refusal, report counts per class, never
fix data inside the pre-flight, and include an RLS-independent count where an RLS-blind scan could under-report.

## 5. Write-quiet window and locks

Some migrations take strong table locks for their scan or for lifting FORCE RLS briefly (0132: `SHARE ROW EXCLUSIVE`, then
`ACCESS EXCLUSIVE` on three ledger tables). For these:

1. Announce a window; stop or drain writers (scale `platform-api` down or fence the load balancer). Background loops
   (payments sweeper, alert dispatcher, reconciliation) run **inside** `platform-api`, so draining it stops them too.
2. Check for long-running transactions first: `SELECT pid, now()-xact_start, state, left(query,80) FROM pg_stat_activity WHERE xact_start IS NOT NULL ORDER BY 2 DESC`.
   A writer holding a row lock can deadlock with the migration; PostgreSQL aborts one side. If the migration loses it
   rolls back cleanly and can be re-run.
3. Set a statement-level guard for the migration session where the migration does not set its own (`lock_timeout`), so a
   blocked migration fails fast instead of queueing behind traffic and then blocking all new traffic behind itself.
4. Run `cmd/migrate up` as the **migration-owner** role, separately from the application rollout, never implicitly at app
   startup (`operational-runbooks.md` section 1).
5. Verify (section 6) before restarting writers.

## 6. Procedure for a production-like migration

1. Take and **verify** a backup first. Today none exists for any deployed environment (`NOT MET`): this is a launch
   blocker for any real-money environment, not a step to skip.
2. Rehearse on a rebuilt copy: create a scratch database, load the previous schema (`migrate up` to the prior head, seed or
   restore a representative snapshot), apply the new migration, run `deploy/drill/verify.sql` and the integration suite.
3. Run the pre-flight read-only checks the migration documents (e.g. the 0132 classes) in advance.
4. Write-quiet window if section 5 applies. `DATABASE_URL=<owner url> go run ./cmd/migrate up`.
5. `go run ./cmd/migrate verify` (checksums, gaps). Re-run `deploy/drill/verify.sql` and compare fingerprints with the
   rehearsal.
6. Confirm role privileges the migration relies on (e.g. `has_function_privilege('<runtime role>', ...)` for 0132).
7. Roll the application; watch `/readyz`, the 5xx ratio and the reconciliation cycle (observability rules are NOT wired:
   watch by hand).
8. If the migration is wrong: section 2. Forward-fix.

## 7. What this does not cover

No automated expand/contract linter exists (a reviewer enforces it); `verify` does not detect a hand-altered live schema
(use the fingerprints); there is no production database or deploy pipeline to rehearse on (AWS off).
