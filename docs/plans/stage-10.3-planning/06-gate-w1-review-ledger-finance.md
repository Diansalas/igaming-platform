# Gate 10.3-W1 — ledger-finance review of W1c (CAS-CAP-ROLLBACK-1 + G-1)

- **Reviewer:** `ledger-finance` (owner of the financial invariants). This review also gives the `casino` domain concurrence the gate log asks for, from a reading of `internal/casino` as the casino specialist would read it.
- **Scope:** diff `4a2a978..ee2192f`, W1c commit `46cf28b`. Files: `internal/casino/{orchestrator.go,bonus_settlement.go,capability.go,types.go}`, `migrations/0094_*`, `internal/httpserver/{casino_handlers.go,casino_admin_handlers.go}`, and the new and changed tests.
- **Baseline:** my own paper, `02-casino-financial-analysis.md` (§1.3 table, §1.4 steps, §1.11, G-1/G-2/G-5). Also ADR 0082 Amendment A6, the ADR 0025 Stage 10.3 amendment, `04-review-qa.md` (W1c section and condition 1), and `05-gate-log.md` (item 9, E10).
- **What was executed:** `go vet` (default tags and `-tags integration`) on `internal/casino` and `internal/httpserver` came back clean. Unit tests passed for `internal/casino` (including `TestClassifyDirectOriginRows_G1` and `TestCasinoConformance_BetWithoutRollback_FailsSuite`), `internal/ledger`, `internal/providers`, and `internal/httpserver` (non-integration).
- **What was not executed:** integration tests were not run, as instructed, because a DB agent is running. Every integration-level claim below comes from reading the tests, not running them.

## Verdict: **APPROVE WITH CONDITIONS**

The code is financially correct. All invariants I own hold (§1). The conditions are about evidence: test cases required by CLAUDE.md, ADR 0082 A6, and QA condition 1 are missing or weaker than specified, and two documentation items are missing. W1c **must not be labelled `IMPLEMENTED`** until conditions C1–C9 are met and the W1c integration suites have run green as the NOBYPASSRLS role. Until then it is `PARTIALLY IMPLEMENTED`. None of the conditions requires a change to production code except the optional C10.

---

## 1. Financial invariants

| Invariant | Result | Evidence |
|---|---|---|
| SUM(debits) == SUM(credits) | **Holds.** No new posting shape was added. E4/E7 are the existing win and reversal paths. E8 is the existing zero-entry tombstone. E3 and E10 post nothing. Every posting still goes through `ledger.Post` and its deferred balance trigger. `internal/ledger` is untouched apart from one test file. | `orchestrator.go:1690`, `:1765`; `bonus_settlement.go:400` |
| Idempotency keys unchanged | **Holds.** Bet, win and rollback keep `provider:provider_tx_id`. The tombstone key stays `tombstone:<provider>:<original>` and is only lifted into a local variable. The DB unique constraints `(tenant, idempotency_key)` and `(tenant, provider_id, provider_tx_id)` are unchanged. | `orchestrator.go:1756`, `:1693` |
| No balance UPDATE | **Holds.** No `UPDATE`, `DELETE`, or float appears in non-test diff lines. Projections change only through the ledger trigger. | diff scan |
| Compensating entries only | **Holds.** A rollback is still a new `casino_rollback` with `reverses_transaction_id` and inverted entries. | `orchestrator.go:1690-1699` |
| Unseen-rollback tombstone always written | **Holds** for every verified rollback. The pre-dispatch capability check is deleted, `postRollback` reads no capability, and the `pgx.ErrNoRows` branch always calls `postRollbackTombstone`. The pre-verification 401 exception (security > settlement; suspended tenant, HD-10.3-4) is by design and disclosed. | `orchestrator.go:704-735`, `:1557-1575` |
| Late original rejected, no posting | **Holds.** The E3 bet check runs under L0.1 on the same reference, before RG/Risk/L0.2/`BindProviderRound`/`Post`, and returns a decline with no ledger write. The E10 win check runs under L0.1 before the L2 round lock and returns `ErrOriginalTombstoned`, which rolls the transaction back. The unique index remains the final backstop. | `orchestrator.go:1016-1030`, `:1426-1430` |
| No stranded exposure caused by configuration | **Holds.** Win and rollback never read the capability. The CHECK in 0094, `WriteCapability` (`capability.go:117`) and the conformance case together stop a configuration from opening exposure it declares it cannot settle. `LaunchGame` requires `supports_bet` for real mode (`orchestrator.go:307`), so no usable real-money session exists in which every bet 503s. The residual stranding paths (credential revocation, adapter deregistration, tenant suspension) are pre-verification and by design, as disclosed in §1.10 and G-7. | |
| Deterministic tombstone correlation | **Correct and replay-safe.** Tombstones are exempt from correlation comparison (`ledger/replay.go:115`). All correlation-keyed casino reads filter on `transaction_type = casino_bet` (`orchestrator.go:1461-1463`, `bonus_settlement.go:80-98`) or are entry-based (the tombstone has no entries), so a tombstone that shares its round's correlation cannot enter origin resolution or the L2 round lock set. Informational: the no-RoundID fallback `uuid.NewSHA1(OID, "tombstone:<provider>:<ref>")` (`:1757`) is not tenant-qualified. That is harmless because `correlation_id` carries no uniqueness constraint and every consumer is tenant-scoped. | |

