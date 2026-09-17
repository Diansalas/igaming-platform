# Security Architecture Proposal

Status: Stage 0 draft. Owned by the `security` specialist going forward.
Source: Blueprint §4.1, §4.6, §4.7, §4.8, §7, plus `CLAUDE.md`.

## Authentication and session model

- Brand-frontend players: short-lived JWT per session, tenant-scoped.
- Game/product launch: separate single-use opaque token bound to
  `(player, provider, product, currency, mode)`, short TTL — never the
  session JWT. A leak in one of many provider integrations must not become
  account takeover platform-wide.
- Back office / partner console staff: separate authentication path from
  players, with RBAC scoped by tenant.

## Authorization

Three-tier RBAC (platform admin / partner admin / brand operator).
Enforced server-side on every request; never inferred from UI state;
`tenant_id` always derived from authenticated context, never accepted from
the client.

## Tenant isolation

PostgreSQL row-level security bound to connection-level tenant context is
the enforcement mechanism (see `docs/architecture/03-database-
architecture.md`). Every new endpoint requires a test proving a valid
token for tenant A cannot read or write tenant B's data.

## Secrets

Vault or a cloud KMS. Provider HMAC keys rotate; PSP credentials are
per-tenant. No secret ever lives in a config file committed to git, an
environment variable dumped to logs, or an API response.

## PCI scope

Hosted fields/redirect only for card data. No PAN ever reaches platform
infrastructure.

## Audit

Every mutating action (financial, administrative, compliance-relevant)
writes an immutable audit record: actor, tenant, entity, before/after
state, IP, reason code. 5–7 year retention, exportable. Manual balance
adjustments require reason code + four-eyes approval above a configurable
threshold.

## Threat model priorities for Stage 0/1

1. Cross-tenant data access (highest architectural risk given the
   multi-tenant model).
2. Session/token leakage into a third-party provider integration.
3. Idempotency-key forgery or replay on financial endpoints (owned jointly
   with `ledger-finance`).
4. Privilege escalation in back-office/partner-console RBAC.
5. Secrets exposure via logs, error messages, or client-visible responses.

## What this document does not cover

Formal penetration testing, GLI-19/ISO 27001 certification audits, and
legal/regulatory security requirements specific to a jurisdiction — those
require external, human-run engagements and are tracked as dependencies in
`docs/architecture/13-dependency-map-and-risk-register.md`, not
substituted for by this document.

## Review cadence

`security` reviews every change touching auth, sessions, tokens, RBAC,
secrets, or PII handling before it is marked `IMPLEMENTED`, per
`.claude/agents/security.md`.

## Stage 4H-B0-R5 — independent security review of five architecture P1s

