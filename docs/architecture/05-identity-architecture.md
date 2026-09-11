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

## Implementation status (Stage 2)

The model above is realized with one adjustment: `Player` is split into
two distinct concepts rather than one row, per
`docs/decisions/0012-brand-distinct-from-tenant.md` and the Stage 2
instructions' explicit requirement not to collapse Person/PlayerAccount/
Tenant/Brand for convenience:

- `Person` (`internal/identity/person.go`, `persons` table) — platform-
  wide, no `tenant_id`, no RLS (not tenant-owned data). Carries only a
  partial-unique `person_key_hash` hook for the future post-KYC
  cross-brand resolution described above; Stage 2 does not populate or
  resolve it (`NOT IMPLEMENTED` — no KYC document data exists yet to hash).
  Each registration currently creates one `Person` per `PlayerAccount`;
  linking two `PlayerAccount`s to the same `Person` is Stage 4 work.
- `PlayerAccount` (`internal/identity/player_account.go`,
  `player_accounts` table) — the "player" row above: one per
  `(brand_id, email)`, `tenant_id`-owned, RLS-protected. This is the
  Brand session token's subject.
- `Brand` (`internal/identity/brand.go`, `brands` table) is new relative
  to this doc's original proposal — the consumer-facing product a
  `PlayerAccount` belongs to, distinct from `Tenant` (the commercial/
  legal entity). A `Tenant` may operate more than one `Brand`. See ADR
  0012 for why this split exists and why `brands` is deliberately
  publicly readable (brand metadata, not player data) while
  `player_accounts` is not.

Session vs. game/product tokens: the Brand session token described above
is implemented (`internal/auth/jwt.go`'s access token, short-lived,
HMAC-signed, carries `tenant_id`/subject/role/principal_type). The
game/product launch token is `NOT IMPLEMENTED` — out of scope until
Stage 3's casino/sportsbook provider integration exists to consume it;
nothing in Stage 2 hands a Brand session token to a provider, so the
"never conflate the two" invariant has nothing to violate yet.

Authentication itself moved well beyond a JWT proof-of-concept: Argon2id
password hashing, `kid`-based key rotation, single-use rotating refresh
tokens with reuse detection revoking the full session chain, Postgres-
backed login lockout, and a permission-oriented RBAC replacing Stage 1's
role-list skeleton. See the Stage 2 completion report for the full
authentication/authorization architecture and its security review.
