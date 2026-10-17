# SRF2R closure record: provider-independent items from the F2 security re-review

Worktree `.claude/worktrees/srf2r`, branch `srf2r-closure`, based on `76187cc` (migration head 0170). Date 2026-10-17. Author:
`ledger-finance`.

Scope: the items in section SRF2R.2 of `docs/plans/f2-payments-recon-integrity-record.md`, plus SRF2-6.
- Everything is against the local database and MOCK providers. Nothing here claims real-PSP behaviour, a vendor contract or a licence
  position.
- Not edited (single writer: the orchestrator): `docs/governance/*`, HANDOVER, progress, active-stage, ADRs. Proposed registry text is in
  section 7.
- Migration: **0172 is used**, authorised by the orchestrator on 2026-10-17. 0171 belongs to the F4 agent and is NOT in this branch, so on
  this branch alone `migrate verify` reports a gap at 0171 (see section 6). The gap closes when the F4 branch merges.

## 1. Deliverables and labels

| # | Item | Label |
|---|---|---|
| SRF2R-1 | The sealed cursor read, the M2 evidence pre-read and the new stall progress read each stream their candidates through ONE server-side cursor (`db.ServerCursor`: `DECLARE ... NO SCROLL CURSOR` / `FETCH 512`), executed once. Before this, each page re-issued a keyset query that could not use an index. Each read now has a per-read scan budget (`CursorScanBudget` / `EvidenceScanBudget`, 50 000 rows each). Past the budget the outcome is explicit and alertable: P1 `cursor_flood` for the stream, closed reason `evidence_scan_budget_exceeded` for the gate. It never trusts anything, never fetches, and the next run retries. Request now pre-reads only the attempt's provider. Migration 0172 adds two keyset indexes that exactly match the streamed orders. | IMPLEMENTED (MOCK). The Decide pre-read is not narrowed (see 2.1 (d)), so this part is PARTIALLY IMPLEMENTED |
| SRF2R-2 | `match_stalled` is now evaluated at the END of each sweep, after the sweep's own match. An import counts as stalled only when no run of this source committed after the import's `fetched_at`; with keys, that run's run seal must verify. When the rule still finds a stalled import, the check waits for the stream's advisory lock to be released, at most 15 s, by polling `pg_locks` without ever taking the lock, and then evaluates once more. A continuously held lock still trips the alert. | IMPLEMENTED (MOCK) |
| SRF2R-3 / 6.10 | Migration 0172: `payment_ref_cleared` is tombstone-only, so the SQL MA020 guard fails closed. No reconciliation-to-ledger write path was added. The case where a reversal appears only in a statement is an accepted operational cost and is documented in the runbook. | IMPLEMENTED |
| SRF2-6 | Staging has no import keys, so its MOCK stream takes the non-sealed (A11) path. | PROVIDER DEPENDENT / infrastructure (AWS secrets for staging). Classified only, not built (2.4) |

## 2. Findings and exact decisions

### 2.1 SRF2R-1: flood cost

**Root cause (confirmed by reading the code).**
- The cursor read paged with `(coverage_end, run_at, id) < ($4, $5, $6) ORDER BY coverage_end DESC, run_at DESC, id DESC LIMIT 512`.
- The only matching index, 0168 `(tenant_id, provider_id, coverage_end DESC)`, cannot evaluate that row comparison as an index condition.
- So every page scanned and sorted the whole flood again: O(n²/512). 200 000 rows means about 390 pages, each touching up to 200 000 rows.
- The M2 gate had the same shape over `(run_at, id)`.

**Decisions.**
- (a) **One streaming read per call.** Each read declares one server-side cursor inside the read's existing transaction (read-only, or the REPEATABLE READ match snapshot) and fetches batches of 512. Other statements, such as the import seal check, run on the same transaction between fetches. The query is planned and executed once.
  - Helper: `internal/db/server_cursor.go`. The cursor name is a fixed identifier, checked by regexp.
