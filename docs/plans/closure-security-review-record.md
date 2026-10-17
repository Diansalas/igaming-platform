# Closure security review record (independent reviewer, `security`)

Worktree `.claude/worktrees/secrev`, branch `secrev-closure` from `claude/focused-wright-jw88w9` @ `2daf352`. Range reviewed:
`76187cc..2daf352` (merges: f4r1, srf2r, na1, na2, na3, u67, jur, na4, na56, regscan-list, u5-money). Date 2026-10-17.
I did not fix anything. This file is the only change on the branch. This was a code- and design-level review against MOCK
providers and synthetic data. It is not a pentest, a certification audit, or a legal or regulatory approval.

Attacker models:
- (A) a SQL-only actor holding the stolen `igaming_runtime` credential;
- (B) a staff insider;
- (C) an external webhook sender or player.

## 1. Verdict

**APPROVE WITH CONDITIONS** for the provider-independent freeze. None of the findings requires a code change before the freeze.
The conditions are about registry accuracy and ownership (section 4).

**F4 R1 (forged dispatch-gate ALLOW): CLOSED for attacker A.** Without the actor-proof key, the runtime role can no longer get
a row into `withdrawal_gate_decisions` that WD053 counts as an allow. That rules out moving `approved -> submitted` or a payout
attempt to `submitting` on a forged allow. What remains is listed in section 3.

## 2. What was verified

### 2.1 F4 (migration 0171, `dispatchproof.go`, verifier change, `withdrawal_gate_decisions_zz_actor_proof_guard`)

Read: the up/down SQL, `actorproof.go` (claims validation, `Attach`, hook), `dispatchproof.go`, `dispatchgate.go`, `prooftest/dispatchhook.go`,
the payments call sites and the static pins.

**Privileges.** Checked on a private DB at head 0172, as `igaming_runtime`:
- `rolsuper=f`, `rolbypassrls=f`;
- no `CREATE` on `public`;
- no `USAGE` on `actorproof_private`;
- no UPDATE, DELETE or TRUNCATE on `withdrawal_gate_decisions`;
- no INSERT, UPDATE or DELETE on `actor_proof_nonces`;
- EXECUTE on `actor_proof_require` (calling it directly only consumes a valid proof).

**Probes.** Each ran as the runtime role in a scratch test that was deleted afterwards and is not committed. All were refused:
- a multi-row INSERT with one proof, second row naming another request: AP004;
- the same request twice: AP005;
- a COPY FROM forged allow: 0A000, because RLS refuses COPY;
- an allow committed in an earlier transaction, then E7: WD053;
- a valid allow rolled back to a SAVEPOINT, then E7: WD053.

**Existing tests.** The 11 F4R1/0171 tests pass on the private DB: binding matrix, wrong key, replay, concurrency, expiry,
no-issuer fail-closed, up/down/up and WD095. So do `TestF4_DB_Catalog` and `TestStatic_*` in `internal/actorproof`.

**The verifier change does not widen any other operation.**
- The new block refuses `scope='system'` for every operation except `withdrawal_dispatch_gate:allow`.
- It refuses that operation for every scope except `system`, and for every actor except `...0f4d1500`.
- `system` still needs a tenant (the `ELSIF p_tenant IS NULL` branch).
- Every other trigger passes a fixed, non-dispatch operation.
- `aml_authorize` only accepts `aml_*` operations and the tenant / platform_acting scopes from `financial_actor_session()`.
- So no existing caller can reach the new scope.
- The LIKE pattern escapes `_`.
- The system actor id is not a credential: the MAC is.

**Trigger hardening.**
- `search_path` is pinned and calls are schema-qualified. pg_temp is never searched for functions or operators.
- The caller's nonce is overwritten.
- The nonce is read from the same GUC in the same statement.
- `allowed IS NULL` falls through to the proof check, so it fails closed.

**Fail-closed when there is no issuer or key.**
- `ErrDispatchProofUnavailable` wraps `ErrGateUnavailable`.
- T1p maps it to 503 and rolls back.
- T2/T12 roll back.
- Production startup requires `ACTOR_PROOF_KEYS` and an active kid.