`security`-owned record of findings raised against ADR 0037 (§B.6/§B.7,
Part C), ADR 0038 (§14/§14.6), `ledger-accounting-model.md` §6.3, and ADR
0034 §14. Architecture-only review; no code exists for any of these
subsystems. Each finding below is routed to the specialist who owns the
file — `security` does not edit another specialist's architecture
document (CLAUDE.md, "no specialist redesigns shared architecture
unilaterally").

**S-1 (P1, ADR 0037 Part B/C, owner `architect`).** No administrative
operation in ADR 0037 §C.5.1 covers the FX control plane: the per-pair
`FXRateProvider` priority list (§B.3), `max_age` (§B.6 item 2), the
magnitude-deviation bound (§B.7.2), and the cross-provider spread bound
(§B.7.3). Those values are the entire quantitative defense of the
conversion path, and their RBAC tier, dual-control requirement, and audit
obligation are undefined. "Absent configuration fails closed" does not
protect against a *present but deliberately widened* bound.

**S-2 (P1, ADR 0037 §B.7.2, owner `architect`/`ledger-finance`).** The
magnitude-plausibility baseline is the *same provider's own*
`GetHistoricalRate`. A compromised provider controls both sides of that
comparison. The platform's own persisted `applied` Conversion records
(§B.4) are an independent, platform-controlled baseline and should be the
primary anchor, with the provider's history at most corroborating.

**S-3 (P1, ADR 0037 Part C, owner `architect`).** `assets` has no RLS and
no `tenant_id` (migration `0003`, by design — it is platform-wide), so the
two-tier split's protection for layers 1-3 is an application permission
check only, with no database backstop. Layers 4-6 are mechanically
enforced (`tenant_jurisdiction_configs` RLS); layers 1-3 are not.
A platform-scoped write path needs its own DB-level guard.

**S-4 (P1, ADR 0037 §C.5.5, owner `architect`).** `assets.active` is
`BOOLEAN NOT NULL DEFAULT true` in migration `0003`. §C.5.5's "creation
forces `active = false`" is an API-level rule contradicted by the live
schema default; any insert path that omits the column fails open. The
implementing migration must flip the default and add
`platform_authorized NOT NULL DEFAULT false`.

**S-5 (P1, ADR 0038 §14.1, owner `ledger-finance`/`sportsbook`).** The
DB uniqueness key becomes an *adapter-composed* string; in the fallback
branch its distinguishing component derives from the adapter's own
delivery observation rather than from a provider-signed field. This
breaks the property the casino callback precedent relies on (the
uniqueness key is a value the provider signed, so verbatim replay
collides). Replay of a validly signed body as a new delivery yields a new
ordinal, a new composed key, no constraint violation, and a second
posting.

**S-6 (P1, ADR 0038 §14.1/§14.6, owner `ledger-finance`).** The
composition `{provider reference}#{ordinal}` concatenates an opaque,
externally-supplied `TEXT` field into a uniqueness key with no delimiter
reservation, escaping rule, or charset validation. Two distinct events
can be made to compose to the same key, silently absorbing the second as
a duplicate (a missed post).

**S-7 (P1, `ledger-accounting-model.md` §6.3.3.1, owner
`ledger-finance`).** The remaining-per-origin recovery query is a
balance-sufficiency read feeding a debit decision, so invariant #15
applies: it must execute in the same database transaction as the posting
it authorizes, under the same `(tenant_id, provider_id, provider_tx_id)`
advisory lock Stage 4G-FINAL Part F added for concurrent deliveries.
Neither is stated, and an empty/short result has no specified
fail-closed handling.

**S-8 (P1, ADR 0034 §14.3/§14.8, owner `identity-compliance`).** The
policy version is described both as "the version in effect when
self-exclusion becomes effective" and as "resolved fresh... never
cached." The applying listener runs after the commit, so `now()`-based
resolution lets a configuration change inside that window govern. The
as-of anchor must be the self-exclusion's own effective timestamp.

**S-9 (P1, ADR 0034 §14.5, owner `identity-compliance`).** One audit
record per affected bet cannot evidence *completeness* of the
enumeration. A dropped `rg.status.changed` event leaves no record that
any bet was skipped, which is precisely what §14.5 claims the record
proves. A per-self-exclusion-event enumeration-completion record plus a
reconciliation sweep is required.

**Authorization / tenant-isolation tests every affected domain's `qa`
coverage must include** (`security`-specified, per
`.claude/agents/security.md`):

- A request for tenant A's asset/eligibility/conversion data using
  tenant B's valid token returns 403/404, never data.
- A tenant-scoped staff token attempting every ADR 0037 §C.5.1 layer-1-3
  operation is denied, including when the tenant-scoped permission for
  layers 4-7 is held.
- A tenant-scoped override that would *widen* past a platform-layer
  denial is rejected at write time and cannot take effect at resolution
  time.
- A four-eyes-required asset operation with one approver, and with the
  same approver twice, both fail — mirroring
  `withdrawal_approvals`' `UNIQUE (request_id, approver_principal_id)`
  and its self-approval trigger.
- `CheckEligibility` with a zero-value jurisdiction denies (a zero brand
  is permitted; a zero tenant or jurisdiction is not).
- A verbatim replay of a signed sportsbook settlement callback delivered
  twice as two separate transport deliveries posts exactly once.
- A crafted provider reference containing the composition delimiter
  cannot collide with another occurrence's key.
- A tenant/brand-scoped `OpenBetSelfExclusionPolicy` row that loosens a
  jurisdiction floor never governs any bet's disposition.
