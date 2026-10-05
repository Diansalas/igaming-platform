# Payment Readiness & Financial Hardening — Completion Report

Block: **PRH (Payment Readiness & Provider-Independent Hardening)**, including the human-authorized
**Financial Hardening / Double-Credit Fix** workstream (FH-1..FH-7).
Branch: `claude/focused-wright-jw88w9`. Date: 2026-09-28. Author: Master Orchestrator.
Final commit: see §9 (updated at the final push).

Everything here is **local / MOCK / synthetic**. No real payment, casino or KYC vendor is integrated or
selected. No production credential exists. Nothing was deployed; AWS is OFF and untouched. Software
capability is not regulatory or licensing approval (CLAUDE.md "Compliance").

Classification vocabulary: IMPLEMENTED · CLOSED · PARTIALLY IMPLEMENTED · BLOCKED · DEFERRED ·
HUMAN DECISION · STAGING REQUIRED · REAL PROVIDER REQUIRED. "IMPLEMENTED" always means against MOCK
adapters and local PostgreSQL unless stated otherwise.

---

## 1. Headline

| Item | Classification | Evidence |
|---|---|---|
| **PAY-DOUBLE-CREDIT-1** (one deposit intent credited twice via fallback + late success) | **CLOSED** (fix IMPLEMENTED) | §2 |
| **INV-DEP-1** (one intent → ≤1 successful attempt, ≤1 authoritative posting) | **IMPLEMENTED** (app choke point + DB backstop + reconciliation) | ADR 0095 §28 AM-2; migration 0107 |
| ADR 0095 amended (rev 4: §28 AM-2, §29 F-POOL-2 states, §30 AM-1, §10.9, §9.3 L-b, §32) | **IMPLEMENTED** (records) | `docs/decisions/0095-*.md` |
| LF-Q1 ("second capture posts") | **CLOSED — SUPERSEDED** by AM-2 for the multiple-success case only | `docs/plans/payment-readiness/lf-q1-supersession.md` |
| HD-LEDGER-UNALLOC-1 | **HUMAN DECISION — DECIDED** "A now, B later"; A IMPLEMENTED, B **DEFERRED** (LEDGER-SUSPENSE-B-1) | registry |
| F-POOL-2 (no provider I/O inside a financial DB transaction) | **PARTIALLY IMPLEMENTED** — payments IMPLEMENTED (MOCK); casino IMPLEMENTED; KYC submission outbox DEFERRED; legacy deposit path remains | §3 |
| Callback security round (PAY-SEC-S-H1, S-M1, FH-5) | **CLOSED** | §4 |
| Payout security round (FH-6) | **IMPLEMENTED**; launch conditions **DEFERRED** (PAY-SEC-LAUNCH-1) | §5 |
| A7 lock-order suite (ADR 0082) | **IMPLEMENTED** | §6 |
| Kill switch phase 2 | **IMPLEMENTED**; KS-AUDIT-TENANT-1 launch-blocking, NOT IMPLEMENTED | §7 |
| KYC enforcement (ADR 0096) | **PARTIALLY IMPLEMENTED** | §8.1 |
| Webhook rate limiting (ADR 0097) | **PARTIALLY IMPLEMENTED**; edge control **STAGING REQUIRED** | §8.2 |
| Provider-reference bound (PROVIDER-REF-BOUND-1) | **IMPLEMENTED** (platform side); deposit-response validation OPEN | §8.3 |
| Outbound credentials (PROV-OUTBOUND-CRED-1) | **PARTIALLY IMPLEMENTED** | §8.4 |
| Payment reconciliation (PRH-I5) | **IMPLEMENTED against a MOCK source**; real statements **REAL PROVIDER REQUIRED** | §8.5 |
| Casino launch two-phase (PRH-I2 casino) | **PARTIALLY IMPLEMENTED**; regression CAS-SESSION-EXPIRY-1 found in final review and **fixed**; C4 revoke gap DEFERRED (launch-blocking) | §8.6 |
| GitHub CI | **BLOCKED** (CI-BILLING-1, account billing) | §9 |

## 2. PAY-DOUBLE-CREDIT-1 — CLOSED

**Root cause.** ADR 0095 T13's "second capture posts" (LF-Q1) plus a T7 success path with no
intent-resolution check. A deposit intent that fell back from provider A to provider B could be
credited twice when A's success arrived late. Introduced by the F-POOL-2 design (never shipped
beyond local); reproduced by A7 test #1a. Full analysis:
`docs/plans/payment-readiness/double-credit-reconciliation.md`.

