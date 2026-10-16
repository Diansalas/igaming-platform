# KYC / AML integration gates

Status: **PROCESS SPECIFICATION.** Nothing here is started or authorised beyond reading public documentation once a vendor
is named. The gates apply per vendor and per vendor type: a KYC document vendor, a screening vendor and a geolocation
vendor each pass K0..K9 separately (one vendor offering all three still needs the evidence per type).

> **No real customer data, no real identity document, no real screening subject, no production credential and no
> production traffic reach a vendor until the owner gives EXPLICIT written production authorization (gate K9).** A sandbox
> authorization (K6) is not a production authorization. Technical readiness is separate from legal/licensing approval and
> from per-brand activation (decision register 2026-10-16): passing a gate says nothing about a regulator's acceptance of
> the verification method, the screening lists, retention periods, SAR format or geolocation method, or about a vendor
> agreement or DPA.

A gate is passed only when its evidence is filed; a later gate cannot be entered while an earlier one is open. The
orchestrator is the single writer of status documents; specialists produce the evidence.

| Gate | Name | Owner / specialist | Entry criteria | Exit evidence |
|---|---|---|---|---|
| K0 | Platform prerequisites | orchestrator, `identity-compliance`, `security`, `architect` | Owner has named the vendor(s) for evaluation and for which type(s). Staging validation done. Binding rows of `conformance-plan.md` s2 re-verified at HEAD, including the state of the market-evidence branch | `KYC-G01..G10` triaged by `architect`/`security`/`identity-compliance` into {not needed, needed, needs ADR} per chosen vendor type; OQ-KYC-02 (hosted vs platform-held documents) decided; X-G01 (credential domains) decision scheduled |
| K1 | Provider documentation review | `identity-compliance` (with `integrations`) | Vendor documentation available to the session (no account, no sign-up, no credential, no real data). **No documentation exists today** | The 23 `U-KYC-nn` items re-run: each closed with a citation (URL + retrieval date) or a vendor written answer, or still UNVERIFIED with the reason; mapping tables updated; viability verdict per type; go/no-go (owner). Settle first: U-KYC-03/04 (hosted vs platform-held), U-KYC-11 (callback scheme registrable under ADR 0022 s3), U-KYC-06/07 (reference model, post-approval events), U-KYC-15 (screening input without attributes), U-KYC-18 (monitoring) |
| K2 | Adapter build | `identity-compliance` | K1 go; K0 triage has no unresolved blocking gap (KYC-G01 and KYC-G06 decided; G02/G03/G07 as applicable); owner authorized adapter work **against recorded fixtures only** | Adapter behind `kyc.KYCProvider` / `aml.ScreeningProvider` / `geolocation.LocationProvider` in its own package; no vendor type outside it; reuses `internal/providers/httpclient` and `internal/providers/config`; `WebhookScheme` + retry semantics declared; reasons through `NormalizeReason`; bundle/registration changes in `cmd/platform-api/registrations.go`; labelled `PROVIDER DEPENDENT` / fixture-backed until K6 |
| K3 | Conformance | `identity-compliance`, `qa` | K2 | `conformance-plan.md` s1 to s3 pass against recorded fixtures for the applicable types; known-answer vector for the callback scheme; scheme added to `conformanceManifest`; evidence per s4 |
| K4 | Security review | `security` | K3 | Written review of: credential storage and the handles/purposes (X-G01 outcome), callback scheme, egress allow-list and SSRF (including any redirect/hosted URL handling), log and audit redaction (no PII, no raw payload, no document content), the hosted-session token lifecycle (KYC-G01), request minimality to the screening vendor (KYC-C-31), pseudonymous subject key handling, geolocation IP handling, tipping-off properties (hold indistinguishable from pending), the PROV-OUTBOUND-CRED-1 tripwire relaxation scoped to this adapter, document storage and scanner if platform-held |
| K5 | Compliance and ledger review | `identity-compliance`, `ledger-finance` (for any gate that touches deposit, withdrawal or payout), `risk` | K3 | Written review of: outcome mapping (KYC-C-04), enforcement behaviour under vendor outage (fail-closed, no manufactured `failed`), hold classes and lift rules (KYC-G08 decision), gate predicates and triggers if built (KYC-G07), policy authoring inputs received from compliance (thresholds are not invented), retention and erasure interplay with AML legal holds |
| K6 | Sandbox | `identity-compliance` + `qa`, owner supplies credentials | K4 and K5 approved (or approved with conditions closed); **owner written sandbox authorization** naming vendor, type and scope; vendor sandbox terms in writing; credentials provisioned by the owner through the secret store (never in chat, repo, docs); **synthetic tenant and synthetic identities only**; tripwire relaxation authorized for this adapter only | Sandbox run log: per-scenario results (approve, reject, pending, review, expiry, abandoned session, callback redelivery and out-of-order, vendor outage, ambiguous create, unknown outcome, reason hostile text; screening clear / potential sanctions / PEP / confirmed / unavailable with vendor test subjects; geolocation resolved / inconclusive / unavailable), enforcement and hold behaviour at each deposit / withdrawal / play gate, no real customer data and no real document, sandbox captures filed as the vendor fixtures, a stop drill (disable provider, in-flight verifications, credential revocation) |
| K7 | Operational acceptance | `devops`, `identity-compliance`, orchestrator | K6 passed | Runbooks: stuck outbox rows, `failed_terminal` handling, vendor outage and enforcement posture, credential and webhook secret rotation, officer procedures for holds/cases (quorum in place, `R-AML-QUORUM`), screening retry backlog, vendor contact/escalation; alert delivery to owner-supplied recipients with a delivery test; monitoring per tenant; egress and inbound allow-lists applied in staging |
| K8 | Pre-production readiness | orchestrator, all reviewers | K7; the "before production" list of the readiness report closed or accepted in writing (ALERT-DELIVERY-1 and others as listed); **Q6 privacy review closed before any identity attribute is sent**; DPA, sub-processor list, data region and retention values received from the owner/compliance; D-AML-TIP-1 legal wording approved; `R-AML-SCREENING` and `R-AML-MONITORING` readiness items understood (monitoring is a standing failing item until S4 exists) | A readiness report update for this vendor; open-rows list with owners |
| K9 | **Explicit production authorization** | **owner only** | K8; vendor contract and DPA signed by the owner/authorised signatory (not by any agent); legal/compliance confirmation owned by the owner and compliance per brand and jurisdiction; per-brand activation decision; production credentials provisioned by the owner into the secret store | Owner's written authorization naming vendor, type, tenants/brands, jurisdictions, date; until then the production registry refuses the adapter (`MarkProductionEligible` not set; startup guards refuse; a Synthetic component never runs in production). **No agent may sign, accept an agreement or DPA, create an account or supply a credential** |

