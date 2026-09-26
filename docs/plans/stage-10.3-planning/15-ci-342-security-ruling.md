# CI #342/#347 — `security` ruling on the `TestStoreOutage_DoesNotPinPool` changes

Reviewer: `security`. Branch `claude/focused-wright-jw88w9`, HEAD `25a3537`.
Rules on: `9df5869` (change A, test-only pool of 64 connections) and
`25a3537` (change B, CI runs the test alone in its own blocking step).
Inputs: `07-w2a-design-review-security.md` §5, `14-ci-342-store-outage-test.md`,
`internal/secretstore/fetcher.go`, `internal/providercred/resolver_integration_test.go`,
`.github/workflows/ci.yml`. Binding rule: "Do not weaken timing thresholds or
remove concurrency/security coverage merely to make CI green."

| Change | Verdict |
|---|---|
| A. `storeOutagePool` = 64 connections | **REJECT** (replacement required, see §3) |
| B. Run the test alone in a separate blocking CI step | **ACCEPT WITH CONDITIONS** (see §4) |
| New finding F-POOL-1 (design, not test) | **Medium. Blocks launch until it is fixed or the human explicitly accepts it** (see §2) |

## 1. What I measured

All runs used `-race -tags=integration`, one package, no other load (4 vCPU).
To vary the pool size and log the unrelated-query latency on passing runs, I
made temporary edits to the test file. I made the mutation edits to
`fetcher.go`. I reverted every edit, and `git status` / `git diff` are clean.
I committed nothing.

Unrelated-tenant query latency during the outage (the §5 "< 500 ms" bound). Nearly all of it is time spent waiting to acquire a pool connection:

| Pool size | Correct code (HEAD `fetcher.go`) | Mutation (b): 8 slots | Mutation (a): no SlotWait fast-fail |
|---|---|---|---|
| 64 (change A) | 1–18 ms (3/3 pass) | 18–19 ms | 20–28 ms |
| 20 (the old shared fixture) | 206–214 ms (3/3 pass; plus 8/8 isolated passes) | 445–450 ms | 467–470 ms |
| **10 (production default, `config.DatabaseMaxConns`)** | **1.42 s: FAILS 3/3, with no contention** | – | – |

Mutation checks against the committed test (pool 64). This re-verifies the claims in the `qa` doc:

- (a) I changed `acquireSlot` to block until a slot is free (no 250 ms fail-fast). The test **fails** 2/2 on the "long" count: 8 held, durations 2–4 s.
- (b) I changed `MaxConcurrentStoreCalls` from 4 to 8. The test **fails** 2/2 on `MaxConcurrent()`: 8 against a bound of 4.
- (c, extra) I changed `SlotWait` from 250 ms to 350 ms. At pool 64 the test **fails** 2/2 on the "long" count, because some callers took 407–479 ms. At pool 20 it passed 2/2. This shows that the "long" bucket, not the pool, catches wait-time regressions.
- `fetcher.go` was reverted after each mutation, and `git diff` is empty.

Correction to `14-ci-342-store-outage-test.md` §6(b): the doc says that with the 64-connection pool the unrelated query "in some runs … crossed 500 ms" under the 8-slot mutation. I could not reproduce that. At pool 64 it stayed at 18–19 ms. That sentence most likely describes the old 20-connection geometry. It must not be cited as evidence that the 64-connection pool still detects pinning.

## 2. Finding F-POOL-1: §5's pool-pinning property does not hold at the production pool size

**Severity: Medium** (cross-tenant availability / noisy-neighbour). This is a
design finding against the W2a §5 claim that the semaphore "bounds how many
pooled DB connections can be held waiting on the store". It is not a test defect.

- **Mechanism.** The semaphore limits *store-call holders* to 4. Every other
  caller still holds its tenant transaction, and so a pool connection, for up
  to `SlotWait` (250 ms). This covers a flight owner that loses the slot race
  (`acquireSlot`) and a follower joining a flight (`await`). Resolution happens
  inside the caller's `WithTenant` transaction (`webhookauth/scheme.go:633,647`).
  A slot-wait loss does not write a negative-cache entry
  (`own` → `fl.err = ClassUnavailable`, no `negative.put`). As a result, each
  new callback for that ref waits the full 250 ms again. Under a callback burst,
  all `min(N, pool)` connections stay occupied: 4 for about 2 s, and the rest
  cycling every 250 ms.
- **Concrete failure scenario.** Tenant A's credential store stops responding.
  Tenant A's provider sends a burst of 50 webhooks. With the production default
  of `DatabaseMaxConns=10`, tenant B's unrelated request waits **about 1.4 s**
  for a connection (measured, with no contention). With more callbacks the wait
  grows further, bounded only by the pool acquire timeout. After that, B's
  request fails. This lasts until the breaker opens, which takes about 2 s
  (three counting timeouts). It recurs for refs not yet in the negative cache,
  and on half-open probes.
- **Why the old 20-connection test did not show this.** It never proved the §5
  property at the production pool size. It passed with about 290 ms of margin
  only because 20 connections at 250 ms per turnover clears the queue fast
  enough. Change A widens the gap between what is tested and production (10)
  instead of closing it.
