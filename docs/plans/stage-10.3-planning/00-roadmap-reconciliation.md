# Stage 10.3 planning — 00 Master roadmap reconciliation

- **Type:** documentation research only. No code, no decision, no AWS action, no commit.
- **Repository state read:** branch `claude/focused-wright-jw88w9`, HEAD `957a3e8` (Stage 10.2 records commit), working tree clean at read time apart from this planning folder.
- **Sources:** `CLAUDE.md`, `MASTER-BUILD-PROMPT.md`, `docs/progress.md`, `docs/active-stage.md`, `docs/governance/task-registry.md`, `docs/governance/project-status.md`, `docs/governance/stage-10*-completion-report.md`, `docs/governance/stage-4i-*.md`, `docs/governance/wave-*-report.md`, the Human Decision Register ADRs (0039, 0041, 0042, 0044), ADRs 0009 and 0080–0091, `docs/testing/testing-strategy.md`, `docs/runbooks/*.md`, `docs/plans/**`.
- **Nothing in this document decides anything.** Where an item is marked "decided", the cited record shows a human answer. Where it is "open", no record of a human answer was found.
- **Line references** such as `progress.md:7955` point to the section heading at that line at HEAD `957a3e8`.
- **Stage completion reports.** Only Stages 10, 10.1 and 10.2 have a standalone completion-report file. For earlier stages the report was delivered in conversation. The in-repo record is the matching `docs/progress.md` section, plus `docs/active-stage.md` and the wave/4I reports where they exist. `progress.md:11`, `:64` and `:224` say this explicitly.

---

## 1. COMPLETED

Format: stage — scope — owner — completion record.

### Stages 0–3

- **Stage 0** — discovery, governance, architecture inventory — Orchestrator — `docs/progress.md:7` (report in git history / conversation, `progress.md:11`)
- **Stage 1** — platform-api foundation — Orchestrator/backend — `docs/progress.md:20` (report in conversation, `progress.md:64`)
- **Stage 2** — identity, tenancy, security — identity/security — `docs/progress.md:119` (report in conversation, `progress.md:224`)
- **Security Hardening Pass (pre-Stage-3)** — sessions RLS, ADRs 0016–0018 — security — `docs/progress.md:338`
- **Stage 3A** (+ addendum) — financial architecture freeze and payment-provider agnosticism — ledger-finance/payments — `docs/progress.md:432`, `:615`
- **Stage 3B** — core financial infrastructure — ledger-finance/payments — `docs/progress.md:762`
- **Stage 3C** — financial hardening — ledger-finance — `docs/progress.md:1068` (header "approved-pending")
- **Stage 3D** — withdrawal governance final gate — payments — `docs/progress.md:1243` (header "approved-pending")

### Stage 4 (4A–4H-B0-R7)

- **Stage 4A** — casino integration foundation — casino — `docs/progress.md:1353`
- **Stage 4D-RG** — RG player-status enforcement — identity-compliance — `docs/progress.md:1460`
- **Stage 4E** — person resolution — identity-compliance — `docs/progress.md:1581`
- **Stage 4F** — verification, documents, authentication — identity-compliance — `docs/progress.md:1736`
- **Stage 4G** — orchestration governance and Risk & Limits — risk — `docs/progress.md:1874`
- **Stage 4G-FINAL** — architectural hardening — architect — `docs/progress.md:2024`; `docs/active-stage.md:1303`
- **Stage 4G-FINAL-FINANCE-GATE** — financial sign-off — ledger-finance — `docs/progress.md:2292`
- **Stage 4H-A** (+ addendum) — bonus architecture freeze — bonus-engine — `docs/progress.md:2513`, `:2785`; `docs/active-stage.md:1157`
- **Stage 4H-B0** — bonus/gamification/retail scope plan — product-owner-proxy/architect — `docs/progress.md:2992`; `docs/active-stage.md:1092`
- **Stage 4H-B0-R1** — gate corrections — architect — `docs/progress.md:3439`; `docs/active-stage.md:958`
- **Stage 4H-B0-R2** — bonus financial gate clarification — ledger-finance — `docs/progress.md:3740`; `docs/active-stage.md:866`
- **Stage 4H-B0-R3** — rounding decision validation — ledger-finance — `docs/progress.md:3937`; `docs/active-stage.md:720`
- **Stage 4H-B0-R4** — asset registry, FX, dual-mode sportsbook architecture — architect — `docs/progress.md:4284`; `docs/active-stage.md:574`
- **Stage 4H-B0-R5** — implementation readiness, P1 closure — architect — `docs/progress.md:4661`; `docs/active-stage.md:379`
- **Stage 4H-B0-R6** — foundational implementation hardening — ledger-finance/risk/security — `docs/progress.md:5014`; `docs/active-stage.md:199`
- **Stage 4H-B0-R7** — `player_locked` split (migration 0048), HDR formalised — ledger-finance/product-owner-proxy — `docs/progress.md:5332`; `docs/active-stage.md:3`; HDR `docs/decisions/0039-human-decision-register-stage-4h-b0-r7.md`

### Stage 4H-B1 (Bonus Engine)

- **Stage 4H-B1 Wave 1 / 1.5 / 1.5 Fix Wave** — bonus design waves — bonus-engine — `docs/progress.md:5449`; `docs/governance/wave-1.5-fixwave-phase2-report.md`
- **Stage 4H-B1 Wave 1.5 Fix Round 2** — P0 closure and product-surfaces roadmap — Orchestrator — `docs/governance/wave-1.5-fix-round-2-report.md`; `docs/active-stage.md:1444`
- **Stage 4H-B1 Wave 2** — real Bonus Engine code — bonus-engine — `docs/progress.md:5500`; `docs/governance/wave-2-report.md`
- **Stage 4H-B1 Wave 3** — bonus completion and final financial gate (verdict READY) — bonus-engine/ledger-finance — `docs/progress.md:5540`; `docs/governance/wave-3-report.md`

