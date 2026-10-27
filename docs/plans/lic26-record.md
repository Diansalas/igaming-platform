# lic26 record: final confirmation fixes (ledger-finance NEW-1 / N-9 / NEW-2 / NEW-3, security F-1..F-7)

Branch `lic26-final-fixes` from integ-c `6457e32` (migration head 0186). Software capability only; the only screening provider is the MOCK; no regulatory approval is claimed.

## 0. Which migration

- **Migration 0181 was edited IN PLACE again** for every AML object (the gate, the queue table, the officer functions, the worker functions, two new read functions). It is unreleased. **Runbook: any dev or staging database that applied 0181 before these edits must be REBUILT**, never rolled back with its down and never patched by hand (the checksum verification of `cmd/migrate` fails loudly otherwise).
- **Migration 0188 is new** (`0188_lic26_aml_enforcement_decisions_parked_index`): an index on `aml_enforcement_decisions (tenant_id, operation, decided_at DESC)`. The table belongs to migration 0141, which is older than 0181, so the index cannot be an in-place edit. 0187 was left to the F1 agent. Its down drops the index.
- `deploy/init-app-role.sql` and the 0181 grant loop gain `aml_screening_parked_overdue(uuid, timestamptz)`; the 0181 down drops it.

## 1. Findings addressed

| Finding | Change | Label |
|---|---|---|
| NEW-1 MEDIUM / F-1 HIGH (a) | `aml_screening_record`: a record under the key of a sanctions-class needs_officer request must be a match that **names the sanctions list and places a hold** (`p_decision <> 'none'`). The pre-existing check only refused a clear / non-sanctions outcome. **What was actually open** (found while writing the test): a decision `none` on a match was already refused by the `aml_screening_results` CHECKs (a match is never "no action"; a sanctions list forces `hold_all_value_movement`), so the real hole was a `confirmed` outcome that names only the PEP list: a legal record whose hold class is derived from the LIST, i.e. `manual` (one officer lifts it), which counted as "resolving" the sanctions request. That is now refused (AM006). | IMPLEMENTED |
| F-1 (request_resolved) | `aml_screening_request_resolved`: a sanctions-class request counts as resolved only through a result that placed a hold of class `confirmed_sanctions` / `sanctions_potential`. `aml_screening_review_cleared_at` likewise requires a sanctions-class hold for a lifted hold of a sanctions-class request, and never counts a clear of one. | IMPLEMENTED (defence in depth: see the mutation note) |
| NEW-1 (b) | `aml_screening_gate_denies`: when the latest unreviewed match is sanctions-class (`v_sanc`), only a worker `done` clear, an auto-reviewed re-flag, or the lift of a `confirmed_sanctions` / `sanctions_potential` hold (the proof-bound four-eyes lift) satisfies the gate. An officer clear (single officer, no four-eyes) and the lift of a manual-class hold no longer count. PEP and non-sanctions paths are unchanged. | IMPLEMENTED |
| F-2 | (a) The auto-review lookup in `aml_screening_request_finish` requires the earlier match to have the SAME outcome and to cover every list now named (`q.outcome = p_result AND p_match_lists <@ q.match_lists`): a potential match later upgraded to confirmed, or one that gains a list, is a new finding (no auto-review, so the attention alert fires and the person is held). (b) The ORIGINAL officer review must be younger than **four re-screen intervals of the request's jurisdiction, capped at 180 days (180 days where no screening policy exists)**; the age is read from the root of the chain (`COALESCE(reviewed_from_request_id, id)`), so chained auto-reviews do not extend it. This is an ENGINEERING bound, not a compliance value. (c) `matchRefPairs` accepts only `^[A-Za-z0-9._-]{1,128}$` references (anything else yields no fingerprint, so an officer reviews it again) and `matchFingerprint` (now `match-fp:v2`) length-prefixes every pair, so two different pair sets can never share a byte string. | IMPLEMENTED |
| F-3 | New columns `requested_by`, `requested_by_person_id`, `request_reason_code` on `aml_screening_requests`, set on every `repl:` row (CHECK: a `repl:` key carries all three), immutable (guard). The static allow-list entry in `internal/httpserver/staff_mutation_audit_static_test.go` now states what is true (the new row carries the officer in both replace paths; the cancelled row keeps its own columns when a request is replaced). | IMPLEMENTED |
| F-4 | `aml_screening_request_replace` refuses a `claimed` request whose lease has not expired (AT003), and the replacement keeps the ORIGINAL `created_at`, so a replacement does not reset the clock of `aml_screening_gate_overdue` (the trapped-person alert). | IMPLEMENTED |
| F-5 | The lifted-hold evidence in the gate applies the re-screen interval to the screening's `recorded_at`, not to the lift time (`lifted_at` stays the ordering test against the last match). `aml_screening_review_cleared_at` returns the screening time for both kinds. | IMPLEMENTED |
| F-6 | `TestLic24_Static_Pre0182HookIsIntegrationOnly` pins the IDENTIFIER: in non-test files `pre0182SchemaHook` may appear only as the declaration plus the single `.Load()` in `restriction.go`, and as `.Swap(true)` / `.Store(prev)` in the integration hook file; `CompareAndSwap`, `&pre0182SchemaHook` or any alias fails. Comment lines are skipped. | IMPLEMENTED |
| N-9 (production path) | `Sweeper.sweepTenant` (the path wired at `cmd/platform-api/main.go`) now calls `reportScreeningParked` first, in its own read-only tenant transaction (`aml_screening_parked_overdue`). It logs counts only (`parked`, `overdue`, never an id), adds `ScreeningParked` / `ScreeningParkedOverdue` to `SweepPassStats` and to the per-pass log line, and raises `aml.screening_gate_trapped` (p1, `ScopeKindPlatform`, attribute `persons` = the overdue COUNT, nothing else) when persons have been parked longer than the gating policy's `gate_alert_after_seconds`. "Parked" run start = the first screening-only hold after the person's last non-parked payout decision, so a decision that is not a screening-only hold restarts the clock. A failing or panicking read is contained, counted in `Errors` and never stops the sweep. `RunScreeningWorkerLoop` no longer discards `RunPass`'s result: it logs the pass counts (including `trapped` and `parked_screening`). `aml.RaiseScreeningTrapped` is shared by the worker and the sweeper. | IMPLEMENTED |
| N-9 (index) | Migration 0188 (above). | IMPLEMENTED |
| N-9 / F-7 (42883) | The `SQLSTATE 42883` swallow and `isUndefinedFunction` are removed from `internal/payments/sweeper.go`. `SweepStats.ScreeningParked` and the parked read in the test-only `RunOnce` are removed with them (nothing used them); there is no old-schema switch left to keep. | IMPLEMENTED |
| F-7 (tipping-off) | The alert kind is platform-routed (`ScopeKindPlatform`) and carries a count only; the log lines carry counts only (a test asserts the person and account ids never appear). `aml_screening_parked_overdue` / `_summary` return counts only and are session-tenant bound. Neither is exposed on any tenant-staff or payments-operator route. | IMPLEMENTED |
| NEW-2 LOW | Documented below (section 3). | DOCUMENTED |
| NEW-3 INFO | Documented below (section 3). | DOCUMENTED |

