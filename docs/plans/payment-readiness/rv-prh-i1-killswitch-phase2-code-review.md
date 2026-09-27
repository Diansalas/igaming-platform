# RV-PRH-I1: independent code review of kill-switch phase 2 (orchestrator wiring and PROV-OUTBOUND-CRED-1 payments kind split)

- Reviewer: `code-reviewer` (independent of the implementer)
- Branch: `worktree-agent-aa2bb3c6bdd6d51eb` (not merged)
- Commits reviewed: `ea7910a` (AM-1, RV2-L1/L2) and `d4520da` (deposit T3, payout hold audit, PROV-OUTBOUND-CRED-1), on top of `f4d7dce`
- Method: I read the full diff in a detached worktree at `d4520da`. I ran the targeted suites on a private scratch DB
  (`internal/payments -tags=integration` for the kill-switch, dispatch, deposit, sweeper and credential tests;
  `internal/httpserver -tags=integration -run KillSwitch`; and `cmd/platform-api`). All were green. `go build`, `go vet` and
  `gofmt` were clean. I ran 20 mutants of my own (below) and trial-merged the branch onto `claude/focused-wright-jw88w9` @ `17e5ffc`.
  The DB was dropped and the worktrees were removed afterwards. No role, password or privilege changes were made.
- Date: 2026-09-27

## Verdict

**APPROVE WITH CONDITIONS.** No correctness bug in money movement was found. The AM-1 change and the
RV2-L1/L2 changes are correct, and their tests kill their mutants. The deposit T3 decline is correct: the zero-row
`INSERT … SELECT` means only "switch engaged", because an RLS-invisible context raises a WITH CHECK error instead of
inserting zero rows. The decline runs in the same transaction, makes no provider call, and never touches a posting path.
The payout hold audit is correct in intent.

The payments kind-split resolver, however, is **not tested the way the commit claims** ("mirroring casino/KYC
exactly"). Its most important property survives mutation. The pool-threading claim is also untested. C1 and C2 must
be closed before PROV-OUTBOUND-CRED-1 (payments) is labelled `IMPLEMENTED`. C3 is a small consistency fix. None of this
blocks the merge ordering relative to PAY-DOUBLE-CREDIT-1 (see the last section).

## Findings (most severe first)

### C1 (MEDIUM, test gap on a credential-isolation property): payments `OutboundKindSplitResolver` has no unit tests; the "real adapter never gets the MOCK credential" property is unpinned

`internal/payments/outbound_resolver.go` is new. Casino and KYC each ship five kind-split tests
(`internal/casino/outbound_kindsplit_test.go`, `internal/kyc/outbound_kindsplit_test.go`): SyntheticUsesMockOnly,
NonSyntheticUsesRealOnly, UnregisteredFailsClosed, BothNilTrueNil and SyntheticWithNilMockFailsClosed. Payments ships
only UnregisteredFailsClosed. That test builds the split with `MockCredentialResolver{}` as **both** mock and real, so it
cannot tell the two apart. The `cmd/platform-api/wiring_test.go` comment states that "the kind split's own
routing-by-adapter-identity logic is unit-tested directly in internal/payments". That statement is false.

Surviving mutants:
- **M1b**: `target := s.mock` for every adapter survives. Failure scenario: a non-production deployment with test
  support on, where a real sandbox PSP adapter is registered next to `mock-payments`. Every call to the real PSP would
  carry the synthetic `NewMockOutboundCredential` instead of the tenant's providercred handle. That is exactly the
  "chosen by whether any mock is wired" behaviour the split exists to prevent. No test fails.
- **M3**: deleting the `target == nil` guard survives. The guard is reachable. It fires when the real `Credentials`
  subsystem is configured, `TEST_SUPPORT_ENDPOINTS_ENABLED=false`, and the synthetic `mock-payments` adapter is routed.
  Without the guard, `Resolve` on a nil interface panics inside `callProvider` step 3, which is outside `safeCall`'s
  recover. On the deposit path, the T1+T2 `submitting` attempt has already committed by then, so the panic strands it.
  The guard is present today. The finding is that nothing pins it.

M2b (always real) and M20 (inverted synthetic detection) are killed, but only by
`cmd/platform-api/TestPaymentsOutboundResolver_FollowsWiring`, not by the package's own tests.

Required: port the casino/KYC kind-split test file to `internal/payments`, using distinct recording fakes for mock and
real. Correct the wiring_test comment.

### C2 (MEDIUM, claimed but untested): pool threading to the credential resolver is not exercised by any test

The commit says pool is "threaded through every call site". Every payments test uses `MockCredentialResolver`, which
ignores pool, or a fake that also ignores it. Surviving mutants:
- **M5**: `callProvider` passes `nil` instead of `pool` to `resolver.Resolve`.
- **M6**: `DispatchWithdraw` passes `nil` to `callProvider`.

Failure scenario: `providercred.OutboundResolver.Resolve` returns `ErrOutboundCredentialUnavailable` when
`pool == nil`. With either mutant, every deposit and payout against a real adapter becomes NotSent (T5) forever,
and no test notices. This fails closed, so no money moves, but it silently breaks the real path. That real path is
the whole deliverable of PROV-OUTBOUND-CRED-1.

Required: add a recording resolver that asserts it received the caller's non-nil pool. Drive it through each call
site: `InitiateDepositAttempt`, `driveCreatedAttempt` (cascade), `DispatchWithdraw`, `PollPayoutStatus` and
`Sweeper.processViaQueryStatus`. Also document or pin the one deliberate `nil`:
`mock_statement_source.go` is constructed with a hard-wired `MockCredentialResolver{}` in `registrations.go:188`.
If anyone later wires that source with the kind-split resolver and a real adapter, it would fail closed with no signal.

### C3 (LOW): `recordPayoutKillSwitchHoldAudit` repeats the RV2-L1 defect that `ea7910a` just fixed, and swallows errors without logging

`payout.go`: the helper writes under the caller's `ctx` (the staff request's `r.Context()`). Its doc comment says it
"mirrors recordKillSwitchRefusalAudit's identical pattern", but that function is now detached with
`deniedAuditCtx`. The error is also discarded with `_ =` and no log line.

