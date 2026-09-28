# Payments review — PRH-2 plan (2026-09-28)

Reviewer: `payments`, read-only; HEAD `a3547f5`, which is identical in code to the plan's base `559483a`. Scope: the payments lane E2 → C → D → F-pay → H → I-wire → K3, file ownership, hidden dependencies, the "no migration needed" claims, and INV-DEP-1, 0107 and kill-switch conflicts.

**Verdict: APPROVE WITH CONDITIONS** (F1–F3; none blocks W0/W1).

**Verified against code:**
- **E2.** The chain `orchestrator.go:555` → `:674` → `:700` (Deposit at `:714`) → `:762` → `:793` (QueryStatus at `:797`) has no non-test caller. Deletion is safe.
- **C.** `drive.go:249-272` never calls `providerref.Validate`. Payout does, at `payout.go:393-395` and `:1178-1181`. The park mechanism `ApplyDisputeFromNonTerminal` already exists.
- **D.** `sweeper.go:513` posts `attempt.Amount`/`AssetCode` without comparing them to the poll result. The only comparison, at `:471`, audits the already-succeeded case. The dispute-instead-of-post fix is exactly the shape INV-DEP-1 (0107) requires.
- **F-pay.** `kycgate.go:56-68` never calls `kyc.RecordDecision`. The function (`enforcement.go:706`) and the RLS-protected table (0100:179-207) already exist, so no migration is needed.
- **H.** `NewSweeper` (`sweeper.go:102`) has no caller. `main.go:394-447` already wires reconciliation, RG and bonus loops with the same per-tenant, panic-recovery and graceful-shutdown pattern.
- **I-wire.** The log-only alert sites are at `orchestrator.go:1018` and `:1509`.
- **K3.** The last migration is 0107, so 0114 is correct in the allocation order. `payment_attempts_guard()` (0107:98) is the right amendment target. `withdrawal.Complete`/`Fail` (`withdrawal.go:1408/1498`) are the right M2 entry points; there is no direct ledger post.
- **"No migration needed"** is confirmed for C, D, E2, F-pay and H.
- **KS-CAS-DISCRIM-TEST-1** is correctly folded into C/W2.

| ID | Severity | Plan section | Evidence | Required change |
|---|---|---|---|---|
| F1 | Medium | §1-H, §3-H, §5-H | `Sweeper` is a single type spanning `sweeper.go` (deposit) and `payout_sweep.go` (payout T2/T12/resolve). `RunOnce` (`sweeper.go:124`) dispatches generically. | State explicitly that H's loop also activates payout dispatch and resend. H is a payout go-live gate too, and F-pay's payout-gate fixes are a hard precondition for H's payout half. |
| F2 | Low | §1, §9 | PAY-F3SM-TEST-1, PAY-SWEEP-CAS-NOISE-1 and RECON-PAYOUT-LIVE-TEST-1 are absent from the plan. | Fold PAY-F3SM-TEST-1 and PAY-SWEEP-CAS-NOISE-1 into D (same success-branch code) and RECON-PAYOUT-LIVE-TEST-1 into H's DoD. Otherwise list them as deliberately not planned, with a reason. |
| F3 | Low | §3 ownership | K3 "uses" `withdrawal.Complete/Fail`, but its Touches column omits `withdrawal.go`, which is a critical file. | State that K3 makes no edits to `withdrawal.go`. If it needs a helper there, add the file to K3's Touches column so Rule 1 applies. |
| F4 | Info | §4, §1-K | Migration allocation and the guard's amendment target are correct. | None. |

**Assessment:**
- **Feasibility:** high; every claim checked holds.
- **Fix designs:** C, D, F-pay, H and K3 reuse already-reviewed primitives, which keeps the risk low.
- **Serial order:** consistent.
- **Invariants:** no conflict with INV-DEP-1, the 0107 guard or the kill switch. C's park uses non-terminal → disputed, not the reason-restricted declined → disputed transition.
