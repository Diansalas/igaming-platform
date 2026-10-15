# A3 record - casino pre-adapter security/correctness fixes (2026-10-15)

Branch `a3-casino-preadapter` (base `73b2836`, hosted-CI-green A1 tip). Author: Casino Integration
specialist. **Everything here is MOCK**: no real provider adapter was written or wired. This file is
the proposed registry/progress text for the orchestrator (single writer of `task-registry.md`,
`HANDOVER`, `progress`, `active-stage`, human-decision register); none of those was edited.

Scope: CAS-GAME-KILL-BET-1, CAS-WIN-IDEMP-1, CAS-BET-REQUIRES-BOOTSTRAP-1, CAS-STMT-IO-1,
CAS-PLAYER-REF-1. CAS-RECON-SCALE-1 was named in the brief but is a design/ADR item (see below).

## 0. Summary and labels

| Row | Label | One line |
|---|---|---|
| CAS-GAME-KILL-BET-1 | IMPLEMENTED (MOCK) | `postBet` re-checks `casino_games.status` and tenant/brand availability exactly as launch does; a refused new bet is a decline `game_unavailable`; win/rollback/replay never blocked |
| CAS-WIN-IDEMP-1 | IMPLEMENTED | `postWin` returns the original result for a redelivered, already-posted win (also after a rollback of its bet or of the win, and on the locked branches), verified against payout/asset/round; DB uniqueness unchanged |
| CAS-BET-REQUIRES-BOOTSTRAP-1 | IMPLEMENTED (MOCK) | a new bet requires a `consumed` session; the B2C play-simulation routes now play the MOCK vendor's bootstrap part |
| CAS-STMT-IO-1 | PARTIALLY IMPLEMENTED | provider statement sources are fetched outside the reconciliation tx and only compared inside it, guarded statically and at runtime; storing the fetched statement append-only is NOT IMPLEMENTED (needs a real source + migration); a real source stays PROVIDER DEPENDENT |
| CAS-PLAYER-REF-1 | IMPLEMENTED (MOCK) | vendor gets only the opaque per-(tenant, provider) reference at launch, echoes it on callbacks, resolved tenant+provider scoped; a raw account id does not resolve. **No migration** (see 6) |
| CAS-RECON-SCALE-1 | NOT IMPLEMENTED (unchanged) | still a design + ADR item; this change adds no per-tenant cost beyond one statement fetch per tick |

A real vendor remains PROVIDER DEPENDENT for all of the above; none of this is a licensing or
approval claim.

## 1. CAS-GAME-KILL-BET-1

Which statuses block a NEW bet (all other paths unaffected), pinned by `TestA3_GameKill_*`:

| Condition | Effect on a new bet |
|---|---|
| `casino_games.status != 'active'` (today `disabled`; any value not known yet also blocks) | decline `game_unavailable`, audit `casino_bet.denied_game_unavailable` (`reason=game_disabled`) |
| no `casino_game_availability` row for the session's brand (tenant-wide or brand) | same decline (`reason=game_not_available`), fail closed |
| most-specific row (brand row replaces tenant-wide row) has `enabled=false` | same decline |
| game row missing | Go error (nothing posts), never an allow |

