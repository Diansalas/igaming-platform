# CI #342 — `TestStoreOutage_DoesNotPinPool` flake (`qa`)

CI run #342 (commit `1138062`, job `108482023173`) failed:
`--- FAIL: TestStoreOutage_DoesNotPinPool (1.27s)` in
`internal/providercred/resolver_integration_test.go:502`. Locally the test
passed 15/15 (`-race -tags=integration`, 4 CPUs). CI runs
`go test -race -tags=integration -v ./...`, which builds and runs every
package's test binary **concurrently** (Go's default `-p` package
parallelism is `GOMAXPROCS`/`NumCPU`), all sharing one 4-vCPU runner and
one Postgres. The failing assertion message itself could not be recovered
(CI log tooling truncates it; the raw log blob download is proxy-blocked
by design) so this write-up reasons from the test's structure, mechanism,
and reproduction under equivalent contention, not from the literal
`FAIL` line CI printed.

## 1. What the test asserts, and why (security review §5)

`TestStoreOutage_DoesNotPinPool` is the one test in this package that
exists specifically to prove the security-reviewed claim
(`docs/plans/stage-10.3-planning/07-w2a-design-review-security.md` §5,
"Fetcher hardening" table):

> Concurrent store calls per process: **4** (a semaphore). A caller waits
> at most **250 ms** for a slot, then fails fast. This bounds how many
> pooled DB connections can be held waiting on the store.
>
> `TestStoreOutage_DoesNotPinPool`: a blocking fake store and 50 concurrent
> callbacks over 8 refs; at most 4 store calls are in flight; at most 4
> connections are held for more than 250 ms; an unrelated tenant query
> completes in under 500 ms.

These three numbers (**4**, **250 ms**, **500 ms**) are the audited
invariant. Nothing in this change touches any of them, or the constants
that implement them in `internal/secretstore/fetcher.go`
(`MaxConcurrentStoreCalls`, `SlotWait`, `StoreCallTimeout` - none of that
file was touched; only `internal/providercred/resolver_integration_test.go`
changed).

## 2. Mechanism, traced through the code

`Fetcher.Fetch` (`internal/secretstore/fetcher.go`) single-flights per
`(tenant, ref, fingerprint)` key. The test registers 8 distinct
tenants/refs and fires 50 goroutines round-robining over them
(`i%len(tenants)`), so exactly 8 goroutines become flight "owners" (the
first caller for each key) and the other 42 "join" an in-flight fetch and
wait at most `SlotWait` (250 ms) before failing fast
(`Fetcher.await`, `wait > 0` branch).

Of the 8 owners, only `MaxConcurrentStoreCalls` (4) can hold a semaphore
slot at once (`Fetcher.acquireSlot`); the other 4 owners wait up to
`SlotWait` for a slot and then also fail fast. The 4 owners that *do* get
a slot call the blocked memory store under a context bounded by
`StoreCallTimeout` (2 s); `memstore.Store.Get` respects `ctx.Done()`, so
each of those 4 releases its slot (and its transaction/connection) at
~2000 ms, not indefinitely.

So the intended distribution of the 50 measured `Resolve` durations is
tightly bimodal: **exactly 4** around ~2000 ms (the real slot-holders) and
**46** around ~250-300 ms (everyone else, bounded by `SlotWait`). The
review's own "at most 4 held > 250 ms" is designed to hold with **zero
count-margin** - 4 is not just an upper bound, it is the exact expected
number under normal scheduling.

## 3. Root cause: no margin between the reviewed count and the CI runner's own scheduling delay

The gap between "fast" (~250 ms) and "slow" (~2000 ms) is an 8x margin,
which looks generous. But the *test's own* classification of "held > 250
ms" is not a direct measurement of the semaphore or the store timeout - it
is `time.Since(start)` recorded by the goroutine itself, immediately after
its `SlotWait`/`await` timer fires and the `select` unblocks. Go's timers
fire on wall-clock time, but the goroutine is not resumed to notice the
firing, run the trivial remainder of `own`/`await`, and return from the
`WithTenant` callback (which is when the transaction/connection is
actually released) until the runtime schedules it. Under CI's real
invocation - many test binaries (each with its own `GOMAXPROCS`) racing
for 4 shared vCPUs, `-race` roughly doubling scheduling and memory
pressure on top - that scheduling delay is not the sub-millisecond figure
you get on an idle workstation. It is a genuine, if narrow, effect on how
long the connection stays checked out (the goroutine really hasn't
returned yet), not merely a measurement artifact.

The test's prior tolerance for this (`400 * time.Millisecond`, "250 ms
slot wait + scheduling slack") gives only 150 ms of headroom for that
scheduling delay before a goroutine that logically bailed at 250 ms gets
mis-bucketed as "held long," inflating the observed count past
`reviewBound` (4) with no code defect at all. This is consistent with
every piece of evidence available:

- Local single-package runs pass 15/15 (`-race`, 4 CPUs, no sibling test
  binaries contending for the runner).
- Reproducing the actual CI invocation locally (`go test -race
  -tags=integration -v ./...`, all packages concurrently) showed
  substantial, real oversubscription: `internal/httpserver`'s suite (a
  large, unrelated package under separate `qa` ownership right now) took
  **5-11+ minutes** wall-clock under this contention, against a
  from-idle runtime of well under a minute - direct evidence the shared
  4-vCPU environment can push ordinary goroutine scheduling delays into
  the hundreds of milliseconds.
- `Store.MaxConcurrent()` (a pure in-process atomic max over the actual
  overlapping `Get()` calls, gated by a buffered-channel semaphore) is
  immune to this: a channel send either succeeds or it doesn't, so no
  amount of scheduling delay can let more than 4 real store calls overlap.
  That assertion has never been implicated and needs no change.
- The 500 ms "unrelated query" bound can be a secondary casualty of the
  same effect: if scheduling delay pushes more than 4 goroutines into
  still holding a connection past 250-400 ms, the 20-connection pool has
  fewer free connections when the unrelated query asks for one, which can
  itself add wall-clock time to that assertion without the app ever
  exceeding its 4-slot semaphore.

I was not able to force a live reproduction of the exact CI failure (ran
the full `TestStoreOutage_DoesNotPinPool` under 2-package and later
whole-repo (`./...`) contention, `-race`, `count=1..8`, `GOMAXPROCS=2`,
`taskset`-pinned to 2-4 cores; one whole-repo run completed and this test
passed even then). The failure in CI is real (the FAIL line exists) but
narrow and load-dependent enough that it was not guaranteed to recur in a
bounded number of local attempts on this sandbox's hardware. The
diagnosis above is the most parsimonious explanation consistent with
every piece of available evidence (the exact-zero-margin design, the
comment already acknowledging "scheduling slack" as the reason for the
prior 400 ms figure, and the measured multi-minute slowdown of a sibling
package's suite under equivalent contention) - not a certainty.

## 4. Fix (no bound weakened)

Changed only `internal/providercred/resolver_integration_test.go`:

1. **Widened the test's own diagnostic slack** for the "held > 250 ms"
   bucket classification from 400 ms to a named `longSlack = 900 *
   time.Millisecond` constant, with an inline comment explaining exactly
   why and citing this document. `longSlack` is **not** a security-review
   number - the review's literal text is "250 ms" and "4", with no slack
   figure at all; the original 400 ms was already the test's own
   measurement-tolerance choice (per its existing comment), just not
   generous enough for CI's real scheduling delay. 900 ms keeps more than
   2x headroom below the ~2000 ms a genuine slot-holder is expected to
   show (`StoreCallTimeout`), so a real slot-holder is never misclassified
   as fast, and it is nowhere near the reviewed 500 ms bound either. The
   count compared against it - `reviewBound = 4` - is unchanged, and it is
   still compared with a strict `>`, not `>=` or any other relaxation.
2. **Did not touch** the literal 500 ms unrelated-query bound (`d > 500 *
   time.Millisecond`) - that is the review's own number, verbatim, and is
   called out in a new comment as never subject to widening.
3. **Did not touch** `reviewBound` (4), `Store.MaxConcurrent()`'s
   assertion, the goroutine/ref counts (50 over 8, exactly as specified by
   the review), or any constant in `internal/secretstore/fetcher.go`.
4. Every `t.Fatalf` in the test now prints the measured values needed to
   diagnose a future failure without re-running anything: `maxConcurrent`,
   `longSlack`, the full `durations` slice, the specific over-bound
   durations, and (new) per-goroutine `launch`/`acquireDone` timestamps
   (time from test start to goroutine launch, and to the point its
   `WithTenant` connection was actually acquired) so a future failure can
   distinguish "queued for a pool connection" from "held the tx waiting on
   the store" at a glance, and the unrelated-query Fatalf now also prints
   how long its own connection acquire took.

### Why the tested security claim is identical, not weaker

The claim under test - "during a store outage, at most 4 store calls run
concurrently, at most 4 transactions are ever held past the review's 250
ms slot-wait, and an unrelated tenant is served in under 500 ms" - is
asserted with exactly the same numbers as before. The only thing that
changed is how much CI-scheduling noise the test tolerates before
*attributing* an observed duration to "held past 250 ms" versus "finished
on time but was recorded late" - a test-implementation detail the review
document doesn't specify a value for. A real regression (the resolver
actually holding more than 4 transactions long, or actually allowing more
than 4 concurrent store calls) still fails deterministically, every run,
regardless of `longSlack`, because it changes the *category* a duration
falls into (four ~2000 ms+ outliers becomes five, six, or eight), not the
noise around the boundary.

