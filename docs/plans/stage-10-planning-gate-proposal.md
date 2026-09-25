# Stage 10 Planning Gate — State Reconstruction and Next-Stage Proposal

**Status: PLANNING ONLY — NOT AUTHORIZED — NOT IMPLEMENTED.**
No migration, application code, Terraform, IAM or AWS change is made by
this document. It is the Master Orchestrator's decision-ready proposal
for the human approval gate required by `CLAUDE.md` "Stage-gate rule"
and `MASTER-BUILD-PROMPT.md` "Stages" (a new stage is proposed only as a
recorded decision). On approval, the stage definition is recorded as an
ADR (see §K) before any implementation starts.

- Prepared: 2026-09-25, Master Orchestrator.
- Repository state reviewed: branch `claude/focused-wright-jw88w9`,
  HEAD `9190d5d01da076141a1f70d7e5897a573d3b18f4` (the deployed staging
  commit).
- Specialist review round: see §9 (consolidated) — DRAFT pending.

---

## 1. Master Orchestrator state

The Master Orchestrator remains the single coordination authority
(`docs/governance/agent-registry.md` "Master Orchestrator";
`MASTER-BUILD-PROMPT.md` "Orchestration model"). This document was
produced from the repository itself, not from conversation memory:
four independent read-only reconstruction passes (governance, human
decision registers, stage history, code reality) plus Orchestrator-run
verification (§2.3). Where the repository's own records disagree with
each other or with the code, that is stated as a finding (§2.4), not
silently resolved.

## 2. Current project state

### 2.1 Where the project is

| Item | State | Evidence |
|---|---|---|
| Last recorded stage | Stage 9.4 — Staging Infrastructure Hardening + Cost Optimization — "complete (repository-side; not deployed)" | `docs/progress.md` §"Stage 9.4 — Staging Infrastructure Hardening…"; `docs/active-stage.md` "Current stage: Stage 9.4" |
| Staging deployment | **Executed by the human** from the allowlisted workstation (not by an agent — the agent sandbox was blocked by its own egress IP and TLS interception) | Human attestation, 2026-09-25 (this directive); commits below |
| Staging acceptance | **PASSED — human-attested**: real B2C browser acceptance (incl. the previously failing sportsbook selection flow) and manual Back Office verification | Human attestation, 2026-09-25. **Not independently re-verified by the Orchestrator** (no access from the agent environment; see §2.4 F-6) |
| Deployed commit | `9190d5d01da076141a1f70d7e5897a573d3b18f4` | Human attestation; branch HEAD |
| Stage 10 | **Not defined, not authorized, not started** | every Stage 9.x record: "Stage 10 is NOT authorized" |

### 2.2 Commits after the last recorded stage (not previously recorded in `docs/`)

| Commit | Summary | Scope |
|---|---|---|
| `221b6ef` | fix: run frontend nginx pid as non-root — `frontend.Dockerfile` pid rewrite accepts `/run` and `/var/run`, build fails unless `pid /tmp/nginx.pid;`; new `deploy/docker/tests/frontend-image-smoke.sh`; new CI job `frontend-image` | Docker/CI only |
| `b22d5c4` | fix(aws): admit CloudFront VPC origin to the internal ALB via the CloudFront origin-facing managed prefix list (tcp/80), drop the 443 rule and `alb_ingress_cidrs`; deployer network policy gains `ec2:GetManagedPrefixListEntries` | Terraform + IAM policy JSON |
| `8ec3dc7` | feat(ui): complete B2C and Back Office workflows for the staging acceptance run (email verification card, deposit simulate-confirmation button, withdrawal page, KYC/RG status card, casino idempotency key, bet-slip rejection handling; Back Office withdrawal queue/detail, tenant/brand/staff forms, configuration page, casino catalogue page) | `b2c/`, `backoffice/` only — no backend/Terraform/IAM |
| `9190d5d` | fix(b2c): selections are pickable when `market.status === 'open' && sel.status === 'active'` (backend selection statuses are `active`/`suspended`), fixtures corrected, regression tests | `b2c/` only |

**Governance note:** `b22d5c4` changed Terraform and a deployer IAM
policy document. Its review record (security / devops sign-off, Access
Analyzer validation of the changed policy, the 28-case simulation) is
not in the repository. Recorded as finding F-5; retroactive review is
proposed in §8 Workstream 0.

### 2.3 Orchestrator verification at `9190d5d` (this session, local, synthetic data only)

