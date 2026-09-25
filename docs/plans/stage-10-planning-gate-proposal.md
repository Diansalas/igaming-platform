# Stage 10 Planning Gate — State Reconstruction and Next-Stage Proposal

**Status: PLANNING ONLY — NOT AUTHORIZED — NOT IMPLEMENTED — AWAITING
HUMAN APPROVAL.**
No migration, application code, Terraform, IAM or AWS change is made by
this document. It is the Master Orchestrator's decision-ready proposal
for the human approval gate required by `CLAUDE.md` "Stage-gate rule"
and `MASTER-BUILD-PROMPT.md` "Stages" (a new stage is proposed only as a
recorded decision). On approval, the stage definition is recorded as
ADR 0087 (see §K) before any implementation starts.

- Prepared: 2026-09-25, Master Orchestrator.
- Repository state reviewed: branch `claude/focused-wright-jw88w9`,
  code HEAD `9190d5d01da076141a1f70d7e5897a573d3b18f4` (the deployed
  staging commit). Draft committed as `8797fcf`; this consolidated
  version supersedes it (commit SHA recorded in `docs/progress.md`).
- Specialist review round: complete — 7 independent reviews,
  consolidated in §9.

> Naming note (architect finding): "ADR 0087/0088" are decision records
> in `docs/decisions/`; migrations `0087`/`0088` are unrelated existing
> files in `migrations/`. This document always writes "ADR" or
> "migration" explicitly.

---

## 1. Master Orchestrator state

The Master Orchestrator remains the single coordination authority
(`docs/governance/agent-registry.md` "Master Orchestrator";
`MASTER-BUILD-PROMPT.md` "Orchestration model"). This document was
produced from the repository, not from conversation memory:

1. Four independent read-only reconstruction passes (governance model;
   human-decision and deferred-work registers; stage history 4H-B1 →
   9.4; code reality check of `internal/`, `cmd/`, `migrations/`,
   routes and the last commits).
2. Orchestrator-run verification of the deployed commit (§2.3).
3. A seven-specialist independent review of the draft (§9), with two
   inter-specialist conflicts resolved by the Orchestrator on evidence
   (§9.2).

Where repository records disagree with each other or with the code,
that is recorded as a finding (§2.4), not silently resolved.

## 2. Current project state

### 2.1 Where the project is

| Item | State | Evidence |
|---|---|---|
| Last recorded stage | Stage 9.4 — Staging Infrastructure Hardening + Cost Optimization — "complete (repository-side; not deployed)" | `docs/progress.md` §"Stage 9.4 — Staging Infrastructure Hardening…"; `docs/active-stage.md` (prior "Current stage: Stage 9.4") |
| Staging deployment | **Executed by the human** from the allowlisted workstation. Agent-sandbox deployment attempts were blocked (sandbox egress IP not allowlisted; sandbox TLS interception prevented image builds) — see §2.2 | Human attestation, 2026-09-25 |
| Staging acceptance | **PASSED — human-attested**: real B2C browser acceptance (including the previously failing sportsbook selection flow) and manual Back Office verification | Human attestation, 2026-09-25. **Not independently re-verified by the Orchestrator** (F-6) |
| Deployed commit | `9190d5d01da076141a1f70d7e5897a573d3b18f4` | Human attestation; branch HEAD at review |
| Staging environment | Running (≈ $0.10/hour per ADR 0086 / `docs/runbooks/stage-9-4-staging-lifecycle-runbook.md`); disposition is a human decision (§V item 4) | Human attestation |
| Stage 10 | **Not defined, not authorized, not started** | Every Stage 9.x record: "Stage 10 is NOT authorized" |

### 2.2 Commits after the last recorded stage (previously unrecorded in `docs/`)

| Commit | Summary | Scope |
|---|---|---|
| `221b6ef` | fix: run frontend nginx pid as non-root — `frontend.Dockerfile` pid rewrite accepts `/run` and `/var/run`; build fails unless `pid /tmp/nginx.pid;`; new `deploy/docker/tests/frontend-image-smoke.sh`; new CI job `frontend-image` (green in CI since) | Docker/CI only |
| `b22d5c4` | fix(aws): ALB security group admits only tcp/80 from the CloudFront origin-facing managed prefix list; 443 rule and `alb_ingress_cidrs` (default `0.0.0.0/0`) removed; deployer network policy gains read-only `ec2:GetManagedPrefixListEntries` scoped to AWS-owned prefix lists; simulation suite grows to **30** cases | Terraform + IAM policy JSON + tests |
| `8ec3dc7` | feat(ui): B2C and Back Office workflows for the staging acceptance run (email verification card, deposit simulate-confirmation, withdrawal page, KYC/RG status card, casino idempotency key, bet-slip rejection handling; Back Office withdrawal queue/detail, tenant/brand/staff forms, configuration page, casino catalogue page) | `b2c/`, `backoffice/` only |
| `9190d5d` | fix(b2c): selections pickable when `market.status === 'open' && sel.status === 'active'`; fixtures corrected; regression tests | `b2c/` only |

### 2.3 Orchestrator verification at `9190d5d` (this session; local; synthetic data only)

| Check | Result |
|---|---|
| `go build ./...`, `go vet ./...` | PASS |
| `gofmt -l .` | PASS (clean) |
| `go test ./...` (unit) | PASS (27 packages) |
| Fresh PostgreSQL 16 DB set up exactly as `.github/workflows/ci.yml`: non-superuser `NOCREATEDB` owner `igaming`; non-owning `igaming_runtime` with default privileges granted before migration; `migrate up` + `migrate verify`; `REVOKE INSERT, UPDATE, DELETE ON schema_migrations FROM igaming_runtime`; `TEST_RUNTIME_DATABASE_URL` set so the runtime-role adversarial probe runs | PASS — 90 migrations applied and verified, no gaps |
| `go test -race -tags=integration ./...` (compiles and runs unit and integration tests together under the race detector) with the CI role setup exactly | **FAIL — 6 packages** (`bonus`, `db`, `jurisdiction`, `ledger`, `operatingmarket`, `sportsbook`). Every failure: `permission denied to create database` in scratch-database migration/RLS tests |
| Same 6 packages with `CREATEDB` granted to the local throwaway test role only | PASS — including `internal/db`'s runtime-role separation probe |
| Migration reversibility (`-steps=4 down`, `up`, `verify`) | PASS |
| `golangci-lint run ./...` (v2.5.0, repo `.golangci.yml`) | **FAIL — 19 issues** (13 errcheck, 6 staticcheck); none in ledger/wallet/money code |
| GitHub Actions CI, the 60 most recent runs on this branch (runs 175–234) | **All `failure`.** `build-test-lint` aborts at "Set up job": `Unable to resolve action golangci-lint/golangci-lint-action, repository not found` (`.github/workflows/ci.yml:99`, present since the file entered this history at `4790de0`). Confirmed in the logs of runs 175, 221 and 234. `frontend`, `frontend-image`, `infrastructure` pass |

### 2.4 Findings from the reconstruction (recorded here, not fixed here)

- **F-1 (P1, process) — CI's Go gate has never executed on this
  branch's history.** Every stage record's Go verification is
  **local Orchestrator evidence only**, never CI evidence. The recorded
  results were re-confirmed locally (§2.3), but `change-control.md`
  Rule 7's evidence standard assumes a working gate.
