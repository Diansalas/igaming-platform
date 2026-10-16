# Nuvei: candidate evaluation (fiat PSP)

Status: **CANDIDATE EVALUATION, INCOMPLETE: `BLOCKED` on documentation access.** Nuvei is a candidate, not mandatory
(owner directive 2026-10-11). Date 2026-10-16. Author: `payments`.

## 0. Evidence status (read before anything else)

| Item | Fact |
|---|---|
| Documentation targeted | Nuvei public developer documentation, root `https://docs.nuvei.com/` |
| Result | **Not read.** The session egress proxy returned `403` to `CONNECT docs.nuvei.com:443` (policy denial, visible in the proxy status endpoint as `connect_rejected`). A second attempt against the vendor's marketing host also failed. `WebSearch`/`WebFetch` were not available to this agent. |
| Action taken | None to route around the denial (README of the proxy: report, do not retry or bypass). No account, sign-up, credential or sandbox access was attempted. |
| Consequence | Every Nuvei-specific cell below is `UNVERIFIED (needs provider documentation/sandbox)`. **No Nuvei URL is cited as a source because none was read.** No vendor field name, endpoint, status string, header or algorithm is stated as fact in this package. |
| What IS verified | The platform side: each "Platform requires" cell cites a repository path read in this session. |
| Next step | Gate G1 (`integration-gates.md`): repeat the documentation review with working access to the host (the owner can allow the host for the session, or a person can read the docs and fill the cells). It needs no credential. |

The owner's terms (hosted/redirect page, direct API, DMN, 3DS, settlement files) are used below only as the **questions
to answer**, not as claims about what Nuvei offers.

## 1. Viability statement

**Viability is NOT ESTABLISHED.** From this session it is neither confirmed nor ruled out.

- Nothing found excludes Nuvei: no platform requirement below is, on its face, something a mainstream card/APM PSP
  cannot meet.
- Nothing found confirms Nuvei either. A candidate is only viable if its public documentation (a) supports hosted /
  redirect card capture with no PAN touching the platform, (b) gives authenticated notifications whose scheme the
  platform can register (section 3, rows W-1..W-4), (c) gives a status query that returns amount and asset, (d)
  gives a reporting source usable as a read-only, time-ordered statement (rows R-1..R-6), and (e) is available to an
  Anjouan-licensed iGaming merchant in the target markets. Items (a) to (d) are documentation questions (this
  package); (e) is a commercial/compliance question that documentation alone cannot answer (open-questions OQ-1).
- **Alternatives are not excluded.** The evaluation checklist in section 3 and the neutral mapping are written
  provider-agnostically so the same review can be run, unchanged, on any other fiat PSP or on several in parallel.
  A recommendation to run the same documentation review on 1 to 2 alternatives before committing a commercial
  relationship is in `open-questions.md` (OQ-1).

## 2. Platform fit surface (what the provider interface can carry, verified)

The platform's adapter contract, from `internal/payments/types.go` and `contract.go`:

- `PaymentProvider`: `Deposit`, `Withdraw`, `QueryStatus(providerReference)`, `HandleCallback(req, cred)`,
  `WebhookScheme()`, `Capabilities()`, `HealthStatus()`.
- Outcome vocabulary is closed: `pending | succeeded | declined | ambiguous`; `DepositResult.Outcome` is never
  `succeeded` from a deposit initiation (success only via verified callback or `QueryStatus`; sync success only when
  `OperationManifest.SyncSuccessPossible`).
- Amount is `int64` minor units + `AssetCode` from the platform asset registry. No free-form passthrough field.
- Canonical callback events: `deposit` and `deposit_reversal` (`types.go:576-577`); the receipt layer also carries the
  strings `payout` and `payout_returned` (`receipt.go:191`). A payout-returned event is NOT IMPLEMENTED as an
  adapter-to-core flow (A5 record 1g).
- Refund (platform-initiated): NOT IMPLEMENTED, registration refuses `SupportsRefund=true` (`capability.go:356`).

## 3. Per-topic evaluation