| Check | Result |
|---|---|
| `go build ./...`, `go vet ./...` | PASS |
| `gofmt -l .` | PASS (clean) |
| `go test ./...` (unit) | PASS (27 packages ok) |
| `go run ./cmd/migrate up` + `verify` on a fresh PostgreSQL 16 DB (non-superuser owner, non-owning runtime role, `schema_migrations` write-revoked — mirrors `.github/workflows/ci.yml`) | PASS — 90 migrations, verified clean, no gaps |
| `go test -race -tags=integration ./...` with the CI role setup exactly (`NOCREATEDB`) | **FAIL — 6 packages** (`bonus`, `db`, `jurisdiction`, `ledger`, `operatingmarket`, `sportsbook`): every failure is `permission denied to create database` in scratch-database migration/RLS tests |
| Same 6 packages with `CREATEDB` granted to the local test role | PASS (all packages) |
| Migration reversibility (`-steps=4 down`, `up`, `verify`) | PASS |
| `golangci-lint run ./...` (v2.5.0, repo `.golangci.yml`) | **FAIL — 19 issues** (13 errcheck, 6 staticcheck; unchecked `Close`/`Fprintf` in mocks, handlers, tests; one S1016) — none in ledger/wallet/money code |
| GitHub Actions CI, last 60 runs on this branch | **All `failure`** — `build-test-lint` fails at "Set up job": `Unable to resolve action golangci-lint/golangci-lint-action, repository not found` (`.github/workflows/ci.yml:99`, introduced `4790de0`). `frontend`, `frontend-image`, `infrastructure` jobs pass. |

### 2.4 Findings from the reconstruction (recorded, not fixed here)

- **F-1 (P1, process) — CI's Go gate has never executed on this
  branch's history.** The `build-test-lint` job (gofmt, vet, lint, build,
  migrations up/verify, runtime-role narrowing, unit + integration tests
  with race detector, migration reversibility) aborts before its first
  step because the action reference `golangci-lint/golangci-lint-action`
  does not exist (correct upstream: `golangci/golangci-lint-action`).
  Every stage record's Go verification is therefore **local
  Orchestrator evidence only**, never CI evidence. This does not
  invalidate the recorded results (they were re-confirmed locally in
  §2.3), but `change-control.md` Rule 7's evidence standard and the
  "real HTTP test"/"round-trip" rows assume a working gate.
- **F-2 (P2) — scratch-database integration tests require `CREATEDB`,
  which neither CI (`ci.yml:64`) nor `deploy/init-app-role.sql` grants.**
  Once F-1 is fixed, CI would go red on these tests. The requirement is
  undocumented.
- **F-3 (P3) — `golangci-lint` is not clean (19 issues)**, contradicting
  recorded "0 issues" claims; likely a linter-version difference (CI pins
  `version: latest`).
- **F-4 (P2, record integrity) — project memory is stale.**
  `docs/governance/project-status.md` stops at Stage 4I and still lists
  decisions ADR 0042 has answered; `docs/progress.md` header says "Stage
  8"; `docs/active-stage.md` says Stage 9.4 "not deployed" and carries a
  stale 4H-B1 "Current stage" header at line ~1444; several
  `task-registry.md` rows still read "Dispatched"/"In progress"; Stage
  5–9.4 final reports were delivered in chat and not committed.
- **F-5 (P2, governance) — `b22d5c4` (Terraform + IAM policy) has no
  recorded specialist review** (see §2.2).
- **F-6 (information) — staging acceptance is human-attested only.** No
  acceptance log/evidence artifact exists in the repository. The
  Orchestrator records it as attested, not as independently verified.

## 3. Governance verification

Verified against the written governance (paths per principle). "Explicit"
= established in a permanent governance file; "Stage/ADR" = established
only in a stage directive, ADR or human-decision record (still binding,
but not in the permanent rulebook).

| Principle | Status | Where |
|---|---|---|
| Orchestrator coordinates all work | Explicit | `agent-registry.md` "Master Orchestrator"; `MASTER-BUILD-PROMPT.md` "Orchestration model" |
| Specialists stay in domain; no silent overwrite | Explicit | `agent-registry.md` "Absolute constraint…"; `ownership.md` Rules 1–4 |
| Cross-domain dependencies via Orchestrator | Explicit | `change-control.md` "Cross-domain change"; `integration-protocol.md` "Dependency requests" |
| No invented human decisions; decisions gated | Explicit | `CLAUDE.md` "When to stop and ask"; HDR files 0039/0041/0044 ("Record only") |
| No production claims without evidence | Explicit (partial) | `CLAUDE.md` "No fake completion"; `change-control.md` Rule 7 |
| No certification claims | Explicit (by implication) | `CLAUDE.md` "Compliance" |
| Financial invariants; append-only double-entry; idempotency | Explicit | `CLAUDE.md` "Financial / ledger rules" |
| RLS mandatory; tenant isolation | Explicit | `CLAUDE.md` "Multi-tenancy"; `change-control.md` schema row |
| Auditability | Explicit | `CLAUDE.md` "Security" |
| Security review for sensitive changes | Explicit | `CLAUDE.md` "Security"; `change-control.md` Rule 3 |
| Fail-closed where required | Stage/ADR | `.claude/agents/risk.md`; ADR 0031; ADR 0085; `stage-4i-exit-register.md` |
| Provider-neutral abstractions; no fake real providers | Explicit | `CLAUDE.md` "Provider abstraction", "No fake completion" |
| Jurisdiction first-class | Explicit | `CLAUDE.md` "Compliance" |
| Jurisdiction operation-specific | Human decision | ADR 0042 HDR-J-2; ADR 0044 HDR-J-7 |
| Licensing / operating-market decisions governed | Explicit | `CLAUDE.md` "When to stop and ask"; ADR 0006; exit register HDR-J-6, HDR-M-1/2 |
| Human decisions not reopened without justification | Stage/ADR | `MASTER-BUILD-PROMPT.md` preamble; Stage 9.2 operating principle (`task-registry.md`) |
| Infra not changed casually | Partial | `.claude/agents/devops.md`; `CLAUDE.md` "Environment safety"; no infra row in `change-control.md` |

