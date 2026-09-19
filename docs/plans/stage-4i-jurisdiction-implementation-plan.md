# Stage 4I — Jurisdiction Implementation Plan (Post-Human-Decision)

**Status of this document: PLANNING ONLY.** Nothing in this document
authorizes, constitutes, or performs an implementation change. No code,
migration, schema, API, or configuration has been created or modified in
producing it. Per CLAUDE.md's no-fake-completion rule this entire document
is labelled **PLANNED — NOT IMPLEMENTED**, in full, without exception.

**Trigger.** `docs/decisions/0042-human-decision-response.md` now carries
verbatim human answers for all 11 previously-open decisions (HDR-J-1
through HDR-J-6, including HDR-J-3's 8 sub-items; G-2;
`OpenBetSelfExclusionPolicy`; the mixed/bonus-funded sportsbook cashout
policy plus FD-1; and the converted-Grant-cancellation/receivable
question). This document traces each of those recorded answers into
concrete technical consequences against the binding specification in
`docs/governance/stage-4i-canonical-model.md` (**CM** below) and the
verified `PARTIALLY IMPLEMENTED` code state CM §13 and this document's own
§1 record.

**Authorship.** Written directly by the orchestrator (this session), not
dispatched to a specialist, because it is synthesis against material
already read in full this session (CM in full including §13, `0042` in
full, `docs/governance/task-registry.md`'s `DR-4I-*` rows, and direct
source verification of `internal/jurisdiction`, `internal/casino`,
`internal/bonus`). §15 recommends the specialist review pass this document
itself should receive before any implementation dispatch begins.

**What this document is not.** It is not an implementation dispatch. It
authorizes no specialist to begin work. It does not itself constitute or
imply an "implementation-authorized" gate — CLAUDE.md's per-item
legal/compliance dependencies (§9 below) remain outstanding, and several
phases in §14's sequence are explicitly marked as requiring a further,
separate human authorization even after this plan is approved, per
CLAUDE.md's "stop and ask" rule for jurisdiction-selection and
production-launch matters.

---

## 1. Current-state assessment (verified against the repository)

### 1.1 What exists and is real (IMPLEMENTED, per CM §13.1)

