# 07 — Payments Architecture Proposal

Status: Core orchestration principles accepted from Stage 0 (Blueprint
§4.6, §1 "Payments are the hard part, from day one"); custody model and
tokenization priority **updated** per human-approved Stage 0 business
decisions (see `docs/decisions/0008-crypto-custody-provider-abstraction.md`
and `docs/decisions/0009-hosting-hyperscale-cloud.md`).

## Why this is phase one, not a refinement

Anjouan-licensed operators are not boarded by tier-1 PSPs (Stripe, PayPal,
Adyen, Worldpay, Checkout.com). Specialist high-risk acquirers charge
roughly 8–12% (vs. 2–3% tier-1) and typically hold rolling reserves.
Payment orchestration and crypto rails must therefore be designed into the
architecture from the start, not bolted on later.

## Orchestration layer

No PSP SDK ever appears in business logic. An internal `PaymentProvider`
interface is implemented per adapter; an orchestrator routes on `(brand,
country, currency, method, amount)`, cascades on decline, and fails over
on provider health. Switching a declining acquirer quickly is an
operational necessity at Anjouan economics, not a nicety.

## Reserve accounting

High-risk acquirers commonly hold a rolling reserve (5–10% for ~180 days).
This must be modeled explicitly as a `psp_reserve` ledger account
(`ledger-finance`-owned) with a defined release schedule — if it sits
outside the ledger, the cash position is a fiction.

## Crypto custody (resolved — see ADR 0008)

Custody model is **decided**: an institutional/professional custody
provider abstraction, not self-custody, not private-key management inside
the core platform. Private keys, HD wallet derivation, and blockchain
signing never enter the platform's trust boundary.

```
Wallet / Ledger (platform-owned)
        │
        ▼
CryptoCustodyProvider interface (platform-owned abstraction)
        │
        ├── Custodian A adapter (e.g. Fireblocks-shaped)
        ├── Custodian B adapter (e.g. BitGo-shaped)
        └── future custodians — swappable behind the same interface
```

**Platform owns**: the player's crypto wallet/account *representation* (a
`Wallet` per crypto asset, see `06-wallet-ledger-architecture.md`), the
ledger, balances, transaction state (pending/confirmed/reversed), deposit/
withdrawal orchestration, reconciliation against custodian statements,
audit, and the custodian provider references.

**Custodian owns**: private-key custody, blockchain signing, secure key
management, and the underlying custody infrastructure.

The `CryptoCustodyProvider` interface is designed so a second or
replacement custodian can be added without a core-platform rewrite — no
single-custodian assumption is baked into the ledger or wallet model.
Regardless of custodian, the platform still handles: per-asset
confirmation thresholds, chain reorganizations, dust, memo/tag chains, and
deposits sent on the wrong network (a weekly occurrence needing a support
workflow, not just an error log).

**This architecture does not by itself satisfy every jurisdiction's
regulatory requirements for holding customer crypto assets** —
requirements vary by jurisdiction and service model and remain a
legal/compliance determination, not a software claim (see `CLAUDE.md`
compliance section).

## Withdrawals as a workflow

Not an endpoint. Required: approval thresholds, KYC gating, velocity/
pattern checks, a manual review queue, and four-eyes approval above a
configurable amount. Never auto-pay above an operator-set threshold.

## PCI scope and tokenized processing

Hosted payment pages, redirect flows, and iframe/tokenized flows are
prioritized wherever a provider supports them — our servers never see a
card PAN. Provider adapters are built against whichever of these flows
the provider offers, plus webhooks/callbacks for state updates. Touching
a PAN directly inherits PCI-DSS obligations that cost more than the
feature is worth — this is a hard boundary, not a tradeoff to weigh per
feature. **Outsourcing card handling to a hosted/tokenized flow reduces,
but does not eliminate, our PCI/security responsibility** — webhook
authenticity, provider credential security, and payment-state integrity
remain ours regardless of flow.

## Stage mapping

Stage 1 defines the `PaymentProvider` and `CryptoCustodyProvider`
interfaces and where they sit in the codebase (foundation only — no real
adapter, no real custodian contract).

