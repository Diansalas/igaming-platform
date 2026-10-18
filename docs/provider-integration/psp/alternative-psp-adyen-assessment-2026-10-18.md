# Alternative PSP assessment: Adyen (PSP phase 1B, 2026-10-18)

Status: **SELECTION + VERIFICATION PLAN ONLY. Nothing is verified; the documentation host is blocked.** No adapter, no sign-up, no credential, no agreement.

## 1. Why Adyen was chosen as the one alternative
Chosen by the assistant on the criteria in the brief. Evidence labels: `SEARCH` = official page summarised by web search this session; `MODEL KNOWLEDGE` = recalled, **not read in this session, treat as a hypothesis**.
| Criterion | Adyen | Basis |
|---|---|---|
| Public documentation quality | Very good, fully public (API explorer, per-flow guides, OpenAPI specs) | MODEL KNOWLEDGE; pages for HMAC and idempotency came back in search |
| Sandbox accessibility | Self-service free *test* account, no commercial agreement (the owner must do the sign-up; I will not) | MODEL KNOWLEDGE |
| iGaming suitability | Accepts gambling merchants **only after approval/licence checks**, market by market | MODEL KNOWLEDGE (not established by search; commercial) |
| Europe / LATAM | Strong Europe (cards, SEPA, iDEAL, Bancontact...); LATAM narrower (Brazil incl. Pix, Mexico) | MODEL KNOWLEDGE |
| Hosted / tokenised instruments | Drop-in/Components/Sessions hosted fields, stored payment methods (tokens) | MODEL KNOWLEDGE |
| Webhook security | HMAC-SHA256, one key per webhook endpoint, key rotation supported, signature in the body (`additionalData.hmacSignature`) for Standard webhooks, header for some families; computed over a fixed field list | SEARCH (HMAC page) + MODEL KNOWLEDGE for the field list |
| Idempotency | `Idempotency-Key` header on POST, max 64 chars, replay returns the first response | SEARCH (official idempotency page) |
| Status query | No general payment-status GET by merchant reference in the Checkout API (results come by webhook; `/payments/details`, `/sessions/{id}`); a Reports API exists | MODEL KNOWLEDGE, **a kill test for us** |
| Refunds / chargebacks | Refund API (full/partial), dispute events and a Disputes API | MODEL KNOWLEDGE |
| Reconciliation | Settlement/payments accounting reports via API or SFTP with a report-only user | MODEL KNOWLEDGE |
Alternatives considered and not chosen (not assessed): Checkout.com (clean docs, `Cko-Signature` over the raw body, no signed timestamp, gambling by approval), dLocal / EBANX (strong LATAM iGaming payouts, weaker Europe, sandbox via sales), Paysafe / Trustly / Praxis (iGaming-native but docs largely gated behind merchant onboarding). The choice is revisable once documentation access exists; it is a *plan to verify*, not a recommendation to contract.

## 2. Verification matrix (same shape as Nuvei; every row `NOT VERIFIED`)
Pages to read: `docs.adyen.com` (host not yet allowed). Rule vocabulary as in `nuvei-verification-matrix-2026-10-18.md`.
| Topic (A..Z) | What the page must show | Verdict rule |
|---|---|---|
| Payment creation (A) | Sessions/Drop-in: server creates a session, shopper pays in hosted component, no PAN on our servers | PAN on our servers -> INCOMPATIBLE |
| Payment status (B) | A server call returning result for our `reference`/pspReference with amount+currency | None -> success only via webhook; then N-1-style confirm-by-query impossible -> need the binding to come entirely from the signed fields |
| Webhooks (C), signature (D), covered fields (F) | HMAC fields include `merchantReference`, amount, currency, `eventCode`, `success`, `pspReference` (hypothesis) | Reference signed -> N-1 resolved by the signature itself |
| Signed timestamp (E) | Whether any time is covered | If none -> G-02 reviewed replay control needed: dedupe on (pspReference, eventCode, success) is durable; freshness cannot be enforced -> **PLATFORM CHANGE (architect+security)**; INCOMPATIBLE only if the owner/security rule "signed timestamp mandatory" stays absolute |
| Binding (G), idempotency (H), duplicates (I), order (J) | Documented retry-until-ack at-least-once; no ordering | Existing receipt idempotency |
| Refund/partial (K, L), chargebacks (M) | Refund API, dispute events | Partial/stages park as unsupported (G-05) |
| Payout creation/status (N, O), signature (P) | Payout API prerequisites (stored payout details, approval, review/confirm step), payout webhooks | Needs read |
| Destination/token model (Q), hosted capture (R) | Stored payment method / recurring detail tokens bound to shopperReference | Token bound to player at capture + immutability -> same G-10 design question as Nuvei |
| Credentials (S) | API key, merchant account, webhook HMAC key, report user | Multi-field handle (G-08) |
| Reporting (T, U) | Reports API / SFTP, record-time and ordering, report-only user | Read-only credential naturally separable (G-04 helped) |
| Currencies (V), sandbox (W), test credentials (X), errors (Y), versions (Z) | Per-currency exponents; test account self-service; refusal reasons; API versioning in URL (v68-style) | Read |

## 3. Provisional view (hypotheses only)
Strengths if confirmed: signed `merchantReference` (resolves N-1 without a pull), documented idempotency header, per-endpoint HMAC keys, token model, report-only credentials. Risks to disprove: no signed timestamp (G-02), no status-by-reference query, gambling eligibility and payout availability for the target markets, LATAM coverage.