### 1.1 Lock order (ADR 0082)

- **`postRollback`** takes L0.1 on the **original** reference as its first lock (`orchestrator.go:1540`). That is before the L2 `FOR UPDATE` (`:1554`) and before any E-1 L0.3 grant advisory (`:1676`) or `postRollbackHeldWin`'s L0.3, so it conforms to A6.
- **`postBet`** is unchanged. L0.1 (`:962`) is followed by the idempotency short-circuit, the E3 check, session, and the capability gate (a plain SELECT, `:1087`). L0.2 (`:1111`) comes after, so a rejected bet never takes the player lock.
- **`postWin`** takes L0.1 on its **own** reference as its first lock (`:1413`). It then runs the E10 check and takes the L2 round `FOR UPDATE ORDER BY id` (`:1459-1466`).
- **Deadlock check.** Each of the three takes L0.1 exactly once, first, and holds nothing else when it requests it. Every caller runs one callback per transaction: `casino_handlers.go:350` and `casino_play_handlers.go:354/447/587` do only plain reads before `ReceiveCallback`. Worked case: T1 is `postWin(W2)`, holding L0.1(W2) and waiting on the bet row B. T2 is `postRollback(orig=B)`, holding L0.1(B) and row B and heading to L3. T2 never requests L0.1(W2), so there is no cycle. Multi-bet rounds lock bet rows in ascending id order. No new edge enters the graph.

**A6 concurrence: I CONCUR** with A6 as applied to `postRollback`, and with the permitted extension to `postWin`.
- **Why the `postWin` lock is justified.** Without it, a win W and a rollback naming W can both see "no row". One writes the tombstone and the other posts `casino_win`, both with `provider_tx_id = W`. The unique index lets only one commit, and the loser gets an untyped unique-violation 500.
- **So the lock buys determinism, not money safety.** Nothing could double-post before. The lock turns a nondeterministic 500 into {win posted, then reversed (E7)} or {tombstone, then 409 (E10)}.
- **The ADR's rules still hold:** one L0.1 per transaction, taken first, the same class and key format (`acquireProviderTxDeliveryLock`, `:587`), and R8 intact.
- **Condition:** A6 itself requires the extension to be recorded in the §1.3 inventory (see C8).

### 1.2 G-1: which wallet a two-cash-bet win credits

