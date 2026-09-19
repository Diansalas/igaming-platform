# Stage 4I — Platform-Wide Jurisdiction Resolution Foundation: Final Gate Report

**Branch:** `claude/focused-wright-jw88w9`. **Final commit:** `f1f8d13`.
**Status label (CLAUDE.md discipline): `PARTIALLY IMPLEMENTED`.**

This is the mandated Final Gate Report for Stage 4I, per the governing human
directive "STAGE 4I — PLATFORM-WIDE JURISDICTION RESOLUTION FOUNDATION."

---

## 1. Executive verdict

**A real, tested, security-certified jurisdiction resolution FOUNDATION now
exists platform-wide** — a canonical data model, a non-forgeable resolution
interface, a registry, an append-only resolution-record table, and four real
consumers (AssetAuthorization, Risk, casino's per-game blocklist, five Bonus
admin surfaces) all speaking the same contract for the first time, replacing
what Wave 3 found were four silently-incompatible absent-value semantics.

**This foundation does NOT yet resolve any player's actual jurisdiction.**
The one producible basis (`tenant_licence`) has **no application write path
anywhere in the repository** — `tenants.licence_id` cannot be set by any
actor through the platform today — so every production resolution for a
player-scoped operation returns `unresolved(no_signal)`, correctly and by
design. This means: Bonus deposit/cashback sweep issuance stays blocked,
casino's per-game blocklist denies every launch of an armed game, and no
regulatory jurisdiction-based enforcement capability can be claimed. This is
the stage's own intended, disclosed outcome — closing four incompatible
absent-value contracts into one correct, fail-closed one — not a partial
failure to deliver a working resolver.

Thirteen specialist phases ran (reconnaissance → identity-compliance → risk
→ security → architect synthesis → product-owner-proxy → security ×3 more
→ backend → casino → bonus-engine → payments → sportsbook → qa → architect
final → security final). **Twelve real defects were found and fixed** across
the review chain (`SEC-4I-F1` through `SEC-4I-F8`, `DR-4I-ARCH-01`,
`DR-4I-BONUS-01`, plus two coverage gaps QA closed), every one proven with a
regression test shown to fail pre-fix and pass post-fix. Nothing was
self-certified. The final independent security/compliance review verdict:
**CERTIFIED WITH NAMED EXCEPTIONS** (none blocking).

## 2. Current jurisdiction architecture (before vs. after)

**Before Stage 4I** (per `docs/governance/stage-4i-reconnaissance.md`): a
fully-built configuration layer (`jurisdictions`, `licences`,
`tenant_jurisdiction_configs`, `asset_authorizations.jurisdiction_id`,
`risk_rules.jurisdiction_code`) with **no production layer** — nothing
computed which jurisdiction applied to a player performing an operation.
Four consumers treated an absent value with four different, mutually
incompatible semantics: AssetAuthorization denied unconditionally; Risk
denied only conditionally (correctly, per its own rule structure); casino's
per-game blocklist failed OPEN (never executed, since its one caller never
set the field); payments' `SupportedCountries` treated empty as permissive.
Three registry tables (`licences`, `tenants.licence_id`,
`tenant_jurisdiction_configs`) had never been read or written by production
code at all.

