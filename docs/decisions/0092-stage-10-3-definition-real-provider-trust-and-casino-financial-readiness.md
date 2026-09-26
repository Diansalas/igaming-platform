# ADR 0092 — Stage 10.3 Definition: Real Provider Trust & Casino Financial Readiness

- **Status:** ACCEPTED 2026-09-26. The human authorized it ("MASTER ORCHESTRATOR — AUTHORIZE
  STAGE 10.3") against baseline `207c922`. The authorization is recorded in
  `docs/plans/stage-10.3-planning-gate-proposal.md` §22, which supersedes any conflicting text
  earlier in that proposal.
- **Decision type:** stage definition. Precedents: ADR 0087, ADR 0090, ADR 0091.
- **Owner:** Master Orchestrator. `architect` records this ADR.

> Numbering note: this is ADR 0092 in `docs/decisions/`. The migration
> `0092_deposit_reversal_one_per_original` in `migrations/` is an unrelated file, and so is
> migration `0093_*`, which is unrelated to ADR 0093. Stage 10.3 migration numbers are
> provisional (0094–0097) and are assigned in merge order.

## Context

Stages 10.1 and 10.2 made every webhook tenant-bound and verify-first, but only for the MOCK
providers. Before the first real PSP, KYC vendor or casino aggregator can be connected, five
things are still missing:

- a per-vendor verification contract (WH-VENDOR-SCHEME-1);
- a real credential resolver backed by a secret store;
- a production guard against synthetic adapters;
- a casino capability contract that never strands exposure (CAS-CAP-ROLLBACK-1), plus the
  multi-bet win defect G-1;
- casino reconciliation.

The evidence for each is in the planning papers (`docs/plans/stage-10.3-planning/01`–`03`) and
the reviews (`04-review-*.md`). The Orchestrator rulings R1–R15 are in the proposal's §19.

## Decision

### Scope and waves

| Wave | ID | Deliverable | Binding contract |
|---|---|---|---|
| W0 | — | Papers only: this ADR, ADR 0093, and dated "Amendment (Stage 10.3, ADR 0092)" sections in ADRs 0022 §3, 0085 §1, 0025, 0082 and 0028; a pointer in `docs/architecture/08-casino-integration-architecture.md`; registry; `docs/active-stage.md` | — |
| W1a | WH-VENDOR-SCHEME-1 | Per-adapter `VerificationScheme`; Verify enforced by the orchestrator; conformance suite `webhookauthtest` SC1–SC13; the remaining conformance skips for non-mock adapters become failures | ADR 0022 §3 (Stage 10.3 amendment) |
| W1b | MOCK-ADAPTER-PROD-1 | Synthetic / `ProductionEligible` markers; a startup guard that refuses to start | ADR 0085 §1 (Stage 10.3 amendment) |
| W1c | CAS-CAP-ROLLBACK-1, CAS-MULTIBET-WIN-1 (G-1) | Capability gates new bets only; tombstone always written; named late-original rejection; L0.1 in `postRollback`; CHECK migration; G-1 characterised, then fixed | ADR 0025 and ADR 0082 (Stage 10.3 amendments) |
| W1d | KYC-REASON-BOUND-1 | Provider reason bounded and sanitised; staff-only; players see status only | ADR 0028 (Stage 10.3 amendment) |
| W2a | PROV-CRED-RESOLVER-1, PROV-OUTBOUND-CRED-1, KYC-PROVIDER-SELECT-1 | Handle table, resolver, `memory`/`devfile` store backends, four-eyes activation, admin handle API; per-call outbound credentials; KYC provider chosen from configuration | ADR 0093 |
| W2b | CAS-RECON-1 | `casino_consistency` C1–C7 and the append-only `casino_callback_rejections` record | ADR 0025 amendment; paper 02 §2 |
| W3a | CAS-RECON-STMT-1 | `casino_statement` stream with a MOCK `CasinoStatementSource`. **First item to cut under schedule pressure** | paper 02 §2.5 |
| W3b | SECRETSTORE-AWS-1 | `awssm` backend code, tested against a local SDK-interface fake only | ADR 0093 |
| W1 | CI-FLAKE-281 | Resolve if reproducible, per R12. Thresholds are never weakened and concurrency or security coverage is never removed | QA review §6 |

Order and parallelism:
- W0 comes first.
- W1a–W1d run in parallel. W1c and W1d merge after W1a or rebase on it.
- W2a needs W1a. W2b needs W1c.
- W3a needs W2b. W3b needs W2a.

W1 and W2 security or financial controls are never traded away to deliver W3.

### Gates 10.3-W0..W3

A wave closes only when all of the following are true:

- its deliverables carry an honest label;
- CI is green: gofmt, vet, golangci-lint, race unit tests, integration tests passing 3× with
  race detection, migration reversibility, and OpenAPI contract tests;
- the per-wave test plan in `04-review-qa.md` §4 is met (R11). This includes red-before-green
  evidence under `docs/plans/stage-10.3-planning/evidence/`;
- the specialist reviews in proposal §17 are done, with `security` review before any
  security-sensitive item is marked complete;
- the secrets check names a real procedure (R10/C17).

`govulncheck` and a real secrets-scan CI step are added in the first code wave (W1), because W0
is docs-only. A `go.sum` drift check lands before or with W3b (QA condition 5).

| Gate | Closes when |
|---|---|
| 10.3-W0 | This ADR, ADR 0093 and the five amendments are recorded, together with the registry and `active-stage.md` updates. Concurrence is on record: `security` (04-review-security C1–C17), `ledger-finance` (paper 02), `identity-compliance` (paper 03, as narrowed by HD-10.3-3). |
| 10.3-W1 | W1a–W1d are each closed as above. CI-FLAKE-281 has a disposition. |
| 10.3-W2 | W2a and W2b are closed. The W2a four-eyes control is live when the admin API first ships (R1; there is never a single-actor form). |
| 10.3-W3 | W3a (or its recorded cut) and W3b are closed. The Stage 10.3 completion report is written. **Stop** for human authorization. |

### Human decisions (§22), binding

| ID | Ruling | Effect |
|---|---|---|
| HD-10.3-1 | APPROVED: full scope | W0 → W1a–W1d → W2 → W3 proceed. |
| HD-10.3-2 | **AWS IAM code changes are EXCLUDED** | W3b is backend code plus a local SDK fake only. There is no new IAM policy, role or KMS code in `deploy/`. ADR 0084's "empty/minimal task role" statement (also in the description of `deploy/aws/modules/iam/main.tf`'s task role) and ADR 0086 §17 are unchanged. Any new IAM architecture needs a separate human decision. |
| HD-10.3-3 | **Players see STATUS ONLY** | No player-facing `reason_code`, and no provider text on any player surface. The bounded, sanitised provider reason is available only to authorized staff and compliance. Migration 0095 adds the bound only; there is no `reason_code` column. |
| HD-10.3-4 | **UNCHANGED** | No new policy for settlement on a suspended tenant. The existing behaviour is documented in the ADR 0025 Stage 10.3 amendment. |

