# RV-PRH-I2 (casino) — Independent code review: ADR 0095 §15.1 casino launch split

Reviewer: `code-reviewer` (independent of the implementer). Date: 2026-09-27.
Scope: commit `224c532` and merge `008ac0c`, including the orchestrator's conflict
resolution in `cmd/platform-api/main.go` and `docs/governance/task-registry.md`,
on branch `claude/focused-wright-jw88w9` at HEAD `5a1ae02`. I read the diff and the
pre-split `LaunchGame` side by side. I did not rely on the §15.1.1 summary. Items in the
`ledger-finance` and `security` domains are listed in §4 for those owners to decide. I
have not ruled on them.

## Verdict: **NOT READY** (one small blocking rework item, R1)

The split itself is well built. Phase A commits before any provider method runs, which
the one-connection-pool test proves directly. Denials still commit their audit row.
`errors.Is(ErrProviderUnavailable)` still holds on the branches that had it. The merge
resolution is correct.

The blocker is narrower. The §15.1 contract, and `LaunchGame`'s own doc comment, say
phase C revokes the session and audits `casino.launch_failed` on **every** failure
branch. Phase C runs on the request `ctx`. If the failure was caused by that ctx being
cancelled (a client disconnect during `Launch`, which is the most common real-world
trigger once a real adapter does I/O), phase C cannot open its transaction. The result:
the session stays `active`, no audit row is written, and the error is discarded
silently. I reproduced this, and a bet callback against that session then **posts**
(details under R1). The fix is about five lines. The rest of the list is follow-ups.

## 1. Verification performed

- `go build ./...` and `go vet` (with and without `-tags=integration`) for `casino`,
  `providercred`, `httpserver` and `cmd/platform-api`: clean.
- Integration tests on a **private** database I created for this review, migrated to
  101 (not the shared CI database):
  - `go test -race -tags=integration ./internal/casino/`: pass. This includes all 7
    tests in `launch_two_phase_integration_test.go` and every re-plumbed call site.
  - `./internal/providercred/`, `./cmd/platform-api/`: pass.
  - `./internal/httpserver/ -run 'Casino|Stage7|Stage9|LockOrder'`: pass.
- `go list -deps ./internal/providercred` does not contain `internal/casino`. There is
  no production cycle. The only new production edge is `casino -> providercred`.
- Scratch probes (in-package temporary test files, run, then deleted, never committed):
  - P1: cancel `ctx` inside `Launch`. Result: `LaunchGame` returns
    `provider launch call failed: context canceled`, the session status is **`active`**,
    and **no** `casino.launch_failed` audit row exists.
  - P2: same as P1, then send a real-money bet callback naming that session. Result:
    `outcome=succeeded`, and the balance goes from 5000 to **4900**.

## 2. Findings (most severe first)

### R1 — BLOCKING: phase C is skipped whenever the request ctx is cancelled, leaving a live, bet-able session

`internal/casino/orchestrator.go`, `launchFailed` (and the phase-C success transaction):

```go
_ = pool.WithTenant(ctx, params.TenantID, func(ctx context.Context, tx pgx.Tx) error {
    _ = RevokeLaunchSession(ctx, tx, session.ID)
    return audit.Record(...)   // "casino.launch_failed"
})
```

- **Failure scenario.** A player opens a real-money launch on a real adapter, and their
  mobile connection drops while `Launch` is in flight. `net/http` cancels
  `r.Context()`. `Launch` returns `context canceled` (the vendor may already have
  created its session). `launchFailed` calls `pool.WithTenant(ctx, …)` with the
  cancelled ctx, so `Begin` fails. That error is dropped by `_ =`, so there is no revoke,
  no audit row, and no log line. The session stays `active`. `postBet` rejects only
  `revoked` sessions (orchestrator.go around L1271). It checks neither `expired` nor
  `expires_at`. So a vendor bet naming this session debits the player (probe P2) for a
  launch the platform reported as failed, with no audit trail of the failure.
- **Why this is not just the ADR's accepted crash window.** §15.1 accepts the end state
  "active and usable by the vendor" only for a process **crash**. Its Flow row
  specifies "failure or ambiguity → `RevokeLaunchSession` … plus audit
  `casino.launch_failed`". Context cancellation is neither a crash nor rare. The same
  defect hits the success path: vendor accepted, then the ctx is cancelled, so there is
  no `casino.launched` audit and the caller gets an error.
