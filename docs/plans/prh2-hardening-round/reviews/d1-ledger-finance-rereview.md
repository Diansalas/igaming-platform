_Reviewer: `ledger-finance`. Branch `prh2-d1-poll-amount` @ `ea17d68`. Recorded verbatim by the orchestrator._

# Ledger-finance re-review: PRH-2 D1 at `ea17d68` (fix round `39604a0` and `ea17d68`)

## Verdict: ACCEPT. There are no pre-merge items left for D1

- D1-M1 is closed.
- V2, my re-kill, is done: all 8 mutants were killed.
- The race baseline is clean.
- D1-F1 and D1-F2 are correct from a ledger perspective.
- I do not require the P1 pin inside D1 itself.

## How I verified it
- **Setup:** I exported `ea17d68` with `git archive` and built a fresh private DB, `lf_d1b_20261003`. No role or credential changes. I did not touch any shared cache, directory or other agent's DB. Free disk stayed at 11.9G or more throughout.
- **Race baseline** (`-race -tags integration -count=1 -p 1`, with pipefail and a grep for FAIL):
  - `./internal/payments/...`: ok (408.3s).
  - `./internal/ledger/...`: ok (21.3s).
  - FAIL 0, DATA RACE 0.
- **Mutants:** each run used `-run 'TestPoll|TestFC4|TestF3SM|TestSweepCASNoise|TestDeferredReceipt|TestParkFaultInjection|TestDepRef|TestDepSync|TestRVLF_N2|TestReceipt_|TestA7_3_|TestPollDecline'`. All compiled and were applied with exact anchors. After each one the files were restored and `cmp` against `git show ea17d68` was byte-identical.

| Mutant | Change | Result | Killed by |
|---|---|---|---|
| D-AMT-2 | Missing falls through to the posting | KILLED (5 fails) | TestPollAmount_Missing_NeverPosts_StaysLiveAndAudited, TestPollDeclinedAttempt_MissingEvidenceIsAuditedOnce... |
| D-ECHO-1 | posting keyed on `res.ProviderReference` | KILLED (4) | TestPollDeclinedAttempt_ContradictionAuditedMatchingPostsT13, TestPollReference_EmptyOrMatchingEcho_PostsUnderTheBoundReference |
| D-TOMB-1 | tombstone lookup uses the echo | KILLED (2) | TestPollTombstone_OnBoundReference_DisputesEvenWithAnEmptyEcho |
| D-FC4 | the F-C4 ledger query never matches (`AND false`) | KILLED (2) | TestFC4_PollSuccess...ParksNotErrorLoop, TestFC4_SyncSuccess...ParksNotErrorLoop |
| LF own: F-C4 tombstone filter dropped | `AND transaction_type <> 'tombstone'` removed | KILLED (7) | TestPollTombstone_OnBoundReference..., TestRVLF_N2_DriveGo_SuccessAfterTombstoneDisputesNotIndexError, TestParkFaultInjection_PhaseC_... |
| D-F1-1 | the Decline branch ignores the bound reference | KILLED (4) | TestPollDecline_EchoNeverOverwritesTheBoundReference |
| D-F2-1 | the terminal mismatch audit stores the raw echo | KILLED (3) | TestF3SM_TerminalMismatchAudit_NeverStoresARawEcho |
| D-M1-1 | Missing on a declined attempt is dropped silently again | KILLED (1) | TestPollDeclinedAttempt_MissingEvidenceIsAuditedOnceNoPostingNoStateChange |

My own mutant confirms that the tombstone check keeps priority over the F-C4 ledger conflict, on both the poll path and the phase C path.

- **Cleanup:** DB dropped (`dropped lf_d1b_20261003`, first try); no `lf_%` DB remains; the export is deleted.

## Rulings

### 1. D1-M1: one audit row is enough for D1, with two binding follow-ups (not pre-merge)
Within payments, D1 does all it can do:
- **No posting:** the amount is not confirmed.
- **No state change:** a declined attempt has no transition to disputed under §4.4, and a re-drive must not be invented from evidence without an amount.
- **No re-poll:** `next_action_at` is NULL, so the attempt is not polled again.

The test pins exactly one row with reason `poll_amount_unconfirmed`, no posting, no state change, and no second row on a further sweep.

