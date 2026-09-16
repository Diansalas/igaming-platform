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
| 4G-FINAL | Architectural hardening: operational governance, jurisdiction/licensing context, flake root-cause fix (10/11 specialist reviews complete - financial/ledger outstanding, see below) | Complete (this stage) |

## Active stage

Stage 4G-FINAL — see `docs/active-stage.md` for full detail.

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
