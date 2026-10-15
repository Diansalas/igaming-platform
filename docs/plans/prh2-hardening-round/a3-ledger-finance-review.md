# A3 casino pre-adapter: independent ledger-finance review (2026-10-15)

Reviewed: branch `a3-casino-preadapter`, commits `e2591aa`, `934b889`, `2bf9791` on base `73b2836`.
Probe items: CAS-WIN-IDEMP-1 (main), CAS-GAME-KILL-BET-1, CAS-STMT-IO-1. Reviewer: ledger-finance
specialist. Author's record (`docs/plans/a3-casino-record.md`) was read, and every claim below was
checked against the code and with my own probes and mutants.

Evidence base. All test evidence comes from a clean `git archive 2bf9791` export run against private
databases (`lf3_*`, built by `priv_db.sh`; roles and grants mirror CI). The shared worktree was being
edited by another agent during the review (uncommitted changes to `orchestrator.go`,
`player_ref.go`, `rejections.go` and two test files, plus the security review commit `75e19de`), so
worktree runs are not used as evidence. This review covers `2bf9791` only, not those uncommitted
edits.

## Verdict

**APPROVE (ledger-finance sign-off) for CAS-WIN-IDEMP-1 and CAS-GAME-KILL-BET-1. APPROVE WITH
CONDITIONS for CAS-STMT-IO-1.** None of the findings is a money-correctness defect. No path posts
twice, posts after a tombstone, or reports success for a win that did not post. No balance is
updated directly. `SUM(debits) = SUM(credits)` holds in every probe. The DB uniqueness on
`(tenant_id, provider_id, provider_tx_id)` is unchanged and still the authority. Conditions: LF-A3-1
must be done before the first real casino statement source. LF-A3-2 is a record/ADR correction.
LF-A3-3 is a sign-off condition on the security review's SR3-2 fix, which is not yet written.

## Findings

### LF-A3-1 (Medium, latent: blocks the first real casino statement source) - `CoverageEnd` can silently empty the comparison

Concurs with security SR3-3. Reproduced independently with a different shape.

`RunCasinoStatement` uses any non-zero `CoverageEnd` as the ledger cutoff (`t.created_at <= $3`, in
both the key view and the totals). The value is never validated or audited.
`CasinoStatementInfo.AuditMetadata` does not record it. Repro (`TestLF3_ImplausibleCoverageEndSuppressesComparison`,
scratch): a clean world of 13 casino postings, a provider-shaped source returning an EMPTY statement
with `CoverageEnd = 1970-01-01`. The result is **0 findings and a `clean` run**. The same empty statement
with no `CoverageEnd` gives 11 findings. A buggy or compromised source, or a clock or timezone error,
can make the casino statement stream compare nothing and still report clean. That is a reconciliation
blind spot of exactly the kind that hides a P1 drift.

The payments stream already solved this (code review F5; `payment_statement.go`, lines 415-420 and
653). It requires a non-empty `[CoverageStart, CoverageEnd)` and rejects `CoverageEnd > fetch time +
PaymentCoverageMaxClockSkew`. It also records the coverage in the run audit.

Fix (before a real source; not needed for the MOCK, which never sets it):

- Validate `CoverageEnd` in `FetchCasinoStatement` for a fetcher. It must not be zero (a real fetcher
  MUST supply it). It must not be before `periodStart`, and it must not be after the fetch time plus
  the skew allowance. On failure, the run is a failed, audited run.
- Add `coverage_end` (and the cutoff applied) to `CasinoStatementInfo.AuditMetadata`.
- Fold this into the registry row CAS-STMT-STORE-1 / R-5, alongside the stored copy.

Not a blind spot over time. A posting excluded by a correct `CoverageEnd` is compared on the next run.
The ledger view is cumulative and has no lower bound. Probe `TestLF3_ReconDetectsBypassingWinAfterCasStmtIO1`
step 3/4: `CoverageEnd` after an injected posting reports it, and before it does not.

### LF-A3-2 (Low) - the simulation route now takes an L1 row lock before L0.1, which ADR 0082 does not record