- **F-2 (P1, raised from P2 on `qa` review) — scratch-database
  integration tests need `CREATE DATABASE`**, which neither CI
  (`ci.yml:64`) nor `deploy/init-app-role.sql` grants. Fixing F-1 alone
  turns CI deterministically red on 6 packages, and because GitHub
  Actions stops at the failing step, the migration-reversibility step
  would still never run in CI until F-2 is fixed.
- **F-3 (P3) — `golangci-lint` is not clean (19 issues)**, contradicting
  recorded "0 issues". Same root cause as the drift: CI asks for
  `version: latest`. Fixed together with F-1 by pinning.
- **F-4 (P2, record integrity) — project memory is stale.**
  `docs/governance/project-status.md` stops at Stage 4I and still lists
  decisions ADR 0042 answered; `docs/progress.md` header said "Stage 8";
  `docs/active-stage.md` carried a stale 4H-B1 "Current stage" header;
  several `task-registry.md` rows still read "Dispatched"/"In progress";
  `ledger-accounting-model.md` §6.5.4/§6.5.7 still call HR-15 "NOT
  IMPLEMENTED" although migration 0082's `ledger_accounts_immutable_fields`
  trigger implements it; Stage 5–9.4 final reports were delivered in
  chat and not committed. (The two headers that misstate the current
  stage are corrected with this document; the rest is W0 item 6.)
- **F-5 (P2, governance) — `b22d5c4` (Terraform + IAM policy) has no
  recorded review.** Static reviews by `security` and `devops` in this
  round found **no material risk** (it tightens ingress and removes a
  fail-open `0.0.0.0/0` default); the live checks are still owed
  (W0 item 4).
- **F-6 (information) — staging acceptance is human-attested only.** No
  acceptance evidence artifact exists in the repository.
- **F-7 (P2, financial — found by `ledger-finance`, verified by the
  Orchestrator) — `ledger.Post` idempotent replay compares only the
  transaction type, not the entries.** On a key conflict it returns the
  original transaction as `AlreadyPosted`
  (`internal/ledger/ledger.go:316-326`), so a replay with the same key
  and a different amount is silently absorbed (first write wins) rather
  than rejected, although ADR 0038 §11 describes same-key /
  different-payload rejection. Whether any existing caller (casino,
  payments, withdrawal, bonus) is exposed depends on caller-side payload
  checks — **not yet audited; not asserted to be an exploitable defect
  here.** Triage is W1 item 0.

## 3. Governance verification

"Explicit" = established in a permanent governance file; "Stage/ADR" =
established in a stage directive, ADR or human-decision record (binding,
but not in the permanent rulebook).

| Principle | Status | Where |
|---|---|---|
| Orchestrator coordinates all work | Explicit | `docs/governance/agent-registry.md` "Master Orchestrator"; `MASTER-BUILD-PROMPT.md` "Orchestration model" |
| Specialists stay in domain; no silent overwrite | Explicit | `agent-registry.md` "Absolute constraint…"; `docs/governance/ownership.md` Rules 1–4 |
| Cross-domain dependencies via Orchestrator | Explicit | `docs/governance/change-control.md` "Cross-domain change"; `docs/governance/integration-protocol.md` "Dependency requests" |
| No invented human decisions; decisions gated | Explicit | `CLAUDE.md` "When to stop and ask"; ADRs 0039/0041/0044 ("Record only") |
| No production claims without evidence | Explicit (partial) | `CLAUDE.md` "No fake completion"; `change-control.md` Rule 7 |
| No certification claims | Explicit (by implication) | `CLAUDE.md` "Compliance" |
| Financial invariants; append-only double-entry; idempotency | Explicit | `CLAUDE.md` "Financial / ledger rules" |
| RLS mandatory; tenant isolation | Explicit | `CLAUDE.md` "Multi-tenancy"; `change-control.md` schema row |
| Auditability | Explicit | `CLAUDE.md` "Security" |
| Security review for sensitive changes | Explicit | `CLAUDE.md` "Security"; `change-control.md` Rule 3 |
| Fail-closed where required | Stage/ADR | `.claude/agents/risk.md`; ADR 0031; ADR 0085; `docs/governance/stage-4i-exit-register.md` |
| Provider-neutral abstractions; no fake real providers | Explicit | `CLAUDE.md` "Provider abstraction", "No fake completion" |
| Jurisdiction first-class | Explicit | `CLAUDE.md` "Compliance" |
| Jurisdiction operation-specific | Human decision | ADR 0042 HDR-J-2; ADR 0044 HDR-J-7 |
| Licensing / operating-market decisions governed | Explicit | `CLAUDE.md` "When to stop and ask"; ADR 0006; exit register (HDR-J-6, HDR-M-1/2) |
| Human decisions not reopened without justification | Stage/ADR | `MASTER-BUILD-PROMPT.md` preamble; Stage 9.2 operating principle (`task-registry.md`) |
| Infra not changed casually | Partial | `.claude/agents/devops.md`; `CLAUDE.md` "Environment safety"; no infra row in `change-control.md` |

**Conclusion:** every listed principle remains active; none has been
reversed. The review round itself exercised them: two conflicting
specialist positions were resolved by the Orchestrator on evidence
(§9.2), and one draft item that would have re-asked a decided human
question was withdrawn (§9.2 R-2). Three principles are not yet in the
permanent rulebook; W0 item 6 promotes them into `change-control.md`
**with a citation to each source ADR, as documentation of existing
rules — no new rule, no new gate.**

## 4. Completed stages

| Stage | Record | Commit(s) |
|---|---|---|
| 0–3 (discovery, foundation, identity/tenancy/security, wallet/ledger/payments) | `docs/progress.md` | see progress.md |
| 4A–4G, 4G-FINAL, 4G-FINAL-FINANCE-GATE | `docs/progress.md` | see progress.md |
| 4H-A, 4H-B0 … 4H-B0-R7 (bonus/gamification/retail/asset/FX architecture; `player_locked` split, migration 0048) | `docs/progress.md`; ADRs 0032–0040 | see progress.md |
| 4H-B1 Waves 1–3 (bonus engine; migrations 0050–0070) | `docs/progress.md`; `docs/governance/wave-*-report.md` | `d145ba1`…`cd6ee62` |
| 4I foundation, Phases A–E, E-SECURITY, Exit Triage (migrations 0071–0077) | `docs/progress.md`; `docs/governance/stage-4i-*.md`; `docs/plans/stage-4i-jurisdiction-implementation-plan.md` | `c8a57eb`, `4e911a9`, `5bb34fd`, `027d0c2`, `bbe404e`, `e554727`, `915bfa9`, `bcf2413` |
| 5 Back Office MVP | `docs/progress.md` | `e77b75b` |
| 6 / 6.1 B2C + first sportsbook slice (singles, placement only, mock provider; ADR 0047) | `docs/progress.md` | `be6042d`, `cb8a0b3` |
| 7 Casino B2C slice (play simulation; ADR 0048) | `docs/progress.md` | `651d5d1` |
| 8 Provider integration readiness (ADR 0080) | `docs/progress.md` | `3c61873` |
| 9 / 9.1 / 9.2 (ADRs 0081–0083) | `docs/progress.md` | `7464fde`, `d09e4b3`, `83ff95d` |
| 9.3 Staging package + local end-to-end acceptance (ADR 0084) | `docs/progress.md` | `1c49068` |
| 9.4 Part 1 APP_ENV fail-closed (ADR 0085) | `docs/progress.md` | `627a1dd` |
| 9.4 Staging hardening (ADR 0086) | `docs/progress.md` | `4e49179`, `fc60d58`, `09553fa`, `5c4ac51`, `af5bfef`, `fa3dd5c` |
| 9.4 Staging deployment + acceptance (human-executed) | this document §2; `docs/progress.md` | `221b6ef`, `b22d5c4`, `8ec3dc7`, `9190d5d` |

