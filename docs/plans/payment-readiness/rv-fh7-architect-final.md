# RV-FH7 — Final architecture review of the Financial Hardening / Payment Readiness block

- **Reviewer:** `architect`.
- **Date:** 2026-09-28.
- **Subject:** branch `claude/focused-wright-jw88w9` at `7f5d0fb`, which contains
  `498559a` (TEST-T11A-FLIP-1) and its successor docs commit. Workstreams FH-1 to FH-6 are all
  merged on it. I checked this by commit ancestry for `8ce538c`, `cb03868`, `92f5889`,
  `5ee09e4`, `4e04f4e`, `2da7548`, `b7f84ec`, `0a96a01`, `109ef04`, `f43025c`, `6fce7a6`,
  `1f76bba` and `201be61`.
- **Method:**
  - I read the code at HEAD and checked it against ADR 0095 §28/§29/§30/§10.9, ADR 0082 A7,
    migrations 0106/0107 and the review records. I did not take the documents' own claims on
    trust.
  - `go build ./...` passes. `go vet -tags=integration ./internal/payments/
    ./internal/reconciliation/ ./internal/ledger/` is clean.
  - **No database tests were run** (per the dispatch: another process is running
    timing-sensitive tests). Test and mutation results below are therefore cited from the named
    review records, not re-executed by me.
- **Environment:** no DB access, no role, password or credential change, no `sudo`, no
  AWS/Terraform.

## Verdict: **APPROVE WITH CONDITIONS**

The double-credit fix is architecturally sound:
- one choke point;
- two independent DB backstops;
- a NULL-safe guard;
- a standing reconciliation report;
- no provider I/O inside any authoritative financial transaction on the live paths;
- an A7-conformant lock order.

I found no money-path defect. The conditions below are:
- latent PROVIDER DEPENDENT gaps that must close before the first real PSP;
- two missing review records;
- one governance synchronization item.

None of them blocks marking PAY-DOUBLE-CREDIT-1 closed. Several do block PRH-GATE, and these are
marked.

## 1. Verification against code

### (a) INV-DEP-1 single choke point

- **One ledger deposit posting site.** `ledger.TxDeposit` is posted only in
  `postDepositSuccess` (`internal/payments/orchestrator.go:1162`). A grep of non-test code finds
  no other `TransactionType: ledger.TxDeposit`.
- **The wrapper and its three callers.** `postDepositSuccessOrDispute` (`orchestrator.go:1049`)
  is called from:
  - `receipt.go:993`, which is `applyDepositSuccessAndPost` and serves callback T7 (`:830`) and
    T13/T13d (`:793`);
  - `drive.go:354`, phase C sync success, which also serves cascade children driven by
    `driveCreatedAttempt`;
  - `sweeper.go:513`, poll success, which also serves T17 re-drive.
- **The only direct caller of `postDepositSuccess`.** It is the legacy `resolveAmbiguous`
  (`orchestrator.go:825`). That path has a test-only caller, and it still hits the internal
  re-check (rule 2) and the ledger index (rule 3).
- **Cascade never posts.** `insertCascadeAttemptIfEligible` inserts a `created` row only.
- **Check order.**
  - Callback path: mismatch (`receipt.go:744`, then `:763`/`:816`) → tombstone
    (`tombstoneExists`, `:786`/`:819`) → `resolvedForOtherDeposit` (wrapper call, `orchestrator.go:1054`) →
    `postDepositSuccess`, which runs its own intent amount/asset compare and the rule-2
    re-check (`:1107`, `:1125`) → `ledger.Post`. This is the documented order.
  - Phase C and sweeper paths: tombstone → INV-DEP-1 → post. The "mismatch" step is missing:
    - on the sync path it is empty by contract, because `DepositResult` carries no amount;
    - on the poll path it is absent: the poll posts `attempt.Amount`, not `res.Amount`. This is
      already registered as PAY-POLL-AMOUNT-1. See FH7-06 for a related reference gap.
- **The predicate.** `resolvedForOtherDeposit` (`orchestrator.go:945`) matches §28.2 exactly:
  - it looks for another `succeeded` deposit attempt of the intent (`id IS DISTINCT FROM $3`,
    which is NULL-safe on the legacy path);
  - or a deposit posting with `correlation_id = intent` under a different idempotency key.

  An exact redelivery keeps its replay semantics.
- **Dispute routing.** T10 (live attempt) or T13d (declined attempt), with no posting. The audit
  and the ids-only P1 log are written in the same transaction. A backstop firing adds a second
  P1, `payments_deposit_intent_index_backstop_fired`.
- **Deleted branch.** The dead `deposit.second_capture_posted` branch is gone.

### (b) Migrations 0107 and 0106

