# lic32 record: real-stack end-to-end of the lic31 Back Office market-configuration and screening-policy flows

Branch `lic32-e2e-market-config` (from integ-c 479db46, migration head 0189). Test code only: no production code, API, OpenAPI or migration
change. Built Back Office (Chromium, Playwright) against the real `cmd/platform-api` and a real migrated PRIVATE Postgres, following
`backoffice/e2e/README.md`. Synthetic data only. Nothing here is a legal approval of any country or jurisdiction.

## Verdict (labels)

| Item | Label |
|---|---|
| Spec `11-market-config` (8 tests, flows 1 to 7 of the lic31 list) | IMPLEMENTED (run against the real stack, 8 of 8 passed) |
| Spec `12-screening-policy` (7 tests, flows 8 to 10 of the lic31 list) | IMPLEMENTED (7 of 7 passed), with the gaps listed below |
| The lic31 Back Office filing and screening-policy screens | now evidenced against the real stack for the paths below; the gaps below are NOT evidenced |

## Command and result

```
export PRIV_DB=<prefix>$(date -u +%H%M%S)p$$x01 PRIV_SRC=<this worktree>
bash <scratchpad>/priv_db.sh                       # fresh private database, migrated to head 0189
export E2E_OWNER_DATABASE_URL=<owner URL of that DB> E2E_RUNTIME_DATABASE_URL=<igaming_runtime URL of that DB>
bash backoffice/e2e/run.sh                         # all specs 01-12
```

Final full run on a fresh database, tip of this branch: **68 passed, 0 failed, 0 skipped, 68 total** (37.5 min, exit 0). Per spec:
01: 4, 02: 1, 03: 1, 04: 2, 05: 1, 06: 8, 07: 11, 08: 9, 09: 8, 10: 8, 11: 8, 12: 7.

History of the runs (not hidden): run 1 (spec 10 as committed in lic22): 60 passed, 1 failed, 7 not run (spec 10 (a) failed, and spec 10 is serial,
so (a2) to (g) did not run). Run 2 (after fixing spec 10 (a)): 64 passed, 1 failed, 3 not run (spec 10 (d) failed, which run 1 had masked). Both
failures are the same cause: lic31 intentionally replaced generic wording with exact wording (see "Edits to existing files"). Spec 10 alone was
then re-run on a fresh database (8 of 8), then the final full run above (68 of 68).

## What spec 11 covers (real UI as PA_A, PA_B, PA_C, PA_D and same-Person twins PA_A2/PA_B2)

| Test | Evidence |
|---|---|
| (a) enable ceiling IE on the synthetic licence STG-SYNTHETIC-1 | hint "widening"; "Pending approval"; DB request `requested`, `widening=true`, `required_approvals=2`; no ceiling row and the real resolver unchanged before approval; requester: Approve disabled and server 409 `LICENSING_CHANGE_SELF_APPROVAL`; PA_A2 (same Person) refused, rendered; PA_B approves (1 of 2, row still absent); PA_B2 refused ("already approved"); PA_C approves, `executed`, 2 approvals; ceiling row `enabled/active` with the reference; read API total 1; real resolver `IE/deposit`: `not_permitted_by_licence` before, `not_configured` after (a ceiling alone does not open the country) |
| (b) disable ceiling DK on a fresh active licence | per-row entry carries the country; hint "narrowing"; "Applied at once" by PA_D alone; request `executed`, `widening=false`; ceiling row `disabled`, 2 versions |
| (c) policy for NL | tenant-rung enable with no ceiling and no basis: server 409 `LICENSING_CHANGE_REFUSED`, UI fixed wording, no change request, no policy row, resolver `not_permitted_by_licence`; after the ceiling through the governed flow the same filing is pending (widening, 2 approvals, nothing stored until PA_B and PA_C approve), then the policy row exists and the real resolver says `permitted`; operation-rung disable of `deposit` is narrowing and immediate and the resolver says `disabled_by_operation` for deposit and `permitted` for registration; brand-rung withdraw: reference field required, Submit disabled (UI blocks first), direct call with the session token without a reference: 400 `validation_error`, nothing stored; with a reference: pending widening (then cancelled by the requester) |
| (d) tenant jurisdiction configuration | six steps (first version, remove currency, add currency, remove geo block, add geo block, close): UI class hint equals `licensing_change_requests.widening` in all six; stored arrays checked after each; narrowing steps show `required_approvals=0` in the database |
| (e) jurisdiction creation | created at once by one actor, 201; no change request filed; no licence and no ceiling created; resolver unchanged for all four operations; one `audit_log` row `jurisdiction_registry.jurisdiction_created` with NULL tenant and the actor PA_A; duplicate code: 400 `validation_error` rendered with the generic invalid wording, nothing stored |
| (f) tenant staff (tenant_admin, compliance) | Licensing nav hidden; licence, tenant and jurisdiction pages denied; ceiling filing 403, jurisdiction creation 403, ceiling read 403, jurisdictions read 403; anonymous 401; nothing stored (counts of changes, jurisdictions, ceilings unchanged) |
| (g) change detail | for a ceiling, a brand-rung policy withdraw, a config set and an already executed narrowing ceiling disable: "Change in words" sentence matches exactly (licence/tenant/brand/jurisdiction ids), "Approval path" says widening/narrowing, "N of M required" equals the database (`1 of 2`, `0 of 2`, `0 of 2`, `0 of 0`) |
| (h) deny by default | ceiling PT approved: read API total 1; no tenant policy; real resolver `not_configured` for registration, deposit, wagering, withdrawal; tenant page lists no PT row; a filed tenant policy with one of two approvals changes nothing; after the second distinct Person approves, `permitted` |

