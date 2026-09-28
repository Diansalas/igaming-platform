# Identity-compliance review — PRH-2 plan (2026-09-28)

Reviewer: `identity-compliance`, read-only, plan at `a3547f5`. Scope: F-kyc (KYC-ENF-OUTAGE-1), F-pay (KYC-ENF-DECISION-ROWS-1), KYC-ENF-TESTPINS-1, E1 (KYC-SUBMIT-OUTBOX-1), and jurisdiction configurability.

**Verdict: APPROVE WITH CONDITIONS.** No KYC threshold or jurisdiction rule is invented, no enforcement rule is weakened, and the plan's citations match the code and the FH-7 re-review. Money is already safe; these fixes close audit and observability gaps. Migrations 0108–0114 are free.

**Verified:**
- `enforcement.go:465-468` swallows a DB error into `unavailableDecision` while the transaction stays aborted. `withdrawal.go:397-417` then calls `RecordDecision` in that aborted transaction, which reproduces 25P02.
- `kycgate.go:40-68` never calls `RecordDecision` (wired at `deposit_v2.go:202` and `drive.go:100`).
- `payout.go:129-140` and the T1p allow path (`:285-323`) write no decision row, and make no provider call in that transaction.
- `payout_sweep.go:99-130` audits a deny but records no decision.
- `withdrawal.go:1046-1051` does not guard `Outcome == OutcomeUnavailable` (item N5).
- `document_service.go:285-292` defers the outbox as a hard precondition for a real KYC adapter.
- The savepoint pattern the fix relies on already exists (`internal/db/idempotency.go:45`).
- HD-KYC-1..8 are not re-asked. The only RG mention (casino B) re-checks an existing gate.

| ID | Severity | Plan section | Evidence | Required change |
|---|---|---|---|---|
| F1 | Low | §3 vs §5 (F-pay) | §3 lists reviewers "(SEC, CR)"; §5 lists security, code-reviewer and qa. | Add `qa` to §3 so it matches §5. |
| F2 | Low | §7 / registry | The registry (~3984) says "HD-KYC-1..7"; ADR 0096 §4 defines 1..8, including HD-KYC-8 (play trigger). | Correct the registry to HD-KYC-1..8 in the W0 registry pass. |
| F3 | Low (design completeness) | §5-E1 | ADR 0096 §2.6(g)/§19 excludes an undecided phase-A orphan (`unverified`, NULL `provider_reference`) from "latest decided row", so a vendor outage cannot manufacture a false `failed`. E1's new outbox states are not reconciled with that rule. | E1's ADR 0095 §15.3 amendment and tests must show that a `pending` or `claimed` (not yet `sent`) outbox row still reads as an orphan for enforcement. This is an E1 DoD item. |
| F4 | Info | F-kyc/F-pay vs F-POOL-2 | The withdrawal decision write, payout T1p allow and the deposit phase-A gate are all transactions with no provider I/O. | None; adding `RecordDecision` in those transactions is safe. |

**Assessment:**
- **F-kyc:** the savepoint fix is correct and feasible. It records the `unavailable` decision in the valid outer transaction and returns 503. The separate-transaction fallback is an acceptable contingency.
- **F-pay:** joint ownership (payments + identity-compliance) is correct, and every write lands in a transaction with no provider I/O.
- **TESTPINS:** matches the registry and the FH-7 re-review.
- **E1:** satisfies F-POOL-2 and is correctly a hard precondition for a real KYC adapter. F3 applies.
- **Jurisdiction:** policies stay jurisdiction- and tenant-scoped, effective-dated (ADR 0096 §3.6, unchanged), fail-closed and audited; no seed values.

**Conditions:** F1 and F2 in W0; F3 before E1 is marked done; no KYC or RG threshold seeded by F-kyc, F-pay or E1 (already self-imposed by the plan).
