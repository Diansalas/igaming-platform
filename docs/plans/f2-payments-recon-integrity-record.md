# F2 + F3 record: payment-statement stream integrity and payout-tombstone follow-ups

Branch `f2-payments-recon-integrity` (base `origin/claude/focused-wright-jw88w9` at `1f833ec`). Date 2026-10-16. Author:
`ledger-finance` (implementation). Work items F2 and F3 of `docs/plans/platform-freeze-work-list-2026-10-16.md`.
Everything here is `IMPLEMENTED` against the local database and MOCK providers only. Real statement ingestion stays
`PROVIDER DEPENDENT`. Nothing here claims real-PSP readiness, a licence position or a vendor contract.

Not edited (orchestrator single writer): `docs/governance/task-registry.md`, HANDOVER, progress, active-stage, the decision
register, ADRs. ADR / registry / runbook proposals are in section 9.

**Fix round (2026-10-17, after SRF2 / LFF2 below).** The orchestrator decided to close the SQL-only blinding (SRF2-1 / LFF2-1,
SRF2-2 / LFF2-2) and the other findings now. Design in 2.8, results in 4.3, mutants in 5.1. Where a statement in sections 1-9
was overstated or is superseded, it has been corrected in place and marked "(fix round)". Sections SRF2 and LFF2 are the
reviewers' text, unchanged.

## 1. Deliverables and labels

| # | Item | Origin | Label |
|---|---|---|---|
| F2a | Sealed cursor: the payment_statement cursor trusts only a run whose RUN seal verifies (migration 0168, fix round) and whose import, written by the same source, has an import seal that verifies | SR11-1 via SR11-10; SRF2-1/2/4, LFF2-1/2/4 | IMPLEMENTED (MOCK); production startup refuses a registered source without keys |
| F2b | `cursor_anomaly` discriminator carries the unbacked-row count and a digest of their ids; audit row per sighting | SR11-11 | IMPLEMENTED |
| F2c | Bounded-mode "missing provider record" (and ledger-join (a)) windowed by the attempt's last change, with a persisted-line exemption | LF-A11-1 | IMPLEMENTED (MOCK); legacy mode unchanged |
| F2d | Per-provider settle lag (source declaration + safe default), completeness horizon `PeriodEnd = now - lag` enforced, time-ordered pages enforced, MOCK conforms; contract text and runbook | LF-A11-2 / LF-A11-6 | IMPLEMENTED (MOCK); real lag value PROVIDER DEPENDENT |
| F2e | M2 (declare paid / declare not paid) gated on a CURRENT payment_statement reconciliation for the provider, at submission and execution | H-W1 second half (LF-A11-8) | IMPLEMENTED (MOCK) |
| F3a | The A5 tombstone branch refuses a payout reference that already carries a tombstone (vote-time check + `AlreadyPosted` backstop), typed error | SR5R-1 | IMPLEMENTED (MOCK) |
| F3b | Standing finding `pay_payout_reversal_tombstone_but_paid` (migration 0167) | A5 R9 / LF-A5-1 (b) | IMPLEMENTED (MOCK); return-line semantics of a real PSP PROVIDER DEPENDENT |

