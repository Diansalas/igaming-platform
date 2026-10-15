# A3 casino pre-adapter - security re-review of the fix round (2026-10-15)

Reviewer: `security` specialist. Branch `a3-casino-preadapter`, fix commit `a5262b9` (diff
`79c59eb..a5262b9`). Inputs: `a3-security-review.md` (SR3-1..SR3-7) and `a3-ledger-finance-review.md`
(LF-A3-1..5). The live probes ran against a PRIVATE database (`sr3b_20184`), rebuilt from this worktree's
migrations and dropped at the end. I changed no DB role, password or shared state, and no production
code. The temporary probe test files were deleted and are not committed.

**Verdict: APPROVE WITH CONDITIONS.**

- CAS-PLAYER-REF-1 can now be recorded CLOSED. SR3-1 and SR3-2 (with the LF-A3-3 correction) close
  their repros live.
- SR3-3 closes the 1970/empty-statement repro. Its monotonicity anchor can be forged by the runtime
  role (SR3B-3). That has to be fixed before the first real casino statement source, together with the
  existing SR3-3 condition.
- SR3-4 and SR3-6 are only partly delivered:
  - The SR3-4 provider predicate is a string the caller supplies. It is not bound to the mock value
    (SR3B-1).
  - The SR3-6 receiver-name exemption is broader than stated. It also weakens the closure scan that
    existed before (SR3B-2).
- Both are Low and cheap to fix. Fix them before the record labels SR3-4 and SR3-6 IMPLEMENTED, or
  relabel them PARTIALLY IMPLEMENTED.
- Nothing here blocks launch on its own. All of it is MOCK or PROVIDER DEPENDENT.

## Per-item verification

### SR3-1 - CLOSED (live)

- **First-delivery bet.** Player A's consumed session, with player B's valid reference:
  - Returns `ErrPlayerRefMismatch`. The error text is byte-identical to `ErrPlayerRefUnresolved`
    (`casino: invalid input: player_ref does not resolve for this tenant and provider`).
  - Nothing is posted, and A's and B's balances are unchanged.
  - One `casino_callback.player_ref_mismatch` row is written.
- **Win.** A foreign reference is refused against the round's bettor, after the round lock and before
  any write.
- **Bet and win replays.** A foreign reference is refused as a "player" payload mismatch.
- **Response class.** Over HTTP, both the mismatch and the unresolvable case reach the
  `ErrInvalidInput` branch: 400 with the same generic body (`callback rejected: invalid input`). No
  branch above it matches the mismatch sentinel.
- **Audit row content** (printed live): `target_id = provider:provider_tx_id` and `outcome = denied`.
  The metadata holds `provider_id`, `event_type`, `provider_tx_id`, `round_id`, `asset_code`,
  `amount`, `reason` and `request_id`. It contains no player account id, no player reference (the
  owner's or the named player's) and no session id. No PII.
- **Oracle.** See SR3B-4. Some residual differences remain, but only the holder of the vendor's
  signing credential can reach them.

### SR3-2 with LF-A3-3 - CLOSED (live)

- **Posted original, foreign valid reference:** `ErrPlayerRefMismatch`.
- **Posted original, unresolvable reference:** `ErrPlayerRefUnresolved`.
- In both cases the ledger-transaction count plus the audit-row count is unchanged across the call.
  Nothing is written in-transaction. The mismatch audit row comes only from the separately committed
  rejection writer.
- **Never-seen original, or one already tombstoned:**
  - The tombstone is still written.
  - An unresolvable reference adds an audit-only `player_ref_mismatch` row with
    `sub_reason = player_ref_unresolved_on_tombstone` in the same committing transaction.
  - A missing reference stays allowed.
- **Tombstone-planting attack (assessed live).** A signed rollback that names a never-seen original
  plants a tombstone. The later real bet under that reference is then declined, and the balance is
  untouched. This works the same with a foreign valid reference, a garbage reference or no reference
  at all. So `player_ref` adds no capability. The ability already exists by design (CLAUDE.md
  rollback rule) and requires the vendor's signing credential.
  - Impact: denial of specific rounds within that vendor's own reference namespace. The tombstone is
    keyed by tenant and provider, so there is no cross-provider effect. No money moves.
  - The new evidence row only records the unresolvable-reference variant. A foreign valid reference on
    an unseen original has no owner to compare against, so it leaves no evidence. That is acceptable.
  - Not a finding beyond what is already accepted.

### SR3-3 - CLOSED for the repro; anchor forgeable (SR3B-3)

- Refused as expected:
  - Zero coverage.
  - Coverage before `periodEnd - 1h`, including 1970 with an empty statement.
  - Coverage after `now + 5min`.
  - Coverage that regresses against the previous run.
- The ledger side of the comparison is cumulative up to the cutoff, not windowed per period. So the
  1h lag allowance creates no permanent gap: the next run covers it.
