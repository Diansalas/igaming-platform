# Runbook: payment kill switch release is wedged by a stale or expired open request

Status: written for ADR 0114 S6b. The condition below is **fail-safe**: payouts and deposits in scope stay BLOCKED.
Nothing here releases a switch by itself and nothing here may be done by editing the database.

## Symptom

A tenant admin cannot get a kill switch released:

- "Request release" on the switch answers 409 `KILL_SWITCH_RELEASE_REQUEST_ALREADY_OPEN`, or the console shows an open
  request whose Approve and Cancel buttons are off ("Platform staff filed this request").
- Approving the open request is refused 409 (stale version, expired, or tenant staff acting on a platform-filed request).

## Why it happens (all verified by live tests, migrations 0105/0106)

1. At most one release request per switch can be `open` (unique index `payment_kill_switch_release_requests_one_open`).
2. Nothing moves a request to `expired`. A request past `expires_at`, or frozen at an old switch `version` because the
   switch was re-engaged, stays `open` and keeps the slot. Approving it is refused by the database.
3. A request filed by platform staff cannot be approved or cancelled by tenant staff (409), even on a tenant-engaged
   switch. So a stale platform-filed request on a tenant-engaged switch wedges the tenant's release path.

## Recovery

1. Read the switch (`GET /v1/admin/tenants/{tenantID}/payments/kill-switches/{id}`): `open_release_request_id` names the
   request. Read it (`GET .../kill-switch-release-requests/{requestID}`): `status`, `expires_at`, `expected_version`
   against the switch `version`, `requested_by_scope`.
2. A **platform admin** cancels it: `POST .../kill-switch-release-requests/{requestID}/cancel` (204). The cancel is audited.
   A request filed by tenant staff can be cancelled by tenant staff too.
3. File a new release request (a different person from the approver), then a different principal approves it.
4. Verify: `GET` the switch shows `engaged=false`, `open_release_request_id=null`, `release_request_id` set; the audit log has
   `payments_kill_switch.cancel_release` and `approve_release` rows.

Do not: update `payment_kill_switch_release_requests` or `payment_kill_switches` directly, disable the guard triggers, or
share a staff login to satisfy four-eyes.

## Known limits (tracked, not fixed)

- LOW: duplicate filings write one denied-audit row each with no rate limit, so a hostile or looping staff client can grow
  the audit log. Requires a valid staff token with `payments_kill_switch:release`. Mitigation today: the general API rate
  limiter and monitoring of `payments_kill_switch.request_release` denied rows. Fix belongs with S6c.
- The structural fix is ticket S6c in ADR 0114 section 11 (NOT implemented).