- **Required action** (architect + `security`, recorded as an ADR or a §5
  amendment; not a test tweak). Choose one:
  - (i) make §5 hold at the configured pool size. Examples: a process-wide cap
    on callers *waiting* for the store, sized as a fraction of
    `DatabaseMaxConns`; or failing fast immediately with no slot or flight
    wait when that cap is reached.
  - (ii) state an explicit minimum pool size or burst assumption in §5, and
    enforce it in config validation.
  - (iii) have the human explicitly accept the risk.

  Whichever is chosen, the test must then assert §5 at the production default
  pool size (or the enforced minimum). Until then, **F-POOL-1 must be listed
  as an open launch-relevant finding** in the stage report. It does not block
  closing the CI flake work if §3 and §4 are applied.

## 3. Ruling A (`9df5869`, 64-connection pool): REJECT

The claim that this is "identical or strictly more precise" coverage is **false** for one of the three §5 assertions:

- A pool with at least as many connections as concurrent callers (64 for 51
  callers) makes the "unrelated tenant query < 500 ms" assertion **vacuous**
  as a pool-pinning detector. The 51st caller always gets a free connection,
  no matter how many connections the resolver pins or for how long. My
  measurements show this directly: with every connection-pinning mutation, the
  query stayed under 30 ms. The doc argues that "the only way the unrelated
  query can still be slow is if the resolver itself holds more connections."
  That is wrong. At pool 64, it cannot become slow through connection holding
  at all.
- That assertion is the *only* part of §5 that tests the end-to-end
  cross-tenant property: the pool is not starved. The other two assertions
  (`MaxConcurrent() ≤ 4` and the "long" count ≤ 4) observe resolver-internal
  behaviour. They are independent of pool size and are unaffected by A. The
  mutation checks confirm they still catch (a), (b) and (c). So resolver
  regressions are still caught, but the pool-level property is no longer
  tested. That is removed concurrency coverage, and the binding rule forbids it.
- A would also hide the real risk in §2. The direction of travel should be
  towards production geometry (10), not away from it.
- A is also unnecessary once B is in place. CI #342's pool-queueing mechanism
  only exceeded the bound under cross-package CPU contention. With the test
  run in isolation at pool 20, I measured about 210 ms against the 500 ms
  bound, and 8/8 isolated passes.

**Required replacement:**
- Revert `storeOutagePool` to a size **below** the number of concurrent
  callers. 20 restores the previously reviewed geometry. Alternatively, drop
  the helper and use `newFx`'s `runtimePool`.
- Keep `longSlack = 400ms` and all other bounds unchanged.
- Keep the improved `Fatalf` diagnostics. They are a pure gain and are accepted.
- Update the code comment and `14-…md` §3/§6 so they no longer claim that 64
  is "strictly more precise".
- Moving to pool 10 is the §2 follow-up. Do not do it as part of this fix,
  because it fails today.

## 4. Ruling B (`25a3537`, run alone in a separate blocking step): ACCEPT WITH CONDITIONS

This is not a quarantine. The test still runs on every CI run, with `-race`
and `integration`, and it blocks the job (a normal step, not
`continue-on-error`). All bounds are unchanged, and the `grep` guard fails the
job if the test is renamed or deleted, which would otherwise make `-run` pass
silently. What B removes is contention from *our own sibling test binaries*.
That is an artefact of `go test ./...` package parallelism, not a condition the
§5 design has to withstand. Its wall-clock buckets are a measurement proxy for
admission decisions, so measuring them without self-inflicted CPU starvation is
legitimate. The CI #347 mechanism (a caller that fails fast is resumed late by
the scheduler) shows only that the proxy is noisy. It does not show a resolver
defect. Mutation (c) shows that the 400 ms bucket still catches real
wait-time regressions.

Conditions:
1. The step must stay blocking: no `continue-on-error`, no retry or rerun
   wrapper, no `-count` greater than 1 with "any pass" logic. The `grep` for
   `--- PASS: TestStoreOutage_DoesNotPinPool` must stay.
2. Apply it together with the §3 replacement (pool below the caller count).
   B is what makes that geometry stable, so B must not be used to justify
   keeping A.
3. No other test may be added to this "run alone" lane, or to the `-skip`
   list, without a `security` or `qa` ruling specific to that test. The
   isolation lane must not become a general flake quarantine.
4. If this test fails even once in the isolated step, treat it as a real
   signal to investigate. Do not rerun it until it passes.
5. Minor, not blocking: the isolated step only runs if the main integration
   step passed. If the main step fails, this test's result is missing from
   that run's log. That is acceptable because the job is already red. It must
   not be "fixed" by making either step non-blocking.

## 5. Scope of this review

- **In scope:** the diffs of `9df5869` and `25a3537`; the §5 bounds; the
  `fetcher.go` admission logic (`Fetch`, `own`, `acquireSlot`, `await`); the
  production pool default; and the resolver call sites that run inside a
  transaction.
- **Not in scope:**
  - breaker and negative-cache correctness beyond their effect on the §2 window;
  - the AWS SM adapter;
  - a CI run on GitHub infrastructure. I verified B only by reading it; I did
    not observe it execute in CI;
  - `internal/httpserver` flakes.

Passing this review does not make the resolver "secure" in general. It rules
only on whether A and B preserve §5 coverage.
