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
| DR-4HB0R6-01 | 4H-B0-R6 | Cross-workstream consistency review (architect) | bonus-engine (doc 10 §2) | Doc 10's "Bonus Dependency Contract Freeze" §2 cites `AssetAuthorization.CheckEligibility` verbatim from ADR 0037 §C.2's code block, which Workstream A's shipped code (`internal/assetregistry/authorization.go`) diverges from in four concrete ways: (1) a `tx pgx.Tx` second parameter (load-bearing — the eligibility read must happen in the same transaction as the financial write it authorizes); (2) `operation Operation` is now `scope OperationScope{Product, Operation}` — the `(product, operation)` widening ADR 0037 §C.6 item 3 records; (3) concrete Go types (`uuid.UUID`/`string`), not the ADR's illustrative `TenantID`/`BrandID`/`AssetCode`; (4) two new fail-closed contract clauses doc 10 §2 does not carry — a zero-value `jurisdiction` is an immediate denial (`ReasonJurisdictionContextMissing`), and `tenant` is cross-checked against the transaction's own `app.tenant_id` GUC (`ErrTenantContextMismatch`). Doc 10 §2 and its "Genuine gaps" item 2 already flag that re-verification is required, so this is a known-open item, not a silent contradiction — but the divergence is now concrete and verifiable. **Practical consequence Bonus must record**: with no per-player jurisdiction resolver anywhere in the codebase (Stage 4G-FINAL Part C), plus all seven seeded assets at `platform_authorized = false` (ADR 0037 §C.6 item 4), a Bonus call to `CheckEligibility` denies today. Doc 10 §1.3's exponent/metadata read path is separately confirmed CORRECT and needs no change — Workstream A's `GetAsset` doc comment explicitly blesses a registry read for `decimal_exponent` as not an eligibility decision. Filed against bonus-engine, who owns doc 10; architect did not edit doc 10 | Stage 4H-B0-R6 | Open |
| DR-4HB0R6-02 | 4H-B0-R6 | 4HB0R6-04 (risk), via ADR 0031 §36 | ledger-finance (ADR 0038 §13) | ADR 0031 §36 resolves the §32(g) conflict by adopting ADR 0038 §13's conclusion (settlement/cashout are NOT Risk checkpoints) and requests one non-blocking cross-check: §13's bold, unconditional sentence "none of them are additional Risk checkpoints" should be explicitly scoped to the provider-driven mode §13 describes, so a future IN-HOUSE cashout mode — where the platform prices the buy-back and therefore does create new exposure at offer/acceptance — does not inherit a blanket "never a Risk checkpoint" reading. Verified by architect: §13's own text already scopes the narrower *offer-generation* sub-claim parenthetically ("the platform does not price cashout offers in the provider-driven shape") but does NOT scope the headline sentence, so the gap is real as §36 describes it. Also verified: Workstream C phase 1's new `ledger-accounting-model.md` §6.4 does **not** resolve this and does **not** contradict it — §6.4.3 holds cashout of every funding origin `NOT IMPLEMENTED` this pass, and §6.4 never mentions Risk, checkpoints or a pricing mode at all (it is an accounting-posting contract, not a Risk-enforcement one). No live contradiction exists; ledger-finance's response is still required to close the item on ADR 0038's side | Stage 4H-B0-R6 | Open |
| DR-4HB0R6-03 | 4H-B0-R6 | Cross-workstream consistency review (architect) | architect (self, ADR 0037 §C.2) | Root cause of DR-4HB0R6-01: ADR 0037 §C.2's `CheckEligibility` code block was left as originally written while the `(product, operation)` widening was recorded only in §C.6 item 3, ~290 lines later. §C.6 is explicit that it supersedes §C.2 on this point, so the ADR is not self-contradictory in substance — but any reader who cites §C.2 verbatim (as Workstream F correctly did, per its own instructions) gets the stale shape. Needs an in-place forward pointer at §C.2 to the shipped signature. **Deliberately NOT applied during this review**: ADR 0037 is under concurrent independent review by security/qa/code-reviewer this stage, and editing it mid-review would collide with their reads. Apply after those reviews land | Stage 4H-B0-R6 | Open |

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

## Stage 4H-B0-R6

Foundational implementation-hardening stage. **AUTHORIZED for production
code and migrations, but ONLY for the six workstreams below.** No Bonus
Engine, no Gamification, no Reward Orchestrator, no real
sportsbook/data-feed/FX/KYC/PSP/custody provider integration.

Reserved migration numbers (assigned upfront to prevent collision across
parallel dispatches): **0043 = Workstream E** (RG self-exclusion policy
config), **0044-0045 = Workstream A** (Asset Registry + Authorization),
**0046 = Workstream D** (Risk, if a schema change proves necessary),
**0047 = Workstream B** (idempotency, if a schema change proves
necessary), **0048 = Workstream C** (`player_locked` origin-split, only
after its implementation ADR is independently validated).

| ID | Owner | Status | Dependencies | Files owned | Workstream | Blockers |
|---|---|---|---|---|---|---|
| 4HB0R6-01 | Orchestrator | Done | none | `docs/governance/*` | Governance setup | none |
| 4HB0R6-02 | architect | Dispatched | none | migrations `0044`-`0045`, `internal/assetregistry` (new), `docs/decisions/0037-*.md` | A: Asset Registry + Authorization implementation | none |
| 4HB0R6-03 | identity-compliance | Dispatched | none | migration `0043`, `internal/rg` (extended), `docs/decisions/0034-*.md` | E: RG self-exclusion technical hardening | none |
| 4HB0R6-04 | risk | Dispatched | none | `internal/risk` (extended), `docs/decisions/0031-*.md` | D: Risk fail-closed hardening + exponent-awareness | none |
| 4HB0R6-05 | ledger-finance | Dispatched | none | `docs/architecture/ledger-accounting-model.md` (implementation ADR update only, no code yet) | C (phase 1): implementation ADR for `player_locked` flows A-L | Code gated on independent bonus-engine + sportsbook validation of this ADR |
| 4HB0R6-06 | integrations | Dispatched | none | `internal/idempotency` (new, or extends existing shared pattern) | B: Idempotency hardening (canonical, provider-authenticated occurrence identifiers) | none |
| 4HB0R6-07 | bonus-engine | Done | none | `docs/architecture/10-bonus-engine-architecture.md` (new Bonus Dependency Contract Freeze section) | F: Bonus dependency contract freeze | none |
| 4HB0R6-08 | sportsbook (Wave 2) | Done | 4HB0R6-05 | `docs/architecture/ledger-accounting-model.md` §6.4.9 V-5/V-6, `docs/architecture/09-sportsbook-architecture.md` §16 | Validates Workstream C phase 1; sportsbook-readiness check on B/A/D | none |
| 4HB0R6-09 | architect (Wave 2, cross-workstream) | Done | 4HB0R6-02..07 | `docs/governance/task-registry.md` (DR-4HB0R6-01/02/03) | Cross-workstream consistency; confirms repo-wide build/vet/fmt clean | none |
| 4HB0R6-10 | qa (Wave 2) | Done | 4HB0R6-02, -03, -04, -06 | none (analysis reported, no file edits) | Test-coverage verification across A/B/D/E; found 3 real gaps (B mislabeled test, D missing TOCTOU test, E missing cross-tenant RLS test) | 3 gaps found, routed to fix wave |
| 4HB0R6-11 | bonus-engine (Wave 2) | Done | 4HB0R6-05 | `docs/architecture/ledger-accounting-model.md` §6.4.11, `docs/architecture/10-bonus-engine-architecture.md` | Validates Workstream C phase 1 (V-1..V-4, OB-1); confirms V-1 as a new real P1 (wagering-progress farming) with a precise fix design | G-3 gate not yet closed (query fix + Progress-trail trigger not built) |
| 4HB0R6-12 | code-reviewer (Wave 2) | Done | 4HB0R6-02, -03, -04, -06 | none (analysis reported, no file edits) | Code-level review across A/B/D/E; found F1 (High, layer-7 eligibility grant had no four-eyes representation) and F2 (High, RLS conjunct missing) plus 7 lower-severity findings | 9 findings, F1/F2/F3 routed to fix wave as must-fix |
| 4HB0R6-13 | security (Wave 2) | Done | 4HB0R6-02, -04, -06 | none (analysis reported, no file edits) | Independent security review of A/D/E; **confirmed and live-reproduced P1-A1** (four-eyes person-identity check unconditionally inert — no code path could ever set person_id on a platform_admin account); found P2-A2 (RLS DELETE-widening), P2-A3 (sequencing hazard, informational), P2-E1 (stalled-run detection gap); D confirmed sound, IMPLEMENTED | P1-A1 launch-blocking, routed to fix wave |
| 4HB0R6-14 | architect (fix wave) | Done | 4HB0R6-12, -13 | `migrations/0047_asset_registry_dual_control_hardening.*`, `internal/assetregistry/*`, `internal/httpserver/asset_registry_*.go`, `docs/decisions/0037-*.md` §C.7, `docs/api/openapi/platform-api.yaml`, `internal/risk/exponent_integration_test.go` (fixture only) | Closes P1-A1, P2-A2, F1/F4/F5; verified fail-before/pass-after against a literal reproduction of the exploit at both DB and HTTP layers | none — verified closed by final security/QA re-pass |
| 4HB0R6-15 | identity-compliance (fix wave) | Done | 4HB0R6-12, -13 | `migrations/0049_self_exclusion_enumeration_rls_and_floor_write_hardening.*`, `internal/rg/self_exclusion_*.go`, `cmd/seed-admin/main.go`, `internal/httpserver/admin_routes.go`, `internal/httpserver/routes.go`, `internal/httpserver/platform_staff_person_link_test.go` | Provides the person-linking path required to unblock 4HB0R6-14's fix; closes P2-E1, F2, F3, F4 (backdating), F5 (RLS alignment); self-resolved a migration-number collision with 4HB0R6-14 by using 0049 | none — verified closed by final security/QA re-pass |
| 4HB0R6-16 | integrations (fix wave) | Done | 4HB0R6-12 | `internal/idempotency/*` | Closes F6 (dead-code trim) and QA's changed-asset test gap | none |
| 4HB0R6-17 | risk (fix wave) | Done | 4HB0R6-10, -12 | `internal/risk/denomination.go`, `internal/risk/cumulative_race_integration_test.go` | Closes F7 (exponent-lookup consolidation through `internal/assetregistry`) and QA's TOCTOU concurrency test gap; mutation-tested the fix (temporarily removed the advisory lock, confirmed the new test fails 10/10, restored byte-identical) | none |
| 4HB0R6-18 | security (final re-verification) | Done | 4HB0R6-14, -15, -16, -17 | none (analysis reported, no file edits) | Re-ran the original P1-A1 exploit against the fixed code — CONFIRMED CLOSED, could not reconstruct by any route tried. Confirmed all other fix-wave items closed. Found 5 new minor items (A-E); only A (stalled-run reconciliation has no scheduler wiring) is non-trivial, labeled PARTIALLY IMPLEMENTED | Finding A carried forward, not launch-blocking |
| 4HB0R6-19 | qa (final re-verification) | Done | 4HB0R6-14, -15, -16, -17 | none (analysis reported, no file edits) | Independently re-verified all 3 originally-flagged test gaps are genuinely closed (non-tautological, real assertions) plus the 2 new four-eyes-bypass regression tests; full 588-test integration suite green, 0 skips/failures; confirmed migration 0039's down-migration data-consistency issue is real, pre-existing (Stage 4E), and fresh-DB-safe | none |
| 4HB0R6-20 | Orchestrator | Done | 4HB0R6-01..19 | `docs/governance/*`, `docs/active-stage.md`, `docs/progress.md` | full validation gate (gofmt/go build/go vet/go test clean) | this stage's completion report | none — **stage explicitly STOPS here; Stage 4H-B1 NOT authorized** |