### Stage 4I (jurisdiction)

- **Stage 4I** — jurisdiction resolution foundation — architect — `docs/progress.md:5615`; `docs/governance/stage-4i-report.md`
- **Stage 4I Phase A** — tenant-licence write path — architect — `docs/progress.md:5683`; `docs/active-stage.md:4018`
- **Stage 4I Phase B** — player jurisdiction evidence — identity-compliance — `docs/progress.md:5802`; `docs/active-stage.md:3917`
- **Stage 4I PHASE-B-ARCH-1** — activation-gate asymmetry — architect — `docs/progress.md:5940`; `docs/active-stage.md:3853`
- **Stage 4I Phase C** — precedence and resolution rules — architect — `docs/progress.md:6034`; `docs/active-stage.md:3740`
- **Stage 4I Phase D** — jurisdiction policy configuration — architect — `docs/progress.md:6169`; `docs/active-stage.md:3631`; HDR `docs/decisions/0044-*.md`
- **Stage 4I Phase E** — operating market foundation (mechanism only) — architect — `docs/progress.md:6300`; `docs/active-stage.md:3264`
- **Stage 4I Phase E-SECURITY** — registry RLS hardening — security — `docs/progress.md:6679`; `docs/decisions/0046-tenant-licence-registry-rls.md`
- **Stage 4I Exit Triage** — exit register — Orchestrator — `docs/progress.md:6859`; `docs/governance/stage-4i-exit-register.md`

### Stages 5–9.4

- **Stage 5** — Operator Back Office MVP — backoffice — `docs/progress.md:7043`; `docs/active-stage.md:2769`
- **Stage 6** — B2C MVP and first sportsbook slice — frontend/sportsbook — `docs/progress.md:7133`; `docs/active-stage.md:2576`
- **Stage 6.1** — B2C/sportsbook hardening — sportsbook — `docs/progress.md:7206`; `docs/active-stage.md:2411`
- **Stage 7** — B2C casino slice (ADR 0048) — casino — `docs/progress.md:7261`; `docs/active-stage.md:2280`
- **Stage 8** — provider readiness without contracts (ADR 0080) — integrations — `docs/progress.md:7340`; `docs/active-stage.md:2194`
- **Stage 9** — production readiness and launch hardening — Orchestrator — `docs/progress.md:7395`; `docs/active-stage.md:2082`
- **Stage 9.1** — production blocker closure (ADRs 0081, 0082) — backend/architect — `docs/progress.md:7489`; `docs/active-stage.md:1996`
- **Stage 9.2** — sportsbook risk, jurisdiction, casino governance (ADR 0083) — sportsbook/risk — `docs/progress.md:7560`; `docs/active-stage.md:1891`
- **Stage 9.3** — staging package and local end-to-end acceptance (ADR 0084) — devops — `docs/progress.md:7675`; `docs/active-stage.md:1704`
- **Stage 9.4 Part 1** — APP_ENV fail-closed (ADR 0085) — backend — `docs/progress.md:7769`; `docs/active-stage.md:1594`
- **Stage 9.4** — staging hardening and cost (ADR 0086) — devops — `docs/progress.md:7848`; `docs/active-stage.md:1561`
- **Stage 9.4 AWS deployment and acceptance** — deployed `9190d5d`, human-executed and human-attested — human/devops — `docs/progress.md:7955`

### Stages 10–10.2

- **Stage 10 planning gate** — state reconstruction — Orchestrator — `docs/plans/stage-10-planning-gate-proposal.md`; `docs/progress.md:7984`
- **Stage 10 (W0 + W1)** — CI restoration and sportsbook settlement, MOCK (ADRs 0087, 0088) — devops/ledger-finance/sportsbook — `docs/governance/stage-10-completion-report.md`
- **Stage 10.1 planning gate** — PAY-REV-1/SB-T1-XMIN plan, ADR 0089 — Orchestrator — `docs/plans/stage-10.1-planning-gate-proposal.md`; `docs/plans/stage-10.1-planning/`
- **Stage 10.1** — PAY-REV-1, SB-T1-XMIN, PAY-WH-TENANT-1 (MOCK resolver) (ADR 0090) — payments/sportsbook/security — `docs/governance/stage-10.1-completion-report.md`
- **Stage 10.2** — KYC-WH-1, CAS-WH-TENANT-1, PAYWH-GATE-1 (MOCK providers) (ADR 0091) — identity-compliance/casino/security — `docs/governance/stage-10.2-completion-report.md`. Status: stopped at the deployment gate. No human acceptance of the 10.2 report is recorded.

---

## 2. IN PROGRESS

- **AWS staging teardown** (`deploy.sh down` of the environment running `9190d5d`) — executed by human/devops. **This is not recorded in the repository.** `docs/progress.md:5`, `docs/active-stage.md:1483` and `stage-10.2-completion-report.md` §15/§16 still say staging runs `9190d5d` and wait for human authorization. The only evidence is the directive for this task.
- **Stage 10.3 planning** (this folder) — Orchestrator — `docs/plans/stage-10.3-planning/`. Documentation only.
- **No other in-progress work** is recorded after Stage 10.2. Evidence: `docs/progress.md:8156` and `docs/active-stage.md:1483` both say "stopped at the deployment gate"; registry "Stage 10.2" rows are all IMPLEMENTED or Registered (`task-registry.md:3819`).
- **Stale "In progress" rows** remain in older registry sections: `task-registry.md:399-407`, `:453-457` and `:680`. They belong to concluded stages; see Discrepancies.