Format: **Platform requires** (verified, repository path) | **Nuvei** (all `UNVERIFIED`) | **Question for the documentation / sandbox**.
Each UNVERIFIED cell carries a registry id `N-Unn`; the registry is section 6.

### 3.1 Integration shape and PCI scope

| # | Platform requires | Nuvei | Question |
|---|---|---|---|
| P-1 | Hosted fields or redirect only; the platform never receives, stores, logs or forwards a raw PAN (CLAUDE.md Security; `internal/payoutinstrument/pan.go` refuses Luhn-valid 12-19 digit strings). A PSP integration that needs the card number to transit our servers (direct card API, server-to-server with PAN) is **out of scope, always**; there is no PCI exception to request. | UNVERIFIED N-U01: which hosted/redirect/embedded-field options exist and whether a PAN-free mode is the default or an opt-in. | Is there a hosted payment page / redirect and/or hosted-fields SDK in which the browser sends the PAN straight to the PSP? What exact merchant configuration keeps the integration at the lowest PCI SAQ level? Does the PSP's doc state the SAQ level for each option? |
| P-2 | `DepositResult` carries either `RedirectURL` or `HostedFieldToken`, at most one (`types.go:491-492`). `Interactive=true` for these (`contract.go:165`). | UNVERIFIED N-U02: what the "start payment" call returns (URL, session token, order id) and its lifetime. | What is returned at session creation, how long is it valid, can it be reused, is it single-use? |
| P-3 | Return URLs and webhooks must not be the source of truth for success; only a verified callback or `QueryStatus` is (`types.go:468-475`). | UNVERIFIED N-U03: whether the player return/redirect carries a result and whether it is signed. | Is the browser return unauthenticated (ignore it) or signed? |

### 3.2 Flows

| # | Platform requires | Nuvei | Question |
|---|---|---|---|
| F-1 Deposit | `Deposit(DepositRequest{MerchantReference, Amount, AssetCode, PaymentMethod})` -> `Pending` + `ProviderReference` + redirect/token (`types.go:452-513`). `MerchantReference` = the platform's deposit-intent/attempt id (`pa:<id>` idempotency key, ADR 0095 s4.3 T12). | UNVERIFIED N-U04. | Which call creates a payment session/order; which field carries our merchant reference; is that field returned in every notification and report line? |
| F-2 Withdrawal / payout | `Withdraw(WithdrawRequest{..., Destination})` with a decrypted, kind-validated destination from the platform's payout-instrument model (`types.go:520-533`, `payoutinstrument/destination.go`). Payout asymmetry: a payout failure is never assumed (ADR 0095 s4.5). | UNVERIFIED N-U05: whether payouts exist for the merchant's account type, to which instrument kinds (card token, bank account, e-wallet), and whether they use the same hosted/tokenised model. | Which payout methods exist per country/currency; do they require the destination to come from a prior PSP-tokenised deposit instrument; what does a payout request carry; is it asynchronous? |
| F-3 Refunds | Platform-initiated refund NOT IMPLEMENTED (ADR 0095 s5.5). A provider-initiated refund is received as `deposit_reversal` (whole-deposit only; second reversal -> `ErrDepositAlreadyReversed`, `types.go:189-198`). | UNVERIFIED N-U06: refund API existence, partial refund support, refund notification. | Can an operator-side refund in the PSP back office occur and notify us? Are partial refunds possible (platform cannot represent them, gap G-05)? |
| F-4 Chargebacks / disputes | Only a **final** debit emits `deposit_reversal` (succeeded or legacy declined carrier); a pending inquiry, representment or **won** chargeback must NOT be emitted as a reversal and maps to the "unsupported callback event type" P1 (`types.go:596-613, 661-669`). | UNVERIFIED N-U07: chargeback lifecycle states and notifications; whether a "won"/"reversed back" event exists; partial chargeback. | List every dispute-stage notification and its finality; map each to {ignore-with-audit, deposit_reversal, unsupported-P1}. |
| F-5 3DS / SCA | The platform sees none of it; it happens inside the hosted flow. Outcomes: success, decline (player-specific -> not cascadable), abandoned (stays `pending` until T16 escalation after `SettlementWindow`, ADR 0095 s4.3). | UNVERIFIED N-U08: 3DS handling in the hosted flow, liability shift flags, whether a 3DS failure is a decline code or a distinct status, timeout/abandon behaviour. | What does an abandoned 3DS challenge look like (no notification? expired status?) and after how long? |

