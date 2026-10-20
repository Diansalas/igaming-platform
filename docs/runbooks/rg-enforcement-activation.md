# Runbook: responsible-gaming limit enforcement activation (A6, ADR 0115 S4-min, migration 0158)

Status: written in A13 against the code at the A6 merge. Software capability only: no step here is a regulatory approval, and the
limit VALUES (jurisdiction floors) are a compliance decision (OQ-2, HDR-A6-*) that this repository does not make. Providers are MOCK.

## 1. What is enforced, and what is behind the switch

| Control | Enforced when | Source of truth |
|---|---|---|
| Self-exclusion | always | `rg_restrictions` (ADR 0026) |
| Time-out | always (not behind the switch; blocks launch, bet, deposit, bonus eligibility; never blocks settlement, win, rollback, void) | A6 |
| Negative-balance block (A5, owner decision 13) | always (blocks wagers, casino launch/bootstrap, bonus eligibility, withdrawals; never deposits, wins, rollbacks, settlements) | derived from the projection |
| Withdrawal RG/risk gate (WD-RG-1) | always (`WD050` RG, `WD051` risk; the 403 body is generic, the reason code is only in the audit row) | `internal/withdrawalgate` |
| Deposit, loss and wager limits | **only after the switch below is ON for the tenant or the brand** (OFF by default) | `rg_enforcement_activations` |
| Session-duration limit, reality check | **NOT IMPLEMENTED** (PROVIDER DEPENDENT / F): an enabled `session_duration` floor makes readiness fail with `session_limit_not_enforced` | n/a |

With the switch ON and no jurisdiction floor in force for a kind and asset, every deposit, real casino launch and wager of the scope is
refused (`rg_config_missing`); a mandatory kind with no player value and no numeric ceiling is refused (`rg_limit_required`). This is
fail-closed by design and is an outage vector if the order below is not followed.

## 2. The switch is ONE-WAY

`rg_enforcement_activations` is append-only (SELECT and INSERT only for the runtime role). There is no disable route and no disable
operation. `migrations/0158` down refuses (`RL099`) once a row exists. Switching enforcement off is a recorded compliance decision plus
a reviewed migration, never an application action. Do not delete or update the rows.

## 3. Order of operations (per tenant, then per brand)

Every step marked **OWNER/COMPLIANCE** needs a human decision this runbook cannot make.

