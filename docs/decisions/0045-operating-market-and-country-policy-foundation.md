# 0045 — Operating Market & Country Policy Foundation (Stage 4I Phase E)

Status: **Implemented as designed.** This ADR records the binding
architect design ruling for Stage 4I Phase E ("Operating Market & Country
Policy Foundation") and the implementation built against it. It is a
**mechanism-only** phase: zero country/market content, zero HTTP routes,
zero production wiring into any consuming domain. See
`docs/governance/task-registry.md`'s "Stage 4I Phase E" section for the
full findings/disposition ledger and `docs/active-stage.md` for the stage
completion record.

## 1. What this phase answers

For a tenant/brand, an operation (registration/deposit/withdrawal/
wagering), optionally a product (casino/sportsbook/...), a country, at an
explicit `AsOf` — is this platform actually permitted to operate, given
both the licence's ceiling and every narrower policy decision beneath it?

It does **not** answer "which jurisdiction governs this player" —
that is `internal/jurisdiction`'s own, structurally separate question
(Stage 4I Phases A–D). The two are different domains answering different
questions, and this phase keeps them separate at the **package boundary**,
not merely by convention.

## 2. Repository-verification corrections honored (§0 of the ruling)

The design that follows corrects six things the original dispatch
otherwise would have gotten wrong, verified directly against the live
repository rather than assumed:

- **INC-1 — product and operation are two dimensions, not one.**
  `casino`/`sportsbook` are **products** (`platform_products`, migration
  0045), not operations. A new table, `platform_operations`, holds the
  operation vocabulary (`registration`, `deposit`, `withdrawal`,
  `wagering`) — deliberately disjoint from three pre-existing
  vocabularies that each answer a different question:
  `jurisdiction.OperationClass` (4 values, the player-jurisdiction
  resolver's call-site taxonomy), `asset_operation_eligibility.operation`
  (6 values, frozen by ADR 0037 §C.2), and `risk_rules.operation` (7
  values). Collapsing product and operation into one flat enum would
  reproduce the exact combinatorial-vocabulary failure `risk_rules` has
  already had to widen twice (migrations 0041, 0065).
- **INC-2 — `asset_change_requests` dual-control assessment made against
  migrations 0044 *and* 0047** (the latter materially hardened the
  former), not 0044 alone.
- **INC-3 — SEC-4I-F10's own hard trigger ("any change that introduces a
  licence status transition") does not fire this phase.** Phase E
  introduces no licence status transition; `resolver.go` gets zero diff.
  SEC-4I-F10 is therefore **re-scoped, not closed** (task registry).
- **INC-4 — `licences.expires_at` is `DATE`, not `TIMESTAMPTZ`** — any
  `AsOf` comparison casts explicitly rather than comparing directly.
- **INC-5 — formal revision of one Phase D.1 ruling**: `status` on both
  new tables is **two-valued** (`active`, `withdrawn`), not three-valued
  (`draft`, `active`, `withdrawn`) as Phase D's own table carries. Phase D
  needed `draft` to park HDR-J-8/HDR-J-9-blocked content; Phase E has no
  blocked content to park, and an in-force `draft` row would create a
  genuinely ambiguous third resolution case with no correct answer.
- **INC-6 — `jurisdictions.country_code` is fenced administrative
  metadata, never a country→jurisdiction resolver.** `jurisdictions.code`
  is a different code space (`KM-ANJ` is sub-national; `MT`/`CO` match
  ISO alpha-2 only coincidentally) — `internal/validation/country.go`'s
  own governing rule. INV-M-4 (below) makes the fence mechanical, not just
  documented.

## 3. Registry prerequisites

### 3.1 `jurisdictions.country_code`

`TEXT`, nullable, no default, shape-CHECKed (`^[A-Z]{2}$`), **not
unique** (several regulatory jurisdictions may sit in one country).
**Zero rows are auto-assigned by migration 0076** — no migration seeds
`jurisdictions` at all, and every existing operator-authored code is
ambiguous under the different-code-space rule, so a mechanical backfill
rule would inevitably get some row wrong.

Go write path: `CreateJurisdictionParams.CountryCode` (trim-only, never
uppercased — a lowercase input is rejected, never coerced, mirroring
SEC-4I-F9's own case-sensitivity lesson solved in the correct direction)
and a new `SetJurisdictionCountryCode` for existing rows (`jurisdictions`
has no other UPDATE path).

**INV-M-4 (binding, mechanically tested):** no Go code in the
player-resolution path (`resolver.go`, `precedence.go`, `types.go`,
`evidence.go`, `player_result.go`, `persist.go`, `restriction.go`,
`resolution_active.go`, `evidence_collection_active.go`,
`operation_purpose.go`, `purpose.go`) may reference the
`jurisdictions.country_code` column. Test:
`TestJurisdictionCountryCode_IsNotAJurisdictionResolver` (a source-level
grep for the literal, underscored SQL column name — deliberately **not**
the bare Go identifier `CountryCode`, which the pre-existing player
evidence types `VerifiedResidenceEvidence`/`DeclaredResidenceEvidence`/
`LocationSignalEvidence` already use for an unrelated, legitimate
concept: a player's own declared/verified/observed residence country).

### 3.2 `platform_operations`

New table, seeded with exactly four active rows (`registration`,
`deposit`, `withdrawal`, `wagering`) and nothing else. Fail-closed
`active` default (mirrors `platform_products`/`assets`). Read-open RLS
(`FOR SELECT USING (true)`) — this is vocabulary, not operating strategy,
and discloses no country/tenant/market. Platform-admin-only INSERT/UPDATE.
No DELETE policy, no `FOR ALL` policy.

## 4. The licence ceiling

`licence_country_ceilings` (new table, platform-scoped, no `tenant_id`
column at all): the platform-wide, append-only, effective-dated statement
of which countries a **licence** permits at all. Not a column on
`licences` (a ceiling is versioned; `licences` is flat current-state) and
not a row in the policy table (a different table, key, RLS posture, and
write permission is what makes "the licence is a ceiling, not a link in
an inheritance chain" structurally true, not merely documentary).

Schema: `state ∈ {enabled, disabled}`, `status ∈ {active, withdrawn}`
(exactly two values each — Phase D.1 rulings 8/9, strengthened by INC-5),
`authorization_reference` **required** when enabling
(`CHECK (NOT (status='active' AND state='enabled') OR
authorization_reference IS NOT NULL)` — ADR 0037 §C.5.3's asymmetry: a
disable, the fail-closed direction, never needs one), a partial unique
index enforcing at most one open version per `(licence_id, country_code)`,
and the identical forge-proof/append-only trigger pair migration 0075
established (byte-for-byte, names changed) — including the same narrow,
raw-SQL-only residual disclosure (two separate raw platform-admin
transactions, one holding a bare close open, can still construct a window
gap; unreachable via the sanctioned write path, which always closes and
inserts in one transaction; a `btree_gist` range-exclusion constraint
remains the named, deferred hardening).

RLS is **explicitly not** `USING (true)` (unlike migration 0075's
permissive precedent for `jurisdiction_precedence_configs`): SELECT
requires platform-admin scope **or** a tenant-scoped connection whose own
tenant is bound to *this* licence (a composite-ownership check) — a BYOL
tenant's own licence ceiling must not be readable by an unrelated tenant.

### 4.1 `EvaluateLicenceValidity` (SEC-4I-F10's technical predicate)

New file `internal/jurisdiction/licence_validity.go` — an **addition**,
not a modification, to `internal/jurisdiction` (every other file in that
package has zero diff). `EvaluateLicenceValidity(ctx, q, licenceID, asOf)`
is the **single** technical implementation, anywhere in this codebase, of
"is this licence currently reliable" (`LicenceValid`, `LicenceNotBound`,
`LicenceNotFound`, `LicenceSuspended`, `LicenceStatusExpired`,
`LicenceDateExpired` — plus `LicenceNotYetIssued`, corrected by finding
F4, §18; the interval is half-open, both boundaries under
`MKT-EXPIRY-1`). It is consumed by `internal/operatingmarket` only;
`resolver.go`'s own `resolveTenantLicence` does not call it and has zero
diff — SEC-4I-F10 stays open for the player-jurisdiction path, re-scoped
in the task registry rather than falsely closed.

**Expiry boundary is strict**: a licence is valid only while `AsOf`'s
UTC date is *strictly before* `expires_at` — invalid **on** its own
stated expiry date. This is a deliberate, disclosed engineering default,
not a human-decision item: it invents no legal grace period (the
fail-closed instruction), and its operational cost is exactly zero in
this phase (no consumer is wired). Named revisit trigger: **MKT-EXPIRY-1**
(task registry) — the phase that first wires `ResolveOperatingCountryPolicy`
into any consuming domain must confirm this boundary with compliance
before that wiring goes live. (Superseded in scope, not in substance, by
finding F4, §18: the interval is now half-open — valid from `issued_at`
INCLUSIVE to `expires_at` EXCLUSIVE — and `MKT-EXPIRY-1` covers BOTH
boundaries, not expiry alone.)

### 4.2 `licences.permitted_markets` disposition

**Retained, deprecated in place, not migrated, not dropped.** The column
is `JSONB NOT NULL DEFAULT '[]'`, written only by `CreateLicence`, read
only by `ListLicences`, has zero enforcement readers, and its content is
HDR-J-6-blocked ("not yet determined") — there is nothing to migrate.
`licence_country_ceilings` becomes the **sole authoritative** expression
of a licence's permitted countries. `COMMENT ON COLUMN` records the
deprecation; `CreateLicenceParams.PermittedMarkets`'s Go doc comment gains
the same statement. Removal trigger: **MKT-PM-1** (task registry) — the
phase that answers HDR-J-6 and writes the first real ceiling content must
drop this column and its Go field in the same change.

## 5. `OperatingCountryPolicy` store — `internal/operatingmarket`

**A new package**, not an addition to `internal/jurisdiction`. This is
what makes "structurally separate from player jurisdiction" and "no raw
player evidence in a market-policy record" **compile-time facts**:

- **INV-M-1** (mechanically tested via `go list -deps`,
  `TestOperatingMarket_ImportGraphInvariant`): `internal/operatingmarket`
  may import `internal/jurisdiction` (for `EvaluateLicenceValidity` only),
  `internal/validation`, `internal/audit`. It must never import
  `internal/identity`, `internal/kyc`, `internal/geolocation`, or
  `internal/rg`. `internal/jurisdiction` must never import
  `internal/operatingmarket`.
- A caller that imports `operatingmarket` cannot reach
  `PlayerJurisdictionResult`, `EvidenceSet`, or `Resolution` — they are
  not in scope. A market-policy row structurally cannot contain a piece
  of player evidence.

`const PolicyVersion = "stage-4i-e.v3"` as of this document's current
state (bumped `.v1` → `.v2` by §3.5-A AMENDMENT-1's resolution-algorithm
rewrite, then `.v2` → `.v3` by finding F4's `issued_at` fix in §18 —
AMENDMENT-2 and AMENDMENT-3 do NOT bump it, since neither changes what
`resolve()` computes for a fixed row set; see §18 for the full
attribution history. No existing row's stored `policy_version` value is
ever backfilled), written from a compiled-in constant onto every row and
every `Result`.

### 5.1 `operating_country_policies` — exact schema

Always tenant-owned (`tenant_id NOT NULL`, `ON DELETE CASCADE`); no
platform-wide row shape exists in this table at all (that is
`licence_country_ceilings`). `scope_kind ∈ {tenant, brand, operation}`
(migration 0045's `asset_authorizations` discriminator pattern, applied
verbatim), each with its own CHECK-enforced legal shape. `operation_code`
is `NULL`-able **only** for non-operation rows (required for `operation`
scope); `product_code` is nullable at operation scope, `NULL` = every
product. Four partial unique indexes (one per rung; two for the
operation rung, using `COALESCE(product_code, '*')`, rather than one
`COALESCE(brand_id, sentinel)` — a NULL-UUID sentinel is a value a real
column could theoretically hold, the two-index form has no such
ambiguity). Same forge-proof/append-only trigger pair as the ceiling
table, **with one deliberate asymmetry**: `operating_country_policies`
gets **no DELETE arm** on its append-only trigger, because it declares
`ON DELETE CASCADE` and Postgres runs referential-integrity actions with
RLS (and triggers keyed off application-level DELETE) bypassed for that
path — migration 0047's own recorded reasoning for why
`asset_authorizations` gets no `BEFORE DELETE` trigger while `assets`
does. `licence_country_ceilings` has no cascade and therefore **does**
get the full deny-DELETE trigger. This asymmetry is deliberate and must
not be "fixed" to be symmetric — doing so would break tenant deletion.

The write-time ceiling-narrowing trigger,
`operating_country_policies_enforce_ceiling` (`BEFORE INSERT` — correct
because this package issues no upsert, so the trigger fires exactly
once), reuses `asset_operation_eligibility_enforce_narrowing`'s exact
mechanism: an early return on the direction this function's own upward
ceiling checks can never be violated by (`disabled`/`withdrawn`), a
`SELECT ... INTO` of the parent fact with `IF NOT FOUND OR NOT <parent
enabled> THEN RAISE`, and a message a Go-side `classifyTriggerError`
(mirroring `internal/assetregistry`'s own precedent) maps to
`ErrCeilingExceeded` by substring. **This is NOT a claim that such a
write can never widen anything** — that claim was false and was the
AMENDMENT-2 defect (SEC-E-REV-1, §17): a withdrawal at the BRAND or
OPERATION rung removes an in-force block at a rung where absence
INHERITS, which is itself a widening act. That case is controlled by a
separate mechanism, the CHECK constraint
`ocp_inherit_rung_withdrawal_requires_authorization`, evaluated
independently of this trigger function.

RLS: **own tenant only, no platform-wide read at all** — a tenant's
country footprint is commercially sensitive operating strategy, and no
platform administrator may read it (a future operational need for that
is a `security`-owned decision requiring its own ruling, not a quiet
policy addition). No `brands`-style public read either (brand identity is
what an unauthenticated browser needs to render a page; operating-market
policy is the opposite kind of fact).

### 5.2 Write API

`CreateLicenceCountryCeilingVersion` (platform scope) and
`CreateOperatingCountryPolicyVersion` (tenant scope) both close the
currently-open version and insert the new one in **one transaction** —
there is **no `ON CONFLICT DO UPDATE`, no `INSERT ... ON CONFLICT`, and
no bare `UPDATE ... SET state = ...` anywhere in this package**. This is
the exact PHASE-B-ARCH-1 provenance defect (`resolution_active.go`/
`evidence_collection_active.go`'s upsert-in-place shape) and it is not
repeated. Every change is a new row; "effective from when, by whom, why,
under which authorization" is always the row's own truthful,
self-describing provenance.

Both writers are documented to be called **at most once per transaction
per key** — a second call shares that transaction's single `now()`,
lands `effective_to == effective_from`, and surfaces as
`ErrConcurrentPolicyWrite` with no concurrent writer
(PHASE-D-CR-P3-5's own disclosed, accepted diagnostic-quality limitation,
reproduced rather than rediscovered).

### 5.3 Resolution algorithm — `resolve()` / `ResolveOperatingCountryPolicy`

**One** internal function, `resolve()`, implements the entire five-step
algorithm and returns both the `Result` fields and the full diagnostic
chain; `ResolveOperatingCountryPolicy` discards the chain,
`ExplainOperatingCountryPolicy` returns it. This is a binding
implementation constraint: the decision logic itself is never duplicated.

1. **The ceiling. Unconditional, evaluated first, on every call, never
   short-circuited by any lower row.** Tenant→licence→
   `EvaluateLicenceValidity`→the ceiling row itself
   (`not_permitted_by_licence` with a distinct `LicenceCeilingReason` for
   each of: no ceiling row, ceiling withdrawn, ceiling disabled, licence
   suspended/expired; `licensing_unknown` for a data-integrity condition —
   no licence bound or a dangling `licence_id`).
2. **Tenant rung. Absence is TERMINAL** (a licence *grants* permission,
   it does not *instruct* — a tenant must explicitly opt in). Absence is
   further classified (`not_configured` / `policy_not_yet_effective` /
   `policy_expired`) by looking at every version for the key, ignoring
   the `AsOf` window, so a "not yet effective" future row and a genuine
   "gap after a gap" both name themselves rather than collapsing to one
   ambiguous `not_configured`. **A withdrawal at this rung needs no
   `authorization_reference`** (unlike bullet 4 below): absence here is
   terminal, never inherited, so a tenant-rung withdrawal can only ever
   narrow — this is the fence AMENDMENT-2 (§17) deliberately does not
   widen its CHECK constraint to cover.
3. **Brand rung. Absence means INHERIT.** Only evaluated when the query
   names a brand.
4. **Operation rung. Absence means INHERIT.** A SET of up to four
   candidates — one per brand-present × product-present combination
   (`brand_id IS NOT NULL`, `product_code IS NOT NULL`), bounded by the
   four partial unique open-version indexes, queried in
   specificity-descending order (`brand_id IS NOT NULL DESC, product_code
   IS NOT NULL DESC, effective_from DESC`) but evaluated as a SET with
   **first-disabled-wins, not most-specific-wins** (see **§3.5-A
   AMENDMENT-1** below, which corrects this bullet from the original
   ruling): withdrawn candidates are tombstones — they never permit,
   never block, and never mask a broader row; among the LIVE (`status ==
   'active'`) candidates, if any is `disabled` the resolver blocks,
   naming the **LEAST SPECIFIC** such disabled candidate as the block;
   only when no live candidate is disabled does the **MOST SPECIFIC**
   live candidate's `enabled` state source the outcome; if there is no
   live candidate at all, the rung inherits unchanged from brand/tenant.
   **Because absence at THIS rung, and at the brand rung above it (bullet
   3), INHERITS, withdrawing an in-force `active`+`disabled` row at
   either rung is a widening act, not the fail-closed direction it
   resembles, and requires a non-blank `authorization_reference`** —
   AMENDMENT-2 (§17), enforced by the CHECK constraint
   `ocp_inherit_rung_withdrawal_requires_authorization`.
5. **Permitted** — reachable only after an explicit, in-force, `active`,
   `enabled` ceiling row *and* an explicit, in-force, `active`, `enabled`
   tenant row (**INV-M-2**, tested by emptying every table and asserting
   no query ever returns `permitted`).

**Top-down, first-disabled-wins across rungs — and, WITHIN the operation
rung itself, first-disabled-wins across the full candidate SET, never
most-specific-wins** (corrected by **§3.5-A AMENDMENT-1**): the algorithm
walks top-down (licence → tenant → brand → operation) and, at each rung,
stops at the first `disabled` finding, naming it as the block. This
closes a real, reachable defect class the write-time trigger alone cannot:
the trigger only checks *upward* at write time, so a tenant disabled
**after** a brand row was validly enabled leaves an orphaned `enabled`
brand row in storage. Most-specific-wins would incorrectly resolve that as
`permitted`; top-down-first-disabled-wins correctly resolves it as
`disabled_by_tenant`, naming the rung an operator must actually change.
The identical principle applies **inside** the operation rung's own
candidate set — a more-specific `enabled` row must never be able to
outrank, and thereby unmask, a broader in-force `disabled` row for the
same operation; see §3.5-A AMENDMENT-1 for the defect this closes and why
"most-specific wins" was wrong even there.

**`AsOf` determinism (INV-M-3):** `resolve()` never calls `time.Now()`
and no SQL on the path calls `now()`/`clock_timestamp()`/
`CURRENT_TIMESTAMP` — mechanically tested by a source-level check
(`TestResolveOperatingCountryPolicy_SourceHasNoTimeNow`, stripping
comment lines first, since the function's own doc comments legitimately
*discuss* `time.Now()` in prose).

**No cache, no resolution table.** The resolved outcome is never
persisted or cached — it is a pure function of administrative rows that
are themselves append-only and effective-dated, so it is reconstructible
from the inputs at any `AsOf` without a second, driftable copy. This
deliberately differs from `jurisdiction_resolutions` (which records a
player-affecting determination regulators require be reconstructible) —
an operating-market answer needs no such record because its inputs
already are one.

### 5.4 The eleven-valued, non-forgeable `Result`

Every field of `Result` is unexported; there is no exported constructor
and no function that accepts an `Outcome` from a caller. Three structural
properties:

1. `Permitted() bool` is the **only** boolean, and it is derived from the
   eleven-valued `Outcome()`, never collapsed into a separate flag —
   `configuration_conflict` can never be mistaken for permission by a
   gate comparing against the wrong sentinel.
2. **No accessor exists** for `blockingScope`, `blockingVersionID`,
   `sourceScope`, `licenceID`, or `licenceCeilingReason` — the oracle
   rule made structural: a player-facing handler holding a `Result`
   cannot leak which scope blocked, because the language will not let it
   read the field. The only way to that information is
   `ExplainOperatingCountryPolicy`, a separate, staff-only call.
   Mechanically tested (`TestResult_ExposesNoBlockingScopeAccessor`,
   reflection over the exact method set).
3. **No `Code()`, `ID()`, or jurisdiction accessor of any kind** — a
   `Result` can never be substituted for a `jurisdiction.Resolution`.
   Mechanically tested
   (`TestOperatingMarket_NeverProducesAJurisdictionResolution`).

Two outcomes — `policy_expired` and `configuration_conflict` — are
**structurally unreachable via any sanctioned write path** (the
close-and-insert-in-one-transaction discipline leaves no gaps; the
partial unique indexes admit no second open row). They exist, are
produced, and are tested by constructing the corrupted state with raw
SQL (temporarily disabling the relevant trigger/CHECK, inserting an
explicit-timestamp or malformed row, then restoring it), because their
job is to make a corrupted version chain fail closed and **name itself**
rather than silently degrade to `not_configured` — strictly better than
Phase D's own `ORDER BY … LIMIT 1` silent pick.

## 6. Explain-why and the player-facing surface

`ExplainOperatingCountryPolicy` re-runs the identical `resolve()` and
additionally returns the full chain (`ChainStep` per rung: licence,
tenant, brand, operation) — **not** a second implementation of the
decision. Staff/admin only; no HTTP route exists in this phase.
Invariant (tested): its `Outcome` always equals
`ResolveOperatingCountryPolicy`'s `Outcome()` for the same query and row
set.

**Phase E exposes no player-facing surface at all.** The only function a
future player-adjacent path may call is `IsRegistrationPermitted`, whose
return type is `(bool, error)` — structurally incapable of carrying an
outcome, a scope, a version id, or a reason. It collapses every
non-`permitted` outcome to `false`, fails closed on every error (never
`(true, err)`), and requires a tenant-scoped, non-player-scoped
transaction (there is no `player_account_id` at registration time by
definition, and `operating_country_policies` has no player-read policy).

Binding rule for the phase that adds a player-facing surface: every
non-`permitted` outcome must map to **one** status code, one message, one
body shape — the ten non-`permitted` outcomes are indistinguishable to a
player.

## 7. Permission model and the dual-control ruling

Four new permissions (`internal/auth/permission.go`), each a named
exception under canonical-model §6.1's amendment ("no other permission
without a recorded architect ruling naming it" — this document is that
record):

| Permission | Scope | Sole grantee(s) |
|---|---|---|
| `PermOperatingMarketCeilingManage` | Platform | `RolePlatformAdmin` |
| `PermOperatingMarketTenantPolicyWrite` | Tenant | `RoleCompliance` |
| `PermOperatingMarketBrandPolicyWrite` | Tenant | `RoleTenantAdmin` |
| `PermOperatingMarketPolicyRead` | Both | `RolePlatformAdmin` (ceiling only), `RoleCompliance`, `RoleTenantAdmin` |

**Zero HTTP routes, zero OpenAPI change.** The permissions exist so the
phase that adds routes adds a handler, not a permission model — Phase
D's exact posture for `PermJurisdictionEvaluationPolicyWrite`.

### 7.1 Dual control — definitive ruling

**`asset_change_requests` cannot be safely extended** to a new operation
type for this, for four independent structural blockers (verified
against migrations 0044 **and** 0047 per INC-2), each sufficient alone:

1. `asset_code TEXT NOT NULL` — every request row must name an asset; an
   operating-country policy has none, and a synthetic sentinel would
   silently collide with a real asset code.
2. RLS makes the table **unreadable from the scope that needs it** — a
   tenant-scoped connection (which a tenant/brand-scope market enable
   runs under) cannot even `SELECT` this table, by migration 0044's own
   deliberate design.
3. `assets_change_requests_require_platform_principal` requires the
   requester to resolve to a platform-scoped (`tenant_id IS NULL`)
   `staff_users` row — a tenant compliance officer is structurally
   ineligible.
4. The consume functions are asset-table-specific, invoked only from two
   asset-registry triggers — wiring a third caller would put an
   asset-registry governance primitive on a foreign table's write path.

`bonus_change_requests` (migration 0063) clears blockers 2/3 (it is
tenant-scoped) but fails for different reasons: an eight-value
bonus-governance operation vocabulary, a monetary threshold model
meaningless for a country policy, and asset/amount columns with no
analogue.

**Fail-closed means, concretely:**

1. **No HTTP route, no console surface, no service-identity caller** —
   the *only* way to reach either write function in this phase is a
   direct Go call from an integration test. Zero production-reachable
   enable path.
2. **No resolver wiring** — no consuming domain calls
   `ResolveOperatingCountryPolicy`. An enable written in this phase has
   zero operational effect.
3. **A recorded authorization is structurally required for every
   enable** — `authorization_reference` is `NOT NULL`-on-enable by CHECK
   constraint on both tables.
4. **The asymmetry is preserved**: `disabled` writes need no
   authorization reference, at any scope, in any order — an emergency
   kill-switch must not need a second approver.
   **A withdrawal is not a disable, however**: at the BRAND/OPERATION
   rungs (where absence inherits) a withdrawal is the REMOVAL of a
   block, not the writing of one, and is therefore governed by
   AMENDMENT-2's own recorded-authorization requirement (§17), not by
   this asymmetry. The kill-switch asymmetry itself is untouched: a
   `state='disabled', status='active'` write still needs no
   `authorization_reference`, at any scope, in any order.

Named follow-up: **MKT-DUAL-1** (task registry, owned by `architect` with
`security` consulted) — creating a third platform approval mechanism (or
a generic `change_requests`/`change_approvals` pair all three domains
migrate onto) is a cross-cutting decision requiring its own ADR, and
**must be resolved before** any HTTP route/console surface reaches either
write path, any resolver wiring, or the first real (non-test) country
row. Until then, the absence of four-eyes is a known, bounded,
zero-exposure gap.

## 8. RLS / security posture summary

- `platform_operations`: read-open (vocabulary, discloses nothing),
  platform-admin-only write.
- `licence_country_ceilings`: **not** `USING (true)` — platform-admin
  read, or a tenant-scoped connection whose own tenant is bound to that
  exact licence. Platform-admin-only write. Full deny-DELETE trigger
  (this table has no `ON DELETE CASCADE`).
- `operating_country_policies`: own tenant only, **no platform-wide read
  at all**. Tenant-staff write (leading `player_account_id IS NULL`
  conjunct). No DELETE policy and **no** deny-DELETE trigger (deliberate
  asymmetry — see §5.1 — required so `ON DELETE CASCADE` from a tenant
  deletion is not blocked).
- No table has a `FOR ALL` policy. No table has a DELETE policy that
  would block referential-integrity cascades.
- `FORCE ROW LEVEL SECURITY` on all three tables.

## 9. Concurrency

Authoritative control: the partial unique indexes plus the
`effective_to > effective_from` CHECK — enforced by the database, never
by application check-then-insert. No advisory locks, no retry loops, no
sleeps, no timing tolerance in production code.

**Concurrency tests use ONLY the deterministic uncommitted-competing-row
technique**, confirmed by polling `pg_stat_activity` for a genuinely
blocked backend — never a `sync.WaitGroup` barrier or `time.Sleep`. This
exact mistake cost Stage 4I Phase D two fix rounds (a barrier
synchronizes transaction *start*, not the write *race*, and still failed
9/200 runs under CPU contention on independent re-verification).
`TestCreateOperatingCountryPolicyVersion_DeterministicConflictViaUncommittedCompetingRow`
and
`TestCreateLicenceCountryCeilingVersion_DeterministicConflictViaUncommittedCompetingRow`
reproduce it for both new tables; a third, loosened invariant-only test
(`TestOperatingCountryPolicy_ConcurrentCreatesNeverCorruptState`) covers
the genuine multi-goroutine case, asserting only what holds under every
legitimate interleaving (at most one open version, no gap/overlap in the
chain, every audit row matches a real version) and never a specific
success/failure count. Independently re-run by the implementer 5
consecutive times under `-race` with no failure.

## 10. Audit

One `audit_log` entry per mutation, same transaction, `before`/`after`
full-record state (real JSON, not merely row existence — tested by
unmarshalling and asserting actual field values, per PHASE-D-QA's own
"dead subtest" lesson). Metadata never contains a player account id, a
residence value, a location/geo signal, or any `EvidenceSet`/
`PlayerJurisdictionResult` field — structurally guaranteed by INV-M-1
(the package cannot reach those types) and asserted by
`TestOperatingMarketAudit_RecordsBeforeAfterAndNeverPlayerEvidence`.
`country_code` here is an administrative policy subject, not player
evidence — canonical-model §5.3's "never the evidence values" rule
governs player determinations, not administrative configuration.

**AMENDMENT-2 (§17) audit metadata.** Both writers (`policy_admin.go`,
`ceiling_admin.go`) additionally compute and record two keys on every
mutation, from `beforeState` and the write's own parameters only
(`resolve()` is NEVER called on the write path): `rung_block_transition`
(`"adds_block"` / `"removes_block"` / `"no_block_change"`, where
`blocking` = `status='active' AND state='disabled'`) and
`widening_capable` (a boolean - true for any `enabled`/`active` write at
any rung; for `operating_country_policies` ALSO true for a
`removes_block` withdrawal at `ScopeBrand`/`ScopeOperation` specifically
— never for a tenant-rung withdrawal, since absence there is terminal;
for `licence_country_ceilings` NEVER true for a withdrawal, since a
ceiling withdrawal is fail-closed by construction).

**Binding auditor rule**: an auditor searching for events that widened
the operating footprint filters on `widening_capable = true`. Filtering
on `state = 'enabled'` is wrong and will miss every inherit-rung
withdrawal, which is the shape that re-permits without ever writing the
word "enabled" — pinned by
`TestOperatingMarketAudit_NoWideningEventIsFindableOnlyByStateEnabled`.

## 11. PHASE-B-ARCH-1 interaction

**Determination: none of the three trigger conditions fire.** Phase E
adds zero HTTP routes/console surfaces, produces no report/export/
evidence pack, and does not read, write, or couple to
`jurisdiction_resolution_active`/`jurisdiction_evidence_collection_active`
in any way. `internal/operatingmarket` contains no reference to either
table or their Go functions. The deferred item remains deferred,
unchanged — re-evaluated this phase per its own three conditions, as
recorded in the task registry.

Two binding constraints kept true through implementation: (1) this
package must never gate its own resolution on either activation table —
absence of a tenant-scope row already means `not_configured`, which
already fails closed; (2) Phase E adds **no fourth activation switch** —
no `operating_market_active` table/column/flag of any kind.

## 12. What is explicitly NOT authorized by this ADR

Any HTTP route or OpenAPI change; any resolver wiring into `casino`/
`bonus`/`risk`/`payments`/`sportsbook`/registration/withdrawal; any
country/market content anywhere; any change to
`internal/jurisdiction/resolver.go`/`precedence.go`/`types.go`/
`evaluation_policy.go`/`evaluation_policy_admin.go`/
`resolution_active.go`/`evidence_collection_active.go`; any licence
status-transition operation; any change to `jurisdiction_resolution_active`
or `jurisdiction_evidence_collection_active`; any cache; any
`operating_market_resolutions` table; any fourth activation switch; any
real geolocation; any answer to HDR-M-1/HDR-M-2/HDR-J-6/HDR-J-7/HDR-J-8/
HDR-J-9.

## 13. Zero new human-decision items

Every candidate resolves to an ordinary, reversible engineering decision
inside this already-approved scope, or is explicitly deferred to an
already-open HDR item (HDR-J-6 for country content, HDR-M-1 for dual
control's governance policy). The licence expiry boundary is a disclosed
engineering default with a named revisit trigger (MKT-EXPIRY-1), not a
manufactured decision item.

## 14. Deliverables

**Created:** `migrations/0076_operating_market_country_policy.{up,down}.sql`;
`internal/operatingmarket/{types,resolve,explain,registration,
ceiling_admin,policy_admin,audit}.go` plus test files for every item in
this ADR's own test list; `internal/jurisdiction/licence_validity.go`;
this ADR.

**Modified:** `internal/jurisdiction/registry_admin.go` (`CountryCode`
field/param, `SetJurisdictionCountryCode`, `PermittedMarkets` doc
comment); `internal/auth/permission.go` (four constants + role wiring);
`internal/jurisdiction/migration_0075_integration_test.go` (the
migration-chain-window bump every prior migration landing on top of
0075 has required in turn — rolling back 2 instead of 1, accounting for
0076 explicitly); `internal/bonus/wave3_phase2_migrations_integration_test.go`
(same window bump, its own established pattern); `docs/governance/
task-registry.md`, `docs/governance/stage-4i-canonical-model.md`,
`docs/architecture/15-jurisdiction-and-licensing-model.md`,
`docs/active-stage.md`, `docs/progress.md`.

**Explicitly not touched:** `internal/jurisdiction/resolver.go`,
`precedence.go`, `types.go`, `evaluation_policy.go`,
`evaluation_policy_admin.go`, `resolution_active.go`,
`evidence_collection_active.go` — verified via `git diff --stat` to carry
zero diff from this phase.

## 15. A defect found and fixed during implementation, not in the original ruling

The original down-migration guard (checking `IF EXISTS (SELECT 1 FROM
operating_country_policies)` / `licence_country_ceilings` before allowing
a rollback) was **silently ineffective** as first written: migrations run
via a plain, scopeless database connection (no `app.platform_admin_principal_id`
or `app.tenant_id` GUC set), and both tables' RLS read policies are
narrower than migration 0075's permissive `USING (true)` precedent —
`operating_country_policies` in particular has **no platform-wide read
policy at all**, by design. A scopeless connection's `EXISTS` check would
therefore always see zero rows regardless of real content, defeating the
guard exactly when it mattered most. Fixed by temporarily disabling RLS
on both tables **inside the same transaction** as the existence checks —
safe and self-contained, since a `RAISE EXCEPTION` on a real finding
rolls back the `ALTER TABLE ... DISABLE ROW LEVEL SECURITY` right along
with everything else, and a clean pass proceeds to drop both tables
outright, making their RLS posture moot from that point on. Caught by
`TestMigration0076_DownMigrationCleanThenFailsOnDirtyDatabase` actually
failing against a real database — exactly the kind of defect the
dispatch's "run everything against a real Postgres" requirement exists to
catch, and is recorded here rather than silently fixed with no trace.

## 16. §3.5-A AMENDMENT-1 — the operation-rung resolution defect and its fix

**Status: RESOLVED IN PHASE E FIX ROUND** (task registry item
`MKT-NARROW-1`). This is a binding amendment to §5.3's original bullet 4
and its "Top-down, first-disabled-wins" paragraph (both corrected in
place above, not left standing alongside this section).

**The defect.** The original resolution algorithm ranked the operation
rung's candidates most-specific-first and evaluated **only the top-ranked
candidate**, falling through to INHERIT when it was absent or withdrawn.
Combined with the write-time trigger's narrowing check (which only
verifies upward, at write time, against whatever is enabled *at that
moment*), this let a more-specific `enabled` row both (a) widen past an
already-disabled broader row that was disabled *after* the narrower row
was written, and — reachable using **only narrowing writes, no widening
needed** — (b) once the narrower row was later withdrawn, unmask the
broader `disabled` row back into `permitted`, because the algorithm never
looked past the (now withdrawn, and therefore ignored) top candidate to
the live broader row beneath it. Concretely: enable every-product
`wagering` tenant-wide; disable it (broad disable, always legal); enable
`casino`-product `wagering` — refused today by the new write-time check
(§A.2), but the OLD resolver would have surfaced this only if that write
somehow succeeded, and — the reachable half, using only narrowing writes —
enable every-product `wagering`; disable it; legally narrow with a
`casino`-specific disable; **withdraw** the `casino`-specific disable. The
old top-candidate-only algorithm then inherited straight past the
withdrawn `casino` row without ever re-examining the every-product
`disabled` row beneath it, wrongly resolving `permitted`. This falsified
the resolution algorithm's own central correctness claim ("explicit
DISABLED always overrides inherited ENABLED"). Found independently by
three reviewers using three different techniques (architect-fidelity via
static trace, security via live API reproduction against a running
service, DB/RLS via raw SQL against the schema) — not by this package's
own test suite, which had no case combining a live broader disable with a
withdrawn narrower row.

**The fix (Option C — write-time narrowing enforcement PLUS a
full-candidate-set resolver rewrite; belt AND braces, neither alone is
sufficient):**

1. **Write-time (migration 0076, amended in place — no new migration
   file):** `operating_country_policies_enforce_ceiling()` gains a new
   step (4) that refuses to INSERT an `enabled`/`active` operation row
   while any broader, in-force, `active`, `disabled` operation row for the
   same operation (and, if named, the same brand/product) exists. This
   closes the write path but is **known to be incomplete by design**: it
   only checks *upward at write time*, so it cannot prevent a *later*
   broader disable from orphaning an *earlier*, validly-written narrower
   enable (the same structural gap the tenant/brand rungs already
   disclosed and accept) — the resolver is what makes that safe, not this
   trigger.
   **SEC-E-REV-3 (re-confirmed by AMENDMENT-2, §17):** this trigger is an
   intent guard — it is checked once, at INSERT time, and is not
   race-safe under READ COMMITTED: two concurrent writers can each pass
   its lookup-based checks against a not-yet-committed peer. Do NOT fix
   this by adding `SELECT ... FOR UPDATE`, an advisory lock, or
   `SERIALIZABLE` — the authoritative concurrency control for this
   package remains the partial unique indexes plus the
   `effective_to > effective_from` CHECK (§9), and this trigger is not,
   and is not meant to be, a substitute for that control.
2. **Read-time (`internal/operatingmarket/resolve.go`):** STEP 4 now
   queries the **full candidate set** (up to four rows, bounded by the
   four partial unique indexes — brand-present × product-present), not
   just the top two, and evaluates it as a **SET** with
   **first-disabled-wins**: (4.1) any two candidates sharing a specificity
   rank → `configuration_conflict` (strengthened from a top-two-only
   check); (4.2) an unparsable `state`/`status` on *any* candidate →
   `invalid_configuration`; (4.3) the **live** set is the candidates with
   `status == 'active'` — withdrawn candidates are tombstones: they never
   permit, never block, and never mask; (4.4) if *any* live candidate is
   `disabled`, block, naming the **least specific** such live-disabled row
   (not the most specific, and not the first one found scanning
   most-specific-first) as the blocking row; (4.5) otherwise, if the live
   set is non-empty (all `enabled` at that point), source from the **most
   specific** live row; otherwise inherit unchanged from brand/tenant. The
   resolver alone is now correct even when the write-time trigger is
   bypassed entirely (raw SQL) — tested directly
   (`TestOperatingCountryPolicy_RawSQLWidenedRowCannotProducePermitted`).

**INV-M-7** (new invariant, joining INV-M-1 through INV-M-6 recorded
elsewhere in this document and its companions; **the second clause below
is a REPLACEMENT, not a restatement, of the original text — the original
"no row can cause an outcome to become permitted that would not be
permitted without it" is FALSE as a blanket statement: an inherit-rung
withdrawal is exactly such a row, per AMENDMENT-2, §17**):
`ResolveOperatingCountryPolicy` returns `permitted` only if no in-force,
`active`, `disabled` row exists at any rung, or at any specificity
variant applicable to the query, at the operation rung, and this is
true only because of a three-part write-governing control, not because
no row can ever change the outcome:

1. **Every write that can itself increase what is permitted is
   authorization-gated at write time.** An `enabled`/`active` row, at
   any rung, requires a recorded `authorization_reference`
   (`ocp_enable_requires_authorization` /
   `licence_country_ceilings_enable_requires_authorization`) — never
   optional, never left to application discipline.
2. **`permitted` is reachable at all only through the two MANDATORY
   opt-in rows** — the licence ceiling and the tenant-scope policy, both
   required affirmatively `enabled`/`active` (INV-M-2). Absence of
   either is fail-closed (`not_permitted_by_licence` /
   `not_configured`), never a permissive default.
3. **A WITHDRAWAL of an in-force `active`+`disabled` row at the BRAND or
   OPERATION rung, AND a bare CLOSE of an in-force `active`+`disabled`
   row at those same two rungs (setting `effective_to` with no successor
   version), are BOTH writes covered by (1), not exceptions to this
   invariant** (AMENDMENT-2, §17; AMENDMENT-3, §18): because absence at
   those rungs inherits, either act is functionally an enable. The
   withdrawal shape carries the recorded-authorization requirement itself
   (`ocp_inherit_rung_withdrawal_requires_authorization`); the bare-close
   shape is refused outright — not authorization-gated, because a bare
   close has no row to attach an `authorization_reference` to — by the
   DEFERRED constraint trigger `ocp_inherit_rung_close_requires_successor`,
   which requires an open successor version to exist, in the same
   transaction, before the close is allowed to stand. This invariant's
   envelope is precisely: **no mutation reachable from an ordinary
   tenant-scoped connection** — an INSERT or an UPDATE, under RLS as
   enforced today — can produce a `permitted` outcome that would not
   otherwise occur, other than through one of the two gated shapes above.
   This envelope is **deliberately NOT extended** to cover "audit record"
   as an independent requirement (a control that merely logs a widening
   act after the fact does not make `permitted` unreachable — refusal at
   write/commit time does), and is **deliberately NOT extended** to cover
   an unqualified "delete": a `DELETE` issued by a role that bypasses RLS
   entirely is a separate, disclosed residual (§18, finding F2; ADR 0026),
   not a gap in this invariant, because it requires a strictly higher
   privilege bar than the ordinary tenant-scoped DML this invariant is
   stated over. No row of any other shape — a disable at any rung, in any
   order, or a withdrawal/close at the TENANT rung or of
   `licence_country_ceilings` (both fail-closed by their own
   absence-semantics, not by this control) — can ever cause a `permitted`
   outcome that would not otherwise occur.

**Tests added:** see `internal/operatingmarket`'s test files for the full
list (mandated names from the ruling): `TestOperatingCountryPolicy_ProductSpecificDisableNarrowsEveryProductEnable`
(replaces the deleted, wrongly-asserting
`TestOperatingCountryPolicy_ProductSpecificRowBeatsEveryProductRow`),
`TestOperatingCountryPolicy_MoreSpecificEnableUnderBroaderDisableIsRefused`,
`TestOperatingCountryPolicy_BroaderDisableStillBlocksAfterNarrowerRowWithdrawn`
(the exact regression reproducing the reviewers' sequence),
`TestOperatingCountryPolicy_RawSQLWidenedRowCannotProducePermitted`,
`TestOperatingCountryPolicy_BroadDisableAfterNarrowEnableBlocksRegardlessOfWriteOrder`,
`TestResolveOperatingCountryPolicy_BlockingRowIsTheBroadestLiveDisable`,
`TestResolveOperatingCountryPolicy_DuplicateRankDetectedBeneathAHigherRankedCandidate`
(fails against the old `LIMIT 2` query, proving the fix is load-bearing),
`TestExplain_EmitsEveryApplicableOperationCandidate`.

**Explicitly unchanged by this amendment:** every RLS policy/predicate on
all three tables; every permission constant and role grant; every CHECK
constraint, index, and FK; the `Result` type's eight accessors and eleven
outcomes; the licence ceiling's own logic (steps 1–3 of the trigger); the
two-dimension (operation × product) model of §1.3/§3.2, which this
amendment confirms rather than weakens; the four licence behavioral cases
of §3.6; no new HTTP route; no resolver wiring into any consuming domain.


## 17. §3.5-A AMENDMENT-2 — the inherit-rung withdrawal defect and its fix

**Status: RESOLVED** (task registry item `MKT-MIG76-1` records the schema
hazard this amendment creates for any environment that already applied
0076 before this change). This is a binding amendment, layered on top of
AMENDMENT-1 (§16) — nothing in §16 is withdrawn or superseded, this
amendment closes a second, independently-found defect (**SEC-E-REV-1**) in
the same trigger's write-time design.

**The defect.** The trigger's early return —
`IF NEW.state = 'disabled' OR NEW.status = 'withdrawn' THEN RETURN NEW`
— was documented (and, before this amendment, believed) to be
unconditionally the "fail-closed direction, always allowed." That is true
for `disabled` writes at every rung, and true for `withdrawn` writes at
the TENANT rung and on `licence_country_ceilings` (both fail-closed by
their own absence-semantics — ruling 5 and the ceiling's own
`not_permitted_by_licence` default). It is **false** for a `withdrawn`
write at the BRAND or OPERATION rung: absence at those two rungs INHERITS
from the rung above (**ruling 6**, from Phase D — see the trigger's own
step (4) comment), so withdrawing an in-force `active`+`disabled` row
there does not merely "assert nothing" — it REMOVES this rung's own
opinion and lets the parent rung's permit through. A withdrawal at an
inheriting rung is therefore a WIDENING act, indistinguishable in its
stored shape (`state='disabled', status='withdrawn'`) from a withdrawal at
a terminal rung, and it carried no authorization requirement and no audit
distinguishability before this amendment.

**Ruling: the kill-switch asymmetry does NOT cover this.** ADR 0037
§C.5.3 and this ADR's own §7.1 bullet 4 establish that writing
`state='disabled', status='active'` — the emergency kill-switch — needs no
`authorization_reference`, at any scope, in any order, so that an
emergency block is never gated behind a second approver. That asymmetry is
about DISABLING, not about WITHDRAWING a disable. A withdrawal is not a
disable; it is the removal of one. The kill-switch asymmetry is preserved
byte-for-byte and is not generalized, narrowed, or reinterpreted by this
amendment — see §7.1's own updated bullet 4.

**Mechanism, and why a CHECK constraint (not a trigger step, not a
resolver change):**

1. By the time the `BEFORE INSERT` trigger runs, the sanctioned write path
   (`CreateOperatingCountryPolicyVersion`) has already closed the
   predecessor version, in the SAME transaction, via its own
   `UPDATE ... SET effective_to = now()`. The trigger therefore has no way
   to see "the row being withdrawn" via an `effective_to IS NULL` lookup —
   there is nothing left to look up.
2. A CHECK constraint is a pure per-row predicate with no cross-row
   lookup, so it is race-free by construction under READ COMMITTED —
   unlike every lookup-based step in `operating_country_policies_
   enforce_ceiling()` (SEC-E-REV-3, §16 fix bullet 1, re-confirmed here:
   that trigger is an intent guard, not a concurrency control, and the
   authoritative concurrency control for this package remains the partial
   unique indexes plus the `effective_to > effective_from` CHECK, §9).
3. `ALTER TABLE ... DISABLE TRIGGER` (and
   `session_replication_role = 'replica'`) both disable triggers, but
   NEITHER disables a CHECK constraint — only `DROP CONSTRAINT` does. A
   CHECK is strictly harder to bypass than an equivalent trigger step,
   which matters here precisely because this control gates a widening
   act. `TestOperatingCountryPolicy_InheritRungWithdrawalCheckIsAConstraintNotATrigger`
   proves this directly: with every user trigger on
   `operating_country_policies` disabled, the raw INSERT is still
   rejected, by SQLSTATE `23514` naming
   `ocp_inherit_rung_withdrawal_requires_authorization`.

**Why there is no read-time half to this fix (unlike AMENDMENT-1).**
AMENDMENT-1 needed both a write-time trigger step AND a resolver rewrite,
because the write-time-only fix was, by its own design, incomplete (it
only checks upward at write time and cannot prevent a later broader
disable from orphaning an earlier narrower enable — §16 fix bullet 1).
AMENDMENT-2 needs no such second half: `resolve.go` already treats a
withdrawn row exactly correctly — a tombstone that never permits, never
blocks, and never masks (§16's own STEP 4 rewrite, AMENDMENT-1). Once an
inherit-rung withdrawal is properly AUTHORIZED and recorded, the resulting
`permitted` outcome is the CORRECT answer, not a defect to be caught at
read time — the defect was solely that the write could occur with no
authorization and no audit distinguishability. Confirmed:
`internal/operatingmarket/resolve.go` has **ZERO diff** from its
post-AMENDMENT-1 state, and `operating_country_policies_enforce_ceiling
()`'s EXECUTABLE body has **ZERO diff** (comment-only changes, correcting
the now-false "always allowed" claim at the early return — see §5.1).

**Explicit Option-2 rejection: ruling 6 is RECONFIRMED, not weakened.** A
candidate alternative fix considered and REJECTED was to close this gap by
changing ruling 6 itself — making absence at the BRAND/OPERATION rungs
TERMINAL (like the tenant rung) instead of inheriting, which would make
every withdrawal fail-closed by construction and need no new control at
all. This was rejected: it is a materially larger architectural change
(it would alter what every existing and future brand/operation ABSENCE
means, not just what a WITHDRAWAL does), it contradicts the deliberately
chosen inheritance model that lets a tenant/brand set a broad default and
narrower rungs opt out selectively, and it was not required to close the
actual defect — the narrow CHECK constraint closes SEC-E-REV-1 completely
on its own. Ruling 6 (absence at BRAND/OPERATION scope INHERITS the rung
above) stands **UNCHANGED** by this amendment, and is now the explicit,
documented REASON the new CHECK constraint exists, not a fact this
amendment revisits.

**`PolicyVersion` stays `"stage-4i-e.v2"` — not bumped to `v3`.**
`PolicyVersion` exists to distinguish resolution-ALGORITHM versions: a
change in what `resolve()` computes for an identical row set (this is why
AMENDMENT-1 bumped `v1` → `v2` — STEP 4's own logic changed). AMENDMENT-2
changes neither `resolve()` nor `operating_country_policies_
enforce_ceiling()`'s executable body; it adds a new constraint on which
row sets the sanctioned write path can construct in the first place.
`resolve()` computes the identical function of the stored rows before and
after this amendment — there is no algorithm version to record. The
schema-level change is versioned by the migration number and the new
constraint's own name, not by `PolicyVersion`.

**Audit change:** see §10's "AMENDMENT-2 audit metadata" subsection —
two new keys, `rung_block_transition` and `widening_capable`, on every
`operating_market.policy_version_created` /
`operating_market.licence_ceiling_version_created` audit row, plus the
binding auditor rule (`widening_capable = true`, never `state = 'enabled'`,
is the correct filter for "did this write widen the operating footprint").

**Explicitly unchanged by this amendment:**
`internal/operatingmarket/resolve.go` (zero diff);
`operating_country_policies_enforce_ceiling()`'s executable body (zero
diff — comment-only); every RLS policy/predicate on all three tables;
every permission constant and role grant; every index; every FK other
than the CHECK constraint itself (which is not an index or FK); the
`Result` type's eight accessors and eleven outcomes; `PolicyVersion`
(stays `"stage-4i-e.v2"`); the kill-switch asymmetry for ordinary
`disabled`/`active` writes, at any scope, in any order (ADR 0037 §C.5.3,
this ADR's §7.1 bullet 4); ruling 5 (tenant-rung absence is terminal) and
ruling 6 (brand/operation-rung absence inherits), both re-confirmed rather
than revisited; the licence ceiling's own fail-closed withdrawal
semantics (`CeilingReasonCeilingWithdrawn`); no new migration file (0076
amended in place — see `MKT-MIG76-1`); no new HTTP route; no resolver
wiring into any consuming domain.

## 18. §3.5-A AMENDMENT-3 — the bare close

**Status: RESOLVED** (task registry item `MKT-MIG76-1` now records
migration 0076 amended in place a THIRD time). This is a binding
amendment, layered on top of AMENDMENT-1 (§16) and AMENDMENT-2 (§17) —
nothing in either is withdrawn or superseded. This is also, per this
amendment's own structural argument below, the **final** amendment in
this defect family: see "do NOT commission a fourth round" at the end of
this section.

**The defect (SEC-E-REV-2, "the bare close").** AMENDMENT-2 fenced the
INSERT-shaped way of removing a live block at an inheriting rung: writing
a `withdrawn` successor row. It did NOT fence the UPDATE-shaped way:
setting `effective_to` on the open row and writing NO successor at all.
`resolve()` windows every rung on `effective_from <= asOf AND
(effective_to IS NULL OR effective_to > asOf)`, so a closed row simply
leaves the window — byte-for-byte the same effect as a withdrawal,
reached by a different verb, with no `authorization_reference` and no
audit distinguishability. Every other control on this table is
INSERT-shaped (both AMENDMENT-1/AMENDMENT-2 CHECKs, `operating_country_
policies_enforce_ceiling()`, and the Go audit computation in
`policy_admin.go`), and `operating_country_policies_enforce_append_only()`
does not gate the close at all — it **blesses** it (it forces
`effective_to := now()` to prevent backdating, and has no opinion on
whether the close should occur).

**Why fix now, not defer.** Two reasons, both independent of each other:
(1) **cost asymmetry** — the fix is a single deferred constraint trigger
with no read-time half and no change to `resolve()`, materially cheaper
than the cost of leaving a second, structurally identical bypass of
AMENDMENT-2's own control sitting next to it; (2) **the piggyback
variant partially defeats AMENDMENT-2's own audit purpose** — a single
transaction can contain one fully authorized, fully audited inherit-rung
withdrawal alongside one bare close of a DIFFERENT key that achieves the
identical widening effect with no authorization and no audit row at all,
so an auditor filtering on `widening_capable = true` (§17's own binding
rule) would still miss half of what actually happened inside that
transaction were this defect left open.

**Ruling: the successor-must-exist requirement is sufficient; no
re-implemented authorization test.** AMENDMENT-3 does not ask "was this
close authorized" — it asks "does an open successor exist at the same
key, in the same transaction." This is sufficient because the successor
set is closed by exactly three existing CHECKs, and every legal shape a
successor can take is either non-widening on its own or already gated by
one of them:

| Successor shape | Why it cannot silently widen |
|---|---|
| `active` + `disabled` | The block persists at this rung; no widening occurred. |
| `active` + `enabled` | Gated by `ocp_enable_requires_authorization`. |
| `withdrawn` + `disabled` | Gated by `ocp_inherit_rung_withdrawal_requires_authorization` (AMENDMENT-2). |
| `withdrawn` + `enabled` | Forbidden outright by `ocp_withdrawn_is_disabled`. |

AMENDMENT-3 therefore forces the **verb** (close vs. withdraw) through
the **shapes** AMENDMENT-2 already fences — the two amendments compose
rather than duplicate each other, and neither is redundant:
AMENDMENT-2 alone would still let a bare close through; AMENDMENT-3
alone (with no successor-shape gating) would let an attacker "satisfy"
it by inserting an unauthorized `withdrawn` successor — which
AMENDMENT-2's own CHECK independently refuses (proven directly by
`TestOperatingCountryPolicy_CloseWithUnauthorizedWithdrawnSuccessorIsStillRejectedByAmendment2`).

**Mechanism, and why a CONSTRAINT TRIGGER — not a CHECK (AMENDMENT-2's
own mechanism), not a trigger step, not a resolver change.** The
predicate is inherently **cross-row**: it compares the closed row's key
against whatever, if anything, replaced it — a fact a `CHECK` constraint
(evaluated against a single row's own columns only) cannot express at
all. **§17's own argument that a CHECK is harder to bypass than a
trigger still stands and is NOT withdrawn here** — it simply does not
decide this case, for two independent reasons: (a) no CHECK formulation
of a cross-row successor-existence predicate exists in PostgreSQL, full
stop; and (b) any actor with the capability to disable triggers is
*already* able to defeat AMENDMENT-2's own CHECK more cheaply than they
could ever defeat a trigger-based control — with the append-only trigger
disabled, a single UPDATE can set `status = 'withdrawn'` **and** forge a
non-blank `authorization_reference` in the same statement, satisfying
the CHECK's literal text while lying about authorization. Trigger-
disabling capability is therefore outside **both** amendments' threat
model equally; it is not a reason to prefer one mechanism over the other
here. The genuine fix for that residual is the migration-owner/runtime-
role separation already recorded at ADR 0026, out of this stage's scope.
A future reader must not conclude §17 forbids §18's mechanism — §17
decided a different, narrower question (CHECK vs. trigger-STEP for a
per-row predicate), and this section decides a question §17 never faced
(a control that is unavoidably cross-row).

**Why `DEFERRABLE INITIALLY DEFERRED` is load-bearing, not decoration.**
The sanctioned writer, `CreateOperatingCountryPolicyVersion`, closes the
predecessor and inserts the successor in ONE transaction, in that order.
An immediate (non-deferred) constraint would evaluate the close before
the insert ever runs and reject the only legitimate write path outright.
A transaction that issued `SET CONSTRAINTS ALL IMMEDIATE` before reaching
that writer's close statement would fail — **fail-CLOSED** (the write is
rejected outright; nothing widens) — so this is a liveness hazard, not a
safety one, and the migration comment instructs future authors not to
add such a call.

**Scope is derived, not arbitrary — the SAME derivation as AMENDMENT-2's
(§17).** Only rows whose disappearance actually widens are covered:

- `scope_kind IN ('brand', 'operation')` only. A TENANT-rung bare close
  resolves to `not_configured`/`policy_expired` (absence there is
  TERMINAL, ruling 5) and a bare close on `licence_country_ceilings`
  resolves to `not_permitted_by_licence`; both directions NARROW.
  **If either absence semantic ever changes, this trigger must be
  widened in the same change** — fenced by
  `TestOperatingCountryPolicy_TenantRungBareCloseNarrowsSoIsPermitted`
  and `TestLicenceCountryCeiling_BareCloseIsFailClosedSoIsPermitted`.
- `status = 'active' AND state = 'disabled'` only. Closing an `enabled`
  row or an already-`withdrawn` row removes nothing that was blocking —
  fenced by
  `TestOperatingCountryPolicy_BareCloseOfEnabledOrWithdrawnRowAtInheritRungIsPermitted`.
- `AFTER UPDATE` only, never `DELETE` — omitted for exactly the reason
  `operating_country_policies_enforce_append_only()` omits it (`tenants`'
  `ON DELETE CASCADE`, §7.3). A `DELETE` by an RLS-bypassing role remains
  an uncontrolled removal path — this is finding **F2** below, a
  DISCLOSED RESIDUAL, not an oversight: it requires bypassing RLS
  entirely, a strictly higher bar than the ordinary tenant-scoped DML
  this trigger closes.

**`PolicyVersion` bumps `stage-4i-e.v2` → `stage-4i-e.v3` for finding F4
ONLY — NOT for AMENDMENT-3.** `PolicyVersion` exists to distinguish
resolution-ALGORITHM versions: a change in what `resolve()` computes for
an identical stored row set. AMENDMENT-3 changes neither `resolve()` nor
`operating_country_policies_enforce_ceiling()`'s executable body; it adds
a write-time/commit-time constraint on which row sets the sanctioned
write path can construct in the first place — `resolve()` computes the
identical function of a fixed stored row set before and after AMENDMENT-3,
exactly as AMENDMENT-2 did not bump the version for the identical reason
(§17). The bump to `v3` in this same round is attributable ENTIRELY to
finding F4 (§2.5's `EvaluateLicenceValidity` gaining the `issued_at`
check below) — a genuine algorithm change, since an identical stored row
set (a licence row with a future `issued_at`) now resolves differently
than it did under `v2`.

**F2/F3/F4 dispositions, recorded together since all three were found in
the same review round as SEC-E-REV-2:**

- **F2 (documentation-only disclosure) — IMPLEMENTED.** The REMOVAL
  direction on `operating_country_policies` is wholly uncontrolled for
  any role that bypasses RLS entirely — no trigger, no CHECK, no audit
  record, for a raw `DELETE`. Disclosed in the migration 0076 comment
  near where the DELETE arm is omitted, in this section, and folded into
  the INV-M-7 rewrite above. The genuine fix is the migration-owner/
  runtime-role separation already recorded at ADR 0026, a platform-wide
  operational change out of this stage's scope. No new task-registry item
  is opened for F2 — ADR 0026's existing item already covers it.
- **F3 (RLS on `tenants`/`licences`/`jurisdictions`) — NOT AUTHORIZED
  THIS DISPATCH.** The actual fix (adding RLS to those three tables) is
  explicitly a separate, future, `security`-owned change. This dispatch
  makes no RLS change to any of the three. `MKT-SCOPE-1` (task registry)
  is amended, not newly created, to add `tenants` alongside
  `jurisdictions`/`licences` and to record trigger condition (b): this
  item must also be resolved before `ResolveOperatingCountryPolicy` is
  wired into any enforcement path, independently of whether any HTTP
  route reaches `internal/jurisdiction` — Phase E made licences the ROOT
  of the operating-market ceiling, so an unprotected `licences`/`tenants`
  row is now a direct path to widening an operating-market answer, not
  merely a player-jurisdiction concern.
- **F4 (licence validity fail-open on `issued_at`) — IMPLEMENTED.** See
  below.

**F4: `EvaluateLicenceValidity`'s missing `issued_at` check.** The
original predicate read only `status` and `expires_at`; a licence with a
future `issued_at` resolved `LicenceValid` — a fail-**OPEN** in the
ceiling's own root fact, the licence itself, independent of and prior to
every rung this ADR otherwise governs. Fixed by widening the interval to
**half-open**: valid from `issued_at` INCLUSIVE to `expires_at`
EXCLUSIVE, checked strictly after the `status` switch (a suspended
licence must still read `suspended`, not `not_yet_issued` — status
outranks the date checks) and strictly before the pre-existing expiry
check. `licences.issued_at` is `DATE` and NULLABLE, and no Go code writes
it today; a `NULL` `issued_at` is treated as "no issue date asserted" and
falls through to the existing expiry check, deliberately NOT as "not yet
issued" (the latter would fail-close every existing licence row with no
compliance-confirmed basis for doing so). `MKT-EXPIRY-1` is widened to
cover both boundaries and the `issued_at` NULL/NOT-NULL open question,
rather than opening a new item.

**Do NOT commission a fourth round of the same kind.** This is the third
and — per the derivation above — structurally final amendment in the
"absence-inherits-so-removal-is-widening" defect family at this table:
AMENDMENT-1 closed the read-time half (a more-specific enable unmasking a
broader disable), AMENDMENT-2 closed the INSERT-shaped removal (an
unauthorized withdrawal), and AMENDMENT-3 closes the UPDATE-shaped
removal (a bare close with no successor). Between them, every mutation
SHAPE in this specific defect family (a removal that inherits its way to
a wider outcome) is now accounted for — but the residual set is THREE
items, not two, and the third is reachable by an entirely ordinary
tenant-scoped connection, not merely by privilege escalation or a
trigger-disabling actor:

1. A privilege-escalated `DELETE` (F2, explicitly disclosed, out of this
   family's concern — it requires bypassing RLS entirely).
2. A trigger-disabling actor (explicitly out of scope for both
   AMENDMENT-2 and AMENDMENT-3 — ADR 0026's migration-owner/runtime-role
   residual is the genuine fix).
3. **A third residual, found and live-reproduced by independent security
   review with a completely ordinary, non-privilege-escalated,
   trigger-intact tenant-scoped transaction**: an ordinary-RLS
   transaction that closes the block and inserts a `withdrawn` successor
   carrying an unverified `authorization_reference`. Every CHECK and
   trigger in this defect family — AMENDMENT-1 through AMENDMENT-3 —
   verifies only that a widening write carries a non-blank
   `authorization_reference` STRING; none of them, and none can, verify
   that the string names a REAL, valid approval. A staff actor (or a
   compromised staff credential) with ordinary write access to this table
   can satisfy every CHECK and every trigger here literally while lying
   about authorization. This is not fixable by a fourth iteration of the
   close/withdraw/insert mechanism this family already covers — the
   mechanism correctly enforces "a reference is present," which is all it
   was ever designed to enforce. The genuine fix is validating that
   reference against a real approvals record, which is **MKT-DUAL-1**'s
   scope (that item already binds itself to "every widening-capable
   write," explicitly including the AMENDMENT-2 shape); no fourth
   amendment to this trigger family would address it.

A future reviewer who suspects a further gap in this specific family
should first check whether it is actually one of these three
already-disclosed, differently-owned residuals before proposing
AMENDMENT-4. Genuinely differently-scoped future review is still
worthwhile and is NOT discouraged — three concrete, named targets, none
of them this family: **AsOf provenance** (whether a caller-supplied
`AsOf` can itself be manipulated to evade a since-corrected block),
**`tenants`/`licences` RLS** (F3 above, already routed to `security`), and
**`platform_operations`/`platform_products` write-time governance**
(whether the vocabulary tables themselves need the same
append-only/authorization discipline as the policy tables that reference
them).

**Tests added** (`internal/operatingmarket/amendment_3_integration_test.go`,
exact names from the ruling):
`TestOperatingCountryPolicy_BareCloseOfBrandRungDisableIsRejectedAtCommit`,
`TestOperatingCountryPolicy_BareCloseOfOperationRungDisableIsRejectedAtCommit`,
`TestOperatingCountryPolicy_CloseSuccessorCheckIsDeferredToCommitNotStatement`,
`TestOperatingCountryPolicy_SanctionedCloseThenInsertStillSucceedsAtEveryRung`,
`TestOperatingCountryPolicy_PiggybackAuthorizedWithdrawalPlusBareCloseRollsBackWholeTransaction`,
`TestOperatingCountryPolicy_BareCloseOfEnabledOrWithdrawnRowAtInheritRungIsPermitted`,
`TestOperatingCountryPolicy_TenantRungBareCloseNarrowsSoIsPermitted`,
`TestLicenceCountryCeiling_BareCloseIsFailClosedSoIsPermitted`,
`TestOperatingCountryPolicy_CloseWithUnauthorizedWithdrawnSuccessorIsStillRejectedByAmendment2`,
`TestOperatingCountryPolicy_ConcurrentCloseCannotBeRescuedByAnotherTransactionsSuccessor`,
`TestOperatingCountryPolicy_TenantDeletionStillCascadesAfterAmendment3`,
`TestOperatingCountryPolicy_CommitErrorIsClassifiedAsPolicyCloseRequiresSuccessor`.
Plus F4's own tests in `internal/jurisdiction` and `internal/operatingmarket`
(see below) and `TestPolicyVersion_IsPinnedToV3`.

**Explicitly unchanged by this amendment:** every RLS policy/predicate on
all three tables; every permission constant and role grant; every CHECK
constraint, index, and FK other than the new constraint trigger itself
(not an index or FK); `resolve.go` (zero diff from AMENDMENT-3 — its one
new case in this same round comes from F4, a separate, independently-
attributed change); `operating_country_policies_enforce_ceiling()`'s and
`operating_country_policies_enforce_append_only()`'s executable bodies
(zero diff — comment-only); the `Result` type's eight accessors and
eleven outcomes; the licence ceiling's own logic (trigger steps 1–3); the
kill-switch asymmetry; ruling 5 and ruling 6, both re-confirmed rather
than revisited; no new migration file (0076 amended in place a third
time — see `MKT-MIG76-1`); no new HTTP route; no resolver wiring into any
consuming domain.
