# RV-PRH-I1 kill switch Phase 2 — Security review

- Reviewer: `security` specialist
- Date: 2026-09-27
- Subject: unmerged branch `worktree-agent-aa2bb3c6bdd6d51eb`, commits `ea7910a` (AM-1, RV2-L1/L2) and `d4520da` (phase 2 orchestrator wiring: kill-switch T3/hold, PROV-OUTBOUND-CRED-1 payments kind split), on top of `f4d7dce`. Reviewed in a detached worktree at `d4520da`.
- **Verdict: APPROVE WITH CONDITIONS. Mergeable now, independently of the PAY-DOUBLE-CREDIT-1 fix.**
  - No High or Medium finding.
  - Four Low items:
    - P2-L1: a gate binding test is missing.
    - P2-L2: the legacy ungated deposit path is still in the code.
    - P2-L3: `callProvider` has no nil-resolver guard.
    - P2-L4: the payout hold audit uses a cancellable context.
  - Two informational items (P2-I1, P2-I2).
  - None blocks merge. P2-L2 is a condition before any non-MOCK payments adapter is wired.

## Scope and method

In scope:
- `internal/payments`: `outbound_resolver.go`, `gate.go`, `contract.go`, and the phase-2 hunks in `deposit_v2.go`, `payout.go`, `drive.go`, `sweeper.go` and `payout_sweep.go`.
- `internal/httpserver`: `payments_kill_switch_handlers.go` and `withdrawal_handlers.go`.
- `cmd/platform-api`: `registrations.go`, `wiring.go`, and the reflection and wiring tests.
- `internal/providercred/outbound.go`, read only to confirm the real resolver's tenant scoping and redaction.

Out of scope:
- The PAY-DOUBLE-CREDIT-1 fix.
- The ledger posting paths.
- A real PSP adapter, because none exists.
- Penetration testing.

Method:
- **Database:** the private DB harness (`priv_db.sh`, `PRIV_DB=secrv_ks_p2`, `PRIV_SRC=<worktree>`), migrated to 106.
  - I also applied the `deploy/init-app-role.sql` grant tail, which is table GRANT/REVOKE only; no roles or credentials were changed.
  - The DB was dropped and the worktree removed afterwards.
- **Mutation run:** a green baseline first:
  - the full `internal/payments` integration suite;
  - `internal/httpserver` tests matching `KillSwitch|Deposit|Withdraw|Payout|Credential`;
  - `cmd/platform-api`.

  Then 11 targeted mutants (below). Nothing from the probes is committed.

## Answers to the security focus questions

### Can the kind split choose MOCK credentials for a real adapter, or real credentials for a mock?

No, not through any configuration or tenant input.

How the resolver is chosen:
- `OutboundKindSplitResolver` picks the resolver by the **adapter's own code-level marker**, `SyntheticComponent()`. It reads it once, from the same adapter map the orchestrator was built with.
- Nothing tenant-editable or request-derived enters the choice.
- A provider id missing from that map fails closed with `ErrOutboundCredentialUnavailable`, which the gate turns into `NotSent`.
  - Mutant P3 (fail open on an unregistered provider) is killed by `TestOutboundKindSplitResolver_UnregisteredProviderFailsClosed_NoAdapterCall`.
  - Mutant P2 (invert the mock/real choice) is killed by `TestPaymentsOutboundResolver_FollowsWiring`.

What production wires:
- The MOCK resolver is wired only when `wiring.PaymentsOutboundResolver`, which is test support, is on.
- The MOCK resolver is registered with `providerkind` (`outbound_resolver` component), so the synthetic-in-production guard refuses it at boot.
- With test support off, a synthetic adapter gets a nil mock slot, so every call is `NotSent`. It never falls back to the real resolver.

Residual risk:
- The split relies on the marker being accurate.
  - A **real** adapter that embedded a MOCK type would inherit the marker and be offered MOCK credentials. It would then fail authentication at the vendor, and the production guard would refuse it at boot.
  - A decorator wrapping the MOCK without forwarding the marker would be offered **real** credentials in-process. That needs an active real handle for a MOCK provider id, and the MOCK never transmits.
