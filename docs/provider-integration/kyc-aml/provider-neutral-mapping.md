# Provider-neutral mapping: KYC / AML platform requirement -> vendor supply -> neutral interface -> gap

Status: **SPECIFICATION (`NOT IMPLEMENTED` as an adapter).** Provider-agnostic: applies to any KYC document-verification
vendor, sanctions/PEP screening vendor or geolocation vendor, whether one vendor offers all three or three vendors each
offer one. Every "vendor must supply" cell is a requirement on the vendor, not a statement of what any vendor does. Code
references were read at base `4a3c85f` of `claude/focused-wright-jw88w9`. `U-KYC-nn` = unverified vendor assumption
(registry in section 12); `KYC-Gnn` = platform-gap candidate (section 11).

Column legend: **Interface** = how the neutral interface carries it today. **Gap** = what is missing or at risk; `PW`
platform-wide (any real vendor), `PS` provider-specific; reviewer in brackets.

## 1. KYC document and verification flow

| Platform requirement | Vendor must supply | Interface (code) | Gap / UNVERIFIED |
|---|---|---|---|
| Verification state is platform-owned; no vendor status string leaks into core | A way to map its states to the closed outcome set | `ProviderOutcome` approved / rejected / pending / review_required / expired / error (`provider.go:21-34`); `statusForOutcome`; `error` never changes a verification's state | section 2 |
| Per-tenant vendor selection fails closed | n/a | `SelectProvider` (`provider_selection.go`): the tenant's active in-window `outbound_api` handle for domain `kyc` IS the selection; ambiguous -> `ErrKYCProviderAmbiguous`, none -> `ErrNoKYCProviderConfigured` (HTTP 503 at `kyc_handlers.go:184`); the single Synthetic adapter is auto-selected only outside production | none |
| Vendor call is outside any DB transaction, retried by an outbox, never from an HTTP path | An idempotent create and submit | `kyc_submission_outbox` (migration 0114, ADR 0106): phase A (tx) -> phase B (vendor call, no tx, `ErrProviderCallRefused` under `txscope.Held`) -> phase C CAS apply; states pending/claimed/sent/failed_terminal/cancelled; closed error classes ambiguous/not_sent/lease_expired/...; exponential backoff, DB-clock deadline; submit idempotency key from the sorted document id set (`submissionIdempotencyKey`) | U-KYC-01 (does create and submit accept an idempotency key / client reference; is a repeated create safe; ambiguous-result semantics) |
| Create a verification session for an opaque (tenant, brand, account, person) tuple | An applicant/session creation call that needs no PII from us | `CreateVerificationInput{TenantID, BrandID, PlayerAccountID, PersonID, Call}`: opaque ids only (`provider.go:60-80`); `ProviderResult.ProviderReference` required on success | U-KYC-02 (what the vendor needs at create time: external user id, level/workflow id, locale, country; whether an opaque id suffices without name/email) |
| **Hosted / redirect collection: the player's identity documents and selfie are captured by the vendor, not by us** | A hosted page, redirect URL or SDK token for the player; the vendor stores the documents | **Not representable.** `ProviderResult` carries only `ProviderReference`, `Outcome`, `Reason`, `ReasonTruncated`. ADR 0028 forward note J12: a hosted vendor "must issue its own short-lived session token ... must not repurpose `provider_reference`" - the open item **KYC-HOSTED-SESSION-1** (ADR 0028 Stage 10.3 amendment, "Not in scope, flagged") | **KYC-G01 [PW, identity-compliance+security+architect; blocks first real adapter]**. U-KYC-03 |
| Platform-held documents (the alternative to hosted) | A way to receive documents we hold | The current model: `UploadDocument` (`document_service.go:99`) stores bytes through `DocumentStorageProvider` (`storage.go`; only `MockDocumentStorageProvider`, in-memory, Synthetic) after `MalwareScanner` (`malware.go`; only a mock; upload FAILS CLOSED on scanner error). `SubmitVerification(ref, []SubmittedDocument{DocumentID, DocumentType}, call)` passes **ids only**; the interface deliberately never carries bytes; how a vendor retrieves them is ADR 0028 s5 "OPEN CONSIDERATION" with no mechanism | KYC-G01: a platform-held model puts raw identity documents in our infrastructure (real encrypted object storage with signed URLs per ADR 0029 s3 is NOT built; retention/legal-hold tooling NOT built; storage-layer malware scanning NOT built). Contradicts the hosted/redirect-only posture; a decision is required (OQ-KYC-02). U-KYC-04 |
| No PII or raw evidence in reasons, logs, audit | n/a | `NormalizeReason` bounds to 512 bytes and strips C0/C1/bidi/format controls (`reason_normalize.go`, KYC-REASON-BOUND-1); the platform re-normalizes on every write; the player never sees any provider text (HD-10.3-3); `RedactedProviderErrorDetail` for error logs; raw payload never stored | adapter must call `NormalizeReason`; the conformance suite fails otherwise (KYC-C-05) |
| Document types | A mapping of the vendor's document types | `DocumentType` closed set: passport, national_id, drivers_license, proof_of_address, selfie, other; `Capabilities{SupportedDocumentTypes, SupportsCallback}` | U-KYC-05 |
| Document and verification retention / erasure | Vendor-side deletion/retention controls and evidence | Retention periods are legal configuration, nothing is purged (ADR 0116 D11, `docs/architecture/16-privacy.md` Retention); identity erasure fails closed against an AML legal hold (C9); no purge tooling | section 9; OQ-KYC-09 |