**Conclusion:** every principle the directive lists remains active; none
has been reversed. Three are not yet in the permanent rulebook
(fail-closed, no-reopen, infra change control). Proposed as a
documentation-only governance consolidation in Workstream 0 — not a new
rule, a promotion of existing binding rules into `change-control.md`.

## 4. Completed stages

| Stage | Record | Commit(s) |
|---|---|---|
| 0–3 (discovery, foundation, identity/tenancy/security, wallet/ledger/payments) | `docs/progress.md` early sections | see progress.md |
| 4A–4G, 4G-FINAL, 4G-FINAL-FINANCE-GATE | `docs/progress.md` | see progress.md |
| 4H-A, 4H-B0 … 4H-B0-R7 (bonus/gamification/retail/asset/FX architecture; `player_locked` split, migration 0048) | `docs/progress.md`; ADRs 0032–0040 | see progress.md |
| 4H-B1 Waves 1–3 (bonus engine, migrations 0050–0070) | `docs/progress.md`; `docs/governance/wave-*-report.md` | `d145ba1`…`cd6ee62` |
| 4I foundation + Phases A–E, E-SECURITY, Exit Triage (jurisdiction mechanism, operating market mechanism-only, registry RLS; migrations 0071–0077) | `docs/progress.md`; `docs/governance/stage-4i-*.md`; `docs/plans/stage-4i-jurisdiction-implementation-plan.md` | `c8a57eb`, `4e911a9`, `5bb34fd`, `027d0c2`, `bbe404e`, `e554727`, `915bfa9`, `bcf2413` |
| 5 Back Office MVP | `docs/progress.md` | `e77b75b` |
| 6 / 6.1 B2C + first sportsbook slice (singles, bet placement only, mock provider) | `docs/progress.md`; ADR 0047 | `be6042d`, `cb8a0b3` |
| 7 Casino B2C slice (play simulation; ADR 0048) | `docs/progress.md` | `651d5d1` |
| 8 Provider integration readiness (no external contracts; ADR 0080) | `docs/progress.md` | `3c61873` |
| 9 / 9.1 / 9.2 Production readiness, blocker closure, sportsbook risk + jurisdiction enforcement (ADRs 0081–0083) | `docs/progress.md` | `7464fde`, `d09e4b3`, `83ff95d` |
| 9.3 Staging package + local end-to-end acceptance (ADR 0084) | `docs/progress.md` | `1c49068` |
| 9.4 Part 1 APP_ENV fail-closed (ADR 0085) | `docs/progress.md` | `627a1dd` |
| 9.4 Staging hardening (ADR 0086) | `docs/progress.md` | `4e49179`, `fc60d58`, `09553fa`, `5c4ac51`, `af5bfef`, `fa3dd5c` |
| 9.4 Staging deployment + acceptance (human-executed) | this document §2 | `221b6ef`, `b22d5c4`, `8ec3dc7`, `9190d5d` |

## 5. Remaining work (status by area)

Labels per `CLAUDE.md` "No fake completion". "Blocked by" names the
gating item; **H** = human decision, **V** = vendor/contract, **E** =
engineering dependency.