- Both are acceptable. Any new adapter is reviewed individually anyway, because the Synthetic tripwire stays in place.

### Can any path call a provider without resolved credentials?

**Not on any production path.**

Every production call site goes through `callProvider`:
- deposit T1+T2 in `deposit_v2.go`;
- the cascade driver in `drive.go`;
- `DispatchWithdraw`;
- `PollPayoutStatus`;
- the sweeper's `QueryStatus`;
- the MOCK statement fetch.

`callProvider` does the following before invoking the adapter:
- resolves the credential outside any transaction (`txscope.Held` is refused);
- maps **every** resolver error, including outage, expiry, revocation, store failure and a context timeout, to `ErrorClassNotSent`;
- re-checks the tenant, provider and domain binding.

Evidence:
- `TestCallProvider_CredentialResolutionOutage_MapsToNotSent_NeverCallsAdapter` and `TestInitiateDepositAttempt_CredentialBindingMismatch_T5_NoCall` kill P5b.
- P5b removes both the error check and the binding check. Removing only one of them (P5) survives, because the other still stops the call. That is intended layering.

**Exception, not production-reachable:** see P2-L2. The legacy `Orchestrator.InitiateDeposit` → `attemptDeposit` / `handleDecline` / `resolveAmbiguous` path in `orchestrator.go`:
- calls `provider.Deposit` and `provider.QueryStatus` directly;
- has no gate, no credential and no kill-switch predicate;
- runs inside a transaction.

A grep of `d4520da` shows no non-test caller. It is still exported.

### Do credentials leak into logs, audit or errors?

No leak path found.

- `providercred.OutboundCredential` holds the secret in an unexported `secretstore.Secret`. Its `String`, `GoString`, `Format`, `LogValue` and `MarshalJSON` render only tenant, domain, provider, handle, key id and fingerprint.
- `CallContext.String` and `MarshalJSON` embed only that redacted form.
- Resolver failures return a bare sentinel. The gate passes it through `redactedReason`, so no store text reaches `GateResult.Err`.
- The kill-switch denied-audit and payout-hold audit rows carry no credential material:
  - denied audit: `actor_scope`, `target_tenant_id`, `denied_class`;
  - payout hold: `provider_id`, `reason_code`, `denied_by_kill_switch`.

Reflection coverage, in `cmd/platform-api/credential_reflection_test.go`:
- It now scans `b.paymentsOutboundCredentials()` and the kind-split resolver.
- In the test bundle `b.Credentials` is nil, so only the MOCK half is actually scanned; see P2-I2.

### Is the kill-switch decline reason visible to players?

No.

Deposit T1+T2:
- `ErrKillSwitchEngaged` finalizes the intent as `declined` with reason `kill_switch` in the same phase-A transaction. It then returns before phase B, because `attemptCreated` is false, so no provider call is made.
- The player-facing `depositIntentResponse` has no reason field. Both the create and list responses expose only `status`.
- `kill_switch` is recorded in the operator audit metadata only.

Tests:
- Mutant P1 (drop the branch, so the error surfaces as a 500) is killed by `TestInitiateDepositAttempt_KillSwitchEngaged_DeclinesCleanly_T3`.
- Mutant P1b (wrong reason label) is killed by the same test.

Payouts:
- `ErrPayoutKillSwitchEngaged` leaves the withdrawal `approved`.
- A separately committed denied audit row labels the hold. Mutant P6 (drop it) is killed by `TestClaimForDispatch_KillSwitchEngaged_FailsClosedNoWithdraw`.
- The route is staff-only.

### Tenant scoping of credential resolution

- The gate passes `in.TenantID`, which comes from the committed attempt row, never from the request.
- The real `providercred.OutboundResolver.Resolve` reads handles under `pool.WithTenant(ctx, tenantID)` with an explicit tenant predicate (`readHandles(…, tenantID, domain, providerID, outbound_api)`). Any other than exactly one active handle fails closed.
- The gate then re-checks `cred.TenantID == in.TenantID`. See P2-L1: that check is untested.

AM-1:
- `runKillSwitchTx` now scopes the tenant session to the authenticated `tc.TenantID`, not the path value.
- Mutant P9 (revert AM-1) is killed by `TestRunKillSwitchTx_UsesAuthenticatedTenantNeverThePathValue`.
- For tenant callers `canActOnTenant` already forced the two values to be equal, so this is correct defence in depth.