Placement (`internal/casino/orchestrator.go`, `gameBlockReasonForNewBet`): after the replay (E2) and
tombstone (E3) short-circuits, the tenant gate, the session/brand checks and the capability gate
(a disabled provider capability is still the retryable 503 whatever the game's state), before the
L0.2 player lock. So replaying an already-posted bet, any win, any rollback and the stake return of a
non-active tenant are never blocked. Plain reads, no lock: a kill committed concurrently with an
in-flight bet is ordered by READ COMMITTED like the capability gate (residual R-3).

## 2. CAS-WIN-IDEMP-1

`postWin` takes L0.1 (unchanged first statement), resolves the player reference, then looks for a
`casino_win` already posted under `(tenant, provider, provider_tx_id)`. Found: it verifies
`verifyPostedWinMatchesEvent` (payout = sum of `house_gaming` debits, every leg's asset, correlation
= the round) and returns the original ledger transaction with `Replayed=true`; any difference is
`ErrProviderTxPayloadMismatch` (409). Not found: unchanged, including a FIRST delivery of a win whose
bet was already rolled back (still `ErrBetNotFound` / `ErrLockAlreadyReleased`, never pays).

Intended outcome for "win redelivered after a rollback of its bet" (ledger-finance semantics, CLAUDE.md
append-only/compensating rules): the win is an already-recorded fact; the rollback of the BET is a
separate compensating entry that does not rewrite it; the redelivery gets the original result, writes
nothing (no ledger row, no entry, no audit row, no balance move) and is neither 400 nor 409. Same
after a rollback of the win itself. A rollback for a never-seen transaction still writes a tombstone
(unchanged); a tombstoned reference still rejects the late original (E10, unchanged, and a
`casino_win` can never coexist with a tombstone under the same unique key).

Idempotency stays DB-enforced: the unique `(tenant_id, provider_id, provider_tx_id)` index and
`ledger.Post`'s key are the authority and the backstop; the new read only chooses the answer. It runs
under L0.1, so it cannot race a concurrent first delivery (`TestA3_WinRedelivery_Concurrent_PostsOnce`,
12 parallel deliveries: exactly one posting, 11 replays).

Side defect closed: before this change even a plain sequential redelivery of a LOCKED-cash or
LOCKED-bonus win came back `ErrLockAlreadyReleased` (409) because the redelivery re-entered
`resolveWinOrigin` after the first win had released the lock
(`TestA3_LockedCashWinRedelivery_*`, `TestA3_LockedBonusWinRedelivery_*` incl. terminal-grant
hold-capture).

## 3. CAS-BET-REQUIRES-BOOTSTRAP-1

`postBet` accepts only `session.Status == 'consumed'` (an allow-list of one). `active` (never
bootstrapped), `expired`, `revoked`, unknown: `ErrLaunchSessionRequired` (400). The old time-based
branch for `active` sessions became unreachable and was removed. Win/rollback never consult the
session (a bet placed before the session ended still settles).

Interaction with the B2C play-simulation routes (decision made here, within the A3 scope; ADR 0048
and `docs/integrations/casino-launch-bootstrap.md` updated): those routes are the MOCK vendor, so
they stand in for its bootstrap. `requireSimulationDrivableSession` accepts an `active` unexpired
session (consumed by one CAS `ConsumeLaunchSessionForMockPlay`, audited
`casino_play_simulation.session_consumed`) and a `consumed` session ONLY if no
`casino_launch_bootstraps` row exists (i.e. the simulation consumed it). A session a real vendor
bootstrapped is still refused to the simulation (security P2-2 invariant;
`TestCasinoBootstrap_ConsumedSessionRefusedBySimulationWagerRoute` unchanged and green).
Security should confirm this reading of the P2-2 ruling (see 8).

## 4. CAS-STMT-IO-1

`statement.CasinoStatementSource` is now a label-only base with exactly two accepted shapes:
`CasinoStatementFetcher.Fetch(ctx, req)` (NO transaction; the only shape a real source may have) and
`CasinoLedgerRenderer.RenderFromLedger(ctx, tx, tenant)` (the in-house MOCK, accepted only from a
component that is `SyntheticSource`, the structural twin of `providerkind.Synthetic`). The stream
(`FetchCasinoStatement` then `RunCasinoStatement`) obtains the statement BEFORE opening the
REPEATABLE READ transaction; `FetchCasinoStatement` refuses under `txscope.Held`; the match takes the
already-fetched value (plus an optional `CoverageEnd`: ledger transactions created after it are not
compared, so a posting between fetch and match is not a false P1). `requireSnapshotIsolation` is
unchanged. A fetch failure is a failed, audited run (`reconciliation.sweep_run_failed`, row-14 alert
path) with no partial rows.

Why the MOCK renders in-tx rather than being fetched like a provider: it reads the platform's own
ledger, not a provider, and rendering at the SAME snapshot as the ledger side is what keeps it
tautological and race-free (a first attempt that fetched the MOCK separately failed
`TestCasinoStatement_LiveTrafficProducesNoFalseMismatch`: a posting transaction that STARTED before
the coverage instant and committed after the MOCK's snapshot appears on the ledger side only,
because `created_at` is the transaction start time).

Static guard (`internal/txscope`): `Fetch` and `FetchCasinoStatement` are now flagged names and
`internal/reconciliation` is now scanned, so a statement fetch lexically inside a `pgx.Tx` closure
fails the build-time test (planted-violation control added).

NOT implemented (residual R-5): storing the fetched statement append-only before matching (the
payments-style import table) - needs a real source's format and a migration; the registry fix text
"fetch outside the tx, store, then match" is therefore split.

## 5. CAS-PLAYER-REF-1

`LaunchRequest`, `BalanceRequest` and `CallbackEvent` carry `PlayerRef` (opaque UUID); the field
`PlayerAccountID` no longer exists on any vendor-facing type (`TestProviderFacingTypesCarryNoRawPlayerAccountID`).
`CreateLaunchSession` issues the reference in the session's own transaction
(`GetOrCreateProviderPlayerRef`: `INSERT .. ON CONFLICT DO NOTHING` then SELECT; never an UPDATE), so
a session never exists without one, bootstrap returns the same value, and the vendor is handed it at
launch. The mock wire field is `player_ref`; a body naming `player_account_id` is refused as
malformed. `postBet`/`postWin` resolve it with `ResolveProviderPlayerRef` scoped to the route's
tenant and provider, after the L0.1 lock (still the first statement) and before any money read;
unknown / foreign-tenant / foreign-provider / raw-account-id values are all `ErrPlayerRefUnresolved`
(an `ErrInvalidInput`, HTTP 400), nothing posted. A rollback may carry a reference; on the
non-active-tenant stake-return path an unresolvable one is a mismatch.

Properties pinned (`a3_player_ref_integration_test.go`): random v4, unrelated to the account id,
stable per player+provider, distinct across tenants/providers; concurrent get-or-create converges on
one row; cross-tenant isolation (other tenant's reference does not resolve, other tenant cannot read
or insert, FORCE RLS on, both UNIQUE constraints, append-only, runtime role SELECT+INSERT only).

The reference identifies; it does not authorise: the wallet a bet debits is the session's, the wallet
a win credits is the round's own bet's. The pre-existing "a payload naming another player is credited
/ debited to the true owner" behaviour (and its two tests) is therefore UNCHANGED - a valid reference
of ANOTHER player is accepted and ignored (residual R-1; stricter refusal is proposed, not done).

## 6. Migration

**0152 was not needed and was not used.** `casino_provider_player_refs` (migration 0111, ADR 0103
section 3.5) already has exactly the requested shape: `tenant_id`, FORCE RLS in the tenant family,
`UNIQUE (tenant_id, provider_id, player_ref)`, primary key `(tenant_id, provider_id,
player_account_id)`, `player_ref DEFAULT gen_random_uuid()`, append-only triggers, runtime role
SELECT+INSERT only (also `deploy/init-app-role.sql`). A second table would duplicate it. The change
is purely in code (issue at session mint, send at launch, resolve on callbacks). 0152 is returned
unused.

## 7. Proposed registry text

- **CAS-GAME-KILL-BET-1** -> **CLOSED (IMPLEMENTED, MOCK) 2026-10-15 (A3).** `postBet` refuses new
  exposure for `casino_games.status != 'active'` and for a game with no/disabled tenant or brand
  availability (decline `game_unavailable`, audited), after the capability gate, before the player
  lock; replays, wins and rollbacks are never blocked. Tests `TestA3_GameKill_*`,
  `TestA3_HTTP_KilledGameDeclinesNewBetsButSettlesExistingRounds`; mutants M1/M2/M28/M29 killed.
  Residual R-3 (kill/in-flight-bet ordering is READ COMMITTED, no lock; same as the capability gate).
- **CAS-WIN-IDEMP-1** -> **CLOSED (IMPLEMENTED) 2026-10-15 (A3).** Redelivered posted win returns the
  original (also after bet rollback / win rollback / on locked-cash and locked-bonus branches), payload
  mismatch 409, concurrency-tested, DB key unchanged. `TestA3_WinRedeliver*`, `TestA3_Locked*`. For
  ledger-finance re-verification (ADR 0025 amendment item 9).
- **CAS-BET-REQUIRES-BOOTSTRAP-1** -> **CLOSED (IMPLEMENTED, MOCK) 2026-10-15 (A3).** Bet requires
  `consumed`. The simulation routes consume an `active` session as the MOCK vendor (new audit action,
  new CAS `ConsumeLaunchSessionForMockPlay`) and still refuse a vendor-bootstrapped session. For
  security confirmation of the P2-2 reading. CAS-PLAY-BOOTSTRAP-1's "still OPEN (functional, MOCK
  play)" clause is resolved by this.