**Down migration.** Refused (WD095) while proof-bound rows or alerts of the new Kind exist. Otherwise it restores the 0166/0169
bodies, which reopens R1. It is a dev-only operation.

**Signer-owner static pin.** It is non-vacuous for `internal/withdrawal`: it plants a second importing file and a second hook
reader, and checks exactly one `Attach` with the system claims. It is narrower than its description, see finding F-3.

**Proof leakage.** `Attach` binds the token as a parameter (`set_config(..., $1, true)`), so `pg_stat_activity` shows only `$1`.

### 2.2 SRF2R (0172, `server_cursor.go`, budget, `match_stalled`)

- Cursor names are fixed and checked by regexp. Queries are bound, so there is no injection.
- Every past-budget path fails closed:
  - the stream returns `cursor_flood` P1, with no fetch and no cursor;
  - M2 returns `evidence_scan_budget_exceeded`;
  - the stall progress read sets `budgetHit`, so nothing counts as progress and the stall is raised.
- The Request narrowing only adds refusals: a provider that was not pre-read gets `no_successful_run`.
- The stall check polls `pg_locks` and never takes the lock.
- A flood or a held lock causes a loud stop, not a silent pass. See residual R-7 for the DoS cost.
- 0172 makes `payment_ref_cleared` tombstone-only, which removes the statement-line vector. Ledger tombstones themselves can
  still be forged; see F-1.

### 2.3 NA-1

- Every changed recover site now logs only the type and a stable code.
- `redactedReason` returns err text only for the gate's own panic type and three allow-listed sentinels.
- The static guard's self-test covers the alias, composite, helper, `%v`/`%w`/`%*T` and escape shapes.
- Neither `go test ./internal/observability ./internal/payments -run 'Panic|Redacted|Static'` failed.
- No production re-panic site exists. A re-panic would print the value to stderr.

### 2.4 NA-2

- Only verified callbacks trigger the raise.
- The alert carries the existing P1 Kind, discriminator `attempt:<id>:reason:<closed>`, and attribute `provider_id` only.
- Dedup per attempt and reason bounds the number of rows. There is no PII.
- A transient raise failure rolls back the delivery, and the PSP's redelivery retries it.

### 2.5 NA-3

- A `HealthStatus` error now fails closed, and the HTTP layer maps it to a fixed 503 message.
- The cause is logged only through `redactedLaunchFailureDetail`. The audit row records only the closed reason.
- The metadata allowlist for `casino.launch_denied_by_jurisdiction` holds ids and codes only: no PII, no IP, no user agent.
- Each denied attempt writes one row. Bounding audit volume from launch spam is the rate limiter's job (informational).

### 2.6 NA-6

- A production guard now admits only the `production` anchor.
- The reverse direction (a non-production guard on a production anchor) is still not checked; see F-5.

### 2.7 U-6

- One allow-list gate, `testSupportRoutesGate`, accepts explicit `development` or `staging` only.
- It is pinned by AST.

### 2.8 U-7

- `bonus_suggestion.claimed` is audited in the same transaction.
- The static reachability proof is "reaches a success audit write", not "every path audits". The record discloses this.
- The 8 AML exclusions write their own SECURITY DEFINER trails (no tipping-off).

### 2.9 U-5

- These are client-side display and input parsing changes only. The server stays authoritative.
- Nothing security-relevant was found. The hand-written `parseJsonExact` can at worst cause a client-side mis-parse, with no
  server effect.

## 3. Findings (severity-ranked)

### F-1 MEDIUM (pre-existing, not widened; the SRF2R-3 closure claim is overstated)

**Attacker A can forge a deposit tombstone that clears the MA020 SQL guard.**
- Location: `migrations/0172_*.up.sql`, `payment_ref_cleared`. The only payout tombstone guard is in `migrations/0165:249`, and it
  covers only `correlation_id` values that are withdrawal requests.
- Verified on the private DB as `igaming_runtime`:

  ```sql
  INSERT INTO ledger_transactions (tenant_id, transaction_type, idempotency_key, provider_id, provider_tx_id, correlation_id)
  VALUES (<t>, 'tombstone', 'mockpsp:ref', 'mockpsp', 'ref', gen_random_uuid())
  ```

  The insert was admitted, and `payment_ref_cleared(<t>, 'mockpsp', 'ref')` then returned `true`.