## 5. Remaining work (status by area)

Labels per `CLAUDE.md` "No fake completion". **H** = human decision,
**V** = vendor/contract, **E** = engineering dependency only.

| Area | Status | Blocked by |
|---|---|---|
| Sportsbook settlement (win/loss) | NOT IMPLEMENTED (ADR 0038 §5 RESOLVED for cash-funded) | E only |
| Sportsbook void (before and after settlement) | NOT IMPLEMENTED (ADR 0038 §8.1, needs amendment — §9) | E only |
| Sportsbook correction (rollback, re-settlement, rollback-then-void) | NOT IMPLEMENTED (ADR 0038 §10) | E: settlement |
| Sportsbook partial settlement | NOT APPLICABLE while singles-only (ADR 0038 §8.2 is multi-leg) | E: accumulators (not authorized) |
| Sportsbook cashout | NOT IMPLEMENTED; cash-funded needs provider-priced offer; bonus/mixed-funded decided "not cashout-eligible" (ADR 0042) | V; E: settlement |
| Sportsbook liability/exposure | Exposure gate IMPLEMENTED, **unarmed** (zero `sb_exposure_limits` rows); no liability reporting | H: HDR-SB-1 (production go-live) |
| Sportsbook jurisdiction rung 2 (`SB-JUR-RUNG2-1`) | SPECIFIED, NOT IMPLEMENTED | H: HDR-J-7 |
| Open-bet self-exclusion auto-void | Policy infra IMPLEMENTED; default `VOID_ON_SELF_EXCLUSION` **decided** (ADR 0042), not seeded; no consumer in `internal/sportsbook`; engineering gaps named in ADR 0042 (effective-date handling; evidence every open bet was checked) | E: void + those gaps; legal review before production use (ADR 0042) |
| Jurisdiction / operating-market completion | MECHANISM ONLY | H: HDR-J-6/7/8/9, HDR-M-1/2, HDR-J-3e/3f |
| Real sportsbook / casino / KYC / payment providers | MOCK only | V |
| Withdrawal lifecycle | Request + four-eyes IMPLEMENTED; no real payout/callback/reconciliation | V: PSP |
| B2B tenant/partner capabilities | PARTIAL (admin APIs + Back Office forms); no partner console, no BYOL onboarding | E + product sequencing (doc 35, Stage 6B) |
| Retail/POS | NOT IMPLEMENTED | H: doc 27 §24 items 1–14 |
| Production hardening | Launch gates open: audit client IP (ADR 0086), Back Office refresh-token storage (Stage 5), KMS/HSM signing (ADR 0018), staff MFA status unverified (ADR 0017), `PLAT-ROLESPLIT-1` production step, `IssueCredentialToken` race (ADR 0085), CloudFront→ALB plaintext HTTP inside AWS (ADR 0086; security finding this round) | E (most); H for production credentials |
| Compliance / certification readiness | None claimed or performed | H; V (test lab) |
| Observability | Code-level logging/tracing; no metrics backend; staging alarms only | E; H (production provider) |
| DR / backup | "NOT MET" (`docs/runbooks/backup-and-disaster-recovery.md`) | H: ADR 0009 AUP / production provider |
| Scaling / load testing | Go concurrency tests only | E |
| CI / evidence pipeline | **Broken (F-1, F-2, F-3)** | E only |
| Ledger replay payload check | **Unaudited (F-7)** | E only |
| Record integrity | Stale (F-4); unreviewed infra commit (F-5) | E only |

## 6. Dependency graph

```
                  ┌───────────────────────────────────────────────────┐
                  │ W0  CI gate restoration + record integrity        │  no human decision
                  │     F-1 action, F-2 test-admin role, F-3 lint,    │
                  │     F-5 live re-validation, F-4 records           │
                  └───────────────────────┬───────────────────────────┘
                                          │ hard gate: ≥5 consecutive green CI runs
     ┌────────────────────────────────────┼──────────────────────────────────────────┐
     ▼                                    ▼                                          ▼
┌──────────────────────────┐   ┌───────────────────────────────┐        ┌────────────────────────────┐
│ W1 SB settlement / void /│   │ Production hardening,         │        │ Jurisdiction completion    │
│ correction (cash, singles│   │ engineering-only gates        │        │ BLOCKED: HDR-J-6/7/8/9,    │
│ in-house mock mode)      │   │ (audit client IP, BO refresh  │        │ HDR-M-1/2, J-3e/3f legal   │
│ W1.0 F-7 ledger triage   │   │ cookie, token race, KMS, MFA) │        └────────────┬───────────────┘
└───┬──────────┬───────────┘   │ proposed as the stage AFTER   │                     ▼
    │          │               │ this one (sequencing, not     │        SB rung 2, operating-market wiring
    │          ▼               │ deprioritization)             │
    │   self-exclusion auto-   └───────────────┬───────────────┘
    │   void consumer (later)                  │
    ▼                                          ▼
 SB cashout ◄── V provider pricing        Production launch ◄── H: ADR 0009 AUP; licence/markets (HDR-J-6);
 Liability reporting ◄── H HDR-SB-1                             HDR-SB-1; vendor contracts; DR; PLAT-ROLESPLIT-1 step;
 Real SB provider (webhook, provider-mode                       legal RG/KYC/AML; data retention; OB-1
   idempotency) ◄── V
 Real casino / KYC / PSP ◄── V ──► withdrawal payout + callbacks + reconciliation
 B2B partner console / BYOL ◄── E (after B2C loop closes) + H (BYOL specifics)
 Retail/POS ◄── H (doc 27 §24)
```

**Reading of the graph.** Three bodies of work need no new human
decision: (1) the CI/evidence pipeline, which every later stage depends
on; (2) the sportsbook lifecycle after placement, the only unimplemented
core B2C money flow whose architecture is already RESOLVED; and (3) the
engineering-only production-hardening gates. (1) must come first. Of (2)
and (3), (2) closes a live financial loop — a placed bet locks its stake
in `player_locked_cash` with no path to release — while (3)'s value is
realized only when production is authorized, which is itself
human-blocked. So (2) is proposed now and (3) next; that ordering is a
sequencing choice, **not** a deprioritization of hardening.

## 7. Open human decisions (not decided here)