| Area | Status | Blocked by |
|---|---|---|
| Sportsbook settlement (win/loss) | NOT IMPLEMENTED (ADR 0038 §5 RESOLVED for cash-funded; no ledger type, no route, no service) | — (E only) |
| Sportsbook void | NOT IMPLEMENTED (ADR 0038 §8.1; ADR 0083 §6.1.2 / INV-SB-CUM-1) | — (E only) |
| Sportsbook resettlement / correction | NOT IMPLEMENTED (ADR 0038 §10) | E: settlement |
| Sportsbook partial settlement | NOT APPLICABLE while singles-only (ADR 0038 §8.2 is multi-leg) | E: accumulators/bet-builder (not authorized) |
| Sportsbook cashout | NOT IMPLEMENTED (ADR 0038 §8.3); cash-funded: needs provider-priced offer; bonus/mixed-funded: human decided "not cashout-eligible" (ADR 0042) | V: provider cashout pricing; E: settlement |
| Sportsbook liability/exposure | Exposure gate IMPLEMENTED but **unarmed** (zero `sb_exposure_limits` rows); no liability reporting | H: HDR-SB-1 (production go-live) |
| Sportsbook jurisdiction rung 2 (`SB-JUR-RUNG2-1`) | SPECIFIED, NOT IMPLEMENTED | H: HDR-J-7 |
| Open-bet self-exclusion treatment | Policy infra IMPLEMENTED; default `VOID_ON_SELF_EXCLUSION` decided (ADR 0042) but **not seeded** (fails closed); ADR 0034 §14.10–14.13 design complete | E: void; H: seeding authorization (legal review flagged) |
| Jurisdiction / operating-market completion | MECHANISM ONLY (4I A–E); resolver wiring, content, `MKT-DUAL-1` absent | H: HDR-J-6, J-7, J-8, J-9, M-1, M-2; HDR-J-3e/3f legal |
| Real sportsbook provider | MOCK only; contract "PENDING, no documentation" (`docs/integrations/dummy-sportsbook.md`) | V |
| Real casino provider | MOCK only (`docs/integrations/dummy-casino.md`) | V |
| KYC provider | MOCK only | V (named "single highest-leverage open item", `project-status.md`) |
| Payment provider | MOCK only (`mock-payments`) | V |
| Withdrawal lifecycle | Request + four-eyes IMPLEMENTED; no real payout, no withdrawal callback/reconciliation path | V: PSP |
| B2B tenant/partner capabilities | PARTIAL (admin APIs + Back Office forms); no partner console, no self-serve/BYOL onboarding | E + product sequencing (doc 35 Stage 6B) |
| Retail/POS | NOT IMPLEMENTED | H: doc 27 §24 items 1–14 |
| Production hardening | Launch gates open: audit client IP (ADR 0086), Back Office refresh token storage (Stage 5), KMS/HSM signing (ADR 0018), staff MFA status unverified (ADR 0017), `PLAT-ROLESPLIT-1` production step, `IssueCredentialToken` race (ADR 0085) | E (most); H for production credentials |
| Compliance / certification readiness | No certification claimed or performed | H: licence/jurisdiction; V: test lab |
| Observability | Logging/tracing in code; no metrics backend deployed; staging alarms only | E; H (production provider/AUP) |
| DR / backup | "NOT MET" (`docs/runbooks/backup-and-disaster-recovery.md`) | H: ADR 0009 AUP / production provider |
| Scaling / load testing | Go concurrency tests only; no load test | E |
| CI / evidence pipeline | **Go gate broken (F-1, F-2, F-3)** | — (E only) |
| Record integrity | Stale status docs (F-4), unreviewed infra commit (F-5) | — (E only) |

## 6. Dependency graph

```
                    ┌──────────────────────────────────────────────┐
                    │ W0  CI gate restoration + record integrity   │  (no human decision needed)
                    │     F-1 action path, F-2 CREATEDB, F-3 lint, │
                    │     F-4 stale docs, F-5 retro review         │
                    └───────────────┬──────────────────────────────┘
                                    │ every later stage relies on CI evidence
          ┌─────────────────────────┼───────────────────────────────────────┐
          ▼                         ▼                                       ▼
 ┌─────────────────────┐  ┌──────────────────────────┐        ┌───────────────────────────┐
 │ SB settlement +     │  │ Production hardening     │        │ Jurisdiction completion   │
 │ void + correction   │  │ (engineering-only gates):│        │ BLOCKED: HDR-J-6/7/8/9,   │
 │ (cash-funded,       │  │ audit client IP, BO      │        │ HDR-M-1/2, J-3e/3f legal  │
 │ singles, mock feed) │  │ refresh cookie, token    │        └────────────┬──────────────┘
 └───┬───────────┬─────┘  │ race, KMS signing, MFA   │                     │
     │           │        └────────────┬─────────────┘                     ▼
     │           ▼                     │                        SB rung 2 (SB-JUR-RUNG2-1)
     │   open-bet self-exclusion       │                        operating-market wiring
     │   void (needs seeding approval) │
     ▼                                 ▼
 SB cashout ◄── V: provider pricing   Production launch ◄── H: ADR 0009 AUP, licence/markets (HDR-J-6),
 SB liability reporting ◄── H: HDR-SB-1                     HDR-SB-1, KYC/PSP/casino/SB vendor contracts,
 Real SB provider ◄── V                                     DR (needs production provider), PLAT-ROLESPLIT-1 step,
                                                            legal RG/KYC/AML, data retention
 Real casino / KYC / PSP ◄── V   ──►  withdrawal payout + callbacks + reconciliation
 B2B partner console / BYOL ◄── E (after B2C MVP loop closes) + H (BYOL onboarding specifics)
 Retail/POS ◄── H (doc 27 §24 items 1–14)
 DR/backup, load testing, observability backend ◄── H (production provider) / E
```

