# KYC / AML adapter: conformance test specification

Status: **SPECIFICATION (`NOT IMPLEMENTED`).** No test is written by this package. A real KYC, screening or geolocation
adapter is not accepted until every applicable case below passes against (1) the adapter's recorded-fixture double and
(2) the vendor sandbox, with the evidence in `integration-gates.md` gates K3 and K6. Provider-agnostic. Case ids
`KYC-C-nn` are new; existing test names are cited as they exist at base `4a3c85f`. Cases are tagged `[KYC]`, `[SCR]`
(screening) or `[GEO]` (geolocation) so each vendor type runs only its own.

Rules for the suite author:

- A case that cannot run for a real adapter must **fail**, never skip (precedent: `internal/kyc/conformance_test.go`
  tenant-binding case "mandatory means this case must FAIL, not skip, for any non-mock adapter").
- Vendor behaviour is taken from vendor-documented examples or sandbox captures with a citation (document URL + date, or
  sandbox capture id). A test must not encode an assumed vendor behaviour (`U-KYC-nn` items stay open until cited).
- **Fixtures contain synthetic identities only**: invented names, invented document numbers, vendor-supplied test
  subjects. No real person, no real document image, no real screening hit. A real person's data is never used in any
  test, sandbox run or fixture.
- A vendor outage, timeout or unknown value must produce a fail-closed result, never a pass.

## 1. Existing suites a real adapter must run unchanged

| Suite | Where | Applies as | Work to make it runnable for a real adapter |
|---|---|---|---|
| `kyc.RunProviderConformanceSuite(t, factory)`: (a) a credential resolved for one tenant is rejected for another (skip-to-fail for non-mock, includes MAC-level sub-case), (b) `HandleCallback` returns a bounded, control-character-free reason (`checkReasonBoundCase`; self-test `TestReasonBoundConformanceSelfTest_DetectsNonConformingReason` proves it goes red for five broken adapters) | `internal/kyc/conformance_test.go` | `[KYC]` mandatory | Both cases take the mock's callback builder (`mockCredentialFor`, `mockReasonCallback`). **New work KYC-C-01**: a test-only fixture-hook interface (`SignedKYCCallbackFixture`: build a vendor-correct signed inbound for {tenant, reference, outcome, reason text incl. oversize/control/bidi, timestamp}) implemented by the adapter's test package |
| `webhookauthtest.RunSchemeConformance` SC1..SC13 with a vendor known-answer vector | `internal/webhookauth/webhookauthtest` | `[KYC]` mandatory; scheme name added to `conformanceManifest` (`webhookauth/scheme.go:341`) in the same change | Subject to PSP G-01/G-02/G-08 outcomes |
| Retry semantics | `internal/webhookauth/retry_semantics_test.go`; `MustRequireRetrySemantics("kyc", ...)` | `[KYC]` mandatory | declared from the vendor's documented policy |
| Two-phase / outbox behaviour with a non-mock provider | `internal/kyc/kyc_two_phase_integration_test.go` (`TestWorker_*`: no connection held across the provider call, credential binding mismatch terminal and never reaches the provider, ambiguous result leaves status unchanged, provider `error` leaves status unchanged, terminal verification cancels without a call, callback approval during a provider call is not demoted, staff reject during a call survives, idempotency key for a multi-document set), `outbox_*`, `internal/httpserver/kyc_outbox_http_integration_test.go`, `kyc_outage_503_integration_test.go` | `[KYC]` run unchanged against the adapter's double | these use `spyKYCProvider`/the mock to drive outcomes; the adapter must be substitutable at the same seam (KYC-C-02) |
| Webhook handler | `internal/httpserver/kyc_webhook_indistinguishable_401_test.go`, `kyc_webhook_auth_failure_logging_integration_test.go`, `kyc_webhook_noop_logging_integration_test.go`, `kyc_flow_integration_test.go` | `[KYC]` run unchanged | the 401 uniformity and the 204 contract are platform-owned; a vendor needing another success status is a hook decision (KYC-G05 family) |
| Forward-only status and staff precedence | `kyc_two_phase_integration_test.go` `TestApplyForwardOnlyStatus_*`, `kyc_integration_test.go` | `[KYC]` run unchanged | n/a |
| Enforcement, holds, dormancy | `enforcement_integration_test.go`, `enforcement_hold_integration_test.go`, `enforcement_dormancy_integration_test.go`, `compose_hold_test.go`, `internal/casino/kyc_play_integration_test.go`, `kyc_play_outage_integration_test.go`, `aml_hold_integration_test.go` | `[KYC]`/`[SCR]` run unchanged | vendor independent; re-run after the adapter lands |
| Credential hygiene | `internal/kyc/credential_reflection_test.go`, `kyc_two_phase_integration_test.go` `TestCallContext_NeverRendersSecret`, `cmd/platform-api/outbound_precondition_test.go`, `cmd/platform-api/kyc_outbox_wiring_test.go` | `[KYC]` the tripwire must be relaxed **deliberately**, scoped to the KYC adapter, `security` sign-off + owner authorization (PROV-OUTBOUND-CRED-1) | a test proving only the named adapter is exempt |
| Screening conformance `screeningtest.Run(t, newProvider, Subjects{Clear, Sanction, PEP, Confirmed, Unavailable})`: identity and capabilities; malformed requests refused; clear; each match outcome consistent; unavailable is an error never a clear; idempotent per key; cancelled context never clears; deadline honoured; concurrent same key gives one answer | `internal/aml/screeningtest/conformance.go`; run by `TestMockScreeningProvider_Conformance` (`internal/aml/screening_test.go:68`) | `[SCR]` mandatory | The suite is parameterized by test SUBJECTS that the provider maps to outcomes. A real vendor needs **vendor-supplied sandbox test subjects** for clear / sanctions / PEP / confirmed / unavailable (U-KYC-15/16). If the vendor supplies none for an outcome, that outcome is `BLOCKED`, not skipped (KYC-C-30) |
| Screening decision engine and orchestrator | `internal/aml/screening_test.go` (`Decide` table), `screening_integration_test.go`, `screening_guards_integration_test.go`, `screening_policy_checks_integration_test.go`, `sanctions_potential_integration_test.go`, `holds_integration_test.go`, `adversarial_integration_test.go` | `[SCR]` run unchanged | vendor independent |
| Screening production guard | `TestMockScreeningProvider_IsSyntheticAndRefusedInProduction`; `providerkind` completeness scan | `[SCR]` | a real provider implements `ProductionEligible` only at K9 |
| Geolocation | `internal/geolocation/locator_test.go`, `mock_provider_test.go`; `internal/marketgate/*_test.go` (`marketgate_integration_test.go`, `hardening_test.go`, `locate_test.go`, `sampler_test.go`, `callers_scan_test.go`, `open_*_test.go`); `cmd/platform-api/market_gate_wiring_test.go` | `[GEO]` run unchanged | the gate is vendor independent: unknown evidence refuses. A real Locator must pass the same refusals (KYC-C-50..) |