- **CAS-STMT-IO-1** -> **PARTIALLY IMPLEMENTED (MOCK) 2026-10-15 (A3).** Fetch-outside / match-inside
  split, runtime refusal under a held tx, static guard extended to `internal/reconciliation` and the
  `Fetch`/`FetchCasinoStatement` names. Remaining (before the first real casino statement source,
  PROVIDER DEPENDENT): append-only stored copy + a defined coverage window (R-5).
- **CAS-PLAYER-REF-1** -> **CLOSED (IMPLEMENTED, MOCK) 2026-10-15 (A3).** No migration; ADR 0103 section
  14. A real vendor stays PROVIDER DEPENDENT.
- **CAS-RECON-SCALE-1** -> unchanged (NOT IMPLEMENTED, Medium, design + ADR).
- New rows to register: **CAS-PLAYER-REF-MISMATCH-1** (Low, R-1), **CAS-GAME-KILL-RACE-1** (Low, R-3),
  **CAS-STMT-STORE-1** (Low, R-5), **CAS-BET-JURISDICTION-RECHECK-1** (R-4, pre-existing SB-3: the
  game's jurisdiction blocklist is still not re-run per bet).

## 8. What reviewers should probe

**security**
1. Launch surface: nothing vendor-facing carries the account id (`LaunchRequest`, `BalanceRequest`,
   `CallbackEvent`, mock wire body, launch URL, `CallContext.IdempotencyKey = cas:<sessionID>`); the
   reference is stable per player+provider (linkable within one provider across sessions, by design).
2. Resolution: scoped by route-derived tenant/provider (never the payload); explicit `tenant_id`
   predicate is redundant with FORCE RLS (mutant M11 survives, justified); all failures indistinguishable.
3. R-1: a valid reference of ANOTHER player on a bet/win is accepted and ignored. Decide strict
   refusal (would change two existing assertions, see 9).
4. The simulation consume path is a new unsigned consume: only registered outside production, MOCK
   provider only, own session only, `active`+`real`+unexpired only, one CAS, audited, never for a
   session with a vendor bootstrap row. Confirm it respects the P2-2 intent.
5. Game gate ordering/TOCTOU (R-3); decline vs 503 choice; audit content.
6. `ResolveLaunchToken` is still test-only-callable and now also a fixture helper in tests.

**ledger-finance**
1. `verifyPostedWinMatchesEvent`: payout = sum of `house_gaming` debits (equals the win for the three
   posting shapes: direct cash, locked cash, locked bonus incl. terminal hold-capture); asset on every
   leg; round via correlation id. Anything it does not compare (player, wallet) is covered by the
   original posting being the same `(provider, tx id)`.
2. Short-circuit position: after L0.1 and the player-reference resolution, BEFORE the tombstone check,
   the tenant-status gate and `resolveWinOrigin`; it only answers a `casino_win` row. A tombstone and a
   `casino_win` cannot coexist under the key.
3. The semantic choice "redelivery after bet rollback = original result" (not a compensating entry, not
   an error). A FIRST delivery after a bet rollback is still refused.
4. No new lock, no new write path, no change to `ledger.Post`; audit written once per fact.
5. Reconciliation: the MOCK renders at the ledger snapshot (tautology preserved); `CoverageEnd` only for
   external-shaped sources; tombstones deliberately not coverage-bounded.

## 9. Changed existing assertions / expectations (none deleted or weakened)

Mechanical (argument kind only, assertions identical): ~200 `CallbackPayload(..., playerAccountID,
...)` call sites now pass the player's opaque reference; bet-posting fixtures (`mintSession`,
`mintSessionWithJurisdiction`, `mustMintCasinoLaunchSessionDirect`, `mustSeedRealCasinoLaunchSession`,
reconciliation `casWorld`) now enable the game for the tenant and consume the session; HTTP tests that
post webhook bets use `mustLaunchAndConsumeCasinoGame`; `casinoFixture` gained `playerRef`;
`seedCasinoFixture` tolerates the pre-0111 scratch schemas of the 0099/0108 round-trip tests; the 0097
scratch helper also applies migration 0111; the 0108 trigger-matrix seed uses a direct INSERT (it runs
below 0111).

