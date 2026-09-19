# Project Master Status

Permanent project governance document (Stage 4G, Part A). The persistent
master map of the platform's state — completed stages, active stage,
blocked stages, open decisions, known risks, external dependencies, and
production blockers. Updated at the end of every stage by the Master
Orchestrator. This is a summary/index — full detail always lives in
`docs/progress.md` (per-stage narrative), `docs/active-stage.md` (current
stage detail), and the ADRs cited below.

## Completed stages

| Stage | Title | Status |
|---|---|---|
| 0 | Governance + architecture foundation | Complete |
| 1 | Platform-API foundation (config, DB, auth skeleton, RBAC skeleton) | Complete |
| Security Hardening (pre-Stage-3) | Sessions RLS hardening, MFA/production-signing ADRs | Complete |
| 2 | Identity + tenancy + security (players, staff, sessions, audit) | Complete |
| 3A | Financial architecture freeze + payment provider agnosticism | Complete |
| 3B | Core financial infrastructure (wallet, ledger, payments, withdrawal) | Complete |
| 3C | Financial hardening & operational controls | Complete |
| 3D | Withdrawal governance final gate | Complete |
| 4A | Casino integration foundation | Complete |
| 4D-RG | Responsible Gaming player-status enforcement foundation | Complete |
| 4E | Person resolution & cross-brand identity foundation | Complete |
| 4F | Player verification, documents & authentication foundation | Complete |
| 4G | Orchestration governance + Risk & Limits engine foundation | Complete |
| 4G-FINAL | Architectural hardening: operational governance, jurisdiction/licensing context, flake root-cause fix (11/11 specialist reviews complete - financial/ledger closed via the FINANCE-GATE follow-up below) | Complete |
| 4G-FINAL-FINANCE-GATE | Independent financial-correctness sign-off on the `postBet` lock + `internal/rg` fix (final-gate-only, no business functionality) | Complete |
| 4H-A | Bonus, Gamification & Reward Orchestration architecture freeze (no code) | Complete |
| 4H-B0 | Bonus, Gamification & Retail scope/implementation plan (no code) | Complete |
| 4H-B0-R1 | B0 gate corrections and finalization (no code) | Complete |
| 4H-B0-R2 | Bonus financial gate clarification (no code) | Complete |
| 4H-B0-R3 | Bonus rounding decision validation and financial gate closure (no code) | Complete |
| 4H-B0-R4 | Asset/Currency Registry, FX/Conversion, and dual-mode Sportsbook architecture closure (no code) | Complete |
| 4H-B0-R5 | Implementation readiness and final P1 closure — five R4 P1s architecturally resolved, residual findings catalogued (no code) | Complete |
| 4H-B0-R6 | Foundational implementation hardening — first authorized implementation stage; Asset Registry, idempotency, Risk fail-closed/exponent-awareness, RG self-exclusion hardening implemented and independently verified (real code + migrations) | Complete (this stage) |

## Active stage

Stage 4H-B0-R6 — see `docs/active-stage.md` for full detail. **Six
foundational-hardening workstreams implemented with real production
code and migrations, each independently reviewed, with a fix wave
closing every confirmed defect (including one live, exploited P1) and a
final independent re-verification pass confirming the fixes hold.
Labels at close: Asset Registry (Workstream A) and Risk fail-closed
hardening (Workstream D) IMPLEMENTED; idempotency hardening (Workstream
B) IMPLEMENTED as a shared primitive with no production call sites yet;
RG self-exclusion hardening (Workstream E) PARTIALLY IMPLEMENTED (one
non-launch-blocking scheduler-wiring gap); `player_locked` origin-split
(Workstream C) phase 1 (implementation ADR) DONE, phase 2 (migration +
code) NOT STARTED pending a human decision and a not-yet-built
wagering-progress-netting design; Bonus dependency contract freeze
(Workstream F) DONE. No implementation stage is authorized to begin
without explicit human confirmation — Stage 4H-B1 (Bonus Engine) remains
READY FOR HUMAN AUTHORIZATION after the `bonus_conversion` Risk
dependency (still NOT STARTED), unaffected by this stage. Stage 4H-B2
(Retail Architecture Hardening) awaits the Retail-Legal/Business gate.**

## Blocked stages

- **Bonus Engine**: architecture frozen (Stage 4H-A), an MVP
  implementation-scope plan is on file (Stage 4H-B0), and the gate was
  corrected in Stage 4H-B0-R1, clarified in Stage 4H-B0-R2, and **closed
  down to a single remaining item in Stage 4H-B0-R3**: the human
  answered the ADR 0021 rounding/precision decision (DS-1 = round-half-
  up; DS-2 = round once at the final monetary boundary, full precision
  until then, explicit function, never an implicit database cast; DS-3
  = one platform-wide rule by default with room for a future override)
  and six specialists (`ledger-finance`, `bonus-engine`, `risk`,
  `architect`, `security`, `qa`) validated it against the existing
  architecture with no contradiction or unsafe consequence found. Full
  recorded decision: `docs/decisions/0021-multi-asset-accounting.md`'s
  "Rounding and precision — RESOLVED" section. **Corrected status:
  Stage 4H-B1 is READY FOR HUMAN AUTHORIZATION after completion of
  `bonus_conversion`.** That dependency was re-verified in Stage
  4H-B0-R3, unchanged: **NOT STARTED — zero of ADR 0031 §16's six
  extension-process steps complete** (no `Operation` constant, no
  migration CHECK value, no HTTP-allowlist entry, no OpenAPI enum entry
  in any of its three required locations, no ledger transaction-type
  mapping, no enforcement call site), and it sits on the first slice's
  critical path since all five in-slice bonus types run through
  `completed → converted`. No other P0/P1 financial dependency remains
  (`architect` re-confirmed this with a focused 12-area review in both
  Stage 4H-B0-R2 and Stage 4H-B0-R3). Non-monetary work (lifecycle state
  machine, Offer/Grant modelling, eligibility) was never gated and
  remains unblocked. **Implementation itself is still not authorized** —
  this is a closed gate, not a start. See `docs/architecture/
  28-bonus-financial-gate-decision-sheet.md` for the plain-language
  decision record (now marked RESOLVED).
- **Retail (agent-hierarchy network)**: architecture/scope frozen (Stage
  4H-B0) across 10 documents, corrected and finalized in Stage 4H-B0-R1
  — see `docs/architecture/27-stage-4h-b0-scope-and-implementation-plan.md`.
  Not started. The earlier "8 P0 decisions block retail" framing was
  corrected: only **3** of those items are genuine human/business/legal
  decisions (retail licensing/jurisdiction status; confirmation of the
  proposed node-owned `agent_float` extension to ADR 0007 — the design is
  now drafted and reviewed, only the approval itself remains a human
  decision; anonymous/bearer retail play policy by jurisdiction) — see
  doc 27 §23A. The remaining items are **mandatory engineering acceptance
  criteria**, not human decisions, and do not require human input to
  resolve — see doc 27 §23B (fail-closed hierarchy RLS with no OR-NULL
  escape; no retail role holding `PermStaffManage`; RG-before-Risk
  ordering; closure-table write protection; server-side terminal
  credential resolution; POS idempotency namespace protection; offline
  fail-closed baseline; among others). A first retail implementation
  slice can be scoped once the 3 genuine human decisions above are made
  and the engineering criteria are designed in a hardening stage — see
  doc 27 §22's corrected two-path dependency graph (Retail-Legal/Business
  gate → Stage 4H-B2 Retail Architecture Hardening → Stage 4H-B3 Retail
  First Implementation).
- **Sportsbook**: not started — no implementation directive has authorized
  it yet. **Architecture closed (Stage 4H-B0-R4), P1s architecturally
  resolved (Stage 4H-B0-R5)**: dual-mode (external provider AND in-house
  engine, co-equal from day one) — canonical domain model,
  `SportsbookProvider`/`DataFeedProvider` abstractions, financial/ledger
  integration (`docs/decisions/0038-sportsbook-accounting-and-ledger-
  integration.md`), Risk integration (ADR 0031 §25-31), RG integration
  (ADR 0034 §9-13/§14) — see `docs/architecture/09-sportsbook-
  architecture.md`. No vendor named, no provider adapter built.
  Sequencing recommendation (not a constraint): external-first (doc 09
  §15). **Foundational primitives IMPLEMENTED as of Stage 4H-B0-R6**
  (real code, independently verified — none is sportsbook-specific
  production code, all are the shared platform primitives a future
  sportsbook implementation will consume):
  - The latent Risk fail-open (`Rule.breach()`'s cumulative-usage query
    blind to `account_type`) is **fixed** (`internal/risk/cumulative.go`)
    — a future `sportsbook_bet` cumulative-amount rule will now correctly
    measure stake rather than netting to zero. Confirmed by sportsbook's
    own independent readiness review.
  - The idempotency `occurrence_ordinal`/composed-key vulnerabilities
    security found (unauthenticated fallback discriminator, unescaped
    collision-prone composition) are **fixed** in the new
    `internal/idempotency` shared package, though it has **zero
    production call sites yet** — no domain has wired it in.
  - The Asset Authorization `(product, operation)` dimension sportsbook
    itself flagged as missing in Stage 4H-B0-R5 is **closed and
    independently tested** — a casino/sportsbook eligibility split for
    the same asset is now a genuinely distinguishable fact.
  - `OpenBetSelfExclusionPolicy`'s technical hardening (as-of resolution,
    tighten-only at both write and read time, authoritative DB time,
    stalled-enumeration detection) is **implemented**
    (`internal/rg/self_exclusion_*.go`, migrations `0043`/`0049`) —
    labeled PARTIALLY IMPLEMENTED only because the stalled-run detector
    has no scheduler wiring yet. **The platform-wide default policy
    value remains an unmade human/legal decision** (unchanged since
    Stage 4H-B0-R4) — this stage deliberately did not select it.
  - The ADR 0031 §26/§31 vs. ADR 0038 §13 cross-document conflict is
    **resolved on Risk's side** (ADR 0031 §36: adopts ADR 0038 §13's
    conclusion, `sportsbook_settlement`/`sportsbook_cashout` are not Risk
    checkpoints in provider-driven mode) with one non-blocking cross-check
    still open for `ledger-finance` regarding a hypothetical future
    in-house cashout-pricing mode.
  - `player_locked` origin-split: Shape A (`player_locked_cash`/
    `player_locked_bonus`) has a full **implementation-ready ADR**
    (`ledger-accounting-model.md` §6.4, Stage 4H-B0-R6) covering 10 of 12
    named accounting flows, independently validated by bonus-engine and
    sportsbook. **Migration and code (phase 2) are NOT STARTED.** Mixed
    cash+bonus funding and cashout are explicitly deferred/out of scope
    for the first implementation (a fail-closed rejection at placement,
    not a silent workaround). Bonus-funded placement specifically remains
    blocked on two gates: **G-2** (the terminal-Grant settlement-credit
    question — still an unmade human decision) and **G-3** (a new,
    real, confirmed P1: wagering progress is not netted against a later
    void/rollback of the same bonus-funded lock, a player-reachable
    progress-farming vector — the fix design is specified but not yet
    built). Cash-only cases and the schema widening itself have no
    remaining objection from any specialist.
