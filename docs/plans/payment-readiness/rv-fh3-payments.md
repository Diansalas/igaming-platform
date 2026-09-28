# Payments review: FH-3 (ADR 0095 §28 AM-2 / §29, migration 0107) — INV-DEP-1

- Reviewer: `payments` (payment-orchestration/state-machine owner)
- Branch reviewed: `worktree-agent-adce273a3a5339f77` @ `a927aed`; main implementation
  commit `8ce538c`, plus `109ef04` (security LOW fixes), `bdb9085` (QA adjudication
  corrections applied), `f43025c`/`5541b23`/`9e107ed` (ledger-finance FH-5 re-review and
  code-review conditions closed).
- Method: detached worktree (`<scratchpad>/pay-fh3` @ `a927aed`), code read against
  ADR 0095 revision 4 §28/§29, `docs/plans/payment-readiness/lf-q1-supersession.md`
  (including its `8d04848` confirmation addendum) and `double-credit-reconciliation.md`.
  Independently re-ran the full `TestINVDEP1_*` matrix (17 tests in
  `internal/payments`, including 50-rep `-race` `D` and `K`), the two
  `internal/reconciliation` INV-DEP-1 tests, and the corrected
  `internal/idempotency` fixture, on a private scratch database
  (`PRIV_DB=pay_fh3rv_1`), independent of QA's own adjudication run. All passed; DB
  dropped afterward, worktree to be removed after this review lands.

## Verdict: **APPROVE**

No state-machine cell was found where a verified success still posts a second credit,
and none where a legitimate first success is wrongly disputed. Fallback/cascade
interaction, F-POOL-2 §29's durable-state mapping, and the reconciliation/audit
surfacing of a held second capture are all internally consistent with ADR 0095 §28/§29
and with the code at `a927aed`. One non-blocking operational recommendation is noted
under §3 below; it does not block this fix and does not touch the BLOCKED refund/
allocation path.

---

## 1. State machine — complete and deterministic; no double-credit, no false dispute

Traced every code path that can reach a deposit `succeeded`/`disputed` transition
(`receipt.go`'s `applyResolvedReceiptEvidence`/`applyDepositSuccessAndPost`,
`drive.go`'s phase-C `ErrorClassSucceeded` arm, `sweeper.go`'s poll `ErrorClassSucceeded`
arm which also serves T17 re-drive) against the §4.4-as-amended matrix (§28.5) and the
transition table (§28.4):

- All three T7/T13 evidence-application sites check, **in the documented order**:
  amount/asset mismatch → tombstone (`tombstoneExists`) → `resolvedForOtherDeposit` →
  post. Verified this order directly in `receipt.go` lines ~745-830 (both the
  `AttemptDeclined` and `AttemptSubmitting/Pending/Ambiguous` cells) and in the
  equivalent `drive.go`/`sweeper.go` arms. This matches §28.3 rule 1's ordering exactly,
  including the deliberate "tombstone before INV-DEP-1" rule so a PSP-refunded capture
  is never misreported as `pay_captured_unposted`.
- The single choke point (`postDepositSuccessOrDispute` wrapping `postDepositSuccess`)
  is the only place any of the three sites can reach `ledger.Post` for a deposit. The
  pre-check (`resolvedForOtherDeposit`) and the re-check inside `postDepositSuccess`
  (immediately before `ledger.Post`, same intent lock) are the *same* predicate, so no
  caller — including the legacy `InitiateDeposit` path — can reach a posting for an
  already-resolved intent. Confirmed `attempt.go`/`orchestrator.go` line references
  match the ADR's own citations (`resolvedForOtherDeposit` at
  `orchestrator.go:876`, wrapper at `:973`, re-check at `:1049`).
- **No cell found where a verified success still posts a second credit.** Every
  `succeeded`-match cell for a deposit either posts once (first success only, `created`→
  T15, `submitting/pending/ambiguous`→T7, `declined`→T13 *iff* first) or takes the
  no-post T10/T13d branch. The DB backstop (partial unique indexes on
  `payment_attempts` and `ledger_transactions`, migration 0107) is defense-in-depth
  behind the application choke point, exactly as ADR 0095 intends — not the primary
  control, which is correct given "the invariant is about money; only the ledger is
  authoritative."