This is CAS-BET-REQUIRES-BOOTSTRAP-1, not CAS-WIN-IDEMP-1, but it is on a money path and that makes it
mine to review. `runCasinoPlaySimulation`'s domain transaction calls `ConsumeLaunchSessionForMockPlay`
(`UPDATE casino_launch_sessions ...`, which takes a FOR NO KEY UPDATE lock on an L1 row) and THEN
calls `ReceiveVerifiedCallback`. That call takes L0.1, the status-gate keys, L0.4 RG and L0.2 in this
same transaction. ADR 0082, section 2.1 and the Amendment A6 table, says L0.1 is taken "first, while
holding no other lock", and orders L0.x before L1.

I looked for a deadlock partner and found none today:

- The only other lockers of that row are bootstrap (FOR NO KEY UPDATE, then RG; the same direction)
  and `RevokeLaunchSession` from `LaunchGame`'s failure path (no L0 lock held).
- `postBet`'s implicit FK lock is FOR KEY SHARE, which does not conflict.

The route is also non-production and MOCK-only. The record's claim "No new lock" is true for
CAS-WIN-IDEMP-1, but the branch as a whole does add this ordering.

Fix (either one):

- (a) Record it in ADR 0082 as a named exception: MOCK simulation route only, the row lock is FOR NO
  KEY UPDATE, and no path holds an L0 lock and then locks that row in a conflicting mode.
- (b) Consume the session in its own committed transaction before the domain transaction. A failed
  bet then leaves the session consumed, which is what a real vendor bootstrap does anyway.

I prefer (b). It restores the "L0.1 first" invariant without an exception.

### LF-A3-3 (sign-off condition on SR3-2's pending fix) - never refuse the tombstone of a never-seen original

Security SR3-2 proposes that a rollback whose `player_ref` does not resolve returns 400, "nothing
written, no tombstone", including for a never-seen original. Ledger-finance does not accept the "no
tombstone" part. A tombstone moves no money. It is the only protection against a late original
(CLAUDE.md rollback rule). The same reasoning made it exempt from the non-active-tenant gate
(R3-GAME-POSTINGS-NONACTIVE-1, ledger-finance C1).

If an identity-invalid rollback is refused without a tombstone, the late original posts, provided its
own reference resolves. Required shape: for a never-seen original, write the tombstone as today and
record the reference problem as a callback rejection or audit row. Refuse an unresolvable or
mismatching reference only on the path that would reverse a posted transaction. I will review the
SR3-1/SR3-2 fix when it lands. SR3-1's win-side check is acceptable from the ledger's side: a refused
win posts nothing and fails closed. The added `"player"` diff in `verifyPostedWinMatchesEvent` must
compare against the credited wallet's owner taken from the posted entries, never from the payload.

### LF-A3-4 (Low, test strength) - the A3 suite does not pin the `casino_win` type filter

Mutant A (drop `transaction_type = casino_win` from `findPostedWinTransaction`) survives every A3
test. It is killed only by my probes and by the pre-existing
`TestTenantNotActive_CasinoTombstonesAlwaysWrittenAndLateOriginalRecorded`.

With the mutant, a win whose reference is held by a tombstone is answered 409 payload-mismatch instead
of `ErrOriginalTombstoned`. It is still never paid, because the verifier sums 0 `house_gaming` debits
against a positive amount. `TestA3_WinRedelivery_SameReferenceDifferentKindIsNotAReplay` only asserts
"not answered as a replay", so it passes either way.

Fix: assert the exact error in that test, and add a tombstone-then-win case asserting
`ErrOriginalTombstoned` on the first and on repeated deliveries (my `TestLF3_TombstoneThenWin_RefusedRepeatedly`
is a ready template).

### LF-A3-5 (Info) - out of A3 scope, recorded so it is not mistaken for an A3 effect

- A rollback of a BET after its round's win settled is accepted on an active tenant. The player keeps
  the win and gets the stake back. The platform does not void the win, and that relies on the provider
  rolling back the win too. A3 does not change this. It only makes a redelivery of that win return
  the original. A shape-valid extra win in a bound round is invisible to both the MOCK statement and
  `casino_consistency` (probe `TestLF3_ReconBypassingWinInBoundRound`, 0 and 0 findings). Only a real
  provider statement catches it (the same probe with a provider-shaped source does).
- The record's sentence "MOCK tautological" is slightly too strong. The MOCK does report a posting
  whose round it cannot render (an orphan win in an unbound round gives a `fields=round` finding).
  This is pre-existing behaviour, pinned by `TestCasinoStatement_MockSeesUnboundRound`.

