# A8 round 2: security re-review (live)

Reviewer: `security` specialist. Code reviewed: the integration tip `12990ce`. That is the A8 round-2 commit `bc4a06a`
merged with A7 round 3 (`ca3e25c`) plus the scan-test `.claude` skip. Reviewed on 2026-10-15 against a private database
(`sr8b_*`, built from a `git archive` export of `12990ce`, dropped afterwards). No roles or passwords were changed. No
repository code was modified. Probes and mutants ran in the throwaway export only, and the export has been deleted.

Earlier review: `a8-security-review.md` (SR8-1..10). Round-2 record: `docs/plans/a8-market-blocking-record.md` §10.

## Verdict

**SR8-1, SR8-2, SR8-3, SR8-4 and SR8-5 are CLOSED as specified.** Each original repro now refuses, fails closed, and
writes no ledger or exposure rows.

This round found no new merge-blocking or launch-blocking finding. The new findings are three LOW and four INFO
(SR8R-1..7 below). None of them lets anyone past the gate.

**Launch note for the human (unchanged):**
- With no geolocation vendor, the gate refuses every registration, deposit, launch and sportsbook bet wherever no static
  country is configured. That is the recorded fail-closed state, pending HDR-J-7/J-8 and LOCATOR-VENDOR-1.
- The SR8-2 residual is PROVIDER DEPENDENT and is an owner acceptance, not a security sign-off: a player who travels into
  an excluded country while their brand's market stays open is bounded only by `CASINO_SESSION_MAX_LIFETIME`.

**Not in scope:**
- `internal/operatingmarket` resolver internals.
- The A7 AML subsystem itself. I only checked how it merged into readiness.
- Any real vendor.
- Penetration testing.

Passing this review does not make the feature "secure".

## What was run

- **Existing suites, all PASS** (integration, `-count=1 -p 1`, private DB):
  - whole packages: `marketgate`, `config`, `launchgov` (includes the A7 `readiness_aml` tests), `geolocation`
  - `casino`, `sportsbook` and `payments` with `-run 'Market|Lifetime|Exposure|Bootstrap'`
  - `httpserver` with `-run 'Market|SR85|Sportsbook|Register'`
- **Unit tests under `-race`:** the marketgate Locate and sampler tests, plus my probes.
- **Temporary probes** (described under each item below).
- **Three mutants** against the SR8-3 scan and the SR8-5 registration ordering.

## Per-finding verification

### SR8-1 (sportsbook placement gate): CLOSED

- **Same two-layer pattern as deposit and launch.**
  - `newPlaceBetHandler` calls `Locate` outside the transaction, using `RemoteAddr` only.
  - It then evaluates the guard inside the placement transaction, *before* `wallet.GetOrCreate`.
  - `sportsbook.PlaceBet` evaluates it again as its first statement after validation, before the idempotency lookup and
    before any catalogue, RG, risk, lock or insert.
  - `insertBet` has exactly one caller (`PlaceBet`), and `PlaceBet` has exactly one production caller (the handler).
    There is no other placement path.
- **Fails closed on unknown evidence and on resolved-not-permitted.** Both give the one canonical 403. Pinned by
  `TestMarketGate_HTTP_SportsbookEveryCauseIsOneRefusalAndLocksNothing`, which checks no bet row, no ledger row and one
  audit row per refusal.
- **Nothing is locked or written on refusal.** `TestPlaceBet_MarketGuardRefusalLocksNoStakeAndWritesNothing` checks cash
  balance unchanged, locked balance 0, 0 ledger transactions and 0 bets. No wallet is created either, because the
  handler gate runs before `GetOrCreate`.
- **Guard errors.** A guard error is a 500 and writes nothing. An empty guard gives `ErrGuardRequired`. `Deps.MarketGate
  == nil` gives a 503 before anything is read (`TestMarketGate_HTTP_SportsbookNilGateFailsClosed`).
- **Replay interplay.** A replay of an *accepted* bet's idempotency key from refused evidence gets a 403 and does not see
  the original bet (`TestPlaceBet_MarketGuardRefusesAReplayOfAnAcceptedBet`).
  - Nothing is duplicated: the original bet stays exactly once, and the DB unique key is still the authority.
  - Nothing is stranded: the bet settles through the normal settlement path, which is not gated.
  - The client-visible side effect is SR8R-5.