## 2. Status mapping (vendor status -> platform KYC state)

The adapter maps; the platform never sees a vendor string. The vendor-status column below is deliberately a *class of
vendor state*, not a name: no vendor is selected and none is invented.

| Class of vendor state (UNVERIFIED which vendor states fall here) | `ProviderOutcome` the adapter returns | Platform status | Rule (code) |
|---|---|---|---|
| Created / awaiting player action / processing | `pending` | `pending` (rank 1) | forward-only (`statusRank`; CAS in `applyForwardOnlyStatus`, `provider.go:526`) |
| Needs a human review at the vendor or ambiguous result | `review_required` | `review_required` (rank 2) | a staff-set `review_required` is sticky against a provider `approved` (`TestApplyForwardOnlyStatus_StaffSetReviewRequiredIsStickyAgainstProviderApproval`); a provider-set one still advances (`..._ProviderSetReviewRequiredStillAdvancesToApproved`) |
| Final positive | `approved` | `approved` (rank 3) | terminal; the only state enforcement treats as `passed` (`effectiveOutcome`) |
| Final negative, retry allowed ("resubmit", "retry") | `rejected` | `rejected` (rank 3, terminal) | **no state for "retryable rejection"**: the player starts a new verification; whether the vendor allows a new check on the same applicant reference is U-KYC-06 (the reference is UNIQUE per `(tenant, provider, provider_reference)`) |
| Final negative, non-retryable (fraud / blocked) | `rejected` | `rejected` | a fraud signal that should feed AML/Risk has no channel: the reason is staff-only and bounded text |
| Expired / abandoned / session timed out | `expired` | `expired` (rank 3) | accepted only forward from a non-terminal state |
| Vendor revokes or re-opens an already `approved` verification (ongoing monitoring, document expiry, re-check, fraud finding) | any | **ignored**: same-rank or backward transitions are no-ops (`statusRank`; "never resurrects a terminal verification") | **KYC-G03 [PW]**: post-approval vendor changes are unrepresentable; staff cannot review a terminal row either (`ErrInvalidTransition`, `ReviewVerification`). U-KYC-07 |
| Provider call or callback failure | `error` | unchanged; one failure audit row for a non-terminal verification, none for an already terminal one (K5) | closed; never corrupts state |
| An outcome string outside the closed set | rejected by the adapter, or `ErrCallbackMalformedBody` | unchanged; an unrecognized outcome on create with a reference still leaves a `pending` (`TestWorker_Create_UnrecognizedOutcomeWithReferenceStillBecomesPending`), without a reference leaves the orphan row untouched | adapter must not return an unknown value |
| Verification validity period (approval lapses) | n/a | `kyc_verifications.expires_at`: enforcement treats an approved row past `expires_at` as `failed` (`enforcement.go:537`) | **KYC-G02**: no non-test writer of `expires_at` exists and `ProviderResult` has no field for it; a vendor-stated validity cannot be recorded. U-KYC-08 |

