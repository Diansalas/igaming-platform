# 23 — External Reward Provider Contract

Status: Stage 4H-A architecture-freeze proposal. Owned by the Master
Orchestrator (new cross-cutting abstraction, no existing specialist owns
it). **Design only — no provider adapter is implemented, no real
provider's API is invented.** The `sportsbook` specialist's
`docs/decisions/0033-provider-interoperability-and-external-bonus-engines.md`
(this same stage) validates this contract against the concrete case of a
known future sportsbook provider with its own bonus engine; read that ADR
alongside this document.

## Why this exists

The Stage 4H-A directive is explicit: **our Bonus Engine must not assume
it is the only bonus engine.** At least one future sportsbook provider is
known to run its own sportsbook-only bonus engine. The platform must be
able to (a) issue rewards itself, and (b) request/observe/reconcile
rewards a provider issues on our behalf — through one abstraction, so the
Reward Orchestrator (`docs/architecture/21-reward-orchestration-architecture.md`)
never branches on "is this internal or external" outside one place.

This mirrors, and reuses the rigor of, this codebase's EXISTING provider
abstraction pattern (ADR 0004, `docs/decisions/0025-casino-provider-abstraction-and-game-session-model.md`):
"the vendor supplies the API; the platform still owns the adapter,
idempotency/retry semantics, per-tenant credentials, the state machine,
and daily reconciliation." Nothing here invents a new philosophy — it
applies the existing one to reward fulfillment.

## The abstraction: `ExternalRewardProvider`

Conceptual interface (Go-shaped for precision, not literally the
eventual implementation signature):

```
ExternalRewardProvider interface {
    Capabilities(ctx) (ProviderCapabilities, error)
    RequestReward(ctx, ExternalRewardRequest) (ExternalRewardHandle, error)
    Status(ctx, ExternalRewardHandle) (ExternalRewardStatus, error)
    Revoke(ctx, ExternalRewardHandle) error
    Health(ctx) (ProviderHealth, error)
}
```

