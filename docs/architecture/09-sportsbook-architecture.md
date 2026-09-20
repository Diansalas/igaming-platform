# 09 — Sportsbook Architecture

Status: **`NOT IMPLEMENTED`** — architecture only. No code, no migrations,
no package under `internal/`, no real sportsbook provider or sports-data
vendor is named or integrated. Stage 4H-B0-R4 (architecture-freeze
supersession of the Stage 0 proposal below). Owner: `sportsbook`, with
`architect` (cross-domain consistency, ADR 0037 Asset/Currency Registry),
`ledger-finance` (financial contract, `docs/decisions/0038-sportsbook-
accounting-and-ledger-integration.md`, authored in parallel), `risk`
(exposure/liability consumption of `internal/risk`), `identity-compliance`
(RG integration, ADR 0034), and `security` review before this document is
treated as binding. Independently reviewed by `architect`, `security` and
`qa` in a second wave; this document does not review itself.

Labeling convention, inherited from `docs/architecture/26-retail-
operations-architecture.md` §0.2 and used throughout: `BLUEPRINT` = traced
to a specific Blueprint section; `ARCHITECTURAL DECISION` = a design choice
made here, binding unless superseded by an ADR; `RECOMMENDATION` = needs
another specialist's or the Orchestrator's sign-off before it binds;
`OPEN DECISION` = genuinely unresolved, named owner, never silently
resolved.

## 0. Supersession statement — what changed, and what did not

**What changed.** The original Stage 0 version of this document (recorded
verbatim in §13 below, for the historical record) proposed a single
build/buy line: start with widget/iframe, defer feed/API build, and did
not consider an in-house engine as anything more than a deferred
possibility. A **new, confirmed product requirement** (Stage 4H-B0-R4
directive) supersedes that framing: the platform must architecturally
support, as co-equal first-class citizens from day one:

- **(A)** external sportsbook-provider integration via API (widget/iframe
  or feed/API), and
- **(B)** a strong in-house sportsbook engine, capable of eventually
  operating from one or more external sports-data feeds, provider-neutral,
  not designed around a single external vendor's shape.

**What did not change.** The Stage 0 document's *operational* recommendation
— start with widget/iframe, because it is weeks not months of work and the
first commercial priority is proving the wallet/RG/ledger integration, not
building a trading desk — remains sound **sequencing guidance**. It is
demoted from "the architecture" to "one section of the architecture" (§14):
it says which capability ships first, never which capability the canonical
domain model, the ledger contract, or the event taxonomy are allowed to
assume. Nothing below is designed around the widget/iframe shape only; the
in-house engine (§3) is a real architecture, not a placeholder kept alive
for optics.

**Why this matters structurally.** A canonical domain model, a
`SportsbookProvider` abstraction, and a `DataFeedProvider` abstraction that
are provider-neutral by construction (§1, §2, §4) make the sequencing
decision in §14 genuinely reversible: shipping external first and in-house
second, or the other way around, or both simultaneously for different
tenants (§5), never requires re-deriving the canonical model. That is the
concrete architectural test this document has to pass, and the standard
every section below is held to.

---

## 1. Canonical Sportsbook Domain Model

`ARCHITECTURAL DECISION`. The canonical domain is derived from the general
shape of sports betting — never from any external provider's API fields,
and never from the in-house engine's own internal representation either.
Both §2 (external) and §3 (in-house) map onto this one model; neither
defines it.

### 1.1 Evaluated against the directive's candidate list

The directive's candidate list (Sport, Competition, Season, Event,
Participant, Market, Selection, Odds, Line, Bet, BetLeg, BetSlip,
BetStatus, Settlement, Result, Void, Cancellation, Cashout, Exposure,
Liability, Trading state) is evaluated below, not accepted wholesale.

| Candidate | Kept? | Disposition |
|---|---|---|
| Sport | Kept, unchanged | Top-level classification (football, tennis, esports title, …). No behavior attached; a pure catalogue node. |
| Competition | Kept, unchanged | A named competition within a Sport (e.g. a league or tournament). |
| Season | Kept, unchanged | A time-bounded instance of a Competition. Exists because outright/futures Markets (below) attach here, not to a single Event. |
| Event | Kept, unchanged | A single fixture/match/race within a Season. The unit most Markets attach to. |
| Participant | Kept, generalized | A competitor in an Event — a team, an individual, or a composite (e.g. a doubles pair). Deliberately not typed as "Team \| Player" in the schema; that split is Sport-specific catalogue data, never a code branch. |
| Market | Kept, split into **MarketType** (template: "1X2", "Total Goals Over/Under") and **Market** (a live instance of a template on one subject) | See §1.3 — this is the one structural addition beyond the candidate list, and it is the same dual-scope template/instance split this codebase already uses for `hierarchy_node_types`/`hierarchy_nodes` (doc 26 §1.4) and `PointType`/`PointBalance` (doc 24 §2). Treating "Market" as one flat concept would force every provider's or the in-house engine's own market catalogue directly into the canonical model — exactly the provider-shape leakage this document exists to prevent. |
| Selection | Kept, unchanged | One outcome within a Market a bettor can back (e.g. "Home", "Over 2.5"). |
| Odds | Kept, renamed conceptually to **Price** | See §1.4. "Odds" is ambiguous between the payout multiplier and the whole pricing concept; the canonical model stores a `Price` (decimal odds, canonical format) per Selection, versioned over time. |
| Line | Kept, folded into **Price** as an optional attribute | A handicap/total number (e.g. `+3.5`, `Over 2.5`) is not a separate top-level entity — it is a property of certain Selections' current Price (see §1.4). Modeling it as a sibling of Odds invited two sources of truth for "what this Selection currently means." |
| Bet | Kept, unchanged | The wagering instrument as accepted: stake, currency/asset, potential return, status, one or more legs. |
| BetLeg | Kept, unchanged | One (Selection, accepted Price, accepted Line-if-any) pair within a Bet. A single bet has one leg; a multiple/combo has several; a system bet (e.g. a Yankee) decomposes into multiple correlated **BetCombinations** over the same leg set — modeled as a derived/computed view over BetLegs, not a fourth top-level entity, since a system bet's combinations are fully determined by its leg set and system type. |
| BetSlip | Kept, demoted to **ephemeral request**, not a persisted domain aggregate | See §1.5 — a BetSlip is the pre-acceptance construct a client assembles; once accepted it produces one or more Bets and is not itself a long-lived state machine. Modeling it as a persisted aggregate with its own lifecycle would create a second place "what did the player actually wager" could be asked, competing with Bet. |
| BetStatus | Kept, unchanged | Enum on Bet: `pending_acceptance \| accepted \| rejected \| open \| settled \| void \| cancelled`. `rejected` and `cancelled` are pre-acceptance/pre-settlement; a Bet that was never accepted is not assigned a stake-locking effect (no ledger event) — mirrors Casino's own "Declined never posts a `LedgerTransaction`" rule (`financial-transaction-flows.md`, ADR 0025 §6). |
| Settlement | Kept, unchanged | A distinct, independently-idempotent financial-event record. Fires once per full settlement, or more than once per Bet for partial-leg settlement and for a post-settlement correction — this platform's own pre-existing point (Stage 0 doc, restated and never weakened here), matching ADR 0033 §2's `sportsbook_settlement` (`is_correction` flag) and `financial-transaction-flows.md` Flow 9/11 exactly. |
| Result | Kept, and deliberately separated from Settlement | A Result is a **sports-data fact** (final score, winner, statistic) ingested from a feed or a provider callback. A Settlement is the **financial/business decision** derived from a Result (and from the Market's own settlement rules — e.g. how a push is handled). Collapsing these into one concept would blur "what happened in the world" with "what we decided to pay," which is exactly the distinction §3's layering (SPORTS DATA vs. SPORTSBOOK BUSINESS LOGIC) depends on. One Result can trigger many Bets' Settlements; a correction to a Result triggers re-settlement (§1.6), never a silent edit of a prior Settlement. |
| Void | Kept, unchanged | A settlement-type variant (`settlement_type = void`, mirroring how ADR 0033 §2 folds cashout into `sportsbook_settlement` rather than inventing a parallel event family) that returns the locked stake with no house revenue recognized. |
| Cancellation | Kept, but reframed as a **Trading Operation** on Event/Market, not a Bet-level financial event | Canceling an Event or a Market is an operational action (§3.6) whose *consequence* is that every open Bet referencing the cancelled subject receives a Void settlement. Modeling Cancellation as a Bet-level event alongside Void would create two code paths for the same financial outcome (stake returned, no revenue) depending on whether the trigger was upstream (market cancelled) or Bet-specific (bet voided individually) — collapsed here into one financial shape with two distinct triggers. |
| Cashout | Kept, not a top-level financial event | Per ADR 0033 §2's already-accepted decision: represented as `sportsbook_settlement` / Flow-11-shaped ledger entries with `settlement_type = cashout`. This document does not reopen that; see §6. |
| Exposure | Kept, explicitly distinguished from Liability | See §1.7 — a **trading-book concept**, never a ledger balance. |
| Liability | Kept, explicitly distinguished from Exposure | See §1.7 — the **ledger-facing concept**: the platform's actual financial obligation, backed by `player_locked` (CLAUDE.md, Stage 0 doc, ADR 0038). |
| Trading state | Kept, as an **attribute**, not an entity | `open \| suspended \| closed \| settled` on Market (and, derived, on Event — an Event with every Market closed is itself closed for new bets). Suspending a Market is a Trading Operation (§3.6), never a property a bet-acceptance code path infers indirectly. |

Nothing on the directive's list was removed outright; two items (Line,
BetSlip) were folded/demoted with an explicit reason, and one addition
(MarketType as distinct from Market) was made because without it the
canonical model cannot express an outright/futures market (§1.3) without
smuggling in provider- or engine-specific catalogue shape.

### 1.2 Sport → Competition → Season → Event → Participant

`ARCHITECTURAL DECISION`. A strict containment chain: `Sport 1─* Competition
1─* Season 1─* Event`, with `Event *─* Participant` via an `EventParticipant`
join (a Participant plays in many Events over a Season; an Event has two or
more Participants — more than two for multi-competitor sports, e.g. racing).
`Participant` carries no Sport-specific typed subclassing in the canonical
model (no `Team`/`Player` table split) — Sport-specific display shape is
catalogue configuration, not schema, mirroring `CLAUDE.md`'s "nothing
brand-specific may become a code path" rule applied to sport-specific
shape instead of tenant-specific shape.

### 1.3 Market — template vs. instance, and why it must attach to more than one subject type

`ARCHITECTURAL DECISION`. Two concepts, not one:

