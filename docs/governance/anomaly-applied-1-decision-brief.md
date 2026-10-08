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

## Supplement (2026-10-08)

Added by `ledger-finance` at baseline `86a5439` (branch `gate-r14-b`). Document only: no code, test, migration or
registry change. The sections above are unchanged. Their `receipt.go` line numbers were re-checked at `86a5439` and still
match. This supplement adds the evidence the ruling in section 8 needs and the brief above did not contain. Where the
record or the code does not establish something, it says so.

### S1. Completeness check of the brief against the (A)/(B) choice

The brief above is enough to understand the race. It does **not** contain five facts that bear directly on the choice:

1. The database guard on receipts (S3). It decides which parts of option (A), as worded in section 8, are possible
   without a migration.
2. Which audit rows are actually lost, by attempt state (S2).
3. Whether reconciliation or any other reader consumes `attempt_id` or `resolution` (S6).
4. A second, non-race path to the same receipt shape, found by code reading (S7).
5. The ownership basis (S9).

### S2. Why there is no financial effect (cited)

- `alreadyApplied` gates only three audit inserts: `auditTerminalAmountAssetMismatch` at `receipt.go:868-872` (succeeded
  cell, R-6) and `:914-918` (declined cell, R-5), and `auditPayoutSucceededForeignRef` at `:896-900` (M-1). It also gates
  the main-path `ResolveReceipt` at `:739-743`. Every state transition, posting, hold or withdrawal call and raise in
  `applyResolvedReceiptEvidence` (`:808-1133`) runs whatever its value. The raises at `:877`, `:901` and `:923` are
  unconditional, and the function comment at `:798-800` says so.
- The only cells it gates are the **no-state-change** terminal cells: the attempt is `succeeded` or `declined` and the
  evidence is a mismatched or foreign-reference success. In those cells nothing posts and nothing transitions
  (ADR 0095 §42.8 R-5/R-6/M-1: "No state change, no release, no settlement, no posting").
- **Consequence by attempt state.**
  - If X is **non-terminal** when B applies E, B's cell runs completely, including any audit row the cell writes itself.
    For example, the payout reference-conflict park audits `payments.payout_parked_reference_conflict` and raises,
    regardless of `alreadyApplied`. The only defect left is R's `attempt_id` and `resolution`.
  - If X is **terminal** (`succeeded`/`declined`) and E is mismatched or carries a foreign reference, the one audit row
    of that cell is lost. For a payout the P1 alert is still raised. **For a deposit there is no alert** at those cells
    (PAY-DEPOSIT-MISMATCH-ALERT-1 is not built, §42.8), so the race leaves the receipt row as the deposit's only trace.
- Existing tests that already assert "no money, no state" for the anomaly-close branches:
  `TestOrphanResolve_AnomalyBranches_OrphanClosedOnRedelivery_NoMoneyNoState`
  (`receipt_orphan_resolve_integration_test.go:194`) and `TestOrphanResolve_AlreadyResolvedDuplicate_Untouched` (`:236`).
  **No test pins the race itself.** The r11 evidence (`prh2-r11-receipt-orphan-mutation-kill.txt:55`) says: "NOT built,
  hence no mutant".
- What B's own transition does when Y has just bound `refP`:
  - **Payout success:** `applyPayoutSuccess` runs `payoutGuardReferenceBinding` before `ApplySuccess`/`Complete`
    (`payout.go:748-750`). X is therefore parked (`provider_reference_conflict`, hold kept), not completed.
  - **Payout pending and decline:** both branches also guard (`receipt.go:817-820`; `applyPayoutDecline`, `payout.go`
    ~778).
  - **Deposit success:** `postDepositSuccessOrDispute` (`orchestrator.go:815-846`) contains no foreign-reference
    pre-check that I could find on the receipt path. Whether B then fails on a unique index and rolls back entirely, or
    takes another path, is **not established** here. This is the ordinary reference-binding race, which the brief
    already places outside this residual.

### S3. What can and cannot be repaired (database guard)

`payment_provider_events_guard` (`migrations/0101_payment_attempts.up.sql:399-436`) is the only definition of the
function. No later migration replaces it. It makes every column immutable except three:

- `attempt_id`: a change is refused **only when `OLD.attempt_id IS NOT NULL`**. A NULL → value write is therefore
  allowed once, **even after `resolved_at` is set**. Re-attributing R's `attempt_id` to X is possible under the current
  schema.
- `resolution`: one-shot (`OLD.resolution IS NOT NULL AND NEW.resolution IS DISTINCT FROM OLD.resolution` raises).
  Changing R from `anomaly_reference_conflict` to the label B would have written **cannot be done without a migration
  that changes this guard**. The CHECK values are `applied`, `anomaly_cross_provider`, `anomaly_reference_conflict`,
  `anomaly_predates_submission` and `anomaly_other`.
- `resolved_at`: one-shot. A re-attribution would not need to change it.
- `disposition_at_receipt` and the evidence columns (`provider_reference`, `merchant_reference`, `outcome`, `amount`,
  `asset_code`, `settlement_reference`, …) are immutable. **The evidence itself is never lost.**

What a correct label would be is also not obvious. For a payout success that B parks (S2), B's cell returns
`ResolutionApplied` (`receipt.go:1048-1051`, because `applyPayoutSuccess` returns nil after a park). A non-terminal "re-label"
would therefore be `applied`. For the terminal cells it would be `anomaly_other`. Which label the ruling wants is
**not established by the record**.

Missing audit rows: `audit_log` is append-only by trigger (`migrations/0014_create_audit_log.up.sql:43-53`), so a row
can only be added later, never back-dated. Its content can be largely rebuilt from the receipt's immutable columns plus
the attempt's immutable `amount`/`asset_code` (`payment_attempts_guard`, latest definition
`migrations/0115_payment_force_resolution.up.sql:1030-1065`). One field cannot always be rebuilt: the
`attempt_state` at the time of the event. A `succeeded` attempt stays `succeeded`, but a `declined` payout may since have
moved to `disputed` by T14.

The original applied cell's **effect** is not lost and needs no recovery: its state, ledger and alert writes committed in
B's transaction. Only the audit row and the receipt attribution are missing.

### S4. Could any repair alter money state?

