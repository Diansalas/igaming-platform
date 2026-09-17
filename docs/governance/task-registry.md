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
| 4HA-11 | Orchestrator | Done | 4HA-01..10 | `docs/progress.md`, `docs/active-stage.md`, this registry, `docs/governance/project-status.md` | none | full validation gate (docs-only; `go build ./...` clean, no code touched) | this stage's completion report | none | n/a |
| 4HA-12 | ledger-finance (independent financial sign-off review) | Done | 4HA-01..11 | n/a (review only) | n/a | n/a | 7 P1 + 5 P2 findings, all applied across ADR 0032, `financial-transaction-flows.md`, docs 10/20/21/23 (this stage's second commit) | none | **VERDICT: PASS WITH FINDINGS, sign-off GRANTED once the 7 P1s were applied.** No P0. All 7 P1s fixed in-place; 4 of 5 P2s fixed, 1 P2 + 1 P3 recorded (doc 17's `PointType` conceptual-shape drift, cosmetic) |
| 4HA-13 | product-owner-proxy (scope-discipline review) | Done | 4HA-01..11 | n/a (review only) | n/a | n/a | Findings applied to `docs/architecture/14-mvp-scope-and-roadmap.md`'s "Features deliberately deferred" section (this stage's second commit) | none | No P0/P1/P2 (scope findings, not correctness). Top finding: the Gamification Engine/Reward Marketplace/Reward Orchestrator domain (docs 17-21) has no anchor in the Blueprint or in this project's own prior MVP roadmap — recorded as deferred scope, not built, per this stage's architecture-freeze-only framing. Reward Orchestrator specifically flagged as premature abstraction (a three-domain-ready fulfillment layer built ahead of a second concrete reward-producing domain) |
| 4HA-14 | Orchestrator | Done | 4HA-12, 4HA-13 | `docs/progress.md`, `docs/active-stage.md`, this registry, `docs/governance/project-status.md` | none | full validation gate (docs-only; `go build ./...` clean, no code touched) | this stage's completion report (revised) | none | n/a — **stage explicitly STOPS here; no Stage 4H (Bonus Engine implementation) authorized** |

## Stage 4H-B0

Architecture/scope-freeze stage, per the "STAGE 4H-B0 — BONUS +
GAMIFICATION + RETAIL ARCHITECTURE/SCOPE FREEZE" directive, responding to
a new confirmed business requirement: the platform must support retail
iGaming operations (a configurable agent-hierarchy network — Operator →
Partner → Super Agent → Agent → Player/Cashier, configurable per
tenant/licence/jurisdiction) as another surface of the same platform,
sharing identity/wallet/ledger/risk/RG/payments/reporting/audit/bonus/
tenant architecture wherever appropriate. **No production code, no
migrations, and no implementation were started.**