---

## 3. NEXT

- **Stage 10.3 — Real Provider Trust & Casino Financial Readiness (planning)** — Orchestrator (+ architect, casino, ledger-finance, payments, identity-compliance, security, devops, qa).
  - **Not defined in the repository.** No ADR or registry row names "Stage 10.3". The title comes from the current directive.
  - The closest recorded recommendation is `docs/governance/stage-10.2-completion-report.md` §17. It proposes (1) the staging refresh first, then (2) a planning gate for pre-real-provider hardening.
  - Recorded candidate scope:
    - WH-VENDOR-SCHEME-1
    - CAS-CAP-ROLLBACK-1 (with casino reconciliation)
    - MOCK-ADAPTER-PROD-1
    - secret-store design for real webhook resolvers
  - Optional: CI-FLAKE-281 timeout margin.
  - A stage-definition ADR (precedent: ADRs 0087, 0090, 0091) and human approval are required before implementation (`CLAUDE.md` "Stage-gate rule").

---

## 4. BLOCKED (technical)

- **S10-W0-05 — `b22d5c4` live IAM re-validation** (Access Analyzer `ValidatePolicy`, 30-case `simulate-deployer-policies.py`) — security + devops.
  - Blocked because the deployer credential is denied `access-analyzer:ValidatePolicy` and `iam:SimulateCustomPolicy`. The human did not supply a credential (ADR 0087 answer 3).
  - Sources: `task-registry.md:3753`; `stage-10-completion-report.md` §9.1.
- **SB-JUR-RUNG2-1 — sportsbook jurisdiction rung 2** — sportsbook — blocked on HDR-J-7. Five CI placeholder tests skip "BLOCKED on HDR-J-7". Sources: `docs/decisions/0083-*.md` §5.3.3; `task-registry.md:3764`.
- **Operating-market / jurisdiction resolver wiring** (registration, deposit, withdrawal, wagering, bonus, catalogue call sites) — architect. Blocked on HDR-J-6/7/8/9, HDR-M-1/2, MKT-DUAL-1, MKT-LICSTATUS-1 and MKT-EXPIRY-1. Source: `docs/governance/stage-4i-exit-register.md` "Integration contract".
- **Bonus sweep issuance** (deposit/reload/cashback) — bonus-engine. Built, but denied at runtime by the platform-wide jurisdiction-resolver gap. Sources: `docs/governance/wave-3-report.md` §1; `progress.md:5540`.
- **Asset eligibility wiring** — ledger-finance. The first domain to wire `CheckEligibility` denies everything until the seven seeded assets are platform-authorized by a dual-controlled act. Source: `docs/governance/project-status.md` "Blocked stages".
- **Backup / DR** — devops. "NOT MET"; blocked on ADR 0009 production provider/AUP. Source: `docs/runbooks/backup-and-disaster-recovery.md:13`.
- **Stage 10.1 / 10.2 staging acceptance** — human/devops. Blocked on the staging refresh (teardown now running; `up` not recorded). Sources: `stage-10.2-completion-report.md` §16; `stage-10.1-completion-report.md` §15.

---

## 5. WAITING FOR HUMAN DECISION

"OPEN" means no human answer was found. "DECIDED" cites the answer. Nothing here is decided by this document.

### Requested items

- **ADR 0009 residual — gambling AUP, contractual permission, data residency, final production cloud provider** — human/legal — **OPEN**.
  - ADR 0009 "What this decision does not do"; ADR 0084 item 9; ADR 0086 ("AUP/legal confirmation stays open"); `stage-10.1-completion-report.md` §11.
  - Staging on AWS does not close it.
- **HDR-J-6 — permitted markets for the first B2C brand** — human/legal — **OPEN**. The human answered "Not yet determined … must be established through a jurisdiction-by-jurisdiction legal/compliance review" (`docs/decisions/0042-human-decision-response.md` HDR-J-6). This is a launch-governance dependency.
- **HDR-J-7 — OperationClass → Purpose** — human — **OPEN**. `docs/decisions/0044-*.md` ("Record only"); `stage-10-planning-gate-proposal.md` §7.
- **HDR-J-8 — location signal required, advisory or undecided** — human/legal — **OPEN**. `docs/decisions/0044-*.md`.
- **HDR-J-9 — location-signal staleness** — human/legal — **OPEN**. `docs/decisions/0044-*.md`.
- **HDR-M-1 — governance model for tenant country enablement** (MKT-DUAL-1) — human — **OPEN**. `docs/governance/stage-4i-exit-register.md:521`.
- **HDR-M-2 — treatment of existing players after a country is disabled** — human — **OPEN**. `docs/governance/stage-4i-exit-register.md:522`.
- **HDR-SB-1 — who carries the trading-book liability; exposure ceiling before go-live** — human — **OPEN**. The exposure gate is unarmed (zero `sb_exposure_limits` rows). Sources: `task-registry.md:3346`; ADR 0083 §8.2.
- **OB-1 — receivable / negative `player_cash` after a won settlement is rolled back** — human/finance — **OPEN**.
  - The posting is implemented; the business treatment is open.
  - ADR 0087 answer 5 ("Remains OPEN"); `docs/architecture/ledger-accounting-model.md:2658`; `stage-10-completion-report.md` §10.
