# Ledger-finance review — PRH-2 I-core (2026-09-28)

**Reviewer:** `ledger-finance`. The orchestrator recorded this review.

**Scope:** commit `bc73c24` (`i-core-alerting`, based on `b433454`), reviewed via `git archive`.

## Verdict: ACCEPT WITH CONDITIONS

The in-transaction semantics are implemented as ADR 0102 specifies. The gaps are test gaps, not code defects.
**C-1 and C-2 must land in I-core.**

## Local runs (not CI)

Baseline: `-race -tags integration` on alerting, db and auth, all ok.

| Mutant | Result |
|---|---|
| M4: a flushable `Pending` returned on the error path | KILLED by `TestInTx_RolledBackTransactionNeverFlushes` and `TestInTx_ValidationFailureInRolledBackTransactionDiscarded` |
| M1: savepoint removed | KILLED by `TestRaiseGuarded_PersistentSwallowedFailure_FallsBackToRaiseFailed` |
| New: `RaiseDetached` exhausts retries without calling the fallback | KILLED by the same test |

The reviewer's probes for LF test 1's co-transactional write, LF test 5 and REPEATABLE READ were written but not run, because the tool safety check was unavailable.

## Verified by code reading

- **Narrow swallow.** Only classes 22 and 23, `42501` and `P0001` are swallowed. Every non-PG error and every other SQLSTATE propagates. A unit test pins this.
- **The savepoint encloses only the raise.** A savepoint open or rollback failure propagates. A Go validation failure is registered as `go_validation`.
- **No alert is ever recorded for a rolled-back transaction.** The in-tx row dies with the transaction. `InTx` returns a nil `Pending` on error, which the M4 kill proves. The constructor is pinned.
- **An alert failure can never roll back or corrupt a committed financial write.**
  - Flush and the detached raise run only after a nil commit, in a fresh transaction in the originating scope.
  - They are bounded at 3 attempts, 0/200/800 ms, inside a 5 s `WithoutCancel` context.
  - The terminal fallback `raise_failed` never recurses.
- **`dedup_key` is stable** exactly when the discriminator is. The uniqueness is partial, on non-resolved alerts.
- **REPEATABLE READ.** `RaisePostCommit` opens a fresh READ COMMITTED transaction. But nothing stops `RaiseGuarded` inside a REPEATABLE READ transaction (C-2).

## Conditions

| ID | Sev | Condition | Where |
|---|---|---|---|
| **C-1** | MEDIUM | The core guarantee needs positive integration tests (**LF test 5 cannot be deferred**). (a) `InTx` with a business write before **and** after a swallowed `RaiseGuarded`: both commit. (b) 25P02: a failed statement aborts the outer transaction, then `RaiseGuarded` returns non-nil and `InTx` errors. (c) 55P03 raised by the alert statement: a second connection holds an uncommitted raise of the same dedup key, and the business transaction has `SET LOCAL lock_timeout`. 55P03 must propagate, and the business row is not committed. (d) 40001/40P01 via (c)'s pattern, or the unit classification test. | I-core |
| **C-2** | MEDIUM | AL-11 is not enforced in code. In `RaiseGuarded`, before the savepoint: if `transaction_isolation` is not `read committed`, do not insert. Instead, register the alert on `Pending` for a post-commit detached raise and log `alert_raise_rr_deferred`. Test: a raise inside `WithTenantSnapshot` executes no alert statement in the snapshot and persists after commit. (The fallback is an I-wire static test forbidding `RaiseGuarded` under snapshot closures.) | I-core (preferred) |
| C-3 | LOW | `RaiseDetached`/`RaisePostCommit` should apply `detachedCtx` themselves. This is the same issue as security's IC-5. | I-core (cheap) |
| C-4 | LOW | `errAlertVanished` is dead code. The "dedup race exceeded 2 retries" error propagates as a non-PG error, which matches the ADR's letter. | I-core, optional |
| C-5 | INFO | The renumbering commit must be a pure rename, and the migration up/down/up tests must be re-run after it. | I-core |

## Disposition of the LF tests the author disclosed as not exercised

| LF test | Decision |
|---|---|
| 5 | **Must be done in I-core** (C-1 b/c/d). |
| 6 | The I-core half is C-2. **Binding on I-wire:** the casino_statement and payment_statement sites use `RaisePostCommit` after a nil commit, with `stream:` keys. The run and mismatch rows commit even when the post-commit raise fails persistently. |
| 10 | **Binding on I-wire:** kill switch. With a persistent P0001, then a 40P01, injected into the post-commit raise, the switch stays engaged, its audit commits, and the response is unchanged. |

**Also binding on I-wire:**
- stable `stream:` discriminators, with `run_id` as an attribute (LF test 7);
- LF test 9 (`-race -count=50`, two T10s plus a concurrent flush on one intent);
- INVDEP1-BACKSTOP-BRANCH-TEST-1 before ADR 0102 §8 row 2 is wired.
