# Stage 10 W1: ledger-finance sign-off

| Field | Value |
|---|---|
| Reviewer | `ledger-finance` specialist |
| Date | 2026-09-25 |
| Reviewed at | `f72d864` (HEAD; tree clean). W1 core `eb3912f` + `d26b3b9`; F-7 remediation `36616f1`; test-support route, read surfaces and reconciliation stream `f72d864` (these were still uncommitted when the review started; the committed content is what was reviewed) |
| Contract | ADR 0088 §2, §4, §5, §7, §8, §11; ADR 0020 Amendment 2026-09-25 |
| Mode | Review only. No production code was changed. One throwaway probe test was written, run and deleted (item 4). |

## Summary

| # | Item | Verdict |
|---|---|---|
| 1 | Sportsbook settlement postings (`internal/sportsbook/settlement.go`, migration 0091, `LockProjectionsForPostings`) | **APPROVED WITH FINDINGS** (P3 only) |
| 2 | F-7 remediation (`36616f1`: `internal/ledger/replay.go`, `ledger.Post`, callers) | **APPROVED WITH FINDINGS** (P3 only) |
| 3 | Sportsbook reconciliation stream (`internal/reconciliation/sportsbook_settlement.go`, `statement/`) | **APPROVED WITH FINDINGS** (P3 only) |
| 4 | Payments concurrent deposit reversal (`internal/payments/orchestrator.go`) | **CONFIRMED DEFECT, P1.** Does **not** block Stage 10 (pre-existing since Stage 3B and outside Stage 10 scope). Must be tracked as its own item. |

No P0 or P1 findings in Stage 10 scope. None of the financial vetoes apply to items 1 to 3:

- no floating point;
- no mutation of historical entries;
- no direct balance UPDATE;
- no money path without an idempotency key.

Evidence re-run for this review. All targeted runs PASS against the local CI database:

- `go test -tags integration -run 'Settlement|OB1|SoleWriter|Catalogue' ./internal/sportsbook/`
- `-run 'Replay|F7|…' ./internal/ledger/`
- `-run Sportsbook ./internal/reconciliation/`
- `-run 'F7|Replay'` over casino, payments, bonus, withdrawal, sportsbook and idempotency

A full-suite run was deliberately not started, because one was already in progress.

---

## 1. Settlement postings: APPROVED WITH FINDINGS

Verified against ADR 0088:

- **§2.3 exact entries.** Each case posts exactly the contracted entries:
  - **Lost** (`settlement.go:633-636`): Dr LOCKED S / Cr HOUSE S.
  - **Won** (`:637-643`): the same pair, then Dr HOUSE P / Cr CASH P. This is the full payout, not the winnings (Flow 9 trap avoided), in R7 insertion order.
  - **Void-before** (`:754-761`): Dr LOCKED S / Cr CASH S.
  - **Rollback** (`:832-896`): the stored entries are loaded and each direction is flipped. The code checks that every entry is on the bet's own CASH, LOCKED or HOUSE account, that the settlement is not already reversed, and that `reverses_transaction_id` is set.
  - **Void-after-settlement** (`:787-816`): rollback, then a void, in one DB transaction.
  - **Rollback-then-void**: uses the before-settlement shape.
  - **Tombstone** (`:713-737`): `TxTombstone`, no entries, and the settlement key.
  - **All W1 postings**: `ProviderID`, `ProviderTxID`, `ReasonCode` and `BonusCost` are nil, and `CorrelationID = bet.id`.
  - **Causation** (§2.1): re-settlement points to the g−1 rollback or tombstone (`:620-652`). The composed void points to its rollback (`:910-914`). Every other posting has nil causation.
- **INV-SB-SETTLE-1.**
  - The stake is taken only from the placement posting (`deriveBetLedgerAccounts`, `:472-540`). The code requires:
    - type `sportsbook_bet`;
    - `correlation = bet.id`;
    - exactly two entries: Dr `player_cash` / Cr `player_locked_cash`;
    - equal amounts;
    - the same asset and wallet;
    - `S = bet.stake_amount > 0`.
  - The payout is taken only from `bet.potential_return` (`:611-616`).
  - Claim fields are validated (V-1…V-4) but never reach a `TransactionInput`.
  - T-1 in migration 0091 re-enforces V-3/V-4 on the history row.
