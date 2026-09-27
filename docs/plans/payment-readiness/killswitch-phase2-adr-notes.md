# ADR 0095 notes from kill-switch phase 2 fix round (for the architect to fold in)

The architect is editing ADR 0095 directly in the main repo concurrently with this
round, so these notes are recorded here rather than as a direct ADR edit, per the
coordinator's routing instruction. Both are one paragraph, for §10.x.

## 1. Merge-ordering decision: phase 2 merges before PAY-DOUBLE-CREDIT-1

The kill-switch phase 2 branch (`ea7910a`, `d4520da`, plus this fix round's commits)
merges **before** the PAY-DOUBLE-CREDIT-1 fix, not after and not interleaved. Both the
independent code review (`rv-prh-i1-killswitch-phase2-code-review.md`, "Merge dependency
on PAY-DOUBLE-CREDIT-1") and the security review (`rv-prh-i1-killswitch-phase2-security.md`,
"Overlap with the deposit/callback code and the double-credit fix") independently confirm
this branch touches no success, credit, decline-with-posting or cascade path: the deposit
kill-switch T3 decline finalizes an intent that never had an attempt row, before any
provider call could exist, so no success evidence can ever later arrive for it; the only
hunks in `drive.go`, `sweeper.go`, `payout_sweep.go`, `deposit_v2.go` and `payout.go` are a
mechanical `pool` argument added to existing `callProvider`/`DispatchWithdraw` calls, never
a change to `applyDepositCallResult`, `postDepositSuccess`, `ApplySuccess`, `receipt.go` or
`cascade.go`. Merging this branch first avoids a signature rebase of the double-credit fix
(which would otherwise need to update its own `callProvider`/`DispatchWithdraw` call sites
and any custom `OutboundCredentialResolver`/`CallContext` fake to this branch's new
`Resolve(ctx, pool, tenantID, providerID) providercred.OutboundCredential` contract after
the fact); merging the double-credit fix first would work too, at the cost of that trivial
fix-up, which is why both reviews rule the ordering as safe in either direction but this
order as strictly cheaper. Conditions for whichever branch lands second, per both reviews:
any `callProvider`/`DispatchWithdraw` call site the double-credit fix adds or moves must
pass `pool` and the kind-split resolver (never a nil resolver - see P2-L3's now-closed gate
guard); the fix must not route a deposit success through the legacy
`InitiateDeposit`/`attemptDeposit` path (see the new task-registry row
`PROV-OUTBOUND-CRED-1-LEGACY-PATH`, unaffected by this branch and owned by the double-credit
fix team's area of `orchestrator.go`); and the full `internal/payments` suite plus the
kill-switch predicate coverage tests should be re-run once both branches are on the same
tree.

## 2. §10.3 wording vs. the deposit kill-switch decline response (C5)

ADR 0095 §10.3 currently reads: "Deposits: `created` attempts are moved to `rejected` (T3,
`kill_switch`) and the player sees 'unavailable'." That sentence predates the
`InitiateDepositAttempt` two-phase cutover it now describes: under the combined T1+T2 form,
a kill-switch refusal at the claim statement never creates an attempt row at all (the
`INSERT ... SELECT` simply matches zero rows), so there is no `created` attempt to move to
`rejected` - the *intent* itself is finalized as `declined` instead, via the exact same
`finalizeDeclined` path (and the exact same player-facing response shape) an RG or KYC
pre-attempt denial already uses. The player-facing `depositIntentResponse` carries only
`status` (never a reason field) for every decline path alike, so a kill-switch refusal is
already indistinguishable from any other decline at the wire level - "the player sees
`unavailable`" was never a distinct HTTP/response-body contract, only a description of the
outcome's *meaning*. The kill-switch specialist's assessment (not binding on the architect):
correct this sentence to match the two-phase reality ("the deposit_intents row itself is
finalized `declined`, with reason `kill_switch` recorded only in the operator-facing audit
metadata, never surfaced to the player") rather than changing the response contract to a
distinct `503`/"unavailable" shape for this one decline reason among several that already
share one generic response - a player-visible distinction here would be new product
behaviour, not a bug fix, and is better decided deliberately than fixed as a side effect of
this review response. `finalizeDeclined`'s call site for this decline now also records
`provider_id` (both on the `deposit_intents` row and in the `deposit.declined` audit
metadata), closing C5's second, uncontested finding.