## 3. Tiers, thresholds, per-jurisdiction rules

| Platform requirement | Vendor must supply | Interface (code) | Gap / UNVERIFIED |
|---|---|---|---|
| Thresholds and triggers are configuration per licensing jurisdiction; no value in code | n/a | `kyc_enforcement_policies` (migration 0100), admin `CreateEnforcementPolicy` / `ActivateEnforcementPolicy` / `WithdrawEnforcementPolicy`; zero rows seeded. Wired triggers: structural `withdrawal_requires_passed` (always on), `cumulative_deposit` (deposit), `play` (casino and sportsbook) | Thresholds are compliance inputs (HD-KYC-2/3 content), not invented here |
| `edd_amount` and `registration_tier` triggers | n/a | Modelled in schema and admin path but **not consulted** by `EvaluateEnforcement` (`enforcement.go` header: "KNOWN GAP ... PARTIALLY IMPLEMENTED") | **KYC-G04 [PW, identity-compliance]**: a vendor's levels (basic / enhanced / EDD) have no platform representation: there is exactly one `approved` state. A jurisdiction needing tiered KYC needs `registration_tier`/`edd_amount` wired and a level notion on `Verification`. Blocks only if the first brand's jurisdiction requires tiers. U-KYC-09 |
| Jurisdiction-specific document sets | Per-country document support list | `Capabilities.SupportedDocumentTypes` is global, not per country | U-KYC-10 |
| Enforcement points | n/a | `EnforcementDeposit`, `EnforcementWithdrawalHold`, `EnforcementWithdrawalPayout`, `EnforcementCasinoPlay`, `EnforcementSportsbookPlay`; outcomes not_required / passed / pending / failed / unavailable (+ internal `held`); decision rows in `kyc_enforcement_decisions`; deny codes closed; `unavailable` fails closed (ADR 0096) | none |
| Vendor outage does not manufacture a `failed` | n/a | an outage leaves the verification in the excluded orphan shape; enforcement never reads the outbox (INV-KYC-OB-5, static test) | none |

## 4. Webhooks, outbox, redelivery (KYC-WH-1)

| Platform requirement | Vendor must supply | Interface (code) | Gap / UNVERIFIED |
|---|---|---|---|
| Verify-first, tenant-bound callbacks at `POST /v1/webhooks/kyc/{tenantSlug}/{providerID}` | An authenticated callback the platform can express as a `VerificationScheme` | `KYCProvider.WebhookScheme()` selected by provider id, verified by the Orchestrator over raw bytes before parsing (`webhook_verify.go`), uniform 401 for every pre-verification failure, 204 empty body on success | Same shared gaps as PSP: **PSP G-01** (header-only `Extract`), **G-02** (mandatory signed timestamp), **G-08** (one secret + one `vendor_account_id`); `../README.md` X-G03. U-KYC-11 |
| Reference mapping | A callback that names the verification reference we hold | `getVerificationByProviderReference` with explicit `(tenant_id, provider_id, provider_reference)` predicate; unknown reference = `ErrVerificationReferenceUnknown` -> retryable 5xx (callback may race `CreateVerification` phase C, no KYC receipt table) | U-KYC-12: whether the vendor redelivers on a 5xx (ADR 0095 s15.2: confirm at intake) |
| Dedupe and ordering | An event id or monotonic status | No event id column; replay is idempotent through the unique reference + forward-only rank; terminal rows absorb everything; no audit growth after conclusion | KYC-G03/G05. U-KYC-13 (event ids, ordering, duplicate delivery) |
| Reference model | One stable reference per verification attempt | `provider_reference` is UNIQUE per `(tenant_id, provider_id, provider_reference)` and staff-only (not returned to players) | **KYC-G05 [PW, identity-compliance]**: a vendor whose stable id is an *applicant* that gets several *checks/sessions* (re-verification) cannot map one applicant to several verifications. U-KYC-06 |
| Retry semantics declared | Documented redelivery policy | `webhookauth.MustRequireRetrySemantics("kyc", providers)` panics for a non-MOCK adapter without a declaration (ADR 0097) | U-KYC-14 |
| Per-merchant keys or a signed account id, timestamp tolerance | per ADR 0022 s3 point 3 as amended for KYC-WH-1 | `providercred` domain `kyc`, purposes `webhook_verify`, `outbound_api` | U-KYC-11 |

