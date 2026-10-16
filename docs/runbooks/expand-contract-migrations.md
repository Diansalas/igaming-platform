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
- `verify` tolerates exactly the reserved unused versions declared in `internal/db/migrate.go` (`reservedUnusedVersions`: 143 to 150, 159 and 161
  at this commit); any other gap fails it. Do not add to that list to silence a real gap.
- Data-bearing refusals in `down` files (a down is refused while the data it would destroy exists): `WD097` (0162, a reversed withdrawal/reversal
  exists), `MR099` (0163, a pending or executed mismatch M2 exists), `RL099` (0158, an RG enforcement activation exists), `AM099` (0154,
  a screening row exists), plus the older ones in the migration files. Forward-fix; never remove the guard.
- Rolling deploys run two application versions against one schema (section 3 of the deployment architecture).
- `up` and `down` take a session advisory lock, so two migrators cannot run concurrently.
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
3. Set `lock_timeout` for the migration session where the migration does not set its own, so a blocked migration fails fast
   instead of queueing behind traffic and then blocking all new traffic behind itself. `cmd/migrate` reads only
   `DATABASE_URL`, and a libpq/pgx `options` parameter reaches the session (verified against this repository's `db.Connect`:
   `SHOW lock_timeout` returned `5s`):
   `DATABASE_URL='postgres://<owner>@<host>/<db>?sslmode=require&options=-c%20lock_timeout%3D5s' go run ./cmd/migrate up`
   (credentials via `PGPASSFILE` or the secret store, not in the URL). Migrations that set their own `lock_timeout` override
   it. The drill's own URLs must not carry `options` (its guard refuses it); this is for real migration runs.
4. Run `cmd/migrate up` as the **migration-owner** role, separately from the application rollout, never implicitly at app
   startup (`operational-runbooks.md` section 1).
5. Verify (section 6) before restarting writers.

### 5.1 Pre-building a large index CONCURRENTLY (DEVOPS-0107-INDEX-WINDOW-1 practice; first used by 0135)

`cmd/migrate` applies every file in one transaction, so `CREATE INDEX CONCURRENTLY` cannot run inside a migration. A
migration that adds an index on a production-size table (first case: 0135, ADR 0115 section 2.6, on `ledger_entries` and
`deposit_intents`) is written so the index can be built **out of band, without blocking writes**, before the migration runs:

1. As the migration-owner role, OUTSIDE any transaction (`psql` autocommit), build the exact index the migration names:
   `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_ledger_entries_tenant_player_asset_created ON ledger_entries (tenant_id, player_account_id, asset_code, created_at);`
   `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_deposit_intents_tenant_player_asset_created ON deposit_intents (tenant_id, player_account_id, asset_code, created_at);`
2. Check each is valid: `SELECT indexrelid::regclass, indisvalid, indisready FROM pg_index WHERE indexrelid::regclass::text LIKE 'idx_%_tenant_player_asset_created';`
   A failed concurrent build leaves an INVALID index: `DROP INDEX CONCURRENTLY <name>;` and repeat step 1.
3. Run `cmd/migrate up`. The migration's pre-flight accepts a pre-built index only if it is VALID, READY and its
   `pg_get_indexdef` equals the migration's definition exactly; otherwise it refuses with `RL098` before creating anything.
   The index step is the FIRST step of 0135 (before any `ALTER TABLE` / `ACCESS EXCLUSIVE` step) and EXECUTEs a
   `CREATE INDEX` only when the index is absent (`to_regclass(...) IS NULL`): with a pre-built index NO statement runs
   against `ledger_entries` / `deposit_intents`, so no `SHARE` lock is taken and writers are not blocked (an earlier
   wording here called a `CREATE INDEX IF NOT EXISTS` a "no-op": that was wrong, `IF NOT EXISTS` still takes the
   table's `SHARE` lock before noticing the index exists). The step sets a transaction-local `lock_timeout = 5s` that
   stays in force for the REST of 0135: every later lock (the `ACCESS EXCLUSIVE` steps on `player_restrictions` and
   `casino_launch_sessions`) also waits at most 5 s, then the migration fails `55P03` and rolls back completely, releasing
   any `SHARE` lock the in-migration index build took. Re-run it when the blocking session has finished
   (`pg_stat_activity`, section 5 step 2). Pinned by `TestMigration0135_BlockedAccessExclusiveStepFailsFast`.
   Without a pre-build: on a small table (dev / CI) the index is built in the migration transaction; on a table above
   the threshold (`reltuples > 100000` or more than 64 MB) the migration refuses with `RL098` naming this section.
4. Pinned by `TestMigration0135_PrebuiltConcurrentIndex` (accepted while writers hold `ROW EXCLUSIVE` on both tables,
   wrong definition refused), `TestMigration0135_InvalidPrebuiltIndexRefused` (a cancelled CONCURRENTLY build leaves an
   INVALID index: refused) and `TestMigration0135_LargeTableWithoutPrebuiltIndexRefused` (internal/rg).
5. Rolling 0135 back (`down`): on a production-size ledger first `DROP INDEX CONCURRENTLY` both indexes out of band
   (`DROP INDEX CONCURRENTLY IF EXISTS idx_ledger_entries_tenant_player_asset_created;` and the `deposit_intents` one); the
   down's `DROP INDEX IF EXISTS` is then a no-op. The down also refuses (`RL099`) while any RG configuration, limit
   change, time_out or recorded `casino_launch_sessions.auth_session_root_started_at` exists.
6. `cmd/migrate -steps=1 down` rolls back the LAST-APPLIED migration (by `schema_migrations` order of application), not the
   highest version number: when 0133/0134 (ADR 0113) are applied after 0135 on some database, check
   `cmd/migrate status` before a down.

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
