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

---

# Re-review 1: fix round (`08b84d1`, `91ead85`, `d25a3fd`, `158ac86`; merged `b3563c8`)

- Reviewed at local HEAD `27de492`, in a detached worktree. Between `27de492` and `9ae3980`,
  `receipt.go`, `drive.go`, `sweeper.go`, `cascade.go`, `attempt.go` and `orchestrator.go` are
  byte-identical, so every finding below also holds at `9ae3980`.
- Private database created with `priv_db.sh`, migrated to head (0106). It is dropped and the
  worktree removed.
- The implementer produced no mutate/revert transcripts this round. All 24 mutants below are my
  own:
  - I applied each one with an anchor-count-checked string replacement, ran it, and restored the
    source with `git checkout`.
  - Every survivor was re-run against the **full** `internal/payments` integration suite.
  - The two mutants that concern webhook behaviour were also run against the
    `internal/httpserver` Webhook, Payment, Deposit, Reversal and PayRev tests.
- Baseline: the full `internal/payments` suite is green at HEAD (261 s).

## Verdict: NOT READY. REJECT stands on H1-R and F2; H2, H3 and H4 are closed

- **Closed:** H2, H3, H4 (money-safe, verified by probe), M2, M4, R4(a) to R4(d), and both
  surviving mutants from round 1.
- **Blocking:**
  - My H1 ruling (below) needs a code change.
  - F2 is **not** closed on the callback path, which is the path real vendors use. An oversized
    decline reason still 500-loops (probe Q3).
- **Not blocking, but required before the stage gate:** several fixes have no test that notices
  their removal (see the mutation table).

## RULING H1: deposit_reversal semantics (binding; ledger-finance authority)

**Option (a), tightened:** the `deposit_reversal` event **type** is the settlement signal. The
wire `Outcome` is not a status, except that values meaning "not final" may never move money.

Why this is the fail-closed choice for the ledger:

- A chargeback, or a completed refund, is money the PSP has already taken back from the
  merchant.
- If a final debit is not posted, `player_cash` is overstated. The player can withdraw it, and
  that loss cannot be recovered.
- If a reversal is posted wrongly, `player_cash` is understated. That is detectable (the
  reconciliation `pay_status_mismatch` below) and correctable with a four-eyes compensating entry.
- Option (b), non-final reversals never post, would require a new contract status field. Every
  adapter mis-mapping would then fail open, towards the unrecoverable loss.

"Tightened" means only the values that name a final reversal may post. `pending` and `ambiguous`
say, by their own meaning, that the reversal is not final, and no reading of this contract makes
them a debit.

Rules. These are code and contract changes, owned by `payments`; I review them.

1. **Posting outcomes.**
   - `succeeded`, and the legacy reason-carrier `declined`, post a reversal, or write a tombstone
     for an unseen or unposted original. This is the current behaviour, kept.
   - `declined` is **deprecated**. New adapters send `succeeded` and put the reason in
     `DeclineReason`. The 14 existing `declined` fixtures may stay until they are migrated.
2. **`pending` and `ambiguous` never post or tombstone.** Such an event is:
   - stored with its real wire outcome (the CHECK allows it);
   - given disposition `anomaly`, resolved `anomaly_other` with `attempt_id` = the original
     attempt if one resolved;
   - answered with the uniform 200;
   - reported as a P1 alert, `reversal_non_final_outcome`.
   Never a 4xx: a vendor may treat 4xx as terminal and lose the event.
3. **The wire outcome must be preserved, even under this reading.** This answers the
   coordinator's question: yes.
   - `computeEventFingerprint` must use the **raw** wire outcome. Deliveries that differ only in
     outcome are then distinct receipts. That is safe, because a repeated reversal reference
     still collapses on ledger idempotency (`AlreadyPosted`), and a different reference hits
     PAY-REV-1.
   - The raw wire outcome and the bounded reason code must be written to the
     `deposit.reversed` / `deposit.reversal_tombstoned` audit metadata.
   - The bounded reason must be stored in the receipt's `decline_reason` column. The CHECK allows
     `decline_reason` for any outcome.
   - The stored receipt `outcome` may stay normalized to `succeeded` for posting reversals: it
     means "applied", and this must be documented on the column. Nothing may be persisted as a
     bare `succeeded` with no trace of what the PSP actually sent.