## What spec 12 covers (tenant `sp1`, officers PA_C and PA_D through the real two-approver grant flow)

(a) unconfigured wording and empty start (nothing preselected, Submit disabled); first version authored with explicit fields through the UI incl.
an explicit grace of 0; stored with scope `platform_acting`. (b) UI blocks: floor sentence, loosening alert, zero requests sent. (c) closed codes on
the wire by direct calls with the officer session (see table) and no row stored. (d) through the UI with exact wording: an effective time 40 days ahead
(`policy_effective_from_out_of_range`), and `policy_loosening_refused` reached by a version dated two days ahead that is looser than a version dated
tomorrow but tighter than the one in force. (e) existing trigger screen: gate with no heartbeat shows the exact heartbeat wording. (f) with a gate of
3600 s in force, interval 3600 is refused with `rescreen_interval_below_alert_threshold` (exact wording rendered), interval 7200 accepted (boundary).
(g) non-officer platform admin, tenant compliance, tenant admin and another tenant refused; nothing written.

Wire observations (status, `code`, `check_codes`):

| Case | Wire |
|---|---|
| interval 3599 | 400 `validation_error`, `["rescreen_interval_below_floor"]` |
| looser version | 409 `conflict`, `["illegal_transition","policy_loosening_refused"]` |
| effective +40 d / -1 h | 400 `validation_error`, `["policy_effective_from_out_of_range"]` |
| gate, no worker heartbeat | 400 `validation_error`, `["screening_worker_heartbeat_missing"]` |
| interval below twice the gate threshold | 400 `validation_error`, `["rescreen_interval_below_alert_threshold"]` |
| non-officer platform admin POST | 403, no check code |
| tenant compliance / tenant admin POST | 403, no check code (compliance may GET: 200) |
| other tenant, tenant staff token (GET and POST) | 404 |
| session officer of tenant sp1 posting for the seeded tenant | 403, no check code |

## Findings

1. **Spec 10 had two stale assertions, changed on purpose (nothing weakened).** (a) expected the generic "The server rejected the policy as invalid." for
   a gate without heartbeat; lic31 now shows the exact heartbeat wording, so the assertion now expects "No screening worker has reported in, so a
   withdrawal gate cannot be enabled." Status 400, code `validation_error`, "no WORKER_REFUSAL text", nothing written and the control are unchanged.
   (d) expected "The resolution was refused." for a sanctions clear; it now expects the exact `sanctions_clear_refused` wording. Both edits are
   commented in the spec.
2. **The heartbeat refusal is a 400, not a 409.** The lic31 premise "the generic 409 now shows exact wording" does not describe this case: the
   server answers 400 `validation_error` with `check_codes: ["screening_worker_heartbeat_missing"]` and the exact wording is rendered from the code.
   The 409 branch (`WORKER_REFUSAL`, "does not say which applied") is therefore never reached for the heartbeat. Spec 12(e) asserts 400 or 409 plus the code.