Further binding instructions:

- **PAYWH-TS-1, PAYWH-BRAND-1 and PAYWH-RL-1 remain DEFERRED** unless the implementation proves
  one of them is required.
  - R4's "PAYWH-TS-1 closes as superseded" is **withdrawn**.
  - Timestamp and replay rules for real schemes are part of W1a (ADR 0022 §3 new point 10).
  - TS-1 stays open until the 10.3 completion gate has evidence that supports closing it.
- **The Stage 10.2 MOCK scheme is not the protocol of any real provider.** A real provider is
  declared supported only after its actual documentation or contract has been implemented and
  tested, including the vendor's known-answer vectors.
- **AWS staging stays OFF.** There is no deployment without separate explicit authorization.
  Items marked STAGING REQUIRED are deferred to the single future governed staging deployment.
- **Unauthorized:** Bonus Engine Wave 4, and any AI implementation (ADR 0089 remains
  architecture only).

### Out of scope

- Real vendors, contracts, credentials, sandboxes; any AWS action or `deploy/` change.
- PAYWH-BRAND-1: triggered when a tenant holds per-brand merchant accounts at one provider.
- PAYWH-RL-1: pre-launch.
- PAYWH-TS-1: open; see above.
- LEDGER-MANUAL-ADJ-4EYES-1. It blocks real-money go-live and gets its own later stage.
- Casino items G-3 (LEDGER-REV-UNIQ), G-4 (the cross-domain provider-id namespace rule) and G-7
  (the stranded-exposure checklist).