- **G-2 — Terminal-Grant settlement credit** — **DECIDED** (not implemented).
  - Answer: all three treatments configurable per brand; default (b) route to `player_cash`.
  - Source: `docs/decisions/0042-*.md` "G-2".
  - Brand-level configurability and the (c) manual-review workflow are NOT IMPLEMENTED. Legal review is "Recommended".
- **OpenBetSelfExclusionPolicy platform-wide default** — **DECIDED**.
  - Answer: `VOID_ON_SELF_EXCLUSION`, as the platform-wide fallback only (`docs/decisions/0042-*.md`).
  - Legal/compliance review is required before production use. The value is not seeded, and no auto-void consumer exists (`stage-10-planning-gate-proposal.md` §5).
- **Mixed / bonus-funded sportsbook cashout policy** — **DECIDED**: "Not cashout-eligible" (`docs/decisions/0042-*.md`). Legal review is "Recommended".

### Licensing, legal, vendor and retail

- **Licence / jurisdiction selection beyond Anjouan** — human/legal — **OPEN**. `CLAUDE.md` "When to stop and ask"; `project-status.md:474`.
- **HDR-J-3e — lawful basis / permitted use** — legal counsel — **OPEN** for validation. The human set the principle, but "must be validated by qualified legal/privacy counsel before production use" (`docs/decisions/0042-*.md` §3e).
- **HDR-J-3f — retention periods per data type and jurisdiction** — legal — **OPEN** for concrete periods. The principle is decided (`docs/decisions/0042-*.md` §3f).
- **Legal interpretation of RG/KYC/AML per jurisdiction before real-money go-live** — legal — **OPEN**. `project-status.md:474`; `stage-10-planning-gate-proposal.md` §7.
- **Vendor: KYC/AML provider** — human/commercial — **OPEN**. Named "the single highest-leverage open item" (`project-status.md` open decision 4 and :466).
- **Vendor: PSP(s)** — human/commercial — **OPEN**. `project-status.md:466`.
- **Vendor: casino aggregator** — human/commercial — **OPEN**. `project-status.md:466`.
- **Vendor: sportsbook feed/widget** — human/commercial — **OPEN**. `project-status.md:466`.
- **Vendor: institutional crypto custodian** — human/commercial — **OPEN**. ADR 0008; `MASTER-BUILD-PROMPT.md` "Business decisions" item 4.
- **Production credentials and commercial pricing** for contracted vendors — human — **OPEN**. `project-status.md:474`.
- **Retail items 1–14** (doc 27 §24) — human/legal/commercial — **OPEN**. Item 15 is resolved. Source: `docs/architecture/27-stage-4h-b0-scope-and-implementation-plan.md:1016`. The items:
  1. licensing structure;
  2. agents as legal entities;
  3. `agent_float` ADR 0007 amendment;
  4. anonymous/bearer play;
  5. offline policy;
  6. proxy play;
  7. commission terms;
  8. agent credit;
  9. franchise model;
  10. cash AML thresholds;
  11. retail KYC evidence;
  12. terminal fleet;
  13. hierarchy-authored risk limits;
  14. first retail market.

### Other open decisions found

- **project-status open decision 1 — cross-tenant KYC reuse** — human/legal — **OPEN**. ADR 0028 §3/§7; `project-status.md:215`.
- **project-status open decision 2 — real KYC `VerifiedAttributes` to PersonResolver** — human — **OPEN**. ADR 0027 §7; `project-status.md:215`.
- **project-status open decision 3 — BYOL opt-out of platform-wide person resolution** — human/legal — **OPEN**. ADR 0027; `project-status.md:215`.
- **project-status open decision 5 — RiskDecision REVIEW: provisional or blocking** — product — **OPEN**. ADR 0031 §6; `project-status.md:215`.
- **project-status open decision 6 — HARD_LIMIT vs CONFIGURABLE_LIMIT precedence** — legal/product — **OPEN**. ADR 0031 §5; `project-status.md:215`.
- **project-status open decision 7 — platform-scoped risk-rule write path** — product/architect — **OPEN**. ADR 0031 §8; `project-status.md:215`.
- **ADR 0023 §6 — production withdrawal approval thresholds; whether policy-edit and approval roles must be disjoint** — human/finance — **OPEN**. `docs/decisions/0023-stage3c-financial-hardening-decisions.md` §6.
- **FD-2 — settlement-finality window** (optional bonus-conversion narrowing) — product-owner-proxy/human — **OPEN**. `docs/architecture/ledger-accounting-model.md:4750`; `stage-10-planning-gate-proposal.md` §7.
- **ADR 0089 §8 items 2–4** — AI model/provider/hosting/residency; automated-targeting restrictions; "vulnerable player" status — human/legal — **OPEN** (future). `docs/decisions/0089-*.md` §8.
- **Staging refresh (10.2 report §16) / staging disposition** — human.
  - Teardown is reported as running (unrecorded).
  - Whether `deploy.sh up` at the Stage 10.2 head follows is **not recorded**.
  - Sources: `stage-10.2-completion-report.md` §16/§17; ADR 0091.
- **Verification credential for `b22d5c4` live checks** — human — **OPEN**. The human declined to supply one (ADR 0087 answer 3), so the item stays BLOCKED until a credential with the two read-only actions exists.
- **Stage 10.3 authorization** — human — **OPEN**. `CLAUDE.md` stage-gate rule; `stage-10.2-completion-report.md` §17.
- **Production launch authorization** — human — **OPEN**. `CLAUDE.md` "When to stop and ask".

