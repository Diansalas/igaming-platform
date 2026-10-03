_Reviewer: `ledger-finance`. Branch `prh2-d1-poll-amount` @ `07aa48d`. Recorded verbatim by the orchestrator._

> **Orchestrator note (2026-10-03):** this reviewer stopped before the mutant re-kills because free disk fell below the 500M floor (root at 99%, shared Go build cache about 14G). Their re-kill is OPEN and pre-merge, or must be recorded as an explicit exception by the user. The implementer's 29/31 mutant evidence is not independent verification. No shared cache or directory was cleaned (cleanup is not authorized).

# Ledger-finance review: PRH-2 D1 (branch `prh2-d1-poll-amount`, `564c515..07aa48d`)

## Verdict: ACCEPT WITH CONDITIONS. Two pre-merge items, and my mutant re-kill was stopped by low disk

The baseline passed, but I could not re-kill any mutants: free disk fell below 500M after the baseline, so I stopped as instructed.

**Pre-merge:**
- **D1-M1:** a poll success with no usable amount on a declined attempt is currently dropped silently. It needs an audit row and a test.
- **V2:** my own re-kill of 5 mutants, once there is disk space.

The money paths are sound: every pre-posting check and the posting itself key on the bound reference, and nothing posts on a poll whose amount is missing or contradicts the record.

## Test run, and why I stopped
- **Setup:** I exported `07aa48d` with `git archive` and built a fresh private DB, `lf_d1_20261003`. No role or credential changes.
- **Baseline** (`-tags integration -count=1 -p 1`, no `-race`, to save disk):
  - `internal/payments`: ok (399.2s).
  - `internal/ledger`: ok (18.5s).
  - FAIL count 0, no out-of-space errors.
- **Disk:** free space went from 1123M before the baseline to 366M after it. My guard stopped the run before any mutant.
- **Cleanup:**
  - my export is deleted and the DB is dropped (`dropped lf_d1_20261003`, first try);
  - no `lf_%` DB remains;
  - free space is now 419M, still under the limit, so I did not retry.
  - I did not touch any shared cache, directory or other agent's DB.
  - The Go build cache at `/root/.cache/go-build` is shared, and my baseline build probably added to it. Freeing it is for you or the human to decide, not me.
- **V2 (PRE-MERGE):** with at least 1.5G free, I need to re-kill these on a fresh private DB, with `-run 'TestPoll|TestFC4|TestF3SM|TestSweepCASNoise|TestDeferredReceipt|TestParkFaultInjection|TestDepRef|TestDepSync|TestRVLF_N2|TestReceipt_|TestA7_3_'`:
  - Missing posts (D-AMT-2);
  - posting keyed on the echo (D-ECHO-1);
  - tombstone lookup uses the echo (D-TOMB-1);
  - F-C4 ledger check never matches (D-FC4-1/2);
  - my own: F-C4 tombstone filter dropped. If killed, a tombstone on the bound reference must still produce `reversal_tombstone_precedes_success`, not `provider_reference_conflict`.

  The script with exact anchors is ready: `/tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad/lfd1.sh`. If you accept the implementer's 29/31 instead, record it as an exception, not as my verification.
- **D2's V1 re-run is still pending** for the same disk reason, and is separate from this.

## Rulings on the 8 scope points

### 1. Check order, and use of the bound reference: CORRECT
- **Order:** `applyStatusEvidence` calls `checkPollSuccessEvidence` with `boundRef = *attempt.ProviderReference`, which checks in this order:
  1. amount/asset (`pollAmountEvidence`);
  2. the echoed reference;
  3. `foreignReferenceBinding(boundRef)`;
  4. `tombstoneExists(boundRef)`.

  Only then come `postDepositSuccessOrDispute(..., boundRef, attempt.Amount, attempt.AssetCode, ...)` (INV-DEP-1 and the posting) and `ApplySuccess{ProviderReference: boundRef}`.
- **The echo is never used as a key.** It is only compared to the bound reference and audited.
- **No bound reference:** the branch returns an error and never derives a key from the echo. It is unreachable: the sweeper polls only attempts with a bound reference, and a bound reference never changes. Refusing is the right choice.
- **Amount:** the posting uses `attempt.Amount`, never the poll's amount. It is reached only when the poll's amount and asset equal the record.

