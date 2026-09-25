# Stage 10 W1 + F-7 — independent code review

| Field | Value |
|---|---|
| Reviewer | `code-reviewer` (independent of the implementers) |
| Date | 2026-09-25 |
| Scope | `git diff 94bc863..f72d864`: eb3912f (core), d26b3b9 (runtime role), 49d6fda (risk tests), 38e4875 (frontend), 0d943fd (docs), 36616f1 (F-7 + QA suite), f72d864 (HTTP route, read surfaces, reconciliation stream, mock statement source) |
| Binding contract | ADR 0088; ADR 0020 Amendment 2026-09-25 (F-7) |
| Mode | Review only. No code was changed. |

Note: the "uncommitted working tree" named in the review request was committed as
`f72d864` while this review was running. `git status` is clean at `f72d864`, and that
commit is what was reviewed.

## Verdict

**APPROVE WITH CONDITIONS. Not ready for Stage 10 sign-off until B-1, B-2 and B-3 are closed.**

- No P0 or P1 correctness bug was found in the settlement lifecycle, the ledger postings,
  the lock order or the F-7 semantics. Money handling follows CLAUDE.md:
  - no float;
  - no balance `UPDATE`;
  - idempotency is the DB `UNIQUE (tenant_id, idempotency_key)`;
  - rollbacks are compensating inverse entries;
  - NUMERIC values are scanned into `big.Int`.
- The blocking items are test and evidence gaps against ADR 0088 §14 and CLAUDE.md's
  "auditability" test requirement. They are not code defects.
- One P2 robustness defect in migration 0091's T-1 was **confirmed empirically**
  (finding 1). It fails closed, so no money is at risk. Its fix is recommended before any
  second settlement driver exists (a batch or provider driver in W2+).
- Financial and security correctness remain for `ledger-finance` and `security` to sign
  off. This review does not adjudicate their vetoes.

### Blocking items for Stage 10 sign-off

| ID | Finding | Owner |
|---|---|---|
| B-1 | Finding 1 (test part): T-1's void-causation branch has no DB-level test in either direction | `qa` |
| B-2 | Finding 2: ADR 0088 §14 mutation pass and SQL trigger branch-coverage checklist are not evidenced | `qa` |
| B-3 | Finding 3: rejection audit (§4.4) and the separate-transaction integrity audit (§4.7) are untested | `qa` / `backend` |

---

## Findings (most severe first)

### 1. P2: T-1's composed-void "same transaction" rule is savepoint-fragile, and the negative branch is untested

`migrations/0091_sportsbook_settlement.up.sql:244-255`

```sql
SELECT xmin::text INTO cause_xmin FROM sportsbook_bet_settlements WHERE id = NEW.causation_record_id;
... OR cause_xmin <> (pg_current_xact_id()::text::bigint % 4294967296)::text THEN RAISE ...
```

`pg_current_xact_id()` always returns the top-level xid. A row inserted inside a
subtransaction carries the subtransaction's xid as `xmin`, and it keeps that xid after
`RELEASE SAVEPOINT`.

**Reproduced on the CI database (PostgreSQL 16.13), inside a transaction that was then rolled back:**

- The rollback history row was inserted inside `SAVEPOINT sp; ... RELEASE SAVEPOINT sp`.
  Its `xmin` was 1343701, while the top-level xid was 1343697.
- The composed void was inserted in the same transaction, citing that row.
- Result: `ERROR: a void may cite only a rollback of this bet inserted by the same transaction`.

**Status today:**

- **Not reachable in practice.** `db.Pool.WithTenant` passes a top-level `pgx.Tx`.
  `insertSettlementRecord` (`internal/sportsbook/settlement.go:984`) runs a plain
  `INSERT` on it.
- `ledger.Post` uses `db.IdempotentInsert`'s savepoint for the `ledger_transactions`
  row only, never for history rows.
- The current path therefore works. The composed-void scenario tests
  (`TestSettlementScenario_VoidAfterSettlement_Won/_Lost`) pass, which exercises the
  positive branch.

**When it would break:** any caller that runs `SimulateSettlementEvent` inside a pgx
nested transaction would hit this. Examples:
- a future batch driver using one savepoint per bet (§5.1 anticipates one);
- a provider-webhook driver that wraps the operation;
- a test harness.