## 2. Binding rows (known open items to close or accept before a real KYC/AML/geolocation adapter)

States are as recorded in the cited documents at base `4a3c85f`; re-verify at gate K0.

| Row | Source | Condition before the first real adapter | Case |
|---|---|---|---|
| KYC-HOSTED-SESSION-1 | ADR 0028 Stage 10.3 amendment | decision (OQ-KYC-02) and, if hosted, the session field (KYC-G01) | KYC-C-10..13 |
| KYC-SANCTIONS-IF-1 | same | satisfied by `aml.ScreeningProvider` (A7); real vendor mapping remains | KYC-C-30..36 |
| F-5 `error`-callback audit dedupe "with the first real KYC adapter" | same | built or accepted: repeated `error` callbacks on a non-terminal verification must not grow the audit log without bound | KYC-C-21 |
| `kyc_documents.rejection_reason` unbounded TEXT returned on player routes | same ("not in scope, flagged") | staff-entered, not provider text; confirm no vendor text flows into it | KYC-C-06 |
| Q6 privacy review (identity attributes to a vendor) | ADR 0116 | closed before any attribute leaves the platform | gate K8 / KYC-C-31 |
| A7 follow-ups F1 (triggers, system actor), F2 (gate predicate), F4 (reject proposal), F5 (alert routing to officers) | `docs/plans/a7-aml-record.md` s5 | F1 and F2 before a real screening vendor; F5 per ALERT-DELIVERY-1 | KYC-C-32, KYC-C-33 |
| D-AML-TIP-1 legal wording (E-class) | A7 record s6 | legal approval of the approver confidentiality duty before launch for `under_platform_licence` tenants | gate K8 |
| Real `DocumentStorageProvider` (encrypted, signed access URLs, access logging) and `MalwareScanner` | ADR 0029 s3 | only if platform-held documents remain (OQ-KYC-02) | KYC-C-14 |
| Real subject-key resolver (per-tenant HMAC key from the secret store) | ADR 0116 C7 | PROVIDER DEPENDENT / deploy | KYC-C-34 |
| Market evidence source (owner decision 2026-10-16) | decision register | implementation on `market-evidence-kyc-residence` merged and verified at HEAD, or documented fail-closed | KYC-C-52 |
| ALERT-DELIVERY-1 | readiness report | production blocker | gate K8 |

