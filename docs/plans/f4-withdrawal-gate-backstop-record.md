# F4 record: WD-RG-1 withdrawal RG/risk gate at payout dispatch, and its database backstop

Status: implementation record for platform-freeze item F4 (`docs/plans/platform-freeze-work-list-2026-10-16.md`), closing
A6 SR6 I-2 / A6 residual 4 (`docs/plans/a6-rg-enforcement-record.md`, section 8 item 4 and section 10). Proposed registry
text only: this file does not edit the task registry, HANDOVER, progress, active-stage, the decision register or any
ADR. ALL providers are MOCK. Software capability only: no regulatory approval is claimed. Needs `security` and
`ledger-finance` review before merge (probe lists in section 9).

## 1. Proposed registry text

| Item | Label | Note |
|---|---|---|
| WD-RG-1 at payout dispatch (T1p claim, sweeper T2 re-claim, T12 resend) | `IMPLEMENTED` (RG = real, risk = real engine over MOCK-fed rules) | Same gate, same rules, same lock position (before L1) as request time. Denial parks, alerts, audits; unavailable fails closed. |
| WD-RG-1 database backstop (migration 0169) | `PARTIALLY IMPLEMENTED` | WD053: approved -> submitted and a payout attempt's created/ambiguous -> submitting require an ALLOWING decision row recorded in the SAME transaction. WD052: account and wallet status re-checked in SQL. NOT in SQL: the risk rules (the DB cannot evaluate them) and the negative-balance rule (residual R2). A deliberately forged allow row by the runtime credential is not detected (residual R1). |
| Alert `withdrawal.payout_gate_denied` | `IMPLEMENTED` (raise); notification `NOT IMPLEMENTED` | p2 engineering default; ALERT-DELIVERY-1 still open (nobody is paged). |
| A6 residual 4 / SR6 I-2 | Proposed: CLOSED for dispatch re-evaluation; narrowed to R1 and R2 for the backstop | |

## 2. Design (the minimal safe version) and why

### 2.1 Where the gate re-runs

Every point that leads to a NEW `Withdraw` call:

- **T1p** `payments.(*Orchestrator).ClaimForDispatch` (approved -> submitted, attempt insert).
- **T2** `(*Sweeper).reclaimPayoutCreated` (a never-sent attempt `created -> submitting`).
- **T12** `(*Sweeper).resubmitPayoutAmbiguous` (a resend `ambiguous -> submitting`).

Not gated, by design: the QueryStatus poll, phase C evidence, completion (E9), failure (E10), rejection, cancellation,
reversal, every dispute / park / force-resolution path. Once money may have left, the platform records the truth and
never refuses it (test `TestF4_DB_TerminalEdgesAreNotGated`).

### 2.2 Which rules (identical to request time)

The SAME injected `withdrawalgate.Gate` (production `withdrawalgate/standard`, now the default of `NewOrchestrator`),
fed the request's immutable columns (tenant, brand, account, wallet, asset, amount). Therefore:

- RG half (`rg.EvaluateWithdrawalEligibility`): account must be `active`, wallet `active`, wallet balance not negative.
  A self-exclusion or a time-out is ALLOWED (ADR 0115, owner decision; the restriction in force is recorded in the
  decision row's `restriction_in_force`).
- Risk half (`risk.Evaluate`, Operation `withdrawal`): ALLOW proceeds; DENY and REVIEW refuse (WD051); an unrecognised
  outcome or an error is "no decision".
- A nil gate, an evaluation error or an unrecognised gate code is `withdrawal.ErrGateUnavailable` (never an allow).
- Cumulative risk rules for `withdrawal` still fail closed at both points (no cumulative spec exists for the operation),
  so the request's own hold is never double counted. If a withdrawal cumulative spec is ever added it must exclude the
  request being dispatched (recorded for ledger-finance, section 9).

### 2.3 Lock order (ADR 0082 A7-N1)

The RG half takes the L0.4 person advisory lock and the risk half may take L0.5, so the gate must run before the first
L1 parent lock. T1p now runs, inside its one claim transaction: an unlocked pre-read of the request (immutable columns
only) -> if `approved`: `tenant.RequireActiveForPaymentInitiation` (status keys SHARED, brand row FOR SHARE, the
request-path order) -> B13-B method-mismatch pre-check -> WD-RG-1 gate (L0.4, L0.5) -> `LockApprovedForSubmission` (L1)
-> the existing chain unchanged (status re-check, mismatch re-check, KYC, routing, capability, destination, KYC decision
row, **allow decision row**, approved -> submitted, attempt insert, submitter re-assertion, snapshot, audit). T2 / T12 run
the gate before `lockSubmittedRequest` (L1), then the existing chain; the decision is applied after L1 (a denial
escalates; an allow records the decision row before the CAS). Pinned by `TestF4_Claim_GateRunsBeforeTheRowLock` (the
gate is evaluated while another transaction holds the request's L1 lock) and by `TestB13B_Static_ClaimForDispatchOrder`.

A request that became `approved` between the unlocked pre-read and the lock (no gate evaluated in this transaction) is
refused as unavailable (retryable): nothing is claimed without a decision taken in the claim transaction.

### 2.4 Outcomes

| Outcome | T1p | T2 / T12 |
|---|---|---|
| Allow | allowing decision row (stage `dispatch`), then the unchanged claim | allowing decision row (stage `redispatch`, bound to the attempt), then the unchanged re-claim / resend |
| Deny (WD050 / WD051) | PARK in the existing safe state: request stays `approved` with its hold, no attempt, no provider call, no KYC decision. Commits: denying decision row, staff-attributed `withdrawal.submit.http` denied audit (gate code, reason, stage, brand, wallet, asset, amount, risk outcome), alert `withdrawal.payout_gate_denied` (discriminator `withdrawal:<id>:gate:<code>`). Returned as `ErrPayoutWithdrawalGateDenied` wrapping `*withdrawal.GateDeniedError`; HTTP 409 `conflict`, generic message. The exits are unchanged: a re-submit re-evaluates; a reject releases the hold. | ESCALATE (T16, the state KYC and destination denials at T2/T12 already use): attempt keeps its state, no resend, no hold release. Commits: denying decision row, Escalate, system audit `payments.payout_redispatch_denied_by_withdrawal_gate`, alert (`payout_attempt:<id>:gate:<code>`). An already-escalated attempt is only rescheduled (no new row, no new alert). |
| Unavailable | the whole claim rolls back (nothing written, `approved`, no attempt); a detached best-effort `withdrawal.submit.http` denied audit labels it; `ErrPayoutWithdrawalGateUnavailable`, HTTP 503 | the tick rolls back; the attempt is re-leased on a later tick (never a send) |

Replay: a claim of a request that is not `approved` is NOT evaluated (the pre-read state check) and is refused by
`LockApprovedForSubmission` exactly as before (`TestF4_Claim_Replay_IsNotReEvaluated`: three replays, one evaluation,
one decision row; a foreign tenant's claim of the id is `ErrNotFound` with zero evaluations).

### 2.5 The database backstop (migration 0169)

- `withdrawal_gate_decisions`: append-only (WD055 on UPDATE / DELETE / TRUNCATE, owner included), FORCE RLS,
  tenant staff/system scope (`tenant_staff_scope`, never a player session), runtime role SELECT + INSERT only (also in
  `deploy/init-app-role.sql`). `id`, `decided_at` and `decided_xact` (= `pg_current_xact_id()`) are set by the BEFORE
  INSERT trigger, never by the caller. The trigger refuses (WD054) a row whose request is not visible in the tenant, a
  `dispatch` row for a request that is not `approved`, a `redispatch` row for a request that is not `submitted` or
  whose attempt is not a payout attempt of that request. CHECKs: stage/attempt shape, outcome shape (`allowed` without a
  gate code; a denial with WD050/WD051 and a reason), closed risk outcome and restriction vocabularies.
- `withdrawal_requests_zz_dispatch_gate_guard` (BEFORE UPDATE, WHEN approved -> submitted) and
  `payment_attempts_zz_payout_redispatch_gate_guard` (BEFORE UPDATE, WHEN a payout attempt moves created|ambiguous ->
  submitting): WD052 if the account or wallet is not `active` (or invisible), WD053 unless an ALLOWING row of the stage
  (and attempt) was recorded in THIS transaction and no denying one was. "Recent" = the same transaction: a decision
  committed by an earlier transaction never authorizes a later transition.
- `zz` names: both fire after every existing BEFORE ROW trigger of their table, so every existing refusal keeps its
  code and precedence (catalog-pinned, `TestF4_DB_Catalog`). No lock is taken; no SECURITY DEFINER; search_path pinned.
- Down: refused (WD095) while a decision row exists (compliance history), and while an alert of the new Kind exists.
- New SQLSTATEs: WD052, WD053, WD054, WD055, WD095. WD050 / WD051 stay application-level codes.

## 3. Changed assertions and fixtures (none weakened)

- `internal/db/migrate.go` + `migrate_test.go` (`TestReservedUnusedVersions_ExactSet`): 166, 167, 168 added to
  `reservedUnusedVersions` because 0169 lands before 0166-0168 (held by F1 and other freeze work). CHANGED ASSERTION.
  MUST be removed again when those migrations merge (merge friction, section 8).
- `internal/payments/b13b_static_test.go` `TestB13B_Static_ClaimForDispatchOrder`: the order list is now matched as a
  subsequence starting at the L1 lock (the F4 pre-lock block also calls `RequireActiveForPaymentInitiation`); the
  post-lock order it pins is unchanged, and an F4 order pin was added (status gate < WD-RG-1 gate < L1 < allow row <
  submitted). CHANGED ASSERTION.
- `internal/withdrawal/adr0113_state_machine_integration_test.go` `TestADR0113_HeaderFirstAttacks`: the header walk to
  `submitted` is now asserted twice: without a decision WD053 (new), with a forged decision row still WD031 at COMMIT
  (the original assertion, unchanged code).
- `internal/alerting/static_wiring_test.go` and `static_derived_raise_set_test.go`: `applyRedispatchGate` added to the
  reviewed raise helpers (reached only from the T2 / T12 InTx closures; raise is its last statement).
- Fixtures that walk E7 / a payout T2 directly now record a fixture ALLOW decision first (they test the state machine,
  not the gate): `internal/testsupport/wdpath` (Advance), `internal/withdrawal` (`f4ThenSubmit` at 11 call sites in 7
  files), `internal/payments` (`payout_dispatch_integration_test.go` T2 race, `dep_ref_validate_integration_test.go`
  seed, `b11_payout_unbound_hold_integration_test.go` legacy E7). Assertions unchanged.
- `internal/launchgov/closure_integration_test.go` `playerWithWallet`: activates the registered player (a withdrawal
  reaches `submitted` only for an active account, WD052; the A6 precedent in `closure_withdrawal_race`). Assertions
  unchanged.
- `internal/payments/a5_amount_dispute_integration_test.go` `TestA5_Migration0163_DownRefused...`: before stepping
  down from head to 0163, the scratch-database owner removes the 0169 decision rows (append-only trigger and FORCE RLS
  lifted for that statement on the THROWAWAY database); otherwise 0169's own WD095 refusal stops the chain first. The
  MR099 assertion is unchanged.
- OpenAPI prose of `POST /v1/admin/withdrawals/{id}/submit`: 409 (held by the gate) and 503 (gate unavailable)
  described. No new response code, no new error code.

## 4. Tests added

`internal/withdrawal/f4_dispatch_gate_integration_test.go` (runtime role): E7 needs an allowing decision of the same
transaction (no decision / deny / allow+deny / allow from an earlier transaction all WD053; MarkSubmitted, MarkSubmittedPending
and a raw UPDATE alike; the WD053 translation); RG backstop WD052 (suspended, pending_verification, frozen wallet) and a
self-exclusion allowed; terminal edges not gated (E9, E10, E5, E2 on a suspended account and frozen wallet); decision
rows append-only (WD055 owner, 42501/WD055 runtime), trigger-owned id / instant / xact, shape and stage rules; tenant
isolation (foreign tenant reads nothing, cannot write for a foreign request, a player session reads and writes nothing,
a foreign E7 matches no row); `EvaluateDispatchGate` = request-time rules (nil / failing / unknown-code gate unavailable,
allow, self-exclusion reported, a risk rule added after the request WD051, suspended WD050, negative balance after
approval WD050); catalog pins; migration 0169 up / down / up byte-identical snapshot and the WD095 down refusal.

`internal/payments/f4_payout_gate_integration_test.go` (MOCK provider, private scratch database per test): allow
records one decision; self-exclusion allowed and recorded; a risk rule that flips between request and dispatch (DENY and
REVIEW) parks in `approved` twice in a row (decision rows, staff audits, one open alert, no attempt, hold unchanged, no
KYC decision), then the governed reject still releases the hold and the ledger balances; account suspended after approval
parks (WD050) and goes through after reactivation; unavailable (failing gate, nil gate) fails closed with the detached
label and goes through on retry; replay not re-evaluated and tenant isolation of the claim; gate before L1 (with the L1
row held by another transaction); concurrency with request and approval on the same player (8 rounds, no deadlock, ledger
balanced, projection = rebuild); T2 denial escalates without resend and the second tick adds nothing; T2 allow records
the attempt-bound decision; T12 risk denial escalates without resend (idempotent manifest, so T12 reaches the gates);
the payout attempt backstop (WD053 without a decision, WD052 on a suspended account, passes with an allow).

`internal/httpserver/withdrawal_f4_dispatch_gate_integration_test.go`: staff submit after the account was suspended is
409 with a generic body (no WD050 / reason / status in it), request `approved`, one decision row, one staff audit, one
alert, no attempt, no ledger posting; after reactivation the same submit goes through.

## 5. Commands and counts

(Filled in section 10 after the final runs.)

## 6. Mutation pass

(Filled in section 10.)

## 7. Residuals

- **R1 (backstop strength, Medium for review).** WD053 proves that an allowing decision was RECORDED in the transaction
  of the transition, not that its content is true: the database cannot evaluate a risk rule, so a runtime-credential
  code path that deliberately inserts an `allowed = true` row and then transitions is not detected (the C-WD023 caveat of
  0132 for approve records). It does close the residual as A6 framed it: a code path that BYPASSES the gate evaluation
  (forgets it) is refused. Detection of a forged allow would need either a signed decision (an actor-proof-like token
  minted by the gate) or a database-side risk evaluator; both are large and are not built.
- **R2 (negative balance not mirrored in SQL).** WD052 re-checks account and wallet STATUS only. The negative-balance
  refusal (owner decision 13) is applied at dispatch by the application gate (test
  `TestF4_EvaluateDispatchGate_NegativeBalanceAfterApproval_Refuses` and the payments park path), but not by the
  trigger: the shared test fixture `wdpath.Plant` deliberately plants unfunded holds (player_cash negative) and 14
  reconciliation fixtures (payment-statement, D2, R2, R8, R9, Res1) and the launch-closure fixtures depend on it; making
  the SQL rule bind would re-fund those fixtures, which belong to the payments-recon workstream editing the same files
  for F2/F3. A governed path cannot reach E7 with an unfunded hold (RequestWithdrawal checks funds), so the residual is a
  chargeback / negative event committed between the gate read and the transition inside one claim transaction, or a
  forged decision (R1). Recommended follow-up: add the clause once the reconciliation fixtures are funded.
- **R3 (status race window).** The trigger reads account / wallet status without a lock (READ COMMITTED, at the
  transition statement). A status change that commits after the transition statement but before the claim commits is not
  seen; it is seen by the next dispatch point. Same as every other status read on the money path (no account-status
  lock exists in ADR 0082).
- **R4 (park has no queue).** A parked payout stays `approved`; nothing re-submits it automatically. The alert is raised
  but notification is ALERT-DELIVERY-1 (open). Staff see it in the approved list; the reason is in the audit row.
- **R5 (T2/T12 escalation is sticky).** A WD-RG-1 denial at T2 / T12 escalates (T16) like a KYC denial; there is no new
  staff action to un-escalate after the cause is cleared beyond the existing ones (RejectCreated for a never-sent
  attempt, force-resolution). Unchanged behaviour class.
- **R6 (unavailable label is best-effort).** The T1p unavailable audit is written in a detached transaction after the
  rollback (the kill-switch pattern); its failure is logged, not surfaced.
- **R7.** The existing T12 KYC test (`TestSweeper_T12Resubmit_KYCDeny_EscalatesNeverResends`) uses the default
  non-idempotent mock manifest, so it escalates on `non_idempotent_manifest` before the KYC gate is ever reached; it does
  not exercise the KYC gate at T12. Pre-existing, not changed here; the new F4 T12 test sets an idempotent manifest.

## 8. Merge friction

- `reservedUnusedVersions` / `TestReservedUnusedVersions_ExactSet`: remove 166-168 as the corresponding migrations land.
- `deploy/init-app-role.sql`: new block at the end.
- `internal/alerting/kind.go` + the two static wiring lists: shared lines.
- `ClaimForDispatch` and the two sweeper claim transactions: F3 (payout tombstone follow-ups) may touch nearby code.
- Any other agent's test that walks E7 / a payout T2 directly without `wdpath` now needs a decision row (WD053); use
  `wdpath.Advance` or record one (`withdrawal.RecordDispatchGateDecision`).

## 9. Review probe lists

Security:
1. Can any session other than a tenant staff/system session of the request's tenant read or write
   `withdrawal_gate_decisions` (player scope, acting / platform principal, another tenant)? Grants SELECT + INSERT only?
2. Can a caller choose `id`, `decided_at` or `decided_xact`, or satisfy WD053 with a row from another transaction,
   another request, another attempt or another stage? (Probe: insert in tx A, transition in tx B; savepoint rollback of
   the decision before the UPDATE.)
3. Is the 409 body of a dispatch denial free of reason codes and status words? Is the staff audit complete (actor, IP,
   user agent, request id) and free of PII?
4. The `zz` trigger order: does any existing guard's refusal change code or precedence?
5. R1: is "a recorded allow in the same transaction" an acceptable backstop for the launch risk list, or is a signed
   decision required before real money?
6. The new alert Kind: attributes are closed codes only; tenant subject; in_tx; dedup by discriminator.

Ledger-finance:
1. Lock order at T1p / T2 / T12: status keys and L0.4 / L0.5 strictly before L1; no new lock in the triggers; the
   deadlock probe of `TestF4_Claim_ConcurrentWithRequestAndApproval_NoDeadlock` (claims racing new requests and
   approvals of the same Person).
2. A denial never moves money: T1p leaves `approved` with the hold (no posting); T2 / T12 escalate with no release; the
   ledger balances and the projection equals the rebuild in every denial test.
3. No double evaluation: replays and concurrent claims evaluate at most once per committed claim; the losing claim
   rolls back its decision row with it.
4. Completion, failure, rejection, reversal and every evidence path are untouched (no new refusal can strand a payout
   that may have been sent).
5. Reconciliation check D and the payment-statement reconciliation are unaffected (suite green; no new ledger rows).
6. R2: confirm the negative-balance rule may stay application-only until the reconciliation fixtures are funded.
7. Cumulative withdrawal risk rules still fail closed; if a cumulative spec for `withdrawal` is ever added, the request
   being dispatched must be excluded from its own usage (its hold is already posted).

## 10. Verification

(Below.)