## 5. Screening: sanctions and PEP (`aml.ScreeningProvider`)

| Platform requirement | Vendor must supply | Interface (code) | Gap / UNVERIFIED |
|---|---|---|---|
| Screening is OUR contract; vendors are mapped onto it | A screening call by an opaque subject, per list, with an idempotent request | `ScreeningRequest{IdempotencyKey ^[A-Za-z0-9_.:-]{1,128}$, SubjectRef 64 lowercase hex, Subject{VendorSubjectRef}, Lists 1..2 of sanctions|pep}`; `ScreeningResult{Outcome clear|potential_match|confirmed|error, Matches[{List, VendorMatchRef, Category}] opaque, ErrorCode closed, ProviderReference, ScreenedAt}` (`screening.go`) | U-KYC-15 (how a vendor identifies a subject without name/DOB: only if the same vendor holds the verified identity from KYC) |
| Pseudonymous subject; no platform id to the vendor | Acceptance of a per-tenant opaque reference | `SubjectRef` = HMAC(per-tenant key from the secret store, person id) (C7); never `persons.id`, never an account id; the real key resolver is PROVIDER DEPENDENT / deploy | |
| **Identity attributes (name, date of birth, nationality) for matching** | Most name-matching engines need them | **BLOCKED** until the ADR 0116 Q6 privacy review; the request cannot carry them; attribute storage not built | **KYC-G07 [PW, identity-compliance+security]**: a screening vendor that matches on attributes cannot be used by the pseudonymous contract alone; works only when the KYC vendor that already holds the subject also screens it (`VendorSubjectRef`). Decision OQ-KYC-04 |
| Fail closed | Distinguishable failure | `Decide` (pure): `clear` -> none; `confirmed` -> hold all value movement + case; `potential_match` naming sanctions -> hold all value movement + case; PEP-only -> hold withdrawal and payout + case; `error` / unknown / panic / timeout -> NEVER clear: no policy -> no hold, retry; with policy: held (withdrawal+payout) for a person registered at or after `effective_from`, or after the backfill grace (`screening.go` `Decide`) | none platform-side. U-KYC-16 (what the vendor returns for "possible match" vs "confirmed"; whether it distinguishes) |
| Idempotency per key | Same key + same request -> same answer; same key, different request -> conflict | `ErrIdempotencyConflict`; the mock and `screeningtest` pin it | U-KYC-17 |
| Result is a record, never a clear without completion | n/a | `aml_screening_results` append-only, FORCE RLS, structural CHECKs (a clear carries no match/error; an error names its closed code; a match names its list), `aml_screening_record` places the required hold in the same transaction; per-tenant/jurisdiction `aml_screening_policies` (no default, no seed, legal review reference required) | |
| Triggers: registration, KYC approval, attribute change, scheduled re-screen | n/a | **NOT IMPLEMENTED** (A7 follow-up F1): officer-initiated only; needs `PlatformService` identity + outbox worker (ADR 0106 pattern) + a system actor for hold placement | **KYC-G07**; `AML-SCREEN-TRIGGERS-1` |
| Gate: "screening required and no completed result" at deposit / withdrawal / payout | n/a | **NOT IMPLEMENTED** (F2): "a recorded `clear` has no enforcement effect"; only holds (placed by a match or the policy rule) gate | KYC-G07; `AML-SCREEN-GATES-1` |
| `RecordScreening` is officer-attested | n/a | The database records what the officer's proof-bound call says; it does not verify a provider was called (A7 record s4.7) | the automatic path (F1) removes the attestation gap |
| Ongoing monitoring / list-update push / async result | A push or a poll for new matches on a known subject | **No async or push model**: `Screen` is request/response; "a vendor push, if one is ever added, enters only through webhook admission (ADR 0091/0097)" (ADR 0116 s4.2); S4 monitoring `NOT IMPLEMENTED` (readiness item `R-AML-MONITORING` always fails) | **KYC-G07**. U-KYC-18 (does the vendor offer continuous monitoring, as a push or a re-screen) |
| Credentials and call context for the vendor | An outbound credential | **None**: `ScreeningRequest` has no `CallContext`; `providercred` domains exclude AML (migration 0096 CHECK) | **KYC-G06 / X-G01 [PW, architect+security]** |
| Match detail (score, category, list names, adverse media) | Opaque references | `VendorMatchRef` and `Category` are opaque, never parsed, never persisted (only the list name is) | by design; an officer needs the vendor's own console for detail. U-KYC-19 |

