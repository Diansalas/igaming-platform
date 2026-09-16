# ADR 0033 — Provider Interoperability and External Bonus Engines

Status: Accepted for the interoperability principles and the sportsbook
canonical-event contract (§2, §3). The concrete Go-level shape in §1 is
stated as an explicit set of **assumptions about the Master Orchestrator's
own `ExternalRewardProvider`/`ExternalBonusProvider` contract document**,
stress-tested from sportsbook's specific angle — not a competing design.
Where this ADR's analysis implies that contract needs to change, it is
flagged, not silently substituted. Issued for Stage 4H-A (Bonus/
Gamification/Reward Orchestration architecture freeze). No code, no
migrations. Owner: `sportsbook`, with `bonus-engine` and `ledger-finance`
co-review (ledger/liability implications) and `architect`
(`ExternalRewardProvider` contract owner) sign-off before this ADR's §1 is
treated as binding rather than advisory.

## Context

Directive §19/§20/§11 ask for three distinct but related things this ADR
answers together, because a sportsbook provider is the platform's first
concrete case of an external vendor that runs **its own bonus engine**
(free-bet promotions, odds boosts, acca insurance) alongside our platform's
own Bonus Engine, on the same player, the same wallet, sometimes the same
bet:

1. How does the platform tell "our bonus" apart from "the provider's own
   bonus," end to end, without inventing a second, parallel financial
   subsystem for the external case?
2. What canonical, provider-neutral events does Sportsbook emit so that
   Bonus/Gamification (and, later, a second sportsbook provider) never see
   provider-specific shapes?
3. How do the two systems (platform Bonus Engine, provider's native
   sportsbook bonus engine) coexist for one integration without either
   double-counting or losing track of who currently owns fulfillment?

This ADR does not redesign the `ExternalRewardProvider` contract itself
(the Master Orchestrator owns that document) and does not invent a real
sportsbook provider's API — no commercial relationship exists yet
(CLAUDE.md, `docs/architecture/09-sportsbook-architecture.md`). It reuses,
rather than reinvents, three precedents already Accepted in this codebase:
ADR 0004 (every integration is a subsystem: the platform owns the adapter,
idempotency/retry, credential storage, state machine, reconciliation), ADR
0025 §1/§5/§6 (`CasinoProvider`'s `HandleCallback` canonicalization,
provider-authentication-before-routing, and the rollback tombstone), and
`internal/audit.Record` (append-only, before/after, actor-scoped audit).

## Assumed shape of `ExternalRewardProvider` (Orchestrator-owned; stated here only to reason about it)

Per the directive's own §3 listing, this ADR assumes the Orchestrator's
contract exposes, at minimum:

```
type ExternalRewardProvider interface {
    Capabilities(ctx) (RewardProviderCapability, error)
    Grant(ctx, GrantRequest) (GrantResult, error)
    Revoke(ctx, RevokeRequest) (RevokeResult, error)
    Status(ctx, StatusRequest) (StatusResult, error)
    HandleCallback(ctx, rawPayload []byte) (RewardCallbackEvent, error)
    HealthStatus(ctx) (ProviderHealth, error)
}
```

with idempotency via `(tenant_id, provider_id, provider_reference)`,
per-tenant credentials, a canonical state machine, error classification
(rejected/ambiguous/transient), and tenant/brand/provider isolation — the
same shape discipline as `CasinoProvider` and `PaymentProvider`. Everything
below is written against this assumption. **Two refinements this ADR's
sportsbook-specific analysis surfaces are flagged to the Orchestrator as
possible contract changes, not adopted unilaterally** — see the callouts
in §1.5 and §1.6.

## Decisions

### 1. Internal grant vs external grant — one request, two fulfillment paths, one canonical state model

A promotional entitlement (free bet, odds boost, deposit-match affecting
sportsbook activity, etc.) always originates as one `RewardRequest`
evaluated by the platform's Reward Orchestrator (Master-Orchestrator-owned
architecture, out of this ADR's scope). What this ADR fixes is what
happens **after** that request is approved and needs fulfilling:

- **Internal grant**: the platform's own Bonus Engine fulfills it directly
  against the ledger/bonus-wallet machinery `ledger-finance`'s Bonus
  Accounting ADR defines. No external call.
- **External grant**: fulfillment is delegated to a provider's own bonus
  engine via `ExternalRewardProvider.Grant`, because the entitlement is
  something only that provider's platform can create (e.g. a sportsbook
  provider's own free-bet token that only its bet slip recognizes).

The `RewardRequest` carries a `fulfillment_owner` field
(`platform` | `external_provider:<provider_id>`), resolved from **tenant-
owned promotion-campaign configuration**, never inferred by an adapter or
guessed from provider capability at grant time — the same "capability is
adapter-declared, ownership/routing is tenant configuration" split ADR
0022 §2.1 and ADR 0025 §4 already use for payments and casino.

#### 1.1 Canonical states vs. provider-observable states

The platform's own reward lifecycle (Orchestrator-owned canonical states,
assumed here as roughly `requested → approved → granted → active →
(progressing) → completed | expired | revoked | rejected`) is **richer**
than most external bonus engines will ever report. A sportsbook provider's
native bonus engine may only ever expose two facts: "granted" and
"rejected" — never a wagering-progress trail, never an explicit "expired"
distinct from silent non-use.

**Rule**: an `ExternalRewardProvider` adapter maps whatever the real
provider reports onto the canonical enum, and any canonical state the
provider's own model cannot express is recorded as `unknown_opaque`, never
fabricated. Concretely: if a provider only reports `granted`/`rejected`,
every state after `granted` (progress, completion, expiry) is
`unknown_opaque` in our record until either (a) the provider's callback
explicitly reports something else, or (b) our own settlement events
(§2) let us **infer** completion indirectly (e.g. we observe the
free-bet's own stake get consumed in a `sportsbook_bet` event referencing
the reward's token) — an inference, explicitly logged as such, never
silently promoted to "the provider told us this."

#### 1.2 Observe external reward status

`Status(ctx, StatusRequest)` is the pull path (polling); `HandleCallback`
(§1.3) is the push path. Both resolve to the same canonical
`RewardStatusEvent` shape, mirroring ADR 0025 §1's rule that
`Bet`/`Win`/`Rollback` (synchronous call shape) and `HandleCallback`
(inbound webhook shape) both collapse to one canonical `CallbackEvent` so
the orchestrator never branches on transport. A capability adapters must
declare (`RewardProviderCapability.callback_capabilities`, mirroring ADR
0022 §2's `webhook | polling_only | both`) determines whether the platform
schedules polling, expects webhooks, or both — never assumed per-provider.

#### 1.3 Reconciliation — reuse Casino's philosophy, do not invent a new one

Reconciliation means exactly what it means for Casino/Payments today:
compare the external provider's own record of what it granted/settled
against **our own ledger and audit trail**, not against some independent
notion of truth. Concretely, a daily job compares:

- Every `external_reward_grant` row (our own record, written the moment
  we call `Grant` or receive a callback claiming a grant) against the
  provider's own grant/settlement report (file, API pull, or accumulated
  callback history) for the same period.
- Every ledger posting that recognized bonus liability or player-facing
  value for an external grant (a `ledger-finance`-owned account —
  coordinated, not decided, here) against the same external record.

A mismatch (provider claims granted, we have no record; we have a record,
provider denies it) is a reconciliation exception, logged and routed for
manual review — the identical shape ADR 0004 §5 already mandates for every
integration ("daily reconciliation against the ledger"), not a new
reconciliation philosophy for bonuses.

#### 1.4 Handle external callbacks

`HandleCallback` is `ReceiveCallback`-shaped: an inbound HTTP request is
parsed and signature-verified **inside the named adapter**, producing one
canonical `RewardCallbackEvent`, before any grant/ledger effect is
considered — identical to ADR 0025 §5's rule ("a provider callback is
never processed by an unauthenticated handler," tenant resolved from the
URL's authenticated tenant slug, never the payload). Idempotency is keyed
`(tenant_id, provider_id, provider_reference)`, and a redelivered,
identical callback is a true no-op, never a second grant/ledger effect —
same discipline as every other provider integration in this codebase.

#### 1.5 Retry — no new semantics

Grant/Revoke/Status calls we initiate follow the same retry policy every
outbound provider call in this codebase follows (ADR 0004: platform-owned
idempotency/retry/timeout semantics, not vendor-specific invention). One
sportsbook-specific note flagged to the Orchestrator: because `Grant` is
**platform-initiated** (unlike a casino bet, which is provider-initiated
against our callback endpoint), the adapter must be able to attach a
platform-generated idempotency key to the outbound `Grant` call itself
(mirroring how `internal/payments`' `InitiateWithdrawal`/`InitiateDeposit`
carry a platform-generated idempotency key on outbound calls, not just
inbound callback dedup). **Flag**: if the Orchestrator's
`ExternalRewardProvider.Grant(ctx, GrantRequest)` does not already carry
an explicit idempotency-key field the adapter must echo back to the
provider on retry, this is a gap for a platform-initiated operation
specifically (§1.6 depends on it) — confirm before Stage 4H-B.

#### 1.6 Provider rejection vs. ambiguous outcome — reuse Casino's tombstone concept, with one required refinement

A definite rejection (`GrantResult.Outcome = rejected`, or an equivalent
explicit provider "no") is terminal: no ledger effect, no player-visible
entitlement, audited and done.

An **ambiguous** outcome (timeout, connection failure after send, provider
health-check failure mid-call) is the exact class of problem
`internal/casino`'s rollback tombstone (`ledger.TxTombstone`,
`postRollbackTombstone` in `internal/casino/orchestrator.go`) already
solves for bet callbacks: we genuinely do not know if the provider created
state on its side. This ADR proposes the analogous concept — a
**reward-ambiguity tombstone** — with one deliberate difference from
Casino's version, flagged as a refinement rather than a straight reuse:

- Casino's rollback tombstone safely discards a late-arriving original
  bet, because the *only* thing at stake is a ledger posting the platform
  fully controls — nothing external happened without us.
- A reward grant is different: if the provider's own bonus engine actually
  *did* create a player-facing entitlement (e.g. a free bet the player can
  already see and use on the provider's own bet slip) before our platform
  gave up waiting and marked the outcome `unknown_terminal`, silently
  discarding a late-arriving "actually succeeded" report would leave a
  real, player-usable entitlement with **no platform record and no
  ledger/liability recognition** — a worse failure mode than a duplicate.

**Decision**: after a bounded ambiguity window with no resolution, the
platform writes a `reward_outcome_tombstone` (keyed
`(tenant_id, provider_id, provider_reference)`) and marks the reward
`unknown_terminal` in our own canonical state — never fabricated as
`rejected` or `granted`. If a callback or later `Status` poll then reports
the grant *did* succeed, it is **not** auto-discarded the way Casino
discards a late original: it is routed to a manual reconciliation/audit
queue (the same exception path as §1.3) so a human resolves whether the
platform must retroactively recognize a liability. A late report of
*rejection* after `unknown_terminal`, by contrast, is a safe no-op (nothing
to recognize). **Flag to the Orchestrator**: this asymmetry (safe-discard
for casino rollbacks vs. safe-escalate for reward-grant ambiguity) means
the canonical `ExternalRewardProvider` error-classification model should
distinguish "ambiguous, and the provider-side effect if real is
financially/player-facing" from "ambiguous, and safely resolvable by
discard" — confirm this distinction is representable in the Orchestrator's
error taxonomy before Stage 4H-B.

#### 1.7 Prevent duplicate issuance

`(tenant_id, provider_id, provider_reference)` is the database-enforced
uniqueness on `external_reward_grants`, identical shape to every other
provider integration's idempotency constraint (CLAUDE.md's ledger rules,
ADR 0004). Because `Grant` is platform-initiated (§1.5), a second,
independent idempotency key — generated by the platform for the *request*
itself, before any `provider_reference` exists — is also required, so a
retried `Grant` call that never got a response cannot produce two grants
even before the provider has assigned its own reference.