- **Asset/Currency Registry + FX/Conversion**: **Part A/C (Asset
  Authorization) IMPLEMENTED as of Stage 4H-B0-R6** —
  `internal/assetregistry`, migrations `0044`/`0045`/`0047`. All 7 layers
  real and independently security-verified; the four-eyes control
  (create/activate/platform-authorize, and now also the layer-7
  platform-wide eligibility grant) is DB-enforced, after a live-exploited
  P1 was found (the person-identity self-approval check was
  unconditionally inert — no code path anywhere in the platform could
  set `person_id` on a `platform_admin` account) and fixed this same
  stage; RLS/FORCE RLS backstop on `assets` confirmed by adversarial
  testing (including a forged-GUC attempt); `assets.active` now defaults
  `false` (the 7 seeded assets are explicitly grandfathered
  `active=true` but `platform_authorized=false`, so every one denies at
  layer 3 until a dual-controlled platform-authorize act runs per
  asset — disclosed and intentional, not yet performed anywhere).
  `asset_authorizations`' RLS no longer permits a DELETE-based
  eligibility-widening exploit security also found and reproduced live.
  Layer 8 (market-rate availability) remains **NOT IMPLEMENTED** —
  concluded to be substantially a runtime FX-provider health/freshness
  check, not a stored fact, so no schema was built for it. **Part B
  (FX/Conversion itself) remains NOT IMPLEMENTED** — the Stage 4H-B0-R5
  security findings on the FX control-plane (no RBAC tier/dual control/
  mandatory audit on the deviation/staleness/spread bounds; the
  single-provider plausibility check is circular) are unchanged and
  still open, since Stage 4H-B0-R6's authorized scope was Asset
  Authorization only. Risk's exponent-awareness gap (ADR 0031 §32) is
  now closed (migration `0046`, `internal/risk/denomination.go`, reading
  through `internal/assetregistry.GetAsset` as the single source of
  truth). No domain wires `CheckEligibility` yet (confirmed by repo-wide
  grep) — the first domain that does will deny every operation until the
  seven seeded assets are platform-authorized; this is correct
  fail-closed behavior, recorded as a hard precondition on that first
  wiring task, not a defect.
- **Real KYC/AML vendor integration**: not started — Stage 4F built the
  provider-neutral boundary only; no vendor is contracted.
- **Real casino provider integration**: not started — `MockCasinoProvider`
  only exists today.

## Open decisions requiring human/product/legal input

1. Should an approved KYC verification ever apply platform-wide to a
   Person (cross-tenant reuse)? (ADR 0028 §3/§7)
2. Should a future real KYC verification supply `VerifiedAttributes` to
   `internal/identityresolution.PersonResolver`, closing the cross-brand
   evasion gap? (ADR 0027 §7, ADR 0028 §7)
3. Should a bring-your-own-licence tenant be able to opt out of
   platform-wide Person resolution for data-controller/legal reasons?
   (ADR 0027)
4. Priority/timeline for contracting a real KYC/identity-verification
   vendor — the single highest-leverage open item, named by every stage
   since 4E.
5. Should a `RiskDecision` of `REVIEW` ever proceed provisionally pending
   a compliance workflow, or must it always block until one exists? This
   stage's `internal/casino` integration blocks on `REVIEW` exactly like
   `DENY` (see ADR 0031 §6) — a deliberate, disclosed simplification, not
   a resolved product decision.
6. What is the platform's target precedence when a HARD_LIMIT and a more
   specific CONFIGURABLE_LIMIT genuinely conflict in a real jurisdiction
   (e.g. a jurisdiction's legal maximum stake vs. a brand's own more
   generous commercial limit)? This stage's answer (HARD_LIMIT always
   wins, never overridden by a more specific configurable rule) is an
   architectural default, not a legal opinion — see ADR 0031 §5.
7. No role can create a genuinely platform-wide risk rule via HTTP yet
   (`risk_config:manage` is always tenant-scoped) — a real, non-negotiable
   legal ceiling that must be tenant-proof requires a future
   platform-scoped write path (ADR 0031 §8), not built as of Stage
   4G-FINAL.
8. Platform-wide default value for `OpenBetSelfExclusionPolicy`
   (`SETTLE_NORMALLY` vs. `VOID_ON_SELF_EXCLUSION`), and whether either
   specific targeted jurisdiction (Anjouan, or any Europe/LATAM
   jurisdiction under consideration) has an existing legal requirement
   either way — flagged, not decided, in ADR 0034 §14 (Stage 4H-B0-R4/R5).
   Security additionally flags (Stage 4H-B0-R5): a permissive default
   combined with "absent jurisdiction configuration" resolving to that
   default is a launch-authorization item a human should see before
   sportsbook is enabled in any jurisdiction lacking an explicit
   configured value.
9. What happens when a settlement or self-exclusion-triggered void credit
   arrives against a bonus Grant that has already gone terminal
   (expired/cancelled/forfeited) — re-forfeit the credit, route it to
   `player_cash` as a mechanical entitlement settlement, or hold it for
   manual review (doc10 §5, ADR 0032 §5, Stage 4H-B0-R5). A genuine
   bonus-terms/product judgment call, not an architecture decision.
10. Mixed cash/bonus-funded sportsbook bet cashout (`C-cashout`,
    `ledger-accounting-model.md` §6.3.3.2, Stage 4H-B0-R5): proportional
    split vs. all-to-cash vs. simply not cashout-eligible. All three are
    ledger-mechanically valid (balanced, B1-safe) — the deciding factor is
    bonus-abuse/consumer-protection policy. `bonus-engine`,
    `product-owner-proxy`, and `sportsbook` all independently recommend
    "not cashout-eligible" as the lowest-risk, most easily reversible
    starting point, but this has not been formally decided.
11. Whether a settlement or void credit landing against a bonus-funded
    locked stake whose Grant has already gone terminal should re-forfeit,
    route to `player_cash`, or hold for manual review — the same
    question as item 9, now confirmed (Stage 4H-B0-R6) to gate
    BONUS-ONLY funding directly, not only mixed funding, since cash-only
    and bonus-only are the entire population `ledger-accounting-model.md`
    §6.4 covers with mixed funding deferred.

## Known P0/P1/P2 risks (carried forward, not silently closed)

- Stage 3D withdrawal TOCTOU (submit/resolve eligibility window).
- Payments-side unguarded-reversal race.
- Casino per-tenant provider signing-key design (not yet built).
- Casino real-provider API documentation requirement (no real vendor
  contracted yet).
