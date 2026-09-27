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

---

# Re-review (fix round): HEAD `9324189`

- Fix commits reviewed: `c8a2b76`, `0154459`, `9be59bb`, `3be159d` (ADR 0095 §27.11), `73e0a77`,
  plus the coordinator's post-merge `f99d940`. The review also covers code that now shares these
  files: the kill-switch predicates (migration 0105, `attempt.go`) and the callback cutover
  (`receipt.go`).
- Environment: a detached worktree and a private database `igaming_lf_rr_payout`. It was created
  via `TEST_ADMIN_DATABASE_URL`, given the `deploy/init-app-role.sql` runtime-role grants, and
  migrated 1→105. Both the worktree and the database have been removed. Per-test `scratchdb`
  databases drop themselves on cleanup.
- Suites on the private DB: `internal/payments`, `internal/withdrawal` and the `internal/httpserver`
  withdrawal tests all pass. The pinned `golangci-lint` 2.9.0 reports 0 issues, both untagged and
  with `integration`.

## Re-review verdict: APPROVE WITH CONDITIONS

The veto on C1 is lifted. My original probes show every original Critical and High finding fixed,
and the mutants that previously survived are now killed. No path I could construct pays out twice,
or moves money twice or with the wrong amount.

This round introduced one new High finding (R1) and two Mediums (R2, R3), which all come from how
the new phase-C late-evidence routing interacts with the callback cutover and `/resolve`. None of
them moves money incorrectly. They leave a held withdrawal stranded (hold not released, or parked
as `disputed`) with no automated or manual exit while M2 is BLOCKED. For that reason they are
conditions to fix before the payout path is enabled for real traffic, not grounds to reject again.

## Original findings: verified status

| Finding | Claim | Evidence (re-run probe or mutant) | Status |
|---|---|---|---|
| C1 unguarded T12 resend | Poll first, then T12 only if `IdempotentSubmission` and under the cap | Probe A, non-idempotent: **1** `Withdraw` over 8 sweeps, then escalated. Probe A, idempotent: **3** calls (1 + 2 resends), then escalated. Mutant MC (manifest check removed) killed by `TestSweeper_T12_NonIdempotentManifest_NeverResends`. MF+MG (both cap checks removed) killed by `…StopsAtMaxResubmits`. The kill-switch predicate is inside the T12/T2/T1p CAS statements. | **Fixed** |
| H1 resend outcome rejected by the guard | `EvidenceSync` | Probe B: a T2 resend decline gives withdrawal `failed`, cash back to 100 000, hold 0. Mutants MA/MB (Complete/Fail removed) killed by the concrete-balance tests. | **Fixed** |
| H2 no amount check on QueryStatus success | `applyPayoutSuccessCheckedFromStatus` disputes on mismatch | Probe D: confirmed 400 vs 500, and confirmed 50 000 vs 500 (×100). Both give `disputed` with `amount_asset_mismatch`, the withdrawal stays `submitted`, and nothing posts. Mutant ME killed. | **Fixed** |
| H3 stranded claim | `next_action_at` set; no-reference `submitting` becomes `ambiguous` | Probe E: `next_action_at = lease_until` at T1p. After lease expiry the sweeper moves it to `ambiguous`, then escalates it (non-idempotent), with 0 `Withdraw` calls and the hold held. That is fail-closed. The attempt remains unresolvable until CP-W1 (merchant-reference query) or M2 exists; see residual R5. | **Fixed (with disclosed residual)** |
| H4 `/resolve` held a tx across QueryStatus and bypassed attempts | `PollPayoutStatus` is the single entry point | Code: the handler reads and commits, then calls `PollPayoutStatus`, which uses no tx across the call. Attempt and withdrawal now transition together. The Stage 3C resolve tests still pass. See R2 for a new timing defect. | **Fixed (see R2)** |
| M1 pending poll errors | State-aware handling | Probe C: a pending poll reschedules (`poll_count` 0→1), with no error. | **Fixed** |
| M2 NotSent on a resend | Routed by `EverPossiblySent` | Probe I: T12 plus a credential failure goes back to `ambiguous` with 0 calls. Mutant MH (branch disabled) **survives**: no test covers it. | **Fixed, untested (condition C-T1)** |
| M3 lock order | "attempt CAS before the ledger" | Attempt CAS now precedes the posting, but the leading withdrawal `FOR UPDATE` was **dropped**, which inverts parent and attempt. See R3. | **Regressed (R3)** |
| M4 late contradicting evidence | T14 / T10 with a P1 audit | Probe G: a sync success after a sync decline gives `disputed`, no money moved, and an audit record. | **Fixed for the sync path (see R1 for over-reach)** |
| M5 kill switch | Fail-closed at T1p/T2/T12 | Code: an in-statement `NOT EXISTS` in `InsertSubmittingAttempt`, `ClaimCreatedForSubmission` and `ResubmitAmbiguous`. A T1p refusal rolls back `MarkSubmittedPending` (the withdrawal stays `approved`). A T2/T12 block reschedules. Covered by `TestClaimForDispatch_KillSwitchEngaged_FailsClosedNoWithdraw` and `…T2Reclaim_KillSwitchEngaged…`. | **Fixed** |
| Outer lock pinned | Lock-presence test added | `TestClaimForDispatch_GateRunsUnderOuterLock` kills mutant MD. But when it fails it calls `t.Fatalf` without releasing its lock-holder goroutine, so the run hangs until `go test`'s timeout instead of failing fast (minor; condition C-T3). | **Fixed** |
| Double submit | n/a | Probe F: 50 reps of concurrent submit+dispatch give 1 success and 1 `Withdraw` per rep; balanced; projection = rebuild. | **Holds** |

