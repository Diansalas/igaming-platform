# Ledger-finance review — PRH-2 F-kyc (2026-09-28)

**Reviewer:** `ledger-finance`. The orchestrator recorded this review.

**Scope:** commit `df73606` (branch `prh2-f-kyc-outage-testpins`, based on `cabca27`).

**Method:**
- Diff read.
- Tree exported with `git archive` and run on a private DB (`lf_fkyc_20260928`, dropped afterwards) through `priv_db.sh`/`priv_test.sh`.
- No role, credential or repo change.

**Local test runs (not CI), all ok:**
- `-race -tags integration`: `internal/withdrawal`, `internal/kyc` and `internal/db`;
- `internal/payments` (full integration);
- `internal/httpserver -run 'KYC|Kyc|Withdraw'`.

**Mutants (both killed, both reverted):**
- **Savepoint bypassed:** `TestRequestWithdrawal_KYCStoreOutage_FailsClosedWithOneUnavailableDecision` fails with `25P02`.
- **N5 guard disabled:** `TestDenyForCompliance_RefusesUnavailableOutcome` fails.
  - The whole `internal/payments` suite still passes under this mutant, so no payments test covers an `unavailable` payout-gate decision (F-2).

## Verdict: ACCEPT WITH CONDITIONS

F-kyc may merge. F-1 is a test-strength fix. F-2 folds into PAY-KYC-UNAVAIL-1 and must close before payout
dispatch runs against real providers.

## Verified

- **LF-20.**
  - On the `unavailable` path, the decision and audit are recorded in the same transaction.
  - `*KYCDeniedError` is returned before `IdempotentInsert`, so no request row and no hold posting are possible.
  - This is proven by fault injection (`LOCK TABLE` + `lock_timeout`): exactly one `unavailable` decision, one audit row and zero requests.
- **Savepoint containment.**
  - `RunReadOnlyInSavepoint` always rolls back.
  - `RecordDecision` runs outside it.
  - A housekeeping failure maps to `unavailable`.
  - An already-aborted outer transaction still fails on its next statement.
- **`DenyForCompliance` posting is unchanged.**
  - The N5 guard is inserted before `lockRequestForUpdate`.
  - The `withdrawal_rejected` posting, the CAS and the audit are byte-identical.
- **N5 is sound.**
  - `unavailable` is refused before any lock or write. The request stays `approved`, the hold stays in place and there is no reversal.
  - At T1p, `ClaimForDispatch` now returns an error and rolls back. The net money-path behaviour is unchanged from `cabca27`.

## Findings

| ID | Sev | Finding | Condition |
|---|---|---|---|
| F-1 | LOW | The LF-20 ledger assertion in the fault-injection test is vacuous. It counts `ledger_transactions` by the KYC-denial correlation id, but the hold posting correlates to `requestID`. Only the `withdrawal_requests = 0` assertion covers it today. | Assert that the tenant's `ledger_transactions` count is unchanged before and after, and that the `player_cash`/`player_withdrawal_hold` projections are unchanged. |
| F-2 | MEDIUM (payments lane) | **Sweeper T2/T12 now escalate on a KYC outage.** A DB-read outage now yields a clean `unavailable` decision. `gateAndEscalateOnDeny` (`payout_sweep.go:99-129`) treats every `!Allowed` as a deny: it calls `Escalate` (T16) and audits `payments.payout_reclaim_denied_by_kyc`. At `cabca27` the outage aborted the transaction and the sweeper rescheduled. No money moves, but a transient outage now causes a sticky escalation. | Under PAY-KYC-UNAVAIL-1(b), treat `OutcomeUnavailable` as transient: `RescheduleNonTerminal`, no `Escalate`, no deny audit. Tests: an outage at T2 and at T12 leaves the attempt un-escalated and rescheduled, with no Withdraw and no posting. Add the missing payments test for T1p under `unavailable`: the request stays `approved`, with no attempt row and no posting. Registry text: "T2/T12 currently escalate (T16)". |
| F-3 | INFO | The read-only contract of `RunReadOnlyInSavepoint` is enforced by convention only. | Concur with security's optional static test. |
