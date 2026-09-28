# Security review — PRH-2 E2 (2026-09-28)

**Reviewer:** `security`. The orchestrator recorded this review.

**Scope:** commit `9a25453` (`e2-prov-outbound-cred-1-legacy`, based on `cabca27`), exported with `git archive`. Tests ran on a private DB (`sec_e2_rv_20260928`), dropped afterwards.

## Verdict: ACCEPT WITH CONDITIONS

The deletion is correct and complete: no production path creates a deposit or calls a provider outside
the gate. E2 can merge. **E2-C1 is required before PROV-OUTBOUND-CRED-1-LEGACY-PATH is treated as closed
against regression**, because the existing IO-1C guard provably does not detect the deleted bypass shape.

## Checks

1. **No ungated production path remains: verified.**
   - Every deleted symbol has zero code references.
   - The only non-test outbound adapter calls are `AdapterCall` closures consumed by `callProvider` (`gate.go:102`): `drive.go:251`, `payout.go:385,1174` and `sweeper.go:354`. `mock_statement_source.go:103` also uses the gate.
   - The only non-test `INSERT INTO deposit_intents` is `deposit_v2.go:125`.
   - Deposit postings go only through `postDepositSuccessOrDispute`.
   - No `reflect` or `plugin` import.
   - The simulation handler makes no outbound call.
2. **IO-1C still covers `internal/payments`: verified, with a blind spot.** It inspects only `*ast.FuncLit` closures with a `pgx.Tx` parameter. A planted named method `(ctx, tx pgx.Tx, provider PaymentProvider)` that calls `provider.Deposit` directly compiles, and **the guard still passes**. That is exactly the shape of the deleted `attemptDeposit`.
3. **The `legacyShape*` bridge is test-only: verified.** It lives in `_test.go` files with an `integration` build tag, which are never compiled into a binary.
4. **The deleted tests remove no needed security assertion: verified.**
   - `deposit.multiple_success_refused` was written only by the legacy path; it was already recorded as never wired.
   - The live path audits `payment.attempt_disputed` via `auditMultipleSuccessForIntent`, pinned by `TestINVDEP1_C3_…`, `…FL2_…` and `TestX5_LedgerBackstopMapping`.
   - The concurrency "exactly one provider `Deposit`" assertion was migrated, not dropped.
5. **ADR 0095 amendments:** accurate for deposit intents, with stale spots (F-3).

**Local runs (not CI):**
- build ok, and vet with both tag sets ok;
- `TestINV_IO_1c*` ok;
- `-tags integration` payments (532.8s) and reconciliation (67.3s) ok.

`-race` and httpserver were not run.

## Findings

| ID | Sev | Finding | Required change |
|---|---|---|---|
| **E2-C1** | **Medium (condition)** | There is no regression guard for PROV-OUTBOUND-CRED-1. IO-1C inspects only closure literals, and a planted named method that calls `provider.Deposit` passes it. Nothing statically forbids an outbound adapter call outside `callProvider`, and `Provider()`/`RouteProvider` return adapters. | Add a static test that allows non-test calls to `PaymentProvider`'s outbound methods (`Deposit`, `Withdraw`, `QueryStatus`, any future `Refund`) **only** inside an `AdapterCall` function literal that is returned by a named adapter builder (`depositAdapterCall`, `payoutAdapterCall`, `payoutStatusQuery`) or passed directly to `callProvider`. Scan all non-test packages. Include planted-violation tests for a named method with `pgx.Tx` and for a call from another package. Extend IO-1C to `ast.FuncDecl` with a `pgx.Tx` parameter, or document that the new guard subsumes it. |
| F-2 | Low | `Orchestrator.RouteProvider` is exported with no non-test caller, and it hands out an adapter outside the gated path. | Unexport or delete it, or cover it with E2-C1's allow-list. |
| F-3 | Low / docs | Stale text: ADR 0095 §28.3 ("Legacy `InitiateDeposit` … `deposit.multiple_success_refused`", around lines 5450 and 5671); comments in `deposit_handlers.go:94,102`, `payment_deposit_simulation_handlers.go`, `idempotency/doc.go:14` and `kyc/enforcement.go:16`. | Add "[deleted by E2]" notes and update the comments. |
| F-4 | Info | `InitiateDepositParams` survives only for tests and the bridge. | Harmless; it may go with F-2. |

**PROV-OUTBOUND-CRED-1-LEGACY-PATH:** the implementation is closable on merge. Record it as "closed; regression guard pending (E2-C1)"
until E2-C1 lands.

**Not covered:**
- the `-race` payments run and httpserver;
- the 18 mutants in the parity evidence, which were not re-run;
- a deeper audit of `mock_statement_source.go`'s gate inputs;
- real adapters, since none exist.