## 6. AML officer flows, holds, SAR (platform-owned; the vendor is not involved)

| Concept | Platform handling (code) | Vendor relevance / gap |
|---|---|---|
| Holds | `aml_hold_active(...)` boolean read by the gates; a held operation looks identical to KYC pending (`account_review`, ADR 0116 s5.1); `held` gates only NEW value entering play or leaving the platform, never settlement, correction or release (D9) | none |
| Hold lift rules | `confirmed_sanctions` class (CONFIRMED outcome naming the sanctions list) lifts only by the proof-bound four-eyes approval of a case; `manual` class (including a **potential** sanctions match and PEP) lifts by an `aml_hold:manage` officer with critical step-up (`holds.go` `LiftHold`; migration 0154 line 335) | **KYC-G08 [PW, identity-compliance+security]**: a potential sanctions match is single-officer-liftable though it holds ALL value movement. A7 security review SR7-8 recommended a distinct class with the same enforcement and lift rule as the confirmed class before a real vendor; the class was later re-derived (round 2). Compliance decides (OQ-KYC-05) |
| Cases and officer routes | 11 routes under `/v1/admin/tenants/{tenantID}/aml/` with permissions `aml_case:read/work/close_approve`, `aml_hold:manage`; step-up classes; four-eyes proposals/approvals with proposer != approver != subject person | none (platform-owned) |
| Quorum | at least 2 distinct officer persons per owner scope before activation | readiness `R-AML-QUORUM` |
| SAR | `aml_sar_*` design (ADR 0116 s3.3): versioned `SARDocument`, `SARRenderer` registry keyed by jurisdiction, only `generic_json_v1` MOCK; vendor identity attributes fetched by `aml_sar_worker` into an encrypted AML-scoped artefact; filing is human (`external_filing_reference`) | S5 `NOT IMPLEMENTED`; a regulator format is a legal input. The vendor attribute fetch depends on Q6 |
| Transaction monitoring | DB-fed `aml_monitorable_tx_type` (deposit, withdrawal*, manual_adjustment, bonus_conversion, conversion); casino and sportsbook types explicitly NOT monitorable | S4 `NOT IMPLEMENTED`; not a vendor matter |
| Tipping-off | tenant/player surfaces never distinguish hold from KYC pending; adjustment-approver confidentiality duty (D-AML-TIP-1 option c decided; legal wording pending, E-class) | |

## 7. Geolocation (only if a vendor is selected)

The owner decision of 2026-10-16: **market evidence = KYC-verified residence as the current source, vendor geolocation
later, no invented provider; registration / deposit / launch stay fail-closed without evidence.** This section says what a
geolocation vendor would have to satisfy; it does not recommend selecting one.

