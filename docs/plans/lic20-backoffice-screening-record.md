# lic20: Back Office screens for AML screening trigger policy and officer resolution (frontend only)

Branch `lic20-backoffice-screening` from integ-b `17f6fcc` (migration head 0184). `/backoffice` only: no Go, no migration, no OpenAPI change, `backoffice/e2e` untouched. Software capability only; no regulatory approval is claimed. The only screening provider that exists is the MOCK, and the production binary starts no screening worker.

## 1. Status labels

| Deliverable | Label | Where |
|---|---|---|
| Trigger policy view per tenant + jurisdiction (GET, `configured=false` shown as "nothing triggers, nothing gates") | IMPLEMENTED (mock-tested) | `features/aml/ScreeningPage.tsx` |
| Trigger policy authoring: events (registration, kyc_approval, scheduled), gate flag, gate_alert_after_seconds, effective_from, legal review reference, reason code; every field empty at start, no preselection, nothing shipped | IMPLEMENTED (mock-tested) | same |
| Prominent gate warning (funds trapped, never in production, heartbeat refusal is a safeguard not permission) and fixed-wording display of the server refusal | IMPLEMENTED (mock-tested) | `ScreeningLabels.ts`, `screeningErrors.ts` |
| Re-screen interval and backfill grace fields | NOT IMPLEMENTED (no API) | see section 3 |
| Per-person screening request list with states (pending, claimed, done, needs_officer, failed_terminal, resolved) | IMPLEMENTED (mock-tested) | same |
| Officer resolution (clear / potential_match / confirmed; `error` not offered) with quorum, four-eyes, step-up and sanctions-class rule shown as server-enforced notes | IMPLEMENTED (mock-tested) | same |
| Link from the AML section (`/aml` links to `/aml/screening`) and route guard `amlRead` | IMPLEMENTED | `AmlPage.tsx`, `app/AppRoutes.tsx` |
| Real-API / browser run, visual review | NOT IMPLEMENTED | |

## 2. Who sees what (UI hints; the server decides every call)

Route table (`internal/httpserver/aml_screening_routes.go`): reads `aml:case:read`, step-up none; writes `aml:hold:manage`, step-up `critical`; policy authoring is platform_acting AML officer only. The UI mirrors: `/aml/screening` needs the existing `amlRead` flag (compliance, platform_admin); the Resolve action needs `amlHolds`; "Author next version" needs `amlHolds` AND a platform session. Tenant-scoped compliance sees the policy and requests read-only with a fixed note. Every other role gets the NotAuthorized page with zero requests (tested for six roles). If the server refuses an action the UI offered, the refusal is shown in fixed wording.

## 3. Gaps and decisions

- **Re-screen interval and grace.** The task listed them as policy fields. They are values of the jurisdiction's screening policy, not of the lic18 trigger-policy request (`AMLScreeningTriggerPolicyRequest` has no such fields) and no HTTP route reads or writes the screening policy. The screen says so and does not invent inputs. If operators must author them in the Back Office, a backend route is required (stop-and-report item for the orchestrator).
- **Worker-heartbeat refusal.** The server returns a generic 409 with check code `guard_refused`; there is no machine-readable heartbeat code. The wording lists the heartbeat as the leading cause plus other possible causes and never claims to know which applied. A distinct closed code would let the UI be exact.
- **Sanctions clear.** A server-refused sanctions clear is also a generic 409 `guard_refused`; same treatment. The UI warns up front when the request's `match_lists` includes `sanctions` but does not disable Clear (server authoritative).
- **Jurisdiction choice.** Platform admins pick from the jurisdiction list; compliance has no `jurisdiction_registry:read` flag so it types the jurisdiction UUID.
- **Client mirrors.** The seconds range (60..2592000), the effective window warning (now-2min .. +30 days, warning only, not blocking), the legal-reference rule and the reason-code pattern are mirrors for convenience; the server decides.

## 4. Behaviour worth knowing

- No defaults: events unchecked, gate "Choose", time, reference, reason empty, acknowledgement unchecked; submit disabled until valid (pinned by tests and a static test). Gate ON additionally requires the scheduled event, a seconds value in range, and an acknowledgement checkbox that funds can be trapped and that the user is not enabling it in production.
- 201 bodies are classified by shape (`policy_id` uuid; `result_id` uuid + boolean `replayed`, optional uuid `hold_id`); anything else shows "could not be recognised, nothing is confirmed". A replay is labelled as such.
- Step-up: the existing `apiFetch` interceptor prompts and re-sends the same body once (tested for both writes).
- Fixed wording only; `message` of the server is never rendered (static + sentinel tests). Server strings (trigger, outcome, error code) go through `safeToken`; XSS tests included.
- No float math: seconds are digit-checked and read with `JSON.parse`; the repo `noFloatMoney` scan passes with no new allow-list entry. Path ids use `encodeURIComponent`.
- Nothing in the browser signs or hashes; the actor proof is server-side.

## 5. Mock-tested only (stated plainly)

Every test is Vitest + MSW against hand-written responses that follow the OpenAPI shapes. They prove the UI's behaviour given those responses, the exact method / path / query / body keys (generic `api/openapiContract.test.ts` picks up `api/amlScreening.ts`; `api/amlScreening.test.ts` pins the exact four routes) and the classification logic. They do NOT prove: that the real server accepts these payloads; the real response bodies (the `replayed`/`hold_id` combinations come from handler reading); the officer grant, quorum, actor proof and sanctions rule (displayed server refusals only); the heartbeat refusal against a real gate attempt; the real step-up endpoint; visual rendering or a real browser. Real-API e2e is for the orchestrator.

## 6. Tests added

`features/aml/screening.test.tsx` (36 tests: RBAC for six roles with zero requests, tenant compliance read-only, link from AML, empty author form, payload pins for gate off / empty events / gate on, validation of seconds, reason, legal reference, unconfirmed 201, heartbeat refusal wording, 403 degradation, step-up resend once for both writes, XSS, request states and Resolve visibility, unrecognised bodies, sanctions notes, clear and match payloads, replay and hold id, three unconfirmed 201 shapes, sanctions-clear refusal with refetch, quorum wording, no server message). `api/amlScreening.test.ts` (OpenAPI contract and route pins, enum equality, classifiers). `features/aml/screeningSafety.test.ts` (static pins).

## 7. For the orchestrator

- Merge the branch. No migration, OpenAPI, golden-route or step-up artefact changed.
- Proposed doc edit (not made): `docs/plans/lic18-record.md` row H-1 (c) "NOT IMPLEMENTED (Back Office)" -> "Back Office `/aml/screening` (lic20)", and the screening runbook can point to it.
- Needs `security` review (new admin screens over a critical-step-up write path; no new permission) and `ledger-finance` awareness (the gate parks withdrawals).
- Decision for a human, interim behaviour built: whether Back Office operators must author the screening policy (interval/grace); interim: not offered.

## 8. Verification

`npm run build` pass; `npx vitest run src`: 72 files, 952 tests, all pass. `backoffice/node_modules` is a symlink (untracked).