**Invariant INV-DEP-1** (ADR 0095 §28 AM-2): one logical deposit intent produces at most one
successful deposit attempt and at most one authoritative ledger posting. A verified provider success
never by itself authorizes another credit once the intent is financially resolved.

**Fix (IMPLEMENTED).**
- *Application choke point:* `postDepositSuccess`, wrapped by `postDepositSuccessOrDispute`, is the only
  deposit posting site. It is called from the callback (T7/T13/T13d), phase C including cascade
  children, and the sweeper poll including the T17 re-drive (architect-verified in code). Check order on
  the callback path: amount/currency mismatch → tombstone → INV-DEP-1 → post.
- *Second real success* (HD-LEDGER-UNALLOC-1 option A): the attempt goes to `disputed` with
  `terminal_reason='multiple_success_for_intent'`. There is no posting; the callback still gets a
  uniform 200, and the platform raises a P1 log alert, writes an audit row and an anomaly receipt.
- *Database backstop (migration 0107, fail-closed, reversible):*
  - partial unique indexes `payment_attempts_one_succeeded_deposit_per_intent` and
    `ledger_transactions_one_deposit_per_intent`;
  - pre-flight refusal blocks that row-level security cannot mask;
  - `payment_attempts_guard()` made NULL-safe (security F-M1);
  - errors `ErrDepositIntentAlreadyResolved` and `ledger.ErrDepositAlreadyPostedForIntent`.
- *Reconciliation:* standing, unwindowed kind `pay_captured_unposted`. It clears only on a PSP
  reversal or a tombstone, and only a `succeeded` line counts as captured. `pay_duplicate` stays as a
  detector.
- *Follow-up FH3-FOLLOWUP-1 (CLOSED):*
  - F3b: the sweeper decides from the fresh, under-lock state; its CAS conflicts dropped from about 13
    to 0 in the 25-round storm.
  - C4b: the audited refusal is tested end to end.
  - L2: A7 #5c requires `wait_event_type='Lock'`.

**Evidence.**
- QA A–O matrix: all 15 rows mapped to concrete, non-vacuous, passing tests. The race tests D and K
  loop 50 internal reps and were re-run with `-count=50`, 0 failures (`qa-fh7-final-gate.md`,
  `qa-fh3-adjudication.md`: 100/100 adjudication).
- Reviews:
  - ledger-finance APPROVED, then CONFIRMED (`rv-fh3-ledger.md`);
  - security APPROVE (`rv-fh3-security.md`);
  - payments APPROVE (`rv-fh3-payments.md`);
  - code-reviewer READY WITH CONDITIONS, conditions closed (`rv-fh3-code-review.md`);
  - architect final APPROVE WITH CONDITIONS (`rv-fh7-architect-final.md`).
- Mutation kills (INV-DEP-1 mutants PRE, RECK, BOTH, IDK, AID, GNULL, C4W and others):
  `evidence/prh-i1-mutation-kill.txt`.

**Residuals (registered, none reopens the invariant):**

| Residual | Severity | Note |
|---|---|---|
| PAY-POLL-AMOUNT-1 | Low | Poll path does not cross-check amount/reference; before the first real PSP |
| PAY-F3SM-TEST-1 | Low | — |
| PAY-SWEEP-CAS-NOISE-1 | Low | — |
| PAY-RECON-N1 | — | Statement-only refund → repeated false positive |
| PAY-P1-MULTISUCCESS-ALERT-1 | launch-blocking | Paging delivery |
| DEVOPS-0107-INDEX-WINDOW-1 | — | Non-concurrent index build; before any production-size ledger |
| PAY-PSP-CONTRACT-INVDEP1 | REAL PROVIDER REQUIRED | Vendor-selection criteria |

## 3. F-POOL-2 — PARTIALLY IMPLEMENTED

Durable states are specified in ADR 0095 §29: three phases A/B/C, and no external call inside a
financial transaction.

| Area | Status |
|---|---|
| Payments (deposit v2, cascade, sweeper, callback, payout) | **IMPLEMENTED** (MOCK). The gate refuses provider calls with a transaction held (`txscope`). Architect-verified in code. |
| Casino launch | **IMPLEMENTED** (PRH-I2, two-phase). See §8.6 for the expiry regression (fixed) and the deferred C4 gap. |
| KYC submission | Phase A/B/C split IMPLEMENTED. KYC-SUBMIT-OUTBOX-1 **DEFERRED** (accepted), required before a real KYC adapter. |
| Legacy `InitiateDeposit` (test-only caller, provider call inside a transaction) | **OPEN**: PROV-OUTBOUND-CRED-1-LEGACY-PATH (architect FH7-04). Delete before the first real PSP. |
| Deposit-response reference validation | **OPEN**: PAY-DEP-REF-VALIDATE-1 (FH7-05, Medium latent). Before the first real PSP. |

