# RV-PRH-I1 payment callback cutover: ledger-finance review

- Reviewer: `ledger-finance` (payments/ledger reviewer; not the implementer)
- Branch / HEAD: `claude/focused-wright-jw88w9` @ `316f048` (cutover merged by `c078968`)
- Commits reviewed: `0580d0d` (reversal receipt path and dispatch cutover), `067b3b3` (§6.2 uniform
  response), `54c94c7` (HTTP test adaptation), `f048a88` (reversal-enrichment key and
  `recomputeDepositIntentProjection`), `c075499` (test migration onto
  `initiateDepositWithAttempt`/`backfillAttemptForIntent`), `d9b0b6d` (HTTP contract tests),
  `05b3684` and `367b017` (ADR 0095 §27.10)
- Files: `internal/payments/receipt.go`, `internal/payments/orchestrator.go`
  (`receiveCallbackViaReceiptPath`, `postDepositReversalTombstone`),
  `internal/httpserver/deposit_handlers.go`, `internal/payments/receive_bridge_test.go`, and the
  kill-switch predicates in `internal/payments/attempt.go` (migration 0105), read together with
  `cascade.go`, `drive.go` and `sweeper.go` where the cutover makes them reachable from a callback.
- Baseline: ADR 0095 §4.3, §4.4, §5.1 (LF95-C7), §5.4 (LF95-C6(b)/(d)), §6.1 to §6.5, §10.3, §14
  (ADR 0082 A7), §22.2 (S-Q2).

## Verdict: REJECT (changes required)

The core of the reversal cutover is correct:

- The original is resolved by `(verifiedProviderID, OriginalProviderReference)` against
  `payment_attempts`.
- The attempt's own `ledger_transaction_id` is the one reversed.
- PAY-REV-1's S2 lock and S4 re-check are kept, and the ledger index is the backstop.
- An unseen or unposted original writes a tombstone, and a late original is then committed as
  `disputed` with no posting.

Probe P6 shows a reversal of a T13 second capture reverses the second capture, not the first,
that a distinct second reversal gets `ErrDepositAlreadyReversed`, and that the ledger and
projections still balance. `recomputeDepositIntentProjection` implements the LF95-C7 order exactly,
and `succeeded` stays sticky.

I am still vetoing, under my authority over money paths, for four reasons:

1. A reversal event is posted as a real debit whatever its wire outcome. The receipt then
   records that outcome as `succeeded` (H1).
2. The cutover turns an unresolved callback into a 200 without the §6.4 backstop that makes that
   200 safe. A verified success that races phase C is acknowledged and then never applied (H2).
3. A decline callback for an attempt that was already submitted fails while a kill switch is
   engaged (H3). ADR 0095 §10.3 says callbacks are never stopped.
4. T13 applied by a callback leaves a sibling `created` cascade attempt claimable. The T2 claim
   has no succeeded-sibling guard, so the intent's money can be taken a second time at another
   PSP (H4).

Test and mutation evidence is at the end. The full `internal/payments` integration suite is green
at HEAD with migration 0105 applied. That green suite did not catch the M1 mutation, which reverses
the intent's first capture instead of the attempt's own. Only my probe P6 caught it.

## Method

- I used a detached worktree at `316f048` under the session scratchpad and a private database
  (`rv_lf_cutover_*`). I created it via `TEST_ADMIN_DATABASE_URL`, applied the grants from
  `deploy/init-app-role.sql`, migrated it to 0105, and dropped it afterwards. I did not touch the
  shared database or `igaming_orch_local`.
- I wrote scratch probe tests (never committed). Each asserts the ADR-correct behaviour, so a
  failure confirms a finding. They drive the real `ReceiveVerifiedCallback` path via
  `receiveCallbackInTx`, on fixtures built with `InitiateDepositAttempt`, not the legacy bridge.
- I ran source mutations M1, M2, M3 and M5 against the existing suite, then restored the source
  byte for byte.

## Findings

### H1 (High, veto): non-succeeded deposit_reversal evidence is posted and recorded as `succeeded`

`applyReversalReceiptEvidence` sets `ev.Outcome = OutcomeSucceeded` unconditionally, before any
decision is made. Nothing downstream looks at the wire outcome. The mock adapter accepts every
outcome for `deposit_reversal`.

