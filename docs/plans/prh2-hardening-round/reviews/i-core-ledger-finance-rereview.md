# Ledger-finance re-review — PRH-2 I-core (2026-09-28)

**Reviewer:** `ledger-finance`. The orchestrator recorded this review.

**Scope:** `bc73c24..9f5970c`, reviewed via `git archive`. Tests ran on a private DB (`lf_icore2_20260928`), dropped afterwards.

## Verdict: ACCEPT

Conditions C-1 to C-5 are all met.

| Mutant | Result |
|---|---|
| M4: flushable `Pending` on the error path | KILLED by `TestInTx_RolledBackTransactionNeverFlushes` and `TestInTx_ValidationFailureInRolledBackTransactionDiscarded` |
| M1: savepoint removed | KILLED by three tests, including the new C-1(a) (25P02 on the business write after the raise) |
| MC2: REPEATABLE READ guard disabled | KILLED by `TestLFC2_…` |
| MF: `Flush` retries on `p.runner` instead of the fresh READ COMMITTED runner | **SURVIVES** (N-1, low; correct by inspection) |

## Conditions

| ID | Status |
|---|---|
| C-1(a) | MET. A business write before and after a swallowed raise both commit. |
| C-1(b) | MET. 25P02 propagates. |
| C-1(c) | MET. A genuine 55P03 from the alert statement propagates, and the business row does not commit. |
| C-1(d) | Acceptable. Every non-allowlisted class takes the same path that (c) proves, the unit test pins 40001/40P01, and at READ COMMITTED the alert INSERT cannot raise 40001. |
| C-2 | MET. No alert statement runs inside the snapshot, the alert persists after commit, and the reconstructed runners are READ COMMITTED by inspection. A deferral cannot lose the alert provided the raise is inside `InTx` (see B-1). |
| C-3 | MET. The context is detached at entry. |
| C-4 | MET. |
| C-5 | MET for the rename commit. The later IC-4 and F-12 changes narrow privilege, which is acceptable. |

**Stale-claim lease:** no ledger concern; it affects only `alert_deliveries` bookkeeping. Operational notes for devops:
- the staleness check compares DB time with the injected clock;
- a reclaim changes the sink idempotency key;
- the sink timeout must stay well below `ClaimLease`.

**N-1 (low):** MF survives. The optional hardening is a spy runner, or asserting `SHOW transaction_isolation` inside the retry.

## Binding on I-wire

- **B-1:** a static/AST test that every `RaiseGuarded` call is inside an `alerting.InTx` closure. A call outside it can lose a swallowed or deferred alert.
- **B-2:** the REPEATABLE READ sites use `RaisePostCommit` after a nil commit, with `stream:` keys. LF test 6.
- **B-3:** LF test 10 (the kill switch).
- **B-4:** LF test 7 and LF test 9.
- **B-5:** INVDEP1-BACKSTOP-BRANCH-TEST-1 before ADR 0102 §8 row 2 is wired.