- Only a `SyntheticSource` skips validation. Synthetic-marked components are refused in production by
  `RefuseSyntheticInProduction`. I verified this by reading the code, not live.

### SR3-4 - PARTIAL (SR3B-1)

- The `provider_id` predicate and the nil and empty checks work as tested.
- The predicate's value is the caller's `mockProviderID` string, not the mock's own id.
- The static caller test passes and has a planted control. It only matches call expressions (SR3B-1).

### SR3-6 - PARTIAL (SR3B-2)

- Named functions and methods that take a `pgx.Tx` are now scanned. The planted controls pass.
- The receiver exemption hides real adapter calls.

### LF-A3-2 - CLOSED (live)

- `TestA3_HTTP_SimulationConsumeCommitsBeforeTheDeliveryLockIsTaken` passes.
- **Simulation first, then vendor bootstrap:** the bootstrap is refused (401, uniform refusal). The
  session stays `consumed` with 0 bootstrap rows.
- **Race** (6 iterations, 8 concurrent simulation wagers against 1 real bootstrap through the HTTP
  route):
  - 5 times the bootstrap won (200; every simulation got 400 or 409; 0 bets).
  - Once the simulations won (8 x 200, 8 bets; bootstrap 401).
  - Never both.
- **Bootstrap first, then simulation:** 400 (existing pin green).
- **Consumed but never played:** an unfunded wager returns 200 (declined), the session is `consumed`
  with 0 bets, and a later bootstrap returns 401. See SR3B-6.
- **Replay of a simulation** (same idempotency key): the consume step is skipped (already consumed) and
  the callback replays under L0.1. The concurrent same-key loser takes the benign CAS-loss branch.

## Findings

### SR3B-1 (Low) - the SR3-4 "mock-only by construction" predicate is a caller-supplied string

**Problem.** `ConsumeLaunchSessionForMockPlay(ctx, tx, id, mock, mockProviderID)` requires a non-nil
`*MockCasinoProvider`. But the SQL predicate is `provider_id = mockProviderID`, and nothing compares
that string to the id the mock value was built with.

**Live repro.**
1. Mint an active session for provider `real-vendor-x`.
2. Call `ConsumeLaunchSessionForMockPlay(ctx, tx, session, NewMockCasinoProvider("mock-casino"), "real-vendor-x")`.
3. Result: `err=nil` and `status=consumed`.

So the function can consume a non-mock provider's session. The only real caller is safe, because
`requireMockCasinoProvider(deps, providerID)` proves the provider registered under that id is the mock.
The claim "a real adapter cannot be passed / even a misrouted call can never consume a session that
belongs to any other provider" in the doc comment and the record is not true of the function itself.

The static caller test also matches only call expressions. A function-value reference
(`f := casino.ConsumeLaunchSessionForMockPlay`) would evade it.

**Fix.**
- Take the predicate from the mock itself: add an unexported or exported `ProviderID()` accessor and
  drop the string parameter, or refuse when they differ.
- Make the static test flag any reference to the identifier, not only calls.

### SR3B-2 (Low) - the SR3-6 receiver exemption is name-only, method-agnostic, and also applies inside closures

**Problem.** `ioc1TxLocalReceivers = {tx, sp, locked}` returns early for ANY method called on a
receiver with one of those names. It is not limited to `Rollback`, `Commit` or `Balance`. It also runs
in the closure scan, which had no exemption before this commit, so the guard is now weaker than it was
before the fix round.

**Live repro (planted source).** Inside a `func(ctx, tx pgx.Tx)` closure and a named tx function, these
calls were all accepted (`violations=[]`):
- `sp.FetchCatalogue`
- `locked.Launch`
- `locked.Bet`
- `sp.Withdraw`
- `sp.Balance`

`sp` is a natural abbreviation for "sportsbook provider". No production code uses these names for an
adapter today (grep).

**Fix.** Exempt exact (receiver, method) pairs only: `tx.Rollback`, `tx.Commit`, `sp.Rollback`,
`sp.Commit` and `locked.Balance`. Preferably apply the exemption only in the named-function walk, so
the closure scan keeps its earlier strictness. Add the planted repro above as a negative control.

### SR3B-3 (Low; must be fixed with SR3-3 before the first real casino statement source) - the coverage monotonicity anchor can be forged by the runtime role

**Problem.** `validateCasinoCoverage` takes the anchor from the newest `audit_log` row (`ORDER BY
created_at DESC`) with `action = 'reconciliation.sweep_run'` and `metadata.stream = 'casino_statement'`.
The runtime role can INSERT `audit_log` rows for its tenant, including an explicit `created_at`. RLS
scopes the row only to the tenant, and the append-only trigger blocks only UPDATE and DELETE.

**Live repro (runtime role, tenant GUC).**
1. A forged row with `created_at = now+100y` and `coverage_end = 2999-01-01`. Every later legitimate
   statement is refused ("earlier than the previous run's coverage 2999-..."). This permanently
   fail-closes the tenant's casino stream (DoS).
