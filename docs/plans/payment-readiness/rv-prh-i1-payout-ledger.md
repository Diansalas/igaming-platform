# RV-PRH-I1 payout dispatch: ledger-finance review

- Reviewer: `ledger-finance` (payments/ledger reviewer; not the implementer)
- Branch / HEAD: `claude/focused-wright-jw88w9` @ `dc4f6d9`
- Commits reviewed: `c7d24b9` (feature), `722a860`, `e3d388a` (tests, mutation evidence)
- Files: `internal/payments/payout.go`, `internal/payments/payout_sweep.go`,
  `internal/payments/sweeper.go` (dispatch hook), `internal/withdrawal/withdrawal.go`
  (`MarkSubmittedPending`, `AttachProviderReference`, `DenyForCompliance`,
  `LockSubmittedForResolution`, `Complete`, `Fail`), `internal/httpserver/withdrawal_handlers.go`
  (submit and resolve handlers), `internal/payments/payout_dispatch_integration_test.go`
- Design baseline: ADR 0095 §4.3 (T1p, T2, T6, T12, W-KYC), §5.2, §5.2.1, §8, §16.1 (CP-W1..W8);
  ADR 0082 Amendment A7; `docs/architecture/withdrawal-state-machine.md`. HD-0095-1 (M1/M2) remains a
  BLOCKED human decision and is not ruled on here.

## Verdict: REJECT

The HTTP submit path (T1p, then phase B with no transaction, then phase C) is structurally sound.
Hold, settle and release postings are balanced. Settle and release are each exactly-once, and they
exclude each other under replay, and under a late decline after success or a late success after
decline. Probe G below verifies this. Staff double-submit is safe (probe F: 50 reps, exactly one
`Withdraw` per request).

The payout **sweeper** path is not safe, though. It resends an ambiguous payout with no manifest
check and no resend cap. That breaks CLAUDE.md's idempotency and no-double-payout rules and ADR 0095
T12 directly, and I veto it under my authority over money paths. Several other defects leave funds
held with no recovery path, or complete a payout for an amount the provider did not confirm. The
test suite does not exercise the settle or release posting at all: removing `withdrawal.Complete` or
`withdrawal.Fail` from phase C leaves every payout test passing.

Mitigating fact: `Sweeper.PayoutKYCGate` is not set anywhere in `cmd/` (the sweeper is not
constructed in production wiring), so C1 is latent today. It is still the code under review, and
enabling it takes one field assignment.

## Method

- Read the code against ADR 0095 §4.3/§5.2/§8 and ADR 0082 A7.
- Ran the implementer's payout suite on HEAD (per-test private scratch DBs via
  `internal/testsupport/scratchdb`, created through `TEST_ADMIN_DATABASE_URL` and dropped with
  `WITH (FORCE)` on cleanup; the shared test DB was not used). Result: pass.
- Wrote 8 probe tests and 3 mutants in a separate detached worktree
  (`scratchpad/wt-lf-payout`, since removed). No change was made to the main tree except this file.

| Probe | What it checks | Result on HEAD |
|---|---|---|
| A | Ambiguous payout on a manifest with `IdempotentSubmission=false`, 6 sweeper passes | **7 `Withdraw` calls for one withdrawal**, submit_count 7 |
| B | T2 re-claim whose resend gets a definite decline | Guard trigger rejects `->declined` with evidence `sweeper`; attempt stays `submitting`, hold stays held |
| C | Sweeper polls a still-`pending` payout | `MarkAccepted` CAS conflict error, no reschedule or backoff |
| D | QueryStatus success with provider-confirmed amount 400 vs requested 500 | Withdrawal **completed for 500** |
| E | Attempt row right after T1p commit | `next_action_at` NULL; `RunOnce` claims 0 |
| F | 2 concurrent submit+dispatch per request, 50 reps | Pass: 1 success, 1 `Withdraw`, 1 attempt per rep; balanced; projection = rebuild |
| G | Sync success/decline, then replayed and contradicting phase C | Pass for money: no second posting. The late contradicting evidence is dropped as an error rather than recorded as T14 |
| I | T12 resend where the gate returns NotSent | T5 CAS fails; attempt stuck `submitting` with `ever_possibly_sent=true` (ADR requires T6) |

| Mutant | Effect | Implementer suite |
|---|---|---|
| M1 | `ClaimForDispatch` outer `LockApprovedForSubmission` replaced by unlocked `GetByID` + state check | Survives (as disclosed); probe F also passes |
| M3 | `withdrawal.Fail` removed from phase C decline branch (hold never released) | **Survives** |
| M4 | `withdrawal.Complete` removed from phase C success branch (payout never settled) | **Survives** |