**Reading of the graph.** Everything that leads to production is gated
by human decisions or vendor contracts except two things: (1) the CI /
evidence pipeline, which every other stage depends on, and (2) the
sportsbook lifecycle after placement, which is the only unimplemented
core B2C money flow whose architecture is already RESOLVED and needs no
new human decision for the cash-funded case. Today a placed bet locks
the stake in `player_locked_cash` forever: the B2C sportsbook loop is
open-ended, reconciliation cannot yet cover sportsbook, and the risk
engine's cumulative spec deliberately over-counts until a void type
exists (ADR 0083 §6.1.2).

## 7. Open human decisions (not decided here)

| ID | Decision | Blocks | Source |
|---|---|---|---|
| ADR 0009 residual | Written gambling-AUP / contractual permission / data residency from the hyperscale provider | Any production workload; DR design | `docs/decisions/0009-*`; ADR 0084/0086 |
| Licence / markets beyond Anjouan; **HDR-J-6** first-brand permitted markets ("not yet determined") | Launch; resolver wiring; ceiling content; `MKT-PM-1` | ADR 0042 HDR-J-6; `project-status.md` |
| **HDR-J-7** OperationClass → Purpose | Resolver wiring; `SB-JUR-RUNG2-1` | ADR 0044 |
| **HDR-J-8 / J-9** location signal required/advisory; staleness | Market-access evaluation policies | ADR 0044 |
| **HDR-J-3e / 3f** lawful basis; retention (legal counsel) | Production use of location/residence evidence | ADR 0042 |
| **HDR-M-1 / M-2** operating-market enablement governance; existing players after disablement | Any operating-market write route | `stage-4i-exit-register.md` |
| **HDR-SB-1** trading-book liability owner; exposure ceiling before go-live | Sportsbook production go-live | ADR 0083 §8.2 |
| FD-2 settlement-finality window | Optional bonus-conversion narrowing | `ledger-accounting-model.md` |
| Vendors: KYC, PSP, casino aggregator, sportsbook feed, crypto custodian | Real-money launch | `project-status.md`; ADR 0005 Q4 |
| `project-status.md` open decisions 1–7 (KYC reuse ADR 0028; BYOL person-resolution opt-out ADR 0027; RiskDecision REVIEW; HARD vs CONFIGURABLE limit precedence ADR 0031 §5; platform-scoped risk-rule write path ADR 0031 §8; …) | Real-money launch | `docs/governance/project-status.md` "Open decisions" |
| Retail doc 27 §24 items 1–14 | Retail/POS | `docs/architecture/27-*.md` §24 |
| Legal RG/KYC/AML interpretation; data-retention periods | Real-money go-live | `project-status.md`; Stage 9 §19 |

**Resolved and binding (not reopened):** HDR-J-1/2/4/5 (ADR 0042), G-2
(configurable, default (b)), `OpenBetSelfExclusionPolicy` default
`VOID_ON_SELF_EXCLUSION`, bonus/mixed-funded cashout "not
cashout-eligible", FD-1 "nullifying", converted-Grant cancellation "no
clawback" (ADR 0042), APP_ENV semantics (ADR 0085).

## 8. Proposed next stage

### F. Proposal

**Stage 10 — CI Evidence Restoration + Sportsbook Settlement Lifecycle
(cash-funded singles: settle, void, correction) on the provider-neutral
settlement boundary, driven by the mock provider.**

Two workstreams, strictly ordered:

- **Workstream 0 (W0) — Evidence pipeline and record integrity.** Must
  complete and show a green CI run before W1 code merges.
- **Workstream 1 (W1) — Sportsbook settlement lifecycle.**

### G. Why this stage is next

1. It is the only remaining core B2C MVP money flow that needs **no new
   human decision** (cash-funded settlement/void/correction are RESOLVED
   architecture: ADR 0038 §5, §8.1, §10, §11, §12; ADR 0083 §6.1.2,
   INV-SB-CUM-1, §7.3 INV-LOCK-E3).
2. Every alternative is gated: jurisdiction completion (HDR-J-6/7/8/9,
   M-1/2), real providers (contracts), production hardening's heaviest
   items (ADR 0009 AUP), retail (doc 27 §24), liability reporting
   (HDR-SB-1).
3. It closes an open financial loop: stakes locked in
   `player_locked_cash` today can never be released, which is
   operationally wrong even in staging and leaves reconciliation and the
   risk engine's cumulative accounting knowingly incomplete.