- **NetLocked** (`:452-466`, `betNetLocked` `:552-575`):
  - The query is scoped to the four lifecycle types and to `correlation_id = bet.id`.
  - The NUMERIC sum is converted to text and then to `big.Int`.
  - An open bet requires `NetLocked = S`. A settled or void bet requires 0.
  - `NetLockedBonus` must be 0.
  - Any violation returns a `SETTLEMENT_INTEGRITY` rejection before any lock beyond L1 is taken.
- **Idempotency keys** (§4.2, `:183-193`):
  - Keys use the server-fixed prefixes, the canonical lowercase UUID, and an unpadded generation.
  - The tombstone shares the settlement key.
  - The generation is validated under L1 and never counted.
  - The ledger `UNIQUE (tenant_id, idempotency_key)` is the only idempotency authority.
  - The history partial unique indexes enforce state. They are also the DB-level backstop against double rollback, one per settlement, which is DB-enforced rather than check-then-insert.
- **§4.7 backstop** (`lockAndPost`, `:905-943`). All three Post replay outcomes abort the transaction as `ErrSettlementIntegrity`: `AlreadyPosted`, `ErrIdempotencyKeyReused` and `ErrIdempotencyPayloadMismatch`. The fault-injection test `TestSettlementFaultInjection_LedgerKeyBackstop` passes. The F-7 audit's §7.4 item 1 ("mapping must land with the remediation") is **resolved**: the mapping is committed in `36616f1`.
- **OB-1.** Rollback and void run no balance check and no RG or eligibility gate. `TestOB1_RollbackOfWonSettlementCanDriveCashNegative` asserts two things: `player_cash` goes to −P and the posting succeeds, and `player_locked_cash` stays at S and never goes negative. **OB-1 remains OPEN.** Its business treatment of the receivable is not resolved by W1, and this sign-off does not resolve it.
- **`LockProjectionsForPostings`** (`internal/ledger/lockorder.go:126-161`):
  - It runs `prepareEntries` per input, so the union includes Rule B2 legs.
  - It rejects mixed tenants (`ErrPreLockTenantMismatch`) and empty input.
  - It takes one canonical-order `ensureAndLockProjectionsInOrder` over the union.
  - R3 generalises correctly. `lockAndPost` pre-locks exactly the inputs it then posts, unchanged apart from `CausationID`, which is not an entry field.
  - A tombstone locks nothing (empty set), which is correct.
  - R4 is preserved.
  - `TestSettlementConcurrency_VoidAfterSettlementRacesPlaceBetOnSameWallet` covers the deadlock case §5.2 exists for.
- **Double-entry.** Every posting balances per asset, and Post's `SET CONSTRAINTS ledger_entries_balanced IMMEDIATE` makes each call fail synchronously if it does not.
- **Migration 0091.**
  - The type CHECK is purely additive (17 → 20 values).
  - The history table is append-only (deny triggers plus REVOKE) with FORCE RLS.
  - Shape CHECKs, the T-1 sequence, target and causation checks, and T-2 status derivation all match §3.
  - The down-migration refuses to run once any settlement evidence exists.
  - `payout_amount BIGINT` is the recorded §3.6 debt, not a new decision.

### Findings (item 1)

- **P3-1.1: rollback "same order" is not literally met.** `settlement.go:858-861` loads the settlement's entries `ORDER BY ledger_account_id, direction`. `ledger_entries` has no insertion-ordinal column (`id` is a random UUID; migration 0022), so the original insertion order cannot be recovered. This is the same pattern casino `loadEntries` uses (`internal/casino/orchestrator.go:1524-1530`). There is no financial effect: the inverse is exact as a multiset, and locking order is owned by L3. **Fix:** amend ADR 0088 §2.3's Rollback row to read "exact inverse, deterministic account order", or record it as an accepted deviation. No code change is needed.
- **P3-1.2: `ErrSettlementTombstoned` does not exist; the ledger backstop maps to `ErrSettlementIntegrity`.** ADR 0088 §4.3/§4.5 name a typed `ErrSettlementTombstoned` and a key-reuse→tombstoned mapping. The implementation:
  - returns the `SETTLEMENT_TOMBSTONED` **result code** from the L1 decision table (`:588-590`);
  - maps `ErrIdempotencyKeyReused` to `ErrSettlementIntegrity` (`:916-918`).

  This is fail-closed and consistent with §4.7. The backstop can only fire when history and ledger disagree, which is an integrity event. But the ADR text, the ADR 0020 amendment ("ADR 0088 §4.5 maps this sentinel to `ErrSettlementTombstoned`") and the comment at `internal/ledger/ledger.go:173-176` all describe a mapping that does not exist. **Fix:** correct the ADR 0088 §4.5 text, the ADR 0020 amendment sentence and the `ledger.go` comment to say "mapped to `ErrSettlementIntegrity` (§4.7)". Alternatively, add the typed error. The doc fix is preferred.