- Free rounds, jackpots and bonus-funded casino stakes. The only W1c work here is a conformance
  rule that forbids mapping them to a win. *(Gate 10.3-W1: that rule is a documented requirement
  for the first real casino adapter. As a conformance case it is `NOT IMPLEMENTED`; see the
  ADR 0025 Stage 10.3 amendment, item 8.)*
- The hosted-KYC session token (KYC-HOSTED-SESSION-1) and the sanctions/PEP interface
  (KYC-SANCTIONS-IF-1).
- A four-eyes casino "settlement freeze". Security §5.2 rejected it (R9).
- Partner-console self-service writing of secrets, and per-tenant IAM or KMS (R13).
- Registered only, not built:
  - PROV-REVOKE-ALL-1, triggered when a second tenant shares a provider;
  - CAS-WIN-ANOMALY-1, required before real-money casino go-live.
- B2B, retail, cashout, AI.

### Carried open items (not decided here)

- ADR 0009 / AUP.
- HDR-J-6, HDR-J-7, HDR-J-8, HDR-J-9; sportsbook jurisdiction Rung 2 / HDR-J-7.
- HDR-M-1, HDR-M-2; HDR-SB-1; OB-1.
- Licensing, legal, vendor and retail decisions.
- LEDGER-MANUAL-ADJ-4EYES-1.
- O7: the regulatory period for retaining raw provider payloads.
- Everything in `00-roadmap-reconciliation.md` §5.

No evidence is invented for any of these.

Disclosures carried to the human (§19):
- ADR 0022 points 3 and 10 and §4.2 constrain which vendors can be selected.
- A B2B or bring-your-own-licence tenant may require per-tenant isolation of credential material.
- Recovering from a casino key compromise depends on LEDGER-MANUAL-ADJ-4EYES-1.

### Status labels (target at the completion gate; every item is `NOT IMPLEMENTED` at acceptance)

| Deliverable | Target label |
|---|---|
| WH-VENDOR-SCHEME-1 | `IMPLEMENTED` (platform contract, suite, MOCK schemes). Any real vendor scheme is `PROVIDER DEPENDENT` |
| MOCK-ADAPTER-PROD-1 | `IMPLEMENTED` |
| CAS-CAP-ROLLBACK-1, G-1 | `IMPLEMENTED — MOCK provider only` |
| KYC-REASON-BOUND-1 | `IMPLEMENTED` (bound and staff-only; players see status only) |
| PROV-CRED-RESOLVER-1 | `IMPLEMENTED` on the `memory`/`devfile` backends. The AWS path is `STAGING REQUIRED` |
| PROV-OUTBOUND-CRED-1 | `IMPLEMENTED` (exercised by MOCK adapters only) |
| KYC-PROVIDER-SELECT-1 | `IMPLEMENTED` |
| CAS-RECON-1 | `IMPLEMENTED` |
| CAS-RECON-STMT-1 | `MOCK`. A real statement source is `PROVIDER DEPENDENT` |
| SECRETSTORE-AWS-1 | `PARTIALLY IMPLEMENTED`: code plus local fake. IAM is `NOT IMPLEMENTED` (HD-10.3-2). Drills are `STAGING REQUIRED` |
| Real PSP / KYC / casino adapters | `NOT IMPLEMENTED` (`PROVIDER DEPENDENT`) |

