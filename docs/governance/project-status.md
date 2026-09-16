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
| 4G-FINAL-FINANCE-GATE | Independent financial-correctness sign-off on the `postBet` lock + `internal/rg` fix (final-gate-only, no business functionality) | Complete (this stage) |

## Active stage

Stage 4G-FINAL-FINANCE-GATE — see `docs/active-stage.md` for full detail.

## Blocked stages

- **Bonus Engine**: explicitly blocked (Stage 4G §32) until the Risk &
  Limits architecture is stable enough for Bonus to consume without
  building its own independent limit engine. Not started.
- **Sportsbook**: not started — no directive has authorized it yet.
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
