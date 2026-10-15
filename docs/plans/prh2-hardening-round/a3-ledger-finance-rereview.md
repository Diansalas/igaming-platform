# A3 fix round: ledger-finance re-review (commit a5262b9)

- **Reviewer:** `ledger-finance` specialist. Scope: the diff `79c59eb..a5262b9` on branch
  `a3-casino-preadapter`, against my earlier review `a3-ledger-finance-review.md` (LF-A3-1..5) and the
  ledger-relevant parts of the security fixes (SR3-1, SR3-2, SR3-3).
- **Method:** a clean `git archive a5262b9` export (the worktree was also confirmed clean at a5262b9),
  and a private, throwaway database that was dropped afterwards. No role, password or shared
  infrastructure was touched. Probes and mutants ran only in the export. Nothing was committed except
  this file. No code was changed. All evidence is MOCK/local only. There is no hosted CI.
- **Verdict: APPROVE. LF-A3-1 to LF-A3-4 are closed. No blocking finding.** There are 3 Low or Info
  notes (LF-RR-1..3). None of them needs a code change before merge.

## 1. Condition-by-condition

### LF-A3-3: the tombstone is never refused (CLOSED)

`postRollback` now resolves a present `player_ref` lazily, at three points:

- **Never-seen original** (`ErrNoRows`). The tombstone is written unconditionally. An unresolvable
  reference adds an audit-only `casino_callback.player_ref_mismatch` row (sub-reason
  `player_ref_unresolved_on_tombstone`) in the same committing transaction.