- **Client headers cannot move the country:** `TestMarketGate_HTTP_SportsbookClientHeadersCannotMoveTheCountry`.
- **Mid-request configuration flip is caught:** `TestMarketGate_HTTP_SportsbookPlaceBetEvaluationRefusesWhenConfigurationFlipsMidRequest`.

### SR8-2 (running exposure): CLOSED as specified (residual PROVIDER DEPENDENT)

**(a) Maximum lifetime of a consumed session**
- `postBet` runs `clock_timestamp() - created_at > max` on the session row.
- Database clock on both sides. `created_at` is `DEFAULT now()`.
- `created_at` cannot be changed: my probe P7 ran an UPDATE and got `casino_launch_sessions: identity/token/expiry
  columns are immutable after insert` (trigger from migration 0036). A session's lifetime cannot be extended.
- The lifetime is absolute from mint, not from bootstrap. Default 4h. Config bounds are 1m..72h, and an out-of-range
  value refuses startup.
- Wired in `cmd/platform-api/main.go`. A check error returns a Go error, so the transaction rolls back (fail closed).

**(b) Brand-level "market still open" check**
- `marketgate.BrandOpen` uses the DB clock and runs the real resolver (licence ceiling, tenant, brand and operation rungs)
  for any enabled tenant-rung country.
- It runs on new casino bets only. It sits after the replay and tombstone short-circuits and after the tenant and brand
  status gates, and before the capability gate, the player lock and any ledger write.
- A closed market is a committed DECLINE (`market_unavailable`) with the audit row `casino_bet.denied_market_closed`.
- An error is a Go error (fail closed).
- Sportsbook relies on its per-request gate, which is strictly stronger.
- Probes P5 (brand-rung disable) and P6 (`wagering`/`casino` operation-rung disable) both DECLINE.

**(c) `BootstrapLaunch`**
- A `market_closed` denial revokes the token, audits and commits (`TestBootstrapLaunch_TokenMintedBeforeCloseIsRefusedAfterIt`).

**Ledger-finance angle (probes, all on the real check):**

| Probe | Result |
|---|---|
| P1: replay of an accepted bet after the market closes | `succeeded, Replayed:true`, the original ledger tx. Not a decline, so the provider never sees "stake not taken" for a stake that was taken |
| P2: replay of an accepted bet after the lifetime has passed | `succeeded, Replayed:true` |
| P3: bet declined for closure, then provider rollback, then reopen, then late original | Rollback writes a tombstone (`Tombstoned:true`). The late original is `declined/original_rolled_back`. Balance unchanged, debits = credits |
| P4: bet refused for lifetime (`ErrLaunchSessionRequired`), then rollback | Tombstone. Balance unchanged |
| Existing tests | Win and rollback of open bets still post after close and after the lifetime. 12 bets racing a disable: each is either posted once or declined with nothing posted, the ledger balances, and the next bet is declined |

A refused bet leaves no half-state: the decline writes only the audit row, and the status lock is released at commit.

**Concurrency.** A bet whose transaction read "open" before a disable committed is accepted. This is the same READ
COMMITTED ordering as the capability and game-kill gates (SR8-9 class, accepted). There is no window in which a
declined bet is posted.

**Cost.** See SR8R-1.

### SR8-3 (mandatory guard and code scan): CLOSED

- **Runtime check.** `LaunchGame`, `InitiateDepositAttempt` and `PlaceBet` each refuse a nil guard with
  `ErrGuardRequired` before reading anything. This is the real control.
- **Scan, sentinel test.** `TestProductionCodeNeverUsesTheUngatedSentinels` covers both `UngatedForTests` and
  `WithUngatedMarketOpenForTests`.
- **Scan, mutant 1.** I removed `MarketGuard: marketGuard` from the sportsbook handler. The scan FAILED, so the mutant was
  killed.
- **Scan, mutant 2.** I aliased the import (`sb.PlaceBet`). The scan reported "found no production caller", so the mutant
  was killed, but only because that file is the sole caller (SR8R-4).
