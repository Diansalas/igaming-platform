# Code review — PRH-2 I-core (2026-09-28)

**Reviewer:** `code-reviewer`. The orchestrator recorded this review.

**Scope:** commit `bc73c24` (`i-core-alerting`, based on `b433454`), exported with `git archive`.
The author's later worktree changes (`847a0fb` plus uncommitted edits) were not reviewed.

## Verdict: NOT READY (at `bc73c24`)

Two confirmed durability bugs (F-1, F-2) defeat the "durable" in durable alerting. The escalation and
"no open transaction during Deliver" tests required by the ADR are missing, and that absence is undisclosed. Two of three reviewer mutants survived, and
the third was killed in only 1 of 10 runs.

**What holds:**
- The RLS families and the exclusion set are correct.
- The meta-only dispatcher WITH CHECK covers both tables.
- The triggers force severity, simulation, scope, created_by and the `raise_failed` discriminator.
- The down's RLS disable is sound.
- The metrics carry no tenant label.
- The retry and backoff arithmetic is correct (a fixed-clock probe passed).

**The claim race is sufficient on the UNIQUE constraint alone.** This was forced deterministically with two sessions: B waited on `Lock/transactionid` and returned no row after A committed, leaving 1 `claimed` row. The test does not prove this (F-6).

## Findings

| ID | Sev | Finding | Required change |
|---|---|---|---|
| F-1 | **High** | A claim with no outcome strands the alert forever. A latest `claimed` row is never due again. A crash, deploy or OOM during `Deliver`, or a failed record insert, leaves the alert undelivered with no `dead` row, no meta-alert and no metric. A probe showed 0 deliveries after 5 passes. This is the same issue as security IC-2. | Add a lease: a stale claim becomes due again under the injected Clock. The retry claims a new attempt number, or a stale claim counts as a failed attempt toward `max_attempts`/`dead`. Test a crash between claim and record. |
| F-2 | **High** | `RaiseDetached`/`RaisePostCommit` never detach the context. A probe with a cancelled context wrote 0 rows, not even `raise_failed`. This is the same issue as security IC-5 and LF C-3. | Apply `detachedCtx` inside `RaiseDetached`, preserving the clock, and pass it to `raiseFailed`. Add a cancelled-context test. |
| F-3 | **High** (test gap, undisclosed) | ADR §7's escalation tests are missing: no test route sets `escalate_after`. Mutant MB (escalation predicate reduced to `nextEscalationAt != nil`) **survived**. | Add clock-driven tests: an alert that is not acked escalates only after `escalate_after`; an acked alert never escalates. |
| F-4 | Medium (undisclosed) | ADR §6.1's "no transaction open during `Deliver`" has no guard and no test. | Add the guard, for example by asserting the `Deliver` ctx is not txscope-marked, plus the §7 test. Otherwise amend the ADR and disclose the gap. |
| F-5 | Medium | Backoff timing is never exercised (the test uses `Backoff: 0`). Mutant MA ("`failed` is always due") **survived**. | Add a fixed-clock test: there is no second attempt before the backoff, and there is one after it. |
| F-6 | Medium | `TestDispatcher_TwoDispatchersOneClaim` is mostly vacuous. Mutant MC (claim returns true on conflict) was killed in 1 of 10 runs. | Use a deterministic harness (the same due item given to two `processOne` calls, or a hook after `readDueWork`), or pin the claim at the SQL level. |
| F-7 | Medium (efficiency) | Every pass re-reads all open and acked alerts, plus one route lookup and one `markUnrouted` per unrouted alert, forever. Routes ship empty, so every alert is permanently unrouted. | Resolve routes once per pass. Skip `markUnrouted` when the latest event is already `unrouted` for that step. Optionally filter out terminal states in SQL. |
| F-8 | Low | An acked alert at `unrouted` step n+1 escalates if a route is added later. | Apply `!acked` in the `unrouted` branch when `step > 0`. |
| F-9 | Low | The latest-row ordering by `recorded_at` has no tiebreaker. Mixing DB time with the injected clock can hide a new route from a fake clock set in the past. | Add an `id` tiebreaker, and document that `effective_from` is DB time. |
| F-10 | Low | Default backoff `1<<attempt` seconds overflows at attempt ≥ 34. | Clamp before shifting. |
| F-11 | Low | The down refusal is untested for a routes-only state. | Add that case. |
| F-12 | Low | Migration 0108 does not narrow runtime grants; only init-app-role does. Migration 0102 does it in the migration. | Mirror the REVOKE/GRANT block in the migration, and extend the init-app-role rerun test to the five tables. |
| F-13 | Info | M8 is killed only by the trigger, so the RLS policy itself has no pin. M6 is being fixed. | Add a `pg_policies` pin for the dispatcher `alerts` policy (`cmd = 'SELECT'`). |

## Commands (local, private DB `cr_ic_review`, dropped)

- Forced two-session claim race: confirmed that the UNIQUE constraint serialises.
- Probes:
  - `StrandedClaim`: 0 deliveries (F-1).
  - `BackoffHoldsRetry`: PASS.
  - `RaiseDetachedCancelledCtx`: 0 rows (F-2).
- Mutants:
  - MA **SURVIVED**;
  - MB **SURVIVED**;
  - MC killed in 1 of 10 runs.
- `-race -tags integration` on alerting, db and auth: ok, no DATA RACE.
- vet and lint: 0 issues.
