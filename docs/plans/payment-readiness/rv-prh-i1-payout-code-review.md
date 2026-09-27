# RV-PRH-I1 — Independent code review: payout dispatch (ADR 0095 T1p / phase B / phase C)

- Reviewer: `code-reviewer` (independent of the implementer)
- Branch / HEAD: `claude/focused-wright-jw88w9` @ `dc4f6d9`
- Commits reviewed: `c7d24b9` (feature), `722a860` (mutation evidence), `e3d388a` (invariant tests + evidence)
- Files: `internal/payments/payout.go`, `internal/payments/payout_sweep.go`,
  `internal/withdrawal/withdrawal.go` (MarkSubmittedPending / AttachProviderReference),
  `internal/httpserver/withdrawal_handlers.go` (submit/resolve), `internal/payments/sweeper.go`,
  `internal/payments/payout_dispatch_integration_test.go`,
  `docs/plans/payment-readiness/evidence/prh-i1-mutation-kill.txt`
- Design reference: ADR 0095 §2 (INV-IO-*), §4.3–§4.5, §4.7, §5.2, §5.2.1, §5.3
- Date: 2026-09-27

## Verdict

**NOT READY — rework required before PRH-I1 payout dispatch is marked `IMPLEMENTED`.**

The T1p claim itself is sound. It commits `approved→submitted` plus the `submitting` attempt
before any `Withdraw` call, the KYC deny is mutually exclusive with the claim under the L1 lock,
and no DB transaction is held across the provider call. The in-transaction `Withdraw` hazard
(F-POOL-2) is removed from the staff submit handler.

The recovery half (the sweeper plus phase C) has one confirmed double-payout defect (B1). It
also has several convergence defects. Because of these, a payout whose first send is not
immediately definite is either re-sent without bound or never converges. The tests do not
exercise any of these paths, and the mutation evidence covers only the branches that were
tested.

B1 and B3 are financial-correctness findings. They must go to `ledger-finance` for its domain
ruling. B1 also needs `security` sign-off, because it re-opens the double-payout class that
§10 C1 of the security review was raised to close.

All findings below were confirmed with probe tests. The probes ran in a detached worktree under
the session scratchpad, on per-test private scratch databases created and dropped by
`scratchdb`. The worktree has been removed and no probe code was committed.

---

## Confirmed correctness bugs (most severe first)

### B1 — CRITICAL: T12 re-sends a payout to a non-idempotent provider, with no bound (double/multiple payout)

`payout_sweep.go` `processPayoutAttempt` sends every `ambiguous` payout attempt straight to
`resubmitPayoutAmbiguous`. That function calls `Withdraw` again. ADR 0095 T12 (§4.3) allows
this **only if** the manifest has `IdempotentSubmission=true`, `submit_count < max_resubmits`
and `NOT legacy_backfill`. The code checks only `legacy_backfill`, in the `ResubmitAmbiguous`
CAS. `OperationManifest.IdempotentSubmission` exists (`contract.go:173`) but is never read on
this path. `max_resubmits` does not exist anywhere. The doc comment on `ResubmitAmbiguous` says
the manifest check is "the caller's job", and the caller does not do it.

Other problems on this path:
- `ambiguous` attempts are never polled. There is no `QueryStatus` before a resend, although
  §4.4 says ambiguous plus a poll is the default.
- Phase C's ambiguous branch (`MarkAmbiguousFromSubmitting`) drops any `ProviderReference`
  the adapter returned alongside `Ambiguous`, so a later `QueryStatus` is impossible anyway.
- `WithdrawRequest` carries no idempotency key. `payoutAdapterCall` ignores `cc`, so even a
  well-behaved provider gets only `MerchantReference` to dedupe on.

Failure scenario (probe `TestProbe_T12_ResendsNonIdempotentPayout`):
1. Default mock manifest, `IdempotentSubmission=false`. Withdrawal amount 333, which the mock
   answers with `Ambiguous` plus a reference.
2. One T1p dispatch, then 4 sweeper ticks.
3. Result: **5 `Withdraw` calls, `submit_count=5`**. Each call creates a distinct payout
   record at the mock. The attempt is still `ambiguous`. The resends have no bound and cause
   no escalation. Every lease period, another payout instruction is sent for the same hold.

The submit handler makes this reachable in production. It passes `r.Context()` into phase B,
so a staff browser disconnecting mid-call gives `ErrorClassAmbiguous` → T6 → this loop.
(`NewSweeper` does not wire `PayoutKYCGate` yet and no binary builds a sweeper, so this is
latent today. It is still merge-blocking, because this sweeper is the designed recovery path.)

Required: gate T12 on `IdempotentSubmission` and a `max_resubmits` bound. Otherwise poll with
`QueryStatus` and escalate (T16), as §4.5 requires. Persist the reference returned with an
ambiguous result. Add a test with a non-idempotent manifest that asserts exactly one
`Withdraw`.

### B2 — HIGH: a T1p attempt has `next_action_at = NULL`, so a crash, a phase-C failure or an unregistered provider after the claim leaves the payout invisible forever

`InsertSubmittingAttempt` never sets `next_action_at`. `claimBatch` selects only
`next_action_at IS NOT NULL AND next_action_at <= now()`. Anything that stops phase C from
committing after T1p leaves an attempt that no sweeper will ever look at, while the payout may
already have been executed:
- a process crash or deploy during phase B;
- a phase-C DB error (see B5 and M1);
- the handler's own `Provider(...)` not-registered branch.

