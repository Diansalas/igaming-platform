# ADR 0119 S5 + S6 — ledger-finance review of the ADR 0082 A9 / ADR 0099 A-19 HARD GATE

- Reviewer: `ledger-finance` specialist (independent review, no code changed)
- Branch: `worktree-agent-a031d51e36f08b4f0`, head `131f0d8`, base `ff23a5c`
- Date: 2026-10-14
- Scope: `payments.reassertSubmitter` in `ClaimForDispatch`; `capability.reassertActor` in K1 approve / revoke and
  the A9 "K1 session" correction; lock order against ADR 0082 (suspension / role-change writers, T2 `lock_timeout`);
  S56-R5 (withdrawal `resolve`); ledger / balance writes; retry idempotency after a `401 session_revoked`.

## Verdict

**APPROVED (ledger-finance sign-off on the financial parts), with non-blocking findings LF-1 to LF-7.** The HARD
GATE from the S4b ledger-finance review is met for withdrawal `submit` and K1 `approve` / `revoke` in the same
change that adds the first production suspension / role-change writer. No ledger posting, balance projection or
idempotency key is altered; no float; no historical row mutated. S56-R5 does not have to be fixed now (reasoning
below), on the condition recorded in that section.

Sign-off covers financial correctness only. `security` (GUC pair, RLS, runtime-SQL residual S56-R1) and `architect`
(the A9 table amendment and the K1 correction) sign-offs are separate and still required.

## What was verified, and how

All database work ran on a PRIVATE database built from this worktree's migrations (`lf_s5s6_<epoch>`, via the
scratchpad `priv_db.sh` / `priv_test.sh`). No shared role, password or test infrastructure was touched.

| Run | Result |
|---|---|
| `TestSubmitReassertion_*` (5 tests, 5 change arms), `TestStatic_SubmitReassertionPins`, `TestS4b_*` (payments) | PASS |
| `TestK1Reassertion_*` (4 tests, suspended and `sv`-only arms), `TestSubmitWithdrawalHandler_SubmitterSuspendedInFlight_401NothingWritten` (httpserver) | PASS |
| `internal/stafflifecycle`, `internal/db` (integration) | PASS |
| `internal/httpserver -run 'Withdrawal\|Capability\|K1\|Submit'` (integration) | PASS |
| `internal/payments -run 'Claim\|Payout\|Submit\|S4b\|K3\|HSEC\|Hold'` (integration) | see "Payments suite" below |
| Unit `internal/payments`, `internal/capability` | PASS |
| A temporary reviewer test (written, run, deleted; not committed) through the real `ClaimForDispatch` | PASS, see below |

Temporary reviewer test (refusal side effects and retry). One approved withdrawal; counts of
`kyc_enforcement_decisions`, `ledger_entries`, `ledger_transactions`, `withdrawal.submit.http` audit rows,
`payout_attempt_destination_snapshots` and `payment_attempts`, before and after each step:

| Step | Outcome | Counts |
|---|---|---|
| before | | decisions 1, entries 4, tx 2, audit 0, snapshots 0, attempts 0 |
| claim with a stale `sv` (2 vs 1) | `ErrSubmitterNotLive` | unchanged |
| same stale claim again (client retry) | `ErrSubmitterNotLive` | unchanged |
| claim with a valid token | success | decisions +1, audit +1, snapshot 1, attempt 1; ledger entries / tx unchanged |
| replay of the valid claim | `withdrawal.ErrStateConflict` | unchanged |
| per-transaction, per-asset debit = credit over the tenant's `ledger_entries` | 0 unbalanced | |

So a refusal writes nothing at all (not only "no attempt, still approved", which is what the committed
`assertNothingWritten` checks: see LF-7). The ALLOW decision row and the B6 audit roll back with the claim, and a
retry after re-login claims exactly once. The T1p claim posts nothing to the ledger, so no ledger effect can appear
or vanish here (the hold was placed at request time; phase C settles it).