### 2. Missing on poll (§36.2): the decision is CORRECT, with conditions
- **Never posting is mandatory.** Posting `attempt.Amount` on a success that did not confirm an amount would credit funds the provider never confirmed.
- **Not treating it as a dispute is correct.** A dispute is terminal: a later callback that does carry the amount would then only be recorded, and the money would wait on a manual M1. Leaving the attempt live keeps the automatic path open. The test covers the liveness: a later poll with evidence posts exactly once.
- **D1-M1 (PRE-MERGE, MEDIUM): a declined attempt with Missing is dropped silently.**
  - In `checkPollSuccessEvidence`, `case AmountEvidenceMissing: if !live { return true, nil }` writes nothing.
  - A declined attempt is not rescheduled, so no later poll will look at it again.
  - The provider is asserting success on an attempt we declined, which is the T13 second-capture shape. Today that evidence vanishes, and only a later statement line would show it.
  - **Required:** write one audit row (`payment.attempt_poll_amount_unconfirmed` with `attempt_state=declined`, or the `payments.poll_evidence_contradicts_terminal_attempt` action with reason `poll_amount_unconfirmed`), plus a test: declined, then a Missing poll gives exactly one audit, no posting and no state change. No test covers this today.
- **Audit growth (up to 48 per attempt per day):** acceptable for now. Loud is what we want while the attempt is unresolved.
  - It must be bounded by PAY-DEPOSIT-ESCALATION-1: after the settlement window, escalate (T16 with a P1), then audit only on a state change or at a reduced rate, such as once per day.
  - That is binding and part of the same gate.
- **The missing T16 escalation matters.** Without it, a provider that never sends an amount leaves the attempt live and the intent pending forever, while funds may already be captured and not credited. The only signals today are the audit rows and, once a statement exists, reconciliation's `pay_status_mismatch`. **Binding:** PAY-DEPOSIT-ESCALATION-1 lands before the first real PSP or non-MOCK statement source, the same gate as D2's B1.
- **Binding (adapter contract):** a real adapter's QueryStatus success must carry amount and asset, or its certification must document why it cannot. Record this in §36.2.
- **Interaction with §36.3:**
  - §36.2's reasoning depends on "a callback can still resolve it".
  - But a callback deferred during phase B on an attempt bound at T6 is not drained at T6, or on a Missing poll. It is applied only when a poll with an amount arrives, which may never happen.
  - Reconciliation does flag old deferred receipts (`payment_statement.go:1121`), so the case stays loud.
  - **Binding:** either drain deferred receipts at the Missing reschedule, or close the §36.3 T6 residual. Do it in the same work item as PAY-DEPOSIT-ESCALATION-1.

### 3. `ledger.Post` rejecting an empty `ProviderTxID`: SAFE
- It cannot reject a posting that would otherwise succeed. Migration 0099 adds `ledger_transactions_provider_tx_id_ref_bound CHECK (octet_length(provider_tx_id) BETWEEN 1 AND 255 ...)` as an immediate constraint (not `NOT VALID`).
- So no row with `''` exists, and no insert with `''` could commit.
- The guard only turns a constraint error into `ErrInvalidEntry` before anything is written.
- Leaving both provider fields nil is still allowed.
- The ledger baseline passed.

### 4. F-C4 (`foreignReferenceBinding` also checks non-tombstone `ledger_transactions`): CORRECT
- It stops the endless retry on `ErrIdempotencyPayloadMismatch` when a payout Step B settlement reference already holds the key. The deposit is parked instead, with `bound_to_operation=ledger_<type>`.
- Excluding tombstones keeps the tombstone check winning, which my extra mutant would confirm.
- **It cannot match an attempt's own posting:**
  - a live attempt has no posting, because posting and `ApplySuccess` happen in one transaction;
  - a declined (T13) attempt has none either;
  - a sibling that succeeded on the same reference is already caught by the `payment_attempts` check.
- **Residual:** if another domain's ledger rows ever used the same `provider_id` string, the check would raise a false conflict. That fails loud (the attempt is parked), so it is acceptable.

