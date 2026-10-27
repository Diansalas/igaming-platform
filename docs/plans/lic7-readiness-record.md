# lic7: launch readiness items R-RISK-RULES-LEGACY and R-KYC-POLICY

Branch `lic7-readiness` (from the merged licensing integration branch, migration head 0179, NO migration added).
Closes ledger-finance review condition C1 (`docs/plans/licensing-ledger-finance-review-record.md`) and the "active tenant with zero active KYC
policies" readiness item of audit G9 (`docs/plans/licensing-audit-enforcement.md`). Software capability only: nothing here is a legal or
regulatory position, and no KYC threshold or default is invented.

## Status labels

| Deliverable | Label |
|---|---|
| `R-RISK-RULES-LEGACY` (Go, DB read inside the launchgov read-only tenant tx, savepoint-isolated) | IMPLEMENTED |
| `R-KYC-POLICY` (same) | IMPLEMENTED |
| `SnapshotVersion` 5 -> 6, OpenAPI code description + `refs`, Back Office labels/notes/rule ids + tests, runbook | IMPLEMENTED |
| Integration tests (written, vetted with `-tags integration`; NOT run by the author: orchestrator runs them) | IMPLEMENTED, UNRUN |
| Detection of legacy rules is a readiness item only (no auto-disable, no platform report endpoint) | by design |

## What the items do

Both are ACKNOWLEDGEABLE informational checks like every other item (ADR 0112 5.2): evaluated for activate / reactivate / ratify, a
non-pass must be acknowledged by the request (exact set), neither is a hard precondition (a unit test pins that `hard.go`, `decide.go`
and `execute.go` never name them). Both change the readiness hash, so a request filed against version 5 is stale and must be re-requested.

### R-RISK-RULES-LEGACY (`internal/launchgov/readiness_legacy.go`)

Counts ACTIVE rules (`status = 'active'` and not past `effective_until`; a rule with a future `effective_from` counts, it will go live)
visible to the tenant: its own rows plus platform-wide rows (`tenant_id = $1 OR tenant_id IS NULL`, explicit predicate on top of the
`tenant_and_platform_read` RLS policy of migration 0041) that (a) carry a non-empty `jurisdiction_code` or (b) have `operation = 'deposit'`.

Precise behaviour read from `internal/risk` (not guessed):

- (a) `Evaluate` runs `missingScopeContext` over EVERY effective rule of the operation BEFORE matching and ignores the rule's brand and
  every other dimension. A jurisdiction-scoped rule with a request carrying no `jurisdiction_code` returns `ErrMissingJurisdiction`: the
  evaluation is REFUSED (not "rule does not apply"). All callers (casino, sportsbook, bonus) take the code from the player-scoped resolver,
  which is `unresolved(no_signal)` for every player (HDR-J-1 / HDR-J-3 pending), and the withdrawal gate supplies none. So the rule refuses
  every evaluation of its operation; for a `withdrawal` rule that is every withdrawal request and payout dispatch (funds stay held). Only if a
  caller supplied a different code would the rule merely never fire (`matches` is false). Neither enforces the rule as written.
- (b) The deposit path never calls `risk.Evaluate`, so a deposit rule is stored and never enforced (false assurance, G7).

Result and display data: pass `ok`; fail `jurisdiction_scoped_rules_present`, `deposit_rules_present` or `both_present` (closed words, no ids or
counts in the detail code); `not_evaluable` `read_error`. Counts go to `counts` (`risk_rules_legacy_jurisdiction_scoped`,
`risk_rules_legacy_deposit_operation`, full counts) and rule ids only (no other column, no PII) to a new live-display `refs` map
(`risk_rules_legacy_jurisdiction_scoped`, `risk_rules_legacy_deposit_operation`), capped at 50 per key. `counts` and `refs` are not hashed or persisted.

### R-KYC-POLICY

KYC policies are PLATFORM-wide rows keyed by `licensing_jurisdiction_id` (`kyc_enforcement_policies`, migration 0100; read policy `USING (true)`),
selected through the tenant's licence jurisdiction (`jurisdiction.ResolveLicensingChain`, the same chain `kyc.EvaluateEnforcement` uses). There is
no per-tenant policy row, so "zero active policies for the tenant" is "zero for its licence jurisdiction". Another jurisdiction's policies never
count (tested). Per operation, the data model supports: DEPOSIT = `cumulative_deposit` rows (per asset); CASINO_PLAY / SPORTSBOOK_PLAY =
`play` rows by `play_operation`; WITHDRAWAL has NO policy row (structural rule compiled in, ADR 0096 s3.2 point 1), so it is never dormant and is
not counted; `edd_amount` / `registration_tier` cannot be activated (trigger) and are not counted.

Result: fail `no_licence_jurisdiction` (no licence: KYC is `not_required`, nothing can apply), fail `no_active_policy` (zero active for all three
operations); pass `ok_partial_operations_dormant` (some operation has none; per-operation active counts in `counts`: `kyc_active_policies_deposit`,
`_casino_play`, `_sportsbook_play`, a 0 is dormant); pass `ok`; `not_evaluable` `read_error`. No threshold value is read, invented or defaulted.