### RV2-L1 / RV2-L2 (re-verification 2 residuals) — CLOSED

- **RV2-L1:** both denied-audit writers now run on `context.WithTimeout(context.WithoutCancel(ctx), 5s)`. These are the foreign-tenant `recordKillSwitchDenied` and `recordKillSwitchRefusalAudit`. Mutant P7 (revert to the cancellable context) is killed by two `…WritesDespiteCancelledRequestContext` tests.
- **RV2-L2:** the IP address and user agent are captured at `beginKillSwitchCall` and written on both denied rows. Mutant P8 (drop them) is killed.

## Findings

### P2-L1 — LOW: the gate's tenant-binding check has no test

Mutant P4 removes `cred.TenantID != in.TenantID` from the gate's step-4 binding check. It **survives** the payments, httpserver and `cmd/platform-api` suites.

Why it is only Low:
- Today it is not exploitable. The real resolver sets `TenantID` from its tenant-scoped argument, and the MOCK resolver echoes it.
- But this check is the gate's independent layer for exactly the property the coordinator asked about. A future resolver, such as a caching or derived-token wrapper, that returned another tenant's credential would not be caught by any test.

Fix: add a gate unit test with a stub resolver that returns a correctly shaped credential for a **different** tenant. Assert `NotSent` and that the adapter is never invoked.

### P2-L2 — LOW (a condition before the first non-MOCK payments adapter): the legacy ungated deposit path is still exported

`Orchestrator.InitiateDeposit` → `attemptDeposit` → `handleDecline` / `resolveAmbiguous`, in `orchestrator.go` around lines 546–770:
- makes provider calls **inside the caller's transaction**;
- has no credential resolution, no kill-switch predicate and no gate redaction.

It is pre-existing and not introduced by this branch. Only tests call it, but it is an exported, compilable bypass of every ADR 0095 §3.2/§10.3/§11 control.

Fix: delete it and migrate its remaining tests to `InitiateDepositAttempt`, or move it behind a test-only build tag. This must happen before `TestOutboundPrecondition_EveryWiredAdapterIsSynthetic` is relaxed for payments.

### P2-L3 — LOW: `callProvider` has no nil-resolver guard

`NewOutboundKindSplitResolver` returns a true nil interface when neither half is wired. `callProvider` then calls `resolver.Resolve` outside `safeCall`, which **panics** instead of returning `NotSent`.

Current state:
- Every production caller checks for nil first: the deposit handler, the withdrawal submit and poll handlers, and the MOCK statement source.
- The `Sweeper` is not wired in `main` yet.

Impact: fail-closed, but a crash rather than a refusal.

Fix: add `if resolver == nil { return NotSent }` as the gate's first step, with a test.

### P2-L4 — LOW: the payout kill-switch hold audit runs on the request context

`recordPayoutKillSwitchHoldAudit` uses `pool.WithTenant(ctx, …)` with the HTTP request context and discards any error. This is the same class as RV2-L1, which this branch fixed for the kill-switch handlers: a client disconnect can drop the labelled-hold row.

Fix: reuse the `WithoutCancel` plus bounded-timeout pattern and log a failure, as `recordKillSwitchRefusalAudit` does. Required before the stage gate.

### P2-I1 — Info: no separate credential-resolve budget

ADR 0095 §11 recommends a resolve budget (`RECOMMENDATION` 2 s) separate from the provider call timeout. The gate passes the caller's context straight to `Resolve`.

- Any timeout or error still maps to `NotSent`, so the claim "outage/timeout → NotSent, adapter never invoked" holds.
- A slow store, however, consumes the request's own deadline. It is bounded only by the Fetcher's own limits (ADR 0094).

Record this as deferred or implement it. It does not block merge.

### P2-I2 — Info: the reflection scan of the real resolver half is vacuous in tests

`buildProviderBundle` has no `Credentials` subsystem in the test configs. `paymentsOutboundCredentials()` therefore contains only the MOCK half, and the new "scan the real kind-split" entry cannot see the real `*providercred.OutboundResolver`. That resolver legitimately reaches the Fetcher's secret cache (ADR 0094), so it must not simply be added to the scan either.

