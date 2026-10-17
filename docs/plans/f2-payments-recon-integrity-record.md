# F2 + F3 record: payment-statement stream integrity and payout-tombstone follow-ups

Branch `f2-payments-recon-integrity` (base `origin/claude/focused-wright-jw88w9` at `1f833ec`). Date 2026-10-16. Author:
`ledger-finance` (implementation). Work items F2 and F3 of `docs/plans/platform-freeze-work-list-2026-10-16.md`.
Everything here is `IMPLEMENTED` against the local database and MOCK providers only. Real statement ingestion stays
`PROVIDER DEPENDENT`. Nothing here claims real-PSP readiness, a licence position or a vendor contract.

Not edited (orchestrator single writer): `docs/governance/task-registry.md`, HANDOVER, progress, active-stage, the decision
register, ADRs. ADR / registry / runbook proposals are in section 9.

## 1. Deliverables and labels

| # | Item | Origin | Label |
|---|---|---|---|
| F2a | Sealed cursor: the payment_statement cursor trusts only a run backed by a SEALED import of the same source whose HMAC seal verifies | SR11-1 via SR11-10 | IMPLEMENTED (MOCK); production startup refuses a registered source without keys |
| F2b | `cursor_anomaly` discriminator carries the unbacked-row count and a digest of their ids; audit row per sighting | SR11-11 | IMPLEMENTED |
| F2c | Bounded-mode "missing provider record" (and ledger-join (a)) windowed by the attempt's last change, with a persisted-line exemption | LF-A11-1 | IMPLEMENTED (MOCK); legacy mode unchanged |
| F2d | Per-provider settle lag (source declaration + safe default), completeness horizon `PeriodEnd = now - lag` enforced, time-ordered pages enforced, MOCK conforms; contract text and runbook | LF-A11-2 / LF-A11-6 | IMPLEMENTED (MOCK); real lag value PROVIDER DEPENDENT |
| F2e | M2 (declare paid / declare not paid) gated on a CURRENT payment_statement reconciliation for the provider, at submission and execution | H-W1 second half (LF-A11-8) | IMPLEMENTED (MOCK) |
| F3a | The A5 tombstone branch refuses a payout reference that already carries a tombstone (vote-time check + `AlreadyPosted` backstop), typed error | SR5R-1 | IMPLEMENTED (MOCK) |
| F3b | Standing finding `pay_payout_reversal_tombstone_but_paid` (migration 0167) | A5 R9 / LF-A5-1 (b) | IMPLEMENTED (MOCK); return-line semantics of a real PSP PROVIDER DEPENDENT |

Migration numbers: **0167** used (`0167_payout_reversal_tombstone_paid_kind`, up and down) and removed from
`internal/db/migrate.go` `reservedUnusedVersions` (the orchestrator's reservation commit `9378c91` says "if a listed version
is later used, remove it"). **0168 NOT used**: it stays in the reserved-unused set (please keep it registered as unused or
free it). F2a needs no migration (the seal columns exist since 0125). `migrate verify` on the merged tree: clean, no gaps.

## 2. Design

### 2.1 F2a: sealed cursor (SR11-10 closing the SQL-only half of SR11-1)

`internal/reconciliation/payment_cursor_seal.go`. With a seal verifier available (the stream's `ImportKeys`, else the
process-wide sealer when it can verify: `payoutinstrument.Keys` does), the cursor of (tenant, provider) is the newest
`coverage_end` among the pair's `payment_statement` run rows (`run_at <= now()`, `coverage_end <= now()`) for which a stored
import exists with the same tenant, provider and `coverage_end`, written by this stream (`imported_by_service`), by THIS
source (`source_label`, `is_mock`), carrying `import_seal` + `seal_kid`, whose seal VERIFIES over the import row and all its
stored lines (`verifyImportSealInTx`, the reconciliation-side twin of `payoutinstrument.VerifyStatementImportInTx`, parity
pinned by a test; the package still holds no key material: it uses the `statement.ImportSealVerifier` interface).

- Every run row of the pair above that cursor (whatever its `run_at`/`coverage_end`) is unbacked: ignored, logged at Error,
  audited (`reconciliation.payment_cursor_anomaly`) and alerted (F2b).
- Bounded work: at most 64 candidate `coverage_end` values (newest first) and 4 sealed imports each are examined; when all 64
  fail the run FAILS closed (`ErrPaymentCursorUnverifiable`, phase `cursor`, nothing fetched) rather than silently falling to
  a zero cursor.
- The stale-cursor re-check under the advisory lock uses the same sealed read (in the match snapshot).
- Ingest seals with the same keys (`importSealerFor`).
- Without any verifier (a non-production process without `PAYOUT_INSTRUMENT_KEYS`) the A11 rule (any backing import) applies
  unchanged. `reconciliation.VerifyPaymentCursorSealing` (called in `run()` right after `SetDefaultImportSealer`, before the
  scoped loop; static pin) refuses a PRODUCTION start with at least one registered statement source and no verifier.
  Production already requires the keys for any non-MOCK source (`payoutinstrument.VerifyStartup`) and refuses MOCK sources,
  so this is a belt-and-braces gate.

