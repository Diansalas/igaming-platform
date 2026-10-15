# A11: payments statement-source wiring and bounded observation: record, proposed registry text, review probes

Branch `a11-payments-wiring` (base `ca3e25c`). Date 2026-10-15. Author: `payments`. Everything here is `IMPLEMENTED` against MOCK providers
only; real statement ingestion and matching stay `PROVIDER DEPENDENT` and `NOT IMPLEMENTED`. Nothing here claims real-PSP readiness, a licence
position or a vendor contract. The companion table is `docs/plans/a11-payments-binding-recheck.md`.

This file is the proposal for registry/ADR text. `docs/governance/task-registry.md`, HANDOVER, progress, active-stage and the
human-decision register were NOT edited.

## 1. What was built

### 1.1 Statement-source registry and the scoped sweep (PAY-K3-STATEMENT-SOURCE-WIRING-1)

`paymentStatementSources()` (cmd/platform-api) is still the ONE list of sources the binary schedules and registers for M2. A11 adds:

- `cmd/platform-api/payment_statement_registry.go`: provider id -> `statement.PaymentStatementSource`, built in `run()` from that same
  `paySources` variable (a static pin, `TestRun_StatementSourcesRegisteredAndScheduledFromOneList`, forbids any other source of truth and the
  legacy flat-list loop).
- `internal/reconciliation/payment_sources.go` plus `scheduler.go`: `RunSchedulerLoopScoped`, `RunSweepScoped`, `RunSweepTenantsScoped`
  (`PaymentSweepConfig{Resolver, RunLineBound, AllowSynthetic}`). The legacy `RunSweep`/`RunSchedulerLoop` (flat list, every source for every
  tenant, no cursor) keep their signatures and behaviour (the whole pre-existing reconciliation suite passes unchanged).

Wiring table (what the production loop does per tenant, active or non-active/observed):

| situation | outcome |
|---|---|
| provider configured for the tenant (a `provider_capabilities` row of any status, or a payment attempt naming it) and an eligible source is registered | the payment_statement stream runs: fetch (no tx) -> ingest -> match (REPEATABLE READ), bounded and resumable |
| provider configured, NO source registered (including a nil resolver, nil/typed-nil source, empty registry) | NOT reconciled: error log, audited `reconciliation.sweep_run_failed` (phase `source`, reason `no_source`), durable P1 `reconciliation.run_failed` (phase `no_statement_source`), run counted `failed`, gauge `reconciliation_payment_unreconciled{reason="no_source"}`, rule `PaymentProviderNotReconciled`. NO `reconciliation_runs` row (a run row reads as clean). Never skipped silently |
| provider configured, source registered but ineligible (it speaks for another provider id, or it is Synthetic where mocks are not allowed) | same, reason `source_not_eligible`; the source is NEVER fetched (no credential resolution, no provider call) |
| the tenant's provider listing fails | one failed outcome, phase `scope`, reason `scope_unavailable`, alert `stream:payment_statement:scope` |
| source registered, provider NOT configured for this tenant | not run for this tenant (no credential resolution or fetch for a tenant that never used it) |
| production, any Synthetic / non-`ProductionEligible` source in the list | startup refused (`newPaymentStatementRegistry`), in addition to `refuseSyntheticInProduction`; the sweep independently treats a Synthetic source as ineligible when `AllowSynthetic` is false |

A non-Synthetic source must implement `statement.ReadOnlyAttested` and return true in EVERY environment. Duplicate, blank or padded provider ids and
nil sources are startup errors; the registry is all-or-nothing.

Tenant scoping: the tenant id and provider list come from the server-side sweep and the tenant's own RLS-scoped reads (`WithTenant`), never from
a statement or a client. Sources resolve per provider id only; there is no "the" source. Tests: `TestScoped_PerTenantIsolation` (tenant 2 never
causes a fetch of a provider only tenant 1 has; each tenant keeps its own cursor), `TestScoped_*`.

### 1.2 Bounded, resumable observation (R3-OBS-BOUND-1)

- **Bound**: `RECONCILIATION_PAYMENT_STATEMENT_MAX_LINES` (default 50000; 1..1000000) is handed to the source as `PaymentFetchRequest.MaxLines`. A
  source must return at most that many lines and, if the window holds more, a `Truncated` statement whose `CoverageEnd` is the exclusive
  instant of the first line left out (`statement.PagePaymentLines` does this and never splits a group of lines sharing one microsecond; an
  unsplittable group is refused with `ErrPaymentStatementUnpageable`, a P1, rather than guessed).
- **Cursor** (migration 0160, additive): `reconciliation_runs.provider_id`, `window_truncated`, plus the existing `coverage_end` (0152), all
  written in the run's own INSERT under the stream's per-tenant advisory lock. The cursor for (tenant, provider) is
  `max(coverage_end)` of its COMMITTED payment_statement runs (never from audit rows; future-dated/forged-high values beyond `now()+5 min`
  are ignored). A failed fetch, ingest or match writes no run row, so the cursor cannot advance past an unmatched window.
- **Continuation contract** (`validateBoundedWindow`, refusals are P1 failed runs with nothing stored): the statement starts EXACTLY at the cursor
  (a gap would skip a window, an overlap would count lines twice), holds no line dated before it, never exceeds the bound, and a truncated one
  makes strict progress, ends no later than the requested end and holds only lines before its `CoverageEnd`.
