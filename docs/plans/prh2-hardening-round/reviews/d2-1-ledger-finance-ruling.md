_Reviewer: `ledger-finance`. Recorded verbatim by the orchestrator._

> **Orchestrator decision (2026-10-03):** the cross-check lands **in D2 before merge**, which is the option ledger-finance prefers, using test list (c) 1-9. So no PAY-RECON-MERCHANT-CROSSCHECK-1 follow-up is registered and there is no PARTIALLY IMPLEMENTED label.
> **Correction:** (b) says creating a conflict park "already raises a P1 through I-wire". It does not. I-wire is not implemented, and P1 visibility for the C/D T10 parks remains an open ALERT-DELIVERY-1 condition.

# Ledger-finance ruling: D2-1 (merchant-reference cross-check on lines resolved by reference)

**Summary:** Yes, add the check. It may land as a follow-up rather than in D2, but only if D2 merges on four conditions (listed under (b)). The deadline is the same as STANDING-1 and no later: before the first real PSP or non-MOCK statement source.

This is a ruling only; I made no code changes. I checked it against `matchPayment` at `0005aa7`. Provider/settlement reference is tried first and merchant reference last. The only merchant cross-check is `byMerchant && a.providerRef != l.ref` (~964), so a line found by provider reference never has its merchant reference checked. `reviews/d2-code-review.md` was not on my local `main`, so I relied on your description of the probe.

My D2 report said unbound-park clearing on the line's reference could not hide unposted money. This scenario shows that was wrong: with one line, nothing is flagged at all. My verdict (ACCEPT WITH CONDITIONS) stands, with the conditions under (b) added.

## (a) Should a line found by reference, whose merchant reference names a different attempt, raise `pay_reference_mismatch`? YES

In this case the PSP is telling us whose capture R is (B's), and we credited someone else (A's player). That is a money misattribution. The ledger stays balanced, so the drift check cannot see it. Reconciliation's job is to catch exactly this, and today it finds nothing (0 mismatches).

The rule:
- **When it applies:** the line was found by provider reference or settlement reference, and its merchant reference is non-empty.
- **Merchant reference names another attempt:** if `m.byMerchant[l.merchant]` is a different attempt than `a`, raise `MismatchKindPayReferenceMismatch` with `check=merchant` against `a`. The detail names the other attempt.
- **Merchant reference names an attempt of the other operation** (deposit vs. payout): raise the same mismatch.
- **Merchant reference names no attempt of this provider:** raise the same mismatch, with detail "names no platform attempt". We issue merchant references, so a value we never issued contradicts the attribution just as much. If an adapter cannot echo our merchant reference, it must leave the field empty, not fill it.
- **Compare attempt identity, not strings:** use `byMerchant[l.merchant] != a`, not a string comparison against one field.
- **Also check the named attempt B:**
  - **When:** B is `disputed` with an unbound reason (`isUnboundParkReason`), the line is `succeeded`, and `capturedUnpostedRef(l.ref)` is true.
  - **What:** also raise `pay_captured_unposted` against B.
  - **Matching:** do not record B in `matchedBy`. The line is still matched to A, and a separate line for B must not get a false `pay_duplicate`.
  - **Why:** this is the case the reviewer said D2's unbound rule should catch.
- **A's normal checks still run:** amount, asset, status and the captured-unposted check on A keep running. The new mismatch is added on top; it does not replace them.
- **Payouts:** the check applies to payout lines too, for the same operation, including lines found through `bySettlement`.

**Effect on every deposit line:** I accept it. A line whose merchant reference matches its attempt, or is empty, does not change. The only lines affected are ones where the PSP contradicts us, and we want those flagged. The existing reconciliation suite must pass with no new findings. That is how we show the change does not fire on correct lines.

## (b) In D2 before merge, or as a follow-up? A follow-up is allowed, with a hard deadline