- **Already-tombstoned original.** Same as above. The idempotent tombstone result is unchanged.
- **Posted bet/win.** The reference is resolved after the non-active-tenant gate. An unresolvable
  reference gives `ErrPlayerRefUnresolved`. A reference of another player gives `ErrPlayerRefMismatch`
  (owner comparison against the original's posted entries). Both refusals happen before any write.

A missing reference is still allowed. This is exactly the shape I required.

Verified by probe `TestLFRR_BadRefTombstone_BlocksLateOriginals` (export only, PASS):

- A rollback with a random reference of a never-seen win tombstones it.
- The late win, delivered twice, then gets `ErrOriginalTombstoned` both times, with the balance
  unchanged.
- A rollback that uses the raw account id as reference tombstones a never-seen bet. The late bet is then
  declined: no posting, the tombstone is still the only row, and the balance is unchanged.
- Redeliveries on the tombstoned original with a bad, a foreign and no reference all return
  `Tombstoned + Replayed`. Exactly one row exists under the original's reference.
- `SUM(debits) = SUM(credits)`.

The committed `TestA3_Rollback_PlayerRefIsResolvedAndMatched` covers the same ground for a late bet.

Mutant M3 makes the tombstone path refuse on a bad reference (the earlier SR3-2 proposal). It is
killed by `TestA3_Rollback_PlayerRefIsResolvedAndMatched`.

### SR3-1, win side: the owner comes from the ledger, never from the payload (CONFIRMED)

- **First delivery.** `postWin` compares `refPlayer` with `wallet.GetByID(origin.WalletID).PlayerAccountID`.
  `origin` is `resolveWinOrigin` over the round's posted bet entries, read under the round-bet
  `FOR UPDATE`. The payload has no player field apart from the reference itself.
- **Replay.** `verifyPostedWinMatchesEvent` calls `postedTxOwnedBy`. That function reads the distinct
  `wallets.player_account_id` behind every wallet-bearing leg of the posted transaction. House legs have
  `wallet_id IS NULL` (migration 0020 CHECK) and drop out of the inner join. It requires at least one
  owner and every owner equal to `refPlayer`.

Probe `TestLFRR_WinShapes_PlayerBinding` exercises all four posting shapes. Each one PASSES:

| Shape | Covered |
|---|---|
| Direct cash | yes |
| Locked cash (lock-release legs) | yes |
| Locked bonus, non-terminal grant | yes |
| Locked bonus, terminal grant (hold-capture to `player_bonus_held`, plus the Rule B2 mirror legs) | yes |

For each shape the probe checks five things:

1. A foreign-reference first delivery gives `ErrPlayerRefMismatch`, with ledger transactions, entries,
   bonus balances and the other wallet unchanged.
2. The own-reference delivery posts.
3. `postedTxPlayers` of the posted win is exactly `[bettor]`.
4. A foreign-reference replay gives `ErrProviderTxPayloadMismatch`.
5. The own-reference replay returns the original id with no new rows. The ledger balances.

Mutants: M2 (drop the replay `player` check) is killed by
`TestA3_Replays_ForeignPlayerRefIsAPayloadMismatch`. M4 (drop the first-delivery check) is killed by
`TestA3_Win_ForeignPlayerRefIsRefusedAndAudited` and the inverted
`TestReceiveCallback_WinCreditsBettorsWalletNeverPayloadPlayer`.

The posted-original rollback path uses the same entries-derived owner. Probe
`TestLFRR_RollbackOfPostedLockedCashWin_PlayerBinding` (PASS) checks it:

- A foreign reference gives a mismatch and an unresolvable one gives unresolved. Nothing moves in
  either case.
- The own reference reverses the win once. Its redelivery replays.

Mutant M5 (owner check forced true) is killed by `TestA3_Rollback_PlayerRefIsResolvedAndMatched`.

### The two INVERTED tests are strengthenings (CONFIRMED)

The tests are `TestFailureModeMatrix_I_BetPayloadNamingAnotherPlayer...` and
`TestReceiveCallback_WinCreditsBettorsWalletNeverPayloadPlayer`. The property each one protected still
holds: the payload-named player is never debited or credited and gets no wallet. The new versions add
four assertions:

- zero `ledger_transactions` under the reference;
- the session owner or bettor is unchanged (5000, or 4000 after the earlier bet);
- zero entries against the other wallet, and the other wallet's balance is 0;
- exactly one `player_ref_mismatch` audit row.

`fmAssertCorrelationBalanced` is kept. Nothing posts or moves.

Operational consequence, not a ledger defect. A win refused for a mismatching reference leaves the
round with the stake taken and no payout, and a provider that retries keeps getting 400. That is the
owner's fail-closed decision. The refusal is durable evidence, and a real provider statement would show
the win line with no ledger posting. See LF-RR-3.

### The replay `player` diff cannot break a legitimate redelivery (CONFIRMED)

Both sides of the comparison are immutable on this branch:

- **Reference to player.** `casino_provider_player_refs` has PK `(tenant_id, provider_id,
  player_account_id)`, UNIQUE `(tenant_id, provider_id, player_ref)`, and an immutable trigger with
  UPDATE/DELETE/TRUNCATE revoked from `igaming_runtime` (migration 0111). One player has exactly one
  reference per provider, forever. There is no "relink".
- **Wallet to player.** Migration 0136 (ADR 0118, BRAND-REHOME-HOLD-1) refuses any change to
  `wallets.player_account_id` (BR002) and checks the `ledger_accounts.player_account_id` agreement. A
  brand re-home is refused outright. Even if it were not, it changes `brand_id`, not the player, so the
  owner read through the posted entries cannot change.

A genuine redelivery therefore always resolves to the same player as the original posting. A tombstone
row has no wallet legs, so it shows as a `player` diff, which is correct: a tombstone is never a win.
Zero-amount wins are impossible (`validateCallbackEvent`, `amount must be positive`), so a posted
bet/win always has at least one player leg.

### LF-A3-1 / SR3-3: CoverageEnd (CLOSED)

`validateCasinoCoverage` runs only for non-Synthetic sources. It refuses five cases:

- a zero coverage;
- a coverage earlier than `periodEnd - 1h`, which is stricter than the `periodStart` floor I asked for;
- a coverage later than `now + 5m`;
- a coverage earlier than the previous run's `coverage_end`;
- through the audit anchor, which is read after the stream's advisory lock in
  `TryRunCasinoStatementForTenant`.

The anchor is real. `runCasinoStatementStreamForTenant` merges `info.AuditMetadata()` (which carries
`coverage_end`) into the `reconciliation.sweep_run` row whose metadata has `stream=casino_statement`,
and the anchor query reads exactly that. `TestCasinoStatementCoverage_SweepRecordsCoverageAndRefusesRegression`
pins it end to end.

The refusal is a failed, audited run: there is no run row and the error path is
`reconciliation.sweep_run_failed` plus an alert. My 1970 repro is now a failure, not clean.

Real drift is still detected with a valid coverage. In probe `TestLFRR_ValidCoverageStillDetectsUnassertedWin`
(PASS), a provider-shaped (non-Synthetic) fetcher returns a captured clean statement with `CoverageEnd = now`.
It runs clean. Then a balanced `casino_win` the provider never asserted (900, bound round `r1`, house to
cash) is injected through `ledger.Post`. The next run is `mismatches_found`, with both the key finding
for that `provider_tx_id` and the GGR total finding, and `coverage_end` is present in the info.

### LF-A3-2: consume before L0.1 (CLOSED, option b)

`runCasinoPlaySimulation` now does the work in two transactions:

1. `ConsumeLaunchSessionForMockPlay` runs in its own `WithTenant` transaction, which commits.
2. A new domain transaction opens. It re-runs `validate`, which is plain SELECTs only:
   `GetLaunchSessionByID`, `LaunchSessionVendorBootstrapped`, and for a rollback
   `requireRollbackTargetOwnedByRound`. I checked them all and none uses `FOR UPDATE/SHARE` or
   `Exec`. It then calls `ReceiveVerifiedCallback`.

`postBet`, `postWin` and `postRollback` each still take L0.1 as their first domain statement. So L0.1 is
taken holding no row lock, and ADR 0082 needs no exception.

`TestA3_HTTP_SimulationConsumeCommitsBeforeTheDeliveryLockIsTaken` (PASS) holds the exact L0.1 key from
another connection. While the wager is parked, it asserts three things: the consume is already committed,
the session row is `FOR UPDATE NOWAIT`-lockable, and the wager has not finished.

A failed wager now leaves the session consumed. That matches what a real vendor bootstrap does, and it
is the trade-off I accepted. Statements that `redeemVerified` runs before L0.1 are pre-existing (ADR
0094) and not A3.

### LF-A3-4: the type-filter mutant is now killed (CLOSED)

I dropped `transaction_type = casino_win` from `findPostedWinTransaction` (M1) and ran `-run TestA3_`
only. Two tests fail:

- `TestA3_WinRedelivery_SameReferenceDifferentKindIsNotAReplay` got a payload mismatch, where
  `ErrIdempotencyKeyReused` was expected.
- `TestA3_WinAfterTombstone_IsOriginalTombstonedNotAReplay` got a payload mismatch
  `[player,amount]`, where `ErrOriginalTombstoned` was expected.

The pre-existing `TestTenantNotActive_*` test is no longer the only one that kills it.

## 2. Findings (non-blocking)

### LF-RR-1 (Low, doc) - the header comment of `a3_sr3_integration_test.go` contradicts LF-A3-3

Lines 8-10 still say "a rollback with an unresolvable reference is refused and never writes a
tombstone". The test body and the code now do the opposite on the never-seen path, which is correct.

**Fix:** reword it to "refused only when reversing a posted bet/win; a never-seen or tombstoned original
still gets its tombstone, plus an audit-only evidence row". It is a comment only, but a future reader
must not "restore" the old rule from it.

### LF-RR-2 (Info) - tombstone-path evidence is written once per delivery

Each redelivery of a bad-reference rollback on a tombstone writes one more
`player_ref_mismatch` audit row. The committed test asserts `n == i+1`. This is intended
("fail toward evidence") and has no financial effect. It is the same accepted pattern as the
`rollback_of_tombstoned_original` record. If audit volume ever matters, dedupe on
`(provider_tx_id, sub_reason)`.

### LF-RR-3 (Info) - the coverage anchor and the refused win, for awareness

There are three items.

**(a) Snapshot timing of the coverage anchor.** The anchor is read in a REPEATABLE READ transaction
whose snapshot is taken at its first statement, before the try-lock. A competing run that commits
between this run's snapshot and its lock acquisition is not visible to the anchor read. The window is
tiny. The consequence is only a less-recent comparison, never a blind spot: the ledger view is
cumulative with no lower bound, so whatever an earlier coverage excludes is compared on the next run.
This is defence in depth, so no action is needed.

**(b) Clock source for the skew ceiling.** The ceiling uses the application clock (`time.Now()`),
while the ledger cutoff compares DB `created_at`. This is the same as payments, and acceptable while
the skew is 5 minutes.

**(c) A refused mismatching win leaves a round open.** The stake is taken and the win is never
credited. The only ledger-side detector is a real provider statement, so this depends on R-7/R-8
(PROVIDER DEPENDENT). Record that in the first real adapter's ADR, together with a staff resolution
path for `player_ref_mismatch` on a win. That path is future work, not A3.

## 3. Evidence (local, private DB `lfrr_*`, dropped; export of a5262b9)

| Run | Result |
|---|---|
| `-tags integration -count=1 -p 1 ./internal/casino/` | PASS (135 s) |
| same with `-race` | PASS (158 s) |
| `./internal/reconciliation/` | PASS (248 s) |
| `./internal/ledger/` | PASS (55 s) |
| `./internal/httpserver/ -run TestA3_` (incl. the LF-A3-2 lock-order test) | PASS (5/5) |
| Probes (export only, not committed): `TestLFRR_WinShapes_PlayerBinding` (4 shapes), `TestLFRR_BadRefTombstone_BlocksLateOriginals`, `TestLFRR_RollbackOfPostedLockedCashWin_PlayerBinding`, `TestLFRR_ValidCoverageStillDetectsUnassertedWin` | PASS |
| Mutants vs committed tests: M1 type filter (LF-A3-4), M2 replay player check, M3 refuse-on-tombstone, M4 first-delivery win check, M5 rollback owner check | all 5 KILLED |

`SUM(debits) = SUM(credits)` was asserted in every probe. No balance was updated directly. No float was
introduced. DB uniqueness on `(tenant_id, provider_id, provider_tx_id)` is unchanged and remains the
idempotency authority.