Migration numbers: **0167** used (`0167_payout_reversal_tombstone_paid_kind`, up and down) and removed from
`internal/db/migrate.go` `reservedUnusedVersions` (the orchestrator's reservation commit `9378c91` says "if a listed version
is later used, remove it"). **0168 used (fix round)**: `0168_reconciliation_run_seal` (up and down), removed from
`reservedUnusedVersions`; the exact-set test now expects `{143..150, 159, 161, 169}`. `migrate verify` on the merged tree:
clean, no gaps.

Additional fix-round items: F2c exemption requires the exact amount and asset, and a `reversed` line becomes
`pay_status_mismatch` (LFF2-5, LFF2-6). Every persisted line that can clear or confirm must come from a verifying import, lie
inside that import's coverage and not be dated in the future (LFF2-5 / SRF2-3). The F2e evidence is read before the M2
transaction opens (SRF2-5 / LFF2-7), and a future-dated failure row cannot block M2 (LFF2-3). All are IMPLEMENTED (MOCK).

## 2. Design

### 2.1 F2a: sealed cursor (SR11-10 closing the SQL-only half of SR11-1)

`internal/reconciliation/payment_cursor_seal.go`. With a seal verifier available (the stream's `ImportKeys`, else the
process-wide sealer when it can verify: `payoutinstrument.Keys` does), the cursor of (tenant, provider) is the newest
`coverage_end` among the pair's `payment_statement` run rows (`run_at <= now()`, `coverage_end <= now()`) for which a stored
import exists with the same tenant, provider and `coverage_end`, written by this stream (`imported_by_service`), by THIS
source (`source_label`, `is_mock`), carrying `import_seal` + `seal_kid`, whose seal VERIFIES over the import row and all its
stored lines (`verifyImportSealInTx`, the reconciliation-side twin of `payoutinstrument.VerifyStatementImportInTx`, parity
pinned by a test; the package still holds no key material: it uses the `statement.ImportSealVerifier` interface).

- (Fix round: superseded by 2.8. The run row itself is now sealed and is the cursor's anchor.) Every run row of the pair
  above that cursor (whatever its `run_at`/`coverage_end`) is unbacked: ignored, logged at Error, audited
  (`reconciliation.payment_cursor_anomaly`) and alerted (F2b).
- (Fix round: REMOVED, see 2.8. There is no candidate limit and no fail-closed state, and `ErrPaymentCursorUnverifiable` no
  longer exists.) The original design examined at most 64 candidate `coverage_end` values, 4 sealed imports each, and
  failed closed when all 64 failed. A flood of forged rows could therefore stop the stream (SRF2-4 / LFF2-4).
- The stale-cursor re-check under the advisory lock uses the same sealed read (in the match snapshot).
- Ingest seals with the same keys (`importSealerFor`).
- Without any verifier (a non-production process without `PAYOUT_INSTRUMENT_KEYS`) the A11 rule (any backing import) applies
  unchanged. `reconciliation.VerifyPaymentCursorSealing` (called in `run()` right after `SetDefaultImportSealer`, before the
  scoped loop; static pin) refuses a PRODUCTION start with at least one registered statement source and no verifier.
  Production already requires the keys for any non-MOCK source (`payoutinstrument.VerifyStartup`) and refuses MOCK sources,
  so this is a belt-and-braces gate.

Residuals (stated honestly):
1. An attacker who can run code in the application process holds the key (the T6 class): out of scope of a seal.
2. (Fix round: CLOSED.) The original text understated this case. A forged run row naming the `coverage_end` of a genuine
   sealed import whose match never committed moved the cursor past that window. Holding the stream's advisory lock (any
   session can) makes the match skip silently, so this was a repeatable SQL-only blinding of the whole stream, not "at most
   one window" (SRF2-1 / LFF2-1). It is closed by the run seal (2.8): a run row that the application did not seal is never a
   cursor. The skip and the stall are also loud now. Tests: `TestF2_SRF2_1_*`.
3. Key rotation: a retired kid that the process no longer holds makes its imports unverifiable; the cursor falls back to an
   older verifying run (one re-fetched window, idempotent at import level) and `cursor_anomaly` fires once. Runbook updated.
4. Relabelling a source resets its trusted cursor (label is part of the backing rule). Documented in the source contract.

### 2.2 F2b: anomaly discriminator (SR11-11)

`raiseRunNotReconciledAs`: the phase attribute stays `cursor_anomaly`, the discriminator tail is
`cursor_anomaly:n<count>:<16-hex digest of the sorted unbacked run ids>` (falls back to `cursor_anomaly:<digest>` when a
100-character provider id would exceed the 160-character CHECK). An unchanged set re-occurs on ONE alert; any new forgery
(even one that keeps the count, e.g. one row drops below a new genuine cursor while another is added) changes the digest and
opens a NEW alert. The audit row carries count, `newest_unbacked_run_id`, digest and `sealed_cursor` for the runbook.

### 2.3 F2c: bounded-mode matcher windowing (LF-A11-1)

`payment_statement.go`. In continuation (bounded) mode, lines are dated by their last status change, so:
- `payAttempt.lastChangeAt = COALESCE(resolved_at, updated_at)` (the platform's record time of the attempt's last change);
- a succeeded attempt unmatched in THIS import is a candidate only if `lastChangeAt` lies in `[coverage_start, coverage_end)`;
- `confirmMissingProviderRecords` reports a candidate unless an ELIGIBLE (D-4) persisted line of this provider already records
  it: same operation, status `succeeded` or `reversed`, by provider reference, payout settlement reference or merchant
  reference. This covers the reverse latency case (provider line in window k, platform success in window k+1): the line was
  imported, so the provider has a record. A provider that never recorded the success (no line, or only a pending line) is
  still reported ("never silently missing", tested both ways). (Fix round, LFF2-5 / LFF2-6: the exemption now needs a
  USABLE line: an eligible import whose seal verifies when the process has keys, the line inside that import's coverage and
  not after this run's coverage end, status `succeeded`, and amount AND asset equal to the attempt's. A usable succeeded line
  with another amount or asset is `pay_amount_mismatch` / `pay_asset_mismatch` (`check=persisted_amount` /
  `persisted_asset`). A usable `reversed` line is `pay_status_mismatch` (`check=persisted_reversed`). The read is bounded
  (64 lines per candidate + 1), and passing that bound fails the run with `ErrPaymentEvidenceOverflow`.);
- ledger-join (a) ("a succeeded attempt has exactly its posting") uses the same windowing in bounded mode, so an attempt that
  succeeds after the window of its `sentAt` is still checked once.
Legacy (creation-dated, non-continuation) mode is unchanged (sentAt, no exemption); its pins pass unchanged.

### 2.4 F2d: settle lag, completeness horizon, paging (LF-A11-2 / LF-A11-6)

- `statement.SettleLagDeclarer` (`StatementSettleLag() time.Duration`), optional on a source. Resolution: declared value
  (must lie in `[0, 7 days]`: refused at registry startup in every environment, and per run as a `source` P1), else
  `statement.DefaultStatementSettleLag` (15 min) for a real source, 0 for a Synthetic (MOCK) source.
- Every continuation request carries `Continuation = true` and `PeriodEnd = min(sweep end, now - lag)` (microsecond
  truncated). `validateBoundedWindow` refuses a continuation statement whose `CoverageEnd` passes `PeriodEnd` (truncated or
  not), nothing stored. When the horizon has not yet passed the cursor (a lag raised since the last run) the run is SKIPPED
  (nothing fetched, Info log, no failure, no clean claim).
- A truncated page must list its lines in non-decreasing `OccurredAt` order (`ErrPaymentStatementOutOfOrder`, which wraps
  `ErrPaymentStatementCoverageGap`). A complete (non-truncated) window may be in any order.
- MOCK: in bounded mode (now also `req.Continuation`, so the first continuation run with a zero cursor is bounded too) it caps
  `CoverageEnd` at `PeriodEnd`, leaves later lines for the next statement and lists lines in time order.
- Contract text: `docs/plans/a11-payments-wiring-record.md` section 3 items 3 and 5 to 9 (what is enforced, what stays
  PROVIDER DEPENDENT); `docs/architecture/reconciliation-model.md` items 14-15; runbook
  `docs/runbooks/observability-and-alerting.md` (`cursor_anomaly`, `ErrPaymentCursorUnverifiable`, stuck-cursor steps 1 and 5-8).

### 2.5 F2e: M2 gated on a current reconciliation (H-W1 second half)

`internal/payments/manual_resolution_recon_gate.go`. Both M2 kinds are refused at submission
(`ErrResolutionReconciliationNotCurrent`, class precondition, HTTP 409 `force_resolve_precondition_failed`, the existing
denied-audit path) and at execution (`refused_at_execution`, refusal code `reconciliation_not_current`, nothing posted)
unless, for (tenant, attempt's provider):
1. the newest stream-recorded run exists. This is a `payment_statement` run row with `run_at <= now()` and a backing import.
   When the service has keys (`WithImportSealKeys`, wired in production from `PayoutInstruments`), the run must instead be the
   newest one whose RUN seal verifies, whose import has the sealed shape, and whose import seal verifies. This lookup uses
   keyset pages of 512 and has no row limit (fix round, SRF2-2 / LFF2-2). The original text said "sealed-import-backed, at
   most 16 rows". That was overstated: a run row copied with `run_at = now()` was admitted (SRF2-2);
2. it is not truncated (with keys: the SEALED `window_truncated`);
3. no `reconciliation.sweep_run_failed` audit row for `payment_statement` and this provider id is newer than it. Fix round
   (LFF2-3): the failure row must also satisfy `created_at <= now() + 5 min`, so a row dated in the future cannot block M2
   for good;
4. it is at most `DefaultStatementEvidenceMaxAge` (6 h, `WithStatementEvidenceMaxAge`) old (database clock; with keys, the
   SEALED `run_at`).
The evidence is read in its own short read-only tenant (system) transaction, because a platform-acting session cannot read
`audit_log` (0112 acting fence). A forged failure row can only refuse. Fix round (SRF2-5 / LFF2-7): this read happens
BEFORE the M2 transaction opens, in `Request` and in `Decide` (approve). The result is carried on the context, and the
in-transaction checks (submission and execution) apply it without opening a second pooled connection. A context with no
pre-read evidence refuses (`reconEvidenceUnread`) and never falls back to an in-transaction read. The execution-time recheck
therefore uses evidence read immediately before the execution transaction. All existing M2 governance (proof-bound request and
approvals, four-eyes, registered-source rule, database guard) is unchanged; M4 is not gated (it rests on its own sealed
evidence). Not added: a database restatement of the gate (the seal cannot be verified in SQL) - Go-only, like the
source-registry rule it sits next to.

### 2.6 F3a: pre-existing tombstone on the payout reference (SR5R-1)

`internal/payments/payout_reversal.go`. `ErrPayoutReversalReferenceTombstoned` (wraps
`ErrPayoutReversalAttemptInconsistent`, so HTTP 409 unchanged):
- vote time (`lockPayoutAttemptsForReversal`, every decision on a submitted payout): refused when ANY tombstone already holds
  (tenant, provider, original reference) - nothing recorded, no vote slot used;
- backstop in the branch: `ledger.Post` returning `AlreadyPosted` for the branch's tombstone is refused and rolls everything
  back (park, E10 release, audit), so WD027 can never be bypassed by a replay.

### 2.7 F3b: standing finding for a paid payout that the tombstone branch failed (A5 R9)

`internal/reconciliation/payment_statement_r9.go`, migration 0167 (`reconciliation_mismatches_mismatch_kind_check` = 0134's
list + `pay_payout_reversal_tombstone_but_paid`; down refuses WD099 while such findings exist). Per tombstone whose
`correlation_id` is a withdrawal request of the tenant (only the A5 branch writes one; WD027 binds it):
- RAISE: any persisted import (MOCK included, any amount) holds a `succeeded` payout line on the original reference or naming a
  merchant reference of the request's payout attempts at this provider;
- CLEAR (only): (1) PSP return evidence - an ELIGIBLE (D-4) `reversed` payout line on exactly the original reference, amount
  and asset equal to the request's, dated no earlier than the newest succeeded line (a later "succeeded" re-raises); or
  (2) a K2 NET recovery of the withdrawn amount under the request's `withdrawal_failed` (E10) transaction on its wallet and
  asset (`netRecovery`, the M2 (d) / RR-1 rule; partial recovery does not clear);
- unwindowed, every run, one finding per tombstone, P1 through the stream's existing mismatch alert.
- (Fix round, LFF2-5 (b) / SRF2-3.) A clearing (return) line must also come from an import whose seal verifies when the
  process has keys, lie inside that import's coverage, and not be dated beyond the database clock plus 5 minutes. A forged
  return line dated 50 years ahead therefore no longer suppresses R9 for good. Raising is unchanged: any import, any amount.
Why it is not a permanent false P1: a legitimate branch is one where the PSP really returned the money; either the PSP never
reports the payout as succeeded (never raised: tested) or it reports the return (clears: tested), or the platform recovers
from the player (clears: tested). Residual: an off-platform recovery has no clearing path (identical to M2 (d)); it is
tracked through the mismatch's investigation status. The kind is in the staff-view vocabulary (`read.go`) and the OpenAPI
`StaffReconciliationKind` enum.

### 2.8 Fix round (SRF2 / LFF2): run seal, loud skip and stall, line eligibility, pre-read evidence

1. **Run seal (SRF2-1/2, LFF2-1/2; migration 0168).**
   - New HKDF label `b13-run-v1` in `payoutinstrument.Keys`, next to the import label `b13-import-v1`.
   - `SealPaymentRun` / `VerifyPaymentRun` (HMAC-SHA256, constant-time compare) sign the canonical text
     `statement.RunCanon` (domain tag `run`) over:
     - run id, tenant, provider, stream;
     - the import the run matched;
     - that import's coverage start and end;
     - `window_truncated`;
     - `run_at` (truncated to microseconds before sealing, so the stored value round-trips).
   - Migration 0168 adds `run_import_id`, `run_seal` and `run_seal_kid` to `reconciliation_runs`, with these constraints:
     - CHECK all-or-none;
     - CHECK shape: 64 lower-hex seal, kid pattern, stream `payment_statement`, provider and `coverage_end` not null;
     - same-tenant FK `(run_import_id, tenant_id)` to `payment_statement_imports`;
     - keyset index `reconciliation_runs_payment_sealed_cursor`.
   - The down migration refuses while sealed runs exist.
   - The application writes the seal in the run's own INSERT (`persistRun`). Reconciliation holds only the
     `statement.RunSealer` / `RunSealVerifier` interfaces, never a key.
   - With keys, a run row is the cursor (and M2 evidence) only if all of these hold:
     - its run seal verifies;
     - its import was written by this stream and source with the sealed coverage;
     - that import's seal verifies over the import and all its lines.
   - A forged row, or a row copied with a new id or `run_at`, never verifies. This includes the exact probe: a forged row at a
     genuine import's `coverage_end` while the advisory lock is held.
   - Without keys (non-production only) the A11 rule applies unchanged. Production refuses a registered source unless BOTH
     verifiers are present (`VerifyPaymentCursorSealing`).
2. **No fail-closed state, bounded anomaly read (SRF2-4 / LFF2-4).**
   - The cursor reads candidate runs in keyset pages of 512 (newest first), with one HMAC per row. The import seal is
     verified only for a row whose run seal already verified.
   - There is no candidate limit and no fail-closed state. Forged rows cost a linear scan but can neither move nor stop the
     stream. `ErrPaymentCursorUnverifiable` is removed.
   - The anomaly set is the run rows that do not back the trusted cursor and are newer than it (`coverage_end` above it, or
     `run_at` after the cursor run's). It is one `count(*)` plus at most 1000 rows read for the digest.
3. **Loud skip, durable stall (SRF2-1 (b)(c)).**
   - Every match skipped for lock contention or a stale cursor writes `reconciliation.payment_match_skipped` (target is the
     import id) and a Warn log.
   - Every sweep then checks for imports of this source above the trusted cursor that were fetched more than 30 minutes ago
     (`MatchStallAfter`). Any such import raises the durable P1 phase `match_stalled` plus the audit row
     `reconciliation.payment_match_stalled`.
   - The window is never lost: the next sweep re-fetches from the unchanged trusted cursor and matches the new import.
   - A held advisory lock therefore turns into a visible stall, not into blinding.
4. **Clearing / confirming lines (LFF2-5 / SRF2-3).** A persisted line may clear or confirm (K3 S1-S6, M2 / M4 evidence,
   R9 return, deposit reversal, F2c exemption) only if all of these hold:
   - its import is D-4 eligible;
   - its import's seal verifies when the process has keys (`importVerified`, cached per run);
   - it lies inside its import's coverage;
   - it is not dated beyond the database clock plus 5 minutes.

   The F2c exemption bounds by this run's coverage end instead, because it is a check of the current window.

   The clock bound replaced an earlier "not after this run's coverage end" bound for K3 / R9 / reversals. A backfill run over
   an older window, or the MA020 Go-vs-SQL parity harness, must keep honouring genuine later evidence (`TestK3_C34-36`,
   `TestR2_GY2*` and `TestMA020_Parity_*` failed with the run bound and pass with the clock bound).

   The current import's own lines clear only if its seal verifies (`currentImportOK`). Raising is unchanged.
5. **F2c amount / asset / reversed (LFF2-5 (3), LFF2-6).** See 2.3. The comment on `lastChangeAt` now states that `now()` is
   the transaction start.
6. **F2e (LFF2-3, SRF2-5 / LFF2-7, SRF2-2).** See 2.5.

## 3. Changed assertions and fixtures (nothing deleted or weakened)

1. `internal/reconciliation/payment_sources_integration_test.go`:
   - `TestBound_SR11_ForgedInSkewCursorDoesNotBlindTheStream_AndIsReportedLoudly`: discriminator equality
     `...:cursor_anomaly` -> prefix `...:cursor_anomaly:` (F2b adds the count and digest; the P1 assertion is unchanged).
   - `TestBound_ForgedCursorsAreIgnored`: "B's cursor differs from A's" -> "B's cursor is exactly where it was before A's
     forgery". With the completeness horizon both providers' runs end at the same sweep end, so equal VALUES are expected;
     the property under test (independence) is now asserted directly and more strictly.
   - `TestScoped_PerTenantIsolation`: "the two tenants' cursors differ" -> "each tenant's cursor equals its OWN last run's
     coverage_end, and tenant 2's cursor did not move during tenant 1's second sweep" (same reason; stricter).
   - Fixtures `recSource` (nil fn) and `contiguousEmpty` cap their CoverageEnd at a continuation request's `PeriodEnd`
     (`horizonOf`): a contract-abiding source. The second sweep of `TestScoped_PerTenantIsolation` (same `now`) is now a
     skip (horizon not past the cursor) instead of an empty re-run; its assertion (no error for tenant 2) is unchanged.
   - `mustCursor` reads the A11-mode cursor through `readPaymentCursorState(..., cursorBasis{})` (helper moved; same rule).
2. `internal/payments/k3_world_integration_test.go`: before the world's FIRST M2 request (`w.request` with an M2 kind) the
   K3 world makes ONE genuine payment_statement run (`ensureStatementRun`: the real stream over the world's MOCK provider,
   continuation mode) unless `k3Opts.noStatementRun`, so the M2 tests keep exercising M2 with honest evidence. Lazy on
   purpose: a first, eager version (a run at world creation) changed the run/import counts and the migrate-down paths (0160's
   down refuses cursor evidence) of R3/R32 tests that never request an M2; those tests are untouched. No assertion changed.
3. `internal/httpserver/payment_force_resolution_api_integration_test.go`: `newFRWorld` runs one genuine stream run for
   (tenant, `mock`) (same reason); `frWorld.statementRun()` helper. No assertion changed.
4. `internal/db/migrate_test.go` `TestReservedUnusedVersions_ExactSet`: the expected set loses 167 (now a real migration),
   exactly as the reservation rule requires; 168 and 169 stay.
5. MOCK statement source: bounded pages in time order (not reference order) and capped at `PeriodEnd` (content digests of
   bounded MOCK imports change; no test pinned them).

Fix round:

6. `TestF2_SR11_10_SealedCursorIsPerSourceLabel`: the expected anomaly count for a genuine verifying run of ANOTHER label
   changed from 1 to 0, and the test now fails when `unbacked != 0`. The old rule counted every run above the cursor. The new
   rule counts only rows that do not back the cursor, and that run's seal verifies. The per-label cursor assertions are
   unchanged.
7. `TestF2_SR11_10_ForgedCandidateFloodFailsClosed` was REPLACED by `TestF2_SRF2_4_ForgedRunFloodNeitherMovesNorStopsTheStream`
   (2000 forged rows). The old test pinned the fail-closed behaviour that SRF2-4 / LFF2-4 asked to remove. The new test
   asserts these four things:
   - the stream keeps working;
   - the cursor advances only on genuine sealed runs;
   - the anomaly count equals the number of forged rows;
   - an alert opens.
8. `ErrPaymentCursorUnverifiable` is removed, together with its runbook bullet.
9. `TestReservedUnusedVersions_ExactSet` also loses 168: the set is now `{143..150, 159, 161, 169}`.
10. `TestF2_VerifyPaymentCursorSealing`: a verifier that verifies imports but not runs now FAILS the production gate, so a
    fake with both verifiers (`f2SealVerifyRuns`) was added for the passing case.
11. `ImportKeys` (`PaymentStatementOptions`, `payment_sources.go`) is typed `statement.StreamSealKeys` (import + run sealer and
    verifier). `*payoutinstrument.Keys` satisfies it, so callers are unchanged. The test fixtures were updated to the type.

## 4. Commands and results

Harness: private scratch database `f220261016215909p2688x01` on the local PostgreSQL (`PRIV_DB=... PRIV_SRC=<worktree> bash
priv_db.sh`, migrations through 0167; `bash priv_test.sh <go test args>`). `internal/payments` integration tests run on per-test
scratch databases migrated from the source tree. Never CI, never staging. No role, password or process was touched.

| Run | Command | Result |
|---|---|---|
| Format | `gofmt -l internal cmd` | clean |
| Vet | `go vet ./...`; `go vet -tags integration ./...` | clean |
| Lint | `golangci-lint run --allow-parallel-runners ./...` (v2.9.0) | 0 issues |
| Lint (integration tag) | `golangci-lint run --allow-parallel-runners --build-tags integration ./internal/reconciliation/... ./internal/payments/... ./internal/httpserver/... ./cmd/... ./internal/db/...` | 0 issues in any F2/F3 file (the 653 reported after the merge are the pre-existing errcheck/staticcheck/unused backlog in other test files) |
| Unit | `go test -count=1 ./...` | 74 packages ok, 0 FAIL |
| Reconciliation (integration) | `priv_test.sh -tags integration -count=1 -p 1 -v ./internal/reconciliation/...` | ok; 354 top-level PASS, 0 FAIL, 0 SKIP |
| Reconciliation (-race) | `priv_test.sh -race -tags integration -count=1 -p 1 -timeout 2h ./internal/reconciliation/...` | ok, no DATA RACE |
| Withdrawal + cmd + db (migration pins) | `priv_test.sh -tags integration -count=1 -p 1 -v ./internal/withdrawal/ ./cmd/... ./internal/db/` | 7 packages ok; 374 PASS, 0 FAIL, 1 SKIP (`TestVerifyEnvironmentAnchor_RealDatabase_FourStates`, needs `TEST_ANCHOR_PRIVATE_DB=1` by design) |
| httpserver subsets | `priv_test.sh -tags integration -count=1 -p 1 -v -run 'TestForceResolution\|TestStaffRead\|Reconciliation\|Golden\|TestStaffRoutes\|PayoutReversal\|TestA5\|OpenAPI\|Openapi\|M2\|ResolutionIsolation' ./internal/httpserver/` | 93 PASS, 3 FAIL, 0 SKIP. The 3 FAILs are `TestResolutionIsolation_{OneTenantStoreOutage,MultipleTenantsOutage,FinancialDuringOutage}` (a 400 ms pool-hold timing bound of the tenant-resolution store): they fail the same way on the untouched base (`git archive origin/claude/focused-wright-jw88w9`, same DB and machine load: 2 of 3 failed there), no tenant-resolution code is touched here, and they are a load artefact of the shared machine. Every force-resolution / M2 / staff-read / reconciliation / route-golden / OpenAPI test passes, including the new `refused_when_reconciliation_not_current` |
| Payments subsets | see 4.1 | see 4.1 |

### 4.1 Payments

| Run (pre-merge, head `fb91815`/`8807c42`) | Command | Result |
|---|---|---|
| A11/A5/T8/M2/K3 subset | `priv_test.sh -tags integration -count=1 -p 1 -v -timeout 110m -run '^Test(A5\|T8\|F2e\|F3\|K3\|SL\|ParkBind\|H12\|R3\|R32\|A11\|PRH2\|Migration0115\|Migration0162\|Migration0163\|Migration0165\|M2\|S4B)' ./internal/payments/` | 181 PASS, 1 FAIL, 0 SKIP. The FAIL was this branch's own `TestF2e_M2_RefusedAfterAFailedOrTruncatedRunOrWhenStale`, compiled before its fixture was adapted to the lazy K3 statement run (commit `fb91815`); re-run alone afterwards: `-run '^TestF2e'` 6 PASS, 0 FAIL |
| "a few letters" subset | `... -run '^Test(D7\|M4\|PostM4\|B11\|B12\|RV\|IWire\|DrainMerchant\|OrphanResolve\|Receipt\|RR\|Sweeper)' ./internal/payments/` | 318 PASS, 0 FAIL, 0 SKIP |
| -race, new tests | `priv_test.sh -race -tags integration -count=1 -p 1 -v -run '^Test(F2e\|F3)' ./internal/payments/` | 19 PASS, 0 FAIL, no DATA RACE |
| (history) first subset, eager K3 statement run | same as row 1 at `af18734` | 171 PASS, 8 FAIL: `TestK3_C9_*` (import count), `TestR3_*` x5 (run/import counts), `TestR32_Migration0127*` x2 (0160's down refuses cursor evidence). All caused by the then-EAGER world run; fixed by making it lazy (section 3 item 2), all PASS in row 1 |

A full `./internal/payments` run (`-timeout 3h`, non-verbose) was started earlier and STOPPED by the agent harness's 2 h
background limit before it reported (a non-verbose package prints nothing until it ends); it is not counted.

### 4.2 After merging `origin/claude/focused-wright-jw88w9` (`9378c91`: F1, F5/F6, migrations 0166/0170, the 0167-0169 reservation)

Private DB rebuilt from the merged tree (through 0170); `migrate verify`: clean, no gaps.

| Run | Result |
|---|---|
| `./internal/reconciliation/...` (integration, -v) | ok; 354 PASS, 0 FAIL, 0 SKIP |
| payments `-run '^Test(F2e\|F3\|A5\|T8\|K3_C4\|K3_C7\|K3_C9\|R3_\|R32)'` (-timeout 100m) | ok; 100 PASS, 0 FAIL, 0 SKIP (a first attempt without `-timeout` hit go test's default 10 min and is not counted) |
| httpserver `-run 'TestForceResolution\|TestStaffRead\|Golden\|TestStaffRoutes\|PayoutReversal\|OpenAPI'` | ok; 88 PASS, 0 FAIL, 0 SKIP |
| `./internal/db/ ./cmd/... ./internal/withdrawal/` (integration, -v) | 7 packages ok; 374 PASS, 0 FAIL, 1 SKIP (the anchor test, by design) |
| unit `./internal/db/ ./cmd/...`; gofmt; vet (plain + integration); golangci-lint | ok / clean / clean / 0 issues (integration tag: 0 in F2/F3 files) |
| `git diff origin/claude/focused-wright-jw88w9 --stat -- docs/governance` | EMPTY |

### 4.3 Fix round (2026-10-17)

Setup:
- Private DBs: `f220261017024540p27680x01` for the suites and `f220261017024540p27680x02` for targeted runs and mutants.
  Both were migrated through 0170, including 0168.
- The suites ran on the tree at `b4cc039`. The only later code commits add tests:
  - `TestF2fr_FutureDatedReversalNeverClears` (80dec80);
  - `TestF2_LFF2_5_OutOfCoverageLineDoesNotExempt` (b098e24).

  These ran separately and PASS. The full reconciliation run predates b098e24, so its count excludes that one new test.
- An earlier fix-round reconciliation run, before the clock-bound change in `8377f93`, had 17 FAILs:
  - `TestK3_C34-36`, `TestR2_GY2*`, `TestMA020_Parity_*`, `TestD2_14`, `TestRes1_I1`, `TestStanding1`, `TestBoundClear1`;
  - the cause was the run-coverage-end date bound (section 2.8 item 4);
  - after the fix, all of these PASS in the table below.

| Run | Command | Result |
|---|---|---|
| Format / vet | `gofmt -l internal cmd`; `go vet ./...`; `go vet -tags integration ./...` | clean / clean / clean |
| Lint | `golangci-lint run --allow-parallel-runners ./...` | 0 issues |
| Lint (integration tag) | same with `--build-tags integration` over reconciliation, payments, httpserver, cmd, db, payoutinstrument | 653 (errcheck 649, staticcheck 4), all pre-existing backlog; 0 in any F2/F3/fix-round file |
| Unit | `go test -count=1 ./...` | 75 packages ok, 0 FAIL |
| Reconciliation (integration, full) | `priv_test.sh -tags integration -count=1 -p 1 -v -timeout 100m ./internal/reconciliation/...` | ok; 361 PASS, 0 FAIL, 0 SKIP |
| Reconciliation (-race, new + touched) | `priv_test.sh -race ... -run 'TestF2\|TestBound_\|TestScoped_\|TestMigration016\|TestK3_C3\|TestMA020' ./internal/reconciliation/` | ok; 63 PASS, 0 FAIL, no DATA RACE |
| Payments (-race, new) | `priv_test.sh -race ... -run '^Test(F2e\|F3\|F2fr)' ./internal/payments/` | ok; 26 PASS, 0 FAIL, no DATA RACE |
| Payments subset 1 | `... -timeout 110m -run '^Test(F2e\|F3\|F2fr\|A5\|T8\|K3\|SL\|ParkBind\|H12\|R3\|R32\|M2\|S4B\|PRH2\|Migration0115\|Migration0162\|Migration0163\|Migration0165)' ./internal/payments/` | ok; 189 PASS, 0 FAIL, 0 SKIP |
| Payments subset 2 (letters) | `... -timeout 110m -run '^Test(D7\|M4\|PostM4\|B11\|B12\|RV\|IWire\|DrainMerchant\|OrphanResolve\|Receipt\|RR\|Sweeper)' ./internal/payments/` | ok; 318 PASS, 0 FAIL, 0 SKIP |
| db (migration pins) + cmd + withdrawal + payoutinstrument | `priv_test.sh -tags integration -count=1 -p 1 -v ./internal/db/ ./cmd/... ./internal/withdrawal/ ./internal/payoutinstrument/` | 8 packages ok; 460 PASS, 0 FAIL, 1 SKIP (the anchor test, by design) |
| httpserver subsets | `... -run 'TestForceResolution\|TestStaffRead\|Golden\|TestStaffRoutes\|PayoutReversal\|OpenAPI\|Reconciliation' ./internal/httpserver/` | ok; 91 PASS, 0 FAIL, 0 SKIP |
| New tests after `b4cc039` | `-run '^TestF2fr'` (payments); `-run 'TestF2_LFF2_'` (reconciliation) | 2 PASS; 3 PASS |
| Merge check | `git fetch origin claude/focused-wright-jw88w9` (still `9378c91`, already an ancestor); `git diff origin/claude/focused-wright-jw88w9 --stat -- docs/governance` | up to date; EMPTY |

## 5. Mutation pass

Script `f2_mutate.py` in the session scratchpad: a copy of the tree, one mutant at a time, the targeted test set, the private DB.
60 mutants of the new logic (round 1: 60 written, 2 of them did not build: B2 and E6, rewritten as B2b and E6b). Round 1: 47
killed, 11 survived, 2 unbuilt. Rounds 2/3: seven survivors killed by new tests (marked "round 2"), B2b and E6b killed.
**Final: 60 valid mutants, 56 killed, 4 survived (A2, A5, A8 equivalent; C5 justified).** After the K3 world's statement run
became lazy (section 3 item 2) the gate mutants E1-E5, E7, E9 were re-run: all killed (E7 needed its "other provider" case to
make the world's run explicit first, commit `8807c42`).

| id | mutant | result |
|---|---|---|
| A1 | sealed cursor accepts an import whose seal does not verify | KILLED `TestF2_SR11_1_*` (made-up and copied seal) |
| A2 | drop `import_seal/seal_kid IS NOT NULL` from the candidate query | SURVIVED, equivalent: `verifyImportSealInTx` refuses a NULL seal or kid; the filter only narrows the scan |
| A3 | drop `source_label` from the backing rule | KILLED `TestF2_SR11_10_SealedCursorIsPerSourceLabel` |
| A4 | drop `is_mock` from the backing rule | KILLED same (`unmarkedSource`) |
| A5 | drop `imported_by_service` from the backing rule | SURVIVED, equivalent for a SQL-only attacker: the value is inside the seal (`ImportCanon`), so another value never verifies without the key; kept as defence in depth |
| A6 | no fail-closed after 64 unverified candidates | KILLED `TestF2_SR11_10_ForgedCandidateFloodFailsClosed` |
| A7 | anomaly count uses the A11 "no import at all" rule in sealed mode | KILLED `TestF2_SR11_1_*`, `TestF2_SR11_11_*` |
| A8 | drop the stored-line-count check in verification | SURVIVED, equivalent: `line_count` and the lines digest are inside the seal, and 0102's constraint trigger forces the stored count to equal the declared one at insert (lines immutable afterwards) |
| A9 | stale-cursor re-check under the lock reads the A11 cursor | KILLED `TestF2_SR11_1_*` (run wrongly skipped) |
| A10 | ingest ignores `opts.ImportKeys` | KILLED `TestF2_SR11_1_*` |
| A11 | startup gate checks "a sealer" instead of "a verifier" | KILLED `TestF2_VerifyPaymentCursorSealing` |
| A12 | anomaly never reported | KILLED `TestF2_SR11_1_*`, `TestBound_SR11_*` |
| A13 | process-wide sealer not used as the cursor verifier | KILLED (round 2) `TestF2_SR11_10_ProcessWideKeysSealAndVerify` |
| B1 | discriminator without the digest | KILLED `TestF2_SR11_11_*`, `TestF2_CursorAnomalyPhase` |
| B2b | discriminator without count and digest | KILLED same |
| B3 | no length fallback | KILLED `TestF2_CursorAnomalyPhase` |
| C1 | bounded rule windowed by `sentAt` (pre-F2) | KILLED by the LF-A11-1 probe |
| C2 | persisted-line exemption dropped | KILLED probe |
| C3 | bounded branch dropped (legacy rule in bounded mode) | KILLED probe |
| C4 | exemption accepts any line status | KILLED `TestF2_LFA11_1_PendingOnlyAtTheProviderIsStillReported` |
| C5 | ledger-join (a) windowed by `sentAt` in bounded mode | SURVIVED: no public path creates a succeeded attempt without its posting (ApplySuccess writes both atomically), so the rule's input cannot be produced without bypassing the payments state machine; the mutant restores exactly the pre-F2 behaviour. Disclosed (section 6) |
| C6 | exemption ignores D-4 eligibility | KILLED `TestF2_LFA11_1_*` |
| C7 | matcher never bounded | KILLED probe |
| C8 | exemption ignores the merchant reference | KILLED (round 2) `TestF2_LFA11_1_ExemptionByMerchantReference` |
| D1 | horizon check dropped | KILLED `TestF2_LFA11_2_SettleLag_*`, `TestF2_ValidateBoundedWindow_*` |
| D2 | request not capped at now - lag | KILLED `TestF2_LFA11_2_SettleLag_*` |
| D3 | real-source default lag 0 | KILLED `TestF2_PaymentSettleLag` |
| D4 | upper lag bound dropped | KILLED `TestF2_PaymentSettleLag`, `TestF2_LFA11_2_InvalidDeclaredLagFailsTheRun` |
| D5 | page-order check dropped | KILLED `TestF2_LFA11_6_*`, `TestF2_ValidateBoundedWindow_*` |
| D6 | MOCK bounded pages sorted by reference | KILLED (round 2, 12 deposits whose references sort unlike time) |
| D7 | MOCK lists lines past the horizon | KILLED `TestF2_LFA11_6_*` |
| D8 | no skip when the horizon has not passed the cursor | KILLED `TestScoped_PerTenantIsolation` |
| D9 | registry does not validate the lag | KILLED `TestF2_Registry_RefusesAnInvalidDeclaredSettleLag` |
| D10 | MOCK coverage not capped at the horizon | KILLED `TestF2_LFA11_6_*`, `TestBound_*`, `TestScoped_*` |
| D11 | request without `Continuation` | KILLED `TestF2_LFA11_2_*`, `TestScoped_*`, `TestBound_*` |
| E1 | M2 submission gate dropped | KILLED `TestF2e_*` |
| E2 | M2 execution gate dropped | KILLED `TestF2e_M2_ExecutionRefusedWhenReconciliationFailsAfterTheRequest` |
| E3 | later failure ignored | KILLED `TestF2e_*` |
| E4 | truncated last run accepted | KILLED `TestF2e_*` |
| E5 | staleness ignored | KILLED `TestF2e_*` |
| E6b | with keys, a sealed-looking import accepted without verification | KILLED (round 3) forged import in `TestF2e_M2_WithKeys_OnlyASealedRunIsEvidence` |
| E7 | a failure of ANY provider counts | KILLED `TestF2e_M2_RefusedAfterAFailedOrTruncatedRunOrWhenStale` |
| E8 | with keys, unsealed imports admitted | KILLED `TestF2e_M2_WithKeys_*` |
| E9 | without keys, a run row with no import counts | KILLED (round 2) `TestF2e_M2_UnbackedRunRowIsNotEvidence` |
| E10 | gate error not classified as a precondition | KILLED `TestF2e_*` |
| F1 | SR5R-1 vote-time check dropped | KILLED `TestF3_SR5R1_*` (a vote would be recorded) |
| F2 | `AlreadyPosted` backstop dropped | KILLED `TestF3_SR5R1_BranchRefusesAnAlreadyPostedTombstone` |
| F3 | tombstone check ignores the provider | KILLED (round 2) `TestF3_SR5R1_AnotherProvidersTombstoneOnTheSameStringDoesNotBlock` |
| G1 | return evidence never clears | KILLED `TestF3_R9_*` |
| G2 | an older return clears | KILLED same |
| G3 | return amount not compared | KILLED same |
| G4 | return eligibility (D-4) ignored | KILLED `TestF3_R9_MockReturnEvidenceIneligibleOnceARealImportExists` |
| G5 | K2 recovery never clears | KILLED `TestF3_R9_ClearedByK2NetRecoveryOnly` |
| G6 | recovery `>` instead of `>=` | KILLED same |
| G7 | tombstone attributed to any request | KILLED (round 2) `TestF3_R9_OnlyRequestCorrelatedTombstones` (a request now exists) |
| G8 | raised without a succeeded line | KILLED `TestF3_R9_NoSucceededLine_NeverRaised` |
| G9 | raising requires an eligible line | KILLED `TestF3_R9_MockSucceededLineRaisesEvenWithARealImport` |
| G10 | R9 check not called | KILLED `TestF3_R9_*` |
| G11 | the original reference not added to the evidence keys | KILLED `TestF3_R9_*` |
| G12 | a return under another reference clears | KILLED (round 2, "other reference" case) |

### 5.1 Fix-round mutation pass

Method: the same as above (script `f2_mutate2.py`, private DB `f220261017024540p27680x02`). Mutants of the old fail-closed
cursor (A6) and of the run-bounded date rule are obsolete, because that code no longer exists.

Results: 26 mutants. R9 first failed to build and was rewritten. R15 survived the first pass and was killed after the new test
`TestF2_LFF2_5_OutOfCoverageLineDoesNotExempt`. **Final: 26 valid, 23 killed, 3 survived (R2, R16, R19), all justified
below.**

| id | mutant | result |
|---|---|---|
| R1 | cursor accepts any run seal | KILLED `TestF2_SRF2_1_*`, `TestF2_SRF2_2_*` |
| R2 | cursor skips the import-seal check of a run-seal-verified run | SURVIVED, equivalent for a SQL-only actor (see the note below the table) |
| R3 | cursor's import backing ignores source label / is_mock | KILLED `TestF2_SR11_10_SealedCursorIsPerSourceLabel` |
| R4 | anomaly set ignores rows newer by `run_at` | KILLED `TestF2_SRF2_2_*` |
| R5 | no run is ever trusted in sealed mode | KILLED `TestF2_SR11_10_*`, `TestF2_SRF2_*` |
| R6 | stall never reported | KILLED `TestF2_SRF2_1_HeldLockIsAVisibleStall` |
| R7 | skipped match not reported | KILLED `TestF2_SRF2_1_*` |
| R8 | the run seal is never persisted | KILLED `TestF2_SRF2_*`, `TestF2_SR11_*` |
| R9 | K3 line eligibility ignores the import seal | KILLED `TestF3_R9_WithKeysOnlyASealedReturnClears` / `TestF2fr_*` |
| R10 | K3 line read without the clock bound | KILLED `TestF3_R9_ForgedFutureReturnLineDoesNotSuppress` |
| R11 | persisted reversal accepted without import verification | KILLED `TestF2fr_WithKeysOnlyASealedDepositReversalClears` |
| R12 | F2c exemption ignores the amount | KILLED `TestF2_LFF2_5_ForgedUnsealedLineDoesNotExempt` |
| R13 | a `reversed` line exempts | KILLED `TestF2_LFF2_6_*` |
| R14 | F2c exemption ignores the import seal | KILLED `TestF2_LFF2_5_*` |
| R15 | F2c exemption ignores the import coverage | KILLED (after the new test) `TestF2_LFF2_5_OutOfCoverageLineDoesNotExempt` |
| R16 | current import's reversal pre-scan without `currentImportOK` | SURVIVED, equivalent in the stream path (see the note below the table) |
| R17 | M2 gate accepts any non-empty run seal | KILLED `TestF2e_*` (copied/forged rows) |
| R18 | M2 failure-after check without the future bound | KILLED `TestF2e_LFF2_3_*` |
| R19 | M2 gate skips the import-seal check | SURVIVED, equivalent for a SQL-only actor (as R2) |
| R20 | `Request` does not pre-read the evidence | KILLED `TestF2e_*` (refused as unread) |
| R21 | `Decide` does not pre-read the evidence | KILLED `TestF2e_M2_Execution*`, `TestK3_C4_M2DeclarePaid_FromAmbiguous` |
| R22 | a missing pre-read admits | KILLED `TestF2e_SRF2_5_*` |
| R23 | runs never sealed at write | KILLED `TestF2_SRF2_*`, `TestF2_SR11_*` |
| R24 | cursor reads only the first page | KILLED `TestF2_SRF2_4_*` (2000-row flood) |
| R25 | reversal lookup without the clock bound | KILLED `TestF2fr_FutureDatedReversalNeverClears` |
| R26 | K3 lines without coverage / clock bound | KILLED `TestF3_R9_*` |

Why the three survivors are equivalent:

- **R2 and R19.** The run seal binds the import id and that import's coverage. Only the key holder can produce a run seal,
  and the key holder also seals the import. Import rows are append-only, and their lines are immutable with the count
  enforced at insert (A8). So for a SQL-only actor, a verifying run always names a genuine sealed import. The import-seal
  check is kept as defence in depth.
- **R16.** The matcher is given only the import id that the stream wrote and sealed in the same sweep. A SQL-only actor cannot
  redirect it, so `currentImportOK` is always true on that path. The check is kept as defence in depth.

## 6. Residuals and open items

1. **SR11-1 (fix round): the SQL-only half is closed.** This is the original task's half: an actor with SQL access, but not
   the application's key, cannot blind or steer the stream. The original record said "mitigated", and its residual 2 was
   understated (see 2.1).
   - The run seal (0168) closes the probes that used to defeat it. Each is now refused and loud, with tests: SRF2-1 / LFF2-1
     (held advisory lock plus a forged run at a genuine import's end) and SRF2-2 / LFF2-2 (a copied run row with
     `run_at = now()`, for both the cursor and the M2 gate).
   - Skip and stall are audited and alerted.
   - A flood cannot stop the stream.

   What is NOT closed, and is out of scope of any seal:
   - an attacker running code in the application process (holds the key: the T6 class);
   - deletion of genuine rows (the tables are append-only for the runtime role, but this is a database-privilege matter);
   - a non-production process without keys, which still uses the A11 rule.

   SR11-1 option (b), the lookback re-match, was not built: the run seal makes it unnecessary for the SQL-only threat.
2. Without import keys (non-production) the cursor uses the A11 rule. Staging has no `PAYOUT_INSTRUMENT_KEYS` today (the
   acceptance check skips on it), so staging's MOCK stream runs in that mode. Production cannot (startup gate).
3. LF-A11-1: mutant C5 (ledger-join (a) windowing) is not killed by a dedicated test (section 5). `lastChangeAt` uses
   `resolved_at` first; a succeeded attempt always has it.
4. LF-A11-2/6: the real settle lag, record-time dating (contract item 7) and server-side paging stay PROVIDER DEPENDENT
   (conformance-test items for the first real source). The 15-minute default is a placeholder, not a vendor fact.
5. F2e: the gate is Go-only (the seal cannot be verified in SQL; the registered-source rule next to it is Go-only too).
   (Fix round: the evidence is now read BEFORE the M2 transaction opens, so there is no second pooled connection inside it;
   this sentence used to say the opposite.) Between the pre-read and the commit there is a window of milliseconds in which a
   new `sweep_run_failed` would not be seen. The next M2 sees it. The staff read model's `admissible_kinds` (ADR 0114) is shape-based and does not reflect the gate: a listed kind can
   still be refused with 409. A `cursor_anomaly` does not block M2 (otherwise a SQL-only forger with a far-future unbacked row
   could deny M2 forever); failure, truncation and staleness do.
6. F3b: off-platform recovery has no clearing path (M2 (d) precedent). The PSP "return" is modelled as a `reversed` payout line
   on the original reference; a real PSP may report returns differently (PROVIDER DEPENDENT, A5 R6). A return reported under
   the payload's `return_reference` is not read.
7. F3a: a refused payout stays `submitted` with its hold and a foreign tombstone on its reference: an integrity case (409
   "investigate"); resolution through M2/M4 per its shape or the provider's own outcome.
8. (Fix round) Migration 0168 is used (the run seal). 0167 changes one CHECK, and its down refuses while findings exist
   (WD099). 0168's down refuses while sealed runs exist.
9. The full `./internal/payments` package was not run end to end in one invocation (stopped by the harness limit; see 4.1).
10. (Fix round, NEW, for ledger-finance + security) The SQL function `payment_attempt_open_exposure` (MA020, migration 0119)
    reads persisted reversal lines WITHOUT the seal / coverage / clock rule (a database function cannot verify an HMAC). Go
    is now stricter than SQL only when a line is forged or out of coverage. The parity test (genuine fixtures only) passes.
    A SQL-only actor can still plant a reversal line to make the SQL MA020 exposure read as closed. This is pre-existing,
    not introduced here. Closing it needs either a sealed "verified line" marker the SQL can trust, or moving the check to Go.
    That is an architect + security decision, and it is not built.
11. (Fix round) The anomaly read is `count(*)` plus at most 1000 rows, and the cursor read is a linear scan over forged
    sealed-looking rows (one HMAC each). A very large flood costs time, but it cannot change the result. If the flood ever
    makes the read too slow, removing the forged rows is an owner-authorised, audited procedure (runbook). It is never a SQL
    edit and never an agent action.
12. (Fix round) Clock bound: a genuine line from a provider whose clock runs more than 5 minutes ahead is not clearing evidence
    until the database clock catches up. It never raises falsely, and it clears on a later run.

## 7. Probes for security review

1. **SR11-1 exact probe** (`TestF2_SR11_1_ForgedImportAndRunAsRuntime_StreamNotBlinded_AnomalyFires`, runs as `igaming_runtime`,
   asserted with `current_user`): in a tenant session forge an import shaped like the stream's own (MOCK label, `is_mock`,
   `imported_by_service`, 0 lines) unsealed / with a made-up seal and the real kid / with a seal COPIED from a genuine import,
   plus a run row with the same `coverage_end`, before each of two sweeps. Expected and observed: the A11-mode cursor moves to
   the forgery (control); the sealed cursor does not; the deposit made after the genuine cursor is imported; a new durable
   `cursor_anomaly:n<k>:<digest>` P1 per new forgery and an audit row per sighting.
2. (Fix round) Can forged rows exhaust, slow down or stop the cursor read? There is no fail-closed state: keyset pages with
   one HMAC per row, and an anomaly read of `count(*)` plus at most 1000 rows
   (`TestF2_SRF2_4_ForgedRunFloodNeitherMovesNorStopsTheStream`, 2000 rows, as `igaming_runtime`). Line reads stay bounded
   (`LIMIT MaxPaymentStatementLines+1`). The old 64-bound fail-closed test was replaced (section 3 item 7).
3. Can production run with a non-verifying sealer? (`VerifyPaymentCursorSealing`, static pin of its position in `run()`.)
4. `verifyImportSealInTx` vs `payoutinstrument.VerifyStatementImportInTx` parity (`TestF2_VerifyImportSealParityWithPayoutInstrument`);
   reconciliation holds no key (interface only).
5. SR11-11: any change in the set of unbacked rows gives a new discriminator; always within the 160-character CHECK.
6. F2e: can a SQL-only actor mint M2 evidence?
   - Without keys (non-production only), a run row needs a backing import (`TestF2e_M2_UnbackedRunRowIsNotEvidence`). A copied
     run row WOULD still count there.
   - With keys, the run seal AND the import seal must verify (`TestF2e_M2_WithKeys_OnlyASealedRunIsEvidence`). Fix round:
     `TestF2e_SRF2_2_CopiedRunRowNeverMakesTheEvidenceCurrent` copies the newest sealed run with `run_at = now()` after a
     failure, after staleness, and after truncation (the truncated copy with `window_truncated = false`). All three stay
     refused.
   - A forged FAILURE row only refuses, and since LFF2-3 a failure row dated more than 5 minutes ahead is ignored
     (`TestF2e_LFF2_3_FutureDatedFailureRowDoesNotBlockForever`, including the skew-tolerance control).
   - Closed token 409 + denied audit row over HTTP (`refused_when_reconciliation_not_current`).
   - Pre-read: `TestF2e_SRF2_5_EvidenceIsReadBeforeTheTransaction` (AST pin; a missing pre-read refuses).
8. **(Fix round) SRF2-1 / LFF2-1 exact probe** (`TestF2_SRF2_1_AdvisoryLockOrphanPlusForgedRun_RefusedAndLoud`, 2 variants:
   unsealed forged run and made-up seal): as `igaming_runtime`, hold `pg_advisory_lock(hashtextextended(
   'reconciliation:payment_statement:'||tenant, 0))` in a separate session during a sweep (the match skips; a genuine sealed
   import is left above the cursor), then forge one run row at that import's `coverage_end`.
   - Expected and observed: the cursor does not move, and the next sweep re-fetches from the genuine cursor and matches the
     deposit.
   - The forgery is reported (`cursor_anomaly`).
   - The skip is audited (`payment_match_skipped`).
   - `TestF2_SRF2_1_HeldLockIsAVisibleStall`: a lock held past `matchStallAfter` raises `match_stalled` with an audit row.
9. **(Fix round) SRF2-2 for the cursor** (`TestF2_SRF2_2_CopiedRunRowIsNeverTrustedAndIsReported`): a run row copied from
   the newest sealed run (same seal, kid and import, new id and `run_at`) is never the cursor and is counted as unbacked.
10. **(Fix round) LFF2-5 / SRF2-3 forged lines**:
    - `TestF2_LFF2_5_ForgedUnsealedLineDoesNotExempt` (keyed and unkeyed; an amount-1 line and an unsealed line);
    - `TestF3_R9_ForgedFutureReturnLineDoesNotSuppress` (a return line dated 50 years ahead);
    - `TestF3_R9_WithKeysOnlyASealedReturnClears`;
    - `TestF2fr_FutureDatedReversalNeverClears`;
    - `TestF2fr_WithKeysOnlyASealedDepositReversalClears`.
11. (Fix round) Key separation: the run label `b13-run-v1` is derived separately from the import label (`runseal_test.go`:
    tampering with every field fails; an import key never verifies a run seal). `TestF2_RunCanonCoversEveryField`.
12. (Fix round) Migration 0168: up/down/up snapshot, the CHECKs, the same-tenant FK, and the down refusing while sealed runs
    exist (`TestMigration0168_UpDownUp_Constraints_DownRefusesSealedRuns`). The database checks shape only. It never trusts a
    seal: verification is in Go.
13. **SR5R-1 exact probe** (`TestF3_SR5R1_PreExistingTombstoneOnThePayoutReference_Refused`; was item 7): a deposit-style
    tombstone (fresh correlation, key `tombstone:<provider>:<ref>`) is placed on a pending payout's reference, then two grant
    holders decide. Result:
    - both are refused (typed, 409);
    - zero decisions are recorded and zero request-correlated tombstones exist;
    - the payout is still submitted with its hold.

    Backstop: `TestF3_SR5R1_BranchRefusesAnAlreadyPostedTombstone`. Specificity: another provider's tombstone on the same
    string does not block.

## 8. Probes for ledger-finance review

1. **LF-A11-1 exact probe** (`TestF2_LFA11_1_TruncationBoundary_SettledAttemptsAreNotFalselyMissing`): three deposits sent after
   the cursor, the provider settles all three, then the platform records them; bound 1 line per run: no run raises
   `pay_missing_provider_record` (pre-F2 the first truncated run raised two: mutant C1). "Never silently missing":
   `..._SucceededWithNoProviderRecordIsStillReported`, `..._PendingOnlyAtTheProviderIsStillReported`; legacy:
   `..._LegacyModeStillWindowsBySentAt` plus the unchanged pre-existing suite.
2. Is `COALESCE(resolved_at, updated_at)` the right platform record time, and should the persisted-line exemption accept a
   `reversed` line (it records that the capture happened)?
3. Completeness horizon `CoverageEnd <= min(sweep end, now - lag)`; skip (no clean claim, no failure) when the horizon has not
   passed the cursor. Is 15 minutes acceptable as the placeholder default for a real source?
4. F2e evidence rule: newest stream-recorded run (sealed when keys), not truncated, no later `sweep_run_failed` for the provider,
   <= 6 h. Is 6 h right relative to the hourly sweep and `PaymentStatementBacklog`? M4 deliberately not gated.
5. F3b clearing: (1) eligible `reversed` payout line on exactly the original reference, same amount and asset, not older than
   the newest succeeded line; (2) K2 net recovery under the request's `withdrawal_failed`. Raising: any import, any amount.
   Tests `TestF3_R9_*`: never raised without a succeeded line; stands across runs; older / other-amount / other-asset /
   other-reference returns do not clear; a MOCK return is ineligible once a real import exists; a newer succeeded line
   re-raises; partial K2 recovery does not clear, full does; a foreign tombstone is never attributed.
6. No new ledger write path: F2 adds none; F3a only refuses (rolls back) the existing branch; F3b is read plus findings.
   `w.assertInvariants()` runs in the F3 tests. (Fix round: still true. The run seal adds columns to `reconciliation_runs`
   only, and no ledger table or posting path changed.)
7. (Fix round, answer to item 2 / LFF2-6) A `reversed` persisted line no longer exempts. It is reported as
   `pay_status_mismatch` (`TestF2_LFF2_6_ReversedPersistedLineIsAStatusMismatch`). An exemption needs a usable `succeeded`
   line with the exact amount and asset; otherwise the finding is `pay_amount_mismatch` / `pay_asset_mismatch`.
8. (Fix round) Clearing evidence across K3 / M4 / R9 / reversals / F2c requires all of: a verifying import (with keys), the
   line in coverage, and the line not dated in the future (clock + 5 min; F2c uses the run's coverage end). Is the 5-minute
   tolerance (`PaymentCoverageMaxClockSkew`, already the coverage skew) acceptable? Residual 6.10: the SQL MA020 exposure
   function does not apply this rule.
9. (Fix round, LFF2-3) M2 unblocks on the next sealed, successful, non-truncated run at most 6 h old with no failure row after
   it (future-dated rows beyond 5 min are ignored). Runbook step 8 states this.

## 9. Proposed registry / ADR / runbook text (for the orchestrator)

(Corrected in the fix round. The first version of this section overstated SR11-1 ("no longer blinds the stream") and
understated residual 2. The text below replaces it.)

- **SR11-1** -> `CLOSED (F2 + fix round, IMPLEMENTED against MOCK)`. Proposed registry text: "The SQL-only blinding of the
  payment_statement stream is closed. The application seals the import (0125) and the run row (0168, B13 label
  `b13-run-v1`). The cursor and the M2 gate trust only a run whose run seal and import seal verify. The SRF2-1 / LFF2-1
  probe (held advisory lock + forged run at a genuine import's end) and the SRF2-2 / LFF2-2 probes (copied run row) are
  refused and loud. A skipped match is audited and a stalled one raises the durable P1 `match_stalled`. A flood cannot stop
  the stream. Production refuses a registered source without both verifiers. Residual (out of scope of any seal): an
  attacker running code in the application process (key holder), and non-production processes without keys (A11 rule)."
  Condition: the probes pass in 4.3 (they do).
- **SR11-10** -> `IMPLEMENTED (against MOCK; F2 + fix round run seal)`. **SR11-11** -> `CLOSED (F2)`.
- **SRF2-1..5 / LFF2-1..7** -> addressed in the fix round (2.8). Proposed: CLOSED, pending the reviewers' re-check of this
  record.
- **LF-A11-1** -> `CLOSED as an engineering item (F2, IMPLEMENTED against MOCK)`; residual C5 disclosed; the exemption is
  sealed, coverage-bounded and exact on amount and asset (LFF2-5/6).
- **LF-A11-2** -> `PARTIALLY IMPLEMENTED (F2)`: settle lag + horizon enforced, contract updated; real lag and record-time
  dating PROVIDER DEPENDENT. **LF-A11-6** -> `PARTIALLY IMPLEMENTED (F2)`: page order enforced; provider-side paging PROVIDER
  DEPENDENT.
- **H-W1 second half / LF-A11-8** -> `CLOSED as an engineering item (F2e + fix round, IMPLEMENTED against MOCK)`: sealed run
  evidence, copied rows refused, future-dated failure rows ignored, evidence pre-read outside the M2 transaction. Residuals
  6.5.
- **SR5R-1** -> `CLOSED (F3a)`. **A5 R9 / LF-A5-1 (b)** -> `CLOSED as an engineering item (F3b + fix round, migration 0167,
  IMPLEMENTED against MOCK)`: clearing lines are sealed, in coverage and not future-dated. R6 (real PSP return reporting)
  stays PROVIDER DEPENDENT.
- **NEW (proposed)**: MA020 SQL exposure function reads unverified reversal lines (residual 6.10): OPEN, MEDIUM, architect +
  security + ledger-finance; pre-existing.
- Migration numbers: 0167 and 0168 used. 0169 stays reserved and unused.
- ADR 0095 addendum (text only): "F2: the payment_statement cursor is sealed: both the import and the run row carry an
  application HMAC (SR11-10, 0168), and only verifying runs move it. Continuation statements are bounded by the provider's
  settle-lag horizon and by time-ordered pages. Bounded-mode matching windows attempts by their last change. Clearing
  evidence must be sealed, in coverage and not future-dated. M2 is admitted only while the provider's reconciliation is
  current. F3: the payout reversal tombstone branch refuses a reference that already carries a tombstone, and
  `pay_payout_reversal_tombstone_but_paid` is the branch's standing finding."
- ADR proposal (B13 key module, security + architect): "new HKDF label `b13-run-v1` for payment_statement run seals; same
  master keys and kid as imports; rotation as for imports (a retired kid the process no longer holds makes its runs
  unverifiable, the cursor falls back to an older verifying run, one re-fetched window)."
- ADR 0101 addendum (text only): "M2 precondition (F2e): the provider's payment_statement reconciliation is current (newest
  SEALED run succeeded, not truncated, no later failure (failure rows more than 5 min in the future ignored), <= 6 h;
  sealed evidence when the process has keys). The evidence is read before the M2 transaction, at submission and execution
  (`reconciliation_not_current`)."
- Runbook: applied in `docs/runbooks/observability-and-alerting.md` (`cursor_anomaly` rewritten, new `match_stalled`, the
  `ErrPaymentCursorUnverifiable` bullet removed, forged-row removal as an owner-authorised audited procedure only, step 8:
  what unblocks M2). Also `docs/architecture/reconciliation-model.md` item 14 and `docs/plans/a11-payments-wiring-record.md`
  section 3 item 9. Proposed addition for `pay_payout_reversal_tombstone_but_paid` (reconciliation kinds section of
  `operational-runbooks.md`): "P1. The PSP reports as paid a payout whose hold the reversal tombstone branch released (possible
  double payout). Ask the PSP for the return; a `reversed` statement line on the original reference clears it; otherwise recover
  from the player through K2 (compensating_entry debit, causation = the request's withdrawal_failed transaction); an off-platform
  recovery is tracked through the mismatch's investigation status."

## SRF2. Independent security review (`security`, 2026-10-17, head `a7c02e4`)

Scope: F2a, F2b, F2e, F3a, F3b and migration 0167 at `a7c02e4` (`git diff origin/claude/focused-wright-jw88w9...a7c02e4`), against
section 7 and the SR11-1 / SR11-10 / SR11-11 history in `docs/plans/a11-payments-wiring-record.md`. Code-level and design-level
review plus probes on a private scratch database (`f2s20261017022043p11364x01`, built with `priv_db.sh` from a `git archive` of
`a7c02e4`, dropped afterwards). All attacker writes ran as `igaming_runtime` (asserted with `current_user`), except the two
F2e probes, which used the K3 world's tenant session (owner role); the same two INSERTs are proven to succeed as
`igaming_runtime` by the reconciliation probes. No role, password or shared infrastructure was touched. The probe files were not
added to the branch; copies are in the reviewer's session scratchpad (`srf2_probes/`). Not in scope: penetration testing, real
PSP behaviour, the full `./internal/payments` package, F2c/F2d financial semantics (owned by `ledger-finance`).

Re-run here: `./internal/reconciliation` `-run 'TestF2|TestBound_|TestScoped_|Migration0167'` (integration): 40 top-level PASS,
0 FAIL. `./internal/payments` `-run '^Test(F2e|F3)'`: 19 PASS, 0 FAIL. `./internal/httpserver` `-run 'TestForceResolutionAPI_M2|PayoutReversal'`:
3 PASS (including `refused_when_reconciliation_not_current`), 0 FAIL. Unit `TestF2|TestReservedUnused` in `cmd/platform-api`, `internal/db`,
`internal/reconciliation`: ok.

### SRF2.1 Findings

| ID | Severity | Finding | Required action |
|---|---|---|---|
| SRF2-1 | **HIGH** (part of SR11-1; binding before the first non-MOCK statement source; not freeze-blocking for a MOCK-only freeze once the wording is corrected) | **A SQL-only attacker can still silently and deterministically blind chosen payment_statement windows. Residual 2 (2.1) is not a rare race. The attacker can produce it on demand.** Any role may take advisory locks. As `igaming_runtime`, hold `pg_advisory_lock(hashtextextended('reconciliation:payment_statement:<tenant>', 0))` during a sweep. The genuine run fetches and commits a SEALED import (phase 2), then `pg_try_advisory_xact_lock` fails and the match is SKIPPED as "lock contention" (no failure, no alert). Release the lock, then INSERT one run row whose `coverage_end` equals that orphan import's. The sealed cursor now trusts it (the backing import is genuine and verifies), `unbacked = 0`, and no `cursor_anomaly` fires. The next sweep continues past the window. **Probe (run, confirmed: `TestF2S_Probe_AdvisoryLockOrphanThenForgedRunBlindsAWindow`).** A provider line (a succeeded deposit with no platform record) dated in the window gives `pay_missing_platform_record` = 1 in the control. Under attack it gives 0: sweep 1 `skipped=true`, sweep 2 clean, 0 anomaly audits, 0 anomaly alerts. Holding the lock across several sweeps orphans consecutive windows, and one forged row then skips all of them. Only `ReconciliationStale` (2 h without a success) bounds this, so a window of up to about 2 h can be blinded per cycle, repeatably. The race variant also works without the lock: a run row INSERTed between a run's ingest commit and its match makes the genuine match a stale-cursor loser. The simple variant was also confirmed (`TestF2S_Probe_OrphanSealedImportPlusForgedRunMovesCursor`): cursor moved to the orphan's end, silently. What F2a does close (confirmed by rerunning the authors' probes and by mine): forged imports (unsealed, made-up seal, copied seal, other label, other `is_mock`, other keys); tampering with genuine rows (appending a line to a sealed import is refused by 0102's count trigger; UPDATE/DELETE of imports gives `42501`; runs are append-only, `P0001`); other tenant and other provider (RLS plus seal binding). | (a) Correct the text: 2.1 residual 2 ("at most that one fetched window") and 6.1, and the proposed registry text in section 9 ("SQL-only forgery of run plus import no longer blinds the stream" is false). Suggested wording: "F2 closes forged-IMPORT blinding. A forged RUN row naming a genuine sealed import that never got a committed run (manufacturable on demand by holding the stream's advisory lock, or by racing ingest and match) still silently skips that window, repeatably." SR11-10 may be recorded as IMPLEMENTED (the step it asked for), but NOT as closing the SQL-only half of SR11-1. SR11-1 stays OPEN HIGH. (b) Cheapest real fix (recommended over the lookback): **seal the run row.** Add an HMAC under a new B13 key-derivation label over (run id, tenant, provider, stream, coverage_start, coverage_end, import id, window_truncated, run_at), written in the match transaction. The cursor, the stale re-check and the F2e gate then trust only runs whose seal verifies AND whose import id's seal verifies. A forged run row can then never become the cursor, whatever imports exist. Verification needs no line reads, so the 64-candidate bound can grow. This needs an architect + security decision (new key label and columns). (c) Also make a skipped match loud (lock not acquired, or stale) when it leaves a sealed import with no run: count it, and alert when it repeats, so a held lock becomes a visible stall instead of a quiet one. The lookback re-match (SR11-1 option b) only delays detection and costs more; keep it as defence in depth for the in-process key holder. |
| SRF2-2 | MEDIUM (binding before the first non-MOCK statement source; not freeze-blocking) | **The F2e gate's freshness, failure and truncation conditions can be bypassed with SQL alone.** The gate trusts `reconciliation_runs.run_at` and `window_truncated`, which the runtime role writes freely. Only the backing import is sealed. A fresh run row (`run_at = now()`, `window_truncated = false`) whose `coverage_end` names ANY genuine sealed import (an old one is fine) becomes the "newest verified run". It sits at or below the cursor, so it raises no `cursor_anomaly`. It hides a later `sweep_run_failed`, staleness, and a truncated last run. **Probe (run, confirmed: `TestF2S_Probe_ReplayedSealedImportMakesFailedRunLookCurrent`, `..._StaleRunLookCurrent`, with keys):** the control refuses (`ErrResolutionReconciliationNotCurrent`), and after the replayed row the M2 request is ADMITTED. So the claim in 2.5 / 7.6 that "a SQL-only forger cannot mint the evidence" holds only for the import, not for currency. Impact is bounded: it also needs the four-eyes M2 approvals, and it only removes the "stream is working" precondition. It does not forge a resolution. | (a) Qualify 2.5 and 7.6: the gate proves a genuine sealed import exists. It does not prove currency against a SQL-capable attacker until runs are sealed. (b) The run-row seal of SRF2-1(b) closes it. Cheap interim fix: base staleness and "failed after" on the backing import's `fetched_at` (inside the seal) instead of `r.run_at`. Truncation still needs the run seal. |
| SRF2-3 | MEDIUM (process; binding before the first non-MOCK source) | **Clear-eligibility (D-4 / RC-3) is not seal-verified.** F3b inherits this from K3. `clearEligible` checks only `is_mock` against the existence of a real import. As `igaming_runtime`, a forged unsealed `is_mock = false` import carrying a `reversed` payout line on the original reference, with the same amount and asset and dated after the newest succeeded line, would CLEAR `pay_payout_reversal_tombstone_but_paid` (a possible double payout, P1). The same forgery flips `hasRealImp`, which disqualifies genuine MOCK clearing evidence. Found by code reading, not probed. It is the same pre-existing class as the M2 standing kinds (ADR 0101). F3b adds a new P1 to it but no new path. | Before the first non-MOCK source, in a process with import keys, a line may CLEAR or CONFIRM only if its import's seal verifies (`verifyImportSealInTx` now exists in this package, so this is cheap), and `hasRealImp` must count only verifying imports. Register it with SR11-1. |
| SRF2-4 | LOW | **Forged run rows can degrade the stream (DoS).** (i) Fail-closed flood: 64 forged run rows with distinct `coverage_end` values above the cursor fail the pair's run on every sweep (`ErrPaymentCursorUnverifiable`). This is loud (P1 failed run, `ReconciliationStale`, and M2 refused via F2e), which is the right choice over a silent zero cursor. But runs are append-only, so recovery needs an owner-level procedure, and the runbook does not describe one. (ii) `unbackedAbove` reads EVERY run row above the cursor with no LIMIT. Rows dated in the future are never candidates, so they never trip the fail-closed path. **Probe:** 200 000 such rows took the sweep for the pair from 74 ms to 1.2 s, linear in rows and in memory (`TestF2S_Probe_FutureFloodCost`). (iii) Alert flooding is bounded: at most one new `cursor_anomaly` discriminator per pair per sweep. | Compute the anomaly count and digest in SQL (for example `count(*)` plus `sha256` over `string_agg(id::text ORDER BY id)`), or cap the read. Add the forged-row recovery procedure to the runbook (owner-level, audited, never an agent action). With run seals (SRF2-1 b) the candidate set becomes sealed runs only, so a bare run-row flood can no longer exhaust it. |
| SRF2-5 | LOW | **Second pooled connection inside the open M2 transaction.** The F2e evidence read takes a second pooled connection while the M2 request or decision transaction holds one. With as many concurrent M2 calls as the pool has connections, every call waits on the pool until its context deadline. This is staff-only and needs the force-resolve grants, so the risk is low but real. Error handling is fine: a closed 409 token plus the denied-audit row, with the reason only in the audit detail. The existing `noLeakage` test covers it. A forged failure row only refuses (fail closed). | At submission, read the evidence BEFORE opening the M2 transaction (execution re-checks anyway). At execution, read it before the transaction and re-check inside it with a cheap comparison. Or give the evidence read its own small dedicated pool. |
| SRF2-6 | LOW | **Staging never exercises the production path.** Staging has no `PAYOUT_INSTRUMENT_KEYS`, so its stream uses the A11 (forgeable) cursor and the F2e gate admits unsealed evidence. Production cannot reach that mode. Verified: `GuardEnvironment()` treats an unset or implicit `APP_ENV` as production; `VerifyPaymentCursorSealing` runs right after `SetDefaultImportSealer` and before `RunSchedulerLoopScoped` (static pin test); `payoutinstrument.NewService` refuses nil keys, so there is no typed-nil verifier hole. | Provision import keys in staging (synthetic) before the first real source so that staging acceptance runs the sealed cursor and the keyed gate. Consider applying `VerifyPaymentCursorSealing` in staging too. |

### SRF2.2 Verified without findings

- Seal verification. The compare is constant time (`hmac.Equal`). The seal binds import id, tenant, provider, label, `is_mock`, coverage
  window, content digest, `line_count`, `fetched_at`, importing service and the digest of ALL stored lines. Lines are read with
  `LIMIT MaxPaymentStatementLines+1` plus a count check, and appending after sealing is refused by the trigger (probe). Package
  `reconciliation` holds only the `ImportSealVerifier` / `ImportSealKeys` interfaces, so it has no key material. Seals, keys and verifier
  errors are never logged (a failed verify is collapsed to `errImportSealMismatch`).
- F2b. The discriminator tail is `cursor_anomaly:n<count>:<16-hex>`, with the provider shown via the `discPart` charset check. Attacker
  influence is limited to a count and a digest, with no free text. The audit row `reconciliation.payment_cursor_anomaly` carries counts,
  a run id, the digest, the cursor and a provider id: no PII, no secrets.
- F3a. Authorization paths are unchanged. `ErrPayoutReversalReferenceTombstoned` wraps `ErrPayoutReversalAttemptInconsistent`, so the HTTP
  response is still 409. The vote-time check is specific to the provider (tested), and the `AlreadyPosted` backstop rolls the whole branch
  back.
- Migration 0167. It changes only the CHECK. The down migration refuses (WD099) while findings exist and restores FORCE RLS within its
  transaction.
- Changed assertions (section 3). None are weakened. `TestBound_ForgedCursorsAreIgnored` and `TestScoped_PerTenantIsolation` are stricter
  than before. The SR11 discriminator equality became a prefix match only because F2b adds the tail; the P1 assertion is unchanged.
  Observation: the pre-existing `TestBound_*` / `TestScoped_*` suite runs in the A11 fallback mode (no keys), while production runs sealed.
  Consider running it with keys as well.

### SRF2.3 Verdict

**NOT APPROVED as written at `a7c02e4`.** The blocking items are text only. The record and the proposed registry text overstate F2a
(SRF2-1 a) and F2e (SRF2-2 a). Once those corrections are made, the code is **acceptable for a MOCK-only freeze** and no code change is
required for it. **Launch flags (production with a non-MOCK statement source):** SR11-1 stays OPEN HIGH. It now includes SRF2-1, with the
run-row seal as the recommended next step (architect + security decision: new B13 key label and columns), together with SRF2-2 and
SRF2-3. Those three must be resolved before the first non-MOCK statement source. SRF2-4, SRF2-5 and SRF2-6 are LOW and recommended.
Registry: SR11-10 can move to IMPLEMENTED (F2a), but not to "SR11-1 SQL-only half closed". SR11-11 can be CLOSED. SR5R-1 can be CLOSED.

## LFF2. Independent ledger-finance review (`ledger-finance`, 2026-10-17, head `a7c02e4`)

Scope: F2a-F2e, F3a, F3b and migration 0167 at `a7c02e4`, against section 8, `docs/plans/a11-payments-wiring-record.md`,
`docs/plans/a5-payout-reversal-record.md` and `docs/architecture/reconciliation-model.md`. Read independently of SRF2. Overlapping
items are cross-referenced, not repeated. Harness: private scratch database `f2l20261017021852p10368x01` (`priv_db.sh` from the worktree,
migrations through 0170; dropped afterwards). `internal/payments` used its own per-test scratch databases. Attacker writes ran as
`igaming_runtime` (asserted with `current_user`). The exceptions are the payments-side probes, which used the K3 world's tenant session.
The same INSERTs (`audit_log` with an explicit future `created_at`, and `reconciliation_runs`) were proven to succeed as
`igaming_runtime` with psql inside a rolled-back transaction. No role, password or shared infrastructure was touched. The probe files were
not added to the branch; copies are in the reviewer's session scratchpad (`lff2_probes/`).

Re-run here:
- `./internal/reconciliation/...` (integration, `-v`): 354 PASS, 0 FAIL, 0 SKIP.
- `./internal/payments` `-run '^Test(F2e|F3|A5|T8|K3_C4|K3_C7|K3_C9|M2)'`: 59 PASS, 0 FAIL.
- `./internal/httpserver` `-run 'TestForceResolution|PayoutReversal|TestStaffRead|OpenAPI'`: 78 PASS, 0 FAIL.

### LFF2.1 Money path (no veto condition)

- No file under `internal/ledger`, `internal/wallet`, `internal/withdrawal` or `internal/adjustment` changed. There is no new ledger
  write path, no new posting shape, no floating point, no balance UPDATE and no historical-entry mutation.
- F3a only adds refusals. The vote-time check is tenant- and provider-scoped. The `AlreadyPosted` backstop returns an error, so the
  park, the E10 release and the audit row roll back with the transaction. Idempotency still rests on the existing
  `tombstone:<provider>:<ref>` key, enforced by the database.
- F2/F3b are reads plus mismatch rows. `w.assertInvariants()` runs in the F3 tests.
- The changed assertions in section 3 are equivalent or stricter. Nothing was weakened (agrees with SRF2.2).

### LFF2.2 Findings

| ID | Severity | Finding | Required action |
|---|---|---|---|
| LFF2-1 | MEDIUM (same item as SRF2-1; binding before the first non-MOCK source) | **Residual 2 confirmed, and it is SILENT.** Probe `TestLFF2_Residual_GenuineUnmatchedImportPlusForgedRun` (keys on). The stream's own fetch plus a sealed ingest from the genuine cursor, whose match never commits, followed by ONE forged run row (as runtime) at that import's `coverage_end`, has these effects: the sealed cursor moves, `unbacked = 0`, no `cursor_anomaly` audit or alert, and the next sweep succeeds from the forged end. The deposit's line is held only by the never-matched import. Financially, the lines of the skipped window are stored but never matched, so no amount, status or missing-platform finding is ever produced for them. Because F2c windows succeeded attempts by `lastChangeAt`, an attempt that settled inside the skipped window is never a "missing provider record" candidate either. Section 2.1 says a failed match "already raised its own P1". That is true for the failure, but nothing reports the SKIP, and a stale-cursor loser raises nothing at all. | Is the residual acceptable? **Yes for a MOCK-only freeze, no for a real source.** Keep SR11-1 OPEN (binding), with the wording correction of SRF2-1 (a). **Cheapest next step: the run-row seal** (SRF2-1 b). A single change closes LFF2-1, LFF2-2 / SRF2-2 and the exhaustion half of LFF2-4. The lookback re-match costs more and only delays detection. Optional interim detector, with no key change: report an anomaly when the chosen cursor's backing import has a `coverage_start` below the `coverage_end` of another verifying candidate (a fork). This catches the stale-loser variant only. |
| LFF2-2 | MEDIUM (same item as SRF2-2) | **The F2e currency evidence is forgeable.** Probe `TestLFF2_GateBypassedByForgedRunOverGenuineSealedImport`, keys on, three variants: failed, stale and truncated. Each control refuses. One run row copied from the newest sealed import (`run_at` = now(), `window_truncated = false`, no import forged) makes the M2 request ADMITTED in all three variants. | As SRF2-2. Interim, no schema change: (1) take staleness and "failure after" from the backing import's sealed `fetched_at`, not `run_at`; (2) require the evidence import to be the one that backs the SEALED cursor (`queryPaymentCursorState` with the same basis), so an old import cannot be replayed; (3) refuse when ANY run row with that `coverage_end` is truncated (the forger can only add rows). The run seal closes the rest. **H-W1 second half / LF-A11-8 must NOT be recorded CLOSED until this is fixed** (section 9 proposes CLOSED). |
| LFF2-3 | MEDIUM (new; fix before H-W1 / LF-A11-8 is closed) | **One future-dated failure row blocks M2 for the provider permanently.** `audit_log.created_at` is caller-settable (runtime can insert `created_at = now() + 10 years`; verified with psql as `igaming_runtime`, rolled back). The gate's `created_at > last.runAt` is then true for every later genuine run. Probe `TestLFF2_FutureDatedFailureRowBlocksM2`: after two genuine successful runs, M2 is still refused with `last_run_failed`. `audit_log` is immutable, so operations cannot remove the row. Nothing unblocks it short of a code change. This contradicts the requirement that staff cannot be blocked forever by an unrelated problem. | Add `AND created_at <= now()` to the failure probe, and compare against the sealed import `fetched_at` (LFF2-2). Document in runbook step 8 what unblocks M2: the next successful, non-truncated run of the provider (at most 6 h old) with no failure audited after it. While the stream fails closed (LFF2-4), nothing unblocks it. |
| LFF2-4 | LOW-MEDIUM (extends SRF2-4) | **The fail-closed cursor never recovers by itself, and the runbook says it does.** Probe `TestLFF2_FailClosedHasNoSelfRecovery`: 64 forged run rows (as runtime) with `coverage_end` between the genuine cursor and now produce `ErrPaymentCursorUnverifiable` on every sweep. Runbook `observability-and-alerting.md` (the `ErrPaymentCursorUnverifiable` bullet) says "the stream resumes when a genuine run row is again among the newest 64". That cannot happen, because no genuine run row is written while the read fails closed. Meanwhile, F2e refuses M2 for the provider. `unbackedAbove` is also unbounded: probe with 200 000 forged far-future rows gave 0.46 s per cursor read and 1.24 s per sweep, linear (agrees with SRF2-4 ii). Failing closed is the correct financial choice and is not contested. | Correct the runbook: recovery needs a governed, owner-level, audited procedure (for example, a migration-recorded exclusion of the named run ids). Never a SQL edit of the append-only tables, and never an agent action. Compute the anomaly count and digest in SQL, or cap the read. With run seals, unsealed rows no longer consume the candidate budget. |
| LFF2-5 | MEDIUM (new for F2c; extends SRF2-3 for F3b, with a probe; binding before the first non-MOCK source) | **The two new decisions trust unsealed persisted lines.** (a) **F2c exemption.** Probe `TestLFF2_ExemptionByForgedUnsealedLine`, stream keys on. A forged, UNSEALED MOCK import (as runtime) holds one `succeeded` deposit line on the attempt's reference, amount 1, dated outside its own coverage. It suppresses `pay_missing_provider_record` for a platform-succeeded deposit that the provider never recorded. That reopens "never silently missing" for a SQL-only actor. (b) **F3b clear path.** Probe `TestLFF2_R9SuppressedByForgedFutureReturn`. A forged unsealed `is_mock = true` import is eligible in a MOCK-only world, so `is_mock = false` is not even needed. It carries a `reversed` payout line on the original reference, with the right amount and asset, dated now + 50 years, and is planted BEFORE the PSP's line arrives. R9 is then never raised for a later genuine `succeeded` line, because every real succeeded line is older than the forged return. That is a pre-emptive, permanent suppression of a possible double-payout P1, not only a clear. The M4 verdict SQL already requires `import_seal IS NOT NULL` plus Go verification, so F2c and F3b are weaker than the M4 precedent. | When the stream has a verifier: (1) accept exemption and clearing lines only from imports whose seal verifies (`verifyImportSealInTx`, cached per import per run), and count only verifying imports for `hasRealImp` (with SRF2-3); (2) accept a line only if its `occurred_at` lies within its import's sealed `[coverage_start, coverage_end]`, which bounds the far-future forgery even before seals; (3) for the F2c exemption, also require amount and asset equality, otherwise report `pay_amount_mismatch` or `pay_asset_mismatch` instead of exempting. |
| LFF2-6 | LOW (answer to probe 8.2; no regression) | **Exemption semantics.** `COALESCE(resolved_at, updated_at)` is the right record time: every path to `succeeded` sets `resolved_at = now()` (`attempt.go`, `manual_resolution.go:1148`). One caveat: `now()` is the TRANSACTION START. A success transaction that opens before the horizon and commits after the run's match snapshot is dated into a window whose snapshot could not see it, and is never a candidate. The 15-minute real-source lag makes this negligible; with the MOCK lag of 0 it is possible but not realistic. On accepting `reversed`: it proves the provider holds a record. But "provider reversed, platform later recorded success" is a money discrepancy: the platform holds a credit the PSP clawed back. The exemption now drops it silently. The pre-F2 `sentAt` windowing missed it too, so this is not a regression. | Exempt on `succeeded` only. For a `reversed` persisted line plus a platform-succeeded attempt, report `pay_status_mismatch`. Optionally window by commit visibility (for example, a small negative margin on `coverage_start` in bounded mode). |
| LFF2-7 | LOW (same item as SRF2-5) | **Second pooled connection inside the M2 transaction.** At execution, `decideInTx` takes the second connection AFTER the L1 locks (withdrawal, attempt, resolution FOR UPDATE). This is not an ADR 0082 row-lock-order violation: the evidence read is read-only and takes no row or advisory locks, so Postgres sees no cycle. It is, however, an application-level hold-and-wait. If the pool fills with sessions waiting on those same rows (callbacks or polls for the attempt), the decision waits for a connection that only frees when those waiters time out. This is staff-only and rare. | Read the evidence before opening the M2 transaction. It already runs in its own snapshot, so nothing is lost. At submission, keep the guard-first ordering (security F-2/F-3) by applying the pre-read result after the INSERT. |
| LFF2-8 | INFO | **Answers to section 8.** (3) **Completeness horizon and lag:** sound. The lag is declared in source code (a review-controlled change) and validated to `[0, 7 d]` at startup and on every run. `PeriodEnd` is always set in continuation mode, so `validateBoundedWindow` cannot be bypassed by a source. Synthetic gets 0, and production refuses Synthetic. The MOCK conforms. The skip (no failure, no clean claim) is correct, but a lag that keeps the horizon behind the cursor for more than 6 h also makes F2e refuse M2 (acceptable). 15 minutes is acceptable only as a placeholder: replace it per vendor before the first real source (PROVIDER DEPENDENT). (4) **6 h:** right against the hourly sweep and `PaymentStatementBacklog`. A sustained backlog (every run truncated) blocks M2 until it drains, which is intended and should be stated in the runbook. Not gating M4: agreed (it rests on its own sealed evidence). `admissible_kinds` not reflecting the gate: acceptable, since the 409 token is closed. (5) **F3b:** tenant scoping is correct (tombstone, request and attempts are all tenant-joined; `netRecovery` is tenant-scoped). The false-P1 risk is low: raising from any import is right for a possible double payout. An off-platform recovery re-raises a NEW open row on every run, indefinitely (the M2 (d) precedent). Track it as a known operational cost. 0167 up/down: verified, and the down refuses with WD099 while findings exist. |

### LFF2.3 Verdict

**APPROVE for a MOCK-only freeze, with conditions. No ledger-finance veto applies:** the change has no money-path regression, no new
write path and no change to postings or idempotency.

Conditions:
1. **Registry and record text (blocking for the record, as SRF2.3).**
   - SR11-1 stays OPEN (binding before the first non-MOCK source) and includes LFF2-1, LFF2-2, LFF2-5.
   - **H-W1 second half / LF-A11-8 is NOT CLOSED** until LFF2-2 interim (1)-(3) and LFF2-3 are fixed.
   - A5 R9 / LF-A5-1 (b) is `PARTIALLY IMPLEMENTED` until LFF2-5 (b) is fixed.
   - LF-A11-1 may close as an engineering item, with LFF2-5 (a) and LFF2-6 as follow-ups.
   - Correct the runbook's `ErrPaymentCursorUnverifiable` recovery sentence (LFF2-4).
2. **Recommended in this branch** (small, Go-only, no schema): LFF2-3, LFF2-2 interim, LFF2-5 (2).
3. **Before the first non-MOCK statement source:** the run-row seal (architect + security decision; a new B13 key label and
   columns), LFF2-5 (1) and (3), LFF2-4 and LFF2-7.

## SRF2R. Security re-review of the fix round (`security`, 2026-10-17, head `6f07dfb`)

Same attacker model as SRF2: arbitrary SQL as `igaming_runtime`, without the application's key. The scratch DB
`f2s20261017040943p2418x01` was built from a `git archive` of `6f07dfb` (migrations through 0170, including 0168) and dropped afterwards.

**Re-run here:**
- My SRF2 probes, the LFF2 probes, and new SRF2R probes. The probe files were not added to the branch; they are described in
  the reviewer's scratchpad (`srf2_probes/SRF2R_README.txt`).
- Authors' suites:
  - reconciliation `-run 'TestF2|TestBound_|TestScoped_|TestMigration016|TestK3_C3|TestMA020'`: 64 PASS, 0 FAIL;
  - payments `-run '^Test(F2e|F3|F2fr)'`: 26 PASS, 0 FAIL;
  - unit tests in `payoutinstrument`, `cmd/platform-api`, `reconciliation`, `db`: ok.

### SRF2R.1 Closure checks (all confirmed by probe)

**SRF2-1 is closed.** Held advisory lock plus one forged run row at the orphan import's `coverage_end`:
- the cursor did not move;
- the next sweep re-covered the window: `pay_missing_platform_record` = 1, the same as the control;
- `cursor_anomaly` was audited and alerted.

A forged unsealed run row over a genuine sealed import that has no run row does not move the cursor (`unbacked` = 1).

**SRF2-2 is closed.**
- My replay probes (failed and stale) are now refused (`last_run_failed`, `last_run_stale`). The LFF2 probes for failed,
  stale and truncated are refused too.
- A genuine sealed run row copied VERBATIM, seal included, with `run_at = now()` is refused by the gate (`last_run_failed`).
- In the cursor, copies with a new id (with `coverage_end + 1 s`, or with `run_at = now()`) are not trusted and are reported
  (`unbacked` 1, then 2). Reusing the id fails on the primary key (`23505`).

**Run seal coverage.** The seal covers every column the cursor and the gate decide on:
- run id, tenant, provider, stream;
- `run_import_id`;
- the import's coverage window: start is read from `period_start`, end from `coverage_end`;
- `window_truncated`;
- `run_at`.

Unsigned columns:
- `status` and `period_end`: not read by the cursor or the gate. The staff read surfaces still display unsealed rows as runs.
  That is informational and not decisive.
- The `sweep_run_failed` audit rows: they can only refuse, and their future dating is now bounded to the DB clock plus 5 min.

Cross-tenant and cross-provider copies fail for two reasons: tenant and provider are inside the seal, and the FK
`(run_import_id, tenant_id)` requires the import to belong to the same tenant. Replaying an old genuine sealed row is
impossible: a new id breaks the seal, and the same id hits the primary key.

**Keys.**
- Separate HKDF info label `b13-run-v1`, plus the domain tag `run` in `RunCanon`, so run and import seals are not
  interchangeable.
- Constant-time compare (`hmac.Equal`).
- The reconciliation package sees only the `RunSealer` / `RunSealVerifier` interfaces.
- The production refusal now requires BOTH verifiers (`streamVerifier`), and its position in `run()` is still pinned.
- An unset `APP_ENV` is still treated as production.

**SRF2-3 is closed for the Go paths.** With keys, a line may clear or confirm only when ALL of these hold: D-4 eligible, its
import's seal verifies, it lies inside its import's coverage, and it is dated no later than the DB clock plus 5 min. This
covers K3, R9, deposit reversals and the F2c exemption (which uses the run's coverage end instead). The LFF2 probes
"forged unsealed line exempts" and "forged future return suppresses R9" are now refused.

**The 5-minute deviation is acceptable.** With keys, a SQL-only actor cannot produce a verifying line at all, so the clock
bound only limits a genuine provider whose clock runs ahead. Continuation imports end at `now - lag`, so their lines are
always in the past. The cost is a delayed clear, never a false clear (residual 6.12).

Note: `hasRealImp` (RC-3) still counts unsealed non-MOCK imports. A forged one can only make MOCK evidence ineligible, so it
fails closed (more findings, not fewer). In production every import is non-MOCK, so this has no effect there.

**SRF2-5 is closed.** The F2e evidence is pre-read before the M2 transaction, so there is no second pooled connection inside
it. The millisecond TOCTOU window is disclosed (6.5), and the execution step re-checks.

Minor: for platform staff, the pre-read (a system-view read of the target tenant) runs BEFORE the acting-session fence. Its
result never leaves the service unless the fence passes, so this is informational. It costs one evidence read per
approve, for every provider of the tenant.

### SRF2R.2 New findings

| ID | Severity | Finding | Recommendation |
|---|---|---|---|
| SRF2R-1 | LOW-MEDIUM (DoS by a SQL-only actor; loud only eventually) | **The flood cost is super-linear.** Sealed-looking forged run rows (random 64-hex seal, a real kid, any existing import id; all accepted by the 0168 CHECKs and FK) above the cursor were measured as follows. **Cursor read:** 2,000 rows took a sweep from 72 ms to 113 ms; 200,000 rows took it from 65 ms to **70.6 s**. That is 100x the rows for about 600x the time. The likely cause is the keyset `(coverage_end, run_at, id) < (...)`: no index matches it, so each 512-row page re-scans and sorts the flood. The read also runs a second time under the advisory lock inside the REPEATABLE READ match transaction. **M2 gate pre-read:** 52,000 rows cost **4.3 s per request**. It runs for every approve (any kind) and for every provider of the tenant, so a few hundred thousand rows push force-resolution requests and approvals past HTTP timeouts. Correctness holds: the cursor still advanced and nothing was trusted. "A flood cannot stop the stream" (2.8 item 2, 6.11) is true for correctness but not for liveness. Only `ReconciliationStale` (2 h) makes a stuck pair loud. If the sweep loop is sequential across tenants, one flooded pair also delays the other tenants. | Before the first non-MOCK source: (a) add an index matching each keyset, e.g. partial `(tenant_id, provider_id, coverage_end DESC, run_at DESC, id DESC) WHERE stream='payment_statement' AND run_seal IS NOT NULL`, plus a `run_at` variant for the gate; or read the candidates in ONE streaming query instead of re-issued pages. (b) Add a per-read budget (rows or time). When it is exhausted, fail THIS run loud (P1 `cursor_flood`, or the gate's `evidence_not_read`) rather than hang. That is not the old fail-closed state, because the next run retries. (c) Narrow the gate pre-read to the attempt's provider. Recovery stays the owner-authorised runbook procedure (6.11). |
| SRF2R-2 | LOW (operational: false P1) | **`match_stalled` also fires on benign multi-instance contention.** `platform_api_desired_count >= 1`, and every instance runs the scoped loop. A contention loser's sealed import usually ends a moment after the winner's run, because it fetched later. It then sits above the trusted cursor until the NEXT sweep, which (hourly) is past `MatchStallAfter` (30 min). So the next sweep raises a durable P1 `match_stalled` even though its own run re-covers and clears the window. Probe (with the stall threshold shortened): `payment_match_stalled` audits = 1 after a benign loser import. False P1s train operators to ignore the alert that exists to expose a held lock. | Count an import as stalled only when no verifying sealed run has `run_at` after that import's `fetched_at`. Any committed run since the fetch means the stream progressed and the orphan window has been, or is being, re-covered. A continuously held lock still trips it (no run commits). Pin it with a test. |
| SRF2R-3 (= residual 6.10) | MEDIUM (pre-existing since 0119; binding before the first real PSP / non-MOCK statement source; NOT freeze-blocking) | **The SQL MA020 guard `payment_attempt_open_exposure` trusts unsealed `deposit_reversal` lines.** It is the DATABASE-level preventive guard: 0115's `ledger_adjustment_payload_refusal` blocks a manual credit while a captured-but-unposted deposit is open. A SQL-only actor who plants an unsealed non-MOCK import with a completed `deposit_reversal` line naming the reference makes the exposure read as closed. That disables the preventive block, which allows a double credit: a manual credit, then the late posting or refund. This was reviewed from code; the record states it and my reading agrees. Remaining controls: the manual adjustment still needs staff, a reason code and four-eyes above the threshold, and the keyed Go reconciliation now ignores the planted line, so the detective standing finding (captured-unposted) stays raised. Net: a preventive control is defeatable by SQL, while the detective control holds. | Architect + security decision, before the first real PSP. Preferred option: make the SQL guard fail closed, with only ledger tombstones clearing in SQL and statement lines no longer clearing there. Then let a Go path that verifies the seal turn a verified completed reversal into a ledger-level clearing artifact (for example the existing tombstone mechanism), which the SQL guard can trust. A "verified" marker table written by the runtime role is NOT acceptable: it is as forgeable as the line. Register it as its own OPEN row (MEDIUM). |

### SRF2R.3 Verdict

**APPROVED for the MOCK-only freeze at `6f07dfb`.** SRF2-1, SRF2-2, SRF2-3 (Go paths) and SRF2-5 are closed, with the
probes re-run and the authors' suites green. SRF2-4 is superseded by SRF2R-1. SRF2-6 (staging has no keys) remains LOW and
open.

**SR11-1 can be CLOSED for the freeze**, scoped to the SQL-only threat it was raised for: "CLOSED (F2 + fix round, run seal
0168, IMPLEMENTED against MOCK): an actor with runtime SQL access but without the key can no longer blind, steer or fake the
currency of the payment_statement stream." Explicitly outside it, and tracked elsewhere:
- an attacker inside the application process who holds the key (T6 class);
- deletion by privileged roles;
- non-production processes without keys.

**Launch flags (first real PSP / non-MOCK statement source):**
- SRF2R-1: index or streaming read plus a per-read budget;
- SRF2R-3 / 6.10: SQL MA020 guard (MEDIUM, own OPEN row);
- SRF2R-2: recommended before production alerting goes live;
- SRF2-6: staging import keys.

None of these block the freeze. This review covers code and design plus targeted probes against MOCK. It is not a
penetration test, and it does not claim real-PSP behaviour.