## 3. New cases

### 3.1 `[KYC]` verification adapter

| Id | Case | Needs DB |
|---|---|---|
| KYC-C-01 | Fixture-hook refactor of the two mock-typed cases (see s1), with a deliberately broken adapter fixture proving each goes red | no |
| KYC-C-02 | The adapter is substitutable at the `spyKYCProvider` seam so the two-phase tests run against its recorded-fixture double | yes |
| KYC-C-03 | Known-answer vector for the callback scheme (SC1..SC13); tamper matrix; tenant binding at MAC level | no |
| KYC-C-04 | Outcome mapping table: for every documented vendor status (per U-KYC-06..09 closure) the adapter returns exactly one closed `ProviderOutcome`; an undocumented status returns `error`/`ErrCallbackMalformedBody`, never `approved` | no |
| KYC-C-05 | Reason bounding: oversize, control, bidi/format characters, multi-byte truncation boundary, empty; `ReasonTruncated` reported; the stored value and the audit metadata are bounded; the player response never carries provider text | yes |
| KYC-C-06 | No vendor text reaches `kyc_documents.rejection_reason` or any player route | yes |
| KYC-C-07 | Raw payload never persisted or logged: static scan of the adapter for payload logging; logs use `RedactedProviderErrorDetail`; the `CallContext` renders redacted | no |
| KYC-C-08 | Create is idempotent: repeated phase-B resend with the same key leaves one verification and one reference (U-KYC-01) | yes |
| KYC-C-09 | Callback for a reference not yet CAS'd onto the row (race with phase C): retryable 5xx, and the redelivered callback applies once | yes |
| KYC-C-10 | Hosted flow (if KYC-G01 built): the session URL/token is returned only on the player's own request, short-lived, is not `provider_reference`, is absent from logs, audit, DB and error text | yes |
| KYC-C-11 | Hosted flow: return from the vendor does not change state; only an authenticated callback or poll does | yes |
| KYC-C-12 | `GetVerification` poll agrees with the callback result for the same reference; a poll never moves a terminal verification | yes |
| KYC-C-13 | Redirect target allow-list: the adapter cannot be made to emit a URL outside the vendor's configured host (SSRF/open-redirect style) | no |
| KYC-C-14 | Platform-held documents (only if kept): content type / size / checksum checks, malware scanner fail-closed on error, no bytes in the submit call, vendor retrieval by signed short-lived URL only, access audited | yes |
| KYC-C-15 | Approved verification with a vendor revocation/expiry (KYC-G03 if built): governed transition, audited; enforcement treats it as `failed`; if not built, the no-op is asserted and accepted in writing | yes |
| KYC-C-16 | Applicant-reference model (KYC-G05): a second verification for the same player/applicant maps to a distinct `provider_reference` or fails with a closed error, never overwriting or colliding | yes |
| KYC-C-17 | Vendor-supplied residence/expiry attributes (KYC-G02 if built): applied only under the evidence-collection-active check, forward-only, country validated alpha-2, never from the callback body of an unverified sender | yes |
| KYC-C-20 | Redelivery of an identical callback: one state change, one audit row, 204 each time; replay after terminal adds no audit row | yes |
| KYC-C-21 | Repeated `error` callbacks on a non-terminal verification do not grow audit without bound (F-5) | yes |
| KYC-C-22 | Out-of-order callbacks (approved before pending): forward-only; a late `pending` after `approved` is a no-op | yes |
| KYC-C-23 | Staff-set `review_required` stays sticky against a provider `approved` (existing test) with the vendor fixture | yes |
| KYC-C-24 | Enforcement with a vendor-driven `approved`, `rejected`, `pending`, vendor outage: deposit, withdrawal hold, payout, casino play, sportsbook play behave per policy; outage never manufactures `failed` | yes |

