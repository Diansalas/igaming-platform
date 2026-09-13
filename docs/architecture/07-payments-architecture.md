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
exists**; the callback path is authenticated by the mock adapter's own
signature check, and the tenant comes from a per-tenant webhook URL slug,
not from a stored per-tenant credential. Withdrawal-direction
orchestration beyond the staff-triggered submit handler, PSP settlement
reconciliation, and crypto rails are all still unbuilt and mature through
Stage 4, against the custodian abstraction decided here. See
`payment-orchestration.md` for the implemented-versus-designed breakdown.