Reasons a follow-up is acceptable:
- The gap was there before D2.
- The statement source is still MOCK, so no real PSP statement can be misattributed yet.
- Creating a conflict park already raises a P1 through I-wire. So B's exposure is not completely silent; only the reconciliation finding is missing.

Deadline: register it as **PAY-RECON-MERCHANT-CROSSCHECK-1**. It must land together with or before PAY-RECON-PARKED-CAPTURE-STANDING-1, and in any case before the first real PSP or non-MOCK statement source, whichever comes first. STANDING-1 cannot close until it lands.

Conditions for D2 to merge without the check (all **PRE-MERGE**):
1. **Fix §35.2.** It must say that a single line (ref R, merchant B) for a conflict-parked attempt is not detected when the holder succeeded (0 mismatches). When the holder is pending, the only finding is a `pay_status_mismatch` against the holder, A. `pay_duplicate` happens only when there are two lines. Cite PAY-RECON-MERCHANT-CROSSCHECK-1 and its deadline.
2. **Label honestly.** D2's detection of conflict parks is **PARTIALLY IMPLEMENTED**. Bound parks and invalid-reference parks stay IMPLEMENTED.
3. **Keep the I-wire alert.** The P1 alert on conflict-park creation must not be removed or downgraded before CROSSCHECK-1 lands.
4. **Add a gap test.** Turn the reviewer's probe into a test that asserts today's behaviour (0 mismatches with the holder succeeded). Name and comment it so CROSSCHECK-1 has to flip it. The gap must not be fixed silently, and must not come back once fixed.

If the D2 author would rather land it now, I prefer that, and the test list below applies unchanged.

## (c) What the test must assert (whenever it lands)

1. **The core case.** A succeeded and bound to R; B parked `provider_reference_conflict` on R; one line with ref R, merchant B, status succeeded. Expect exactly one `pay_reference_mismatch check=merchant` attributed to `attempt=A`, with detail naming B, plus exactly one `pay_captured_unposted` against B. Expect no `pay_duplicate` and no `pay_missing_platform_record`.
2. **Clearing.** The same case with a tombstone on R, or with a reversal line naming R in the run. Expect the `check=merchant` mismatch still raised and no `pay_captured_unposted` for B.
3. **Holder pending.** Expect `pay_status_mismatch` against A and `pay_reference_mismatch check=merchant` naming B. The finding is loud and also names the right attempt.
4. **Negatives.** A merchant reference that matches A, or an empty one, produces no `check=merchant` finding. The full existing reconciliation suite passes unchanged, with no new findings on existing fixtures.
5. **Merchant reference naming no attempt**, or naming an attempt of the other operation: a `check=merchant` mismatch.
6. **Payout version.** A payout line found by settlement reference whose merchant reference names a different payout attempt gives a `check=merchant` mismatch.
7. **B is not consumed.** With a second line (ref R2, merchant B), B is matched through the normal merchant path and gets its usual checks. There is no false `pay_duplicate` from the first line.
8. **Ledger invariants.** SUM(D) = SUM(C), and `RunLedgerVsProjection` reports 0 mismatches in every scenario. This uses the existing D2 helper.
9. **Mutants, each killed.**
   - Remove the new cross-check.
   - Gate it back on `byMerchant`.
   - Compare against A's own merchant reference instead of resolving `byMerchant`.
   - Record B in `matchedBy`.
   - Drop the B captured-unposted step.
   - Clear B on `a.providerRef` instead of `l.ref`.

   Record the results in the evidence file the same way as `prh2-d2-mutation-kill.txt`.

## Relevant paths
- /home/user/igaming-platform/internal/reconciliation/payment_statement.go (`matchPayment`: resolution ~937–950, the `byMerchant`-only cross-check ~964)
- /home/user/igaming-platform/internal/reconciliation/prh2_d2_parked_capture_integration_test.go
- ADR 0095 §35.2, under /home/user/igaming-platform/docs/decisions/