- Impact:
  - The MA020 preventive barrier against a manual credit for a captured-unposted deposit is removed.
  - The manual credit itself still needs the signed four-eyes flow, so attacker B is also required.
  - The forged tombstone also makes a late, genuine success for that reference be rejected (player harm). The SQL-only
    attacker can do this regardless of 0172.
- The SRF2R record's claim "a ledger tombstone is written only by the ledger posting path" holds for code, not for attacker A.
- Fix (follow-up, needs a migration):
  - bind a deposit tombstone to a verified reversal receipt row written in the same transaction, or make it proof-bound;
  - add a detective rule: every deposit tombstone must have a matching verified reversal receipt.

### F-2 MEDIUM (ledger-finance matter)

**NA-4 re-scan can issue two FirstDepositOnly grants for one player.**
- Location: `internal/bonus/deposit_sweep.go`, `isFirstDeposit` (around line 208) and `listLateCommitDeposits`.
- How it happens:
  - `isFirstDeposit` orders by `posted_at`, which is the transaction START time.
  - B is swept first, while A is not yet visible, so B gets a first-deposit grant.
  - When A commits late, the re-scan finds no earlier deposit for A, so A also qualifies.
  - The unique key includes `trigger_reference`, so both grants stand. The NA-4 record discloses this.
- Attacker C (a player) can try to provoke it with concurrent deposits. Success depends on timing, and the 15-minute window
  helps.
- The same window also grants retroactively when an offer is activated within it.
- Fix options:
  - a uniqueness rule for first-deposit grants per (tenant, offer, player): a partial unique index (migration), or a
    per-player advisory lock plus an existing-grant check;
  - or a commit-ordered watermark.
- Owner: `ledger-finance` / product. This must be decided before any FirstDepositOnly offer is live with real money.

### F-3 LOW

**The F4 signer pin covers one package of fourteen.**
- Location: `internal/actorproof/actorproof_test.go`, `staticAllowedFiles` and `TestStatic_WithdrawalSignsOnlyInDispatchProof`.
- Every package in `staticAllowed` (including `internal/payments`, `internal/httpserver` via capability, and `cmd/platform-api`)
  can build `{ScopeSystem, OpDispatchGateAllow, SystemActorDispatchGate}` claims, which `Claims.validate` accepts.
- Nothing pins `INSERT INTO withdrawal_gate_decisions` to `dispatchgate.go`.
- So a future code change could sign an allow without `EvaluateDispatchGate`, and the four-call-site pin would not catch it.
- `actorproof.SystemActorDispatchGate` is also a mutable exported `var`. Changing it cannot widen anything, because SQL pins
  the UUID; it can only cause a denial of service.
- Fix:
  - `codeLines` pins limiting `OpDispatchGateAllow`, `ScopeSystem` and `SystemActorDispatchGate` to `actorproof.go` and
    `dispatchproof.go`, and the table INSERT to `dispatchgate.go`;
  - make the actor an unexported value behind a function.
- Attacker model: B or a code-change path, not A.

### F-4 LOW

**The SF4-5 T2/T12 alert can be suppressed in a mass outage.**
- Location: `internal/payments/payout_withdrawal_gate.go`, `observeRedispatchGate`.
- `gateFailCounts` is wiped when it reaches 4096 entries.
- With more than 4096 attempts failing closed at once (for example a missing key everywhere, the case the alert exists for),
  each attempt's counter keeps resetting before it reaches 3, so the T2/T12 alert never fires.
- Payouts still fail closed, and T1p still raises on every refusal.
- Fix: on overflow, raise (or evict only entries that are not failing) instead of resetting everything.

### F-5 LOW (pre-existing, outside NA-6 scope)

**The anchor check runs in one direction only.**
- Location: `internal/db/app_environment.go:55`.
- A `staging` or `development` guard against a database anchored `production` checks nothing.
- So a staging process misconfigured with a production `DATABASE_URL` and test-support routes would run its MOCK
  settlement/activation routes against production.
- Fix: non-production guards refuse the `production` anchor.

### F-6 INFO

