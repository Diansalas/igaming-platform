# Decision brief: PAY-RECEIPT-ANOMALY-APPLIED-1 (ledger-finance re-attribution ruling)

Status: `NOT IMPLEMENTED` (named residual; owners `payments` + `ledger-finance`). This brief is a document only. It
changes no code, no tests and no registry. Baseline `2ecfbbc`. Sources: ADR 0095 §42.8 (lines ~7838-7919), registry rows
`CLASSB-R10/R11/R12-*` (`docs/governance/task-registry.md` ~4212-4215), `internal/payments/receipt.go`,
`internal/payments/attempt.go`, and the mutation evidence `docs/plans/prh2-hardening-round/prh2-r10-callback-audit-2-mutation-kill.txt`
and `prh2-r11-receipt-orphan-mutation-kill.txt`.

## 1. Current behaviour (file:line, `internal/payments/`)

- `ResolveReceipt` (`attempt.go:860-866`) is the strict one-shot CAS: `UPDATE ... SET attempt_id, resolution, resolved_at
  WHERE id = $1 AND resolved_at IS NULL`; zero rows is a conflict error.
- `closeAnomalyReceipt` (`receipt.go:398-408`): a NEW receipt is closed with the strict `ResolveReceipt(..., nil, reason)`;
  a DUPLICATE is closed with a tolerant CAS (`attempt_id = NULL`, `WHERE ... tenant_id, provider_id, resolved_at IS NULL`,
  zero rows accepted). It takes no parent or attempt lock (documented exception, LF I-2, `receipt.go:390-397`) and writes
  no audit row (security I-2).
- The three anomaly branches run BEFORE the parent lock and call it: precondition anomaly (`receipt.go:558-575`),
  event-type vs operation mismatch (`619-631`), late reference-conflict recheck (`655-670`).
- Main path: receipt insert/dedup (`681`), parent then attempt lock and re-read (`691-704`), then for a duplicate
  `alreadyApplied = receiptIsResolved(...)` (`712-718`). `receiptIsResolved` (`371-379`) returns only
  `resolved_at IS NOT NULL` (read `FOR UPDATE`); it does not look at `resolution` or `attempt_id`.
- `applyResolvedReceiptEvidence(..., alreadyApplied)` (`720`) runs unconditionally. Inside it `alreadyApplied` gates ONLY the
  no-state-change terminal-cell audit rows: `auditTerminalAmountAssetMismatch` (succeeded cell `868`, declined cell `914`)
  and `auditPayoutSucceededForeignRef` (M-1, `896`). The function comment (`798-800`) states no state transition,
  financial effect or raise depends on it; the raises (`877`, `901`, `923`) are unconditional.
- `ResolveReceipt(receiptID, &attempt.ID, resolution)` is skipped when `alreadyApplied` (`739-743`). The deferred drain
  still runs if `changed` (`763-771`); a duplicate returns `DispositionDuplicateEffect` (`773-774`).

## 2. Race scenario (ADR §42.8: "a lock-free anomaly close lands first in a reference-binding race")

The ADR states the scenario in one sentence; the step order below is derived from the code above and is NOT pinned by
any test (the r11 evidence: "NOT built, hence no mutant").

1. Receipt R for evidence E (merchant reference of attempt X, provider reference `refP`) exists and is unresolved (an
   orphaned deferred receipt, the ORPHAN-RESOLVE-1 precondition for the tolerant close).
2. Delivery B of E: resolves X, passes the event-type check and the late recheck (`refP` not yet bound to another
   attempt, `656-657`), dedups onto R (`681`, `duplicate=true`), proceeds to the parent lock.
3. Another attempt Y binds `refP` and commits.
4. Delivery A of E: `ResolveAttemptForEvidence` now sees by-reference Y vs by-merchant X, an anomaly (`558`); dedups onto
   R and the tolerant close commits R as `anomaly_reference_conflict`, `attempt_id` NULL (`568` -> `402-404`). No parent
   lock is taken, so nothing orders A after B.
5. B, under the parent lock, reads `receiptIsResolved(R)` = true (`714`), so `alreadyApplied = true`: it applies E to X
   (`720`), skips the terminal-cell audit rows (`868/896/914`) and skips its own `ResolveReceipt` (`739-743`).

## 3. Why attribution/audit becomes inconsistent

`receiptIsResolved` cannot tell "resolved as applied" from "closed as an anomaly" (ADR §42.8). After step 5: (a) R's
`resolution` is an `anomaly_*` value although E WAS applied to X by B; (b) R's `attempt_id` stays NULL, so the receipt
does not name the attempt it moved; (c) if B landed in a terminal mismatch cell, the once-per-receipt audit row
(PAY-PAYOUT-CALLBACK-AUDIT-2) is never written, because the only writer was gated off and A's close writes none (I-2).
The security I-2 reconstruction claim ("`disposition_at_receipt` plus `resolution`") then reconstructs the wrong story
for R.

## 4. Financial effect

