# ADR 0005 — Open Business Decisions That Block Detailed Design

Status: OPEN — requires Fernando's decision, not Claude's. Source:
Blueprint §10. Architecture proceeds provisionally (pluggable/config-first)
on all six axes until answered; each answer materially changes downstream
design, so treat any schema/service built against a default assumption
below as provisional until confirmed.

## Q1 — Crypto-first or fiat-first?

Changes ledger numeric precision, the entire payment layer, KYC
thresholds, and the PSP shortlist. Given Anjouan's banking reality this
leans crypto-first, but the Blueprint is explicit that it should be a
decision, not a default. **Provisional assumption for Stage 0–3 design:**
support both, ledger uses `NUMERIC(38,0)` + exponent (crypto-capable)
regardless of which is prioritized first commercially.

## Q2 — Do B2B partners sit under our licence, or bring their own?

Determines aggregator contract shape, whether provider credentials are
per-platform or per-tenant, how provider cost is attributed, and how
strictly tenants must be isolated. The single highest-leverage commercial
question per the Blueprint. **Get this answered before the game-gateway
credential-storage schema is finalized** (Stage 3/4).

## Q3 — Which markets in year one?

Drives languages, currencies, PSP selection, KYC vendor coverage, game
jurisdiction blocklists, and RG defaults. "Everywhere that isn't blocked"
is not an answer a payment provider will accept.

## Q4 — Self-custody crypto, or a custodian?

A security/insurance decision more than a technical one. Blueprint's read:
at year one with a small team, a custodian (Fireblocks/BitGo) is usually
right despite the cost, because it removes an entire category of
existential risk. **Must be answered before crypto-rail implementation
begins** (see `docs/architecture/07-payments-architecture.md`).

## Q5 — Is the own B2C brand confirmed as part of the plan?

Blueprint §1 argues strongly yes — the master project instructions confirm
this is the plan (own brand first, then B2B). **Treated as answered: yes**,
unless the human says otherwise.

## Q6 — Which host has confirmed in writing that it permits gambling activity?

Cheap to check now, expensive to discover after certification. No
infrastructure work should target a specific host before this is
confirmed in writing (Blueprint §7 hosting row: "VERIFY FIRST").

## Disposition

Q1, Q3, Q4, Q6 are commercial/legal/vendor decisions outside engineering's
authority to make — surfaced to the human via the Stage 0 completion
report. Q2 is the highest-leverage one and should be prioritized for an
answer before Stage 3/4 schema work locks in an assumption. Q5 is treated
as resolved (yes) per the master project brief.