- **P3-1.3: `house_gaming` is resolved before L2 and on rejection paths.** `deriveBetLedgerAccounts` (`:532-538`) calls `GetOrCreateAccounts` during state load. That happens before the §5.1 step 3 L2 lock, and also for events that are then rejected. It is harmless:
  - account creation takes no L0 to L3 lock that could close a cycle;
  - L2 is only contended by same-bet writers, which queue behind L1;
  - creation is idempotent.

  But it deviates from the §5.1 step table. **Fix:** either move the house resolution into the posting branches after `buildRollbackInput`, or amend §5.1 to say "step 4 may precede step 3". Non-blocking.
- **P3-1.4: `lockAndPost` overwrites `CausationID` implicitly.** `lockAndPost` (`:910-914`) unconditionally sets `in.CausationID` of input i>0 to the id of input i−1. This is correct for its only two-input caller (the composed void), but a future multi-posting caller would have its causation silently overwritten. **Fix:** set the void's causation explicitly in `voidBet`, for example with a per-input callback or by posting the two inputs individually after the union pre-lock, or add a guard that fails if an input i>0 already carries a `CausationID`.
- **P3-1.5: no ledger-package test pins `LockProjectionsForPostings`' own contract.** Missing: a tenant-mismatch rejection test, an empty-input test, and "union equals the set subsequent Posts lock". Coverage is only indirect, through sportsbook tests. **Fix:** add these tests in `internal/ledger`.

## 2. F-7 remediation (`36616f1`): APPROVED WITH FINDINGS

This is the ADR 0088 §11.2 "ledger-finance review".

Verified:

- **`Post` conflict path** (`internal/ledger/ledger.go:361-383`). It returns exactly one of the following, in this order:
  1. `ErrIdempotencyKeyReused` if the type differs. This check still runs first.
  2. `ErrIdempotencyPayloadMismatch` if the payload differs.
  3. `AlreadyPosted` only on a full match.

  Nothing new is written on any of these branches.
- **Canonical compare** (`internal/ledger/replay.go`):
  - The entries compared are the *final* set (`entriesToPost`, including Rule B2 legs), so a legitimate retry regenerates identical legs. `applyBonusMirror` is a pure function and reads no balance.
  - Entries are compared as a multiset, order-insensitive and never netted.
  - Amounts are compared as decimal text on both sides. The stored side is `amount::text` of `NUMERIC(38,0)`; the requested side is `FormatInt`. There is no truncation path.
  - The compare also covers `reverses_transaction_id`, `provider_id`/`provider_tx_id`, `reason_code` and `causation_id`, and `correlation_id` for every type except `tombstone`.
  - Error text names field classes only, never amounts, accounts or references.
- **Tombstone correlation exemption: accepted.**
  - A tombstone moves no value.
  - Its key and provider reference are still compared.
  - The legacy writers mint `uuid.New()` correlations that cannot be made deterministic retroactively.
  - Sportsbook tombstones use a deterministic correlation anyway.
- **No new lock.** The compare reads immutable rows while holding the L3 set of the request. This is sound.
- **No schema change.** The ADR 0020 "comparison hash" OPEN DECISION is resolved by comparing stored rows directly. Accepted.
- **Callers:**
  - `PlaceBet` resolves its per-attempt-correlation race by re-reading the bet.
  - Casino and payments map the mismatch to domain integrity errors, returning 409 with an alert.
  - The payments same-reference redelivery is now idempotent.
  - The tests that pinned the old behaviour were rewritten.
  - A replay regression test exists per caller package, and all of them pass: casino, payments, bonus, withdrawal, sportsbook, idempotency and ledger.

### Findings (item 2)