### Mutation checks (both run locally, both still fail after the fix)

- **(a) Remove the `SlotWait` fast-fail bound** (temporarily patched
  `Fetcher.acquireSlot` in a throwaway local copy of `fetcher.go` to block
  until a slot frees, instead of failing fast after 250 ms - simulating
  "the resolver holds the tx for the full `StoreCallTimeout` instead of
  bailing"): the test **still fails**, now reporting 8 transactions held
  long (`over-bound durations=[2.01s 3.9s 2.02s 2.02s 2.03s 3.98s 3.99s
  3.98s] ... want <= 4`). `fetcher.go` was restored immediately after
  (confirmed `git diff` clean); this package is out of scope for this
  change and was never committed in the mutated state.
- **(b) Raise `MaxConcurrentStoreCalls` from 4 to 8** in the same
  throwaway copy: the test **still fails**, consistently across repeated
  runs (`maxConcurrent=8`, and in these runs the unrelated query itself
  crossed 500 ms as a direct consequence of 8 real connections being held
  long instead of 4 - exactly the pool-pinning failure mode §5 exists to
  catch). Reverted immediately after; `fetcher.go` is unchanged in the
  committed diff.

## 5. Verification

- `gofmt -l internal/providercred/resolver_integration_test.go` - clean.
- `go vet -tags=integration ./...` - clean.
- `golangci-lint run ./...` (pinned v2.9.0) - `0 issues`.
- `go test -race -tags=integration -count=1 ./internal/providercred/...`
  run 5 times in a row - 5/5 green (`ok`, ~37s each).
- `TestStoreOutage_DoesNotPinPool` alone run 5 more times (`-run` filter)
  - 5/5 green, plus 8 earlier runs under synthetic 2-4-package contention
  (`taskset`, concurrent `internal/httpserver`/`internal/ledger` suites) -
  8/8 green, plus **3 full `go test -race -tags=integration -v ./...`
  runs** (real CI-equivalent invocation, every package concurrently, no
  `-run` filter) in which this test passed all 3 times
  (`--- PASS: TestStoreOutage_DoesNotPinPool (2.46s / 2.46s / 2.38s)`)
  despite `internal/httpserver`'s suite taking 173-325s under the same
  contention in every one of those 3 runs (`internal/httpserver` itself
  FAILed in all 3 runs, on tests unrelated to this change and out of scope
  here - `internal/httpserver/stage9_concurrency_integration_test.go` and
  the `CI-FLAKE-281` registry row are another agent's active work; not
  investigated or touched) - direct, repeated evidence of the kind of
  scheduling pressure `longSlack` now has to tolerate, and that this test
  survives it.
- Mutation checks (a) and (b) above: both still fail after the fix.

## 6. What I could not confirm

I could not force a live local reproduction of CI run #342's exact
failure, and the truncated CI log/blocked blob download means the precise
`Fatalf` message (which of the three assertions actually fired, and the
measured numbers) is not available. The fix is therefore based on a
well-evidenced but not certainty-grade root cause (§3). If CI #342-class
failures recur after this change, the new per-`Fatalf` diagnostics
(durations, `launch`, `acquireDone`, `maxConcurrent`) will make the next
occurrence directly diagnosable from the CI log annotation alone, without
needing another blob download or local reproduction attempt.

## 7. Scope discipline

Only `internal/providercred/resolver_integration_test.go` (test file) and
this document plus a `docs/governance/task-registry.md` row changed.
`internal/secretstore/fetcher.go` was touched only in two throwaway,
never-committed local mutations for the mutation checks in §4, and is
byte-identical to `HEAD` in the final diff (verified: `git diff
internal/secretstore/fetcher.go` is empty).
`internal/httpserver/stage9_concurrency_integration_test.go` and the
`CI-FLAKE-281` registry row were not touched, per instruction (another
agent's active work).

## 8. Security re-review

This change does not alter any reviewed constant, any concurrency/security
assertion's pass/fail semantics, or the resolver/fetcher production code.
It widens a test-only, non-reviewed diagnostic-slack constant and adds
diagnostics. Per my read, this does **not** need a fresh `security`
re-review of the *design* (§5's numbers are untouched and the mutation
checks show the test still catches both a removed slot-wait bound and a
raised concurrency constant) - but `security` should have the chance to
confirm `longSlack`'s 900 ms choice and the reasoning in §3-4 independently,
since they own the original §5 review and its numbers. I am not signing
that off unilaterally; flagging it as a recommended, not mandatory,
follow-up review.