Negative tests added for replace: another tenant's person and another tenant's request (nothing queued in either tenant), a missing proof, a proof of another officer, a proof for another reason code / person / operation (each AP001 / AP004; a correct proof reaches AT004, so the refusals are about the proof), and an officer who is the subject (ErrFourEyes, nothing queued).

## 2. Deviation recorded: `hold_all_value_movement` is not required literally

The coordinator proposed that a record resolving a sanctions-class request carry `decision = 'hold_all_value_movement'`. The enforced rule is: the record must name the **sanctions list** and place a hold (`p_decision <> 'none'`). The table CHECKs already force `hold_all_value_movement` for any sanctions-list match and for a `confirmed` outcome, so with the sanctions list required the two coincide for every row the database can hold; requiring the literal decision in the function as well would add a second statement of the same CHECK. If a later change relaxes the CHECK, the function still requires a hold.

## 3. Notes for the lic23 record and runbook

- **NEW-2 (LOW): "defaults preserve behaviour" for payout freshness holds only for tenants WITHOUT approved operating bases.** For a tenant that HAS bases, `payoutinstrument.FreshnessJurisdiction` resolves the governing basis for the player's scope. A basis-tenant player with no verified residence country, or with no `payout_instrument_verification_max_age` row for the basis jurisdiction, now errors fail-closed (the payout is not dispatched), and a synthetic dev verification then has no expiry. **Remedy: author the `payout_instrument_verification_max_age` row for the basis jurisdiction, or make the residence determination (residence maker-checker).** Nothing is defaulted for such a player.
- **NEW-3 (INFO): the match fingerprint is worker-supplied.** `p_match_fingerprint` is computed by the screening worker and passed to `aml_screening_request_finish`; the database cannot recompute it (it never sees the vendor's match references). A forged fingerprint therefore has the same trust as a forged worker clear, which the worker API already accepts from a holder of the runtime credential and a tenant session (ADR 0116 F1: no system-actor attestation). The bound is the environment anchor (a worker result is accepted only where `database_environment` says development, staging or test) plus the new F-2 rules: a forged fingerprint can reuse only an officer-reviewed match of the SAME outcome and lists, for the same person, for at most four re-screen intervals.
- **Runbook additions** (`docs/runbooks/aml-screening-triggers.md`): replacing a claimed request needs its lease to have expired (a worker that is merely slow is not cancelled under itself); a replacement keeps the original age; a repeated `aml.screening_gate_trapped` alert with no worker pass log lines means the screening worker is down: restart it, then use the replace function for requests that stay stale.

## 4. Verification

See section 6 of the hand-off report; results are recorded there with the exact commands. Mutation checks (each applied to the live function or the source, killed by the named test, then restored):

| Mutation | Killed by |
|---|---|
| Drop the "names the sanctions list" requirement in `aml_screening_record` | `TestLic26_F1_SanctionsRequestIsResolvedOnlyByASanctionsHold` (the confirmed-naming-only-PEP record) |
| Officer-clear branch without `AND NOT v_sanc` | `TestLic26_NEW1_GateIgnoresOfficerClearAndManualLiftForASanctionsMatch` |
| Lifted-hold branch without the sanctions-hold condition | same test (manual lift) |
| Auto-review lookup without the outcome / lists condition | `TestLic26_F2_UpgradedMatchIsNotAutoReviewed` |
| Auto-review age bound replaced by 9999 days | `TestLic26_F2_ReviewExpiresAfterFourRescreenIntervals` |
| Interval applied from `lifted_at` | `TestLic26_F5_LiftedHoldIntervalRunsFromTheScreening` |
| Replace without the live-lease refusal | `TestLic26_F3F4_ReplaceAuditLeaseAndClock` |
| Replacement with a fresh `created_at` | same test |
| Parked run start ignoring the last non-parked decision | `TestLic26_N9_ParkedOverdue` |
| Sweeper without the `reportScreeningParked` call | `TestLic26_N9_ProductionSweepReadsAndLogsParkedCountsOnly` and the alert test |
| Sweeper raising the alert without the overdue condition | both sweeper tests |
| Sweeper alerting with the parked count instead of the overdue count | `TestLic26_N9_ProductionSweepRaisesTheTrappedAlertWithCountsOnly` |
| `CompareAndSwap(false, true)` on the hook in a non-test file | `TestLic24_Static_Pre0182HookIsIntegrationOnly` |

## 5. Pin changes (declared)

- `TestLic23_N4_ReplaceStalePendingRequest_AndQueueForAPersonWithNone`: the stale claimed request is now claimed with the 5 s lease minimum (was 3600 s) and the test waits 6 s before replacing, because F-4 refuses to replace a live lease. No assertion was weakened.
- `internal/aml/catalog_integration_test.go`: `aml_screening_parked_overdue` added to the two function allow-lists (proof-less, session-tenant-bound, runtime-executable).
- The `staff_mutation_audit_static_test.go` allow-list text for the replace route was corrected (F-3). The route set is unchanged.

## 6. Residuals (not hidden)

1. **`aml_screening_request_resolved`'s own sanctions-hold-class condition is masked by the record check**: with the record check intact the state it guards cannot be created through any API, and a mutation of it alone is not killed by a test. It is defence in depth for rows written outside the function.
2. **The sweeper alert threshold is not exercised end to end in `internal/payments`**: authoring a trigger policy needs the officer-proof machinery of `internal/aml`. The overdue computation is tested there against a real policy (`TestLic26_N9_ParkedOverdue`, which authors a gating policy); the sweeper tests inject the counts through `testScreeningParked`, and the real read is exercised through RunPass with no policy (overdue 0).
3. **A dead worker AND a dead sweeper** is still invisible (no third observer); `ALERT-DELIVERY-1` stays open (nobody is paged unless a route exists).
4. The review-age bound is an engineering value (four re-screen intervals, 180-day cap); the compliance value, if any, is a licence-owner decision.
5. The worker still accepts a worker-supplied fingerprint (NEW-3).
