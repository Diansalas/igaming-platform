# RV-PRH-I1 — Architect review of kill-switch Phase 2 (human-required pre-merge gate)

- **Reviewer:** `architect`.
- **Date:** 2026-09-27.
- **Subject:** `worktree-agent-aa2bb3c6bdd6d51eb` @ `4e04f4e`.
  - Phase 2: `ea7910a` (AM-1 token-tenant scope, RV2-L1/L2) and `d4520da` (orchestrator wiring,
    payments kind split).
  - Fix round: `ce77bac`, `50b595d`, `533f85f`, `f5e96c4`, `4e04f4e`.
- **Read:**
  - the branch diff against its merge base;
  - `internal/payments/{outbound_resolver.go, gate.go, contract.go, deposit_v2.go, drive.go,
    attempt.go, cascade.go, payout.go}` at `4e04f4e`;
  - casino's `OutboundKindSplitResolver` for comparison;
  - `rv-prh-i1-killswitch-phase2-security.md` and `rv-prh-i1-killswitch-phase2-code-review.md`;
  - `killswitch-phase2-adr-notes.md`.
- **Not done:**
  - No tests were run. I relied on the two reviews' executions and the fix-round commit
    evidence.
  - The code-reviewer has not yet re-reviewed the fix round.
- **Recorded in:** ADR 0095 §10.9, plus the header status table and in-place notes on §10.3 and
  §10.6.

## Verdict: **APPROVE THE MERGE, with one new residual (KS-DEP-T2-T3-1) that does not block the merge**

### (1) Kind split and pool threading — consistent (approved as-is)

- `payments.OutboundKindSplitResolver` is line-for-line the casino/KYC design:
  - it selects by adapter identity (`SyntheticComponent()`);
  - an unregistered provider fails closed;
  - a nil target fails closed;
  - it returns a nil interface when nothing is wired.

  It is deliberately duplicated per domain (ADR 0095 §3.2: no cross-domain import). A shared
  neutral helper is **not** requested now.
- `callProvider` preserves the ADR 0095 §3.2 order: nil-resolver guard → `txscope.Held` →
  committed claim → `Resolve(ctx, pool, …)` → binding check.
- The resolver opens its own short transaction on `pool`, which is the ADR 0094 §4.1 split, so
  no connection is held across the call. That satisfies INV-IO-1 and INV-IO-11.
- C1, C2 and P2-L1 now pin the routing, the pool at all five call sites, and the tenant-binding
  layer. The fix-round commits give mutation evidence for each.
- The hard-wired MOCK resolver on the MOCK statement source is correct (§12.4) and documented.
- C4 (MOCK half wired only when test-support endpoints are enabled) is accepted as consistent
  with casino/KYC. When a sweeper is wired, add a boot-time refusal for "synthetic payments
  adapter, no mock resolver".

### (2) Merge ordering — **confirmed: Phase 2 before PAY-DOUBLE-CREDIT-1**

I agree with both reviews. No hunk touches:
- `receipt.go`;
- `postDepositSuccess`;
- `applyDepositCallResult` / `applyStatusEvidence`;
- cascade posting.

The deposit decline happens before any attempt exists. Landing Phase 2 first lets the ADR 0095
§28 fix be written against the final signatures.

Conditions on the §28 fix (ADR 0095 §10.9.1):
- it passes `pool` and the kind-split resolver at every call site;
- it adds no provider call inside an evidence transaction (the INV-IO-1(c) scan stays green);
- it posts nothing through the legacy `InitiateDeposit` path;
- it re-runs the full payments suite plus the predicate-coverage and pool-threading tests on the
  combined tree.

### (3) C5 — **amend the ADR, keep the code** (architecture ruling; no human decision needed)

- **What happens:** on the player path no attempt row is created, so the intent is finalized
  `declined`. This is the same shape as an RG or KYC phase-A denial.
- **What the player sees:** the generic decline (`status = "declined"`, no reason).
- **What operators see:**
  - `kill_switch` in the audit metadata;
  - the routed `provider_id` on the intent and in the audit row (fixed in `f5e96c4`).

Why amend the ADR rather than the code:
- "unavailable" in §10.3 was a description of meaning, never a specified response contract.
- A reason-specific player response would disclose operator containment state, which no other
  decline cause discloses.
- A terminal decline, where the retry needs a new key, is safer than a retryable 503 that parks
  a `pending` intent across a containment.

If product wants distinct player copy, it is a UI/brand-configuration mapping. It is recorded
as a deferred consideration, not built. This is not a licence, legal or commercial question,
so no human decision is required.

### New finding — KS-DEP-T2-T3-1 (Low/Medium; `payments`; NOT IMPLEMENTED)

§10.3's "created attempts → `rejected` (T3, `kill_switch`)" is still unimplemented for
**cascade** deposit attempts.

- **Mechanism:** a provider-scoped switch covering the fallback provider makes
  `ClaimCreatedForSubmission` match zero rows. `drive.go` returns that as an error, so the
  player's synchronous-cascade request fails. The attempt stays `created` and the intent stays
  `pending`.
- **Why it matters now:** `cmd/platform-api` wires no `payments.Sweeper`, so the intent is stuck.
- **Why it is not dangerous:** it fails closed. Nothing is sent and no money moves.
- **Required:** in the same per-item transaction, classify with `KillSwitchEngaged` (read-only;
  the in-statement predicate stays the control), then:
  - `RejectCreated(…, 'kill_switch')`;
  - recompute the intent projection;
  - audit;
  - add a test plus a mutant.
- **Gate:** before PRH-I1 is marked complete and before a sweeper is wired. It is **not** a
  merge blocker.

### Still open (unchanged)

- The code-reviewer's re-review of the fix round.
- PROV-OUTBOUND-CRED-1-LEGACY-PATH: a precondition on any real payments adapter.
- Alert delivery and KS-AUDIT-TENANT-1: launch-blocking.