| ID | Decision | Blocks | Source |
|---|---|---|---|
| ADR 0009 residual | Written gambling-AUP / contractual permission / data residency | Any production workload; DR | `docs/decisions/0009-*`; ADR 0084/0086 |
| Licence / markets beyond Anjouan; **HDR-J-6** first-brand permitted markets ("not yet determined") | Launch; resolver wiring; `MKT-PM-1` | ADR 0042; `project-status.md` |
| **HDR-J-7** OperationClass → Purpose | Resolver wiring; `SB-JUR-RUNG2-1` | ADR 0044 |
| **HDR-J-8 / J-9** location signal required/advisory; staleness | Market-access evaluation policies | ADR 0044 |
| **HDR-J-3e / 3f** lawful basis; retention (legal counsel) | Production use of location/residence evidence | ADR 0042 |
| **HDR-M-1 / M-2** operating-market enablement governance; players after disablement | Any operating-market write route | `docs/governance/stage-4i-exit-register.md` |
| **HDR-SB-1** trading-book liability owner; exposure ceiling before go-live | Sportsbook production go-live | ADR 0083 §8.2 |
| **OB-1** negative `player_cash` after a won settlement is rolled back (receivable treatment) | Production operation of settlement correction; **does not block Stage 10 implementation** — the correct posting is mandated (`ledger-accounting-model.md` ~l.2640–2658) | `docs/architecture/ledger-accounting-model.md` OB-1 |
| FD-2 settlement-finality window | Optional bonus-conversion narrowing | `ledger-accounting-model.md` |
| Vendors: KYC, PSP, casino aggregator, sportsbook feed, crypto custodian | Real-money launch | `project-status.md`; ADR 0005 Q4 |
| `project-status.md` open decisions 1–7 (ADR 0028 KYC reuse; ADR 0027 BYOL person-resolution opt-out; RiskDecision REVIEW; ADR 0031 §5 precedence; ADR 0031 §8 platform-scoped risk-rule write path; …) | Real-money launch | `docs/governance/project-status.md` "Open decisions" |
| Retail doc 27 §24 items 1–14 | Retail/POS | `docs/architecture/27-*.md` §24 |
| Legal RG/KYC/AML interpretation; data-retention periods; legal review of `VOID_ON_SELF_EXCLUSION` before production use | Real-money go-live | `project-status.md`; Stage 9 §19; ADR 0042 |

**Resolved and binding (not reopened):** HDR-J-1/2/4/5; G-2
(configurable, default (b)); `OpenBetSelfExclusionPolicy` default
`VOID_ON_SELF_EXCLUSION`; bonus/mixed-funded cashout "not
cashout-eligible"; FD-1 "nullifying"; converted-Grant cancellation "no
clawback" (all ADR 0042); APP_ENV semantics (ADR 0085).

## 8. Proposed next stage

### F. Proposal

**Stage 10 — CI Evidence Restoration + Sportsbook Settlement Lifecycle
(cash-funded singles, in-house mock mode: settle, void, correction).**

- **Workstream 0 (W0) — Evidence pipeline and record integrity.** A
  workstream, not a separate stage (`qa`), but a **mechanical hard
  gate**: no W1 code merges until W0's acceptance (§S W0) is met.
- **Workstream 1 (W1) — Sportsbook settlement lifecycle.**

### G. Why this stage is next

1. Sportsbook settlement is the only unimplemented core B2C money flow
   that needs **no new human decision** for the cash-funded case (ADR
   0038 §5, §8.1, §10, §11, §12; ADR 0083 §6.1.2, INV-SB-CUM-1; ADR
   0082), and `MASTER-BUILD-PROMPT.md` Stage 5 names it explicitly
   ("settlement/void/partial/cashout events").
2. The alternatives are gated: jurisdiction completion (HDR-J-6/7/8/9,
   M-1/2), real providers (contracts), the heaviest production gates
   (ADR 0009), retail (doc 27 §24), liability reporting (HDR-SB-1).
3. It closes an open financial loop: stakes locked in
   `player_locked_cash` can never be released today, which also leaves
   reconciliation and the risk engine's cumulative accounting knowingly
   incomplete (ADR 0083 §6.1.2).
4. W0 comes first because every rule in `change-control.md` assumes a
   working CI gate, and F-1 shows it has never run. A financial stage
   must not be the next one to rely on local-only evidence.

