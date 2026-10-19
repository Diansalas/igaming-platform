> **SUPERSEDED by `nuvei-verified-matrix.md` (documentation read).** Kept for the verification rules.

# Nuvei documentation verification matrix (PSP phase 1B, 2026-10-18)

Status: **READY TO EXECUTE, NOT EXECUTED: the official documentation could not be read.** Nothing below is `VERIFIED`.
Frozen platform: `1505a165e9152729b5df573211c80b1c0a35bc87`. No adapter code, no platform change.

## 0. Access result (exact)
The owner authorised `docs.nuvei.com` and `docs-apm.nuvei.com`. The environment's network policy still denies both: the egress proxy answers `403` to `CONNECT docs.nuvei.com:443` and `CONNECT docs-apm.nuvei.com:443` (proxy status endpoint: `connect_rejected`, "policy denial or upstream failure"), `curl` and `WebFetch` both fail. The session cannot change the policy; it is not routed around. **The owner must change it (section 5 of the PSP gate message; steps below).** Every other candidate documentation host tried (Adyen, Checkout.com, dLocal, EBANX, Stripe, Paysafe, Trustly, Worldpay, Mollie, TrueLayer, Mastercard developers) is denied the same way, so the alternative PSP's documentation also needs an allowed-domain entry.

How to change it: in the Claude app, open the cloud environment menu in the session's title bar -> **Edit** -> **Network access**: either set a broader access level, or keep *Limited* and add the hosts under **Allowed domains** (leave "Allow package managers" ticked). Hosts: `docs.nuvei.com`, `docs-apm.nuvei.com`, and for the alternative PSP `docs.adyen.com` (see `alternative-psp-adyen-assessment.md`). Reference: https://code.claude.com/docs/en/cloud-environments#network-access . Then start a new turn; the verification below needs no credential.

## 1. Rules for the verification
- Read the **page itself** (not a search snippet) and record the exact URL and the exact field/endpoint names. Pointers below are URLs that web search returned for the topic; they are *where to look*, not evidence.
- Status vocabulary: `VERIFIED` (read on the official page) / `NOT VERIFIED`. Verdict vocabulary: `COMPATIBLE` / `PLATFORM CHANGE` (needs a reviewed, provider-neutral change) / `INCOMPATIBLE` (cannot be met without weakening security or financial controls).
- A hypothesis column exists only to say what the reading must confirm or refute; it is the earlier search-summary content and is **not** a finding.

