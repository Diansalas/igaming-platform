# Runtime / Migration-Owner Role Separation (`PLAT-ROLESPLIT-1`)

**Status: IMPLEMENTED IN-REPO. Remains a PRODUCTION BLOCKER only for the
genuinely external, non-repository action described in §9 below.**

Everything this repository's own code, migrations-bootstrap, CI, and test
suite can do to close `PLAT-ROLESPLIT-1` has been done (§9 records exactly
what and where). What is described through §8 below is the original
design/verification record from the pass that specified the fix, kept
intact for its reasoning and empirical evidence; §9 is the later pass that
actually rolled it into the codebase. **This session (§9's pass) still
did not, and could not, execute anything against a real production
database** — it has no production credential and CLAUDE.md's Environment
Safety rule forbids requesting one. Everything verified in this document,
including §9's, was run against local development/CI Postgres only, never
production data.

## 1. The problem, precisely

The application's single database role, `igaming`:

- owns the database (`igaming_platform_dev` in dev; the production
  database's owner today is presumed identical in shape),
- owns the `public` schema,
- owns every table in it (it ran every migration), and
- is the SAME credential the running HTTP service uses for ordinary
  request traffic.

PostgreSQL's row-level security model **never applies to a table's
owner**, regardless of `ENABLE ROW LEVEL SECURITY` or `FORCE ROW LEVEL
SECURITY` — `FORCE` only makes RLS apply to the owner's own **DML**, and
even that is trivially bypassed because the owner can simply turn `FORCE`
or RLS itself back off, since altering either is *also* an owner-only
operation with no RLS policy that could ever stop it. Every RLS policy
this platform has ever written (migrations 0044, 0045, 0075, 0076, 0077)
is therefore enforced against every caller **except** the one credential
the internet-facing application actually uses.

## 2. Required migration-owner role

**Keep the existing `igaming` role in this exact function, unchanged.**
It should be used ONLY by `cmd/migrate` (or an equivalent deploy-time
step), never embedded in the running application's runtime configuration
after this change lands.

Required privileges (already held, no change needed):
- Owner of the database, schema, and all tables/sequences/functions/
  triggers.
- Implicitly: `CREATE`, `ALTER`, `DROP`, `TRUNCATE` on everything it owns,
  and the ability to enable/disable RLS and triggers on its own objects
  (all owner-implicit; no explicit grant needed or possible to remove
  while it remains the owner).

> **Stage 10 W0 note — test-only admin role.** CI and local development
> additionally create `igaming_test_admin` (`CREATEDB`, `NOSUPERUSER`,
> `NOBYPASSRLS`, member of the migration-owner role) solely so
> `internal/testsupport/scratchdb` can create and drop scratch databases
> for migration/RLS tests. It is **not** part of this document's
> deployment model, is never created by `deploy/init-app-role.sql`, and
> does not change either application role (both stay `NOCREATEDB`). See
> `docs/testing/testing-strategy.md` "Scratch databases".

## 3. Required runtime application role

Create a new role — this document uses `igaming_runtime` as the example
name; the actual production naming convention is an infra decision, not
an engineering one.

Required privileges, and only these:
- `LOGIN` (it is the credential the application connects with).
- `CONNECT` on the database.
- `USAGE` on the `public` schema.
- `SELECT, INSERT, UPDATE, DELETE` on every application table.
- `USAGE, SELECT` on every sequence (for `nextval`/`currval` on any
  `SERIAL`/`IDENTITY` columns the schema still uses, if any — most tables
  in this schema use `uuid` primary keys generated in Go, but sequence
  privilege is harmless to grant broadly and avoids a silent failure if
  one exists).
- Nothing else.

Explicitly do **not** grant, and confirm these are absent (they are
absent by default for a freshly created role — the risk is a manual
`GRANT` later reintroducing one of them, not the initial creation):
- `TRUNCATE` on any table.
- `REFERENCES`, `TRIGGER` on any table (the ability to create/alter FKs
  or triggers — not needed to have existing triggers fire, only to
  create new ones).
- `CREATE` on the schema or database (blocks `CREATE TABLE`/`CREATE
  INDEX`/etc.).
- Any form of table/schema/database ownership or `ALTER ... OWNER TO`.
- Role attributes: `SUPERUSER`, `CREATEROLE`, `CREATEDB`, `BYPASSRLS`,
  `REPLICATION`.

## 4. Direct answers to the six capability questions

Verified empirically (§5), not inferred:

| Capability | Runtime role can do this? |
|---|---|
| `CREATE ROLE` | **No** (no `CREATEROLE` attribute) |
| `CREATE`/`ALTER`/`DROP` tables | **No** (`permission denied for schema public` / `must be owner of table`) |
| `ALTER TABLE` (any form) | **No** (`must be owner of table ...`) |
| `DISABLE ROW LEVEL SECURITY` | **No** (`must be owner of table ...` — this is the specific capability that matters most; it is refused for the identical reason as any other `ALTER TABLE`, because disabling RLS is itself an `ALTER TABLE` subcommand) |
| `DISABLE TRIGGER` | **No** (`must be owner of table ...`, same reasoning) |
| `TRUNCATE` | **No** (`permission denied for table ...` — distinct from the owner-only cases above: `TRUNCATE` is grantable independently of ownership, and this role is simply never granted it) |
| Modify schema (`CREATE TABLE`, etc.) | **No** (`permission denied for schema public`). |
| `CREATE TEMP TABLE` / any object in `pg_temp` | **No since migration 0116** (`permission denied to create temporary tables in database`, SQLSTATE 42501) - see the TEMP section below. |
| Ordinary `SELECT`/`INSERT`/`UPDATE`/`DELETE` under RLS | **Yes**, and — unlike the current `igaming` credential — genuinely enforced by RLS, because a non-owner role is never exempt regardless of any `FORCE` setting or its absence |


### TEMPORARY privilege (PRH-2 R2, migration 0116, ADR 0108)

**Invariant: the runtime role cannot create temporary objects.** PostgreSQL grants `TEMPORARY` on every
database to `PUBLIC`, and `igaming_runtime` inherited it from there. An earlier version of this document judged
`CREATE TEMP TABLE` harmless; that judgement was withdrawn on 2026-10-05 after the K3 delta security review
reproduced a bypass of the K2 four-eyes check: a TEMP table named like `staff_users` shadows the unqualified
name inside a trigger/guard function with no pinned `search_path`. Migration `0116_revoke_temp_from_runtime`
revokes `TEMPORARY` on the current database from `PUBLIC` and from `igaming_runtime` (a role-only revoke is a
no-op while `PUBLIC` holds the privilege) and ASSERTS the end state, raising if it cannot be established. The
same revoke is in `deploy/init-app-role.sql` and `deploy/aws/sql/init-runtime-role.rds.sql`. The owner role
keeps `TEMPORARY` as the database owner. Deployment requirement: the migration role must OWN the database
(on RDS the master user does). Tests: `internal/db/temp_revoke_integration_test.go`,
`internal/adjustment/temp_revoke_integration_test.go`, and the static guard
`TestNoRuntimeTempObjectsInProductionCode`. This closes the exploitation path for the older unpinned functions
but does not replace pinning them (TRIGGER-SEARCH-PATH-1 residual, defence in depth).

**Independently re-verified and extended by the security review with 20
additional escalation probes** (`SET session_replication_role =
'replica'` — the classic trigger-bypass — `ALTER TABLE ... OWNER TO`,
`DROP`/`CREATE POLICY`, `SET ROLE igaming`, `SECURITY DEFINER` function
creation, `pg_authid` reads, `ALTER ROLE ... BYPASSRLS`, `CREATE SCHEMA`,
`GRANT ... WITH GRANT OPTION`, and more): all denied. Also confirmed
there are zero `SECURITY DEFINER` functions anywhere in this database
today (all 96 application and pgcrypto functions are `prosecdef =
false`), so no existing trigger or function confers elevated privilege to
the runtime role by way of invoking it, and sequence grants are
`USAGE, SELECT` only (no `UPDATE`, so no `setval`).

**Exceptions to "ordinary DML is genuinely enforced by RLS," corrected by
the Stage 9 architect review (ARCH-DB-1) — SIX tables, not one.** (Read
this paragraph as the HISTORICAL record of the finding; the "Stage 9.1
update" paragraph below it records the actual fix, migration `0084`,
which closed the "no RLS at all" gap described here.)
`casino_games` (the global game catalogue, migration 0035) had row-level
security disabled entirely (`relrowsecurity = false`) and no `tenant_id`
column — deliberately global, not tenant-owned data. Stage 6's migration
0078 added **five more** in the identical shape: `sb_sports`,
`sb_competitions`, `sb_events`, `sb_markets`, `sb_selections` (the
sportsbook catalogue, mirroring `casino_games`' own precedent per that
migration's own header comment). This document's earlier draft named only
`casino_games`; that was accurate when written (Stage 4I exit triage,
before migration 0078 existed) and is now factually incomplete. The
blanket `GRANT ... ON ALL TABLES IN SCHEMA public` in §6 gives the runtime
role unconstrained `INSERT`/`UPDATE`/`DELETE` on all six of these tables
from any connection, RLS or no RLS. This is pre-existing (not introduced
by this split) and not a tenant-isolation issue (none of the six carries
tenant-owned data), but the claim above should not be read as "every
table" without this six-table exception. **The architect review separately
classified the six tables' complete lack of a DB-level write backstop as
`ARCH-DB-2` (HIGH, FIX BEFORE PRODUCTION)** — a cross-domain fix (routing
`casino_games`' admin writer through `db.Pool.WithPlatformAdmin` and
giving the sportsbook catalogue sync a defined service-identity scope,
mirroring the `assets` registry's own `ENABLE`+`FORCE` RLS precedent) that
this role-split document does not attempt to close and that the split
itself cannot substitute for.

**Stage 9.1 update — `ARCH-DB-2` is now CLOSED. This exception is
HISTORICAL** (kept below, struck through in substance rather than
deleted, because the surrounding numbered exception list and its
reasoning remain useful context for why the gap existed in the first
place).
`docs/decisions/0081-arch-db-2-catalogue-write-authorization.md` is the
architect's decision for all six tables, implemented by migration
**`0084`** (not `0083` — see the renumbering note at the top of that ADR:
by the time this ruling was implemented, a separate, parallel Stage 9.1
devops workstream, PLAT-MIGDRIFT-1, had already landed migration `0083`
for schema-migration checksum tracking, an unrelated change, so this
migration became `0084` instead; no design changed as a result). Its
substance, so this section can be read without following the link:

- The six tables stay **permanently without a `tenant_id`/`brand_id`
  column**, and ADR 0081 §2.2 rules that adding one would be
  *semantically wrong*, not merely unnecessary — they are single-canonical-
  row platform catalogue data, and per-tenant scoping already lives where
  it belongs (`casino_game_availability` for casino; a future, separate
  tenant-owned availability table for sportsbook). So the "no `tenant_id`
  column" half of this exception is **permanent and intentional**, and the
  regression test that asserts it keeps asserting it.
- The "no RLS at all" half **is now closed** by migration `0084`: all six
  tables have `ENABLE` + `FORCE ROW LEVEL SECURITY`, with
  `FOR SELECT USING (true)` (all five `sb_*` tables have genuinely
  anonymous readers — `GET /v1/sportsbook/sports` and
  `GET /v1/sportsbook/events/{id}` are unauthenticated routes, confirmed
  still returning data end-to-end after this change) and a write policy
  scoped to a platform identity: the existing
  `app.platform_admin_principal_id` GUC for `casino_games`
  (`db.Pool.WithPlatformAdmin`, used by `newUpsertCasinoGameHandler` and
  asserted independently in Go by `casino.UpsertGame`), and a new,
  closed-vocabulary `app.platform_service_id = 'sportsbook_catalogue_sync'`
  GUC for the five `sb_*` tables, set only by the new
  `db.Pool.WithPlatformService` (`internal/db/platform_service.go`) and
  asserted independently in Go by `sportsbook.SyncCatalogue` via
  `db.AssertPlatformServiceScope`. The sportsbook catalogue sync
  (`cmd/platform-api/main.go`'s startup call) now uses
  `WithPlatformService` instead of `WithoutTenant`.
- Plus a shared `catalogue_enforce_immutable_identity()` trigger function
  freezing each table's identity and parent-link columns only (never
  price/status — `sb_selections.odds_*` are deliberately left mutable),
  and deny-DELETE / deny-TRUNCATE triggers on all six (TRUNCATE reuses the
  existing shared `ledger_deny_mutation()` from migration `0021`, this
  repo's established convention).

**§6's blanket `GRANT … ON ALL TABLES IN SCHEMA public` is now
constrained for these six tables by row-level security, exactly like
every other RLS-protected table in this schema** — the runtime role's
raw `INSERT`/`UPDATE`/`DELETE` grant still exists at the SQL-privilege
level (unchanged, and not the mechanism that matters here — see this
document's own "critical structural point" below), but the policies
introduced by migration `0084` are what actually decide whether a given
connection's write takes effect, per this document's general model for
every other table.

`TestRuntimeRole_CasinoGamesExceptionUnchanged` (§9 point 5,
`internal/db/runtime_role_separation_test.go`) has been REPLACED by
`TestRuntimeRole_PlatformCatalogueTablesForceRLSAndHaveNoTenantColumn`,
per ADR 0081 §7.5: it asserts `relrowsecurity AND relforcerowsecurity`
on all six tables, asserts the absence of a `tenant_id`/`brand_id` column
as an intentional positive invariant, and asserts the expected policy
names (`<table>_read`, plus either `<table>_platform_admin_*` for
`casino_games` or `<table>_catalogue_sync_*` for the five `sb_*` tables)
are present. A separate, direct-SQL adversarial suite,
`internal/db/catalogue_write_authorization_integration_test.go`, exercises
the write policies, immutable-identity triggers, and deny-DELETE/
deny-TRUNCATE triggers themselves under every connection scope
(`WithTenant`, `WithPlayerScope`, `WithoutTenant`, `WithPlatformAdmin`,
`WithPlatformService`) — the level of verification this document's §5
adversarial-probe suite already applies to the runtime-role split itself,
now applied to ARCH-DB-2's fix too.

**A second exception, not yet live but worth a standing rule:** this
database currently has zero `VIEW`s, so there is no current exposure —
but a view created later by the owning `igaming` role WITHOUT
`security_invoker = true` runs with the owner's rights regardless of who
queries it, handing any caller (including the runtime role) a complete
RLS bypass through that view; §6's `ALTER DEFAULT PRIVILEGES ... ON
TABLES` would also auto-grant `SELECT` on it. Add to the migration-owner
role's own discipline: any future view must be created with
`security_invoker = true`, or it defeats this entire split.

**The critical structural point:** none of the six denials above depend
on `CREATEROLE`. `rolcreaterole=false` on the *existing* `igaming` role is
why *that* role cannot self-provision `igaming_runtime` from inside the
application or a migration — it is not itself the mechanism that makes
the new role safe. The new role is safe purely because it does not own
anything. `CREATEROLE` is only needed, once, by whoever runs the
provisioning script in §6 (an operator's own admin credential, or
Postgres's `postgres` superuser) — not by anything this repository's own
code or migration-owner role ever needs to hold.

## 5. Verification performed this pass (local dev Postgres, not production)

Ran directly against this session's local development database
(`igaming_platform_dev`, Postgres 16), using a temporary role created and
dropped within the same session — no schema, migration, or application
file was changed to do this:

```sql
CREATE ROLE igaming_runtime_verify LOGIN PASSWORD '...' 
  NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
GRANT CONNECT ON DATABASE igaming_platform_dev TO igaming_runtime_verify;
GRANT USAGE ON SCHEMA public TO igaming_runtime_verify;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO igaming_runtime_verify;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO igaming_runtime_verify;
```

Then, connected AS that role and attempted every capability
`PLAT-ROLESPLIT-1`'s live-reproduced attack chain used against the
current `igaming`-owned setup:

```
ALTER TABLE tenants DISABLE ROW LEVEL SECURITY;   -- ERROR: must be owner of table tenants
ALTER TABLE tenants DISABLE TRIGGER USER;          -- ERROR: must be owner of table tenants
TRUNCATE tenants CASCADE;                          -- ERROR: permission denied for table tenants
DROP TABLE tenants;                                -- ERROR: must be owner of table tenants
ALTER TABLE tenants ADD COLUMN pwned boolean;      -- ERROR: must be owner of table tenants
CREATE TABLE pwned_test(id int);                   -- ERROR: permission denied for schema public
SELECT count(*) FROM tenants;                      -- succeeded (ordinary DML, as intended)
```

Every escalation the adversarial security reviewer demonstrated against
the current single-role setup (Phase E-SECURITY completion report, §I) is
refused under this role split, while ordinary application traffic
continues to work. The role was dropped immediately after verification;
no persistent change was made to the local database, and nothing was
committed to a migration file (this is deliberately not modeled as a
migration — see §7).

**Correction, per the independent security review:** an earlier run of
this verification (this document's first draft) used this session's
long-lived local `igaming_platform_dev` database without first
confirming its actual live state matched the current migration files.
The security review found that database had drifted (a stale, pre-fix
`tenants_read` policy and several stale `internal/operatingmarket` test
failures — both artifacts of this database having migration 0077 applied
before that file's own later in-place amendments, per the `MKT-MIG76-1`
hazard this codebase already documents, not a defect in the committed
code). **The database was dropped and rebuilt fresh from HEAD's migration
files before finalizing this document**, and the ownership/privilege
verification above was re-confirmed against that rebuilt database; the
previously-drifted tests (`TestTenantsRLS_PlayerScopedConnectionReadsZeroTenants`
plus ten `internal/operatingmarket` tests) all pass against it, and the
full 30-package `-tags=integration` suite is green. The six ownership
probes themselves were unaffected by the drift either way — Postgres
ownership/privilege semantics are independent of any RLS policy's
predicate content — but a reviewer re-running this section against a
long-lived, never-rebuilt database should rebuild it fresh first (`DROP
DATABASE` + `CREATE DATABASE` + `cmd/migrate up`) rather than assume its
current state matches the committed migration files, exactly as this
codebase's own `MKT-MIG76-1` note already advises.

**Operational consequence flagged by the independent security review, not
yet addressed by any CI change:** this verification was performed once,
by hand, against a role manually created for the purpose. This
repository's actual CI and local `make test-integration` runs both
connect as the migration-owner role (`igaming`), never as a
non-owning runtime role — so unless a second CI job is added that runs
the integration suite AS the eventual `igaming_runtime` role, nothing
will continue to verify that this split behaves correctly on every future
change (a future migration or handler could introduce an operation that
only an owner can perform, and CI would not catch it). Recording this as
a follow-up recommendation, not implementing it this pass, since it is a
CI/tooling change beyond a documentation-only triage's scope; see the new
task-registry note.

**Closed by §9, below:** `internal/db/runtime_role_separation_test.go` is
the permanent regression test this paragraph called for. It reproduces
every probe above (plus the six additional ones the independent security
review's 20-probe extension added — `ALTER TABLE ... OWNER TO`, `DROP`/
`ALTER POLICY`, `SET ROLE igaming`, `SET session_replication_role`,
`CREATE ROLE`, `ALTER ROLE ... BYPASSRLS`, `CREATE FUNCTION ...
SECURITY DEFINER`) as ordinary, always-in-tree Go test code, gated on a
new `TEST_RUNTIME_DATABASE_URL` env var so it skips cleanly wherever that
role hasn't been provisioned and runs for real in CI, which now
provisions `igaming_runtime` and sets that var on every run (see §9).

## 6. Minimum infrastructure action required (exact script)

To be run ONCE per environment by an operator holding a role with
`CREATEROLE` (e.g., the database's own superuser, or a dedicated
provisioning credential — never the application's own `igaming` role,
which cannot run this):

```sql
-- 1. Create the new, non-owning runtime role.
CREATE ROLE igaming_runtime LOGIN PASSWORD '<generate a real secret>'
  NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;

-- 2. Grant exactly the privileges in §3.
GRANT CONNECT ON DATABASE igaming_platform_dev TO igaming_runtime;
GRANT USAGE ON SCHEMA public TO igaming_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO igaming_runtime;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO igaming_runtime;

-- 3. Ensure every table `igaming` creates in the FUTURE (via later
--    migrations) automatically extends the same grant to the runtime
--    role, so this split does not silently regress on the next migration.
ALTER DEFAULT PRIVILEGES FOR ROLE igaming IN SCHEMA public
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO igaming_runtime;
ALTER DEFAULT PRIVILEGES FOR ROLE igaming IN SCHEMA public
  GRANT USAGE, SELECT ON SEQUENCES TO igaming_runtime;

-- 3b. Correction from independent review: step 2's blanket
--     "ALL TABLES IN SCHEMA public" also grants the runtime role write
--     access to schema_migrations (the migration ledger `cmd/migrate`
--     owns) - narrow it back to read-only for that one table.
REVOKE INSERT, UPDATE, DELETE ON schema_migrations FROM igaming_runtime;

-- 3c. ADR 0088 §3.5 (Stage 10 W1, sportsbook settlement): the same
--     blanket grant also hands the runtime role UPDATE/DELETE/TRUNCATE on
--     sportsbook_bet_settlements, the append-only settlement history
--     table (migration 0091). The deny triggers on that table
--     (BEFORE UPDATE OR DELETE / BEFORE TRUNCATE, executing
--     ledger_deny_mutation()) are the BINDING control - they bind the
--     table owner too, so no role, however privileged, can actually
--     mutate or truncate a history row through them. This REVOKE is
--     defence in depth only: it exists so that re-running this
--     provisioning script's step 2 (e.g. after a later migration adds a
--     new table) cannot silently re-grant these three privileges and
--     leave only the triggers standing between the runtime role and a
--     write the triggers already refuse. Migration 0091 applies the
--     identical REVOKE itself at migration time; this script's copy is
--     what keeps that narrowing in force across every later re-run of
--     this idempotent bootstrap.
REVOKE UPDATE, DELETE, TRUNCATE ON sportsbook_bet_settlements FROM igaming_runtime;

-- 4. Confirm no unwanted attribute or grant is present:
--    (run as a check, expect the runtime role to show no attributes)
\du+ igaming_runtime
```

Then, entirely outside the database (secrets manager / deploy config, not
this repository): change the running application's `DATABASE_URL`
credential from `igaming` to `igaming_runtime`. Keep `igaming`'s
credential reachable only from the deploy pipeline's migration step, not
from any application server's runtime environment or secret mount.

## 7. What this document deliberately does NOT do (original pass; superseded in part by §9)

- It does not add a migration file. Role/privilege provisioning is an
  environment concern (dev/staging/production have, or will have,
  different credentials and secret stores) — encoding it as a
  version-controlled `up.sql` migration executed by the `igaming` role
  itself would not close the gap (the migration-owner role running the
  script is exactly the role this split exists to stop being the runtime
  identity), and per this stage's directive, "no speculative
  infrastructure automation" was to be built. **Still true after §9**:
  `deploy/init-app-role.sql` is a one-time bootstrap script run by an
  operator/CI step with elevated credentials, not a `cmd/migrate`
  migration — that distinction was preserved when it was extended to
  create `igaming_runtime` too.
- It does not change any Go code. `internal/db.Pool` already connects
  using whatever `DATABASE_URL` it is given; no code assumes the
  connecting role owns anything. Repointing the credential is sufficient.
  **No longer true as stated — see §9.** A small, narrowly-scoped Go
  change was added: `db.VerifyRuntimeRoleInProduction`
  (`internal/db/production_safety.go`), a fail-closed startup check that
  refuses to run in `production` if the connecting role owns tables. This
  does not touch `Pool`'s connection behavior itself (the original claim
  about `Pool` remains true) — it is an additional safety check called
  once at startup, from `cmd/platform-api/main.go`.
- It does not touch RLS policies, migrations, or any jurisdiction/
  licensing/operating-market schema. This is orthogonal hardening of the
  connection identity, not the authorization model those policies
  express. **Still true after §9.**

## 8. Residual note (original pass)

Once this split lands, `PLAT-ROLESPLIT-1` is closed and the corresponding
task-registry entry should be marked resolved with the production
environment's verification output (the same six-probe check as §5, run
against the real production database by whoever executes §6). Until then,
it remains the platform's single highest-severity open item: everything
else in `docs/governance/stage-4i-exit-register.md` is either non-blocking
or requires a human policy decision that has no urgency; this one is a
mechanical, fully specified fix blocked only on someone with the right
credential running four `GRANT` statements.

**Follow-up flagged by the independent architect review, applied in §9:**
`deploy/init-app-role.sql` (the local dev/CI bootstrap) previously
created only the single owning `igaming` role and made it the database
and schema owner — dev and CI kept connecting as the owner even though
production was meant to adopt this split, so no local test signal would
ever catch a future migration or handler that accidentally relies on an
owner-only operation. §9 records exactly what changed: `igaming_runtime`
is now created alongside `igaming` in every dev/CI bootstrap path, paired
with the permanent regression test this section originally asked for
(`internal/db/runtime_role_separation_test.go`). One thing this follow-up
explicitly did NOT do, deliberately: it did not repoint local dev's own
`DATABASE_URL`/`TEST_DATABASE_URL` at `igaming_runtime` — see §9's own
explanation of why a large share of this repository's integration suite
still needs to run as the owning role, and why that is a considered
choice, not an oversight.

## 9. What was actually implemented in-repo, and what still is not

This section records the pass that turned §1-§8's design into committed
code, closing every part of `PLAT-ROLESPLIT-1` that does not require a
real production credential this repository's own session is not permitted
to hold (CLAUDE.md, "Environment safety").

**Implemented, in this repository, verified locally and in CI:**

1. **`deploy/init-app-role.sql`** now creates both `igaming` (unchanged,
   migration-owner) and `igaming_runtime` (non-owning, exactly the §3
   privilege set: `CONNECT`, `USAGE` on schema `public`, `SELECT,
   INSERT, UPDATE, DELETE` on all tables via `ALTER DEFAULT PRIVILEGES
   FOR ROLE igaming IN SCHEMA public` plus direct grants for
   already-existing tables, `USAGE, SELECT` on sequences, and a
   `REVOKE INSERT, UPDATE, DELETE ON schema_migrations FROM
   igaming_runtime` narrowing). Dev-only placeholder password, same
   convention as `igaming`'s own.
2. **`.github/workflows/ci.yml`** provisions `igaming_runtime` on every
   CI run (mirroring the same SQL as `init-app-role.sql`, since CI's
   Postgres service container doesn't run that file directly), narrows
   its `schema_migrations` access after migrations create that table,
   and sets `TEST_RUNTIME_DATABASE_URL` unconditionally so
   `internal/db/runtime_role_separation_test.go`'s adversarial-probe
   suite runs for real on every push/PR, not only by hand. This closes
   the exact "operational consequence... not yet addressed by any CI
   change" gap §5 named.
3. **`Makefile`** gained `dev-db-init-roles` (provisions both roles
   against this sandbox's native, non-Docker Postgres — there is no
   `docker-entrypoint-initdb.d` mechanism available here, so this target
   is the sandbox's equivalent bootstrap path) and
   `test-integration-runtime-role` (runs the new regression test with
   `TEST_RUNTIME_DATABASE_URL` set). `deploy/docker-compose.dev.yml`'s
   comments were updated to explain the same split for the Docker path,
   where `init-app-role.sql` continues to be used directly.
4. **`TEST_DATABASE_URL` was deliberately left pointed at `igaming`,
   unchanged.** A large share of this repository's integration suite
   (every `internal/*/migration_*_test.go` file, e.g.
   `internal/jurisdiction/migration_*_test.go`,
   `internal/operatingmarket/migration_*_test.go`,
   `internal/bonus/wave3_phase2_migrations_integration_test.go`) calls
   `Pool.MigrateUp`/`Pool.MigrateDown` directly against the test
   database and requires owner (DDL) privileges to do that at all.
   Switching `TEST_DATABASE_URL` to the non-owning role would not make
   the suite "more correct" — it would simply break every one of those
   tests, for a reason unrelated to what they actually verify. This is
   why a *second*, additive env var (`TEST_RUNTIME_DATABASE_URL`) was
   introduced instead of repointing the existing one.
5. **`internal/db/runtime_role_separation_test.go`** (`//go:build
   integration`) is the permanent regression test called for throughout
   §5 and §8. It reads `TEST_RUNTIME_DATABASE_URL`, skips cleanly
   (`t.Skip`) if unset, and — connected as `igaming_runtime` — proves
   every one of the following is denied with SQLSTATE `42501`
   (`insufficient_privilege`) and a message naming the specific reason:
   `SET session_replication_role = 'replica'`, `SET ROLE igaming`,
   `ALTER TABLE` (add column), `DROP TABLE`, `TRUNCATE`, `ALTER POLICY`,
   `DROP POLICY`, `ALTER TABLE ... DISABLE ROW LEVEL SECURITY`,
   `ALTER TABLE ... OWNER TO`, `CREATE ROLE`, `ALTER ROLE ... BYPASSRLS`,
   and `CREATE FUNCTION ... SECURITY DEFINER`. It also proves the flip
   side: seeding two tenants' `tenant_jurisdiction_configs` rows using
   the runtime role itself, then confirming a tenant-A-scoped connection
   genuinely reads zero rows for tenant B (not merely "the role is
   denied outright" — RLS is actually enforced for it, unlike `igaming`
   today). A dedicated test,
   `TestRuntimeRole_PlatformCatalogueTablesForceRLSAndHaveNoTenantColumn`,
   confirms the SIX-table catalogue exception's current, post-`ARCH-DB-2`
   state: `casino_games`/`sb_sports`/`sb_competitions`/`sb_events`/
   `sb_markets`/`sb_selections` all have `relrowsecurity AND
   relforcerowsecurity` true (migration `0084`) and none carries a
   `tenant_id`/`brand_id` column (permanent by design, ADR 0081 §2.2), so
   a future schema change to any of the six is caught here rather than
   silently invalidating this document's own claim. (This test replaced
   the earlier `TestRuntimeRole_CasinoGamesExceptionUnchanged`, which
   asserted the OPPOSITE — RLS disabled — before migration `0084` closed
   that gap.)
6. **`internal/db/production_safety.go`** adds
   `ConnectingRoleOwnsNoTables` (queries `pg_tables` for rows the
   connecting role owns in `public`) and
   `VerifyRuntimeRoleInProduction(ctx, environment, checker)`, called
   once from `cmd/platform-api/main.go` immediately after the database
   connects. Gated strictly on `environment == "production"` (the exact
   string, case-sensitive) — development/CI/staging are unaffected,
   because they legitimately and intentionally still connect as
   `igaming` today (see point 4 above). If, in production, the
   connecting role owns any table, startup fails immediately with a
   fatal error naming the exact problem and pointing back at this
   document, converting "an operator forgot to switch the credential in
   production" from a silent, platform-wide RLS bypass into a startup
   crash. `internal/db/production_safety_test.go` unit-tests all four
   branches (non-production never gated, production + non-owning role
   passes, production + owning role fails closed, production +
   ownership-check-itself-fails also fails closed) against a fake
   checker, with no real database required.

**Genuinely NOT implemented — remains an external operational action, not
something this or any future session inside this repository can perform
without a production credential:**

- Creating `igaming_runtime` in the real production database, generating
  its real secret in a real secrets manager (never a value copied from
  `deploy/init-app-role.sql`'s dev placeholder), and repointing
  production's `DATABASE_URL` at it. §6's exact script is what an
  operator holding `CREATEROLE` on the production database runs, once,
  outside this repository. `db.VerifyRuntimeRoleInProduction` (point 6
  above) exists specifically so that if this step is skipped or done
  incorrectly, the application refuses to start in production rather
  than serving traffic with RLS silently inert — but it cannot perform
  the provisioning itself, by design (see §7's unchanged point that no
  code in this repository should ever be the thing that grants itself
  elevated privilege).
- Re-running §5's verification (or `internal/db/
  runtime_role_separation_test.go` directly, pointed at the real
  production database via `TEST_RUNTIME_DATABASE_URL`) against
  production itself, once that switch is made. This is the exact trigger
  the corresponding `docs/governance/stage-4i-exit-register.md` entry
  names for actually closing the item.

**Net effect on classification:** `PLAT-ROLESPLIT-1` is no longer a gap
in the *committed code* — the mechanism, the CI enforcement, the
regression test, and the fail-closed safety net all exist in-repo today
and were verified locally (build, vet, the full `-tags=integration`
suite, and the new regression test's adversarial probes all pass against
a freshly migrated scratch database with `igaming_runtime` provisioned).
It remains classified a production blocker only in the narrow sense that
no engineering session without a production credential can complete the
one remaining step (creating the role for real, in production, with a
real secret) — see `docs/governance/stage-4i-exit-register.md` §1 for the
updated disposition.
