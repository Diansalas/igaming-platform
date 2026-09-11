# ADR 0005 — Open Business Decisions That Blocked Detailed Design

Status: **RESOLVED** — answered by the human at the Stage 0→1 gate. Source:
Blueprint §10. Kept as a historical record; see the linked ADRs for the
resolutions, which now drive architecture.

## Q1 — Crypto-first or fiat-first? → RESOLVED: both, simultaneously

Not a sequencing choice — the platform supports multi-currency,
multi-asset wallets per player from the model level up (fiat and crypto
alike). See `docs/decisions/0007-multi-wallet-per-player-model.md` and
`docs/architecture/06-wallet-ledger-architecture.md`.

## Q2 — Do B2B partners sit under our licence, or bring their own? → RESOLVED: hybrid, both

Both models are supported per tenant. See
`docs/decisions/0006-hybrid-licensing-and-jurisdiction-model.md` and
`docs/architecture/15-jurisdiction-and-licensing-model.md`.

## Q3 — Which markets in year one? → RESOLVED: Europe + LATAM, jurisdiction-aware

Modeled as multiple distinct `Jurisdiction` rows, never one ruleset per
region. See `docs/decisions/0006-hybrid-licensing-and-jurisdiction-model.md`.

## Q4 — Self-custody crypto, or a custodian? → RESOLVED: custodian abstraction

Institutional custody provider behind a `CryptoCustodyProvider`
interface; no private-key management in the core platform. See
`docs/decisions/0008-crypto-custody-provider-abstraction.md`. Specific
custodian vendor selection remains a separate, still-open human/legal
decision, not blocking Stage 1–3 engineering.

## Q5 — Is the own B2C brand confirmed as part of the plan? → RESOLVED: yes

Confirmed at Stage 0 and reaffirmed by proceeding to Stage 1.

## Q6 — Which host has confirmed in writing that it permits gambling activity? → PARTIALLY RESOLVED

Hosting **strategy** is resolved: a major hyperscale cloud provider (see
`docs/decisions/0009-hosting-hyperscale-cloud.md`), superseding the Stage 0
Hetzner/OVH recommendation. The specific provider and its **written**
gambling-AUP confirmation remain outstanding — no production deployment
happens until that confirmation exists and is documented. This does not
block Stage 1 (development-only scaffolding).

## Residual open item

Specific hyperscale provider selection (AWS/GCP/Azure) and its written
AUP confirmation, and specific custodian vendor selection (Q4 follow-up).
Both are commercial/legal tracks that run alongside engineering, not
blockers to Stage 1–3 work.
