# Security review — PRH-2 F-kyc (2026-09-28)

Reviewed commit `df73606` (based on `cabca27`) on a private DB, which was dropped afterwards. No role changes.

**Verdict: ACCEPT WITH CONDITIONS.** F-kyc may merge. The single condition, **C-F1**, belongs to the **payments lane (F-pay)**. Fail-closed holds everywhere today.

## Verified

**The savepoint masks nothing that should abort.**
- The evaluation path (`enforcement.go:203-745`) runs SELECTs only. It has no write, lock, `set_config`, sequence use or audit call.
- `RecordDecision` runs outside the savepoint.
- If the outer transaction is already aborted (25P02), opening the savepoint fails and the result is `unavailable`. The next statement then fails and the whole transaction rolls back. This is still fail-closed.

**Fail-closed at every call site.** `unavailableDecision` sets `Allowed=false`.
- Withdrawal: records the decision and returns 503.
- Casino and sportsbook play: record the decision.
- Deposit: unchanged; records no row (pre-existing).
- Payout dispatch: see C-F1.

**One decision row per evaluation.**
- The withdrawal outage path produces exactly one `unavailable` decision and one audit row, with 0 withdrawal requests and 0 postings.
- The play deny paths are pinned.

**Nothing invented.** No KYC threshold or jurisdiction value appears. B4 only adds a `status='active'` filter.

**The test-only DSN `lock_timeout` changes no shared role, DB or credential.**
- Operational note: the `LOCK TABLE` fault injection must not run in parallel on a shared test DB.

**Test runs.**
- `-race` integration passed for kyc, withdrawal, casino, sportsbook and db.
- The httpserver KYC/Withdrawal subset passed (94 tests, including the 503 test).

**Mutants re-killed by security:**
- MB1 (the 503 branch disabled);
- M-SP (the savepoint helper bypassed), killed by both the package test and the HTTP test;
- M-N5 (the N5 guard disabled).

A mutant that only redirected the reads while the savepoint stayed open is equivalent, not a test gap.

## Findings

| ID | Sev | Finding | Required change |
|---|---|---|---|
| **C-F1** | **Med** (payments lane) | N5 correctly makes `DenyForCompliance` refuse `unavailable`. But its existing caller, `ClaimForDispatch` (`payments/payout.go:292-297`, reached from `withdrawal_handlers.go:910`), passes every denial to it. So on a KYC outage the staff "submit payout" call gets a **non-retryable 500**, and the rollback leaves **no decision row and no `withdrawal.submit.http` audit**. The sweeper's T2/T12 checks probably share the path. Fail-closed holds (no `Withdraw`; the request stays `approved`), but the outcome is misclassified and unrecorded. ADR 0096 and the `ErrKYCUnavailable` comment call this caller "future", which is wrong. | (a) In `ClaimForDispatch`, handle `OutcomeUnavailable` before `DenyForCompliance`: record the decision and the denied submit audit so they commit (the LF-I3-3 pattern), and return a result the handler maps to **503**. (b) Same for sweeper T2/T12: retry later, never reject. (c) Test: an outage during submit gives 503; the request stays `approved`; exactly one `unavailable` decision row and one audit row; no attempt row. (d) Correct the "future" wording. **Assigned to F-pay** (registry: PAY-KYC-UNAVAIL-1). |
| F-2 | Low | `RunReadOnlyInSavepoint`'s read-only contract is enforced only by convention. | Optionally add a static test that the evaluator's call graph contains no writes. Keep the doc warning. |
| F-3 | Info | The deposit gate and payout-allow paths record no decision row. This is pre-existing, and F-pay closes it (KYC-ENF-DECISION-ROWS-1). | — |