- **Which wallet.** `classifyDirectOriginRows` (`bonus_settlement.go:194-224`) runs only on the direct branch (`:271`). Two or more un-reversed `casino_bet` rows on one wallet, all `player_cash`, resolve to that wallet. `postWinDirectCash` then posts Dr `house_gaming` / Cr `player_cash` on `origin.WalletID` for `event.Amount`, after `postWin` has checked `wallet.AssetCode == event.AssetCode` (`orchestrator.go:1485`). So the win credits the **one wallet that funded every stake in the round** (the bettor's wallet for that asset). It never credits a payload-named player or wallet.
- **Why picking `rows[0]` is harmless.** `BetTransactionID` is not dereferenced on the cash path, so the choice has no financial effect. Reversed bets are excluded by the `NOT EXISTS reverses_transaction_id` clause, so rollback-one-then-win resolves to the remaining bets, and rollback-all gives `ErrBetNotFound`.
- **Bonus origins are not mis-resolved:**
  - Different wallets → `ErrCorrelationWalletCollision`.
  - Cash + bonus in any combination → `ErrMixedFundingUnsupported`. Pre-fix, cash in one transaction plus bonus in another returned `ErrAmbiguousMultiOriginRound`. Both map to 409 and neither posts.
  - Two or more bare `player_bonus` rows → `ErrAmbiguousMultiOriginRound`.
  - A single `player_bonus` row → `ErrBonusBetNotLocked` (`:279`).
  - The locked branch still uses the unchanged strict `classifyOriginRows`.
- **Pre-existing observation, not introduced by W1c:** a round that has both a locked stake and a direct cash stake resolves through the locked branch. The cross-branch wallet-collision check does not run there. This is unreachable today because `postBet` is cash-only (G-6). It must be addressed when bonus-funded or locked casino stakes are designed.
- **Mapping of the other abort classes.** The remaining §16.4 abort classes now map to 409 (`casino_handlers.go:425-444`). That is right: a retry can never resolve them. The cost is a terminal non-payment with only a log line until W2b's rejection record exists. **W2b must capture these 409s** (see C9).

### 1.3 E3 decline path posts nothing

This holds. The branch writes exactly one `audit_records` row (`casino_bet.rejected_tombstoned`) and returns `OutcomeDeclined` with a nil error, so the transaction commits with zero ledger, round, or projection writes (`orchestrator.go:1016-1030`). Nothing runs before it except L0.1, `findPostedBetTransaction`, and the tombstone SELECT.

---

## 2. Response-shape concurrence (financial + casino domain)

- **E1 → 503 unchanged: CONCUR.**
  - Financially, 503 and decline are equivalent because neither posts.
  - If the provider retries after re-enable, E2 idempotency makes it safe.
  - If the provider cancels instead, the rollback tombstones (E8), and a later retry of the bet gets E3.
  - Casino domain: keeping 503 avoids an API change on the bet path. Aggregators generally treat a 5xx on a debit as "retry, or cancel with a rollback", and both outcomes are safe here.
- **E3 → 200 `declined` / `original_rolled_back` + audit in the same transaction: CONCUR.** This is the recommendation in §1.3 note B. "Declined" is literally true, it is deterministic and non-retryable, and the durable record commits. Redeliveries write one audit row per attempt, which is correct for an attempt log.
- **E10 → 409 `ErrOriginalTombstoned`: CONCUR.**
  - It matches the §1.3 table.
  - The durable record is W2b's rejection record, per the ADR 0025 amendment item 4. Until W2b lands, only the log line `casino_webhook_integrity_alert_original_tombstoned` remains. This is disclosed.
- **Capability gate in `postBet` with the brand-scoped lookup (`:1087-1092`): CONCUR.**
  - It closes F4.
  - S-none is rejected.
  - The asset is re-checked against `supported_assets`.
- **`LaunchGame` requires `supports_bet` (real mode): CONCUR.** It has no ledger effect.
- **Migration 0094: CONCUR.**
  - The refusal-based CHECK uses the validating `ALTER` as its pre-flight and does not fix data automatically.
  - Down is `DROP CONSTRAINT IF EXISTS`.
  - It matches §1.5 verbatim.
- **Capability audit before/after: CONCUR.** It closes F6/G-5. A minor point: the before-image is an unlocked read (`casino_admin_handlers.go:267`), so two concurrent admin writes could record a stale "before". This is an audit-accuracy issue, not a financial one, and is optional to fix.

---

## 3. Findings

| # | Sev | Finding | Location |
|---|---|---|---|
| F-1 | **Condition** | The late-original race test runs **20** iterations. ADR 0082 A6 and QA require **50**, and require the test to assert **where the waiter blocks (L0.1)**, for example by observing the second transaction in `pg_locks` (advisory, not granted) before releasing it. Today it asserts only the outcome. The same applies to the E10 race. No W1c mutation-kill evidence file exists for the claimed "untyped 500 without the `postWin` lock", although `evidence/` holds only e1–e6 and W1a files. | `casino_cap_rollback1_concurrency_integration_test.go:202`, `:304` |
| F-2 | **Condition** | The E3 audit row `casino_bet.rejected_tombstoned` is **never asserted** by any test. This is a QA auditability requirement, and the row is what justifies choosing decline over 409. Also absent: the six-point no-effect check (`noeffect.AssertNoCasinoEffect`) on the E3 path and on the sequential E10 path. There is no sequential E10 test outside the race. | `casino_cap_rollback1_prefix_test.go:195-258` |
| F-3 | **Condition** | G-1 test set incomplete versus `04-review-qa.md` W1c: `_Replay`, `_ConcurrentWins`, `_RollbackOneThenWin`, and an HTTP-level 409 for any abort class are missing. `TestCasMultiBetWin_G1_AmbiguousIntegrityAlertsMapTo409` is **mislabelled**: it asserts the orphan-win **400** and never exercises the new 409 mapping. The two-wallet case exists only at unit level. | `casino_multibet_win_prefix_test.go:107-127` |
| F-4 | **Condition** | Idempotency-under-concurrency tests (QA condition 1) assert status 200, the row count and the balance, but not "exactly one audit row" or "every loser got the *original* result" (the same `ledger_transaction_id` in the body). | `casino_cap_rollback1_concurrency_integration_test.go:31-186` |
| F-5 | **Condition** | Missing §1.11 cases: **S-nobet** (active, `supports_bet=false`) bet rejection; a brand-B capability must not authorize brand-A bets; a capability disabled concurrently with an in-flight bet; bet 503 → re-enable → retry posts once; partial failure (injected error after the tombstone insert, before the audit → full rollback, then one tombstone on redelivery); E2 bet replay while disabled returns the original id; rollback of a posted **win** while disabled; a `ledger_vs_projection` sweep clean after the scenarios. | test suite |
| F-6 | Low | The player play-simulation error mapper, `writeCasinoCallbackError`, claims to mirror the webhook mapper "exactly" but does not map `ErrOriginalTombstoned` or the G-1 409 classes, which fall through to 500 on the simulation route. It has no money effect, because the transaction rolls back. | `casino_play_handlers.go:219-275` |
| F-7 | Info | `txs` is populated but never read in `classifyDirectOriginRows` (dead code). The E3 audit row uses `TargetType: "ledger_transaction"` with a provider reference as `TargetID`. Both are cosmetic. | `bonus_settlement.go:200`; `orchestrator.go:1020` |
| F-8 | **Condition (docs)** | ADR 0082 §1.3 inventory (`postWin` row `:127`, `postRollback` row) is not updated with L0.1, which A6's "Permitted extension" makes mandatory once implemented. A6's status still reads `NOT IMPLEMENTED`. | `docs/decisions/0082-canonical-financial-lock-ordering.md:127,1497,1504` |

## 4. Conditions (binding before W1c is labelled `IMPLEMENTED`)

- **C1.** Raise both L0.1 race tests to 50 iterations, and add a waiter-position assertion (`pg_locks` shows the advisory lock as not granted) to at least one iteration of each. Record W1c mutation-kill evidence for L0.1 in `postRollback` and `postWin`. [F-1]
- **C2.** Assert the E3 `casino_bet.rejected_tombstoned` audit row. Add `AssertNoCasinoEffect` to E3. Add a sequential E10 test (tombstone first, then the win → 409, no effect). [F-2]
- **C3.** Add the G-1 cases `_Replay`, `_ConcurrentWins`, `_RollbackOneThenWin`, and an HTTP-level 409 for at least one abort class, for example `ErrCorrelationWalletCollision` seeded directly. Rename or fix the mislabelled test. [F-3]
- **C4.** In the three concurrent-identical tests, assert one audit row and the same `ledger_transaction_id` in every response. [F-4]
- **C5.** Add the missing cases listed in F-5.
- **C6.** Run every W1c integration suite green as the NOBYPASSRLS role once the DB is free, and record the run.
- **C7.** Neither this review nor the tests cover the security side of losing the settlement kill switch. That remains `security`'s concurrence at the gate (§1.7).
- **C8.** Update the ADR 0082 §1.3 inventory for `postWin`/`postRollback` L0.1, and set A6's status to `IMPLEMENTED` after C1 and C6. This is `architect`'s action with my sign-off. [F-8]
- **C9.** Carry forward to W2b: the rejection record must capture E10 and the G-1 409 classes, because those are terminal non-payments with only a log line today.
- **C10 (optional, not blocking).** Align `writeCasinoCallbackError` with the webhook mapper. [F-6]

## 5. Coverage against the CLAUDE.md financial list (W1c)

| Item | Status |
|---|---|
| Normal transactions | Covered: win/rollback while disabled; G-1 normal |
| Duplicates | Partial: sequential replay exists (pre-existing); E2-while-disabled missing (C5) |
| Concurrency | Partial: identical bet/win/rollback, E3 race, E10 race; lock-position proof, 50 iterations, and disable-vs-bet missing (C1, C5) |
| Retries | Missing: 503 → re-enable → retry (C5) |
| Partial failure | Missing for tombstone + audit (C5) |
| Rollback | Covered: with a prior original (bet); unseen original while disabled. Missing: win rollback while disabled (C5) |
| Settlement | Covered: win on a disabled capability pays the bettor's wallet |
| Reconciliation | Missing: projection sweep after the scenarios (C5) |
| Provider callbacks | Covered for most §1.3 rows at HTTP level; S-nobet and E10-sequential missing |
| Idempotency | Partial: see C4 |
| Authorization | Partial: cross-tenant (existing, unedited) and brand-only positive exist; the brand-B/brand-A negative is missing (C5) |
| Auditability | Partial: capability before/after covered; E3 audit row not asserted (C2) |

## 6. Status labels

- **CAS-CAP-ROLLBACK-1 code:** financially approved. The deliverable is `PARTIALLY IMPLEMENTED` until C1–C6 and C8 are done.
- **G-1 code:** financially approved. The deliverable is `PARTIALLY IMPLEMENTED` until C3 is done.
- **Migration 0094:** approved. It is `IMPLEMENTED` subject to C6, because its integration tests have not run in this review.
- **ADR 0082 A6:** concurred, including the `postWin` extension. It stays `NOT IMPLEMENTED` on paper until C8.

---

## Re-verification after fix round A

- **Reviewer:** `ledger-finance`.
- **HEAD:** `e45831c` on `claude/focused-wright-jw88w9`.
- **Commits reviewed:** `5f98e23` (fix round A), `a94e610` (scheme-set routing, with no ledger effect), `e45831c` (evidence), and the ADR 0082 edit in `575f9c6`.
- **Executed:**
  - `go vet` passed for `internal/{casino,httpserver,ledger,providers}` under both the default and `integration` tags.
  - Unit tests passed for `internal/{casino,httpserver,ledger,providers,webhookauth}/...`. They ran with `TEST_DATABASE_URL`/`DATABASE_URL` unset, so no DB was touched.
- **Not executed:** integration tests, per instruction, because a CI replay is recreating the DB. Every integration-level claim below comes from reading the tests or from the implementer's attestation, and says which.
- **Evidence file provenance.** `w1c-mutation-kill.txt` names HEAD `a56f00d`. That commit is not on the branch; it is the pre-rebase twin of `a94e610` (same message). `git diff a56f00d a94e610` is empty, so the evidence applies to the code at HEAD.

### Verdict: **CONDITIONS MET for C1–C6, C8 and C10, and for both cleanup items. One new binding condition, C11, arises from the fix itself.**

- **A6** may move to `IMPLEMENTED` now. Its conditions are C1, C6 and C8, and all three are met.
- **W1c** moves to `IMPLEMENTED (MOCK provider only)` once C11 is met. C11 is small and test-only.
- **C7** stays with `security`.
- **C9** is W2b scope.

### Ruling on the disclosed replay-audit defect

**1. The fix is correct.**
- `ledger.Post` returns `AlreadyPosted = true` only when the stored transaction's canonical payload equals the request (Stage 10 F-7, `ledger.go:264-296`).
- A divergent redelivery errors instead (`ErrIdempotencyPayloadMismatch` or `ErrIdempotencyKeyReused` → 409 plus an integrity-alert log). The gate therefore cannot swallow a conflicting attempt.
- The original posting's audit row is written in the same DB transaction as the posting. So `AlreadyPosted` implies the fact already has exactly one committed audit row, and none is lost.
- `postWin` needs this gate because, unlike `postBet`, it has no idempotency short-circuit before `Post`. `postRollback`'s generic path deliberately lets a same-reference redelivery fall through to `Post`. The gate on the Post result is the right place in both.
- The fix has no ledger effect.
- The side effects are unchanged:
  - `postWinLockedBonus` still calls `AttributeGrantLedgerTransactionIdempotent` and `ResolveTerminalGrantCredit` unconditionally, and both are idempotent in their own right.
  - `postRollback`'s grant attribution and recheck were already gated on `!AlreadyPosted`.
- Cosmetic: the `"already_posted"` metadata key on these rows is now always `false`. Keep it for schema stability, or drop it later. It does not matter.

**2. It is consistent with CLAUDE.md auditability.**
- CLAUDE.md requires an audit record for every mutating financial action. A byte-identical redelivery that resolves to `AlreadyPosted` mutates nothing, and the fact it describes is already audited exactly once.
- One row per fact is the correct audit semantics and matches `postBet` (E2) and the tombstone replay (E9).
- The one intentional exception is the E3 per-attempt `casino_bet.rejected_tombstoned` row. That row records a rejection decision made on this delivery, not a posting, and my original review concurred with it. Rule: **postings are audited once per fact; rejections are audited once per verified attempt.**
- No durable redelivery record is required by CLAUDE.md, and I do not require one for W1c.
- Today a replay leaves only the generic `http_request` log line (method, path, status, duration, request id). It cannot be told apart from a first delivery without a join.
- **Recommendation R1 (non-blocking):** add a structured `Info` log such as `casino_callback_replayed` carrying callback type, `provider_id`, `provider_tx_id` and `ledger_transaction_id`.
  - Emit it for every replay short-circuit: the `AlreadyPosted` gate, `postBet`'s E2 short-circuit, the E9 tombstone short-circuit, and `postRollbackHeldWin`'s voided-by-same-reference short-circuit.
  - This needs a `Replayed bool` on `ReceiveCallbackResult`.
  - Any durable per-delivery record belongs in W2b's callback/rejection record, not in `audit_log`.

**3. Scope: complete for every reachable path.** I checked each redelivery audit site in `internal/casino`:

| Path | Replay behaviour | Audit on replay |
|---|---|---|
| `postBet` E2 (`orchestrator.go:984-993`) | Short-circuit under L0.1 before `Post` | none (correct) |
| `postBet` audit (`:1320`) | Ungated, but `AlreadyPosted` is unreachable there: L0.1 plus the E2 short-circuit | none reachable |
| `postWin` ×4 branches (`bonus_settlement.go:423,476,576,636`) | Gated (the fix) | none |
| `postRollback` generic inversion (`orchestrator.go:1733`) | Gated (the fix) | none |
| `postRollback` first tombstone (`:1569`) | Written once. A concurrent identical rollback serializes on L0.1(original) and then sees the tombstone row | once (asserted by the concurrent-identical rollback test) |
| **E9** tombstone replay or a second reference (`:1584-1596`) | Returns at the `TxTombstone` check, before any audit | none (correct) |
| `postRollbackHeldWin` (`bonus_settlement.go:826`) | Ungated, but a same-reference replay returns at the `VoidedByRollback` short-circuit before `Post`. The void commits atomically with the reversal, so `AlreadyPosted` is unreachable | none reachable |
| E3 (`orchestrator.go:1019`) | One row per verified attempt, by design | per attempt (intended) |
| E10, and `ErrAlreadyRolledBack` for a distinct second reference | Transaction rolls back or 409, log only | none (W2b, C9) |

- **R2 (non-blocking hygiene):** gate `postBet:1320` and `postRollbackHeldWin:826` on `!AlreadyPosted` as well. Both are unreachable today, but gating makes the one-row-per-fact rule structural rather than dependent on upstream short-circuits.
- **C9 extension (W2b):** an E9 *distinct second reference* to a tombstoned original is acknowledged 200 with no ledger, audit or log record of its own. W2b's rejection/callback record must capture it, together with E10 and the G-1 409 classes.
- **Outside casino, not verified, flagged to the owners:** `sportsbook/orchestrator.go:484` and `payments/orchestrator.go:796` use the same ungated `"already_posted"` audit pattern. Their owners should confirm that an upstream short-circuit makes `AlreadyPosted` unreachable at those sites.

**New binding condition from the fix:**
- **C11.** The `postRollback` generic-path audit gate has **no killing test**.
  - The only concurrent-identical rollback test exercises the tombstone path, which was already short-circuited before this fix.
  - No test counts `casino_bet.rolled_back` or `casino_win.rolled_back` rows after a redelivery. Removing the `!postResult.AlreadyPosted` guard at `orchestrator.go:1733` would survive the suite.
  - **Required:** add a test that posts a bet and rolls it back, then redelivers the identical rollback both sequentially and concurrently (N=8). It must assert:
    - exactly one reversal row;
    - the same `ledger_transaction_id` in every response;
    - **exactly one** `casino_bet.rolled_back` audit row.
  - Record a mutation kill for removing that guard. The direct-cash `postWin` guard is already killed by `TestPostWin_ConcurrentIdenticalWinCallbacks_PostsExactlyOnce`, which asserts one row.
  - **Recommended, not blocking:** a locked-branch replay case in `internal/casino/bonus_settlement_integration_test.go`. The locked branches are unreachable from real callbacks while `postBet` is cash-only (G-6).
  - Run it as `igaming_runtime` and append the run output to `evidence/`. That also closes the record gap noted under C6.

### Conditions

| # | Status | Basis |
|---|---|---|
| C1 | **Met** | Both race tests run `iterations = 50`. Iteration 0 of each holds the exact production L0.1 key externally (`casHoldProviderTxDeliveryLock`, same `hashtextextended` key) and asserts a real delivery queues on an advisory lock (`pg_stat_activity` `wait_event_type='Lock', wait_event='advisory'`). That is equivalent to my `pg_locks` not-granted wording. Mutation kills M1 (L0.1 removed from `postRollback`) and M2 (removed from `postWin`) are recorded with quoted failing assertions. **H1 (non-blocking hardening):** the poll is cluster-wide and unfiltered. CI runs `go test -tags=integration ./...` with packages in parallel against one DB, so another package's advisory wait could satisfy it spuriously. Filter on `datname = current_database()` and `pid <> <blocker pid>`, and ideally require ≥2 waiters, since both racers queue. |
| C2 | **Met** | `TestCasCapRollback1_E3_AuditRowAndBoundedEffect` asserts exactly one `casino_bet.rejected_tombstoned` row with `target_type = casino_provider_tx`, `target_id = mock-casino:<ref>` and `outcome = failure`. It also runs the six-point no-effect check with an audit delta of exactly 1. `TestCasinoWebhook_E10_SequentialWinAfterTombstone` checks the 409, the unmodified `AssertNoCasinoEffect`, that the bet is untouched, and the balance. M3 and M4 kill both checks. |
| C3 | **Met** | `_Replay` (same id, one row, balance unchanged), `_ConcurrentWins` (N=8, one row, one credit), `_RollbackOneThenWin` (credits the surviving bet's wallet), and `TestCasMultiBetWin_G1_HTTPLevel409Mappings` (wallet collision, mixed funding and ambiguous bare bonus, each seeded and delivered through the real webhook, with 409 plus `AssertNoCasinoEffect`). The mislabelled test is now `TestCasMultiBetWin_OrphanWin_MapsTo400NotG1`. M8 kills the G-1 special case. |
| C4 | **Met** | All three concurrent-identical tests assert one row, the same `ledger_transaction_id` across all 8 responses, and exactly one audit row (`casino_bet.posted`, `casino_win.posted`, `casino_rollback.tombstoned`). See C11 for the generic rollback path this did not reach. |
| C5 | **Met** | S-nobet (killed by M6); brand-B does not authorize brand-A (M5 note accepted: the positive brand-only test is the kill); capability disabled concurrently with an in-flight bet; 503 → re-enable → retry posts once; E2 replay while disabled returns the original; rollback of a posted win while disabled (balance −2500, one win row); ledger-vs-projection sweep `StatusClean` with zero mismatches. The partial-failure test injects the error **after** `ReceiveCallback` returns rather than literally between the tombstone and audit statements. That is accepted: both writes share the caller's one transaction and neither function commits, so every in-transaction failure point is equivalent. It asserts zero tombstone and zero audit rows after the failure, then exactly one of each after redelivery. |
| C6 | **Met on attestation** | I could not run it (DB off-limits). The implementer attests that the full W1c suites are green as `igaming_runtime` (NOBYPASSRLS), in the `5f98e23` message and the evidence file. Migration tests need the owner role for `CREATE DATABASE`, which is acceptable because they test DDL, not RLS-scoped access. CI's `TEST_DATABASE_URL` is the owner role `igaming`, so the running CI replay does **not** independently confirm C6. The record is a single line with no log. Closing C11 with a recorded runtime-role run covers this. |
| C7 | Not mine | `security`. |
| C8 | **Met** | The ADR 0082 §1.3 Stage 10.3 inventory table lists `postBet`/`postWin`/`postRollback` with L0.1 first, and its line numbers match `bc72fe4` exactly (checked: `:587, :913, :962, :1016, :1388, :1413, :1426, :1463, :1518, :1540, :1554, :1676`). The §2.1 L0.1 row note, the "As implemented" block and the corrected E3/E10 wording are all accurate. **Architect action:** flip "Status at gate 10.3-W1" to `IMPLEMENTED`, and update the Tests parenthetical ("the fix round raises them…") to past tense, citing `w1c-mutation-kill.txt` M1/M2. |
| C9 | Carried to W2b | Extended by the E9 distinct-reference item above. |
| C10 | **Met** | `writeCasinoCallbackError` now maps `ErrOriginalTombstoned` and the five G-1 abort classes to 409 with the same `errors.Is` set as the webhook mapper, with generic bodies. Two integration tests were added. |

**Cleanup items:**
- **E3 audit target: met.** `TargetType = "casino_provider_tx"` and `TargetID = "<provider>:<provider_tx_id>"`, and the E3 test asserts both. No `ledger_transaction` id is fabricated.
- **Tombstone correlation fallback: met and replay-safe.** The fallback is now `uuid.NewSHA1(OID, tenantID + ":" + "tombstone:<provider>:<ref>")` (`orchestrator.go:1784`), and the `RoundID` path is unchanged.
  - Replay safety: `TestReplay_CorrelationComparedExceptTombstone` (`ledger/replay_integration_test.go:238`) proves that a tombstone redelivered with a *different* correlation id returns `AlreadyPosted`. Pre-change tombstones therefore still replay cleanly under the new value.
  - The provider reference is still compared.
  - `correlation_id` has no uniqueness constraint, and every correlation-keyed casino read filters on `casino_bet` or is entry-based.
- **Dead `txs` map:** removed.

### Pre-existing observation (not introduced by W1c; no money effect; carry forward)

- **F-9 (Medium, follow-up item):** `postWin` has no `postBet`-style "already posted → verify match → return original" short-circuit before `resolveWinOrigin`. So a redelivery of a posted win does **not** return the original result in two cases, although by reading it never double-pays:
  - **Direct cash:** bet, then win, then rollback of the bet, then a win redelivery gives `ErrBetNotFound` → 400.
  - **Locked branches:** a win redelivery after the stake release appears to hit `ErrLockAlreadyReleased` → 409, or a payload mismatch, because `NetOutstandingLocked` is now 0.
- Both contradict §1.3 row E6 ("idempotent") for those sequences.
- **Fix:** after L0.1 and the E10 check, look up `(tenant, provider, provider_tx_id)` for a `casino_win`, verify it matches the event, and return the original. This would also make the new audit gate structural.
- Must be resolved before bonus-funded or locked casino stakes (G-6) ship. It is not a W1c blocker.

### Status labels after this re-verification

| Item | Status |
|---|---|
| **ADR 0082 A6** | `IMPLEMENTED` (C1, C6, C8 met). Architect to edit the status line. |
| **Migration 0094** | `IMPLEMENTED`, subject to C6's attested runtime-role run |
| **CAS-CAP-ROLLBACK-1 and G-1 (W1c)** | `PARTIALLY IMPLEMENTED`. Becomes `IMPLEMENTED (MOCK provider only)` once C11 lands. It is `MOCK` because the only registered casino provider is `mock-casino`, and no real aggregator integration exists. |