- **0107** (`migrations/0107_deposit_intent_double_credit_backstop.up.sql`) contains:
  - `payment_attempts_one_succeeded_deposit_per_intent` on `(tenant_id, deposit_intent_id)
    WHERE operation='deposit' AND state='succeeded'`;
  - `ledger_transactions_one_deposit_per_intent` on `(tenant_id, correlation_id) WHERE
    transaction_type='deposit'`.

  Each index build is its own check inside `DO … EXCEPTION WHEN unique_violation`. That is
  RLS-proof, with no bypass and no pre-check fooled by `FORCE RLS`.
- **The guard.** The T13t/T13d line reads `NEW.terminal_reason IS NULL OR NEW.terminal_reason
  NOT IN (…)`, which is NULL-safe (security F-M1, closed). The ledger's
  `ErrDepositAlreadyPostedForIntent` sentinel maps the index violation, and the payments layer
  maps it to the same dispute (`orchestrator.go:1169`).
- **The CHECK.** `pay_captured_unposted` is added to the mismatch-kind CHECK. The non-concurrent
  index build is tracked as DEVOPS-0107-INDEX-WINDOW-1.
- **0106.** `tenant_staff_scope` on `payment_attempts`/`payment_provider_events` now also
  requires `app.platform_admin_principal_id` unset, which closes the mixed-GUC fail-open. The
  release-request guard forces `expected_version` and requires an engaged switch.

### (c) F-POOL-2 (§29): no provider call inside a financial DB transaction

