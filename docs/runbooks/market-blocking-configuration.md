# Runbook: operating-market blocking (A8, `internal/marketgate`, ADR 0045)

Status: written in A13 against the code at the A8 merge; updated for owner decision 3 (`docs/plans/market-evidence-kyc-record.md`). Software capability only; which countries a brand may serve is a licensing and
legal decision (HDR-J-6/7/8/9, HDR-M-1/2, MKT-DUAL-1 are OPEN: **OWNER/COMPLIANCE**). The CURRENT platform market-evidence source is the player's
**KYC-verified residence** (production-eligible, `IMPLEMENTED`); vendor geolocation can be added later (`PROVIDER DEPENDENT`, not invented).

## 1. What the gate does

One server-side gate refuses **registration**, **deposit initiation** and **casino launch (and launch-token mint)**, and **sportsbook
bet placement** (operation `sportsbook_wagering`, SR8-1), unless the market is open for that brand and operation.

**Evidence (owner decision 3).** For deposit, launch and placement the country is the player's **KYC-verified residence**
(`marketgate.KYCResidenceLocator`, read through the tenant session / RLS; never from the client). It exists only when the player account's
LATEST decided verification is `approved` and not expired AND a reviewer recorded a residence determination
(the staff verification review with `verified_residence_country`, for a tenant whose `verified_residence` evidence collection is
active). Unverified, pending, review_required, rejected, expired, no determination, no KYC record, a database error, a timeout: **no evidence, refused**
(fail closed). A new pending re-verification withdraws the evidence on the next request. **Registration** happens before KYC can exist, so it
takes no player evidence and no client value: it is permitted only while the brand's configuration permits `registration` for **at least one
country** (PD-MKT-REG-1, owner can reverse; an unconfigured or closed brand refuses). A registered account has no residence evidence and so cannot
deposit, launch or place a bet until KYC verifies a residence in an open market. The refusal is `403 MARKET_NOT_AVAILABLE` with one body for every cause (no oracle); the cause is in the staff-only audit
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

**Since migration 0177 (MKT-DUAL-1) every runtime write of these rows goes through the licensing change primitive**:
`POST /v1/admin/licensing-changes` with kind `licence_ceiling_version` or `operating_policy_version`, then two approvals by two
other Persons for a widening change (enable; a withdrawal at the brand / operation rung). A disable, or a withdrawal at the tenant
rung, executes immediately (single actor with a reason code). The database refuses any other runtime write (LC040). Procedure:
`licensing-change-approvals.md`. Back Office filing dialogs exist since lic31 (licence page: country ceilings; tenant licensing page: operating-country policies and jurisdiction configurations; `PARTIALLY IMPLEMENTED`: mock-tested only, see `docs/plans/lic31-record.md`). The table owner path
(`cmd/seed-market`, synthetic staging / development only, refuses production) is unchanged. Rules enforced by the package and by
migration 0076:

- Enabling (state `enabled`, status `active`) requires a non-empty `AuthorizationReference` at every rung; withdrawing a brand/operation
  row also needs one (it removes a block). **Disabling needs none** (the emergency stop, section 5).
- A tenant policy cannot enable a country above the licence ceiling (`ErrCeilingExceeded`).
- A change takes effect on the next request (the gate reads the database clock); old versions stay as history.
- Call at most once per key per transaction.
- MKT-DUAL-1 (four-eyes on market enablement) is IMPLEMENTED in software by migration 0177 for every runtime writer. The approver
  roles and count are an INTERIM answer (platform_admin only, two approvals): HDR-M-1 remains an **OWNER decision**
  (`licensing-change-approvals.md`, section 7).

## 3. Environments

| Environment | What is required for registration/deposit/launch to work |
|---|---|
| local dev / staging (mock components, non-production) | (1) a licence ceiling plus a tenant opt-in row for the country and the brand (and no narrowing row); (2) `verified_residence` evidence collection activated for the tenant (the jurisdiction evidence-collection admin route, step-up critical); (3) per test player: a verification (MOCK KYC provider where test support is on) approved by a compliance reviewer **with** `verified_residence_country` in an open country. `MOCK_GEOLOCATION_COUNTRY` is now **optional**: leave it unset to exercise the real KYC evidence path; set it (honoured only when `TEST_SUPPORT_ENDPOINTS_ENABLED=true`; `config.Load` refuses it under `APP_ENV=production`) only to force one static country for every request and bypass KYC, e.g. for the loadtest smoke |
| production | the KYC residence source needs no vendor and is wired by default. Startup guards: `refuseSyntheticInProduction` accepts it (it is `ProductionEligible`, not Synthetic) and refuses the static MOCK Locator by name `geolocation/locator`; `requireMarketEvidenceInProduction` refuses to start with no `geolocation/locator` component. Everything else (market rows, KYC operations, `verified_residence` collection active) is configuration |

`deploy/aws/modules/ecs` sets `MOCK_GEOLOCATION_COUNTRY` only when the owner supplies the root variable `mock_geolocation_country`
(STAGING-PREP-1, gap G3 in `aws-staging-deploy-plan.md`); unset (the default and the recommended staging state) `platform-api` uses the KYC residence
source and starts normally. The market rows themselves are still an owner action
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
| Everything 403 `MARKET_NOT_AVAILABLE` in staging | no market rows; or (deposit/launch/bets) the player has no KYC-verified residence (not approved, expired, no reviewer determination, tenant `verified_residence` collection inactive, residence in a closed country); audit `evidence=unknown` means no residence evidence, `resolved` means the market refused | section 3; registration fails only when no country is open for the brand |
| 503 on register/deposit/launch | `Deps.MarketGate` is nil (wiring defect; production entry points fail closed) | report as a defect; do not bypass |
| Refusals only for some players | evidence unknown (no verified residence, or a database error) vs policy refusal: compare `evidence` in the audit row (`brand_level` = a registration refusal) | the player's KYC status; a database error is also logged |
| A verified player is refused after re-verification starts | a newer decided `pending`/`review_required`/`rejected`/`expired` verification withdraws the evidence (same rule as KYC enforcement) | complete the review; this is by design |
| Deposit initiated, callback after a later disable | credit is not gated (open question: refund/hold path is a ledger-finance decision) | treat as a normal deposit; escalate if the market closure requires holding funds |
| A sportsbook market is closed by the operation rung | operation code is `sportsbook_wagering` (casino bets: `casino_wagering`) | write the disable at that operation |