- Cross-brand real-player protection remains inactive for a real player
  (Stage 4E's own limitation, unchanged by 4F/4G).
- RG future controls not implemented (deposit/loss/wagering/session
  limits at the RG layer specifically, reality checks, time-outs/
  cooling-off) — NOTE: Stage 4G's Risk & Limits engine can express a
  deposit/session/stake THRESHOLD, but does not implement RG's own
  player-protective controls (self-exclusion remains `internal/rg`'s
  sole domain) — see ADR 0031 §1 for why these stay separate domains.
- Bonus Engine accounting decisions not yet made.
- Sportsbook provider documentation not yet available (no vendor).
- ~~**Latent fail-open in `internal/risk`'s `Rule.breach()` cumulative-usage
  query**~~ **RESOLVED Stage 4H-B0-R6.** Found Stage 4H-B0-R5 (`risk`
  independent review, ADR 0031 §32): the query never joined
  `ledger_accounts`, so it was blind to `account_type`. Fixed via
  `internal/risk/cumulative.go`'s explicit, self-defending leg-selection
  model (`operationCumulativeSpecs`); a regression test proves the
  pre-fix query computed exactly zero against a real two-player-owned-leg
  posting while the fix correctly measures it, and a mutation-tested
  TOCTOU race test proves the advisory lock genuinely prevents overshoot.
- ~~**Cross-document conflict on sportsbook Risk checkpoints**~~
  **RESOLVED Stage 4H-B0-R6 (Risk's side).** ADR 0031 §36 adopts ADR 0038
  §13's conclusion and withdraws §26/§31's "near-term, load-bearing"
  framing for `sportsbook_settlement`/`sportsbook_cashout` in
  provider-driven mode (visible superseded-in-part markers, not a silent
  rewrite). One non-blocking cross-check remains open for
  `ledger-finance`: scope ADR 0038 §13's unconditional headline sentence
  to the provider-driven mode it describes, so a future in-house
  cashout-pricing mode (which would create genuine new exposure) doesn't
  inherit a blanket "never a Risk checkpoint" reading.
- **New Stage 4H-B0-R6 P1: bonus-funded sportsbook wagering progress is
  not netted against a later void/rollback of the same lock.** Found by
  `bonus-engine` while validating the `player_locked` implementation ADR
  (`ledger-accounting-model.md` §6.4.9 V-1, §6.4.11): wagering progress is
  a derived read over lock-time debits to `player_bonus`; an ordinary
  market void or rollback credits `player_bonus` back without netting
  the earlier debit, so progress stays counted for a bet that was never
  actually risked — a player-reachable farming vector (generalizes far
  beyond the Stage 4H-B0-R5 self-exclusion-specific finding this
  originated from). Fix design specified (net a lock debit only against
  a same-`correlation_id` void, or a rollback whose
  `reverses_transaction_id` points at the lock transaction itself — never
  a rollback of a settlement) but not yet built; also needs a new
  Progress-trail audit trigger point. This is gate **G-3**, blocking
  bonus-funded sportsbook placement (cases B/E/G/I of `ledger-accounting-
  model.md` §6.4) alongside gate **G-2** (the pre-existing terminal-Grant
  human decision, item 9/11 above, now confirmed to gate bonus-only
  funding directly). `bonus-engine` owns this fix.
- **Stage 4H-B0-R6 finding: RG self-exclusion stalled-enumeration-run
  detection has no operational wiring.** `internal/rg.FindStalledEnumerationRuns`
  (added this stage) is correctly implemented and tested, but has zero
  callers — the platform's one running reconciliation scheduler
  (`internal/reconciliation/scheduler.go`) only runs the ledger-vs-
  projection sweep. A stalled self-exclusion enumeration run (crashed
  before dispatch completed) is therefore still never surfaced in a
  running system. Confirmed by security's final Stage 4H-B0-R6
  verification. Not launch-blocking (no sportsbook self-exclusion
  enforcement exists to stall yet) but must be wired into a scheduler
  before any such enforcement ships. Labeled `PARTIALLY IMPLEMENTED` per
  CLAUDE.md's no-fake-completion rule, not silently reported as closed.
  `identity-compliance`/`devops` own wiring this.
- Five Stage 4H-B0-R4 P1s (FX rate-plausibility, Asset Authorization RBAC,
  idempotency design, `player_locked` origin-split,
  `OpenBetSelfExclusionPolicy`) are architecturally resolved as of Stage
  4H-B0-R5 but carry catalogued implementation-readiness residual
  findings — see the Sportsbook and Asset/Currency Registry + FX/
  Conversion entries above under Blocked stages for the full itemized
  list, and `docs/security/security-architecture.md` /
  `docs/testing/testing-strategy.md`'s Stage 4H-B0-R5 sections and ADR
  0031 §32 for full text.
- Stage 4G's own P2s — see `docs/progress.md`'s Stage 4G entry for the
  full itemized list.
- Stage 4G-FINAL's own P2s — see `docs/progress.md`'s Stage 4G-FINAL
  entry for the full itemized list.
- Stage 4G-FINAL-FINANCE-GATE's own P2s/P3s (6 P2s, 3 P3s, none fixed -
  see the "Resolved via the Stage 4G-FINAL-FINANCE-GATE follow-up"
  section below and `docs/progress.md`'s Stage 4G-FINAL-FINANCE-GATE
  entry for the full itemized list) — of particular note before any
  future stage builds further on `internal/casino`'s bet/win/rollback
  path: F1 (idempotent bet replay not amount-verified) and F2 (`postWin`
  missing a replay short-circuit after rollback).

## Resolved this stage (Stage 4G-FINAL) — for the historical record

- **`TestConcurrent_DuplicateBetDeliveryDuringSelfExclusion` intermittent
  flake (carried since Stage 4D-RG/4E)**: root-caused, NOT a test-only
  nondeterminism issue — a genuine correctness gap. `postBet`'s
  idempotency short-circuit only serializes SEQUENTIAL redeliveries of
  the same `provider_tx_id`; two GENUINELY CONCURRENT deliveries could
  each start before the other committed, both see "not yet posted," and
  independently re-evaluate live RG state — if a self-exclusion became
  effective between the two evaluations, the two deliveries could report
  DIFFERENT outcomes (one succeeded, one declined) for what the ledger
  itself proves was a single, idempotent financial effect. Financial
  correctness was never actually violated (`SUM(debits)==SUM(credits)`,
  at most one `ledger_transactions` row always held), but a provider
  treating "declined" as "no stake taken" would disagree with the
  ledger's own truth. **Fixed**: a `pg_advisory_xact_lock` scoped to
  `(tenant_id, provider_id, provider_tx_id)` (the tenant component added
  after adversarial/multi-tenancy review independently found the first
  version's `(provider_id, provider_tx_id)`-only key could serialize two
  DIFFERENT tenants sharing a provider against each other), acquired at
  the top of `postBet` before the idempotency check, forces a
  truly-concurrent second delivery to wait for the first's transaction to
  fully commit (or roll back) before re-reading — after that, the
  idempotency check reliably sees the first delivery's real outcome and
  the two can never diverge. No ADR was needed for this fix — it is a
  casino-package-internal concurrency fix
  (`internal/casino/orchestrator.go`'s `postBet`), not a Risk & Limits
  design change.
- **A second, deeper, previously-undiscovered bug this same
  investigation surfaced in `internal/rg`** (not anticipated by the
  directive — found only because adversarial review widened the shipped
  2-delivery regression test to N=8 concurrent deliveries across 5
  iterations, `internal/casino/adversarial_lock_stress_test.go`'s
  `TestConcurrentStress_ManyDuplicateBetDeliveriesDuringSelfExclusion`):
  even after the lock fix above, the wider test still failed
  intermittently (~50% of full-repo `-race -tags=integration` runs).
  Root cause: `internal/rg.EvaluateEligibility`'s self-exclusion query
  used Postgres `now()`, which is STABLE per transaction (frozen at
  transaction start), instead of `clock_timestamp()` (re-evaluated per
  call) — a transaction forced to wait on `rg.lockPerson`'s advisory lock
  (made more likely by the new casino-level lock's own queuing) could
  fail to see a self-exclusion that had ALREADY committed by the time its
  query ran, because its `now()` was frozen earlier. A genuine,
  pre-existing self-exclusion-enforcement correctness gap, not a test
  artifact. **Fixed** by switching both time-window comparisons in
  `internal/rg/rg.go` to `clock_timestamp()`. Verified by a new
  deterministic regression test,
  `TestEvaluateEligibility_DetectsSelfExclusionCommittedAfterTransactionBegan`
  (`internal/rg/rg_integration_test.go`, 5/5 pass with `-race`), plus 9
  total post-fix `-race -tags=integration` runs across the whole repo and
  targeted `internal/casino`+`internal/rg` re-runs, all clean (0/9
  failures) versus the prior ~50% failure rate. Both fixes together
  verified via 30+ repeat full-iteration runs under
  `-race -tags=integration -count=N` with zero failures (previously
  flaked within a single 15-iteration run), plus adversarial stress tests
  up to 10 concurrent identical deliveries
  (`internal/casino/adversarial_lock_stress_test.go`). Reverting the
  `clock_timestamp()` fix to confirm a red-before-fix control run was
  attempted and explicitly declined by the Claude Code auto-mode safety
  classifier ("[Security Weaken]"); correctness instead rests on the
  mechanism proof plus the empirical live-reproduction evidence already
  captured during investigation.
- **Jurisdiction-scoped risk rules unreachable from `postBet`** (Stage 4G
  disclosed limitation) — the STRUCTURAL gap is closed: `postBet` now
  reads the session's own persisted jurisdiction and forwards it (ADR
  0031 §9). Labeled `PARTIALLY IMPLEMENTED`, not `IMPLEMENTED`, per
  CLAUDE.md's no-fake-completion rule: no HTTP handler populates
  `LaunchGameParams.JurisdictionCode` yet (`internal/httpserver/
  casino_handlers.go`'s `LaunchGame` call site leaves it nil), so a
  production launch persists no jurisdiction today and a
  jurisdiction-scoped rule is evaluable but not yet reachable outside
  tests until a per-player jurisdiction resolver exists - the
  pre-existing `TODO(jurisdiction)` root cause, unchanged by this stage.
- **No licensing-mode scoping dimension existed** (Stage 4G disclosed
  limitation) — closed and reachable in production: unlike jurisdiction,
  `tenants.licensing_model` always has a real value and both real
  `internal/casino` call sites (`LaunchGame`, `postBet`) resolve and
  supply it unconditionally. See ADR 0031 §10.

## Resolved via the Stage 4G-FINAL-FINANCE-GATE follow-up

- **Financial/Ledger specialist review outstanding** (Stage 4G-FINAL's
  own disclosed gap - its dedicated review agent stalled and was stopped
  without findings) — closed: an independent `ledger-finance` review of
  the `postBet` advisory lock and the `internal/rg` `clock_timestamp()`
  fix ran to completion, answered all 20 required financial-correctness
  questions, and assessed 14 adversarial scenarios. **Verdict: PASS,
  independent sign-off GRANTED.** No P0/P1 found. 6 P2s (idempotent
  replay not amount-verified; `postWin` missing a replay short-circuit;
  a rollback-vs-concurrent-bet race surfaces an opaque 500 though
  financially safe; the lock has no `lock_timeout`; the
  `clock_timestamp()` fix leaves a smaller app-clock-vs-DB-clock skew
  window; no canonical lock order across bet/win/rollback on the
  balance-projection hot rows) and 3 P3s (declined bets aren't
  idempotent across sequential redeliveries of a *transient* denial;
  idempotent replays aren't audited; `postBet` doesn't reject
  expired/consumed sessions, which is deliberate per ADR 0025) were
  recorded, none fixed this stage (none blocking; fixing was judged
  outside this final-gate-only stage's authorized scope). Full findings:
  `docs/progress.md`'s Stage 4G-FINAL-FINANCE-GATE entry;
  `task-registry.md`'s `IA-4GF-01`/`IA-4GF-03` rows.

## External dependencies / provider documentation requirements

- Real KYC/AML vendor (Onfido or equivalent) — not selected.
- Real casino game aggregator — not selected.
- Real PSP(s) for production payment rails — not selected.
- Real crypto custody provider (ADR 0008) — not selected.
- Sportsbook feed/widget provider — not selected.

## Human decisions required before production launch

- Gambling licence decisions and jurisdiction selection beyond Anjouan
  (per CLAUDE.md's "When to stop and ask").
- Vendor/provider contracts for every "External dependency" above.
- Production credentials and commercial pricing for any contracted
  vendor.
- Legal interpretation of jurisdiction-specific RG/KYC/AML requirements
  before any real-money go-live.
- Explicit authorization for each future stage per the stage-gate rule.
- **Retail licensing status per target jurisdiction** (Stage 4H-B0) —
  land-based/retail gambling is typically licensed separately from
  online, and the platform's only current licence (Anjouan) is
  online-only.
- **Whether anonymous/bearer retail play is permitted or required** in
  any target jurisdiction (Stage 4H-B0) — would make RG/KYC/Risk
  enforcement structurally unsatisfiable as currently designed.
- **Whether a retail hierarchy node's float amends ADR 0007's
  human-approved multi-wallet-per-player model** (Stage 4H-B0, design
  drafted and reviewed in Stage 4H-B0-R1 — only the approval itself
  remains human) — see
  `docs/architecture/27-stage-4h-b0-scope-and-implementation-plan.md`
  §24 for the full 15-item register of retail- and bonus-specific human
  decisions. Item 15 (the ADR 0021 rounding/precision decision) is
  **RESOLVED as of Stage 4H-B0-R3** — see `docs/decisions/
  0021-multi-asset-accounting.md`'s "Rounding and precision — RESOLVED"
  section; retained in the register per its own append-only convention.
- **Whether an open, unsettled sportsbook bet should settle normally or
  be voided/refunded if the player self-excludes while it remains open**
  (Stage 4H-B0-R4) — a jurisdiction-dependent RG/compliance question
  `identity-compliance` explicitly declined to decide unilaterally; see
  `docs/decisions/0034-bonus-gamification-rg-kyc-identity-integration.md`
  §11 for the reasoning and recommended default (settle normally,
  consistent with this platform's existing "prospective, not
  retroactive" principle).

## Stage 4H-A: Bonus, Gamification & Reward Orchestration architecture freeze

Architecture/accounting/domain-contract freeze only — **no Bonus Engine,
Gamification Engine, Reward Orchestrator, sportsbook-bonus, or
external-reward-provider code was written this stage.** Three domains
frozen as architecturally distinct (never merged): Bonus Engine
(economic/promotional reward policy + lifecycle), Gamification Engine
(engagement/progression policy — points/XP/levels/missions/challenges/
achievements/badges/leaderboards/tournaments/streaks/marketplace/
raffles/mini-games), Reward Orchestrator (fulfillment mechanism only —
never decides whether a reward is earned).

Key decisions frozen this stage:
- **`ExternalRewardProvider` abstraction** (doc 23) for coexistence with
  provider-native bonus engines (a known future sportsbook provider has
  its own) — provider-neutral, two future providers mapped onto one
  canonical contract, neither built now.
- **Canonical Activity/Event taxonomy** (doc 22) extending the Stage-1
  `internal/eventbus.Event` stub (still unwired) — `event_id`
  (per-publish) vs `idempotency_key` (per-business-fact, stable across
  redelivery) is the load-bearing distinction; confusing the two would
  have caused real double-count/double-award bugs and was found
  conflated in 3 draft documents before being fixed.
- **Bonus accounting (ADR 0032, CRITICAL/authoritative)**: `promo_liability`
  as debit-side mirror of `player_bonus`; new `bonus_expense` account
  type; Invariant B1 (`signed(promo_liability) + Σ signed(player_bonus)
  == 0` per tenant/asset, hourly, zero-tolerance, P1); cash conversion is
  a single atomic 4-entry transaction (never retire-and-recredit, never a
  `ConversionOperation`); three funding scenarios — operator-funded
  (`bonus_expense`), provider-funded (`provider_payable`),
  externally-fulfilled (**zero ledger entries, ever** — this rule was
  violated by 3 draft documents and required cross-document fixes).
- **`PointType` scope correction**: the type DEFINITION is dual-scope
  (nullable `tenant_id`, mirroring `risk_rules`), matching a
  platform-wide catalogue; actual balance tables (`PointAccount`/
  `PointTransaction`/`PointEntry`) remain always `tenant_id NOT NULL` — a
  liability can never be platform-wide even if its definition is.
- **RG mid-lifecycle self-exclusion semantics** (ADR 0034 §2): prospective
  not retroactive — already-committed effects stand, in-progress grants
  simply stop progressing, no clawback. Conversion-time RG/Risk denial
  leaves the Grant `completed` (retryable), never auto-forfeits — an
  earlier draft's auto-forfeit-on-any-denial would have created a
  perverse incentive to delay self-excluding.
- **Points caps disclosed as unimplemented**: two draft documents claimed
  `internal/risk` already covers points earning/spending caps; ADR 0031
  §15h explicitly puts this out of scope while points are
  non-convertible. Corrected — no points-cap mechanism exists anywhere
  today; two possible future resolutions recorded but not authorized.

Wave 2 specialist review (code-reviewer, security, qa, casino) found ~20
genuine P1-severity cross-document contradictions among the
parallel-authored architecture set (not stylistic — actual conflicting
decisions about the same entity/mechanism, e.g. 3 documents disagreeing
on externally-fulfilled bonus ledger treatment, 3 documents keying
idempotency on the wrong field, a synchronous-vs-asynchronous
marketplace-redemption transaction-boundary conflict between docs 20 and
24). **All P1s were fixed in-place** across docs 10, 17, 18, 19, 20, 21,
22, 23, 24, 25 and ADR 0033, each with an explicit "specialist-review
correction" callout rather than a silent edit. Security's most notable
finding (F7): the External Reward Provider callback contract (doc 23)
was missing 7 rules present in the casino-callback precedent it claimed
to mirror, including an integrity-alert rule for callbacks not matching
a platform-created handle — a leaked-credential path that reconciliation
would otherwise route to a human to book as if real. Fixed. Lower-priority
P2/P3 findings not fixed this stage (tournament entry/withdrawal
re-entry cycling, achievement-unlock reversal/void handling, demo-event
exclusion enforced only as stated policy rather than structurally) are
recorded as open items for the eventual Bonus/Gamification implementation
stage, not fixed now, per this stage's own architecture-freeze framing
(CLAUDE.md's "no uncontrolled scope expansion").

### Addendum: ledger-finance financial sign-off + product-owner-proxy scope review

Two further Wave-2 specialists — `ledger-finance` (the independent
financial-correctness sign-off CLAUDE.md requires before any monetary
architecture is considered reviewed) and `product-owner-proxy` (scope
discipline) — completed after the review round above and required a
second round of fixes.

**ledger-finance: PASS WITH FINDINGS, sign-off granted once 7 P1s were
applied.** No P0. All 7 P1s fixed in-place: (1) the bonus-funded mirror
legs (`promo_liability`/`bonus_expense`) were missing from
`financial-transaction-flows.md`'s actual play flows (5/6/7/9/11/20) —
an implementer following the frozen flows literally would have broken
invariant B1 on the first bonus-funded bet; fixed, plus a new Flow 21
for externally-fulfilled = no posting; (2) no binding map existed from
Bonus Engine lifecycle events to ledger postings, leaving `cancelled`
with no accounting treatment and `converted` undefined — added ADR 0032
§3.1; (3) a direct cash reward (`cash_credit`) had no defined double-entry
treatment despite being referenced as supported in two documents — added
to ADR 0032 §3; (4) Rule B2 (the bonus mirror rule) didn't explicitly
bind `manual_adjustment`, and ADR 0032 §7 told staff to post a
partly-consumed-grant correction with no named target account — both
fixed; (5) the Reward Orchestrator's reversal path rejected
never-fulfilled reversals without a tombstone (reopening the exact race
CLAUDE.md's rollback rule exists to prevent) and placed its
single-reversal guarantee on a tracking table that doesn't exist for
money-moving mechanisms — fixed to defer to the owning ledger's own
guarantee; (6) the External Reward Provider contract never declared a
per-reward-type fulfilment-destination flag, leaving its own reversal
row hedging on "if any monetary effect exists" — exactly the
guess-at-posting-time ADR 0032 forbids — fixed; (7) the marketplace
document's cross-tenant points-isolation rationale cited the point
type's own scope, invalidated by this stage's own dual-scope correction
to `PointType` definitions — fixed to cite the balance tables' RLS
instead. 4 of 5 P2s fixed (idempotency-key terminology in ADR 0032 §8,
a decimal-vs-int64 amount representation gap in doc 21, an inline
zero-ledger-entries note in doc 21's mapping table, two new open
decisions on tournament prize-pool liability and split-prize rounding
added to ADR 0032); 1 P2 + 1 P3 recorded rather than fixed (a campaign
budget-cap enforcement gap disclosed as `NOT IMPLEMENTED`, and a cosmetic
`PointType` shape drift in doc 17 that document already labels
conceptual-only).

**product-owner-proxy: no P0/P1/P2 (a scope-discipline review, not a
correctness review).** Read the Blueprint in full (20 pages) and found
it never mentions gamification, points, XP, levels, achievements,
badges, missions, tournaments, leaderboards, streaks, a reward
marketplace, raffles, or mini-games — the entire Gamification Engine/
Reward Marketplace/Reward Orchestrator domain (5 of this stage's
architecture documents) has no anchor in the Blueprint and no anchor in
this project's own prior MVP roadmap (`14-mvp-scope-and-roadmap.md`).
Execution discipline within the frozen documents was found unusually
strong (correct `NOT IMPLEMENTED` labeling throughout, several genuine
`RECOMMENDATION`s correctly labeled as such, no recommendation
dishonestly presented as a Blueprint requirement) — the finding is that
the volume and fidelity of speculative design is itself a form of scope
creep, independent of how honestly it's labeled. The Reward Orchestrator
was specifically flagged as **premature abstraction**: a
three-domain-ready fulfillment layer built ahead of a second concrete
reward-producing domain, when Bonus Engine alone (the only domain
actually required by the Blueprint/MVP) already has a sufficient
lighter-weight fulfillment mechanism of its own. `18-tournament-
architecture.md` was flagged as the single most disproportionately
designed sub-capability — its settlement/prize-arithmetic/anti-collusion
depth exceeds parts of the Bonus Engine's own MVP-required core
lifecycle, for a feature with zero scheduled build. All findings applied
to `14-mvp-scope-and-roadmap.md`'s "Features deliberately deferred"
section, including guidance for when Stage 4H is eventually authorized:
scope the first Bonus Engine implementation to the MVP-required bonus
types only (deposit, reload, cashback, generic wagering bonus, coupon),
and do not build the Reward Orchestrator as a standalone domain unless
Gamification is authorized alongside it.

**This stage's directive contained a contradiction**: its 27-section body
was explicit, detailed, and internally consistent about being
architecture-freeze-only ("YOU ARE NOT AUTHORIZED TO IMPLEMENT THE BONUS
ENGINE YET"), but a single trailing line appended after the full body
read "Approved — proceed with Stage 4H: Bonus Engine." Per CLAUDE.md's
stage-gate rule ("never begin the next stage's implementation
unprompted, even if it seems obviously next"), the detailed body was
treated as authoritative and the trailing line was not acted on. **Stage
4H (Bonus Engine implementation) is NOT authorized and has not been
started.** Explicit human confirmation is required before any Bonus
Engine, Gamification Engine, Reward Orchestrator, or sportsbook-bonus
code is written.

## Stage 4H-B0: Bonus, Gamification & Retail scope/implementation plan

Architecture/scope-freeze stage responding to a new confirmed business
requirement: the platform must support retail iGaming operations (a
configurable agent-hierarchy network — Operator → Partner → Super Agent
→ Agent → Player/Cashier, configurable depth/structure per tenant/
licence/jurisdiction, never hardcoded) as another surface of the same
platform, sharing identity/wallet/ledger/risk/RG/payments/reporting/
audit/bonus/tenant architecture wherever appropriate. **No production
code, no migrations, and no implementation were started.** Full detail:
`docs/architecture/27-stage-4h-b0-scope-and-implementation-plan.md` (the
directive's own required master synthesis document, covering all 25
numbered deliverables).

Ten specialists (architect, ledger-finance, security, identity-
compliance, payments, risk, data-analytics, backend, qa, bonus-engine)
each produced a Wave-1 architecture document, each owning a distinct
file so nothing overwrote another's work. Key decisions: a configurable
hierarchy model (adjacency list authoritative + closure-table derived
projection, no hardcoded level ladder — architect); agent float modeled
as a platform liability, never commingled with player/commission/house
funds (ledger-finance, ADR 0035); a three-axis authorization model
(capability + tenant + hierarchy-scope) with closure-table RLS
fail-closed by construction (security, ADR 0036); RG/KYC non-bypass made
structural rather than policy-based (identity-compliance); retail cash
modeled as a fulfillment channel, not a `PaymentProvider` (payments);
two new Risk scope dimensions with an honest disclosure that daily/
periodic hierarchy-level funding limits are not yet expressible (risk,
ADR 0031 §19-24); one shared reporting pipeline for online+retail
(data-analytics); a single API surface for Back Office and a future
retail console (backend); and a concrete Bonus Engine MVP
implementation-scope plan with a qualified gate-check lifting Stage 4G
§32's block for a first slice (bonus-engine).

**Wave-2 review found and fixed 1 P0 + 8 P1 genuine cross-document
contradictions** (code-reviewer, 14 findings F1-F14) — the same "seam
failure" pattern Stage 4H-A's own Wave-2 review found, at larger scale
given this stage produced roughly 8,000+ lines across 10 documents.
Notable fixes: a transaction-phasing contradiction in ADR 0036 that would
have made every retail posting silently update zero projection rows or
fail closed permanently (F1, P0); a fail-open ancestor-suspension check
defeated by the RLS policy meant to protect it (F8); a payments handler
that never called RG at all and inverted the fixed gate order (F9,
safety-critical); four independently-proposed, mutually inconsistent
`risk.Operation` name sets (F4); and a three-way disagreement on
player-registration provenance, resolved as two complementary mechanisms
rather than picking one (F7). Full list:
`docs/architecture/27-*.md` §22a.

**Wave-2 scope review (product-owner-proxy) independently confirmed the
Blueprint-anchor finding**: retail, like Gamification in Stage 4H-A, has
zero Blueprint content — it is human-directed business scope, correctly
labeled as such throughout by every specialist (none presented it as a
Blueprint requirement). Gave a concrete recommended MVP-vs-deferred split
(a fixed 2-3 level hierarchy, cash deposit only, no commission
automation, no offline, no anonymous play, for a first slice — see
doc 27 §1.3/§2) and one scope-creep finding: ADR 0035's commission-
accounting machinery (periodic-run mechanism, override-cascade posting)
is more fully designed than its own unresolved commercial terms justify
— downgraded from binding to "documented for future reference" pending
those terms.

**This stage's original "8 P0 decisions block retail" framing was
corrected in Stage 4H-B0-R1** (doc 27 §23A/§23B) — see that stage's
section below for the reclassification into 3 genuine human/business/
legal decisions and a longer list of mandatory engineering acceptance
criteria that do not require human input.

**This stage's directive had no contradictory trailing line** (unlike
Stage 4H-A's) — it explicitly stated "This stage must NOT automatically
proceed to implementation. Wait for explicit approval before Stage
4H-B1," consistent with the detailed body. No ambiguity to flag.

## Stage 4H-B0-R1: B0 gate corrections and finalization

A correction/finalization stage — no architecture redesign, no
production code, no migrations. Purpose: resolve a contradiction the
Stage 4H-B0 report itself disclosed (claiming Stage 4H-B1 was
"independently authorizable" while also disclosing an unresolved ADR
0021 rounding dependency that blocks it) and correct several
misclassifications before any further stage is authorized.

Key corrections (full detail: doc 27, this stage's edits):
- **Bonus gate corrected**: Stage 4H-B1 is `CONDITIONALLY READY`, not
  independently ready — see the "Blocked stages" entry above for the
  exact three blocking conditions. `risk` verified `bonus_conversion` is
  NOT STARTED (zero of ADR 0031 §16's six steps), correcting an earlier
  understatement of "one small dependency request." `ledger-finance`
  enumerated ADR 0021's rounding/precision decision as three separable
  questions (direction, rounding point/precision handling, uniformity
  scope) with six neutrally-presented direction options, without
  selecting one.
- **Retail P0 list reclassified**: split into 3 genuine human/business/
  legal decisions (doc 27 §23A) and a longer list of mandatory
  engineering acceptance criteria that are not human decisions at all
  (doc 27 §23B) — see the "Blocked stages" entry above.
- **Agent float vs. Player Wallet formalized**: ADR 0007's `Wallet`
  remains untouched and player-owned; agent float is a hierarchy-node-
  owned operational ledger account in the same authoritative ledger,
  never a second ledger, never confused with a physical till.
  `ledger-finance` drafted a minimum additive schema amendment
  (`ledger_accounts` gaining a nullable `hierarchy_node_id` + a
  mutual-exclusion CHECK) — explicitly `NOT IMPLEMENTED`, reviewed by
  `architect` and `security` this stage, still requiring human approval
  before any migration is written since it amends the practical shape of
  a table whose broader design traces to a human-approved ADR (0007/
  0019). `ledger-finance`'s review also found and corrected a load-
  bearing error in the original draft: an earlier claim that the
  amendment "changes no existing constraint" was false — the existing
  house-level unique index predicate (`wallet_id IS NULL`) would have
  silently collapsed every hierarchy node's `agent_float` per asset into
  one shared row per tenant; the corrected proposal widens that
  predicate to `wallet_id IS NULL AND hierarchy_node_id IS NULL`.
- **First Retail Product Baseline formalized** (doc 27 §1.3a): identified
  players only, online-connected, single currency, cash deposit/
  withdrawal, a fixed shallow hierarchy for the first contracted
  operator, no automated commissions, no agent-to-agent float transfer,
  no direct bank agent settlement, no proxy/assisted play — implementation-
  scope constraints for the first slice, not jurisdiction-wide product
  claims.
- **Generic hierarchy model reaffirmed**: Node Type/Structure/Capability
  stay separate concepts; Operator/Partner/SuperAgent/Agent remain seed
  data, never hardcoded schema roles; Player is not a hierarchy node;
  Cashier is a staff identity assigned to a node; Terminal is a service
  principal (doc 27 §5, unchanged from Stage 4H-B0, reaffirmed this
  stage).
- **Stage dependency graph corrected into two independent, parallel
  paths** (doc 27 §22): Path A (Bonus financial gate → Stage 4H-B1) and
  Path B (Retail-Legal/Business gate → Stage 4H-B2 Retail Architecture
  Hardening → Stage 4H-B3 Retail First Implementation). Gamification and
  the Reward Orchestrator remain independently deferred with no scheduled
  next stage.
- **Human Decision Register** (doc 27 §24) rewritten as a clean list
  containing only genuine human/business/legal decisions — 15 items,
  including the 14 the directive named plus the ADR 0021 rounding
  decision.

**No production code, no migrations, no implementation was authorized or
started this stage.** Full detail, decisions resolved/still open, and the
exact updated gates: this stage's completion report and doc 27.

## Stage 4H-B0-R2: Bonus financial gate clarification

A financial-gate clarification stage — no architecture redesign, no
production code, no migrations. Purpose: close the remaining financial-
design gate for Bonus implementation by preparing an exact, plain-
language human decision package for ADR 0021's rounding decision and
independently re-verifying the `bonus_conversion` Risk dependency and
every other financial-gate area, so Stage 4H-B1 can be authorized the
moment the human decision is made, with nothing left to discover
afterward.

**New document**: `docs/architecture/28-bonus-financial-gate-decision-
sheet.md` — written for a non-accountant business owner, containing
only the decisions genuinely requiring human approval (the three linked
questions inside ADR 0021's rounding decision: direction, rounding
point/precision handling, uniformity/scope), each with plain-language
options, financial consequences, and a numerical worked example; a
precise bonus-type impact table; the `bonus_conversion` six-step
engineering checklist (informational, explicitly not a decision for the
human); and confirmation that no other blocker exists.

Six specialists reviewed in parallel, each independently verifying
against current repository state rather than trusting prior-stage prose:

- **`ledger-finance`**: produced numerical worked examples (a 50% match
  on a €133.33 deposit landing exactly on a half-cent tie; a repeating
  7.3% weekly cashback showing the truncate-and-carry mechanism
  concretely); confirmed no rounding option (A-F/P1-P3) can break
  `SUM(DEBITS)==SUM(CREDITS)` — for options A-E there is no separable
  residue to drop at the bonus-grant/cashback posting site (the mirrored
  `promo_liability`/`player_bonus` legs are always posted with the same
  already-rounded integer), while option F introduces a genuine new
  piece of financial state (a remainder accumulator) that would need the
  same concurrency-safe, auditable, reconciliation-capable discipline as
  the ledger itself, plus an unresolved reversal/forfeiture policy for
  an unreleased remainder; confirmed the five-type first slice is
  otherwise fully compatible with the ledger, wallet, idempotency, and
  reconciliation design regardless of which option is chosen; flagged a
  concrete implementation trap (PostgreSQL's default numeric-to-integer
  cast silently implements round-half-up, so whichever option is chosen
  must be an explicit function in the one shared rounding helper, never
  an implicit cast).
- **`bonus-engine`**: produced a precise, non-blanket bonus-type impact
  table — the rounding decision affects the grant amount itself for
  Deposit bonus, Reload bonus, and Cashback, but only the derived
  wagering-requirement/contribution-tracking computation (not the flat
  grant/face amount) for the generic Wagering bonus and Coupon;
  independently confirmed (not merely trusted) that all five in-slice
  types reach `completed → converted` and therefore all five require the
  `bonus_conversion` Risk dependency.
- **`risk`**: re-verified `bonus_conversion` is still NOT STARTED against
  the current repository state (no code had changed since Stage
  4H-B0-R1); produced a formatted six-step engineering checklist (owner,
  affected file, dependency, required test, and whether each step can
  land before Stage 4H-B1 is authorized) for the decision sheet's
  informational appendix; reconfirmed `bonus_conversion` is the only
  Risk-owned P0/P1 dependency for the five-type slice.
- **`architect`**: performed a focused 12-area financial-gate review
  (ledger accounting, wallet architecture, idempotency, concurrency,
  Risk, RG, audit, RLS, reconciliation, transaction/account types,
  multi-asset precision, bonus conversion) against the current
  repository state and **found no additional P0/P1 blocker** beyond the
  two already-known gates; flagged one non-blocking documentation
  cross-reference gap (ADR 0034 §2's RG mid-lifecycle nuance vs. doc 10
  §5's already-correct resolution).
- **`security`**: found no additional blocker; flagged that whichever
  rounding rule is eventually built must ship with an explicit manage
  permission and mandatory audit logging on any rule change (a
  forward-looking implementation requirement, not a gap in today's
  system, since no rounding-rule config exists yet); confirmed a player
  cannot influence or bypass the `bonus_conversion` enforcement point
  under the existing server-side-resolution discipline; found one minor,
  non-blocking audit-table completeness gap (a `reversed` bonus
  transition has no explicit row in doc 10 §10's audit table, though the
  Progress-trail requirement already covers it).
- **`qa`**: confirmed the core CLAUDE.md financial test matrix (normal
  transactions, duplicates, concurrency, retries, rollback) is already
  well-mapped to bonus scenarios via ADR 0032's testing floor; designed
  the property-based test approach needed to prove whichever rounding
  rule is chosen is deterministic, exactly recomputable, and never
  breaks debit/credit balance; flagged several non-blocking test-plan
  additions for engineering's backlog (a self-exclusion mid-lifecycle
  race test for Bonus specifically, a bonus actor-matrix authorization
  test, a coupon-redemption concurrency test, and others) — none require
  new architecture or a human decision.

**No production code, no migrations, no implementation was authorized or
started this stage.** `go build ./...` re-run after all edits and
remains clean (docs-only diff). **Stage 4H-B1 remains BLOCKED and is NOT
authorized** — this stage produced the decision package, it did not make
the decision or approve the next stage.

## Stage 4H-B0-R3: Bonus rounding decision validation and financial gate closure

A financial-gate closure stage. The human proposed answers to the three
ADR 0021 rounding questions from the Stage 4H-B0-R2 decision sheet; this
stage's purpose was to independently validate those proposed answers
against the existing architecture (never to select or silently change
them), and, once validated, formally record them and re-check the
overall Bonus financial gate one final time. **No production code, no
migrations, no implementation was authorized or started.**

**Decisions validated and recorded**: DS-1 = round-half-up (ties away
from zero); DS-2 = round once, at the final monetary boundary, full
`NUMERIC` precision until then, via an explicit shared function, never
an implicit database cast, no truncate-and-carry remainder mechanism;
DS-3 = one platform-wide rule by default, with room for a future
per-asset/jurisdiction override if genuinely required. Six specialists
(`ledger-finance`, `bonus-engine`, `risk`, `architect`, `security`,
`qa`) independently validated these against the current repository
state and found them **safe, deterministic, and reconciliation-
compatible — no contradiction, financial problem, precision problem, or
architecturally unsafe consequence requiring the human to change the
decision.** A small number of non-blocking implementation-time
clarifications were found and folded into the recorded decision (a
zero-rounding-result edge case needing a Bonus Engine eligibility guard;
disambiguation between the wagering-requirement target, which is a
non-posted comparison threshold, and per-game contribution weighting,
which is genuinely monetary and does have a rounding boundary; the exact
multiply-then-cap rounding order; and an open Coupon Offer-template
scoping question) — none of these reopen or change DS-1/DS-2/DS-3
themselves. Full recorded decision, algorithm, and storage
specification: `docs/decisions/0021-multi-asset-accounting.md`'s
"Rounding and precision — RESOLVED" section.

**Cashback residual confirmed**: repeated cashback events leave a small
fractional residue every time. The existing architecture has no
remainder-accumulation mechanism anywhere; building one would be new,
unauthorized architecture. **What DS-1+DS-2 as recorded actually mean
for cashback is: each calculation rounds independently and immediately,
with no accumulation** — recorded explicitly rather than left implicit.

**`bonus_conversion` re-verified, unchanged**: `risk` re-checked all six
ADR 0031 §16 artifacts directly against current repository state — still
**NOT STARTED, zero of six steps**. The rounding decision introduces no
new Risk-side dependency: Risk evaluates only the already-rounded,
posted minor-unit integer, confirmed by construction of
`RiskRequest.Amount`'s `int64` type, not assumed. `architect`
re-confirmed no other P0/P1 financial dependency exists.

**Final financial gate: Stage 4H-B1 is READY FOR HUMAN AUTHORIZATION
after completion of `bonus_conversion`.** No P0, no P1 financial
blocker, no unresolved accounting decision, no unresolved precision
decision, no unresolved rounding ambiguity remains. `bonus_conversion`
is not implemented this stage, consistent with the directive's
instruction to prefer keeping it for Stage 4H-B1 unless closing the
rounding decision required it internally (it did not).

**New confirmed product requirement analyzed (not implemented): an
extensible Asset/Currency Registry and future FX/Conversion
architecture.** `architect` found the underlying `assets` schema is
already open and extensible (no closed enum, no hardcoded decimal
count) but lacks the operational surface the requirement needs (admin
API, authorization model, audit logging, additional eligibility
columns) — an additive extension, not a redesign. Recommended: a future
ADR (0037) and a dedicated future implementation stage, both recorded as
deferred in `docs/architecture/14-mvp-scope-and-roadmap.md` and detailed
in `docs/architecture/27-*.md` §26. The FX/Conversion boundary was
designed on paper (Registry / FX Rate Provider / Conversion Service /
Ledger transaction, kept structurally separate) with the required
immutable audit fields for any future conversion specified. **Confirmed:
no impact on Bonus Stage 4H-B1; no new blocker for Retail** beyond the
pre-existing, independently-tracked conversion-clearing-account open
decision. Two P1 risks flagged for the eventual Registry design (the
asset-creation/activation authorization boundary is undefined; fail-
closed FX behavior must be written into the new ADR as a binding rule).
**No code, no migrations, no new ADR, and no Registry/FX implementation
were created this stage** — analysis and documentation-scoping only, as
directed.

Full detail: this stage's completion report, `docs/decisions/
0021-multi-asset-accounting.md`, `docs/architecture/27-stage-4h-b0-
scope-and-implementation-plan.md` §1.1/§22/§24/§25/§26, and
`docs/architecture/28-bonus-financial-gate-decision-sheet.md` (now
marked RESOLVED).

## Stage 4H-B0-R4: Asset/Currency Registry, FX/Conversion, and dual-mode Sportsbook architecture closure

An architecture-only closure stage — **no production code, no
migrations, no provider integration, no real vendor named.** Closes the
Asset/Currency Registry requirement doc 27 §26 deferred in Stage
4H-B0-R3, and formalizes a new mandatory requirement: the platform must
architecturally support BOTH an external sportsbook-provider integration
and a strong in-house sportsbook engine, provider-neutral, co-equal from
day one — not designed around only one model.

**New documents**: `docs/decisions/0037-asset-currency-registry-and-fx-
conversion-architecture.md` (new ADR, `architect`) and `docs/decisions/
0038-sportsbook-accounting-and-ledger-integration.md` (new ADR,
`ledger-finance`). **Rewritten**: `docs/architecture/09-sportsbook-
architecture.md` (48 → ~1050 lines, `sportsbook`) — a Stage-0 proposal
that recommended external-only ("start with widget/iframe") is now the
canonical, dual-mode architecture; that original recommendation is
preserved as operational sequencing guidance, not an architectural
constraint. **Extended**: `docs/decisions/0031-risk-and-limits-engine.md`
(new §25-31, `risk` — Sportsbook Risk integration) and `docs/decisions/
0034-bonus-gamification-rg-kyc-identity-integration.md` (new §9-13,
`identity-compliance` — Sportsbook RG integration), both pure appends;
existing content untouched. A small, explicitly-attributed clarification
was also made to `docs/decisions/0033-provider-interoperability-and-
external-bonus-engines.md` (nullable `provider_id` = in-house; the
two-bonus-source model correctly degenerates to one source for in-house
bets).

**Nine specialists** worked this stage: five drafted in parallel
(`architect`, `sportsbook`, `ledger-finance`, `risk`,
`identity-compliance`), then four performed genuinely independent review
(`architect` on the other four's work, `ledger-finance` reviewing
`architect`'s ADR 0037 — not its own ADR 0038, `security`, `qa` running
the full 20-item extensibility test) — no specialist reviewed its own
authored document, per the directive's explicit requirement.

**One real, substantive contradiction was found and fixed**: ADR 0038
§13 originally proposed netting `sportsbook_rollback` (in addition to
`sportsbook_void`) against a player's cumulative daily stake usage —
this directly contradicted the same ADR's own §10 void/rollback
distinction and would have zeroed out or gone negative on a player's
genuinely-staked amount after any market-correction event. `ledger-
finance` corrected it directly in its own document once the
contradiction was identified by independent review; `sportsbook_rollback`
is never netted against cumulative stake usage.

**Five smaller gaps were found and closed**, all as narrow, explicitly-
attributed additions (no redesign): two fail-closed/error-contract gaps
in the Asset Authorization boundary (absence of a config row must read
as deny, not permit; a non-nil error from the check is always treated
as ineligible); a rate-plausibility caveat on the FX fail-closed rule
(the 8 structural checks do not detect a well-formed-but-economically-
wrong rate from a compromised or malfunctioning provider — a
rate-deviation-bound check is named as a required implementation-time
control, not resolved here); a missing `rounding_rule_id` field mapping
between the new Conversion record and ADR 0021's existing
`ConversionOperation`; and a mischaracterized precedent (ADR 0037
originally claimed ADR 0031 §8's platform-admin write path was "already
established and implemented" — it is, by ADR 0031's own words, itself
an open, unbuilt gap; corrected).

**Final gate — all closed, no P0 found:**
- **A. Asset/Currency Registry**: closed (ADR 0037 Part A). The `assets`
  schema is already open/extensible; the missing operational surface
  (8 distinct authorization/eligibility layers, one canonical
  `AssetAuthorization.CheckEligibility` service) is now designed.
- **B. FX/Conversion**: closed (ADR 0037 Part B). Four structurally
  separate components (Registry / FX Rate Provider / Conversion Service
  / Ledger transaction); a canonical Conversion record; an 8-condition
  fail-closed rule (now with the rate-plausibility caveat stated
  explicitly).
- **C. Asset Authorization Boundary**: closed (ADR 0037 Part C),
  resolving Stage 4H-B0-R3's flagged P1 — a two-tier split
  (platform-admin-only registration/activation; tenant-scoped,
  narrow-only activation), one canonical check every domain must
  consume.
- **D/E/F. Sportsbook (external/in-house/data-feed)**: closed (doc 09).
  Canonical domain model, provider-neutral `SportsbookProvider`/
  `DataFeedProvider` abstractions, a five-layer in-house engine (Sports
  Data → Business Logic → Ledger → Risk → Trading Operations, the
  sportsbook never becomes a second wallet), tenant/brand/jurisdiction
  mode-selection routing.
- **G. Financial integration**: closed (ADR 0038, corrected). No new
  account types; 6 new transaction types; idempotency keyed on
  `(tenant_id, provider_id, provider_tx_id)` with a `correlation_id`
  tying a bet's lifecycle together — `qa` flagged one residual,
  explicitly-disclosed risk (a non-conformant provider reusing a
  reference across two same-type, coincidentally-identical-payload
  events) as a required future test/design hardening item, not a
  present defect.
- **H. Risk/RG integration**: closed (ADR 0031 §25-31, ADR 0034 §9-13).
  Sportsbook consumes the existing central Risk and RG engines
  exclusively — no second engine. One genuine open human/compliance
  decision recorded, not resolved: whether an open, unsettled bet
  should settle normally or be voided if the player self-excludes while
  it's still open (`identity-compliance` recommends settling normally,
  consistent with this platform's existing "prospective, not
  retroactive" principle, but flagged this as requiring compliance
  confirmation, not decided unilaterally).
- **I. Bonus/Gamification integration**: closed (doc 09 §7, ADR 0033
  clarified). Sportsbook feeds the existing canonical Activity/Event
  architecture; provider-native sportsbook bonuses coexist with the
  Platform Bonus Engine via ADR 0033's existing `fulfillment_owner`
  mechanism, now also covering the in-house degenerate case.
  Explicitly: **no separate sportsbook-only bonus system.**
- **J. Retail integration**: closed (doc 09 §10). A retail-originated
  bet is an ordinary Bet through the same acceptance pipeline — no
  separate retail sportsbook engine.
  **K. Future extensibility**: demonstrated. All 20 required
  extensibility-test questions answered YES, each independently verified
  against the actual document content (not the documents' own summary
  claims) by both `qa` and, for the sportsbook-specific items, `architect`.
- **L. No P0 remains.** Five P1s are disclosed, not hidden or
  downgraded, none blocking this architecture-closure stage: (1) a
  rate-plausibility check is a required control before any live FX
  provider connects (ADR 0037, now stated explicitly); (2) the Asset
  Authorization boundary's concrete RBAC permission names and admin API
  surface remain to be designed at implementation time; (3) ADR 0038's
  idempotency scheme's residual same-type/coincidental-payload collision
  risk needs a per-occurrence distinguishing mechanism before
  implementation; (4) the pre-existing `player_locked` origin-split
  decision (ADR 0032 §10) still blocks bonus-funded (not cash-funded)
  sportsbook wagering; (5) the open-bet self-exclusion policy is a
  genuine human/compliance decision, not yet made.

**Architecture is READY FOR IMPLEMENTATION for the Asset/Currency
Registry, FX/Conversion, and Sportsbook (external + in-house) domains.
None of them are authorized to begin.** This stage does not affect
Bonus Stage 4H-B1's own separate gate (still READY FOR HUMAN
AUTHORIZATION after `bonus_conversion`) or Retail's Stage 4H-B2 gate
(still awaiting the Retail-Legal/Business decisions).

**No production code, no migrations, no provider integration, no real
vendor was named or implemented this stage.** `go build ./...` remains
clean (docs-only diff).

## Stage 4H-B0-R7: Final Financial/Bonus Implementation Gate

**Note on this file's own gap, disclosed rather than hidden**: this
document has no dedicated sections for Stage 4H-B0-R5 (Implementation
Readiness and Final P1 Closure) or Stage 4H-B0-R6 (Foundational
Implementation Hardening) — it jumps from Stage 4H-B0-R4 directly to this
R7 entry. `docs/active-stage.md`, `docs/progress.md`, and
`docs/governance/task-registry.md` all correctly carry the full R5/R6
record; this file was simply never updated for those two stages. Not
backfilled as part of this stage — recording R5/R6 retroactively here was
not authorized by this stage's directive — but disclosed here so it is
not silently perpetuated. A future stage should backfill it.

Closed the implementation-blocking financial dependencies Stage 4H-B0-R6
discovered: `player_locked` phase 2, gate G-3 (bonus-funded wagering-
progress farming after a later void/rollback), the Terminal-Grant and
self-exclusion technical contracts, and a formal Human Decision Register.
**No Bonus Engine, Gamification, Reward Orchestrator, or real provider
code was authorized or written this stage.**

**Workstream A — `player_locked` phase 2 (migration `0048`): IMPLEMENTED,
independently reviewed twice over, fixed, and re-verified.** Splits the
ledger account type `player_locked` into
`player_locked_cash`/`player_locked_bonus` (invariant L1, 5-layer
enforcement), adds the HR-9 fail-closed posting guard against
`player_bonus`/`player_locked_bonus` until `bonus_expense` and the Rule
B2 mirror generator both exist, and extends `wallet.GetSummary` with
per-origin balances and an erroring default arm. A real defect
(the pre-flight guard's `SELECT count(*)` was silently inert under
`ledger_accounts`' `FORCE ROW LEVEL SECURITY`) was found and fixed during
implementation; the first fix attempt (toggling `FORCE ROW LEVEL
SECURITY` around the count) was itself found blocking by independent
`security` review (finding S-1: the restore is transaction-local, risking
a silent, permanent loss of tenant isolation on a standalone migration
run) and replaced with a mechanism that is RLS-immune by construction.
`security`, `code-reviewer`, and `qa` each independently reviewed the
implementation (no self-review); all findings routed to one consolidated
fix wave and re-verified. New governance item **HR-15** (not
implemented): a `BEFORE UPDATE` trigger guarding `ledger_accounts`'
identity columns is a required gate before any code posts to a
locked-origin account. Full detail: `docs/active-stage.md`'s Stage
4H-B0-R7 section and `docs/architecture/ledger-accounting-model.md`
§6.5.

**Workstreams B/C/D/E/F — design/validation only, no code authorized.**
Gate G-3 (bonus-funded wagering-progress farming) closed at the design
level via Model C, independently validated by `sportsbook`, `bonus-
engine`, and `architect` — BLOCKED on Stage 4H-B1 authorization, not on
further design. The Terminal-Grant technical contract (gate G-2) and
self-exclusion technical hardening are both design-complete without
selecting their respective human decisions. Sportsbook financial-contract
conformance confirmed consistent (read-only, no sportsbook code). The
Human Decision Register (`docs/decisions/0039-*.md`) formalizes the three
still-unmade human decisions.

**Verification**: `gofmt`, `go build ./...`, `go vet ./...`,
`golangci-lint run` (0 issues) all clean. Full `go test -tags=integration
./...` suite run repeatedly (5+ times) with zero failures. Migration
round-trip confirmed. Commit `17f1057` on `claude/focused-wright-jw88w9`,
pushed, working tree clean.

**B1 readiness: NOT READY.** `player_locked` phase 2's cash-only ledger
capability is implementation-complete, but Bonus Engine, Gamification,
and the Reward Orchestrator remain entirely unbuilt, and three human
decisions (G-2, the self-exclusion default, cashout policy) remain
unmade. **Stage 4H-B1 is NOT authorized by this stage.**

**Update — Stage 4H-B1 Wave 1.5 Fix Wave, Phase 2 (independent
re-verification, 11 specialists) concluded: NOT READY.** Design/
documentation only throughout (no code, no migration beyond `0049`).
None of the four Wave 1.5 P0s (LF-2, SEC-W15-01/02/03) independently
certified closed; four new P0-severity findings surfaced this round,
three inside the fix wave's own output (a reachable ledger-invariant
violation in casino's lock-release logic; the LF-2 fix's own
disposition-resolution step is an ungated self-dealing surface; the
shared `SEP-1` actor≠beneficiary resolver fails open, not closed, on a
partial RLS read); bonus-engine's and casino's two largest fixes were
independently found architecturally incompatible by three reviewers.
No Human Decision Register item was selected or narrowed. Full report:
`docs/governance/wave-1.5-fixwave-phase2-report.md`. Wave 2, CRM,
Affiliate, Gamification, and bonus-funded-wagering implementation
remain unauthorized pending a new human directive scoping a further
fix round.

**Update — Stage 4H-B1 Wave 1.5 Fix Round 2 concluded: READY** for
Wave 2 authorization, subject to two routed, non-blocking P1s (LF-10;
`SEP-1`'s `ancestor_closure` resolver's `agentnetwork` tenant-edge
dependency, awaiting `architect` confirmation). 24 specialist dispatches
across three phases (authorship, independent re-verification, closing
pass) closed all four original P0s and all four Phase-2-discovered new
P0s, each independently certified by a reviewer who did not author the
fix; the composition failure between bonus-engine's and casino's
designs that blocked the prior round was root-caused, fixed, and
re-certified by `architect`. Also delivered the human-directed Product
Surfaces roadmap gate: Stage 6A (Back Office MVP), 6B (Partner Console
MVP, blocked on a not-yet-built partner-scoped RBAC role), 6C (B2C
Brand Frontend MVP), 6D (Retail/POS) — none authorized for
implementation. Design/documentation only throughout (no code, no
migration beyond `0049`, no UI). No Human Decision Register item was
selected. Full report: `docs/governance/wave-1.5-fix-round-2-report.md`.
Wave 2, CRM, Affiliate, Gamification, bonus-funded-wagering, and Back
Office/Partner Console/B2C frontend implementation remain unauthorized
pending a new human directive.

**Update — Stage 4H-B1 Wave 2 (Bonus Engine, real-code implementation)
concluded: READY**, subject to explicitly-open, non-blocking items. The
platform now has a real, tested `internal/bonus` package (Campaign/
Offer/Grant lifecycle, the G-2/AOE mechanism, 5 bonus types, Bonus
Conversion functional end to end, static/pinned targeting, bulk grant
jobs, four-eyes governance), `internal/economicop`
(`EconomicOperationIdentity`), real casino integration (`postWin`/
`postRollback` call the G-2 seams), a landed `bonus_conversion` Risk
Operation, and player/staff HTTP surfaces. Migrations `0050`-`0067`.
Seven real defects (gate bypasses, posting-shape bugs, lock-ordering
and decomposition-vector reopenings) were found and closed during
implementation/review, each with a proven regression test. Open,
non-blocking: LF-10's general case, a KYC-tier taxonomy gap, a
multi-account abuse detector gap (schema-only), missing HTTP admin
surfaces for four-eyes/EOI-root-minting/campaign-activation. Segmentation
(dynamic), CRM, Affiliate, Gamification, real sportsbook, and real
external providers were correctly not implemented. No Human Decision
Register item was selected. Full report:
`docs/governance/wave-2-report.md`. Wave 3, CRM, Affiliate,
Gamification, sportsbook, Retail/POS, and Back Office/Partner
Console/B2C frontend implementation remain unauthorized pending a new
human directive.

**Update — Stage 4H-B1 Wave 3 (Bonus Engine completion, integration
hardening, final financial gate) concluded: READY**, subject to
explicitly-open, non-blocking items and one newly-raised human decision
question. Mandatory first action: `architect`'s from-code reconnaissance
(`docs/governance/wave-3-reconnaissance.md`). 11 specialist phases plus
1 product-owner-proxy dispatch built real deposit/reload/cashback/expiry
sweep jobs, cash-funded wagering-contribution event consumption wired
into `postBet` (bonus-funded/locked-stake staking deliberately not
built), four-eyes application wiring for 4 of 7 `ChangeOperation` types,
and new HTTP/API admin surfaces (API only, no Back Office UI). Migrations
`0068`-`0070`. Twelve real defects found and fixed across the review
chain (two live fail-opens in the wagering path; a multi-account
eligibility gap; five security defects including a subject-set
containment gap and an unbounded EOI-mint budget; two qa-found defects;
two architect-found defects including a live betting-outage vector; one
ledger-finance-found clock-source defect), each with a proven regression
test. The branch was interrupted by a container restart twice mid-Wave;
both times the surviving code was independently re-verified before being
trusted and committed. Open, non-blocking: a latent lock-order inversion
gated on future reachability; a standing-authorization EOI type with no
mint point (inert on the jurisdiction gap); a narrow idempotency-scoping
finding with no live exposure; the bulk-job HTTP-execute path (fail-closed,
non-functional); 3 of 7 `ChangeOperation` types unwired (posting shapes
now specified); the platform-wide jurisdiction-resolver gap; the KYC-tier
taxonomy gap (scheduled, not resolved). CRM, Affiliate, Gamification,
real sportsbook, Retail, Back Office/Partner Console/B2C frontend UI,
and real external providers were correctly not implemented, confirmed by
an explicit sportsbook boundary review. No pre-existing Human Decision
Register item was selected, narrowed, or defaulted; one new item was
raised (not resolved) — whether the platform may ever create a
receivable from a customer via `converted`-Grant cancellation clawback.
Full report: `docs/governance/wave-3-report.md`. Wave 4, CRM, Affiliate,
Gamification, sportsbook, Retail/POS, and Back Office/Partner
Console/B2C frontend implementation remain unauthorized pending a new
human directive.

## Production blockers (summary)

Every item in "Blocked stages" and "External dependencies" above is a
production blocker. The platform today (post-4G-FINAL) has: identity/auth,
wallet/ledger, a mock casino provider, RG self-exclusion foundation,
person-resolution foundation, KYC/document-management foundation
(mock vendor), and a hardened Risk & Limits foundation (licensing-mode
context reachable end to end; jurisdiction context structurally plumbed
but PARTIALLY IMPLEMENTED - no production launch populates it yet; no
real provider/vendor integration anywhere) — a real-money launch
requires resolving every "External dependency" and every numbered "Open
decision" above first.
