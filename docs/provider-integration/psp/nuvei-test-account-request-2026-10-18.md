# Nuvei test (sandbox) account: what is required and the request to send

Status: **PREPARED, NOT SENT.** No sign-up, no agreement, no credential exists. Everything in the "expected" column is a **hypothesis** from search summaries (documentation unreadable in this session); Nuvei's answer overrides it.

## 1. What a Nuvei sandbox integration needs (expected, to be confirmed by Nuvei)
| Item | Needed? | Note |
|---|---|---|
| Integration (test) account | **Yes** | Search summaries: Nuvei issues it on onboarding (welcome e-mail with an activation link; sandbox control panel `sandbox.nuvei.com`; REST 2.0 sandbox host `api-sandbox.nuvei.com`). No self-serve sign-up found |
| merchantId | Yes (expected) | Part of every request checksum |
| merchantSiteId | Yes (expected) | One site per tenant/brand recommended (per-tenant secret) |
| Merchant secret key | Yes (expected) | Signs requests and, per summaries, DMNs; the DMN checksum algorithm is a per-site setting: must be **SHA-256** |
| Notification (DMN/webhook) configuration | Yes | Notification URL(s) per site, event types enabled; our inbound route is `POST /v1/webhooks/payments/{tenantSlug}/{providerID}`; a **public HTTPS endpoint** is needed for delivery (staging-only, so unavailable while AWS is deferred; sandbox DMNs can be captured through a request-capture URL for conformance fixtures, or delayed until the integrated phase) |
| Payout-enabled sandbox | Yes if payouts are in scope | Account capture / payout methods enabled for the test site |
| Test report / SFTP access | Yes for reconciliation work | Scheduled Settlement/Movement/Transaction reports and a test SFTP login (read-only) |
| Test cards / APM test data | Provided in documentation | 3DS scenarios |
| Anything production | **No** | No production credential is requested or accepted |

## 2. Request to send to Nuvei (owner action; copy as is)
> Subject: Sandbox / integration account request — iGaming platform, fiat deposits and payouts
>
> We are building a multi-tenant iGaming platform (first brand under an Anjouan licence; target markets Europe and LATAM) and are evaluating Nuvei as a payment provider. We would like an **integration (test) account** to run a technical evaluation with synthetic data only (no real money, no production traffic).
>
> Please provide, for **one test merchant with two test sites** (so we can test per-tenant isolation):
> 1. merchantId, merchantSiteId(s) and the integration API credentials; the REST API generation you recommend for new integrations (REST 2.0 or the classic REST API) and its deprecation outlook.
> 2. Direct Merchant Notification (DMN / webhook) configuration: **SHA-256** checksum for payment AND payout notifications; the exact list of fields covered by each checksum, whether a timestamp is covered, retry/acknowledgement behaviour, and source IP ranges.
> 3. Sandbox support for: hosted/redirect card deposits with 3DS; local payment methods relevant to EU and LATAM (Pix, SEPA, others you recommend); refunds (full and partial); chargeback/dispute simulation; **payouts** (to cards and bank accounts) with the account-capture / UPO flow and any alternative that accepts bank details directly; payout status lookup.
> 4. A server-side **transaction status query** by transaction id and by our own client reference, returning status, amount and currency.
> 5. Reporting for reconciliation: report types, programmatic API availability, SFTP delivery, record-time field, ordering, delivery lag, and a **read-only** credential for report retrieval.
> 6. Confirmation of whether merchants operating under an Anjouan gaming licence are eligible, the markets/currencies/methods available to such merchants, and what additional documentation your onboarding requires.
> 7. Written sandbox terms. We do not intend to enter any commercial agreement at this stage.

## 3. What I will do with the result
Store credentials only as `providercred` handles (never in source/docs/chat), run the A..Z verification against the official pages, then the conformance suite (`conformance-plan.md`) against the sandbox.