### Status at gate 10.3-W1 (2026-09-26) — GATE 10.3-W1 PASSED

Gate record: `docs/plans/stage-10.3-planning/05-gate-log.md`, "GATE 10.3-W1 — PASSED". Reviews:
`docs/plans/stage-10.3-planning/06-gate-w1-review-*.md` (security, ledger-finance including its
"Re-verification after fix round A", identity-compliance, code). All fix-round conditions that bind
at this gate are met; the code landed in `1a6287e`, `8324aa0`, `f275298`, `5f98e23`, `a94e610`,
`8516951` and `98a7f08`.

| Deliverable | Label at gate 10.3-W1 |
|---|---|
| W1a WH-VENDOR-SCHEME-1 | `IMPLEMENTED` (platform contract, SC1–SC13 suite, MOCK schemes, registration-time restrictions). Real vendor schemes `PROVIDER DEPENDENT`. `KeyImplicit` resolution `NOT IMPLEMENTED` until W2a (refused at registration). Domain callback-fixture hook `NOT IMPLEMENTED`. `code-reviewer` "`hmac.Equal` only" checklist item `NOT IMPLEMENTED` (CR-CHECKLIST-HMAC-1, needs the human) |
| W1b MOCK-ADAPTER-PROD-1 | `IMPLEMENTED`. By design, an all-mock production binary (today's) refuses to start |
| W1c CAS-CAP-ROLLBACK-1, CAS-MULTIBET-WIN-1 (G-1) | `IMPLEMENTED — MOCK provider only`. `ledger-finance` C1–C6, C8, C10 met (re-verification), C11 met in `98a7f08`; C7 is `security`'s and its gate review accepted credential revocation as the emergency stop (§7, C14); C9 carried to W2b (CAS-RECON-1) |
| ADR 0082 A6 | `IMPLEMENTED` (C1, C6, C8 met) |
| W1d KYC-REASON-BOUND-1 | `IMPLEMENTED` (bound and staff-only; players see status only; platform-side normalization and `reason_truncated` audit flag; staff-UI escaping test) |

- **Unchanged / carried forward:**
  - Every real vendor scheme and every real PSP, KYC and casino adapter is `PROVIDER DEPENDENT`.
    The first real scheme type and adapter type must each implement `MarkProductionEligible()`
    or production startup fails (ADR 0085 §1 point 6; ADR 0022 §3).
  - The domain callback-fixture hook is `NOT IMPLEMENTED`. Any non-mock adapter fails the casino,
    payments and KYC conformance suites.
  - The free-round/jackpot conformance case is `NOT IMPLEMENTED`.
  - CAS-WIN-IDEMP-1 (F-9, Medium): `postWin` lacks a `postBet`-style already-posted
    short-circuit; must be fixed before bonus-funded/locked casino stakes (G-6) ship.
  - PAY-SB-REPLAY-AUDIT-1 (Low); CI-FLAKE-281 (W3).
- **Disclosed.** Every bundled component is a MOCK, so an `APP_ENV=production` binary refuses to
  start. This is the intended fail-closed result (ADR 0085, Stage 10.3 amendment).
- W2 (W2a design-reviewed: `07-w2a-design-review-security.md`, ADR 0093 amendment) is next.

## Consequences

Records:
- the proposal and papers in `docs/plans/stage-10.3-planning/` (evidence in `evidence/`);
- ADR 0093;
- the Stage 10.3 amendments in ADRs 0022, 0025, 0028, 0082 and 0085;
- the "Stage 10.3" section of the task registry;
- the completion report at `docs/governance/stage-10.3-completion-report.md`.

A production binary cannot start with any synthetic component wired (W1b). A disabled casino
capability no longer stops settlement; the casino emergency stop is revocation of the credential
(R9).