- **P1 (confirmed).** A deposit of 5000 is posted. Then a verified `deposit_reversal` arrives with
  wire outcome `declined` (reason `chargeback`), `pending` or `ambiguous`. In each case
  `player_cash` goes 5000 → 0. The disposition is `applied`, and the receipt stores
  `outcome='succeeded'`.
  - Real-world shape: a chargeback is opened (`pending`) and later won by the merchant
    (`declined`). The platform debits the player on the first event and never gives the money
    back.
  - Reconciliation says this is wrong. `payment_statement.go` `matchReversal` (review F3) says a
    `pending` or `declined` reversal "expects no posting", and it flags the posted one as
    `pay_status_mismatch`. The callback path and reconciliation contradict each other.
  - The normalization also erases the only durable record of what the provider actually said.
    Two deliveries of one reversal (for example `pending` then `declined`) get the same
    fingerprint and collapse into one receipt.
- **P1b (confirmed).** For a deposit still `pending`, a `declined` reversal (amount omitted) writes
  a tombstone on the deposit's reference. The genuine success that follows is committed as
  `disputed` (`reversal_tombstone_precedes_success`) and nothing is posted. The PSP captured the
  funds, but the player is not credited until M1 manual resolution.
- **Origin.** The pre-cutover `receiveDepositReversalCallback` also ignored `event.Outcome`. The
  cutover wrote that behaviour into the new code and into the receipt instead of closing it.
  §27.10's rationale ("`declined` is a chargeback-reason carrier") describes test-helper usage
  only. It is not a contract any adapter declares.
- **Required.**
  - Only `succeeded` reversal evidence may post or tombstone.
  - Anything else gets a receipt with its real wire outcome and no ledger effect. That needs a
    reversal-specific relaxation of `payment_provider_events_check1`, or storing it with
    disposition `anomaly`/`unsupported_event`. It must not go through the deposit decline
    columns.
  - If an adapter really uses `declined` to carry a reason, it must map that at the adapter
    boundary, not in core.
  - Add probes P1 and P1b as permanent tests.

### H2 (High): `deferred_unresolved` now answers 200, but nothing ever applies the deferred receipt

`067b3b3` turns an unresolved deposit callback from 404 into 200 after storing a receipt. S-Q2
(§22.2) accepted that only because "T4 applies it in the same transaction that learns the
reference", and §6.4 requires phase C T4/T9 and the sweeper to call
`ApplyDeferredReceiptsForAttempt`. That function has **no non-test caller**: the ones in `drive.go`
(`applyDepositCallResult`), `sweeper.go` (`applyStatusEvidence`) and the callback's own T4/T9 cell
in `receipt.go` are all missing. `CallbackEvent` also has no `MerchantReference`, so the live path
cannot resolve by merchant reference either.

- **P8 (confirmed).** The adapter delivers the verified success webhook for reference R after
  phase A commits but before phase C writes R onto the attempt. This is the ordinary "webhook
  beats the sync response" race.
  - The callback gets `deferred_unresolved`, and HTTP returns 200.
  - Phase C then marks the attempt `pending`. The receipt stays unresolved, the balance stays at
    0, and the PSP will not redeliver because it got a 2xx.
  - Recovery depends only on the sweeper's `QueryStatus` poll. That does not exist for a manifest
    without status query. The receipt is also later reported as `pay_unresolved` P1 even when the
    poll does recover it.
- Before the cutover, the 404 at least left redelivery to the vendor. §27.10 row 1 says "retried
  later (phase C/sweeper)". That is not implemented.
- **Required.** Either wire `ApplyDeferredReceiptsForAttempt` into every T4/T9 (phase C, sweeper
  and callback) under the parent and attempt locks, as §6.4 says, or keep the pre-cutover
  non-2xx for unresolved callbacks until it is wired. Add P8 as a permanent test.

### H3 (High): a decline callback for an attempt already submitted fails while a kill switch is engaged

The callback decline cell calls `insertCascadeAttempt` → `InsertCreatedAttempt`. Since 0105, that
INSERT … SELECT refuses when a wildcard switch is engaged and returns `ErrKillSwitchEngaged`. The
callback returns that as an error, so the whole transaction rolls back and HTTP returns a generic
500.

- **P2 (confirmed).** A tenant-wide deposit switch (`*`, `deposit`) is engaged. Then a verified
  `declined`, `cascadable=true` callback arrives for a `pending` attempt.
  - Result: `insert cascade attempt (T1): ... kill switch is engaged`. The attempt stays
    `pending`, no receipt is stored, and HTTP returns 500. The PSP redelivers into the same 500
    until the switch is released.
  - Providers with `RedeliveryOn5xx=terminal` lose the event.
  - A success callback under the same switch does apply (control check).