## 1. `payments.reassertSubmitter` (submit)

- **Position: correct.** Inside the T1p transaction, after `LockApprovedForSubmission` (withdrawal FOR UPDATE),
  `MarkSubmittedPending` and `InsertSubmittingAttempt` (the last L1 row), before the destination snapshot, the B6
  audit and the commit. No L2 / L3 (ledger account / wallet) lock is taken anywhere in T1p, so "before L2/L3" holds
  trivially. The earlier statements in the transaction (tenant / brand active check, KYC evaluation in a savepoint,
  instrument FOR SHARE in the destination gate, the decision row) touch no row a staff writer touches.
- **Full rollback on refusal: correct.** `reassertSubmitter` returns `db.ErrActorStaffNotLive`, the closure returns
  it, `alerting.InTx` rolls back: no decision row, no state change, no attempt, no snapshot, no audit, no
  `Pending` alert flush (the error path returns before `pending.Flush`), no provider call (phase B is never reached).
  The kill-switch audit side path is gated on `ErrPayoutKillSwitchEngaged` and is not triggered. Verified above.
- **Prod refusal of an empty token role: correct.** `submit_reassert_prod.go` (`//go:build !integration`) returns
  `false`, so `reassertSubmitter` reaches `db.CheckActorStaffLive`, which refuses an empty role and an `sv < 1`.
  The integration fallback only skips when `Role == ""`; a `SubmitActor` that carries a role is always checked. The
  handler always sets `Role` and `StaffVersion` from the verified token. Pinned by `TestStatic_SubmitReassertionPins`.
- **Visibility.** Submit runs in `db.WithTenant(tc.TenantID)` (via `alerting.NewTenantRunner`), the same scope as the
  pre-claim eligibility check; the submitter's own tenant row is visible through the dual-scope policy. `staff_version`
  is `BIGINT`, scanned as `int64` by `ScanActorStaffRows`: no silent zero.
- **HTTP mapping.** `errors.Is(err, db.ErrActorStaffNotLive)` maps to `401 session_revoked` with the
  `staff_reassertion_refusals_total{flow="withdrawal_submit"}` counter; no earlier branch can shadow it.
- **Retry idempotency after 401: correct.** The request stays `approved`; a retry with the same stale token is
  refused again (here, and by request-level liveness first); after re-login the claim succeeds once; a replay is a
  `409` state conflict. The approved -> submitted CAS under the L1 row lock remains the single-claim guarantee.

## 2. `capability.reassertActor` (K1 approve / revoke) and the A9 correction

- **Positions: correct.** Approve: `staff_capability_grant_requests FOR UPDATE` (L1) then the approver's own row
  `FOR SHARE`, before `attachProof` and the approval / grant inserts. The approval guard's own
  `SELECT ... FOR UPDATE` on the request row is now a re-entry of a lock already held, so no new ordering. Revoke:
  grant row `FOR UPDATE` then the revoker's row `FOR SHARE`, before the `UPDATE`. Reject is not re-asserted
  (protective; matches the A9 table).
- **The CORRECTION is right.** `runCapabilityTx` uses `db.WithPlatformAdmin` for a platform caller and
  `db.WithPrincipalScope` for a tenant caller; K1 never runs under `WithTenant(target)`. The approval guard
  (migration 0112, R-7) refuses any non-platform session for a decision, so approve is platform-only and the platform
  row is visible with `app.tenant_id` empty. The positive "approval first, suspension waits" tests could not pass if
  the row were hidden (every approval would be refused and nothing would be locked). No `app.actor_staff_id` GUC is
  needed on K1. Recommend the architect accept the correction to the recorded HARD GATE shape.
- **Fail-closed without a principal.** `reassertActor` returns `nil` when no `ProofActor` is in the context. In a
  production binary that path also has no actor proof (`TestSessionProofHook` is nil, pinned), so the governed
  approval / grant write is refused by the database (AP001). Acceptable; see LF-4.

