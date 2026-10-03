_Reviewer: `ledger-finance`. Recorded verbatim by the orchestrator._

# Ledger-finance re-review — PRH-2 C (2026-10-03)

**Scope:** `prh2-c-dep-ref-validate` @ `c2d1fc1`. The fix is `34779b3`, the fixtures change is `c2d1fc1`, and `e8f56e0` merges main `d9a85c0`. Reviewed via `git archive`.

**Method:**
- Fresh private DB `lf_c2_20261003`, plus a control DB `lf_c0_20261003` at `fc0d18b`, under `pipefail`, with every log grepped for FAIL.
- **Both dropped** (no `lf_%` DB remains), and the exports and my throwaway probe deleted.
- No repo, role or credential change. DB access worked throughout.

## Verdict: ACCEPT

Every item I re-reviewed is closed. The fixture change is endorsed; keep the MOCK as it is. The disclosed residual is confirmed as money-neutral, with one scope note.

## Test runs (local, not CI)
- `-race -tags integration -count=1 -p 1 ./internal/payments/... ./internal/reconciliation/...`: `ok` (payments 582s, reconciliation 64s, statement 1s). FAIL count 0.

## Per item

| Item | Verdict | Evidence |
|---|---|---|
| **F-C1** T6 binds the validated reference | **CLOSED** | `MarkAmbiguousFromSubmittingBindingRef` sets `provider_reference = COALESCE(provider_reference, NULLIF($4,''))` on T6. Phase C's ambiguous branch passes `res.ProviderReference`, which has already passed `providerref` validation and the LF-6 binding pre-check. The **error path** is covered: `depositAdapterCall` now validates *before* returning the adapter error. An invalid reference parks; a valid one goes ambiguous and is bound. The **`sync_amount_mismatch` park binds** it (`bindRef`, guarded by `state IN (submitting,pending,ambiguous)`). The **conflict, invalid-reference and tombstone parks pass `""`** and never bind. **Probe re-run:** a sync success with a valid reference and no amount echo gives ambiguous with the reference bound and 0 postings. After one sweep, `QueryStatus` was called with exactly that reference (`calls=[lfp2-miss-…]`), the poll succeeded, the attempt became `succeeded`, there is **exactly 1** deposit posting, player cash is 5000, and the ledger is balanced. |
| **F-C2** tombstone T10 unified | **CLOSED** | The phase C tombstone branch now calls `parkDepositAttempt(…, TerminalReasonTombstonePrecedesSuccess, res.Outcome, "", …)`: one dispute audit plus the intent recompute, with the reference not bound (correct, since a tombstone already owns it). |
| **F-C3** `adapter_outcome` in the audit | **CLOSED** | `parkDepositAttempt` metadata carries `adapter_outcome` on all four parks, pinned by `TestDepSyncAmount_MismatchPark_BindsReferenceAndAuditsAdapterOutcome`. |
| **F-C5** §34.7 text | **CLOSED** | §34.7 now states the new T10s are **not alerted** and **not reported by reconciliation**, with coverage routed to PAY-RECON-PARKED-CAPTURE-1 (D, together with the F-C4 `ledger_transactions` binding extension) and P1 alerting to I-wire. |
| **§34.8 check order** | **CONFIRMED, matches the code** | (1) invalid reference, including on the error return (decided in `depositAdapterCall`); (2) binding conflict; (3) amount mismatch (sync success only; missing evidence is already ambiguous); (4) reversal tombstone; (5) the INV-DEP-1 choke point; (6) post. All four T10s go through `parkDepositAttempt`. None posts. The drift-zero result from my first round still holds. |

## Ruling on the fixture change (`refLessAmbiguousProvider`, `newInvDep1SetupWith`)

**ENDORSED. Keep the MOCK as it is; do not change it.**

- **Why the four tests broke, and why the wrapper is the right fix.** Each test's stated premise is "the attempt is ambiguous and its reference is **not known yet**":
  - `TestReceipt_Unresolved_DeferredThenAppliedOnceReferenceKnown` and `_PredatesSubmission_NeverApplied`: a callback naming a reference the platform does not yet hold must defer;
  - `TestA7_3_`: the same deferral setup, then a race;
  - `TestINVDEP1_I_`: "this attempt has no provider_reference yet", resolved by merchant reference.

  Once T6 binds the reference the MOCK returns on `Ambiguous`, that premise no longer holds. Removing the reference from the result reproduces it, and it models a real timeout (no response, so no reference). Both PSP behaviours are legitimate: "accepted, outcome unclear, here is a reference" and a timeout.
- **Why the MOCK should stay as it is.** Its reference-bearing `Ambiguous` is now the default path that exercises the F-C1 binding across the whole suite. Changing the MOCK would remove that coverage. The wrapper is opt-in, used only by the four tests whose premise needs it.
- **The tests were not weakened** (my mutants on the fixed tree, each reverted byte-identical):

| Mutant | Effect on the four tests |
|---|---|
| **MS4**: deferred-receipt application disabled inside `ApplyDeferredReceiptsForAttempt` | **KILLS** both `TestReceipt_*` tests (their subject) |
| **MS5**: T13d (declined → disputed `multiple_success_for_intent`) turned into a no-op | **KILLS** `TestINVDEP1_I_` (its subject: the late original after a timeout and fallback must dispute and not double-credit) |
| Call-site mutants (sweeper T9 deferred apply; phase C T4 deferred apply; choke-point pre-check bypassed) | Survive. Expected: the receipt tests call `MarkAccepted` and `ApplyDeferredReceiptsForAttempt` **directly** (unit-level subject), and the pre-check bypass is masked by `postDepositSuccess`'s own re-check (MC2). These call sites are covered elsewhere. |

- `TestA7_3_` also survives MS4. I checked it on a control DB at **`fc0d18b`** (before the fixture change): A7_3 **also** survives MS4 there. Its subject is the lock order and exactly-once application under the race, not "deferred receipts get applied". So the insensitivity is **pre-existing, not a weakening.**
- Optional, non-blocking: replace the ad-hoc wrapper with a named MOCK knob (for example an amount constant `MockAmountAmbiguousNoRef`), so future fixtures choose a timeout explicitly.

## PAY-DEFERRED-RECEIPT-SYNC-1 (disclosed residual): CONFIRMED, money-neutral
- Phase C's sync-success branch never calls `ApplyDeferredReceiptsForAttempt`; only the Pending (T4) branch and the sweeper's T9 Pending branch (`sweeper.go:440`) do.
- So a callback receipt deferred during phase B for a reference that then succeeds synchronously stays `deferred_unresolved`. Phase C posts exactly once.
- Any later redelivery resolves by reference to the succeeded attempt as `duplicate_effect`, with no posting.
- **Money is correct.** The effects are operational: the receipt ages into `pay_unresolved`, and it counts toward the per-(tenant, provider) `DeferredReceiptCap`.
- **Scope note for D:** the same pattern now also applies to an ambiguous attempt bound at T6 that a poll resolves directly to **Succeeded**. The sweeper's Succeeded branch does not drain deferred receipts either. It is equally money-neutral. Fold it into PAY-DEFERRED-RECEIPT-SYNC-1, so D drains deferred receipts on every transition that binds or resolves a reference: sync success, T6-bound poll success, and T9.

**Carried forward, unchanged:**
- PAY-RECON-PARKED-CAPTURE-1, including the F2 matcher and test spec from my first round, plus F-C4 → D;
- MA020-SYNC-MISMATCH-1 → K2 follow-up;
- P1 alerting for the new T10s → I-wire;
- PAY-PAYOUT-ERRREF-1 → F-pay.