### 3.3 Notifications (DMN / webhooks)

Platform side, verified: inbound route `POST /v1/webhooks/payments/{tenantSlug}/{providerID}`; `webhookauth.Inbound{TenantID, ProviderID, Header, Body []byte}` (`webhookauth.go:244-249`); a non-synthetic scheme **must sign a timestamp** within `MaxSkew <= 10 min` (`scheme.go:241-263`, `MaxSkewCap`, ADR 0022 s3 point 10); tenant binding must be `PerMerchantKey` or `PerMerchantKeySignedAccount`, never platform-signed (`scheme.go:100-112, 265`); `Extract` parses **headers only**, no body, no clock (`scheme.go:303-327`); constant-time MAC compare over the RAW body before any parse; a non-synthetic adapter must declare `WebhookRetrySemantics` and retry at least 429 or 503 (`webhookauth/retry_semantics.go:70-99`); a real scheme must be in `conformanceManifest` and pass `webhookauthtest.RunSchemeConformance` with a known-answer vector (`scheme.go:333-341`).

| # | Platform requires | Nuvei | Question |
|---|---|---|---|
| W-1 Signature scheme | HMAC/MAC verifiable over raw bytes with a per-tenant secret; signature and key id in a place `Extract(Inbound)` may read | UNVERIFIED N-U09: algorithm, which fields are covered, where the signature/checksum travels (header vs body fields vs query string), whether the covered field list is order-sensitive. | If the signature lives only inside a form-encoded body or query string, `Extract` (header-only by contract) cannot read it: platform gap G-01 (mapping doc). |
| W-2 Timestamp | A signed timestamp inside the MAC input (replay window <= 10 min). A scheme with **no signed timestamp cannot be registered** (`scheme.go:261`) | UNVERIFIED N-U10: whether any timestamp/nonce is covered by the signature. | If none: this is a **blocking, platform-wide** gap G-02; options in the mapping doc, all needing `security` and an ADR 0022 amendment. Not solvable inside the adapter. |
| W-3 Tenant binding | Per-tenant key (separate merchant account/secret per tenant) and optionally a signed account id equal to the credential's bound account | UNVERIFIED N-U11: is the signing secret per merchant/site, is an account/site id covered by the signature, can the notification URL be configured per merchant/site. | Per-tenant merchant accounts at the PSP (cost/commercial) or a shared account (cannot bind a tenant: refused). |
| W-4 Delivery method | Raw bytes in an HTTP request to our route; key id derivable | UNVERIFIED N-U12: HTTP method, content type (JSON vs form), character encoding, source IP ranges. | Is the notification a POST with JSON? A GET with query parameters? Does it support custom headers? |
| W-5 Retry semantics | Declared `Retries429`/`Retries503`/`HonorsRetryAfter`/`RetryWindow`; also `RedeliveryOn401`/`5xx` (ADR 0095 s6.6): if redelivery on 401 is `terminal` or `unknown`, a production-eligible statement source for the provider is mandatory | UNVERIFIED N-U13: retry schedule, retry window, response code expected to stop retries, behaviour on 4xx/5xx/timeouts, whether it retries at all. | Exact acknowledgement contract (status code/body) and retry policy. |
| W-6 Event types and ordering | Closed canonical set; unknown event -> P1 unsupported, never applied | UNVERIFIED N-U14: list of notification types (payment, refund, chargeback, payout, subscription...), per-transaction ordering guarantee (none assumed), duplicates. | Are notifications at-least-once, unordered? Does one transaction produce several notifications (auth, settle)? |
| W-7 Merchant reference echo | `CallbackEchoesMerchantReference=true` is mandatory for a production-eligible adapter (`capability.go:394`) | UNVERIFIED N-U15: our reference returned verbatim on every notification, max length/charset. | |
| W-8 Amount/asset in notification | Provider's amount and asset (minor units + currency) must be present on a success so the platform can compare (`amount_evidence.go`) | UNVERIFIED N-U16: decimal string vs integer, field names, charged vs settlement currency. | |

