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

A NOT-ACTIVE tenant or brand is not refused by the pre-lock status call: the gate is skipped and the unchanged post-L1
H-SEC-11 check refuses it with the same error and the same precedence as before (the claim still serializes on L1 with
a governed hold release: `TestHSEC_HoldRelease_Concurrency_ExecuteVsSubmit` unchanged). Any other error of the pre-lock
status call aborts. A request that became `approved`, or a subject that became active, between the unlocked pre-read
and the lock (no gate evaluated in this transaction) is refused as unavailable (retryable): nothing is claimed without a
decision taken in the claim transaction.

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

- `internal/db/migrate.go` + `migrate_test.go` (`TestReservedUnusedVersions_ExactSet`): after the merge with the
  upstream reservation of 0167-0169, 169 is REMOVED from `reservedUnusedVersions` (0169 is now a real migration; the
  list's own rule). 0167 / 0168 stay reserved for F2 / F3. CHANGED ASSERTION (the expected set).
- `internal/payments/b13b_static_test.go` `TestB13B_Static_ClaimForDispatchOrder`: the order list is now matched as a
  subsequence starting at the L1 lock (the F4 pre-lock block also calls `RequireActiveForPaymentInitiation`); the
  post-lock order it pins is unchanged, and an F4 order pin was added (status gate < WD-RG-1 gate < L1 < allow row <
  submitted). CHANGED ASSERTION.
- `internal/withdrawal/adr0113_state_machine_integration_test.go` `TestADR0113_HeaderFirstAttacks`: the header walk to
  `submitted` is now asserted twice: without a decision WD053 (new), with a forged decision row still WD031 at COMMIT
  (the original assertion, unchanged code).
- `internal/alerting/static_wiring_test.go` and `static_derived_raise_set_test.go`: `applyRedispatchGate` added to the
  reviewed raise helpers (reached only from the T2 / T12 InTx closures; raise is its last statement).
- Fixtures that walk E7 or a payout T2 / T12 directly now record a fixture ALLOW decision first (they test the state
  machine, the evidence paths or reconciliation, not the gate). Assertions unchanged:
  - `internal/testsupport/wdpath` (`Advance`, E7 step; skipped on a scratch schema older than 0169);
  - `internal/withdrawal`: `f4ThenSubmit` / `f4AllowDispatch` (new `f4_dispatch_fixture_integration_test.go`) at the
    E7 call sites of `a5_migration_0162`, `a5_payout_reversal_db`, `adr0113_state_machine` (3), `adversarial`,
    `lockorder`, `replay_f7`, `withdrawal_integration` (2);
  - `internal/payments`: wrappers `f4ClaimCreatedForSubmission`, `f4ResubmitAmbiguous`, `f4MarkSubmittedPending` (the
    allow is recorded only for a PAYOUT attempt; a deposit attempt is untouched) in `pay_callback_mismatch_bind`,
    `payout_dispatch_round3`, `payout_dispatch_round4`, `payout_security_round` (2), `receipt_drain_merchant_ref`,
    `rvlf_i1_regression` (13 sites), `sweeper_loop`; direct recording in `payout_dispatch_integration_test.go` (T2
    race), `dep_ref_validate` (seed), `b11_payout_unbound_hold` (legacy E7), `hsec_hold_resolution_db` (2 control
    probes) and `hsec_hold_resolution_guard` (1);
  - `internal/payments/b13b_destination_integration_test.go`: the two hand-built `&Orchestrator{...}` literals that
    reach `ClaimForDispatch` carry `withdrawalGate: standard.New()` (NewOrchestrator's default; a nil gate refuses);
  - `internal/httpserver`: `a5_payout_reversal_http` (E7) and `iwire_webhook_reversal_link` (payout T2) via
    `f4RecordDispatchAllowHTTP`.
- Scratch down-chain tests (`a5_amount_dispute` 0163, `m4_resolve1` 0125, `m4_integrity` 0127, `hsec_hold_resolution_db`
  0124): before stepping a THROWAWAY scratch database from head below 0169, `f4ScratchClearGateDecisions` removes the
  decision rows as the scratch owner (append-only trigger and FORCE RLS lifted for that statement, restored in the same
  transaction; the 0124 test already clears launch history the same way). Otherwise 0169's own WD095 refusal stops the
  chain first. The refusal codes those tests pin (MR099 etc.) are unchanged.
- `internal/launchgov/closure_integration_test.go` `playerWithWallet`: activates the registered player (a withdrawal
  reaches `submitted` only for an active account, WD052; the A6 precedent in `closure_withdrawal_race`). Assertions
  unchanged.
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

## 5. Commands and counts (final run, after merging origin/claude/focused-wright-jw88w9 tip 9378c91)

Private scratch database only (`priv_db.sh` / `priv_test.sh`, database `f4<ts>p<pid>x01`, rebuilt at the merged head
including 0166 and 0170), `-count=1 -p 1`, synthetic data, MOCK providers.

| Command | Result |
|---|---|
| `gofmt -l .` | clean |
| `go vet ./...` and `go vet -tags integration ./...` | clean |
| `golangci-lint run ./...` (2.9.0) | 0 issues (the `-tags integration` run shows only pre-existing errcheck/staticcheck findings in older files; the new F4 files are clean) |
| `go test -count=1 ./...` | 75 packages ok, 0 FAIL |
| `-tags integration ./internal/withdrawal/ ./internal/withdrawalgate/... ./internal/rg/ ./internal/risk/ ./internal/db/ ./internal/launchgov/ ./cmd/...` | all ok (withdrawalgate has no test files) |
| `-tags integration ./internal/reconciliation/ ./internal/alerting/...` | all ok (check D and payment-statement reconciliation included; alert Kind parity with the database included) |
| `-tags integration -v -run 'F4\|Claim\|Payout\|Withdraw\|Sweeper\|Dispatch\|HSEC\|B13B\|M4_Migration\|A5\|T8\|R32\|RVLF\|K3_World\|DepRef\|Drain' ./internal/payments/` | ok: 450 top-level tests passed, 0 failed, 0 skipped (12 of them the new TestF4_*) |
| `-tags integration -run 'Withdraw\|Submit\|Route\|Golden\|OpenAPI\|Staff\|Financial\|HSEC\|Stage3C\|Stage3D\|Payout\|A5\|T8\|Launch\|StepUp\|Freeze\|IWire\|PaymentForce' ./internal/httpserver/` | ok |
| `-race -tags integration ./internal/withdrawal/` | ok, no race |
| `-race -tags integration -run 'TestF4\|TestSubmitWithdrawalHandler_F4' ./internal/payments/ ./internal/httpserver/` | ok, no race |

Before the merge, a wider payments sweep ran in two shards (`Payout|Withdraw|Claim|Sweeper|Dispatch|A5|T8|F4`: 264
tests; `K3_World|K3_[A-H]|HSEC|B13B|B11|M4|H8|RGEnforce|Kill|Resolution|Refbind|Echo|IWire|B12|B5_|B3_|B6|B8|A7|Lock|Escalat|RVLF|Receipt|Drain|DepRef|Poll`:
603 tests). Every failure they showed was an F4 fixture case listed in section 3 (fixed and re-run green). The full payments
suite was NOT run end to end (it does not finish in this environment, as recorded for A6); payments is therefore
`PARTIALLY VERIFIED` beyond the subsets above. `TestResolutionIsolation_FinancialDuringOutage` (httpserver, the known
load-sensitive INV-POOL 400 ms bound) failed once at load average 15+, passed alone and in the final subset run.

## 6. Mutation pass

Driver `docs/plans/f4-mutation-driver.py`, results `docs/plans/f4-mutation-results.tsv`. 34 hand-written single-site
mutants (21 Go: T1p claim, `dispatchgate.go`, the T2 / T12 helpers, the orchestrator default, the HTTP mapping; 13 SQL in
migration 0169), each run against the F4 suites of the affected package (`TestF4*` in withdrawal / payments,
`TestSubmitWithdrawalHandler_F4` in httpserver; SQL mutants rebuild a second private database). **33 KILLED, 1
SURVIVED.** Two mutants needed a corrected pattern on the first pass (a build error and a stale pattern after the
pre-lock refactor) and were re-run; their final verdicts are in the table. The pass ran on the branch before the final
merge with origin; the merge touched none of the mutated F4 files except by adding unrelated lines to `kind.go`.

Survivor, justified: `t1p_not_evaluated_guard_off` (the `if !gateEvaluated` refusal after L1 disabled). It is reachable
only when the request becomes `approved`, or its subject active, between the unlocked pre-read and the L1 lock of the
same claim; no test can schedule that without a new hook inside the claim transaction. With the guard removed the
unevaluated zero decision takes the denial branch and `RecordDispatchGateDecision` is refused by the
`withdrawal_gate_decisions_outcome_shape` CHECK (empty gate code), so the claim still rolls back and nothing is sent: the
mutant changes the error class (an internal 23514 instead of the retryable unavailable), not the outcome.

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

- `reservedUnusedVersions` / `TestReservedUnusedVersions_ExactSet`: this branch drops 169 from the set; F2 / F3 drop
  167 / 168 when they land (whoever merges second resolves the one-line conflict).
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

## 10. Labels summary

- Dispatch re-evaluation (T1p, T2, T12): `IMPLEMENTED`, verified as in section 5.
- Database backstop: `PARTIALLY IMPLEMENTED` (R1, R2).
- Alert raise: `IMPLEMENTED`; delivery `NOT IMPLEMENTED` (ALERT-DELIVERY-1).
- Payments suite: `PARTIALLY VERIFIED` (subsets, section 5).
- Not claimed: regulatory approval of the withdrawal policy; the policy choices (self-exclusion / time-out allowed,
  REVIEW refuses) are the A6 owner-reversible interpretations, applied unchanged at dispatch.
