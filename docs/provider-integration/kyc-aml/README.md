# KYC / AML provider-integration package (KYC document verification, sanctions/PEP screening, geolocation if selected)

Status: **DOCUMENT / SPECIFICATION PACKAGE ONLY.** No adapter, no vendor call, no credential, no account, no agreement.
Prepared 2026-10-16 by `integrations` on branch `provider-packages-rest` (base: `claude/focused-wright-jw88w9` at `4a3c85f`).
Provider-integration phase item 3 of 5 (PSP -> casino -> **KYC/AML** -> sportsbook -> sports data; see `../README.md`).
Same structure and rigour as `../psp/`.

Labels (CLAUDE.md): everything in this directory is `NOT IMPLEMENTED` as software. No KYC, screening or geolocation vendor
is selected and no vendor documentation is available (owner statement 2026-10-16). **This package asserts no vendor
behaviour.** Vendor-side statements are requirements the platform places on a vendor ("must supply") or are marked
`UNVERIFIED (needs provider documentation/sandbox)` with an id `U-KYC-nn`. The platform side is verified against code at
base `4a3c85f`, with paths cited.

Software capability is not legal approval. Nothing here says that any verification flow, screening list coverage,
retention period, SAR format or geolocation method satisfies any regulator (CLAUDE.md "Compliance"). All thresholds,
retention periods, list choices and filing formats are legal configuration supplied by compliance; none is invented here.

## Index

| File | Content |
|---|---|
| `provider-neutral-mapping.md` | Platform concept -> what the vendor must supply -> how the neutral interface carries it -> gap/UNVERIFIED, for KYC verification, screening, AML officer flows, geolocation, enforcement, data protection. Platform-gap candidates `KYC-G01..` (section 11), UNVERIFIED registry `U-KYC-nn` (section 12). |
| `conformance-plan.md` | Existing KYC, webhook-scheme, screening and geolocation suites a real adapter must run unchanged, what must change to make them runnable for a non-mock adapter, new cases `KYC-C-nn`. |
| `integration-gates.md` | Gates K0..K9 (documentation review -> adapter -> conformance -> security -> ledger-finance (where it applies) -> sandbox -> operational acceptance -> pre-production -> explicit production authorization), entry criteria, evidence, exact owner/vendor inputs, and the privacy gate for identity attributes. |
| `open-questions.md` | The minimum decisions/inputs `OQ-KYC-nn`, each with a recommendation. |

## Three sub-areas, three interfaces

| Sub-area | Neutral interface (verified) | State |
|---|---|---|
| KYC document verification | `kyc.KYCProvider` (`internal/kyc/provider.go`): `CreateVerification`, `GetVerification`, `SubmitVerification`, `HandleCallback`, `WebhookScheme`, `GetCapabilities`, `HealthStatus`; MOCK `MockKYCProvider`; two-phase outbox worker (`outbox_worker.go`) | `IMPLEMENTED` (MOCK). Real vendor `PROVIDER DEPENDENT`; production self-service KYC returns 503 without a configured provider |
| Sanctions / PEP screening | `aml.ScreeningProvider` (`internal/aml/screening.go`): `ID`, `Capabilities`, `Screen`; MOCK `MockScreeningProvider`; fail-closed `Decide`; officer-initiated orchestrator; conformance `internal/aml/screeningtest` | Interface + decision engine `IMPLEMENTED`; provider `MOCK`; automatic triggers, gate wiring, outbox/worker, monitoring (S4) and SAR (S5) `NOT IMPLEMENTED` |
| Geolocation (only if a vendor is selected) | `geolocation.LocationProvider` / `geolocation.Locator` (`internal/geolocation`), consumed only by `internal/marketgate` | Interface `IMPLEMENTED`; only `StaticLocator` (Synthetic) wired. Owner decision 2026-10-16: market evidence = KYC-verified residence now, vendor geolocation later, no invented provider |

## Headline gaps (detail in `provider-neutral-mapping.md` section 11)

1. `KYC-G01` The interface cannot carry a hosted/redirect session: `ProviderResult` has only `ProviderReference`,
   `Outcome`, `Reason`; documents are uploaded to the platform's own `DocumentStorageProvider` (mock only; no real
   encrypted store, no real malware scanner) and the vendor is handed document ids. This is the opposite of the
   "hosted/redirect only, no PII through our infrastructure" posture the owner asked this package to assume. Open item
   KYC-HOSTED-SESSION-1.
2. `KYC-G02` A vendor cannot return a verified residence country, document expiry or level; `verified_residence_country`
   is set only by a staff `ReviewVerification`. The owner decision makes it the market evidence source.
3. `KYC-G03` Terminal verification states are final for the provider path: a later vendor revocation or expiry of an
   approved verification is a no-op (same rank). Post-approval vendor changes are unrepresentable.
4. `KYC-G06` / `X-G01` There is no credential seam for screening or geolocation: `providercred` domains are
   `payments`, `kyc`, `casino` only (migration 0096 CHECK), and `ScreeningRequest`/`SignalRequest` carry no `CallContext`.
5. `KYC-G07` Screening is request/response only, officer-initiated, input limited to a pseudonymous reference (identity
   attributes BLOCKED on the Q6 privacy review), with no async result, no ongoing monitoring and no gate that requires a
   completed screening.
6. `KYC-G08` A potential sanctions match places an all-value-movement hold that a single officer (critical step-up) can
   lift; only a CONFIRMED sanctions match needs four-eyes. A compliance decision before a real vendor produces
   false positives.

## Principles applied

- Providers adapt to the neutral interfaces; the core is not redesigned around the first vendor. Gaps are numbered with
  scope, reviewer and a minimal proposal, never a redesign. No core change is made by this package.
- **Enforcement is our code and fails closed.** The vendor reports; the platform blocks. Unknown, unavailable, error and
  timeout are never a pass (KYC `unavailable`, screening `error`, geolocation non-`resolved`).
- Raw vendor payloads are never persisted or logged; reasons are bounded and normalized (KYC-REASON-BOUND-1); player-visible
  surfaces show a closed status only (HD-10.3-3); KYC pending and AML hold are indistinguishable to the player and tenant
  (ADR 0116 s5.1, no tipping-off).
- No raw PAN ever (KYC does not need one). No identity attribute leaves the platform to a vendor until the Q6 privacy
  review allows it.
- Sandbox only until explicit production authorization; synthetic tenant and synthetic identities only; no real customer
  data, no real identity document.
