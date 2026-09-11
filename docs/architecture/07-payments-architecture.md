# 07 — Payments Architecture Proposal

Status: Stage 0 proposal. Source: Blueprint §4.6, §1 ("Payments are the
hard part, from day one").

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

## Crypto

Custody model (self-custody with HD wallets/hot-cold split, vs. a
custodian like Fireblocks/BitGo) is an open business decision (Blueprint
§10 Q4) — tracked in `docs/decisions/0005-open-business-decisions.md`.
Either way, the platform must handle: per-asset confirmation thresholds,
chain reorganizations, dust, memo/tag chains, and deposits sent on the
wrong network (a weekly occurrence needing a support workflow, not just an
error log).

## Withdrawals as a workflow

Not an endpoint. Required: approval thresholds, KYC gating, velocity/
pattern checks, a manual review queue, and four-eyes approval above a
configurable amount. Never auto-pay above an operator-set threshold.

## PCI scope

Hosted fields or redirect only. Our servers never see a card PAN. Touching
a PAN inherits PCI-DSS obligations that cost more than the feature is
worth — this is a hard boundary, not a tradeoff to weigh per feature.

## Stage mapping

One PSP adapter + orchestration skeleton is Stage 3 (alongside the
ledger). Full withdrawal-approval workflow and crypto rails mature through
Stage 4. Custody decision (Q4) should be made before crypto rail work
begins in earnest.