| Path | Phase A (tx) | Phase B (no tx) | Phase C (tx) |
|---|---|---|---|
| Deposit, player (`deposit_v2.go`) | `:121` `WithTenant` | `:288` `callProvider` | `:300` `WithTenant` |
| Deposit, cascade/sweeper-driven (`drive.go`) | `:76` `WithTenant` (gates, L1, T2 claim) | `:202` `callProvider` | `:210` `WithTenant` + re-read under lock |
| Sweeper poll (`sweeper.go`) | `:332` `WithTenant` | `:353` `callProvider(QueryStatus)` | `:370` `WithTenant` |
| Payout (`withdrawal_handlers.go:910/958/963`, `payout_sweep.go:382-383`) | `ClaimForDispatch` | `DispatchWithdraw` → `callProvider` | `ApplyPayoutResult` |
| Payout poll (`payout.go:1240-1314`) | lookups in their own txs | `:1313` `callProvider` | `applyPayoutStatusEvidence` |
| Callback (`receipt.go`) | — | — | a single domain tx with no provider call; the cascade inserts a `created` row only (LF-C2 #6) |

- `callProvider` refuses under `txscope.Held` (`gate.go:122`), so a regression fails closed.
- The only remaining violation is the legacy `Orchestrator.InitiateDeposit` →
  `attemptDeposit`/`resolveAmbiguous` (`orchestrator.go:555`, `:714`, `:797`). It calls
  `provider.Deposit`/`QueryStatus` inside the caller's tx and has no non-test caller. It is
  already tracked as PROV-OUTBOUND-CRED-1-LEGACY-PATH (FH7-04).

### (d) Kill-switch phase 2

- **AM-1.** `runKillSwitchTx` scopes the principal session to `c.tc.TenantID` (the
  authenticated tenant), never the path value (`payments_kill_switch_handlers.go:210-224`).
- **Tenant-scoped switch rows.** `KillSwitchEngaged` (`killswitch.go:294`) matches `tenant_id =
  $1` with wildcard provider and operation scope.
- **KS-DEP-T2-T3-1** (`drive.go:134-170`), which is my own §10.9.3 item:
  - only `ErrAttemptStateConflict` is reclassified;
  - `KillSwitchEngaged` is a read-only label, and a genuine CAS conflict still errors;
  - then the audit `payments.cascade_rejected_kill_switch`, then `RejectCreated(…,
    'kill_switch')`, then `finalizeDeclined`, whose sticky guard means a `succeeded` intent is
    never regressed.

  This is exactly the required fix. `qa` verified it with a killed mutant
  (`qa-killswitch-phase2-verification.md` §2).
- **Kill switch versus evidence.** A kill switch never turns evidence into a 5xx
  (`receipt.go:952`, `cascade.go:71-90`).

### (e) Lock order (ADR 0082 A7)

- **R0 before L1.** R0 (`insertReceiptDeduped`) precedes the parent `FOR UPDATE` on:
  - the main receipt path (`receipt.go:611` → `:625`/`:629`);
  - the reversal tombstone branch (`:1154` → `:1172`).
- **Parent before attempt.** Deposit phase C and the sweeper take `deposit_intents FOR UPDATE`
  and then re-read the attempt under it (`drive.go:211`, `sweeper.go:371`, `deposit_v2.go:302`).
- **Payout order.** Payout paths take `withdrawal.LockForPayoutEvidence` before the attempt
  re-read (`payout.go:487/598/672/901/933/1282`).
- **Gates before L1.** RG/KYC gates run before L1 (`drive.go:79-116`, rule N1).
- **The new index adds no lock cycle.** This is `ledger-finance`'s ruling, and it is consistent
  with what I read: the only contender for the same `(tenant, correlation_id)` key already holds
  the same L1.
- **Test suite.** The A7 §(7) suite is present: `TestA7_1a`, `1b`, `3`, `4_N1`, `5a`, `5b` and
  `5c`, plus the #2 receipt-key tests. #5c asserts `wait_event_type='Lock'`.

### (f) `pay_captured_unposted` standing check

`internal/reconciliation/payment_statement.go:1000` emits it for every `disputed` /
`multiple_success_for_intent` attempt that `capturedUnposted` (`:977`) still considers open.
"Open" means no reversal line in the run and no ledger tombstone. The check is deliberately
unwindowed, and the matched-line case is at `:948`.

It is standing per provider statement run. A provider whose statement feed stops produces no run.
The durable backstops for that case are:
- the attempt row itself, `disputed` (§29.2 R1);
- the P1 log.

See FH7-09.

## 2. Findings

| ID | Severity | Location | Finding | Recommendation / disposition |
|---|---|---|---|---|
| FH7-01 | Low (docs) | `docs/decisions/0095-…md` header, §28 status, §10.9.3/§10.9 labels | The status still read "§28 NOT IMPLEMENTED", "KS-DEP-T2-T3-1 NOT IMPLEMENTED", callback reviews "NOT READY" and "§28.9 kind NOT IMPLEMENTED". | **Fixed in this change** with dated status notes. No text was deleted. |
| FH7-02 | Low (docs) | ADR 0095 §9.3 | L-b: §9.3 did not state that a verified success is evidence rather than authorization to post (§28 AM-2), and it did not carry `ledger-finance` H1 rule 5 (reversal `Outcome` semantics) or rule 3 (raw-outcome fingerprint). | **Fixed in this change** with a dated amendment note. The original text is kept. |
| FH7-03 | Low (docs) | ADR 0082 A7 status; ADR 0094, 0096, 0097 headers; `double-credit-reconciliation.md` header | These were stale against the review records: the A7 L2 residual is closed; F-POOL-1 is CLOSED WITH CONDITIONS; 0096's real label is PARTIALLY IMPLEMENTED; 0097's security review is done; the design doc still said "no fix implemented". | **Fixed in this change** with dated notes. |
| FH7-04 | Medium (latent; test-only caller) | `internal/payments/orchestrator.go:555-835` (`InitiateDeposit` → `attemptDeposit` `:714`, `resolveAmbiguous` `:797`) | Provider calls inside the caller's tx (an F-POOL-2 violation), with no gate, no credential resolution and no kill-switch predicate. INV-DEP-1 still holds through rule 2 and the ledger index. | **Deferred (tracked):** PROV-OUTBOUND-CRED-1-LEGACY-PATH. Delete the path, or move it behind a test build tag, before any real payments adapter. Required before F-POOL-2 can close. |
| FH7-05 | Medium (latent; PROVIDER DEPENDENT) | `internal/payments/drive.go:249-270` (`depositAdapterCall`), `sweeper.go:353-365`; compare `payout.go:77-85/393/1179` | PROVIDER-REF-BOUND-1 security C1 requires `providerref.Validate` on every adapter-response reference. Payout does this (`ErrorClassProviderRefInvalid` → park). The deposit `Deposit`/`QueryStatus` responses do not. An over-bound reference hits the 0099 CHECK in phase C, the tx rolls back, and the attempt loops or escalates instead of parking deterministically. It is money-safe: nothing is posted and INV-DEP-1 holds. But a real capture could sit unparked. | **Escalated to the orchestrator for registration** (suggested id PAY-DEP-REF-VALIDATE-1; owner `payments`, reviewer `security`). Before the first real PSP. Not registered by me. |
| FH7-06 | Low (latent; PROVIDER DEPENDENT) | `internal/payments/sweeper.go:513-521` | A poll success for a live deposit attempt is not cross-checked against the attempt's **stored** `provider_reference`. The posting uses `res.ProviderReference` as `provider_tx_id`, while `ApplySuccess` keeps the stored reference (`COALESCE`). The result is one posting (INV-DEP-1 holds) that is mislinked. Reconciliation `pay_status_mismatch` would flag it. The payout path disputes this case (N6/S-M1); the deposit poll path does not. | **Deferred:** fold into PAY-POLL-AMOUNT-1 (amount **and** reference cross-check on the poll path → T10 dispute). It is also a PAY-PSP-CONTRACT-INVDEP1 criterion ("same reference across sync/callback/poll"). |
| FH7-07 | Low (process) | `rv-prh-i1-killswitch-phase2-code-review.md` | No `code-reviewer` record exists for the kill-switch phase-2 fix round (`ce77bac`..`4e04f4e`; my §10.9 approval noted it as pending) or for the KS-DEP-T2-T3-1 delta `2da7548`. `qa` verified both with mutants, and I verified `2da7548` by reading. | **Escalated:** a short `code-reviewer` confirmation before PRH-GATE. It does not affect KS-DEP-T2-T3-1 closure. |
| FH7-08 | Low (process) | `rv-prh-i2-casino-code-review.md`, `rv-prh-i3-code-review.md`, `rv-prh-i5-code-review.md` | Each has a NOT READY verdict with no re-review record under `docs/plans/payment-readiness/`. The fixes are recorded in ADR sections, but the reviewer's own confirmation is missing. This is outside FH scope but blocks PRH-REV/PRH-GATE (gate item N). | **Escalated** to the orchestrator: schedule the three `code-reviewer` re-reviews. |
| FH7-09 | Info | `internal/reconciliation/payment_statement.go:1000` | `pay_captured_unposted` is emitted per provider statement run, so it depends on the statement feed. | No change. PAY-P1-MULTISUCCESS-ALERT-1 (paging) and the §29.2 R1 read-only view (RECOMMENDATION) are the feed-independent surfaces. Keep PAY-P1-MULTISUCCESS-ALERT-1 prioritized. |
| FH7-10 | Medium (governance) | `docs/active-stage.md`, `docs/progress.md` | Neither mentions the PRH or Financial Hardening block. `active-stage.md` still reads "Stage 10.3 ACCEPTED … awaiting human authorization". The KYC (PRH-I2/I3), rate-limit (PRH-I4) and provider-reference (PRH-REF) work lives only in the registry and in ADRs. This is gate item K. | **Escalated** to the orchestrator (PRH-0 owner). Update both status docs at PRH-GATE. Not edited by me, because the orchestrator owns stage docs. |
| FH7-11 | Low (governance) | `docs/governance/task-registry.md` | CP-W1 (no binary constructs a payments or payout `Sweeper`; I verified that `NewSweeper` has no non-test caller) is a named launch condition in `prh-i1-payout-launch-conditions.md` but has **no registry row**. | **Escalated:** register CP-W1 as its own row (launch-blocking). |
| FH7-12 | Info | `internal/httpserver/deposit_handlers.go:94,102` | The comments say the handler "drives PaymentOrchestrator.InitiateDeposit". It calls `InitiateDepositAttempt` (`:176`). | **Deferred** to `payments` (cosmetic, together with FH7-04's deletion). |
| FH7-13 | Low (process) | `rv-prh-i1-callback-ledger.md` re-review 2, conditions C2/C3 | The implementer's evidence (`f43025c`; `prh-i1-mutation-kill.txt`: DFR, DFS and SIBS killed) and `code-reviewer`'s C3 closure exist. No `ledger-finance` line explicitly confirms its own FH-5 C2/C3 closure. `rv-fh3-ledger.md` only confirms H1R3/C1. | **Escalated** (non-blocking): a one-line `ledger-finance` confirmation at PRH-GATE. |

**No finding requires reopening a human decision.** HD-LEDGER-UNALLOC-1 is decided and
implemented as (A).

## 3. Registry rows changed (`docs/governance/task-registry.md`)

Every change **prepends** a `**Status 2026-09-28 …**` note and keeps the original text after
"Original status:".

| Row | Old status | New status | Evidence |
|---|---|---|---|
| PAY-DOUBLE-CREDIT-1 | OPEN — HIGH; implementation HALTED pending human approval | **CLOSED — FIXED** (IMPLEMENTED against MOCK; real PSP PROVIDER DEPENDENT) | `8ce538c`/`cb03868`/`92f5889`/`5ee09e4`, merged `a72128d`; `rv-fh3-ledger.md` APPROVED + CONFIRMED; `rv-fh3-security.md` APPROVE; `rv-fh3-payments.md` APPROVE; `rv-fh3-code-review.md` READY WITH CONDITIONS; `qa-fh3-adjudication.md` |
| PAY-SEC-S-H1 | OPEN — HIGH (not reachable) | **CLOSED** | `rv-prh-i1-payout-security.md` re-verification 1 (`cb1330f`, at `0a96a01`); `rv-prh-i1-callback-ledger.md` re-review 2; LOW pins in `109ef04` |
| PAY-SEC-S-M1 | OPEN — MEDIUM | **CLOSED** | same records; `TestRVLF_SM10_…`; sync-path centralization `TestPayoutDispatch_SecGapC_…` (`109ef04`) |
| KS-DEP-T2-T3-1 | OPEN — must close before PRH-I1 complete | **CLOSED — FIXED** (`2da7548`) | `qa-killswitch-phase2-verification.md` §2 PASS with a killed mutant; architect code read (this review); code-review record missing (FH7-07) |
| F-POOL-2 | Registered; "Payments part still NOT IMPLEMENTED" | **OPEN; payments part IMPLEMENTED (MOCK)** | this review §1(c); `rv-fh3-payments.md`; remaining: FH7-04, FH7-05, LF-C1, casino re-review, KYC-SUBMIT-OUTBOX-1 |
| PRH-I1 | In progress; "Phase 2 NOT IMPLEMENTED" | **PARTIALLY IMPLEMENTED (MOCK only)** | phase 2 merged, KS-DEP closed, callback/payout/INV-DEP-1 records; open items listed in the row |
| PRH-I2 | Casino re-review pending; KYC "not yet gate-reviewed" | Unchanged label; KYC reviewed, casino code re-review still pending | `rv-prh-i2-kyc-security.md` re-verification 5; `rv-prh-i2-kyc-code-review.md` re-review 2; `rv-prh-i2-casino-code-review.md` NOT READY |
| PRH-I3 | PARTIALLY IMPLEMENTED | Unchanged label; deposit and payout gates now wired by PRH-I1; code re-review missing | `deposit_v2.go:202`, `drive.go:100`, `payout.go:291`, `payout_sweep.go:104`; `rv-prh-i3-*.md` |
| PRH-I5 | IMPLEMENTED against MOCK, reviews pending | Unchanged label; security APPROVE with C1/C2; code re-review missing | `rv-prh-i5-security.md`, `rv-prh-i5-code-review.md` |
| PRH-D1 | Not started | **DONE (design)** | ADR 0095 ACCEPTED rev 4 (+ §32) |
| PRH-D2 | Not started | **DONE (design)** | ADR 0096 ACCEPTED |
| PRH-D3 | Design complete, IMPLEMENTED pending security review | **DONE (design)**; security review complete | `rv-prh-i4-security.md` §8 APPROVE |
| PRH-REV | Not started | **IN PROGRESS** (missing re-reviews listed) | this review, §5 N |
| PRH-REF | IMPLEMENTED; security agreement + review PENDING | Unchanged label; security AGREED + APPROVE WITH CONDITIONS; C1 partial (FH7-05) | `prh-ref-provider-reference-bound.md` §8/§10; `rv-prh-ref-code-review.md` |
| PRH-REF-F4 | Registered — Medium; not yet ruled | **FIXED** | callback F2 fix (`boundedDeclineReasonAudited`); `rv-prh-i1-payout-security.md` re-verification 1; `rv-prh-i1-callback-ledger.md` re-review 2 |
| PRH-REF-F1 | Medium, latent | Unchanged: **OPEN, latent** (no payments caller of `idempotency.Assign`) | grep of non-test code |

**Checked and left unchanged, because the text is accurate:**
- FH3-FOLLOWUP-1 (CLOSED);
- A7-5C-STATIC-1 (SUPERSEDED);
- HD-LEDGER-UNALLOC-1 (DECIDED);
- LEDGER-SUSPENSE-B-1;
- PAY-SEC-S-M2, PAY-SEC-TESTS-1, PAY-SEC-S-L2, PAY-SEC-PC3, PAY-SEC-CR-1;
- PRH-I4;
- PAY-F3SM-TEST-1, PAY-POLL-AMOUNT-1 (see FH7-06), PAY-SWEEP-CAS-NOISE-1;
- TEST-T11A-FLIP-1.

TEST-RESISO-RACE-1 was left to the orchestrator, as instructed.

**Launch blockers: status text checked, none closed.**

| Item | Status text | How checked |
|---|---|---|
| KS-AUDIT-TENANT-1 | Accurate | `auditTenantID()` is still used (`payments_kill_switch_handlers.go:246/326/643/703`); no `audit_log` RLS migration after 0107 |
| PAY-P1-MULTISUCCESS-ALERT-1 | Accurate | Still a `slog` line only (`orchestrator.go:1018`, `:1509`) |
| WEBHOOK-EDGE-1 | Accurate | Infra item; not code-verifiable |
| CP-W1 | **Not registered** (FH7-11) | `NewSweeper` has no non-test caller |
| KYC-SUBMIT-OUTBOX-1 | Accurate | Registry text read |
| PAY-SEC-LAUNCH-1 | Accurate | Registry text read |
| PROV-OUTBOUND-CRED-1-LEGACY-PATH | Accurate | Code at `orchestrator.go:555-835` |
| DEVOPS-0107-INDEX-WINDOW-1 | Accurate | 0107 builds its indexes non-concurrently |
| PAY-PSP-CONTRACT-INVDEP1 | Accurate | Registry text read |
| PAY-RECON-N1 | Accurate | Registry text read |
| LEDGER-MANUAL-ADJ-4EYES-1 | Accurate | Registry text read |
| CI-BILLING-1 | Accurate | Registry text read |
| F-POOL-1 K1 | Accurate | Registry text read |

## 4. ADR status changes

All changes are dated notes; no status text was deleted.

- **ADR 0095:**
  - A header status note gives per-part corrections. The overall label stays ACCEPTED —
    PARTIALLY IMPLEMENTED. §28 is IMPLEMENTED; callback cutover IMPLEMENTED against MOCK; payout
    PARTIALLY IMPLEMENTED; kill switch PARTIALLY IMPLEMENTED with KS-DEP IMPLEMENTED; §28.9
    IMPLEMENTED; §29 conformant except for the legacy path.
  - §9.3 has amendment L-b.
  - §10.9.3 and the §10.9 labels have status notes.
  - The §28 status has a note.
  - A new §32 records this review.
- **ADR 0082:** the A7 status stays **IMPLEMENTED**, with a dated note that the L2 residual is
  closed (`430d4f7`) and that R0/L1 were verified in code.
- **ADR 0094:** a dated note records that F-POOL-1 is CLOSED WITH CONDITIONS with K1
  outstanding. The implementation stays IMPLEMENTED.
- **ADR 0096:** the header was stale ("IMPLEMENTED, pending …"). A dated note sets it to
  **ACCEPTED — PARTIALLY IMPLEMENTED**, which matches its own §16–§18 and registry PRH-I3.
- **ADR 0097:** a dated note sets it to **ACCEPTED — PARTIALLY IMPLEMENTED**. The security
  review is done (APPROVE); PRH-I4-METRICS-1 and WEBHOOK-EDGE-1 are open.
- **ADR 0093** (PROV-OUTBOUND-CRED-1): not edited. Its header is a decision status (ACCEPTED),
  and the implementation label lives in registry PROV-OUTBOUND-CRED-1 (PARTIALLY IMPLEMENTED).
  I did not re-verify that label.

## 5. Final-gate items A–N (architecture viewpoint)

**A. PAY-DOUBLE-CREDIT-1 closed.**
- **Yes.** INV-DEP-1 is enforced in three independent layers:
  1. the application choke point, before posting;
  2. the rule-2 re-check under the same intent lock;
  3. two DB partial unique indexes, which are RLS-independent.
- Every deposit-success source routes through the one wrapper: callback T7/T13/T13d, phase C,
  cascade children, and the sweeper poll with T17 re-drive.
- The only other caller, the legacy test-only path, is caught by layers 2 and 3.
- The registry row is now CLOSED — FIXED, with every mandatory specialist record present.

**B. ADR 0095 amended.**
- **Yes.** Revision 4 added §28 (AM-2), §29 and §30 (AM-1), with in-place SUPERSEDED/AMENDED
  markers at T7, T13, §4.4, §20, §21.2 and §21.6 LF95-C6.
- This review adds:
  - the §9.3 L-b amendment;
  - the dated status corrections;
  - §32.
- No earlier text was rewritten.

**C. LF-Q1 superseded appropriately.**
- **Yes.** `ledger-finance` owns the financial ruling (`lf-q1-supersession.md`, `17e5ffc`), and
  ADR 0095 §28.7 restates it without overriding it. T13 as the intent's *first* success still
  posts to `player_cash`, so LF-Q1 survives for that case. Only the multiple-success case is
  superseded.
- HD-LEDGER-UNALLOC-1 "(A) now, (B) later" is implemented as (A). (B) is deferred as
  LEDGER-SUSPENSE-B-1.
- The architect did not overrule `ledger-finance` on any financial point.

**D. F-POOL-2 updated and implemented.**
- **Updated: yes** (§29 durable-state map and rules).
- **Implemented for payments against MOCK adapters: yes.** I verified the three-phase A/B/C
  split on every live deposit, sweeper, callback and payout path, and the `txscope.Held` refusal
  in the gate.
- **Not CLOSED.** Before it can close:
  - the legacy `InitiateDeposit` path must be removed (FH7-04);
  - LF-C1 needs recording per real adapter;
  - deposit-response reference validation is needed (FH7-05);
  - the casino code re-review is needed (FH7-08);
  - KYC-SUBMIT-OUTBOX-1 must be done before a real KYC adapter.

**E. A–O matrix green (evidence exists).**
- **Evidence exists; not re-run by me.**
- `internal/payments/inv_dep1_matrix_integration_test.go` has scenarios A–L, O, O2, the two
  inverted tests, and the mutation tests. `internal/reconciliation/inv_dep1_recon_integration_test.go`
  covers M.
- N (mutation) is in `evidence/prh-i1-mutation-kill.txt`, in the FH-3, FH-3b, FH-3c and
  FH3-FOLLOWUP-1 sections.
- Green runs are recorded in:
  - `qa-fh3-adjudication.md` §3, at `109ef04`: 17/17, D and K at 50 reps under `-race`;
  - `rv-fh3-ledger.md` re-review 1 and the FH3-FOLLOWUP-1 confirmation (full suites; D, K, A7
    #1a and #5c at `-race -count=3`);
  - registry TEST-T11A-FLIP-1, which covers the full race suites at `498559a`.
- A GitHub CI run is still outstanding (CI-BILLING-1).

**F. PAY-SEC-S-H1 closed.**
- **Yes.** `security` closed it in re-verification 1. The deferred-receipt replay filter is
  derived from `attempt.Operation`, and MH1 and MH1b are killed.
- `ledger-finance` independently records N3/S-H1 closed.
- The fix is merged.
- Payout callbacks remain route-refused (PROVIDER DEPENDENT), with a standing `security` review
  condition on future wiring.

**G. PAY-SEC-S-M1 closed.**
- **Yes.** A callback payout success whose reference conflicts with the stored one goes T10
  `provider_reference_mismatch`, and MM1 is killed.
- In `109ef04` the rule is centralized in `applyPayoutSuccess`, which also covers the sync path
  (SecGapC is killed).
- The deposit-side analogue on the poll path is FH7-06 (Low, latent).

**H. Payout security findings completed or explicitly escalated.**
- **Yes.** Completed:
  - S-H1, S-M1 and S-M2 are closed;
  - S-L2 is closed (SP-C V1; the `ledger-finance` FH-6 round 2 P-C1 confirmation);
  - PC3 and CR-1 are closed;
  - the MA, MRF, cross-operation and sync-reference pins landed in `109ef04`.
- Explicitly escalated as tracked launch conditions:
  - S-L1, S-L3, S-L4 and destination binding (PAY-SEC-LAUNCH-1, NOT IMPLEMENTED);
  - HD-0095-1 (BLOCKED, human);
  - CP-W1 (no sweeper wired; **needs its own registry row**, FH7-11).

**I. A7 lock-order tests complete.**
- **Yes.** All eight §(7) tests are present, and the three named mutants are killed.
- The #5c waiter also requires `wait_event_type='Lock'` (`430d4f7`).
- The §1.6/§1.7 as-built rows are present, and both halves are gate-reviewed by
  `ledger-finance` (`rv-a7-tests.md` closure; `rv-fh3-ledger.md`).
- ADR 0082 A7 is IMPLEMENTED. I confirmed R0 before L1 and the parent before the attempt in code.

**J. Kill-switch phase 2 reviewed and safely integrated.**
- **Yes, with FH7-07.**
- Reviews: `security` APPROVE WITH CONDITIONS, with P2-L1..L4 closed or tracked; `code-reviewer`
  APPROVE WITH CONDITIONS, with C1/C2 closed by `ce77bac`/`50b595d` per ADR 0095 §10.9.4;
  architect approved the merge (§10.9); `qa` PASS.
- Merged before FH-3, as ordered. The FH-3 wrapper passes the pool and resolver at every site
  and adds no provider call inside an evidence tx.
- KS-DEP-T2-T3-1 is fixed and verified.
- Remaining:
  - a `code-reviewer` record for the fix round and `2da7548` (FH7-07);
  - KS-AUDIT-TENANT-1 and alert delivery (launch-blocking, open).

**K. KYC/rate-limit/provider-reference work synchronized with the roadmap, not lost.**
- **Partially.** Nothing is lost at the registry and ADR level:
  - PRH-I2, PRH-I3, KYC-SUBMIT-OUTBOX-1 and KYC-REVIEWREQ-FORWARD-1;
  - PRH-I4 and PRH-I4-*, and WEBHOOK-EDGE-1;
  - PRH-REF, PRH-REF-C2, C3 and F1, and PROVIDER-REF-BOUND-1.

  ADR 0096 and 0097 headers are now corrected.
- **But** `docs/active-stage.md` and `docs/progress.md` do not mention the PRH or FH block at all
  (FH7-10). That must be synchronized by the orchestrator at PRH-GATE.
- PRH-REF security C1 is only half-implemented (FH7-05).

**L. Governance incident documented.**
- **Yes.** `docs/governance/incident-2026-09-27-local-db-credential-mutation.md` exists, with
  status RESOLVED, and records the actors, actions, restoration and corrective control.
- Registry row GOV-INC-2026-09-27-DBCRED is RESOLVED.
- Review records since then (for example `rv-fh3-*`, `qa-killswitch-phase2-verification.md`)
  state that they used no `sudo` and changed no role or password.

**M. CLAUDE.md safety rule preserved.**
- **Yes.** `CLAUDE.md` § Environment safety carries the permanent rule: no sub-agent alters shared
  DB roles, passwords, test infrastructure or credentials; STOP AND REPORT; no `sudo`/`ALTER
  ROLE`/`CREATE ROLE`. It points to the incident record, and it was introduced in `a60b08c`.
- This review did not modify `CLAUDE.md`.

**N. All specialist reviews complete.**
- **Not all.** Records present, with each one's final verdict (in `docs/plans/payment-readiness/`
  unless stated otherwise):

| Record | Final verdict |
|---|---|
| `rv-fh3-ledger.md` | APPROVED, then CONFIRMED (FH3-FOLLOWUP-1) |
| `rv-fh3-security.md` | APPROVE (re-verification 1) |
| `rv-fh3-payments.md` | APPROVE |
| `rv-fh3-code-review.md` | READY WITH CONDITIONS (N-1 → PAY-RECON-N1; C4b closed) |
| `qa-fh3-adjudication.md` | INV-DEP-1 held; the matrix bugs were test bugs, corrected |
| `rv-prh-i1-callback-ledger.md` | APPROVE WITH CONDITIONS (re-review 2) |
| `rv-prh-i1-callback-code-review.md` | READY WITH CONDITIONS (re-review 2; conditions later closed per `rv-fh3-code-review.md`) |
| `rv-prh-i1-payout-security.md` | APPROVE WITH CONDITIONS (re-verification 1) |
| `rv-prh-i1-payout-ledger.md` | FH-6 round 2 confirmation: P-C1..3 closed |
| `rv-prh-i1-payout-code-review.md` | FH-6 round 2: all conditions closed |
| `rv-a7-tests.md` | A7 IMPLEMENTED |
| `rv-prh-i1-killswitch-security.md` | Re-verification 2: no blocking finding |
| `rv-prh-i1-killswitch-phase2-security.md` | APPROVE WITH CONDITIONS |
| `rv-prh-i1-killswitch-phase2-code-review.md` | APPROVE WITH CONDITIONS; **no re-review of the fix round** |
| `rv-prh-i1-killswitch-phase2-architect.md` | APPROVE THE MERGE |
| `qa-killswitch-phase2-verification.md` | PASS |
| `rv-prh-architect.md` | Architect review |
| `rv-0095-ledger-reverify.md`, `rv-0095-security-reverify.md` | ADR 0095 accepted |
| `rv-0096-ledger-reverify.md`, `rv-0096-security-reverify.md` | ADR 0096 accepted |
| `rv-prh-i2-kyc-security.md` | Re-verification 5: no launch-blocking finding |
| `rv-prh-i2-kyc-code-review.md` | Re-review 2: READY (N-1b then closed by security) |
| `rv-prh-i2-kyc-identity-compliance.md` | Rulings 1–5 |
| `rv-prh-i2-casino-security.md` | APPROVE WITH CONDITIONS |
| `rv-prh-i2-casino-code-review.md` | **NOT READY, no re-review** |
| `rv-prh-i3-security.md` | APPROVE WITH CONDITIONS |
| `rv-prh-i3-ledger.md` | SIGN-OFF WITH CONDITIONS |
| `rv-prh-i3-code-review.md` | **NOT READY, no re-review** |
| `rv-prh-i4-security.md` | APPROVE (§8) |
| `rv-prh-i5-security.md` | APPROVE with C1/C2 |
| `rv-prh-i5-code-review.md` | **NOT READY, no re-review** |
| `rv-prh-ref-code-review.md` | READY WITH FOLLOW-UPS |
| PRH-REF security (in `prh-ref-provider-reference-bound.md` §10) | APPROVE WITH CONDITIONS |
| This record | APPROVE WITH CONDITIONS |

- **Missing:**
  - `code-reviewer` re-reviews of PRH-I2 casino, PRH-I3 and PRH-I5 (FH7-08);
  - a `code-reviewer` record for the kill-switch phase-2 fix round and `2da7548` (FH7-07);
  - an explicit `ledger-finance` confirmation of FH-5 C2/C3 (FH7-13);
  - the orchestrator synthesis (PRH-REV).
- **FH-specific reviews (FH-1 to FH-6) are complete.**

## 6. Conditions (summary)

**Before PRH-GATE:**
- FH7-07: `code-reviewer` confirmation of the phase-2 fix round and `2da7548`.
- FH7-08: `code-reviewer` re-reviews of PRH-I2 casino, PRH-I3 and PRH-I5.
- FH7-10: sync `active-stage.md` and `progress.md`.
- FH7-11: register CP-W1.
- FH7-13: `ledger-finance` one-line confirmation of FH-5 C2/C3.

**Before the first real PSP:**
- FH7-04: delete the legacy deposit path.
- FH7-05: validate deposit-response references and park on a violation.
- FH7-06: cross-check the poll amount **and** reference.
- The registered launch blockers (unchanged).
