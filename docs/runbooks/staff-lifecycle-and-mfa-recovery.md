# Runbook: staff lifecycle and MFA recovery (ADR 0119 S5/S6/S7, A10a)

Status: written in A13 against the routes in `internal/httpserver/staff_lifecycle_routes.go` and `staff_auth_routes.go`. This supersedes the
"until S6" parts of `staff-mfa-s3-cutover.md` section 5 for routine cases; the owner-SQL procedure there remains the last resort when
no platform admin can act at all. Normative request/response contract: `docs/api/openapi/platform-api.yaml` (tag `staff-lifecycle`) and
`docs/plans/adr0119-s5s6-api-contract.md`. Email delivery is MOCK in development and **PROVIDER DEPENDENT** in production: a production
binary refuses the MOCK email provider, so invitations and MFA-reset emails cannot be delivered to a person until a real adapter exists;
until then onboarding beyond the `cmd/seed-admin` bootstrap admins is blocked in production (owner decision, ADR 0119 OQ-1).

## 1. Who can do what (all mutations are critical step-up, audited, session-revoking)

| Action | Route (tenant staff: `/v1/admin/tenants/{tenantID}/staff/...`) | Permission | Notes |
|---|---|---|---|
| Invite | `POST .../staff/invitations` `{email, role, person_id?}` | `staff:manage` (platform_admin, tenant_admin) | status `invited`; 72 h token by email; Person required for `tenant_admin`, `finance`, `compliance` |
| Resend / revoke invitation | `POST .../staff/{staffID}/invitation/resend` / `.../revoke` | `staff:manage` | resend invalidates the earlier token |
| Suspend / reactivate / offboard | `POST .../{staffID}/suspend` / `reactivate` / `offboard` `{reason_code, note?}` | `staff:manage` | offboard is terminal (password, factors, sessions revoked); `security_compromise` also revokes the second factor |
| Role change | `POST .../{staffID}/role` `{role, reason_code}` | `staff:manage` | active staff only; tenant admin may only touch `tenant_admin`, `support`, `compliance`; `finance`, `risk_manager`, `promotions_manager`, `bonus_operations` need a platform admin |
| MFA reset | `POST .../{staffID}/mfa-reset` `{reason_code, note?}` | `staff:manage` | revokes factor, recovery codes, challenges, sessions (the only way to clear a hard lock); emails a 24 h `mfa_reset` token |
| Read | `GET .../staff`, `.../staff/{staffID}`, `.../lifecycle-events` | `staff:read` | |
| Platform staff | `GET /v1/admin/platform-staff[/{id}[/lifecycle-events]]`, `POST /v1/admin/platform-staff/{id}/suspend|offboard|invitation/resend|invitation/revoke` | `staff:manage`, platform caller | single actor, bounded by last-admin |
| Platform four-eyes | `POST /v1/admin/platform/staff-lifecycle-requests` kinds `platform_invite`, `platform_reactivate`, `platform_mfa_reset`; then `.../{requestID}/approve` `{payload_hash}` (or `reject`, `cancel`) | `platform_staff:govern` (platform_admin only) | requester and approver are two staff AND two Persons, neither is the target; expires in 72 h |

Rules the API enforces: no self-targeting (`403 self_target_forbidden`); the last active enrolled platform admin (or tenant admin for a tenant
caller) cannot be removed (`409 last_admin`); suspension/offboarding revoke sessions first (`409 lifecycle_busy` = retry, sessions are already
revoked). Reason codes are a closed list (contract section 4); `other` needs a 10 to 500 character note without personal data.
`platform_password_reset` is `NOT IMPLEMENTED` (password reset for staff has no path; see section 5).

## 2. Joiner

1. Tenant staff: an authorised inviter calls the invite route. Platform admin: a platform admin files `platform_invite` (needs `person_id`),
   a different platform admin approves with the displayed `payload_hash`.
2. The person opens the emailed link `<backoffice>/#/enrol?token=...&invite=1` (token in the URL fragment only; the static host must not
   log query strings, see `production-configuration-checklist.md`), sets a password (`POST /v1/staff/invitations/accept`, at least 8
   characters), enrols an authenticator (`/v1/staff/auth/enrol/start`, `/confirm`) and stores the 10 recovery codes.
