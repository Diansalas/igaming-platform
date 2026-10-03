_Reviewer: `ledger-finance`. Recorded verbatim by the orchestrator._

> **Orchestrator note (2026-10-03):** this re-review is a static review. The reviewer could NOT run the tests or re-kill X1/X4/X6 because the host ran out of disk (root at 97%, shared Postgres crashed into recovery). Condition **V1** (run the baseline and re-kill X1, X4, X6) is therefore OPEN and pre-merge for D2. The author's 23/23 evidence is not an independent verification. The disk condition is reported to the user; no cleanup has been done (not authorized).

# Ledger-finance re-review: PRH-2 D2 at `0c1e8ac` (PAY-RECON-PARKED-CAPTURE-1 plus the D2-1 merchant cross-check)

## Verdict: ACCEPT WITH CONDITIONS. I could not run the tests or re-kill X1, X4 and X6, because the host ran out of disk.

From reading the code, D2 now meets everything I asked for: P2, B1, N1, the D2-1 ruling (a), and test points (c) 1–9. The one pre-merge condition is that I still need to run the tests myself.

## The database failed, so I stopped as instructed
- **What I ran:** I exported `0c1e8ac` with `git archive` and confirmed `payment_statement.go` matched `git show` exactly. I built a fresh private DB, `lf_d2r2_20261003`, with no role or credential changes, and started the baseline `-race` run of `./internal/reconciliation/...`.
- **The failure:** the root filesystem filled up (`/dev/vda` showed 14M free).
  - Every build failed with `link: mapping output file failed: no space left on device`, so the baseline and all three mutants failed to build, not as test failures. None of those results count as evidence either way.
  - The shared Postgres went into crash recovery. My drop attempts failed with `FATAL: the database system is in recovery mode`.
- **What I did, all limited to my own resources:**
  - deleted my own 33M export;
  - waited, then retried the drop. It succeeded (`dropped lf_d2r2_20261003`), and no `lf_%` database remains.
  - Free space is now about 1.5G, at 96% used.
- **What I did not do:** I did not touch the Go cache, other agents' worktrees or any shared state, and I did not retry the tests. The D1 background agent shares this Postgres, and another disk-full crash would hit it too.
- **Action for you:** someone needs to free disk on the host. My 33M export was not the main consumer.

## Pre-merge condition
**V1 (PRE-MERGE):** once the disk is healthy, I need to run, on a fresh private DB:
- the baseline `-race` run for `internal/reconciliation/...`, which must show 0 FAIL and 0 DATA RACE, and also covers the second half of (c)4: the existing suite passes unchanged;
- X1, X4 and X6 with `-run 'TestD2_|TestINVDEP1_|TestPaymentStatement_'`, each restored byte-identical afterwards.

The script and exact mutant anchors are ready at `/tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad/lfd2r2.sh`, so it is about a 5-minute task. If you would rather accept the author's 23/23 evidence in place of my re-kill, that is your decision. Please record it as an exception, not as my independent verification.

## Static review results

### P2: the F13 wording. RESOLVED
All three sites now read "a PSP-initiated reversal/tombstone, or allocation (LEDGER-SUSPENSE-B-1); M1 only acknowledges": the bound in-run case, the bound standing case in `checkUnmatchedAttempts`, and the unbound case. The const doc comment was updated too.

The P2 text mutants (P2-TEXT-INRUN and P2-TEXT-STANDING) are recorded as killed, and every D2 finding asserts the text.

### B1 and N1 in §35.4: RESOLVED
- **B1** is now an explicit GATE: no first real PSP and no first non-MOCK statement source until both STANDING-1 has landed and the I-wire P1 alert exists and is delivered. The alert covers all five C/D T10 reasons.
- §35.4 says plainly: "**Status at D2: neither holds.** The I-wire P1 alert does **not** exist yet." Today these parks write only the `payment.attempt_disputed` audit row, which is acceptable only because they are reachable solely through the MOCK adapter and the MOCK source. This is what I required.
- **N1** is recorded accurately, and STANDING-1 is named as the replacement.
- §35.2 now correctly separates the one-line case from the two-line case.

### D2-1 `checkMerchantAttribution` against my ruling (a): CONFORMS

| Ruling (a) point | Code | OK |
|---|---|---|
| Applies only to lines resolved by provider or settlement reference with a non-empty merchant reference | `if !byMerchant && l.merchant != ""`. This covers both byRef and bySettlement. | yes |
| Merchant reference names a different attempt: `pay_reference_mismatch check=merchant` against A, with detail naming B | final `m.r.add`, detail `names attempt=<B>` | yes |
| Names the other operation: mismatch | `b.operation != op` | yes |
| Names no attempt: mismatch, "names no platform attempt" | `b == nil` | yes |
| Compares attempt identity, not strings | `b == a`, using the `byMerchant` lookup | yes |
| B unbound park, line succeeded, not cleared on `l.ref`: `pay_captured_unposted` against B | final `if`, using `capturedUnpostedRef(l.ref)` | yes |
| B not recorded in `matchedBy` | it is not | yes |
| Additive: A's checks still run | the function returns into `matchPayment`, which continues with settlement, asset, amount and status checks | yes |
| Payouts included | yes, including lines resolved by settlement reference | yes |