## Gate K1 checklist (what the documentation review must answer)

Per type, in priority order for a go/no-go.

- KYC: how the player's documents are collected (hosted/redirect vs upload), what data the vendor needs at create time,
  the status model and its mapping (including retryable rejection and post-approval changes), the callback scheme and
  redelivery, the reference model (applicant vs check), levels, validity and expiry (U-KYC-01..14).
- Screening: how a subject is identified, whether attributes are required, possible-match vs confirmed, idempotency,
  monitoring (U-KYC-15..19). A vendor that requires attributes before the Q6 review is `BLOCKED`, not rejected.
- Geolocation: only if the owner chooses to evaluate one: outcomes, error semantics, client-side flows, latency
  (U-KYC-20..23).

Closure is by citation or vendor written answer. An unverifiable item stays UNVERIFIED and is carried into the K2 risk list.

## Exact inputs needed (minimal, no questionnaire)

From the **owner** (decisions; each with a recommendation in `open-questions.md`):

1. Name the vendor(s) and the types to evaluate; confirm the evaluation is documentation-only (OQ-KYC-01).
2. Decide hosted/redirect-only vs platform-held documents (OQ-KYC-02), and whether vendor-derived residence may feed
   market evidence (OQ-KYC-03).
3. Provide or authorize reading the vendor documentation; authorize the adapter build on recorded fixtures only after K1.
4. Commission the Q6 privacy review (OQ-KYC-04); give compliance's inputs: policy thresholds per jurisdiction, screening
   lists and re-screen interval, retention periods, data region, SAR format (OQ-KYC-07..09). None is invented by engineering.
5. Later, only at K6: written sandbox authorization. Later, only at K9: written production authorization.

From the **vendor** (via the owner; this package contacts no vendor):

1. Documentation: verification flow (hosted/redirect), status model, callback specification with signature test vectors,
   screening API contract and sandbox test subjects, monitoring options, geolocation contract if evaluated.
2. Sandbox terms in writing and sandbox access. Sandbox credentials: outbound API credential and an **inbound callback
   signing secret**, supplied by the owner **through the platform's per-tenant credential model** (`providercred`, domain
   `kyc` today; screening and geolocation need the X-G01 decision first; ADR 0093), `secret_ref` in the approved secret
   store. Never in chat, source, docs or logs.
3. One vendor workspace/account per tenant, with the callback URL form `POST /v1/webhooks/kyc/{tenantSlug}/{providerID}`
   configurable per account; non-secret workflow/level ids go to per-tenant provider configuration (PSP G-08).
4. Synthetic test subjects and test documents for every outcome; sandbox data-retention behaviour.
5. DPA draft, sub-processor list, data-region options, retention and deletion controls (for the owner and legal; not for
   an agent to sign).
6. Written answers to the `U-KYC-nn` items the documentation leaves open; published source IP ranges; escalation contacts.

Nothing else is requested. No production credential, no real customer data, no real identity document, no agreement or
DPA signature is requested at any gate before K9.
