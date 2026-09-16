# Active Stage

## Stage 4H-B0 — Bonus, Gamification & Retail Scope/Implementation Plan — Complete

Status: **Complete, pending explicit human approval before Stage 4H-B1
(or any retail implementation stage).**

Architecture/scope-freeze stage responding to a new confirmed business
requirement: retail iGaming operations (a configurable agent-hierarchy
network — Operator → Partner → Super Agent → Agent → Player/Cashier,
never hardcoded) as another surface of the same platform. **No
production code, no migrations, and no implementation were started.**
Full detail: `docs/architecture/27-stage-4h-b0-scope-and-implementation-
plan.md` (the master synthesis, covering the directive's 25 numbered
deliverables) and `docs/governance/project-status.md`'s Stage 4H-B0
section. Governance record: `docs/governance/task-registry.md`'s
Stage 4H-B0 rows (4HB0-01 through 4HB0-14).

**Ten Wave-1 specialist documents**, each specialist owning a distinct
file: architect (`26-retail-operations-architecture.md`, the core
hierarchy/retail architecture — adjacency list + closure-table
projection, no hardcoded level ladder), ledger-finance (ADR 0035, agent
float as a platform liability, three new account types, Invariants
R1-R3), security (ADR 0036, three-axis authorization, closure-table RLS
fail-closed by construction, RG/KYC non-bypass made structural),
identity-compliance (docs 05/11 addenda), payments (doc 07's "Retail
cash rail" section), risk (ADR 0031 §19-24), data-analytics (doc 12
addition), backend (doc 04 addition), qa (testing-strategy.md addition),
bonus-engine (doc 10's MVP implementation-scope plan — 5-type first
slice, Stage 4G §32 gate qualified-lifted).

**Wave-2 review found and fixed 1 P0 + 8 P1 genuine cross-document
contradictions** (code-reviewer, mirroring Stage 4H-A's own Wave-2
pattern at larger scale — 14 findings F1-F14 across 10 documents,
~8,000+ lines). Most severe: a transaction-phasing contradiction in ADR
0036 that would have silently broken every retail ledger posting or made
every counter operation fail closed permanently (F1); a fail-open
ancestor-suspension check defeated by the very RLS policy meant to
protect it (F8); a payments handler that never called RG at all and
inverted the fixed RG-then-Risk gate order (F9, safety-critical). All
fixed in-place with explicit "Wave-2 review correction" callouts. Full
list: doc 27 §22a.

**Wave-2 scope review (product-owner-proxy)** independently confirmed
retail has zero Blueprint content (like Gamification in Stage 4H-A —
human-directed business scope, correctly never presented as a Blueprint
requirement by any specialist), gave a concrete recommended MVP-vs-
deferred split, and found one scope-creep item (ADR 0035's commission
machinery downgraded from binding to documented-for-future-reference
pending unresolved commercial terms).