2. A second forged row with a later `created_at` and `coverage_end = 1970`. The anchor is lowered, and a
   regressed statement (`now - 30min`) is accepted.
3. A forged non-timestamp `coverage_end`. Every run errors with SQLSTATE 22007 (DoS).

**Bound.** The absolute floor (`periodEnd - 1h`) still refused 1970. Forging only removes
monotonicity inside the 1h window, or denies service. The prerequisite is runtime-role DB write, which
is already a severe compromise. So: Low.

Related (Info): under `WithTenantSnapshot` the REPEATABLE READ snapshot is taken by the GUC
`set_config` before the stream's try-lock. The anchor read can therefore miss a run that committed just
before this run got the lock. The impact is bounded by the same floor.

**Fix.**
- Persist `coverage_end` on the `reconciliation_runs` row the same transaction writes, and read the
  anchor from the latest `casino_statement` run for the tenant. Or at least join `target_id` to an
  existing run of that stream and filter `created_at <= now()`.
- Cast defensively: ignore or alert on a malformed value instead of erroring the stream.

### SR3B-4 (Info) - residual oracle differences between "foreign valid reference" and "unresolvable reference"

The status, body and error text are identical on first delivery (verified). Two differences remain:
- **Timing.** A mismatch is a recorded rejection. It does a session load and one separately committed
  audit insert before the response, so it costs one extra commit.
- **Replay class.** On a replay, a foreign valid reference returns 409 (payload mismatch, plus an
  integrity alert). An unresolvable one returns 400.

Both are reachable only with the vendor's signing credential. That holder already knows every reference
it was issued, so it learns nothing new. The player-facing simulation route builds the payload from the
session owner's reference and cannot choose one. Accept and record.

### SR3B-5 (Info) - the mismatch audit row cannot say which player was named

The row correctly carries no PII. Forensics can tie it to the posted transaction and session through
`provider_tx_id` and `round_id`. It cannot identify the other player the vendor named. If staff need
that, store the presented opaque reference (pseudonymous, already shared with the vendor), not the
account id. Optional.

### SR3B-6 (Info) - a simulation call now consumes the session even when its callback fails

Before LF-A3-2, a callback error rolled the consume back. Now the consume commits first. So any
simulation action on an active session consumes it, including one whose callback is refused (for
example a settle with no bet) or declined. A later vendor bootstrap of that session is then refused
(401). This affects only the caller's own session, mock provider only, on non-production routes, and
moves no money. The `session_consumed` audit row records it truthfully. Accept. Mention it in the
record next to LF-A3-2.

### SR3B-7 (Info) - stale test header

The header of `internal/casino/a3_sr3_integration_test.go` still says a rollback with an unresolvable
reference "is refused and never writes a tombstone". LF-A3-3 changed this, and the test body asserts the
opposite for unseen originals. Correct the comment.

### SR3B-8 (Info) - Synthetic Fetch-shaped sources skip CoverageEnd validation but keep the cutoff

A `SyntheticSource` that implements `Fetch` may return any `CoverageEnd`. The cutoff is applied
unvalidated. This is in-house and refused in production. Consider validating any non-zero
`CoverageEnd` regardless of the synthetic marker.

## Verification run (private DB `sr3b_20184`, this worktree at `a5262b9`, `-p 1`)

- `go test -tags integration -count=1 -p 1 ./internal/casino/ ./internal/txscope/ ./internal/reconciliation/`:
  all `ok` (casino 450s, txscope 0.7s, reconciliation 249s).
- `go test -tags integration -count=1 -p 1 -run 'Casino|A3|Simulation|Bootstrap|IWire' ./internal/httpserver/`:
  `ok` (146s).
- Probes (temporary, deleted):
  - `TestSR3B_BetFirstDelivery_ForeignRef`
  - `TestSR3B_TombstonePlant_ByRefVariant`
  - `TestSR3B_RollbackPosted_NothingWritten`
  - `TestSR3B_MockConsume_PredicateNotBoundToMock`
  - `TestSR3B_ExemptReceiverNamesHideProviderIO` (fails by design; it is the SR3B-2 repro)
  - `TestSR3B_RuntimeRoleForgesCoverageAnchor`
  - `TestSR3B_SimThenBootstrap`
  - `TestSR3B_RaceBootstrapVsSim` (6 iterations)
  - `TestSR3B_ConsumedNeverPlayed`
- The first full attempt failed to build because a concurrent process evicted the shared Go build
  cache. It was not a code failure, and the rerun was green.

## Not in scope of this re-review

- SR3-5 and SR3-7 (record-only, unchanged).
- Any real vendor adapter or real statement source.
- A full httpserver suite run (only the casino, simulation and bootstrap subset).
- Timing measurement beyond classifying the code paths.
- Penetration testing.

This review covers `79c59eb..a5262b9` only. It does not certify the casino subsystem as secure.