### 3.4 Status vocabulary and idempotency

| # | Platform requires | Nuvei | Question |
|---|---|---|---|
| S-1 Status vocabulary | Every provider status maps to exactly one of pending / succeeded / declined / ambiguous; **an unknown status is never succeeded** (sandbox request K; `M4UnknownRejectImport` / `AsPending`, `m4_source_contract.go`) | UNVERIFIED N-U17: the full transaction status and sub-status list, final vs non-final, "approved but not captured/settled". | Produce the table in `provider-neutral-mapping.md` section 3 with real values. Authorize-then-capture flows: which status means money is ours. |
| S-2 Idempotency | `IdempotentSubmission` declaration; a resubmission of the same attempt with the same key must not double-charge (T12; registration requires it for any `NotProcessed` payout mapping) | UNVERIFIED N-U18: whether the create call accepts a client idempotency key, how a duplicate client reference is answered (error vs original result), retention period. | If duplicates are rejected with an error rather than returning the original, the adapter must follow with a status query; state which. |
| S-3 Status query | `QueryStatus(providerReference)` only; success MUST carry amount and asset (`StatusQuerySuccessCarriesAmountAndAsset`); merchant-reference lookup is not in the interface (A7) | UNVERIFIED N-U19: which identifier the lookup takes (transaction id vs session token vs client order id), what it returns, rate limits. | |
| S-4 Reference semantics | One PSP-issued, stable `ProviderReference` known at `Deposit` return (Pending requires a reference) and reused unchanged in callbacks, status queries and statements; a different reference later = `provider_reference_conflict` dispute park | UNVERIFIED N-U20: whether the identifier returned at session creation is the same as the transaction id seen in notifications and reports. | A session/order token that is later superseded by a transaction id is **gap G-03** (two-reference lifecycle). |

### 3.5 Money fields

| # | Platform requires | Nuvei | Question |
|---|---|---|---|
| A-1 | Integer minor units at the asset registry's exponent; no floats (CLAUDE.md; `types.go:347-351`) | UNVERIFIED N-U21: amount representation (decimal string with decimals, integer), currency code standard, zero/three-decimal currencies. | Adapter converts using the platform's asset exponent; test with a 0-decimal and a 3-decimal currency. |
| A-2 | The echoed currency must be the **charged** currency; mismatch -> `sync_amount_mismatch` / `poll_amount_mismatch` / `callback_amount_asset_mismatch` dispute park, nothing posted | UNVERIFIED N-U22: DCC, multi-currency pricing, settlement-currency conversion and which currency each notification/report shows. | A report in settlement currency vs charged currency is gap G-06. |

### 3.6 Reporting / settlement / reconciliation

Platform side, verified (`statement.PaymentStatementSource`, `statement.go:288-323`; A11 record section 3): `Fetch(ctx, PaymentFetchRequest{TenantID, ProviderID, PeriodStart, PeriodEnd, MaxLines, ContinueFrom})` with **no tx held**; read-only credential and `ReadOnlyStatementCredential()==true`; lines `OccurredAt` = provider's **record time** of the change (never value date); provider-side time-ordered paging with tie group kept whole; `CoverageEnd` never beyond the provider's completeness horizon (settle lag); lines carry `MerchantReference` (payout lines: R-S3).

