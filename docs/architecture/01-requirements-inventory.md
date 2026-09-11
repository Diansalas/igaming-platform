# 01 — Requirements Inventory

Status: Stage 0 draft. Items marked `RECOMMENDATION` are not stated in the
Blueprint; they are engineering judgment and should be confirmed as the
project proceeds, not treated as fixed requirements.

## 1. Functional requirements

- Player registration, authentication, session management per brand.
- Cross-brand person resolution for self-exclusion and multi-accounting
  detection (Blueprint §4.1).
- Game launch via aggregator/direct-studio integration, with catalogue
  sync, RTP-variant handling, and jurisdiction blocklists.
- Sportsbook betting via widget/iframe on the shared wallet.
- Deposit/withdrawal via orchestrated PSP/crypto rails, with a withdrawal
  approval workflow.
- Bonus campaign creation, eligibility evaluation, grant issuance, wagering
  progress tracking, payout/forfeiture.
- KYC verification tiered to lifecycle events; AML screening and
  transaction monitoring.
- Responsible-gaming controls: deposit/loss/wager/session limits, reality
  checks, time-outs, self-exclusion.
- Back-office operations: player management, campaign management, payment
  approval, risk/case queues, CMS, reporting.
- Partner-console operations: brand provisioning, provider/PSP credential
  management, revenue-share statements, invoicing, compliance overview.
- Affiliate attribution capture and FTD/deposit/NGR postbacks.
- Regulatory and financial reporting, per-jurisdiction export formats.

## 2. Non-functional requirements

See `00-system-overview.md` NFR table (latency, availability, RPO/RTO,
reconciliation drift, report freshness, audit retention) — these are
Blueprint-sourced and binding, not aspirational.

`RECOMMENDATION`: define per-environment (dev/staging/prod) SLO
enforcement in CI/observability from Stage 1 onward so regressions are
caught before Stage 7 hardening, not during it.

## 3. Security requirements

- No secrets in code or logs; Vault/KMS-managed credentials, rotated.
- No raw PAN in our infrastructure — hosted fields/redirect only (PCI scope
  exclusion, Blueprint §4.6).
- Server-side authorization only; tenant_id never client-trusted; RBAC
  scoped by tenant.
- Session/game-token separation: brand-frontend JWT vs. single-use opaque
  game-launch token bound to (player, provider, game, currency, mode)
  (Blueprint §4.1).
- Immutable audit log for every mutating action (actor, tenant, entity,
  before/after, IP, reason code), 5–7 year retention (Blueprint §4.8).
- Four-eyes approval above configurable thresholds for manual balance
  adjustments and large withdrawals.
- Least-privilege access model; three-tier RBAC (platform admin, partner
  admin, brand operator).

## 4. Financial requirements and ledger invariants

- Append-only, double-entry ledger. `SUM(DEBITS) == SUM(CREDITS)` always.
- Never `UPDATE` a balance; balance is a recomputed projection.
- Integer minor units with per-currency exponent; `NUMERIC(38,0)` +
  per-asset exponent if crypto is in scope (floating point is a defect).
- Idempotency via unique constraint on `(provider_id, provider_tx_id)`.
- Rollbacks are compensating entries; a rollback for an unseen transaction
  writes a tombstone.
- Reconciliation: hourly recompute-vs-projection diff; non-zero drift is a
  P1 incident.
- Redis/cache never authoritative for balances; never read on the bet
  path.
- Full financial test matrix required (see `docs/testing/testing-
  strategy.md`): normal, duplicate, concurrent, retry, partial failure,
  rollback, settlement, reconciliation, callbacks, idempotency,
  authorization, auditability.

## 5. Compliance requirements

- Jurisdiction as a pluggable, first-class concept — KYC thresholds, RG
  rules, reporting formats, geo-blocking, data residency vary per brand/
  jurisdiction without a rewrite (Blueprint §1 "Design consequence").
- Mandatory, audited market blocking (Anjouan-prohibited markets: US, UK,
  France, Germany, Netherlands, Spain, Australia, Austria, FATF-blacklisted
  states), enforced in-platform and logged — not left to a CDN rule.
- Tiered KYC, recurring sanctions/PEP screening, transaction monitoring
  with a case queue and SAR export.
- GLI-19 certification readiness is a target for the B2B sale, not a
  Stage-0 deliverable (Blueprint §8) — engineering discipline
  (reproducible builds, change control) should be adopted early because
  retrofitting it is harder than starting with it.
- Distinguish software capability from legal/regulatory/licensing approval
  in all documentation and communication (`CLAUDE.md`).

## 6. Responsible gaming requirements

- Deposit, loss, wager, session limits; decrease is immediate, increase
  requires a cooling-off period.
- Reality checks, time-outs, self-exclusion at brand and platform level.
- A permanent self-exclusion flag survives account closure and
  re-registration (tied to the `person` cluster, not the `player` row).

## 7. Multi-tenant requirements

- `tenant_id` on every tenant-owned row, enforced via PostgreSQL row-level
  security bound to connection-level tenant context.
- All brand differences (theme, CMS, catalogue/ordering, payment methods,
  currencies, languages, RG defaults, bonus templates, jurisdiction rules,
  provider credentials, domains/SEO) are versioned configuration rows, not
  code paths.
- Isolation must be able to tighten (shared cluster+RLS → schema-per-tenant
  → database-per-tenant → cluster-per-tenant) as a deployment decision, not
  a rewrite (Blueprint §5).

## 8. API requirements

- API-first: brand frontend, back office, and partner console all consume
  the same platform APIs — no business logic embedded only in frontend
  code.
- REST + JWT is the Blueprint-illustrated baseline (Blueprint §3 system
  map). OpenAPI specs maintained per service under `docs/api/`.
- `RECOMMENDATION`: version APIs from day one (e.g. URI or header
  versioning) since B2B partners will integrate against them and cannot be
  broken silently.

## 9. External provider/integration requirements

Per Blueprint's build/buy table (§1) and integration-subsystem model (§1
diagram): every external integration needs, on our side, an adapter,
idempotency/retry/timeout handling, per-tenant credentials, a state
machine (pending/settled/reversed), and daily reconciliation against the
ledger — regardless of what the vendor's API itself provides. See
`13-dependency-map-and-risk-register.md` for the concrete vendor list.

## Sources

`iGaming-Platform-Blueprint.pdf`, all sections. Figures (fees, revenue
shares, timelines) are Blueprint-cited and explicitly marked in the source
as indicative/needing verification before commitment — see Blueprint's
"Sources for figures cited" page.