This is consistent with the M5 PARTIAL label. Record the real-half exemption explicitly in the test so it is not later cited as coverage.

## Mutation summary (11 targeted mutants)

| Mutant | Result | Killed by |
|---|---|---|
| P1: deposit kill-switch branch removed | KILLED | `TestInitiateDepositAttempt_KillSwitchEngaged_DeclinesCleanly_T3` |
| P1b: wrong decline label | KILLED | same |
| P2: kind split inverted | KILLED | `TestPaymentsOutboundResolver_FollowsWiring` |
| P3: unregistered provider fails open | KILLED | `…UnregisteredProviderFailsClosed_NoAdapterCall` |
| P4: gate tenant-binding clause removed | **SURVIVED** | see P2-L1 |
| P5: resolve error ignored (binding check still present) | SURVIVED | Layered; P5b kills |
| P5b: resolve error ignored and binding check removed | KILLED | three tests |
| P6: payout hold audit removed | KILLED | `TestClaimForDispatch_KillSwitchEngaged_FailsClosedNoWithdraw` |
| P7: denied-audit context cancellable | KILLED | two cancelled-context tests |
| P8: denied audit missing IP/UA | KILLED | |
| P9: AM-1 reverted | KILLED | `TestRunKillSwitchTx_UsesAuthenticatedTenantNeverThePathValue` |

## Overlap with the deposit/callback code and the double-credit fix (merge ruling)

What this branch changes in the named files:

| File | Change |
|---|---|
| `drive.go` | `callProvider(..., pool, ...)` argument, line 161 |
| `sweeper.go` | same, line 316 |
| `payout_sweep.go` | same, line 346 |
| `deposit_v2.go` | the argument change, plus the phase-A `ErrKillSwitchEngaged` branch that finalizes a **decline** before any attempt row, provider call or posting exists |
| `payout.go` | the argument change, plus the hold-audit call on the payout claim refusal |
| `gate.go` | the resolver signature and pool threading |

It does **not** touch any of the following:
- the success or posting paths: `applyDepositCallResult`, `postDepositSuccess`, `ApplySuccess`, `receipt.go`, `cascade.go`;
- the ledger;
- any code that can credit a wallet.

A kill-switch decline posts nothing.

**Ruling: this branch can merge independently of, and in either order with, the PAY-DOUBLE-CREDIT-1 fix, without touching the double-credit invariant.** Expected textual conflicts are limited to the single `callProvider` lines in `drive.go`, `sweeper.go` and `deposit_v2.go`, if the fix edits those hunks. Conditions on whichever branch merges second:
1. Any `callProvider` or `DispatchWithdraw` call site the double-credit fix adds or moves must pass `pool` and the kind-split resolver. It must never pass a nil resolver (P2-L3) or bypass the gate.
2. The fix must not route a deposit success through the legacy `InitiateDeposit`/`attemptDeposit` path (P2-L2).
3. Re-run the full `internal/payments` suite and the kill-switch predicate coverage tests after the merge.

For the record, ledger-finance H3 (the cascade refusal blocking callbacks and polls) is already addressed on the base at all three sites via `insertCascadeAttemptIfEligible` (`cascade.go`, used by `receipt.go`, `drive.go` and `sweeper.go`). This review did not re-verify it beyond confirming the call sites. `ledger-finance` owns that sign-off.

## Status and conditions

| Item | Status |
|---|---|
| Merge of `ea7910a` + `d4520da` | Approved (independent of PAY-DOUBLE-CREDIT-1) |
| P2-L1, P2-L3, P2-L4 | Before the stage gate |
| P2-L2 | Before any non-MOCK payments adapter is wired or the Synthetic tripwire is relaxed |
| Earlier open items, unchanged | Architect amendment for the route deviation; KS-AUDIT-TENANT-1; alert delivery; raw-string secret review per real adapter |

Scope note: this review covers the Phase 2 diff listed above. It is not a general declaration that the payments subsystem is secure, and it is not a sign-off on the PAY-DOUBLE-CREDIT-1 fix.