4. W0 is placed first because every rule in `change-control.md` assumes a
   working CI gate, and F-1 shows it has never run; a financial stage
   must not be the first to rely on local-only evidence again.

Rejected alternatives (recorded, not built): production-hardening-first
(valuable but its launch gates are not on the critical path while
production itself is human-blocked; several engineering-only gates are
proposed as the stage after this one); B2B partner console first
(`MASTER-BUILD-PROMPT.md` commercial objective 1 — own B2C brand — is not
yet a closed loop); cashout (needs provider pricing — building a
platform-side price would invent trading, which the platform does not
own per `CLAUDE.md` "What this project is").

### H. Exact scope

**W0 — Evidence pipeline and record integrity**

1. Fix `.github/workflows/ci.yml` action reference to the real upstream
   action (`golangci/golangci-lint-action`) and pin a linter version
   (not `latest`) so results are reproducible.
2. Make the scratch-database integration tests runnable in CI without
   weakening the application roles: a **dedicated test-only admin
   connection** (e.g. a `TEST_ADMIN_DATABASE_URL` used only for
   `CREATE/DROP DATABASE` by test helpers) rather than granting
   `CREATEDB` to `igaming`. Document it in `docs/testing/testing-strategy.md`.
   Final mechanism: `qa` + `security` + `devops` decide.
3. Fix the 19 lint findings (errcheck/staticcheck) — no behavior change.
4. Retroactive review of `b22d5c4` (Terraform + deployer IAM policy):
   `security` + `devops`, Access Analyzer `ValidatePolicy` and
   `simulate-deployer-policies.py` against the changed network policy,
   recorded in `task-registry.md`.
5. Record integrity: `project-status.md` status refresh, `progress.md`
   header, stale `active-stage.md` header, stale `task-registry.md`
   rows; promote the three stage-level binding rules (fail-closed,
   no-reopen, infrastructure change control) into `change-control.md`
   (documentation of existing rules; no new rule).
6. Record the staging acceptance evidence the human can supply (test
   IDs/checklist, not credentials) in the Stage 9.4 runbook.

**W1 — Sportsbook settlement lifecycle (cash-funded singles)**

1. Ledger (owner `ledger-finance`): admit `sportsbook_settlement`,
   `sportsbook_void`, `sportsbook_rollback` transaction types
   (migration widening the `transaction_type` CHECK), postings per ADR
   0038 §5/§8.1/§10 against `player_locked_cash` (post-0048 accounts),
   and — in the **same change** — `ReversalTypes` for
   `OperationSportsbookBet` (INV-SB-CUM-1).
2. Sportsbook domain (owner `sportsbook`): provider-neutral settlement
   event model (settle won/lost with provider-stated payout, void,
   settlement rollback/resettlement); bet status transitions
   `open → settled_won/settled_lost/void` with an immutable history;
   idempotency per ADR 0038 §11/§14 keyed on the settlement's own
   provider reference; lock ordering per ADR 0082 / INV-LOCK-E3; exposure
   automatically excludes non-open bets (already true by construction).
3. Mock provider settlement driver (owner `sportsbook` + `integrations`):
   a signed provider-webhook ingress mirroring the casino/payments
   pattern (`POST /v1/webhooks/sportsbook/{tenantSlug}/{providerID}`) and
   a **test-support-gated** settlement trigger usable in staging,
   behind the existing `TestSupportRoutesEnabled()` double opt-in (ADR
   0085). No real provider contract is invented; the webhook shape is the
   mock adapter's, translated at the adapter boundary.
4. Reconciliation (owner `ledger-finance`): extend the reconciliation
   job's coverage to sportsbook settlement per ADR 0038 §12 (posted
   amount recomputable from stored inputs; provider-statement
   reconciliation remains PROVIDER DEPENDENT).
5. Audit: `sportsbook_bet.settled`, `.voided`, `.settlement_rolled_back`
   audit events (ADR 0038).
6. API/UI read surface (owners `frontend`, `backoffice`): player bet
   history shows settled/void outcomes; Back Office bet list shows
   settlement status and settlement transaction references. **No
   operator manual-settlement button** (see out of scope).
7. Tests per `CLAUDE.md` financial list: normal, duplicate, concurrency
   (settle vs void race, double settlement), retries, partial failure,
   rollback/correction, settlement, reconciliation, provider callbacks
   (signature, replay), idempotency, authorization (tenant scope, RLS,
   player cannot trigger settlement), auditability; ledger invariant
   `SUM(debits)=SUM(credits)` and projection-vs-ledger drift = 0.

### I. Explicitly out of scope

- Cashout (any funding) — needs provider pricing (V); bonus/mixed-funded
  already decided not eligible.
- Bonus-funded sportsbook staking/settlement (ADR 0038 §9; not built;
  G-2 configurability deferred).