Failure scenario: the staff client's request is cancelled (a disconnect or a proxy timeout) just after T1p is refused.
The hold label is then silently not written, and nothing is logged. This is the same audit gap that RV2-L1 closes for the
kill-switch routes.

In addition:
- **M9** (drop `provider_id` from the metadata) survives.
- **M10** (drop IP/UA) survives.

The test asserts only `denied_by_kill_switch` and `reason_code`.

Required: use `context.WithTimeout(context.WithoutCancel(ctx), …)`, log a failure the way
`payments_kill_switch_denied_audit_failed` does, and extend the assertion to `provider_id` and `ip_address`/`user_agent`.

### C4 (LOW, behaviour change beyond the stated scope): `paymentsOutboundCredentials()` no longer returns the MOCK resolver unconditionally

Before this branch, `b.Payments` was always non-nil, so the function always returned `MockCredentialResolver{}`. Now the
mock half is wired only when `TestSupportRoutesEnabled()` is true. `TEST_SUPPORT_ENDPOINTS_ENABLED` defaults to false.

In a non-production deployment with test support off:
- **(a) `Credentials` subsystem not configured.** The resolver is nil. Deposits return 503 ("deposits are not
  enabled"), and withdrawal submit and poll return 503 (`withdrawal_handlers.go:836/1079`).
- **(b) `Credentials` subsystem configured.** The resolver is non-nil, but every call against `mock-payments` is
  NotSent. For a deposit, the intent and attempt commit, the attempt reverts to `created`, and the intent stays
  `pending`. For a payout, T1p commits `approved→submitted` (the hold stays) and the attempt parks in `created`.
  `cmd/platform-api` constructs no `Sweeper`, so these rows are never retried.

This is consistent with the casino and KYC split, and staging sets `test_support_endpoints_enabled = true`, so no
current environment is affected. The commit message does not state this consequence, though. The orchestrator should
either acknowledge it or add a boot-time refusal for case (b): a synthetic payments adapter with no mock resolver wired.

### C5 (LOW, spec deviation and observability): deposit kill-switch decline

- ADR 0095 §10.3 says a kill-switched deposit makes the player see "unavailable". The implementation returns
  `201` with `status="declined"`, the same as an RG or KYC decline. A player cannot tell a paused provider from a
  refusal. Either amend the ADR or map the response.
- `finalizeDeclined(…, nil, nil, "kill_switch")` records no `provider_id`. The `deposit.declined` row therefore does
  not show which provider's switch fired. Wildcard and provider-scoped switches both exist, so operators need this.
  `capability.ProviderID` is in scope and could be put in the metadata without setting the intent's provider column.
- Semantics note (not a defect): the refusal is now terminal for that idempotency key. Before, the 500 rolled back
  the whole transaction, so a retry with the same key succeeded once the switch was released. Now the retry returns
  the declined intent. This matches ADR T3. The player client must mint a new key.

### Informational (no action required)

- **AM-1**: `auditTenantID()` and every `payments.*(…, c.target, …)` call still take the path value. If
  `canActOnTenant` ever regressed, RLS WITH CHECK would reject those writes under the authenticated GUC. That fails
  closed, so AM-1 is sufficient as defence in depth.
- **M14**: the `WithPlatformAdmin` branch of `recordKillSwitchDenied` survives because it is unreachable.
  `canActOnTenant` always admits `TenantID == uuid.Nil`, so no platform-scoped caller is ever denied as a foreign
  tenant. The branch is dead code. Removing it or leaving it is harmless.
- Credential resolution in `callProvider` runs before the step-5 deadline. On the deposit path it is therefore bounded
  only by `r.Context()`. Casino and KYC behave the same way. Payouts are bounded by `payoutOutboundCallBound`.

## Mutation results (my own runs)

| # | Mutant | Result | Killed by |
|---|---|---|---|
| M1b | kind split: `target := s.mock` for every adapter | **SURVIVED** | (C1) |
| M2b | kind split: `target := s.real` for every adapter | killed | cmd `TestPaymentsOutboundResolver_FollowsWiring` only |
| M3 | kind split: drop the `target == nil` guard | **SURVIVED** | (C1) |
| M4b | kind split: drop the unregistered guard | killed | `TestOutboundKindSplitResolver_UnregisteredProviderFailsClosed_NoAdapterCall` |
| M20 | kind split: invert synthetic detection | killed | cmd wiring test only |
| M5 | gate passes a nil pool to `Resolve` | **SURVIVED** | (C2) |
| M6 | `DispatchWithdraw` passes a nil pool | **SURVIVED** | (C2) |
| M7 | deposit: drop the kill-switch T3 branch | killed | `TestInitiateDepositAttempt_KillSwitchEngaged_DeclinesCleanly_T3` |
| M19 | deposit: wrong decline reason | killed | same |
| M8b | payout: skip the hold audit | killed | `TestClaimForDispatch_KillSwitchEngaged_FailsClosedNoWithdraw` |
| M9 | payout hold audit: drop `provider_id` | **SURVIVED** | (C3) |
| M10 | payout hold audit: drop IP/UA | **SURVIVED** | (C3) |
| M11 | `runKillSwitchTx` back to `c.target` | killed | `TestRunKillSwitchTx_UsesAuthenticatedTenantNeverThePathValue` |
| M12 | `deniedAuditCtx` without `WithoutCancel` | killed | `TestRecordKillSwitch*_WritesDespiteCancelledRequestContext` |
| M13 | foreign-tenant denied writer uses `ctx` | killed | same |
| M14 | same, platform-admin branch | survived | unreachable branch (informational) |
| M15b | refusal writer uses `ctx` | killed | `TestRecordKillSwitchRefusalAudit_WritesDespiteCancelledRequestContext` |
| M16 | refusal row drops IP/UA | killed | same |
| M17 | `beginKillSwitchCall` drops IP/UA | killed | approve-release L2 assertion |
| M18 | wiring: mock regardless of the flag | killed | cmd wiring test |

The implementer's own "mutation-killed" claims (deposit T3, hold audit, AM-1, L1 for both writers) are confirmed.

## Merge dependency on PAY-DOUBLE-CREDIT-1 (postDepositSuccess callers)

The fix will change `postDepositSuccess` (`orchestrator.go:841`) and its callers: `receipt.go` T7/T13,
`drive.go:268` (`applyDepositCallResult`), `sweeper.go:381` (`applyStatusEvidence`) and legacy `orchestrator.go:757`.

The branch's hunks in those files, listed exactly:
- `internal/payments/receipt.go`: **none**.
- `internal/payments/orchestrator.go`: **none**.
- `internal/payments/drive.go`: one hunk, `@@ -158,7 +158,7 @@`, in `driveCreatedAttempt` phase B. It adds the `pool`
  argument to `callProvider`. It does not touch `applyDepositCallResult`, which is the function containing line 268.
- `internal/payments/sweeper.go`: one hunk, `@@ -313,7 +313,7 @@`, in `processViaQueryStatus`. It adds the `pool`
  argument to `callProvider`. It does not touch `applyStatusEvidence`, which is the function containing line 381.
- `internal/payments/deposit_v2.go`: two hunks. The first is the kill-switch branch at T1+T2 (`@@ -237,6 +238,22 @@`),
  which runs before any provider call and has no posting. The second is the phase-B `callProvider` pool argument
  (`@@ -256,7 +273,7 @@`). Phase C (`applyDepositCallResult`) is unchanged.
- Indirect: `callProvider`, `DispatchWithdraw` and `OutboundCredentialResolver.Resolve` change signature
  (`gate.go`, `payout.go`, `contract.go`), and the local `payments.OutboundCredential` type is removed.

Assessment: **merging this branch first does not create an unsafe dependency.** No hunk changes any success, credit,
decline-with-posting or cascade path. The deposit kill-switch T3 decline happens before any attempt exists, so no
success evidence can later arrive for it. A trial merge onto `17e5ffc` is conflict-free, and on the merged tree `go
build`, `go vet`, `internal/txscope`'s INV-IO-1(c) static scan and `cmd/platform-api` all pass.

The only coupling is mechanical. Code written for the fix against the old signatures will not compile after this
merge: direct `callProvider(ctx, resolver, …)` or `DispatchWithdraw(ctx, resolver, …)` calls, a custom
`Resolve(cc CallContext, domain string)` fake, or a use of `payments.OutboundCredential`. The current A7 #1a reproducer
passes `MockCredentialResolver{}` by value, which still satisfies the new interface, so it is unaffected. The
recommendation is to merge this branch first, then rebase the double-credit fix onto it. The reverse order also works,
at the cost of a trivial signature fix-up.

## Routing

- `security`: C1 (a mock credential could reach a real adapter if the split regresses) and C3 (audit durability).
  This review surfaces them. It does not adjudicate them.
- `ledger-finance`: nothing new. No posting path is touched.
- The status of PROV-OUTBOUND-CRED-1 (payments) should stay `PARTIALLY IMPLEMENTED` until C1 and C2 land.