The louder signals belong outside D1:
- **(a) Binding, under the B1/I-wire gate:** add `payments.poll_evidence_contradicts_terminal_attempt` to the I-wire P1 alert set, alongside the T10 park reasons. One row is enough evidence. Without an alert, nobody is told it exists.
- **(b) Binding, reconciliation (D2 side):** the matcher already treats a succeeded statement line against a non-succeeded attempt as `pay_status_mismatch`. D2 or its follow-up should pin this with a test for a declined attempt (in the T13 shape) plus a succeeded line. That reconciliation finding is the escalation that can actually move money.

### 2. D1-F1 and D1-F2: CORRECT
- **D1-F1:** for a decline, `refPtr` is the bound reference whenever the attempt has one. The echo never overwrites the attempt's or the intent's reference. That removes both the overwrite and the endless retry on the 0099 CHECK or the unique index. A different echo is audit-only, recorded under the validate-or-hash rule.
  - The `else if res.ProviderReference != ""` fallback for an attempt with no bound reference is the old behaviour. The sweeper cannot reach it, because it never polls an attempt without a reference.
  - **Note, not a condition:** if a future caller does reach it, the echo should go through `providerref.Validate` first.
- **D1-F2:** `payments.callback_amount_asset_mismatch_terminal` now stores the bound reference as `provider_reference`. The echo is added only through `echoAuditMeta`, never raw: the test checks that no raw invalid echo appears in any audit row. That is correct for both audit evidence and B3.

### 3. V2: DONE
Table above: 8 of 8 killed. This clears my D1 pre-merge item.

### 4. Survivor classification: CONFIRMED
- **D-ECHO-2** is equivalent and unreachable: `COALESCE(provider_reference, $3)`, a non-empty bound reference is guaranteed, and a differing echo is parked earlier.
- **D-DRAIN-4** was there before D1 and is outside its scope. It is owned by PAY-RECEIPT-T4-DRAIN-TEST-1, which stays binding before the first real PSP.
- The evidence file's total of 36 mutants (34 killed, 2 classified survivors) agrees with what I re-ran.

### 5. Race baseline: CLEAN
Results above: `internal/payments/...` and `internal/ledger/...` both ok, with 0 FAIL and 0 DATA RACE.

### 6. Where the P1 pin belongs: CONFIRMED, I do not require it inside D1
The payments half is already in D1: `DepositDisputeTerminalReasons()` plus D-REASON-1/2. The reconciliation half has to enumerate that list and fail on any reason it does not classify, so it belongs in D2, which can only land after D1 merges. It stays **pre-merge for D2**. It must classify:
- `callback_amount_asset_mismatch` as bound;
- `success_for_never_sent_attempt` as bound or unbound depending on whether the attempt holds a reference;
- `reversal_tombstone_precedes_success` as excluded.

## Items that remain open, none pre-merge for D1
- **Registered and binding:** E1–E3 (PAY-DEPOSIT-ESCALATION-1, before the first real PSP or non-MOCK source), B3r, B4, T4 (PAY-RECEIPT-T4-DRAIN-TEST-1) and H1 (PAY-PAYOUT-UNBOUND-HOLD-1).
- **New binding item, (a) above:** the I-wire P1 alert covers `payments.poll_evidence_contradicts_terminal_attempt`.
- **New binding item, (b) above:** a D2 or reconciliation test for a declined attempt with a succeeded line giving `pay_status_mismatch`.
- **D2:** the V1 re-kill is still pending and can run now that there is disk space; the P1 reconciliation pin is pre-merge for D2.

## Labels
- PAY-POLL-AMOUNT-1, FH7-06, F-C4, the ledger empty-ID guard, D1-F1, D1-F2 and D1-M1: **IMPLEMENTED**.
- PAY-DEFERRED-RECEIPT-SYNC-1: **PARTIALLY IMPLEMENTED** (the T6 and `sync_amount_mismatch` residuals remain).
- Provider: **MOCK**.

## Relevant paths
- /home/user/igaming-platform/internal/payments/poll_evidence.go
- /home/user/igaming-platform/internal/payments/sweeper.go
- /home/user/igaming-platform/internal/payments/receipt.go (`auditTerminalAmountAssetMismatch`)
- /home/user/igaming-platform/internal/payments/poll_amount_integration_test.go
- /home/user/igaming-platform/docs/plans/payment-readiness/evidence/prh2-d1-mutation-kill.txt
- The run script and logs: /tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad/lfd1b.sh, with `lfd1b_out.txt`, `lfd1b_base.log` and `lfd1b_*.log` next to it.