- **Fix.**
  - Run both phase-C transactions on
    `context.WithTimeout(context.WithoutCancel(ctx), <short bound>)`.
  - Log (at minimum) when the phase-C transaction itself fails, instead of `_ =`.
  - Add a test that cancels ctx inside `Launch` and asserts the session is `revoked`
    and the `launch_failed` audit row exists.
  - The mutation set had no mutant for this branch. It needs one.

### F1 — Mutation evidence is 9/9, not "10/10"

`docs/plans/payment-readiness/evidence/prh-i2-casino-mutation-kill.txt` lists M1–M8 and
M10 as killed (9) and M9 as equivalent. The commit message, ADR §15.1.1 and the evidence
file's own header all say "10/10 killed". Correct the count to "9/9 killed, 1
equivalent" (the CLAUDE.md no-fake-completion rule).

Two gaps in the mutant set are worth adding when R1 is fixed:

- **Circuit-open revoke.** `TestLaunchGame_UnhealthyProviderCircuitOpenRejected` asserts
  only the error. It does not assert the revoked row or the `launch_failed` audit.
  Deleting the revoke from the circuit-open path alone would survive.
- **Resolver-error branch.** `outbound.Resolve` returning an error has no test at all.

I agree that M9 is equivalent. With the transport-error branch removed, the zero-value
`Outcome` (`""`) still falls into the decline branch.

### F2 — ADR 0095 §15.1.1 wording inaccuracies

1. "wrapping the triggering cause with `%w` so `errors.Is(err, ErrProviderUnavailable)`
   still holds for the health/credential/registry branches exactly as before the split".
   - The credential-**resolution-error** branch wraps the resolver's own error, not
     `ErrProviderUnavailable`. A real `providercred` failure therefore maps to HTTP 500
     `failed to launch game`, not 503.
   - No credential branch existed "before the split".
   - Either wrap it (`%w: … %w` with `ErrProviderUnavailable`, which I recommend: an
     unconfigured or rotated vendor credential is "provider unavailable" to the player),
     or narrow the sentence to nil-resolver and binding-mismatch only.
2. Test inventory. The record says "five new tests" in `launch_two_phase_…` plus "two new
   tests in `failure_mode_matrix_integration_test.go` / `launch_two_phase_…`". In fact
   all **seven** new tests are in `launch_two_phase_integration_test.go`. The failure-mode
   matrix file has one modified and renamed test, and no new ones. The commit message has
   the same miscount.
3. "No production import graph changed". The test move changed none, but the change set
   did add the `casino -> providercred` production edge (harmless, verified acyclic).
   Reword to "the test move changes no production import".
4. §15's own test obligation 18 ("Launch: crash after phase A, and crash after vendor
   accept") has no corresponding test. The `F_…RevokesSessionNoTrace` test covers only
   "bet on a revoked session is rejected". Either add the tests or list them under
   "Not done here". As written, `IMPLEMENTED (code + tests)` implies §15's test list is
   met.

### F3 — Phase-C failures are invisible to operators (fold into R1)

Every phase-C error is discarded (`_ = pool.WithTenant(...)`, `_ = RevokeLaunchSession`).
If the revoke UPDATE fails, the transaction aborts and the audit insert fails too. The
session then stays `active` with no trace in the logs or the audit table. At minimum,
log this at error level so the "session left active" state can be alerted on.

### F4 — Simplification/efficiency (low, optional)

Health now runs after phase A. Every launch attempt during a vendor outage therefore:

- runs the full RG/risk evaluation (advisory locks included),
- inserts a session,
- writes `launch_requested`,
- revokes the session, and
- writes `launch_failed`.

The old code short-circuited before any of this, and a player retry loop during an
outage multiplies the writes. §15.1.1 records the reorder as a deliberate choice. That is
acceptable, but a pre-phase-A `HealthStatus` fast-fail (still outside any transaction,
using the provider id from the game row) would remove the write amplification without
touching the committed-intent semantics. Not required.

### Informational (no action)

- The handler's `wallet.GetOrCreate` now commits in its own transaction before
  `LaunchGame`. A wallet row therefore persists even when the launch then errors, where
  before it rolled back. This is harmless: an empty wallet with no ledger effect, and the
  pre-split denial path already committed it.

## 3. Items confirmed correct

- **No transaction across Health/Launch.** `TestLaunchGame_NoConnectionHeldAcrossHealthAndLaunchCalls`
  uses a pool of `MaxConns=1` and actually acquires the only connection from inside
  `HealthStatus` and `Launch`. It also asserts `txscope.Held == false`. This is a real
  proof, not a statistics check.