## 4. Callback security — CLOSED

| Finding | Status | Fix and evidence |
|---|---|---|
| PAY-SEC-S-H1 (High) | **CLOSED** | The deferred-receipt replay after a payout receipt skipped the event_type/operation check. The filter is now derived from `attempt.Operation`; mutation-killed. |
| PAY-SEC-S-M1 (Medium) | **CLOSED** | A payout success with a different provider reference now goes to disputed (`TestRVLF_SM10_PayoutSuccessProviderReferenceMismatchDisputes`). |
| FH-5 C2/C3 | **CLOSED** | ledger-finance confirmation 2026-09-28: DFR, DFS and SIBS killed. Low L-f fixed in `62352b0`: the SIBS test now asserts `query_status` and `intent_succeeded`, which kills SIBSE. |
| TEST-T11A-FLIP-1 | **CLOSED** | A test tampered with the `v1=` prefix instead of the signature about 1.6% of the time. Fixed in six tests (`498559a`); T11a passed 100/100 and 200/200 under `-race`. |

Records: `rv-prh-i1-callback-ledger.md`, `rv-prh-i1-callback-code-review.md`,
`rv-prh-i1-payout-security.md` re-verification 1.

## 5. Payout security — IMPLEMENTED; launch conditions DEFERRED

- **Security round FH-6:** S-M2, S-L2/SP-C, PC3, CR-1 and TESTS-1 are FIXED.
- **Review verdicts:** security APPROVED WITH CONDITIONS, ledger-finance approved, code-review closed.
- **Launch conditions: DEFERRED, recorded in PAY-SEC-LAUNCH-1** and
  `prh-i1-payout-launch-conditions.md`. They must be in place before real-money payouts:
  - S-L1: finance staff linked to the player's own Person.
  - S-L3: a `/resolve` throttle.
  - S-L4: a staff eligibility check inside T1p.
  - Payout destination binding. This is PROVIDER DEPENDENT and needs architect/human sign-off.
- **CP-W1 is launch-blocking (newly registered).** No binary constructs a payments/payout `Sweeper`, so
  T2/T12, crash recovery, escalation and polling do not run in any deployment.
- **HD-0095-1 is a HUMAN DECISION:** who may force-resolve an unresolvable payout or a disputed
  attempt (manual transitions M1/M2), and above what threshold. M1/M2 stay BLOCKED until decided.

## 6. A7 lock-order suite — IMPLEMENTED

- ADR 0082 A7 has all eight §(7) tests: #1a, #1b, #2, #3, #4, #5a, #5b and #5c. The #5c runtime
  waiter check requires `wait_event_type='Lock'`.
- The named mutants are killed.
- ledger-finance closed it in `rv-a7-tests.md` and `rv-fh3-ledger.md`.
- A7-5C-STATIC-1 is SUPERSEDED by the runtime method.

## 7. Kill switch — IMPLEMENTED (phase 1 + 2)

- **Phase 1 (migration 0105):** data model, four-eyes release, audit.
- **Phase 2 (migration 0106, GUC RLS hardening):** payments kind-split outbound resolver, pool
  threading, AM-1 (authenticated-tenant scope), and KS-DEP-T2-T3-1 (**CLOSED**: a fallback-scoped
  switch now gives a clean decline).
- **Reviews:** security, architect and code-review. The fix-round code re-review on 2026-09-28 was
  **READY**; QA verified.
- **Open items:**

| Item | Status |
|---|---|
| **KS-AUDIT-TENANT-1** | **NOT IMPLEMENTED, launch-blocking.** Platform-scoped switch actions are audited with `tenant_id NULL`, so tenant-scoped audit reads never see them. Before production or the first B2B tenant. |
| KS-CAS-DISCRIM-TEST-1 | Low test gap |
| PROV-OUTBOUND-CRED-1-LEGACY-PATH | Open |

## 8. Other PRH workstreams

### 8.1 KYC enforcement (ADR 0096, PRH-I3, KYC-ENFORCE-1) — PARTIALLY IMPLEMENTED

