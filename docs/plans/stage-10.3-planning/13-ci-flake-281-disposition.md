# CI-FLAKE-281 disposition (Stage 10.3 W3)

Owner: `qa`. Baseline branch `claude/focused-wright-jw88w9`, HEAD `e80114b`.

## 1. What the 30s value actually is

`internal/httpserver/stage9_concurrency_integration_test.go`'s `stage9AwaitAll` helper
(lines ~171-194) races a fixed timeout against `sync.WaitGroup.Wait()` and, on expiry:

```go
t.Fatalf("timed out after %s waiting for %d concurrent operations to finish - possible deadlock/pool exhaustion", timeout, n)
```

This is a **hang/deadlock guard**, not a behavioural, performance, or security assertion.
It asserts nothing about *how fast* the system must respond, only that N concurrent
operations do not hang forever (the comment at the top of the file states this explicitly:
"an explicit bounded wait, never relying on `go test`'s own overall timeout to surface a
deadlock"). All six call sites in the file (the two concurrent-login tests, the catalogue-
read test, the two casino-launch tests, and the Back Office queue-read test) used the same
literal `30*time.Second`. None of the six tests assert on elapsed time — `elapsed` is only
`t.Logf`'d for a human to eyeball. No lockout window, rate-limit timing, or Argon2 cost
value is being checked here (the per-IP auth rate limiter is explicitly disabled in this
file's own server setup, `AuthRateLimitPerMinute: -1`, specifically to keep it out of scope).

Per `docs/plans/stage-10.2-planning/02-ci-flake-281-investigation.md` and planning ruling
R12 (`docs/plans/stage-10.3-planning-gate-proposal.md`), this classification means the
disposition is in-scope for this specialist to change: "raise/scale ... as a reviewed test
change ... never weaken thresholds or remove concurrency/security coverage."

## 2. Change made

`internal/httpserver/stage9_concurrency_integration_test.go`:

- Added `stage9CalibrateArgon2Cost(t)`: times one real, uncontended call to
  `auth.HashPassword` — the exact function and exact configured Argon2id parameters
  (`internal/auth/password.go`'s `defaultArgon2Params`) that every register/login in this
  file already exercises.
- Added `stage9Ceiling(t, n)`: `n * stage9SafetyFactor(15) * calibratedCost`, floored at
  `stage9MinCeiling` (30s, the ORIGINAL fixed value — never lowered) and capped at
  `stage9MaxCeiling` (3 minutes, so a genuine hang still fails in bounded CI-reasonable
  time rather than only at `go test`'s own overall timeout).
- Replaced all six `stage9AwaitAll(t, 30*time.Second, n, ...)` call sites with
  `stage9AwaitAll(t, stage9Ceiling(t, n), n, ...)`.
- Concurrency counts (`n`), assertions, and Argon2 parameters are byte-for-byte unchanged.
  Nothing was skipped, quarantined, or loosened.

The 15x safety factor is derived from the Stage 10.2 investigation's own stress
measurement (`02-ci-flake-281-investigation.md` §3): ~0.74s/login observed under heavy
artificial CPU contention against a typical uncontended Argon2id-64MiB hash of tens of
milliseconds — roughly an order of magnitude of degradation, so 15x keeps margin.

## 3. CI recurrence check (runs #331-#342, branch `claude/focused-wright-jw88w9`)

Checked via the GitHub REST API (`GET .../actions/runs?branch=...` then
`.../actions/runs/{id}/jobs` then `.../check-runs/{job_id}/annotations`, unauthenticated).

| Run | Conclusion | Failure cause |
|---|---|---|
| #331 | failure | `TestCatalogueRLS_DeleteAndTruncateAreRefusedLoudly` (`internal/db`) — CI-331-LOCK TRUNCATE deadlock, already resolved separately |
| #332-336 | success | — |
| #337 | failure | `govulncheck` findings (`awssm`/xml decoder, `conformance`/httptest, `platform.run`/TLS chains) — unrelated dependency-vuln gate |
| #338 | failure | same `govulncheck` findings |
| #339 | failure | same `govulncheck` findings |
| #340 | failure | `golangci-lint` step only (v2.6.2 release binary built with go1.25 refused the go1.26.8 module; fixed by pinning v2.9.0 in `0fbd0dc`) — not govulncheck, not Stage 9 |
| #341 | success | — |
| #342 | in progress at check time | — |

**No occurrence of a `stage9AwaitAll` timeout or any `internal/httpserver` Stage 9 test
failure appears in this range.** CI-FLAKE-281 has not recurred since the original run #281.

## 4. Verification performed

- `gofmt -l` — clean.
- `go vet -tags=integration ./internal/httpserver/...` — clean.
- `golangci-lint run --build-tags=integration ./internal/httpserver/...` — 556
  pre-existing `errcheck` findings, identical count and lines before and after this change
  (confirmed via `git stash`/`git stash pop` diff); nothing new introduced by this change.
- `go test -race -tags=integration -count=1 ./internal/httpserver/... -run TestStage9` —
  **3 consecutive green runs**, all six Stage 9 tests passing each time (~44-56s wall
  total per run; individual login tests logged 7-11s elapsed against derived ceilings of
  ~60-140s in these runs, well inside both floor and cap).
- **Mutation kill**: injected `time.Sleep(5 * time.Minute)` into one goroutine of
  `TestStage9_ConcurrentLogins_SameAccount_NoDeadlockAndCorrectSessionCount`'s login burst.
  Result: `stage9_concurrency_integration_test.go:315: timed out after 2m16.777518s
  waiting for 30 concurrent operations to finish - possible deadlock/pool exhaustion` —
  `--- FAIL` at ~137s (bounded by `stage9MaxCeiling`, well under the 5-minute injected
  sleep). Confirms the guard still catches a genuine hang after scaling, and that the cap
  keeps failure time bounded. The mutation was reverted before committing (no test file
  content changed beyond the calibration/ceiling addition described in §2).
- Floor: no local run's calibrated ceiling ever computed below 30s (`stage9MinCeiling`
  strictly enforced by `stage9Ceiling`'s own `if scaled < stage9MinCeiling` branch), so the
  original guard's minimum sensitivity to a real, fast hang is unchanged.

## 5. Disposition

**IMPLEMENTED.** Registry updated: `docs/governance/task-registry.md` (CI-FLAKE-281 row).
No threshold was weakened (only ever raised, with an enforced 30s floor); no concurrency or
security coverage was removed; concurrency counts and assertions are unchanged. The item
stays open only in the general sense that CI-FLAKE-281 was never conclusively reproduced in
CI (per the Stage 10.2 investigation) — the fix addresses the most-likely-cause mechanism
that investigation identified, per ruling R12's mandate ("resolve if reproducible"), and no
further recurrence has been observed in #331-#342.