Plus an inbound callback contract the platform exposes (mirroring
Casino's `ReceiveCallback` pattern exactly):

```
ReceiveExternalRewardCallback(ctx, tenantID, providerID, payload) (result, error)
```

### Capability discovery

`ProviderCapabilities` answers, per tenant/provider pairing: which reward
*types* this provider can fulfill (free bet, odds boost, cashback, etc.
— provider-defined, mapped onto our canonical reward-type vocabulary,
never the provider's own vocabulary leaking into Bonus Engine or
Gamification), whether it supports revoke/cancel, whether it reports
settlement or only grant/reject, and whether it pushes callbacks or
requires polling. **Do not assume every provider supports every
capability** — `ExternalRewardRequest`/`Status` calls for an
unsupported capability fail closed with a typed
`ErrCapabilityUnsupported`, never silently no-op.

**Fulfilment-destination declaration (Wave-2 ledger-finance review
correction, P1-6).** Every reward type a provider declares MUST
additionally declare its **fulfilment destination**:
`into_platform_wallet` (the provider funds it but value lands in one of
our wallets — `docs/decisions/0032-bonus-accounting.md` §6(a)/(b),
ordinary ledger postings apply) or `inside_provider` (the provider
grants, tracks and settles it in its own system; value never enters a
platform wallet — ADR 0032 §6(c), **the ledger posts zero entries,
ever**). A reward type that does not declare a destination is **rejected
at configuration time**, per ADR 0032 §6(c) — never defaulted, and never
resolved at posting time. This field is the single discriminator every
downstream posting decision in this contract reads; it is not advisory
metadata. An earlier draft of this document left the destination
undeclared here even though `10-bonus-engine-architecture.md` §3.2
adopted the `into_platform_wallet`/`inside_provider` split on the Bonus
Engine side — this section is the actual configuration point where a
reward type's destination is declared, and it is now the binding source
those other documents defer to.

### Grant (`RequestReward`)

Takes a canonical `ExternalRewardRequest` (tenant, brand, player
reference, reward type, amount/parameters, our own idempotency key) and
returns an `ExternalRewardHandle` — an opaque, platform-owned reference
(NOT assumed to be the provider's own transaction id, since we may not
have one yet at request time for an async-granting provider). The
platform persists this handle immediately, before the provider call
completes, exactly like `casino_launch_sessions` is created before a
provider game session is guaranteed live — so a request that times up
after being sent is still tracked, never orphaned.

### Revoke/cancel

Best-effort — the contract must NOT assume a provider can always honor a
revoke (the reward may have already been irreversibly granted
provider-side). `Revoke` returns a typed outcome: `Revoked`,
`AlreadyFulfilled` (too late, provider already gave it to the player),
or `Unsupported` (per capability discovery) — never a bare error that
conflates these.

### Status / settlement

`Status` returns one of a small canonical state set the platform defines
(NOT the provider's own state names): `Pending`, `Granted`,
`Rejected`, `Settled`, `Reversed`, `Unknown`. **`Unknown` is a first-class
state, not an error** — the directive's own instruction: "distinguish
provider rejection from ambiguous outcome." A provider that times out or
returns an unparseable response yields `Unknown`, which is safe (no
financial effect assumed) and reconciled later, exactly like
`internal/casino`'s tombstone mechanism handles "rollback of a
never-seen original" — the analogous case here is "we don't yet know if
this externally-requested reward was actually granted."

### Callbacks/events

Inbound provider notifications follow the EXACT shape of Casino's
existing provider-callback pattern: idempotent on
`(tenant_id, provider_id, provider_reference)`, processed inside one
database transaction, acknowledged only after that transaction commits.
A duplicate callback for an already-processed `provider_reference` is an
idempotent no-op (mirrors `internal/casino`'s bet-delivery idempotency
check, including the Stage 4G-FINAL lesson that TRULY CONCURRENT
duplicate deliveries need a serializing lock, not just a unique
constraint, to avoid divergent *reported* outcomes — the same
`pg_advisory_xact_lock`-shaped pattern applies here without redesign).

**Specialist-review correction (P1 fix, F7)**: an earlier draft of this
section claimed to follow Casino's callback pattern "EXACTLY" while
actually omitting several requirements the real precedent
(`internal/httpserver/casino_handlers.go`'s hardened callback bar, ADR
0025 §5/§7) enforces. This section now states all of them explicitly,
not by reference alone:

1. **Tenant is resolved from the URL's authenticated tenant slug, NEVER
   from any field in the inbound payload.** The `ReceiveExternalReward
   Callback(ctx, tenantID, providerID, payload)` signature's `tenantID`
   parameter is populated by the HTTP layer from the URL before the
   handler body ever runs — a payload-supplied tenant identifier is never
   read, exactly like every other provider callback in this codebase.
2. **Signature verification happens inside the named adapter, before any
   payload field is used for anything** — including before `provider_id`
   or `provider_reference` are extracted for the idempotency check. An
   unsigned or mis-signed callback is rejected before it can influence
   any decision, including the decision to look up a handle.
3. **A suspended tenant's slug stops accepting callbacks** — resolved
   identically to a nonexistent slug (see rule 5), never a distinguishing
   response that would let an unauthenticated caller enumerate tenant
   status.
4. **A body size limit applies**, matching this codebase's existing
   provider-callback ingress limits — an external reward callback is not
   exempt from the same DoS-surface hardening every other inbound
   provider endpoint already has.
5. **Errors are generic and enumeration-resistant** — an invalid tenant
   slug, an invalid provider reference, and an unauthenticated request
   all return the same generic 4xx shape; the response never echoes back
   a submitted value to an unauthenticated caller (the same rule this
   codebase's own `casino_handlers.go` documents at its own callback
   endpoint).
6. **`provider_id` is resolved from the credential that verified the
   signature, never from the payload** — identical to
   `docs/architecture/24-points-accounting-architecture.md` §8's bar for
   externally-originated points movements; a payload cannot claim to be
   from a provider it did not authenticate as.
7. **The hard integrity rule this earlier draft omitted entirely — an
   inbound reward callback that does not resolve to an
   `ExternalRewardHandle` this platform itself created, for this tenant,
   this provider, and this player, is REJECTED as an integrity alert. It
   is never used to create a grant record, and never used to recognize a
   liability, no matter what the callback claims.** This is the direct
   analogue of Casino's `ErrLaunchSessionRequired`/`ErrBetNotFound`
   treatment (a win/rollback with no matching prior bet is an integrity
   alert, not a fact to record) and closes a real gap: without this rule,
   a leaked or compromised per-tenant callback credential could post
   grant callbacks for arbitrary player accounts, and
   `docs/decisions/0033-provider-interoperability-and-external-bonus-
   engines.md` §1.3/§1.6's reconciliation/escalation path would then route
   the forged grant to a human being ASKED to book a liability that was
   never real — the documented "happy path" for a forgery must not be "a
   human is asked to recognize it." Concretely: the callback's
   `provider_reference` (or handle reference) must match an existing
   `ExternalRewardHandle` row scoped to the SAME `(tenant_id, provider_id)`
   the signature verified against, and the callback's player reference
   must match that handle's own player reference — a mismatch on either
   dimension is rejected outright, never partially applied. An
   externally-*initiated* grant with no corresponding platform-created
   handle (a provider proactively granting something we never requested)
   is explicitly OUT OF SCOPE for this callback path — if a future
   provider genuinely needs that capability, it requires its own ADR, not
   a side effect of relaxing this rule.

### Idempotency

Every outbound `RequestReward` call carries a platform-generated
idempotency key derived the same way as every other provider integration
in this codebase: generated ONCE, when the underlying business fact is
first accepted (e.g. `tenant_id + campaign_id + player_account_id +
the logical grant's own persistent identity`), and reused VERBATIM on
every retry of that same logical request — **never regenerated per
delivery attempt**. A retried `RequestReward` for the same key MUST be
safe to send twice without the provider (or our own bookkeeping)
double-granting. **Specialist-review correction**: an earlier draft of
this section's example used a literal `grant_attempt` component, which
reads as a per-attempt retry counter — exactly the anti-pattern
`docs/architecture/24-points-accounting-architecture.md`'s own
idempotency design explicitly warns is "the single most likely practical
failure of this design," since a fresh key per attempt defeats
idempotency entirely rather than providing it. This is a REQUIREMENT the
eventual real adapter must satisfy, not a mechanism this stage builds.

### Provider reference

`ExternalRewardHandle` always carries both our own idempotency key AND,
once known, the provider's own reference — both are persisted, and
reconciliation (see below) matches on whichever the provider's own
reporting surface actually returns.

### Error classification

Every provider error is classified into: `Rejected` (provider says no,
definite), `Transient` (safe to retry, e.g. timeout/5xx), or
`ConfigurationError` (our own request was malformed/unauthorized — not
retryable without a fix). This mirrors the existing `internal/payments`
provider-error classification discipline (see
`internal/payments/orchestrator.go`'s provider error handling) — reused,
not reinvented.

### Retry semantics

Retries follow this codebase's existing "integration is a subsystem"
principle (ADR 0004): the platform owns retry/backoff policy per
provider capability, a `Transient` classification is retryable, a
`Rejected` or `ConfigurationError` classification is not. No retry
policy is implemented this stage.

### Health

`Health` reports whether the provider integration is currently able to
accept new reward requests — used by the Reward Orchestrator to decide
whether to attempt an external request at all or fail fast to a
configured fallback (e.g. deny, or fall back to an internal-only reward
if the campaign allows it — campaign-level fallback policy is Bonus
Engine's decision, not this contract's).

### Tenant/brand/provider isolation

Every call is scoped by `(tenant_id, brand_id, provider_id)`,
server-derived, RLS-protected — a tenant never sees or can trigger
another tenant's external reward activity, and per-tenant provider
credentials are never shared across tenants even for the same provider.
**Specialist-review correction (P1 fix, F11)**: an earlier draft of this
section cited `provider_capabilities`/`provider_capability_amount_limits`
as the RLS precedent for CREDENTIAL storage specifically — those tables
(verified against `migrations/0024_create_provider_capabilities.up.sql`)
carry capability/routing configuration only and have **no credential
columns**; they remain the correct precedent for the *routing/capability*
row's RLS shape, but not for where a secret lives. Per-tenant external
reward provider credentials (API keys, signing secrets, webhook shared
secrets) follow `docs/security/security-architecture.md`'s governing
secrets rule instead: stored in Vault or a cloud KMS, never in a
plain RLS-protected Postgres column (which would be backed up, possibly
replicated to analytics, and readable by any code path holding a
tenant-scoped connection), rotated per that document's rotation policy,
and never logged, never returned in an API response, never committed to
git. This mirrors how this codebase's existing provider integrations
already separate "capability/config, in Postgres" from "credential
material, in the secrets store" — no new pattern is invented here, an
earlier draft simply cited the wrong half of the existing one.

### Audit

**Specialist-review addition (P2 fix, F10)**: an earlier draft of this
document had no audit section of its own — the requirement existed only
in `docs/decisions/0033-provider-interoperability-and-external-bonus-
engines.md` §1.8, and `docs/architecture/21-reward-orchestration-
architecture.md`'s own Audit section pointed at "capability #9" of a
numbered list this document never actually contained. This contract now
states its own audit requirement directly (absorbing ADR 0033 §1.8 in
substance): every `RequestReward`, `Status`, `Revoke`, `Health` call and
every inbound callback writes an `audit.Record` entry (actor=`ActorService`
with a registered service identity, tenant, entity=the
`ExternalRewardHandle`'s id, before/after canonical state in `Metadata`,
the provider's raw reference) in the same transaction as the effect
itself — and this record **never contains provider secrets or
credentials**, only the reference material needed to reconstruct the
decision later for a disputing player or a regulator.

## OUR PLATFORM BONUS vs EXTERNAL PROVIDER BONUS — lifecycle mapping

| Our platform's lifecycle stage (Bonus Engine, `docs/architecture/10-bonus-engine-architecture.md`) | External provider equivalent (via this contract) |
|---|---|
| Campaign / Offer | Provider capability discovery — a campaign MAY be configured to fulfill via an external provider for a subset of its offers |
| Grant | `RequestReward` — creates a `Pending` `ExternalRewardHandle` |
| Activation | Provider-side, opaque — we observe via `Status`/callback, we do not control the provider's own activation timing |
| Progress | `Unknown` unless the provider's capability discovery reports progress visibility — many providers will only report Grant/Reject/Settle, no progress trail; the platform does NOT fabricate a progress trail it cannot observe |
| Completion / Conversion | `Status` = `Settled`, or a callback reporting the same |
| Expiry | Provider-side unless the provider reports it; if unreported, the platform's own record ages out per a configured maximum-unknown-duration and is marked `Unknown` → reconciliation-flagged, never silently assumed either way |
| Cancellation | `Revoke` (best-effort, see above) |
| Reversal | `Status` = `Reversed`, or a callback. **Wave-2 ledger-finance review correction (P1-6)**: an earlier draft hedged this row ("if any monetary effect exists"), which is precisely the guess-at-posting-time ADR 0032 §6(c) forbids. For an `inside_provider` reward type (per the fulfilment-destination declaration in "Capability discovery," above): **no ledger effect whatsoever**, on the reversal as on the grant — the record is the domain event + `audit.Record`, reconciled under `reconciliation-model.md` §2.10. For an `into_platform_wallet` reward type: an ordinary compensating transaction exactly like any other reversal (ADR 0032 §7). Where external value had genuinely landed in `player_cash` under the Flow 9 shape, the reversal is a **provider-settlement reversal** under that flow, not bonus accounting. |

**Do not assume external providers have the same lifecycle as our
platform.** Any stage that maps a real provider's fewer/different states
onto this table's right column, when that provider's documentation
arrives, must EXTEND the mapping, never bend our own canonical left
column to match the provider.

## Reconciliation

Daily (or configurable-interval) reconciliation compares our own
`ExternalRewardHandle` records against the provider's own reporting
surface (a report pull, if the provider offers one, or accumulated
callback history otherwise) — mirroring `internal/reconciliation`'s
existing ledger-vs-projection reconciliation discipline applied to
external reward state instead of ledger balances. Any `Unknown` or
stale-`Pending` handle past a configured threshold is surfaced to a
staff review queue — never auto-resolved to a financial assumption in
either direction.

## What this stage does NOT do

- No real provider is named, no real API endpoint is invented.
- No Go interface is written under `internal/`.
- No adapter, no credential storage schema, no reconciliation job.
- No decision on which reward *types* a real future provider will
  actually support — that is determined when that provider's
  documentation arrives (directive §3/§20).