- **Phase-A denial commits.** Denials return `nil` from the callback with a
  closure-captured result, so the RG/risk audit rows commit. The
  `TestLaunchGame_Denied*` tests still pass, and M1 kills the early-return removal.
- **Every non-ctx failure branch revokes and audits.** This covers: registry re-check,
  circuit open, nil resolver, resolver error, binding mismatch, transport error, and
  non-`Succeeded` outcome. The ctx exception is R1.
- **`errors.Is(ErrProviderUnavailable)`** holds for nil pool, capability branches
  (unchanged, phase A), circuit open, nil resolver, binding mismatch and registry
  re-check. The resolver-error branch does not (F2.1).
- **Changed test expectation is correct.** Before the split, the revoke ran inside the
  caller's transaction, and the returned error rolled back the entire transaction,
  session insert included, so zero rows were left. After the split, phase A has
  committed, so exactly one `revoked` row is the only possible correct end state. The
  rewritten test is **stronger** than before: it also asserts the `launch_failed` audit,
  a rejected bet on the revoked session with zero `ledger_transactions` rows,
  debits == credits before and after, and a fresh session id on retry.
- **Call-site updates (about 26).** None is weakened. The removed `WithTenant` wrappers
  only supplied a transaction, and every assertion is unchanged.
  `TestConcurrent_SelfExclusionDuringLaunch_Deterministic` remains meaningful, because
  the RG check and the session mint are still in one phase-A transaction. That is the
  atomicity the test pins.
- **Import-cycle fix.** `TestOutbound_NoCredentialOnLongLivedTypes` moved verbatim to
  `package providercred_test`, plus `casino.MockOutboundResolver{}` was added to the
  sweep. The test passes, and there is no production graph change from the move.
- **Wiring.** `tc.TenantID` (server-side) feeds `LaunchGame`. The binding check re-asserts
  tenant, provider and domain on the resolved credential.
  - `casinoOutboundCredentials()` returns the MOCK resolver only when `b.Casino`
    (typed `*casino.MockCasinoProvider`) is wired. Otherwise it returns
    `Credentials.Outbound("casino")`, and nil fails closed.
  - `MockOutboundResolver` carries `SyntheticComponent()`.
  - `CallContext` renderers never print the secret.
- **Merge `008ac0c` main.go resolution.** `NewWithAdmission` keeps
  `CasinoOutboundCredentials`, `WebhookAdmission`, `LoadDirectory`, `RunDirectoryRefresh`
  and the `http.Server` timeouts. The combined diff against both parents is exactly the
  union. The registry rows are well-formed.
- **CLAUDE.md money rules.** No floats, no direct balance writes, and no hardcoded real
  secret (the mock credential is a fixed placeholder behind the synthetic guard).
- The `hmac.Equal` checklist item does not apply (no webhook-scheme code in this change).

## 4. For domain owners (not adjudicated here)

- **ledger-finance / casino.** §15.1's Callbacks row says "A revoked **or expired**
  session gives `ErrLaunchSessionRequired`". `postBet` rejects only `revoked` (plus
  not-found, wrong-provider and demo). It checks neither `status='expired'` nor
  `expires_at`. §15.1's crash-window "harmless … it expires" argument, and the ledger
  re-verify text (rv §…3044 in the ADR), depend on expiry bounding bet acceptance. In the
  code, a never-resolved `active` session accepts bets. This predates PRH-I2, but
  PRH-I2's safety argument now rests on it. Please confirm whether the session is meant
  to outlive the token TTL (then fix the ADR text) or whether `postBet` should reject it
  (then fix the code).
- **security.** `launchFailed` persists `cause.Error()` into append-only
  `audit_log.metadata`. For today's MOCK this is benign. For a real HTTP adapter, a
  `*url.Error` includes the full request URL. If an adapter ever places the launch token
  or credential material in the query string, it becomes unerasable audit content.
  Consider a bounded reason code plus a redacted cause.

## 5. Rework required before "IMPLEMENTED" stands

| # | Item | Blocking |
|---|---|---|
| R1 | Phase C on `context.WithoutCancel(ctx)` + short timeout; log phase-C failure; ctx-cancel test (revoked + `launch_failed` audit) + mutant | Yes |
| F1 | Correct "10/10" to "9/9 + 1 equivalent"; add circuit-open-revoke and resolver-error tests/mutants | No (fold into R1 commit) |
| F2 | Fix §15.1.1 wording (errors.Is claim, test inventory, import-graph sentence, §15 item 18 status) | No |
| F3 | Covered by R1's logging | — |
| F4 | Optional pre-phase-A health fast-fail | No |