**Implemented:**
- The withdrawal-request gate.
- Casino and sportsbook play gates, with call-site tests on a licensed tenant.
- Deposit gates at `deposit_v2.go:202` and `drive.go:100`.
- Payout T1p deny via `DenyForCompliance`, and the T2/T12 re-claim gate.
- Migrations 0100 and 0103 (supersession enforced by the database).

The FH-7 code re-review verdict is READY WITH CONDITIONS for this label and NOT READY for IMPLEMENTED.

**Open:**

| Item | Severity | Note |
|---|---|---|
| KYC-ENF-OUTAGE-1 | Medium, fail-closed | A real DB outage gives a 500 with no decision row |
| KYC-ENF-DECISION-ROWS-1 | Medium | Deposit/payout evaluations write no decision rows |
| KYC-ENF-TESTPINS-1 | Low | — |
| B7, F4/F5, LF-I3-4/5 | — | — |
| Thresholds | **HUMAN DECISION** | Dormant pending HDR/legal review (HD-KYC-1..8) |
| Real KYC vendor | **REAL PROVIDER REQUIRED** | KYC-SUBMIT-OUTBOX-1 first |

### 8.2 Webhook rate limiting (ADR 0097, PRH-I4) — PARTIALLY IMPLEMENTED

- Per-tenant admission control is IMPLEMENTED. Security re-verification #3 gave a plain APPROVE.
- **Open items:**

| Item | Status |
|---|---|
| PRH-I4-METRICS-1 (OTel metrics) | NOT IMPLEMENTED |
| **WEBHOOK-EDGE-1** (flooding one tenant's URL delays that tenant's callbacks) | Medium, **STAGING REQUIRED** (edge WAF/CDN rule) |

### 8.3 Provider-reference bound (PROVIDER-REF-BOUND-1, PRH-REF) — IMPLEMENTED (platform side)

- Platform maximum, boundary validation, migration 0099 with pre-flight, and no truncation.
- Security AGREED, APPROVE WITH CONDITIONS.
- **Open items:**

| Item | Severity | Note |
|---|---|---|
| PAY-DEP-REF-VALIDATE-1 | Medium latent | Condition C1 is only half done for deposit responses |
| PRH-REF-F1 | Latent | — |
| PRH-REF-F4 | — | **FIXED** |

### 8.4 Outbound credentials (PROV-OUTBOUND-CRED-1) — PARTIALLY IMPLEMENTED

- **Implemented:**
  - per-call resolver (own transaction), authenticator and derived-token cache;
  - the static API key removed;
  - kind-split resolvers for payments, casino and KYC.
- **Launch-blocking precondition (enforced by tripwire test
  `TestOutboundPrecondition_EveryWiredAdapterIsSynthetic`):** no non-synthetic adapter may be
  registered until every outbound call runs outside a domain transaction.
- The legacy deposit path is still open (§3).
- **M5 (raw-string secrets):** open. Reviewed per real adapter by `security` (kill-switch security
  review, launch-blocking list).

### 8.5 Reconciliation (PRH-I5) — IMPLEMENTED against a MOCK source

- **Implemented:**
  - the payment statement stream (migrations 0102/0104);
  - the legacy-posting exclusion;
  - `pay_captured_unposted`.
- **FH-7 code re-review: READY.** F1–F5 closed, 51/51 mutants killed.
- **Open items:**

| Item | Status |
|---|---|
| Real PSP statement matching | **REAL PROVIDER REQUIRED** |
| LF95-R1 automatic re-drive | DEFERRED |
| PAY-RECON-N1 | Open |
| RECON-PAYOUT-LIVE-TEST-1 | Low test gap |

### 8.6 Casino launch two-phase (PRH-I2 casino) — PARTIALLY IMPLEMENTED

**Implemented:**
- `LaunchGame` phases A/B/C;
- a context-independent, bounded phase C;
- redaction;
- the kind-split resolver.

**Review verdicts:**
- code-reviewer re-review 2: **READY WITH CONDITIONS**;
- security: fix **ACCEPTED** (`rv-prh-i2-casino-security.md`, FH-7).

**CAS-SESSION-EXPIRY-1 (HIGH regression): fixed in `80eda28`.**
- *Cause:* the PRH-I2 rework `2c00e10` applied the 2-minute launch-token TTL to consumed, in-play
  sessions.
- *Fix:* the expiry check now applies to never-consumed sessions only. The mutants M-CAS1–3,
  M-REAPPLY and M-DROP are all killed.
