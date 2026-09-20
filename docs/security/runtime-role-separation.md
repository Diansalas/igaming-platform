# Runtime / Migration-Owner Role Separation (`PLAT-ROLESPLIT-1`)

**Status: PRODUCTION BLOCKER — EXTERNAL INFRASTRUCTURE ACTION.**

This document is implementation-ready for an infrastructure/operations
actor with Postgres superuser (or `CREATEROLE`) access to the target
database. It requires no Go code change and no migration file — the fix
is entirely a role/privilege change, executed once per environment
(dev/staging/production) by a human or a deploy pipeline running with
elevated, non-application credentials. **This session did not, and could
not, execute this against any production database** — it has no
production credential and CLAUDE.md's Environment Safety rule forbids
requesting one. Everything below was verified empirically against this
session's local development Postgres only (see §5), which is not
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
| Modify schema (`CREATE TABLE`, etc.) | **No** (`permission denied for schema public`) — with one narrow, harmless exception: `CREATE TEMP TABLE` succeeds, because PostgreSQL grants `TEMPORARY` on the database to `PUBLIC` by default. Temp objects live in the connection's own `pg_temp` schema and hold no platform data, so this is not an escalation, but it means the table above should be read as "cannot create a persistent table," not "cannot create any table." |
| Ordinary `SELECT`/`INSERT`/`UPDATE`/`DELETE` under RLS | **Yes**, and — unlike the current `igaming` credential — genuinely enforced by RLS, because a non-owner role is never exempt regardless of any `FORCE` setting or its absence |

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

**One exception to "ordinary DML is genuinely enforced by RLS," flagged
by the security review:** `casino_games` (the global game catalogue) has
row-level security disabled entirely (`relrowsecurity = false`) and no
`tenant_id` column — it is deliberately global, not tenant-owned data.
The blanket `GRANT ... ON ALL TABLES IN SCHEMA public` in §6 therefore
gives the runtime role unconstrained `INSERT`/`UPDATE`/`DELETE` on that
one table from any connection, RLS or no RLS. This is pre-existing (not
introduced by this split) and not a tenant-isolation issue, but the claim
above should not be read as "every table" without this named exception.

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

-- 4. Confirm no unwanted attribute or grant is present:
--    (run as a check, expect the runtime role to show no attributes)
\du+ igaming_runtime
```

Then, entirely outside the database (secrets manager / deploy config, not
this repository): change the running application's `DATABASE_URL`
credential from `igaming` to `igaming_runtime`. Keep `igaming`'s
credential reachable only from the deploy pipeline's migration step, not
from any application server's runtime environment or secret mount.

## 7. What this document deliberately does NOT do

- It does not add a migration file. Role/privilege provisioning is an
  environment concern (dev/staging/production have, or will have,
  different credentials and secret stores) — encoding it as a
  version-controlled `up.sql` migration executed by the `igaming` role
  itself would not close the gap (the migration-owner role running the
  script is exactly the role this split exists to stop being the runtime
  identity), and per this stage's directive, "no speculative
  infrastructure automation" was to be built.
- It does not change any Go code. `internal/db.Pool` already connects
  using whatever `DATABASE_URL` it is given; no code assumes the
  connecting role owns anything. Repointing the credential is sufficient.
- It does not touch RLS policies, migrations, or any jurisdiction/
  licensing/operating-market schema. This is orthogonal hardening of the
  connection identity, not the authorization model those policies
  express.

## 8. Residual note

Once this split lands, `PLAT-ROLESPLIT-1` is closed and the corresponding
task-registry entry should be marked resolved with the production
environment's verification output (the same six-probe check as §5, run
against the real production database by whoever executes §6). Until then,
it remains the platform's single highest-severity open item: everything
else in `docs/governance/stage-4i-exit-register.md` is either non-blocking
or requires a human policy decision that has no urgency; this one is a
mechanical, fully specified fix blocked only on someone with the right
credential running four `GRANT` statements.

**Follow-up flagged by the independent architect review, not applied this
pass:** `deploy/init-app-role.sql` (the local dev/CI bootstrap) still
creates only the single owning `igaming` role and makes it the database
and schema owner — dev and CI will keep connecting as the owner even
after production adopts this split, so no local test signal would ever
catch a future migration or handler that accidentally relies on an
owner-only operation. When this split is actually rolled out, the
repository's own dev/CI bootstrap should be updated in the same change to
mirror it (create `igaming_runtime` there too, point the local
`DATABASE_URL` the application actually runs against at it, keep
`igaming` for `cmd/migrate` only), ideally paired with a lightweight
regression test that connects as the runtime role and asserts the same
six denials from §5. Not done here because it is inseparable from
actually switching a running credential — exactly the action §7 explains
this document deliberately does not perform on its own — and doing only
half of it (creating an unused role nothing connects as) would not
improve real coverage.