- **`.claude` skip.** The scan skips directories named `.claude`, `node_modules`, `.git`, `vendor` and `dist`. The repo
  contains no `.go` file under any directory with those names (`git ls-files`), so the skip hides no production code.
  The Go toolchain ignores dot-directories anyway.

### SR8-4 (hard Locator deadline): CLOSED

- `Locate` runs `GetSignal` in a goroutine and selects on `lctx.Done()`.
- A late answer is discarded: the buffered `done` channel means a late send never blocks, and the goroutine also
  rejects a result once `lctx.Err() != nil`.
- A panic is recovered as unknown.
- Probe S3 (`-race`): a Locator that ignores cancellation and hangs, then 2000 `Locate` calls:
  - exactly 128 goroutines are added;
  - 0 calls return known evidence; over the cap the answer is immediately unknown;
  - after release, 0 slots are held and the goroutine delta is 0;
  - a timeout never allows.

### SR8-5 (registration pre-check and audit sampling): CLOSED, with SR8R-2 and SR8R-3

**Pre-check**
- It runs the same guard (same evidence) in its own tenant transaction before `auth.HashPassword`.
- A pre-check refusal commits its audit row and returns 403. A pre-check error returns 500.
- An allow only lets the handler continue: the in-transaction check before `RegisterPlayerWithResolution` is still
  present and authoritative (verified in code).
- The pre-check cannot allow anything on its own. However, nothing behavioral pins that the authoritative check still
  exists (SR8R-3).

**Sampling**
- Key: (tenant, brand, player, operation, reason). No IP or hash.
- Token bucket: burst 5, then 1 per minute. The refusal itself is never sampled.
- Every refusal is still logged per request (`*_refused_market_not_available`, with the request id), so logs keep the
  complete count.
- A player can only suppress rows for their own key, so one player cannot hide another player's rows.
- Anonymous registration refusals share one bucket per (tenant, brand, reason). Those rows carry no identity, so only
  per-request correlation in `audit_log` is lost; logs keep it.
- Memory is bounded: at most 10k keys, a few MB.
- Weaknesses are in SR8R-2.

### A7 merge (launchgov readiness)

- `readiness.go` appends `R-MARKET-CONFIG` and then the three `R-AML-*` checks.
- `SnapshotVersion` 3 is part of the canonical hashed form (`readiness.go` Version field). An approval snapshot computed
  under v2 therefore cannot match a v3 recomputation, so it fails closed.
- `R-MARKET-CONFIG` now calls the shared `marketgate.AnyCountryPermits` for registration, deposit and casino wagering, so
  there is one definition of "open".
- `launchgov` passes as a whole package, including the `readiness_aml` pin (`!= 3`).
- OpenAPI lists both code families. No conflict found.

## New findings

### SR8R-1: LOW (availability; no bypass). The brand-level check costs O(configured countries) on every new casino bet and bootstrap

**Problem:** `AnyCountryPermits` lists the tenant-rung countries in country-code order and runs the full resolver
(about 5 queries) for each one until one permits.

**Measured on the private DB:**
- P8: 95 configured countries, casino closed in all of them: about 100–170 ms per *declined* bet.
- P9: the same, with only `MT` open (it sorts late): about 130 ms per *accepted* bet. So normal play pays the cost
  whenever the open casino countries sort after many closed ones. That is realistic, for example in a tenant with many
  sportsbook-only or deposit-only countries.
- The scan is capped at 250 countries, so the worst case is about 1,250 queries per bet.
- All of this runs inside the bet transaction while it holds the per-provider-tx advisory lock and the shared brand
  status lock, which extends lock hold and connection time under load.

**Fix (any of):**
- a set-based single query, for example: tenant-enabled countries joined to an enabled licence ceiling, excluding brand
  and operation disables, then confirm one candidate with the resolver;
- try the most recently permitted country first;
- memoise per (tenant, brand) per transaction.

Add a latency test with about 100 countries.

### SR8R-2: LOW. Sampling loses suppressed counts, and more than 10k keys defeats it

Probes S1 and S2:
- **(i) Lost counts on reset.** When the map reaches 10k keys it is reset wholesale. This happens in normal operation,
  because buckets never expire. Every pending `suppressed` count is dropped. S1: 40 suppressed refusals, then a reset,
  and the next row says `suppressed=0`.