3. **`policy_loosening_refused` arrives with a second check code first**: `["illegal_transition","policy_loosening_refused"]`. `illegal_transition` is the
   pre-existing generic 409 check code and is not in `aml.RefusalCodes`; the UI picks the first code it knows, so the exact wording is correct. A
   client that read only the first element would miss it.
4. **The UI loosening check compares with the version in force; the server compares with the latest version.** A future-dated tighter version makes a
   version that the UI lets through and the server refuses; it is rendered with the exact wording (spec 12(d)). Not a defect, but it means the UI block is
   advisory.
5. **`platform_officer_required` was NOT observed on the wire.** Tenant roles are refused earlier by the route's permission gate (403, no check code),
   and a tenant-scope officer grant cannot be created for tenant staff by a platform admin (409 "platform-originated grants for tenant staff are not
   supported"; a tenant-originated G-T request flow exists and was not exercised). Its wording is covered by the lic31 unit tests only.
6. **Governance gap, DECISION PENDING (owner / security), confirmed on the real stack:** jurisdiction creation is applied at once by one platform
   administrator (no maker-checker). Verified that the row grants no permission (no licence, no ceiling, resolver unchanged, audit row with NULL tenant), so
   the gap is "any single platform admin can add registry rows", not "a market can be opened". Recommendation unchanged: put it behind a licensing-change kind.
7. **Tenant compliance can file an own-tenant tenant-rung policy** (201 `executed:false`, widening, 2 platform approvals needed, nothing stored until then).
   This is the lic5 design (the Back Office section stays platform-only) but it differs from "tenant staff cannot file": tenant_admin gets 403, compliance does not.
   Cancelled by the requester in the test. Tenant-staff filing of configuration or brand-rung kinds was not characterised.
8. **The precheck refusal carries no closed code**: 409 `LICENSING_CHANGE_REFUSED`, no `check_codes`; the UI can only say "a precondition such as a country
   ceiling", never which. Duplicate jurisdiction code is 400 `validation_error` rendered as the generic invalid wording (the dialog's own note says a duplicate
   is refused, the error text does not).
9. **A server refusal of "brand-rung withdraw without reference" cannot be rendered by the UI** (the UI blocks first). Evidence is the wire (400) plus the UI block.
10. The UI class hints agree with the database for all six configuration steps and for ceiling enable/disable and policy filings (no mismatch found).

## Edits to existing files (every one)

- `backoffice/e2e/run.sh`: +2 lines, builds `tools/mcprobe` into the run directory as `e2e-mcprobe` (additive).
- `backoffice/e2e/README.md`: two table rows for specs 11 and 12 and one paragraph "lic32 additions" (additive).
- `backoffice/e2e/specs/10-aml-screening.spec.ts`: the two intentional assertion changes of finding 1 (the old wording is no longer shown by the UI).
- No change to `lib/api.ts`, `lib/session.ts`, `setup/*`, `playwright.config.ts`. New files only otherwise: `specs/11-market-config.spec.ts`,
  `specs/12-screening-policy.spec.ts`, `lib/mc_world.ts`, `lib/mc_ui.ts`, `lib/sp_ui.ts`, `tools/mcprobe/main.go`.

## Not covered (gaps, not claims)

Ceiling WITHDRAW and tenant-rung policy withdraw/disable through the UI; brand-rung enable; product-scoped operation rung; an unassigned country code, a
currency that is not an active asset and a foreign brand id refused by the server; the 24 h stale-approval and 72 h expiry paths; the step-up prompt on a
stale session for these dialogs (the fresh sessions did not need one; spec 06(h) covers the prompt generally); audit rows for the ceiling, policy and
configuration writes (only jurisdiction creation was checked in `audit_log`); `jurisdiction_not_in_scope`, `tenant_not_in_jurisdiction`,
`screening_policy_missing`, `gate_*` and resolution codes beyond those listed; tenant-originated officer grants (finding 5); concurrency of two approvers;
operating-basis interplay with a ceiling for the same country; browsers other than Chromium; a real provider (the screening worker is the MOCK).
The real market resolver is exercised through `tools/mcprobe` (the same function `marketgate` calls), not through a player request.

## Cleanup

Private databases dropped; `backoffice/node_modules` symlink removed by `run.sh` at exit; the run directory (secrets) removed.