**Stage 3B built the orchestration layer** (alongside the ledger): the
`PaymentProvider` interface and its adapter-conformance suite, deposit
orchestration with capability-based routing, cascade-on-decline and
ambiguous-outcome handling, the deposit/deposit-reversal callback path,
and the withdrawal approval/submission workflow (`internal/payments`,
`internal/withdrawal`, migrations `0024`–`0026`). What it deliberately did
**not** build: **no real PSP is integrated** — the only adapter is a
`MOCK` fiat adapter — and **no production provider credential storage
exists**; callbacks are tenant-bound (docs/decisions/0022 §3 as amended
2026-09-26, PAY-WH-TENANT-1). The per-tenant URL selects one (tenant,
provider, key id) credential, and the signature covers the tenant and
provider. Only a `MOCK` resolver exists. The real per-tenant credential
store is `NOT IMPLEMENTED`, and no real PSP may be connected until it
exists. Withdrawal-direction
orchestration beyond the staff-triggered submit handler, PSP settlement
reconciliation, and crypto rails are all still unbuilt and mature through
Stage 4, against the custodian abstraction decided here. See
`payment-orchestration.md` for the implemented-versus-designed breakdown.

**Stage 10.1 (PAY-REV-1, ADR 0090)** fixed a check-then-insert race in the
deposit-reversal callback path (two distinct-reference reversals of one
deposit could both post, over-debiting `player_cash`): an ADR 0082
class-L2 lock plus a database backstop index (migration 0092). See ADR
0090, financial-transaction-flows.md Flow 2 and the ADR 0020 amendment
(2026-09-26) for the fix and the general doctrine it establishes.
PAY-WH-TENANT-1 (the payments webhook's tenant resolution from a URL slug
rather than a verified per-tenant credential, ADR 0022 §3) remains open
and launch-blocking for any real PSP; it is a separate, later workstream.

## Retail cash rail — architecture (Stage 4H-B0)

Status: `NOT IMPLEMENTED` — architecture/design only, no code, no
migration, no table. Scope: the payments-domain procedural/operational
path for a retail (agent-network, cash-in/cash-out) deposit or
withdrawal, i.e. how it flows through the platform as a "payment method"
analogous to a PSP/crypto rail. This section does **not** design the
ledger postings for the retail agent network — that is
`docs/decisions/0035-retail-agent-network-accounting.md`, owned by
`ledger-finance`, produced in parallel this wave — and does **not** design
the hierarchy-level limit rules — that is an extension of
`docs/decisions/0031-risk-and-limits-engine.md`, owned by `risk`, also
produced in parallel this wave. **Neither document exists in the
repository as of this writing.** Everything below that depends on either
is written as an integration point and an explicit assumption, flagged
where it occurs, so the Orchestrator can check consistency once both land.
Same caveat for identity-compliance's retail-KYC design (§3): no such
document exists in the repo yet either.

Read alongside: `docs/decisions/0022-payment-provider-agnosticism-and-
capability-model.md`, `payment-orchestration.md`, `withdrawal-state-
machine.md`, `withdrawal-policy-configuration.md` — this section extends
those, and every place it diverges from them is called out explicitly in
§7 below rather than silently overridden.

### 1. Is retail cash a `PaymentProvider`? — `ARCHITECTURAL DECISION`: no

**Decision: retail cash is not a `PaymentProvider` implementation.** It is
a structurally distinct fulfillment channel that plugs into the
orchestration layer only at the routing-configuration level (§7), not at
the adapter level.

Justification, from the actual shape of the interface
(`payment-orchestration.md` §2), not an assumption about what "should"
work:

| `PaymentProvider` method | What it models | Why retail cash doesn't fit it |
|---|---|---|
| `Deposit`/`Withdraw` | Hand off to an external vendor that will act asynchronously and report back | There is no external vendor. The "acting party" is the platform's own staff/agent-network principal, already inside the platform's own trust boundary and RBAC system. |
| `HandleCallback` | Verify a cryptographic signature over an **untrusted external payload** before trusting any field in it (`payment-orchestration.md` §10, ADR 0022 §4.1's whole rationale for a hardened parsing boundary) | A cashier's confirmation is not an untrusted external payload — it is an authenticated, RBAC-checked, already-trusted internal staff action over the platform's own session/JWT auth, the same trust boundary every other staff-initiated state transition in this codebase already uses (e.g. `withdrawal-state-machine.md` §5's approver checks). There is no signature to verify because there is no third party asserting anything. |
| `QueryStatus` / ambiguous-outcome resolution | Resolve a timeout/5xx where the provider's payment *may* have silently succeeded, to avoid double-charging on a false "it failed" cascade (`payment-orchestration.md` §5) | There is no async settlement window to be ambiguous about. Cash either was physically handed over and a human confirmed it, or it wasn't — the confirmation *is* the terminal fact, not a signal about a fact that happened elsewhere. |
| `HealthStatus` / circuit breaker / cascade-on-decline | Route around a vendor whose success rate/latency has degraded, among interchangeable candidates for the same rail (`payment-orchestration.md` §6) | A specific physical cash handover is not "cascaded" to a different retail location the way a declined card payment cascades to a different acquirer — the player (or the flow) already fixed which physical agent is involved. An under-performing or under-floated *agent* is an agent-network operations/risk concern (§3's OPEN DECISION on reroute), not a payments-layer health score. |
| `Capabilities()` / `ProviderCapability` | Declare which currencies/countries/methods/amounts a **registered adapter** supports, gating routing | The country/currency/amount-limit *shape* is genuinely reusable (§7) — but `ProviderCapability.provider_kind` is fixed to `'fiat' \| 'crypto_payment'` precisely because a row must describe a real, registered adapter (ADR 0022 §2.1, "a capability row describes an adapter; it never promotes one") — retail cash has no adapter to describe. |

Forcing retail cash into `PaymentProvider` would mean either most of the
interface becomes a meaningless no-op (violates CLAUDE.md's "no fake
completion" in spirit — an adapter that always returns "healthy" and
never really has a decline/cascade path is not honestly implementing the
interface it claims to), or `HealthStatus`/cascade get silently
repurposed into agent-reliability/fraud scoring, which is a `risk`/
agent-network-operations decision, not a payments routing concept, and
would smuggle a real business decision in as if it were routine adapter
plumbing.

**What is structurally different, concretely**: retail cash is a
**fulfillment channel**, not a **provider**. It is dispatched to an
internal retail-confirmation flow (§2–§3) rather than to an adapter's
`Deposit`/`Withdraw`/`HandleCallback`. It never appears in
`provider_capabilities` (that table's `provider_kind` constraint must not
be widened to admit it — doing so would also incorrectly subject it to
ADR 0022 §6's mock-and-real-adapter conformance suite, §7's provider-swap
test, and the custody-boundary-preserved checks, none of which are
meaningful for it) and it is never registered as a `provider_id` in the
adapter registry.

### 2. Retail deposit flow — `ARCHITECTURAL DECISION`

**Wave-2 review correction (F3, P1)**: an earlier draft of this section
described only a player-pre-request flow (step 1 below) as if it were the
sole entry point. `ledger-finance`'s ADR 0035 §3.2 and `architect`'s doc
26 §4.1 both describe a **counter-originated** flow instead — a player
walks up with cash, with no prior app session or pending request — which
is what a retail network is for and cannot be built on the player-
pre-request flow alone (a player without an app session could never
deposit at all). **Resolution**: the counter-originated flow is
**primary**; the flow below is retained as an **optional second entry
point** for a player who prefers to initiate from their own session
before walking to the counter. Both resolve to the identical posting and
the identical `internal/ledger` idempotency key (ADR 0035 §8.1) — the
only difference is which principal creates the `RetailDepositRequest`
row and when. Concretely: in the counter-originated case, the cashier's
own confirm call (step 3, below) both creates and confirms the request in
one step (server-derived `player_account_id` resolved from whatever
identity check the counter performs, per `11-kyc-aml-rg-architecture.md`);
in the player-pre-request case, step 1 creates the row ahead of time and
step 3 only confirms it. The handler in step 3 does not need to know
which case it's in — the idempotency mechanism is the same either way.

Procedural flow (accounts/postings excluded — `ledger-finance`'s ADR
0035):

1. **Request creation (optional entry point — see above).** The player
   initiates a retail deposit (in the
   player app, or at the cashier's terminal under the player's own
   authenticated session — the specifics of that UI are `frontend`/
   `backoffice`'s territory, not this document's) for `(amount,
   asset_code, target retail location)`. The platform creates a
   `RetailDepositRequest` row (name illustrative, not a migration
   commitment this stage) — analogous in role to `deposit_intents`
   (`payment-orchestration.md` §3) but not a `DepositIntent`, since there
   is no provider being routed to — in state `awaiting_cash_handover`,
   and returns the player a reference (code/QR) to present physically to
   the cashier. `tenant_id`/`brand_id`/`player_account_id`/`wallet_id` are
   all server-derived from the player's authenticated session, exactly as
   `withdrawal-state-machine.md` §7 already requires for
   `WithdrawalRequest` — never client-asserted.
2. **Physical handover.** The player hands the cashier cash.
3. **Cashier confirmation — the API call.** The cashier's terminal calls
   an endpoint conceptually `POST /v1/retail/deposit-requests/{id}/confirm`
   authenticated as **both the terminal's own per-terminal credential and
   the cashier's own staff/agent-network principal** (both server-derived
   actors, presented together — see §6's corrected two-principal
   requirement; never a terminal-embedded *shared* credential). The
   request body carries the **amount actually received**
   (attested by the cashier) and, pending §3's identity-compliance
   dependency, an identity-verification attestation. The handler:
   a. Looks up `RetailDepositRequest` by `id` (never trusts a
      client-supplied player/amount — the platform's own stored request is
      authoritative, matching §6's "no client device is a source of
      financial truth").
   b. **Wave-2 review correction (F9, P1 — safety-critical)**: an earlier
      draft of this step ran the hierarchy/limit (`risk.Evaluate`) check
      first and had no RG call anywhere in this handler, contradicting
      the fixed gate order every sibling document states as binding
      (`docs/decisions/0035-retail-agent-network-accounting.md` §3.2,
      `docs/decisions/0036-retail-hierarchy-rbac-and-audit.md` §8.1, ADR
      0031 §22, `11-kyc-aml-rg-architecture.md` §3's own `ARCHITECTURAL
      DECISION` that a cash deposit crediting a wallet calls
      `rg.EvaluateEligibility`). The corrected order, all inside the
      posting transaction, before commit: **`rg.EvaluateEligibility`
      first (short-circuiting on any restriction) → `risk.Evaluate`
      (§4, hierarchy/funding-limit check) → the `agent_float`
      sufficiency lock.** A non-nil result from either RG or Risk is a
      denial; the handler never reaches the posting step.
   c. Compares the cashier-attested amount to the request's own amount.
      **On mismatch: the request is not silently adjusted** (mirrors
      `withdrawal-state-machine.md` §7's immutable-amount principle) — it
      is rejected back to the cashier for a fresh, correctly-amounted
      request, or escalated through the manual-adjustment path CLAUDE.md
      already requires (reason code + four-eyes above threshold). Exact
      workflow for a mismatch is an `OPEN DECISION` (§8) — the guardrail
      (no silent substitution) is not.
   d. Performs the state transition (§2's idempotency, below) and calls
      into `ledger-finance`'s wallet/ledger posting API — never a direct
      balance write, per CLAUDE.md — to record the confirmed cash-in. What
      account(s) that posts to is ADR 0035's decision, assumed here only
      to exist and to be callable synchronously from this handler, the
      same way `internal/withdrawal.LockApprovedForSubmission` calls out
      to a provider adapter today.
   e. Writes an audit record (actor = cashier's principal, entity =
      `RetailDepositRequest`, before/after state, reason code where
      applicable) per CLAUDE.md.
4. **Settlement**: conceptually `settlement_behavior = 'instant'` (reusing
   `ProviderCapability`'s existing enum value, ADR 0022 §2) — the funds are
   available in the player's wallet balance projection as soon as step 3d's
   posting commits, since there is no async provider confirmation to wait
   for. This is the retail rail's one genuine advantage over most PSP
   rails, and worth stating precisely because it is a real behavioral
   difference downstream code must not assume away.

**Idempotency — what it protects against here.** A physical cash handover
cannot be "retried" the way a PSP callback can (there is no external
system that might redeliver an identical message), so the risk this
protects against is **not** provider redelivery (`payment-orchestration.md`
§8) — it is **double-confirmation of the same handover**: a cashier's
double-click, a terminal retry after a network hiccup/page reload, or two
terminals racing on the same request. The mechanism is a state-transition
guard, not a client-supplied idempotency key:

```
UPDATE retail_deposit_requests
   SET state = 'confirmed', confirmed_by = $cashier_principal, confirmed_at = now()
 WHERE id = $1 AND state = 'awaiting_cash_handover'
-- RowsAffected() checked before the ledger posting call is ever made
```

A `RowsAffected() == 0` result means the request was already resolved
(confirmed, cancelled, or expired) — the handler returns the
already-resolved state idempotently and **does not** call the ledger
posting API a second time, mirroring the pattern
`internal/withdrawal.LockApprovedForSubmission` already established in
Stage 3B for the equivalent "only one caller may make the one call that
matters" problem, and the terminal-state guard `internal/payments`
already uses for a late/out-of-order deposit callback
(`docs/progress.md`'s Stage 3B P1 fix log). This is a different mechanism
from the ledger's own `(provider_id, provider_tx_id)` uniqueness
(ADR 0020) precisely because there is no `provider_id` here — the
uniqueness anchor is the `RetailDepositRequest` row's own state, not a
provider reference.

### 3. Retail withdrawal flow — `ARCHITECTURAL DECISION`

**Decision: reuses the existing withdrawal state machine and four-eyes
approval flow (`withdrawal-state-machine.md`, `withdrawal-policy-
configuration.md`) unchanged, adding a new fulfillment method rather than
a parallel workflow.** Justification: every state prior to `submitted`
(`requested → pending_review → approved`) is already fulfillment-method-
agnostic — the hold posts at `requested` regardless of how the payout will
ultimately reach the player (`withdrawal-state-machine.md` §1), and the
four-eyes/policy-threshold machinery (§5 of that document, and all of
`withdrawal-policy-configuration.md`) has no PSP-specific assumption
baked in. Duplicating that machinery for one more fulfillment method would
re-open every bypass path `withdrawal-state-machine.md` §5 spent Stage
3A–3D closing (self-approval, threshold manipulation, structuring, TOCTOU)
for no reason — it would simply be the same bugs, unfixed a second time.

What is genuinely new is the **`approved → submitted` transition's
mechanics**, and the terminal `submitted → completed` fact:

- **`approved → submitted`.** `payment-orchestration.md` §3 documents this
  transition today as a staff-triggered HTTP handler that calls
  `RouteProvider` then `PaymentProvider.Withdraw` directly. For a retail
  fulfillment method there is no `PaymentProvider` to route to (§1) — this
  is a **new, sibling handler variant** for this one fulfillment method,
  not a modification of the existing PSP/custodian submit handler. It
  marks the request visible in the chosen retail location's fulfillment
  queue (conceptually setting `fulfillment_method = 'cash_at_cashier'` and
  a `retail_location_id` on the request — exact schema shape is a Stage
  4H-B implementation decision, not fixed here) instead of calling
  `RouteProvider`/`Withdraw`. **Flagged explicitly so this is not read as
  silently redesigning the existing PSP/crypto `submitted` path**: that
  path is unchanged; this is an additional branch selected by fulfillment
  method, decided once at `approved → submitted` time and never
  reinterpreted later.
- **`submitted → completed`.** For the PSP/custodian path this is "provider
  confirms sent" (an async callback). For retail cash this is the same
  kind of human-attested, synchronous confirmation as the deposit side
  (§2) — the cashier confirms cash was handed to the player, via an
  endpoint conceptually `POST /v1/retail/withdrawal-requests/{id}/complete`,
  authenticated as both the terminal and the cashier's own principal
  together (§6's two-principal requirement), with the identical
  idempotency shape (a state-transition guard on `WithdrawalRequest.state
  = 'submitted'`, `RowsAffected()` checked before the release-side ledger
  call, per §2 above and `withdrawal-state-machine.md` §4's existing
  concurrency pattern for this table).

**What the cashier must see/confirm.** The terminal displays only what the
platform returns for that request id — amount, currency, and whatever
player-identifying information the platform chooses to surface (never
data the cashier's terminal computed or cached itself, per §6). Before
completing, **the cashier must confirm the player's identity** — handing
physical cash to the wrong person is a distinct risk from anything a PSP
payout flow has to solve, since a PSP's own KYC/rails already bind the
payout instrument to a verified identity, whereas a cash handover binds
nothing until a human checks it at the counter. This document requires
the `complete` API call to carry a **mandatory, non-optional
identity-verification attestation field** the handler will not proceed
without — but the actual rule for what counts as sufficient verification
(document type, liveness, threshold amounts requiring stronger checks) is
**identity-compliance's retail-KYC design, not designed here** (see this
section's preamble — that document does not exist in the repo yet;
treated as an open dependency, not invented). The same requirement is
recommended, not mandated, on the deposit side (§2 step 3) for the
equivalent reason (cash-in AML risk), left as identity-compliance's call.

Because the state machine is reused unchanged, the retail cashier's role
is structurally limited to the **fulfillment** step, never the
**approval** decision — a request only reaches `submitted` after `approved`
already ran the full four-eyes/threshold check (§5 of
`withdrawal-state-machine.md`). This mirrors the existing PSP path, where
submission is likewise distinct from approval, and needed no new design
here beyond confirming the separation holds for this fulfillment method
too.

**Wave-2 review clarification (F9)**: unlike the deposit-confirm handler
(§2 step 3b), this `complete` hand-over call is **not** required to
re-run `rg.EvaluateEligibility` before it proceeds — this is the one
legitimate exception `docs/decisions/0036-retail-hierarchy-rbac-and-
audit.md` §8.1 grants, stated here explicitly rather than left silent:
`complete` discharges an authorization (RG and Risk) that already ran at
`approved` time, on the same `WithdrawalRequest`, inside its own gated
transition. It is a fulfillment of an already-cleared decision, not a
new value-crediting event that could itself be the point a self-exclusion
first applies.

**`OPEN DECISION`** (agent-network operations, not resolved here): what
happens when the chosen agent/location cannot fulfill (float too low to
pay out the requested cash amount)? The PSP path's analogue is
cascade-on-decline (`payment-orchestration.md` §5); a naive retail
analogue ("reroute to a different location") is a real operational need
but requires the agent-hierarchy design this stage's directive explicitly
does not ask this document to build, and interacts with §4's hierarchy
limits and whatever ledger-finance's ADR 0035 says about float
accounting. Recorded as open, not invented.

**`OPEN DECISION`**: `payment-orchestration.md` §4's existing "return to
source" AML open question (a withdrawal routed back to the same
instrument/provider used for the matching deposit) has no obvious
retail-cash analogue — there is no "instrument" a cash handover returns
to. Left to `identity-compliance`/legal alongside the existing open
question, not resolved differently here.

### 4. Funding/withdrawal limits by hierarchy level — `ARCHITECTURAL DECISION` (integration point only)

**Decision: the hierarchy-level limit check happens at cashier-confirm
time** (deposit §2 step 3b, withdrawal §3's `complete` handler) — **in
addition to**, not instead of, the existing player-level limit check that
already happens at request-creation time (`requested`/
`RetailDepositRequest` creation) via the platform's existing
`internal/risk.Evaluate` integration (ADR 0031), unchanged.

Why confirm-time and not only request-creation-time: a hierarchy-level
limit (an Agent's float ceiling, a Super Agent's daily cash-out ceiling,
etc.) is a property of the **fulfilling node**, which is only definitely
known once a specific agent/cashier is the one confirming — not
necessarily at the moment the player first requested the transaction.
Checking it at confirm time, inside the same transaction as the state
transition it gates, is the same "no TOCTOU window" principle
`withdrawal-state-machine.md` §5 item 5 already applies to the four-eyes
threshold check — a check read earlier and trusted later is exploitable.

This document confirms the flow **calls** `internal/risk.Evaluate` at that
point and is **fail-closed** on it (ADR 0031 §6's existing platform-wide
contract: if `Evaluate` cannot be evaluated, the confirm/complete action is
refused, not silently allowed) — it does not design the limit rule itself.
Flagged dependencies on `risk`'s parallel-wave ADR 0031 extension:

- A `RiskRequest` shape that can carry the fulfilling agent/location's
  hierarchy identity (e.g. an Agent/Super Agent/Partner id or a hierarchy
  path), which does not exist in `RiskRequest` today (`docs/decisions/0031`
  §9/§10 added jurisdiction and licensing-mode fields the same way; a
  hierarchy-level field is the same shape of extension, not a new
  mechanism).
- New `Operation` values, following ADR 0031 §16's existing extension
  model. **Wave-2 review correction (F4, P1)**: an earlier draft of this
  bullet speculated example names (`retail_cash_deposit_confirm`/
  `retail_cash_withdrawal_fulfill`) that conflicted with names
  independently proposed elsewhere. `risk` owns `Operation` naming (ADR
  0031 §16); the authoritative set, resolved in ADR 0031 §21, is
  `retail_deposit`, `retail_withdrawal`, `retail_funding` — this
  document's deposit-confirm and withdrawal-fulfill call sites map onto
  `retail_deposit` and `retail_withdrawal` respectively.

### 5. Cashier settlement / float replenishment — `RECOMMENDATION`

**Is float replenishment (topping up an Agent's cash float from its
upstream Partner/Super Agent) itself a "payment" through this
abstraction?** No, for the same reason retail cash isn't a
`PaymentProvider` (§1): both counterparties are internal hierarchy
principals already inside the platform's trust boundary, there is nothing
to route (`RouteProvider` exists to pick the best of several competing
candidates; a float replenishment has one fixed pair of counterparties),
and there is no external vendor to adapt to. So: **not** a
`PaymentProvider`/`PaymentOrchestrator` transaction, and the actual
accounting treatment (which ledger accounts represent a float, whether a
replenishment is a transfer between two float-holding accounts) is
correctly and entirely `ledger-finance`'s call under ADR 0035, with no
payments-domain design needed for that part.

**Where payments-domain involvement is still real, as a recommendation,
not a mandate**: procedurally, a float replenishment is the *same shape*
of problem as the retail deposit confirmation in §2 — a human-attested
handover (of physical cash, or of an internal instruction) between two
now-known parties, needing an API confirm action, the identical
double-confirmation idempotency guard, and an audit trail. `RECOMMENDATION`:
reuse the §2 confirmation-flow *mechanism* (state-transition-guarded
confirm endpoint, `RowsAffected()`-checked idempotency, mandatory audit
record) for float replenishment's procedural/API side, rather than a
second bespoke human-confirmation pattern being invented independently by
whichever domain builds it — while leaving the account model, and whether
this needs its own tenant-owned request table distinct from
`RetailDepositRequest`/`WithdrawalRequest`, to be decided once ADR 0035's
accounting treatment is known. This is explicitly a recommendation for
`architect`/`ledger-finance` to accept or reject, not a claim of
ownership over float accounting.

### 6. Retail terminals/POS as API clients — `ARCHITECTURAL DECISION` (hard requirement, confirmed)

**Confirmed from the payments-domain side: a POS/cashier terminal is
never a source of financial truth.** This generalizes CLAUDE.md's "Redis
never holds an authoritative balance" principle to any client device, per
this stage's directive item #22:

- **Wave-2 review correction (F2, P1)**: an earlier draft of this bullet
  (and §2 step 3's "authenticated as the cashier's own... principal")
  required only the cashier's own principal, with no terminal credential
  at all. This contradicted `docs/architecture/26-retail-operations-
  architecture.md` §2.2 and `docs/decisions/0036-retail-hierarchy-rbac-
  and-audit.md` §4.4, both of which require **two** authenticated
  principals presented together for any money-touching retail
  operation — the terminal (a `service` principal, ADR 0014 option 2)
  and the cashier (their own `staff_users` principal) — neither alone
  sufficient. This is not the same thing as a *shared* terminal
  credential (which both this document and ADR 0036 correctly reject for
  destroying actor attribution): a **non-shared, per-terminal**
  credential presented **alongside** the cashier's own login satisfies
  both requirements simultaneously — actor attribution stays on the
  cashier, and the terminal identity is what ADR 0035 §8.2's idempotency
  key (`'retail:' || terminal_id || ':' || operation_id`) is
  **server-resolved from**, closing the namespace-squatting attack ADR
  0035 §8.2 describes (a terminal-identity claimed from request payload,
  rather than resolved from its own credential, could replay or squat
  another terminal's idempotency keys). Corrected: the terminal
  authenticates with its own per-terminal credential (registration-secret
  exchange, per ADR 0036 §4.4) **and** the cashier authenticates as their
  own staff/agent-network principal through the platform's existing auth
  system — both required, neither a shared secret standing in for
  "whichever cashier is currently at this terminal." Every retail
  confirm/complete action's audit record still names the real, individual
  staff principal as actor, exactly like every other mutating
  administrative action in this codebase — the terminal identity is
  recorded alongside it, never instead of it.
- `tenant_id`/`brand_id` are resolved server-side from that authenticated
  context, never asserted by the terminal — identical to every existing
  rule in this codebase (`withdrawal-state-machine.md` §7, CLAUDE.md's
  multi-tenancy section).
- The terminal calls platform APIs to look up a pending request (§2 step
  3a, §3), and **displays only what the platform returns** — amount,
  currency, status, player-identifying information for the identity check
  (§3). It never computes, locally caches-as-truth, or independently
  asserts a balance, an approval decision, or a "payment completed" fact.
  Any offline/cached view on the terminal is explicitly non-authoritative:
  on reconnect, a confirm/complete action must be re-validated against the
  platform's current state (the state-transition guards in §2/§3 already
  make a stale confirm attempt fail safely — `RowsAffected() == 0` — rather
  than silently re-applying a stale local view).
- The actual terminal/POS device software is `frontend`/`backoffice`'s
  territory; this document's ownership is limited to the API surface it
  calls and the guarantee that surface never trusts anything the terminal
  asserts about money.

### 7. Explicit interaction with the existing PSP/crypto payments architecture

Flagged rather than silently changed:

- `ProviderCapability.provider_kind` (ADR 0022 §2) remains
  `'fiat' | 'crypto_payment'` — **not** widened to admit a retail value.
  Retail cash is deliberately excluded from `provider_capabilities`, the
  adapter registry, and ADR 0022 §6's conformance-suite/provider-swap/
  custody-boundary-preserved test obligations (§1 above), all of which
  presuppose a real adapter.
- `payment-orchestration.md` §4's routing dimensions 1–5 (tenant/brand,
  jurisdiction/country, currency/asset, payment method, amount) are
  reused conceptually for retail configuration (which tenant/brand/
  country/currency/amount range allows cash-at-cashier at all) — but
  routing **forks after dimension 5**: for a PSP/crypto method it
  continues into dimension 6 (provider health, cascade, `RouteProvider`);
  for the retail method it terminates by handing off to the retail
  confirmation flow (§2/§3) instead. If a config table is wanted for the
  retail-specific dimensions 1–5 shape, it must be a **sibling** table
  (not a `provider_capabilities` row — §1, §7 above) mirroring the field
  shape (tenant/brand scope, currencies, countries, amount limits, status)
  without the adapter-only fields (`provider_kind`,
  `callback_capabilities`, async `settlement_behavior` values) that don't
  apply. Not designed further than this shape-level statement this stage.
- The staff-triggered `approved → submitted` withdrawal handler
  (`payment-orchestration.md` §3) is **unchanged** for its existing
  PSP/custodian path; retail cash adds a sibling handler variant for its
  own fulfillment method (§3 above), never a branch inside the existing
  one that special-cases retail.
- `withdrawal-policy-configuration.md`'s existing threshold/four-eyes/
  step-up mechanism is reused **unchanged**. `OPEN DECISION` (not resolved
  here): whether `withdrawal_policies` eventually needs a
  `fulfillment_method` scoping dimension (alongside its existing
  `asset_code`/`jurisdiction_code`/`brand_id` dimensions) so a tenant can
  set a different threshold for cash-at-cashier than for a bank transfer —
  plausible, not invented as a requirement here.
- `payment-orchestration.md` §9's `ListSettledTransactions`/PSP
  reconciliation hook does not apply to retail cash (there is no PSP
  settlement batch to reconcile against) — retail's reconciliation need
  (does the ledger's float/cash-in-hand records match what agents actually
  hold) is `ledger-finance`'s/`reconciliation-model.md`'s territory, not
  designed here.

### 8. Summary of open decisions surfaced (not resolved here)

1. Exact amount-mismatch handling at deposit confirmation (§2 step 3c) —
   reject-and-recreate vs. a formal manual-adjustment escalation path.
2. Under-floated agent / fulfillment failure and reroute at withdrawal
   completion (§3) — needs the agent-hierarchy operational design, not
   built here.
3. "Return to source" AML control's retail-cash analogue, if any (§3).
4. Whether `withdrawal_policies` needs a `fulfillment_method` dimension
   (§7).
5. Whether float replenishment gets its own request table or reuses/
   extends `RetailDepositRequest`'s shape (§5) — deferred to
   `ledger-finance`/`architect` once ADR 0035 lands.
6. The exact `RiskRequest`/`LimitKind`/`Operation` extension shape for
   hierarchy-level limits (§4) — `risk`'s design, anticipated not invented
   here.
7. The retail-KYC identity-verification rule itself (§3) —
   `identity-compliance`'s design, anticipated not invented here.