- **(ii) Tail loss.** Counts are only written on the *next* written row of the same key. A key whose refusals stop is
  never reported. A process restart also drops all counts.
- **(iii) Sampling bypass.** An actor controlling more than 10,000 keys defeats sampling completely. S2: cycling 10,001
  keys wrote 100,010 of 100,010 rows; the control with 10,000 keys wrote 50,000 of 100,000. The player is part of the
  key, so this needs about 3,400 authenticated accounts (3 operations each). That bounds it, but it is feasible.
- **(iv) Cross-tenant effect.** The reset is process-global, so one tenant's key growth erases another tenant's counts.

Nothing is hidden from logs. The record's "volume stays reconstructible" claim holds only through logs, not through
`audit_log`.

**Fix:**
- evict idle, full, zero-suppressed buckets (LRU) instead of resetting;
- on eviction or shutdown, flush non-zero suppressed counts as one summary audit row, or emit them to a counter metric
  `market_refusals_total{tenant,operation,reason}`;
- optionally cap keys per tenant.

### SR8R-3: LOW (test gap; the code is correct today). The authoritative in-transaction registration check is not behaviourally pinned

**Mutant:** delete the in-transaction `marketGuard` call (replace it with an allow) and add a second guard call inside
the pre-check transaction. All `httpserver` `Market|SR85|Regist` tests PASS, so the mutant **survives**.

**Why it survives:** `TestRegisterHandler_MarketPreCheckPrecedesThePasswordHash` only counts at least 2 calls, and no
HTTP test flips the configuration between the pre-check and account creation. Deposit, launch and sportsbook all have
such a flip test.

**Risk if it regresses:** a market disable committed during the Argon2 window would let one registration through.

**Fix:**
- add a flip test, for example a `Checker` whose `Guard` allows on the first evaluation and refuses on the second; expect
  403, no account and no session;
- tighten the AST test so one guard call sits in the same `FuncLit` as `RegisterPlayerWithResolution`, before it.

### SR8R-4: INFO. Limits of the caller scan

The scan matches `sportsbook.PlaceBet` by package identifier, and a call by its literal composite type. It would miss:
- a second caller that uses an aliased import;
- a function value;
- a non-nil but always-allow inline guard.

The runtime nil check is the real control, so this is a review aid only. Keep the "no production caller found" assertion,
since it caught the alias mutant here.

### SR8R-5: INFO. Sportsbook replay from refused evidence answers 403 for a bet that exists

A client that lost the 201 response and retries from refused evidence is told "not available". The bet is accepted, its
stake is locked, and it will settle.

There is no financial defect. Casino deliberately does the opposite (replay before the gate), which is correct for a
vendor protocol.

b2c and support should treat bet history as the source of truth. This needs product and ledger-finance acknowledgement,
not a security fix.

### SR8R-6: INFO (vendor-time). The 128-slot semaphore also refuses healthy load

The cap counts every lookup in flight, not only hung ones. With a real vendor, peak RPS × vendor latency above 128
refuses legitimate players, which fails closed but costs availability. Size the cap at vendor time and add an
"over-cap" metric.

### SR8R-7: INFO

- `R-MARKET-CONFIG` does not cover sportsbook wagering. A brand that only offers sportsbook could pass readiness with
  sportsbook closed everywhere. Refusal still happens at placement.
- A lifetime-refused casino bet is a Go error, not a committed decline. I did not verify which durable rejection row,
  if any, `wrapRejection` records for that class.

## Tests every domain specialist's `qa` coverage must keep

- Everything listed in `a8-security-review.md`, plus:
  - replays of accepted casino bets after a market close and after the lifetime stay `Replayed`, never declined (P1, P2);
  - a closure decline, then rollback, then a late original gives a tombstone and `original_rolled_back` (P3);
  - the registration flip test (SR8R-3).
- **When SR8R-1 is fixed:** a latency or query-count bound for the brand-level check with about 100 configured countries.
- **When SR8R-2 is fixed:** eviction preserves suppressed counts, and more than 10k keys does not reset other tenants'
  buckets.
