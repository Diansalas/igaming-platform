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