- **Concurrency**: under the advisory lock the match re-reads the cursor; if another instance committed a run for the same pair since this run's
  read, this run is SKIPPED (not failed; `errPaymentCursorStale`), so a window is never covered twice by design. Residual: the re-read uses the
  match transaction's snapshot, so a commit in the sub-millisecond gap between the snapshot and the lock call can be missed; the consequence is
  one re-matched window (idempotent), never a skipped one.
- **What happens when the bound is hit (explicit statement)**: the run still matches the window the source vouched for; it is recorded
  `window_truncated=true` (run row and audit `window_truncated`), logged at Warn, raises ONE durable P1 `reconciliation.run_failed` with phase
  `truncated` (discriminator `stream:payment_statement:provider:<id>:truncated`, occurrences grow), is counted in `reconciliation_runs_total`
  as `truncated`, does NOT advance `reconciliation_last_success_unix_seconds` (so `ReconciliationStale` still fires on a backlog that never
  drains), keeps its mismatches in `reconciliation_mismatches_open`, and shows as `reconciliation_payment_unreconciled{reason="truncated"}`
  (`PaymentStatementBacklog` after 6 h). It is NEVER a clean claim. The next run continues from the cursor; the pair leaves the gauge when a
  run completes its window.
- **MOCK**: `MockStatementSource` honours `MaxLines` and `ContinueFrom` (lines dated by their last status change in bounded mode, see the
  limitation below). Real sources implement the same contract (section 3).

### 1.3 Read-only requirement (R3-FIRST-REAL-SOURCE-READONLY-1, engineering half)

- By construction: `statement.PaymentStatementSource` exposes exactly `Label`, `ProviderID`, `Fetch(ctx, PaymentFetchRequest)`; the request and
  statement types are plain data (no tx, func, chan, pointer or interface). `TestPaymentStatementSource_TypeLevelReadOnly` fails on any change.
  A source is never handed a database transaction or a `PaymentProvider` (which has `Deposit`/`Withdraw`).
- The MOCK fetch goes through the provider-call gate as a `ReadOnly` call; a real one must too.
- **Real-credential requirement (PROVIDER DEPENDENT, launch checklist)**: a real source MUST use a separate, vendor-provisioned
  read/reporting-scoped credential (statements/reports only; it must be unable to initiate, capture, refund or pay out), resolved per tenant
  through the outbound-credential subsystem, never the payment/payout credential; `ReadOnlyStatementCredential()` may return true only once
  that is provisioned and verified; the first real `Fetch` needs its own security read-only review and a conformance test against the real wire
  format. The code can attest and refuse; it cannot prove the vendor-side scope.

## 2. Decisions taken (engineering, reversible) and open ones

Taken here (within A11 scope, reversible):

1. The cursor lives on `reconciliation_runs` (migration 0160), not in a new table or in audit rows: it is atomic with the run, append-only,
   already the SR3B-3 pattern, and needs no new RLS policy or grant.
