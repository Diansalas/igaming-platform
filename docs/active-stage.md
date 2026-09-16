# Active Stage

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