| # | Platform requires | Nuvei | Question |
|---|---|---|---|
| R-1 Source kind | A pull API or retrievable report that can be fetched per tenant by time range | UNVERIFIED N-U23: reporting API vs scheduled settlement files (SFTP/email/portal) vs dashboard export only. | If file-only: gap G-07 (file ingestion fits the `Fetch` shape only through an adapter-side file pull; "OccurredAt record time" may not exist in a daily settlement file). |
| R-2 Record time | Every line has a provider-side record/status-change timestamp that is monotonic with reporting (LF-A11-2b) | UNVERIFIED N-U24. | |
| R-3 Ordered paging | Server-side ordering by that timestamp, page size control (LF-A11-6) | UNVERIFIED N-U25. | |
| R-4 Completeness lag | A documented reporting delay so the sweep can request `PeriodEnd = now - lag` (LF-A11-2a; **per-provider settle lag is NOT IMPLEMENTED in the platform**) | UNVERIFIED N-U26. | |
| R-5 Read-only credential | A separate reporting-scope credential that cannot initiate, capture, refund or pay out (`ReadOnlyAttested`; R3-FIRST-REAL-SOURCE-READONLY-1) | UNVERIFIED N-U27: whether the PSP supports scoped keys/users. | If the only available credential is the full API secret, the source cannot satisfy the A11 rule: blocking until the vendor provides scoping or the owner/security rules otherwise. |
| R-6 Line content | kind (deposit / deposit_reversal / payout), status (pending/succeeded/declined/reversed), amount+asset, provider reference, original reference for reversals, our merchant reference | UNVERIFIED N-U28. | |

### 3.7 Environment, regions, documentation and credentials

| # | Topic | Nuvei |
|---|---|---|
| E-1 Sandbox | Existence, whether it needs a signed contract or self-service signup (this package does **not** sign up), test cards/accounts, ability to simulate decline / 3DS / chargeback / refund / delayed notification | UNVERIFIED N-U29 |
| E-2 Regions / markets | Availability and local methods for EU member states and LATAM countries, currencies, licensed entities, whether iGaming merchants in the Anjouan licence context are accepted (merchant category / acceptable-use) | UNVERIFIED N-U30 (documentation) and a commercial/compliance question the owner takes (open-questions OQ-1) |
| E-3 Rate limits / timeouts | Per-call timeouts and rate limits to set `CallTimeout`, breaker thresholds, sweeper budget (ADR 0095 s7.3) | UNVERIFIED N-U31 |
| E-4 Credentials structure | What identifies a merchant (account / site / terminal ids), which of them are secret, key rotation | UNVERIFIED N-U32; platform side: `providercred` stores ONE secret + a single optional `vendor_account_id` per handle (`providercred/outbound.go:52-61`); multi-field merchant configuration is gap G-08 |
| E-5 Egress | Host names and IP ranges for allow-listing (outbound and inbound) | UNVERIFIED N-U33 |
| E-6 Currency/method catalogue | Payment methods available per currency/country to fill `AdapterCapability` (`SupportedFiatCurrencies`, `SupportedPaymentMethods`, `SupportedCountries`, `AmountLimits` per asset) | UNVERIFIED N-U34 |

## 4. Fit / gap table against the platform's provider interface

Legend: FIT-PLATFORM = platform side already supports it (verified); the Nuvei column is always UNVERIFIED. Scope: PS = provider-specific (adapter handles), PW = platform-wide (affects any real PSP).

