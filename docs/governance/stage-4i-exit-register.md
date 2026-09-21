# Stage 4I Exit Register

Produced by the "STAGE 4I — EXIT TRIAGE AND PRODUCTION INTEGRATION
READINESS" directive. Purpose: close out Stage 4I's open item list with a
concrete disposition per item — not to eliminate items by building more
architecture, but to say plainly what blocks production, what blocks the
next feature, what needs a human decision now versus later, and what is
safe to leave alone.

**This document does not change any semantics.** No player-jurisdiction,
licensing, or operating-market behavior was modified to produce it. Every
classification below is either read directly from the existing governance
record (`docs/governance/task-registry.md`) or from a fresh, narrowly
targeted verification performed this pass (noted per item). Stage 4I's
architecture (player jurisdiction, licensing jurisdiction, operating-market
policy) is FROZEN per this directive and was not reopened.

## Classification key

- **A. PRODUCTION BLOCKER** — must be fixed before any production
  deployment, regardless of which feature ships next.
- **B. NEXT-FEATURE BLOCKER** — safe today; becomes blocking only when a
  specific named feature/stage is built.
- **C. HUMAN DECISION REQUIRED** — an engineering-only fix is impossible or
  inappropriate; a person must decide, but not necessarily right now.
- **D. DEFERRED — SAFE** — genuinely safe to leave exactly as-is; no
  known trigger makes it urgent.
- **E. OBSERVATION / TECHNICAL DEBT** — noted for completeness, not
  tracked as a gating item.

---

## 1. `PLAT-ROLESPLIT-1` — migration-owner/runtime-role split

**Classification: A. PRODUCTION BLOCKER — NARROWED TO EXTERNAL
INFRASTRUCTURE ACTION ONLY. Everything this repository's own code, CI,
and test suite can do has been IMPLEMENTED and verified; see the update
below and `docs/security/runtime-role-separation.md` §9 for the full
record.**

**Update (Stage 9 production-readiness pass):** `deploy/init-app-role.sql`
now provisions both `igaming` (unchanged) and `igaming_runtime` (the
non-owning role this item calls for); `.github/workflows/ci.yml`
provisions `igaming_runtime` and runs the adversarial-probe regression
suite (`internal/db/runtime_role_separation_test.go`, previously only a
one-off manual verification) against it on every push/PR; and
`cmd/platform-api/main.go` now calls a new fail-closed startup check
(`db.VerifyRuntimeRoleInProduction`) that refuses to start in production
if the connecting role owns tables. `TEST_DATABASE_URL` was deliberately
left pointed at `igaming` (see that document's §9 point 4 for exactly
why — a large share of the integration suite needs owner/DDL privileges
to run migration mechanics at all). **What remains, and is genuinely
outside any engineering session's reach without a production
credential:** an operator with `CREATEROLE` on the real production
database must still run §6's script there with a freshly generated
secret, and repoint production's own `DATABASE_URL`. This is why the
classification below is unchanged at "blocks production" — it is not a
code gap, and the trigger for closing it (below) is unchanged.

- **Owner:** `security` (design), operations/infra (execution — outside
  this repository's reach).
- **Why it exists:** the application's single Postgres role (`igaming`)
  both owns every table (ran every migration) and is the credential the
  running application uses for ordinary traffic. Table ownership grants
  `ALTER TABLE`, `DROP`, `TRUNCATE`, `DISABLE ROW LEVEL SECURITY`, and
  `DISABLE TRIGGER` unconditionally, independent of any RLS policy — RLS
  never applies to a table's owner (only `FORCE ROW LEVEL SECURITY` makes
  it apply to the owner, and even then the owner can simply turn `FORCE`
  or RLS itself off, since altering that setting is also an owner-only
  operation).
- **Concrete consequence (empirically verified this pass, see
  `docs/security/runtime-role-separation.md` §5 for the exact
  reproduction):** any code path, credential leak, or compromised
  component that obtains the `igaming` connection string can run
  `ALTER TABLE tenants DISABLE ROW LEVEL SECURITY`, disable every user
  trigger platform-wide, and then `TRUNCATE ... CASCADE` — wiping tenants,
  licences, the entire ledger, and the audit log to zero rows. No RLS
  policy, CHECK constraint, or application code can prevent this, because
  none of those controls apply to the object's owner.
