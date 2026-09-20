# ADR 0046 — Tenant/Licence/Jurisdiction Registry Row-Level Security (Stage 4I Phase E-SECURITY)

**Status: DECIDED AND IMPLEMENTED.** Binding architect ruling, closing
task-registry items `MKT-SCOPE-1` and `MKT-SCOPE-1(b)`. Supersedes ADR
0045 §18 finding F3's "NOT AUTHORIZED THIS DISPATCH" disposition, which is
now **DISCHARGED**.

## Context

`tenants`, `licences`, and `jurisdictions` had never had `ENABLE ROW LEVEL
SECURITY` applied, in any migration, since the platform's earliest schema
(migrations 0001/0002). Every layer built on top of them —
`internal/jurisdiction`'s resolver, migration 0076's
`licence_country_ceilings`/`operating_country_policies` ceiling
enforcement, `AssignTenantLicence`'s write path — assumed this was a
"platform-wide reference data, no RLS needed" posture, matching
`jurisdictions`' own deliberate design (canonical-model §6.1). That
assumption was correct for reads (nothing in this platform's history ever
needed to hide a `jurisdictions` row), but it was never independently
re-examined for **writes** on `tenants`/`licences` specifically, and it
was wrong.

The architect live-reproduced a full attack chain: an ordinary
tenant-scoped connection (`db.Pool.WithTenant`) could execute

```sql
UPDATE tenants SET licensing_model = 'own_licence', licence_id = '<another tenant's BYOL licence>'
 WHERE id = '<this tenant's own id>';
```

in a single statement. This defeats migration 0007's composite FK
(`tenants_licence_matches_model`) because `expected_licensee` is a
`GENERATED ALWAYS ... STORED` column recomputed from the **new**
`licensing_model` in the same statement — the FK only ever validates a
self-consistent (new-model, new-licence) pair, never "did this tenant's
ORIGINAL model actually license this licence". After that single UPDATE,
`internal/operatingmarket`'s ceiling-enforcement trigger
(`operating_country_policies_enforce_ceiling()`, step 1: `SELECT
t.licence_id FROM tenants t WHERE t.id = NEW.tenant_id`) and its own
resolver (`resolve.go`) both read the **forged** licence and happily
computed a `permitted` answer for a country the tenant's real licence
never granted.

Also reproduced, independently of the ceiling attack:

- A tenant-scoped connection could `INSERT INTO licences (...)` and mint
  its own licence outright.
- A tenant-scoped connection could `UPDATE licences SET status =
  'suspended' WHERE id = '<a DIFFERENT tenant's licence>'`, or extend an
  expiry date, or reactivate a suspended one.
