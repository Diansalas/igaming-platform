# ADR 0119 S5 + S6 - staff lifecycle API contract (for the S8 back-office agent)

Status: contract for slices S5 + S6 (backend, one merge unit, ADR 0119 review C6). The normative source is
`docs/api/openapi/platform-api.yaml` (tag `staff-lifecycle`, plus `POST /v1/staff/invitations/accept` under
`staff-auth`); this page summarises it for the back-office work (S8). If the two disagree, the OpenAPI file wins and this
page is a defect. Nothing here is a regulatory claim. Email delivery is `MOCK` in development and `PROVIDER DEPENDENT`
in production (ADR 0119 OQ-1 default (b): no interim; a production binary with the mock email provider refuses to
start, so staff onboarding beyond the `cmd/seed-admin` bootstrap admins is blocked in production until a real email
adapter exists).

## 1. Conventions shared by every route

- **Chain.** Every `/v1/admin/...` route below is a `staffRoute`: bearer token, staff principal, per-request liveness
  (`401 session_revoked`), aal2, then the step-up class (`403 step_up_required` with `max_age`, ADR 0119 S4b contract
  section 2), then the route's own permission gate. The class of every route is pinned in
  `internal/httpserver/staff_route_golden_test.go`.
- **Tenant comes from the token.** A tenant-scoped caller may only name its own tenant in `{tenantID}`; another tenant
  is `404 not_found` (never data). A tenant-scoped caller can never reach a platform staff row: the `/v1/admin/platform-staff`
  and `/v1/admin/platform/staff-lifecycle-requests` routes answer `403 forbidden` to it.
- **Reason codes.** Every mutation except an invitation (and its resend) carries `reason_code` from a closed list
  (section 4). `other` requires `note` (10-500 characters, no control, bidi-override or zero-width characters; treat it
  as visible to other staff and keep personal data out of it).
- **Self-target.** No caller may suspend, reactivate, offboard, change the role of, MFA-reset, or revoke the invitation
  of their own account: `403 self_target_forbidden` (also refused by the database, `SF013`).
- **Role authority.** A platform admin may act on every tenant role. A tenant admin may only invite into, change into
  or out of, or act on a staff member whose role is one of `tenant_admin`, `support`, `compliance`; anything else is
  `403 role_not_grantable`. The platform-only roles (`finance`, `risk_manager`, `promotions_manager`,
  `bonus_operations`) are granted and governed by a platform admin only.
- **No credential is ever returned.** An invitation or an MFA reset emails a single-use token to the staff member's
  address. The inviter / resetter never sees a password, token or factor. The response says only whether the email
  was handed to the provider (`email_delivery: "sent" | "failed"`); on `failed` use the resend route.
- **Sessions.** Suspension, offboarding, role change, MFA reset and Person linking end every session of the target
  (their next request answers `401 session_revoked`). Every one of these bumps `staff_version`; a token minted before
  it is refused by liveness.
- **Errors** (the platform error envelope, `code` is closed):

| HTTP | `code` | Meaning |
|---|---|---|
| 400 | `validation_error` | malformed body, unknown role, unknown reason code, missing note for `other`, malformed id |
| 403 | `forbidden` | missing permission, or a tenant caller on a platform route |
| 403 | `self_target_forbidden` | the target is the caller |
| 403 | `role_not_grantable` | the caller may not grant / act on that role |
| 403 | `four_eyes_violation` | platform four-eyes: the approver is the requester or the target (by staff id or by Person), or has no Person |
| 404 | `not_found` | unknown staff member / request, another tenant's staff member, or (tenant caller) a platform row |
| 409 | `conflict` | the email is already used in that scope |
| 409 | `lifecycle_transition_invalid` | the target is not in a state the action applies to (e.g. reactivate an active account) |
| 409 | `last_admin` | the change would remove the last active, enrolled platform admin (`SF020`) or, for a tenant caller, the last active, enrolled tenant admin of the tenant (`SF021`) |
| 409 | `lifecycle_busy` | suspension / offboarding phase 2 could not take the staff row lock within 5 s (twice). The target's sessions ARE already revoked (fail-safe); retry |
| 422 | `person_required` | `platform_admin`, `tenant_admin`, `finance` and `compliance` require a linked Person (`SF014`) |
| 503 | `service_unavailable` | database fault / no proof key / no MFA keys |

## 2. Tenant staff (`/v1/admin/tenants/{tenantID}/staff`)