| Platform requirement | Vendor must supply | Interface (code) | Gap / UNVERIFIED |
|---|---|---|---|
| Country-grain evidence per request, fail closed | A country code for a request, with a distinguishable "could not determine" | `Locator.GetSignal(ctx, SignalRequest{TenantID, BrandID, PlayerAccountID, RequestID, IPAddress}) (SignalResult{Outcome resolved|inconclusive|unavailable|error, CountryCode, ProviderReference, Reason})` (`geolocation/locator.go`, `provider.go`); `CountryCode` meaningful only for `resolved`; a Go error or any other outcome is "no usable evidence" and every caller refuses | U-KYC-20 |
| No coordinates, city, ISP, ASN, postal code, raw payload | n/a | `SignalResult` structurally cannot carry them (binding requirement 1); the package has no SQL and no persistence | A vendor whose value is VPN/proxy detection, device attestation, confidence or subdivision has no field: `Capabilities.SupportsSubdivision`/`SupportsVPNDetection` are declared but `SignalResult` has no way to carry the result. **KYC-G09 [PW, security+identity-compliance+architect]** |
| IP is request-scoped and never stored or logged | IP lookup | `IPAddress` "MUST NOT be stored, logged, or returned"; audit rows carry ids and closed codes only, never country or IP (`marketgate.AuditActionRefused`) | A client-side / SDK-based geolocation (device GPS, Wi-Fi) flow where the player's device produces a signed token cannot be expressed: the request has an IP only. U-KYC-21 |
| Timeout bound; a slow vendor cannot hold requests | Low latency | `marketgate.DefaultLocateTimeout = 2s`, in-flight bound (`maxInflightLookups`), timeout = unknown evidence = refuse; lookup outside the transaction | U-KYC-22 (latency, availability SLO: the vendor becomes a hard dependency of every gated request, canonical-model s12.2) |
| Where the evidence is consumed | n/a | `marketgate.Gate.Locate` (evidence) then `Guard` inside the guarded tx for registration, deposit, launch, sportsbook wager; brand-level `AnyCountryPermits` for new casino bets (no player request there) | **No per-bet player evidence** (casino bets are vendor callbacks): the 4 h session lifetime is the bound (CAS-G05) |
| Production-eligible evidence | A real vendor adapter marked `ProductionEligible` | Only `StaticLocator` (Synthetic) is wired (`registrations.go:209`); the startup guard refuses a Synthetic Locator in production, so production refuses registration / deposit / launch until an eligible evidence source exists. The KYC-verified-residence evidence source (owner decision 2026-10-16) is tracked on branch `market-evidence-kyc-residence`, **not on this base**; its state is UNVERIFIED here | Registration happens before KYC, so residence evidence cannot gate the registration step itself; how registration is evidenced is an owner/compliance decision (OQ-KYC-06). U-KYC-23 |
| Credentials | An outbound credential for the vendor | **None**: `SignalRequest` has no `CallContext`; `providercred` has no geolocation domain | KYC-G06 / X-G01 |
| Audit | One audit row per consultation `{provider_id, outcome, provider_reference, request_id}`, never the country or IP | Contractual note for the consumer (provider.go binding 4) | verify the consumer writes it (KYC-C-60) |

## 8. Enforcement is our code (fail closed) and audit

| Control | Where | Property |
|---|---|---|
| KYC-before-money | `kyc.EvaluateEnforcement` at deposit (`internal/payments/kycgate.go`), withdrawal hold and payout, casino bet (`orchestrator.go:2027`), sportsbook bet (`sportsbook/orchestrator.go:372`) | pure DB read; never calls a vendor; no caller-supplied outcome; `unavailable` denies |
| AML hold composition | `evaluateHold` inside `EvaluateEnforcement`; hold-evaluator error = deny `account_review` | same public decline as KYC pending |
| Market evidence | `marketgate` | unknown evidence refuses |
| Audit | every KYC state change, every enforcement decision row except the dormant `not_required` path, every AML action; staff reads of document content audited (`GetDocumentContent`); IP / user agent / request id on mutating admin actions | append-only; reasons bounded |
| Vendor cannot unlock | a vendor `approved` cannot override a staff-set `review_required`; an AML hold is independent of KYC state | |

## 9. Data protection: residency, retention, DPA

| Topic | State | Gap |
|---|---|---|
| Data residency per jurisdiction | CLAUDE.md requires data residency to vary by jurisdiction; **no code concept** exists for choosing a vendor's region per tenant or jurisdiction (grep for residency in `internal` finds none) | Not representable in `ProviderConfig`; non-secret per-tenant vendor configuration needs a home (PSP G-08). Owner/legal input: required vendor region(s) |
| Retention | Legal configuration; no period invented; nothing purged; `aml_retention_policies` designed (ADR 0116 s5.4), HD-RET-1 pseudonymisation scope; retention/legal-hold tooling for documents NOT built (ADR 0029) | Vendor-side retention and deletion SLAs are a DPA input |
| Erasure vs legal hold | fails closed against an AML legal hold (C9) | vendor deletion must follow the same rule |
| DPA / sub-processor | n/a (not code) | Owner/legal: a data-processing agreement, sub-processor list and transfer mechanism per vendor, signed by the owner. No agent signs |
| Cross-tenant reuse of a verification | ADR 0028 s7 OPEN DECISION: no cross-tenant reuse now | Vendor reusable-KYC products are out of scope until decided |
| PII in this repository | none; fixtures must be synthetic | |