- **P3-2.1: stale documentation of the `ErrIdempotencyKeyReused` mapping.** See P3-1.2 (ADR 0020 amendment §Decision item 1 and `ledger.go:173-176`).
- **P3-2.2: the F-7 audit record does not list the W1 call sites.** `docs/governance/stage-10-f7-ledger-replay-audit.md` says the W1 settlement, rollback and void sites "must be added to §3 when they land". They are not listed. **Fix:** add `internal/sportsbook/settlement.go` `lockAndPost`, which covers settlement, rollback, void and tombstone postings. Classify it as **A** (key is `prefix + bet_id + #g`; every payload input is derived from stored state under L1) plus the §4.7 backstop, with evidence `TestSettlementFaultInjection_LedgerKeyBackstop` and the decision-table replay tests.
- **P3-2.3 (pre-existing, recorded only): `db.IdempotentInsert` treats any unique violation as a key conflict.** When a new key collides only on `(provider_id, provider_tx_id)`, the result is an untyped wrapped `ErrNoRows` error. This is fail-closed and now pinned by a test. A typed sentinel would make caller mapping clearer. Defer.

## 3. Sportsbook reconciliation stream: APPROVED WITH FINDINGS

Verified against ADR 0088 §8:

- **(a) §8.1** (`sportsbook_settlement.go:210-306`):
  - `player_locked_cash` net is scoped to the four sportsbook types only, which excludes casino.
  - It is compared per `(wallet, asset)` with Σ stake of open bets, using a FULL OUTER JOIN so a missing side on either end is caught.
  - `player_locked_bonus` over the same types must be 0. The code never uses the wallet's whole bonus balance.
- **(b) §8.2** (`:532-617`):
  - CASH and LOCKED are re-derived from the placement posting, with the same checks as §2.2.
  - HOUSE is the tenant `house_gaming` account for the bet's asset.
  - Any net on a foreign account is flagged.
  - End states match the §2.3 table exactly.
  - For `settled_won`, the code requires exactly one un-reversed won row. It checks that the ledger CASH payout of that row's transaction equals `payout_amount` and that the transaction is not reversed in the ledger.
- **(c) §8.3** (`:633-816`):
  - Two-way orphan check, including sportsbook-namespace tombstones: exactly one history row with matching kind and bet; each history row has a ledger transaction of the matching type and correlation.
  - Causation follows §2.1, including `causation_id IS NULL` for every other posting.
  - A rollback's `reverses_transaction_id` equals its settlement's.
  - Status equals the T-2-derived status.
- **(d) §8.4 MOCK** (`:842-946`; `internal/sportsbook/mock.go`):
  - The platform side is derived from the **ledger**, not from the history table the mock renders, so the match is not self-referential on the ledger side.
  - The MOCK label is carried in mismatch values, audit metadata and logs.
  - The source is injectable, and a nil source fails the run closed.
- **`big.Int` discipline.** Every NUMERIC sum and amount is cast to `::text` and parsed with `big.Int.SetString`. No int64 summation happens anywhere in the stream. Statement lines carry int64 because the history column is BIGINT (recorded §3.6 debt), and they are widened to `big.Int` before comparison.
- **Zero tolerance.** Every comparison is `Cmp != 0` or `Sign != 0`, with no epsilon or threshold. The footprint guard (`:147-158`) only skips tenants where every check input is provably empty. It is a cost guard, not a tolerance.
- **Scheduling** (`scheduler.go`):
  - The stream runs per tenant after `ledger_vs_projection`, in its own tenant-scoped transaction under a stream-specific advisory lock.
  - Every attempt is audited, and failures are audited in a fresh transaction.
  - Mismatches log at Error level ("MISMATCH FOUND"), the same P1 path as `ledger_vs_projection`.
- **Tests:** clean after every lifecycle flow, divergent statement detected, and drift injection per mismatch kind. All pass.

### Findings (item 3)

- **P3-3.1: statement-view key collision is silent.** In `sbLedgerStatementView` (`:875-883`), two un-reversed settlement transactions of one bet whose keys fail `sbGenerationFromKey` would both map to `…settlement#<null>`, and the second would overwrite the first in the map. It is mitigated because the orphan and per-bet checks flag the same corruption. **Fix:** detect the duplicate ledger-side key and record a mismatch, mirroring the statement-side duplicate check at `:912-915`.
- **P3-3.2: every run scans the whole tenant.** Each run rescans the tenant's entire sportsbook population, which is acceptable at W1 volumes and consistent with `ledger_vs_projection`. **Fix (deferred):** record it as a scaling consideration: incremental or partitioned recomputation before sportsbook volume grows.

## 4. Payments deposit reversal race: CONFIRMED, P1, does not block Stage 10

**Confirmed by reading.** `receiveDepositReversalCallback` (`internal/payments/orchestrator.go:931-1045`):

