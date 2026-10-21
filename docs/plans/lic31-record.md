# lic31 record: Back Office market-configuration filing and AML screening policy

Branch `lic31-backoffice-market-config` (from integ-c be47960, migration head 0189). Frontend only (`/backoffice`), no API, OpenAPI
or migration change. `backoffice/e2e` untouched. Owner requirement: markets and arrangements are added and changed later through
the Back Office, under governed and audited workflows, without code changes or releases.

## Deliverables and labels

| Item | Label |
|---|---|
| Licence country ceiling filing (kind `licence_ceiling_version`; enable / disable / withdraw) on the licence page, plus per-row entry | PARTIALLY IMPLEMENTED (mock-tested only) |
| Operating-country policy filing (kind `operating_policy_version`; tenant / brand / operation rungs; enable / disable / withdraw) on the tenant licensing page, plus per-row entry | PARTIALLY IMPLEMENTED (mock-tested only) |
| Tenant jurisdiction configuration filing (kind `tenant_jurisdiction_config_version`; set / close) on the tenant licensing page, plus per-row set / close | PARTIALLY IMPLEMENTED (mock-tested only) |
| Jurisdiction creation (`POST /v1/admin/jurisdictions`, exists) on the jurisdictions page | PARTIALLY IMPLEMENTED (mock-tested only). NOT four-eyes gated by the server; see decisions |
| Change detail screen renders the three kinds in words (sentence, approval path, fields) | PARTIALLY IMPLEMENTED (mock-tested only) |
| AML screening policy read + authoring (`GET/POST .../screening/policy`) as a new "Screening policy" tab on `/aml/screening`, with the safety rules in words | PARTIALLY IMPLEMENTED (mock-tested only) |
| Exact wording for the closed refusal codes (all 24 of `aml.RefusalCodes`) on every screening operation, including the existing trigger policy and resolution screens | PARTIALLY IMPLEMENTED (mock-tested only) |

"Mock-tested only" means: vitest + MSW against hand-written fixtures that follow the OpenAPI. No test here has touched the real API,
the real database classification (`licensing_change_classify`), real step-up, the real actor-proof issuer or the real officer quorum.

## What was built

- `api/marketConfig.ts`: `fileMarketChange` (POST `/v1/admin/licensing-changes`, body keys `kind`, `payload`, `reason_code`),
  `createJurisdiction`, payload types. A test compares the declared payload keys with the json tags of the Go payload structs
  (`kinds.go`) so drift fails.
- `features/licensing/MarketDialogs.tsx`: `CeilingDialog`, `PolicyDialog`, `ConfigDialog`, `JurisdictionDialog`. Every choice starts empty (no
  preselected country, rung, action, jurisdiction or reference). Country entry is two upper-case letters, validated client side, the
  server checks the code is assigned. The authorization reference is labelled "operator-supplied" with the note that it is not
  verified. A disclaimer states that nothing here is legal approval. The widening rule (two approvers) versus narrowing (one
  platform administrator, immediate) is stated in words and a per-selection class hint is shown (a hint only; the database classifies).
  Result classification is by body shape (`classifyBasisFiling`): `executed:false` and status `requested` is "Pending approval";
  `executed:true` and status `executed` is "Applied at once"; anything else is "not confirmed". Errors use the fixed wording of
  `errors.ts` (server message never shown). Step-up is the existing `apiFetch` interceptor (prompt, one re-send of the same body).
- Config set loads the version in force into the form when a jurisdiction is chosen (so a blank field is a deliberate removal), and
  the hint mirrors the runbook table (first version, ruleset change, added currency / method, removed geo block = widening).
- `MarketBits.tsx` + `ChangeDetailPage`: the three kinds are rendered as a sentence and a field list; hostile text stays text.
- `api/amlScreeningPolicy.ts`, `aml/ScreeningPolicyTab.tsx`, `aml/screeningPolicyRules.ts`: read and authoring. Fields: jurisdiction,
  lists (none preselected), `rescreen_interval_seconds`, `backfill_grace_seconds` (an explicit 0 is a stated value),
  `effective_from`, legal review reference, reason code. Integers come from digit-only text (`JSON.parse`), no float maths.
  The form blocks: no list, interval below 3600 or above 31536000, grace above 31536000, bad reference or reason, and a version that
  is certainly looser than the policy in force (list removed, longer interval or grace, earlier effective time). It only WARNS for the
  "twice the gate alert threshold" rule (the number comes from the trigger policy read) and for an effective time outside the window;
  the server decides those.
- `screeningErrors.ts`: `REFUSAL_TEXT` covers exactly the server's closed set. A test reads `internal/aml/refusal.go` and the OpenAPI
  enum and fails if the three sets differ. A code in `check_codes` now produces the exact sentence for every op and any of 400 / 403 /
  409. A bare `guard_refused` keeps the old wording (existing assertion unchanged).
- Refactor: `ScreeningError`, `JurisdictionInput`, `isLegalReference` moved from `ScreeningPage.tsx` to `ScreeningBits.tsx` (behaviour
  unchanged). The "Interval and grace" note on the trigger tab used to say the API had no route for them; it now points to the new tab.

## Intentional behaviour changes to existing tests (nothing weakened silently)