**8 P0 human/legal/cross-specialist decisions block any retail
implementation stage from even being scoped** (doc 27 §23/§24): retail's
licensing status (the platform's only current licence is online-only),
whether a hierarchy node's float amends ADR 0007's human-approved
`Wallet` model, whether anonymous/bearer retail play is required
(structurally breaks RG/KYC/Risk if so), delegated limit-authoring
self-defeat risk, and others — all explicitly escalated, none guessed
at or resolved unilaterally.

**Bonus Engine's Stage 4G §32 block is qualified-lifted** for a
first-slice implementation (deposit/reload/cashback/generic-wagering/
coupon bonus types only) — bonus-engine made this gate-check call
directly, citing Stage 4G-FINAL's clean financial sign-off. This is a
scope plan, not an authorization to implement; Stage 4H-B1 still requires
explicit human approval like every other stage.

This stage's directive had no contradictory trailing line (unlike Stage
4H-A's) — it stated plainly "This stage must NOT automatically proceed
to implementation. Wait for explicit approval before Stage 4H-B1," and
this stage complies with that exactly: no implementation stage
(4H-B1/"Retail-Legal"/4H-B2/4H-B3) has been started.

---

## Stage 4H-A — Bonus, Gamification & Reward Orchestration Architecture Freeze — Complete

Status: **Complete, pending explicit human approval to authorize the next
stage (Stage 4H: Bonus Engine implementation).**

Explicitly an architecture + accounting + domain-contract freeze only,
per the stage's own directive: **no Bonus Engine, Gamification Engine,
Reward Orchestrator, sportsbook-bonus, CRM, notification-provider, or
external-reward-provider code was written.** Every deliverable this
stage is a design document, ADR, or governance-table update — zero
production code, zero migrations, zero tests, `go build ./...` untouched
and clean.

### What was frozen

- **Three distinct core domains** (never merged): Bonus Engine
  (`docs/architecture/10-bonus-engine-architecture.md`, rewritten),
  Gamification Engine (`17-gamification-engine-architecture.md`, new,
  plus `18-tournament-architecture.md`, `19-mission-architecture.md`,
  `20-reward-marketplace-architecture.md`), Reward Orchestrator
  (`21-reward-orchestration-architecture.md`, new — fulfillment
  mechanism only, never decides whether a reward is earned).
- **`ExternalRewardProvider` abstraction**
  (`23-external-reward-provider-contract.md`) for coexistence with a
  known future sportsbook provider's own native bonus engine —
  provider-neutral, not built.
- **Canonical Activity/Event taxonomy**
  (`22-canonical-activity-event-taxonomy.md`) extending the Stage-1
  `internal/eventbus.Event` stub, with the load-bearing `event_id` vs
  `idempotency_key` distinction made explicit after being found
  conflated in 3 draft documents.
- **Bonus accounting** (`docs/decisions/0032-bonus-accounting.md`,
  CRITICAL/authoritative, ledger-finance-owned): `promo_liability`,
  `bonus_expense`, Invariant B1, atomic 4-entry cash conversion, and the
  three funding-scenario ledger treatments (operator-funded,
  provider-funded, externally-fulfilled = zero ledger entries ever).
- **Points accounting** (`24-points-accounting-architecture.md`) with the
  `PointType` dual-scope-definition / tenant-scoped-balance correction.
- **Risk integration** (`docs/decisions/0031-risk-and-limits-engine.md`
  §14-§18): Bonus/Gamification consume `internal/risk.Evaluate`
  exclusively, no new limit engine.
- **RG integration** (`docs/decisions/0034-...rg-kyc-identity-
  integration.md`): RG remains sole authority; self-exclusion is
  prospective not retroactive; conversion-time denial leaves a Grant
  `completed`, never auto-forfeited.
- **Provider-neutral sportsbook interoperability**
  (`docs/decisions/0033-provider-interoperability-and-external-bonus-
  engines.md`): two future providers mapped onto one canonical contract,
  neither built.
- **API/RBAC contract** (`25-bonus-gamification-api-architecture.md`,
  design only): `bonus_config:read/manage`, `tournament:settle` split
  from `tournament_config:manage`, a recommended dedicated
  `RolePromotionsManager` role.

### Specialist review

Wave 1: bonus-engine, architect (Gamification/Tournament/Mission/
Marketplace), ledger-finance (ADR 0032 + doc 24), sportsbook (ADR 0033),
identity-compliance (ADR 0034), risk (ADR 0031 §14-§18), backend (doc
25) — 7 parallel drafts, plus 3 cross-domain connective documents
authored directly by the Orchestrator (doc 21, 22, 23).

Wave 2 (code-reviewer, security, qa, casino — parallel review of the
frozen set): found ~20 genuine P1-severity cross-document
contradictions (not stylistic — real conflicting decisions about the
same entity/mechanism). **All P1s fixed in-place**, each with an
explicit "specialist-review correction" callout: externally-fulfilled
bonus ledger treatment (3 conflicting documents), `event_id`-vs-
`idempotency_key` idempotency keying (3 documents), a
synchronous-vs-asynchronous marketplace-redemption transaction-boundary
conflict (docs 20 vs 24), a missing Reward Orchestrator reversal path,
the External Reward Provider callback contract missing 7 security rules
present in the casino-callback precedent it claimed to mirror
(including an integrity-alert rule for a callback not matching a
platform-created handle), tournament-settlement permission bundled with
prize-authoring permission, no permission proposed for bonus-campaign
authoring, anti-manipulation controls never wired into tournament
settlement as a required step, a contradictory instruction on reading
`internal/identityresolution` directly, and others (full list in this
stage's completion report delivered to the user). Lower-priority P2/P3
items (tournament entry/withdrawal re-entry cycling, achievement-unlock
reversal/void handling, demo-event exclusion enforced only as stated
policy rather than structurally) were recorded as open follow-up items
rather than fixed, per the stage's own architecture-freeze scope.

### Addendum: ledger-finance financial sign-off + product-owner-proxy scope review

Two further Wave-2 specialists completed after the first review round
above: `ledger-finance` (independent financial sign-off, required by
CLAUDE.md before any monetary architecture counts as reviewed) and
`product-owner-proxy` (scope discipline).

**ledger-finance: PASS WITH FINDINGS, sign-off granted once 7 P1s were
applied** — all 7 fixed in-place across ADR 0032, `financial-transaction-
flows.md` (Flows 5/6/7/9/11/20 gained the bonus-funded mirror legs an
implementer following the frozen documents literally would have missed,
breaking invariant B1 on the first bonus-funded bet; a new Flow 21 added
for externally-fulfilled = no posting), and docs 10/20/21/23 (a binding
lifecycle-event-to-posting map, a direct cash-reward treatment, an
explicit `manual_adjustment` account/mirror-leg rule, a tombstone on
Reward Orchestrator reversals of never-fulfilled decisions, a
fulfilment-destination declaration on the External Reward Provider
contract, and a corrected cross-tenant points-isolation rationale). Full
detail: `docs/governance/project-status.md`'s Stage 4H-A addendum.

**product-owner-proxy: no correctness findings, but a real scope-anchor
gap** — read the full Blueprint (20 pages) and confirmed it never
mentions gamification/points/XP/levels/achievements/badges/missions/
tournaments/leaderboards/streaks/marketplace/raffles/mini-games; the
entire Gamification Engine/Reward Marketplace/Reward Orchestrator domain
(5 architecture documents this stage) had no anchor in the Blueprint or
this project's prior MVP roadmap. Recorded as deferred scope in
`14-mvp-scope-and-roadmap.md`, with the Reward Orchestrator flagged as
premature abstraction and doc 18 (Tournaments) flagged as the most
disproportionately-designed sub-capability, both for if/when this domain
is ever authorized.

### Directive contradiction — flagged, not acted on

The stage's directive was a detailed, internally consistent 27-section
body explicit about being architecture-freeze-only ("YOU ARE NOT
AUTHORIZED TO IMPLEMENT THE BONUS ENGINE YET"), followed by a single
trailing line appended after the full body: "Approved — proceed with
Stage 4H: Bonus Engine." Per CLAUDE.md's stage-gate rule ("never begin
the next stage's implementation unprompted, even if it seems obviously
next"), the body was treated as authoritative and the trailing line was
not acted on. **Stage 4H (Bonus Engine implementation) is NOT
authorized.** This is re-flagged in the stage's completion report,
which explicitly asks for human confirmation before any Bonus Engine,
Gamification Engine, Reward Orchestrator, or sportsbook-bonus code is
written.

### Decisions/input needed before the next stage

1. Confirm which stage to authorize next: Stage 4H (Bonus Engine
   implementation) as the directive's trailing line suggested, or a
   different next stage — and confirm the architecture-freeze-only
   reading of this stage's directive was correct.
2. The lower-priority P2/P3 items recorded above (not fixed this stage)
   should be revisited once Bonus Engine/Gamification implementation is
   authorized.
3. All previously-open decisions from Stages 0-4G-FINAL remain open —
   see `docs/governance/project-status.md`'s consolidated list.

---

## Stage 4G-FINAL (+ FINANCE-GATE follow-up) — Architectural Hardening & Final Gate — Complete

Status: **Complete, pending human approval to authorize the next stage.**
Explicitly not a business-functionality stage - the directive's objective
was to harden Stage 4G (project orchestration governance + the Risk &
Limits engine) so the platform core is genuinely extensible, governed,
and safe to build future domains on. No new domain, no new business
capability. A follow-up "Stage 4G-FINAL-FINANCE-GATE" closed the one gap
left open when this stage originally committed: the independent
Financial/Ledger specialist review (see the updated specialist-review
section below) - itself also final-gate-only, no business functionality.

### Part A — Governance made operational

- `docs/governance/agent-registry.md` - new "Absolute constraint on every
  specialist" (no silent cross-domain edits, no self-assigned scope, no
  self-reviewed work) and "How the Orchestrator assigns every task to an
  owner."
- `docs/governance/task-registry.md` - two new permanent, append-only,
  cross-stage tables: the **Dependency Request Log** and the
  **Integration Approval Log** - the concrete mechanism for "how
  dependency requests/integration approvals are recorded," not just
  prose. The Stage 4G task table is preserved unmodified alongside the
  new Stage 4G-FINAL one, per the registry's own "never delete history"
  rule.
- `docs/governance/ownership.md`/`integration-protocol.md`/
  `change-control.md` updated to reference the new logs and the new test-
  reporting standard.

### Part B — Risk Engine contract finalized

`docs/decisions/0031-risk-and-limits-engine.md` gained §9-§13:
jurisdiction-context contract, licensing-mode contract, explicit `REVIEW`
semantics (a distinct outcome from `DENY` in the domain model - only
today's enforcement points collapse them, as an enforcement-point choice),
the three-step extension model for any future `LimitKind`, and a table of
every future domain's Risk-integration obligation. `risk.Evaluate`'s
signature and every Stage 4G decision are otherwise unchanged.

### Part C — Jurisdiction context gap structurally closed (PARTIALLY IMPLEMENTED)

Stage 4G disclosed: jurisdiction-scoped rules were reachable only from
`LaunchGame`, never `postBet`. Migration `0042_jurisdiction_and_licensing_
context` adds `casino_launch_sessions.jurisdiction_code`, populated once
at launch and read back by every subsequent bet in that round. No
geolocation vendor invented. **Not claimed as fully `IMPLEMENTED`**: no
HTTP handler populates `LaunchGameParams.JurisdictionCode` yet (the
pre-existing `TODO(jurisdiction)` root cause - no per-player jurisdiction
resolver exists anywhere in this codebase), so a real production launch
persists no jurisdiction today; the wiring is proven correct only by
tests that populate it directly. Regression tests:
`TestReceiveCallback_BetDeniedByJurisdictionScopedRiskRuleViaLaunchSession`,
`TestReceiveCallback_JurisdictionScopedRiskRuleDoesNotDenyADifferentJurisdiction`,
`TestReceiveCallback_SessionWithNoJurisdictionDoesNotMatchJurisdictionScopedRule`.

### Part D — Licensing-mode scoping added

New `LicensingMode` scope dimension on `Rule`/`RiskRequest`, mirroring
`tenants.licensing_model`'s existing two values (ADR 0006 - not a new
taxonomy). Resolved server-side by the caller (new `internal/identity.
GetTenantByID` + `internal/casino`'s `resolveLicensingMode`), never by
`risk.Evaluate` itself. Lets a platform-wide legal-ceiling `HARD_LIMIT`
avoid binding a future bring-your-own-licence tenant. No BYOL tenant
onboarded. Regression test:
`TestEvaluate_LicensingModeScopedHardLimitNeverBindsADifferentLicensingMode`.

### Part F — Flake root-caused and fixed (two distinct bugs)

`TestConcurrent_DuplicateBetDeliveryDuringSelfExclusion` (intermittent
since Stage 4D-RG/4E) was root-caused to a genuine mechanism, not
re-labeled: `postBet`'s idempotency short-circuit only reliably
serializes SEQUENTIAL redeliveries; two truly-concurrent deliveries of
the same bet could each start before the other committed and
independently re-evaluate live RG state, producing divergent outcomes for
the identical bet even though the ledger never posted more than once.
**Fixed**: a `pg_advisory_xact_lock` scoped to
`(tenant_id, provider_id, provider_tx_id)`, acquired before the
idempotency check.

Widening the shipped regression test to N=8 concurrent deliveries
(`internal/casino/adversarial_lock_stress_test.go`, QA-authored, not
requested by the directive) then surfaced a SECOND, deeper,
previously-undiscovered bug in `internal/rg.EvaluateEligibility`: it used
Postgres `now()` (frozen at transaction start) instead of
`clock_timestamp()` (re-evaluated per call), so a transaction queued
behind `rg.lockPerson`'s advisory lock could miss a self-exclusion that
had already committed. **Fixed** by switching to `clock_timestamp()` in
`internal/rg/rg.go`, with a new deterministic regression test
(`TestEvaluateEligibility_DetectsSelfExclusionCommittedAfterTransactionBegan`).

Both fixes verified via 30+ repeat full-iteration runs plus 9 additional
post-fix full-repo and targeted `-race -tags=integration` runs, all clean
(previously flaked within a single 15-iteration run, and the `internal/rg`
bug alone reproduced in ~50% of full-repo `-race -tags=integration` runs
before its fix).

### Parts E, G, H, I — documentation only

REVIEW semantics (E), the test-reporting standard (G,
`docs/testing/testing-strategy.md`), the LimitKind extension model (H,
ADR 0031 §12), and cross-domain boundary verification (I,
`docs/architecture/02-domain-and-service-boundaries.md`) - all
documentation, no code change beyond what Parts C/D/F already required.

### Specialist review: 11 of 11 areas now complete

Architecture, Risk, Casino, Responsible Gaming, Security/RBAC (x2),
PostgreSQL/RLS, API/HTTP, Adversarial Testing, Multi-tenancy,
Documentation/Governance all completed during the original Stage 4G-FINAL
run. **Financial/Ledger** did not complete in that run (its review agent
stalled and was stopped without findings; the Orchestrator's own
self-review at the time was explicitly recorded as not a substitute) but
was completed via the Stage 4G-FINAL-FINANCE-GATE follow-up: an
independent `ledger-finance` review of the `postBet` advisory lock and the
`internal/rg` `clock_timestamp()` fix, answering all 20 required questions
plus a 14-scenario adversarial-coverage matrix. **Verdict: PASS,
independent sign-off GRANTED.** No P0/P1 found; 6 P2s and 3 P3s recorded
as follow-up hardening/observability items (none blocking), none fixed
this stage per the finance-gate directive's own "no scope expansion"
instruction. Full findings, the 20 answers, and the coverage matrix are in
`docs/progress.md`'s Stage 4G-FINAL-FINANCE-GATE entry;
`task-registry.md`'s `IA-4GF-01`/`IA-4GF-03` rows carry the sign-off.

### Verification performed

See the Stage 4G-FINAL completion report's test matrix
(PASS/FAIL/FLAKE/NOT RUN/BLOCKED per suite, per the new reporting
standard).

### Decisions/input still useful from the human before the next stage

1. Approve Stage 4G-FINAL and authorize the next stage. Explicitly not
   authorized by this stage: Bonus Engine, a real KYC provider, a real
   casino provider, sportsbook.
2. The open decisions carried forward from Stage 4G (REVIEW-provisional-
   proceed question, HARD_LIMIT-vs-CONFIGURABLE_LIMIT precedence in a
   real jurisdiction, no platform-wide-rule HTTP write path) remain open
   - see `docs/governance/project-status.md`'s consolidated list.
3. All other already-open, non-blocking items from Stages 0-4G remain
   open (see `docs/governance/project-status.md`).
