# Ledger-finance DELTA review: PRH-2 K3, prh2-k3-impl @ 31e4501 (main a642e8c merged; previous review 041fb55 had F-1..F-10, PM-1..PM-4)

READ-ONLY. Nothing was committed, pushed or merged. No DB role, password or shared-infra changes. I used private scratch DBs `lf_k3d` (suite) and `lf_k3m` (mutants), built from a `git archive 31e4501` export. Both DBs and both exports were dropped afterwards. Free disk at the end was 4.4G.

## VERDICT: ACCEPT WITH CONDITIONS (for merge)

The money core is unchanged since 041fb55. The reconciliation clearing defect F-1 is fixed correctly, and the F-2..F-6 tests are present and non-vacuous. One new test gap (D-1) and a few honesty/wording items in docs that are outside my verbatim blocks (D-2..D-5) are left. None needs a schema or posting change. D-1, D-2 and D-3 are pre-merge. D-4..D-6 can go in the same docs commit or before PAY-K3-STATEMENT-SOURCE-WIRING-1.

## Runs (local, private DB, not CI)
- `go test -race -tags integration -count=1 -p 1 -run TestK3 ./internal/payments/ ./internal/reconciliation/`: PASS (payments 651s, reconciliation 3.7s; includes C-42b, Y01..Y11, C-34b).
- The C-12 class after the fixes (PM-4): `-race -count=20 -run 'TestK3_C12|TestK3_C26|TestK3_Y07' ./internal/payments/`: PASS (392s). The §27.6 verification lists only `-count=1`. Add this run to the record (D-6).
- Mutants: 9 of mine against an export of 31e4501, each restored and checked with `cmp` (all OK).

| id | mutant | result |
|---|---|---|
| LFD1 | F01 re-run: `resolvesTo` = union (holder check dropped) | KILLED (C-42b) |
| LFD2 | F03 re-run: provider-reference arm dropped | KILLED (C-42b) |
| LFD3 | own: holder lookup keyed on `deposit` instead of `l.kind` | KILLED (C-42b) |
| LFD4 | G01x/GK re-run: `sumFor` ignores causation | KILLED (Y05) |
| LFD5 | G02x/GM re-run: (c2) raised only by confirming lines | KILLED (Y06) |
| LFD6 | G04x/GD re-run: staff `FOR SHARE` removed | KILLED (Y07) |
| LFD7 | own/GU: `holder_attempt` removed from the format | invalid (compile error), replaced by LFD7b |
| LFD7b | own/GU: `holder_attempt` always `none` | KILLED (C-34b) |
| LFD8 | own: (d) raising narrowed to `resolvesTo` | **SURVIVED** (finding D-1) |

## Status of the previous findings
- **F-1 (CLOSED).** `payMatcher.resolvesTo` (`internal/reconciliation/payment_statement_k3.go:355-364`) matches my rule. A line clears an attempt only in two cases:
  - its reference equals the attempt's `providerRef`; or
  - its merchant reference is non-empty and equals the attempt's, and `m.byRef[l.kind+"\x00"+l.ref]` is nil or is the attempt itself.

  The "is the attempt itself" case is equivalent to (a), because `byRef` indexes only non-empty refs. `byRef` loads every attempt of (tenant, provider), whatever its state (`loadPlatform`, no state filter), so a terminal holder also counts. The function is applied only to clearing (c) (`:500`). The raising predicates for (c2), (d) and S1 still use the broad `linesFor` union, as I ruled. The in-run confirmation metric resolves by reference first, so it stays consistent.

  C-42b is non-vacuous:
  - the borrowed line is a MOCK line in a MOCK-only world, so it is eligible, and under the union it would clear (LFD1 killed);
  - the persisted second run is asserted;
  - the confirmation arms cover reference-only and merchant-only with no holder (LFD2, LFD3 killed).