- *How it was found:* the FH-7 code re-review.
- *Follow-ups:* the stale comments (C2) and security's test findings (N-A, a vacuous JSONB assertion,
  now mutant-proven; N-B, a flake risk) are fixed.

**Open items:**
- **CAS-REVOKE-CONSUMED-1 (security C4 reopened): MEDIUM, DEFERRED, launch-blocking.**
  - *Why it is open:* a failed launch cannot revoke a session the vendor has already consumed, because
    migration 0036/0042's immutability trigger forbids leaving `consumed`.
  - *Reachability:* not reachable today. No code consumes a token, only MOCK adapters exist, and the
    tripwire holds.
  - *Required fix:* a migration relaxing the trigger to exactly `consumed → revoked` (spec and tests
    in the security record).
  - *Deadline:* before the first of: a vendor token-bootstrap caller, a non-synthetic casino adapter,
    or a production launch request.
- **CAS-PLAY-BOOTSTRAP-1: functional limitation (MOCK).**
  - *Behaviour:* because no token-bootstrap path exists, every launched session stays `active`, and
    the B2C MOCK play route refuses new bets about 2 minutes after launch. The player must relaunch;
    earlier bets still settle.
  - *Security position:* this is correct and must not be relaxed. The fix is the vendor bootstrap
    endpoint, which is next-stage design.
  - *Behaviour change:* Stage 7's MOCK play simulation previously had no time limit.
- **Low items:** CAS-REVOKE-BET-RACE-1 (unchanged, open) and MF3 (optional).

## 9. CI and verification

- **GitHub Actions: BLOCKED (CI-BILLING-1).** Every run since #365 fails in about 3 s without
  starting ("recent account payments have failed or your spending limit needs to be increased"). The
  latest example is run #517 on `3a38930`. This is a human action on the GitHub account. No CI
  evidence exists for this block. Local replays are recorded as substitutes, **never as CI**. Final gate
  item O is BLOCKED, not PASS.
- **F-POOL-1 K1** (first green CI run of the timing lane) and the GitHub half of TEST-RESISO-RACE-1
  both wait on CI-BILLING-1.
- **Local verification on the final code** (QA `qa-fh7-final-gate.md`, orchestrator runs):
  - build, vet (plain and integration), gofmt, and golangci-lint 2.9.0 with 0 issues;
  - `migrate verify` clean through 0107; 0107 up/down/up passes;
  - full `-race -tags integration ./...` with the timing lane skipped: every package ok, 0 FAIL,
    0 DATA RACE.
- **Timing lane on an idle machine: 40/40.** That is 8 tests × 5 rounds, exactly CI's commands,
  thresholds unchanged (`evidence/fh7-timing-lane-idle.txt`).
- **Final full re-verification on the final code** (`20d3ce0`, after the casino fix), run by the orchestrator:
  - build, vet (plain and integration) and gofmt clean; lint 0 issues;
  - `migrate verify` clean through 0107;
  - `-race -tags integration -count=1 -p 1 ./...` with the timing lane skipped: **45 packages ok**, 0 FAIL, 0 panic, 0 DATA RACE.

## 10. Governance

- **Incident** `docs/governance/incident-2026-09-27-local-db-credential-mutation.md`: RESOLVED.
  - Two sub-agents changed local Postgres role passwords via sudo.
  - Recovery was human-authorized; no credential was committed.
  - The permanent rule is in CLAUDE.md § Environment safety and was repeated in every DB-touching
    delegation this block. No further violation occurred.
- **Housekeeping:** 210 leaked per-test scratch databases were dropped with ordinary
  `DROP DATABASE … WITH (FORCE)` and no role changes. TEST-SCRATCH-LEAK-1 (the helper fix) is open.
- **Process lesson (TEST-T11A-FLIP-1):** the orchestrator once pushed a merge whose verification pipeline
  masked a FAIL, because it ran without `pipefail`. All verification now uses `pipefail` plus a grep
  for FAIL.
- **Scope check** (product-owner-proxy `rv-fh7-product-owner-proxy.md`, with a git addendum):
  - No Terraform, `deploy/aws`, Bonus Wave 4, AI-agent or vendor code.
  - `ci.yml` only gained must-PASS test names.
  - `deploy/init-app-role.sql` only gained least-privilege grants for new tables.

## 11. Remaining launch blockers, human decisions, staging and real-provider dependencies