#### 1.8 Audit the complete lifecycle

Every state transition (request, approval, `Grant` call issued, response
received, callback received, status poll result, tombstone written,
reconciliation exception) writes an `audit.Record`-shaped entry: actor
(system, with the initiating specialist/service identity), tenant, entity
= the `external_reward_grant` id, before/after canonical state, and the
provider's own raw reference (never provider secrets/credentials) —
mirroring `internal/audit.Record` exactly, not a bonus-specific audit
mechanism.

### 2. Provider-neutral Sportsbook → Activity/Event → Bonus/Gamification contract

Sportsbook never calls into Bonus/Gamification directly, and Bonus/
Gamification never branches on which sportsbook provider is live. Both
sides depend only on canonical events Sportsbook emits. Three events are
sportsbook's own contribution to the Master-Orchestrator-owned canonical
Activity/Event taxonomy — flagged here for inclusion there, expected shape
only (field names illustrative, not final until the taxonomy doc fixes
the shared envelope):

- **`sportsbook_bet`** — emitted at placement, once stake is moved to
  `player_locked` (`09-sportsbook-architecture.md`). Carries: tenant/
  brand/player/wallet ids, `provider_id`, `provider_bet_reference`,
  `placed_at`, stake (amount + asset code, integer minor units per
  CLAUDE.md), a provider-opaque bet-structure descriptor (`single` |
  `multiple` | `system` | `bet_builder` — a closed, small, genuinely
  provider-neutral set, never a market/sport-specific taxonomy), leg count
  (for partial-settlement correlation, §20's bet-builder case), and
  `potential_return` if the provider exposes it (nullable — never
  fabricated when a provider doesn't report it, same "unknown, not
  invented" rule as §1.1).
- **`sportsbook_settlement`** — emitted per settlement event, which may
  fire more than once per bet (bet-builder partial legs, and market
  correction re-settlement, per `09-sportsbook-architecture.md`'s explicit
  "never a silent balance edit" rule). Carries: `provider_bet_reference`,
  a `leg_reference` (nullable, for partial settlement), `settlement_type`
  (`win` | `loss` | `partial_win` | `push`), `amount_returned`,
  `settled_at`, and `is_correction` (boolean — true when this settlement
  supersedes a prior one for the same reference, so consumers can tell an
  initial settlement from a re-settlement without inferring it from
  ordering).
- **`sportsbook_void_cancel`** — emitted when a bet (or a leg) is voided,
  releasing the locked stake. Carries: `provider_bet_reference`,
  `leg_reference` (nullable), an opaque `void_reason_code` (never a
  provider-specific string leaked into consumer logic), `voided_at`, and
  `stake_released`.

Cashout (`09-sportsbook-architecture.md`'s fourth distinct ledger event) is
deliberately **not** listed as a fourth canonical event here: it is a
settlement at a provider-offered price before the bet's natural
conclusion, and is represented as a `sportsbook_settlement` with
`settlement_type = cashout` rather than a fifth top-level event type,
keeping the settlement family closed at one event shape with a
discriminator — flagged to the Master Orchestrator's taxonomy doc as the
recommended shape, open to being overridden if the canonical taxonomy
prefers a distinct type.

**Hard process rule for future stages** (recorded here as directive,
not merely a suggestion): when Sportsbook Provider #1's documentation
arrives, its capabilities are analyzed and mapped **onto this canonical
contract as it exists at that time**; the contract is not redesigned
around Provider #1's specific field names or state model. When Provider
#2's documentation arrives later, it is mapped onto the **same** canonical
contract. If Provider #2 cannot express something the contract assumed
from Provider #1, that gap is resolved by generalizing the contract (with
`architect`/Reward-Orchestrator sign-off) or by marking the field
`unknown_opaque` for Provider #2 — never by adding a Provider #1-specific
branch anywhere in Bonus/Gamification or in the canonical event shape
itself. This mirrors ADR 0025's closing consequence that "adding a second
provider never touches the orchestration/routing code."

### 3. Coexistence — tracking which system owns fulfillment, and reconstructable total exposure

For one sportsbook integration, two independent bonus sources exist on the
same player at the same time: our platform's own Bonus Engine (e.g. a
deposit-match that happens to apply to sportsbook activity, fulfilled and
ledgered entirely on our side) and the provider's own native sportsbook
bonus engine (e.g. an odds boost or free-bet promotion the provider runs
and fulfills itself, reaching our platform only as the `fulfillment_owner
= external_provider:<id>` case in §1).

**Decision**: every active promotional context for a player carries an
explicit, queryable `fulfillment_owner` (§1) for as long as it is active —
this is not a one-time tag but the durable answer to "which system is
currently responsible for fulfilling this," so that, e.g., a player
support query or a compliance request never has to guess whether an
odds-boost credit came from our ledger or from the provider's own system.

**Decision**: reconstructable total exposure is an audit/ledger
requirement, not a reporting-mechanism decision (out of scope here). This
ADR establishes only that every record — our own bonus-engine grants
(ledgered on our side per `ledger-finance`'s Bonus Accounting ADR) and
every `external_reward_grant` row (§1, with its own reconciliation trail,
§1.3) — carries enough to reconstruct, after the fact, total promotional
exposure per player/tenant/period across both systems: `fulfillment_owner`,
`provider_id` (nullable for platform-owned), amount/asset, and the
originating promotion/campaign identity common to both paths. Building the
actual cross-system reporting view is future work for `data-analytics`/
`bonus-engine`, consistent with `09-sportsbook-architecture.md`'s existing
"open-liability line, owned jointly with data-analytics" precedent for
sportsbook GGR reporting generally — this ADR does not design that report,
only guarantees the underlying records make it possible.

## Consequences

- No new financial-truth machinery: external reward grants post through
  the same `ledger.Post` API and account model `ledger-finance`'s Bonus
  Accounting ADR defines, exactly as ADR 0025 established for casino —
  this ADR adds new *rows and a new adapter category*, never a second
  ledger.
- Bonus/Gamification code never branches on sportsbook provider identity;
  Sportsbook code never branches on which system (platform vs. provider)
  currently owns a given promotion's fulfillment beyond reading
  `fulfillment_owner`.
- The `reward_outcome_tombstone` concept (§1.6) is the one place this ADR
  proposes something Casino's precedent does not already cover verbatim —
  flagged explicitly for `architect`/Reward-Orchestrator confirmation, not
  adopted as settled fact.
- Genuinely open, deferred to the Orchestrator's contract finalization or
  a later stage, not silently assumed resolved: (a) whether
  `ExternalRewardProvider.Grant` carries an outbound idempotency-key field
  (§1.5); (b) whether the canonical error taxonomy distinguishes
  safe-discard ambiguity from escalate-on-late-success ambiguity (§1.6);
  (c) the exact shared event envelope (correlation id, versioning) for
  `sportsbook_bet`/`sportsbook_settlement`/`sportsbook_void_cancel`, owned
  by the Master Orchestrator's canonical taxonomy doc; (d) the specific
  ledger account names for external-grant liability, owned by
  `ledger-finance`'s Bonus Accounting ADR.

## Owner

`sportsbook`, with `bonus-engine` (fulfillment-ownership model, §3) and
`ledger-finance` (external-grant ledger postings and reconciliation, §1.3)
co-review, and `architect`/Reward-Orchestrator sign-off required before
§1's assumed `ExternalRewardProvider` shape and the §1.6
`reward_outcome_tombstone` refinement are treated as binding on that
contract's actual document.