## 10. Credentials and environments

| Requirement | Interface | Gap |
|---|---|---|
| KYC vendor: per-tenant handles, domain `kyc`, purposes `webhook_verify` + `outbound_api`, `secret_ref` only | `providercred` (ADR 0093); `OutboundCredentialResolver` per call with `CallContext` (redacted everywhere) | PSP G-08 (non-secret ids) |
| Screening vendor and geolocation vendor | **no domain** | KYC-G06 / X-G01 |
| Sandbox vs production | `APP_ENV` + synthetic guard; real adapter `ProductionEligible` only at K9; `kyc` registrations are conditional on wiring (`registrations.go` `b.KYC != nil`) | the bundle fields are concretely typed mocks (`AMLScreening`, `Geolocation *geolocation.StaticLocator`, `DocumentStorage`, `MalwareScanner`); expected adapter-build work (gate K2) |

## 11. Platform-gap candidates (for `architect`, `security`, `identity-compliance`; no core change is made here)

Shared with the PSP package and not repeated: PSP G-01, G-02, G-08 apply to the KYC webhook scheme; G-04 does not apply
(no statement source in KYC).

| Id | Gap | Scope | Reviewer | Blocks first real adapter? | Minimal proposal (only if confirmed necessary) |
|---|---|---|---|---|---|
| KYC-G01 | No hosted/redirect session in `ProviderResult`; documents are platform-held with mock-only storage and scanner; no mechanism for the vendor to fetch bytes (KYC-HOSTED-SESSION-1) | PW | identity-compliance, security, architect | **Yes** | Additive: an optional `HostedSession{URL or token, ExpiresAt}` on the create result, returned only on the player's own request path, short-lived, never persisted or logged, never reusing `provider_reference`; plus a decision (OQ-KYC-02) whether platform-held document upload remains at all. Real encrypted storage and scanner only if platform-held stays |
| KYC-G02 | A vendor cannot supply verified residence country, document expiry, verification validity or level; `expires_at` has no writer; residence is staff-set only | PW | identity-compliance, security (privacy Q6), architect | Yes if the owner wants vendor-derived residence as market evidence; no if staff determine it | Additive optional normalized attributes on the result (country alpha-2, valid-until), applied under the same forward-only CAS and the same evidence-collection-active check as `ReviewVerification`; legal basis per the privacy review |
| KYC-G03 | Terminal states are final on the provider path; post-approval revocation/expiry/re-check events from a vendor are no-ops | PW | identity-compliance, security | Yes if the vendor sends such events (likely) | A distinct "revocation" outcome handled as a governed transition (approved -> revoked/expired) with audit, never silently; enforcement already treats `expired`/past-`expires_at` as failed |
| KYC-G04 | `registration_tier` and `edd_amount` triggers are not consulted; no level notion | PW | identity-compliance | Only if the first jurisdiction requires tiers | Wire the two triggers once HD-KYC-2/3 content exists; add a level to the verification only with them |
| KYC-G05 | One `provider_reference` per verification, UNIQUE per tenant+provider: applicant-vs-check reference models and re-verification on the same applicant are not representable; no event id | PW | identity-compliance, architect | Potentially (U-KYC-06/13) | Adapter chooses the per-attempt reference as `provider_reference`; if the vendor offers only an applicant id, an alias rule is needed (design by identity-compliance). Do not loosen the UNIQUE |
| KYC-G06 / X-G01 | No credential domain or `CallContext` for screening, geolocation (and, outside this package, sportsbook and sports data) | PW | architect, security | **Yes** for any real screening or geolocation vendor | ADR 0093 amendment + migration: extend the `domain` CHECK (and `Domains`) with `aml_screening`, `geolocation`; add an optional `CallContext` to `ScreeningRequest` / `SignalRequest` (or resolve the credential in the adapter via a resolver). One decision for all areas (see `../README.md`) |
| KYC-G07 | Screening: pseudonymous input only (attributes BLOCKED, Q6); no triggers, outbox, system actor, gate predicate, async/push result or monitoring; officer-attested record | PW | identity-compliance, security, architect, ledger-finance (gates) | **Yes** for any real screening vendor | A7 follow-ups F1 (triggers + system actor + worker), F2 (gate predicate), F6 (Q6 review) in that order; monitoring S4 and SAR S5 after |
| KYC-G08 | A potential sanctions match is a single-officer-liftable all-value hold; false-positive churn with a real list | PW | identity-compliance, security | No (compliance decision), but decide before a real vendor | Make the potential-match class lift via four-eyes (distinct class, same enforcement, SR7-8) if compliance wants it |
| KYC-G09 | `SignalResult` cannot carry confidence / VPN-proxy / device attestation / subdivision / freshness; no client-side geolocation flow; no per-bet evidence | PW | security, identity-compliance, architect | Only if a geolocation vendor is selected | Add fields only when the selected vendor's documentation shows what is needed; keep country grain and no coordinates (binding requirement 1) |
| KYC-G10 | Data residency / retention / DPA are not modelled in code | PW (non-code + small config) | architect, identity-compliance, devops | Owner/legal input, not code | Per-tenant non-secret vendor configuration (region); retention values from compliance; DPA signed by the owner |

