# NA-1 record: PANIC-LOG-VALUE-1, REDACTEDREASON-PASSTHROUGH-1, SAFECALL-LOG-GUARD-1

Worktree `.claude/worktrees/na1`, branch `na1-panic-log`, base `76187cc`.
Author: `security` specialist (implementer). No migration. No ADR, governance,
HANDOVER, progress or active-stage edits (proposed registry text in s7).

## 1. What I found (verified at 76187cc)

Sites that logged a whole recovered panic value:

| Site | Before | Listed in NA-1? |
|---|---|---|
| `internal/httpserver/middleware.go` `recoverMiddleware` | `"panic", rec` | yes |
| `internal/httpserver/webhook_admission.go` `admitPreAuth` | `"panic", rec` | yes |
| `internal/httpserver/webhook_admission.go` `admitVerified` | `"panic", rec` | yes |
| `internal/bonus/schedulers.go` `runSchedulerLoop` | `"panic", r` | yes |
| `internal/rg/enumeration_sweep.go` scheduler loop | `"panic", r` | yes |
| `internal/httpserver/withdrawal_handlers.go` `recordRequestIntegrityRefusal` goroutine | `"panic", fmt.Sprint(r)` | **NO - sixth site, found by the new guard's scan** |
| `deploy/aws/acceptance/world.go` check runner | `"check panicked: %v", r` into the acceptance report | **NO - seventh site (operator tool report, not a runtime log)** |

`internal/payments/gate.go` `redactedReason` default branch returned
`err.Error()` for any non-transport, non-context error (contradicting its own
doc comment). Reached from (a) the credential resolver's error (gate step 3)
and (b) every adapter-call error that is not a `*providerref.Error` (gate step
8), i.e. `GateResult.Err`, which feeds logs, audit and receipts.

The casino (`redactedLaunchFailureDetail`) and KYC
(`RedactedProviderErrorDetail`) copies already fall back to a generic label:
verified, unchanged.

Every other `recover()` in production code already complied (type only via
`fmt.Sprintf("%T", r)`, nil-compare only, or discard).

## 2. Decisions and changes

1. **Type + stable code only** at all seven sites: attributes `panic_code`
   (stable per site) and `panic_type` (`fmt.Sprintf("%T", r)`). Log messages
   are unchanged, so `deploy/observability/log-event-alert-rules.yml`
   (`msg = "panic_recovered"`, `"webhook_admission_panic_recovered"`) still
   match. The `"panic"` attribute key is gone (intentional behaviour change;
   no test or alert rule read it). No stack trace is logged anywhere (none
   was before either).
   Codes: `http_handler_panic`, `webhook_admission_preauth_panic`,
   `webhook_admission_verified_panic`, `bonus_scheduler_panic`,
   `rg_enumeration_sweep_panic`, `withdrawal_integrity_record_panic`.
   The acceptance report now reads `check panicked (<type>)`.
2. **Recovery behaviour unchanged**: 500 from `recoverMiddleware`, 503 plus
   the `panic` admission metric from both admission stages, loops continue
   after a panicking tick. For testability the rg loop's and the withdrawal
   goroutine's deferred closures became named functions deferred directly
   (`recoverEnumerationSweepPanic`, `recoverIntegrityRecordPanic`), the same
   pattern `internal/reconciliation/sweep_alert.go` already uses; defer
   order in the withdrawal goroutine is unchanged (semaphore release still
   runs last).
3. **`redactedReason` allow-list** (`internal/payments/gate.go`):
   `*url.Error` -> `Op: timeout|canceled|transport error` (unchanged);
   context deadline/canceled (unchanged); safeCall's panic error is now a
   private type `adapterPanicError{typ}` whose `Error()` is built from the
   type name only and is the only "rendered" case; `redactedReasonSentinels`
   = `ErrProviderCallRefused`, `ErrStatementFetchUnderTx`,
   `providercred.ErrOutboundCredentialUnavailable` -> the sentinel's own
   fixed text (context wrapped around it is dropped); everything else ->
   `"adapter error (redacted)"` (same label as casino). The sentinel list is
   pinned by a test so widening it is a reviewed change.
   Consequence: the MOCK adapter's own non-sentinel errors (e.g.
   `payments/mock: unknown provider reference %q`) now render as the generic
   label inside `GateResult.Err`; no test asserted their text (grep-checked).
   `errors.Is(gr.Err, ErrProviderCallRefused)` on refusals and the
   `*providerref.Error` `%w` path are unchanged.
4. **Static guard** `internal/observability/panic_value_log_guard_static_test.go`
   (`TestStatic_RecoveredPanicValueNeverLogged`): parses every non-test
   `.go` file under `cmd/`, `internal/`, `deploy/` (sanity floor: >= 500
   files, >= 15 with `recover()`). A value from `recover()` may only be
   nil-compared, discarded (`_ = recover()`), re-raised (`panic(r)`), or be
   the argument of a `fmt.*f` call whose matching verb is `%T` (format
   parsed; `*` width/precision and explicit indexes are refused). Any other
   use (logger/slog/fmt argument, helper call, alias, composite literal,
   type assertion, return, `v = recover()` escape, `recover()` straight into
   a call) fails unless in an EXACT allow-list (key -> count; a count
   mismatch or a stale entry fails). Allow-list today: three entries, all in
   the test-support harness `internal/webhookauth/webhookauthtest` (panic
   value goes to `testing.T` failure text of a scheme under conformance
   test, imported only by `_test.go` files).
   `TestStatic_RecoveredPanicValueGuard_SelfTest` feeds 19 synthetic shapes
   (14 bad, 5 good) through the analyzer.
5. Runbook `docs/runbooks/observability-and-alerting.md` `panic_recovered`
   bullet updated (it said "the panic value and path are logged").