| ID | Owner | Status | Dependencies | Files owned | Interfaces affected | Tests | Docs | Blockers | Integration |
|---|---|---|---|---|---|---|---|---|---|
| 4HB0-01 | Orchestrator | Done | none | `docs/governance/*` | none | n/a (documentation) | this registry + siblings | none | n/a |
| 4HB0-02 | architect | Done | none | `docs/architecture/26-retail-operations-architecture.md` (new), `02-domain-and-service-boundaries.md` (Retail/Agent Network section) | Configurable hierarchy model (adjacency list authoritative + closure-table derived projection), cashier/terminal actor model, retail registration attribution (`retail_player_origins`), money-movement authorization shape (design only) | n/a | Retail core architecture, 9-area conflict-check table | none | 3 P0 findings disclosed (node-float/`Wallet` conflict, licensing gap, anonymous-play structural risk) — all deferred to human/cross-specialist sign-off, none resolved unilaterally |
| 4HB0-03 | ledger-finance | Done | 4HB0-02 | `docs/decisions/0035-retail-agent-network-accounting.md` (new) | Agent float as platform liability, 3 new account types, Invariants R1-R3, retail deposit/withdrawal postings, commission accounting (design only) | n/a | ADR 0035 | none | 1 P0 disclosed (`ledger_accounts` owner-family gap for node-scoped accounts, needs architect+security+human) |
| 4HB0-04 | security | Done | 4HB0-02, 4HB0-03 | `docs/decisions/0036-retail-hierarchy-rbac-and-audit.md` (new) | Three-axis authorization, closure-table RLS (fail-closed by construction), cashier/terminal identity, audit extension, RG/KYC non-bypass structure (design only) | n/a | ADR 0036, 36-test mandatory spec | none | 5 P0 design invariants stated (binding requirements on future implementation, not existing bugs); 1 P1 (existing `audit_log` RLS doesn't survive retail as-is, deferred to a fresh security review of the eventual migration) |
| 4HB0-05 | identity-compliance | Done | 4HB0-02 | `docs/architecture/05-identity-architecture.md`, `11-kyc-aml-rg-architecture.md` (Stage 4H-B0 addenda) | Retail registration provenance, cashier identity-linkage, retail KYC/AML/RG non-bypass (design only) | n/a | doc 05/11 additions | none | 2 P0 disclosed (anonymous-play/RG-KYC-Risk structural unsatisfiability if permitted; POS-offline fail-open risk if not deliberately fail-closed) |
| 4HB0-06 | payments | Done | 4HB0-02, 4HB0-03 | `docs/architecture/07-payments-architecture.md` (Retail cash rail section) | Retail cash as a fulfillment channel (not a `PaymentProvider`), deposit/withdrawal procedural flow, terminal-as-client boundary (design only) | n/a | doc 07 addition | none | none at landing (2 P1s found and fixed in Wave-2 review: RG order, two-principal auth) |
| 4HB0-07 | risk | Done | 4HB0-02 | `docs/decisions/0031-risk-and-limits-engine.md` §19-24 | Retail limit integration via `internal/risk`, two new scope dimensions, aggregate-exposure gap disclosure (design only) | n/a | ADR 0031 §19-24 | none | 2 P0 disclosed (anonymous-play RG/KYC/Risk unsatisfiability; delegated limit-authoring self-defeat risk); 1 P1 (daily/periodic hierarchy-level funding limit not yet expressible — no node-keyed cumulative aggregation) |
| 4HB0-08 | data-analytics | Done | 4HB0-02 | `docs/architecture/12-audit-reporting-architecture.md` (Stage 4H-B0 section) | Hierarchy-scoped reporting, one shared pipeline for online+retail (design only) | n/a | doc 12 addition | none | none at landing (1 P2 found and fixed in Wave-2 review: hard-coded per-level scope ladder) |
| 4HB0-09 | backend | Done | 4HB0-02..07 | `docs/architecture/04-api-architecture.md` (Stage 4H-B0 section) | Retail/POS + hierarchy-management API surface (design only) | n/a | doc 04 addition | none | 3 P1s found and fixed in Wave-2 review (isolation-mechanism claim, JWT-claim scope, `staff_users` column) |
| 4HB0-10 | qa | Done | 4HB0-02..09 | `docs/testing/testing-strategy.md` (Stage 4H-B0 section) | Retail financial/isolation/RG-bypass test strategy (design only) | n/a | testing-strategy.md addition | none | none |
| 4HB0-11 | bonus-engine | Done | none (independent of retail) | `docs/architecture/10-bonus-engine-architecture.md` (Stage 4H-B0 section) | Bonus Engine MVP implementation-scope plan: 5-type first slice, migration order, Stage 4G §32 gate check (qualified-lifted) | n/a | doc 10 addition | none | none |
| 4HB0-12 | code-reviewer (Wave-2 cross-document consistency review) | Done | 4HB0-02..11 | n/a (review only) | n/a | n/a | 14 findings (F1-F14: 1 P0, 8 P1, 4 P2, 1 consolidated P3 list), all P0/P1/P2 fixed in-place across docs 04/05/07/11/12/26 and ADR 0031/0035/0036 (this stage's Wave-2 commit) | none | **VERDICT: 1 P0 + 8 P1 genuine cross-document contradictions found, all fixed.** Mirrors Stage 4H-A's Wave-2 pattern at larger scale. Full list in `docs/architecture/27-stage-4h-b0-scope-and-implementation-plan.md` §22a |
| 4HB0-13 | product-owner-proxy (scope-discipline review) | Done | 4HB0-02..11 | n/a (review only) | n/a | n/a | Blueprint-anchor finding independently confirmed (zero retail content); concrete MVP/deferred scope split provided; 1 scope-creep finding (ADR 0035 §5's commission machinery downgraded from RESOLVED to documented-not-binding pending commercial terms) | none | No P0/P1/P2 (scope findings). Findings applied to ADR 0035 §5 status and `docs/architecture/27-*` §1.3/§2 |
| 4HB0-14 | Orchestrator | Done | 4HB0-01..13 | `docs/architecture/27-stage-4h-b0-scope-and-implementation-plan.md` (new, master synthesis), `docs/progress.md`, `docs/active-stage.md`, this registry, `docs/governance/project-status.md`, `docs/governance/ownership.md` | none | full validation gate (docs-only; `go build ./...` clean, no code touched) | this stage's completion report | none | n/a — **stage explicitly STOPS here; no Stage 4H-B1/4H-B2/4H-B3 (implementation) or "Retail-Legal" workstream authorized** |

## Stage 4H-B0-R1

Correction/finalization stage over the Stage 4H-B0 architecture set,
responding to a directive that identified specific errors in the Stage
4H-B0 completion report (a bonus-gate contradiction, a misclassified
retail P0 list, and several items needing formalization). **No
production code, no migrations, and no implementation were started.**

| ID | Owner | Status | Dependencies | Files owned | Interfaces affected | Tests | Docs | Blockers | Integration |
|---|---|---|---|---|---|---|---|---|---|
| 4HB0R1-01 | Orchestrator | Done | none | `docs/governance/*` | none | n/a (documentation) | this registry + siblings | none | n/a |
| 4HB0R1-02 | risk | Done | none | `docs/decisions/0031-risk-and-limits-engine.md` (new §16a) | none (documentation only) | n/a | verified `bonus_conversion` NOT STARTED (0/6 ADR 0031 §16 steps) directly against repository state | none | n/a |
| 4HB0R1-03 | ledger-finance | Done | none | `docs/decisions/0021-multi-asset-accounting.md` (new "Rounding and precision" section), `docs/decisions/0035-retail-agent-network-accounting.md` (new §1.3.1) | none (documentation only, illustrative SQL, no migration) | n/a | ADR 0021 rounding-decision enumeration (3 questions, 6 neutral direction options, none selected); ADR 0035 agent-float/ADR 0007 formalization + minimum additive schema amendment draft, corrected a load-bearing error in the original scope-freeze draft (house-level index predicate collision) | none | Amendment explicitly `NOT IMPLEMENTED`, requires architect + security + human sign-off before any migration |
| 4HB0R1-04 | architect | Done | 4HB0R1-03 | `docs/decisions/0035-retail-agent-network-accounting.md` (new §1.3.2, review only) | none | n/a | Reviewed ledger-finance's schema amendment: sound with caveats; confirmed `hierarchy_nodes` (doc 26) has `id`/`tenant_id`/`status`; confirmed generic-hierarchy-model and shared-platform-model statements accurate; flagged `ledger-accounting-model.md`/ADR 0032 §2 shorthand as required follow-up | none | Review only, not approval — §1.3 remains an `OPEN DECISION` |
| 4HB0R1-05 | security | Done | 4HB0R1-03 | `docs/decisions/0035-retail-agent-network-accounting.md` (new subsection, review only) | none | n/a | Reviewed RLS compatibility, composite-FK tenant-isolation safety, and the widened index predicate's collision fix | none | Review only, not approval — §1.3 remains an `OPEN DECISION` |
| 4HB0R1-06 | Orchestrator | Done | 4HB0R1-01..05 | `docs/architecture/27-stage-4h-b0-scope-and-implementation-plan.md` (corrections across §1.1, §1.3a new, §6, §8, §9, §9a new, §22, §23A/§23B split, §24, §25), `docs/progress.md`, `docs/active-stage.md`, this registry, `docs/governance/project-status.md` | none | full validation gate (docs-only; `go build ./...` clean, no code touched) | this stage's completion report | none | n/a — **stage explicitly STOPS here; no Stage 4H-B1 or 4H-B2/4H-B3 authorized** |

## Stage 4H-B0-R2

Financial-gate clarification stage. Purpose: close the remaining
financial-design gate for Bonus implementation by preparing an exact
human decision package for ADR 0021's rounding decision and verifying
the remaining `bonus_conversion` Risk dependency and every other
financial-gate area. **No production code, no migrations, and no
implementation were started.**

| ID | Owner | Status | Dependencies | Files owned | Interfaces affected | Tests | Docs | Blockers | Integration |
|---|---|---|---|---|---|---|---|---|---|
| 4HB0R2-01 | Orchestrator | Done | none | `docs/governance/*` | none | n/a (documentation) | this registry + siblings | none | n/a |
| 4HB0R2-02 | ledger-finance | Done | none | none (analysis reported to Orchestrator, no file edits) | none | n/a | numerical worked examples, debit/credit-balance-safety confirmation for all rounding options, PostgreSQL implicit-cast trap flagged — folded into doc 28 by the Orchestrator | none | n/a |
| 4HB0R2-03 | bonus-engine | Done | none | none (analysis reported to Orchestrator, no file edits) | none | n/a | precise per-bonus-type rounding-dependency table; independently confirmed all five types reach `completed → converted` — folded into doc 28 | none | n/a |
| 4HB0R2-04 | risk | Done | none | none (analysis reported to Orchestrator; ADR 0031 §16a re-verified, left unmodified since still accurate) | none | n/a | re-verified `bonus_conversion` NOT STARTED against current repository state; formatted six-step checklist — folded into doc 28 | none | n/a |
| 4HB0R2-05 | architect | Done | none | none (analysis reported to Orchestrator, no file edits) | none | n/a | focused 12-area financial-gate review; no additional P0/P1 blocker found | none | n/a |
| 4HB0R2-06 | security | Done | none | none (analysis reported to Orchestrator, no file edits) | none | n/a | rounding-rule config-security requirements (future manage permission + audit logging); confirmed no bypass at the `bonus_conversion` enforcement point; one minor non-blocking audit-table gap flagged | none | n/a |
| 4HB0R2-07 | qa | Done | none | none (analysis reported to Orchestrator, no file edits) | none | n/a | confirmed core financial test matrix already mapped to bonus scenarios; designed rounding-determinism test approach; flagged non-blocking test-plan additions | none | n/a |
| 4HB0R2-08 | Orchestrator | Done | 4HB0R2-01..07 | `docs/architecture/28-bonus-financial-gate-decision-sheet.md` (new), `docs/governance/project-status.md`, `docs/active-stage.md`, this registry, `docs/progress.md` | none | full validation gate (docs-only; `go build ./...` clean, no code touched) | this stage's completion report | none | n/a — **stage explicitly STOPS here; Stage 4H-B1 NOT authorized** |

## Stage 4H-B0-R3

Financial-gate closure stage. Purpose: validate the human's proposed
answers to ADR 0021's rounding questions (DS-1/DS-2/DS-3) against the
existing architecture, record them if safe, re-verify the
`bonus_conversion` Risk dependency, and analyze (not implement) a new
confirmed Asset/Currency Registry + FX/Conversion architecture
requirement. **No production code, no migrations, and no implementation
were started.**

| ID | Owner | Status | Dependencies | Files owned | Interfaces affected | Tests | Docs | Blockers | Integration |
|---|---|---|---|---|---|---|---|---|---|
| 4HB0R3-01 | Orchestrator | Done | none | `docs/governance/*` | none | n/a (documentation) | this registry + siblings | none | n/a |
| 4HB0R3-02 | ledger-finance | Done | none | none (analysis reported to Orchestrator, no file edits) | none | n/a | validated DS-1/DS-2 numerically, exact algorithm specification, cashback-residual finding, zero-rounding-result edge case — folded into ADR 0021 by the Orchestrator | none | n/a |
| 4HB0R3-03 | bonus-engine | Done | none | none (analysis reported to Orchestrator, no file edits) | none | n/a | per-bonus-type DS-2 validation, wagering-requirement/contribution-weighting monetary distinction, cap-rounding-order clarification, Coupon scoping question — folded into ADR 0021 | none | n/a |
| 4HB0R3-04 | risk | Done | none | none (analysis reported to Orchestrator; ADR 0031 §16a re-verified, left unmodified) | none | n/a | confirmed Risk evaluates only the post-rounded amount by construction; re-verified `bonus_conversion` unchanged, NOT STARTED | none | n/a |
| 4HB0R3-05 | architect | Done | none | none (analysis reported to Orchestrator, no file edits) | none | n/a | validated DS-3, confirmed no other cross-document contradiction, focused 12-area financial-gate re-review, Asset/Currency Registry + FX architecture analysis | none | n/a |
| 4HB0R3-06 | security | Done | none | none (analysis reported to Orchestrator, no file edits) | none | n/a | rounding-decision determinism/reconstructability confirmation, exact rounding-rule storage-location specification, Asset Registry authorization/audit requirement | none | n/a |
| 4HB0R3-07 | qa | Done | none | none (analysis reported to Orchestrator, no file edits) | none | n/a | multi-asset genericity validation across 0/2/6/8/18-decimal exponents, refined 9-category test-plan design, confirmed no Bonus MVP expansion | none | n/a |
| 4HB0R3-08 | Orchestrator | Done | 4HB0R3-01..07 | `docs/decisions/0021-multi-asset-accounting.md` (rounding decision recorded), `docs/architecture/27-stage-4h-b0-scope-and-implementation-plan.md` (§1.1/§22/§24/§25 updated, new §26), `docs/architecture/28-bonus-financial-gate-decision-sheet.md` (marked RESOLVED), `docs/architecture/financial-domain-model.md`, `docs/architecture/14-mvp-scope-and-roadmap.md`, `docs/governance/project-status.md`, `docs/active-stage.md`, this registry, `docs/progress.md` | none | full validation gate (docs-only; `go build ./...` clean, no code touched) | this stage's completion report | none | n/a — **stage explicitly STOPS here; Stage 4H-B1 NOT started, READY FOR HUMAN AUTHORIZATION after `bonus_conversion`** |

## Stage 4H-B0-R4

Architecture-only closure stage. Purpose: close the Asset/Currency
Registry requirement doc 27 §26 deferred, and formalize a new mandatory
dual-mode (external provider + in-house engine) Sportsbook architecture.
**No production code, no migrations, no provider integration, no real
vendor named.**

| ID | Owner | Status | Dependencies | Files owned | Interfaces affected | Tests | Docs | Blockers | Integration |
|---|---|---|---|---|---|---|---|---|---|
| 4HB0R4-01 | Orchestrator | Done | none | `docs/governance/*` | none | n/a (documentation) | this registry + siblings | none | n/a |
| 4HB0R4-02 | architect | Done | none | `docs/decisions/0037-asset-currency-registry-and-fx-conversion-architecture.md` (new) | none (design only) | n/a | Asset Registry 8-layer authorization model, FX/Conversion 4-component architecture with 8-condition fail-closed rule, canonical `AssetAuthorization.CheckEligibility` service | none | Resolves Stage 4H-B0-R3's flagged authorization-boundary P1 |
| 4HB0R4-03 | sportsbook | Done | none | `docs/architecture/09-sportsbook-architecture.md` (rewritten) | none (design only) | n/a | canonical sportsbook domain model, `SportsbookProvider`/`DataFeedProvider` abstractions, five-layer in-house engine, mode-selection routing | none | Superseded Stage-0 external-only recommendation with dual-mode architecture; original recommendation preserved as operational sequencing guidance |
| 4HB0R4-04 | ledger-finance | Done | none | `docs/decisions/0038-sportsbook-accounting-and-ledger-integration.md` (new) | none (design only) | n/a | sportsbook financial/ledger posting contract, 6 new transaction types, idempotency key design | none | n/a |
| 4HB0R4-05 | risk | Done | none | `docs/decisions/0031-risk-and-limits-engine.md` (new §25-31) | none (design only) | n/a | sportsbook Risk integration, Operation proposals, trading-vs-Risk boundary | none | n/a |
| 4HB0R4-06 | identity-compliance | Done | none | `docs/decisions/0034-bonus-gamification-rg-kyc-identity-integration.md` (new §9-13) | none (design only) | n/a | sportsbook RG integration; flagged open-bet self-exclusion policy as genuine human/compliance decision, not decided unilaterally | none | n/a |
| 4HB0R4-07 | architect (independent review) | Done | 4HB0R4-02..06 | `docs/decisions/0033-provider-interoperability-and-external-bonus-engines.md` (2 narrow fixes) | none | n/a | cross-document consistency review across 4HB0R4-03..06 (did not review own 4HB0R4-02); found the ADR 0038 rollback-netting contradiction, routed to `ledger-finance`; resolved 2 open questions in ADR 0031 §27/§30 against sportsbook's final model, routed to `risk`; re-verified extensibility items 10-18 | none | n/a |
| 4HB0R4-08 | ledger-finance (independent review) | Done | 4HB0R4-02 | none (analysis reported to Orchestrator, no file edits) | none | n/a | independent financial-correctness review of ADR 0037 (not own ADR 0038); found `rounding_rule_id` field-mapping gap, tenant/wallet-context sourcing question, rate-plausibility gap, mischaracterized ADR 0031 §8 precedent — folded into ADR 0037 by the Orchestrator | none | n/a |
| 4HB0R4-09 | security (independent review) | Done | 4HB0R4-02..06 | none (analysis reported to Orchestrator, no file edits) | none | n/a | 5 fail-closed/error-contract/audit gaps found — folded into ADR 0037 by the Orchestrator; confirmed provider-callback authentication, tenant isolation, audit coverage | none | n/a |
| 4HB0R4-10 | qa (independent review) | Done | 4HB0R4-02..06 | none (analysis reported to Orchestrator, no file edits) | none | n/a | full 20-item extensibility test, all YES; adversarial idempotency-collision and authorization-widening test-plan designs | none | n/a |
| 4HB0R4-11 | ledger-finance (follow-up correction) | Done | 4HB0R4-07 | `docs/decisions/0038-sportsbook-accounting-and-ledger-integration.md` | none | n/a | fixed the rollback-netting contradiction; disambiguated "Cancellation" terminology; added ADR 0037 cross-reference; recorded a non-blocking settlement-side mapping gap | none | n/a |
| 4HB0R4-12 | risk (follow-up correction) | Done | 4HB0R4-07 | `docs/decisions/0031-risk-and-limits-engine.md` | none | n/a | closed the two open questions in §27/§30 using sportsbook's final domain model | none | n/a |
| 4HB0R4-13 | Orchestrator | Done | 4HB0R4-01..12 | `docs/decisions/0037-*.md` (5 security-flagged fixes applied), `docs/governance/project-status.md`, `docs/active-stage.md`, this registry, `docs/progress.md` | none | full validation gate (docs-only; `go build ./...` clean, no code touched) | this stage's completion report | none | n/a — **stage explicitly STOPS here; no implementation authorized** |

## Stage 4H-B0-R5

Implementation-readiness closure stage. Purpose: close the five P1s Stage
4H-B0-R4 disclosed and reach a genuine implementation-readiness verdict.
**No production code, no migrations, no provider integration.**

| ID | Owner | Status | Dependencies | Files owned | Interfaces affected | Tests | Docs | Blockers | Integration |
|---|---|---|---|---|---|---|---|---|---|
| 4HB0R5-01 | Orchestrator | Done | none | `docs/governance/*` | none | n/a (documentation) | this registry + siblings | none | n/a |
| 4HB0R5-02 | architect | Done | none | `docs/decisions/0037-*.md` (new §B.7, §C.5), `docs/architecture/09-*.md` (new §2.5) | none (design only) | n/a | FX rate-plausibility 10-failure-mode classification + 9th fail-closed condition; Asset Authorization admin API (9 operations, four-eyes reasoning, immutable/mutable field split); sportsbook canonical-identity clarification | none | Closes P1-1, P1-2 |
| 4HB0R5-03 | ledger-finance | Done | none | `docs/decisions/0038-*.md` (new §14), `docs/architecture/ledger-accounting-model.md` (new §6.3) | none (design only) | n/a | idempotency contract (`occurrence_ordinal`, no schema change) closing P1-3; `player_locked` origin-split PROPOSAL (Shape A), explicitly not self-approved | none | Closes P1-3; P1-4 is proposal-only pending independent review |
| 4HB0R5-04 | identity-compliance | Done | none | `docs/decisions/0034-*.md` (new §14) | none (design only) | n/a | `OpenBetSelfExclusionPolicy` configurable architecture; explicitly did not select the platform-wide default value | none | Closes P1-5 (default value remains human decision) |
| 4HB0R5-05 | sportsbook | Done | none | `docs/architecture/09-*.md` (new §15) | none | n/a | external-first vs. in-house-first sequencing recommendation (business/engineering, not architectural constraint) | none | n/a |
| 4HB0R5-06 | sportsbook (independent review) | Done | 4HB0R5-03 | none (analysis reported to Orchestrator, no file edits this round) | none | n/a | reviewed P1-4 proposal from sportsbook angle: approved Shape A; found the worked cases proved lock-time split but never worked through unlock-side cases | none | n/a |
| 4HB0R5-07 | architect (independent review) | Done | 4HB0R5-03 | none (analysis reported to Orchestrator, no file edits this round) | none | n/a | reviewed P1-4 proposal from architecture angle: approved Shape A over Shape B; found a false "already done once" precedent claim and the `wallet.go GetSummary` silent-defect call site | none | n/a |
| 4HB0R5-08 | bonus-engine (independent review) | Done | 4HB0R5-03 | none (analysis reported to Orchestrator, no file edits this round) | none | n/a | reviewed P1-4 proposal from bonus-engine angle: approved Shape A/extended B1; found Rule B2 needed restating; found the terminal-Grant settlement-credit gap (real, structurally triggered, same open question as ADR 0034 §2) | none | n/a |
| 4HB0R5-09 | ledger-finance (follow-up fix) | Done | 4HB0R5-08 | `docs/decisions/0038-*.md` (new §14.6) | none | n/a | closed the NULL-`provider_id` idempotency gap `bonus-engine` found: in-house-mode postings route through `UNIQUE (tenant_id, idempotency_key)`, never a reserved sentinel | none | n/a |
| 4HB0R5-10 | ledger-finance (gap closure) | Done | 4HB0R5-06, 4HB0R5-07, 4HB0R5-08 | `docs/architecture/ledger-accounting-model.md` (§6.3 extended), `docs/decisions/0038-*.md` (§15 extended) | none | n/a | closed all four Wave 2 gaps; added mixed-funded unlock-side cases (C-void/C-loss/C-win/C-partial), settlement-time recovery mechanism, Rule B2 boundary-crossing restatement with worked numeric proof; produced new unreviewed content (C-win rule) and an explicit OPEN QUESTION (C-cashout) | none | n/a |
| 4HB0R5-11 | sportsbook (doc fix) | Done | 4HB0R5-09 | `docs/architecture/09-*.md` (§6.1 new) | none | n/a | added in-house-mode idempotency-routing statement per ledger-finance's §14.6 requirement | none | n/a |
| 4HB0R5-12 | bonus-engine (cross-reference) | Done | 4HB0R5-08 | `docs/architecture/10-*.md` (§5), `docs/decisions/0032-*.md` (§5) | none | n/a | added terminal-Grant cross-reference as explicit Human decision required item, three named options, none selected | none | n/a |
| 4HB0R5-13 | bonus-engine (Wave 3) | Done | 4HB0R5-10 | `docs/architecture/ledger-accounting-model.md` (§6.3.5.3), `docs/architecture/financial-transaction-flows.md` (§13), `docs/decisions/0032-*.md` (§8), `docs/decisions/0034-*.md` (§14.7 area), `docs/architecture/10-*.md` (Grant lifecycle table) | none | n/a | APPROVE-WITH-CHANGES on C-win (found a real rounding-based bonus-abuse/structuring vector, required anti-structuring control); C-cashout input (recommends not-cashout-eligible); fresh P1 challenge found VOID_ON_SELF_EXCLUSION doesn't net wagering-progress debit against reversal | none | n/a |
| 4HB0R5-14 | sportsbook (Wave 3) | Done | 4HB0R5-10 | `docs/architecture/ledger-accounting-model.md` (§6.3.5.2, C-cashout input), `docs/decisions/0038-*.md` (§8.1 flag, open item 7), `docs/decisions/0034-*.md` (§14.7 note), `docs/decisions/0037-*.md` (open item 7, B.7 confirming note) | none | n/a | found C-win's split-recovery computation belongs in `internal/ledger`, not sportsbook; found P1-2 has no casino-vs-sportsbook product dimension; found P1-5 unspecified for mid-partial-settlement void | none | n/a |
| 4HB0R5-15 | product-owner-proxy (Wave 3) | Done | 4HB0R5-10 | none (analysis reported to Orchestrator, no file edits) | none | n/a | recommended not-cashout-eligible for C-cashout; ran a full scope-check of this stage's output, found no scope creep | none | n/a |
| 4HB0R5-16 | security (Wave 3, independent) | Done | none | `docs/security/security-architecture.md` (new Stage 4H-B0-R5 section) | none | n/a | first independent look at all five P1s, verified against live schema/code; found 9 P1-level gaps (FX control-plane RBAC/audit, Asset Authorization RLS/fail-closed/four-eyes/server-sourcing, idempotency tamper-resistance, self-exclusion as-of/completeness); confirmed no tenant-isolation defect in P1-4 | none | n/a |
| 4HB0R5-17 | qa (Wave 3, independent) | Done | none | `docs/testing/testing-strategy.md` (new Stage 4H-B0-R5 section) | none | n/a | first independent testability look at all five P1s; found P1-2 layers 4/6 not independently testable despite a distinguishable-reason-code claim; found P1-5 enforcement-point and listener test-coverage gaps | none | n/a |
| 4HB0R5-18 | risk (Wave 3, independent) | Done | none | `docs/decisions/0031-*.md` (new §32) | none | n/a | first independent look at all five P1s verified against actual code; found no gap in P1-1/P1-4/P1-5; found a real P1-2 exponent-awareness gap; discovered a pre-existing latent fail-open in `internal/risk`'s own cumulative-usage query while investigating P1-3; flagged a cross-document Risk-checkpoint conflict (ADR 0031 vs. ADR 0038) | none | n/a |
| 4HB0R5-19 | Orchestrator | Done | 4HB0R5-01..18 | `docs/governance/project-status.md`, `docs/active-stage.md`, this registry, `docs/progress.md` | none | full validation gate (docs-only; `go build ./...` clean, no code touched) | this stage's completion report | none | n/a — **stage explicitly STOPS here; no implementation authorized; all five P1s architecturally resolved but not implementation-ready, residual findings catalogued by owner** |

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