- **F-2 (CLOSED).** Y05 covers both (d) and (c2) with an unrelated executed `debit_player` whose causation is another resolution's transaction. It is at least as large as the exposure, and A's own recovery then clears (non-vacuity). LFD4 killed.
- **F-3 (CLOSED).** Y06 covers a different-amount line, and a MOCK line after a real import. LFD5 killed.
- **F-4 (CLOSED).** Y07 uses `testHookResolutionAfterShareLocks`. A revoke and a staff suspension both block during the execution. The revoke succeeds after commit, which proves it is not refused for another reason. LFD6 killed (and G03x is in the evidence file). Nit: the hook comment at `manual_resolution.go:619-621` still says "C-12b"; it should say `TestK3_Y07`.
- **F-5 (CLOSED).** `TestK3_C34b` asserts the import, `line_no`, `is_mock` and `holder_attempt=<holder id>` on a later persisted-only run. LFD7b killed.
- **F-6 (CLOSED).** The hint at `:54` is verbatim, and Y11 pins it.
- **F-7 / PM-4 wording.**
  - ADR 0095 §39.4 block C: verbatim. Block D (`reconciliation-model.md` (i) and amendment items 1-7): verbatim.
  - ADR 0095 §35.4 block A: all five replacements applied. The GATE item 1 text and "Status at D2: neither holds" were kept rather than replaced, with an explicit "superseded by this bullet for item 1" note. I accept this; it preserves history.
  - §39.3 block B: see D-3.
- **F-8/F-9/F-10:** optional. They are recorded as deferred in ADR 0101 §27.6. Accepted.

## Findings (delta)
- **D-1 LOW-MEDIUM, REQUIRED test (pre-merge recommended; at the latest before PAY-K3-STATEMENT-SOURCE-WIRING-1).** Mutant LFD8 survived: narrowing the (d) `pay_declared_not_paid_but_paid` raising loop (`payment_statement_k3.go:544-548`) to `m.resolvesTo(l, a)` fails no test. That would silence the double-payout signal whenever the PSP's success line carries another payout's reference plus A's merchant reference (the C-42b shape). The raising-broad side of the F-1 rule is unpinned for (d). The (c2) `anySucceeded` predicate has the same shape. Fix: add a C-42c test. A is declared not paid (or declared paid, compensated); B is another live payout; a line has B's reference and A's merchant reference with status `succeeded`. Assert that (d) (or (c2)) is raised for A, in that run and in a persisted-only later run. (d) cannot arise in production today (O-4), which is why this is not HIGH.
- **D-2 MEDIUM, REQUIRED doc (operator runbook money statement is false; pre-existing at 041fb55, missed in my earlier review).** `docs/runbooks/operational-runbooks.md` (K3 section, "psp_clearing residual") says: "A declared-paid payout does not move the `psp_clearing` house account until the provider's settlement is reconciled; the clearing balance can sit off by the declared amount." That is wrong. Step B (`withdrawal.Complete`) credits `psp_clearing` by the withdrawn amount at execution; this was verified in the previous review, with `psp_clearing` +700 asserted. The text also contradicts `reconciliation-model.md` amendment item 6. Replace with: "**psp_clearing residual.** An executed declare-paid credits `psp_clearing` by the withdrawn amount at execution, without a PSP confirmation. Each open `pay_declared_paid_unconfirmed` finding itemises that credit as an explained difference. If the PSP never paid, the amount stays in `psp_clearing` until a WITHDRAWAL-REVERSAL-1 posting or a governed correction; it is never netted away. Do not fix it by hand."
- **D-3 LOW, REQUIRED doc.** In ADR 0095 §39.3 the old clause "a confirming line needs reference, amount and asset to match (D-5)." was left in front of my sentence. It now contradicts the merchant-reference resolution order that follows it. Delete that clause, so that the bullet reads "... A MOCK import may confirm or clear only when no non-MOCK import exists (D-4/RC-3). A confirming line must resolve to the attempt by provider reference, ...".
- **D-4 LOW, REQUIRED doc/text (pre-existing).** Two texts say a confirming line must come from a non-MOCK source:
  - the runbook (`pay_declared_paid_unconfirmed`: "It clears only on a line from a non-MOCK import." and "(same reference, amount AND asset)");
  - the `declaredPaidUnconfirmedResolutionHint` constant (`payment_statement_k3.go:53`, "non-MOCK source").

  The code (D-4/RC-3, C-42b) clears on a MOCK line when no non-MOCK import exists for (tenant, provider), and it resolves by merchant reference when no other payout holds the line's reference. Runbook replacement: "declared paid but no confirming statement line in any eligible persisted import (payout, succeeded, resolved to the attempt by provider reference, or by merchant reference when no other payout holds the line's reference, amount AND asset equal). An import is eligible if it is non-MOCK, or if no non-MOCK import exists for the provider. Chase the provider statement." Hint replacement: "resolution: a confirming statement line from an eligible import (payout, succeeded, resolved to this attempt, amount and asset equal) or a withdrawal reversal (WITHDRAWAL-REVERSAL-1); a compensating credit annotates but does not clear".

  Also in the runbook, for (d), say the K2 debit's causation must be the resolution's `withdrawal_failed` transaction (otherwise it does not clear; Y05).