### 3.2 `[SCR]` screening adapter

| Id | Case | Needs DB |
|---|---|---|
| KYC-C-30 | `screeningtest.Run` with vendor-supplied sandbox subjects for each outcome; an outcome without a subject is recorded `BLOCKED` | no |
| KYC-C-31 | Request minimality: the serialised vendor request contains only the pseudonymous `SubjectRef` / `VendorSubjectRef` and list names; no name, DOB, nationality, platform id, tenant id, email (static check of the wire fixture) unless the Q6 review has closed and then only the approved set | no |
| KYC-C-32 | Vendor outage, timeout, 429, malformed or partial response, unknown outcome string, panic: all `error` with a closed code; never `clear`; `Decide` yields retry or hold per policy | yes |
| KYC-C-33 | A potential sanctions match places the all-value hold and opens a case in the same transaction; a PEP-only match holds withdrawal and payout; a confirmed match needs four-eyes to lift; lift routes behave per the decision on KYC-G08 | yes |
| KYC-C-34 | Subject key: the pseudonymous reference is stable per person per tenant, differs across tenants, is not reversible to a platform id; the real key resolver reads only the secret store | yes |
| KYC-C-35 | Idempotency: same key + same request returns the same result and one stored row; same key + different request is `ErrIdempotencyConflict`; concurrent same key gives one vendor call or one answer | yes |
| KYC-C-36 | Match opacity: `VendorMatchRef` and `Category` are never parsed or persisted; only the list name is stored; no raw vendor payload in logs or audit | yes |
| KYC-C-37 | Screening-required gate predicate (KYC-G07/F2 if built): deposit, withdrawal and payout are refused when a policy requires screening and no completed result exists; an `error` result is never a pass | yes |
| KYC-C-38 | Trigger and worker (F1 if built): registration, KYC approval and scheduled re-screen enqueue through the outbox; no vendor call in an HTTP path or a money transaction (`txscope`); system-actor hold placement audited | yes |
| KYC-C-39 | Async/push result (if the vendor offers one): enters only through webhook admission (verify-first, tenant-bound), updates the stored result append-only, and a later `potential_match` on a previously cleared subject places the hold | yes |

### 3.3 `[GEO]` geolocation adapter (only if a vendor is selected)

| Id | Case | Needs DB |
|---|---|---|
| KYC-C-50 | A real Locator returns `resolved` + valid alpha-2 only for a documented fixture; invalid country, empty, `inconclusive`, `unavailable`, `error`, Go error, panic, timeout (`DefaultLocateTimeout`) all refuse at the gate | yes |
| KYC-C-51 | IP and country never stored, logged or audited (audit row carries ids and closed codes only; scan of log output in the test) | yes |
| KYC-C-52 | Production eligibility: the Synthetic Locator is refused in production; the real Locator is accepted only when marked `ProductionEligible`; with no eligible evidence source registration, deposit and launch remain refused | yes |
| KYC-C-53 | Latency/availability: vendor slowness cannot exhaust request capacity (`maxInflightLookups`), and does not hold a transaction | yes |
| KYC-C-54 | Evidence source precedence if both KYC-verified residence and a vendor are configured: the stricter/declared rule per the owner decision, with a test for disagreement (country differs) | yes |
| KYC-C-60 | Exactly one audit row per consultation `{provider_id, outcome, provider_reference, request_id}`, without country or IP, at every consumer (registration, deposit, launch, sportsbook wager) | yes |

## 4. Evidence required per case

- Unit/adapter cases: test name, fixture citation (URL + retrieval date or sandbox capture id), `go test` output.
- DB cases: run as the NOBYPASSRLS runtime role, as the existing integration suites do.
- Sandbox cases: run log with request/response ids, redacted; synthetic identities only; no real document image.
- A skipped mandatory case is a failure. A case blocked by an open `U-KYC-nn` is recorded `BLOCKED` with the item id.