- **MarketType** — a definitional template ("1X2", "Total Goals
  Over/Under 2.5", "Outright Winner"), Sport-scoped, carrying the
  selection-shape rules (how many Selections, whether a Line applies, how a
  push/void is determined) but no live odds and no subject.
- **Market** — a live instance of a MarketType attached to a **subject**.

The subject is **polymorphic by necessity, not by preference**: most
Markets attach to a single `Event` (e.g. "Match Winner"), but outright/
futures Markets ("Season Winner", "Top Goalscorer") attach to a
`Season` (or, rarely, a `Competition` spanning seasons) and have no single
Event to key off. Forcing every Market to carry an `event_id` would make
outright markets either impossible to represent correctly or represented
as a fake Event, which is exactly the kind of structural workaround this
canonical model exists to avoid. `Market.subject_type ∈ {event, season}`,
`Market.subject_id` — closed, small, extensible only by adding a new
subject type through review, never by a provider/engine inventing a third
shape silently.

### 1.4 Price — canonical decimal odds, Line as an attribute, both versioned and frozen at acceptance

`ARCHITECTURAL DECISION`.

- **Canonical format**: decimal odds (`NUMERIC`, never `FLOAT`, mirroring
  every other numeric-precision rule this codebase already enforces for
  money-adjacent fields — ADR 0021's `exchange_rate`/`spread` precedent
  applies identically here). A provider or the in-house engine may compute
  in fractional or American odds internally; the canonical `Price` a
  `Selection` carries is always decimal.
- **Line** (a handicap or total number, e.g. `+3.5`, `Over 2.5`) is an
  **optional attribute of a Price snapshot**, not a sibling top-level
  entity. A Selection's meaning for a line-based Market is fully
  determined by (MarketType, Line) together — "Home -1.5" and "Home -2.5"
  are different Selections of the same MarketType with different Lines,
  never the same Selection with a mutable Line.
- **PriceHistory**: every change to a Selection's live Price is an
  append-only, timestamped row — never an in-place update — for the same
  reason the ledger is append-only: a settlement dispute, a trading-error
  investigation, or a regulator's audit needs to reconstruct "what was the
  price at time T," not just "what is it now."
- **Acceptance freezing**: a `BetLeg` stores the **accepted Price and Line
  at acceptance time**, copied from the Selection's live Price at the
  moment the Bet was accepted — never a live reference to the Selection's
  current Price. This is the sportsbook analogue of the ledger's
  "denormalize onto the row a policy/reconciliation needs it on" pattern
  (ADR 0019's `wallet_id`/`player_account_id` denormalization,
  ADR 0035 §1.3.1's `hierarchy_node_id` denormalization): a Bet's
  potential return must be computable and auditable years later even if
  the Selection's price has since changed a thousand times or the Market
  no longer exists.

### 1.5 BetSlip is a request, not a stored state machine

`ARCHITECTURAL DECISION`. A BetSlip (a client's proposed set of one or more
Bets, assembled before submission) is a short-lived request payload — it
does not get a `bet_slip_status` state machine, does not get settled, and
is not the thing Settlement/Void/Cashout ever reference. On submission, the
platform validates it (§3.4) and, for each accepted line, creates exactly
one `Bet` (a "multi-bet slip" submitting three independent single bets in
one request produces three independent Bets, each with its own lifecycle).
This avoids a second status enum that must always be kept consistent with
`BetStatus` and answers a question ("is the *slip* settled?") that has no
single correct answer once its constituent Bets can resolve independently.

### 1.6 Settlement, correction, and re-settlement — restated, not weakened

`BLUEPRINT`-equivalent (restated from the Stage 0 version of this document,
already correct, and from `financial-transaction-flows.md` Flows 9–11,
already `BLUEPRINT`/`ARCHITECTURAL DECISION` status): settlement can be
**full, partial (per-leg, e.g. a bet-builder or system bet with independent
legs), or void**, is always a **distinct, independently-idempotent
financial event**, never conflated into one "resolve bet" call, and a
Market can be **corrected after initial settlement** (wrong Result
ingested, data error, post-match sanction changing an outcome), which
triggers **re-settlement as its own new event carrying `is_correction =
true`** (ADR 0033 §2's already-accepted shape) — never a silent edit of the
original Settlement record. This is unconditional regardless of whether the
originating engine is external or in-house (§2, §3): a re-settlement event
looks identical to a consumer either way, because both map onto the same
canonical Settlement shape.

### 1.7 Exposure vs. Liability — two different concepts this list conflates by naming them side by side

`ARCHITECTURAL DECISION`, stated explicitly because getting this wrong is
the most likely way an in-house engine accidentally grows a second
financial-truth system:

- **Liability** is the platform's actual, ledger-backed financial
  obligation: the sum of `player_locked` balances across open sportsbook
  bets (per asset, per CLAUDE.md/ADR 0038). It is **authoritative**,
  computed the same way every other balance on this platform is computed —
  as a projection recomputed from ledger entries, never a counter a
  sportsbook service maintains itself.
- **Exposure** is a **trading-book concept**: the aggregate potential
  payout the book would owe, per Selection/Market/Event, if a given
  outcome occurs, across all currently-open Bets referencing it. It exists
  so Trading Operations (§3.6) can decide whether to suspend a Market,
  move a Price, or hedge — a business/risk-facing read-model, **not** a
  financial balance and **not** a second ledger. It is computed from the
  same canonical Bet/BetLeg data the rest of the domain model already
  holds (grouped and aggregated differently — by Selection/Market/Event
  rather than by player/wallet), refreshed as Bets are accepted, settled,
  or voided.

**The load-bearing rule**: Exposure is read-only trading intelligence
derived from the canonical domain model; Liability is the one authoritative
number the Ledger owns. A future implementation that lets a Trading
Operation's Exposure figure be treated as an alternative source of truth
for what the platform owes a player has built exactly the "second
financial truth system" CLAUDE.md and every prior ADR in this codebase
(ADR 0032 §0, ADR 0035 §0) already forbid, restated here for sportsbook
specifically.

---

## 2. External Sportsbook Provider Architecture

`ARCHITECTURAL DECISION`. Reuses, and does not reinvent, this codebase's
existing provider-abstraction discipline: ADR 0004 (every integration is a
subsystem — platform owns the adapter, idempotency/retry, credentials,
state machine, reconciliation), ADR 0025 (`CasinoProvider`'s method
enumeration, `HandleCallback` canonicalization, `Capabilities()`
declaration), and ADR 0022 (`PaymentProvider`'s two-layer
adapter-declared/tenant-configured capability split). No real provider is
named; no real API field is invented; when a real provider's documentation
arrives, its capabilities are **mapped onto this contract**, per the same
"the contract is not redesigned around Provider #1" rule ADR 0033 §2
already states for the canonical event shape.

### 2.1 The interface (conceptual — Go-shaped for precision, not literal)

```
type SportsbookProvider interface {
    Capabilities(ctx) (SportsbookAdapterCapability, error)

    // Catalogue / markets / odds — read paths
    Catalogue(ctx, CatalogueRequest) (CatalogueResult, error)
    Markets(ctx, MarketsRequest) (MarketsResult, error)
    Odds(ctx, OddsRequest) (OddsResult, error)

    // Bet lifecycle — write/read paths
    PlaceBet(ctx, PlaceBetRequest) (PlaceBetResult, error)
    GetBet(ctx, GetBetRequest) (BetResult, error)
    BetStatus(ctx, BetStatusRequest) (BetStatusResult, error)
    CancelBet(ctx, CancelBetRequest) (CancelResult, error)
    Cashout(ctx, CashoutRequest) (CashoutResult, error)

    // Inbound — mirrors CasinoProvider/PaymentProvider exactly
    HandleCallback(ctx, rawPayload []byte) (SportsbookCallbackEvent, error)

    HealthStatus(ctx) (ProviderHealth, error)
}
```

Same shape discipline as `CasinoProvider`/`PaymentProvider` (ADR 0025 §1):
every request/response struct is explicitly enumerated and typed, no
free-form passthrough field, no provider SDK type anywhere in the
signature. `PlaceBet`/`GetBet`/`BetStatus`/`CancelBet`/`Cashout` are what an
adapter's own `HandleCallback` implementation may call internally to talk
to that provider's specific API shape (the common real-world pattern where
the provider is called synchronously per-operation); `HandleCallback` is
the canonical entry point for asynchronous settlement/void/cashout
notifications a provider pushes — both shapes resolve to the same
canonical `SportsbookCallbackEvent`, so the orchestrator never branches on
transport, exactly mirroring ADR 0025 §1's `Bet`/`Win`/`Rollback` vs.
`HandleCallback` duality.

### 2.2 Two-layer capability model, mirroring ADR 0022 exactly

`ARCHITECTURAL DECISION`, applying ADR 0022 §2.1's "a capability row
describes an adapter; it never promotes one" rule to sportsbook:

- **Layer (a) — adapter-declared, compile-time truth**:
  `SportsbookAdapterCapability`, returned by `Capabilities(ctx)`, states
  what the adapter's code can actually do: which bet-structure shapes it
  supports (`single \| multiple \| system \| bet_builder`), whether it
  supports cashout, whether it supports partial settlement, whether it
  reports odds/line changes, its `callback_capabilities`
  (`webhook \| polling_only \| both` — mirroring ADR 0033 §1.2's identical
  field for `ExternalRewardProvider`), and its supported markets/sports at
  a coarse level.
- **Layer (b) — operator/tenant-configured, database rows**: a
  `sportsbook_provider_capability`-shaped table (naming and shape mirroring
  `provider_capabilities`/`provider_capability_amount_limits`, ADR 0022
  §2, migration `0024_create_provider_capabilities.up.sql`'s precedent),
  scoped `(tenant_id, brand_id, provider_id)`, narrowing which of the
  adapter's declared capabilities are **enabled** for that tenant/brand,
  plus routing priority and per-asset stake/exposure limits it is willing
  to accept from that provider.

A tenant/brand may never enable a capability the adapter does not declare
(fails closed with a typed `ErrCapabilityUnsupported`, mirroring ADR 0023
§Capability discovery's identical rule for `ExternalRewardProvider`); the
adapter registry — which interface a given `provider_id` is compiled
against — remains the sole authority on what is possible, exactly as ADR
0022 §2.1 states for payments.

### 2.3 Idempotency, callbacks, and health — reused, not reinvented

- **Idempotency**: `(provider_id, provider_bet_reference)` is the
  DB-enforced uniqueness for every sportsbook financial write, identical
  in shape to `(provider_id, provider_tx_id)` for casino/payments
  (CLAUDE.md, ADR 0020). Settlement/void/cashout events each carry their
  own distinct provider reference, per `financial-transaction-flows.md`
  Flows 9–11 (already established, unchanged here).
- **Callbacks**: `HandleCallback` follows ADR 0025 §5/ADR 0023's hardened
  bar verbatim — tenant resolved from the URL's authenticated tenant slug,
  never from the payload; signature verification inside the named adapter
  before any payload field is used for anything; `provider_id` resolved
  from the credential that verified the signature, never from the payload;
  generic, enumeration-resistant errors; a body size limit; and the ADR
  0023 §"the hard integrity rule" analogue — a callback that does not
  resolve to a Bet **this platform's own records show it placed with this
  provider** is rejected as an integrity alert, never used to create a
  settlement or recognize a liability.
- **Ambiguous outcomes**: a timeout/connection failure mid-call to a
  provider (e.g. `PlaceBet` sent, no response received) is `Unknown`, not
  fabricated as accepted or rejected — the same tombstone-shaped discipline
  `internal/casino`'s rollback tombstone and ADR 0033 §1.6's reward-outcome
  tombstone already establish, applied here to bet placement specifically:
  a bounded ambiguity window, then a tombstone, then routed to
  reconciliation rather than guessed.
- **Health**: `HealthStatus` reports whether the provider can currently
  accept new bets — used by mode-selection routing (§5) to fail fast to a
  configured fallback (deny sportsbook betting for that tenant/market, or
  route to an alternative provider/in-house engine if configured) rather
  than hang.

### 2.4 Adding a second external provider

`ARCHITECTURAL DECISION`, stated explicitly per the directive: adding a
second `SportsbookProvider` requires **only**: a new adapter implementing
§2.1's interface, a `SportsbookAdapterCapability` declaration, per-tenant
credentials/config, routing configuration (§5), and passing the same
conformance test suite every adapter passes (mirroring ADR 0025 §8's
casino conformance suite and ADR 0022's payment adapter conformance
precedent). It must **never** require a change to the canonical domain
model (§1), the ledger contract (§6), or the event taxonomy (§7) — those
are derived from the general shape of sports betting, not from Provider
#1's shape, per §0's supersession statement.

### 2.5 Canonical identity is platform-owned; provider references are non-authoritative (Stage 4H-B0-R5 correction, `architect`)

**Verification finding, Stage 4H-B0-R5 (directive §9):** §4.2 already
states, explicitly, that a feed adapter maps a vendor's own fixture/
participant/market/result identifiers onto this document's canonical
`Sport`/`Competition`/`Season`/`Event`/`Participant`/`Result` shapes, and
that the vendor's own identifiers are retained only as a **non-
authoritative external reference**, never as the row's primary identity.
§2.3 and §9 separately state the same property for `Bet` (idempotency
keyed on `(provider_id, provider_bet_reference)`, but the platform's own
row — "keyed by the platform's own identity" — is what every canonical
event and audit record points at). Those statements are correct and are
**not** edited here.

**Gap found and closed here**: §4.2's enumeration is scoped to the
`DataFeedProvider` pipeline (§4, used by the in-house engine, §3) and does
not name `Market` or `Selection` — because for the in-house engine, Market
and Selection are constructed by the engine's own trading logic (§3.2)
from canonical `MarketType`/`Event`/`Season` identifiers, not raw-ingested
from a feed, so they were correctly out of scope for §4.2's specific list.
But the **external** `SportsbookProvider` path (§2) has no equivalent
statement anywhere: `Catalogue`/`Markets`/`Odds` (§2.1) return a real
external provider's own Event/Competition/Participant/Market/Selection
objects directly, and nothing in §2 said explicitly that these are
re-keyed onto platform-owned canonical identifiers with the provider's own
identifiers retained as non-authoritative references only. Left
unstated, a reader could wrongly infer that in external mode a provider's
own `market_id`/`selection_id`/`event_id` might be used *as* the canonical
row's identity rather than mapped onto one.

**Correction, closing the gap**: this document states explicitly, for
every entity the directive asked to verify, that the rule already
established for the feed path (§4.2) and for `Bet` (§2.3/§9) is universal
across **all** of them, regardless of which path (external provider,
Stage 4H-B0-R5 data feed, or in-house engine) produced the row:

- `Event`, `Competition`, `Season`, `Participant` — platform-generated
  canonical identifiers (as §4.2 already states for the feed path; this
  extends the identical rule to `Catalogue`/`Markets` results ingested
  through a `SportsbookProvider`, §2.1). A provider's own fixture/
  competition/participant identifier is stored as a non-authoritative
  external reference field on the canonical row, never as the row's
  primary key.
- `Market`, `Selection` — the one addition this correction makes: these
  carry their own platform-generated identifiers exactly like every other
  entity in §1's model, whether they were constructed by the in-house
  engine from `MarketType`+subject (§1.3, §3.2) or ingested from an
  external provider's `Markets`/`Odds` calls (§2.1). In the latter case,
  the provider's own market/selection identifier is retained as a
  non-authoritative external reference only, following the exact pattern
  §4.2 already established for Event/Participant.
- `Bet`, `Settlement` — unchanged, already explicit (§2.3, §9): the Bet's
  platform-generated identity is the domain's source of truth;
  `provider_bet_reference` and any provider-issued settlement reference
  are non-authoritative references used for idempotency and
  reconciliation, never for identity. This applies identically to a
  settlement produced via `HandleCallback` (§2.1) or by the in-house
  engine (§3.5) — both post the same canonical Settlement shape (§1.6).

No other part of §1-§9 is changed by this correction; it generalizes an
already-correct principle to close one naming gap, it does not introduce a
new one.

---

## 3. In-House Sportsbook Engine Architecture

`ARCHITECTURAL DECISION`. This is a real architecture: five layers that
must never collapse into each other, an explicit boundary with the Risk
engine, and an explicit, repeated statement of the one rule that matters
most: **the sportsbook never becomes a second wallet.**

### 3.1 The five layers

```
 SPORTS DATA              →  raw feed/catalogue ingestion, canonicalized (§4)
        │
 SPORTSBOOK BUSINESS LOGIC →  catalogue, markets, odds/lines, bet
        │                     validation/acceptance, settlement,
        │                     result ingestion, cashout (this section)
        │
 FINANCIAL LEDGER          →  internal/wallet + internal/ledger — the
        │                     ONLY authoritative source of financial
        │                     truth, unchanged, no new financial
        │                     mechanism
        │
 RISK                      →  internal/risk — consumed, not reimplemented
        │
 TRADING OPERATIONS        →  odds-setting, market suspension, exposure
                              management (§3.6) — reads §1.7's Exposure
                              projection, writes Trading actions
```

These are **package/module boundaries, not merely conceptual layers**: a
future `internal/sportsbook` package's business-logic code must not import
`internal/ledger`'s posting internals to mutate a balance directly, must
not implement its own limit/threshold engine competing with
`internal/risk`, and must not read/write another layer's tables. This
mirrors doc 26 §7.1's `agentnetwork`/`retail` package-boundary reasoning
and ADR 0035 §0's "the retail subsystem is a decision and workflow system,
not an accounting system" — restated here as "the in-house sportsbook
engine is a trading and catalogue system, not an accounting system, and
not a second risk engine."

### 3.2 Event catalogue, market management, odds/line management

The in-house engine owns the SPORTSBOOK BUSINESS LOGIC layer's read side:
maintaining `Sport`/`Competition`/`Season`/`Event`/`Participant`/`Market`/
`Selection`/`Price` (§1) as its own authoritative catalogue, **populated
from canonical sports data** (§4) rather than a provider's raw feed shape.
Odds/line management (setting or adjusting a Selection's Price) is a
**Trading Operation** (§3.6), never an automatic, unreviewable function of
raw feed data alone — a feed supplies facts (scores, statistics, a
data-provider's own suggested price where offered); the decision of what
price the platform actually offers is the in-house engine's own trading
logic, which may use a feed's suggested price as one input among several
without being obligated to mirror it.

### 3.3 Bet validation and acceptance

`ARCHITECTURAL DECISION` — the acceptance pipeline, in required order,
fail-closed at every step (mirroring the RG-before-Risk-before-ledger-write
ordering already established platform-wide, ADR 0031 §1, restated for
sportsbook in §8 below):

1. **Structural validation**: the BetSlip's legs reference open Markets/
   Selections in `trading_state = open`, at a Price/Line the platform is
   still willing to honor (a price that has since moved is rejected or
   re-offered, never silently honored at a stale price — this is the
   "accepted Price frozen at acceptance" rule from §1.4 applied at the
   validation boundary, not just the storage boundary).
2. **RG eligibility** (`rg.EvaluateEligibility`) — §8.
3. **Risk evaluation** (`risk.Evaluate`) — §3.5.
4. **Funds check and lock** — inside the same database transaction as the
   ledger write that moves stake to `player_locked` (§6); insufficient
   funds is rejected pre-posting, identical to every other financial flow
   on this platform (`financial-transaction-flows.md` Flow 8).

A Bet is created (status `accepted`, ledger-locked) only if all four steps
pass; any failure produces a `rejected` outcome with **no** ledger effect,
mirroring Casino's "Declined never posts a `LedgerTransaction`" rule.

### 3.4 Trading, exposure, and the Risk boundary

`ARCHITECTURAL DECISION` — the boundary the directive specifically asks to
be kept conceptual rather than redesigned here: the in-house engine
**consumes** `internal/risk.Evaluate` for every bet-acceptance decision
(`risk_rules.product = 'sportsbook'`, `Operation = sportsbook_bet`, already
present in the risk engine's own `Operation` enum per ADR 0031 — this
document adds no new Risk mechanism, only confirms the existing
integration point). Per-Selection/Market/Event **Exposure** (§1.7) is
computed and owned by the in-house engine's own trading logic as an input
**to** Risk decisions and Trading Operations (e.g. "this Market's exposure
has crossed a configured threshold, suspend it" is a Trading Operation
informed by Exposure, evaluated and enforced through the same
`risk.Evaluate` call the rest of the platform uses) — it is explicitly
**not** a second risk-rule engine. The detailed design of any
sportsbook-specific Risk *rule* dimensions (e.g. a per-market or
per-selection exposure ceiling as a first-class `risk_rules` scope
dimension) is `risk`'s own parallel Stage 4H-B0-R4 work, per ADR 0031 §16's
established extension process — this document states the boundary and
integration point only, consistent with the task's own instruction not to
design a second Risk engine here.

### 3.5 Settlement, result ingestion, cashout, and operational controls

- **Result ingestion**: a canonical `Result` (§1.1) is recorded from a data
  feed (§4) or a manual trading-operations action (for sports/markets with
  no automated feed yet), always as an append-only, sourced fact —
  never a direct settlement side-effect. Ingesting a Result **triggers**
  settlement evaluation; it is not itself a financial event.
- **Settlement**: the in-house engine evaluates each open Bet/BetLeg
  referencing the resulted Market against that MarketType's settlement
  rule (win/loss/push/void), producing the same canonical Settlement shape
  §1.6/§6/ADR 0033 §2 already define — a consumer (Ledger, Bonus/
  Gamification, Reporting) cannot tell, from the event shape, whether the
  originating engine was external or in-house, per §0's design test.
- **Cashout**: computed and offered by the in-house engine's own trading
  logic (a live price to close out an open position before its natural
  conclusion), accepted or rejected by the player, and posted as a
  `sportsbook_settlement` with `settlement_type = cashout`, per ADR 0033
  §2 and `financial-transaction-flows.md` Flow 11 — unchanged whether the
  cashout price came from an external provider or the in-house trading
  engine.
- **Operational controls (Trading Operations)**: suspending a Market
  (stops new bet acceptance on it, does not affect already-accepted open
  Bets), voiding a Market (§1.1's Cancellation → Void cascade), correcting
  a Result and triggering re-settlement (§1.6), and adjusting a Selection's
  Price are all **explicit, audited Trading Operations** — each one writes
  an `audit.Record` (actor, tenant, entity = the Market/Event, before/after
  trading state, reason code), mirroring CLAUDE.md's "every mutating
  administrative/financial action writes an audit record" rule applied to
  trading actions specifically, since a market suspension or a Result
  correction has direct financial consequences for every open Bet
  referencing it.
- **Ledger-posting instruction for every event above** (bet acceptance,
  settlement, void, partial settlement, cashout, rollback/re-settlement):
  `provider_id`/`provider_tx_id` stay `NULL` and a stable, intrinsic,
  per-event reference is supplied as `idempotency_key` instead — see §6.1
  for the full statement of this rule and ADR 0038 §14.6 for the
  underlying database-constraint design it satisfies.

### 3.6 The rule that must never be relaxed

**The sportsbook — in-house or external — never becomes a second wallet.**
It never holds an authoritative balance, never performs its own `UPDATE`
of a balance, and never maintains a parallel "player owes/is owed"
counter that could diverge from the Ledger. Every financial consequence of
a bet, a settlement, a void, a cancellation, or a cashout is expressed as
a canonical financial instruction/event (§6) that `internal/ledger`
posts, exactly once, through the existing `ledger.Post` API and account
model — restated here, emphatically, per the task's own instruction,
because an in-house trading engine is precisely the kind of subsystem
where "just cache the running balance for performance" is the most tempting
and most forbidden shortcut on this platform.

---

## 4. Sports Data-Feed Architecture

`ARCHITECTURAL DECISION`. Provider-neutral by the same discipline as §2, and
for the same reason: the in-house engine (§3) must depend only on a
canonical sports-data shape, never on one data-feed vendor's raw format, so
that adding a second (or first) feed provider later is an adapter, not an
engine rewrite. No real data-feed vendor is named.

### 4.1 The abstraction

```
type DataFeedProvider interface {
    Capabilities(ctx) (DataFeedCapability, error)

    Fixtures(ctx, FixturesRequest) (FixturesResult, error)   // catalogue: sports/competitions/seasons/events/participants
    LiveState(ctx, LiveStateRequest) (LiveStateResult, error) // in-play state, if supported
    SuggestedOdds(ctx, OddsFeedRequest) (OddsFeedResult, error) // optional — not every feed offers pricing
    Results(ctx, ResultsRequest) (ResultsResult, error)

    HandleCallback(ctx, rawPayload []byte) (DataFeedCallbackEvent, error)
    HealthStatus(ctx) (ProviderHealth, error)
}
```

`DataFeedCapability` declares, per feed: which sports/competitions it
covers, whether it offers in-play/live state, whether it offers suggested
pricing (many data-feed vendors supply fixtures/results only, with pricing
being the in-house engine's own trading decision — §3.2), its update
cadence/latency characteristics, and its callback shape
(`webhook | polling_only | both`, same enum as §2.2/ADR 0033 §1.2) — the
same "capability is adapter-declared" discipline as every other provider
abstraction in this codebase.

### 4.2 The pipeline, and where canonicalization happens

```
External Feed → Feed Adapter → Canonical Sports Data → Sportsbook
Catalogue (§1) → Markets/Odds/Lines (§3.2) → Trading/Risk (§3.4) →
Bet Acceptance (§3.3)
```

Canonicalization happens **once, inside the named adapter** — exactly
where `HandleCallback` canonicalizes a payment or casino callback (ADR
0025 §5, ADR 0022 §5). The adapter maps a vendor's own fixture/participant/
market/result identifiers onto this document's canonical `Sport`/
`Competition`/`Season`/`Event`/`Participant`/`Result` shapes (§1); nothing
downstream of the adapter — the catalogue, market management, trading,
risk, settlement — ever sees a vendor-specific field name or identifier
format. A vendor's own identifiers are retained as a **non-authoritative
external reference** on the canonical row (mirroring ADR 0033's
`provider_bet_reference`/ADR 0022's provider references generally) for
reconciliation and support lookups, never as the row's primary identity.

### 4.3 Package boundary

`RECOMMENDATION`: a `DataFeedProvider` adapter layer lives as a sub-package
of the in-house engine (e.g. `internal/sportsbook/datafeed`), not a
sibling top-level package on the `agentnetwork`/`retail` split's model
(doc 26 §7.1). Unlike the hierarchy/retail case, no second consumer of
"sports data" exists anywhere else in this platform's scope today — a
future need (e.g. `data-analytics` wanting raw fixture data independent of
the sportsbook engine) would be a genuine reason to promote it to a
sibling package at that time, mirroring doc 26 §7.1's own explicit
"acceptable, reversible simplification" concession for a speculative
split. This is ordinary engineering judgment, not a decision requiring
sign-off, and is flagged for `architect`'s review as such.

### 4.4 Adding a second feed provider

Mirrors §2.4 exactly: a new adapter implementing §4.1's interface, a
capability declaration, credentials/config, and conformance tests. The
in-house engine's catalogue/market/trading code depends only on the
canonical shapes in §1 and §4.2's pipeline output — never on a specific
feed vendor's schema — so a second (or replacement) feed provider never
requires a change to §3's business logic.

---

## 5. External vs. In-House Mode Selection

`ARCHITECTURAL DECISION` for the architecture (a routing/config layer must
exist and must not foreclose future dimensions); `RECOMMENDATION`/deferred
for the actual routing implementation, per the directive's own instruction
not to over-engineer this now.

### 5.1 The routing layer, conceptually

A `sportsbook_mode_routing`-shaped configuration, resolved server-side at
request time from **trusted, server-derived context only** — never a
client-supplied "use provider X" parameter, mirroring CLAUDE.md's
authorization rule applied to product routing. Modeled on the same
dual-scope, most-specific-wins precedent this codebase already uses
repeatedly (`risk_rules` ADR 0031 §5, `hierarchy_node_type_relations` doc
26 §1.4/H6, `TenantJurisdictionConfig` doc 15): a routing decision resolves
by matching the most specific configured row across whichever of these
dimensions the platform chooses to support:

- **Tenant** — the coarsest, always-available dimension (a B2B partner
  brand routes entirely to their own contracted external provider; the
  house B2C brand routes to the in-house engine, or vice versa).
- **Brand** — a tenant with multiple brands may run different modes per
  brand.
- **Jurisdiction** — a jurisdiction may require or forbid a particular
  mode (a licence condition, or a market where only a licensed external
  provider's odds-compliance certification is accepted).
- **Asset** — a mode may only support certain settlement assets (e.g. an
  external provider integration that only settles in fiat; the in-house
  engine may support the platform's full asset registry per §11).
- **Market/event/product** — the finest-grained dimension: even within one
  tenant/brand, some Sports/Competitions might route to the in-house engine
  (established, well-modeled markets) while others route externally
  (breadth-of-catalogue markets the in-house engine does not yet cover) —
  a genuine future **hybrid** mode, not merely "pick one."
- **Provider health** (§2.3/§4.1's `HealthStatus`) — a live signal, not a
  configuration row: routing fails over to a configured fallback (a
  different external provider, the in-house engine, or a hard deny) when
  the currently-routed option reports unhealthy.
- **Operational configuration** — a manual override (e.g. a staff-initiated
  failover during an incident), audited like any other administrative
  action.

### 5.2 What this document commits to now, and what it defers

`ARCHITECTURAL DECISION`: the canonical domain model (§1) and the
financial contract (§6) are **identical regardless of which mode is
active** — this is the property that makes hybrid, per-market routing
possible without a rewrite, and it is verified by construction: §1's
model, §2's `SportsbookProvider`, and §3's in-house engine all produce the
same canonical `Bet`/`Settlement`/`Void` shapes and the same
`sportsbook.bet.*` events (§7), so a consumer downstream of "a bet was
placed" never needs to know or care which mode produced it.

`OPEN DECISION`, explicitly deferred, not designed here per the directive:
the exact precedence/specificity resolution order across the seven
dimensions above (mirroring ADR 0031 §5's and doc 26 §1.4 H6's own
most-specific-wins pattern, which this routing layer should reuse rather
than invent a competing ordering), the exact schema of the routing
configuration table(s), and whether a genuinely mixed per-bet-slip
(multi-provider single slip) mode is ever required — the directive asks
only that the architecture not foreclose this, which it does not: nothing
in §1–§4 assumes a single active mode per tenant.

---

## 6. Financial Contract

`ARCHITECTURAL DECISION` at this document's level of detail; the
posting-level design is `docs/decisions/0038-sportsbook-accounting-and-
ledger-integration.md` (`ledger-finance`, authored in parallel this same
stage) and is authoritative on account types, entry shapes, and any
sportsbook-specific ledger schema question (e.g. the `player_locked`
stake-origin gap `financial-transaction-flows.md` Flow 8 already flags as
an open precondition). This document confirms the boundary and does not
restate or second-guess ADR 0038's detail.

**The contract, stated at the boundary this document owns**: every
sportsbook operation — bet placement, rejection, settlement (win/loss/
push), void, cancellation, cashout, and reversal/correction — is expressed
by the sportsbook domain (§2 or §3, indistinguishably, per §5.2) as a
canonical financial instruction into the existing `Wallet →
LedgerAccount → LedgerTransaction → LedgerEntry` architecture. The
sportsbook domain **never** bypasses this: never writes a `ledger_accounts`
row, a `ledger_transactions` row, or a `ledger_entries` row itself, and
never mutates a balance directly (§3.6). This is consistent with, and adds
nothing beyond, everything already established: CLAUDE.md's ledger rules,
the Stage 0 version of this document's own `player_locked` model
(restated, not weakened, §1.6), and `financial-transaction-flows.md` Flows
8–11 (bet lock, settlement, void, partial settlement/cashout), all of which
remain the binding specification for entry shapes pending ADR 0038's final
detail.

### 6.1 In-house-mode ledger-posting idempotency routing (Stage 4H-B0-R5 addition)

`ARCHITECTURAL DECISION`, closing a gap `ledger-finance` identified this
stage in ADR 0038 §14.6 (triggered by an independent `bonus-engine`
review) and explicitly flagged back to this document to state, rather
than fix unilaterally in ADR 0038 alone. Restated here at the level of
detail this document owns — what the in-house engine's own adapter/
posting layer must do when it hands a posting instruction to
`internal/ledger`/`internal/wallet` for an in-house-originated event — not
re-derived; ADR 0038 §14.6 is authoritative on the underlying database
constraint design, the full rationale (including why a reserved
`provider_id` sentinel was rejected), and a worked two-attempt retry
example.

(a) **`provider_id` is left `NULL`** on every ledger-posting instruction
the in-house engine hands to `internal/ledger`/`internal/wallet` for an
in-house-originated event (bet acceptance, settlement, void, partial
settlement, cashout, or rollback/re-settlement alike) — consistent with
ADR 0033 §2's canonical-event statement that this document already
implicitly relies on (§0's design test that a consumer cannot tell,
from the event shape, whether the originating engine was external or
in-house). `provider_id` is **never** translated into a reserved
sentinel value at the ledger-posting boundary: `NULL` in from the
canonical event, `NULL` stored on the `LedgerTransaction` row.
`provider_tx_id` correspondingly stays `NULL` too — it is never
half-populated while `provider_id` is `NULL`.

(b) **The in-house engine's adapter/posting layer mints one stable,
intrinsic, per-event reference** for each lifecycle event — bet
acceptance, settlement, void, and per-occurrence partial settlement/
cashout/rollback alike — and supplies it as `idempotency_key`, **never**
as `provider_tx_id` (which stays `NULL` per (a)). This reference must be
derived from a signal intrinsic to the specific event (e.g. the in-house
engine's own acceptance/settlement/void decision id), assigned once and
reused verbatim on every retry of that same event — **never** freshly
regenerated per attempt (a fresh UUID per retry would satisfy the letter
of "enforced by a database constraint" while never actually causing the
constraint to fire, defeating the point). This is what routes in-house
postings through `UNIQUE (tenant_id, idempotency_key)` (unconditional, no
`WHERE` clause) rather than the provider-keyed partial index `UNIQUE
(tenant_id, provider_id, provider_tx_id) WHERE provider_id IS NOT NULL`,
which never evaluates for a `provider_id IS NULL` row and would otherwise
leave in-house postings with no database-level duplicate protection at
all. See ADR 0038 §14.6 for the full routing table (by mode) and the
worked retry example this rule is built to satisfy.

---

## 7. Bonus/Gamification Integration

`ARCHITECTURAL DECISION` — confirmed, not reinvented. Sportsbook activity
feeds the canonical Activity/Event architecture (doc 22) exactly like every
other domain: `sportsbook.bet.placed`, `sportsbook.bet.settled`,
`sportsbook.bet.void_cancelled` (doc 22's candidate taxonomy table, already
listing `sportsbook` as the `source`), themselves derived from ADR 0033
§2's `sportsbook_bet`/`sportsbook_settlement`/`sportsbook_void_cancel`
canonical-event contract. No separate sportsbook-only bonus/gamification
mechanism is designed or permitted here — Bonus Engine, Gamification
(Points/XP/Missions/Tournaments/Leaderboards), Reward Orchestrator, Risk,
RG, and Reporting all consume these same canonical events, per doc 22's
consumer contract, unchanged by this document.

### 7.1 ADR 0033 already defines exactly the boundary a dual-mode sportsbook needs

`docs/decisions/0033-provider-interoperability-and-external-bonus-
engines.md` §1–§3 defines the `fulfillment_owner`
(`platform | external_provider:<provider_id>`) split, the internal-vs-
external-grant coexistence model, and the canonical-event contract this
document's §6/§7 rely on. This is exactly the mechanism needed for "a
provider's own native bonus engine coexisting with the platform's Bonus
Engine" — this document cites it and extends it (below), rather than
reinventing it, per the task's explicit instruction.

### 7.2 Gap flagged: ADR 0033 was written assuming an external provider always exists; dual-mode confirms it does not

**Flag, for `architect`/`bonus-engine` confirmation, not adopted
unilaterally here** (this document does not redesign ADR 0033, per
CLAUDE.md's "no specialist redesigns shared architecture unilaterally"):

ADR 0033's own Context section frames the whole document around "a
sportsbook provider is the platform's first concrete case of an external
vendor that runs its own bonus engine" — every example, and the entire
§1/§3 coexistence model, is written as if an external `provider_id`
identifying a real vendor is always present whenever sportsbook activity
occurs. Stage 4H-B0-R4 confirms that is no longer guaranteed: the in-house
engine (§3) has **no external vendor and no external bonus engine at
all** — every sportsbook promotional entitlement for an in-house-engine
bet is necessarily `fulfillment_owner = platform`, fulfilled entirely by
the platform's own Bonus Engine, with no `external_reward_grant` row ever
created for that activity.

This does not break ADR 0033's model — the `platform` fulfillment path
already exists and needs no change — but it surfaces two concrete,
narrowly-scoped gaps worth naming precisely rather than leaving implicit:

1. **`provider_id` on the canonical `sportsbook_bet`/`sportsbook_settlement`/
   `sportsbook_void_cancel` events (ADR 0033 §2) has no stated convention
   for the in-house-engine case.** ADR 0033 §2 lists `provider_id` as a
   carried field without saying what it is when there is no external
   provider. Leaving it `NULL`/absent is the obvious choice, but ADR 0033
   itself never states this, because it was written assuming `provider_id`
   always identifies a real external vendor — a Bonus/Gamification
   consumer written against ADR 0033's examples alone could reasonably
   (and wrongly) assume `provider_id` is always populated. **Recommended
   resolution** (not adopted here, flagged for the taxonomy/ADR 0033
   owners): doc 22's envelope already carries `source = "sportsbook"`
   regardless of mode, so the fix is narrow — ADR 0033 §2 (or doc 22's
   eventual schema) should state explicitly that `provider_id` is nullable
   and `NULL` means "in-house engine," never a reserved sentinel string
   that could collide with a future real vendor's own `provider_id`.
2. **ADR 0033 §3's "coexistence" model degenerates to a single-source case
   for in-house-engine activity, and this should be stated, not left to be
   discovered.** For an in-house bet, there is exactly one bonus source
   (the platform's own Bonus Engine) rather than two — the
   `fulfillment_owner`-per-active-promotion tracking ADR 0033 §3 requires
   still works unmodified (every row is simply `platform`), but a future
   implementer reading §3's "two independent bonus sources on the same
   player" framing without this document's context could reasonably wonder
   whether coexistence logic is broken or inapplicable for in-house bets.
   It is neither — it is correctly and safely degenerate. Flagged so it is
   recorded rather than re-derived, or worse, mistaken for a defect during
   implementation.

Both gaps are documentation/precision gaps in how ADR 0033's already-sound
model is *stated*, not architectural defects requiring a new mechanism —
consistent with ADR 0033's own precedent of flagging refinements to the
Orchestrator rather than silently substituting a competing design.

### 7.3 Stage 4H-B1 Wave 1 confirmation — coexistence boundary, correlation contract, and wagering-progress interaction

`ARCHITECTURAL DECISION` (confirmation, one additive extension), recorded
because Stage 4H-B1's Bonus Engine implementation dispatch asked this
document and ADR 0038 to confirm three things hold, rather than re-derive
them from the Bonus Engine side.

1. **Coexistence boundary confirmed, not reopened.** ADR 0033 §0/§1/§3 and
   `10-bonus-engine-architecture.md` §3.2/§10 already state, jointly, that
   an external sportsbook provider's own bonus engine and its provider-
   native bonus state are provider-scoped and never merged into the
   platform's own Bonus accounting: an `inside_provider`-destined Grant
   posts zero ledger entries (ADR 0032 §6(c)), and a provider's own
   promotion that never passed through our Reward Orchestrator at all
   (never even producing an `external_reward_grant` row) is, a fortiori,
   never recognized as platform Bonus liability. ADR 0033 §2.1 (added this
   stage) closes the one narrow sentence that was previously implied but
   not written down: this holds "unless an explicit future contract says
   otherwise," per ADR 0033 §2's own hard process rule requiring
   `architect`/Reward-Orchestrator sign-off before the canonical contract
   is ever generalized.
2. **Correlation contract**: ADR 0033 §2.1 adds one optional, nullable,
   provider-opaque `provider_promo_reference` field to the
   `sportsbook_bet`/`sportsbook_settlement`/`sportsbook_void_cancel`
   canonical events (cashout and partial settlement are
   `sportsbook_settlement` discriminators, per §2, and inherit the field
   identically) — populated only when a provider's own payload supplies
   one, never inferred, never required, and never given meaning outside a
   correlation/audit lookup. No new event type, no new
   `transaction_type`, no ledger-schema change.
3. **Wagering-progress interaction (Model C) confirmed sufficient as-is.**
   `ledger-accounting-model.md` §6.6.7 cases 11 ("in-house sportsbook
   occurrence") and 12 ("external sportsbook occurrence") are explicitly
   worked and confirmed **identical by construction** — the derivation
   reads only `transaction_type`, `correlation_id`,
   `reverses_transaction_id`, `account_type`, `direction`, and `amount`,
   never a provider field (§6.6.9 property 6). **Nothing further is needed
   from this document's side** for Wave 1's Bonus domain-model dispatch to
   consume sportsbook's ledger facts correctly: the contribution record
   Model C requires is a `bonus-engine`-owned table keyed off
   `lock_ledger_transaction_id`, sourced entirely from facts this
   document's §6/ADR 0038 already commit to posting (correlation_id
   stability across a bet's lifecycle, per §6's `sportsbook_bet`/
   `sportsbook_settlement`/etc. discipline). This confirmation is scoped to
   the mechanism only — it does not resolve, and does not need to resolve,
   any of the still-open items §6.6.11 lists (G-2, the push-counts-as-
   wagering question, multi-Grant attribution, FD-1/FD-2), all of which
   remain `bonus-engine`'s/upward-referred, unchanged by this section.
4. **Mixed/bonus-funded cashout stays gated; what Bonus can safely build
   now is unaffected.** FD-1 (`ledger-accounting-model.md` §6.5.10) —
   cashout's wagering-progress treatment, to be decided together with its
   proceeds-split policy (ADR 0038 §8.3's `OPEN DECISION`, item 6) — is
   **not** selected here, consistent with this document's own limitation
   against deciding a multi-month commercial/product tradeoff unilaterally.
   What is already safe to build without that decision: (a) cash-funded
   cashout's posting shape is unaffected (ADR 0038 §8.3, `RESOLVED
   (architecture) — NOT IMPLEMENTED`) and never touches bonus wagering-
   progress at all, since no `player_locked_bonus`/`player_bonus` account
   is involved; (b) bonus- and mixed-funded cashout is **already
   BLOCKED** at the ledger-posting level pending the `player_locked`
   origin split (ADR 0038 §9/§15) — sportsbook cannot post one today even
   if it wanted to; (c) `ledger-accounting-model.md` §6.6.5's exhaustive
   classification switch already specifies the correct fail-closed
   behavior for the day cashout does ship: `sportsbook_cashout` is
   explicitly **UNCLASSIFIED**, excluded from `P_firm`, and raises an
   integrity alert rather than silently defaulting either way — this is
   exactly the "reject deterministically, fail-closed" posture the Bonus
   Engine dispatch asked to confirm is buildable, and it requires no
   sportsbook code change to hold, since sportsbook has not built cashout
   at all yet (confirmed, `ledger-accounting-model.md` §6.5.10: "no
   sportsbook code exists to offer or accept a cashout price"). No part of
   this document authorizes building cashout, bonus-funded or otherwise,
   this stage.

---

## 8. Responsible Gaming

`ARCHITECTURAL DECISION` — confirmed, high-level only; the detailed
RG-integration design is `docs/decisions/0034-bonus-gamification-rg-kyc-
identity-integration.md` (`identity-compliance`, authored in parallel this
stage), which this document defers to and does not restate.

Sportsbook consumes the existing central RG eligibility model
(`internal/rg`, `rg.EvaluateEligibility`) **exclusively** — no
sportsbook-specific self-exclusion, limit, or cool-off mechanism is
designed or permitted here, mirroring ADR 0034's own restated rule for
Casino/Bonus and CLAUDE.md's "enforcement is our code, not the vendor's."

- **Where RG must be evaluated**: before bet **acceptance** (§3.3 step 2,
  and equally before an external provider's `PlaceBet` call is made in
  external mode — the RG check happens in the platform's own orchestration
  layer regardless of which mode is active, never delegated to or trusted
  from a provider), before **cashout** is offered/executed (a cashout
  credits `player_cash`/`player_bonus` and is exactly the kind of
  money-affecting operation ADR 0027's "any future endpoint that can move
  money... MUST call `EvaluateEligibility`" forward note already governs),
  and RG's own status changes (`rg.status.changed`, doc 22) are consumed —
  never produced — by sportsbook, exactly as they are by every other
  domain. **Settlement** (win/loss/push determination, and re-settlement
  after a correction) is **not** independently gated by RG at the moment
  of settlement itself, because it resolves a liability already accepted
  under an RG check made at acceptance time — this mirrors Casino's own
  settlement path, which does not re-run RG per `postWin`/`postBet`
  settlement either, only at the money-committing step.
- **Fixed ordering, no override parameter**: RG runs **before** Risk, in
  the same transaction as the effect, with no caller-supplied parameter
  able to skip or reorder it — restated, not modified, from ADR 0031 §1's
  already-established platform-wide ordering, applied to sportsbook's own
  acceptance/cashout call sites exactly as §3.3 states.
- **No bypass via mode**: because RG evaluation happens in the platform's
  own orchestration layer (§3.3) rather than inside either a
  `SportsbookProvider` adapter or the in-house engine's trading logic, an
  external provider integration cannot bypass RG by, e.g., accepting a bet
  synchronously on its own side before notifying the platform — the
  platform's own pre-flight RG/Risk/funds check (§3.3) is what authorizes
  the outbound `PlaceBet` call in the first place; a provider that
  self-accepts without waiting for the platform's authorization is a
  provider integration defect flagged during conformance testing (§2.4),
  not a gap this architecture's ordering has.

---

## 9. Reporting/Audit

`ARCHITECTURAL DECISION`. Both external and in-house sportsbook activity
produce **canonical** reporting/audit events — never a provider's
proprietary schema surfacing in a report or an audit record — while
preserving provider references as non-authoritative metadata where
applicable:

- Every canonical `sportsbook.bet.*` event (§7, doc 22) carries an
  `operation_ref`/`provider_ref` (doc 22's envelope field) pointing at the
  domain's own source-of-truth row (the Bet, keyed by the platform's own
  identity) plus, where one exists, the provider's own reference —
  reportable and searchable by either, authoritative by neither alone
  except the platform's own row.
- Every Trading Operation (§3.5) and every provider-adapter-level decision
  that affects a Bet (accept/reject, cancel, cashout offer) writes an
  `audit.Record` (actor, tenant, entity, before/after, reason code — the
  same shape CLAUDE.md already mandates platform-wide), regardless of
  mode.
- **Open-liability reporting**, restated unchanged from the Stage 0 version
  of this document: sportsbook GGR is only known at settlement, so every
  sportsbook report needs an explicit open-liability line (computed from
  the Ledger's `player_locked` projection, §1.7/§6 — never from the
  trading engine's own Exposure figure, which is a different number for a
  different purpose), owned jointly with `data-analytics`. This applies
  identically whether the open liability originated from an external
  provider's bet or the in-house engine's own book.
- A provider's own settlement/report file (where offered) is used for
  **reconciliation against the ledger** — the same "compare our own record
  against the provider's own report, exception-queue any mismatch"
  discipline ADR 0033 §1.3/ADR 0023's reconciliation sections already
  establish, never adopted as an alternative source of truth.

---

## 10. Retail Compatibility

`ARCHITECTURAL DECISION` — confirmed compatible with `docs/decisions/0035-
retail-agent-network-accounting.md` and `docs/architecture/26-retail-
operations-architecture.md`, with no sportsbook-specific retail concept
introduced here.

A retail terminal/cashier is, per doc 26 §2, an API client of the same
platform — never a second financial system and never a second sportsbook
engine. Nothing in this document's §1–§9 is retail-aware, and nothing
needs to be: a retail-originated sportsbook bet is an ordinary Bet (§1)
placed by an ordinary `PlaceBet`-equivalent call into the same §3
acceptance pipeline (RG → Risk → funds check → ledger lock, §3.3), with the
counter-operation's own idempotency (`internal/ledger`'s
`(tenant_id, idempotency_key)` shape, per doc 26 §2.4/ADR 0035 §8.1)
layered on top the same way it already is for a retail counter deposit —
sportsbook adds no new idempotency mechanism of its own beyond what §2.3/
§6 already establish. Identity, Wallet, Ledger, Risk, RG, Bonus,
Gamification, Reporting, and Audit remain shared across online and retail
for sportsbook activity exactly as doc 26 §6 already establishes for
casino/bonus — retail introduces a new **actor and entry point**, never a
parallel sportsbook domain. This document creates no obstacle to a future
retail cashier placing, viewing, or cashing out a sportsbook bet through
the same domain/APIs an online player uses, once retail implementation is
authorized.

**Forward-looking note (Stage 4H-B0-R7 sportsbook financial-contract
conformance pass) — not a defect, correctly out of scope today.** The
above holds precisely because the current retail baseline (doc27's First
Retail Product Baseline) excludes proxy/assisted play and anonymous/
bearer play for its first slice: the wallet a retail-placed bet debits
genuinely is the player's own wallet (`player_cash`/`player_bonus`), so
`correlation_id` and the `player_locked_cash`/`player_locked_bonus`
lock/settlement flow (`ledger-accounting-model.md` §6.4) need no third
case. If a future retail/agent-network slice ever adds proxy/assisted
play (an agent placing on a player's behalf) or a bet funded directly
from `agent_float` rather than the player's own wallet, the current
single-origin locked-value model would need the same kind of
origin-split treatment `player_locked` already needed for cash-vs-bonus,
and the mixed-funding hard-rejection rule (§6.4.1) would need to decide
whether agent-float-funded stakes are in or out of scope. Recorded here
for whenever that retail slice is scoped — not a gap in anything built
or authorized today.

---

## 11. Multi-Asset Compatibility

`ARCHITECTURAL DECISION` — confirmed asset/exponent-aware throughout,
never hardcoded to any closed currency/asset list, consistent with ADR
0021 and conceptually anticipating ADR 0037 (Asset/Currency Registry +
FX/Conversion architecture, `architect`, authored in parallel this same
stage).

- **Stake, payout, potential return**: `NUMERIC(38,0)` minor units plus
  `assets.decimal_exponent` looked up per asset (§1.4's `Price` is a
  dimensionless decimal multiplier and needs no asset dimension itself;
  every monetary figure computed *from* a Price — stake, potential return,
  settlement payout — does, per ADR 0021's standing rule).
  `BetLeg`/`Bet` carry an explicit `asset_code`, never an assumed currency.
- **Exposure and Liability (§1.7)** are computed **per asset**, never
  netted across assets — an aggregate cross-asset exposure figure would
  require an FX conversion decision at read time, which this document does
  not make (ADR 0021's `ConversionOperation`/FX architecture is the only
  place that decision belongs, and it explicitly does not exist as an
  automatic engine, ADR 0021 "no exchange engine is built").
- **Settlement/void/cashout** post in the same asset the stake was locked
  in (per Flow 8/9/10/11) — a sportsbook bet is never silently settled in
  a different asset than it was staked in; any future player-initiated
  cross-asset conversion of sportsbook proceeds is an ordinary, separate,
  explicit `ConversionOperation` (ADR 0021), never a property of
  settlement itself.
- **No closed list anywhere**: nothing in §1–§10 enumerates currencies or
  assets by name (this document itself uses "EUR"/"BTC" only as
  illustrative examples in prose, never as a schema value) — every asset
  dimension is a lookup against the platform's `Asset` registry, per
  CLAUDE.md's standing rule and ADR 0021's restatement of it, and remains
  so under whatever registry/FX shape ADR 0037 formalizes.

---

## 12. Jurisdiction and Licensing

`ARCHITECTURAL DECISION` — confirmed jurisdiction-aware and compatible with
the hybrid licensing model (`docs/architecture/15-jurisdiction-and-
licensing-model.md`), no jurisdiction or licensing assumption hardcoded.

- **No single jurisdiction assumed anywhere in §1–§11.** Every example
  (Anjouan, a LATAM market) used elsewhere in this codebase's sportsbook-
  adjacent documents is illustrative; this document's canonical model,
  provider abstraction, and in-house engine carry no jurisdiction-specific
  behavior as a code path.
- **Enforcement points reuse doc 15's existing mechanism, unchanged**:
  catalogue/market availability (which Sports/Competitions/Markets a
  tenant may offer) is filtered by `TenantJurisdictionConfig`'s existing
  geo-block/permitted-products mechanism (doc 15's "Provider availability"
  enforcement point, already stated to cover sportsbook catalogue
  filtering); RG/KYC rulesets resolve per `TenantJurisdictionConfig` exactly
  as they do for every other domain (doc 15's "KYC/AML/RG" enforcement
  point); and `internal/risk` already consumes `JurisdictionCode`/
  `LicensingMode` as optional `RiskRequest` scope dimensions (doc 15,
  "Risk & Limits consumption", ADR 0031 §9/§10) — sportsbook's own risk
  rules (§3.4) inherit this unchanged, requiring no new jurisdiction
  mechanism.
- **Hybrid licensing compatibility**: a platform-licensed tenant and a
  self-licensed (BYOL) tenant may each independently choose external vs.
  in-house sportsbook mode (§5) — the mode-selection routing layer's
  `jurisdiction`/`tenant` dimensions are exactly the hooks doc 15's
  `licensing_model`/`Licence.permitted_products` model already provides,
  never a new licensing concept. A self-licensed tenant's own regulatory
  obligations for sportsbook (e.g. a licence condition requiring a
  specific certified odds-compliance provider) are expressed as ordinary
  routing configuration (§5.1's jurisdiction/tenant dimensions) and/or a
  `Licence.permitted_products` constraint, never as a platform code branch
  keyed on that tenant's identity.

---

## 13. Historical record — the Stage 0 proposal this document supersedes

Preserved verbatim for provenance, per this stage's governance
requirement not to silently discard prior recorded reasoning:

> **Build/buy line.** Odds, trading, and risk management are never built
> in-house. Two integration shapes exist: **Widget/iframe** (provider
> renders the whole betting experience; platform supplies authentication
> and a seamless wallet — weeks of work, **RECOMMENDATION: start here**,
> per Blueprint's explicit guidance); **Feed and API** (platform receives
> odds and places bets programmatically, builds its own UI — months of
> work, more presentation control, still no ownership of risk; deferred
> unless a specific commercial reason emerges). Start on the same wallet
> as casino so the player sees one balance.
>
> **What is genuinely platform-owned.** An open sportsbook bet is a
> liability that can span days or months, unlike a casino round that
> settles in milliseconds. Stake moves to `player_locked` at placement;
> an open-bet record carries potential return; settlement, void, partial
> settlement, and cashout are each distinct ledger events; markets can be
> corrected after initial settlement, requiring re-settlement as its own
> event.
>
> **Reporting consequence.** Sportsbook GGR is only known at settlement;
> every sportsbook report needs an explicit open-liability line.

**What this stage corrects about the framing above, explicitly**: the
opening sentence — "odds, trading, and risk management are never built
in-house" — is the one clause this document does not carry forward as an
architectural constraint. It was correct as a Stage 0 **build/buy business
recommendation** (and remains correct as sequencing guidance, §14), but a
later, confirmed product requirement now requires the architecture to
support building odds/trading/risk-consuming logic in-house (§3) as a
first-class capability. Everything else in the historical proposal above —
the `player_locked` model, the distinct-ledger-events rule, the
re-settlement rule, the open-liability reporting rule — was already
correct and is restated, not superseded, throughout §1–§9 above.

---

## 14. Operational/sequencing guidance (not an architectural constraint)

`RECOMMENDATION`, explicitly demoted from architectural constraint to
sequencing guidance per §0: **start commercial delivery with an external
provider, widget/iframe shape first**, because it is the fastest path to
proving the wallet/ledger/RG integration end-to-end (weeks, not months),
and defer investing in the in-house engine's trading/pricing depth until
either (a) a specific commercial reason emerges (the Stage 0 document's own
original condition, still sound), or (b) the business decides in-house
sportsbook is a strategic priority independent of any single deferral
condition. This recommendation binds **delivery sequencing only** — it
does not and must not constrain §1's canonical domain model, §2/§4's
provider-neutral abstractions, §3's in-house engine design, or §5's routing
architecture, all of which remain first-class regardless of which mode
ships first. Feed/API-only integration (external, no widget) remains the
same "deferred unless a specific commercial reason emerges" position the
Stage 0 document took, unchanged, since it is squarely covered by §2's
external-provider architecture already and needs no separate treatment.

---

## Ownership and stage mapping

Owned by `sportsbook`, with `ledger-finance` review on all ledger-event
postings (ADR 0038), `risk` review on the exposure/liability integration
point (§3.4), `identity-compliance` review on the RG integration point
(§8, ADR 0034), `bonus-engine`/`architect` review on the ADR 0033 gap flag
(§7.2), and `architect` + `security` + `qa` independent review of this
document as a whole per this stage's governance requirement. Stage mapping
is unchanged from the Stage 0 document: sportsbook implementation remains
Stage 5 in the build sequence, deliberately after wallet/ledger (Stage 3)
and compliance/bonus (Stage 4), since it depends on both a working shared
wallet and RG controls already being in place — this document changes what
Stage 5 must be capable of building (both modes, §0), not when Stage 5
starts.

## Open questions for reviewers

0. **Stage 8 (ADR 0080 Decision 2) readiness addition, deliberately left
   undecided**: `sportsbook_bets` now carries nullable `provider_id`/
   `provider_bet_reference` columns for a future real provider's own bet
   acceptance reference, but the ordering question this raises — for a
   real provider whose bet-placement contract turns out to be
   asynchronous (accept-then-confirm rather than immediate ack), what is
   the correct ordering between provider acceptance and the
   `player_locked_cash` posting, and does `sportsbook_bets.status` need a
   pre-acceptance/pending state to represent it? — is explicitly **not
   decided or built**. It cannot be decided without knowing whether real
   bet placement is even synchronous, which requires the actual provider
   contract (see `docs/integrations/dummy-sportsbook.md`, itself pending).
   Recorded here, not only in that integration-status file, because that
   file is expected to be rewritten in full once real documentation
   exists — this open question should survive that rewrite.
1. **§7.2's ADR 0033 gap** — is the recommended resolution (nullable
   `provider_id`, `NULL` = in-house) the right fix, or should the
   canonical event envelope (doc 22) instead carry an explicit
   `origin_mode ∈ {external, in_house}` discriminator alongside a nullable
   `provider_id`, so a consumer never has to infer mode from the absence of
   a field? Flagged for `architect`/doc 22's owner, not resolved here.
2. **§1.3's Market subject polymorphism** (`event | season`) — confirm this
   two-member set is sufficient, or whether a `competition`-level subject
   (a futures market spanning multiple seasons, e.g. "next Season's
   Champion" priced before a Season is even confirmed) is a real enough
   case to add now rather than later; this document deliberately kept the
   set minimal per the no-scope-expansion rule.
3. **§3.4's Risk boundary** — confirm with `risk`'s own parallel Stage
   4H-B0-R4 work that `Operation = sportsbook_bet` (already present in
   ADR 0031's enum) is sufficient, or whether Trading Operations (market
   suspension driven by Exposure thresholds) need their own distinct
   `Operation`/`LimitKind` values under ADR 0031 §16/§12's extension
   process — this document intentionally did not decide that, per the
   task's instruction to state the boundary only.
4. **§5.2's routing precedence** — confirm the recommendation to reuse ADR
   0031 §5's most-specific-wins ordering verbatim (rather than defining a
   new one) is acceptable once a real routing implementation is scoped.
5. **§1.5's BetSlip-as-request decision** — confirm no product requirement
   (e.g. a "save my bet slip for later" feature) needs BetSlip to be a
   persisted, resumable aggregate after all; if so, it would need its own,
   narrowly-scoped state machine that still never competes with `Bet` as
   the settlement-relevant record.

---

## 15. External-first vs. in-house-first — implementation sequencing recommendation (Stage 4H-B0-R5)

`RECOMMENDATION` — a business/engineering sequencing recommendation only,
owned by `sportsbook`, for whichever specialist/Orchestrator eventually
authorizes a real Stage 5 sportsbook implementation. **This is not an
architectural constraint and does not amend §0–§14 above.** The directive
for this stage is explicit and is restated here so it cannot be
misread by a future implementer: the platform continues to
architecturally support **both** external-provider integration (§2) and
an in-house engine (§3) as co-equal, first-class capabilities, regardless
of which is built first, and regardless of anything concluded below.
Nothing in this section authorizes building either one — Stage 5 remains
unauthorized (§ "Ownership and stage mapping" above), and this document's
own status line (`NOT IMPLEMENTED`) is unchanged by this section. No real
vendor, sports-data feed, or commercial term is named anywhere below.

### 15.1 Method

Each dimension below is evaluated for *this* platform's actual, currently
specified architecture — citing doc 09's own sections and ADR 0038's own
financial contract — not for sportsbook implementations in the abstract.

### 15.2 Dimension-by-dimension

**1. Time to production.** Concretely asymmetric, per what §2 and §3
actually specify. External-first (once a real vendor contract exists)
requires: one adapter implementing §2.1's `SportsbookProvider` interface
(`Capabilities`, `Catalogue`/`Markets`/`Odds`, `PlaceBet`/`GetBet`/
`BetStatus`/`CancelBet`/`Cashout`, `HandleCallback`, `HealthStatus`), a
`SportsbookAdapterCapability` declaration (§2.2), per-tenant credentials/
routing config (§5), and the conformance suite (§2.4) — the same shape of
work ADR 0025 already proved out for `CasinoProvider` and ADR 0022 for
`PaymentProvider`, i.e. an incremental extension of an existing,
already-implemented pattern, not new architectural ground. In-house-first
requires building all five layers of §3.1 from nothing — SPORTS DATA
ingestion/canonicalization (§4), the SPORTSBOOK BUSINESS LOGIC catalogue/
market/odds-line management (§3.2), bet validation/acceptance (§3.3),
TRADING OPERATIONS (odds-setting, suspension, exposure management, §3.6),
and settlement/result-ingestion/cashout logic (§3.5) — plus every ADR 0038
posting path currently specified but `NOT IMPLEMENTED` (§3–§10 there). None
of this exists anywhere in `internal/` today (ADR 0038 §13 confirms even
the Risk `Operation` enum value is unwired because `internal/sportsbook`
does not exist as a package). This is a materially larger, first-of-its-
kind build, not a difference of degree.

**2. External dependency.** The one dimension that does not point the same
direction as the rest, and is stated honestly as such. External-first
requires a **signed commercial relationship with a real sportsbook
provider before any implementation can start at all** — per §13's
historical framing (no such relationship exists yet) — a dependency
entirely outside engineering's control and subject to another party's
contracting timeline, exactly the caveat MVP doc 14's "What still requires
human/vendor/legal involvement" section already names generally for
provider/PSP contracts. In-house-first requires no betting-platform vendor
relationship for the trading/acceptance/settlement logic itself, but does
require a sports-data-feed relationship (§4) to have real events/odds to
trade against before the engine is anything more than an empty catalogue —
a smaller-scope dependency (a data feed, not a full betting-platform
vendor with regulatory/trading-certification weight), but still an
external dependency, not a clean "no dependency" case for in-house. Net:
in-house-first can begin **engineering** work (schema, catalogue,
validation pipeline) without waiting on any contract; it cannot reach a
*meaningfully tradeable* state without one.

**3. Canonical model validation.** Favors external-first. §0's own stated
test for this document is that the canonical model (§1) is
provider-neutral "by construction," verified on paper by the R4
extensibility exercise but **not yet verified against any real vendor's
actual API shape** — nothing has stress-tested §1.3's Market
template/instance split, §1.4's Price/Line versioning, or §1.7's
Exposure/Liability distinction against a real vendor's actual field names,
event/market taxonomy, or settlement-callback shape. Building the first
real *external* integration is what forces that mapping exercise
cheaply and early — mirroring exactly how ADR 0025's `CasinoProvider`
mapping and ADR 0022's `PaymentProvider` mapping each surfaced real model
gaps against a real vendor's documentation, not in the abstract. Building
the in-house engine first risks the opposite failure mode: the model
"looks" clean because the in-house engine's own code was written to fit
it, with no external pressure ever testing whether §1 actually
generalizes past this platform's own assumptions — the precise blind spot
a from-scratch build cannot see in itself.

**4. Trading complexity.** Favors external-first, and this is the
dimension this recommendation weighs most heavily. §3's own five-layer
design — specifically the TRADING OPERATIONS layer (§3.6): odds-setting,
market suspension, exposure management — is exactly the functionality the
original Stage 0 framing (preserved verbatim in §13) identified as never
built in-house, a position §0/§13 correctly demote from an architectural
constraint to a sequencing input, not discard. In-house-first means this
platform's first-ever experience pricing risk (Bonus and Casino, per §13's
restated framing, never price risk the way a sportsbook trading desk
does) would happen simultaneously with standing up the rest of the
sportsbook stack — RG (§8), Risk integration (§3.4), the full ADR 0038
financial contract — compounding a genuinely new risk discipline on top
of every other new-to-this-platform piece at once. External-first defers
that specific expertise requirement to the vendor while the platform
proves out the parts it already has direct experience with (wallet,
ledger, RG, provider-adapter discipline).

**5. Data feed dependency.** Favors external-first. The in-house engine
needs a live, accurate, adequately-covered sports-data feed (§4) from day
one merely to have anything to trade against — an empty in-house catalogue
with correct code but no real events is not a shippable capability.
External-first needs no data-feed relationship at all: per §2, the
provider supplies and owns its own event/market/odds data internally.

**6. Financial risk.** Favors external-first, decisively. §1.7's own
Exposure/Liability distinction exists specifically because an in-house
engine that mis-prices a Market or fails to suspend it fast enough during
a fast-moving live event creates real, potentially large, **uncapped**
exposure — not a bounded engineering-bug cost, a trading-book loss. §3.6's
own load-bearing rule ("the sportsbook never becomes a second wallet")
protects the ledger's integrity but does not, and cannot, protect the
*trading book* from a bad Price or a late suspension — that risk lives
entirely in the TRADING OPERATIONS layer (§3.6) an in-house-first sequence
would be building and operating soonest, with the least accumulated
platform experience behind it. External-first transfers this specific
category of risk to the vendor's own book entirely; the platform's
exposure is limited to the ordinary provider-integration risk already
covered under dimension 7.

**7. Operational risk.** Favors external-first, though both paths carry
real risk of different categories. External-first's risk — provider
outage, upstream API changes, callback unreliability — is already
substantially mitigated by §2.3's specified hardened callback pattern
(signature verification, tenant resolved from the authenticated URL never
the payload, ambiguous-outcome handling via the `Unknown` state and
tombstone discipline rather than guessing, `HealthStatus`-driven failover
per §5.1). In-house-first's risk is categorically harder to catch before
it costs money: a latent bug in trading/settlement logic (a bad payout
formula, a settlement rule misapplied for a MarketType, a race in market
suspension) has no vendor absorbing or catching the mistake — it surfaces
as either a wrong ledger posting (caught by ADR 0038's reconciliation
discipline, §6/§12, but only after the fact) or a mispriced Market
generating exposure before anyone notices, with the platform itself as
the sole backstop in both cases.

**8. Regulatory readiness.** Generally favors external-first, stated as a
general consideration only, since this document does not hold
jurisdiction-specific licensing detail. A widget/iframe external
integration is commonly the shape a new operator's licence conditions are
already built around, since the vendor typically already carries its own
trading/odds-compliance certifications for the product it operates; an
in-house trading engine may need its own, separate certification effort
before it can be licensed to price and accept bets in a given
jurisdiction — consistent with, and not contradicting, §12's existing
"no jurisdiction or licensing assumption hardcoded" framing, since this
is a general operational observation about certification burden, not a
jurisdiction-specific rule asserted here.

**9. Ability to replace the provider later.** Not a differentiator between
the two sequencing choices, and stated as such rather than silently
omitted. §2.4 already guarantees swapping or adding a `SportsbookProvider`
requires only a new adapter, capability declaration, and conformance pass
— never a change to §1's canonical model, §6's financial contract, or
§7's event taxonomy — regardless of which mode was built first. This
factor does not move the recommendation in either direction.

### 15.3 Recommendation

**Sequence: external-provider-first, then in-house-engine second (if and
when a specific commercial reason or strategic priority emerges, per
§14's existing condition, restated and not weakened by this section).**
This is the same operational conclusion §14 (Stage 0, preserved through
R4) already reached; this section's contribution is the explicit
9-dimension analysis above, done for this stage's specific instruction,
confirming rather than revising it.

**The two or three dimensions that most drove this recommendation**:

1. **Dimension 4 (trading complexity) and dimension 6 (financial risk)
   together** — building genuine odds-setting/market-suspension/exposure-
   management expertise (§3.6) this platform has never needed before, and
   the uncapped trading-book exposure that comes with getting it wrong
   early, is the single largest and most asymmetric risk in the entire
   in-house-first path. External-first defers this specific risk category
   to the vendor's own book while the platform proves out its own
   already-familiar territory (wallet/ledger integration, provider-adapter
   discipline, RG/Risk wiring) against a live sportsbook product for the
   first time.
2. **Dimension 3 (canonical model validation)** — the first real
   integration, whichever it is, is what actually proves §1's canonical
   model is provider-neutral rather than merely claimed to be. Doing that
   proof against a real external vendor's documentation is cheaper and
   faster than doing it for the first time against an in-house engine that
   has had no external pressure testing its own assumptions.
3. **Dimension 1 (time to production), as a supporting factor, not the
   primary driver** — the external path reuses an already-proven adapter
   pattern (ADR 0025, ADR 0022) and is materially smaller in scope than
   standing up all five layers of §3.1 plus ADR 0038's full financial
   contract from nothing.

**Honest tradeoffs on the other side, not minimized**:

- **Dimension 2 cuts the other way.** External-first makes the entire
  sportsbook program hostage to a signed commercial relationship with a
  real provider before any implementation work can begin at all — a
  dependency outside engineering's control. In-house-first could begin
  real engineering work (catalogue schema, canonical data-feed
  ingestion, validation pipeline scaffolding) against a lighter-weight
  data-feed relationship without waiting on that negotiation, at the cost
  of taking on dimensions 4/5/6's risks sooner and with less platform
  experience behind them.
- **Deferring in-house indefinitely does not make the trading-expertise
  gap disappear.** If the business ever wants genuine in-house trading
  capability — the original Blueprint guidance's "never built in-house"
  notwithstanding, since the confirmed product requirement (§0) already
  overrides that as an architectural constraint — that expertise has to
  be built at some point regardless of sequencing. External-first spends
  that effort later, with more platform maturity behind it but also more
  elapsed time before the platform has any in-house trading capability at
  all; in-house-first spends it sooner, at higher near-term risk. This
  recommendation weighs the near-term risk reduction more heavily than
  the deferred-capability cost, consistent with doc 14's B2C-MVP-first,
  avoid-premature-generality philosophy (sportsbook itself is out of MVP
  scope and sequenced at P3/Stage 5 in doc 14's own build order, after
  the wallet/ledger/compliance spine the platform has direct experience
  with) — but this is a judgment call, not a determined fact, and is
  exactly the kind of "does the business consider this a strategic
  priority independent of any single deferral condition" question §14
  already reserves for a future business decision rather than resolving
  unilaterally here.

**What this recommendation does not do**: it does not authorize building
either path (Stage 5 remains unauthorized); it does not change §1's
canonical model, §2's or §3's architecture, §5's mode-selection design, or
ADR 0038's financial contract in any way; and it does not foreclose a
future decision to build in-house first, or both simultaneously for
different tenants (§5.1's per-tenant/brand/jurisdiction/market routing
already anticipates exactly that), should the business's priorities or a
specific commercial opportunity change the calculus above.

## 16. Stage 4H-B0-R6 sportsbook-readiness check on the three foundational primitives

`sportsbook`-authored, documentation-only, per this stage's explicit
directive: verify (not implement) that the R6 financial primitives
(`internal/idempotency`, `internal/assetregistry`'s new `(product,
operation)` dimension, `internal/risk/cumulative.go`'s leg-aware
cumulative model) are actually sufficient for a future sportsbook
implementation, in both provider modes, by reading the shipped code and
its own tests rather than trusting prose. No sportsbook code, migration,
or provider integration is added by this section.

### 16.1 Idempotency (`internal/idempotency`, commit `68675df`) — **SUFFICIENT**

The canonical identifier set (`PlatformOperationID`, `ProviderOperationID`/
`ProviderReference`, `ProviderOccurrenceID`, `OccurrenceOrdinal`,
`CorrelationID`, `CorrectionID`/`ReversalID`) and the composition/routing
primitives (`ComposeOccurrenceKey`/`DecomposeOccurrenceKey`,
`ResolveOccurrence`, `Assign`/`Mode`) map cleanly onto every lifecycle
event a first sportsbook slice reaches:

- **Placement, ordinary settlement, ordinary void** — single-occurrence
  types, no discriminator required (`RequiresOrdinalFunc` returns false),
  `ComposeOccurrenceKey(reference, nil)`. Verified against
  `TestIntegration_CallbackAndSettlementRedelivery`.
- **Partial settlement, cashout** — multi-occurrence types requiring
  `OccurrenceOrdinal`, sourced only from an authenticated payload field
  (`OccurrenceSource`), fail-closed to
  `ErrOccurrenceOrdinalRequiredButUnavailable` when no such field exists,
  with `CanonicalOccurrenceIssuer` as the explicit round-trip fallback for
  a provider with no signed per-occurrence field at all. Verified against
  `TestIntegration_LegitimateSecondOccurrenceDoesNotCollapse` (two
  same-payload occurrences of the same reference, distinct ordinals, both
  post independently — the exact ADR 0038 §14 gap this package exists to
  close).
- **Void/rollback/market-correction re-settlement** — `CorrectionID`/
  `ReversalID` (both `= LedgerTransaction.id` of the transaction being
  corrected/reversed) plus the existing `FOR UPDATE` double-reversal
  protection and tombstone-on-never-seen-original mechanism. Verified
  against `TestIntegration_RollbackRedeliveryCorrectionAndReversal`.
- **Both provider modes** — `Mode`/`Assign` route external-provider
  postings through `(tenant_id, provider_id, provider_tx_id)` and
  in-house postings through the unconditional `(tenant_id,
  idempotency_key)`, per ADR 0038 §14.6, with `provider_id`/
  `provider_tx_id` left `NULL` (never a sentinel) for in-house postings.
  Verified against `TestIntegration_InHouseModeDuplicateDoesNotDoubleDebit`
  and `TestAssign_InHouseMode`/`TestAssign_ExternalProviderMode`.

No gap found. The package is explicitly provider-neutral (`doc.go`: "does
not implement a real sportsbook adapter, a real vendor integration... those
remain the owning domain specialist's work") — confirmed by inspection,
zero references to any named vendor, sports-data feed, or wire protocol
anywhere in the package.

### 16.2 Asset authorization `(product, operation)` dimension (`internal/assetregistry`, commits `eed0416`/`4bcd649`/`3b9cad4`) — **CLOSED, confirmed by test, not merely by claim**

Stage 4H-B0-R5's finding (P1-2 had no casino-vs-sportsbook product
dimension) is genuinely closed: `OperationScope{Product, Operation}` is a
required parameter on every `CheckEligibility` call (a caller that cannot
name its product is denied — `ReasonProductContextMissing` — never
defaulted), and layers 4-7 (`asset_authorizations`,
`asset_operation_eligibility`) carry a nullable `product` column with
most-specific-product precedence (`resolveScopeFact`/`resolveEligibility`:
a row naming the exact product wins over a `product IS NULL` row).

Confirmed against the actual test, not the doc comment's claim:
`assetregistry_integration_test.go`'s scenario at lines 985-1015
authorizes an asset for `casino` wagering, proves that grants **no**
`sportsbook` wagering eligibility (denies at layer 4,
`ReasonTenantNotAuthorized`, since no sportsbook-scoped tenant
authorization row exists), then authorizes layers 4-6 for `sportsbook` but
deliberately leaves layer 7 (operation eligibility) unauthorized and
confirms the distinguishable `ReasonOperationNotEligible` denial. This is
the exact "BTC is wagering-eligible for casino but not for sportsbook in
jurisdiction X" scenario this specialist's Stage 4H-B0-R5 review named as
missing — it is now expressible and independently tested, not just
claimed. No gap found. `assetregistry`'s package doc and code are
vendor/product-neutral: `Product` is a caller-supplied string validated
against `platform_products`, never a hardcoded enum naming "sportsbook" or
any vendor.

### 16.3 Risk cumulative-usage leg-awareness (`internal/risk/cumulative.go`/`denomination.go`, commit `9ff8695`) — **SUFFICIENT, with one minor drift note for the future implementer**

The fix (`cumulativeSpec` requiring an explicit `MeasuredAccountTypes`/
`IgnoredAccountTypes` declaration, the `ledger_accounts` join, and
`ErrUnrecognizedCumulativeLeg`'s fail-closed behavior for any undeclared
player-side leg) closes the exact defect this specialist found in Stage
4H-B0-R5: a cumulative-amount rule over a two-player-owned-leg posting
shape (stake absorption debiting `player_cash` and crediting a locked
account, both wallet-scoped) no longer nets to zero. No sportsbook
operation is wired yet (correctly — no `internal/sportsbook` exists), so
this is proven by analogy, not by a sportsbook-specific test:
`TestEvaluate_CumulativeUsageIsLegAwareForATwoPlayerOwnedLegOperation`
posts a real `withdrawal_requested`-shaped two-player-owned-leg
transaction (`Dr player_cash / Cr player_withdrawal_hold`) — structurally
identical to a `sportsbook_bet`'s `Dr player_cash / Cr player_locked_cash`
— and proves (a) the pre-fix account-type-blind query nets it to exactly
0 (the bug's own signature, asserted in the same test), (b) the fixed
leg-aware query measures the full stake, and (c) `Evaluate` denies a
request the pre-fix evaluator would have wrongly allowed. This generalizes
to a future `sportsbook_bet` cumulative rule by construction: the fix is
in the shared `cumulativeUsage` primitive, not in a casino-specific code
path.

**One drift note, not a gap, for whoever wires `OperationSportsbookBet`
later.** `cumulative.go`'s own reference comment (line ~111) illustrates
the shape a future author would need as `{[sportsbook_bet], [sportsbook_void],
measured=[player_cash], ignored=[player_locked], debit}` — written before
`ledger-accounting-model.md` §6.4/HR-8 (this same stage, Workstream C
phase 1) decided that bare `player_locked` is never minted once migration
`0048` lands; the real ignored account type will be `player_locked_cash`
(and, once gates G-2/G-3 close, `player_locked_bonus` for a bonus-funded
stake). Wiring the comment's literal example against post-migration-0048
data would not silently under-count — `ErrUnrecognizedCumulativeLeg` fails
closed the moment it observes `player_locked_cash` undeclared — but it
would surface as an availability defect (every cumulative sportsbook rule
evaluation erroring) rather than working on the first try. Flagged so the
Workstream C phase 2 / sportsbook implementer updates this comment's
account-type names alongside the migration, not found as a code defect.

### 16.4 Provider-neutrality confirmation (this stage's section 16 requirement)

Confirmed by direct inspection of all three packages' source and package
docs: none references a named sportsbook provider, sports-data vendor,
wire protocol, or vendor-specific field name. `internal/idempotency`'s
`OccurrenceSource`/`CanonicalOccurrenceIssuer` interfaces are declared
against `verifiedEvent any` specifically so the package never depends on
any domain's own parsed-callback type; `internal/assetregistry`'s
`Product` is a caller-supplied, registry-validated string with no
sportsbook-specific literal anywhere in the package; `internal/risk`'s
`cumulativeSpec` map has exactly one production entry (`casino_bet`) and
the sportsbook shape exists only as an unwired, explicitly-labeled
reference comment. All three remain correctly gated behind
`NOT IMPLEMENTED`/`BLOCKED` for any actual sportsbook capability — nothing
in this section authorizes wiring any of them.