- loads the original intent with a plain SELECT, without `FOR UPDATE` (`loadDepositIntentByProviderRef`, `:383-396`);
- runs an unlocked `EXISTS (… reverses_transaction_id = $1 AND other reference)` check (`:986-995`);
- then calls `ledger.Post` under a key derived from the **reversal's own** reference (`:1010`).

No constraint makes one reversal per deposit unique at the database: `idx_ledger_transactions_reverses` (migration 0021:55) is not unique. Two concurrent callbacks with **different** reversal references both pass the EXISTS check before either commits. Post's L3 projection lock only serializes them; it does not re-check. Both then commit. This is check-then-insert, which CLAUDE.md forbids for financial writes ("enforced by the database, not check then insert").

**Confirmed empirically (deterministic probe, since deleted).** The probe:

- used a throwaway integration test in `internal/payments` (removed after the run; tree verified clean);
- ran reversal A to completion inside tx1 without committing;
- started tx2 with reversal B for the same deposit;
- polled `pg_stat_activity` until tx2's backend was blocked on a lock inside Post, which is after its EXISTS check;
- then committed tx1.

Result, 3 of 3 runs: **both callbacks returned success, 2 `deposit_reversal` postings for one 1,000 deposit, and `player_cash` went from 1,000 to −1,000.** This matches the F-7 implementer's report of −3,000 to −5,000 with 6 concurrent reversals.

**Pre-existing.** The pre-F-7 check (`EXISTS … reverses_transaction_id = $1`) was equally unlocked. F-7 only excluded the reversal's own reference, which is correct and adds no new sequential bypass: a distinct reference after a committed reversal is still rejected. The defect dates from Stage 3B.

**Severity: P1.** It is a financial-correctness defect:

- a deposit can be reversed N times, over-debiting the player and over-crediting `psp_clearing`;
- it is **undetectable by current reconciliation**, because the ledger stays balanced and the projection agrees with it, so `ledger_vs_projection` is clean;
- no stream checks "at most one reversal per deposit".

It is not P0 because:

- it is reachable only through a signature-verified PSP webhook (`POST /v1/webhooks/payments/{tenantSlug}/{providerID}`), not by a player (the player simulate-callback route cannot emit reversals);
- no real PSP is integrated (mock only, dev and synthetic data);
- the error direction debits the player and yields no value extraction.

**Stage 10 impact: does not block.** It is outside Stage 10's approved scope, pre-existing, and not touched by W1's ledger paths. It **must** be closed before any real PSP integration or production launch.

**Recommended tracking:** open it as its own registered finding, for example `PAY-REV-1`, owned by `payments` with `ledger-finance` review of the fix and ADR 0082 lock-order review by `architect`. Record it in `docs/governance/task-registry.md` and in the Stage 10 completion report's risk list. Do not fold it into W1.

**Recommended fix (all three parts):**

1. **L2 lock.** Before the already-reversed check, `SELECT … FROM ledger_transactions WHERE id = original.LedgerTransactionID FOR UPDATE`. This is the casino `postRollback` precedent, and it comes before any L3 per ADR 0082.
2. **Database backstop (ledger schema, needs ledger-finance sign-off).** Add a partial unique index `ON ledger_transactions (reverses_transaction_id) WHERE transaction_type = 'deposit_reversal'`, so that the invariant holds even for a future writer that skips the lock. The migration must first verify that no duplicate exists, and must fail loudly if one does. Extending the index to other reversal types (`casino_rollback`, `sportsbook_rollback`, `withdrawal_reversed`) should be evaluated in the same item: sportsbook is already protected by its history index.
3. **Test.** Add a regression test with exactly this probe's shape (tx1 posted but uncommitted, tx2 blocked, commit), asserting exactly one reversal and `ErrDepositAlreadyReversed` for the loser. Optionally add a reconciliation check, "≤ 1 `deposit_reversal` per original", to the payments stream when it exists.

---

## Open business decisions flagged (not decided here)

- **OB-1** (receivable from a negative `player_cash` after rollback of a spent win) remains **OPEN** (ADR 0087). W1 correctly posts it. The business treatment (collections, write-off, limits) needs the human.

## Sign-off statement

Items 1 to 3 are signed off by `ledger-finance` for Stage 10 W1 **with the P3 findings above**. None blocks merge or the stage gate; each should be closed or recorded as a deviation before Stage 10 completion. Item 4 is a confirmed pre-existing P1 outside Stage 10 scope. It does not block Stage 10, and it must be tracked and fixed as its own item before any real payment provider is connected. This sign-off is about financial correctness only. It is not a security review (`security`) and says nothing about regulatory approval.
