# Security DELTA re-review: PRH-2 H at cd6f167 (delta from 03269da)

Source: security subagent final report (model output, not user input). Condensed record. Ran: build, vet (both tag sets) on payments/reconciliation/cmd, gofmt, `TestSweeperLoop_` -race (PASS, 83s), `TestPaymentStatement_LivePayoutPath_AgainstWiredMockSource_NoMismatches` (PASS, confirmed not skipped), `TestRun_StartsSweeper*` (PASS). Not run: full suites, mutants. Not a pen test / not a security declaration; MOCK adapters only.

## VERDICT: ACCEPT WITH CONDITIONS (no code change required; one doc registration C-1)

## Earlier pre-merge items
1 DONE (ADR 0095 §37.3 safeguards 1/4, cascade row, `sweeper_resolution_only.go` header, `sweeper.go:332-334` comment accurate). 2 DONE (§37.5 head-of-line H-SEC-2; §37.4 payout escape). 3 DONE ("never longer than this" removed; "best effort" wording). 4 MOSTLY DONE: H-SEC-4 registered in §37.5; H-SEC-5 only as §37.3 safeguard 5 without "registered follow-up / required before launch" wording (C-1).

## Delta security effects
Item context (`sweeper_loop.go:237-252`) OK: no goroutine in normal operation; after shutdown at most one per item, exits on itemDone; no timer leak (Go 1.26); tenant/RLS scope explicit tenantID; WithoutCancel keeps loop-context values only (no principal). Deposit phase C and poll apply on detached ctx bounded 5s (`drive.go:212-215`, `sweeper.go:407-410`) OK. `drive.go` hunk on HTTP path (cascade children only, `deposit_v2.go:346`) OK, net auditability improvement; no new goroutine; at most one DB connection held 5s more; no amplification. `payment.cascade_skipped_resolution_only` audit OK (same tx, WithTenant, ActorSystem, metadata deposit_intent_id only; test: 1 row non-active, 0 active). `testBeforeClaim` unexported, set only in integration-tagged tests, nil in production; `sweeperItemTimeout` var likewise. Claim-phase ctx handling OK (skipping claim leaves no lease; attempts stay due). nil CredResolver Error log OK. Last-pass gauge OK (info: advances when listing succeeds even if all claims fail; covered by `tenant_failures{phase=claim}`). RECON-PAYOUT-LIVE-TEST-1 confirmed test-only.

## Findings
- **H-SEC-9 LOW optional (register with H-SEC-2)** `sweeper_loop.go:237`: items now have no deadline in normal operation; provider calls still capped by gate `CallTimeout` (`gate.go:163-170`) but DB statements in item txs (claim, `deferIfResolutionOnly`, T2) are unbounded (no repo `statement_timeout`/`lock_timeout`; previously capped at 8s). An idle-in-transaction session or long row lock stalls the single sweeper loop indefinitely; gauge stops advancing (detectable, but alert delivery not wired); a deposit cascade chain can take N x 30s, worsening H-SEC-2. Fix before real-provider go-live: pool-level `statement_timeout`/`lock_timeout` or per-tx bound alongside H-SEC-2 pass budget.
- **H-SEC-10 INFO (LF domain, pre-existing)** `deposit_v2.go:308`: first HTTP deposit attempt's phase C still on request ctx; a player disconnect during `Deposit()` leaves `submitting` with no reference (F1 shape; reschedule without poll); deliberately triggerable; integrity/liveness only, no double credit. ADR §37.4 should note the hunk also changes the HTTP cascade path and that first-attempt HTTP phase C is not detached.
- **H-SEC-11 INFO** brand-level suspended/closed not honoured by resolution-only (tenant-scoped only); belongs with the H-SEC-5 launch item.

## Honesty
New residuals stated honestly (§37.5: H-SEC-2, H-SEC-4, closed-tenant funds OPEN decision, F4, deferral backoff); runbook §12 steps 5/7 say no supported release path, point to kill switch; MOCK/PROVIDER DEPENDENT labels kept; §37.6 calls RECON-PAYOUT-LIVE-TEST-1 MOCK evidence only; no alert-delivery claim; no I-wire dependency.

## Pre-merge
- **C-1 REQUIRED (doc only)**: register H-SEC-5 (+ H-SEC-11) as launch-blocking follow-up in ADR 0095 §37.3 safeguard 5 or §37.5 ("Registered follow-up; required before real-provider/launch: tenant-status (and brand-status) gate on HTTP deposit/withdrawal initiation") or in progress risks. Optional: add H-SEC-9 to the §37.5 H-SEC-2 bullet; H-SEC-10 sentence to §37.4.
## Before real-provider / payout go-live
H-SEC-1 code fix, H-SEC-2 + H-SEC-9, H-SEC-4, H-SEC-5 + H-SEC-11, ALERT-DELIVERY-1.
