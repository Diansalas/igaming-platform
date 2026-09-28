# Ledger-finance review — ADR 0102 (2026-09-28)

Reviewed `docs/decisions/0102-…` at `0939c5a`, plus ADR 0104 §1–4 where they touch financial audit. Checked against LF-7 and security's addendum §1 (a)–(e). Read-only: no edits, no DB session, no tests. PostgreSQL behaviour reasoned about but not executed is marked as such.

**Verdict: ACCEPT WITH CONDITIONS.**
- The structure satisfies all three LF-7 requirements: the savepoint rule (§7.1–7.2), a detached raise for failure-path P1s (§7.4), and the drift and backstop sites (§8 rows 2–3).
- No money path, ledger write or balance changes, and no veto trigger applies.
- F1–F3 (HIGH) must be fixed in the ADR before I-core merges. F4–F9 must be fixed before I-wire merges.

**Facts verified in code:**
- `auditMultipleSuccessForIntent` is at `orchestrator.go:989-1024`. Its only callers are `:1062` and `:1072`, both after the dispute has been applied.
- The evidence sites are `receipt.go:993`, `drive.go:354` and `sweeper.go:513`.
- The uniform 200 is written after `WithTenant` returns (`deposit_handlers.go:419-423`, `:583/:593`). The admission slot is released at the end of the handler (`:404-416`).
- `WithTenant` has no retry, and it errors whenever `Commit` errors (`tenant_rls.go:47-64`).
- Reconciliation streams commit in their own transactions and log MISMATCH after commit. casino_statement (`scheduler.go:516`) and the payment_statement match (`:683`) run at REPEATABLE READ.
- `pay_captured_unposted` is unwindowed (`payment_statement.go:941-949,989-1002`). `pay_duplicate` counts only succeeded attempts (`:1087-1095`), and T10/T13d leave the attempt disputed (`attempt.go:655-662`).
- casino_statement uses an all-time MOCK source, and casino_consistency findings are re-detected on every run.
- The kill-switch engage transaction is at `payments_kill_switch_handlers.go:596-648`, with its post-commit alert at `:653`.

| ID | Sev | ADR § | Finding | Required change |
|---|---|---|---|---|
| F1 | **HIGH** | §5, §7.2(4) | §7.2 propagates "any non-PG error", and Go-side attribute validation returns a non-PG error before any SQL runs. So a bad attribute would roll back the T10/T13d evidence transaction or a reconciliation run, which breaks LF-7(1). | `RaiseGuarded` validates **before** creating the savepoint. A validation failure is never propagated: log it, count it (`phase="in_tx"`), and record it for the F3 fallback. Only the §7.2 PG classes and ctx cancellation propagate. Add AL-6a. |
| F2 | **HIGH** | §3.1, §5, §6.3, §8 | Every raise site runs in a tenant session. For every Kind except rows 1 and 8, §8 sets neither a subject tenant nor `in_tx_raisable_by_tenant`. So the tenant INSERT is refused and swallowed, the detached `WithTenant(subject)` is refused again, and the dispatcher's WITH CHECK allows only meta-Kinds. **The P1 is lost deterministically.** | Every platform-owned Kind raised from a tenant session gets subject = the session tenant and `in_tx_raisable_by_tenant = true` (in §8 and the 0110 seed). Add a migration test that walks every Kind and asserts its raise mode is compatible with its scope. |
| F3 | **HIGH** | §7.2(4), §7.3, §7.5 | The swallow allowlist (22, 23, 42501, P0001) contains exactly the deterministic failures. The detached retry repeats the same insert in the same RLS scope, so it recovers only transient causes, and those are the classes that propagate anyway. Log plus metric becomes the only signal, which addendum (b) rejects. The §11 test proves only transient recovery. | Add a **terminal fallback**: when detached attempts are exhausted, `Flush` inserts `alerting.raise_failed` (p1, platform-owned, discriminator `kind:<kind>`, attributes `{kind, sqlstate_class}`) under the dispatcher identity. Widen the §6.3 WITH CHECK to allow it; **security must rule**. The test must use a persistent failure. |
| F4 | MEDIUM | §7.3 | The deferred collector lives on the context, but the sweeper (`:332/:370`), the webhook handler (`:419`, then `:483`) and `sweepTenants` run several transactions per context. An alert swallowed in a rolled-back transaction could flush after a later commit, which violates AL-7. | Bind the collector to one transaction (e.g. `alerting.InTx(ctx, runner, fn)`): flush only after a nil commit, discard on error. The AST test asserts no collector spans two transactions. |
| F5 | MEDIUM | §7.3 | `Flush` (up to 3 attempts with backoff) placed between `WithTenant` and the response would delay the 200 and hold the admission slot. | Run `Flush` after the response is written, or asynchronously with a hard budget. The response stays byte-identical. |
| F6 | MEDIUM | §3.2, §8 rows 3–7 | A `run:<uuid>` discriminator, combined with standing findings that are re-detected on every run, gives a new open P1 per tenant × stream (× provider) **every hour, indefinitely**. | Use a stable discriminator `stream:<stream>[:provider:<id>]`, with `run_id` as an attribute. A persisting condition is then one open alert with growing occurrences; resolving it and re-detecting creates a new alert. Combine with F7. |
| F7 | MEDIUM | §5, §8 rows 6–7 | With stable keys, `INSERT … ON CONFLICT DO NOTHING` at REPEATABLE READ sites against a row committed after the snapshot raises 40001 (PG semantics, not executed). §7.2 propagates it, rolling back the run and its mismatch rows. | Choose one: **(a)** swallow a 40001 raised inside the alert savepoint at REPEATABLE READ, then raise detached; this narrows addendum (a), so **security must confirm**, and a test must prove the snapshot survives `ROLLBACK TO SAVEPOINT`. **(b)** At REPEATABLE READ sites, raise post-commit and detached only; acceptable today, because those findings are re-detected every run. |
| F8 | MEDIUM | §1, §7.5, §7.6 | The backstop table is partly wrong. `pay_duplicate` does not back rows 1–2 (`pay_captured_unposted` does). The "period-bound, not re-covered" claim is wrong for today's state-type and all-time sources. The §7.6 rationale is overstated; the correct reason is that an alerting failure must never roll back a run or its mismatches. | Correct §7.5 rows 1–2 and the §1/§7.6 wording. Split the reconciliation row into state-type (re-detected) and event-type (not re-covered). |
| F9 | MEDIUM | §6.1 | Unless `attempt_no` is pinned for `unrouted` and the meta occurrence is raised only when the insert actually inserted a row, every dispatcher pass appends rows for every open alert until HD-PRH2-4-OPS is answered. | Pin `attempt_no = 0` for `unrouted`. Raise the meta occurrence only when `RETURNING` yields a row. Meta-Kinds do not recurse. |
| F10 | LOW | §7.6, §8 row 8 | The propagated classes (40P01, 57014, 55P03) could still roll back a kill-switch engage, contradicting "never prevent the brake". | Put the raise last, after `audit.Record` (`:642`), or make row 8 post-commit detached only. |
| F11 | LOW | §7.1 | The alert dedup lock inside T10 is undocumented in the lock order. | Add an ADR 0082 note: alert tables are the terminal lock level; after a `RaiseGuarded`, take no business-row lock on a different intent or attempt. |
| F12 | LOW | §7.3 | An ambiguous commit error skips `Flush`. | Document it as covered by the §7.5 backstops and the F3 metric. |
| F13 | LOW | §3.2 | There is no retention policy for the append-only occurrence and delivery tables. | Register a retention/partitioning follow-up. |
| F14 | INFO | 0104 | Consistent with ADR 0104. But `Raise` must **never** write `audit_log`: ADR 0104's subject guard would P0001 outside the savepoint and abort T10. | State this explicitly in 0102. |
| F15 | INFO | §8 | All sites verified; `:1509` dies with E2. | — |