Mutant summary for this round: MA, MB, MC, MD, ME and MF+MG are killed. MF alone and MG alone
survive, which is acceptable because they are redundant copies of the same cap (DB CAS and Go
check). MH survives.

## New findings from this round

### R1 (High): a routine async payout is parked as `disputed` when a provider callback arrives during phase B

`payoutHandleContradiction` → `applyPayoutLateEvidence` routes **any** `ErrAttemptStateConflict`
from `MarkAccepted`/`MarkAmbiguousFromSubmitting` on a non-terminal attempt to T10 `disputed`
(§27.11 item 4: "T10 (any other unexpected state)"). The callback cutover makes that conflict
routine: a verified `pending` webhook often arrives before the synchronous `Withdraw` response.
- Probe J: callback `pending` moves the attempt `submitting→pending`. The sync `Pending` result
  then fails `MarkAccepted` (it does not accept `pending`) and the attempt ends **`disputed`**.
- Probe K: the same, but the sync call times out (`Ambiguous`). The attempt again ends
  **`disputed`**.

In both cases the provider holds a healthy accepted payout, while the platform has terminally
parked the attempt, left the hold held, and made it unresolvable except by M2 (BLOCKED). If the
payout then settles, the success callback finds a `disputed` attempt and cannot settle it. The
money has left, and the hold is never debited to clearing, so the ledger and the PSP drift apart.
Probe M shows `/resolve` (R2) produces the same outcome.

Required: a weaker-or-equal piece of evidence arriving after a stronger non-terminal state is
**not** a contradiction. Specifically:
- Sync `Pending` on `pending`: no-op or reschedule.
- Sync `Ambiguous`/timeout on `pending` or `ambiguous`: no-op or reschedule. Transport silence is
  not the provider "forgetting".
- Sync `NotSent` on a non-`submitting` attempt: no-op.

Only definite outcomes that contradict a **terminal** state (success after decline gives T14;
decline or success conflicting with a different terminal state) should dispute. Add probes J/K/M as
tests.

### R2 (Medium): `/resolve` transitions an in-lease `submitting` attempt, racing its own phase B

`PollPayoutStatus`, for `submitting` with no reference, runs `MarkAmbiguousFromSubmitting` without
checking `lease_until`. The sweeper only reaches such a row after its lease is due, but `/resolve`
calls it at any time (probe M). A staff member pressing Resolve while phase B is in flight:
- moves the attempt to `ambiguous` and sets `ever_possibly_sent`;
- then, if phase C's result is `Ambiguous`, parks it as `disputed` (R1);
- if the result is `NotSent` (provably never sent), phase C fails its CAS and the attempt stays
  `ambiguous` with `ever_possibly_sent=true`. A never-sent payout thereby loses M3 eligibility and,
  on a non-idempotent provider, any resend path.

Required: a `submitting` attempt whose `lease_until > now()` must be refused by `/resolve` (409,
"dispatch in progress") and left untouched by `PollPayoutStatus`, as a predicate inside the T6 CAS
statement.

### R3 (Medium): phase C now takes attempt → withdrawal, inverting A7's parent-before-attempt; a real deadlock