- This contradicts ADR 0095 §10.3: "Never stopped: callbacks, QueryStatus polls". The same call
  sits in `sweeper.go` `applyStatusEvidence` and `drive.go` `applyDepositCallResult`, so the
  sweeper's decline poll and phase C also fail while a switch is engaged.
- No money moves wrongly, but a switch engaged during an incident blocks decline evidence for
  every attempt already in flight, exactly when containment matters.
- **Required.** A refused cascade insert must mean "not cascade-eligible". Either evaluate the
  predicate inside `cascadeEligible`, or map `ErrKillSwitchEngaged` from `insertCascadeAttempt`
  to "no child". The decline and the projection then commit as `declined`. Add a test for each
  of the three call sites.

### H4 (High): T13 via callback does not reject a `created` sibling, and T2 has no succeeded-sibling guard

`applyDepositSuccessAndPost` does not implement T13's "any sibling `created` attempt → `rejected`
(T3, `intent_succeeded`)". Separately, `ClaimCreatedForSubmission` (T2) lacks the
`NOT EXISTS(succeeded attempt for the same intent)` predicate that §4.3 T2 requires. The 0101
guard trigger enforces it for T12 only.

- **P3 (confirmed).**
  1. Attempt A1 is `pending`.
  2. A verified cascadable decline → A1 `declined`, and cascade child A2 is `created`.
  3. A late verified success for A1 (T13) → A1 `succeeded`, the ledger is credited 5000, and the
     intent is `succeeded`.
  4. A2 is still `created`. A T2 claim of A2 under the intent lock **succeeds**.
  - The sweeper (`processCreated` → `driveCreatedAttempt`) has no intent-status check either, so
    it would route A2 and call `Deposit` at another PSP. That charges the player a second time
    for an intent that is already paid.
- A related case, found by code inspection: `finalizeDeclined` overwrites `deposit_intents.status`
  with `declined` before `cascadeEligible(…, liveIntent.Status, …)` reads it. A decline for a
  still-live sibling after a T13 success therefore passes the "intent already succeeded" check and
  inserts a new cascade child. `recomputeDepositIntentProjection` repairs the status afterwards,
  but by then the child exists.
- **Origin.** The missing T2 predicate and sweeper check predate this cutover. The cutover makes T13
  reachable from live callbacks.
- **Required.**
  - Add the T2 predicate and a trigger guard mirroring the T12 one.
  - Implement the T13 sibling rejection.
  - Compute cascade eligibility from the attempts, not from the status `finalizeDeclined` just
    wrote.

### M1 (Medium): §4.4 terminal-state cells surface as `ErrAttemptStateConflict` → 500 redelivery loops

In `applyResolvedReceiptEvidence`, success evidence routes every non-live state that is not
`succeeded`/`declined` to `ApplyDisputeFromNonTerminal`. That CAS only allows
`submitting`/`pending`/`ambiguous`. The amount/asset mismatch check also runs before the
`succeeded` check.

- **P4 (confirmed):**
  - Success for 5000 posted, then a verified success with amount 4999 → `T10 ->disputed: CAS
    transition conflict`, rollback, 500. §4.4 requires "P1 anomaly, receipt `anomaly`, no change",
    returned as a 200.
  - A declined attempt followed by a mismatched success gives the same error.
- **P7 (confirmed).** A tombstone, then a late success (correctly committed `disputed`), then a
  **redelivery** of that same success → the same CAS conflict and 500. Every at-least-once
  redelivery after T10 or T13t now loops.
- `created`/`rejected` + success (T15) also routes to the wrong CAS, found by code inspection.
- None of these move money, but receipts are not stored durably and the PSP retries indefinitely.
  Before the cutover, a mismatch was a non-retryable 400.
- **Required.** Implement the `succeeded`, `declined`-mismatch, `disputed`, `created` and
  `rejected` rows of §4.4: anomaly or recorded-only receipt, T15 via `ApplyDisputeFromNeverSent`,
  no error.

### M2 (Medium): reversal and tombstone receipts are never resolved

`applyReversalReceiptEvidence` inserts its receipt with disposition `applied` but never calls
`ResolveReceipt`, so `resolved_at` stays NULL.

