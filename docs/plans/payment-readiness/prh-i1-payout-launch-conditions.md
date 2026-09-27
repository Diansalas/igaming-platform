# PRH-I1 payout dispatch — launch conditions (S-L1, S-L3, S-L4, destination binding)

- Source: `rv-prh-i1-payout-security.md` (security review of the payout
  dispatch path, ADR 0095 T1p/phase B/phase C, §27.11-§27.13).
- Registry: `docs/governance/task-registry.md`, `PAY-SEC-LAUNCH-1`.
- This is a small, standalone note, NOT an edit to ADR 0095 itself
  (ADR 0095's own §27 change log is ledger-finance's to maintain; this file
  exists so these four items are recorded and traceable without touching
  that ADR directly). If/when ledger-finance folds these into ADR 0095's
  own launch-condition list, this file can be superseded/removed.
- These are launch conditions for enabling real-money payout dispatch in
  production, not blockers for the current stage's dev/sandbox work. None
  of the four are fixed by this round (PRH-I1 payout security round,
  registry items S-M2/TESTS-1/S-L2, which ARE fixed - see
  `rv-prh-i1-payout-security.md`'s own conditions list and this repository's
  commit history for that work).

## S-L1 — beneficiary/approver separation on submit and `/resolve`

**Finding.** `approverEligibilityCheck` (submit and `/resolve`) checks
linked-and-active only, never whether the staff account resolving/
submitting a payout is linked to the SAME Person as the withdrawing
player. Probe HP3 (security review): a `finance` staff account linked to
the same Person as the withdrawing player got 200 on submit
(approved→submitted) and 200 on `/resolve`. The four-eyes approvals
themselves are unaffected (the `withdrawal_approvals` trigger blocks
self-approval by a distinct mechanism); what a beneficiary-staff account
controls here is dispatch TIMING and the payout RAIL
(`payment_method` is staff-supplied at submit time).

**Required before launch.** Pass the same `BeneficiaryCheck` `Approve`
already uses (`internal/withdrawal`) through both `newSubmitWithdrawalHandler`
and `newResolveWithdrawalHandler` (`internal/httpserver/withdrawal_handlers.go`),
returning 403 when the acting staff member is linked to the withdrawing
player's own Person - and add a permanent test for both routes.

## S-L3 — no rate limiting/throttling on `/resolve`

**Finding.** Each `/resolve` call can run up to ~60s of outbound
`QueryStatus` on a context detached from the request (a client disconnect
frees nothing), 2-3 DB transactions, and one audit write. Every
non-transitioning call also runs `RescheduleNonTerminal`
(`next_action_at = now()+30s`, `poll_count` bump), so a finance user
calling it repeatedly can defer the sweeper's own T12/escalation
indefinitely for that one attempt, push its backoff to the cap, or
overwrite the escalated cadence. Blast radius is bounded (the circuit
breaker is per (tenant, provider) and `/resolve` is finance-role-only), so
this is an insider/compromised-account risk, not an unauthenticated DoS
vector.

**Required before launch.** A per-attempt minimum interval between
`/resolve` calls (409/429 below it), and staff polls must not advance
`next_action_at`/`poll_count` unless the call actually changed state.

## S-L4 — T1p not fully inside ADR §5.2 phase-A order

**Finding.** Two gaps against ADR 0095 §5.2's own phase-A ordering:

1. The staff eligibility check (submit) runs in a SEPARATE transaction
   before `ClaimForDispatch` - §5.2 step (1) places it inside the phase-A
   transaction. A staff account deactivated in that exact window can still
   claim.
2. §5.2 step (4)'s in-transaction capability re-read is absent. A
   capability disabled between A0 routing and the T1p commit is still
   claimed.

The kill switch (evaluated inside the claim statement itself) remains the
emergency stop for both gaps in the meantime.

**Required before launch.** Move the staff eligibility check into the T1p
transaction the next time `ClaimForDispatch` changes, and add the
in-transaction capability re-read §5.2 step (4) calls for.

## Destination binding — `WithdrawRequest` carries no verified destination