Every legitimate void-after-settlement would then abort as `ErrSettlementIntegrity` and
return 409. The effect is fail-closed (no money moves), but it is an availability defect.

The trigger comment says "history rows are never inserted inside a savepoint". Nothing
enforces that precondition.

**Also:** no test exercises the rule's reject branch, i.e. a void citing a rollback that
an earlier transaction committed. `TestSettlementScenario_RollbackThenVoid`
(`settlement_scenarios_integration_test.go:379-381`) only asserts that the Go code does
not set `causation_record_id`. The trigger is never invoked with one, yet the failure
message cites "T-1's xmin rule". Against T-1, that test is vacuous. I ran the reject case
by hand, and T-1 does reject it correctly. The case is simply not pinned by any test.

**Fix:**

1. **Test (blocking, B-1).** Add tests in `settlement_db_constraints_integration_test.go`:
   - a void citing an earlier-committed rollback is rejected;
   - a same-transaction void citing its rollback is accepted;
   - the savepoint case, pinned either as rejected (documenting the precondition) or as
     accepted (after fix (b)).
2. **Robustness (non-blocking; do it before any second driver).** Replace the xmin
   equality with a check that is true for any xid in the current transaction tree:
   `pg_xact_status(<epoch-qualified xmin>::xid8) = 'in progress'`. Rows of other
   in-progress transactions are invisible to this snapshot, so a visible row whose xmin
   is "in progress" belongs to this transaction or one of its subtransactions. I verified
   on PG 16 that a released-savepoint row reports `in progress` and an earlier row reports
   `committed`. Handle the epoch boundary: when `xmin` > the current low 32 bits, use
   epoch − 1.
3. **Alternative to (2).** Keep the xmin check, and document the "no savepoint"
   precondition on `SimulateSettlementEvent`'s doc comment and in ADR 0088 §3.3.

### 2. P2 (blocking, B-2): ADR 0088 §14 mutation pass and SQL branch-coverage checklist are not evidenced

ADR 0088 §14 makes the following binding:

- a Go mutation pass over:
  - V-1…V-5;
  - the §4.3 decision tables;
  - the `NetLocked` derivation;
  - `LockProjectionsForPostings`;
  - lock call order;
- "a recorded manual branch-coverage checklist (every CHECK and every trigger IF branch
  exercised both true and false)".

Neither record exists under `docs/governance/`. The only Stage 10 governance file is the
F-7 audit. Finding 1 shows the gap is real: at least one T-1 IF branch is exercised on one
side only.

**Fix:** `qa` produces both records against `f72d864`, or later. Each T-1/T-2 branch
maps to a named test. Every untested branch either gets a test or is listed as a
recorded, accepted gap.

### 3. P2 (blocking, B-3): rejection audit (§4.4) and the §4.7 separate-transaction audit are untested

- `rejectSettlement` (`settlement.go:1044`) is designed to write
  `sportsbook_bet.settlement_rejected` and have it **commit** with no ledger write.
- On `ErrSettlementIntegrity`, the handler (`internal/httpserver/sportsbook_settlement_handlers.go`,
  the `errors.Is(err, sportsbook.ErrSettlementIntegrity)` branch) writes the rejection
  audit in a **second** `WithTenant` transaction and returns `409 SETTLEMENT_INTEGRITY`.

A search of every `_test.go` for `settlement_rejected`, `RecordSettlementRejection`,
`SETTLEMENT_INTEGRITY` or `CodeSettlementIntegrity` finds **no matches**. The consequences:

- No test proves a rejection's audit row survives commit.
- No test proves that an integrity abort leaves exactly one rejection audit row, and zero
  ledger, history or status rows.
- No test proves the HTTP 409 mapping for the integrity path.

CLAUDE.md requires auditability tests for financial functionality.

**Concrete failure this would not catch:** a refactor makes `rejectSettlement` return an
error, or the handler reuses the failed `tx` for the audit. The audit would then be
silently rolled back, and every existing test would still pass.

**Fix:**

- Service level: extend `TestSettlementDecision_PayloadMismatch_Settle` (or add a test) to
  assert one committed `sportsbook_bet.settlement_rejected` row with
  `outcome=failure, rejection_code=SETTLEMENT_PAYLOAD_MISMATCH`.
- HTTP level: add a test that drives an integrity abort. The `LedgerKeyBackstop` fixture
  can be reused through the route. Assert:
  - 409 `SETTLEMENT_INTEGRITY`;
  - one rejection audit row committed;
  - no history rows;
  - bet still `open`.

