# Licensing and jurisdiction: configurable operating model (owner clarification 2026-10-20)

Supersedes the framing (not the facts) of `licensing-owner-decision-requests.md`: questions that only concern WHICH markets or arrangements the business chooses are **operational decisions made later through configuration, not blockers to technical completion**. Nothing here is a legal opinion; no jurisdiction is declared licence-exempt; no country, threshold or licence value is shipped by the platform.

## 0. Owner requirement as engineering requirements

| # | Requirement | Where it is met |
|---|---|---|
| R1 | Configurable countries/markets and operation-specific policies | `licence_country_ceilings`, `operating_country_policies` (tenant/brand/operation rungs), governed writers (0177), read APIs (0178); no allowlist in code |
| R2 | Basis per tenant/brand: our licence, a partner's licence, or a documented reviewed no-licence basis | licence kinds: hybrid model ADR 0006 (done). **No-licence basis and per-brand/per-country governing licence: lic9 (migration 0180), in progress** |
| R3 | Basis, evidence, review status, restrictions, decision-maker, audit history explicit | licences: evidence, review-due, history (0176-0179). Operating basis rows (lic9): determination reference, evidence, review due, restrictions, approvers via executed change |
| R4 | Authorised admins change country/basis config without a release | governed licensing changes + Back Office; lic9 adds basis kinds (UI filing: see residuals) |
| R5 | Country configuration alone is not proof of legality | lic9: a market opens only through an explicit basis (licence ceiling or documented basis); ceilings/policies without basis resolve not-permitted |
| R6 | No-licence is never silent | lic9: `no_licence_documented` requires determination reference, jurisdiction, review-due; absent = today's behaviour (licence required); conflict with a ceiling = fail closed; overdue review = not permitted |
| R7 | Activation/suspension governed and audited | ADR 0112 launch governance, unchanged |
| R8 | Fail closed when jurisdiction evidence unresolved; no universal licence or no-licence rule | unchanged resolver; lic9 adds no default in either direction |

## 1. Q1-Q16 re-triaged

Legend: **A** technical, can be completed now. **B** business configuration, decide later (preserved as configurable). **C** legal/compliance determination needing evidence or professional review (the platform records the outcome, it never decides). **D** genuine technical/safety blocker.

| Q | Question | Split | Treatment |
|---|---|---|---|
| Q1 | Permitted countries per product/operation | B + C | **No longer a prerequisite.** Configuration only (ceilings/policies/operating bases). Until configured, nothing is open (safe default). Legal determination per country is C and is recorded as evidence reference on the basis row. |
| Q2 | Residence vs location | C (+ vendor if location needed) | Per-jurisdiction/operation evaluation policy mechanism exists (default OFF, library). A geolocation vendor is only needed if a determination says so (class D then). Not a blocker; no provider-independent work remains except exposing the writer when a location provider exists. |
| Q3 | Registration of excluded-market visitors | B | Already per-brand configuration (registration is an operation in the market policy); ask when the first brand is configured. |
| Q4 | RG floor values | B + C values | Mechanism exists: enforcement per brand OFF until floors are configured; switching ON is refused without floors (safety control, already implemented). Values supplied later by compliance via configuration. |
| Q5 | Monthly limits legally required? | C | Mechanism: windows up to 7 days. A monthly rollup is built only if a determination requires it (record as conditional A). |
| Q6 | KYC thresholds | B + C values | **A: governed KYC policy authoring routes (lic10) so thresholds are entered later without a release.** Nothing shipped; the lic7 readiness item flags a brand with no policy. |
| Q7 | AML monitoring at launch; officers | A mechanism, B/C decision | **A: AML screening/monitoring trigger wiring with the MOCK provider and configurable trigger policy (lic10).** Whether a brand may launch without monitoring stays an acknowledged per-brand decision (R-AML-MONITORING always fails honestly until real monitoring exists; real screening vendor is class D). |
| Q8 | Licence expiry boundary; alert lead time | C (boundary) + B (lead time) | Conservative boundary stays; the 30-day alert window is a constant (record: make configurable if the business wants another value; not a blocker). |
| Q9 | Approver counts | Technical safety constant | Counts are control FLOORS (two distinct approvers for widening; activation counts forced by trigger). Not configurable downward by design; raising them is a governed migration. Business choice limited to "raise". |
| Q10 | Mandatory evidence reference on activation | B | Optional note today; making it mandatory is a small policy change when chosen. |
| Q11 | Who may approve | B + A | Interim: platform_admin x2 distinct Persons. A configurable approver-role set per change kind is a future design item (not needed to operate); recorded, not a blocker. |
| Q12 | Withdrawals under suspension/expiry | **Safety-critical control** + C | Current built behaviour (withdrawals/payouts/settlement continue; new deposits/play/registration stop) is the safe default because it never traps player funds. Any tenant-specific restriction is a legal determination (C) and would need an explicit configurable mechanism then; none is invented now. |
| Q13 | Auto-suspend on expiry | B | Alert-only today; the runtime gate already fails closed on an expired licence. |
| Q14 | Revoked status | A (done) | Modelled as `expired` + reason code. |
| Q15 | Licences per tenant / per brand | **A (design decision, resolved)** | **lic9: the governing licence can be chosen per tenant/brand/country/operation via operating basis rows; the tenant's own licence remains the default.** Hybrid arrangements inside one tenant become configuration. |
| Q16 | Tenant read of licence history | B (product) | Not built; redacted projection possible later. Security advises against opening `licence_versions`. |

## 2. Report buckets

**A. Technical capability completable now (in progress or done):** governed licensing changes (done), licence lifecycle (done), read APIs and Back Office licensing section (done), launch readiness items (done), operating basis model with no-licence documented basis and per-scope governing licence (lic9), AML trigger wiring with MOCK provider (lic10), residence maker-checker as configurable policy (lic10), KYC policy authoring routes (lic10), Back Office screens for the new kinds (follow-up slice after lic9).

**B. Business configuration decided later:** country list per product/operation/brand, operating arrangement per scope, RG floor values, KYC thresholds, registration policy for excluded markets, alert lead time, auto-suspend preference, approver raising, evidence-mandatory choice.

**C. Legal/compliance determinations needing evidence or professional review:** whether a country needs a licence for a product; whether a documented no-licence basis is valid and for how long; residence vs location duty; RG/KYC/AML legal values; monitoring-at-launch acceptance; expiry boundary; handling of funds under suspension. The platform records the determination reference, role, review-due date and approvers; it never makes or implies the determination.

**D. Genuine technical/safety blockers:** none for provider-independent completion. Provider-dependent: production KYC vendor (residence evidence), screening/monitoring vendor, geolocation vendor if required. AWS-dependent: runtime-role split, kill switch, backups in a deployed environment. Safety controls that stay fixed: fail-closed resolution, approver floors, no-licence only by explicit documented basis, withdrawals not trapped by default.
