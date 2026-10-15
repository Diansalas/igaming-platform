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