### 4. P3: the ledger-key backstop test's premise is stale after F-7; two `lockAndPost` branches are untested

`internal/sportsbook/settlement_fault_injection_integration_test.go:90-101`

The test comment says `ledger.Post` "reports AlreadyPosted (F-7: replay compares only
transaction_type…)". After 36616f1, the fabricated posting (Dr HOUSE 1 / Cr CASH 1)
differs in entries, so `Post` now returns `ErrIdempotencyPayloadMismatch`. The test still
passes, but through the F-7 branch that was added in `lockAndPost` (`settlement.go:928`).

Two `lockAndPost` branches are exercised by no test:
- `res.AlreadyPosted` (`settlement.go:934`);
- `ErrIdempotencyKeyReused` (`settlement.go:916`).

Both are reachable. Fabricate a `tombstone` under `sportsbook_settlement:<bet>#1` with no
history row. Then:
- `rollback(1)` → the decision table says NEW tombstone → `Post` returns `AlreadyPosted`
  (tombstone correlation is exempt, and there are no entries);
- `settle(1)` → `ErrIdempotencyKeyReused`.

**Fix:**
- Update the comment.
- Add the two tombstone-fixture cases, each asserting `ErrSettlementIntegrity` and zero
  rows written.

### 5. P3: `ErrSettlementTombstoned` is referenced but does not exist; the ADR contradicts itself

- `internal/ledger/ledger.go:174-175` says "ADR 0088 §4.5 maps it to ErrSettlementTombstoned".
- ADR 0088 §4.3 and §4.5 also name a typed `ErrSettlementTombstoned`.
- No such error exists. The ledger-key backstop maps `ErrIdempotencyKeyReused` to
  `ErrSettlementIntegrity`, per §4.7.

The code follows §4.7, which is the safer reading: abort the transaction. The doc comment
and the ADR text are wrong.

**Fix:**
- Correct the `ledger.go` comment.
- Record a one-line ADR 0088 clarification: the tombstone case is a §4.3 rejection code
  (`SETTLEMENT_TOMBSTONED`), and the ledger backstop is §4.7 `ErrSettlementIntegrity`.

### 6. P3: the unknown-bet alert name deviates from §4.6

`sportsbook_settlement_handlers.go:183` builds the alert name by lower-casing the
rejection code. For `NOT_FOUND` this emits `sportsbook_settlement_integrity_alert_not_found`.
ADR 0088 §4.6 names `sportsbook_settlement_integrity_alert_bet_not_found`. An alert rule
written against the ADR name would never fire. No test asserts any alert event name.

**Fix:** special-case `NOT_FOUND` → `bet_not_found`, or amend §4.6. Add a log-capture
assertion for at least one alert.

### 7. P3: house-account resolution runs before L2 (ADR 0088 §5.1 orders L2 first)

`deriveBetLedgerAccounts` (`settlement.go:533`) calls `ledger.GetOrCreateAccounts` for
HOUSE inside `loadSettlementState`. That happens before `buildRollbackInput` takes L2
(`settlement.go:836`). §5.1 lists step 3 (L2) before step 4 (HOUSE resolution).

No cycle is possible. L2 (`FOR UPDATE` on a sportsbook settlement `ledger_transactions`
row) is only ever taken by a holder of the same bet's L1, so a transaction waiting on a
`ledger_accounts` unique-index insert can never hold L2 for this bet. This is a documentation
deviation, not a deadlock.

**Fix:** add a sentence to §5.1 or Amendment A4 stating that HOUSE resolution may precede
L2. Alternatively, reorder the code, which costs a second pass.

### 8. P3: decoder leniency on the test-support route

`sportsbook_settlement_handlers.go:134` (`parseClaimPayoutAmount`) and the `json.Number`
field:
- `encoding/json` accepts a quoted numeric string (`"payout_amount": "2500"`) into
  `json.Number`, so V-5's "parsed as a JSON integer" is not strictly enforced.
- `"generation": 0` on a void cannot be told apart from an absent field, so it is accepted
  although §9.2 says the field is forbidden.

Neither case allows a value that is later posted. The payout claim is only compared with
`potential_return`.