Residuals (stated honestly; SR11-1 is NOT "forgery-proof"):
1. An attacker who can run code in the application process holds the key (the T6 class): out of scope of a seal.
2. A forged run row naming the `coverage_end` of a GENUINE sealed import whose own match did not commit (a stale-cursor loser,
   or a failed match, which already raised its own P1) moves the cursor to that import's end: it skips at most that one
   fetched window, whose lines are stored. Closing this needs a seal on the run row itself (a new key-derivation label in
   the B13 key module: security + architect decision, proposed in section 9).
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
  still reported ("never silently missing", tested both ways);
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
1. the newest stream-recorded run exists (a `payment_statement` run row, `run_at <= now()`, with a backing import; when the
   service has import seal keys (`WithImportSealKeys`, wired in production from `PayoutInstruments`) the backing import must
   be sealed by the stream and VERIFY; at most 16 newest rows examined);
2. it is not truncated;
3. no `reconciliation.sweep_run_failed` audit row for `payment_statement` and this provider id is newer than it;
4. it is at most `DefaultStatementEvidenceMaxAge` (6 h, `WithStatementEvidenceMaxAge`) old (database clock).
The evidence is read in its own short read-only tenant (system) transaction, because a platform-acting session cannot read
`audit_log` (0112 acting fence). A forged failure row can only refuse. All existing M2 governance (proof-bound request and
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
Why it is not a permanent false P1: a legitimate branch is one where the PSP really returned the money; either the PSP never
reports the payout as succeeded (never raised: tested) or it reports the return (clears: tested), or the platform recovers
from the player (clears: tested). Residual: an off-platform recovery has no clearing path (identical to M2 (d)); it is
tracked through the mismatch's investigation status. The kind is in the staff-view vocabulary (`read.go`) and the OpenAPI
`StaffReconciliationKind` enum.

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

## 6. Residuals and open items

1. **SR11-1 is mitigated, not closed.** A forger inside the application process (holds the key) and the "genuine sealed import
   whose match did not commit" case (2.1 residual 2) remain. Proposed next step: seal the run row (a run-cursor HMAC under a new
   key-derivation label in the B13 key module plus a column: security + architect decision) and/or the cursor-independent
   lookback re-match (SR11-1 option b). Keep SR11-1 OPEN (binding before the first non-MOCK source).
2. Without import keys (non-production) the cursor uses the A11 rule. Staging has no `PAYOUT_INSTRUMENT_KEYS` today (the
   acceptance check skips on it), so staging's MOCK stream runs in that mode. Production cannot (startup gate).
3. LF-A11-1: mutant C5 (ledger-join (a) windowing) is not killed by a dedicated test (section 5). `lastChangeAt` uses
   `resolved_at` first; a succeeded attempt always has it.
4. LF-A11-2/6: the real settle lag, record-time dating (contract item 7) and server-side paging stay PROVIDER DEPENDENT
   (conformance-test items for the first real source). The 15-minute default is a placeholder, not a vendor fact.
5. F2e: the gate is Go-only (the seal cannot be verified in SQL; the registered-source rule next to it is Go-only too). The
   evidence read uses a second pooled connection while the M2 transaction is open (staff-only and rare; a pool-saturation
   note). The staff read model's `admissible_kinds` (ADR 0114) is shape-based and does not reflect the gate: a listed kind can
   still be refused with 409. A `cursor_anomaly` does not block M2 (otherwise a SQL-only forger with a far-future unbacked row
   could deny M2 forever); failure, truncation and staleness do.
6. F3b: off-platform recovery has no clearing path (M2 (d) precedent). The PSP "return" is modelled as a `reversed` payout line
   on the original reference; a real PSP may report returns differently (PROVIDER DEPENDENT, A5 R6). A return reported under
   the payload's `return_reference` is not read.
7. F3a: a refused payout stays `submitted` with its hold and a foreign tombstone on its reference: an integrity case (409
   "investigate"); resolution through M2/M4 per its shape or the provider's own outcome.
8. Migration 0168 unused. 0167 changes one CHECK; its down refuses while findings exist (WD099).
9. The full `./internal/payments` package was not run end to end in one invocation (stopped by the harness limit; see 4.1).

## 7. Probes for security review

