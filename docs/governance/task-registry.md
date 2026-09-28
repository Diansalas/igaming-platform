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
| 4HB1-02 | bonus-engine | Done | none | `docs/architecture/10-bonus-engine-architecture.md` | Wave 1: domain model + full bonus catalogue + campaign/offer/grant/segmentation/coded-bonus/bulk/suggestion design | none |
| 4HB1-03 | ledger-finance | Done | none | `docs/architecture/ledger-accounting-model.md` | Wave 1: financial/ledger integration contract (bonus_expense, Rule B2 generator, conversion flow) | none |
| 4HB1-04 | risk | Done | none | ADR 0031 addendum | Wave 1: Risk integration contract | none |
| 4HB1-05 | identity-compliance | Done | none | ADR 0034 addendum | Wave 1: RG + identity/multi-account contract | none |
| 4HB1-06 | sportsbook | Done | none | doc 09 addendum (review only) | Wave 1: provider-native bonus coexistence contract | none |
| 4HB1-07 | casino | Done | none | doc 08 addendum (review only) | Wave 1: casino event-consumption contract | none |
| 4HB1-08 | security | Done | none | `docs/security/security-architecture.md` addendum | Wave 1: RBAC/audit/tenancy/RLS contract | none |
| 4HB1-09 | architect | Done | none | new cross-domain implementation-contract doc | Wave 1: master architecture→ADR→object→service→API→event→ledger→audit→test mapping | none |
| 4HB1-10 | qa | Done | none | `docs/testing/testing-strategy.md` addendum | Wave 1: full test-matrix design | none |

*Status note 2026-09-26: rows 4HB1-02..10 corrected from "In progress" to "Done" per the Wave 1 status line directly below.*

**Wave 1 status: COMPLETE, all 9 dispatches reported back, reviewed, and
committed** (`41c029f`, `0171e02`, `39be4af`, `b0442b1`, `5b7e7ed`,
`89a09ab`, `29490dc`). Reconciliation performed by the Orchestrator below.

**Migration-number ledger** (claimed during Wave 1, none written yet —
Wave 1 was design-only):
- `0050` — `bonus_expense` account-type CHECK widening (`ledger-finance`, §7.2)
- `0051` — `bonus_grant`/`bonus_conversion`/`bonus_forfeiture`/`bonus_reversal` transaction types + `reason_code` constraint widening (`ledger-finance`, §7.3)
- `0052` — `rounding_rules` table + `ledger_transactions.rounding_rule_id` (`ledger-finance`, §7.8 — requested, now assigned)
- *(Stage 10 W0 correction: this trigger was ultimately built as `ledger_accounts_immutable_fields` in migration `0082_stage9_db_hardening`; migration `0053` became `economic_operations`.)* `0053` — `ledger_accounts` identity-immutability trigger, HR-15/HR-16 (`ledger-finance`, §7.14 — requested, now assigned; **hard gate: must land before the first `player_bonus` posting**, not merely before the first locked-account posting)
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
| 4HB1W15-01 | casino | Done | Casino postWin financial source/destination resolution design, G-2 boundary specification (not selecting G-2), adversarial scenarios (§A) |
| 4HB1W15-02 | bonus-engine | Done | Grant terminal-state invariant + proof (§A.9), bonus targeting/bulk-assignment validation (§C), Bonus Suggestion full spec (§D), bonus catalogue validation (§H) |
| 4HB1W15-03 | architect | Done | Segmentation Engine architecture doc (§B), CRM Engine architecture doc (§E), Affiliate Engine architecture doc (§F), canonical cross-domain relationship diagrams (§G), updated ownership map + dependency graph |
| 4HB1W15-04 | sportsbook | Done | Provider-native bonus coexistence re-confirmation against the new CRM/Affiliate/Segmentation additions (§I) |
| 4HB1W15-05 | qa | Done | Cross-domain test matrix covering all new domains + §A.10's adversarial scenarios |

*Status note 2026-09-26: rows 4HB1W15-01..05 corrected from "In progress" to "Done" per "Phase 1 status: COMPLETE, all 5 dispatches committed" below.*

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

## Stage 4H-B1, Wave 3 — Bonus Engine Completion, Integration Hardening & Final Financial Gate (Phase 10 of 11 — `architect`, COMPLETE)

*Status note 2026-09-26: header corrected from "IN PROGRESS". Phase 10 is Done (W3-P10 row and verdict below); Phase 11 is recorded COMPLETE in the next section; `docs/governance/wave-3-report.md` records Wave 3 as READY. The "Phase 11 … has not run" sentence below is historical.*

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
| DR-PRHI3-01 | PRH-I3 | `identity-compliance` (ADR 0096) | `casino` | One new `kyc.EvaluateEnforcement` call added to `internal/casino/orchestrator.go`'s `postBet`, appended after the existing RG→Risk sequence, immediately before `ledger.GetOrCreateAccounts` (ADR 0096 §2.4/§3.5). Requesting `casino`'s review of the exact insertion point and the `OutcomeDeclined`/`DeclineReason` reuse convention (no new provider-visible outcome variant), per ADR 0096 §11's own casino-review conditions | PRH-I3 (2026-09-27) | Open |
| DR-PRHI3-02 | PRH-I3 | `identity-compliance` (ADR 0096) | `sportsbook` | One new `kyc.EvaluateEnforcement` call added to `internal/sportsbook/orchestrator.go`'s bet-placement path, appended after the existing RG→Risk sequence, before the exposure-limit gate and `ledger.GetOrCreateAccounts`; a new `RejectionKYCDenied` category added to `PlaceBetResult.RejectionCategory`'s closed set. Requesting `sportsbook`'s review of the insertion point relative to the exposure gate and the new rejection category | PRH-I3 (2026-09-27) | Open |
| DR-PRHI3-03 | PRH-I3 | `identity-compliance` (ADR 0096) | `withdrawal` (no distinct package owner exists today per ADR 0096 §8 item 3) | `internal/withdrawal/withdrawal.go`'s `RequestWithdrawal` restructured (pre-insert idempotency lookup → KYC gate → insert), and a new `DenyForCompliance` function added (approved→rejected, mirrors `Reject`/`Fail`'s posting shape, exactly-once via L1 + conditional UPDATE). Requesting `ledger-finance` review against ADR 0096 §12.2 C1/C2/C7 in full, and `security` review of the admin route/RLS/raw-guard gap, before PRH-I3 is marked complete without qualification | PRH-I3 (2026-09-27) | Open |
| DR-PRHI3-04 | PRH-I3 | `identity-compliance` (ADR 0096 §15.4) | `payments`/PRH-I1 | `payments.InitiateDeposit` must call `kyc.EvaluateEnforcement`/`kyc.RecordDecision` (`EnforcementDeposit`) immediately after the existing RG check, per ADR 0096 §2.4/§8 item 2 - the deposit gate is designed and exported but deliberately NOT wired by PRH-I3, since PRH-I1 rewrites `InitiateDeposit`. Also carries the deposit call site's own `Amount <= 0` rejection requirement (recorded per orchestrator instruction) | PRH-I3 (2026-09-27) | Resolved 2026-09-28 (wired by PRH-I1 at `deposit_v2.go:202`, `drive.go:100`); **except** the `kyc.RecordDecision` requirement, which is unmet — tracked as KYC-ENF-DECISION-ROWS-1 (was: Open) |
| DR-PRHI3-05 | PRH-I3 | `identity-compliance` (ADR 0096 §5/§15.4) | `payments`/`architect`, PRH-I1/ADR 0095 | ADR 0095's Phase A/`ClaimForDispatch` (T1p) must call `kyc.EvaluateEnforcement`/`withdrawal.DenyForCompliance` (`EnforcementWithdrawalPayout`) inside the same L1-locked claim transaction, before T1p's own CAS commits, per ADR 0096 §5's coordination note and ledger-finance C5; any sweeper re-claim (T2/T12) must re-run the gate before its own first send, routing a deny to ADR 0095's M3 (provably-never-sent) path, never `DenyForCompliance` directly, once a request may have reached the provider. The security-condition-6 raw-guard test (currently deferred, `withdrawal.DenyForCompliance`'s own doc comment) should be retargeted from `MarkSubmitted` to `ClaimForDispatch`/T2/T12 once these exist | PRH-I3 (2026-09-27) | Resolved 2026-09-28 (wired by PRH-I1: T1p `payout.go:291-311` via `DenyForCompliance`; T2/T12 re-claim `payout_sweep.go:99-130`); T1p-allow and T2/T12-deny decision rows missing — KYC-ENF-DECISION-ROWS-1 (was: Open) |
| DR-PRHI3-06 | PRH-I3 | `ledger-finance` (rv-prh-i3-ledger.md LF-I3-4) | `ledger-finance` (self, future migration) | A partial unique index `(tenant_id, reverses_transaction_id) WHERE transaction_type IN ('withdrawal_rejected','withdrawal_failed')` as a DB backstop for exactly-once hold release (defense in depth alongside the L1 lock + conditional UPDATE already in place) could not land in migration 0100 (already applied) and migrations 0101-0103 are allocated to other in-flight PRH work. Deferred to the next `ledger-finance`-owned migration slot | PRH-I3 (2026-09-27) | Open |
| DR-PRHI3-07 | PRH-I3 | `identity-compliance` (ADR 0096 §17.3, security F3) | orchestrator (migration number allocation), then `identity-compliance` | **RESOLVED (fix round 4, 2026-09-27)**: orchestrator allocated migration 0103; `identity-compliance` implemented the proposed design exactly (nullable `superseded_by_policy_id` column + `CREATE OR REPLACE FUNCTION` extending 0100's own lifecycle trigger, plus a new deferred constraint trigger validating the successor is active/key-matching/non-self by commit time - 0100's up.sql itself untouched). `kyc.WithdrawEnforcementPolicy` on an ACTIVE row is now refused by the database itself, not by convention. See ADR 0096 §18 for the full record, tests, and mutation-kill evidence (`docs/plans/payment-readiness/evidence/prh-i3-migration-0103-mutation-kill.txt`) | PRH-I3 fix round 4 (2026-09-27) | Resolved |
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

## Stage 4I Phase C — jurisdiction precedence and resolution rules foundation

Implementation dispatch: mandatory pre-implementation impact-map analysis
→ `architect` design ruling (full type/function signature specification) →
`backend` implementation exactly per that ruling → four independent
parallel reviews (`architect` fidelity, `security`, `identity-compliance`
for compliance/privacy, `qa`), none seeing the others' findings →
orchestrator triage and fix round → full validation re-run → orchestrator
integration. Built the deterministic precedence/resolution rule engine per
HDR-J-1 through HDR-J-6 (`docs/decisions/0042-human-decision-response.md`):
`internal/jurisdiction.DeterminePlayerJurisdiction` (`precedence.go`), the
`Purpose` taxonomy (`purpose.go`), the `EvidenceSet`/`LocationSignalEvidence`
evidence model (`evidence.go`), the non-forgeable `PlayerJurisdictionResult`/
`Candidate`/`ConsideredEvidence` result types (`player_result.go`), and the
`ComposeRestrictions` most-restrictive-outcome composition primitive
(`restriction.go`). See `docs/governance/stage-4i-canonical-model.md` §14
for the full canonical-contract writeup (resolution outcomes, operation
taxonomy, evidence precedence, unresolved/fail-closed semantics,
more-restrictive semantics, event-time semantics, tenant/player
separation, and the four deferred PC-GAP items), and §7.3/§7.4 (amended
this phase) for the MROC severity vocabulary and honest "primitive exists,
nothing calls it yet" status.

**`internal/jurisdiction/resolver.go` — the only resolver any consuming
domain (`casino`, `bonus`, `risk`) actually calls — has ZERO diff
throughout this phase**, verified by `git diff --stat` after
implementation, after all four reviews, and after the fix round. There are
zero production call sites of `DeterminePlayerJurisdiction` or
`ComposeRestrictions`. All activation switches remain OFF; no production
permitted-market list, country allow/deny content, real geolocation
vendor, or production jurisdiction enforcement exists as a result of this
phase.

**Findings from the independent review round, and disposition.** Two
defects were independently found by three of the four reviewers using
different methods (direct code reading, adversarial mutation/compile
probes, adversarial format-verb probes) and are listed first as the
highest-confidence findings.

| ID | Severity | Finding | Disposition |
|---|---|---|---|
| PHASE-C-ARCH-P1-3 / SEC-4I-C-01 / PHASE-C-QA-1 | P1 | `architect`, `security`, and `qa`, independently converging (three different methods): `PlayerJurisdictionResult.Candidates()`, `.ConsideredEvidence()`, and `ComposedRestriction.Contributors()` all returned their internal backing slice directly rather than a copy. A caller could reorder a `Candidates()` result in place and have `PrimaryCandidate()` then report an additional-restriction candidate (e.g. a geo signal) as the primary determination — defeating HDR-J-3a's "location never substitutes for verified residence" guarantee. `ConsideredEvidence`'s exported fields made this the more dangerous variant: a caller could relabel an entry's `Basis` to `BasisTenantLicence` in place | **FIXED.** All three accessors now return `slices.Clone` of the internal slice. New regression tests: `TestPlayerJurisdictionResult_CandidatesReturnsADefensiveCopy`, `TestPlayerJurisdictionResult_ConsideredEvidenceReturnsADefensiveCopy`, `TestComposedRestriction_ContributorsReturnsADefensiveCopy` (`player_result_test.go`) |
| PHASE-C-ARCH-P1-1 | P1 | `architect`: `determineMarketAccess` treated any `LocationSignalRequirement` value other than the recognized zero-value `LocationRequirementUnset` as if it were `LocationAdvisory` (the more permissive of the two known values) — an unrecognized/misconfigured/stale policy value would fail OPEN rather than closed | **FIXED.** An exhaustive check now rejects any value other than `LocationRequired`/`LocationAdvisory` with `ErrInvalidInput`. New test: `TestDetermine_UnrecognizedLocationSignalRequirementIsRejected` |
| PHASE-C-ARCH-P1-2 | P1 | `architect`: `DeterminePlayerJurisdiction` never validated `AsOf`. A zero `AsOf` makes `asOf.Sub(observedAt)` hugely negative for any real `ObservedAt`, so every location signal would read as fresh, silently disabling the entire freshness gate | **FIXED.** A zero `AsOf` is now rejected with `ErrInvalidInput` before any evaluation. New test: `TestDetermine_ZeroAsOfIsRejected` |
| PHASE-C-ARCH-P2-1 | P2 | `architect`: the location `ConsideredEvidence` entry (including its `EvidenceRef`) was only appended on the `LocationAdvisory`-and-unusable path, not the `LocationRequired`-and-unusable path — silently dropping the one evidence reference an incident investigator needs, on exactly the path that denies a player | **FIXED.** The entry-construction block was moved before the `LocationRequired`/`LocationAdvisory` branch, so it is now appended on both. New test: `TestDetermine_LocationRequiredAndUnusableStillRecordsConsideredEvidence` |
| PHASE-C-ARCH-P2-2 / SEC-4I-C-07 | P2 | `architect` and `security`, converging: `determineMarketAccess` already upgraded `ReasonNoSignal` to `ReasonNoApplicableEvidence` when a location signal was the only evidence supplied (location is never a residence signal), but `determineIdentity` did not perform the symmetric upgrade for the identical case | **FIXED.** The same upgrade logic was added to `determineIdentity`. The existing test asserting the old `ReasonNoSignal` behavior (`TestDetermine_LocationSignalIsNeverASubstituteForVerifiedResidence`) was updated to assert `ReasonNoApplicableEvidence`, per architect's explicit instruction; a new test, `TestDetermine_MarketAccessEmitsReasonNoApplicableEvidenceForLocationOnlyEvidence`, closes the market-access side's own zero positive-coverage gap `qa` separately found |
| SEC-4I-C-02 | P3 | `security` (independently also found by `architect` and `qa`): `fmt`'s `%#v` verb bypasses `Stringer` entirely and dumps unexported field values in full, defeating every `String()` method's redaction on `PlayerJurisdictionCode`, `Candidate`, `PlayerJurisdictionResult`, `ComposedRestriction`, and `AppliedRestriction` | **FIXED.** `GoString() string` (`fmt.GoStringer`) added to all five types, each returning the same redacted shape as its own `String()`. New test: `TestGoStringRedaction_NeverLeaksACountryCodeViaSharpV` plus `TestPlayerJurisdictionCode_GoStringNeverLeaksTheCode` (`player_result_test.go`) |
| SEC-4I-C-03 | P3 | `security`: a future-dated `LocationSignalEvidence.ObservedAt` (age < 0) computed as "always fresh," since the freshness check only compared against the upper bound — defeating the entire purpose of a freshness bound on a signal that should reflect the present | **FIXED.** A negative age is now explicitly treated as stale, identically to an age past the freshness boundary. New test: `TestDetermine_FutureDatedLocationSignalIsTreatedAsStale` |
| SEC-4I-C-04 | P4 | `security`: an unrecognized caller-supplied `LocationSignalState` was echoed verbatim into the recorded diagnostic (`result.locationState`) — the decision already failed closed regardless, but a future persistence/reporting phase would see values outside the documented closed set | **FIXED.** An unrecognized state now normalizes to `LocationUnavailable` in the recorded diagnostic. New test: `TestDetermine_UnrecognizedLocationSignalStateNormalizesToUnavailable` |
| SEC-4I-C-05 | P4 | `security`: a structurally invalid or empty declared-residence value, alongside a valid verified residence, was compared directly against the verified code and could be recorded as `StatusDisagreed` — indistinguishable from a genuine contradiction (e.g. MT vs. DE), risking a false-positive fraud/review signal for every player whose declared-residence row is blank or malformed | **FIXED.** A 3-way switch now records `StatusInvalid` (never `StatusDisagreed`, never sets `HasDisagreement()`) when the declared value is structurally invalid. New test: `TestDetermine_InvalidDeclaredResidenceNeverTriggersDisagreementWithVerified` |
| SEC-4I-C-06 | P4 (non-blocking, doc-only) | `security`: a pointer to a ZERO (non-nil) `Duration` for `MaxLocationSignalAge` is a distinct footgun from `nil` — it means "fresh only at age == 0" because the freshness boundary is inclusive, almost certainly not what a caller building this from config actually wants. No code-level fix exists (zero is a structurally valid, distinct value from nil) | **FIXED (doc-only).** A doc-comment note added to `EvaluationPolicy.MaxLocationSignalAge` explaining the footgun explicitly, cross-referenced from PC-GAP-2 in the canonical-model doc |
| PHASE-C-ARCH-P3-1 | P3 | `architect`: `ComposeRestrictions`' winning-outcome assignment used a last-wins loop assignment, silently contradicting the function's own doc comment ("no tie-break policy needs to exist") — harmless only by coincidence because today's three outcomes are each other's unique severity; a fourth outcome sharing a severity would reintroduce an undetected arbitrary pick | **FIXED.** Derived from an explicit `severityToOutcome` map keyed by the winning severity instead |
| PHASE-C-ARCH-P3-2 | P3 | `architect`: `PlayerJurisdictionCode`'s doc comment claimed a non-empty code could not be constructed externally, but the empty composite literal `PlayerJurisdictionCode{}` does compile from outside the package (Go permits a field-less struct literal even with unexported fields) | **FIXED (doc + code).** Doc comment corrected; a new `IsSet() bool` method added so callers can distinguish a real code from the zero value rather than assuming external construction is impossible |
| PHASE-C-QA-2 | (test-coverage gap, not a defect) | `qa`: `StatusUnavailable`/`StatusInvalid` on the advisory-and-unusable location path were only exercised for panic-safety, not for the actual recorded status/EvidenceRef content, once PHASE-C-ARCH-P2-1 changed which paths append this entry | **FIXED.** `TestDetermine_LocationRequiredAndUnusableStillRecordsConsideredEvidence` asserts the exact `Status`/`Ref` content on the required-and-unusable path (the more critical of the two, since it is the one that denies a player); the pre-existing table test already covered the advisory path's five unusable states |
| PHASE-C-IC-1 | (informational, no violation) | `identity-compliance` (compliance/privacy review): found **no compliance or privacy violations** — no nationality concept introduced, no evidence value persisted where only a reference belongs, correct separation of player/tenant jurisdiction maintained. Independently surfaced the same `%#v` leak `security` and `architect` found, via its own scratch adversarial test, and deferred it to `security`'s track | Already fixed under SEC-4I-C-02 above; no separate compliance action required |

**Deferred legal/policy decisions — the PC-GAP items** (full detail,
including owner/dependency/future-phase/security-impact for each, in
`docs/governance/stage-4i-canonical-model.md` §14.6): **PC-GAP-1**
(whether/when a market-access operation may proceed with an unusable
location signal — `LocationRequirement`'s zero value fails closed with
`ErrPolicyUnset`); **PC-GAP-2** (the maximum age before a location signal
is stale — `MaxLocationSignalAge` nil fails closed with `ErrPolicyUnset`);
**PC-GAP-3** (the `OperationClass`→`Purpose` mapping — no code touches
this; a legal-content decision, not an engineering one); **PC-GAP-4**
(tenant/jurisdiction-aware precedence keying per canonical-model §3.4 —
`architect`-flagged documentation-only debt; no code exists to key against
yet, since HDR-J-2's precedence-configuration content is still open).

**Verdicts, all independent, none self-certified:** `architect` —
CERTIFIED WITH NAMED EXCEPTIONS (no blocking issues after the fix round;
`resolver.go` verified at literal zero diff throughout). `security` —
CERTIFIED WITH NAMED EXCEPTIONS (the one P1 — SEC-4I-C-01 — is the same
defect architect and qa also found, closed with the shared fix; no
unresolved P0/P1). `identity-compliance` — NO VIOLATIONS FOUND. `qa` —
READY WITH NAMED GAPS, all closed in the fix round.

**Carried to a future phase (not this phase's to build, confirmed
absent):** the four PC-GAP items above; wiring `DeterminePlayerJurisdiction`
into any production evaluation path (`casino`, `bonus-engine`, `risk`,
`payments`, `sportsbook`); wiring `ComposeRestrictions` into any domain
that produces a genuine second candidate; a `jurisdiction_precedence_configs`
write/read surface with real content (HDR-J-2); real player-side evidence
collection feeding this engine in production (HDR-J-3); the
staff-correction endpoint (deliberately out of scope per the directive,
confirmed no minimal seam was found strictly required); G-2; sportsbook
cashout; converted-Grant clawback; BYOL; any payment/casino/risk behaviour
change beyond the resolver seams already named as buildable now.

## Stage 4I Phase D — jurisdiction policy configuration and operational semantics

Implementation dispatch: mandatory pre-implementation impact-map analysis
(confirming, contrary to a tempting but invalid inference from consumer
code, that the `OperationClass`→`Purpose` mapping cannot be deterministically
derived) → `architect` design ruling (binding, exhaustive, covering all four
PC-GAP items) → `backend` implementation exactly per that ruling → five
independent parallel reviews (`architect` fidelity, `security`
double-hatting as DB/RLS specialist, `identity-compliance` for
compliance/privacy, `qa`, `risk` for cross-domain integration) → orchestrator
fix round → a focused `security` re-verification of that fix round (which
found one fix itself still incomplete) → a second, targeted fix closing the
one remaining defect with a genuinely deterministic replacement, independently
re-run by the orchestrator → orchestrator integration.

Built the configuration infrastructure for three of the four Phase C
deferred gaps (`docs/governance/stage-4i-canonical-model.md` §14.6):
`internal/jurisdiction.RequiredPurposes` (the PC-GAP-3 mapping seam — zero
mapping content, all four `OperationClass` values fail closed with
`ErrPurposeMappingUndetermined`), `internal/jurisdiction.ResolveEvaluationPolicy`/
`CreateEvaluationPolicyVersion`/`ListEvaluationPolicyVersions` (the PC-GAP-1/
PC-GAP-2/PC-GAP-4 config read/write API, backed by migration `0075` widening
`jurisdiction_precedence_configs` in place — new `status`/`location_requirement`/
`max_location_signal_age_seconds`/`precedence_status`/`precedence_policy_version`/
`legal_review_reference`/`reason_code` columns, RLS added for the first time
on this table, three new triggers enforcing forge-proof `effective_from`/
`effective_to` stamping and append-only immutability), and one new permission
(`PermJurisdictionEvaluationPolicyWrite`, platform-admin-only, no HTTP route).
Three new human-decision items were opened and registered, never guessed at:
**HDR-J-7** (which `Purpose`(s) each `OperationClass` requires), **HDR-J-8**
(location-requirement threshold per licensing jurisdiction/operation class),
**HDR-J-9** (location-staleness bound per licensing jurisdiction/operation
class) — `docs/decisions/0044-human-decision-register-stage-4i-phase-d.md`.
Design record: `docs/decisions/0043-jurisdiction-evaluation-policy-configuration.md`.

`internal/jurisdiction/resolver.go`, `precedence.go`, and `types.go` have
**ZERO diff**; `purpose.go` carries a doc-comment-only diff. Zero production
callers of any new function exist. No HTTP route, no OpenAPI change, no
country/market content, no real geolocation vendor, no production
jurisdiction enforcement.

**A genuinely load-bearing architect correction, recorded so it is never
re-litigated:** the orchestrator's own pre-implementation reconnaissance
concluded all four `OperationClass` values deterministically require
`PurposeMarketAccessControl`, reasoning that every existing consumer
(casino blocklist, bonus issuance/conversion gates, `assetregistry` layer 6)
makes an availability/restriction decision, never a KYC/identity decision.
**`architect` explicitly rejected this inference as invalid** — `Purpose`
does not classify what kind of decision a consumer makes; it selects which
evidence hierarchy is legally authoritative, and a per-game blocklist
applied to *verified residence* is an equally coherent, and in several
regulated markets legally correct, design. Guessing the mapping in either
direction is dangerous asymmetrically (one direction silently deletes a
geolocation control a licence may require; the other invents a denial
condition no regulator asked for) — hence HDR-J-7, not a coded default.

**Findings from the independent review round, and disposition:**

| ID | Severity | Finding | Disposition |
|---|---|---|---|
| PHASE-D-ARCH/SEC/QA-CONCURRENCY | P1 (three-way independent convergence) | `architect`, `security`, and `qa`, each independently and empirically (not by code reading — the defect was invisible from reading): the integration test proving `CreateEvaluationPolicyVersion`'s concurrency control (`ErrConcurrentPolicyWrite`) launched two goroutines with no synchronization barrier, so depending on scheduling the two calls either genuinely raced (the intended, asserted outcome) or effectively serialized into a legitimate supersession (2 successes) — failing the test's assertions roughly 30-50% of the time under a plain repeat run | **FIXED, in two rounds.** Round 1 added a `sync.WaitGroup` barrier — closed the failure mode `qa`/`architect` reproduced directly, but a dedicated `security` re-verification pass then reproduced the SAME class of failure under CPU contention (9/200 runs), because the barrier synchronized only transaction start, not the write race itself. Round 2 replaced scheduling-dependent assertions entirely: a loosened invariant-only test (`TestCreateEvaluationPolicyVersion_ConcurrentCreatesNeverCorruptState`, asserting only what holds under every legitimate interleaving) plus a new, genuinely deterministic test (`TestCreateEvaluationPolicyVersion_DeterministicConflictViaUncommittedCompetingRow`) that forces the race via real PostgreSQL unique-index locking semantics — an uncommitted competing row blocks the real call's `INSERT`, confirmed via a `pg_stat_activity` poll (not a sleep, zero timing assumption) before the blocker is released. Independently re-run by the orchestrator: 30 consecutive passes (`-race`, two repeat batches) plus a clean whole-repo integration run |
| PHASE-D-ARCH-P2-1 | P2 | `architect`, empirically (applied the down migration against a live database): the down migration for `0075` silently `DROP COLUMN`s content an append-only table's own design says must never be destroyed, and re-applying `up.sql` after `down.sql` then fails once any row exists (the added `NOT NULL` columns have no DEFAULT) | **FIXED.** Guarded with a `RAISE EXCEPTION` refusal when the table holds any row, matching migrations 0048/0052's own precedent for a destructive rollback on a table that may hold real data. New test `TestMigration0075_DownMigrationCleanThenFailsOnDirtyDatabase` (clean round-trip; dirty refusal with row survival), matching `internal/ledger/migration_0048_integration_test.go`'s scratch-database convention |
| PHASE-D-SEC-P4-1 (folded into P2-1's fix) | P4 | `security`: the down migration's `DISABLE ROW LEVEL SECURITY` never issued the matching `NO FORCE`, leaving `relforcerowsecurity=true` after rollback instead of restoring the pre-0075 `false` | **FIXED** in the same edit. New test `TestMigration0075_DownMigrationRestoresPreMigrationRLSPosture` asserts both flags read `false` after a clean rollback |
| PHASE-D-SEC-P3-1 | P2 (real cross-tenant read gap) | `security`, code-reading + empirical: `ListEvaluationPolicyVersions` took a raw `pgx.Tx` and a caller-supplied jurisdiction id with **no scope assertion at all** — combined with the deliberately permissive `FOR SELECT USING (true)` RLS policy, any tenant- or player-scoped Go caller of this function could enumerate another licensing jurisdiction's full authoring history (`reason_code`, `legal_review_reference`, actor ids included), contradicting ADR 0043's own "no caller handles a jurisdiction id" principle. The integration test file's own header additionally overclaimed coverage of this function while never calling it | **FIXED.** `assertPlatformScope` added as the function's first statement. New tests: tenant-scoped and player-scoped calls → `ErrTransactionScope`; a genuine happy-path history test. Doc comment corrected (per `code-reviewer`'s follow-up finding, PHASE-D-CR-P3-4 below) to scope the guarantee to "Go callers of this function" — the permissive RLS `SELECT` policy itself is unchanged and still permits a raw-SQL reader with a tenant- or player-scoped connection to read this table directly; whether to narrow that policy is recorded as a `security`-owned open question, not decided by this fix |
| PHASE-D-ARCH/SEC-P3-2 | P3 (both independently found; one fix closes both) | `architect` and `security`, independently: `ResolveEvaluationPolicy`'s selection query had no `ORDER BY`/`LIMIT`, and — though unreachable via the sanctioned Go write path — two SEPARATE raw-platform-admin-SQL transactions (one holding a bare close with no successor, another inserting under an earlier-pinned transaction timestamp) could still construct an overlapping or gapped `[effective_from, effective_to)` window; `security` reproduced this live | **FIXED (mechanism-hardening), residual accepted and disclosed.** The append-only trigger now forces `NEW.effective_to := now()` on any UPDATE transitioning it from NULL to non-NULL (mirroring the existing `effective_from` forgery-proofing on INSERT), closing the caller-supplied-timestamp half of the attack; `ResolveEvaluationPolicy`'s query gained `ORDER BY effective_from DESC LIMIT 1` for deterministic resolution if the underlying near-simultaneous-raw-SQL scenario is ever hit. `security`'s own narrower residual (a true overlap constructed via two raw sessions with one holding a bare close open) is disclosed in the migration's own trigger comment as an accepted, named gap requiring a raw-SQL bypass of every sanctioned write path to reach, with a future range-exclusion-constraint hardening option named and deferred (adds a `btree_gist` extension dependency for a gap only reachable outside every sanctioned path) |
| PHASE-D-ARCH-P2-2/P2-3 | P2 (two overstated claims) | `architect`, empirically: ADR 0043 claimed "the only draft a caller could author today is content-free" (false — `CreateEvaluationPolicyVersion` accepts real content on a `draft`, only `withdrawn` is forced content-free, and a passing test proves it) and "`ResolveEvaluationPolicy` never returns a zero policy with a nil error" (false for the deliberate field-level-unset case, which is correct behaviour but wrongly described in absolute terms) | **FIXED.** Both claims corrected in ADR 0043, canonical-model §15, and the relevant Go doc comments, restating the TRUE invariant each was reaching for without overclaiming |
| PHASE-D-ARCH-P3-1 | P3 | `architect`: canonical-model §14.6's Phase-D status note for PC-GAP-4 said bare "CLOSED", overclaiming against its own "closes in" criterion (content still HDR-J-2-blocked, zero production callers) | **FIXED.** Restated as "lookup/config mechanism CLOSED; production wiring NOT IMPLEMENTED", matching the honest form the other three PC-GAP rows already used |
| PHASE-D-SEC-P3-3 | P3 | `security`: §15.4 self-declared a "CERTIFIED... full build/vet/gofmt/race/integration coverage" verdict written by the implementer BEFORE any independent review ran — inverting CLAUDE.md's "security review before marked complete" rule, and inaccurate at the time for the two defects above | **FIXED.** §15.4 rewritten to state factual CLAUDE.md labels and the five reviewers' own actual verdicts, including — after the fix round's own doc rewrite initially re-introduced a similar problem by asserting the P1 was closed when `security`'s own re-verification had just reproduced it — a further correction recording that item as genuinely open until the deterministic replacement test (above) actually closed it |
| PHASE-D-CR-P2-1 | P2 | `code-reviewer`: none of `docs/active-stage.md`, `docs/progress.md`, or this registry's own Phase D section (every prior Stage 4I phase's commit updated all three) were touched by the implementation/fix-round commits, leaving the repo self-contradicting (`active-stage.md` asserting Phase D "remains unauthorized" while Phase D code existed in the working tree) | **FIXED.** This section, plus the `active-stage.md`/`progress.md` entries accompanying this commit |
| PHASE-D-CR-P3-3 | P3 | `code-reviewer`: ADR 0043/canonical-model §15.2/`evaluation_policy_admin.go`'s header each said writing `status='active'` "is refused outright"/"blocks it structurally" — true only of the sanctioned Go write path; migration 0075 imposes no database-level guard against an `active` row (the CHECK and RLS INSERT policy both admit one from any platform-admin-scoped writer) | **FIXED.** All three re-worded to state the refusal is an application-layer control, not a schema-level one, with an explicit note that a future activation-writer phase must not assume otherwise |
| PHASE-D-CR-P3-5 | P4 | `code-reviewer`: the 23514→`ErrConcurrentPolicyWrite` mapping is correctly scoped to the one UPDATE it's meant to cover, but the same 23514 also fires — deterministically, not as a race — when a caller invokes `CreateEvaluationPolicyVersion` twice for the same key inside ONE transaction (each call shares that transaction's single `now()`), surfacing a misleading "retry" signal for what is actually a caller-usage-contract violation | **NOT FIXED — documented instead.** Distinguishing the two cases reliably requires additional bookkeeping (e.g. an `xmin`-based check) disproportionate to a diagnostic-quality issue with zero functional or security impact (nothing is written either way; the transaction rolls back). `CreateEvaluationPolicyVersion`'s doc comment now states the constraint explicitly: call at most once per transaction per key |
| PHASE-D-CR-P3-2 | P3 | `code-reviewer`: ~35 lines of row-scanning (column list, scan targets, post-processing) are byte-identical between `ListEvaluationPolicyVersions` and `readEvaluationPolicyRecordByID`, with no shared helper — a future column addition updating only one risks a silent divergence between the audit trail and the list surface | **NOT FIXED — accepted, deferred.** Genuine simplification opportunity, but the code has already been through two full review-and-fix rounds; reopening tested code for a refactor with no behavioural defect is deferred to whichever future phase next adds a column to `EvaluationPolicyRecord` (the PC-GAP-1/2 content phase, at the earliest) |
| PHASE-D-CR-P4-2 | P4 | `code-reviewer`: withdrawing an already-withdrawn key succeeds, producing an unbounded chain of content-free tombstones with no semantic content | **NOT FIXED — accepted.** Harmless (the read path still correctly returns `ErrPolicyNotActive`); a future admin UI would need its own idempotency guard on repeated withdrawal clicks, not a change to this function's contract |
| PHASE-D-CR-P4-3 | P4 | `code-reviewer`: a no-op `UPDATE ... SET status = status` on an open row falls through to the append-only trigger's last branch and raises "effective_to may not be cleared" — a confusing message for what the caller actually did | **NOT FIXED — accepted, cosmetic.** No caller in this phase issues such an update; correct outcome (rejection), misleading diagnostic only |
| PHASE-D-QA/CR gaps (audit content, dead subtest, doc-comment duplication) | P2-P4 | `qa` and `code-reviewer`, independently: the audit-shape test declared `var after map[string]any` and never scanned into it (before/after content unverified); the immutability test's `delete_rejected` subtest asserted nothing (a 0-row-affected DELETE returns no error); three near-identical doc-comment paragraphs were duplicated verbatim across file headers and their own exported symbols | **Audit content and dead subtest: FIXED** (real JSON assertions added; `RowsAffected()==0` plus row-survival asserted). **Doc-comment duplication: NOT FIXED — accepted, cosmetic**, no functional risk |

**Verdicts, all independent, none self-certified:** `architect` — original
review BLOCKED (one P1, four P2s); CERTIFIED WITH NAMED EXCEPTIONS after
both fix rounds, the P1 closed by a genuinely deterministic test
independently re-run by the orchestrator (30 consecutive passes, no
failure). `security` — CERTIFIED WITH NAMED EXCEPTIONS on both its original
review and its fix-round re-verification (no P0/P1 on either pass; the
named residual on `max_location_signal_age_seconds`'s unbounded staleness
ceiling is accepted with conditions per ADR 0043 Decision 8). `identity-compliance`
— NO VIOLATIONS FOUND. `qa` — original review NOT READY (the same P1,
found independently); READY WITH NAMED GAPS after both fix rounds.
`risk` — NO INTEGRATION CONCERNS (two informational notes: a correction to
this dispatch's own overstated premise about `internal/risk`'s coupling to
`OperationClass`, and a suggested — not applied — clarifying clause in
HDR-J-7's "not blocked" list naming Risk's R-2b precondition explicitly).
`code-reviewer` — READY WITH NAMED EXCEPTIONS (no P0/P1; the governance-doc
gap was the only P2, closed by this section and its companion entries).

**Independently confirmed, not merely trusted:** the orchestrator itself
re-ran (rather than accepted on report) the two concurrency tests 15× each
under `-race` after the final fix, the full `internal/jurisdiction`/
`internal/auth` integration suites, and the whole-repo
`go test -tags=integration ./...` gate — all green. One pre-existing,
unrelated test outside this phase's own files
(`internal/bonus/wave3_phase2_migrations_integration_test.go`'s
`TestWave3Phase2Migrations_FullChainUpDownUpRoundTrip`) required updating
its hardcoded migration-chain window by one entry, mirroring the identical,
already-established pattern each of migrations 0071-0074 required in turn
when it became the chain's new tip — not a Phase D defect, a routine
consequence of adding a migration, fixed and re-verified passing.

**Carried to a future phase (not this phase's to build, confirmed
absent):** HDR-J-7/HDR-J-8/HDR-J-9's actual content (all three genuinely
open, correctly worded as neutral questions per `identity-compliance`'s
review); any production wiring of `ResolveEvaluationPolicy`/
`RequiredPurposes`/`CreateEvaluationPolicyVersion` into `casino`, `bonus`,
`risk`, or `assetregistry`; an activation permission and dual-control
ruling for writing `status='active'` (ADR 0043 Decision 5); the
`jurisdiction_resolutions.reason` CHECK widening for Phase C's three new
`Reason` values (architect's Correction 2, deliberately not touched —
shape-without-a-writer); the deferred PHASE-B-ARCH-1 `effective_from`/
actor-provenance defect on `jurisdiction_resolution_active`/
`jurisdiction_evidence_collection_active` (re-evaluated this phase per its
own three trigger conditions — none fired, remains deferred, unchanged);
a `btree_gist`-based range-exclusion constraint closing PHASE-D-ARCH/SEC-P3-2's
narrow raw-SQL-only residual; a maximum-staleness ceiling for
`max_location_signal_age_seconds` (framed as a sub-question inside HDR-J-9,
not decided); the row-scan duplication refactor (PHASE-D-CR-P3-2); all
carried-forward items from every prior Stage 4I phase, unchanged.

## Stage 4I Phase E — Operating Market & Country Policy Foundation

**Status: IMPLEMENTED as a MECHANISM ONLY**, built exactly to the
architect design ruling recorded in full at `docs/decisions/0045-
operating-market-and-country-policy-foundation.md`. Migration `0076`
(`jurisdictions.country_code`, `platform_operations`,
`licence_country_ceilings`, `operating_country_policies`); new package
`internal/operatingmarket`; new file
`internal/jurisdiction/licence_validity.go` (`EvaluateLicenceValidity` —
`internal/jurisdiction`'s own resolver files carry ZERO diff); four new
permissions (`internal/auth/permission.go`). Zero HTTP routes, zero
OpenAPI change, zero production wiring of `ResolveOperatingCountryPolicy`/
`IsRegistrationPermitted` into any consuming domain, zero country/market
content (only the four seeded `platform_operations` vocabulary rows
exist). Originally 32 tests/test-groups; the AMENDMENT-1 fix round
(`MKT-NARROW-1`) brought the package to 49 top-level test functions; the
AMENDMENT-2 fix round (SEC-E-REV-1, §17) brought it to 63, all passing
under `-race` (10 consecutive runs) and the whole-repo `go test
-tags=integration ./...` gate, run against a freshly-migrated scratch
database per `MKT-MIG76-1` (below).

### New items opened this phase

- **MKT-DUAL-1 — four-eyes on operating-market enablement.** Phase E
  builds no dual control because `asset_change_requests` cannot carry it
  (four structural blockers, verified against migrations 0044 **and**
  0047, ADR 0045 §7.1) and `bonus_change_requests` is bonus-domain-specific.
  Creating a **third** platform approval mechanism is a cross-cutting
  decision requiring its own ADR and an architect ruling on whether the
  right answer is a generic `change_requests`/`change_approvals` pair all
  three domains migrate onto. **This item MUST be resolved before ANY of
  the following ships:** (a) any HTTP route, partner-console surface, or
  service-identity caller reaching either Phase E write path; (b) any
  wiring of `ResolveOperatingCountryPolicy` into any consuming domain; (c)
  the first real (non-test) country row in `licence_country_ceilings` or
  `operating_country_policies`. Until then, the absence of four-eyes is a
  known, bounded, zero-exposure gap (no production-reachable enable path
  exists at all), and no document may describe Phase E's enable path as
  dual-controlled. Owned by `architect`, `security` consulted. HDR-M-1
  (production authorization governance) remains open and is **not**
  answered by this item. The dual-control mechanism this item
  produces must cover every widening-capable write this domain can make,
  not merely every write that literally sets `state='enabled'` — this
  includes the AMENDMENT-2 inherit-rung withdrawal shape
  (`ocp_inherit_rung_withdrawal_requires_authorization`). The audit field
  `widening_capable` (ADR 0045 §10/§17) is the authoritative definition of
  the set this item must cover.
- **MKT-PM-1 — remove `licences.permitted_markets`.** The phase that
  answers HDR-J-6 and writes the first real `licence_country_ceilings`
  content must drop `licences.permitted_markets` (and
  `CreateLicenceParams.PermittedMarkets`) in the same change, so the
  platform never holds two country lists with one populated.
- **MKT-EXPIRY-1 — confirm BOTH licence validity boundaries with
  compliance before production wiring** (widened by ADR 0045 §18, finding
  F4, to cover issuance as well as expiry). `EvaluateLicenceValidity`
  treats a licence as invalid **on** its own stated `expires_at` date
  (strict, exclusive upper boundary) and as invalid **before** (but valid
  **on**) its own stated `issued_at` date (inclusive lower boundary) — a
  half-open interval, both ends a disclosed, fail-closed engineering
  default, not a legal determination. The phase that first wires
  `ResolveOperatingCountryPolicy` into any consuming domain must confirm
  BOTH the inclusive/exclusive expiry boundary and the issuance boundary
  with compliance before that wiring goes live, and must not assume this
  engineering default settles either question. Also open: whether
  `licences.issued_at` (currently `DATE` and NULLABLE, written by no Go
  code today) should become `NOT NULL` with a populating admin surface —
  a NULL `issued_at` is currently treated as "no issue date asserted"
  (falls through, not treated as "not yet issued"), which fails closed
  for every existing licence row but leaves the "not yet issued" check
  structurally unable to fire until some writer populates the column.

### MKT-NARROW-1 — operation-rung resolution defect (§3.5-A AMENDMENT-1)

**Status: RESOLVED IN PHASE E FIX ROUND.**

**Defect.** Three independent reviewers (architect-fidelity via static
trace, security via live API reproduction, DB/RLS via raw SQL) found that
the original operation-rung resolution algorithm (most-specific-candidate-
only, falling through to inherit on absence/withdrawal) let a
more-specific `enabled` row both widen past an already-disabled broader
row AND — reachable using ONLY narrowing writes, no widening needed —
once its own narrower disable was withdrawn, unmask a broader in-force
`disabled` row back into `permitted`. This falsified the resolution
algorithm's own central correctness claim ("explicit DISABLED always
overrides inherited ENABLED").

**Fix (architect-ruled amendment, Option C).** (1) Migration 0076
(amended in place — no new migration file; it was uncommitted and
unreleased) gains a write-time step (4) in
`operating_country_policies_enforce_ceiling()` refusing to enable a more-
specific operation/product row while a broader in-force active disabled
row for the same operation exists. (2)
`internal/operatingmarket/resolve.go`'s STEP 4 is rewritten to query the
FULL operation-rung candidate set (up to 4 rows, no `LIMIT`) and evaluate
it as a SET with first-disabled-wins: duplicate-rank and parse checks over
the full set, withdrawn candidates treated as tombstones (never permit,
never block, never mask), the LEAST specific live-disabled candidate names
the block, and only the MOST specific live row sources a `permitted`
result. The resolver is correct even with the write-time trigger bypassed
entirely (raw SQL) — see ADR 0045 §3.5-A AMENDMENT-1 and INV-M-7.

**Tests.** New/replaced tests in `internal/operatingmarket`:
`TestOperatingCountryPolicy_ProductSpecificDisableNarrowsEveryProductEnable`
(replaces the deleted, wrongly-asserting
`TestOperatingCountryPolicy_ProductSpecificRowBeatsEveryProductRow`),
`TestOperatingCountryPolicy_MoreSpecificEnableUnderBroaderDisableIsRefused`,
`TestOperatingCountryPolicy_BroaderDisableStillBlocksAfterNarrowerRowWithdrawn`
(the exact regression reproducing the reviewers' sequence via the public
API only), `TestOperatingCountryPolicy_RawSQLWidenedRowCannotProducePermitted`,
`TestOperatingCountryPolicy_BroadDisableAfterNarrowEnableBlocksRegardlessOfWriteOrder`,
`TestResolveOperatingCountryPolicy_BlockingRowIsTheBroadestLiveDisable`,
`TestResolveOperatingCountryPolicy_DuplicateRankDetectedBeneathAHigherRankedCandidate`
(fails against the OLD `LIMIT 2` query, proving the fix is load-bearing),
`TestExplain_EmitsEveryApplicableOperationCandidate`. `PolicyVersion`
bumped `stage-4i-e.v1` → `stage-4i-e.v2` (no backfill of any existing
row's stored value — none existed to backfill; zero rows in either table
as of this phase).

**Explicitly unchanged:** every RLS policy/predicate on all three tables;
every permission constant/role grant; every CHECK constraint, index, FK;
`Result`'s eight accessors and eleven outcomes; the licence ceiling's own
logic (trigger steps 1-3); the two-dimension (operation × product) model
(confirmed, not weakened); the four licence behavioral cases; no new HTTP
route; no resolver wiring into any consuming domain.

- **MKT-SCOPE-1 / MKT-SCOPE-1(b) — RESOLVED (Stage 4I Phase E-SECURITY,
  migration 0077, ADR 0046).** Originally: pre-existing `internal/
  jurisdiction` platform-registry functions omit `assertPlatformScope`;
  `jurisdictions`/`licences`/`tenants` carry no RLS at all. Found during
  the Phase E fix round while adding the scope assertion to
  `SetJurisdictionCountryCode`. The PRE-EXISTING writers
  (`CreateJurisdiction`, `CreateLicence`, `AssignTenantLicence`) and
  readers (`ListJurisdictions`, `ListLicences`) had the identical gap and
  the identical missing database-level backstop. **MKT-SCOPE-1(b)**
  (independently-labeled trigger condition, ADR 0045 §18) required
  resolution before `ResolveOperatingCountryPolicy` was wired into any
  enforcement path, because Phase E made `licences` the ROOT of the
  operating-market ceiling.

  **Resolution.** A binding architect ruling (ADR 0046) gave `tenants`,
  `licences`, and `jurisdictions` row-level security (migration 0077):
  `tenants`/`jurisdictions` read-open (`USING (true)`, migration 0044's
  `assets` precedent — several scopeless/cross-tenant readers legitimately
  need it), `licences` narrowed to platform-admin or the tenant whose own
  `tenants.licence_id` names the row; every write on all three restricted
  to a genuinely platform-admin-scoped transaction, with `tenants` alone
  additionally getting a platform-admin-only DELETE policy (the one
  legitimate DELETE on that table). `AssignTenantLicence`'s contract moved
  from `db.Pool.WithTenant` to `db.Pool.WithPlatformAdmin`, and its audit
  row moved from tenant-scoped to platform-scoped as a direct consequence.
  `internal/identity.CreateTenant` gained the identical
  `assertPlatformScope` gate; the platform-admin-only `tenant.created`
  HTTP route moved from `WithoutTenant` to `WithPlatformAdmin` and now
  writes a `reason_code`/before-after audit record (previously present but
  incomplete). `internal/operatingmarket`'s own schema/triggers/resolution
  algorithm received **zero** executable diff — its `assertTenantScope`
  remains load-bearing (read-side isolation for `tenants` is still
  deliberately open) and its RLS policies were already correct; only the
  premise they rested on (that `tenants` had no writable path) was wrong,
  and that premise is what this migration fixes.

  **Two previously-unrecorded attacks, live-reproduced and now closed,
  recorded here for the historical record:**
  1. **The composite-FK-defeating combined UPDATE.** Migration 0007's
     `tenants_licence_matches_model` composite FK
     (`FOREIGN KEY (licence_id, expected_licensee) REFERENCES licences
     (id, licensee)`) was believed to make a licensee/licensing_model
     mismatch structurally impossible. It does not defend against a
     SINGLE UPDATE statement changing `licensing_model` AND `licence_id`
     together: `expected_licensee` is a `GENERATED ALWAYS ... STORED`
     column recomputed from the NEW `licensing_model` value in the same
     statement, so the FK check only ever sees a self-consistent
     (new-model, new-licence) pair and never detects that the tenant
     "became" a different licensing model specifically to accept a
     licence its ORIGINAL model would have rejected. Combined with the
     complete absence of RLS on `tenants`, an ordinary tenant-scoped
     connection could execute exactly this combined UPDATE and forge a
     match the FK was trusted to prevent. Migration 0077 closes this
     without any FK/CHECK change: a tenant-scoped connection can no
     longer write `tenants` AT ALL, so the combined-UPDATE shape is
     refused identically to a single-column one. Regression:
     `TestTenantsRLS_TenantScopedConnectionCannotDefeatCompositeFKByChangingLicensingModel`.
  2. **The cascading-DELETE escalation of finding F2.** ADR 0045 §18
     finding F2 disclosed that `operating_country_policies` has no
     DELETE-protecting trigger (by design, to preserve `tenants.id ...
     ON DELETE CASCADE`), and named "the migration-owner/runtime-role
     separation" as the residual's genuine fix, implying the exposure
     required a privileged/bypassing role to reach. Live reproduction
     during this dispatch found the bar was **strictly lower**: with no
     RLS on `tenants` at all, an ORDINARY tenant-scoped (or even
     scopeless) connection could `DELETE FROM tenants WHERE id =
     <a different tenant's id>` directly — no elevated role, no RLS
     bypass, nothing beyond an ordinary application connection — and the
     `ON DELETE CASCADE` (which runs with RLS bypassed by Postgres
     itself, regardless of the deleting connection's own scope) would
     remove that OTHER tenant's entire `operating_country_policies` set.
     Migration 0077 closes this as a NET TIGHTENING: `tenants` DELETE is
     now restricted to platform-admin scope, so this specific escalation
     path is closed, while the underlying, narrower, role-bypass-only
     residual F2 originally described remains open (now tracked as
     `PLAT-ROLESPLIT-1`, below — genuinely blocked on infrastructure this
     repository cannot provide). Regression:
     `TestTenantsRLS_TenantScopedConnectionCannotDeleteAnotherTenant`,
     `TestOperatingCountryPolicies_TenantDeleteCascadeNowRequiresPlatformAdminScope`.

  Also fixed in the same migration, not an RLS matter: **BYOL licence
  exclusivity** — two distinct tenants, both `licensing_model=
  'own_licence'`, could bind the SAME `licensee='tenant'` licence
  (`tenants_licence_matches_model` only checks licensee KIND, never
  exclusivity), silently sharing one licence's entire
  `licence_country_ceilings` set. Closed by
  `uq_tenants_exclusive_own_licence`, a partial unique index on
  `tenants(licence_id) WHERE licence_id IS NOT NULL AND
  expected_licensee = 'tenant'` — deliberately keyed on the GENERATED
  `expected_licensee` column so it cannot drift from `licensing_model`,
  and deliberately NOT constraining `licensee='platform'` licences, which
  remain legitimately shared across every `under_platform_licence` tenant
  per ADR 0006. Regressions:
  `TestTenantsRLS_ExclusiveOwnLicenceCannotBeBoundToTwoTenants`,
  `TestTenantsRLS_PlatformLicenceMayStillBeSharedAcrossManyTenants`.

  Full ruling, task dispositions, and read-posture rationale: ADR 0046.
  Test evidence: `internal/jurisdiction/migration_0077_integration_test.go`,
  `internal/jurisdiction/registry_rls_integration_test.go`, and the
  additions to `internal/operatingmarket/rls_integration_test.go`.

### New items opened by Stage 4I Phase E-SECURITY (ADR 0046)

- **`PLAT-ROLESPLIT-1` — migration-owner/runtime-role split.** The
  genuinely narrower residual ADR 0045 §18 finding F2 originally
  described (a role that bypasses RLS entirely — DDL access, or a
  superuser/BYPASSRLS connection — can still DELETE/TRUNCATE/ALTER any of
  these tables with no application-level control of any kind) remains
  open; migration 0077 closes only the ORDINARY-application-connection
  escalation (see MKT-SCOPE-1's resolution note above), not this one.
  Owner: `security`. Dependency: a PostgreSQL superuser action
  (`CREATEROLE`) to provision a second, narrower-privileged application
  credential — neither is available to this repository today (verified
  live by the architect: the application role lacks `CREATEROLE`). **Not**
  a gate on Phase E resolver wiring itself. Pre-production gate: before
  the first production deployment against real tenant data, OR before the
  first `RoleTenantAdmin`/`RoleCompliance` credential grant to anyone
  outside platform-operator staff, whichever comes first.
- **`MKT-LICSTATUS-1` — no sanctioned write path for `licences.status`.**
  `registry_admin.go`'s `CreateLicence` always creates `active` rows and
  deliberately exposes no status-transition operation
  (suspend/reinstate/expire); every non-active licence fixture in this
  codebase is seeded via raw SQL, never through application code. A real
  licence WILL eventually need a status change (regulatory suspension,
  renewal, non-renewal). Owner: `architect` (the write surface's shape —
  who may call it, what reason codes/evidence it requires, whether it
  needs dual control — is a cross-cutting design question, not a routine
  CRUD addition). Gate: before the first real (non-test)
  `licence_country_ceilings` row — the same gate as `MKT-DUAL-1`/
  `MKT-PM-1` — since a ceiling's authority is only as good as the licence
  underneath it being genuinely current.
- **`MKT-AUDIT-1` — tenant-visible evidence of licence assignment.**
  `AssignTenantLicence`'s audit row is now platform-scoped only
  (`tenant_id IS NULL`), a direct consequence of migration 0077's write-
  scope fix (see MKT-SCOPE-1's resolution note above) — the affected
  tenant itself can no longer read back its own licence-assignment
  history via its own `PermAuditRead`, where it previously could (a
  tenant-scoped audit row, Phase A's own design). Not yet needed: no
  tenant-facing surface exposes licence-assignment history today, and no
  consumer has asked for one. Owner: `security` (any tenant-visible
  evidence surface over `licences`/`tenants` history is a
  disclosure-boundary decision, not a routine reporting feature). Gate:
  before any partner-console/back-office surface exposes a tenant's own
  licence-assignment history to that tenant, **or before the first B2B/
  non-first-party tenant onboarding, whichever comes first** (compliance/
  privacy review, Stage 4I Phase E-SECURITY fix round: a real external
  operator has a materially stronger interest in seeing its own
  licence-assignment history than our own first-party B2C brand does, so
  the gate must not wait solely on a UI surface existing).

### New items opened by the Stage 4I Phase E-SECURITY fix round (six-review dispatch)

- **`MKT-DORMANT-1` — dormant tenant-rung policy silently resumes on
  licence-ceiling re-expansion.** (Adversarial security review's SEC-E2-5.)
  When a licence's country ceiling is contracted (narrowed) and later
  re-expanded, a tenant-rung `operating_country_policies` policy that was
  left `enabled` from before the contraction silently resumes being
  effective on re-expansion, with no new authorization event or audit
  record generated at the tenant rung at the moment it resumes taking
  effect. This may or may not be the intended semantics — a licence
  contraction is typically a regulator-driven revocation, and whether
  automatic resumption on re-issuance is correct or requires a fresh
  affirmative act is a policy question, not obviously a bug. **Not fixed
  this round; explicitly not a blocker.** Owner: `architect`.
  `resolve.go`'s algorithm and ceiling/tenant-rung policy semantics were
  NOT changed to address this — any fix is a deliberate design decision,
  not a mechanical patch.
- **`PLAT-TENANTREAD-1` — `tenants_read` permits full cross-tenant
  enumeration.** (Architect fidelity review, distinct from the Fix 5
  player-scope exclusion this same fix round shipped.) Even after Fix 5
  excludes player scope, `tenants_read` remains `USING (true)` for every
  other scope, so any tenant-scoped connection can enumerate every OTHER
  tenant's `name`/`slug`/`licensing_model`/`status`/`licence_id` — no PII,
  no licence *content* (that's `licences`, separately narrowed), but
  potentially commercially sensitive in a B2B multi-operator context once
  real external tenants exist (e.g. one operator learning a competitor
  operator's licensing model or account status). Owner: `architect`/
  `security`, to be revisited once real B2B tenant onboarding is planned.
  **Not fixed this round; explicitly not narrowed beyond what Fix 5
  specifies** — tenant-to-tenant read visibility is a separate, bigger
  design question with real tradeoffs (see ADR 0046/migration 0077's own
  "no read-side narrowing was pursued" rationale), not a mechanical
  tightening.

### SEC-E-REV-1 — disposition: CLOSED by AMENDMENT-2

Finding SEC-E-REV-1 (an inherit-rung withdrawal at the BRAND/OPERATION
scope is functionally a widening act but was treated as unconditionally
fail-closed-safe, with no `authorization_reference` required and no audit
distinguishability) is **CLOSED** by ADR 0045 §3.5-A AMENDMENT-2 (§17): a
new CHECK constraint (`ocp_inherit_rung_withdrawal_requires_authorization`,
migration 0076 amended in place), a mirroring Go-side guard in
`policy_admin.go`, and two new audit metadata keys
(`rung_block_transition`/`widening_capable`) making the widening shape
distinguishable after the fact. `internal/operatingmarket/resolve.go` and
`operating_country_policies_enforce_ceiling()`'s executable body both
carry ZERO diff — this is a write-time-only fix (see ADR 0045 §17's own
"why no read-time half" reasoning). `PolicyVersion` stays
`"stage-4i-e.v2"` (no algorithm change).

### MKT-MIG76-1 — migration 0076 amended in place three times; stale-schema hazard

Migration 0076 has now been amended in place THREE TIMES since it was
first authored this stage — once for AMENDMENT-1 (§16, the write-time
narrowing step plus the trigger comment fix), once for AMENDMENT-2 (§17,
the new `ocp_inherit_rung_withdrawal_requires_authorization` CHECK
constraint plus two comment fixes), and once for AMENDMENT-3 (§18, the
new DEFERRED constraint trigger `ocp_inherit_rung_close_requires_
successor` plus two comment fixes, one of them the F2 disclosure). The
migration's version number (`0076`) has not changed any of the three
times, because it remains untracked/uncommitted and was never released —
this is the ONLY reason in-place amendment is permitted here at all
(CLAUDE.md's ledger/migration discipline otherwise treats a released
migration as immutable).

**Binding instruction, applicable to every session/stage that touches
this migration or `internal/operatingmarket` before it is released:**
ANY environment (local dev database, CI cache, a previously-created
scratch database) where migration 0076 was applied BEFORE any of the
three amendments landed carries a STALE schema — it is missing the
write-time narrowing trigger step (AMENDMENT-1), the new CHECK constraint
(AMENDMENT-2), and/or the new deferred constraint trigger (AMENDMENT-3),
silently, with no version-number signal that anything is wrong. Such an
environment MUST be rebuilt via a full migrate-down-then-up cycle (or
dropped and recreated from a fresh `cmd/migrate up` run) before any test
result against it is trusted. This was confirmed as a REAL, not
theoretical, hazard during AMENDMENT-2's own verification: the shared
local dev database (`igaming_platform_dev`) was found to still be missing
`ocp_inherit_rung_withdrawal_requires_authorization` after AMENDMENT-2 was
authored, because it had been migrated up before that session's changes
landed — verification therefore used a genuinely fresh scratch database
instead. `TestMigration0076_SchemaMatchesTheCurrentMigrationFile` is the
regression guard for this exact class of hazard going forward, and — as
of the Phase E polish round that closed this item — genuinely asserts on
all THREE amendments' markers, not just the first two: the RAISE-message
substring `"may only narrow and may never widen"` in the EXECUTABLE body
of `operating_country_policies_enforce_ceiling` (AMENDMENT-1); the CHECK
constraint `ocp_inherit_rung_withdrawal_requires_authorization` in
`pg_constraint` (AMENDMENT-2); and, for AMENDMENT-3, both the
`ocp_inherit_rung_close_requires_successor` row in `pg_trigger` (asserting
`tgdeferrable`/`tginitdeferred` are both true — the DEFERRABLE INITIALLY
DEFERRED property AMENDMENT-3's own liveness argument depends on) and the
`operating_country_policies_enforce_close_successor` function in
`pg_proc`. Before this fix the test only checked the first two markers,
even though this paragraph's prose already (prematurely) described it as
covering all three — that gap is what this fix closes.

**Expires** when migration 0076 is released (committed and merged to the
trunk branch other stages build from). **After release, no further
in-place amendment of 0076 is permitted** — any further fix becomes a new,
separately-numbered migration, per ordinary migration discipline. Owned
by whichever specialist next touches `internal/operatingmarket` or
migration 0076/0077+; no action required if 0076 is released with all
three amendments already folded in, as is the case as of this entry.

### SEC-E-REV-2 — disposition: CLOSED by AMENDMENT-3

Finding SEC-E-REV-2 ("the bare close": setting `effective_to` on an
in-force `active`+`disabled` brand- or operation-scope row with NO
successor row is functionally identical to an inherit-rung withdrawal —
the same widening effect AMENDMENT-2 gated — but was reachable with no
`authorization_reference` and no audit distinguishability, because every
control on this table before AMENDMENT-3 was INSERT-shaped and none
gated a bare UPDATE-shaped close) is **CLOSED** by ADR 0045 §3.5-A
AMENDMENT-3 (§18), found and fixed in this same round: a new DEFERRED
constraint trigger (`ocp_inherit_rung_close_requires_successor`,
migration 0076 amended in place a third time) refusing, at COMMIT, to let
a bare close of such a row stand unless an open successor version exists
at the same key in the same transaction; a new Go sentinel
(`ErrPolicyCloseRequiresSuccessor`) and classifier
(`ClassifyCommitError`) in `internal/operatingmarket`, since the refusal
surfaces only from the caller's own `tx.Commit()`, never from a call in
the package itself. `internal/operatingmarket/resolve.go` and
`operating_country_policies_enforce_ceiling()`'s/`operating_country_
policies_enforce_append_only()`'s executable bodies all carry ZERO diff —
this is a write/commit-time-only fix (see ADR 0045 §18's own "why a
constraint trigger" reasoning, including the explicit rebuttal of §17's
own CHECK-vs-trigger argument). `PolicyVersion` stays unbumped by
AMENDMENT-3 itself — the `v2` → `v3` bump recorded in this same round is
attributable entirely to finding F4 (below), a separate, independently-
attributed change. This is disclosed as the THIRD and, per ADR 0045
§18's own structural argument, FINAL amendment in this defect family —
see that section's "do NOT commission a fourth round" guidance.

### SEC-4I-F10 — re-scoped, NOT closed

SEC-4I-F10's own hard trigger ("any change that introduces a licence
status transition must land with a resolver-query change and a security
review, in the same change") does not fire this phase — Phase E
introduces no licence status transition. `internal/jurisdiction/
resolver.go`'s own `resolveTenantLicence` still checks neither `status`
nor `expires_at` and has ZERO diff from this phase. What changed: the
single, tested, shared technical predicate this finding's eventual
closure needs (`jurisdiction.EvaluateLicenceValidity`) now exists and is
consumed by `internal/operatingmarket`'s own ceiling check. The remaining
work for the player-jurisdiction path is exactly one call site plus one
semantic choice (`refused(dependency_unavailable)` vs. `unresolved`)
instead of a whole design — still requiring its own resolver-query change
and security review, in the same change, per this finding's own closing
condition. **This finding is NOT closed by Phase E.**

### PHASE-B-ARCH-1 — re-evaluated, none of its three trigger conditions fired

Re-evaluated against Phase E as specified in ADR 0045, the same method
used for every prior phase: (a) Phase E adds zero HTTP routes, zero
OpenAPI changes, zero console surfaces — the history surfaces it does add
(`ListOperatingCountryPolicyVersions`, `ListLicenceCountryCeilingVersions`)
read only the two brand-new, append-only-by-construction tables; (b)
Phase E produces no report, export, or evidence pack of any kind; (c)
Phase E does not read, write, alter, reference, or import
`jurisdiction_resolution_active` or
`jurisdiction_evidence_collection_active` — `internal/operatingmarket`
contains no reference to either table or to `SetResolutionActive`/
`IsActive`/`SetEvidenceCollectionActive`/`IsEvidenceCollectionActive`.
**The deferred item remains deferred, unchanged.** `audit_log` remains
the authoritative source of activation-change history for those two
tables.

### A defect found and fixed during implementation (not present in the architect's own ruling)

The down-migration's own existence-guard (refusing a rollback while
`operating_country_policies`/`licence_country_ceilings` hold rows) was
**silently ineffective** as first coded: `db.Pool.MigrateDown` runs the
down-migration SQL over a plain, scopeless connection (no
`app.platform_admin_principal_id`/`app.tenant_id` GUC set), and — unlike
migration 0075's deliberately permissive `jurisdiction_precedence_configs`
read policy (`USING (true)`) — both new tables' RLS read policies are
narrower than a scopeless connection can ever satisfy (`operating_country_
policies` in particular has **no platform-wide read policy at all**, by
design). The guard's own `EXISTS` checks would therefore always see zero
rows regardless of real content, defeating the guard exactly when a
dirty-database rollback attempt needed it most. **Fixed** by temporarily
disabling RLS on both tables inside the SAME transaction as the existence
checks (safe and self-contained: a `RAISE EXCEPTION` on a real finding
rolls back the `ALTER TABLE ... DISABLE ROW LEVEL SECURITY` along with
everything else; a clean pass proceeds to drop both tables outright,
making their RLS posture moot). Caught by
`TestMigration0076_DownMigrationCleanThenFailsOnDirtyDatabase` genuinely
failing against a real database on the first implementation pass, not by
code review — recorded here per this project's own "flag what the ruling
did not anticipate" discipline (ADR 0045 §0's own precedent).

### Explicitly NOT this phase's to build (confirmed absent)

Any HTTP route or OpenAPI change; any resolver wiring of
`ResolveOperatingCountryPolicy`/`IsRegistrationPermitted` into `casino`/
`bonus`/`risk`/`payments`/`sportsbook`/registration/withdrawal; any
country/market content anywhere (migration 0076 inserts only the four
seeded `platform_operations` vocabulary rows); any change to
`internal/jurisdiction/resolver.go`/`precedence.go`/`types.go`/
`evaluation_policy.go`/`evaluation_policy_admin.go`/`resolution_active.go`/
`evidence_collection_active.go` (verified zero diff via `git diff
--stat`); any licence status-transition operation; any cache of a
resolved outcome; any `operating_market_resolutions` table; any fourth
activation switch; any real geolocation; any answer to HDR-M-1/HDR-M-2/
HDR-J-6/HDR-J-7/HDR-J-8/HDR-J-9.

## Stage 4I Exit Triage — Exit Register and Production Integration Readiness

Per the "STAGE 4I — EXIT TRIAGE AND PRODUCTION INTEGRATION READINESS"
directive: an explicit change in execution strategy away from further
jurisdiction/KYC/security architecture review and toward delivery
velocity. Player-jurisdiction, licensing, operating-market, KYC, bonus,
sportsbook, wallet/ledger, payments, and RG architecture are all
explicitly FROZEN this stage — no concrete implementation dependency was
found that required reopening any of them. **No Go code, migration, or
existing architecture document was modified this stage** — this stage's
entire output is two new governance/security documents plus the routine
doc-trailer updates (this registry, `active-stage.md`, `progress.md`).

| ID | Owner | Status | Dependencies | Files owned | Tests | Docs | Blockers | Integration |
|---|---|---|---|---|---|---|---|---|
| 4IET-01 | Orchestrator | Done | none | `docs/governance/stage-4i-exit-register.md` (new) | n/a (documentation) | full classification of every open Stage 4I task-registry item (`PLAT-ROLESPLIT-1`, `PLAT-TENANTREAD-1`, `MKT-LICSTATUS-1`, `MKT-AUDIT-1`, `MKT-DORMANT-1`, `MKT-DUAL-1`, `MKT-EXPIRY-1`, `MKT-PM-1`, `HDR-J-6/7/8/9`, `HDR-M-1/2`) into A/B/C/D/E per the directive's own rubric, plus a fail-closed "integration contract" table (informational, zero wiring) | none | n/a |
| 4IET-02 | Orchestrator (security-owned design), verified empirically against local dev Postgres | Done | none | `docs/security/runtime-role-separation.md` (new) | Empirical verification performed directly (not simulated): created a temporary non-owning Postgres role with only `SELECT/INSERT/UPDATE/DELETE` grants against the local dev database, connected as it, and confirmed it cannot disable RLS, disable triggers, `TRUNCATE`, `DROP`, `ALTER TABLE`, or `CREATE TABLE`, while ordinary `SELECT` still succeeds — the exact six-probe reproduction the adversarial security reviewer used against the current single-role setup in Phase E-SECURITY, now shown refused | `PLAT-ROLESPLIT-1`'s implementation-ready runbook: exact required migration-owner vs. runtime role privileges, the six-capability determination table, and the exact `GRANT`/`CREATE ROLE` script for an operator to run once per environment | **This is an infrastructure action outside this repository's reach — no production credential exists in this session, and CLAUDE.md's Environment Safety rule forbids requesting one.** Classified `PRODUCTION BLOCKER — EXTERNAL INFRASTRUCTURE ACTION` | n/a — cannot be integrated by this repository; awaits an operator running §6 of the runbook against each real environment |
| 4IET-03 | security, architect, qa (independent review) | Done | 4IET-01, 4IET-02 | n/a (review only) | n/a | Findings folded into this section and the exit register directly | none | See findings below |
| 4IET-04 | Orchestrator | Done | 4IET-01..03 | this registry, `docs/active-stage.md`, `docs/progress.md` | full validation gate (docs-only; `go build ./...`/`go vet ./...`/`gofmt -l` clean, no code touched) | this stage's completion report | none | n/a — **stage explicitly STOPS here; no implementation authorized** |

### `PLAT-ROLESPLIT-1` — precise design (this stage's core deliverable)

Full detail in `docs/security/runtime-role-separation.md`. Summary: the
root cause is table **ownership**, not the `CREATEROLE` attribute —
`igaming` owns the database, schema, and every table (it ran every
migration), and PostgreSQL RLS never applies to a table's owner
regardless of `FORCE ROW LEVEL SECURITY`. The fix is a second,
non-owning `igaming_runtime` role granted only
`SELECT/INSERT/UPDATE/DELETE`, with the existing `igaming` role kept
exactly as-is but restricted to the migration/deploy path only. No Go
code change is required (`internal/db.Pool` makes no assumption about the
connecting role owning anything). This requires one operator action
(`CREATE ROLE` + four `GRANT` statements, run once per environment by
someone holding `CREATEROLE`) that this repository/session cannot perform
itself.

### `MKT-SCOPE-1`/`MKT-SCOPE-1(b)` — status unchanged

Already RESOLVED (Stage 4I Phase E-SECURITY, migration 0077, ADR 0046).
This stage's triage did not reopen or modify that resolution.

### Items re-classified, not re-designed

`PLAT-TENANTREAD-1` and `MKT-DORMANT-1` were both re-examined against
their *actual* reachability (real HTTP routes / real code paths, not the
RLS predicate or resolver algorithm in the abstract) and confirmed safe
to leave exactly as previously recorded — see the exit register for the
concrete evidence (grep results, `ceiling_admin.go` audit-write
confirmation). Neither item's underlying mechanism was touched.
`MKT-LICSTATUS-1`, `MKT-AUDIT-1`, `MKT-DUAL-1`, `MKT-EXPIRY-1`, `MKT-PM-1`
and all six HDR items were confirmed to not block the next recommended
stage and were left exactly as previously recorded, with no new action
taken on any of them.

### Independent review findings (4IET-03)

**Architect review — landed.** Independently re-verified all three of
the orchestrator's own load-bearing empirical claims by reading code
directly (not trusting the draft): confirmed `ceiling_admin.go` writes a
platform-scoped, `widening_capable`-tagged audit row on every
`CreateLicenceCountryCeilingVersion` return path; confirmed zero
`operatingmarket`/`DeterminePlayerJurisdiction` occurrences anywhere in
`internal/httpserver`; confirmed no code in `internal/db` assumes the
connecting role owns its tables (the only runtime DDL is
`schema_migrations` creation inside `cmd/migrate`'s own path). Findings:
one P2 (the exit register's MKT-DUAL-1 section and integration-contract
footer had silently narrowed the registry's own three-trigger scope for
that item down to one — fixed by restoring the other two verbatim from
this registry's existing record); two P3 (MKT-EXPIRY-1's `issued_at`
schema/write-surface sub-question was dropped from the register's
restatement — restored; the "Operator Back-Office MVP" dependency-
readiness claim was verified line-by-line and found overstated for every
named capability except withdrawal approval — corrected throughout
`active-stage.md`/`progress.md`/this section, see above); two more P3
(PLAT-ROLESPLIT-1's and MKT-LICSTATUS-1's "blocks next stage: NO" needed
the same explicit conditional flag `MKT-AUDIT-1` already carries — added);
one P4 (the MKT-DORMANT-1 "only path to resumption" claim was narrower
than stated — `resolve.go` also gates on licence validity, so a licence-
validity-boundary crossing can resume a dormant policy with zero audit
row once `MKT-LICSTATUS-1` ships a write path — corrected and the two
items cross-linked); one P4 each on the dev/CI bootstrap script not
mirroring the proposed runtime-role split (noted as a follow-up in
`runtime-role-separation.md`, not applied — inseparable from actually
switching a live credential) and on the runbook's grant script over-
granting write access to `schema_migrations` (fixed directly in the
script). All findings applied; nothing left open from this review.
Classifications for `PLAT-ROLESPLIT-1`/`PLAT-TENANTREAD-1`/
`MKT-AUDIT-1`/`MKT-DORMANT-1`/`MKT-PM-1`/the HDR row were independently
confirmed to match this registry and ADR 0045/0046 with no discrepancy.
No new architecture created, no frozen decision reopened, no
jurisdiction/country content invented.

**QA review — landed.** No P0-P4 findings. Independently reproduced the
runtime-role-separation §5 verification end to end (own throwaway role,
identical six probes, identical denials, role dropped afterward);
independently confirmed the `PLAT-TENANTREAD-1` no-enumeration-route claim
via grep; spot-checked `MKT-DUAL-1` (zero `operatingmarket` references
anywhere in `internal/httpserver` — even stronger than the register's own
claim) and `MKT-AUDIT-1` (confirmed the tenant-scoped-audit-row
impossibility is structural, via `audit_log`'s RLS `WITH CHECK` plus the
platform-admin transaction never setting `app.tenant_id`, not merely
avoided by convention) against this registry's history — both matched.
Ran the full validation gate (`go build`/`go vet`/`gofmt -l` clean;
`git status --short` showed only the expected two new files plus this
registry's diff) and, since the recommended next stage depends on it, a
full fresh-scratch-database integration run (`-tags=integration`, all 30
packages green, no flakes) plus `go test ./... -count=1` (green) as a
back-office-readiness health check — nothing in current test coverage
would make this a bad time to start that stage. One E-level, non-blocking
housekeeping note (not acted on this pass): `make test-integration`
defaults `TEST_DATABASE_URL` to the same dev `DATABASE_URL`, so the local
dev database has accumulated ~67,000 `tenants` rows from repeated runs;
tests avoid collision via randomized slugs so this causes no false
passes, but it is exactly the kind of untrustworthy shared-DB history
`MKT-MIG76-1` already warns engineers not to rely on — worth eventually
pointing `test-integration` at a disposable database by default, tracked
here for awareness only, not a task-registry item.

**Security review — landed.** Independently reproduced §5's six probes
plus 20 additional escalation probes (trigger-bypass via
`session_replication_role`, `OWNER TO`, policy manipulation, `SET ROLE`,
`SECURITY DEFINER` function creation, `pg_authid` reads, `BYPASSRLS`
self-grant, schema creation, `GRANT ... WITH GRANT OPTION`, and more) —
all denied, confirming ownership (not `CREATEROLE`) is the load-bearing
fact and finding no escalation path out of the proposed design. Confirmed
zero `SECURITY DEFINER` functions exist anywhere in the database (no
function-invocation escalation possible) and confirmed the split is
transparent to real application code (`internal/db/tenant_rls.go`'s GUC
scoping needs no privilege; `identity`/`wallet`/`audit` integration suites
pass fully as the non-owning role).

**One P1 finding, root-caused and resolved during this review round:**
the reviewer found this session's long-lived local `igaming_platform_dev`
database had drifted — its live `tenants_read` policy was the stale,
pre-Phase-E-SECURITY-fix-round `USING (true)` text with no player-scope
exclusion, and ten `internal/operatingmarket` tests were failing
(AMENDMENT-2/3 enforcement not in force) — despite `schema_migrations`
showing migration 0077 applied and despite the CURRENT committed
migration file already containing the fix. Root cause confirmed directly
by the orchestrator: this local database had migration 0077 applied at an
earlier point in this session's lifetime, before that (then still
uncommitted) migration file's later in-place amendments landed — the
exact `MKT-MIG76-1` hazard this registry already documents, now
recurring against the orchestrator's own environment rather than a
reviewer's. **Not a defect in the committed code.** Fixed by dropping and
rebuilding the database fresh from HEAD's migration files; the previously
red test (`TestTenantsRLS_PlayerScopedConnectionReadsZeroTenants`) and the
ten `operatingmarket` tests all pass against the rebuilt database, and the
full 30-package `-tags=integration` suite is green. A genuinely
first-time deployment (staging or production) is not exposed to this
specific drift, since it applies today's already-corrected file for the
first time. New item opened for the underlying, generalizable tooling
gap this exposed — see `PLAT-MIGDRIFT-1` below.

Three P2/P3 corrections applied to the two documents: `docs/security/
runtime-role-separation.md` §5 now records the drift finding and its
resolution rather than silently citing a since-corrected verification
run; §4 now names `casino_games` (RLS disabled, no `tenant_id` column,
global catalogue data) as an explicit exception to the "ordinary DML is
genuinely enforced by RLS" claim, and records that CI/`make
test-integration` both run as the migration-owner role today, so the
split's correctness is verified once by hand and not continuously (a
follow-up CI recommendation, not implemented this pass). Two P4
corrections: §4 now notes `CREATE TEMP TABLE` succeeds (harmless,
`pg_temp`-scoped, but the capability table's "No" needs this caveat), and
a standing rule was added that any future `igaming`-owned view must be
created with `security_invoker = true` or it would silently defeat the
entire split (zero views exist today, so no live exposure). `docs/
governance/stage-4i-exit-register.md` §2 (`PLAT-TENANTREAD-1`) now names
the three actual scopeless multi-row readers (`internal/rg/
enumeration_sweep.go`, `internal/reconciliation/scheduler.go`,
`internal/bonus/schedulers.go`) instead of citing "migration 0077's own
rationale" vaguely, and the register's "exactly one production blocker"
summary line is qualified to state explicitly that it describes the
committed code/migration files, not the live state of every already-
running instance of this schema. Verdict on both documents:
`runtime-role-separation.md` approved as the `PLAT-ROLESPLIT-1` fix;
`stage-4i-exit-register.md` approved with the §2 corrections above
applied. No new mechanism proposed by either reviewer; no frozen
architecture reopened.

- **`PLAT-MIGDRIFT-1` — `cmd/migrate` has no live-schema-vs-file-content
  verification (new, this stage).** Classification: **E. OBSERVATION /
  TECHNICAL DEBT** — not a production blocker, because a genuinely
  first-time migration apply always uses the current, correct file
  content; it is a blind spot only for an environment that had an
  in-place-amended migration applied before the amendment landed (this
  codebase's own documented, permitted practice for uncommitted
  migrations — see `MKT-MIG76-1`). Owner: `devops`/`architect`. Why it
  matters: `schema_migrations` tracks applied versions by number only, no
  content checksum, so `migrate status` reports "clean" even when a
  table's actual live policy/constraint text no longer matches its
  migration's current source. Concrete consequence: exactly the drift
  this review round found and fixed in this session's own local
  database — silent, and only caught here because a reviewer happened to
  run the actual regression tests rather than trust `migrate status`.
  Recommended action: none required now; if `devops` ever wants to close
  it, the shape is a content-hash column on `schema_migrations` and a
  `migrate verify` command comparing stored hash to current file — not
  built this pass (new engineering work outside a documentation-only
  triage's scope). Exact trigger for reopening: any incident where a
  live environment's schema is suspected to not match its migration
  files, or the phase (if any) that hardens migration tooling generally.
  Related, not merged: the CI-runs-as-owner-role-only gap noted in
  `runtime-role-separation.md` §5 (the runtime-role split, once rolled
  out, is verified once by hand and not continuously in CI) — same
  underlying theme (verification gaps in the deploy/migration pipeline),
  different mechanism, tracked together here for visibility, not as one
  fix.

### Next stage recommended, not authorized

**Operator Back-Office MVP.** See this stage's completion report §13 for
the full dependency-chain rationale. Not started; no code written. Stage
6A was already named as a candidate option in `docs/active-stage.md`'s
prior-stage entries before this triage; this stage's own investigation
confirms it as the dependency-ready next capability (zero unresolved
HDR/policy dependency, zero frozen-architecture reopening, and the
repository currently has zero frontend/back-office code of any kind —
confirmed via `find`). **Corrected per the independent architect
review's line-level verification:** the initial framing ("UI over
already-built, already-tested backend APIs") overstated readiness for
every capability except withdrawal approval — KYC case-queue and RG-admin
handlers require an already-known player-account ID with no tenant-wide
pending-case query at any layer, bonus-campaign-admin has zero `GET`
routes (write-only, no approval queue), tenant/brand listing has no
`ListTenants` anywhere in `internal/identity`, player management is
hardcoded to `LIMIT 50` with no pagination/search and no reinstate-
after-suspend route, and platform-scoped audit rows (where migration
0077 now places licence-assignment and operating-market audit entries)
are unreachable through the sole existing audit-read route because it
filters to one tenant. Correctly scoped, the stage is "back-office
read/query API surface + UI" — new domain query functions and endpoints
for roughly half the named capabilities, each with its own tests, before
any UI screen can consume them.

## Stage 5 — Operator Back Office MVP

One large, single-authorization implementation stage (the directive's own
"one stage = one large product objective" instruction — no intermediate
approval gates). Frozen architecture (Stage 4I jurisdiction/licensing/
operating-market, KYC, RG, bonus, wallet/ledger, payments, sportsbook,
casino) was not reopened. Full narrative: `docs/progress.md`'s "Stage 5"
section and `docs/active-stage.md`'s current-stage entry.

| ID | Owner | Status | Files owned | Tests | Blockers | Integration |
|---|---|---|---|---|---|---|
| S5-01 | Orchestrator | Done | `internal/httpserver/pagination.go` (new, shared) | n/a | none | n/a |
| S5-02 | backend | Done | `internal/identity/{tenant,brand,player_account}.go`, `internal/httpserver/{admin_routes,routes}.go`, `backoffice_admin_integration_test.go` (new) | 20 new integration tests, all authorized/unauthorized/cross-tenant cases | none | Verified in combined whole-repo run |
| S5-03 | identity-compliance | Done | `internal/kyc/verification_service.go`, `internal/rg/rg.go`, `internal/httpserver/{kyc_admin_handlers,kyc_routes,rg_handlers}.go` + test files | new KYC/RG tests, cross-tenant isolation | Self-caught and self-fixed a real cross-tenant PII leak before shipping (see below) | Verified |
| S5-04 | bonus-engine | Done | `internal/bonus/{campaign,change_governance,grant}.go`, `internal/httpserver/bonus_admin_read_handlers.go` (new) + test file | 9 new integration tests | none | Verified |
| S5-05 | backend (2nd instance) | Done | `internal/withdrawal/withdrawal.go`, `internal/httpserver/{withdrawal_handlers,financial_routes}.go` + `stage5_admin_withdrawal_view_test.go` (new) | new admin withdrawal tests, no regression to frozen state machine | none | Verified |
| S5-06 | backoffice | Done | `backoffice/` (new npm project, ~40+ files) | 17 Vitest tests, `npm run build` clean | one deliberate partial (player-detail audit sub-section omitted — no matching filtered endpoint exists) | Verified (build+test independently re-run by Orchestrator) |
| S5-07 | Orchestrator | Done | `docs/api/openapi/platform-api.yaml` (72 paths, 30 schemas after this stage) | YAML-validated | none | n/a |
| S5-08 | architect, security, qa, ledger-finance (narrow) | Done | review only | n/a | See findings below | n/a |
| S5-09 | Orchestrator (fix round) | Done | `backoffice/src/{auth/AuthContext.tsx,api/{auth,client,withdrawals}.ts,lib/money.ts (new),features/withdrawals/*}`, `internal/httpserver/{withdrawal_handlers,rg_flow_integration_test}.go` | Full validation gate re-run clean after fixes | none | Verified |
| S5-10 | Orchestrator | Done | this registry, `docs/active-stage.md`, `docs/progress.md` | full validation gate | none | n/a — **stage explicitly STOPS here; Stage 6 NOT authorized** |

### Review findings and dispositions

**Architect** (PASS, no P0/P1/P2): 2 P3s recorded, not fixed —
`GET /v1/admin/rg/restrictions`'s two response shapes (bare array vs.
paginated envelope) is the one inconsistency with the otherwise-uniform
Stage 5 pagination convention; the new RG tenant-wide queue has no
`player_account_id` field, so it can't attribute a restriction to a
player (a real functional gap, left as a fast-follow per the reviewer's
own "your call" framing — not fixed this stage to avoid scope creep on a
completion-gate pass). 2 P4s (partial theming tokens; `risk_manager` role
sees an empty nav with no held Stage 5 permission) — cosmetic, deferred.

**QA** (PASS, no P0/P1/P2): full validation gate green (1492 Go tests
across 29 packages, `-race` clean on withdrawal/identity, 17 frontend
tests); coverage judgment confirmed every new endpoint has genuine
authorized/unauthorized/cross-tenant tests, and the reinstate mutation's
audit-row shape is asserted, not just its status code.

**Ledger-finance** (SIGN-OFF on the withdrawal UI, narrow scope): pure
pass-through confirmed, no client-side financial logic. **P2, fixed**:
amounts rendered in raw minor units with no exponent formatting at the
irreversible approval decision (e.g. "10000 EUR" for what is actually
EUR 100.00) — fixed by adding `decimal_exponent` to the two new
withdrawal admin response shapes and a shared frontend `formatMoney`
helper. P3 recorded, not fixed: `amount` is a JSON number, a latent
precision hazard once crypto-asset withdrawals (exponent 8/18) reach this
endpoint — `bonus.ts` already uses the safer string convention, adopt it
here when that day comes.

**Security** (the review round's two most consequential findings, both
**P2, both fixed**):
1. The SPA's "Sign out" only cleared local browser state and never called
   the existing `POST /v1/auth/logout` — a stolen refresh token remained
   valid server-side for its full 30-day TTL with no user-reachable way to
   revoke it. Fixed: `logout` now calls the server best-effort before
   clearing local state.
2. `TestRGAdminRestriction_TenantWideList_CrossTenantDenied` — the named
   regression guard for S5-03's own leak fix — was itself vacuous: it
   seeded a `scope:"tenant"` restriction the pre-existing RLS policy
   already filtered on its own, and the comparison tenant had no player at
   all, so the assertion passed unconditionally. Proven by mutation
   testing (weakening the join to a `LEFT JOIN` left the OLD test green).
   Fixed: rewritten to seed a genuinely platform-wide self-exclusion via
   the player's own self-exclusion endpoint, assert the fixture's
   `tenant_id IS NULL` directly, and give the comparison tenant its own
   real restriction so the isolation check is non-vacuous — re-verified
   by the same mutation test (weakening the join now correctly fails it).

One **P3, fixed** alongside the P2s: the SPA's session-bootstrap-on-reload
called the raw refresh function directly instead of the existing
single-flight `refreshSessionOnce()`, so React 18 StrictMode's deliberate
double-effect-invocation could present the same refresh token twice —
which the backend correctly treats as reuse and revokes the entire
session chain (`auth.session_reuse_detected`). Fixed by routing bootstrap
through the same dedup path `apiFetch`'s own 401 handling already uses.

**Deferred P3/P4s, recorded, not fixed** (per the directive's own
"record and continue" instruction — none block this stage or the next):
refresh token in `sessionStorage` (XSS-readable) combined with the 30-day
TTL — security flagged this **launch-blocking for the Back Office
specifically** (production needs httpOnly/Secure/SameSite cookies, a
backend change out of this stage's scope), tracked alongside
`PLAT-ROLESPLIT-1` as a second pre-production gate, not resolved here;
zero frontend test coverage for the 401-refresh-retry logic despite it
being the most security-sensitive module in the SPA; `tenants`/`brands`
read isolation is handler-only (`canActOnTenant` plus an explicit
tenant-ID comparison) with no RLS backstop — correct as written and
tested, but a future handler against these tables should not assume
isolation it doesn't structurally have; `q` search values are not
LIKE-escaped in `ListTenants`/`ListPlayerAccounts` (not injection —
bound parameters — a search-semantics quirk only).

### Explicitly NOT this stage's to build (confirmed absent)

The B2C player frontend, sportsbook, casino, partner console; any
tenant/brand CREATE forms, licence management, or dual-control approval
infrastructure in the Back Office; any change to
`internal/operatingmarket`, `internal/jurisdiction`, KYC/RG/bonus
business logic, or the withdrawal state machine/four-eyes controls; any
answer to `HDR-J-6/7/8/9`/`HDR-M-1/2`; any production resolver wiring;
any fix to `PLAT-ROLESPLIT-1` (remains documented-not-executed per
`docs/security/runtime-role-separation.md`).

## Stage 6 — B2C Player/Brand MVP + First Sportsbook Vertical Slice

One large, single-authorization implementation stage. Dependency
discovery confirmed every non-sportsbook domain needed (identity/auth,
wallet/ledger, RG, risk, KYC, audit, brand/tenant config, Back Office)
already existed and was reused; sportsbook had zero pre-existing code
beyond an architecture doc. Full narrative: `docs/progress.md`'s "Stage 6"
section and `docs/active-stage.md`'s current-stage entry.

| ID | Owner | Status | Files owned | Tests | Blockers | Integration |
|---|---|---|---|---|---|---|
| S6-01 | sportsbook | Done | `internal/sportsbook/` (new package), `internal/httpserver/{sportsbook_handlers,sportsbook_routes}.go` (new), `migrations/0078_sportsbook_foundation.{up,down}.sql` (new), `internal/ledger/ledger.go` (`TxSportsbookBet`), `internal/auth/permission.go` (`PermSportsbookBetRead`), `cmd/platform-api/main.go`, `internal/httpserver/server.go` | 10 orchestrator + 11 HTTP-flow integration tests | Disclosed and corrected the orchestrator's own wrong pointer to a non-compliant concurrency-test precedent, used the genuinely-compliant one instead | Verified in combined whole-repo run |
| S6-02 | backoffice | Done | `backoffice/src/{api/sportsbookAdmin.ts,features/sportsbook/{SportsbookBetsPage,status}.tsx,ts,auth/permissions.ts,layout/nav.ts,app/AppRoutes.tsx}` | `nav.test.ts` updated for the new grant | none | Verified (build+test re-run) |
| S6-03 | frontend | Done | `b2c/` (new npm project, ~entire app) | 13 Vitest tests at handoff (18 after the fix round below) | Two disclosed gaps: no dev-only way to complete a mock deposit without an out-of-band signed webhook call (needs architect/security sign-off if ever built — not built this stage); no player-facing asset-catalogue endpoint (client uses a small hardcoded list, server still authoritatively validates) | Verified (build+test independently re-run by Orchestrator) |
| S6-04 | Orchestrator | Done | `internal/httpserver/stage6_b2c_sportsbook_acceptance_test.go` (new) — the defining acceptance test | 1 test, 10 chained assertion points | none | Passed first run against a fresh 78-migration scratch DB; full `internal/httpserver` suite stayed green after adding it |
| S6-05 | architect, security, qa | Done | review only | n/a | See findings below | n/a |
| S6-06 | Orchestrator (fix round) | Done | `b2c/src/lib/{odds,money}.ts` + `odds.test.ts` (new), `b2c/src/features/betslip/BetSlip.test.tsx`, `internal/sportsbook/{bets,orchestrator}.go`, `internal/httpserver/sportsbook_handlers.go`, `migrations/0078_sportsbook_foundation.up.sql`, `internal/sportsbook/orchestrator_integration_test.go` (new regression test), `docs/api/openapi/platform-api.yaml` | Full validation gate re-run clean after fixes (31 Go packages, `b2c` 18 tests, `backoffice` 17 tests) | none | Verified |
| S6-07 | Orchestrator | Done | this registry, `docs/active-stage.md`, `docs/progress.md`, `docs/api/openapi/platform-api.yaml` (5 new paths, ~10 new schemas) | full validation gate | none | n/a — **stage explicitly STOPS here; Stage 7 NOT authorized** |

### Review findings and dispositions

**Architect** — one **P0, fixed**: `b2c/src/lib/odds.ts` computed decimal
odds as `1 + numerator/denominator` and potential-return as `stake ×
(denominator+numerator)/denominator`, but the server's `odds_numerator/
odds_denominator` ratio IS the decimal odds value directly, already
including the stake (`internal/sportsbook/orchestrator.go`'s
`computePotentialReturn`: `stake × numerator/denominator`, no "+1").
Every price and return shown in the B2C app — including the server's own
authoritative placed-bet fields re-rendered through the same formatter —
was overstated by the size of the stake itself (2.50 odds shown as 3.50).
Fixed in both `formatDecimalOdds` and `estimatePotentialReturnMinorUnits`,
the OpenAPI `Selection` schema description clarified, a new
`lib/odds.test.ts` (5 tests) added as a permanent regression guard, and
the one `BetSlip.test.tsx` fixture whose assertion baked in the old wrong
value corrected.

Two **P1s, fixed**: the sportsbook bet idempotency key was scoped only
by `(tenant_id, idempotency_key)` — unlike `deposit_intents`/
`withdrawal_requests`, both `(tenant_id, player_account_id,
idempotency_key)` — so one player supplying a key another player had
already used in the same tenant would silently receive the OTHER
player's bet back as `accepted:true`, their own stake never charged (a
cross-player financial-data disclosure; independently confirmed by
security). Fixed: migration's unique constraint, the lookup
(`findBetByIdempotencyKey`), and the ledger's own idempotency-key
namespace (now `player_account_id + ":" + idempotencyKey`, previously
the raw client string) all rescoped to include `player_account_id`; a
genuine key collision now surfaces a new `ErrBetIdempotencyKeyReused`
(mirroring `withdrawal`/`payments`'s identical precedent) instead of
silently returning the wrong bet; `insertBet` rewritten onto
`db.IdempotentInsert` (the same pattern `withdrawal.RequestWithdrawal`
uses) as the real DB-enforced concurrent-race backstop, not just the
pre-check; new regression test
`TestPlaceBet_SameIdempotencyKeyDifferentPlayersNeverCollide` proves two
players sharing a key get two independent bets with neither balance
affected by the other's stake.

Two **P2s, fixed**: the OpenAPI `EventSummary`/`EventDetail`/
`MarketDetail`/`Selection` status enums didn't match the real Go enums
(the Orchestrator's own transcription error when first writing the spec)
— corrected to `scheduled|live|finished|cancelled`,
`open|suspended|closed`, `active|suspended`. `b2c/src/lib/money.ts`'s
`toMinorUnits` silently guessed exponent 2 for an unrecognized asset code
on the OUTBOUND request path — changed to refuse (`null`, already
handled by every caller) rather than risk submitting a silently-wrong
amount.

One **P2, deferred** (needs dedicated `risk`/`ledger-finance` design
work, not a Stage 6 blocker): `internal/risk`'s cumulative-rule spec has
no `OperationSportsbookBet` entry yet, so no cumulative stake/velocity
cap constrains sportsbook bets beyond the per-request `risk.Evaluate`
call this stage already wires.

**P4s recorded, not fixed**: the public catalogue routes carry no
tenant/brand/jurisdiction gating — must be addressed before any second
tenant or jurisdiction (doc 09 §5's own open decision); the B2C app's
one-build-per-brand env-var model needs runtime host→brand resolution
before true multi-brand B2C; `internal/casino`/`internal/withdrawal`'s
own concurrency tests still use the older bare-`WaitGroup` technique
instead of this stage's (and `internal/operatingmarket`/
`internal/jurisdiction`'s) `pg_stat_activity` convention (QA finding).

**Security** — the same cross-player idempotency-key **P1**,
independently discovered and fixed as above. Everything else verified
sound: cross-player bet forgery structurally impossible (player identity
always resolved server-side, never from a request field); the
cross-tenant Back Office visibility test is genuinely non-vacuous (two
real bets in two tenants, `total==1` asserted, correct player attributed);
public catalogue routes are safe (no PII, no tenant scoping, read-only,
`WithoutTenant` fails any RLS table closed); RG/risk denials and
unrecognized risk outcomes fail closed; the audit trail is written
in-transaction for every outcome; the `DecimalExponent` addition to the
admin response introduces no new exposure. One **P3, fixed**: a stale
test comment claiming support was "deliberately NOT" granted
`sportsbook_bet:read` when the permission table does grant it to
`RoleSupport` — corrected.

**QA** — clean sign-off, no P0/P1. Independently verified the defining
acceptance test's assertions are genuinely non-vacuous at every hop;
confirmed idempotency is DB-enforced (not check-then-insert) and
concurrency uses the mandated deterministic technique with exact
balance/row-count assertions; confirmed the `backoffice/nav.test.ts`
change (finance's nav items growing to include Sportsbook) is a
legitimate reflection of a real backend permission grant, not a weakened
assertion.

### Explicitly NOT this stage's to build (confirmed absent)

A full casino product, B2B partner console, retail surface, or every
future brand; full production PSP integration beyond the existing mock;
every KYC provider, every sportsbook feature (settlement/void/cashout/
accumulators/live odds), every bonus type, every RG feature; any answer
to `HDR-J-6/7/8/9`/`HDR-M-1/2`; any jurisdiction check on a sportsbook
bet; bonus-funded sportsbook stakes (remain platform-wide blocked per
`docs/decisions/0038` §9); any fix to `PLAT-ROLESPLIT-1` (remains
documented-not-executed per `docs/security/runtime-role-separation.md`).

## Stage 6.1 — B2C/Sportsbook Hardening & Architectural Closure Gate

A focused hardening/closure pass over Stage 6 (commit
`be6042d1189a365bd9026577422dea69df8bbec4`), not a feature stage. Full
narrative: `docs/progress.md`'s "Stage 6.1" section,
`docs/active-stage.md`'s current-stage entry, and
`docs/decisions/0047-sportsbook-catalogue-jurisdiction-boundary-and-
cumulative-risk-deferral.md`.

| ID | Owner | Status | Files owned | Tests | Blockers | Integration |
|---|---|---|---|---|---|---|
| S61-01 | Orchestrator | Done | baseline re-verification only | full 31-package suite, fresh migration, both frontends | none | Verified |
| S61-02 | architect, security, ledger-finance, architect (DB/RLS), qa | Done | review only | n/a | See findings below | n/a |
| S61-03 | Orchestrator (fix round) | Done | `b2c/src/features/betslip/{BetSlipContext,BetSlip}.tsx` + `BetSlip.test.tsx`, `b2c/src/features/deposit/DepositPage.tsx`, `b2c/src/lib/idempotencyKey.ts` | new idempotency-retry regression test | none | Verified |
| S61-04 | Orchestrator (fix round) | Done | `internal/sportsbook/{orchestrator,bets}.go`, `internal/sportsbook/orchestrator_integration_test.go` (4 new tests), `internal/risk/cumulative.go` (comment) | 4 new sportsbook tests, full package re-run | none | Verified |
| S61-05 | Orchestrator (fix round) | Done | `migrations/0078_sportsbook_foundation.up.sql`, `internal/httpserver/sportsbook_handlers.go`, `internal/httpserver/sportsbook_flow_integration_test.go` | fresh-migration re-verification, 2 new assertions in existing test | none | Verified |
| S61-06 | Orchestrator | Done | `docs/decisions/0047-*.md` (new) | n/a | none | n/a |
| S61-07 | Orchestrator | Done | this registry, `docs/active-stage.md`, `docs/progress.md`, `docs/api/openapi/platform-api.yaml` (AdminBet.brand_id) | full validation gate | none | n/a — **stage explicitly STOPS here; Stage 7 NOT authorized** |

### Review findings and dispositions

**Architect**: no P0/P1. Verified the PlaceBet financial path is sound
(single authoritative path, no float, atomic, correct rollback). Found
and I fixed 3 P2/P3 factual corrections in ADR 0047's first draft: (1)
casino's `jurisdiction_blocklist` IS admin-configurable today via
`PUT /v1/admin/casino/games` (my draft had wrongly claimed no
configuration path exists — this sharpens the asymmetry, doesn't change
the SAFE DEFERMENT disposition); (2) `jurisdiction.Resolve` is also
called by `internal/bonus/eligibility.go`, not "only casino"; (3)
sportsbook's own `risk.RiskRequest` doesn't set `JurisdictionCode`
(fail-closed via `ErrMissingJurisdiction` if ever relevant — safe, but
worth documenting). One P2 recorded, not fixed: the bet stores only
`selection_id`, and `SyncCatalogue`'s upsert-on-`external_ref` could in
principle re-parent a selection under a different market/event for a
misbehaving future real provider, silently changing what an OPEN bet's
`selection_id` points to. Not exploitable with the mock provider;
recorded in ADR 0047 as a precondition for any live-odds/real-provider
work, not fixed via migration this stage (architect's own recommendation
— additive if/when actually needed, not speculative now).

**Security**: no P0. One **P2, fixed**: the B2C bet slip and deposit form
minted a fresh idempotency key per submit click rather than once per
attempt — the P0-equivalent fix described above (a duplicate-stake/
duplicate-deposit exposure security explicitly flagged as needing
resolution "before any real-money B2C traffic"). Three **P3s**, two
fixed: unknown `asset_code` returned 500 not 400 on
`POST /v1/me/sportsbook/bets` — fixed to match casino's
`db.IsForeignKeyViolation` → 400 pattern; a test comment wrongly
described `rg.EvaluateEligibility`'s concurrency-safety mechanism as "a
row lock on player_accounts" when it is actually a
`pg_advisory_xact_lock` keyed on `person_id` — fixed. One P3 recorded,
not fixed (pre-existing class, not sportsbook-specific): a
`cumulative_amount` risk rule for an operation with a live `risk.Evaluate`
call site but no `operationCumulativeSpecs` entry is accepted at write
time but fails every subsequent request at evaluation time — fails safe
(no money at risk) but is a self-inflicted outage lever; a real fix
belongs in `risk.CreateRule` generally, out of this stage's scope. One P3
also recorded: no explicit `PrincipalType==player` assertion on
`/v1/me/sportsbook/bets` (safe today, matches casino's identical shape).
One P4 recorded: `getSelectionWithContext`'s read is not `FOR SHARE`
(inert until a live-odds feed exists). Extensive adversarial verification
(A-K per the directive) found every scenario genuinely safe by
construction — see the security review's own report for the full
evidence trail (advisory-lock serialization, composite-FK structural
impossibility of cross-tenant/cross-player misattribution, RLS fail-closed
on malformed settings, etc).

**Ledger-finance**: SOUND, no P0/P1. Verified the full financial chain
independently: SUM(DEBITS)==SUM(CREDITS) holds (exactly 2 legs, same
variable), zero floating-point (grep-verified across the whole path),
asset exponent correctly looked up (never assumed), ledger amount and
`sportsbook_bets.stake_amount` cannot diverge (same variable, same
transaction), atomicity holds end-to-end. **Found the review round's
most consequential result, a real P2**: my own Stage 6.1 ledger-key
strengthening (adding the transaction-type prefix) introduced a latent
rolling-deploy hazard — two application versions deriving different
ledger keys for the same (player, idempotency_key) pair could each post
a genuinely new (and, for the loser, orphaned) ledger transaction before
the `sportsbook_bets` unique constraint resolved which bet row wins. Not
live (Stage 6 is unreleased), but the guard is 3 lines. **Fixed**: a
cross-check after `insertBet`'s conflict-resolution path aborts the whole
transaction if the resolved bet's `ledger_transaction_id` doesn't match
what this call itself posted. Two P3s recorded, not fixed (test
thoroughness, not correctness): the ledger-balance test doesn't assert
exact leg count/accounts (only debit-total==credit-total); the
concurrency test doesn't assert ledger-transaction-count/locked-balance
alongside cash/bet-row-count. One P4 recorded: the player-facing
`betResponse` omits `decimal_exponent` (only the admin response has it) —
same money-display rationale, not fixed (player-facing display precision
is a real but lower-priority gap than Back Office's approval-decision
context).

**Database/RLS** (architect, DB/RLS-scoped pass): no P0/P1. Confirmed RLS
fail-closed in both directions (forged/empty `app.player_account_id`,
NULL/malformed `app.tenant_id`), the two-policy shape textually identical
to `casino_launch_sessions`' established precedent, `db.IdempotentInsert`
correctly SAVEPOINT-isolated, append-only discipline holds (no DELETE
anywhere), lock ordering consistent with casino/withdrawal (no new
deadlock edge), and a CHECK constraint for
`potential_return = stake*num/den` correctly rejected as inexpressible at
the DB level (depends on an `assets` lookup and a rounding rule — money's
own `RoundToMinorUnits` is the right layer). **One real P2, fixed**: the
original three composite FKs left `brand_id` pinned only to "some brand
in this tenant," not the player's own specific brand — not reachable via
the application (which always derives `brand_id` correctly), but a real
DB-level gap. Fixed via a composite `(player_account_id, tenant_id,
brand_id)` FK against `player_accounts`, reusing the exact `UNIQUE (id,
tenant_id, brand_id)` key `wallets` and `bonus_grants` already use for
the identical pinning — noted `casino_launch_sessions` (migration 0035)
has the SAME gap, recorded as a backlog item (not fixed — "do not build
casino" this stage). One **P3, fixed**: `asset_code` had no FK to
`assets(code)` (contrast `wallets`, which does) — added. Two P3s
recorded, not fixed: audit-attribution completeness (`brand_id` added to
the accepted-bet audit record for consistency with RG/risk denial
records, which already included it — the insufficient-funds record still
targets the wallet with no direct player/brand attribution, a smaller
residual gap); unindexed `ledger_transaction_id` (not a real problem —
nothing queries by it today; if ever indexed, should be `UNIQUE`).

**QA**: no P0. One **P1, fixed**: two of three sibling rejection
branches (event-finished had a test; market-not-open and
selection-not-active did not) — a refactor deleting either would have
passed the full suite silently. Added
`TestPlaceBet_MarketNotOpenRejected`/`TestPlaceBet_SelectionNotActiveRejected`,
plus a `RejectionCode` assertion on the existing event-finished test (the
three branches share one `RejectionCategory`, differing only by code).
Two **P2s, fixed**: `brand_id` was never asserted anywhere and had no
HTTP field to check it against — added to `adminBetResponse` and
asserted in the cross-tenant test; `decimal_exponent` was populated but
never asserted against a known-correct value (EUR=2) — added. Confirmed
the acceptance test and other mutation-style scenarios (tenant/player
swap, idempotency-key reuse, insufficient-funds/RG/risk-denial residue,
authorization removal) are all genuinely non-vacuous, backed by
RLS/unique-constraint/exact-balance assertions, not just status-code
checks. One P4 recorded: no test exists for a mid-transaction failure
injection (consistent with casino/withdrawal, which also have none) —
reasonably covered structurally by Postgres transaction guarantees plus
the SAVEPOINT-based conflict handling ledger-finance reviewed separately.

### Four deferred-item dispositions (from Stage 6's own registry)

1. **Cumulative sportsbook risk rule** — SAFE DEFERMENT (ADR 0047 §4; the
   measurement shape is already specified in ADR 0038 §13, wiring it
   later is one additive map entry).
2. **Catalogue tenant/jurisdiction gating** — SAFE DEFERMENT, required
   before a second jurisdiction or the first B2B tenant, NOT required
   before Stage 7 (ADR 0047 §2-3; inert today since every jurisdiction
   blocklist on the platform is empty, but casino's mechanism is
   admin-configurable today while sportsbook's isn't).
3. **Casino/withdrawal concurrency-test convention backfill** — SAFE
   DEFERMENT, a backlog item with no financial-integrity impact (those
   tests already pass correctly via an older, still-valid technique).
4. **B2C build-time brand model before true multi-brand** — SAFE
   DEFERMENT, required before a second B2C brand goes live (confirmed by
   architect review: `brand_slug` only selects the login/register target
   server-side; every authenticated call derives tenant/brand purely from
   the verified JWT).

### Explicitly NOT this stage's to build (confirmed absent)

Casino, settlement, void, cashout, real sportsbook provider integration,
production country/jurisdiction approvals, any answer to
`HDR-J-6/7/8/9`, any new human decision, any cumulative risk calculation
for sportsbook, any catalogue jurisdiction-blocklist mechanism (recorded
as a future precondition only, per ADR 0047 — not built this stage).

## Stage 7 — B2C Casino Player Experience + Casino Vertical Slice

First complete B2C casino vertical slice using the existing casino
architecture (built Stage 4A onward and hardened through 4G/4H/4I). Full
narrative: `docs/progress.md`'s "Stage 7" section, `docs/active-stage.md`'s
current-stage entry, and
`docs/decisions/0048-casino-play-simulation-trust-boundary.md`.

| ID | Owner | Status | Files owned | Tests | Blockers | Integration |
|---|---|---|---|---|---|---|
| S7-01 | Orchestrator | Done | baseline re-verification only | full 32-package suite, fresh migration | none | Verified |
| S7-02 | Orchestrator | Done | `migrations/0079_casino_launch_session_brand_pinning.{up,down}.sql`, `internal/casino/launch_session_brand_pinning_test.go` (6 new tests) | up/down/up round-trip on scratch DB, 6 adversarial tests, full `internal/casino` re-run | none | Verified |
| S7-03 | Orchestrator | Done | `internal/casino/history.go` (new), `internal/httpserver/casino_history_handlers.go` (new), `internal/auth/permission.go` (`PermCasinoTransactionRead`), `internal/httpserver/casino_routes.go` | covered by S7-05's HTTP tests | none | Verified |
| S7-04 | Orchestrator | Done | `internal/httpserver/casino_play_handlers.go` (new) | covered by S7-05's HTTP tests | none | Verified |
| S7-05 | frontend (subagent) | Done | `b2c/src/api/casino.ts`, `b2c/src/features/casino/*` (new), `b2c/src/app/AppRoutes.tsx`, `b2c/src/layout/nav.ts`, `b2c/src/test/handlers.ts` | 12 new Vitest+RTL+MSW tests, 31/31 total green, `tsc -b`/build clean | none | Verified (independently re-run) |
| S7-06 | backoffice (subagent) | Done | `backoffice/src/api/casinoAdmin.ts`, `backoffice/src/features/casino/*` (new), `backoffice/src/auth/permissions.ts`, `backoffice/src/layout/nav.ts`, `backoffice/src/app/AppRoutes.tsx` | 2 new tests, 19/19 total green, `tsc -b`/build clean | none | Verified (independently re-run) |
| S7-07 | architect, security, ledger-finance, architect (DB/RLS), qa | Done | review only | n/a | See findings below | n/a |
| S7-08 | Orchestrator (fix round) | Done | `internal/httpserver/casino_play_handlers.go` (rewritten), `internal/casino/history.go` (`TransactionBelongsToRound`, multi-leg accumulation, pagination tiebreaker), `internal/httpserver/server.go` (`CasinoPlaySimulationEnabled`), `internal/httpserver/casino_routes.go` (route gate), `cmd/platform-api/main.go` (wiring) | 5 new adversarial regression tests + 1 idempotent-retry test + 1 cross-player-isolation test, full suite re-run | none | Verified |
| S7-09 | Orchestrator | Done | `docs/decisions/0048-*.md` (new) | n/a | none | n/a |
| S7-10 | Orchestrator | Done | `internal/httpserver/stage7_b2c_casino_acceptance_test.go` (new) | 11-step acceptance test, exact financial assertions; Stage 6 sportsbook acceptance test re-run | none | Verified |
| S7-11 | Orchestrator | Done | this registry, `docs/active-stage.md`, `docs/progress.md`, `docs/api/openapi/platform-api.yaml` | full validation gate | none | n/a — **stage explicitly STOPS here; Stage 8 NOT authorized** |

### Review findings and dispositions

**The headline finding — one P0, independently confirmed FOUR times**
(architect, security, ledger-finance, database/RLS, each reproducing it
live over HTTP against a real database, none aware of the others' work
until after independently finding the same defect): the play-simulation
rollback endpoint (`POST /v1/me/casino/sessions/{id}/rollback`) let an
authenticated player name ANY `original_provider_tx_id` as the reversal
target. `postRollback`'s own lookup
(`internal/casino/orchestrator.go`) is scoped to `(tenant_id, provider_id,
provider_tx_id)` only — correct for a real, independently-signed provider
webhook, where that triple is fully provider-attested, and never designed
to receive a player-supplied reference, since the mock adapter's signature
on this endpoint is self-issued by the platform on the player's own
behalf and authenticates nothing about which transaction they name.
Confirmed exploitable, live, for: cross-player fund reversal (player B,
using B's own genuinely-owned session, reverses player A's win or bet,
crediting/debiting A's wallet); cross-round reversal (the same player
using one round to reverse a transaction from an unrelated round of their
own); and a tenant-wide, unrecoverable denial-of-service (a rollback
naming a never-issued reference writes a permanent ledger tombstone,
later making a genuine bet/win that happens to mint that exact reference
fail outright, with no recovery possible since the ledger is append-only).
**Fixed** in `casino_play_handlers.go` via
`requireRollbackTargetOwnedByRound`/`casino.TransactionBelongsToRound`
(the named transaction must belong to the calling session's own round —
same `correlation_id` — and wallet, checked before the payload is ever
signed, returning the same enumeration-resistant 404 as a session
mismatch). Recorded as ADR 0048.

**Compounding P0-adjacent findings, all fixed in the same round**: the
three play-simulation routes were registered unconditionally in the
production binary with no deployment gate (a mock provider is by
definition one that says yes to everything, so "the provider type is
restricted" was not itself a safety property) — fixed with
`Deps.CasinoPlaySimulationEnabled`, set only outside `production`; the
rollback handler was missing the `ModeReal`/session-active/expiry guard
wager and win both already had — fixed; a player could declare an
unbounded `win_amount` with no real game outcome behind it — fixed with
`maxCasinoPlaySimulationAmount`.

**Ledger-finance's own independent P1 judgment call** (explicitly asked
for, not just flagged): minting a fresh `provider_tx_id` per wager/win
call meant a client retry after a lost response was, by construction, a
SECOND financial transaction — ledger-finance rated this **P1, not P0**
(each posting stays individually well-formed and balanced; the failure is
a duplicate instruction, not ledger corruption, and is correctable via
the existing rollback path) but explicitly **not acceptable as a disclosed
mock-only limitation**, since the platform already decided this exact
question one stage ago for `POST /v1/me/sportsbook/bets`'s required
`idempotency_key` contract (Stage 6.1), and this file is now the repo's
reference implementation of "a request-scoped caller drives
`ReceiveCallback`" that a future specialist would copy. **Fixed**: wager/win
now require a client `idempotency_key`, deriving a deterministic
`provider_tx_id` from it (rollback did not need this — a retry naming the
same original is already safe via `postRollback`'s own
`ErrAlreadyRolledBack` check, independently confirmed by reading that
code path).

**Ledger-finance also found, fixed**: `internal/casino/history.go`'s
round summary overwrote rather than accumulated repeated bet/win/rollback
legs, silently under-reporting a round's true stake/payout the moment a
player interacts with a session more than once (the normal way the new
session screen is used, not a corner case) — fixed to sum each leg type.

**Database/RLS review, fixed**: `listSessionsPage`'s `ORDER BY
created_at DESC` had no tiebreaker, so pagination was non-deterministic
under same-transaction timestamp ties — added `, id DESC`. Also
recommended (not yet added, recorded as tech debt): a
`(tenant_id, created_at DESC)` index on `casino_launch_sessions` before
either round-visibility view carries real volume (matches the sportsbook
precedent's own identical, already-disclosed gap).

**Security review, fixed**: added a player-attributed
`casino_play_simulation.{wager,win,rollback}` audit record alongside the
existing system-attributed `postBet`/`postWin`/`postRollback` records
(which remain correct, unchanged, for their real intended caller) — a
player-triggered financial mutation was otherwise indistinguishable in
`audit_log` from a genuine provider callback, which would make an abuse
investigation of this simulation seam impossible.

**Architect review, disclosed as a known limitation, not fixed this
stage**: the round read model (session-anchored, `roundCorrelationID`
re-derivation, deliberately never a new "rounds" table — confirmed
consistent with `docs/architecture/08-casino-integration-architecture.md`
§7's own frozen design) is accurate only for rounds whose bet/win/rollback
callbacks all shared the session's own id as `RoundID` — the convention
this stage's play-simulation endpoints introduce. A real provider posting
via the public webhook with its own, different round id would render
blank in this view. Recorded in ADR 0048's "known residual limitation"
section; the clean fix (persisting the provider-declared round id) is
deferred to whenever a real provider integration is actually scoped, not
spent effort on speculatively now.

**QA review**: independently reproduced the same P0 (a fifth confirmation,
via its own from-scratch test before the fix landed), confirmed none of
the pre-existing three new backend test files were vacuous under direct
mutation (6 separate mutations tried, each caused the expected, correctly-
targeted test failure), and confirmed the frontend's decline-vs-error
rendering distinction is genuinely tested (mutation-verified). Its
mid-review snapshot caught the fix-in-flight test/contract mismatch this
same round resolved (new tests added, existing tests updated for the new
`idempotency_key`/`CasinoPlaySimulationEnabled` contract, full suite
re-confirmed green after).

### Dispositions carried over from prior stages (unaffected by this one)

None of Stage 6.1's four disposed items (cumulative sportsbook risk rule,
catalogue tenant/jurisdiction gating, casino/withdrawal concurrency-test
convention backfill, B2C build-time brand model) were reopened or affected
by Stage 7's casino work.

### Explicitly NOT this stage's to build (confirmed absent)

Real casino provider integration, sportsbook settlement/cashout, real PSP
integration, production country/jurisdiction approvals, any answer to
`HDR-J-6/7/8/9`, B2B/partner console, retail, and any
jurisdiction/wallet-ledger/identity/RG redesign.

## Stage 8 — Provider Integration Readiness Without External Contracts

Originally scoped as external Dummy Sportsbook/Dummy Casino API
integration; re-scoped by the platform owner, once both APIs proved
undiscoverable from this environment, to provider-integration readiness
without any external network call. Full narrative:
`docs/decisions/0080-provider-integration-readiness-without-external-
contracts.md`, `docs/active-stage.md`'s Stage 8 section,
`docs/progress.md`'s Stage 8 section.

| ID | Owner | Status | Files owned | Tests | Blockers | Integration |
|---|---|---|---|---|---|---|
| S8-01 | Orchestrator | Done | reconnaissance only; `docs/decisions/0080-*.md` (new) | n/a | none | n/a |
| S8-02 | casino (subagent) | Done | `migrations/0080_casino_provider_rounds.{up,down}.sql`, `internal/casino/rounds.go` (new), `internal/casino/orchestrator.go` (`postBet` binding), `internal/casino/types.go` (sentinel errors) | 4 initial adversarial tests, 84/84 `internal/casino` suite green | none | Verified |
| S8-03 | sportsbook (subagent) | Done | `migrations/0081_sportsbook_bets_provider_reference.{up,down}.sql`, `internal/sportsbook/types.go`, `internal/sportsbook/bets.go`, `internal/sportsbook/provider_reference_integration_test.go` (new) | 17/17 `internal/sportsbook` suite green | none | Verified |
| S8-04 | integrations (subagent) | Done | `internal/providers/httpclient/{client,errors}.go` (new), `internal/providers/httpclient/conformance/conformance.go` (new), `internal/providers/config.go` (new) | 24 new tests, race-clean | none | Verified |
| S8-05 | ledger-finance (subagent) | Done | `internal/casino/failure_mode_matrix_integration_test.go` (new), `internal/sportsbook/failure_mode_matrix_integration_test.go` (new) | 15 new failure-mode tests (A/B/C/D/E/F/H/I/JK matrix items) | none | Verified |
| S8-06 | backoffice (subagent) | Done | `internal/casino/rounds.go` (`LookupProviderRoundIDsBySession`), `internal/httpserver/casino_history_handlers.go`, `internal/httpserver/sportsbook_handlers.go`, `backoffice/src/features/casino/CasinoRoundsPage.tsx`, `backoffice/src/features/sportsbook/SportsbookBetsPage.tsx`, `docs/api/openapi/platform-api.yaml` | 5 new backend tests, 23/23 frontend tests, `tsc -b`/build clean | none | Verified |
| S8-07 | architect, security, qa | Done | review only | n/a | See findings below | n/a |
| S8-08 | casino (subagent, fix round) | Done | `internal/casino/rounds.go`, `internal/casino/orchestrator.go`, `migrations/0080_*.sql` (immutability trigger added), `internal/httpserver/casino_handlers.go`, `internal/httpserver/casino_play_handlers.go` | 2 new HTTP tests + 2 new domain tests, full `internal/casino`+`internal/httpserver` re-run | none | Verified |
| S8-09 | integrations (subagent, fix round) | Done | `internal/providers/httpclient/{client,errors}.go` | 5 new tests (redirect-credential, context-cancellation, Sent classification ×3), race-clean | none | Verified |
| S8-10 | sportsbook (subagent, fix round) | Done | rename `provider_bet_ref`→`provider_bet_reference` across migration 0081, `internal/sportsbook/*`, `internal/httpserver/sportsbook_handlers.go`, OpenAPI, `backoffice/src/api/sportsbookAdmin.ts`, `SportsbookBetsPage.tsx` | full round-trip + suite re-run, frontend build clean | none | Verified |
| S8-11 | Orchestrator | Done | `docs/decisions/0080-*.md` (corrections), `docs/architecture/08-*.md` (§13a new), `docs/architecture/09-*.md` (open question #0), this registry, `docs/active-stage.md`, `docs/progress.md` | full validation gate re-run (32 packages, 1273 tests, race-clean) | none | n/a — **stage explicitly STOPS here; Stage 9 NOT authorized** |

### Review findings and dispositions

**Three P1s, two independently confirmed by more than one reviewer:**

1. **Credential-exfiltration path in `internal/providers/httpclient`**
   (security AND qa, independently reproduced via live redirect tests):
   the client had no `CheckRedirect` policy, so Go's default redirect
   behavior forwarded any caller-configured auth header (deliberately
   arbitrary, e.g. a vendor `X-API-Key`) to a redirect target — a
   compromised or malicious provider could exfiltrate the credential.
   Fixed: `New()` now refuses to auto-follow any redirect
   (`http.ErrUseLastResponse`); permanent regression test added.
2. **Unmapped `ErrProviderRoundOwnershipConflict`** (architect finding F1,
   security finding P2-2): a cross-player/cross-brand round-id collision
   — the exact integrity signal `casino_provider_rounds` exists to catch
   — fell through to a generic 500 with no alert, which a well-behaved
   provider reads as "retry forever." Fixed: explicit branch at both
   `casino_handlers.go` (public webhook) and `casino_play_handlers.go`
   (play-simulation), logging an integrity alert and returning a generic
   409 (no round id or identity echoed, to avoid an enumeration oracle).
3. **Round binding committed on a declined bet** (architect finding F2,
   security finding P3-7): `BindProviderRound` was called before the RG/
   Risk/insufficient-funds checks, all three of which return
   `(OutcomeDeclined, nil)` — a committed outcome — so a blocked bet still
   left a durable binding claiming that round id. Fixed: bind moved to
   immediately before `ledger.Post`.

**P2s fixed while still cheap** (all uncommitted, so free to change):
the ownership-conflict predicate originally required `launch_session_id`
to match, rejecting a legitimate same-player/same-brand round
continuation across two launch sessions (e.g. a free-spins round
outliving a session timeout) — relaxed to `player_account_id`+`brand_id`
only; `casino_provider_rounds` had no DB-level immutability trigger
against this repo's own repeated precedent — added, modeled on migration
0036's `casino_launch_sessions` trigger; the HTTP client's retry loop
burned its entire retry budget instantly on a cancelled parent context
and misclassified it as a provider-health signal — fixed with an
up-front `ctx.Err()` check and a `CallerCanceled` field; the error
taxonomy didn't distinguish "definitely never reached the provider" from
"possibly reached it" — added a `Sent bool` field to the relevant error
types; `sportsbook_bets.provider_bet_ref` didn't match doc 09's own
pre-existing canonical name `provider_bet_reference` — renamed while the
column was still unwritten by any code path (free now, would not have
been once a real adapter started writing to it).

**Documentation-accuracy corrections** (architect finding F4/F5, security
finding P3-5/P3-6): ADR 0080 Decision 1 originally claimed `postWin`/
`postRollback` verify a callback's `RoundID` against the existing
binding — no such code exists (`postWin`/`postRollback` are unchanged,
resolving accounts entirely from ledger truth, a stronger anchor) — and
Decision 5 claimed `casino_provider_rounds` is "never populated outside
tests," which is false (Stage 7's play-simulation seam populates it on
every real-mode wager). Both corrected in place rather than left as
inaccurate decision-record text, per CLAUDE.md's "no fake completion"
rule. `docs/architecture/08-casino-integration-architecture.md` gained a
new §13a describing the table (it was previously undocumented outside
the ADR); `docs/architecture/09-sportsbook-architecture.md` gained an
explicit "Open questions" entry recording the deferred provider-
acceptance-vs-financial-posting ordering decision durably (previously
recorded only in `docs/integrations/dummy-sportsbook.md`, which is
expected to be rewritten in full once real documentation exists).

**P3/P4 items documented, not fixed** (per Stage 8's own "fix P0/P1
always, P2 only when directly relevant, document P3/P4" rule): a
diagnosability gap where a late-arriving original after a tombstone
surfaces an opaque wrapped error rather than a named sentinel (found by
ledger-finance, requires a shared `ledger.Post`/`db.IdempotentInsert`
change, not incidental to this stage); `MockCasinoProvider` has no hook
to simulate a callback-path provider failure and `MockSportsbookProvider`
has no failure-injection mechanism at all; a redundant `correlationID`
parameter on `BindProviderRound` (removed anyway, it was cheap);
`ProviderRound.ProviderSessionID`'s nullability convention diverged from
`internal/sportsbook`'s `*string` idiom (fixed anyway, cheap); an RLS
policy pattern (`player_self_scope`) that relies on `player_account_id`'s
global uniqueness rather than stating a `tenant_id` predicate explicitly
— pre-existing platform-wide convention, not Stage 8's to redesign.

**Governance-record backfill (Stage 9 architect review, §26 item 24):**
one Stage 8 security-review P3 finding was missing from this table
entirely — it existed only in the Stage 8 completion report delivered to
the human, not in any committed document. Recorded here for completeness:
`RejectedError.Body`/`.Header` and `UnavailableError.Body`/`.Header`
(`internal/providers/httpclient/errors.go`) can carry a provider-echoed
value verbatim (a vendor debug endpoint reflecting the request back, or a
gateway error page). Stage 9's `integrations` agent closed the
platform's-own-credential half of this with `redactCredential`/
`redactCredentialHeader` scrubbing; the orchestrator additionally added
`GoString()` to `TimeoutError`/`UnavailableError`/`RejectedError`
(mirroring `internal/jurisdiction`'s established `SEC-4I-C-02` remedy) so
`fmt.Sprintf("%#v", err)` cannot bypass that scrubbing — see Stage 9's own
section below for the fix. Residual, disclosed and accepted: a provider
echoing a value the platform does not itself recognize (e.g. a player PAN
fragment or its own session token) is still stored unredacted in `Body`;
no production call site consumes these errors today, so this remains P3.

### Explicitly NOT this stage's to build (confirmed absent)

External Dummy Sportsbook/Dummy Casino API integration (undiscoverable,
per the platform owner's own instruction not to invent either contract),
real commercial provider integration, B2B, retail, full reconciliation/
settlement platform, any jurisdiction human decision or country approval,
and any wallet/ledger/identity/RG/risk redesign.

## Stage 9 — Production Readiness, Security, Resilience & Launch Hardening

Directive: move from an architecturally-proven B2C MVP toward a production
launch candidate across 28 named sections (database/role hardening,
migration safety, deployment, config/secrets, auth/session, RBAC,
tenant/brand security, financial-integrity adversarial concurrency,
withdrawal four-eyes, payment/provider readiness, RG/KYC, jurisdiction,
observability, health/readiness, backup/DR, retention/audit,
performance/load, API security, frontend readiness, CI/CD, runbooks,
compliance-evidence-only, technical-debt disposition). Explicitly out of
scope: B2B/partner/retail, undocumented external provider integration,
invented regulatory requirements, and any of the unresolved jurisdiction
human decisions (HDR-J-6/J-7/J-8/J-9). Full narrative in the Stage 9 final
report delivered to the human; this table is the durable record.

Mid-stage, 6 of 9 initially-dispatched specialist agents failed to a
weekly API rate-limit error. The Orchestrator diagnosed actual repo damage
(one build break, fixed directly), stopped and asked the human how to
proceed rather than guessing, and resumed all 6 workstreams after an
empirical probe confirmed the human's plan upgrade had resolved the
block — each resumed agent was instructed to inspect its own partial work
first rather than restart or duplicate.

| ID | Owner | Status | Files owned | Tests | Blockers | Integration |
|---|---|---|---|---|---|---|
| S9-01 | Orchestrator | Done | baseline re-verification only | full pre-existing suite re-run | none | n/a |
| S9-02 | devops (subagent) | Done | `deploy/init-app-role.sql`, `Makefile` (role/runtime-test targets), `.github/workflows/ci.yml` (runtime role provisioning, `-race`, new `frontend` CI job), `internal/db/production_safety.go` (new), `cmd/platform-api/main.go` (wiring), `docs/security/runtime-role-separation.md` (§9), `docs/architecture/38-deployment-architecture.md` (new), `docs/runbooks/production-configuration-checklist.md` (new) | `internal/db/production_safety_test.go` (4 branches), `internal/db/runtime_role_separation_test.go` (12 adversarial probes + 2 supplementary — independently re-run by Orchestrator against `stage9_final`, all pass) | PLAT-ROLESPLIT-1's actual production rollout still needs a human with real prod credentials — mechanical/in-repo half only | Verified |
| S9-03 | security (subagent) | Done | `internal/httpserver/ratelimit.go` (new), `internal/httpserver/server.go` (`Deps.AuthRateLimitPerMinute`/`authLimiter`), `internal/httpserver/{bonus,casino,credential,financial,kyc,rg,sportsbook}_routes.go`, `internal/auth/middleware.go` (`RequirePlayerPrincipal`), `docs/security/security-architecture.md` (Stage 9 section) | `ratelimit_test.go`, `ratelimit_routes_test.go`, `require_player_principal_test.go`, `player_surface_principal_test.go` (31-route table) | S9.1-LAUNCH-1 (rate limiter keys on RemoteAddr, needs trusted XFF at the edge), S9.1-LAUNCH-2 (`AuthRateLimitPerMinute` not yet plumbed from config — one-line devops follow-up) | Verified |
| S9-04 | identity-compliance (subagent) | Done | `internal/payments/orchestrator.go` (`rg.EvaluateEligibility` wired into `InitiateDeposit`) | `internal/payments/rg_enforcement_integration_test.go` (new); fixed 2 stale fixtures in `internal/httpserver/financial_flow_integration_test.go` | Implicit new policy ("pending_verification can't deposit") flagged by ledger-finance as deserving an explicit ADR, not decided here | Verified |
| S9-05 | ledger-finance (subagent) | Done | `migrations/0082_stage9_db_hardening.{up,down}.sql` (new tip — immutability/TRUNCATE-deny triggers on 6 tables, ARCH-DB-3 composite-FK fix on 7 tables, LOCK-2 fix, 3 pagination indexes), `internal/casino/orchestrator.go` (postWin FOR UPDATE lock — double-stake-release + deadlock fix) | 6 new migration-0082 regression test files (one per trigger/domain), `internal/casino/stage9_concurrency_integration_test.go` (4 tests, 2 proving genuine pre-existing defects via stash-and-rerun) | LOCK-1 (ABBA deadlock risk between postBet/postWinDirectCash) correctly proven NOT fixable by the architect's suggested in-`Post` sort, and deferred with full documented reasoning — needs a cross-cutting `ledger.LockProjectionsInOrder`-style discipline, architect-level, not this stage | Verified |
| S9-06 | payments (subagent) | Done | review + `internal/payments/stage9_concurrency_integration_test.go` (new, 2 tests) | concurrent-deposit no-lost-update, tampered-webhook-never-reaches-ledger | none | Verified |
| S9-07 | identity-compliance / security (withdrawal four-eyes re-audit) | Done | `internal/withdrawal/stage9_four_eyes_adversarial_test.go` (new) | duplicate-approve, concurrent-same-approver-double-submit, two-logins-one-person-cannot-satisfy-four-eyes (+ concurrent twin) | none | Verified |
| S9-08 | qa (subagent) | Done | `internal/httpserver/stage9_concurrency_integration_test.go` (new, 6 tests — fixed an RLS-scoping bug in the test harness itself via `pool.WithPrincipalScope`), `internal/httpserver/{sportsbook,casino}_handlers.go` (policy-blocked log events), `docs/runbooks/observability-and-alerting.md` (new), `docs/testing/testing-strategy.md` (§20) | 6 new concurrency tests | Explicitly discloses no metrics backend is deployed yet | Verified |
| S9-09 | frontend (subagent) | Done | `b2c/src/app/ErrorBoundary.tsx` (new), `b2c/src/app/App.tsx`, `b2c/src/config/brand.ts` (fail-closed `resolveBrandSlug`), `b2c/.env.example` | `ErrorBoundary.test.tsx`, `brand.test.ts`, `AuthContext.test.tsx` (new) | none | Verified |
| S9-10 | backoffice (subagent, resumed after rate-limit interruption) | Done | `backoffice/src/lib/useOnceGuard.ts` (new), `backoffice/src/features/bonus/ChangeRequestQueuePage.tsx` (double-submit guard + self-approval warning — the one genuine gap found), `backoffice/src/features/bonus/PlayerBonusSummary.tsx`, `backoffice/src/lib/money.ts`, `backoffice/.env.example` (new) | `useOnceGuard.test.ts`, `ChangeRequestQueuePage.test.tsx` (double-submit regression), `money.test.ts`, `WithdrawalDetailPage.test.tsx` (double-submit regression) | No manual-balance-adjustment UI exists anywhere — confirmed, flagged as a real out-of-scope Blueprint gap, not built | Verified |
| S9-11 | integrations (subagent, resumed) | Done | `internal/providers/httpclient/client.go` (`redactCredential`/`redactCredentialHeader`), `internal/providers/casino_adapter_composition_demo_test.go` (new), `docs/decisions/0080-*.md` (Stage 9 addendum) | new composition-demo test against a real `httptest.Server` | none | Verified |
| S9-12 | architect (review) | Done | review only — ARCH-DB-1/2/3, LOCK-1..4, `PLAT-MIGDRIFT-1` upgrade, migration-safety Rules A-E for 0083+ | n/a | ARCH-DB-2 (6 RLS-free catalogue tables have app-only write authorization, no DB backstop — cross-domain casino+sportsbook+internal/db+cmd fix, explicitly deferred) | n/a |
| S9-13 | Orchestrator | Done | `internal/providers/httpclient/errors.go` (`GoString()` redaction on 3 error types, mirroring `internal/jurisdiction`'s `SEC-4I-C-02` pattern), `internal/providers/httpclient/client_test.go`, `docs/security/runtime-role-separation.md` (ARCH-DB-1 exception-list correction), `docs/governance/task-registry.md` (backfill), `docs/governance/stage-4i-exit-register.md` (PLAT-ROLESPLIT-1 status update) | `TestGoStringRedaction_NeverLeaksCredentialOrBodyViaSharpV` (new) | none | n/a |
| S9-14 | Orchestrator | Done | `docs/runbooks/backup-and-disaster-recovery.md` (new — honest NOT MET/NOT IMPLEMENTED evidence status), `docs/runbooks/operational-runbooks.md` (new — 9 concise incident runbooks), `docs/runbooks/README.md` (updated index) | n/a (documentation) | Backup/DR genuinely blocked on ADR 0009's hosting decision (no cloud account provisioned yet) — production blocker, not a gap in this stage's work | n/a |
| S9-15 | Orchestrator | Done | this registry (Stage 9 section), `docs/active-stage.md`, `docs/progress.md` | full independent validation gate re-run against fresh `stage9_final` DB: 82 migrations, build/vet/gofmt clean, full integration suite (32 packages), full `-race` suite, runtime-role adversarial suite, Stage 6+7 defining acceptance tests by exact name, both frontends' test+build re-verified against final merged state | none | n/a |

### Review findings and dispositions

**Genuine, previously-undetected financial defects found and fixed**
(both empirically reproduced by stashing the fix and re-running): (1) a
double stake-release race in `resolveWinOrigin`'s pre-existing `LF-18`
`ErrLockAlreadyReleased` guard — an unlocked check-then-act read allowed
two concurrent distinct win callbacks on one locked round to each read
"still locked" and each post the full release, driving
`player_locked_cash` negative while every individual posting still
balanced (invisible to the tenant-wide debit/credit invariant); (2) an
unhandled Postgres deadlock (40P01) between a win and a rollback of the
same round from a lock-order inversion — fixed with a `FOR UPDATE` lock on
the round's own `casino_bet` rows at the top of `postWin`.

**ARCH-DB-3 (composite brand-pinning FK gap)**: 7 tenant-owned tables
pinned `brand_id` to "some brand in this tenant" rather than the player's
own brand — the same defect class Stage 6.1/Stage 7 already fixed
elsewhere. 6 fixed via drop-and-replace; `jurisdiction_resolutions` got
the composite added alongside its existing FK rather than replacing it
(a MATCH-SIMPLE composite-only FK would silently skip nullable rows);
`player_restrictions` was correctly excluded — the architect's claim it
needed fixing was stale (migration 0038 already added the correct
composite) and further widening would have been semantically wrong
(brand_id there is deliberately independent administrative scope).

**LOCK-1 (deferred, not a silent drop)**: the architect's suggested fix —
sorting entries inside `ledger.Post` — was rigorously proven not to close
the ABBA cycle, because `postBet` acquires its cash-account lock outside
and before calling `ledger.Post`. Documented in full rather than landing
a fix that looked complete but wasn't; needs a cross-cutting
`ledger.LockProjectionsInOrder`-style discipline spanning ledger, casino,
sportsbook, and withdrawal — an architect-level decision for a future
stage.

**`PLAT-MIGDRIFT-1` upgraded from observation to FIX BEFORE PRODUCTION**,
and a live instance of it was independently discovered: the shared
`igaming_platform_dev` database still carries a stale `provider_bet_ref`
column name predating Stage 8's rename, despite presumably reporting a
clean migration status. Worked around by building/testing against a
fresh scratch database instead of trusting the shared one; flagged for a
rebuild before that specific shared DB is trusted again.

**ARCH-DB-2 (deferred, cross-domain, not built this stage)**: 6 RLS-free
catalogue tables (`casino_games` + 5 sportsbook catalogue tables from
migration 0078) have only application-level write authorization, no DB
backstop — the platform already solved the identical problem for
`assets` (migrations 0044/0045). Fix requires routing casino/sportsbook
catalogue writes through `db.Pool.WithPlatformAdmin` and defining a
service-identity scope for the sportsbook sync — spans casino, sportsbook,
internal/db, and cmd, so explicitly not attempted in a parallel window.

**Backup/disaster-recovery: genuinely near-empty evidence category**,
confirmed rather than assumed. No automated backup mechanism, no tested
restore, no replica/standby exist anywhere in this codebase or its
deploy/ tooling. This is not an oversight of this stage's work — it is
structurally blocked on ADR 0009's still-open hosting-provider decision
(no cloud account has been provisioned; only local/dev environments
exist). Documented honestly in `docs/runbooks/backup-and-disaster-
recovery.md` as NOT MET / NOT IMPLEMENTED / PROVIDER DEPENDENT, with a
concrete minimum action plan for when a provider is selected, rather than
fabricating a runbook that would produce evidence that doesn't transfer
to the eventual real topology.

**Data retention/audit (§19)**: already correctly modeled in
`docs/architecture/16-privacy.md`'s "Retention" section as deferred
future configuration pending a human legal decision on retention periods
(CLAUDE.md forbids inventing one) — reconfirmed current and accurate this
stage, no code or doc change needed.

### Explicitly NOT this stage's to build (confirmed absent)

B2B/partner/retail work of any kind, undocumented external provider API
integration, any answer to HDR-J-6/J-7/J-8/J-9, any invented regulatory
or certification claim, ARCH-DB-2's cross-domain catalogue-authorization
fix, LOCK-1's cross-cutting lock-ordering discipline, and an actual
production backup/restore mechanism (structurally blocked on the open
hosting decision, not skipped).

## Stage 9.1 — Production Blocker Closure

Directive: "STAGE 9.1 — PRODUCTION BLOCKER CLOSURE," opening with "STAGE
9 IS APPROVED." A focused hardening stage closing the concrete
engineering gaps Stage 9 identified but did not fix: ARCH-DB-2 (database
backstop for 6 RLS-free catalogue tables), LOCK-1 (canonical financial
lock ordering), the two rate-limiter launch gates, PLAT-MIGDRIFT-1
(migration content-drift protection), the live shared-dev-DB drift
instance, a provider-error-body safety P3, and 6 named minor technical
debt items. Explicitly out of scope: B2B/retail, undocumented external
providers, any jurisdiction/legal decision, any redesign of completed
architecture. Full narrative in the Stage 9.1 final report delivered to
the human; this table and the sections below are the durable record.

Structured as: Phase 1 (parallel, design-only) — two architect rulings
(ADR 0081 for ARCH-DB-2, ADR 0082 for LOCK-1) plus three independent
hardening workstreams (rate limiter/migration-drift/dev-DB-rebuild;
provider error-body safety; casino/sportsbook debt classification, the
QA audit below). Phase 2 — two implementation waves executing the two
ADRs exactly (migration 0084 + `db.WithPlatformService`; the
`ledger.LockProjectionsForPosting` canonical-lock-ordering rewrite across
6 packages). Phase 3 — mandatory `security` review + `code-reviewer`
review of both large changes (CLAUDE.md: "security-sensitive
functionality requires explicit review… before being marked complete").
Phase 4 — a fix round closing every P1/P2 finding from Phase 3 (one real
regression: `AdvisoryLockGrant` losing its `ErrNotFound` wrapping,
causing a 404→500 regression; migration 0085 adding a
staff-principal-resolution trigger to `casino_games`; two anti-decay
static-guard tests meaningfully strengthened; a genuine test-isolation
flake between two new test files fixed). Phase 5 — the orchestrator's own
independent full validation gate against a fresh 85-migration database
(not merely trusted from agent self-reports), governance docs, commit,
push, final report.

| ID | Owner | Status | Files owned | Tests | Blockers | Integration |
|---|---|---|---|---|---|---|
| S91-01 | architect | Done | `docs/decisions/0081-arch-db-2-catalogue-write-authorization.md` (new — design ruling only, no code) | n/a | none | n/a |
| S91-02 | architect | Done | `docs/decisions/0082-canonical-financial-lock-ordering.md` (new — design ruling only, no code) | n/a | E-1 exception named (a real but currently-unreachable L2-before-L0 inversion in casino settlement, documented not fixed) | n/a |
| S91-03 | devops | Done | `internal/httpserver/ratelimit.go` (trusted-proxy client-identity model), `internal/config/config.go` (`AuthRateLimitPerMinute`/`TrustedProxyCount`), `cmd/platform-api/main.go`, `migrations/0083_migration_checksum_tracking.{up,down}.sql` (new), `internal/db/migrate.go` (checksum recording + `VerifyMigrations`), `cmd/migrate/main.go` (`migrate verify`), `.github/workflows/ci.yml` | new trusted-proxy/rate-limit tests, migration-checksum integration tests | none | Verified |
| S91-04 | integrations | Done | `internal/providers/httpclient/{client,errors}.go` (bounded truncation, redact-then-truncate ordering), `docs/security/security-architecture.md` | new truncation/composition tests | none | Verified |
| S91-05 | qa | Done | review + `docs/governance/task-registry.md` (QA technical-debt-classification subsection below) | re-ran `internal/casino`/`internal/sportsbook` suites | none | n/a |
| S91-06 | backend | Done | `migrations/0084_catalogue_write_authorization.{up,down}.sql` (new), `internal/db/platform_service.go` (new), `internal/httpserver/casino_admin_handlers.go`, `internal/casino/catalogue.go`+`types.go`, `internal/sportsbook/catalogue.go`, `cmd/platform-api/main.go`, re-scoped test fixtures across 5 files | 3 new adversarial test files covering all 11 ADR §7.6 invariants, end-to-end HTTP proof public catalogue reads still work | none | Verified |
| S91-07 | ledger-finance | Done | `internal/ledger/lockorder.go` (new), `internal/ledger/ledger.go`, `internal/casino/{orchestrator,bonus_settlement}.go`, `internal/sportsbook/orchestrator.go`, `internal/payments/orchestrator.go`, `internal/withdrawal/withdrawal.go`, `internal/bonus/{lifecycle,conversion}.go` | all 14 ADR 0082 tests + 2 extra, tests 1-6 individually shown to fail pre-fix (40P01), full suite `-race -count=5` | none | Verified |
| S91-08 | security | Done | review only — appended `docs/security/security-architecture.md`'s Stage 9.1 dated section | n/a | 3 P2s found (SEC-S91-1/2/3), 6 P3s documented | n/a |
| S91-09 | code-reviewer | Done | review only | n/a | 1 correctness regression found (AdvisoryLockGrant sentinel), 2 documented-not-fixed ordering gaps, 1 simplification noted (deferred) | n/a |
| S91-10 | ledger-finance (fix round) | Done | `internal/bonus/lifecycle.go` (ErrNotFound fix), `internal/ledger/lockorder_static_test.go` (both anti-decay guards widened on `go/ast`), `docs/decisions/0082-*.md` (§5.1a, E-2 exception), `internal/rg/rg.go`+`internal/risk/evaluator.go` (stale comments), `internal/bonus/wave3_phase2_migrations_integration_test.go` (nit) | new regression test for the ErrNotFound fix, new evasion-pinning test for the widened guards | flagged (not its own to fix): migration-count fixture needed a further bump for migration 0085 | Verified |
| S91-11 | backend (fix round) | Done | `migrations/0085_casino_games_require_platform_principal.{up,down}.sql` (new — SEC-S91-3), test-isolation fix in `internal/sportsbook/catalogue_write_authorization_integration_test.go`, `errors.Is` nits, `internal/casino/types.go` stale comment | new staff-principal-rejection test, flake re-verified gone under 5x concurrent-package runs | none | Verified |
| S91-12 | Orchestrator | Done | migration-count fixture bump for 0085 across the same 5 files Stage 9 established (found and fixed one occurrence S91-10 missed, caught by the orchestrator's own independent full-suite run — see below), `docs/governance/stage-4i-exit-register.md` (PLAT-MIGDRIFT-1 closure update), this registry, `docs/active-stage.md`, `docs/progress.md` | full independent validation gate: 85 migrations clean, `migrate verify` clean, full integration suite + `-race` green (32 packages), runtime-role adversarial suite green, lock-ordering concurrency tests repeated 3x under `-race`, Stage 6+7 acceptance tests by exact name, both frontends re-verified | none | n/a |

### Review findings and dispositions

**One genuine correctness regression found and fixed** (code-reviewer):
the LOCK-1 implementation's new `AdvisoryLockGrant` precondition (added
to resolve `player_account_id` before acquiring the new L0.2 player-scope
advisory lock) returned a bare error for an unknown/cross-tenant grant id
instead of wrapping the package's `ErrNotFound` sentinel — every
`AdvisoryLockGrant` caller (`ActivateGrant`, `ConvertGrant`,
`TerminateGrant`, `RecordWageringContribution`,
`CheckAndCompleteGrant`) would have returned HTTP 500 instead of 404 for
a bad grant id the first time a handler was wired directly to one of
them. Fixed by wrapping with `%w`; verified by reverting the fix and
confirming 7 sub-tests fail loudly.

**One defense-in-depth gap closed** (security finding SEC-S91-3,
migration 0085): the ARCH-DB-2 `casino_games` write policies checked only
that `app.platform_admin_principal_id` was SOME non-null UUID, not that
it resolved to a real platform-scoped `staff_users` row — meaning any
`WithPlatformAdmin`-scoped code path, regardless of what permission
gated it, would satisfy the policy. The five `sb_*` tables' policies
already pinned an exact literal service-identity string and were
unaffected. Fixed by mirroring migration 0044's `assets` precedent — a
new trigger validating the principal against `staff_users` — while
deliberately deferring to the pre-existing RLS-policy error (SQLSTATE
42501) when the GUC is unset at all, so no unrelated scope's tests broke.

**Two anti-decay regression guards meaningfully strengthened**
(code-reviewer + security, independently): the R4 guard
(`TestLockOrder_NoProjectionForUpdateOutsideLedgerPackage`) was a
single-pattern string match security defeated with 6 different
constructions (string concatenation, `fmt.Sprintf` table names, `FOR NO
KEY UPDATE`/`FOR SHARE`, direct `UPDATE`, `ON CONFLICT…DO UPDATE`) and
never walked `cmd/`; rewritten on `go/ast` to catch all 6 plus walk
`cmd/`, with a new test pinning every evasion. The INV-LOCK-E1 guard only
walked `internal/bonus`; widened to also cover `internal/casino` (which
now transitively holds the same lock scope via the L0.2 fold-in) with a
function-scoped positional check.

**One real, previously-latent test defect found while fixing a nit**: the
`_ = activeGrant` placeholder in a new concurrency test turned out to
mask a genuine ordering bug in the test's own setup (the grant was seeded
*before* the racing bet, so it was already `completed` by the time of the
race and the test asserted nothing about actual lock contention) — fixed
by reordering the seed, which now genuinely exercises contention.

**One genuine test-isolation flake found and fixed** (qa, reproduced
1-in-3 running two packages concurrently): two new ARCH-DB-2 test files,
written independently by the same implementation wave, both assumed
exclusive ownership of `sb_*` tables' row counts in the shared
integration database. Fixed by scoping one test's counts to its own
known `external_ref` prefix.

**Two exceptions named rather than silently left inconsistent**
(architect + code-reviewer, ADR 0082 §5.1/§5.1a): E-1 (pre-existing, a
casino-settlement L2-before-L0 lock inversion, safe today because no
counterpart path holds a grant advisory lock and waits on
`ledger_transactions`) and the new E-2 (`postBet`'s `BindProviderRound`
call taking an L1 lock after the L3 pre-lock step, safe today because
`postBet` is `casino_provider_rounds`' sole writer). Both are standing,
grep-testable invariants, not silent gaps.

**Explicitly documented, not fixed, with full reasoning:** ARCH-DB-2's
Phase 2 four-eyes governance for `casino_games.jurisdiction_blocklist`
removals/`status` re-activation (ADR 0081 §5.1-5.2 — specified in detail,
requires new public API surface, its own stage slot); LOCK-1's E-1/E-2
exceptions (above); the ~1,280-line duplication across
`internal/{casino,payments,withdrawal}/lockorder_harness_test.go`
(code-reviewer's simplification finding — SAFE DEFERMENT, a later
stage's refactor, not a correctness issue); security's 6 P3 findings
(GUC-settable-by-app-role — already ADR-acknowledged; silent-no-op
denied-scope UPDATEs; partial regression coverage on frozen columns;
scope-helper reliance on the tree-wide `is_local=true` GUC convention;
open SELECT exposing `jurisdiction_blocklist` — already an accepted
consequence per ADR 0081 §2.3; one stale code comment, fixed anyway).

### QA Technical Debt Classification (S91-05)

Purpose: classify six named pieces of prior-stage technical debt
(casino round-visibility index, migration 0079's constraint-addition
strategy, mock inbound-failure injection, sportsbook cumulative risk,
sportsbook catalogue jurisdiction gating, B2C build-time brand model) as
exactly one of `FIX NOW`, `FIX BEFORE PRODUCTION`, `SAFE DEFERMENT`, or
`EXTERNAL/HUMAN DEPENDENCY`, and implement only what is genuinely small
and within a `qa`-owned surface. Explicitly did NOT touch jurisdiction/
country-approval policy (HDR-J-6/7/8/9 untouched), did NOT touch
`internal/db`, RLS policies, or the sportsbook catalogue tables'
ownership model (another Stage 9.1 workstream's territory), and did NOT
unilaterally implement any cross-domain architecture change per
CLAUDE.md's "no specialist redesigns shared architecture unilaterally"
rule. Verified every claim against live code/schema/tests, not prior
summaries.

| ID | Owner | Status | Files touched | Tests | Classification |
|---|---|---|---|---|---|
| S91-QA-01 | qa | Done | none — verification only | re-ran `internal/casino` full integration suite against a fresh `qa_stage91_scratch` DB (82 migrations, clean up) | Item 1 (casino round-visibility index) |
| S91-QA-02 | qa | Done | none — review only | n/a | Item 2 (migration 0079 constraint strategy) |
| S91-QA-03 | qa | Done | none — review only | re-ran `internal/casino` and `internal/sportsbook` full integration suites | Item 3 (mock inbound-failure injection) |
| S91-QA-04 | qa | Done | none — review only, `internal/risk` out of this agent's allowed scope | n/a | Item 4 (sportsbook cumulative risk) |
| S91-QA-05 | qa | Done | none — review only | n/a | Item 5 (sportsbook catalogue jurisdiction gating) |
| S91-QA-06 | qa | Done | none — review only | n/a | Item 6 (B2C build-time brand model) |
| S91-QA-07 | qa | Done | this registry section | full validation gate (see below) | Governance write-up |

### Item 1 — Casino round-visibility index: ALREADY FIXED, no action taken

The `(tenant_id, created_at DESC)`-style index the Stage 7 DB/RLS review
recommended (task registry's Stage 7 section: "recommended (not yet
added, recorded as tech debt): a `(tenant_id, created_at DESC)` index on
`casino_launch_sessions` before either round-visibility view carries
real volume") **was already added** by Stage 9's `ledger-finance`
workstream in `migrations/0082_stage9_db_hardening.up.sql` §3
("Pagination indexes (audit §26 item 2)"):
`idx_casino_launch_sessions_tenant_time ON casino_launch_sessions
(tenant_id, created_at DESC, id DESC)`. Verified live against a fresh
`qa_stage91_scratch` database built from all 82 migrations — the index
exists exactly as described, matching `listSessionsPage`'s own `ORDER BY
created_at DESC, id DESC` query shape (`internal/casino/history.go`)
column-for-column.

Checked `casino_provider_rounds` for the same class of gap: its only two
production call sites (`internal/casino/rounds.go`'s
`BindProviderRound`/point lookup by the `UNIQUE (tenant_id, provider_id,
provider_round_id)` constraint, and `LookupProviderRoundIDsBySession`'s
`WHERE tenant_id = $1 AND launch_session_id = ANY($2)` used only against
the page-bounded set of session ids `ListRoundsForTenant`/
`ListRoundsForPlayer` already paginated) are both already covered by
existing indexes (`idx_casino_provider_rounds_session (tenant_id,
launch_session_id)` and the unique constraint's own index) — no
unbounded scan exists at either call site, so no additional index is
needed there.

**Classification: no longer applicable — resolved prior to this audit.**
No migration added by this stage.

### Item 2 — Migration 0079 constraint-addition strategy: SAFE DEFERMENT (process reminder only)

`migrations/0079_casino_launch_session_brand_pinning.up.sql` drops one
FK constraint and adds a replacement composite FK
(`casino_launch_sessions_player_tenant_brand_fkey`) with a bare `ADD
CONSTRAINT ... FOREIGN KEY`, not the `NOT VALID` + separate `VALIDATE
CONSTRAINT` split. `ADD CONSTRAINT` for a foreign key without `NOT
VALID` takes `ACCESS EXCLUSIVE` on the table for the full duration of
its initial validation scan (blocking all reads/writes), whereas `NOT
VALID` acquires the exclusive lock only briefly and defers the scan to a
separate `VALIDATE CONSTRAINT` step that takes the much weaker `SHARE
UPDATE EXCLUSIVE` lock instead.

Note: this specific document could not locate a committed ADR
documenting "migration safety Rules A-E" — `docs/governance/
task-registry.md`'s own Stage 9 row (`S9-12`) references it as
`architect (review)`'s output ("migration-safety Rules A-E for 0083+")
but no `docs/decisions/*.md` file or `docs/architecture/*.md` file
contains that content; it appears to exist only in the Stage 9
completion report delivered to the human, never backfilled into a
committed doc. This is itself a minor governance-record gap (same class
as the Stage 8→9 httpclient P3 backfill already recorded in this file),
noted here rather than silently worked around, but out of this agent's
scope to fix (would require locating/reconstructing the original Stage 9
`architect` output, not something `qa` should reconstruct unilaterally).

Per this repo's own established rule, migration 0079 is **not
retro-edited** — it is already applied everywhere and doing so would
create exactly the `PLAT-MIGDRIFT-1`-style drift this project already
suffered once. Two additional facts bound the real-world risk to
effectively zero for this specific migration: (a) `casino_launch_sessions`
had at most a handful of synthetic test rows when 0079 was authored and
applied (Stage 7, pre any real player traffic), and (b) the platform's
real production database does not exist yet (Stage 9's own disclosure:
ADR 0009's hosting decision is still open) — when migrations are
eventually applied to a real production database, they will be applied
as the full ordered sequence starting from an empty schema, so
`casino_launch_sessions` will be empty at the moment 0079 runs. This
migration therefore poses no actual downtime risk to the real, eventual
production rollout, only to a hypothetical scenario where 0079 is
replayed in isolation against an already-populated table via drift.

**Classification: SAFE DEFERMENT for migration 0079 itself (already
applied, inert, and provably harmless given production doesn't exist
yet). Recorded as a PROCESS REMINDER for future migration authors**: any
migration adding a `NOT NULL`-validated or foreign-key constraint against
a table that could hold non-trivial production rows by the time it runs
should use the `NOT VALID` + `VALIDATE CONSTRAINT` split (the pattern
Stage 9's `architect` ruling establishes for migrations 0083+), not
0079's own bare `ADD CONSTRAINT` — 0079 should not be cited as precedent
by a future migration touching a genuinely populated table.

### Item 3 — Mock inbound-failure injection: SAFE DEFERMENT (both packages)

**Casino (`MockCasinoProvider`)**: `FailNextCall`/`consumeFailure`
already exist and are wired into every OUTBOUND call
(`Launch`/`Bet`/`Win`/`Rollback`), and are exercised by
`TestFailureModeMatrix_F_ProviderTransportFailureAtLaunchLeavesNoTrace`.
The gap Stage 8 flagged is narrower than "no failure injection at all":
`HandleCallback` (the INBOUND webhook-parsing path) has no
error-injection hook beyond signature/parse rejection (already tested).
Read `internal/casino/failure_mode_matrix_integration_test.go`'s own
item-F doc comment (lines ~883-891, pre-existing, not written by this
audit): it already reasons through why this has no real analogue — "an
HTTP 5xx on the INBOUND callback path... There is no such thing — a
callback is the provider calling US; a 5xx would be the platform's own
response, and retrying it is the provider's business." No other
downstream failure mode inside `HandleCallback` exists to inject (it is
a pure parse-and-verify function articulating exactly two failure
branches: malformed JSON, bad signature — both tested). No concrete,
currently-writable test is blocked by this gap.

**Sportsbook (`MockSportsbookProvider`)**: has zero failure-injection
mechanism, but this is not a mock-completeness gap — it is a structural
fact about the `Provider` interface itself:
`type Provider interface { Catalogue() CatalogueResult }`
(`internal/sportsbook/types.go`) has **no error return at all**, so
`SyncCatalogue` (`internal/sportsbook/catalogue.go`) has no failure
channel from the provider to receive in the first place. Adding a
`FailNext`-style hook to the mock would be inert until the `Provider`
interface itself is widened to return `(CatalogueResult, error)` — an
interface change spanning `SyncCatalogue`, every future adapter, and
`cmd/platform-api`'s wiring, which is an architecture-level change, not
a "minimal mock hook." Confirmed via
`internal/sportsbook/failure_mode_matrix_integration_test.go`'s own item
F disposition ("DOES NOT APPLY... MockSportsbookProvider is consulted
for catalogue data only; no call it makes is on the money path... It
also has no error-injection hook") — `Catalogue()` runs once at
same-process startup/sync, never per-request, and is not on the money
path, so no financial-integrity or authorization test is blocked by its
absence either.

**Classification: SAFE DEFERMENT for both.** No mock code changed, no
new test added — implementing either would be cosmetic (casino) or
require a real interface redesign (sportsbook), neither of which this
audit's "minimal hook, only if genuinely blocking real coverage today"
bar clears.

### Item 4 — Sportsbook cumulative risk: FIX BEFORE PRODUCTION, architect/risk/ledger-finance-owned, NOT implemented here

Confirmed still unwired in current code:
`internal/risk/types.go`'s `operationCumulativeSpecs` map has an explicit
`OperationSportsbookBet: {}` placeholder entry (a comment, not a real
`cumulativeSpec`), and `internal/risk/cumulative.go`'s
`operationCumulativeSpecs` map itself has exactly one production entry
(`casino_bet`) — matching `docs/architecture/09-sportsbook-architecture.md`
§16.3's own "no sportsbook operation is wired yet" statement and ADR
0047 §4's disposition, both still accurate. This refers to the
rolling-window/cumulative-exposure RISK LIMIT wiring (`risk_rules`'
`cumulative_amount` kind for `sportsbook_bet`), not the separate
"open-liability reporting" concept (doc 09 §9, which is a reporting-line
computation off the existing `player_locked` ledger projection, owned
jointly with `data-analytics`, and not part of this audit's scope since
no reporting surface has been authorized at all).

Per-bet risk (max/min, hard/soft limits) and RG eligibility are already
enforced on every sportsbook bet, fail-closed; nothing can currently even
configure a sportsbook cumulative rule (it fails closed with
`ErrUnsupportedCumulativeOperation` if attempted), so there is no live
exposure gap today. Wiring the map entry is described by three separate
specialist reviews (Stage 6.1 ADR 0047 §4, Stage 4H-B0-R6 doc 09 §16.3)
as small in isolation, but doc 09 §16.3's own "drift note" flags that the
correct `IgnoredAccountTypes` value changed from `player_locked` to
`player_locked_cash`/`player_locked_bonus` once migration 0048 landed —
meaning whoever wires this must re-verify the exact account-type set
against current code, not copy the older reference comment literally.
`internal/risk` is outside this agent's allowed scope for this task, and
wiring a real risk limit is also a `risk_rules` configuration/business
decision, not a pure mechanism fix — so this is left to `architect`/
`risk`/`ledger-finance` as a scoped, well-specified follow-up, not
attempted here.

**Classification: FIX BEFORE PRODUCTION** (specifically: before any
tenant is expected to configure a sportsbook exposure cap), owned by
`risk` with `ledger-finance` confirming the post-migration-0048 account
types — not a `qa`-agent patch.

### Item 5 — Sportsbook catalogue jurisdiction gating: real mechanism gap confirmed, FIX BEFORE PRODUCTION, NOT implemented here

Verified current code still matches ADR 0047 §2-3's disposition exactly:
`internal/sportsbook/orchestrator.go`'s `PlaceBet` doc comment (lines
~92-110) states, unchanged, that jurisdiction is "DELIBERATELY NOT
evaluated here," and `sb_events`/`sb_selections` (migration 0078) still
carry no `jurisdiction_blocklist`-equivalent column — confirmed absent
from the current schema. Casino's own mechanism
(`casino_games.jurisdiction_blocklist` +
`internal/casino/orchestrator.go`'s `evaluateJurisdictionBlocklist`,
called with an already-resolved `jurisdiction.Resolution` at `LaunchGame`
time) remains real, tested, and — since `PUT /v1/admin/casino/games`
already lets a `platform_admin` populate it today with no further code
change — one authorized admin action away from being live, while
sportsbook has no equivalent lever to reach for at all. This is the same
architectural-symmetry gap ADR 0047 named, still open, still inert today
(every blocklist on the platform is empty; no HDR-J item is answered).

This is **not** a "one missing filter check reusing the same existing
jurisdiction-resolution service" — it requires: (1) a new migration
adding a blocklist-equivalent column to `sb_events`/`sb_selections`
(schema change, on the SAME 5 tables another Stage 9.1 workstream is
concurrently redesigning write-authorization/ownership for — explicitly
out of this agent's territory this stage, and a genuine collision risk
if attempted in parallel); and (2) wiring `jurisdiction.Resolve` into
`PlaceBet` for the first time (sportsbook's `RiskRequest` does not even
set `JurisdictionCode` today, per ADR 0047 §1 row E) — comparable in
shape and size to what casino's `LaunchGame` already does, but new code
in `internal/sportsbook`, not a one-line reuse. Per this task's own
fallback instruction and CLAUDE.md's "no specialist redesigns shared
architecture unilaterally" rule, this is documented precisely rather
than implemented.

**Exact missing mechanism** (for whoever picks this up): add a
nullable/defaulted `jurisdiction_blocklist text[]` column to `sb_events`
and/or `sb_selections`; export (or duplicate under an
`internal/sportsbook`-local name) casino's `evaluateJurisdictionBlocklist`
shape; call `jurisdiction.Resolve` inside `PlaceBet` before the ledger
posting, exactly where casino calls it inside `LaunchGame`; gate the
control identically to casino's K3-1 "empty blocklist = not armed" rule
so it stays a no-op until a real blocklist entry is ever configured. No
new human/policy decision is required to BUILD the mechanism — only to
ever populate a blocklist with it, per ADR 0047 §3's own reasoning,
unchanged.

**Classification: FIX BEFORE PRODUCTION** (specifically: before a second
jurisdiction or the first B2B tenant is onboarded, per ADR 0047's own
already-recorded disposition — re-verified accurate today), requiring
`architect`-level sequencing against the parallel catalogue-ownership
workstream plus `sportsbook`/`identity-compliance` implementation — not
a `qa`-agent patch.

### Item 6 — B2C build-time brand model: SAFE DEFERMENT, confirmed already-decided MVP scope limitation

Verified `b2c/src/config/brand.ts`'s own doc comment: "Stage 6 brand-
awareness mechanism (per the directive: build-time env vars are
acceptable for this MVP, a full runtime multi-brand theming engine is
explicitly out of scope/future work)" — an already-recorded, explicit
Stage 6 directive decision, not an unexamined default. The Stage 6.1
registry section's own item 4 disposition ("B2C build-time brand model
before true multi-brand — SAFE DEFERMENT, required before a second B2C
brand goes live... every authenticated call derives tenant/brand purely
from the verified JWT") remains accurate: `slug` only selects the
pre-authentication login/register target; nothing in `internal/httpserver`
trusts a client-supplied tenant/brand id post-authentication (CLAUDE.md's
"tenant_id is authoritative from server-side authenticated context
only" is not violated by this build-time mechanism).

`docs/architecture/37-b2c-brand-frontend-architecture.md` §3.3
recommends a future SSR/per-request brand-resolution architecture
(Next.js/SvelteKit-class) specifically for when true runtime multi-brand
serving is needed — but that document is explicitly a Stage 4H-B1 Wave
1.5 architecture-freeze document that "does not authorize" building
anything, and B2B (the only scenario that would require more than one
brand build) remains entirely out of scope for the whole platform right
now (Stage 9's own directive: "Explicitly out of scope: B2B/partner/
retail"). The underlying data model this frontend consumes (`brands`
table, `tenant_jurisdiction_configs`, casino tenant-scoped availability,
withdrawal policy) is already fully data-driven, not brand-name-coded —
only the frontend's own DEPLOYMENT artifact (which brand's slug/colors
get compiled into a given build) is build-time, which is the accepted
MVP limitation, not a mechanism defect.

**Classification: SAFE DEFERMENT**, confirmed, not assumed — required
before a second B2C brand or any B2B rollout, not before current
production launch of the single existing B2C brand.

### Explicitly NOT this stage's to build (confirmed absent)

Any answer to HDR-J-6/J-7/J-8/J-9 or any new jurisdiction/country policy
value; any change to `internal/db`, RLS policies, or the sportsbook
catalogue tables' (`sb_sports`/`sb_competitions`/`sb_events`/
`sb_markets`/`sb_selections`) write-authorization/ownership model (another
Stage 9.1 workstream's territory); any retro-edit of migration 0079; any
`internal/risk` code change; any sportsbook jurisdiction-blocklist
migration or `PlaceBet` wiring; any B2C frontend architecture change.

### Validation

Fresh scratch database (`qa_stage91_scratch`) built from all 82
migrations (clean up-only run, no down/up round-trip needed since no
migration was added); `go build ./...`, `go vet ./...`, `gofmt -l .` all
clean; `go test -tags=integration ./internal/casino/...` and
`./internal/sportsbook/...` both green against the fresh database. No
production code, test code, or migration was added or modified by this
audit — every classification above was reached by reading live code/
schema/tests directly, not by trusting prior summaries.

### Explicitly NOT this stage's to build (confirmed absent, stage-wide)

B2B/partner/retail work of any kind; any undocumented external provider
API integration; any answer to HDR-J-6/J-7/J-8/J-9 or HDR-M-1/M-2; any
production database cutover, cloud provider selection, hosting AUP,
backup infrastructure, production restore, or production credentials;
any legal retention period; any redesign of completed architecture
outside the two named ADRs; ARCH-DB-2's Phase 2 four-eyes governance API
(specified, not authorized); a distributed/shared-state rate limiter
(deliberately not built — an accepted, documented single-instance
limitation).

### Governance-record note: the migration-count fixture pattern, twice in one stage

Two new migrations landed independently this stage (`0083` from S91-03,
`0084` from S91-06), then a third (`0085` from S91-11, in the fix round).
Each advance requires bumping the same 5 hardcoded-rollback-step-count
test files this project has bumped at every migration-tip advance since
Stage 8 (`internal/bonus/wave3_phase2_migrations_integration_test.go`,
`internal/jurisdiction/migration_0075_integration_test.go`,
`internal/jurisdiction/migration_0077_integration_test.go`,
`internal/operatingmarket/migration_0076_integration_test.go`,
`internal/operatingmarket/qa_migration_rls_survives_failed_rollback_test.go`).
S91-05 (qa) correctly bumped all 5 for `0083`+`0084` together. S91-10
(the ledger-finance fix round) correctly flagged that `0085` needed the
same treatment but explicitly left it for whoever owned that pass, per
its own stated file-territory boundary. The Orchestrator's own bump for
`0085` initially missed ONE occurrence — a third, distinct `MigrateDown`
call inside `TestMigration0075_DownMigrationRestoresPreMigrationRLSPosture`
(a full clean rollback including migration 0075 itself, not the two
"roll back on top of 0075" scenarios the other two occurrences in that
file cover) — which the Orchestrator's own independent full-suite run
(not any agent's self-report) caught as a real, reproducible test
failure, diagnosed, and fixed before this stage was marked done. Recorded
here as a concrete instance of why the final validation gate is always
run independently rather than trusted from agent summaries, and as a
reminder for a future migration-tip advance: this specific file has
THREE occurrences needing the bump, not two — a plain `grep -c
"MigrateDown(context.Background(), dir"` against each of the 5 files is
the reliable way to find all of them, rather than assuming the count
from a prior stage's pattern.

## Stage 9.2 — Sportsbook Risk + Jurisdiction Enforcement + Casino Governance

Directive: "STAGE 9.2 — SPORTSBOOK RISK + JURISDICTION ENFORCEMENT +
CASINO GOVERNANCE," opening with "Stage 9.1 is approved." A consolidated
production-critical stage closing the three items Stage 9.1 classified
FIX BEFORE PRODUCTION: ARCH-DB-2 Phase 2 (casino four-eyes governance),
sportsbook cumulative risk, and sportsbook jurisdiction/market gating.
Explicit operating principle: fix meaningful risk now, do not reopen
completed architecture without a concrete defect, do not create
mini-stages for cosmetic issues. Full narrative in the Stage 9.2 final
report delivered to the human; this table and the two detailed wave
records below (Part C, Part B2) are the durable record.

Structured as: Phase 1 (parallel) — an architect design ruling covering
both sportsbook workstreams together (ADR 0083, since both terminate in
`PlaceBet` and needed one authoritative composed order) plus direct
implementation of Workstream A (already fully specified in ADR 0081 §5.2
from Stage 9.1, needing no fresh design). Phase 2 — three implementation
waves, sequenced per ADR 0083 §9.4 (B1 risk map entry and Part C
jurisdiction gating ran in parallel since they touch disjoint files; Part
B2 exposure limits ran only after Part C landed, since both edit
`PlaceBet`'s composed order and landing them concurrently is explicitly
unsafe). Phase 3 — mandatory five-specialist review (security,
ledger-finance, architect self-verification against ADR 0083's own
invariants, qa, code-reviewer). Phase 4 — a two-track fix round closing
every P1/P2 finding. Phase 5 — the orchestrator's own independent full
validation gate against a fresh 90-migration database.

| ID | Owner | Status | Files owned | Tests | Blockers | Integration |
|---|---|---|---|---|---|---|
| S92-01 | architect | Done | `docs/decisions/0083-sportsbook-jurisdiction-gating-and-cumulative-exposure.md` (new — combined design ruling for Workstreams B+C, no code) | n/a | none | n/a |
| S92-02 | casino | Done | `migrations/0086_casino_catalogue_dual_control.{up,down}.sql` (new), `internal/casino/catalogue_governance.go` (new), `internal/httpserver/casino_catalogue_governance_handlers.go` (new), `internal/auth/permission.go` (`PermCasinoCatalogueGovern`), `docs/api/openapi/platform-api.yaml` | 5 new adversarial test files covering concurrent approval/rejection, self-approval, duplicate/replay, rollback, payload-match, atomicity | none (initial P1s closed in S92-06) | Verified |
| S92-03 | risk | Done | `internal/risk/cumulative.go` (one map entry, `OperationSportsbookBet`, no migration) | 2 new test files verifying every field against actual code | none | Verified |
| S92-04 | sportsbook | Done | `migrations/0087_sportsbook_jurisdiction_restrictions.{up,down}.sql` (new), `internal/sportsbook/jurisdiction.go`+`jurisdiction_admin.go` (new), `orchestrator.go` steps 5-7/9/17-18, `catalogue.go` annotators, `internal/httpserver/sportsbook_handlers.go`+new admin handlers, `internal/auth/permission.go`, `docs/decisions/0082-*.md` (Amendment A3/E-3) | 19 of ADR §12.1's 20 named tests implemented and green; 5 rung-2 tests correctly stubbed `t.Skip`-BLOCKED-on-HDR-J-7 rather than omitted | rung 2 (`evaluateOperatingMarket`) SPECIFIED, NOT IMPLEMENTED, blocked on pre-existing HDR-J-7 (`SB-JUR-RUNG2-1`) | Verified |
| S92-05 | sportsbook | Done | `migrations/0088_sportsbook_exposure_limits.{up,down}.sql` (new), `internal/sportsbook/exposure.go`+`exposure_admin.go` (new), `orchestrator.go` steps 10-11, new admin handlers, `docs/decisions/0082-*.md` (Amendment A2/L0.6) | 12 of ADR §12.2's 14 named tests implemented; items 31/33 delivered by S92-03 at the `internal/risk` level, not duplicated | none (initial gaps closed in S92-07) | Verified |
| S92-06 | security, ledger-finance, architect, qa, code-reviewer | Done | review only | n/a | 1 P1 (four-eyes self-approval defeatable), 1 correctness bug found independently by two reviewers (approval-consumption picks wrong request), 1 real information-leak (exposure rejection category is a trading-book oracle), 1 defense-in-depth gap mirroring an already-fixed Stage 9.1 issue, several test-integrity/coverage gaps — see findings below | n/a |
| S92-07 | casino (fix round) | Done | `migrations/0089_casino_catalogue_governance_hardening.{up,down}.sql` (new), `internal/casino/catalogue_governance.go`, 3 test files | 4 new adversarial tests including a reproduction of the exact pre-fix bypass scenario, now rejected | none | Verified |
| S92-08 | sportsbook (fix round) | Done | `migrations/0090_sb_jurisdiction_restrictions_require_platform_principal.{up,down}.sql` (new), `internal/sportsbook/exposure.go` test fixes, `internal/httpserver/sportsbook_handlers.go` (rejection collapse), 2 new RLS/admin-flow test files | 6+ new/repaired tests, 2 previously-vacuous tests repaired and re-verified to actually exercise their claimed code path | none | Verified |
| S92-09 | Orchestrator | Done | this registry, `docs/active-stage.md`, `docs/progress.md` | full independent validation gate: 90 migrations clean, `migrate verify` clean, full integration suite + `-race` green (32 packages), runtime-role adversarial suite green, all new concurrency-sensitive tests repeated 3x under `-race`, Stage 6/7 acceptance tests by exact name, both frontends re-verified unchanged, OpenAPI spec validated | none | n/a |

### Review findings and dispositions

**One P1 security finding, fixed:** the casino four-eyes self-approval
check mirrored migration 0044's original (weaker) shape rather than
0047's later-hardened shape; security reproduced, empirically, that two
`platform_admin` staff accounts with `person_id IS NULL` (the actual
`seed-admin` default) could file→approve→apply the same change end to
end, and that a suspended principal was accepted as a valid approver.
Both justifications originally given for choosing the weaker shape were
verified false at HEAD (0047's own deployment blocker no longer exists;
migration 0085 is a principal-*resolution* check, not a precedent for
self-approval strength). Fixed in migration 0089 by upgrading both
triggers to 0047's actual stricter shape (mandatory, unconditional
person-linkage comparison, active-status required on both principals),
with a new adversarial test reproducing the exact pre-fix bypass and
proving it now fails.

**One real correctness bug, found independently by `code-reviewer` and
`security`:** the casino consume-approved-request function selected the
OLDEST pending approved request for a game rather than the one whose
payload actually matched the mutation being applied — meaning two
legitimate, independently-approved requests for the same game (e.g.
"unblock DE" and, separately, "unblock FR") could deadlock each other,
with no cancel path to recover. Fixed in migration 0089 by switching to
migration 0047's own established pattern (payload-containment matching
inside the `SELECT ... FOR UPDATE`), verified by new tests proving two
disjoint approved requests now apply independently in either order. The
"add a cancel endpoint" option was evaluated and explicitly NOT built —
the payload-matching fix alone was proven, by test, to fully resolve the
practical deadlock, and building an unneeded endpoint would have violated
this stage's own "smallest correct API surface" principle.

**One real information-leak channel, distinct from (and not covered by)
the exposure-limit design's own INV-SB-EXP-2 invariant:** INV-SB-EXP-2
(no amount/threshold/scope/limit-id ever reaches a player-facing payload)
was independently confirmed by security to hold literally. The separate
problem: the DISTINCT `RejectionCategory: "exposure_limit"` value itself
was a trading-book oracle — an attacker could binary-search stake amounts
against a selection and, purely from which rejection category came back,
reconstruct remaining open capacity under a configured ceiling, without
any number ever literally appearing in a response. Fixed by collapsing
the player-facing shape of an exposure-limit decline into the same shape
a genuine risk decline already produces, while leaving the internal
category and its audit trail unchanged (staff/audit visibility of the
real reason remains correct and desired).

**One defense-in-depth gap, mirroring an already-fixed Stage 9.1 issue:**
the new `sb_jurisdiction_restrictions` table (migration 0087) had the
identical `SEC-S91-3`-class gap Stage 9.1 already fixed once for
`casino_games` in migration 0085 — its write policy checked only that a
platform-admin GUC was non-null, not that it resolved to a real staff
row. Fixed in migration 0090 by adding the identical defense-in-depth
trigger `casino_games`'s own migration-0085 fix already established as
this codebase's pattern.

**Test-integrity gaps found and fixed:** two new exposure tests were
passing a pre-cancelled `context.Context` into `pool.WithTenant`, which
fails before the callback under test ever runs — silently proving nothing
about the rollback/database-error scenarios they claimed to cover; both
repaired with genuine fault-injection mechanisms and verified (by
temporarily breaking the code under test and confirming the repaired
tests then fail). The idempotent-retry test for cumulative/exposure gates
disarmed the limit between its two calls, so it could not actually
distinguish correct short-circuit behavior from a coincidentally-matching
re-evaluation; fixed by keeping the limit armed and unchanged across both
calls. `sb_exposure_limits` had zero RLS or admin-HTTP-API test coverage
(unlike its sibling `sb_jurisdiction_restrictions`); both gaps closed
with new test files mirroring the sibling's own established pattern.

**Explicitly documented, not fixed, with full reasoning:** rung 2 of
sportsbook jurisdiction gating (`evaluateOperatingMarket`) — SPECIFIED in
full by ADR 0083 §5.3.3, deliberately NOT shipped as a stub ("no stub is
shipped" was itself the ADR's ruling, to avoid dead fail-open-shaped
code), blocked on the pre-existing HDR-J-7 (no player-scoped operating-
country determination exists anywhere in the codebase to feed it — a
platform-wide gap, not sportsbook-specific; casino has the identical gap,
also unfixed, per ADR 0083 §11). `sb_jurisdiction_restrictions`'
withdrawal action lacking four-eyes (unlike casino's analogous action) —
this was a deliberate, already-reasoned design choice in ADR 0083 §8.1
(deny-only data, platform-admin-only write already sufficient per that
section's own analysis), not something this fix round should redesign
unilaterally. The read-open RLS policy on `sb_jurisdiction_restrictions`
technically exposing `authorization_reference`/`reason_code` to any
connection — deferred, since exploiting it requires raw database access
no untrusted external party has. Several P3 findings across all five
reviews (annotators currently inert in production pending an
authenticated catalogue route; `NaN` passing a `> 0` CHECK and becoming a
zero threshold; no structured log/alert for the two new controls firing;
approvals never expire; admin-surface boilerplate duplication between the
two new tenant-scoped admin files) — documented, not fixed, per this
stage's own "document and defer minor/cosmetic issues" classification
rule.

### Human Decision Register — HDR-SB-1 (new)

Recorded verbatim in the Part B2 wave record below (ADR 0083 §8.2): who
carries the sportsbook trading-book liability, and whether an exposure
ceiling must exist before go-live. Fail-closed/fail-open default stated
honestly: the mechanism ships genuinely unarmed (zero `sb_exposure_limits`
rows anywhere), identical in kind to every other tenant-configurable risk
control on this platform. What is blocked until answered is sportsbook
production go-live under a platform-carries-the-book model, not anything
in this stage's own implementation.

### Explicitly NOT this stage's to build (confirmed absent)

B2B/partner/retail work of any kind; any undocumented external
sportsbook/casino/payment/KYC provider integration; any answer to
HDR-J-6/7/8/9 or HDR-M-1/2; rung 2 of sportsbook jurisdiction gating
(blocked on HDR-J-7, specified not implemented); any redesign of
`PlayerJurisdictionResult`, jurisdiction precedence, `OperatingCountryPolicy`,
licence ceiling, tenant/brand/operation policy, `internal/risk`'s core
evaluation engine, or the wallet/ledger canonical lock order beyond the
two ADR-0082 amendments this stage's own design required; four-eyes
governance for `sb_jurisdiction_restrictions`' withdrawal action (a
deliberate, already-reasoned design choice, not a gap); a cancel endpoint
for casino change requests (evaluated and proven unnecessary by test); any
production database cutover, cloud provider selection, backup
infrastructure, or production credential action.

### Detailed wave record — Part C (sportsbook specialist, S92-04)

Written by the implementing wave itself at landing time; retained
verbatim as the detailed record beneath the consolidated table above.

**What this wave (Part C, `docs/decisions/0083-sportsbook-jurisdiction-
gating-and-cumulative-exposure.md`) delivered:** migration `0087`
(`sb_jurisdiction_restrictions` + `sportsbook_bets.jurisdiction_code`
snapshot column); `internal/sportsbook/jurisdiction.go` (the shared
`ResolvePlayerJurisdiction`/`loadActiveRestrictions`/
`evaluateJurisdictionRestriction` triple, INV-SB-JUR-1); `PlaceBet`'s new
composed-order steps 5-7/9/17/18 (`internal/sportsbook/orchestrator.go`);
the two catalogue availability annotators
(`internal/sportsbook/catalogue.go`); the platform-admin governance
surface for the new table (`internal/sportsbook/jurisdiction_admin.go`,
`internal/httpserver/sportsbook_jurisdiction_admin_handlers.go`, new
routes under `/v1/admin/sportsbook/jurisdiction-restrictions`); and ADR
0082 Amendment A3 (naming pre-existing exception E-3).

**Closes Stage 9.1's Item 5 disposition** ("Sportsbook catalogue
jurisdiction gating: real mechanism gap confirmed, FIX BEFORE PRODUCTION,
NOT implemented" - this same file, Stage 9.1 section) and **ADR 0047's
§3 catalogue/jurisdiction deferral** ("MUST FIX BEFORE PRODUCTION/B2B, not
now") - both are now IMPLEMENTED for rungs 1 and 3 (player jurisdiction
resolution; sportsbook catalogue restriction). Neither disposition is
fully closed: rung 2 (licence-ceiling/operating-market policy) remains
open, tracked as follows.

**New tracking item `SB-JUR-RUNG2-1`:** rung 2
(`evaluateOperatingMarket`, ADR 0083 §5.3.3) is SPECIFIED but NOT
IMPLEMENTED, BLOCKED on HDR-J-7 (no operating-country determination exists
anywhere in this codebase for a player-scoped subject - this is a
platform-wide gap, not sportsbook-specific, and casino has the identical
gap, recorded but not fixed per ADR 0083 §11). Consequence to schedule
alongside the HDR-J-7 answer, not discover reactively: on the day an
operating-country determination becomes available, every tenant serving
sportsbook must already have its `licence_country_ceilings` and
tenant-scope `operating_country_policies` rows configured, or its bets
will correctly begin failing closed the moment rung 2 is wired. Rung 2
remains blocked on the **pre-existing HDR-J-7** - this is not a new Human
Decision Register item.

**Explicitly NOT this wave's territory (confirmed untouched):** Part B
(cross-player exposure, `sb_exposure_limits`, `evaluateExposureLimits`,
orchestrator steps 10-11, ADR 0082 Amendment A2/L0.6) - a separate,
concurrently-run Stage 9.2 wave's own territory, deliberately sequenced
after this one per ADR 0083 §9.4; the `internal/risk/cumulative.go`
player-scoped cumulative map entry (a third, concurrently-run Stage 9.2
workstream in this same sandbox); casino's own four-eyes governance
(`internal/casino/catalogue_governance.go` and migration `0086`, a
fourth, concurrently-run Stage 9.2 workstream).

### Detailed wave record — Part B2 / Wave 3 (sportsbook specialist, S92-05)

Landed AFTER the Part C wave above, per ADR 0083 §9.4's own wave-split
requirement (both waves edit `PlaceBet`'s composed call order; landing
them concurrently is explicitly not fine). Written by the implementing
wave itself at landing time; retained verbatim as the detailed record.

**What this wave (Part B2, `docs/decisions/0083-sportsbook-jurisdiction-
gating-and-cumulative-exposure.md` §6.2) delivered:** migration `0088`
(`sb_exposure_limits` + `idx_sportsbook_bets_open_exposure`);
`internal/sportsbook/exposure.go` (`evaluateExposureLimits`, the class
**L0.6** advisory lock, `numericToBigInt`/`verifyTenantScopedExposureConnection`
mirroring `internal/risk`'s identical disciplines); `PlaceBet`'s composed
steps 10 (moved `computePotentialReturn`) and 11 (the exposure gate,
`internal/sportsbook/orchestrator.go`); the tenant-scoped governance
surface for the new table (`internal/sportsbook/exposure_admin.go`,
`internal/httpserver/sportsbook_exposure_admin_handlers.go`, new routes
under `/v1/admin/sportsbook/exposure-limits`, gated by the new tenant
`risk_manager`-class permissions `sportsbook_exposure_limit:manage`/`:read`);
and ADR 0082 Amendment A2 (new lock class L0.6), landed alongside Wave
2's own Amendment A3 without duplicating or conflicting with it.

**Confirms the full combined Part-B-and-Part-C composed order now matches
ADR 0083 §7.2 exactly:** (no lock) steps 1-7 jurisdiction gate → L0.4 (RG)
→ L0.5 (risk cumulative, when scoped) → **L0.6 (sportsbook exposure, when
armed) [this wave]** → L3 (`wallet_balance_projection`) → L4
(`ledger_transactions`) → L1 `sportsbook_bets` insertion wait (E-3,
pre-existing, unchanged).

**New Human Decision Register item — HDR-SB-1** (ADR 0083 §8.2, recorded
here verbatim per this wave's own directive):

> **Exact question:** For the platform's own Anjouan-licensed B2C
> sportsbook, does the platform itself carry the payout liability on
> accepted bets - a trading book it owns and must cap - or will
> sportsbook operate under a commercial arrangement in which a
> third-party provider underwrites payouts, making the
> per-event/market/selection exposure ceiling a provider-contract term
> rather than a platform-set number? If the platform carries it: what is
> the per-scope_kind, per-asset ceiling on aggregate open gross potential
> payout (§6.2.2's measure), and who owns reviewing it?
>
> **Why engineering cannot decide it:** the answer depends on a
> commercial provider contract that does not exist (ADR 0080;
> `docs/integrations/dummy-sportsbook.md` is still pending) and on the
> operator's own risk appetite and solvency position. Any number
> engineering chose would be an invented financial control, which
> CLAUDE.md forbids. Choosing "no ceiling" is not a neutral default
> either - it is a decision to accept unbounded trading-book exposure,
> which doc 09 §12 characterises as "not a bounded engineering-bug cost, a
> trading-book loss". There is no fail-safe direction engineering can
> pick.
>
> **Affected domains:** `sportsbook`, `ledger-finance` (the Liability vs
> Exposure boundary, doc 09 §1.7), `risk`, and the commercial/product
> owner.
>
> **Options:** (a) the platform carries the book => a ceiling per
> (`scope_kind`, `asset_code`) must be set, authorized and owned before
> sportsbook go-live; (b) a provider underwrites payouts => the ceiling is
> a contract term and the platform's own mechanism stays unarmed, or is
> armed only as a defence-in-depth backstop at a number the contract
> implies; (c) no sportsbook go-live until (a) or (b) is settled.
>
> **Fail-closed / fail-open default until answered, stated honestly:** no
> migration, seed or fixture creates any `sb_exposure_limits` row. With
> zero rows the exposure gate is unarmed and applies no check - genuinely
> fail-OPEN-when-unconfigured, identical to `risk_rules`,
> `casino_games.jurisdiction_blocklist` and `operating_country_policies`,
> and identical to today's behaviour, so this wave introduces no new
> permissiveness. The fail-CLOSED half is the other half: once a limit row
> exists, any error reading it, any unscannable NUMERIC, any wrongly
> scoped transaction, or any failure computing the aggregate aborts the
> bet rather than allowing it.
>
> **What is blocked until answered:** nothing in this wave's own
> implementation - the mechanism ships unarmed. What is blocked is
> **sportsbook production go-live**, which must not proceed without
> HDR-SB-1 being answered and, if (a), a configured and authorized
> ceiling.

**Explicitly NOT this wave's territory (confirmed untouched):** rung 2
(`evaluateOperatingMarket`, `SB-JUR-RUNG2-1`, still BLOCKED on HDR-J-7 -
Wave 2's own territory, unchanged here); `internal/sportsbook/
jurisdiction.go`, `jurisdiction_admin.go`, and the jurisdiction-gate
portion of `orchestrator.go`'s steps 5-9/17-18 (Wave 2, untouched except
for this wave's own steps 10-11 insertion into the same function);
`internal/risk/cumulative.go` (a separate, already-completed Stage 9.2
wave); `internal/jurisdiction`, `internal/operatingmarket` (out of
scope).

## Stage 9.3 — Staging Deployment + Real End-to-End Acceptance

Directive: "STAGE 9.3 — STAGING DEPLOYMENT + REAL END-TO-END ACCEPTANCE,"
opening with "Stage 9.2 is approved." Deploy the existing B2C and Back
Office MVPs into a real, isolated, non-production staging environment and
prove it with genuine browser/API acceptance — not a production launch.
The human explicitly directed a production-grade AWS staging design but
refused to let this session use the ambient AWS credentials present in
the container (unconfirmed account/billing ownership), so the deliverable
is a complete, independently-validated deployment package plus a
locally-run staging-equivalent stack, not a live cloud URL.

| ID | Owner | Status | Files owned | Tests | Blockers | Integration |
|---|---|---|---|---|---|---|
| S93-01 | devops | Done | `deploy/docker/{platform-api,frontend}.Dockerfile`, `nginx-spa.conf`, `README.md`, `.dockerignore` | reviewed line-by-line + hadolint (no Docker daemon in this sandbox — stated honestly, not claimed as executed) | none | Verified |
| S93-02 | devops | Done | `deploy/aws/` (10 modules: network/security/database/ecr/secrets/iam/ecs/alb/dns/observability, `environments/staging/`, `scripts/deploy.sh`, `sql/init-runtime-role.rds.sql`), `docs/decisions/0084-stage-9-3-staging-aws-architecture.md`, `docs/runbooks/stage-9-3-staging-deployment-runbook.md` | `terraform fmt -check -recursive` and `terraform validate` clean, run independently by both the implementing agent and the Orchestrator (fresh `terraform init -backend=false`) | AWS account/billing ownership not confirmed — package not applied anywhere real, by explicit human instruction | Verified |
| S93-03 | Orchestrator | Done | `internal/httpserver/cors.go`+`cors_test.go`, `internal/config/config.go` (`CORSAllowedOrigins`), `internal/httpserver/server.go`, `cmd/platform-api/main.go`, `docs/runbooks/production-configuration-checklist.md` | 6 new unit tests (empty-allowlist no-op, allowlisted/non-allowlisted origin, preflight short-circuit, Vary header, config parsing) | none | Verified |
| S93-04 | Orchestrator | Done | local staging-equivalent stack: fresh Postgres, 90 migrations + `migrate verify`, `igaming`/`igaming_runtime` role split (including the schema_migrations write-revoke, corrected mid-stage after being applied out of order once), `platform-api` under `APP_ENV=staging` | `migrate verify` clean; runtime-role privilege probes | none | n/a (infrastructure, not a deliverable file) |
| S93-05 | payments | Done | `internal/httpserver/{server.go,financial_routes.go,payment_deposit_simulation_handlers.go (new),payment_deposit_simulation_test.go (new)}`, `cmd/platform-api/main.go`, `docs/api/openapi/platform-api.yaml` | 6 tests, extended to 12 by security review (cross-tenant, body-ignored, staff-denied, race-vs-real-webhook, declined-deposit-rejection, audit-with-IP/UA/request-id) | none (flagged pre-existing `APP_ENV` fail-open risk, not fixed here — see findings) | Verified |
| S93-06 | backend | Done | `internal/httpserver/{server.go,credential_routes.go,credential_handlers.go,email_verification_dev_token_handlers.go (new),email_verification_dev_token_test.go (new)}`, `cmd/platform-api/main.go`, `internal/config/config.go`, `docs/runbooks/production-configuration-checklist.md`, `docs/api/openapi/platform-api.yaml` | 6 tests | none (flagged a real multi-replica operational gap, not fixed here — see findings) | Verified |
| S93-07 | security | Done | review only — payments seam (S93-05) | independently re-ran all 6 original tests + wrote and ran 6 more; empirically proved the `requireDepositAwaitingCallback` gap is load-bearing by driving a `Succeeded` callback into an already-`declined` intent and watching it post a real credit before the fix | 0 blocking; 1 pre-existing `APP_ENV` fail-open finding (HIGH if it ever reaches production, latent today) | n/a |
| S93-08 | security | Done | review only — account-activation seam (S93-06) | independently re-ran all 6 tests under `-race`; wrote and ran a cross-tenant adversarial probe, concurrent fetch/resend/confirm storms | 0 blocking; 1 correctness note (store staleness under concurrent resend, sequential use unaffected) + 1 operational note (multi-replica gap) + fixed one OpenAPI inaccuracy (503 documented but structurally unreachable — actual behavior is 404) | n/a |
| S93-09 | qa | Done | staging test-data seeding via real HTTP APIs only (platform admins, tenant `staging-demo` + `staging-demo-brand`, 6 staff roles across 4 approval flows, 4+ players, asset authorization, casino/sportsbook catalogue, KYC/RG/bonus/withdrawal records, isolation-check tenant `staging-tenant-b`), full B2C+Back Office acceptance checklist run via headless Chromium + curl, live discovery and diagnosis of S93-10 | every acceptance-checklist item PASS (see Stage 9.3 completion report); found and fully root-caused S93-10 rather than working around it | none remaining | n/a |
| S93-10 | Orchestrator | Done | `cmd/platform-api/main.go` (payments/casino mock provider ids: `mock` → `mock-payments`/`mock-casino`) | full repo build/vet/gofmt/unit/integration suite re-run clean; regression proven directly by qa (fresh deposit, wager, win, and the exact previously-failing rollback, now all succeed with non-colliding idempotency keys) | none | Verified |

### Review findings and dispositions

**One genuine, deterministic financial defect (S93-10), found by real
end-to-end use, fixed.** `payments.MockProvider` and `casino.
MockCasinoProvider` were both registered under the literal `provider_id`
`"mock"`, and each independently generates `provider_tx_id` via an
identical low-entropy per-instance sequential counter starting at 1.
Since the ledger's idempotency key is `(provider_id, provider_tx_id)`,
the first transaction from each domain aliased onto the same key
(`"mock:mock-1"`) — reproduced live when a funded deposit was followed by
the first-ever casino rollback in the freshly-seeded staging environment,
which failed as `internal/ledger`'s own reused-key guard correctly
refused the collision (no corruption resulted; the guard did its job, but
the operation was unusable). Fixed by re-registering the two adapters
under distinct ids (`mock-payments`/`mock-casino`) — the shape any real
deployment would have anyway, since no two real vendors share a provider
identity. `qa` (S93-09) diagnosed the exact root cause via direct
`ledger_transactions.idempotency_key` inspection rather than guessing,
and re-verified the fix by direct regression (not by trusting the fix
description).

**Two testability gaps, closed as reviewed, non-production-gated seams
mirroring the Stage 7 `CasinoPlaySimulationEnabled` precedent — never
worked around:** S93-05 (mock deposit completion) and S93-06 (account
activation). Both were found by `qa` running real acceptance flows, both
were explicitly declined as a unilateral QA-authored workaround (`qa`
correctly stopped and asked rather than building its own bypass harness
when blocked, twice), and both were implemented by the owning domain
specialist and independently security-reviewed before being accepted.

**Security findings on the two new seams, both closed or explicitly
deferred as informational/pre-existing:**
- S93-07 (payments seam): confirmed `requireDepositAwaitingCallback`'s
  pending-only gate is load-bearing, not hygiene, by empirically driving
  a `Succeeded` callback into an already-`declined` intent pre-fix and
  observing a real ledger credit post; fixed one real gap itself
  (`recordDepositSimulationAudit` lacked IP/user-agent/request-id, and a
  factually wrong doc comment claiming the audit write was best-effort
  when it in fact runs inside the same transaction as the settlement).
- S93-08 (account-activation seam): confirmed cross-tenant isolation
  holds via a fresh adversarial probe (RLS backstop, not just
  application logic); found and fixed one OpenAPI documentation defect
  (503 documented for the disabled case, but the route is structurally
  absent when disabled, so the real response is 404 — a client written
  against the old spec would poll for a response that never arrives);
  surfaced but did not fix a real operational gap (the in-memory token
  store is per-process, so it will silently fail under the staging
  Terraform's own default 2-replica ALB topology with no sticky
  sessions) — recommended fix (return the raw token from the existing
  `POST /v1/auth/email-verification/request` response instead of a
  separate stateful store) is `backend`/`architect`'s to make, not
  applied this stage.
- Both reviews independently surfaced the SAME pre-existing (Stage
  7-origin) finding: `APP_ENV` fails OPEN for all three non-production
  simulation flags (`CasinoPlaySimulationEnabled`,
  `PaymentsMockSettlementEnabled`, `AccountActivationTestSupportEnabled`)
  — an unset or mistyped value in real production would silently
  register these routes. Recorded as a remaining production blocker
  (see the Stage 9.3 completion report), not fixed unilaterally since it
  changes platform-wide config semantics and needs an `architect`/
  `config` decision.

**Explicitly NOT this stage's territory (confirmed untouched):** no
B2B/Partner/Retail/Stage 10 work; no undocumented external provider API;
no HDR-SB-1/HDR-J-7 decision made or worked around; no gambling-AUP
determination (ADR 0009 remains the open umbrella decision); no
production credential requested, created, or used (the ambient AWS
credentials present in this environment were identified and explicitly
declined per the human's own instruction); no claim of production
readiness.

## Stage 9.4 Part 1 — APP_ENV Fail-Closed Validation + Stateless Activation Seam

Directive: "STAGE 9.4 — STAGING DEPLOYMENT READINESS + AWS STAGING
DEPLOYMENT," opening with "Stage 9.3 is approved." Part 1 closes the two
issues Stage 9.3 explicitly deferred (S93-05/S93-06/S93-07/S93-08 above)
as needing a config/architecture decision rather than a unilateral fix.
Part 2 (AWS account safety verification) and Parts 3-11 (actual
provisioning) required authorized AWS credentials; the human again
declined ("produce operator instructions only"), so this table covers
Part 1 only — no AWS work was performed or attempted.

| ID | Owner | Status | Files owned | Tests | Blockers | Integration |
|---|---|---|---|---|---|---|
| S94-01 | backend | Done | `internal/config/{config.go,config_test.go}`, `cmd/platform-api/main.go` | 9 test functions (`TestLoad_UnsetAppEnvStillDefaultsToDevelopment`, `TestLoad_InvalidAppEnvRejected` — 8 subtests, `TestLoad_ValidAppEnvValuesAccepted`, `TestLoad_TestSupportEndpointsEnabledOverride`, `TestLoad_InvalidTestSupportEndpointsEnabled`, `TestLoad_ProductionWithTestSupportEndpointsEnabledFailsClosed`, `TestLoad_ProductionWithTestSupportEndpointsDefaultSucceeds`, `TestLoad_TestSupportRoutesEnabled_BothConditionsIndependentlyRequired`) | none | Verified |
| S94-02 | backend | Done | `internal/httpserver/{server.go,credential_routes.go,credential_handlers.go,email_verification_dev_token_test.go}` (rewritten), `internal/httpserver/email_verification_dev_token_handlers.go` (deleted), `docs/api/openapi/platform-api.yaml` | 6 tests (rewritten), including `TestAccountActivationDevToken_MultiReplica_RequestOnReplicaA_ConfirmOnReplicaB` (two independent `httptest.NewServer` instances sharing only `*db.Pool`) | none | Verified |
| S94-03 | security | Done | review only — S94-01/S94-02 | independently re-verified every trust-boundary claim; grepped the repo to confirm `TestSupportRoutesEnabled()` is the only call site computing the three flags | 0 blocking; fixed 1 doc-only inaccuracy itself (OpenAPI rate-limit-distinguishability claim); found the `APP_ENV=""` residual gap (see S94-05) | Verified |
| S94-04 | architect | Done | review only — S94-01/S94-02 | independently re-verified the two-layer design against `docs/architecture/`; grepped for reference-format assumptions before recommending the mock-provider fix | 0 blocking; required a new ADR (S94-06); found the `APP_ENV=""` residual gap (see S94-05); found the mock-provider multi-replica bug (see S94-07); flagged 2 doc-drift items (see S94-06/S94-08) | Verified |
| S94-05 | Orchestrator | Done | `internal/config/{config.go,config_test.go}` (`resolveAppEnv`, `TestLoad_ExplicitEmptyAppEnvRejected`) | new regression test passes; full `internal/config` suite re-run clean | none | Verified |
| S94-06 | Orchestrator | Done | `docs/decisions/0085-app-env-fail-closed-and-stateless-activation-seam.md` (new), `docs/decisions/0048-casino-play-simulation-trust-boundary.md` (amended in place, Stage-8-update pattern) | n/a (documentation) | none | n/a |
| S94-07 | Orchestrator | Done | `internal/payments/mock.go`, `internal/casino/mock.go` (`nextReference()` — `uuid.NewString()` suffix, no new dependency) | `internal/payments/...` and `internal/casino/...` full package suites re-run against real Postgres, clean; confirmed by grep that no test hardcoded the old exact reference format before changing it | none | Verified |
| S94-08 | Orchestrator | Done | `deploy/aws/modules/ecs/variables.tf` (`test_support_endpoints_enabled` doc-comment trimmed) | n/a (documentation/comment only, no `.tf` logic changed) | terraform CLI unavailable in this sandbox to re-run `fmt`/`validate`; heredoc block structure verified by inspection | n/a |
| S94-09 | Orchestrator | Done | `docs/runbooks/stage-9-4-aws-account-verification.md` (new) | n/a (documentation) | AWS account/billing ownership not confirmed — no credential supplied or used, per explicit human instruction | n/a |
| S94-10 | Orchestrator | Done | `docs/active-stage.md`, `docs/progress.md`, `docs/governance/task-registry.md` (this entry) | n/a (documentation) | none | n/a |

### Review findings and dispositions

**Both reviews (S94-03, S94-04) independently found the same residual
gap**: `APP_ENV=""` (explicit empty string, distinct from unset) still
resolved to `"development"` under the first implementation's
`getEnvDefault`-style helper, the one value Layer 1 did not reject.
Closed directly (S94-05) rather than left deferred, per the directive's
own "fix any genuine review findings in the same batch" instruction —
`resolveAppEnv()` now reads `APP_ENV` via `os.LookupEnv` directly so a
present-but-empty value is distinguishable from a genuinely absent one.

**Architect (S94-04) found a genuine, previously-undiscovered multi-replica
correctness bug on the actual financial simulation path**, not merely a
theoretical one: `internal/payments.MockProvider.nextReference()` and
`internal/casino.MockCasinoProvider.nextReference()` both minted
references from a bare per-process `seq int` counter, so two ECS/Fargate
replicas (the staging Terraform's own `desired_count=2`, per ADR 0084)
could each mint the identical `provider_reference` for their own first
transaction — colliding on `deposit_intents`' `(tenant_id, provider_id,
provider_reference)` uniqueness constraint and aliasing the ledger
idempotency key `(provider_id, provider_tx_id)`. This is squarely a
multi-replica correctness issue in scope per the directive's own
efficiency rule (FIX NOW), not a redesign of approved architecture, so it
was fixed in the same pass (S94-07) rather than merely recorded. Casino's
copy of the bug was "safe today by accident, not by construction" (per
architect's own words — nothing on the live orchestrator path currently
calls it) and was fixed for symmetry rather than left as a second
instance of the same defect shape in the codebase.

**Architect (S94-04) also found the `deploy/aws/modules/ecs/variables.tf`
doc comment on `test_support_endpoints_enabled` described a scenario (a
future production invocation of *this* module setting both flags) that
this module's own existing `app_environment` `validation` block already
structurally prevents.** Trimmed (S94-08) to describe the actual
defense-in-depth reasoning (module reuse outside this validated context,
backstopped by `config.Load()`'s own hard-fail check) instead.

**Explicitly NOT this stage's territory (confirmed untouched):** no
AWS resource of any kind created, modified, or planned against a real
account; no B2B/Partner/Retail/Stage 10 work; no HDR-SB-1/HDR-J-7
decision made or worked around; no gambling-AUP determination (ADR 0009
remains the open umbrella decision); no production credential requested,
created, or used. The pre-existing, non-9.4-caused
`IssueCredentialToken` concurrent-request race (documented in ADR 0085's
"What this decision does not do") was reconfirmed as out of scope by
both reviews and remains a separate, narrower deferred item.

## Stage 9.4 — Staging Infrastructure Hardening + Cost Optimization

Directive: "STAGE 9.4 — STAGING INFRASTRUCTURE HARDENING + COST
OPTIMIZATION" (read-only AWS credential; account 765578795051 and
eu-central-1 confirmed by the human). Repository-side only; no AWS
resource created, modified or deleted.

| ID | Owner | Status | Files owned | Tests | Blockers | Integration |
|---|---|---|---|---|---|---|
| S94-H01 | Orchestrator (devops) | Done | `deploy/aws/**` (modules, staging root, bootstrap, iam policies, scripts, sql), `deploy/docker/platform-api.Dockerfile`, `.gitignore`, `.dockerignore`, `.github/workflows/ci.yml` | 42 `terraform test` runs (mock providers) + 7 node tests + mutation checks + repo guards; read-only plan (74 / bootstrap 9); Access Analyzer 0 findings; IAM simulation 28/28 | deployment credential + bootstrap not yet authorized | Integrated (repository) |
| S94-H02 | Orchestrator | Done | `docs/decisions/0086-*.md` (new), `docs/runbooks/stage-9-4-staging-lifecycle-runbook.md` (new), amendments to ADR 0084 / Stage 9.3 runbook / Stage 9.4 verification runbook | n/a (documentation) | none | n/a |
| S94-H03 | architect | Done | review only | CHANGES REQUIRED → APPROVED WITH MINOR NOTES | none | n/a |
| S94-H04 | security | Done | review only | CHANGES REQUIRED → APPROVED WITH MINOR NOTES (2 P1 closed; audit client IP deferred as production launch gate) | none | n/a |
| S94-H05 | FinOps (general-purpose) | Done | review only | APPROVED WITH MINOR NOTES | none | n/a |
| S94-H06 | backend | Done | review only | CHANGES REQUIRED → APPROVED (TRUSTED_PROXY_COUNT premise withdrawn on AWS-doc evidence) | none | n/a |
| S94-H07 | qa | Done | review only | CHANGES REQUIRED → APPROVED (no surviving mutations) | none | n/a |
| S94-H08 | code-reviewer | Done | review only | CHANGES REQUIRED → APPROVED WITH MINOR NOTES | none | n/a |
| S94-H09 | Orchestrator | Done | `docs/progress.md`, `docs/active-stage.md`, this registry | n/a | none | n/a |

## Stage 10 Planning Gate — state reconstruction and next-stage proposal

Directive: "MASTER ORCHESTRATOR — RESUME DEVELOPMENT AFTER FINAL STAGING
ACCEPTANCE" (planning only; no implementation authorized). Staging
deployment and acceptance of `9190d5d` were executed and attested by the
human. Record: `docs/plans/stage-10-planning-gate-proposal.md`.

| ID | Owner | Status | Files owned | Tests | Blockers | Integration |
|---|---|---|---|---|---|---|
| S10P-01 | Orchestrator (Explore ×4) | Done | read-only reconstruction (governance, HDR/deferred, stage history, code reality) | n/a | none | n/a |
| S10P-02 | Orchestrator | Done | local verification at `9190d5d` (no files) | build/vet/gofmt/unit PASS; migrations up/verify/round-trip PASS; integration PASS except 6 scratch-DB packages (PASS with `CREATEDB`); lint 19 issues; CI 60/60 runs failed at "Set up job" | none | n/a |
| S10P-03 | architect | Done | review only | CHANGES REQUIRED — all findings adopted | none | n/a |
| S10P-04 | ledger-finance | Done | review only | APPROVED WITH CHANGES — all findings adopted; F-7 raised | none | n/a |
| S10P-05 | sportsbook | Done | review only | APPROVED WITH CHANGES — adopted except webhook driver (overruled, R-1) | none | n/a |
| S10P-06 | security | Done | review only | APPROVED WITH CHANGES — all adopted; `b22d5c4` static review: no material risk | none | n/a |
| S10P-07 | qa | Done | review only | CHANGES REQUIRED — all adopted | none | n/a |
| S10P-08 | devops | Done | review only | APPROVED WITH CHANGES — adopted; action-major claim contested, to verify in W0 | none | n/a |
| S10P-09 | product-owner-proxy | Done | review only | APPROVED WITH CHANGES — adopted | none | n/a |
| S10P-10 | Orchestrator | Done | `docs/plans/stage-10-planning-gate-proposal.md` (new), `docs/progress.md`, `docs/active-stage.md`, `docs/governance/project-status.md` (pointer), this registry | n/a (documentation) | none | n/a |

### Findings recorded (not fixed this gate)

| ID | Severity | Finding | Proposed disposition |
|---|---|---|---|
| F-1 | P1 | CI `build-test-lint` never executed: `.github/workflows/ci.yml:99` action `golangci-lint/golangci-lint-action` does not exist | Stage 10 W0 item 1 |
| F-2 | P1 | Scratch-database tests need `CREATEDB`; CI/dev roles are `NOCREATEDB` | W0 item 2 (`igaming_test_admin`) |
| F-3 | P3 | 19 golangci-lint issues; linter version unpinned | W0 items 1, 3 |
| F-4 | P2 | Stale records (project-status, task rows, HR-15 status) | W0 item 6 (the two misleading "Current stage" headers corrected this gate) |
| F-5 | P2 | `b22d5c4` Terraform/IAM change had no recorded review | Static reviews done this gate (no material risk); live checks W0 item 4 |
| F-6 | info | Staging acceptance human-attested only | W0 item 7 |
| F-7 | P2 | `ledger.Post` replay compares transaction type only, not amounts (`internal/ledger/ledger.go:316-326`) | W1 item 0 (`ledger-finance` audit of existing callers) |

### Next stage recommended, not authorized

Stage 10 — CI Evidence Restoration + Sportsbook Settlement Lifecycle
(proposal §8). Requires the five human approvals in proposal §V.

## Stage 10 — CI Evidence Restoration + Sportsbook Settlement Lifecycle

Approved by the human on 2026-09-25 (ADR 0087; planning-gate commit
`2355ab7`). Staging kept running and untouched. W1 may not start before
the W0 five-consecutive-green-run gate.

### W0 — CI evidence restoration

| ID | Owner | Status | Files owned | Tests | Blockers | Integration |
|---|---|---|---|---|---|---|
| S10-W0-01 | devops (Orchestrator implemented) | Done | `.github/workflows/ci.yml` (action `golangci/golangci-lint-action@v9`, linter pinned `v2.5.0`, `workflow_dispatch`, test-admin role step, guard step, integration-evidence assertion step) | CI runs below | none | Integrated |
| S10-W0-02 | qa + security (Orchestrator implemented) | Done | `internal/testsupport/scratchdb/scratchdb.go` (new), five migration-test callers, `deploy/init-test-admin-role.dev.sql` (new), `Makefile`, `deploy/docker-compose.dev.yml` | local: 3/3 full integration runs, 32 packages, 0 DB-URL skips; negative: admin URL unset → skip; owner `BYPASSRLS` → fail | none | Integrated |
| S10-W0-03 | qa (Orchestrator implemented) | Done | `internal/sportsbook/{orchestrator,jurisdiction,failure_mode_matrix}_integration_test.go`, `internal/casino/{failure_mode_matrix,stage9_concurrency}_integration_test.go` — lock-wait polling scoped to the test's own blocker (flake root cause) | as above | none | Integrated |
| S10-W0-04 | code-reviewer scope (Orchestrator implemented) | Done | 19 lint findings: `internal/{casino,payments}/mock.go`, `internal/httpserver/{kyc_handlers.go,player_surface_principal_test.go,ratelimit_routes_test.go}`, `internal/providers/httpclient/client.go`, `internal/kyc/verification_service.go`, `internal/jurisdiction/precedence_invariants_test.go`, `internal/rg/self_exclusion_policy_test.go` | `golangci-lint run ./...` v2.5.0: 0 issues | none | Integrated |
| S10-W0-05 | security + devops | **BLOCKED (isolated external dependency)** — offline part Done | `b22d5c4` re-validation | offline `deploy/aws/tests/run-static-checks.sh`: 42/42 terraform tests + 7 node tests + guards PASS; network policy parses, 3,545 bytes; static security/devops review: no material risk | live Access Analyzer `ValidatePolicy` + 30-case `simulate-deployer-policies.py`: deployer credential probed and **denied** `access-analyzer:ValidatePolicy` and `iam:SimulateCustomPolicy`; needs a credential holding those two read-only actions (the Stage 9.4 read-only verification user ran both before) | n/a |
| S10-W0-06 | Orchestrator | Done | `docs/governance/{project-status,change-control,ownership}.md`, `docs/architecture/ledger-accounting-model.md` (HR-15 implemented by migration 0082), `docs/testing/testing-strategy.md`, `docs/security/runtime-role-separation.md`, `docs/decisions/0087-*.md` | n/a (documentation) | none | n/a |
| S10-W0-R | security, devops, architect, qa, code-reviewer | Done | review only | security APPROVED WITH CHANGES (3 P3, adopted); devops APPROVED (earlier "v6" claim withdrawn); architect APPROVED WITH CONDITIONS (deferral record, ownership row, lint-debt registration — done); qa PARTIALLY IMPLEMENTED pending CI gate (P2 below tracked); code-reviewer CHANGES REQUIRED → P1 (docs claimed two controls) resolved by the security-P3 implementation, P3s fixed | none | n/a |

**W0 findings, decisions, deferrals**
- F-1 root cause confirmed: the action repository `golangci-lint/golangci-lint-action` does not exist. Upstream README "Compatibility": action v7+ required for golangci-lint v2 config; v9 requires node24. The `devops` planning-review claim that v6 sufficed was wrong and is withdrawn.
- **Record correction:** the planning gate attributed all six failing integration packages to `CREATEDB`. Five were; the sixth (`internal/sportsbook`) was an intermittent failure of `TestSportsbookJurisdiction_ConcurrentConfigurationReadIsConsistent`, root-caused to a database-wide lock-wait count seeing other packages' waits. Fixed (S10-W0-03).
- Orchestrator ruling: the scratch helper lives in a non-`_test` package (`internal/testsupport/scratchdb`) because Go cannot share `_test.go` code across packages; every file carries `//go:build integration` (never compiled into an application binary) and the CI guard enforces it.
- DEFERRED (P3, text only, no security effect): stale ALB security-group description from `b22d5c4`. Changing an `aws_security_group` `description` forces replacement, which would modify the running staging environment the human ordered untouched. Do it in the next human-authorized staging change window.
- DEFERRED (P3): integration-tagged test files are not linted in CI (`--build-tags=integration` reports 467 errcheck/unused findings).
- DEFERRED (P3, `qa` P2 downgraded by the Orchestrator with reason): `internal/jurisdiction/{evaluation_policy,tenant_licence_admin}_integration_test.go` and `internal/operatingmarket/concurrency_integration_test.go` filter lock waits by their own statement text rather than blocker pid. Those statements are unique to their packages, so parallel packages cannot produce matching waits; convert when those files are next touched.
- Expected CI skips: exactly five sportsbook rung-2 placeholder tests, `BLOCKED on HDR-J-7` (ADR 0083 §5.3.3).
- Pre-gate: run 238 (`36141525827`, commit `05e1990`) — first green `build-test-lint` in this branch's history. Not counted: the next commit changed the workflow.

**W0 gate — five consecutive green CI runs** (GitHub Actions `CI`, job `build-test-lint`; every step green; "Assert integration evidence" step fails the job on any database-URL skip and requires the scratch migration/RLS tests to PASS)

| # | Run id | Run no. | Event | Commit | gofmt / vet / lint v2.5.0 | Build | Migrations up + verify | Runtime-role narrowing | Unit (race) | Integration (race) + evidence assertion | Reversibility | Test-admin guard | Other jobs | Result |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | [36142847281](https://github.com/Diansalas/igaming-platform/actions/runs/36142847281) | #239 | push | `77a9293` | PASS / PASS / PASS (step fails on any issue) | PASS | PASS (up + verify steps green) | PASS | PASS | PASS — 32 ok / 2202 pass / 5 skip (HDR-J-7) / 0 fail; evidence assertion PASS | PASS (4 down / 4 up) | PASS | frontend ×2, frontend-image ×2, infrastructure: PASS | **GREEN** |
| 2 | [36142996408](https://github.com/Diansalas/igaming-platform/actions/runs/36142996408) | #240 | push | `012d99f` | PASS / PASS / PASS (step fails on any issue) | PASS | PASS (up + verify steps green) | PASS | PASS | PASS — 32 ok / 2202 pass / 5 skip (HDR-J-7) / 0 fail; evidence assertion PASS | PASS (4 down / 4 up) | PASS | frontend ×2, frontend-image ×2, infrastructure: PASS | **GREEN** |
| 3 | [36143224956](https://github.com/Diansalas/igaming-platform/actions/runs/36143224956) | #241 | push | `6c9b9f3` | PASS / PASS / PASS (step fails on any issue) | PASS | PASS (up + verify steps green) | PASS | PASS | PASS — 32 ok / 2202 pass / 5 skip (HDR-J-7) / 0 fail; evidence assertion PASS | PASS (4 down / 4 up) | PASS | frontend ×2, frontend-image ×2, infrastructure: PASS | **GREEN** |
| 4 | [36143564397](https://github.com/Diansalas/igaming-platform/actions/runs/36143564397) | #242 | workflow_dispatch | `6c9b9f3` | PASS / PASS / PASS (step fails on any issue) | PASS | PASS (up + verify steps green) | PASS | PASS | PASS — 32 ok / 2202 pass / 5 skip (HDR-J-7) / 0 fail; evidence assertion PASS | PASS (4 down / 4 up) | PASS | frontend ×2, frontend-image ×2, infrastructure: PASS | **GREEN** |
| 5 | [36143929805](https://github.com/Diansalas/igaming-platform/actions/runs/36143929805) | #243 | workflow_dispatch | `6c9b9f3` | PASS / PASS / PASS (step fails on any issue) | PASS | PASS (up + verify steps green) | PASS | PASS | PASS — 32 ok / 2202 pass / 5 skip (HDR-J-7) / 0 fail; evidence assertion PASS | PASS (4 down / 4 up) | PASS | frontend ×2, frontend-image ×2, infrastructure: PASS | **GREEN** |

**W0 gate: PASSED — five consecutive green `build-test-lint` runs (#239–#243), 2026-09-25.** Each row's integration counts and reversibility output come from that run's own CI log (the "Assert integration evidence" step's printed totals), not from local runs. The 5 skips are the sportsbook rung-2 placeholders `BLOCKED on HDR-J-7`. "Security checks" in the approved row definition = the test-admin guard step, the runtime-role narrowing + probe, and the RLS/adversarial suites inside the integration step. Later runs on documentation commits continue to be monitored.

### W1 — Sportsbook settlement lifecycle (in-house MOCK mode, ADR 0088)

| ID | Owner | Status | Files owned | Tests | Blockers | Integration |
|---|---|---|---|---|---|---|
| S10-W1-00 | ledger-finance | Done | F-7 audit `docs/governance/stage-10-f7-ledger-replay-audit.md` (class C found); remediation `36616f1` (`internal/ledger/replay.go`, casino/payments/sportsbook callers, ADR 0020 amendment) | ledger L1–L11 + a replay test per caller | none | Integrated |
| S10-W1-01 | ledger-finance (Orchestrator implemented) | Done | `migrations/0091_sportsbook_settlement.{up,down}.sql` | T-1/T-2/deny/RLS/down-refusal + 24 SQL-branch tests | none | Integrated (`eb3912f`) |
| S10-W1-02 | ledger-finance | Done | `internal/ledger/{ledger,lockorder}.go` (types, `LockProjectionsForPostings`) | lockorder multipost/canonical-order tests | none | Integrated |
| S10-W1-03 | sportsbook (Orchestrator implemented) | Done | `internal/sportsbook/settlement*.go` | scenario/decision/OB-1/concurrency/fault/readpath/sole-writer suites | none | Integrated |
| S10-W1-04 | risk | Done | `internal/risk/cumulative.go` (INV-SB-CUM-1), netting + OI-5 pin tests | `TestSportsbookCumulative_EveryADR0088NettingRow` | none | Integrated (`49d6fda`) |
| S10-W1-05 | ledger-finance | Done | `internal/reconciliation/sportsbook_settlement.go`, `statement/`, `sportsbook/mock.go` (MOCK statement) | clean flows, drift per kind, divergent statement | none | Integrated (`f72d864`) |
| S10-W1-06 | backend + security | Done | auth permission/principal, route + handler, apierror codes, config/main, OpenAPI, read surfaces | HTTP flow/alert/audit/integrity tests | none | Integrated (`f72d864`, `dac94be`) |
| S10-W1-07 | frontend + backoffice | Done | b2c bet history, back office bet grid (read-only) | vitest suites + builds | none | Integrated (`38e4875`) |
| S10-W1-08 | devops | Done | `deploy/init-app-role.sql`, CI narrowing step, runtime-role doc §6 | runtime-role probe | none | Integrated (`d26b3b9`) |
| S10-W1-09 | architect | Done | ADR 0088 §13 amendments + ADR 0082 A4 | n/a | none | Integrated (`0d943fd`) |
| S10-W1-R | security, code-reviewer, ledger-finance, qa | Done | reviews + fixes | mutation pass (sportsbook 92.44%, ledger 92.00%), SQL branch checklist | none | Integrated (`dac94be`, `a318c10`, `312db16`) |
| PAY-REV-1 | payments (review: ledger-finance, architect) | **Superseded — implemented in Stage 10.1 (see below)** | `internal/payments/orchestrator.go` deposit reversal; migration (partial unique index) | regression per probe shape | needs human-authorized stage | n/a |
| SB-T1-XMIN | ledger-finance | **Superseded — implemented in Stage 10.1 (see below)** | T-1 composed-void causation → `pg_xact_status` (new migration) before any savepoint-using driver | pinned by `TestDBConstraints_T1_ComposedVoidCausation_*` | none | n/a |

**W1 CI evidence:** every W1 commit green — #247 `eb3912f`, #251 `0d943fd`, #252 `36616f1`, #253 `f72d864`, #254–#256 (docs), #257 `a318c10`, #258 `dac94be`, #259 `312db16` (run 36162444009: all 20 `build-test-lint` steps incl. "Assert integration evidence" and reversibility; other jobs green). Local replay of the full CI on a fresh database: lint 0 issues, 3/3 integration runs 32 packages, reversibility PASS.

**Stage 10: COMPLETE** — see `docs/governance/stage-10-completion-report.md`.

## Stage 10.1 — PAY-REV-1 + SB-T1-XMIN + PAY-WH-TENANT-1 — APPROVED 2026-09-26 (ADR 0090 ACCEPTED; PAY-WH-TENANT-1 added by the human) — IMPLEMENTED; stopped at the staging-deployment gate

Planning report: `docs/plans/stage-10.1-planning-gate-proposal.md`; stage definition ADR 0090 (ACCEPTED 2026-09-26); specialist record `docs/plans/stage-10.1-planning/`; completion report `docs/governance/stage-10.1-completion-report.md`.

| ID | Owner | Status | Scope | Tests | Blockers | Integration |
|---|---|---|---|---|---|---|
| PAY-REV-1 | payments (+ ledger-finance for `ledger.Post` routing) | **IMPLEMENTED** (`e9e0ad8`, fixes `3f67ac5`, `009c6d0`, `2704bdd`) | L2 lock + re-check; migration 0092 tenant-leading `deposit_reversal` partial unique index; `ErrReversalAlreadyExists` (key-first classification); 409 + allow-listed alert + separately committed denial audit naming intent/original/rejected reference/existing reversal | plan §J #1–#12 + defect repro (pre-fix: 2 reversals posted) + index-order test (fails on pre-fix Post) | none | Integrated |
| SB-T1-XMIN | sportsbook (+ ledger-finance review) | **IMPLEMENTED** (`e9e0ad8`, `2cb3600`) | migration 0093 body-only fail-closed `pg_xact_status` check, epoch anchored to `pg_current_xact_id()` (deviation from ruling R-2, RATIFIED by architect and ledger-finance) | plan §J #13–#19 + F4 status-check test (mutation-killed) | none; residuals SB-T1-XMIN-STRADDLE (P3, deferred) | Integrated |
| PAY-WH-TENANT-1 | payments + security | **IMPLEMENTED — MOCK resolver only** (`250828b`, tests `505a311`, fixes `3f67ac5`); security re-verification CLEARED; real resolver NOT IMPLEMENTED (launch-blocking for any real PSP) | route tenant selects the single per-(tenant, provider) credential; HMAC over v1 prefix + tenant + provider + key id + raw body; verify before any parse/read/lock/write; uniform 401; ADR 0022 §3 amendment (contract points 1–7), ADR 0019 wording (ledger-finance concurrence given) | QA plan paper 16 incl. T8/T11a/T12/T13; pre-fix cross-tenant tombstone evidence | real `WebhookCredentialResolver` + secret store (future, human-authorized); PAYWH-TS-1 | Integrated |
| KYC-WH-1 | identity-compliance + security | **Superseded — resolved by ADR 0091, implemented in Stage 10.2 (see Stage 10.2 section)** (status note 2026-09-26). Original status: **Registered — High, pre-existing (present at staging commit `9190d5d`); needs a human scope ruling; NOT fixed in Stage 10.1** | KYC webhook: literal HMAC constant in `cmd/platform-api/main.go`, mock KYC provider + route registered with no environment gate, `provider_reference` returned to the player ⇒ a player can forge an "approved" KYC callback. No payment/withdrawal path consults KYC status today (immediate harm: false compliance record; latent bypass). Verification: `docs/plans/stage-10.1-planning/12-kyc-wh-1-verification.md` | — | human scope ruling | n/a |
| CAS-WH-TENANT-1 | casino + security | **Superseded — resolved by ADR 0091, implemented in Stage 10.2 (see Stage 10.2 section)** (status note 2026-09-26). Original status: Registered — Medium, pre-existing; outside 10.1 | casino webhook tenant from URL slug; per-process key; no tenant in signature | — | — | n/a |
| PAYWH-BRAND-1 / PAYWH-RL-1 / PAYWH-TS-1 | payments + security | Registered — deferred | webhook capability check has no brand scoping; webhook rate limiting; signed-timestamp replay window | — | — | n/a |
| CI-FLAKE-281 | qa + devops | **Open — not root-caused** | one intermittent integration-step failure in CI run #281 (`a2a0981`, documentation-only commit); same code green in #280/#282 and 6/6 local runs; failing test not identifiable (log tail window, proxy-blocked download); failure annotations added to CI (`444e6e1`) | next occurrence names the test | — | n/a |
| LEDGER-REV-UNIQ | ledger-finance | Deferred | cross-type, amount-aware "one reversal per original" | — | — | n/a |
| REV-UNIQ-CASINO | casino + ledger-finance | Deferred (P3) | unique index for `casino_rollback` (lock already exists) | — | data check | n/a |
| API-DOC-PAYWH | backend | IMPLEMENTED | payments webhook path documented in `docs/api/openapi/platform-api.yaml` (headers, `PaymentsMockCallback` schema, 200/400/401/404/409/500/503) | `TestOpenAPI_PaymentsWebhook_ContractMatchesHandler` (structural check; no full JSON-Schema conformance test - no verified YAML-parsing dependency in go.sum, per task constraint not to add one) | — | n/a |
| AI-ARCH-FUTURE | architect (+ security, identity-compliance, bonus-engine, ledger-finance, risk, qa) | **Future requirement — NOT IMPLEMENTED** | ADR 0089 boundary: agents read/propose/simulate; deterministic core validates and executes | ADR 0089 §9 invariants I-1..I-6 when built | future human-authorized stage; future human decisions in ADR 0089 §8 | n/a |

## Stage 10.2 — Webhook trust hardening — APPROVED 2026-09-26 (ADR 0091) — IMPLEMENTED (MOCK providers); stopped at the deployment gate (`docs/governance/stage-10.2-completion-report.md`)

| ID | Owner | Status | Scope | Tests | Blockers | Integration |
|---|---|---|---|---|---|---|
| KYC-WH-1 | identity-compliance + security (+ backend, architect, qa) | **IMPLEMENTED — MOCK provider only** (Stage 10.2) | committed secret removed (per-process random master, G7 guard test); mock provider/resolver/route only with `TestSupportRoutesEnabled()` (404 / 503 otherwise); `provider_reference` staff-only; tenant-bound, verify-first under ADR 0022 §3 points 1–9; forward-only CAS status with same-tx audit; 204. Real KYC vendor/resolver NOT IMPLEMENTED. Staging `9190d5d` still exposed until the human-authorized refresh | K1–K16 (`docs/plans/stage-10.2-planning/08-webhook-test-traceability.md`) | — | n/a |
| CAS-WH-TENANT-1 | casino + security (+ backend, architect, qa) | **IMPLEMENTED — MOCK provider only** (Stage 10.2) | per-(tenant, provider) credential bound into the signing input; zero statements before verification; mock resolver only with test support (401 otherwise); money path unchanged (ledger-finance verified); cross-tenant callback has no effect in either tenant | C1–C14 + C7 cross-tenant capture (08) | CAS-CAP-ROLLBACK-1 before any real resolver | n/a |
| CI-FLAKE-281 | devops + qa | **Investigated — not reproduced; open pending recurrence** (not blocking) | most likely fixed 30 s ceiling on Argon2-heavy Stage 9 concurrent-login tests under runner contention (`docs/plans/stage-10.2-planning/02-ci-flake-281-investigation.md`); diagnostics added: per-test annotations (`444e6e1`) + full log artifact on failure (`b9f842c`) | 3 local reproduction runs (all green) | — | n/a |
| PAYWH-GATE-1 | payments + backend (+ architect) | **IMPLEMENTED — MOCK resolver** (Stage 10.2, ruling J9; payments tests unedited) | payments MOCK webhook resolver wired only when `TestSupportRoutesEnabled()` via `cmd/platform-api/wiring.go` `mockProviderWiring`; prod / test-support-off → nil resolver → 401 `no_resolver`. Commit `fe44a54` | `cmd/platform-api/wiring_test.go` | — | n/a |
| MOCK-ADAPTER-PROD-1 | architect + devops (+ payments, casino) | Registered — follow-up, not 10.2 scope | the mock payments and mock casino adapters remain registered in production for initiation, catalogue and launch (resolvers are gated, so no callback verifies and no money moves); remove or gate before production launch. Pre-launch checklist item (architect review `docs/plans/stage-10.2-planning/07-review-architect-db.md` §3; ruling J16) | — | pre-launch | n/a |
| CAS-CAP-ROLLBACK-1 | casino + ledger-finance | Registered — follow-up, not 10.2 scope; **HARD PRE-CONDITION for wiring any real casino webhook resolver or going live with a real aggregator** (ledger-finance condition, Stage 10.2) | A disabled casino capability (or `supports_rollback=false`) returns 503 to a *verified* callback before settlement. Scope per ledger-finance review (docs/plans/stage-10.2-planning/10-review-ledger-finance.md §5): (a) a verified rollback of a posted bet leaves the stake debited; (b) a verified win for a posted bet is withheld; (c) a rollback of an unseen original writes **no tombstone**, so a late original arriving after re-enable posts unreversed (breaks the late-arrival guarantee for that window); (d) `supports_rollback` defaults false with no CHECK tying it to `supports_bet` (migration 0035:112); (e) no casino provider reconciliation exists to detect any of this; (f) callbacks load the tenant-wide capability row (`brandID = uuid.Nil`), so brand-only capability rows 503 every callback. Diverges from ADR 0022 §3 "status governs routing only". Pre-existing (ADR 0025 review P1); no ledger invariant violated (nothing is written). Recorded in ADR 0025 Stage 10.2 amendment (architect review §4; ruling J16). ledger-finance position: capability/status gates new exposure (bets) only, never settlement of existing exposure; an unseen-original rollback always writes its tombstone. | — | ledger-finance ruling given (see §5 of the review) | n/a |
| WH-VENDOR-SCHEME-1 | architect | Registered — pre-condition for the first real adapter in any domain (not 10.2 scope) | Final-review finding K12/L8 (`docs/plans/stage-10.2-planning/01-webhook-trust-design.md` §K, `11-review-code.md` L8): `internal/httpserver`'s shared `webhookPreamble` and every domain's `Orchestrator.ReceiveCallback` hard-require the platform-defined MOCK `Scheme.ParseHeaders` wire format (fixed header names, `v1=<64 lowercase hex>` signature, `mock-v1`-shaped key ids) before the adapter's own `HandleCallback`/`Verify` ever runs. A real vendor's own header names or signature encoding cannot survive that parse and would 401 before reaching adapter-level verification. Header parsing must become an adapter/`Scheme` capability, not a shared hard-coded preamble step, before payments, KYC or casino can wire in a first real adapter. Recorded in ADR 0022 §3 Stage 10.2 amendment. | — | none (design work, no code change in 10.2) | n/a |
| KYC-REASON-BOUND-1 | identity-compliance | Registered — follow-up, not 10.2 scope | Security final review F-7 (`docs/plans/stage-10.2-planning/09-review-security-final.md`): the verified sender's `reason` string (up to ~256 KiB, pre-existing, unbounded length/charset) reaches both `kyc_verifications.reason`/audit metadata (DB) and the player-facing verification response (`internal/kyc/mock_provider.go:216`, `internal/httpserver/kyc_handlers.go:81-82`). ADR 0028 intends a short machine-readable code. When the first real KYC adapter lands, bound `reason` (length and charset) in the adapter's normalisation before it reaches the DB or the player. | — | none (no real adapter yet) | n/a |

### Records hygiene 2026-09-26

Records-only entries from the Stage 10.3 planning reconciliation (`docs/plans/stage-10.3-planning/00-roadmap-reconciliation.md` "Discrepancies" items 9–11). No code, migration or deployment change.

| ID | Owner | Status | Scope | Sources | Blockers | Integration |
|---|---|---|---|---|---|---|
| ACC-EVIDENCE-1 | Orchestrator + devops (human supplies results) | **Open — STAGING REQUIRED; to be produced for the single future governed staging deployment** | Planning finding F-6 / Stage 10 W0 item 7: a staging acceptance-evidence checklist (test IDs, no credentials) was planned for `docs/runbooks/stage-9-4-staging-lifecycle-runbook.md` but has no W0 registry row, no mention in the Stage 10 completion report, and no checklist in the runbook. The Stage 9.4 acceptance remains human-attested only. | `docs/plans/stage-10-planning-gate-proposal.md` §2.4 F-6, §8.H W0 item 7, §10 risk 10; `docs/progress.md` "Stage 9.4 — AWS staging deployment + acceptance (human-executed)"; `docs/governance/stage-10-completion-report.md` | next governed staging deployment | n/a |
| STAGING-9.4-VERIFY-1 | devops + security | **Unrecorded — results not in repository; re-run at the next governed staging deployment (STAGING REQUIRED)** | Stage 9.4 runbook §12 "First real apply" verifications items 1–10 and the §5 temporary multi-replica test. The runbook says to record results; none are recorded. No claim is made here that they passed or failed. | `docs/runbooks/stage-9-4-staging-lifecycle-runbook.md` §5, §12; `docs/progress.md` "Stage 9.4 — AWS staging deployment + acceptance (human-executed)" | next governed staging deployment | n/a |
| STAGE-NAMING-1 | human (architect records the decision) | **Open — needs a recorded human decision** | `MASTER-BUILD-PROMPT.md` stage map not revised via a recorded decision; stages 8–10.x added by stage-definition ADRs 0087/0090/0091; needs a recorded decision (human) to revise the master stage map. (`MASTER-BUILD-PROMPT.md` defines Stages 0–7 and says it is revised only through a recorded decision; Stages 8–9.4 have stage ADRs such as 0080/0084/0086 but none of decision type "stage definition".) | `MASTER-BUILD-PROMPT.md` "Stages"; `docs/decisions/0087-*.md`, `0090-*.md`, `0091-*.md`; `docs/governance/stage-10-completion-report.md` header | human decision | n/a |

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

## Stage 10.3 — Real Provider Trust & Casino Financial Readiness — AUTHORIZED 2026-09-26 (ADR 0092) — IN PROGRESS

Proposal: `docs/plans/stage-10.3-planning-gate-proposal.md`. The human authorization and rulings
HD-10.3-1..4 are recorded in its §22 and in
`docs/decisions/0092-stage-10-3-definition-real-provider-trust-and-casino-financial-readiness.md`.
The credential model is in ADR 0093. Gate 10.3-W0 PASSED; gate 10.3-W1 PASSED 2026-09-26
(`docs/plans/stage-10.3-planning/05-gate-log.md`). W2 is next.
"Approved — W<n> pending" means authorized, with implementation not yet started.

| ID | Owner | Status | Scope | Wave |
|---|---|---|---|---|
| STAGE-10.3-W0 | architect (+ orchestrator) | Done — docs only; gate 10.3-W0 PASSED | ADR 0092 (stage definition), ADR 0093 (provider credential model and secret store); Stage 10.3 amendments to ADR 0022 §3, ADR 0085 §1, ADR 0025, ADR 0082 (A6), ADR 0028; pointer in `08-casino-integration-architecture.md` §9a; this registry; `docs/active-stage.md` | W0 |
| WH-VENDOR-SCHEME-1 | architect + security (+ payments, casino, identity-compliance) | **IMPLEMENTED** (gate 10.3-W1 PASSED): platform contract, SC1–SC13 suite, **MOCK** schemes and the real-scheme contract (registration-time restrictions, S-1..S-4). Real vendor schemes **PROVIDER DEPENDENT** (each scheme type must implement `MarkProductionEligible()` or production startup fails). `KeyImplicit` resolution **NOT IMPLEMENTED** until W2a (refused at registration). Domain callback-fixture hook **NOT IMPLEMENTED** (first real adapter). `code-reviewer` checklist item: see CR-CHECKLIST-HMAC-1 | per-adapter VerificationScheme, orchestrator-enforced verify, conformance suite SC1–SC13 incl. provider-specific timestamp/replay rules (ADR 0022 §3 Stage 10.3 amendment, point 10) | W1a |
| MOCK-ADAPTER-PROD-1 | architect + devops + security | **IMPLEMENTED** (gate 10.3-W1 PASSED). By design, an all-mock production binary (today's) refuses to start | synthetic marker + production startup guard (missing APP_ENV = production) | W1b |
| CAS-CAP-ROLLBACK-1 | casino + ledger-finance | **IMPLEMENTED — MOCK provider only** (gate 10.3-W1 PASSED). `ledger-finance` C1–C6, C8, C10 met; C11 met in `98a7f08`; C7 is `security`'s, whose gate review accepted credential revocation as the emergency stop; C9 → CAS-RECON-1 (W2b). ADR 0082 A6 `IMPLEMENTED` | capability gates new bets only; tombstone always; late-original named rejection; CHECK migration | W1c |
| CAS-MULTIBET-WIN-1 (G-1) | casino + ledger-finance | **IMPLEMENTED — MOCK provider only** (gate 10.3-W1 PASSED; C3 met) | two-cash-bet round returns 500 on any win; characterise then fix | W1c |
| KYC-REASON-BOUND-1 | identity-compliance | **IMPLEMENTED** (gate 10.3-W1 PASSED; identity-compliance conditions 1–2 and security S-5/S-6 met) | bounded (512 B, sanitised) staff-only raw reason; status-only for players (HD-10.3-3: no reason_code, no provider text on any player surface) | W1d |
| PROV-CRED-RESOLVER-1 | architect + security + payments | **IMPLEMENTED** (W2a `3ef18f2`; `security` code review APPROVE WITH CONDITIONS, `09-gate-w2-review-security-w2a.md`; W2A-SEC-2, the matched-`key_id` log for `KeyImplicit` verification, closed in the W2/W3 close-out). Real backends: `devfile` (development only) and `awssm` (see SECRETSTORE-AWS-1). Pending the gate 10.3-W2 record. Evidence: `evidence/w2a-mutation-kill.txt`, `evidence/w2w3-closeout-mutation-kill.txt` | provider_credential_handles (FORCE RLS, tenant-namespaced refs), resolver, four-eyes activation, admin API | W2a |
| PROV-OUTBOUND-CRED-1 | payments + security | **PARTIALLY IMPLEMENTED** (W2a; ADR 0093 "W2a implementation status"). Implemented: per-call `OutboundResolver` (own tenant transaction, committed before it returns), per-call `httpclient.Authenticator`, `DerivedTokenCache`, static `APIKeyEnvVar` removed. **Not implemented:** passing the tenant and resolved credential through the payments/casino/KYC adapter request types, and moving outbound calls out of the domain DB transaction (today `Deposit`, `Withdraw`, `QueryStatus`, `Launch`, `CreateVerification` run inside one; withdrawal submit holds a row lock across it). **LAUNCH-BLOCKING PRECONDITION (security W2A-SEC-1): no non-synthetic payments, casino or KYC adapter may be registered or make an outbound call until outbound provider calls run outside any domain DB transaction; owners architect + ledger-finance + domain specialists.** Enforced by the tripwire `TestOutboundPrecondition_EveryWiredAdapterIsSynthetic` (`cmd/platform-api/outbound_precondition_test.go`), which fails, pointing here, when the first non-synthetic adapter is wired | outbound calls carry tenant + resolved credential; per-call resolution | W2a |
| KYC-PROVIDER-SELECT-1 (O4) | identity-compliance | **IMPLEMENTED** (W2a `3ef18f2`; verified in `security` review `09-gate-w2-review-security-w2a.md` §5, mutant M24 killed): `kyc.SelectProvider` reads the tenant's active outbound KYC handle; ambiguous or none fails closed (a lone synthetic adapter only in test-support deployments). Pending the gate 10.3-W2 record | remove hard-coded Provider("mock") in kyc_handlers.go | W2a |
| CAS-RECON-1 | ledger-finance + casino | **IMPLEMENTED (code + tests), W2b; not complete until gate 10.3-W2** — `security` review of the rejection write path and the new `casino_reconciliation:read` permission PENDING; `code-reviewer`/`qa` gate PENDING. Migration 0097, `casino_consistency` C1–C7 (C3 order sub-check `NOT IMPLEMENTED`, see `reconciliation-model.md` §2.3), rejection record for all 11 classes incl. E3, E10, the five G-1 409 classes and the E9 distinct-reference case, sweep wiring, read-only admin API. Compensation mechanism (LEDGER-MANUAL-ADJ-4EYES-1) `NOT IMPLEMENTED`. Evidence: `docs/plans/stage-10.3-planning/evidence/w2b-mutation-kill.txt` | casino_consistency C1–C7 + rejection record. **W2b scope note (gate 10.3-W1, `ledger-finance` C9 and its extension):** the verified-only rejection record must capture E10 (late win after its tombstone, 409), the G-1 409 abort classes (`ErrAmbiguousMultiOriginRound`, `ErrCorrelationWalletCollision`, `ErrLockAlreadyReleased`, `ErrMixedFundingUnsupported`, `ErrBonusBetNotLocked`), and an E9 rollback naming an already-tombstoned original under a different reference (acknowledged 200 with no ledger, audit or log record of its own) — all terminal outcomes with only a log line (or nothing) today | W2b |
| CAS-RECON-STMT-1 | ledger-finance + casino | **IMPLEMENTED — MOCK source; real statement PROVIDER DEPENDENT; pending gate 10.3-W3.** Migration 0098 (`cas_mock_statement_mismatch`), provider-neutral `statement.CasinoStatementSource`, `casino_statement` key + totals match (rollback/casino-tombstone pairing is a match), REPEATABLE READ enforced (`db.Pool.WithTenantSnapshot`), sweep wiring after `casino_consistency` under its own advisory lock, `?stream=` filter on the W2b read-only views, `casino.MockStatementSource` (synthetic, registered as `casino/statement_source`). Against the MOCK the match is **tautological** (it renders from the ledger it is matched against): it proves the plumbing only; detection is proven by test-only divergent sources. Real statement ingestion/storage, a real kind and the period timing window: `NOT IMPLEMENTED` (PROVIDER DEPENDENT). Evidence: `docs/plans/stage-10.3-planning/evidence/w3a-mutation-kill.txt` | casino_statement with MOCK source; real source PROVIDER DEPENDENT | W3a |
| CAS-RECON-SCALE-1 | ledger-finance + architect (+ devops) | Registered — **Medium**; must be resolved (design + ADR) before real-money multi-tenant load or any tenant with production-scale history. **NOT IMPLEMENTED**; only the test-side symptom is fixed. | `reconciliation.RunSweep` is O(active tenants x each tenant's full history) per tick, serial, in one goroutine: every tick it enumerates every active tenant and, per tenant, runs four streams (`ledger_vs_projection`, `sportsbook_settlement`, `casino_consistency`, `casino_statement`) in four transactions, each recomputing the tenant's WHOLE population (period bounds are recorded, never used as a filter - by design, see the stream file comments), and inserts 4 `reconciliation_runs` + 4 `audit_log` rows per tenant per tick even when nothing changed. Evidence (2026-09-26, local shared dev DB, ~9,035 active mostly-empty tenants): one full `RunSweep` took 67-83 s without -race (~8 ms/tenant); the reconciliation integration package took 781.8 s (hit the 10-minute default once at 623.7 s) because 7 tests each ran 1-2 full sweeps; W2b/W3a doubled the per-tenant stream count (package ~118 s before W2b). At the hourly target a tenant count in the low hundreds of thousands, or tenants with large casino histories, would overrun the interval. Test-side fix (this change): `RunSweepTenants` (same per-tenant body, restricted to named ACTIVE tenants; also the on-demand single-tenant re-run entry point for operations) - tests now sweep only their own tenants (package 781.8 s -> 20.8 s; 30.3 s with -race). Open design questions for the fix (not decided here): incremental/watermarked checks per stream (full-history recompute only on a slower cadence), bounded parallelism across tenants, skipping audit/run rows for footprint-less tenants vs. the "every attempt is observable" rule (ADR 0023), and run/audit row retention. | Stage 10.3 follow-up (after W3) |
| PROVIDER-REF-BOUND-1 | ledger-finance + casino + payments (+ security review) | **IMPLEMENTED (code + tests), PRH-REF, 2026-09-27; not closed.** Two conditions remain: `security` agreement on the value and charset rule is **PENDING**, and gate review is **PENDING**.<br>- **Bound:** `internal/providerref`, `MaxBytes` = 255 bytes (octets); valid UTF-8; no C0, DEL or C1 control character; never truncated.<br>- **Casino:** validated at the verified callback boundary before any domain statement, as a deterministic 400 with nothing written. The evidence is a log line only, carrying length and a SHA-256 prefix and never the value.<br>- **Payments:** validated at the verified callback boundary before any domain statement; deterministic 400, nothing written.<br>- **Sportsbook:** catalogue sync and settlement asset code.<br>- **Migration 0099:** 27 CHECKs covering ledger, payments, KYC, casino and sportsbook provider references and `provider_id`. The pre-flight counts per tenant without disabling RLS, fails loudly with per-column counts and never modifies a row. The down is unconditionally reversible.<br>- **Also updated:** OpenAPI maxLength; chain-tip pins 0098 → 0099; CI evidence list.<br>- **Mutation testing:** 28/28 killed.<br>- **Residuals** (design note §9): payments and KYC **outbound** adapter references are bounded by the DB CHECK only (a generic 500) until PRH-I1 / ADR 0095. Design note: `docs/plans/payment-readiness/prh-ref-provider-reference-bound.md`; evidence: `docs/plans/payment-readiness/evidence/prh-ref-mutation-kill.txt`. | Security review `10-gate-w2w3-review-security.md` R-2: provider reference columns have no length bound - migration 0097 `casino_callback_rejections` (`provider_tx_id`, `original_provider_tx_id`, `round_id`, `asset_code`) and the ledger's equivalent `ledger_transactions.provider_tx_id` (plus the payments/sportsbook reference columns). A verified sender can put a reference of several KB (up to the 1 MiB body cap) into `provider_tx_id`; that exceeds the ~2.7 KB btree tuple limit of the UNIQUE/idempotency indexes, so the INSERT fails: on the ledger side nothing is posted (a generic, retryable failure rather than a deterministic rejection), on the rejection side the evidence row is lost (logged only). Fix: one platform-wide maximum length for provider references, validated at each adapter boundary after verification (a deterministic non-retryable rejection), with a matching DB CHECK added by a later, reviewed migration (pre-flight for existing over-long rows). | Stage 10.3 follow-up |
| CODE-HYGIENE-10.3-1 | architect + security + ledger-finance | **Closed except F-2 (surfaced to `security`)**. First pass `a09f88c`; `code-reviewer` (`16-code-hygiene-review.md`, independent of the implementer) found item 4 did not do what its commit message/comments/registry row claimed (F-1, Medium) plus four Low findings (F-2..F-5); this row reflects the fix round, same commit series. **(1)** `WithTenantSnapshot`/`WithTenant` share one private `db.(*Pool).withTenantTx(ctx, tenantID, pgx.TxOptions, fn)` (`internal/db/tenant_rls.go`, `internal/db/tenant_snapshot.go`). Verified correct by the review. One accuracy correction (F-4): `WithTenantSnapshot`'s own "begin tx" failure text used to be `db: begin repeatable-read tx: %w`; it is now `db: begin tx: %w`, indistinguishable from `WithTenant`'s (no code matches on the string; operators lose the isolation-level hint in a begin-failure log line). Left as-is rather than adding a label parameter to `withTenantTx`, to avoid a second, broader change to the shared tx-scope helper in the same pass; `tenant_snapshot.go`'s doc comment ("the tenant set_config below") is fixed to point at `tenant_rls.go`. Test `TestWithTenantAndWithTenantSnapshot_SetIdenticalTenantSessionState` (`internal/db/tenant_snapshot_integration_test.go`) proves the GUC value, RLS enforcement and the isolation-level difference; its doc comment is corrected (F-5) to say what it actually checks (`app.tenant_id`, RLS on one table, `transaction_isolation`) rather than claiming it would catch any future divergence in tenant session setup - what prevents that is the shared helper, not this test. **(2)** `providercred.DerivedTokenCache` is bounded (4096 entries, FIFO eviction) with proactive rotation eviction and zeroing on both the bound and rotation paths, mutation-tested through the ACTUAL `Put` code paths (not just the shared `removeElementLocked` helper) per F-3, plus a `-race` concurrent Put/Get/rotate test. **F-2 (Low, open, owned by `security`):** `derivedEntry.token` is a raw-byte-backed local unexported type (`derivedTokenBytes`, `internal/providercred/outbound.go`), not `secretstore.Secret`, because it needs an in-place `zero()` that `Secret` doesn't have and `internal/secretstore` must not be touched here (F-POOL-1). Both `derivedTokenBytes` and `derivedEntry` itself now implement `String`/`GoString`/`Format` to redact (`[REDACTED-DERIVED-TOKEN]`) on every formatting path, including the outer-struct case Go's fmt would otherwise print (an unexported `[]byte`-kind field's raw byte values, fully reconstructable, when the STRUCT itself has no formatter - confirmed by a failing test before the entry-level formatter was added); `TestDerivedTokenCache_FormattingRedactsToken` covers `%v`/`%+v`/`%#v` of both the token and the entry, value and pointer forms. Residual, deferred to `security`: whether the cleaner long-term fix (a `secretstore.Secret.Wipe()`) should replace this cache-local type once F-POOL-1 lands - not decided here. **(4)** Fixed for real this time (F-1): the three `internal/jurisdiction/migration_0075_integration_test.go` tests now use `stagedMigrations0075`, a temp directory holding back every migration numbered above 0098 (mirroring `internal/ledger/migration_0092_integration_test.go`'s `stagedMigrations0092`/`copyMigrationFile` pattern), so `wantDown`/`wantDirty` (the exact 24-/23-element ordered lists) stay valid indefinitely instead of breaking the moment a 0099 lands - the first pass only derived the `MigrateDown` depth from the real directory, which did not fix `wantDown`/`wantDirty` themselves (F-1's exact finding). Verified: adding a throwaway `0099_*.up/down.sql` to the real migrations directory and re-running these tests still passes; a `wantDown` order mutant still fails loudly. **(6)** N-B fix (C6 partition test matches the 0097 CHECK by constraint name) verified correct by the review, unchanged. **(3), (5), (7)** judged sound/justified by the review, unchanged: casino statement aliases are used in their own file (not dead code); `awssm.NewWithSDKFake` stays in a regular file (a `_test.go`-only symbol can't be imported by `cmd/platform-api`'s untagged test); `RunSweepTenants` kept per CAS-RECON-SCALE-1. Verification this round: `gofmt`, `go vet` (default + `-tags=integration`), `golangci-lint run ./...` (v2.9.0, 0 issues), `go test -race -tags=integration -count=1` on `internal/db`, `internal/providercred`, `internal/jurisdiction`, `internal/reconciliation` all pass; full non-integration `go test ./...` passes; every new/changed assertion mutation-tested by hand, including the two rotation/bound eviction mutants F-3 predicted would survive the first pass's tests (both now caught). | Low-risk hygiene; none affects money movement or tenant isolation | F-2 (derived-token redaction type vs. a future `secretstore.Secret.Wipe()`) to `security`, coordinated with F-POOL-1; otherwise closed |
| SECRETSTORE-AWS-1 | devops + security | **PARTIALLY IMPLEMENTED.** Backend code (W3b `becc5c2`), wiring into `cmd/platform-api` (constructed only when `SECRETSTORE_BACKENDS` names `awssm` in an explicit staging/production `APP_ENV`, region from `AWS_SECRETSMANAGER_REGION`/`AWS_REGION`, refuses startup if it cannot be built, registered with the synthetic guard as `ProductionEligible`) and local SDK-fake tests: **IMPLEMENTED**. Security gate W2/W3 S-1 (credentials by allow-list: ECS container task-role provider only, source checked on every retrieval, SDK aliases refused) and S-2 (trust-root, endpoint and credential-source overrides refused; `awssm://` refs must be full secret ARNs in the Go parser; the migration 0096 CHECK still accepts bare names, and the parser refuses them at registration and resolve) and S-5 (import guards widened to `github.com/aws/`) are fixed; The wiring was re-reviewed by `security` (`11-gate-w2w3-reverify-security.md`: APPROVE WITH CONDITIONS, all prior conditions closed); N-1 fixed in `e80114b` and verified CLOSED (file 11 addendum); addendum Low L-N1a fixed at close-out (SDK retry attempts 2 -> 1, Fetcher is the only retry layer; mutation M46 killed). IAM/`deploy/`: **NOT IMPLEMENTED** (HD-10.3-2, a future human decision). Real-AWS use and drills: **STAGING REQUIRED**. Security gate W2/W3 finding N-1 (`AWS_DEFAULTS_MODE=auto` reaching EC2 IMDS during `New`; `AWS_MAX_ATTEMPTS`/`AWS_RETRY_MODE` letting ambient config change retries under the breaker) is fixed: all three env vars refused at startup, defaults mode and retry attempts (2) pinned explicitly as defence in depth (ADR 0093 W2/W3 close-out, N-1 section; evidence `evidence/w2w3-closeout-mutation-kill.txt`). Open, not decided here (HD-10.3-2): `HTTPS_PROXY` for the Secrets Manager client, IRSA/web identity as a source | awssm backend code + local fake only; IAM code EXCLUDED (HD-10.3-2 — future human decision); apply/drills STAGING REQUIRED | W3b |
| CI-FLAKE-281 | qa | **IMPLEMENTED** (W3, `qa`). Confirmed the 30s value is a pure hang/deadlock guard (`stage9AwaitAll`'s `t.Fatalf("... possible deadlock/pool exhaustion")`), never a behavioural/security assertion — safe to scale per ruling R12. Replaced the fixed `30*time.Second` at all six `stage9AwaitAll` call sites in `internal/httpserver/stage9_concurrency_integration_test.go` with `stage9Ceiling(t, n)`: calibrates one real `auth.HashPassword` call (same function/params every login/register in the file already uses) immediately before each burst, scales `n * 15x * measuredCost`, floored at the original 30s (never lowered) and capped at 3 min (so a real hang still fails in bounded time). Concurrency counts, assertions and Argon2 parameters unchanged. Verified: 3x green `-race -tags=integration` local runs; a mutation check (injected 5-minute sleep in one goroutine) still failed at ~2m17s with the original "possible deadlock/pool exhaustion" message, proving the guard still catches a genuine hang; CI runs #331-#342 on this branch show zero recurrence of a Stage-9/`stage9AwaitAll` timeout (the failures in that range are CI-331-LOCK's TRUNCATE deadlock, `govulncheck` and `golangci-lint` findings — unrelated). Full writeup: `docs/plans/stage-10.3-planning/13-ci-flake-281-disposition.md`. | 3x local `-race -tags=integration` runs + mutation-kill (injected hang) + CI #331-#342 annotation review, no recurrence | W3 |
| CAS-WIN-IDEMP-1 (F-9) | casino + ledger-finance | Registered — **Medium**; must be fixed before bonus-funded/locked casino stakes (G-6) ship | `postWin` lacks a `postBet`-style "already posted → verify match → return original" short-circuit, so a win redelivered after a rollback of its bet returns 400 (`ErrBetNotFound`, direct cash) or 409 (`ErrLockAlreadyReleased`, locked branches) instead of the original result; never pays twice (pre-existing; `ledger-finance` re-verification, ADR 0025 amendment item 9) | — |
| PAY-SB-REPLAY-AUDIT-1 | payments; sportsbook | Registered — **Low**; owners to confirm reachability | payments (`internal/payments/orchestrator.go:796`, `deposit.posted`) and sportsbook (`internal/sportsbook/orchestrator.go:484`, `sportsbook_bet.placed`) write their audit row ungated on `AlreadyPosted` (the pattern casino fixed in gate 10.3-W1); confirm an upstream short-circuit makes `AlreadyPosted` unreachable there, or gate it | — |
| CR-CHECKLIST-HMAC-1 | orchestrator (human-authorized 2026-09-26, "MASTER ORCHESTRATOR — CLOSE STAGE 10.3") | **IMPLEMENTED** | security S-3's item added to `.claude/agents/code-reviewer.md` ("Standing checklist items": signature/MAC comparison in scheme packages uses `hmac.Equal` only; new scheme packages must be covered by `internal/webhookauth/constant_time_lint_test.go`, the enforcing control) — ADR 0022 §3 amendment, conformance item 5 | — |
| BRANCH-PROTECTION-1 | human (repository admin) | Verified 2026-09-26 — **NOT ENABLED**; needs the human | GitHub API (read-only): `main` `protected=false`, `required_status_checks` off, no checks required. No CODEOWNERS existed; `.github/CODEOWNERS` added (owner `@Diansalas` for `.github/`, `deploy/`, `migrations/`, `.claude/`, `CLAUDE.md`, `MASTER-BUILD-PROMPT.md`) — it is enforced only once protection is on. Human action (repo Settings → Branches/Rulesets, `main`): require a PR, require status checks `build-test-lint`, `infrastructure`, `frontend (b2c)`, `frontend (backoffice)`, `frontend-image (b2c)`, `frontend-image (backoffice)`; require review from Code Owners; block force-push and deletion. Not changeable from this environment (repository settings, admin-only). Note: the development branch `claude/focused-wright-jw88w9` is not `main`; protection applies at merge. | before any merge to `main` |
| ACCESS-ANALYZER-CHECK-1 | human (AWS account admin) | Open — **needs the human**; not possible from this environment | Runbook §6 session-end IAM Access Analyzer check after the 2026-09-26 teardown. Re-attempted read-only at Stage 10.3 close-out: the deployer credential is denied `access-analyzer:ListAnalyzers` in eu-central-1 and us-east-1 (least privilege, by design). No IAM change made to work around it. Mitigation: all three staging IAM roles were destroyed and verified gone. See `docs/governance/staging-teardown-2026-09-26.md`. | before the next staging deployment |
| KYC-ENFORCE-1 | identity-compliance (+ payments, casino, security review) | **Status 2026-09-28 (FH-7, code-reviewer PRH-I3 re-review):** label **PARTIALLY IMPLEMENTED** confirmed accurate. The prose below under-claims and is superseded on one point: `DenyForCompliance` **is now wired** at payout T1p by PRH-I1 (`payout.go:291-311`), and a player approved then rejected before staff submit the payout is **denied** at T1p (`TestClaimForDispatch_KYCDeny_NoAttemptRowHoldReleased`, real gate); deposit gates are wired at `deposit_v2.go:202`/`drive.go:100`, payout re-claim at `payout_sweep.go:99-130`. Remaining before IMPLEMENTED: KYC-ENF-OUTAGE-1, KYC-ENF-DECISION-ROWS-1, KYC-ENF-TESTPINS-1, B7, F4/F5, LF-I3-4/5, thresholds dormant pending HDR/legal. Registered 2026-09-26 — **launch-blocking; vendor-independent**. ADR 0096 **PARTIALLY IMPLEMENTED** (PRH-I3, fix round 4, 2026-09-27): withdrawal-request gate and casino/sportsbook play gates are implemented WITH call-site tests against a real active policy, and a withdrawal deny now commits its decision+audit in the SAME transaction as the domain attempt (LF-I3-3, closed round 3). `DenyForCompliance` is implemented and tested (posting/idempotency/concurrency, exactly-once confirmed over 50 concurrent-repetition runs under `-race`, LF-I3-1 closed round 3) but **NOT WIRED** to any live payout call site, so the withdrawal-payout-dispatch backstop the row's own text names ("at minimum before payout submission") is **NOT IMPLEMENTED** in the sense that nothing calls it yet — a player approved, then rejected before staff submit the payout, is still paid today. **NOT IMPLEMENTED** for the deposit call site (PRH-I1 wires `payments.InitiateDeposit`) and the actual payout-dispatch call site (PRH-I1/ADR 0095's `ClaimForDispatch`/T1p). Four-eyes on withdrawing an ACTIVE enforcement policy (security F3) is now **IMPLEMENTED IN FULL at the database level** (migration 0103, fix round 4, DR-PRHI3-07 resolved): `kyc.WithdrawEnforcementPolicy` on an active row is refused by the database itself (a deferred constraint trigger requires a genuine, active, key-matching, non-self successor named in the same transaction), not merely by convention. See ADR 0096 §15/§16/§17/§18 for the full condition map; F4/F5, LF-I3-4/5, and B7 (§16.2) remain open and disclosed; security/ledger-finance/code-reviewer sign-off still pending on those. | Read-only trace (identity-compliance, Stage 10.3 close-out): KYC status is not consulted on any money/play path. Withdrawal request (`internal/httpserver/withdrawal_handlers.go:59-76` → `withdrawal.RequestWithdrawal`) and promotion/approval (`MoveToPendingReview`) have no KYC/risk gate — the handler comment defers it ("owned by identity-compliance, not Stage 3B scope"); deposits (`internal/payments`) and casino bet placement (`internal/casino` `postBet`) have none either; no test asserts an unverified player is blocked. `internal/kyc` (state machine, orchestrator, verified webhooks) is built and tested but not called from these paths. KYC-WH-1's "superseded" status covered only the webhook-forgery vector. CLAUDE.md: enforcement is our code, not the vendor's. Work: server-side, fail-closed gate on withdrawal (at minimum before payout submission), deposit and play thresholds, driven by per-jurisdiction configuration; threshold VALUES depend on HDR-J-6 + legal review — the mechanism and a conservative fail-closed default do not. | next stage (vendor-independent track) |
| PAYWH-TS-1 | payments + security | Deferred (remains open per §22; timestamp rules for real schemes land in W1a) | closure decided at the 10.3 completion gate on evidence; R4 "superseded" withdrawn | — |
| SUSP-TENANT-SETTLE-1 | architect + casino + ledger-finance | Decided UNCHANGED (HD-10.3-4) — existing behaviour documented in ADR 0025 amendment | suspended/closed tenant: shared webhook preamble returns uniform 401 (`tenant_inactive`) before verification for every event type; no posting/tombstone/audit; exposure may strand; change needs a new human decision | — |
| PAYWH-BRAND-1 | payments | Deferred (trigger: tenant with per-brand merchant accounts at one provider) | — | — |
| PAYWH-RL-1 | payments + devops | Deferred (pre-launch) | — | — |
| LEDGER-MANUAL-ADJ-4EYES-1 | ledger-finance + security | **2026-09-28: the K1 grant substrate is IMPLEMENTED** (CAP-GRANT-1). Governed manual adjustments themselves are **NOT IMPLEMENTED** until K2 (ADR 0100, migration 0113), which is subject to K2-G1..G4 and the ADR 0100 §19 conditions. History: **Status 2026-09-28: design PROPOSED in ADRs 0099/0100/0101 (`d83a71c`); awaiting security + ledger-finance review; NOT IMPLEMENTED.** **HUMAN DECISION DECIDED 2026-09-28** (`0098-…` §2): manual adjustments are a configurable capability (platform/tenant-scoped grant and revoke) with configurable four-eyes (initiator cannot approve; independent authorized second approver; auditable, deterministic, idempotent; no direct balance mutation); no invented threshold. Implementation: PRH-2 plan. Still blocks real-money go-live until implemented. Was: Registered — blocks real-money go-live; own later stage | four-eyes manual adjustment / mismatch resolution API | — |
| HD-PRH2-1 | **human** | **DECIDED 2026-09-28** (`0098` §5): no switch disabling mandatory four-eyes; no CLAUDE.md bypass amendment; configurable thresholds/profiles; explicit non-mandatory classification allowed; tighten-only jurisdiction/operator config. Was: **HUMAN DECISION (new, PRH-2 plan §7)** | May a policy switch four-eyes off entirely (needs a CLAUDE.md amendment), and is there a ceiling for `above_threshold`? Options, consequences and recommendation in `docs/plans/prh2-hardening-round/plan.md` §7. Blocks K2. | — |
| HD-PRH2-2 | **human** | **DECIDED 2026-09-28 — option (c), platform co-approval** (`0098` §5): tenant admins manage and request; granting any financial capability requires platform co-approval; no self-grant; no platform/out-of-tenant grants by tenant admins; extensible. Was: **HUMAN DECISION (new)** | Control over granting money-moving capabilities, given the sock-puppet fact (tenant admins can create tenant_admin/support/compliance accounts; Persons are unverified). Options (a)–(d); only (c) or (d) structurally prevent unilateral money movement. Blocks K1. | — |
| HD-PRH2-3 | **human** | **CONFIRMED 2026-09-28** (`0098` §5): no invented thresholds; configurable mechanism by operation/tenant/jurisdiction/profile/asset. Was: **CONFIRMATION requested** | Confirm that no monetary threshold is needed before implementation (architect + ledger-finance: none; per-asset rows; synthetic fixtures). | — |
| HD-PRH2-4 | **human** | **DECIDED 2026-09-28** (`0098` §5): full provider-neutral alerting with configurable routing, but no fictional recipients. Real recipients/on-call = future operational decision (**HD-PRH2-4-OPS**, open, launch-blocking for paging). Was: **HUMAN DECISION (new)** | Alert routing: destinations, severity matrix, on-call ownership, later a paging vendor. Until decided, P1s are durable but not paged; PAY-P1-MULTISUCCESS-ALERT-1 stays launch-blocking. | — |
| HD-PRH2-5 | **human** | **DECIDED 2026-09-28** (`0098` §5): tenants see the identifiable platform staff actor; the full accountability record is preserved; presentation configurable per jurisdiction. Was: **HUMAN DECISION (new)** | Whether tenants see platform staff identity on platform actions (full / pseudonymous / per contract). IP, user agent and metadata are never disclosed under any option. Interim default: pseudonymous. | — |
| HD-PRH2-6 | **human** | **DECIDED 2026-09-28 — yes, via explicit tenant-scoped grants only** (`0098` §5): never implicit by platform employment; same framework for platform-licence and own-licence tenants. Was: **HUMAN DECISION (new)** | May platform-scope capability holders move money in tenants operating under their own licence? (a)/(c) need a new platform-in-tenant RLS family. Until answered, K2/K3 refuse platform scope on tenant ledgers. Blocks K1 platform scope. | — |
| HD-PRH2-7 | **human** | **DECIDED 2026-09-28** (`0098` §5): only platform admins write or loosen the platform financial approval policy; tenants and brands may only tighten; all changes audited, effective-dated, reproducible. Was: **HUMAN DECISION (new, security S-2)** | Who may author or loosen `financial_approval_policies` at tenant/brand level. Security's recommended default: platform-only authoring, tenants may only tighten. Blocks K2. | — |
| HD-PRH2-8 | **human** | **HUMAN DECISION (new, surfaced by ADR 0100 §3.4) — non-blocking; stricter interim enforced** | Below-threshold semantics for the mandatory four-eyes class (manual adjustments, force-resolve). (a) At or below a configured threshold, one person holding the capability may execute alone; less load, but repeated small adjustments need a cumulative/velocity rule to prevent splitting. (b) Always at least one independent approver; thresholds only add approvers. **Interim: (b) is enforced by trigger** (`base_required_approvals >= 1`); switching to (a) later is a trigger change plus an ADR amendment, so K2 is not blocked. Context: CLAUDE.md "four-eyes above a configurable threshold" vs HD-PRH2-1 "four-eyes remains mandatory for the class". | — |
| HD-PRH2-4-OPS | **human / operations** | **OPEN — future operational decision; launch-blocking for paging** | The real alert recipients, on-call rota and escalation contacts for production. The platform is built to route once they are configured; nothing fictional is populated (HD-PRH2-4). | — |
| SB-CATALOGUE-IO-1 | sportsbook (review: architect, security) | **IMPLEMENTED 2026-09-28 (PRH-2 E3; merged; code review READY, conditions F1–F3/F5/F7 closed)** — the provider fetch now runs outside any transaction via `FetchCatalogue(ctx)`, and the IO-1C static guard covers sportsbook and cmd. **F6 remains a binding condition before any real sportsbook adapter:** a bounded per-call timeout. Evidence: `docs/plans/prh2-hardening-round/reviews/e3-code-review.md`; ADR 0095 §33/§33.1. Classification history: **OPEN → IN PRH-2 as workstream E3** (classified 2026-09-28 at the human's request) | `internal/sportsbook/catalogue.go:52-56` calls `provider.Catalogue()` inside the `WithPlatformService` transaction opened at startup (`cmd/platform-api/main.go:254-256`). Classification: (1) inside a DB tx: yes; (2) pooled connection held during the call: yes, and `Catalogue()` takes no ctx and returns no error, so a real adapter is neither cancellable nor able to fail cleanly; (3) financial side effects: none (catalogue tables only); (4) pool/lock risk: low today (one startup call, in-memory mock), real once a network adapter exists (connection and catalogue row locks held across network I/O; blocks startup; contends with odds reads if run periodically); (5) violates the established no-provider-I/O-with-a-connection-held rule (ADR 0094 INV-POOL / ADR 0095); (6) needs no transaction for the fetch: fetch + validate outside, upsert inside, interface `Catalogue(ctx) (CatalogueResult, error)`; (7) not retained as an exception. Before any real sportsbook adapter. |
| HANDOVER-1 | orchestrator | **OPEN — continuous (PRH-2 W0 created the index)** | `docs/HANDOVER.md`: links-only handover index, mock-vs-real matrix, secret-names inventory (names only). Every PRH-2 workstream updates the rows it materially changes (DoD). |
| CAP-GRANT-1 | architect + security + ledger-finance | **IMPLEMENTED 2026-09-28 (PRH-2 K1, merged `460a95d`; migration 0112; ADR 0099).** Scoped financial capability grants:
- **Flows:** G-T (tenant request plus platform co-approval, HD-PRH2-2 (c)) and G-P2 (a time-bounded platform-acting grant, HD-PRH2-6).
- **Acting session:** the sole setter `WithPlatformActingInTenant` (no production caller until K2), the restrictive fence on 7 tables, and C-1's exact GUC shape.
- **Grant rules:** R-12 no overlapping validity (per-key advisory lock plus overlap trigger, READ COMMITTED only); R-14 no backdating, with a clamp; the I-5 G-P2 live re-read; the three-way distinct-Person check with the `grantee_person_id` snapshot (0034-pinned).
- **Surface and tests:** per-command RLS, an HTTP API bound to the path tenant, audit content, and A-1/A-12/A-18.

**G-P1: NOT IMPLEMENTED (DEFERRED by architect ruling;** fail-closed CG010; widening `staff_users` visibility is a human + security decision). **`grant.reattested`: NOT IMPLEMENTED** (moved to STAFF-LIFECYCLE-1).

**Reviews:** security ACCEPT (re-check conditions RC-1/RC-2 closed; the orchestrator re-killed RC-1, M12 and MC-1), LF ACCEPT, code review conditions closed, architect rulings (G-P1, R-12) applied (`reviews/k1-*.md`).

**Verification (local, not CI):** build/vet/gofmt/lint 0; `migrate verify` through 0112; `-race` for db/auth/audit/identity/alerting/casino/kyc/txscope/cmd/full httpserver all ok.

**Carried to K2 (ADR 0100 §19):** K2-G1..G4 (A-18 false negatives, acting-tenant scoping pins M9/M10, use-time re-checks, locking the grant in force at `now()`), C-K1-3 and N-1/N-2. **Launch flags unchanged:** TM-7, TM-10, HD-PRH2-8. History: **Status 2026-09-28: design PROPOSED in ADRs 0099/0100/0101 (`d83a71c`); awaiting security + ledger-finance review; NOT IMPLEMENTED.** **OPEN — PRH-2 K lane** | Scoped financial capability grants on the existing RBAC with platform co-approval (HD-PRH2-2 c), explicit tenant-scoped grants for platform staff (HD-PRH2-6), policy authoring platform-only/tighten-only (HD-PRH2-7). ADRs 0099–0101. |
| STAFF-LIFECYCLE-1 | identity + security | **OPEN — before real-money K operations** (security K1-3/K1-2, review of ADR 0099) | There is no audited API to suspend staff, change a staff role, reset a password or force a first-login password change. The only staff UPDATE is the person link (`internal/identity/staff_user.go:164`). So the S-4 status/role re-checks and grant re-attestation can only be reached by direct DB changes, and the creator of a `finance` account knows its initial password. Build: audited suspend/role-change that invalidates refresh sessions, a credential reset, and a forced first-login change; plus the grant re-attestation table and view (deferred from ADR 0099 §8.3, with a `grant.reattested` audit action meanwhile). Until then, **grant revoke** is the emergency stop (runbook). | **Scope widened 2026-09-28 (ADR 0099 rev 2):** a suspend or role change must also revoke the person's capability grants in the same transaction, because a tenant-session execution cannot re-read a platform approver's staff status (ADR 0099 §7.5). Also in scope: the re-attestation table, view and p2 alert; the DB bootstrap guard refusing `platform_admin` inserts without a bootstrap GUC (security C-99-4), which needs a shared fixture helper first because 23 test files insert `platform_admin`; and the residual that a `finance` account's creator chooses its initial password **Extended 2026-09-28 (architect K1 ruling S-b):** any path that suspends a staff member, changes their role or tenant, or unlinks their Person must, in the same transaction, cancel that staff member's pending capability-grant requests (as grantee or requester) and revoke their grants (ADR 0099 §7.5 as amended). **Strengthened (security K1 review, S-b):** enforce it in the DB (a `staff_users` trigger or a guard refusing the change while unrevoked grants or pending requests exist), not only in the API. Also include a role change to `platform_admin` and staff deletion or deactivation. Pin with a test that reactivation never restores a revoked grant. **Also includes (orchestrator decision, K1 code re-review C-2):** the capability-grant re-attestation audit action `grant.reattested` (ADR 0099 §8.3), which K1 does not implement. |
| MANUAL-ADJ-LINK-1 (= LEDGER-MANUAL-ADJ-LINK-1) | ledger-finance | **OPEN — launch-blocking for the first real-money tenant; not K2-blocking** (security K2-2; ledger-finance ruling 4, review of ADRs 0099–0101) | A preventive BEFORE INSERT trigger, for **all** sessions, requiring every `manual_adjustment` ledger transaction to link an executing governed request (`executed_txid = txid_current()`). First move the **19** test files that post `manual_adjustment` as a fixture to request-backed helpers. **No test-bypass GUC in the production schema.** Meanwhile, K2 ships the static single-caller test plus a detective P1 reconciliation kind, `ledger_unlinked_manual_adjustment`. |
| PAYOUT-AMOUNT-DISPUTE-1 | payments + ledger-finance | **NOT IMPLEMENTED — before real payouts** (ledger-finance F9) | A payout attempt disputed with `amount_asset_mismatch` (the provider reported a different amount or asset, `payout.go:1118`) has no resolution path. M2 refuses it (ADR 0101): "declare paid" would post the full `wr.Amount` against contradicting evidence, and "declare not paid" would release the full hold although something was paid. Needs a partial-payout design. |
| WITHDRAWAL-REVERSAL-1 | ledger-finance + payments | **NOT IMPLEMENTED — before real payouts** (ledger-finance F11) | The proper posting for a payout declared paid but never paid (or a double payout recovered) is `withdrawal_reversed` (`TxWithdrawalReversed` exists at `ledger.go:115`; the transition is not implemented, `withdrawal.go:1492-1494`). Until it exists, a correction via `compensating_entry` restores the player but leaves `psp_clearing` misstated. The standing reconciliation kinds `pay_declared_paid_unconfirmed` and `pay_declared_not_paid_but_paid` track the residual. |
| ALERT-DELIVERY-1 | backend + devops (review: security, ledger-finance) | **I-core IMPLEMENTED 2026-09-28 (merged `74024c4`, migration 0110):** durable, provider-neutral alerting core (`internal/alerting`, 5 tables with FORCE RLS, the `alert_dispatcher` service identity, the stale-claim lease, `RaiseGuarded`/`InTx`/`RaiseDetached`, deferral under REPEATABLE READ, a log sink plus a MOCK sink, and `alert:manage`). The dispatcher is **NOT wired into `main.go`** (I-wire). No routes or recipients are seeded (HD-PRH2-4); every alert is `unrouted` until real routes exist. Real channels are PROVIDER DEPENDENT. Reviews: security ACCEPT, LF ACCEPT, code re-review conditions closed (`reviews/i-core-*.md`). Orchestrator verification (local, not CI): build/vet/gofmt/lint 0, `migrate verify` through 0110, `-race` integration for alerting/db/auth/audit/txscope/identity/kyc/casino/cmd/full httpserver all ok, after two integration test fixes (`8360bc2`). Stays launch-blocking until real recipients and a real channel exist. **OPEN — PRH-2 I lane** | Durable, provider-neutral alerting with configurable routing, delivery state, retry, escalation, dedup (ADR 0102). Supersedes the log-only P1s; PAY-P1-MULTISUCCESS-ALERT-1 closes with it except for real recipients (HD-PRH2-4-OPS). **2026-09-28, I-core reviews** (`reviews/i-core-*.md`): security and LF ACCEPT WITH CONDITIONS, code review NOT READY at `bc73c24`. The fixes are in progress in I-core. **Binding on I-wire:** (1) failure-path callers pass a detached, bounded ctx (SEC IC-5); (2) LF test 6: the casino_statement and payment_statement sites use `RaisePostCommit` with `stream:` keys, and the run and mismatch rows commit even when the raise fails persistently; (3) LF test 10: a kill-switch engage is unaffected by an injected persistent P0001, then 40P01, in the post-commit raise; (4) LF test 7 (stable `stream:` discriminators, `run_id` as an attribute); (5) LF test 9 (`-race -count=50`); (6) INVDEP1-BACKSTOP-BRANCH-TEST-1 before ADR 0102 §8 row 2. **Carried (security I-core re-review):** the migration that adds the first tenant-owned alert Kind must add the positive-then-negative exclusion matrix for `alerts_tenant_owned` (SELECT and INSERT, on `alerts` and `alert_occurrences`). **Also binding on I-wire (LF re-review B-1):** a static/AST test that every `RaiseGuarded` call sits inside an `alerting.InTx` closure. |
| ALERT-RETENTION-1 | devops + architect (review: security, ledger-finance) | **OPEN — before production launch** (ledger-finance F13 on ADR 0102) | `alert_occurrences` and `alert_deliveries` (ADR 0102, migration 0110) are append-only and grow without bound. Design retention and partitioning (for example monthly range partitions on `raised_at`/`recorded_at`), archiving partitions older than the regulatory audit horizon for **resolved** alerts only; open alerts are never pruned. Deletion only by partition detach/archive consistent with the append-only triggers, never a row DELETE. The horizon per jurisdiction is configuration, not a code constant. |
| CAS-PLAYER-REF-1 | casino (review: security) | **OPEN — before a real casino adapter** (raised by ADR 0103 drafting) | Phase-B `LaunchRequest` still sends the raw `PlayerAccountID` to the vendor (`internal/casino/types.go:547`). That defeats the opaque, provider-scoped player reference ADR 0103 introduces. Switch the launch request to the provider-scoped ref. |
| CAS-GAME-KILL-BET-1 | casino (review: security) | **OPEN — Medium, pre-existing; BLOCKS REAL-MONEY CASINO LAUNCH** (security SB-4, review of ADR 0103) | `casino_games.status` is documented as a platform-level game kill switch (`internal/casino/types.go:83-85`), but `postBet` never reads it. A game whose licence is pulled keeps accepting new bets on `consumed` sessions. Fix: `postBet` refuses bets for a non-active game. Test and mutant. Not a PRH-2 B blocker. |
| CAS-BET-REQUIRES-BOOTSTRAP-1 | casino (review: security) | **OPEN — Low; before real-money casino** (security SB-5) | Betting does not require the vendor bootstrap: `active` sessions are bet-eligible within their TTL. Once ADR 0103 (B) lands, require `consumed` for real-money bets. |
| PLAT-AUDIT-SUBJECT-1 | security + backend | **OPEN — follow-up to KS-AUDIT-TENANT-1 / ADR 0104** | Other platform writes into tenant scope should also carry `subject_tenant_id`, so tenants see them: `provider_credential_handlers.go` platform actions, and `admin_routes.go:277-286,555,660`. G1 covers the kill switch only. **Added 2026-09-28 (G1 reviews: security G1-I1, code review F-8(a)):** a platform admin renaming a tenant's staff via `PATCH /v1/admin/tenants/{t}/staff/{s}/display-name` writes `staff.display_name_changed` with `TenantID = target`, a new instance of this shape. |
| AUDIT-PRESENTATION-POLICY-1 | security + backend (+ product-owner-proxy) | **DEFERRED — future consideration** (product-owner-proxy ruling on ADR 0104 §5.3) | A platform-authored, effective-dated presentation-policy table and write API for jurisdiction or tenant overrides of the default tenant-audit presentation (HD-PRH2-5). PRH-2 G1 ships a resolver with compiled-in defaults: the identifiable actor is shown, and IP, user agent, request_id and metadata are hidden in the presentation but kept in the record. Build this when a real jurisdiction or tenant requires a non-default presentation. Purely additive. |
| RECON-RUN-FAILED-ALERT-1 | ledger-finance + backend | **OPEN — alerting follow-up** (raised by ADR 0102 drafting) | The "tenant run failed" reconciliation log lines (`reconciliation/scheduler.go:314,407,473,547,647`; `:647` already labelled P1) are not wired to durable alerts. Add a `reconciliation.run_failed` Kind in I-wire or a follow-up. |
| CAS-WIN-ANOMALY-1 | casino + risk | Registered — before real-money casino go-live | detection-only large-win alert | — |
| PROV-REVOKE-ALL-1 | security + architect | Registered — needed once two tenants share a provider | cross-tenant "revoke all handles for provider P" | — |
| KYC-HOSTED-SESSION-1 | identity-compliance | Registered (J12) | vendor short-lived hosted-KYC session token | — |
| KYC-SANCTIONS-IF-1 | identity-compliance | Registered | sanctions/PEP vendor interface (does not exist) | — |
| VERIFY-TEARDOWN-ECS-1 | devops | Registered — small follow-up (not 10.3 scope unless approved) | `verify-teardown.sh` tag sweep reports INACTIVE (deleted, non-billable) ECS cluster/services as leftovers after a successful destroy; exclude them as INACTIVE task definitions already are (`docs/governance/staging-teardown-2026-09-26.md`) | — |
| KYC-DOC-REJECTION-BOUND-1 | identity-compliance | Registered — not 10.3 scope | `kyc_documents.rejection_reason` is staff-entered, unbounded, and returned on player document routes (`internal/httpserver/kyc_handlers.go` ~306, ~337); bound + decide player visibility | — |
| DEPLOY-FPKEY-1 | devops + security | Registered — **human decision before the future staging deployment** | delivering ADR 0093's fingerprint HMAC key to AWS needs a new platform secret + execution-role secret ARN in `deploy/`; excluded from 10.3 by HD-10.3-2; STAGING REQUIRED | — |
| CI-331-LOCK | qa + devops | **Resolved** (Stage 10.3, `13b6407`) | CI #331 intermittent failure: `TRUNCATE … CASCADE` immutability tests deadlocked (40P01, 1s default deadlock_timeout) with concurrent packages on the shared CI DB; moved to isolated scratch DBs (3 tests); repo-wide sweep in `docs/plans/stage-10.3-planning/08-ci-331-lock-contention.md`; CI-FLAKE-281 not reclassified (inconclusive) | live repro + isolation proof | — |
| CI-342-STOREOUTAGE | qa + security + devops | **Resolved** (test + CI change; bounds unchanged) | CI #342 (1.27 s, unrelated-query bound) and #347 (2.66 s, held-long count) failures of `TestStoreOutage_DoesNotPinPool`: CPU scheduling delay from every package's -race binary sharing the runner. Slack widening 400->900 ms (`c5f05a9`) rejected by the orchestrator (weakens a timing check); 64-conn test pool (`9df5869`) REJECTED by security (`15-ci-342-security-ruling.md` ruling A: removes pool-starvation coverage); reverted to the shared 20-conn fixture pool with all bounds unchanged and richer failure diagnostics. CI runs the test alone in its own blocking step (`25a3537`; ruling B ACCEPT WITH CONDITIONS: blocking, no retries, name guard, nothing else joins without its own ruling). CI annotation step now captures detail lines printed before `--- FAIL` (`fea3b8d`). Details `14-ci-342-store-outage-test.md`. | W3 close-out |
| F-POOL-1 | architect + security | **CLOSED WITH CONDITIONS 2026-09-27** (security file 17 + addendum: A root cause/measurement ACCEPTED, B `FailureStreakTTL` ACCEPTED with B1 ADR wording — done in `8fec18f`, C K2/K3 CLOSED). **Final condition K1 OUTSTANDING:** the next GitHub CI run of the timing lane must pass all 8 tests by name, no reruns; any failure re-opens F-POOL-1. K1 cannot currently be evaluated: GitHub Actions jobs are not starting (account billing, CI-BILLING-1). Local evidence: timing lane 24/24 (security, `taskset -c 0-1`) and 40/40 (architect). Launch-blocking until K1 holds | Root-cause fix implemented (ADR 0094). Security review 17 closed F-POOL-1 with conditions K1–K3.<br>- **K1 failed on CI #360.** Both `TestStoreOutage_DoesNotPinPool` variants reported "more than 4 resolves over 250 ms" with `maxConcurrent` = 4.<br>- **Root cause** (reproduced with `taskset`, instrumented): the test's clock began before pool acquisition. The cold fixture pool dials its connections during the burst, which costs 125–270 ms per caller under `-race` on 1–2 CPUs. The store-side span always had exactly 4 callers over 400 ms.<br>- **Fix:** the clock now starts when the caller's pre-verification transaction runs on its connection. That is the pre-ADR span plus the commit. All bounds and pool sizes are unchanged. The argument is in the ADR 0094 implementation record, "Fix round".<br>- **K2** (S-1..S-3), **S-4** and **K3** (S-5, S-6) are applied, as are code review R-1..R-4 and ledger-finance LF-R1.<br>- Mutations: `docs/plans/stage-10.3-planning/evidence/f-pool-1-mutation-kill.txt`. | before real-provider go-live (security ruling on K1 + green CI) |
| F-POOL-2 | architect + security (+ `ledger-finance` for the dual-write aspect) | **Status 2026-09-28 (architect FH-7): still OPEN (launch-blocking per domain); payments part now IMPLEMENTED against MOCK adapters.** The "Payments part still NOT IMPLEMENTED" sentence below is superseded. ADR 0095 §29 defines the durable states. The three-phase A/B/C rule was verified in code for the deposit (`deposit_v2.go`, `drive.go`), sweeper (`sweeper.go`), callback (`receipt.go`; no provider call in the domain tx) and payout (`payout.go`, `payout_sweep.go`, `withdrawal_handlers.go`) paths. The gate `callProvider` refuses under `txscope.Held`. `rv-fh3-payments.md` APPROVE confirms §29 is consistent. Remaining before CLOSED: PROV-OUTBOUND-CRED-1-LEGACY-PATH (the legacy `InitiateDeposit` path still calls `provider.Deposit`/`QueryStatus` inside the caller's tx, with a test-only caller); LF-C1 per real adapter (PRH-I1-MANIFEST-2); a casino code-review re-review, not on record; KYC-SUBMIT-OUTBOX-1 before a real KYC adapter; and deposit-response reference validation (FH7-05, `rv-fh7-architect-final.md`). Original status: Registered 2026-09-26. **Pool pinning: Medium. Dual-write sub-finding: High (P1-class) once reachable** (ledger-finance review 19, §6). **Launch-blocking per domain: must be designed and fixed before that domain's first non-MOCK adapter is wired** (payments, casino, KYC). The design is ADR 0095, ACCEPTED. **Casino part IMPLEMENTED 2026-09-27 (PRH-I2, code + tests):** `casino.LaunchGame`'s phase A commits the launch-session intent in its own transaction, `HealthStatus`/`Launch` run with no transaction held, and phase C (a second, separate, ctx-independent-and-bounded transaction) revokes the session on failure/ambiguity or audits success - see ADR 0095 §15.1.1/§15.1.2 implementation and fix records and `docs/plans/payment-readiness/evidence/prh-i2-casino-mutation-kill.txt`. Independent code review (`rv-prh-i2-casino-code-review.md`) found one blocker (R1: phase C skipped on a cancelled request ctx) and security review (`rv-prh-i2-casino-security.md`) APPROVE WITH CONDITIONS C1-C4; both rounds addressed in the §15.1.2 fix record (ctx-independent bounded phase-C transactions with logging; `postBet` now enforces session status/expiry at bet placement, closing the "orphaned session accepts a bet forever" gap C1/I3 depended on; `CallContext` redaction tested; outbound resolver chosen by adapter kind via `OutboundKindSplitResolver`, C3). Re-review not yet re-run against the fix. **KYC part IMPLEMENTED 2026-09-27 (PRH-I2, code + tests):** `kyc.CreateVerification`'s phase A commits the orphan `unverified` row (no `provider_reference`) in its own transaction, the provider call runs with no transaction held, and phase C (a second, separate, ctx-independent-and-bounded transaction) applies the result under CAS; `kyc.SubmitVerification` is the identical split for document submission, with an ambiguous/timeout/transport-error result leaving `kyc_verifications.status` unchanged (IC condition 2) - see ADR 0095 §15.2/§15.3 and `docs/plans/payment-readiness/evidence/prh-i2-kyc-mutation-kill.txt` (11/11 killed). Not yet gate-reviewed (code/security review pending, mirroring casino's own two-round pattern). **Payments part still NOT IMPLEMENTED** (PRH-I1 not started) - not reachable today, because the payments adapter is an in-process MOCK | This is the same mechanism and cross-tenant impact as F-POOL-1, but for **vendor HTTP I/O inside the tenant transaction**. Scope per security C11 / ADR 0094 security review (7):<br>- `payments.attemptDeposit` (`provider.Deposit`, plus `QueryStatus` in `resolveAmbiguous`, repeated up to `MaxCascadeDepth`);<br>- casino launch (`provider.Launch` after `CreateLaunchSession(ctx, tx, …)`);<br>- `kyc.CreateVerification` / `SubmitVerification`;<br>- `HealthStatus` in payments and casino;<br>- the webhook path's cascade (`handleDecline` → `attemptDeposit`) and `resolveAmbiguous` inside the webhook domain transaction.<br>The txscope guard makes outbound-credential resolution inside such a transaction fail closed. It does **not** cover an adapter that resolves before the transaction or keeps a pre-loaded client.<br>**Dual-write hazard (High once reachable).** If the domain transaction rolls back after the PSP accepted, the PSP holds a request whose intent was never committed. The player can be charged and not credited, or charged twice on a retry.<br>**Binding design constraints LF-C2 (review 19, §6):**<br>1. The intent or attempt is committed in a `submitting` state before any external call, and the call runs with no transaction held.<br>2. The external idempotency key is deterministic and persisted, derived from the committed intent or attempt.<br>3. An explicit compare-and-set state machine, with every transition audited.<br>4. The callback path resolves the intent by merchant reference as well as provider reference, and accepts a `submitting` intent.<br>5. A crash-recovery sweeper uses `QueryStatus` with no transaction held. It never auto-declines and never cascades on an unknown outcome.<br>6. Cascades go through an outbox, never inline in a webhook transaction.<br>7. The ledger posting never spans external I/O.<br>8. The required failure-injection tests end with SUM(debits)==SUM(credits) and projection==rebuild.<br>9. The same claim-before-call rule applies to withdrawal payout dispatch.<br>**Related condition LF-C1 (next real-provider integration gate; `payments`/`casino` + `ledger-finance`):** record each non-MOCK adapter's redelivery semantics for 401 and 5xx. If a vendor treats 401 as terminal for win, rollback or reversal, then either (a) with `security`'s agreement return a retryable 5xx for a post-verification re-check DB error, or (b) have daily provider reconciliation cover provider-settled, platform-unposted events with a P1 alert.<br>Do not start before the orchestrator schedules it. | before each domain's first non-MOCK adapter (next real-provider integration gate) |
| CI-BILLING-1 | human (GitHub account owner) | Open 2026-09-27 — **needs the human; blocks all CI evidence** | GitHub Actions runs #365 (`49ee4ec`) and #366 (`8fec18f`): every job failed without starting — "The job was not started because recent account payments have failed or your spending limit needs to be increased. Please check the 'Billing & plans' section in your settings." Not a code failure. Consequences: F-POOL-1's final condition K1 (first green CI run of the fixed timing lane) cannot be satisfied; runs from #365 onward have no CI evidence. Runs #360–#364 (`0cbb574`..`c7b68bb`, all carrying the pre-K1-fix test) ran and each failed only the isolated timing step, consistent with the K1 root cause (measurement span included pool connection establishment); local CI replays are recorded as substitutes, never as CI. Human action: fix billing / spending limit, then re-run CI on the branch head. | before any gate that requires CI |
| GO-TOOLCHAIN-VULN-1 | devops | **Resolved** (CI run #337, job 108475467193, `govulncheck` step; symbol-level reachable findings) | Standard-library findings against the go1.25.0 toolchain the module built with (no `toolchain` line): GO-2026-6218 (net/url), GO-2026-6090/5856 (crypto/tls), GO-2026-6089 (net/http), GO-2026-6088 (encoding/xml), GO-2026-5972 (encoding/asn1), GO-2026-5039 (net/textproto), GO-2026-5037 (crypto/x509), GO-2026-5026 (x/net/idna via net/http), GO-2026-4971 (net), GO-2026-4970 (os); all fixed in go1.25.10-go1.25.13. Module findings: GO-2026-5970 (`golang.org/x/text` v0.29.0, fixed v0.39.0) and GO-2026-5426 (`go.opentelemetry.io/otel/sdk` v1.38.0, fixed v1.43.0). **Toolchain decision:** go1.27.0/1.27.1 exist upstream (confirmed via `go list -m -versions golang.org/toolchain`), so under Go's two-most-recent-major-release support policy Go 1.25 is out of support; moved to **go1.26.8** (latest 1.26.x patch, matching CI run #337's toolchain download), not the latest 1.25.x (1.25.14). Pinned in all four places: `go.mod` (`go 1.26.0` + `toolchain go1.26.8`), CI `setup-go` (unchanged `go-version-file: go.mod`, which now resolves to go1.26.8 via the toolchain line), `deploy/docker/platform-api.Dockerfile` (`golang:1.26.8-alpine`, exact tag; repo convention uses tags not digests for Go/Node/nginx base images, so no digest added), and no other `1.25` references were found repo-wide (`grep -rn "1\.25" --include=*.yml --include=Dockerfile* --include=*.mod --include=Makefile .`) besides this row and the historical W2/W3 security-review doc recording the original finding. Module fixes: `golang.org/x/text` -> v0.42.0, all `go.opentelemetry.io/otel/*` -> v1.46.0 (one consistent version across `otel`/`metric`/`trace`/`sdk`/`sdk/metric`/`exporters/stdout/*`), `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp` -> v0.71.0 (paired contrib release); `go mod tidy` re-run clean. `govulncheck` itself pinned `@latest` -> `@v1.8.0` (security S-4, `docs/plans/stage-10.3-planning/10-gate-w2w3-review-security.md`); v1.8.0 requires go >= 1.26, satisfied by the new toolchain. Knock-on fix: `golangci-lint-action` version bumped v2.5.0 -> v2.9.0 because the published binaries of v2.5.0-v2.8.0 are built with go1.25.x and refuse to run against a go1.26-targeted module (CI run #340 failed on v2.6.2); v2.9.0 is the first release binary built with go1.26. Could not run `govulncheck` locally post-fix to confirm zero findings (vuln.go.dev is egress-blocked in the dev sandbox by design); verification is CI run on the pushed commit. AWS SDK versions left unchanged (no finding required a bump). | Stage 10.3 follow-up |

## Payment Readiness & Provider-Independent Hardening (PRH) — AUTHORIZED 2026-09-27 — IN PROGRESS

Human instruction "MASTER ORCHESTRATOR — PAYMENT READINESS & PROVIDER-INDEPENDENT HARDENING" (2026-09-27).
Scope: F-POOL-1 finalize (CI-gated), F-POOL-2, payment financial safety, KYC-ENFORCE-1, PAYWH-RL-1,
PROVIDER-REF-BOUND-1, PROV-OUTBOUND-CRED-1, payment provider contract foundation, payment reconciliation
(MOCK source), capability/kill-switch foundation (payment scope). Out of scope: any real vendor, AWS
(staging OFF, no Terraform/IAM), Bonus Wave 4, AI agents, every recorded human decision. Final gate
requires green GitHub CI (CI-BILLING-1 is an external prerequisite; never bypassed or compensated).

Allocations (to avoid collisions between parallel specialists): ADR 0095 payments/provider-I/O
boundary (F-POOL-2 + PROV-OUTBOUND-CRED-1 + payment contract + capability/kill switch + payment
reconciliation), ADR 0096 KYC enforcement, ADR 0097 webhook admission/rate limiting. Migrations:
0099 provider-reference bound; 0100 KYC enforcement policy + decision audit (re-allocated 2026-09-27,
was 0101 — KYC is implemented first and migrations must be gap-free); 0101 payment attempts / provider-operation
state (was 0100); 0102 payment statement reconciliation (MOCK); **0103 KYC policy supersession (security F3 DB-level closure, DR-PRHI3-07; allocated 2026-09-27)**; **0104 payment-statement DB CHECKs for merchant_reference/asset_code (PRH-I5 security C2 DB half; allocated 2026-09-27)**; **0105 capability manifest + kill switch** (moved again from 0104, then originally from 0103 because the kill switch is not built yet and migrations must be gap-free; originally swapped 2026-09-27 so PRH-I5 can proceed in parallel with the payments cutover without a migration gap; ADR 0095 text still says 0102 kill switch / 0103 reconciliation — read as swapped); **0106 payment_attempts/payment_provider_events platform-GUC RLS hardening + kill-switch L1–L4 (kill-switch fix round 1b, renumbered from 0107 2026-09-27)**; **0107 INV-DEP-1 backstops (PAY-DOUBLE-CREDIT-1: `payment_attempts_one_succeeded_deposit_per_intent`, `ledger_transactions_one_deposit_per_intent`, `payment_attempts_guard()` T13d, `pay_captured_unposted` kind; ADR 0095 §28.8; allocated 2026-09-27)**. Authoritative map: ADR 0095 header (revision 4)
(MOCK). A migration number is re-allocated only by the Orchestrator.

| ID | Owner (reviewers) | Status | Scope | Depends on |
|---|---|---|---|---|
| PRH-0 | orchestrator | In progress | Governance: this section, logs, ADR/migration allocation, roadmap/status docs, completion report | — |
| PRH-FPOOL1 | architect + security | Waiting on CI-BILLING-1 | Finalize F-POOL-1: the next GitHub CI run must pass the isolated timing lane (8 tests by name, no reruns) and full CI; then mark CLOSED. No redesign without new evidence | CI-BILLING-1 |
| PRH-D1 | architect (payments, ledger-finance, security) | **Status 2026-09-28: DONE (design).** ADR 0095 is ACCEPTED (revision 4, plus the §32 FH-7 record); its implementation is tracked under PRH-I1/I2/I5. Original status: Not started | ADR 0095: provider-I/O transaction boundary (F-POOL-2) — payments priority; casino launch + KYC analysis; PROV-OUTBOUND-CRED-1; provider-neutral payment contract; capability + kill switch (payment scope); payment reconciliation MOCK design | — |
| PRH-D2 | identity-compliance (security, payments, casino) | **Status 2026-09-28: DONE (design).** ADR 0096 is ACCEPTED; its implementation is tracked under PRH-I3 (PARTIALLY IMPLEMENTED). Original status: Not started | Reconnaissance of ALL money/value movement paths + ADR 0096 KYC enforcement boundary (no invented thresholds/jurisdiction rules) | — |
| PRH-D3 | security (devops, payments) | **Status 2026-09-28: DONE (design).** The security review is complete: re-verification #3 returned APPROVE (`rv-prh-i4-security.md` §8). ADR 0097 is ACCEPTED — PARTIALLY IMPLEMENTED (PRH-I4-METRICS-1, WEBHOOK-EDGE-1). Original status: Design complete — ADR 0097 ACCEPTED (IMPLEMENTED pending security review); implemented as PRH-I4 | ADR 0097 webhook admission/rate limiting (ordering: admission → verification → binding → parsing → domain) | — |
| PRH-REF | ledger-finance (casino, payments, security) | **Status 2026-09-28 (FH-7, verification only):** the "PENDING" below is stale. - `security` AGREED the value and charset rule, and its review returned APPROVE WITH CONDITIONS (`prh-ref-provider-reference-bound.md` §8/§10). - `code-reviewer` returned READY WITH FOLLOW-UPS (`rv-prh-ref-code-review.md`). - Security C1 is PARTIALLY IMPLEMENTED: payout adapter-response references are validated (`payout.go`, `ErrorClassProviderRefInvalid`), but deposit `Deposit`/`QueryStatus` response references are not (FH7-05). - C2 is PRH-REF-C2; C3 is PRH-REF-C3 (CI). Original status: **IMPLEMENTED (code + tests); security agreement + review PENDING** — see the PROVIDER-REF-BOUND-1 row and `docs/plans/payment-readiness/prh-ref-provider-reference-bound.md` | PROVIDER-REF-BOUND-1: platform maximum, boundary validation, migration 0099 with pre-flight, no truncation | — |
| PRH-I1 | payments + ledger-finance (security, code-reviewer, qa) | **Status 2026-09-28 (architect FH-7): PARTIALLY IMPLEMENTED (MOCK adapters only).** The "NOT IMPLEMENTED, Phase 2" text below is superseded: - phase 2 is merged (`4e04f4e`) and KS-DEP-T2-T3-1 is fixed (`2da7548`); - the callback cutover has `ledger-finance` APPROVE WITH CONDITIONS and `code-reviewer` READY WITH CONDITIONS, with their conditions closed (`rv-fh3-code-review.md`, `f43025c`); - the payout path has FH-6 round 2 confirmed by `ledger-finance` and `code-reviewer`, and `security` APPROVE WITH CONDITIONS; - INV-DEP-1 is done (PAY-DOUBLE-CREDIT-1 CLOSED). Open before "complete": PROV-OUTBOUND-CRED-1-LEGACY-PATH; PRH-I1-MANIFEST-1..4; LF95-R1 automatic re-drive; alert delivery, including PAY-P1-MULTISUCCESS-ALERT-1; KS-AUDIT-TENANT-1; PAY-SEC-LAUNCH-1; a `code-reviewer` record for the phase-2 fix round and `2da7548` (FH7-07); and deposit-response reference validation (FH7-05). Evidence: `rv-fh7-architect-final.md`. Original status: In progress — re-dispatched 2026-09-27 as ordered steps a→i, round 2 same day. Round 1: kill switch (migration 0105, §10.2-§10.4) **IMPLEMENTED** (schema, guard triggers, RLS, Go service layer `internal/payments/killswitch.go`, claim-statement predicate wired into T2/T1+T2/T1p/T12/cascade-T1); mutation-kill MX17/MX19/MX20/MX21/MX25 5/5 killed. Round 2 (Phase 1, files other agents were not touching): §10.5 staff/platform HTTP routes **IMPLEMENTED** (`internal/httpserver/payments_kill_switch_handlers.go`, one route family reusing `canActOnTenant` rather than the ADR's literal two-family split - documented deviation, functionally equivalent) with OpenAPI + contract test + 10 HTTP integration tests (authz matrix, tenant isolation, audit rows); permissions `payments_kill_switch:engage/release/read` (`internal/auth/permission.go`, platform_admin + tenant_admin); S95-C7 engage alert **IMPLEMENTED** as a structured log line (`logKillSwitchEngagedAlert`, mirrors `logSettlementIntegrityAlert` - no dedicated alert subsystem exists); §10.1 manifest gets its first two real, enforced, tested fields (`SupportsRefund`, `CallbackEchoesMerchantReference` - LF95-C5), the rest deferred as PRH-I1-MANIFEST-1..4 rather than left unread; §16.2 item 15 reflection test **IMPLEMENTED** for payments+casino+KYC (`internal/testsupport/credentialscan`, did not exist anywhere before this round). See ADR 0095 §10.6 implementation record (round 2) and `docs/plans/payment-readiness/evidence/prh-i1-mutation-kill.txt`. **NOT IMPLEMENTED, Phase 2 (gated on the payout/callback-cutover agents merging first):** the orchestrator wiring from a claim refusal to a labelled T3 `kill_switch` decline/payout hold; PROV-OUTBOUND-CRED-1's payments kind-split resolver (`gate.go` + call sites, timeout/outage → T5). **Fix round 1b (2026-09-27), responding to `docs/plans/payment-readiness/rv-prh-i1-killswitch-security.md` (verdict CHANGES REQUIRED):** H1 CLOSED (permanent, mutation-killed coverage for T1+T2/T1p/T12/cascade-T1 predicates, `killswitch_claim_predicate_coverage_test.go`); M1 CLOSED via migration 0106 (`payment_attempts`/`payment_provider_events` RLS now also requires the platform GUC unset, closing the mixed-GUC fail-open; migration 0106 also folds in L1-L4); M2 CLOSED (`provider_scope` validated against the orchestrator registry, 400 on mismatch); M3 CLOSED (audit before/after + platform-takeover/KS-L6 detection); M4 test added + **KS-AUDIT-TENANT-1 registered** as an architect decision (schema fix not built, launch-blocking); M5 substantially closed (atomic.Pointer/chan/unsafe.Pointer/pointer-receiver-Authenticator handling, package-wide static checks, and a new `cmd/platform-api` test scanning the ACTUALLY-wired `buildProviderBundle` registries) but relabelled **PARTIALLY IMPLEMENTED** per the review's own instruction, still launch-blocking; M6 CLOSED for K10/K11/K14 (mutation-killed), K19 could not be demonstrated as a live bypass (RLS on `staff_users` already provides an independent, sufficient layer - documented, not silently claimed); L1-L4, L6, L8 CLOSED; L5 partially closed (error classification + 5xx-vs-409, no separately-audited refusal row yet); L7 relabelled ("event IMPLEMENTED, delivery NOT IMPLEMENTED", runbook entry added, launch-blocking for real delivery). See ADR 0095 §10.7. **Fix round 1c (2026-09-27), responding to the security re-verification's N1 finding** (`rv-prh-i1-killswitch-security.md`, "Re-verification 1"): confirms every §10.7 behaviour fix correct on the LIVE migration-0106 code; N1 CLOSED — `migration_0105_integration_test.go`, `killswitch_integration_test.go` and `killswitch_claim_predicate_coverage_test.go` all built their scratch DB pinned at exactly 0105 via `migration0105Scratch`, exercising the SUPERSEDED trigger bodies migration 0106 `CREATE OR REPLACE`s, so the round 1b "K10/K11/K14 killed" claim did not hold for live code (corrected in ADR 0095 §10.7); `migration0105Scratch` now migrates to HEAD (same mechanism as `migration0106Scratch`), and K5/K10/K11/K12/K13/K14/K19 are all now mutation-killed against the live bodies (two new tests close K19's broader/suspended-staff cases: `TestMigration0105_N1_K19_RandomUUIDInPlatformGUCIsRefused`, `TestMigration0105_N1_K19_SuspendedPlatformStaffUUIDIsRefused`); L5 CLOSED (`recordKillSwitchRefusalAudit` writes a denied-audit row in a transaction separate from the failed mutation, for both refusal classes, mutation-killed); L7/K21 CLOSED (HTTP-level assertion that a real engage call emits the alert log line, mutation-killed; delivery itself remains launch-blocking, unchanged); M5 residual #1 CLOSED (dropped the `rv.CanAddr()` gate on the pointer-receiver `Authenticator` check, since a value-typed adapter in a map/interface — `paymentsAdapters()`'s own shape — is never addressable; M5 residual #2, raw string/[]byte secrets, remains inherent and PARTIALLY IMPLEMENTED stands). See ADR 0095 §10.8. | Implement ADR 0095 payments (intents/state machine/outbox/sweeper/contract/capability/kill switch/outbound creds) | PRH-D1, PRH-REF |
| PRH-I2 | casino + identity-compliance | **Status 2026-09-28 (later, FH-7): casino code re-review NOT READY** (`rv-prh-i2-casino-code-review.md` "Re-review (FH-7)"): all prior findings closed, but the `2c00e10` postBet expiry change introduced HIGH regression **CAS-SESSION-EXPIRY-1** (consumed real-money sessions refuse bets 2 minutes after launch); fix in progress. **Status 2026-09-28 (FH-7, verification only):** - KYC part: "not yet gate-reviewed" below is stale. `security` re-verification 5 (`rv-prh-i2-kyc-security.md`) found nothing launch-blocking; KYC-SUBMIT-OUTBOX-1 still blocks a real adapter. `code-reviewer` re-review 2 (`rv-prh-i2-kyc-code-review.md`) was READY for N1–N6, and N-1b was then closed by `security` re-verification 3. - Casino part: `security` APPROVE WITH CONDITIONS (`rv-prh-i2-casino-security.md`). `code-reviewer` NOT READY (R1), with no re-review record in `docs/plans/payment-readiness/`, so it is still pending. Original status: **Casino part IMPLEMENTED 2026-09-27 (code + tests); code review R1 and security C1-C4 rework completed same day, re-review pending**; **KYC part IMPLEMENTED 2026-09-27 (code + tests); not yet gate-reviewed** | Implement ADR 0095 casino-launch/KYC-submission boundary (scope per ADR). Casino: `casino.LaunchGame` two-phase split (§15.1), ctx-independent bounded phase-C transactions (R1/C1), `postBet` session status/expiry enforcement (item 2/I3), `CallContext` redaction tests (C2), adapter-kind-keyed outbound resolver (C3), pool/outbound-resolver wiring, mutation evidence `docs/plans/payment-readiness/evidence/prh-i2-casino-mutation-kill.txt` (14/14 killed, 1 equivalent). KYC: `kyc.CreateVerification`/`kyc.SubmitVerification` phase A/B/C split (§15.2/§15.3), KYC's own `CallContext`/`OutboundCredentialResolver`/`MockOutboundResolver`/`OutboundKindSplitResolver` (`internal/kyc/callcontext.go`, mirrors casino's), `ErrVerificationReferenceUnknown` retryable-5xx-never-200 callback-race disposition (IC-Q1), IC condition 2's ambiguous/timeout-leaves-status-unchanged rule, content-derived `SubmitVerification` idempotency key, pool/outbound-resolver wiring (`httpserver.Deps.KYCOutboundCredentials`, `cmd/platform-api` `kycOutboundCredentials()`/`KYCOutboundResolver`), mutation evidence `docs/plans/payment-readiness/evidence/prh-i2-kyc-mutation-kill.txt` (11/11 killed, 0 equivalent) | PRH-D1 |
| PRH-I3 | identity-compliance (+ payments, casino, withdrawal owners via dependency requests) | **Status 2026-09-28 (FH-7, verification only): still PARTIALLY IMPLEMENTED.** - The "deposit gate ... and payout gate ... are NOT wired" sentence below is superseded: PRH-I1 wires them (`kycGate.EvaluateDeposit` in `deposit_v2.go` phase A and the `drive.go` cascade T2; `evaluatePayoutGate` in `payout.go` T1p and `payout_sweep.go`). - `code-reviewer` NOT READY (`rv-prh-i3-code-review.md`) with no re-review record. - `security` APPROVE WITH CONDITIONS; `ledger-finance` SIGN-OFF WITH CONDITIONS. - The ADR 0096 header was corrected the same day (`rv-fh7-architect-final.md`). Original status: **PARTIALLY IMPLEMENTED** (fix round 4, 2026-09-27) — round 4 closed security F3 IN FULL at the database level via migration 0103 (allocated by the orchestrator; DR-PRHI3-07 resolved): `kyc.WithdrawEnforcementPolicy` on an ACTIVE row is now refused by the database itself (a deferred, commit-time constraint trigger requires a genuine active, key-matching, non-self successor named in the same transaction), superseding round 3's Go-level-only `SupersedeEnforcementPolicy` mitigation; migration 0100's own up.sql remains untouched/checksummed. Round 4 also fixed a robustness gap two reviewers found in `internal/kyc`'s own migration-0100 tests (round 3's step-count fix was insufficient once 0102 landed with its own down-guard) by adopting `internal/ledger`'s `stagedMigrations0092` pattern (hold back every migration above 0100 dynamically), verified on a freshly migrated-to-head database. See ADR 0096 §18 for the full round-4 record and mutation-kill evidence; §16.2's other still-open items (LF-I3-4/5, security F4/F5, B7) remain open and untouched this round | Implemented ADR 0096 KYC enforcement: `internal/kyc.EvaluateEnforcement`, migration 0100, withdrawal-request gate + `DenyForCompliance` (tested, unwired), casino/sportsbook play gates (now with active-policy call-site tests), staff read API (`GET /v1/admin/kyc/enforcement-decisions`, `PermKYCEnforcementDecisionRead`, cursor-tested), dormancy query (`ListDormantJurisdictionTriggers`, now includes `play`, no HTTP route), and migration 0103 (security F3 DB-level closure). Fixed chain-tip pin tests in internal/bonus, internal/jurisdiction, internal/operatingmarket, and (twice, round 3 and round 4) `internal/kyc`'s own 0100 migration tests, to derive rather than hard-code/undercount the migration chain tip. The deposit gate (InitiateDeposit phase A) and payout gate (T1p/ClaimForDispatch) are NOT wired — left for PRH-I1 to call the exported service, per ADR 0096 §15/§16/§17/§18 implementation record | PRH-D2 |
| PRH-I4 | security + backend | **PARTIALLY IMPLEMENTED — security's third re-verification (`rv-prh-i4-security.md` §8, "re-verification #3") returned a plain APPROVE: all conditions (C1–C4, T4, T6, C3/L8, L1–L3(partial), T10, T11) are CLOSED, with no remaining security condition on PRH-I4/PAYWH-RL-1 within that review's scope. §8 also found round 5's "Info I7 done" claim (§21.10) overstated (the KYC row-unchanged check was vacuous — no real verification was ever seeded, so it could never fail); round 6 fixes this (§21.11: both the T6g KYC subtest and the C1a KYC test now seed a real kyc_verifications row and assert it is genuinely unchanged, verified falsifiable via a reverted production mutation) and corrects the T6g header comment (Info I8). Stays PARTIALLY IMPLEMENTED only because PRH-I4-METRICS-1 (§8 OTel metrics) is NOT IMPLEMENTED and WEBHOOK-EDGE-1 (R1) is still open pre-launch — neither is a condition of this ADR's own admission control** — see ADR 0097 §21.11 implementation record and `docs/plans/payment-readiness/evidence/prh-i4-mutation-kill.txt` (round 6 section) | Implement ADR 0097 webhook rate limiting | PRH-D3 |
| PRH-I5 | ledger-finance (payments, security, code-reviewer, qa) | **Status 2026-09-28 (later, FH-7): code-reviewer re-review READY** (`rv-prh-i5-code-review.md` "Re-review (FH-7)"): F1–F5 closed, F6 accepted, RM2–RM6 killed; new Low RECON-PAYOUT-LIVE-TEST-1. Label unchanged: IMPLEMENTED against a MOCK source; real PSP statement matching PROVIDER DEPENDENT. **Status 2026-09-28 (FH-7, verification only):** still IMPLEMENTED against a MOCK source. - `security` APPROVE with C1/C2 (`rv-prh-i5-security.md`). - `code-reviewer` NOT READY (`rv-prh-i5-code-review.md`). The fix round is recorded in ADR 0095 §12.7.1, but no code-review re-review record exists. - `pay_captured_unposted` (§28.9) was added by FH-3/FH-3c and reviewed in `rv-fh3-ledger.md`/`rv-fh3-code-review.md`. Original status: **IMPLEMENTED against a MOCK source** (2026-09-27, `16c69b7`; migration **0102** after the allocation swap), pending security/code/qa review; real PSP statement matching PROVIDER DEPENDENT; LF95-R1 automatic re-drive NOT IMPLEMENTED (deferred; operator T17 → sweeper path proven); see ADR 0095 §12.7 | Payment reconciliation stream with MOCK source | PRH-I1 (a–d) |
| PRH-REV | all mandatory reviewers | **Status 2026-09-28 (later, FH-7): the four missing code re-reviews are now recorded** — kill-switch phase-2 fix round + `2da7548` **READY** (`rv-prh-i1-killswitch-phase2-code-review.md`); PRH-I5 **READY** (`rv-prh-i5-code-review.md`); PRH-I3 **READY WITH CONDITIONS** for PARTIALLY IMPLEMENTED (`rv-prh-i3-code-review.md`); PRH-I2 casino **NOT READY** — new HIGH regression CAS-SESSION-EXPIRY-1 (`rv-prh-i2-casino-code-review.md`), fix in progress. Ledger-finance FH-5 C2/C3 confirmed CLOSED (`rv-prh-i1-callback-ledger.md`). Orchestrator synthesis: `docs/governance/payment-readiness-completion-report.md`. **Status 2026-09-28: IN PROGRESS.** The review records and verdicts are listed in `rv-fh7-architect-final.md` (gate item N). Missing re-review records: `code-reviewer` re-reviews of PRH-I2 casino (R1), PRH-I3 and PRH-I5; a `code-reviewer` record for the kill-switch phase-2 fix round and `2da7548`. The orchestrator synthesis is not started. Original status: Not started | Architect, security, payments, ledger-finance, backend, QA, code-reviewer reviews; Orchestrator synthesis | PRH-I* |
| PRH-GATE | orchestrator | Not started | Local full CI replay; GitHub CI green (needs CI-BILLING-1); docs/governance/payment-readiness-completion-report.md; STOP | all |

PRH findings registered 2026-09-27 (from ADR 0097 design, security):

| ID | Owner | Status | Scope | Target |
|---|---|---|---|---|
| RL-F1 | security + backend | **CLOSED (implemented)** — pre-auth admission (A2/A3/A4a) + A4b DB gate wired into all three webhook handlers; security review of the diff pending | Unauthenticated webhook requests can trigger the tenant-slug lookup and 1–2 read-only verification transactions with no rate limit | PRH-I4 |
| RL-F2 | security + backend | **CLOSED for webhook routes (implemented)** — B2 per-tenant concurrent domain-tx cap; the authenticated-route (player/admin) pool-pinning class is out of scope and registered separately as WEBHOOK-RL-F2-AUTHROUTES-1 (architect review AC7) | A verified tenant can hold unbounded concurrent domain transactions; those waiting on row locks can pin the 10-connection pool | PRH-I4 |
| HTTP-TIMEOUTS-1 (RL-F3) | devops + security | **CLOSED (implemented)** — `cmd/platform-api`'s `http.Server` now sets `ReadTimeout`/`WriteTimeout`/`IdleTimeout`, landed as its own separate commit per devops review condition 1; `WriteTimeout`=60s is a disclosed non-gating estimate pending real measurement | `http.Server` has no `ReadTimeout`/`ReadHeaderTimeout`/`IdleTimeout`; slow senders are never cut off | PRH-I4 |
| RL-F4 | devops + security | **CLOSED (implemented)** — access log and panic-recovery lines both key off the matched route pattern via `observability.RequestState.LogPath`, verified end-to-end through the real middleware chain incl. `otelhttp`'s request cloning | Access log writes the raw attacker-chosen URL path (log amplification / injection) | PRH-I4 |
| WEBHOOK-RL-F2-AUTHROUTES-1 | security + backend | Registered — open, unowned (architect review AC7) | Authenticated player/admin routes retain the RL-F2 pool-pinning class; PRH-I4 closed only the webhook-route instance | after PRH-I4 |
| PRH-I4-METRICS-1 | security + devops | Registered — open | ADR 0097 §8 OTel metrics (`webhook_admission_decisions_total`, gauges) are not wired; only the allow-listed log line exists | before relying on §8 alerts |
| PRH-I4-T6-EXTEND-1 | ledger-finance + payments | **CLOSED for its ledger-invariant scope (round 3)** — the T6a–f idempotency/reversal/reorder matrix, the B2 payments-backlog scenario, and SUM(debits)==SUM(credits)+projection==rebuild assertions are implemented in `internal/httpserver/webhook_admission_t6_idempotency_integration_test.go`; the §6.3 reconciliation-surfacing statement points to PRH-I5's payment reconciliation stream (separate workstream, not this ADR's own ledger-vs-projection stream) | before real-provider gate |
| PRH-I4-T11-TIMING-1 | qa + security | **CLOSED (round 2)** — T11 now drives a virtualized deadline seam (`armBodyReadDeadline` swapped for a fake-clock-driven reader in the test), runs deterministically in the main lane, no timing-lane addition | before PRH-I4 is marked fully IMPLEMENTED |
| PRH-I4-SECREVIEW-1 | security | **CLOSED** — security's third re-verification (`rv-prh-i4-security.md` §8) returned APPROVE: "I have no remaining security conditions on PRH-I4 / PAYWH-RL-1 within the scope of this review." Round 6 additionally closed the one non-blocking Info gap that review found (I7, genuinely falsifiable now; I8, documentation-only) — see ADR 0097 §21.11 | closed |
| PRH-I4-I7-1 | backend | Registered — closed (round 6) | Security Info I7: the KYC row-unchanged checks in `TestAdmission_T6g`'s KYC subtest and `TestAdmission_C1a_KYC…` now seed a real `kyc_verifications` row and assert it, and only it, is unchanged - verified falsifiable by a reverted production mutation | closed |
| PRH-I4-I8-1 | backend | Registered — closed (round 6) | Security Info I8: `TestAdmission_T6g`'s header comment corrected to state the pinned deltas include `deps.DB.WithTenant` and exclude credential resolution (MOCK webhook credentials make no pool acquisition) | closed |
| PRH-I4-L4-1 | security + backend | Registered — open | Security review L4: `WEBHOOK_ADMISSION_ENABLED=false` is accepted in any explicit non-production environment; ADR 0097 §7 says "only when test-support routes are enabled" - tie the config validation to `TestSupportRoutesEnabled()` | before relying on the disable path outside dev |
| PRH-I4-L5-1 | security + backend | Registered — open | Security review L5: a panic inside the A4b gate path (`gatedTenantLookup`/`gatedReader`) reaches `recoverMiddleware` as a 500, not an admission 503 (admitPreAuth/admitVerified have their own recover; the A4b path does not) | before relying on admission's own fail-safe panic behaviour uniformly |
| PRH-I4-I1-1 | architect | Registered — open | Security review I1: A4a's per-key share is keyed per (tenant, provider) rather than per tenantKey as ADR 0097 §3/§9.1 describe, so a tenant with P providers can hold P×16 slots (still under the 64 global cap) - align the ADR text or the key | non-blocking, documentation/config accuracy |
| PRH-I4-I2-1 | security + backend | Registered — open | Security review I2: an operator override with `provider_id: "_unknown"` passes the config charset check and raises the unknown bucket's cap; reject reserved ids in `internal/config.WebhookAdmissionConfig.Validate` | before relying on §9.2 overrides in production |
| PRH-I4-I3-1 | backend | Registered — open | Security review I3: dead code in `newWebhookAdmission` (an `orElse(...)`-style assignment on one line is unconditionally overwritten a few lines later) - cosmetic cleanup | non-blocking |
| PRH-I4-I4-1 | backend | Registered — open | Security review I4: `admission.Bulkhead.Acquire` ignores context cancellation, bounded only by its own `wait` duration (100ms/2s in practice) - security accepted this as-is; tracked only so it is not silently forgotten | non-blocking, accepted |
| PRH-I4-L3-RESIDUAL-1 | devops + security | Registered — open | Security review L3 residual: the raw, un-redacted URL path is still logged on unmatched `/v1/webhooks/*` paths (404/405) - the orchestrator-nil branch is closed, this residual is not; bounded by `MaxHeaderBytes` | non-blocking, bounded |
| WEBHOOK-EDGE-1 | devops + security | Registered — Medium residual (ADR 0097 R1); should be decided before real-money launch; **STAGING/INFRA REQUIRED** | Flooding tenant B's own webhook URL can delay B's callbacks (other tenants unaffected); only an edge control (WAF/CDN rate rule or per-tenant secret path) closes it | before real-money launch |
| WEBHOOK-PATH-TOKEN-1 | security | Registered — conditional on the human's answer to HD-PRH-1 | Unguessable per-tenant webhook path token, only if tenant slugs are confidential | after HD-PRH-1 |
| WEBHOOK-RL-SHARED-1 | devops | Deferred (not built) | Shared (multi-instance) webhook limiter; per-process limits are correct for the caps today | multi-instance deployment |
| WEBHOOK-RL-ADMIN-1 | security + backend | Deferred (not built) | Rate limiting for player/admin API routes (ADR 0097 residual) | later hardening |
| HD-PRH-1 | **human** | **HUMAN DECISION (new)** | Are tenant slugs confidential? Rate-limit behaviour can reveal whether a slug is an active tenant (ADR 0097 R2, Low). Slugs appear in provider-facing webhook URLs; if they are not confidential, R2 is accepted as-is | — |
| HD-KYC-1..8 (corrected 2026-09-28, IC F2; was 1..7) | **human** (with legal) | **HUMAN DECISIONS** recorded by ADR 0096 §4 (not resolved) | HD-KYC-1 cumulative-deposit threshold values; HD-KYC-2 EDD thresholds and whether EDD scores stake size; HD-KYC-3 KYC-at-registration tier; HD-KYC-4 bonus-conversion KYC/AML checkpoint; HD-KYC-5 whether a jurisdiction may relax the structural first-withdrawal rule (default: no); HD-KYC-6 cross-tenant/brand verification reuse (restates ADR 0028 §7); HD-KYC-7 gates for ConversionOperation / affiliate payout once built. HD-KYC-1..3 are tied to HDR-J-6 + legal review | — |
| WD-RG-1 | identity-compliance + payments | Registered — pre-existing gap (surfaced by ADR 0096 reconnaissance); **NOT IMPLEMENTED**; not in PRH scope unless the human adds it | The withdrawal path has no RG (responsible-gaming) or Risk gate at all today, only balance sufficiency. Separate from KYC-ENFORCE-1 | next hardening stage |
| API-POOL-PIN-1 | security + backend | Registered — residual (ADR 0097 R5, architect review); **NOT IMPLEMENTED**; not in PRH scope | Authenticated player/admin API routes have no per-tenant concurrency bulkhead; a burst of slow authenticated requests could pin the connection pool (webhook routes are covered by ADR 0097) | later hardening |
| HD-0095-1 | **human** | **2026-09-28: the K1 grant substrate is IMPLEMENTED** (CAP-GRANT-1, migration 0112). M1/M2 force-resolve itself is **NOT IMPLEMENTED** until K3 (ADR 0101, migration 0115). History: **Status 2026-09-28: design PROPOSED in ADRs 0099/0100/0101 (`d83a71c`); awaiting security + ledger-finance review; NOT IMPLEMENTED.** **DECIDED 2026-09-28 by the human** (`docs/decisions/0098-human-decision-response-force-resolve-and-manual-adjustment.md` §1): force-resolve is a configurable capability granted/revoked by authorized platform or tenant administrators within their own scope; no self-grant; tenant admins cannot grant platform authority; no bypass of the deterministic financial engine; no invented threshold (configurable where required; a required value is a separate human decision). M1/M2 move from BLOCKED to design pending (PRH-2 plan). Was: HUMAN DECISION (new, ADR 0095) | Who may force-resolve an unresolvable payout or a disputed payment attempt, and above what threshold (manual transitions M1/M2). Ties to LEDGER-MANUAL-ADJ-4EYES-1 (not decided). M1/M2 stay BLOCKED until decided | — |
| CAS-STMT-IO-1 | casino + ledger-finance | Registered — Low; not reachable today (MOCK only) | The casino statement source is read inside the reconciliation REPEATABLE READ tx; a real source would be provider I/O inside a tx (F-POOL-2 class). Fix before a real casino statement source: fetch outside the tx, store, then match (ADR 0095 §reconciliation pattern) | before first real casino statement source |
| KYC-SUBMIT-OUTBOX-1 | identity-compliance | Deferred, accepted (ADR 0095 §15.3/§25 condition 5, identity-compliance review); PRH-I2 (KYC part) landed 2026-09-27 without it, per that acceptance | Outbox/sweeper for KYC create/submit if a vendor's semantics need it. A durable KYC submission outbox is a **hard precondition on the first real KYC adapter** — no real KYC adapter is accepted into PRH-I2 or any later stage without this design landing first | KYC vendor intake (hard precondition, not merely a suggestion) |
| PAY-ATTEMPT-RETENTION-1 | payments + ledger-finance | Deferred candidate (ADR 0095) | Retention/archival of payment_attempts and payment_provider_events rows | later |
| PAY-PAYOUT-CASCADE-1 | payments | Deferred candidate (ADR 0095) | Cascading a definitely-declined payout to another provider | later |
| KYC-FX-AGG-1 | identity-compliance + ledger-finance (+ human for rate source) | Registered 2026-09-27 — **NOT IMPLEMENTED**; design/human input needed | Cross-asset aggregation of cumulative deposits for KYC thresholds (security re-verify N2 of ADR 0096). No FX/rate source exists; the platform must not invent one. Interim (PRH-I3): per-asset threshold rows; a jurisdiction with an active cumulative_deposit policy but no row for the transaction's asset yields `unavailable` → fail closed | before real-money launch in any multi-asset jurisdiction with a cumulative-deposit threshold |
| PRH-REF-C2 | casino + devops (+ security) | Registered — before real casino go-live; **NOT IMPLEMENTED**; **STAGING REQUIRED** (log pipeline) | Security review of PROVIDER-REF-BOUND-1 C2: an over-bound casino callback leaves only the log line `casino_webhook_provider_reference_rejected` as evidence; those events need retention meeting the rejection-evidence requirement plus per-provider rate alerting | before real casino go-live |
| PRH-REF-C3 | orchestrator | Waiting on CI-BILLING-1 | Re-run the 0099 integration, migration and mutation evidence on GitHub CI | PRH final gate |
| PRH-REF-F1 | payments + ledger-finance | **Status 2026-09-28 (FH-7, verification only): still OPEN, latent.** No payments posting calls `idempotency.Assign`: deposit postings use the raw provider reference, which is bounded by 0099. It must be handled if and when a payments posting adopts `Assign`. Original status: Registered — Medium, latent (assigned into PRH-I1) | `idempotency.Assign` external mode composes `"<len>:"+ref[+"#"+disc]` into `ledger_transactions.provider_tx_id`; a valid 250–255-byte reference overflows the 0099 bound → generic 500. Must be handled when payments postings adopt it | PRH-I1 |
| PRH-REF-F4 | security + payments | **Status 2026-09-28: FIXED (by the RV-PRH-I1 callback F2 fix).** `boundedDeclineReasonAudited` bounds the reason before any receipt insert, at the callback, phase C and sweeper sites; oversize text becomes an ASCII sentinel with a length and SHA-256 prefix in the audit, never the raw text. `security` confirmed "Decline-reason bounding before persistence: Correct" (`rv-prh-i1-payout-security.md` re-verification 1), and `ledger-finance` closed F2 (`rv-prh-i1-callback-ledger.md` re-review 2). Original status: Registered — Medium; not yet ruled | A payments callback's `decline_reason` is unbounded and written twice into the permanent audit log (PROVIDER-REF-BOUND-1 code review F4) | PRH-I1 (security to rule) |
| KS-L6 | security + payments | **IMPLEMENTED, 2026-09-27** (migration 0105, PRH-I1): a platform take-over (tenant-engaged → platform-engaged, true→true re-engage) cancels any `open` tenant release request for that switch in the same guard trigger, so it can never be raced open by a request the tenant filed before the take-over. Test: `TestMigration0105_PlatformTakeoverCancelsOpenTenantRequest` | PRH-I1 step building migration 0105 (was numbered 0102 at registration) |
| PRH-I1-MANIFEST-1 | payments | Registered 2026-09-27 (PRH-I1 round 2) — deferred, not built; NOT a regression, an explicit scope decision | ADR 0095 §10.1's `SupportsPayout`/`SupportsDepositReversalEvents` fields are not modeled on `OperationManifest` (`internal/payments/contract.go`): no code path reads either one today (payout eligibility is `AdapterCapability.SupportsWithdrawal`; there is no deposit-reversal-event handling distinct from the existing `payment_provider_events.event_type='deposit_reversal'` receipt path). Add only alongside a real consumer, per CLAUDE.md's no-fake-completion rule (an unread manifest field is a capability nothing can exercise) | whichever PRH-I1 step first needs to gate registration/new-activity on either fact |
| PRH-I1-MANIFEST-2 | payments | Registered 2026-09-27 (PRH-I1 round 2) — deferred, not built | ADR 0095 §10.1's `RedeliveryOn401`/`RedeliveryOn5xx` fields (`redelivers \| terminal \| unknown`, LF-C1) are not modeled. No code path in this repo currently branches on a vendor's 401/5xx redelivery semantics - the closed condition this exists for (§6.6) is design-only pending a real vendor contract | before the first real (non-MOCK) payments adapter is wired (mirrors PROV-OUTBOUND-CRED-1's own tripwire gate) |
| PRH-I1-MANIFEST-3 | payments | Registered 2026-09-27 (PRH-I1 round 2) — deferred, not built | ADR 0095 §10.1's `ErrorClassMapping` (a declared list of `NotProcessed` codes; empty means every undocumented response is `Ambiguous`) is not modeled. `gate.go`'s ErrorClass mapping today is adapter-internal (each adapter's own `Deposit`/`Withdraw`/`QueryStatus` returns an `ErrorClass` directly per §8's own "the adapter returns NotSent or Ambiguous directly" rule) - there is no generic vendor-code-to-class table for this field to drive yet | before the first real (non-MOCK) payments adapter is wired |
| PRH-I1-MANIFEST-4 | payments + ledger-finance | Registered 2026-09-27 (PRH-I1 round 2) — deferred, not built | ADR 0095 §10.1's `StatementSource` field ("required for production eligibility when LF-C1 demands it") is not modeled. PRH-I5's reconciliation stream determines eligibility through a different, already-built mechanism (`payments.MockStatementSource`, `providerkind.Synthetic`/`RefuseSyntheticInProduction`), not a manifest field - adding an unread duplicate would violate CLAUDE.md's no-fake-completion rule. Revisit only if reconciliation's own eligibility check is refactored to read the manifest instead | before the first real (non-MOCK) payment statement source is wired |
| PROV-OUTBOUND-CRED-1-LEGACY-PATH | payments | Registered 2026-09-27 (RV-PRH-I1 kill-switch phase 2 security review P2-L2) — **NOT IMPLEMENTED; a condition, not launch-blocking today** | `Orchestrator.InitiateDeposit` → `attemptDeposit` → `handleDecline`/`resolveAmbiguous` (`internal/payments/orchestrator.go`, roughly lines 546-770) is a pre-`InitiateDepositAttempt` deposit path still present and exported: it calls `provider.Deposit`/`provider.QueryStatus` directly, inside the caller's own transaction, with no credential resolution (PROV-OUTBOUND-CRED-1's gate is bypassed entirely), no kill-switch predicate, and no gate redaction. It is not introduced by PRH-I1 phase 2 and not reachable from any non-test caller today (`httpserver`'s deposit handler calls `InitiateDepositAttempt` exclusively) - a grep of the phase-2 branch found none - but it remains an exported, compilable bypass of ADR 0095 §3.2/§10.3/§11 in full. Left as-is this round: the PAY-DOUBLE-CREDIT-1 fix team owns this area of `orchestrator.go` and deleting it here would risk an avoidable rebase collision. Fix: delete `InitiateDeposit`/`attemptDeposit`/`handleDecline`/`resolveAmbiguous` (or move them behind a test-only build tag) and migrate their remaining tests onto `InitiateDepositAttempt`, before `TestOutboundPrecondition_EveryWiredAdapterIsSynthetic` is ever relaxed for a real payments adapter | before the first real (non-MOCK) payments adapter is wired, and before `TestOutboundPrecondition_EveryWiredAdapterIsSynthetic` is relaxed for payments |
| KS-AUDIT-TENANT-1 | architect (+ security, payments) | **IMPLEMENTED 2026-09-28 (kill switch only; PRH-2 G1, merged `7f54f56`; migration 0109).** Adds `audit_log.subject_tenant_id`, RLS `subject_tenant_read`, the `audit_log_subject_actor_guard` trigger, `staff_users.display_name` with hygiene CHECK, the kill-switch write path, `GET /v1/admin/audit-log/platform-actions` (identified actor per HD-PRH2-5; no IP/UA/email), and the display-name endpoints. Reviews: security ACCEPT on re-review, code review conditions closed (`reviews/g1-*.md`). Orchestrator verification (local, not CI): build/vet/gofmt/lint 0, `migrate verify` through 0109, `-race` integration audit/identity/kyc/casino/cmd ok, full httpserver ok on re-run (the first run hit TEST-ADMISSION-FLAKE-1, pre-existing and unrelated; now being root-caused). Remaining: PLAT-AUDIT-SUBJECT-1 (other platform writes), AUDIT-PRESENTATION-POLICY-1 (configurable presentation). History: Registered 2026-09-27 (RV-PRH-I1 kill-switch security review M4) — **NOT IMPLEMENTED; launch-blocking before production launch or the first B2B tenant** | A platform-scoped payment kill-switch mutation (engage/request/approve/release/cancel acting on a tenant's switch as `platform_admin`) is audited with `tenant_id = NULL` (a platform-level event) and the real target tenant only in `metadata->>'target_tenant_id'` (untyped JSONB, no index) - `internal/httpserver/payments_kill_switch_handlers.go`'s `auditTenantID()`, because `audit_log`'s RLS (migration 0014) has no "platform writes into a named tenant's scope" policy family, unlike migration 0105's own two-family design. A tenant-scoped audit read (tenant back office, or a B2B own-licence operator's regulator export) therefore NEVER sees that the platform engaged, took over, or released ITS OWN kill switch - exactly the question a regulator asks ("who stopped payments on this licence, and when"). Interim measure is accepted (the row is real, in the same transaction, attributed to the true platform actor, and cannot be forged - `TestPaymentsKillSwitchAPI_AuditRecordsCarryBeforeAfterAndTargetTenant` pins `target_tenant_id`'s presence). Fix (architect decision, changes `audit_log`'s RLS): either an `audit_log` platform INSERT-only policy family mirroring migration 0105's, so a platform action lands under the target tenant's own `tenant_id` with actor scope in metadata; or a first-class indexed `target_tenant_id` column | before production launch or the first B2B tenant onboarded under this platform's audit trail |
| CAS-JURIS-AUDIT-1 | casino | Registered 2026-09-27 (security review RV-PRH-I2 I1) — pre-existing, not introduced by PRH-I2's launch split; **NOT IMPLEMENTED** | `evaluateJurisdictionBlocklist`'s denial writes no audit record, unlike the RG (`evaluateAndAuditEligibility`) and Risk (`evaluateAndAuditRisk`) denials in the same `LaunchGame` phase A - ADR 0095 §15.1.1's own phase-A description overstated this ("A policy denial ... still commits its own audit row"; true for RG/risk, not for jurisdiction). Fix: add a `casino.launch_denied_by_jurisdiction` audit record alongside the existing `LaunchGameResult{Denied:true}` result, mirroring the other two denial paths; or correct the ADR text if an audit record is deliberately not wanted here | next casino jurisdiction-enforcement task |
| CAS-HEALTH-FAILOPEN-1 | casino | Registered 2026-09-27 (security review RV-PRH-I2 I2) — pre-existing (predates PRH-I2), acceptable while `HealthStatus` is an in-memory-only contract (§9.6); **NOT IMPLEMENTED** | `LaunchGame` treats a `HealthStatus` error as healthy (`if health, err := provider.HealthStatus(ctx); err == nil && health.CircuitState == CircuitOpen`) - a real adapter that ever returns an error from `HealthStatus` would let a launch proceed against an unhealthy provider rather than failing closed. Revisit before a real casino adapter: either the contract must guarantee `HealthStatus` never errors, or an error must be treated as unhealthy (fail closed) | before first real casino adapter |
| CAS-REVOKE-BET-RACE-1 | casino + ledger-finance | Registered 2026-09-27 (security review RV-PRH-I2 I3) — pre-existing, bounded; **NOT IMPLEMENTED** | `postBet` reads the launch session via a plain `SELECT` (`GetLaunchSessionByID`), not a row lock, so a bet racing a concurrent `RevokeLaunchSession` (or, after item 2's fix, a concurrent status/expiry transition) can still commit against a session that is revoked microseconds later. The bet stays fully ledger-accounted (no invariant violation, no double-post) - this is a narrow authorization-timing race, not a financial-correctness one. Record as known/bounded; revisit with a `SELECT ... FOR UPDATE` or an equivalent lock if a real adapter's callback volume ever makes the window material | before first real casino adapter |
| KYC-REVIEWREQ-FORWARD-1 | identity-compliance | **RULED AND IMPLEMENTED, 2026-09-27** (`rv-prh-i2-kyc-identity-compliance.md` Rulings 2 and 5; ADR 0096 §21.2/§22.1/§22.6) — security re-verification 3 CLOSED N-3 (`rv-prh-i2-kyc-security.md`); security re-verification 4 CLOSED the whole series and found only LOW notes L-1/L-2, both now implemented. **Pending a light security confirmation of this round's own L-1/L-2 fix** (not a fresh finding, no known gap — a final pass on the exact diff) | A staff `review_required` escalation is STICKY against a later provider or callback result when that result is `approved` OR `expired` — `applyForwardOnlyStatus` gates on `reviewed_by IS NOT NULL AND newStatus IN (approved, expired)`. A DENY-direction result that is NOT `expired` (`rejected`) is NEVER blocked, even on a staff-escalated row — N-3 found the original, unconditional gate silently discarded a genuine vendor rejection too, with no audit trail, which both weakened the ADR 0096 cross-account overlay and created an insider-abuse vector. `expired` is held (Ruling 5, L-1) because it is already excluded from the cross-account overlay (N-1b) and this account's own enforcement outcome is identical whether held or applied — holding it instead preserves the officer's own open case for an explicit decision. A PROVIDER-set `review_required` (no staff actor) is unaffected either way. Whenever a provider result IS discarded by the sticky guard, an explicit `kyc.provider_result_held_for_review` audit row is written, de-duplicated per (tenant, verification, provider outcome) so vendor redelivery of the SAME held outcome does not repeat the row (L-2). Tests: `TestApplyForwardOnlyStatus_StaffSetReviewRequiredIsStickyAgainstProviderApproval`, `TestApplyForwardOnlyStatus_ProviderSetReviewRequiredStillAdvancesToApproved`, `TestEvaluateEnforcement_N3_StaffReviewRequiredThenVendorRejected_StillApplies`, `TestEvaluateEnforcement_N3_StaffReviewRequiredThenVendorApproved_HeldForReviewAudited`, `TestEvaluateEnforcement_L1_StaffReviewRequiredThenVendorExpired_HeldForReviewAudited`, `TestEvaluateEnforcement_L2_RedeliveredHeldOutcome_DoesNotDuplicateAudit` (all mutation-killed) | ruled and implemented — pending light security confirmation |
| KYC-COMMIT-MISLABEL-1 | identity-compliance (record only) | Registered 2026-09-27 — informational, no history rewrite | Commit `be423c3`'s message reads "identity-compliance: rule on N-1 fix, review_required forward-move, and ReviewVerification CAS", but its diff actually carries security's own re-verification 2 section of `rv-prh-i2-kyc-security.md` — two concurrent agents' commits crossed in the same window. The genuine identity-compliance ruling with the matching message is a SEPARATE commit, `6621326` (correct diff: `rv-prh-i2-kyc-identity-compliance.md`), plus its addendum `c80103b`. No commit history rewrite performed; recorded here for the record only | none — historical record |
| PAY-DOUBLE-CREDIT-1 | payments + ledger-finance (design: architect) | **Status 2026-09-28: CLOSED — FIXED (IMPLEMENTED against MOCK adapters; real PSP PROVIDER DEPENDENT, see PAY-PSP-CONTRACT-INVDEP1).** The human approved the plan under Financial Hardening. Implemented as ADR 0095 §28 AM-2 / INV-DEP-1 and migration 0107: FH-3 `8ce538c`, FH-3b `cb03868`, FH-3c `92f5889`, FH3-FOLLOWUP-1 `5ee09e4`, merged at `a72128d`. Reviews: `ledger-finance` APPROVED with financial sign-off, then CONFIRMED (`rv-fh3-ledger.md`, `057587c`/`3a38930`); `security` APPROVE (`rv-fh3-security.md` re-verification 1, `93f13b7`); `payments` APPROVE (`rv-fh3-payments.md`); `code-reviewer` READY WITH CONDITIONS (`rv-fh3-code-review.md` re-review 1, `81754d4`), whose N-1 was ruled by `ledger-finance` and registered as PAY-RECON-N1; `qa` A–O matrix (`qa-fh3-adjudication.md`); architect final review `rv-fh7-architect-final.md`. HD-LEDGER-UNALLOC-1 (A) is implemented. Non-blocking residuals: PAY-RECON-N1, PAY-F3SM-TEST-1, PAY-POLL-AMOUNT-1, PAY-SWEEP-CAS-NOISE-1. Open launch items: PAY-P1-MULTISUCCESS-ALERT-1, DEVOPS-0107-INDEX-WINDOW-1, LEDGER-SUSPENSE-B-1 (deferred). Original status: **OPEN — HIGH, financial integrity; implementation HALTED pending human approval (2026-09-27)** | One deposit intent can be credited twice through fallback + late success. The cause is ADR 0095 T13 "second capture posts" (the LF-Q1 ruling), plus a T7 success path with no intent-resolution check. It is introduced by the F-POOL-2 design; Stage 10.3 did not have it. Reproduced by A7 test #1a (kept in the session scratchpad, not committed). Full analysis, invariant INV-DEP-1, fix design and test matrix: `docs/plans/payment-readiness/double-credit-reconciliation.md`. |
| GOV-INC-2026-09-27-DBCRED | orchestrator | **RESOLVED 2026-09-27** | The callback and payout sub-agents changed the local Postgres role passwords via `sudo`. Restored with human authorization; 173 orphan scratch DBs dropped; no credential committed. Permanent rule added to `CLAUDE.md` § Environment safety. Record: `docs/governance/incident-2026-09-27-local-db-credential-mutation.md`. |
| PAY-SEC-S-H1 | payments (callback owner) | **Status 2026-09-28: CLOSED.** `security` re-verification 1 (`rv-prh-i1-payout-security.md`, `cb1330f`, at `0a96a01`, now merged) closed S-H1: probe SP-B2 is safe, and mutants MH1 and MH1b are killed by `TestRVLF_N3_DeferredApplyNeverReplaysADepositDeclineAsPayoutEvidence` and others. `ledger-finance` re-review 2 (`rv-prh-i1-callback-ledger.md`, `b9e8045`) records N3/S-H1 CLOSED. That review's LOW pins (MA, MRF, the cross-operation deferred receipt, and the sync-path reference rule) landed in `109ef04`, with mutation evidence in `evidence/prh-i1-mutation-kill.txt` ("FH-5 security re-verification follow-up"). Standing condition, unchanged: a `security` review of the future route and adapter wiring that accepts `payout` callbacks (PROVIDER DEPENDENT). Original status: **OPEN — HIGH (not reachable: no payout events emitted)** | Payout security review `rv-prh-i1-payout-security.md`: the deferred-receipt replay after a payout receipt skips the event_type/operation check, so a stored deposit-typed decline released a payout hold (probe SP-B2). Same as ledger callback N3. The unmerged callback WIP `7641332` contains in-progress edits. |
| PAY-SEC-S-M1 | payments (callback owner) | **Status 2026-09-28: CLOSED.** `security` re-verification 1 (`rv-prh-i1-payout-security.md`, `cb1330f`, at `0a96a01`, merged) closed S-M1: probe SP-A leaves the attempt `disputed` (`provider_reference_mismatch`), and mutant MM1 is killed by `TestRVLF_SM10_PayoutSuccessProviderReferenceMismatchDisputes`. `ledger-finance` re-review 2 (`rv-prh-i1-callback-ledger.md`) also records it CLOSED. In `109ef04` the rule is centralized in `applyPayoutSuccess`, which covers the sync path: `TestPayoutDispatch_SecGapC_SyncSuccessProviderReferenceMismatchDisputes`. The MM1b fallback is covered by that defence in depth, and it was disclosed rather than claimed as an isolated kill. Original status: **OPEN — MEDIUM** | A callback payout success with a different provider reference completes instead of being disputed (probe SP-A). Must match the QueryStatus N6 behaviour; pin with test SM10. |
| PAY-SEC-S-M2 | payments (payout owner) | **FIXED, 2026-09-27** | `payoutResolveAudit` (`internal/payments/payout.go`) now records `attempt_id`, `evidence_class`, `attempt_state_before`/`attempt_state_after`, `withdrawal_state_before`/`withdrawal_state_after`, and `terminal_reason` when set; `Outcome` reflects a genuine dispute (`failure`) distinctly from every other resolve effect (`success`). Tests: `TestPayoutResolveAudit_SM2_RecordsBeforeAfterStateAndMetadata`, `TestPayoutResolveAudit_SM2_DisputeRecordsFailureOutcomeAndTerminalReason` (`internal/payments/payout_security_round_test.go`). |
| PAY-SEC-TESTS-1 | payments (payout owner) | **FIXED, 2026-09-27** | SM2a killed: `TestResubmitPayoutAmbiguous_SM2a_KillSwitchReschedulesNeverEscalates` (mutation-confirmed - removing the T12 Go-level `checkPayoutKillSwitch` call now fails this test). SM7 killed: `TestWithdrawalResolve_UnlinkedStaffAccountRejected` / `TestWithdrawalResolve_SuspendedStaffAccountRejected` (`internal/httpserver/stage3c_withdrawal_resolution_test.go`, mutation-confirmed - removing `/resolve`'s linked-staff check now fails both). Permanent cross-tenant tests added: `TestWithdrawalSubmit_CrossTenantDenied`, `TestWithdrawalResolve_CrossTenantDenied` (same file). SM11/SM12/SM14 (resolve/submit audit ActorID, deny-audit outcome) re-confirmed already covered by the existing, still-passing M5/N3-derived tests - no new test needed. SM13 (four-eyes `pending_review` bypass) re-run and CONFIRMED TO SURVIVE on a working database when BOTH `withdrawal.LockApprovedForSubmission` and `withdrawal.MarkSubmittedPending`'s own state checks are mutated together (mutating either alone is caught by the other - genuine defence in depth, but the pair together was untested); killed by the new `TestWithdrawalSubmit_PendingReviewBypassRejected` (`internal/httpserver/stage3d_withdrawal_governance_test.go`). All mutations applied, run, and reverted; reverts verified byte-clean via `git diff --stat`. |
| PAY-SEC-S-L2 | payments (payout owner) | **FIXED, 2026-09-27 (FH-6 round 2, P-C1)** | New `SweeperBatchLeaseOwner = "sweeper-batch"` constant (`internal/payments/sweeper.go`), used by `claimBatch` and both of `payout.go`'s N7 exemption points, replacing the bare `"sweeper"` literal. SP-C (the stale-snapshot relabel path) is now FIXED too, per the ledger-finance ruling in `rv-prh-i1-payout-ledger.md`: an earlier attempt excluded any row under a live non-batch lease from `claimBatch`'s SELECT regardless of state, which regressed 7 tests - the real cause was phase C moving a row OUT of `submitting` without clearing its lease, not (as an earlier, now-corrected version of this row said) T2/T12 setting `next_action_at` earlier than `lease_until` (P-C2: both set them equal). V1, restricted to `state = 'submitting'` (`AND NOT (state = 'submitting' AND lease_until > now() AND lease_owner IS DISTINCT FROM 'sweeper-batch')`), closes SP-C without that regression - full suite green, including all 7 previously-regressed tests. Tests: `TestClaimBatch_SL2_UsesDistinctBatchLeaseOwnerConstant`, `TestClaimBatch_SL2_SPC_StaleSnapshotNeverRelabelsALiveSubmittingLease` (the restored permanent SP-C reproduction; `internal/payments/payout_security_round_test.go`). Corrected root-cause writeup: `docs/plans/payment-readiness/prh-i1-payout-launch-conditions.md`. No longer a condition on wiring a payout sweeper. |
| PAY-SEC-PC3 | payments (payout owner) | **FIXED, 2026-09-27 (FH-6 round 2, Low)** | Escalate's terminal-state CAS predicate (added for N-none, confirmed "correct but untested" by ledger-finance - a mutant removing it survived the whole suite) now has a permanent test, `TestEscalate_PC3_RefusesOnATerminalAttempt_StaleSnapshot` (mutation-confirmed). `payoutResolveAudit`'s "before" attempt AND withdrawal state are now read under the withdrawal lock at both call sites (`applyPayoutStatusEvidence`, `PollPayoutStatus`'s no-reference fallback), never from the caller's outer pre-transaction snapshot - `withdrawal_state_before` is no longer hardcoded to `submitted`. A genuine no-op (before==after) now carries an explicit `metadata["no_op"]` boolean (the `Outcome` enum itself stays `success`/`failure`/`denied` only - the `audit_log` table's own CHECK constraint, migration 0014, has no fourth value, and adding one is a schema migration outside this fix's scope). Test: `TestPayoutResolveAudit_PC3_BeforeStateReadUnderTheLock` (mutation-confirmed). |
| PAY-SEC-CR-1 | payments (payout owner) | **FIXED, 2026-09-27 (FH-6 code review round, `rv-prh-i1-payout-code-review.md`, e48c8e7)** | Four conditions from the code review's own "Conditions to close": (1) pinned the two S-M2 audit fields the reviewer found unpinned/SURVIVED (`withdrawal_state_after`, `evidence_class`) with a new mutation-confirmed test, `TestPayoutResolveAudit_CodeReview_WithdrawalStateAfterAndEvidenceClass`; (2) `escalateAmbiguousPayout` no longer swallows every `ErrAttemptStateConflict` unconditionally - it re-reads the attempt and swallows ONLY when the re-read confirms the attempt is terminal or already escalated, otherwise returns the error loud (closing the reviewer's own disclosed LOW gap: an unintended 0-row conflict, e.g. a vanished/wrong-tenant row, used to be silently skipped forever) - mutation-confirmed by the new `TestEscalateAmbiguousPayout_CodeReview_UnintendedConflictIsLoud`; the existing `TestEscalate_PC3_RefusesOnATerminalAttempt_StaleSnapshot` (PAY-SEC-PC3) already provided the reviewer's requested DETERMINISTIC Escalate-on-terminal coverage (the reviewer's own flagged gap, `TestA7_1b` hitting the window ~2.5%/1-in-40, is a separate, pre-existing test unaffected by this fix); (3) `TestA7_3`'s vacuous "exactly 1 ledger posting" check (`SELECT count(DISTINCT ledger_transaction_id) FROM payment_attempts WHERE id = $1` - one column of one row, can never exceed 1) now counts `ledger_transactions` by `correlation_id = deposit_intent_id` instead, the real structural signal; (4) the S-M2 before-state hardcode overlapped and was already fixed by `PAY-SEC-PC3` above. |
| PAY-SEC-LAUNCH-1 | payments | **OPEN — launch conditions, recorded** | S-L1 (finance staff linked to the player's own Person), S-L3 (`/resolve` throttle), S-L4 (staff eligibility check inside T1p; in-tx capability re-read), payout destination binding in `WithdrawRequest` - all four **NOT IMPLEMENTED**, deliberately deferred to before real-money payout launch, not this stage. Recorded in a standalone note (not an ADR 0095 edit): `docs/plans/payment-readiness/prh-i1-payout-launch-conditions.md`. |
| HD-LEDGER-UNALLOC-1 | human (ledger-finance recommendation) | **DECIDED by the human 2026-09-27: "A now, B later"** | Treatment of a second REAL provider capture for an already-credited deposit intent. **Now (A):** the attempt goes to `disputed` (`multiple_success_for_intent`) with no ledger posting, a P1 alert and the `pay_captured_unposted` reconciliation kind; the player is never credited. **Later (B):** a suspense/unallocated account posting, backfilled idempotently from the disputed rows, in a separately authorized stage (registered as deferred item LEDGER-SUSPENSE-B-1). Dispute resolution and the refund stay blocked on HD-0095-1 and LEDGER-MANUAL-ADJ-4EYES-1. Ruling: `docs/plans/payment-readiness/lf-q1-supersession.md`. |
| LEDGER-SUSPENSE-B-1 | ledger-finance | **DEFERRED (human decision HD-LEDGER-UNALLOC-1, end state B)** | Suspense account type plus a new non-`deposit` transaction type for PSP-held unallocated captures; the reversal path must debit suspense for these postings; one-time idempotent backfill from `disputed`/`multiple_success_for_intent` attempts. Needs separate authorization. |
| DEVOPS-0107-INDEX-WINDOW-1 | devops + ledger-finance | **DEFERRED — required before any production-size ledger** | Migration 0107 builds `ledger_transactions_one_deposit_per_intent` non-concurrently inside the migration transaction, which blocks writes to `ledger_transactions` while it runs. That is acceptable at current scale; a production deployment needs a planned window or a CONCURRENTLY build procedure (ledger-finance confirmation in `lf-q1-supersession.md`). |
| KS-DEP-T2-T3-1 | payments (kill-switch owner) | **Status 2026-09-28: CLOSED — FIXED in `2da7548` (merged on this branch).** `qa` delta verification PASS, with the named mutant killed (`qa-killswitch-phase2-verification.md` §2; `TestDriveCreatedAttemptCascade_KillSwitchOnFallbackProvider_DeclinesCleanly_T3`). The raising reviewer (`architect`) verified `drive.go` against the required fix (ADR 0095 §10.9.3 status note; `rv-fh7-architect-final.md`). No `code-reviewer` record exists for this delta (FH7-07, non-blocking). Original status: **OPEN — must close before PRH-I1 is marked complete and before any sweeper is wired** (architect review of kill-switch phase 2, `rv-prh-i1-killswitch-phase2-architect.md`) | A kill switch scoped to the fallback provider makes the cascade T2 claim (`ClaimCreatedForSubmission`) match zero rows. `drive.go` returns that as an error, so the attempt stays `created`, the intent stays `pending`, and the player request errors instead of declining cleanly. No money moves. Fix: in the same tx, check `KillSwitchEngaged`, `RejectCreated(…,'kill_switch')`, recompute the intent projection, audit; add a test plus a mutant. |
| TEST-RESISO-RACE-1 | qa + devops | **Status 2026-09-28: idle-machine half SATISFIED — 40/40 PASS** (all 8 lane tests × 5 rounds, exactly CI's lane commands, `-race -count=1`, thresholds unchanged; 3 rounds `taskset -c 0-1`, 2 rounds on 4 CPUs; private DB migrated to 0107; evidence `docs/plans/payment-readiness/evidence/fh7-timing-lane-idle.txt`, `649f3ba`). A first attempt against the stale shared `igaming_platform_ci_local` (at migration 0100) failed with SQLSTATE 42P01 (missing `payment_attempts`) — environment, discarded. **GitHub-CI half still OPEN, BLOCKED by CI-BILLING-1** (together with F-POOL-1 K1). Originally: **OPEN — gate item (must be settled on an idle machine and on GitHub CI; thresholds are NOT to be loosened)** | `TestResolutionIsolation_*` (8 tests) assert wall-clock budgets (400ms/150ms). Without `-race` they pass reliably. Under `-race` on this shared container a varying 2–5 of 8 fail, and the older baseline `f2755ac` fails the same way under the same load. The CI timing lane runs them alone with `-race` on an otherwise idle runner. Evidence: `qa-killswitch-phase2-verification.md` §1.2; orchestrator A/B run. |
| TEST-ADMISSION-FLAKE-1 | qa (+ security, owner of ADR 0097) | **CLOSED 2026-09-28 — root-caused and fixed (merged `51bc583`; test-only).** **Root cause:** T6a, T6c, T6e and T6g drive ADR 0097's tier-B1 GCRA bucket (10/s, burst 1) into its limited state with real HTTP requests, then assert that the next request is limited. That holds only while real elapsed time stays under the ~100 ms emission interval. Under CPU contention the round trip exceeded it and the token refilled. **Reproduced deterministically:** a forced 150 ms delay makes the unmodified test fail in isolation with the reported message. **Fix:** inject ADR 0097 §5.1's existing `admission.FakeClock` (already wired via `WebhookAdmissionSettings.Clock`), and replace `time.Sleep` with `clock.Advance`. No production change and no assertion weakened. **Verified:** T6 at `-race -count=20` isolated and under CPU-stress load, 0 failures; full httpserver 3× under real load, 0 T6 failures; orchestrator re-run of T6 at `-count=10` after the merge: ok. History:  **OPEN — must be root-caused (not dismissed); not yet reproduced deterministically** | Reported by identity-compliance during PRH-2 F-kyc verification: `TestAdmission_T6a_PaymentsDeposit_RetryAfter429_Idempotent` and `TestAdmission_T6c_CasinoBetLimitedThenRollbackReorder` failed once in `internal/httpserver` under heavy concurrent load (several agents running `-race` suites against the shared local Postgres). Their limiter uses a 1-token burst with 100 ms replenish. The same failure reproduced at the base `cabca27` under the same load, and they passed 3× in isolation and under `-count=3`: a pre-existing timing sensitivity, not an F-kyc regression. Required: reproduce, determine the actual cause (wall-clock dependence in the test vs the limiter), and fix the test to assert on order/outcome with an injectable clock (plan rule T-1/T-2). Thresholds are not to be loosened. **2026-09-28:** T6a failed again during the G1 merge verification (full httpserver under concurrent agent load). It passed 10/10 in isolation and on a `-p 1` re-run of the full package. Root-cause work assigned to `qa` (branch `prh2-test-admission-flake-1`). |
| TEST-SCRATCH-LEAK-1 | qa + devops | **OPEN** | `depositV2ScratchPool` (and similar per-test scratch helpers) sometimes fail to drop their database: the auto-drop logs "permission denied to terminate process" when it races another session. About 190 `m01*` databases had accumulated by 2026-09-27, a plausible contributor to the disk exhaustion that crashed Postgres and forced a container restart. The orchestrator drops the idle orphans during quiet windows. Fix: make the helper's cleanup drop `WITH (FORCE)` using the test-admin role, and fail the test loudly if cleanup fails. The shared-infrastructure rule still applies: sub-agents must not do bulk drops themselves. |
| A7-5C-STATIC-1 | payments + architect | **SUPERSEDED 2026-09-27 by ledger-finance C8 (`rv-fh3-ledger.md`): a runtime test IS possible (two identical reversals of a resolved-but-never-posted original; assert the waiter's query is the receipt INSERT, not the intent lock). Implemented in FH-3c instead of the static check below** | The A7 #5c mutant ("receipt insert after an L1 lock" on the reversal tombstone branch) cannot be caught by a runtime test: the ABBA deadlock needs a second call site with the opposite lock order, and none exists. Decision (reversible, within scope): enforce the rule with a STATIC conformance test. In `internal/payments`, every function that calls `insertReceiptDeduped` must call it before any `FOR UPDATE` query or L1-lock helper in the same function, in the style of the existing AST guard tests. It must kill the #5c mutant. To be built after the FH-3/FH-5 merge. |
| PAY-P1-MULTISUCCESS-ALERT-1 | devops + payments | **OPEN — recommended priority within alert delivery (launch-blocking together with the general alert-delivery item)** | The P1 `payments_multiple_success_for_intent_alert` is a log line only, like the kill-switch engage alert. It signals real, unrefunded player money held at the PSP, so the payments review recommends wiring it to paging before general observability work. Source: `rv-fh3-payments.md`. |
| PAY-PSP-CONTRACT-INVDEP1 | payments | **REAL PROVIDER REQUIRED — vendor-selection criteria** | For INV-DEP-1 to hold with a real PSP, the adapter contract must guarantee: a unique reference per real capture; the same reference for one attempt across sync, callback and poll; "succeeded" meaning captured, not merely authorized, for two-phase PSPs; idempotent submission; signed, verifiable callbacks; reversal events that name the original reference; and a statement feed at the right granularity. Source: `rv-fh3-payments.md` §5. |
| PAY-RECON-N1 | ledger-finance + payments | **OPEN — follow-up, non-blocking for FH-3 (ledger-finance ruling, `rv-fh3-ledger.md`)** | A refund reported ONLY as the capture's own statement line with status `reversed` clears `pay_captured_unposted` for that run only. It is re-reported on later runs: a permanent false positive, over-reporting, never hiding money. Target: option (b), persist the statement-reported refund durably in the append-only `payment_statement_lines` and include it in clearing; no new schema and no ledger write. Interim, option (a): a vendor-intake requirement that the PSP sends a reversal callback or reversal line for every refund. A recurring hit with a historical `reversed` line is escalated as a known false positive, never suppressed. |
| FH3-FOLLOWUP-1 | payments (FH-3 owner) | **CLOSED 2026-09-28 — ledger-finance CONFIRMED at `5ee09e4` (`rv-fh3-ledger.md`, `3a38930`); merged `a72128d`** | F3b: `applyStatusEvidence` (sweeper.go) now maps the fresh, under-lock attempt state onto the §4.4 matrix's own cells before dispatching (succeeded × succeeded = no-op, disputed = recorded-only) instead of blindly re-running the live-attempt flow; corrected the misleading re-read comment; mutant F3S and each new switch case killed by two dedicated tests (`TestINVDEP1_F3b_SweeperEvidenceDecidesFromFreshState_*`). C4b: `TestC4_InitiateDepositAudited_WritesRefusalAudit` now calls `InitiateDepositAudited` end to end via a new `c4bAmbiguousThenAlreadyResolvedProvider` stub (uses `Deposit`'s own `req.MerchantReference` to inject a competing posting for the same fresh intent id before returning Ambiguous); kills C4W. L2: `TestA7_5c_*` now also requires `wait_event_type='Lock'` on the matched backend (`loBackendQueryAndWaitEventType`); the existing A7-TOMB-1/C8 mutation re-verified still killed. Evidence: `docs/plans/payment-readiness/evidence/prh-i1-mutation-kill.txt` (FH3-FOLLOWUP-1 section). Full `internal/payments` + `internal/reconciliation` green under `-race`; `golangci-lint` (pinned 2.9.0) 0 issues. |
| PAY-F3SM-TEST-1 | payments + qa | **OPEN — Low, non-blocking (ledger-finance, FH3-FOLLOWUP-1 confirmation)** | Mutant F3SM survives: the mismatch audit written when a poll contradicts an attempt that is already posted (`succeeded`) has no test. No money moves either way; only the audit write is unasserted. Fix: a mismatched-poll integration test that asserts the audit row, then re-run F3SM. |
| PAY-POLL-AMOUNT-1 | payments + ledger-finance | **OPEN — Low, non-blocking; launch-readiness item before a real PSP** | When a status poll (`QueryStatus`) reports success for a live attempt, the posting uses the attempt's own recorded amount; the amount/currency the poll reported is not cross-checked. The callback path does cross-check (mismatch → T10 disputed). INV-DEP-1 still holds (at most one posting per intent); the risk is a silent under/over-credit if a PSP captured a different amount. Fix: apply the same mismatch check on the poll path, routing to disputed. Tests: poll success with a different amount → disputed, no posting. | **Scope widened 2026-09-28 (architect FH7-06, `rv-fh7-architect-final.md`):** the poll path also does not cross-check the polled `provider_reference` against the attempt's stored one (`sweeper.go:513-521`: posting uses `res.ProviderReference` as `provider_tx_id` while `ApplySuccess` keeps the stored reference) — one posting (INV-DEP-1 holds) but mislinked; fix both amount and reference on the poll path → T10 dispute, as the payout path (N6/S-M1) does. Also a PAY-PSP-CONTRACT-INVDEP1 criterion. Before the first real PSP.
| PAY-SWEEP-CAS-NOISE-1 | payments | **OPEN — Low, non-blocking** | After F3b the sweeper's success branch has 0 CAS conflicts in the 25-round storm; the pending, ambiguous and decline branches still produce benign CAS-conflict retries (no financial effect, log/metric noise only). Fix: apply the same fresh-state short-circuit on those branches. |
| PAY-DEP-REF-VALIDATE-1 | payments (review: security) | **OPEN — Medium, latent; PROVIDER DEPENDENT; before the first real PSP** (architect FH7-05, `rv-fh7-architect-final.md`) | PROVIDER-REF-BOUND-1 security condition C1 requires `providerref.Validate` on every adapter-response reference. The payout path does this (`ErrorClassProviderRefInvalid` → park, `payout.go`). The deposit `Deposit`/`QueryStatus` responses do not (`drive.go:249-270` `depositAdapterCall`, `sweeper.go:353-365`): an over-bound reference hits the 0099 CHECK in phase C, the transaction rolls back, and the attempt loops/escalates instead of parking deterministically. Money-safe (no posting; INV-DEP-1 holds) but a real capture could sit unparked. Fix: validate deposit adapter-response references and park, mirroring payout; test + mutant. PRH-REF C1 is only half done until this closes. |
| CP-W1 | payments + devops (review: ledger-finance, security) | **OPEN — launch-blocking** (registered 2026-09-28 per architect FH7-11; named launch condition in `docs/plans/payment-readiness/prh-i1-payout-launch-conditions.md`) | No binary constructs a payments or payout `Sweeper`: `NewSweeper` has no non-test caller (architect-verified). So the sweeper-driven transitions (payout T2/T12, crash recovery, escalation, deposit QueryStatus polling and T17 re-drive) do not run in any current deployment (source: R5/CP-W1 in the launch-conditions note); ambiguous attempts are resolved only by callbacks or operators. Wiring it (process, schedule, metrics; its kill-switch/F-POOL-2/INV-DEP-1 behaviour is already tested) is required before real-money payments. |
| KYC-ENF-OUTAGE-1 | identity-compliance (+ security) | **OPEN — Medium; fail-closed (money safe)** (code-reviewer PRH-I3 FH-7 re-review N1) | For withdrawals every `unavailable` KYC outcome is a DB query failure (`enforcement.go:464-473`) that aborts the caller's transaction; `RequestWithdrawal` then calls `kyc.RecordDecision` in the aborted tx (`withdrawal.go:414`, SQLSTATE 25P02), so the handler returns a generic 500 instead of the B1 503, and no decision row is recorded (contrary to ADR 0096 §7.6). Reproduced with a `lock_timeout` probe. Fix: savepoint around the evaluator reads, or record the unavailable decision in a separate tx after rollback; fault-injection test; kill mutant MB1. Same class as the disclosed play-path gap (ADR 0096 §16.2). |
| KYC-ENF-DECISION-ROWS-1 | payments + identity-compliance (security informed) | **OPEN — Medium** (code-reviewer PRH-I3 FH-7 re-review N3) | Deposit (`payments/kycgate.go:56-68`, used at `deposit_v2.go:202` and `drive.go:100`) and payout (T1p allow `payout.go:291+`, T2/T12 deny `payout_sweep.go:99-130`) KYC evaluations write no `kyc_enforcement_decisions` row; only T1p deny does (via `DenyForCompliance`). Denials still write `audit_log` rows, but this contradicts ADR 0096 §7.6 and DR-PRHI3-04, and the staff decisions API shows no deposit/payout decisions. Fix, or record a disclosed deviation approved by security. |
| PAY-KYC-UNAVAIL-1 | payments (+ identity-compliance) | **OPEN — part of PRH-2 F-pay; must close before payout dispatch is used with real providers** (security C-F1, review of F-kyc) | After F-kyc's N5 (`DenyForCompliance` correctly refuses `OutcomeUnavailable` as retryable), `ClaimForDispatch` (`internal/payments/payout.go:292-297`) still passes every denial to it. So a KYC-store outage during staff payout submit returns a non-retryable 500 and records no decision or audit row. It is still fail-closed: no `Withdraw` call, and the request stays `approved`. Fix: handle `unavailable` before `DenyForCompliance`, commit the decision and the denied submit audit (LF-I3-3 pattern), and map the result to 503. **Sweeper T2/T12 currently escalate (T16)** (ledger-finance F-2, F-kyc review):
| INVDEP1-BACKSTOP-BRANCH-TEST-1 | payments (review: ledger-finance) | **OPEN — MEDIUM. Pre-existing; not caused by E2. Must close before I-wire wires ADR 0102 §8 row 2. Recommended in PRH-2 C/D.** (ledger-finance C-1, E2 review) | Mutant M2 survives the full payments suite at both `cabca27` and `9a25453`. M2 disables the INV-DEP-1 choke point's backstop branch (`postDepositSuccessOrDispute`, `orchestrator.go:738-746`: `ErrDepositIntentAlreadyResolved` → T10/T13d dispute). **Needed:** a test where the pre-check passes and `postDepositSuccess` then returns the sentinel, through its re-check or the X5 ledger index, via a real race or a test seam. **It must assert:** the dispute is applied; exactly one `payment.attempt_disputed`; the `payments_deposit_intent_index_backstop_fired` P1 is logged; no second posting; the transaction commits. This is a liveness and evidence gap, not a money-safety gap: the 0107 unique index still prevents a double credit. |
- **Today:** `gateAndEscalateOnDeny` (`payout_sweep.go:99-129`) treats an `unavailable` decision as a deny. It calls `Escalate` and audits `payout_reclaim_denied_by_kyc`, so a transient outage becomes a sticky escalation. No money moves.
- **Fix:** treat `OutcomeUnavailable` as transient: `RescheduleNonTerminal`, with no `Escalate` and no deny audit.
- **Tests:** an outage at T2 and at T12 leaves the attempt un-escalated and rescheduled, with no Withdraw and no posting. T1p under `unavailable` leaves the request `approved`, with no attempt row and no posting. Today the N5 mutant survives the whole payments suite.

Add a test, and correct the "future" wording in ADR 0096 §23 and the `ErrKYCUnavailable` comment. |
| KYC-ENF-TESTPINS-1 | identity-compliance | **OPEN — Low** (code-reviewer PRH-I3 FH-7 re-review N4/N5 + non-blocking items) | Pin with tests: `DenyForCompliance` B6 guard (mutant MB6 survived); casino/sportsbook play-deny decision rows (MPLAYREC survived). N5: `DenyForCompliance` accepts `Outcome=unavailable` (`withdrawal.go:1046-1051`) — refuse or make retryable. Also: B4 dormancy test + tenant-status filter; `ActiveRequiresLegalReviewReference` assert SQLSTATE/constraint (`enforcement_integration_test.go:967`); ADR 0096 §15.1 N2 row names the wrong tests (~line 2131); stale comment `withdrawal.go:388-390`. |
| KS-CAS-DISCRIM-TEST-1 | payments + qa | **OPEN — Low (test gap)** (code-reviewer kill-switch phase-2 FH-7 re-review N1) | `drive.go:157-163`: the `!engaged` discrimination in the cascade T2 kill-switch branch is untested (mutant K1 survived). A non-kill-switch CAS conflict would otherwise be recorded as `terminal_reason='kill_switch'` with a false audit (no money moves). Add a test forcing a non-kill-switch T2 conflict; re-run K1. |
| RECON-PAYOUT-LIVE-TEST-1 | ledger-finance + qa | **OPEN — Low (test gap)** (code-reviewer PRH-I5 FH-7 re-review N1) | No committed test reconciles the live, attempt-based payout path against the wired MOCK statement source (a reviewer probe showed 0 mismatches today). Commit an equivalent test in `internal/reconciliation` so a change to MOCK payout rendering or to `ApplyPayoutResult`/`PollPayoutStatus` cannot silently recreate the F1 false-P1 class for payouts. |
| TEST-T11A-FLIP-1 | orchestrator (qa) | **FIXED 2026-09-28 (`498559a`): T11a 200/200 under -race; httpserver tenant-binding + auth-failure-logging 10×; full race suites (payments, httpserver, reconciliation, withdrawal, idempotency, ledger, kyc, casino, webhookauth, cmd) green; lint 0** | Intermittent failure of `TestWebhook_BadSignature_NoWriteBeforeVerification` (payments T11a) on the FH3-FOLLOWUP-1 merge verification run: "expected at least one statement ... got none". Root cause (pre-existing test bug, not a product defect, not caused by the merge): six bad-signature tests built their tampered header with `Replace(sig, "0", "f", 1)`, else `Replace(sig, "1", "e", 1)`. When the 64-hex signature contained no `0` (probability (15/16)^64 ≈ 1.6%), the fallback rewrote the `v1=` prefix to `ve=`, so `ParseHeaders` rejected a *malformed* header before `ProviderAcceptsWebhook`. That produced the same `signature_invalid` reason, but through a different path than the test claims to exercise. Deterministic reproduction recorded in the orchestrator scratchpad (`^v1=[0-9a-f]{64}$` fails on the old flip of a no-`0` hex). Fix: flip only the last hex digit (always hex per the pattern), in payments T11a, httpserver payment/casino tenant-binding and auth-failure-logging, kyc and casino statement-capture tests. No assertion loosened; no product code changed. Process note: the orchestrator pushed `a72128d`/`a5cc1e6` before noticing this FAIL, because a pipeline without `pipefail` reported exit 0. Verification runs now use `pipefail` and grep for FAIL. |
| CAS-SESSION-EXPIRY-1 | casino | **FIXED 2026-09-28 (`80eda28`, merged by the orchestrator; found by the code-reviewer FH-7 re-review N1; ADR 0095 §15.1.5); code-reviewer re-review 2: N1 CLOSED, PRH-I2 casino READY WITH CONDITIONS (`rv-prh-i2-casino-code-review.md`); C2 comment fix applied.** `internal/casino/orchestrator.go` `postBet`'s `now() > expires_at` check now applies only to `status='active'` sessions, not `'consumed'` ones. **Qualification 2026-09-28 (security FH-7 re-review, I-4):** the fix applies to `consumed` sessions, but no production code consumes a launch token today (`ResolveLaunchToken` has no non-test caller; the MOCK `Launch` does not consume). So in the current wiring every `LaunchGame` session stays `active`, and bets are still refused about 2 minutes after launch — see CAS-PLAY-BOOTSTRAP-1. Security: refusing expired `active` sessions is correct and must not be relaxed. | HIGH regression from `2c00e10` (PRH-I2 item 2's own fix): `expires_at` is the un-consumed launch TOKEN's TTL (`DefaultLaunchTokenTTL`, 2 minutes), not an in-play bound, but the original fix applied it to `'consumed'` sessions too - every real-money round stopped accepting bets ~2 minutes after launch. Tests: `TestReceiveCallback_ConsumedSessionAcceptsBetAfterTokenTTLExpires` (regression repro), `TestReceiveCallback_NewBetRejectedPastExpiresAtEvenIfStillActive` (unchanged, still proves the `active` half). Mutation evidence: `docs/plans/payment-readiness/evidence/prh-i2-casino-mutation-kill.txt` §4. Reopened C4 as a side effect - see CAS-REVOKE-CONSUMED-1. |
| CAS-REVOKE-CONSUMED-1 | casino | **CLOSED 2026-09-28 (PRH-2 A, merged `086e45a`; migration 0108).** A consumed launch session can now be revoked. Security ACCEPT (`reviews/a-security.md`). Code review conditions A1–A8 closed (`a9ab5c4`). 7/7 mutants killed (`docs/plans/payment-readiness/evidence/prh2-casino-a-mutation-kill.txt`). Orchestrator verification (local, not CI): build, vet, gofmt, lint 0, `migrate verify` through 0108, `-race` integration for casino, txscope, cmd, sportsbook and the httpserver casino tests, all ok. CAS-PLAY-BOOTSTRAP-1 (PRH-2 B) is now unblocked. Earlier history: **Status 2026-09-28 (security ruling, `rv-prh-i2-casino-security.md` FH-7 re-review): MEDIUM; DEFERRED — LAUNCH-BLOCKING (option b).** Must be fixed (option a: a migration relaxing the 0036/0042 trigger to exactly `consumed → revoked` with whole-row equality, `RevokeLaunchSession` over `active`/`consumed` with `prior_status` audited, the specified tests and mutants) before the FIRST of: (i) any non-test caller of `ResolveLaunchToken`, such as a vendor bootstrap endpoint (same change or earlier); (ii) registering any non-synthetic casino adapter, or weakening the PROV-OUTBOUND-CRED-1 casino tripwire; (iii) any request for production launch authorization. Not reachable today (no session ever reaches `consumed`; MOCK only; tripwire holds). The "document as deliberate" option is withdrawn. Originally: **OPEN — HIGH, reopened by the CAS-SESSION-EXPIRY-1 fix; NOT IMPLEMENTED, blocked at the DB level without a migration** | Security review RV-PRH-I2's C4 ("revoke CAS misses a vendor-consumed session") was closed in §15.1.2 by relying on `postBet`'s expiry check applying to `'consumed'` sessions too - now corrected (CAS-SESSION-EXPIRY-1), so C4 is reopened: `RevokeLaunchSession`'s CAS (`WHERE status='active'`) never revokes a session the vendor already consumed before phase C recorded the launch as failed, and such a session is now bet-eligible indefinitely (no expiry bound applies to `'consumed'`). Widening the CAS to `status IN ('active','consumed')` is blocked by migration 0036/0042's `casino_launch_sessions_enforce_immutable_fields` trigger, which forbids ANY `UPDATE` transition out of a terminal status (`'consumed'`/`'expired'`/`'revoked'`), regardless of the target status - the widened `UPDATE` raises `casino_launch_sessions: row is immutable once consumed, expired, or revoked` and aborts the whole phase-C transaction. Fix requires either a migration relaxing the trigger for the specific `consumed -> revoked` transition, or a different mechanism (e.g. a separate revocation record consulted by `postBet` instead of a status mutation) - a decision for the architect/orchestrator, not a specialist to make unilaterally. Characterization test (documents, does not fix, the gap): `TestLaunchGame_FailedLaunchOnConsumedSession_RevokeCASMissesAndBetStillAccepted` (`internal/casino/launch_two_phase_integration_test.go`) | before the first real casino adapter, and before this gap is exploited in the MOCK-only window |
| CAS-PLAY-BOOTSTRAP-1 | casino (+ security, product-owner-proxy) | **Endpoint IMPLEMENTED (MOCK) 2026-09-28 (PRH-2 B, merged `9c6d1d5`; migration 0111).** `POST /v1/webhooks/casino/{tenantSlug}/{providerID}/launch-bootstrap`, per ADR 0103: Redeem+Recheck first, uniform refusal, gate denial revokes and commits, a binding-aware CAS, the session lock `FOR NO KEY UPDATE` (ABBA deadlock with `postBet` fixed and pinned), idempotency inside a single transaction, opaque provider-scoped player refs, and a MOCK vendor client over HTTP. Contract: `docs/integrations/casino-launch-bootstrap.md`. Reviews: architect/QA/POP, security (hard gate; ADR 0103 §4 acknowledgement binding) and code review, all conditions closed (`reviews/b-*.md`). The orchestrator re-killed MF1 (lock revert → 40P01) and MC2 (cross-token conflict accept, 3/3). Verification (local, not CI): build/vet/gofmt/lint 0, `migrate verify` through 0111, `-race` casino/audit/txscope/rg/cmd/full httpserver ok, alerting ok after the 0110 test-cap fix (`fade02b`). A real vendor is PROVIDER DEPENDENT; CAS-PLAYER-REF-1 is required first. **Still OPEN (functional, MOCK play):** the B2C simulation routes stay `active`-only (security P2-2 ruling, pinned by a test), so the ~2-minute MOCK-play window that motivated this row is **not** fixed by B. The clean fix (the simulation routes become a MOCK vendor client) is to be decided with CAS-BET-REQUIRES-BOOTSTRAP-1. **Follow-up:** delete `ResolveLaunchToken` (ADR 0103 §11 item 5; a static guard now forbids non-test callers). History:  **OPEN — functional limitation (MOCK play); REAL PROVIDER / next-stage design** (security FH-7 re-review I-4) | No vendor token-bootstrap path exists: `ResolveLaunchToken` has no production caller, so launched sessions never become `consumed`. Since `2c00e10` (PRH-I2 item 2, kept by security), a never-consumed `active` session refuses new bets once its 2-minute launch-token TTL passes. So the B2C MOCK play route (`casino_play_handlers.go`) accepts bets for about 2 minutes per launch; the player must relaunch. Settlement of earlier bets is unaffected (win/rollback resolve via `correlation_id`). Fix: build the vendor bootstrap (token consume) endpoint, together with or after CAS-REVOKE-CONSUMED-1 (security gate (i)). Do NOT relax the `active`-session expiry. |