All rows Done. Labels at close: Workstream A (Asset Registry) —
**IMPLEMENTED** for the four-eyes control, RLS backstop, and layers 1-7
(security's explicit final verdict); layer 8 (market-rate availability)
remains **NOT IMPLEMENTED** (concluded to be substantially a runtime
FX-provider check, not a stored fact). Workstream B (idempotency) —
**IMPLEMENTED** as a shared primitive, explicitly disclosed as having
zero production call sites yet (no adapter has adopted it). Workstream C
(player_locked) — phase 1 (ADR) **DONE**; phase 2 (migration 0048 +
code) **NOT STARTED**, gated on G-2 (human decision, terminal-Grant) and
G-3 (query-netting + Progress-trail design, not yet built) for
bonus-funded cases; cash-only cases and the schema widening itself have
no remaining objection. Workstream D (Risk) — **IMPLEMENTED**, security
sign-off granted, no findings. Workstream E (RG self-exclusion) —
**PARTIALLY IMPLEMENTED**: policy config/resolution/tighten-only/as-of/
authoritative-time are IMPLEMENTED; the stalled-run reconciliation
primitive exists but has no scheduler wiring (4HB0R6-18 finding A).
Workstream F (Bonus dependency contract) — **DONE** (documentation).

## Stage 4H-B0-R7

Final financial/bonus implementation gate before Stage 4H-B1. Purpose:
close the remaining implementation-blocking financial dependencies R6
discovered — `player_locked` phase 2, the G-3 wagering-progress-farming
P1, the Terminal-Grant and self-exclusion technical contracts, and a
formal Human Decision Register for the three still-unmade human
decisions. **No Bonus Engine, no Gamification, no Reward Orchestrator,
no real provider integration. Stage 4H-B1 NOT authorized by this
stage.**

Reserved migration numbers: **0048 = Workstream A** (`player_locked`
phase 2, already reserved from Stage 4H-B0-R6), **0050 = Workstream B**
(wagering-progress-integrity mechanism, if a schema change proves
necessary), **0051 = Workstream C** (Terminal-Grant technical contract,
if needed), **0052 = Workstream D** (self-exclusion further hardening,
if needed).

| ID | Owner | Status | Dependencies | Files owned | Workstream | Blockers |
|---|---|---|---|---|---|---|
| 4HB0R7-01 | Orchestrator | Done | none | `docs/governance/*` | Governance setup | none |
| 4HB0R7-02 | ledger-finance | Done | none | `ledger-accounting-model.md` §6.5 | A design | none |
| 4HB0R7-03 | architect/bonus-engine/sportsbook | Done | 4HB0R7-02 | (review only) | A design validation | none |
| 4HB0R7-04 | ledger-finance | Done | 4HB0R7-03 | `ledger-accounting-model.md` §6.6 | B design (G-3, Model C) | none |
| 4HB0R7-05 | sportsbook/bonus-engine/architect | Done | 4HB0R7-04 | (review only) | B design validation | none |
| 4HB0R7-06 | bonus-engine | Done | none | `10-bonus-engine-architecture.md` T.1-T.13 | C (Terminal-Grant contract) | none |
| 4HB0R7-07 | identity-compliance | Done | none | ADR 0034 §14.10-§14.13 | D (self-exclusion hardening) | none |
| 4HB0R7-08 | sportsbook | Done | none | (review only) | E (conformance validation) | none |
| 4HB0R7-09 | product-owner-proxy/architect | Done | none | ADR 0039 | F (Human Decision Register) | none |
| 4HB0R7-10 | ledger-finance | Done | 4HB0R7-02, 4HB0R7-03 | `migrations/0048_*`, `internal/ledger/ledger.go`, `internal/wallet/wallet.go`, three new test files | A implementation | none |
| 4HB0R7-11 | security/code-reviewer/qa | Done | 4HB0R7-10 | (review only) | A independent review, round 1 | S-1 (blocking), F1/F2/F3 (code-reviewer), S-2/S-3/S-4 (security, non-blocking) — all routed to 4HB0R7-12 |
| 4HB0R7-12 | ledger-finance | Done | 4HB0R7-11 | same files as 4HB0R7-10, plus `reconciliation-model.md`, `03-database-architecture.md`, `06-wallet-ledger-architecture.md`, `financial-domain-model.md`, `financial-transaction-flows.md` | A fix wave | none |
| 4HB0R7-13 | security | Done | 4HB0R7-12 | (review only) | A final re-verification (S-1 close-out) | none — S-1 CONFIRMED CLOSED |
| 4HB0R7-14 | Orchestrator | Done | 4HB0R7-10 through 4HB0R7-13 | `docs/active-stage.md`, this file, `docs/governance/project-status.md`, `docs/progress.md` | Governance close-out | none |

**Migration-number reservation outcome**: only `0048` was used (Workstream
A). Workstreams B/C/D did not require a schema change this stage (design/
validation only, no code authorized), so `0050`/`0051`/`0052` were never
consumed and remain reserved for whichever future stage implements those
workstreams.

**Integration status**: Workstream A's migration `0048` and its
`internal/ledger`/`internal/wallet` changes are **Integrated** — committed
to `claude/focused-wright-jw88w9` at `17f1057`, pushed, full integration
suite green. No other workstream produced code this stage, so no other
integration action applies.

## Stage 4H-B1 — Bonus Engine Implementation

**AUTHORIZED**, started at HEAD `7e1656f` on `claude/focused-wright-jw88w9`.
Wave structure per the stage directive §36: 8 gated waves, each requiring
a clean prior-wave review before the next starts. No specialist may
review its own work at any wave.

**Migration-number reservation**: block `0050`+ (the `0050`/`0051`/`0052`
numbers Stage 4H-B0-R7 reserved for its own Workstreams B/C/D were never
consumed — R7 closed as design-only for those workstreams — so this block
is released back to the pool and reused here, not duplicated). Exact
count to be determined by Wave 1's schema design; specialists claim
numbers sequentially and self-resolve collisions per the established
protocol (check `git log`/`ls migrations/` before writing, document any
collision found).

**Roster note**: the directive names `bonus-finance` as a specialist.
No such agent type exists in this environment's configured roster. Its
concerns (bonus-specific financial correctness) are covered jointly by
`ledger-finance` (ledger invariants, ownership platform-wide per
CLAUDE.md) and `bonus-engine` (bonus-specific calculation rules) — the
same adaptation pattern used for other roster gaps in prior stages.
`architect`'s Wave 1 cross-domain map is instructed to confirm this
coverage is sufficient before Wave 1 closes.

| ID | Owner | Status | Dependencies | Files owned | Workstream | Blockers |
|---|---|---|---|---|---|---|
| 4HB1-01 | Orchestrator | Done | none | `docs/governance/*` | Governance setup | none |
| 4HB1-02 | bonus-engine | In progress | none | `docs/architecture/10-bonus-engine-architecture.md` | Wave 1: domain model + full bonus catalogue + campaign/offer/grant/segmentation/coded-bonus/bulk/suggestion design | none |
| 4HB1-03 | ledger-finance | In progress | none | `docs/architecture/ledger-accounting-model.md` | Wave 1: financial/ledger integration contract (bonus_expense, Rule B2 generator, conversion flow) | none |
| 4HB1-04 | risk | In progress | none | ADR 0031 addendum | Wave 1: Risk integration contract | none |
| 4HB1-05 | identity-compliance | In progress | none | ADR 0034 addendum | Wave 1: RG + identity/multi-account contract | none |
| 4HB1-06 | sportsbook | In progress | none | doc 09 addendum (review only) | Wave 1: provider-native bonus coexistence contract | none |
| 4HB1-07 | casino | In progress | none | doc 08 addendum (review only) | Wave 1: casino event-consumption contract | none |
| 4HB1-08 | security | In progress | none | `docs/security/security-architecture.md` addendum | Wave 1: RBAC/audit/tenancy/RLS contract | none |
| 4HB1-09 | architect | In progress | none | new cross-domain implementation-contract doc | Wave 1: master architecture→ADR→object→service→API→event→ledger→audit→test mapping | none |
| 4HB1-10 | qa | In progress | none | `docs/testing/testing-strategy.md` addendum | Wave 1: full test-matrix design | none |

**Wave 1 status: COMPLETE, all 9 dispatches reported back, reviewed, and
committed** (`41c029f`, `0171e02`, `39be4af`, `b0442b1`, `5b7e7ed`,
`89a09ab`, `29490dc`). Reconciliation performed by the Orchestrator below.

**Migration-number ledger** (claimed during Wave 1, none written yet —
Wave 1 was design-only):
- `0050` — `bonus_expense` account-type CHECK widening (`ledger-finance`, §7.2)
- `0051` — `bonus_grant`/`bonus_conversion`/`bonus_forfeiture`/`bonus_reversal` transaction types + `reason_code` constraint widening (`ledger-finance`, §7.3)
- `0052` — `rounding_rules` table + `ledger_transactions.rounding_rule_id` (`ledger-finance`, §7.8 — requested, now assigned)
- `0053` — `ledger_accounts` identity-immutability trigger, HR-15/HR-16 (`ledger-finance`, §7.14 — requested, now assigned; **hard gate: must land before the first `player_bonus` posting**, not merely before the first locked-account posting)
- `0054`+ — reserved for `bonus-engine`'s own domain tables (`bonus_campaigns`, `bonus_offers`, `bonus_grants`, `bonus_progress`, and any others Wave 2's schema design determines are needed); `bonus-engine` claims sequentially and self-resolves any collision with `0052`/`0053` per the standing protocol (check `git log`/`ls migrations/` before writing)
- ADR `0040` reserved for `architect` to formally ratify the activity-consumption transport decision (ledger-derived + in-process adapter, no broker) made in `docs/architecture/29-bonus-implementation-contract.md` §2 — to be written by `architect` as part of its Wave 2 role, not minted speculatively now

**Reconciliation findings and resolutions:**