- **P5 (confirmed).** After one reversal and one tombstone, `CountUnappliedReceipts` = 2.
- Every lifetime reversal and tombstone for a `(tenant, provider)` counts toward the 10 000
  deferred cap. Once it fills, every genuinely unresolved deposit callback for that provider gets
  503.
- `ApplyDeferredReceiptsForAttempt` (once wired, see H2) also does not check `event_type`. An
  unresolved reversal receipt whose own reference later equals a deposit attempt's reference, with
  a receipt time after that attempt's `first_submitted_at`, would be applied as a normalized
  `succeeded` **deposit** and credited.
- §6.1 step 8 requires the one-shot resolution in the same transaction.
- **Required.**
  - Resolve reversal receipts in the transaction that applies them. Resolution `applied`;
    `attempt_id` is the original attempt, or NULL for an unseen original, which the schema
    allows.
  - Filter `event_type = 'deposit'` in the deferred backstop.

### M3 (Medium): ADR 0082 A7 / §14 R0 ordering is not followed

A7 is binding: R0 is "at most one … as its **first write** … Nothing that already holds an L1+ lock
ever inserts a receipt."

- The deposit branch inserts the receipt after `deposit_intents FOR UPDATE`.
- The reversal branch inserts it last, after L1, the L2 original lock, `ledger.Post` (L3/L4) and
  the audit write.
- I could not build a concrete deadlock: one fingerprint always resolves to the same parent, so
  duplicate deliveries serialize on the parent lock first, and
  `TestLockOrder_ConcurrentDepositAndDepositReversal_NoDeadlock` passes. But A7's deadlock-freedom
  argument no longer covers this code, and §27.10 says the reversal is locked "in ADR 0082/§14
  order" without disclosing the deviation.
- **Required.** Either move the receipt insert to be the first write, as §6.1 orders it, or record
  an A7 amendment with the replacement argument, reviewed by me.

### M4 (Medium): a deposit_reversal naming a payout attempt tombstones the payout's reference

The tombstone branch (`unresolved || original.LedgerTransactionID == nil`) runs **before** the
`Operation != deposit` integrity check. Payout attempts never carry a `ledger_transaction_id`, so
a verified reversal whose `original_provider_reference` is a payout's reference always takes the
tombstone branch. The integrity check is dead code for payouts.

- Scenario (code inspection): an adapter mapping bug or a hostile verified sender puts a payout
  reference into a reversal.
  - A tombstone takes `(provider_id, payout_ref)`.
  - `withdrawal.Complete` later posts with `provider_tx_id = payout_ref`, hits the unique index,
    and fails with an untyped error, again and again.
  - A payout the PSP actually made can then never be completed, and the hold stays.
- **Required.** Check `Operation`/`DepositIntentID` before the tombstone branch, for any resolved
  attempt.

### M5 (Medium): the intent projection is not the single writer, so status can still regress or mis-report

`recomputeDepositIntentProjection` runs only on the receipt path. Other writers still set status
directly:

- `finalizeDeclined` in the sweeper's interactive-expiry path;
- `drive.go` RG/KYC/no-route rejections;
- `setIntentAttempt(... DepositIntentPending)` in phase C and the sweeper's T4.

Scenario (code inspection): T13 makes the intent `succeeded`, then an interactive sibling `created`
attempt expires. `processCreated` → `finalizeDeclined` sets the intent to `declined` over a posted
deposit. That regresses a `succeeded` projection. The same path also writes a `deposit.declined`
audit record for an intent that is not declined. The receipt path's own decline cell does this too
before it cascades.

**Required.** Route every attempt transition's intent status through the one projection function.

### L1 (Low): test gaps shown by surviving mutants