**After Stage 4I**: a real `internal/jurisdiction` package provides one
canonical `Resolve` function every consumer calls; a registry admin surface
exists for the first time; an append-only `jurisdiction_resolutions` table
records every resolution attempt; casino's blocklist and Bonus's five admin
surfaces both now consume the resolver correctly; Risk's conditional
fail-closed contract was reviewed and confirmed correct as originally
designed (not a defect, per risk's own Phase 3 ruling); payments'
`SupportedCountries` was reviewed and determined to be a legitimate,
distinct provider-capability concept, not a jurisdiction-gating defect.

## 3. Canonical data model

Per `docs/governance/stage-4i-canonical-model.md` §2-3, seven concepts are
distinguished (per the directive's own requirement, never conflated):
player location, player residence, nationality, tenant licensing
jurisdiction, brand operating jurisdiction, transaction/operation
jurisdiction, product jurisdiction. Of these: **only tenant licensing
jurisdiction is currently producible** (via `tenants.licence_id →
licences.jurisdiction_id → jurisdictions.code`, gated to tenant/brand-subject
operations only, never player-scoped — enforced by a DB `CHECK`). Player
location/residence/nationality do not exist in any form anywhere in the
codebase and are gated on HDR-J-3 (a newly-opened Human Decision Register
item). Brand operating jurisdiction has no independent schema — a brand
*narrows* within its tenant's jurisdiction set, never sources one (ruling
`BI-4I-1`, closing a prior config-vs-enforcement inconsistency with no
schema change). Product jurisdiction exists only as unread configuration.
Operation jurisdiction exists only as the new `jurisdiction_resolutions`
table (per-attempt, not yet linked via FK from any consumer — a named,
carried gap, `DR-4I-BE-02`).

## 4. Resolution interface

`internal/jurisdiction.Resolution`: a non-forgeable type (unexported fields,
no exported constructor; `Code()`/`ID()` return `ErrNotResolved` for any
non-`resolved` outcome — verified structurally, not just by convention, by
both the implementation spot-check and the final certification). A 3-valued
`Outcome` (`resolved` / `unresolved` / `refused`) is the ONLY thing any gate
branches on; a separate diagnostic `Reason` enum exists for logging/audit
but no gate ever branches on it — a deliberate two-axis design specifically
to prevent an eight-value branch eventually getting one case wrong in the
permissive direction. `Params` carries tenant/brand/player-account/operation
scope, server-derived only (never client-suppliable — see §16). `AssertScope`
provides layer-2 re-assertion at each consumption point. `Resolve` runs
against a `ReadOnlyQuerier` interface (no `Exec` — a write inside `Resolve`
does not compile), enforcing the mandatory "resolver must be read-only on
the evaluation path" constraint (risk's H-2 finding) structurally, not by
convention.

## 5. Source hierarchy

A closed `Basis` enum: `tenant_licence` (the only currently-producible
basis), plus `player_declared_residence`, `player_verified_residence`,
`kyc_corroboration`, `geo_signal`, `retail_node`, `tenant_asserted` (all
reserved, schema-ready, structurally unreachable today), and
`staff_supplied`/`platform_fallback` (reserved specifically so that a value
recorded via an interim audit-labeling mechanism, or a future fallback
policy, is always distinguishable from a real resolver output — the
resolver itself can never emit either). Precedence CONTENT (which signal
wins when more than one exists) is blocked on HDR-J-2; the precedence
MECHANISM (a table keyed on tenant/licence side specifically, to avoid a
bootstrap circularity risk identified by risk's Phase 3 report) is built
now (`jurisdiction_precedence_configs`, schema only, no rows).

## 6. Outcome states

`resolved(determined)`; `unresolved(no_signal)` (the universal answer for
every player-scoped operation today); `unresolved(insufficient_confidence)`;
`unresolved(irreconcilable_bases)`; `refused(dependency_unavailable)`
(genuine query failure, distinct from a data gap); `refused(scope_mismatch)`
(the layer-2 assertion firing, added this stage as `DR-4I-ARCH-01`'s fix);
`refused(unsupported_operation_class)`; `refused(registry_unknown_code)`.
The directive's five required distinctions (successful/uncertain/
unavailable/conflicting/unsupported) map cleanly onto this set.

## 7. Tenant/brand/licensing interaction

Tenant is server-derived only from the verified JWT/GUC, never client-
supplied — the same discipline this platform already applies to
`tenant_id` everywhere else. Licensing model comes from `tenants.licensing_model`
(the pre-existing, already-server-resolved field). A brand has no
independent jurisdiction (ruling `BI-4I-1`) — it narrows within its
tenant's jurisdiction set. The one live cross-tenant isolation gap found and
fixed this stage (`DR-4I-ARCH-01`) was precisely in this area: `tenants`,
`licences`, and `jurisdictions` carry **no RLS at all**, so the resolver's
layer-2 tenant-scope assertion (`assertTenantScope` inside
`resolveTenantLicence`) is load-bearing, not defense-in-depth — confirmed
independently by the final security review via a live cross-tenant query
that returned tenant A's data from a tenant-B-scoped connection before the
fix, and returns `refused(scope_mismatch)` after it.

## 8. Persistence/history model

`jurisdiction_resolutions`: append-only, tenant-scoped, `FORCE ROW LEVEL
SECURITY`, `NOBYPASSRLS`, no UPDATE/DELETE policy, `BEFORE UPDATE OR
DELETE`/`BEFORE TRUNCATE` deny triggers (mirroring `audit_log`'s pattern),
no player-read policy (a player learning which basis was selected/rejected
is itself a side channel, per security's ruling), two binding CHECK
constraints (`resolved <=> jurisdiction_code present`; `player_account_id
IS NULL OR selected_basis <> 'tenant_licence'`). **Named, disclosed gap**:
`Persist` (the write function) has **zero production call sites** —
`jurisdiction_resolutions` is never actually written outside tests
(`DR-4I-BE-01`). Architect's explicit ruling: this is deliberate for Stage
4I — writing one row per launch attempt, all identical
`unresolved(no_signal)` entries, into an append-only unprunable table for
zero decision-relevant value would itself be an audit-amplification hazard
(the same class security's own `SEC-4I-F8`/original-Phase-4 finding named
for `audit_log`). A future phase must not assume this table is populated.

## 9. Audit model

Every registry/admin write (`CreateJurisdiction`, `CreateLicence`,
`SetResolutionActive`) audits in the same transaction, including a required
`reason_code` (added by `SEC-4I-F5` after being found missing). Governing
principle, stated explicitly by security's final certification: record the
DECISION and REFERENCES to evidence, never the evidence VALUES. Confirmed
against the FINAL live schema (not migration text) that none of
`jurisdiction_resolutions`' 18 columns holds a prohibited value (no country/
residence/nationality/IP/geo value, no `person_key_hash`, no name/DOB/
document field, no vendor payload, no credential). The interim
`staff_supplied` audit-labeling mechanism (fixing `SEC-4I-F2`) was built,
then correctly removed once JV-2 made it moot (a converted surface has no
staff-typed jurisdiction left to label).

## 10. AssetAuthorization integration

Unchanged in contract — unconditional deny on an unresolved/refused
jurisdiction — confirmed by risk's Phase 3 report to be the CORRECT
specialization of the platform-wide invariant for a structurally
jurisdiction-keyed lookup (not one of the four original "incompatible
contracts," on reflection), not touched this stage beyond consuming the new
resolver's output correctly at every call site.

## 11. Risk integration

Risk's conditional fail-closed contract (`ErrMissingJurisdiction` fires only
when a jurisdiction-scoped rule exists for that tenant/operation) was
reviewed in full by risk's own Phase 3 design phase and RULED CORRECT and
retained unmodified — not a defect, a legitimate specialization, since
Risk's rules are per-tenant configurable and a tenant with zero
jurisdiction-scoped rules has legitimately decided jurisdiction is
irrelevant to their policy. An authoring-time precondition (R-2b:
`jurisdiction_resolution_active` table + accessor, preventing a
jurisdiction-scoped Risk rule from being created for a not-yet-active
tenant/operation pair) was specified and its schema/accessor built by
backend (`B-6`); the precondition CHECK itself remains correctly unbuilt,
routed to Risk's own future implementation phase (`DR-4I-RISK-01`, carried,
not blocking).

## 12. RG integration

Not touched this stage. `internal/rg` was confirmed untouched by every
phase's own diff review, and sportsbook's boundary review independently
confirmed zero commits touched it. `OpenBetSelfExclusionPolicy` remains
exactly as it was.

## 13. KYC interaction

KYC evidence (`kyc_documents.issuing_country`) was determined by
identity-compliance's Phase 2 review to be, at most, a narrow future
corroborating signal — never authoritative, never conflated with player
residence/nationality/jurisdiction. Confirmed by the final security
certification: **no code path anywhere in this stage reads
`kyc_documents.issuing_country`** — the resolver has exactly one basis
today (`tenant_licence`), with no player-evidence dimension at all. No
KYC-tier taxonomy was invented (explicitly forbidden by the directive).

## 14. Casino integration

K-3: casino's per-game jurisdiction blocklist fail-open defect was fully
remediated. The control is now "armed" only when a game's own blocklist is
genuinely non-empty (a correction architect made to risk's/security's
original literal recommendation, which would have denied 100% of casino
launches the moment resolution went live, since the resolver cannot yet
resolve any player). Within an armed game, an unresolved jurisdiction
denies; a genuinely blocked jurisdiction denies; both use a distinguishable
internal sentinel, never reusing the old `ErrJurisdictionBlocked`; one
resolution feeds all three consumers (blocklist check, Risk's request,
session snapshot); demo mode is explicit and jurisdiction-bearing by
default; both denial reasons collapse to a byte-identical player-facing
HTTP response (oracle prevention), while remaining internally distinct.
`SEC-4I-F3` (a hard prerequisite: the `casino_game.upserted` audit event
previously omitted the blocklist field entirely from its before/after
record) was fixed first, before K-3 shipped.

## 15. Bonus integration

JV-2: `jurisdiction_code` was removed as a client/staff-suppliable field
from all 4 structs across the 5 Bonus admin HTTP surfaces (manual-grant
issue, manual-grant activate, held-disposition-resolve, bulk-job execute —
one struct backs two endpoints), replaced with server-side resolution via
the shared `resolveGrantJurisdiction` helper feeding the existing
`GateCheckpoint`. The latent fail-open helper `resolveJurisdictionID`
(which collapsed "empty code" and "unknown code" to the same `uuid.Nil`,
letting an unknown jurisdiction silently match zero Risk rules and ALLOW)
was deleted outright, not wrapped, per architect's explicit instruction.
**Disclosed consequence, confirmed correct and NOT to be narrowed** by
architect's final certification: because `AssetAuthorization` denies
unconditionally on an unresolved jurisdiction (unlike casino's per-game-
armed blocklist), this correctly makes essentially all Bonus grant
activation/conversion/route-to-cash deny at the gate now, not merely the 5
admin surfaces — this is exactly what the canonical model's own §4.1
predicted ("Bonus stays blocked in Stage 4I"), not scope creep. To keep
~50 pre-existing tests meaningful without weakening any real gate, two
test-only seams were introduced (`SEC-4I-F6`'s shared four-eyes closure,
`SEC-4I-F7`'s injectable `activateGrantFunc`) — both independently verified
production-unreachable by a dedicated security pass, and both certified by
architect as sound, minimal, sanctioned platform conventions with a named
expiry trigger (re-examine every use once HDR-J-1/J-3 are answered).

## 16. Security/RLS

Every jurisdiction value is either (a) a scope declaration on a
configuration row (legitimately staff-authored) or (b) a resolved fact
about an operation (server-only, never client-suppliable) — the binding
JV-1/JV-2/JV-3 ruling, verified end-to-end by the final certification: the
field is deleted (not merely cross-validated) everywhere class (b) applied,
following this platform's own established `tenant_id` discipline. Both new
tables (`jurisdiction_resolutions`, `jurisdiction_resolution_active`) carry
`FORCE ROW LEVEL SECURITY`/`NOBYPASSRLS`, per-command policies (no `FOR
ALL`, no DELETE — `SEC-4I-F4`'s fix), and `BEFORE TRUNCATE` deny triggers on
both (`SEC-4I-F8`'s fix, closing a real, live, unaudited platform-wide
data-erasure vector `SEC-4I-F4` itself had missed). Two new RBAC
permissions (`PermJurisdictionRegistryManage`, platform-only;
`PermJurisdictionResolutionActiveWrite`, tenant-admin-only) were verified
correctly scoped by three independent passes and are now pinned by tests.
Adversarial RLS testing covered all 9 of security's own named scenarios
(cross-tenant, cross-brand, player-scope, forged jurisdiction/tenant/brand
payload, stale context, conflicting source, unavailable resolver) — 2 were
structurally inapplicable in Stage 4I (stale context, conflicting source —
no consumer persists-and-reuses a resolution yet; only one basis is
producible) and are named as forward triggers, not silently skipped. Final
verdict: **CERTIFIED WITH NAMED EXCEPTIONS** — `SEC-4I-F9` (blocklist
matching is case-sensitive against an unnormalized registry code space,
inert today), `SEC-4I-F10` (a future licence status transition must not be
added without a coupled resolver-query change, hard-gated), `SEC-4I-F11` (a
pre-existing, not-Stage-4I-introduced finding that `jurisdictions`/
`licences` carry no RLS at all on the tenant-write side — filed to
architect). None blocks Stage 4I.

## 17. Caching behavior

**No jurisdiction cache exists in Stage 4I — a deliberate decision (`CA-1`),
not a deferral.** Grounds: no cache infrastructure exists on this platform
today; adding one would recreate the exact H-3 hazard (network I/O between
an advisory lock and commit converting cache latency into cascading
fail-closed denials) the platform has already been burned by in this
domain's history; it would undo the availability property that makes
fail-closing casino's blocklist acceptable. If ever reversed, the binding
constraint is: a cache read only before the guarded transaction begins,
never a negative (`unresolved`/`refused`) result, never anything whose
invalidating write the platform cannot observe, and reversal requires its
own ADR plus a security review.

## 18. Concurrency

The resolver's read-only-on-the-evaluation-path constraint (risk's H-2,
confirmed mandatory by security, recorded by architect as a documentation
precondition in `docs/architecture/34-economic-operation-identity.md` §5.3
rather than a numbered "rule 0," per security's own reasoning) is enforced
structurally via the `ReadOnlyQuerier` interface (no `Exec` — a write does
not compile), not by convention. A genuine concurrency test (two goroutines,
casino's `LaunchGame` and Bonus's `ActivateGrant` running simultaneously for
the same tenant/player) was added by qa and passes repeatedly under `-race`
with no deadlock and correct independent denial on each side. **Named,
carried gap**: `DR-4I-QA-01` — the two remaining mechanisms from the
directive's own concurrency-testing requirement (a `BEGIN ... READ ONLY`
enforcement test, a `pg_locks` test catching a `FOR SHARE`/`FOR UPDATE`)
were not built; the structural compile-time mechanism holds regardless, but
nothing yet catches a future regression at the SQL-lock level.

## 19. API changes

Four new HTTP admin routes: `POST/GET /v1/admin/jurisdictions`,
`POST/GET /v1/admin/licences` (platform-admin-only, no tenant scope), and
`PUT/GET /v1/admin/jurisdiction-resolution-active[/{operationClass}]`
(tenant-admin-only). All four: authn via existing JWT middleware, authz via
the two new permissions, tenant-scoped where applicable (never
client-supplied), input validated via `DisallowUnknownFields`, audited in
the same transaction as the write. Five existing Bonus admin endpoints had
their `jurisdiction_code` request field removed entirely (a breaking
change to those request bodies — any client that submitted the field now
gets a 400, not a silent ignore). No Back Office UI was built anywhere,
per the directive's explicit constraint — API surfaces only.

## 20. Database/migrations

Three new migrations this stage: `0071_jurisdiction_resolution_foundation`
(the `jurisdiction_resolutions`, `jurisdiction_resolution_active`, and
`jurisdiction_precedence_configs` tables), `0072_jurisdiction_resolution_active_per_command_policies`
(fixing `SEC-4I-F4`'s `FOR ALL` policy defect), `0073` (fixing `SEC-4I-F8`'s
missing TRUNCATE-deny trigger). All three verified with a full down/up/down
round-trip against real Postgres 16 by qa's phase and re-confirmed by the
final security certification, with zero data loss on any unrelated table.

## 21. Tests and exact results

Full validation floor run repeatedly across all 13 phases, most recently
confirmed clean by the final security certification: `go build ./...`,
`go vet ./...` (including `-tags=integration`), `gofmt -l .` (empty every
time), full unit suite, full integration suite (`-tags=integration`, real
Postgres 16), full integration suite `-race`, migration round-trip
(`0071`-`0073`), RLS adversarial tests (all 9 named scenarios, 2 marked
structurally inapplicable with named triggers), authorization tests (both
new permissions, now pinned by dedicated tests), fail-closed tests (every
consumer, adversarially probed for a silent-allow path — none found),
concurrency tests (genuine two-goroutine cross-domain test, `-race`-clean),
audit tests (every write path, schema-verified against the prohibited-PII
list), Risk/RG/AssetAuthorization/Casino/Bonus integration tests. Zero
skipped financial/security tests without explicit written justification
(the one `t.Skip` found is the universal pre-existing `TEST_DATABASE_URL`
guard present in every integration test file in the repo). All 27 repo
packages green at final HEAD.

## 22. P0/P1/P2/P3 findings

- **P0: none open.**
- **P1: none open.** Twelve real defects found across the stage — two by
  the initial security spot-check (`SEC-4I-F4`/`F5`), one by a targeted
  test-seam verification (`SEC-4I-F6`), two by qa (a coverage-gap fix
  labeled `SEC-4I-F7`, plus two RLS/unavailable-resolver coverage gaps), one
  by architect's final pass (`DR-4I-ARCH-01`, closed) and one residual
  ruling closed (`DR-4I-BONUS-01`), and one by the final security
  certification (`SEC-4I-F8`) — every one fixed with a proven regression
  test before this report was written.
- **P2 (named, disclosed, gated, non-blocking)**: `SEC-4I-F9` (case-sensitive
  blocklist matching, inert today), `SEC-4I-F10` (licence-status not
  filtered, hard-gated against future change), `SEC-4I-F11` (pre-existing,
  not Stage-4I-introduced: `jurisdictions`/`licences` carry no RLS on the
  tenant-write side — filed to architect), `DR-4I-BE-01` (resolution table
  never actually written — deliberate), `DR-4I-BE-02` (no FK linking a
  consumer's own record to the resolution that governed it — a prerequisite
  named for whenever resolution moves outside a single transaction),
  `DR-4I-RISK-01` (R-2b's authoring-time precondition unbuilt),
  `DR-4I-QA-01` (two of three read-only-enforcement test mechanisms
  unbuilt), `DR-4I-QA-02` (one untested clamp branch in deposit-bonus
  reward computation, pre-existing), `DR-4I-BONUS-02` (a named expiry
  trigger for the JV-2 test seams, not itself a defect).
- **P3**: the pre-existing hand-copy pattern in `forceIssueAndActivateDepositBonusForTest`/
  `forceIssueAndActivateCashbackForTest` (predates Stage 4I, flagged by qa,
  ruled by architect as carried, not blocking, same fix pattern available
  whenever addressed).

## 23. Human Decision Register changes

**Six new candidate items formally opened**, recorded in
`docs/decisions/0041-human-decision-register-stage-4i-jurisdiction.md`
(companion to the pre-existing `0039`), following that document's own
established neutral, non-recommending discipline:

- **HDR-J-1** — may a missing player jurisdiction ever fall back to
  tenant/brand jurisdiction? (Blocks: Bonus deposit/cashback sweep
  issuance.)
- **HDR-J-2** — which signal is authoritative per operation class when more
  than one exists? (Mechanism already buildable/keyed; only the content is
  blocked.)
- **HDR-J-3** — should the platform collect a player residence/location/
  nationality attribute at all, and under what lawful basis/retention rule?
  **The single highest-leverage item** — until answered, every
  player-scoped resolution returns `unresolved(no_signal)`, which is the
  actual root cause of Bonus staying blocked, not a resolver implementation
  gap.
- **HDR-J-4** (narrowed) — only the record-of-authority question for
  regulator-facing purposes when jurisdiction changes mid-obligation; the
  enforcement question was resolved technically (Most-Restrictive-Outcome
  Composition, forward-binding, not built this stage) and needs no human
  input. Explicitly kept distinct from `0039`'s existing
  `OpenBetSelfExclusionPolicy` decision — verified by sportsbook's own
  boundary review that this separation holds in the final text, not just in
  intent.
- **HDR-J-5** — BYOL tenant jurisdiction question. Registered, explicitly
  marked not-urgent (no BYOL tenant exists yet).
- **HDR-J-6** — which markets may the first Anjouan-licensed B2C brand
  serve? Already implied by CLAUDE.md's own existing stop-and-ask language;
  now has a concrete technical hook (`licences.permitted_markets`, existing
  but empty/unread).

**No pre-existing Human Decision Register item was selected, narrowed, or
defaulted.** G-2, `OpenBetSelfExclusionPolicy`, mixed/bonus-funded
sportsbook cashout policy, FD-1, and Wave 3's Grant-cancellation-after-
conversion item all remain exactly as they were — confirmed independently
by sportsbook's dedicated boundary review (zero commits touched
`internal/rg`; the new HDR items reference FD-1/`OpenBetSelfExclusionPolicy`
only to explicitly disclaim overlap).

## 24. Deferred items

Everything named in §22's P2 list, plus: MROC (most-restrictive-outcome
composition) machinery — specified, explicitly NOT built this stage, per
architect's own "forward-binding, not Stage 4I" ruling; precedence table
content (blocked on HDR-J-2); any residence/location/nationality column
(blocked on HDR-J-3); `platform_fallback` basis (blocked on HDR-J-1);
`withdrawal_policies.jurisdiction_code`'s existing `CHECK (... IS NULL)`
constraint (explicitly not lifted, per Q-16's own "deferred explicitly, not
silently" ruling); payments' routing dimension 2 / country→jurisdiction
mapping (still an explicit `TODO(jurisdiction)`, correctly not conflated
with jurisdiction gating per payments' own Phase 9 determination); folding
`docs/security/security-architecture.md`'s new §J4I section's remaining
loose ends into other permanent docs where still owed. CRM, Affiliate,
Gamification, Reward-Orchestrator, real/in-house sportsbook, Retail,
Back Office/Partner Console/B2C frontend UI, and every real external vendor
(geolocation, KYC, AML, FX, PSP, casino) were correctly not implemented,
confirmed by sportsbook's explicit boundary review and every phase's own
scope discipline.

## 25. Git commit/branch/status

Branch `claude/focused-wright-jw88w9`. Working tree clean. All commits
pushed to `origin/claude/focused-wright-jw88w9`. Final commit before this
report: `f1f8d13`. Full commit sequence this stage (14 documentation/design
commits + 18 code/test commits, 32 total):
`2bab29f` (Phase 1 reconnaissance) → `c91fe40` (Phase 2 identity-compliance)
→ `61c020e` (Phase 3 risk) → `89af4c0` (Phase 4 security) → `22ac91a`
(Phase 5 architect synthesis) → `a7c6279` (HDR register, product-owner-proxy)
→ `f9fb1ff` (SEC-4I-F1) → `701518d`/`63f5eae`/`f960f41` (backend) →
`51b01ed`/`f3452bc` (SEC-4I-F4/F5) → `4790de0`/`41fe5b3` (casino SEC-4I-F3/K-3)
→ `1b07747` (bonus JV-2/B-11) → `4b18076` (SEC-4I-F6) → `998c5d5` (payments
C-3(d)) → (sportsbook, clean, no commit) → `12acccb`..`ef6a941` (qa,
4 commits) → `fb632d7`/`c481e2a`/`72567e7`/`854ec79`/`10859e4` (architect
final, 5 commits) → `d24331f`/`c053b07`/`ee3908a`/`3cfb488`/`f1f8d13`
(security final, 5 commits).

## 26. Overall verdict

**PARTIALLY IMPLEMENTED, per CLAUDE.md's no-fake-completion discipline.**
Stage 4I's own authorized scope — a canonical, provider-neutral,
fail-closed jurisdiction resolution FOUNDATION, reusable by every domain,
never a per-domain reimplementation — is complete, tested, and
independently certified. It deliberately does not, and could not honestly
claim to, resolve any player's actual jurisdiction: that capability is
gated entirely on human decisions (chiefly HDR-J-3) this stage correctly
declined to make. Every phase's work was independently reviewed by the
next; every genuine defect found was fixed and proven with a regression
test; no control was weakened to manufacture a pass; no Human Decision
Register item was touched; every remaining gap is named, owned, and either
fail-closed-and-inert or explicitly scheduled with a trigger.

## 27. Per this stage's own governing instruction

**Do not proceed automatically to another stage. STOP after this report and
await explicit human authorization.**
