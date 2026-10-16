# Runbook: kill switches and containment (payments, games, markets, brands)

Status: written in A13 as the single entry for "stop something now". Every control below is `IMPLEMENTED`; the PSP, casino and sportsbook
behind them are MOCK. Engaging is fast and single-actor on purpose; releasing is slower and four-eyes. Nothing here edits a database row.
Choose the NARROWEST control that stops the harm, in the order of this table.

| Harm | Control | Who / how | Reversal |
|---|---|---|---|
| One PSP failing, or suspected payment compromise, for a tenant | **Payment kill switch** (section 1) | `payments_kill_switch:engage` (platform_admin; tenant_admin for its own tenant), single actor, no step-up | four-eyes release request + approval (section 1.3) |
| One casino game misbehaving (RTP/integrity, vendor request) | **Game status `disabled`** (section 2) | platform_admin catalogue manage, or the tenant opt-out | re-enabling a disabled game is four-eyes |
| A country must close (licence, sanctions, regulator) | **Disable write in the operating-market tables** (`market-blocking-configuration.md` section 5) | authorised human with a reviewed Go call; no route | enabling write with an authorisation reference |
| A brand or tenant must stop (integrity, compliance, fraud, licence) | **Suspension** (`launch-and-suspension.md` section 4) | anyone holding `launch_authorisation:suspend`, single actor, mandatory reason | new four-eyes activation (reactivate) |
| Application misbehaving, not a business control | roll back the build (`operational-runbooks.md` section 2) or stop `platform-api` (fails closed) | operator | redeploy |

Before and after any containment: note the time, who, why, and tell the people the system cannot (players, brand operator, finance, compliance). Declare
an incident when money or a compliance control is involved (`incident-response-and-oncall.md`).

## 1. Payment kill switch (ADR 0095 section 10, migrations 0105/0106, ADR 0114 S6b)

Scope: a tenant, a `provider_scope` (a provider id or `*`) and an `operation_scope` (`deposit`, `payout` or `*`). While engaged, new
deposits/payouts matching the scope are withheld; in-flight work and polls continue (the payments sweeper still resolves).

### 1.1 Engage

`POST /v1/admin/tenants/{tenantID}/payments/kill-switches` with `{"provider_scope":"<provider id or *>","operation_scope":"deposit|payout|*","reason_code":"<code>"}`.
All three fields required; `operation_scope` outside the three values is `400`. Idempotent per (tenant, provider, operation): engaging an engaged
switch changes nothing. A tenant-scoped caller may name only its own tenant; a platform caller may name any. A switch engaged by platform staff cannot be
released by tenant staff. The engage writes an audit row and emits the Error-level log event `payments_kill_switch_engaged_alert` and the durable
alert `payment.kill_switch_engaged` (p2); delivery to a person depends on `alert-delivery-channels.md`.

### 1.2 Read

`GET .../payments/kill-switches` and `GET .../payments/kill-switches/{killSwitchID}` (`payments_kill_switch:read`): `engaged`, `engaged_by_scope`,
`version`, `open_release_request_id`. Find out who engaged it (audit `changed_by`) before releasing.

### 1.3 Release (four-eyes)

1. Requester: `POST .../kill-switches/{killSwitchID}/release-requests` `{"reason_code":"..."}` (`payments_kill_switch:release`, critical step-up).
2. A DIFFERENT principal approves: `POST .../kill-switch-release-requests/{requestID}/approve` (critical step-up). Cancel: `.../cancel`.
3. Before approving: reconciliation is clean for the tenant, the provider is healthy, queued work behaves as expected (sweeper state,
   `operational-runbooks.md` section 12). Verify afterwards: the switch shows `engaged=false` and the audit log has `approve_release`.
4. Wedged release (a stale or expired open request keeps the slot): `payment-kill-switch-release-recovery.md`.

## 2. Casino game availability

- Platform wide: `PUT /v1/admin/casino/games` (platform_admin, `casino_catalogue:manage`) with the game's definition and `"status":"disabled"`.
  Since A3 (CAS-GAME-KILL-BET-1) a NEW bet on a game whose status is not `active`, or with no/disabled tenant or brand availability, is declined
  (`game_unavailable`, audit `casino_bet.denied_game_unavailable`); wins, rollbacks, settlements and replays of existing exposure are never blocked.
  Residual (R-3): a kill committed concurrently with an in-flight bet is not ordered by a lock.
- One tenant: `PUT /v1/admin/casino/games/{gameID}/availability` (tenant-scoped, `casino_config:write`).
- Provider capability for a tenant: `PUT /v1/admin/casino/providers/{providerID}/capability` (critical step-up).
- Re-enabling a disabled game, or removing a jurisdiction from a blocklist, is the four-eyes catalogue change:
  `POST /v1/admin/casino/games/{gameID}/change-requests`, then a different platform principal
  `POST /v1/admin/casino/change-requests/{requestID}/approvals` (both critical, `casino_catalogue:govern`).

## 3. Verifying containment actually holds

After engaging: attempt the blocked action as a synthetic player in a non-production stack and confirm the refusal; confirm the alert/audit row;
watch `payments_kill_switch`-related and `http_request` 5xx signals for collateral damage (`observability-and-alerting.md`). A switch is
not a ledger control: money already in flight still settles and reconciliation keeps running.

## 4. Not covered

A global "stop all play" switch does not exist; use tenant/brand suspension. A sportsbook-specific market kill is the operation rung of the
market tables (`sportsbook_wagering`). Automatic engagement on a threshold does not exist (`NOT IMPLEMENTED`).
