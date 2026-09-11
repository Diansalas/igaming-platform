# 12 — Back Office, Partner Console, Audit and Reporting Architecture Proposal

Status: Stage 0 proposal. Source: Blueprint §4.8, §4.9.

## RBAC

Three-tier: platform administrator (us), partner administrator, brand
operator. Permissions are scoped by tenant and never inferred from the UI
— every mutating action is authorized server-side regardless of what the
requesting UI displayed.

## Audit

Every mutating action writes an audit record — actor, tenant, entity,
before state, after state, IP, reason code — to an append-only store with
5–7 year retention. Manual balance adjustments require a reason code and
four-eyes approval above a threshold. This is the first thing an auditor
asks to see; its absence has ended platform businesses (Blueprint §4.8) —
treated as core infrastructure, not defensive extra work.

## Back office and partner console

Two distinct surfaces (see `00-system-overview.md`), both API-first,
sharing the same RBAC and audit substrate but serving different users with
different usability bars — back office must be usable by a non-technical
retention manager; partner console serves us and licensees for
provisioning, credentials, revenue share, and compliance overview.

## Reporting and BI

Never run reports against the transactional ledger. Change data capture
(Debezium) feeds the event bus, which feeds ClickHouse — the operational
database keeps its latency budget, analysts get columnar speed (Blueprint
§4.9).

Daily reports: GGR/NGR by brand/game/provider/country/currency/day, bonus
cost, provider cost, PSP cost, first-time depositors, retention cohorts,
player LTV. Sportsbook reports must carry an explicit open-liability line
(see `09-sportsbook-architecture.md`).

Regulatory exports are per-jurisdiction, behind a pluggable interface —
same reasoning as the jurisdiction-pluggability design consequence in
`01-requirements-inventory.md` §5.

## Ownership and stage mapping

Back office/partner console/audit: `backoffice` (implementation),
`security` (RBAC/audit review). Reporting/BI: `data-analytics`, with
`ledger-finance` review on anything presented as a financial figure. Audit
foundation is Stage 2 (alongside identity/tenancy); full back office,
partner console, and reporting pipeline are Stage 6.