## 2. Matrix (A..Z). Every row: official page | status | verdict rule | security | financial/ledger | required adapter behaviour
| Id | Topic | Page(s) to read (pointer) | Status | Verdict rule (what the page must show) | Security implication | Financial / ledger implication | Required adapter behaviour if confirmed |
|---|---|---|---|---|---|---|---|
| A | Payment creation | `/documentation/integration/...` payment flow; `/getSessionToken`, `/openOrder`, `/payment`; REST 2.0 `/payments` (https://docs.nuvei.com/api/v2/main/docs/introduction) | NOT VERIFIED | Server-side create returning a redirect URL/session usable with no PAN on our servers -> COMPATIBLE; PAN must transit our servers -> INCOMPATIBLE (CLAUDE.md) | PCI scope stays out of our infra | Create is never success; Pending + reference | `Deposit` returns Pending + `ProviderReference` + `RedirectURL`/`HostedFieldToken`; `clientRequestId` = attempt idempotency key |
| B | Payment status | status/lookup methods (`getPaymentStatus`/transaction details) pointer: https://docs.nuvei.com/documentation/integration/... | NOT VERIFIED | A server call by transaction id **and/or** our client reference returning status, amount, currency, our reference -> COMPATIBLE; none -> N-1 blocker | The DMN must be confirmable by an authenticated pull | `QueryStatus` success must carry amount+asset | `QueryStatus` calls it; amount/asset mismatch -> existing dispute park |
| C | Webhooks/DMNs | https://docs.nuvei.com/documentation/integration/webhooks/payment-dmns/ ; REST 2.0 webhooks page | NOT VERIFIED | Event families, HTTP method, content type, encoding, source IPs, retry/ack contract | Delivery shape defines Extract (G-01) | Duplicates and second final DMN expected | Closed event map; unknown -> unsupported P1 |
| D | Signature/checksum | same page | NOT VERIFIED | Exact algorithm(s), key, where it travels | MD5 must be refusable (N-2) | - | Pin SHA-256; refuse MD5 at registration |
| E | Signed timestamp | same page | NOT VERIFIED | A timestamp inside the covered input -> COMPATIBLE with `scheme.go:261`; none -> a reviewed replay control is required (G-02) | Replay window <= 10 min | - | Platform skew check on the signed timestamp |
| F | Covered fields (exact list, order, encoding of `+`/spaces) | same page | NOT VERIFIED | Needs the full list | Anything uncovered is attacker-controlled (N-1) | - | MAC recomputed over the vendor's rule only |
| G | Merchant/order/reference binding | same page + create response | NOT VERIFIED | Our reference covered by the signature, **or** confirmable by B (positive binding) -> COMPATIBLE; neither -> INCOMPATIBLE for card DMN-driven success | Misattribution risk (N-1) | Wrong-intent credit = double credit class | Bind on signed ids; confirm via B before any financial effect |
| H | Idempotency | `clientRequestId`/duplicate rules | NOT VERIFIED | Duplicate create with same key returns the original, no second charge -> COMPATIBLE | - | No double charge on retry (T12) | Declare `IdempotentSubmission` only if verified |
| I | Duplicate callbacks | DMN page | NOT VERIFIED | Documented at-least-once / second DMN | Idempotent apply | One financial effect per (provider, tx id, status) | Existing receipt idempotency |
| J | Out-of-order callbacks | DMN page | NOT VERIFIED | Ordering guarantee or none | State machine monotonic | Late success after terminal decline -> existing rules | Never regress a terminal state |
| K | Reversal/refund | https://docs.nuvei.com/documentation/features/financial-operations/ ; REST 2.0 refund | NOT VERIFIED | Refund/void methods; async pending + DMN | Operator-initiated PSP-side actions | Platform-initiated refund is NOT IMPLEMENTED (ADR 0095 s5.5) | Map only provider-initiated final reversals to `deposit_reversal` |
| L | Partial refunds | refund page | NOT VERIFIED | Partial amounts exist -> PLATFORM CHANGE (G-05) | - | Whole-deposit reversal only today | Partial -> unsupported P1 until reviewed design |
| M | Chargebacks | DMN control-panel page (dispute/pre-chargeback types) | NOT VERIFIED | Final-debit event identifiable; stages/won distinguishable | - | Only a **final debit** reverses (types.go) | Map final debit; park others |
| N | Payout creation | https://docs.nuvei.com/documentation/features/financial-operations/payout/ | NOT VERIFIED | Request fields, destination input (G-10) | Destination integrity | Payout asymmetry (never assume failure) | `Withdraw` with bound destination; ambiguous on timeout |
| O | Payout status | payout page + DMN | NOT VERIFIED | Status list, query by reference | - | Payout failure never assumed (ADR 0095 s4.5) | `QueryStatus` for payouts; closed mapping |
| P | Payout notification signature | payout DMN page | NOT VERIFIED | Same or different scheme (G-09) | Two schemes per provider | - | One scheme per event family or separate provider id |
| Q | Payout destination/token model | account-capture page https://docs.nuvei.com/documentation/general/account-details-capture/ | NOT VERIFIED | Vaulted UPO vs raw account details (G-10) | Substitution of destination | Destination binding / echo | New destination kind or raw-detail method; never weaken integrity |
| R | Hosted capture / instrument creation | account capture, Web SDK/Simply Connect | NOT VERIFIED | No PAN on our side | PCI | - | Capture DMN verified and bound to player+brand |
| S | Credential model | integration settings (control panel) | NOT VERIFIED | merchantId, merchantSiteId, secret, others (G-08) | Secret handling | - | Multi-field `providercred` handle |
| T | Reporting/statement | https://docs.nuvei.com/documentation/control-panel-docs/finance/... | NOT VERIFIED | Pull API vs files (G-07); record time; ordering; lag | Read-only credential (A11/G-04) | Reconciliation source of truth | Statement source per A11 |
| U | SFTP/file reporting | settlement/Movement report pages | NOT VERIFIED | File format, schedule, SFTP access | SFTP credential separable | Settlement currency (G-06) | File-ingestion adapter, read-only |
| V | Currencies | supported currencies/methods | NOT VERIFIED | Per brand fiat list; exponents | - | Exponent per asset registry | `AdapterCapability` filled from docs |
| W | Sandbox capabilities | testing pages (cards, APMs, Postman) | NOT VERIFIED | What can be simulated (decline, 3DS, refund, delayed DMN, payout) | - | - | Conformance scope |
| X | Test credential requirements | onboarding/sandbox pages | NOT VERIFIED | How an integration account is obtained | Credential custody | - | See `nuvei-test-account-request-2026-10-18.md` |
| Y | Error/status mappings | status/errCode references | NOT VERIFIED | Full list, final vs non-final | Unknown status never succeeded | `M4UnknownRejectImport` rule | Closed mapping table |
| Z | API version/deprecation | REST 2.0 vs legacy REST pages | NOT VERIFIED | Which version carries payouts, account capture, signed webhook; deprecation notices | Target one version | - | Pin one API generation |

## 3. The nine findings: resolution rule (applied when the pages are read)
| Finding | Resolved by | Compatible when | Else |
|---|---|---|---|
| G-01 checksum in body vs header-only `Extract` | rows C, D, F | Reviewed provider-neutral extension (security + architect): a scheme may read the signature from a parsed body field of the same raw bytes; duplicate keys rejected; canonical form per vendor rule | PLATFORM CHANGE; INCOMPATIBLE only if no deterministic canonical form |
| N-1 merchant reference outside the checksum | rows B, F, G | Reference signed **or** status query positively binds signed transaction id -> our reference + amount + currency before any effect | INCOMPATIBLE for DMN-driven success. No weaker workaround accepted |
| N-2 MD5 vs SHA-256 | row D | SHA-256 selectable per site and verifiable | Refuse registration; RED if SHA-256 unavailable |
| G-03 session/order id vs transaction id | rows A, B, C | Stable reference lifecycle documented; platform stores order/session id as merchant reference and binds transaction id at first verified event (ledger-finance review) | PLATFORM CHANGE |
| G-09 payout signing scheme | row P | Documented; adapter registers per-family scheme or separate provider id | PLATFORM CHANGE (small) |
| G-10 payout destination/token | rows N, Q, R | Destination bound at capture to player+brand+attempt, immutable, echo declared, four-eyes unchanged | INCOMPATIBLE if destination cannot be bound/verified without weakening integrity |
| G-07 file-based reconciliation | rows T, U | Time-ordered, complete-by-lag, read-only retrievable files | PLATFORM CHANGE (file-ingestion adapter) |
| G-05 refunds/chargebacks | rows K, L, M | Final debit identifiable | Partial/staged events park as unsupported (P1) |
| G-08 credential fields | row S | Fields enumerated | PLATFORM CHANGE (small) |
