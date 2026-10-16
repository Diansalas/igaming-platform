# Fiat PSP adapter: conformance test specification

Status: **SPECIFICATION (`NOT IMPLEMENTED`).** No test is written by this package. A real adapter is not accepted until
every case below passes against (1) the adapter's recorded-fixture double and (2) the vendor sandbox, with the
evidence listed in `integration-gates.md` gate G3 and G6. Provider-agnostic. Case ids `PSP-C-nn` are new; existing
test names are cited as they exist at base `12edec0`.

Rules for the suite author:

- A case that cannot run for a real adapter must **fail**, never skip (existing precedent:
  `conformance_skip_to_fail_test.go`, and the `t.Fatalf(... a real adapter cannot skip it ...)` branches in
  `internal/payments/conformance_test.go`).
- Vendor behaviour is taken from recorded vendor-documented examples or sandbox captures with a citation in the test
  (document URL + date or sandbox capture id). A test must not encode an assumed vendor behaviour (CLAUDE.md "no
  invented provider behaviour").
- Fixtures contain synthetic data only; no secret, no real PAN, no real customer data.
- Financial tests end with the ledger invariants (SUM(D)=SUM(C), projection parity, INV-IO-13) as the existing
  integration tests do.

## 1. Existing suites a real adapter must run unchanged

| Suite | Where | Applies as | Work to make it runnable for a real adapter |
|---|---|---|---|
| `RunProviderConformanceSuite(t, factory)` six sub-tests: decline vs ambiguous distinguishable (sync + `QueryStatus`); `HandleCallback` decline/ambiguous; redelivered callback idempotent; credential for tenant A rejected for tenant B; `Capabilities` well-formed (every declared asset has an `AmountLimits` pair); inbound key material rejected | `internal/payments/conformance_test.go` | mandatory | Today four sub-tests `t.Fatalf` unless the provider is `*MockProvider` (they call `mock.CallbackPayload`/`SignRawBody`). **New work (PSP-C-01)**: define a test-only fixture-hook interface (`SignedCallbackFixture`: build a vendor-correct signed inbound for {tenant, event type, outcome, amount, asset, reference, timestamp}) that the real adapter's test package implements; the suite uses it instead of the type assertion. Also the first two sub-tests drive the mock's magic amounts (`MockAmountPlayerDeclineNoCascade` etc.): a real adapter needs a sandbox-scenario map instead (PSP-C-02) |
| `webhookauthtest.RunSchemeConformance` SC1..SC13 with a **vendor known-answer vector** | `internal/webhookauth/webhookauthtest` (`conformance_test.go`, `registry_test.go`) | mandatory; the scheme name must be added to `conformanceManifest` (`scheme.go:341`) in the same change | known-answer vector from a vendor-documented example or a sandbox capture; SC7 timestamp window is fail-not-skip for non-synthetic; SC8 signed-account binding if the scheme declares it. `TestEveryRegisteredNonSyntheticSchemeRunsConformance` fails if the fixture is missing |
| Echo conformance | `echo_conformance_integration_test.go` (`TestEchoConformance_MockSupported`, `_MockUnsupported`) | mandatory for a withdrawal-capable adapter | run with the adapter's declared `DestinationEchoSemantics` |
| `CheckM4SourceContract` C1..C11 on the statement source's declaration | `m4_source_contract.go`, tests `TestD7_*` | mandatory for a payout statement source; necessary, not sufficient | vendor status table, unknown-status policy `reject_import` or `treat_as_pending` (never `treat_as_succeeded`), channel authenticated, content-signature declaration, final-status semantics and reversal window |
| Manifest registration refusals | `capability_manifest_test.go`, `TestE3_ValidateManifest_StatusSuccessMustCarryAmountAndAsset` | mandatory | adapter declares truthfully: `CallbackEchoesMerchantReference=true`, `StatusQuerySuccessCarriesAmountAndAsset=true`, `DestinationEchoSemantics` (withdrawal), `SupportsRefund=false`, `SettlementWindow`, `IdempotentSubmission`, `StatusQuery`, `MerchantLookupAuthoritativeAfter` |
| Webhook retry semantics | `webhookauth/retry_semantics_test.go` | mandatory | declared from the vendor's documented retry policy; neither-429-nor-503 is refused |
| Statement source contract | `statement` tests (`TestPaymentStatementSource_TypeLevelReadOnly`, `payment_page_test.go`, `payment_limits_test.go`), `cmd/platform-api/payment_statement_registry_test.go` (`TestRegistry_*`), `internal/reconciliation/payment_sources_integration_test.go` (`TestScoped_*`, `TestBound_*`) | mandatory | real source registered through the same registry; non-Synthetic requires `ReadOnlyStatementCredential()==true` and `MarkProductionEligible()` |
| Outbound-credential tripwire | `cmd/platform-api/outbound_precondition_test.go` (`TestOutboundPrecondition_EveryWiredAdapterIsSynthetic`) | must be relaxed **deliberately**, scoped to the payments adapter, with `security` sign-off and owner authorization (PROV-OUTBOUND-CRED-1) | a test that proves only the named adapter is exempt |

## 2. Binding rows from the A11 recheck ("binding before the first real PSP")

Source: `docs/plans/a11-payments-binding-recheck.md` (and `a11-payments-wiring-record.md` sections 3, 9, 10). For each
row: the closure condition for the first real adapter, and the case that proves it.

| Row | State today | Condition before the first real PSP | Case |
|---|---|---|---|
| PAY-K3-STATEMENT-SOURCE-WIRING-1 | wired; `IMPLEMENTED` against MOCK | A real source registered via `registerPaymentStatementSources`; coverage check refuses a configured provider with no eligible source (`reconciliation.run_failed`, never clean) | PSP-C-41 |
| R3-OBS-BOUND-1 / R3-FIRST-REAL-SOURCE-READONLY-1 | bound/cursor done for MOCK; read-only attestation is a declaration | Vendor read-scoped credential provisioned and proven read-only (attempt a write/pay-out with it in the sandbox: must be refused by the vendor); first Fetch read-only review by `security` | PSP-C-44 |
| PAY-DEPOSIT-ESCALATION-1 | manifest attestation only | Conformance test that a real `QueryStatus` success carries amount and asset on the wire | PSP-C-12 |
| PAY-SEC-LAUNCH-1 (destination echo) | echo framework done | Real payout rail echo behaviour proven (`DestinationEchoSemantics`) | PSP-C-33 (payout scope only) |
| PAY-PAYOUT-REFBIND-1 | `IMPLEMENTED` against MOCK | A vendor conformance test of payout reference semantics (PSP-issued, stable, never an echo of our merchant reference) | PSP-C-32 |
| PAY-RECON-POLL-REF-CLEAR-1 | against MOCK | Real-PSP poll behaviour (does the PSP reverse after a "Y" poll) observed in sandbox | PSP-C-14 |
| PAY-RECON-PARKED-CAPTURE-STANDING-1 | against MOCK | R-S3: statement carries `merchant_reference` | PSP-C-45 |
| PAY-CALLBACK-MISMATCH-BIND-1, PAY-PAYOUT-UNBOUND-HOLD-1, MA020-SYNC-MISMATCH-1 | against MOCK | real callback shapes drive the same paths | PSP-C-20..26 |
| PAY-PAYOUT-CONTRADICTION-HOLD-1 | (d) decision open | governed resolution for a refused payout park before live payouts | gate G9 input (payout scope), OQ-7 |
| PAY-RECEIPT-T4-DRAIN-TEST-1, PAY-POLL-DECLINED-ALERT-RECON-1 | closed/engineering done | none new beyond PSP-C-20..26 | |
| PAY-FPAY-HARDENING-1 (F-L1), PAY-RECON-D2-HARDENING-1 | (b-open) | optional before a real PSP per the recheck; recommended: make an unclassified disputed deposit reason loud and add the two-tenant same-merchant-reference matcher test **before sandbox reconciliation**, because a real PSP can produce reasons the MOCK never does | PSP-C-46 |
| PROV-OUTBOUND-CRED-1 | payments half done | tripwire relaxation reviewed and scoped; LF-C1 redelivery semantics per adapter (c) | PSP-C-27, section 1 last row |
| F-POOL-2 | against MOCK | LF-C1 per real adapter (PRH-I1-MANIFEST-2) | PSP-C-27 |
| H-W1 second half (gate M2 on last-run success) | OPEN real-money row (LF-A11-8) | owner + ledger-finance decision implemented, or the owner accepts M2-without-last-run-gate in writing | gate G9 precondition (OQ-8) |
| SR11-1 forged payment_statement cursor (HIGH), SR11-10 sealed backing import, SR11-11 | OPEN | resolved before the first non-MOCK statement source (A11 launch flag; readiness report s4) | PSP-C-42 |
| LF-A11-1 matcher windowing under truncation | OPEN, MUST CLOSE | window the missing-record rule by the platform attempt's last state change or defer attempts crossing `CoverageEnd`, with a test that keeps "never silently missing" | PSP-C-43 |
| LF-A11-2 / LF-A11-6 real-source contract (completeness horizon, record-time `OccurredAt`, provider-side ordered paging) | rules written; settle lag NOT IMPLEMENTED | settle lag built (`PeriodEnd = now - lag`); source meets items 6 to 8 of the A11 contract | PSP-C-40, PSP-C-47, PSP-C-48 |
| WD050/WD051 no DB backstop; payout dispatch does not re-run the withdrawal gate (SR6 I-2) | OPEN (readiness s4) | accepted or closed before real payouts | payout scope, G9 |
| T8: payout-reversal approvals proof-bound (ADR 0110 amendment, A5 11.3) | owner-approved 2026-10-16, implementation tracked elsewhere | implemented and merged before any real PSP is activated | gate G0 evidence |

## 3. New cases (adapter-level; no database unless stated)

### 3.1 Contract and classification

| Id | Case | Pass criterion |
|---|---|---|
| PSP-C-01 | Signed-callback fixture hook exists and the four formerly mock-only sub-tests of `RunProviderConformanceSuite` run against the adapter | no `t.Fatalf` of the "cannot skip" kind |
| PSP-C-02 | Sandbox/recorded scenario map replaces mock magic amounts: {definite player decline, definite provider decline, ambiguous (timeout), pending, success} | each scenario yields the exact `Outcome`/`Cascadable`; decline and ambiguous never collapse |
| PSP-C-03 | Every documented vendor status maps to one of the four outcomes; table-driven over the vendor's full status list | complete coverage, no status unaccounted for |
| PSP-C-04 | Unknown / new vendor status -> `Ambiguous` (or import refusal), never `Succeeded`, never `Declined` for a payout | asserted for deposit, payout, callback, `QueryStatus`, statement line |
| PSP-C-05 | Every HTTP/transport outcome is classified into exactly one `ErrorClass`: dial refused before write -> `NotSent`; timeout after write, reset, 5xx, malformed body -> `Ambiguous`; documented 4xx "not processed" -> `NotProcessed` only where the test cites the vendor document; otherwise `Ambiguous` | table-driven; panic in the adapter -> `Ambiguous` via the gate (`gate_panic_redaction_test.go`) |
| PSP-C-06 | `Deposit` never returns `Succeeded` unless the manifest declares `SyncSuccessPossible`, and then returns amount and asset | matches `DepositResult` rules |
| PSP-C-07 | Hostile provider reference (empty on Pending; over the bound; non-UTF-8; control characters) -> `providerref` validation parks `invalid_provider_reference:<reason>` and stores nothing | reuse `TestPayoutAdapterCall_ErrorPathReferenceValidation` pattern for deposit and payout |
| PSP-C-08 | No free-form passthrough; no vendor SDK type outside the adapter package | static import test: nothing outside `internal/payments/<adapter>` imports the vendor client |
| PSP-C-09 | Hosted/redirect only: no code path accepts, logs, stores or forwards a PAN; the adapter's request types have no card-number field | static test + `payoutinstrument` PAN refusal reuse; `security` review item |
| PSP-C-10 | Redaction: `CallContext`, credential, request bodies, vendor errors never render secrets in `String/GoString/Format/LogValue/JSON`, logs or audit | `credential_reflection_test.go` pattern |

### 3.2 Money fields

| Id | Case | Pass criterion |
|---|---|---|
| PSP-C-11 | Minor-unit conversion round trip for currencies with 0, 2 and 3 decimals (use the asset registry exponent); the adapter never uses float | exact equality; rejects an amount the exponent cannot represent |
| PSP-C-12 | `QueryStatus` success always carries amount and asset on the real wire format | fails if either is zero/empty (proves `StatusQuerySuccessCarriesAmountAndAsset`) |
| PSP-C-13 | Amount/asset mismatch on every evidence surface -> dispute park, nothing posted: sync, `QueryStatus`, callback, statement | reuse `TestA5_*` / `TestB5_*` / `poll_amount_integration_test.go` structure |
| PSP-C-14 | `QueryStatus` after a callback-reported decline / success: observe the real PSP and compare with the platform's rules (PAY-RECON-POLL-REF-CLEAR-1 residual) | sandbox capture recorded |
| PSP-C-15 | Gross charged amount is echoed; fees, taxes and conversion are never folded into the echoed amount | fixture with a fee/DCC example when the vendor documents one (G-06) |

### 3.3 Callback authentication, replay, ordering, duplicates

| Id | Case | Pass criterion |
|---|---|---|
| PSP-C-20 | Genuine signed callback verifies; every tampered byte of body and of each auth field is rejected | SC1/SC2 |
| PSP-C-21 | Signature missing / malformed -> uniform 401 `signature_missing/invalid`; no write before verification; identical response for every pre-verification failure | `webhook_no_write_before_verification_integration_test.go`, `payment_webhook_auth_failure_logging_integration_test.go`, `kyc_webhook_indistinguishable_401_test.go` pattern |
| PSP-C-22 | Replay inside the window is inert (idempotent effects); outside `MaxSkew` -> `timestamp_out_of_window`; future-dated beyond skew rejected | SC7 + `webhook_replay_duplicate_integration_test.go` |
| PSP-C-23 | Cross-tenant: a callback signed with tenant A's credential delivered on tenant B's route is rejected; shared-secret configuration is refused | SC3/SC4 + `webhook_tenant_binding_integration_test.go` |
| PSP-C-24 | Key rotation overlap: old key accepted only before `not_after` and only for KeyImplicit | SC9, `scheme_keyimplicit_test.go` |
| PSP-C-25 | Duplicate callbacks (same event twice; same event concurrently) -> exactly one effect, one audit row per receipt; the second is a no-op receipt | `receipt_integration_test.go`, `stage9_concurrency_integration_test.go` pattern |
| PSP-C-26 | **Ordering**: success-before-pending, decline-after-success, reversal-before-success (tombstone), success after decline (T13), success for a never-sent attempt (T15), reversal of an unseen original | reuse `iwire_tombstone_integration_test.go`, `TestB3_*`, `receipt_orphan_resolve_integration_test.go` with the adapter producing the events |
| PSP-C-27 | Redelivery semantics (LF-C1): response code the PSP treats as "stop retrying" and codes it retries on (200/2xx, 401, 429, 503, 5xx) recorded from the vendor doc/sandbox; manifest `WebhookRetrySemantics` equals the recorded facts; if the PSP does not redeliver on 401, a production-eligible statement source for the provider is registered (option b) | declaration test + startup refusal test |
| PSP-C-28 | **Late callbacks**: a callback arriving after `Ambiguous`, after T16 escalation, after a poll-decided state, and after a manual M2 resolution (T7-T9, T13, T13d) | state machine outcomes per ADR 0095 s4.3; P1 where specified |
| PSP-C-29 | Unknown event type, non-final dispute stage, "chargeback won": no evidence applied, P1 unsupported-event raised, 2xx/4xx per `ReceiveVerifiedCallback` mapping | `types.go:661-669` behaviour |
| PSP-C-30 | Body > size cap, wrong content type, wrong encoding, duplicate auth headers | SC5/SC12, `ReasonBodyTooLarge` |
| PSP-C-31 | Merchant-reference echo present on every event; absence -> not convergeable on reference alone (documented behaviour) | proves `CallbackEchoesMerchantReference` |

### 3.4 Payouts (only when payout is in scope)

| Id | Case | Pass criterion |
|---|---|---|
| PSP-C-32 | Payout reference is PSP-issued, stable, not an echo of the merchant reference | PAY-PAYOUT-REFBIND-1 |
| PSP-C-33 | Destination echo per declaration; mismatch/malformed/unknown kid -> `destination_mismatch`; absent when `Supported` -> ambiguous; present when `Unsupported` -> untrusted/fail closed | echo conformance suite |
| PSP-C-34 | Payout timeout after write -> `Ambiguous`, hold kept; `not_found` never releases the hold; only T7/T8/T12 exit automatically | `payout_dispatch_*`, `b11_*` pattern |
| PSP-C-35 | Payout success for a `declined` attempt (double payout) -> T14 park | existing T14 tests |
| PSP-C-36 | Returned/reversed payout: no adapter event; staff reversal route works with a real-shaped return reference; tombstone branch when the return precedes completion | A5 tests; T8 proof binding in place |
| PSP-C-37 | Payout never dispatched without: four-eyes at threshold, KYC/RG/AML gates, kill switch, capability re-read, non-Synthetic verified instrument | `stage9_four_eyes_adversarial_test.go`, `payout_sl4_capability_integration_test.go`, tiering tests |

### 3.5 Statement source (A11 contract)

| Id | Case | Pass criterion |
|---|---|---|
| PSP-C-40 | Settle lag: sweep requests `PeriodEnd = now - lag`; `CoverageEnd` never exceeds the vendor-documented completeness horizon; a line that becomes visible late is never lost | settle lag implemented and tested (currently NOT IMPLEMENTED) |
| PSP-C-41 | Source registered per (tenant, provider) through the one list; a configured provider without an eligible source is an audited failed run + `reconciliation.run_failed`, never clean | `TestRun_StatementSourcesRegisteredAndScheduledFromOneList`, `TestScoped_*` |
| PSP-C-42 | Forged cursor: sealed backing import (SR11-10); forged unbacked run row ignored + `cursor_anomaly` P1; far-future `coverage_end` cannot blind the stream | `TestBound_SR11_*`, `TestBound_ForgedCursorsAreIgnored` plus the SR11-10 sealed-import tests |
| PSP-C-43 | Truncated run: a succeeded attempt whose line settled after the cut does not raise a false `pay_missing_provider_record`, and a genuinely missing record is still found by a later run | LF-A11-1 closure test |
| PSP-C-44 | Read-only credential: the reporting credential is rejected by the vendor for initiate/capture/refund/payout (sandbox); `ReadOnlyStatementCredential()==true`; fetch runs with no tx (`txscope.Held` refuses) | registry tests + sandbox capture |
| PSP-C-45 | Lines carry `MerchantReference`; payout lines declare `PayoutLinesCarryMerchantReference` honestly | R-S3 |
| PSP-C-46 | Unclassified disputed deposit reason is loud at run time; two-tenant same-provider same-merchant-reference matcher test | PAY-RECON-D2-HARDENING-1 |
| PSP-C-47 | `OccurredAt` is the provider's record time and monotonic with reporting; a status change of an earlier-dated line is re-reported in the window where it happened (continuation mode) | recorded sandbox sequence: pending -> succeeded -> reversed |
| PSP-C-48 | Provider-side ordered paging with the tie group kept whole, stop at `MaxLines`; 1M-line/256 MiB caps respected using `LimitPaymentStatementBody`/`PaymentLineCollector`; statement start == `ContinueFrom` | `TestBound_*`, `TestPagePaymentLines_*` against the real source |
| PSP-C-49 | Daily reconciliation of the sandbox run against the ledger is clean: no `pay_*` finding, no drift; any finding stops the run | exit criterion of gate G6 |

### 3.6 Partial failures, timeouts, retries (platform-integrated, with the adapter plugged in)

| Id | Case | Pass criterion |
|---|---|---|
| PSP-C-50 | Crash/failure injection at each phase boundary (after claim, mid-call, after response before phase C, during phase C) with the real adapter's recorded responses | the A/B/C failure matrix of ADR 0095 s16.1 holds; no double post; `ever_possibly_sent` correct |
| PSP-C-51 | Timeout on `Deposit` after write -> `Ambiguous`; poll resolves; T12 resend with the same key produces one provider transaction (idempotency proven in sandbox) | exactly one provider-side transaction |
| PSP-C-52 | Provider health: failing adapter trips the breaker/health state and routing/cascade skip it; ambiguous outcome is never cascaded without a status query | `cascade.go`, `breaker.go` tests with adapter errors |
| PSP-C-53 | Concurrency: two simultaneous callbacks + a poll + a sweeper claim for one attempt -> one terminal state, one posting | `stage9_concurrency_integration_test.go`, `lockorder_*` pattern |
| PSP-C-54 | Kill switch engaged mid-flight: no new call; in-flight attempts drain per runbook (runbook is a G7 deliverable) | `killswitch_integration_test.go` |
| PSP-C-55 | Tenant isolation with a synthetic tenant: credential, webhook, statement, attempts never cross tenants | `attempt_tenant_predicate_integration_test.go`, `prh2_r3_*` pattern |

## 4. Evidence required per case

Test source + the recorded vendor fixture (with its documentation citation or sandbox capture id), command and result
output, run on a private scratch database for integration cases (local PostgreSQL; never shared roles; CLAUDE.md
environment safety; CI is billing-blocked, so local evidence is labelled as such), and a mutation note for any
ledger-critical branch the case claims to cover.