**Answers to the questions asked:**
1. **Savepoint rule.** It is safe except for F1 (Go validation), F10/F11 (low risk) and F7 (REPEATABLE READ with stable keys). The 200 is at risk only from where `Flush` is placed (F5).
2. **Backstop table.** Not accurate (F8), though the mandatory-detached conclusion stands.
3. **Moving raises inside the run and kill-switch transactions.** Safe only with F1, F2 and F7 applied.
4. **The detached retry.** Safe, but ineffective for the swallowed classes (F3) and unsafe across multi-transaction contexts (F4).
5. **Dedup.** Sound for the per-intent payment P1s. The reconciliation keys (F6) and the unrouted handling (F9) grow without bound.

## Required tests (in addition to ADR §11)

1. In T10 via receipt, `drive.go:354` and `sweeper.go:513`, inject each swallowable class and a Go validation failure. Expected: the dispute and its audit commit, the 200 is byte-identical, no ledger transaction exists, SUM(D) = SUM(C), and the in-tx metric increments.
2. A persistent P0001 makes the detached raise fail too. Expected: `alerting.raise_failed` persists, and the 200 is unchanged in body and within its time budget.
3. Table-driven: every Kind's scope, subject and raisable flag let both the in-tx raise and `RaiseDetached` succeed from the site's real session.
4. One context with two transactions, where tx1 swallows and rolls back and tx2 commits. Expected: no alert for tx1. Cover the sweeper and webhook-plus-reversal paths.
5. 25P02 propagates. 40001 and 40P01 raised by the alert statement in a READ COMMITTED transaction propagate, and the idempotent re-apply gives one dispute, one audit, no posting.
6. At a REPEATABLE READ site, with the stable key committed concurrently after the snapshot: the run row and its mismatch rows commit, the alert exists afterwards, and the snapshot survives `ROLLBACK TO SAVEPOINT`.
7. A condition persisting across N runs gives 1 open alert and N occurrences. Resolve, then the next run, gives a new alert.
8. With zero routes, K dispatcher passes over M alerts give exactly M `unrouted` rows and M meta occurrences.
9. `-race -count=50`: two T10s on one intent plus a concurrent detached flush. Expected: one alert, no deadlock, one dispute, zero postings.
10. An injected alert failure in the kill-switch engage transaction: the switch stays engaged and its audit row commits.
11. Mutants that must be killed:
    - validation error propagated;
    - `Flush` called on the error path;
    - a collector shared across transactions;
    - an incrementing `attempt_no` for `unrouted`;
    - a `run:<uuid>` discriminator;
    - the fallback removed.
12. The existing LF-7 test set stays, unchanged.

**Not verified:** any non-MOCK statement source in production; the full lock sequence after the raise; sportsbook-stream windowing; whether any lock or statement timeout is configured; and PostgreSQL's behaviour for 40001 on ON CONFLICT and savepoint recovery at REPEATABLE READ (test 6 must establish both).

**For the orchestrator:** F3 and F7(a) need a security ruling.
