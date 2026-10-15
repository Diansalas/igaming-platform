# A3 casino pre-adapter - independent security review (2026-10-15)

Reviewer: `security` specialist. Branch `a3-casino-preadapter`, commits `e2591aa`, `934b889`,
`2bf9791` on base `73b2836`. Live probes ran against a PRIVATE database (`sr3_30798`, rebuilt from
this worktree's migrations, dropped at the end). No DB role, password or shared state was changed.
I did not change any production code. The temporary probe test files were deleted and are not
committed.

**Verdict: APPROVE WITH CONDITIONS.** The A3 controls do what they claim for bets and wins,
CAS-STMT-IO-1's split, the game kill gate and the bootstrap requirement. However, CAS-PLAYER-REF-1
should NOT be recorded CLOSED until SR3-1 and SR3-2 are fixed. Both are small, fail-closed changes.
SR3-3 is latent today because no real statement source exists. It must block the first real casino
statement source. Nothing in this review blocks launch on its own, because everything here is MOCK
and PROVIDER DEPENDENT. The "Not in scope" section below lists what this review did not cover.

## Findings

### SR3-1 (Medium) - a valid reference of ANOTHER player is accepted and ignored on bet, win and replay (R-1). Must refuse.

Live repro (probe `TestSecProbe_OtherPlayersValidRef`, same tenant and same provider, player B
seeded via `seedSecondPlayerWallet`):

- A bet on player A's consumed session with player B's valid `player_ref` returned `succeeded`. A was
  debited.
- Redelivering the same bet with A's own reference returned `Replayed=true`. So a replay does not
  compare the reference either.
- A win for A's round that names B's reference returned `succeeded`. A was credited, and B's balance
  stayed at 0.
- `audit_log` had **0 rows** that recorded the disagreement.

Exploitability: a player cannot do this, because the simulation route builds the payload from the
session owner's reference. Only a validly-signed vendor can. Money still lands in the right wallet,
because the session or the round decides it. The real risk is detection. This is the only point
where the vendor's own view of the player can be cross-checked against the platform's binding, and
today a disagreement is silently ignored.

Concrete failure scenario: a real adapter (or a vendor-side bug or compromise) sends player B's play
on player A's `session_id`. Session ids are UUIDv4 values handed to the vendor and are the only
binding a bet checks. The platform debits A. The vendor's records attribute the play to B. Nothing
is refused, audited or alerted. The reference is a second, independent binding factor that costs
nothing to enforce, and the lenient choice throws it away.

**Required change (fail closed):**

1. `postBet`: keep the `refPlayer` returned by `ResolveProviderPlayerRef` instead of discarding it.
   Right after the session is loaded and its provider checked (before
   `requireActiveBrandForNewStake`, so before any gate or lock), add
   `if refPlayer != session.PlayerAccountID`. In that case, write an audit row
   `casino_callback.player_ref_mismatch` (ActorSystem, target the launch session; metadata
   `provider_id`, `provider_tx_id`, `round_id`, `event_type`. Internal ids are fine in the internal
   audit, but put no reference values in any vendor-facing response), record a
   `RecordCallbackRejection` (new kind `player_ref_mismatch`), and return `ErrPlayerRefUnresolved`.
   Externally this is the same 400 as unresolved, so the failure classes stay indistinguishable to
   the vendor.
2. `verifyPostedBetMatchesEvent` (bet replay): when `refPlayer` is not the posted session's
   `PlayerAccountID`, add `"player"` to `diffs` (409 `ErrProviderTxPayloadMismatch`).
3. `postWin`: compare `refPlayer` with the bettor's `player_account_id`, which comes from the round's
   bet wallet that `resolveWinOrigin` already determines. On mismatch, apply the same audit,
   rejection and error as in step 1, with nothing posted. In the CAS-WIN-IDEMP-1 short-circuit,
   compare it with the posted win's credited wallet owner and add `"player"` to the
   `verifyPostedWinMatchesEvent` diffs.
4. Tests: invert `TestFailureModeMatrix_I_BetPayloadNamingAnotherPlayerDebitsOnlyTheSessionOwner` and
   `TestReceiveCallback_WinCreditsBettorsWalletNeverPayloadPlayer`. The new assertions are: refused,
   nothing posted, neither wallet moved, one mismatch audit row. This strengthens the protected
   property ("other player untouched") rather than weakening it. Add cases for bet replay and win
   replay with a foreign reference.

Operational note: a refused win cannot settle until the vendor corrects it. That is the correct
fail-closed outcome for an identity disagreement on a payout. It should page through the existing
callback-rejection alerting, not be swallowed.

### SR3-2 (Low-Medium) - the ordinary rollback path ignores `player_ref` entirely, including a raw account id and another tenant's reference

Live repro (probe `TestSecProbe_RollbackIgnoresRef`, active tenant):

- A rollback carrying the player's **raw `player_account_id`** as `player_ref` returned `succeeded`
  and reversed the bet.
- A rollback carrying **another tenant's** reference returned `succeeded`.
- A rollback of a never-seen original with a random reference wrote a tombstone.

Only the non-active-tenant stake-return path (`verifyStakeReturnMatchesBet`) checks the reference.
This contradicts the record's claim (section 5) that "unknown / foreign-tenant / foreign-provider /
raw-account-id values are all refused", which holds only for bet and win. It is also inconsistent
between the two rollback paths. It is not a money move: the rollback targets the vendor's own
`provider_tx_id`, scoped by RLS. But it is a fail-open validation gap on a signed vendor field.

**Required change:** in `postRollback`, if `event.PlayerRef != uuid.Nil`, resolve it. An
unresolvable value returns `ErrPlayerRefUnresolved` (400, nothing written, no tombstone). A resolved
player that differs from the original transaction's player is handled as in SR3-1. For a never-seen
original, only resolvability is required. A missing reference stays allowed, because a rollback must
always be possible.

### SR3-3 (Medium, latent; blocks the first real casino statement source) - `CoverageEnd` is source-controlled, unvalidated and unaudited, and can silence reconciliation

Live repro (probe `TestSecProbe_CoverageEndInThePastSilencesReconciliation`): a non-Synthetic
`CasinoStatementFetcher` returned `CoverageEnd = 1970-01-01T00:00:01Z` with no lines, against a
ledger holding casino postings. The run came back `status=clean, mismatches=0`. The control (same
empty statement, zero `CoverageEnd`) returned `mismatches_found, 11`.

A buggy or hostile provider statement can therefore exclude the whole ledger from the comparison.
That is the opposite of what reconciliation exists to catch. The value also does not appear in
`CasinoStatementInfo.AuditMetadata()`, so an investigator would not see it.

**Required before any real source is wired** (register as part of CAS-STMT-STORE-1 or as a new row,
CAS-STMT-COVERAGE-1):

- Reject (fail the run, audited and alerted) a `CoverageEnd` before `PeriodEnd - maxLag`, after
  `now + skew`, or earlier than the previous successful run's coverage for that tenant and provider.
- Treat a zero `CoverageEnd` from a non-Synthetic source as an error.
- Record `coverage_end` in the run's audit metadata and the stored statement copy.

Not blocking A3, because only the Synthetic MOCK is wired and it uses no coverage bound.

### SR3-4 (Low) - the simulation identifies "its own" consumed session by the ABSENCE of a bootstrap row

`requireSimulationDrivableSession` accepts a `consumed` session when there is no
`casino_launch_bootstraps` row. That is safe today:

- The only production consume paths are `BootstrapLaunch`, which consumes and inserts its row in one
  transaction (verified), and `ConsumeLaunchSessionForMockPlay`. `ResolveLaunchToken` has no
  non-test caller (verified by grep).
- The routes are mock-provider-only (`requireMockCasinoProvider`) and non-production-only.

However, it is a negative signal. A future consume path that writes no bootstrap row, such as a
different vendor bootstrap protocol or a new caller of `ResolveLaunchToken`, would silently make
those sessions drivable by the unsigned player route. `ConsumeLaunchSessionForMockPlay` is also
provider-agnostic at the CAS level and relies on the handler for the mock check.

Recommendation: use a positive marker, either a `consumed_via` column (`vendor_bootstrap` or
`mock_simulation`) or an `AND provider_id = <mock>` predicate in the CAS. Also add a static test
asserting that `ConsumeLaunchSessionForMockPlay` is called only from
`internal/httpserver/casino_play_handlers.go`. Not blocking.

### SR3-5 (Low) - a bet's session-status check is a plain read; a concurrent revoke can be overtaken

`postBet` reads the session with `GetLaunchSessionByID`, which takes no row lock. The revoke is a
status UPDATE (`FOR NO KEY UPDATE`), and any FK `KEY SHARE` taken by the posting does not conflict
with it. So a bet that read `consumed` before a revoke committed can still commit after it. This is
the same class as R-3 (kill gate) and the capability gate, and it is consistent with READ COMMITTED
ordering ("exposure taken before the revoke").

If a strict "no bet after revoke commits" property is wanted, read the session `FOR SHARE`. That
needs an ADR 0082 lock-order review before L0.2. Register it with R-3, not as a blocker.

### SR3-6 (Info) - the static IO-in-tx guard only scans closure literals

`ioc1ScanFile` inspects `*ast.FuncLit` with a `pgx.Tx` parameter. A named function that takes
`tx pgx.Tx` and calls `source.Fetch` would not be flagged. Mitigations today:

- `RunCasinoStatement` now takes a `FetchedCasinoStatement` value, so the type shape prevents the
  defect.
- `FetchCasinoStatement` refuses under `txscope.Held`.

A direct `src.Fetch` call bypasses the runtime check, though. Recommendation: extend the scan to
`*ast.FuncDecl` with a `pgx.Tx` parameter, and require real `Fetch` implementations to refuse under
`txscope.Held` themselves (the interface comment already says so; add a conformance test for the
first real source).

### SR3-7 (Info, pre-existing)

- `TestSupportRoutesEnabled` uses `Environment`, not `GuardEnvironment`. With `APP_ENV` unset and
  `TEST_SUPPORT_ENDPOINTS_ENABLED=true`, the simulation routes would be registered. However, the
  synthetic startup guard (which treats a missing `APP_ENV` as production) refuses to start the
  binary, because `MockStatementSource` and the mock casino provider are always registered. So the
  route is not reachable in production. That depends on two layers, not one.
- With bets now requiring `consumed`, the absence of any idle or absolute lifetime on a consumed
  session (CAS-SESSION-EXPIRY-1 / CAS-REVOKE-CONSUMED-1) applies to every bet-eligible session. A
  vendor holding a session id can bet until it is revoked. This is already tracked; it is noted here
  because A3 makes it universal.
- The `player_account_id` refusal in `HandleCallback` matches the exact key only. Go's
  `json.Unmarshal` is case-insensitive, but no struct field maps `Player_Account_ID`, so such a key is
  ignored and never becomes a reference. Harmless; noted for the real adapter's parser.
- The decline `game_unavailable` (and RG/risk declines) persists nothing under the `provider_tx_id`.
  A redelivery after the game is re-enabled would post. This is pre-existing behaviour for every
  decline; the real adapter contract should state that a decline is final on the vendor side.

## Scope items confirmed (what passed)

1. **CAS-PLAYER-REF-1, vendor surface.**
   - `LaunchRequest`, `BalanceRequest` and `CallbackEvent` carry only `PlayerRef`.
   - The mock launch URL contains `ProviderGameID`, the launch token and the session id, but no
     account id.
   - The mock wire body uses `player_ref`. A body naming `player_account_id` is
     `ErrCallbackMalformedBody` (test green).
   - Bootstrap returns the same reference.
   - Rejection records are built from `CallbackEvent`, which no longer has the account id.
   - The reference is PostgreSQL `gen_random_uuid()` (v4, 122 random bits), and the platform never
     supplies the value.
   - Resolution is scoped by the route's tenant (explicit predicate plus FORCE RLS) and provider.
     Probe `TestSecProbe_SameTenantOtherProviderRefRefused` confirmed that a same-tenant,
     other-provider reference is `ErrPlayerRefUnresolved` for both bet and win. That case was missing
     from `TestA3_Callbacks_RawAccountIDAndForeignRefsDoNotResolve`; add it.
   - Unknown, foreign-tenant, foreign-provider and raw-id values share one sentinel and one message.
   - The webhook echo of an `ErrInvalidInput` message therefore reveals nothing that distinguishes
     these cases.
2. **P2-2 (simulation versus vendor bootstrap).**
   - Order 1, bootstrap first (probe `TestSecProbe_SimulationConsumeVsBootstrapRace`): the
     simulation CAS blocks on the row, then gets `ErrLaunchSessionNotActive`, re-reads
     `status=consumed, bootstrapped=true` and is refused.
   - Order 2, simulation first: the bootstrap CAS blocks, then gets zero rows, which is a uniform
     refusal.
   - Replay: a second simulation consume returns `ErrLaunchSessionNotActive`. A consumed session is
     never consumed twice.
   - The bootstrap row's insert guard requires `consumed`, and `BootstrapLaunch` requires `active`,
     so a simulation-consumed session can never acquire a vendor row afterwards.
   - `TestCasinoBootstrap_ConsumedSessionRefusedBySimulationWagerRoute` and
     `TestSimulationHandlers_SessionRevalidatedInDomainTx` are green.
   - A player abusing their own session can at most consume it before the mock vendor does, which
     only denies service to themselves. The simulation consume skips the bootstrap gates (game,
     capability, RG), but `postBet` re-runs all three, plus risk, on every bet.
   - Production exposure: the routes are registered only under `TestSupportRoutesEnabled`, are
     mock-only, and the synthetic guard refuses a production binary (see SR3-7).
3. **CAS-GAME-KILL-BET-1.**
   - Any `casino_games.status != 'active'` blocks; the DB CHECK allows only `active` and `disabled`.
   - With no availability row, the gate fails closed.
   - The most specific availability row wins.
   - A missing game is an error, never an allow.
   - The gate is placed after replay, tombstone, tenant, session, brand and the capability gate, and
     before the L0.2 lock.
   - `gameBlockReasonForNewBet` has exactly one caller (`postBet`). Win, rollback and stake-return
     never reach it (grep plus tests).
   - The audit `casino_bet.denied_game_unavailable` carries provider id, provider tx id, round id,
     game id and reason, with no PII or secrets.
4. **CAS-BET-REQUIRES-BOOTSTRAP-1.** `postBet` uses an allow-list of exactly `consumed`; `active`,
   `expired`, `revoked` and unknown statuses are refused (tests green). See SR3-5 for the race.
5. **CAS-STMT-IO-1.**
   - The fetch runs before `WithTenantSnapshot`, refuses under `txscope.Held`, and a failed fetch is
     a failed, audited run.
   - Only `Fetcher` or a Synthetic `Renderer` is accepted.
   - The production guard refuses any Synthetic component and requires `ProductionEligible` on every
     registered component, including `casino/statement_source`. The only scheduler source
     (`providers.CasinoStmt`) is the registered one.
   - So the "Synthetic" shape cannot carry a real source into production: a real source that adds
     `SyntheticComponent()` is refused at startup.
   - The static guard tests (`TestINV_IO_1c_*`, including the new planted control) are green.
   - See SR3-3 and SR3-6.

## Verification run (private DB, this worktree, -p 1)

- `./internal/casino -run 'TestA3_|TestFailureModeMatrix_I_|TestReceiveCallback_WinCreditsBettorsWalletNeverPayloadPlayer|TestStakeReturn'`: PASS.
- `./internal/httpserver -run 'TestA3_HTTP_|TestCasinoBootstrap_ConsumedSessionRefusedBySimulation|TestSimulationHandlers_'`:
  PASS (6 tests).
- `./internal/txscope` (`TestINV_IO_1c_*`, 4 tests): PASS.
- `./internal/reconciliation -run TestCasinoStatement`: PASS.
- Probes (temporary, deleted): `TestSecProbe_SameTenantOtherProviderRefRefused`,
  `TestSecProbe_RollbackIgnoresRef`, `TestSecProbe_OtherPlayersValidRef`,
  `TestSecProbe_SimulationConsumeVsBootstrapRace`,
  `TestSecProbe_CoverageEndInThePastSilencesReconciliation` (with an empty-statement control). Their
  results are quoted above.

## Not in scope of this review

- CAS-WIN-IDEMP-1's financial semantics (ledger-finance owns them). I only checked that the
  short-circuit runs after L0.1 and after reference resolution.
- The full httpserver and payments suites, and hosted CI.
- A real vendor adapter, a real statement format or a real bootstrap protocol: all PROVIDER
  DEPENDENT.
- Penetration testing.

This review covers the code at `2bf9791` only. It is not a statement that the casino integration is
secure for production.