| Route | Permission | Step-up | Body | Success |
|---|---|---|---|---|
| `GET .../staff?status=&role=` | `staff:read` | none | - | `200 {"staff": [StaffDirectoryEntry]}` (sorted by email; at most 500, `truncated: true` beyond) |
| `GET .../staff/{staffID}` | `staff:read` | none | - | `200 StaffDirectoryEntry` |
| `GET .../staff/{staffID}/lifecycle-events` | `staff:read` | none | - | `200 {"events": [StaffLifecycleEvent]}` (newest first) |
| `POST .../staff/invitations` | `staff:manage` | critical | `{email, role, person_id?}` | `201 {"staff": StaffDirectoryEntry, "email_delivery": "sent"\|"failed"}` (status `invited`) |
| `POST .../staff/{staffID}/invitation/resend` | `staff:manage` | critical | `{}` | `200 {"staff": ..., "email_delivery": ...}` |
| `POST .../staff/{staffID}/invitation/revoke` | `staff:manage` | critical | `{reason_code, note?}` | `200 {"staff": ...}` (status `offboarded`) |
| `POST .../staff/{staffID}/suspend` | `staff:manage` | critical | `{reason_code, note?}` | `200 {"staff": ...}` |
| `POST .../staff/{staffID}/reactivate` | `staff:manage` | critical | `{reason_code, note?}` | `200 {"staff": ..., "email_delivery"?: ...}` (an MFA-reset email when the target has no usable factor) |
| `POST .../staff/{staffID}/offboard` | `staff:manage` | critical | `{reason_code, note?}` | `200 {"staff": ...}` |
| `POST .../staff/{staffID}/role` | `staff:manage` | critical | `{role, reason_code, note?}` | `200 {"staff": ...}` |
| `POST .../staff/{staffID}/mfa-reset` | `staff:manage` | critical | `{reason_code, note?}` | `200 {"staff": ..., "email_delivery": ...}` |

`staff:read` is held by `platform_admin` (any tenant), `tenant_admin` and `compliance` (own tenant). `staff:manage`
by `platform_admin` and `tenant_admin`.

Effects (all audited; each writes one `staff_lifecycle_events` row with before / after):

- **Invite:** creates the staff member `invited` (no password), emails an `invite` token (72 h). Person is required
  for `tenant_admin`, `finance`, `compliance`.
- **Resend:** re-issues the invite token (the earlier one stops working). Only for `invited`.
- **Revoke invitation:** `invited -> offboarded`, the token is revoked.
- **Suspend:** phase 1 revokes every session, live challenge and enrolment token of the target and commits; phase 2
  moves `active -> suspended`. Reason `security_compromise` also revokes the second factor and recovery codes.
- **Reactivate:** `suspended -> active`. If the target has no usable second factor, an `mfa_reset` token is emailed.
- **Offboard:** `active | suspended -> offboarded` (terminal): password removed, every factor, recovery code,
  challenge, token and session revoked; email and Person kept for audit identity. Same two phases as suspend.
- **Role change:** `active` only, role A to B (both grantable by the caller). Sessions revoked.
- **MFA reset:** `active` only. Revokes factor, recovery codes, challenges and sessions (this is the only way to clear
  a hard lock), emails an `mfa_reset` token (24 h). The staff member redeems it at `POST /v1/staff/auth/enrol/redeem`
  with their current password, then enrols.

## 3. Platform staff

| Route | Permission | Step-up | Notes |
|---|---|---|---|
| `GET /v1/admin/platform-staff` | `staff:read`, platform caller | none | `200 {"staff": [...]}` |
| `GET /v1/admin/platform-staff/{staffID}` | `staff:read`, platform caller | none | |
| `GET /v1/admin/platform-staff/{staffID}/lifecycle-events` | `staff:read`, platform caller | none | |
| `POST /v1/admin/platform-staff/{staffID}/suspend` | `staff:manage`, platform caller | critical | single actor (protective), bounded by last-admin |
| `POST /v1/admin/platform-staff/{staffID}/offboard` | `staff:manage`, platform caller | critical | single actor, bounded by last-admin |
| `POST /v1/admin/platform-staff/{staffID}/invitation/resend` | `staff:manage`, platform caller | critical | only an `invited` platform row |
| `POST /v1/admin/platform-staff/{staffID}/invitation/revoke` | `staff:manage`, platform caller | critical | `invited -> offboarded` |
| `POST /v1/admin/platform/staff-lifecycle-requests` | `platform_staff:govern` | critical | four-eyes request: `{kind, target_staff_id?, email?, person_id?, reason_code?, note?}` |
| `GET /v1/admin/platform/staff-lifecycle-requests` | `platform_staff:govern` | none | `200 {"requests": [PlatformStaffLifecycleRequest]}` (newest first, at most 200) |
| `GET /v1/admin/platform/staff-lifecycle-requests/{requestID}` | `platform_staff:govern` | none | |
| `POST .../staff-lifecycle-requests/{requestID}/approve` | `platform_staff:govern` | critical | `{payload_hash}` (the frozen hash the approver reviewed); executes the change |
| `POST .../staff-lifecycle-requests/{requestID}/reject` | `platform_staff:govern` | none | `{}` |
| `POST .../staff-lifecycle-requests/{requestID}/cancel` | `platform_staff:govern` | none | requester only |