## CAS-WIN-IDEMP-1: what was verified

1. **`verifyPostedWinMatchesEvent`**
   - Payout is the sum of `house_gaming` DEBITS. That equals `event.Amount` for all three shapes:
     direct cash (2 legs), locked cash (+ lock-release legs on player accounts), and locked bonus.
     Locked bonus covers both the non-terminal grant and the terminal hold-capture to
     `player_bonus_held`.
   - The Rule B2 mirror legs that `ledger.Post` generates for BONUS_SET postings use
     `bonus_expense` / `promo_liability` / `provider_payable`, never `house_gaming`
     (`ledger/bonus_mirror.go`), so they cannot inflate the payout.
   - Asset is checked on every leg. Round is checked through the deterministic correlation id.
   - Integer `big.Int`, no float.
   - Mutants are all killed: remove the amount check, the round check or the asset check; count
     credits instead of debits; count all debits; remove the short-circuit. Under "count all debits"
     the locked branches fail, which proves the lock-release legs are correctly excluded.
   - Not compared: player and wallet. Today those are covered by the posting being the same
     `(provider, ref)`, whose wallet came from the round's own bet. SR3-1 adds the player check (see
     LF-A3-3).
2. **Short-circuit position**
   - The order is: L0.1, then the reference resolution, then the short-circuit, then the tombstone,
     tenant gate and `resolveWinOrigin`.
   - It answers only a committed `casino_win` row. A tombstone for the same reference is a row with
     the same `(tenant_id, provider_id, provider_tx_id)` (the tombstone carries `provider_tx_id =
     OriginalProviderTxID`), so the unique index `idx_ledger_transactions_tenant_provider_tx` makes
     the two mutually exclusive. Moving the short-circuit ahead of the tombstone check cannot unmask
     a tombstoned win.
   - Ahead of the tenant gate, it is equivalent to the pre-existing exemption for replays.
   - It writes nothing (no entry, no audit, no attribution, no held disposition). Skipping
     `ResolveTerminalGrantCredit` / `AttributeGrantLedgerTransactionIdempotent` on replay is safe,
     because both commit atomically with the original posting.
   - Under mutant E (short-circuit moved before L0.1) the A3 tests survive. That is expected: the
     read-only answer is safe either way, because `ledger.Post`'s key is the backstop. L0.1 only
     makes the answer deterministic under concurrency.
3. **Semantics against CLAUDE.md**
   - A redelivery after a rollback of the bet or of the win returns the original posting, writes
     nothing, and is not 400 or 409. That is correct. The win is a recorded fact. Each rollback is a
     separate compensating entry, and a duplicate of an already-recorded request must return the
     original (idempotency).
   - A FIRST delivery after a bet rollback is still refused (`ErrBetNotFound`), and a new reference
     for that round never pays (`TestA3_WinRedeliveredAfterBetRollback_ReturnsOriginal`).
   - The tombstone rule is untouched. A rollback of a never-seen win writes a tombstone. The late win
     is refused with `ErrOriginalTombstoned` on the first and every later delivery, and no
     `casino_win` row appears (`TestLF3_TombstoneThenWin_RefusedRepeatedly`).
4. **Concurrency**
   - First-delivery win vs. a rollback naming it, 25 parallel iterations: both serial orders were
     observed (4 win-first, 21 tombstone-first). There was never a win row and a tombstone row
     together, and a later redelivery always agreed with the order (replay vs. `ErrOriginalTombstoned`).
   - 10 parallel redeliveries plus a bet rollback plus a win rollback: all 10 returned the original
     id, each rollback posted once, the win posted once, and the balance was exact
     (`TestLF3_RedeliveryParallelWithRollbacks`).
   - The author's 12-way `TestA3_WinRedelivery_Concurrent_PostsOnce` is green, also under `-race`.
5. **Locked branches**
   - A redelivery of a locked-cash or locked-bonus win (non-terminal grant and terminal hold-capture)
     returns the original, and no ledger, entry, audit or balance row changes. This was a 409 before.
   - On a locked-cash win replay, these are all refused 409: a different round, a different asset,
     and the amount the PLAYER was credited (payout + released lock = 500 vs. 100)
     (`TestLF3_LockedCash_RedeliveryMismatches`).
6. **Payload mismatch maps to 409.** `ErrProviderTxPayloadMismatch` maps to 409 with an integrity
   alert and a generic body in `casino_handlers.go` (unchanged mapping).