- (b) **Budget.** At most `CursorScanBudget` candidate rows are scanned per read. The check happens after each full batch, so the overshoot is at most one batch (511 rows).
  - Stream: the cursor read returns `ErrPaymentCursorScanBudgetExceeded`. `ReconcilePaymentStatementForTenant`'s `fail()` maps it to phase `cursor_flood`. That phase gets the audit row `reconciliation.sweep_run_failed` and the durable P1 `reconciliation.run_failed` with attribute `phase=cursor_flood`.
  - In phase 0 the run fails before the fetch: nothing is fetched or stored.
  - In the match re-read under the advisory lock (a flood planted mid-sweep), the match transaction rolls back. The import stays an orphan, no run is committed, and the cursor does not move.
  - There is no partial or zero cursor in any case (no fail-open). The next run retries. While the flood stands, every run fails loud. That is the trade-off SRF2R-1 recommended: a loud stop instead of a quiet 70-second hang. Recovery is the owner-authorised exclusion procedure (residual 6.11).
  - Gate: `newestSealedRun` returns `budgetHit`. The verdict is then `evidence_scan_budget_exceeded`, a closed reason under the existing 409 `force_resolve_precondition_failed`, and it is logged at Error. M2 is refused at submission and at execution.
- (c) **Keyset indexes in 0172.**
  - `reconciliation_runs_payment_sealed_keyset (tenant_id, provider_id, coverage_end DESC, run_at DESC, id DESC) WHERE stream='payment_statement' AND run_seal IS NOT NULL`;
  - `reconciliation_runs_payment_sealed_by_run_at (tenant_id, provider_id, run_at DESC, id DESC)` with the same predicate.
  - With these, the streamed order comes straight from the index, so the budget bounds the work itself, not only the rows handed to Go.
  - The 0168 index is kept. It is a prefix of the first one; dropping it is a possible later clean-up, not done here.
- (d) **Narrowing.**
  - `Request` pre-reads only `in.AttemptID`'s provider.
  - `Decide` cannot narrow. 0115's RLS hides a pending resolution from the system session the pre-read uses (`tenant_system_read_executed` shows only executed M2 rows), and opening the caller's own session just for this would add a second session shape.
  - So `Decide` still reads every provider of the tenant, each bounded by the budget.
  - A provider not pre-read is never current (`no_successful_run`), so narrowing can only refuse, never admit.

**Not changed:** `anomalyRows` (one `count(*)` plus at most 1000 rows, already bounded) and the A11 no-keys rule (a single `max()`).

### 2.2 SRF2R-2: match_stalled

**The rule.** An import of this source counts as stalled when all of these hold:
- its `coverage_end` is above the trusted cursor read at the start of this sweep;
- it is sealed (when the cursor is read sealed);
- `fetched_at < now() - MatchStallAfter`;
- NO run committed after its `fetched_at`. Here a run counts only if:
  - it has `run_seal IS NOT NULL` and `run_at` in `(fetched_at, now()]`;
  - its run seal verifies in Go;
  - it is joined to an import of THIS source (`imported_by_service`, `source_label`, `is_mock`).
  - Without keys (non-production only), any run row backed by an import of this source counts, like the A11 rule.

Only the run seal is checked, not the import seal. Only the application can write a verifying run seal, and a run seal is never written without a match. Forged or copied rows never verify, so they cannot suppress a stall.

**The progress read** uses the same streaming cursor and budget. If the budget is hit, nothing counts as progress and the stall is raised. The audit row then carries `progress_scan_budget_exceeded`.

**Placement.** The check moved from phase 0 to a deferred step that runs at the end of the sweep on every path after a successful cursor read. That is the reviewer's case: the next sweep's own run re-covers the window. An error in the stall read fails the run with phase `match_stall_check`, but only if the run had no error already.

