# 05 — Identity Architecture Proposal

Status: Stage 0 proposal. Source: Blueprint §4.1.

## Model

- One `player` row per `(tenant_id, email)`. Player identity is per brand
  — the same human can legitimately hold accounts at multiple partner
  brands.
- One `person` cluster, resolved post-KYC from a hash of document number +
  date of birth, linking `player` rows across brands/tenants that belong
  to the same human.
- Self-exclusion, fraud links, and AML case history attach to `person`,
  never to `player` — otherwise platform-level self-exclusion and
  multi-accounting detection (both audited) are impossible.

## Session vs. game/product tokens

Two different token types, never conflated:

1. **Brand session token** — short-lived JWT, identifies player + tenant,
   used for brand-frontend/back-office API calls.
2. **Game/product launch token** — separate, single-use, opaque, bound to
   `(player, provider, game, currency, mode)`, short TTL, minted at launch
   and exchanged once by the provider for their own session.

A provider is never handed the brand session token. A leak at any one of
potentially thirty+ integrations must not become account takeover across
the whole platform — this is a `security`-reviewed invariant, not a
suggestion.

## Ownership

`identity-compliance` owns this model; `security` reviews token issuance
and scoping; `architect` signs off on the schema since it's referenced by
KYC/AML, RG, and audit.

## Open dependency

Cross-brand person resolution assumes B2B partners' players are visible to
the platform-level `person` cluster. Whether partners bring their own
KYC/AML vendor relationship (Blueprint §10 Q2) affects how much of this
resolution can be automated vs. requires a partner-side data-sharing
agreement — flagged in `docs/decisions/0005-open-business-decisions.md`.

## Stage mapping

Built in Stage 2 (Identity + tenancy + security). KYC/AML orchestration
that hangs off this model is Stage 4 (see `11-kyc-aml-rg-architecture.md`).