### Already decided — recorded here to prevent reopening

- **HDR-J-1, HDR-J-2, HDR-J-4, HDR-J-5** — DECIDED — `docs/decisions/0042-*.md`.
- **FD-1 "Nullifying"** — DECIDED — `docs/decisions/0042-*.md`.
- **Converted-Grant cancellation: no automatic clawback** — DECIDED — `docs/decisions/0042-*.md`.
- **project-status items 8–11** — marked ANSWERED — `project-status.md:215`.
- **ADR 0021 rounding (DS-1/2/3)** — DECIDED — ADR 0021.
- **KYC-WH-1 and CAS-WH-TENANT-1 scope rulings** — DECIDED — ADR 0091.
- **PAY-WH-TENANT-1 scope** — DECIDED — ADR 0090 amendment.

---

## 6. DEFERRED

### Webhooks and payments

- **PAYWH-BRAND-1** — the webhook capability check has no brand scoping — payments + security — `task-registry.md:3801` (10.1 table); ADR 0091; 10.2 ruling J10 (`docs/plans/stage-10.2-planning/01-webhook-trust-design.md`).
- **PAYWH-RL-1** — no webhook rate limiting — payments + security — same sources.
- **PAYWH-TS-1** — no signed-timestamp replay window — payments + security — same sources.
- **KYC-REASON-BOUND-1** — bound the provider `reason` length/charset in the first real KYC adapter — identity-compliance — `task-registry.md` Stage 10.2; `docs/plans/stage-10.2-planning/09-review-security-final.md` F-7.
- **WH-VENDOR-SCHEME-1** — header parsing must become an adapter/`Scheme` capability before the first real adapter — architect — `task-registry.md` Stage 10.2; ADR 0022 §3 Stage 10.2 amendment.
- **MOCK-ADAPTER-PROD-1** — mock payments and casino adapters are still registered in production for initiation, catalogue and launch; pre-launch item — architect + devops — `task-registry.md` Stage 10.2; `stage-10.2-completion-report.md` §11.
- **CAS-CAP-ROLLBACK-1** — a disabled casino capability 503s verified wins/rollbacks, writes no tombstone, and no casino reconciliation exists; **hard pre-condition for any real casino resolver** — casino + ledger-finance — `task-registry.md` Stage 10.2; `docs/plans/stage-10.2-planning/10-review-ledger-finance.md` §5; ADR 0025 amendment.
- **CI-FLAKE-281** — unidentified intermittent integration failure (run #281); not reproduced; diagnostics added; open pending recurrence — devops + qa — `task-registry.md` Stage 10.2; `docs/plans/stage-10.2-planning/02-ci-flake-281-investigation.md`.
- **Unknown-slug timing gap** — accepted residual — security — `01-webhook-trust-design.md` ruling K15.
- **LEDGER-REV-UNIQ** — cross-type, amount-aware "one reversal per original" — ledger-finance — `task-registry.md` Stage 10.1; ADR 0090 "Out of scope".
- **REV-UNIQ-CASINO** — unique index for `casino_rollback` (P3) — casino + ledger-finance — same sources.
- **Single-column `reverses_transaction_id` FK (not tenant-scoped)** — ledger-finance — `stage-10.1-completion-report.md` §10.
- **0092 refusal message lacks Postgres `DETAIL`** — ledger-finance — `stage-10.1-completion-report.md` §10; ADR 0020.
- **10.1 P3 polish** (composite resolver still wired in `main.go`; one error branch bypasses the mapper) — payments/backend — `stage-10.1-completion-report.md` §10.

### Sportsbook

- **SB-T1-XMIN-STRADDLE** — epoch-boundary straddle (P3) — sportsbook/ledger-finance — `stage-10.1-completion-report.md` §10.
- **OI-5 / R-3 named debt** — cumulative limits measure net outflow by posting time — risk — `stage-10-completion-report.md` §11.
- **L0.6 rollback re-opening residual** — must be revisited before arming exposure limits — sportsbook/risk — `stage-10-completion-report.md` §11.
- **Sportsbook money width** (`NUMERIC(38,0)` / `big.Int` for crypto) — ledger-finance — `stage-10-completion-report.md` §11.
- **Reconciliation scale** (full recompute; unbounded catalogue read) — ledger-finance — `stage-10-completion-report.md` §11.
- **Test-support settlement route removal** at the real-provider stage — sportsbook — `stage-10-completion-report.md` §11.
- **Reserved `sportsbook_` provider-id prefix guard** — sportsbook — `stage-10-completion-report.md` §11.
- **`pgx.Batch.Close()` error-branch mutation gap; handler mutation scope PARTIALLY IMPLEMENTED** — qa — `stage-10-completion-report.md` §11.
- **Record tidy-ups** (ADR 0038 `player_locked` wording, ADR 0083 §7.3, ADR 0082 P3-3 note) — architect — `stage-10-completion-report.md` §11.

### Stage 10 W0 deferral

- **Stale ALB security-group description from `b22d5c4`** (P3) — changing it forces SG replacement, so it was deferred to the next authorized staging change window — devops — `task-registry.md:3745` ("W0 findings"); `stage-10-completion-report.md` §11.
- **Integration-tagged test files not linted** (467 findings) — qa/devops — same sources.
- **Three statement-text lock-wait helpers** — qa — same sources.

### Bonus Engine (Stage 4H-B1)

- **Status:** Waves 1–3 COMPLETE (READY). Wave 4 is NOT authorized.
- Sources: `progress.md:5540`; `docs/governance/wave-3-report.md` §1; `docs/active-stage.md:1444`; `project-status.md` table row "4H-B1".
- **Deferred bonus items** (owner bonus-engine):
  - bonus-funded / locked-stake staking;
  - 3 of 7 `ChangeOperation` types unwired;
  - bulk-job HTTP-execute gap F3;
  - latent `ConvertGrant` lock-order inversion;
  - `bonus_campaign_activation` EOI with no mint point (DR-4HB1W3-ARCH-02);
  - G-2 brand configurability;
  - Back Office UI for bonus admin.
- **Not authorized:** CRM, Affiliate, Gamification, Partner Console (6B), Retail/POS (4H-B2/B3, 6D) — `docs/active-stage.md:1444`; `project-status.md` "Blocked stages".

### Other deferred

- **FX / Conversion (ADR 0037 Part B)** — NOT IMPLEMENTED; FX control-plane security findings open — ledger-finance/security — `project-status.md` "Blocked stages"; ADR 0037.
- **Sportsbook cashout, partial settlement / accumulators, liability reporting, self-exclusion auto-void consumer** — sportsbook — ADR 0087 "Out of scope"; `stage-10-planning-gate-proposal.md` §5.
- **ADR 0086 deferrals** — devops/backend — `docs/decisions/0086-*.md` "Deferred":
  - audit-record client IP;
  - joining duplicate `X-Forwarded-For` headers;
  - CloudFront/ALB/VPC flow logs;
  - RDS `verify-full`;
  - serving the SPAs from S3.
- **4I exit-register items** — architect/security — `docs/governance/stage-4i-exit-register.md` summary table:
  - PLAT-TENANTREAD-1;
  - MKT-LICSTATUS-1;
  - MKT-AUDIT-1;
  - MKT-DORMANT-1;
  - MKT-DUAL-1;
  - MKT-EXPIRY-1;
  - MKT-PM-1.
- **AI-ARCH-FUTURE (ADR 0089)** — binding future requirement, NOT IMPLEMENTED — architect — `task-registry.md` Stage 10.1; ADR 0089.
- **Stage 9.1 QA technical-debt deferrals** (migration 0079 strategy; mock inbound-failure injection; B2C build-time brand model) — qa — `task-registry.md:2868`.
- **F-6 acceptance evidence (Stage 10 W0 item 7)** — add the human-supplied staging acceptance checklist to the lifecycle runbook — Orchestrator.
  - **No completion record found.** No registry W0 row covers it, and the runbook has no such checklist.
  - Sources: `stage-10-planning-gate-proposal.md` W0 item 7; `task-registry.md:3731`.

---

## 7. PRODUCTION / PROVIDER BLOCKERS

### Credentials, resolvers and adapters

- **Real `WebhookCredentialResolver` + secret store** (payments, KYC, casino) — NOT IMPLEMENTED / PROVIDER DEPENDENT — payments/identity-compliance/casino + security — `stage-10.2-completion-report.md` §10; `stage-10.1-completion-report.md` §10.
- **Real PSP adapter** plus withdrawal payout, callbacks and reconciliation — PROVIDER DEPENDENT — payments — `stage-10-planning-gate-proposal.md` §5; `project-status.md:466`.
- **Real KYC/AML adapter** — PROVIDER DEPENDENT. Production self-service KYC returns 503 without it — identity-compliance — `production-configuration-checklist.md` "Fields intentionally NOT…"; `stage-10.2-completion-report.md` §11.
- **Real casino aggregator adapter** — PROVIDER DEPENDENT; gated by CAS-CAP-ROLLBACK-1 and WH-VENDOR-SCHEME-1. ADR 0048 simulation is superseded when a real provider exists — casino — `task-registry.md` Stage 10.2; ADR 0048.
- **Real sportsbook provider** (webhook, provider-mode idempotency, statement matching) — PROVIDER DEPENDENT — sportsbook — `stage-10-completion-report.md` header; `stage-10-planning-gate-proposal.md` §6.
- **Crypto custodian integration** (ADR 0008) — PROVIDER DEPENDENT — integrations — ADR 0008; `project-status.md:466`.
- **MOCK-ADAPTER-PROD-1** — remove or gate mock adapters before launch — architect + devops — `task-registry.md` Stage 10.2.

### Sportsbook production readiness

- **Exposure gate unarmed** (HDR-SB-1) — sportsbook — `task-registry.md:3346`.
- **Rung 2 blocked** (HDR-J-7) — sportsbook — ADR 0083 §5.3.3.
- **L0.6 residual; OI-5** — risk — `stage-10-completion-report.md` §11.
- **Money width** — ledger-finance — `stage-10-completion-report.md` §11.
- **Test-support route removal; reserved-prefix guard** — sportsbook — `stage-10-completion-report.md` §11.
- **`VOID_ON_SELF_EXCLUSION` not seeded; no auto-void consumer; legal review** — sportsbook/identity-compliance — `stage-10-planning-gate-proposal.md` §5.
- **OB-1 business treatment** — human/finance — ADR 0087.

### Jurisdiction and legal

- **Jurisdiction items gated by legal** — HDR-J-6/7/8/9, HDR-J-3e/3f, HDR-M-1/2; resolver wiring; MKT-PM-1 — human/architect — `stage-4i-exit-register.md`; `stage-10-planning-gate-proposal.md` §7.

### Platform security hardening

- **PLAT-ROLESPLIT-1 production step** — real production cutover to `igaming_runtime` — security/infra — `stage-4i-exit-register.md` summary; `progress.md:7395`.
- **Production signing (ADR 0018 KMS/HSM), staff MFA (ADR 0017 NOT IMPLEMENTED)** — security — ADRs 0017 and 0018; `stage-10-planning-gate-proposal.md` §5.
- **Audit client IP launch gate** — backend — ADR 0086 "Deferred"; `docs/governance/stage-10-w1-security-review.md:129`.
- **Back Office refresh-token storage; `IssueCredentialToken` race (ADR 0085); CloudFront→ALB plaintext HTTP inside AWS** — backend/security/devops — `stage-10-planning-gate-proposal.md` §5.

### Operations

- **Backup/DR "NOT MET"** — devops — `docs/runbooks/backup-and-disaster-recovery.md`.
- **Observability: no metrics backend; OTel exporter PROVIDER DEPENDENT** — devops — `production-configuration-checklist.md` "Observability"; `stage-10-planning-gate-proposal.md` §5.

### Production configuration checklist

Owner devops; source `docs/runbooks/production-configuration-checklist.md`:

- `DATABASE_URL` must use the non-owning `igaming_runtime` role with `sslmode=require` or stricter.
- `APP_ENV=production` exactly, verified from the startup log.
- `TEST_SUPPORT_ENDPOINTS_ENABLED` unset.
- `JWT_SIGNING_SECRET` freshly generated from a secrets manager; move to asymmetric KMS signing before go-live.
- `TRUSTED_PROXY_COUNT` set to the exact hop count.
- `CORS_ALLOWED_ORIGINS` set only when cross-origin.
- Secrets absent from logs.
- `/healthz` and `/readyz` return 200.
- Migrations complete before traffic.
- A real OTLP exporter configured.

### Governance

- **ADR 0009 AUP / production provider** — human/legal — ADR 0009.
- **Vendor contracts, production credentials, compliance/certification readiness (none claimed)** — human — `project-status.md:474`; `stage-10-planning-gate-proposal.md` §5.
- **Production launch authorization** — human — `CLAUDE.md` "When to stop and ask".

---

## 8. TESTING DEFERRED UNTIL NEXT STAGING DEPLOYMENT

**Baseline:** staging ran `9190d5d` (migration 0090). Stages 10, 10.1 and 10.2 (migrations 0091–0093 and all webhook changes) have never run on AWS.

### Stage acceptance runs

- **Stage 10 acceptance** — sportsbook settle/void/rollback simulation; Back Office bet lifecycle view; migration 0091 and runtime-role REVOKEs applied by `role-init` + `migrate` — **STAGING REQUIRED** — `stage-10-completion-report.md` §14; `stage-10.2-completion-report.md` §16.3/§16.5.
- **Stage 10.1 acceptance** — deposit, reversal and 409 via the simulate route; cross-tenant payments webhook returns 401; migrations 0092/0093 on a fresh DB — **STAGING REQUIRED** — `stage-10.1-completion-report.md` §15.
- **Stage 10.2 acceptance** — **STAGING REQUIRED** — `stage-10.2-completion-report.md` §16.5. Checks:
  - KYC player response has no `provider_reference`;
  - a forged KYC webhook returns 401;
  - staff approval through the review route;
  - a cross-tenant casino callback returns 401 with no effect;
  - CloudWatch logs carry no key material, signatures or references.
- **KYC-WH-1 live exposure closure on staging** — old staging KYC rows are untrusted. The refresh removes them, and that removal must be recorded — **STAGING REQUIRED** — `stage-10.2-completion-report.md` §11/§16.6.

### Edge, replicas and IAM

- **Edge allowlist** — `/readyz` returns 200 from the allowlisted CIDR and 403 elsewhere; ALB ingress only from the CloudFront origin-facing prefix list (`b22d5c4`) — **STAGING REQUIRED** — `stage-9-4-staging-lifecycle-runbook.md` §4, §12.5, §12.7; `progress.md:7955`.
- **Multi-replica test** — `deploy.sh scale 2`; activation token issue/confirm across replicas; unique deposit/casino references; 10.2 in-process simulate signing "also holds at 2 replicas". **No recorded execution found** — **STAGING REQUIRED** — `stage-9-4-staging-lifecycle-runbook.md` §5; `stage-10.2-completion-report.md` §16.4; ADR 0085.
- **`b22d5c4` live IAM validation** (Access Analyzer `ValidatePolicy`, 30-case simulation, `simulate-principal-policy`) — AWS credential required — **STAGING REQUIRED** — `task-registry.md:3753`; runbook §12.1.

### Secrets and RDS

- **Secrets Manager rotation** — bump `secret_version`; role-init `ALTER ROLE`; service roll. The RDS-managed master secret rotated by RDS — **STAGING REQUIRED** — `stage-9-4-staging-lifecycle-runbook.md` §10.
- **Secrets-in-state / log checks** — `terraform state pull` grep prints 0; role-init log has no password — **STAGING REQUIRED** — runbook §12.3–§12.4.
- **RDS behaviour** — PostgreSQL 16.15; role-init/migrate/seed-admin read the RDS-managed secret via the `aws:rds:primaryDBInstanceArn` tag condition (fallback documented); fresh-DB migration chain to 0093 — **STAGING REQUIRED** — runbook §3, §12.6; `stage-10.2-completion-report.md` §16.3.
- **Post-apply `terraform plan` shows no changes** (write-only secret refresh) — **STAGING REQUIRED** — runbook §12.2.

### Observability and teardown

- **Observability / alarms** — RDS alarms on `DBInstanceIdentifier`; ALB `HealthyHostCount < 1` alarms; SNS only with `alarm_email`. Firing was never exercised on AWS — **STAGING REQUIRED** — ADR 0086 §6; `progress.md:7848`.
- **Teardown behaviour** — `verify-teardown.sh` exit 0; lingering `CloudFront-VPCOrigins-Service-SG` / VPC-origin ENIs; RDS secret deletion mode; Access Analyzer shows no ACTIVE findings. "Unverified until the first real teardown", which is now running. Record the results — **STAGING REQUIRED** — `stage-9-4-staging-lifecycle-runbook.md` §6, §12.8, §12.10.
- **Deployer network-policy tag-scoping follow-up** (`ec2:CreateRoute`, `AssociateRouteTable`, `AttachInternetGateway`) after the first-apply checks pass — **STAGING REQUIRED** — runbook §12.9.
- **Stale ALB SG description fix** — can only be done in an authorized staging change window — **STAGING REQUIRED** — `task-registry.md` Stage 10 W0 findings.
- **F-6 acceptance evidence** — 9.4 acceptance was human-attested with no evidence artifact. The next acceptance should produce recorded evidence — **STAGING REQUIRED** — `progress.md:7955`; `stage-10-planning-gate-proposal.md` F-6.

---

## Discrepancies

1. **Staging teardown is unrecorded.** The records (`progress.md:5`, `active-stage.md:1483`, `stage-10.2-completion-report.md` §15) say staging still runs `9190d5d` and wait for authorization. The teardown now running has no repository record.
2. **`docs/active-stage.md` is out of order.** Its first section (line 3) is Stage 4H-B0-R7 and states that G-2, the `OpenBetSelfExclusionPolicy` default and the mixed-cashout policy "remain unmade". All three were answered in ADR 0042. The actual "Current stage" is at line 1483.
3. **`docs/active-stage.md:1444` (Wave 1.5 FR2)** still lists "four HDR items (G-2, OpenBetSelfExclusionPolicy default, mixed cashout, FD-1) remain unmade". All four are answered in ADR 0042.
4. **`docs/governance/project-status.md` is stale.**
   - Its completed table ends at "10.1 planning gate — awaiting human approval".
   - "Active stage" says Stage 10.
   - Stages 10.1 and 10.2 are missing.
   - "Blocked stages" still says sportsbook "not started" and `player_locked` phase 2 "NOT STARTED". Migration 0048 and Stage 10 W1 are done.
   - This is the F-4 staleness finding recurring.
5. **`project-status.md:474` open-bet self-exclusion bullet contradicts the record.** It still presents the self-exclusion question as open with a recommended default of "settle normally". ADR 0042 decided `VOID_ON_SELF_EXCLUSION`, and the same file's item 8 is marked ANSWERED.
6. **Registry rows left "In progress"** for concluded work: `task-registry.md:399-407` (4H-B1 Wave 1) and `:453-457` (Wave 1.5). The section header at `:680` reads "Phase 10 of 11 — IN PROGRESS", while `:712` and `wave-3-report.md` record Wave 3 as COMPLETE/READY.
7. **Registry Stage 10.1 table not marked superseded.** It still shows KYC-WH-1 as "needs a human scope ruling; NOT fixed" and CAS-WH-TENANT-1 as "outside 10.1". PAY-REV-1/SB-T1-XMIN in the Stage 10 table were marked "Superseded". `active-stage.md:1495` likewise still lists "Open human rulings: KYC-WH-1 scope". Both were resolved by ADR 0091 and implemented in 10.2.
8. **ADR status headers lag implementation.**
   - ADR 0081 status is "design ruling only — NOT IMPLEMENTED", but migration `0084_catalogue_write_authorization` exists and `progress.md:7489` records ARCH-DB-2 closed in Stage 9.1.
   - ADRs 0082/0083 headers say "design only", although Stage 9.1/9.2 and Stage 10 implemented them (migrations 0087, 0088, 0091).
   - ADR 0038 is still "Proposed … NOT IMPLEMENTED" in its header, although Stage 10 W1 implemented cash-funded settlement against it.
9. **F-6 / W0 item 7 has no disposition.** The acceptance-evidence checklist planned in `stage-10-planning-gate-proposal.md` W0 item 7 has no row in the W0 registry table, no mention in `stage-10-completion-report.md`, and no checklist in the runbook.
10. **Stage 9.4 first-apply verifications have no recorded results.** Runbook §12 items 1–10 and the §5 multi-replica test are not recorded in `progress.md:7955`, which records only human-attested B2C/Back Office acceptance.
11. **Stage naming mismatch.**
    - `MASTER-BUILD-PROMPT.md` and `CLAUDE.md` define Stages 0–7, with Stage 7 = "Integration, hardening, MVP release". The executed Stage 7 was the B2C casino slice, and Stages 8–10.2 were added. Only Stages 10, 10.1 and 10.2 have stage-definition ADRs (0087, 0090, 0091).
    - `MASTER-BUILD-PROMPT.md` was not revised through a recorded decision, as its own header requires.
    - `stage-10-completion-report.md` refers to the next stage as "Stage 11", but the next stage was 10.1.
12. **"approved-pending" labels never updated.** Several `progress.md` headers (3C, 3D, 4A–4G, 4H-B0-R7, Stage 7) still read "approved-pending", although later stages proceeded.
13. **Stage 10.3 has no definition in the repository.** "Stage 10.3 — Real Provider Trust & Casino Financial Readiness" is not in any ADR or registry. The 10.2 report §17 calls the next step "a pre-real-provider hardening stage".
