# Runbook: operating-market blocking (A8, `internal/marketgate`, ADR 0045)

Status: written in A13 against the code at the A8 merge. Software capability only; which countries a brand may serve is a licensing and
legal decision (HDR-J-6/7/8/9, HDR-M-1/2, MKT-DUAL-1 are OPEN: **OWNER/COMPLIANCE**). The geolocation source is a MOCK
(`PROVIDER DEPENDENT` for production).

## 1. What the gate does

One server-side gate refuses **registration**, **deposit initiation** and **casino launch (and launch-token mint)**, and **sportsbook
bet placement** (operation `sportsbook_wagering`, SR8-1), unless the country is open for that brand and operation. Evidence is the `geolocation.Locator` result only; a
client-declared country is never used. Anything but a resolved valid ISO-3166-1 alpha-2 code (no locator, error, timeout, unknown)
**refuses**. The refusal is `403 MARKET_NOT_AVAILABLE` with one body for every cause (no oracle); the cause is in the staff-only audit
row `market.access_refused` (operation, reason code, evidence resolved|unknown, brand, outcome, policy version; never a country or IP).
Not gated by design: PSP deposit credit callbacks, casino bet/win/rollback callbacks, withdrawals, bonus, catalogue listing. A consumed
casino session may take NEW bets only for `CASINO_SESSION_MAX_LIFETIME` (default `4h`, 1m..72h) after launch.

A freshly migrated database has no country rows, so **everything is refused**. That is the designed state.

## 2. Configuration model (three rungs, all versioned and append-only)

| Rung | Table | Written by | Scope of the call |
|---|---|---|---|
| Licence ceiling per (licence, country) | `licence_country_ceilings` | `operatingmarket.CreateLicenceCountryCeilingVersion` | platform admin transaction (`WithPlatformAdmin`) |
| Tenant opt-in per country (absence = terminal block) | `operating_country_policies` (scope tenant) | `operatingmarket.CreateOperatingCountryPolicyVersion` | tenant transaction |
| Brand and operation narrowing | `operating_country_policies` (scope brand / operation) | same | tenant transaction |

**There is no HTTP route and no console for these rows** (ADR 0045: zero routes; `NOT IMPLEMENTED`). A change is a reviewed Go call (a one-off
operator tool or script that uses the functions above; an agent never runs it against a shared or production database) by a human with
legitimate access. Rules enforced by the package and by migration 0076:

- Enabling (state `enabled`, status `active`) requires a non-empty `AuthorizationReference` at every rung; withdrawing a brand/operation
  row also needs one (it removes a block). **Disabling needs none** (the emergency stop, section 5).
- A tenant policy cannot enable a country above the licence ceiling (`ErrCeilingExceeded`).
- A change takes effect on the next request (the gate reads the database clock); old versions stay as history.
- Call at most once per key per transaction.
- MKT-DUAL-1 (four-eyes on market enablement) is OPEN: until it is built, require two humans by process (one prepares, one reviews the
  script and the `AuthorizationReference` before it is run) and record both in the change ticket. **OWNER decision.**

## 3. Environments

| Environment | What is required for registration/deposit/launch to work |
|---|---|
| local dev / staging (mock components, non-production) | (1) `MOCK_GEOLOCATION_COUNTRY=<ISO code>` on `platform-api` (honoured only when `TEST_SUPPORT_ENDPOINTS_ENABLED=true`; `config.Load` refuses it under `APP_ENV=production`), AND (2) a licence ceiling plus a tenant opt-in row for that country and the brand (and no narrowing row) |
| production | a real production-eligible `Locator` (`PROVIDER DEPENDENT`, not built). The MOCK locator is registered with the synthetic-component guard (`geolocation/locator`), so a production process cannot start on it |

`deploy/aws/modules/ecs` sets `MOCK_GEOLOCATION_COUNTRY` only when the owner supplies the root variable `mock_geolocation_country`
(STAGING-PREP-1, gap G3 in `aws-staging-deploy-plan.md`); unset, the gate refuses everything. The market rows themselves are still an owner action
(there is no route or tool; see the plan, owner action C).

## 4. Verify a configuration

1. `GET /v1/admin/tenants/{tenantID}/launch-readiness?brand_id=` : `R-MARKET-CONFIG` passes when at least one country is open for
   registration, deposit and casino wagering. `no_market_configured` = the tenant never opted a country in;
   `no_country_permits_all_operations` = a ceiling, brand or operation row closes every country.
2. End to end in a non-production stack: register, deposit-initiate and launch as a synthetic player; then change the row to disabled and
   confirm the next request is `403 MARKET_NOT_AVAILABLE` and one `market.access_refused` audit row exists.
3. `operatingmarket.ExplainOperatingCountryPolicy` returns the rung that decided (staff tooling only; never expose it to a client).

## 5. Emergency stop for one country

Write a **disable** version (state `disabled`, status `active`) at the narrowest sufficient rung (operation, brand, or tenant; or the
licence ceiling for every tenant on that licence). It needs no authorization reference and is effective on the next request. Record the
reason code and ticket. Re-enabling is an enabling write with an `AuthorizationReference`. Existing sessions: new casino bets continue
until `CASINO_SESSION_MAX_LIFETIME` expires; wins, rollbacks and settlements of existing exposure are never blocked.

## 6. Failure modes

| Symptom | Cause | Action |
|---|---|---|
| Everything 403 `MARKET_NOT_AVAILABLE` in staging | no `MOCK_GEOLOCATION_COUNTRY`, or no rows | section 3 |
| 503 on register/deposit/launch | `Deps.MarketGate` is nil (wiring defect; production entry points fail closed) | report as a defect; do not bypass |
| Refusals only for some players | evidence unknown (locator error) vs policy refusal: compare `evidence` in the audit row | locator health (PROVIDER DEPENDENT in production) |
| Deposit initiated, callback after a later disable | credit is not gated (open question: refund/hold path is a ledger-finance decision) | treat as a normal deposit; escalate if the market closure requires holding funds |
| A sportsbook market is closed by the operation rung | operation code is `sportsbook_wagering` (casino bets: `casino_wagering`) | write the disable at that operation |