## 3. Lock order (ADR 0082) — no new deadlock cycle found

Writers of `staff_users` on this branch: lifecycle T2 (suspend / offboard), `ChangeRole`, `Reactivate`, reset MFA,
invite accept, person link. Each locks the target `staff_users` row (and the actor's row `FOR SHARE`, ascending id)
and then writes only staff-side tables: `staff_auth_challenges`, `staff_enrolment_tokens`, `sessions`,
`staff_mfa_factors`, `staff_mfa_recovery_codes`, `staff_lifecycle_events`, `audit_log` (plus the `SF020` advisory
lock, taken only by staff writers). None of them locks a withdrawal, payment attempt, K1 request / grant,
K2 / K3 / HSEC request or any L2 / L3 ledger row. Every re-asserting financial flow takes its L1 rows first and the
staff row after. A staff writer therefore can wait on a financial transaction (or vice versa) but never in both
directions, so no cycle is possible. `audit_log` is not hash-chained (no shared chain-head lock), so both sides
inserting audit rows does not create a wait. The K1 grant insert's FK `KEY SHARE` on the grantee row conflicts with a
T2 `FOR UPDATE` on that grantee: a one-directional wait (K1 waits for T2), no cycle.

**T2 `lock_timeout` vs execution share locks: sound.** T2 sets `lock_timeout` (5 s) locally; a long K2 / K3 / HSEC
execution makes T2 fail `55P03` (retried once, then `lifecycle_busy`, sessions already revoked by T1) while the
execution commits. T1p and K1 carry no provider I/O, so they hold the share lock for milliseconds. The financial side
sets no `lock_timeout`, so a claim waiting on an in-flight T2 waits for T2's short commit and is then refused
(`TestSubmitReassertion_SuspensionInFlightFirst_ClaimWaitsThenRefused`). No partial money outcome either way. See
LF-2 for `ChangeRole`.

## 4. S56-R5 — withdrawal `resolve` has no in-transaction re-assertion

**Assessment: does not need to be fixed now; keep it as a recorded residual.** Reasons:

1. `resolve` makes no staff-discretionary money decision. It reads the withdrawal and its live attempt (no lock), then
   calls `payments.PollPayoutStatus`, the same phase B (QueryStatus, no transaction held) and phase C (state-aware
   evidence matrix, amount / asset cross-check, CAS guards) the automatic sweeper runs. The outcome (completed /
   failed / still submitted) and any ledger effect are determined by provider evidence, not by the staff member.
2. A staff member suspended between the request-level liveness check and phase C can only cause what the sweeper
   would cause on its next tick. No value can be moved that would not otherwise move, and none in a different amount
   or direction.
3. Phase C is idempotent (CAS on the attempt / withdrawal state), so a concurrent sweeper and staff resolve cannot
   double-post.
4. The only staff-attributed write is the audit row inside phase C; an attribution to a just-suspended staff member is
   accurate (they did trigger it) and not a money risk.

Conditions (to record with S56-R5): re-open as a HARD GATE if `resolve` ever gains a staff-chosen outcome or a manual
override (that path today is K3 / M4 force resolution, which is already re-asserted). If a re-assertion is later
added inside `PollPayoutStatus`, the resolve handler must first pass `Role` / `StaffVersion` into its `SubmitActor`
(today it passes neither; with the production fallback off, every resolve would be refused, fail-closed but an
outage).

## 5. Ledger / balance writes

`git diff ff23a5c..131f0d8` touches no file under `internal/ledger` or `internal/wallet`; migration 0139 references no
ledger, wallet or balance object. The K2 (`adjustment`), K3 / M4 (`manual_resolution.go`) and HSEC
(`withdrawal_hold_resolution.go`) changes only add `staff_version` to the existing A8 `SELECT` list and to the
comparison; lock statement, set and position are unchanged. No balance is updated directly; no posting path or
idempotency key changed.

## Findings

| # | Severity | Finding | Recommendation |
|---|---|---|---|
| LF-1 | Low (doc) | Only the ALLOW path of the claim is re-asserted. The KYC-deny branch (`withdrawal.DenyForCompliance`, which releases the hold, a ledger posting), the KYC-unavailable branch (decision row + denied audit) and the destination-refusal branch (denied audit, possibly a P1 alert) commit before reaching `reassertSubmitter`. All are protective and decided by KYC / destination evidence, not by the submitter, so this is financially acceptable. But the A9 table row says only "submit" and does not state this. | State explicitly in the A9 submit row that non-ALLOW outcomes are protective and not re-asserted (the same wording as K1 reject). No code change. |
| LF-2 | Low | `stafflifecycle.ChangeRole` is single-phase and sets no `lock_timeout`, unlike suspension T2. A9 item 6 bounds writer starvation for suspension only. Not a deadlock (see section 3); a role change can wait without bound behind share holders (in practice short: no provider I/O in any re-asserting transaction). | Either set the same `lock_timeout` in `ChangeRole` (map `55P03` to `lifecycle_busy`) or amend A9 item 6 to say role change is deliberately unbounded. |
| LF-3 | Low (test gap) | K1 tests cover the platform session only, with suspended and `sv`-only arms. There is no token-role-mismatch arm and no tenant-scope revoke test (`revoked_by_scope = 'tenant'` is allowed by 0112, runs in `WithPrincipalScope`). `reassertActor` re-implements `db.CheckActorStaffLive` (import cycle), so drift between the two is caught only by tests. | Add a K1 role-mismatch arm and a tenant-scope revoke arm (both orders), or a unit test that `reassertActor` and `CheckActorStaffLive` agree on a shared table of cases. |
| LF-4 | Info | `reassertActor` is a no-op without a `ProofActor` in the context. Safe today because the same path has no actor proof and the database refuses it (AP001), and `TestSessionProofHook` is pinned nil in production. | Keep the pin. If K1 ever gains a non-HTTP caller with its own proof signer, it must also carry `Role` / `StaffVersion`. |
| LF-5 | Info | K1 `revoke` is the emergency stop (ADR 0099 section 8.1) and is protective, but it is re-asserted per the HARD GATE text. Consequence: an admin whose `sv` was just bumped must log in again before revoking. | No financial objection either way; the architect may keep or relax it. |
| LF-6 | Info | The re-assertion governs T1p only. A suspension committed after T1p does not stop phase B (`Withdraw`) or phase C. This is by design: the claim is the authorization point and the sweeper would continue the attempt anyway. | None; consistent with the other flows. |
| LF-7 | Low (test strength) | `assertNothingWritten` in `adr0119_submit_reassert_integration_test.go` checks only "no attempt" and "state approved". It would not catch a regression that commits the ALLOW decision row, the B6 audit or the snapshot on a refusal (for example, an error swallowed on the refusal path). The reviewer's temporary test confirmed this does not happen today. | Extend `assertNothingWritten` with the decision-row, `withdrawal.submit.http` audit and snapshot counts, and add a "refused then retried with a valid token claims once, replay 409" test. |

None of LF-1 to LF-7 blocks merge from the ledger-finance side.

## Payments suite

The first broad run (`-run 'Claim|Payout|Submit|S4b|K3|HSEC|Hold'`, `-timeout 20m`) hit the 20-minute test binary
timeout inside `TestK3_C17b_T11_GuardMatrixIsIdenticalOn0107AndHead`. That is a combinatorial scratch-schema matrix of
the `payment_attempts` guard; neither it nor that guard changed on this branch. It was re-run excluding that test
(`-skip GuardMatrix`, `-timeout 60m`); result: PAYMENTS_RESULT_PLACEHOLDER.