ADR 0082 A7 fixes payout evidence as "withdrawal `FOR UPDATE` → attempt CAS → L3 → L4". My original
M3 asked for the attempt CAS to move before the **posting**, not before the **parent lock**.
`ApplyPayoutResult`/`applyPayoutStatusEvidence` no longer lock the withdrawal first, and the branch
order is inconsistent: success, decline and pending lock the attempt first, while the ambiguous
branch locks the withdrawal first. The receipt path and the T2/T12 claims both take
withdrawal → attempt.
- Probe N: one tx held the withdrawal row (the receipt-path order) and then updated the attempt,
  while phase C applied a sync success. Result: **`deadlock detected (40P01)`**; phase C was the
  victim.
- The money stays correct, because the tx is atomic. But the aborted phase C loses the sync
  success and its reference. The attempt then falls to lease expiry, then `ambiguous`, then
  escalation, even though the provider paid, unless a callback happens to rescue it.

Required: restore `SELECT … FROM withdrawal_requests WHERE id = $1 FOR UPDATE` as the first
statement of both phase-C functions, then the attempt CAS, then the posting. §27.11 item 4's
wording ("A7's attempt-before-posting lock order") should be corrected to state the full order.

### R4 (High, callback cutover coexistence): a payout decline delivered by callback never releases the hold

`applyResolvedReceiptEvidence`'s `OutcomeDeclined` branch calls `ApplyDecline` on a payout attempt
but never `withdrawal.Fail`. Its success branch still records `payout_success_not_yet_wired`
(disputed), so payout settlement via callback is still unwired.
- Probe L, event type `deposit` naming a payout reference: the attempt becomes `declined`
  (terminal, `next_action_at` NULL) while the withdrawal stays `submitted` with the hold of 500
  held. `GetLiveAttemptForWithdrawalRequest` then finds nothing, so `/resolve` answers "will be
  picked up automatically", which never happens, and the sweeper never sees the row. The hold is
  permanently stranded. The receipt path also never checks `ev.EventType` against
  `attempt.Operation`.
- Probe L, event type `payout`: the receipt INSERT violates `payment_provider_events_check1`
  (decline fields are populated only for `deposit` events). The callback returns an error, is
  redelivered forever, and nothing is recorded (INV-IO-10).

This is the code the orchestrator asked me to check for coexistence. The payout phase-C logic is
right, but the callback path bypasses it. Required, before payout callbacks are enabled for any
provider:
- route payout receipts through the same `applyPayoutDecline`/`applyPayoutSuccess` (withdrawal lock
  first, per R3);
- reject or record-as-anomaly any event whose type does not match `attempt.Operation`;
- make the payout receipt row satisfy its CHECK.

### R5 (Medium, disclosed residual): a crash after T1p ends escalated but unresolvable

This is the H3 path (probe E). A payout that was never actually sent ends `ambiguous` and escalated
with `ever_possibly_sent=true`, so it is not M3-eligible. On a non-idempotent provider it cannot be
resent, and without a reference it cannot be polled. Its only exits are CP-W1 (merchant-reference
QueryStatus, disclosed NOT IMPLEMENTED in §27.11) and M2 (BLOCKED on HD-0095-1). This is correct
fail-closed behaviour, but the orchestrator must track it: CP-W1 is a launch condition for any
provider whose manifest is non-idempotent, and HD-0095-1 remains a human decision.

### R6 (Low): the ambiguous branch records the reference on the withdrawal, not the attempt

