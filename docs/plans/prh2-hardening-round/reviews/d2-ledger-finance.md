_Reviewer: `ledger-finance`. Recorded verbatim by the orchestrator._

> **Orchestrator correction:** ruling 3 says the C T10 parks "already raise P1 on park creation through the I-wire alert path". That is not yet true. I-wire is not implemented, and P1 visibility for the C T10s is an open I-wire condition (ALERT-DELIVERY-1). B1 is therefore read as: the I-wire P1 alert for those parks must be in place, and STANDING-1 must land, before the first real PSP or non-MOCK statement source.

# Ledger-finance review: PRH-2 D2 (PAY-RECON-PARKED-CAPTURE-1)

**What I reviewed:** branch `prh2-d2-recon-parked-capture`, HEAD `0005aa7`, base `564c515`, commits `32dc4f0`, `9ff8757` and `0005aa7`. Files:
- `internal/reconciliation/payment_statement.go`
- `internal/reconciliation/prh2_d2_parked_capture_integration_test.go`
- ADR 0095 §35
- `evidence/prh2-d2-mutation-kill.txt`

## Verdict: ACCEPT WITH CONDITIONS

There are 2 pre-merge items (P1 and P2). The other conditions are binding follow-ups. Nothing in D2 posts to the ledger, mutates a balance or changes the schema. D2 only adds detection, so it cannot break SUM(D) = SUM(C), and it holds in every D2 test.

## How I verified it
- **Setup:** I exported `0005aa7` with `git archive` to the scratchpad and ran it against a fresh private DB, `lf_d2r_20261003`, built with `priv_db.sh` and `priv_test.sh` (`PRIV_SRC` exported). I made no role or credential changes, and DB access worked throughout.
- **Baseline:** I ran `go test -race` with `set -o pipefail`. `internal/reconciliation` passed (65.1s) and `internal/reconciliation/statement` passed. `grep FAIL` found 0.
- **Mutants:** I re-ran 3 mutants and all 3 were killed. After each one I restored the file and checked it with `cmp` against `git show 0005aa7:...`, and it was byte-identical every time.
  - **M-PRED** (both sites reverted to `multiple_success_for_intent` only): killed by TestD2_1, TestD2_2 and TestD2_3.
  - **M-UNBOUND-CLEAR-ATTEMPT-REF** (`capturedUnpostedRef(l.ref)` replaced with `capturedUnpostedRef(a.providerRef)`): killed by TestD2_4 and TestD2_5.
  - **LF own mutant** (the unbound case deleted from `matchPayment`): killed by TestD2_4 and TestD2_5.
  - Together these confirm the evidence file's 17/17 claim for the mutants that matter most.
- **Cleanup:** I dropped the DB (`dropped lf_d2r_20261003`) and confirmed no `lf_%` DB remains. The scratch export is removed.

## Rulings

### 1. Does D2 meet my C ruling's (c) items 1–7? Yes.

| (c) | Requirement | Where it is covered | Met |
|---|---|---|---|
| 1 | Bound park plus a matched succeeded line gives `pay_captured_unposted` in-run, including a non-succeeded line not being flagged | TestD2_1, including subtests `poll_reference_mismatch_line_carries_echo` and `non_succeeded_line_is_not_flagged_in_run` | yes |
| 2 | Standing finding when the line is outside the coverage window | TestD2_2 | yes |
| 3 | Clearing only via a reversal line or tombstone on the bound reference; `investigation_status=resolved` and a tombstone on another reference do not clear it | TestD2_3 | yes |
| 4 | Conflict parks: flagged on a merchant-resolved succeeded line, an unbound reason on a reference-holding attempt is flagged, and deposit-bound plus a second line still gives `pay_duplicate` | TestD2_4 | yes |
| 5 | Invalid-reference park: the line with the invalid reference is refused and the run failure is audited as P1, and a merchant-resolved succeeded line is flagged | TestD2_5 | yes |
| 6 | SUM(D) = SUM(C) and `RunLedgerVsProjection` reports 0 mismatches in every scenario | shared assertion helper | yes |
| 7 | Mutation kill on the reason predicate | M-PRED (re-killed above); TestD2_6 shows existing reasons are unchanged | yes |

### 2. Widening the unbound-park rule so it is not gated on byMerchant: ACCEPTED
The ungated rule is the safe direction. It flags any succeeded line on an unbound park whose line reference has no reversal or tombstone, whichever path resolved it (byRef, bySettlement or byMerchant). Gating on byMerchant would have hidden a captured-unposted deposit that resolved by reference to a conflict-parked attempt. That would be a silent miss, which this platform must never tolerate. The 32dc4f0 battery showed the gate was an equivalent mutant, and `9ff8757` pins the ungated behaviour with a test. Being louder here is correct.

### 3. Deferring standing coverage for unbound parks to PAY-RECON-PARKED-CAPTURE-STANDING-1: ACCEPTED, as binding
An unbound park has no reliable reference of its own. Standing detection would have to come from persisted statement-line evidence, which does not exist yet. This is acceptable because:
- in-run detection covers every run where the line appears;
- the C T10 parks already raise P1 on park creation through the I-wire alert path.

**Binding (B1):** STANDING-1 must land before the first real PSP or the first non-MOCK statement source, whichever comes first. Until then, the I-wire P1 alert for the C T10s must stay in place. ADR 0095 §35 should record both points as an explicit gate, not as a "future consideration".