## Decisions made (interim, reversible; flag if the owner disagrees)

1. **Fail only on ZERO policies, as briefed.** A jurisdiction with, say, a deposit policy but no casino-play policy PASSES with detail
   `ok_partial_operations_dormant` and the per-operation zeros in `counts`. This is the weaker reading. The stricter alternative (fail when ANY of
   the three operations is dormant, which is what `kyc.ListDormantJurisdictionTriggers` reports) is a one-line change in `kycPolicyFrom`; I did not
   take it because the brief said "zero". Decision for the orchestrator/owner.
2. **No licence jurisdiction = fail**, not not_evaluable: KYC then resolves `not_required` for the tenant, which is exactly the dormant state.
3. **A rule with a future `effective_from` counts as active** (it goes live without any further action). An expired or disabled rule does not.
4. **Rule ids are shown to tenant-scoped viewers**, including the ids of platform-wide rules visible to the tenant under RLS anyway. Ids are
   UUIDs only. If the owner wants platform-rule ids hidden from tenant staff, coarsen `refs` like `coarsenForTenant` does for AML (not done).
5. **Savepoints**: both reads run in a savepoint so a failing read degrades only its item to `not_evaluable` and never poisons the readiness tx.

## Existing assertion changed intentionally

`internal/launchgov/readiness_aml_test.go` `TestAMLChecks_CodesPinnedAndAcknowledged` pinned `SnapshotVersion == 5`. It now pins 6 (the check set
changed; message extended). No other assertion was weakened or removed. No existing test pins the full list of item codes (they use
`NonPassing()` dynamically); the new unit test `TestLic7Items_AcknowledgeableNotHard` and the integration tests add the pins.

## Files

- `internal/launchgov/readiness_legacy.go` (new), `readiness.go` (codes wired in `configChecks`, `SnapshotVersion = 6`, `Readiness.Refs`,
  `EvalInput.live`), `hard.go` (sets `in.live`).
- `internal/httpserver/launch_authorisation_routes.go` (`refs` in the readiness DTO).
- `docs/api/openapi/platform-api.yaml` (`LaunchCheck.code` description, `LaunchReadiness.refs`), `docs/runbooks/launch-and-suspension.md`.
- Back Office: `backoffice/src/features/launch/labels.ts` (titles + fixed interim notes), `LaunchBits.tsx` (note and rule ids under a non-passing
  item), `backoffice/src/api/launch.ts` (`refs`), `lic7Labels.test.tsx`.
- Tests: `internal/launchgov/readiness_legacy_test.go` (unit), `readiness_legacy_integration_test.go` (integration).

## Tests

Unit (run): pass/fail arms of both pure functions, ids-only and cap, acknowledgeable-not-hard (source pin + non-passing set + hash change).
Integration (written; orchestrator runs): risk pass/fail, tenant isolation (tenant B does not see A's rules or ids), platform-wide rules count for
every tenant, disabled/expired do not count, brand subject, hash change, request refused as `ErrUnacknowledged` without the ack and filed with it;
KYC no-licence, zero policies, draft not counted, another jurisdiction's policy not counted, per-operation counts, acknowledgeable not hard.
All run through the launchgov Service over the igaming_runtime pool (non-BYPASSRLS), i.e. RLS-correct inside the launchgov read-only tenant tx.
The risk tests use `newScratchWorld` because platform-wide risk rules are visible to every tenant.

## Residuals

- The items detect; nothing disables rules or authors policies. Disabling is existing `DisableRule` (risk admin); KYC values are HD-KYC-1..8.
- Raw runtime INSERT of such a rule is still possible (security F-9); the readiness item is the detection, as C1 asked.
- `refs` is capped at 50 ids; the count is exact.

## Proposed text for the orchestrator (not applied; governance/ADRs untouched)

- `docs/plans/licensing-ledger-finance-review-record.md` C1: "Closed by lic7: readiness item `R-RISK-RULES-LEGACY` (snapshot version 6) detects ACTIVE
  `risk_rules` with a jurisdiction_code or operation `deposit` visible to the tenant; acknowledgeable, not a hard precondition. Detection only: the rules
  are not changed."
- `docs/plans/licensing-audit-enforcement.md` G9: "Readiness item `R-KYC-POLICY` (lic7, snapshot version 6) added: fails when the tenant's licence jurisdiction
  has no active KYC enforcement policy. Threshold values remain HD-KYC-1..8 (human decision)."
- Task registry: lic7 IMPLEMENTED (integration tests UNRUN by the author until the orchestrator runs them); decision pending: fail-on-any-dormant-operation (D1).
- ADR 0112 5.1 check list: add `R-RISK-RULES-LEGACY` and `R-KYC-POLICY`; SnapshotVersion 6.