### 5. Deferred-receipt drains (PAY-DEFERRED-RECEIPT-SYNC-1): CORRECT
- The phase C drain runs under the locks already held, after `ApplySuccess`. The receipt resolves against the succeeded attempt, so it cannot post twice; ledger idempotency is a second guard.
- The poll-success drain at the sweeper runs after `rejectCreatedSiblings`, and the T9 drain now uses the bound reference.
- **The §36.3 residuals are acceptable:**
  - The **T6** residual is tied to the Missing condition above.
  - On the **`sync_amount_mismatch` park**, the deferred receipt stays unresolved on a disputed attempt. Reconciliation flags it once old, and it would only ever be recorded anyway. Follow-up: drain it as recorded-only.

### 6. Behaviour changes beyond the scope
- **Poll Pending no longer writes the echo to `deposit_intents`: ACCEPT.** The bound reference identifies the money and must not change. Writing the echo risked an endless retry on the 0099 CHECK (empty echo) and silently overwriting the intent's reference (different echo).
  - D-PEND-1 is recorded as killed.
  - LOW, not pre-merge: a different, non-empty echo on Pending is now ignored silently. Add an audit row (no money moves, but it shows the provider is confused).
- **Contradictions on a declined attempt are audit-only: ACCEPT.** This matches §4.4: a declined attempt has no transition to disputed, and a contradicting success is a P1 audit with no posting. The test covers it. The Missing-on-declined gap is D1-M1 above.
- **Poll tombstone T10 now goes through `parkDepositAttempt`: ACCEPT, and it is an improvement.** It gains the audit row (with `adapter_outcome`) and the intent recompute, so the intent becomes ambiguous rather than staying stale. It is called with `bindRef=""`, so no rebinding, and the reason string is unchanged.

### 7. B2, B3 and the reason strings
- **B2 is met.** Every poll park passes `bindRef=""`, and `ApplyDisputeFromNonTerminal` does not touch `provider_reference`, so the bound reference X stays on the attempt. Reconciliation's bound-park clearing on X works.
- **B3 is met on the payments side.** The echo Y is audited as `echoed_provider_reference` only when `providerref.Validate` passes. Otherwise only the reason, length and hash prefix are recorded, so an invalid echo is never stored.
  - The reconciliation half (clearing on X or Y) is still open.
  - **Ruling for that follow-up:** reconciliation should not parse audit JSON as money evidence. Persist Y as structured evidence (a column or an evidence row) when the clearing rule is built. Audit-only is acceptable for D1.
- **The strings are exact:** `poll_amount_mismatch` and `poll_reference_mismatch`. `DepositDisputeTerminalReasons()` enumerates every write site, and D-REASON-1/2 pin it on the payments side.
- **New input for the P1 reconciliation pin.** The list contains three reasons reconciliation does not classify today:
  - **`callback_amount_asset_mismatch`:** the same captured-but-unposted exposure as `sync_amount_mismatch`. Classify it as BOUND, and include it in the MA020 widening (B4).
  - **`success_for_never_sent_attempt` (T15):** the provider says it succeeded and we posted nothing. Classify it bound if the attempt holds a reference, otherwise unbound. Never exclude it.
  - **`reversal_tombstone_precedes_success`:** EXCLUDED is correct, because the PSP reversed the capture.

  The P1 pin must enumerate `payments.DepositDisputeTerminalReasons()` and fail on any reason it does not classify. That stays pre-merge for whichever of D1 or D2 merges second.