Not by the code paths cited. A re-attribution would be an UPDATE of `payment_provider_events.attempt_id` (and of
`resolution` only if S3's migration were made). A missing-row repair would be an `audit_log` INSERT. Neither table is a
ledger, attempt, withdrawal or projection table. Money could move only if a repair **re-ran** `applyResolvedReceiptEvidence`
or the drain, and neither option in section 8 says that. Under MOCK, no real-provider rows exist that would need
repair.

### S5. Idempotency, audit and concurrency facts relevant to either option

- **Lock order** (ADR 0095 §14; §42.8 L-1): parent, then attempt, then receipt rows. In step 5 of the race, B already
  holds the parent and attempt locks, and the receipt row lock taken by `receiptIsResolved`'s `FOR UPDATE`
  (`receipt.go:373-379`). An UPDATE of R inside B's transaction would therefore take no new lock.
- **Tolerant close** (`receipt.go:398-408`): it takes only its own receipt row lock and no parent or attempt lock
  (LF I-2 exception). The residual exists only when A **commits before** B's `FOR UPDATE` read:
  - If B reads first, A waits on the row lock. After B's strict `ResolveReceipt` commits, A's `resolved_at IS NULL`
    predicate matches nothing.
  - If A has not committed when B reads, B waits and then sees it resolved.
- **The strict `ResolveReceipt`** (`attempt.go:868-872`, `WHERE resolved_at IS NULL`) cannot be reused for a
  re-attribution, because it would conflict (r11 M3). Any new compare-and-set would need a predicate that tells an
  anomaly close (`attempt_id IS NULL`, `resolution LIKE 'anomaly_%'`) apart from a genuinely applied receipt.
  `TestOrphanResolve_AlreadyResolvedDuplicate_Untouched` pins that an applied receipt is never rewritten. The guard
  already makes `attempt_id` one-shot, so concurrent redeliveries would write it at most once.
- **The drain** selects `FOR UPDATE` and closes with the strict CAS (§42.8). It applies only unresolved rows, so R is
  invisible to it either way.
- **Existing audit rule:** once per NEW receipt, never per redelivery (the L-e precedent; PAY-PAYOUT-CALLBACK-AUDIT-2).
  Whether a re-attribution writes its own audit row is the open sub-question already in section 8.
- **Security I-2:** an orphan close writes no audit row, and the receipt is "reconstructable from
  `disposition_at_receipt` plus its `resolution`" (§42.8). Under the race that reconstruction is wrong for R. That is
  security's claim, and it is affected by option (B).

### S6. Facts the brief could not establish, now checked in code

- **Does reconciliation read the receipt's `attempt_id` or `resolution`? No.** The only receipt query in
  `internal/reconciliation` is `checkDeferredReceipts` (`payment_statement.go:1497-1525`). It reads
  `id, event_type, provider_reference, outcome, received_at` with `resolved_at IS NULL AND disposition_at_receipt =
  'deferred_unresolved'`. R is resolved in both orderings, so it is never a `pay_unresolved` finding either way.
- **Any other reader?** No non-test Go code outside `internal/payments` references `payment_provider_events`. No
  migration defines a view or function over it (0106 changes only the RLS policy; 0115 only adds prefix CHECKs). Inside
  `internal/payments` the readers are:
  - `CountUnappliedReceipts` (`receipt.go:259-275`), `receiptIsResolved` and the drain, which use `resolved_at`;
  - the dedup lookup (`:358`, by fingerprint).

  None reads `resolution` or `attempt_id`. **The mislabel affects audit and human reconstruction only.**
- **Is B's transition correct when Y has just bound `refP`?** Established for payouts (S2: parked, hold kept). Not
  established for the deposit receipt path.

### S7. A second path to the same shape (code reading; not in the record; not tested)

`ResolveAttemptForEvidence` treats a merchant-reference match on an attempt whose `provider_id` is NULL as
`anomaly_cross_provider` (`receipt.go:458-465`). Every other anomaly input is fixed once set: provider id, provider
reference and operation are immutable. A cascade row (`created`, `provider_id` NULL) gets `provider_id` at T2, though.
A redelivery after that T2 would:

- resolve by merchant reference;
- dedup onto the anomaly-closed receipt (`duplicate=true`, `receiptIsResolved=true`);
- apply E with `alreadyApplied=true`, including any posting, which is not gated.

The result is the same shape (`anomaly_cross_provider`, `attempt_id` NULL, evidence applied) with no concurrency
involved. Whether an honest provider can reach it is doubtful: the merchant reference is the id of an attempt never
sent to that provider (INV-IO-3). This is **not established by the record** and contradicts the ADR's "arises only
when …" wording only if it is reachable. Any (A) predicate would need to decide whether it covers this path.

### S8. Recommendation

**No recommendation is documented.** No ADR, review or registry text prefers (A) or (B), and this supplement makes no
choice. ADR 0095 §42.8 records the item as "NOT built, ruled out of scope", with a ruling "required before any real
provider".

### S9. Why the choice sits with ledger-finance (cited)

- CLAUDE.md "Specialist agents": "`ledger-finance` specialist owns financial invariants". CLAUDE.md "Security": every
  mutating financial action writes an append-only audit record.
- ADR 0095 §42.8 names the residual's owners as "`payments` + `ledger-finance`" and makes a ruling on re-attribution
  semantics a precondition for any real provider.
- `docs/governance/human-decision-register.md` row PAY-RECEIPT-ANOMALY-APPLIED-1 names `ledger-finance` as the decider
  ("AWAITING LEDGER-FINANCE DECISION").
- Not established by the record:
  - whether `security` must co-sign. Its I-2 reconstruction claim is affected (S5).
  - whether option (A)'s `resolution` change (S3) counts as "mutating a historical entry" under the ledger-finance
    veto. The receipt table is append-only except three one-shot columns, and it is not the ledger. If (A) includes a
    `resolution` change, a migration altering `payment_provider_events_guard` is a schema change and needs the usual
    architect, ADR and review path.