## 12. UNVERIFIED registry (needs provider documentation/sandbox)

23 entries. Closure is by citation (URL + retrieval date), vendor written answer or sandbox capture.

| Id | Question |
|---|---|
| U-KYC-01 | Create/submit idempotency (client reference or key), safe repeat, ambiguous semantics |
| U-KYC-02 | What the vendor needs at create time; whether an opaque id suffices without PII |
| U-KYC-03 | Hosted page / redirect / SDK token model, session lifetime, return URL handling |
| U-KYC-04 | If platform-held: how the vendor receives documents (upload URL, direct bytes, pull by reference) |
| U-KYC-05 | Document types and per-country coverage |
| U-KYC-06 | Applicant vs check/session reference; re-verification on the same applicant; retryable rejection |
| U-KYC-07 | Post-approval events: revocation, expiry, re-check, ongoing monitoring |
| U-KYC-08 | Verification validity period and expiry notification |
| U-KYC-09 | Levels / tiers (basic, enhanced) and how they are expressed |
| U-KYC-10 | Per-country document and workflow configuration |
| U-KYC-11 | Callback authentication scheme (location, algorithm, timestamp, key id, per-merchant keys, signed account id) |
| U-KYC-12 | Redelivery on 5xx; retry window; behaviour for an unknown reference |
| U-KYC-13 | Event ids, ordering and duplicate delivery guarantees |
| U-KYC-14 | Documented retry policy (429/503, Retry-After) |
| U-KYC-15 | Screening subject identification without name/DOB |
| U-KYC-16 | Possible-match vs confirmed distinction in results |
| U-KYC-17 | Screening idempotency |
| U-KYC-18 | Continuous monitoring: push vs re-screen, list-update notification |
| U-KYC-19 | Match detail available to officers (console, API) |
| U-KYC-20 | Geolocation: signal outcomes, error semantics, coverage |
| U-KYC-21 | Client-side / device-based geolocation flow and token format |
| U-KYC-22 | Geolocation latency, availability, rate limits |
| U-KYC-23 | Whether geolocation is needed at all once KYC-verified residence is the evidence source |

## 13. What must be true before any of this is real

See `integration-gates.md`. In short: documentation review (K1), adapter against recorded fixtures (K2), conformance (K3),
`security` and `identity-compliance` review (K4, K5), sandbox on a synthetic tenant with synthetic identities (K6),
operational acceptance (K7), pre-production readiness including the privacy review (K8), explicit production
authorization (K9). No real customer data and no real money until K9, in writing, from the owner.