Non-mechanical, each with its justification:
1. `bootstrap_l2_denial_lockorder_integration_test.go` `TestLockOrder_BootstrapGateDenialAndFirstBetOfRound_NoDeadlock`:
   the staged interleaving (a bet blocked on RG while bootstrap holds the session row) can no longer
   occur - a bet on an unconsumed session is refused at the session check, before RG or the FK share.
   Old: `betErr == nil` ("never an error"). New: the bet returns promptly (10 s bound, no lock wait)
   with `ErrLaunchSessionRequired`, bootstrap still finishes its denial, neither deadlocks, session
   revoked. Strictly the cycle is removed, not hidden.
2. `postbet_session_expiry_integration_test.go`: `TestReceiveCallback_NewBetRejectedOnExpiredStatus_LedgerBalanced`
   uses `mintActiveSession` (consumed rows are immutable except consumed->revoked, migration 0108);
   `TestReceiveCallback_WinSettlesForPreExpiryBetAfterSessionExpires` ends the consumed session with
   `RevokeLaunchSession` instead of flipping it to `expired`; same assertions.
   `TestReceiveCallback_NewBetRejectedPastExpiresAtEvenIfStillActive` is unchanged but no longer
   isolates a time check (the branch is gone); a comment says so and points at the new test.
3. `stake_return_integration_test.go`: the `amount_mismatch` / `wrong_round` / final matching cases
   pass the valid reference (they used the raw account id, which would now also mismatch on "player"
   and mask the case under test); `wrong_player` still passes a non-resolving value.
