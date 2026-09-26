# 19 - F-POOL-1 (ADR 0094) ledger-finance review

- Reviewer: `ledger-finance` (financial-invariant owner)
- Date: 2026-09-26
- Branch / HEAD: `claude/focused-wright-jw88w9` @ `0cbb574`
- Under review: `4779958` (two-phase webhooks), `8d973ef` (adversarial/re-check suite)
- Scope: **money-path consequences only.** That covers casino bet/win/rollback callbacks and
  payments deposit/deposit-reversal callbacks (payments has no withdrawal or refund callback
  today; `ReceiveVerifiedCallback` dispatches only `CallbackEventDeposit` and
  `CallbackEventDepositReversal`). It also covers a ruling on F-POOL-2's dual-write hazard
  (security C11). Secret-store fairness, the fetcher and the KYC path are out of scope.

## Verdict: **SIGN-OFF WITH CONDITIONS**

The split leaves the posting semantics unchanged. None of the conditions below blocks
F-POOL-1. LF-C1 must be met at the next real-provider integration gate. LF-C2 constrains the
F-POOL-2 design.

## 1. Posting semantics are unchanged (verified)

`git diff 4779958~1 HEAD` touches no file under `internal/ledger`, `internal/wallet`,
`internal/reconciliation`, `internal/withdrawal`, `internal/economicop`, `internal/idempotency`
or `migrations`. The only reconciliation change is in a test file. In
`casino/orchestrator.go` and `payments/orchestrator.go`, the only non-comment change is the
function head. The old `verifyCallback(ctx, tx, in)` became `Redeem` (plus a provider-map
lookup). Everything after it is byte-identical: HandleCallback, dispatch, postBet/postWin/
postRollback, the tombstone branches, receiveDepositCallback and
receiveDepositReversalCallback.

| Property | Status |
|---|---|
| Double-entry posting via `ledger.Post` | Unchanged. It runs only in phase 2, in the domain tx. |
| Idempotency on `(tenant, provider_id, provider_tx_id)` | Unchanged. It is enforced by DB unique constraints in phase 2. Phase 1 writes nothing. |
| Tombstones (rollback-before-original; late original rejected) | Unchanged, phase 2 only. |
| ADR 0082 lock ordering | Unchanged. `HandleRecheckSQL` is a lock-free, `FOR`-less SELECT on `provider_credential_handles`, so it takes no row lock ahead of the wallet/projection locks. The simulation handlers' re-validation reads (`resolvePlayerOwnedSession`, `resolveOwnDepositIntent`) are also lock-free, and their ordering matches the pre-split code. |
| Balance-projection locking | Unchanged. Phase 1 reads no wallet, balance or ledger row. It reads only the handle row and the payments `ProviderAcceptsWebhook` EXISTS, in separate READ ONLY txs. |
| Authoritative balance read inside the same tx as the write | Holds. Every balance read and ledger write stays in the single `WithTenant` domain tx. No balance crosses the phase boundary. The `VerifiedCallback` carries only the verified inbound bytes and the credential. |

## 2. Ack/post windows

- **Acknowledged (200) but not posted: not possible.** Both public handlers
  (`casino_handlers.go:352-508`, `deposit_handlers.go:301-425`) call `writeJSON(200)` only
  after `DB.WithTenant` returns nil, and it returns nil only after `tx.Commit` succeeds
  (`db/tenant_rls.go:62`). A commit failure returns 500. The ambiguous case is a connection
  lost during COMMIT: the handler answers 500 even though the tx may have committed. That is
  pre-existing and safe, because the provider's redelivery hits the replay short-circuit and
  gets the original result.
- **Posted twice: not possible.** Each HTTP delivery gets its own `VerifiedCallback`, so two
  concurrent deliveries of one `provider_tx_id` both reach phase 2 and serialize on the same
  DB unique constraints and wallet locks as before. The single-use flag (C1) only prevents
  one token from being redeemed twice. It is not the idempotency mechanism, and nothing
  relies on it for that. `WithTenant` has no in-process retry loop, so a consumed token is
  never re-presented after a rollback.