- **No cell found where a legitimate first success is wrongly disputed.** The predicate
  is `id IS DISTINCT FROM $attemptID` for the attempt half (so the resolving attempt
  itself never counts against itself) and `idempotency_key <> $K` for the ledger half
  (so an exact redelivery under the same key is a no-op, not a dispute). `provider_id`
  is namespaced into both the attempt-level check (implicitly, since attempts are
  provider-scoped) and the ledger idempotency key (`providerID+":"+providerReference`),
  closing the cross-provider reference-collision risk the code's own comment at
  `orchestrator.go:1075-1085` flags and fixes. `provider_reference` is immutable once
  set on `payment_attempts` (0107's guard trigger, carried from 0101), so a single
  attempt can never present two different references across sync/callback/poll and
  spuriously trip `resolved_for_other` against itself.
- T17/re-drive: structurally confined to non-terminal attempts by the existing CHECK
  tying `next_action_at` to non-terminal state (0101), so it can only ever reach
  `declined` (via T13/T13d) among "terminal" states — matching §29.3's "T17 never
  state-changes a terminal attempt except `declined`" restated in `lf-q1-supersession.md`
  §4. `TestINVDEP1_O`/`_O2` (re-run, both PASS) cover the disputed-attempt re-drive
  no-post case directly.
- Independent re-run confirms this empirically, not just by inspection: 50/50 `-race`
  reps of `TestINVDEP1_D` (concurrent original+fallback) and `TestINVDEP1_K`
  (concurrent adversarial orderings) each land exactly one `succeeded` deposit attempt
  and one deposit ledger posting per intent, with the disputed sibling correctly
  logging `payments_multiple_success_for_intent_alert`. This corroborates, independently
  of, QA's own 100-rep adjudication in `qa-fh3-adjudication.md`.

## 2. Fallback/cascade

- **In-flight siblings.** At the resolving success, `created` siblings are rejected
  (`rejectCreatedSiblings`, called in all three evidence sites at the correct point —
  after a genuine T13/T7 credit, and explicitly *not* called on the T10/T13d no-post
  branch, since a disputed sibling was never itself the credited one and any leftover
  `created` sibling of *that* attempt's intent was already handled by the succeeding
  sibling's own rejection). `submitting/pending/ambiguous` siblings are correctly left
  alone to be polled to their own conclusion, landing on the T7 guard → T10 if they
  later report success — verified this is exactly what `TestINVDEP1_H`
  (three distinct references, all-but-first disputed) and `TestINVDEP1_J` (ambiguous
  sibling after resolution) exercise, both PASS.
- **Sibling rejection on success is race-safe against the cascade race the ADR calls
  out** (ledger-finance H4): a `created` sibling from a *different* prior decline,
  discovered concurrently with this success, is rejected in the *same* transaction as
  the success, before it can reach T2 and place a second real PSP charge. Present in
  all three sites (`receipt.go:811`, `drive.go` and `sweeper.go`'s equivalent calls).
- **Cascade creation after the intent is resolved.** T2's `NOT EXISTS(succeeded attempt
  for the same intent)` CAS predicate is unchanged and still sufficient: once the intent
  has a succeeded attempt, no new cascade child can ever be claimed for submission
  regardless of how many `created` rows exist. Cascade *insertion*
  (`insertCascadeAttemptIfEligible`) is gated on `cascadeEligible`, which reads the
  intent's own post-decline status — for a resolved intent this reports `succeeded`,
  which `cascadeEligible` treats as ineligible (§4.6, unchanged). No path was found that
  inserts a fresh cascade attempt for a resolved intent.
- **Reversals against a disputed (unposted) sibling correctly take the tombstone
  branch**, not a reversal-of-a-posting branch, since the disputed attempt's
  `LedgerTransactionID` is nil. Re-ran
  `TestINVDEP1_Inverted_RVLF_P6_ReversalOfDisputedSecondCaptureTakesTombstoneBranch`
  directly (PASS) — the reversed disputed attempt still occupies the INV-DEP-1 slot
  (a later sibling success after the reversal correctly lands on T10/T13d again, per
  §28.6 point 3), confirmed by the surrounding table's construction in
  `inv_dep1_matrix_integration_test.go`.

## 3. Operations

- **Finding the second capture.** A `disputed`/`multiple_success_for_intent` attempt
  carries, in its audit record (`payment.attempt_disputed`), the matched
  `provider_id`, `provider_reference`, `deposit_intent_id`, and (when one exists) the
  succeeded sibling's own `ledger_transaction_id` — enough for an operator with
  audit-log access to identify both the held capture and the one real posting it must
  be reconciled against. `pay_captured_unposted` independently surfaces the same
  attempt through the standard reconciliation-mismatch pipeline (ages every run,
  amount/asset present in the mismatch row, never in log lines — correct per S-5),
  and clears itself the moment a PSP-side reversal/tombstone appears, without any
  automatic remediation (`matchPayment`/§28.9 confirmed by direct read and by
  `TestINVDEP1_Recon_M_CapturedUnposted_ReplacesDuplicate`, re-run, PASS). Combined,
  the audit table plus the reconciliation-mismatches table give an operator with
  ordinary DB/reporting access everything needed to locate the second capture and
  initiate an out-of-band PSP-side refund request — the refund/allocation mechanics
  themselves correctly stay BLOCKED on HD-0095-1/LEDGER-MANUAL-ADJ-4EYES-1, which this
  review does not re-litigate.
- **P1 alerts.** `payments_multiple_success_for_intent_alert` and (when the backstop
  itself fires — a defect signal) `payments_deposit_intent_index_backstop_fired` are
  structured, allow-listed Error-level log lines with no amounts/references, matching
  §28.11 and security S-5. **Non-blocking recommendation, already tracked as a gap
  elsewhere in this codebase (the kill-switch alert has the identical status, so this
  is not a new gap introduced by FH-3):** delivery beyond the log line is
  NOT IMPLEMENTED, so nothing currently pages a human when a real, unrefunded second
  capture exists. Given this event represents actual player-attributable money sitting
  unposted, I'd prioritize wiring this specific alert (and `pay_captured_unposted`'s
  ageing) into whatever paging channel the platform adopts, ahead of the general
  kill-switch alert if the two are ever sequenced separately — but this is a
  recommendation for a later observability workstream, not a defect in FH-3, and does
  not block approval here.
- No dedicated staff-facing list of `disputed`/`pay_captured_unposted` attempts exists
  yet; §29.2 already records the read-only M1-queue view as a `RECOMMENDATION`
  (NOT IMPLEMENTED). I confirm that scope call — it's correctly deferred, not silently
  dropped, and doesn't block this fix.

## 4. F-POOL-2 §29 durable states — consistent with the implementation

Checked the §29.1 state map's deposit column and §29.2/§29.3 rules against the code
directly (not just against ledger-finance's own confirmation in
`lf-q1-supersession.md`'s addendum):

- **succeeded**: "at most one per intent (INV-DEP-1, §28)" — matches; enforced at both
  the choke point and the two migration-0107 indexes.
- **disputed**: "Attempt `disputed`... Exit is manual only (M1/M2, BLOCKED)" — matches;
  no code path transitions a `disputed` deposit attempt back to `succeeded` (0107's
  guard whitelist has no such pair; the only way into `succeeded` is the two listed
  `->succeeded` pairs, neither sourced from `disputed`).
- **Idempotency/retry/timeout/late-success/fallback rules (§29.3)**: each rule's code
  citation was independently located and matches its stated site (T7/T10 guard in the
  choke point; T13/T13d in the `AttemptDeclined` cell; T12's sibling-succeeded refusal
  both in application code and, as defense-in-depth, in the 0107 trigger's N3 clause at
  lines 208-221 of the up-migration; cascade eligibility's "intent is not succeeded"
  condition unchanged in `cascade.go`/`finalizeDeclined`'s return value).
- No inconsistency found between §29's prose and the implementation at `a927aed`.

## 5. Vendor-contract assumptions (real-PSP dependencies for INV-DEP-1 to hold)

These are **PROVIDER DEPENDENT** — the mock satisfies them by construction; a real PSP
adapter must be verified against each before INV-DEP-1's guarantees carry over from
"holds against the mock" to "holds in production":

1. **Reference uniqueness per real money movement.** `provider_reference` (stored as
   the ledger's `provider_tx_id`) must never be reused by the vendor across two
   distinct captures, even for different attempts/intents. If a vendor reuses a
   reference, `ledger.Post`'s own `(tenant_id, provider_id, provider_tx_id)` uniqueness
   will treat the second, genuinely distinct, capture as a replay of the first
   (`AlreadyPosted`) — an **under-credit**, the opposite failure mode from
   PAY-DOUBLE-CREDIT-1, and outside INV-DEP-1's own scope to detect.
2. **Reference stability for one attempt across evidence channels.** The same
   attempt's sync response, callback and `QueryStatus` result must all report the
   *same* `provider_reference` for the same underlying event. The platform enforces
   this from its own side (`provider_reference` is immutable once set on the attempt),
   but if the vendor itself returns a different id to the callback than it gave at
   submission time, the callback cannot resolve to the attempt at all (an unresolved-
   receipt/reconciliation gap), not a double-credit — still a vendor-contract risk
   worth confirming during sandbox integration.
3. **"Succeeded" means captured, not authorized.** For a two-phase (authorize +
   capture) vendor, the adapter must map only a genuine capture/settlement event to
   `OutcomeSucceeded`; treating an authorization as `succeeded` would let INV-DEP-1
   correctly dedupe against a real capture that has not, in fact, moved money yet.
4. **Idempotent submission handling.** The vendor must honor
   `external_idempotency_key = "pa:" + attempt.id` (or an equivalent merchant-reference
   idempotency key) so a network-level retry of the *same* submission is deduped
   provider-side rather than creating a second real charge under a new reference —
   INV-DEP-1 only protects the platform's own posting; it cannot stop the vendor from
   capturing twice at its own end (§28.10, correctly not claimed as prevented).
5. **Signed/verifiable callbacks bound to the provider.** INV-IO-14's provider-binding
   check must actually be satisfiable — the vendor must supply a verifiable signature
   or equivalent so a callback can be authenticated as genuinely originating from the
   named `provider_id` before its evidence is trusted by the §4.4 matrix at all.
6. **Reversal/refund events name the original reference.** The tombstone/reversal
   matching (`applyReversalReceiptEvidence`, `matchReversal`) keys off the vendor's own
   "original reference" field on a refund/chargeback event. A vendor that omits or
   mis-populates this field breaks both the tombstone-precedes-success check and
   `pay_captured_unposted`'s clearing condition.
7. **Statement/reconciliation feed granularity.** `pay_captured_unposted`'s clearing
   condition and `matchPayment`'s general matching assume one statement line per
   provider reference per kind; a vendor whose settlement file aggregates multiple
   captures under one line, or omits held/disputed captures entirely, would degrade
   (not defeat) reconciliation visibility of a held second capture.

All of the above are correctly out of scope for FH-3 itself (no real PSP is connected
today; the mock is explicitly labeled as such throughout), but they are the concrete
list I'd check item-by-item during the first real PSP's sandbox integration before
relying on INV-DEP-1's guarantees against that vendor.

## Labels

- INV-DEP-1 state machine (T7 guard, T13 first-success-only, T13d), the migration 0107
  backstops, and the `pay_captured_unposted` reconciliation kind: **IMPLEMENTED**,
  independently re-verified against `a927aed` on a private database.
- Real-PSP conformance to §5's vendor-contract list: **PROVIDER DEPENDENT / NOT
  IMPLEMENTED** (no vendor contract exists yet; correctly out of scope here).
- Final accounting treatment of a held second capture (suspense vs. no-post) and its
  refund/allocation: **BLOCKED** on HD-LEDGER-UNALLOC-1(B)/HD-0095-1/
  LEDGER-MANUAL-ADJ-4EYES-1, unchanged by this review — not re-litigated.
