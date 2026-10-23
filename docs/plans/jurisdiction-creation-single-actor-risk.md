# Jurisdiction creation: single-actor path, risk statement (2026-10-23)

STATUS: **characterised, policy UNCHANGED, owner/security decision recorded as OPEN** (register row `JURIS-CREATE-GOV-1`). Nothing in code was changed for this item.

## Facts (verified on the real stack in lic32 finding 6; re-pinned by the C3 CHARACTERISATION test)

- `POST /v1/admin/jurisdictions` (`internal/httpserver/jurisdiction_admin_routes.go`): permission `PermJurisdictionRegistryManage` (platform_admin), step-up `critical`, applied immediately by one platform administrator. No maker-checker, no licensing-change kind.
- The row grants no market permission. A jurisdiction alone creates no licence, no licence country ceiling, no operating-country policy and no operating basis; the market resolver is unchanged and every market stays closed (country configuration is not proof of legality). An audit row is written (tenant NULL, platform-wide registry).

## Risk (what a single admin CAN do)

1. Add registry rows (code, name, regulator, country_code) that are inert alone but become *selectable* in later governed filings (licence create, ceiling enablement, basis filing). Those later steps remain two-person governed, so the registry row is a precondition, not a permission.
2. A mislabelled or misleading registry entry (e.g. a regulator name that suggests an authority that is not real) could be presented to the two approvers of a later filing and bias their review. Mitigation today: the approvers see the registry data and the filing reason/evidence; the registry row is append-only (no silent edit).
3. Registry pollution / denial-of-clarity (many near-duplicate codes). Duplicate code is refused by uniqueness.

Not a risk: opening a market, enabling withdrawals, changing a licence status, loosening any control.

## Options (RECOMMENDATION, not a decision)

- (a) Leave as is: single actor with critical step-up and audit (current).
- (b) **Recommended before a second operator / B2B tenant is onboarded:** put creation behind a licensing-change kind `jurisdiction_create` (widening-class: two distinct platform Persons, signed actor proof, 24 h freshness, 72 h expiry), reusing the existing primitive. Migration + Back Office dialog change; roughly one slice.
- (c) Keep single actor but add an alert/audit review item.

## Decision required

Owner + security: choose (a)/(b)/(c). Until chosen, behaviour (a) stands and is pinned by the characterisation test. This is a governance decision, not an engineering blocker.
