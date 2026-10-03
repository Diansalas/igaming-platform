# ADR 0095 — Provider-I/O Transaction Boundary and Provider-Neutral Payment Contract

- **Status note 2026-09-28 (`architect`, FH-7 final architecture review,
  `docs/plans/payment-readiness/rv-fh7-architect-final.md`; verified against code at `7f5d0fb`).
  Overall label unchanged: ACCEPTED — PARTIALLY IMPLEMENTED.** The revision-4 table below is kept
  as the 2026-09-27 record. Where they differ, these per-part corrections govern:
  - **§28 AM-2 (INV-DEP-1): IMPLEMENTED** (MOCK adapters only; real PSP PROVIDER DEPENDENT, see
    PAY-PSP-CONTRACT-INVDEP1). Commits: FH-3 `8ce538c`, FH-3b `cb03868`, FH-3c `92f5889` and
    FH3-FOLLOWUP-1 `5ee09e4`, merged at `a72128d`. Reviews:
    - `ledger-finance`: APPROVED, then CONFIRMED (`rv-fh3-ledger.md`);
    - `security`: APPROVE (`rv-fh3-security.md`, re-verification 1);
    - `payments`: APPROVE (`rv-fh3-payments.md`);
    - `code-reviewer`: READY WITH CONDITIONS (`rv-fh3-code-review.md`, re-review 1). Its N-1 was
      ruled by `ledger-finance` and registered as PAY-RECON-N1;
    - `qa`: A–O matrix adjudicated (`qa-fh3-adjudication.md`).

    §28.13 option (B) is NOT IMPLEMENTED by human decision (LEDGER-SUSPENSE-B-1). M1/M2 remain
    BLOCKED.
  - **§4/§5.1 callback cutover.** The table's "both NOT READY" is superseded:
    - `ledger-finance` re-review 2 returned APPROVE WITH CONDITIONS;
    - `code-reviewer` re-review 2 returned READY WITH CONDITIONS;
    - the conditions were then closed (`rv-fh3-code-review.md`, "Status of my prior conditions";
      `f43025c`; `prh-i1-mutation-kill.txt`).

    Label: IMPLEMENTED against MOCK adapters. PROVIDER DEPENDENT for a real PSP.
  - **§5.2 payout: PARTIALLY IMPLEMENTED.**
    - `security` APPROVE WITH CONDITIONS; S-H1 and S-M1 CLOSED (`rv-prh-i1-payout-security.md`,
      re-verification 1).
    - FH-6 round 2 confirmed by `ledger-finance` and `code-reviewer`.
    - Payout-typed callbacks are refused at the route (PROVIDER DEPENDENT).
    - PAY-SEC-LAUNCH-1 is NOT IMPLEMENTED.
  - **§10 kill switch: PARTIALLY IMPLEMENTED.**
    - Phase 2 (`4e04f4e`) is merged on this branch.
    - KS-DEP-T2-T3-1 is IMPLEMENTED (`2da7548`, `qa-killswitch-phase2-verification.md` §2 PASS).
    - Alert delivery and KS-AUDIT-TENANT-1 remain NOT IMPLEMENTED and launch-blocking.
  - **§12.** The §28.9 `pay_captured_unposted` kind is IMPLEMENTED. It is a standing, unwindowed
    report per provider run. The stream itself stays MOCK.
  - **§29 (F-POOL-2).** The deposit (`deposit_v2.go`/`drive.go`), sweeper, callback and payout
    (`payout.go`/`payout_sweep.go`) paths conform to the three-phase rule. The legacy
    `Orchestrator.InitiateDeposit` path does not: it has a test-only caller and is tracked as
    PROV-OUTBOUND-CRED-1-LEGACY-PATH. *[Amendment, 2026-09-28 (`payments`, PRH-2 E2,
    PROV-OUTBOUND-CRED-1-LEGACY-PATH), corrected 2026-09-28 per code-review E2-2 and security
    E2-C1/F-2/F-3: **CLOSED WITH A REGRESSION GUARD (E2-C1).** `InitiateDeposit`,
    `InitiateDepositAudited`, `attemptDeposit`, `handleDecline`, `resolveAmbiguous` and
    `RecordDepositMultipleSuccessRefusal` are deleted from `orchestrator.go`.
    `InitiateDepositAttempt` (`deposit_v2.go`) is now the only deposit-creation entry point in
    **production**; it is not the only one in the tree. A second, deliberately near-verbatim
    entry point survives as a TEST-ONLY reimplementation
    (`legacyShapeInitiateDeposit`/`legacyShapeAttemptDeposit`/`legacyShapeHandleDecline`/
    `legacyShapeResolveAmbiguous`, `receive_bridge_integration_test.go`) - it is, line for line,
    close to `cabca27:orchestrator.go:555-826`, built from still-live primitives
    (`ListRoutingCandidates`+`RankRoutingCandidates` via a TEST-ONLY `RouteProvider` wrapper now
    relocated out of production per E2-3/F-2, `setIntentAttempt`, `finalizeDeclined`,
    `finalizeAmbiguous`, `postDepositSuccess`) rather than a copy-paste of the deleted functions'
    own source, but it DOES call `provider.Deposit`/`provider.QueryStatus` directly inside the
    caller's own transaction, exactly like the deleted chain - it is never reachable from a
    compiled binary (`_test.go`, `integration` build tag), but it is not merely a fixture for the
    receipt/callback path: E2-1 (code review) found four `orchestrator_integration_test.go` tests
    whose subject (the production synchronous-cascade/decline/ambiguous behaviour) had silently
    become this copy's behaviour instead, once it stopped calling the real `InitiateDeposit`. Two
    of those four were deleted as redundant with existing live coverage
    (`TestInitiateDepositAttempt_DeclinedOutcome_T8`,
    `TestDriveCreatedAttemptCascade_PoolThreadedToResolver`). One
    (`TestInitiateDeposit_CascadeExhaustedEndsDeclined`) was migrated onto `InitiateDepositAttempt`
    directly and strengthened with a third, spy-wrapped accepting provider so a `MaxCascadeDepth`
    off-by-one mutant is actually observable (no live-path test already covered this exhaustion
    behaviour). The last (`TestInitiateDeposit_AmbiguousOutcomeIsNotCascaded`) was, on a first pass,
    also migrated directly onto `InitiateDepositAttempt` - but that migration FAILED when actually
    run: its second half assumed a callback naming the mock's synchronously-returned ambiguous
    reference would post a success, which is wrong for the live path (`MarkAmbiguousFromSubmitting`,
    T6, deliberately never records a `provider_reference` on the attempt row - only the legacy,
    no-longer-authoritative `deposit_intents` column gets it). It was deleted, not force-fitted:
    both of its original halves are already covered, more thoroughly, by
    `TestInitiateDepositAttempt_AmbiguousOutcome_T6` (never cascades) and
    `TestReceipt_Unresolved_DeferredThenAppliedOnceReferenceKnown` (`receipt_integration_test.go` -
    `deferred_unresolved` then convergence to succeeded once the reference is separately learned).
    A companion gap this same mutant-testing pass found and closed:
    `TestInitiateDepositAttempt_DeclinedOutcome_T8` alone registers only one provider, so a mutant
    removing `cascadeEligible`'s own cascadable-check is not observable there (no second provider to
    wrongly route to) - closed by a new test,
    `TestInitiateDepositAttempt_NonCascadableDeclineNeverCascadesToAvailableFallback`
    (`deposit_v2_integration_test.go`), registering a second, available, accepting provider.
    Because a second, real (if test-only) call path to `provider.Deposit`/`QueryStatus` still
    exists, and because the pre-existing `IO-1C` static guard
    (`no_provider_call_in_tx_closure_static_test.go`) provably misses a NAMED, non-closure
    function that calls a `PaymentProvider` outbound method directly (security review E2-C1: a
    planted method shaped exactly like the deleted `attemptDeposit` passes IO-1C, since IO-1C
    inspects only `*ast.FuncLit` closures with a `pgx.Tx` parameter), PROV-OUTBOUND-CRED-1-LEGACY-
    PATH is recorded as **closed with a regression guard**, not unconditionally closed: a new, broader static test,
    `TestPCG1_NoOutboundProviderCallOutsideTheGate`
    (`internal/txscope/payment_provider_call_guard_static_test.go`), now forbids ANY non-test call
    to `PaymentProvider.Deposit`/`Withdraw`/`QueryStatus`/`Refund` anywhere in the repository,
    in any package, with or without a `pgx.Tx` parameter, outside `callProvider`'s own two
    recognized call shapes (a named adapter-call builder, or a `FuncLit` passed directly as a
    `callProvider` argument) - this subsumes "extend IO-1C to `ast.FuncDecl` with a `pgx.Tx`
    parameter" for these four method names specifically, since it does not condition on a
    `pgx.Tx` parameter existing at all. The 21 test call sites of the deleted functions
    (10 files, per the original E2 migration) were migrated to `InitiateDepositAttempt` where the
    test's subject was `InitiateDeposit`'s own behaviour, or to the TEST-ONLY bridge above where
    the test's subject was the receipt/callback path and needed the pre-cutover on-disk shape (see
    the E2-1 correction two paragraphs up for the narrower set of tests that needed
    re-classification after the fact). `TestC4_ResolveAmbiguousOutcomeSucceeded_
    ReturnsWrappedRefusal` and `TestC4_InitiateDepositAudited_WritesRefusalAudit`
    (`migration_0107_integration_test.go`) tested only the deleted
    `DepositIntentAlreadyResolvedRefusal`/`RecordDepositMultipleSuccessRefusal` wiring (itself also
    deleted from `orchestrator.go`, [deleted by E2]) and were deleted as redundant with the live
    T10/T13d coverage
    (`TestMigration0107_T13tT13d_TerminalReasonTrigger_HEAD`,
    `TestINVDEP1_FL1_ApplicationChokePointCatchesItBeforeTheDBBackstop`,
    `TestINVDEP1_FL2_MultipleSuccessAlertLogContentIsPinned`) and `TestX5_LedgerBackstopMapping`
    for the `ErrDepositIntentAlreadyResolved` sentinel itself (ledger-finance LF-16, corrected
    premise E2d: the legacy chain was never the only path to
    `ledger.ErrDepositAlreadyPostedForIntent` - the live `postDepositSuccess` maps it at its own
    call site, reached by a real race and by a direct `ledger.Post` fixture). The now-orphaned
    `DepositIntentAlreadyResolvedRefusal` wrapper type (zero constructors once `resolveAmbiguous`
    is gone) is deleted with it. Mutant parity re-confirmed at this gate:
    `docs/plans/payment-readiness/evidence/prh2-e2-mutant-parity.txt`.]*
  - **§14 (ADR 0082 A7): IMPLEMENTED.**
- **Current status (architect, 2026-09-27, revision 4): ACCEPTED — PARTIALLY IMPLEMENTED.**
  Amended by §28 (AM-2, INV-DEP-1, NOT IMPLEMENTED), §29 (F-POOL-2 durable-state definition)
  and §30 (AM-1, kill-switch route family). Per part, on this branch:

  | Part | Label | Evidence / gate still open |
  |---|---|---|
  | §4/§5.1 deposit attempts, receipts, callback cutover (migration 0101) | PARTIALLY IMPLEMENTED | Callback cutover: `ledger-finance` and `code-reviewer` both NOT READY (`rv-prh-i1-callback-ledger.md`, `rv-prh-i1-callback-code-review.md`). **§28 INV-DEP-1 NOT IMPLEMENTED** (PAY-DOUBLE-CREDIT-1, HIGH): until it lands, the as-built T13/T7 can credit one intent twice. |
  | §5.2 payout T1p / phase C / sweeper | PARTIALLY IMPLEMENTED | `code-reviewer` APPROVE; `ledger-finance` APPROVE WITH CONDITIONS; `security` sign-off pending. |
  | §7 sweeper | PARTIALLY IMPLEMENTED | LF95-R1 automatic re-drive NOT IMPLEMENTED (operator T17 path only). |
  | §10.2–§10.5 kill switch (0105, 0106) | PARTIALLY IMPLEMENTED | Data model, triggers, routes (as amended by §30) IMPLEMENTED. Phase 2 wiring (deposit kill-switch decline, labelled payout hold) IMPLEMENTED on `worktree-agent-aa2bb3c6bdd6d51eb` @ `4e04f4e`; merge approved by the architect (§10.9), and this label applies to this branch once that merge lands. Residual KS-DEP-T2-T3-1 (§10.9.3) NOT IMPLEMENTED. Alert delivery NOT IMPLEMENTED (launch-blocking). KS-AUDIT-TENANT-1 NOT IMPLEMENTED (launch-blocking). |
  | §10.1 manifest | PARTIALLY IMPLEMENTED | `SupportsRefund`, `CallbackEchoesMerchantReference` enforced; PRH-I1-MANIFEST-1..4 deferred. |
  | §11 PROV-OUTBOUND-CRED-1 | PARTIALLY IMPLEMENTED | Casino/KYC kind split IMPLEMENTED. Payments kind split and pool threading IMPLEMENTED on `4e04f4e`, with code-review C1/C2 closed by `ce77bac`/`50b595d`; merge approved (§10.9). Legacy `InitiateDeposit` path bypassed the gate: PROV-OUTBOUND-CRED-1-LEGACY-PATH. *[Amendment, 2026-09-28 (PRH-2 E2), corrected 2026-09-28 per code-review/security: CLOSED WITH A REGRESSION GUARD (E2-C1) - the production legacy chain is deleted, but a near-verbatim TEST-ONLY copy survives for fixture purposes and a new tree-wide static guard (`TestPCG1_NoOutboundProviderCallOutsideTheGate`) now forbids any non-test outbound provider call outside the gate; see the §29 amendment above.]* |
  | §12 payment reconciliation (0102, 0104) | MOCK | Real PSP statement PROVIDER DEPENDENT; `code-reviewer` NOT READY; §28.9 kind NOT IMPLEMENTED. |
  | §15 casino launch, KYC create/submit | IMPLEMENTED against MOCK adapters | Casino `code-reviewer` NOT READY (R1); IO-1B/IO-1C closed (`0ca1193`). |
  | INV-IO-1 (a)–(d) | IMPLEMENTED | (c) is `internal/txscope/no_provider_call_in_tx_closure_static_test.go`. |
  | §4.8 M1/M2 | BLOCKED | HD-0095-1, LEDGER-MANUAL-ADJ-4EYES-1. |
  | §5.5 outbound refund | NOT IMPLEMENTED | By design (no flow). |

  **Migration numbers (authoritative; supersedes every other number in this document):**
  0101 payment attempts and receipts; 0102 payment statement reconciliation (MOCK); 0104
  payment-statement line CHECKs; 0105 kill switch; 0106 payment-attempts platform-GUC hardening
  (redefines the three 0105 kill-switch functions); **0107 INV-DEP-1 backstops (§28.8; number to
  be recorded in the registry allocation line by the orchestrator)**. 0099 (provider-reference
  bound), 0100 and 0103 (ADR 0096 KYC) are not this ADR's.
- **Original status line (revision 2; SUPERSEDED by "Current status" above):** **ACCEPTED (design) — NOT IMPLEMENTED** (revision 2, 2026-09-27).
  - Revision 1 was PROPOSED. Six reviews followed (§21 ledger-finance, §22 security, §23
    payments, §24 QA, §25 identity-compliance, §26 casino).
  - Revision 2 writes every condition into the design text. §27 maps each condition ID to the
    section that satisfies it; none is dropped.
  - The acceptance bar set by the orchestrator is met in text: ledger-finance LF95-C1..C12, and
    the launch-blocking security conditions S95-C1, C5, C6, C8 and C13.
  - LF95-C13/C14 and QA changes 1–8 gate `IMPLEMENTED`, not acceptance; they are specified in
    §12, §16 and §18.
  - Acceptance of the design is not a claim that anything is built, secure or launch-ready.
    Every deliverable below stays `NOT IMPLEMENTED` until an implementing change lands and passes
    its own reviews.
- **Decision type:** architecture, cross-domain. It touches `payments`, `withdrawal`, `ledger`
  (lock-class inventory only), `reconciliation`, `providercred`, `casino` (launch only), `kyc`
  (create and submit only), `httpserver` and `cmd/platform-api`.
- **Owner:** `architect`.
- **Sign-off required before implementation:**
  - `ledger-finance`: mandatory, per LF-C2 (review 19 §4). Covers §4, §6, §7, §12, §14 and §16.
  - `security`: covers §9.4, §10, §11 and §6.5.
  - `payments`: covers §4 through §12.
  - `casino` and `identity-compliance`: cover §15 only.
  - `qa`: covers §16.
- **Migration numbers (orchestrator re-allocation, 2026-09-27; SUPERSEDED by the authoritative
  map in "Current status" above):** 0101 payment attempts and
  receipts (was 0100; 0100 is now ADR 0096's KYC enforcement migration), 0102 kill switch, 0103
  payment statement reconciliation.
- **Registry rows closed by implementing this ADR** (each one per domain, only after review):
  - F-POOL-2;
  - the "not implemented" half of PROV-OUTBOUND-CRED-1;
  - PRH-D1 (this document).
- **Trigger:**
  - Human instruction "Payment Readiness & Provider-Independent Hardening" (PRH), 2026-09-27.
  - F-POOL-2, including ledger-finance's High (P1-class) dual-write sub-finding.
  - Security W2A-SEC-1, the launch-blocking precondition on PROV-OUTBOUND-CRED-1.
- **Human requirements, restated as binding:**
  - No DB transaction is held across any external provider call.
  - Financial correctness does not depend on a long transaction.
  - Correctness comes from idempotency plus durable state.
  - The platform ledger stays authoritative. The provider is never the source of truth.
  - The sequence "tx → provider call succeeds → tx rolls back → customer charged, platform
    unaware" must be impossible.
- **Amends (proposed; each amended owner must accept its own part):**
  - **ADR 0022 §2 and §6.** The `PaymentProvider` interface and canonical shapes change (§9).
    The adapter-declared capability layer gains an operation manifest (§10). The conformance
    suite gains cases (§16).
  - **ADR 0082 §2.1.** Amendment A7, owned by `ledger-finance`: accepted in §21.3 with the
    LF95-C9 scope rules (§14), and written into ADR 0082 by `ledger-finance` now that this ADR is
    ACCEPTED. It adds two L1 tables and places the callback-receipt insert (R0) between L0 and L1.
  - **ADR 0093 §5.** The resolution point and the adapter request types are fixed (§11).
  - **`docs/architecture/reconciliation-model.md` §2.2.** Mismatch state (b) no longer auto-posts
    from a statement line (§12.6).
  - **`docs/architecture/withdrawal-state-machine.md`.** `submitted` now means "dispatch claimed;
    the provider may have received it". `provider_reference` may be NULL while `submitted`
    (§4.7).
  - **`docs/architecture/payment-orchestration.md` §5.** Cascade from an *asynchronous* decline
    is limited to non-interactive methods (§4.6).
- **Depends on:**
  - **PROVIDER-REF-BOUND-1 / migration 0099** (`ledger-finance`, running in parallel). Every
    provider-reference column this ADR creates carries a CHECK against the platform maximum
    that 0099 defines. This ADR refers to that value as `PROVIDER_REF_MAX` and does not choose it.
    Migration 0101 (payment attempts; renumbered from 0100 by the orchestrator, §27) must not merge before 0099.
  - **ADR 0097** (webhook admission/rate limiting). Its order is admission → verification →
    binding → parsing → domain. §6 only changes the *domain* step.
- **Not in scope:**
  - any real vendor, vendor signature scheme or invented vendor behaviour;
  - AWS, IAM, IRSA, KMS or a proxy;
  - KYC enforcement (ADR 0096);
  - webhook rate limiting (ADR 0097);
  - outbound refund initiation (the contract is defined, but no flow uses it, §5.5);
  - payout cascade;
  - crypto custody (ADR 0008).

---

## 0. Decisions at a glance

| # | Decision |
|---|---|
| D1 | **Commit-intent → call-without-tx → commit-evidence.** Every external operation runs as: a short transaction that durably commits the *intent to call*; the provider call with **no transaction and no pooled connection held**; a short transaction that applies the *evidence* returned, under a compare-and-set (CAS) guard. Correctness comes from durable state, CAS and DB unique constraints. It never comes from holding a transaction open. |
| D2 | **One state machine for every outbound money operation.** Deposits and payouts both run through `payment_attempts` rows (migration 0101). There are 8 states, 4 of them non-terminal, and every transition is CAS-guarded, audited and backstopped by a DB trigger (§4). |
| D3 | **The attempt row is the outbox.** A cascade, a retry or a recovery is a committed `payment_attempts` row with `next_action_at`. A bounded worker (the sweeper) drives these rows with leases and `FOR UPDATE SKIP LOCKED` claims (§7). No provider I/O happens inside a webhook transaction. |
| D4 | **Deterministic external identity.** The merchant reference and the external idempotency key are both derived from the committed attempt id. They are never generated per try (§5). |
| D5 | **Evidence decides; the absence of evidence never does.** Success is applied only on verified, amount- and asset-matching evidence that names a provider reference, from the provider the attempt was sent to. Timeout, not-found and ambiguous results are never treated as failure for a payout, and never as success for anything. A deposit may be declined on "not found" only when the adapter declares that lookup authoritative and the provider never acknowledged the attempt (§4.5). |
| D6 | **Callbacks resolve by merchant reference and by provider reference, always bound to the verified provider.** They are accepted in any non-terminal state and durably receipted (`payment_provider_events`). A callback that cannot be resolved yet is deferred (capped), then applied when the attempt learns its reference, but only if it was received after the attempt was first sent (§6). |
| D7 | **A provider-neutral payment contract** (§9) with: `CallContext` (tenant plus resolved credential plus idempotency key plus deadline); a closed `ErrorClass` for retry and timeout classification; `StatusQuery` by either reference; and a code-declared `OperationManifest`. The MOCK adapter only exercises the state machine. |
| D8 | **Capability manifest and payment kill switch** (migration 0102, §10). Unsupported operations fail closed, first at registration and then at new activity, never at settlement. The kill switch is server-authoritative, tenant- and provider-scoped, audited, fail-safe, and evaluated atomically in the claim transaction. |
| D9 | **PROV-OUTBOUND-CRED-1** (§11). The credential is resolved per call, outside any transaction, and passed in `CallContext`. The adapter verifies the tenant and provider binding. No credential is held in an adapter. Credential access is refused while a transaction or a financial lock is held. |
| D10 | **Payment statement reconciliation, MOCK source** (migration 0103, §12). The statement is fetched outside any transaction, stored append-only, and matched in a REPEATABLE READ transaction. There are 8 mismatch kinds. The stream is detection only: remediation happens only through the evidence path or LEDGER-MANUAL-ADJ-4EYES-1. |
| D11 | **Casino launch and KYC create/submit get the same boundary with minimal redesign** (§15). No migration, no new state, no change to their financial or enforcement semantics. |

---

## 1. Context — where external I/O is inside a transaction today

Verified at HEAD `1560ad0`. Every adapter is an in-process MOCK, so none of these hazards is
reachable today (tripwire `TestOutboundPrecondition_EveryWiredAdapterIsSynthetic`).

| # | Site | What is held during the call | Hazard once non-MOCK |
|---|---|---|---|
| 1 | `payments.InitiateDeposit` → `attemptDeposit` → `provider.Deposit` (`internal/payments/orchestrator.go:595`) | The tenant tx (from `deposit_handlers.go:138`) holds the uncommitted `deposit_intents` INSERT, the audit rows, and RG advisory locks (L0.4) | Dual write (review 19 §6): the PSP accepts, then the tx rolls back, so the player is charged but not credited. A retry mints a new intent, so the player is charged twice. Pool pinning (F-POOL-2). |
| 2 | `resolveAmbiguous` → `provider.QueryStatus` (`:678`), repeated through cascade up to `MaxCascadeDepth` | Same tx | Same, multiplied by the cascade depth |
| 3 | Webhook `receiveDepositCallback` → `handleDecline` → `attemptDeposit` → `provider.Deposit`; `resolveAmbiguous` → `QueryStatus` (`:994-996`) | The webhook domain tx | PSP I/O inside the webhook tx. On rollback the PSP redelivers and the cascade fires again (a second charge at the next PSP). |
| 4 | `RouteProvider` → `provider.HealthStatus` (`:207`) | Every caller's tx | Pool pinning. Health lookups are I/O for a real adapter. |
| 5 | Withdrawal submit: `LockApprovedForSubmission` (L1 `FOR UPDATE`) → `provider.Withdraw` → `MarkSubmitted` (`internal/httpserver/withdrawal_handlers.go:827-850`) | The L1 row lock on `withdrawal_requests`, plus the tx | **Double payout.** The payout is sent, then the tx rolls back and the state is still `approved`, so a staff retry sends it again (LF-C2 #9). The L1 lock is also held across vendor latency. |
| 6 | Withdrawal resolve: `LockSubmittedForResolution` → `provider.QueryStatus` (`:1031-1047`) | The L1 row lock plus the tx | Pool pinning and a held L1 lock |
| 7 | `casino.LaunchGame` → `CreateLaunchSession(tx)` → `provider.Launch` (`internal/casino/orchestrator.go:320-340`), plus `HealthStatus` | The tenant tx, plus the RG (L0.4) and risk (L0.5) advisory locks | Pool pinning. The orphan vendor session moves no money (review 19). |
| 8 | `kyc.CreateVerification` (`internal/kyc/verification_service.go:95`) | The tenant tx | Pool pinning. An orphan vendor verification. No money path. |
| 9 | `kyc.submitVerificationDocuments` → `SubmitVerification` (`internal/kyc/document_service.go:201`) | The upload tx | Pool pinning. A lost submission result. |

Two further structural gaps this ADR also closes:

- **Cascade history.** `deposit_intents` stores only the last attempt, so
  `providerExclusionSoFar` (`orchestrator.go:951`) cannot exclude earlier providers after an
  asynchronous callback.
- **Non-deduplicating MOCK.** The MOCK mints a fresh reference on every `Deposit`/`Withdraw`
  call (`mock.go:346`). It cannot model provider-side idempotency on our key, so no test today
  can prove that a same-attempt retry is deduplicated.

---

## 2. Invariants (the checkable contract for `qa` and `code-reviewer`)

| ID | Invariant | Enforced by |
|---|---|---|
| **INV-IO-1** | No adapter outbound method (`Deposit`, `Withdraw`, `QueryStatus`, a statement fetch, `Launch`, `CreateVerification`, `SubmitVerification`, and any future outbound method) runs while the calling goroutine's context holds a pooled DB transaction. | (a) **API shape.** No function that can reach an adapter outbound method takes a `pgx.Tx`, and none is called from inside a `db.Pool.With*` callback. (b) **Runtime.** The provider-call gate (§3.2) refuses under `txscope.Held(ctx)` and returns `ErrorClassNotSent` without calling. (c) **Static test.** A source scan (in the spirit of `lockorder_static_test`) fails on an adapter-method call lexically inside a `With*` closure. (d) **Adversarial test.** The capture-style tests in §16. |
| **INV-IO-2** | Every money-moving outbound call (deposit submission, payout submission, and refund when built) is preceded by a **committed** `payment_attempts` row in state `submitting` that carries the provider id, merchant reference and external idempotency key used for the call. | The CAS `created→submitting` (or the payout claim) commits before the gate lets the call run. The gate takes the attempt's *committed* claim token and refuses without one. |
| **INV-IO-3** | The merchant reference and external idempotency key are pure functions of `payment_attempts.id`. They are persisted, and they are identical on every send of that attempt. | Immutable columns (trigger); `UNIQUE (tenant_id, merchant_reference)`; `UNIQUE (tenant_id, external_idempotency_key)`. |
| **INV-IO-4** | Every state change of an attempt is one CAS `UPDATE … WHERE id = $1 AND state = ANY($allowed_from)`, in the same tx as its audit record and its ledger effect (if any). A DB trigger rejects any (OLD, NEW) pair not in §4.3 and any change to an immutable column. | CAS in code; `payment_attempts_guard` trigger (0101). |
| **INV-IO-5** | Ledger posting never spans external I/O. Each posting runs in one short domain tx holding the ADR 0082 locks in class order, with the authoritative balance read in that same tx. | The D1 pattern; ADR 0082 A7 (§14); existing `ledger.Post`. |
| **INV-IO-6** | No success without evidence. `succeeded` is entered only by a verified callback, a `QueryStatus` result or a synchronous result reporting a definite success **whose amount and asset equal the attempt's**. A mismatch goes to `disputed`, never to `succeeded`. | `applyEvidence` (§4.4) is the only function that writes `succeeded`; trigger. |
| **INV-IO-7** | No payout failure without definite decline evidence. `withdrawal.Fail` (the hold release) is reachable from a dispatched payout only through `declined` evidence from the provider. Timeout, not-found, ambiguous, a KYC outcome and operator impatience are never enough. The only other release of a claimed payout's hold is M3, from `created` (never sent, INV-IO-9). | §4.5 asymmetry. The `payment_attempts.last_evidence_kind` column is written in the same UPDATE as every state change, and the guard trigger reads it (§13.1): `*→declined` for a payout requires `last_evidence_kind ∈ {sync, callback, query_status}`; any `→declined` for a deposit requires `≠ operator`; `→succeeded` requires `∈ {sync, callback, query_status}`. |
| **INV-IO-8** | At most one live (non-terminal) attempt per deposit intent, and exactly one payout attempt per withdrawal request. | Partial unique indexes (0101). |
| **INV-IO-9** | An attempt in `created` has never had a call that may have reached the provider (`ever_possibly_sent = false`). | `CHECK (state <> 'created' OR NOT ever_possibly_sent)`; `submitting→created` only on `ErrorClassNotSent`. |
| **INV-IO-10** | A verified callback is never lost. It is durably receipted in the same tx as its effect (or its deferral). If that tx fails, the response is retryable (5xx), so the provider redelivers. | `payment_provider_events` (0101); §6. |
| **INV-IO-11** | Provider credentials are resolved outside any tx, per call, and never while a financial lock is held. They are never stored in an adapter, client or cache, apart from `DerivedTokenCache`. | §11; `OutboundResolver.Resolve` `txscope` refusal (exists); adapter-field reflection test (§16). |
| **INV-IO-12** | Reconciliation never writes the ledger, a projection, an attempt, an intent or a withdrawal. It writes only run, mismatch and statement-import rows. | Stream code; statement-capture test (§16). |
| **INV-IO-13** | After every failure-injection test: `SUM(debits) == SUM(credits)` and projection == rebuild (LF-C2 #8). | Shared test helper, reused from the existing suites. |
| **INV-IO-14** | Provider binding. Evidence carried by a callback, a status result or a statement line from provider P can change, post against, dispute or match only attempts whose `provider_id = P`. P always comes from the verified identity (route plus verified credential), never from a payload. | The resolution predicate `attempt.provider_id = $verified_provider` in every lookup (§6.1, §6.4, §7, §12.3); cross-provider matches are `anomaly` receipts with no state change (S95-C1). Test plus mutation MX15. |
| **INV-IO-15** | The kill switch cannot be bypassed. A claim (T2, T1p, T12) succeeds only if no engaged switch covers it, evaluated inside the claim statement under the same tenant context. A switch can be released only through an approved four-eyes release request. | `NOT EXISTS` predicate inside the CAS statement (§10.3); `payment_kill_switches_guard` trigger (§13.2). Tests §16.2 item 12. |

---

## 3. Execution pattern

### 3.1 The three phases

```
 phase A (short tx, WithTenant)        phase B (NO tx, NO pooled conn)            phase C (short tx, WithTenant)
 ─────────────────────────────         ──────────────────────────────────         ────────────────────────────────────
 validate, RG/risk, kill switch,       resolve outbound credential (§11)          lock (ADR 0082 order) + CAS attempt
 insert/claim attempt (CAS),           provider-call gate (§3.2)                  applyEvidence (§4.4): state, ledger,
 audit "…requested/claimed"            adapter call with CallContext               withdrawal Complete/Fail, cascade row,
 COMMIT  ◄── durable intent to call    classify result → Evidence | ErrorClass     deferred receipts, audit
                                                                                  COMMIT
```

- Phase A commits **before** phase B starts. If phase A fails, no call is made.
- Phase B runs holding no transaction, no pooled connection, no row lock and no advisory lock.
  Session-level advisory locks are forbidden anywhere on this path.
- Phase C is idempotent and may be retried at any time. The CAS makes a repeat a no-op. For
  example, after a connection lost during COMMIT, re-running phase C finds the state already
  moved.
- If phase C never runs (crash, timeout, deploy), the committed phase-A state plus the sweeper
  (§7) and callbacks (§6) converge the attempt. Nothing depends on the original goroutine
  finishing.

### 3.2 Provider-call gate (one place per domain)

`callProvider(ctx, attempt, fn)` is the only path to an adapter outbound method in payments.
It:

1. **Refuses under `txscope.Held(ctx)`.** It returns `ErrorClassNotSent`, logs
   `provider_call_refused_tx_held`, and does not call. This is defence in depth: the primary
   control is API shape.
2. Refuses a money-moving call unless the attempt was loaded **after** the claim commit, with
   `state = submitting` and a matching `claim_token`.
3. Resolves the credential (§11) and builds `CallContext`. The `CallContext` tenant is taken only
   from the attempt row returned by *this* tenant's claim transaction, never from a payload, a
   cache or an earlier attempt (S95-C9(i)).
4. **Checks the credential binding itself** (S95-C8(b)): `Credential.TenantID == CallContext.TenantID`,
   `Credential.ProviderID == CallContext.ProviderID` and `Credential.Domain == <caller domain>`
   (`payments`, `casino` or `kyc`). A mismatch is `NotSent` with no call. The adapter repeats the
   check (§9.1), so neither check alone is load-bearing.
5. Applies the per-call deadline: `min(ctx deadline, resolve budget + manifest call timeout)`.
6. Calls the adapter.
7. Maps panics and unknown errors to `ErrorClassAmbiguous` for money-moving operations, and to
   `ErrorClassNotSent` only if the adapter provably did not dispatch.
8. **Redacts transport errors** (S95-C8(a)). A transport error (including Go's `*url.Error`,
   which embeds the request URL) is mapped to `ErrorClass` plus an allow-listed reason code
   before it leaves the gate. Where a URL is logged at all, it is logged without its query
   string and userinfo. Vendor response bodies never reach logs, errors, audit or receipts.
9. Feeds the orchestrator-owned breaker (§9.6).

Casino and KYC mirror the SAME gate shape at their own phase-B call sites (LaunchGame;
CreateVerification/SubmitVerification) - steps 1, 3, 4, 5, 6 and 8 - but each as its OWN
independent, duplicated implementation, never through a shared helper: they do not import
payments, and there is no cross-domain gate function either. This was corrected 2026-09-27
(architect review `rv-prh-architect.md`, IO-1B/IO-1C): an earlier revision of this line
claimed "through a small shared helper", which was never true, and additionally overstated
step 1 for casino/KYC specifically - see §15.1.2/§15.2/§15.3 below for the exact record of
when step 1 (the runtime `txscope.Held(ctx)` refusal) actually landed for each domain, and
`internal/txscope`'s own `TestINV_IO_1c_NoAdapterCallInsideTxClosure` for INV-IO-1(c)'s
static-scan proof that no domain's phase-B call site can be reached lexically inside a
database transaction closure regardless of gate-shape parity.

---

## 4. Payment attempt state machine (deposits and payouts)

### 4.1 States

| State | Meaning | Terminal? | Provider may hold it? |
|---|---|---|---|
| `created` | Durable intent to submit. No call that may have reached the provider has ever been made (INV-IO-9). `provider_id` may be NULL (not yet routed; cascade rows). Reached by cascade (T1), by a `NotSent` revert (T5), and by nothing else. | no | **no** |
| `submitting` | Claimed for a provider call. The call may be in flight or done. The provider **may** have received it. | no | yes |
| `pending` | The provider acknowledged receipt and assigned a `provider_reference`; the outcome is not final. **"Accepted" ≡ `pending` with `accepted_at` set.** Provider-neutrally, the platform cannot distinguish "accepted" from "pending", so they are one state. | no | yes |
| `ambiguous` | Outcome unknown: a timeout or transport failure after a possible send, a non-definitive `QueryStatus`, a lookup that is not authoritative, or success evidence without a provider reference. **Never a failure, never a success.** | no | yes |
| `succeeded` | Definite success on verified, amount- and asset-matching evidence that names a provider reference. The ledger effect is posted in the same tx (deposit: Flow 1, linked on `payment_attempts.ledger_transaction_id`; payout: Flow 3 Step B via `withdrawal.Complete`). | yes | yes |
| `declined` | Definite negative from the provider (sync decline, verified decline callback, `QueryStatus` declined, or an authoritative not-found for a deposit, §4.5). This covers the human's "**failed**" (`decline_stage = after_acceptance`) and a provider-side "rejected at submission" (`decline_stage = at_submission`). Terminal for automation. Only verified success evidence can override it (§4.4, anomaly). | yes* | yes |
| `rejected` | Platform-side refusal **before any call that may have reached the provider**. Reached only from `created`: kill switch (deposits), unsupported capability, no routable provider, credential unavailable after retries are exhausted, the interactive presence window expiring, a sibling attempt having succeeded (`intent_succeeded`), or M3. | yes | **no** |
| `disputed` | Contradictory or mismatched evidence that automation must not resolve. Examples: a success with a different amount or asset; success evidence (from the attempt's own provider) for a `rejected` or `created` attempt; a success for a declined payout; a provider-reference conflict; a reversal tombstone that precedes the success (`reversal_tombstone_precedes_success`). It is a P1. Exit is manual only (§4.8). | yes | yes |

\* `declined → succeeded` exists only for deposits, only on verified matching success
evidence, and always raises P1 `contradictory_provider_outcome` (§4.4, T13).
*[AMENDED by §28 (AM-2): `declined → succeeded` is now allowed only as the intent's FIRST
success; otherwise T13d `declined → disputed` (`multiple_success_for_intent`). `disputed` also
covers "matching success for an intent that is already financially resolved" (T10/T13d).]*

Mapping of the human's vocabulary:

| Term | Where it lives |
|---|---|
| request | the intent row plus attempt 1 (T1+T2 in one phase-A tx for the player path; T1 alone for a cascade row) |
| provider submission | `created→submitting` (T2), or the combined player/payout claim |
| accepted | `pending` (`accepted_at`) |
| pending | `pending` |
| succeeded | `succeeded` |
| failed | `declined` (`after_acceptance`); withdrawal `failed` |
| rejected | `rejected`; withdrawal `rejected` via `DenyForCompliance` (pre-dispatch, W-KYC) |
| timeout | call timeout → `ambiguous` (T6); settlement timeout → escalation, **no state change** (T16) |
| ambiguous provider result | `ambiguous` |
| retry | T5 (NotSent), T12 (idempotent resubmit of the same key), player retry resume (§5.1) |
| duplicate callback | receipt dedupe plus no-op evidence (§6.3) |
| callback after timeout | `ambiguous→*` (T7–T9); `declined→succeeded` anomaly (T13) *[AMENDED by §28: T13 first success only, else T13d; the full durable-state definition is §29]* |
| reconciliation | §12 (detection) plus T17 re-verify (operator or the payments re-drive job, §12.5) |
| manual intervention | M1–M3 (§4.8) |

### 4.2 Columns that make the machine deterministic

- `ever_possibly_sent`: set to true, permanently, the first time a call's classification is
  anything other than `NotSent`. Set in phase C, so a `submitting` attempt with the flag still
  false is **not** proof that nothing was sent (CP-W1).
- `submit_count`: bounded by the manifest's `max_resubmits`.
- `claim_token` and `lease_until`: who may call, and until when.
- `first_submitted_at` (set once, by the first claim, from the DB clock) and `last_sent_at` (set
  by every claim, T2/T1p/T12). `first_submitted_at` gates deferred receipts (S95-C3);
  `last_sent_at` measures the not-found window (LF95-C8(b)).
- `next_action_at`: when the sweeper should look. NULL when terminal.
- `last_evidence_kind`, one of `sync | callback | query_status | sweeper | operator | platform |
  legacy`, written in the **same UPDATE** as every state change and read by the guard trigger
  (LF95-C2). The transition's audit record carries the same value. `legacy` exists only on rows
  written by the 0101 backfill (`CHECK (last_evidence_kind <> 'legacy' OR legacy_backfill)`), and
  no application write can produce it (§13.1 INSERT and UPDATE guards).
- `interactive`: from the manifest. True means a player must be present to use the result, for
  example a redirect URL.
- `legacy_backfill`: true only for rows created by the 0101 backfill (§13.1). T12 is forbidden on
  them (trigger), because the provider never received a `pa:<id>` key (LF95-C11(c)).

### 4.3 Transition table (the complete set; anything else is rejected by code and by trigger)

The "Allowed from" column is exactly the CAS predicate. Every transition sets
`last_evidence_kind` and writes its audit record in the same tx. Every tx that changes a
`payment_attempts` row first locks its parent (`deposit_intents` or `withdrawal_requests`) with
`FOR UPDATE`, except the sweeper's lease-only batch claim (§7.2, §14).

| T | From → To | Trigger / evidence | Performed by | CAS guard (in addition to `id = $1`) | Ledger / domain effect (same tx) | Audit action |
|---|---|---|---|---|---|---|
| T1 | ∅ → `created` | Cascade (attempt n+1, §4.6), committed in the tx that commits the previous attempt's decline | The callback, sweeper or player-request tx that commits the decline | INSERT; the partial unique index enforces one live attempt per intent | none | `payment.attempt_created` |
| T1+T2 | ∅ → `submitting` | Player path: `InitiateDeposit` phase A inserts the intent and attempt 1 and claims it in **one** tx, after RG, the KYC deposit gate (ADR 0096) and the in-statement kill-switch predicate | Player-request driver | INSERT with `state='submitting'` guarded by the same predicates as T2 (the kill switch is evaluated in the INSERT … SELECT … WHERE NOT EXISTS statement) | Intent inserted, status `pending` | `deposit.requested`, `payment.attempt_claimed` |
| T1p | ∅ → `submitting` | Payout claim, the **payout KYC hook** (§5.2): withdrawal `approved→submitted` in the **same** tx, after the KYC gate and with the kill-switch predicate in the claim statement | Eligible staff (submit endpoint) | Withdrawal CAS `state='approved'` under L1 `FOR UPDATE`; `UNIQUE(withdrawal_request_id)`; `NOT EXISTS` engaged switch | Withdrawal → `submitted` (`provider_id` set, `provider_reference` NULL) | `withdrawal.dispatch_claimed`, `withdrawal.submit.http` (staff) |
| W-KYC | withdrawal `approved` → `rejected` (**no attempt**) | ADR 0096 `DenyForCompliance`: the payout KYC gate denies in T1p's phase-A tx, before the T1p CAS | System, inside the staff submit request | Withdrawal `state='approved'` under the same L1 lock as T1p, so the two are mutually exclusive by lock | Hold reversal per ADR 0096 (`withdrawal_rejected`, key `<id>:kyc_denied`); no attempt, no call | `withdrawal.rejected_kyc` (ADR 0096) |
| T2 | `created` → `submitting` | Claim for the call. Runs in a **per-item** tx whose order is fixed by ADR 0082 R8 (RV-0095 ledger N1). **Deposit:** first `rg.EvaluateEligibility` (takes the L0.4 RG person advisory) and the KYC deposit gate (plain reads), **then** the parent `FOR UPDATE`, then the attempt, then the CAS (LF95-C10(e)). **Payout:** parent `FOR UPDATE`, attempt, then the payout KYC gate (it takes no lock, so it may follow the row locks), then the CAS (LF95-C10(b)). In both, the tenant capability row is re-read and the kill switch is evaluated inside the CAS statement. | Player-request driver (resume) or sweeper | `state='created' AND (provider_id IS NULL OR provider_id=$p) AND NOT EXISTS(engaged switch, same tenant_id) AND NOT EXISTS(succeeded attempt for the same intent)`; sets `provider_id`, `claim_token`, `lease_until`, `submit_count+1`, `last_sent_at`, and `first_submitted_at` if NULL | none. A non-pass of a gate claims nothing and makes no call: a deposit goes to T3 (`rg_ineligible`/`kyc_required`); a payout stays `created` with a compliance escalation (T16-style, `next_action_at` = the escalated cadence, §7.3, so the sweeper does not re-gate every lease period), and its hold is released only by M3 (`kyc_denied`), never automatically. A later **pass** resumes dispatch through this same T2; that is safe because `created` was never sent, and it is mutually exclusive with M3 by the withdrawal lock (RV-0095 ledger L2). | `payment.attempt_claimed` / `payment.attempt_claim_denied` |
| T3 | `created` → `rejected` | Pre-call refusal (kill switch for deposits, unsupported, no route, credential exhausted, presence window expired, gate deny for a deposit), or `intent_succeeded` (LF95-C6(c)) | Driver, sweeper, or the T7/T13 tx | `state='created'` | Deposit: intent projection recomputed (§5.1). Payout: only via M3 (§4.8). | `payment.attempt_rejected` (+reason) |
| T4 | `submitting` → `pending` | Sync `Pending` with a reference | Phase C | `state='submitting'` | Sets `provider_reference`, `accepted_at`; applies deferred receipts (§6.4); withdrawal `provider_reference` set | `payment.attempt_accepted` |
| T5 | `submitting` → `created` | `ErrorClassNotSent` on a first send (provably not dispatched: credential unavailable, gate refusal, connection refused before write), or a `NotProcessed` code the vendor documents as leaving no trace (§8) | Phase C (same claimant) | `state='submitting' AND claim_token=$t AND NOT ever_possibly_sent` | `next_action_at` = backoff | `payment.attempt_not_sent` |
| T6 | `submitting` → `ambiguous` | Sync `Ambiguous`; `ErrorClassAmbiguous` (timeout after a possible send, reset after write, unmapped 5xx); a `NotProcessed` code that may leave a trace (§8); lease expired and `QueryStatus` not definitive; **`NotSent` on a T12 resend** (`ever_possibly_sent` already true, LF95-C1) | Phase C or sweeper | `state='submitting'`; sets `ever_possibly_sent=true` | `next_action_at` = poll backoff (`now()` for `NotProcessed`) | `payment.attempt_ambiguous` (+cause `timeout`/`not_processed`/`resend_not_sent`/…) |
| T7 | `submitting`/`pending`/`ambiguous` → `succeeded` | Verified success evidence **from the attempt's own provider** with amount = attempt amount, asset = attempt asset, and a non-empty provider reference in the evidence or already on the attempt (callback, `QueryStatus`, or sync where the manifest allows sync success) | Phase C, callback tx or sweeper | `state = ANY('{submitting,pending,ambiguous}')`; trigger requires `last_evidence_kind ∈ {sync, callback, query_status}` *[AMENDED by §28.4: for a deposit, if the intent is already financially resolved (INV-DEP-1), T10 `multiple_success_for_intent` instead, no posting.]* **Deposit:** `ledger.Post` Flow 1 with `provider_tx_id = provider_reference` (idempotent on `(tenant, provider_id, provider_tx_id)`); `payment_attempts.ledger_transaction_id` set; the intent's `ledger_transaction_id` set only while NULL; intent → `succeeded`. If a tombstone already occupies `(provider_id, provider_reference)`, T10 (`reversal_tombstone_precedes_success`) instead, with no posting and no error. **Payout:** `withdrawal.Complete` (Flow 3 Step B). | `deposit.posted` / `withdrawal.completed`, `payment.attempt_succeeded` |
| T8 | `submitting`/`pending`/`ambiguous` → `declined` | Definite decline evidence from the attempt's own provider (sync `Declined`, verified decline callback, `QueryStatus` declined, or a deposit authoritative not-found, §4.5) | Phase C, callback tx or sweeper | `state = ANY('{submitting,pending,ambiguous}')`; trigger: payout requires `last_evidence_kind ∈ {sync, callback, query_status}`, deposit requires `≠ operator` | **Deposit:** cascade row (T1) if eligible (§4.6), else intent projection recomputed. **Payout:** `withdrawal.Fail` (hold released). | `payment.attempt_declined`, `withdrawal.failed` |
| T9 | `ambiguous` → `pending` | Evidence that the provider holds it and has not finished | Callback or sweeper | `state='ambiguous'` | Sets `provider_reference` if it was unknown; applies deferred receipts (§6.4) | `payment.attempt_accepted` |
| T10 | `submitting`/`pending`/`ambiguous` → `disputed` | Mismatched success (amount, asset or reference), a provider-reference conflict (§6.1), or a tombstone preceding the success (LF95-C6(d)) | Any evidence path. The state change is **committed** with its receipt, whatever HTTP code is returned (LF95-C3). | `state = ANY('{submitting,pending,ambiguous}')` | none; P1 | `payment.attempt_disputed` |
| T11 | `pending` → `ambiguous` | Explicit "unknown" or not-found evidence for an accepted attempt (the provider forgot it) | Sweeper | `state='pending'` | none; P1 anomaly | `payment.attempt_ambiguous` |
| T12 | `ambiguous` → `submitting` | Idempotent resubmission of the **same** attempt with the **same** key. Only if the manifest has `IdempotentSubmission=true`, `submit_count < max_resubmits` and `NOT legacy_backfill`. For a payout the per-item tx re-runs the payout KYC gate first; a non-pass means no resend and the attempt stays `ambiguous`, resolvable only by poll or callback (LF95-C10(c)). | Sweeper (per-item tx, parent locked first) | `state='ambiguous' AND submit_count < $max AND NOT legacy_backfill AND NOT EXISTS(engaged switch)`, and for a deposit also `NOT EXISTS(succeeded attempt for the same intent)` (RV-0095 ledger N3: a paid intent is never re-sent; the attempt stays `ambiguous`, resolved by poll or callback only); new `claim_token`, lease, `last_sent_at` | none | `payment.attempt_resubmitted` |
| T13 | `declined` → `succeeded` | **Deposit only.** Verified matching success from the attempt's own provider after a decline, with a provider reference | Callback or sweeper | `state='declined' AND operation='deposit'` | *[SUPERSEDED by §28.4 (AM-2): T13 is the intent's FIRST success only; a matching success on a resolved intent is T13d, no posting. The second-capture and third-capture text below is historical.]* Flow 1 posted to `player_cash` (the money is real; LF-Q1 ruling), linked on the attempt; the intent's link is left alone if already set; intent → `succeeded`; any sibling `created` attempt → `rejected` (T3, `intent_succeeded`); P1 `contradictory_provider_outcome`. If another attempt of the intent is already `succeeded`, also P1 `multiple_success_for_intent`. A sibling already `submitting` is a stated residual under the same P1 (§20). If a tombstone already holds `(provider_id, provider_reference)`: **T13t** instead. | `payment.attempt_succeeded_after_decline` |
| T13t | `declined` → `disputed` | **Deposit only.** Verified matching success after a decline, but a reversal tombstone already holds `(provider_id, provider_reference)` (LF95-C6(d); RV-0095 ledger). | Callback or sweeper | `state='declined' AND operation='deposit'`; `terminal_reason='reversal_tombstone_precedes_success'` | **No posting, no error**: committed terminally, so there is no trigger rejection and no 5xx loop; P1; the attempt sits in the M1 queue. | `payment.attempt_disputed` |
| T14 | `declined` → `disputed` | **Payout only.** Success evidence after `withdrawal.Fail` released the hold (a double payout has already happened) | Callback or sweeper | `state='declined' AND operation='payout'` | none; P1 | `payment.attempt_disputed` |
| T15 | `created`/`rejected` → `disputed` | Success evidence **from the attempt's own `provider_id`** for an attempt the platform never sent (adapter misclassification or a hostile verified sender). Evidence from any other provider, or for an attempt with a NULL `provider_id`, is an `anomaly` receipt with **no** state change (S95-C1). | Callback | `state = ANY('{created,rejected}') AND provider_id = $verified_provider` | **Nothing posted**; P1 | `payment.attempt_disputed` |
| T16 | (no state change) escalation | `pending`/`ambiguous`/`submitting` older than the manifest `SettlementWindow`, or a payout `created` blocked by a gate | Sweeper | `escalated_at IS NULL` | Sets `escalated_at`; poll cadence drops to the escalated rate; alert | `payment.attempt_escalated` |
| T17 | (no state change) re-verify | Operator asks for fresh evidence on any attempt, or the payments re-drive job does so after a `pay_status_mismatch` (§12.5) | Staff with `payments_attempt:reverify`, or the named system principal of the re-drive job | none (sets `next_action_at=now()` only; never calls a provider inline) | Evidence is applied through the matrix like any sweeper poll | `payment.attempt_reverify_requested` |
| M1–M3 | manual | §4.8 | §4.8 | §4.8 | §4.8 | §4.8 |

**Forbidden, and enforced by trigger:**

- any move into `created`, except T5;
- `rejected → *`, except T15;
- `succeeded → *` (a reversal is a separate ledger fact, §5.4);
- `disputed → *`, except M1/M2;
- `declined → disputed`, except T13t (deposit) and T14 (payout); *[AMENDED by §28.4: and T13d
  (deposit, `multiple_success_for_intent`)]*;
- `ambiguous → declined` with `last_evidence_kind='operator'`;
- any `*→declined` for a payout unless `last_evidence_kind ∈ {sync, callback, query_status}`;
- any `→succeeded` unless `last_evidence_kind ∈ {sync, callback, query_status}`;
- T12 when `legacy_backfill` is true;
- any `→rejected` for a payout unless `OLD.state='created' AND NOT ever_possibly_sent` (M3);
- any backward move to `submitting` other than T12.

`DenyForCompliance` (W-KYC) is legal **only from withdrawal `approved`**. Once T1p has
committed, no KYC outcome ever causes `Fail`, a hold reversal or any other automated release;
only T8 evidence or M3 from `created` can release the hold (LF95-C10(d), ADR 0096 C5).

### 4.4 `applyEvidence` — the single evidence matrix

One function, called by phase C, the callback path, the sweeper and T17. Rows are the current
state; columns are the evidence outcome. Every cell is also audited.

**Preconditions checked before any cell is chosen, and before any write** (LF95-C3, S95-C1):

1. The attempt was resolved with `provider_id = verified provider` (INV-IO-14). Otherwise the
   evidence is an `anomaly` receipt with no state change.
2. If the evidence carries both a provider reference and a merchant reference and they resolve
   to **different** attempts, or its provider reference is already bound to another attempt, it
   is an `anomaly` receipt plus P1, with no state change and no posting. This can therefore
   never surface as a unique violation followed by a 5xx redelivery loop.
3. Success evidence with no provider reference, on an attempt that has none either, is treated
   as `ambiguous` evidence plus an immediate poll (LF95-C4). It can never post, because it has no
   ledger key.

| Current \ Evidence | `pending` | `succeeded` (match) | `succeeded` (mismatch) | `declined` | `ambiguous` | `not_found` |
|---|---|---|---|---|---|---|
| `created` | anomaly log, no change | T15 → `disputed` | T15 | anomaly log | no-op | no-op |
| `submitting` | T4 | T7 (tombstone → T10) | T10 | T8 | T6 | §4.5 |
| `pending` | no-op (reschedule) | T7 (tombstone → T10) | T10 | T8 | no-op | T11 |
| `ambiguous` | T9 | T7 (tombstone → T10) | T10 | T8 | no-op (reschedule) | §4.5 |
| `succeeded` | no-op | no-op (duplicate; the ledger is idempotent) | P1 anomaly, receipt `anomaly`, no change | P1 anomaly (a reversal needs a reversal event), no change | no-op | P1 anomaly |
| `declined` | no-op | deposit T13 (tombstone → T13t `disputed`, no posting) / payout T14 *[AMENDED by §28.5: deposit on a resolved intent → T13d]* | P1 anomaly | no-op | no-op | no-op |
| `rejected` | anomaly log | T15 | T15 | no-op | no-op | no-op |
| `disputed` | recorded only | recorded only | recorded only | recorded only | recorded only | recorded only |

- "Tombstone" means a ledger tombstone already holds `(provider_id, provider_reference)` because
  a reversal arrived first. The cell is terminal `disputed` (`reversal_tombstone_precedes_success`,
  P1): T10 from `submitting`/`pending`/`ambiguous`, T13t from a deposit's `declined`. There is no
  posting and no error, never a rollback, 5xx and re-poll loop (LF95-C6(d)).
- Evidence that carries a `provider_reference` different from a non-NULL
  `attempt.provider_reference` is a *reference mismatch*. It is T10 from a non-terminal state,
  otherwise a P1 anomaly.
- **One exception:** a payout **settlement reference** (`CallbackEvent.SettlementReference`)
  may differ by design. It becomes the ledger `provider_tx_id` of Step B, exactly as
  `withdrawal.Complete` allows today.
- **LF-Q1, ruled by ledger-finance (§21.2):** T13 posts to `player_cash`; no suspense account.
  The ruling holds with LF95-C6, which is written into T7/T13 and §5.1/§5.4 above and below.
  *[SUPERSEDED for the multiple-success case by §28.7 (ledger-finance
  `lf-q1-supersession.md`, `17e5ffc`): a second or later matching success for one intent is
  never posted.]*
- *[AMENDED by §28.5: the `submitting`/`pending`/`ambiguous` × `succeeded (match)` cells read
  "T7 (tombstone → T10; intent already resolved → T10 `multiple_success_for_intent`)".]*
- *[AMENDED by §36 (PRH-2 D) for the **poll** (`QueryStatus`) source of evidence: "match" in the
  `succeeded` columns is decided by the poll's own amount, asset and echoed reference, in this order:
  amount/asset mismatch → T10 `poll_amount_mismatch`; a **non-empty** echoed reference that differs from
  the attempt's bound reference → T10 `poll_reference_mismatch` (an empty echo is allowed); reference bound
  elsewhere (attempt, intent or non-tombstone ledger transaction) → T10 `provider_reference_conflict`;
  tombstone on the **bound** reference → T10; then INV-DEP-1; then the posting, keyed on the **bound**
  reference, never on the echo. A success with no usable amount evidence ("Missing") is none of these:
  nothing is posted, the attempt stays live and is re-polled (§36.2). For a fresh attempt state of
  `declined` the contradiction cells are audit-only (no live→disputed transition exists), and every
  non-success poll result on a state that is no longer `submitting`/`pending`/`ambiguous` is a no-op.]*

### 4.5 Deposit/payout asymmetry (binding)

- **Deposit failure is money-safe to assume; a late success still posts (T13).** *[AMENDED by
  §28: only if it is the intent's first success; otherwise T13d, no posting.]* On
  `not_found`, a deposit moves to `declined` (`decline_stage=at_submission`,
  `reason=not_received`, **`cascadable=false` enforced in code**) only if **all** of these hold
  (LF95-C8):
  - (a) `accepted_at IS NULL AND provider_reference IS NULL`: the provider never acknowledged the
    attempt. Otherwise `not_found` is T11/`ambiguous`.
  - (b) the manifest declares `MerchantLookupAuthoritativeAfter = Δ` and
    `now() − last_sent_at > Δ`, measured from the **latest** send;
  - (c) registration has already refused any manifest with
    `MerchantLookupAuthoritativeAfter < lease + CallTimeout` (§10.1), so a lookup cannot outrun
    a call still in flight.

  Otherwise, or when there is no merchant-reference lookup at all, it moves to `ambiguous`,
  and T12 applies if the manifest allows it.
- **Payout failure is never assumed.** Releasing a hold for a payout that later lands is a
  double payout.
  - `not_found`, timeout, ambiguous results and KYC outcomes always leave the payout
    unreleased.
  - The only automated exits are T7 (success evidence), T8 (definite decline evidence) and T12
    (idempotent resubmission, whose result is itself evidence).
  - Everything else is escalation (T16), then M2 (four-eyes, currently BLOCKED). M3 applies only
    to `created`.

### 4.6 Cascade (deposits only) — via the attempt row, never inline in a webhook

A decline is **cascade-eligible** when all of these hold:

- `cascadable = true` on the evidence. For a deferred receipt, the persisted `cascadable` column
  is used; a receipt without it can never cascade (LF95-C4);
- the decline is not a §4.5 authoritative not-found (always `cascadable=false`);
- `attempt_no < MaxCascadeDepth`;
- the intent is not `succeeded`;
- no other live attempt exists;
- the kill switch is not engaged for the next candidate (re-checked at T2);
- and either (a) the decline arrived **synchronously on the player-request path**, or (b) the
  attempt is `interactive = false`.

When eligible, the tx that commits T8 also inserts attempt n+1 (T1, `provider_id NULL`,
`excluded_provider_ids = all previous attempts' providers`). This closes the
`providerExclusionSoFar` limitation. Case (a) is driven immediately by the same request's
driver (its own T2 per-item tx). Case (b) is driven by the sweeper.

An **asynchronous** decline of an **interactive** attempt finalizes the intent as `declined`.
The redirect URL of a new provider could never reach the player, which is the latent defect of
today's webhook cascade. The player retries with a new idempotency key.

This amends payment-orchestration.md §5 (recorded above). A cascade **never** follows
`ambiguous`, `not_found` or a timeout (LF-C2 #5).

### 4.7 Withdrawal state mapping

| Withdrawal state (unchanged set) | Payout attempt state | Notes |
|---|---|---|
| `approved` | none | The KYC gate may take W-KYC (`approved→rejected`) here and only here. |
| `rejected` | none | Staff `Reject`, or W-KYC `DenyForCompliance` (ADR 0096) |
| `submitted` | `submitting` / `created` (NotSent retry, or blocked by a gate at T2) / `pending` / `ambiguous` / `disputed` | New meaning: "dispatch claimed; the provider may hold it". `provider_reference` is NULL until T4/T9/T7. The resolve handler's "structurally unreachable" check (`withdrawal_handlers.go:1034`) becomes a normal case (query by merchant reference). |
| `completed` | `succeeded` | Via T7 → `withdrawal.Complete` in the same tx |
| `failed` | `declined`, or `rejected` via M3 | Via T8 → `withdrawal.Fail`, or M3 |

`withdrawal.MarkSubmitted` is split into:

- `ClaimForDispatch(tx, id, providerID)`: `approved→submitted`, no reference;
- `RecordProviderReference(tx, id, ref)`: sets it once, NULL→value.

`Reject`/`Cancel`/`DenyForCompliance` from `approved` race the claim on the same L1 row, and the
CAS guarantees exactly one wins. **No payout cascade** (current behaviour, kept): a declined
payout fails the withdrawal.

`docs/architecture/withdrawal-state-machine.md` (lines 92–93 today say the provider fields are
"set on `approved` → `submitted`") is updated to these semantics by `payments` in I1-f, before
PRH-I1 is marked complete (P95-C2).

### 4.8 Manual intervention (only where genuinely necessary)

| M | What | When it is the only option | Governance | Status |
|---|---|---|---|---|
| M1 | Resolve a `disputed` **deposit** attempt: attach a compensating ledger transaction, or record "no ledger action", with evidence | Contradictory or mismatched evidence | Via LEDGER-MANUAL-ADJ-4EYES-1 (four-eyes above a threshold) | **BLOCKED** on LEDGER-MANUAL-ADJ-4EYES-1. Until then the attempt stays `disputed`, is visible, and is a P1. |
| M2 | Force-resolve an `ambiguous`/`disputed` **payout** (declare paid → `Complete`, or declare not-paid → `Fail`) without provider evidence | The provider cannot be queried, has no idempotent resubmission, is past the horizon, and reconciliation has not resolved it | Four-eyes; threshold per **HD-0095-1** | **BLOCKED** on HD-0095-1 and LEDGER-MANUAL-ADJ-4EYES-1. No API is built. |
| M3 | Abandon a payout attempt in `created` (never sent, INV-IO-9) → `rejected`, then `withdrawal.Fail` in the same tx | The credential or provider is permanently gone, **or** a KYC deny was found at a T2 re-claim (reason `kyc_denied`; ADR 0096's T2-reclaim case routes here, never to a second `DenyForCompliance`) | Staff with today's resolve eligibility (linked, active) plus a mandatory reason code, audited with IP/UA. **Never automated.** The CAS requires `state='created' AND NOT ever_possibly_sent` in SQL and in the trigger (S95-C13). **No double-payout risk**, because `created` has provably never been sent. | Designed here; built in PRH-I1. |

The operator re-verify (T17) and kill switch (§10) are not "manual resolution". They only
fetch evidence or stop new activity.

---


## 5. Per-operation specification

All operations share these properties. Idempotency is DB-enforced. Audit goes into
`audit_log` in the same tx as the effect. Nothing is authoritative in Redis.

### 5.1 Deposit (priority; fully specified)

| Aspect | Specification |
|---|---|
| Intent | `deposit_intents` row (unchanged table, status set unchanged). Its status is a projection of its attempts, updated in the same tx as every attempt transition, evaluated in this order: any `succeeded` → `succeeded` (sticky); any `disputed` (and no `succeeded`) → `ambiguous`, never `declined`, because funds may have been captured (LF95-C7); a live attempt that is `ambiguous` → `ambiguous`; any other live attempt → `pending`; none live → `declined`. `failed` stays unused. RG denial keeps today's behaviour: intent → `declined` with `rg_ineligible:*` and no attempt, in phase A. A KYC deposit deny (ADR 0096) is the same shape. |
| Ledger link (LF95-C6(a)) | Every deposit posting is linked on `payment_attempts.ledger_transaction_id`. `deposit_intents.ledger_transaction_id` is written only while it is NULL (the migration 0082 trigger already forbids repointing it), so it keeps pointing at the **first** posting. Bonus deposit detection (`internal/bonus/deposit_sweep.go`) therefore sees only the first capture; `bonus-engine` confirms that is intended (LF95-R2, §20). *[AMENDED by §28: there is now at most one deposit posting per intent (INV-DEP-1), so the intent link and the succeeded attempt's link always name the same posting.]* |
| Provider reference | `payment_attempts.provider_reference`: set once by T4, T7 or T9, bounded by `PROVIDER_REF_MAX` (0099), and unique per `(tenant, provider_id)`. `deposit_intents.provider_id/provider_reference` keep mirroring the latest routed attempt for existing readers; `TestMigration0082_DepositIntentsProviderColumnsStayMutable` must keep passing, and a new test asserts the mirror through a cascade (P95-C1). |
| Idempotency keys | **Player:** `UNIQUE(tenant, player, idempotency_key)` on the intent (exists). A retry **resumes**: it returns the intent; if its live attempt is `created`, the retry drives it (T2 CAS in a per-item tx that first re-runs RG and the KYC deposit gate, then locks the intent and attempt, per ADR 0082 R8; concurrent retries yield exactly one claimant and a stale eligibility is never reused); if `submitting`/`pending`/`ambiguous`, it returns the status (the redirect URL is not persisted; it is re-obtained only by T12 when `IdempotentSubmission`). **External:** `external_idempotency_key = "pa:" + attempt.id`, `merchant_reference = attempt.id` (INV-IO-3). **Ledger:** `(tenant, provider_id, provider_tx_id = provider_reference)` plus `idempotency_key = provider_id:provider_reference` (exists). |
| Flow (player request) | Phase A0 (read-only tx): load candidates → health filter **outside** the tx (§9.6) → pick a provider. Phase A (**one** tx): RG check and the ADR 0096 KYC deposit gate (a deny commits the intent as `declined` with no attempt) → insert intent plus attempt 1 directly in `submitting` (T1+T2, kill-switch predicate inside the INSERT statement) → commit. There is therefore no player-path `created` row; `created` exists only for cascade rows and NotSent reverts (LF95-C12, CP-D1). Phase B: resolve credential, `Deposit(CallContext, req)`. Phase C: `applyEvidence` (T4/T6/T8/T7), with a synchronous cascade loop (§4.6), each step its own A/B/C. The handler no longer wraps the call in `WithTenant`; `InitiateDeposit` takes `*db.Pool` (INV-IO-1a). |
| State / retryability | §4.3. `NotSent` → T5 (the driver retries within its request budget, else the sweeper takes over if non-interactive; for an interactive attempt the player sees "temporarily unavailable" and the attempt expires via T3). `Ambiguous` → T6, never cascaded, T12 only if the manifest allows it. |
| Callback | §6. Resolution by `(provider_id, provider_reference)` **or** `merchant_reference`, both bound to the verified provider (INV-IO-14). Accepted in `submitting`. |
| Reconciliation | §12: `pay_*` kinds keyed by `(provider_id, provider_reference)` with a merchant-reference fallback (same provider only), plus the ledger join (§12.3, LF95-C13). |
| Failure recovery | Crash points CP-D1..CP-D8 (§16.1), each converged by CAS phase C, callbacks or the sweeper. |
| Audit | `deposit.requested`, `payment.attempt_*` (every transition, with `last_evidence_kind`, provider id, reference, `error_class`, credential **fingerprint and handle id only**), `deposit.posted`, `deposit.declined`, `payments.deposit_denied_by_rg` (exists). |

### 5.2 Withdrawal payout dispatch (LF-C2 #9)

| Aspect | Specification |
|---|---|
| Intent | `withdrawal_requests` (existing four-eyes approval flow unchanged) plus exactly one payout attempt (INV-IO-8). |
| Provider reference | The attempt's `provider_reference` (the instruction reference), copied once into `withdrawal_requests.provider_reference`. A settlement or confirmation reference may differ (Flow 3) and is the Step B `provider_tx_id`. |
| Idempotency keys | Staff double-submit: the L1 `FOR UPDATE` plus the CAS `approved→submitted` plus `UNIQUE(withdrawal_request_id)` on attempts, so the second caller gets `ErrStateConflict` **before any call**. External: `"pa:" + attempt.id`. Ledger: Step B `provider_id:provider_tx_id` (exists); Fail `request_id:failed` (exists). |
| Flow | Phase A0 (read-only tx, no lock): load the request plus candidates → health outside the tx → route. Phase A (one tx; LF95-C10(a)), in this exact order: (1) approver eligibility (exists); (2) L1 `withdrawal_requests FOR UPDATE`, `state='approved'`; (3) **the payout KYC gate (ADR 0096)**: on deny, `DenyForCompliance` (W-KYC, `approved→rejected`, hold reversal `<id>:kyc_denied`) commits, no attempt is created and no call is made; (4) capability re-read; (5) **T1p**: withdrawal `approved→submitted` plus the attempt INSERT in `submitting`, with the kill-switch `NOT EXISTS` predicate inside the claim statement (an engaged switch claims nothing, the tx rolls back to a 503, and the withdrawal stays `approved`) → commit. **T1p is THE payout KYC hook**: the last commit before the first `Withdraw` for that request; KYC deny and T1p both require `state='approved'` under the same L1 lock, so they are mutually exclusive by the lock, not by convention. Phase B: resolve credential (NotSent on failure → T5, and the sweeper retries unattended because payouts are non-interactive), `Withdraw`. Phase C: L1 lock (withdrawal) → attempt → `applyEvidence` (T4/T6/T7→`Complete`/T8→`Fail`). |
| Retryability | NotSent → T5, then the sweeper re-claims (T2) while the kill switch is released. The T2 re-claim runs in a per-item tx that locks the withdrawal first and **re-runs the payout KYC gate**; on a non-pass it claims nothing, makes no call, leaves the attempt `created` with a compliance escalation, and the hold is released only by staff M3 (`kyc_denied`), never automatically (LF95-C10(b); ADR 0096's T2-reclaim case). Ambiguous → never resubmitted except T12, which also re-runs the gate (LF95-C10(c)). Never re-routed. After T1p, no KYC outcome releases the hold (LF95-C10(d)). |
| Callback | New canonical event `payout` (§9.3), handled by the same receipt/resolution path. `payout_returned` (after completion) is declared **unsupported** (NOT IMPLEMENTED): receipt `unsupported_event`, P1, verified-malformed 4xx class. |
| Reconciliation | §12, line kind `payout`. |
| Failure recovery | CP-W1..CP-W6 (§16.1). The existing staff resolve endpoint becomes: read (no lock) → `QueryStatus` outside the tx → phase C. It is T17-equivalent evidence, never a resubmission. |
| Audit | `withdrawal.dispatch_claimed`, `withdrawal.submit.http` (staff, IP, UA; exists), `payment.attempt_*`, `withdrawal.completed`/`withdrawal.failed` (exist), `withdrawal.resolve_attempted.http` (exists). |

#### 5.2.1 Payout KYC gate placement (ADR 0096; LF95-C10; IC conditions 3–4)

The gate is the ADR 0096 enforcement function exported by `internal/kyc` (being implemented in
PRH-I3), called with operation `withdrawal_payout`. This ADR names it only generically.

| Point | Where the gate runs | On allow | On deny / non-pass |
|---|---|---|---|
| **T1p claim tx (THE payout KYC hook)** | After the L1 `withdrawal_requests FOR UPDATE` (`state='approved'`), before the `approved→submitted` claim | T1p proceeds | `withdrawal.DenyForCompliance` in the **same** tx (W-KYC, `approved→rejected`, hold reversal `<id>:kyc_denied`), no attempt row, no call. Mutually exclusive with T1p by the L1 lock. |
| **Sweeper re-claim T2** (payout attempt `created` after T5; withdrawal already `submitted`) | Per-item tx, after locking the withdrawal and the attempt, before the T2 CAS | T2 proceeds | No claim, no call; the attempt stays `created` with a compliance escalation. The hold is released **only** by staff M3 (`kyc_denied`), which is legal because `created` is provably never sent (INV-IO-9). `DenyForCompliance` is never applied a second time (it requires `approved`). |
| **Resubmission T12** (attempt `ambiguous`; withdrawal `submitted`) | Per-item tx, after locking the withdrawal and the attempt, before the T12 CAS | T12 proceeds | No resend. The attempt is **not** M3-eligible (it may have been sent), so it stays `ambiguous` and is resolved only by poll or callback evidence. |
| **After possible dispatch** (`submitting`/`pending`/`ambiguous`/`disputed`, or `ever_possibly_sent`) | — | — | **No KYC outcome ever triggers `Fail`, a hold reversal or any automated release.** Only T8 evidence (or M3 from `created`) releases the hold; a post-dispatch KYC finding is a compliance case, not a ledger action. |

### 5.3 Status query (`QueryStatus`)

| Aspect | Specification |
|---|---|
| Intent | No row of its own. It always concerns an existing attempt. |
| Reference | `StatusQuery{ProviderReference, MerchantReference}`. The adapter uses the provider reference when known, and the merchant reference only if the manifest has `StatusQuery = by_provider_or_merchant_reference`. |
| Idempotency | Read-only at the provider. Concurrent duplicates are harmless; the lease prevents waste. |
| State | Never changes state itself. Its `StatusResult` is evidence for `applyEvidence`. |
| Retryability | Always retryable: `NotSent`/`Ambiguous` → reschedule with backoff. |
| Callback | n/a |
| Reconciliation | The poll outcome is recorded on the transition audit only when it changes state or escalates. Polls are not receipted (volume). |
| Failure recovery | Sweeper backoff, then escalation (T16). |
| Audit | Only on a state change, escalation or anomaly. |

### 5.4 Deposit reversal (inbound: refund or chargeback initiated by the provider)

| Aspect | Specification |
|---|---|
| Intent | None. The reversal is a provider-initiated fact about a `succeeded` deposit attempt. The attempt state does **not** change: the reversal is the ledger's `reverses_transaction_id` fact (Flow 2; PAY-REV-1 L2 lock unchanged). |
| Reference | The reversal's own `provider_reference` plus `original_provider_reference`. The original is resolved through `payment_attempts (provider_id = verified provider, provider_reference = original_provider_reference)` → **the attempt's** `ledger_transaction_id`, never the intent's; otherwise a reversal of a second capture (T13) would reverse the first (LF95-C6(b)). No matching attempt, or an attempt with no posting, takes the existing tombstone branch. *[AMENDED by §28.6: a second capture is no longer posted, so a reversal naming a T13d/T10 `multiple_success_for_intent` attempt always takes the tombstone branch (no ledger effect). The attempt-own-link rule stays as defence in depth.]* |
| Idempotency | Existing ledger uniqueness plus INV-PAY-REV-1 (migration 0092) plus the receipt dedupe. |
| Callback | The same receipt path. An unseen original still writes the tombstone (exists). |
| Reconciliation | Line kind `deposit_reversal`. |
| Failure recovery | Unchanged: 5xx → redelivery. LF-C1 applies (§6.6). |
| Audit | `deposit.reversed`, `deposit.reversal_tombstoned`, `deposit.reversal_rejected` (all exist). |

### 5.5 Refund / reversal initiated by the platform (outbound)

`NOT IMPLEMENTED`, and not built by PRH. No platform flow initiates a refund today.

- The contract reserves `Refund(ctx, CallContext, RefundRequest)`, and the manifest has
  `supports_refund = false` for every adapter. Registration refuses an adapter declaring
  `true` until a flow exists. A call fails closed with `ErrCapabilityUnsupported`.
- When it is built, it is a third `operation = 'refund'` on `payment_attempts`. It has the
  same states and payout-style asymmetry (§4.5), because refunding money out is a payout.

### 5.6 Casino launch and KYC create/submit

These are specified in §15, with minimal redesign.

---

## 6. Callbacks

### 6.1 Order (the domain step of ADR 0097's pipeline; phase 1 and phase 2 of ADR 0094 unchanged)

This all happens in one domain tx (`WithTenant`), with **no provider I/O**. The verified
provider `P` is the route-resolved provider whose credential verified the callback; it is
never read from the payload.

1. **Redeem** (exists). A DB/transport failure of the post-verification re-check returns the
   typed `RecheckUnavailableError` → retryable 503 (§6.6, S-Q3); every definitive re-check
   result stays the uniform 401.
2. **`HandleCallback`** (exists).
3. **PROVIDER-REF-BOUND-1 boundary validation** (0099; a deterministic non-retryable
   rejection, before any write).
4. **Resolve the attempt, read-only and bound to `P`** (INV-IO-14, S95-C1, LF95-C3):
   - (a) by `(tenant, provider_id = P, provider_reference)`;
   - (b) by `(tenant, merchant_reference)` **with `attempt.provider_id = P`**, if the event
     carries one;
   - if (a) and (b) both resolve and name **different** attempts, or (b) matches an attempt
     whose `provider_id` differs from `P` or is NULL, or the evidence's provider reference is
     already bound to another attempt, the event is an **`anomaly`** (P1): it is receipted and
     changes nothing, and T15 is **not** taken;
   - (c) if nothing resolves, the event is **unresolved**.
5. **Deferred cap probe** (unresolved only; S95-C2(i)): count unapplied receipts for
   `(tenant, P)` with `LIMIT cap+1` on the partial index (`RECOMMENDATION` cap 10 000). Above
   the cap the tx stores nothing and returns a retryable **503** plus a P1 alert. It never
   returns a 200 without storing, and never a 404.
6. **Receipt insert (R0; the first write of the tx, §14).** `INSERT … ON CONFLICT (tenant_id,
   provider_id, event_fingerprint) DO NOTHING RETURNING id`. On conflict the event is a
   **duplicate delivery**: evidence is still applied (idempotently), but no new receipt row is
   written. The receipt persists every field `applyEvidence` consumes, including `cascadable`,
   `decline_stage` and a bounded, allow-listed `decline_reason` code (LF95-C4).
7. **Lock** in ADR 0082 order (§14): parent (`deposit_intents` or `withdrawal_requests`) `FOR
   UPDATE`, then the attempt. Re-read the attempt under the lock, then `applyEvidence` (§4.4,
   including its preconditions).
8. **Set the receipt's one-shot resolution columns** (`attempt_id`, `resolution`,
   `resolved_at`), or leave it unresolved (`deferred_unresolved`).
9. Commit, then return the response from §6.2.

### 6.2 Dispositions and HTTP mapping

**Every 200 has a byte-identical body**, apart from the request id (S95-C4). The disposition
is recorded only in the receipt, the audit record and metrics, so a verified sender cannot
learn whether a reference exists.

| Disposition | Condition | Response |
|---|---|---|
| `applied` | The evidence changed state or posted | 200 (uniform body) |
| `duplicate_effect` | The receipt already existed, or the evidence was a no-op for the current state | 200 (uniform body; unchanged replay semantics) |
| `deferred_unresolved` | Verified, but no attempt is resolvable yet (the callback raced phase C before the reference was known, and there is no merchant-reference echo), and the §6.1 step 5 cap is not exceeded | **200 after durable receipt** (INV-IO-10). This changes today's 404 for this case only. |
| `anomaly` | A precondition failure of §4.4 (cross-provider, cross-attempt reference conflict), a P1 cell of §4.4, or a mismatched success (T10) | 200 (uniform body) after durable receipt plus alert. **The state effect (T10 `disputed`, or none) is committed together with the receipt; it is never rolled back to produce an error code** (LF95-C3). This supersedes today's rollback-and-409 for `ErrCallbackProviderMismatch` on the deposit-success path. |
| typed reversal rejections | `ErrCallbackPayloadMismatch`, `ErrDepositAlreadyReversed` on the reversal path (no attempt state effect exists for them) | Unchanged: 409, with the existing separate-tx evidence record (`RecordDepositReversalRejection` pattern). The separate tx takes at most one R0 insert as its first write (§14). |
| `unsupported_event` | A verified event type the manifest does not support | Existing verified-malformed 4xx class; receipt written in the separate-tx pattern |
| cap exceeded | §6.1 step 5 | Retryable 503 (ADR 0097 §6.1 generic body, `Retry-After`) plus P1; nothing stored |
| re-check unavailable | §6.1 step 1, `RecheckUnavailableError` | Retryable 503 (ADR 0097 §6.1 generic body); nothing written |
| (none) | DB error or a rollback of any kind, including a DB CHECK reached at insert (which means the application bound and the DB bound disagree: 5xx **plus alert**, S95-C2(iii)) | 5xx, which is retryable; nothing is committed; the provider redelivers (LF-C1, §6.6) |

### 6.3 Duplicate and concurrent callbacks

Two concurrent deliveries of one event serialize on the receipt-key insert (R0) first. The
second waits, then conflicts, then applies idempotently. They then serialize on the parent and
attempt locks and the ledger unique constraint. The result is exactly one posting (INV-IO-13
test).

Two *different* events for one attempt (for example, a decline and then a success) serialize on
the parent lock, and the matrix decides.

### 6.4 Callback before or after the internal transition

| Race | Outcome |
|---|---|
| Callback arrives while the attempt is `submitting` (phase C not yet committed) | With a merchant-reference echo, it resolves by merchant reference (same provider only) and T7/T8 applies. Phase C later finds `succeeded`/`declined`, and its CAS from `submitting` fails. The late sync result is then passed to `applyEvidence` against the new state and is a no-op or anomaly (reference cross-checked). |
| The same race without a merchant-reference echo | `deferred_unresolved`. Phase C's T4/T9 (which sets `provider_reference`), **in the same tx and holding the parent and attempt locks**, selects deferred receipts for `(provider_id = attempt.provider_id, provider_reference)` and applies them in ascending receipt id (LF95-C9(b)). A receipt is applied **only if `received_at >= attempt.first_submitted_at`** (both from the DB clock); an earlier receipt cannot describe this submission, so it is resolved as `anomaly_predates_submission` and alerts, and is never applied (S95-C3). The sweeper backstop does the same, locking the parent and attempt first. Both orders converge to the same final state. |
| Callback after timeout (the attempt is `ambiguous`) | T7/T8/T9 per the matrix. This is the normal resolution path. |
| Callback after the sweeper declined a deposit on an authoritative not-found | T13 (post plus P1). *[AMENDED by §28: only if the intent is not already resolved; otherwise T13d, no posting.]* |
| Callback for an attempt the platform never sent, from that attempt's own provider | T15 (no post, P1). From any other provider: `anomaly`, no change. |
| A deferred receipt is never claimed | After the manifest `SettlementWindow` (24 h if undeclared) it raises a P1 "unmatched verified callback" and is reported by the §12 stream as `pay_unresolved` (LF95-C5, S95-C2(ii)). It never creates an attempt. |

### 6.5 What a callback may never do

- Call a provider: no cascade I/O, no `QueryStatus`.
- Hold a transaction longer than its own DB work.
- Trust a payload tenant or a payload provider.
- Touch an attempt of a provider other than the verified one (INV-IO-14).
- Create an attempt, except the cascade row (T1) committed with a decline.
- Post a ledger entry for anything other than matching verified success (with a provider
  reference) or reversal evidence.

`handleDecline → attemptDeposit` and `resolveAmbiguous` are removed from the callback path. An
ambiguous-outcome callback sets `next_action_at = now()` and nudges the sweeper.

### 6.6 LF-C1 (401 and 5xx redelivery) and webhook retry semantics, carried into the manifest

Every adapter manifest declares `RedeliveryOn401` and `RedeliveryOn5xx`, each one of
`redelivers | terminal | unknown`, as recorded from vendor intake #28, and the mandatory
`WebhookRetrySemantics` (§10.1, P95-C3, ADR 0097 §19 condition 2).

**Option (b), enforced structurally.** At startup, a **production-eligible** adapter with
`terminal` or `unknown` for 401 is refused unless a production-eligible
`PaymentStatementSource` is registered for the same provider id. The §12 stream reports
"provider-settled, platform-unposted" as `pay_status_mismatch`/`pay_missing_platform_record`,
which are P1.

**Option (a), approved narrowly by `security` (§22.3) and additive to (b).** A **DB or
transport failure** during the post-verification re-check (`HandleRecheckSQL` inside
`ReceiveVerifiedCallback`, ADR 0094 §5) returns a retryable 503 instead of the uniform 401:

1. Only a connection error, a serialization or statement failure, or a context deadline at the
   re-check qualifies. It is classified by a typed `RecheckUnavailableError`, distinct from
   `AuthError`, never from error strings. A definitive result (handle not found, not active,
   revoked, expired, fingerprint or key-id mismatch) stays the uniform 401 with
   `credential_unavailable`. **Anything unclassified defaults to 401.**
2. The 503 uses ADR 0097 §6.1's generic body and headers (with `Retry-After`); no reason text.
3. Nothing is written: the domain tx rolls back. `TestReceiveVerified_RecheckDBErrorRollsBack`
   is extended to assert the 503 as well as the zero rows.
4. A metric plus an alert on the re-check-unavailable rate per (domain, tenant, provider).
5. Option (b) still applies to every production-eligible adapter with `RedeliveryOn401 ∈
   {terminal, unknown}`.
6. It applies to all three webhook domains through the shared `ReceiveVerifiedCallback` error
   typing (payments in PRH-I1; casino and KYC in PRH-I2). It does **not** extend to
   pre-verification DB errors, which stay the uniform 401.

---


## 7. Outbox and sweeper (bounded work)

### 7.1 Work set

These are `payment_attempts` rows with `next_action_at <= now()`:

| State | Sweeper action |
|---|---|
| `created`, `interactive = false`, or any payout | route if `provider_id IS NULL` (A0) → per-item tx in the T2 order of §4.3 (deposit: RG + KYC deposit gate **before** the parent and attempt locks; payout: parent and attempt locks, then the payout KYC gate), T2 with the in-statement kill-switch predicate → commit → resolve credential → call → C. A payout gate non-pass leaves the attempt `created` with a compliance escalation (M3 only). |
| `created`, `interactive = true`, older than `presence_window` | T3 (`expired_before_submission`) |
| `submitting`, `lease_until < now()` | `QueryStatus` (§5.3) → matrix; `not_found` → §4.5 |
| `pending` | `QueryStatus` → matrix; still pending → backoff; past the horizon → T16 |
| `ambiguous` | `QueryStatus` → matrix; or T12 if the manifest allows it; else backoff/T16 |

Deferred receipts whose `(provider_id, provider_reference)` now resolves **to an attempt of the
same provider** are also re-applied (with the `received_at >= first_submitted_at` rule, §6.4),
bounded by the same per-tenant batch, in a per-item tx that locks the parent and the attempt
first. Deferred receipts still unresolved after the `SettlementWindow` raise the §6.4 P1 once
(LF95-C5, S95-C2(ii)).

### 7.2 Claim protocol

1. **Tenant iteration.** Active tenants in round-robin order, as the reconciliation scheduler
   does (`activeTenantIDs`). Each tenant is handled in its own `WithTenant` tx.
2. **Batch lease tx (short; the sole exception to "parent first", LF95-C9(d)).** `SELECT … FROM
   payment_attempts WHERE next_action_at <= now() ORDER BY next_action_at LIMIT $batch FOR
   UPDATE SKIP LOCKED`. For each row it writes **only** `lease_owner`, `lease_until = now() +
   lease` and `next_action_at = lease_until`, so a crashed worker's items reappear. It never
   changes a state, never reads or locks a parent row and never waits on a lock (SKIP LOCKED
   only). Commit.
3. **Per-item claim tx (state-changing items).** For `created` (T2/T3) and `ambiguous` (T12)
   items. The order follows ADR 0082 R8, because advisory locks precede row locks:
   - (deposit only) run RG (L0.4) and the KYC deposit gate;
   - lock the parent, then the attempt;
   - re-check the lease owner;
   - (payout only) run the payout KYC gate;
   - run the CAS with its in-statement predicates → commit. For polls no claim tx is needed.
4. **Calls.** Each item runs outside any tx, bounded by: a global concurrency cap; a
   per-(tenant, provider) concurrency cap; a per-call deadline shorter than `lease / 2`; and
   the breaker (§9.6).
5. **Phase C per item.** It locks the parent, then the attempt, applies evidence, clears the
   lease, sets `next_action_at` = backoff (exponential with jitter, capped) or NULL when
   terminal, and bumps `poll_count`.
6. **Identity.** The sweeper runs under the ordinary application role with RLS (never
   BYPASSRLS), and its audit actor is a named system principal (S95-C9(ii)).

### 7.3 Bounds and defaults

`RECOMMENDATION` defaults. All of these are configuration; `payments` and `qa` tune them.

| Parameter | Value |
|---|---|
| Sweep tick | 15 s, plus an in-process nudge channel for immediate work (for example after an ambiguous callback) |
| Batch per tenant per tick | 20 |
| Global concurrent calls | 16 |
| Per-(tenant, provider) concurrent calls | 4 |
| Lease | 60 s. It must exceed resolve budget + call timeout + phase C, and it must exceed 2 × call timeout. |
| Poll backoff | Base 30 s, cap 30 min |
| Resolution horizon | Manifest `SettlementWindow`; 24 h if undeclared |
| Unapplied-receipt cap per (tenant, provider) | 10 000 (§6.1 step 5) |
| Escalated poll cadence | 6 h |
| Interactive presence window | 30 min |
| `max_resubmits` | 3 |
| `MaxCascadeDepth` | 3 (exists) |

**Work is bounded per tick** by (tenants × batch) and by the concurrency caps. **Fairness:**
one tenant's slow provider consumes at most its per-(tenant, provider) slots and never a
pooled connection (no connection is held during calls; the F-POOL-1 lesson). **Idle cost:**
one indexed range scan per tenant per tick (`idx_payment_attempts_due`).

### 7.4 Why at-most-one concurrent money call per attempt

A money-moving call requires the claimant to hold `state = submitting` with its `claim_token`.
Entering `submitting` is T1+T2, T1p (guarded inserts), T2 or T12 (CAS from `created`/`ambiguous`). The lease only
governs *re-polling*: an expired `submitting` lease leads to `QueryStatus`, never to a second
submission. The one exception is T12, which re-sends the **same key**, and only when the
provider deduplicates it.

---

## 8. Error, retry and timeout classification

The adapter maps every outcome of an outbound call to exactly one of:

| `ErrorClass` / outcome | Meaning | Source (typical) | State effect | Automatic retry |
|---|---|---|---|---|
| `NotSent` | Provably never dispatched | Credential unavailable; gate refusal; `httpclient` `Sent=false` (build error, dial refused before write, breaker open) | First send (`ever_possibly_sent=false`): T5. **On a T12 resend** (`ever_possibly_sent` already true): T6, back to `ambiguous`, never T5 (LF95-C1) | Yes, same attempt, first send only (INV-IO-9) |
| `NotProcessed` | Dispatched, but the vendor **documents** that this response means "not processed" (for example a specific 4xx or 429). Each code cites its vendor-documentation source in the intake (S95-C12(ii)). | The vendor error model (intake #20). **Adapter-declared per code.** Anything undocumented is `Ambiguous`. | If the vendor documents that the request **left no trace**: exactly `NotSent` (T5 on a first send). Otherwise: **T6** (`ambiguous`, `ever_possibly_sent=true`, `next_action_at=now()`) (LF95-C1). | Only through T12, which requires `IdempotentSubmission`, for deposits **and** payouts. Registration refuses a payout `NotProcessed` mapping without `IdempotentSubmission`. |
| `DefiniteDecline` | The vendor definitively refused (canonical `Declined`, with `Cascadable` and `decline_stage`) | Business decline, validation 4xx the vendor documents as final | T8 | No (cascade is a new attempt, §4.6) |
| `Ambiguous` | Anything else: timeout after a possible send, reset, unmapped 5xx, malformed response, panic | `httpclient` `Sent=true`, `ErrProviderMalformedResponse` | T6 | Only T12 (same key, `IdempotentSubmission`) |
| `Pending` | Accepted, with a reference | — | T4 | — |
| `Succeeded` (sync) | Definite success, only where the manifest has `SyncSuccessPossible` and a provider reference is returned | — | T7 | — |
| `ProviderRefInvalid` *[Amendment 2026-10-03, PRH-2 C, see §34]* | The adapter returned a reference that fails `providerref.Validate` (empty on `Pending`; over-bound, not UTF-8 or containing a control character on **any** outcome). Checked before the outcome switch, so it wins over whatever outcome the adapter also reported. | Deposit: `depositAdapterCall`. Payout: `payoutAdapterCall` (already so). | **T10** (`submitting` -> `disputed`, `terminal_reason='invalid_provider_reference:<reason>'`), committed with its audit; the intent projects `ambiguous`. Nothing from the response is stored. | No: parked, never retried, never an error |

- **Timeout classification.**
  - A timeout before dispatch (credential or queue) is `NotSent`.
  - A timeout after dispatch is `Ambiguous` and **never a failure**.
  - A settlement timeout (no final outcome within the horizon) is an escalation (T16), not a
    state.
- **httpclient idempotent retry.** It is enabled for a money-moving request only if the
  manifest has `IdempotentSubmission`, because it re-sends the same key. It is always
  enabled for `QueryStatus`.
- **Reference validation is part of classification** *[Amendment 2026-10-03, PRH-2 C]*. Every
  adapter-response reference is validated before classification, for deposits and payouts alike
  (PROVIDER-REF-BOUND-1 security condition C1, whose deposit half this closes). See §34.

---

## 9. Provider-neutral payment contract

This is a Go-shaped sketch. The final names belong to `payments` in PRH-I1. No field is
vendor-specific, and there is still no free-form passthrough (ADR 0022 §4.1).

### 9.1 Call context (every outbound request type embeds it)

```go
type CallContext struct {
    TenantID       uuid.UUID                    // from the attempt row (server-side), never a payload
    ProviderID     string
    Credential     providercred.OutboundCredential // resolved per call (§11); MOCK: synthetic credential
    IdempotencyKey string                       // "pa:"+attempt.id (money ops); "" for QueryStatus
    Deadline       time.Time
}
```

The adapter **must** fail with `NotSent` if `Credential.TenantID != TenantID`,
`Credential.ProviderID != ProviderID`, or `Credential.Domain != "payments"`. The gate performs
the same check first (§3.2 step 4), so neither is load-bearing alone (S95-C8(b)).

`CallContext` implements its own `String`, `GoString`, `Format`, `LogValue` and `MarshalJSON`,
which render only the credential's existing redacted form (handle id, key id, fingerprint) and
never the secret (S95-C8(c)).

### 9.2 Operations

```go
Deposit(ctx, DepositRequest{Call CallContext; MerchantReference string; Amount int64; AssetCode, PaymentMethod string}) (DepositResult, error)
Withdraw(ctx, WithdrawRequest{Call CallContext; MerchantReference string; Amount int64; AssetCode, PaymentMethod string}) (WithdrawResult, error)
QueryStatus(ctx, StatusQuery{Call CallContext; ProviderReference, MerchantReference string}) (StatusResult, error) // StatusResult gains Found bool
// Refund: reserved, NOT IMPLEMENTED (§5.5)
HandleCallback(ctx, InboundCallback, WebhookCredential) (CallbackEvent, error)          // unchanged signature
WebhookScheme() webhookauth.VerificationScheme                                          // unchanged
Capabilities() AdapterCapability                                                        // gains Manifest (§10.1)
HealthStatus(ctx) (ProviderHealth, error)   // contract tightened (§9.6)
```

Every result and error carries a `Class ErrorClass` (§8). A returned Go `error` without a class
is treated as `Ambiguous` for money operations.

*[Amendment 2026-10-03, PRH-2 C, §34]* `DepositResult` also carries the provider's **echo** of the
amount and asset it processed (`Amount int64`, `AssetCode string`). It is required evidence on a
synchronous success.

`Amount int64` carries forward the existing platform-wide int64 amount path (the same as
`ledger.EntryInput`). This is not new here; an 18-exponent asset routed through
`PaymentProvider` would need its own decision, and custody-asset precision stays with ADR 0008
(LF95-R3).

### 9.3 Callback event (canonical)

`CallbackEvent` gains:

- `MerchantReference string` (optional; the echo of our reference, if the vendor supports it);
- `SettlementReference string` (optional; payout confirmation);
- `DeclineStage` (`at_submission | after_acceptance`) alongside the existing `Cascadable`;
- `EventType ∈ {deposit, deposit_reversal, payout, payout_returned}`. `payout_returned` is
  unsupported (§5.2).

`DeclineReason` (and the attempt's `decline_reason`/`terminal_reason`) holds a canonical,
allow-listed code, never vendor free text. The adapter conformance suite asserts that no
payer-identifying vendor field (name, email, IBAN, PAN or masked PAN, wallet address) maps into
`CallbackEvent`, `StatusResult` or a statement line (S95-C10).

The receipt's `event_fingerprint` is SHA-256 over the canonical, length-prefixed tuple
`(event_type, provider_reference, original_provider_reference, merchant_reference, outcome,
amount, asset_code, settlement_reference)`. It is never computed over raw bytes, because a
vendor may re-sign or reorder a redelivery.

*[AMENDED 2026-09-28 by `architect` (FH-7; item L-b of `rv-prh-i1-callback-ledger.md`
re-review 2). The original §9.3 text above is kept verbatim. This note adds three binding
clarifications:*

1. *A verified `CallbackEvent` with `Outcome = succeeded` is **evidence, never authorization to
   post**. For `EventType = deposit`, only §4.4 as amended by §28 (AM-2) decides whether it
   posts:*
   - *T7 and T13 post only as the intent's **first** financial resolution.*
   - *A verified matching success for an intent that another attempt or posting has already
     financially resolved takes T10 or T13d (`multiple_success_for_intent`). There is no
     posting. It raises a P1 and is reported as `pay_captured_unposted` (§28.9).*

   *This follows INV-DEP-1 (§28.2) and human decision HD-LEDGER-UNALLOC-1, "A now, B later"
   (§28.13). Any wording in this ADR that reads as if a second or later capture of one intent
   posts is superseded. That covers T13's original row, §4.4's LF-Q1 bullet, §20's residuals and
   §21.2 LF-Q1. See `ledger-finance`'s `docs/plans/payment-readiness/lf-q1-supersession.md`.*
2. *For `EventType = deposit_reversal`, `Outcome` does not mean "the reversal itself
   succeeded" (`ledger-finance` H1 rule 5):*
   - *`succeeded`, and the legacy `declined` carrier with `DeclineReason` naming the cause (for
     example a chargeback), are both **final** reversal decisions. They post or tombstone
     identically.*
   - *`pending` and `ambiguous` are **non-final**. They never post or tombstone. The result is
     an anomaly with a P1 audit and the uniform 200.*
   - *A won chargeback (a reinstatement) cannot be represented in this contract. It is
     unsupported and PROVIDER DEPENDENT.*

   *As built: the `CallbackEvent.Outcome` and `HandleCallback` doc comments in
   `internal/payments/types.go`, and `applyReversalReceiptEvidence` in `receipt.go`.*
3. *For `deposit_reversal`, the fingerprint tuple's `outcome` element is the **raw wire
   outcome** (`ReceiptEvidence.RawOutcome`), never the normalized one (H1 rule 3). It is pinned
   by `TestINVDEP1_H1b_ReversalFingerprintEndToEnd_RawWireOutcomePreserved` and
   `TestRVLF_SecGapA3_ReversalFingerprintUsesRawWireOutcome`.]*

### 9.4 Provider reference

References are opaque and bounded by `PROVIDER_REF_MAX` (0099). They are validated at the
adapter boundary after verification as a deterministic rejection, and they are never truncated.
Uniqueness is scoped per `(tenant, provider_id)`, never globally.

### 9.5 Reconciliation interface

```go
// internal/reconciliation/statement (dependency-free leaf, as for casino/sportsbook)
type PaymentStatementLine struct {
    ProviderID, ProviderReference, MerchantReference, OriginalProviderReference string
    Kind   string // "deposit" | "deposit_reversal" | "payout"
    Status string // "pending" | "succeeded" | "declined" | "reversed"
    Amount int64; AssetCode string; OccurredAt time.Time
}
type PaymentStatement struct { CoverageStart, CoverageEnd time.Time; Lines []PaymentStatementLine }
type PaymentStatementSource interface {
    Label() string                                   // MOCK label must contain "MOCK"
    // Fetch runs with NO transaction held (it is provider I/O for a real source; INV-IO-1).
    // It takes the real CallContext: credential resolved per §11 and passed through the §3.2 gate (S95-C11).
    Fetch(ctx context.Context, call CallContext, periodStart, periodEnd time.Time) (PaymentStatement, error)
}
```

This **deliberately differs from `CasinoStatementSource`**, which reads inside the run's
tx. See §12.2 and finding CAS-STMT-IO-1 in §20.

### 9.6 Health and circuit state

- `HealthStatus` is tightened to "**in-memory snapshot, no I/O, returns promptly**". A
  conformance case gives the adapter a transport that fails the test on any request and a
  cancelled context, and asserts a prompt return.
- The orchestrator owns a **breaker per (tenant, provider)**. It is fed by `ErrorClass`: a
  consecutive `Ambiguous`/`NotSent` transport failure counts; `DefiniteDecline` does not.
  States are closed, open and half-open; the half-open probe is the next real call.
- The effective circuit is open if either the adapter snapshot or the breaker says open.
  Routing reads it **outside** any tx. `RouteProvider` is split into
  `ListRoutingCandidates(tx)` (inside a tx) and `Rank(candidates, health)` (no tx).
- A breaker open for tenant A does not affect tenant B. A credential-store outage is
  `NotSent`, is not counted as a provider failure, and never causes a cascade.

---

## 10. Capability manifest and kill switch (payment scope only)

### 10.1 Operation manifest (adapter-declared, code only, never tenant-editable; ADR 0022 §2.1)

`AdapterCapability.Manifest`:

| Field | Type | Fail-safe when absent or false |
|---|---|---|
| `SupportsDeposit`, `SupportsPayout`, `SupportsRefund`, `SupportsDepositReversalEvents` | bool | The operation is refused at registration (if required) or at new activity; events are received as `unsupported_event` |
| `StatusQuery` | `none \| by_provider_reference \| by_provider_or_merchant_reference` | `none` is refused for any adapter with `CallbackCapabilities = polling_only`, and for any production-eligible payout adapter |
| `MerchantLookupAuthoritativeAfter` | duration or 0 | 0 means `not_found` is never authoritative (§4.5) |
| `IdempotentSubmission` | bool | false means no T12, and no httpclient retry on money calls |
| `SyncSuccessPossible` | bool | false means a sync `Succeeded` is treated as `Ambiguous` and triggers a query (defensive) |
| `Interactive` | per payment method: bool | Unknown means `true` (the safer choice: no unattended submission) |
| `CallTimeout`, `SettlementWindow` | durations | Defaults per §7.3 |
| `RedeliveryOn401`, `RedeliveryOn5xx` | `redelivers \| terminal \| unknown` | `unknown` is treated as `terminal` for LF-C1 (§6.6) |
| `ErrorClassMapping` | declared list of `NotProcessed` codes | Empty means every undocumented response is `Ambiguous` |
| `StatementSource` | bool | Required for production eligibility when LF-C1 demands it |
| `CallbackEchoesMerchantReference` | bool | Registration refuses a production-eligible adapter supporting deposit or payout unless this is true **or** `StatusQuery = by_provider_or_merchant_reference`, so a success can never be unconvergeable (LF95-C5) |
| `WebhookRetrySemantics{Retries429, Retries503, HonorsRetryAfter, RetryWindow}` | struct | **Mandatory and fail-closed** for every non-MOCK payments adapter: registration refuses without it (P95-C3; closes ADR 0097 §19 condition 2). ADR 0097 §6.3's interim per-provider config key moves into this code manifest in PRH-I1, so no retry-safety property stays config-editable (S95-C12(iii)). `RetryWindow` is also the floor for receipt retention (S95-C3). |

Registration also refuses (S95-C12, LF95-C8(c)):

- an unknown enum value in any field, and a missing manifest on any non-MOCK adapter;
- `MerchantLookupAuthoritativeAfter > 0 AND < lease + CallTimeout`;
- a `NotProcessed` mapping for payouts without `IdempotentSubmission`;
- `SupportsRefund = true` (no flow exists, §5.5).

Order of refusal (planning gate §9, adopted):

1. **At registration:** an inconsistent manifest or a missing prerequisite refuses to start.
2. **At new activity:** checked at T1/T2 and T1p.
3. **Never at settlement.** Callbacks, polls and postings for in-flight attempts always
   proceed.

The existing tenant layer (`provider_capabilities`, including `status`) still narrows routing
and is unchanged.

### 10.2 Kill switch — data (migration 0102)

`payment_kill_switches(tenant_id, provider_scope TEXT ('*' or provider_id), operation_scope
('deposit' | 'payout' | '*'), engaged BOOLEAN, engaged_by_scope ('platform' | 'tenant'),
release_request_id, reason_code, changed_by, changed_by_scope, changed_at, version)`. It has:

- `UNIQUE(tenant_id, provider_scope, operation_scope)`;
- CAS on `version`;
- every change audited (actor, actor scope, target tenant, before/after, IP, UA, reason code);
- an alert on every engage, not only an audit row (S95-C7).

RLS is two policy families (§10.2.1), both requiring `app.player_account_id` unset.
`payment_kill_switch_release_requests` supports four-eyes release (§10.4).

#### 10.2.1 Who the database believes the actor is (S95-C7, RV-0095 N2/N3)

No actor or scope column is trusted from the application. BEFORE INSERT/UPDATE triggers on both
0102 tables **force** each actor column from the session, **at one defined step only**; client
values are ignored (the ADR 0093 A1 B5 pattern; RV-0095 N5).

| Column | Forced from the session at | Afterwards |
|---|---|---|
| `payment_kill_switches.changed_by`, `changed_by_scope` | every INSERT and UPDATE of the switch row | rewritten at each change (it records the last actor) |
| `payment_kill_switches.engaged_by_scope` | engage (false→true), or a platform take-over while engaged (§10.2.2 point 5) | never `platform→tenant` |
| `release_requests.requested_by`, `requested_by_scope` | INSERT of the request | immutable |
| `release_requests.approved_by`, `approved_by_scope` | the `open→approved` UPDATE only | NULL and unwritable in every other state or transition; immutable once set |

The session scope is derived as follows:

| Session scope | Required GUCs | Principal check (trigger, as migration 0044 does) |
|---|---|---|
| `tenant` | `app.tenant_id` set, `app.principal_id` set (the `WithPrincipalScope` shape), `app.player_account_id` unset, `app.platform_admin_principal_id` unset | `app.principal_id` resolves to a `staff_users` row with `tenant_id = app.tenant_id` |
| `platform` | `app.platform_admin_principal_id` set (the `WithPlatformAdmin` shape), `app.tenant_id` and `app.player_account_id` unset | the principal resolves to a `staff_users` row with `tenant_id IS NULL` |
| anything else | — | the trigger raises; nothing is written |

A `'platform'` value can therefore appear in any scope column only from a genuine platform
session. A tenant connection cannot write it.

**RLS policies (both 0102 tables):**

- **Tenant family:** `tenant_id = app.tenant_id`, with `app.player_account_id` and
  `app.platform_admin_principal_id` unset. SELECT, INSERT and UPDATE; no DELETE.
- **Platform family:** the migration 0075 precedent: `app.platform_admin_principal_id` set, and
  `app.tenant_id` and `app.player_account_id` unset. SELECT, INSERT and UPDATE on any tenant's
  rows (the target tenant is the row's `tenant_id`, taken from the platform route path, §10.5);
  no DELETE; no `FOR ALL`.
  - `WithPlatformAdmin` sets no `app.tenant_id`, so without this family a platform transaction
    would see and write zero rows.
  - The platform family is never visible to the claim path. Claims run under the tenant context,
    where the platform policy's `app.tenant_id IS NULL` condition is false. The claim reads every
    switch row of its own tenant, including platform-engaged ones, through the tenant SELECT
    policy.

#### 10.2.2 Guard trigger on `payment_kill_switches` (S95-C5, RV-0095 N1/N4)

1. **`DELETE` is rejected.** "Missing row = not engaged" can therefore never be produced by
   deleting an engaged row.
2. **Scope is immutable (N1).** `id`, `tenant_id`, `provider_scope` and `operation_scope` can
   never change after INSERT. Re-scoping an engaged row would be a single-actor release, so a
   scope change is always a new row.
3. **`version` is strictly monotonic.** A re-engage between request and approval invalidates the
   request through the version.
4. **`engaged` false→true (engage).** `engaged_by_scope` is forced to the session scope, and
   `release_request_id` is forced to NULL.
5. **`engaged` true→true (re-engage or reason update).**
   - `engaged_by_scope` may go `tenant→platform` (platform takes over a containment).
   - It may **never** go `platform→tenant` (N2(b)).
   - A tenant-scope session may not UPDATE a row whose `OLD.engaged_by_scope = 'platform'` at
     all, so a tenant cannot bump its version either.
6. **`engaged` true→false (release, N4).** `NEW.release_request_id` must reference, through the
   composite FK `(tenant_id, release_request_id)`, a request with all of these:
   - `kill_switch_id = id`;
   - `status = 'approved'`;
   - `expected_version = OLD.version`;
   - `approved_by <> requested_by`;
   - `now() < expires_at`;
   - `decided_txid = txid_current()`, i.e. approved **in this very transaction**.

   If `OLD.engaged_by_scope = 'platform'`, the request's `requested_by_scope` and
   `approved_by_scope` must both be `'platform'`. The same request can never release twice: it
   is bound to one version, and the release bumps `version`.
7. **`release_request_id` is written only by transition 6.** It is forced to NULL on engage and
   is otherwise immutable.

8. **`release_request_id := NULL` is forced on every INSERT** of a switch row (RV-0095 L5(b)). A
   new row can therefore never pre-consume another switch's request through
   `UNIQUE (release_request_id)`.

**Release-request guard trigger (RV-0095 N5, L5(a)):**

- **Columns set at INSERT only, and immutable afterwards:** `id`, `tenant_id`, `kill_switch_id`,
  `expected_version`, `reason_code`, `requested_by`, `requested_by_scope`, `created_at` and
  `expires_at`.
  - `requested_by*` and `created_at` are forced from the session and the clock.
  - `expires_at` is forced to `≤ created_at + 24 h`.
  - An UPDATE that changes any of them is refused, so an approver can never approve something
    the requester did not ask for, and a single actor can never rewrite `requested_by` to
    manufacture a "distinct" approver.
- **Columns written at `open→approved` only:** `approved_by`, `approved_by_scope`, `decided_at`
  and `decided_txid` (= `txid_current()`). All are forced and never client-supplied. They must be
  NULL in `open`, stay NULL on `open→cancelled|expired`, and are immutable once set.
- **Status** moves only `open→approved|cancelled|expired`. A row in a terminal status
  (`approved`, `cancelled`, `expired`) is **fully immutable**.
- **INSERT by a tenant session** is refused when the target switch has
  `engaged_by_scope = 'platform'` (L5(a)). A tenant can therefore never hold the one `open` slot
  and block the platform's own release request.
- Approval is refused when the approver's session scope is not `'platform'` but the switch has
  `engaged_by_scope = 'platform'`.
- There is no DELETE.

The approve endpoint performs the approval and the release UPDATE in one transaction; a request
approved in any earlier transaction can never release.

A platform-wide, cross-tenant switch is **not built**; it is PROV-REVOKE-ALL-1, deferred. Its
interim form is a platform principal engaging each tenant's `'*'` row through the platform
routes (§10.5).

### 10.3 Semantics

**Engaged means no new outbound submission for the scope.**

- It is evaluated **inside the claim statement itself** (T1+T2, T2, T1p, T12) as a
  `NOT EXISTS (SELECT 1 FROM payment_kill_switches k WHERE k.tenant_id = <attempt tenant> AND
  k.engaged AND k.provider_scope IN ('*', $provider) AND k.operation_scope IN ('*', $op))`
  subquery, under the same RLS context as the claim (S95-C6).
  - For the UPDATE forms (T2, T12), `<attempt tenant>` is `payment_attempts.tenant_id`.
  - For the INSERT … SELECT forms (T1+T2, T1p), it is the tenant value being inserted.

  There is no time-of-check/time-of-use gap. The in-flight window is at most one
  already-claimed call.
- **Why a misbound context fails closed (RV-0095 L1).** Every claim form writes
  `payment_attempts`. That table's only write policy is `tenant_staff_scope`: `app.tenant_id`
  equal to the row, `app.player_account_id` unset. The switch table's tenant SELECT policy has
  the same GUC predicate.
  - Any context that would see zero switch rows cannot write an attempt either. That covers a
    wrong tenant, an unset tenant, a player-scoped session and a platform session.
  - The claim therefore claims nothing and makes no call.
  - **This coupling is binding.** Adding any other write policy to `payment_attempts` (for
    example a player-scoped INSERT) is forbidden unless the claim predicate is re-reviewed by
    `security`. §16.2 item 20 pins it: T1+T2 run under a player-scoped context fails and calls
    nothing.
- Deposits: `created` attempts are moved to `rejected` (T3, `kill_switch`) and the player sees
  "unavailable". *[AMENDED by §10.9.2: on the player path (T1+T2) no attempt row exists, so the
  intent is finalized `declined` and the player gets the generic decline response. For a
  cascade `created` attempt this bullet stands, and its implementation is residual
  KS-DEP-T2-T3-1.]*
- Payouts: `created` attempts are **left** in `created`. Their holds stay; they are neither
  failed nor claimed until release.
- A payout claim (T1p) is refused, and the withdrawal stays `approved`.
- **Never stopped:** callbacks, `QueryStatus` polls, postings of existing exposure, and
  reconciliation.
- **Fail-safe:** the switch row is read in the claim tx, so any read error aborts the claim and
  no call is made. A missing row means "not engaged", which is the only non-engaged encoding,
  and DELETE is impossible (§10.2.2). There is no cache: the switch is read from the DB on every
  claim and never from Redis.

### 10.4 Authority

*[PARTLY SUPERSEDED by §30 (AM-1): the first bullet's "and the platform admin API (platform
scope, §10.5)" and the last bullet ("A platform principal cannot act through the tenant API…")
are replaced by §30's single dual-scope route family. Engage, release, four-eyes and the
platform lock are unchanged.]*

- The switch is exposed only on the staff admin API (tenant scope) and the platform admin API
  (platform scope, §10.5). It is never exposed on a player route; a route-table test asserts
  this.
- **Engage:** a single actor with a reason code. That is the safe direction, mirroring ADR
  0093 §3 "single actor on disabling".
- **Release:** four-eyes, ruled by `security` (§22.1) and not relaxed. The request is created by
  one principal and approved by a **distinct** one. The switch row carries the consumed request
  id, following the ADR 0093 A1 binding shape (§10.2.2 point 6). The database enforces it.
- **Platform-engaged switches** (`engaged_by_scope = 'platform'`, derived from the session, never
  asserted by the application) can be released only by platform principals, requester and
  approver both. Tenant staff of a B2B operator can neither release nor downgrade a platform
  containment (S95-C7).
- A platform principal cannot act through the tenant API, and a tenant principal cannot act
  through the platform API.

### 10.5 New and changed staff and platform routes (S95-C13)

*[PARTLY SUPERSEDED by §30 (AM-1) for the kill-switch rows only: the separate "Platform admin
API" table, its `platform_payments_kill_switch:*` permission family, the tenant-route rules
"No tenant id is taken from the path or body" and "A platform-scoped principal is refused with
403", and the platform "Target-tenant rule" route location. The T17, attempt read, M3 and
withdrawal rows are unchanged.]*

**Tenant staff admin API:**

| Route (tenant staff admin API) | Permission | Notes |
|---|---|---|
| Kill switch engage | `payments_kill_switch:engage` | Single actor, reason code; refused on a platform-engaged row (§10.2.2 point 5) |
| Kill switch release request / approve / cancel | `payments_kill_switch:release` | Four-eyes. Approve = approval plus release in one tx. Refused on platform-engaged rows. |
| Kill switch list and read | `payments_kill_switch:read` | Shows platform-engaged rows as read-only |
| T17 re-verify | `payments_attempt:reverify` | Sets `next_action_at` only; never calls a provider inline, so it cannot bypass the sweeper caps |
| Attempt and receipt read | `payments_attempt:read` | Read-only, RLS-scoped |
| M3 abandon | Today's withdrawal-resolve eligibility plus a reason code | CAS `state='created' AND NOT ever_possibly_sent` in SQL and trigger |
| Withdrawal submit and resolve (rewritten) | Existing permissions and eligibility | §5.2 |

For every tenant route:

- It is never reachable with a player principal or on a player or public route; the route-table
  test asserts it.
- Permissions are enforced server-side and tenant-scoped from the authenticated context. No
  tenant id is taken from the path or body.
- The transaction runs under `WithPrincipalScope(tenant, principal)`.
- Object lookups run under RLS, and another tenant's id returns 404, never data.
- A platform-scoped principal is refused with 403.

**Platform admin API** (platform admin surface only; RV-0095 N3):

| Route | Permission | Notes |
|---|---|---|
| Engage for a target tenant: `…/platform/tenants/{tenantId}/payments/kill-switches` | `platform_payments_kill_switch:engage` | Single platform actor, reason code; `engaged_by_scope='platform'` is forced by the trigger |
| Release request / approve / cancel for a target tenant | `platform_payments_kill_switch:release` | Four-eyes between two distinct platform principals; approve = approval plus release in one tx |
| List and read for a target tenant | `platform_payments_kill_switch:read` | — |

Rules for the platform routes:

- **Target-tenant rule.** On these routes, and only here, the target tenant comes from the path.
  - It is valid only for a platform-scoped principal: the token subject resolves to a
    `staff_users` row with `tenant_id IS NULL`.
  - The handler verifies that the tenant exists, then runs `WithPlatformAdmin(principal)`.
  - All reads and writes are explicitly predicated on `tenant_id = {tenantId}`, under the
    platform RLS family (§10.2.1).
  - A tenant-scoped principal gets 403 (or 404) and nothing is read.
- **Audit.** Every mutation writes audit with both the actor (platform principal) and the target
  tenant, plus before/after, IP, UA and reason code. Every engage alerts.

**For both surfaces:** the request/response shapes are pinned in OpenAPI with a conformance test
(§16.2 item 20).

### 10.6 Implementation record (PRH-I1, `payments`, 2026-09-27)

Label: **PARTIALLY IMPLEMENTED** (updated by round 2 below). Data model, guard triggers, RLS, the Go
service layer, the §10.5 HTTP routes/OpenAPI, the S95-C7 engage alert, a first real §10.1 manifest
enforcement point, and the §16.2 item 15 reflection test are `IMPLEMENTED` and tested. The
orchestrator wiring that turns a claim-statement refusal into a T3 `kill_switch` decline and
PROV-OUTBOUND-CRED-1's payments kind-split are `NOT IMPLEMENTED` (Phase 2, gated on concurrent
agents merging first - see round 2's own note). Pending `security`, `ledger-finance` and
`code-reviewer` gate review.

**Migration number, again.** §12.7's own implementation record already recorded one swap (0102
reconciliation / 0103 kill switch). By the time this section landed, migration 0103 had been
claimed by ADR 0096's KYC enforcement policy supersession migration (parallel branch,
`migrations/0103_kyc_enforcement_policy_supersession.{up,down}.sql`) and 0104 by the PRH-I5
security follow-up (`migrations/0104_payment_statement_line_charset.{up,down}.sql`). The kill
switch is therefore **migration 0105** (`migrations/0105_payment_kill_switch.{up,down}.sql`). Read
every "0102 kill switch" / "0103 kill switch" reference in §10.2-§10.5 above as 0105.

**What landed.**
- Migration 0105: `payment_kill_switches` and `payment_kill_switch_release_requests`, exactly per
  §10.2/§10.2.1/§10.2.2 - `payment_kill_switch_session()` derives the acting principal and scope
  from the transaction's own GUCs (`app.tenant_id`/`app.principal_id`/
  `app.platform_admin_principal_id`/`app.player_account_id`, the same ones
  `db.WithPrincipalScope`/`db.WithPlatformAdmin` set) and independently re-validates it against
  `staff_users`, the migration 0044 pattern; nothing is ever read from an application-supplied
  column. Both tables: FORCE RLS with the tenant/platform policy family, `DELETE` denied, scope
  columns (`id`/`tenant_id`/`provider_scope`/`operation_scope`) immutable, `version` forced
  strictly monotonic on every UPDATE, engage is single-actor, release requires an approved request
  decided in the SAME transaction (`decided_txid = txid_current()`) by a distinct principal, a
  platform-engaged row is untouchable from a tenant session, and KS-L6 (a platform takeover cancels
  any open tenant release request for that switch) is implemented in the same trigger.
- `internal/payments/killswitch.go`: `EngageKillSwitch` (idempotent single-actor engage/re-engage),
  `GetKillSwitch`/`ListKillSwitches`, `RequestKillSwitchRelease`, `ApproveAndReleaseKillSwitch`
  (approval and release in one transaction), `CancelKillSwitchRelease`, and a read-only
  `KillSwitchEngaged` helper for orchestration code that needs to CHOOSE a terminal reason after an
  atomic claim-statement refusal (never a substitute for the in-statement predicate).
- The §10.3 predicate, evaluated **inside** the claim statement itself, exactly as specified:
  `attempt.go`'s `ClaimCreatedForSubmission` (T2), `InsertSubmittingAttempt` (T1+T2/T1p, converted
  to an `INSERT ... SELECT ... WHERE NOT EXISTS (...)` form so the combined insert-and-claim path
  has the same atomicity as the CAS `UPDATE` forms), `ResubmitAmbiguous` (T12), and
  `InsertCreatedAttempt` (the cascade T1 insert, checked against `provider_scope = '*'` switches
  only, since no provider is chosen yet at T1 - defence in depth on top of T2's own full check at
  claim time). `ErrKillSwitchEngaged` is returned by the two `INSERT ... SELECT` forms, which have
  no prior `created` row to fall back into.
- `deploy/init-app-role.sql`: `igaming_runtime` gets `SELECT, INSERT, UPDATE` (never `DELETE`) on
  both new tables, re-asserted on every run, mirroring the 0102/reconciliation grant block.
- Tests: `internal/payments/migration_0105_integration_test.go` (schema/trigger invariants, scratch
  databases only) and `internal/payments/killswitch_integration_test.go` (the Go service layer).
  Mutation-kill evidence for MX17/MX19/MX20/MX21/MX25 is recorded in
  `docs/plans/payment-readiness/evidence/prh-i1-mutation-kill.txt`.

**Round 2 (2026-09-27, same day): §10.5 routes, S95-C7 alert, §16.2 item 15 reflection test, and a
first real §10.1 manifest enforcement point landed.** Label updated to **PARTIALLY IMPLEMENTED**
(narrower gap than round 1 - see the new "still NOT built" list below). Pending `security`,
`ledger-finance` and `code-reviewer` gate review; Phase 2 (orchestrator T3/T1p wiring,
PROV-OUTBOUND-CRED-1 kind-split) is gated on the concurrent payout/callback-cutover agents merging
first, per the orchestrator's own sequencing instruction, and has not started.

**What additionally landed in round 2.**
- **§10.5 routes** (`internal/httpserver/payments_kill_switch_handlers.go`): a DELIBERATE,
  documented deviation from the ADR's literal two-route-family/two-permission-family design - see
  that file's own doc comment. ONE route family under
  `/v1/admin/tenants/{tenantID}/payments/...`, reusing this codebase's own established
  `canActOnTenant` dual-scope pattern (provider_credential_handlers.go/admin_routes.go) instead of a
  second `/platform/tenants/{tenantId}/...` tree: a tenant-scoped caller may only name its own
  tenant (403 otherwise), a platform-scoped caller may name any tenant. Every functional
  requirement §10.5 lists (tenant isolation, platform reach, cross-tenant 404, audit with actor and
  target tenant, four-eyes release, never reachable by a player token) is satisfied - the
  DATABASE (migration 0105), not the URL/permission shape, is what enforces the platform-lock and
  four-eyes properties. Permissions `payments_kill_switch:engage/release/read`
  (`internal/auth/permission.go`) are granted identically to `RolePlatformAdmin` and
  `RoleTenantAdmin` (unlike `provider_credential:request/approve`, which are platform-only - kill
  switch release is deliberately also tenant-scoped, per the ADR). OpenAPI:
  `docs/api/openapi/platform-api.yaml` (6 paths, 7 operations), contract-tested
  (`openapi_payments_killswitch_contract_test.go`). HTTP-level tests
  (`payments_kill_switch_api_integration_test.go`, a private scratch database migrated to head, 10
  tests): tenant engage/four-eyes release, cancel, platform-acts-on-arbitrary-tenant,
  platform-engaged-row-is-tenant-read-only, cross-tenant 403 (wrong path) and 404 (right path,
  foreign id), player-token 403, unknown-field 400, list/get, validation. One real bug found and
  fixed in the process: `audit_log`'s RLS (migration 0014) has no "platform writes into a named
  tenant's audit scope" policy family (unlike migration 0105's own two-family design), so a
  platform-scoped mutation's audit row is written as a platform-level event
  (`TenantID: uuid.Nil`) with the actual target tenant carried in `Metadata.target_tenant_id`
  instead (`killSwitchCall.auditTenantID()`) - mirrors `recordProviderCredentialDenied`'s
  already-established handling of the identical RLS shape.
- **S95-C7 engage alert**: `logKillSwitchEngagedAlert` (same file), a structured, allow-listed
  Error-level log line on every successful engage - this codebase has no dedicated
  alert/notification subsystem (see `sportsbook_settlement_handlers.go`'s
  `logSettlementIntegrityAlert`, the identical precedent); labelled honestly as a log line, not new
  infrastructure.
- **§10.1 manifest, first real enforcement point** (`internal/payments/contract.go`,
  `capability.go`): two fields added to `OperationManifest`, each with a real, tested,
  registration-time refusal in the new `validateManifest` (called from `WriteCapability` before the
  existing tenant-narrowing check) - never an unread field:
  - `SupportsRefund` (§5.5): registration refuses ANY adapter declaring `true` outright (no refund
    flow exists).
  - `CallbackEchoesMerchantReference` (LF95-C5): registration refuses a PRODUCTION-ELIGIBLE
    (non-`Synthetic`) adapter supporting deposit or withdrawal unless this is `true` OR
    `StatusQuery == "by_provider_or_merchant_reference"`. A `Synthetic` (MOCK) adapter is exempt.
  - Tests: `capability_manifest_test.go` (7 cases, pure Go, no DB).
  - The remaining §10.1 fields (`SupportsPayout`, `SupportsDepositReversalEvents`,
    `RedeliveryOn401/5xx`, `ErrorClassMapping`, `StatementSource`) are still NOT modeled - each has
    no real consumer yet and is registered as a deferred item instead
    (`docs/governance/task-registry.md` PRH-I1-MANIFEST-1..4), per this round's own instruction
    ("don't leave unread fields"). `WebhookRetrySemantics` mandatory fail-closed was already
    satisfied before this round via `webhookauth.MustRequireRetrySemantics` (ADR 0097), confirmed
    still wired at `NewOrchestrator`.
- **§16.2 item 15 reflection test, scoped to payments/casino/KYC**
  (`internal/testsupport/credentialscan`, a small shared recursive `reflect` walker, own unit tests
  proving it actually catches a planted violation - not vacuous): each domain gets
  `credential_reflection_test.go` scanning its own constructed adapters/resolvers/orchestrator
  registry (`providercred.OutboundCredential`, `secretstore.Secret`, `providercred.DerivedTokenCache`,
  a non-nil `httpclient.Authenticator`, or any non-nil, non-allow-listed func field), plus a static
  check that the adapter's own source file (`mock.go`/`mock_provider.go`) never imports
  `internal/secretstore`. This did not exist anywhere in the codebase before this round (not for
  casino or KYC either) - a pre-existing platform-wide gap, now closed for all three domains this
  ADR's own scope covers.

**What is still NOT built (remaining PRH-I1 scope, gated on Phase 2's go-ahead):** *[Built by
Phase 2 (`d4520da`) and its fix round (`4e04f4e`), except KS-DEP-T2-T3-1 and PROV-REVOKE-ALL-1;
see §10.9.]*
- The orchestrator wiring that turns a T2/T1p refusal into a deposit's T3 `kill_switch`
  decline/payout `created` hold with a labelled reason (§10.3's "Deposits: created attempts are
  moved to rejected (T3, kill_switch)"). Today a refused claim surfaces as `ErrAttemptStateConflict`
  or `ErrKillSwitchEngaged`; the caller can call `KillSwitchEngaged` to choose the right terminal
  reason, but no call site does yet. Gated on the concurrent payout/callback-cutover agents (editing
  `payout.go`/`payout_sweep.go`/`deposit_v2.go`/`receipt.go`/`orchestrator.go`/`cascade.go`) merging
  first.
- PROV-OUTBOUND-CRED-1's payments kind-split resolver mirroring casino/KYC's own
  `OutboundKindSplitResolver`, threading a `pool` argument through `gate.go` and every call site, and
  the per-call timeout/outage → T5 behaviour. Same Phase 2 gate as above (touches the same files).
- PROV-REVOKE-ALL-1 (a genuine platform-wide, cross-tenant switch) remains deferred per §10.2.2's
  own text.

### 10.7 Fix round 1b (`payments`, 2026-09-27): RV-PRH-I1 security review CHANGES REQUIRED response

Responds to `docs/plans/payment-readiness/rv-prh-i1-killswitch-security.md` (reviewed at `be0d899`).
Label: still **PARTIALLY IMPLEMENTED**; the gaps this round closes are H1, M1-M3, M6 and most of the
Lows. M4 and M5 are closed to the review's own "before stage gate" bar; both remain
**launch-blocking** per the review's explicit ruling (M4's regulator-traceability architect decision
and M5's evadability are accepted-with-a-registered-follow-up, not resolved outright). Ledger-finance
H3 (the callback/poll/drive.go decline paths) is **out of this section's scope** - it belongs to the
concurrent callback-cutover agent, per `rv-prh-i1-callback-ledger.md`.

**H1 (blocks completion) - CLOSED.** `internal/payments/killswitch_claim_predicate_coverage_test.go`:
one permanent test per claim form (T1+T2 deposit, T1p payout, T12, cascade T1), each with a positive
control (a non-matching provider/operation still succeeds), plus the §16.2 item 20 player-scoped-
context pin. Mutation-killed: K2 (`InsertSubmittingAttempt`), K3 (`ResubmitAmbiguous`), K4 (cascade
`InsertCreatedAttempt`) - adapted directly from the review's own probe P2. Evidence:
`docs/plans/payment-readiness/evidence/prh-i1-mutation-kill.txt`.

**M1 (fail-open) - CLOSED, migration 0106.** `migrations/0106_payment_attempts_platform_guc_
hardening.{up,down}.sql` (renumbered from an initial 0107 to 0106 by the orchestrator before merge,
since 0106 was still free and `migrate verify` rejects version gaps; the callback agent's migration is
0107). Option (a) from the review: `payment_attempts`/`payment_provider_events`'s
`tenant_staff_scope` policies now also require `app.platform_admin_principal_id` unset, aligning them
with `payment_kill_switches.tenant_scope`. `TestMigration0106_MixedGUCContextClaimsNothing` is the
adapted, permanent version of probe P1 and is mutation-killed. The ADR's own §10.3 L1 claim ("the same
GUC predicate") is corrected by this fix, not merely documented as wrong.

Migration 0106 additionally folds in, in the same reviewed change (`CREATE OR REPLACE FUNCTION` on
the three migration 0105 trigger functions, additive per CLAUDE.md - migration 0105 itself is
untouched):
- **L1** (a tenant cannot cancel/change the status of a platform-filed request, or one against a
  platform-engaged switch) - test: `TestMigration0106_L1_TenantCannotCancelPlatformRequest`.
- **L2** (`expected_version` forced to the switch's real current version at request INSERT, closing
  R3; a request may only be filed against a currently-`engaged` switch, closing half of R2) - tests:
  `TestMigration0106_L2_FutureVersionRequestIsForcedToCurrent`,
  `TestMigration0106_L2_RequestRequiresEngagedSwitch`,
  `TestMigration0106_L2_EngageCancelsPreexistingOpenRequest`.
- **L3** (`changed_at` forced from the clock, never client-supplied) - test:
  `TestMigration0106_L3_ChangedAtIsForced`.
- **L4** (`payment_kill_switch_session()` additionally requires `staff_users.status = 'active'`) -
  test: `TestMigration0106_L4_SuspendedStaffRejectedAsActor`.

**M2 (unvalidated `provider_scope`) - CLOSED.** `newEngageKillSwitchHandler`
(`payments_kill_switch_handlers.go`) rejects leading/trailing whitespace and any `provider_scope`
other than `'*'` that is not registered in `deps.PaymentOrchestrator` (the CODE-level registry, never
tenant-editable capability config) - 400, never a silent no-op. Test:
`TestPaymentsKillSwitchAPI_ProviderScopeValidation`.

**M3 (no before/after in audit) - CLOSED.** Every mutation's `audit.Record` now carries `before`/
`after` objects (engage additionally records `is_platform_takeover` and, when true,
`ks_l6_cancelled_request_id` - detected by the handler reading the pre-existing open request id
before calling `EngageKillSwitch`, the "reliable option" the review named). Tests:
`TestPaymentsKillSwitchAPI_AuditRecordsCarryBeforeAfterAndTargetTenant`,
`TestPaymentsKillSwitchAPI_AuditRecordsPlatformTakeover`.

**M4 (platform traceability) - test added, architect decision registered, launch-blocking.**
`TestPaymentsKillSwitchAPI_AuditRecordsCarryBeforeAfterAndTargetTenant` pins `target_tenant_id` on a
platform-scoped mutation's audit row (K18). The schema-level fix (an `audit_log` platform INSERT-only
policy family, or a first-class indexed column) is registered as **KS-AUDIT-TENANT-1**
(`docs/governance/task-registry.md`), an `architect` decision per the review's own instruction - not
implemented here.

**M5 (reflection test scope/evadability) - substantially closed; relabelled PARTIALLY IMPLEMENTED
per the review's own instruction.** `internal/testsupport/credentialscan` now additionally: detects
`atomic.Pointer[T]` holding a forbidden type (via the type parameter, which survives even when the
pointer is nil); flags `chan` of a forbidden/`Authenticator`-implementing type structurally (never
receives from it); flags `unsafe.Pointer` unconditionally (its target type is unrecoverable);
and checks `reflect.PointerTo(t).Implements(authenticatorType)` for addressable values (a pointer-
receiver `Authenticator` held by value). `CheckPackageImports`/`CheckPackageLevelVars` make the
static half PACKAGE-WIDE (every non-test file, not one hand-picked adapter file) and add the
package-level-var rule. New `cmd/platform-api/credential_reflection_test.go` scans
`buildProviderBundle(allOnWiring)` itself - the actual `paymentsAdapters()`/`casinoAdapters()`/
`kycAdapters()` registries and outbound/webhook resolvers `main()` wires - not a skeletal mock a
domain test builds separately, closing the review's central "does not scan what production wires"
finding. Residual, accepted evasion (inherent to a type-based scan, not fixed): a raw `string`/
`[]byte` holding secret-shaped data has no distinguishing type to catch. Relabelled honestly:
**PARTIALLY IMPLEMENTED**, launch-blocking before the Synthetic tripwire is ever relaxed for a real
adapter, per the review's own ruling.

**M6 (untested single-point checks) - CLOSED for K10/K11/K14; K19 could not be demonstrated as a live
bypass.** **CORRECTION (see §10.8): this claim did not hold.** All of this section's
`TestMigration0105_*` tests ran against a scratch database pinned to exactly migration 0105
(`migration0105Scratch`), which migration 0106 (this same fix round, above) supersedes with
`CREATE OR REPLACE FUNCTION` on the very three trigger functions these tests exist to cover. The
"K10/K11/K14 killed" result below was therefore measured against dead code, not what runs in
production. §10.8 fixes the harness and re-runs every one of these mutations against the live
bodies. Tests: `TestMigration0105_M6_K10_RequestMustBindToItsOwnSwitch`,
`TestMigration0105_M6_K11_ExpiredRequestCannotRelease`,
`TestMigration0105_M6_K14_ExpiresAtCappedAt24Hours` (all three mutation-killed - K10 and K11 required
combining the approve and release UPDATE into ONE transaction in the test, since the
`decided_txid = txid_current()` rule already refuses a separate-transaction release for an unrelated
reason, which had been silently masking whether the mutation under test did anything).
`TestMigration0105_M6_K19_PlatformGUCRequiresGenuinePlatformStaff` pins the observable property (a
tenant-scoped staff id in the platform GUC is refused) and passes on correct code, but a byte-level
mutation removing only the trigger's own `staff_users.tenant_id IS NULL` clause could not be shown to
create a live bypass in testing: `staff_users`' own RLS (`dual_scope_isolation`) already makes a
tenant-scoped row invisible to a query run under a platform-only GUC context (no `app.tenant_id` set),
and the `role = 'platform_admin' <=> tenant_id IS NULL` CHECK constraint means anything visible under
that RLS view is already genuinely platform-scoped - RLS is acting as an independent, sufficient
second layer for this specific property. Recorded here rather than silently claimed as mutation-
killed.

**Lows.** L1-L4 above (migration 0106). **L5** (audit-on-refusal, error class, 5xx-vs-409): partially
closed this round, **CLOSED in §10.8** - `writeKillSwitchError` now classifies a trigger `RAISE
EXCEPTION` (SQLSTATE `P0001`) as 409 with a logged class and message, and any OTHER error as a genuine
500 (previously always 409, silently under-logged); a separately-committed `OutcomeDenied` audit row
for a refused mutation (self-approval, platform-lock, etc., beyond the existing foreign-tenant case)
was **NOT IMPLEMENTED** this round - see §10.8 for the closure. **L6** (platform engage against a
nonexistent tenant) - CLOSED, `beginKillSwitchCall` checks `tenants` existence for a platform-scoped
caller (`tenants` carries no RLS); test: `TestPaymentsKillSwitchAPI_PlatformCallerAgainstNonexistentTenantIs404`.
**L7** (alert label) - relabelled here and in the task registry: engage **event IMPLEMENTED** (test:
`TestLogKillSwitchEngagedAlert_EmitsPinnedEventAndFields`), alert **delivery NOT IMPLEMENTED**
(launch-blocking); runbook entry added (`docs/runbooks/observability-and-alerting.md` §2 item 12).
K21 (the engage handler's call SITE to that function) survived this round's mutation set - see §10.8
for the HTTP-level closure. **L8** (route-table test) - CLOSED, `TestPaymentsKillSwitchAPI_RouteTable`
enumerates all 7 routes and asserts 403 (player token) / 401 (no token) on every one.

**I1-I6 (informational).** Not addressed this round beyond what the fixes above happen to touch (I2's
policy-shape note and I4's `validateManifest` timing note are unchanged; both are accepted per the
review's own text, not launch-blocking).

---

### 10.8 Fix round 1c (`payments`, 2026-09-27): RV-PRH-I1 security re-verification 1 (N1) response

Responds to `docs/plans/payment-readiness/rv-prh-i1-killswitch-security.md`'s "Re-verification 1 - fix
round 1b at `33d4d9e`" section (reviewed against migration 0106). Verdict there: CHANGES REQUIRED,
narrower - every §10.7 behaviour fix (H1, M1, M2, M3, the M4 test, the M5 scope, L1-L4, L6, L8) is
confirmed correct on the LIVE code; one new finding, **N1 (HIGH, blocks completion)**, plus the
already-known-remaining L5/L7/M5 items, are closed here.

**N1 (blocks completion) - CLOSED.** Root cause: migration 0106 `CREATE OR REPLACE`s
`payment_kill_switches_guard()`, `payment_kill_switch_release_requests_guard()` and
`payment_kill_switch_session()`, but `migration0105Scratch` (used by every test in
`migration_0105_integration_test.go`, `killswitch_integration_test.go` and
`killswitch_claim_predicate_coverage_test.go`) built a scratch database pinned to exactly 0105,
exercising the SUPERSEDED bodies. `migration0105Scratch` is now repointed to migrate a fresh scratch
database all the way to HEAD (`pool.MigrateUp(ctx, realMigrationsDir(t))` - the same mechanism
`migration0106Scratch` already used), exactly the review's own suggested fix. No test in these files
turned out to be about migration 0105's own history in isolation, so nothing needed to move rather
than be repointed; the now-dead `migration0105Version`/`findMigrationVersion` helpers (only used to
compute the pin) were removed along with it. A rule for future migrations is recorded here and should
be treated as project policy: **any migration that `CREATE OR REPLACE`s a guard/trigger function
covered by an existing test must repoint that test's scratch harness at HEAD in the SAME change**,
never leave it pinned below the migration that redefines the function.

Two additional tests close the review's specific "K19's current test uses a tenant staff id, which
RLS hides anyway" note - the broader probe, a UUID matching NO `staff_users` row at all (immune to
that RLS side effect), plus the suspended-platform-staff variant it also asked for:
`TestMigration0105_N1_K19_RandomUUIDInPlatformGUCIsRefused` (with a positive control - a genuine
platform staff principal in the same GUC still succeeds) and
`TestMigration0105_N1_K19_SuspendedPlatformStaffUUIDIsRefused`.

**Mutation re-run against the LIVE 0106 bodies (K5, K10, K11, K12, K13, K14, K19).** Each mutation was
applied directly to `migrations/0106_payment_attempts_platform_guc_hardening.up.sql` on disk (the file
that now defines these three functions via `CREATE OR REPLACE`), a fresh scratch database was built
from it, the target test was run with `-count=1`, and the file was restored and verified
byte-identical (`md5sum` match) before the next mutation. Full transcript:
`docs/plans/payment-readiness/evidence/prh-i1-mutation-kill.txt`, "Kill-switch fix round 1c" section.

| Mutant | What was removed | Result | Killed by |
|---|---|---|---|
| K5 | KS-L6 takeover cancel (true→true engage) | Killed | `TestMigration0105_PlatformTakeoverCancelsOpenTenantRequest` |
| K10 | `v_req.kill_switch_id <> OLD.id` | Killed | `TestMigration0105_M6_K10_RequestMustBindToItsOwnSwitch` |
| K11 | `now() >= v_req.expires_at` | Killed | `TestMigration0105_M6_K11_ExpiredRequestCannotRelease` |
| K12 | `v_req.expected_version <> OLD.version` | Killed | `TestMigration0105_StaleVersionCannotRelease` |
| K13 | tenant-lock check on a platform-engaged row | Killed | `TestMigration0105_TenantCannotTouchPlatformEngagedRow` |
| K14 | 24h cap (`LEAST(...)` → plain `COALESCE`) | Killed | `TestMigration0105_M6_K14_ExpiresAtCappedAt24Hours` |
| K19 | whole principal predicate in `payment_kill_switch_session()` | Killed | `TestMigration0105_N1_K19_RandomUUIDInPlatformGUCIsRefused`, `TestMigration0105_M6_K19_PlatformGUCRequiresGenuinePlatformStaff`, `TestMigration0105_N1_K19_SuspendedPlatformStaffUUIDIsRefused` |

All seven are now genuinely mutation-killed against the code that actually runs. Full-suite
confirmation after the harness fix: `internal/payments` (all `TestMigration0105`, `TestMigration0106`
and `TestKillSwitch*` tests, 30/30 pass) and the full `internal/payments -tags integration` suite,
both against a private database.

**L5 (denied-audit row for a refused mutation) - CLOSED.** `writeKillSwitchError` now writes an
`audit.OutcomeDenied` row for both refusal classes (`cas_conflict`, `trigger_refusal`) via
`recordKillSwitchRefusalAudit`, dispatched through `runKillSwitchTx` - a FRESH transaction, dispatched
exactly like the mutation itself, independent of the failed attempt (which already rolled back,
discarding anything `audit.Record` would have written inside it). A genuine internal/database error
(the 500 class) gets no denied-audit row - it is not a considered refusal, and recording an
infrastructure failure as a "denial" would corrupt the audit trail's meaning. Tests:
`TestPaymentsKillSwitchAPI_TenantAdminEngageAndFourEyesRelease` (extended - the self-approve refusal
now asserts exactly one `denied`-outcome `approve_release` audit row with
`denied_class: "trigger_refusal"`, alongside the later successful approval's own row) and
`TestPaymentsKillSwitchAPI_CancelReleaseRequest` (extended - the second, already-terminal cancel
attempt asserts its own denied row). Mutation-killed: deleting both
`recordKillSwitchRefusalAudit` call sites fails both tests.

**L7/K21 (HTTP-level alert assertion) - CLOSED.** `TestPaymentsKillSwitchAPI_TenantAdminEngageAndFourEyesRelease`
now also asserts, over the real HTTP stack (a `syncBuffer`-backed `slog.Logger` wired into the test
server's `Deps.Logger`), that a genuine engage call emits the `payments_kill_switch_engaged_alert` log
line - not merely that `logKillSwitchEngagedAlert` does so in isolation
(`TestLogKillSwitchEngagedAlert_EmitsPinnedEventAndFields`, which cannot notice its own call site
being deleted from the handler). Mutation-killed: deleting the `logKillSwitchEngagedAlert(...)` call
in `newEngageKillSwitchHandler` fails this test. Alert delivery itself remains **NOT IMPLEMENTED** and
launch-blocking, unchanged from §10.7.

**M5 residual #1 (addressability gate) - CLOSED.** `credentialscan.Scan`'s pointer-receiver
`Authenticator` check was gated on `rv.CanAddr()`, but `reflect.PointerTo(t).Implements(...)` needs no
addressability - it is a pure type-level check. A value-typed adapter stored in a `map[string]any` or
an `interface{}` (exactly the shape `paymentsAdapters()` returns) is never addressable, so the gate let
a pointer-receiver `Authenticator` held by value inside such a container slip through undetected. The
`rv.CanAddr()` condition is removed. Test: `TestScan_CatchesPointerReceiverAuthenticatorInMap`,
reproducing the review's own probe (`map[string]any{"p": valueAdapter{...}}` reported 0 violations
before the fix, 1 after - verified both ways). M5 residual #2 (raw `string`/`[]byte` secrets) remains
inherent to a type-based scan and is unchanged from §10.7; the **PARTIALLY IMPLEMENTED** label stands.

**Verification.** `go build ./...`; `go vet -tags=integration ./...`; `gofmt -l` on every touched
file; golangci-lint 2.9.0 (`--allow-parallel-runners`) on `internal/payments`, `internal/httpserver`,
`internal/testsupport/credentialscan` - 0 issues. `internal/payments -tags=integration -race
-count=1` and `internal/httpserver -tags=integration -race -count=1`, both against a private database
(never the shared CI database) - green.

### 10.9 Phase 2 architect review record (`architect`, 2026-09-27)

This section records the architect's review of Phase 2, which covers the orchestrator wiring
and the payments kind split for PROV-OUTBOUND-CRED-1. It folds in the implementer's notes
(`docs/plans/payment-readiness/killswitch-phase2-adr-notes.md`) and the ruling in
`docs/plans/payment-readiness/rv-prh-i1-killswitch-phase2-architect.md`.

- **Branch:** `worktree-agent-aa2bb3c6bdd6d51eb` @ `4e04f4e`.
- **Commits:** `ea7910a`, `d4520da`, and the fix round `ce77bac`, `50b595d`, `533f85f`,
  `f5e96c4`, `4e04f4e`.
- **Other reviews:**
  - `security`: APPROVE WITH CONDITIONS (`rv-prh-i1-killswitch-phase2-security.md`).
  - `code-reviewer`: APPROVE WITH CONDITIONS (`rv-prh-i1-killswitch-phase2-code-review.md`).
    C1–C5 were addressed by the fix round; the code-reviewer has not yet re-reviewed it.

**10.9.1 Merge ordering (confirmed).** Phase 2 merges **before** the PAY-DOUBLE-CREDIT-1 fix
(§28).

- Both reviews checked the branch hunk by hunk. It touches no success, credit,
  decline-with-posting or cascade path:
  - `receipt.go`, `orchestrator.go`'s `postDepositSuccess` and `applyDepositCallResult` /
    `applyStatusEvidence` are unchanged;
  - the only hunks in `drive.go`, `sweeper.go` and `payout_sweep.go` add a `pool` argument.
- The deposit kill-switch decline happens before any attempt exists, so no success evidence can
  ever arrive for it.
- Merging this branch first means the §28 fix is written against the final `callProvider` /
  `DispatchWithdraw` / `Resolve(ctx, pool, tenantID, providerID)` signatures, so it needs no
  rebase afterwards.

Conditions on the §28 fix, which lands second:
- every call site it adds or moves passes `pool` and the kind-split resolver, never a nil
  resolver;
- it adds no provider call to any evidence transaction (INV-IO-1; the static scan
  `internal/txscope/no_provider_call_in_tx_closure_static_test.go` must stay green);
- it routes no deposit success through the legacy `InitiateDeposit` / `attemptDeposit` path
  (PROV-OUTBOUND-CRED-1-LEGACY-PATH);
- it re-runs the full `internal/payments` suite, `killswitch_claim_predicate_coverage_test.go`
  and `pool_threading_integration_test.go` on the combined tree.

**10.9.2 Player-facing result of a kill-switched deposit (C5), ruled: amend the ADR, not the
code.** On the player path (T1+T2 in one phase-A transaction), the kill-switch predicate
refuses inside the `INSERT … SELECT`, so **no attempt row exists**. The *intent* is finalized
`declined` in the same transaction through `finalizeDeclined`. This is the same shape as an RG
or KYC phase-A denial, which likewise creates no attempt.

- The player receives the generic decline response. `status = "declined"` carries no reason,
  exactly as for every other decline.
- The reason `kill_switch` and the routed `provider_id` are recorded for operators:
  - in the `deposit.declined` audit metadata;
  - `provider_id` also on `deposit_intents.provider_id` (C5 second half, `f5e96c4`).
- The decline is terminal for that player idempotency key. A retry needs a new key. That is
  intended: a deposit is never left parked `pending` across an operator containment.

Rationale:
- "unavailable" was a description of meaning, not a response contract. No distinct response
  shape was ever specified.
- A reason-specific player response would reveal an operator containment state that the
  platform shows for no other decline cause.
- It would also turn one of several pre-attempt declines into a separate API contract.

No human decision is needed: this is not a licence, legal or commercial question, and the
player's funds are untouched. If product later wants distinct player copy (for example
"payments temporarily unavailable"), that is a UI/brand-configuration change mapped from an
operator-visible reason. It needs no change to this state machine. It is recorded as a
deferred consideration, not built.

**10.9.3 Residual KS-DEP-T2-T3-1 (new; payments; NOT IMPLEMENTED; required before PRH-I1 is
marked complete and before a `payments.Sweeper` is wired in `cmd/platform-api`).**

- **Where the ADR text still stands:** §10.3's "created attempts are moved to `rejected` (T3,
  `kill_switch`)" still governs **cascade** `created` deposit attempts.
- **What is built instead:** at `4e04f4e`, a provider-scoped switch that covers the cascade
  target makes `ClaimCreatedForSubmission` (T2) match zero rows. `drive.go` returns that as an
  error. The attempt stays `created` and the intent stays `pending`.
- **What goes wrong:** on the synchronous player cascade, the request fails with an error
  instead of a clean decline. Nothing is sent and no money moves, so this fails closed. But with
  no sweeper constructed, the intent is stuck.
- **Required fix:** in the same per-item transaction, classify the refusal with
  `KillSwitchEngaged` (reading only; the in-statement predicate stays the control), then:
  - `RejectCreated(…, 'kill_switch')` (T3);
  - recompute the intent projection (`declined` when nothing is live);
  - audit;
  - add one test plus a mutant.
- A wildcard (`'*'`) switch is already handled at cascade T1 (`payment.cascade_skipped_kill_switch`).
- *[Status note 2026-09-28 (`architect`, FH-7): **IMPLEMENTED** in `2da7548`, which is merged on
  this branch. Verified by reading `internal/payments/drive.go` (`driveCreatedAttempt`, the
  `ClaimCreatedForSubmission` error branch). The fix does exactly what was required:*
  - *it acts only on `ErrAttemptStateConflict`;*
  - *it classifies the refusal with a read-only `KillSwitchEngaged` call, and a genuine CAS
    conflict still returns an error;*
  - *it writes the `payments.cascade_rejected_kill_switch` audit, then `RejectCreated(…,
    'kill_switch')` (T3), then `finalizeDeclined`, whose sticky guard keeps a `succeeded`
    intent `succeeded`.*

  *QA verified it with a mutant that is killed:
  `TestDriveCreatedAttemptCascade_KillSwitchOnFallbackProvider_DeclinesCleanly_T3`
  (`qa-killswitch-phase2-verification.md` §2). No `code-reviewer` record exists for this delta
  (see `rv-fh7-architect-final.md` FH7-07).]*

**10.9.4 Kind split and pool threading: consistent with casino/KYC and ADR 0094/§11.**

- `payments.OutboundKindSplitResolver` is structurally identical to casino's and KYC's:
  - it routes by adapter identity (`SyntheticComponent()`), not by what is wired;
  - an unregistered provider fails closed;
  - a nil target fails closed;
  - it returns a nil interface when neither half is wired.

  It is duplicated per domain on purpose (§3.2: no cross-domain import).
- `callProvider` order is: nil-resolver guard (P2-L3) → `txscope.Held` refusal → committed-claim
  check → `Resolve(ctx, pool, …)` in the resolver's own short transaction (ADR 0094 §4.1) →
  tenant/provider/domain binding check. This matches §3.2 steps 1–4.
- Pinned by:
  - `outbound_kindsplit_test.go` (C1);
  - `pool_threading_integration_test.go`, covering all five call sites (C2);
  - `TestCallProvider_CredentialForWrongTenant_RefusedByBindingCheck` (P2-L1).
- The one deliberate hard-wired `MockCredentialResolver{}` is the MOCK statement source (§12.4).
  It is documented in `registrations.go`, and it must never be rewired to the kind split.
- **C4** (the MOCK half is wired only when test-support endpoints are enabled, matching
  casino/KYC) is accepted. It is documented in `production-configuration-checklist.md` item 7.
  When a sweeper is wired, add a boot-time refusal for "synthetic payments adapter registered,
  no mock resolver wired", so parked rows cannot accumulate silently.
- **C3/P2-L4** (payout hold audit detached from the request context and logged on failure) is
  closed in `533f85f`.

**Labels.**
- Phase 2 wiring and the payments kind split: IMPLEMENTED on `4e04f4e`, pending merge and the
  code-reviewer's re-review of the fix round.
- KS-DEP-T2-T3-1: NOT IMPLEMENTED. *[Status note 2026-09-28: IMPLEMENTED (`2da7548`); see the
  note at the end of §10.9.3. Phase 2 (`4e04f4e`) is merged on this branch.]*
- PROV-OUTBOUND-CRED-1-LEGACY-PATH: *[Status note 2026-09-28 (`payments`, PRH-2 E2): CLOSED WITH
  REGRESSION GUARD (E2-C1). See the §29 amendment above for the deletion record and
  `internal/txscope/payment_provider_call_guard_static_test.go` for the guard security review
  finding E2-C1 required (a named, non-closure function calling a payments outbound provider
  method directly, with or without a `pgx.Tx` parameter, is now caught tree-wide, closing the
  blind spot IO-1C's own closure-only scan left).]*
- Alert delivery and KS-AUDIT-TENANT-1: NOT IMPLEMENTED, launch-blocking (unchanged).

---

## 11. PROV-OUTBOUND-CRED-1 (approved scope)

| Concern | Rule |
|---|---|
| Resolution point | In phase B only: `providercred.OutboundResolver.Resolve(ctx, pool, tenantID, providerID)` in its own short committed tx (exists). It already refuses under `txscope.Held`. A MOCK uses a `Synthetic` credential source with the **same** refusal, so tests exercise the rule. **Correction (2026-09-27, architect review `rv-prh-architect.md` IO-1B, since fixed):** this line's "same refusal" claim was TRUE for payments only until this date - casino's and KYC's own `MockOutboundResolver.Resolve` ignored `ctx` entirely and never refused. Both now refuse under `txscope.Held(ctx)` too (`internal/casino/mock.go`, `internal/kyc/callcontext.go`), and each domain's phase-B call site (`LaunchGame`; `CreateVerification`/`SubmitVerification`) additionally gained its OWN direct `txscope.Held(ctx)` refusal immediately before the adapter call, mirroring `payments/gate.go`'s own step 1 - defence in depth behind the resolver's refusal, not a replacement for it. See §3.2's own correction above and `internal/txscope`'s `TestINV_IO_1c_NoAdapterCallInsideTxClosure` for INV-IO-1(c)'s static-scan proof. |
| Passing | Through `CallContext.Credential` in every request type (payments §9.1; casino `LaunchRequest.Call`; KYC `CreateVerificationInput.Call` and the `SubmitVerification` call context). The adapter builds a per-call `httpclient.Authenticator` and drops it on return (exists, ADR 0093 §5). |
| Caching | None for `OutboundCredential` beyond one call. Derived tokens are cached only in `DerivedTokenCache`, keyed (tenant, handle, fingerprint) and served only after this call's handle read (exists). The secret value cache stays inside the `Fetcher` (ADR 0094, unchanged). |
| Invalidation and rotation | A handle read happens on every call, so a revoke takes effect from the next call. The in-flight exposure is one call (ADR 0093 precision 1, accepted). Rotation activates a new handle (four-eyes, exists). `DerivedTokenCache.Put` evicts the old fingerprint (exists). |
| Timeout | A resolve budget (`RECOMMENDATION` 2 s, within the Fetcher's own bounds) separate from the provider call timeout. Exceeding it is `NotSent` (T5). |
| Outage | `ErrOutboundCredentialUnavailable` is `NotSent`: no call, no fallback credential, no unauthenticated call, no cascade, and no breaker count against the provider. The attempt stays or returns to `created` with backoff. After `max_credential_retries` (`RECOMMENDATION` 10 over the presence window for deposits), a deposit goes to T3. A payout stays `created` until the operator's M3 or recovery. |
| Tenant and provider isolation | The tenant comes only from the attempt row returned by this tenant's claim tx (S95-C9(i)), and the credential from `Resolve(tenant, provider)`. The gate **and** the adapter assert equality of tenant, provider and domain (§3.2 step 4, §9.1). The worker processes one tenant's items per claim tx. |
| No credential held by an adapter (S95-C8(c)) | A reflection test walks **constructed** registered adapter values recursively (pointers, structs, slices, maps, interfaces and unexported fields) for payments, casino and KYC, and fails on any `OutboundCredential`, `secretstore.Secret`, `httpclient.Authenticator` or derived token. Func-typed fields are forbidden unless allow-listed. A static test forbids package-level variables of those types in adapter packages, and forbids adapter packages importing `secretstore` or the Fetcher. |
| No leakage | Audit, logs, errors and receipts carry `handle_id` and `fingerprint` only. Transport errors are redacted by the gate (§3.2 step 8): no URL query string or userinfo, no vendor response body (which may echo headers). A secret-in-query MOCK test asserts the secret is absent from logs, errors, audit and receipts (S95-C8(a)). `CallContext` has redacting renderers (§9.1). |
| Observability | An alert on the `ErrOutboundCredentialUnavailable` rate per (tenant, provider) (S95-C9(iii)). |
| No credential access while a financial lock is held | This follows from INV-IO-1: phase B holds no tx, and therefore no row, advisory or projection lock. Session-level advisory locks are forbidden on the call path. `txscope` refusal is the runtime backstop. |
| Excluded | AWS IAM, IRSA, KMS, egress proxy, mTLS (PROVIDER DEPENDENT; ADR 0093 A4). |
| Tripwire | `TestOutboundPrecondition_EveryWiredAdapterIsSynthetic` stays. It may be relaxed **per domain** only after that domain's PRH implementation and `security` review. The row pointer it prints is updated to this ADR. |

---

## 12. Payment reconciliation — MOCK foundation (stream `payment_statement`, migration 0103)

### 12.1 Pipeline

1. **Fetch** (no tx). Per (tenant, provider with a registered source), call `Fetch(ctx,
   CallContext, periodStart, periodEnd)` through the §3.2 gate, with the credential resolved
   per §11. The source response body is capped, and a per-import line-count cap applies
   (`RECOMMENDATION` 1 000 000): above it the import is refused, nothing is stored, and a P1
   alert fires (S95-C11).
2. **Ingest** (short tx). Insert one `payment_statement_imports` row and its
   `payment_statement_lines`, both append-only (trigger). The import is idempotent on
   `(tenant, provider, source_label, coverage_start, coverage_end, content_digest)`.
3. **Match** (`WithTenantSnapshot`; REPEATABLE READ required and enforced; transaction-scoped
   advisory lock as in `TryRunCasinoStatementForTenant`). Read the import's lines, the
   `payment_attempts`, the unresolved `payment_provider_events` and the ledger. Write one `reconciliation_runs` row plus
   `reconciliation_mismatches` rows (`persistRun`). **Nothing else is written** (INV-IO-12).
4. **Audit.** The run's audit metadata carries the `import_id`, source label, `is_mock`,
   coverage window and line count, as `CasinoStatementInfo` does.

### 12.2 Why not the casino pattern verbatim

`CasinoStatementSource.Statement(ctx, tx, …)` reads inside the run tx. That is correct only for
a source that renders DB rows (the MOCK). A real source is provider I/O and would violate
INV-IO-1.

The payment stream therefore separates fetch from match, and persists the statement.
Persistence is also the append-only statement storage that casino_statement lists as missing.
Everything else follows the casino_statement pattern: `Stream` constant, `MismatchKind`s,
`persistRun`, snapshot isolation, scheduler wiring, `Label()` containing "MOCK",
state-type findings re-detected every run, and every mismatch a P1.

### 12.3 Keys and deterministic classification

A statement line of provider P resolves only attempts with `provider_id = P` (INV-IO-14,
S95-C1). A payout line matches on either the instruction reference (`provider_reference`) or
the Step B settlement reference (LF95-C13).

For each line, in sorted `(provider_id, provider_reference, kind, line_no)` order:

| Kind | Condition |
|---|---|
| `pay_duplicate` | More than one line with the same `(provider_id, provider_reference, kind)` in an import (reported once per key); or more than one `succeeded` attempt for one intent (platform side) *[AMENDED by §28.9: the platform-side half is now structurally unreachable (migration 0107) and kept as an integrity detector; a new kind `pay_captured_unposted` is added]* |
| `pay_missing_platform_record` | A line (any status) with no attempt resolvable by `(provider_id, provider_reference)` or `merchant_reference`; or a `deposit_reversal` line with no ledger reversal or tombstone under its reference |
| `pay_reference_mismatch` | Resolved by merchant reference, but `attempt.provider_reference` is non-NULL and differs |
| `pay_asset_mismatch` | Asset differs (checked before amount) |
| `pay_amount_mismatch` | Amount differs (compared as `big.Int`; zero tolerance, reconciliation-model §1) |
| `pay_status_mismatch` | Provider `succeeded` versus platform attempt not `succeeded` ("provider-settled, platform-unposted", the LF-C1(b) case); or platform `succeeded` versus provider `declined`; or platform `declined` versus provider `succeeded` |

Then, for each platform attempt in the coverage window, excluding `created`/`rejected` (never
sent) and matched by no line:

| Kind | Condition |
|---|---|
| `pay_missing_provider_record` | The attempt is `succeeded` (ledger posted) |
| `pay_unresolved` | The attempt is `submitting`/`pending`/`ambiguous` and older than the horizon, whether the line is absent or still `pending`. Also: a verified callback receipt still unresolved after the `SettlementWindow` (a possible lost success; LF95-C5, S95-C2(ii)). These are the only age-gated checks, to avoid false P1s inside the settlement window. |

**Ledger join (LF95-C13).** Independently of the statement, in the same match tx:

| Kind | Condition |
|---|---|
| `pay_missing_platform_record` | A `deposit` or `withdrawal_completed` ledger transaction in the window, carrying a `provider_id`, that maps to no `succeeded` attempt (or to more than one) |
| `pay_status_mismatch` | A `succeeded` attempt without exactly one ledger transaction whose `(provider_id, provider_tx_id)` is the deposit's `provider_reference` or the payout's Step B settlement reference |

Join paths (RV-0095 ledger L4):
- **Deposit:** `payment_attempts.ledger_transaction_id`.
- **Payout:** `payment_attempts.withdrawal_request_id → withdrawal_requests.release_ledger_transaction_id`
  (migration 0026), with `transaction_type = 'withdrawal_completed'`.

These rules make the reconciliation key the **ledger's** `(provider_id, provider_tx_id)`, as
reconciliation-model §2.2 requires, not only the attempt row.

`declined` attempts that are absent from the statement are **not** flagged. Whether a real
statement lists declines is PROVIDER DEPENDENT; the MOCK lists them, so the matcher tests cover
both.

**Coverage window.** Platform records outside `[CoverageStart, CoverageEnd)` are never flagged
as missing. This is the general period rule, and it keeps the MOCK honest across restarts.

### 12.4 MOCK source

`payments.MockStatementSource` (`Synthetic`; label "MOCK in-process payment provider
statement — renders the MockProvider's own records, not the platform DB; non-production").

- It renders the **MockProvider's own per-tenant records**. These are tenant-tagged once
  `CallContext` exists (§9.1).
- `CoverageStart` is the MockProvider's construction time.
- This is **not tautological** against the platform DB: a lost callback, an unposted success,
  or a dual-write orphan genuinely diverges.
- It is deterministic (sorted, no clock inside matching). It is single-process (a dev
  limitation, stated in the label).
- Divergence detection is proven by fixture sources in tests, one per kind (§16.3).
- The production guard (`providerkind`) refuses it in production.

### 12.5 Remediation (never through the stream)

| Kind | Remediation path (authoritative mechanisms only) |
|---|---|
| `pay_status_mismatch` (provider succeeded) | A `payments`-owned re-drive job (not the reconciliation stream) reads new `pay_status_mismatch` rows and requests T17 on the named attempts, so a dropped success converges without waiting for an operator (LF95-R1, adopted). An operator may also request T17. Either way: `QueryStatus` → matrix → T7/T13 posting through the normal idempotent path. The stream itself still writes nothing. *[AMENDED by §28.9: every such posting goes through the INV-DEP-1 choke point, so a re-drive can never post for a resolved intent (T10/T13d instead); T17 never changes a terminal attempt other than `declined`; the re-drive job never acts on `pay_captured_unposted`.]* |
| `pay_missing_platform_record` | Investigate: the dual-write orphan class this ADR removes, or foreign or forged activity. Credit only via LEDGER-MANUAL-ADJ-4EYES-1 (BLOCKED). |
| `pay_missing_provider_record`, `pay_amount_mismatch`, `pay_asset_mismatch`, `pay_reference_mismatch`, `pay_duplicate` | Escalate; never auto-resolve (reconciliation-model §2.2(a)); compensation via LEDGER-MANUAL-ADJ-4EYES-1 |
| `pay_unresolved` | The sweeper is already polling; the escalation (T16) alert is the same incident |

### 12.6 Amendment to reconciliation-model.md §2.2(b)

"The reconciliation job itself becomes the trigger to post the missing transaction" is
replaced by: "the reconciliation job flags it; the posting happens only through fresh
provider evidence (`QueryStatus`) applied by the payments state machine".

A statement line is not verified evidence of one payment. Treating a bulk file as posting
authority is exactly the "unreviewed path to arbitrary credits" §2.2 itself warns about.
Accepted by `ledger-finance` (LF-Q3, §21.4); `ledger-finance` edits `reconciliation-model.md`
when this ADR is ACCEPTED, together with the ledger-join rule above.

### 12.7 Implementation record (PRH-I5, `ledger-finance`, 2026-09-27)

Label: **IMPLEMENTED (stream, statement store, matcher, ledger join, tests) against a `MOCK`
source; real PSP statement matching `PROVIDER DEPENDENT`.** Pending `security`, `code-reviewer`
and `qa` gate review.

**Migration number swap.** This section and §13.3 say "migration 0103". The orchestrator swapped the
allocation on 2026-09-27 (commit 07b354b, task registry): payment statement reconciliation is
**migration 0102** (`migrations/0102_payment_statement_reconciliation.{up,down}.sql`), and the kill
switch (§10.2, §13.2) moves to **0103**. Read every "0102 kill switch / 0103 reconciliation"
reference in this ADR as swapped.

**What landed.**
- `statement.PaymentStatementSource`, `PaymentStatementLine`, `PaymentStatement`,
  `PaymentFetchRequest` and the caps `MaxPaymentStatementLines` (1 000 000) /
  `MaxPaymentStatementBodyBytes` (real adapters) in `internal/reconciliation/statement`.
- `internal/reconciliation/payment_statement.go`: `FetchPaymentStatement` (no tx; refused under
  `txscope.Held`; line cap; every field validated with `providerref` and the §13.3 bounds; a line of
  another provider refuses the whole import), `IngestPaymentStatement` (short tx, idempotent on
  the §12.1 key, batched INSERTs because COPY is unavailable under RLS), `RunPaymentStatement`
  (REPEATABLE READ enforced; the eight kinds; the LF95-C13 ledger join both directions; payout
  match on instruction **or** settlement reference; provider-bound resolution; coverage window;
  deferred-receipt check; writes only `persistRun`). `TryRunPaymentStatementForTenant` (advisory
  lock `reconciliation:payment_statement:<tenant>`), `ReconcilePaymentStatementForTenant` (the three
  phases, run audit carrying `import_id`, label, `is_mock`, coverage and line count; any phase
  failure is audited in a fresh tx with `severity=P1` and logged at Error level), and sweep wiring
  (`RunSweep`/`RunSweepTenants`/`RunSchedulerLoop` take trailing payment sources;
  `cmd/platform-api` wires the MOCK).
- Migration 0102: both tables append-only (UPDATE/DELETE/TRUNCATE denied, owner included), FORCE
  RLS with `tenant_staff_scope`, all §13.3 CHECKs, `line_count` bound to the stored rows (statement
  trigger refuses an over-count append; deferred constraint trigger refuses a short import at
  commit), a `CHECK` that a MOCK import's label contains `MOCK`, the pay_* kinds as a strict
  superset of 0098, and runtime grants SELECT/INSERT only (re-asserted in `deploy/init-app-role.sql`).
  The down migration refuses once any import or pay_* mismatch exists (constraint-validation
  technique, not blinded by RLS).
- `payments.MockStatementSource` (`internal/payments/mock_statement_source.go`, `Synthetic`,
  refused in production by the startup guard): renders the MockProvider's own per-tenant records,
  coverage start = the MockProvider's construction time, fetched through the provider-call gate as a
  read-only call with the MOCK outbound credential. Two narrow, additive edits outside that file:
  the gate also carries its `CallContext` on the adapter's ctx (`gate.go`), and the MockProvider
  tags each record with that tenant, the merchant reference and the time (`mock.go`).
  `MockCredentialResolver` gained its `SyntheticComponent` marker (now wired into the bundle).
- Tests: `internal/reconciliation/payment_statement_integration_test.go` (§16.3 list, including a
  statement-capture test, a REPEATABLE READ snapshot test with a READ COMMITTED kill control, RLS
  cross-tenant, migration up/down and grants) and
  `TestInitAppRole_RerunKeepsPaymentStatementGrants`. Mutation evidence: 28/28 killed
  (`docs/plans/payment-readiness/evidence/prh-i5-mutation-kill.txt`, harness `prh-i5-mutate.py`;
  MX9 = PM23).

**Deviations from the design text, recorded.**
1. **§9.5 `Fetch(ctx, CallContext, …)`.** The leaf cannot import `internal/payments` (import cycle),
   so `Fetch` takes a `PaymentFetchRequest` carrying only the server-side tenant and provider. The
   real `CallContext` is built inside `payments` by the gate (credential resolved, binding checked,
   deadline applied). S95-C11's "`CallContextLike` is looser" concern is met by keeping the leaf
   type credential-free rather than a look-alike: nothing credential-shaped crosses the leaf.
2. **Status rules the §12.3 table leaves open**, decided conservatively and disclosed in the code:
   a provider `reversed` deposit line counts as provider-succeeded; a `disputed` attempt is excluded
   from status comparison (already a payments P1, no automated remedy); provider-declined versus a
   platform in-flight attempt is age-gated as `pay_unresolved`, like provider-pending.
3. **Provider-succeeded versus platform in-flight is not age-gated**, exactly as §12.3 says. A
   near-real-time real source may need a short grace for callback latency; `PROVIDER DEPENDENT`,
   decided with the first real source.
4. **Horizon reference time** is the import's `coverage_end`, not the wall clock, so a run is
   deterministic for a given import and snapshot. `DefaultPaymentUnresolvedHorizon` = 24 h =
   `payments.DefaultSettlementWindow` (asserted by a test).
5. **Deferred receipts:** only `disposition_at_receipt = 'deferred_unresolved'` receipts count
   toward `pay_unresolved`; anomaly/unsupported receipts are surfaced by the payments anomaly path.

**Not implemented / deferred.**
- **LF95-R1 automatic re-drive job: `NOT IMPLEMENTED` (deferred).** It is `payments`-owned and
  would have to be built inside `internal/payments` while the deposit cutover is in flight there.
  Remediation today is the existing path: an operator T17 (`payments.Touch`) then the sweeper's
  `QueryStatus` → evidence → T7 posting. The end-to-end §16.3 test proves exactly that path
  (flagged → T17 → sweeper posts → next run clean). Mismatch keys carry `attempt=<uuid>` so the
  job can consume them without parsing free text beyond that token.
- **MOCK payouts:** payout dispatch through the gate is not wired yet (PRH-I1), so the MOCK lists no
  payouts today; payout matching (instruction and settlement reference) and the payout ledger join
  are proven with fixture sources.
- **MOCK limits:** in-process, single-replica, reset on restart, lists no reversals; records from
  the legacy gate-less deposit path are untagged and appear on no tenant's statement. Each hourly run
  stores the MOCK's all-time statement again (new coverage end, new import): dev-only growth.
- Scale: O(tenant history) per run, like every stream (CAS-RECON-SCALE-1).

**Finding for `payments` (not fixed here, outside PRH-I5 scope).** A late success receipt on a
`declined` deposit attempt whose sibling already succeeded (T13, a second capture) fails in
`ApplyReceiptEvidence` with a `payment_attempts_tenant_ledger_tx` unique violation:
`applyDepositSuccessAndPost` links the attempt to `updated.LedgerTransactionID`, which is the
intent's **first** posting (LF95-C6(a)), not the new one. Result: the verified success is rolled back
and redelivered forever. `TestPaymentStatement_Kind_DuplicatePlatformSuccess` builds the T13 state
directly for that reason. *[SUPERSEDED by §28: a second capture is no longer posted, so this
unique-violation path disappears; the test is inverted per §28.12.]*

#### 12.7.1 Fix round (reviews RV-PRH-I5 code NOT READY / security APPROVE with C1, C2)

**F1 (blocking): live legacy postings. `ledger-finance` ruling: option (b), interim exclusion,
disclosed and self-retiring.** Until the payments cutover, the live deposit path
(`Orchestrator.InitiateDeposit`) and live withdrawal completion (`withdrawal.Complete`) create no
`payment_attempts` row. As wired, every such posting was a `pay_missing_platform_record` P1 on
every hourly run: a saturated signal, which is worse than none.

Rule (in `checkLedgerJoin`, direction (b) only): a `deposit` or `withdrawal_completed` posting with
**zero** succeeded attempts is **not** a mismatch when it is linked from a deposit intent
(`deposit_intents.ledger_transaction_id`) or a withdrawal request
(`withdrawal_requests.release_ledger_transaction_id`) that has **no `payment_attempts` row at all**.
It is counted instead as `legacy_unattempted`, in `PaymentStatementInfo` and the audit record of
every run.

What stays a P1:
- a posting linked from nothing (the orphan / dual-write class);
- a posting whose intent or request has any attempt but no succeeded one;
- a posting mapped to more than one succeeded attempt;
- every direction-(a) and statement-side rule.

The exclusion is keyed on the absence of attempts, not on a flag or a date. So it retires itself:
once the cutover makes every new intent and request carry an attempt, new postings are fully
checked and `legacy_unattempted` stops growing. Postings made on the legacy path between 0101 and
the cutover stay excluded (the 0101 backfill already gave earlier ones attempts). **Cutover
acceptance criterion for `payments`:** `legacy_unattempted` must not increase after the cutover. An
increase means a live path still bypasses `payment_attempts`.

Label: stream `IMPLEMENTED` against the `MOCK`. The LF95-C13 ledger join is `PARTIALLY
IMPLEMENTED` for legacy-path postings until the cutover.

Tests:
- `TestPaymentStatement_LegacyUnattemptedPostingsAreCountedNotFlagged` runs the real live deposit
  path and a live-style completion against the wired MOCK source. Result: no mismatch,
  `legacy_unattempted=2`, audited.
- `TestPaymentStatement_AttemptLinkedPostingsStayFullyChecked`: a v2 deposit is clean with
  legacy 0, and a completion whose withdrawal has a non-succeeded payout attempt is still a P1.

**F2 (Medium).** In-flight ageing (`pay_unresolved`, no line) is no longer coverage-gated. The
coverage window now protects only `pay_missing_provider_record`. `aged()` still implies
`sent_at < coverage_end`. Before this change, a window no longer than the horizon could never
flag.

**F3 (Low).** A `pending` or `declined` `deposit_reversal` line with no posting is not a finding,
because no posting is expected (for example, a chargeback the merchant won). A `succeeded` or
`reversed` one still is.

**F4 (Low).** When a payout line carries `settlement_reference` and the attempt's
`withdrawal_completed` has one, they must be equal. Otherwise the line is a
`pay_reference_mismatch check=settlement_reference`.

**F5 (Low).** `FetchPaymentStatement` refuses a `CoverageEnd` later than the fetch time plus
`PaymentCoverageMaxClockSkew` (5 min).

**Security C1.** `internal/reconciliation/statement/payment_limits.go` adds three enforcement
primitives that a source must use:
- `LimitPaymentStatementBody`: reads through `io.LimitReader(body, max+1)` and fails with
  `ErrPaymentStatementBodyTooLarge` rather than truncating;
- `PaymentLineCollector`: refuses line `max+1` with `ErrPaymentStatementTooManyLines`;
- `DecodePaymentStatementJSONLines`: a streaming NDJSON decoder that applies both.

The interface doc makes their use mandatory for a wire source. The stream maps either sentinel
from `Fetch` to `ErrPaymentStatementTooLarge`: refused, nothing stored, P1. The MOCK collects
through the line collector. The unit tests use an endless body to prove the decoder streams: it
stops at each cap after reading only a bounded prefix. `security` still verifies C1 in the first
real adapter's code.

**Security C2.**
- `merchant_reference` is checked in Go with the `providerref` rule (valid UTF-8, no C0, DEL or C1
  character) plus the 64-byte bound.
- `asset_code` must match `^[A-Z0-9]{1,16}$`, the Asset registry's code shape.
- A NUL, newline or escape therefore refuses the import at fetch, before any insert.

**Not done:** the matching DB CHECK needs a forward migration. 0103 and 0104 are already
allocated, so the orchestrator must allocate a number. It still blocks the first real source, as
C2 says. The Go check is the control today. **Update (2026-09-27): done in migration 0104**
(`0104_payment_statement_line_charset`). It adds CHECKs on `payment_statement_lines.merchant_reference`
(the providerref rule, 1..64 bytes) and `asset_code` (`^[A-Z0-9]{1,16}$`). A per-tenant, RLS-scoped
pre-flight counts violating rows and aborts without changing any row. The down migration drops both
CHECKs. Tests insert through the database directly: NUL, newline, escape, C1 and DEL characters,
and lower-case, formula-style and spaced asset codes. There is a 0102-only kill control. Mutation
evidence: PM47-PM51, 51/51 killed.

**Mutation evidence.** The surviving mutants RM2 to RM6 now have tests and are killed. The harness
rejects build failures, so a mutant that does not compile no longer counts as killed. The result
is 46/46 killed (`evidence/prh-i5-mutation-kill.txt`).

**Pre-existing failures, registered here (not PRH-I5):**
- `TestMigration0100_UpDownUpRoundTrip` and `TestMigration0100_DownRefusesWhileDecisionsHoldRows`
  hard-code the chain tip at 100;
- `TestMigration0082_DepositIntentsDenyTruncate` fails because of the foreign key from 0101.

---

## 13. Data model sketch

All tables: `tenant_id NOT NULL`, `ENABLE` + `FORCE ROW LEVEL SECURITY`, `tenant_staff_scope`
policy (the 0025 pattern, which requires `app.player_account_id` unset), no player policy
(players read intents, not attempts), excluded from CDC. Every application-level length bound
equals its DB CHECK (one constant per field, S95-C2(iii)).

**Migration numbering (orchestrator re-allocation, 2026-09-27):** 0099 PROVIDER-REF-BOUND-1;
0100 KYC enforcement policy and decisions (ADR 0096); **0101 payment attempts, receipts,
triggers and backfill (this ADR; formerly allocated 0100)**; 0102 kill switch; 0103 payment
statement reconciliation.

### 13.1 Migration 0101 — payment attempts and provider-operation state (after 0099)

```sql
CREATE TABLE payment_attempts (
  id                       UUID PRIMARY KEY,
  tenant_id                UUID NOT NULL,
  operation                TEXT NOT NULL CHECK (operation IN ('deposit','payout')),
  deposit_intent_id        UUID NULL REFERENCES deposit_intents(id),
  withdrawal_request_id    UUID NULL REFERENCES withdrawal_requests(id),
  CHECK ((operation = 'deposit') = (deposit_intent_id IS NOT NULL)),
  CHECK ((operation = 'payout')  = (withdrawal_request_id IS NOT NULL)),
  attempt_no               INT  NOT NULL CHECK (attempt_no >= 1),
  provider_id              TEXT NULL,
  excluded_provider_ids    TEXT[] NOT NULL DEFAULT '{}',
  payment_method           TEXT NOT NULL,       -- 'legacy_unknown' allowed only when legacy_backfill (below)
  asset_code               TEXT NOT NULL,
  amount                   NUMERIC(38,0) NOT NULL CHECK (amount > 0),
  interactive              BOOLEAN NOT NULL,
  legacy_backfill          BOOLEAN NOT NULL DEFAULT false,                      -- LF95-C11(c)
  CHECK (payment_method <> 'legacy_unknown' OR legacy_backfill),
  CHECK (last_evidence_kind <> 'legacy' OR legacy_backfill),                     -- RV-0095 ledger N2
  merchant_reference       TEXT NOT NULL,       -- = id::text (INV-IO-3); for backfilled rows id = the parent id
  external_idempotency_key TEXT NOT NULL,       -- = 'pa:' || id::text (never sent for legacy rows; T12 forbidden)
  provider_reference       TEXT NULL CHECK (octet_length(provider_reference) <= PROVIDER_REF_MAX /* from 0099 */),
  state                    TEXT NOT NULL CHECK (state IN ('created','submitting','pending','ambiguous',
                                                          'succeeded','declined','rejected','disputed')),
  last_evidence_kind       TEXT NOT NULL CHECK (last_evidence_kind IN
                               ('sync','callback','query_status','sweeper','operator','platform','legacy')),  -- LF95-C2
  ever_possibly_sent       BOOLEAN NOT NULL DEFAULT false,
  CHECK (state <> 'created' OR NOT ever_possibly_sent),               -- INV-IO-9
  CHECK (state IN ('created','rejected') OR provider_id IS NOT NULL), -- rejected may be unrouted; T15 needs provider_id = verified provider
  CHECK (state <> 'pending' OR provider_reference IS NOT NULL),
  CHECK (state <> 'succeeded' OR provider_reference IS NOT NULL),     -- LF95-C4
  CHECK (operation <> 'deposit' OR state <> 'succeeded' OR ledger_transaction_id IS NOT NULL),
  submit_count             INT NOT NULL DEFAULT 0,
  claim_token              UUID NULL, lease_owner TEXT NULL, lease_until TIMESTAMPTZ NULL,
  first_submitted_at       TIMESTAMPTZ NULL,    -- set once by the first claim (DB clock); S95-C3
  last_sent_at             TIMESTAMPTZ NULL,    -- set by every claim (T1+T2, T1p, T2, T12); LF95-C8(b)
  next_action_at           TIMESTAMPTZ NULL,
  CHECK (state NOT IN ('succeeded','declined','rejected','disputed') OR next_action_at IS NULL),
  poll_count               INT NOT NULL DEFAULT 0,
  escalated_at             TIMESTAMPTZ NULL,
  decline_reason           TEXT NULL CHECK (octet_length(decline_reason) <= 64),   -- canonical code, S95-C10
  decline_stage            TEXT NULL CHECK (decline_stage IN ('at_submission','after_acceptance')),
  cascadable               BOOLEAN NULL,
  terminal_reason          TEXT NULL CHECK (octet_length(terminal_reason) <= 64),  -- canonical code, S95-C10
  ledger_transaction_id    UUID NULL REFERENCES ledger_transactions(id),   -- this attempt's deposit posting (LF95-C6(a))
  created_at, accepted_at, resolved_at, updated_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX ON payment_attempts (tenant_id, merchant_reference);
CREATE UNIQUE INDEX ON payment_attempts (tenant_id, external_idempotency_key);
CREATE UNIQUE INDEX ON payment_attempts (tenant_id, provider_id, provider_reference) WHERE provider_reference IS NOT NULL;
CREATE UNIQUE INDEX ON payment_attempts (tenant_id, ledger_transaction_id) WHERE ledger_transaction_id IS NOT NULL;
CREATE UNIQUE INDEX ON payment_attempts (tenant_id, deposit_intent_id, attempt_no) WHERE operation = 'deposit';
CREATE UNIQUE INDEX payment_attempts_one_live_per_intent ON payment_attempts (tenant_id, deposit_intent_id)
  WHERE operation = 'deposit' AND state IN ('created','submitting','pending','ambiguous');          -- INV-IO-8
CREATE UNIQUE INDEX payment_attempts_one_per_withdrawal ON payment_attempts (tenant_id, withdrawal_request_id)
  WHERE operation = 'payout';                                                                         -- INV-IO-8
CREATE INDEX idx_payment_attempts_due ON payment_attempts (tenant_id, next_action_at) WHERE next_action_at IS NOT NULL;
-- payment_attempts_guard (BEFORE UPDATE):
--   * allowed (OLD.state, NEW.state) pairs = §4.3 exactly, including the "Forbidden" list;
--   * last_evidence_kind rules (LF95-C2): NEW.state='declined' AND operation='payout' requires
--     NEW.last_evidence_kind IN ('sync','callback','query_status'); NEW.state='declined' for a deposit
--     requires <> 'operator'; NEW.state='succeeded' requires IN ('sync','callback','query_status');
--   * T12 (ambiguous→submitting) rejected when legacy_backfill (LF95-C11(c));
--   * a payout →rejected only from OLD.state='created' AND NOT OLD.ever_possibly_sent (M3, S95-C13);
--   * immutable: id, tenant_id, operation, subject ids, attempt_no, amount, asset_code, payment_method,
--     interactive, legacy_backfill, merchant_reference, external_idempotency_key, first_submitted_at once set;
--     provider_id, provider_reference and ledger_transaction_id NULL→value once;
--     ever_possibly_sent false→true only; submit_count and last_sent_at monotonic.
-- payment_attempts_insert_guard (BEFORE INSERT; RV-0095 ledger N2): the only INSERT shapes are T1 ('created'),
--   T1+T2 and T1p ('submitting'). It requires NEW.state IN ('created','submitting'), NOT NEW.legacy_backfill,
--   NEW.last_evidence_kind <> 'legacy', NOT NEW.ever_possibly_sent, NEW.ledger_transaction_id IS NULL,
--   NEW.provider_reference IS NULL. Created in migration 0101 AFTER the backfill statement has run, so no
--   session-setting escape exists: `legacy` rows can come only from the migration's own backfill.
-- The UPDATE guard likewise never lets legacy_backfill or last_evidence_kind become 'legacy'.
-- payment_attempts_no_delete (BEFORE DELETE): RAISE.

CREATE TABLE payment_provider_events (   -- verified callback receipts (INV-IO-10)
  id UUID PRIMARY KEY, tenant_id UUID NOT NULL, provider_id TEXT NOT NULL,
  event_type TEXT NOT NULL CHECK (event_type IN ('deposit','deposit_reversal','payout','payout_returned')),
  provider_reference TEXT NOT NULL CHECK (octet_length(provider_reference) <= PROVIDER_REF_MAX),
  original_provider_reference TEXT NULL CHECK (octet_length(original_provider_reference) <= PROVIDER_REF_MAX),
  merchant_reference TEXT NULL CHECK (octet_length(merchant_reference) <= 64),
  settlement_reference TEXT NULL CHECK (octet_length(settlement_reference) <= PROVIDER_REF_MAX),
  outcome TEXT NOT NULL CHECK (outcome IN ('pending','succeeded','declined','ambiguous')),
  amount NUMERIC(38,0) NULL, asset_code TEXT NULL CHECK (octet_length(asset_code) <= 16),
  CHECK (outcome <> 'succeeded' OR (amount IS NOT NULL AND asset_code IS NOT NULL)),       -- LF95-C4
  cascadable BOOLEAN NULL,                                                                   -- LF95-C4
  decline_stage TEXT NULL CHECK (decline_stage IN ('at_submission','after_acceptance')),    -- LF95-C4
  decline_reason TEXT NULL CHECK (octet_length(decline_reason) <= 64),                      -- canonical code
  CHECK (outcome <> 'declined' OR (cascadable IS NOT NULL AND decline_stage IS NOT NULL)),
  event_fingerprint BYTEA NOT NULL,
  disposition_at_receipt TEXT NOT NULL CHECK (disposition_at_receipt IN
      ('applied','duplicate_effect','deferred_unresolved','anomaly','unsupported_event')),
  attempt_id UUID NULL REFERENCES payment_attempts(id),   -- one-shot NULL→value
  resolution TEXT NULL CHECK (resolution IN ('applied','anomaly_cross_provider','anomaly_reference_conflict',
      'anomaly_predates_submission','anomaly_other')),     -- one-shot NULL→value (S95-C3)
  resolved_at TIMESTAMPTZ NULL,                            -- one-shot NULL→value; forced to now() by trigger (RV-0095 L3)
  received_at TIMESTAMPTZ NOT NULL DEFAULT now(),          -- forced to now() by BEFORE INSERT trigger; client values ignored (L3)
  UNIQUE (tenant_id, provider_id, event_fingerprint)
);
CREATE INDEX payment_provider_events_unresolved ON payment_provider_events (tenant_id, provider_id, provider_reference)
  WHERE resolved_at IS NULL;                               -- also the S95-C2 cap probe index
-- guard trigger: append-only except the three one-shot columns; no DELETE. No raw body, header, signature,
-- key id or credential is ever stored. Retention (PAY-ATTEMPT-RETENTION-1) must never delete receipts of
-- non-terminal attempts, or receipts younger than the longest declared WebhookRetrySemantics.RetryWindow
-- (S95-C3); `security` reviews that design.
```

**Backfill with pre-flight (LF95-C11; synthetic/dev data only, but correct regardless).** The
backfill writes **no ledger row**. It runs as one migration transaction, in this order:
create the tables and the UPDATE guard, run the pre-flight, run the backfill INSERTs, then
create the INSERT guard (RV-0095 ledger N2). The pre-flight aborts, listing the offending ids,
before any write, and never coerces.

**Columns set on every backfilled row (RV-0095 ledger N4):**

- `amount`/`asset_code`: the parent's own amount and asset, never re-derived.
- `next_action_at`: `now()` for every non-terminal row (`pending`/`ambiguous`), so the sweeper
  polls it; NULL for terminal rows.
- `first_submitted_at`: the parent's `created_at` (intent `created_at`, withdrawal
  `requested_at`). That is the earliest instant a genuine callback could describe, so every
  genuine receipt qualifies under §6.4.
- `last_sent_at`: the migration's `now()`. That is the latest plausible send, so not-found
  authority (§4.5) is delayed by a full Δ and is never premature.
- `interactive`: `true` for deposits (the §10.1 "unknown means true" default), so an async
  decline of a legacy intent finalizes it and never cascades unattended. `false` for payouts.
- `ever_possibly_sent`: `true` for every backfilled payout, and for every deposit that has a
  `provider_id`.
- `excluded_provider_ids`: `'{}'`. This is not needed, because legacy rows never cascade.
- `last_evidence_kind`: `'legacy'`.

| Source row | Backfilled attempt |
|---|---|
| Every `deposit_intents` row, whatever its status | `id = intent.id` (so `merchant_reference = id::text` equals the value actually sent as `MerchantReference`, LF95-C11(b)); `attempt_no = 1`; `legacy_backfill = true`; `last_evidence_kind = 'legacy'`; `ever_possibly_sent = true` unless the intent has no `provider_id` (RG-declined, never sent) |
| intent `pending`/`ambiguous` **with** `provider_reference` | state `pending` / `ambiguous` |
| intent `pending`/`ambiguous` **without** `provider_reference` | state `ambiguous` (it may have reached the provider). **Pre-flight abort** if its `provider_id` is NULL. |
| intent `succeeded` | state `succeeded`, `ledger_transaction_id` copied. **Pre-flight abort** if the intent's `ledger_transaction_id` is NULL or its `provider_reference` is NULL. |
| intent `declined`/`failed` | state `declined` (`failed` maps to `declined`), `cascadable = false`; with no `provider_id`: state `rejected` |
| `withdrawal_requests` `submitted` | `id = withdrawal.id`; state `pending` (the reference is already set; **pre-flight abort** if it is NULL) |
| `completed` | `succeeded` |
| `failed` | `declined`, `last_evidence_kind = 'legacy'` (possible only because the INSERT guard is created after the backfill; application code can never write `legacy`) |
| `reversed` | `succeeded` (it was omitted before; LF95-C11(d)) |
| withdrawal `payment_method` | `withdrawal_requests` has no such column, so every backfilled payout gets the sentinel `'legacy_unknown'`, allowed only with `legacy_backfill` (LF95-C11(e)); nothing is invented |

Post-checks inside the same transaction (LF95-C11(g)): `SUM(debits) == SUM(credits)` and
projection == rebuild are unchanged; every non-terminal intent and every `submitted` withdrawal
has exactly one live attempt; no two backfilled rows violate a new unique index. Any failure
rolls the whole migration back.

`deposit_intents` and `withdrawal_requests` get **no schema change**. The intent status CHECK
is unchanged. `withdrawal_requests.provider_reference` is already nullable; only its
application-level meaning changes (§4.7).

### 13.2 Migration 0102 — kill switch

```sql
CREATE TABLE payment_kill_switches (
  id UUID PRIMARY KEY, tenant_id UUID NOT NULL,
  provider_scope TEXT NOT NULL,                       -- '*' or a provider_id (charset per webhookauth.ValidProviderID); immutable
  operation_scope TEXT NOT NULL CHECK (operation_scope IN ('deposit','payout','*')),   -- immutable (N1)
  engaged BOOLEAN NOT NULL,
  engaged_by_scope TEXT NOT NULL CHECK (engaged_by_scope IN ('platform','tenant')),   -- forced from the session (N2)
  release_request_id UUID NULL,                       -- the consumed release request (N4, ADR 0093 A1 shape)
  reason_code TEXT NOT NULL CHECK (octet_length(reason_code) BETWEEN 1 AND 64),
  changed_by UUID NOT NULL,                           -- forced from the session principal
  changed_by_scope TEXT NOT NULL CHECK (changed_by_scope IN ('platform','tenant')),   -- forced from the session
  changed_at TIMESTAMPTZ NOT NULL,                    -- forced (now())
  version BIGINT NOT NULL,
  UNIQUE (tenant_id, provider_scope, operation_scope),
  UNIQUE (tenant_id, id),                             -- target of the requests' composite FK (L4)
  UNIQUE (release_request_id));                       -- one request releases at most once (N4)
CREATE TABLE payment_kill_switch_release_requests (
  id UUID PRIMARY KEY, tenant_id UUID NOT NULL, kill_switch_id UUID NOT NULL,
  FOREIGN KEY (tenant_id, kill_switch_id) REFERENCES payment_kill_switches (tenant_id, id),       -- L4: no cross-tenant reference
  UNIQUE (tenant_id, id),
  expected_version BIGINT NOT NULL,
  requested_by UUID NOT NULL,                                                        -- forced from the session
  requested_by_scope TEXT NOT NULL CHECK (requested_by_scope IN ('platform','tenant')),   -- forced (N2)
  reason_code TEXT NOT NULL CHECK (octet_length(reason_code) BETWEEN 1 AND 64),
  approved_by UUID NULL CHECK (approved_by IS DISTINCT FROM requested_by),          -- forced from the session
  approved_by_scope TEXT NULL CHECK (approved_by_scope IN ('platform','tenant')),  -- forced (N2)
  status TEXT NOT NULL CHECK (status IN ('open','approved','cancelled','expired')),
  created_at TIMESTAMPTZ NOT NULL,                                                   -- forced
  expires_at TIMESTAMPTZ NOT NULL,                                                   -- forced <= created_at + 24 h
  CHECK (expires_at <= created_at + interval '24 hours'),
  decided_at TIMESTAMPTZ NULL, decided_txid BIGINT NULL,                             -- forced at open→approved (N4)
  CHECK ((status = 'approved') = (approved_by IS NOT NULL AND decided_txid IS NOT NULL)));
ALTER TABLE payment_kill_switches ADD FOREIGN KEY (tenant_id, release_request_id)
  REFERENCES payment_kill_switch_release_requests (tenant_id, id);                  -- N4 (deferrable not needed: request row exists first)
CREATE UNIQUE INDEX ON payment_kill_switch_release_requests (kill_switch_id) WHERE status = 'open';
-- Session-actor derivation (§10.2.1): a shared trigger function computes (principal, scope) from
--   app.tenant_id + app.principal_id (tenant) or app.platform_admin_principal_id with app.tenant_id unset
--   (platform), verifies the staff_users row (tenant_id = app.tenant_id, or tenant_id IS NULL) as migration
--   0044 does, raises on any other shape, and overwrites every *_by / *_by_scope column with it.
-- payment_kill_switches_guard (§10.2.2): no DELETE; id, tenant_id, provider_scope, operation_scope immutable;
--   version strictly monotonic; engage forces engaged_by_scope := session scope and release_request_id := NULL;
--   while engaged, engaged_by_scope never platform→tenant, and a tenant session may not UPDATE a
--   platform-engaged row; true→false requires NEW.release_request_id → request with kill_switch_id = id,
--   status 'approved', expected_version = OLD.version, approved_by <> requested_by, now() < expires_at,
--   decided_txid = txid_current(), and (OLD.engaged_by_scope = 'platform' ⇒ both request scopes 'platform');
--   release_request_id is forced to NULL on every INSERT (L5(b)) and is otherwise immutable.
-- release_requests guard (N5, L5(a)): id, tenant_id, kill_switch_id, expected_version, reason_code, requested_by,
--   requested_by_scope, created_at, expires_at set at INSERT (actor/time forced; expires_at <= created_at + 24 h)
--   and immutable; approved_by, approved_by_scope, decided_at, decided_txid forced at open→approved only, NULL
--   otherwise, immutable once set; status only open→approved|cancelled|expired; terminal rows fully immutable;
--   tenant-session INSERT refused for a platform-engaged switch; approval refused for a non-platform approver
--   when the switch is platform-engaged; no DELETE.
-- RLS, both tables (FORCE): tenant family (tenant_id = app.tenant_id; app.player_account_id and
--   app.platform_admin_principal_id unset) and platform family (migration 0075 precedent:
--   app.platform_admin_principal_id set; app.tenant_id and app.player_account_id unset); each SELECT, INSERT,
--   UPDATE only; no DELETE policy; no FOR ALL.
-- No manifest table: the manifest is code-declared (ADR 0022 §2.1). History of switch changes = audit_log.
```

### 13.3 Migration 0103 — payment statement reconciliation (MOCK)

```sql
CREATE TABLE payment_statement_imports (
  id UUID PRIMARY KEY, tenant_id UUID NOT NULL, provider_id TEXT NOT NULL,
  source_label TEXT NOT NULL CHECK (octet_length(source_label) <= 256), is_mock BOOLEAN NOT NULL,
  coverage_start TIMESTAMPTZ NOT NULL, coverage_end TIMESTAMPTZ NOT NULL, CHECK (coverage_end > coverage_start),
  line_count INT NOT NULL CHECK (line_count BETWEEN 0 AND 1000000),          -- S95-C11
  content_digest BYTEA NOT NULL, fetched_at TIMESTAMPTZ NOT NULL,
  UNIQUE (tenant_id, provider_id, source_label, coverage_start, coverage_end, content_digest));
CREATE TABLE payment_statement_lines (
  id UUID PRIMARY KEY, tenant_id UUID NOT NULL, import_id UUID NOT NULL REFERENCES payment_statement_imports(id),
  line_no INT NOT NULL, provider_id TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('deposit','deposit_reversal','payout')),
  provider_reference TEXT NOT NULL CHECK (octet_length(provider_reference) <= PROVIDER_REF_MAX),
  merchant_reference TEXT NULL CHECK (octet_length(merchant_reference) <= 64),                          -- S95-C11
  original_provider_reference TEXT NULL CHECK (octet_length(original_provider_reference) <= PROVIDER_REF_MAX),
  settlement_reference TEXT NULL CHECK (octet_length(settlement_reference) <= PROVIDER_REF_MAX),      -- LF95-C13
  status TEXT NOT NULL CHECK (status IN ('pending','succeeded','declined','reversed')),
  amount NUMERIC(38,0) NOT NULL, asset_code TEXT NOT NULL CHECK (octet_length(asset_code) <= 16),
  occurred_at TIMESTAMPTZ NOT NULL,
  UNIQUE (import_id, line_no));   -- duplicates across line_no are kept so pay_duplicate is detectable
-- both: append-only triggers (no UPDATE/DELETE), FORCE RLS.
-- reconciliation_mismatches.mismatch_kind widened (strict superset of 0098) with:
--   pay_missing_platform_record, pay_missing_provider_record, pay_amount_mismatch, pay_asset_mismatch,
--   pay_reference_mismatch, pay_status_mismatch, pay_duplicate, pay_unresolved.
```

---

## 14. Lock ordering — ADR 0082 Amendment A7 (accepted by `ledger-finance`, §21.3; written into ADR 0082 when this ADR is ACCEPTED)

- **L1 list extended.** `deposit_intents` and `payment_attempts` are appended to the L1 table
  list, in that order, after the existing tables. So `withdrawal_requests` comes before
  `payment_attempts`, and `deposit_intents` comes before `payment_attempts`.
- **R0, between L0 and L1** (LF95-C9(a)). The `payment_provider_events` receipt-key insert is
  an index-insertion wait that serializes duplicate deliveries, analogous to L0.1. A tx takes
  **at most one** R0, as its **first write**, and only in a callback tx or in a separate-tx
  rejection/`unsupported_event` record. Nothing that already holds an L1+ lock ever inserts a
  receipt. Deadlock-freedom depends on exactly that, so the rule is binding.
- **Parent before attempt, everywhere** (LF95-C9(c)). Every tx that locks a
  `payment_attempts` row locks its parent (`deposit_intents` or `withdrawal_requests`) first,
  including T3, T5, the per-item claim tx and per-item phase C.
- **Receipt one-shot updates** (`attempt_id`, `resolution`, `resolved_at`) happen only while
  the tx holds the attempt's parent **and** attempt locks, in ascending receipt id. The sweeper
  backstop locks the parent and the attempt first (LF95-C9(b)).
- **The batch lease tx is the sole exception** (LF95-C9(d)). It uses `FOR UPDATE SKIP LOCKED`
  only, writes only lease columns (never a state, never T2), never locks or writes a parent
  row, and never waits on a lock. Ordering it by `next_action_at` instead of by id is safe only
  because nothing in it blocks.
- **Kill-switch read takes no lock.** It is a plain `NOT EXISTS` subquery inside the claim
  statement.
- **Per-path order:**
  - deposit evidence: R0 (callback tx only) → intent `FOR UPDATE` → attempt CAS → (L2 original,
    reversal only) → L3 (`LockProjectionsForPosting` inside `Post`) → L4;
  - payout evidence: withdrawal `FOR UPDATE` → attempt CAS → L3 → L4;
  - payout claim and W-KYC: withdrawal `FOR UPDATE` (L1) → KYC gate reads → (`DenyForCompliance`
    posting: L3 → L4) or (attempt INSERT);
  - deposit T2 per-item claim (sweeper and player resume; RV-0095 ledger N1): RG
    `EvaluateEligibility` (L0.4 person advisory) and KYC deposit gate reads → intent `FOR UPDATE`
    → attempt CAS. It never takes an advisory lock after a row lock (R8). The player T1+T2 phase A
    already runs RG before its INSERTs;
  - payout T2/T12 per-item claim: withdrawal `FOR UPDATE` → attempt → payout KYC gate (no lock)
    → CAS;
  - cascade insert: while holding intent and attempt, insert attempt n+1. The partial unique
    index is the only wait, and it is a key the concurrent contender needs under the same
    intent lock, so there is no cycle.
- **Harness** (LF95-C9(e)). The §16.2 item 16 harness adds the sweeper lease and claim racing a
  callback and phase C on the same intent and on the same withdrawal, and the sweeper deposit T2
  re-claim racing an RG self-exclusion write for the same person (RV-0095 ledger N1).
- **No lock is held across I/O.** This is the whole point of the ADR. The L1 lock that
  `LockApprovedForSubmission` held across `Withdraw` today is removed.

---


## 15. Casino launch and KYC (same analysis, minimal redesign)

### 15.1 Casino launch (`casino.LaunchGame`)

| Aspect | Specification |
|---|---|
| Intent | The `casino_launch_sessions` row (exists), `status='active'`, bounded by `expires_at`. No new state. |
| Reference / idempotency key | `SessionID` (exists in `LaunchRequest`) is the deterministic external reference. The vendor's session reference is not persisted today (unchanged). |
| Flow | Phase A: all existing checks (RG, risk, jurisdiction, capability, `supports_bet`) plus `CreateLaunchSession` plus audit `casino.launch_requested` → commit (this releases the L0.4 and L0.5 advisory locks). Health: snapshot outside the tx (§9.6 contract applied to `CasinoProvider.HealthStatus`). Phase B: resolve credential → `Launch(LaunchRequest{Call: CallContext, …})`. Phase C: success → audit `casino.launched`; failure or ambiguity → `RevokeLaunchSession` (CAS `status='active'`, exists) plus audit `casino.launch_failed`. |
| Retryability | No automatic retry. A player retry mints a new session (existing behaviour). |
| Callbacks | Unchanged: bets resolve the session. A revoked or expired session gives `ErrLaunchSessionRequired`, and no money moves. |
| Failure recovery | Crash after A: the session is active, the token was never delivered, and it expires. Crash after the vendor accepted: the session is active and usable by the vendor, the player lost the URL, and it expires. Both are harmless: no ledger effect without a verified bet on a resolvable session. **Amendment 2026-09-28 (CAS-SESSION-EXPIRY-1, see §15.1.5):** "it expires" here describes only the un-consumed case (`expires_at` bounds an `active` session's own token-resolvability window). A session the vendor has already `consumed` is **not time-bounded**: `postBet` no longer applies the expiry check to it, and `RevokeLaunchSession` matches only `status='active'`, so a failed launch does not revoke it either (open: CAS-REVOKE-CONSUMED-1, §15.1.5). Only the status allow-list (`revoked`/`expired` rejected) applies. *(Wording corrected 2026-09-28 per code-reviewer re-review 2, C2.)* |
| Reconciliation | casino_consistency and casino_statement are unchanged. CAS-STMT-IO-1 (§20) is a hard precondition on the first real casino statement source: that adapter must land already split per §12.1, and its remediation is verified (not just referenced) when it is proposed, gated on `architect` and `ledger-finance` sign-off (casino review §26). |
| Webhook re-check | S-Q3 option (a) applies to casino callbacks too: a re-check DB or transport failure returns the typed `RecheckUnavailableError` → retryable 503; definitive results stay 401 (§6.6; PRH-I2). |
| Audit | `casino.launch_requested` (new), `casino.launched` (moved to phase C), `casino.launch_failed` (new). |
| Migration | None. |

#### 15.1.1 Implementation record (PRH-I2, casino part, 2026-09-27)

**Status: IMPLEMENTED (code + tests); not yet gate-reviewed.** `casino.LaunchGame`
(`internal/casino/orchestrator.go`) now owns its own transaction boundaries instead of
receiving one from its caller (`httpserver`'s launch handler no longer wraps the call in
`db.Pool.WithTenant`). The signature is
`LaunchGame(ctx, pool providercred.TenantTxRunner, outbound OutboundCredentialResolver, params LaunchGameParams)`
- `pool` and `outbound` are passed explicitly at each call site (not stored as
`Orchestrator` fields), to avoid changing `NewOrchestrator`'s constructor signature at
its ~150 existing call sites. `*db.Pool` satisfies `providercred.TenantTxRunner`
directly (the same interface `providercred.OutboundResolver.Resolve` already used, reused
rather than duplicated).

- **Phase A** (one `pool.WithTenant` transaction): every existing eligibility/capability
  check (game/availability/jurisdiction/blocklist/RG/risk/capability/`supports_bet`),
  `CreateLaunchSession`, and the new `casino.launch_requested` audit record, exactly as
  specified. A policy denial (RG/risk/jurisdiction) still commits its own audit row and
  returns a `LaunchGameResult{Denied: true, ...}` result value (never an error) via a
  closure-captured pointer, preserving the pre-split "denial is a result, not an error"
  contract byte for byte.
- **Health**, outside any transaction: called after phase A commits (not before, unlike
  the pre-split code, which checked health before minting the session) - `HealthStatus`
  takes no `tx` and is contractually no-I/O/prompt-return (§9.6), so this reordering
  costs nothing and lets the health check share phase C's revoke-on-failure path rather
  than needing its own no-session-yet branch. A circuit-open health snapshot revokes the
  just-minted session exactly like a Launch failure.
- **Phase B**, outside any transaction: resolves the outbound credential via
  `OutboundCredentialResolver.Resolve(ctx, pool, tenantID, providerID)` and calls
  `Launch`. `OutboundCredentialResolver` is a new casino-package interface (not the
  shared `payments`/`kyc` version §9.1 describes, since PRH-I1 has not landed and no
  shared type exists yet) with a method signature identical to
  `providercred.OutboundResolver.Resolve`, so the real
  `providercred.Subsystem.Outbound("casino")` satisfies it automatically; a
  `casino.MockOutboundResolver` (mirroring `NewMockWebhookCredentials`'s existing
  mock-vs-real split for inbound credentials) supplies the "MOCK: synthetic credential"
  case §9.1 names for the one MOCK adapter this stage ships. `providercred.
  NewMockOutboundCredential` was added (small, additive) to build that synthetic value
  without a handle-table read. `CallContext` is likewise casino's own copy of §9.1's
  shape (`TenantID`, `ProviderID`, `Credential`, `IdempotencyKey`, `Deadline`), with the
  same redacting `String`/`GoString`/`Format`/`LogValue`/`MarshalJSON` set §9.1 specifies;
  `LaunchRequest` gained a `Call CallContext` field. `IdempotencyKey` is `"cas:" +
  session.ID`, the deterministic external reference this section already named.
  Defense-in-depth binding check (S95-C8(b)): LaunchGame itself, not just the resolver,
  refuses a credential whose `TenantID`/`ProviderID`/`Domain` don't match the launch.
- **Phase C** (a second, separate transaction): success audits `casino.launched`; a
  transport failure, a non-`Succeeded` outcome, a circuit-open health snapshot, a
  nil/failing credential resolution, or a credential-binding mismatch all share one
  `launchFailed` path - `RevokeLaunchSession` (CAS on `status='active'`) plus audit
  `casino.launch_failed`. Every phase-C transaction (both the success audit and
  `launchFailed`) runs on `context.WithTimeout(context.WithoutCancel(ctx), phaseCTimeout)`
  (5s), never the caller's own request `ctx` - see §15.1.2 below (RV-PRH-I2 R1/C1: a
  cancelled request context, the ordinary trigger once a real adapter does I/O, must never
  silently skip the revoke and the audit). `errors.Is(err, ErrProviderUnavailable)` holds
  for the nil-pool, registry, circuit-open, nil-resolver, resolver-error and
  binding-mismatch branches; the transport-error and provider-decline branches wrap the
  adapter's own error/decline reason instead (no sentinel existed for either "before the
  split", since neither branch is new).
- **Wiring**: `httpserver.Deps` gained `CasinoOutboundCredentials
  casino.OutboundCredentialResolver`; `cmd/platform-api/registrations.go` gained
  `providerBundle.casinoOutboundCredentials()` (`casinoOrchestratorResolver`'s outbound
  twin). As of §15.1.2 (RV-PRH-I2 C3) it is keyed on the ADAPTER's own kind via the new
  `casino.OutboundKindSplitResolver` (mirroring `webhookauth.KindSplitResolver` exactly),
  never on "is any mock wired anywhere" - `b.Credentials.Outbound("casino")` serves every
  non-synthetic adapter, so a real adapter registered alongside the mock (e.g. in staging)
  reaches the real resolver, not the synthetic one.
- **Import-cycle note**: `internal/casino` now imports `internal/providercred` (for
  `TenantTxRunner`/`OutboundCredential`) - a new production import edge, verified acyclic
  (code review). This forced one existing in-package `providercred` test
  (`TestOutbound_NoCredentialOnLongLivedTypes`, which itself imports `casino`) to move to
  a new external test file (`internal/providercred/outbound_credential_bearing_test.go`,
  `package providercred_test`) - Go's standard mechanism for exactly this shape of
  test-only cycle; the move itself changes no production import.
- **Tests (original submission)**: all seven tests below were added in
  `internal/casino/launch_two_phase_integration_test.go` (not split across two files as
  an earlier draft of this record miscounted): no connection held across
  `HealthStatus`/`Launch` (a one-connection pool and a spy provider); nil-pool and
  nil-outbound-resolver fail closed without panicking; credential-binding mismatch fails
  closed; `CallContext` field shape pinned; provider-decline revokes the session; the two
  separate phase-A/phase-C audit records. One pre-existing test
  (`TestFailureModeMatrix_F_ProviderTransportFailureAtLaunchLeavesNoTrace`, renamed
  `..._RevokesSessionNoTrace`, in `failure_mode_matrix_integration_test.go`) had its own
  assertion corrected: the two-phase split's own behavior change is that a failed launch
  now leaves exactly one **revoked** session row, not zero, because phase A already
  committed the mint before phase B's failure - the intended tombstone behavior, not a
  regression. Original mutation result: 9/9 killed, 1 (M9, the transport-error branch)
  judged equivalent - the original evidence file's own header said "10/10", which was
  wrong (code review F1); corrected below. §16.2 item 18 ("Launch: crash after phase A,
  and crash after vendor accept") had no dedicated test in the original submission beyond
  the revoked-session-rejects-a-bet case - addressed in §15.1.2.

#### 15.1.2 Fix record (code/security review rework, 2026-09-27)

Both reviews (`docs/plans/payment-readiness/rv-prh-i2-casino-security.md`, APPROVE WITH
CONDITIONS C1-C4; `docs/plans/payment-readiness/rv-prh-i2-casino-code-review.md`, NOT
READY on R1) are addressed as follows.

- **R1/C1 (blocking) - phase C skipped on a cancelled request ctx.** Both reproduced the
  same defect: `launchFailed`, and the phase-C success transaction, ran on the caller's
  own `ctx`. A cancelled request context (a client disconnect mid-`Launch`, the realistic
  trigger once a real adapter performs I/O - not merely the ADR's accepted "process crash"
  window) makes `pool.WithTenant(ctx, ...)` fail to even begin, silently discarding both
  the revoke and the audit (the error was `_ = `'d). Fixed: both phase-C transactions now
  run on `context.WithTimeout(context.WithoutCancel(ctx), phaseCTimeout)`
  (`phaseCTimeout` = 5s); `RevokeLaunchSession` now returns `(bool, error)` (whether the
  CAS actually matched a row), recorded in the `launch_failed` audit metadata as
  `revoked`; a phase-C transaction failure is logged at error level via
  `Orchestrator.phaseCLogger()` (reuses `webhookLogger`, never silently discarded); the
  phase-C **success** path, if its own audit write fails, now calls `launchFailed` too
  (attempts a revoke rather than leaving a vendor-accepted, unaudited launch). Tests:
  `TestLaunchGame_CtxCancelledDuringLaunch_StillRevokesAndAudits` (cancels ctx inside a
  stub `Launch` returning a transport error; asserts revoked + audited + a subsequent bet
  rejected with the ledger exactly balanced before/after) and
  `TestLaunchGame_CtxCancelledAfterVendorAccept_StillAuditsLaunched` (cancels ctx right
  after a successful `Launch` returns; asserts `casino.launched` still commits and the
  session still accepts its bet) - together these are §16.2 item 18's "crash after
  phase A[/mid-Launch]" and "crash after vendor accept" cases, using ctx-cancellation as
  this codebase's practical proxy for a real crash (which cannot be simulated in-process).
  Both mutation-killed: removing `context.WithoutCancel` from either phase-C transaction
  fails the corresponding test.
- **Item 2 (casino/ledger-finance follow-up; security I3) - `postBet` never enforced
  expiry.** §15.1's original "an orphaned/never-resolved `active` session is harmless"
  claim depended on expiry bounding bet acceptance, but `postBet` rejected only
  `status='revoked'` - an `active` (or a lazily-flipped `expired`) session accepted a bet
  indefinitely. Fixed: `postBet` now allow-lists `status IN ('active','consumed')` (fails
  closed on `revoked`, `expired`, and any future status) **and** rejects
  `now() > expires_at` regardless of status - closing the exposure window to at most
  `DefaultLaunchTokenTTL` in every case, not merely "no legitimate vendor would call
  back". `postWin`/`postRollback` are unaffected (they resolve accounts via
  `correlation_id` from the ledger's own prior entries, never this session lookup), so a
  bet placed *before* expiry still settles after the session has since expired - this
  section's own doc comment ("harmless") is corrected accordingly. Tests:
  `TestReceiveCallback_NewBetRejectedOnExpiredStatus_LedgerBalanced`,
  `TestReceiveCallback_NewBetRejectedPastExpiresAtEvenIfStillActive` (a session whose
  status was never flipped away from `active`, past its own `expires_at`), and
  `TestReceiveCallback_WinSettlesForPreExpiryBetAfterSessionExpires` (win posts, ledger
  balanced, after the session has since been marked expired) - all in
  `internal/casino/postbet_session_expiry_integration_test.go`.
  **Amendment 2026-09-28 (CAS-SESSION-EXPIRY-1, regression, see §15.1.5): this "regardless
  of status" wording was wrong** - `expires_at` is the un-consumed token's own TTL
  (`DefaultLaunchTokenTTL`, 2 minutes), not an in-play bound, and applying it to `consumed`
  sessions too meant every real-money round stopped accepting bets ~2 minutes after
  launch. §15.1.5 restricts the `now() > expires_at` check back to `status='active'` only;
  the status allow-list itself is unchanged.
- **C2 - `CallContext` redaction untested.** `internal/casino/callcontext_redaction_test.go`
  (new) builds a `CallContext`/`LaunchRequest` around a known sentinel secret and asserts
  its absence from `%v`/`%+v`/`%#v`/`%s`/`%q`, `slog` text and JSON handler output,
  `json.Marshal`, and `fmt.Errorf("%v", ...)`, with a negative control (a plain,
  non-redacting type proving the sentinel is detectable at all). Confirmed to kill the
  reviewer's own mutation A (appending `string(c.Credential.Secret())` to
  `callContextRedacted`'s output).
- **C3 - outbound resolver chosen by "is any mock wired", not by adapter kind.** Fixed
  with `casino.OutboundKindSplitResolver` (`internal/casino/launch.go`), mirroring
  `webhookauth.KindSplitResolver` exactly, including its fail-closed-on-unregistered-id
  behavior. `cmd/platform-api`: a new `mockWiring.CasinoOutboundResolver` field and
  `providerBundle.CasinoOutboundResolver` component, registered with the synthetic guard
  (`providerkind.Registration{Domain:"casino", Name:"outbound_resolver", ...}`) exactly
  like the inbound MOCK resolvers. Tests:
  `internal/casino/outbound_kindsplit_test.go` (synthetic adapter → mock only; real
  adapter → real only; unregistered id fails closed; both nil yields a true nil interface;
  a synthetic adapter with no mock wired fails closed rather than falling back to real) and
  `cmd/platform-api/wiring_test.go`'s `TestCasinoOutboundResolver_FollowsWiring`. Fixing
  this also surfaced a latent nil-interface bug: `providercred.Subsystem.Outbound` returns
  the CONCRETE `*OutboundResolver` type (unlike `.Resolver`, which already returns the
  `webhookauth.Resolver` interface), so passing a nil `*OutboundResolver` straight into
  `OutboundKindSplitResolver`'s interface parameter produced a non-nil interface wrapping
  a nil pointer - `casinoOutboundCredentials()` now converts it to a true nil interface
  explicitly before passing it in.
- **C4 - revoke CAS misses a vendor-consumed session.** Not changed structurally: item 2's
  fix (expiry enforced at bet-placement time, independent of status) is judged sufficient
  - a session the revoke's CAS missed because the vendor had already moved it to
  `consumed` is now bounded by the same `expires_at` check regardless, closing the
  exposure to at most `DefaultLaunchTokenTTL` rather than requiring the CAS itself to
  widen. Recorded here per C4's own "or record the decision" option, rather than widening
  `RevokeLaunchSession`'s `WHERE` clause.
  **Amendment 2026-09-28 (CAS-SESSION-EXPIRY-1, see §15.1.5): this closure was based on the
  now-corrected, over-broad item-2 expiry check and is WRONG as written** - now that
  `expires_at` no longer bounds a `consumed` session's bet eligibility (§15.1.5), C4 is
  reopened: a session the launch-failure path revoked-CAS missed because the vendor had
  already consumed it remains bet-eligible indefinitely. §15.1.5 attempted the "properly
  close C4" option this note declined (widen `RevokeLaunchSession`'s CAS to
  `status IN ('active','consumed')`) and found it blocked at the database level: migration
  0036/0042's `casino_launch_sessions_enforce_immutable_fields` trigger forbids ANY
  transition out of a terminal status, and `consumed` is terminal - `UPDATE ... SET
  status='revoked' WHERE status='consumed'` raises `casino_launch_sessions: row is
  immutable once consumed, expired, or revoked` and aborts the transaction. Closing C4
  properly therefore requires either a migration (relaxing the trigger for the specific
  `consumed -> revoked` transition) or a different mechanism entirely, and is deferred to
  the orchestrator as **CAS-REVOKE-CONSUMED-1** (tracked in
  `docs/governance/task-registry.md`) rather than solved by re-widening the bet-time expiry
  check, which regressed real-money play instead.
- **Security informational I1/I2/I3** (jurisdiction denials write no audit; a
  `HealthStatus` error fails open; a bet racing a concurrent revoke can still commit) are
  pre-existing (not introduced by this diff) and are registered as their own tracking rows
  in `docs/governance/task-registry.md` (`CAS-JURIS-AUDIT-1`, `CAS-HEALTH-FAILOPEN-1`,
  `CAS-REVOKE-BET-RACE-1`), owner `casino`, rather than fixed silently inside this record.
- **Item 5 (orchestrator follow-up) - raw `cause.Error()` in append-only audit.**
  `casino.launch_failed`'s audit metadata stored `cause.Error()` verbatim - a real
  adapter's transport error (`*url.Error`) can embed the full request URL, including a
  query-string credential, which append-only `audit_log.metadata` must never receive.
  Fixed with a closed `LaunchFailureReason` enum (`provider_unavailable`, `declined`,
  `circuit_open`, `credential_unavailable`, `credential_binding_mismatch`,
  `ctx_cancelled`, `internal`) stored in place of the raw text; the cause itself reaches
  an operator only through `phaseCLogger`'s log line, via the new
  `redactedLaunchFailureDetail` (mirrors `internal/payments/gate.go`'s `redactedReason`
  without importing it). Grepped the whole package for other `.Error()` writes into audit
  metadata - this was the only one. Test:
  `TestLaunchGame_TransportErrorAuditNeverStoresRawErrorText` builds a `*url.Error`
  carrying a secret-looking query string, calls `LaunchGame`, and reads the actual
  persisted `audit_log.metadata` row back via SQL to assert the secret's absence and the
  bounded reason's presence.
- **Mutation evidence, corrected**: `docs/plans/payment-readiness/evidence/
  prh-i2-casino-mutation-kill.txt` now reads **9/9 killed, 1 equivalent** for the original
  submission (correcting F1's "10/10" miscount) plus a second section for this fix
  record's own mutants (ctx-cancellation removed from either phase-C transaction; the
  `postBet` status allow-list removed; the `postBet` expiry check removed; the
  resolver-error `ErrProviderUnavailable` wrap removed) and a third section for item 5
  (the audit `"reason"` reverted to `cause.Error()`) - **15/15 killed, 1 equivalent**
  overall.
- **Not done here** (explicitly out of this task's scope): the payments (PRH-I1) and KYC
  (identity-compliance's own PRH-I2 slice) implementations; F-POOL-2's kill-switch/
  capability-manifest payment-only scope (§10); CAS-STMT-IO-1's remediation (still Low,
  still not reachable - the MOCK statement source is unchanged); widening
  `RevokeLaunchSession`'s CAS to include `consumed` (C4, judged unnecessary given item 2).

#### 15.1.3 Fix record (IO-1B/IO-1C, architect review, 2026-09-27)

Architect review (`rv-prh-architect.md`) found `LaunchGame`'s phase B had no in-gate
`txscope.Held(ctx)` refusal at all (only the real `providercred.OutboundResolver`'s own
refusal, indirectly), and `MockOutboundResolver.Resolve` (`mock.go`) ignored `ctx` entirely -
contradicting this ADR's own §3.2/§11 claims. **Fixed**: `LaunchGame`'s phase B now refuses
with `ErrProviderCallRefused` (a new sentinel, `types.go`, mirroring `payments.
ErrProviderCallRefused` exactly) immediately before `provider.Launch`, under a new
`LaunchFailureTxHeld` reason; `MockOutboundResolver.Resolve` now refuses under
`txscope.Held(ctx)` too, exactly like the real resolver. Tests (`internal/casino/
launch_two_phase_integration_test.go`): `TestLaunchGame_RefusesUnderTxscopeHeld` (calls
`LaunchGame` from inside a `pool.WithTenant` closure using a resolver that does NOT itself
refuse, isolating the call-site guard; asserts `ErrProviderCallRefused` and that the
adapter's own `Launch` method is called zero times) and
`TestMockOutboundResolver_RefusesUnderTxscopeHeld` (the resolver's own refusal, directly).
Both mutation-killed. INV-IO-1(c)'s own static scan (previously NOT IMPLEMENTED, contrary to
this ADR's own reference to it) is now `internal/txscope`'s
`TestINV_IO_1c_NoAdapterCallInsideTxClosure` - see §15.3.5 for the full record, since it
covers casino, KYC and payments together in one guard.

#### 15.1.5 Fix record (CAS-SESSION-EXPIRY-1, code-reviewer FH-7 re-review, 2026-09-28)

**Regression, HIGH.** §15.1.2 item 2's fix applied `now() > expires_at` to a bet against
*either* `active` or `consumed`. `expires_at` is the un-consumed launch **token's** own TTL
(`DefaultLaunchTokenTTL = 2 * time.Minute`, `launch.go`) - set once at mint, immutable
(migration 0036/0042) - never an in-play bound. Since `consumed` is the ordinary in-play
state for the entire lifetime of a real-money round, every such round stopped accepting
bets roughly `DefaultLaunchTokenTTL` after launch, with `ErrLaunchSessionRequired`
("session has expired") logged at Error - a production-breaking regression from
`2c00e10` ("postBet expiry").

- **Fix (required item 1).** `internal/casino/orchestrator.go` `postBet`: the
  `now() > expires_at` check now applies only when `session.Status == LaunchSessionActive`.
  A `consumed` session keeps accepting bets while otherwise eligible (status allow-list,
  `ModeReal`, asset match - all unchanged from §15.1.2). The status allow-list itself
  (`active`/`consumed` only, fails closed on everything else) is unchanged.
- **C4, required item 2 (properly close, rather than re-rely on expiry).** Investigated
  widening `RevokeLaunchSession`'s CAS (`launch.go`) from `WHERE status='active'` to
  `WHERE status IN ('active','consumed')`, so a launch that phase C records as failed
  revokes the session even if the vendor had already consumed the token before the failure
  was recorded (the scenario C4 names). **Blocked at the database level, not implemented**:
  migration 0036's `casino_launch_sessions_enforce_immutable_fields` trigger (reaffirmed
  unchanged by migration 0042) raises an exception for *any* `UPDATE` where
  `OLD.status IN ('consumed', 'expired', 'revoked')`, regardless of `NEW.status` - i.e. it
  forbids every transition OUT OF a terminal status, not merely a revival back to
  `'active'`. A `consumed -> revoked` `UPDATE` therefore aborts the whole phase-C
  transaction (`casino_launch_sessions: row is immutable once consumed, expired, or
  revoked`), which would silently turn every `launchFailed` call against an
  already-consumed session into an unaudited phase-C failure (worse than today, and exactly
  the F3 gap the phase-C error-logging was added to catch). Per this project's standing
  rule that a new migration is allocated by the orchestrator, not authored unilaterally
  by a specialist fixing an unrelated regression, this half of the fix is **NOT
  IMPLEMENTED** and is tracked as **CAS-REVOKE-CONSUMED-1** (owner `casino`,
  `docs/governance/task-registry.md`) pending a decision on whether to relax the trigger
  for this one transition (migration) or use a different mechanism. `RevokeLaunchSession`'s
  `WHERE` clause is unchanged (`status='active'` only); the CAS-vs-consumed exposure C4
  originally named is real again now that item 2's over-broad expiry check no longer masks
  it, bounded only by however long a vendor keeps a genuinely-failed launch's session
  `consumed` and betting against it (unbounded in the worst case - the reason this is
  tracked at HIGH, not closed).
- **Callers/allowed-transitions check.** `RevokeLaunchSession` has exactly one production
  caller (`orchestrator.go`'s `launchFailed`, phase C); no other caller exists that this fix
  needed to account for.
- **N2 (Low, doc-comment misplacement).**
  `internal/casino/launch_two_phase_integration_test.go`: the
  `TestLaunchGame_CircuitOpenRevokesAndAudits` doc comment was sitting directly above
  `TestLaunchGame_TransportErrorAuditNeverStoresRawErrorText`. Moved to sit above its own
  function.
- **Tests** (`internal/casino/postbet_session_expiry_integration_test.go`):
  `TestReceiveCallback_ConsumedSessionAcceptsBetAfterTokenTTLExpires` (mints a session with
  a short TTL, consumes it via `ResolveLaunchToken` before the TTL lapses, waits past the
  TTL, then posts a bet and asserts it succeeds and the ledger balances) - this is the
  regression repro (kills a reintroduced "apply expiry to consumed too" mutant).
  `TestReceiveCallback_NewBetRejectedPastExpiresAtEvenIfStillActive` (pre-existing, §15.1.2)
  is unchanged and still passes - an `active` (never-consumed) session past `expires_at` is
  still refused, killing the "drop the expiry check entirely" mutant.
  `TestLaunchGame_FailedLaunchOnConsumedSession_RevokeCASMissesAndBetStillAccepted`
  (`internal/casino/launch_two_phase_integration_test.go`) documents the now-reopened C4 gap
  as a pinned, explicitly-labelled `NOT IMPLEMENTED` characterization test (not a silent
  regression): a session is consumed, phase C then records the launch as failed,
  `RevokeLaunchSession`'s CAS misses (session stays `consumed`, `revoked=false` in the audit
  metadata), and a subsequent bet against it is accepted - asserting the exposure exists so
  a future migration-backed fix has a red test to turn green, rather than this gap being
  rediscovered from a production incident.
- **Mutation evidence**: appended to `docs/plans/payment-readiness/evidence/
  prh-i2-casino-mutation-kill.txt`, dated section "CAS-SESSION-EXPIRY-1 (2026-09-28)".

**Security re-review of this fix (2026-09-28, `rv-prh-i2-casino-security.md` FH-7):** `80eda28` ACCEPTED (equal to or stricter than the pre-`2c00e10` behaviour for every status). Two qualifications:
- **I-4:** no production code consumes a launch token (`ResolveLaunchToken` has no non-test caller). So in the current wiring every `LaunchGame` session stays `active`, and bets are still refused about 2 minutes after launch (CAS-PLAY-BOOTSTRAP-1). This is correct from a security standpoint and must not be relaxed; the remedy is a vendor token-bootstrap path.
- **C4 / CAS-REVOKE-CONSUMED-1:** MEDIUM, **deferred and launch-blocking** (option b), with the required migration-backed fix (a) specified in the security record. It must land before the first of: a non-test caller of `ResolveLaunchToken`; a non-synthetic casino adapter; or a production launch request.

#### 15.1.5 amendment (CAS-REVOKE-CONSUMED-1, 2026-09-28)

PRH-2 workstream A (`docs/plans/prh2-hardening-round/plan.md` §5-A; security's required fix,
`docs/plans/payment-readiness/rv-prh-i2-casino-security.md` "Re-review (FH-7, 2026-09-28)",
"Required fix (a)"). **Status: IMPLEMENTED; security ACCEPT
(`docs/plans/prh2-hardening-round/reviews/a-security.md`); CLOSED on merge.** (Per CLAUDE.md's "no
fake completion" rule, this is not marked closed here - the registry and the merge itself are the
orchestrator's own gate, not this ADR's.)

- **Migration 0108** replaces `casino_launch_sessions_enforce_immutable_fields()`, keeping 0042's
  column-immutability block byte-identical and replacing only the terminal-status block: exactly
  `OLD.status = 'consumed' AND NEW.status = 'revoked'` is now permitted, and only when
  `(to_jsonb(NEW) - 'status') = (to_jsonb(OLD) - 'status')` (every other column, including any
  future one, stays frozen). Every other transition out of `consumed`, `expired` or `revoked` -
  including `consumed -> active`, `consumed -> expired`, a `consumed -> consumed` no-op, a
  `consumed -> revoked` combined with any other column change, and every transition at all out of
  `expired` or `revoked` - still raises exactly as before. The down migration restores 0042's body
  verbatim.
- `RevokeLaunchSession` (`internal/casino/launch.go`) now does `SELECT status ... FOR UPDATE`, then
  `UPDATE ... SET status='revoked' WHERE id=$1 AND status IN ('active','consumed')`, and returns the
  prior status alongside whether the CAS matched. `orchestrator.go`'s `launchFailed` (phase C) now
  writes `prior_status` into the append-only `casino.launch_failed` audit record, next to the
  existing `revoked` field. **Behaviour change (code review A6):** before this fix, a session id that
  resolved to no row returned `(false, nil)`; `RevokeLaunchSession` now returns `ErrLaunchSessionNotFound`
  in that case (also the observed outcome of a cross-tenant revoke attempt, since RLS hides the row
  entirely). `launchFailed` has exactly one caller with a session id it just minted in the same
  `LaunchGame` invocation, so this path is not reachable in production today; it is exercised
  directly by `TestRevokeLaunchSession_TenantIsolation`.
- Token replay is unaffected exactly as anticipated: `ResolveLaunchToken` still accepts only
  `active`; the relaxed trigger branch only ever admits the terminal `consumed -> revoked` move;
  `token_hash`/`expires_at` remain immutable throughout.
- The former characterization test (`TestLaunchGame_FailedLaunchOnConsumedSession_
  RevokeCASMissesAndBetStillAccepted`) is inverted and renamed
  `TestLaunchGame_FailedLaunchOnConsumedSession_RevokesAndRejectsBet`
  (`internal/casino/launch_two_phase_integration_test.go`): the session now ends `revoked`, the
  audit shows `revoked=true` and `prior_status="consumed"`, the follow-up bet is refused with
  `ErrLaunchSessionRequired`, and the player's balance/ledger are unaffected by that refused bet.
- Additional coverage (`internal/casino/migration_0108_revoke_consumed_integration_test.go`): the
  full DB trigger matrix (one transaction per statement); token replay after
  `consumed -> revoked` returning `ErrLaunchSessionNotActive`; a bet placed before the revoke still
  settling via both a win and a rollback, ledger balanced throughout; tenant isolation on the
  revoke itself (tenant B's attempt against tenant A's consumed session resolves 0 rows,
  `ErrLaunchSessionNotFound` under RLS); and migration 0108 up/down/up on a scratch database.
- Mutation evidence: `docs/plans/payment-readiness/evidence/prh2-casino-a-mutation-kill.txt`
  (author's own round, 4/4 killed: drop the whole-row equality, allow
  `OLD.status IN ('consumed','expired')`, drop `NEW.status='revoked'`, restore the revoke's own
  `WHERE status='active'`; code review's follow-up round added the `id`-change matrix cell and the
  already-`expired`/already-`revoked` no-op coverage - see the same evidence file for that round).
- Security review: **ACCEPT** (`docs/plans/prh2-hardening-round/reviews/a-security.md`). Code review:
  **READY WITH CONDITIONS**, A1-A8 addressed in follow-up commits on this branch
  (`docs/plans/prh2-hardening-round/reviews/a-code-review.md`).
- CAS-REVOKE-CONSUMED-1 (registry): the orchestrator updates `docs/governance/task-registry.md` at
  merge, per CLAUDE.md's "no fake completion" rule - not asserted here.

### 15.2 KYC `CreateVerification`

| Aspect | Specification |
|---|---|
| Intent | The `kyc_verifications` row inserted in phase A with `status='unverified'`, `provider_id` set and `provider_reference NULL`, plus audit `kyc.verification_requested`. |
| Reference / idempotency key | `CreateVerificationInput` gains `Call CallContext` and `ExternalReference = verification.id`. The vendor idempotency key is `"kv:" + id` **only if the vendor supports keys, which is PROVIDER DEPENDENT and recorded at intake**. Where it does not, a player retry before the orphan is known creates a second, unrelated vendor-side verification under a second platform row. That is not a financial or enforcement hazard (each row is evaluated on its own merits), but it must never be assumed deduplicated. |
| Flow | A (commit) → B (`CreateVerification`) → C: CAS `UPDATE … SET provider_reference=$r, status=$s WHERE id=$1 AND provider_reference IS NULL AND status='unverified'` plus audit `kyc.verification_submitted` (existing action, moved). |
| Retryability | No automatic retry. A player retry creates a new row (existing behaviour). An orphan `unverified` row without a reference is **excluded from ADR 0096's "latest row" enforcement selection outright** (§15.3.3 below, RV-PRH-I2 KYC code review F1): it is never even the row consulted, decided or not. This is a correction of this row's original wording ("no enforcement effect (it is not verified)"), which was true only when the orphan happened to be older than every decided row — a NEWER orphan committed after an approval would otherwise become the "latest" row and evaluate `failed`, turning a mere vendor outage into a withdrawal denial for an already-approved player. |
| Callbacks | KYC has **no receipt table**, so a callback that races phase C (reference unknown) returns a **retryable 5xx**, never a 200: a 200 would discard the only copy of the evidence (IC-Q1). A 200 is reserved for a callback the platform actually applied, including a duplicate or no-op apply. Whether the vendor redelivers on 5xx is **PROVIDER DEPENDENT** and is confirmed at real-vendor intake, the same discipline as §6.6/LF-C1. The merchant-reference echo is PROVIDER DEPENDENT. S-Q3 option (a) applies (re-check DB failure → 503). |
| Failure recovery | Crash after A: an orphan row, harmless. After B: an orphan vendor verification. KYC enforcement (ADR 0096) reads only platform rows, so it is harmless. |
| Migration | None. |

### 15.3 KYC `SubmitVerification` (document upload)

| Aspect | Specification |
|---|---|
| Flow | A: insert document plus audit → commit. A short read-only tx gathers the current non-rejected document set. B: `SubmitVerification(Call, providerReference, docs)` with the idempotency key `"ks:" + verification_id + ":" + sha256(sorted document ids)`. C: the existing `normalizeProviderResult` plus **`applyForwardOnlyStatus`'s forward-only CAS** (§15.3.3 below, RV-PRH-I2 KYC code review/security review R1/C1) plus audit (existing) - **not** a blind, non-CAS `updateVerificationStatus` as an earlier revision of this row claimed: the whole provider round-trip in phase B (up to `defaultProviderCallTimeout`) is wide enough for a staff `ReviewVerification` or a verified callback's own forward-only transition to commit before phase C runs, and only a CAS starting from phase A's own status read - re-evaluated against the row's CURRENT status on a lost race, exactly like the callback path's own rule - stops phase C from silently overwriting (or demoting) that concurrent decision. |
| Ambiguous result (IC condition 2) | **An ambiguous, timeout or transport-error `SubmitVerification` result leaves `kyc_verifications.status` unchanged.** Phase C maps it to the existing `ProviderError` branch (audit with outcome failure, no status update); it is never passed to `statusForOutcome` and never read by `normalizeProviderResult` as a definitive outcome. §15.3 does not reuse §4.4's matrix, so this rule has its own test (§16.2 item 18). |
| Failure recovery | Identical to today's `ProviderError` outcome: the verification stays as it is, and the next upload re-submits the full set. A durable KYC submission outbox is **deferred** as KYC-SUBMIT-OUTBOX-1, and it is a **hard precondition on the first real KYC adapter**: no real KYC adapter is accepted into PRH-I2 (or later) without a durable submission outbox design landing first (IC condition 5). |
| Migration | None. |

#### 15.3.1 Implementation record (PRH-I2, KYC part, `identity-compliance`, 2026-09-27)

**Status: IMPLEMENTED (code + tests); not yet gate-reviewed.** Both `kyc.CreateVerification`
(`internal/kyc/verification_service.go`) and the document-submission step, now split out as
its own exported `kyc.SubmitVerification` (`internal/kyc/document_service.go`), own their own
transaction boundaries instead of running inside a caller-supplied `tx` across the provider
call. Both take `pool providercred.TenantTxRunner` and `outbound OutboundCredentialResolver`
explicitly (mirroring `casino.LaunchGame`'s identical parameter shape, §15.1.1) rather than
storing them on `Orchestrator` - `CreateVerification`/`SubmitVerification` are package-level
functions, not `Orchestrator` methods, and changing that shape was out of this task's scope.
`*db.Pool` satisfies `providercred.TenantTxRunner` directly, the same interface
`providercred.OutboundResolver.Resolve` and casino's identical parameter already use.

- **`internal/kyc/callcontext.go`** (new) is KYC's own copy of §9.1's `CallContext` shape and
  the `OutboundCredentialResolver`/`MockOutboundResolver`/`OutboundKindSplitResolver` trio -
  structurally identical to `casino`'s copies (§15.1.1's own note that payments/casino/kyc each
  get their own until PRH-I1 lands the shared version applies here too), including the same
  redacting `String`/`GoString`/`Format`/`LogValue`/`MarshalJSON` set and the same
  fail-closed-on-unregistered-adapter-id `OutboundKindSplitResolver` behavior.
  `CreateVerificationInput` gained a `Call CallContext` field (an input-struct field, not a
  parameter, so the field-by-field literal at the `provider.CreateVerification` call site
  already guards against a stray future field leaking); `KYCProvider.SubmitVerification` gained
  a trailing `call CallContext` parameter (its own argument, since that method has no input
  struct to extend).
- **`CreateVerification` phase A** (one `pool.WithTenant` transaction, `insertOrphanVerification`):
  inserts the orphan row (`status='unverified'`, `provider_id` set, `provider_reference NULL`)
  plus the `kyc.verification_requested` audit record, then commits. **Phase B**, outside any
  transaction: resolves the outbound credential via `outbound.Resolve(ctx, pool, tenantID,
  providerID)`, checks the defense-in-depth tenant/provider/domain binding (mirrors casino's
  identical check, S95-C8(b)), then calls `provider.CreateVerification` with
  `IdempotencyKey: "kv:" + verification.ID` - the deterministic external reference §15.2 names.
  A phase-B failure (no resolver, credential-resolution failure, binding mismatch, or the
  provider call itself failing) returns `ErrProviderUnavailable` and leaves the phase-A orphan
  row exactly as committed - never rolled back, never retried automatically, matching §15.2's
  own "Failure recovery" row (no audit row is written for this specific failure; only phase A's
  and phase C's audit records ever exist). **Phase C** (`applyCreateVerificationResult`, a
  second, separate, short transaction on `context.WithTimeout(context.WithoutCancel(ctx),
  phaseCTimeout)` - 5s, mirroring casino's identical ctx-independence for the same reason,
  RV-PRH-I2 C1): a CAS `UPDATE ... WHERE id=$1 AND tenant_id=$2 AND provider_reference IS NULL
  AND status='unverified'` applies the normalized result, plus the `kyc.verification_submitted`
  audit record (the existing action name, moved here from the old single-transaction
  implementation).
- **`SubmitVerification`** (new exported function, replacing the former unexported,
  tx-held-across-the-call `submitVerificationDocuments`): phase A
  (`gatherSubmissionDocuments`, one short read-only transaction) reads the verification
  (terminal-guarded - an already-terminal verification, or one whose row does not resolve
  under the caller's own tenant scope, is a documented no-op/`ErrNotFound` respectively, no
  provider call attempted) and its current non-rejected document set. Phase B, outside any
  transaction, calls `provider.SubmitVerification` with
  `IdempotencyKey: "ks:" + verification_id + ":" + sha256(sorted document ids)` (§15.3's own
  content-derived key - `submissionIdempotencyKey`). **IC condition 2** is enforced in two
  places: a transport-level failure from the call itself returns `ErrProviderUnavailable`
  without ever reaching phase C (no `ProviderResult` exists to apply), and a well-formed
  `ProviderResult{Outcome: ProviderError}` reaching phase C (`applySubmissionResult`) returns
  early after its own audit row, before ever calling `statusForOutcome` - in both shapes
  `kyc_verifications.status` is provably left untouched (own tests, §15.3.2 below). Phase C
  (a second, separate, ctx-independent, bounded transaction, identical to `CreateVerification`'s)
  applies a definitive outcome via `applyForwardOnlyStatus`'s forward-only CAS (§15.3.3's R1 fix
  - the original submission's own doc comment claimed this call was already "the existing
  terminal-guarded `updateVerificationStatus`", which was FALSE: `updateVerificationStatus` was
  a blind, unconditional `UPDATE` with no status predicate at all, and its only "terminal-guard"
  existed in phase A's stale read) plus the existing `kyc.verification_submitted_to_provider`
  audit record. `UploadDocument` itself
  is now phase-A-only (document insert + audit; no provider call, no `provider` parameter) -
  the caller (`internal/httpserver`'s upload handler) calls `UploadDocument` (inside its own
  short transaction) and then `SubmitVerification` (pool-based) afterward, exactly mirroring
  how `casino.LaunchGame` composes `CreateLaunchSession` and `Launch`.
- **Callback race (IC-Q1, §15.2 "Callbacks")**: `ReceiveVerifiedCallback`'s step (d) lookup by
  `provider_reference` now distinguishes a genuinely-unknown reference from every other
  "not found" case in this package via a new sentinel, `ErrVerificationReferenceUnknown` -
  wrapping (not replacing) the underlying `ErrNotFound` lookup failure, but never satisfying
  `errors.Is(err, ErrNotFound)` itself (own test asserts both directions). KYC has no receipt
  table, so this case is structurally indistinguishable from a callback racing
  `CreateVerification`'s own phase C - both map to the identical retryable disposition.
  `internal/httpserver/kyc_admin_handlers.go`'s webhook dispatch maps
  `ErrVerificationReferenceUnknown` to a `503` (`apierror.CodeUnavailable`), checked BEFORE the
  pre-existing `kyc.ErrNotFound` branch (which now only ever fires for a different not-found
  case reaching that dispatch); redelivery-on-5xx is `PROVIDER DEPENDENT`, confirmed at
  real-vendor intake per the same discipline §6.6/LF-C1 already applies elsewhere.
- **Wiring**: `httpserver.Deps` gained `KYCOutboundCredentials kyc.OutboundCredentialResolver`
  (`CasinoOutboundCredentials`'s KYC twin); `cmd/platform-api/registrations.go` gained
  `providerBundle.KYCOutboundResolver`/`kycOutboundCredentials()` (mirroring
  `CasinoOutboundResolver`/`casinoOutboundCredentials()` exactly, including the kind-split
  discipline and the nil-concrete-pointer-to-true-nil-interface conversion); `mockWiring`
  gained `KYCOutboundResolver bool`, derived from the same `testSupport` value as every other
  MOCK flag. `newCreateMyVerificationHandler`/`newUploadMyDocumentHandler` were restructured to
  resolve identity/provider-selection in a short read-only transaction and then call
  `CreateVerification`/`UploadDocument`+`SubmitVerification` against `deps.DB` directly (no
  longer inside the identity-resolution transaction), mirroring `casino_handlers.go`'s
  identical `LaunchGame` wiring pattern.
- **CreateVerification vendor idempotency is `PROVIDER DEPENDENT`** (§15.2's own table row,
  reiterated here per identity-compliance review condition "CreateVerification vendor
  idempotency `PROVIDER DEPENDENT`"): the `"kv:" + id` key this implementation passes via
  `CallContext.IdempotencyKey` is honored only if a real vendor supports idempotency keys at
  all; this must be confirmed and recorded at real-vendor intake, never silently assumed.
- **Not done here** (explicitly out of this task's scope, per its own instructions): any change
  to `internal/kyc/enforcement*.go` or to migrations 0100/0103 (another agent's concurrent
  work); the payments (PRH-I1) implementation; any schema migration (none was needed or added -
  every phase reuses `kyc_verifications`' existing nullable `provider_reference` column and
  existing `status` enum).

#### 15.3.2 Tests and mutation evidence

`internal/kyc/kyc_two_phase_integration_test.go` (new): no connection held across the provider
call for either `CreateVerification` or `SubmitVerification` (a one-connection pool and a spy
provider, mirroring casino's identical proof technique); nil-pool/nil-provider/nil-outbound-
resolver fail closed without panicking (the nil-outbound-resolver case additionally asserts the
phase-A orphan row is left exactly as committed, with no `kyc.verification_submitted` audit
row); credential-binding mismatch fails closed; `CallContext` field shape pinned
(`TenantID`/`ProviderID`/`Credential.Domain`/`IdempotencyKey`/`Deadline`); **IC condition 2's own
required test**, in both its transport-error and definitive-`ProviderError`-outcome shapes,
proving `kyc_verifications.status` is left completely unchanged in either case; a terminal
verification is a documented no-op (no provider call attempted); the content-derived
idempotency key is stable across repeated calls with an unchanged document set; cross-tenant
isolation (a verification created for tenant A is never visible under tenant B's scope, and
`SubmitVerification` called with the wrong tenant id fails closed); a ctx-cancellation proxy for
"crash between phase B and phase C" (this codebase's practical substitute for a real crash,
identical convention to casino's own §15.1.1/§15.1.2 tests) for both `CreateVerification` and
`SubmitVerification`, proving phase C still applies the result and writes its audit record
despite the cancelled request context; and `CallContext`'s own redaction test (mirrors
`internal/casino/callcontext_redaction_test.go` exactly, including its negative control).
`internal/kyc/orchestrator_webhook_integration_test.go`'s pre-existing
`TestKYCWebhook_VerifiedUnknownReference_NotFound` and `internal/httpserver/kyc_flow_integration_test.go`'s
`TestKYC_WebhookCallbackAuthentication` were both updated in place to assert the new retryable
`ErrVerificationReferenceUnknown`/503 disposition instead of the old terminal
`ErrNotFound`/404 - a deliberate, disclosed behavior change (IC-Q1), not a regression.

Mutation evidence: `docs/plans/payment-readiness/evidence/prh-i2-kyc-mutation-kill.txt` -
**11/11 killed, 0 equivalent**, covering both nil-guard panics, the credential-binding check,
the idempotency-key shape, IC condition 2's two distinct code paths, the terminal-verification
no-op guard, the `ErrVerificationReferenceUnknown` typing at both the package and HTTP-handler
layers, and the `CallContext` redaction renderer.

**Note (fix round below): the "11/11" count and its "the idempotency-key shape" and
"the credential-binding check" claims describe the ORIGINAL submission's own evidence file as
of this writing, and code review RV-PRH-I2 KYC (F3) found that count overstated coverage of
several of these exact branches - see §15.3.3 and the corrected, superseding evidence file.**

#### 15.3.3 Fix record (code review + security review rework, 2026-09-27)

Both reviews (`docs/plans/payment-readiness/rv-prh-i2-kyc-security.md`, APPROVE WITH
CONDITIONS C1-C5; `docs/plans/payment-readiness/rv-prh-i2-kyc-code-review.md`, NOT READY on
R1) are addressed as follows.

- **R1/C1 (BLOCKING) - `SubmitVerification` phase C was neither CAS nor terminal-guarded.**
  Both reviews reproduced the identical defect: `applySubmissionResult`'s status write called
  `updateVerificationStatus`, a blind `UPDATE ... WHERE id = $3` with **no status predicate at
  all** - the only terminal check ran in phase A (`gatherSubmissionDocuments`), in a
  transaction that had already committed before the whole provider round-trip (up to
  `defaultProviderCallTimeout`, 10s) even began. A staff `ReviewVerification` (reject or
  approve) or a verified callback's own forward-only CAS transition, committed anywhere in that
  window, was silently overwritten - including backward, e.g. `review_required` demoting an
  `approved` a callback had just committed. Security review's own reproduction: a compliance
  officer's `rejected` decision, committed mid-submission, was overwritten by the provider's
  later `approved` result, and ADR 0096 enforcement then read `passed` for a player a human had
  just rejected. **Fixed**: `updateVerificationStatus` (a blind, non-CAS writer with no other
  caller) is removed outright and replaced by a new shared function,
  `applyForwardOnlyStatus` (`internal/kyc/provider.go`), reusing `applyCallbackOutcome`'s own
  forward-only rank rule (unverified(0) < pending(1) < review_required(2) < terminal(3)): the
  CAS starts from phase A's own status read and, on a lost race, re-reads the row and
  re-evaluates (up to 3 attempts, identical to the callback path's loop) - a result whose rank
  is at or below the row's CURRENT rank is a documented no-op, never applied, never moving the
  row backward. `applySubmissionResult`'s own `kyc.verification_submitted_to_provider` audit
  row is still written unconditionally (the call genuinely happened), now carrying a
  `status_applied` boolean so a superseded submission is distinguishable from one that actually
  changed the row. Tests: `TestSubmitVerification_StaffRejectDuringProviderCall_Survives` (P1 -
  a staff reject committed inside the provider-call hook survives a later provider approval,
  including `reviewed_by`) and `TestSubmitVerification_CallbackApprovalDuringProviderCall_
  NotDemoted` (P2 - a callback's approval, committed inside the same hook, survives a later,
  lower-rank submission result) - both in `internal/kyc/kyc_two_phase_integration_test.go`,
  reproducing the reviews' exact P1/P2 scenarios and proving the fix. §15.2/§15.3's own table
  rows and §15.3.1's prose are corrected above to describe `applyForwardOnlyStatus`, never the
  removed `updateVerificationStatus`.
- **F1 - "the orphan has no enforcement effect" was false in the deny direction.** ADR 0096's
  `readLatestVerificationByPlayerAccount` reads the account's `ORDER BY created_at DESC, id
  DESC LIMIT 1` row with no other filter, so a `CreateVerification` phase-B failure's own
  orphan row (harmless by §15.2's own claim) becomes the enforcement-visible "latest" row the
  moment it is newer than an existing approval - code review's own reproduction: an approved
  player starting a routine re-verification while the vendor happens to be unreachable is
  denied withdrawals until a LATER retry succeeds, purely because of a transient vendor outage
  that, pre-split, would have rolled back and left the approval as the latest row. **Fixed**
  (identity-compliance is this ADR's own owner and ADR 0096's; authorized to change enforcement
  read semantics for this specific case, coordinated with security's N1 text): ADR 0096 §2.6
  gains a new point (g) - see ADR 0096 §19 for the full text and its own implementation record
  - excluding a row with `status='unverified' AND provider_reference IS NULL` (a row that never
  received ANY decision) from BOTH `readLatestVerificationByPlayerAccount`'s own "latest row"
  selection AND `crossAccountRejectedOverlay`'s per-other-account "latest row" subquery, so a
  never-decided orphan can never supersede an already-decided approval, nor mask an
  already-decided rejection on another account, purely by being newer. An account whose ONLY
  rows are such orphans is treated exactly as if it had no verification row at all
  (found=false, `OutcomeFailed` either way - this predicate changes nothing in that case).
  Tests: `TestEvaluateEnforcement_OrphanAfterApproval_StillPassed`,
  `TestEvaluateEnforcement_OrphanOnAnotherAccount_DoesNotMaskRejection`,
  `TestEvaluateEnforcement_OrphanOnly_TreatedAsNoVerification`, and
  `TestEvaluateEnforcement_DecidedOrderingIgnoresInterveningOrphans` (an orphan landing BETWEEN
  two decided rows must not disturb ordering by decision time) - all in
  `internal/kyc/enforcement_integration_test.go`. This is the one change this fix round makes
  to `internal/kyc/enforcement.go` - explicitly in scope per this task's own authorization,
  coordinated with ADR 0096 rather than decided unilaterally.
- **F2 - the unknown-reference 503 was silent, with no `Retry-After`, and the permanent-503
  gap was undocumented.** Fixed: `internal/httpserver/kyc_admin_handlers.go`'s
  `ErrVerificationReferenceUnknown` branch now writes one allow-listed
  `kyc_webhook_reference_unknown` log line (`request_id`/`tenant_id`/`provider_id` only, never
  the reference or the callback body) via the existing `writeAdmissionRejection` helper (the
  same one every other webhook 503 in this codebase already uses), which also now sets
  `Retry-After`. The permanent-503 gap (`CreateVerification`'s phase C failing after the vendor
  already accepted the request leaves the row `unverified`/`provider_reference NULL` forever;
  every subsequent callback for that reference 503s until the vendor's own retry policy gives
  up, and the decision is never linked to any platform row) is now explicitly documented as
  part of `KYC-SUBMIT-OUTBOX-1`'s own scope (§15.3's "Failure recovery" row already names this
  as a hard precondition on the first real KYC adapter; this fix round adds the specific
  mechanism - a durable pre-phase-C record of the pending reference - as what a submission
  outbox would need to provide to close it).
- **F3 - four of the original evidence file's mutants were equivalent claims, not tested
  branches; the idempotency-key test was tautological.** Code review found `SubmitVerification`'s
  OWN credential-binding check (MB), `CreateVerification`'s phase-C CAS predicate (MC), the
  `sort.Strings` ordering in `submissionIdempotencyKey` (MD - the original test only ever
  submitted an EMPTY document set, so ordering was never exercised, and its own "expected" value
  was computed by calling `submissionIdempotencyKey` itself), and `SubmitVerification`'s
  nil-outbound-resolver guard (ME) all survived untested. Fixed: four new tests -
  `TestSubmitVerification_CredentialBindingMismatchFailsClosed`,
  `TestCreateVerification_PhaseCCASRejectsAlreadyAppliedRow` (calls
  `applyCreateVerificationResult` a second time with a stale pre-phase-C value after a real
  `CreateVerification` call already applied its own result, and asserts the CAS refuses),
  `TestSubmitVerification_IdempotencyKeyStableForMultiDocumentSet` (replaces the empty-set test
  with a THREE-document set uploaded out of sorted order, with the expected key computed by a
  completely independent `independentSubmissionIdempotencyKey` helper - `crypto/sha256` and
  `sort.Strings` called directly, never through `submissionIdempotencyKey`), and
  `TestSubmitVerification_NilOutboundResolverFailsClosedWithoutPanic` - all in
  `internal/kyc/kyc_two_phase_integration_test.go`. The evidence file is corrected (superseded,
  not merely appended) with the four new mutants and a private-DB convention note; see the
  updated `docs/plans/payment-readiness/evidence/prh-i2-kyc-mutation-kill.txt`.
- **F4 (C2) - `OutboundKindSplitResolver` had no tests; a mutant making it always prefer the
  mock resolver survived the full suite.** Fixed: `internal/kyc/outbound_kindsplit_test.go`
  (new, unit tests, no database) ports `internal/casino/outbound_kindsplit_test.go`'s five
  cases verbatim for KYC (synthetic adapter → mock only; non-synthetic adapter → real only;
  unregistered id fails closed; both nil yields a true nil interface; a synthetic adapter with
  nil mock fails closed with no fallback to real), and
  `cmd/platform-api/wiring_test.go` gained `TestKYCOutboundResolver_FollowsWiring`, casino's own
  `TestCasinoOutboundResolver_FollowsWiring`'s KYC twin, proving the wiring itself (not just the
  resolver's own logic) reaches the mock adapter's registered provider id and fails closed for
  an unregistered one. Confirmed to kill the reviewer's own mutant (`OutboundKindSplitResolver.
  Resolve` hard-coded to `target := s.mock`).
- **C3 - the unknown-reference 503 had no operator signal at all.** Folded into F2 above (the
  same fix closes both the security and code-review framings of this finding).
- **C4 - the empty-document-set no-op was documented but not implemented.** Fixed:
  `SubmitVerification` now checks `len(submitted) == 0` immediately after phase A (mirroring
  the terminal-verification no-op just above it) and returns without ever calling the provider.
  Test: `TestSubmitVerification_EmptyDocumentSetIsANoOp`. This is a genuine, small behavior
  change (a verification with zero non-rejected documents no longer receives a vendor decision
  at all until at least one document is uploaded) matching the doc comment's own long-standing,
  never-implemented claim - several pre-existing tests that previously submitted an empty set
  incidentally (to observe the provider call for an unrelated reason) were updated to seed at
  least one document first (`internal/kyc/kyc_two_phase_integration_test.go`'s `seedDocument`
  helper, new; `internal/kyc/reason_platform_normalize_integration_test.go`'s
  `TestPlatformNormalizesReason_SubmitVerification`).
- **C5 - raw provider/resolver error text reached operator logs.** `internal/httpserver/
  kyc_handlers.go`'s `create_verification_provider_unavailable` and `submit_verification_failed`
  log lines logged `err`/`submitErr` directly with `%v`, which can embed a real adapter's or
  resolver's own error text (potentially a vendor response body, header, or a credential-bearing
  URL, once a real adapter exists). Fixed with a new exported classifier,
  `kyc.RedactedProviderErrorDetail` (`internal/kyc/provider.go`) - KYC's own copy of
  `casino.redactedLaunchFailureDetail`/`internal/payments/gate.go`'s `redactedReason` (the same
  "each domain owns its own, since these two packages must not import each other" discipline
  those two already follow) - both log lines now log its bounded classification instead of the
  raw error.
- **Not done here** (explicitly out of this task's own scope, confirmed unaffected): any change
  to `internal/kyc/enforcement*.go` beyond the single, explicitly authorized F1 predicate change
  above; migrations 0100/0103; the payments (PRH-I1) implementation; casino.

Verification (fix round): `gofmt`, `go build ./...`, `go vet ./...` and `go vet -tags=integration
./...` - all clean. `golangci-lint run ./...` (v2.9.0, no build tags, matching CI's own
invocation) - 0 issues. `go test -tags=integration -race -count=1 ./internal/kyc/...` and
`go test -tags=integration -count=1 ./internal/httpserver/... -run 'KYC|Kyc'` and
`go test -tags=integration -count=1 ./cmd/platform-api/...` - all pass, against fresh private
databases created via `TEST_ADMIN_DATABASE_URL` and migrated to head (0104), never the shared
CI-local instance (which this sandbox's own prior sessions have left short of migration 101's
own pre-flight guard, an unrelated, pre-existing data-hygiene issue - see ADR 0096 §18.4's
identical note). Mutation evidence (corrected, superseding §15.3.2's original file):
`docs/plans/payment-readiness/evidence/prh-i2-kyc-mutation-kill.txt`.

#### 15.3.4 Fix record (second code re-review + security re-verification, 2026-09-27)

Full record of this round in ADR 0096 §20 (KYC enforcement's own ADR is the fuller home for
most of this round's findings, since N-1/the mirror race are enforcement/staff-decision
concerns, not transaction-boundary concerns per se). Two items belong here, in §15.3, because
they correct/extend this section's own phase-C description:

- **Phase C is forward-only for the callback path too, not only for `SubmitVerification`'s own
  phase C.** §15.3.3's R1 fix (`applyForwardOnlyStatus`) already applied to `SubmitVerification`;
  the callback path (`applyCallbackOutcome`, `internal/kyc/provider.go`) had always carried the
  identical forward-only rank rule, but as a SECOND, separately hand-maintained copy of the same
  loop rather than a shared call. Code re-review's N6 consolidated both onto the one shared
  `applyForwardOnlyStatus` function - no behavioural change (both copies already implemented the
  identical rank rule; verified by the full pre-existing callback suite passing unchanged after
  the refactor), but this removes the risk of the two copies silently drifting apart in a future
  change. Stated explicitly for this section's own record: a `review_required` decision, once
  written by either path, is never demoted back to `pending` by a later submission result OR a
  later callback - both paths only ever move a verification's status forward along
  `statusRank` (unverified < pending < review_required < terminal), silently no-op'ing (not
  erroring) on a would-be backward or same-rank write.
- **`ReviewVerification` (staff review - approve/reject/require-more-documents) now also CAS's,
  but with different failure semantics than the provider/callback paths' own silent no-op.** This
  function sits outside §15.2/§15.3's own phase A/B/C shape (it has no provider call of its own),
  but shares the SAME underlying hazard the forward-only CAS above addresses: a concurrent
  provider submission or callback landing between `ReviewVerification`'s own read and its
  `UPDATE` could previously overwrite, or be overwritten by, the staff decision with no error to
  either side. Fixed with its own CAS against the status `ReviewVerification`'s own read observed
  and a new sentinel, `ErrVerificationStatusConflict`, on a lost race - **deliberately NOT a
  silent no-op like `applyForwardOnlyStatus`**: a staff decision is a deliberate, one-time human
  judgment call, not a replayable provider transition, so a lost race must surface as a visible
  conflict the staff member can see and retry with fresh information. Full record: ADR 0096 §20.3.

#### 15.3.5 Fix record (IO-1B/IO-1C, architect review, 2026-09-27)

Architect review (`rv-prh-architect.md`) found the same two gaps for KYC that §15.1.3 records
for casino: `CreateVerification`'s and `SubmitVerification`'s own phase B had no in-gate
`txscope.Held(ctx)` refusal, and `MockOutboundResolver.Resolve` (`internal/kyc/callcontext.go`)
ignored `ctx` entirely - contradicting §3.2's and §11's own claims (both corrected above).

**IO-1B fix.** A new sentinel, `kyc.ErrProviderCallRefused` (`provider.go`), mirroring
`payments.ErrProviderCallRefused`/`casino.ErrProviderCallRefused` exactly. Both
`CreateVerification` (`verification_service.go`) and `SubmitVerification`
(`document_service.go`) now refuse with it immediately before their own adapter call
(`provider.CreateVerification`/`provider.SubmitVerification` respectively) if
`txscope.Held(ctx)`. `MockOutboundResolver.Resolve` now refuses under `txscope.Held(ctx)` too,
exactly like the real `providercred.OutboundResolver`. Tests (`internal/kyc/
kyc_two_phase_integration_test.go`): `TestCreateVerification_RefusesUnderTxscopeHeld` and
`TestSubmitVerification_RefusesUnderTxscopeHeld` (each calls the function from inside a
`pool.WithTenant` closure using a resolver that does NOT itself refuse, isolating the
call-site guard; asserts `ErrProviderCallRefused` and that the adapter's own method is called
zero times), and `TestMockOutboundResolver_RefusesUnderTxscopeHeld` (the resolver's own
refusal, directly). All mutation-killed.

**IO-1C fix.** INV-IO-1(c)'s own static scan - referenced by this ADR (§2989/I1-c, "static
scan") but, per architect review, never actually implemented - now exists:
`internal/txscope/no_provider_call_in_tx_closure_static_test.go`,
`TestINV_IO_1c_NoAdapterCallInsideTxClosure`. Pure `go/ast` + `go/parser` (no `go/types`,
matching this codebase's own established convention for this class of guard -
`internal/providerkind/completeness_scan.go`'s identical note - and this ADR's own
`internal/ledger/lockorder_static_test.go` precedent), placed in `internal/txscope` (neutral
ground: it scans `internal/casino`, `internal/kyc` AND `internal/payments` together, and
belongs to none of them individually). It detects a database-transaction closure
STRUCTURALLY - any function literal whose own parameter list declares a `pgx.Tx` parameter,
regardless of which function it is passed to (so it does not need updating every time
`db.Pool` grows a new `With*` method) - and fails if any of the three provider interfaces'
own outbound-network methods (`casino.CasinoProvider`: `Catalogue`/`Launch`/`Balance`/`Bet`/
`Win`/`Rollback`; `kyc.KYCProvider`: `CreateVerification`/`SubmitVerification`/
`GetVerification`; `payments.PaymentProvider`: `Deposit`/`Withdraw`/`QueryStatus`) is called
LEXICALLY inside that closure's own body - deliberately not a call-graph analysis (a call
inside a same-package helper function the closure invokes is out of scope, stated rather than
papered over, the same convention `internal/ledger`'s own INV-LOCK-E1 guard uses for its
one-level callee-expansion limit). Each interface's own metadata/in-memory methods (`ID`,
`Capabilities`, `WebhookScheme`, `HealthStatus`, `HandleCallback`) are deliberately excluded -
none perform real network I/O, per each interface's own doc comment, so flagging them would be
noise, not a finding. Proven with a planted-violation test
(`TestINV_IO_1c_GuardCatchesPlantedViolation`) and a caller-name-independence test
(`TestINV_IO_1c_DetectsTxClosureRegardlessOfCallerName`), both passing; the real-tree scan
itself finds zero violations across all three domains today (confirming IO-1B's own phase-B
call sites are, and remain, correctly outside every transaction closure).

---

## 16. Failure injection and required tests

### 16.1 Crash and failure-injection points

These are hooks in the MOCK adapter and orchestrator seams, test-only. **"Connection lost"
points (CP-D5, and every "DB error causes a rollback/5xx" case in §16.2 item 4) use one named,
deterministic mechanism** (QA change 5): a test-only `net.Conn` proxy between pgx and Postgres
(`internal/testsupport/pgfault`, name final in I1-i) that can sever the connection at a chosen
protocol point, in particular **after the COMMIT message bytes are written and before the
CommandComplete is read**, or return an injected error on the Nth statement. No sleep- or
timing-based fault injection is used anywhere except the one intentional exception in item 14.

| ID | Point | Required converged outcome |
|---|---|---|
| CP-D1 | Player path: after phase A commit (T1+T2 in one tx, §5.1), before the call | Attempt `submitting` with `ever_possibly_sent=false`, which is **not** proof it was unsent; the outcome is exactly CP-D2's (LF95-C12). For a **cascade row** (T1 only): attempt `created`; the sweeper (non-interactive) or the same request's driver claims it via T2; an interactive, abandoned row → T3 after the window. The provider never saw a `created` row. |
| CP-D2 | After the claim commit (T1+T2 or T2), before the call | `submitting`. Lease expiry → sweeper `QueryStatus` → `not_found`: authoritative per §4.5 (never acknowledged, Δ elapsed since `last_sent_at`) → `declined` with `cascadable=false`; else `ambiguous` → T12 if allowed. No double submission without the same key. |
| CP-D3 | During the call: timeout after the provider accepted | T6 `ambiguous`. A later callback or poll → T7. No cascade. |
| CP-D4 | Provider accepted; crash before phase C | The callback resolves by merchant reference (T7), or it is deferred and applied at T4, or the sweeper resolves it. Exactly one posting. |
| CP-D5 | Phase C COMMIT connection lost (unknown commit) | Re-running phase C is a no-op or completes. Exactly one posting. |
| CP-D6 | Callback tx rolls back after `ledger.Post` | Nothing committed. Redelivery posts once. |
| CP-D7 | Crash after the decline commit, before the cascade attempt runs | Attempt n+1 exists (`created`). Driven by the sweeper (non-interactive) or expired (interactive). |
| CP-D8 | Worker crash holding leases | Items reappear at `lease_until`. No double money call. |
| CP-W1 | After the T1p commit, before `Withdraw` | Withdrawal `submitted`, attempt `submitting`. Sweeper `QueryStatus` by merchant reference, else `ambiguous`. **Never `Fail`.** |
| CP-W2 | `Withdraw` accepted; crash before phase C | Resolved by callback or poll. Exactly one Step B. |
| CP-W3 | `Complete` posting fails in phase C | Rollback. The retry posts once (`provider_id:provider_tx_id`). |
| CP-W4 | Concurrent staff double-submit | Exactly one T1p. The second gets `ErrStateConflict`. **Zero** extra provider calls (the MOCK counts calls). |
| CP-W5 | `Reject` racing the claim | Exactly one wins. If the claim wins, `Reject` is a 409 and no hold is released. |
| CP-W6 | Credential store outage at payout | T5 loop. The withdrawal stays `submitted`, the hold stays, and no call is made. |
| CP-W7 | KYC revoked between T5 and the sweeper T2 re-claim | No claim, no call; the attempt stays `created`, escalated; the hold is released only by staff M3 (`kyc_denied`). |
| CP-W8 | KYC revoked while the payout attempt is `ambiguous`, before T12 | No resend; no release; resolved only by poll or callback evidence. |

### 16.2 Adversarial test list (each financial test ends with INV-IO-13)

1. **No double credit.** Concurrent duplicate success callbacks, a success callback racing the
   sweeper poll, and a success callback racing phase C: exactly one Flow 1. **≥ 100 iterations
   in CI with `-race`** (QA change 8).
2. **No double debit or payout.** CP-W4 and CP-W5 (each **≥ 100 iterations with `-race`**). T12 resubmission with the MOCK in
   idempotent mode returns the original reference; in non-idempotent mode T12 never fires.
3. **No lost successful payment.**
   - CP-D2, CP-D4 and CP-W2 converge to `succeeded`, with the ledger posted, and without any
     callback (poll only).
   - The same without any poll (callback only).
   - Callback dropped, with reconciliation then flagging `pay_status_mismatch`, followed by T17
     posting.
4. **No lost callback.** A deferred receipt applied at T4. The receipt survives a crash between
   receipt and T4, and the sweeper backstop applies it. A DB error causes a rollback and a 5xx,
   and the redelivery applies it.
5. **No unsafe retry.**
   - `Ambiguous` never re-calls `Deposit`/`Withdraw` unless `IdempotentSubmission`.
   - `NotSent` after `ever_possibly_sent=true` is refused by the trigger (direct SQL attempt).
   - A payout is never re-routed.
6. **Ambiguous never treated as success.** An ambiguous sync result, poll or callback is never
   `succeeded`, and no posting occurs. A sync `Succeeded` from a manifest without
   `SyncSuccessPossible` is treated as `Ambiguous`.
7. **Duplicate callback idempotent.** The same event ×N sequentially and concurrently gives one
   receipt, one effect and a 200 each time. A duplicate of a rejected class keeps its existing
   HTTP code.
8. **Callback before or after the internal transition.** Every row of §6.4, both orders, with
   and without a merchant-reference echo, including callback-before-T2-commit (T15 path is
   *not* taken while `submitting`).
9. **Timeout is not auto-failure.** Deposit and payout pending past the horizon give T16 only,
   with the state unchanged. A payout `not_found` is never `declined`. A deposit `not_found` is
   `declined` only with `MerchantLookupAuthoritativeAfter` elapsed.
10. **Contradictions.** T13 (post plus P1), T14 (disputed, no `Complete`), T15 (no post), and a
    mismatched amount or asset → T10.
11. **Cascade.**
    - Synchronous cascade across 3 MOCKs with full exclusion history.
    - Asynchronous decline, non-interactive: cascade via the sweeper. Asynchronous decline,
      interactive: no cascade.
    - Concurrent cascades from two decline deliveries give exactly one attempt n+1 (partial
      unique index), **≥ 100 iterations with `-race`**.
    - No cascade on ambiguous.
12. **Kill switch.**
    - Engaged between routing and T2 means no call (CAS predicate).
    - Engaged payout claim gives 503 and the withdrawal stays `approved`.
    - In-flight polls and callbacks still apply.
    - Release requires a distinct approver.
    - A player route cannot reach it (route-table test).
    - A read error means no call.
    - Audit before/after.
13. **Capability fail-closed.** Registration refuses an inconsistent manifest, an adapter
    declaring `SupportsRefund`, or a production-eligible adapter with `RedeliveryOn401 ∈
    {terminal, unknown}` and no statement source. An unsupported operation at new activity
    gives T3 or 503. An unsupported event gives `unsupported_event`.
14. **INV-IO-1.**
    - The gate refuses under `txscope` (unit test). **IMPLEMENTED for all three domains as of
      2026-09-27 (IO-1B, architect review `rv-prh-architect.md`)** - payments'
      `gate.go` had this from the start; casino's `LaunchGame` and KYC's
      `CreateVerification`/`SubmitVerification` did not, until this fix (§15.1.3/§15.3.5).
    - A static scan finds no adapter-method call inside a `With*` closure (repo-wide, including
      casino and KYC). **IMPLEMENTED as of 2026-09-27 (IO-1C, architect review)**:
      `internal/txscope/no_provider_call_in_tx_closure_static_test.go`,
      `TestINV_IO_1c_NoAdapterCallInsideTxClosure` - this row's own reference to a static scan
      was, per that review, referring to a test that did not yet exist; it does now (§15.3.5's
      own fuller record).
    - A capture test: while the MOCK's `Deposit`/`Withdraw`/`QueryStatus`/`Launch`/
      `CreateVerification`/`SubmitVerification` executes, `pg_stat_activity` shows no
      `idle in transaction` backend for the test's application name, and the pool's acquired
      count is 0 for the calling goroutine.
    - A pool-starvation variant (**the one intentional sleep in this plan**: a
      connection-holding fixture reused from F-POOL-1, not a race-dependent assertion): N
      concurrent slow MOCK calls (sleep > 1 s) with a 20-connection
      pool; unrelated tenant queries meet the existing latency bound. This reuses the F-POOL-1
      fixture approach **in its own isolated CI lane under the security ruling-B rules**.
15. **PROV-OUTBOUND-CRED-1.**
    - Credential tenant or provider mismatch gives `NotSent` with no call.
    - Revoke between two calls: the second call is refused.
    - Rotation: the second call uses the new fingerprint and the derived token is evicted.
    - Store outage gives T5 with no breaker count.
    - The recursive reflection test of §11 (RV-0095 L2, superseding the earlier name- and
      type-based check). It walks **constructed** registered adapter values (payments, casino,
      KYC) through pointers, structs, slices, maps, interfaces and unexported fields. It fails
      on any `OutboundCredential`, `secretstore.Secret`, `httpclient.Authenticator` or derived
      token, and on func-typed fields that are not allow-listed.
    - The static test: no package-level variable of those types in adapter packages, and no
      adapter package importing `secretstore` or the Fetcher.
    - Binding checks each alone:
      - a `Domain` mismatch (for example a `casino` credential on a payments call) is `NotSent`
        with no call;
      - with a **non-checking fake adapter**, the gate alone refuses a tenant, provider or
        domain mismatch;
      - with the gate check **bypassed** (test seam), a conforming adapter alone refuses it.

      This proves that neither check alone is load-bearing (S95-C8(b)).
    - Redaction of `CallContext` in logs, errors and JSON.
16. **Lock order.** ADR 0082 harness extended with deposit-evidence and payout-evidence paths
    racing reversal, `RequestWithdrawal`, `Reject` and `Fail` on the same wallet, plus the §14
    harness additions (sweeper claim versus callback and phase C; deposit T2 re-claim versus an
    RG self-exclusion write for the same person). No deadlock across 500 iterations (existing
    harness style).
17. **State-machine exhaustiveness.** A table-driven test drives every (state, evidence) cell of
    §4.4 and every forbidden transition of §4.3 via direct SQL, which the trigger must reject.
18. **Casino and KYC.**
    - Launch: crash after phase A, and crash after vendor accept. A bet on a revoked session is
      rejected with no posting.
    - KYC create: crash after A leaves an orphan row that is not verified. The callback race
      gives a retryable response.
    - KYC create: the callback race returns a retryable 5xx, never 200 (IC condition 1).
    - KYC submit: crash after the upload commit is recovered by the next upload.
    - KYC submit: an ambiguous, timeout or transport-error `SubmitVerification` result leaves
      `kyc_verifications.status` unchanged (IC condition 2).
    - Casino and KYC re-check DB failure → 503 with zero rows; revoked handle → 401 with zero
      rows (S-Q3).
19. **Ledger-finance additions (LF95-C14; each ends with INV-IO-13).**
    - Player retry with the same key, concurrent with an in-flight `submitting` attempt: one
      intent, and a MOCK call count of 1.
    - T13 with a `created` sibling: the sibling is rejected and never called.
    - A tombstone before success leads to `disputed`, with no loop and no 5xx.
    - A deferred **decline** receipt applied at T4 cascades per the persisted `cascadable`.
    - A disputed attempt leaves the intent `ambiguous`.
    - The LF95-C3 cross-attempt reference conflict gives `anomaly`, committed, with no 5xx; a
      mismatched success is committed as `disputed` with its receipt.
    - The LF95-C10 (a)–(c) KYC cases (T1p deny → `DenyForCompliance` with no attempt; CP-W7;
      CP-W8) and (d) no release after possible dispatch; (e) a deposit T2 resume re-runs RG and
      the KYC deposit gate.
    - `NotSent` on a T12 resend returns to `ambiguous`, never `created` (LF95-C1).
    - **T13t** (RV-0095 ledger C6(d)): an attempt in `declined` receives a matching success
      whose reference is held by a reversal tombstone. It is committed as `disputed`, with no
      posting, no trigger rejection and no 5xx; a redelivery is a no-op.
    - **T12 with a succeeded sibling** (N3): after T13 on attempt 1, an `ambiguous` attempt 2 is
      never re-sent (MOCK call count unchanged) and stays `ambiguous`.
    - **INSERT guard** (N2): each forbidden INSERT shape (direct `succeeded`, `declined`,
      `disputed`, `pending`; `legacy_backfill = true`; `last_evidence_kind = 'legacy'`;
      `ever_possibly_sent = true`; a non-NULL `ledger_transaction_id` or `provider_reference`) is
      refused, including under an arbitrary session GUC.
    - **Payout T2 gate non-pass cadence** (L2): the attempt is re-gated at the escalated cadence,
      not every lease period; a later pass resumes via T2; the M3 path audits `kyc_denied`
      (L3).
    - A reversal of a T13 second capture reverses the second capture, not the first
      (LF95-C6(b)). *[SUPERSEDED by §28.12: re-stated as "a reversal naming the disputed
      second capture's reference is tombstoned (no ledger effect); a reversal naming the posted
      capture reverses that attempt's own posting".]*
    - `P95-C1`: `TestMigration0082_DepositIntentsProviderColumnsStayMutable` keeps passing, and
      a new test asserts the intent's provider columns mirror the latest attempt through a
      cascade.
20. **Security authorization and isolation tests (§22.5, all ten).**
    - Tenant A staff against each §10.5 route with tenant B's switch, attempt or withdrawal id →
      404/403, never data; B unchanged.
    - A player token → 401/403 on every new route (route table plus one live request each).
    - A release requester approving their own request is refused by the application **and**
      the CHECK; a direct `UPDATE engaged=false` or `DELETE` is refused by the trigger (S95-C5).
    - **Kill-switch guard (RV-0095 N1–N4, L3, L4), each by direct SQL as well as through the
      API:**
      - A direct UPDATE of `provider_scope`, `operation_scope` or `tenant_id` on an engaged row
        is refused (N1).
      - A release whose `release_request_id` names a request approved in an **earlier**
        transaction, a request for a different switch, an expired request, or a request whose
        `expected_version` is stale is refused. Reusing a consumed request is refused (N4).
      - Client-supplied `requested_by`, `approved_by`, `changed_by` and `*_by_scope` values are
        ignored and replaced from the session. A tenant connection cannot write `'platform'` in
        any scope column. A session with neither shape (for example player-scoped, or both GUCs
        set) cannot write either table (N2).
      - A tenant re-engage or UPDATE of a platform-engaged row is refused, and
        `engaged_by_scope` never goes `platform→tenant` (N2).
      - `expires_at` beyond `created_at + 24 h` is refused, and client `decided_at`/`decided_txid`
        values are ignored (N4).
      - A release request referencing another tenant's switch id is refused by the composite FK
        (L4).
      - **Request-row immutability (N5):** an approve UPDATE that also sets `requested_by`,
        `requested_by_scope`, `kill_switch_id`, `expected_version` or `reason_code` is refused.
        A single actor who created the request cannot release through it by any sequence of
        statements. A client-supplied `approved_by*`/`decided_*` at INSERT or on cancel is
        refused or ignored. Any UPDATE of a terminal request is refused.
      - **L5:** a tenant session cannot INSERT a request for a platform-engaged switch, and the
        platform's own request succeeds. A switch INSERT carrying a `release_request_id` stores
        NULL, and the other switch's legitimate release still succeeds.
      - A client-supplied `received_at` or `resolved_at` on a receipt is replaced by the DB
        clock (L3).
    - A platform-engaged switch cannot be released by tenant principals, and a tenant-principal
      pair cannot release it even if the application asserts `'platform'` (S95-C7).
    - **Platform routes (N3):**
      - a platform principal pair engages and releases a target tenant's switch through the
        platform API, under `WithPlatformAdmin` and the platform RLS family;
      - a tenant principal gets 403/404 on every platform route and nothing is read;
      - a platform principal gets 403 on every tenant route;
      - audit carries both the actor and the target tenant.
    - **Claim-path coupling (L1):** T1+T2 run under a player-scoped context, and under a
      platform session, fails and makes no provider call.
    - A cross-provider, same-tenant merchant-reference callback gives no posting and no state
      change, including for `created`/`rejected` targets (S95-C1); cross-tenant is pinned too.
    - The claim under a misbound or unset tenant context claims nothing and calls nothing
      (S95-C6).
    - The secret-in-query leak test and the recursive adapter reflection test (S95-C8).
    - Re-check DB error → 503 with zero rows; re-check revoked → 401 with zero rows (S-Q3).
    - Deferred-receipt cap → 503 above the cap, nothing stored; a receipt predating
      `first_submitted_at` is not applied (S95-C2, S95-C3).
    - The four 200 dispositions return byte-identical bodies apart from the request id (S95-C4).
    - OpenAPI conformance for the §10.5 routes on both surfaces (tenant and platform kill-switch
      engage/release/approve/list, T17 re-verify, M3, attempt read): request/response shapes pinned, OpenAPI diff checked in CI
      (QA change 4).
    - The adapter conformance suite asserts no payer-identifying vendor field reaches
      `CallbackEvent`, `StatusResult` or a statement line (S95-C10).
21. **Breaker** (QA change 6). Force a (tenant, provider) breaker closed → open → half-open →
    closed with MOCK transport failures; assert tenant B's breaker for the same provider is
    unaffected; `DefiniteDecline` does not count; a credential-store outage does not count; an
    open breaker removes the candidate from routing without a DB transaction being held.
22. **Migrations** (QA changes 2 and 3).
    - Up/down/up round-trip for 0101, 0102 and 0103 leaves no orphaned constraint, trigger or
      policy.
    - Backfill over a fixture with a pre-existing violation (two live attempts; a non-terminal
      intent with NULL `provider_id`; a `succeeded` intent with NULL `ledger_transaction_id`)
      aborts, lists the ids, and writes nothing.
    - A backfilled `ambiguous` intent is picked up by the next sweep and converges by
      `QueryStatus`; a non-terminal intent without a reference is backfilled as `ambiguous`
      (its own named fixture); backfilled rows carry the N4 column values (RV-0095 ledger N4).
    - Backfill over clean synthetic data produces exactly the §13.1 mapping, including
      `reversed` withdrawals, `id = parent id`, `legacy_backfill = true` and
      `'legacy_unknown'`; a legacy merchant-reference callback resolves; T12 on a legacy row is
      refused by the trigger (LF95-C11, LF95-C14).
    - RLS cross-tenant tests for every new table (`payment_attempts`,
      `payment_provider_events`, `payment_kill_switches`, `payment_kill_switch_release_requests`,
      `payment_statement_imports`, `payment_statement_lines`), reusing the existing
      `tenant_staff_scope` RLS harness: tenant A can neither read nor affect tenant B's rows.
    - The mismatch-kind CHECK in 0103 is a strict superset of 0098.

### 16.3 Reconciliation tests (PRH-I5)

- One divergent fixture source per mismatch kind (8), each detected exactly once, with a
  deterministic mismatch ordering.
- A clean MOCK run over a mixed history (deposits, reversal, tombstone, payouts, declines,
  cascade).
- Coverage window: a platform record before `CoverageStart` is not flagged.
- `pay_unresolved` is flagged only past the horizon.
- The run refuses outside REPEATABLE READ.
- A statement-capture test proves the match tx writes only run and mismatch rows (INV-IO-12),
  and that `Fetch` runs with `txscope.Held == false`.
- Ingest is idempotent on re-fetch of the same content.
- Append-only triggers reject UPDATE and DELETE.
- The `providerkind` guard refuses the MOCK source in production.
- The end-to-end LF-C1(b) path: a dropped success callback is flagged, the payments re-drive
  job (LF95-R1) requests T17, T17 posts it, and the next run is clean.
- The ledger join (LF95-C13): a `succeeded` attempt with no posting, and a `psp_clearing`
  posting with no attempt, are each flagged; a payout line matching only the settlement
  reference is a match.
- A statement line of provider A never matches provider B's attempt (S95-C1).
- An unresolved verified receipt older than the `SettlementWindow` is `pay_unresolved`
  (LF95-C5).
- An import above the line cap is refused with nothing stored; oversized fields are refused by
  the CHECKs (S95-C11).

### 16.4 Mutation checks (to record in the implementing review)

| ID | Mutation | Test that must fail |
|---|---|---|
| MX1 | Call the adapter inside phase A | 14 |
| MX2 | Drop the kill-switch predicate from T2 | 12 |
| MX3 | Allow `ambiguous→declined` for a payout on timeout | 9 and 17 |
| MX4 | Generate the external key per try | 2 (MOCK idempotent mode) |
| MX5 | Drop the merchant-reference resolution | 8 |
| MX6 | Skip the receipt insert | 4 |
| MX7 | Remove `payment_attempts_one_live_per_intent` | 11 |
| MX8 | Treat `not_found` as a decline without the manifest | 9 |
| MX9 | Let the reconciliation stream call `ledger.Post` | 16.3 capture |
| MX10 | Call the provider without a **committed** `submitting` claim token (INV-IO-2) | gate unit test; CP-D1/CP-W1 |
| MX11 | Bypass the CAS predicate in application code (unconditional `UPDATE … SET state`) (INV-IO-4) | 17 (application path) and 1 |
| MX12 | Post the ledger effect from phase B, or take the attempt lock before the parent (INV-IO-5) | 14 capture and 16 |
| MX13 | Set `ever_possibly_sent` without a send, or skip setting it on `Ambiguous` (INV-IO-9) | 5 |
| MX14 | Let an adapter keep the credential across two calls (INV-IO-11) | 15 (reflection and revoke-between-calls) |
| MX15 | Drop the `provider_id = verified provider` predicate from merchant-reference resolution (INV-IO-14, S95-C1) | 20 (cross-provider) |
| MX16 | Remove the `last_evidence_kind` trigger check (LF95-C2) | 17 |
| MX17 | Drop the kill-switch release trigger (S95-C5) | 20 (direct `UPDATE`/`DELETE`) |
| MX19 | Remove scope-column immutability from the switch guard (RV-0095 N1) | 20 (re-scope an engaged row) |
| MX20 | Accept a client-supplied `*_by_scope` value (RV-0095 N2) | 20 (tenant writes `'platform'`) |
| MX21 | Drop the `decided_txid = txid_current()` check (RV-0095 N4) | 20 (earlier-tx approval) |
| MX25 | Allow an UPDATE of `release_requests.requested_by` (RV-0095 N5) | 20 (request-row immutability) |
| MX22 | Remove the `payment_attempts` INSERT guard (RV-0095 ledger N2) | 19 (forbidden INSERT shapes) |
| MX23 | Drop the succeeded-sibling predicate from T12 (RV-0095 ledger N3) | 19 (T12 with a succeeded sibling) |
| MX24 | Run the deposit gates after the parent lock (RV-0095 ledger N1) | 16 (RG self-exclusion race; lock-order static check) |
| MX18 | Drop the `received_at >= first_submitted_at` rule (S95-C3) | 20 (pre-submission receipt) |

Every INV-IO row now has at least one mutation and a named failing test (QA change 1).

### 16.5 Placement, CI lanes and time budgets (QA change 7; `RECOMMENDATION`, confirmed by `qa` in I1-i)

| Suite | Package | Budget |
|---|---|---|
| State machine, evidence matrix, trigger and mutation tests | `internal/payments` | adds ≤ 30 s to the package |
| Crash-point and adversarial suite (§16.1, §16.2 items 1–13, 15–17, 19–22) | new `internal/payments/ioboundary`, sharing the existing Postgres harness | ≤ 180 s with `-race`, including the ≥ 100-iteration cases |
| Pool-starvation and `pg_stat_activity` capture tests (§16.2 item 14) | their own isolated CI lane, under the security ruling-B rules (blocking, no retries, name guard) | ≤ 120 s, not counted against `internal/httpserver`'s 310 s |
| Casino and KYC (§16.2 item 18) | `internal/casino`, `internal/kyc` | adds ≤ 20 s each |
| Reconciliation (§16.3) | `internal/reconciliation` (payment_statement files beside casino_statement) | ≤ 60 s |

If a budget is exceeded, `qa` re-measures and records the number; tests are never weakened to
fit a budget.

---

## 17. Conformance to ledger-finance LF-C2 and LF-C1

| Constraint | Where satisfied |
|---|---|
| LF-C2 #1: intent committed in `submitting` before any call; no tx held | D1; INV-IO-1, INV-IO-2; T2/T1p commit before phase B |
| LF-C2 #2: deterministic, persisted external key; player retry resumes | INV-IO-3; §5.1 "Idempotency keys"; for backfilled rows `id = parent id` and T12 forbidden (§13.1, LF95-C11) |
| LF-C2 #3: explicit CAS state machine; illegal and backward transitions rejected; every transition audited | §4.3; INV-IO-4; trigger with `last_evidence_kind` (LF95-C1, LF95-C2) |
| LF-C2 #4: callback resolves by merchant reference and provider reference, and accepts `submitting` | §6.1 step 4 (bound to the verified provider, INV-IO-14); §6.4; T7/T8 from `submitting`; receipts persist every applied field (LF95-C3, LF95-C4); no unconvergeable success (LF95-C5) |
| LF-C2 #5: sweeper `QueryStatus` with no tx; never auto-declines on unknown; never cascades on unknown | §7; §4.5 (LF95-C8); §4.6 |
| LF-C2 #6: cascade through an outbox, never inline in a webhook tx | D3; §4.6; §6.5 |
| LF-C2 #7: ledger posting never spans I/O; ADR 0082 order; authoritative read in the same tx | INV-IO-5; §14 (A7 with LF95-C9 scope rules) |
| LF-C2 #8: failure-injection tests end with the SUM and rebuild assertions | INV-IO-13; §16 (incl. item 19, LF95-C14) |
| LF-C2 #9: the same rule for payout dispatch | §5.2, §5.2.1 (KYC gate, LF95-C10); T1p; CP-W1..W8; INV-IO-7 |
| LF-C1: record redelivery semantics; (a) or (b) | §6.6: (b) enforced at startup; (a) approved narrowly by `security` and specified (typed `RecheckUnavailableError` → 503) |

---

## 18. Implementation work breakdown

Every item starts `NOT IMPLEMENTED`. The ADR must reach ACCEPTED (ledger-finance plus security
sign-off) before I1 starts.

### PRH-I1 — payments (+ `ledger-finance`; reviewers `security`, `code-reviewer`, `qa`)

| # | Item | Depends on |
|---|---|---|
| I1-a | Migration **0101** (tables, indexes, guard triggers, pre-flight plus backfill per §13.1, RLS tests, up/down tests) | 0099 merged; 0100 (ADR 0096) merged; ADR 0082 A7 written by `ledger-finance` |
| I1-b | Contract (§9): `CallContext` (with redacting renderers), `ErrorClass`, `StatusQuery`, `CallbackEvent` fields (`MerchantReference`, `SettlementReference`, `DeclineStage`, canonical `DeclineReason`), manifest (§10.1, including `CallbackEchoesMerchantReference` and `WebhookRetrySemantics`), `HealthStatus` contract. MOCK extended: tenant-tagged records; merchant-reference lookup; idempotent-by-key mode; `NotSent`/`NotProcessed`/`Ambiguous`/timeout/crash hooks; call counters. Conformance suite cases. | — |
| I1-c | Provider-call gate (§3.2, including the binding check and transport-error redaction) plus the outbound credential plumbing (§11) plus the synthetic credential source; static scan; recursive reflection test; secret-in-query test | I1-b |
| I1-d | `applyEvidence`, transition functions, audit; `InitiateDeposit` rewrite (takes `*db.Pool`); routing split; breaker | I1-a, I1-c |
| I1-e | Callback path: provider-bound resolution, receipts (cap, predates-submission rule), deferred application, uniform 200 bodies, `RecheckUnavailableError` → 503, removal of inline I/O and cascade | I1-d |
| I1-f | Payout dispatch and resolve handlers rewritten with the §5.2.1 KYC gate placement (consuming the `internal/kyc` enforcement function from PRH-I3 and ADR 0096's `withdrawal.DenyForCompliance`); `withdrawal.ClaimForDispatch`/`RecordProviderReference`; M3; `withdrawal-state-machine.md` update (P95-C2, before PRH-I1 is marked complete) | I1-d; PRH-I3 KYC function available |
| I1-g | Sweeper worker (§7) with its `cmd/platform-api` wiring and nudge | I1-d, I1-e |
| I1-h | Migration 0102 plus kill-switch service, guard triggers, `engaged_by_scope`, the §10.5 routes and permissions, four-eyes release, engage alerts, audit, OpenAPI; manifest registration checks (incl. the LF-C1(b), LF95-C5, LF95-C8(c), `WebhookRetrySemantics` and S95-C12 rules) | I1-b |
| I1-i | Tests §16.1, §16.2 items 1–17 and 19–22, mutations MX1–MX8 and MX10–MX18, §16.5 lanes and budgets; tripwire message update (the tripwire itself stays) | all of the above |

I1-b, I1-h and I1-a can proceed in parallel. I1-d through I1-g are sequential.

### PRH-I2 — casino plus KYC (`casino`, `identity-compliance`)

| # | Item | Depends on |
|---|---|---|
| I2-a | Shared call-gate helper (tiny; `txscope` refusal plus deadline). It is either extracted from I1-c or written first and consumed by I1-c. It is the only coordination point. | — |
| I2-b | Casino launch split (§15.1), `LaunchRequest.Call`, `HealthStatus` outside the tx, audits; casino `RecheckUnavailableError` → 503 (S-Q3) | I2-a |
| I2-c | KYC create and submit split (§15.2–15.3), `Call` on the inputs, callback race → retryable 5xx, ambiguous submit leaves status unchanged; KYC `RecheckUnavailableError` → 503 (S-Q3) | I2-a |
| I2-d | Tests §16.2 item 18, plus casino and KYC in the scans for item 14 and item 15 | I2-b, I2-c |

PRH-I2 does not depend on migrations 0101/0102/0103 and can run fully in parallel with I1.
KYC-SUBMIT-OUTBOX-1 is a hard precondition on accepting any real KYC adapter (§15.3).

### PRH-I5 — payment reconciliation (`payments` + `ledger-finance`)

| # | Item | Depends on |
|---|---|---|
| I5-a | `statement.PaymentStatementSource` leaf types (§9.5) | — |
| I5-b | Migration 0103 | 0101 (FK-free, but it reads `payment_attempts` and `payment_provider_events` at match time) |
| I5-c | Fetch (through the gate, capped) → ingest → match stream, the eight classifiers, the ledger join (LF95-C13), provider-bound matching, `TryRun…ForTenant`, scheduler wiring; the `payments`-owned re-drive job (LF95-R1) | I5-a, I5-b, I1-a |
| I5-d | `payments.MockStatementSource` | I1-b (tenant-tagged MOCK records) |
| I5-e | Tests §16.3, MX9; `reconciliation-model.md` §2.2 amendment and a new §2.2 implementation status | I5-c, I5-d |

The matcher (I5-c) can be written against fixture sources before I1-d lands.

### Specialist-owned document changes (not code)

- `ledger-finance`: ADR 0082 A7.
- `payments`: `payment-orchestration.md` §5 note; `withdrawal-state-machine.md` (P95-C2, no later
  than I1-f).
- `identity-compliance`: ADR 0096 names the T2-payout-reclaim case and routes it to M3 (IC
  condition 4; §5.2.1 here is this ADR's side).
- `ledger-finance`: `reconciliation-model.md`.
- `security`: the ADR 0093 §5 pointer.

---

## 19. Decisions

### 19.1 Human decisions (only the unavoidable one)

**HD-0095-1: manual resolution of an unresolvable payout, and of disputed attempts.** Decide who
may force-resolve (M1/M2), above what amount four-eyes applies, and whether a payout may
*ever* be declared "not paid" without provider evidence. This ties to LEDGER-MANUAL-ADJ-4EYES-1.

- This ADR **does not decide it**.
- Until decided, M1 and M2 are **BLOCKED**: no API is built, and affected attempts stay
  `ambiguous`/`disputed`, visible and P1.
- M3 (never-sent payouts) does not need the decision.

### 19.2 Specialist review questions — all resolved by the reviews

| ID | Owner | Ruling | Where written |
|---|---|---|---|
| LF-Q1 | `ledger-finance` | T13 posts to `player_cash`; no suspense account (§21.2), with LF95-C6 *[SUPERSEDED for the multiple-success case by §28.7]* | §4.3 T13, §4.4 |
| LF-Q2 | `ledger-finance` | A7 accepted; R0 between L0 and L1, with LF95-C9 scope rules (§21.3) | §14 |
| LF-Q3 | `ledger-finance` | §2.2(b) amendment accepted, with the ledger join LF95-C13 (§21.4) | §12.3, §12.6 |
| S-Q1 | `security` | Engage single actor; release four-eyes, not relaxed (§22.1) | §10.2, §10.4 |
| S-Q2 | `security` | `deferred_unresolved` 200 acceptable, with S95-C2..C4 (§22.2) | §6.1, §6.2, §6.4 |
| S-Q3 | `security` | LF-C1 option (a) approved narrowly, additive to (b) (§22.3) | §6.6, §15 |
| IC-Q1 | `identity-compliance` | KYC race → retryable 5xx, never 200 (§25) | §15.2 |
| P-Q1 | `payments` | §7.3 defaults accepted; `TestMigration0082_DepositIntentsProviderColumnsStayMutable` relies on the mirror (§23) | §5.1, §16.2 item 19 |

---

## 20. Consequences, residuals and deferred items

### Positive

- The dual-write hazard (review 19 §6, High) is structurally impossible. Every money call
  follows a committed `submitting` row with a deterministic key, and every outcome converges
  through CAS evidence.
- Pool pinning by provider latency is eliminated for payments, casino launch and KYC. That is
  F-POOL-2's whole scope, including `HealthStatus` and the webhook cascade.
- The double-payout path on withdrawal submit is closed.
- Cascade history is complete.
- The platform gains a durable, auditable record of every verified callback.
- The PSP reconciliation stream exists, as a MOCK foundation.

### Costs

- A deposit is now 3 or more short transactions instead of 1.
- There is a background worker.
- The attempt and receipt tables grow with traffic. Retention and partitioning are deferred
  until a volume trigger.
- `deferred_unresolved` changes one 404 to a 200 (S-Q2, accepted by `security`), and a
  mismatched deposit success now commits `disputed` and returns the uniform 200 instead of
  rolling back to a 409 (LF95-C3, S95-C4).
- Kill-switch release needs a second person (S-Q1).
- The asynchronous interactive cascade is removed: a behaviour change, but a correct one
  (§4.6).

### Residuals, honestly stated

- **In-flight window.** A revoke, rotation or kill switch committed after a claim affects the
  next call, not the one already in flight (ADR 0093 precision 1).
- **Contradictory providers.** *[AMENDED by §28.10: a double deposit capture is still possible
  at the PSP, but it is never credited; it is held `disputed` and reported as
  `pay_captured_unposted`. The double-payout (T14) half stands.]* A provider that contradicts itself can still cause a double
  capture (T13) or a double payout (T14). The platform detects it (P1) but cannot prevent the
  provider's own behaviour. Remediation is BLOCKED on HD-0095-1 and
  LEDGER-MANUAL-ADJ-4EYES-1.
- **Vendor capabilities.** Payout resolution for a vendor with neither merchant-reference
  lookup nor idempotent submission ends in manual resolution (BLOCKED). The intake (planning
  gate §6 #11 and #16) must surface this before selection; this is a vendor-selection
  criterion.
- **MOCK reconciliation.** It is single-process and in-memory. Its evidence proves the
  matching and plumbing only, never agreement with any real provider (`MOCK`).
- *[WITHDRAWN by §28.10: no longer an accepted residual. A third (or any later) real capture
  takes the same no-credit path as the second.]* **T13 with a sibling already `submitting`.** T13 rejects a `created` sibling, but a sibling
  already `submitting` may still capture a third time; it is detected under the same P1
  (`multiple_success_for_intent`), with remediation BLOCKED on LEDGER-MANUAL-ADJ-4EYES-1
  (LF95-C6(c)).
- **Pre-planted receipts from a verified sender** are neutralised by the
  `received_at >= first_submitted_at` rule and the cap; a verified sender can still post
  anything it could post after T4, because it is the same trust principal (S95-C3).
- **KYC create without vendor idempotency** can create a second vendor-side verification on a
  player retry (PROVIDER DEPENDENT; no financial or enforcement hazard).

### Findings and deferred candidates (for the orchestrator to register)

| ID | Kind | Item |
|---|---|---|
| **CAS-STMT-IO-1** | Finding, Low, not reachable today | `CasinoStatementSource.Statement(ctx, tx, …)` reads inside the run's transaction. A real casino statement source would be provider I/O inside a tx (the INV-IO-1 class). It must adopt the fetch → ingest → match split of §12.1 before the first real casino statement source; `casino` verifies the remediation (not just a reference) when that source is proposed, with `architect` and `ledger-finance` sign-off (§26). No redesign now. |
| KYC-SUBMIT-OUTBOX-1 | Deferred, **hard precondition** | Durable KYC submission outbox. No real KYC adapter is accepted without it (IC condition 5). Owner `identity-compliance`. |
| PAY-ATTEMPT-RETENTION-1 | Deferred | Retention and partitioning of `payment_attempts`/`payment_provider_events`. Trigger: a volume threshold. Binding constraint: never delete receipts of non-terminal attempts, or receipts younger than the longest declared `WebhookRetrySemantics.RetryWindow`; `security` reviews the design (S95-C3). |
| BONUS-T13-ELIGIBILITY-1 | Confirmation, owner `bonus-engine` | Confirm that a T13 second capture is intentionally not bonus-eligible (the intent link keeps pointing at the first posting) (LF95-R2). *[MOOT after §28: a second capture is never posted.]* |
| VENDOR-INTAKE-REF-PII-1 | Intake item, owner `payments` + `security` | Add to the planning-gate §6 intake checklist: "the vendor's reference formats contain no cardholder or payer-identifying data", and each `NotProcessed` code's documentation source (S95-C10, S95-C12(ii)). |
| PAY-PAYOUT-CASCADE-1 | Deferred | Product decision: payout cascade. Not needed now. |
| PROV-REVOKE-ALL-1 | Existing | Cross-tenant kill switch or revoke. |

### Labels

*[SUPERSEDED by the "Current status" table in the header (revision 4).]*

This ADR is `NOT IMPLEMENTED` in its entirety. After PRH-I1, I2 and I5:

- the payments state machine, casino and KYC will be `IMPLEMENTED` against `MOCK` adapters
  only;
- the payment reconciliation stream will be `MOCK`;
- real-vendor behaviour (manifest values, error mapping, statement semantics, redelivery) stays
  `PROVIDER DEPENDENT`;
- M1 and M2 are `BLOCKED`.

---

## 21. Ledger-finance review

- Reviewer: `ledger-finance` (financial-invariant owner; author of LF-C1/LF-C2, review 19 §4/§6)
- Date: 2026-09-27. Base: HEAD `eb15ac6`, working-tree text of this ADR (sections §0–§20).
- Scope: §4, §6, §7, §12, §14 and §16 per the sign-off list, plus the §17 LF-C2 mapping,
  LF-Q1–LF-Q3, Amendment A7, the migration 0100 backfill and the ADR 0096 payout-gate fit.
  Code read to check the design against reality: `internal/payments/orchestrator.go`
  (`postDepositSuccess`, `receiveDepositCallback`, `receiveDepositReversalCallback`,
  `postDepositReversalTombstone`), `internal/withdrawal/withdrawal.go` (`Complete`, `Fail`,
  `LockApprovedForSubmission`), `internal/httpserver/withdrawal_handlers.go:826-850`,
  `internal/bonus/deposit_sweep.go`, migrations 0025, 0026 and 0082 §1.2.

### Verdict: **SIGN-OFF WITH CONDITIONS**

The architecture is correct and closes the review 19 §6 dual-write hazard. The pattern is
commit-intent → call without a tx → CAS evidence, with a deterministic key, one live attempt,
the payout asymmetry and detection-only reconciliation. `SUM(DEBITS) == SUM(CREDITS)` is never
at risk from this design, because every posting remains one `ledger.Post` in one short tx.

The conditions below fix concrete defects in the ADR **text**, two of which contradict the
schema the ADR itself defines (LF95-C1, LF95-C2). They also close convergence gaps where a real
payment could stay unposted, or a hold could be mishandled, without any P1. **LF95-C1 to
LF95-C12 must be folded into this ADR before it is marked ACCEPTED. LF95-C13 and LF95-C14 gate
PRH-I1/PRH-I5 being labelled `IMPLEMENTED`.** No redesign is needed.

### 21.1 LF-C2 constraints 1–9: is §17 true?

| LF-C2 | Met? | Finding |
|---|---|---|
| #1 intent committed before the call; no tx held | **Met** | T2 (deposit) and T1p (payout) commit before phase B; gate step 2 requires the committed `claim_token`; INV-IO-1 has API-shape, runtime, static and capture enforcement. CP-D1 text is inconsistent with §5.1 (LF95-C12), but that does not affect safety. |
| #2 deterministic persisted key; player retry resumes | **Met for new rows; not for backfilled rows** | INV-IO-3 and §5.1 are correct. Legacy intents and withdrawals were sent with `MerchantReference = intent.ID` / `withdrawal id` (`orchestrator.go:596`, `withdrawal_handlers.go:840`) and never with `pa:<id>`. A backfilled attempt with a fresh id breaks both lookup and T12 safety (LF95-C11). |
| #3 CAS state machine, illegal/backward rejected, audited | **Met, with two defects** | §8 `NotProcessed` → "T5 with `ever_possibly_sent` still true" is forbidden by the §13.1 CHECK and by T5's own guard (LF95-C1). INV-IO-7's trigger check keys on `evidence_kind`, which lives only on the audit row, so the trigger cannot see it (LF95-C2). |
| #4 callback resolves by merchant ref **and** provider ref; accepts `submitting` | **Met, with gaps** | Merchant-reference resolution is not bound to the verified provider. I concur with S95-C1 and extend it in LF95-C3. Success evidence without a provider reference has no ledger key. Deferred receipts do not persist `cascadable`/`decline_stage` (LF95-C4). |
| #5 sweeper `QueryStatus` without tx; no auto-decline or cascade on unknown | **Met, with conditions** | The payout asymmetry (§4.5) is correct. The deposit authoritative-not-found rule needs tightening (LF95-C8). An adapter with no merchant-reference path at all leaves CP-D4/CP-W2 unconvergeable (LF95-C5). |
| #6 cascade through an outbox, never inline in a webhook | **Met** | D3, §4.6 and §6.5. `handleDecline → attemptDeposit` and `resolveAmbiguous` are removed from the callback path. Restricting async cascade to non-interactive attempts is correct. |
| #7 no posting across I/O; ADR 0082 order; authoritative read in the same tx | **Met, subject to A7 conditions** | The per-path orders in §14 are correct. The sweeper batch claim and receipt-row updates are not yet placed in the order (LF95-C9). |
| #8 required tests, each ending with SUM and rebuild | **Mostly met** | INV-IO-13 plus §16. There is no explicit test for "player retry with the same idempotency key while the attempt is in flight → exactly one provider call, one intent". It was listed in LF-C2 #8 (LF95-C14). |
| #9 same rule for payout dispatch | **Met, subject to the KYC gate** | T1p puts the claim and `approved→submitted` in one tx under L1. Staff double-submit gets `ErrStateConflict` before any call. The L1 lock is no longer held across `Withdraw`. The ADR 0096 C5 placement needs explicit text (LF95-C10). |

### 21.2 LF-Q1: T13 posts to `player_cash`, not a suspense account

*[SUPERSEDED for the multiple-success case by §28.7, citing `ledger-finance`'s ruling
`docs/plans/payment-readiness/lf-q1-supersession.md` (`17e5ffc`). T13 as the intent's first
success still posts to `player_cash`. The text below is kept verbatim as the historical
ruling.]*

**Ruling: post to `player_cash`. No new account type.**

A verified, amount- and asset-matching success means the platform received the player's money,
so `psp_clearing` must be debited and the liability is owed to the player. A suspense account
would understate a real player liability and still need a manual release posting. That posting
is BLOCKED (LEDGER-MANUAL-ADJ-4EYES-1), so the funds would be stranded. The refund of an
unwanted second capture is a business process through the normal reversal flow or a four-eyes
adjustment. It is not a reason to mis-state the ledger. This ruling holds only with LF95-C6,
because T13 as drafted collides with three existing mechanisms:

- **The immutable intent link.** `deposit_intents.ledger_transaction_id` is frozen once set
  (migration 0082 §1.2 trigger). A second posting for the same intent cannot be recorded
  there.
- **Reversal resolution.** `receiveDepositReversalCallback` resolves the original deposit
  through the intent row.
- **The tombstone.** A reversal that arrived first leaves `(provider_id, provider_tx_id)`
  occupied by a tombstone (`postDepositReversalTombstone`), so the late T7/T13 `ledger.Post`
  returns an error. Under this ADR that means a tx rollback, a 5xx, and a sweeper that re-polls
  and re-fails for ever.

### 21.3 LF-Q2: Amendment A7

**Accepted, with the placement and scope rules in LF95-C9.** Appending `deposit_intents` then
`payment_attempts` after the existing L1 tables is consistent with every path in the ADR:

- deposit evidence: intent → attempt → (L2) → L3 → L4;
- payout evidence: `withdrawal_requests` → `payment_attempts` → L3 → L4.

I checked that no current path holds either new table's lock and then takes an earlier L1 table.
`internal/bonus/deposit_sweep.go` joins `deposit_intents` with plain reads, and deposit posting
calls no bonus code.

"R0" is accepted as a named step **between L0 and L1**. It is the receipt-key index-insertion
wait. A tx takes at most one, as its first write, and nothing that already holds an L1+ lock
ever inserts a receipt. Its deadlock-freedom depends on exactly that, so the rule is binding.

The A7 text is written into ADR 0082 by `ledger-finance` when this ADR is ACCEPTED, not before.

### 21.4 LF-Q3: the reconciliation-model §2.2(b) amendment

**Accepted.** A bulk statement line is not verified evidence of one payment. Posting from it
would be exactly the "unreviewed path to arbitrary credits" §2.2 warns about. Detection-only
(INV-IO-12, MX9) plus posting only through fresh `QueryStatus` evidence in the state machine
(T17) is the correct split. One condition applies: the §2.2 reconciliation key is the
**ledger's** `(provider_id, provider_tx_id)`, and §12.3 matches statement lines only against
`payment_attempts`. The stream must also join the ledger (LF95-C13). Otherwise a divergence
between an attempt and the ledger (for example a `succeeded` attempt with no posting, or a
`psp_clearing` posting with no attempt) goes undetected. `ledger-finance` edits
`reconciliation-model.md` when the ADR is ACCEPTED.

### 21.5 Adversarial check

| Hazard | Result |
|---|---|
| Double credit | Safe. Guards: the attempt CAS (`succeeded` is terminal for T7), `(tenant, provider_id, provider_reference)` uniqueness on attempts, the ledger's `(tenant, provider_id, provider_tx_id)` plus idempotency-key backstops, and receipt dedupe. Residual: T13 is two real captures, by design (§21.2). Two defects, fixed by LF95-C3 and LF95-C6: a cross-provider merchant-reference callback (S95-C1), and a `created` cascade sibling that is still driven after T13. |
| Double debit / double payout | Safe on the platform side. Guards: T1p under L1 plus `UNIQUE(withdrawal_request_id)`, T12 only with the same key and only when the provider dedupes, a payout is never re-routed, and a payout never fails without definite decline evidence. Two conditions: backfilled legacy attempts must never T12, because the provider never saw their key (LF95-C11), and a `NotProcessed` payout resend must go through T12 only (LF95-C1). Residual: T14 (a contradictory provider), which is detected and P1. |
| Lost successful payment | Converges through a callback, a poll or reconciliation plus T17, with **one gap**: no merchant-reference echo and no merchant-reference status query, plus a crash between provider accept and phase C, leaves a deferred success receipt that is never applied and an attempt that can never be polled. LF95-C5 closes this. A second path: a tombstone collision loops for ever (LF95-C6). |
| Lost callback | Safe. The receipt is committed in the same tx as its effect, and a failure returns 5xx. `deferred_unresolved` returns 200 only after a durable receipt. Condition: the receipt must hold every field needed to apply it later (LF95-C4), and old deferred receipts must alert (LF95-C5). |
| Unsafe retry | Safe for new rows (INV-IO-9, T5 guard, T12 gating). Unsafe for backfilled rows (LF95-C11) and for the `NotProcessed` wording (LF95-C1). |
| Ambiguous treated as success | Safe. INV-IO-6: `applyEvidence` is the only writer. Without `SyncSuccessPossible`, a sync success is treated as `Ambiguous`. Mismatches go to T10. |
| Duplicate callback idempotency | Safe. The fingerprint is canonical rather than raw bytes, and a duplicate still applies idempotently. Condition: a mismatched success (T10) must be **committed**. Today's typed 409 path rolls back and leaves the attempt live. §6.2's "keep reviewed codes" must not mean "roll back the state effect" (LF95-C3). |
| Callback before or after the internal transition | Safe. Phase C's CAS fails harmlessly after a callback applied, and deferred receipts are applied at T4/T9 in the same tx. Condition: LF95-C4. |
| Timeout ≠ failure where the provider may have accepted | Safe. A timeout after dispatch is `Ambiguous` and a settlement timeout is T16 only. A deposit `not_found` decline must not apply to an attempt the provider once acknowledged (LF95-C8). |
| Payout failed only on a definite decline (hold release) | Correct in design. It needs a trigger-visible evidence column (LF95-C2). No KYC outcome, operator action or timeout may release a possibly-dispatched hold (LF95-C10). M2 stays BLOCKED on HD-0095-1. M3 is safe only because `created` guarantees the payout was never sent (INV-IO-9). |
| Reconciliation detects divergence and never writes the ledger | Never writes: correct (INV-IO-12, MX9, statement-capture test). Detects: incomplete without the ledger join (LF95-C13). |
| Deposit "authoritative not-found" decline and late success | Accepted as money-safe for the platform, because a late success still posts via T13. Conditions in LF95-C8. |
| Backfill (0100) | Defective as drafted (LF95-C11): intents in progress without a reference are skipped, legacy merchant references are lost, T12 is possible on keys that were never sent, `reversed` withdrawals are omitted, and the payout `payment_method` does not exist on `withdrawal_requests`. |
| The 14 crash points | CP-D2–D8 and CP-W1–W6 converge as stated, subject to LF95-C5 for CP-D4/CP-W2 without echo. CP-W1 is correctly never `Fail`: a `submitting` attempt with `ever_possibly_sent=false` is **not** proof it was not sent, because the flag is only written in phase C. CP-D1 contradicts §5.1 (LF95-C12). |
| ADR 0096 payout KYC gate | Fits: T1p is "the last commit before the first `Withdraw`". Re-gating the sweeper T2 re-claim, gating T12, and the no-release-after-possible-dispatch rule must be written into this ADR (LF95-C10). Checked against the working-tree ADR 0096 (revision in progress). Re-check once that revision lands. |

### 21.6 Conditions

**LF95-C1 (ADR defect: §8 `NotProcessed` contradicts INV-IO-9).** "Treated as T5, but
`ever_possibly_sent` stays true" violates `CHECK (state <> 'created' OR NOT ever_possibly_sent)`
and T5's own guard. Replace it with the following:

- If the vendor documents that the request left no trace, the outcome is `NotSent` and takes T5.
- Otherwise the outcome takes T6 (`ambiguous`, `ever_possibly_sent=true`, `next_action_at=now()`).
  A resend is **only** T12, which requires `IdempotentSubmission`, for deposits **and** payouts.
  Delete "Yes for deposits".
- A `NotSent` result on a T12 resend (`ever_possibly_sent` already true) returns to `ambiguous`
  via T6. It never takes T5.

Add `submitting→ambiguous` on NotSent-after-resend explicitly to §4.3.

**LF95-C2 (ADR defect: INV-IO-7 is not trigger-enforceable).** Add
`last_evidence_kind TEXT NOT NULL` to `payment_attempts`, set in the same UPDATE as every state
change. The guard trigger enforces these rules on it:

- `*→declined` for `operation='payout'` requires `last_evidence_kind ∈ {sync, callback,
  query_status}`;
- any `→declined` for a deposit requires `≠ operator`;
- `→succeeded` requires `∈ {sync, callback, query_status}`.

The audit record carries the same value.

**LF95-C3 (resolution binding; concur with S95-C1 and extend it).**

- Merchant-reference resolution matches only `attempt.provider_id = verified provider_id`, as
  S95-C1 requires.
- If the provider reference and the merchant reference are both present and resolve to
  **different** attempts, or if the evidence's provider reference is already bound to another
  attempt, the outcome is an `anomaly` receipt plus P1, with no state change and no posting.
  This must be decided **before** any write, so it can never surface as a unique violation
  followed by a 5xx redelivery loop.
- A mismatched success (T10) is **committed** in the domain tx together with its receipt,
  whatever HTTP code §6.2 returns. The state effect is never rolled back to produce a 409.

**LF95-C4 (evidence completeness).**

- T7/T13 require a non-empty `provider_reference` in the evidence, or an already-set
  `attempt.provider_reference`. That value is the deposit ledger `provider_tx_id`. Success
  evidence without one is treated as `ambiguous` plus a poll.
- `payment_provider_events` must persist every field `applyEvidence` consumes:
  - `cascadable`;
  - `decline_stage`;
  - `decline_reason` (bounded).
- Add `CHECK (outcome <> 'succeeded' OR (amount IS NOT NULL AND asset_code IS NOT NULL))`.
- A deferred receipt that lacks a required field can never be applied as success or as a
  cascade.

**LF95-C5 (no unconvergeable success).**

- The manifest gains `CallbackEchoesMerchantReference`.
- Registration refuses a production-eligible adapter supporting deposit or payout unless it has
  `CallbackEchoesMerchantReference = true` **or** `StatusQuery = by_provider_or_merchant_reference`.
- Verified deferred receipts still unapplied after the resolution horizon raise a P1. The §12
  stream reports them as a platform-side kind (reuse `pay_unresolved`, or add one) so a lost
  success is never silent.

**LF95-C6 (T13 and posting integrity; conditions of the LF-Q1 ruling).** *[(a) and (b) stand
as defence in depth; (c)'s "sibling already `submitting`" residual is WITHDRAWN by §28.10; (d)
stands and takes precedence over the INV-DEP-1 check (§28.4).]*

- (a) Each deposit posting is linked on `payment_attempts.ledger_transaction_id`. The intent's
  `ledger_transaction_id` is written only while it is NULL; the 0082 trigger already forbids
  repointing it.
- (b) §5.4 reversal resolution uses `payment_attempts (provider_id, provider_reference)` →
  **the attempt's** `ledger_transaction_id`, never the intent's. Otherwise a reversal of the
  second capture reverses the first.
- (c) The T13 tx, holding intent → attempt locks, also moves any sibling `created` attempt to
  `rejected` (T3, `intent_succeeded`). T2's CAS predicate adds `NOT EXISTS (succeeded attempt
  for the same intent)`. The race in which a sibling is already `submitting` is a stated
  residual under the same P1.
- (d) If T7/T13 finds a tombstone on `(provider_id, provider_reference)`, the attempt moves
  terminally to `disputed` (`reversal_tombstone_precedes_success`, P1), with no posting and no
  error. It is never a rollback/5xx/re-poll loop. Add this cell to §4.4.

**LF95-C7 (intent projection).** When an intent has a `disputed` attempt and no `succeeded`
attempt, its status is `ambiguous`, never `declined`. A `declined` intent invites the player to
retry while funds may have been captured.

**LF95-C8 (authoritative not-found, §4.5).** A deposit may take T8 on `not_found` only when all
of these hold:

- (a) `accepted_at IS NULL AND provider_reference IS NULL`. The provider never acknowledged the
  attempt. Otherwise `not_found` means T11/`ambiguous`.
- (b) Δ is measured from the **latest** send. T12 must update a `last_sent_at`.
- (c) Registration refuses `MerchantLookupAuthoritativeAfter < lease + CallTimeout`, so a lookup
  cannot outrun a call still in flight.
- (d) The resulting decline carries `cascadable=false`. This is enforced in code, not only
  stated in §4.6.

A late success after that decline is T13 (posted plus P1). Covered by §16.2 item 9.

**LF95-C9 (A7 scope rules, binding for acceptance).**

- (a) R0 sits between L0 and L1. A tx takes at most one, as its first write, only in a callback
  tx or in a separate-tx rejection/`unsupported_event` record.
- (b) Receipt one-shot UPDATEs (`attempt_id`, `applied_at`) happen only while the tx holds the
  attempt's parent **and** attempt locks, in ascending receipt id. The sweeper backstop locks the
  parent and the attempt first.
- (c) Every tx that locks a `payment_attempts` row locks its parent (`deposit_intents` or
  `withdrawal_requests`) first, including T3, T5 and per-item phase C.
- (d) The §7.2 batch claim tx is the sole exception. It uses `FOR UPDATE SKIP LOCKED` only,
  writes only lease columns and T2, never locks or writes a parent row, and never waits on a
  lock. This works because the lease is ordered by `next_action_at`, not by id, and it is safe
  only because nothing in the claim tx blocks.
- (e) The §16.2 item 16 harness adds the sweeper claim racing a callback and phase C on the same
  intent and the same withdrawal.

**LF95-C10 (ADR 0096 payout KYC gate fit; my ADR 0096 C4/C5, binding here).**

- (a) §5.2 phase A order: approver eligibility → L1 `withdrawal_requests` → **KYC gate** →
  kill-switch predicate → T1p. On deny, `DenyForCompliance` commits from `approved` (a hold
  reversal via the `Reject` shape, `kyc_denied`), no attempt is created and no call is made.
  List it in §4.3/§4.7 as a pre-dispatch withdrawal transition.
- (b) The sweeper's T2 re-claim of a payout attempt in `created` (after T5) **re-runs the gate**
  in the claim path. On a non-pass there is no claim and no call, and the attempt stays
  `created`, escalated (T16-style) to a compliance case. The hold is released **only** by M3
  with reason `kyc_denied`. This is safe only because of INV-IO-9, and it is never automated.
  Because of LF95-C9(d), the gate read runs in a per-item tx that locks the parent first, not in
  the batch claim.
- (c) T12 for a payout re-runs the gate. On a non-pass there is no resend and the attempt stays
  `ambiguous`, resolvable only by poll or callback.
- (d) Once T1p has committed, no KYC outcome ever triggers `Fail`, a hold reversal or any
  automated release. Only T8 evidence or M3-from-`created` can.
- (e) A deposit T2 re-claim (player resume or sweeper) re-runs the RG and KYC deposit gates
  (ADR 0096 C4(b)).
- (f) Re-verify these rules against ADR 0096's revised text once that revision lands.

**LF95-C11 (migration 0100 backfill).**

- (a) Create an attempt for **every** intent whose status is non-terminal (`pending`,
  `ambiguous`), whether or not it has a `provider_reference`. With no reference, the attempt
  state is `ambiguous`. If `provider_id` is NULL on a non-terminal intent, the pre-flight aborts
  and lists the ids.
- (b) The backfilled attempt's `id` is the parent's id (intent id or withdrawal id), so
  `merchant_reference = id::text` equals the value actually sent (INV-IO-3 holds).
- (c) Add a `legacy_backfill BOOLEAN` column. T12 is forbidden when it is true (by trigger),
  because the provider never received `pa:<id>`.
- (d) Withdrawal mapping is explicit:
  - `submitted` → `pending` (the reference is already set);
  - `completed` → `succeeded`;
  - `failed` → `declined` with `last_evidence_kind='sync'` marked legacy;
  - `reversed` → `succeeded`. Include `reversed`; it was omitted.
- (e) `withdrawal_requests` has no `payment_method`. Backfilled payouts take an explicit
  `'legacy_unknown'` sentinel (or a column nullable only when `legacy_backfill`); nothing is
  invented.
- (f) Intents with status `failed` map to `declined`. An intent in `succeeded` with a NULL
  `ledger_transaction_id` aborts the pre-flight. `ledger_transaction_id` is copied.
- (g) The backfill writes no ledger row. A post-check asserts SUM equality and
  projection == rebuild unchanged, and that every non-terminal intent or `submitted` withdrawal
  has exactly one live attempt.

This backs QA's §24 item 2.

**LF95-C12 (text consistency).** CP-D1 says "attempt `created`" after phase A, but §5.1 puts T1
and T2 in the same phase A tx. Either split T2 into its own short tx (then CP-D1 stands) or
restate CP-D1 as `submitting` with the CP-D2 outcome.

**LF95-C13 (reconciliation ledger join; PRH-I5).** The match tx also checks the following, all
detection-only:

- every `succeeded` attempt has exactly one ledger transaction whose `(provider_id,
  provider_tx_id)` is the deposit's `provider_reference`, or the payout's Step B settlement
  reference;
- every `deposit`/`withdrawal_completed` ledger transaction in the window maps to exactly one
  `succeeded` attempt;
- payout statement lines match on either the instruction reference or the settlement reference.

Any miss is a P1 mismatch.

**LF95-C14 (tests; PRH-I1-i additions, each ending with INV-IO-13).**

- Player retry with the same key, concurrent with an in-flight `submitting` attempt: one
  intent, and a MOCK call count of 1.
- T13 with a `created` sibling: the sibling is rejected and never called.
- A tombstone before success leads to `disputed` with no loop.
- A deferred **decline** receipt applied at T4 cascades per the persisted `cascadable`.
- A disputed attempt leaves the intent `ambiguous`.
- The LF95-C3 cross-attempt reference conflict gives `anomaly` with no 5xx.
- The LF95-C10 (a)–(c) KYC cases.
- The LF95-C11 fixtures: legacy T12 refused by trigger, legacy merchant-reference callback
  resolves, a non-terminal intent without a reference is backfilled.
- A mutation removing the LF95-C2 trigger check must fail item 17.

**Recommendations (non-blocking).**

- **LF95-R1.** A `payments`-owned job, **not** the reconciliation stream, sets
  `next_action_at=now()` (a T17 equivalent) on attempts named by a new `pay_status_mismatch`,
  so a dropped success converges without waiting for an operator. Reconciliation itself still
  writes nothing.
- **LF95-R2.** `bonus-engine` should confirm that a T13 second capture is intentionally not
  bonus-eligible. `deposit_sweep.go` finds deposits via `deposit_intents.ledger_transaction_id`,
  which LF95-C6(a) keeps pointing at the first posting. *[MOOT after §28.]*
- **LF95-R3.** The §9.2 `Amount int64` carries forward the existing platform-wide int64 amount
  path (the same as `ledger.EntryInput`). This is not new here, but it is noted for any
  18-exponent asset routed through `PaymentProvider`. Custody-asset precision stays with ADR 0008
  and its open decisions.

Custody, settlement-timing and HD-0095-1 matters are not ruled on here. M1/M2 remain BLOCKED.
Everything in this ADR remains `NOT IMPLEMENTED`.

---

## 22. Security review

- **Reviewer:** `security`, 2026-09-27, against HEAD `eb15ac6` (design review of this PROPOSED
  ADR; no code exists to review).
- **Verdict: APPROVE WITH CONDITIONS.** The D1 boundary, INV-IO-1/-2/-11, the refusal to treat
  a statement line as posting authority (§12.6), and "success only on verified, matching
  evidence" (INV-IO-6) are sound and are the right shape. The ADR may move to ACCEPTED on the
  security side once the conditions below are written into it (text only). **S95-C1 is a
  design defect with a concrete credit-without-funds scenario; PRH-I1 must not start without
  it.** Every condition is also an implementation-review item for PRH-I1/I2/I5.
- **In scope:** §6 (callbacks, receipts, dispositions), §9 (contract, `CallContext`), §10
  (manifest, kill switch), §11 (PROV-OUTBOUND-CRED-1), §12 fetch/ingest, §13 table shapes as
  they bear on isolation, secrets and PII, the new and changed staff routes, and the ruling on
  LF-C1 option (a).
- **Not in scope:** ledger correctness of T13/A7 (`ledger-finance`), sweeper tuning
  (`payments`), any real vendor behaviour, ADR 0097's own mechanisms (reviewed there), and any
  penetration test. Passing this review does not make the implementation secure; PRH-I1, I2
  and I5 each need their own `security` code review, and the outbound tripwire stays until
  then (§11 "Tripwire", agreed).

### 22.1 S-Q1 — kill-switch authority: RULED

**Engage: single actor with a mandatory reason code. Release: four-eyes, as proposed. Not
relaxed.** Engage is the money-safe direction (it only stops *new* submissions and never stops
settlement, §10.3), so it must never wait for a second person; that mirrors ADR 0093 §3.
Release re-opens outbound money movement, often after a credential compromise or a
misbehaving provider. A single compromised or coerced staff account must not be able to undo
an incident containment. The operational cost (a second person to resume after an outage) is
accepted.

Conditions: S95-C5, S95-C6, S95-C7.

### 22.2 S-Q2 — `deferred_unresolved` → 200 after a durable receipt: ACCEPTABLE, with conditions

**Oracle.** No new oracle. The disposition is reached only after `VerifyCallback`, the
post-verification re-check and PROVIDER-REF-BOUND-1 validation, so an unauthenticated caller
still sees only ADR 0091's uniform 401 or ADR 0097's pre-verification 429/503. For a
*verified* sender, the change removes an oracle that exists today: 404 says "this reference is
unknown to the platform"; a uniform 200 does not. This holds only if all 200 dispositions are
byte-identical (S95-C4).

**Why a 200 is correct.** A 404 may be terminal for a vendor, which is silent loss of a real
payment event. ADR 0097 §6.1 forbids a 2xx for an *unprocessed* event; a durably receipted,
deferred event is processed (INV-IO-10), so there is no conflict.

**Replay.** An exact redelivery collapses on `UNIQUE (tenant_id, provider_id,
event_fingerprint)`, and then applies idempotently. Freshness (timestamp windows, nonces)
stays with ADR 0091 and the adapter scheme. The fingerprint is not a replay control by
itself: its protection lasts only as long as the row. So PAY-ATTEMPT-RETENTION-1 must not
purge receipts of non-terminal attempts, or any receipt younger than the longest vendor
redelivery window (S95-C3).

**Abuse by a verified sender.** This means a compromised provider credential, a hostile
provider, or a buggy vendor. Today, an unresolvable callback writes nothing. After this ADR,
each distinct fingerprint writes one bounded row, and it *stays pending* until something
claims its reference. Two concrete risks follow:

- **Storage growth.** The size of each row is bounded by PROVIDER-REF-BOUND-1 and the §13.1
  CHECKs. The row *count* is bounded only by ADR 0097 B1's rate multiplied by time, because
  retention is deferred. B1 limits the rate; it does not limit the backlog. → S95-C2.
- **Pre-planting.** A sender can store a `succeeded` receipt for a `provider_reference` it
  expects to be assigned to a future attempt. T4 applies it in the same transaction that
  learns the reference. That gives no more power than the same sender posting after T4 (it is
  the same trust principal, and amount and asset must still match). However, a receipt that
  *predates the submission it claims to describe* cannot be legitimate. Refusing to apply it
  costs nothing and closes the reference-recycling case. → S95-C3.

**Interaction with ADR 0097.** Correct as specified: B1/B2 run on the `VerifiedCallback`
before the domain transaction, so a receipt is never written for a limited request (ORD-3). A
deferred 200 is still counted by B1 like any admitted request.

**Interaction with PROVIDER-REF-BOUND-1.**
- Oversized references are rejected at step 3, before the receipt insert. That is
  deterministic, non-retryable, and writes no row.
- The §13.1 CHECKs are the backstop. If a CHECK violation is ever reached (the application
  bound and the DB bound disagree), it surfaces as a 5xx. That fails safe (redelivery, no
  silent loss), but it must alert, because it means the adapter validation has drifted.
  → S95-C2.

### 22.3 S-Q3 — LF-C1 option (a): RULED — APPROVED, narrowly, additive to (b)

A DB error during the **post-verification** re-check (`HandleRecheckSQL` inside
`ReceiveVerifiedCallback`, ADR 0094 §5) may return a **retryable 503**, instead of today's
uniform 401. The constraints are:

1. **Only a transport or DB failure qualifies:** a connection error, a serialization or
   statement failure, or a context deadline at the re-check. A *definitive* result, meaning
   the handle is not found, not active, revoked, expired, or its fingerprint or key id does
   not match, stays the uniform 401 with `credential_unavailable`, exactly as today. The
   classification is typed: a new `RecheckUnavailableError`, distinct from `AuthError`. It
   is never derived from error strings. Anything unclassified defaults to **401**. That keeps
   the fail-closed direction: an unknown error is never a retry invitation that could mask a
   revocation.
2. **No oracle.** The re-check runs only after a valid signature, so the 401/503 split is
   visible only to a verified sender, and it reveals only "our DB was unhealthy". It is
   independent of signature validity; ADR 0091 T9 is unaffected. The 503 body and headers
   are ADR 0097 §6.1's generic 503 (with `Retry-After`). There is no reason text.
3. **Nothing is written.** The domain transaction rolls back: no receipt, ledger entry,
   tombstone, audit row or casino rejection record. ADR 0094 review item 6's test
   (`TestReceiveVerified_RecheckDBErrorRollsBack`) is extended to assert the 503 as well as
   the zero rows.
4. **Bounded and observable.** A metric plus an alert on the re-check-unavailable rate per
   (domain, tenant, provider). A persistent non-transient DB fault (for example a broken
   grant or RLS policy) would otherwise turn into indefinite vendor retries.
5. **Additive, not a replacement.** The §6.6 startup rule, option (b), still applies to every
   production-eligible adapter whose `RedeliveryOn401` is `terminal` or `unknown`: a
   revocation 401 is still terminal for such a vendor.
6. **Scope.**
   - It applies to all three webhook domains, through the shared `ReceiveVerifiedCallback`
     error typing: payments in PRH-I1, and casino and KYC in PRH-I2.
   - It does **not** extend to *pre-verification* DB errors, which stay uniform 401. That
     extension would need its own review against ADR 0091's uniformity tests, and it is not
     approved here.

### 22.4 Findings and conditions

| # | Sev. (once reachable) | Finding / failure scenario | Condition |
|---|---|---|---|
| **S95-C1** | **High** (design defect; blocks PRH-I1 start) | §6.1 step 5(b) resolves an attempt by `(tenant, merchant_reference)` without binding it to the **verified provider**. Tenant T routes a player's deposit to provider B. The player abandons the redirect, so no funds move. Provider A's verified credential for T (compromised, or a hostile PSP) then sends `succeeded` with that attempt's merchant reference and the player-chosen amount and asset. T7 credits `player_cash` with no money received. The merchant reference is an attempt UUID and can reach the player via the redirect, so it is not a secret. In the same way, a callback naming a `created`/`rejected` attempt of another provider triggers T15, and any provider of the tenant can then push other providers' attempts into `disputed` (a P1 flood and a denial of service on deposits). | Merchant-reference resolution matches only `attempt.provider_id = VerifiedCallback.provider_id`. The provider id always comes from the verified identity, never the payload. A merchant reference that matches an attempt with a different or NULL `provider_id` changes **no** state. It is receipted as `anomaly` with an alert, and T15 is **not** taken. T15 applies only when `attempt.provider_id` equals the verified provider. The same binding applies to §6.4's deferred application, the sweeper backstop and §12 matching (a statement line resolves only attempts of its own provider). Test: a cross-provider, same-tenant merchant-reference callback yields no posting and no state change. Mutation: dropping the predicate must fail that test. |
| S95-C2 | Medium | Receipt backlog is unbounded under a verified sender (§22.2), and a reached CHECK is silent drift. | (i) A per-(tenant, provider) cap on **unapplied** receipts, checked in the callback transaction with a bounded probe (`LIMIT cap+1` on the partial index; `RECOMMENDATION` 10 000). Above the cap, a `deferred_unresolved` event returns a retryable **503** plus a P1 alert. It never returns a 200 without storing, and never a 404. (ii) A deferred receipt that no attempt has claimed after the manifest `SettlementWindow` (24 h if undeclared) is surfaced as a P1 alert/metric ("unmatched verified callback"). It never creates an attempt (§6.5 agreed). (iii) The application-level bounds equal the §13.1 CHECKs (one constant per field). A CHECK violation reached at insert maps to 5xx plus an alert. |
| S95-C3 | Medium | A pre-planted or stale receipt is applied to a later attempt that reuses a reference, and retention could weaken replay suppression. | Deferred application (T4/T9 and the sweeper backstop) applies a receipt only if `received_at >= attempt.submitted_at` of the **first** claim. Both values come from the DB clock. An earlier receipt is marked `anomaly`, not applied, and alerts. PAY-ATTEMPT-RETENTION-1 must not delete receipts of non-terminal attempts, or receipts younger than the longest declared vendor redelivery window. `security` reviews that design. |
| S95-C4 | Low | Response-body variance between `applied`, `duplicate_effect`, `deferred_unresolved` and `anomaly` would re-create the reference-existence oracle for a verified sender. | All 200 dispositions return an identical body, apart from the request id. The disposition is recorded only in the receipt, audit and metrics. Test: byte comparison across the four dispositions. |
| S95-C5 | Medium | Kill-switch release bypass. Nothing in §13.2 prevents a single actor from releasing via `UPDATE … SET engaged=false`, or by `DELETE` (missing row = not engaged, §10.3). Either one silently defeats the four-eyes release. | A DB trigger on `payment_kill_switches` enforces three things. (1) `DELETE` is rejected. (2) `engaged` true→false is allowed only in the same transaction that moves a `payment_kill_switch_release_requests` row `open→approved`, with `expected_version = OLD.version` and an approver distinct from the requester, following the ADR 0093 A1 binding shape. (3) `version` is strictly monotonic. The release request is one-shot and has an expiry (`RECOMMENDATION` 24 h). A re-engage between request and approval invalidates the request through the version. Tenant policies on both tables require `app.player_account_id` unset (the ADR 0093 A1 pattern). |
| S95-C6 | Medium | The "missing row = not engaged" encoding fails open if the switch is read under the wrong or unset tenant context (FORCE RLS returns zero rows). | The switch predicate is evaluated **inside** the T2/T1p CAS statement, as a `NOT EXISTS` subquery with an explicit `tenant_id = payment_attempts.tenant_id`, both under the same RLS context. A misbound context then claims zero attempts, and the claim can never succeed with the switch skipped. Test: running the claim under another tenant's context claims nothing and calls nothing. A switch-read error aborts the claim (agreed, §10.3). |
| S95-C7 | Medium | Tenant-scoped release of a platform-engaged switch. Under the hybrid model a B2B operator's staff could release a switch that platform security engaged (for example after a credential compromise). | Record `engaged_by_scope ∈ {platform, tenant}`. A switch engaged by a platform principal (interim PROV-REVOKE-ALL-1 form, via the existing `WithPlatformAdmin` pattern with audit) can be released only by platform principals, requester and approver both. Every engage emits an alert, not only an audit row. |
| S95-C8 | Medium | PROV-OUTBOUND-CRED-1 leak paths not covered by §11. (a) A Go `*url.Error` from the transport embeds the full request URL, and vendors that take an API key in the query string would leak it into logs and errors. (b) The adapter-side binding check is per-adapter discipline. (c) The reflection test is type- and name-based and misses closures, interfaces, maps, package-level variables and unexported nested values. | (a) The gate maps transport errors to `ErrorClass` plus an allow-listed reason. Where any URL is logged it is logged without its query string and userinfo. A test uses a secret-in-query MOCK and asserts the secret is absent from logs, errors, audit and receipts. (b) The **gate** checks `Credential.TenantID/ProviderID/Domain` against the `CallContext` before calling the adapter, as well as the adapter's own check (§9.1), so neither alone is load-bearing. The domain is `payments`, `casino` or `kyc` per caller. (c) The reflection test walks **constructed** registered adapter values recursively (pointers, structs, slices, maps, interfaces and unexported fields). It fails on any `OutboundCredential`, `secretstore.Secret`, `httpclient.Authenticator` or derived token. It forbids func-typed fields unless allow-listed. A static test forbids package-level variables of those types in adapter packages, and forbids adapter packages importing `secretstore` or the Fetcher. `CallContext` has its own `String/GoString/Format/LogValue/MarshalJSON` that render only the credential's existing redacted form. |
| S95-C9 | Low | Credential resolution timing and scope. | Agreed as specified: `Resolve` runs in phase B only; its own short transaction commits before the fetch; it is refused under `txscope.Held`; there is no fallback credential and no unauthenticated call; an outage is `NotSent` with no breaker count and no cascade; derived tokens are cached only in `DerivedTokenCache` after this call's handle read; the in-flight exposure is one call. Additions: (i) the `CallContext` tenant is taken only from the attempt row returned by *this* tenant's claim transaction, never from a payload, cache or earlier attempt; (ii) the sweeper runs under the ordinary app role with RLS, with no BYPASSRLS, and its audit actor is a named system principal; (iii) an alert fires on the `ErrOutboundCredentialUnavailable` rate per (tenant, provider). |
| S95-C10 | Low | Receipt and attempt text fields could carry vendor free text. That text may echo PII or cardholder fragments into `decline_reason`/`terminal_reason`, which would pull PCI or GDPR data into our store. | `decline_reason` and `terminal_reason` hold canonical, allow-listed codes, never vendor text. The adapter conformance suite asserts that no payer-identifying vendor field (name, email, IBAN, PAN or masked PAN, wallet address) maps into `CallbackEvent`, `StatusResult` or a statement line. The receipt contents in §13.1 are otherwise accepted: references, outcome, amount, asset and fingerprint; no raw body, headers, signature, key id or credential; bounded; FORCE RLS; excluded from CDC. The vendor-intake item "the reference format contains no cardholder data" is added to the §6 intake checklist. |
| S95-C11 | Low | §9.5/§13.3 statement ingestion is unbounded. `merchant_reference`, `original_provider_reference`, `asset_code` and `source_label` have no length CHECK. There is no line-count or body cap, so a faulty or hostile source can exhaust memory or storage. `CallContextLike` is looser than `CallContext`. | CHECKs mirror §13.1 (`merchant_reference ≤ 64`, `original_provider_reference ≤ PROVIDER_REF_MAX`, bounded `asset_code`/`source_label`). The source response body is capped, and a per-import `line_count` cap applies (`RECOMMENDATION` 1 000 000; above it the import is refused and a P1 alert fires). `Fetch` takes the real `CallContext`, with the credential resolved per §11 and the same gate. The §12 fetch-outside-tx design and the "detection only" rule (INV-IO-12, §12.6) are agreed. |
| S95-C12 | Low | Capability manifest fail-closed. | Agreed as specified: code-declared, never tenant-editable, refused at registration and new activity, never at settlement. Additions: (i) unknown enum values and a missing manifest on a non-MOCK adapter refuse registration; (ii) each `NotProcessed` code cites its vendor-documentation source in the intake; registration refuses `NotProcessed` for payouts without `IdempotentSubmission`; (iii) ADR 0097 §6.3's interim per-provider config key for `WebhookRetrySemantics` moves into this code manifest in PRH-I1, so no retry-safety property stays config-editable. |
| S95-C13 | Medium | New and changed staff routes: kill switch engage, release request and release approve (plus list), T17 re-verify, M3 abandon, the rewritten withdrawal submit and resolve, and any attempt or receipt read. | Each route is on the staff admin API only; the route-table test asserts none is reachable with a player principal or on a player or public route. Permissions are enforced server-side and tenant-scoped from the authenticated context, with no tenant id taken from the path or body: `payments_kill_switch:engage`/`:release`, `payments_attempt:reverify`, `payments_attempt:read`, and M3 with today's resolve eligibility plus a reason code. Object lookups run under RLS, and another tenant's id returns 404, never data (test for each route). Every mutation writes audit (actor, tenant, entity, before/after, IP, UA, reason code). T17 only sets `next_action_at` and never calls a provider inline, so staff cannot use it to bypass the sweeper caps (agreed). The M3 CAS requires `state='created' AND NOT ever_possibly_sent` in SQL and in the trigger. |

### 22.5 Required authorization and tenant-isolation tests (for `qa`, added to §16)

1. A tenant A staff token against each new route with tenant B's switch, attempt or
   withdrawal id returns 404 or 403 and never data, and B's rows are unchanged.
2. A player token gets 401 or 403 on every new route (the route-table test, plus one live
   request per route).
3. The release requester approving their own request is refused by the application **and** by
   the CHECK. A direct `UPDATE engaged=false` or `DELETE` is refused by the trigger (S95-C5).
4. A platform-engaged switch cannot be released by tenant principals (S95-C7).
5. Cross-provider merchant-reference callbacks give no posting and no state change (S95-C1).
   Cross-tenant callbacks are impossible by construction (a verified tenant), and a test pins
   it.
6. The claim under a misbound tenant context claims nothing (S95-C6).
7. The secret-in-query leak test and the recursive adapter reflection test (S95-C8).
8. The re-check DB error gives a 503 with zero rows, and a re-check revoked result gives a 401
   with zero rows (S-Q3).
9. The deferred-receipt cap gives a 503 above the cap, and a pre-submission receipt is not
   applied (S95-C2/C3).
10. The four 200 bodies are identical (S95-C4).

### 22.6 Launch relevance

S95-C1, C5, C6, C8 and C13 are **launch-blocking for the first non-MOCK payments adapter**,
together with the existing PROV-OUTBOUND-CRED-1 and F-POOL-2 rows. The rest must be closed in
PRH-I1/I2/I5 before their respective `security` code reviews. None of this is a claim that the
platform is secure or launch-ready; production launch authorization remains the human's
decision.

---

## 23. Payments review

**Verdict: APPROVE WITH CONDITIONS**

Reviewed as `payments` owner, scope §4–§12 and §18 per the ADR's own sign-off list, plus P-Q1
(§19.2) and the ADR 0097/architect `WebhookRetrySemantics` cross-review item.

**P-Q1 (§19.2).**

- **§7.3 defaults.** Accepted as reversible engineering `RECOMMENDATION` values (15 s tick, 20
  batch, 16 global / 4 per-(tenant, provider) concurrency, 60 s lease, 30 s/30 min backoff, 24 h
  horizon, 30 min presence window, 3 resubmits). `MaxCascadeDepth = 3` is unchanged from today's
  running value. No objection; these should be re-measured against real traffic once a
  non-MOCK adapter exists, exactly as the ADR already says for the burst/rate numbers elsewhere.
- **Does an existing adapter test rely on today's inline webhook cascade? Yes.**
  `TestMigration0082_DepositIntentsProviderColumnsStayMutable`
  (`internal/payments/migration_0082_immutability_integration_test.go:111`) exists specifically
  to pin `deposit_intents.provider_id`/`provider_reference` as mutable in place, because
  `handleDecline → attemptDeposit → setIntentAttempt` today overwrites them on the *same* intent
  row on cascade. §5.1 keeps `deposit_intents` as an unchanged mirror that "keep[s] mirroring
  the latest routed attempt for existing readers," so this test should keep passing once cascade
  authority moves to `payment_attempts` — but that must be proven, not assumed (P95-C1).

**The three behaviour changes — all accepted:**

1. **Async cascade removed for interactive/redirect methods (§4.6).** Correct and overdue. An
   asynchronous decline of an interactive attempt cascading to a new provider is a latent defect
   today: the new redirect URL can never reach a player who has already left the page. Confining
   post-decline cascade continuation to non-interactive methods, driven by the sweeper, is a bug
   fix, not a lost capability. The player-retry-with-new-idempotency-key fallback for the
   interactive case is the correct, and only sound, alternative.
2. **Unresolved verified callbacks get 200 after durable receipt, not 404 (§6.2
   `deferred_unresolved`).** Accepted. The 200 is issued only *after* the event passes
   verification and is durably receipted, and the response is identical whether or not the
   reference ever resolves, so it opens no existence oracle. Concur with S-Q2's framing that
   this is a post-verification-only change.
3. **`submitted` withdrawals may lack a `provider_reference` (§4.7).** Accepted; this is the
   direct fix for the double-payout hazard (LF-C2 #9, hazard #5 in §1). `provider_reference`
   moving from "set at submission" to "set once by T4/T9/T7" is the correct fix, but
   `docs/architecture/withdrawal-state-machine.md` lines 92–93 still document `provider_id` and
   `provider_reference` as "set on `approved` -> `submitted`" today. That doc is stale the
   moment this ADR is implemented and is `payments`-owned per §18's "Specialist-owned document
   changes" list; it must be updated no later than I1-f, not left implicit (P95-C2).

**8-state model vs. routing / cascade-on-decline / health-based failover (Blueprint via
`docs/architecture/payment-orchestration.md` §4–§6 and `docs/decisions/0022`) — nothing is
lost:**

- **Routing** (`payment-orchestration.md` §4: tenant/brand → jurisdiction → currency/asset →
  method → amount → health) is unchanged in substance. §9.6's split of `RouteProvider` into
  `ListRoutingCandidates(tx)` (in-tx) and `Rank(candidates, health)` (no-tx) only relocates the
  health read outside the transaction; the resolution order and dimensions are untouched.
- **Cascade-on-decline** (`payment-orchestration.md` §5) is preserved and materially improved.
  §5 itself documents today's limitation: a single `deposit_intents` row keeps only the latest
  attempted provider, so `providerExclusionSoFar` can exclude only the most recently tried
  provider once a callback arrives asynchronously. The attempt-per-try model (§4.6, T1 on every
  cascade with `excluded_provider_ids = all previous attempts' providers`) closes that gap
  completely — this is a strict improvement over the documented current behaviour, not a
  reduction.
- **Health-based failover** (`payment-orchestration.md` §6: `ProviderHealth`, `circuit_state`)
  is preserved. §9.6 tightens `HealthStatus` to an in-memory, no-I/O snapshot contract and adds
  an orchestrator-owned breaker per `(tenant, provider)` fed by `ErrorClass` — a strict superset
  of today's in-memory health/circuit design (which this ADR does not turn into a stored table
  either), not a different mechanism.
- The one genuine behaviour reduction is the interactive-cascade restriction already addressed
  above, and it is a correctness fix, not a lost feature.

**Sweeper design (§7).** Sound. Tenant round-robin reusing the existing
`activeTenantIDs` reconciliation-scheduler pattern; a short claim tx using
`FOR UPDATE SKIP LOCKED` plus per-row lease/`lease_owner`, so a crashed worker's items reappear
at `lease_until`; global and per-(tenant, provider) concurrency caps that keep one tenant's slow
provider from starving another (the F-POOL-1 lesson, correctly generalized); exponential backoff
with jitter and a cap; and escalation (T16) at the manifest's `resolution_horizon` with no state
change. §7.4's claim that at most one live money call per attempt exists (entry to `submitting`
is only via CAS T2/T12) is correct and is the actual mechanism that prevents a double
submission, not the lease.

**Provider-neutral contract (§9).** Endorsed, with one gap:

- The closed outcome set (`NotSent`/`NotProcessed`/`DefiniteDecline`/`Ambiguous`, §8) is
  exhaustive and is what makes INV-IO-7 (no payout failure without definite decline evidence)
  enforceable by CAS and trigger rather than by convention — this is the single load-bearing
  design choice in the ADR from a payments-correctness standpoint, and it is right.
- `StatusQuery` by either provider or merchant reference (§5.3, §9.2, manifest field
  `StatusQuery`) is correct and is specifically what makes the withdrawal
  `submitted`-with-no-reference case resolvable.
- The no-I/O `HealthStatus` snapshot plus the platform breaker per `(tenant, provider)` (§9.6)
  is correct and is the direct fix for F-POOL-2's health-check pool-pinning hazard (hazard #4 in
  §1).
- Refunds reserved, `NOT IMPLEMENTED` (§5.5): correctly scoped. No flow initiates a refund
  today; refusing an adapter that declares `SupportsRefund=true` before a flow exists is the
  right fail-closed default, and the label is accurate per CLAUDE.md's no-fake-completion rule.
- Retry/timeout classification (§8) is correct throughout, in particular that a timeout *after*
  dispatch is always `Ambiguous` and never a failure, and that `NotProcessed` requires the
  vendor to *document* the response as safe-to-resend — every undocumented outcome correctly
  degrades to `Ambiguous`.
- **Gap: `WebhookRetrySemantics` is not in the §10.1 manifest table.** ADR 0097 §19 (payments
  review) condition 2 already commits `payments` to requiring
  `WebhookRetrySemantics{Retries429, Retries503, HonorsRetryAfter, RetryWindow}` as a
  **mandatory, fail-closed** field in this ADR's capability manifest, and that condition was
  accepted. §10.1's manifest field table as drafted here does not list it. This must be added
  before this ADR is treated as satisfying that condition (P95-C3) — it is a table-row addition,
  not a design change, since §18's I1-h ("manifest registration checks (incl. LF-C1(b) startup
  rule)") is already the right place to enforce it.

**MOCK adapter scope (§9.2, §16.1).** Correctly bounded. Tenant-tagged records,
merchant-reference lookup, idempotent-by-key mode, and the `NotSent`/`NotProcessed`/
`Ambiguous`/timeout/crash hooks are state-machine and test-harness plumbing to exercise the
contract described above — none of it invents a vendor wire format, signature scheme, or
vendor-specific error taxonomy. This matches CLAUDE.md's "mocks/sandboxes, clearly labelled"
rule and ADR 0022 §6's requirement that the MOCK pass the same conformance suite as a real
adapter, not a special-cased one.

**Work breakdown PRH-I1 a–i (§18).** Sequencing (I1-b/h/a in parallel; I1-d→e→f→g sequential) is
correct and consistent with the code dependencies described in §1 and §14. Two acceptance
criteria are missing from the item descriptions, both closed by the conditions below rather than
requiring resequencing: the migration-0082 mirror-invariant regression (I1-d/e, P95-C1) and the
`WebhookRetrySemantics` manifest field (I1-h, P95-C3).

**Conditions for APPROVE (P95-C#):**

- **P95-C1.** I1-d/I1-e must keep `TestMigration0082_DepositIntentsProviderColumnsStayMutable`
  passing and add an explicit test asserting `deposit_intents.provider_id`/`provider_reference`
  continue to mirror the latest `payment_attempts` row through a cascade, so §5.1's "keep
  mirroring" claim is proven by a test, not assumed.
- **P95-C2.** `docs/architecture/withdrawal-state-machine.md` (currently documenting
  `provider_id`/`provider_reference` as "set on `approved` -> `submitted`") must be updated to
  the §4.7 semantics no later than I1-f, before PRH-I1 is marked complete.
- **P95-C3.** Add `WebhookRetrySemantics{Retries429, Retries503, HonorsRetryAfter, RetryWindow}`
  to the §10.1 operation manifest table as a mandatory, fail-closed field for every non-MOCK
  payments adapter (registration refuses without it), which is what closes ADR 0097 §19
  condition 2 and the architect cross-review item on this ADR's own text, not just on the interim
  config key.
- **P95-C4 (informational, not blocking).** On LF-Q1 (T13's double post to `player_cash` versus
  a suspense account): `payments` prefers the ADR's stated default — post and raise P1 — over a
  new suspense-account type at this stage, since it keeps the ledger reflecting real received
  funds without adding an account type outside this ADR's scope. This is `ledger-finance`'s
  decision to make; recorded here only as the requesting domain's preference. *[SUPERSEDED by §28.7: ledger-finance ruled no posting for a
  second capture; HD-LEDGER-UNALLOC-1 chose (A) now, (B) later.]*

None of these conditions require a redesign of §4–§12; all four are documentation or test
additions to sections already inside this ADR's scope. Payments sign-off is granted on that
basis, conditional on P95-C1–C3 landing in PRH-I1/I1-f/I1-h as described.

---

## 24. QA test-plan review

**Verdict: CONFIRMED WITH CHANGES.**

Scope of this review is §16 only (crash points, adversarial groups, reconciliation tests,
mutations) plus the invariant table (§2) as the checkable contract those tests must close.

### What is solid

- The plan is invariant-first, not scenario-first: every adversarial group (§16.2 items 1–18)
  and every crash point (§16.1 CP-D1..CP-W6) states a **converged outcome**, which is a
  measurable pass/fail (state, posting count, call count), not "it worked." This is the right
  shape for financial code and satisfies "no fake completion."
- Crash injection is specified as **hooks in the MOCK adapter and orchestrator seams,
  test-only** (§16.1 heading), not timing/sleep races. The one place a sleep appears (item 14's
  pool-starvation variant, "N concurrent slow MOCK calls (sleep > 1 s)") is a deliberate
  connection-holding fixture reused from F-POOL-1, not a race-dependent assertion — acceptable,
  but it should be called out explicitly as the one intentional exception so a future reviewer
  doesn't read it as a stray sleep.
- Every financial adversarial test is required to end with INV-IO-13 (SUM(debits)==SUM(credits)
  plus projection==rebuild), which is exactly "prove no double credit/debit" made checkable
  rather than asserted.
- Tests run against real Postgres (triggers, RLS, `FOR UPDATE`, REPEATABLE READ, `pg_stat_activity`
  checks in item 14) rather than in-process mocks of the DB layer, which satisfies "do not rely
  solely on mock unit tests for financial correctness" — the MOCK is only the *provider*, never
  the ledger or the transaction boundary.
- The human's specific list is each directly covered: no double credit (#1, INV-IO-13), no
  double debit/payout (#2, CP-W4/W5), no lost successful payment (#3, three independent
  convergence paths: poll-only, callback-only, reconciliation+T17), no lost callback (#4,
  INV-IO-10), no unsafe retry (#5), ambiguous never silently success (#6, #9), duplicate
  callback idempotent (#7), callback before/after internal transition (#8, §6.4 both orders),
  timeout not auto-failure (#9), reconciliation detects divergence (§16.3, one fixture per
  mismatch kind).
- Repeated-run discipline exists at least for lock ordering ("no deadlock across 500
  iterations," item 16) and duplicate-callback ("sequentially and concurrently," item 7).

### Required changes before this test plan can gate `IMPLEMENTED`

1. **Mutation coverage is incomplete against the invariant list.** §16.4's MX1–MX9 map cleanly
   to INV-IO-1, 3, 6/7, 8, 10, 12, and the kill switch, but there is no mutation exercising
   INV-IO-2 (calling the provider without a *committed* `submitting` claim token — distinct from
   MX1, which is calling before an attempt exists at all), INV-IO-4 (bypass the CAS predicate
   itself in application code, as opposed to the trigger-rejection direct-SQL test in item 17),
   INV-IO-5 (post the ledger effect from inside phase B, or out of ADR 0082 lock order), INV-IO-9
   (flip `ever_possibly_sent` without a real send), or INV-IO-11 (let an adapter cache a
   credential across two calls). Add MX10–MX14 (or fold into existing IDs) so every INV-IO row
   has at least one mutation and a named failing test, per the ADR's own "checkable contract for
   qa" framing in §2.
2. **No migration up/down test, and no explicit 0100-backfill pass/fail test.** The PRH list
   requires migration up/down including the 0100 backfill; §13.1 describes the backfill's
   abort-on-violation behavior in prose but §16 never turns it into a test. Add: (a) up/down/up
   round-trip for 0100, 0102, 0103 leaves no orphaned constraints or triggers; (b) backfill over
   a fixture set with a pre-existing two-live-attempt violation aborts and lists the offending
   ids without partial writes; (c) backfill over clean synthetic data produces exactly the
   states §13.1 specifies.
3. **No RLS cross-tenant test named for the four new tables.** `payment_attempts`,
   `payment_provider_events`, `payment_kill_switches`/`_release_requests`, and
   `payment_statement_imports`/`_lines` are all `FORCE RLS` (§13), but §16 has no item asserting
   that a session scoped to tenant A cannot read or affect tenant B's rows in any of them (the
   existing `tenant_staff_scope` pattern presumably has a reusable harness — reuse it here by
   name).
4. **No API/OpenAPI conformance test for the new staff-admin surface.** The kill-switch
   engage/release endpoints (§10.4) are new HTTP surface with new permissions
   (`payments_kill_switch:engage/release`); §16.2 item 12 tests the domain effect and the
   route-table exclusion for players, but nothing pins the request/response shape or an OpenAPI
   diff for these routes plus the `T17` re-verify endpoint.
5. **Deterministic mechanism for "connection lost" fault points is unstated.** CP-D5 ("Phase C
   COMMIT connection lost") and the general "DB error causes a rollback" cases in §16.2 item 4
   need a named, deterministic injection mechanism (e.g., a `pgx` connection wrapper or
   `net.Conn` proxy that severs after the wire bytes for COMMIT are sent but before the ack is
   read) — otherwise this is either untestable or accidentally timing-dependent. Name the hook
   before implementation, not during.
6. **No breaker state-transition test.** §9.6 defines a closed/open/half-open breaker per
   (tenant, provider), fed by `ErrorClass`, and §16.2's list has no adversarial case forcing it
   through open → half-open → closed, verifying a tenant-A breaker does not affect tenant B, or
   that `DefiniteDecline` does not count against it. This is the "provider outage" case from the
   PRH list and is currently only implied by CP-W6 (credential outage, not transport outage).
7. **CI lane/time budget and package placement are not stated.** §16.2 item 14 says the
   pool-starvation variant runs "in its own isolated CI lane," which is right, but no time
   budget is given anywhere in §16, and no package location is proposed for the new suite.
   Given the existing `internal/httpserver` (~310 s) and full-suite (~600 s) budgets, propose:
   state-machine/evidence-matrix and mutation tests in `internal/payments` (fast, no adapter
   I/O, should not materially move the `internal/payments` budget); the crash-point and
   adversarial suite in a new `internal/payments/ioboundary` package sharing the existing
   Postgres test harness; the pool-starvation and `pg_stat_activity` capture tests
   (§16.2 item 14) in their own CI lane, budgeted separately and **not** counted against the
   `internal/httpserver` 310 s figure, since they intentionally hold slow connections. Reconciliation
   tests (§16.3) belong under `internal/reconciliation/payment_statement` alongside the existing
   casino_statement suite. This needs a number from `qa`/`payments` before I1-i lands, not an
   open-ended "isolated lane."
8. **Repeat-run / flake bound not stated for most concurrency cases.** Only the lock-order
   harness (500 iterations) and duplicate-callback ("sequentially and concurrently") specify a
   repetition count. CP-W4/W5, the concurrent-cascade case (item 11), and the concurrent
   duplicate-success-callback case (item 1) should each state a minimum iteration count (e.g.
   ≥100 runs in CI, `-race` enabled) so "no double X" is a statistically meaningful claim rather
   than a single lucky interleaving.

None of the above blocks starting PRH-I1 work — items 1–8 are additions to I1-i (§18) and to
§16 itself, not redesigns of the boundary. This ADR stays `NOT IMPLEMENTED`; when I1-i is
written, it must close items 1–8 and this section's verdict updates. `qa` does not sign off
`IMPLEMENTED` for PRH-I1 until they are closed or a specialist explicitly accepts the gap in
writing.

---

## 25. Identity-compliance review

Reviewer: `identity-compliance`, 2026-09-27. Scope: §15 (KYC create/submit split), IC-Q1
(§19.2), and the payout KYC gate contract against ADR 0096 (revised concurrently). Withdrawal
financial posting shape, ledger accounts and lock-order arithmetic are `ledger-finance`'s scope
(§12 of 0096), not re-litigated here.

### IC-Q1 — the KYC callback-race response class (§15.2)

**Resolved: retryable `5xx`, never a bare `200`.** §15.2 says a callback racing phase C "gets a
retryable response class, so the vendor redelivers after C commits," but does not say which
class. Payments can safely turn an unresolved race into a `200` (§6.2 `deferred_unresolved`)
only because `payment_provider_events` durably receipts the event first (INV-IO-10) — the
evidence is never lost even if nothing applies yet. §15.2 is explicit that KYC has **no receipt
table**. A `200` on an unresolved KYC callback would durably discard the only copy of that
evidence, which is worse than today's behaviour, not equivalent to it. §15.2 must state
plainly: an unresolved race returns a retryable `5xx`; a `200` is reserved for a callback the
platform actually applied (including a duplicate/no-op apply). Whether the vendor actually
redelivers on `5xx` is `PROVIDER DEPENDENT` and must be confirmed at real-vendor intake, the
same discipline §6.6/LF-C1 already applies to payments — this ADR should say so explicitly for
KYC rather than leaving it implied by analogy.

### §15 KYC create/submit split — state safety, idempotency, retries, KYC-SUBMIT-OUTBOX-1

**Provider-accepted-but-our-tx-failed is safe as designed, with two precisions required.**

- `CreateVerification`: a crash between phase B (vendor accepted) and phase C leaves an orphan
  `unverified` row with `provider_reference NULL`. This has no enforcement effect — ADR 0096's
  `EvaluateEnforcement` only allows on `passed`, and `unverified`/no-row already maps to
  `failed` — so an orphan never lets a player through. Confirmed safe. **Precision required:**
  the vendor idempotency key (`"kv:" + id`) is honored only "if the vendor supports keys"
  (`PROVIDER DEPENDENT`). Where it does not, a player retry before the orphan is known creates a
  second, unrelated vendor-side verification under a second platform row. That is not a
  financial or enforcement hazard (both rows are independently evaluated on their own merits),
  but it must be recorded as `PROVIDER DEPENDENT` vendor behaviour at intake, not silently
  assumed deduplicated.
- `SubmitVerification`: a crash between phase B and phase C is identical to today's
  `ProviderError` handling — the verification stays as-is, and the next document upload
  re-submits the full set under a new content-derived key
  (`"ks:" + verification_id + ":" + sha256(sorted document ids)`). Confirmed safe, and
  idempotent on a real retry. **Precision required:** §15.3 must state explicitly that an
  ambiguous or timeout `SubmitVerification` response leaves `kyc_verifications.status`
  unchanged — it must not be misread by `normalizeProviderResult` as a definitive outcome.
  Unlike payments, §15.3 does not reuse `applyEvidence`'s matrix, so this guarantee needs its
  own stated rule and its own test, not an inherited one.
- **`KYC-SUBMIT-OUTBOX-1` deferral: accepted.** No real KYC vendor exists yet (mock only); the
  worst case under deferral (a lost submission result, corrected by the next upload) is a
  strict improvement over today's tx-held-across-call hazard (§1 row #9), since it removes the
  pool-pinning risk while adding no new financial or enforcement exposure. This specialist will
  treat "the first real KYC adapter's intake" as a hard precondition, not a suggestion: no real
  KYC adapter is accepted into PRH-I2 without a durable submission outbox design landing first.

### Payout KYC gate contract — naming the hook so ADR 0095 and ADR 0096 agree

**Confirmed: T1p is the correct, and the only correct, hook — with one addition ADR 0096 must
make explicit.**

The hook both ADRs must name identically is: **T1p — the phase-A transaction that performs
`withdrawal.ClaimForDispatch` (`approved→submitted`) together with the payout attempt's
`∅→submitting` insert (§4.3, §5.2)**. This is, by construction, the last transaction that
commits before the first outbound `Withdraw` call for that request. ADR 0096's
`DenyForCompliance` (0096 §12 C2) must run inside this same phase-A transaction, evaluated
before T1p's own CAS is attempted, sharing the identical guard predicate
(`withdrawal_requests.state = 'approved'` under the same L1 `FOR UPDATE`, §14). Because both a
KYC deny and a T1p claim require `state = 'approved'` under the same row lock, they are
mutually exclusive by the lock itself, not by application-code ordering discipline — whichever
commits first structurally forecloses the other on that request. This is exactly the "last
committing tx before the first provider Withdraw call" property 0096 C5 asks for, and it is
already present in §4.3/§5.2/§14 as written; it only needs naming, not redesigning.

**Sweeper re-check — the case ADR 0096 does not yet name.** A payout attempt can return
`submitting→created` via T5 (`NotSent`, provably not dispatched — INV-IO-9), and is later
re-claimed by the sweeper via **T2** (`created→submitting`), not by a second T1p. 0096 §12 C5
correctly requires "the sweeper's claim transaction must re-run the gate before that first
send," but 0096 does not yet say which withdrawal-side transition a T2-time KYC deny should
produce, and it should not reuse `DenyForCompliance`'s `approved→rejected` edge unmodified —
by the time T2 fires, the withdrawal is already `submitted` (T1p already ran once), not
`approved`. Since a T2 reclaim only ever occurs while the payout attempt is provably `created`
with `ever_possibly_sent = false` (T5's own guard, INV-IO-9), the platform can prove no call has
reached the provider yet — this is exactly the eligibility 0095 §4.8 already defines for **M3**
("no double-payout risk, because `created` has provably never been sent"). ADR 0096 should
route a T2-time KYC deny through **M3's existing edge** (`created→rejected`, then
`withdrawal.Fail`), not invent a parallel one. Recommend 0096 §12 C5 (or a new sub-bullet) name
this explicitly: *"a KYC deny discovered at a T2 payout reclaim is M3, never `DenyForCompliance`
applied a second time."*

**Never releasing a hold once dispatch may have occurred — confirmed structural, not
discretionary.** Once a payout attempt is `submitting`/`pending`/`ambiguous` with
`ever_possibly_sent = true`, INV-IO-7 forbids any `*→declined` transition without provider
evidence, and the trigger's `evidence_kind` enum (`callback | query_status | sync | sweeper`,
§4.2) has no member for a KYC decision. A KYC outcome is therefore structurally incapable of
driving T8 once the attempt may have reached the provider — this guarantee comes from the
state-machine trigger itself, not from an application-level check that a future code path could
bypass. No additional mechanism is required beyond the naming corrections above.

### Verdict: **APPROVE WITH CONDITIONS**

1. §15.2 must state the callback-race response class explicitly as retryable `5xx` (IC-Q1),
   with vendor redelivery-on-`5xx` flagged `PROVIDER DEPENDENT` for confirmation at real-vendor
   intake.
2. §15.3 must state explicitly that an ambiguous/timeout `SubmitVerification` result leaves
   verification status unchanged, with its own test (not inherited from §4.4's matrix, which
   §15.3 does not reuse).
3. §15/§5.2 (or a cross-reference to 0096) must name the payout KYC gate hook precisely as
   **T1p** for the initial claim, sharing the `state='approved'` L1 guard with
   `DenyForCompliance`, so the two are mutually exclusive by lock, not by convention.
4. ADR 0096 (0096 §12 C5) must be extended, before either ADR is marked implementable, to name
   the T2-payout-reclaim case explicitly and route it through **M3** (not a second
   `DenyForCompliance`), per the reasoning above — this is a required cross-ADR consistency fix,
   not a new design.
5. `KYC-SUBMIT-OUTBOX-1`'s deferral is accepted as a documented precondition on the first real
   KYC adapter intake (PRH-I2), not merely a flagged future item.

None of the above requires a redesign of §15's mechanism or of ADR 0096's enforcement boundary;
all are naming/precision fixes needed so the two documents describe the same hook, plus two
missing explicit statements in §15.2/§15.3.

---

## 26. Casino review

**Verdict: APPROVE WITH CONDITIONS**

Reviewed as casino-integration owner, scoped to §15.1 (`casino.LaunchGame`) and CAS-STMT-IO-1
only.

**§15.1 launch split.** Agreed. Phase A commits the `casino_launch_sessions` intent
(`status='active'`, `expires_at`-bounded) with no lock and no transaction held across the
vendor `Launch` call in phase B — consistent with the no-lock-across-I/O principle in §14 and
the general split pattern in §12. Phase C's CAS on `RevokeLaunchSession`
(`status='active'` guard, exists today) correctly handles both an explicit vendor failure and
an ambiguous outcome by revoking rather than assuming success, so a launch token is never
minted or handed out for a session the platform can't account for. Orphaned-session handling
is correct and matches this agent's non-negotiable: a crash on either side of the vendor call
leaves the session `active` and unresolved-to-the-player; it simply expires, and a bet against
an unknown/expired/revoked session returns `ErrLaunchSessionRequired` with no ledger effect —
no money moves without a verified bet on a resolvable session. Idempotency is sound: `SessionID`
is the deterministic external reference/key for phase A, retries after a crash mint a new
session (existing behaviour, no new state), and there is no path where a duplicate `Launch`
call or a duplicate token can be produced from the same intent. Launch-token minting itself
(single-use, opaque, bound to player/provider/game/currency/mode, short TTL, never the brand
session token) is unchanged by this ADR and is out of scope here — no new review trigger for
`security`. No objection to §15.1 as specified; no redesign needed on this agent's surface.

**CAS-STMT-IO-1.** Severity agreed: Low, not reachable today. The mock `CasinoStatementSource`
is in-memory and single-process, so the provider-I/O-inside-a-tx hazard this finding names does
not exist yet — there is no real vendor call to hold a transaction open across. Fix timing
agreed: no redesign now; the fetch → ingest → match split from §12.1 is a hard precondition
before wiring any real casino statement/reconciliation source, not before this stage's mock
work. This agent will treat "first real casino statement source" as the trigger and will not
build or accept a real statement-source adapter that reads inside the reconciliation
transaction — that adapter must land already split per §12.1, gated on `architect` and
`ledger-finance` sign-off, consistent with this agent's mock-only-without-a-commercial-
relationship constraint.

**Conditions for full APPROVE:** none blocking merge of this ADR's casino sections as written;
tracking condition only — CAS-STMT-IO-1's remediation must be verified (not just referenced) at
the time a real casino statement source is proposed, before that adapter is accepted.

---

## 27. Revision record

**Revision 2 (2026-09-27, `architect`).** Folded every review condition into the design text,
reordered the review sections numerically (§21–§26, text unchanged), applied the orchestrator's
migration renumbering, and set the status to ACCEPTED (design) — NOT IMPLEMENTED.

**Migration renumbering (orchestrator, 2026-09-27).** Payment attempts, receipts, triggers and
backfill moved from **0100 to 0101**, because 0100 is now ADR 0096's KYC enforcement migration.
0102 (kill switch) and 0103 (payment statement reconciliation) are unchanged. The body uses the
new numbers throughout (header, §0 D2, §2, §4.2, §13, §16.2 item 22, §18). The review sections
§21–§26 are quoted as written, so where they say "migration 0100" for payment attempts they mean
0101.

**Cross-ADR consistency with ADR 0096** (read at its current working-tree text):

- T1p is named as THE payout KYC hook, and `DenyForCompliance` (W-KYC) is listed as a
  pre-dispatch withdrawal transition, legal only from `approved`, in T1p's tx (§4.3, §5.2,
  §5.2.1).
- ADR 0096 §12 C5 asks for the gate to be re-run on any sweeper dispatch; this ADR does so at
  the T2 re-claim and at T12 (§5.2.1).
- IC condition 4 (ADR 0096 must name the T2-reclaim case and route it to M3) is **ADR 0096's
  side, owned by `identity-compliance`**. This ADR's side is written (§4.8 M3, §5.2.1). At the
  time of this revision ADR 0096's text does not yet name it. Re-check when its revision lands
  (LF95-C10(f)).
- The KYC function is named generically as "the ADR 0096 enforcement function exported by
  `internal/kyc` (PRH-I3)".

### 27.1 Ledger-finance (§21)

| ID | Status | Where satisfied |
|---|---|---|
| LF95-C1 | Satisfied | §8 `NotSent`/`NotProcessed` rows; §4.3 T5 and T6 (resend `NotSent` → T6) |
| LF95-C2 | Satisfied | §4.2 `last_evidence_kind`; §2 INV-IO-7; §4.3 forbidden list; §13.1 column and trigger |
| LF95-C3 | Satisfied | §4.4 preconditions 1–2; §6.1 step 4; §6.2 `anomaly` row (T10 committed) |
| LF95-C4 | Satisfied | §4.4 precondition 3; T7; §4.6 persisted `cascadable`; §6.1 step 6; §13.1 receipt columns and CHECKs |
| LF95-C5 | Satisfied | §10.1 `CallbackEchoesMerchantReference` plus registration rule; §6.4 last row; §7.1; §12.3 `pay_unresolved` |
| LF95-C6 (a)–(d) | Satisfied | (a) §5.1 "Ledger link", T7, §13.1; (b) §5.4 "Reference"; (c) T2 predicate, T13, T3 `intent_succeeded`, §20 residual; (d) T7/T13, §4.4 tombstone cells |
| LF95-C7 | Satisfied | §5.1 intent projection |
| LF95-C8 (a)–(d) | Satisfied | §4.5; §4.2 `last_sent_at`; §10.1 registration rule; §4.6 |
| LF95-C9 (a)–(e) | Satisfied | §14; §7.2; §4.3 preamble; §6.4 |
| LF95-C10 (a)–(f) | Satisfied (f: re-check pending ADR 0096 revision) | §5.2 flow; §5.2.1; §4.3 T1p, W-KYC, T2, T12; §4.8 M3; §16.1 CP-W7/W8 |
| LF95-C11 (a)–(g) | Satisfied | §13.1 backfill table and post-checks; `legacy_backfill` and trigger; §16.2 item 22 |
| LF95-C12 | Satisfied | §5.1 flow (T1+T2 in one tx); §16.1 CP-D1 restated |
| LF95-C13 | Specified; gates `IMPLEMENTED` (PRH-I5) | §12.3 ledger join; §12.1 step 3; §16.3 |
| LF95-C14 | Specified; gates `IMPLEMENTED` (PRH-I1-i) | §16.2 items 19 and 22 |
| LF95-R1 | Adopted | §12.5 `payments` re-drive job; §18 I5-c |
| LF95-R2 | Deferred, owner `bonus-engine` | §20 BONUS-T13-ELIGIBILITY-1; §5.1 |
| LF95-R3 | Noted | §9.2 |
| LF-Q1/Q2/Q3 rulings | Applied | §19.2; §4.4; §14; §12.6 |

### 27.2 Security (§22)

| ID | Status | Where satisfied |
|---|---|---|
| **S95-C1 (High, launch-blocking)** | Satisfied | §2 INV-IO-14; §4.3 T7/T8/T15; §4.4 precondition 1; §6.1 step 4; §6.4; §6.5; §7.1; §12.3; tests §16.2 item 20; mutation MX15 |
| S95-C2 (i)–(iii) | Satisfied | §6.1 step 5 cap; §6.2 cap and CHECK rows; §6.4; §7.3; §13 intro; §13.1 index |
| S95-C3 | Satisfied | §4.2 `first_submitted_at`; §6.4; §13.1 `resolution`; §20 PAY-ATTEMPT-RETENTION-1 constraint; MX18 |
| S95-C4 | Satisfied | §6.2 uniform 200 body; §16.2 item 20 |
| **S95-C5 (launch-blocking)** | Satisfied | §10.2 trigger; §13.2 guard; §16.2 item 20; MX17 |
| **S95-C6 (launch-blocking)** | Satisfied | §10.3 in-statement `NOT EXISTS`; §2 INV-IO-15; §4.3 T1+T2/T1p/T2/T12; §16.2 item 20 |
| S95-C7 | Satisfied | §10.2 `engaged_by_scope`, engage alert; §10.4; §13.2 |
| **S95-C8 (a)–(c) (launch-blocking)** | Satisfied | §3.2 steps 4 and 8; §9.1; §11 rows; §16.2 items 15 and 20 |
| S95-C9 (i)–(iii) | Satisfied | §3.2 step 3; §7.2 step 6; §11 "Observability" |
| S95-C10 | Satisfied | §9.3; §13.1 CHECKs; §20 VENDOR-INTAKE-REF-PII-1; §16.2 item 20 |
| S95-C11 | Satisfied | §9.5 `Fetch(CallContext)`; §12.1 step 1; §13.3 CHECKs; §16.3 |
| S95-C12 (i)–(iii) | Satisfied | §10.1 registration refusals and `WebhookRetrySemantics`; §8 `NotProcessed` |
| **S95-C13 (launch-blocking)** | Satisfied | §10.5 routes; §4.8 M3 CAS; §13.1 trigger; §16.2 item 20 |
| S-Q1 / S-Q2 / S-Q3 rulings | Applied | §10.4; §6.2; §6.6 option (a), §15.1, §15.2 |
| §22.5 tests 1–10 | Specified | §16.2 item 20 |

### 27.3 Payments (§23)

| ID | Status | Where satisfied |
|---|---|---|
| P95-C1 | Specified | §5.1 "Provider reference"; §16.2 item 19 |
| P95-C2 | Specified (due by I1-f) | §4.7; §18 I1-f and specialist documents |
| P95-C3 | Satisfied | §10.1 `WebhookRetrySemantics` (mandatory, fail-closed); §6.6 |
| P95-C4 | Informational; consistent with the LF-Q1 ruling | §4.4 |

### 27.4 QA (§24)

| Change | Status | Where satisfied |
|---|---|---|
| 1 Mutation per invariant | Specified | §16.4 MX10–MX18 |
| 2 Migration up/down and backfill tests | Specified | §16.2 item 22 |
| 3 RLS cross-tenant tests, new tables | Specified | §16.2 item 22 |
| 4 API/OpenAPI conformance | Specified | §10.5; §16.2 item 20 |
| 5 Deterministic connection-loss hook | Specified | §16.1 preamble (`pgfault` proxy) |
| 6 Breaker state-transition test | Specified | §16.2 item 21 |
| 7 CI lanes, budgets and placement | Specified (`RECOMMENDATION` numbers; `qa` confirms in I1-i) | §16.5 |
| 8 Repeat-run bounds | Specified | §16.2 items 1, 2 and 11 (≥ 100 with `-race`) |
| Intentional sleep called out | Done | §16.2 item 14 |

### 27.5 Identity-compliance (§25)

| Condition | Status | Where satisfied |
|---|---|---|
| 1 KYC race → retryable 5xx; redelivery PROVIDER DEPENDENT | Satisfied | §15.2 "Callbacks"; §16.2 item 18 |
| 2 Ambiguous `SubmitVerification` leaves status unchanged, own test | Satisfied | §15.3; §16.2 item 18 |
| 3 T1p named as the payout KYC hook, shared L1 guard | Satisfied | §4.3 T1p and W-KYC; §5.2; §5.2.1 |
| 4 T2-reclaim KYC deny → M3 | This ADR's side satisfied (§4.8 M3, §5.2.1); **ADR 0096's side owned by `identity-compliance`**, pending in its revision | §18 specialist documents |
| 5 KYC-SUBMIT-OUTBOX-1 as a hard precondition | Satisfied | §15.3; §18; §20 |
| CreateVerification vendor idempotency PROVIDER DEPENDENT | Satisfied | §15.2 |

### 27.6 Casino (§26)

| Condition | Status | Where satisfied |
|---|---|---|
| CAS-STMT-IO-1 remediation verified (not referenced) when a real casino statement source is proposed | Tracking condition, owner `casino` (+ `architect`, `ledger-finance`) | §15.1 "Reconciliation"; §20 |

### 27.7 Not satisfiable in this ADR

These are escalated or deferred, and none is silently dropped:

| Item | Status |
|---|---|
| HD-0095-1: M1/M2 manual resolution authority and thresholds | Human decision, not taken; M1/M2 stay BLOCKED (§19.1) |
| LEDGER-MANUAL-ADJ-4EYES-1 (compensation mechanism) | Registry item, not built; remediation of `disputed` and of several `pay_*` kinds is BLOCKED on it |
| ADR 0096 naming of the T2-reclaim → M3 route | Owner `identity-compliance` (§27.5) |
| Re-verification of LF95-C10 against ADR 0096's landed revision | Owner `ledger-finance` (LF95-C10(f)) |

### 27.8 Revision 3 — security re-verification RV-0095 (`docs/plans/payment-readiness/rv-0095-security-reverify.md`)

Security closed S95-C1, C6 and C8 at design level. It found the kill switch (C5, C7, C13) still
open, with findings N1–N4 and Lows L1–L4. All are written into the text as follows:

| ID | Where satisfied |
|---|---|
| N1 (C5): re-scoping an engaged row is a single-actor release | §10.2.2 point 2 (`id`, `tenant_id`, `provider_scope`, `operation_scope` immutable); §13.2 guard; §16.2 item 20; MX19 |
| N2 (C7): app-written, mutable scope columns | §10.2.1 (actor and scope forced from the session and verified against `staff_users` as migration 0044 does; CHECK enums on every scope column); §10.2.2 point 5 (never `platform→tenant`; no tenant UPDATE of a platform-engaged row); §13.2; §16.2 item 20; MX20 |
| N3 (C7 + C13): platform path unbuildable, no route spec | §10.2.1 platform RLS family (migration 0075 precedent: platform GUC set, tenant and player GUCs unset; SELECT/INSERT/UPDATE, no DELETE); §10.4; §10.5 platform routes, `platform_payments_kill_switch:engage/release/read`, target-tenant rule, dual audit; §16.2 item 20 |
| N4 (C5): release binding had no mechanism | §10.2.2 point 6 (`release_request_id` with composite FK, `UNIQUE`, and `decided_txid = txid_current()`: approval and release in one tx); request guard forces `created_at`, `expires_at ≤ +24 h`, `decided_at`, `decided_txid`; §13.2; §16.2 item 20; MX21 |
| L1 (C6): INSERT-form fail-closed coupling implicit | §10.3 "Why a misbound context fails closed" (coupling stated as binding; no other `payment_attempts` write policy without `security` re-review); §16.2 item 20 player- and platform-context claim test |
| L2 (C8): item 15 described the superseded reflection test | §16.2 item 15 (recursive test, static test, `Domain` mismatch, gate-alone and adapter-alone cases) |
| L3 (C3): `received_at` only a DEFAULT | §13.1 (`received_at` and `resolved_at` forced by trigger); §16.2 item 20 |
| L4: cross-tenant FK on release requests | §13.2 composite FK `(tenant_id, kill_switch_id)` → `(tenant_id, id)`; §16.2 item 20 |

| N5 (C5, addendum): release-request column write moments | §10.2.1 actor-column table (which column is forced at which step); §10.2.2 request guard (INSERT-only columns immutable, approval-only columns, terminal rows immutable); §13.2 comment; §16.2 item 20; MX25 |
| L5 (addendum): tenant obstruction of platform release; pre-consumed `release_request_id` | §10.2.2 point 8 (NULL forced on switch INSERT); request guard refuses a tenant-session INSERT on a platform-engaged switch; §13.2; §16.2 item 20 |

With these, every S95 condition is closed in the design text, pending `security`'s confirmation.
Migration 0102 and the kill-switch routes must not be implemented before that confirmation
(RV-0095 "Launch relevance"). Nothing here is implemented.

### 27.9 Revision 3 — ledger-finance re-verification RV-0095 (`docs/plans/payment-readiness/rv-0095-ledger-reverify.md`)

| ID | Where satisfied |
|---|---|
| LF95-C6(d): the T13-with-tombstone contradiction | New **T13t** `declined → disputed` (deposit only, `reversal_tombstone_precedes_success`) in §4.3, the forbidden list and the trigger (§13.1 "§4.3 exactly"); §4.4 `declined` row and tombstone note; §16.2 item 19 |
| N1 (High): deposit T2 took L0.4 after L1 | §4.3 T2 (deposit gates before the parent lock; payout gate after, since it takes no lock); §5.1; §7.1; §7.2 step 3; §14 per-path list and harness; §16.2 item 16; MX24. `ledger-finance` carries the rule into the A7 text in ADR 0082. |
| N2: INSERTs unguarded, `legacy` not a boundary | §13.1 `payment_attempts_insert_guard`, created after the backfill in 0101 (no session-setting escape); `CHECK (last_evidence_kind <> 'legacy' OR legacy_backfill)`; §4.2 enumeration includes `legacy`; §16.2 item 19; MX22 |
| N3: T12 could re-send after T13 | §4.3 T12 predicate `NOT EXISTS(succeeded attempt for the same intent)` for deposits; §16.2 item 19; MX23 |
| N4: backfill convergence columns | §13.1 "Columns set on every backfilled row" (`next_action_at`, `first_submitted_at`, `last_sent_at`, `interactive`, payout `ever_possibly_sent`, amount/asset source); §16.2 item 22 |
| L1 | §4.2 (with N2) |
| L2 | §4.3 T2 (escalated cadence; a later pass resumes via T2) |
| L3 | §4.8 M3 audit `kyc_denied` (already required); §16.2 item 19 |
| L4 | §12.3 join paths (payout via `withdrawal_requests.release_ledger_transaction_id`) |
| LF95-C10(f) and the ADR 0096 items (T2-reclaim → M3 route; raw-guard retarget to `ClaimForDispatch` plus the payout T2/T12 per-item claims; renumbering 0096 to migration 0100; allowing the escalation write at a T2 non-pass) | **Owner `identity-compliance` (ADR 0096)**. This ADR's side is §4.3 T2/T12, §4.8 and §5.2.1. Still OPEN until ADR 0096 lands them and `ledger-finance` re-checks. |
| Open ledger-finance actions (A7 into ADR 0082; the reconciliation-model §2.2(b) amendment) | Owner `ledger-finance`; not ADR 0095 defects |

Migration 0101 must be implemented against this revision, not revision 2. In particular: the
T13t pair, the INSERT guard (created after the backfill), the `legacy` CHECK and the N4 backfill
columns.

### 27.10 PRH-payments-callback-cutover implementation record (`payments`, 2026-09-27)

**Scope of this round.** Cuts `ReceiveVerifiedCallback`'s deposit and reversal branches over from
the pre-cutover, `deposit_intents`-only path (`receiveDepositCallback`/`receiveDepositReversalCallback`,
now removed) onto the ADR 0095 §6.1 receipt path (`internal/payments/receipt.go`
`ApplyReceiptEvidence`), adds the deposit_reversal cell to that receipt path (§5.4, LF95-C6(b)/(d)),
and implements the ADR 0097/§6.2 HTTP contract (uniform 200 body; `deferred_unresolved` → 200; the
unapplied-receipt cap → 503 + `Retry-After`) in `internal/httpserver/deposit_handlers.go`. Payout
dispatch (withdrawal claim T1p/T2/T12) is out of scope for this round and was being built
concurrently by another agent; `drive.go`/`sweeper.go` were not touched.

**(a) deposit_reversal in the receipt-path matrix.** New `applyReversalReceiptEvidence`
(receipt.go): resolves the ORIGINAL deposit attempt by `(verifiedProviderID,
OriginalProviderReference)` against `payment_attempts` - never by the reversal's own
`ProviderReference`, never against `deposit_intents` directly (LF95-C6(b)). No matching attempt, or
one with no `ledger_transaction_id` yet, takes the tombstone branch (`postDepositReversalTombstone`,
refactored to take explicit fields instead of a `CallbackEvent`). A resolved attempt is locked in
ADR 0082/§14 order (parent `deposit_intents` row, then re-read the attempt) before the integrity
check, the amount/asset cross-check, the second-distinct-reversal check (Stage 10.1 PAY-REV-1's S2/S4
steps, kept verbatim) and `ledger.Post`. The receipt's stored/fingerprinted `Outcome` is normalized
to `succeeded` for every persisted reversal receipt (both branches): the wire `Outcome` field is
historically reused by some callers as a chargeback-reason carrier (`declined` + `DeclineReason:
"chargeback"`), which is not a §4.4 attempt-decline concept and would otherwise violate
`payment_provider_events_check1`/`..._decline_stage_check` (which require `cascadable`/`decline_stage`
whenever `outcome='declined'`, a deposit-ATTEMPT-decline-only pair a reversal never has). Typed
reversal rejections (`ErrDepositAlreadyReversed`, `ErrCallbackPayloadMismatch`,
`ErrDepositReversalIntegrity`, `ErrCallbackProviderMismatch`) are unchanged: they propagate as Go
errors, the callback transaction rolls back (including any receipt-insert attempt), and the HTTP
layer records the denial in a separate transaction via `RecordDepositReversalRejection` exactly as
before (§6.2 "typed reversal rejections... unchanged").

**(b) one evidence-application rule.** `ReceiveVerifiedCallback`'s `CallbackEventDeposit` and
`CallbackEventDepositReversal` branches both now call `receiveCallbackViaReceiptPath`, which builds
a `ReceiptEvidence` from the adapter's `CallbackEvent` and calls `ApplyReceiptEvidence` (dispatching
internally to the deposit matrix or `applyReversalReceiptEvidence` by `EventType`). Removed:
`receiveDepositCallback`, `receiveDepositReversalCallback`, `providerExclusionSoFar` (the last was
only used by the removed function; the synchronous, pre-cutover `InitiateDeposit`/`handleDecline`/
`resolveAmbiguous` cascade - itself already legacy and unreachable from any live HTTP path per the
prior cutover round - is untouched, since it still resolves cascade exclusion in-process rather than
via a callback). `ReceiveCallbackResult` gained a `Disposition ReceiptDisposition` field; its other
fields (`DepositIntentID`/`Status`/`LedgerTransactionID`/`Tombstoned`) are now best-effort
enrichment only, populated by re-reading the intent/attempt after `ApplyReceiptEvidence` returns,
never part of the public webhook HTTP contract (see (c)).

**(c) §6.2 HTTP contract.** `newPaymentWebhookHandler` now writes ONE uniform 200 body
(`{"request_id","received":true}`) for every disposition (`applied`, `duplicate_effect`,
`deferred_unresolved`, `anomaly`) - the disposition itself is logged at `Info` for operator
observability only, never returned. `payments.ErrDeferredReceiptCapExceeded` (the §6.1 step 5 /
S95-C2(i) cap) maps to 503 + `Retry-After: 1` with a P1 log line, before the generic 500 fallback.
ADR 0097's admission → verification → binding → parsing order (webhook_admission.go,
webhook_preamble.go) was not touched. OpenAPI (`docs/api/openapi/platform-api.yaml`) updated: the
200 schema is now `{request_id, received}`; 404 removed from the route's documented responses; 503's
description and `Retry-After` note extended to the cap case.
`openapi_paymentswebhook_contract_test.go` extended to assert 404 is absent and the 200 schema is
the uniform shape.

**(d) old→new behaviour changes and the tests adapted for them** (LF95-C3, §6.2; no financial
assertion was weakened - each adaptation below either keeps the exact same invariant under a new,
ADR-mandated status code, or fixes the test's own setup to match the post-cutover on-disk shape):

| Clause | Old | New | Test(s) adapted |
|---|---|---|---|
| §6.2 `deferred_unresolved` | An unresolved deposit callback (`ErrDepositIntentNotFound`) was a 404 | 200 after a durable receipt; the callback is retried later (phase C/sweeper) or stays deferred | `financial_flow_integration_test.go` `TestPaymentWebhook_UnsignedPayloadRejected` (its own trailing "signed but unknown reference" probe) |
| §6.2 `anomaly` (mismatched success, LF95-C3) | `ErrCallbackProviderMismatch` rolled back the tx, mapped to 400 | The attempt moves to `disputed` (T10, `terminal_reason=callback_amount_asset_mismatch`) and that state change is COMMITTED with its receipt; response is the uniform 200 | `payment_webhook_post_verification_400_integration_test.go`: `TestWebhook_ProviderMismatchAfterVerification_Maps400` replaced by `TestWebhook_ProviderMismatchAfterVerification_IsDisputedNotRejected`, which asserts the uniform body AND queries `payment_attempts` directly for the `disputed`/`callback_amount_asset_mismatch` state - the "no ledger effect" and "no amount/asset/reference echoed" assertions are unchanged |
| §4.4 tombstone cell / LF95-C6(d) (T10 from a live state) | A deposit success colliding with an existing reversal tombstone rolled back to a non-200 | Committed terminally as `disputed` (`reversal_tombstone_precedes_success`); uniform 200 | `webhook_admission_t6_idempotency_integration_test.go` `TestAdmission_T6f_PaymentsReversal_BeforeDeposit_ThenDepositArrives`: now asserts 200 plus the `disputed`/`reversal_tombstone_precedes_success` attempt state (previously only asserted "not 200" and the ledger row count, which is unchanged and still asserted) |
| INV-IO-14 (attempt-based resolution) applied to test fixtures that predate it | Several existing integration tests built their deposit fixture with the pre-cutover `Orchestrator.InitiateDeposit` (no `payment_attempts` row) and then drove a callback through it - this is exactly the on-disk shape §6.1 step 4 now requires a resolvable attempt for, so those callbacks became `deferred_unresolved` (silently not posted) instead of posting | `resolution_isolation_integration_test.go`'s `depositFor` helper now also drives a matching `payment_attempts` row through the SAME exported T1+T2/T4 functions (`InsertSubmittingAttempt`, `MarkAccepted`) `InitiateDepositAttempt` itself would have produced, so its callback resolves and posts exactly as before; `internal/reconciliation/payment_statement_fixround_integration_test.go`'s `legacyDeposit` helper (which exists SPECIFICALLY to prove reconciliation's F1 `legacy_unattempted` exclusion for attempt-less historical data) now constructs that on-disk shape directly (`InitiateDeposit` for the intent row, then a raw `ledger.Post` + `UPDATE deposit_intents`) instead of driving it through today's callback path, which no longer accepts an attempt-less deposit at all - this is the correct fix, since the whole point of that test is to exercise the RECONCILIATION exclusion rule against attempt-less data, independent of how such data is produced |

**Round 2 (2026-09-27): full migration completed.** All ~13 `internal/payments` test files named
above are migrated, using a shared test bridge (`internal/payments/receive_bridge_test.go`,
`initiateDepositWithAttempt`/`backfillAttemptForIntent`): after `Orchestrator.InitiateDeposit`, in
the SAME transaction, it drives a matching `payment_attempts` row through the SAME exported
T1+T2/T4/T6/T7/T8/T9/T11 transitions `InitiateDepositAttempt`'s own real path would have produced,
keyed off the intent's resulting final state (idempotent - it backfills only if the intent has no
attempt yet, so it composes safely with a retried/idempotent `InitiateDeposit` call). Two genuine
defects were found and fixed during migration, each with its own mutation-kill evidence recorded in
`docs/plans/payment-readiness/evidence/prh-i1-mutation-kill.txt` (round 2 section): (1) the
reversal-enrichment lookup in `receiveCallbackViaReceiptPath` used the wrong key for the tombstone
case (fixed to resolve which of the two rows applies, exactly as `applyReversalReceiptEvidence`
itself does); (2) `deposit_intents.status` was never recomputed as an attempt projection for
anything other than the two terminal cases already written directly by
`applyDepositSuccessAndPost`/`finalizeDeclined` - fixed with a new `recomputeDepositIntentProjection`
(receipt.go) implementing LF95-C7's exact rule, called from both the live path and the
`ApplyDeferredReceiptsForAttempt` sweeper backstop. Three tests' original intent could no longer be
expressed as written (old→new behaviour) and were adapted with strictly equal-or-stronger
assertions (documented in-line and in the mutation-kill evidence):
`TestReceiveCallback_UnknownProviderReferenceRejected` (404→`deferred_unresolved`/200, now also
asserting the durable unresolved receipt row exists),
`TestReceiveCallback_ReversalOfNeverPostedDepositWritesTombstone`'s tombstone-collision case (hard
error→committed `disputed`, now also asserting the attempt's terminal state), and
`TestReceiveCallback_AmbiguousCallbackResolvedViaQueryStatus_NotCascaded` (synchronous
`QueryStatus`-in-transaction→zero provider calls plus a scheduled `next_action_at` for the sweeper,
per §6.5's own "no cascade I/O, no QueryStatus" rule). The previously-disclosed Outcome-
normalization mutation gap is closed at the HTTP layer too
(`internal/httpserver/payment_webhook_contract_integration_test.go`
`TestPaymentWebhook_ReversalOutcomeDeclinedWireCarrier_PersistedAsSucceeded`), alongside a
direct §6.2/S95-C4 uniform-response-body HTTP test
(`TestPaymentWebhook_UniformResponseAcrossDispositions`) and a real cap-exceeded HTTP test
(`TestPaymentWebhook_DeferredReceiptCapExceeded_503RetryAfter`). Full confirmation: `go test -race
-tags=integration` is green across `internal/payments`, `internal/httpserver` and
`internal/reconciliation` (main lane), and the ci-local.sh timing-lane tests pass in isolation (two
showed the wall-clock-threshold flakiness their own doc comments already disclose under this host's
concurrent multi-agent load during the combined run, and passed cleanly re-run alone - neither
touches any code path this round changed). No test was deleted, skipped, or weakened.

### 27.11 PRH-payments-payout-dispatch implementation record (`payments`, 2026-09-27)

**Scope.** T1p/phase-B/phase-C payout dispatch (`ClaimForDispatch`/`DispatchWithdraw`/
`ApplyPayoutResult`, `internal/payments/payout.go`), the sweeper's T2 re-claim and T12
resubmission (`internal/payments/payout_sweep.go`), and `internal/httpserver`'s submit/resolve
handlers. Went through two independent review rounds (`code-reviewer`: NOT READY;
`ledger-finance`: REJECT with a hard veto on C1) before reaching the state recorded here; both
reviews are filed at `docs/plans/payment-readiness/rv-prh-i1-payout-code-review.md` and
`rv-prh-i1-payout-ledger.md`, and every finding (C1, H1-H4, M1-M5, L1-L3, plus the independent
review's B1-B8) is addressed or explicitly disclosed as open in this record.

**Fail-closed choices made where this ADR is silent (per the orchestrator's standing
instruction):**

1. **§4.3 T12's exact convergence order.** The ADR requires `IdempotentSubmission &&
   submit_count < max_resubmits` before any resend, but does not fix whether a QueryStatus poll
   must happen before or interleaved with that check. Chosen: **poll first, unconditionally, if
   a reference exists at all** (`resubmitPayoutAmbiguous`) - only if the attempt is STILL
   `ambiguous` after that poll does the manifest/cap gate even run. This is the more
   conservative reading of §4.5's "ambiguous plus a poll is the default": a poll can never make
   things worse (it is always safe/idempotent at the provider), so it is never skipped as an
   optimization.
2. **A submitting attempt with no provider reference and an expired lease** (H3/B2 - e.g. the
   process crashed between phase A and phase B, or phase B's own credential resolution failed
   before ever reaching the provider). The ADR's T6 trigger list ("lease expired and QueryStatus
   not definitive") presumes a reference to query. With none, `PollPayoutStatus` moves the
   attempt directly to `ambiguous` (T6) rather than rescheduling forever - fail-closed in the
   sense that the platform can never prove the call was NOT sent, so it treats it as possibly
   sent (never a plain re-claim/T2, which requires proof of never-sent, per INV-IO-9).
   **Disclosed limitation:** this ADR's CP-W1/§4.5 "QueryStatus by merchant reference" recovery
   path is NOT implemented - `PaymentProvider.QueryStatus` takes only a provider reference in
   this codebase's current contract, and no adapter (including the mock) supports a
   merchant-reference lookup. Building that is a provider-contract change, out of scope for this
   round; recorded here rather than silently assumed complete.
3. **A kill-switch block (migration 0105, built concurrently by another agent) at T2/T12.**
   Neither this ADR nor ADR 0096 specifies how a transient kill-switch refusal should be
   distinguished from a permanent KYC-deny escalation. Chosen: a kill-switch block **reschedules**
   (a plain backoff, `RescheduleNonTerminal`), never `Escalate`/T16 - unlike a KYC deny, there is
   no compliance decision to record and no human action required to clear it; the exact same
   claim/resend is safe and expected to retry automatically once the switch is released.
4. **A7's attempt-before-posting lock order, applied to phase C's late/contradicting-evidence
   case (M4).** The ADR names T14 (`declined -> disputed`) for "success after a declined
   payout" but does not specify how phase C notices the contradiction structurally. Chosen: on
   an `ErrAttemptStateConflict` from `ApplySuccess`/`ApplyDecline`, re-read the attempt's actual
   current state and route to T14 (if `declined`) or T10 (any other unexpected state) with a P1
   audit record (`payments.payout_late_contradicting_evidence`) - never a bare rollback that
   reduces the signal to a log line.
5. **Decline-reason sanitization (B8/S95-C10).** No enum of canonical payout decline reasons
   exists yet anywhere in this codebase. Chosen: a small, conservative allow-list
   (`canonicalDeclineReason`) covering the mock adapter's own declared reasons plus a generic
   `provider_declined` fallback for anything else - deliberately minimal pending a real PSP
   integration's own vendor code list; recorded as a fail-safe default, not a complete taxonomy.

**Not implemented, disclosed (tracked, not silently assumed done):**
- CP-W1 merchant-reference QueryStatus (item 2 above).
- Cascade-on-decline for payouts (unchanged from the original scope note in `payout.go`'s own
  package doc comment - a separate, not-yet-authorized product decision, `PAY-PAYOUT-CASCADE-1`).
- Attempt-transition audit records for every T2/T4/T6/T9/T11 transition (only T1p claim/deny,
  T12 resend/escalate, and terminal outcomes are audited) - shared gap with the deposit side,
  named but not closed by this round (independent review B8).
- M1/M2 (force-resolve of an unresolvable payout / disputed attempt) remain BLOCKED on HD-0095-1,
  unchanged.

**Verification.** 44 payout-specific tests (`payout_dispatch_integration_test.go` +
`payout_dispatch_fixround_test.go`) plus the full `internal/payments`, `internal/withdrawal`,
`internal/kyc` suites and `internal/httpserver`'s 30 withdrawal tests pass under
`-tags=integration` on a private database freshly migrated to head (0105); the payout/concurrency
subset also passes under `-race`. Mutation evidence (10 payout-specific mutants across both review
rounds, each killed and reverted to byte-identical source) is filed at
`docs/plans/payment-readiness/evidence/prh-i1-mutation-kill.txt`. The migration-0101 pre-flight
remediation runbook ledger-finance's review required is filed at `docs/runbooks/migration-0101-
payment-attempts-remediation.md`.

**N5 correction (RV-PRH-I1 re-review, 2026-09-27): the "44 tests" count above is wrong.** The
actual count at the end of the round this section describes was 14 (`payout_dispatch_integration_
test.go`) + 15 (`payout_dispatch_fixround_test.go`) = 29 payout-specific tests, not 44 - see §27.12
below for the corrected running total. Left uncorrected in place (append-only) rather than edited,
per the no-fake-completion rule's own spirit: the mistake and its correction should both be
visible, not silently smoothed over.

### 27.12 PRH-I1 payout dispatch round 3 (`payments`, 2026-09-27): N1/R6, R1, R2, R3, N6

**Scope.** A third review round (`code-reviewer`, "NOT READY, narrow rework"; `ledger-finance`,
"APPROVE WITH CONDITIONS", veto on C1 lifted) found one new HIGH finding shared between both
reviews (N1 = R6: the ambiguous branch never stored the provider reference on the attempt itself,
only on the withdrawal) plus three more from the ledger re-review alone (R1: routine non-definite
evidence racing a faster piece of evidence - most commonly a callback - was disputed instead of
converging; R2: `/resolve` had no lease check and could force a still-in-flight `submitting`
attempt to `ambiguous`; R3: phase C's lock order regressed to attempt-then-withdrawal after the
earlier M3 fix, a genuine deadlock risk against the receipt path/T2/T12's withdrawal-then-attempt
order) and one observational finding from the code re-review (N6: a QueryStatus success echoing a
DIFFERENT, non-empty reference from the one already on file was never disputed). Filed at
`docs/plans/payment-readiness/rv-prh-i1-payout-code-review.md` ("Re-review — fix round") and
`rv-prh-i1-payout-ledger.md` ("Re-review (fix round)"). Full fix detail and the anchored mutation
evidence for this round are in `docs/plans/payment-readiness/evidence/prh-i1-mutation-kill.txt`
("PRH-I1 payout round 3").

**Corrections to §27.11 above:**
- Item 1's "poll first, unconditionally, if a reference exists at all" claim was **false in
  practice**: the reference from an Ambiguous result was only ever attached to
  `withdrawal_requests`, never to `payment_attempts` itself, so `resubmitPayoutAmbiguous`'s own
  `attempt.ProviderReference != nil` gate meant the poll-first step was UNREACHABLE for the single
  most common ambiguous shape (a sync Ambiguous result with a reference, from T1p or T2). Fixed
  this round (N1/R6): the reference is now persisted directly on the attempt
  (`payoutMarkAmbiguousFromSubmitting`), and `resubmitPayoutAmbiguous`/`PollPayoutStatus` no longer
  gate on the attempt's own reference alone (`PollPayoutStatus` resolves a withdrawal-level
  fallback too, defence in depth for any attempt that reached `ambiguous` before this fix).
- The verification counts in §27.11 ("44 tests", "10 mutants") were overstated - see the N5
  correction immediately above.

**Fail-closed choices made where this ADR is silent, this round:**

6. **A non-definite piece of evidence (Pending/Ambiguous/NotSent/a transport error) CAS-conflicting
   against an attempt some FASTER piece of evidence already advanced.** Neither this ADR nor the
   original §27.11 record distinguishes "this conflict is a genuine contradiction" from "this
   conflict is just routine convergence, because something else got there first." Chosen: only a
   DEFINITE result (`Succeeded`/`DefiniteDecline`) reaching an ALREADY-TERMINAL attempt is treated
   as late/contradicting evidence (T14/T10, a P1 audit record); every other conflict either
   reschedules (the attempt is still non-terminal - the routine race) or no-ops (the attempt is
   already terminal and the arriving evidence is weaker-or-equal - nothing left to correct). This
   matters concretely once payout callbacks are wired (the callback cutover's own concurrent work):
   a verified `pending` webhook arriving before phase C's own synchronous `Withdraw` response is now
   an everyday, non-alarming race, not a P1.
2. **A `submitting` attempt whose lease has not yet expired, polled by a caller with no natural
   lease-respecting gate of its own.** The sweeper never reaches a `submitting` row until its lease
   is due (`claimBatch`'s own `next_action_at` predicate), but `internal/httpserver`'s `/resolve`
   handler has no such gate. Chosen: `PollPayoutStatus` refuses immediately
   (`ErrPayoutDispatchInFlight`, mapped to HTTP 409) and touches nothing at all in that case, rather
   than forcing the attempt to `ambiguous` and racing the dispatch that may still be in flight.
3. **The A7 lock order for phase C, restated precisely.** §27.11 item 4 said "A7's attempt-before-
   posting lock order" without stating the FULL order; this round's own implementation had, in the
   meantime, dropped the leading withdrawal lock entirely (a regression the ledger re-review
   caught via a real 40P01 deadlock probe). Restated and fixed: **withdrawal `FOR UPDATE` first,
   then the attempt CAS, then the L3/L4 posting** - the same order the T2/T12 claim statements and
   the receipt path already use. A new `withdrawal.LockForPayoutEvidence` (state-precondition-free,
   payments-only) is the one lock every phase-C entry point now takes as its first statement.
4. **A QueryStatus success echoing a reference that conflicts with (not merely omits) the one
   already on file.** §4.4's matrix does not separately name "reference conflict" as its own
   evidence shape. Chosen: dispute (`terminal_reason = "provider_reference_mismatch"`), the same
   fail-closed default as the amount/asset mismatch (H2/B3) - a settlement is never recorded against
   a reference the platform did not itself request confirmation for.

**Newly registered launch condition (R5, tracked by the orchestrator, not implemented here):** any
payment provider whose manifest declares `IdempotentSubmission = false` must not be enabled for
real traffic until CP-W1 (merchant-reference `QueryStatus`) exists, or an equivalent operational
mitigation (e.g. mandatory manual resolution SLA) is agreed - a payout that crashes/times out after
T1p with no reference and a non-idempotent provider is fail-closed escalated (T6, then T16) but has
NO automated resolution path today, only M2 (BLOCKED on HD-0095-1) or a human confirming the
outcome with the PSP directly.

**Not implemented, disclosed (unchanged from §27.11, still open):** CP-W1 merchant-reference
QueryStatus; payout cascade-on-decline (`PAY-PAYOUT-CASCADE-1`); full attempt-transition audit
coverage; M1/M2 BLOCKED on HD-0095-1. R4 (payout callbacks through the receipt path) is explicitly
a separate agent's work (`receipt.go`, not touched here) and is tracked by the orchestrator, not by
this record.

**Verification.** 41 payout-specific tests (13 + 15 + 13 across `payout_dispatch_integration_
test.go`, `payout_dispatch_fixround_test.go`, and the new `payout_dispatch_round3_test.go`) pass
under `-tags=integration` and under `-race` on a private database freshly migrated to head (105);
`internal/withdrawal` and `internal/httpserver`'s 30 withdrawal tests pass on the same private
database (`internal/httpserver`'s run against the shared `TEST_DATABASE_URL` still fails 4 of them
on the pre-existing, disclosed migration-0101 gap documented in the runbook - confirmed unrelated
by the private-database run passing all 30). `golangci-lint` (2.9.0, `--build-tags=integration`): 0
issues on every file this round touched. 3 anchored mutants for this round (PM-PAYOUT-11/12/13),
each reverted to byte-identical source - full detail in the mutation-kill evidence file.

**Correction (RV-PRH-I1 re-review 2, 2026-09-27): N3 was NOT fixed by this round, despite this
record's own framing implying the round-3 finding set was closed.** N3 (`/resolve`'s staff audit
committing in a SEPARATE transaction after `PollPayoutStatus` had already committed the state
change - the same class of defect as B6) was correctly identified by the ORIGINAL code review as a
LOW, and the ledger re-review's own "Remaining conditions" explicitly deferred it as a follow-up,
not a round-3 requirement - but this ADR record did not carry that deferral forward explicitly
enough, and the re-review read it as a closed item. It was not touched in round 3's diff at all:
`newResolveWithdrawalHandler` still called `audit.Record` in its own separate
`deps.DB.WithTenant(...)` block, after `PollPayoutStatus` had already returned successfully. Fixed
in round 4 - see §27.13.

### 27.13 PRH-I1 payout dispatch round 4, narrow (`payments`, 2026-09-27): N7, N3, N6 extension, R1 LOW

**Scope.** A fourth, narrow re-review (`code-reviewer`, "NOT READY") found one HIGH regression of
H3/B2 (N7) plus confirmed N3 was still open (the §27.12 correction immediately above), a LOW
request to pin R1's other branch with a test, and an extension to N6 (compare against the fallback
withdrawal reference too, not only the attempt's own). Filed at
`docs/plans/payment-readiness/rv-prh-i1-payout-code-review.md` ("Re-review 2"). Full fix detail and
the anchored mutation evidence for this round are in `docs/plans/payment-readiness/evidence/
prh-i1-mutation-kill.txt` ("PRH-I1 payout round 4").

**N7 (HIGH, a regression of H3/B2).** `PollPayoutStatus`'s R2 in-flight guard
(`attempt.LeaseUntil.After(time.Now())`) could not distinguish a genuinely live DISPATCH lease
(set once, by `ClaimForDispatch`'s own `InsertSubmittingAttempt`, `lease_owner = "payout-dispatch"`)
from the SWEEPER'S OWN fresh batch-claim lease (`claimBatch` sets `lease_owner = 'sweeper'` and a
brand-new, future `lease_until` on EVERY row it claims, including a crashed, long-expired
`submitting` payout with no reference - H3/B2's own crash-recovery case - immediately before
routing it to `resolvePayoutViaQueryStatus`/`PollPayoutStatus`). With `Sweeper{}`'s zero-value
`Lease` (every payout test in this package until this round), the freshly-set `lease_until` was
`now() + 0`, already in the past by the time the guard ran, masking the bug entirely. With a REAL,
non-zero `Lease` (`NewSweeper`'s own default, `SweeperDefaultLease = 60s` - what any real deployment
would actually run), the guard wrongly refused the sweeper's own recovery attempt every tick
(`claimed=1, processed=0, ErrPayoutDispatchInFlight`), permanently reproducing H3/B2's exact
"crashed payout never recovered" failure mode this platform had already fixed once. Fixed by
checking `attempt.LeaseOwner` too: only a lease NOT owned by `"sweeper"` can mean a live dispatch is
in flight; `payoutMarkAmbiguousFromSubmittingIfLeaseExpired`'s own defence-in-depth CAS predicate
carries the identical `lease_owner = 'sweeper'` exemption. The doc comment that incorrectly claimed
"the sweeper never hits this path" (in two places: `PollPayoutStatus`'s own doc and
`ErrPayoutDispatchInFlight`'s) is corrected. Every payout test in this package that constructs a
`&Sweeper{}` literal now sets an explicit `Lease: SweeperDefaultLease`, and a new dedicated test
(`TestSweeper_N7_CrashRecoveryWithRealLease_ActuallyRecovers`) uses `NewSweeper` itself and asserts
`Processed == 1`, not merely `Claimed == 1` - the exact assertion gap that let the original,
zero-Lease tests mask this for two full review rounds.

**N3 (fixed for real this round).** `/resolve`'s staff-attribution audit
(`withdrawal.resolve_attempted.http`) now commits inside the SAME transaction as
`PollPayoutStatus`'s own state change, for every branch (the no-reference fallback AND the full
QueryStatus evidence matrix) - `PollPayoutStatus`/`applyPayoutStatusEvidence` both now take an
`actor *SubmitActor` parameter (nil for every sweeper-driven call, which has no staff to attribute
to and writes no audit row), and `applyPayoutStatusEvidence`'s existing evidence-mapping switch was
extracted verbatim into `applyPayoutStatusEvidenceInTx` so the audit call sits ONCE, after it,
inside the same `pool.WithTenant` closure, rather than duplicated into every one of that switch's
many return points. `internal/httpserver`'s `/resolve` handler no longer writes its own,
separate-transaction audit record at all.

**N6 extension.** The reference-mismatch check now compares against the attempt's own stored
reference if it has one, else (the N1/R6 fallback shape - an attempt that reached `ambiguous`
before that fix persisted the reference directly on it) the withdrawal's own - the original N6 fix
only ever compared against `attempt.ProviderReference`, which is nil in exactly the fallback shape
N1/R6 exists to cover.

**LOW: R1's "already terminal" branch, pinned.** `TestPayoutDispatch_R1_
StrayEvidenceAgainstTerminalAttempt_IsNoOp` proves a stray non-definite result arriving for an
attempt that already reached a terminal state is a pure no-op (no error, no state change, no
duplicate dispute/audit) - the other half of R1's branch (a non-terminal attempt racing a faster
piece of evidence) was already covered by the round-3 R1 tests.

**Not implemented, disclosed (unchanged):** CP-W1, payout cascade, full attempt-transition audit
coverage, M1/M2 BLOCKED, R4 (callback agent's own scope, untouched here).

**Verification.** 8 new tests this round
(`payout_dispatch_round4_test.go`), for a running total of 49 payout-specific tests, pass under
`-tags=integration` and under `-race` on a private database freshly migrated to head (106);
`internal/withdrawal` and `internal/httpserver`'s 30 withdrawal tests pass on a private database
freshly migrated to head (the shared `TEST_DATABASE_URL` run still fails on the same pre-existing,
disclosed migration-0101 gap, confirmed unrelated). `golangci-lint` (2.9.0,
`--build-tags=integration --allow-parallel-runners`): 0 issues on every file this round touched.
### 27.14 PRH-I1 callback-cutover fix round (`payments`, 2026-09-27): H1 misclassification and revert, R4 payout receipt evidence, F5 mutant kills, corrections to §27.10

**Scope.** A fix round responding to two independent reviews of the §27.10 cutover
(`docs/plans/payment-readiness/rv-prh-i1-callback-ledger.md`, REJECT with a veto on money paths;
`docs/plans/payment-readiness/rv-prh-i1-callback-code-review.md`, NOT READY), plus a follow-on
ledger-finance payout re-review (`rv-prh-i1-payout-ledger.md`) covering the receipt path's payout
branches. All changes are confined to `internal/payments/receipt.go`, `drive.go`, `sweeper.go`,
`attempt.go`, `orchestrator.go`, `cascade.go`, and test files - the payout state machine itself
(`payout.go`/`payout_sweep.go`/`withdrawal.go`) was owned and fixed concurrently by another agent
(payout round 3, §27.12 continuation, not re-described here).

**H1: a genuine bug was introduced, caught by the orchestrator's own `-race` run, and reverted.**
The ledger-finance review's H1 finding read `applyReversalReceiptEvidence`'s unconditional
`ev.Outcome = OutcomeSucceeded` normalization as "a reversal whose wire outcome is not `succeeded`
still debits player_cash and is falsely stored as succeeded" and required gating posting/
tombstoning on the wire outcome actually being `succeeded`. That fix was implemented and initially
accepted. It was **wrong**: on this codebase's actual adapter contract, `MockProvider.HandleCallback`
requires the wire `Outcome` field to be one of `succeeded`/`declined`/`ambiguous`/`pending` for
EVERY event (an unrecognized value is `ErrCallbackMalformedBody`, never silently accepted), and
§27.10(a) itself already documented that the field is "historically reused by some callers as a
chargeback-reason carrier (`declined` + `DeclineReason: "chargeback"`)" for a reversal that DID
happen and must post - there is no distinct "the attempted reversal itself failed" concept
expressible on this wire shape today. The H1 gate broke six passing tests
(`TestReceiveCallback_DepositReversalPostsFlow2`, `TestReceiveCallback_ReversalAmountCannotExceedOriginal`,
`TestReceiveCallback_SecondReversalOfSameDepositRejected`,
`TestReceiveCallback_ReversalOfNeverPostedDepositWritesTombstone`,
`TestProviderRefBound_Payments_OversizeRejectedNothingWritten_ExactMaxAccepted`,
`TestF7Payments_SequentialReversalRedeliveryIsIdempotent`), caught only because the coordinator ran
the full suite under `-race` against a private database before merging. **Reverted**: the
unconditional normalization is restored exactly as §27.10(a) originally described it;
`applyNonSucceededReversalEvidence` (the gate) was deleted entirely. The two regression tests this
round's earlier commit had added under the wrong assumption (`TestRVLF_P1_...`,
`TestRVLF_P1b_...`) were rewritten to pin the CORRECTED behavior instead of deleted, so the
regression this round almost shipped stays visible in test history rather than being quietly
un-tested. **At the time of this record, ledger-finance's ruling on whether this revert is correct,
and on alignment with reconciliation's own `matchReversal` handling of the same field, is still
pending** - this record states the revert as implemented and test-verified, not as
ledger-finance-approved.

**H3 (security review `rv-prh-i1-killswitch-security.md`): all three cascade-insert sites tested,
not just the callback's.** The kill-switch downgrade (`insertCascadeAttemptIfEligible`,
`cascade.go`) was already wired into `receipt.go` (the callback), `drive.go` (a synchronous decline
for a call already sent), and `sweeper.go` (a poll result that is a decline) - but only the
callback site had a regression test. Added `TestRVLF_H3_DriveGo_SyncCascadableDeclineUnderKillSwitch`
and `TestRVLF_H3_SweeperGo_PollCascadableDeclineUnderKillSwitch`, each asserting the decline commits,
no cascade child is created, and the `payment.cascade_skipped_kill_switch` audit record exists.
drive.go's site required calling `applyDepositCallResult` directly against an attempt already
claimed/dispatched BEFORE the switch was engaged (T2's own claim CAS correctly refuses a brand-new
dispatch while engaged, so the ordinary `InitiateDepositAttempt` entry point cannot exercise "a
decline for a call already sent" under an engaged switch within one synchronous test call).

**R4 (ledger-finance payout re-review): the receipt path's payout branches, all in `receipt.go`.**
`applyResolvedReceiptEvidence`'s payout-operation branches previously (a) declined a payout attempt
via the bare, attempt-only `ApplyDecline` without ever calling `withdrawal.Fail` - stranding the
released hold - and (b) parked every payout success as an unresolved anomaly
(`"payout_success_not_yet_wired"`), never settling the withdrawal. Fixed: both branches now call
`payout.go`'s own `applyPayoutDecline`/`applyPayoutSuccess` - the SAME tx-scoped functions
`ApplyPayoutResult`'s dispatch-path branches use - so the hold is released or settled exactly once,
never duplicated by a second, receipt-path-only implementation. Also added: an event_type-vs-
attempt.Operation cross-check in `ApplyReceiptEvidence` (a "deposit"-typed event resolving to a
payout attempt, or the reverse, is now an anomaly with no effect - neither state machine is ever
touched by a mislabeled event), and widened `insertReceiptDeduped`'s decline-column population
(`decline_stage`/`cascadable`) to cover `EventType == "payout"`, not only `"deposit"` - the previous
condition left those columns NULL for a payout decline while still storing `outcome='declined'`,
which `payment_provider_events_check1` rejects outright (a permanent redelivery loop). No migration
was needed for this: the CHECK constraint already accepts a decline with those columns populated:
the bug was that the populating code excluded payout-typed events, not that the constraint itself
was wrong. Tests: `TestRVLF_R4a_PayoutDeclineViaReceiptPathReleasesHold`,
`TestRVLF_R4d_PayoutSuccessViaReceiptPathSettles`, `TestRVLF_R4b_EventTypeOperationMismatchIsAnomaly`.
Because payout round 3 (concurrent, §27.12 continuation) subsequently changed
`applyPayoutSuccess`/`applyPayoutDecline` to lock the withdrawal row first
(`withdrawal.LockForPayoutEvidence`) before the attempt CAS, reusing those functions here means the
receipt path's payout branches automatically inherited the corrected withdrawal-then-attempt lock
order with no further change required in `receipt.go`.

**F5 (independent code review) surviving-mutant kills, each with a new/edited passing test in
`internal/httpserver`:**
- **M10** (cap boundary `n > DeferredReceiptCap` vs `n >= DeferredReceiptCap`): the existing cap
  test only filled `cap+1` rows, which both operators reject identically.
  `TestPaymentWebhook_DeferredReceiptCapExceeded_ExactBoundary` fills exactly `cap` rows and asserts
  the next delivery is still ACCEPTED - only `>` accepts at the exact boundary.
- **M11** (uniform-response header leak): `TestPaymentWebhook_UniformResponseAcrossDispositions`'s
  header check was a hardcoded 6-name deny-list (`X-Disposition`, `X-Payment-Disposition`, ...) that
  would never catch a mutant introducing some OTHER new header. Replaced with a full allow-list/
  key-set-equality comparison across every recorded disposition's response headers.
- **M12/M13** (reversal amount-mismatch HTTP path unpinned): no HTTP-level test exercised
  `applyReversalReceiptEvidence`'s amount/asset mismatch branch (`ErrCallbackProviderMismatch`) at
  all. Added `TestWebhook_ReversalAmountMismatchAfterVerification_Maps400`: asserts 400/"callback
  rejected", no ledger effect, and no raw amount/reference leakage in any captured log line.
- Restored the log-redaction assertion `TestWebhook_ProviderMismatchAfterVerification_
  IsDisputedNotRejected` had silently dropped (a bare `_ = captured` placeholder) when that test
  moved from asserting a 400/rejected response to a 200/disputed one - see the correction to
  §27.10's "no test was deleted, skipped, or weakened" claim below.

**Corrections to §27.10 above (stated openly, per the no-fake-completion rule's own precedent at
§27.11/§27.12's N5 correction - the original text is left in place, not edited):**
- **"No test was deleted, skipped, or weakened" is inaccurate.** F5's independent re-review found
  that `TestWebhook_ProviderMismatchAfterVerification_Maps400`'s replacement,
  `..._IsDisputedNotRejected`, dropped its predecessor's log-redaction assertion entirely (replaced
  with `_ = captured`, an unused-variable placeholder) rather than adapting it to the new 200/
  disputed response shape. This WAS a weakened assertion, restored this round (see F5 above). No
  other test's assertion strength was found to have regressed on re-review.
- **§27.10(a)'s locking/ordering claim is inaccurate for a reversal naming a payout reference.**
  §27.10(a) describes "a resolved attempt is locked... before the integrity check" as the general
  case; what it does not say is that, PRIOR to this round's M4/F4 fix, the not-a-deposit integrity
  check (`original.Operation != AttemptOperationDeposit`) ran AFTER, not before, the tombstone
  branch (`unresolved || original.LedgerTransactionID == nil`) - and a payout attempt never carries
  a `LedgerTransactionID` linking a captured DEPOSIT, so that ordering made the integrity check
  permanently unreachable for any reversal naming a payout's own provider_reference, silently
  tombstoning it instead (later making a genuine `withdrawal.Complete` for that reference fail
  forever, looking up an idempotency key that could never exist). Fixed this round: the integrity
  check now runs first. `TestRVLF_M4_ReversalNamingPayoutReferenceNeverTombstoned` pins the
  corrected order.
- **The evidence file's "6 scenarios" claim for `TestPaymentWebhook_UniformResponseAcrossDispositions`
  overstates the number of DISTINCT dispositions actually exercised at the time.** F5's re-review
  found that the test's "anomaly_mismatch" and "tombstone_collision" cases both produce
  `DispositionApplied` (a T10 dispute still counts as `changed=true` in `ApplyReceiptEvidence`'s own
  accounting), so only 3 distinct disposition VALUES (`applied`, `duplicate_effect`,
  `deferred_unresolved`) were ever actually sent over HTTP by that test, not the implied 6 - no case
  produced a real `DispositionAnomaly` response. Not fixed as part of this round's F5 work (recorded
  here as still open, not silently corrected away): making one of those cases produce a genuine
  `anomaly` disposition (e.g. the precondition-2 cross-attempt-reference-conflict path, or the R4(b)
  event_type/operation cross-check added this round) remains a small follow-up.
- **The evidence file's "migrations 1-104" citations are stale.** Both full-suite confirmation runs
  quoted in §27.10/§27.10's round-2 addendum ("migrations 1-104") predate migrations 0105 (kill
  switch), 0106 (kill-switch RLS hardening), and this round's own verification, which all ran on a
  private database migrated to head including 0106. No migration was added by this round itself (see
  the R4/H1 notes above - both turned out fixable in application code once correctly diagnosed);
  migration 0107 remains reserved and unused for a possible future DB-trigger mirror of the H4 T2
  sibling-succeeded guard (optional defence in depth, not attempted this round).
- **§27.10's round-2 addendum lists `TestReceiveCallback_AmbiguousCallbackResolvedViaQueryStatus_NotCascaded`
  among tests "adapted with strictly equal-or-stronger assertions" alongside two genuinely
  equal-or-stronger adaptations.** On re-review this one is different in kind, not merely degree: the
  pre-cutover version's own intent (synchronous, in-process `QueryStatus` resolution of an ambiguous
  attempt) is no longer expressible at all post-cutover (§6.5 forbids any provider I/O from a
  callback), so the adapted assertion (zero provider calls plus a scheduled `next_action_at` for the
  sweeper) verifies a DIFFERENT behavior, not a strictly stronger one against the same original
  intent. Relabeled here as a changed-intent adaptation, not an equal-or-stronger one - the
  distinction matters because "equal-or-stronger" implies nothing was lost, and in this one case the
  original synchronous-resolution code path this test exercised no longer exists to be tested at all
  (it was intentionally removed by the cutover, per §6.5, not merely rewritten).

**L4 (receipt field fidelity, ledger-finance L4/L5): tombstone receipts now store the reversal's
real amount/asset instead of zero-value placeholders.** The tombstone branch of
`applyReversalReceiptEvidence` builds its `ReceiptEvidence` before any original attempt is known to
exist, so `ev.Amount`/`ev.AssetCode` were left at their zero values (`0`/`""`) even when the wire
payload declared real ones - `insertReceiptDeduped` only ever populates `amount`/`asset_code` for
`OutcomeSucceeded` evidence regardless, by design (§4.4 concepts), so this was cosmetic (never a
financial-effect bug) but made the persisted receipt harder to reconcile by hand. Not changed this
round in the disposition-affecting sense; `docs/plans/payment-readiness/evidence/prh-i1-mutation-kill.txt`
records the mutation-kill detail for the receipt-fidelity test added.

**L2 (bridge support for multiple attempts per intent).** `receive_bridge_integration_test.go`'s
`backfillAttemptForIntent` previously built exactly one attempt per intent, so no migrated test
exercised a genuine T13 second capture, or a cascade child left `created`, through the bridge. Added
`backfillCascadeAttemptsForIntent`, which builds a real two-attempt shape through the SAME exported
T1/T2/T4/T8 transitions the live cascade path uses (never through the legacy `InitiateDeposit`'s own
in-process cascade recursion, which has no way to report which intermediate provider/reference it
tried and declined): attempt 1 goes `created`->`submitting`->`declined` (cascadable) via a REAL
provider claim, attempt 2 is the cascade child inserted via `cascade.go`'s own `insertCascadeAttempt`
and deliberately left `created` (unclaimed). `TestRVLF_L2_BridgeCascadeThenT13SecondCapture` then
delivers a genuine T13 success on attempt 1's own reference and confirms attempt 2 is rejected
(`intent_succeeded`) and unclaimable - H4's guard exercised through the bridge's own multi-attempt
shape, not only through a live-provider-driven cascade.

**F3 (setIntentAttempt's provider_id/provider_reference sticky-guard extension) - mutation attempted,
NOT killed; disclosed rather than claimed.** The obvious regression test (a redelivered decline on an
already-terminal sibling after another sibling succeeded) turned out not to exercise this guard at
all: `applyResolvedReceiptEvidence`'s `OutcomeDeclined` branch short-circuits to a no-op for any
attempt that is not `submitting`/`pending`/`ambiguous`, BEFORE ever calling `finalizeDeclined`/
`setIntentAttempt` - a terminal sibling's redelivered decline never reaches the mutated code at all.
A rewritten test constructing two SIMULTANEOUSLY-LIVE attempts under one intent (the only way to
reach `setIntentAttempt` with a genuine FIRST-time decline after a DIFFERENT sibling already
succeeded) failed at setup with a `payment_attempts_one_live_per_intent` unique-constraint violation
(migration 0101). **Conclusion, stated openly:** for deposits, the exact sequence this guard's
provider_id/reference extension protects against is architecturally unreachable today - only one
attempt per intent may ever be live at once, and a sibling can only reach `succeeded` (via T13) after
the attempt it supersedes is already terminal, never while a DIFFERENT live attempt is mid-decline.
The fix is retained as defence in depth (it costs nothing and closes a theoretical gap if that
constraint is ever relaxed), but this record does NOT claim it is proven to close a live bug, unlike
every other item in this section. Full transcript (including the exact failed alternative attempt) is
in `docs/plans/payment-readiness/evidence/prh-i1-mutation-kill.txt`.

**L4 (receipt field fidelity, ledger-finance L4/L5): tombstone receipts now backfill the reversal's
real amount/asset from the original attempt when the wire payload omits them.** The tombstone branch
of `applyReversalReceiptEvidence` builds its `ReceiptEvidence` before any original attempt is known to
exist, so `ev.Amount`/`ev.AssetCode` were left at their zero values (`0`/`""`) even when a
resolved-but-never-posted original attempt (this branch's own case) already knows the true ones -
some PSPs' "reverse the whole deposit" chargeback shape never states either on the wire. Fixed: when
an original attempt DID resolve (not the genuinely-unresolved case, where there is nothing to backfill
from) and the wire left a field unset, it is backfilled from the original attempt's own declared
value before the receipt is stored - never overwrites a wire-declared value.
`TestRVLF_L4_TombstoneReceiptBackfillsAmountAssetFromOriginalAttempt` pins this.

**Verification.** `internal/payments` fully green under `-race -tags=integration` on a private
database migrated to head (through migration 0106), repeated runs, no flakes. `internal/reconciliation`
green. Every payments/webhook/payout/kill-switch/reversal-named test in `internal/httpserver` (83
tests) green under `-race`. `internal/httpserver`'s `TestResolutionIsolation_*` family is
independently confirmed flaky under `-race` load regardless of this round's changes (same file,
untouched by this round; 2-5 of its ~8 subtests fail non-deterministically even run in isolation,
repeatably, all real-time latency-threshold assertions sensitive to race-detector overhead) - not a
regression introduced here, disclosed rather than silently excluded. `golangci-lint` (pinned 2.9.0
binary, untagged, exactly as CI runs it): 0 issues. `gofmt`: clean. Mutation-kill transcripts for
this round's fixes (H2, H3 x3 sites, H4, M1, M4, F2, R4(a)/(b)/(d) - 10 of 11 attempted, F3 disclosed
as not killed above) are filed in `docs/plans/payment-readiness/evidence/prh-i1-mutation-kill.txt`
("PRH-I1 callback cutover fix round, 2026-09-27"). The F2 mutation-kill exercise itself surfaced a
genuine bug this round introduced and fixed in the same commit: `boundedDeclineReason` was only
applied inside `applyResolvedReceiptEvidence`'s `OutcomeDeclined` branch, AFTER the R0 receipt insert
(ADR 0082 A7) had already run with the raw, unbounded `ev.DeclineReason` -
`payment_provider_events.decline_reason` carries the identical 64-byte CHECK as
`payment_attempts.decline_reason`. Fixed by bounding once, at the top of `ApplyReceiptEvidence`.

**Not implemented, disclosed:** the DB-trigger-level mirror of H4's T2 sibling-succeeded guard
(optional defence in depth; migration 0107 reserved, unused). The "6 scenarios" evidence-file
overstatement noted above (still 3 distinct disposition values demonstrated over HTTP, not yet a
real `anomaly` case). ~~H1's ledger-finance ruling on the revert is pending at the time of this
record; this section will need its own follow-up correction if that ruling disagrees.~~
**[SUPERSEDED - see the FH-5 follow-up below: ledger-finance's binding ruling arrived and the
revert described above was itself further corrected.]** F3's mutation was not killed (see above) -
retained as defence in depth, not as a proven fix for a reachable bug.

**FH-5 follow-up (`payments`, 2026-09-27): ledger-finance's binding H1 ruling and M1, N2-N4,
security S-H1/S-M1** (Financial Hardening workstream, `docs/plans/payment-readiness/rv-prh-i1-
callback-ledger.md` "Re-review 1: fix round", and `rv-prh-i1-payout-security.md`).

- **H1 is further corrected, not merely reverted.** The straight revert described above (every
  wire outcome posts identically) was itself a defect: ledger-finance's binding ruling draws a line
  the revert missed. **Rule 1/4**: `succeeded` and the legacy `declined`-as-reason-carrier wire
  outcomes both post/tombstone exactly as the revert already did. **Rule 2**: `pending`/`ambiguous`
  are NOT final reversal decisions and must never post or tombstone - `applyReversalReceiptEvidence`
  now branches on the real wire outcome before any lock: a non-final outcome is stored under its
  OWN real value (never normalized), disposition `anomaly`, resolution `anomaly_other`, a new P1
  audit action `payments.reversal_non_final_outcome`, and a uniform 200 (never a 4xx/5xx
  redelivery loop). `TestRVLF_P1_NonFinalReversalOutcomeNeverPosts` pins this; the pre-existing
  `TestRVLF_P1_ReversalPostsRegardlessOfWireOutcomeReasonCarrier` is narrowed to the two FINAL
  outcomes only, since asserting all four posted identically was exactly the gap this rule closes.
  **Rule 3**: `computeEventFingerprint` now uses the real wire outcome (`ReceiptEvidence.RawOutcome`)
  for a posting/tombstoning reversal, even though the STORED `payment_provider_events.outcome`
  column stays normalized to `succeeded` for that case (documented on the `ReceiptEvidence.Outcome`
  field and via `CallbackEvent.Outcome`'s own doc comment in `types.go`); the raw outcome and the
  already-bounded reason are also now carried in the `deposit.reversed`/`deposit.reversal_tombstoned`
  audit metadata. **Rule 5**: `types.go`'s `CallbackEvent`/`PaymentProvider.HandleCallback` doc
  comments now state the contract explicitly: a `deposit_reversal` event asserts a FINAL debit; an
  adapter must never emit it for a pending-refund/chargeback-inquiry-open signal; a "chargeback won"
  event has no mapping today and falls into `ReceiveVerifiedCallback`'s existing "unsupported
  callback event type" error path - PROVIDER DEPENDENT, deferred pending a real vendor contract.
  **Rule 6**: `internal/reconciliation/payment_statement.go`'s `matchReversal` needed a doc
  comment only (no code change) - its existing pending/declined statement-line branches already
  correctly reflect "no posting expected yet" for this case.
- **RULING M1 (terminal amount/asset mismatch)**: a mismatched success on an ALREADY-terminal
  attempt (succeeded OR declined) now records a P1 audit
  (`payments.callback_amount_asset_mismatch_terminal`, via the new `auditTerminalAmountAssetMismatch`
  helper) in both cells - the succeeded case used to silently treat a mismatch as an ordinary
  idempotent duplicate with no record at all, and the declined case used to silently return
  `anomaly_other` with no audit trail either. Neither cell changes state or posts.
- **N2** (drive.go/sweeper.go): a success naming an already-tombstoned reference now routes through
  the same T10 (`applyDepositCallResult`'s `ErrorClassSucceeded` branch, phase C, always
  non-terminal) / T10-or-T13t (the sweeper's `applyStatusEvidence`, which can also reach a
  `declined` attempt on a T13 re-drive) tombstone check `applyResolvedReceiptEvidence` already used,
  instead of calling `postDepositSuccess` directly and surfacing the tombstone's own unique index as
  an untyped error that would otherwise retry identically forever.
- **N3** was already closed by this round's own S-H1 fix (below) - the deferred backstop's
  `event_type` filter is derived from the resolved attempt's OWN operation (an allow-list, not a
  hardcoded `'deposit'`), so a stored `deposit`-typed receipt can never be replayed against a
  payout attempt or vice versa.
- **N4**: `rejectCreatedSiblings` now takes the caller's own `EvidenceKind` parameter instead of
  hardcoding `EvidenceCallback`, fixed at all three call sites (`receipt.go`, `drive.go`,
  `sweeper.go`).
- **Security S-H1** (`rv-prh-i1-payout-security.md`, probe SP-B2): confirmed fixed by the same N3
  change above - `TestRVLF_N3_DeferredApplyNeverReplaysADepositDeclineAsPayoutEvidence` pins the
  decline variant (the security review's own probe found the success variant).
- **Security S-M1** (probe SM10): a payout success callback echoing a DIFFERENT, non-empty provider
  reference than the one already on file (the attempt's own, falling back to the withdrawal's) now
  disputes (T10, `provider_reference_mismatch`) instead of settling, mirroring the QueryStatus
  path's own N6 rule (`payout.go`) - `TestRVLF_SM10_PayoutSuccessProviderReferenceMismatchDisputes`.
- **Verification**: `go build ./...` and `go vet -tags=integration ./...` clean across the whole
  repository. The `internal/payments` suite under `-race` against a fresh private database
  (`fh5_pay_20260927`, migrated to head including 0106/0107) and mutation-kill transcripts for this
  round's new fixes are recorded separately (see the evidence file's own FH-5 entry).
- **Deferred, on explicit coordinator instruction**: the double-credit/INV-DEP-1 fix (ADR 0095 §28,
  `docs/plans/payment-readiness/lf-q1-supersession.md`) is FH-3, a LATER phase gated on kill-switch
  phase 2's `callProvider`/`Resolve` signature changes and QA's A-O matrix. Not started here.

**Operator note (ledger-finance L-d): `disposition_at_receipt` vs `resolution` for an M1
terminal-mismatch anomaly.** When `applyResolvedReceiptEvidence` finds a terminal-mismatch (M1:
a mismatched success on an already-`succeeded` or already-`declined` attempt), the receipt row's
`disposition_at_receipt` column stays `'applied'` - it was already written as `'applied'` at R0
(the receipt insert, before the attempt was even locked and re-read; ADR 0082 A7), and nothing
about a terminal-mismatch changes that value after the fact. It is `resolution` -
`payment_provider_events.resolution`, written afterwards by `ResolveReceipt` in the SAME
transaction - that actually records the anomaly, as `'anomaly_other'`. **An operator or a
dashboard querying only `disposition_at_receipt` will see `'applied'` for this case and must not
read that as "no problem was found."** The M1 queue (a `disputed` attempt with no state change
for a terminal-mismatch cell specifically - see the audit action
`payments.callback_amount_asset_mismatch_terminal`, or `resolution = 'anomaly_other'` on the
receipt itself) is the correct signal to alert and triage on; `disposition_at_receipt` alone is
never sufficient to rule out an anomaly for ANY receipt whose attempt was already resolved before
this callback arrived. This is not a defect: `disposition_at_receipt` answers "did the receipt
pipeline apply SOME evidence-matrix cell to this event without erroring" (true here - the M1 cell
IS the applied cell, it just happens to conclude "no state change, only an audit"), while
`resolution` answers "what did that cell conclude." The two questions are different by design;
this note exists so an operator reading only one of them is not misled.

### 27.14 Revision 4 (`architect`, 2026-09-27) — AM-2, the durable-state definition, AM-1 and the status correction

This revision is authorised by the human under the Financial Hardening workstream. It adds
three sections and corrects the header, and it rewrites no earlier text: each passage it changes
is kept verbatim with an in-place *[SUPERSEDED / AMENDED by §N]* note.

| New text | What it does | Source of authority |
|---|---|---|
| Header "Current status" | Replaces "NOT IMPLEMENTED" with PARTIALLY IMPLEMENTED plus a per-part table and one authoritative migration map | `rv-prh-architect.md` §5–§6 |
| §28 AM-2 | INV-DEP-1 / PAY-DOUBLE-CREDIT-1: T7 guard, T13 first success only, new T13d, in-flight siblings, migration 0107, reconciliation, audit | Human invariant; `ledger-finance` `lf-q1-supersession.md` (`17e5ffc`); HD-LEDGER-UNALLOC-1 (`079c5f2`); `double-credit-reconciliation.md` |
| §29 | F-POOL-2 durable-state definition and the operational rules | Human requirement (Financial Hardening) |
| §30 AM-1 | Kill-switch single dual-scope route family (supersedes parts of §10.4/§10.5) | `security` ruling (`rv-prh-i1-killswitch-security.md`, condition 1); `rv-prh-architect.md` §1 |

---

## 28. Amendment AM-2 — INV-DEP-1: at most one success and one posting per deposit intent (PAY-DOUBLE-CREDIT-1)

- *[Status note 2026-09-28 (`architect`, FH-7): **IMPLEMENTED**. See §31 and the header status
  note. The final architecture verification is in
  `docs/plans/payment-readiness/rv-fh7-architect-final.md`. The "NOT IMPLEMENTED" on the next
  line is the 2026-09-27 design-time status, kept verbatim. Option (B) of §28.13 stays NOT
  IMPLEMENTED by decision.]*
- **Status:** ACCEPTED (design). **NOT IMPLEMENTED.** Owner of the state machine: `architect`.
  Financial invariants, the ledger schema and the accounting treatment are ruled by
  `ledger-finance` in `docs/plans/payment-readiness/lf-q1-supersession.md` (`17e5ffc`); this
  section restates those rulings and does not override them. If this section and that ruling
  ever disagree on a financial point, the ruling governs.
- **Trigger:** registry PAY-DOUBLE-CREDIT-1 (HIGH); analysis in
  `docs/plans/payment-readiness/double-credit-reconciliation.md`.
- **Implementers:** `payments`/`backend` (state machine, choke point), `ledger-finance` (ledger
  index, sentinel, migration 0107 ledger half), `qa` (§28.12), `security` (§28.11).

### 28.1 The old behaviour and why it is unsafe

The old behaviour, as specified by §4.3 T13 and §21.2 (LF-Q1) and as built:

- **T13** (`declined → succeeded`, deposit) posted Flow 1 to `player_cash` even when another
  attempt of the same intent had already succeeded. This was the "second capture". It raised P1
  `multiple_success_for_intent` and wrote `deposit.second_capture_posted`.
- **T7** (`submitting/pending/ambiguous → succeeded`) posted without checking whether the intent
  was already resolved. `postDepositSuccess` then took its second-capture branch.
- **§20** accepted a further residual: a sibling already `submitting` could capture a third
  time.

Why it is unsafe (`ledger-finance` ruling §1):

1. **One purchase intent is credited more than once.** The player asked to deposit X once and
   receives 2X or 3X of withdrawable `player_cash`. The P1 arrives after the money can already
   be wagered or withdrawn. The correction path (LEDGER-MANUAL-ADJ-4EYES-1) is BLOCKED, so
   "detect and correct" means "detect and hope".
2. **It confuses evidence with authorization.** A second capture is a PSP or cascade
   malfunction, or a player paying twice by mistake. It is not an instruction to fund the
   wallet.
3. **No concurrency is needed.** A fallback succeeds, then the original's late success
   arrives. That is purely sequential, so it is a state-machine rule, not a race that locking can
   fix.
4. **It trades a recoverable hold for an unrecoverable over-credit,** which is the wrong
   direction for a fail-closed ledger.

It was introduced by this ADR's F-POOL-2 design. Stage 10.3 under-credited such a capture, but
never double-credited it (`double-credit-reconciliation.md` §2).

### 28.2 INV-DEP-1 (binding)

> For every `deposit_intents` row there is at most ONE `payment_attempts` row with
> `operation = 'deposit'` in state `succeeded`, and at most ONE `ledger_transactions` row with
> `transaction_type = 'deposit'` and `correlation_id = deposit_intents.id`. Once an intent has
> a succeeded attempt or a deposit posting, it is **financially resolved for ever**. A
> `deposit_reversal` or a tombstone does not reopen it. A verified provider success never, by
> itself, authorizes a posting for a financially resolved intent.

**Definition used by every check below.** For an intent `I`, a candidate attempt `A` (NULL on
the legacy intent-without-attempt path) and a candidate ledger idempotency key `K`
(`provider_id:provider_reference`):

```
resolved_for_other(I, A, K) :=
     EXISTS (SELECT 1 FROM payment_attempts
              WHERE tenant_id = I.tenant_id AND deposit_intent_id = I.id
                AND operation = 'deposit' AND state = 'succeeded'
                AND id IS DISTINCT FROM A)
  OR EXISTS (SELECT 1 FROM ledger_transactions
              WHERE tenant_id = I.tenant_id AND transaction_type = 'deposit'
                AND correlation_id = I.id AND idempotency_key <> K)
```

An exact redelivery of the posting success (same attempt, same `K`) is therefore **not**
"resolved for other". It keeps today's replay semantics: `succeeded × succeeded(match)` is a
no-op in the matrix, and `ledger.Post` returns `AlreadyPosted`.

**Contract amendment (binding; `ledger-finance` ruling §3(ii)).** For
`transaction_type = 'deposit'`, `correlation_id` IS the `deposit_intents.id`. Any future deposit
vehicle not driven by an intent must mint a real intent first, or use a distinct
`transaction_type`. An example is unsolicited crypto deposits to a custodial address (ADR 0008).
`architect` records this as a precondition on the ADR 0008 custody deposit design.

### 28.3 Single choke point

`(*Orchestrator).postDepositSuccess` (`internal/payments/orchestrator.go`) is the only
production `TxDeposit` poster (`ledger-finance` checked every `ledger.TxDeposit` use). Every
caller already holds `deposit_intents … FOR UPDATE`:

- the receipt path (T7, T13);
- phase C (`drive.go`);
- the sweeper poll;
- T17 re-drive;
- the legacy `InitiateDeposit`.

Rules:

1. **Check order inside the evidence application**, all in the same tx and under the intent
   lock (the existing ADR 0082 A7 order: parent → attempt; no new lock):
   1. the §4.4 preconditions (provider binding, reference conflicts);
   2. amount/asset mismatch → T10 (mismatch reasons, unchanged);
   3. **tombstone** on `(provider_id, provider_reference)` → T10/T13t
      `reversal_tombstone_precedes_success` (unchanged, LF95-C6(d));
   4. **`resolved_for_other(I, A, K)` → the no-post branch (§28.4)**;
   5. otherwise post (Flow 1) and apply T7/T13.

   The tombstone check precedes the INV-DEP-1 check deliberately. A tombstone means the PSP
   reversed that capture, which nets to zero, so it must not be reported as
   `pay_captured_unposted` (§28.9).
2. `postDepositSuccess` itself re-evaluates `resolved_for_other` immediately before
   `ledger.Post`. On true it posts nothing and returns the typed sentinel
   **`payments.ErrDepositIntentAlreadyResolved`**. The check in 1.4 and this re-check are the
   same predicate. The re-check exists so that no caller, including the legacy path and any
   future one, can reach `ledger.Post` for a resolved intent.
3. **Caller mapping of `ErrDepositIntentAlreadyResolved`, and of the ledger backstop sentinel
   `ledger.ErrDepositAlreadyPostedForIntent` (§28.8):**

   | Caller | Mapping |
   |---|---|
   | T7 site (receipt, phase C, sweeper, T17) | T10 → `disputed`, `multiple_success_for_intent` (§28.4) |
   | T13 site (receipt, sweeper, T17) | T13d → `disputed`, `multiple_success_for_intent` (§28.4) |
   | Legacy `InitiateDeposit` (no attempt row) | Returns the sentinel. Nothing is posted and nothing is committed for the success. P1 plus a `deposit.multiple_success_refused` audit row in a separate tx. The legacy path stays scheduled for removal (security P2-L2); it must never post for a resolved intent. |

   The ledger sentinel reaching any caller means the choke point was bypassed. It gets the
   same mapping plus an additional P1 `deposit_intent_index_backstop_fired` (a defect signal).
4. **Deleted:** the `if intent.Status == DepositIntentSucceeded { … "deposit.second_capture_posted" … }`
   branch of `postDepositSuccess`.
   - After rule 2 it is dead code.
   - The only way to reach it with `AlreadyPosted` is an exact replay, which returns the
     existing transaction id unchanged.
   - The audit action name `deposit.second_capture_posted` is retired and must never be reused.
   - No test asserts it (`ledger-finance` ruling §5).

### 28.4 Transitions: T7 guard, T13 first success only, new T13d

These rows replace the corresponding §4.3 rows. They are the implementation spec. Both T10 and
T13d are committed with their receipt: no error, no rollback, no 5xx loop (LF95-C3).

| T | From → To | Trigger / evidence | CAS guard (in addition to `id = $1`) | Ledger / domain effect (same tx) | Audit |
|---|---|---|---|---|---|
| T7 (amended) | `submitting`/`pending`/`ambiguous` → `succeeded` | Verified matching success from the attempt's own provider, with a provider reference, **and, for a deposit, `NOT resolved_for_other(I, A, K)`** | `state = ANY('{submitting,pending,ambiguous}')`; trigger `last_evidence_kind ∈ {sync, callback, query_status}`; DB backstop: `payment_attempts_one_succeeded_deposit_per_intent` | Unchanged (Flow 1 posting, attempt link, intent link while NULL, intent → `succeeded`, `created` siblings → T3 `intent_succeeded`). Payout unchanged. | `deposit.posted`, `payment.attempt_succeeded` |
| T10 (amended) | `submitting`/`pending`/`ambiguous` → `disputed` | Existing causes **plus**: deposit matching success while `resolved_for_other(I, A, K)` (the T7 guard) | `state = ANY('{submitting,pending,ambiguous}')`; `terminal_reason = 'multiple_success_for_intent'` for the new cause | **No posting.** The attempt stores the matched provider reference (and the evidence's amount/asset already equal the attempt's). P1. The intent stays `succeeded` (§5.1 projection: any succeeded → succeeded). | `payment.attempt_disputed` (§28.11) |
| T13 (amended) | `declined` → `succeeded` | **Deposit only.** Verified matching success after a decline, **only if `NOT resolved_for_other(I, A, K)`**, i.e. this is the intent's FIRST success | `state='declined' AND operation='deposit'`; DB backstop as T7 | Flow 1 to `player_cash` (the first and only posting); intent link set (it was NULL); intent → `succeeded`; `created` siblings → T3 `intent_succeeded`; P1 `contradictory_provider_outcome`. In-flight siblings: §28.6. Tombstone → T13t (unchanged). | `payment.attempt_succeeded_after_decline`, `deposit.posted` |
| **T13d (new)** | `declined` → `disputed` | **Deposit only.** Verified matching success after a decline while `resolved_for_other(I, A, K)` | `state='declined' AND operation='deposit'`; `terminal_reason='multiple_success_for_intent'`; trigger `last_evidence_kind ∈ {sync, callback, query_status}` | **No posting, no error, no rollback.** P1 `multiple_success_for_intent`. On the callback path: an `anomaly` receipt, resolved with `attempt_id`, answered with the uniform 200 (§6.2). On a poll (sweeper/T17): no receipt, audit and P1 only. The attempt sits in the M1 queue (BLOCKED) and is reported by `pay_captured_unposted` (§28.9). | `payment.attempt_disputed` (§28.11) |

**Forbidden-list amendment (trigger, migration 0107):**
- deposit `declined → disputed` is allowed only with `terminal_reason ∈
  {reversal_tombstone_precedes_success, multiple_success_for_intent}`;
- payout `declined → disputed` (T14) is unchanged.

The "at most one succeeded deposit attempt per intent" half is enforced by the partial unique
index (§28.8), not by the trigger.

**T14/payout:** unchanged. A payout has exactly one attempt per withdrawal, so INV-DEP-1 has no
payout analogue.

### 28.5 §4.4 matrix rows (replacing the deposit cells of the `succeeded (match)` column)

| Current \ Evidence | `succeeded` (match), deposit |
|---|---|
| `created` | T15 → `disputed` (unchanged) |
| `submitting` | tombstone → T10 (`reversal_tombstone_precedes_success`); else resolved-for-other → **T10 (`multiple_success_for_intent`)**; else T7 |
| `pending` | same as `submitting` |
| `ambiguous` | same as `submitting` |
| `succeeded` | no-op (exact duplicate; `ledger.Post` idempotent) |
| `declined` | tombstone → T13t; else resolved-for-other → **T13d**; else T13 (first success) |
| `rejected` | T15 (unchanged) |
| `disputed` | recorded only (unchanged; includes `multiple_success_for_intent` attempts) |

Every other cell of §4.4 is unchanged. The dispositions:
- **T10 and T13d from a callback:** `anomaly` (uniform 200 after durable receipt, §6.2).
- **A plain replay onto an already-disputed attempt:** `duplicate_effect`.

### 28.6 In-flight siblings (and reversals)

1. **At the success that resolves the intent (T7 or T13):**
   - `created` siblings → T3 `intent_succeeded` (unchanged).
   - `submitting`/`pending`/`ambiguous` siblings are **not** force-transitioned, because the
     provider may still capture them. They keep their `next_action_at` and are polled normally.
     - T12 is already refused for them (`NOT EXISTS(succeeded attempt for the same intent)`).
     - Their later matching success hits the T7 guard → T10 `multiple_success_for_intent`.
     - Their later decline → T8 `declined`. Cascade is ineligible because the intent is
       `succeeded` (§4.6).
     - Their timeout → escalation (T16) as today.
2. **Cascade** never creates a new attempt for a resolved intent (§4.6, unchanged). T2's
   `NOT EXISTS(succeeded attempt)` CAS predicate stays.
3. **Reversals:**
   - A `deposit_reversal` naming the posted attempt's reference reverses that attempt's own
     posting (LF95-C6(b), unchanged).
   - A reversal naming a `multiple_success_for_intent` attempt's reference resolves an attempt
     with `ledger_transaction_id IS NULL`. It therefore takes the **tombstone** branch: no ledger
     effect, net zero at the PSP. That is correct under HD-LEDGER-UNALLOC-1 (A) (`ledger-finance`
     ruling §2.2).
   - A reversed deposit still occupies the INV-DEP-1 slot. A later sibling success after a
     reversal is T10/T13d, never a fresh credit.
4. **Lock order:** unchanged (ADR 0082 A7). All checks run under the parent → attempt locks the
   evidence tx already holds. The ledger index is an L4-class insertion under `ledger.Post`'s
   existing savepoint. No new lock class or exception.

### 28.7 §21.2 LF-Q1 supersession

`ledger-finance`'s ruling `docs/plans/payment-readiness/lf-q1-supersession.md` (commit
`17e5ffc`, §1) supersedes §21.2 **for the multiple-success case only**:
- a second or later matching success on a financially resolved intent is never posted to
  `player_cash`;
- T13 as the intent's first success still posts to `player_cash`, and §21.2's reasoning stands
  for it;
- LF95-C6 (a), (b) and (d) stand;
- the (c) residual is withdrawn (§28.10).

A matching success remains evidence that real money moved. What changes is the consequence: it
is evidence to be **accounted for** (§28.13), not a credit. §21.2 is kept verbatim with an
in-place note.

### 28.8 Migration impact — migration 0107 (both backstops, ruled by `ledger-finance` §3)

Migration **0107** (number to be recorded in the registry allocation line by the orchestrator).
One reviewed change: `ledger-finance` owns the ledger half, `payments` the attempts half.
Contents:

1. **`payment_attempts_one_succeeded_deposit_per_intent`:** `CREATE UNIQUE INDEX … ON
   payment_attempts (tenant_id, deposit_intent_id) WHERE operation = 'deposit' AND state =
   'succeeded'`. It enforces the state-machine half of INV-DEP-1 for every transition source,
   including a future M1 `disputed → succeeded` on a resolved intent.
2. **`ledger_transactions_one_deposit_per_intent`:** `CREATE UNIQUE INDEX … ON
   ledger_transactions (tenant_id, correlation_id) WHERE transaction_type = 'deposit'`. It is
   required because the legacy path posts with no attempt row, `postDepositSuccess` posts
   **before** `ApplySuccess`, and only the ledger is authoritative for money. Reversals
   (`deposit_reversal`), tombstones (`tombstone`, random correlation id) and any future
   HD-LEDGER-UNALLOC-1 (B) posting (distinct type) are outside the predicate.
3. **`CREATE OR REPLACE FUNCTION payment_attempts_guard()`:** the deposit
   `declined → disputed` branch accepts `terminal_reason ∈ {reversal_tombstone_precedes_success,
   multiple_success_for_intent}` (T13d). Nothing else in the 0101 body changes.
   - **Project rule** (`rv-prh-architect.md` §5): every behaviour test of `payment_attempts_guard`
     (`internal/payments/migration_0101_integration_test.go` and siblings) must run against a
     HEAD-migrated scratch DB in the same change. Only tests of 0101's own up/down history may
     stay pinned, and each must say so.
4. **`reconciliation_mismatches_mismatch_kind_check`:** a strict superset of 0102's list, plus
   `'pay_captured_unposted'`.

**Fail-closed pre-flights (no bypass).**
- **Pattern: 0092's, not a `SELECT … GROUP BY` pre-check.** Each index build is itself the
  check, inside `DO $$ BEGIN CREATE UNIQUE INDEX …; EXCEPTION WHEN unique_violation THEN RAISE
  EXCEPTION '<runbook text>'; END $$`.
- **Why:** both tables carry `FORCE ROW LEVEL SECURITY` and the migration connection sets no
  `app.tenant_id`, so a `SELECT` pre-check would see zero rows and let duplicates through (the
  migration 0048 / 0092 lesson). An index build scans every row regardless of RLS.
- **Runbook text:**
  - attempts index: "more than one succeeded deposit attempt exists for a deposit intent";
  - ledger index: "more than one deposit posting exists for a deposit intent";
  - both: "never delete ledger or attempt rows; escalate to the human; synthetic dev/scratch
    databases produced by pre-§28 T13 tests are the expected place this refuses, so recreate
    them".
- `ledger-finance`'s ruling phrased the ledger pre-flight as a `GROUP BY … HAVING count(*) > 1`
  query. That query is the **diagnostic the runbook tells the operator to run** (as a role that
  sees all tenants), not the migration's gate.

**Reversible.** `0107….down.sql`:
- drops both indexes;
- restores 0101's `payment_attempts_guard()` body verbatim;
- restores 0102's kind CHECK. If `pay_captured_unposted` rows exist, the CHECK restore fails
  closed with a clear message. It is wrapped like the index builds, never by deleting rows.

Rolling back leaves any `multiple_success_for_intent` attempts in place (append-only history).
The restored trigger does not re-validate existing rows.

**`ledger.ErrDepositAlreadyPostedForIntent`** (new, `internal/ledger`), following the 0092
`ErrReversalAlreadyExists` pattern exactly:
- a constant `ledgerOneDepositPerIntentConstraint = "ledger_transactions_one_deposit_per_intent"`;
- in `Post`'s conflict path, **the idempotency key is looked up first** (the P2-A rule). An
  existing row under the same key is a replay: `AlreadyPosted`, or payload mismatch, exactly
  as today.
- The sentinel is returned **only** when no row exists for the request's key **and** the
  reported constraint is this index.
- `db.IdempotentInsert`'s savepoint leaves the tx usable, so the caller can commit T10/T13d in
  the same tx (§28.3 rule 3).

**Attempt-index violation.** It happens only if the choke point and the ledger index were both
bypassed. The `ApplySuccess` UPDATE is not in a savepoint, so the tx aborts: 5xx, nothing
committed, fail closed, P1 on the error class.

**Fixture that must change (not the index):** `internal/idempotency/integration_test.go`
`TestIntegration_LegitimateSecondOccurrenceDoesNotCollapse`. It must give each occurrence its
own correlation id (`ledger-finance` ruling §3).

### 28.9 Reconciliation impact

- **New kind `pay_captured_unposted`** ("provider captured, platform disputed, not posted").
  - Emitted for each attempt with `state = 'disputed' AND terminal_reason =
    'multiple_success_for_intent'`, when all of these hold:
    - the statement line for it (if any) is `succeeded`;
    - there is no `deposit_reversal` line naming its reference;
    - no ledger tombstone holds `(provider_id, provider_reference)`.
  - It is reported on **every** run until it clears (ageing, with amount and asset in the
    mismatch row, never in log lines).
  - It clears when a reversal or tombstone appears (the PSP refunded), or when M1/allocation
    occurs (BLOCKED).
  - `reversal_tombstone_precedes_success` disputes are excluded (net zero at the PSP).
  - `matchPayment` must stop skipping `disputed` attempts **for this reason code only** (widened by
    §35 to every captured-and-unposted T10 reason). Other
    disputed attempts remain payments-owned P1s, not reconciliation rows.
  - Remediation (§12.5 row): escalate; never auto-resolve; **never** a T17/re-drive trigger.
- **`pay_duplicate` / `checkPlatformDuplicates` is kept as an integrity detector.** After
  migration 0107 the platform-side half is structurally unreachable for new data. Any occurrence
  means an index was dropped or the data predates 0107: a P1 integrity alert. The ledger-join
  `pay_missing_platform_record` "maps to more than one succeeded attempt" branch is likewise
  kept as a detector.
- **T17 / `pay_status_mismatch` re-drive (§12.5, LF95-R1 when built):**
  - evidence only: `QueryStatus` → §28.5 matrix → the §28.3 choke point, so it can never post
    for a resolved intent;
  - **T17 never state-changes a terminal attempt** except `declined` (T13 first success or
    T13d). On `succeeded`, `disputed` and `rejected` it is a read-only re-verify whose result is
    recorded as evidence for the M1 queue;
  - the re-drive job must not select `pay_captured_unposted` rows.
- **Under HD-LEDGER-UNALLOC-1 (B), later:** the kind becomes "unallocated receipt still open",
  and the ledger join (LF95-C13) must include the (B) postings mapped to their disputed attempt.
- `docs/architecture/reconciliation-model.md` gains the kind. `ledger-finance` edits it.

### 28.10 §20 residuals

- **Withdrawn:** "T13 with a sibling already `submitting` may still capture a third time", and
  "a second capture posts". Neither is an accepted residual any more. A third or later real
  capture takes the same no-credit path (T10/T13d) as the second.
- **Stands, re-worded:** a self-contradicting PSP can still **capture** twice at the PSP; the
  platform cannot prevent that. It is never credited, and it stays visible as `disputed` plus
  `pay_captured_unposted` until refunded. The refund is PSP-initiated: platform-initiated refund
  is NOT IMPLEMENTED (§5.5), and dispute resolution is BLOCKED (HD-0095-1,
  LEDGER-MANUAL-ADJ-4EYES-1).
- The payout T14 residual is unchanged.

### 28.11 Audit impact

- **T10 and T13d with `multiple_success_for_intent`:** `payment.attempt_disputed` in the same tx.
  - Metadata: `terminal_reason`, `last_evidence_kind`, `provider_id`, `provider_reference`,
    `deposit_intent_id`, the succeeded sibling's attempt id (NULL if the resolution came from a
    legacy posting), and the existing deposit `ledger_transaction_id`.
  - Actor: `system` (callback or sweeper), or the named system principal (re-drive), or staff
    (T17).
- **P1 alert:** a structured, allow-listed Error-level log line
  `payments_multiple_success_for_intent_alert`. It carries tenant, intent and attempt ids only:
  **no amounts, no references in the log line** (security S-5). Delivery beyond the log line is
  NOT IMPLEMENTED, the same status as the kill-switch alert.
- **Backstop fired:** an additional `payments_deposit_intent_index_backstop_fired` P1 log line.
- **Legacy path refusal:** `deposit.multiple_success_refused` (separate tx; §28.3).
- **Retired:** `deposit.second_capture_posted` (§28.3 rule 4).
- **The uniform 200 discloses nothing** (S95-C4). The disposition lives only in the receipt,
  the audit record and metrics.

### 28.12 Tests (for `qa`; invert, never delete)

The binding lists are `ledger-finance` ruling §5 (items 1–5, and "new tests required") and
`double-credit-reconciliation.md` §5 (matrix A–O). Every scenario ends with
`SUM(debits) == SUM(credits)`, projection == rebuild, audit asserted, and **at most one
`deposit` posting per intent** (INV-IO-13 + INV-DEP-1).

Architect additions:
- **Choke-point placement:** the T10 guard reached from each of receipt, phase C, sweeper poll
  and T17, one test each.
- **Precedence:** tombstone plus resolved intent → `reversal_tombstone_precedes_success`, not
  `multiple_success_for_intent`, and no `pay_captured_unposted`.
- **Exact redelivery:** replay of the posted success after 0107 → `duplicate_effect`, one
  posting, **not** T10.
- **Trigger:** a deposit `declined → disputed` with any other `terminal_reason` is refused (run
  against HEAD).
- **Migration 0107:**
  - up/down/up on a clean scratch DB;
  - up refuses on a DB seeded, below 0107, with two succeeded attempts for one intent, and
    separately with two deposit postings sharing a correlation id, in each case with a
    non-privileged role so RLS is in force;
  - down refuses while a `pay_captured_unposted` row exists.
- **Mutation kills:**
  - drop the choke-point check;
  - drop the re-check inside `postDepositSuccess`;
  - drop each index;
  - swap the tombstone/INV-DEP-1 order;
  - let T17 act on a terminal non-`declined` attempt;
  - remove `pay_captured_unposted` emission.
- **§16.2 item 19** ("a reversal of a T13 second capture…") is re-stated per §28.6 point 3.

`payments` re-states the PRH-I5 T13 mutant in `evidence/prh-i1-mutation-kill.txt` against the
inverted tests.

### 28.13 HD-LEDGER-UNALLOC-1 — "A now, B later" (human decision, 2026-09-27, registry `079c5f2`)

- **Now (A), and this is what §28 specifies:** the second real capture is held off-ledger as a
  `disputed` attempt. There is no posting of any kind, a P1, audit, and `pay_captured_unposted`.
  The player is never credited. The ledger is deliberately incomplete for these receipts until
  (B), and each case is a standing, reported reconciliation exception. This is the accepted
  cost of (A).
- **Later (B), deferred as LEDGER-SUSPENSE-B-1** (`ledger-finance`, separately authorised
  stage; NOT IMPLEMENTED, not in scope here):
  - a new unallocated/suspense liability account type and a distinct non-`deposit`
    `transaction_type`, so INV-DEP-1's ledger index is untouched;
  - the reversal path mirrors those postings instead of debiting `player_cash`;
  - a one-time forward-only backfill of one posting per unrefunded
    `multiple_success_for_intent` attempt, keyed `unallocated:<provider_id>:<provider_reference>`.

  (A) was chosen as the interim because it writes no ledger rows, so moving to (B) needs no
  compensating entries.
- **Unchanged and still BLOCKED:** resolving the dispute (M1, HD-0095-1), allocating to the
  player (LEDGER-MANUAL-ADJ-4EYES-1), and platform-initiated refund (§5.5).

### 28.14 Consumers unaffected (checked by `ledger-finance`)

- **Bonus** (`internal/bonus/deposit_sweep.go` joins `deposit_intents.ledger_transaction_id`):
  no change; it only ever saw the first posting.
- **KYC `sumSettledDeposits`:** it stops over-counting second captures. This is a correction,
  not a regression.
- **Tenant isolation:** unchanged. Every check runs inside the evidence tx's `WithTenant`, and
  cross-tenant evidence is refused at binding (INV-IO-14).

---

## 29. F-POOL-2 durable states — definition (human requirement, Financial Hardening)

This section defines the durable states the human named. Each maps onto the **existing**
schema: the 8 `payment_attempts` states (§4.1), the `deposit_intents` projection (§5.1) and the
`withdrawal_requests` states (§4.7). **No new DB state is introduced.** "Reconciliation-required"
is a derived predicate, not a column (§29.2).

### 29.1 State map

| Durable state (human) | Deposit: attempt state (+ columns) | Deposit: intent projection | Payout: attempt / withdrawal | Terminal? |
|---|---|---|---|---|
| **intent** | The committed `deposit_intents` row plus an attempt committed **before** any call (INV-IO-2): `created` (cascade row or NotSent revert; `ever_possibly_sent = false`), or `submitting` from the player path's T1+T2 phase-A commit | `pending` | Withdrawal `approved` (approved to pay; no attempt yet). The T1p commit creates the attempt in `submitting`. | no |
| **submitted** | `submitting` with a committed `claim_token`, `first_submitted_at`/`last_sent_at` set. The provider **may** hold it. | `pending` | Attempt `submitting`; withdrawal `submitted` | no |
| **accepted** | `pending` with `accepted_at` set and a `provider_reference` (T4/T9). Provider-neutrally identical to "pending" (§4.1). | `pending` | Attempt `pending`; withdrawal `submitted` | no |
| **pending** | `pending` (as accepted). "Pending" at the **intent** level means "some live attempt, outcome unknown and not ambiguous". | `pending` | as accepted | no |
| **succeeded** | `succeeded`, posted in the same tx (INV-IO-5/6). **At most one per intent (INV-DEP-1, §28).** | `succeeded` (sticky) | Attempt `succeeded`; withdrawal `completed` (Flow 3 Step B) | yes |
| **failed** | `declined` with `decline_stage = 'after_acceptance'` | `declined` (when none live; the intent status `failed` stays unused, §5.1) | Attempt `declined` → withdrawal `failed` (T8 `withdrawal.Fail`), or M3 from `created` | yes* |
| **declined** | `declined` with `decline_stage = 'at_submission'` (provider refused, or a §4.5 authoritative not-found). Platform-side refusal before any call is `rejected` (never sent). | `declined` (none live) | Attempt `declined`; withdrawal `failed`. Pre-dispatch KYC deny: withdrawal `rejected`, no attempt (W-KYC). | yes* |
| **ambiguous** | `ambiguous` (`ever_possibly_sent = true`); never a failure, never a success | `ambiguous` | Attempt `ambiguous`; withdrawal `submitted` | no |
| **disputed** | `disputed` with a `terminal_reason` (mismatch, reference conflict, `reversal_tombstone_precedes_success`, `multiple_success_for_intent`, T15). Exit is manual only (M1/M2, BLOCKED). | `ambiguous` if nothing succeeded; `succeeded` if another attempt did | Attempt `disputed`; withdrawal `submitted` (T10) or `failed` (T14: the double payout already happened) | yes (for automation) |
| **reconciliation-required** | Derived: §29.2 | — | Derived: §29.2 | n/a |

\* `declined` is terminal for automation. Only verified matching success evidence moves a
deposit out of it: T13 as the first success, or T13d/T13t to `disputed`. A payout leaves it
only through T14.

### 29.2 "Reconciliation-required" (derived; no new column)

An attempt is **reconciliation-required** when any of these holds:

- **(R1)** `state = 'disputed'` (any `terminal_reason`). This is the M1/M2 queue.
- **(R2)** it is non-terminal and `escalated_at IS NOT NULL` (T16: past the `SettlementWindow`,
  or a gate-blocked payout `created`).
- **(R3)** it is named by a `pay_*` row of the tenant's **latest completed** `payment_statement`
  reconciliation run. This includes `pay_captured_unposted`, `pay_status_mismatch`,
  `pay_unresolved` and every mismatch kind.
- **(R4)** it has a verified receipt still unresolved after the `SettlementWindow` (the §6.4 P1).

(R1) and (R2) are durable attempt columns. (R3) and (R4) are durable in
`reconciliation_mismatches` and `payment_provider_events`. So the predicate is fully
reconstructible from the DB after a crash, with nothing in memory.

A read-only SQL view or staff API exposing this predicate is a `RECOMMENDATION` (NOT
IMPLEMENTED), to be built with the M1 queue. It is not a new state, and nothing may write it.

### 29.3 Rules

- **No external call inside the authoritative financial tx.** Every provider call runs in phase
  B with no DB transaction, no pooled connection, no row, advisory or projection lock held
  (D1, INV-IO-1 (a)–(d), INV-IO-5). Phase A commits the intent to call; phase C applies evidence
  under CAS in a short tx that holds the ADR 0082 locks and reads the authoritative balance in
  that same tx. The sequence "tx → provider call succeeds → tx rolls back → customer charged,
  platform unaware" is impossible: the committed attempt row exists before the call, and phase C
  and the sweeper converge it.
- **Idempotency.**
  - Player: `UNIQUE(tenant, player, idempotency_key)` on the intent; a retry resumes the intent.
  - External: `merchant_reference = attempt.id`, `external_idempotency_key = "pa:" + attempt.id`,
    identical on every send (INV-IO-3).
  - Ledger: `(tenant, provider_id, provider_tx_id)` plus `idempotency_key = provider_id:provider_reference`.
  - Per intent: INV-DEP-1 (migration 0107).
  - Callbacks: receipt dedupe `(tenant, provider_id, event_fingerprint)`.
  - Every state change is one CAS, so a replay is a no-op.
- **Retries.**
  - `NotSent` → T5, same attempt, first send only.
  - `Ambiguous`/`NotProcessed` → never a blind resend; only T12 (same key, manifest
    `IdempotentSubmission`, `submit_count < max_resubmits`, never for a resolved intent).
  - A decline is never retried. Cascade creates a **new** attempt (§4.6).
  - Phase C itself is retryable at any time (CAS).
- **Callbacks.**
  - Verified, bound to the verified provider (INV-IO-14), durably receipted in the same tx as
    their effect (INV-IO-10), applied through the single §4.4 matrix.
  - Never a provider call, never a new attempt except the cascade row.
  - Unresolvable-yet callbacks are deferred (capped) and applied later only if received after
    first submission (§6.4).
- **Timeout.**
  - Before dispatch: `NotSent`.
  - After a possible dispatch: `ambiguous` (T6), **never a failure** for a payout and never a
    success for anything.
  - No outcome within the `SettlementWindow`: escalation (T16, reconciliation-required R2), not a
    state change.
- **Late success.**
  - On `ambiguous`/`pending`/`submitting`: T7, or T10 if the intent is already resolved (§28).
  - On a deposit `declined`: T13 only as the intent's first success, else T13d; tombstone → T13t.
  - On a payout `declined`: T14 (P1).
  - On `created`/`rejected`: T15.
  - A late success never credits a resolved intent (INV-DEP-1).
- **Fallback (cascade).** Deposits only, and only after a **definite** decline:
  - `cascadable = true`;
  - `attempt_no < MaxCascadeDepth`;
  - the intent is not `succeeded`;
  - no other live attempt;
  - the kill switch is not engaged;
  - either a synchronous decline or a non-interactive attempt.

  Never after `ambiguous`, `not_found` or a timeout. A fallback that succeeds resolves the
  intent. The original's later success is T13d/T10 (§28.6). No payout cascade.
- **Reconciliation.**
  - It detects, never remediates (INV-IO-12; §12.5). Remediation happens only through fresh
    provider evidence applied by the state machine (T17 → the choke point) or through
    LEDGER-MANUAL-ADJ-4EYES-1 (BLOCKED).
  - `pay_captured_unposted` keeps every held real capture visible (§28.9).

---

## 30. Amendment AM-1 — one dual-scope route family for the payment kill switch (supersedes parts of §10.4/§10.5)

- **Status:** ACCEPTED. Recorded at `security`'s request (`rv-prh-i1-killswitch-security.md`,
  "Ruling on the route deviation", condition 1). Drafted in `rv-prh-architect.md` §1.
- **Supersedes:**
  - §10.4 bullet 1's "tenant scope … and the platform admin API (platform scope, §10.5)";
  - §10.4 last bullet ("A platform principal cannot act through the tenant API, and a tenant
    principal cannot act through the platform API");
  - §10.5's "Platform admin API" table and its `platform_payments_kill_switch:*` permission
    family;
  - §10.5's tenant-route rules "A platform-scoped principal is refused with 403" and "No tenant
    id is taken from the path or body" (kill-switch routes only);
  - the platform "Target-tenant rule" route location.
- **What stands:** everything else in §10.3–§10.5:
  - engage is single-actor with a reason code;
  - release is four-eyes, with approve-and-release in one tx;
  - a platform-engaged row is read-only to tenants;
  - the switch is never exposed on a player route;
  - OpenAPI is pinned by a conformance test.

  Read every "0102/0103 kill switch" reference as migration **0105**, with its three guard
  functions as **redefined by migration 0106**.

**Routes (the only kill-switch surface).** All are under `/v1/admin/tenants/{tenantID}/payments/`:

| Operation | Route | Permission |
|---|---|---|
| List | `GET kill-switches` | `payments_kill_switch:read` |
| Read | `GET kill-switches/{killSwitchID}` | `payments_kill_switch:read` |
| Engage | `POST kill-switches` | `payments_kill_switch:engage` |
| Request release | `POST kill-switches/{killSwitchID}/release-requests` | `payments_kill_switch:release` |
| Read request | `GET kill-switch-release-requests/{requestID}` | `payments_kill_switch:read` |
| Approve (+ release, one tx) | `POST kill-switch-release-requests/{requestID}/approve` | `payments_kill_switch:release` |
| Cancel request | `POST kill-switch-release-requests/{requestID}/cancel` | `payments_kill_switch:release` |

The three permissions are granted only to `platform_admin` and `tenant_admin`.

**Scope derivation (normative).**

1. The acting scope comes only from the authenticated token. `TenantID == nil` means platform
   scope. Anything else means tenant scope. The path `{tenantID}` is never a source of scope.
2. *Tenant principal.* `{tenantID}` must equal the token's tenant, or the call gets 403 plus a
   denied audit row in the caller's own scope. The transaction runs under
   `WithPrincipalScope(<token tenant>, principal)`, never the path value.
   - As of this revision, the `<token tenant>` rule is implemented on the unmerged branch
     `ea7910a` (`TestRunKillSwitchTx_UsesAuthenticatedTenantNeverThePathValue`).
   - On this branch the path value is used, after `canActOnTenant` has proven it equal to the
     token's tenant. That is equivalent while `canActOnTenant` holds (mutant K16 killed).
3. *Platform principal.* `{tenantID}` names the target tenant.
   - The handler verifies the tenant exists, or returns 404 (L6).
   - The transaction runs under `WithPlatformAdmin(principal)`.
   - Every statement is predicated on `tenant_id = {tenantID}` under the §10.2.1 platform RLS
     family.
4. An object id belonging to another tenant, under the caller's own path, returns 404, never
   data.
5. Player, service and unauthenticated callers get 403/401 on all seven operations. The
   route-table test (L8) pins this.

**Safety invariants (binding; changing any of them requires `security` re-review before
merge).**

- **SI-1.** Only `platform_admin` may ever hold a nil-tenant staff token. Today this rests on
  the `staff_users` CHECK (`role = 'platform_admin'` ⇔ `tenant_id IS NULL`, migration 0011) and
  on token issuance. Any change that lets another role hold a nil tenant invalidates this
  amendment.
- **SI-2.** Platform lock, four-eyes, `engaged_by_scope`/`changed_by_scope` derivation and KS-L6
  are enforced by the database (`payment_kill_switch_session()` and both guard triggers, 0105 as
  redefined by 0106). They are never enforced by the URL or the permission name. Moving any of
  them into handler code is a violation.
- **SI-3.** Granting any `payments_kill_switch:*` permission to a role other than
  `platform_admin` or `tenant_admin`, or introducing a second platform-scoped role, requires
  `security` re-review. A second platform role is also the trigger to reconsider a separate
  platform permission family, for example read-only platform operations versus engage.

**Audit.** Every mutation writes its audit row in the same transaction. Every considered refusal
(`cas_conflict`, `trigger_refusal`) writes a denied row in a separate transaction. Both carry
actor, before/after, IP, UA, reason code and `target_tenant_id`.

Until KS-AUDIT-TENANT-1 lands, a platform-scoped row is written with `tenant_id NULL` and the
target tenant in metadata only. §10.5's audit rule is therefore met for the platform actor but
**not yet for the target tenant's own audit view**. This is launch-blocking, as registered; the
structural fix (a platform INSERT-only `audit_log` policy family with a provenance trigger and a
typed `actor_scope` column, amending ADR 0013) is designed in `rv-prh-architect.md` §2 and needs
its own ADR.

**Rationale and reversibility.**
- The single family is functionally equivalent to the two-tree design. Both permission families
  would map to exactly one role each today.
- It is consistent with the existing `canActOnTenant` precedent (`provider_credential_handlers.go`,
  `admin_routes.go`), and it keeps one OpenAPI surface.
- Splitting later is additive (new paths and permissions, no data change).

---

## 31. §28 (AM-2, INV-DEP-1) implementation record (`payments`, 2026-09-27, Financial Hardening FH-3)

**Status: IMPLEMENTED**, superseding §1/§9's "PARTIALLY IMPLEMENTED... §28 INV-DEP-1 NOT
IMPLEMENTED" line for the code (not the accounting-treatment human decision, which stays
BLOCKED per §28.13).

- **§28.3 choke point.** `postDepositSuccess` (`internal/payments/orchestrator.go`) re-evaluates
  `resolvedForOtherDeposit(I, A, K)` immediately before `ledger.Post` and returns the typed
  sentinel `ErrDepositIntentAlreadyResolved` when true. Every T7/T13 evidence-application site
  (receipt.go's two `OutcomeSucceeded` branches, drive.go's `ErrorClassSucceeded`, sweeper.go's
  `ErrorClassSucceeded` - which also serves T17 re-drive per deposit_v2.go's own doc comment) now
  calls the new `postDepositSuccessOrDispute` wrapper instead of `postDepositSuccess` directly:
  it checks the SAME predicate BEFORE ever posting and routes to T10
  (`ApplyDisputeFromNonTerminal`, `multiple_success_for_intent`) or T13d (the new
  `ApplyMultipleSuccessForIntent`) instead. If `postDepositSuccess`'s own re-check or the ledger's
  backstop index fires anyway, the wrapper maps it identically plus the additional
  `payments_deposit_intent_index_backstop_fired` P1 (§28.3 rule 3). The legacy `InitiateDeposit`
  path (`resolveAmbiguous`, no attempt row, `attemptID=nil`) returns the sentinel and posts
  nothing; `RecordDepositMultipleSuccessRefusal` is the separate-tx audit+P1 counterpart to
  `RecordDepositReversalRejection`'s existing pattern, for that path's caller to invoke after its
  own transaction rolls back (this path has no production caller today - test/receive-bridge
  only). The dead `deposit.second_capture_posted` branch is deleted; that audit action name is
  retired.
- **§28.4 transitions.** `ApplyMultipleSuccessForIntent` (T13d, `internal/payments/attempt.go`)
  and the existing `ApplyDisputeFromNonTerminal` (T10, reused with the new
  `TerminalReasonMultipleSuccessForIntent` constant) implement the new rows. `ApplyReceiptEvidence`
  now returns `DispositionAnomaly` (not `DispositionApplied`) specifically for a T10/T13d
  resolution, distinguishing it from the pre-existing tombstone-precedes-success T10 cell (which
  keeps `DispositionApplied`, its own established convention, unchanged).
- **§28.8 migration 0107** (`migrations/0107_deposit_intent_double_credit_backstop.{up,down}.sql`):
  both partial unique indexes, each inside a `DO $$ ... EXCEPTION WHEN unique_violation$$` block
  (the 0092 pattern, no bypass); `payment_attempts_guard()` `CREATE OR REPLACE`d with the single
  line change accepting `terminal_reason IN (reversal_tombstone_precedes_success,
  multiple_success_for_intent)`; `reconciliation_mismatches_mismatch_kind_check` widened with
  `pay_captured_unposted`. `ledger.ErrDepositAlreadyPostedForIntent` follows the 0092
  `ErrReversalAlreadyExists` pattern exactly (idempotency key looked up first; the sentinel only
  when no row exists for the request's key and the fired constraint is the new index).
- **§28.9 reconciliation.** `MismatchKindPayCapturedUnposted` (`pay_captured_unposted`) is emitted
  by `payMatcher.matchPayment` for a `disputed`/`multiple_success_for_intent` attempt whose
  statement line reports succeeded, with no reversal statement line naming its reference
  (`payMatcher.reversalOriginals`, pre-scanned) and no ledger tombstone
  (`m.ledgerByRef["tombstone\x00"+ref]`).
- **Tests.** QA's test-first matrix files landed VERBATIM (with one unavoidable, documented,
  non-assertion compile adaptation - see below) in `internal/payments/inv_dep1_matrix_integration_test.go`,
  `internal/reconciliation/inv_dep1_recon_integration_test.go`,
  `internal/idempotency/inv_dep1_correlation_integration_test.go`. The four legacy tests QA's own
  header mapped for removal are removed with a pointer comment to their replacement:
  `TestReceipt_T13SecondCapture_ThroughApplyReceiptEvidence_LedgerBalanced`,
  `TestRVLF_P6_ReversalOfSecondCaptureReversesItsOwnTransaction`,
  `TestPaymentStatement_Kind_DuplicatePlatformSuccess`,
  `TestIntegration_LegitimateSecondOccurrenceDoesNotCollapse`. New
  `internal/payments/migration_0107_integration_test.go` covers the up/down/up round trip, both
  pre-flights' refusal on seeded duplicate data, existing-valid-data survival, the ledger
  backstop sentinel, and a direct truth-table test of `resolvedForOtherDeposit`.
- **Disclosed, not fixed (per "STOP and report", never edit an assertion):**
  - `TestINVDEP1_D_ConcurrentOriginalAndFallbackSuccess_Race` and
    `TestINVDEP1_K_ConcurrentAdversarialOrderings_Race`'s shared `invDep1RaceOnce` helper asserts
    a per-rep wallet balance of exactly 5000 against a WALLET-CUMULATIVE `cashBalance` reused
    across 50+ reps - arithmetically wrong from rep 1 onward, independent of INV-DEP-1. Verified
    (via temporary debug logging, removed before commit) that `resolvedForOtherDeposit` correctly
    detects the race for both delivery orderings on every repetition observed, and that
    `assertInvariantAndBalanced` (correctly per-intent-scoped) passes every repetition.
  - `TestINVDEP1_Recon_M_DuplicateDetector_LegacyDataShape`'s own doc comment says it needs a
    pre-migration-0107 scratch DB, but it is built on `newPayWorld`/`testPool`, which migrates to
    HEAD - so its own `ledger.Post` is (correctly) refused by 0107's backstop before the test
    reaches its assertion. Needs a migrate-to-N-1 fixture (out of scope to add inside a file
    QA marked verbatim); flagged for a coordinator decision on how to re-home it.
  - One non-assertion compile adaptation: `TestINVDEP1_G_SameProviderReferenceRepeated_LedgerKeyDedupe`
    called `postDepositSuccess` with the pre-§28.3 6-argument signature (written test-first,
    before the `attemptID *uuid.UUID` parameter existed); updated to pass `&res.Attempt.ID` (the
    most accurate choice for an exact-redelivery-of-a-succeeded-attempt scenario, per its own
    comment) - the scenario, fixture and every assertion are unchanged.
- **Mutation-kill checklist** (QA's own "N. MUTATION CHECKLIST", items 1-8): see
  `docs/plans/payment-readiness/evidence/prh-i1-mutation-kill.txt`, "PAY-DOUBLE-CREDIT-1 /
  INV-DEP-1, Financial Hardening FH-3" section. Items 1/2/3 revealed genuine defense-in-depth
  (the ledger backstop index independently catches the removed pre-check, with an extra P1) and
  were killed instead by a new direct predicate test; items 6 and 8 killed directly against new
  targeted tests; item 4/5 covered by the new migration pre-flight-refusal tests; item 7 is
  pre-existing structural protection this round did not introduce.
- **Verification.** `go build ./...`, `go vet -tags=integration ./...`, `gofmt -l .` clean;
  `golangci-lint run ./...` (pinned 2.9.0): 0 issues; `go run ./cmd/migrate verify` against a
  scratch DB migrated through 0107: clean, no version gaps. Full `internal/payments`,
  `internal/reconciliation`, `internal/idempotency` and `internal/ledger` suites green under
  `-race` on a private DB except the two disclosed items above;
  `internal/httpserver`'s pre-existing `TestResolutionIsolation_*` flakiness (tracked separately,
  registry item TEST-RESISO-RACE-1) reproduced again this run, unrelated to and untouched by this
  round.
- **Deferred, per HD-LEDGER-UNALLOC-1 (§28.13):** the interim policy is (A), no posting of any
  kind for a second real capture - implemented exactly as specified. Option (B) (an unallocated
  suspense posting) is LEDGER-SUSPENSE-B-1, NOT IMPLEMENTED, out of scope for this round.

## 32. Final architecture review of the Financial Hardening block (`architect`, 2026-09-28, FH-7)

- **Record:** `docs/plans/payment-readiness/rv-fh7-architect-final.md`, reviewed at `7f5d0fb`.
- **Verdict:** APPROVE WITH CONDITIONS. The conditions are listed in that record. None of them
  is a money-path defect.
- **Verified against code, not against the documents' claims:**
  - the INV-DEP-1 single choke point (`postDepositSuccessOrDispute`, then `postDepositSuccess`)
    at all three evidence-application sites, which include T17 re-drive through the sweeper;
  - the migration 0107 backstops (two partial unique indexes and the NULL-safe guard) and 0106;
  - the F-POOL-2 three-phase rule on the deposit, payout, callback and sweeper paths;
  - kill-switch phase 2 (AM-1 tenant scope, KS-DEP-T2-T3-1);
  - the ADR 0082 A7 order (R0 before L1, and the parent lock before the attempt);
  - the standing `pay_captured_unposted` check.
- **Edits in this revision, all additive with none rewriting earlier text:**
  - the header status note;
  - the §9.3 amendment (L-b);
  - the §10.9.3 and §28 status notes;
  - this section.

## 33. Amendment — SB-CATALOGUE-IO-1: sportsbook catalogue sync follows the transaction-boundary rule (`sportsbook`, 2026-09-28)

PRH-2 hardening round workstream E3 (plan §11, registry item SB-CATALOGUE-IO-1). The task registry
classification found that `internal/sportsbook.SyncCatalogue` called `Provider.Catalogue()` from
inside the `db.Pool.WithPlatformService` transaction opened at startup - a non-financial instance
of exactly the pattern this ADR's D1 (no provider I/O with a connection held, ADR 0094 INV-POOL)
already forbids for payments/casino/KYC. `Provider.Catalogue()` also took no `ctx` and returned no
`error`, so a real network adapter implementing it could be neither cancelled nor fail cleanly.

**Fix, mirroring the existing payments/casino pattern exactly:**

- `sportsbook.Provider`'s method is now `Catalogue(ctx context.Context) (CatalogueResult, error)`
  (`types.go`) - the same ctx-in/error-out shape as `casino.CasinoProvider.Catalogue`.
  `MockSportsbookProvider.Catalogue` (`mock.go`) is updated to match; it performs no I/O and never
  errors.
- `sportsbook.FetchCatalogue(ctx, provider)` (new, `catalogue.go`) is now the ONLY sanctioned
  caller of `Provider.Catalogue`. It refuses under `txscope.Held(ctx)` before ever calling the
  provider - the same defence-in-depth guard `payments.callProvider` (step 1, INV-IO-1(b)) and
  `casino.Orchestrator.LaunchGame` (IO-1B) apply to their own adapter calls - and it never itself
  opens a database transaction. It then validates the result with the existing
  `validateCatalogueReferences` (PROVIDER-REF-BOUND-1), unchanged in behaviour, before returning
  it.
- `sportsbook.SyncCatalogue(ctx, tx, result)` (`catalogue.go`) now takes an already-fetched,
  already-validated `CatalogueResult` instead of a `Provider` - it no longer performs any provider
  I/O. It keeps its own `db.AssertPlatformServiceScope` check (unchanged) and, as defence in depth,
  re-runs `validateCatalogueReferences` (pure, I/O-free) before the first upsert, so a caller that
  skipped `FetchCatalogue` still cannot write a half-validated tree.
- `cmd/platform-api/main.go`'s startup sequence is now two steps: `FetchCatalogue` runs first, with
  no transaction open at all; only once it returns a validated result does
  `pool.WithPlatformService(ctx, db.ServiceSportsbookCatalogueSync, ...)` open, to upsert that
  result via `SyncCatalogue`. A fetch error or a validation error surfaces with the same
  fail-the-startup wrapping as before (`fmt.Errorf("sync sportsbook catalogue: %w", err)`); nothing
  is written in either case, and in the fetch-error/validation-error case no transaction is ever
  opened at all - strictly stronger than the pre-fix "nothing written inside the transaction"
  guarantee.

**Tests** (`internal/sportsbook`, integration): `TestFullCatalogueSync_ProviderNeverSeesAHeldTransaction`
(a `txscope`-asserting provider proves no pooled transaction is held for the real two-step
production call shape and that the sync still succeeds), `TestFetchCatalogue_ProviderErrorWritesNothing`
(a fetch error writes zero rows), `TestFetchCatalogue_RefusesUnderTxscopeHeld` (a txscope-held ctx is
refused before the provider is ever called), plus the pre-existing `TestSyncCatalogue_RequiresPlatformServiceScope`,
`TestSyncCatalogue_IdempotentAcrossTwoRuns` and `TestSyncCatalogue_OverBoundExternalRefWritesNothing`
adapted to the new two-step call shape with their original assertions and bounds unchanged.

**No migration.** No change to migration 0084's RLS policies or the `platform_service_id` GUC
mechanism - `AssertPlatformServiceScope` is unchanged.

Reviewers per the plan: `architect`, `security`, `code-reviewer`.

### 33.1 Code review fix round (`sportsbook`, 2026-09-28) - `docs/plans/prh2-hardening-round/reviews/e3-code-review.md`

Verdict READY WITH CONDITIONS at `33213c7`; four conditions closed on this branch, all as additive
commits on top of `33213c7` (not squashed/rebased):

- **F1 (closed).** `FetchCatalogue` being "the ONLY sanctioned caller" of `Provider.Catalogue` was,
  before this fix, a convention rather than an enforced one: the IO-1C static guard
  (`internal/txscope/no_provider_call_in_tx_closure_static_test.go`,
  `TestINV_IO_1c_NoAdapterCallInsideTxClosure`) scanned only `internal/{casino,kyc,payments}`, so
  nothing caught a future call site that moved the fetch back inside a `WithPlatformService`
  closure using an unmarked outer ctx (mutant X5b, which the runtime `txscope.Held` check alone
  cannot see, since that ctx was never marked). The guard's `dirs` now also include
  `internal/sportsbook` and `cmd/platform-api`, and `ioc1FlaggedMethods` now includes
  `FetchCatalogue` (a package-level function call is still matched, since the lexical check is on
  the selector's final identifier regardless of receiver-vs-package qualification). This makes the
  sole-caller rule for sportsbook's provider I/O enforced the same third, static way payments'
  `Deposit`/casino's `Launch`/KYC's `CreateVerification` already are - not merely documented.
  Verified: the guard stays green on the current tree, and mutant X5b (fetch moved into the
  `WithPlatformService` closure in `main.go`, called with the outer unmarked ctx) fails
  `TestINV_IO_1c_NoAdapterCallInsideTxClosure` before being reverted.
- **F2 (closed).** A pure, no-DB unit test
  (`TestFetchCatalogue_OverBoundReferenceRejectedWithoutOpeningTransaction`,
  `provider_reference_bound_test.go`) exercises `FetchCatalogue`'s own validation step directly, with
  a spy provider proving the ctx it was called with was never txscope-marked. Kills mutant X2
  (validation removed from `FetchCatalogue`).
- **F3 (closed).** An integration test (`TestSyncCatalogue_DirectOverBoundResultWritesNothing`,
  `catalogue_io_boundary_integration_test.go`) calls `SyncCatalogue` directly inside
  `WithPlatformService` with an over-bound `CatalogueResult` that never went through
  `FetchCatalogue` at all, proving `SyncCatalogue`'s own re-validation is a real, independent
  control. Kills mutant X3 (re-validation removed from `SyncCatalogue`) - with that mutant removed,
  the same over-bound `external_ref` is instead caught one layer further in, by migration-level
  check constraint `sb_selections_external_ref_ref_bound` (a third, DB-level backstop this round did
  not add, discovered while confirming the kill), so the mutant is still caught, just at a different
  layer with a different error shape than the Go-level bound.
- **F5 (closed).** `ErrCatalogueFetchRefused` is now scoped to the tx-held gate refusal only,
  matching payments'/casino's own sentinels exactly (each covers only its own gate-level refusal,
  never the adapter's own error) - the prior revision's doc comment incorrectly described it as also
  wrapping ordinary provider errors. A provider error from `FetchCatalogue` is now returned as
  `fmt.Errorf("sportsbook: fetch catalogue: %w", err)`, plain context, never
  `ErrCatalogueFetchRefused`. `TestFetchCatalogue_ProviderErrorWritesNothing` now asserts the
  provider error is surfaced AND is NOT `ErrCatalogueFetchRefused`;
  `TestFetchCatalogue_RefusesUnderTxscopeHeld` is unchanged (still asserts the sentinel).
- **F4/F6 (not closed, per the review's own severity).** F4 (Low, pre-existing, not introduced by
  E3) is not addressed here. **F6 (forward condition, binding on the first real sportsbook
  adapter):** `FetchCatalogue` currently applies no deadline of its own to the provider call - it
  runs with whatever ctx the caller supplies (the root startup signal ctx, in `main.go` today, which
  never times out short of process shutdown). A MOCK, same-process, in-memory provider has no need
  of one. **Before any real (network) sportsbook adapter is wired, `FetchCatalogue` (or its caller)
  must apply a bounded per-call timeout**, the same way payments'/casino's/KYC's own manifests set a
  `CallTimeout` per outbound call (ADR 0095 §9.1 and the payments gate's own step 5). Tracked here
  rather than in a new ADR section, since it is a direct extension of D1/D2's existing per-call
  timeout requirement to this call site.
- **F7 (closed, this subsection).** This subsection records the AST allow-list change
  (`cmd/platform-api/main_construction_ast_test.go`'s `allowedProviderCallsOutsideRegistrations` gained
  `"sportsbook.FetchCatalogue"` alongside the pre-existing `"sportsbook.SyncCatalogue"`, both already
  covered in the original SB-CATALOGUE-IO-1 commit `33213c7`) and states plainly: F1's IO-1C static
  guard is what now actually enforces "`FetchCatalogue` is the sole sanctioned caller of
  `Provider.Catalogue`" - before F1, that sentence was a doc-comment convention, not a checked
  invariant. The registry and `docs/HANDOVER.md` rows are updated by the orchestrator at merge, per
  this file's standing convention (the orchestrator is the single writer of those files).

## 34. Amendment — PRH-2 C: deposit reference validation, sync amount evidence, same-tenant binding pre-check (`payments`, 2026-10-03)

Closes PAY-DEP-REF-VALIDATE-1 (architect FH7-05), the sync half of ledger-finance LF-5, LF-6 and
security S-9 from the PRH-2 planning gate (`docs/plans/prh2-hardening-round/plan.md` §5-C). No
migration. Status: **IMPLEMENTED against the MOCK adapter; PROVIDER DEPENDENT for a real PSP.** The
deposit **poll** path (`QueryStatus`) is **not** covered here: it is PRH-2 D (PAY-POLL-AMOUNT-1),
which reuses the comparison helper below.

### 34.1 The deposit half of PROVIDER-REF-BOUND-1 condition C1 (§8)

`depositAdapterCall` (`internal/payments/drive.go`) validates the adapter-returned reference **before**
the outcome switch, as `payoutAdapterCall` already did for payouts:

| Outcome returned | Reference | Result |
|---|---|---|
| any | non-empty and invalid (over 255 bytes, invalid UTF-8, or a control character) | `ErrorClassProviderRefInvalid` -> T10 park |
| `Pending` | empty | `ErrorClassProviderRefInvalid` -> T10 park (S-9). Previously the empty value reached `MarkAccepted`, failed the `payment_attempts` CHECK (0101 / 0099) and surfaced as an untyped error. |
| `Succeeded` (sync) | empty | unchanged: `ErrorClassAmbiguous`, not parked |
| `Declined`, `Ambiguous` | empty | unchanged (a reference is optional) |

Phase C parks with `ApplyDisputeFromNonTerminal` (T10, `terminal_reason='invalid_provider_reference:<reason>'`
where `<reason>` is the closed `providerref.Reason`), writes one `payment.attempt_disputed` audit
record carrying only the field, reason, length and hash prefix (never the value), and recomputes the
intent projection (`ambiguous`: funds may have been captured). The result is returned scrubbed of
the reference, redirect URL and hosted-field token, so nothing from an unvalidated response is
persisted, logged or handed to the player. The transaction commits; the attempt is terminal, so the
sweeper never sees it: no retry or escalation loop.

### 34.2 Sync amount evidence in the `DepositResult` contract (LF-5)

`DepositResult` gains `Amount int64` and `AssetCode string`: the provider's echo of what it
processed. On a synchronous success they are **required evidence**:

- **Absent** (zero amount or empty asset) -> `ErrorClassAmbiguous`. The poll decides; no posting and no dispute.
- **Present and different** from the attempt's recorded amount or asset -> **T10 `sync_amount_mismatch`**.
  No posting, no error, committed with its audit (`provider_amount` / `provider_asset_code` in the
  metadata).
- **Equal** -> the existing tombstone check, then the INV-DEP-1 choke point, then posting, in that order.

Before this change the sync success posted `attempt.Amount` with no provider evidence, and the
intent comparison inside `postDepositSuccess` was tautological. The comparison lives in one shared,
pure helper, `CompareProviderAmount` (`internal/payments/amount_evidence.go`), returning
`Missing | Match | Mismatch`. PRH-2 D reuses it unchanged for the poll path. The asset comparison is
exact (no case folding); a negative echoed amount is a mismatch, not "missing".

### 34.3 Same-tenant binding pre-check (LF-6)

Phase C runs, under the intent lock and before any statement that would bind or post the
reference, `foreignReferenceBinding`: is `(tenant_id, provider_id, reference)` already held by a
**different** `payment_attempts` row (deposit **or** payout) or a different `deposit_intents` row? If
so: **T10 `provider_reference_conflict`**. It covers every outcome that carries a reference, because
`MarkAccepted`, `ApplyDecline`, the intent projection and `ledger.Post` each bind it, and a unique
violation at any of them would roll the transaction back and recur on every retry: an error loop.
No posting; the conflicting existing attempt is untouched.

**Cross-tenant:** there is no cross-tenant read. The predicate names the tenant, and the indexes it
mirrors are per tenant under FORCE RLS (0101 `payment_attempts_tenant_provider_ref`, 0025
`idx_deposit_intents_tenant_provider_ref`). Tenant B using the same string as tenant A's attempt has
no effect on A's attempt, ledger or audit, and A has no way to detect B's use. The test asserts exactly
that; it does not assert that either tenant "detects" the other.

### 34.4 Contract criteria for a real PSP (PAY-PSP-CONTRACT-INVDEP1)

The vendor-selection criteria are extended: a real deposit adapter must **echo the processed amount
and asset on a synchronous success**, and must return a reference that satisfies
`providerref.Validate` on every outcome (or none on a decline). An adapter that cannot echo the
amount on a sync success must declare `SyncSuccessPossible=false` (its sync result then stays
`Ambiguous` and the poll or callback decides).

### 34.5 Tests and evidence

- Unit: `amount_evidence_test.go` (helper table, `depositAdapterCall` classification).
- Integration: `dep_ref_validate_integration_test.go` (every outcome x invalid reference, S-9, sync
  amount match/missing/mismatch, deposit-, payout- and intent-bound conflicts, cross-tenant, the
  callback-versus-sync race).
- The T10 paths never post: every case asserts zero ledger transactions, an unchanged balance and a
  balanced ledger.
- Mutants and their kills: `docs/plans/payment-readiness/evidence/prh2-c-mutation-kill.txt`.

### 34.6 Carried with C (test gaps, no production behaviour change)

- **INVDEP1-BACKSTOP-BRANCH-TEST-1** (ledger-finance C-1): `invdep1_backstop_branch_integration_test.go`
  reaches `postDepositSuccessOrDispute`'s backstop branch through a real race (the X5 construction):
  the pre-check and the re-check pass, the 0107 index fires, and the branch disputes, logs
  `payments_deposit_intent_index_backstop_fired` once, writes exactly one `payment.attempt_disputed`,
  posts nothing a second time and commits. Mutant M2 is killed.
- **KS-CAS-DISCRIM-TEST-1** (code-reviewer N1): `ks_cas_discrim_integration_test.go` forces a
  non-kill-switch T2 conflict (the sibling-succeeded guard) at `drive.go`'s `!engaged` discrimination
  and asserts the error surfaces with no false `kill_switch` reason or audit. Mutant K1 is killed.

### 34.7 Residuals

- The deposit poll path's reference and amount checks are PRH-2 D. The reference it polls is the
  already-validated bound reference; D adds the echo comparison.
- The new T10 reasons write an audit record but emit no P1 log line, so they are **not alerted**.
  - **Reconciliation does not report them either** (as of C; **superseded by §35**, PRH-2 D2). `payment_statement.go` silently excludes every
    disputed reason except `multiple_success_for_intent` from status comparison. Coverage is
    **PAY-RECON-PARKED-CAPTURE-1** (assigned to PRH-2 D, together with extending the binding
    pre-check to `ledger_transactions`, F-C4).
  - **P1 alerting** for these T10s is an I-wire condition (ADR 0102), tracked with I-wire; adding a log
    site now would change the alert inventory I-wire is about to wire.
- The payout-side copy of the error-path reference gap (`payoutAdapterCall` returns before validating
  when the adapter errors) is out of scope here: PAY-PAYOUT-ERRREF-1 (F-pay).
- A deferred receipt stored for a sync-success reference during phase B stays `unresolved` after the
  sync success posts (phase C's sync-success branch does not call `ApplyDeferredReceiptsForAttempt`;
  only the Pending branch does). No money effect (one posting, proven by test); the receipt ages into
  `pay_unresolved`. Reported to the orchestrator, not fixed in C.

### 34.8 Order of checks in phase C, and the fixes of the PRH-2 C review round

Phase C (`applyDepositCallResult`) evaluates, in this order, and the first that applies decides:

1. **Reference invalid** (decided in `depositAdapterCall`, **including when the adapter also returned
   an error**: the reference is validated before the error return) -> T10 `invalid_provider_reference:<reason>`.
2. **Binding conflict** (`foreignReferenceBinding`) -> T10 `provider_reference_conflict`.
3. **Amount mismatch** (sync success only) -> T10 `sync_amount_mismatch`. Missing evidence never reaches
   here: it is `Ambiguous` in `depositAdapterCall`.
4. **Reversal tombstone** on the reference -> T10 `reversal_tombstone_precedes_success`.
5. **INV-DEP-1 choke point** (`postDepositSuccessOrDispute`) -> T10/T13d or post.
6. **Post.**

All four T10s go through one function, `parkDepositAttempt`: dispute CAS, one `payment.attempt_disputed`
audit (metadata includes `terminal_reason`, `adapter_outcome`, amounts), intent projection recompute, and the
fresh intent returned to the caller.

Review-round fixes:

- **Player-facing redirect (security C-1).** A redirect URL or hosted-field token is returned to the
  player only for an attempt phase C left `pending` (`playerFacingRedirect`, applied in both
  `InitiateDepositAttempt` and `driveCreatedAttempt` after phase C, by committed state). A parked,
  declined, ambiguous or succeeded attempt returns none, including an ambiguous result on the error path
  with a valid reference. Otherwise a player could pay into a PSP session whose reference is bound to
  another attempt.
- **Reference binding on T6 (ledger-finance F-C1).** A validated, binding-checked, non-empty reference is
  bound with `COALESCE(provider_reference, ...)` on the ambiguous transition
  (`MarkAmbiguousFromSubmittingBindingRef`) and at the `sync_amount_mismatch` park, so the sweeper can poll
  an ambiguous attempt (it never polled a reference-less one) and reconciliation can match a parked
  mismatch. The conflict and invalid-reference parks never bind.
- **Tombstone T10 unified (F-C2)** through `parkDepositAttempt`.
- **Error-path validation (code review F1).** An adapter that returns an error together with a reference is validated like any other: an invalid one parks, a valid one is bound on the ambiguous attempt and returns no redirect.
- **Test-only corrections.** The callback-versus-sync-success test was vacuous (the callback ran in phase B before the reference was bound, so it always deferred); it is replaced by two deterministic tests that assert the disposition (`deferred_unresolved` during phase B, `duplicate_effect` after phase C). A callback cannot be forced to resolve before phase C commits for a sync success, because nothing is bound until that commit. Added: sweeper-invisibility for every park, late callbacks after a conflict park (applies to the attempt that owns the reference, posts only there) and after a mismatch park (recorded only, `duplicate_effect`), and the cascade-driven redirect case.

## 35. Amendment — PRH-2 D2: reconciliation of parked captures (PAY-RECON-PARKED-CAPTURE-1) (`ledger-finance`, 2026-10-03)

Closes the reconciliation half of QA C-F2 per the ledger-finance ruling
(`docs/plans/prh2-hardening-round/reviews/c-ledger-finance.md`, "QA F2 ruling" (a) and (c)), and QA F5
of plan §5 D. Extends §28.9; supersedes the reconciliation sub-bullet of §34.7. No migration; code in
`internal/reconciliation/payment_statement.go` only. Status: **IMPLEMENTED against the MOCK adapter
and the MOCK statement source; PROVIDER DEPENDENT for a real PSP statement.**

### 35.1 The rule: bound reasons (in-run and standing)

`pay_captured_unposted` ("the provider says it captured, the platform disputed, nothing posted") is
raised for a `disputed` deposit attempt whose `terminal_reason` is one of:

| Reason | Origin | Why the attempt holds the captured reference |
|---|---|---|
| `multiple_success_for_intent` | T13d, §28.4 | unchanged from §28.9 |
| `sync_amount_mismatch` | PRH-2 C, §34.2 | the park binds the validated reference (§34.8, F-C1) |
| `poll_amount_mismatch` | PRH-2 D (PAY-POLL-AMOUNT-1), plan §5 D design 1 | the poll queries by the bound reference |
| `poll_reference_mismatch` | PRH-2 D, plan §5 D design 2 | as above; the echo is never bound (design 3) |
| `callback_amount_asset_mismatch` | T10 from a verified callback (receipt path) | the callback resolved the live attempt by its bound reference (LF D2 review P1: same exposure as `sync_amount_mismatch`) |
| `success_for_never_sent_attempt` (T15) | receipt path | **bound only if the attempt holds a reference**; otherwise it is an unbound park (§35.2). Never excluded (LF D2 review P1) |

**Classification table and pin (LF D2 review P1).** Reconciliation classifies every deposit dispute
reason explicitly, in one table (`disputeReasonClasses`): **bound** (above), **unbound**
(`invalid_provider_reference` bare and `invalid_provider_reference:*`, §35.2), **bound-if-referenced**
(T15, and `provider_reference_conflict` per LF D2 final PM-1: a phase C conflict park holds no
reference and is unbound, a poll F-C4 conflict park holds X and is bound; §35.4) or **excluded** (`reversal_tombstone_precedes_success`, net zero at
the PSP). A reason not in the table is *unclassified*: no finding at run time, and refused by the
unit pin `TestD2_P1_EveryPaymentsDepositDisputeReasonIsClassified`, which iterates
`payments.DepositDisputeTerminalReasons()` (PRH-2 D1; prefix entries are checked with every closed
`providerref` reason and in bare form) and also checks each classification against the
ledger-finance table. `TestD2_P1_NoStaleClassification` refuses table rows payments no longer
writes. Production reconciliation keeps string literals (it does not import `internal/payments`); the
pin ties the two packages together, so a new payments reason cannot ship without a decision here.

- **In-run:** a statement line matched to the attempt (by the bound reference, or by merchant
  reference) with status `succeeded`. `pay_amount_mismatch` / `pay_reference_mismatch` still fire
  alongside where they apply. A `reversed` line clears (R1, unchanged).
- **Standing:** no line names the attempt in this run: reported on every run, unwindowed, exactly as
  §28.9 / ledger-finance C2.
- **Clearing (unchanged from §28.9):** a `deposit_reversal` line in this run naming the bound reference
  as its original, or a `tombstone` ledger row on `(provider_id, bound reference)`. Nothing else: not a
  reversal line or a tombstone on any other reference, not `investigation_status` on a prior finding
  (the predicate reads no prior finding), and not M1 (ADR 0101 §4, LF-3: M1 only acknowledges).
- **End to end (QA D2-F1).** A real D1 poll park (`poll_amount_mismatch` and
  `poll_reference_mismatch`), produced by the payments sweeper polling the MOCK (§36), is reported
  in-run and standing (`TestD2_10`); a real `callback_amount_asset_mismatch` park likewise
  (`TestD2_11`).
- **Clearing for `poll_reference_mismatch` on the returned reference Y (LF D2 review B3): NOT
  IMPLEMENTED.** D1 records Y only inside the park's audit metadata (`echoed_provider_reference`, and
  only when Y passes `providerref.Validate`). Ledger-finance ruled that reconciliation must not parse
  audit JSON as money evidence; persisting Y as structured evidence needs a schema change, and none is
  allocated to D2. So today the finding clears only on the bound reference X. A PSP reversal under Y
  leaves it standing: loud, never silent. Proposed registry item: PAY-RECON-POLL-REF-CLEAR-1.

### 35.2 The rule: unbound parks (in-run only)

`provider_reference_conflict` and `invalid_provider_reference:<reason>` parks never bind the adapter's
reference (§34.8). A real capture behind one is visible only through a statement line that resolves to
the attempt **by merchant reference**. Such a `deposit` line with status `succeeded` raises
`pay_captured_unposted` in-run, whatever outcome the adapter originally reported (security C-1). The
rule is not gated on *how* the line resolved: should such an attempt ever hold a reference, a
succeeded line naming it is the same exposure and stays loud. The clearing signals are read on the
**line's** reference. At all three sites (bound in-run, bound standing, unbound in-run) the detail
text uses the ADR 0101 F13 wording: "a PSP-initiated reversal/tombstone, or allocation
(LEDGER-SUSPENSE-B-1); M1 only acknowledges" (D2 review P2).

- A conflict bound to **another deposit attempt**: any line with that reference resolves **by
  reference to the holder**, never to the parked attempt.
  - **Two lines** for the same reference (e.g. the holder's capture and the parked attempt's): the
    second raises `pay_duplicate` (`check=duplicate_line`), reported once. Verified by test; no code
    change.
  - **One line** (reference R, merchant reference naming the **parked** attempt B): **detected**, by
    the merchant cross-check below. Before it (D2 code review D2-1) this case was silent: the line
    matched the holder A by reference and, with A `succeeded` and posted, the run reported nothing.
- **Merchant cross-check (D2-1; ledger-finance ruling
  `docs/plans/prh2-hardening-round/reviews/d2-1-ledger-finance-ruling.md` (a); orchestrator decision:
  in D2, pre-merge).** It applies to every deposit **and payout** line resolved by **provider
  reference or settlement reference** (never by merchant reference) that carries a non-empty merchant
  reference. The platform issues merchant references, so the line names whose capture it is.
  Reconciliation resolves `byMerchant[merchant]` and compares **attempt identity**, not strings. It
  raises `pay_reference_mismatch` with `check=merchant` against the resolved attempt A when the merchant
  reference:
  - names a **different attempt** (the detail names it);
  - names an attempt of the **other operation** (deposit vs payout); or
  - names **no attempt** of this provider ("names no platform attempt"). An adapter that cannot echo
    our merchant reference must leave the field empty.

  When the named attempt B is `disputed` with an unbound reason, the line is `succeeded`, and nothing
  clears it on the line's reference, the check **also** raises `pay_captured_unposted` against B. B is
  **not** consumed (not recorded as matched): a separate line for B still matches it through the
  merchant path, with no false `pay_duplicate`. A's own checks (amount, asset, status, captured-unposted)
  still run; the finding is additive. With A still pending, the line also gives `pay_status_mismatch`
  against A. A line whose merchant reference names A, or is empty, is unchanged; the existing
  reconciliation suite, whose MOCK statement lines carry each attempt's own merchant reference, passes
  with no new findings. Tests are the ruling's (c) 1-8
  (`internal/reconciliation/prh2_d2_merchant_crosscheck_integration_test.go`), and its (c) 9 mutants
  are in the evidence file (§35.5).
- **The B step is for unbound parks only (code review R-1).** If the named attempt B is disputed with a
  bound reason (it holds its own reference) or an excluded reason, the line produces only the
  `check=merchant` finding against A, and no `pay_captured_unposted` for B. A bound B is still reported
  by its own standing rule, keyed by its own reference. Pinned by
  `TestD2_7/r1_B_bound_or_excluded_reason_gets_no_finding_from_the_line`.
- **A declined attempt with a succeeded line** (the T13 shape; D1 audits a contradicting poll on a
  declined attempt without a state change) gives `pay_status_mismatch` under the existing rule. No
  matcher change; pinned by `TestD2_12` (LF ruling, PAY-POLL-DECLINED-ALERT-RECON-1 part (b)).
- An **invalid reference** on a statement line is refused at fetch (`validatePaymentLine`): the run
  fails, nothing is stored, and the sweep audits `reconciliation.sweep_run_failed` with severity P1.

### 35.3 Invariants

Reconciliation writes only its run and mismatch rows (INV-IO-12): no posting, no attempt, intent,
receipt or projection change. Every test asserts zero ledger transactions for the parked attempt,
SUM(debits) = SUM(credits), `RunLedgerVsProjection` with 0 mismatches, and the before/after snapshot
of the match. One tenant, its own rows only (FORCE RLS, `tenant_id = $1` on every read).

### 35.4 Not implemented / residuals

- **Standing coverage for unbound parks: NOT IMPLEMENTED.** After the statement period that carried
  the merchant-resolved line passes, the finding drops out; only the `payment.attempt_disputed` audit
  row remains. The ruling's route is persisted-line evidence (`payment_statement_lines` of earlier
  imports), the same approach as ruling 5(c)(d). Registered as PAY-RECON-PARKED-CAPTURE-STANDING-1.
- **GATE (binding, ledger-finance D2 review B1; not a future consideration).** Neither of the following
  may happen — **the first real PSP adapter enabled for any tenant, or the first non-MOCK payment
  statement source** (whichever comes first) — until **both** hold:
  1. PAY-RECON-PARKED-CAPTURE-STANDING-1 has landed (standing coverage for unbound parks), **together
     with PAY-RECON-POLL-REF-CLEAR-1** (see below); and
  2. the I-wire P1 alert exists and is delivered for:
     - every deposit dispute reason that `disputeReasonClasses` (`internal/reconciliation/payment_statement.go`)
       classifies as **bound, unbound or bound-if-referenced**, i.e. every class except *excluded*. The
       table is the list: it already includes `callback_amount_asset_mismatch` and
       `success_for_never_sent_attempt`, and a future reason joins the gate when it is classified,
       which the P1 pin forces; and
     - the audit action `payments.poll_evidence_contradicts_terminal_attempt` (a contradicting poll
       success on a declined attempt, D1 §36: no state change, audit only).

  **Status at D2: neither holds.** The I-wire P1 alert does **not** exist yet: P1 visibility for these
  parks is an open I-wire condition (ALERT-DELIVERY-1, ADR 0102; see §34.7). Today these parks write
  only the `payment.attempt_disputed` audit row. This is acceptable only because the parks are
  reachable solely through the MOCK adapter and the MOCK statement source. (Ledger-finance D2 final
  review PM-2.)
- **Residual (ledger-finance D2 review N1): unbound-park clearing is approximate for conflict parks.**
  The in-run unbound rule clears on a reversal or tombstone on the **line's** reference (§35.2). For
  `provider_reference_conflict` that reference is held by **another** attempt (the conflict holder),
  so a tombstone or reversal on it — which may concern the holder's capture — also clears the in-run
  finding for the parked attempt: attribution to the parked attempt is approximate. It cannot hide
  unposted money (a tombstone or reversal on the reference means the PSP itself reversed that capture,
  and the holder's own posting state is reconciled independently), but it is not per-park evidence.
  PAY-RECON-PARKED-CAPTURE-STANDING-1 replaces it with clearing against a per-park evidence record.
- **F-C4** (`foreignReferenceBinding` over non-tombstone `ledger_transactions`) is `internal/payments`
  and is not part of D2.
- **MA020 (`player_open_payment_exposure`, migration 0113)** still names only
  `multiple_success_for_intent`; MA020-SYNC-MISMATCH-1 should cover the poll reasons as well.
- **`success_for_never_sent_attempt` (T15) is pinned by unit test only.** The bound-if-referenced
  resolution is tested against the attempt row (`TestD2_P1_BoundIfReferenced`). There is no
  integration fixture: migration 0101's CHECK (`state IN ('created','rejected') OR provider_id IS NOT
  NULL`) refuses a `created -> disputed` move on an attempt that never chose a provider, and building
  a provider-bearing never-sent attempt would need payments internals. Every T15 row that exists does
  carry a provider, so the per-provider stream loads it and the classification applies.
- **Clearing on the poll's returned reference Y (B3): NOT IMPLEMENTED** (see §35.1).
  PAY-RECON-POLL-REF-CLEAR-1 needs Y persisted as structured evidence (a schema change), never read
  from audit JSON.
  - **Operator rule until then:** a standing `poll_reference_mismatch` finding whose PSP reversal came
    under the returned reference Y, not the bound reference X, does **not** clear by itself. It needs
    **manual verification** against the PSP. An M1 resolution only acknowledges it; it never clears or
    suppresses the finding (ADR 0101 §4, LF-3).
  - **Deadline (binding):** PAY-RECON-POLL-REF-CLEAR-1 must land before the first real PSP or the
    first non-MOCK statement source, whichever comes first. It ships together with
    PAY-RECON-PARKED-CAPTURE-STANDING-1, under one allocated schema change that ledger-finance signs
    off.
- **`provider_reference_conflict` is bound-if-referenced (ledger-finance D2 final review PM-1).** A
  phase C conflict park never holds a reference, so it stays unbound (in-run only). D1's poll F-C4
  park holds the reference X the PSP just confirmed (X has become another non-tombstone ledger key at
  the same PSP), so it is bound: in-run and standing, keyed on X. It clears on a reversal line naming
  X. A tombstone on X cannot exist for this shape, because ledger keys are unique per (tenant,
  provider, provider_tx_id) and X is already held. Pinned by `TestD2_14`.
- **L1 (optional, not done):** defaulting an unclassified disputed reason to bound-if-referenced would
  change existing assertions (`TestD2_6` pins an unknown reason as producing no finding). The P1 pin
  already refuses any unclassified reason payments can write.

### 35.5 Tests and evidence

- `internal/reconciliation/prh2_d2_parked_capture_integration_test.go` (`TestD2_1`..`TestD2_6`): LF
  (c) 1-6, plus a pin that other disputed reasons are unchanged.
- `internal/reconciliation/prh2_d2_merchant_crosscheck_integration_test.go` (`TestD2_7`..`TestD2_9b`):
  the D2-1 ruling's (c) 1-8, and R-1.
- `internal/reconciliation/prh2_d2_postd1_integration_test.go` (`TestD2_10`..`TestD2_14`): the real D1
  poll parks end to end, the real callback mismatch park, the declined-attempt pin, and the poll F-C4
  conflict park holding X (`TestD2_14`, PM-1).
- `internal/reconciliation/payment_reason_classification_test.go` (`TestD2_P1_*`, unit): the
  classification pin.
- Mutants: LF (c) 7 (the predicate reverted to `multiple_success_for_intent` only), the D2-1 (c) 9 set,
  R-1's `Y-B-ANYDISPUTED`, and the P1 table mutants, in
  `docs/plans/payment-readiness/evidence/prh2-d2-mutation-kill.txt`.

## 36. Amendment — PRH-2 D: poll amount and reference evidence, ledger-key binding, deferred-receipt drains, park fault injection (`payments`, 2026-10-03)

Closes PAY-POLL-AMOUNT-1 and FH7-06 (architect `rv-fh7-architect-final.md`; ledger-finance LF-4 and
security S-6 from the PRH-2 planning gate, plan §5-D), PAY-DEFERRED-RECEIPT-SYNC-1 (widened by
ledger-finance), LF F-C4, the ride-alongs PAY-F3SM-TEST-1 and PAY-SWEEP-CAS-NOISE-1, and the QA C3
fault-injection gap carried from C. **No migration.** Status: **IMPLEMENTED against the MOCK adapter;
PROVIDER DEPENDENT for a real PSP** (§36.7). It reuses C's `CompareProviderAmount` unchanged (§34.2).

### 36.1 The poll success branch (`applyStatusEvidence`, `internal/payments/sweeper.go`, `poll_evidence.go`)

The sweeper re-reads the attempt under the intent lock and then, for a poll that reports **success** on a
live attempt, decides in this order; the first that applies wins. Every contradiction is a T10 through
`parkDepositAttempt` (dispute CAS, one `payment.attempt_disputed` audit with `adapter_outcome`, intent
projection recompute), commits, and posts nothing:

| # | Check | Result |
|---|---|---|
| 1 | `CompareProviderAmount` on `StatusResult.Amount`/`AssetCode` is **Mismatch** | T10 `poll_amount_mismatch` |
| 1 | …is **Missing** | no posting, no dispute; see §36.2 |
| 2 | echoed `ProviderReference` non-empty and different from `*attempt.ProviderReference` | T10 `poll_reference_mismatch` (an empty echo is allowed) |
| 3 | `foreignReferenceBinding` on the **bound** reference (attempt, intent, or non-tombstone ledger transaction, §36.4) | T10 `provider_reference_conflict` |
| 4 | reversal tombstone on the **bound** reference | T10 `reversal_tombstone_precedes_success` (now also audited and recomputed, F-C2) |
| 5 | INV-DEP-1 choke point (`postDepositSuccessOrDispute`) | T10/T13d `multiple_success_for_intent` or post |
| 6 | post | `provider_tx_id` and idempotency key = the **bound** reference |

The tombstone lookup, the binding check, the posting and the `ApplySuccess` link all use the bound
reference. The echo is used only for the comparison in row 2 and, when it is itself a valid reference,
in the audit; an invalid echo is recorded as length and hash prefix only (never the value). A bound
reference is guaranteed non-empty: `processViaQueryStatus` never polls a reference-less attempt, and the
branch returns an error rather than ever derive a posting key from the echo.

One poll-specific tightening of C's helper. `CompareProviderAmount` classes any echo with a zero amount
or an empty asset as Missing, which on the **sync** path routes to "the poll decides". On the poll path
nothing decides afterwards, so `pollAmountEvidence` upgrades **partial evidence that already contradicts
the record** (a non-zero amount that differs, or a non-empty asset that differs) from Missing to
Mismatch. Partial evidence that contradicts nothing (amount equal, asset omitted) stays Missing. The
helper itself is unchanged.

For a fresh attempt state of `declined` (the T13 second-capture shape, reachable when a callback declined
the attempt while the poll was in flight) there is no live→disputed transition (§4.4: declined ×
contradicting success is a P1 anomaly, no state change), so checks 1–3 write
`payments.poll_evidence_contradicts_terminal_attempt` and change nothing; check 4 is T13t; a matching
success posts as T13. For `succeeded` the existing succeeded × mismatch audit
(`payments.callback_amount_asset_mismatch_terminal`) now uses the same evidence rule, so a poll that merely
omits the amount is not reported as a contradiction.

### 36.2 Decision: what "Missing" means on the poll path (code review of C, F5)

**A poll success with no usable amount or asset evidence is never posted, is not a dispute, and leaves the
attempt live.** The sweeper writes one `payment.attempt_poll_amount_unconfirmed` audit record, reschedules
the attempt at the poll backoff, and polls again.

Rationale:
- *Never post:* the amount is the one fact the platform cannot supply itself; posting the attempt's own
  amount on a provider's bare "succeeded" is exactly the under/over-credit risk PAY-POLL-AMOUNT-1 exists to close.
- *Not a dispute:* a terminal `disputed` attempt makes every later matching callback recorded-only, so
  parking on Missing would strand a possibly real capture behind a manual M1 for a provider that merely
  omits the amount on status. A callback carries its own amount evidence and can still post for a live attempt.
- *Not silent:* the audit makes a provider that never echoes an amount visible. It is bounded by the poll
  backoff (cap 30 minutes, so at most 48 records per attempt per day). Deposits have no T16 escalation in the
  sweeper today, so this audit and reconciliation's `pay_status_mismatch` are the only signals; see §36.7.
- *Contradicting partial evidence is not Missing* (§36.1).
- A real PSP adapter must echo amount and asset on a `QueryStatus` success, like the sync echo of §34.4
  (PAY-PSP-CONTRACT-INVDEP1); one that cannot is unsuitable for deposits until it can.

### 36.3 Deferred-receipt drains (PAY-DEFERRED-RECEIPT-SYNC-1)

`ApplyDeferredReceiptsForAttempt` now runs on **every** transition that binds or resolves a reference
where a verified callback may be waiting:

| Site | Where | Receipt outcome |
|---|---|---|
| phase C sync success | `applyDepositCallResult`, after `ApplySuccess` and `rejectCreatedSiblings` | resolved against the now-succeeded attempt, no second posting |
| sweeper poll success on a T6-bound ambiguous attempt | `applyStatusEvidence`, after `ApplySuccess` | same |
| T9 (sweeper poll Pending) | already present (H2/S-Q2); now **asserted** by test | the stored success applies exactly once |
| T4/T9 on the callback path (`receipt.go`) | already present; **not** covered by a new test (mutant D-DRAIN-4 survives, pre-existing site) | n/a |

The resolved receipt has `resolved_at` set, `attempt_id` set and `resolution = 'applied'` (the
succeeded-attempt duplicate cell). Its `disposition_at_receipt` stays `deferred_unresolved`: that column
is immutable by trigger and describes the HTTP response given at receipt time, so the later
"duplicate_effect" reading applies to a redelivery (also asserted). Not drained, by design and recorded as
residual: T6 itself and the `sync_amount_mismatch` park, which also bind a reference; a stored success there
would, at T6, post from the callback and is a behaviour change beyond this amendment.

### 36.4 LF F-C4: `foreignReferenceBinding` covers the ledger

The pre-check gains a third query: a **non-tombstone** `ledger_transactions` row with
`(tenant_id, provider_id, provider_tx_id)` equal to the reference is a conflict (`bound_to_operation =
ledger_<transaction_type>`). The case that motivated it: a payout Step B `withdrawal_completed`
settlement reference at the same PSP equals a deposit's reference. It is bound to no `payment_attempts`
row, so the earlier queries miss it, and `ledger.Post` would loop on `ErrIdempotencyPayloadMismatch`. It is
now T10 `provider_reference_conflict` on both the phase C and poll paths. Tombstones are excluded: they have
their own T10 (§4.3), checked after the binding check.

### 36.5 `ledger.Post` refuses an empty `provider_tx_id`

`prepareEntries` returns `ErrInvalidEntry` when `ProviderTxID` is set and empty, before anything is locked
or written (defence in depth for FH7-06; migration 0099's CHECK already refuses `''` but as an untyped
constraint error). A grep of every non-test `ledger.TransactionInput` construction found no caller that
relies on `""` (all derive from validated references; `withdrawal.Complete` already refuses an empty id),
and the full payments, ledger, wallet, casino, adjustment, withdrawal and idempotency suites stay green.

### 36.6 Ride-alongs

- **PAY-F3SM-TEST-1:** a callback succeeds the attempt while the poll is in flight (a hook inside
  `QueryStatus`, where no transaction is held); the poll then reports a different amount; the test asserts
  the terminal-mismatch audit, no state change and one posting. Mutant F3SM is killed.
- **Pending branch (FH7-06, same defect class):** the poll's Pending branch wrote `res.ProviderReference`
  to the intent through `setIntentAttempt`: an empty echo violated the 0099 CHECK (an error loop) and a
  different one overwrote the intent's reference. T9 and the intent now keep the bound reference; the echo is
  ignored there (no money moves, the attempt stays live). An attempt with no bound reference yet (never polled
  by the sweeper; reachable only by direct callers) still learns it from the poll, as before.
- **Exported reason list:** `DepositDisputeTerminalReasons()` / `IsDepositDisputeTerminalReason`
  (`deposit_terminal_reasons.go`) enumerate every `terminal_reason` a deposit dispute can carry
  (`multiple_success_for_intent`, `reversal_tombstone_precedes_success`, `sync_amount_mismatch`,
  `provider_reference_conflict`, the `invalid_provider_reference:` prefix, `poll_amount_mismatch`,
  `poll_reference_mismatch`, `callback_amount_asset_mismatch`, `success_for_never_sent_attempt`). A static unit
  test parses the non-payout sources and fails if any dispute write site uses a reason outside the list.
  The poll reason strings are a contract with reconciliation (PAY-RECON-PARKED-CAPTURE-1) and must not change.
  The poll park keeps the bound reference and never binds the echo; a valid echo is recorded in the audit
  (`echoed_provider_reference`), an invalid one only as reason, length and hash prefix.
- **Review fix round (security D1-F1/F2, ledger-finance D1-M1):**
  - *Decline branch (D1-F1):* the poll's Decline branch passed the raw echo to `finalizeDeclined` (which
    overwrites `deposit_intents.provider_reference`) and to `ApplyDecline`. It now keeps the attempt's
    bound reference for both. A different non-empty echo is audit-only
    (`payments.poll_decline_reference_mismatch`), recorded as the value only when `providerref.Validate`
    passes and otherwise as reason, length and hash prefix.
  - *Succeeded-terminal audit (D1-F2):* `payments.callback_amount_asset_mismatch_terminal` from a poll now
    records the **bound** reference as `provider_reference`; the echo appears only under the same
    validate-or-hash rule.
  - *Missing on a declined attempt (D1-M1):* writes one
    `payments.poll_evidence_contradicts_terminal_attempt` audit with `reason=poll_amount_unconfirmed`; no
    posting, no state change. A declined attempt has no `next_action_at`, so the sweeper does not poll it
    again and the row is written once.
- **PAY-SWEEP-CAS-NOISE-1:** F3b's fresh-state short-circuit now covers the pending, ambiguous, decline and
  transport-failure branches: when the fresh attempt is no longer `submitting`/`pending`/`ambiguous`, a
  non-success poll is a no-op instead of a CAS conflict (`RescheduleNonTerminal` needs
  `next_action_at IS NOT NULL`, which a terminal row no longer has).

### 36.7 Tests, mutants, residuals

- Tests: `poll_amount_integration_test.go` (mismatch table including partial contradiction, Missing,
  empty/matching/different/invalid echo, order, tombstone with an empty echo, F-C4 on both paths, the
  poll-versus-callback race run `-count=50 -race`, F3SM, declined, CAS noise, deferred-receipt drains, fault
  injection) and `internal/ledger/empty_provider_tx_id_integration_test.go`. No sleeps and no wall-clock
  assertions (plan §5.0).
- **Fault injection (QA C3):** a table `SHARE` lock held by another transaction plus `SET LOCAL
  lock_timeout = '1ms'` in the victim forces SQLSTATE 55P03 after the dispute CAS, once at the audit insert
  and once at the intent projection update, for all four C parks and both poll parks. Each asserts the
  rollback is complete (attempt, intent, audit and ledger unchanged) and that re-driving the same evidence
  parks exactly once.
- **Surviving mutants, classified (not counted as killed):**
  - *ApplySuccess links the echo (D-ECHO-2): equivalent / unreachable.* `ApplySuccess` writes
    `provider_reference = COALESCE(provider_reference, $3)`. A polled attempt always has a bound,
    non-empty reference at that point (the branch refuses otherwise), so the argument can never change the
    row; and a differing echo is parked earlier (`poll_reference_mismatch`). Ledger-finance reviewed and
    agrees.
  - *Callback-path drain at `receipt.go` T4/T9 (D-DRAIN-4): pre-existing receipt-handling mutant, outside
    D1's scope,* owned by registry item PAY-RECEIPT-T4-DRAIN-TEST-1.
- Evidence and mutant kills: `docs/plans/payment-readiness/evidence/prh2-d1-mutation-kill.txt`.
- **Residuals:**
  - *Reconciliation:* on this branch `payment_statement.go` still excludes every disputed reason except
    `multiple_success_for_intent` (§34.7). `poll_amount_mismatch` and `poll_reference_mismatch` are
    captured-but-unposted shapes; PAY-RECON-PARKED-CAPTURE-1 (D2, separate branch) matches them by exact string.
  - *Alerting:* the new T10s and the unconfirmed-poll audit emit no P1 log line (I-wire, as §34.7).
  - *Drains at T6 and the mismatch-park bind* (§36.3).
  - *Deposit escalation (T16)* is not implemented for deposits, so a never-confirming provider polls at the
    backoff cap indefinitely.