- Partial settlement, accumulators, bet-builder, live odds.
- **Operator manual settlement** from the Back Office (a new
  privileged financial control; needs its own four-eyes/threshold design
  and human approval).
- Automatic void of open bets on self-exclusion — unless the human
  separately authorizes seeding `VOID_ON_SELF_EXCLUSION` (see §V item 3);
  without that authorization the policy stays unconfigured and fails
  closed, as today.
- Liability reporting / arming exposure limits (HDR-SB-1).
- Jurisdiction rung 2 (HDR-J-7), operating-market wiring, any
  jurisdiction content.
- Real sportsbook/casino/KYC/PSP integrations; crypto custody.
- B2B partner console, retail/POS, reporting/ClickHouse.
- Terraform/IAM/ECS/RDS/CloudFront/network changes (none required); no
  staging redeploy unless separately authorized after the stage.
- Production anything.

### J. Required specialist agents

| Agent | Role in Stage 10 |
|---|---|
| `devops` | W0 CI fix, test-admin connection wiring in CI, `b22d5c4` retro review (with security) |
| `qa` | W0 test-environment mechanism; W1 test plan and gate |
| `security` | W0 test-admin mechanism and `b22d5c4` review; W1 webhook signature/replay, test-support gating, RLS, authorization review |
| `ledger-finance` | W1 ledger types, postings, reversal types, reconciliation; financial sign-off |
| `sportsbook` | W1 domain model, state machine, orchestrator, mock provider driver |
| `integrations` | W1 settlement webhook adapter pattern consistency |
| `risk` | W1 `ReversalTypes` change review (INV-SB-CUM-1) |
| `architect` | Stage ADR; cross-domain consistency (sportsbook ↔ ledger ↔ risk ↔ reconciliation) |
| `frontend`, `backoffice` | W1 read-only status surfaces |
| `code-reviewer` | Independent review of every W0/W1 change |
| `product-owner-proxy` | Scope guard (esp. no manual settlement, no cashout) |

### K. Required ADRs

- **ADR 0087 (new)** — Stage 10 definition and scope (this proposal,
  once approved), per `MASTER-BUILD-PROMPT.md` "Stages".
- **ADR 0088 (new)** — Sportsbook settlement implementation contract:
  the provider-neutral settlement event, state machine, the mock
  webhook's trust boundary (analogous to ADR 0048 for casino), and how
  ADR 0038's `player_locked` postings map onto the post-0048
  `player_locked_cash` account. Amends (does not reopen) ADR 0038 status
  for §5/§8.1/§10 from NOT IMPLEMENTED to IMPLEMENTED on completion.
- ADR 0083 §6.1.2 status note (ReversalTypes now populated) — an
  amendment note, not a new decision.
- No ADR needed for W0 items 1–3 (tooling); W0 item 2's test-admin
  connection is recorded in `docs/testing/testing-strategy.md` and
  `docs/security/runtime-role-separation.md` if roles are touched.

### L. Required database changes (W1 only; none in W0)

- Migration widening `ledger_transactions.transaction_type` CHECK with
  `sportsbook_settlement`, `sportsbook_void`, `sportsbook_rollback`
  (up/down/up round-trip; dirty-down rejection per repo convention).
