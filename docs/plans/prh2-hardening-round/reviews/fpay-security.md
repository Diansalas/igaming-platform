_Reviewer: `security`. Branch `prh2-fpay-kyc-gate` @ `3c049ed`. Recorded verbatim by the orchestrator (the F-pay part of a combined hand-back; the D2 part is in `d2-security-pm1-confirmation.md`)._

Two reviews in this hand-back: (A) Security review — PRH-2 F-pay, and (B) Security confirmation — PRH-2 D2 delta (PM-1).

Common method:
- Disk was 5.8G free at the start and 4.9G at the end; it never approached the 3G floor.
- Each review used a fresh private DB (sec_fp_rv_20261003 and sec_d2c_rv_20261003), both DROPPED WITH (FORCE); verified 0 left. DB access worked.
- One git archive per review, built once. Exports, logs and scripts deleted.
- `-p 1` throughout. No role or credential changes, no shared cache, directory or other agent's DB touched, pipefail on, grepped for FAIL.
- Every mutant was reverted and cmp-verified identical.

======================================================================

(A) Security review — PRH-2 F-pay (prh2-fpay-kyc-gate @ 3c049ed; diff 6836319..3c049ed)

VERDICT: ACCEPT. Nothing is pre-merge. C-F1 (my F-kyc condition) is CLOSED.

Tests:
- go build and go vet (payments, httpserver): clean.
- `-race -tags integration -count=1 -p 1`:
  - internal/txscope (TestPCG1, INV-IO-1(c)): ok;
  - internal/payments: ok, in three name partitions (^Test[A-H] 86s, ^Test[I-O] 237s, ^Test[P-Z] 290s), no DATA RACE. Every test in the package matches one partition. I partitioned only because of the tool's 600s per-command limit.

Mutants (re-killed with this branch's tests):
- MU-1, the T1p unavailable branch disabled, so an outage falls into DenyForCompliance: KILLED by TestClaimForDispatch_KYCStoreOutage_RetryableAndRecorded.
- MU-4, the unavailable branch returns an error inside the transaction (rollback instead of commit): KILLED by the same test.
- MU-6, the sweeper's unavailable branch disabled (an outage would Escalate, T16): KILLED by TestSweeper_T2Reclaim_KYCStoreOutage_ReschedulesNeverEscalates and TestSweeper_T12Resubmit_KYCStoreOutage_ReschedulesNeverEscalates.
- Own mutant, the payoutAdapterCall invalid-reference result not scrubbed (`return res, …`): KILLED by TestPayoutAdapterCall_ErrorPathReferenceValidation.

Focus items:
- Fail-closed: CONFIRMED.
  - payout.go ~306-332: `unavailable` is handled BEFORE the `!decision.Allowed` deny branch. It commits only the decision row and a denied `withdrawal.submit.http` audit; the request stays `approved`, with no attempt, no hold release and no posting. ErrPayoutKYCUnavailable is returned only after the commit and maps to 503.
  - payout_sweep.go ~131-138: T2/T12 records the decision, then on unavailable does RescheduleNonTerminal and returns allowed=false. There is no Escalate, no deny audit and no resend, and the caller never reaches a provider.
  - DenyForCompliance still refuses an unavailable decision (N5 guard; MU-13 in the evidence).
  - There is no path that maps unavailable to allow, or to a deny that triggers Escalate or a hold release.
- PII and raw values on the unavailable path: the audit metadata is `denied_by_kyc_unavailable`, `reason_code` (closed: "kyc_unavailable:<code>") and `outcome`. The decision row is kyc.RecordDecision with server-built params (tenant, brand, player and person ids, amount, asset), as reviewed in F-kyc. No provider value is involved.
- Authorization and tenant isolation: decision rows are written in the caller's own WithTenant transaction, with tenant taken from the locked withdrawal row or intent (server context), never from the client, under the F-kyc table's RLS. The F-kyc isolation tests pass in the run above.
- DoS and bypass:
  - `unavailable` arises only from a contained DB-read failure inside the evaluator's savepoint. A staff member or an outside party cannot cause one deliberately, and a forced outage only ever fails closed (it cannot bypass KYC).
  - Row growth during an outage is about one decision row plus an audit per staff submit call, and per sweeper tick per due attempt at the backoff reschedule. That is bounded by tick rate times attempts, as the implementer says. Acceptable.
  - Low (F-L1), recommended: alert on a sustained unavailable rate, so an outage is not visible only as row growth.
- Deposit gate writing a decision row for `not_required` allows: ACCEPTED. It is one extra row plus an audit per deposit evaluation, in a transaction that already writes the intent and attempt rows, so the write cost grows by a constant factor and is bounded by deposit initiation (idempotency-keyed). The casino and sportsbook hot paths skipping it is a performance trade-off, not a security gap.
  - Note (F-L2, pre-existing, not F-pay's change): on the deposit path an `unavailable` evaluation finalizes the intent as declined with "kyc_required:kyc_unavailable:…" (deposit_v2.go ~214-223). It is fail-closed and moves no money, but the outage is reported to the player and staff as a KYC requirement. Consider a distinct, retryable reason.
- Payout error-path validation and scrub: CONFIRMED. payoutAdapterCall validates a non-empty reference before the err != nil return and returns `WithdrawResult{Outcome}` only (payout.go ~452-456). Phase C parks it (ErrorClassProviderRefInvalid, T10), and its audit carries only `reason` (the closed providerref reason). No raw value reaches persistence, logs or audit.
- PAY-PAYOUT-REFBIND-1 (no LF-6-style same-tenant binding pre-check in payout phase C): ACCEPTABLE FOR MERGE as MOCK-only. It must be registered and closed before any real payout provider. Payout phase C binds a VALID reference via MarkAccepted / AttachProviderReference / payoutMarkAmbiguousFromSubmitting without checking other attempts, intents or ledger keys. That gives the same error-loop or rebinding exposure D1-F1 closed on deposits.
- payoutStatusQuery error path: VERIFIED. applyPayoutStatusEvidenceInTx (payout.go) handles ErrorClassProviderRefInvalid first (park plus reason-only audit), then `if gr.Err != nil { return RescheduleNonTerminal(…) }`, before `res := gr.Value` is read. An unvalidated reference on the error path is never used.
  - Low (F-L3): payoutStatusQuery's invalid branch still returns the unscrubbed `status` (payout.go ~1243). The consumer does not use it, but scrub it to `StatusResult{Outcome: status.Outcome}` for consistency with payoutAdapterCall.
- No provider I/O inside a transaction: the decision-row writes sit in transactions that contain no adapter call. TestPCG1 and INV-IO-1(c) are green.
- withdrawal.go is a comment-only change.

======================================================================