4. `TestFailureModeMatrix_I_*` and `TestReceiveCallback_WinCreditsBettorsWalletNeverPayloadPlayer`:
   the "other player" is named through that player's own (valid) reference instead of a raw id; the
   assertions (true owner debited/credited, other player untouched) are unchanged (this is why R-1 is
   lenient).
5. `TestCasinoStatement_SnapshotPreventsFalseMismatchUnderInterleaving`: unchanged semantics; the MOCK
   source implements `RenderFromLedger` (renamed from `Statement`), sources are fetched first.
6. `webhook_admission_t6_pool_ordering_integration_test.go`: the reference lookup is resolved before the
   measured pool-acquisition window (the lookup itself acquires a connection); expected counts
   unchanged. `resolution_isolation_integration_test.go`: the per-player reference is resolved at seed
   time (a lookup per callback inside the outage window tripped the INV-POOL probe under load).
7. `casino_webhook_tenant_binding_test.go` / `casino_flow_integration_test.go`: raw JSON bodies carry
   `player_ref` instead of `player_account_id` (the latter is now refused as malformed); `casino_rejection_record...`
   cross-tenant payload uses an arbitrary uuid for the (irrelevant, 401) reference.
8. `TestSimulationHandlers_SessionRevalidatedInDomainTx` keeps `mustLaunchCasinoGame` (active session)
   because it expires the session between phases.