- A tenant-scoped (or even scopeless) connection could
  `DELETE FROM tenants WHERE id = '<a different tenant's id>'` outright —
  no elevated role, no RLS bypass required — and PostgreSQL's own
  `ON DELETE CASCADE` (which runs with RLS bypassed regardless of the
  deleting connection's scope) would remove that OTHER tenant's entire
  `operating_country_policies` set. This is a materially worse, lower-bar
  version of the residual ADR 0045 §18 finding F2 disclosed (F2 assumed
  the exposure required a role that bypasses RLS entirely; it did not).
- A tenant-scoped connection could `INSERT INTO jurisdictions (...)` and
  create arbitrary jurisdiction rows.

**`internal/operatingmarket`'s own code and schema are NOT defective and
required NO executable change.** Migration 0076's `licence_country_
ceilings_read` policy and `operating_country_policies_enforce_ceiling()`
are both internally correct given their inputs; the defect was entirely
that `tenants`/`licences` gave an attacker a writable path to forge those
inputs. This ADR fixes the premise, not the policy.

## Decision

### 1. Row-level security, asymmetric by table, uniform by write

**`tenants` — read-open, write platform-admin-only, DELETE platform-admin-only:**

```sql
CREATE POLICY tenants_read ON tenants FOR SELECT USING (true);
```

Deliberately `USING (true)`, for the same reason migration 0044 gave
`assets` an open read policy: multiple legitimate call sites have no
tenant context to offer.

- `identity.GetTenantBySlug` — staff-login tenant resolution, called
  *before* any tenant context exists by construction.
- Three `WithoutTenant` active-tenant sweeps: `internal/rg/
  enumeration_sweep.go`, `internal/reconciliation/scheduler.go`,
  `internal/bonus/schedulers.go` (`SELECT id FROM tenants WHERE status =
  'active'`).
- Migration 0076's own `licence_country_ceilings_read` EXISTS join and
  `operating_country_policies_enforce_ceiling()`'s own lookup — both run
  from a TENANT-scoped connection reading its OWN row, which a
  tenant-match policy would satisfy anyway, but a narrower policy would
  add a second, driftable copy of "which tenant may see which row" logic
  with no security benefit (a tenant can already always read its own
  row).

No read-side narrowing was pursued: nothing in this platform's history
has ever needed to hide one tenant's `id`/`slug`/`status` from another's
connection, and read-narrowing here would not have closed the actual
defect (the WRITE path).

Write policies (`tenants_platform_admin_insert`, `_update`, `_delete`)
all require `app.platform_admin_principal_id` set AND both
`app.tenant_id` and `app.player_account_id` unset — migration
0044/0045/0075/0076's identical predicate, applied a fifth time. **There
is no tenant-scoped write policy of any kind, and no `FOR ALL` policy.** A
tenant must never be able to write its own row — that is the entire
defect this migration exists to close.

`tenants` is the ONE table of the three that gets a DELETE policy
(`tenants_platform_admin_delete`), restricted to platform-admin scope.
Tenant deletion is the one legitimate DELETE on this table (integration
test teardown, and the `ON DELETE CASCADE` migration 0076 §7.3
deliberately preserves for `operating_country_policies`); blocking it
outright would break both. Restricting it to platform-admin scope is a
**net tightening** versus the pre-migration state, not a new capability.

**`licences` — narrow read, write platform-admin-only, no DELETE:**

```sql
CREATE POLICY licences_read ON licences FOR SELECT USING (
    NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    AND (
        (platform_admin_principal_id IS NOT NULL AND app.tenant_id IS NULL)
        OR EXISTS (SELECT 1 FROM tenants t WHERE t.id = app.tenant_id AND t.licence_id = licences.id)
    )
);
```

Narrower than `tenants`/`jurisdictions` — platform-admin, or the tenant
whose own `tenants.licence_id` names the row. This is ADR 0045 §4's
already-ruled "a BYOL tenant's own licence ceiling must not be readable
by an unrelated tenant", applied one level down from
`licence_country_ceilings` to `licences` itself. The EXISTS clause is
migration 0076's own `licence_country_ceilings_read` shape, reused
verbatim; it cannot recurse (`tenants_read` is `USING (true)` and
references no other table).

No DELETE policy: no production or test code deletes a licence (`rg
"DELETE FROM licences"` returns zero hits, verified), this table is the
root of `licence_country_ceilings`' FK (no cascade), and a licence is a
record of a real-world legal instrument — deleting one is not a
capability this platform should have at all, let alone grant casually.
Same posture as `licence_country_ceilings` itself (migration 0076).

**`jurisdictions` — read-open, write platform-admin-only, no DELETE:**

```sql
CREATE POLICY jurisdictions_read ON jurisdictions FOR SELECT USING (true);
```

Read-open for the HDR-J-5 (player-jurisdiction) reason:
`resolver.go`'s licence→jurisdiction JOIN is on the player-jurisdiction
resolution path and must not change behavior — canonical-model §6.1's
existing, deliberate posture, unchanged by this ADR. Writes require
platform-admin scope (closing the gap `SetJurisdictionCountryCode`'s own
Phase E fix-round comment already disclosed: "this Go-level check is the
ONLY control" — it now has the database-level backstop that comment said
it lacked). No DELETE policy: no jurisdiction is ever deleted by any
sanctioned path.

### 2. BYOL licence exclusivity (not an RLS matter)

Independently discovered and fixed in the same migration: two distinct
tenants, both `licensing_model = 'own_licence'`, could bind the SAME
`licensee = 'tenant'` licence — `tenants_licence_matches_model` (migration
0007) checks licensee **kind** only, never exclusivity. Because
`licence_country_ceilings` is keyed on `licence_id` alone (no tenant
column), the second tenant would silently inherit the first's entire
country ceiling.

Fixed with `uq_tenants_exclusive_own_licence`, a partial unique index:

```sql
CREATE UNIQUE INDEX uq_tenants_exclusive_own_licence
    ON tenants (licence_id)
    WHERE licence_id IS NOT NULL AND expected_licensee = 'tenant';
```

Keyed on the `GENERATED` `expected_licensee` column (never drifts from
`licensing_model` independently), and deliberately scoped ONLY to
`licensee = 'tenant'` rows — a `licensee = 'platform'` licence remains
legitimately shared across every `under_platform_licence` tenant, per ADR
0006's hybrid-licensing model. This says only what ADR 0006 already says
("a licence a TENANT brought is that tenant's") and invents no BYOL
onboarding process.

### 3. Go-level changes

- **`internal/identity`**: new sentinel `ErrPlatformTransactionScope` and
  `assertPlatformScope` (mirrors `internal/jurisdiction`'s function of the
  same name byte-for-byte). `CreateTenant`'s first statement is now
  `assertPlatformScope`; its contract is otherwise unchanged (same
  signature).
- **`internal/httpserver.newCreateTenantHandler`**: pool scope changed
  from `WithoutTenant` to `WithPlatformAdmin`; the previously-swallowed
  `uuid.Parse(tc.Subject)` error is now surfaced explicitly (a parse
  failure would otherwise silently become `uuid.Nil` and be rejected by
  `WithPlatformAdmin`'s own nil-principal guard with an opaque error);
  `reason_code` is now a required request field, and the `tenant.created`
  audit record now carries `reason_code`/`before: null`/`after:
  {id, name, slug, licensing_model, status}`, mirroring
  `jurisdictionState`/`licenceState`'s existing shape.
- **`internal/jurisdiction.AssignTenantLicence`**: contract changed from
  `db.Pool.WithTenant` to `db.Pool.WithPlatformAdmin`; first statement is
  now `assertPlatformScope`; its audit write moved from tenant-scoped
  (`recordRegistryAudit(ctx, tx, p.TenantID, ...)`) to platform-scoped
  (`recordRegistryAudit(ctx, tx, uuid.Nil, ...)`), since a
  `WithPlatformAdmin` transaction never sets `app.tenant_id` and
  `audit_log`'s dual-scope RLS `WITH CHECK` requires an exact match for a
  non-NULL `tenant_id`. Signature unchanged.
- **`internal/jurisdiction.CreateJurisdiction`, `CreateLicence`,
  `ListJurisdictions`, `ListLicences`**: each gained `assertPlatformScope`
  as its first statement, mirroring `SetJurisdictionCountryCode`'s
  existing Phase E fix-round pattern exactly.
- **`internal/operatingmarket/resolve.go`**: comment-only correction.
  `assertTenantScope` is NOT removed or weakened — it remains load-bearing
  because `tenants_read` is deliberately `USING (true)`, so a tenant-scoped
  connection can still READ another tenant's `licence_id` (though it can
  no longer WRITE it). The prior comment's claim that RLS provided "ZERO
  tenant isolation" on this path is corrected to explain the actual
  post-migration division of labor: write-side RLS now prevents forgery;
  `assertTenantScope`'s own read-side check remains the only thing
  preventing a transaction scoped to tenant A from resolving an answer for
  a caller-supplied tenant B.

### 4. Task dispositions

- **BYOL licence exclusivity**: fixed now (§2 above).
- **Licence validity (`EvaluateLicenceValidity`)**: unchanged — already
  correctly fail-closed; RLS-invisibility of a foreign licence now maps to
  the same `LicenceNotFound` outcome a genuinely-absent licence produces
  (proven by `TestEvaluateLicenceValidity_ForeignLicenceIsInvisibleAndFailsClosedNotOpen`),
  which is the CORRECT fail-closed behavior, not a regression to fix.
- **`licence_country_ceilings`/`operating_country_policies` schema,
  triggers, RLS**: zero changes. Confirmed via `git diff` showing no
  changes to migration 0076 or any pre-existing
  `internal/operatingmarket/*.go` executable logic.
- **`MKT-DUAL-1`** (dual control): deferred, unchanged. This ADR confirms
  the foundation is *capable* of supporting dual control later; it builds
  no approvals table, FK, or four-eyes mechanism now.
- **`PLAT-ROLESPLIT-1`** (new, deferred): the migration-owner/runtime-role
  split — genuinely blocked on infrastructure this repository cannot
  provide (creating a second Postgres role requires `CREATEROLE`, which
  the application role lacks, verified live). Owner `security`.
  Pre-production gate, not a gate on Phase E resolver wiring.
- **`MKT-LICSTATUS-1`** (new, deferred): no sanctioned write path exists
  for `licences.status` (suspend/reinstate/expire). Owner `architect`.
  Gate: before the first real `licence_country_ceilings` row.
- **`MKT-AUDIT-1`** (new, deferred): `AssignTenantLicence`'s audit row is
  now platform-scoped only, so the affected tenant cannot read back its
  own licence-assignment history via its own `PermAuditRead`. Not yet
  needed — no tenant-facing surface exists today. Owner `security`.

Full disposition ledger, including the two previously-unrecorded attacks
(the composite-FK-defeating combined UPDATE, and the cascading-DELETE
escalation of ADR 0045 §18 finding F2): `docs/governance/task-registry.md`
under `MKT-SCOPE-1`/`MKT-SCOPE-1(b)` (RESOLVED).

## Consequences

- `internal/identity.CreateTenant` and `internal/jurisdiction.
  AssignTenantLicence` are now platform-admin-scoped-transaction-only —
  any future caller that assumed tenant-scoped or scopeless access must be
  updated (this dispatch mechanically updated every existing test fixture
  across ~25 files that seeded `tenants`/`licences`/`jurisdictions` under
  a scopeless connection).
- `ListLicences`'s "every licence row, platform-wide" contract now
  actually depends on being called from platform-admin scope — a
  tenant-scoped call would silently see at most one row (its own bound
  licence) under `licences_read`'s narrow policy, rather than the full
  set; `assertPlatformScope` prevents this silent narrowing by erroring
  instead.
- A denied write under this migration's RLS is a **silent zero-row
  no-op**, not an error, for UPDATE/DELETE (INSERT still raises a `WITH
  CHECK` violation). Every fixture and application call site touched by
  this migration checks rows-affected explicitly rather than trusting a
  nil error alone — the architect's own reproduction of this exact
  failure mode (`TestResolveOperatingCountryPolicy_
  NotYetIssuedLicenceYieldsNotPermittedByLicence`'s `UPDATE licences SET
  issued_at = ...` fixture silently affecting zero rows) is the reason
  this discipline is now load-bearing project-wide, not merely a style
  preference.
- `docs/architecture/15-jurisdiction-and-licensing-model.md` and
  `docs/governance/stage-4i-canonical-model.md` §6.1 are corrected
  wherever they stated `tenants`/`licences`/`jurisdictions` carry no RLS.
  **`docs/security/security-architecture.md` was NOT corrected by the
  original implementation** despite this ADR having previously claimed
  (inaccurately) that `docs/architecture/03-database-architecture.md` was
  one of the files corrected — verified: `03-database-architecture.md`
  never made any claim about `tenants`/`licences`/`jurisdictions` RLS in
  the first place (`rg` finds zero mentions of any of the three table
  names in that file), so it never needed correction and citing it here
  was simply wrong. `docs/security/security-architecture.md` §J4I.6/§J4I.7,
  by contrast, contained the actual stale claims ("no `tenant_id` and no
  RLS", "Verified directly: a tenant-scoped transaction can INSERT into
  jurisdictions today", "the permission check is the entire control on the
  write side; there is no database backstop behind it") and has now been
  corrected as part of this fix round (Fix 3) to describe migration 0077's
  actual RLS posture.

## Validation

Migration 0077 verified directly against `pg_class`/`pg_policies`/
`pg_indexes` on a freshly-migrated scratch database (all 77 migrations,
`cmd/migrate up`, no reused local database). Full validation gate (`go
build`, `go vet`, `go vet -tags=integration`, `gofmt -l .`, focused
packages, 10 consecutive `-race` runs of the concurrency-relevant tests,
whole-repo `go test -tags=integration ./... -count=1`) recorded in the
Stage 4I Phase E-SECURITY entry of `docs/active-stage.md`/
`docs/progress.md`.
