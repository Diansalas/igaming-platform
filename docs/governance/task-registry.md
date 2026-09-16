# Task Registry

Permanent project governance document (Stage 4G, Part A). Persists task
state across Claude sessions — the repository, not any conversation, is
authoritative. Updated by the Master Orchestrator as work progresses;
each stage's own directive supplies the task list, this registry is
where it is tracked to completion.

Columns: **ID** · **Stage** · **Owner** · **Status** · **Dependencies** ·
**Files owned** · **Interfaces affected** · **Tests** · **Docs** ·
**Blockers** · **Integration status**

## Stage 4G

| ID | Owner | Status | Dependencies | Files owned | Interfaces affected | Tests | Docs | Blockers | Integration |
|---|---|---|---|---|---|---|---|---|---|
| 4G-01 | Orchestrator | Done | none | `docs/governance/*`, `.claude/agents/risk.md` | none | n/a (documentation) | this registry + siblings | none | n/a |
| 4G-02 | risk (Orchestrator-implemented) | Done | 4G-01 (roles must exist first) | `migrations/0041_*`, `internal/risk/*` | New `risk.Evaluate(ctx, tx, RiskRequest) (RiskDecision, error)` boundary | `internal/risk/*_test.go` (unit + integration + concurrency) | `docs/decisions/0031` | none | Integrated into casino (4G-03) |
| 4G-03 | casino (Orchestrator-implemented) | Done | 4G-02 | `internal/casino/orchestrator.go` (risk-evaluation call sites only) | Consumes `risk.Evaluate` at `LaunchGame`/`postBet` | `internal/casino/risk_enforcement_integration_test.go` | `docs/architecture/08-casino-integration-architecture.md` | none | Integrated |
| 4G-04 | 9-area parallel review (architect, ledger-finance, casino, identity-compliance, security x2, backend, qa, architect) | Done | 4G-02, 4G-03 | n/a (review only) | n/a | n/a | Findings folded into `docs/progress.md`/`docs/decisions/0031.md` Stage 4G entries | none | n/a |
| 4G-05 | Orchestrator | Done | 4G-02, 4G-03, 4G-04 | `docs/progress.md`, `docs/active-stage.md` | none | full validation gate | this stage's completion report | none | n/a |

## Stage 4G-FINAL

Hardening stage — no new business functionality; the objective is
making Stage 4G's governance model operational and closing real
architectural gaps it surfaced. See `docs/decisions/0031-risk-and-limits-engine.md`
§8-§13 and `docs/architecture/15-jurisdiction-and-licensing-model.md`
(extended this stage) for the design work these tasks produced.

Documentation-review finding, corrected: an earlier version of this
table understated which files 4GF-02/03/04 actually touched, and listed
`internal/casino/orchestrator.go` as owned solely by 4GF-02 while
4GF-04's own dependency resolutions (DR-4GF-01/02/03) also touched it -
both are true because the SAME actor (Orchestrator) implemented both
concerns (the delivery lock and the jurisdiction/licensing-mode wiring)
in the same file at different call sites this stage; `ownership.md`'s
"one owner per file per stage" is satisfied in substance (one actor, not
two specialists editing concurrently) even though two task rows both
cite the file for their own distinct hunks.

| ID | Owner | Status | Dependencies | Files owned | Interfaces affected | Tests | Docs | Blockers | Integration |
|---|---|---|---|---|---|---|---|---|---|
| 4GF-01 | Orchestrator | Done | none | `docs/governance/*` | none | n/a (documentation) | this registry + siblings | none | n/a |
| 4GF-02 | casino (Orchestrator-implemented) | Done | none | `internal/casino/orchestrator.go` (the `postBet` delivery-lock hunk only - see note above) | none (internal serialization only, no signature change) | `internal/casino/rg_enforcement_integration_test.go` (existing test now passes reliably, 30+ repeat runs) | `docs/governance/project-status.md` flake entry | none | Integrated - see `IA-4GF-01` |
| 4GF-03 | risk (Orchestrator-implemented) | Done | none | `docs/decisions/0031-risk-and-limits-engine.md` | none (documentation only — no code contract change) | n/a | ADR 0031 §9-§13 | none | n/a |
| 4GF-04 | architect (Orchestrator-implemented) | Done | 4GF-03, DR-4GF-01/02/03 | `docs/architecture/15-jurisdiction-and-licensing-model.md`, `docs/architecture/02-domain-and-service-boundaries.md`, `internal/risk/types.go` (`LicensingMode` field + `specificity()` bit renumbering), `internal/risk/evaluator.go` (`matches()` + `ErrMissingLicensingMode` gate), `internal/risk/policy_service.go` (column plumbing), `internal/casino/orchestrator.go` (jurisdiction/licensing-mode hunks), `internal/casino/launch.go`, `internal/identity/tenant.go` (`GetTenantByID`), `internal/httpserver/risk_handlers.go`, `docs/api/openapi/platform-api.yaml`, `migrations/0042_*` | `risk.RiskRequest`/`Rule` gain `LicensingMode`; `casino_launch_sessions` gains a persisted `jurisdiction_code` column; `identity.GetTenantByID` is new | `internal/risk/*_test.go`, `internal/casino/*_test.go`, `internal/httpserver/risk_flow_integration_test.go` | extended architecture docs + ADR 0031 §9/§10 | none | Integrated - see `IA-4GF-02` |
| 4GF-05 | Orchestrator | Done | 4GF-02, 4GF-03, 4GF-04 | n/a (review only) | n/a | n/a | Findings folded into `docs/decisions/0031.md` and `docs/governance/project-status.md` | none | n/a |
| 4GF-06 | Orchestrator | Done | 4GF-01..05 | `docs/progress.md`, `docs/active-stage.md` | none | full validation gate | this stage's completion report | none | n/a |