> **SUPERSEDED as of 2026-10-05.** This section is a 2026-09-28 snapshot; several items listed below are now closed or implemented against MOCK and newer blockers are missing. The current list is in `docs/HANDOVER.md` "Launch blockers (current)" and "Human decisions awaiting the owner"; the registry row `REGISTRY-HYGIENE-2026-10-05` records the evidence. The text below is kept unchanged as history.

**Launch-blocking (NOT IMPLEMENTED or OPEN):**

| Item | What is missing |
|---|---|
| CAS-REVOKE-CONSUMED-1 | Before a vendor bootstrap caller, a real casino adapter, or a launch request |
| CAS-PLAY-BOOTSTRAP-1 | Vendor token-bootstrap path (MOCK play window is 2 minutes) |
| KS-AUDIT-TENANT-1 | — |
| Alert delivery, incl. PAY-P1-MULTISUCCESS-ALERT-1 | Paging |
| CP-W1 | No sweeper wired |
| PAY-SEC-LAUNCH-1 | S-L1, S-L3, S-L4, destination binding |
| PROV-OUTBOUND-CRED-1 precondition + PROV-OUTBOUND-CRED-1-LEGACY-PATH | — |
| M5 | Raw-string secrets, reviewed per real adapter |
| KYC-ENFORCE-1 | Remaining items in §8.1 |
| KYC-SUBMIT-OUTBOX-1 | Before a real KYC vendor |
| DEVOPS-0107-INDEX-WINDOW-1 | Before a production-size ledger |
| PAY-DEP-REF-VALIDATE-1, PAY-POLL-AMOUNT-1 | Before the first real PSP |
| LEDGER-MANUAL-ADJ-4EYES-1 | Blocks real-money go-live |
| F-POOL-1 K1 and CI-BILLING-1 | — |
| BRANCH-PROTECTION-1 | Existing |

**HUMAN DECISION:**

| Decision | Status |
|---|---|
| HD-LEDGER-UNALLOC-1 | **decided** (A now, B later) |
| HD-0095-1 | Who may force-resolve payouts or disputed attempts (M1/M2), and the threshold |
| LEDGER-MANUAL-ADJ-4EYES-1 threshold | Recorded as BLOCKED on the human |
| HD-PRH-1 | Are tenant slugs confidential? (ADR 0097 R2, Low) |
| HD-KYC-1..8 | KYC thresholds, legal review |
| CI-BILLING-1 | Account action |
| Earlier HDRs | Not re-asked |
| Vendor selection, contracts, production credentials, licence or jurisdiction scope beyond Anjouan, production launch | Always the human's |

**STAGING REQUIRED:**
- WEBHOOK-EDGE-1: the edge rate rule.
- The timing lane on real CI runners: F-POOL-1 K1 and TEST-RESISO-RACE-1.
- DEVOPS-0107-INDEX-WINDOW-1: an index-build window rehearsal.
- End-to-end callback delivery through a real ingress.

**REAL PROVIDER REQUIRED:**
- PAY-PSP-CONTRACT-INVDEP1: the vendor must meet the INV-DEP-1 contract criteria, including the same
  reference across sync, callback and poll.
- Real PSP statements (PRH-I5).
- Payout destination binding.
- Real KYC and casino adapters, each behind the PROV-OUTBOUND-CRED-1 precondition.
- A crypto custodian (ADR 0008).

## 12. Final state

- **Final code commit:** `20d3ce0`. The branch head carries this report, which is docs only on top of it.
  HEAD == `origin/claude/focused-wright-jw88w9`, and the working tree is clean at the final push.
- **Scratch databases:** 247 leaked per-test scratch DBs were dropped across the block, with ordinary
  `DROP DATABASE … WITH (FORCE)` and no role or credential changes.
- **Final gate A–R:**

| Item | Result | Note |
|---|---|---|
| A | **PASS** | |
| B | **PASS** | |
| C | **PASS** | |
| D | **PASS** | Payments IMPLEMENTED (MOCK); remaining F-POOL-2 items registered, §3 |
| E | **PASS** | |
| F | **PASS** | |
| G | **PASS** | |
| H | **PASS** | Launch conditions deferred and recorded |
| I | **PASS** | |
| J | **PASS** | |
| K | **PASS** | |
| L | **PASS** | |
| M | **PASS** | |
| N | **PASS** | Every review record exists with a final verdict; open conditions are registered |
| **O** | **BLOCKED** | CI-BILLING-1 |
| P | **PASS** | |
| Q | **PASS** | |
| R | **PASS** | |

- **Stop:** this is the final human gate. No real provider integration was started and nothing was
  deployed; the next step needs explicit human authorization.