- `licensing.test.tsx` "detail ... read-only ceilings": the old assertion was "no button matching /ceiling/i". The ceilings are no longer
  read-only, so it now asserts exactly one ceiling control (the filing entry) and the per-row entry for the current version.
- Card titles "(read-only)" were removed from the ceilings, configuration and policy cards (no test depended on them).

## Verification run

`npm run build` OK; `npx vitest run src` 80 files / 1055 tests pass; `docs/api/openapi/platform-api.yaml` parses (not edited).
`tsc -b` clean. node_modules symlink removed at the end.

## Tests that exist (all mock-based)

RBAC (7 non-platform roles, zero requests on the licence, tenant and jurisdiction screens; zero requests on `/aml/screening` for non-officer
roles), payload pins for every kind and action, pending / applied / unconfirmed classification, fixed error wording, step-up single re-send
with identical body, 403 degradation, XSS, empty-start pins, OpenAPI contract coverage for both new api modules, refusal-code coverage
against the Go table and OpenAPI enum, rules unit tests.

## E2E flows the orchestrator must run against the real API (not run here)

1. platform_admin A files a ceiling ENABLE for a country on a synthetic licence: expect 201 `executed:false`, UI "Pending approval";
   platform_admin B (different Person) approves, platform_admin C approves: expect execution and a new current ceiling row; A cannot
   approve (LC032); a 24 h stale approval refuses at execution.
2. Ceiling DISABLE and WITHDRAW by A alone: expect 201 `executed:true` and "Applied at once". Confirm the audit rows
   (`licensing_change.requested/executed` plus the domain writer rows) exist for each.
3. Tenant policy rung ENABLE of a country not enabled by the licence ceiling nor an approved operating basis: expect a refusal at filing
   (precheck) shown in fixed wording. Then with a ceiling or basis in place: expect pending, two approvals, then a current policy row.
4. Brand-rung and operation-rung policy: disable (immediate), withdraw (widening, needs a reference; verify the server refuses it
   without one and that the UI blocks it first), brand chosen from the tenant's brands (a foreign brand id is refused as not found).
   Operation code vocabulary: confirm which codes the server accepts (the UI only checks shape).
5. Tenant jurisdiction configuration: first version (widening), edit removing a currency (narrowing, immediate), adding a currency
   (widening), removing a geo block (widening), close (narrowing). Confirm a currency that is not an active asset is refused. Confirm
   the class hint in the dialog agrees with `licensing_change_classify` for each (hint mismatch is a UI bug).
6. Jurisdiction create: 201 with the row, a duplicate code answers 400 (UI shows the generic invalid wording), step-up prompt, audit row
   `jurisdiction_registry.jurisdiction_created` with a NULL tenant.
7. Change detail screen for each kind: sentence, approval path, fields against the real payload.
8. AML screening policy: GET unconfigured; POST first version (as a platform admin acting as an AML officer with the quorum): 201 with a
   `policy_id`; then POST a looser version through the API directly and confirm the server answers 409 with `policy_loosening_refused`
   (the UI blocks the obvious cases earlier, so the refusal text path needs a direct call or an earlier-effective-time attempt);
   interval 3599 -> 400 `rescreen_interval_below_floor`; interval below twice a gate threshold -> `rescreen_interval_below_alert_threshold`;
   effective_from out of range -> `policy_effective_from_out_of_range`; a tenant not configured in the jurisdiction ->
   `tenant_not_in_jurisdiction`. Confirm that `check_codes` really arrives on each status (400 / 403 / 409) the way the OpenAPI says.
9. Existing screening screens: trigger policy and resolution refusals now show exact wording; confirm real `check_codes`.
10. A compliance (tenant) user can read the screening policy but is offered no authoring, and a direct POST is refused by the server.

## Decisions and residuals

- DECISION PENDING (owner / security): `POST /v1/admin/jurisdictions` is a direct, single-actor, non-maker-checker write (the OpenAPI
  says so). The UI exposes it with a plain notice, step-up and a reason code, because the brief asked for it, but a market owner adding
  jurisdictions without a second person is a governance gap. Interim safe behaviour: it grants no market permission and creates no
  licence; adding a country still needs the governed kinds above. Recommendation: put jurisdiction creation behind a licensing-change
  kind (server work, not frontend). `security` review requested for the new admin capability exposure.
- Licence creation / bind (lic5) are unchanged. Per the OpenAPI and the Go code there is no route to change a licence's identity
  fields or to edit `permitted_markets`; those remain informational.
- Per-row "File a change" on policies carries scope, brand, operation, product and country over from the selected row (the operator
  chose that row); action, reference and reason stay empty.
- The configuration list only knows the versions on the page shown; "version in force" detection is therefore by the loaded page.
  The dialog says so when none is found.
- The UI does not embed an ISO-3166 list (it checks shape only; the server checks the code is assigned). No country, jurisdiction or
  operation list is suggested.
- `ledger-finance` review is not relevant (no balance or adjustment flow). `security` review is requested for: the jurisdiction create
  exposure, and the AML policy authoring tab (same server rules as the trigger policy tab).
- Not covered: tenant-staff (compliance / tenant_admin) filing of tenant-rung / brand / operation policies through the Back Office. The
  server allows it for their own tenant; the Back Office licensing section stays platform_admin only (existing lic5 decision).