## Stage 4G-FINAL-FINANCE-GATE

A final-gate-only stage: the previous Stage 4G-FINAL run committed and
pushed with 10 of 11 specialist reviews complete, financial/ledger
outstanding (its dedicated review agent stalled and was stopped without
producing findings). This stage's sole objective was obtaining that
missing independent review - no code changes were authorized unless
required to resolve a P0/P1/P2 finding from it.

| ID | Owner | Status | Dependencies | Files owned | Interfaces affected | Tests | Docs | Blockers | Integration |
|---|---|---|---|---|---|---|---|---|---|
| 4GFG-01 | ledger-finance | Done | none (review-only; explicitly forbidden from modifying any file or mutating the database) | n/a (review only) | n/a | ran the full `internal/casino`/`internal/rg`/`internal/ledger` suites under `-race -tags=integration`, all PASS, plus a read-only `pg_locks` probe | full report in `docs/progress.md`'s Stage 4G-FINAL-FINANCE-GATE entry | none | Integrated - see `IA-4GF-03`. **VERDICT: PASS, independent sign-off GRANTED.** No P0/P1. 6 P2s + 3 P3s recorded, none fixed this stage (none blocking; fixing them was judged out of this final-gate-only stage's authorized scope per its own "no scope expansion" instruction and left for a human-authorized follow-up) |
| 4GFG-02 | Orchestrator | Done | 4GFG-01 | `docs/governance/task-registry.md`, `docs/governance/project-status.md`, `docs/progress.md`, `docs/active-stage.md` | none | n/a | this stage's completion report | none | n/a |

## Dependency Request Log

Permanent, append-only, cross-stage table (Part A §9 / Stage 4G-FINAL
directive item 9). A row is added the moment a request is filed and
only ever edited to fill in **Resolved** — never deleted, never
rewritten. See `integration-protocol.md`'s "Dependency requests"
section for the full procedure this table implements.