## Findings

### Critical

**C1. The sweeper resends an ambiguous payout with no `IdempotentSubmission` check, no `max_resubmits` cap and no kill-switch predicate (double payout).**
`payout_sweep.go` `processPayoutAttempt` sends every `ambiguous` payout attempt to
`resubmitPayoutAmbiguous`. That function runs the KYC gate and then `ResubmitAmbiguous` (T12), then
calls `Withdraw` again. Neither the Go code nor the CAS predicate checks the manifest's
`IdempotentSubmission`, `submit_count < max_resubmits`, or the INV-IO-15 kill switch. ADR 0095 §4.3
T12 requires all three ("Only if the manifest has `IdempotentSubmission=true`, `submit_count <
max_resubmits`…"), and so does §8's `Ambiguous` row ("Only T12 (same key, `IdempotentSubmission`)").
`attempt.go`'s own doc says the caller must check them. The deposit sweeper documents T12 as out of
scope; the payout sweeper added it without those guards.
- Failure scenario (probe A): a PSP times out after executing a bank payout, so phase C records T6.
  30 s later the sweeper resends. The PSP does not dedupe on `pa:<id>`, so it pays again. Each
  resend times out again and the loop repeats. Probe A saw 7 payouts for one 333-unit withdrawal
  after 6 sweeps. Only one hold exists and at most one `Complete` can post, so the extra payouts are
  unrecorded losses that never show in ledger drift.
- The trigger makes it worse. `payoutAdapterCall` classifies `OutcomeSucceeded` with an empty
  reference as `Ambiguous`, so a provider-confirmed success can also be resent.
- The ambiguous branch of `ApplyPayoutResult` also discards the provider reference the adapter
  returned (probe A: adapter returned a ref, but the attempt's `provider_reference` is NULL). That
  makes QueryStatus resolution impossible and pushes the attempt toward resend. CP-W1's "QueryStatus
  by merchant reference" is not implemented either.
- The implementer's own mutation evidence (PM-PAYOUT-7) shows a T12 resend reaching `pending` on
  the default (non-idempotent) mock manifest. Nobody flagged it.
- Required: T12 only when `manifest.IdempotentSubmission && submit_count < max_resubmits && NOT
  legacy_backfill && NOT EXISTS(engaged switch)`, with the switch predicate inside the CAS statement.
  Otherwise ambiguous converges only through QueryStatus or callback (by provider reference, or by
  merchant reference where the manifest allows), then T16. Persist any reference returned alongside
  an ambiguous outcome. Add a test: in non-idempotent mode T12 never fires, and in idempotent mode it
  stops at `max_resubmits` (ADR §16.1 item 2 already requires this).

### High

**H1. Every definite outcome on a sweeper-driven resend (T2 or T12) is rejected by the DB guard, so a paid payout goes unrecorded.**
`dispatchPayoutAttempt` passes `EvidenceSweeper` to `ApplyPayoutResult`. The 0101 guard allows
`->succeeded`, and a payout `->declined`, only with evidence in (`sync`, `callback`, `query_status`).
The outcome of the resend's own `Withdraw` call is `sync` evidence.
- Failure scenario (probe B): after a T2 re-claim, the PSP declines synchronously. Phase C raises
  P0001 and rolls back the whole transaction, including `Fail`'s release. The attempt stays
  `submitting` with no reference and the hold stays held. On a synchronous **success**, `Complete`
  is rolled back the same way: the money has left, the ledger never records it, and the reference is
  lost. Every later sweep then hits "no reference → reschedule" forever. There is no T16, and M2 is
  BLOCKED. Fail-closed for double-release, but it strands funds and hides a completed payout from
  the ledger.
- Required: phase B/C of a resend uses `EvidenceSync`, plus a test that drives T2 and T12 to a
  definite success and a definite decline and asserts the resulting balances.

**H2. The sweeper's QueryStatus resolution completes a payout without the amount/asset cross-check (INV-IO-6).**
The doc comment on `resolvePayoutViaQueryStatus` says it cross-checks amount and asset "identical to
internal/httpserver's own resolve handler". It does not: `StatusResult.Amount/AssetCode` are dropped
when converted to `WithdrawResult`.
- Failure scenario (probe D): the provider confirms 400 for a 500 request. The sweeper posts
  `withdrawal_completed` for 500 (hold → clearing), and the ledger now disagrees with the PSP by 100
  with no dispute record. Required: mismatch leads to T10 `disputed` with a receipt, never
  `Complete`.

**H3. A crash, timeout or phase-C failure after T1p strands the withdrawal with no sweeper visibility (CP-W1 not met).**
`InsertSubmittingAttempt` leaves `next_action_at` NULL (probe E), and `claimBatch` selects only on
`next_action_at`. Several things leave the attempt in `submitting` forever:
- a process crash between the T1p commit and phase C;
- a phase-C error (the 5 s `payoutPhaseCTimeout`, a deadlock victim, a ledger idempotency conflict
  from a provider reusing a reference, or H1/H4);
- the handler returning 500 ("check /resolve").

In each case the withdrawal stays `submitted` with no provider reference. `/resolve` refuses it
because it needs `wr.ProviderReference`; its comment calling that "structurally unreachable" is now
false, since P95-C2 makes it the normal T1p shape. No escalation fires. ADR 0095 §4.3 T6 lists "lease
expired and QueryStatus not definitive" as a T6 trigger, and CP-W1 requires a sweeper QueryStatus by
merchant reference, else `ambiguous`. Required: T1p/T2/T12 set `next_action_at = lease_until`.
The sweeper maps a lease-expired `submitting` payout to QueryStatus (by merchant reference where
supported), else T6, and applies T16 past `SettlementWindow`. It never resends outside C1's T12
rules.

**H4. The resolve handler still calls `provider.QueryStatus` inside the DB transaction while holding the L1 row lock, and it bypasses `payment_attempts`.**
`newResolveWithdrawalHandler` was edited in `c7d24b9` (reference validation added) but still runs
QueryStatus inside `deps.DB.WithTenant`, holding `withdrawal_requests FOR UPDATE` across I/O. That
breaks INV-IO-1 ("no adapter-method call runs while a transaction is held") and A7's "No lock is
held across I/O". It also calls `withdrawal.Complete/Fail` without the matching attempt transition.
- Failure scenario: staff resolve completes the withdrawal while the attempt stays
  `ambiguous`/`pending`. The sweeper then fails every tick: T12 fails at
  `LockSubmittedForResolution`, and QueryStatus fails at `AttachProviderReference`. Attempt and
  withdrawal state diverge permanently, and reconciliation over `payment_attempts` misreports.
  After a staff `Fail`, a late success callback has no T14 path through this handler either.
- Required: route `/resolve` through the same phase-B/phase-C functions (`ApplyPayoutResult`
  with `EvidenceQueryStatus`) with no transaction held across the call.

### Medium

**M1. Still-pending polls error instead of rescheduling (probe C).** `ApplyPayoutResult`'s Pending
branch calls `MarkAccepted`, which only accepts `submitting|ambiguous`. A `pending` attempt polled as
still pending, or a transport error on a pending attempt (which calls
`MarkAmbiguousFromSubmitting`), returns a CAS error. No backoff advances and no T16 fires, so the
sweeper logs an error for every pending payout every lease period until settlement. The QueryStatus
path needs its own evidence mapping, with `RescheduleNonTerminal` for "still pending" and T11 for
not-found or ambiguous on a pending attempt, matching the deposit `processViaQueryStatus` matrix.

**M2. NotSent on a T12 resend takes T5 instead of T6 (probe I).** ADR §4.3 T6 and §8 (LF95-C1)
send it back to `ambiguous`. `MarkNotSent`'s `NOT ever_possibly_sent` predicate prevents the wrong
transition, which is fail-closed. But the transaction errors and the attempt is left `submitting`,
which feeds H3. Branch on `attempt.EverPossiblySent`.

**M3. Phase-C lock order deviates from A7.** A7 specifies payout evidence as "withdrawal `FOR
UPDATE` → attempt CAS → L3 → L4". `ApplyPayoutResult` instead runs `Complete`/`Fail` (L3/L4
postings) **before** `ApplySuccess`/`ApplyDecline` (the L1 attempt row). Separately, the receipt path
(`receipt.go`) applies payout evidence by locking the attempt without locking the withdrawal first.
No cycle exists today, because the payout-success callback only disputes. Once the success callback
is wired to `Complete`, it would lock attempt → withdrawal while phase C locks withdrawal → attempt:
a deadlock whose victim then falls into H3. Fix the order now: attempt CAS first, then the posting.

**M4. Late contradicting evidence is dropped rather than recorded (probe G).** A success arriving
after a payout was declined and its hold released (a double payout has occurred) makes phase C fail
on `AttachProviderReference`/`Complete` with `ErrStateConflict`. The whole transaction rolls back,
so no T14 `disputed` state and no receipt are recorded. The money is correct, because nothing posts,
but the P1 signal ADR §4.3 T14 requires exists only as a log line. Same for the payout-success
callback, which marks `payout_success_not_yet_wired` disputed, so a genuine success resolves to a
dispute that only M2 (BLOCKED) can resolve.

**M5. Kill switch (INV-IO-15, S95-C6 launch-blocking) is absent from T1p, T2 and T12.** It is a
known residual on the deposit side as well. It is listed here because ADR 0095 names the payout
claim statement explicitly.

### Low

- **L1.** `ApplyPayoutResult` takes `requestID` and `attempt` independently and never asserts
  `*attempt.WithdrawalRequestID == requestID`. RLS bounds this to one tenant, but a caller bug could
  apply attempt Y's evidence to withdrawal X. Assert it.
- **L2.** A QueryStatus success with an empty echoed reference calls `AttachProviderReference("")`,
  which returns `ErrInvalidInput`, and errors on every tick. Use the attempt's stored reference.
- **L3.** Escalate on an already-escalated attempt at a repeat KYC deny fails its CAS
  (`escalated_at IS NULL`) and errors on every tick. Fail-closed, but noisy.

## Idempotency, tenant scoping, no-tx-across-I/O: confirmed sound (outside the findings above)

- Hold: `RequestWithdrawal` in the same transaction as the row (pre-existing). Release keys
  `<id>:failed` / `<id>:kyc_denied`. Settle key `provider_id:provider_ref` plus
  `UNIQUE(tenant_id, provider_id, provider_tx_id)`. One attempt per withdrawal via
  `payment_attempts_one_per_withdrawal`. External key `pa:<id>` is unique. All are enforced by the
  DB, not by check-then-insert. Settle and release are mutually exclusive through
  `state='submitted'` CAS under L1, and W-KYC and T1p are mutually exclusive through
  `state='approved'` under L1.
- Every write runs in `WithTenant` under FORCE RLS. Tenant comes from the authenticated context in
  the handler and from the tenant loop in the sweeper.
- `DispatchWithdraw` takes no `pgx.Tx`, and `callProvider` step 1 refuses if `txscope.Held`. Phase C
  uses `context.WithoutCancel`. The one violation is the resolve handler (H4).

## Ruling on the implementer's disclosure: removing only the outer lock in `ClaimForDispatch`

**Ruling: safe for money. Keep the lock, and add a test that pins it.**

Probes F and M1 show dispatch exclusivity does not depend on the outer lock. Three independent,
DB-enforced layers remain:
1. `MarkSubmittedPending` takes its own `FOR UPDATE` and re-checks `approved`.
2. Its `UPDATE … WHERE state='approved'`: under READ COMMITTED, a writer that waited re-evaluates
   the predicate after the lock is released and matches 0 rows.
3. `UNIQUE(tenant_id, withdrawal_request_id)` on `payment_attempts`.

`DenyForCompliance` has the same lock and CAS, so W-KYC against T1p stays mutually exclusive.
Under M1, 50 reps of concurrent submit+dispatch gave exactly one `Withdraw` each, and the ledger
stayed balanced with projection equal to rebuild.

What the outer lock does buy is the A7 and LF95-C10(a) ordering: L1 before the gate reads, so the
gate decision is taken against a row nobody else can transition in the meantime. That matters for
decision/audit coherence (no gate verdict recorded for a request that was already
rejected/submitted), not for money. The KYC tables are not serialised by the withdrawal lock either
way, so gate freshness is identical with or without it.

Condition: the current race tests cannot detect removal of the lock, so add one that can. Hold the
withdrawal row `FOR UPDATE` in a separate transaction and assert that a spy `PayoutKYCGate` is not
invoked until that transaction ends. Separately, `TestConcurrent_RejectVsClaimForDispatch` is not a
race test: `Reject` is illegal from `approved` whatever the timing, so it would pass with no locking
at all. Replace it with a claim-vs-claim (CP-W4) test that asserts `Withdraw` call count, attempt
count and hold/release postings on every rep.

## Test adequacy

- M3 and M4 survive: no payout test reaches `Complete` or `Fail`. `TestPayoutDispatch_EndToEnd_Success`
  asserts `pending`, not success. `loAssertBalanced`/`loAssertProjectionMatchesRebuild` are real
  invariants, and the implementer's projection-corruption injection shows the rebuild check is
  load-bearing. But they cannot detect a posting that is **missing**: an unreleased hold is still
  balanced. Tests must assert concrete account balances (player_cash, player_withdrawal_hold,
  psp_clearing) after success, decline, KYC deny, and each replay.
- Missing CLAUDE.md-required coverage for this money path: settlement (success), release (decline),
  duplicate or late callback (success after decline gives T14; decline after success is a no-op),
  QueryStatus resolution including amount mismatch, partial failure (phase-C error), crash after T1p
  (sweeper recovery), T12 in both idempotent and non-idempotent modes, and staff double-submit (CP-W4).
- The 50-rep Deny-vs-Claim race is meaningful and passes. It should also assert per-rep attempt
  count (0 or 1) and exactly one release posting when deny wins.

## Ruling on migration 0101's `succeeded_missing_link` pre-flight

**Ruling: refusing is the correct fail-closed behaviour and must not be weakened. It does need a
documented remediation path, recorded as a runbook, not as an edit to 0101.**

1. **Can it refuse on production-shaped data?** Not through application code. Since 0025 the only
   writer of `status='succeeded'` is `postDepositSuccess` (`orchestrator.go`), which sets
   `provider_id`, `provider_reference`, `status` and `ledger_transaction_id` in one `UPDATE`, in the
   same transaction as `ledger.Post`. `setIntentAttempt` never writes `succeeded`. So a row the
   pre-flight rejects can only come from:
   - raw SQL, meaning test fixtures (for example `payrev1_tenant_isolation_integration_test.go`
     sets `status='succeeded'` by hand; the shared test DB's 198 rows are this kind), or a manual
     DBA "fix" during an incident;
   - a data import or restore from outside the platform;
   - an undiscovered historical bug.

   Every one of those is either non-production data or an existing financial-integrity incident. No
   production exists yet (synthetic data only, per CLAUDE.md). Staging, demo, or any database that
   integration tests have ever run against can realistically trip it.
2. **Why refuse rather than skip or auto-fix.** A `succeeded` intent with no ledger link means
   either the player was told "deposited" with no posting (missing credit), or the ledger row exists
   and the link was lost. With no provider reference, the platform cannot look it up at the PSP.
   Backfilling would either fabricate a `succeeded` attempt that violates
   `CHECK (… state <> 'succeeded' OR ledger_transaction_id IS NOT NULL)`, or invent linkage.
   Skipping would silently drop a money record from the attempt model that reconciliation and
   T13/T13t rely on. Both break "never guess about money". Aborting the whole migration with
   "Nothing was changed" and the offending ids is correct.
3. **Remediation (required before any non-test environment runs 0101).** Write it as an operator
   runbook under `docs/`, executed before `migrate up`, never as a change to 0101 (which is already
   applied in places and must not be modified):
   - (a) List the rows per tenant; the pre-flight already reports up to 20 ids per tenant.
   - (b) For each row, look for a `ledger_transactions` deposit with `correlation_id = intent.id` or a
     matching `(provider_id, provider_tx_id)`. If one exists and the amount/asset match, set the
     missing `ledger_transaction_id`/`provider_reference` (0082's guard allows NULL → value) through a
     reviewed, audited, four-eyes script.
   - (c) If no posting exists, treat it as a P1 reconciliation incident. Confirm with the PSP, then
     either post the missing credit through the normal deposit path or move the intent to a
     non-success status with evidence.
   - (d) For test and scratch databases: drop and recreate. Never hand-patch test rows to satisfy the
     pre-flight. The shared test DB stuck at 100 should be rebuilt, not repaired.

   Deliberately, there is no bypass flag.
4. **Related note.** After this PR, `MarkSubmittedPending` legitimately creates `submitted`
   withdrawals with a NULL `provider_reference`, which is exactly the shape pre-flight 3 rejects. So
   0101 can no longer be re-applied (down, then up) on a database that has run the new payout code.
   That is acceptable, since down-migrating a payments table in a live environment is itself
   forbidden, but the runbook should state it.

## Conditions to lift the rejection

1. C1 fixed and tested in both non-idempotent and idempotent+cap modes. This is a hard veto.
2. H1 through H4 fixed, each with a test asserting concrete account balances.
3. M1 through M3 fixed. M4 and M5 may be tracked with owners and deadlines if the orchestrator
   accepts them.
4. The M3/M4 mutants are killed, a lock-presence test kills M1, and the vacuous Reject-vs-Claim race
   is replaced by a CP-W4 claim-vs-claim test.
5. The 0101 remediation runbook is written.