4. **The declined-reversal-on-pending-deposit tombstone (old P1b) is correct and is kept.**
   - Under this ruling, a `declined`/`succeeded` reversal is a statement by the PSP that the
     capture was taken back. Refusing to credit a later success for the same reference is the
     fail-closed outcome: net money at the PSP is zero.
   - The success becomes T10/T13t `disputed` (P1, M1 queue), exactly as now.
   - A `pending`/`ambiguous` reversal must **not** tombstone (rule 2).
5. **Adapter contract.**
   - ADR 0095 §9.3 and the `CallbackEvent` doc comment in `types.go` must state it: "a
     `deposit_reversal` event asserts that the PSP has **finally** debited the merchant for
     `original_provider_reference`."
   - Adapters must not emit it for:
     - refund requested/pending;
     - a chargeback inquiry, retrieval request or pre-arbitration notice;
     - a chargeback **won** by the merchant (funds returned).
   - A "chargeback won" is a re-credit, or reversal-of-reversal. That is NOT IMPLEMENTED: it maps
     to `unsupported_event` with a P1, and is resolved by a four-eyes compensating entry. Record
     it as a deferred decision.
   - Vendor intake (#28) must record each PSP's reversal lifecycle mapping. That item is PROVIDER
     DEPENDENT.
6. **Reconciliation `matchReversal` needs no code change.** The statement line carries the PSP's
   own settlement status, which is independent of the callback `Outcome`. Under this ruling, a
   posted reversal whose line is `pending`/`declined` is exactly the detector for two cases:
   - an adapter contract violation;
   - a chargeback the merchant later won, which needs a manual re-credit.
   Only the comment needs updating to say so. The contradiction I reported in H1 is resolved by
   rule 2 plus this reading, not by changing reconciliation.

Tests:

- The current `TestRVLF_P1_ReversalPostsRegardlessOfWireOutcomeReasonCarrier` asserts that
  `pending` and `ambiguous` **post**. That contradicts rule 2 and must be inverted for those two
  values.
- New tests are needed for the raw-outcome fingerprint and for the audit-metadata preservation.

## RULING M1: mismatched-amount success on a terminal attempt

- **This is a P1 anomaly with no state change, not a dispute.**
  - `succeeded → *` is forbidden (§4.3).
  - `declined → disputed` exists only as T13t (tombstone) and T14 (payout).
  - So §4.4's "P1 anomaly, receipt `anomaly`, no change" is the only legal cell for:
    - `succeeded` × mismatch;
    - deposit `declined` × mismatch.
- Required behaviour:
  - receipt disposition `anomaly`, resolution `anomaly_other`;
  - a P1 alert `callback_amount_asset_mismatch_terminal` with no amounts in the log line
    (security S-5);
  - an audit record;
  - the uniform 200;
  - no posting.
- Current code fails this in two ways:
  - `succeeded` returns `ResolutionApplied` **before** the mismatch check, so it is mislabelled
    `applied`/`duplicate_effect`.
  - `declined` is a silent `anomaly_other` with no alert.
- Both are open (M1-R below). The 500-loop part of M1 is closed.

## Status of round-1 findings

| Item | Status | Evidence |
|---|---|---|
| H1 | superseded by RULING H1; code change required (rules 2, 3, 5) | reading of `applyReversalReceiptEvidence` |
| H2 | **CLOSED (wired at all 3 sites)**. Only the phase C site is pinned: DFD is killed by P8. The callback-path site (DFR) and the sweeper T9 site (DFS) survive the full suite. | mutants DFD/DFR/DFS |
| H3 | **CLOSED, and all 3 sites have tests**: KSR killed by P2, KSD by `TestRVLF_H3_DriveGo_*`, KSS by `TestRVLF_H3_SweeperGo_*` | mutants |
| H4 | **CLOSED, money-safe.** The T2 predicate works: probe Q2 shows a direct T2 claim on a paid intent → CAS conflict. Sibling rejection on the receipt path is pinned (SIBR killed by P3). But the T2 guard itself (T2G) and sibling rejection in drive/sweeper (SIBD, SIBS) survive the full suite. See also new finding N1. | probe Q2, mutants |
| M1 | 500-loop **CLOSED** (P4, P7 pass). Terminal-mismatch semantics **OPEN** per RULING M1. The live-state mismatch → T10 is pinned only at the HTTP layer: M1C survives `internal/payments` and is killed by `TestWebhook_ProviderMismatchAfterVerification_IsDisputedNotRejected`. | mutants |
| M2 | **CLOSED** (RVRS killed by P5). The `event_type='deposit'` filter in the deferred backstop (DFT) survives; see N3. | mutants |
| M3 | Deposit branch and reversal posting branch **CLOSED** (R0 before L1). The reversal **tombstone** branch now takes `deposit_intents FOR UPDATE` **before** its receipt insert: the L5 fix reintroduced the A7 violation there. **OPEN (Low).** Insert the receipt first, then lock. | `receipt.go` ~L794–L803 |
| M4 | **CLOSED** (M4 mutant killed) | |
| M5 / F3 | The sticky guard is implemented in `setIntentAttempt`, but **untested**: FRZ (status freeze) and FRZP (provider-reference freeze) survive the full suite. `TestRVLF_F3_*` sends its late decline to an attempt that is already `declined`, which is a no-op cell, so `finalizeDeclined` never runs. The projection is still not a single writer: drive/sweeper decline paths still write status directly. **OPEN.** | mutants |
| F2 | **NOT CLOSED on the callback path.** `insertReceiptDeduped` stores the raw `ev.DeclineReason` before `boundedDeclineReason` runs. Probe Q3: a 99-byte vendor reason on a decline callback → `payment_provider_events_decline_reason_check` violation → rollback → 500, forever. BDR (bound disabled) survives the full payments suite and the httpserver subset. **BLOCKING.** Apply `boundedDeclineReason` inside `insertReceiptDeduped` (and to `ev` before the matrix), and add Q3 as a test. | probe Q3, mutant BDR |
| R4(a) | **CLOSED**: payout decline → `applyPayoutDecline`, the hold is released (PDEC killed) | |
| R4(b) | **CLOSED**: event_type vs operation cross-check (XOP killed). A bypass exists via the deferred backstop; see N3. | |
| R4(c) | **CLOSED**: payout-typed decline populates the check1 columns (PCOL killed) | |
| R4(d) | **CLOSED**: payout success → `applyPayoutSuccess` | |
| R4 reachability | **Not live**: `MockProvider.HandleCallback` emits only `deposit`/`deposit_reversal`, so R4 is reachable only by calling `ApplyReceiptEvidence` directly. Label PROVIDER DEPENDENT until an adapter emits `payout` events. | `mock.go` |
| L1 mutants | **Both KILLED**: M1 (first-capture reversal) by `TestRVLF_P6_*`; M2 (disputed→declined projection) by `TestRVLF_P7_*` | mutants |

## New findings

**N1 (Low, money-safe): a cascade child is still inserted for an intent that already succeeded.**

`finalizeDeclined` returns its **input** intent when the sticky guard turns the write into a
no-op. The receipt path passes `DepositIntent{ID, TenantID}`, whose `Status` is `""`, so
`cascadeEligible` sees "not succeeded" and inserts a child. Phase C passes a stale pre-lock intent,
with the same effect. Probe Q2:

1. A1 declines (cascadable) → A2 is driven to PSP B.
2. A1's late success (T13) → the intent is `succeeded`.
3. A2's cascadable decline → **attempt 3 is `created`**.

The T2 guard then refuses a claim of attempt 3 (verified), and in this 2-provider set-up the
sweeper rejects it as `no_routable_provider`. So no second charge happens. The fix: set
`intent.Status = actual` on the no-op branch.

**N2 (Medium, pre-existing, now reachable): phase C and sweeper successes do not check for a tombstone.**

`drive.go` and `sweeper.go` success branches call `postDepositSuccess` directly. A reversal
tombstone that holds `(provider_id, provider_reference)` therefore produces an untyped
unique-index error on every poll and phase C. The attempt never reaches T10
`reversal_tombstone_precedes_success`. That contradicts §4.3 T7 ("…T10 instead, with no posting
and no error"). It is money-safe, but it loops. Route both paths through the same tombstone check
as `applyResolvedReceiptEvidence`.

**N3 (Low): the deferred backstop has no event-type-vs-operation check.**

`ApplyDeferredReceiptsForAttempt` filters `event_type='deposit'` but runs for **payout** attempts
too (`receipt.go` L489, after any changed payout receipt). A deferred deposit-typed receipt whose
reference equals a payout attempt's reference would be applied through the matrix to the payout
(for example → `applyPayoutSuccess`), bypassing the R4(b) cross-check. The filter itself (DFT) is
also unpinned. Filter by the attempt's own operation, and add a test.

**N4 (Low): `rejectCreatedSiblings` always records `EvidenceCallback`.**

This is true even when it is called from phase C (`sync`) or the sweeper (`query_status`). The
audit and `last_evidence_kind` are mislabelled. Pass the caller's evidence kind.

## Surviving mutants (full `internal/payments` suite; each needs a test)

| Mutant | What it removes | Required test |
|---|---|---|
| T2G | the succeeded-sibling predicate in `ClaimCreatedForSubmission` | a direct T2 claim on a paid intent → CAS conflict (probe Q2's final step) |
| SIBD / SIBS | `rejectCreatedSiblings` in `drive.go` / `sweeper.go` success | T13 via phase C and via sweeper poll with a `created` sibling |
| DFR / DFS | deferred-receipt application at the callback T4/T9 site / sweeper T9 site | a callback-pending race and a sweeper-poll race, like P8 |
| DFT | `event_type='deposit'` filter in the backstop | a stored reversal receipt sharing a later deposit attempt's reference is not applied |
| FRZ / FRZP | sticky freeze of status / provider_reference on a succeeded intent | a decline on a **live** sibling after success (probe Q2 shape) |
| BDR | `boundedDeclineReason` | probe Q3, over callback, phase C and sweeper |

Killed (for the record): M1, M2, SIBR, KSR, KSD, KSS, DFD, XOP, M4, RVRS, PDEC, PCOL, and M1C (by
the httpserver tests only).

## Conditions for sign-off (this round)

- **Blocking:**
  - RULING H1, rules 2, 3 and 5 (code, contract doc, and inverted P1 for `pending`/`ambiguous`).
  - F2 on the callback path, with probe Q3 as a test.
- **Before the stage gate:**
  - RULING M1 (terminal mismatch → anomaly + P1);
  - N1, N2, N3;
  - the M3 tombstone-branch ordering;
  - M5 (single writer);
  - a test for every surviving mutant above.
- **Not blocking:** N4, and the §27.10/evidence-file corrections (F6).

Probe sources (scratch, not committed):
`/tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad/rvlf2-probe_integration_test.go.txt`

---

# Re-review 2: FH-5 callback security round (`dd44d04`, `0a96a01` on top of `7641332`, `be15a11`, `bf5813f`, `9dab8f3`)

- Reviewed AT `0a96a01` (branch `worktree-agent-adce273a3a5339f77`), in a detached worktree.
- Private DB via `priv_db.sh` (`rv_lf_cb3_*`, migrated to 0106). It is dropped and the worktree
  removed.
- **No `sudo`, no role or password change.** DB access worked throughout.
- A container restart interrupted the run once. The interrupted mutant (SIBS) was restored with
  `git checkout` and re-run from scratch; no result below comes from a partial run.
- INV-DEP-1 (FH-3) is out of this range and was not reviewed here.
- **Baseline:** the full `internal/payments` suite is green (303 s), and `internal/reconciliation`
  is green (43 s).
- **Mutation method:** 21 mutants, each applied with an anchor-checked replacement and restored
  with `git checkout`.
  - Every failure was re-run alone twice before it counted as a kill. The machine was shared with
    other agents' suites, and contention produced spurious failures under load.
  - Every survivor was re-run against the **full** `internal/payments` suite.
  - Survivors that touch webhook behaviour were also run against the `internal/httpserver`
    webhook, payment and reversal tests.

## Verdict: APPROVE WITH CONDITIONS

The callback REJECT is lifted:

- The two blocking items from re-review 1 are fixed and pinned by tests: H1 rule 2 (non-final
  reversals never post) and F2 on the callback path.
- Every money-path finding is closed and pinned. None of the open items below lets money move
  wrongly.
- **Conditions, before the stage gate and not blocking FH-3:** add tests for four surviving
  mutants (H2 ×2, SIBS, H1 rule 3), plus the Low items below.

## Item-by-item

| Item | Status | Evidence |
|---|---|---|
| H1 rule 1/4 (final reversal: `succeeded` or legacy `declined` posts or tombstones) | **CLOSED** | `applyReversalReceiptEvidence`; `TestRVLF_P1_ReversalPostsRegardlessOfWireOutcomeReasonCarrier` (narrowed to the two final outcomes) and `P1b` |
| H1 rule 2 (`pending`/`ambiguous` never post or tombstone) | **CLOSED** | Receipt kept under its real outcome, disposition `anomaly`, resolution `anomaly_other` (with `attempt_id` when resolved), P1 audit `payments.reversal_non_final_outcome`, uniform 200. Mutant **H1R2 killed** by `TestRVLF_P1_NonFinalReversalOutcomeNeverPosts`. |
| H1 rule 3 (wire outcome preserved) | **CLOSED in code, fingerprint untested** | `ReceiptEvidence.RawOutcome` feeds `computeEventFingerprint`. `wire_outcome` and `reason` are in the `deposit.reversed`/`deposit.reversal_tombstoned` audit. Mutant **H1R3 (fingerprint ignores RawOutcome) SURVIVES** the full payments suite and the httpserver tests. Condition C1. Minor gap: the bounded reason is kept in audit only, not in the receipt's `decline_reason` column as ruled. That is acceptable (the audit is append-only and durable). Low L-a. |
| H1 rule 5 (contract docs) | **CLOSED (code docs)** | `types.go` `CallbackEvent.Outcome` and `HandleCallback` doc comments state it, with "chargeback won" mapped to unsupported (PROVIDER DEPENDENT). ADR 0095 §9.3's own text is not amended: the rule is recorded in the §27 implementation record instead. That is `architect`'s to fold in; Low L-b. |
| H1 rule 6 (reconciliation) | **CLOSED (doc-only)** | The comment in `payment_statement.go` covers the "no posting expected" half. It omits the other half: a *posted* reversal against a `pending`/`declined` line is the detector for a contract violation or a won chargeback. Low L-c. |
| M1 (terminal amount/asset mismatch) | **CLOSED** | Both terminal cells call `auditTerminalAmountAssetMismatch` (stable P1 action `payments.callback_amount_asset_mismatch_terminal`). Amounts go in the audit only; no state change; no posting; 200. Mutants **M1S and M1D killed** by `TestRVLF_P4_*`. Residual Low L-d: because R0 precedes the matrix, the receipt's `disposition_at_receipt` stays `applied`, and the returned disposition is `duplicate_effect`. The truth is carried by `resolution = anomaly_other` plus the audit. That is acceptable given A7, and must be documented for operators. The live-state mismatch → T10 (M1C) is still pinned only at the HTTP layer: it survives `internal/payments`, and is killed by `TestWebhook_ProviderMismatchAfterVerification_IsDisputedNotRejected` and `TestPaymentWebhook_UniformResponseAcrossDispositions`. That is acceptable. |
| F2 (reason bounded before R0, multibyte-safe) | **CLOSED** | `boundedDeclineReasonAudited` runs at the top of `ApplyReceiptEvidence`, before any receipt insert. Oversize text is replaced by an ASCII sentinel, never truncated, so it is multibyte-safe. The audit carries the byte length and a SHA-256 prefix, never the raw text. Phase C and sweeper sites are also bounded. Mutants **BDR** and **F2R** (the callback-site bound) **killed** by the `TestRVLF_F2_*` tests and my Q3/Q2 probes (committed as `rvlf_i1_ledger_ruling2_integration_test.go`). Low L-e: the oversize audit is written before R0, and is repeated on each redelivery. The audit insert takes no L1 lock, so there is no deadlock risk. |
| H2 (deferred backstop at 3 sites) | **CLOSED in code; 2 of 3 sites untested** | Phase C (DFD) was killed last round by P8. **DFR** (callback-site call) and **DFS** (sweeper T9 site) still **SURVIVE** the full payments suite. Condition C2. |
| H4 (T2 guard, sibling rejection) | **CLOSED** | **T2G killed** by `TestRVLF_F3_T2ClaimRefusesCreatedSiblingOfSucceededIntent`. **SIBD killed** (`TestRVLF_N4_*`). SIBR was killed last round. **SIBS** (sweeper-success sibling rejection) **SURVIVES** the full suite; the T2 guard makes it money-safe. Condition C3. |
| M3 / A7-TOMB-1 | **CLOSED** | The reversal R0 is now the first write (after the payout integrity check), before the parent lock, in both branches, and the branch is decided from the locked re-read. Pinned by `TestRVLF_A7Tomb1_ConcurrentIdenticalTombstoneReversalsNoDeadlockExactlyOneEffect`. My re-review-1 finding about the tombstone branch locking before R0 is fixed. The only earlier write is L-e's audit row. |
| M5 / F3 (sticky freeze; stub status) | **CLOSED** | `finalizeDeclined`/`finalizeAmbiguous` set `intent.Status = actual` (my N1). **FRZ**, **FRZP** and **F3R2 killed** by `TestRVLF_F3_LiveSiblingDeclineAfterT13NeverCreatesOrphanCascade` and my Q2. The single-writer projection concern remains (drive/sweeper still write status directly), but it is money-safe and superseded by FH-3's choke point. Carried to FH-3. |
| N2 (success after tombstone → dispute, phase C and sweeper) | **CLOSED** | Phase C goes to T10. The sweeper goes to T13t from `declined`, else T10. **N2D** and **N2S killed** by the `TestRVLF_N2_*` tests. |
| N3 / S-H1 (event_type allow-list in the deferred replay) | **CLOSED** | The filter is derived from `attempt.Operation`. **DFT** (filter removed) killed by N1 and N3. **DFTX** (payout mapped to deposit) killed by `TestRVLF_N3_DeferredApplyNeverReplaysADepositDeclineAsPayoutEvidence`. The main-path cross-check is now an allow-list too (an unknown `payout_returned` becomes an anomaly). |
| N4 (caller evidence kind) | **CLOSED** | **N4 killed** by `TestRVLF_N4_RejectCreatedSiblingsRecordsCallerEvidenceKind` |
| S-M1 (payout success with a different reference → dispute) | **CLOSED** | T10 `provider_reference_mismatch` plus audit, with the stored reference falling back to the withdrawal's. **SM1 killed** by `TestRVLF_SM10_PayoutSuccessProviderReferenceMismatchDisputes`. That mutant was confirmed by an isolated re-run after contention noise. The payout receipt path is still not live (no adapter emits `payout` events): PROVIDER DEPENDENT. |

## Mutant table, re-run (the survivors from re-review 1, plus this round's fixes)

| Mutant | Result | Mutant | Result |
|---|---|---|---|
| T2G | killed | BDR | killed |
| SIBD | killed | F2R | killed |
| **SIBS** | **survives** (full suite) | M1C | survives payments, killed by httpserver |
| **DFR** | **survives** (full suite) | M1S / M1D | killed |
| **DFS** | **survives** (full suite) | H1R2 | killed |
| DFT / DFTX | killed | **H1R3** | **survives** (full payments suite and httpserver) |
| FRZ / FRZP | killed | N2D / N2S | killed |
| F3R2 | killed | SM1 / N4 | killed |

## Conditions (before the stage gate; not blocking FH-3)

- **C1:** a test that two deliveries of one reversal reference differing only in wire outcome
  (`declined` then `succeeded`) produce two receipts and exactly one ledger effect. This kills
  H1R3.
- **C2:** a callback-path race (callback T4 by merchant reference while a deposit-typed receipt
  is deferred), and a sweeper-poll race, each asserting the deferred receipt is applied and
  resolved. This kills DFR and DFS.
- **C3:** a T13 success through a sweeper poll with a `created` sibling, asserting the sibling is
  `rejected` (`intent_succeeded`, evidence `query_status`). This kills SIBS.
- **Low (L-a to L-e):**
  - L-a: store the bounded reason in the reversal receipt's `decline_reason`.
  - L-b: `architect` folds rule 5 into ADR §9.3.
  - L-c: complete the rule-6 comment.
  - L-d: document that `disposition_at_receipt` is fixed at R0 and that `resolution` is
    authoritative.
  - L-e: skip the oversize audit when the receipt is a duplicate, and move it after R0.

## FH-5 C2/C3 closure confirmation (FH-7, 2026-09-28)

ledger-finance, read-only confirmation for architect item FH7-13 (`rv-fh7-architect-final.md`), verified at HEAD `ada1573` (newer than the evidence commit `f43025c`; all results are for HEAD). Private DB via the sanctioned harness; no roles, passwords or shared infrastructure touched.

| Condition | Status | Code site (HEAD) | Test evidence |
|---|---|---|---|
| **C2 / DFR** (callback-site deferred-apply backstop) | **CLOSED** | `internal/payments/receipt.go:681-689`: when `changed`, re-read with `GetAttemptByID`, then `ApplyDeferredReceiptsForAttempt` | `TestRVLF_C2_DFR_CallbackSiteDeferredApplyBackstop` (`migration_0107_integration_test.go:748`): deferred success receipt, then T4 by merchant reference; asserts `state=succeeded` and 0 unresolved receipts |
| **C2 / DFS** (sweeper T9 poll-site backstop) | **CLOSED** | `internal/payments/sweeper.go:437-441` | `TestRVLF_C2_DFS_SweeperSiteDeferredApplyBackstop` (:832): deferred success, then a Pending poll that learns the reference; asserts `state=succeeded` and 0 unresolved receipts |
| **C3 / SIBS** (sweeper T13 success rejects a `created` sibling) | **CLOSED for money safety; Low gap L-f** (ruled evidence assertion missing) | `sweeper.go:526` `rejectCreatedSiblings(ctx, tx, attempt, EvidenceQueryStatus)`; helper `cascade.go:121`, terminal reason `"intent_succeeded"` at :146 | `TestRVLF_C3_SIBS_SweeperSuccessRejectsCreatedSibling` asserted only `state=rejected` |

**What was run:** HEAD exported with `git archive` to a scratch copy; unmutated baseline: the three tests pass. Mutants (each applied to a unique anchor, restored byte-identical, verified with `cmp`):
- DFR (receipt.go backstop disabled): **killed** by `TestRVLF_C2_DFR_*` and `TestRVLF_C2N12_DeferredAmbiguousBackstopAlsoRecomputesIntentProjection`.
- DFS (sweeper T9 backstop disabled): **killed** by `TestRVLF_C2_DFS_*`.
- SIBS (sweeper T13 `rejectCreatedSiblings` disabled): **killed** by `TestRVLF_C3_SIBS_*`.
- SIBSE (new: `EvidenceQueryStatus` → `EvidenceCallback` at sweeper.go:526): **survived** the full `./internal/payments/...` suite.

These match the implementer's evidence (`evidence/prh-i1-mutation-kill.txt`) for DFR, DFS and SIBS.

**Residual L-f (Low, not stage-blocking):** SIBSE is a mislabel only (the sibling is still rejected; the T2 guard holds; no money moves). Close it by asserting `last_evidence_kind = 'query_status'` and terminal reason `intent_succeeded` in `TestRVLF_C3_SIBS_*`.

### Orchestrator addendum (2026-09-28): L-f fixed

The two assertions were added to `TestRVLF_C3_SIBS_SweeperSuccessRejectsCreatedSibling`. Evidence: baseline passes 5/5 under `-race`; the SIBSE mutant now **fails** the test (`last_evidence_kind = "callback", want "query_status"`); `sweeper.go` restored byte-identical (`git diff --quiet`). **C3 is CLOSED as ruled.**