- **Re-check failure after partial writes: not possible.** `Redeem` is the first call in
  `ReceiveVerifiedCallback` for both domains. In order, it checks the consumed flag, the
  binding, the age, and then `Recheck`, the only SQL. Any failure returns before
  HandleCallback, and the error rolls back the domain tx. On the public webhook path,
  `HandleRecheckSQL` is the first statement after `set_config`. The point-nine capture tests
  pin this, and `TestReceiveVerified_RecheckDBErrorRollsBack` and the `assertNothingWritten`
  variants test it. On the two **simulation** handlers, lock-free re-validation SELECTs run
  before `Redeem`. They are reads only, so no write can come before the re-check. This is
  noted, not a defect.

## 3. Is the re-check failure response retry-safe?

A re-check failure, a stale token (> 30 s), or a token mismatch returns the uniform
`credential_unavailable`, which maps to **401 "callback rejected"**. Casino writes no
rejection record, because `recordCasinoCallbackRejection` only handles
`CallbackRejectedError`.

- **The retry is idempotent.** Nothing was written, so a redelivery runs the full two-phase
  path as a first delivery. If an earlier delivery of the same `provider_tx_id` had committed,
  the redelivery hits the existing replay short-circuit. No new double-post path exists.
- **A 401 is not guaranteed to be retried.** Some aggregators and PSPs treat 4xx as terminal.
  Money stays safe for a **bet** (the provider sees it as rejected, so nothing is taken on
  either side) and for a **deposit** (the intent stays pending). For a **win**, a **rollback**
  or a **deposit reversal** that the provider does not redeliver, the player is not credited
  (or the refund is not posted) until reconciliation catches it. `Recheck` also folds a
  *transient DB error* into the same 401 (ADR 0094 §5 point 2). This behaviour was already
  there before the split: in-tx resolve DB errors were folded to C5 reasons. The split adds a
  second trigger, a handle revoked or expired between the phases, whose window is at most
  about 30 s.

## 4. Conditions

- **LF-C1 (next real-provider integration gate; `payments`/`casino` + `ledger-finance`).**
  For each non-MOCK casino and payment adapter, record the vendor's redelivery semantics for
  401 and 5xx. If a vendor treats 401 as terminal for win, rollback or reversal, do one of the
  following:
  - (a) with `security`'s agreement, return a retryable 5xx for **post-verification**
    re-check failures caused by a DB error. The sender has already proven the secret, so this
    reveals nothing to an unauthenticated caller.
  - (b) make daily provider reconciliation (CAS-RECON / PSP recon) explicitly cover
    "provider-settled, platform-unposted" wins, rollbacks and reversals, with a P1 alert.

  Not blocking today, because every adapter is a MOCK.
- **LF-C2 (F-POOL-2 design constraint).** See §6. The F-POOL-2 ADR needs `ledger-finance`
  sign-off before implementation.
- **LF-R1 (recommendation, non-blocking).** Add one test for a revoked-between-phases
  callback: restore the handle, redeliver, and assert that it posts exactly once, with
  SUM(debits)==SUM(credits) and projection==rebuild. The current suite proves the rejection
  writes nothing and that verify_only posts once. It does not chain the two.

## 5. Tests run (`-tags=integration -race -count=1`, shared CI-local DB)

| Package | Result |
|---|---|
| `./internal/ledger/...` | ok (73 top-level PASS, 0 SKIP, 0 FAIL) |
| `./internal/casino/...` | ok (177 PASS, 0 SKIP, 0 FAIL) |
| `./internal/payments/...` | ok (87 PASS, 0 SKIP, 0 FAIL) |
| `./internal/httpserver -run 'Resolution\|Recheck\|Isolation'` | ok. All eight `TestResolutionIsolation_*` pass, including `FinancialDuringOutage` and `ConnectionExhaustion`, which end with the SUM and projection==rebuild assertions. |

There were no data races and no contention re-runs were needed.

## 6. Ruling on F-POOL-2's dual-write hazard (security C11)

**Mechanism** (`payments/orchestrator.go:474-700`). `InitiateDeposit` inserts the
`deposit_intents` row, then `attemptDeposit` calls `provider.Deposit` with `MerchantReference
= intent.ID`, **inside the same tenant tx**. After the PSP accepts, the tx can still roll back
on a `setIntentAttempt` failure, an `audit.Record` failure, a commit failure, context
cancellation or a lost connection. When that happens:

1. The PSP holds a live payment whose merchant reference names an intent that never existed.
   Its success callback finds no row (`ErrDepositIntentNotFound`, 404), so the **player can be
   charged and not credited.**
2. The player's retry with the same idempotency key finds no conflicting row, because it was
   rolled back. It mints a **new** `intent.ID` and calls the PSP again, so the player can be
   **charged twice**.
3. The same hazard exists on the **webhook path**. A cascadable decline callback runs
   `handleDecline` → `attemptDeposit` → `provider.Deposit` at the next PSP, and
   `resolveAmbiguous` runs `QueryStatus`, both inside the webhook's domain tx. If that tx
   rolls back, the first PSP redelivers and the cascade fires again.

The ledger invariant itself is **not** broken. Nothing is posted without a committed intent,
and posting stays idempotent. What is broken is money-in-flight traceability: customer funds
are held externally with no platform record.

**Severity: High (P1-class) once reachable.** I raise the dual-write sub-finding from Medium,
because the realistic outcome is an unreconciled customer-funds liability and a possible
double charge. **Sequencing is unchanged:** it is not reachable today because every adapter is
an in-process MOCK, and it stays launch-blocking before the first non-MOCK payment adapter, as
registered. Casino `Launch` after `CreateLaunchSession` has the same shape but lower financial
severity: an orphan vendor session has no platform session binding, so its bets are rejected
with `ErrLaunchSessionRequired` and no money moves. KYC has no money path.

**Required design constraints (LF-C2).** The fix must satisfy all of these.

1. **Intent before call.** The intent (or attempt) row is committed in its own short tx, in a
   non-terminal `submitting` state, before any external call. The external call runs with
   **no tx held** (INV-POOL).
2. **Deterministic, persisted external idempotency.** The merchant reference or PSP
   idempotency key is derived from the committed intent or attempt id. It is never freshly
   generated per try. A retry of the same attempt must be deduplicated by the PSP. A player
   retry with the same idempotency key must resume the committed intent, not create a new one.
3. **Explicit state machine with compare-and-set transitions** on the intent or attempt row,
   for example `UPDATE ... WHERE id=$1 AND status='submitting'`: submitting →
   pending/declined/ambiguous/succeeded. Illegal and backward transitions are rejected. A
   status column on the intent is a state row, not a balance, so this is permitted. Every
   transition writes an audit record.
4. **Callback-before-result race.** A PSP callback may arrive before the result tx commits,
   so the callback path must resolve the intent by merchant reference (intent id) as well as
   by provider reference, and must accept a `submitting` intent. Ledger posting stays
   idempotent on `(tenant, provider_id, provider_tx_id/reference)`.
5. **Crash recovery.** A sweeper or reconciler resolves intents stuck in `submitting` or
   `ambiguous` via `QueryStatus` by merchant reference, with no tx held during the call. It
   never auto-declines without a definite answer and never cascades on an unknown outcome
   (payment-orchestration.md §5).
6. **Cascade through an outbox, not inline.** A decline, whether synchronous or from a
   webhook, commits the attempt's terminal state plus an outbox row for the next attempt in
   one tx. The webhook then acknowledges. The next PSP call is made by the outbox worker under
   rules 1-3. No PSP I/O inside a webhook domain tx.
7. **The ledger posting never spans external I/O.** Credit only on a definite success, in one
   domain tx that holds the wallet and projection locks in ADR 0082 order, with the
   authoritative balance read in that same tx.
8. **Required tests.** Rollback or crash injected after PSP acceptance; callback before the
   result commit; player retry while the intent is in flight; duplicate and concurrent PSP
   callbacks; sweeper resolution of `submitting`/`ambiguous`; concurrent cascade. Each test
   ends with SUM(debits)==SUM(credits) and projection==rebuild.
9. The same claim-before-call rule applies to **withdrawal payout dispatch** when it is wired
   to a real PSP or custodian. It is not wired today; `withdrawal.go` already records the
   double-payout concern around line 844.

Custody and settlement-timing decisions with legal weight are not in this ruling.