When the adapter returns a reference alongside an ambiguous outcome, `ApplyPayoutResult` attaches
it to the withdrawal only. `MarkAmbiguousFromSubmitting` never stores it on the attempt (probe A
shows the attempt's `provider_reference` is NULL). T12's poll-first step reads
`attempt.ProviderReference`, so it cannot poll and goes straight to escalation or resend. Store the
reference on the attempt too (it is set-once on NULL).

## Runbook check: `docs/runbooks/migration-0101-payment-attempts-remediation.md`

**Policy matches the ruling:** fail-closed, no bypass flag, 0101 never edited, lost-link repair
through a four-eyes audited script, missing credit handled as P1 via the normal posting path, test
databases rebuilt rather than patched, and the down/up consequence stated. **The procedure has
defects that would mislead an operator.** Fix them in the runbook (it is not a migration):

1. **§3(a)'s query is wrong and cannot run pre-0101.** It filters on `NOT EXISTS (… FROM
   payment_attempts …)`, but that table does not exist before 0101. It also does not mirror the
   pre-flight predicate, which is `status = 'succeeded' AND (ledger_transaction_id IS NULL OR
   provider_reference IS NULL)`. §1 also describes the check as "no provider reference **and** no
   ledger link"; it is **or**.
2. **Wrong RLS setting.** "`SET app.current_tenant_id` per tenant" should be `app.tenant_id`, with
   `app.player_account_id` cleared, as 0101 itself does. With the wrong setting, FORCE RLS returns
   zero rows, so the operator sees a false "nothing to fix". That is exactly the 0048 lesson that
   0101's own comments cite.
3. **§3(b-i) covers only a missing ledger link.** Rows with a ledger link but a NULL
   `provider_reference` are also flagged, and the runbook sends them to "investigate" with no
   procedure. Add one: attach `provider_reference` from the matched transaction's `provider_tx_id`.
   The column is mutable per 0082, and the same four-eyes/audit rules apply. Also drop the
   "if this column exists on your checkout" hedge: `deposit_intents.ledger_transaction_id` has
   existed since 0025.
4. **§3(c) names a non-existent status.** It says "e.g. `disputed`", but `deposit_intents.status`
   allows only `pending|succeeded|declined|ambiguous|failed`. After PSP confirmation that nothing
   was captured, the target is `failed` (or `declined`), with evidence.
5. **Pre-flights 1 and 3 are not covered.** They are pending/ambiguous intents with neither
   provider id nor reference, and `submitted` withdrawals with no reference. Add a line for each
   (classify with the PSP; never guess), and correct §4's wording: the NULL-reference `submitted`
   shape is pre-flight 3's **withdrawal** check, not "the shape … for a deposit intent".

## Conditions for final sign-off on the payout money path

- **C-R1:** R1, R2, R3 fixed, with probes J, K, M and N converted to tests.
- **C-R4:** R4 fixed before any payout callback is accepted from a real provider. Until then, payout
  callbacks must be refused or recorded as anomalies.
- **C-T1:** a test for NotSent after a resend (mutant MH must be killed).
- **C-T3:** the lock-presence test must release its holder before `t.Fatalf`.
- **C-RB:** runbook items 1 to 5 corrected.
- Tracked (orchestrator): R5 (CP-W1 as a launch condition for non-idempotent providers;
  HD-0095-1 still BLOCKED), R6 (Low).

---

# Re-review 2 (payout round 3): `efc63f5`, merged at `07094a3`

- Environment: a detached worktree at `07094a3` and a private DB `igaming_lf_rr3_payout` (admin
  create, runtime-role grants from `deploy/init-app-role.sql`, migrated 1→106). Both have been
  removed.
- Suites on the private DB: `internal/payments` (full), `internal/withdrawal` and the
  `internal/httpserver` withdrawal tests pass. The concurrency, round-3 and lock-order subset passes
  under `-race`. The pinned `golangci-lint` 2.9.0 reports 0 issues, both untagged and with
  `integration`.

## Verdict: APPROVE WITH CONDITIONS (conditions narrowed to R4 and the tracked items)

Every round-3 condition I set is met, and the probes back it up. The only open money-path
condition is **R4** (payout callbacks through `receipt.go`), which the orchestrator assigned to the
callback agent and which this round does not touch. My probe L still reproduces it.

## Probe results on `07094a3`

| Probe | Round-2 result | Round-3 result |
|---|---|---|
| J: callback `pending` during phase B, then sync `Pending` | `disputed` | **`pending`**, no dispute |
| K: callback `pending` during phase B, then sync timeout | `disputed` | **`pending`**, no dispute |
| M: `/resolve` while phase B in flight (3 phase-C outcomes) | moved to `ambiguous`; could end `disputed`, or strip M3 from a never-sent payout | **Refused with `ErrPayoutDispatchInFlight` (409)**; attempt stays `submitting`. Phase C then applies normally: ambiguous→`ambiguous`, NotSent→`created` (`ever_possibly_sent=false`, M3 kept), pending→`pending`. |
| N / 40P01: tx holding the withdrawal row (receipt-path order), then updating the attempt, racing phase-C sync success | **`deadlock detected (40P01)`**, phase C aborted | **No deadlock**: phase C waits on the withdrawal lock and then commits |
| A: double payout, non-idempotent / idempotent | 1 / 3 calls | 1 / ≤3 calls. The reference returned with an ambiguous result is now stored on the attempt (R6/N1). |
| B, C, D (400 and ×100), E, F, G, I | pass | pass, unchanged |
| L: payout decline via callback | hold stranded | **Still stranded (R4, out of scope, open)** |

## Round-2 conditions

- **R1: closed.** `payoutHandleContradiction` sends only `Succeeded`/`DefiniteDecline` into
  late-evidence dispute. A weaker result on a state conflict is a no-op for a terminal attempt, or
  a reschedule for a non-terminal one. Covered by the three R1 tests and my J/K probes.
- **R2: closed.** Two layers: a Go-level lease check returns `ErrPayoutDispatchInFlight`, which the
  handler maps to 409, and the no-reference T6 CAS carries `lease_until <= now()` in its own
  predicate. Low residual (not a condition): the Go check reads a snapshot. If a T12 claim commits
  between `/resolve`'s read (state `ambiguous`) and its poll, the status is applied against the stale
  state. Every transition that can result is still one ADR 0095 allows for an in-flight resend, and
  the resend's own phase C converges without dispute (R1), so money is unaffected.
- **R3: closed. The lock order now matches A7 everywhere I checked:**
  - Phase C (`ApplyPayoutResult`) and status apply (`applyPayoutStatusEvidence`): withdrawal
    `LockForPayoutEvidence` first, then the attempt CAS, then `Complete`/`Fail` (L3/L4).
    `applyPayoutSuccess`/`applyPayoutDecline` re-lock the same row, which is harmless.
  - T1p (`ClaimForDispatch`): withdrawal lock, then KYC gate reads, then the attempt INSERT (with
    the kill-switch predicate).
  - T2/T12 (`reclaimPayoutCreated`, `resubmitPayoutAmbiguous`): `LockSubmittedForResolution`
    (withdrawal), then the kill-switch check and KYC gate, then the attempt CAS.
  - Receipt path: a withdrawal `FOR UPDATE` (line 351) precedes every attempt write. The earlier
    receipt inserts touch only `payment_provider_events`.
  - The only attempt-only transactions left are the sweeper's `claimBatch` (`SKIP LOCKED`, the
    ADR's named exception) and single-statement reschedule/escalate/T6 updates. Those take one row
    lock and nothing after it, so they cannot join a cycle. Probe N confirms there is no 40P01.
- **C-T1: closed.** `TestPayoutDispatch_CT1_NotSentOnResendRoutesToAmbiguousNotCreated`.
- **C-T3: closed.** The outer-lock test now releases its holder and joins it before `t.Fatalf`.
- **N6 (reference mismatch disputes): verified in code**
  (`applyPayoutSuccessCheckedFromStatus`), with a test.
- **R5 (CP-W1):** recorded in ADR 0095 §27.12 as a launch condition. That is accepted as tracked.
  HD-0095-1 stays BLOCKED.

## Runbook against my five items

1. **Closed.** The §3(a) queries touch only pre-0101 tables and mirror all three pre-flight
   predicates, including pre-flight 2's `OR`. The columns (`created_at`, `requested_at`) exist.
2. **Closed.** It says `app.tenant_id` with `app.player_account_id` cleared, and explicitly warns
   against `app.current_tenant_id`.
3. **Closed.** Case (b-ii) now covers a present link with a missing reference, via the matched
   `provider_tx_id`. The "if this column exists" hedge is gone. Nit, no action required:
   `provider_reference` is *mutable* under 0082, not "immutable-once-set" as the text says. The
   `COALESCE`-only script is still the safer choice.
4. **Closed.** The terminal status is `failed`, with `disputed` explicitly ruled out.
5. **Closed.** Pre-flights 1 and 3 each have a PSP-confirmed procedure. §4 correctly names
   pre-flight 3 as the withdrawal shape.

## Remaining conditions

- **R4 (High, owner: callback agent)** must be fixed before any payout callback from a real provider
  is accepted. Until then payout webhooks must be refused or recorded as anomalies. Probe L still
  shows:
  - A `deposit`-typed decline naming a payout reference declines the attempt without releasing the
    hold, and leaves no live attempt for `/resolve` or the sweeper to recover it.
  - A `payout`-typed decline violates `payment_provider_events_check1` and is redelivered forever.
  - The receipt path never checks the event type against the attempt's operation.
- **Tracked:** CP-W1 (§27.12 launch condition for non-idempotent providers); HD-0095-1
  (M1/M2 BLOCKED).
