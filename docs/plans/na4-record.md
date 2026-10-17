# NA-4 record: BONUS-SWEEP-WATERMARK-SKEW-1 (IMPLEMENTED)

Branch na4-bonus-watermark (from 76187cc). No migration, no ledger/table change, no ADR edit.

## Finding
`internal/bonus/deposit_sweep.go` advanced its `(posted_at, id)` watermark on
`ledger_transactions.posted_at`, which is `DEFAULT now()` = transaction START
(migration 0021:31). Deposit A starts (posted_at t0), B starts later (t1) and commits,
the sweep advances the watermark to t1, A commits afterwards: `(posted_at,id) > watermark`
never matches A again, so A got no bonus Grant, permanently.
Note: A cannot hold ledger locks while B commits (shared psp_clearing projection rows are
locked FOR UPDATE), so the realistic skew is A started early (PSP callback handling) and
reaches `ledger.Post` after B committed and the sweep ran. The test reproduces exactly that.
No other repo sweep uses xmin/commit ordering for watermarks (only sportsbook rollback
uses xmin for a different purpose), so the idiom chosen is a bounded trailing re-scan.

## Fix (smallest safe mechanism)
- `DepositSweepLateCommitLookback = 15m` and `listLateCommitDeposits`: each tick also reads
  deposits with `(posted_at,id) <= watermark`, `posted_at` within the lookback behind the
  watermark, and NO `bonus_grants` row with `trigger_reference = ledger tx id`.
  Bounded by `DepositSweepBatchSize`; read-only; never moves the watermark.
- Per-event logic extracted unchanged into `processDepositSweepEvent`, used for both late
  and new events.
- Idempotency: existing unique key `(tenant, campaign, offer_version, player, trigger_reference)`
  (migration 0057) -> `ErrAlreadyGranted`; deposits that already hold a Grant are excluded in SQL,
  so re-scans cost nothing and emit no duplicate audit/outcome.
- `DepositSweepOutcome.LateCommitEventsRescanned` added (diagnostic).

## Tests (internal/bonus/deposit_sweep_watermark_integration_test.go)
1. `..._LateCommittingDepositIsNotSkippedByWatermark`: FAILED on pre-fix code
   ("late-committing deposit A was skipped permanently (grants=0, want 1)"), passes after;
   then 3 repeated sweeps -> still exactly 1 grant each for A and B.
2. `..._RescanIdempotentUnderConcurrencyRetryAndPartialFailure`: sweep tx rolled back after work
   (partial failure), 6 concurrent un-locked overlapping sweeps, then retry -> exactly 1 grant per deposit.
3. `..._RescanSkipsAlreadyGrantedDeposits`: second tick evaluates 0 events.
All pre-existing deposit-sweep tests pass unmodified (no assertion changed).

## Residuals
- A deposit tx open longer than 15m before commit is outside the guarantee.
- A deposit with no matching Offer / ineligible has no Grant, so it is re-evaluated each tick
  during its 15m window: repeated multi-account-signal / malformed-reward audit entries are
  possible, and an Offer activated within that window can grant retroactively (same retroactivity
  the existing backlog path already has).
- A late-committing earlier deposit can qualify as "first deposit" after a later one already got a
  FirstDepositOnly grant (previously it was silently skipped); per-trigger unique key means both grant.
  Proposed ADR 0121 note: "deposit sweep watermark is start-time keyed; a 15m trailing re-scan covers late commits."

## Commands
- `go test -tags integration -count=1 -p 1 ./internal/bonus` (private DB): ok 22.7s (full package).
- `go test -count=1 ./internal/bonus` (unit): ok. gofmt clean; go vet plain + integration clean.
- httpserver untouched (only cmd/platform-api and config reference the sweep, no signature change).
- `git diff --stat 76187cc` touches only internal/bonus/* and this record: no ledger tables/migrations.