- **M1 mutant survives** the whole `internal/payments` suite and the relevant `internal/httpserver`
  tests. The mutant posts `ReversesTransactionID: intent.LedgerTransactionID`, reversing the
  first capture. §16.2 requires this test ("a reversal of a T13 second capture reverses the second
  capture, not the first"), and no test exists. Only probe P6 kills it. Add P6.
- **M2 mutant survives.** It projects `disputed` to `declined` (the LF95-C7 violation "never
  `declined`, funds may be captured"). No test pins the projection of a disputed attempt; only
  probe P7 asserts it.
- **M3** (S4 re-check disabled) is output-equivalent, because the 0092 index backstop maps to the
  same `DepositAlreadyReversedError`. Fine as defence in depth, but the index is the real guard.
- **M5** (tombstone check disabled) is killed by
  `TestReceiveCallback_ReversalOfNeverPostedDepositWritesTombstone`. Good.

### L2 (Low): the legacy fixture bridge

`backfillAttemptForIntent` builds one attempt, marked `EvidenceSync`, after the legacy
`InitiateDeposit` has already posted. The callbacks under test do go through the real transitions,
and the bridge does not bypass them. But the bridge cannot represent multiple attempts, so none of
the migrated tests exercise cascade, T13 or a callback for an earlier cascade provider's reference.
Those callbacks now become `deferred_unresolved`, not the matrix cell the test name suggests. That
coverage exists only in `receipt_integration_test.go`. Do not count the migrated files as T13 or
cascade coverage.

### L3 (Low): the three tests whose intent changed

- `TestReceiveCallback_UnknownProviderReferenceRejected`: stronger. It checks no error, no ledger
  rows, and a durable unresolved receipt. Its comment ("left for phase C/the sweeper to apply")
  describes behaviour that does not exist (H2).
- `TestReceiveCallback_ReversalOfNeverPostedDepositWritesTombstone`: stronger. It checks no
  posting, 1 ledger row, and the attempt is `disputed` with `reversal_tombstone_precedes_success`.
  It does not assert the intent projection (`ambiguous`), and it does not redeliver the late
  success, which would expose M1/P7.
- `TestReceiveCallback_AmbiguousCallbackResolvedViaQueryStatus_NotCascaded`: this is a changed
  intent, not an equal or stronger one. The old test proved the ambiguity got resolved. The new one
  proves only zero synchronous I/O and a non-NULL `next_action_at`. That is ADR-correct (§6.5), but
  resolution is now proven only by the sweeper tests. The §27.10 wording "strictly
  equal-or-stronger" overstates this one.

### L4 (Low): receipt and disposition accuracy

- A reversal receipt stores `amount=0` and `asset_code=''` when the wire omits them, next to
  `outcome='succeeded'`. It does not store the amount actually posted.
- A reversal redelivered with a different fingerprint gets `applied` even when `ledger.Post`
  returned `AlreadyPosted`.
- Typed reversal rejections (`ErrCallbackProviderMismatch` in particular) leave no receipt; only
  `ErrDepositAlreadyReversed` gets the separate-transaction audit record.
- The `tombstoneExists` doc comment is now attached to `recomputeDepositIntentProjection`.

### L5 (Low): the tombstone branch takes no parent lock

A reversal for an attempt with `LedgerTransactionID == nil` posts its tombstone without locking
`deposit_intents`. When it races a success posting for the same reference, the
`(tenant, provider_id, provider_tx_id)` unique index decides, and the loser rolls back with an
untyped error and retries. Retries converge correctly: the tombstone wins → T10, or the deposit
wins → a real reversal. This is safe, but the "no 5xx loop" property of LF95-C6(d) holds only
after one spurious 500. Taking the intent lock when an attempt resolves would remove it.

## Checks that passed

| Check | Result |
|---|---|
| Reversal resolves by `(verified provider, original_provider_reference)` against `payment_attempts`, and reverses the attempt's own posting (P6) | PASS |
| A reversal of the 2nd capture never reverses the 1st (P6; the M1 mutant is killed only by P6) | PASS (untested in the suite) |
| PAY-REV-1: a distinct second reversal after the S2 lock → `ErrDepositAlreadyReversed`, backed by the 0092 index | PASS |
| Tombstone for an unseen or unposted original; a late original → `disputed`, no posting, intent `ambiguous`, exactly one tombstone after three reversal deliveries (P7) | PASS; replay loop per M1 |
| Duplicate success, exact redelivery, concurrent duplicates | PASS (existing tests) |
| `recomputeDepositIntentProjection` matches LF95-C7; `succeeded` is sticky | PASS (the other writers are M5) |
| Lock order on the reversal posting path: intent L1 → original L2 → `Post` L3/L4 | PASS (R0 placement is M3) |
| Success callbacks for submitted attempts while a kill switch is engaged | PASS (declines fail, H3) |
| Full `internal/payments` integration suite at HEAD (migration 0105) | green, 93 s |

## Conditions for sign-off

- **Blocking:** H1, H2, H3 and H4, each with a permanent regression test (probes P1, P1b, P8, P2
  and P3).
- **Before the stage gate:** M1 to M5.
- **Also required:** tests for L1 (P6, and a disputed-projection assertion).
- **Not blocking:** L2 to L5.

My M1/M2 resolution rights under HD-0095-1 are unaffected.