| # | Requirement | Platform status (verified) | Nuvei | Gap / scope | Review |
|---|---|---|---|---|---|
| 1 | Hosted/redirect deposit, no PAN | FIT-PLATFORM (`DepositResult.RedirectURL/HostedFieldToken`) | UNVERIFIED N-U01/02 | none platform-side | security (PCI boundary) |
| 2 | Signed timestamp in notification | **Required, no exception** | UNVERIFIED N-U10 | **G-02 PW, potentially blocking** | security + architect |
| 3 | Signature material readable from headers | Contract is header-only `Extract` | UNVERIFIED N-U09/12 | **G-01 PW, potentially blocking** | security + architect |
| 4 | Per-tenant key / signed account id | FIT-PLATFORM (`BindingPerMerchantKey[SignedAccount]`) | UNVERIFIED N-U11 | commercial: one PSP merchant account per tenant | security |
| 5 | Retry semantics declared | FIT-PLATFORM (`RequireRetrySemantics`) | UNVERIFIED N-U13 | adapter declaration; neither-429-nor-503 refused | security |
| 6 | Merchant reference echoed | FIT-PLATFORM, mandatory | UNVERIFIED N-U15 | adapter declaration | ledger-finance |
| 7 | Single stable provider reference | FIT-PLATFORM (conflict -> dispute park) | UNVERIFIED N-U20 | **G-03 possible PW** (session vs transaction ref) | ledger-finance |
| 8 | QueryStatus success has amount+asset | FIT-PLATFORM (manifest attestation, T16 backstop) | UNVERIFIED N-U19 | conformance test | ledger-finance |
| 9 | Merchant-reference status lookup | NOT in interface (A7) | UNVERIFIED N-U19 | PW, only if no provider reference lookup | ledger-finance |
| 10 | Idempotent resubmission | FIT-PLATFORM (`IdempotentSubmission`, T12) | UNVERIFIED N-U18 | PS | ledger-finance |
| 11 | Final-debit chargeback as `deposit_reversal` | FIT-PLATFORM (whole deposit) | UNVERIFIED N-U07 | **G-05 PW**: partial refund/chargeback, dispute stages, won chargeback | ledger-finance |
| 12 | Platform-initiated refund | NOT IMPLEMENTED | UNVERIFIED N-U06 | out of scope for first adapter | ledger-finance |
| 13 | Payout with bound destination, echo declared | FIT-PLATFORM (`DestinationEchoSemantics`, snapshot binding) | UNVERIFIED N-U05 | needs non-Synthetic payout-instrument verifier + HD-R15-1/-5, D-REG-1 (sandbox request C) | security + ledger-finance |
| 14 | Payout returned event | NOT IMPLEMENTED (A5 1g) | UNVERIFIED N-U07 | PW | ledger-finance |
| 15 | Statement source, read-only credential | Interface FIT; real source NOT IMPLEMENTED | UNVERIFIED N-U23..28 | **SR11-1, LF-A11-1/2/6** prerequisites; **G-04** credential purpose; **G-07** file-based reports; **G-06** settlement currency | security + ledger-finance |
| 16 | Multi-field merchant configuration | One secret + one `vendor_account_id` | UNVERIFIED N-U32 | **G-08 PW (small)** | security |
| 17 | Minor units / exponents | FIT-PLATFORM | UNVERIFIED N-U21 | adapter conversion tests | ledger-finance |

## 5. What to obtain from the vendor (documentation and credentials)

Documentation (read-only, no credential, no account required for the public portion): everything marked N-Un above.
From the vendor, only after the owner authorizes a sandbox (gates G4/G6, `integration-gates.md`):

1. Sandbox terms in writing; sandbox merchant identifiers; sandbox API credential; sandbox notification-signing secret;
   a read-only/reporting-scoped credential if the vendor offers one.
2. Written confirmation of: notification signature algorithm incl. timestamp coverage; retry/acknowledgement contract;
   statement/report API or file spec incl. record-time field, ordering, lag; chargeback lifecycle; supported
   methods/currencies/countries for the target brands; IP ranges.
3. Nothing production. No real merchant credential is requested by this package.

## 6. UNVERIFIED registry

N-U01 to N-U34 = **34 items**, all Nuvei-specific, all open. (Closing each needs either the public docs, the vendor's
written answer, or sandbox observation; the column "Question" above is the closure test.) Sub-claims inside a row (for
example the three items of N-U09) count once.

| Group | Ids |
|---|---|
| Integration shape / PCI | N-U01, N-U02, N-U03 |
| Flows | N-U04, N-U05, N-U06, N-U07, N-U08 |
| Notifications | N-U09, N-U10, N-U11, N-U12, N-U13, N-U14, N-U15, N-U16 |
| Status / idempotency / references | N-U17, N-U18, N-U19, N-U20 |
| Amounts | N-U21, N-U22 |
| Reporting | N-U23, N-U24, N-U25, N-U26, N-U27, N-U28 |
| Environment | N-U29, N-U30, N-U31, N-U32, N-U33, N-U34 |

Platform-side gaps that apply to **any** PSP (found while writing the evaluation; detailed in
`provider-neutral-mapping.md` section 12): G-01 header-only `Extract`, G-02 mandatory signed timestamp, G-03
two-reference lifecycle, G-04 no statement-read credential purpose, G-05 chargeback stages / partial amounts, G-06 no
settlement-currency field on statement lines, G-07 file-based statements, G-08 single-secret credential model.