7. **Unchanged:** the ledger schema, migrations, `internal/ledger` and `internal/wallet`. There is no
   balance `UPDATE` in the diff; the only `UPDATE`s are on `casino_launch_sessions` status. The A3
   win path adds no new lock and no new write: only plain SELECTs (`casino_provider_player_refs`,
   `ledger_transactions`, `ledger_entries`).

## CAS-GAME-KILL-BET-1: what was verified

- The gate is only in `postBet`, after the E2 replay, the E3 tombstone and the capability gate, and
  before L0.2. It uses plain reads. `postWin` and `postRollback` never reach it.
- A killed or unavailable game declines a NEW bet (`game_unavailable`) with zero ledger entries and
  no tombstone. The reference is not consumed: after the game is re-enabled, the same reference is
  evaluated fresh and posts once.
- While the game is disabled, these all work and the ledger stays balanced: a replay of a posted bet,
  a win for existing exposure, a rollback of a posted bet, and a rollback tombstone for a never-seen
  original (`TestA3_GameKill_*`, `TestLF3_GameKill_NoLedgerEffect`).
- The kill-vs-in-flight-bet ordering is READ COMMITTED (R-3). That is acceptable: such a bet is
  exposure taken before the kill and stays fully settleable.

## CAS-STMT-IO-1: what was verified

- The fetch happens before the REPEATABLE READ transaction. It is refused under `txscope.Held`, and a
  fetch failure is a failed, audited run with no partial rows.
- The MOCK renders inside the match transaction at the ledger's snapshot (Synthetic only), so it stays
  tautological and race-free.
- Tombstones are deliberately not bounded by coverage. A tombstone only pairs a rollback line, so it
  can excuse one but never create one.
- **Drift is still detected.** I injected a real drift with a bypassing writer: a `casino_win` posted
  directly with `ledger.Post`, never asserted by the provider, balanced, in a round with no bet.
  Probes `TestLF3_ReconDetectsBypassingWinAfterCasStmtIO1` and
  `TestLF3_ReconDetectsProviderOnlyLineViaFetcher` show:
  - `casino_consistency` reports `cas_orphan_win`.
  - The provider-shaped statement (captured before the injection) reports a key finding ("no
    statement line") and a GGR total finding. It does so with no `CoverageEnd` and with a
    `CoverageEnd` after the injection.
  - In a full `RunSweepTenants` with the MOCK, the sweep is not clean.
  - A provider-only line (the provider paid a win the ledger never posted) is a finding even with a
    `CoverageEnd`.
  - `SUM(debits) = SUM(credits)` still holds. The drift is semantic, which is exactly why these
    streams exist.
- Condition: LF-A3-1.

## Verification (clean `2bf9791` export, private DBs, `-tags integration -count=1 -p 1`)

| Check | Result |
|---|---|
| `./internal/casino/...` (incl. my probes) | PASS (135.4 s) |
| `./internal/casino/...` with `-race` | PASS (361.2 s), no data race |
| `./internal/reconciliation/...` (incl. my probes) | PASS (226.5 s) |
| `./internal/ledger/...` | PASS (51.2 s) |
| `./internal/payments/ -run TestR3_` (16 tests; the only payments file that drives the casino statement source in a sweep; no payments file changed on the branch, and no payments test touches casino bonus settlement) | PASS (114.1 s) |
| `./internal/httpserver/ -run 'Casino\|GamePostings\|ProviderReferenceBound\|WebhookAdmissionT6\|SimulationHandlers' -skip TestResolutionIsolation_` (82 tests) | PASS (56.7 s) |
| Win-idempotency mutants on `verifyPostedWinMatchesEvent` / `findPostedWinTransaction` / short-circuit (8) | 7 killed (amount, round, asset, credits-not-debits, all-debits, no short-circuit, no type filter*); 1 survivor E (before L0.1), which is equivalent for money (see item 2). *Killed only by my probes and a pre-existing test (LF-A3-4) |
| Hosted CI | not run by this review |

My probe tests (`internal/casino/lf3_probe_integration_test.go`,
`internal/reconciliation/lf3_probe_integration_test.go`) and the mutants were run only in the scratch
export. They are not committed and the code was not changed. LF-A3-4 suggests turning two of them
into permanent tests.