1. **SR11-1 exact probe** (`TestF2_SR11_1_ForgedImportAndRunAsRuntime_StreamNotBlinded_AnomalyFires`, runs as `igaming_runtime`,
   asserted with `current_user`): in a tenant session forge an import shaped like the stream's own (MOCK label, `is_mock`,
   `imported_by_service`, 0 lines) unsealed / with a made-up seal and the real kid / with a seal COPIED from a genuine import,
   plus a run row with the same `coverage_end`, before each of two sweeps. Expected and observed: the A11-mode cursor moves to
   the forgery (control); the sealed cursor does not; the deposit made after the genuine cursor is imported; a new durable
   `cursor_anomaly:n<k>:<digest>` P1 per new forgery and an audit row per sighting.
2. Can forged rows exhaust or slow the cursor read? 64-candidate / 4-import bound, fail closed
   (`TestF2_SR11_10_ForgedCandidateFloodFailsClosed`); line reads bounded (`LIMIT MaxPaymentStatementLines+1`).
3. Can production run with a non-verifying sealer? (`VerifyPaymentCursorSealing`, static pin of its position in `run()`.)
4. `verifyImportSealInTx` vs `payoutinstrument.VerifyStatementImportInTx` parity (`TestF2_VerifyImportSealParityWithPayoutInstrument`);
   reconciliation holds no key (interface only).
5. SR11-11: any change in the set of unbacked rows gives a new discriminator; always within the 160-character CHECK.
6. F2e: can a SQL-only actor mint M2 evidence? No keys: a run row needs a backing import (`TestF2e_M2_UnbackedRunRowIsNotEvidence`).
   Keys: the import must verify (`TestF2e_M2_WithKeys_OnlyASealedRunIsEvidence`, forged sealed-looking import refused). A forged
   FAILURE row only refuses. Closed token 409 + denied audit row over HTTP (`refused_when_reconciliation_not_current`).
7. **SR5R-1 exact probe** (`TestF3_SR5R1_PreExistingTombstoneOnThePayoutReference_Refused`): deposit-style tombstone (fresh
   correlation, key `tombstone:<provider>:<ref>`) on a pending payout's reference; two grant holders decide: both refused (typed,
   409), zero decisions, zero request-correlated tombstones, payout still submitted with its hold. Backstop:
   `TestF3_SR5R1_BranchRefusesAnAlreadyPostedTombstone`. Specificity: another provider's tombstone on the same string does not block.

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
   `w.assertInvariants()` runs in the F3 tests.

## 9. Proposed registry / ADR / runbook text (for the orchestrator)

- **SR11-1** (OPEN, HIGH, binding before the first non-MOCK source): add "F2 (SR11-10 IMPLEMENTED): the cursor trusts only a run
  backed by a sealed import of the same source whose seal verifies; production refuses a registered source without keys; SQL-only
  forgery of run plus import no longer blinds the stream (probe test). Residual: in-process key holder; a forged run naming a
  genuine sealed import whose match did not commit (one window). Next: run-row seal or lookback re-match."
- **SR11-10** -> `CLOSED (IMPLEMENTED against MOCK, F2)`. **SR11-11** -> `CLOSED (F2)`.
- **LF-A11-1** -> `CLOSED as an engineering item (F2, IMPLEMENTED against MOCK)`; residual C5 disclosed.
- **LF-A11-2** -> `PARTIALLY IMPLEMENTED (F2)`: settle lag + horizon enforced, contract updated; real lag and record-time dating
  PROVIDER DEPENDENT. **LF-A11-6** -> `PARTIALLY IMPLEMENTED (F2)`: page order enforced; provider-side paging PROVIDER DEPENDENT.
- **H-W1 second half / LF-A11-8** -> `CLOSED as an engineering item (F2e, IMPLEMENTED against MOCK)`; residuals 6.5.
- **SR5R-1** -> `CLOSED (F3a)`. **A5 R9 / LF-A5-1 (b)** -> `CLOSED as an engineering item (F3b, migration 0167, IMPLEMENTED
  against MOCK)`; R6 (real PSP return reporting) stays PROVIDER DEPENDENT.
- Migration numbers: 0167 used; 0168 unused (free).
- ADR 0095 addendum (text only): "F2: the payment_statement cursor is sealed (SR11-10); continuation statements are bounded by the
  provider's settle-lag horizon and time-ordered pages; bounded-mode matching windows attempts by their last change; M2 is admitted
  only while the provider's reconciliation is current. F3: the payout reversal tombstone branch refuses a reference that already
  carries a tombstone; `pay_payout_reversal_tombstone_but_paid` is the branch's standing finding."
- ADR 0101 addendum (text only): "M2 precondition (F2e): the provider's payment_statement reconciliation is current (last run
  succeeded, not truncated, no later failure, <= 6 h; sealed evidence when the process has keys), at submission and execution
  (`reconciliation_not_current`)."
- Runbook: applied in `docs/runbooks/observability-and-alerting.md` (`cursor_anomaly`, `ErrPaymentCursorUnverifiable`, stuck-cursor
  steps 1 and 5-8). Proposed addition for `pay_payout_reversal_tombstone_but_paid` (reconciliation kinds section of
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