- **D-5 LOW, REQUIRED label (real-money precondition mislabelled).**
  - ADR 0101 §27.6 lists PAY-K3-STATEMENT-SOURCE-WIRING-1 under "Deferred (docs only)", alongside optional hardening. It is not docs-only or optional. It is a real-money precondition (my RM-1): populate `DefaultStatementSources` from the same list handed to `RunSchedulerLoop`, and extend the O-4 refusal to `m2_declare_paid`.
  - The item is not in `docs/governance/task-registry.md`.
  - Runbook point 2 ("Until that lands, treat every M2 as unmonitored") does not name the item. It also omits that the MOCK provider's stream is scheduled, so (c), (c2) and S1-S4 do run for MOCK (§39.4).

  Fix: move the item in §27.6 to a "Required before real-PSP / real-money enablement" line (with RM-2..RM-6 of my previous review), register it in the task registry with that gate, and name it in runbook point 2 and in the §27.1 table row.
- **D-6 NIT.** Three record fixes:
  - §27.6 names the F-1 mutant `Y-F1`, but the evidence file calls it F01..F03. Align them.
  - Record the post-fix C-12 class run (above) in §27.6. §27.2's `-count=50` refers to 041fb55.
  - Fix the hook comment (F-4 nit above).

## Honest labels (checked)
- No delivered-alert claim was found. ADR 0095 §39.4, ADR 0101 §27.1/§27.5, the runbook ("Nothing pages anyone"), `observability-and-alerting.md` ("every alert is `unrouted`"), `payment-orchestration.md`, `withdrawal-state-machine.md` and `security-architecture.md` all say NOT IMPLEMENTED / ALERT-DELIVERY-1 open.
- No real-PSP claim was found. Everything is labelled `IMPLEMENTED` against MOCK, and real statement matching and real-PSP behaviour are `PROVIDER DEPENDENT`.
- The ADR 0095 §35.4 GATE is stated CLOSED.
- The registry is unwired and `m2_declare_not_paid` is refused:
  - `DefaultStatementSources` is empty in the binary; its only reference outside `internal/payments` is `httpserver/payment_force_resolution_routes.go:45`, and nothing calls `Register`.
  - The refusal sits at submission (`manual_resolution.go:474-479`) and at execution (`:887-893`).
  - It is tested through `k3Opts{noStatementSource: true}` (`k3_m2_integration_test.go:460`).

  The labelling problem is only D-5.
- ADR 0101 §26.1 O-K3 is factually correct against migration 0026: `withdrawal_requests_enforce_immutable_fields` freezes tenant, brand, player, wallet, asset, amount, idempotency key and requested_at, not `state`, `provider_id` or `provider_reference`. The launch flag (column discipline on `withdrawal_requests` under an acting session before real money) is appropriate. From the ledger side there is no posting exposure: the deferred verifier re-checks the withdrawal state and release link at commit, and `Complete`/`Fail` post exactly the two fenced legs. I accept it as a residual.

## Money core delta (31e4501 vs 041fb55)
- Migration 0115 changes only the `SET search_path` pins on all 18 functions and the R-2 `kind IN ('m2_declare_paid','m2_declare_not_paid')` tightening. That tightening is consistent with the reader, which selects M2 only.
- The route changes are audit-only (the SQLSTATE in the denied audit).
- `payment_statement_k3.go` changes only the F-1 and F-6 lines.
- No change to `ledger.Post` usage, posting legs, idempotency keys, fences, the MA020 exemption or lock order. The SUM(debits) = SUM(credits) and projection-drift assertions in the K3 suite pass.

## Conditions
- **Pre-merge:** D-1 (test), D-2 (runbook money text), D-3 (§39.3 clause).
- **Same docs commit or before wiring:** D-4, D-5, D-6.
- **Unchanged real-money preconditions:** RM-1 (now PAY-K3-STATEMENT-SOURCE-WIRING-1) through RM-7 from my 041fb55 review.