## Re-review (FH-7, 2026-09-28) — code-reviewer

HEAD `0d019b2`; fix commits reviewed `2c00e10` (R1/C1, item 2, C2/C3, F1/F2), `be5b828` (bounded audit reason), `0ca1193` (IO-1B txscope refusal). Private DB `rr_fh7_cr_20260928` at 0107 (dropped afterwards); `go test -race -tags=integration ./internal/casino/ ./cmd/platform-api/` ok, 0 FAIL.

### Verdict: **NOT READY**. All prior findings are closed, but the item-2 fix adds a new blocking regression (N1)

| # | Status | Evidence |
|---|---|---|
| R1 (blocking) phase C skipped on a cancelled ctx | **CLOSED** | `orchestrator.go:535`/`:660` use `context.WithTimeout(context.WithoutCancel(ctx), phaseCTimeout)`; the success-audit failure falls back to `launchFailed` (`:672-680`); `RevokeLaunchSession` returns `(bool, error)`; tests `TestLaunchGame_CtxCancelledDuringLaunch_StillRevokesAndAudits`, `..._CtxCancelledAfterVendorAccept_StillAuditsLaunched` |
| F1 mutation count and missing mutants | **CLOSED** | evidence 15/15 + 1 equivalent; `TestLaunchGame_CircuitOpenRevokesAndAudits`, `TestLaunchGame_ResolverErrorRevokesAuditsAndMapsToProviderUnavailable`; mutant MCO independently killed |
| F2.1 resolver error not `ErrProviderUnavailable` | **CLOSED** | `orchestrator.go:605` wraps with `%w` |
| F2.2–F2.4 ADR wording | **CLOSED** | ADR 0095 §15.1.1/§15.1.2/§16.2 |
| F3 phase-C failures invisible | **CLOSED** (residual Low) | Warn/Error logs with redacted detail; no test asserts the Error line (MF3 survived) |
| F4 pre-phase-A health fast-fail | OPEN (optional) | unchanged |
| §4 security question (raw `cause.Error()` in audit) | Addressed (`be5b828`) | closed `LaunchFailureReason` only; security to rule |

Mutants: MCO killed; MF3 (phase-C Error log disabled) survived; **MEXP (expiry applied only to `active`) survived the whole casino suite**.

**N1 — HIGH, BLOCKING — every real-money casino session stops accepting bets 2 minutes after launch.** `internal/casino/orchestrator.go:1466-1468` rejects a bet when `now > session.ExpiresAt`. `expires_at` is the *un-consumed launch-token* TTL (`launch.go:31-33`, `DefaultLaunchTokenTTL = 2m`), set once at mint and made immutable by the 0036 trigger. The check also applies to `consumed`, the normal in-play state. Failure: a player launches a slot; spins post for about 2 minutes; then every bet callback returns `ErrLaunchSessionRequired`, which is mapped to a validation error ("request rejected") and logged at Error level. Confirmed by a probe (TTL 400ms: bet 1 ok; after the TTL, bet 2 on the consumed session fails "session has expired"). The suite misses it because no test bets past the TTL on a consumed session; MEXP survives, so security C4's claimed closure in ADR 0095 §15.1.2 is untested. Fix direction (casino/security/ledger-finance design call): apply the token TTL to never-consumed (`active`) sessions only; close C4 properly, either by revoking `WHERE status IN ('active','consumed')` on a failed launch (C4 option 1) or with a separate explicit lifetime/idle bound for consumed sessions; correct ADR 0095 §15.1 and §15.1.2; add a consumed-session-past-TTL test.

**N2 — LOW.** `launch_two_phase_integration_test.go:554-558`: the `TestLaunchGame_CircuitOpenRevokesAndAudits` doc comment sits above `TestLaunchGame_TransportErrorAuditNeverStoresRawErrorText`.

Confirmed correct: `OutboundKindSplitResolver` fails closed; nil-in-interface handled in `casinoOutboundCredentials()`; `txscope.Held` refusal before `Launch`; no floats, no balance UPDATEs, no hardcoded secrets.

**Required before READY:** N1 fix + test + ADR 0095 §15.1/§15.1.2 correction. Optional: MF3 assertion.