## 10. Residuals / deviations

- R-1 Valid reference of another player accepted and ignored on bet/win (session / ledger decide the
  wallet). Strict refusal proposed (CAS-PLAYER-REF-MISMATCH-1).
- R-2 `Balance` is still part of `CasinoProvider` but has no production caller; its request type now
  carries `PlayerRef`.
- R-3 Kill vs in-flight bet: READ COMMITTED plain read, no lock (as the capability gate).
- R-4 Jurisdiction blocklist still not re-evaluated per bet (pre-existing SB-3).
- R-5 No stored/append-only copy of a fetched casino statement; no defined coverage window for a real
  source; the MOCK is Synthetic in-DB by design.
- R-6 The simulation `session_consumed` audit reuses `recordCasinoPlaySimulationAudit` (best effort,
  like the other simulation audit rows).
- Deviation D-1: migration 0152 unused (section 6).
- Deviation D-2: the worktree was created on an unrelated root commit; branch `a3-casino-preadapter`
  was cut at `73b2836` (`git reset --hard` was refused by the permission layer; `git switch -c` used).
- Deviation D-3: the full `internal/payments` and `internal/httpserver` integration packages exceed
  Go's default 10 min test timeout on this shared machine (CI shards them); they were run with an
  explicit timeout, payments restricted to the touched file's tests.

## 11. Verification (local, private database `a3_47213`, mirrors CI role grants; not hosted CI)

| Check | Result |
|---|---|
| `gofmt -l .` | clean |
| `go vet ./...` and `go vet -tags integration ./...` | PASS |
| `golangci-lint run ./...` (2.9.0) and `--build-tags integration --new-from-rev=HEAD~1` | 0 issues |
| `go test -count=1 ./...` (unit) | PASS |
| `-tags integration -p 1 ./internal/casino/...` | PASS (101.7 s) |
| same with `-race` | PASS (122.1 s) |
| `-tags integration -p 1 ./internal/reconciliation/...` | PASS (197.8 s) |
| `./internal/txscope/... ./cmd/...` (static IO-in-tx guard incl. new controls) | PASS |
| `./internal/alerting/...` | PASS (252.8 s) |
| `./internal/payments/ -run TestR3_` (the file touched) | PASS (94.8 s); the full package exceeds 60 min on the shared machine and was not completed |
| `./internal/httpserver/...` (full, 21 min) | all PASS except the 3-4 timing-lane tests (`TestResolutionIsolation_*` INV-POOL probes), which also fail on the unmodified base `73b2836` on this loaded machine (CI runs them in the isolated timing lane, `-skip` in the main suite); they pass when run alone on a quiet machine |
| Mutation pass | 30 mutants over the money/state logic: 29 killed (M19 only after adding the revoked-session win/rollback assertions), 1 survivor justified (M11: the explicit `tenant_id` predicate in the reference resolver is redundant with FORCE RLS, kept as defence in depth) |

New tests: `internal/casino/a3_preadapter_gates_integration_test.go`,
`a3_player_ref_integration_test.go`, `a3_win_idemp_locked_integration_test.go`,
`player_ref_shape_test.go` (+ two conformance sub-tests in `conformance_test.go`);
`internal/reconciliation/casino_statement_fetch_integration_test.go`;
`internal/httpserver/casino_a3_preadapter_integration_test.go`; planted-violation control in
`internal/txscope/no_provider_call_in_tx_closure_static_test.go`.