1. **P1 escalation (`architect`, doc 29 §4.1) — gate G-2 is reachable in the casino-only slice, not only sportsbook as ADR 0039 originally framed it**, because `internal/casino`'s `postBet`/`postWin` are separate provider callbacks with an arbitrary time gap: a bonus-funded bet can have its Grant go terminal (expired/cancelled/forfeited) before its win settles. Compounding this, `internal/casino`'s `postWin` hardcodes the credit destination to `AccountPlayerCash` — verified directly against `orchestrator.go` — so wiring bonus-funded casino stakes today, without a fix, would let a bonus-funded win credit withdrawable cash with zero wagering requirement enforced. **Resolution, within engineering authority (does not require selecting G-2's answer):**
   - `casino` must fix `postWin`'s destination resolution to route a bonus-funded bet's win back to `player_bonus`/`player_locked_bonus` per the already-specified case E (`ledger-accounting-model.md` §6.4) — required before any bonus-funded casino stake is wired, independent of G-2.
   - A Grant may only reach a genuinely terminal status (expired/cancelled/forfeited-final) once its attributable locked balance (`player_locked_bonus` tied to that Grant) reaches zero. A terminal trigger firing while locked funds remain outstanding defers the Grant into a pending-settlement sub-state that finalizes automatically once the locked stake resolves (win or loss) — never crediting against an already-terminal Grant, by construction. This is a named engineering design choice (not a selection of ACTION_REFORFEIT/ACTION_ROUTE_TO_CASH/ACTION_HOLD_FOR_REVIEW), reversible, and does not touch the separately-tracked `OpenBetSelfExclusionPolicy` gate (self-exclusion-triggered voids remain gated exactly as already recorded).
   - Both are Wave 2/3 dependencies for `casino` and `bonus-engine`/`ledger-finance` jointly, flagged prominently to the human before dispatch (see completion message).

2. **OI-1 (the `bonus-finance` roster gap) — 5 of 7 named concerns have a confirmed owner and design** (posting map, Model C derivation queries, HR-9 sequencing, rounding boundaries, `bonus_expense` reporting deferred to `data-analytics` for a future reporting wave). **2 remain genuinely unowned, assigned now:** campaign/global budget-cap enforcement — assigned to `bonus-engine` (not `risk`: `risk`'s own Wave 1 report confirms `campaign` is not an implemented Risk dimension and recommends against forcing it into one) — Wave 2 dependency; the Bonus reconciliation stream (WP-R/B1 extension) — assigned to `ledger-finance`, Wave 2+ dependency, not blocking Wave 2's core schema.

3. **OI-2 (doc 22 event-taxonomy defects)** — fixed directly: `docs/architecture/22-canonical-activity-event-taxonomy.md` now splits `bonus.grant.cancelled` (posts `bonus_forfeiture`) from `bonus.grant.reversed` (posts `bonus_reversal`), adds the previously-missing `bonus.grant.forfeited`, and adds `bonus.grant.activated`/`bonus.grant.progress_changed`/`bonus.grant.converted`/a reserved `bonus.reward.requested`.

4. **OI-3 (segmentation placement)** — ratified. `docs/governance/ownership.md` updated: `internal/segment` is a new, separate, capability-minimal shared package (`architect` owns the interface/contract/schema, `bonus-engine` owns the first consumer's call sites), per `architect`'s reasoning in doc 29 §3 (two already-frozen future consumers, not speculative generality; explicitly no rule DSL/recompute job/CRM engine authorized inside it).

5. **OI-4 (AssetAuthorization checkpoint decision)** — `architect` made this decision within its own ownership (doc 29 §4.2: every Bonus checkpoint passes `CheckEligibility(operation = wagering)`, no new seventh `bonus` operation value). The physical reflection into ADR 0037 §C.2 is assigned to `architect` as a Wave 2 task (a small, mechanical edit recording an already-made decision into its source-of-truth document).

6. **BF-1 (`ledger-finance`'s finding — concurrent operator-funded and provider-funded Grants share one fungible `player_bonus` balance with no lot-attribution mechanism)** — correctly deferred to `architect` + human decision, not resolved. Wave 2's first slice is restricted to **operator-funded Grants only**, per `ledger-finance`'s own recommendation, until this is resolved.

7. **No contradictions found** between the nine reports beyond the two closed above (OI-2, OI-4) — `architect`'s independent cross-domain read of all nine (via doc 29's master map) and this Orchestrator's own review agree.

**Wave 2 authorization (superseded by Wave 1.5 below, human directive)**: the human interposed a Wave 1.5 architecture-reconciliation gate before ordinary Wave 2 begins, in response to the G-2/casino-postWin escalation above. Wave 2 does not start until Wave 1.5 explicitly clears it.

## Stage 4H-B1, Wave 1.5 — Commercial Ecosystem + Casino Win + Segmentation Architecture Reconciliation Gate

**AUTHORIZED** (human directive), started at HEAD `b8b2d1d`. **Architecture/design only — no bonus-funded wagering implementation, no Gamification/CRM/Affiliate implementation. STOP after the gate report; no automatic continuation to Wave 2.**

Roster adaptation, disclosed per this project's standing practice (mirrors the "bonus-finance" gap in Wave 1): the directive treats CRM and Affiliate as new first-class platform domains, but no dedicated `crm`/`affiliate` specialist exists in this environment's roster. Following this project's own precedent (every prior brand-new cross-cutting domain — Retail's doc 26, Asset Registry's ADR 0037 — was authored by `architect`), `architect` authors the CRM and Affiliate architecture documents. Since `architect` cannot then independently review its own output, `code-reviewer` stands in as the independent architectural-consistency reviewer for CRM/Affiliate/Segmentation specifically (its own mandate explicitly includes "consistency with docs/architecture/"), while `ledger-finance`/`security`/`bonus-engine`/`product-owner-proxy` cover their own named review angles per the directive's §J.

**Phase 1 — Authorship (parallel, distinct ownership):**

| ID | Owner | Status | Deliverable |
|---|---|---|---|
| 4HB1W15-01 | casino | In progress | Casino postWin financial source/destination resolution design, G-2 boundary specification (not selecting G-2), adversarial scenarios (§A) |
| 4HB1W15-02 | bonus-engine | In progress | Grant terminal-state invariant + proof (§A.9), bonus targeting/bulk-assignment validation (§C), Bonus Suggestion full spec (§D), bonus catalogue validation (§H) |
| 4HB1W15-03 | architect | In progress | Segmentation Engine architecture doc (§B), CRM Engine architecture doc (§E), Affiliate Engine architecture doc (§F), canonical cross-domain relationship diagrams (§G), updated ownership map + dependency graph |
| 4HB1W15-04 | sportsbook | In progress | Provider-native bonus coexistence re-confirmation against the new CRM/Affiliate/Segmentation additions (§I) |
| 4HB1W15-05 | qa | In progress | Cross-domain test matrix covering all new domains + §A.10's adversarial scenarios |

**Phase 2 — Independent review (parallel, after Phase 1, none reviewing own work)** — to be dispatched once Phase 1 reports back:
- `ledger-finance` — casino's postWin design, bonus-engine's terminal-invariant, G-2 boundary spec (financial-correctness angle, §J)
- `security` — tenant/player isolation, privilege escalation, bulk-assignment/promo-code/segment/CRM-triggered-grant/affiliate-attribution abuse, self-awarding, replay, concurrency, across all Phase 1 docs (§J)
- `code-reviewer` — independent architectural-consistency review of CRM/Affiliate/Segmentation (substituting for "independent architect," since architect authored these) plus the casino/bonus-engine docs
- `bonus-engine` — independent review of architect's Segmentation Engine doc only (first-consumer integration-correctness angle; does not review CRM/Affiliate, outside its domain)
- `product-owner-proxy` — scope-discipline review: confirms architecture-only framing held, no implementation crept in, no scope beyond the directive's own ask

No human decision (G-2, `OpenBetSelfExclusionPolicy`, cashout policy, FD-1, or any other Human Decision Register item) will be selected in this gate.

**Phase 1 status: COMPLETE**, all 5 dispatches committed (`8a126db` sportsbook/qa-partial, `5a9a554` casino, `de3a639` bonus-engine, `ad126bb` architect's four docs, plus `8a126db`/qa). **Phase 2 status: COMPLETE**, all 5 dispatches reported back (`product-owner-proxy`: all five deliverables CLEAN, no scope creep; `bonus-engine`: no P0 on doc 30, 2 P1s; `code-reviewer`: **NOT READY as a frozen set**, 4 P1s (real defects, not wording) plus 8 P2s/6 P3s; `ledger-finance`: **NOT SIGNED OFF**, 1 P0 + 5 P1s + 6 P2s + 5 P3s, fixed 2 of its own factual errors directly (commit `10c543a`); `security`: **blocking implementation authorization on 3 P0s**, plus 13 P1s + 8 P2s + 4 P3s).

**Consolidated P0/P1/P2/P3 register (Wave 1.5 gate):**

**P0 — blocking, must close before the affected subsystem's implementation is authorized:**
1. **LF-2** (`ledger-finance`) — `bonus-engine`'s TI-1 mechanism (doc 10 §N1.4 step 5) auto-forfeits a late win uniformly across all terminal dispositions, which is economically identical to selecting G-2's `ACTION_REFORFEIT` — a relabeling, not a resolution. `casino`'s parallel design (doc 08 §16.10.2) has the correct value-creating/value-reducing split; `bonus-engine`'s formalization must adopt it.
2. **SEC-W15-01** (`security`) — Affiliate commission four-eyes is satisfiable by two colluding external (affiliate) accounts; the approver must be provably internal and outside the benefiting subtree.
3. **SEC-W15-02** (`security`) — CRM's `RequestOfferGrant` interface decomposes what should be a bulk operation into N individually-sub-threshold calls, entirely escaping the always-four-eyes bulk control. The volume control must attach to campaign/journey activation, enforced in Bonus, not CRM.
4. **SEC-W15-03** (`security`) — No actor≠subject rule exists anywhere (Wave 1's own RBAC contract included) preventing a staff member from being the beneficiary of their own grant/adjustment/campaign; a size-based four-eyes threshold is inverted for this exact vector (a size-1 audience is below every threshold).

**P1 — must close before the affected subsystem's first implementation wave is marked complete:**
- **LF-1** — casino's postWin query is not actually the claimed verbatim reuse of its own §6.3.3.1 query; the divergence makes the G-2 seam structurally unreachable as designed (reached "zero times," not "exactly once").
- **LF-4** — casino's postWin fix never releases the lock, so `L(G)` can never reach zero; a losing bonus-funded casino round has no settlement event at all and leaves the stake locked indefinitely.
- **LF-5** — the casino settlement-window dependency is not a rare-residual nicety; without it, a Grant with any losing bonus-funded casino bet can never satisfy `AOE = ∅` and conversion is permanently blocked — consumer harm on the normal path, not an edge case.
- **LF-6** — doc 32's commission settlement is idempotent at the instruction level but not the accrual level; two distinct instructions with overlapping `accrual_refs` both post, a double-settlement vector.
- **LF-11** — doc 10 §N1's `FOR UPDATE` on `ledger_entries` implies a protection append-only tables cannot provide; serialization actually comes from the advisory locks.
- **P1-1 / P1-2** (`code-reviewer`) — doc 30's Kleene fail-closed mapping is reachable-bypassable via a disabled/draft/absent `member_of` segment reference (the same `Not(false)=true` exploit it was built to close, one door over); `inclusion_safe` doesn't propagate through composition (two concrete laundering paths for an RG-derived criterion to become an inclusion input).
- **P1-3** (`code-reviewer`) — `architect`'s CRM→Bonus interface (`RequestOfferGrant`) diverges, unreconciled, from `bonus-engine`'s own same-wave N2.4 specification (`BulkGrantJob`/`CheckOfferEligibility`) — this is directly entangled with SEC-W15-02 above.
- **P1-4** (`code-reviewer`) — doc 32's absolute ledger-import ban for Affiliate misstates the ADR 0035/doc 26 precedent it cites and would make commission settlement unimplementable as written.
- **SEC-W15-04 through SEC-W15-13** (`security`, 10 items) — including: the Eligibility Decision Record has no assigned RLS/read-gating story despite being the most sensitive new table in the gate; `Click` is the platform's first unauthenticated public write path with an unspecified tenant-resolution/`unattributed` case; the `tracking_token` has no subject binding (enables attribution hijacking) and no key-rotation story; re-attribution four-eyes will ship inert via a config-absent-default mechanism different from (and not covered by) the R6 precedent it cites; a `pending_settlement` Grant's balance is not ring-fenced from wagering under a different Grant (a new G-3-family instance); `pending_settlement` as a new Grant status has no re-audit plan for existing status-branching predicates (self-exclusion enumeration, player projection, cancellation four-eyes); `CustomerProfile` has no bounded-read control for its own enormous blast radius; the CRM send gate never re-validates the contact endpoint itself (a distinct disclosure risk from the already-covered consent gap); `CommissionSettlementInstruction`'s approval references are data, not a consumed control; `AttributionCandidate`'s stated dispute-resolution purpose is incompatible with its own node-subtree access model as specified.
- Two `bonus-engine`-flagged items on doc 30 (DEP-SEG-1 provenance anchor point; explicit `Resolve`-vs-`IsMember` call-site commitment).

**P2/P3**: see each specialist's full report (11 P2s + 10 P3s not restated here in full — recorded in the source reports, routed per-owner as each report specifies).

## Stage 4H-B1, Wave 1.5 Fix Wave — Close and Re-Verify the 4 P0s + ~20 P1s

**AUTHORIZED** (human directive), started at HEAD `9ae397f`. **Fix + independent re-verification only — no Wave 2, no bonus-funded-wagering/CRM/Affiliate/Gamification implementation code, unless strictly required to close an already-identified defect.**

**Orchestrator's unifying technical contract**, given to every Phase 1 dispatch below to prevent the kind of unreconciled divergence that produced LF-2/LF-1 last round:

- **Eligibility state vs. financial disposition, formally separated.** A Grant's *wagering eligibility* (may new stakes be authorized against it) is a purely technical property that may change immediately and uncontestedly the moment any termination trigger fires (expiry/cancellation/forfeiture-condition/conversion) — this is never G-2. A Grant's *financial disposition* (what happens to value already at risk when the trigger fires) is where G-2 lives, and only for the specific sub-case of a **value-creating credit (a win) arriving against exposure that was already open at the moment eligibility closed**. Loss-side resolution of the same exposure is value-reducing, creates nothing to dispute, and proceeds immediately and technically, exactly as `casino`'s original B(G)/L(G) split intended.
- **The G-2 seam must genuinely fail closed, not post-then-reverse.** `ledger-finance`'s LF-2 finding is that posting a win credit to `player_bonus` and then immediately reforfeiting it is economically `ACTION_REFORFEIT`, regardless of the state-machine label attached. The corrected behavior: a win credit reaching the G-2 seam is **never posted to any player-accessible balance**. It is held in an economically-explicit, policy-neutral representation (extending, not duplicating, the already-named `ACTION_HOLD_FOR_REVIEW` holding mechanism as the necessary interim parking state for any of the three eventual answers, not a selection of that specific answer) until a human supplies G-2.
- **`EconomicOperationIdentity`**: extend existing operation/transaction-identity mechanisms (correlation_id, idempotency keys, campaign/offer/grant versioning) to carry parent-operation/batch-lineage/intended-aggregate-value, rather than inventing a new domain. Owned by `architect`, since it is the entity that must be consistently referenced by Bonus, CRM, and Affiliate alike.
- **Actor≠subject/beneficiary is one reusable platform invariant**, not three domain-local ones. Owned by `security` (design) with `identity-compliance` confirming the underlying Person-linkage mechanism is the right primitive to build it on (the same `staff_users.person_id` ↔ `PlayerAccount`→`Person` link already used for four-eyes). Each domain (Bonus, CRM, Affiliate) then adopts the same invariant at its own enforcement point — no domain reinvents it.
- **Affiliate four-eyes**: the simplest fail-closed answer that needs no new "affiliate entity" identity resolution is that the approver on any affiliate-financial decision must be an **internal** (non-affiliate) principal, full stop — `security`'s own Phase 2 report already specified this as DEP-AFF-1 condition 4 / SEC-W15-01's required fix.

**Phase 1 — Authorship (parallel, distinct ownership):**

| ID | Owner | Deliverable |
|---|---|---|
| 4HB1FW-01 | bonus-engine | P0#1 (LF-2) fix: redesign TI-1 per the eligibility/disposition split; fix LF-11 (FOR UPDATE claim) |
| 4HB1FW-02 | casino | Casino postWin fix: LF-1 (query must actually deliver what's claimed), LF-4 (lock release / loss-settlement mechanism), LF-7/LF-8 (missing 4th outcome, wrong HR-2 unreachability claim), LF-10 (stop pre-committing to a ledger-finance decision) |
| 4HB1FW-03 | architect | Segmentation fix (P1-1 Kleene/`member_of` gap, P1-2 `inclusion_safe` propagation); CRM fix (P1-3 reconcile with bonus-engine's real N2.4 interface, SEC-W15-02's volume-control-on-activation requirement); Affiliate fix (P1-4 ledger-import misstatement, structural support for SEC-W15-01); `EconomicOperationIdentity` design; ownership map + dependency graph update; P2/P3 doc-level corrections from `code-reviewer`'s and `security`'s Phase 2 reports |
| 4HB1FW-04 | security | P0#4 actor≠subject canonical invariant design; P0#2 affiliate four-eyes redesign detail; its own promised Wave 1.5 security-architecture.md section (DEP-AFF-1/DEP-CRM-4 formalization, EDR constraint set, `bonus_suggestion:*` permissions, `pending_settlement` four-eyes extension) |
| 4HB1FW-05 | identity-compliance | Confirms/extends the Person-linkage mechanism underlying the actor≠subject invariant; defines the affiliate identity/authority boundary security's design needs (entity vs. account vs. beneficial owner) |
| 4HB1FW-06 | ledger-finance | Fixes its own previously-disclosed, not-yet-fixed design errors in its own file: LF-16 (`RoundToMinorUnits` int64 overflow risk vs. NUMERIC(38,0)), LF-17 (stale unextended-B1 documentation row) |

**Phase 2 — Independent re-verification (parallel, after Phase 1, none reviewing own work)** — to be dispatched once Phase 1 reports back:
- `ledger-finance` — independently verifies LF-2's fix (bonus-engine) and the Casino postWin fix (casino) — explicitly required by the directive
- `security` — independently verifies all four original P0s are actually closed — explicitly required
- `architect` — independently reviews bonus-engine's and casino's fixes (cross-domain consistency) — did not author either this round
- `code-reviewer` — substitutes as the independent architectural reviewer for Segmentation/CRM/Affiliate/EconomicOperationIdentity, since `architect` authored those fixes and cannot review its own work (same disclosed roster adaptation as Wave 1.5's own Phase 2)
- `bonus-engine` — independently reviews architect's Segmentation fix only (first-consumer angle, as before)
- `identity-compliance` — independently reviews bonus-engine's P0#1 fix from the RG/self-exclusion interaction angle (a fresh angle; did not author the fix itself)
- `casino` — independently reviews bonus-engine's P0#1 fix from the concrete async-callback-model stress-testing angle (did not author it)
- `risk` — independently reviews the actor≠subject invariant and `EconomicOperationIdentity` design against Risk's own operation/eligibility model (new reviewer this round)
- `sportsbook` — independently reviews whether the new cross-cutting designs generalize correctly to sportsbook's own future domain (new reviewer this round, authored nothing)
- `qa` — produces the required adversarial test-plan updates (12 items per the directive's Testing section)
- `product-owner-proxy` — scope-discipline check: confirms no implementation crept in beyond what's strictly required to close an already-identified defect

No human decision (G-2, `OpenBetSelfExclusionPolicy`, cashout policy, FD-1) will be selected in this fix wave.

**Wave 2 readiness decision (prior gate, superseded by this fix wave's own eventual verdict): NOT READY.** Three P0s block implementation authorization outright (per `security`'s explicit blocking position, which stands under schedule pressure); a fourth P0 (LF-2) means the central G-2-avoidance claim this entire gate exists to validate does not currently hold as designed. This is not a documentation-polish backlog — it is unresolved design defects in the mechanisms meant to prevent silent human-decision selection, self-dealing, and mass-grant abuse. A fix wave is required, routed to the owning specialists per each finding's source report, followed by a second independent re-verification pass, before Wave 2 (or any narrower re-scoped implementation) can be authorized.

## Stage 4H-B1, Wave 1.5 Fix Wave, Phase 2 — Independent Re-Verification Results

All 11 Phase 2 dispatches listed above have reported. Full synthesis:
`docs/governance/wave-1.5-fixwave-phase2-report.md`.

**Verdict: NOT READY.** Summary (full detail in the report above):

- None of the four original P0s (LF-2, SEC-W15-01, SEC-W15-02,
  SEC-W15-03) is independently certified as closed. LF-2's core
  mechanism is sound but has no buildable holding-representation shape.
  SEC-W15-01 has a fail-open/deadlock defect in its corrected resolver
  (`code-reviewer` NEW-6, one-word fix identified, not yet applied).
  SEC-W15-02 is fully open on the Bonus enforcement side, and its
  intended closer (`EconomicOperationIdentity`'s budget projection) was
  independently proven not to work as specified. SEC-W15-03 is adopted
  at 2 of 5 required enforcement points.
- **Four new P0-severity findings** surfaced this round, three of them
  defects in this round's own fixes: LF-18 (`ledger-finance` — casino's
  lock-release step can drive a locked balance negative and materialize
  restricted bonus value into a spendable one, vetoed outright),
  REQ-SEP-BONUS-4 (`security` — the LF-2 fix's own disposition-resolution
  step is an ungated self-dealing surface), a held-win-rollback gap
  (`casino`, corroborated by `qa` as test `C28`), and RK-W15P2-1 (`risk`
  — the shared `SEP-1` mechanism fails open, not closed, on a partial
  RLS-filtered read).
- `architect`'s independent cross-domain review found bonus-engine's and
  casino's two largest fixes do not compose — three concrete,
  independently-reproduced incompatibilities (posting-sequence
  contradiction, circular holding-representation branch selection, no
  destination for the released lock amount). Quoted verdict: "I would
  not certify this pair as architecturally consistent for a Wave 2
  readiness call."
- No Human Decision Register item was selected, narrowed, or defaulted
  in Phase 1 or Phase 2.

Required scope for the next (not yet authorized) fix round is recorded
in the report's §7. Per the authorizing directive, the Orchestrator
stops here: no Wave 2, no CRM/Affiliate/Gamification/bonus-funded-
wagering implementation proceeds without a new human directive.

## Stage 4H-B1, Wave 1.5 Fix Round 2 — Final Financial/Security Closure + Product Surfaces Roadmap Gate

**AUTHORIZED** (human directive), started at HEAD `34aeb82`. Full synthesis:
`docs/governance/wave-1.5-fix-round-2-report.md`.

**Verdict: READY** for Wave 2 authorization, subject to two routed,
non-blocking P1s (LF-10 — rollback of an already-resolved disposition;
`SEP-1`'s `ancestor_closure` resolver's dependency on an `agentnetwork`
tenant-boundary invariant, awaiting `architect` confirmation).

24 specialist dispatches this round, in three phases:

- **Phase 1 (8 dispatches, parallel/sequenced)**: `ledger-finance` decided
  the final G-2 holding-representation design (`player_bonus_held`
  account + `bonus_held_dispositions` table); `casino` and `bonus-engine`
  adopted it (sequenced, not parallel, specifically to avoid repeating
  last round's composition failures); `security` fixed `SEP-1`'s
  fail-open defect and the reflexive-ancestor-closure bug's contract;
  `architect` redesigned `EconomicOperationIdentity` to actually close
  SEC-W15-02 and started the Product Surfaces roadmap; `backoffice` and
  `frontend` wrote the Back Office/Partner Console/B2C architecture.
- **Phase 2 (10 independent re-verification dispatches)**: all four
  original P0s and all four Wave-1.5-Phase-2-discovered new P0s
  independently certified closed by reviewers who did not author the
  fixes. Found and fixed in the same round: NEW-2/NEW-6/NEW-7, the
  `ACTION_REFORFEIT` posting-sequence contradiction (relocated but not
  resolved by Phase 1), a missing Grant-status-finalization seam, a
  genuine disagreement between `risk` and `security`/`architect` on a
  four-eyes threshold (resolved with dissent recorded), and
  `REQ-SEP-STAFF-1`'s two required changes (one of which surfaced a real
  NULL-comparison bug in `SEP-1` step 4, also fixed).
- **Closing pass (6 further dispatches)**: `ledger-finance`,
  `security`, `architect` (×2), `casino`, `bonus-engine` closed every
  Phase-2 finding above, ending with `architect`'s final composition
  re-certification (which itself found and fixed three residual
  text-drift defects the fix-chain left behind — disclosed, not hidden).

No Human Decision Register item was selected. No code, migration, or UI
was written — all 24 dispatches stayed within the authorized
design/documentation scope, confirmed by `product-owner-proxy`'s explicit
audit. Per the authorizing directive, the Orchestrator stops here: no
Wave 2, CRM, Affiliate, Gamification, or Back Office/Partner
Console/B2C frontend implementation proceeds without a new human
directive.

## Stage 4H-B1, Wave 2 — Real Bonus Engine Implementation (Phases 1-11, CONCLUDED)

**AUTHORIZED** (human directive, real code this time — not design).
Commits `d145ba1`..`3526d87`: Phases 1-9 (`d145ba1`..`be2eed6`), Phase 10
(`90b0ae7`), two dependency-request fix dispatches (`28e34db`,
`af4f9cd`), Phase 11 (`3526d87`, final independent financial
certification). Full synthesis: `docs/governance/wave-2-report.md`.
**Final verdict: READY.**

| ID | Owner | Status | Dependencies | Files owned | Tests | Docs | Blockers |
|---|---|---|---|---|---|---|---|
| W2-P1 | ledger-finance | Done | Wave 1.5 Fix Round 2 (§7.7.2 frozen) | `internal/ledger/bonus_mirror.go` (Rule B2 generator), `internal/money` (shared rounding), migrations `0050`-`0052` | `bonus_mirror_integration_test.go`, `bonus_migrations_integration_test.go`, `money_test.go` | none this phase | none |
| W2-P2 | backend | Done | W2-P1 | migrations `0053`-`0060`, `internal/bonus` schema/repository skeleton, `internal/economicop` skeleton, RBAC wiring | `bonus_integration_test.go`, `economicop_integration_test.go` | none this phase | none |
| W2-P3 | bonus-engine | Done | W2-P1, W2-P2 | `internal/bonus` (lifecycle, AOE/eligibility, attribution, `held_disposition_ops.go`'s `ResolveTerminalGrantCredit`/`RecheckGrantExposure`, 5 bonus types, EOI enforcement, SEP-1, four-eyes, HTTP handlers), migrations `0061`-`0064` | `lifecycle_integration_test.go`, `bonus_integration_test.go` | none this phase (doc reconciliation deferred to Phase 10) | Signature drift vs. doc 08 §16.9/§16.21, doc 10 §N1.4.2 pseudocode — **closed this Phase 10** (docs corrected to match shipped code) |
| W2-P4 | risk | Done | W2-P3 | `internal/risk/cumulative.go` (new `bonus_conversion` entry), migration `0065` | risk integration suite | ADR 0031 §16 checklist completed | none |
| W2-P5 | identity-compliance | Done | W2-P3 | `internal/bonus/held_disposition_ops.go` (T.1 gate fix), posting-shape fix found in passing | `TestHeldDisposition_ResolveRouteToCash_BlockedBySelfExclusion`/`_BlockedByRiskDeny`/`_AllowedPlayerSucceeds` | inline doc comments | **Real gap closed**: `ACTION_ROUTE_TO_CASH` previously bypassed RG/Risk/AssetAuthorization entirely — see `ErrHeldDispositionActionDenied`'s doc comment |
| W2-P6 | security | Done | W2-P3 | migration `0066` (SEP-1 Step-0 self-proof), bulk-worker error-masking fix (`targeting.go`'s `ConsumeRootBudget` error-conflation bug) | new SEP-1 core-case test (first ever exercise of it) | none this phase | none |
| W2-P7 | casino | Done | W2-P3, W2-P4, W2-P5, W2-P6 | `internal/casino/bonus_settlement.go` (new), `types.go`, `orchestrator.go` (`postWin`/`postRollback` G-2 wiring) | `bonus_settlement_integration_test.go` (incl. 2 real-race tests under `-race`) | doc 08 §16 implemented against — **§16.15's self-contradictory prose found here, closed this Phase 10** | LF-10 (rollback of an already-resolved disposition) correctly still routed to ledger-finance, not worked around |
| W2-P8 | sportsbook | Done | W2-P7 | none (read-only boundary review, no code) | n/a | n/a | No P0/P1 found; no `internal/sportsbook` package exists, confirming Phase 7's boundary claims have no live sportsbook counter-example yet |
| W2-P9 | qa | Done | W2-P1..P8 | `internal/bonus/lifecycle_integration_test.go` (3 new adversarial tests: concurrent-terminate, concurrent-reversal, cross-brand isolation), `docs/testing/testing-strategy.md` reconciliation | full suite re-run `-race -tags=integration`, zero flakes across 3-5x repeats per touched package | testing-strategy.md test-ID reconciliation | QA-W2P9-1..6 (P2/P3, non-blocking, recorded) |
| W2-P10 | architect | Done | W2-P1..P9 | `docs/architecture/08-casino-integration-architecture.md` (§16.9/§16.15/§16.21 signature-drift + self-contradiction fixes), `docs/architecture/10-bonus-engine-architecture.md` (§N1.4.2 signature-drift fix), `docs/governance/ownership.md`, `docs/governance/task-registry.md` (this entry) | full validation floor re-run (`gofmt`, `go build`, `go vet`, `go test -count=1 ./...`, `go test -tags=integration -count=1 ./...`, `go test -race -tags=integration -count=1 ./...`) | this entry | **New finding, not closed here** — EOI/Risk lock-ordering reversal in `IssueSingleManualGrant`/`RunStaticBulkGrantJob` (see Dependency Request Log `DR-4HB1W2-01` below), dormant today, **adjudicated as a real pre-condition that must be fixed before any `bonus_grant`-scoped cumulative Risk rule is added, not a permanently-acceptable "documented risk"** |

**Certification verdict (Phase 10, independent — this session did not
author any Phase 1-9 code): CERTIFY**, with one finding routed forward
(not blocking today, blocking a specific future change — see
`DR-4HB1W2-01`) and no new composition gap found across the full 9-phase
chain. Full reasoning in this Phase's completion report to the human.

**What Phase 10 fixed (docs only, no domain-logic redesign):**
- `08 §16.15` item 1's genuine internal self-contradiction (wrote `Cr
  player_locked_bonus released_lock_amount` — literally restoring the
  lock — one sentence before stating restoration is "deliberately never
  attempted"). The same contradictory phrasing, being the source `08
  §16.15` quoted "verbatim" from, was also present in
  `ledger-accounting-model.md` §7.7.2.7 and is corrected there identically
  (ledger-finance's own frozen text, corrected by architect as a
  cross-cutting fix per CLAUDE.md's "no specialist redesigns shared
  architecture unilaterally... cross-cutting changes go through the
  architect" rule — this is a wording correction of already-decided
  content, not a new design decision). Both now state, unambiguously,
  matching the real code and §16.18's worked proof: both legs reverse
  straight to `house_gaming`; `player_locked_bonus` is never touched.
- `08 §16.9`/`§16.21` and `10 §N1.4.2`'s pseudocode signatures, reconciled
  against the real, shipped, tested Go signatures: `correlationID string`
  → `uuid.UUID`; `payoutAmount`/`releasedLockAmount decimal.Decimal` →
  `*big.Int` (this platform has no `decimal.Decimal` dependency anywhere);
  `RecheckGrantExposure` gained its missing `tenantID uuid.UUID`
  parameter. No implementation defect found — the code is correct per
  CLAUDE.md's own money-representation rule; only the illustrative
  pseudocode was stale.

**What Phase 10 found and did NOT fix — routed as a Dependency Request:**

| ID | Stage | Requesting task | Target domain | What's needed | Filed | Resolved |
|---|---|---|---|---|---|---|
| DR-4HB1W2-01 | 4H-B1 Wave 2 Phase 10 | Architect's independent composition review | bonus-engine | EOI/Risk lock-ordering reversal in `IssueSingleManualGrant`/`runBulkGrantJobItem` (see prior entry text for full detail). **RESOLVED** (commit `28e34db`): `ActivateGrant` gained an optional `PostGateHook`, invoked strictly after the T.1 gate chain and strictly before the effecting ledger write, still inside the same transaction. `ActivateGrant` stays EOI-agnostic for non-EOI bonus types (nil hook). Both EOI-gated call sites now thread `ConsumeRootBudget` through the hook. New regression test verified to fail against the pre-fix code and pass against the fix. Full validation floor (`-race -tags=integration`, all packages) run to completion twice, independently, zero regressions. | Stage 4H-B1 Wave 2 Phase 10 | **Closed** |
| DR-4HB1W2-02 | 4H-B1 Wave 2, DR-4HB1W2-01's own fix dispatch | bonus-engine (self-identified while fixing DR-4HB1W2-01) | bonus-engine | `economicop.ConsumeRootBudget`'s recipient-ceiling check was a structural no-op on the single-manual-grant surface: `bonus_grants` had no "realized" filter (unlike `bulk_grant_job_items`), so the executing attempt's own just-inserted row was always already counted, and the ceiling could never bite. This reopened the exact SEC-W15-02 decomposition vector through the single-grant door (the bulk door was correctly protected). **RESOLVED** (commit `af4f9cd`): added a `"bonus_grants"` entry to `consumptionRealizedFilter` excluding `'issued'`/`'cancelled'` rows (symmetric with the bulk surface's existing `'pending'`/`'denied'` exclusion). Two new regression tests (sequential + concurrent) verified to fail against the pre-fix code and pass against the fix; bulk surface confirmed unaffected. Full `-race -tags=integration` suite run to completion, zero regressions. Also surfaced (not fixed in this dispatch, fixed in Phase 11 below): `ConsumeRootBudget`'s value-budget branch referenced a `granted_amount` column that did not exist on `bonus_grants` at all — dormant/unreached since no caller sets a non-null value budget on this path yet. | DR-4HB1W2-01's fix dispatch | **Closed** |
| DR-4HB1W2-03 | DR-4HB1W2-02's fix dispatch | bonus-engine | ledger-finance | `bonus_grants` had no column to record a Grant's own posted `amountMinor`, needed for `ConsumeRootBudget`'s value-budget branch (referenced by name in docs 10/29/34 before any migration created it). **RESOLVED** (commit `3526d87`, Phase 11): migration `0067` adds `bonus_grants.granted_amount NUMERIC(38,0)`, nullable, non-negative, immutable-once-set (extends the existing immutability trigger), populated in `ActivateGrant` in the same transaction as the Grant's sole ledger posting. Migration round-trip verified. | DR-4HB1W2-02 | **Closed** |

| W2-P11 | ledger-finance | Done | W2-P1..P10, DR-4HB1W2-01/02 | migration `0067` (`bonus_grants.granted_amount`), `internal/bonus/lifecycle.go` (populate it), `docs/architecture/ledger-accounting-model.md` (new §7.7.2.12, final `ACTION_ROUTE_TO_CASH` ruling) | full validation floor run to completion (`-race -tags=integration`, all 27 packages, 3m50s) | §7.7.2.12 | LF-10 general case (rollback of an already-resolved financial event) remains open, correctly still ledger-finance's, fails closed today (no unsafe behavior, incomplete coverage); LF-12/B1(extended) reconciliation stream proven correct by test but not wired into the production scheduler (non-blocking, generic sweep already covers bonus accounts) |

**Final certification (Phase 11, independent financial sign-off): READY.** Every real posting shape traced against actual call sites (not re-derived from design docs) and confirmed balanced; `ACTION_ROUTE_TO_CASH`'s 4-leg posting shape (via Rule B2 firing unconditionally, since `player_bonus_held` is a BONUS_SET member) given a final, closing ruling — correct, no further ambiguity. Idempotency and reconciliation confirmed end to end. Both dependency-request fixes confirmed to carry no residual financial-correctness angle.

## Stage 4H-B1, Wave 3 — Bonus Engine Completion, Integration Hardening & Final Financial Gate (Phase 10 of 11 — `architect`, IN PROGRESS)

Commits `9d857dc`..`8c6ab7a` are Phases 0-9 (reconnaissance through `qa`).
This entry records **Phase 10 only** (`architect`, cross-domain
composition certification). Phase 11 (`ledger-finance`, final financial
certification) has not run; the Wave 3 gate report is the Orchestrator's,
not this entry's, and no verdict on the Wave as a whole is recorded here.

| ID | Owner | Status | Dependencies | Files owned | Tests | Docs | Blockers |
|---|---|---|---|---|---|---|---|
| W3-P10 | architect | Done | W3-P1..P9 | `internal/economicop/enforce.go` (`ValidateRootAuthorizationBounds`, `boundedRootOperationTypes`, `ErrUnboundedRootAuthorization`), `internal/bonus/targeting.go` (`MintRootOperation` calls it), `internal/bonus/wagering_contribution_entry.go` (contribution-weight clamp), 6 test fixtures updated | `internal/economicop/bounds_test.go` (new, unit), `internal/bonus/eoi_root_bounds_integration_test.go` (new), `TestResolveContributionWeightBP` (6 new cases). Both fixes verified to FAIL pre-fix by reverting them in place. Full validation floor: `go build`/`go vet` (both tag sets)/`gofmt` clean, `-race -tags=integration` on `economicop`/`bonus`/`casino`/`httpserver`, full repo-wide `-tags=integration` green (26 packages) | `docs/decisions/0040-wave-3-cross-domain-composition-rulings.md` (new), `docs/architecture/34-economic-operation-identity.md` (§5.6, §5.7, §3.1 note, EOI-19/EOI-20, two §8 rows, §7 status correction), `docs/architecture/29-bonus-implementation-contract.md` (BI-2 correction, BI-16), `docs/architecture/13-dependency-map-and-risk-register.md` (R16, R5 update, scheduled-items table), this entry | `DR-4HB1W3-ARCH-01` open and **gated** (below); `DEP-EOI-8` open and gated; eight further items routed to other owners, all recorded in doc 13 |

**Dependency Requests filed by Phase 10:**

| ID | Stage | Requesting task | Target domain | What's needed | Filed | Resolved |
|---|---|---|---|---|---|---|
| DR-4HB1W3-ARCH-01 | 4H-B1 Wave 3 Phase 10 | Architect's independent composition review | casino + bonus-engine | `internal/casino`'s `postBet` takes the `player_cash` `wallet_balance_projection` row lock (`lockCashBalance`) and posts (migration 0023's trigger write-locks every projection row touched) **before** taking the `(tenant_id, grant_id)` advisory lock — the reverse of every Bonus-domain path, which locks the Grant first and posts second. Both lock families **block**, so the two orders form an AB-BA cycle wherever they meet on a shared projection row. This is exactly the fourth participant `ledger-accounting-model.md` §6.6.16 predicted ("'no cycle today' is not a property that survives a fourth participant"). **Latent, not live**: the only Bonus paths posting to `player_cash`/`house_gaming` are `ConvertGrant` (zero non-test callers, no route), `ACTION_ROUTE_TO_CASH` and the two G-2 win/rollback seams (all unreachable without bonus-funded stake locking). Fix: resolve the qualifying Grant and take `AdvisoryLockGrant` before `lockCashBalance`. Not made in Phase 10 — it restructures a hot path across two domains, more than a certification phase should change this late in a Wave. **Rule recorded as HR-21 extended (doc 34 §5.6) + invariant EOI-20; risk R16 added.** | Stage 4H-B1 Wave 3 Phase 10 | **Open — GATED**: must land before any conversion trigger, before `postBet`'s bonus-funded leg, and before anything else making those paths concurrently reachable with a bet |
| DR-4HB1W3-ARCH-02 (= `DEP-EOI-8`) | 4H-B1 Wave 3 Phase 10 | Architect's independent composition review | bonus-engine + security | `bonus_campaign_activation` is declared in doc 34 §3.1 as a standing-authorization EOI type, with no mint point, no `consumptionShapes` entry and no `boundedRootOperationTypes` entry — while Wave 3's deposit sweep and cashback scheduler made the automatic, campaign-driven grant-issuing effecting writes it was declared to bound live for the first time. One four-eyes-approved activation therefore authorizes unbounded automatic issuance. Inert today only because every sweep-driven issuance denies at `AssetAuthorization` (no per-player jurisdiction resolver exists). Either mint the EOI with declared bounds, or record a `security`-co-signed determination of what bounds it instead. | Stage 4H-B1 Wave 3 Phase 10 | **Open — GATED**: before campaign-driven automatic issuance can actually succeed, i.e. as part of whatever closes the sweeps' jurisdiction-resolution gap |
| DR-4HB1W3-ARCH-03 | 4H-B1 Wave 3 Phase 10 | Architect's independent composition review | bonus-engine (write-path half) | `contribution_weight_table` had no validation at any layer, and `ResolveContributionWeightBP` neither validated nor clamped, so migration 0058's column `CHECK (contribution_weight_bp BETWEEN 0 AND 10000)` was the only enforcement — firing at the END of the chain, as a raw constraint error that propagated out of `postBet` and **rolled back the player's own cash bet**. One mistyped Offer field was a live betting outage. **Bet-time half RESOLVED in Phase 10** (commit `5af05d2`: table-derived weights clamped into `[0, 10000]`, the value-reducing direction only, restoring symmetry with `DR-4HB1W3-RISK-02`'s own posture). **Write-path half open**: reject an unparseable/out-of-range table at Offer-version creation, and audit-signal the unparseable case at bet time (ADR 0040 D3 ruling items 1 and 3). | Stage 4H-B1 Wave 3 Phase 10 | **Partially closed** (bet-time clamp shipped); write-path validation routed to `bonus-engine` |

**Phase 10 certification verdict: CERTIFY WITH GATED FINDINGS.** Wave 3's
Phases 1-9 compose correctly — no cross-domain contradiction was found
that makes shipped behavior wrong today. Three composition findings no
single phase could have caught (`DR-4HB1W3-ARCH-01`/`-02`/`-03`) are
recorded with owners and gates; two doc-vs-code contradictions created or
exposed by this Wave were corrected in place (doc 29's BI-2 precondition;
doc 34 §7's status). Every item `risk` (Phase 4) and `security` (Phase 6)
routed to `architect` by name is disposed of in ADR 0040. **This verdict
covers cross-domain architectural composition only** — it is not a
financial certification (Phase 11, `ledger-finance`) and not a stage-gate
authorization (human, via the Orchestrator).

## Stage 4H-B1, Wave 3 — Phase 11 of 11 (`ledger-finance`, final financial certification — COMPLETE)

The last specialist phase of Wave 3. Records `ledger-finance`'s
independent certification of the Wave's financial surface only. **The
Wave 3 Gate Report is the Orchestrator's synthesis across all eleven
phases and is not this entry**; nothing here authorizes a stage
transition, and no Human Decision Register item is resolved.

| ID | Owner | Status | Dependencies | Files owned | Tests | Docs | Blockers |
|---|---|---|---|---|---|---|---|
| W3-P11 | ledger-finance | Done | W3-P1..P10 | `internal/bonus/schedulers.go` (`dbClockTimestamp`; `RunCashbackSweep`/`RunExpirySweep` now read the DB clock per tenant transaction), doc-comment corrections in `internal/bonus/cashback_scheduler.go` and `internal/bonus/expiry_sweep.go` | `internal/bonus/wave3_ledger_finance_certification_integration_test.go` (new, 5 tests): the first SUCCESSFUL postings via the deposit and cashback chains with B1/Rule-B2/rounding/`granted_amount`/reconciliation assertions, duplicate-replay for both, the `never now()` regression guard (verified to fail pre-fix), expiry write-down B1, and deposit-sweep partial-failure + audit-row coverage. Validation floor: `go build`/`go vet` (both tag sets)/`gofmt` clean; full unit suite green; full repo `-tags=integration` green (exit 0, 26 packages); `-race -tags=integration` green on `bonus`/`casino`/`reconciliation` | `docs/architecture/ledger-accounting-model.md` §7.18 header + §7.18.7 (stale-status correction routed here by ADR 0040), §7.18.8 (certification outcome, `LF-W3-01`/`-03`/`-04`), §7.18.9 (`DR-4HB1W3-ARCH-01`/LF-10/RC-4 confirmations), §7.14 HR-26 (ADR 0040 D2 mirrored into this document's HR catalogue), §7.19 (RC-1/RC-3 posting shapes, DESIGN ONLY), this entry | One item escalated to the Orchestrator as an open **business** decision (below). `DR-4HB1W3-ARCH-01`/`-02` remain open and gated as `architect` left them — independently re-verified, not re-opened |

**Fixes made this phase:**

| ID | What | Proof |
|---|---|---|
| DR-4HB1W3-LF-01 | The cashback scheduler and the expiry sweep compared their windows against Go's `time.Now()` — read once in the API process, before the tenant loop — while three doc comments claimed the value was a `clock_timestamp()` read. Every instant they compare *against* (`ledger_transactions.posted_at`, `bonus_grants.expires_at`) is DB-written. With the API host's clock ahead of the database, a cashback window settles before it has elapsed in `posted_at` terms and permanently excludes every bet posted inside the skew interval — a silent under-payment in the platform's favour; the same skew expires a Grant before its advertised deadline. Fixed: `dbClockTimestamp` reads `clock_timestamp()` inside each tenant's own transaction. | The `never now()` half is a genuine failing-pre-fix regression test (verified by swapping the helper to `SELECT now()` and observing the failure). The `never time.Now()` half is deployment-topology-dependent and **cannot** be reproduced on a single-host rig — stated in the helper's own doc comment rather than implied to be proven. |

**Findings recorded, deliberately not fixed** (each with its exact
reachability stated in `ledger-accounting-model.md` §7.18.8):
`LF-W3-01` (migration 0057's idempotency constraint includes
`offer_version_id`, so both new triggers dedupe only *per Offer
version* — routed to `bonus-engine`; no live exposure, since the
watermark tables carry no RLS `DELETE` policy and nothing tenant-scoped
can re-scan a processed event), `LF-W3-03` (the cashback net-loss
query's nullification join fails **open** on an unclassified reversal
where §6.6.5 fails closed — unreachable today, and a blanket widening
would be wrong because the conservative direction is not uniform across
the bet and win legs), `LF-W3-04` (a rollback arriving after a cashback
window is settled cannot retroactively reduce it — inherent to window
settlement, bounded, over-credits the player).

**Escalated to the Orchestrator — open BUSINESS decision, not an
engineering one.** `grant_cancel_completed`'s posting shape is specified
for a `completed` Grant (§7.19.3). For a **`converted`** Grant it is
deliberately **not** specified: the value is in `player_cash`, fungible,
possibly already staked or withdrawn, so clawing it back debits a real
player balance and can create a **receivable from a customer**. Whether
this platform ever creates player receivables, under which
jurisdictions' consumer-protection rules, and what recourse exists when
the balance is insufficient, carries legal and insurance weight that
`ledger-finance` does not decide alone — and inventing a posting shape
would decide it by implication.

**Phase 11 certification verdict: CERTIFY.** Wave 3's financial surface
is sound. `SUM(DEBITS) == SUM(CREDITS)` now holds on **real postings**
from both new mechanisms rather than trivially over the empty set (the
gap `qa` Phase 9 §4 disclosed); no balance is ever `UPDATE`d; no
floating point exists anywhere in the Wave's arithmetic; every financial
write remains DB-constraint-idempotent; no historical ledger or audit
row is edited or deleted; and both new posting-adjacent paths
participate in the existing reconciliation sweep automatically, proven
by running it. **This verdict covers financial invariants only** — it is
not a security, architectural or QA sign-off, and it is not a stage-gate
authorization.

## Stage 4I — Platform-wide Jurisdiction Resolution Foundation

**Carried debt and dependency requests, recorded durably.** Stage 4I's
carried-debt items were named in `docs/governance/stage-4i-canonical-
model.md` §13.6 (`architect`'s final cross-domain certification) and in
`docs/security/security-architecture.md` §J4I.10 (`security`'s final
independent certification). They are restated here, in the registry, per
the Wave 3 convention — a stage document is a stage artefact, and an item
that lives only inside one is an item a future stage will not find.

**None of these blocks Stage 4I's certification.** Each has an owner and a
trigger. Nothing here decides a Human Decision Register item.

| ID | Stage | Requesting task | Target domain | What's needed | Filed | Resolved |
|---|---|---|---|---|---|---|
| DR-4I-ARCH-01 | 4I | `architect`, final cross-domain certification | `architect` (self) | The resolver-side half of the canonical model's §4.4 Layer 2 tenant-scope assertion was never implemented, and was load-bearing rather than decorative: `tenants`, `licences` and `jurisdictions` carry **no row-level security at all**, so `Resolve` returned a fully `Resolved` Resolution carrying tenant A's jurisdiction to a transaction that had only ever proven tenant B, on the strength of a caller-supplied argument. `Resolution.AssertScope` cannot substitute — it compares the resolution's binding against the caller's own arguments. **RESOLVED** (commit `fb632d7`): `assertTenantScope` in `internal/jurisdiction`, a behavioural mirror of `internal/assetregistry`'s function of the same name, placed inside the one branch that can return `Resolved` and the one branch that queries at all. Mismatch → `refused(scope_mismatch)`; no scope at all → a Go error. Five new tests including a real-database cross-tenant case with an anti-inertness control. **Independently re-verified by `security` in the closing phase**: the no-RLS premise confirmed directly against `pg_class.relrowsecurity` and by issuing the resolver's own two queries from a cross-tenant connection (both returned the other tenant's data); the assertion confirmed to hold no lock and issue no row-locking read, so H-2's read-only-on-the-evaluation-path constraint is intact | Stage 4I `architect` closing phase | **Closed** |
| DR-4I-BONUS-01 | 4I | `architect`, final cross-domain certification | `architect` (self) | The last hand-copied force helpers on the deposit-bonus and cashback paths, closed by applying §13.5's seam convention. **RESOLVED** (commit `c481e2a`) | Stage 4I `architect` closing phase | **Closed** |
| DR-4I-SEC-01 | 4I | `architect`, final cross-domain certification (deliberately re-deferred, not dropped) | `security` | Fold the Stage 4I security model's §S-2 / §S-3 / §S-6 into `docs/security/security-architecture.md` as a permanent numbered section. **RESOLVED** (commit `ee3908a`, `security`'s final certification phase): landed as §J4I.1–J4I.11, with three corrections made in place rather than transcribed (the TRUNCATE rule applies to every such table because RLS does not cover TRUNCATE; the "bare connection reads zero rows under FORCE RLS" safety net is false on the resolver's own path; §S-3.2's "a tenant-scoped transaction cannot write `jurisdictions`" assertion is unachievable and is withdrawn), plus per-case coverage disclosure for every §S-6 adversarial case and the full SEC-4I-F1…F11 findings register | Stage 4I `architect` closing phase | **Closed** |
| DR-4I-BE-01 | 4I | `architect`, final cross-domain certification | `backend` + each consuming domain | **`jurisdiction.Persist` has ZERO production call sites.** `jurisdiction_resolutions` is never written outside tests, so §5.1's "one row per resolution attempt, including failures" is **specification, not behaviour**. Deliberately carried, with reasons: in Stage 4I every player-scoped resolution is `unresolved(no_signal)`, so a per-launch row would be an unbounded, player-triggerable stream of identical rows into an append-only table with no delete path and no pruning — the same amplification hazard forbidden for `audit_log`, for less evidentiary value. **Trigger:** wire it when a resolution can carry a real basis (HDR-J-3), or earlier if a consumer needs `AR-2`'s reference. **A future phase must not read §5.1, or `security-architecture.md` §J4I.1/J4I.2, and assume the table is populated** | Stage 4I `architect` closing phase | Open |
| DR-4I-BE-02 | 4I | `architect`, final cross-domain certification | `casino`, `bonus-engine` | **`AR-2`'s `jurisdiction_resolution_id` FK** on `casino_launch_sessions` / `bonus_grants` is not built. Lands with `DR-4I-BE-01`; meaningless before it. The §6.4 condition it was to discharge is presently moot — casino resolves *inside* the guarded transaction, so the TOCTOU window the FK was to make visible does not currently exist. **`security` note:** this is also the change that first makes a resolution reusable across transactions, which is the exact trigger for the staleness bound in `security-architecture.md` §J4I.8 row 7 and the §J4I.9 category-C tests. Both are owed **with** this change, not after it | Stage 4I `architect` closing phase | Open |
| DR-4I-RISK-01 | 4I | `architect`, final cross-domain certification | `risk`, after `security` review | **B-7 / R-2b not implemented.** `jurisdiction.IsActive` exists, is tested, and has **zero production callers**; `risk.CreateRule` still accepts a jurisdiction-scoped rule for a `(tenant, operation)` pair not recorded as resolution-active. `risk` had no implementation phase in this stage's sequence. Until it lands, the fixed activation order plus the enumeration check remain the fallback — weaker but acceptable, and not a resolver blocker. Carries the `§J4I.9` case G-6 assertion with it | Stage 4I `architect` closing phase | Open |
| DR-4I-QA-01 | 4I | `architect`, final cross-domain certification | `qa` | **§6.4's read-only enforcement mechanisms 2 and 3 are absent**: no test resolves inside `BEGIN … READ ONLY`, and no test queries `pg_locks` after a resolution. Mechanism 1 (compile-time, via the `Query`/`QueryRow`-only interface the resolver accepts) is the only one in force. It is the strongest of the three and genuinely holds, so H-2 is not unguarded — but mechanism 3 exists specifically to catch a `SELECT … FOR SHARE` that mechanisms 1 and 2 both permit, and nothing catches that today. Same item as `security-architecture.md` §J4I.9 case F-4 | Stage 4I `architect` closing phase | Open |
| DR-4I-QA-02 | 4I | `architect`, found while mutation-testing `DR-4I-BONUS-01` | `qa`, with `ledger-finance` input | `DepositBonusParams.MaxQualifying`'s clamp branch is **untested** — removing the clamp fails no test in the repository. A monetary-boundary branch with no coverage. Pre-existing; not introduced by Stage 4I | Stage 4I `architect` closing phase | Open |
| DR-4I-BONUS-02 | 4I | `architect`, final cross-domain certification | `bonus-engine`, `qa` | **Test-seam expiry.** The sanctioned seam convention (§13.5) exists because a *temporary* condition — every player-scoped resolution being unresolved — makes the real gate chain unreachable. When HDR-J-1/HDR-J-3 are answered and player-scoped activation can resolve, **every use of the activation force adapter must be re-examined and re-pointed at the real gate chain wherever the test's own subject does not require the bypass.** They are not a permanent licence to test around the gate. `security`'s HTTP-test tripwire (asserting *which* denial is returned, so the test fails once the four-eyes branch becomes reachable) is the right mechanism for the expiry and should be imitated, not removed | Stage 4I `architect` closing phase | Open |
| SEC-4I-F9 | 4I | `security`, final independent certification | `casino` + `architect` | A per-game jurisdiction blocklist entry is compared to a resolved jurisdiction code by **exact, case-sensitive string match**, and nothing validates that a blocklist entry is a real `jurisdictions.code`. A blocklist of `["mt"]` against a registry code of `MT` leaves the game *armed* but that specific block silently inert. Registry codes are not case-normalised on the write path either, so `MT` and `mt` can both exist as distinct jurisdictions. Severity MEDIUM when reachable; **inert today**, because an armed game denies every launch while all player-scoped resolutions are unresolved, so the mismatch cannot manifest. Deliberately not fixed unilaterally: whether an unknown code should be rejected at write time or permitted for a not-yet-registered jurisdiction is a registry-model decision, not `security`'s. **Trigger: HDR-J-3** | Stage 4I `security` closing phase | Open |
| SEC-4I-F10 | 4I | `security`, final independent certification | `backend` + `architect` | The resolver's licence lookup joins `licences` → `jurisdictions` **without filtering on `licences.status`**. A suspended or expired licence would still yield `resolved` with `authoritative` confidence — a compliance fail-open, since the tenant would no longer be licensed in that jurisdiction. Currently **unreachable**: the registry write surface deliberately exposes no status-transition operation and the column can only hold its `'active'` default. Severity LOW today, MEDIUM once reachable. Not fixed unilaterally because the correct outcome for a suspended licence (`refused(dependency_unavailable)` vs. `unresolved`) is a jurisdiction-model semantic decision. **Hard trigger: any change that introduces a licence status transition must land with a resolver-query change and a security review, in the same change.** The existing "no status-transition operation is exposed" comment in the registry admin surface does not flag this coupling and should. **Stage 4I Phase A note (architect, `docs/plans/stage-4i-jurisdiction-implementation-plan.md` Phase A):** Phase A added `internal/jurisdiction.AssignTenantLicence`, which rejects binding a non-`'active'` licence to a tenant — but this is a bind-time (assign) check only, not a resolve-time (evaluation) check. It does NOT close this finding: `resolveTenantLicence` still performs no status filtering, so a licence suspended/expired AFTER assignment still resolves `authoritative` with no change of behaviour. The hard trigger above is unchanged and remains the closing condition | Stage 4I `security` closing phase | Open |
| SEC-4I-F11 | 4I | `security`, final independent certification | `architect` | Platform reference tables (`jurisdictions`, `licences`, `casino_games`) carry no row-level security and therefore **no database-level write control at all** — the HTTP permission check is the entire control on the write side. Verified directly: a tenant-scoped transaction can `INSERT` into `jurisdictions` today. This is a pre-existing, accepted platform posture and **not** a Stage 4I regression, but it is materially weaker than every tenant-owned table in this platform, and the Stage 4I security model's own §S-3.2 contained a contradiction about it (it required both "no RLS" and an RLS-enforced write denial). Withdrawn and restated honestly in `security-architecture.md` §J4I.6. Whether to tighten the posture (e.g. a policy permitting `SELECT` to all scopes and `INSERT`/`UPDATE` only to an unscoped connection) is cross-cutting and belongs to `architect`. Severity LOW. **Scope widened, Stage 4I Phase A (architect):** `tenants` now also carries an application write surface with no RLS backing it — `internal/jurisdiction/tenant_licence_admin.go`'s `AssignTenantLicence` (`PUT /v1/admin/tenants/{tenantID}/licence`, `PermTenantLicenceAssign`). `tenants` joins `jurisdictions`/`licences`/`casino_games` in this finding's scope | Stage 4I `security` closing phase | Open |

**Fixes made by `security` in the closing phase:**

| ID | What | Proof |
|---|---|---|
| SEC-4I-F8 | No `BEFORE TRUNCATE` deny trigger on `jurisdiction_resolution_active`. **PostgreSQL RLS does not apply to TRUNCATE at all** — it is governed only by the TRUNCATE privilege, which the application role holds implicitly by owning the tables. So every control migrations 0071/0072 placed on that table, including SEC-4I-F4's own deliberately-absent DELETE policy, was bypassable by one statement from an ordinary tenant-scoped — or even player-scoped — connection, erasing **every tenant's** resolution-active facts unaudited while `audit_log` continued to record each one as set. The same hazard F4 closed for DELETE, through a different command, with a larger blast radius | Migration `0073` (commit `d24331f`), following `asset_authorizations_no_truncate`'s precedent. Pre-fix proof by migration round-trip, not inspection: with 0073 rolled back the new test fails ("expected TRUNCATE to be rejected"); re-applied, it passes. The test covers the tenant-scoped and player-scoped connections and carries an anti-inertness control |
| — (no finding) | The two new permissions' role scoping had **no test anywhere** — the platform/tenant split rested on reading the map by eye. This matters more than usual for `PermJurisdictionRegistryManage`, because `jurisdictions`/`licences` have no RLS behind it (see SEC-4I-F11), so the permission check is the entire control | `internal/auth/jurisdiction_permission_test.go` (commit `c053b07`): four tests iterating every role, so a role added later cannot silently acquire either permission |

## Stage 4I Phase A — tenant-licence write path (`docs/plans/stage-4i-jurisdiction-implementation-plan.md` Phase A)

Implementation dispatch: `architect` design ruling → `backend` implementation
→ independent `security`/`architect`/`qa` review → `backend` fix round
→ orchestrator integration. Closed the write-path gap Phase A targeted:
`internal/jurisdiction/tenant_licence_admin.go`'s `AssignTenantLicence`,
`PUT /v1/admin/tenants/{tenantID}/licence`, `PermTenantLicenceAssign`
(platform-admin-only).

**Findings from the independent review round, and disposition:**

| ID | Severity | Finding | Disposition |
|---|---|---|---|
| PHASE-A-SEC-1 | P2 | `security`: the audit record for this tenant-targeted mutation was written platform-scoped (`tenant_id = NULL`, via the shared `recordRegistryAudit` helper), so the affected tenant could never see it via its own `PermAuditRead` — unlike every other "platform_admin acts on a target tenant" handler in this codebase (brand/staff creation), which writes a tenant-scoped audit row | **FIXED** in the same-session fix round: `recordRegistryAudit` gained a `tenantID` parameter (`uuid.Nil` for `CreateJurisdiction`/`CreateLicence`, the target tenant for `AssignTenantLicence`); the HTTP handler now runs under `deps.DB.WithTenant(tenantID, ...)` instead of `WithPlatformAdmin`, matching `newCreateBrandHandler`'s precedent |
| PHASE-A-SEC-2 | P2 | `security`/`architect` (converges with SEC-4I-F10): `licences.status` is checked only at bind time (`AssignTenantLicence`); the resolver never re-checks it at evaluation time, and `licences.expires_at` is never checked anywhere. A licence suspended/expired after assignment still resolves `authoritative` | **NOT FIXED — deliberately deferred.** SEC-4I-F10's hard trigger (resolver-query change + security review, landing together with any status-transition operation) already covers this; a sentence was appended to SEC-4I-F10 above making explicit that Phase A's bind-time check does not close it |
| PHASE-A-SEC-3 | P2 | `security`: `licences` has no `owner_tenant_id` (or equivalent) — the composite FK enforces `licensee ↔ licensing_model` but not *which* `own_licence` tenant a `'tenant'`-licensee licence belongs to. Verified empirically: two distinct `own_licence` tenants can be bound to the SAME licence row with no constraint violation | **NOT FIXED — routed to `architect`, open decision, not a Phase A blocker.** No BYOL tenant exists yet (HDR-J-5 "not urgent"). Recorded as a named prerequisite: before the first BYOL tenant is onboarded, `architect` + compliance must decide between (a) a schema addition binding a `'tenant'`-licensee licence to its owning tenant, or (b) an explicit recorded decision that this correctness is operator-verified/audit-reviewed, not machine-enforced |
| PHASE-A-SEC-4 | P2 | `security`: no dual control (four-eyes) on this write, whose only control is a single permission check on tables (`tenants`/`licences`) with no RLS — the same condition Stage 4H-B0-R5 finding S-3 required dual control for on the asset registry | **NOT FIXED — routed to `architect`/`product-owner-proxy`, open decision, not a Phase A blocker.** Either extend the existing `asset_change_requests` four-eyes mechanism to cover `tenant_licence:assign`, or record an ADR accepting single-control for this operation with a stated reason |
| PHASE-A-QA-1 | P1 | `qa`: the BYOL (`own_licence`/`licensee='tenant'`) success path had zero test coverage at either layer — every success-path test only exercised `licensing_model='under_platform_licence'` | **FIXED** in the fix round: a new package-level test exercises the `own_licence`/`licensee='tenant'` success path end to end |
| PHASE-A-QA-2 | P1 | `qa`: none of the 10 HTTP-layer tests ever drove `AssignTenantLicence`'s own domain-error branches (unknown licence, non-active licence, licensee mismatch, unknown tenant) — including the codebase's only `ErrNotFound` producer, never exercised over HTTP anywhere | **FIXED** in the fix round: 4 new HTTP-layer tests added, one per domain-error branch, asserting the exact expected status code |
| PHASE-A-ARCH/QA-1 | P2 | `architect` + `qa` (convergent, independently found): the concurrency test asserted "no deadlock, no corruption" but not the actual anti-stale-before-state property the `FOR UPDATE`/`FOR SHARE` locking was written to guarantee — the same assertions would pass with the locking removed | **FIXED** in the fix round: the test now additionally asserts the two audit rows form a genuine before/after chain |
| PHASE-A-SEC-5 | P3 | `security`: `TestAssignTenantLicenceAPI_ForgedExtraFieldRejected`'s framing over-claimed general field-smuggling protection; Go's `encoding/json` + `DisallowUnknownFields` matches struct tags case-insensitively, so a case-variant key would not be caught (pre-existing, platform-wide `decodeJSON` characteristic, not introduced here) | **FIXED (doc-only):** the test's doc comment was tightened to claim only what it actually proves |
| PHASE-A-SEC-6/7/8/10 | P3 | `security`: `decodeJSON` ignores trailing JSON content after the first value; denied (403) attempts are not audited; `reason_code` is unbounded free text; error messages return `err.Error()` verbatim to the caller (a controlled, hand-written message in every case — never raw SQL/stack traces) | **NOT FIXED — pre-existing, platform-wide conventions, not introduced by Phase A, not blocking.** Recorded here so they are not re-discovered as "new" in a future stage |
| PHASE-A-ARCH-2 | P3 | `architect`: the entire Stage 4I jurisdiction-admin OpenAPI surface (`/v1/admin/jurisdictions`, `/v1/admin/licences`, `/v1/admin/jurisdiction-resolution-active/**`, and now `/v1/admin/tenants/{tenantID}/licence`) is absent from `docs/api/openapi/platform-api.yaml` | **CLOSED.** All four Phase A endpoint groups (7 operations across 5 path templates - `GET`/`POST /v1/admin/jurisdictions`, `GET`/`POST /v1/admin/licences`, `GET /v1/admin/jurisdiction-resolution-active`, `PUT /v1/admin/jurisdiction-resolution-active/{operationClass}`, `PUT /v1/admin/tenants/{tenantID}/licence`) are now documented in `docs/api/openapi/platform-api.yaml`, each with its exact permission/role requirement, request/response schemas (`Jurisdiction`, `Licence`, `JurisdictionResolutionActive`, `AssignTenantLicenceRequest`, `TenantLicenceAssignment`), and a path-group note stating there is deliberately no HTTP jurisdiction-resolution endpoint. **Correction (Stage 4I Phase B independent architect review, finding F5):** an earlier version of this row claimed the backfill landed "as an isolated commit before Phase B" - that commit never existed; the backfill and Phase B's own new-surface documentation were both made in the same working tree and are landed together in the Stage 4I Phase B commit series (see Phase B section below for the commit hash). The row is corrected here rather than left asserting a git fact that did not hold |

**Verdicts, all independent, none self-certified:** `security` — CERTIFIED WITH NAMED EXCEPTIONS (no P0/P1). `architect` — ARCHITECTURALLY CERTIFIED, with named exceptions (no blocking issues; code quality noted as exceeding the ruling's own spec in several places). `qa` — READY WITH NAMED GAPS, both P1s closed in the fix round.

## Stage 4I Phase B — player jurisdiction evidence foundation (the human directive's own "Phase B" label — see the phase-lettering correction note in `docs/plans/stage-4i-jurisdiction-implementation-plan.md` §14 for how this maps to that document's own A–I lettering, roughly its Phase E plus a narrowed slice of Phase F)

Implementation dispatch: `architect` design ruling (12 numbered rulings)
→ parallel `backend`/`integrations`/`identity-compliance` implementation
→ independent `security`/`architect`/`qa` review → orchestrator fix
round → orchestrator integration. Built the technical evidence
foundation for player-level jurisdiction determination per
HDR-J-3a/b/c/e/f/g/h: declared residence
(`player_accounts.declared_residence_*`, `PUT`/`GET /v1/me/residence`),
KYC-verified residence (`kyc_verifications.verified_residence_*`,
extending the existing `POST /v1/admin/kyc/verifications/{id}/review`),
and a physical-location signal abstraction (`internal/geolocation` —
interface + mock only, no vendor, no HTTP surface, no resolver wiring).
`internal/jurisdiction/resolver.go` has zero diff — evidence is not yet
consumed by any jurisdiction decision.

**Findings from the independent review round, and disposition:**

| ID | Severity | Finding | Disposition |
|---|---|---|---|
| PHASE-B-SEC-1 / PHASE-B-ARCH-F4 / PHASE-B-QA-1 | P1 | `security`, `architect`, and `qa`, independently converging: the `kyc.verified_residence_determined` audit entry included the reviewer's free-text `reason` field verbatim — the one channel that could carry the country value the entry's own "never record the value" comment forbade two lines above it. `qa` independently found the matching test-coverage gap (no exact-shape audit test on the KYC side, unlike `internal/identity`'s equivalent) | **FIXED.** The `reason` key was removed from this metadata map (it remains, correctly, on the sibling `kyc.verification_status_changed` entry, written in the same transaction). A new test, `TestReviewVerification_VerifiedResidenceDeterminedAuditRecordShape`, deliberately puts the country in the reviewer's free-text reason and asserts the raw audit row for this specific action never contains it |
| PHASE-B-QA-2 | P1 | `qa`: no test exercised the four new paired-NULL CHECK constraints or the `verified_residence_set_by` FK directly via raw SQL — the established convention in this same codebase (`internal/jurisdiction`'s own CHECK-constraint tests) was not followed here, even though application code always writes the paired columns together today | **FIXED.** Four new raw-SQL tests added: `TestKYCVerifications_VerifiedResidenceCountryPairCheckConstraint`, `..._VerifiedResidenceSetByPairCheckConstraint`, `..._VerifiedResidenceSourcePairCheckConstraint`, `..._VerifiedResidenceSetByForeignKeyConstraint` (kyc package), plus `TestPlayerAccounts_DeclaredResidencePairCheckConstraint` (identity package) |
| PHASE-B-QA-3 / PHASE-B-SEC-8 | P1 / P3 | `qa`: no cross-tenant isolation test existed for `kyc.GetVerifiedResidence`, unlike its sibling `identity.GetDeclaredResidence`. `security` independently flagged a related design trap: the function took `tenantID` as a plain caller-supplied argument used in its own `WHERE` clause, in addition to relying on RLS — asymmetric with `identity.GetDeclaredResidence`'s better shape (RLS alone, no `tenantID` parameter), and a caller passing a mismatched `tenantID` would get a silent `ok=false` rather than a loud error | **FIXED, root cause.** `kyc.GetVerifiedResidence`'s signature was changed to drop the `tenantID` parameter entirely, relying on RLS alone (matching `identity.GetDeclaredResidence`) — this closes both the missing-test gap and the design trap in one change, since the caller-supplied-mismatch class of bug is now structurally impossible. `TestGetVerifiedResidence_CrossTenantReadReturnsNotOK` added. (Safe: the function had zero non-test callers at the time of the change, confirmed by grep) |
| PHASE-B-SEC-4 / PHASE-B-ARCH-F1 | P2 | `security` and `architect`, independently: the two write paths' activation-gate enforcement is asymmetric. The KYC path's gate is inside `ReviewVerification` itself (unreachable-around). The declared-residence path's gate is enforced only in the HTTP handler; `identity.SetPlayerAccountDeclaredResidence` is an exported, ungated function protected only by a doc comment — enforcement by application-code discipline, which CLAUDE.md rejects for exactly this class of control | **CLOSED by the PHASE-B-ARCH-1 hardening gate.** The architect's design ruling for that gate (see the PHASE-B-ARCH-1 section below) REJECTED the `BEFORE INSERT OR UPDATE` trigger remedy this row originally recorded as "preferred" — the trigger would need to read `jurisdiction_evidence_collection_active`, whose RLS policies are invisible to a player-scoped connection, so the trigger would either misfire on legitimate player-scoped writes or require a `SECURITY DEFINER` RLS-bypassing function, a net security regression. The canonical fix instead moved the check INTO `identity.SetPlayerAccountDeclaredResidence` itself (mirroring `kyc.ReviewVerification`'s own gate exactly, in the same transaction as the write), accepting `internal/identity -> internal/jurisdiction` as a new, cycle-free import (`internal/jurisdiction` depends on neither `internal/identity` nor `internal/kyc`). The HTTP handler's own duplicate check was removed, not kept as defence-in-depth, closing the asymmetry rather than doubling it. See the PHASE-B-ARCH-1 section below for the full implementation, tests, and independent review disposition |
| PHASE-B-SEC-2 | P2 | `security`: `effective_from`/`created_by_actor_id` on `jurisdiction_evidence_collection_active` are set only on first INSERT, never updated on a subsequent toggle — the table can misreport both when a lawful-basis clearance took effect and who last changed it. Verified live (an `active=false` toggle by a different actor left both fields showing the original INSERT's values) | **NOT FIXED — routed to `architect`, cross-cutting.** The identical defect exists on the sibling `jurisdiction_resolution_active` table (a purely-engineering fact, where the gap matters less); fixing one without the other would leave an inconsistent pair. Recorded as a named prerequisite for whichever future change next touches either table's audit-provenance columns |
| PHASE-B-SEC-3 | P2 | `security`: (closed by PHASE-B-SEC-1's fix, recorded separately as it was found independently) the KYC audit entries this phase writes carried no `IPAddress`/`UserAgent`/`RequestID`, unlike the declared-residence path's equivalent entry — a gap on the *more* privileged of the two paths, since it is a staff act against another person's data | **FIXED.** `ReviewVerificationParams` gained `IPAddress`/`UserAgent`/`RequestID` fields, populated from the HTTP handler and recorded on both audit entries this call writes (also closes the same gap on the pre-existing `kyc.verification_status_changed` entry, as a side effect, not a scope expansion) |
| PHASE-B-SEC-6 / PHASE-B-SEC-7 | P3 | `security`: no row lock (`FOR SHARE`) on the evidence-collection-active gate read, and no row lock (`FOR UPDATE`) on `ReviewVerification`'s own status pre-check — both are check-then-act with a narrow TOCTOU/race window under concurrent admin actions. The `ReviewVerification` race is pre-existing (predates Phase B; the diff only added columns to an already-unguarded UPDATE) | **NOT FIXED — deferred, named, low likelihood.** Both require a deliberate concurrent admin action (a compliance toggle mid-write, or two staff reviewing the same verification simultaneously) to manifest, both are fully audited either way, and the `ReviewVerification` race is explicitly out of Phase B's own scope (a pre-existing `internal/kyc` characteristic). Tracked as a named gap for whoever next touches either code path, not a Phase B blocker |
| PHASE-B-SEC-10 | P3 | `security`: the `captured_at`/`determined_at` audit-metadata timestamps used a separate Go-side `time.Now()` call rather than the database's own timestamp already available from the same statement's `RETURNING`, creating a forensic clock-skew risk between the audit row and the underlying column | **FIXED.** Both `SetPlayerAccountDeclaredResidence` and `ReviewVerification` now return the database's own timestamp via `RETURNING` and use it in the audit metadata, eliminating the two-clock discrepancy |
| PHASE-B-ARCH-F9 | P4 | `architect`: `provenanceFromActorType`'s doc comment claimed it prevents a future third `ActorType` from "silently producing an unrecognized provenance value," but the switch that calls it already rejects any non-player/non-staff actor type before this function is ever reached, making its `default` branch unreachable — the comment overclaimed relative to the actual code | **FIXED (doc-only).** The comment was corrected to state the actual, narrower property (the default branch is unreachable given the caller's own validation, and exists only so the function still compiles/returns something sane if that guard is ever loosened) |
| PHASE-B-QA-7 | P3 | `qa`: a test in `internal/geolocation` was misnamed `TestSignalResult_FieldsAreExactlyTheFiveDocumented` when `SignalResult` has exactly four fields — a copy-paste artifact from the doc comment's five *forbidden example* fields (coordinate/city/ISP/ASN/postal code), not a functional defect | **FIXED (rename-only).** Renamed to `TestSignalResult_FieldsAreExactlyTheFourDocumented`; the test's actual assertion (a reflection-based field-count/type guard) was already correct |
| PHASE-B-ARCH-F3 | P3 (documentation accuracy) | `architect`: the design ruling's own claim that a verified-residence determination "can only be made in the SAME call that reaches a terminal status" is false as implemented — `ReviewVerification` also accepts the non-terminal `StatusReviewRequired` as a target, and a determination made there is later overwritable by a subsequent review call. The implementation is correct; the ruling's stated consequence was not | **Not a code change — documentation correction.** The true property, stated correctly here and in this phase's completion report: a determination is always bound to a reviewer act on a non-terminal-at-the-time verification, is overwritable only by another such act, and is only ever *read back* (`GetVerifiedResidence`) once the verification reaches `approved` — the `status = 'approved'` filter in that accessor is what makes an inert determination on a since-rejected verification unreachable, not any restriction on when a determination can be recorded |
| PHASE-B-ARCH-F5 | P3 | `architect`: `task-registry.md`'s `PHASE-A-ARCH-2` row (above) asserted an "isolated OpenAPI backfill commit" landing before Phase B that did not exist at review time — both were still uncommitted in the same working tree | **FIXED (doc-only).** The `PHASE-A-ARCH-2` row above was corrected to state the actual git history: the Phase A backfill and Phase B's own new-surface OpenAPI documentation are both part of this phase's own commit series, not a separate prior commit |
| PHASE-B-ARCH-F6 | P3 | `architect`: the phase-lettering correction note this plan's §14 was supposed to receive (confirming the human directive's own "Phase A"/"Phase B" labels are a different scheme from this document's internal A–I lettering) was never added, leaving the collision the original architect ruling flagged still live | **FIXED.** Added to `docs/plans/stage-4i-jurisdiction-implementation-plan.md` immediately after the §14 phase list |
| PHASE-B-ARCH-F2 | P3 | `architect`: `SetDeclaredResidenceParams` already models the deferred staff-correction path (an `ActorStaff` branch with a required, currently-unvalidated free-text `ReasonCode`), pre-empting a decision the ruling explicitly deferred to a future `security`-owned design (permission, reason-code vocabulary, four-eyes threshold) | **Not fixed — accepted, named.** No caller exercises the staff branch today (dead-but-tested capability). Recorded as an explicit note for `PHASE-B-ARCH-1`: the future `security`/`architect` design for that endpoint is free to reject this shape (e.g. requiring a closed reason-code vocabulary instead of free text) |
| PHASE-B-ARCH-F7 / PHASE-B-QA-6 | P4 | `architect` and `qa`, independently: `PermPlayerResidenceRead` is defined and role-scoped (`RoleCompliance`) but wired to zero HTTP handlers in this phase | **Not fixed — deliberate, documented.** The permission exists ahead of the staff-facing read surface it will eventually gate, matching the same "built standalone, no callers yet" pattern already used for the two read accessors it would protect. Recorded so a future reader does not mistake the grant for a live capability |
| PHASE-B-ARCH-F8 | P4 | `architect`: `internal/validation/country.go` (a 249-entry ISO-3166-1 alpha-2 allowlist) was not part of any architect ruling, though it is genuinely load-bearing (the DB CHECK constraints only enforce the two-letter shape, not that the code is real) | **Accepted, registered.** Written directly by the orchestrator (not a subagent) after three reproducible content-filter failures generating the same content via `backend` dispatch. No country names, codes only. Needs a maintenance policy the next time ISO 3166-1 changes — tracked here as a standing note, not an open defect |
| PHASE-B-ARCH-F10 | P4 | `architect`: `GET /v1/me/residence` is not gated by the evidence-collection-active switch, unlike the `PUT` | **Accepted, defensible.** Turning collection off is not the same as revoking read access to an already-collected value; the interaction with HDR-J-3f's future erasure/retention mechanism (not yet built) is the more relevant future gate, noted for that phase |
| PHASE-B-ARCH-F11 | P4 | `architect`: `HasVerifiedResidence` can read `true` on a verification that later reaches `rejected` (a determination made during an earlier `review_required` review is not cleared by a subsequent rejection) | **Accepted, inert today.** `GetVerifiedResidence`'s own `status = 'approved'` filter makes this unreachable from the one accessor that matters; noted as a caution for any future back-office reader that might otherwise treat `HasVerifiedResidence` alone as meaning "currently valid" |
| PHASE-B-QA-4 | P2 | `qa`: no test exercises a forged cross-tenant *write* (tenant B's connection attempting to write against tenant A's player_account_id/verification_id) on either new write path | **Not fixed — deferred, pattern-level pre-existing gap.** The same gap already exists for the base `PlayerAccount`/`Verification` write paths platform-wide (only the read side has cross-tenant tests); not introduced or worsened by Phase B. Recorded for whichever future stage closes this class of gap package-wide |
| PHASE-B-QA-5 | P2 | `qa`: no concurrency test on either write path. The KYC-side race (two concurrent reviews of the same non-terminal verification, no `FOR UPDATE`) is real and pre-existing; the declared-residence path's single-CTE-UPDATE atomicity claim is plausible but untested | **Not fixed — deferred**, same disposition as PHASE-B-SEC-6/7 above (this is the same underlying gap, found independently by both reviewers) |
| PHASE-B-QA-8 | P3 | `qa`: migration 0074's round-trip is exercised only as part of the existing 7-migration full-chain test, never standalone | **Not fixed — accepted.** Consistent with the existing convention for migrations 0068–0073 in the same test file; not a new regression |

**Verdicts, all independent, none self-certified:** `security` — CERTIFIED WITH NAMED EXCEPTIONS (one P1, fixed in this phase's own fix round; the enforcement-asymmetry exception now gates `PHASE-B-ARCH-1` explicitly, not this phase). `architect` — CERTIFIED WITH NAMED EXCEPTIONS (no blocking issues; all nine design rulings conformed, `resolver.go` and the `Basis` enum verified at literal zero diff). `qa` — READY WITH NAMED GAPS, all three P1s closed in the fix round.

**Carried to a future phase (not this phase's to build, confirmed absent):** BYOL onboarding; `jurisdiction_precedence_configs` content/write surface (HDR-J-2, table remains shape-only); staff correction of declared residence (`PHASE-B-ARCH-1`'s own follow-on — see below; the activation-gate prerequisite that name originally referred to is now CLOSED); revocation/clear of either residence fact; `verified_residence_source_document_id`/corroboration policy (HDR-J-3h); nationality; permitted-market content (HDR-J-6); the retention/erasure job (HDR-J-3f); a real geolocation vendor and its own security review; resolver wiring of the two read accessors; a staff-facing read surface consuming `player_residence:read`; `GetVerifiedResidence`'s "most-recent-approved-wins" selection rule (may need revisiting once a player can have multiple approved verifications carrying determinations).

## Stage 4I "PHASE-B-ARCH-1" — activation-gate enforcement asymmetry hardening gate

A narrowly-scoped, human-dispatched hardening gate closing PHASE-B-SEC-4/
PHASE-B-ARCH-F1 (above) before any Phase C-dependent work. Dispatch:
`architect` design ruling (independently re-verifying every claim in the
prior Phase B reviews rather than trusting them) → orchestrator
implementation per that ruling → independent `security`/`qa` review.

**The fix.** `internal/identity.SetPlayerAccountDeclaredResidence` now
checks `jurisdiction.IsEvidenceCollectionActive(ctx, tx, p.TenantID,
jurisdiction.EvidenceDeclaredResidence)` itself, inside the same
transaction as the write, before the write — mirroring
`kyc.ReviewVerification`'s own gate exactly. A new sentinel
`identity.ErrEvidenceCollectionInactive` is returned when closed (nothing
is written, no audit row exists). The gate applies to EVERY actor type,
including the currently-unused `ActorStaff` branch — the architect
ruling's own words: "the gate answers a lawful-basis question about the
DATA, not about who is writing it," pre-answering the question for the
still-deferred staff-correction endpoint rather than leaving it open. A
new connection-scope assertion runs first: the transaction must be
tenant-scoped to `p.TenantID` specifically and NOT player-scoped,
returning a distinct non-sentinel error otherwise — required because
`jurisdiction_evidence_collection_active`'s own RLS policies exclude a
player-scoped connection (unlike `player_accounts`' own policy), so
without this assertion a player-scoped caller would see a misleading
"collection is off" result when collection is in fact on. The HTTP
handler's own duplicate check (`internal/httpserver/player_residence_
handlers.go`) was REMOVED, not kept as defence-in-depth — the architect
ruling's reasoning: keeping the same predicate checked twice in the same
transaction by the same package graph is exactly how the two sibling
paths drifted apart in the first place; `kyc_admin_handlers.go`'s own
precedent (checks nothing, only maps the sentinel to 403) is what the
residence handler now matches.

**The trigger alternative — considered and REJECTED**, correcting this
registry's own prior "preferred remedy" framing. A `BEFORE INSERT OR
UPDATE` trigger on `player_accounts` was the remedy both the original
Phase B security and architect reviews floated as strongest. The
PHASE-B-ARCH-1 architect ruling rejects it: the trigger body would need
to read `jurisdiction_evidence_collection_active`, whose RLS excludes
player-scoped connections, and the application role is
`FORCE ROW LEVEL SECURITY`/`NOBYPASSRLS`-bound — so the trigger would
either misfire on every legitimate player-scoped write (the same
misleading-403 hazard the in-code fix's own scope assertion exists to
prevent, but baked into the schema where no caller can diagnose it) or
require a `SECURITY DEFINER` function, i.e. a deliberate RLS-bypass
surface introduced specifically to enforce a privacy gate — a net
security regression, not a hardening. It would also put a cross-domain
read on the hot path of every `player_accounts` update (status changes,
password resets, email verification), and — since this pass explicitly
excludes touching `kyc_verifications` — would leave the two sibling
tables enforced by different mechanisms in the opposite direction from
today, the same class of asymmetry being closed. Revisit condition,
recorded for the future: if a second writer of `declared_residence_
country` ever appears outside `internal/identity` (e.g. a bulk/import
path), this rejection must be reopened.

**Dependency-direction rule recorded (INV-J-B3):** `internal/identity`
and `internal/kyc` may import `internal/jurisdiction`; `internal/
jurisdiction` must never import either, now or later. Verified via
`go list -deps ./internal/jurisdiction`: its only non-test dependency is
`internal/audit`. No import cycle was introduced.

**Deferred, not fixed in this pass — directive Task 4's `effective_from`/
actor-provenance disposition**, ruled on by the architect and copied here
verbatim per the directive's own requirement:

> **Issue:** `SetEvidenceCollectionActive` (`internal/jurisdiction/
> evidence_collection_active.go`) and `SetResolutionActive`
> (`internal/jurisdiction/resolution_active.go`) both upsert with
> `ON CONFLICT (...) DO UPDATE SET active = EXCLUDED.active`.
> `effective_from`, `created_by_actor_type`, and `created_by_actor_id`
> are therefore written only by the first INSERT and never updated.
> After a second toggle — in particular a toggle performed by a
> different actor — the row reports the timestamp and the actor of the
> FIRST activation, not of the change actually in force.
>
> **Owner:** `architect` (data model), with `security` consulted on the
> audit/provenance consequences before implementation.
>
> **Affected tables (both, identically):**
> `jurisdiction_evidence_collection_active` (migration `0074`, Stage 4I
> Phase B — gates collection of privacy-sensitive personal data under a
> lawful-basis judgment) and `jurisdiction_resolution_active` (migration
> `0071`, pre-existing, a purely engineering-fact table). This defect is
> **not new to Phase B**; Phase B reproduced the existing table's shape
> faithfully, including this flaw.
>
> **Affected behaviour:** historical provenance only — *when* a given
> tenant's activation last changed, and *which actor* last changed it, as
> read from these two tables. The `active` flag itself, the column every
> enforcement path actually reads, is correct at all times. `updated_at`
> IS maintained correctly by each table's BEFORE UPDATE trigger, so "when
> did this row last change" remains answerable from the table; only
> "effective from when, by whom" is stale. Every toggle additionally
> writes a complete, append-only `audit_log` entry with `before_active`,
> `after_active`, the acting principal, the reason code, IP/user-agent
> and request id, so the full and authoritative change history —
> including who and when — is already recoverable today from the audit
> log; the defect is a redundancy/convenience gap in the projection, not
> a loss of record.
>
> **Reason for deferral:** (1) it is a pre-existing defect shared with an
> older table, so fixing only the newer one would introduce a fresh
> inconsistency between two deliberately symmetric tables; (2) the right
> fix is a modelling decision, not a one-line change — naively
> re-stamping `created_by_actor_*` on update would make columns named
> `created_by_*` mean "last changed by", and re-stamping `effective_from`
> on every toggle conflates creation with amendment, so the likely
> correct answer is an append-only history of activation periods with the
> current row as a projection; (3) that change touches both tables, needs
> a migration and a backfill decision for existing rows, and is out of
> scope for a hardening gate whose sole purpose is closing an
> enforcement-placement asymmetry.
>
> **Must be resolved by:** the next Stage 4I phase that adds any write
> surface, reporting surface, or compliance export that presents
> activation *history* (rather than current state) — in particular any
> phase that (a) exposes activation state or history in the back
> office/partner console, (b) wires either activation fact into a
> regulatory report or evidence pack, or (c) adds a second writer/toggler
> of either table. It must be resolved for BOTH tables in one change,
> with an ADR recording the chosen shape (re-stamp vs. append-only
> history) and a migration covering existing rows. Until then, the
> `audit_log` is the authoritative source for activation change history,
> and no document, report, or console screen may present these three
> columns as the record of the change in force.
>
> **Why deferring weakens no CURRENT enforcement:** the activation gate
> is a binary on/off control, and its on/off state is correct and
> fail-closed at all times — absence of a row means OFF,
> `IsEvidenceCollectionActive`/`IsActive` read the `active` column only,
> and neither `effective_from` nor `created_by_actor_id` is read by any
> enforcement, authorization, RLS or privacy path anywhere in the
> codebase. No security or privacy decision, and no data-collection
> permission, is derived from the stale columns. The imprecision is
> confined to the historical provenance of when the current setting took
> effect and who set it — and even that is fully recorded, unaffected, in
> the append-only audit log. No player data is collected, exposed, or
> retained differently because of this defect.

**Independent review findings and dispositions** (`security` CERTIFIED
WITH NAMED EXCEPTIONS — the P2 is genuinely closed, no bypass found;
`qa` READY WITH NAMED GAPS — no P0/P1/P2, one pre-existing P3 confirmed
unchanged in class/severity):

| ID | Severity | Finding | Disposition |
|---|---|---|---|
| PHASE-B-ARCH-1-SEC-1 | P3 | `security`: ISO-3166 validation of `p.CountryCode` inside `SetPlayerAccountDeclaredResidence` was caller-only (a doc comment, not enforced) — the identical doc-comment-as-control pattern this pass just removed for the gate, on a different field. An unassigned code (e.g. `"ZZ"`) would satisfy the DB's shape-only `^[A-Z]{2}$` CHECK and be stored, later feeding jurisdiction resolution/reporting as if it were a real country | **FIXED.** `validation.IsISO3166Alpha2` is now checked in-function (mirroring `kyc.ReviewVerification`'s own in-function validation), returning `ErrInvalidInput` before any write. `TestSetPlayerAccountDeclaredResidence_InvalidCountryCodeIsError` added |
| PHASE-B-ARCH-1-SEC-2 | P3 | `security`: TOCTOU — `IsEvidenceCollectionActive` is an unlocked `SELECT`; a write in flight when the gate is toggled OFF can still commit, so `audit_log` can show a residence-set entry timestamped after the toggle-off entry | **NOT FIXED — accepted, documented.** Identical, pre-existing shape on `kyc.ReviewVerification`; not introduced or widened by this pass. Data stays internally consistent (write+audit in one transaction); only the audit trail's apparent ordering vs. the toggle is affected. A fix (e.g. `SELECT ... FOR SHARE` on the gate row) is a cross-path design change for `architect`, out of this hardening gate's scope. Documented in `docs/security/security-architecture.md` §J4I.12.2 and `docs/architecture/16-privacy.md` |
| PHASE-B-ARCH-1-SEC-3 | P4 | `security`: `docs/architecture/16-privacy.md`'s working-tree diff overclaimed "no caller — HTTP or otherwise — can reach either write without passing this check," true only of Go callers of the two domain functions, not of arbitrary SQL | **FIXED (doc-only).** Scoped to "no Go caller of either domain function"; the trigger-rejection rationale is referenced for why a DB-level backstop was considered and rejected |
| PHASE-B-ARCH-1-SEC-4 | P4 | `security`: the trigger-rejection rationale's "would misfire on player-scoped writes" argument is undercut by this very pass — the new connection-scope assertion guarantees residence writes are never player-scoped, so a `WHEN`-conditioned trigger would not in fact misfire that way | **Corrected in place, trigger decision NOT reopened.** `docs/security/security-architecture.md` §J4I.12.2 now records the corrected reasoning explicitly, per governance's "no specialist unilaterally overturns another's ruling" rule — the architect's rejection stands, but the "revisit if a second writer appears" condition it already recorded must be judged against the corrected argument, not the original, if it is ever revisited |
| PHASE-B-ARCH-1-SEC-5 | P4 | `security`: the mis-scoped-transaction error was a bare, unclassifiable `fmt.Errorf` — no caller or alerting rule could distinguish "a developer mis-scoped the transaction" from a transient DB failure | **FIXED.** Added `identity.ErrTransactionScope`, a dedicated sentinel (deliberately not wrapping `ErrInvalidInput`, since this is a server-side caller bug, not a client input error) |
| PHASE-B-ARCH-1-QA-1 | (test-coverage gap, not a defect) | `security`'s own adversarial probing found the canonical "tenant B's valid, self-consistent token targeting tenant A's data" case for the WRITE path was untested — the committed `WrongTenantIDErrors` test only covered a mismatched-params case caught by the scope assertion before ever reaching `player_accounts` | **FIXED.** `TestSetPlayerAccountDeclaredResidence_CrossTenantWriteTargetDenied` added: tenant B, self-consistent scope, targeting tenant A's `player_account_id` → `ErrNotFound`, tenant A's value untouched, zero audit rows under tenant B |

**Scope discipline confirmed:** `internal/kyc`'s own gate has zero diff
in this pass. No new HTTP endpoint (the deferred staff-correction
endpoint remains deferred — this ruling only removes the prerequisite
blocking it, it does not authorize building it). No OpenAPI change. No
migration. No change to `internal/jurisdiction/resolver.go`, the `Basis`
enum, or `jurisdiction_precedence_configs`.

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