The doc comment at lines 34-43 is also garbled ("would let encoding/json silently truncate
... is untrue").

**Fix:**
- Decode `generation` as `*int` and reject when present on void.
- Reject a `payout_amount` token that starts with `"` (use a `json.RawMessage` check), or
  record the leniency in §9.2.
- Rewrite the comment.

### 9. P3: simplification in the reconciliation stream

`internal/reconciliation/sportsbook_settlement.go:480-494` versus `:842-855`.

The stream derives "the ledger payout of a settlement" two different ways:
- `sbLoadCurrentSettlements` joins credits in the settlement transaction to accounts that
  were *debited by the bet's placement* (`JOIN ledger_entries pe … pe.direction='debit'`);
- `sbLedgerStatementView` sums credits to any `player_cash` account in the settlement
  transaction.

Both are correct on well-formed data, but they can disagree on malformed data, which makes
the mismatch evidence confusing.

**Fix:** use one helper, the placement-CASH-scoped one, because it is stricter.

The stream also rescans the tenant's whole sportsbook population, including a
`LIKE 'sportsbook\_settlement:%'` scan over `ledger_transactions`, on every hourly run.
That is acceptable for W1 MOCK volumes. Record it as scale debt next to ADR 0088 §8.

### 10. P3: the down-migration tests do not assert state after a refusal

`settlement_migration_0091_integration_test.go:68-150` asserts only the error text.

Check 1 drops `ledger_transactions_transaction_type_check` *before* its `DO` block. If
the refusal were not atomic, the ledger would be left with no type CHECK at all. I verified
that it is atomic: `db.Pool.MigrateDown` (`internal/db/migrate.go:377-391`) runs each
down-file in one transaction and rolls back on error. But no test pins this.

**Fix:** after each refusal, assert that:
- `schema_migrations` still holds 91;
- the 20-value CHECK is present;
- `sportsbook_bet_settlements` still exists.

---

## Verified correct (no finding)

- **Postings (§2.3).** Settle lost, settle won (two balanced pairs, full payout), void-before,
  rollback (exact inverse loaded from `ledger_entries`, `reverses_transaction_id` set) and
  void-after-settlement (rollback, then a before-shape void, one DB transaction) all match
  the ADR. The tombstone has no entries and uses the settlement key.
- **Payout (§2.4).** The payout is derived from `potential_return` and is never taken from
  the request. V-1…V-4 are enforced in Go (`settlement.go:602-616`) and again by T-1
  (`up.sql:191-198`).
- **Decision tables (§4.3), row by row.**
  - `settleBet`: row order is payload compare → tombstone → void → settled → `G+1` →
    validation. `BET_ALREADY_SETTLED` for any g on a settled bet is equivalent to the ADR's
    "(g > G)": for g ≤ G a settlement or tombstone row always exists, because generations
    are contiguous.
  - `rollbackBet`: replay-of-rollback, replay-of-tombstone, current-g → post, open ∧
    g = G+1 → tombstone, void → `BET_VOIDED`, else out-of-sequence.
  - `voidBet`: void row → compare `void_reason`; open → void-before; settled → composed.
- **Causation (§2.1).** Re-settlement cites the g−1 rollback or tombstone row and its
  ledger transaction. The composed void's ledger `CausationID` is set in `lockAndPost` after
  pre-lock, which does not change the entry set. A standalone rollback-then-void carries nil.
- **Lock order.** L1 bet `FOR UPDATE` → (L2 settlement transaction `FOR UPDATE`) → L3
  `LockProjectionsForPostings` over the union of every posting → L4 `Post` in order →
  E-4 history row, status `UPDATE`, audit. INV-LOCK-E4 holds: the only writers are
  `insertSettlementRecord` and `updateBetStatus`, both reached only after `lockAndPost`.
  The static sole-writer test is not vacuous: it parses the AST and folds string
  concatenation.
- **`LockProjectionsForPostings`.** Unions `prepareEntries` over every input, sorts
  canonically, locks once, and rejects mixed tenants. R3 holds because the same `ins` are
  then posted unchanged apart from the `CausationID` header.
- **Migration 0091.**
  - T-2 derives status exactly as the Go code, the reconciliation stream and the read path
    do.
  - The partial unique indexes back up INV-LOCK-E4.
  - The shape CHECKs match §3.2.
  - The RLS policies match security S4 (the player-scoped INSERT is rejected, and tested).
  - The guarded REVOKE is correct.
- **Down migration.** Every check uses constraint validation, which FORCE RLS cannot
  filter, and every check runs before any drop. The runner makes the down-migration atomic.
- **F-7 (36616f1).**
  - The type check runs first and keeps `ErrIdempotencyKeyReused`.
  - The canonical comparison covers the entry multiset after the B2 legs (decimal-string
    amounts, never netted), the reversal link, the provider reference, the reason code,
    causation and correlation.
  - The field names in the error carry no values.
  - The tombstone correlation exemption is justified: tombstones carry no value, the key
    and provider reference are still compared, and historical casino/payments tombstones
    carry per-call `uuid.New()` correlations that no code change can make deterministic.
  - The `PlaceBet` concurrent-duplicate race (per-attempt `betID` correlation) is resolved
    by `resolveConcurrentDuplicateBet` with a key-owner check.
  - The payments same-reference reversal redelivery now correctly reaches `Post`.
  - The QA bugfix in `lockAndPost` (`ErrIdempotencyPayloadMismatch` →
    `ErrSettlementIntegrity`) is correct and necessary.
  - I spot-checked the correlation and causation determinism of all `ledger.Post` call
    sites. I did not re-derive all 21 audit rows independently.
- **Reconciliation (§8).**
  - All sums are NUMERIC → text → `big.Int`.
  - Locked sums are scoped to the four sportsbook types (the casino shares
    `player_locked_cash`).
  - `player_locked_bonus` is checked as a sportsbook-scoped zero.
  - The two-way orphan check includes sportsbook tombstones (the `\_` escape is correct
    under `standard_conforming_strings`).
  - Causation and reversal-link consistency are checked.
  - The status matches the history-derived status.
  - The MOCK statement is labelled everywhere, and a nil source fails the run closed.
  - The sportsbook stream runs in its own transaction after `ledger_vs_projection`.
  - Drift injection uses a scratch database, so the `NO FORCE` toggle cannot leak into the
    shared CI database.
- **Handler.**
  - Registered only when both flags are set.
  - Middleware chain per §9.1; the subject is UUID-parsed and fails closed.
  - The actor is re-checked as active staff in the tenant inside the transaction.
  - The bet is RLS-resolved (cross-tenant → identical 404).
  - The status map matches §9.3 (400/404/409/422).
  - Alerts carry only the allow-listed fields.
  - Audit IP comes from `trustedProxyClientIP`; the raw `RemoteAddr` goes to metadata only.
- **Read surfaces.** History is batch-loaded (no N+1). The player surface omits the staff
  and request ids, and a test covers this. Composed-void rows share `created_at` but are
  ordered correctly by `generation NULLS LAST`.

## Tests run by this reviewer (targeted; no full suite)

- `go build ./...` and `go vet -tags=integration` on sportsbook, reconciliation,
  httpserver and ledger: clean.
- `internal/sportsbook`, `-run 'TestSettlementScenario_VoidAfterSettlement|TestSettlementScenario_RollbackThenVoid|TestDBConstraints_T1_CausationRules|TestSettlementFaultInjection|TestSettlementConcurrency'`: ok.
- `internal/reconciliation`, `-run TestSportsbookSettlementRecon`: ok.
- `internal/httpserver`, `-run 'TestSettlementSimulate|TestListMyBets_PlayerSurface|TestListAdminBets_Lifecycle'`: ok.
- `internal/ledger`, `-run 'Replay|F7'`: ok.
- Manual SQL probes against the CI database, each in a transaction that was rolled back:
  - T-1 rejects a void citing an earlier-committed rollback;
  - T-1 rejects a same-transaction composed void when the rollback row was inserted under
    a released savepoint (finding 1);
  - `pg_xact_status` correctly classifies released-savepoint rows as `in progress`.

## Adjacent item surfaced (not a W1 finding; must stay tracked)

The F-7 audit (`docs/governance/stage-10-f7-ledger-replay-audit.md`, final item 5)
records a **P1 candidate** that predates W1:
- payments `receiveDepositReversalCallback` takes no lock on the original deposit;
- concurrent reversals under *distinct* references each post, driving `player_cash`
  negative.

36616f1 neither fixes nor worsens this. It must be carried as its own item, with
`ledger-finance` and ADR 0082 lock-order review, before any real payments provider goes
live. It does not block W1 sign-off.