`platform_staff:govern` is held by `platform_admin` only and is never grantable through K1.

Request kinds:

| `kind` | Body | Executes (on approval) |
|---|---|---|
| `platform_invite` | `email`, `person_id` (required), `reason_code` absent | creates an `invited` `platform_admin` and emails the invite token. The ONLY way to create a platform admin over HTTP |
| `platform_reactivate` | `target_staff_id`, `reason_code` (reactivate list) | `suspended -> active` (an `mfa_reset` email if no usable factor) |
| `platform_mfa_reset` | `target_staff_id`, `reason_code` (MFA reset list) | the MFA reset of section 2 |

`platform_password_reset` exists in the database vocabulary but is `NOT IMPLEMENTED` in this slice (`400`).

Four-eyes: one approver plus the requester, two distinct staff members AND two distinct Persons; the approver is
neither the requester nor the target, by staff id and by Person; a staff member without a Person can neither request
nor approve. A request expires 72 h after creation (`state` reads `expired`). `state` is one of `pending`, `executed`,
`rejected`, `cancelled`, `expired`. A request whose precondition no longer holds at approval (for example the target was
reactivated meanwhile) answers `409 lifecycle_transition_invalid` and stays `pending` (cancel it or let it expire).

The console should warn when fewer than two platform admins are active and enrolled: the four-eyes cannot complete then,
and only the `cmd/seed-admin -break-glass` deployment job can add an admin (ADR 0119 section 11).

## 4. Reason codes (closed; ADR 0119 section 10.1)

| Action | `reason_code` |
|---|---|
| suspend | `security_compromise`, `policy_violation`, `investigation`, `leave_of_absence`, `role_review`, `other` |
| offboard | `employment_ended`, `contract_ended`, `security_compromise`, `duplicate_account`, `other` |
| reactivate | `investigation_closed`, `returned_from_leave`, `error_correction`, `other` |
| role change | `role_change_approved`, `error_correction`, `other` |
| MFA reset | `device_lost`, `device_replaced`, `hard_locked`, `security_compromise`, `other` |
| invitation revoke | `no_longer_required`, `wrong_details`, `other` |

## 5. Invitation acceptance (`POST /v1/staff/invitations/accept`, tag `staff-auth`)

- Unauthenticated, rate bucket `credential` (20 per IP per minute), `no-store`.
- Body `{token, password}` (password at least 8 characters). The token comes from the invite email link
  `<backoffice>/#/enrol?token=<token>&invite=1` (fragment form only, S7a-G1).
- Success `200` returns a challenge of type `enrol` (the S3 `StaffLoginChallenge`); continue with
  `/v1/staff/auth/enrol/start` and `/enrol/confirm` exactly as after a redeem. The account is `active` from this point
  but cannot obtain a session until a factor is confirmed.
- Every refusal (unknown, expired, consumed, revoked token; an account that is not `invited`) is the single
  `401 invalid_enrolment_token`. `400` for a malformed body or a short password; `429` rate limited; `503` fault.

## 6. DTOs

`StaffDirectoryEntry`: `id`, `tenant_id` (absent for platform staff), `email`, `display_name` (nullable), `role`,
`status` (`invited | active | suspended | offboarded`), `mfa: {enrolled, hard_locked}`, `person_linked`,
`last_login_at` (nullable; the newest aal2 session `auth_time`), `created_at`, `status_changed_at` (nullable).

`StaffLifecycleEvent`: `id`, `staff_user_id`, `event` (closed: `invited`, `invite_revoked`, `invite_accepted`,
`suspended`, `reactivated`, `offboarded`, `role_changed`, `person_linked`, `mfa_enrolled`, `mfa_reset`,
`mfa_hard_locked`, `mfa_recovery_used`, `recovery_codes_regenerated`, `owner_provisioned`), `actor` (`{type: staff |
system | owner, staff_id}`; a tenant viewer sees `staff_id: null` for a platform actor, ADR 0114 H1), `before`, `after`
(objects with `status`, `role`, `staff_version`, `person_linked`), `reason_code`, `note`, `lifecycle_request_id`,
`created_at`.

`PlatformStaffLifecycleRequest`: `id`, `kind`, `state`, `target_staff_id`, `invite_email`, `reason_code`, `note`,
`requested_by` (staff id), `required_approvals` (1), `payload_hash`, `expires_at`, `created_at`, `decided_at`.

## 7. What is NOT in this contract

- Password reset (`/password-reset`, `POST /v1/staff/auth/password/redeem`, kind `platform_password_reset`):
  `NOT IMPLEMENTED` (a later slice).
- A per-tenant invitation email-domain allow-list: deferred consideration (ADR 0119 R14), not built.
- The back-office screens themselves (S8).