3. Verify: `GET .../staff/{staffID}` shows `status: active`, `mfa.enrolled: true`; lifecycle events contain `invited`, `invite_accepted`, `mfa_enrolled`.
4. First platform admins: `cmd/seed-admin` (section 6).

## 3. Mover and leaver

- Role change or leaver: apply the route the same day; sessions end immediately (`401 session_revoked` on the next request).
- **Capability grants are NOT revoked automatically** when a grantee is suspended, demoted or offboarded (K1, `operational-runbooks.md`
  section 10; the AML quorum does drop a non-live grantee at once). Revoke the grants as part of the same change ticket
  (`capability.RevokeGrant` / the K1 revoke route). Also revoke provider-credential and other four-eyes approvals still pending from that person.
- Suspected compromise: suspend with `security_compromise` (revokes the factor too), review `staff.*` alerts and the lifecycle events,
  then offboard or MFA-reset after investigation. Treat as a security incident (`operational-runbooks.md` section 9).

## 4. MFA recovery

| Situation | Procedure |
|---|---|
| Lost device, has recovery codes | the staff member signs in with a recovery code (`POST /v1/staff/auth/mfa/recover`; raises `staff.mfa_recovery_used`, p3), then regenerates codes (`POST /v1/staff/mfa/recovery-codes/regenerate`) and re-enrols |
| Lost device, no codes left, or hard-locked (10 failures in 24 h) | an authorised manager calls `.../mfa-reset` (reason `device_lost`, `device_replaced` or `hard_locked`; `security_compromise` if a password was also correct while codes failed); the member redeems the emailed token at `POST /v1/staff/auth/enrol/redeem` with the current password, then enrols. Platform admin: `platform_mfa_reset` request, second platform admin approves |
| Only one platform admin exists, or none can sign in, or email cannot be delivered | break-glass, section 6. The four-eyes cannot complete with fewer than two active enrolled platform admins |
| Expected alert after any reset | `staff.mfa_integrity` / `staff.platform_mfa_integrity` (p1) fires when an account that had a factor has none; acknowledge it against the ticket. An unexpected one is an incident |

Alerts: `staff.mfa_hard_locked` (p2), `staff.mfa_recovery_used` (p3), `staff.session_integrity` (p1; a forged or edited session row); full table in
`staff-mfa-s3-cutover.md` section 6. Notification to a person depends on `alert-delivery-channels.md`.

## 5. Staff password reset

No staff password-reset route exists (`NOT IMPLEMENTED`). A forgotten password is handled by offboarding and re-inviting (tenant staff), or by
the owner procedure in `staff-mfa-s3-cutover.md` section 5.1 step 1 (owner-run, ticketed). Raise it as a gap if it becomes routine.

## 6. Break-glass: `cmd/seed-admin` (deployment job, owner/database authority)

`-operator` and `-reason` are REQUIRED in every mode and recorded in the audit rows. Needs the staff MFA key families, `DATABASE_URL` for the
migration/owner role, and `SEED_ADMIN_PASSWORD` (at least 12 characters, from the secret store, not the shell history) when creating an account.

```sh
# an additional platform admin when no active platform admin with a usable second factor exists
SEED_ADMIN_PASSWORD=<from the secret store> seed-admin -email=<address> -create-person -break-glass -operator=<who> -reason=<ticket>
# add -force-additional only when a usable admin exists but the situation still requires it (it is logged)
# issue a bootstrap enrolment token for an EXISTING active staff member without a second factor (creates no account)
seed-admin -email=<address> -issue-bootstrap [-tenant-slug=<slug>] -operator=<who> -reason=<ticket>
```

Human controls: two people present, ticket first, run inside the deployment environment only, deliver the token out of band, then review the audit
rows `staff.break_glass_used` (or `staff.platform_admin_bootstrapped` for the first admin) and `staff.bootstrap_token_issued` and the lifecycle event, and expect the P1 alert `staff.platform_break_glass_used`.
Deployed staging uses `deploy/aws/scripts/deploy.sh seed-admin <email>` (A13 fixed it to pass `-operator`/`-reason`; the one-off task still lacks the staff MFA key families, see gap G3 in `aws-staging-deploy-plan.md`).

## 7. Key rotation

`staff-mfa-keys.md` (factor and MAC key families; factor rotation is by re-enrolment campaign, the counting query for it is `NOT IMPLEMENTED`).