- Settlement/void idempotency storage: a unique constraint on the
  settlement event's provider reference per `(tenant_id, provider_id,
  reference)` — enforced by the database (`CLAUDE.md`), exact table
  shape per ADR 0088.
- `sportsbook_bets` status transition support (settled_at, outcome
  columns or a separate append-only `sportsbook_bet_settlements` table —
  ADR 0088 decides; FORCE RLS on any new tenant-owned table; runtime-role
  grants per `runtime-role-separation.md`).
- No change to `player_locked_*` account types (0048 already split them).

### M. Required API changes (W1)

- `POST /v1/webhooks/sportsbook/{tenantSlug}/{providerID}` — signed
  provider callback (mock provider only).
- A test-support-gated settlement simulation route (exact path per ADR
  0088), registered only when `TestSupportRoutesEnabled()`.
- Read-only additions: settlement status/outcome/payout on
  `GET /v1/me/sportsbook/bets` and `GET /v1/admin/sportsbook/bets`.
- OpenAPI (`docs/api/openapi/platform-api.yaml`) updated for all of the
  above (`change-control.md`).

### N. Required frontend changes (W1)

- B2C bet history: show won/lost/void and payout.
- Back Office sportsbook bets list/detail: settlement status, payout,
  ledger transaction reference. No settlement controls.

### O. Security / RLS impact

- New tenant-owned tables (if any) carry `tenant_id` + FORCE RLS +
  adversarial isolation tests; runtime role gets least privilege.
- Webhook: HMAC signature, timestamp/replay window, per-tenant provider
  credentials via existing provider config (no secrets in repo).
- Test-support settlement route: registered only under the ADR 0085
  double opt-in; must be absent in `APP_ENV=production` (test proves
  absence).
- Players can never settle, void or correct a bet (principal-type
  assertion; closes ADR 0047 §5(b) for the new routes).
- W0 test-admin connection must never be available to application code
  paths (test helpers only), reviewed by `security`.

### P. Financial / ledger impact

- New postings are balanced pairs per ADR 0038 §5/§8.1/§10; corrections
  are compensating entries with `reverses_transaction_id`; no balance
  `UPDATE`; authoritative balance read inside the posting transaction.
- Payout amount is the provider-stated minor-unit integer (ADR 0038 §5);
  no platform odds math, no floating point.
- Idempotency by DB unique constraint; late/out-of-order events
  (void-after-settlement, rollback-before-settlement tombstone per
  `CLAUDE.md`) specified in ADR 0088 and tested.
- Risk: `ReversalTypes` populated in the same change as the CHECK
  widening (INV-SB-CUM-1).
- Reconciliation extended; drift must stay 0.
- `ledger-finance` financial sign-off is a completion gate.

### Q. Jurisdiction / compliance impact

- None added. Settlement does not re-evaluate jurisdiction: a placed
  bet's `jurisdiction_code` is immutable (INV-SB-JUR-6) and HDR-J-4
  (record authority at the time the obligation arose) governs.
- RG: settlement credits are not "play"; no new RG gate. Self-exclusion
  auto-void only if §V item 3 is authorized.
- No regulatory or certification claim is made.

### R. Testing strategy

- `qa`-owned test plan before code (W1), covering the full `CLAUDE.md`
  financial list and ADR 0083 §10 invariants.
- Integration tests with `-race` against real PostgreSQL 16 through the
  restored CI gate (W0 is a hard prerequisite).
- Adversarial RLS tests for new tables; runtime-role tests.
- Real HTTP tests for webhook + simulation route (`change-control.md`).
- Migration round-trip; `migrate verify`.
- Frontend unit tests for new status rendering.
- Staging verification only if a redeploy is separately authorized.

### S. Acceptance criteria

1. CI `build-test-lint` job runs to completion and is green on the stage
   head commit (link recorded).
2. A cash-funded single can be placed, then settled won (stake absorbed +
   full payout credited), settled lost, or voided (stake returned), each
   exactly once under duplicate/concurrent/replayed delivery.
3. A settled bet can be rolled back and re-settled via compensating
   entries; history is never edited.
4. `SUM(debits)=SUM(credits)`; projection-vs-ledger drift 0 after every
   test scenario; reconciliation covers sportsbook settlement.
5. Player cumulative risk capacity is released by void per the declared
   `ReversalTypes`.
6. Exposure excludes settled/void bets.
7. Players cannot trigger settlement; test-support route absent in
   production mode; webhook rejects bad signature/replay.
8. Audit events present for every lifecycle transition.
9. Independent reviews: `ledger-finance` (financial sign-off),
   `security`, `qa`, `code-reviewer`, `architect` — no open P0/P1.
10. `docs/progress.md`, `active-stage.md`, `task-registry.md`, ADRs
    updated; commit SHAs recorded; pushed to origin.

### T. Rollback strategy

- Code: revert commits; the stage adds new routes/types only (no change
  to placement path semantics beyond reading non-open bets).
- Database: down migrations provided and round-trip tested; a down
  migration **refuses** to run if any row uses the new transaction types
  (dirty-down rejection, repo convention) — ledger history is never
  deleted to roll back.
- Staging: no redeploy is part of this stage; if later authorized, the
  one-commit-per-environment rule (`deploy.sh`) means `down`/`up`.

### U. Deployment requirements

- None for completion. The stage is complete on green CI + reviews.
- A staging redeploy to demonstrate settlement is a **separate, optional
  human authorization** after the stage (full `down`/`up` per
  `deploy.sh`), from the allowlisted workstation.

### V. Human approvals required

1. **Approve Stage 10 as proposed** (W0 then W1), or select an
   alternative from §G.
2. **Approve W0's test-environment mechanism** (dedicated test-admin
   connection for scratch databases, CI only) — or specify another.
3. **Optional:** authorize seeding the already-decided platform default
   `OpenBetSelfExclusionPolicy = VOID_ON_SELF_EXCLUSION` for
   development/staging tenants so open bets are voided on
   self-exclusion (ADR 0042 flagged legal review; without this approval
   the policy remains unconfigured and fails closed — auto-void stays
   out of scope).
4. Confirm that no staging redeploy is expected within this stage.

## 9. Specialist review summary

_DRAFT — pending the review round._

## 10. Risks

_DRAFT — pending the review round._

## 11. Required approvals

See §V.

## 12. Exact next action after approval

_DRAFT — pending the review round._