2. The "not reconciled" alerts reuse the registered Kind `reconciliation.run_failed` with a phase and a phase-suffixed discriminator (no new
   Kind, no `alert_kinds` migration; consistent with ALERT-KINDS-DEDICATED-1's deferred option).
3. Scoped sweep = configured providers only (capabilities union attempt providers). A registered source does not run for a tenant that never
   used its provider.
4. Truncated runs are not successes for freshness.
5. S-L1, S-L4(2), PAY-PARK-BIND-RACE-1, H(12) and the E3 manifest declaration were closed (see the recheck table). All are fail-closed additions.

Open (not decided here):

- H-W1 second half: gate M2 on "the last run for (tenant, provider) succeeded" (owner; ledger-finance).
- HD-CTF-10: when observation of a closed tenant ends and what happens to its credential (owner). The cursor does not retire a closed tenant.
- Whether launchgov gets an `R-PAY-RECONCILIATION` readiness check (bumps `SnapshotVersion`, ADR 0112); `reconciliation.PaymentUnreconciled()` is the
  accessor. The metric, alert rules and durable alerts are the delivered visible state.
- An overall per-sweep time budget and bounded concurrency (the sweep is serial).
- MOCK limitation (disclosed, not a defect of a real source): in bounded mode the MOCK dates each line by its last status change, so a later
  settlement re-appears in the window it happened in; a MOCK deposit that is created and settles inside one window is seen once. A real source
  must define `OccurredAt` the same way (time of the line's last status change) or its status changes after the cursor are never re-reported.

## 3. Contract for a real `PaymentStatementSource` (PROVIDER DEPENDENT)

1. Implement `Label`, `ProviderID` (exactly the provider id of its adapter, validated by `providerref`), `ReadOnlyStatementCredential() bool`,
   `MarkProductionEligible()` (only after review), and `Fetch`.
2. `Fetch` runs with NO transaction held (`txscope`), resolves the tenant's read-only credential through the gate (`ReadOnly: true`), reads the
   wire body through `statement.LimitPaymentStatementBody` and collects lines through `PaymentLineCollector`/`DecodePaymentStatementJSONLines`.
3. Honour `req.MaxLines` (use `statement.PagePaymentLines`) and `req.ContinueFrom`: return `CoverageStart == ContinueFrom` when it is set, no
   line dated before it, and `Truncated`/`CoverageEnd` per section 1.2. Never return more than `MaxLines` lines.
4. Date every line by the time its status last changed; carry `merchant_reference` on payout lines (R-S3) and declare
   `PayoutLinesCarryMerchantReference` honestly.
5. Never write; never return a `PaymentProvider`, a tx, a callback or a credential through the statement.

## 4. Migration numbers

- **0160** used: `0160_reconciliation_run_payment_cursor` (additive, up and down; the down refuses while any cursor evidence exists).
- **0161** NOT used. Please register 0161 as reserved/unused (a proposed MA020-K2-VISIBILITY-1 tenant-session policy would be its first user).
- 0158 and 0159 belong to other agents; this branch has a gap there until they merge, so `migrate verify` reports the gap on this branch alone.

## 5. Changed assertions (no assertion was deleted or weakened; each moved to the new structure and the new pins are additive)

1. `internal/reconciliation/adr0113_check_d_static_test.go`: `RunSweep` must call `sweepTenants`/`observeNonActiveTenants` -> the shared body
   `runSweepAll` must, and `RunSweep`/`RunSweepScoped` must call `runSweepAll`; `RunSchedulerLoop` must call `RunSweep` (kept) and
   `runSchedulerLoop`; `RunSchedulerLoopScoped` must call `RunSweepScoped` and `runSchedulerLoop`.
2. `internal/reconciliation/sweep_alert_static_test.go`: the loop's `handleSweepError`/`recoverSweepPanic` pin moved to `runSchedulerLoop`
   (both loops must reach it and their sweep); the listing-sentinel pin moved to `runSweepAll`/`runSweepNamed` (and the four entry points must
   call them).
3. `internal/reconciliation/r3_observation_static_test.go`: `observeNonActiveTenants` may call `reconcilePaymentStreamsForTenant` (instead of
   `ReconcilePaymentStatementForTenant` directly); NEW `TestR3_Static_PaymentStreamsHelperCallsOnlyTheStreamAndRecorders` pins that helper to
   `ReconcilePaymentStatementForTenant`, `resolvePaymentTargets`, `reconcilePaymentNotReconciled`, builtins and `fmt.Errorf` only.
4. `cmd/platform-api/payment_statement_wiring_test.go` (`TestRun_StatementSourcesRegisteredAndScheduledFromOneList`): the scheduler's trailing
   argument is no longer `paySources...` of `RunSchedulerLoop`; it is `<registry>.SweepConfig(...)` of `RunSchedulerLoopScoped`, where the registry
   is built by `newPaymentStatementRegistry(env, paySources)`; the legacy loop must not be used.
5. Fixtures (not assertions): `capability_manifest_test.go`, `echo_declaration_test.go` accepted-real-adapter manifests gained
   `StatusQuerySuccessCarriesAmountAndAsset: true`.

## 6. Proposed registry text (for the orchestrator to apply)

- **PAY-K3-STATEMENT-SOURCE-WIRING-1** -> `CLOSED as an engineering item (IMPLEMENTED against MOCK; PROVIDER DEPENDENT for a real source)`.
  A11: provider-id source registry in `cmd/platform-api`; production refuses Synthetic/ineligible/unattested sources; the scheduler runs the scoped
  per-(tenant, provider) stream; a configured provider with no eligible source is a failed run + durable `reconciliation.run_failed` + gauge,
  never clean (tests `TestRegistry_*`, `TestScoped_*`). Residual (real money): a real source; H-W1 second half (gate M2 on last-run success).
- **R3-OBS-BOUND-1** -> `PARTIALLY IMPLEMENTED (A11)`: per-run line bound, persisted per-(tenant, provider) cursor (migration 0160), visible
  truncated status + alert, no skipping/double counting (tests `TestBound_*`). Remaining: per-sweep time budget and bounded concurrency;
  closed-tenant observation lifetime (HD-CTF-10); real source semantics (section 3).
- **R3-FIRST-REAL-SOURCE-READONLY-1** -> engineering half done (type-level read-only pin, `ReadOnlyAttested`, registry refusals, real-source
  configuration in the coverage tests); OPEN: vendor read-scoped credential + the first real Fetch's own security review.
- **R3-LISTING-FAILURE-LOG-ONLY-1** -> `CLOSED (A9, migration 0156)`.
- **PAY-SEC-LAUNCH-1** -> destination binding `IMPLEMENTED against MOCK` (B13-A/B); S-L1 `CLOSED` (A11); S-L4 `CLOSED` (ADR 0119 s9.5 + A11 capability
  re-read); S-L3 OPEN.
- **PAY-DEPOSIT-ESCALATION-1** -> `CLOSED as an engineering item (B6/B7, ADR 0095 s41)`; adapter-contract clause enforced as a registration-time
  declaration (A11); vendor conformance PROVIDER DEPENDENT.
- **PAY-RECON-POLL-REF-CLEAR-1**, **PAY-RECON-PARKED-CAPTURE-STANDING-1**, **PAY-CALLBACK-MISMATCH-BIND-1**, **PAY-PAYOUT-UNBOUND-HOLD-1**,
  **MA020-SYNC-MISMATCH-1**, **STMT-TABLE-INSERT-RLS-1**, **KYC-ENF-DECISION-ROWS-1**, **PAY-KYC-UNAVAIL-1**, **PAY-PAYOUT-ERRREF-1** ->
  `CLOSED (IMPLEMENTED against MOCK)` with the evidence in the recheck table.
- **PAY-PARK-BIND-RACE-1** -> `CLOSED (A11)`. **PAY-H-FOLLOWUPS-1 (12)** -> `CLOSED (A11)`; items 1, 9, 11 closed earlier; the rest per the table.
- **MA020-K2-VISIBILITY-1** -> acting-session half closed by 0125; tenant-staff half OPEN (security + architect).
- **PAY-PAYOUT-CONTRADICTION-HOLD-1**, **PAY-K3-FOLLOWUPS-1**, **PROV-OUTBOUND-CRED-1**, **F-POOL-2** -> no status change; residuals restated in the table.

Proposed ADR 0095 s40 addendum (text only): "A11: the payment_statement stream is scoped per (tenant, provider). R-S1 is narrowed: a provider the
tenant still has configured or in flight whose statement source is removed or ineligible is a failed run, a durable P1 and a gauge, never a
silent end of its standing findings. The stream is bounded per run and resumes from a persisted cursor (migration 0160); a truncated run is
never a clean claim."

## 7. Review probes

Security (credential/PCI/RLS boundaries; none of this touches PAN or hosted fields):

1. Registry: can a Synthetic, nil, typed-nil, duplicate, blank-id or unattested source reach the scheduler in production? (`TestRegistry_*`)
2. Can the scoped sweep fetch for a provider the tenant does not have, or use another tenant's credential? (provider list is the tenant's own
   RLS read; the credential resolves per tenant inside the gate; `TestScoped_PerTenantIsolation`.)
3. Forged cursor: the runtime role may INSERT `reconciliation_runs` (accepted SR3B-3 posture). A forged far-future `coverage_end` is ignored; a
   forged value within 5 minutes of now skips at most that window; a forged low value re-matches. Is that acceptable for a real PSP?
   (`TestBound_ForgedCursorsAreIgnored`)
4. A hostile provider id (from a capability/attempt row) in alert discriminators/audit/logs: `discPart` keeps the discriminator charset safe; the
   the audit/log value is sanitised the same way (`invalid_provider_id` for an unsafe id).
5. S-L1: submit/resolve refuse the beneficiary before claim/poll (`TestSL1_*`); S-L4(2): capability re-read inside the claim tx
   (`TestSL4_*`, test-only hook `payoutClaimAfterRoutingHook`, nil in production).
6. PAY-PARK-BIND-RACE-1: bind via the audited, savepoint-scoped helper; lost race leaves the park unbound (`TestParkBindRace_*`).

Ledger-finance (no ledger path was added or changed; the stream is read plus findings):

1. Cursor advance is atomic with the match: the run row (with `coverage_end`/`provider_id`) is inserted in the same transaction as the
   mismatches and the audit; a failed phase writes none (`TestBound_FailedFetchDoesNotMoveTheCursor_*`).
2. Window semantics vs the matcher: platform attempts are windowed by `sentAt` against the statement coverage; a truncated statement narrows
   coverage so lines beyond the cut cannot cause "missing provider record" for attempts outside it. Please confirm the boundary case (an attempt
   sent just before a window end whose line is dated just after) behaves as in the unbounded mode (the same race exists there).
3. Stale-cursor skip leaves an unmatched content-addressed import (plain evidence); duplicate lines across imports already occur on every
   MOCK run (each run re-imports its window). Confirm no K3/M4 evidence rule treats the loser's duplicate lines as ambiguity.
4. `window_truncated` runs: standing M2/M4 evidence loaders read persisted lines across all imports, so truncation does not hide evidence.
5. E3 declaration: registration-time only; the T16 escalation remains the fail-safe for a false declaration.
6. `ClaimForDispatch` S-L4(2) adds a refusal only (no new posting); H(12) moves an existing phase C onto a detached bounded context (no new
   path; it can post through the existing T6 drain exactly as before).

## 8. Mutation pass (wiring and bound logic; script in the session scratchpad)

47 mutants over the registry, source resolution, window validation, cursor query/persistence, stale-cursor skip, truncation alert/flag,
tracker, pager, MOCK source, S-L1, S-L4(2), PAY-PARK-BIND-RACE-1, H(12) and the E3 declaration: all KILLED except where noted. First pass
found one SURVIVOR (P6, the "providers known only from attempts" union): killed after adding
`TestScoped_ProviderKnownOnlyFromAttemptsIsStillInScope`. `B2` (drop the whole S-L4(2) re-read) could not be built as a valid mutant
(unused variable); its two halves B3 (status ignored) and B4 (withdrawal capability ignored) are both killed.

## 9. Ledger-finance independent review (2026-10-15, head `e2f0206`)

Reviewer: `ledger-finance`. Scope: the three-dot diff `origin/claude/focused-wright-jw88w9...a11-payments-wiring`, against the probes in
section 7.

**Verification limits.** DB access FAILED: the documented dev roles (`igaming`, `igaming_test_admin`) are rejected with
`password authentication failed`, and the session has no DB settings of its own. Per CLAUDE.md I stopped there. No scratch DB was created
and no role, password or process was touched. So NO integration test (`-tags=integration`) was run by this reviewer: none of `TestBound_*`,
`TestScoped_*`, `TestParkBindRace_*`, `TestSL1_*`, `TestSL4_*` or `TestH12_*`. What did run: `go build ./...`, `go vet` and the non-DB
unit tests for `internal/reconciliation/...`, `internal/payments`, `cmd/platform-api` (all pass, including `TestValidateBoundedWindow_Table`,
`TestPagePaymentLines_*`, `TestPaymentStatementSource_TypeLevelReadOnly`, `TestRegistry_*`). Everything else below is static review.

### 9.1 Confirmed from the code (static)

- **Migration 0160**: additive (two nullable columns, two CHECKs, one partial index). RLS, the append-only triggers and the grants are
  unchanged. The down migration refuses while cursor evidence exists. The index matches the cursor query's predicate.
- **Cursor atomicity**: `persistRun` writes `coverage_end`/`provider_id`/`window_truncated` in the run's own INSERT, inside the REPEATABLE READ
  match transaction, together with the mismatches and the `reconciliation.sweep_run` audit row. A failed fetch, ingest or match returns
  before `persistRun` and writes no run row, so the cursor cannot advance past an unmatched window.
- **Contiguity**: `validateBoundedWindow` requires `CoverageStart == cursor` exactly and no line before the cursor. A truncated statement
  must make strict progress, end at or before `PeriodEnd`, and hold only lines before `CoverageEnd`.
- **Stale cursor**: the pair's cursor is re-read under `pg_try_advisory_xact_lock`. If it moved, the run is skipped with no row. The
  documented residual is accurate: `set_config` is the transaction's first statement and fixes the snapshot, and the try-lock is the next
  statement, so the gap really is tiny. The worst case is a re-matched window (a duplicate run, mismatch rows and alert occurrences); it
  is never a skipped one, because both racers started from the same cursor and the cursor is a `max()`.
- **Paging**: `PagePaymentLines` sorts at microsecond precision, steps back to the start of the instant group straddling the bound, and
  returns `cutoff = first excluded instant`. Every page line is `< cutoff` and every excluded line is `>= cutoff`, so no line is skipped or
  counted twice. An unsplittable first group is refused.
- **Missing or ineligible source**: the outcome carries `Err` (tracker `failed`), with no run row, an audited `sweep_run_failed`, a durable
  `run_failed` and the gauge. A nil or typed-nil source, a provider-id mismatch, Synthetic without `AllowSynthetic`, a nil resolver and a
  listing failure all fail closed. In production the registry refuses Synthetic and non-`ProductionEligible` sources at startup.
- **Truncated freshness**: `runTracker.record` handles `resultTruncated` in its own branch, which never sets `lastSuccessUnix`.
- **No new ledger write path**: the only new INSERT in non-test code is `reconciliation_runs`. No posting, balance or ledger table is
  touched. Ingest idempotency (content-addressed import reuse) and the `(provider_id, provider_tx_id)` uniqueness are unchanged.
- **PAY-PARK-BIND-RACE-1**: `parkDepositAttempt` now binds through `bindParkReference`. That path uses a savepoint, treats only
  `payment_attempts_tenant_provider_ref` 23505 as `lost_race`, and writes an audit row. A lost race leaves the park reference-less, and
  reconciliation classes it `unboundPark` (reported in-run, cleared on the line's reference). No posting is involved.
- **H(12)**: phase C runs on `context.WithTimeout(context.WithoutCancel(ctx), depositPhaseCTimeout)`, the same as `drive.go:272`.
  `Pending.Flush` detaches internally. The phase C logic and its T6 drain are unchanged.
- **S-L4(2)**: the capability is re-read inside the claim transaction. A refusal only rolls back; nothing is posted. See LF-A11-3 for the
  locking caveat.

### 9.2 Findings

| id | severity | finding | required action |
|---|---|---|---|
| LF-A11-1 | MEDIUM; binding before the first non-MOCK statement source | **Matcher windowing vs line dating (answers probe 2: it does NOT behave as in unbounded mode).** In bounded mode, lines are dated by their last status change. The "missing provider record" rule, however, windows platform attempts by `sentAt`. In a TRUNCATED run, every `succeeded` attempt with `sentAt < CoverageEnd` whose provider line is dated `>= CoverageEnd` (it settled after the cut) is reported as `pay_missing_provider_record` (a P1). The next run's coverage starts at the cut, so the line is matched there by reference, but the false finding stays. In unbounded mode lines were dated by creation (about `sentAt`), so this needed a fetch-to-match race. Under truncation the exposure is the attempt's whole settlement lag, and it peaks during a backlog. It is fail-safe (a false positive, not a false negative), but it is untested and floods P1 at the worst moment. Simply suppressing the rule in truncated runs would create a false negative: no later run has that `sentAt` in its coverage. | Payments + ledger-finance: window the rule by the platform attempt's last state change (consistent with line dating), or defer it for attempts whose settlement window crosses `CoverageEnd`, with a test that keeps "never silently missing". Register as its own row. Correct the probe-2 text above. |
| LF-A11-2 | MEDIUM; PROVIDER DEPENDENT, binding before the first non-MOCK statement source | **The cursor turns `CoverageEnd` into a permanent completeness claim.** Section 3 lacks two contract clauses: (a) `CoverageEnd` must never exceed the provider's own completeness or reporting-lag horizon; (b) `OccurredAt` must be the provider's record time of the change and never a backdatable value or effective date. Breaking either loses data. A line that becomes visible later with `OccurredAt < cursor` is never requested again, because the request starts at the cursor (a silent false negative). If the source returns it anyway, every later statement is refused with `ErrPaymentStatementCoverageGap`: stuck, P1 each run, no runbook. Separately, a non-truncated statement's `CoverageEnd` is checked only against `now + 5 min`, not against `PeriodEnd`. The MOCK legitimately returns its fetch time. | Add clauses (a) and (b) to section 3 and the launch checklist. Add a per-provider settle lag (`PeriodEnd = now - lag`) before a real source is wired. Add a runbook entry for a stuck cursor. |
| LF-A11-3 | LOW | **S-L4(2) narrows the window but does not close it.** `LoadCapability` is a plain SELECT. A disable that commits after the re-read and before the claim commits still dispatches. `reassertSubmitter` (S-L4(1)) uses `FOR SHARE`; this re-read does not. The proposed registry text "S-L4 CLOSED" therefore overstates it. | Either lock the capability row `FOR SHARE` (check the lock order against the capability writers) or reword to "narrowed (A11); kill switch remains the stop". |
| LF-A11-4 | MEDIUM (registry integrity) | **The recheck table is incomplete.** Five OPEN registry rows labelled binding or "before the first real PSP" are absent from it: PAY-RECEIPT-T4-DRAIN-TEST-1, PAY-POLL-DECLINED-ALERT-RECON-1 (payments + ledger-finance), PAY-PAYOUT-REFBIND-1 (binding before any real payout provider; a money-binding issue), PAY-FPAY-HARDENING-1 and PAY-RECON-D2-HARDENING-1 (ledger-finance). None is mis-closed; they are untouched. But "What was NOT closed" reads as exhaustive. | Add the five rows to the recheck table with their honest class (b-open or c), and to the not-closed summary, before the orchestrator applies section 6. |
| LF-A11-5 | LOW | `statement.PaymentFetchRequest` doc says "CoverageStart <= ContinueFrom", but the validator requires equality (`CoverageStart == ContinueFrom`; an earlier start is refused as an overlap). A real-source author following the type doc would be refused. | Fix the doc comment to "exactly ContinueFrom". |
| LF-A11-6 | LOW | Paging contract: a real source that collects the whole window and pages afterwards (as the MOCK does through `PaymentLineCollector`) hits the 1M streaming or body cap on a large backlog. It is then refused every run and the cursor is stuck. | Section 3: require provider-side ordered paging (by `OccurredAt`, with the tie group kept whole), not collect-then-page. |
| LF-A11-7 | LOW | Migration 0160 does not tie `provider_id IS NOT NULL` to `stream = 'payment_statement'` and `coverage_end IS NOT NULL`. The cursor query filters on both, so this is harmless today. | Optional CHECK in a later migration; no action needed for A11. |
| LF-A11-8 | INFO | Section 6 closes PAY-K3-STATEMENT-SOURCE-WIRING-1 and leaves H-W1's second half (gate M2 on "last run for (tenant, provider) succeeded") as a residual sentence only. It is a real-money blocker (registry H-W1, 2026-10-05). R-S1 scope note: a provider known only from legacy ledger postings (no capability row, no attempt) is outside the scoped sweep, which is acceptable for the pre-cutover path only. | Keep H-W1 second half as its own OPEN real-money row; do not fold it into the closure. |

Answers to the other section 7 probes: (1) confirmed statically. (3) persisted-line evidence dedupes on content that includes
`occurred_at` (`payment_statement_k3.go`), so a stale-cursor loser's duplicate lines collapse. Change-dated lines for the same reference
with different statuses already occurred in legacy overlapping imports, so this is not a new ambiguity class. (4) confirmed: the K3/M4
loaders read across all imports. (5) confirmed: the E3 declaration is registration-time only, and T16 stays. (6) confirmed: no new
posting.

### 9.3 Verdict

**NOT APPROVED (BLOCKED on verification), with no blocking defect found in static review.** The ledger invariants hold by construction:
no ledger write, the cursor is atomic with its run, failed and missing paths never read as clean, and truncated runs do not advance
freshness. Approval needs:

1. The integration suites named in 9.1 run green on a private scratch DB once DB access is restored by the orchestrator/human.
2. Doc fixes LF-A11-3 (wording or lock), LF-A11-4 and LF-A11-5 before the section 6 registry text is applied.
3. LF-A11-1, LF-A11-2 and LF-A11-6 registered as OPEN rows binding before the first non-MOCK statement source. LF-A11-8 kept as its own
   row.

None of this is a blocker for a MOCK-only merge once items 1 and 2 are done.

## 10. Security independent review (2026-10-15, head `e2f0206`)

Reviewer: `security`. Scope: the three-dot diff `origin/claude/focused-wright-jw88w9...e2f0206` against the section 7 security probes and the
orchestrator's focus list. Out of scope: real-PSP statement ingestion (none exists), vendor-side credential scope, penetration testing, and the
ledger-finance follow-up commit `5fc0991`. The concurrent payments working-tree edits (FOR SHARE capability re-read, doc fixes, section 9.4)
were NOT reviewed. They were uncommitted while this review ran.

**Verification.** Private scratch DB `sec_a11_review` (sanctioned `priv_db.sh` harness). No role, password or process was touched. Integration
runs (`-tags integration`) were green:
- the targeted A11 sets `TestRegistry_*`, `TestScoped_*`, `TestBound_*`, `TestSL1_*`, `TestSL4_*`, `TestParkBindRace_*`, `TestH12_*`,
  `TestR3_*`, `TestRun_*`, `TestPagePaymentLines_*`, `TestPaymentStatementSource_TypeLevelReadOnly`;
- the full `./internal/reconciliation/...` and `./cmd/platform-api`.

Caveat: these ran against the shared worktree, which may already have held the payments agent's in-progress uncommitted edits. The `e2f0206`
code itself was reviewed statically. One temporary probe test (SR11-1) was run and then removed. A copy is in the reviewer's session scratchpad
(`sr11/zz_sr11_probe_integration_test.go`), and nothing from it is committed.

### 10.1 Findings

| id | severity | finding | required action |
|---|---|---|---|
| SR11-1 | HIGH. Launch-blocking before any non-MOCK statement source. Blocks the section 6 wording until the record is corrected | **A forged cursor silently blinds the payment_statement stream. The documented residual understates this.** The runtime role can INSERT `reconciliation_runs` rows. One forged row (`stream='payment_statement'`, `provider_id=X`, `run_at=now()`, `coverage_end=now()+4 min`) passes the cursor filter (`run_at <= now()`, `coverage_end <= now()+5 min`) and becomes `max(coverage_end)`. The next run fetches from that point with `ContinueFrom`. The source then omits every line dated before it, which means every line since the last genuine run (up to a whole sweep interval, default 1 h) plus the 4 minutes. The run commits `status=clean`, and its `coverage_end` (forged + 1 µs from the MOCK) re-persists the forged future cursor as genuine evidence. Repeating the INSERT before each sweep blinds the provider indefinitely, with every run reading clean, no alert, no gauge and no failed run. **Probe (run, confirmed):** cursor established, a MOCK deposit created, forged row inserted, three sweeps: the deposit was never imported and all runs were `clean`. Why the SR3B-3 acceptance does not carry over: in `casino_statement` the anchor only VALIDATES a statement, so a forged high value causes a loud refusal (a P1). Here the anchor SELECTS what is fetched, so a forged value causes silent data loss. The unbounded mode that A11 replaced in the production loop could not be blinded this way. The claims in 1.2, 7.3 and the recheck table ("skips at most that window", "at most that many minutes") are incorrect. Precondition: arbitrary SQL as the runtime role (SQLi or a compromised app instance). That is exactly the threat the reconciliation detective control exists to catch. | (a) Correct 1.2, 7.3 and the R3-OBS-BOUND-1 recheck row: a forged in-skew cursor skips everything since the last genuine run, and repeated forging blinds the pair indefinitely while every run reads clean. (b) Register SR11-1 as an OPEN row (owner security + architect, ledger-finance consulted), binding before the first non-MOCK statement source. Do not fold it into R3-OBS-BOUND-1. (c) Mitigation design, to be decided by architect + security. Options: make the cursor unwritable by the runtime role (owner-only table or SECURITY DEFINER writer, which is an ADR-level role change and not an agent action); and/or run a periodic cursor-independent lookback re-match (for example, daily over the last N days) so a forged cursor delays detection instead of preventing it; and/or alert on cursor anomalies (cursor row with no matching import, or cursor > its own `run_at`). Also clamp the persisted `coverage_end` to the fetch time (SR11-2). Code is not required for a MOCK-only merge. |
| SR11-2 | LOW (related to LF-A11-2) | A genuine run can persist a FUTURE `coverage_end`. A statement `CoverageEnd` up to `now + 5 min` is accepted, and it becomes the cursor. Lines the provider later reports with `OccurredAt` between the fetch time and that cursor are never requested again. The same path is what makes SR11-1 self-perpetuating. | Persist `min(CoverageEnd, fetch time)` as the cursor, or refuse a continuation statement whose `CoverageEnd` exceeds the fetch time. Fix this together with LF-A11-2's settle lag. |
| SR11-3 | LOW | Probe 7.4 is only partly true. The audit `error` field and the log `error` attribute of `reconcilePaymentNotReconciled` include the RAW provider id through `fmt.Errorf("... provider %q ...", tg.providerID)`; only `provider_id` is sanitised (`discPart`). `%q` escapes control characters, so there is no log injection, but a hostile or oversized id from a capability or attempt row still reaches audit and logs verbatim. No secret or PII exposure was found. | Build the error with the sanitised value (`shown`), and add a test with a hostile id that asserts the audit `error` text. |
| SR11-4 | LOW | S-L1 refusals (submit and resolve by a staff member linked to the withdrawing player's Person) are logged at Warn only (ids, no PII) and are not audited. A beneficiary-staff attempt is an insider-fraud signal and should be durable, like the payout kill-switch Denied audit. The pre-existing self-approval 403 has the same gap. | Write a Denied audit row (actor, tenant, withdrawal id, action, reason `staff_is_beneficiary`) on both refusals. This does not block the S-L1 closure. |
| SR11-5 | INFO | S-L1 check placement: it runs in the eligibility transaction before `ClaimForDispatch` (submit) and before any attempt read or poll (resolve). That is a separate transaction from the claim. Acceptable: `staff_users.person_id` is append-only once set (0034), and eligibility already requires it to be non-NULL and visible in tenant scope, so an RLS-invisible staff row yields "not linked" (403), never a silent pass. The only theoretical TOCTOU is a concurrent change of the PLAYER's `person_id` (identity merge). Tests `TestSL1_*` cover forbidden submit, forbidden resolve (no poll, no reschedule, no state change) and a different-Person control. | None. S-L1 can be marked CLOSED. |
| SR11-6 | INFO | S-L4(2) at `e2f0206` is a plain SELECT (= LF-A11-3): it narrows the window but does not close it. The refusal is fail-closed (rollback, request stays `approved`, no attempt or provider call), and the re-read is tenant/brand scoped in the claim transaction. The FOR SHARE follow-up in the working tree was not reviewed here. | S-L4(2) "CLOSED" only after the FOR SHARE change is committed and its tests run. Otherwise word it "narrowed". |
| SR11-7 | INFO | `payoutClaimAfterRoutingHook` is an unexported package variable, nil by default. Its only assignment is in `payout_sl4_capability_integration_test.go` (`//go:build integration`, reset in `t.Cleanup`), so it is nil in the production binary and unreachable from outside package `payments`. Nothing pins this, so a future non-test file in `payments` could assign it. | Optional: an AST static test asserting that only `_test.go` files assign it. |
| SR11-8 | INFO | Registry refusals confirmed (static review plus `TestRegistry_*`). Production is `cfg.GuardEnvironment()`, which fails closed to production when `APP_ENV` is unset. Refused at startup, all-or-nothing: Synthetic (directly and through wrappers), non-`ProductionEligible`, a non-Synthetic source without `ReadOnlyStatementCredential()==true` in any environment, nil, typed-nil, blank, padded and duplicate ids. The sweep independently refuses a Synthetic source where `AllowSynthetic` is false, and a source whose `ProviderID()` differs from the configured provider. Staging admits Synthetic sources, consistent with `RefuseSyntheticInProduction`. `ReadOnlyAttested` is a code attestation only: vendor-side credential scope stays PROVIDER DEPENDENT (R3-FIRST-REAL-SOURCE-READONLY-1 stays OPEN). The registry checks only whitespace on ids, not `providerref` (ids come from code, so the impact is low). | None for A11. |
| SR11-9 | INFO | Confirmed with no defect found: tenant isolation of the provider list (`WithTenant` + `tenant_id = $1`; `TestScoped_PerTenantIsolation`; no client input; the tenant comes from the server-side sweep); credential selection per tenant inside the provider-call gate (`ReadOnly: true`, tenant from the request, never from the statement); the cursor read (`WithTenantReadOnly`, tenant- and provider-keyed); migration 0160 (additive, RLS, append-only triggers and grants unchanged, control-character CHECK on `provider_id`); PAY-PARK-BIND-RACE-1 (savepoint, only the `payment_attempts_tenant_provider_ref` 23505 is `lost_race`, any other error rolls back, audit stores a reference fingerprint and never the raw reference); H(12) (detached bounded phase C, no new path); no route, middleware or permission changes (only the submit and resolve handler bodies gained a refusal, and the approve refactor is behaviour-equivalent); alert content (Kind `reconciliation.run_failed`, discriminator through `discPart`, attributes stream/phase/sqlstate_class only); Prometheus (`reconciliation_payment_unreconciled{reason}` uses a closed reason vocabulary, with no tenant or provider label and no PII; `PaymentProviderNotReconciled` pages, `PaymentStatementBacklog` tickets; the `run_failed` reason is left to the existing `ReconciliationRunFailed` rule); no secrets in the diff. | None. |

### 10.2 Verdict

**NOT APPROVED at `e2f0206`.** The one blocking item is SR11-1. The code is acceptable for a MOCK-only merge, but the record states an incorrect,
understated residual for a security posture, and section 6 would close R3-OBS-BOUND-1 / R3 items on that basis. To approve the MOCK-only merge:

1. Correct the forged-cursor residual text (1.2, 7.3, recheck-table row R3-OBS-BOUND-1) as in SR11-1 (a).
2. Register SR11-1 as its own OPEN row, binding before the first non-MOCK statement source (owner security + architect). Register SR11-2 with it,
   or under LF-A11-2.
3. S-L4(2): either the FOR SHARE change committed with its tests, or "narrowed" wording (SR11-6).

SR11-3 and SR11-4 are recommended low-cost fixes and are not blocking. No code change for SR11-1 is required for the MOCK-only merge. Its
mitigation is a design decision (architect + security; any DB-role change is human-authorised). Launch flag: SR11-1 must be resolved before any
real PSP statement source or real-money payment reconciliation relies on this stream.

This review is not a statement that A11 is "secure". It covers code- and design-level review of the diff above against MOCK providers only.