### 8. The two surviving mutants: both rulings confirmed
- **D-ECHO-2** (`ApplySuccess` linking the echo) is EQUIVALENT. It updates with `provider_reference = COALESCE(provider_reference, $3)`. At that point the attempt always has a non-empty bound reference (there is an explicit guard), and the echo is either empty or equal to it (a different echo was parked earlier). Two independent reasons the row cannot change.
- **D-DRAIN-4** (`receipt.go:686`, the callback path's T4 drain): it was there before D1 and is outside D1's scope. **Binding follow-up:** add a test that kills it before the first real PSP (suggested ID PAY-RECEIPT-T4-DRAIN-TEST-1). It protects money correctness and nothing tests it today.

## PAY-PAYOUT-UNBOUND-HOLD-1 (my D2 PO-2): register it as binding, before the B1 gate
- **Rule:** a payout attempt that is disputed with an unbound reason must keep its withdrawal hold. The hold must never be released automatically, by a sweeper, a timeout or an ordinary decline path. It may be released only through the governed manual path (reason code, four-eyes above the threshold, audit), after the PSP confirms the money was not paid.
- **Test:** park a payout with an unbound reason, then run the sweeper and expiry. The hold must stay intact and the balance projection must be unchanged.
- **D1's F-C4 is not a substitute.** F-C4 parks the deposit when its reference collides with a payout settlement key, and it never touches a payout hold. It does show that payout settlement references share the `(tenant, provider, provider_tx_id)` namespace with deposits.
- **HOLD-1 should also check the reverse case:** a payout whose Step B settlement reference collides with an existing deposit key. Does it park, and keep the hold, rather than retrying forever on the ledger's idempotency check? If there is no payout-side F-C4, add one under HOLD-1.

## Conditions table

| ID | Severity | Item | When |
|---|---|---|---|
| D1-M1 | MEDIUM | A Missing poll on a declined attempt is silently dropped. Add an audit row and a test. | **PRE-MERGE** |
| V2 | process | My re-kill of D-AMT-2, D-ECHO-1, D-TOMB-1, D-FC4 and the F-C4 tombstone-filter mutant (blocked by disk) | **PRE-MERGE** (or a recorded exception) |
| P1 | MEDIUM | The reconciliation pin over `DepositDisputeTerminalReasons()`, including classifying `callback_amount_asset_mismatch` (bound) and `success_for_never_sent_attempt` | **PRE-MERGE**, for whichever of D1/D2 merges second |
| E1 | binding | PAY-DEPOSIT-ESCALATION-1: T16 and a P1 after the settlement window, and bound the unconfirmed-poll audit rows | before the first real PSP or non-MOCK source |
| E2 | binding | Drain deferred receipts at the Missing reschedule, or close the §36.3 T6 residual | with E1 |
| E3 | binding | Adapter contract: QueryStatus success carries amount and asset (§36.2) | before adapter certification |
| B3r | binding | Reconciliation clears on X or Y, with Y stored as structured evidence, not audit JSON | reconciliation follow-up |
| B4 | binding | Widen MA020 to `poll_amount_mismatch`, `poll_reference_mismatch` and `callback_amount_asset_mismatch` | with or after D1 |
| T4 | binding | A test that kills D-DRAIN-4 (`receipt.go:686`) | before the first real PSP |
| H1 | binding | PAY-PAYOUT-UNBOUND-HOLD-1, including the reverse-collision case | before the B1 gate |
| L1 | LOW | Audit a different, non-empty echo on poll Pending | next time this code is touched |

## Labels
- PAY-POLL-AMOUNT-1, FH7-06, F-C4 and the ledger empty-ID guard: **IMPLEMENTED** (once D1-M1 and V2 are done).
- PAY-DEFERRED-RECEIPT-SYNC-1: **PARTIALLY IMPLEMENTED** (the T6 and `sync_amount_mismatch` residuals remain).
- PAY-DEPOSIT-ESCALATION-1 and PAY-PAYOUT-UNBOUND-HOLD-1: **NOT IMPLEMENTED**.
- Provider: **MOCK**.

## Relevant paths
- /home/user/igaming-platform/internal/payments/poll_evidence.go (the `case AmountEvidenceMissing: if !live { return true, nil }` silent path)
- /home/user/igaming-platform/internal/payments/sweeper.go
- /home/user/igaming-platform/internal/payments/drive.go (`foreignReferenceBinding`, the phase C drain)
- /home/user/igaming-platform/internal/payments/deposit_terminal_reasons.go
- /home/user/igaming-platform/internal/ledger/lockorder.go
- /home/user/igaming-platform/internal/payments/poll_amount_integration_test.go
- /home/user/igaming-platform/docs/plans/payment-readiness/evidence/prh2-d1-mutation-kill.txt
- The V2 script, ready to re-run: /tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad/lfd1.sh. The baseline log is next to it as `lfd1_base.log`.