- **Unsigned denial rows.** Attacker A can append fake DENY history. WD053 is bound to the transaction, so this has no effect on
  dispatch; it only affects how far the compliance history can be trusted.
- **Proof reuse after a rollback.** A proof whose use rolled back can be reused within 60 s, for the same row only, by anyone
  who reads parameter logs. pgaudit `log_parameter` must stay off for the runtime role, as `rekey-actor-proof.sh` already
  enforces for the rekey session (ADR 0110 s9.5).

## 4. Conditions for the freeze (orchestrator)

1. Reword SRF2R-3 / 6.10 to: "statement-line vector CLOSED. Deposit tombstones remain forgeable by the runtime credential (F-1),
   tracked as an open residual". Do not register it as fully closed against attacker A.
2. Register F-2 as an OPEN ledger-finance item that blocks real-money FirstDepositOnly offers.
3. Register the F4 residuals of section 5 explicitly in the R1 launch-risk row.
4. Track F-3, F-4 and F-5 as LOW follow-ups. They do not block the freeze.

## 5. Accepted residuals (threat model / impact / detection)

| # | Residual | Threat model | Impact | Detection |
|---|---|---|---|---|
| R-1 | T6: whoever holds the app signing key (code execution in platform-api, or the secret store) can sign an allow | outside A; process compromise | any gate decision can be attested | key-access audit (secret store); AP00x in the DB log only for malformed attempts |
| R-2 | Approvals are not proof-bound (ADR 0113 s9.3) | A forges `withdrawal_approvals` and the `approved` state | a request lacking genuine four-eyes approval becomes dispatchable. It still needs the app to dispatch (staff T1p or sweeper), and the gate is then genuinely evaluated and signed. **Flag for owner: decide before production launch.** | no dedicated detective today; recommend an approvals-vs-staff-session reconciliation |
| R-3 | Gate input governance (SF4-6): the runtime can UPDATE `player_accounts`/`wallets` status and `risk_rules.status`/`effective_until` (verified: the immutable-core trigger allows these columns) | A weakens inputs, then waits for a legitimate dispatch | the app evaluates the weakened inputs and signs a genuine allow | audit of status/rule changes made outside the app paths: none for raw SQL; recommend a trigger-level audit |
| R-4 | Chargeback/negative event between the gate read and the transition (F4-R2-NEGBAL-SQL withdrawn) | race (C, or ordinary timing) | payout of a negative wallet inside one claim window | hourly drift and reconciliation; ledger-finance follow-up |
| R-5 | A proof seen in transit or in logs is reusable within 60 s for exactly its row | log reader | the same allow the app decided | nonce table |
| R-6 | F-1 tombstone forgery | A (+ B for the credit) | MA020 barrier removed; a genuine deposit is rejected | none today (recommend a tombstone-vs-receipt detective) |
| R-7 | SRF2R DoS cost: a held advisory lock costs 15 s per pair per sweep; Decide pre-reads up to providers x 50 000 HMACs | A | slower sweeps and M2 decisions, always loud (P1 `match_stalled` / `cursor_flood`) | P1 alerts (ALERT-DELIVERY-1 still open: nobody is paged) |

## 6. Commands

- Private DB `secrev154005p25747x01` (`priv_db.sh`, head 0172), dropped at the end.
- `priv_test.sh -tags integration -count=1 -p 1 -v -run 'SecRev|TestF4R1' ./internal/withdrawal/`:
  - the 6 F4R1 tests pass;
  - of the 3 scratch probes, 2 pass. The COPY probe failed only because its expected code was AP001 and it got 0A000 (an RLS
    refusal, which is stricter).
  - The scratch file was deleted.
- `priv_test.sh ... -run 'TestMigration0171|TestF4_DB_Catalog' ./internal/withdrawal/`: ok.
- `go test -count=1 -run TestStatic ./internal/actorproof/`: ok.
- `go test -count=1 ./internal/observability/ ./internal/payments/ -run 'Panic|Redacted|Static'`: ok.
- psql probes as runtime: the grants listed in section 2.1, and the F-1 tombstone insert in a rolled-back transaction.
- Not run: full package suites. The orchestrator's verification run covers them.