## 3. Tests added

- `internal/httpserver/panic_log_redaction_test.go`:
  `TestRecoverMiddleware_PanicValueNeverLogged_Still500`,
  `TestWebhookAdmission_PanicValueNeverLogged_Still503` (pre-auth and
  verified), `TestRecoverIntegrityRecordPanic_PanicValueNeverLogged` -
  panics carrying a fake secret/PII string as string, error, `*url.Error`
  with a query-string key, and struct; assert the secret never appears in
  the log (nor the response), `panic_code`/`panic_type` present, no
  `"panic":` key, no stack trace, status 500/503 unchanged.
- `internal/bonus/schedulers_panic_log_test.go`: same four values; loop runs
  a third tick after two panicking ticks.
- `internal/rg/enumeration_sweep_panic_log_test.go`: same four values via the
  deferred helper; nil logger still swallows; the real loop survives repeated
  panics (nil pool -> nil dereference) and the runtime error text is not
  logged.
- `internal/payments/gate_redacted_reason_test.go`: foreign error shapes ->
  generic label; sentinels bare and wrapped with a secret -> fixed text;
  transport/context/panic unchanged; end-to-end through `callProvider` for
  adapter error and two credential-resolver failures (wrapped sentinel with a
  secret, foreign DSN-bearing error); `errors.Is(ErrProviderCallRefused)`
  kept; sentinel list pinned.
- Existing `TestCallProvider_PanicValueNeverReachesErr`,
  `TestRLF4_PanicRecoveryRedactsWebhookPath`,
  `TestRecordRequestIntegrityRefusal_PanicIsRecoveredAndSlotReleased`,
  `TestIWire_*Panic*` unchanged and passing. No existing assertion weakened
  or removed.

Mutation checks (each reverted afterwards): re-adding `"panic", r` in bonus
-> guard and bonus test fail; `fmt.Sprint(rec)` in `recoverMiddleware` ->
guard (NOT ALLOWED ... `call:fmt.Sprint`) and 5 middleware subtests fail;
`return err.Error()` as `redactedReason` default -> 9 payments (sub)tests fail.

## 4. Commands and counts

- `gofmt -l internal deploy cmd` -> empty.
- `go vet` and `go vet -tags integration` on `./internal/httpserver
  ./internal/bonus ./internal/rg ./internal/payments ./internal/observability
  ./deploy/aws/acceptance` -> clean. `go build ./...` -> ok.
- Unit (`go test -count=1 -json`, pass counts include subtests):
  httpserver 350, payments 248, rg 50, bonus 33, observability 33,
  acceptance 1; 0 fail.
- Integration on private DBs (`priv_db.sh`/`priv_test.sh`, `-tags
  integration -count=1 -p 1`):
  - `./internal/bonus ./internal/rg` full: bonus 137 pass, rg 208 pass /
    1 skip, 0 fail.
  - `./internal/httpserver -run 'Panic|Recover|Webhook|Admission|Middleware|RequestWithdrawal|Integrity'`:
    248 pass, 0 fail (includes `TestIWire_PaymentWebhookPanic_*`,
    `TestIWire_CasinoCallbackPanic_*`,
    `TestRecordRequestIntegrityRefusal_PanicIsRecoveredAndSlotReleased`).
  - `./internal/payments` full: 2304 pass, 0 fail, 0 skip (package elapsed 4231 s).
- Private databases dropped after the runs.

## 5. Labels

- PANIC-LOG-VALUE-1: IMPLEMENTED (7 sites, incl. 2 not on the list).
- REDACTEDREASON-PASSTHROUGH-1: IMPLEMENTED (payments; casino/KYC already
  compliant, verified).
- SAFECALL-LOG-GUARD-1: IMPLEMENTED (static AST guard + self-test).

## 6. Residuals (stated, not hidden)

- The guard is syntactic and per function (no type checking, no
  inter-procedural flow). It tracks `x := recover()` via Go object
  resolution; every escape (alias, helper call, composite, return,
  assignment to an outer variable) is itself a violation, which is what
  closes the inter-procedural gap. A value laundered through an allow-listed
  test-support entry is not tracked further (harness output goes to
  `testing.T` only).
- The guard covers recovered panic values only. Ordinary `"error", err`
  logging of adapter errors outside the gate (e.g. phase-C or sweeper error
  logs) is not in its scope; payments gate errors are now redacted at
  source, casino/KYC have their own redactors.
- `panic_type` for an anonymous struct reveals field names (program text),
  never values.
- Not reviewed in this item: third-party library panics that print to
  stderr themselves (none known in the platform).

## 7. Proposed registry row (orchestrator applies)

| NA1-PANIC-LOG-MERGED-2026-10-17 | security | **PANIC-LOG-VALUE-1, REDACTEDREASON-PASSTHROUGH-1, SAFECALL-LOG-GUARD-1 CLOSED** via branch `na1-panic-log` (tip in the merge commit): seven `recover()` sites (`recoverMiddleware`, webhook admission pre-auth/verified, bonus `runSchedulerLoop`, rg enumeration sweep loop, plus two found by the guard: withdrawal request-integrity goroutine and the acceptance-suite check runner) now log `panic_code` + `panic_type` only (messages unchanged, alert rules still match); `redactedReason` returns transport/context classification, the gate's own type-only panic error, the fixed text of three allow-listed own sentinels, or `adapter error (redacted)`; static AST guard `TestStatic_RecoveredPanicValueNeverLogged` (exact allow-list: 3 test-support entries in `webhookauthtest`) plus self-test. Record `docs/plans/na1-record.md`. No migration. Residual: guard is syntactic/per-function; non-panic adapter error logging outside the payments gate is out of its scope. |