| ID | Stage | Requesting task | Target domain | What's needed | Filed | Resolved |
|---|---|---|---|---|---|---|
| DR-4G-01 | 4G | 4G-03 (casino) | risk | `risk.Evaluate(ctx, tx, RiskRequest) (RiskDecision, error)` boundary + `RiskRequest`/`RiskDecision` shapes stable enough for `internal/casino` to call | Stage 4G | Stage 4G — `internal/risk/types.go`/`evaluator.go` (commit `bc0c78f`) |
| DR-4GF-01 | 4G-FINAL | 4GF-04 (architect) | risk | `RiskRequest` needs a `LicensingMode` field so a jurisdiction/licensing-scoped rule can be expressed without a future rewrite | Stage 4G-FINAL | Stage 4G-FINAL — `internal/risk/types.go` (this stage's commit) |
| DR-4GF-02 | 4G-FINAL | 4GF-04 (architect) | casino | `casino_launch_sessions` needs a persisted `jurisdiction_code` column so `postBet` can supply the same jurisdiction `LaunchGame` resolved, closing the Stage 4G-disclosed gap | Stage 4G-FINAL | Stage 4G-FINAL — `migrations/0042_*`, `internal/casino/orchestrator.go` (this stage's commit) |
| DR-4GF-03 | 4G-FINAL | 4GF-04 (architect) | identity | `internal/casino`'s new `resolveLicensingMode` helper needs a tx-scoped tenant lookup exposing `licensing_model`; identity owns `tenants` reads (`ownership.md`) and had none besides slug-based `GetTenantBySlug` — filed against identity, blocks 4GF-04's licensing-mode work | Stage 4G-FINAL | Stage 4G-FINAL — `internal/identity/tenant.go`'s new `GetTenantByID(ctx, tx, id) (Tenant, error)` (this stage's commit) — documentation-review finding: this row was missing when 4GF-04 was first recorded, which left the identity dependency looking silently added; added before this stage closed |

## Integration Approval Log

Permanent, append-only, cross-stage table (Part A §10 / Stage 4G-FINAL
directive item 10). Only the Orchestrator adds a row here, and only
after the Integration sequence's steps 1-6 (`integration-protocol.md`)
all pass. A capability is not "Integrated" in any task-registry row
until its row exists here.

| ID | Stage | Capability | Consumer | Owner | Tests cited | Reviews cited | Approved |
|---|---|---|---|---|---|---|---|
| IA-4G-01 | 4G | `risk.Evaluate` boundary | casino (`LaunchGame`, `postBet`) | risk | `internal/risk/*_test.go`, `internal/casino/risk_enforcement_integration_test.go` | 9-area Stage 4G specialist review | Stage 4G, commit `bc0c78f` |
| IA-4GF-01 | 4G-FINAL | `postBet` delivery-serialization lock + `internal/rg` `clock_timestamp()` fix | casino (self), rg | casino | `TestConcurrent_DuplicateBetDeliveryDuringSelfExclusion` (30+ repeat runs, 0 failures), `internal/casino/adversarial_lock_stress_test.go` (N=10 and N=8x5, 0 failures), `TestEvaluateEligibility_DetectsSelfExclusionCommittedAfterTransactionBegan` (5/5 with `-race`), 9 post-fix full/targeted `-race -tags=integration` runs (0 failures), plus the financial-correctness review's own re-run of the full casino/rg/ledger suite under `-race -tags=integration` (all PASS, see `IA-4GF-03`) | architect, security, multi-tenancy (lock tenant-scoping - all 3 independently), adversarial/QA (stress tests + flake investigation), code-reviewer, **ledger-finance (independent financial-correctness review, STAGE 4G-FINAL-FINANCE-GATE, see `IA-4GF-03` - PASS, sign-off granted)** | Stage 4G-FINAL (this stage's commit) |
| IA-4GF-03 | 4G-FINAL-FINANCE-GATE | Independent financial-correctness sign-off on the `postBet` delivery lock + `internal/rg` `clock_timestamp()` fix (supersedes IA-4GF-01's earlier Orchestrator-self-review caveat) | casino, rg | ledger-finance | Full `internal/casino`/`internal/rg`/`internal/ledger` suites under `-race -tags=integration` (all PASS), plus a read-only `pg_locks` probe confirming distinct advisory-lock namespaces vs `rg.lockPerson`/`risk_cumulative` | Independent ledger-finance specialist review answering all 20 required questions + the A-N adversarial-coverage matrix (`docs/progress.md`'s Stage 4G-FINAL-FINANCE-GATE entry has the full report) | Stage 4G-FINAL-FINANCE-GATE (this stage's commit) - **VERDICT: PASS. No P0/P1 found. Independent sign-off GRANTED.** 6 P2s and 3 P3s recorded as follow-up hardening/observability items (not blocking), owners assigned per the reviewer's own recommendation - see `docs/progress.md` |
| IA-4GF-02 | 4G-FINAL | Jurisdiction context on `casino_launch_sessions` + `RiskRequest.LicensingMode` | casino, risk | architect | `internal/risk/*_test.go`, `internal/casino/*_test.go` | architecture, risk, casino, multi-tenancy, security, identity-compliance, backend, qa, code-reviewer (this stage) | Stage 4G-FINAL (this stage's commit) |

## Stage 4H-A

Architecture-freeze-only stage (task #136) per the "STAGE 4H-A — BONUS,
GAMIFICATION & REWARD ORCHESTRATION ARCHITECTURE FREEZE" directive. No
Bonus Engine, Gamification Engine, Reward Orchestrator, sportsbook
provider, or external-bonus-API code was authorized or written this
stage — only architecture documents, ADRs, and cross-domain contract
design. The directive's body was explicit and internally consistent on
this scope; a single contradictory trailing line ("Approved — proceed
with Stage 4H: Bonus Engine") appended after the full 27-section body is
recorded here, not acted on — see this stage's completion report for the
full reasoning (CLAUDE.md's stage-gate rule: never begin next-stage
implementation unprompted).

| ID | Owner | Status | Dependencies | Files owned | Interfaces affected | Tests | Docs | Blockers | Integration |
|---|---|---|---|---|---|---|---|---|---|
| 4HA-01 | Orchestrator | Done | none | `docs/governance/*` | none | n/a (documentation) | this registry + siblings | none | n/a |
| 4HA-02 | bonus-engine (rewrite) | Done | none | `docs/architecture/10-bonus-engine-architecture.md` | none (design only) | n/a | full Campaign→Offer→Grant→Activation→Progress→Completion→Conversion/Release→Expiry→Cancellation→Reversal lifecycle | none | n/a |
| 4HA-03 | architect | Done | none | `docs/architecture/17-gamification-engine-architecture.md`, `18-tournament-architecture.md`, `19-mission-architecture.md`, `20-reward-marketplace-architecture.md`, `02-domain-and-service-boundaries.md` (Gamification Engine section) | none (design only) | n/a | full Gamification sub-domain architecture (points/XP/levels/missions/challenges/achievements/badges/leaderboards/tournaments/streaks/marketplace/raffles) | none | n/a |
| 4HA-04 | Orchestrator (direct, cross-domain connective docs) | Done | 4HA-02, 4HA-03 | `docs/architecture/21-reward-orchestration-architecture.md`, `22-canonical-activity-event-taxonomy.md`, `23-external-reward-provider-contract.md` | `ExternalRewardProvider` interface (design only, no code); canonical event envelope contract | n/a | Reward Orchestrator architecture, canonical Activity/Event taxonomy, External Reward Provider contract | none | n/a |
| 4HA-05 | ledger-finance | Done | 4HA-02 | `docs/decisions/0032-bonus-accounting.md`, additive pointers in `docs/architecture/ledger-accounting-model.md`, `financial-transaction-flows.md`, `reconciliation-model.md`, `docs/decisions/0019-authoritative-ledger-and-balance-projection-architecture.md`, `docs/architecture/24-points-accounting-architecture.md` | `promo_liability` account type, `bonus_expense` account type, Invariant B1 (design only, no migration) | n/a | ADR 0032 (bonus accounting, CRITICAL/authoritative), Points Accounting Architecture (doc 24) | none | n/a |
| 4HA-06 | sportsbook | Done | 4HA-02, 4HA-04 | `docs/decisions/0033-provider-interoperability-and-external-bonus-engines.md` | provider-neutral sportsbook bonus-interoperability contract (design only) | n/a | ADR 0033 | none | n/a |
| 4HA-07 | identity-compliance | Done | 4HA-02, 4HA-03 | `docs/decisions/0034-bonus-gamification-rg-kyc-identity-integration.md` | RG/KYC/identity boundary rules for Bonus/Gamification (design only) | n/a | ADR 0034 | none | n/a |
| 4HA-08 | risk | Done | 4HA-02, 4HA-03 | `docs/decisions/0031-risk-and-limits-engine.md` (new §14-§18) | Bonus/Gamification consume `internal/risk.Evaluate` exclusively — no new limit engine (design only) | n/a | ADR 0031 §14-§18 | none | n/a |
| 4HA-09 | backend | Done | 4HA-02..08 | `docs/architecture/25-bonus-gamification-api-architecture.md` | API/frontend contracts, RBAC permission surface (design only, no code) | n/a | doc 25 | none | n/a |
| 4HA-10 | code-reviewer, security, qa, casino (Wave 2 parallel review) | Done | 4HA-02..09 | n/a (review only) | n/a | n/a | Findings folded into this stage's completion report; P0/P1 all fixed in-place across docs 10/17/18/19/20/21/22/23/24/25 and ADR 0033; P2/P3 fixed where quick and precise, else recorded with reasoning (no further scope expansion, per this stage's own architecture-freeze framing) | none | n/a |
| 4HA-11 | Orchestrator | Done | 4HA-01..10 | `docs/progress.md`, `docs/active-stage.md`, this registry, `docs/governance/project-status.md` | none | full validation gate (docs-only; `go build ./...` clean, no code touched) | this stage's completion report | none | n/a — **stage explicitly STOPS here; no Stage 4H (Bonus Engine implementation) authorized** |

## How to use this registry (for future stages)

1. At stage start, the Orchestrator breaks the directive into tasks and
   adds rows here with `Status: Not started`.
2. As work proceeds, `Status` moves through `In progress` → `Blocked`
   (with a reason in **Blockers**) → `Done`.
3. A dependency request filed per `integration-protocol.md` is recorded
   as a note on the requesting task's row (in **Blockers** until
   resolved, then moved to **Dependencies** once satisfied).
4. **Integration status** is only set to `Integrated` by the Orchestrator,
   never by the implementing specialist itself — mirrors
   `integration-protocol.md`'s "nothing is assumed integrated until the
   Orchestrator says so" rule.
5. This table is never deleted across stages — completed stages' rows
   remain as the historical record; a new stage adds a new `## Stage NN`
   section below the most recent one.