**Finding.** `WithdrawRequest` (`internal/withdrawal`) has no destination
field at all; `payment_method` is supplied directly by staff on submit.
Label: `PROVIDER DEPENDENT` / open architecture question, tracked
alongside HD-0095-1 (M3's own missing release route) and R5/CP-W1 (no
binary constructs a payout `Sweeper` yet, so T2/T12/crash-recovery/
escalation do not run in any current deployment).

**Required before launch of any real payout adapter.** The payout
destination must come from a player-bound, verified instrument resolved
SERVER-SIDE (never taken from the staff request body) - this is the same
class of decision as ADR 0008's custodian-vs-self-custody split for
crypto payout rails, and belongs with the architect/human sign-off this
project's "when to stop and ask" rule already requires for major,
irreversible architecture decisions - not something the payments
specialist decides unilaterally.

## Addendum: S-L2/SP-C - FIXED (FH-6 round 2, corrected per P-C2)

Not one of this note's own four items (S-L2 is tracked separately in
`docs/governance/task-registry.md`, `PAY-SEC-S-L2`), but recorded here for
anyone reading this file for full payout-sweeper launch context, since it
bears directly on "required before any binary wires the payout sweeper":

**P-C2 correction (FH-6 ledger-finance ruling, `rv-prh-i1-payout-ledger.md`):**
an earlier version of this addendum attributed the 7-test regression below
to "T2/T12 setting `next_action_at` earlier than `lease_until`". That
statement was wrong and is struck through by this correction, not left
standing: at the commit in question, both `ClaimCreatedForSubmission` (T2)
and `ResubmitAmbiguous` (T12) set `next_action_at = lease_until` exactly -
there is no gap between them at claim/resend time. The REAL cause ledger-
finance's own reproduction isolated: **phase C moving a row OUT of
`submitting`** (to `ambiguous`/`pending`/`created`) without clearing its
lease. A `claimBatch` exclusion that checked lease liveness for EVERY
state (not just `submitting`) would then wrongly refuse to reclaim a
NON-submitting row still carrying an old, technically-live lease from a
PRIOR claim - exactly the shape of the 7 regressed tests, none of which
involve a live lease on a `submitting` row at the point they call
`claimBatch` again.

The fix (**V1**, restricted to `state = 'submitting'`, ledger-finance's own
required condition) is now implemented in `claimBatch`
(`internal/payments/sweeper.go`):

```sql
AND NOT (state = 'submitting' AND lease_until > now()
         AND lease_owner IS DISTINCT FROM 'sweeper-batch')
```

This closes SP-C (a `submitting` row under a live, non-batch lease - e.g.
a T12 resend's own claim - is never relabelled as the batch's own lease
while it may still be in flight) without touching any non-`submitting`
row's ordinary cross-tick continuation, which is exactly what the
over-broad first attempt (state-independent) regressed. Verified: the full
`internal/payments` suite passes (including all 7 previously-regressed
tests), a permanent SP-C reproduction test
(`TestClaimBatch_SL2_SPC_StaleSnapshotNeverRelabelsALiveSubmittingLease`,
`internal/payments/payout_security_round_test.go`) passes, and ledger-
finance's own V1 probe (run independently on their own private DB) also
passed.

**V2** (`RescheduleNonTerminal` clamping `next_action_at` to
`GREATEST($2, lease_until)` when a live, non-batch lease is held) is
ledger-finance's own documented "optional defence in depth, not a
substitute" - it closes only the `RescheduleNonTerminal` path, while
`Escalate` and `Touch` (T17) write `next_action_at` with no lease
awareness either. Not implemented this round; V1 alone is the required,
structural fix (it guards every writer of `next_action_at` at the single
point of claim, which V2 alone would not).

**S-L2 is no longer a registered condition against the launch conditions
below** - it is fixed and tested. `PAY-SEC-S-L2` in the task registry
reflects this.

## Status

All four items above are **NOT IMPLEMENTED** as of this note (2026-09-27).
None block the current stage's B2C MVP payout-dispatch path (dev/sandbox,
mock provider only, no binary yet wires a real payout `Sweeper` per R5/
CP-W1 above) - they are recorded here as explicit, tracked conditions for
enabling real-money payout dispatch in production, per
`rv-prh-i1-payout-security.md`'s own "Before production launch (payouts)"
section. Launch authorization itself remains the human's decision
(CLAUDE.md "When to stop and ask").
