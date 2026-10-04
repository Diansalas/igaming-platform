# Security review: PRH-2 H (payments sweeper process), commit 03269da

Source: security subagent final report (model output, not user input). Condensed record; verdict and findings preserved.
Scope: code/design review of the MOCK-adapter path at 03269da; `go vet` only; integration suites and mutants not re-run (relied on author's evidence file). Not a penetration test; not a security declaration.

## VERDICT: ACCEPT WITH CONDITIONS (no code change required pre-merge; 4 doc corrections/registrations required pre-merge)

## Findings
- **H-SEC-1 (LOW-MEDIUM)**: deposit-dispatch tenant-status read is NOT in the claim transaction (`deferIfResolutionOnly`, `sweeper.go:330-333`, own tx; T2 claim `ClaimCreatedForSubmission` in `drive.go` never reads status; drive.go:637 phase-C cascade insert ungated). Scenario: suspension commits between the status read and the claim -> a real Deposit goes out; a cascade child row can be inserted (never driven). ADR 0095 §37.3 safeguards 1 and 4, the cascade table row, and the header of `sweeper_resolution_only.go` wrongly say "in-transaction" for deposit dispatch. RULING: acceptable residual for MOCK-only merge once docs are corrected; recommended now and REQUIRED before the real deposit path goes live: run `tenantResolutionOnly` inside `driveCreatedAttempt`'s T2 claim tx when sweeperDriven and gate the drive.go:637 cascade insert.
- **H-SEC-2 (MEDIUM, launch-relevant)**: cross-tenant head-of-line blocking; payout `DispatchWithdraw`/`PollPayoutStatus`/phase C re-detach with `WithoutCancel` (60s/5s), escaping the 8s `SweeperItemTimeout`; pass is sequential across tenants (batch 20, manifest CallTimeout 30s) so a hanging tenant A can block a pass up to ~20 min and the 60s batch lease can expire mid-batch (money-safe via claim CAS, duplicates work). Pre-merge: disclose in ADR §37.5. Before real-provider/payout go-live: per-tenant pass budget or §7.3 concurrency caps.
- **H-SEC-3 (LOW)**: shutdown drain bound overstated ("never longer than this" in `sweeper_loop.go:48-54` and ADR §37.4); payout item can run 30-60s so main may log "did not stop" with a Withdraw in flight (money-safe: stays submitting -> T6 ambiguous -> poll, resend only under idempotent manifest). Fix wording. Note for LF: 8s item context also caps deposit Deposit below DefaultCallTimeout (30s) and spans the whole cascade chain -> systematically more ambiguous deposits with a slow PSP.
- **H-SEC-4 (LOW-MEDIUM)**: unbounded NotSent churn: NotSent returns attempt to `created` with fixed 30s next action, no counter/escalation (`drive.go:511`, payout `MarkNotSent`, `payout.go:55`); each payout T2 re-claim writes a `kyc_enforcement_decisions` row + audit (~2x2,880 append-only rows/day/attempt) on persistent credential failure. Newly activated by H. Track now; fix (backoff/escalate after N) before payout go-live. Ruling: nil CredResolver at startup ACCEPTED (fails closed as NotSent, matches HTTP path; optional log at Error).
- **H-SEC-5 (INFO, launch-relevant)**: resolution-only applies to sweeper only; no `tenants.status` read on HTTP deposit/withdrawal initiation paths (deposit_v2 incl. cascade `deposit_v2.go:346`, `ClaimForDispatch`). Not introduced by H. No doc may describe suspension as stopping payments; register launch item (security/architect).
- **H-SEC-6 (INFO)**: `RescheduleNonTerminal` increments poll_count so a withheld attempt waits up to the 30-min backoff cap after reactivation; non-interactive created deposits of a `closed` tenant never terminate.
- **H-SEC-7 (INFO)**: advisory lock acceptable (int4,int4 `hashtext` keyspace distinct from other subsystems; collision only skips a tenant for one pass; H-ADV survivor legitimate). Optional: `hashtextextended`.
- **H-SEC-8 (INFO, pre-existing)**: `gate.go` `safeCall` puts adapter panic value into `GateResult.Err` via `%v`; register for a future gate pass.

## Rulings on author judgement calls
1. T12 payout resend withheld for non-active tenants: CONFIRMED (Withdraw is a money-moving call even with the same idempotency key; the preceding poll still runs; consequence: idempotent-manifest ambiguous payout of a suspended tenant stays ambiguous, funds held, until poll resolves or reactivation; mention in runbook §12 step 5).
2. T3 expiry of interactive created deposit for non-active tenants: CONFIRMED allowed (no provider call).
3. Status read in separate short tx before driveCreatedAttempt: ACCEPTABLE residual for this merge, docs must be corrected (H-SEC-1).
6. Recovered panic leaves lease until expiry: CONFIRMED.

## Verified (no finding)
Tenant isolation (WithTenant + WHERE tenant_id, per-attempt-tenant credentials with binding re-check, ids-only tenant list, missing tenant fails closed, no status caching); resolution-only covers every Deposit/Withdraw site reachable from sweeper (only `drive.go:292`, `payout.go:438`); kill switch unchanged; synthetic guard and DB connect precede sweeper construction (`main.go` 69/106 < 464); PayoutKYCGate non-nil pinned and refused at startup; no tx across provider calls; detached shutdown context carries no principal/txscope marker, RLS scope from explicit tenantID; panic values dropped; fixed low-cardinality metric labels; logs carry ids + redacted errors; no dependency on I-wire/ALERT-DELIVERY-1 and no claim of alert delivery.

## Pre-merge items (all documentation)
1. Correct ADR 0095 §37.3 safeguards 1 and 4, the cascade row, and `sweeper_resolution_only.go` header (deposit dispatch is a separate pre-claim tx; drive.go phase-C cascade insert not gated, child inserted but never driven).
2. ADR §37.5: name cross-tenant head-of-line effect and payout context escape from `SweeperItemTimeout`.
3. Correct "never longer than this" drain wording in `sweeper_loop.go` comment and ADR §37.4.
4. Register H-SEC-4 and H-SEC-5 as tracked items.

## Must close before real-provider / payout go-live
H-SEC-2 (pass budget or concurrency caps); H-SEC-4 (bounded NotSent retry/escalation); H-SEC-5 (tenant-status gate on HTTP deposit/payout initiation); H-SEC-1 code fix (status read in claim tx); ALERT-DELIVERY-1 (open, outside H).