Two notes, neither blocking:
- `byMerchant` is scoped by `tenant_id = $1 AND provider_id = $2` and keyed by `merchant_reference` across both operations. The other-operation branch depends on that, and it works.
- The lookup assumes merchant references are unique per provider. The older merchant-reference resolution path already relied on that, so D2 adds no new risk.

### Tests against my ruling (c) 1–9: CONFORM

| (c) | Test | OK |
|---|---|---|
| 1 | TestD2_7/c1: exact counts of 1 `pay_reference_mismatch` and 1 CU, so no `pay_duplicate` or missing-record finding can slip in; attributed to A, names B; B's CU names the attempt the line resolved to | yes |
| 2 | TestD2_7/c2 (reversal line on R) and c2 (tombstone on R, with a check that exactly one tombstone exists): `check=merchant` stays and B's CU clears | yes, with nit T1 below |
| 3 | TestD2_7/c3: `pay_status_mismatch` on A, plus `check=merchant` naming B, plus B's CU, with exact counts | yes |
| 4 | TestD2_7/c4: merchant reference equal to A's, or empty, gives no findings. The second half (the existing suite passes unchanged) is the package run, so it waits on V1. | yes / V1 |
| 5 | TestD2_8: names no platform attempt; names an attempt of the other operation (payout) | yes |
| 6 | TestD2_9: a payout line resolved by settlement reference names P2. The control case is clean, and P2's own line also catches X4 (a consumed B would give `pay_duplicate`). | yes |
| 7 | TestD2_7/c7: B is matched through the merchant path by the R2 line. The ordering guard (`r2 > r`) fixed in `0c9b3c9` makes a consumed B observable. | yes |
| 8 | `d2AssertBalanced` (SUM(D) = SUM(C) and `RunLedgerVsProjection` reports 0) is called in every subtest | yes |
| 9 | X1–X6 recorded as killed. The X6 analysis is correct: for a deposit resolved by reference, `l.ref == a.providerRef` by construction, so only TestD2_9b (a payout resolved by settlement reference) can tell the two apart. | per the evidence file; my re-kill is V1 |

**T1 (LOW, not blocking):** TestD2_7/c2 checks the reversal case with `d2OneMerchant` and `d2NoCU`, but not with an exact `d2Expect` count map, so an extra spurious finding there would go unnoticed. Add `d2Expect` the next time this file is touched.

## Your question: should the unbound `pay_captured_unposted` rule also fire for a PAYOUT B (TestD2_9b)? KEEP IT.

For a payout, the finding means "the PSP says it paid B's player, and the platform posted no `withdrawal_completed`." That exposure is real and arguably worse than the deposit case:
- money has left the house's PSP balance with no ledger debit;
- if the hold on B is later released back to the player, the player has both the payout and the released funds, which is a double payout.

Restricting the rule to deposits would turn a loud finding into a silent one, and I veto that direction. The rule fires only when a succeeded line explicitly names B's merchant reference and nothing clears it on the line's reference, so there is no noise from correct lines.

Follow-ups, none pre-merge:
- **PO-1 (LOW, documentation):** the finding name sounds deposit-specific ("captured"). §35.2 should define its payout meaning ("paid out, completion unposted"). For payouts, the resolution is a PSP recall/reversal, or posting the completion against the hold through the governed path; the deposit-style suspense allocation is not the main route. The detail text can say this later; it does not block.
- **PO-2 (for payments to confirm, binding before the B1 gate):** a payout parked in T10 with an unbound reason must keep its hold. The hold must never be released automatically while this exposure can exist. Payments should confirm this with a test, or point to an existing one, before the first real PSP. Today such a park is reachable only by fixture: TestD2_9b creates it with `ApplyDisputeFromNonTerminal`. So for now the rule is a defensive guard.

## Status of earlier conditions
Still outstanding and, as you said, outside this review:
- P1, the cross-package reason pin: pre-merge for whichever of D1 or D2 merges second;
- the end-to-end poll test;
- B2 (D1 keeps the bound reference) and B3 (the returned reference Y, with clearing on either reference);
- B4 (widening MA020 to the two poll reasons).

## Labels
- In-run detection for bound and unbound parks, standing detection for bound parks, and the D2-1 merchant cross-check: **IMPLEMENTED**, pending V1.
- Standing detection for unbound parks: **NOT IMPLEMENTED**, gated by B1 in §35.4.
- I-wire P1 alert for the T10 parks: **NOT IMPLEMENTED**.
- Statement source: **MOCK**.

## Relevant paths
- /home/user/igaming-platform/internal/reconciliation/payment_statement.go (`checkMerchantAttribution`, and its call from `matchPayment`)
- /home/user/igaming-platform/internal/reconciliation/prh2_d2_merchant_crosscheck_integration_test.go
- /home/user/igaming-platform/docs/decisions/0095-provider-io-transaction-boundary-and-payment-contract.md (§35.2, §35.4)
- /home/user/igaming-platform/docs/plans/payment-readiness/evidence/prh2-d2-mutation-kill.txt
- The V1 script, ready to re-run: /tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad/lfd2r2.sh. The failed-build logs are next to it as `lfd2r2_*.log`.