1. **Floors (OWNER/COMPLIANCE for the values).** A jurisdiction floor row per limit kind (deposit, loss, wager) and per allowed currency is
   required in force for EVERY jurisdiction the tenant relies on (lic25): the tenant's LICENCE jurisdiction (if it has one), the
   jurisdictions of its approved operating bases (a documented no-licence basis names one; a licence basis uses its licence's), and every
   jurisdiction with a configuration in effect. A tenant with no licence and no approved basis is refused (`no_licence_jurisdiction`);
   a tenant with approved bases and no licence is no longer refused for that reason, it needs floors in each basis jurisdiction.
   - Platform admin A proposes: `POST /v1/admin/platform/rg/limit-config-change-requests` (platform token, critical step-up, signed actor
     proof; `effective_from` required, in the future, within the 72 h request lifetime; the content and a named signatory go in
     `attestation_ref`).
   - A DIFFERENT platform admin (different Person) approves: `POST .../{id}/approve` (critical step-up). Nothing is in force before that.
     `.../{id}/reject` and `.../{id}/cancel` exist. List: `GET /v1/admin/platform/rg/limit-config-change-requests?status=pending`.
   - Request and body schemas: `docs/api/openapi/platform-api.yaml` (`RGFloorRequestBody`).
2. **Tenant operator bounds (optional).** Tenant compliance staff may set ceilings below the floor: `GET|POST /v1/admin/rg/limit-configs`
   (`rg_config:read|write`; the write is critical step-up). lic25: one write creates the row under EVERY jurisdiction the tenant relies
   on, all or nothing, so a bound that loosens the floor of ANY of them is refused as a whole. A bound written before a basis was added
   exists only under the jurisdictions the tenant relied on then: repeat the write after adding a basis if the bound should cover it.
3. **Check readiness.** `GET /v1/admin/tenants/{tenantID}/launch-readiness?brand_id=<brand>`; `R-RG-CONFIG` must not report
   `floor_missing_<kind>`, `session_limit_not_enforced`, `session_aggregation_unsupported` or `no_currency`. Before the switch it
   reports `enforcement_not_enabled`, which step 4 resolves.
4. **Switch ON (tenant compliance staff).** `POST /v1/admin/rg/enforcement` with `{"brand_id": "<uuid, optional>", "reason_code": "<1-64 printable ASCII>",
   "attestation_ref": "<1-200 printable ASCII, the compliance record>"}`. Requires permission `rg_config:write` (role `compliance`
   only), a fresh critical step-up, a tenant-scoped token. Answers `201` (switched on), `200` (already on for that scope; nothing written),
   `409` (precondition: R-RG-CONFIG floor part is not passing; audited as `rg.enforcement.activation_denied` with the detail code), `403`
   `step_up_required` (repeat after step-up). Omit `brand_id` for a tenant-wide row, which covers every brand.
5. **Verify.** `GET /v1/admin/rg/enforcement` returns the row and `enforcement_enabled: true`. Audit has `rg.enforcement.activated`.
   In staging with synthetic data: a player with no deposit limit and a mandatory kind is refused; a player within limit succeeds;
   a player over limit is refused with the S3 figures in the audit row (not in the response).
6. Re-read launch readiness: `R-RG-CONFIG` passes. Brand activation remains a separate four-eyes act (`launch-and-suspension.md`).

## 3a. Which jurisdiction governs which check (lic25)

| Check | Jurisdiction used | If it cannot be resolved |
|---|---|---|
| Deposit, wager and casino launch enforcement (switch ON) | ONE jurisdiction, selected like KYC: the player's verified residence country (read in the same transaction), the brand, the operation (`deposit` / `wagering`) and the product, then the governing basis row's jurisdiction; a scope with no basis row uses the tenant licence's. A tenant with no approved basis: the licence jurisdiction exactly as before | the action is refused (evaluation unavailable, the transaction rolls back): no verified residence country, a basis that does not permit, no basis row and no licence |
| Setting or changing a limit, options screen, cooling-off | the STRICTEST FOLD of every jurisdiction the tenant relies on: the lowest numeric ceiling, the longest cooling-off, mandatory / enabled if any says so, the union of the option sets | a relied-on jurisdiction with no floor: an increase, a removal and a confirm are refused (`config_missing`); an initial value, a decrease, a cancel and a time-out are never refused for it; the options screen is refused |
| Time-out, self-exclusion | not blocked by any of the above | a time-out uses the floors that exist, or the platform defaults |

Examples are configuration, not law: the platform compares the configured numbers only and never decides which regime is stricter in
substance. Two jurisdictions that disagree on `calendar_tz` / `week_start` cannot be folded: reading the usage configuration for the
account-level view fails closed (enforcement itself uses one jurisdiction and is not affected).

## 4. If it goes wrong

| Symptom | Cause | Action |
|---|---|---|
| Every deposit/launch/wager of a brand refused right after step 4 | a floor lapsed or a currency was added after activation (`rg_config_missing`) | Forward-fix: put the missing floor in force (step 1). Do NOT try to delete the activation row. If player impact is unacceptable meanwhile, contain with the brand/tenant suspension or the payment kill switch (`kill-switch-and-containment.md`) |
| Activation `409` | floors absent, session control enabled, a relied-on jurisdiction (licence or basis) without floor | read `readiness_detail` in the audit row; fix configuration |
| A withdrawal refused with generic 403 | WD050 (RG: account not active, wallet frozen, negative balance) or WD051 (risk DENY or REVIEW; REVIEW has no queue yet) | read the `withdrawal.requested`/denial audit row for the code; the player is not told the reason |
| A stuck pending deposit intent keeps counting toward the deposit limit | no pending-intent TTL sweep exists (`NOT IMPLEMENTED`, owner acceptance pending) | resolve the intent through the payment procedures (`operational-runbooks.md` sections 12 and 14) |

## 5. Not covered here

Player-facing screen: b2c `/account/responsible-gaming`. Staff self-exclusion/restriction routes: `POST|GET /v1/admin/rg/restrictions`.
Staff limit writes on a player's request: `POST /v1/admin/players/{id}/rg/limits`. Reading: `GET /v1/admin/players/{id}/rg/limits|history`.
A `rg_deposit_limit_exceeded_post_facto` alert does NOT exist (`NOT IMPLEMENTED`).

A limit increase refused with `config_missing` right after a new operating basis was approved: the basis jurisdiction has no floor yet
(step 1). Decreases and time-outs keep working meanwhile.
