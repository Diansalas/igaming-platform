# ADR 0124: Operating basis (per tenant/brand/country/operation) and rulebook selection

Status: ACCEPTED as the engineering design on branch `claude/licensing-jurisdiction-completion` (migrations 0180, 0183). Detail and proposed AMENDMENT text: `docs/plans/lic9-operating-basis-record.md` (section on ADR 0045 AMENDMENT-4), `docs/plans/lic14-record.md` section 7. Not a legal opinion; no jurisdiction is declared licence-exempt and no row is shipped.

## Context
Owner clarification 2026-10-20: no preselected country list; the business chooses countries and arrangements later by configuration; arrangements may be the platform's licence, a partner's licence, or a documented, reviewed no-licence basis; country configuration is not proof of legality; no-licence is never inferred.

## Decision
1. `operating_bases` (append-only, versioned, FORCE RLS, platform-written only through an executed licensing change) records the basis for a scope (tenant, optional brand, country, operation, optional product): `platform_licence`, `partner_licence` or `no_licence_documented`. A no-licence row requires a jurisdiction, a determination reference, a responsible role and a mandatory `review_due_at`; the review-due date is the last valid day.
2. A scope with no basis row resolves exactly as before (tenant licence chain + `licence_country_ceilings`). A scope with a row is decided by that row: licence kinds run licence validity and the ceiling on the row's licence (overriding `tenants.licence_id` for the scope); a no-licence row permits only when active, approved by an executed change, in window, not past review and not flagged for re-review. Contradictions (two rows, or a no-licence row beside a licence ceiling for the country) are `configuration_conflict` (fail closed). A wagering question that names no product is refused when the basis restricts any product.
3. Governance: `operating_basis_set` always widens (2 distinct Persons); `operating_basis_withdraw` and review that shortens or flags are single-actor narrowing; owner-written rows (no executed change) never create permission (`change_request_id` NULL = not approved) and an owner UPDATE without an executed request is refused (LC041); every closed row needs a same-scope successor (LC044).
4. Partner-licence exclusivity is permanent (a tenant-licensee licence ever used by another tenant's basis cannot be used or bound, LC043), under one advisory lock.
5. Rulebook selection follows the governing basis (H-1): KYC deposit/play thresholds use the basis row's jurisdiction (or the basis licence's); when the tenant has any approved basis and the scope cannot be resolved the result is `unavailable` (deny), never the dormant `not_required` default. RG floors and payout freshness have no country context yet: a tenant with an approved basis and no licence is refused there (follow-up F1: scope-aware RG).
6. Accepted residuals: write-time policy ceiling relaxation is per tenant and country (R9/L-2); readiness covers casino wagering only; `effective_from` is DB-stamped (no scheduled activation).

## Consequences
Markets are opened by configuration through the Back Office (platform_admin, 2 approvers) with no release. The platform never decides legality; the basis row records who decided, the evidence reference and the review date.