| Artefact | File(s) | State |
|---|---|---|
| `internal/jurisdiction` package: `Resolve`, non-forgeable `Resolution` (unexported fields, `Code()`/`ID()` unreachable off `resolved`), 3-outcome (`resolved`/`unresolved`/`refused`) + diagnostic `Reason` axis, closed `Basis` enum, `ReadOnlyQuerier` (Query/QueryRow only — no `Exec`), `AssertScope`, `Persist` | `internal/jurisdiction/types.go`, `resolver.go`, `persist.go`, `registry_admin.go`, `resolution_active.go` | Real, tested (`resolver_test.go`, `resolver_unavailable_test.go`, `jurisdiction_integration_test.go`) |
| `jurisdiction_resolutions` (append-only, `FORCE RLS`, no UPDATE/DELETE policy, `BEFORE UPDATE OR DELETE` + `BEFORE TRUNCATE` deny triggers, two `CHECK`s: `(jurisdiction_code IS NOT NULL) = (outcome='resolved')` and `player_account_id IS NULL OR selected_basis <> 'tenant_licence'`) | `migrations/0071_jurisdiction_resolution_foundation.up.sql` | Real, empty in production (§1.3) |
| `jurisdiction_resolution_active` (Risk's R-2b precondition fact table), per-command policies, no DELETE | `migrations/0071...up.sql`, `0072_jurisdiction_resolution_active_per_command_policies.up.sql`, `0073_jurisdiction_resolution_active_deny_truncate.up.sql` | Real, zero production writers (§1.3) |
| `jurisdiction_precedence_configs` — table **shape** only, keyed `(tenant licensing jurisdiction, operation_class)` | `migrations/0071...up.sql` | Real shape, **zero rows**, no writer surface |
| `jurisdictions` / `licences` registry write surface + platform-only permission | `internal/jurisdiction/registry_admin.go`, `internal/auth/permission.go` | Real |
| `tenant_licence` basis: reads `tenants.licence_id → licences.jurisdiction_id`, DB `CHECK`-bounded to non-player-scoped resolutions, resolver-side `assertTenantScope` (the `DR-4I-ARCH-01` fix) | `internal/jurisdiction/resolver.go:106-172` | Real, but **structurally unreachable in production today** (§1.3) |
| K-3 casino remediation: per-game armed blocklist, `DenialCodeJurisdictionUnresolved` / `DenialCodeJurisdictionBlocked` distinct internally, byte-identical player-facing collapse, one resolution feeding blocklist + Risk + session snapshot, explicit demo-mode ruling | `internal/casino/orchestrator.go`, `internal/httpserver/casino_handlers.go`, `internal/httpserver/casino_admin_handlers.go` | Real, tested |
| JV-2: `jurisdiction_code` removed from 4 structs / 5 Bonus admin HTTP surfaces; `resolveJurisdictionID` deleted (not wrapped); `resolveGrantJurisdiction` added | `internal/bonus/eligibility.go`, `internal/httpserver/bonus_handlers.go`, `bonus_domain_ops_handlers.go` | Real, tested |
| `SEC-4I-F1`–`F8` | Various | **Closed** |
| Test-seam convention (`activateGrantFunc`, `resolveHeldDispositionAction(skipAssetAuthorization bool)`, `applyGrantActivation`/`applyGrantConversion`) | `internal/bonus/*_test.go`, `internal/bonus/lifecycle.go`, `conversion.go`, `held_disposition_ops.go` | Real, certified sound, expiry tracked as `DR-4I-BONUS-02` |

### 1.2 What does NOT exist — the actual gap this plan closes

No player-side jurisdiction signal of any kind exists in the schema or
code. Specifically absent, confirmed by direct repository inspection, not
inferred:

- No `player_accounts.declared_residence_country` column.
- No `kyc_verifications.verified_residence_country` column.
- No geolocation/`geo_signal` producer of any kind — no vendor interface,
  no HTTP surface, no session field.
- No `jurisdiction_precedence_configs` rows, and no admin surface to write
  one.
- No admin/provisioning surface anywhere in `internal/httpserver` writes
  `tenants.licence_id`. Grep confirms the column has exactly one production
  reader (`internal/jurisdiction/resolver.go:147`) and zero production
  writers. **This is the sharpest current-state finding of this
  assessment**: even the one basis Stage 4I built (`tenant_licence`) cannot
  produce a `resolved` outcome for any real tenant today, because nothing
  ever populates the column it reads. Every tenant row's `licence_id` is
  either NULL (from creation) or has never been touched. This is a gap
  this plan's Phase A must close before `tenant_licence` can resolve
  anything for a real tenant, independent of every HDR-J-3 question.
- `licences.permitted_markets` (HDR-J-6's target column, per `0042`'s
  register text: "a database field already exists to hold the answer; it
  is currently empty") carries no validation consumer — nothing reads it
  to gate an operation.
- `jurisdiction.Persist` has zero production call sites (`DR-4I-BE-01`).
  No consumer domain writes a `jurisdiction_resolutions` row today; the
  table is real but empty in production.
- No `jurisdiction_resolution_id` FK exists yet on `casino_launch_sessions`
  or `bonus_grants` (`DR-4I-BE-02`; AR-2's linkage is unbuilt).
- `risk.CreateRule`'s R-2b authoring-time precondition is unbuilt
  (`DR-4I-RISK-01`); `jurisdiction.IsActive` has zero production callers.
- No `READ ONLY` transaction test or `pg_locks` test exists for H-2's
  read-only-resolver constraint (`DR-4I-QA-01`); only the compile-time
  `ReadOnlyQuerier` mechanism is in force.
- G-2's three configurable settlement-credit treatments: none built.
  `grant_cancel_completed`'s posting shape exists only for `completed`
  Grants (`docs/architecture/ledger-accounting-model.md` §7.19.3); nothing
  exists for routing a late credit to `player_cash` or to a manual-review
  queue, and no brand-level policy configuration surface exists for this
  or any other bonus settlement policy.
- No brand-level policy-configuration mechanism of the general shape G-2
  requires (a versioned, four-eyes-gated, per-brand-selectable enum) exists
  anywhere in the codebase today; the closest precedent is
  `bonus_approval_policies`, which is tenant-scoped, not brand-scoped, and
  governs approval thresholds, not settlement treatment.
- No manual-review-and-approval workflow of the shape G-2(c) needs exists
  (a held-item queue with a per-item human disposition is closest to
  Bonus's existing `held_disposition` mechanism, but that mechanism
  disposes of *forfeiture/hold* states, not *late-settlement-credit*
  routing, and is not wired to it).
- `OpenBetSelfExclusionPolicy` has both named values already fully built
  and ready to use as a default (per `0039`'s technical-consequences note,
  re-confirmed unchanged this session) — this is a **configuration
  selection**, not new code, and is the one Part-2 decision in `0042` with
  zero implementation gap.
- No sportsbook cashout feature exists at all in the current codebase (no
  early-cashout endpoint, no partial-settlement-into-cashout posting path)
  — confirmed by the existing architecture record's framing of the
  mixed-funding cashout question as forward-looking ("if sportsbook
  betting ever gains an early 'cash out' feature"). FD-1 and the
  proceeds-split policy are therefore currently **inactive by
  non-existence of the feature they would gate**, not merely by
  configuration.
- No code path anywhere permits cancelling a `converted` Grant; §7.19.4
  deliberately specifies no posting shape for it. Nothing to change here
  under the recorded "do not permit" answer — this decision requires **no
  new code**, only a documentation/registry closure (§7 below).

### 1.3 The single sentence that governs this whole plan

**Every player-scoped jurisdiction resolution in production today returns
`unresolved(no_signal)`, and will continue to, until Phase A/B below lands
at minimum one producible player-side or tenant-side basis with a real
write path feeding it.** Nothing in `0042`'s answers changes this sentence
by itself — every answer recorded is a *policy* decision; none of them is,
by itself, a column, a write path, or a producer. This plan exists
precisely to specify the gap between "policy decided" and "basis
producible."

---

## 2. Decision → technical consequence: full trace

Each of the 11 `0042` items, traced to what it actually authorizes to be
built, distinguished sharply from what remains additionally blocked by an
outstanding legal/compliance dependency even after this plan's
implementation phases land.

### 2.1 HDR-J-1 — "No fallback, ever."

**Consequence: none.** This is the only one of the eleven answers that
authorizes **zero new code**. It confirms the current, structurally
enforced state (`platform_fallback` remains a reserved, resolver-
unproducible `Basis` value; the DB `CHECK` preventing `tenant_licence` on a
player-scoped row stays as-is). The only action item is **documentation**:
CM's own text already states this is the current behaviour; this plan's
§13 records the decision as closing, not opening, an implementation
question. No phase in §14 does anything for HDR-J-1.

### 2.2 HDR-J-2 — Operation-specific precedence content

**Consequence: `jurisdiction_precedence_configs` rows become writable in
principle, but the row *content* the human supplied is a policy statement
("verified > declared for KYC/reporting; location is a real-time
market-access overlay, never a residence substitute; most-restrictive-
applicable wins on conflict; historical reporting is frozen at
event-time") — not a machine-readable precedence table. Translating this
into rows requires:

1. A concrete `operation_class`-keyed schema addition to
   `jurisdiction_precedence_configs` (already shaped in `0071`) capturing,
   per `(tenant licensing jurisdiction, operation_class)`: an **ordered
   basis-preference list** and a **most-restrictive-applicable
   tie-break flag** (never an arbitrary pick — CM §2.2's `reason =
   irreconcilable_bases` remains the outcome when the ordered list itself
   does not resolve a tie, which the human's answer explicitly permits:
   "where multiple applicable signals impose different restrictions, the
   more restrictive applicable result should prevail" is itself an
   evaluable rule, not an escape from `irreconcilable_bases` — see §5.4).
2. A **per-operation-class confidence threshold** distinguishing
   enforcement-grade operations (declared residence insufficient) from
   non-enforcement ones (declared residence sufficient) — this is CM
   §3.3's existing mechanism; §2.2's answer does not change *where* the
   threshold lives, only supplies enough content to populate it for the
   four seeded operation classes.
3. **This is still blocked**, not merely "content to be typed in":
   populating real rows requires the legal/compliance validation the
   human's own `NOTES/CONDITIONS` demands ("subject to legal/compliance
   validation for each operating jurisdiction"). §9.2 records this as an
   outstanding legal dependency independent of the engineering work.

**What is buildable now, without further legal review:** the schema
extension to `jurisdiction_precedence_configs` (the ordered-list and
tie-break-flag columns) and the versioning/four-eyes write-path mechanism
around it. **What remains blocked:** the actual row content for any real
jurisdiction pair, which needs per-jurisdiction legal sign-off before it
can be activated (not merely authored).

### 2.3 HDR-J-3 (a–h) — the practical root; traced sub-item by sub-item

This is the item that actually unblocks a producible player-side basis.
Each sub-answer maps to a specific schema/code item; **none may be built
ahead of §9.2's stated legal/privacy dependency actually landing**,
because the human's own answer for 3e conditions collection itself on a
validated lawful basis, not merely on the "yes" to 3a/3b/3c.

| Sub-item | Answer | Technical consequence | Gating note |
|---|---|---|---|
| **3a** location | Yes — market-access/access-control only, never persisted as residence | New `geo_signal` basis producer: a point-in-time signal captured and referenced (`evidence_ref`) on a `jurisdiction_resolutions` row, **never** a column on `persons`/`player_accounts`. Requires a location vendor/provider interface (RECON Q-7, still unselected — see §9.4) | Vendor selection is itself a new dependency this plan does not choose (§12) |
| **3b** declared residence | Yes — on brand-specific `player_accounts`, unverified | New column: `player_accounts.declared_residence_country` + `declared_residence_captured_at`. New `player_declared_residence` basis producer, capped at `insufficient_confidence` for enforcement-grade operation classes per CM §3.3 | Buildable once 3e's lawful basis is validated |
| **3c** verified residence | Yes — via approved KYC process only, never auto-inferred | New column: `kyc_verifications.verified_residence_country` + `source_document_id`, written only by an explicit reviewer action (no automatic derivation from `kyc_documents.issuing_country`, per RULING BI-4I-2, unchanged). New `player_verified_residence` basis producer | Requires an identity-compliance-owned reviewer UI/workflow addition; buildable once 3e is validated |
| **3d** nationality | No | **No schema, no code.** Explicitly not authorized. Any future work needs both a fresh HDR item and a demonstrated operational need | N/A |
| **3e** lawful basis | Documented lawful basis required, validated by legal/privacy counsel before production use | **Gates 3a/3b/3c's activation, not merely their authoring.** The schema/code for 3a-3c may be built and tested against synthetic data (CLAUDE.md's environment-safety rule already requires this), but **must not be enabled against real player data** until legal/privacy counsel validates the basis per jurisdiction and updates `docs/architecture/16-privacy.md` accordingly | **Hard legal gate, tracked in §9.2 and §13's Traceability Matrix** |
| **3f** persistence/retention | By data type/purpose/jurisdiction, not one universal period | New retention-scheduling mechanism: per-column, per-jurisdiction TTL/erasure job, keyed off `docs/architecture/16-privacy.md`'s (to-be-updated) classification. Distinct from — and additional to — the append-only `jurisdiction_resolutions` audit trail, which never holds the raw value (CM §5.3) and is therefore not itself subject to a residence-specific retention rule | Final periods require legal/privacy validation before the retention job's parameters can be set for any real jurisdiction |
| **3g** audit | Existing audit model sufficient as baseline; staff corrections need actor/subject/reason/timestamp | **No new audit *mechanism*.** New requirement: a staff-correction workflow (for 3b/3c edits by an operator) writing a `jurisdiction_fact.corrected` `audit_log` entry (CM §5.1 already names this event class) with actor/subject/reason/timestamp — this event class exists in the audit taxonomy today with **zero emitters**, since nothing yet writes 3b/3c facts to correct | Buildable alongside 3b/3c |
| **3h** KYC sourcing | Issuing country is corroborating evidence only, never auto-populating | Activates the existing, currently-inert `kyc_corroboration` basis value (CM §3.1, RULING BI-4I-2) as a genuine input **once 3c exists** — corroboration compares an independently captured verified-residence fact against `kyc_documents.issuing_country`; it has nothing to corroborate until 3c lands. Requires an identity-compliance-owned permitted-document-type/evidentiary-weight policy (human's own `NOTES/CONDITIONS`), itself a further compliance-owned specification, not engineering | Blocked on 3c landing first, and on identity-compliance's document-type policy |

**Net technical effect of HDR-J-3 as a whole:** three new player-scoped
resolver basis producers become buildable (`geo_signal`,
`player_declared_residence`, `player_verified_residence`), each gated
independently on its own legal/vendor dependency, and each capped by CM
§3.3's existing confidence-threshold mechanism (no new mechanism needed —
the mechanism was already built in Stage 4I; only its inputs were always
missing). This is the item that, once its legal gate clears, converts
Bonus and casino-blocklist denials from "always, because no signal exists"
to "correctly conditional on the player's actual jurisdiction."

### 2.4 HDR-J-4 — Record-of-authority (narrowed to the reporting half)

**Consequence:** the enforcement half is already adjudicated and requires
no new decision-tracing (CM §7, MROC). What the human's answer newly
authorizes: a **reporting-view mechanism** that, given an obligation's
`jurisdiction_resolutions` row(s), determines which is authoritative for a
regulator-facing export — "the determination applicable at the time the
obligation arose," with the later one retained "as contextual evidence."
Concretely:

- No change to how a resolution is recorded (AR-2's per-checkpoint
  resolution reference already achieves "detectable" per CM §7.5).
- A new **reporting/query construct** (not a schema change — the data is
  already there once AR-2's FK lands) that, given an
  operation's originating resolution and any later resolution for the same
  subject, selects the originating one as "authoritative-for-reporting"
  and surfaces the later one as "contextual" in the same export record.
  This is a **data-analytics / reporting-domain** consumer of the ledger
  that does not exist yet and has no consumer today because no divergent
  resolution has ever been recorded (there being no player-side signal to
  diverge).
- **MROC itself is not built** — HDR-J-4's answer does not change CM §7.4's
  ruling; MROC remains forward-binding, not a Stage-4I/this-plan
  deliverable, because it activates only when two candidate jurisdictions
  can genuinely arise for one operation, which requires HDR-J-3 to be live
  first.

### 2.5 HDR-J-5 — BYOL tenant jurisdiction authority (not urgent)

**Consequence: none required now.** The human's answer confirms CM's
existing non-foreclosure design (`tenant_asserted` remains a reserved,
resolver-unproducible basis). No schema or code change is authorized by
this answer alone; it is recorded so a future BYOL-onboarding phase does
not have to re-litigate it. §14 places no phase against this item.

### 2.6 HDR-J-6 — Permitted markets for the first B2C brand (not yet determined)

**Consequence: still blocked**, but the human's answer clarifies the
*mechanism* even though the *content* (the actual list) is not yet
available: "once approved, the permitted-market list must be explicit and
enumerable [and] any jurisdiction not on the approved list must fail
closed." This authorizes building, ahead of the list itself:

- A new **market-authorization check** consuming `licences.permitted_markets`
  (the existing, empty column) against a `resolved` player-jurisdiction —
  distinct in its own reason code from a Risk limit breach or a casino
  blocklist hit, per CM §10.2's own binding constraint on any J-6 answer.
- This check can be built and unit-tested against a synthetic
  `permitted_markets` list, but **must not be activated for any real
  tenant/brand** until the jurisdiction-by-jurisdiction legal/compliance
  review actually populates the list — the human's answer is explicit that
  "no country is permitted by default." Its fail-closed default (empty
  list ⇒ deny every market) is a property this plan requires
  the implementation to have from day one, verifiable independent of legal
  review.

### 2.7 G-2 — Terminal-Grant settlement-credit resolution

**Consequence:** the largest single implementation item this plan
identifies outside HDR-J-3, and explicitly **not implemented now** per the
human's own `NOTES/CONDITIONS` ("this is a decision/configuration
requirement only at this stage"). What it authorizes to be *planned* (not
built) in this document:

1. A new **brand-level policy configuration row** —
   `bonus_settlement_credit_policies (brand_id, treatment, effective_from,
   ...)` — selecting one of `ACTION_REFORFEIT` / `ACTION_ROUTE_TO_CASH` /
   `ACTION_HOLD_FOR_REVIEW`, versioned, four-eyes-gated on change, default
   `ACTION_ROUTE_TO_CASH`, activatable only before a brand goes live (the
   human's "configurable at brand level before the brand is activated"
   condition — this plan reads that as: the policy must be *set* no later
   than brand activation, not that it can never subsequently change, since
   the human's own "changes... must be subject to four-eyes controls"
   clause presumes post-activation change is possible under control).
2. A new **cross-system check**: settlement-posting logic (today entirely
   outside Bonus, in the casino/sportsbook settlement path) must consult a
   Grant's status before deciding where a late credit lands — this does
   not exist today in any form and is Bonus's and the settlement domain's
   joint new integration point.
3. `ACTION_HOLD_FOR_REVIEW`'s manual-review-and-approval workflow, which
   does not exist in any form today (Bonus's existing `held_disposition`
   mechanism handles a different state class and is not a ready-made
   substitute — confirmed by direct code reading this session; extending
   it or building a parallel workflow is an open design question for the
   phase that eventually builds this, not resolved here).
4. **This plan does not schedule G-2's implementation into any phase
   below.** It is recorded as an approved policy target with a named
   default, entirely separate from HDR-J-3, and — being independent of
   every jurisdiction basis producer — could be scheduled by the
   orchestrator as its own, jurisdiction-unrelated implementation dispatch
   whenever prioritized. It is included in this document only because
   `0042` bundled it with the jurisdiction decisions; it is not itself a
   jurisdiction feature.

### 2.8 `OpenBetSelfExclusionPolicy` — `VOID_ON_SELF_EXCLUSION`

**Consequence: a configuration change only**, per `0039`'s own recorded
technical-consequence note (both values are "already fully built and ready
to use as the default"). This plan records it as: set the platform-wide
default configuration value to `VOID_ON_SELF_EXCLUSION` wherever that
default is currently unset, with no new code. Not scheduled as an
engineering phase; flagged in §14 as a **zero-cost configuration item**
that can land independently of every other phase, at any time, with no
sequencing dependency.

### 2.9 Mixed/bonus-funded sportsbook cashout — "Not cashout-eligible"

**Consequence: none, currently**, because no cashout feature exists to
gate (§1.2). This answer is recorded as **binding policy for whenever
sportsbook cashout is eventually built**, not as an implementation item
for this plan: the eventual cashout feature's own design (a future,
separate dispatch, sportsbook-owned) must implement an eligibility check
excluding any bet funded entirely or partially by bonus value, per the
recorded answer. Not scheduled in §14.

### 2.10 FD-1 — "Nullifying" (currently inactive)

**Consequence:** identical in kind to §2.9 — inactive until cashout
exists, and then binding as "nullify wagering progress on cashout of a
bonus-funded stake" unless separately replaced. Not scheduled in §14.

### 2.11 Converted-Grant cancellation → customer receivable — "Do not permit"

**Consequence: none.** No code exists to cancel a converted Grant today,
and none is authorized. This answer closes the open question by
*confirming absence is correct*, not by requesting a build. The only
action item is a **documentation closure**: `docs/architecture/
ledger-accounting-model.md` §7.19.4 should be updated (by `ledger-finance`,
not by this plan) to record that the escalation has been answered
("no automatic cancellation/receivable; any future exceptional reversal
requires a separately approved finance/legal process") rather than left
as an open escalation. This is a documentation-only follow-up, not part of
§14's phases, and does not touch code.

---

## 3. Cross-domain impact classification (24 domains)

Legend: **NC** = No change · **ECC** = Existing code change · **NCo** =
New component · **NCfg** = New configuration · **NMig** = New migration ·
**NTC** = New test coverage · **LCD** = Legal/compliance dependency.

| # | Domain | Classification(s) | What, precisely |
|---|---|---|---|
| 1 | Jurisdiction Resolver (`internal/jurisdiction`) | ECC, NMig, NTC, LCD | New basis producers (`geo_signal`, `player_declared_residence`, `player_verified_residence`); precedence-config consumption; confidence-threshold wiring already present, now exercised |
| 2 | Identity / Player Account | NMig, ECC, NTC, LCD | `player_accounts.declared_residence_country` + captured_at; brand-account placement per IC §1, unchanged |
| 3 | KYC | NMig, ECC, NTC, LCD | `kyc_verifications.verified_residence_country` + `source_document_id`; reviewer-action-only write path; corroboration policy (identity-compliance-owned) |
| 4 | Risk | ECC, NTC | B-7/R-2b's `CreateRule` precondition (`DR-4I-RISK-01`); no change to `Evaluate`'s contract |
| 5 | RG (Responsible Gaming) | NCfg | `OpenBetSelfExclusionPolicy` default value set to `VOID_ON_SELF_EXCLUSION`; no code change |
| 6 | AssetAuthorization | NC | CM §4.1 unchanged; benefits automatically once a resolution can be `resolved` |
| 7 | Casino Gateway | NC (for K-3 mechanism) / ECC (AR-2 FK) | K-3 remediation already landed and unaffected; `DR-4I-BE-02`'s `jurisdiction_resolution_id` FK is new work, scheduled Phase D |
| 8 | Sportsbook | NC now; LCD/NCo later | Cashout-eligibility and FD-1 checks are future, unscheduled work gated on a cashout feature that does not exist |
| 9 | Bonus Engine | ECC, NCfg, NCo (G-2 only), LCD | `resolveGrantJurisdiction` benefits automatically once a basis resolves; G-2's settlement-credit policy is a separate, unscheduled new component (§2.7) |
| 10 | Reward / Gamification | NC | No jurisdiction dependency identified in this stage's scope |
| 11 | Wallet / Ledger | NC | No posting-shape change from any of the 11 decisions; G-2 (if separately scheduled) would touch settlement-posting, not the ledger's own invariants |
| 12 | Payments | NC | CM §4.5/§13.7 unchanged; explicitly out of this plan's scope (payments' own jurisdiction phase has not run) |
| 13 | Tenant Config | NCo, NMig | Phase A's tenant-licence assignment surface (§4); precedence-config admin surface (§2.2) |
| 14 | Brand Config | NCfg, NCo (if G-2 scheduled) | G-2's brand-level policy row, if/when separately scheduled |
| 15 | Retail | NC | `retail_node` basis remains reserved/unproducible; retail is out of scope |
| 16 | Audit | ECC, NTC | New `jurisdiction_fact.corrected` emitter (3g); `jurisdiction_resolution_active.changed` emitter already specified, still zero production writers until B-7/precedence work lands |
| 17 | RBAC | NCfg | One new tenant-scoped write permission for declared-residence capture (player-self-service) and one reviewer-scoped permission for verified-residence write, following `PermAssetAuthorizationWrite`'s precedent; no new role |
| 18 | Reporting / BI | NCo | HDR-J-4's reporting-authoritative-jurisdiction view (§2.4); no consumer exists today |
| 19 | Event Bus | NC | No new event type identified; resolution records are read via direct query, not event-sourced, consistent with CM's design |
| 20 | API / OpenAPI | ECC | New fields on player-account self-service endpoints (declared residence) and KYC reviewer endpoints (verified residence); new market-authorization denial reason code (HDR-J-6) |
| 21 | Database / RLS | NMig, NTC | New columns inherit `player_accounts`'/`kyc_verifications`' existing RLS; no new RLS *model* needed — existing tenant/player scoping already covers these tables |
| 22 | Security | NTC, LCD | Adversarial test extension for the three new basis producers (each is a new "Layer 1" input surface per CM §4.4); `security` review mandatory before any 3a/3b/3c code activates against real data |
| 23 | Observability | NCfg | Metric/alert on `jurisdiction.resolver_unavailable` volume once a real basis exists and dependency failure becomes a live possibility (today it cannot occur, since the resolver has no external dependency — CM §9.4) |
| 24 | Operations / Back Office | NCo | A reviewer-facing verified-residence capture UI (KYC domain); a tenant-licence-assignment admin screen (Phase A); a market-authorization-list admin screen (HDR-J-6, blocked on content) |

---

## 4. Phase A prerequisite, stated precisely (the gap §1.2 identified)

Before any HDR-J-3 work is useful, **Phase A must close the
`tenants.licence_id` write-path gap**, because it blocks the one basis
Stage 4I already built, independent of every privacy/legal question:

- **New admin surface**: a platform-admin-only endpoint (or extension of
  tenant provisioning) to set `tenants.licence_id`, following
  `PermCasinoCatalogueManage`'s exact RBAC precedent (CM §6.1) —
  platform-scoped, `RolePlatformAdmin`, audited.
- **No migration required** — the column already exists (RECON's own
  finding, re-confirmed: `tenants.licence_id` exists, unwritten).
- **This item has no legal/compliance dependency of its own** for the
  *mechanism*; assigning an actual licence to the actual production tenant
  is an operational/business action requiring the human to confirm which
  licence row corresponds to the Anjouan licence already referenced
  throughout CLAUDE.md and the Blueprint — a one-time data-entry
  confirmation, not a new legal question. This plan does **not** assume
  which value to write; §14 Phase A schedules only the admin surface, not
  the data entry itself.
- Until this lands, `tenant_licence`-basis resolutions remain
  `unresolved(no_signal)` for every tenant, exactly as they are today, and
  every downstream item in this plan that assumed `tenant_licence`
  eventually works (the precedence-config keying in §2.2, HDR-J-6's
  market check) is itself blocked transitively.

---

## 5. Proposed data model (planning sketch — not a migration)

None of the following is a migration. Each is a **proposed** shape for
`backend`'s eventual authoring, subject to `architect` review before any
migration is written (per CM's own ownership split, §2.1).

### 5.1 `player_accounts` additions (HDR-J-3b)

```
declared_residence_country   TEXT NULL   -- ISO-3166 alpha-2, explicit FK/lookup to a country registry, never to jurisdictions.code directly (CM §2.4's collision trap applies identically here)
declared_residence_captured_at TIMESTAMPTZ NULL
```
Access: its own read permission, not implied by `player:read` (per HDR-J-3
sub-item 3f's binding constraint and CM §10.2's J-3 constraint).

### 5.2 `kyc_verifications` additions (HDR-J-3c)

```
verified_residence_country   TEXT NULL   -- same ISO-3166 discipline as 5.1
verified_residence_source_document_id UUID NULL REFERENCES kyc_documents(id)
verified_residence_set_by    UUID NULL REFERENCES staff_users(id)   -- explicit reviewer, never auto-derived
verified_residence_set_at    TIMESTAMPTZ NULL
```

### 5.3 `jurisdiction_precedence_configs` extension (HDR-J-2)

The `0071` shape is `(tenant licensing jurisdiction, operation_class)`-keyed
with no content columns yet defined beyond the key. Proposed addition:

```
basis_preference_order   basis[]  NOT NULL  -- ordered, closed-enum array
tie_break                TEXT NOT NULL DEFAULT 'most_restrictive_applicable'  -- closed enum; no other value in Stage-4I-successor scope
policy_version            TEXT NOT NULL
effective_from            TIMESTAMPTZ NOT NULL
```
Append-only / versioned, following `risk_rules`' own precedent (never
UPDATE a live row; a new effective-dated row supersedes).

### 5.4 New audit_log emitter (HDR-J-3g)

No schema change — `audit_log`'s existing shape and `jurisdiction_fact.
corrected` event-class name (already reserved by CM §5.1) suffice. This
is a **code** item (a new call site), not a migration.

### 5.5 Market-authorization check (HDR-J-6) — no new table

`licences.permitted_markets` already exists as a column (per `0039`/`0042`'s
own text: "a database field already exists to hold the answer"). No
migration proposed; only a new **consumer** of the existing column.

### 5.6 G-2's proposed shape (recorded for completeness; NOT scheduled)

```
bonus_settlement_credit_policies (
  brand_id       UUID NOT NULL,
  treatment      TEXT NOT NULL CHECK (treatment IN
                   ('reforfeit','route_to_cash','hold_for_review')),
  effective_from TIMESTAMPTZ NOT NULL,
  set_by         UUID NOT NULL REFERENCES staff_users(id),
  ...
)
```
Recorded here only for traceability; §14 does not schedule its migration.

---

## 6. Proposed API / OpenAPI changes (planning sketch)

| Endpoint area | Change | Gated on |
|---|---|---|
| Player self-service (`PATCH /player/account` or equivalent) | New optional field `declared_residence_country` | HDR-J-3b + 3e legal validation |
| KYC reviewer surface | New reviewer action setting `verified_residence_country` on a `kyc_verifications` row, distinct from document upload | HDR-J-3c + 3e legal validation |
| Casino launch / Bonus grant denial responses | New, distinguishable-internally-only reason code for a market-authorization failure (HDR-J-6), collapsed player-facing per the oracle rule (CM §6.3) generalized to this new gate | HDR-J-6 content |
| Tenant admin (platform-scoped) | New endpoint or field to set `tenants.licence_id` | None (Phase A, no legal gate on the mechanism) |
| Jurisdiction precedence admin (tenant-scoped, `RoleTenantAdmin`) | New endpoint to author `jurisdiction_precedence_configs` rows | HDR-J-2 content (legal validation) before activation; mechanism buildable before |
| Licence/market admin (platform-scoped) | New endpoint to author `licences.permitted_markets` | HDR-J-6 content before activation; mechanism buildable before |

No existing endpoint's request/response shape is proposed to change in an
incompatible way. `DisallowUnknownFields`'s existing discipline (CM §4.4
Layer 1) continues to govern every new field: it is additive-only, and any
of these fields appearing in a request to an endpoint that does not
declare it is a 400, not silently ignored.

---

## 7. Configuration model

- `jurisdiction_precedence_configs` rows are configuration, not code, and
  inherit the same versioned/four-eyes/audited discipline `risk_rules`
  already has (CM §10.2's own binding constraint on any J-2 answer).
- `licences.permitted_markets` is configuration, platform-scoped, written
  only by `RolePlatformAdmin` (mirroring the registry-write permission
  already built for `jurisdictions`/`licences` in Stage 4I).
- `OpenBetSelfExclusionPolicy`'s default is a configuration value only
  (§2.8) — no code path change.
- G-2's policy (if separately scheduled) would be brand-scoped
  configuration, distinct from every tenant-scoped configuration table
  that exists today — a new configuration *scope*, not merely a new table,
  and worth flagging to `architect` as a genuinely new pattern if and when
  G-2 is scheduled.

---

## 8. Fail-closed analysis per jurisdiction-sensitive operation

Restating CM §4.0's invariant against every operation this plan's new
producers or consumers touch, to confirm none of the proposed work
introduces a fail-open path.

| Operation | Consumer | Fail-closed behaviour required | Already correct today (needs no new code) | New code must preserve |
|---|---|---|---|---|
| Casino launch (armed game) | Casino blocklist (K-3) | Unresolved → deny | Yes (CM §13.2 item 4) | New basis producers must not weaken this — an `unresolved(insufficient_confidence)` from a declared-only residence must still deny an armed game exactly as `no_signal` does today |
| Asset eligibility | AssetAuthorization layer 6 | `uuid.Nil`/non-resolved → deny | Yes (CM §4.1, unconditional) | Nothing — this consumer needs no change to benefit from a new basis |
| Jurisdiction-scoped Risk rule | `risk.Evaluate` | Conditional fail-closed (only if a rule scopes jurisdiction) | Yes (CM §4.2, R-1) | B-7/R-2b's authoring-time precondition (Phase … below) must land before any jurisdiction-scoped rule is authored for an operation not yet resolution-active, closing RISK §1.6's stuck-round hazard |
| Bonus grant issuance/activation/conversion | AssetAuthorization (via Bonus) | Unresolved → deny | Yes | Same as AssetAuthorization row — automatic |
| Market authorization (new, HDR-J-6) | New check | Empty/unpopulated `permitted_markets` → deny **every** market, not "no restriction" | **Must be built this way from day one** — this is the single most important fail-closed property this plan introduces, because an empty-list-means-permissive bug here would be the exact `SupportedCountries` defect CM §4.5 already found and rejected in payments, reintroduced in a new domain | New code — the inverted-default failure mode is the primary adversarial test target (§10) |
| Precedence resolution (new, HDR-J-2) | Resolver | No matching precedence row → `unresolved(irreconcilable_bases)` or `unresolved(no_signal)` per CM §2.2, never a silent pick | Mechanism already exists (CM §3.4) | New precedence-config content must not introduce an implicit "prefer most recent" or "prefer permissive" default — explicitly forbidden by CM §6.2 scenario 8 |
| Declared/verified residence read | New resolver basis producers | Missing column value → `unresolved(no_signal)`, not an error masquerading as resolved | N/A — new | New code must return `unresolved`, never treat a NULL residence column as "no restriction applies" |

---

## 9. Legal/compliance dependencies — collected in one place

### 9.1 Hard gates (implementation may be built and unit-tested against
synthetic data; activation against real player data is prohibited until
cleared)

| Gate | Blocks | Owner |
|---|---|---|
| Lawful basis / privacy-notice validation (HDR-J-3e) | 3a, 3b, 3c activation | Legal/privacy counsel |
| Retention-period validation (HDR-J-3f) | The retention job's actual TTL parameters (the job's *mechanism* is not gated) | Legal/privacy/compliance |
| KYC document-type/evidentiary-weight policy (HDR-J-3h) | `kyc_corroboration` activation | identity-compliance + compliance policy |
| Per-jurisdiction precedence content validation (HDR-J-2) | Any real `jurisdiction_precedence_configs` row | Legal/compliance, per jurisdiction |
| Jurisdiction-by-jurisdiction market review (HDR-J-6) | Any real `licences.permitted_markets` content | Legal/compliance |
| BYOL policy validation (HDR-J-5) | Not urgent; gates only a future BYOL onboarding | Legal/compliance, deferred |

### 9.2 Soft/recommended reviews (not blocking, but named)

- G-2's consumer-protection dimension (recommended legal review before
  the eventual G-2 implementation dispatch, whenever scheduled).
- Mixed/bonus-funded cashout + FD-1's bonus-abuse dimension (recommended,
  applies only once a cashout feature is designed).
- Converted-Grant-receivable question's legal/insurance weight — already
  answered "do not permit"; the soft dependency here is only for the
  future exceptional-reversal process the human's answer explicitly
  reserves, not for anything this plan schedules.

### 9.3 What is explicitly NOT gated

Phase A's tenant-licence-assignment mechanism, the market-authorization
check's *mechanism* (as opposed to its content), the precedence-config
schema extension's *mechanism*, and every RLS/audit/test item in §10 below
carry no legal dependency and can proceed on ordinary engineering review
alone.

---

## 10. Test strategy

All items below are **planned test obligations** for the eventual
implementation phases, written now so no phase begins without a stated
test floor, per this project's established discipline (CLAUDE.md's
financial-testing rule and Stage 4I's own `qa` chain precedent).

**Unit:**
- Each new basis producer (`geo_signal`, `player_declared_residence`,
  `player_verified_residence`) returns `unresolved` for every absent-input
  case, never a zero-value `resolved`.
- Confidence-threshold enforcement: a declared-only residence never
  satisfies an enforcement-grade operation class (regression form of CM
  §3.3's existing test, extended to the new producers).
- Market-authorization check: empty `permitted_markets` denies every
  market; a populated list denies everything not listed; the reason code
  is distinct from a Risk denial and a blocklist denial.

**Integration:**
- End-to-end: a synthetic tenant with `licence_id` set (Phase A) and a
  synthetic player with a declared residence resolves `resolved` for a
  non-enforcement operation class and `unresolved(insufficient_confidence)`
  for an enforcement-grade one.
- Precedence-config row selection: two disagreeing bases (declared vs.
  verified) resolve per the configured order; no matching row resolves
  `unresolved(no_signal)`.

**RLS:**
- New columns (`player_accounts.declared_residence_country`,
  `kyc_verifications.verified_residence_country`) inherit existing
  table-level RLS; a cross-tenant read attempt returns zero rows,
  mirroring CM §6.2 scenario 1's existing pattern extended to the new
  columns.
- The new reviewer-only write permission for verified residence is
  RLS/RBAC-tested independently of `kyc:write` generally (least-privilege,
  per HDR-J-3's own access-control condition).

**Concurrency:** no new lock-ordering hazard is introduced by these
columns (they are read the same way existing resolver reads are, under
H-2's read-only constraint) — a regression test confirming the new reads
also satisfy `ReadOnlyQuerier` (compile-time) plus the still-open
`DR-4I-QA-01` `pg_locks`/`READ ONLY` tests, which this plan explicitly
recommends closing **before**, not after, the new basis producers land,
since they will be exercised for the first time by real resolutions once
this work ships.

**Security / authz:**
- Adversarial case extending CM §6.2's nine-scenario contract to each new
  producer: forged declared-residence payload on an enforcement path
  (must 400, per Layer 1); a resolution's declared/verified basis never
  reaching a log or an audit record as a raw value (CM §5.3's never-record
  list, extended to the new columns explicitly).
- A player-scope read of `jurisdiction_resolutions` still returns zero
  rows (scenario 3, unchanged) even once the table has real rows in it —
  this is the first time this scenario is testable against non-empty data
  and must be re-run, not assumed to still hold.

**Audit:**
- `jurisdiction_fact.corrected` emits exactly once per staff correction,
  with actor/subject/reason/timestamp, and never the raw before/after
  value (per 3g).
- `jurisdiction_resolution_active.changed` and precedence-config change
  audit emitters, both currently unbuilt, land with B-7/HDR-J-2's work
  respectively.

**Fail-closed / provider-failure:**
- The market-authorization check under a database outage: `refused
  (dependency_unavailable)`, not "no restriction" (mirrors CM §6.2
  scenario 9).
- A geo-signal vendor outage (once 3a is built): the launch path fails
  closed per CM §9.4's forward constraint, and this must be its own
  dedicated availability/security review before the vendor is wired in.

**Config-versioning / historical reproducibility:**
- A resolution made under precedence-config version N remains
  reconstructable after version N+1 supersedes it (the existing
  `resolver_policy_version` column, already built, is exercised for the
  first time with real content — confirm it is read correctly on replay).

**Cross-brand / cross-tenant:** extend CM §6.2 scenarios 1–2 to the new
columns and the new market-authorization check exactly as already
specified; no new scenario shape is needed.

**Sportsbook-bonus interaction, ledger invariant, callback retry:** not
applicable to this plan's scope — no sportsbook cashout feature and no
ledger posting change is proposed here (§2.7, §2.9, §2.10).

**Adversarial cases specific to this plan's new surface area:**
1. A player declares a residence, then a KYC reviewer verifies a
   *different* country — confirm `irreconcilable_bases`/precedence
   resolves per configured order, never a silent last-write-wins.
2. A staff member attempts to set `verified_residence_country` without
   the new reviewer-scoped permission — 403, not merely a UI-hidden
   button.
3. A request body attempts to smuggle `declared_residence_country` into
   an enforcement-facing endpoint that does not declare it — 400 (Layer 1
   re-verified for the new field).
4. `licences.permitted_markets` populated for jurisdiction X but a
   resolution's `jurisdiction_code` is for jurisdiction Y (not X, not
   absent) — confirm denial, distinguishing "not on the list" from "list
   empty" in the internal (never player-facing) reason code.

---

## 11. Security / tenancy / RLS analysis

- Every new column is tenant-scoped by virtue of living on an
  already-tenant-scoped table (`player_accounts`, `kyc_verifications`);
  no new RLS policy design is required, only extension of existing
  `SELECT`-list discipline to project the new columns out of any query
  that does not need them (per HDR-J-3f's explicit condition).
- The new reviewer-write permission for verified residence must **not**
  ride on `kyc:write` generally — a distinct, narrower permission,
  consistent with `verification:read`'s existing precedent for
  `considered_bases` projection (CM §5.2).
- `tenants.licence_id`'s new write surface (Phase A) is platform-scoped
  only, following `PermCasinoCatalogueManage`'s precedent exactly — this
  plan does not authorize a tenant-scoped write to its own `licence_id`,
  since a tenant asserting its own licence is precisely HDR-J-5's
  unresolved BYOL question, not this plan's Phase A.
- `jurisdiction_precedence_configs`'s write surface is tenant-scoped
  (`RoleTenantAdmin`), matching `PermAssetAuthorizationWrite`'s precedent,
  per CM §6.1 — reconfirmed here because it is a genuinely new write
  surface, not merely a new column on an existing one.
- No new caching is introduced anywhere in this plan; CA-1 (CM §6.5)
  remains binding and unreversed. Any future reversal still requires an
  ADR + `security` review per CM's existing terms, unchanged by this
  document.
- Every new basis producer must be independently verified against CM
  §4.4's three-layer non-forgeability mechanism (no parse path; context
  cross-check; database FK/immutability) before being considered
  complete — this plan does not relax that bar for any new producer.

---

## 12. Risks and open questions

1. **Geolocation vendor selection (HDR-J-3a's forward dependency,
   RECON Q-7) is unresolved and this plan does not select one.** Building
   `geo_signal` requires choosing a provider interface first. **This is
   not a new Human Decision** — it is an ordinary vendor/integration
   choice within `integrations`'/`architect`'s existing authority, per
   CLAUDE.md's provider-abstraction section — but it must happen before
   3a can be implemented, and it introduces the hard-dependency-on-launch-
   path risk CM §9.4 already flagged and this plan does not resolve.
2. **G-2's brand-scoped configuration pattern is genuinely new** (§7) —
   no existing table is brand-scoped-with-versioning in the way G-2
   requires. `architect` should confirm this is the right shape before
   any migration is authored, whenever G-2 is scheduled.
3. **The precedence-config "most-restrictive-applicable" tie-break
   (§5.3) needs a formally specified partial order per operation class**,
   analogous to CM §7.3's MROC per-gate total-order requirement, or it
   risks becoming exactly the "rule sets are not totally ordered" trap CM
   §7.2 already found and rejected for HDR-J-4's mechanism. **Recommend**:
   whichever specialist eventually authors HDR-J-2's real content
   (identity-compliance, per CM §2.1's ownership split) must explicitly
   define this per operation class, not assume a generic ordering exists.
   This is not a new Human Decision — it is a specification obligation
   within the already-answered HDR-J-2 policy — but it is flagged because
   getting it wrong reproduces a defect this stage already fixed once.
4. **Retention-job mechanism (3f) has no existing platform precedent** —
   no per-column, per-jurisdiction TTL/erasure job exists anywhere in the
   codebase today. This is new infrastructure, not an extension of an
   existing pattern, and should get its own `architect` design pass before
   being scheduled, separate from the basis-producer work itself.
5. **No new Human Decision is identified as unavoidable by this plan.**
   Every open item above is either an ordinary engineering/vendor choice
   within an existing specialist's authority, or an already-flagged legal
   dependency under an already-answered HDR item (never a *new* jurisdiction
   question). This plan does not invent a twelfth decision.

---

## 13. Human Decision Traceability Matrix

| Decision | Technical consequence | Affected components | Required implementation | Required tests | Legal/compliance dependency | Can implement before legal approval? | Can activate before legal approval? |
|---|---|---|---|---|---|---|---|
| HDR-J-1 | None (confirms current state) | None | None | None (existing tests already cover this) | None | N/A | N/A |
| HDR-J-2 | Precedence-config content + schema extension | Resolver, precedence-config schema, audit | Schema extension (§5.3); content authoring surface | Precedence-selection tests (§10) | **Yes** — per-jurisdiction validation | **Yes** (schema/mechanism) | **No** (content) |
| HDR-J-3a | `geo_signal` producer | Resolver, vendor interface (unselected) | New producer + vendor integration | Adversarial cases 1/3 (§10) | **Yes** (3e) | **Yes**, against synthetic data | **No** |
| HDR-J-3b | `declared_residence_country` column | `player_accounts`, resolver, API | Migration + producer + endpoint | Full §10 suite | **Yes** (3e) | **Yes**, against synthetic data | **No** |
| HDR-J-3c | `verified_residence_country` column | `kyc_verifications`, resolver, reviewer UI | Migration + producer + reviewer workflow | Full §10 suite + adversarial case 2 | **Yes** (3e) | **Yes**, against synthetic data | **No** |
| HDR-J-3d | None | None | None | None | N/A (not authorized) | N/A | N/A |
| HDR-J-3e | Gates 3a/3b/3c activation | All of the above | None itself — a review, not a build | N/A | **Yes**, itself | N/A | N/A |
| HDR-J-3f | Retention job | New infra (§12 item 4) | New scheduled job + classification doc update | Retention-enforcement test | **Yes** (period values) | **Yes** (mechanism), **No** (periods) | **No** |
| HDR-J-3g | New audit emitter | Audit | `jurisdiction_fact.corrected` emitter | Audit-emission test | No | Yes | Yes |
| HDR-J-3h | Activates `kyc_corroboration` | Resolver, identity-compliance policy | Corroboration-consumption wiring | Corroboration test | **Yes** (document-type policy) | Partial (wiring), No (activation) | No |
| HDR-J-4 (narrowed) | Reporting-authoritative view | Reporting/BI | New query construct | Historical-reproducibility test (§10) | No (mechanism); Yes (SAR/data-residency specifics, per-jurisdiction) | Yes | Yes (mechanism); case-by-case for specific regulator filings |
| HDR-J-5 | None (not urgent) | None | None | None | Yes, deferred | N/A | N/A |
| HDR-J-6 | Market-authorization check | New check, licences.permitted_markets | New consumer + fail-closed test | §10's dedicated fail-closed suite | **Yes** — jurisdiction-by-jurisdiction review | **Yes** (mechanism) | **No** (content) |
| G-2 | Brand-level settlement policy | Bonus, settlement-posting, new config table | Not scheduled in §14 (§2.7) | N/A until scheduled | Recommended | N/A | N/A |
| `OpenBetSelfExclusionPolicy` | Config value only | RG | Set default value | Existing tests already cover both values | Yes (RG regulatory weight, but decision itself already answered) | Yes | Yes |
| Mixed/bonus cashout + FD-1 | None (feature doesn't exist) | Sportsbook (future) | Not scheduled | N/A | Recommended, future | N/A | N/A |
| Converted-Grant receivable | Documentation closure only | ledger-accounting-model.md §7.19.4 | Doc update, not code | N/A | Already resolved | Yes | Yes |

---

## 14. Implementation sequence — phases A through I

Each phase is independently shippable; a phase does not require the next
to exist to deliver value, and several can run in parallel once their own
prerequisites clear. **No phase below is authorized to begin by this
document alone** — each still requires its own implementation dispatch
from the orchestrator, and phases marked **LCD** additionally require the
stated legal/compliance clearance before activation (not merely before
merging tests).

- **Phase A — Prerequisite foundation.** Tenant-licence write surface
  (§4). Zero legal dependency. Unblocks `tenant_licence` for real tenants.
  Independently shippable today.
- **Phase B — Precedence-config mechanism.** Schema extension (§5.3),
  admin write surface, versioning/four-eyes wiring. Buildable without
  legal content; **content is LCD** (§9.1).
- **Phase C — Market-authorization mechanism.** New check consuming
  `licences.permitted_markets`, fail-closed-on-empty by construction, own
  reason code. Buildable without legal content; **content is LCD**.
- **Phase D — AR-2 linkage.** `jurisdiction_resolution_id` FK on
  `casino_launch_sessions`/`bonus_grants` (`DR-4I-BE-02`); wire
  `jurisdiction.Persist` into casino and Bonus checkpoints
  (`DR-4I-BE-01`). No legal dependency. Should land before Phase E so real
  resolutions are recorded from the first day they can occur.
- **Phase E — Player-side basis producers (HDR-J-3b/3c schema + code).**
  Migrations for `player_accounts`/`kyc_verifications`; new producers;
  new permissions; new audit emitter (3g). **Built and unit-tested against
  synthetic data only; LCD gates activation** (§9.1).
- **Phase F — Geo-signal producer (HDR-J-3a).** Vendor interface
  selection (§12 item 1) + producer + availability/security review of the
  new hard dependency (CM §9.4). **LCD gates activation**; vendor
  selection itself is not LCD but is a prerequisite this plan does not
  resolve.
- **Phase G — KYC corroboration activation (HDR-J-3h).** Wires the
  already-existing, currently-inert `kyc_corroboration` basis once Phase
  E's 3c lands. Requires identity-compliance's document-type policy.
  **LCD gates activation.**
- **Phase H — Retention infrastructure (HDR-J-3f).** New scheduled
  job/classification mechanism (§12 item 4), its own `architect` design
  pass first. **LCD gates the retention *periods*, not the job's
  existence.**
- **Phase I — Reporting/HDR-J-4 view + Testing/hardening closure.**
  Reporting-authoritative-jurisdiction construct (§2.4); closes
  `DR-4I-QA-01`, `DR-4I-RISK-01` (B-7) before Phase E's producers go live,
  since both become load-bearing the moment a real basis can resolve; full
  §10 test suite; final independent security/compliance review of the
  whole assembled feature, mirroring Stage 4I's own closing-phase
  discipline.

**Recommended order for value delivery, distinct from lettering:** A → D →
B/C (parallel, mechanism-only) → E (synthetic-data-only until its own LCD
clears) → G → F → H → I, with I's `DR-4I-QA-01`/B-7 closure items pulled
forward to run **before** E activates against real data, not after,
because both become genuinely load-bearing only once E does.

**Independently shippable slices**, restated: A; D; B's mechanism; C's
mechanism; each of E/F/G's producers individually (a producer with no
downstream legal clearance simply continues resolving `unresolved`, which
is safe); H's job mechanism (idle until 3f content exists). None of these
requires another to exist first except where stated (G needs E's 3c; the
LCD-gated activations need their respective legal clearance regardless of
code readiness).

> **Phase-lettering correction (added during the Stage 4I "Phase B"
> implementation dispatch, per the independent architect review's finding
> C-4/F6):** the human orchestration directives that actually authorized
> and dispatched this work use their own, different "Phase A"/"Phase B"
> labels, numbered strictly by dispatch order (Phase A = the tenant-licence
> write path; Phase B = the player jurisdiction evidence foundation). Those
> directive-level labels do **not** line up with this document's own A–I
> lettering above: the directive's "Phase B" is substantially this
> document's **Phase E** (player-side declared/verified residence schema +
> code) plus a slice of **Phase F** (the geo-signal producer, built here
> only as an interface/mock skeleton with no vendor selection, no HTTP
> surface, and no wiring into the resolver - a narrower scope than this
> document's own Phase F, which also includes vendor selection and a live
> availability/security review). Wherever a human directive or a completion
> report says "Phase A"/"Phase B", it means the directive's own numbering,
> not this section's. This document's A–I lettering is kept unchanged
> below as the substantive sequencing reference; readers should map
> directive-phase names to this section's letters via this note rather
> than assuming they match.

---

## 15. Specialist review plan

| Specialist | Scope |
|---|---|
| **Architect** | Reviews this plan's phase sequencing and the two flagged new patterns (G-2's brand-scoped config, §12 item 2; the retention-job infrastructure, §12 item 4) before either is scheduled. Owns any resulting ADR. |
| **Backend** | Owns Phase A, D, B/C mechanism authorship, subject to architect's schema review per CM §2.1's ownership split. |
| **Database / RLS** | Reviews every new column's RLS inheritance and the two new permissions (§11) before Phase E migrations are authored. |
| **Security** | Mandatory pre-activation review of every LCD-gated producer (E, F, G) per CM §4.4's three-layer mechanism, extended to the new surfaces; reviews Phase F's vendor-dependency posture per CM §9.4's forward constraint before any vendor is selected. |
| **Identity/KYC (identity-compliance)** | Owns HDR-J-2's real precedence content (per CM §2.1), the reviewer workflow for 3c, and the document-type/evidentiary-weight policy for 3h (Phase G). |
| **RG** | Confirms `OpenBetSelfExclusionPolicy`'s default-value change (§2.8) needs no code path review beyond configuration. |
| **Risk** | Owns B-7/R-2b's `CreateRule` precondition (`DR-4I-RISK-01`), scheduled in Phase I ahead of Phase E's activation. |
| **Bonus/Gamification (bonus-engine)** | Confirms Bonus's automatic benefit from a resolving basis needs no Bonus-side code change (§3, row 9); separately scopes G-2 if/when the orchestrator schedules it. |
| **Sportsbook** | No action under this plan; owns the future cashout-feature design that would eventually activate §2.9/§2.10. |
| **Payments/Ledger (ledger-finance)** | Owns the §2.11 documentation closure to `ledger-accounting-model.md` §7.19.4; confirms no ledger posting shape changes under this plan. |
| **QA** | Owns the full §10 test floor; owns closing `DR-4I-QA-01` in Phase I ahead of Phase E. |
| **Compliance-readiness (orchestrator-coordinated)** | Tracks §9.1's hard gates to closure; confirms no phase activates against real data ahead of its stated legal clearance. |

---

## 16. Proposed implementation gates

1. **Gate 1 (after Phase A):** tenant-licence write surface tested,
   security-reviewed, and a real licence value confirmed assignable —
   requires a one-time human confirmation of which `licences` row
   represents the Anjouan licence (an operational data-entry
   confirmation, not a new legal decision).
2. **Gate 2 (before Phase E begins):** `DR-4I-QA-01` and `DR-4I-RISK-01`
   (B-7) closed, per Phase I's pull-forward.
3. **Gate 3 (before Phase E/F/G activate against real player data):**
   HDR-J-3e's lawful-basis validation complete and `docs/architecture/
   16-privacy.md` updated accordingly — code may exist and pass tests
   before this gate; it may not run against a real player before it.
4. **Gate 4 (before Phase B's content activates):** HDR-J-2's
   per-jurisdiction precedence content legally validated.
5. **Gate 5 (before Phase C's content activates):** HDR-J-6's
   jurisdiction-by-jurisdiction market review complete.
6. **Gate 6 (before Phase G activates):** identity-compliance's
   document-type/evidentiary-weight policy for KYC corroboration
   finalized.
7. **Gate 7 (before Phase F is even scheduled):** a geolocation vendor
   selected and its availability/security posture reviewed per CM §9.4.
8. **Gate 8 (stage-level, mirroring Stage 4I's own closing discipline):**
   a full independent security/compliance review of the assembled feature
   before any of it is claimed as a jurisdiction *capability* rather than
   infrastructure — per CLAUDE.md, software capability and regulatory
   approval remain different things throughout every phase above.

---

## 17. Pre-stop validation checklist

1. ✅ Every one of the 11 `0042` decisions is traced in §2 to an explicit
   technical consequence or an explicit "no consequence" statement.
2. ✅ HDR-J-3's 8 sub-items are each individually traced (§2.3 table).
3. ✅ The 24-domain cross-domain impact matrix (§3) classifies every named
   domain, including domains with "No change."
4. ✅ A fail-closed analysis (§8) is stated per jurisdiction-sensitive
   operation, including the new market-authorization check's specific
   empty-list failure mode.
5. ✅ A security/tenancy/RLS analysis (§11) is included and does not
   propose any new RLS *model* beyond what already exists.
6. ✅ A migration inventory is proposed (§5) and is explicitly labelled as
   a planning sketch, not an authored migration.
7. ✅ An API/OpenAPI change list is proposed (§6) and is explicitly
   additive-only.
8. ✅ A comprehensive test plan (§10) covers unit/integration/RLS/
   concurrency/security/authz/audit/fail-closed/provider-failure/
   config-versioning/historical-reproducibility/cross-brand/cross-tenant
   categories, plus this plan's own new adversarial cases; sportsbook/
   ledger-invariant/callback-retry categories are explicitly marked
   not-applicable with a stated reason (§10, closing paragraph).
9. ✅ A dependency-safe implementation sequence across 9 named phases
   (§14) identifies independently-shippable slices explicitly.
10. ✅ A dedicated legal/compliance blockers section (§9) distinguishes
    hard gates from soft/recommended reviews and states explicitly what is
    NOT gated.
11. ✅ A full Human Decision Traceability Matrix (§13) covers all 11
    decisions with every required column, including can-implement-before-
    legal-approval and can-activate-before-legal-approval, both marked per
    row.
12. ✅ A risks/open-questions section (§12) is included; no new Human
    Decision is invented — §12 item 5 states explicitly that none was
    found unavoidable, and every flagged item is routed to an existing
    specialist's ordinary authority instead.
13. ✅ A specialist-review recommendation section (§15) names all twelve
    requested specialist scopes.
14. ✅ A 20-section-equivalent output structure is present (§1 current-state
    through §16 gates, §17 this checklist, §18 file/git report below —
    executive framing is carried by this document's opening two
    paragraphs plus §1, in place of a separate redundant "executive
    summary" heading, since CLAUDE.md's no-uncontrolled-scope-in-documents
    discipline disfavors restating the same content twice under two
    headings).
15. ✅ No code, migration, schema, API, or production-configuration file
    was created or modified in producing this document — verified in §18.
16. ✅ No existing ADR or existing decision record (`0001`–`0042`) was
    modified — verified in §18.

---

## 18. File and git report

**Only file created:** `docs/plans/stage-4i-jurisdiction-implementation-
plan.md` (this document).

Git status, diff scope, and commit are reported in this turn's final
message per the directive's required format, after this document is
written and before any commit is made.

---

## Closing statement

This document authorizes no implementation. Every phase in §14 requires
its own future orchestrator dispatch. Every LCD-marked item in §9 and §13
requires legal/compliance clearance this document does not provide and
cannot substitute for. Per the governing directive: **STOP here. Await
explicit human approval of this plan before any implementation, migration,
schema change, API change, configuration change, or next engineering stage
begins.**