Rejected alternatives (recorded, not built): engineering-only
production hardening first (proposed as the next stage; see §6);
B2B partner console first (commercial objective 1, the own B2C brand,
is not yet a closed loop); cashout (a platform-side price would invent
trading, which the platform does not own — `CLAUDE.md` "What this
project is"); a signed sportsbook settlement webhook (see §9.2 R-1).

### H. Exact scope

**W0 — Evidence pipeline and record integrity**

1. **CI action.** Replace the non-existent
   `golangci-lint/golangci-lint-action` with the upstream
   `golangci/golangci-lint-action`, at the **major version whose
   documentation states support for golangci-lint v2 configuration**
   (`.golangci.yml` is `version: "2"`), confirmed from upstream
   documentation during W0 (the `devops` review asserted "v6"; the
   Orchestrator's understanding is that v2 support begins in a later
   major; this is to be verified, not assumed). Pin the linter version
   (e.g. `v2.5.0`, the version verified locally) instead of `latest`,
   and record why in the commit and `docs/testing/testing-strategy.md`.
2. **Test-admin role for scratch databases** (security-required
   mechanism; `igaming` is **not** granted `CREATEDB`):
   - (a) new role `igaming_test_admin`: `LOGIN NOSUPERUSER CREATEDB
     NOCREATEROLE NOBYPASSRLS`, `GRANT igaming TO igaming_test_admin`;
     created **in CI and a dev-only init file only**, never in
     `deploy/init-app-role.sql`'s deployment path; never the
     `postgres` superuser URL.
   - (b) scratch helpers run `CREATE DATABASE <name> OWNER igaming`
     through `TEST_ADMIN_DATABASE_URL`, used for CREATE/DROP only; the
     scratch pool still connects as `TEST_DATABASE_URL`'s role through
     `db.Connect`, so `verifyNotPrivileged` (`internal/db/db.go:55-80`)
     still applies.
   - (c) the five copied helpers are consolidated into one
     integration-tagged test utility; `TEST_ADMIN_DATABASE_URL` is never
     read by `internal/config` or any non-`_test.go` file — enforced by
     a CI grep guard.
   - (d) a guard test asserts the scratch pool's `current_user` is not
     superuser, not `BYPASSRLS`, and owns the scratch database.
   - (e) unset → skip (as today); never fall back to `TEST_DATABASE_URL`.
   - Documented in `docs/testing/testing-strategy.md` (variable,
     CI/local-dev only, lifecycle, `qa` + `security` co-ownership).
3. **Lint.** Fix the 19 findings (no behavior change).
4. **`b22d5c4` live re-validation.** Access Analyzer `ValidatePolicy` on
   `deploy/aws/iam/staging-deployer-network-policy.json`; the full
   30-case `deploy/aws/tests/simulate-deployer-policies.py`; offline
   `terraform test` — outputs recorded in `task-registry.md`. Requires a
   credential allowed `iam:SimulateCustomPolicy` /
   `access-analyzer:ValidatePolicy` (the deployer deliberately lacks
   them); **the human names or supplies it** (§V item 3). If none is
   provided, this item is recorded BLOCKED, not marked done. Also fix
   the stale security-group description (P3, text only).
5. **Ordering guard.** State in `testing-strategy.md` that the
   migration-reversibility step only gains CI coverage once W0 items 1–2
   are fixed (it runs after the integration step).
6. **Record integrity.** Refresh `project-status.md`; correct stale
   `task-registry.md` rows; correct `ledger-accounting-model.md`
   §6.5.4/§6.5.7 HR-15 status (implemented by migration 0082,
   `ledger_accounts_immutable_fields`); promote the fail-closed,
   no-reopen and infrastructure-change-control rules into
   `change-control.md` citing their source ADRs (documentation only).
7. **Acceptance evidence.** Add the human-supplied staging acceptance
   checklist (test IDs, no credentials) to
   `docs/runbooks/stage-9-4-staging-lifecycle-runbook.md`.

**W1 — Sportsbook settlement lifecycle (cash-funded singles, in-house mock mode)**

0. **F-7 triage (`ledger-finance`, first).** Audit every existing
   `ledger.Post` caller for same-key / different-payload replay
   exposure; record the result. Any real defect found in an existing
   domain is reported to the Orchestrator as its own item with its own
   review — not silently folded into W1. W1's own postings compare
   payloads regardless (item 4).
1. **ADR 0088 first** (`sportsbook` + `ledger-finance`, reviewed by
   `architect`, `security`, `risk`, `qa`) — no settlement code before it
   is accepted. Contents per §K.
2. **Ledger** (`ledger-finance`): admit `sportsbook_settlement`,
   `sportsbook_void`, `sportsbook_rollback` (migration widening the
   `transaction_type` CHECK). Postings on post-0048 accounts:
   - Loss: Dr `player_locked_cash` S · Cr `house_gaming` S.
   - Win: that pair **plus** Dr `house_gaming` S+W · Cr `player_cash` S+W.
   - Void before settlement: Dr `player_locked_cash` S · Cr `player_cash` S.
   - **Void after settlement = `sportsbook_rollback` (exact inverse of
     the settlement, `reverses_transaction_id` = settlement) + a
     before-settlement-shape `sportsbook_void`, in one DB transaction**
     (single `reverses_transaction_id` column; ADR 0038 §8.1 and Flow 10
     amended accordingly).
   - Stake released is read from the bet's own ledger postings
     (`correlation_id`), never from the request.
   - **Same change:** `operationCumulativeSpecs[OperationSportsbookBet]
     .ReversalTypes = ["sportsbook_void"]` exactly — `sportsbook_rollback`
     is never netted (ADR 0038 §13; INV-SB-CUM-1). `MeasuredAccountTypes`
     /`IgnoredAccountTypes` confirmed by `risk` for the new types.
3. **State machine** (`sportsbook`; DB-enforced):
   - Append-only `sportsbook_bet_settlements` history table (outcome,
     payout, asset, generation/`occurrence_ordinal`, `causation_id`,
     ledger transaction id; deny UPDATE/DELETE/TRUNCATE triggers; FORCE
     RLS; runtime role INSERT/SELECT only).
   - DB uniqueness on (bet, settlement generation) and on the reversed
     settlement id (at most one rollback per settlement).
   - `sportsbook_bets.status` becomes a derived cache guarded by a
     transition trigger: `open → settled_won | settled_lost | void`;
     `settled_* → open` only via rollback (bet returns to `open`, so
     exposure counts it again); rollback-then-void → `void`; **void is
     terminal**.
   - A second settlement on a settled bet is rejected and alerted
     unless preceded by a rollback; settlement after void is rejected
     and alerted.
   - `sportsbook_bets_enforce_immutable_fields` still protects stake,
     odds and references (regression test).
   - Settlement never reads or locks `sb_selections`; ADR 0047 §5(c)'s
     deferment is unaffected.
4. **Idempotency and payload integrity** (`ledger-finance`):
   - In-house mode for the whole lifecycle (ADR 0038 §14.6):
     `provider_id` NULL; deterministic, namespaced, server-derived
     `idempotency_key`s; the ledger's existing `(tenant_id,
     idempotency_key)` unique constraint is the **single** idempotency
     authority (no parallel key table).
   - On a key hit, the stored settlement record's outcome, payout and
     asset are compared with the request; any difference is rejected
     with a typed error, an integrity alert and an audit record, and
     nothing is posted.
   - Rollback for a never-seen settlement writes a tombstone (casino
     `postRollbackTombstone` pattern); the late original fails with a
     typed "tombstoned" error; the bet stays open and a later
     re-settlement under a new generation succeeds.
   - Corrections compose `occurrence_ordinal` (ADR 0038 §14.1/§14.5).
   - Payout validation: positive integer; asset equals the bet's asset;
     **for a won single, payout == the bet's frozen `potential_return`**
     (anti-minting, a stored-value comparison, not platform odds math);
     explicit int64 overflow rejection. A mismatch is rejected and
     alerted.
5. **Lock ordering** (`ledger-finance` + `architect`): L1 `FOR UPDATE`
   on the bet row (ascending id for multiple bets; INV-LOCK-E3) →
   idempotency and state check under that lock → L2 `FOR UPDATE` on the
   settlement being reversed → L3 `LockProjectionsForPosting` over
   {`player_cash`, `player_locked_cash`, `house_gaming`} → L4 Post. ADR
   0088 resolves (a) the history-row insert after Post (reserve before
   Post, or a named exception E-4 with a safety argument) and (b)
   whether settlement takes L0.6 (`ledger-finance` recommends not —
   settlement only releases exposure); ADR 0082 amended to match.
6. **Settlement driver** (`sportsbook`; `security`-reviewed): a
   **test-support-gated staff route** only — no provider webhook this
   stage (§9.2 R-1). Registered only when `TestSupportRoutesEnabled()`
   (ADR 0085 double opt-in); staff principal with `RequireTenantScope`
   and a new dedicated permission; player and platform-admin tokens
   rejected; payout bounded per item 4; audit entry with actor,
   tenant, bet, before/after status. This is test tooling, **not** the
   out-of-scope operator manual settlement, and never exists under
   `APP_ENV=production`. ADR 0048's trust-boundary mitigations apply
   (tenant/ownership re-check, bounded payout, deterministic
   idempotency).
7. **Reconciliation** (`ledger-finance`): a new zero-tolerance
   sportsbook stream in `internal/reconciliation` —
   (a) per wallet/asset: `player_locked_cash` balance == Σ stake of
   open bets, scoped by sportsbook transaction types/bet ids (casino
   also writes `player_locked_cash`), and `player_locked_bonus` == 0;
   (b) per bet (`correlation_id`): non-open ⇒ net locked 0; void or
   rollback-then-void ⇒ net 0 on `player_cash` and `house_gaming`; won
   ⇒ un-reversed payout == recorded payout; (c) two-way orphan check
   between ledger and `sportsbook_bet_settlements`; (d) a mock
   provider-statement match labelled **MOCK** (real match PROVIDER
   DEPENDENT). A negative test proves injected drift is detected.
8. **Audit**: `sportsbook_bet.settled`, `sportsbook_bet.voided`,
   `sportsbook_bet.rolled_back` (ADR 0038 names). New audit call sites
   use the `TRUSTED_PROXY_COUNT`-aware client-IP extraction, or ADR
   0088 records explicitly that they inherit the open ADR 0086 client-IP
   defect (to be decided in ADR 0088 with `security`).
9. **Read surfaces** (`frontend`, `backoffice`): B2C bet history shows
   won/lost/void and payout; Back Office bet list/detail shows
   settlement status, payout, ledger transaction id and settlement
   reference/`correlation_id` for traceability. No settlement controls
   in either UI.
10. **Records**: ADR 0038 amendments via ADR 0088 (post-0048 account
    names in §1/§5/§8.1/§13; §13 `ReversalTypes` naming; §15 marked
    implemented by migration 0048; §11 payload-rejection wording vs
    F-7; §8.1/Flow 10 void-after-settlement shape); ADR 0019 actor
    matrix rows; ADR 0083 §6.1.2 status note; ADR 0082 amendment.

### I. Explicitly out of scope

- A signed sportsbook settlement **webhook** and any provider-mode
  (external-reference) idempotency — deferred to the real-provider
  stage (`security`'s webhook requirements are recorded in §9.1 for
  that stage).
- Cashout (any funding); bonus-funded sportsbook staking/settlement
  (ADR 0038 §9; HR-9); partial settlement, accumulators, bet-builder,
  live odds.
- Operator manual settlement from the Back Office (a new privileged
  financial control needing its own four-eyes/threshold design).
- **Self-exclusion auto-void consumer** — out of scope for engineering
  reasons (no consumer in `internal/sportsbook`; the effective-date and
  "every open bet checked" gaps ADR 0042 names). The decided default is
  **not** re-asked (§9.2 R-2). The void mechanism built here is the one
  ADR 0034 §14.7 will reuse; ADR 0088 records that dependency.
- Liability reporting / arming exposure limits (HDR-SB-1); jurisdiction
  rung 2 (HDR-J-7); operating-market wiring; jurisdiction content.
- Real sportsbook/casino/KYC/PSP integrations; crypto custody; B2B
  partner console; retail/POS; reporting/ClickHouse.
- Widening money columns to `NUMERIC(38,0)` (recorded as known debt:
  `stake_amount`/`potential_return` are `BIGINT` and ledger amounts are
  int64; W1 adds explicit overflow rejection, and new money columns
  follow the existing width to stay consistent — ADR 0088 records the
  debt).
- Terraform/IAM/ECS/RDS/CloudFront/network changes (none required); no
  staging redeploy within the stage.
- Production anything.

### J. Required specialist agents

| Agent | Role |
|---|---|
| `devops` | W0 items 1, 2 (CI wiring), 4, 5 |
| `qa` | W0 item 2 (co-owner), W1 test plan and gate, mutation pass |
| `security` | W0 items 2 and 4; W1 route principal model, RLS, audit, ADR 0088 review |
| `ledger-finance` | W1 items 0, 2, 4, 5, 7; financial sign-off |
| `sportsbook` | W1 items 1, 3, 6; ADR 0088 co-author |
| `risk` | W1 item 2 (`ReversalTypes`, measured/ignored accounts) |
| `architect` | ADR 0087; ADR 0088 and ADR 0082 amendment review; cross-domain consistency |
| `frontend`, `backoffice` | W1 item 9 |
| `code-reviewer` | Independent review of every W0/W1 change |
| `product-owner-proxy` | Scope guard (no webhook, no manual settlement, no cashout) |

`integrations` is not required (no provider adapter work this stage).

### K. Required ADRs

- **ADR 0087 (new)** — Stage 10 definition and scope (this proposal as
  approved).
- **ADR 0088 (new)** — Sportsbook settlement implementation contract:
  in-house idempotency mode for the full lifecycle; the transition
  graph including rollback and rollback-then-void; append-only history
  table; payout validation; tombstones and `occurrence_ordinal`;
  settlement-for-unknown-bet integrity alert; lock order and the E-4 /
  L0.6 decisions; test-support route trust boundary (ADR 0048 pattern);
  audit client-IP treatment; the ADR 0034 §14.7 dependency note; the
  money-width debt; and the ADR 0038 amendments listed in W1 item 10.
- **ADR 0082 amendment** (lock order for settlement/void/rollback).
- **ADR 0019 actor-matrix rows** for each `sportsbook_*` type.
- **ADR 0083 §6.1.2 status note** (`ReversalTypes` populated).
- No ADR for W0 tooling; W0 item 2 is recorded in
  `docs/testing/testing-strategy.md` and, if role documentation is
  affected, `docs/security/runtime-role-separation.md`.

### L. Required database changes (W1 only; W0 changes no schema)

- Migration widening `ledger_transactions.transaction_type` CHECK with
  `sportsbook_settlement`, `sportsbook_void`, `sportsbook_rollback`.
- New append-only `sportsbook_bet_settlements` (tenant-owned: `tenant_id`,
  FORCE RLS, deny UPDATE/DELETE/TRUNCATE triggers, uniqueness on
  (bet, generation) and on reversed settlement id; runtime role
  INSERT/SELECT only; probe added to `runtime_role_separation_test.go`).
- Transition-guard trigger on `sportsbook_bets.status`.
- Down migrations refuse if any row uses the new transaction types
  **or** the settlement table is non-empty (financial evidence is never
  dropped); after the first posting, rollback is forward-fix only.
- No change to `player_locked_*` account types (migration 0048).

### M. Required API changes (W1)

- A test-support-gated staff settlement route (exact path in ADR 0088),
  registered only when `TestSupportRoutesEnabled()`; staff principal +
  dedicated permission; players and platform-admin rejected.
- Read-only additions: settlement status, outcome, payout and
  references on `GET /v1/me/sportsbook/bets` and
  `GET /v1/admin/sportsbook/bets`.
- OpenAPI (`docs/api/openapi/platform-api.yaml`) updated for all of the
  above (`change-control.md`).

### N. Required frontend changes (W1)

- B2C bet history: won/lost/void and payout.
- Back Office sportsbook bets list/detail: settlement status, payout,
  ledger transaction id, settlement reference / `correlation_id`. No
  settlement controls.

### O. Security / RLS impact

- W0: the test-admin role exists only in CI and dev init; never in the
  deployment path; never read by application code (CI grep guard);
  scratch pools still run as the non-privileged owner (guard test).
- W1: new table FORCE RLS with adversarial cross-tenant tests;
  runtime-role least privilege and probe; DB-enforced transitions.
- Settlement route: absent in production mode and when the flag is off
  (real HTTP tests); 403 for player tokens; tenant-scoped staff
  principal; tenant B can never settle or read tenant A's bet (test).
- Players can never settle, void or correct a bet.
- Audit client-IP treatment decided in ADR 0088 (W1 item 8).
- CloudFront→ALB plaintext HTTP inside AWS recorded as a production
  gate (not introduced by this stage).
- No secrets in the repository; test credentials only as CI
  placeholders of the existing kind.

### P. Financial / ledger impact

- Balanced postings per W1 item 2; corrections are compensating
  entries with `reverses_transaction_id`; no balance `UPDATE`; the
  authoritative balance read happens inside the posting transaction.
- Payout is validated against stored inputs (== `potential_return` for a
  won single); no platform odds math; no floating point.
- One idempotency authority (ledger constraint) plus payload comparison
  on replay; tombstones for rollback-before-settlement.
- `ReversalTypes = ["sportsbook_void"]` in the same change
  (INV-SB-CUM-1): void releases cumulative capacity; rollback does not;
  rollback-then-void releases exactly once.
- Rolling back a won settlement may drive `player_cash` negative; that
  posting is correct and must post; its business treatment is OB-1
  (open, §7).
- Reconciliation sportsbook stream with zero tolerance.
- `ledger-finance` financial sign-off is a completion gate.

### Q. Jurisdiction / compliance impact

- None added. A placed bet's `jurisdiction_code` is immutable
  (INV-SB-JUR-6); HDR-J-4 (record authority at the time the obligation
  arose) governs settlement.
- No new RG gate; settlement credits are not play.
- No regulatory or certification claim is made.

### R. Testing strategy

`qa` owns a test plan written before W1 code. Required, each as a
named test:

- **CLAUDE.md financial list**: normal, duplicates, concurrency,
  retries, partial failure (fault injection between Post and the
  history/audit insert), rollback, settlement, reconciliation, provider
  callbacks (the staff simulation route plays this role; no webhook),
  idempotency, authorization, auditability.
- **Lifecycle**: settle won; settle lost; void before settlement; void
  after settlement (won and lost variants: end balances and risk
  netting); settlement after void rejected; second settlement without
  rollback rejected; rollback then re-settlement; rollback-then-void;
  repeated corrections with ordinals; tombstone → late original
  rejected → re-settlement succeeds; exposure counted again after a
  rollback; exposure released after settle/void (a previously blocked
  bet is admitted).
- **Idempotency/integrity**: same key with a different payout, outcome
  or asset rejected and alerted; two distinct settlement attempts on
  one bet concurrently — exactly one wins; payout validation (asset,
  ≤ 0, overflow, ≠ `potential_return`).
- **ADR 0083 §10 invariants**: INV-SB-CUM-1 (exact `ReversalTypes`;
  void nets S, rollback nets nothing, rollback-then-void nets once);
  INV-SB-EXP-1/EXP-2 (settlement in one tenant never affects another
  tenant's exposure or ledger — a business-logic test independent of
  RLS); INV-LOCK-E3 and ADR 0082 order (deterministic blocker-based
  deadlock tests: settle vs PlaceBet on one wallet; settle vs void on
  one bet; two settlements on different bets of one wallet; settlement
  vs casino bet sharing `house_gaming`).
- **Schema/security**: RLS adversarial tests; runtime-role probe;
  transition trigger; immutable-fields regression; down-migration
  refusal; HR-15 trigger present; `player_locked_cash` never negative;
  route absent in production and with the flag off; 403 for players;
  cross-tenant settlement impossible.
- **Reconciliation**: drift 0 for the new types; injected drift detected.
- **Mutation pass** (`qa`) over the new ledger/sportsbook settlement
  code, mirroring Stage 9.4 practice; surviving mutants are fixed or
  justified in the record.
- Frontend unit tests for new status rendering.

### S. Acceptance criteria

**W0 (gate before any W1 merge):**
1. `build-test-lint` runs to completion and is green on **at least 5
   consecutive CI runs** on the W0 completion commit (run links
   recorded).
2. Flake policy: any failure in those runs is root-caused and fixed;
   rerun-until-green is not acceptance.
3. Scratch-database tests run in CI through `igaming_test_admin`; guard
   test and grep guard pass; `igaming` unchanged (`NOCREATEDB`).
4. Lint clean at the pinned version.
5. `b22d5c4` live checks recorded — or item explicitly BLOCKED on the
   verification credential (§V item 3).
6. Records refreshed (W0 item 6).

**W1:**
7. A cash-funded single can be settled won, settled lost, voided
   (before or after settlement), rolled back, re-settled and
   rolled-back-then-voided, each exactly once under duplicate,
   concurrent and replayed requests, with the postings in W1 item 2.
8. `SUM(debits)=SUM(credits)`; projection-vs-ledger drift 0 and the
   sportsbook reconciliation stream at 0 after every scenario.
9. Cumulative capacity is released by void and **not** by rollback;
   rollback-then-void releases exactly once.
10. Exposure excludes settled/void bets and re-includes rolled-back bets.
11. Players cannot trigger settlement; the route is absent in
    production mode and when the flag is off; cross-tenant settlement
    is impossible.
12. Audit events present for every transition.
13. All tests in §R present and passing in CI; mutation pass recorded.
14. Independent reviews: `ledger-finance` (financial sign-off),
    `security`, `qa`, `risk`, `architect`, `code-reviewer` — no open
    P0/P1.
15. `docs/progress.md`, `docs/active-stage.md`,
    `docs/governance/task-registry.md`, ADRs updated; commit SHAs
    recorded; pushed to origin; working tree clean.

### T. Rollback strategy

- W0: revert commits (CI/test/tooling only).
- W1 before the first posting in any environment: revert commits and
  run the down migration.
- W1 after any posting: **forward-fix only**. Down migrations refuse
  when the new transaction types or settlement rows exist; ledger and
  settlement history are never deleted to roll back.
- A code revert with the migration kept must still render non-open
  bets (tested).
- Staging is not touched by this stage; any later redeploy follows the
  one-commit-per-environment rule (`deploy/aws/scripts/deploy.sh`:
  `down`, then `up`).

### U. Deployment requirements

- None for completion; the stage completes on green CI and reviews.
- A staging redeploy to demonstrate settlement is a separate, optional
  human authorization after the stage (`deploy.sh down`/`up` from the
  allowlisted workstation).

### V. Human approvals required

1. **Approve Stage 10 as proposed** (W0 as a hard gate, then W1), or
   choose an alternative from §G.
2. **Approve the W0 test-admin mechanism** (`igaming_test_admin` in CI
   and dev init only, `TEST_ADMIN_DATABASE_URL` for scratch-database
   create/drop only), or specify another.
3. **Name or supply the verification credential** for W0 item 4
   (`iam:SimulateCustomPolicy` + `access-analyzer:ValidatePolicy`, e.g.
   the existing read-only verification user), or accept W0 item 4 being
   recorded BLOCKED.
4. **Staging disposition during Stage 10** (neutral; both reversible and
   low-cost): keep the accepted environment running (≈ $0.10/hour, live
   but allowlisted attack surface) or tear it down now via
   `deploy.sh down` and redeploy only if a later demonstration is
   authorized.
5. Acknowledge **OB-1** remains open: the settlement-correction posting
   that can make `player_cash` negative will be built (it is the
   mandated correct posting), while its business/receivable treatment
   stays an open decision before production use.

## 9. Specialist review summary

### 9.1 Verdicts and dispositions

| Specialist | Verdict | Key findings (severity) | Disposition |
|---|---|---|---|
| `architect` | CHANGES REQUIRED | P1 idempotency-mode contradiction (in-house bets cannot bind to a provider-keyed webhook; ADR 0038 §14.6, ADR 0080); P1 incomplete, non-DB-enforced state machine (rollback edge, rollback-then-void); P1 test-support trigger trust boundary (ADR 0048); P2 lock order / ADR 0082 amendment; P2 exact `ReversalTypes`; P2 reconciliation misdescribed; P2 missing ADR work (ADR 0019 rows, ordinals, unknown-bet alert, money width); P2 self-exclusion reframing of ADR 0042; P3 prose/graph mismatch; P3 audit event name; P3 W0 order justified | All adopted (W1 items 2–10, §I, §K, §6, §9.2) |
| `ledger-finance` | APPROVED WITH CHANGES | P1 bet binding and idempotency mode; P1 void-after-settlement = rollback + void (ADR 0038 §8.1/Flow 10 amendment); P1 same-key different-payout silently absorbed (F-7); P1 DB-enforced single outcome + append-only history; P1 tombstone semantics + ordinals; P1 payout validation and OB-1; P2 lock order (E-4, L0.6); P2 reconciliation scope; P2 stale records (HR-15 implemented by migration 0082); P2 rollback strategy additions; P2 test gaps; P3 money width | All adopted (W1 items 0, 2, 4, 5, 7, 10; §L, §P, §R, §S, §T, §V item 5); HR-15 confirmed satisfied (no new work) |
| `sportsbook` | APPROVED WITH CHANGES | P2 "immutable history" requires the append-only table; supports webhook driver; confirms exposure excludes non-open bets by construction; `MeasuredAccountTypes` check; ADR 0047 §5(c) unaffected; BO traceability fields; P1 test cases (same-bet settle/void race; late/out-of-order) | Adopted except the webhook driver (overruled — §9.2 R-1) |
| `security` | APPROVED WITH CHANGES | P1 exact test-admin mechanism (`igaming_test_admin`, `CREATE DATABASE … OWNER igaming`, guards); P1 webhook trust model inaccurate (no per-tenant credential store exists; replay safety is DB idempotency); P1 settlement route principal model (staff + permission; players rejected); P2 `b22d5c4` static review: no material risk, live checks owed, 30 cases; P2 audit client-IP; P2 RLS on new tables; P3 no re-sequencing; P3 no secrets in the proposal | All adopted (W0 item 2, W1 items 6, 8, §O); webhook requirements retained for the real-provider stage |
| `qa` | CHANGES REQUIRED | P1 F-2 severity; reversibility-step ordering; named W1 test cases; ≥5 consecutive green runs; flake policy; mutation pass; mechanical W0→W1 gate; keep W0 a workstream | All adopted (§2.4, W0 item 5, §R, §S) |
| `devops` | APPROVED WITH CHANGES | P1 action reference; P1 CREATEDB gap (reject granting it to `igaming`); P2 `b22d5c4` well scoped and test-covered, live checks need a simulate-capable credential; P2 staging disposition decision; P3 one-commit-per-environment confirmed | Adopted; the "action v6 supports golangci-lint v2" claim is **contested** by the Orchestrator and left to verification from upstream documentation in W0 item 1 |
| `product-owner-proxy` | APPROVED WITH CHANGES | Right next stage; no scope creep in W1; exclusions correct; P2 staging disposition approval; P3 hardening is sequenced, not deprioritized | Adopted (§V item 4, §6, §G) |

Webhook requirements recorded for the future real-provider stage
(`security` finding 3): secret source (never committed or logged);
HMAC verified with `hmac.Equal` before any field is used; replay
control = DB unique constraint + tombstone (timestamp window optional);
tenant from the URL slug, server-resolved, identical 404 for
unknown/inactive tenant or provider, body size limit, work inside
`WithTenant`; bet binding with 409 + integrity alert on cross-bet or
cross-player references; payout ≤ frozen `potential_return`;
DB-enforced transitions.

### 9.2 Conflicts resolved by the Orchestrator

- **R-1 — Settlement driver: test-support staff route (adopted) vs
  signed mock webhook (`sportsbook`).** Evidence: every bet is placed in
  in-house mode — `provider_id`/`provider_tx_id` NULL
  (`internal/sportsbook/orchestrator.go`, §14.6 comment) and
  `provider_bet_reference` NULL on every row (migration 0081 header).
  ADR 0038 §14.6 keys every in-house lifecycle posting on
  `(tenant_id, idempotency_key)`. A provider-keyed webhook has nothing to
  bind to, and mixing modes on one bet is undefined by any ADR. The
  `architect`, `ledger-finance` and `security` findings converge on this.
  The webhook moves to the real-provider stage.
- **R-2 — Self-exclusion seeding approval withdrawn.** ADR 0042 recorded
  the human answer `VOID_ON_SELF_EXCLUSION` (platform-wide fallback) with
  "LEGAL / COMPLIANCE REVIEW REQUIRED: Yes". Re-asking the human to
  authorize it would reframe a decided item (`architect`). The consumer
  stays out of scope for the engineering reasons ADR 0042 itself lists;
  legal review remains a gate before production use.
- **Contested, not resolved by assertion:** the `golangci-lint-action`
  major version (devops "v6" vs the Orchestrator's understanding) — to
  be verified from upstream documentation in W0 item 1.

## 10. Risks

| # | Risk | Mitigation |
|---|---|---|
| 1 | Restoring CI surfaces further latent failures beyond F-2/F-3 | W0 is a hard gate with ≥5 green runs and a root-cause flake policy; W1 cannot merge first |
| 2 | Settlement contract built on the mock diverges from a future real provider's | In-house mode is a documented ADR 0038 §14.6 mode, not a provider contract; no webhook or provider shape is invented; provider mode is deferred with requirements recorded |
| 3 | Financial defects in correction paths (void-after-settlement, tombstones, repeated corrections) | Rollback-then-void shape; DB-enforced transitions; payload comparison; named tests; mutation pass; `ledger-finance` sign-off |
| 4 | F-7 turns out to affect an existing domain | W1 item 0 audits first; any finding becomes its own reviewed item |
| 5 | Lock-order inversion or deadlock | Explicit order, ADR 0082 amendment, deterministic blocker-based tests |
| 6 | Test-support route mistaken for an operator feature or reachable in production | ADR 0085 double opt-in, staff principal and permission, production-absence tests, ADR 0088 wording |
| 7 | Negative `player_cash` after a won-settlement rollback without a business process (OB-1) | Posting is correct and mandated; OB-1 remains an explicit pre-production decision (§V item 5) |
| 8 | Records drift again | W0 item 6 refresh; stage-end record update is an acceptance criterion |
| 9 | Staging cost / attack surface while unused | Human decision §V item 4 |
| 10 | Staging acceptance evidence exists only as attestation | W0 item 7 records the human-supplied checklist |

## 11. Required approvals

See §V (five items). No implementation, migration, application-code,
Terraform, IAM or AWS change is made until item 1 is approved.

## 12. Exact next action after approval

1. Record ADR 0087 (Stage 10 definition, as approved, including the
   human's answers to §V items 2–5) and open the Stage 10 section in
   `docs/governance/task-registry.md` with owners per §J.
2. Start **W0 item 1** (`devops`): verify the correct
   `golangci/golangci-lint-action` major for golangci-lint v2 from
   upstream documentation, fix the reference and pin the linter
   version; then W0 items 2–7 in order, with `code-reviewer` and (for
   item 2) `security` review.
3. Do not start W1 code until W0's acceptance (§S items 1–6) is met and
   recorded. In parallel with W0, only documentation work may proceed:
   `sportsbook` + `ledger-finance` draft ADR 0088 and `qa` drafts the W1
   test plan, for review — no settlement code, no migration.
4. Stop and report at the end of W0 before W1 implementation begins
   only if W0 produces a finding that changes W1's scope; otherwise
   continue into W1 under this approval and stop at the Stage 10
   completion gate.