### 4. Clearing semantics: ACCEPTED, with a residual noted
- **Bound parks** clear only on a reversal or tombstone on the attempt's bound reference. That is correct, because the bound reference is the money's identity.
- **Unbound parks** clear on a reversal or tombstone on the line's reference. I accept this, with a known residual. For `provider_reference_conflict`, the line's reference belongs to the attempt that already holds it. A tombstone on that reference therefore clears the in-run flag for the parked attempt too, so attribution to the parked attempt is approximate. Because detection is in-run only and the holder's own posting state is reconciled independently, this approximation cannot hide unposted money: a tombstone on the reference means the PSP itself has reversed that capture. Record this residual in §35. STANDING-1 should replace it with clearing against a per-park record of evidence.

### 5. Is it safe to treat D1's `poll_amount_mismatch` and `poll_reference_mismatch` as bound reasons?
Yes, with conditions. It is semantically right: both reasons describe a capture the provider confirmed against our bound attempt, with no matching posting. The risk is that the coupling fails quietly. If D1 writes a reason string that differs by a single character (casing, a suffix, `poll_ref_mismatch`), D2 sends it to the generic disputed branch, and the captured-unposted finding disappears without any test failing. The D2 fixtures insert the strings themselves, so they cannot catch drift in D1. The following conditions apply:

- **P1 (PRE-MERGE, for whichever of D1 or D2 merges second):** add a cross-package classification pin test. It must enumerate every deposit terminal reason the payments package can write and assert that reconciliation classifies each one as bound, unbound or explicitly excluded, failing on any unclassified reason. The best source is exported constants or a list in payments, used by both D1 and the pin, rather than literal strings in two places.
- **B2 (binding on D1):** D1 must keep the bound reference on its poll parks and use `ApplyDisputeFromNonTerminal`, which does not clear the reference. If the bound reference is empty, `capturedUnpostedRef("")` will check the wrong key.
- **B3 (binding, D1 together with a D2 follow-up):** with `poll_reference_mismatch`, the provider may have captured under the returned reference Y, not our bound reference X. If clearing only accepts X, a legitimate PSP reversal on Y will never clear the finding. That failure is loud, so it is safe, but it is operational noise. D1 must audit or persist Y on the park. The follow-up must then accept a reversal or tombstone on either X or Y for that reason.

### 6. Widening MA020-SYNC-MISMATCH-1 to the two poll reasons: YES
The exposure is the same as `sync_amount_mismatch`: provider-captured funds with no player credit. Without this, `player_open_payment_exposure` (K2) under-reports for poll parks. Clearing should stay tombstone-only on the attempt's bound reference. B3's either-reference rule should be added for `poll_reference_mismatch` when it lands. **Binding (B4):** do it in the same change set as D1, or immediately after it.

## Findings and conditions

| ID | Severity | Finding / condition | Pre-merge? |
|---|---|---|---|
| P1 | MEDIUM | No cross-package pin on terminal-reason strings, so a drift between D1 and D2 would silently disable detection for the poll reasons. Add the classification pin test (ruling 5). | **PRE-MERGE** (whichever of D1/D2 merges second) |
| P2 | LOW | The detail text at both bound sites, `matchPayment` and `checkUnmatchedAttempts`, still says "M1/allocation (BLOCKED)". Per F13/LF-3 and C-19, M1 only acknowledges a finding, and allocation is LEDGER-SUSPENSE-B-1. Change it to the same text as the unbound site: "...a PSP-initiated reversal/tombstone, or allocation (LEDGER-SUSPENSE-B-1); M1 only acknowledges". Operators read this text to decide what to do. | **PRE-MERGE** (text only) |
| B1 | binding | PAY-RECON-PARKED-CAPTURE-STANDING-1 must land before the first real PSP or non-MOCK statement source; the I-wire P1 alert for the C T10s stays until then; record the gate in §35. | follow-up (gated) |
| B2 | binding | D1 keeps the bound reference on its poll parks and uses `ApplyDisputeFromNonTerminal`. | D1 review |
| B3 | binding | D1 records the returned reference Y for `poll_reference_mismatch`; clearing then accepts X or Y. | D1 plus follow-up |
| B4 | binding | Widen MA020-SYNC-MISMATCH-1 to `poll_amount_mismatch` and `poll_reference_mismatch`. | with or after D1 |
| N1 | note | Unbound-park clearing on the line's reference is approximate for conflict parks (ruling 4). Document it in §35; STANDING-1 fixes it. | no |
| N2 | note | M1 remains NOT IMPLEMENTED (K3). D2 adds no acknowledgement path, which is correct. | no |

## Labels
- PAY-RECON-PARKED-CAPTURE-1, in-run bound and unbound detection plus standing bound detection: **IMPLEMENTED** (once P1 and P2 are done).
- Standing detection for unbound parks: **NOT IMPLEMENTED** (deferred to STANDING-1, gated by B1).
- Statement source: **MOCK**.

## Relevant paths
- /home/user/igaming-platform/internal/reconciliation/payment_statement.go
- /home/user/igaming-platform/internal/reconciliation/prh2_d2_parked_capture_integration_test.go
- /home/user/igaming-platform/evidence/prh2-d2-mutation-kill.txt
- ADR 0095 §35 under /home/user/igaming-platform/docs/decisions/
