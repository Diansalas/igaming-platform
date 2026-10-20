# lic16: Back Office screens for three governed policy areas (frontend only)

Branch `lic16-backoffice-policies` from `integ-b` 5503334 (migration head 0183). `/backoffice` only: no Go, no migration, no OpenAPI change (every call already
exists in `docs/api/openapi/platform-api.yaml`). Software capability only. Nothing in the UI recommends a value, approves a jurisdiction or states that any
operation is lawful. Every threshold, mode and action is shown as a decision recorded by named staff (compliance), not by the platform.

## 1. Status labels

| Deliverable | Label | Where |
|---|---|---|
| KYC enforcement policies (lic10 L-20): list (server filters jurisdiction, status), author draft (explicit fields, no defaults; only `cumulative_deposit` and `play`), activate, withdraw draft, supersede with a same-key draft successor | IMPLEMENTED | `api/kycEnforcementPolicies.ts`, `features/policies/KycPoliciesPage.tsx` |
| KYC: four-eyes refusal shown (author = activator; supersede must differ from BOTH authors), "values come from compliance", dormant-by-default note, no switch-off action for an active policy | IMPLEMENTED | same |
| Residence maker-checker: mode in force, explicit mode change (no preselection, stricter 200 applied at once, looser 202 pending), history with pending requests and approve, pending-determinations queue with second-Person approve | IMPLEMENTED | `api/residenceApproval.ts`, `ResidencePage.tsx` |
| Withdrawal restriction policy (lic11): per-tenant cause x stage effective action with UNCONFIGURED/default notice and source; file a change (allow / hold_for_review / block per licence cause); change list and detail; approve / reject / cancel; hard-restrictions note; tightening vs loosening semantics; tenant/brand status and market closure as information only | IMPLEMENTED | `api/withdrawalRestriction.ts`, `WithdrawalRestrictionPages.tsx` |
| RBAC UI hints, nav entries, route guards (`kycPolicyRead/Manage`, `residencePolicyRead/Manage`, `residenceQueue`, `wrpRead/Change`) | IMPLEMENTED (UI hint only; server authoritative) | `auth/permissions.ts`, `layout/nav.ts`, `app/AppRoutes.tsx` |
| Tests (RBAC with zero requests, payload pins, validation, step-up resend once, 200/202/201 body classification, refusal wording, XSS, OpenAPI contract coverage, static pins) | IMPLEMENTED | `features/policies/*.test.ts(x)`, `api/policies.test.ts` |
| AML trigger policy screens | NOT IMPLEMENTED | no HTTP routes exist yet; deliberately not built (residual R1) |
| Real-API / browser run, visual review | NOT IMPLEMENTED | tests use Vitest + MSW only (repo convention) |

## 2. Who can use which screen (mirrors the route table; the server decides every call)

| Screen | Route | Roles shown the entry |
|---|---|---|
| KYC policies | `/kyc-policies` | platform_admin only (`kyc_enforcement_policy:read/manage`; a tenant token is refused 403) |
| Residence approval | `/residence-approval` | tenant-scoped `compliance` (read, change, approve) and `tenant_admin` (read only). The routes are tenant-scoped staff only: a PLATFORM token is refused 403 and there is no platform_acting path, so platform_admin gets no entry and the guard shows "no access" with zero requests |
| Withdrawal restrictions | `/withdrawal-restrictions` (+ `/tenants/:id`, `/changes`, `/changes/:id`) | platform_admin: any tenant (explicit picker), file and decide, change list. tenant_admin and compliance: their OWN tenant, read only (redirected to it). The change list and change detail are platform-only reads, so they are guarded by the platform_admin flag |

The tenant id for the residence screens is the signed-in staff member's own tenant from the token (a UI convenience; the server derives tenant scope itself).

## 3. Behaviour worth knowing