ADR §42.8 and registry `CLASSB-R11`: "No state or money effect" / "attribution/audit gap only". Code reading agrees that
`alreadyApplied` gates no state transition, posting, hold/withdrawal change or raise (`798-800`; audit/resolve only).
None identified. Not established from the sources: whether B's transition in step 5 is itself correct when Y has just
bound `refP` (that is the ordinary reference-binding race, outside this residual), and whether any reconciliation job
reads the receipt's `attempt_id`/`resolution`.

## 5. Remediation options already recorded (no others are proposed here)

- **O-A: treat an anomaly close as "not applied" on the main path.** Recorded in the r11 mutation evidence ("NOT built,
  hence no mutant: 'treat an anomaly close as not-applied' (needs a ruling)"). The ADR (§42.8, ~7884-7886) states the
  consequence: B would then need to write the audit rows and attribute R to X, which "needs a re-attribution transition
  on an already-resolved receipt", i.e. a new UPDATE of a resolved row's `attempt_id`/`resolution`.
- **Strict vs tolerant `ResolveReceipt` (constraint, not a standalone fix).** ADR §42.8 (~7868-7871, ~7885) and r11
  mutant M3 ("always strict ResolveReceipt ... KILLED"): the strict one-shot CAS on an already-resolved receipt
  conflicts, so B cannot simply call the existing `ResolveReceipt`; the anomaly close is tolerant precisely so the
  second actor sees no `ErrAttemptStateConflict`.
- **Status quo.** The ADR records the item as "NOT built, ruled out of scope", with a ruling "required before any real
  provider" (also `CLASSB-R12`, `HANDOVER.md` ~792).

## 6. Recommended direction

None documented. No LF, security or ADR text recommends O-A over the status quo or prescribes re-attribution semantics.

## 7. Tests required for any option (from existing patterns)

- A deliberate-interleaving test in the style of `TestOrphanResolve_Interleaved_AnomalyCloseDuringDrain_NoConflictLeaksToBindingTx`
  (`receipt_orphan_resolve_integration_test.go:467`): no existing hook sits between the main-path recheck (`656`) and
  `receiptIsResolved` (`714`); one would be a `testHook*` seam under the generalised F-L3 static guard (`CLASSB-R11`).
  Assert R's final `resolution`/`attempt_id` per the ruling, the terminal-cell audit row exactly once, no
  `ErrAttemptStateConflict` reaching either delivery.
- No-money assertions as in `TestOrphanResolve_AnomalyBranches_OrphanClosedOnRedelivery_NoMoneyNoState` (`:194`):
  `orMoney` snapshot (state, `ledger_transactions`, `ledger_entries`, receipt count) and `assertLedgerBalanced`.
- Preserved invariants: `TestOrphanResolve_AlreadyResolvedDuplicate_Untouched` (`:236`; a genuinely applied receipt is
  never rewritten), `TestOrphanResolve_Concurrent_TwoRedeliveries_ResolvedOnce_NoError` (`:268`, `runTwo`, 20 reps),
  `TestOrphanResolve_TolerantClose_ScopedToTenantAndProvider` (`:538`), the `TestCallbackAudit2_*` orphan/replay/concurrent
  suites (r10 addendum N1-N6), deposit and payout forms, replay 1,1,1.
- Mutation kill run per the r10/r11 method, including the currently unbuilt "anomaly close treated as not-applied" mutant
  and a re-attribution-overwrites-applied mutant.

## 8. Decision required (ledger-finance)

Ledger-finance is asked to rule on the re-attribution semantics for a payment-provider receipt that a lock-free anomaly
close resolved (`attempt_id` NULL, `anomaly_*`) while a concurrent main-path delivery applied the same evidence to an
attempt: EITHER (A) a resolved receipt may be re-attributed exactly once, from an `anomaly_*`/NULL-attempt closure to
the applying attempt and its resolution, by a new dedicated compare-and-set under the parent and attempt locks (the
one-shot `ResolveReceipt` stays strict and unchanged), with the skipped terminal-cell audit row then written once; OR (B)
a resolved receipt is never re-attributed, the residual is accepted as an attribution/audit gap with no money effect, and
it is documented as such for real-provider acceptance. Whichever is chosen, the ruling should also state whether the
re-attribution (if A) itself writes an audit row.

## 9. Not being decided here / continues meanwhile

Not decided: I-1 predates-submission on the live path (security policy), S-4 benign-PSP paging, LF L-1, the
`GetAttemptByMerchantReference` tenant predicate, the reference-binding race itself, any alert/audit on drain-time
anomaly closes, and every gate listed in `HANDOVER.md` (B13, H-SEC-5/11, ALERT-DELIVERY-1, PAY-DEPOSIT-MISMATCH-ALERT-1,
B14-B18, sandbox PSP, AWS). Meanwhile the current behaviour stays as merged (`7564653`, `bd3c144`) against MOCK; no
real provider may be connected on this path until the ruling exists (ADR §42.8).