This breaks ADR §3.1 ("If phase C never runs … the committed phase-A state plus the sweeper
converge the attempt") and T6 ("lease expired and QueryStatus not definitive").

The handler tells staff "check /resolve". But `/resolve` refuses any `submitted` row with a
NULL `provider_reference`: it returns `"structurally unreachable"`, a comment that P95-C2 has
made false. So there is no path forward at all.

Probe `TestProbe_T1p_NoNextActionAt`: after T1p with an expired lease, `next_action_at IS
NULL = true` and the sweeper claimed 0 rows.

Required:
- T1p (and T2/T12) set `next_action_at = lease_until`.
- A lease-expired `submitting` attempt with no reference is resolved by merchant-reference
  `QueryStatus` or moved to `ambiguous` (T6), not rescheduled forever (today
  `resolvePayoutViaQueryStatus` calls `RescheduleNonTerminal` whenever `ProviderReference ==
  nil`).
- Add the missing escalation (T16 on `SettlementWindow`) for payouts.

### B3 — HIGH: the sweeper completes a payout on `QueryStatus` success without checking amount or asset (INV-IO-6)

The doc comment on `resolvePayoutViaQueryStatus` says the function does "an amount/asset
cross-check against the withdrawal request identical to internal/httpserver's own resolve
handler". **No such check exists.** `StatusResult.Amount/AssetCode` are read nowhere on this
path. The resolve handler does check them (`ErrCallbackProviderMismatch`).

Probe `TestProbe_QueryStatusAmountMismatch_Completes`: `QueryStatus` reports `Succeeded`, amount
×100, asset `USD`. The withdrawal went to **`completed`** and Flow 3 Step B was posted. It
should have gone to T10 `disputed`.

Required: a mismatch → `ApplyDisputeFromNonTerminal` (T10). Add a test for it. Correct the
false comment.

### B4 — MEDIUM: the `pending` (and QueryStatus-inconclusive) poll path errors on every tick instead of rescheduling

`ApplyPayoutResult` is reused as-is for `QueryStatus` evidence, but its branches are written for
`submitting` only:
- `Pending` → `MarkAccepted` (CAS `state IN ('submitting','ambiguous')`);
- `Ambiguous` or a QueryStatus transport error → `MarkAmbiguousFromSubmitting` (CAS
  `state='submitting'`);
- `NotSent` (a credential or gate refusal on the status read) → `MarkNotSent` (CAS
  `submitting` and `claim_token`).

On a `pending` attempt, every inconclusive poll therefore returns `ErrAttemptStateConflict`
and rolls back. §4.4 and §5.3 require a no-op reschedule. The attempt is re-leased every
`Lease` with no backoff, `poll_count` never grows, and the escalation clock never starts.

Probe `TestProbe_PendingPoll_Errors`: `T4/T9 ->pending: payment_attempts CAS transition
conflict`, `poll_count=0`.

Relatedly, a `NotSent` result on a T12 resend hits `MarkNotSent … AND NOT ever_possibly_sent`
and conflicts. ADR T6 requires `resend_not_sent → ambiguous`. The attempt is then stranded in
`submitting` (see B2).

Required: the QueryStatus path must map evidence by current state as in the §4.4 matrix
(pending+pending/ambiguous/not-sent → `RescheduleNonTerminal`). Phase C must route `NotSent`
with `ever_possibly_sent` to T6.

### B5 — MEDIUM: the providerref validation on the `Withdraw` path is untested (surviving mutant), and the reason code never reaches audit

1. **Surviving mutant.** I disabled the `providerref.Validate` call in `payoutAdapterCall`
   (`if false && res.ProviderReference != ""`) and ran all 14 payout tests. **They all
   pass.** `TestPayoutDispatch_OversizeProviderReference_Parks` builds the `GateResult` by
   hand and never goes through `payoutAdapterCall` or `callProvider`. Its own comment says an
   adapter returning an oversize reference is "never actually constructible". That is not
   true: a three-line wrapper type does it, and the probe used one. With the mutant, a real
   oversize success reference reaches phase C. `AttachProviderReference` then fails the 0099
   CHECK (`withdrawal_requests_provider_reference_ref_bound`), phase C rolls back, and the
   attempt is stranded in `submitting` with NULL `next_action_at` (B2). This is exactly the
   §10 C1 scenario, and it is unguarded by tests.
2. **Reason code lost.** `callProvider` step 8 re-wraps every adapter error with `%s`
   (`"provider call error: %s"`), which breaks the `errors.As` chain. As a result,
   `providerref.AsError(gr.Err)` in `ApplyPayoutResult` **never matches in production**, and
   the audit/terminal reason is always the bare `invalid_provider_reference`.
   `invalid_provider_reference:<reason>` is only produced by the hand-built test input. Probe
   `TestProbe_InvalidRefReasonLost`: `AsError-ok=false`, audit `reason="invalid_provider_reference"`.
   (No raw value leaks: `providerref.Error.Error()` is length/hash only. The problem is
   dead code plus a misleading test.)

Required: drive the oversize case through `DispatchWithdraw` with a wrapper provider. Carry
the providerref reason as a typed field on `GateResult` (or wrap with `%w` for that typed,
already-redacted error only).

### B6 — MEDIUM: the staff `withdrawal.submit.http` audit is best-effort and outside the T1p transaction

The staff-attribution audit ("the ONLY record … that attributes the actual payout-triggering
action to a human", Stage 3B P2-4) used to be written in the same tx as the transition. Now it
is written in a **separate transaction after T1p commits**, and its error is discarded
(`_ = deps.DB.WithTenant(...)`). If that tx fails (DB blip, context cancelled by a client
disconnect, since it uses `r.Context()`), the payout proceeds to `Withdraw` with **no record
of which staff member triggered it**. ADR §4.3 T1p lists `withdrawal.submit.http` as an audit
action of the T1p tx itself. This also breaks the CLAUDE.md rule "every mutating
administrative/financial action writes an audit record".

Required: pass the staff actor, IP, UA and request ID into `ClaimForDispatch` and record the
audit inside the T1p tx (and inside the W-KYC deny tx). A smaller point: when KYC denied the
submit, the entry is written with `Outcome: success`.

### B7 — MEDIUM: `/resolve` and the sweeper act on the same payout without coordinating through the attempt

`newResolveWithdrawalHandler` still calls `Complete`/`Fail` on the withdrawal directly. It
never transitions the `payment_attempts` row, and it still calls `QueryStatus` **inside**
`WithTenant` (an INV-IO-1 breach). This commit touched that handler without fixing either
problem. ADR §5.2 says: "read (no lock) → QueryStatus outside the tx → phase C".

Failure scenario:
1. Staff resolves a `pending` payout to `completed`.
2. The attempt stays `pending`.
3. Every sweeper tick after that: `QueryStatus` succeeds → `AttachProviderReference` returns
   `ErrStateConflict` (withdrawal is no longer `submitted`) → rollback → error. This
   repeats forever.

The attempt and the withdrawal disagree permanently (the attempt is `pending`, the withdrawal
`completed`).

Required: route `/resolve` through the same phase-C evidence application, or at minimum
transition the attempt in the same tx.

### B8 — LOW: other correctness notes

- **Repeated KYC deny on an escalated attempt.** `Escalate`'s CAS is `escalated_at IS NULL`.
  A second KYC deny on an already-escalated T2/T12 attempt therefore returns a conflict and
  rolls back, so the attempt is re-gated with an error every lease period. ADR T2 wants the
  "escalated cadence" instead.
- **No attempt-transition audit.** No `payment.attempt_*` audit record is written for any
  attempt transition (T2/T4/T5/T6/T12). Most notably, the T12 re-send, which moves money, is
  unaudited. This gap is shared with the deposit path, so it is pre-existing, but INV-IO-4
  requires the record in the same tx.
- **Vendor decline text persisted.** A provider-supplied `DeclineReason` (vendor free text,
  unvalidated, unbounded) is written into `payment_attempts.decline_reason` and into
  `withdrawal.failed`'s audit `reason_code`. S95-C10 asks for canonical codes. This is carried
  over from the old handler, not newly introduced.
- **Kill switch and capability re-read missing.** T1p/T2/T12 have no kill-switch predicate
  (INV-IO-15) and no in-tx capability re-read (§5.2 step 4). The kill switch is not
  implemented anywhere yet (a later PRH-I1 step), so this is informational. The claim
  statements will need the `NOT EXISTS` predicate when it lands. The docs should not describe
  T1p as fully ADR-conformant until then.

---

## Claimed test coverage vs what it exercises

| Claim | Reality |
|---|---|
| `TestConcurrent_RejectVsClaimForDispatch_ExactlyOneWins` (50 reps, "exactly one wins") | `Reject` is illegal from `approved` whatever the ordering. It would pass run sequentially, so it is not a race test. The race that actually matters for F-POOL-2 is a staff double-submit (two concurrent `ClaimForDispatch`), and it is **not tested**. Neither is `Cancel`-vs-claim. |
| `TestClaimForDispatch_NeverCallsProviderBeforeCommit` | Passes by construction: `ClaimForDispatch` has no code path to `Withdraw`. It cannot fail, so it proves nothing about the handler's ordering. |
| `TestPayoutDispatch_OversizeProviderReference_Parks` proves §10 C1 | Covers only the phase-C park branch with a hand-built input. The adapter-call validation is a surviving mutant (B5). |
| T12 path covered | Only the KYC-deny branch. The allow/resend branch (where B1 lives) has no test. |
| QueryStatus resolution (`resolvePayoutViaQueryStatus`) | **Zero tests.** B3 and B4 both sit here, so any mutation in this function survives. |
| INV-IO-13 balanced/projection after every scenario | Present and correct in all 14 tests. |

## Mutation evidence credibility

The recorded mutants PM-PAYOUT-1 to PM-PAYOUT-7 are plausible:
- the cited failure lines match the test file at `722a860` (203/352/383/523) and at HEAD
  (769/993);
- the mechanics described are consistent with the code;
- the negative result in PM-PAYOUT-6 is disclosed honestly.

I re-ran the suite at HEAD and all 14 pass. The evidence is **credible but selective**. Every
mutant targets a branch that already had a test. None targets the untested paths (the
adapter-call validation, the T12 manifest gate, the QueryStatus mapping, the amount/asset
check). One reviewer mutant (B5.1) survived the full suite. PM-PAYOUT-5 corrupts the
projection from the test itself, not from production code. That shows the assertion can fail,
but it is not a mutation of the implementation.

## Simplification

- `lockSubmittedRequest` is a one-line pass-through to
  `withdrawal.LockSubmittedForResolution`, and its comment contradicts itself. Call the
  function directly.
- `ApplyPayoutResult` is one function serving two evidence sources (sync `Withdraw` from
  `submitting`, and `QueryStatus` from `pending`/`submitting`), with branches written for only
  one of them. That design is the root cause of B4. Replace it with a single state-aware
  evidence function as §4.4 prescribes ("one function, rows are the current state"). That is
  simpler and fixes B4 and part of B7.

## Required before re-review

1. B1 fixed, with a non-idempotent-manifest test asserting exactly one `Withdraw` (plus a
   `max_resubmits` bound test).
2. B2: `next_action_at` set at T1p/T2/T12, plus lease-expiry convergence (T6 or merchant-ref
   QueryStatus), with a crash-after-phase-B test.
3. B3: amount/asset mismatch → T10, with a test.
4. B4: state-aware QueryStatus mapping, with a pending-still-pending test.
5. B5: an end-to-end oversize-reference test through `DispatchWithdraw`, and a working reason
   code.
6. B6: staff audit inside the T1p tx.
7. B7: decided, or explicitly deferred with a recorded decision.
8. `ledger-finance` ruling on B1/B3, and `security` sign-off on B1/B5/B6.

---

# Re-review — fix round (HEAD `9324189`)

- Commits reviewed: `c8a2b76` (fixes), `0154459` (kill-switch wiring), `9be59bb` (tests + evidence),
  `3be159d` (ADR 0095 §27.11), `73e0a77` (B5 end-to-end test), `f99d940` (post-merge build/lint)
- Date: 2026-09-27
- Method:
  - detached worktree at `9324189` under the session scratchpad;
  - `go build ./...`;
  - `go vet -tags=integration` on payments/httpserver/withdrawal;
  - pinned `golangci-lint` 2.9.0, untagged and `--build-tags=integration`: **0 issues** both ways;
  - full `internal/payments`, `internal/withdrawal` and `internal/httpserver` integration suites on
    a private database (created via `TEST_ADMIN_DATABASE_URL`, migrated to 0105, grants from
    `deploy/init-app-role.sql`), plus the harness's own per-test scratch databases. **All
    pass.** All 29 payout tests pass.
  - my original probes re-run;
  - two new probes;
  - 12 anchored mutants, each reverted with `git checkout`.
- The private database and the worktree have been dropped/removed. No probe code was committed.

## Re-review verdict

**NOT READY — narrow rework only.**

The critical finding is fixed: B1 re-probed gives 1 `Withdraw` call, not 5. B2–B7 are fixed in
code, and my original probes now all pass. One new correctness defect (N1) remains, and it sits
in the very mechanism the B1 fix relies on. It makes the "poll before resend" design, as recorded
in ADR §27.11 item 1, unreachable for the most common ambiguous case.

Test coverage has improved, but several fix branches are still unpinned (9 of 12 mutants
survive). Among them is the B6 audit, which was a named review finding. The verification counts
in the evidence file and the ADR are overstated. None of this is a double-payout risk any more.
The remaining work is small.

## Status of the original findings

| # | Status | Evidence |
|---|---|---|
| B1 | **Fixed.** T12 is gated on `IdempotentSubmission`, with a `max_resubmits` bound in Go and in the CAS. Phase B runs on a context detached from the request. | Probe `TestProbe_T12_ResendsNonIdempotentPayout`: **1** `Withdraw`, `submit_count=1`, escalated (was 5). Killed by `TestSweeper_T12_NonIdempotentManifest_NeverResends` (PM-PAYOUT-10). But see N1. |
| B2 | **Fixed.** `next_action_at = lease_until` at T1p, T2 and T12. A lease-expired `submitting` attempt with no reference goes to T6. | `TestClaimForDispatch_NextActionAtSet_CrashRecovery`. My probe shows `next_action_at` non-NULL. |
| B3 | **Fixed.** `applyPayoutSuccessCheckedFromStatus` routes a mismatch to T10 and audits it. | Probe: amount ×100 in USD now leaves the withdrawal `submitted` (was `completed`). Mutant M6 is **killed** by both mismatch tests. |
| B4 | **Fixed.** `applyPayoutStatusEvidence` is state-aware, and a NotSent result on a resend goes to T6. | Probe: still-pending poll gives no error and `poll_count=1`. PM-PAYOUT-9 is killed. The NotSent-on-resend branch is **untested** (M8 survives). |
| B5 | **Fixed.** `callProvider` keeps the `*providerref.Error` chain via `%w`. That error's `Error()` output is already redacted, so nothing leaks. | Probe: `AsError-ok=true`, audit reason `invalid_provider_reference:too_long`. My original surviving mutant (M1) and a gate-chain revert (M2) are **both killed** by `TestDispatchWithdraw_OversizeReference_ParksThroughRealAdapterPath`. |
| B6 | **Fixed in code, untested.** The staff audit is written inside the T1p and deny transactions, with the correct outcome. | Mutant M5 (the allow-path `withdrawal.submit.http` audit made a no-op) **survives** the full payments suite and the full httpserver suite. No test anywhere asserts this audit row. |
| B7 | **Fixed.** `/resolve` reads without a lock, then calls `PollPayoutStatus` (QueryStatus outside any transaction, attempt and withdrawal transitioned together). | httpserver suite passes on the private DB, including `TestWithdrawalResolve_ProviderAmountMismatchRejected`. See N2 and N3 for new side effects. |
| B8 | **Mostly fixed.** Repeated KYC deny is idempotent. Decline reasons go through an allow-list. The kill switch is wired into the T1p/T2/T12 claim statements. Attempt-transition audit remains disclosed as open (§27.11). | Both new branches are **untested**: M11 (raw vendor reason passed through) and M12 (repeated-deny idempotence removed) survive. |

## Checks the coordinator asked for

1. **Vacuous Reject-vs-claim race replaced: partly.** A real claim-vs-claim race was added
   (`TestConcurrentClaimForDispatch_ExactlyOneWithdraws`: 5 concurrent claims × 50 reps, exactly one
   attempt row and exactly one `Withdraw`). That is good. But
   `TestConcurrent_RejectVsClaimForDispatch_ExactlyOneWins` is still in the file, unchanged, and
   still vacuous. Its name still claims a race it cannot lose. Delete it or rename it.
2. **"Never calls provider before commit" made falsifiable: not done.**
   `TestClaimForDispatch_NeverCallsProviderBeforeCommit` is byte-identical apart from the new
   `actor` argument. It still cannot fail, because `ClaimForDispatch` has no way to reach
   `Withdraw`. The new `TestClaimForDispatch_GateRunsUnderOuterLock` is a real test, but of a
   different property (the gate waits for the L1 lock).
3. **T12 resend branch and `PollPayoutStatus` have tests: partly.**
   - `PollPayoutStatus` now has direct tests for amount mismatch, asset mismatch and still-pending.
   - T12 has a non-idempotent test (which pins "never resend") and an idempotent test. The
     idempotent test only asserts an **upper bound** (`Withdraw ≤ 3`, `submit_count ≤ 3`).
     Mutant M3 (`if true || !manifest.IdempotentSubmission`, so T12 *never* resends) **survives**.
     The positive resend is therefore not pinned.
   - Mutant M4, which deletes the poll-before-resend block entirely, also **survives**. That
     block is unreachable in every test (see N1).
   - The idempotent test's comment says "1 + MaxResubmits(=2) = 3 total". With `SubmitCount >=
     maxResubmits` counting the T1p send, the real total is 2. The loose `≤ 3` bound hides this.
4. **My extra mutants now die: yes for the B5 pair (M1, M2).** New mutants against the fix
   branches:

| Mutant | Result |
|---|---|
| M1 `payoutAdapterCall` validation disabled | killed |
| M2 `callProvider` providerref `%w` special case removed | killed |
| M3 T12 never resends (always escalate) | **survived** |
| M4 poll-before-resend block removed | **survived** |
| M5 B6 allow-path staff audit made a no-op | **survived** (payments and httpserver suites) |
| M6 amount/asset cross-check removed | killed |
| M7 no-reference `submitting` → reschedule instead of T6 | **survived** (the crash-recovery test only asserts `Claimed>0` and no errors, not the resulting state) |
| M8 NotSent-on-resend → T6 removed | **survived** |
| M9 `payoutHandleContradiction` late-evidence routing removed | **survived** |
| M10 T14 late-success-after-decline routing removed | **survived** |
| M11 `canonicalDeclineReason` passes raw vendor text | **survived** |
| M12 repeated-KYC-deny idempotence removed | **survived** |

## New findings

### N1 — HIGH: an ambiguous payout's provider reference is stored only on the withdrawal, so the attempt is never polled and "poll before resend" is dead code

`ApplyPayoutResult`'s ambiguous branch now "persists" the reference returned with an Ambiguous
result, but only via `withdrawal.AttachProviderReference`. `MarkAmbiguousFromSubmitting` does not
set `payment_attempts.provider_reference`. Two places decide whether to poll from
`attempt.ProviderReference`:
- `resubmitPayoutAmbiguous` (`if attempt.ProviderID != nil && attempt.ProviderReference != nil`);
- `PollPayoutStatus` (`attempt.ProviderReference == nil` → reschedule).

So an ambiguous attempt that came from a sync Ambiguous result is **never polled**, even though
the platform holds the reference. The only way an attempt reaches `ambiguous` with a reference is
pending→ambiguous (T11).

Failure scenario, probe `TestProbe_N1_AmbiguousRefNeverPolled`:
1. Mock `Withdraw` returns Ambiguous with a reference. The manifest is non-idempotent (the
   default).
2. The provider later settles the payout (outcome set to `Succeeded`).
3. After 4 sweeper ticks: the withdrawal has the reference, the attempt has
   `provider_reference=NULL`, and **`QueryStatus` calls = 0**. The attempt is `ambiguous` and
   escalated, and the withdrawal is still `submitted`.

`/resolve` cannot help either: it goes through the same `PollPayoutStatus`, which just
reschedules. The hold therefore stays until M2, which is BLOCKED (HD-0095-1), even though one
status query would settle the payout. With an idempotent provider, the same path resends without
polling first.

This makes two recorded claims false:
- ADR §27.11 item 1: "poll first, unconditionally, if a reference exists at all";
- the "C1/B1 … reference … is PERSISTED" comment in `ApplyPayoutResult`.

Required:
- Record the reference on the attempt at T6 (`provider_reference = COALESCE(provider_reference,
  $ref)` in `MarkAmbiguousFromSubmitting`, if the 0101 guard allows NULL→value there), **or**
  have `PollPayoutStatus` fall back to `withdrawal_requests.provider_reference` (safe: INV-IO-8
  allows exactly one payout attempt per withdrawal).
- Add a test where an ambiguous-with-reference payout converges by poll with zero resends. That
  test would also kill M4.
- Correct §27.11.

### N2 — LOW: `/resolve` forces T6 on an attempt that is still in flight

`PollPayoutStatus` does not check the lease. Staff pressing `/resolve` while the submit
handler's phase B is still running (up to the 60 s outbound bound) moves the fresh `submitting`
attempt straight to `ambiguous`, with `ever_possibly_sent=true`.

Probe `TestProbe_N2_…`: state becomes `ambiguous`. The original phase C still converges
(`MarkAccepted` from `ambiguous` → `pending`), so there is no financial effect. But:
- the attempt is permanently marked possibly-sent;
- on an idempotent manifest, a T12 resend can race the in-flight original (same key).

Suggested fix: when there is no reference and the lease has not expired, `PollPayoutStatus` (or
`/resolve`) should return "in flight" and change nothing.

### N3 — LOW: `/resolve`'s staff audit now runs after the effect commits

The effect is committed inside `PollPayoutStatus` (`withdrawal.completed`/`failed` as system).
`withdrawal.resolve_attempted.http` is then written in a separate transaction afterwards. If
that transaction fails, the payout was resolved with no staff attribution. This is the same
class of defect as B6, now on `/resolve`, where the audit used to be in the same transaction.
The outcome is dictated by provider evidence, not staff discretion, hence LOW. Pass the actor
into `PollPayoutStatus`, as was done for `ClaimForDispatch`.

### N4 — LOW: mixed lock order after the M3 reversal

`applyPayoutSuccess`/`applyPayoutDecline` now lock the attempt before the withdrawal. But:
- `ApplyPayoutResult`'s ambiguous branch and `applyPayoutStatusEvidence`'s submitting-ambiguous
  branch call `AttachProviderReference` (which locks the withdrawal) **before** the attempt CAS;
- T2/T12 lock the withdrawal first.

ADR §4.3/§14 still say "parent before attempt". Opposite orders on the same pair of rows can
deadlock, for example T12 against a late phase C after N2. Postgres aborts one side and the
sweeper retries, so this heals itself, but the order should be made consistent. The lock-order
ruling itself belongs to `ledger-finance`. I am flagging it to them, not adjudicating it.

### N5 — LOW: the verification claims are overstated

- `9be59bb` and the evidence file say the fix-round file has "20 tests". It has **15**.
- ADR §27.11 claims "44 payout-specific tests". There are 29 (14 + 15), or 32 counting the 3
  deposit-sweeper regressions.
- The evidence file says H2/B3 and H3/B2 were "not independently re-mutated". That is fair for
  H2/B3, whose mutant I confirmed is killed. For H3/B2, the crash-recovery test does not pin the
  T6 outcome (M7 survives).

Correct the numbers. Under the no-fake-completion rule, a verification summary must be exact.

### N6 — observation: evidence reference mismatch is still not disputed

`applyPayoutSuccess` settles with the *echoed* `QueryStatus` reference when it differs from the
attempt's stored reference. `AttachProviderReference` is a set-once no-op, so the ledger
`provider_tx_id` then differs from both reference columns. ADR §4.4 routes a reference mismatch to
T10. This was not in my original list. Route it to `ledger-finance` together with N4.

## Environment notes

- My first, broad `-run` pattern accidentally matched 7 non-payout `internal/payments` tests that
  use the **shared** `TEST_DATABASE_URL`. That database has no migration 0101, so they failed at
  once (`relation "payment_attempts" does not exist`). All 7 pass on the private DB. This is an
  environment gap already disclosed in the fix-round evidence, not a product defect.
- In one full httpserver run, three `TestResolutionIsolation_*` tests failed. They passed on the
  baseline run and twice when run on their own. They are timing-sensitive under load, so I treat
  them as a flake.
- 22 `m0101v2_*` harness scratch databases were present afterwards (13 before this session's
  re-review). Other agents are active concurrently and I cannot attribute them, so I did not drop
  them.

## Required before sign-off

1. Fix N1 and add a test: an ambiguous payout with a reference converges by poll, with zero
   resends.
2. Assertions that kill M3 (a positive T12 resend on an idempotent manifest, with an exact
   count), M5 (the `withdrawal.submit.http` staff audit row: actor, outcome, same transaction) and
   M7 (the crash-recovery test asserts `ambiguous`). M8–M12 should also be pinned; they are
   cheap.
3. Delete or rename the vacuous Reject-vs-claim test. Either make
   `NeverCallsProviderBeforeCommit` falsifiable (drive the handler with a spy that records whether
   the T1p row was committed when `Withdraw` was called) or delete it.
4. Correct the test counts in the evidence file and in §27.11.
5. N2/N3 can be follow-ups if recorded. N4/N6 go to `ledger-finance`.

---

# Re-review 2 — payout round 3 (`efc63f5`, merged at `07094a3`)

- Method:
  - detached worktree at `07094a3`, build, `go vet -tags=integration`;
  - pinned `golangci-lint` 2.9.0, untagged and `--build-tags=integration`: **0 issues**;
  - full `internal/payments` and `internal/withdrawal` suites **pass** on a private database
    (created via `TEST_ADMIN_DATABASE_URL`, migrated to 0106, grants from
    `deploy/init-app-role.sql`);
  - `internal/httpserver`: everything passes except the 3 `TestResolutionIsolation_*` INV-POOL
    timing tests. These also fail on the **unchanged `9324189` baseline**, run in a second
    worktree on its own private DB (machine load average about 6–7), so they are environmental
    and not caused by this round;
  - probes from `probe2_copy.go.txt` re-run, plus one new probe;
  - 16 anchored mutants, each reverted with `git checkout`, tree confirmed clean.
- Both worktrees and both private DBs are removed. No probe code was committed.

## Verdict

**NOT READY — one regression (N7), plus N3 still open.**

Every mutant from my list now dies, N1 is fixed, and the test-quality items are closed. But the
R2 in-flight guard added this round breaks crash recovery under the sweeper's production lease.
That re-opens B2. All 41 payout tests use `Sweeper{…}` with `Lease: 0`, which hides this.

## Claimed items

| Claim | Result |
|---|---|
| N1: reference on the attempt, converges by poll with zero resends | **Fixed.** `payoutMarkAmbiguousFromSubmitting` sets the reference with COALESCE, and `PollPayoutStatus` falls back to the withdrawal's reference. My N1 probe: 1 `QueryStatus`, attempt `succeeded`, withdrawal `completed`, 1 `Withdraw`. `TestSweeper_N1_…` kills M4. |
| Mutant-pinning tests | **Done.** All of M1–M12 are **killed**. So are the new-code mutants R_N6 (reference-mismatch dispute), R_R2 (in-flight refusal) and R_N1_ref. R_N1_ref is killed partly because the SQL breaks, and the N1 test's attempt-reference assertion would catch it anyway. One minor survivor: R1's `isPayoutAttemptTerminal` early return. Stray non-definite evidence on a terminal attempt then falls through to `RescheduleNonTerminal`, which conflicts and returns an error instead of `nil`. Not a money issue; worth one test. |
| Vacuous Reject-vs-claim test deleted | **Done.** |
| "Never calls provider before commit" made falsifiable | **Done, with a narrow scope.** `commitVisibilitySpyProvider` reads the attempt from a separate connection when `Withdraw` runs, so a claim that returned without committing would fail. The test itself still sequences claim → dispatch, so it does not cover a handler that reorders them. Acceptable. |
| Counts corrected to 41 | **Correct** (13 + 15 + 13). |
| N2: lease refusal | **Fixed** for `/resolve`: `ErrPayoutDispatchInFlight` maps to 409, and the CAS predicate respects the lease. My N2 probe now gets the refusal. **But see N7.** |
| N3: `/resolve` audit in the same tx | **Not addressed.** Neither `efc63f5` nor its commit message mentions N3. At `07094a3`, `withdrawal_handlers.go` still calls `PollPayoutStatus` (which commits `Complete`/`Fail`), then writes `withdrawal.resolve_attempted.http` in a **separate** `WithTenant` afterwards (around lines 1178 and 1218). The claim is not supported by the code. |
| N6: echoed-reference mismatch disputes | **Fixed** (T10 plus audit; R_N6 killed). One small gap: the check compares against `attempt.ProviderReference` only. When the poll used the withdrawal-reference fallback (attempt reference NULL), an echo that conflicts with the withdrawal's reference is not compared. Low. |
| R3 lock order | Withdrawal `FOR UPDATE` is now first in both phase-C transactions and in `applyPayoutSuccess`/`applyPayoutDecline`. That is consistent with T2/T12, which closes N4. `ledger-finance` owns that ruling. |

## N7 — HIGH (regression of B2): the R2 guard refuses the sweeper's own crash recovery

`claimBatch` leases every due row with `lease_until = now() + s.Lease`. `processAttempt` then
re-reads the attempt, so `PollPayoutStatus` receives a `submitting` attempt whose `LeaseUntil`
is the sweeper's **fresh** lease, in the future. The new guard (`attempt.State ==
AttemptSubmitting && attempt.LeaseUntil.After(time.Now())`) then returns
`ErrPayoutDispatchInFlight`. The doc comment says "the sweeper never hits this", which is wrong.
Each tick re-leases the row, so a crashed dispatch never converges. It stays `submitting` and the
sweeper logs an error every tick.

Probe `TestProbe_N7_CrashRecoveryWithDefaultLease` (the `NewSweeper` defaults, `Lease=1m`, plus
`PayoutKYCGate`, on a lease-expired `submitting` attempt with no reference):
`claimed=1 processed=0 errs=[… payout dispatch is still in flight, its lease has not expired]
state=submitting`.

`TestClaimForDispatch_NextActionAtSet_CrashRecovery` passes only because it builds
`&Sweeper{…}` with `Lease` zero, so the sweeper's lease is `now()`. No payout test uses a
non-zero lease.

Required:
- Make the in-flight test distinguish the dispatch claimant's lease from the sweeper's. For
  example, check `lease_owner` (T1p `payout-dispatch` / `sweeper-payout-*` against the batch
  lease owner `sweeper`), or have `/resolve` alone apply the check, or have `claimBatch` not
  overwrite a live claimant lease.
- Change the crash-recovery test (and ideally every sweeper-driven payout test) to use
  `NewSweeper` defaults or a non-zero `Lease`.

## Required before sign-off

1. Fix N7, with a crash-recovery test that runs under a non-zero sweeper lease.
2. Fix N3 (the staff actor recorded in `PollPayoutStatus`'s own transaction), or record it as an
   explicitly deferred decision. Either way, correct the claim that it was addressed.

Low items that can be follow-ups:
- the R1 terminal early-return test;
- the N6 comparison against the effective (fallback) reference.

## Environment notes

- The harness logged `scratch database … left behind (drop failed: permission denied to
  terminate process)` when a drop raced another session. This explains the leftover
  `m0101v2_*` databases seen in earlier rounds. It is a harness/role limitation, not this
  change's fault.
- My own `rvprhi1_*` databases: 0 remain.

---

# Re-review 3 — payout round 4 (`bfb075e`, merged at `4977d26`)

- Method:
  - detached worktree at `4977d26`, build, `go vet -tags=integration`;
  - pinned `golangci-lint` 2.9.0: untagged, 0 issues on payments/httpserver/withdrawal;
    `--build-tags=integration`, 0 issues on `internal/payments`. The integration-tag run on
    `internal/httpserver` reports pre-existing `errcheck` issues (`resp.Body.Close`) in older test
    files this round did not touch;
  - full `internal/payments` and `internal/withdrawal` suites **pass** on a private database
    (via `TEST_ADMIN_DATABASE_URL`, migrated to 0106, grants from `deploy/init-app-role.sql`);
  - `internal/httpserver` passes except the 3 `TestResolutionIsolation_*` INV-POOL timing tests.
    Re-review 2 showed they also fail on the unchanged baseline, so they are environmental;
  - probes re-run;
  - 22 anchored mutants (my 12, the earlier new-code ones, and 7 new ones for this round), each
    reverted with `git checkout`, tree confirmed clean.
- The worktree and the private DB are removed. No probe code was committed.

## Verdict

**APPROVE — code review has no remaining blockers.**

This covers code review only. Under CLAUDE.md, the financial and security domain sign-offs
(`ledger-finance`, `security`) are still required and are not decided here.

## Verification of claimed fixes

| Claim | Result |
|---|---|
| **N7:** in-flight guard exempts only `lease_owner='sweeper'`, in the Go check and in the CAS; `NewSweeper` test | **Fixed.** My N7 probe (`NewSweeper` defaults, `Lease=1m`): `claimed=1 processed=1 errs=[] state=ambiguous` (was `processed=0`, error every tick). `TestSweeper_N7_CrashRecoveryWithRealLease_ActuallyRecovers` asserts `Processed==1` and `ambiguous`. The live-dispatch refusal still holds (`TestSweeper_N7_LiveDispatchLease_StillRefusesResolve`; my N2 probe still gets `ErrPayoutDispatchInFlight`). All sweeper-driven payout tests now set `Lease: SweeperDefaultLease` or use `NewSweeper`. |
| **N3:** `/resolve` staff audit in the same tx | **Fixed** (verified by reading the code). `PollPayoutStatus` takes `*SubmitActor`. `payoutResolveAudit` runs inside the same `WithTenant` closure as the state change, on the evidence path (`applyPayoutStatusEvidence` wrapper) and on the no-reference fallback. The handler's post-hoc `audit.Record` is removed; that closure is now read-only. Sweeper calls pass `nil`, which is a no-op. Minor: the refusal paths (409 in-flight, 409 `created`) write no staff audit, and the audit row no longer carries `withdrawal_state` metadata. Neither is a regression that matters. |
| **R1:** early-return test | **Pinned.** R_R1 is killed by `TestPayoutDispatch_R1_StrayEvidenceAgainstTerminalAttempt_IsNoOp`. |
| **N6:** comparison against the withdrawal-reference fallback | **Fixed and pinned.** N6_fallback is killed by `TestPollPayoutStatus_N6_MismatchAgainstFallbackWithdrawalReference_Disputes`. |

## Mutants: 22 of 22 killed

M1–M12 are all killed. So are R_N6, R_R2 and R_R1, and the new ones:

| Mutant | Result |
|---|---|
| N7_go_exempt: the Go-check `sweeper` exemption removed, which reverts to the round-3 bug | killed |
| N7_cas_exempt: `OR lease_owner='sweeper'` removed from the CAS | killed |
| N7_overbroad: exemption applied to every owner, so a live dispatch is never refused | killed |
| N3_audit_noop | killed |
| N3_fallback_branch_audit | killed |
| N6_fallback | killed |

## Can `lease_owner` be spoofed, and who else writes `'sweeper'`?

- **Not client-controllable.** Every `lease_owner` write takes a string literal from server code:
  - `InsertSubmittingAttempt` (`payout-dispatch`, `player-request`);
  - `ClaimCreatedForSubmission` / `ResubmitAmbiguous` (`sweeper-payout-reclaim`,
    `sweeper-payout-resubmit`, `player-request-cascade`, `sweeper`);
  - `claimBatch` (`'sweeper'`).

  No HTTP input, payload or provider evidence reaches that column. Forging it would need direct
  DB write access as the runtime role, which is outside this threat model and would bypass far
  more than this guard. Rows are tenant-scoped under RLS.
- **The only other `'sweeper'` writer is `drive.go`'s `driveCreatedAttempt`.** It uses
  `leaseOwner = "sweeper"` for a sweeper-driven **deposit** T2 claim, which is a live dispatch
  lease under the same literal. It is deposit-only (it takes a `DepositIntent`), and
  `PollPayoutStatus` only ever sees payout attempts, so the exemption cannot be triggered by it
  today.
- **Latent coupling, LOW.** The literal `"sweeper"` now means both "batch claim lease, never in
  flight" (claimBatch) and "live dispatch lease" (drive.go). If a future change routes a payout
  through a `sweeper`-owned T2 claim, the in-flight guard silently stops protecting it.
  Recommendation: give claimBatch a distinct owner (e.g. `sweeper-batch`) and key both the Go
  check and the CAS on that constant.
- **Owner NULL, LOW.** When `lease_owner` is NULL (legacy-backfill rows only), the Go check
  treats the attempt as not in flight, but the CAS still refuses while the lease is live. The
  result is an `ErrAttemptStateConflict` rather than the 409. This is fail-closed and
  legacy-only.
- **Residual, inherent to the lease design.** The sweeper only picks up a `submitting` row after
  `next_action_at = lease_until`, which is 2 min after T1p. The dispatch path is bounded by 60 s
  (outbound) plus 5 s (phase C). Only a submit handler stalled for more than about 55 s beyond
  those bounds could see its in-flight attempt moved to T6 underneath it. Phase C then converges
  (`MarkAccepted`/`ApplySuccess` both accept `ambiguous`), so the effect is at most a spurious
  `ever_possibly_sent`. Not a blocker.

## Open items (non-blocking)

1. The overloaded `"sweeper"` lease-owner literal (above). Recommended hardening.
2. The environment: the `TestResolutionIsolation_*` timing flakes under load, and pre-existing
   integration-tag `errcheck` noise in older httpserver tests. Neither comes from this change.
3. Domain sign-offs from `ledger-finance` and `security` (the N4/N6 lock and reference rulings are
   theirs) before PRH-I1 payout dispatch is labelled `IMPLEMENTED`.

---

# FH-6 code review — `4b544f5` (payout security round, A7 suite, production fixes)

- Scope: `ca696e1..4b544f5` on `worktree-agent-a5b19b46582e95b75`.
- Method:
  - detached worktree at `4b544f5`;
  - private database created via `TEST_ADMIN_DATABASE_URL`, migrated to 0106, grants from
    `deploy/init-app-role.sql`. DB access worked throughout. No role, password or privilege
    changes were made;
  - build, `go vet -tags=integration`, and pinned `golangci-lint` 2.9.0 (untagged, and with
    integration tags on payments): **0 issues**;
  - anchored mutants, each reverted with `git checkout`.
- The container restarted mid-review. My stale worktree held one un-reverted experiment edit
  (`sweeper.go`); it was reverted and the lost run re-run. The worktree and the private DB are
  now removed.

## Verdict

**APPROVE WITH CONDITIONS.**

The production changes are correct, and the claimed kills are real. Three conditions, none a
correctness bug:
- two S-M2 audit fields are not pinned by any test;
- the Escalate-predicate regression test catches its mutant only about 1 time in 40;
- the SP-C root-cause analysis in the launch-conditions note looks wrong, and a narrower fix
  appears to exist.

The domain rulings on S-L2/SP-C and the lock order belong to `ledger-finance` and `security`
(`fca1741` is ledger-finance's).

## Suites

- `internal/payments` and `internal/withdrawal` **pass**.
- `internal/httpserver` passes except 4 `TestResolutionIsolation_*` INV-POOL timing tests. The
  same family fails on unchanged baselines under this machine's load (see re-reviews 2 and 3), and
  `TEST-RESISO-RACE-1` is filed.
- The A7 suite is stable: `-count=5` all pass, and `TestA7_1b` passes 40 of 40 at baseline.

## Claimed kills, verified with my own mutants

| Mutant | Result |
|---|---|
| SM2a: T12 Go-level `checkPayoutKillSwitch` removed | **killed** (`TestResubmitPayoutAmbiguous_SM2a_…`) |
| SM7: `/resolve` linked-staff check removed | **killed** (`TestWithdrawalResolve_UnlinkedStaffAccountRejected`) |
| SM7b: `/resolve` active-staff check removed | **killed** (`…_SuspendedStaffAccountRejected`) |
| SM13a: only `LockApprovedForSubmission` accepts `pending_review` | **survives, harmlessly.** `MarkSubmittedPending`'s own check and CAS still refuse, so submit returns 409. This is defense in depth, not a gap. The new test's comment says this single mutant "was confirmed to SURVIVE the full suites", which is consistent. |
| SM13b: `LockApprovedForSubmission` **and** `MarkSubmittedPending` (Go check and CAS) accept `pending_review` | **killed** (`TestWithdrawalSubmit_PendingReviewBypassRejected`). This is the real four-eyes bypass, and it is now pinned. |
| S-M2: dispute outcome forced to `success` | killed |
| S-M2: `attempt_state_after` shows the before-state | killed |
| S-M2: `terminal_reason` dropped | killed |
| **S-M2: `withdrawal_state_after` hard-coded to `submitted`** | **SURVIVED** |
| **S-M2: `evidence_class` blanked on the QueryStatus path** | **SURVIVED** |
| S-L2: `claimBatch` writes the bare `"sweeper"` | killed (3 tests) |
| S-L2: Go in-flight check compares against `"sweeper"` | killed (N7 crash-recovery tests) |
| S-L2: CAS exemption compares against `"sweeper"` | killed (N7 crash-recovery tests) |
| `escalateAmbiguousPayout` conflict-swallow removed | killed (`TestA7_1b`) |
| **Escalate CAS `state NOT IN (terminal)` predicate removed** | **SURVIVED at `-count=10`; killed 1 of 40 at `-count=40`**, with `violates check constraint "payment_attempts_check9"`. Baseline 0 of 40. |

The cross-tenant submit and `/resolve` tests are not vacuous:
- each asserts a 404 for the foreign tenant's staff;
- each asserts no new audit row and no `payment_attempts` row;
- each asserts that the owning tenant's staff can still act afterwards.

## Is the Escalate predicate change safe? Could it mask a real state bug?

- **Correct and required.** `payment_attempts_check9` requires `next_action_at IS NULL` in every
  terminal state. Without the predicate, a Escalate that loses a race to a terminal transition
  (T12 polls, then escalates *outside* the withdrawal lock, while a success callback commits)
  would write `next_action_at` onto a terminal row and fail with a CHECK violation. My 1-in-40
  kill reproduced exactly that error.
- **`gateAndEscalateOnDeny`** (T2/T12 KYC deny, under the withdrawal lock) does not swallow the
  conflict. A terminal attempt there becomes a returned error and a rollback: loud, never silent.
- **`escalateAmbiguousPayout`** swallows *every* `ErrAttemptStateConflict` as `nil`. That covers
  the intended cases (the attempt went terminal concurrently, or `escalated_at` was set
  concurrently). It also covers the unintended one: 0 rows because the row was not visible at
  all, e.g. a wrong tenant in RLS, or the id is gone. That case would be skipped with no error,
  no audit and no escalation, and re-leased every tick. No such caller bug exists today, so this
  is **LOW**. Recommended hardening: on conflict, re-read and return `nil` only if the state is
  terminal or `escalated_at IS NOT NULL`, otherwise return the error.
- **Test strength, condition.** The only regression test for the predicate (`TestA7_1b`) hits the
  window about 2.5% of the time. For a production fix described as "required, not defense in
  depth", add a deterministic test: drive the attempt to `succeeded`, then call Escalate or
  `escalateAmbiguousPayout` with the stale ambiguous copy, and assert a conflict/`nil`, no CHECK
  error, and the attempt unchanged.

## S-M2 audit: correctness notes

- `withdrawal_state_before` is hard-coded to `submitted`, justified by the handler's precondition.
  But the handler reads that state without a lock, before `PollPayoutStatus`. A concurrent
  sweeper resolution in between would make the audit claim `submitted → completed` when the
  request was already `completed`. This is an audit-accuracy nit (LOW). Recording the state read
  under `LockForPayoutEvidence` inside the same transaction would make it exact.
- Two fields are unpinned: `withdrawal_state_after` and `evidence_class` on the QueryStatus path
  (see the surviving mutants above). Add assertions for a completing resolve
  (`withdrawal_state_after=completed`, `evidence_class=succeeded`).

## SP-C: the disclosed root cause appears wrong, and a narrow fix passes the suite

The launch-conditions addendum says excluding live non-batch leases in `claimBatch` "regress[ed]
7 existing tests" because per-item claims set `next_action_at` earlier than `lease_until`. In
fact, `ClaimCreatedForSubmission` and `ResubmitAmbiguous` both set `next_action_at =
lease_until`. The early-`next_action_at`-with-live-lease rows come from phase C moving the row
out of `submitting` (to `pending`/`ambiguous`) without clearing the lease.

Experiment: I added the security review's actual condition, **scoped to `state='submitting'`**,
to `claimBatch`:

```sql
AND NOT (state = 'submitting' AND lease_until > now() AND lease_owner IS DISTINCT FROM 'sweeper-batch')
```

The **full `internal/payments` suite passed** with it (one run, not `-race`). This matches SP-C's
shape: step 4 claims a `submitting` row under a live `sweeper-payout-resubmit` lease, which this
predicate excludes.

I did not write an SP-C reproduction here, so this is evidence, not a verified fix. Route it to
`security`/`ledger-finance` together with the addendum's correction. SP-C remains a disclosed,
not-fixed launch condition (money-safe today per the security review).

## A7 suite and the scoping fix

- `a7WaitAnyLockWaiter` is now scoped to `pg_stat_activity.datname = current_database()`, which
  is correct for a shared cluster. Each call site also confirms the found waiter is blocked by
  its own blocker (`loBlockingPIDs`), which guards against picking up a foreign pid.
- **#4/N1, #5a, #5b, #1b:** real production code on both sides. 5a/5b have documented
  hand-kills. #1b is timing-dependent for its Escalate-race purpose (see above).
- **#3 (deferred receipt vs. fresh callback): one assertion is vacuous.**
  `SELECT count(DISTINCT ledger_transaction_id) FROM payment_attempts WHERE id = $1` reads one
  column of one row, so it can never exceed 1. The "exactly 1 ledger posting" claim needs to count
  `ledger_transactions` for `(provider_id, provider_tx_id)` or for the intent. The DB's own
  idempotency constraint makes a double post structurally unlikely, but as written the assertion
  proves nothing. LOW; fix the query.

## Conditions to close

1. Pin S-M2's `withdrawal_state_after` and `evidence_class`.
2. Add a deterministic Escalate-on-terminal test.
3. Fix `TestA7_3`'s posting-count query.
4. Hand the SP-C evidence to `security`/`ledger-finance` and correct the addendum's root-cause
   text.

Optional hardening:
- `escalateAmbiguousPayout` should swallow the conflict only for the terminal/already-escalated
  cases;
- record the actual withdrawal before-state in the S-M2 audit.

---

# FH-6 round 2 — confirmation of code-review conditions (`b7f84ec`)

- Method:
  - detached worktree at `b7f84ec`, build, `go vet -tags=integration`;
  - **pinned** `golangci-lint` 2.9.0: `run ./...` gives **0 issues**, and `--build-tags=integration
    ./internal/payments/...` gives 0 issues. The round's own lint used a different binary; the
    pinned one agrees;
  - targeted payments tests on a private DB (created via `TEST_ADMIN_DATABASE_URL`, migrated to
    0106, grants applied);
  - mutants reverted with `git checkout`.
- No role, password or privilege changes. The worktree and the private DB are removed.

| # | Condition | Result |
|---|---|---|
| 1 | Pin `withdrawal_state_after` and `evidence_class` | **Closed.** `TestPayoutResolveAudit_CodeReview_WithdrawalStateAfterAndEvidenceClass` drives a completing resolve. Both previously surviving mutants are now **killed**. |
| 2 | Deterministic Escalate-on-terminal test; swallow only on a confirmed terminal/escalated state | **Closed.** `TestEscalate_PC3_RefusesOnATerminalAttempt_StaleSnapshot` kills the predicate-removal mutant on a single run (it previously survived 10 runs and was caught 1 time in 40). `escalateAmbiguousPayout` now re-reads and swallows only on a terminal state or `escalated_at` set; a missing or invisible row surfaces the re-read error. The swallow-everything mutant is **killed** by `TestEscalateAmbiguousPayout_CodeReview_UnintendedConflictIsLoud`. |
| 3 | `TestA7_3` posting count | **Closed.** It now counts `ledger_transactions` with `transaction_type='deposit' AND correlation_id = intent`. Deposit postings set `CorrelationID: intent.ID` (`orchestrator.go:877`), so the count is meaningful. |
| 4 | Before-state read under the lock | **Closed.** Both `payoutResolveAudit` call sites take `LockForPayoutEvidence` and re-read the attempt inside the transaction. `withdrawal_state_before` comes from that locked read, not a hard-coded `submitted`. A mutant restoring the stale snapshot is **killed** by `TestPayoutResolveAudit_PC3_BeforeStateReadUnderTheLock`. |

**Verdict: all four code-review conditions are closed.** Code review has no remaining conditions
on FH-6.

This round's other contents (the SP-C V1 fix, P-C1/P-C2, A7-C1, the ADR 0082 as-built rows, the
kill-switch hold audit, credential pool threading) were outside this confirmation. They belong to
the `ledger-finance` and `security` confirmations.