**In-flight winner.** A contention loser checks while the winning instance is still inside its match transaction.
- At that moment the rule cannot yet see the winner's run.
- So when the rule says "stalled", the check polls `pg_locks` (every 50 ms, at most `matchStallLockWait` = 15 s) until the stream's advisory lock is free, then evaluates once more.
- It never takes the lock. Taking it would make another instance's `pg_try_advisory_xact_lock` fail and create a new orphan.
- A continuously held lock times out, then raises. The audit row records `lock_still_held: true`.

**Cost.** Under attack, each sweep of a pair waits up to 15 s. Normally the cost is one small query.

### 2.3 SRF2R-3: MA020 SQL guard

0172 `CREATE OR REPLACE FUNCTION payment_ref_cleared(uuid, text, text)`:
- The new body is only the tombstone `EXISTS` from 0119; the `deposit_reversal` statement-line branch is removed.
- Signature, STABLE, pinned `search_path`, not SECURITY DEFINER, and owner are unchanged, so the callers (`payment_attempt_open_exposure`, `player_open_payment_exposure`, 0115's refusal, the `adjustment/execute.go` audit read) are unchanged.
- Down restores the exact 0119 body (byte-identical, checked with `diff`; the round-trip test compares the normalised body) and drops the two indexes. 0172 stores nothing, so the down migration loses nothing.

Why this is safe:
- A ledger tombstone is written only by the ledger posting path. It is keyed and idempotent, and the ledger is append-only.
- A verified deposit-reversal callback for a never-posted original already writes exactly that tombstone (`payments.applyReversalReceiptEvidence` → `postDepositReversalTombstone`).

**Intentional behaviour change (accepted operational cost, orchestrator decision).**
- A reversal reported ONLY in a statement no longer clears the SQL guard. Manual credits for that player stay refused (MA020) until a tombstone exists.
- The Go detective rule (`clearedRef`) still clears its standing finding on an eligible completed line (sealed, with keys). After 0172: SQL-cleared ⊆ Go-cleared.
- Operator remedy: ask the PSP to re-deliver the reversal callback, whose verified handling writes the tombstone. Written into `operational-runbooks.md` (MA020 paragraph).
- No reconciliation-to-ledger write path was added (orchestrator instruction).

### 2.4 SRF2-6: classification only

Staging runs without `PAYOUT_INSTRUMENT_KEYS`, so its stream uses the A11 cursor and the F2e gate admits unsealed evidence. Production cannot reach that mode (`VerifyPaymentCursorSealing`, `GuardEnvironment`).
- Remedy (not built here): provision synthetic import keys in the staging secret store and consider running `VerifyPaymentCursorSealing` in staging too.
- Label: PROVIDER DEPENDENT / infrastructure (AWS Secrets Manager / Terraform staging stack, which is outside this provider-independent closure).
- Keep it as a LOW open row, binding before staging acceptance of the first real statement source.

## 3. Ledger-finance invariants

- No ledger table, posting path or wallet code changed.
  - 0172 replaces a STABLE read function and adds two indexes on `reconciliation_runs`.
  - The Go changes are reads plus the existing failure, audit and alert paths.
  - No balance UPDATE, no new write path, no historical-row mutation, no floating point.
- The MA020 change only adds refusals (fail closed).
  - The adjustment tests run `w.assertInvariants()` (SUM(debits) = SUM(credits), projection = ledger).
  - The planted-line probe asserts that no adjustment posting appears.
- Idempotency is unchanged. The budget failure is a run failure. A retry repeats the same failure with no side effect except the audit row and the alert occurrence (one alert row per pair, by discriminator).

## 4. Tests

New tests, all integration, private DB:

| Test | Covers |
|---|---|
| `TestSRF2R1_StreamingCursorReadCrossesBatchesUnderAFlood` | normal: 1543 forged rows over 4 fetch batches; cursor unchanged; each row scanned once; next genuine run is the first row scanned |
| `TestSRF2R1_FloodPastTheBudgetFailsLoudAsCursorFloodAndFetchesNothing` | budget, retry (two attempts), loud (audit phase `cursor_flood`, one P1 alert), no fetch, no store, cursor unchanged, recovery |
| `TestSRF2R1_FloodDuringTheSweepFailsTheMatchAndCommitsNoRun` | partial failure: flood planted during the fetch; match re-read hits the budget; no run; next sweep re-covers from the genuine cursor |
| `TestSRF2R1_ConcurrentCursorReadsUnderAFlood` | concurrency: 6 parallel reads agree inside the budget and all fail typed past it |
| `TestSRF2R2_BenignContentionLoserIsNotAStall` | the reviewer's probe: 0 `payment_match_stalled` audits and no alert; duplicate sweep also 0 |
| `TestSRF2R2_LoserWaitsForTheWinnersCommitInsteadOfRaising` | concurrency: winner paused in its match (hook) while the loser checks; the loser waits and raises nothing |
| `TestSRF2R2_ContinuouslyHeldLockStillTrips` | a held lock raises exactly one stall per sweep (`lock_still_held=true`, P1 alert); recovery raises nothing new |
| `TestSRF2R2_ForgedRunsAreNotProgressAndTheBudgetFailsClosed` | 1100 forged rows after the fetch are not progress; past the budget the stall is raised; a genuine run is progress |
| `TestMigration0172_UpDownRoundTrip` | 0172 up (body, shape, indexes), down (exact 0119 body, indexes dropped), re-up |
| `TestSRF2R1_M2GateStreamsTheFloodAndRefusesPastTheBudget` (payments) | gate: flood inside the budget admits; concurrent pre-reads agree; past the budget refuses with `evidence_scan_budget_exceeded` (retry: still refused); budget restored admits |
| `TestSRF2R1_M2PreReadIsNarrowedToTheAttemptsProvider` (payments) | narrowing; an unknown attempt reads nothing and is never current |
| `TestMA020_K2_ReversalLineNoLongerClears_OnlyATombstoneDoes` (adjustment) | replaces `TestMA020_K2_ReversalLineClearsInTenantSessionOnly` (see 5) |
| `TestMA020_SRF2R3_PlantedUnsealedReversalLineDoesNotOpenTheGuard` (adjustment) | the SRF2R-3 attack as `igaming_runtime` (asserted with `current_user`): unsealed non-MOCK import with a succeeded or reversed line; MA020 in the tenant and acting sessions; nothing posted; invariants hold |

Mutants. Each was applied, run, then reverted.

| Mutant | Result |
|---|---|
| 0172 down applied to the private DB | `TestMA020_SRF2R3_*`, `TestMA020_K2_ReversalLineNoLongerClears_*` and 5 of the 6 `TestMA020_Parity_*` FAIL (the reason-pin test has no line fixtures) |
| progress ignored in `evaluateMatchStall` | all 4 `TestSRF2R2_*` FAIL |
| no lock wait | `TestSRF2R2_LoserWaits*` FAILS |
| budget checks removed | 3 of 4 `TestSRF2R1_*` stream tests FAIL (the batch-crossing test runs inside the budget by design), plus `TestSRF2R2_ForgedRuns*` |

Commands and results: see section 6.

## 5. Changed existing tests (intentional behaviour change; nothing weakened)

1. **`TestMA020_Parity_*`** (`internal/reconciliation/ma020_parity_integration_test.go`).
   - What changed: before, the parity check was SQL == Go. Now `ma020Parity` checks:
     - (i) SQL == GoTomb, the Go rule with statement-line clearing removed (the same F-VIS exception as before);
     - (ii) the SUBSET rule: Go-open ⇒ SQL-open, i.e. SQL-cleared ⊆ Go-cleared.
   - `ma020Want` keeps every fixture's pinned value. Fixtures cleared only by a completed reversal line are named explicitly (`lineOnly`); for them the test asserts SQL OPEN and Go still CLEARED.
   - The test is stricter than before: three relations instead of one.
   - Changed per-fixture SQL expectations (pinned value false → SQL true), all line-only clears:
     - XClearing: "X reversed by a MOCK line", "REVERSED-status line";
     - YAttribution: "unheld Y reversed", "held Y, X reversed";
     - GY2: 6 fixtures (`gy2LineOnly`);
     - RC3: "MOCK reversal, no real import", "real reversal", "Y evidenced only by ineligible MOCK line";
     - TenantIsolation: "tenant 2 park cleared by its own reversal".
2. **`TestMA020_K2_ReversalLineClearsInTenantSessionOnly` → `TestMA020_K2_ReversalLineNoLongerClears_OnlyATombstoneDoes`** (`internal/adjustment`).
   - Before: a completed line cleared, and both sessions admitted.
   - After 0172: refused (MA020) in the system shape, the tenant session and the acting session. A tombstone then clears in all three, and both sessions admit.
   - The pending and declined assertions are unchanged.
3. **`TestF2_SRF2_1_HeldLockIsAVisibleStall`**: fixture only. `matchStallLockWait` is set to 100 ms; with the default 15 s the test took 61 s. No assertion changed.

## 6. Commands and results

Harness:
- Private database `srf2r140455p22342x01` (`priv_db.sh` from this worktree, migrations through 0172 with the 0171 gap), then `priv_test.sh`.
- Payments tests use their own per-test scratch databases.
- No role, password or shared infrastructure was touched. PostgreSQL was down at the first attempt; I stopped and reported, and resumed after the orchestrator brought it back.

| Run | Command | Result |
|---|---|---|
| Format | `gofmt -l internal cmd` | clean |
| Vet | `go vet ./internal/... ./cmd/...`; `go vet -tags integration` on `internal/{reconciliation,payments,adjustment,db}` | clean |
| Lint | `golangci-lint run ...` | NOT RUN: the installed golangci-lint was built with go1.25 and refuses the module's go1.26.9 ("can't load config"); a toolchain issue, not a code finding |
| Unit | `go test -count=1 ./internal/db/ ./internal/reconciliation/... ./internal/payments/ ./internal/adjustment/ ./cmd/...` | 10 packages ok |
| Reconciliation (integration) | `priv_test.sh -tags integration -count=1 -p 1 -v -timeout 110m ./internal/reconciliation/...` | ok; **371 PASS, 0 FAIL, 0 SKIP** (2 packages) |
| Reconciliation, new and touched tests (-race) | `priv_test.sh -race -tags integration -count=1 -p 1 -v ./internal/reconciliation/ -run 'TestSRF2R\|TestMigration0172\|TestF2_SRF2_1_HeldLock\|TestF2_SRF2_4\|TestF2_SRF2_2'` | ok; 12 PASS, 0 FAIL, no DATA RACE |
| MA020 parity | `... ./internal/reconciliation/ -run TestMA020` | 9 PASS (also inside the full run) |
| Adjustment + db (integration) | `priv_test.sh -tags integration -count=1 -p 1 -v -timeout 60m ./internal/adjustment/ ./internal/db/` | both ok; **257 PASS, 0 FAIL, 1 SKIP** (`TestVerifyEnvironmentAnchor_RealDatabase_FourStates`, needs `TEST_ANCHOR_PRIVATE_DB=1` by design) |
| Payments subsets touched (gate, MA020, M2, K3, F2e/F2fr/F3, A5, B13B) | `priv_test.sh -tags integration -count=1 -p 1 -v -timeout 115m ./internal/payments/ -run '^Test(SRF2R\|F2e\|F2fr\|F3\|MA020\|M2\|K3\|A5\|B13B)'` | ok; **181 PASS, 0 FAIL, 0 SKIP** |
| Payments new tests + F2e (-race) | `priv_test.sh -race ... ./internal/payments/ -run 'TestSRF2R1\|TestF2e'` | ok; 11 PASS, 0 FAIL, no DATA RACE |
| Mutants | see section 4 | all killed |
| `migrate verify` (private DB) | `go run ./cmd/migrate verify` | every file OK including 0172; **GAP 171** (expected on this branch alone: F4's 0171 is not here) |
| Flood timing (temporary test, not committed) | sealed cursor read only, same machine, flood cumulative | 2 001 rows 41 ms; 22 001 rows 225 ms; 222 001 rows **2.03 s** with no budget (linear; the reviewer measured 70.6 s per sweep for 200 000 rows on the old paging), and **0.40 s** with the default budget (stops at 50 176 rows with `ErrPaymentCursorScanBudgetExceeded`) |

The whole `./internal/payments` package was not run (the orchestrator runs it). `internal/alerting` was not touched.
The private database is dropped at the end of this work.

## 7. Proposed registry / ADR text (for the orchestrator)

- **SRF2R-1** → `CLOSED as an engineering item (IMPLEMENTED against MOCK; migration 0172)`: "Candidate reads stream through one server-side cursor in keyset-index order (0172) with a per-read scan budget of 50 000 rows. Past it the stream run fails loud as P1 `cursor_flood` (no fetch, nothing trusted, retried next run) and the M2 gate refuses (`evidence_scan_budget_exceeded`). Request pre-reads only the attempt's provider. Residual: the Decide pre-read still reads every provider (0115 RLS hides the pending resolution from the system session), each bounded by the budget. A flood above the budget stops the pair until the owner-authorised exclusion procedure (6.11) runs."
- **SRF2R-2** → `CLOSED (IMPLEMENTED against MOCK)`: "`match_stalled` is evaluated at the end of the sweep. It counts an import only when no run-seal-verified run of the source committed after its fetch, waiting up to 15 s for an in-flight match (`pg_locks`, the lock is never taken). A continuously held lock still trips it (`lock_still_held=true`)."
- **SRF2R-3 / residual 6.10** → `CLOSED (migration 0172)`: "The SQL MA020 guard clears only on a ledger tombstone. Statement lines no longer clear in SQL. Accepted operational cost: a reversal reported only in a statement keeps manual credits refused until a tombstone exists (re-delivered verified callback). Go detective clearing is unchanged, so SQL-cleared ⊆ Go-cleared (parity pinned)." The proposed "own OPEN row (MEDIUM)" can be registered directly as CLOSED with this text.
- **SRF2-6** → stays `OPEN LOW, PROVIDER DEPENDENT / infrastructure`: synthetic import keys for staging, before staging acceptance of the first real statement source.
- **Migration numbers**: 0172 used (`0172_srf2r_ma020_tombstone_only_and_run_keyset`). It must merge after F4's 0171; until then, a branch-local `migrate verify` reports the 0171 gap.
- **ADR 0095 addendum (text only)**: "SRF2R: payment_statement candidate reads are single streaming reads with a per-read scan budget (`cursor_flood`); `match_stalled` requires the absence of any verified run after the import's fetch."
- **ADR 0101 / MA020 addendum (text only)**: "Since 0172 the SQL exposure guard (`payment_ref_cleared`) clears only on a ledger tombstone (R-MA-3 amended); statement lines clear only the Go detective finding."
- **Runbooks (applied in this branch)**:
  - `docs/runbooks/observability-and-alerting.md`: new `cursor_flood` entry, rewritten `match_stalled`, corrected the `cursor_anomaly` "no fail-closed state" sentence;
  - `docs/runbooks/operational-runbooks.md`: the MA020 paragraph.

## 8. Residuals

1. Decide's pre-read is not narrowed (2.1 (d)).
2. A flood above the budget stops the pair loudly until owner-authorised recovery. This is by design.
3. The stall check waits up to 15 s per pair per sweep while the lock is held by an attacker or a very long match. A genuine match longer than 15 s can produce one false `match_stalled`, but only for an orphan older than 30 min.
4. The budget is a row budget, not a time budget. With the 0172 indexes the per-row cost is bounded.
5. A reversal reported only in a statement leaves MA020 blocking until a tombstone exists (accepted, 2.3).