- **No defaults anywhere (G-14).** Jurisdiction, trigger type, threshold, asset, play operation, legal reference, reason code, residence mode, withdrawal cause and action all start empty; submit stays disabled until valid. Pinned by tests and a static test. The platform ships no policy and the UI says so on every screen.
- **Thresholds are strings.** `threshold_minor_units` is typed and sent as a digit string (regex `^[1-9][0-9]{0,37}$`); a 38-digit value is round-tripped exactly (test). The code never calls `Number`, `parseInt`, `BigInt` or `toFixed` on it (static test plus the repo-wide `noFloatMoney` scan). It is displayed raw as "minor units of <asset>" because there is no asset-exponent API on these screens.
- **Success is never inferred from the status line.** `apiFetch` returns the body only, so every outcome is classified from the BODY: KYC writes need the expected `status` and (for activate / supersede) the expected policy id; residence set needs `active` + 0 approvals (applied) or `pending` + 2 (pending) AND an echo of the requested mode; residence approvals need `active` with approvals >= required, or `pending` below it; determination approve needs `approved` for the same verification; withdrawal-restriction filing (201 for both outcomes) needs `executed:true`+`executed` (applied) or `executed:false`+`requested` (pending). Anything else shows "could not be recognised, nothing is confirmed".
- **Fixed-wording errors.** `features/policies/errors.ts` maps the closed `code`, the HTTP status and the action context to UI-owned sentences; the server `message` is never rendered (static test + sentinel tests). For the generic 403 / 409 of the KYC and residence routes the server gives no machine-readable sub-reason (they all use `forbidden` / `conflict`), so the UI lists the usual causes ("either ... or you authored it") and does not claim to know which applied. The WITHDRAWAL_RESTRICTION_POLICY_* codes have their own sentences.
- **Step-up.** KYC writes, residence policy approvals and determination approvals, withdrawal-restriction filing and approval are critical; the existing `apiFetch` interceptor prompts and re-sends the same body once (tested for each family). Residence mode filing, withdrawal reject and cancel need none. The signed actor proof (ADR 0110) is produced server-side: nothing here signs, hashes or sends a proof (static test).
- **Cancel has no body and no reason** (OpenAPI), as in lic5.
- **Hints, not authority.** Approve / activate are disabled or warned when the signed-in `sub` equals the recorded author / requester / determiner; the "stricter / looser" and "tightening / loosening" labels shown before filing are computed in the browser from the displayed action (allow < hold_for_review < block; none < into_open_market < all) and are labelled as hints. The database classifies and the screens show the server's class (`widening`, `required_approvals`) afterwards. A non-configurable cause is not offered; if forced, the server's NOT_CONFIGURABLE is shown.
- **Pending residence determinations are not market evidence.** Stated on the queue (fixed note + a "Not market evidence" badge per row). The list carries no country, and the UI renders none. `determined_at` is passed back as the exact string listed, never parsed.
- **Residence and withdrawal-restriction "applies to future" semantics** are on screen (a mode change applies to future determinations only).
- **Hard restrictions note** (account status, KYC, AML / sanctions holds, risk, kill switch, provider capability, payout instrument are never overridden by a policy; `allow` never skips another gate) is shown on the policy page and on the change detail.
- **Server descriptive fields** (`current_behaviour`, `governing_reference`, `notice`, free-text note, legal review reference) are rendered as React text only (XSS tests with `<img onerror>`); identifiers go through `safeToken` or the short-id helper.
- **Router ids** use `encodeURIComponent` (static test).

## 4. Honesty: what is mock-tested only

All tests are Vitest + MSW against hand-written responses that follow the OpenAPI shapes. They prove the UI's behaviour given those responses, the exact method / path / body keys / query keys of every call (generic `api/openapiContract.test.ts` plus `api/policies.test.ts`, which pins the exact route set of each module so the generic check is not vacuous), and the classification logic. They do NOT prove:

- that the real server accepts the payloads built here (for example that the asset code is registered, the jurisdiction exists, `legal_review_reference` passes every server rule beyond the mirrored subset, or `note` passes the server's lint);
- the real response bodies. The `executed` / `status` combinations, the 200 vs 202 residence answers and the 201 withdrawal-restriction answers are taken from OpenAPI text and handler reading, not run against a server. In particular the real KYC `created_by` visibility and whether `determined_by` / `requested_by` are populated for the viewer are unverified;
- the database four-eyes and Person rules (author != activator, supersede differs from both authors, distinct Persons for loosening approvals, 24 h approval window). The UI only displays the server's refusals;
- the mapping of a real 403 / 409 to the sub-reason (the server gives none for KYC / residence);
- the step-up flow against the real step-up endpoint (the mock grants a stepped token);
- visual rendering, accessibility beyond role / label queries, or behaviour in a real browser. No screenshot review was done.

Real-API e2e is for the orchestrator (the backoffice/e2e tree was not touched).

## 5. Tests added

`features/policies/policies.test.tsx` (75 tests): RBAC for seven non-admin roles on the KYC routes, six roles incl. platform_admin on residence, five roles on withdrawal restrictions (all with zero requests), tenant_admin / compliance read-only on withdrawal restrictions with zero change-list requests, tenant_admin without manage controls on residence, 403 degradation; KYC list / filters / no switch-off / XSS, author form empty, both payload shapes, validation, unconfirmed 201, activate payload, four-eyes 403 and 409 wording, author hint, withdraw, supersede (same-key successors only, payload), unconfirmed supersede, step-up resend once; residence default, stricter 200, looser 202, unconfirmed 200, same-mode guard, approve flows, own-request disabled, queue wording, determined_at byte-for-byte, wrong-verification body, 404 / 403 wording, step-up; withdrawal restriction unconfigured / configured / informational causes, explicit tenant picker, empty filing dialog and option sets, applied vs pending vs unconfirmed, refusal codes, step-up, list filter, detail approve / pending / reject / cancel (no body), refusal codes, XSS. `features/policies/policiesSafety.test.ts` (static pins). `api/policies.test.ts` (route pins, body / query pins, classifier tables). `layout/nav.test.ts`: the platform_admin and compliance exact label lists intentionally gained the new entries (`KYC Policies`, `Withdrawal Restrictions`; `Residence Approval`, `Withdrawal Restrictions`) and a new test pins which roles see them; no assertion was weakened.

## 6. Residuals

- R1 AML trigger policy: no HTTP routes yet, so no screen. Build it when the routes exist.
- R2 No asset registry API on these screens: `asset_code` is a validated text field (server refuses unknown); thresholds are shown as raw minor units without exponent formatting.
- R3 The KYC list is a bare array of at most 500 (newest first); a notice appears at 500. No paging API exists.
- R4 The withdrawal-restriction change list has `limit` (max 200) and no offset; the newest 200 are shown with a notice. Versions of a tenant are not paged by the API.
- R5 The Back Office has no "re-check existing players after a residence mode change" action (documented as not built in the runbook); the UI only states that existing determinations are not re-checked.
- R6 A tenant staff member's own-tenant id comes from the decoded token (UI convenience). If the token claim and the server disagree the server wins and the page shows its refusal.
- R7 The server does not put a machine-readable sub-reason on KYC / residence 403 and 409 answers, so the UI wording lists possible causes. If operators need exact reasons, a backend change adding closed codes is required (not done here).
- R8 Security review required for the new admin screens (new admin capability surface; no new permission level was introduced). `ledger-finance` review is suggested for the withdrawal-restriction screens (they configure payout gating).

## 7. For the orchestrator

- Merge the branch. No migration, OpenAPI, golden-route or step-up-class artefact changed.
- Proposed edits (not made; docs/runbooks are shared): `docs/runbooks/kyc-enforcement-policy-authoring.md` status line "Back Office is NOT IMPLEMENTED" -> "Back Office `/kyc-policies` (lic16)", and residual "Back Office pages" closed; `docs/runbooks/residence-maker-checker.md` and `docs/runbooks/withdrawal-restriction-policy.md`: add "Back Office `/residence-approval` and `/withdrawal-restrictions` (lic16)".
- Real-API e2e to run: author draft as admin A -> activate as admin B (and as A: expect refusal) -> third admin supersedes; residence set stricter (200) and looser (202) -> two approvals -> mode in force; pending queue -> second Person approve (and as the determiner: expect refusal); withdrawal-restriction tightening (201 executed) and loosening (201 pending) -> two other admins approve -> executed; non-configurable cause refusal.

## 8. Verification

`npm run build` (typecheck + vite build) pass; `npx vitest run src`: 69 files, 901 tests, all pass (new: 3 test files, 100 tests incl. the generic contract test now covering the three new api modules). A bare `npx vitest run` additionally tries to load the tracked Playwright specs under `backoffice/e2e/specs` and fails on `Failed to resolve import "playwright/test"`; that is pre-existing at the base commit (the e2e tree is untouched here and is run by Playwright, not Vitest). `backoffice/node_modules` is a symlink in the worktree (untracked).