- **Blocks production:** **YES.**
- **Blocks the next implementation stage:** **NO, conditionally**
  (flagged by the independent architect review, same shape as
  `MKT-AUDIT-1`'s conditional flag below) — the recommended next stage
  (§13) does not require this closed first ONLY while its back-office
  surface remains platform-operator-staff-only. The registry's own
  second trigger for this item (below) is "before the first
  `RoleTenantAdmin`/`RoleCompliance` credential grant to anyone outside
  platform-operator staff" — if the back-office stage's scope grows to
  issue credentials to non-platform-operator users (e.g. a genuine B2B
  tenant-admin login), this item becomes a hard blocker for THAT specific
  expansion, not merely a parallel-track infrastructure item anymore.
- **What feature/stage actually needs it (restated in full — an earlier
  draft of this section understated the second trigger; corrected per
  the independent architect review):** none as a feature dependency; two
  independent exposure triggers instead: (i) the first production
  deployment against real tenant data, or (ii) the first
  `RoleTenantAdmin`/`RoleCompliance` credential grant to anyone outside
  platform-operator staff — whichever comes first.
- **Recommended action:** infrastructure/ops must provision a second,
  non-owning Postgres role for the running application (`igaming_runtime`
  or equivalent) with only `SELECT`/`INSERT`/`UPDATE`/`DELETE` on
  application tables, no `CREATE`/`TRUNCATE`/schema privileges, and no
  ownership of anything — then repoint the application's runtime
  `DATABASE_URL` at it, while keeping the current `igaming` role
  (unmodified) as the migration-owner credential used only by `cmd/
  migrate` at deploy time. Full exact commands and verification: `docs/
  security/runtime-role-separation.md`. **This document does not execute
  that action** — it requires a credential this session does not have and
  must not request (CLAUDE.md "Environment safety": no production
  credentials without explicit later authorization). **As of the Stage 9
  pass, everything up to that exact action is done in-repo** (role
  definition, dev/CI provisioning, the fail-closed startup guard, and the
  permanent regression test) — see `docs/security/runtime-role-
  separation.md` §9.
- **Exact trigger for reopening (i.e., for treating this as done, not for
  reopening the investigation — the investigation is already complete):**
  closed the moment infra confirms the runtime role has been switched and
  the old `igaming` credential is no longer reachable from the running
  application's environment. Re-verify with the exact SQL probes in
  `runtime-role-separation.md` §5 against the production database once
  that switch is made.

---

## 2. `PLAT-TENANTREAD-1` — `tenants_read` permits cross-tenant enumeration

**Classification: D. DEFERRED — SAFE** (re-verified this pass against real
execution contexts, not just the RLS predicate in isolation).

- **Owner:** `architect`/`security`.
- **Why it exists:** migration 0077's `tenants_read` RLS policy is
  `USING (true)` for every non-player scope (narrowed in the Phase
  E-SECURITY fix round only to exclude player scope). At the database
  level, a connection with the `tenant_id` GUC set to Tenant A and no
  further restriction could `SELECT * FROM tenants` and see every OTHER
  tenant's `name`/`slug`/`licensing_model`/`status`/`licence_id`.
- **Verification performed this pass:** grepped every HTTP handler in
  `internal/httpserver` and every caller of `identity.GetTenantByID`/
  `GetTenantBySlug` in the whole repository. **There is no `ListTenants`
  function anywhere in `internal/identity`, and no HTTP route exists that
  lists or enumerates `tenants` rows.** Every existing call site resolves
  exactly one tenant by an already-known ID or slug (staff login, casino
  orchestration, deposit/casino/KYC handlers reading the caller's own
  tenant by its own slug). B2B/tenant-scoped credentials in this platform
  are HTTP API tokens, not direct database credentials — there is no
  mechanism today by which an external tenant's own request could reach a
  raw SQL connection with its `tenant_id` GUC set and issue an arbitrary
  query. **Concrete consequence: reachable today only via direct database
  access (i.e., already inside the trust boundary this document's
  `PLAT-ROLESPLIT-1` item addresses), not via any implemented product
  surface, staff or tenant.**
- **What feature/stage actually needs it fixed:** the first HTTP/API
  endpoint that queries `tenants` under a non-platform-admin scope for
  anything beyond a single already-known row (i.e., any future listing/
  enumeration endpoint), OR the first time a B2B tenant is issued anything
  resembling direct database access (not this platform's model today).
- **Legitimate scopeless readers named explicitly, per the independent
  security review (an earlier draft cited "migration 0077's own
  rationale" without naming them):** `internal/rg/enumeration_sweep.go`,
  `internal/reconciliation/scheduler.go`, and `internal/bonus/
  schedulers.go` each run `SELECT id FROM tenants WHERE status =
  'active'` — a genuine multi-row read — but all three execute under
  `pool.WithoutTenant` (platform-scoped background sweeps), not under a
  tenant-scoped connection. These are the actual reason `tenants_read`
  cannot simply require tenant scope; this item's classification and
  reopening trigger are unaffected (a platform-scoped sweep is not the
  cross-tenant exposure this item is about).
- **A live database-drift instance found and fixed during this pass'
  independent security review, distinct from the classification above:**
  the security reviewer found the LOCAL DEVELOPMENT database's actual
  deployed `tenants_read` policy text was `USING (true)` with the
  player-scope exclusion clause entirely absent — i.e., this specific
  local Postgres instance was running a STALE, pre-Fix-5 version of
  migration 0077's policy, despite `schema_migrations` showing version 77
  applied and despite the current committed migration FILE containing the
  correct, fixed clause. Root-caused and confirmed: this local database's
  data directory had migration 0077 applied at an earlier point in this
  session's history, before the Phase E-SECURITY fix round's in-place
  amendment to that same (then still uncommitted) migration file landed —
  exactly the `MKT-MIG76-1` hazard this codebase already documents
  (never trust a shared/reused dev database's schema). This is a
  local-environment artifact, not a defect in the committed code: dropping
  and rebuilding the database fresh from the current migration files
  (`DROP DATABASE` + `CREATE DATABASE` + `cmd/migrate up`) reproduced the
  correct policy text immediately, and the previously-failing regression
  test (`TestTenantsRLS_PlayerScopedConnectionReadsZeroTenants`) then
  passed, along with ten previously-failing `internal/operatingmarket`
  tests (AMENDMENT-2/3 enforcement, same root cause) and the full 30-
  package `-tags=integration` suite. **A genuinely first-time deployment
  (staging or production) is not exposed to this specific drift**,
  because it will apply today's already-corrected migration file for the
  first time, never an earlier version of it. What the drift DOES expose,
  independent of this one instance: `cmd/migrate` tracks applied
  migrations by version number only, with no check that a previously-
  applied migration's live schema still matches its current file content
  — so if any environment (dev, staging, or in the future production)
  ever has an in-place-amended migration applied before the amendment
  lands, nothing will detect the mismatch afterward. Recorded as a new,
  distinct, non-blocking observation (see the new `PLAT-MIGDRIFT-1` note
  in `docs/governance/task-registry.md`'s Stage 4I Exit Triage section) —
  not fixed this pass, since building drift-detection tooling is new
  engineering work outside a documentation-only triage's scope.
- **Blocks production:** NO.
- **Blocks the next implementation stage:** NO.
- **Recommended action:** none now. Do not narrow further — full tenant-
  to-tenant enumeration is a real design tradeoff (the three legitimate
  scopeless readers named above rely on the open predicate) that
  deserves its own review once a real B2B tenant-facing
  listing surface is actually proposed, not a mechanical tightening today.
- **Exact trigger for reopening:** the day a PR adds any HTTP handler that
  runs a multi-row query against `tenants` under a `tenant`/`brand`-scoped
  (non-platform-admin) transaction.

---

## 3. `MKT-LICSTATUS-1` — no sanctioned write path for `licences.status`

**Classification: B. NEXT-FEATURE BLOCKER.**

- **Owner:** `architect` (the write surface's shape — caller, reason
  codes, evidence, dual control — is a cross-cutting design question, not
  a routine CRUD addition).
- **Why it exists:** `CreateLicence` always creates `active` rows; no code
  path can suspend, reinstate, or expire one. Every non-active licence in
  the codebase today is a raw-SQL test fixture.
- **Concrete consequence:** a real regulatory suspension or non-renewal
  cannot be recorded through the application at all today.
- **What feature/stage actually needs it:** the first real (non-test)
  `licence_country_ceilings` row, i.e. the same gate as `MKT-DUAL-1`/
  `MKT-PM-1` — a ceiling is only as trustworthy as the licence status
  underneath it. Also linked to `MKT-DORMANT-1` above: once this item
  ships a write path for `expires_at`/`status`, a licence-validity-
  boundary-driven dormant-policy resumption (with no audit row) becomes
  live, not merely latent — whoever specifies this write surface should
  decide then whether such a resumption needs its own audit event.
- **Second trigger, corrected per the independent architect review:** the
  back-office stage recommended below (§13) does NOT need this fixed to
  start, but `GET`/`POST /v1/admin/licences` and `PUT /v1/admin/tenants/
  {tenantID}/licence` already exist today — **if that stage's screens
  include licence assignment/administration** (as opposed to only
  tenant/brand/player/KYC/bonus/RG/audit screens), this item's trigger
  fires immediately, not later. Whoever scopes the back-office stage's
  screens must explicitly exclude licence administration, or this item
  stops being deferrable.
- **Blocks production:** NO (no real licence content exists yet to need
  suspending).
- **Blocks the next implementation stage:** NO, conditionally — see the
  second trigger above.
- **Recommended action:** defer. Do not build a licence-management UI or
  status-transition API speculatively.
- **Exact trigger for reopening:** the phase that first writes real (non-
  test) `licence_country_ceilings` content, or any phase that gives an
  operator a UI over licence records.

---

## 4. `MKT-AUDIT-1` — tenant-facing licence-assignment audit visibility

**Classification: B. NEXT-FEATURE BLOCKER.**

- **Owner:** `security` (any tenant-visible evidence surface over
  `licences`/`tenants` history is a disclosure-boundary decision).
- **Why it exists:** migration 0077 moved `AssignTenantLicence`'s audit
  row from tenant-scoped to platform-scoped (`tenant_id IS NULL`), as a
  direct, correct consequence of the RLS hardening. A tenant that could
  previously read back its own licence-assignment history via
  `PermAuditRead` no longer can.
- **Concrete consequence:** no tenant-facing surface exposes licence-
  assignment history today, and none has been requested, so nothing is
  currently broken by this — but it is a real regression waiting for a
  consumer.
- **What feature/stage actually needs it:** any partner-console/back-
  office surface that would show a tenant its own licence-assignment
  history, OR the first real B2B (non-first-party) tenant onboarding,
  whichever comes first. **The back-office stage recommended below (§13)
  is staff-facing (viewing ALL tenants as platform-admin), not
  tenant-facing self-service** — it does not trigger this gate as
  currently scoped, but if that stage's design later grows a
  tenant-self-service view of its own licence history, this item must be
  resolved before that specific screen ships.
- **Blocks production:** NO.
- **Blocks the next implementation stage:** NO, conditionally (see above).
- **Recommended action:** defer; flag explicitly to whoever scopes the
  back-office stage's screens so the gate isn't missed if tenant
  self-service is added later.
- **Exact trigger for reopening:** first tenant-facing (not staff-facing)
  UI/API surface exposing licence-assignment history; first non-first-
  party B2B tenant onboarding.

---

## 5. `MKT-DORMANT-1` — dormant tenant-rung policy resumes on ceiling re-expansion

**Classification: D. DEFERRED — SAFE** (mechanism re-examined this pass,
not redesigned).

- **Owner:** `architect`.
- **Why it exists:** if a licence's country ceiling is contracted and
  later re-expanded, a tenant-rung `operating_country_policies` row left
  `enabled` from before the contraction resumes being effective, with no
  NEW write at the tenant rung at the moment it resumes.
- **Verification performed this pass:** read `ceiling_admin.go`'s
  `CreateLicenceCountryCeilingVersion` (confirmed independently by the
  architect reviewer at line-level: `wideningCapable` is computed and the
  platform-scoped `recordOperatingMarketAudit` call is on every return
  path, uuid.Nil scope, same transaction) — every ceiling version write
  (contraction AND re-expansion alike) writes a platform-scoped
  `audit_log` entry in the same transaction, including the
  `widening_capable` metadata field this codebase's own auditors are
  bound to filter on. **For a ceiling-version write specifically, the
  only path to "resumed" tenant-rung effectiveness is a platform-admin's
  own explicit, fully audited act at the licence-ceiling rung.**
  **Correction from the independent architect review:** this is narrower
  than "the only path" in general — `resolve.go` also gates the ceiling
  on `jurisdiction.EvaluateLicenceValidity`, so a dormant tenant-rung row
  can equally resume when TIME PASSES across a licence validity boundary
  (e.g. a licence reaching its `issued_at` date, or — once
  `MKT-LICSTATUS-1` ships a status/expiry write path — a renewal
  extending `expires_at`), with **no ceiling write and therefore no
  audit row of any kind**. This second path is latent today only because
  no code path currently writes `licences.expires_at`/`status` at all
  (`MKT-LICSTATUS-1`, item 3 above) — it becomes live the moment that
  item ships, which is why `MKT-LICSTATUS-1`'s own gate ("before the
  first real ceiling row") and this item are linked: whoever specifies
  the licence status/renewal write surface must also decide whether a
  validity-boundary-driven resumption needs its own audit event. No
  lower-privileged actor, and no code path bypassing an actual
  platform-admin act (ceiling write, or eventually a licence status/date
  write), can produce the resumption — that is what makes it a fail-
  closed distinction, not a genuine
  design question (should re-expanding a ceiling require operators to
  also re-affirm every tenant-rung row it un-shadows?), not a security
  defect.
- **Concrete consequence:** none today — zero real (non-test) ceiling or
  tenant-rung rows exist; this is a latent semantics question, not a live
  exposure.
- **What feature/stage actually needs it:** the phase that first writes
  real ceiling content and needs to define operational procedure around
  contraction/re-expansion.
- **Blocks production:** NO.
- **Blocks the next implementation stage:** NO.
- **Recommended action:** no code change. Record for the eventual
  ceiling-operations runbook: re-expanding a ceiling should include a
  manual review of which tenant-rung rows it will un-shadow, until/unless
  `architect` decides the resolver itself should require a fresh
  affirmative act.
- **Exact trigger for reopening:** the phase that authors the operational
  runbook for real ceiling contraction/expansion, or any phase that
  proposes to change `resolve()`'s algorithm itself.

---

## 6. `MKT-DUAL-1` — four-eyes/dual-control seam on operating-market enablement

**Classification: C. HUMAN DECISION REQUIRED**, but **not now**.

- **Owner:** `architect` (design), `security` consulted.
- **Why it exists:** Phase E built no dual-control mechanism for
  widening-capable writes in `internal/operatingmarket`, because neither
  existing approval mechanism (`asset_change_requests`,
  `bonus_change_requests`) fits, and building a third, generic
  approval mechanism is a cross-cutting decision, not a routine addition.
- **Verification performed this pass:** confirmed (again) zero HTTP
  routes, zero partner-console surface, and zero service-identity caller
  reach either Phase E write path. `grep` across `internal/httpserver`
  confirms no route mounts any `operatingmarket` write function.
  **Country/market widening is not reachable from any current production
  HTTP path — full stop.**
- **Concrete consequence:** none today. The absence of dual control is a
  known, bounded, zero-exposure gap exactly as recorded at Phase E close.
- **What feature/stage actually needs it:** per `docs/governance/
  task-registry.md`'s own original wording (line ~1281), this item is a
  hard prerequisite of ALL THREE of: (a) any HTTP route, partner-console
  surface, or service-identity caller reaching either Phase E write path;
  (b) any wiring of `ResolveOperatingCountryPolicy`/
  `IsRegistrationPermitted` into ANY consuming domain (not just the write
  side — read-side resolver wiring counts too); (c) the first real
  (non-test) row in `licence_country_ceilings` or
  `operating_country_policies`. **Correction from the independent
  architect review: an earlier draft of this section stated only
  trigger (a); (b) and (c) are restored here** to match the registry's
  original, still-binding scope — the omission was a drafting error in
  this document, not a change to the registry's own record.
- **Blocks production:** NO (nothing reachable to widen).
- **Blocks the next implementation stage:** NO — the back-office stage
  recommended below wires none of (a)/(b)/(c).
- **Recommended action:** keep the mechanism fail-closed exactly as-is;
  do not build a dual-control platform speculatively; do not wire any
  route, resolver call, or real ceiling/policy content until this item is
  resolved.
- **Exact trigger for reopening:** the first PR that would add an HTTP
  route, partner-console handler, or service-identity caller reaching
  `CreateLicenceCountryCeilingVersion` or any `operating_country_policies`
  write function; OR the first PR wiring `ResolveOperatingCountryPolicy`/
  `IsRegistrationPermitted` into any consuming domain's read path; OR the
  first real (non-test) row written to either table.

---

## 7. `MKT-EXPIRY-1` — licence validity boundary confirmation

**Classification: C. HUMAN DECISION REQUIRED**, but **not now**.

- **Owner:** `architect`/compliance.
- **Why it exists:** `EvaluateLicenceValidity`'s half-open
  `[issued_at, expires_at)` window is a disclosed engineering default, not
  a confirmed legal determination. **This item also carries a distinct
  schema/write-surface sub-question, restored here per the independent
  architect review (an earlier draft of this section dropped it):**
  `licences.issued_at` is `DATE` and NULLABLE, and no Go code writes it
  today — a NULL is currently treated as "no issue date asserted" (falls
  through, not treated as "not yet issued"), which fails closed for every
  existing row but leaves the "not yet issued" check structurally unable
  to ever fire until some writer populates the column. Whether
  `issued_at` should become `NOT NULL` with a populating admin surface is
  open, and is a schema-plus-write-surface change, not merely a
  compliance confirmation.
- **What feature/stage actually needs it:** the phase that first wires
  `ResolveOperatingCountryPolicy` into any consuming domain — that phase
  must confirm both the expiry and issuance boundaries with compliance
  before going live, AND decide the `issued_at` nullability/write-surface
  question above.
- **Blocks production:** NO (nothing wired yet).
- **Blocks the next implementation stage:** NO.
- **Recommended action:** defer exactly as already recorded; no new
  action this pass.
- **Exact trigger for reopening:** first production wiring of
  `ResolveOperatingCountryPolicy`/`IsRegistrationPermitted`.

---

## 8. `MKT-PM-1` — remove `licences.permitted_markets`

**Classification: B. NEXT-FEATURE BLOCKER.**

- **Owner:** whichever phase answers `HDR-J-6`.
- **Why it exists:** the platform must never hold two country lists (the
  legacy `licences.permitted_markets` column and the new
  `licence_country_ceilings` table) with only one populated.
- **What feature/stage actually needs it:** the phase that answers
  `HDR-J-6` and writes the first real ceiling content.
- **Blocks production:** NO. **Blocks the next implementation stage:** NO.
- **Recommended action:** defer exactly as recorded.
- **Exact trigger for reopening:** the phase that writes the first real
  `licence_country_ceilings` row.

---

## 9. `PLAT-MIGDRIFT-1` — `cmd/migrate` has no live-schema-vs-file-content verification

**Classification: E. OBSERVATION / TECHNICAL DEBT — CLOSED (Stage 9.1).**
New this pass, surfaced by the independent security review; upgraded to
FIX BEFORE PRODUCTION by the Stage 9 architect review after a live
instance of exactly this drift was found in the shared dev database; and
closed by Stage 9.1 as recommended below.

**Update (Stage 9.1 production-readiness pass):** a content-hash column
(`schema_migrations.checksum`, migration `0083_migration_checksum_tracking`)
now records a SHA-256 of each migration's up-file content automatically on
every `migrate up`; a new `migrate verify` subcommand
(`internal/db.Pool.VerifyMigrations`, wired into `cmd/migrate` and into
CI right after `migrate up`) recomputes every applied migration's current
on-disk hash and reports any mismatch, plus any version gap or duplicate.
Exact documented scope (see `docs/architecture/38-deployment-
architecture.md`): it detects up-file content drift on already-applied
migrations and version-number gaps/duplicates; it does **not** check
`.down.sql` files or live schema against migration SQL, and it cannot
retroactively detect drift that predates checksum tracking (a legacy
NULL-checksum row is backfilled from current on-disk content, which is an
honest, documented limit, not a false-positive gap). Run for real against
the shared `igaming_platform_dev` database that carried the originally-
found drift: it correctly reported clean (per its documented scope), and
that database was separately rebuilt from a clean migration chain as part
of the same fix.

- **Owner:** `devops`/`architect`.
- **Why it exists:** `schema_migrations` tracks applied migrations by
  version number only, with no content checksum. If a migration is
  amended in place after being applied somewhere (this codebase's own
  documented, permitted practice for genuinely uncommitted migrations —
  see `MKT-MIG76-1`), that environment's live schema silently keeps the
  pre-amendment content forever; `migrate status` reports "clean"
  regardless.
- **Concrete consequence, found live this pass:** this session's own
  local development database had exactly this drift on `tenants_read`
  and on `internal/operatingmarket`'s AMENDMENT-2/3 enforcement — fixed
  by rebuilding the database fresh; see §2's fuller account. **Not a
  defect in the committed code** — a genuinely first-time deployment
  applies today's already-correct file, so this specific instance cannot
  recur there.
- **What feature/stage actually needs it fixed:** nothing currently —
  this is a blind spot, not an active exposure, for any environment that
  has not had an in-place-amended migration applied pre-amendment.
- **Blocks production:** NO.
- **Blocks the next implementation stage:** NO.
- **Recommended action:** none now. If ever addressed, the shape is a
  content-hash column on `schema_migrations` plus a `migrate verify`
  command — not built this pass; new engineering work outside a
  documentation-only triage's scope.
- **Exact trigger for reopening:** any incident where a live
  environment's schema is suspected to not match its migration files, or
  a future phase that hardens migration tooling generally.

---

## Human Decision Register items

Per the directive's instruction: only decisions that genuinely block the
**next** implementation stage are surfaced as needing an answer now. None
of the seven below do, because the recommended next stage (§13) is
staff-facing back-office tooling over already-built, already-approved
core APIs and touches no jurisdiction/licensing/operating-market content
or resolver wiring.

| Item | One-line question | Required for next stage? | Required when |
|---|---|---|---|
| `HDR-J-6` | Which markets is the first B2C brand permitted to serve? | **No** | Before any resolver wiring or real ceiling content |
| `HDR-J-7` | Which `Purpose` does each `OperationClass` require? | **No** | Before any code branches on `OperationClass` to select a `Purpose` |
| `HDR-J-8` | Is a physical-location signal required/advisory/undecided, per jurisdiction and operation? | **No** | Same wiring phase as `HDR-J-7` |
| `HDR-J-9` | Maximum age before a location signal is stale? | **No** | Same wiring phase as `HDR-J-7`/`HDR-J-8` |
| `HDR-M-1` | What production authorization/governance model applies to tenant country enablement (the dual-control policy `MKT-DUAL-1` needs)? | **No** | Before any operating-market write path is wired to a route |
| `HDR-M-2` | What happens to existing players after a country is disabled for their tenant? | **No** | Same wiring phase as `HDR-M-1` |
| `HDR-J-1`/`HDR-J-5` | (Answered/fixed — no player-jurisdiction fallback; platform player-jurisdiction resolution governs player jurisdiction, licensing jurisdiction is separate context.) | n/a | Already resolved; unchanged this pass |

**No country list was chosen, no jurisdiction was selected, and no legal/
compliance policy was invented while producing this register.**

---

## Summary table

| ID | Class | Blocks production | Blocks next stage | Owner |
|---|---|---|---|---|
| `PLAT-ROLESPLIT-1` | A | **YES** (external action only — in-repo code/CI/test closed, see §1) | No (conditional — see §1) | security / infra |
| `PLAT-TENANTREAD-1` | D | No | No | architect / security |
| `MKT-LICSTATUS-1` | B | No | No (conditional — see §3) | architect |
| `MKT-AUDIT-1` | B | No | No (conditional) | security |
| `MKT-DORMANT-1` | D | No | No | architect |
| `MKT-DUAL-1` | C | No | No | architect / security |
| `MKT-EXPIRY-1` | C | No | No | architect / compliance |
| `MKT-PM-1` | B | No | No | (phase answering HDR-J-6) |
| `PLAT-MIGDRIFT-1` | E — **CLOSED (Stage 9.1)**, see §9 | No | No | devops / architect |
| `HDR-J-6/7/8/9`, `HDR-M-1/2` | C | No | No | human |

**Net result: exactly one production blocker (`PLAT-ROLESPLIT-1`), and it
is an infrastructure action, not a code defect this repository can fix
directly — as of the Stage 9 production-readiness pass, this repository's
own share of that fix (role definition, dev/CI provisioning, the
fail-closed startup guard, and the permanent regression test) is
committed and verified; only the real production execution step remains,
by design outside any session's reach here. Nothing blocks the
recommended next stage.** **Qualified per
the independent security review:** this statement is about the
*committed code and migration files*, which is what "production blocker"
must mean for a repository-level triage — a genuinely fresh deployment of
today's HEAD is not exposed to any second issue. It does NOT mean every
currently-running instance of this schema is known to be correct; §2
above records a live drift the security review found and this session
fixed in its own local database, and names the underlying tooling gap
(`cmd/migrate` has no live-schema-vs-file-content verification) as a
distinct, non-blocking observation, not a second production blocker.

---

## Integration contract (informational only — no wiring performed)

Per the directive's "Production Wiring Rule": this section states exactly
which future call sites will eventually need to invoke the Stage 4I
mechanisms, and what happens when a result is unresolved. **None of this
is implemented or wired this pass.** It restates, in one place, facts
already established across ADR 0043/0044/0045/0046 and
`stage-4i-canonical-model.md` — it invents no new call site and no new
behavior.

| Call site (future) | Needs player jurisdiction? | Needs licence validity? | Needs operating-country policy? | Input required | On unresolved result |
|---|---|---|---|---|---|
| Player registration | Yes (`DeterminePlayerJurisdiction`, Purpose=identity and/or market-access per `HDR-J-7`) | No (registration isn't gated on the tenant's own licence being currently valid — it is gated on the ceiling/tenant policy for the player's country) | Yes (`IsRegistrationPermitted`, operation code `registration`) | `TenantID`, `BrandID`, `CountryCode` (from evidence, not client-declared), `AsOf` | **Fail-closed**: deny registration. This is already `operatingmarket`'s documented behavior for every unresolved/undetermined outcome — there is no permissive default anywhere in the eleven-valued `Outcome` enum. |
| Deposit (`internal/httpserver/deposit_handlers.go`) | Possibly (RG/KYC thresholds may vary by player jurisdiction — separate from this decision) | Yes, indirectly via the ceiling (a lapsed/suspended licence must not permit new deposits under it) | Yes, operation code `deposit` | Same as above, plus the depositing player's brand | **Fail-closed**: deny the deposit. |
| Withdrawal (`internal/withdrawal`) | Possibly (RG/KYC) | Yes, indirectly via the ceiling | Yes, operation code `withdrawal` | Same as above | **Fail-closed** for new withdrawal requests. (Existing, already-approved withdrawals in flight are a separate, `HDR-M-2`-adjacent question — not decided here.) |
| Wagering / bet placement (`internal/casino/orchestrator.go` `postBet`, and the equivalent future sportsbook bet-placement path) | Yes (`OperationPlay`, Purpose per `HDR-J-7`) | Yes, indirectly via the ceiling | Yes, operation code `wagering` | Same as above, plus per-bet context already flowing through `RiskRequest` | **Fail-closed**: deny the bet. |
| Bonus issuance / conversion (`internal/bonus`) | Yes (`OperationBonusIssuance`/`OperationBonusConversion`) | Yes, indirectly via the ceiling | Not a seeded `platform_operations` code today — would need a new vocabulary row (`bonus_issuance`/`bonus_conversion`) before this could resolve at all, since `resolve()` fails closed (`configuration_conflict`/`not_configured`) for any operation code with no matching row | Same as above | **Fail-closed**. |
| Catalogue availability (which games/products are listed for a player) | Yes (`OperationCatalogueAvailability`) | Yes, indirectly via the ceiling | Same "no seeded vocabulary row yet" gap as bonus, above | Same as above | **Fail-closed** (an unresolvable catalogue-availability check must hide content, never show it). |

**Every row above is fail-closed by construction already** — this is not
a new design decision, it is what `resolve()`, `IsRegistrationPermitted`,
and `DeterminePlayerJurisdiction` already do for every unresolved case
(no permissive default exists in any of the three mechanisms). The table
exists so the phase that eventually performs this wiring has one place to
start from, not because any of it is being built now. **`HDR-J-6/7/8/9`
and `HDR-M-1/2` (§ above) must all be answered before ANY row in this
table can be wired**, since every row depends on at least one of them.
**Corrected per the independent architect review (an earlier draft of
this footer omitted these): every row is also read-side resolver wiring
of `ResolveOperatingCountryPolicy`/`IsRegistrationPermitted`, which is
itself `MKT-DUAL-1`'s own trigger (b) — so `MKT-DUAL-1` must also be
resolved before any row is wired. `MKT-LICSTATUS-1` and `MKT-EXPIRY-1`
are additional prerequisites for any row that depends on licence validity
(every row in this table, via the ceiling) once real licence content
exists.** The recommended next stage (§13 of the completion report)
touches none of these call sites.
