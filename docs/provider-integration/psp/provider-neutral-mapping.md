# Provider-neutral mapping: platform requirement -> PSP supply -> neutral interface -> gap

Status: **SPECIFICATION (`NOT IMPLEMENTED` as an adapter).** Provider-agnostic: applies to Nuvei or any other fiat PSP.
Every "PSP must supply" cell is a requirement on the vendor, not a statement of what any vendor does; where this file
mentions Nuvei it says `UNVERIFIED` and points to the registry in `nuvei-evaluation.md` (N-Unn). Code references were
read at base `12edec0` of `claude/focused-wright-jw88w9`.

Column legend: **Interface** = how the neutral interface carries it today. **Gap** = what is missing or at risk;
`PW` platform-wide (any real PSP), `PS` provider-specific; reviewer in brackets. `G-nn` = platform gap id (section 12).

## 1. Deposit initiation

| Platform requirement | PSP must supply | Interface (code) | Gap / UNVERIFIED |
|---|---|---|---|
| Three-phase call: A (claim, tx), B (provider call, **no tx**), C (apply evidence, tx). INV-IO-1 | A request that is safe to issue exactly once per claim and tolerant of the T12 resend with the same key | `deposit_v2.go` (`InitiateDepositAttempt`), `drive.go` (`depositAdapterCall`, T2/T4/T5/T6), `gate.go` (provider-call gate; refuses under `txscope.Held`) | none platform-side |
| No raw PAN; hosted fields or redirect only | A hosted page / redirect or hosted-fields token | `DepositResult.RedirectURL` or `HostedFieldToken` (`types.go:491-492`); `OperationManifest.Interactive=true` (`contract.go:165`) | N-U01..03 |
| Our reference is carried to the PSP | A field that holds `MerchantReference` and is returned on notifications and report lines | `DepositRequest.MerchantReference` (`types.go:462`), `PaymentAttempt.MerchantReference`, `ExternalIdempotencyKey` (`pa:<id>`) | N-U04, N-U15; registration requires `CallbackEchoesMerchantReference` (`capability.go:394`) |
| PSP-issued stable provider reference at return of `Deposit` | One id that stays the same across initiation, notification, status query and report | `DepositResult.ProviderReference` required on Pending/Ambiguous (`types.go:483-487`); reference validated by `internal/providerref` before any state change (invalid -> T10 `invalid_provider_reference:<reason>`, `drive.go:408`) | **G-03** (PW): a session/order token that is replaced by a transaction id later; N-U20 |
| Decline carries cascade-eligibility | Machine-readable decline reason class: issuer/player-specific (do not cascade) vs provider/route-specific (cascade) | `DepositResult.DeclineReason`, `Cascadable` (adapter-declared taxonomy, `types.go:493-499`); cascade logic `cascade.go` `cascadeEligible` (max depth, not for ambiguous) | PS: the decline-code -> `Cascadable` table is the adapter's; must be reviewed by `payments` + `ledger-finance` (a wrongly cascadable decline can double-charge across two PSPs) |
| Sync success is rare / forbidden | Initiation never reports success; success arrives by notification/status | `DepositResult` doc: not `succeeded` from initiation; `SyncSuccessPossible=false` default | If the PSP can return a final approval in the initiation response, the adapter must still report `Pending` and let the callback/poll decide, or declare `SyncSuccessPossible` and echo amount+asset (`types.go:501-512`) |

## 2. Callbacks / notifications (DMN or webhook)

| Platform requirement | PSP must supply | Interface (code) | Gap / UNVERIFIED |
|---|---|---|---|
| Authenticated before any read or write | A MAC/signature over the raw request bytes with a per-merchant secret | `webhookauth.VerificationScheme` (`scheme.go:303`); `ExtractInbound`/`VerifyInbound`; route `POST /v1/webhooks/payments/{tenantSlug}/{providerID}`; `Orchestrator.VerifyCallback` (`webhook_verify.go:79`); no write before verification: `webhook_no_write_before_verification_integration_test.go` | N-U09 |
| Signature material in headers | The signature and key id in HTTP headers | `Extract` "parses and format-validates the auth headers only ... no body parsing" (`scheme.go:306-314`) | **G-01** (PW): signature inside the body or query string cannot be extracted under the current contract. Options: (a) vendor/relay supplies headers (not acceptable if it needs infrastructure we do not have), (b) amend ADR 0022 s3 so `Extract` may read a bounded, canonical body prefix, **security review required**. Not decided here |
| Signed timestamp, replay window <= 10 min | A timestamp inside the MAC input | `ValidateProperties`: non-synthetic must `SignedTimestamp`, `MaxSkew <= MaxSkewCap` (10 min) (`scheme.go:241-263`, `:41`) | **G-02** (PW, potentially **blocking**): a PSP whose notification signature has no time component cannot be registered. Options: ADR 0022 amendment allowing an `IdempotencyOnly` replay defence for a named non-synthetic scheme with compensating controls (statement backstop + idempotent effects), or reject that PSP. `security` + `architect` decision. N-U10 |
| Tenant binding | Per-tenant secret (a separate PSP merchant/site per tenant) and ideally a signed account id | `BindingPerMerchantKey`, `BindingPerMerchantKeySignedAccount` + `Credential.BoundAccountID`; `webhook_tenant_binding_integration_test.go`; `payment_webhook_tenant_binding_test.go` | Commercial: one merchant/site per tenant. A shared account cannot bind a tenant and is refused. N-U11 |
| Key selection | Key id on the request (`KeyFromHeader`) or none (`KeyImplicit`, active + one verify-only predecessor) | `KeySelection` (`scheme.go:127-148`) | adapter-declared |
| Retry/redelivery declaration | Documented retry policy | `WebhookRetrySemantics` (`retry_semantics.go`), `RequireRetrySemantics` at orchestrator construction; ADR 0095 s6.6 `RedeliveryOn401/5xx` NOT YET a manifest field (deferred, `contract.go:135-148`) | N-U13. A PSP that retries neither 429 nor 503 is **refused at registration** (`retry_semantics.go:94`); the LF-C1(b) backstop is not built (PRH-I4-T6-EXTEND-1) |
| Verified-but-malformed body | Stable schema | `ErrCallbackMalformedBody` (4xx), pre-verification failure is the uniform 401 (`types.go:113-141`) | adapter |
| Key-material scan | n/a | `ErrInboundKeyMaterial` (`types.go:105-111`) | adapter must call the scan after verification (conformance case in the existing suite) |
| Duplicate / concurrent / out-of-order delivery | At-least-once delivery is assumed; ordering is NOT assumed | Receipt dedupe, no-op evidence (ADR 0095 s6.3), `webhook_replay_duplicate_integration_test.go`, `receipt.go`, deferred receipts applied at T4/T9 (s6.4) | N-U14 |
| Unknown event type | n/a | `ReceiveVerifiedCallback` default case: P1 "unsupported callback event type", no evidence applied (`types.go:661-669`) | desired behaviour for non-final dispute stages and "won" chargebacks |

## 3. Status mapping (provider status -> platform attempt state)

The adapter owns the table; the platform owns the state machine (`attempt.go:36-45`, ADR 0095 s4). The table below
is the **template the adapter must fill from the vendor's real status list** (N-U17). Rule 0 applies to every row:
**an unknown or unmapped status is never `succeeded`**; it maps to `ambiguous` (poll again) or to an import/dispatch
refusal (`M4UnknownRejectImport`), never to `declined` for a payout (ADR 0095 s4.5).

| Vendor meaning (to be filled) | Adapter returns | Attempt transition | Notes |
|---|---|---|---|
| Session created / awaiting player / 3DS in progress | `Pending` with reference | `submitting -> pending` (T4), poll or T16 escalation after `SettlementWindow` | abandoned challenge = stays pending, escalates (no state change) |
| Authorised, not yet captured/settled | `Pending` (**not** `Succeeded` unless the vendor documents that money is guaranteed ours) | pending | **Decision per vendor** (open-questions OQ-5): posting the ledger credit is irreversible in the sense that a later decline is a Flow 2 reversal |
| Captured / approved (final) with amount+currency | `Succeeded` + amount + asset | T7: compare amount/asset (`amount_evidence.go`), post Flow 1 `provider_tx_id=provider_reference` | mismatch -> T10 dispute |
| Declined by issuer / risk / 3DS failure | `Declined` + reason + `Cascadable` | T8 (+ cascade T1 if eligible) | decline reason text is stored bounded (`canonicalDeclineReason`, `payout.go:1108`) |
| Timeout / 5xx / unknown status / malformed | `Ambiguous` | T6 / stays ambiguous, poll, T12 resend if `IdempotentSubmission` | never a decline |
| Not found on lookup | `Ambiguous`, unless the manifest declares `MerchantLookupAuthoritativeAfter` and the deposit rules of s4.5 hold | T11 or authoritative `declined(not_received)` | merchant-reference lookup is not in the interface (A7) |
| Refunded / charged back (final) | `deposit_reversal` event, outcome succeeded (or legacy declined carrier) | no attempt state change; ledger reversal `reverses_transaction_id` (Flow 2) | whole-deposit only (`ErrDepositAlreadyReversed`) |
| Chargeback inquiry / representment open | not a reversal event; mapped to ignore-with-audit or unsupported-P1 | none | `types.go:596-613` |
| Chargeback won / reversal of a reversal | **not** `deposit_reversal`; unsupported-P1 | none | `types.go:661-669`; **G-05** |
| Payout accepted / queued | `Pending` | `submitting -> pending` | |
| Payout paid | `Succeeded` + amount + asset (+ `DestinationEcho` if supported) | T7 payout: `withdrawal.Complete` (Flow 3 step B) | |
| Payout rejected before money left | `Declined` (definite) | T8 payout: `withdrawal.Fail`, hold released | only on verified evidence kind sync/callback/query_status |
| Payout returned / reversed after paid | **no adapter event type exists** | A5 governed route records it (`POST /v1/admin/withdrawals/{id}/payout-reversal`) | PW gap, NOT IMPLEMENTED (A5 1g); N-U07 |

Parked (terminal-for-automation) reasons the PSP's behaviour can trigger (closed sets in code; the adapter must not invent new ones):

- Deposit `disputed` reasons: `multiple_success_for_intent`, `reversal_tombstone_precedes_success`, `sync_amount_mismatch`, `provider_reference_conflict`, `invalid_provider_reference[:<reason>]`, `poll_amount_mismatch`, `poll_reference_mismatch`, `callback_amount_asset_mismatch`, `success_for_never_sent_attempt` (`deposit_terminal_reasons.go:28-39`, `drive.go:408-423`).
- Payout parks: the `PayoutDisputeReasons()` set (`manual_resolution.go:149`), including `destination_mismatch`, `destination_integrity_failure` (`payout_destination.go:56-60`), amount/asset mismatch, reference conflict, tombstone, invalid reference.
- Every park raises a P1 (durable alert), keeps any payout hold, and exits only through governed M1/M2/M4 (`manual_resolution.go`, `manual_resolution_m4.go`); **non-MOCK M4 is BLOCKED** (G-NONMOCK, `m4EligibilityRefusal`).

## 4. Idempotency

| Platform requirement | PSP must supply | Interface | Gap |
|---|---|---|---|
| Database-enforced idempotency on `(tenant, provider_id, provider_tx_id)` for every financial write (CLAUDE.md) | A stable per-transaction id | ledger unique constraint; `payment_attempts_tenant_provider_ref` unique; receipt dedupe | G-03 if the id changes |
| Resubmission safety (T12) | The create call is idempotent on our key (`pa:<id>`) or the duplicate is detectable and answers with the original | `OperationManifest.IdempotentSubmission`; registration refuses a payout `NotProcessed` mapping without it (ADR 0095 s8) | N-U18; if unsupported: set `IdempotentSubmission=false`; ambiguous attempts then resolve only by poll/callback (never resend) |
| Error classification | Every response mapped to exactly one `ErrorClass` (`NotSent`, `DefiniteDecline`, `Ambiguous`, `Pending`, `Succeeded`; `NotProcessed` only where the vendor **documents** it, citing the doc) | `contract.go:30-61`; ADR 0095 s8 | The vendor error table (N-U17/N-U18) is needed to classify each code; anything undocumented is `Ambiguous` |
| Client retries | Same-key retries do not double-charge | `httpclient` retry only if `IdempotentSubmission` (ADR 0095 s8) | N-U18 |

## 5. Withdrawal / payout flow

Platform flow (verified): request -> `pending_review` -> approval (four-eyes at/above the tenant policy threshold) ->
`approved` -> staff submit -> payout KYC/RG/AML gates + kill switch + capability re-read **in the claim tx** (T1p,
`payout.go:ClaimForDispatch:307`) -> provider call -> evidence (`ApplyPayoutResult:699`, `PollPayoutStatus:1622`) ->
`Complete` / `Fail` / park. The platform never auto-pays: dispatch is always a staff action after approval; a request
at or above the tenant threshold needs `RequiredApprovals` (default 2) distinct humans (`withdrawal/policy.go:37-54`).

| Platform requirement | PSP must supply | Interface | Gap |
|---|---|---|---|
| Pay only to the bound, snapshotted instrument | Acceptance of a destination in the vendor's own form (token, IBAN, wallet id) | `WithdrawRequest.Destination` (decrypted `PayoutDestination`, redacted under every rendering), built in phase B from the snapshot (ADR 0111 2.7); `payoutinstrument` kinds `bank_account`, `card_token`, `ewallet_account`, `crypto_address` (open set) | N-U05: payout capability and instrument kinds. B2C payout-instrument **registration is deferred** (owner decision 12, 2026-10-15) until PCI/PII review, and a **non-Synthetic payout-instrument verifier** is required before any real payout (tiering refuses `synthetic`; sandbox-package A). Therefore the first adapter should be **deposits-only** (Option 1 of the sandbox request) |
| Destination echo | Declared `Supported` (fingerprint computed inside the adapter with the injected `DestinationFingerprinter` under the snapshot's kid) or `Unsupported` (no echo, an echo that arrives is untrusted) | `OperationManifest.DestinationEchoSemantics` mandatory, zero value refused (`contract.go:188`, `capability.go:371`); `WithdrawResult.DestinationEcho`; mismatch -> park `destination_mismatch` | N-U05: what the vendor can echo. `Unsupported` needs an owner acknowledgement (undefined yet, sandbox request L) |
| Callbacks never change the destination | n/a | static pins; `b13b_static_test.go` | none |
| Amount/asset on payout success | Amount + currency in the success response, status query and notification | `StatusResult.Amount/AssetCode`, `CompareProviderAmount` | N-U16/19 |
| Payout failure never assumed | n/a | ADR 0095 s4.5; `not_found`/timeout/ambiguous never release the hold | adapter must not map "not found" or "unknown" to `Declined` for a payout |
| Four-eyes above threshold, KYC gating, velocity | Nothing from the PSP | `withdrawal/policy.go`, `Approve` (`withdrawal.go`), `rggate.go`, `kycgate.go`/`payout.go:evaluatePayoutGate`; velocity = RG/AML holds (`aml_hold_*`) | none; the adapter is called only after these gates in the claim tx |
| Staff cannot be the beneficiary | n/a | S-L1 `withdrawal.ErrStaffIsBeneficiary` | none |

## 6. Destination handling: echo and binding rules (consolidated)

1. The destination is chosen by the platform (bound instrument -> immutable snapshot -> attempt). The PSP never
   proposes, replaces or edits it (ADR 0111 s18, B13B-15).
2. The adapter computes any echo **inside the adapter** from the vendor's response using the injected fingerprinter;
   it cannot read the platform fingerprint or keys (`destination.go:107`).
3. Echo cases: supported + present + equal = match; supported + absent = success is `Ambiguous`; present + different /
   malformed / unknown kid = park `destination_mismatch`; unsupported + present = untrusted, fail closed.
4. Statement lines carry no destination field (`PaymentStatementLine`, pinned by `TestD7_StatementLineHasNoDestinationField_PinsTheConstant`); positive destination evidence on statements is a recorded GAP (G-DEST, ADR 0111 s25.7, decision + migration needed) and matters only for M4 (blocked for non-MOCK anyway).
5. Existing suite to run: `echo_conformance_integration_test.go` (`TestEchoConformance_MockSupported`/`MockUnsupported`).

## 7. Reconciliation and the statement source (A11 contract)

| A11 rule | PSP must supply | Interface | Gap / status |
|---|---|---|---|
| Source shape: no tx, read-only by construction | A fetch path that needs no platform tx | `statement.PaymentStatementSource` {Label, ProviderID, Fetch}; method set pinned by `TestPaymentStatementSource_TypeLevelReadOnly` | none |
| Read-only credential, `ReadOnlyStatementCredential()==true`, refused otherwise (everywhere) | A reporting-scoped credential distinct from the payment/payout credential | registry `cmd/platform-api/payment_statement_registry.go` (`registerPaymentStatementSources`); `statement.ReadOnlyAttested` | **G-04** (PW): `providercred` has only purposes `webhook_verify` and `outbound_api` (`providercred.go:44-45`); the mock statement source resolves the **outbound** credential through the gate with `ReadOnly:true` (`mock_statement_source.go:113`). A real read-scoped credential needs either a third purpose (migration CHECK + ADR 0093 amendment) or a distinct handle convention; `security` + `architect`. N-U27 |
| `OccurredAt` = provider's **record time** of the status change, never a value/effective date (LF-A11-2b) | Per-line record timestamp, monotonic with reporting | `PaymentStatementLine.OccurredAt` (`statement.go:231`) | N-U24; **G-07**: a daily settlement file dated by settlement/value date violates this |
| `CoverageEnd` never beyond the provider's completeness horizon; sweep requests `PeriodEnd = now - settle lag` (LF-A11-2a) | Documented reporting delay | `PaymentFetchRequest.PeriodEnd`; **per-provider settle lag NOT IMPLEMENTED** | prerequisite to build before the first real source (conformance PSP-C-40); N-U26 |
| Time-ordered provider-side paging, tie group kept whole, stop at `MaxLines` (LF-A11-6) | Server-side sort/limit by the record timestamp | `PaymentFetchRequest.MaxLines/ContinueFrom`, `PaymentStatement.Truncated`, `statement.PagePaymentLines`; default `RECONCILIATION_PAYMENT_STATEMENT_MAX_LINES=50000` | N-U25; collect-then-page is forbidden (hits the 1M-line / 256 MiB cap and sticks the cursor) |
| Statement start == `ContinueFrom` exactly; no line earlier | n/a | `validateBoundedWindow` (`internal/reconciliation/payment_statement.go:480`) | adapter test |
| Body/line caps | n/a | `LimitPaymentStatementBody`, `PaymentLineCollector`, `DecodePaymentStatementJSONLines`; `MaxPaymentStatementLines=1_000_000`, `MaxPaymentStatementBodyBytes=256 MiB` | adapter must use them |
| Lines carry `MerchantReference` (R-S3, payout lines declare `PayoutLinesCarryMerchantReference`) | Our reference on every line | `PaymentStatementLine.MerchantReference` | N-U28 |
| Single amount + asset per line, minor units | The charged amount and currency | `PaymentStatementLine.Amount/AssetCode` | **G-06** (PW): if the report is in settlement currency (after conversion/fees), the line cannot represent both; matcher would report a mismatch |
| Forged-cursor protection (SR11-1) | n/a | A11 mitigation only: backed-by-import cursor + `cursor_anomaly` P1; **not protected if the import row is forged** | **OPEN HIGH, binding before the first non-MOCK source**; next step SR11-10 (sealed backing import) |
| Matcher windowing under truncation (LF-A11-1) | n/a | false `pay_missing_provider_record` P1 in a truncated run | **OPEN, binding** |
| H-W1 second half: gate M2 on "last run for (tenant, provider) succeeded" | n/a | durable data exists (`reconciliation_runs.provider_id/coverage_end`) | **OPEN real-money row (LF-A11-8)**; owner + ledger-finance |
| Findings vocabulary | n/a | `pay_missing_provider_record`, `pay_missing_platform_record`, `pay_status_mismatch`, amount/asset mismatch (`internal/reconciliation/payment_statement.go`) | none |

## 8. Disputes and chargebacks (deposits)

| Requirement | PSP must supply | Interface | Gap |
|---|---|---|---|
| Final debit -> Flow 2 reversal, original resolved through the attempt's own `ledger_transaction_id`, never the intent's | `original_provider_reference` on the reversal event; the reversal's own reference | `CallbackEvent{EventType: deposit_reversal, ProviderReference, OriginalProviderReference}`; ADR 0095 s5.4 | N-U07 |
| Unseen original -> tombstone (late original rejected) | n/a | existing tombstone branch; `INV-PAY-REV-1` | none |
| Second reversal of the same deposit rejected with audit | n/a | `ErrDepositAlreadyReversed` / `deposit.reversal_rejected` audit | **G-05**: partial refunds and multi-stage chargebacks (second presentment, arbitration) arrive as a second reversal reference -> rejected, P1 |
| Chargeback won / inquiry | n/a | unsupported-event P1 | **G-05**; ledger-finance decides whether to add a canonical "dispute lifecycle" event or keep P1 + manual |
| Negative balance after a chargeback | n/a | A5: wallet negative-balance block in the RG seam (`CodeWalletNegativeBalance`), cleared by balance >= 0 | owner Q2/LF-A5-4 (recommendations in the A5 record) |

## 9. Payout reversal (A5) and T8

| Item | State | Mapping |
|---|---|---|
| Governed payout reversal (compensating `withdrawal_reversed`, E11 `completed -> reversed`) | IMPLEMENTED against MOCK/local DB (A5) | `internal/withdrawal/payout_reversal.go`, `internal/payments/payout_reversal.go` `DecidePayoutReversal`, route `POST /v1/admin/withdrawals/{id}/payout-reversal`, migration 0162 |
| Provider-initiated "payout returned" event | NOT IMPLEMENTED (A5 1g); staff record the return. A future adapter event feeds the **same** decision path (A5 Q6) | needs a canonical event design: **PW**, ledger-finance |
| Partial / different-asset return | NOT IMPLEMENTED (refused 409) | PW |
| Four-eyes on reversal | `reverse` decisions need `max(2, policy)` distinct Persons at any amount, digest-grouped (WD026) | done; **NOT bound to the ADR 0110 signed actor proof (R1 / SR5-2 / LF-A5-2)**: **T8 amendment = proof-bound `reverse` decisions** (A5 record 11.3: operation `withdrawal_payout_reversal:decide`, trigger on `withdrawal_approvals`, WD026/WD027 count proof-bound rows only). Owner decision 2026-10-16 item (2) (human-decision-register) APPROVED the T8 amendment: payout-reversal approvals bound to the signed actor proof **before any real PSP is activated**. Status of the T8 implementation is tracked on its own branch, not here. The decision text says "activated"; this package reads it conservatively as a gate before the first adapter is enabled against any real PSP endpoint, sandbox included, until the owner says a deposits-only sandbox may precede it (open-questions OQ-6) |
| Statement view of a returned payout | R9: a succeeded statement line for a tombstoned reference is silent | open with R6; before real money |

## 10. Amount and asset mismatch

Closed paths, all `IMPLEMENTED` against MOCK: sync (`sync_amount_mismatch`), poll (`poll_amount_mismatch`, `poll_evidence.go`), callback (`callback_amount_asset_mismatch`), payout amount dispute (`PayoutDisputeReasons`, tested in `a5_amount_dispute_integration_test.go`), statement (`pay_*` mismatch). Governed exit for payout mismatch: M2 with basis `provider_confirmed_out_of_band` only (migration 0163). A zero amount or empty asset in provider evidence is "missing", not "mismatch" -> ambiguous, poll decides (`amount_evidence.go`); this is why `StatusQuerySuccessCarriesAmountAndAsset` must be true. PSP supply: amount+currency on every success surface. Gap: **G-06** settlement currency; PS: rounding/fees must never be folded into the echoed amount (the adapter must return the gross charged amount).

## 11. Four-eyes, audit, kill switch, credentials

| Requirement | Mapping |
|---|---|
| Four-eyes above a configurable amount (withdrawals) | `withdrawal/policy.go` (`ThresholdAmount`, `RequiredApprovals`, `RequireStepUp`), `withdrawal.go` approve path; manual force-resolve M2 four-eyes with signed actor proof (ADR 0101, 0110); reversal four-eyes (A5) |
| Audit of every mutating financial action | transition audit rows in the same tx (`payment.attempt_*`), `withdrawal.*`, `deposit.*`; append-only (ADR 0013); callback receipts stored with attribution; free-text notes are an open DPO question |
| Kill switch / disable | `payment_kill_switches` evaluated inside the claim statement (ADR 0095 s10.3); per-tenant capability `status`; tenant/brand non-active gates (H-SEC-5/11) |
| Per-tenant credentials | `providercred` handles (ADR 0093): `outbound_api` and `webhook_verify` purposes, tenant-namespaced `secret_ref`, resolved outside any tx; `CallContext.Credential` redacts on every rendering; **PROV-OUTBOUND-CRED-1 tripwire** (`cmd/platform-api/outbound_precondition_test.go`) must be relaxed deliberately for the first non-MOCK adapter with security sign-off (owner authorization too, sandbox request item 5) |
| Merchant/site ids | **G-08**: one secret + one `vendor_account_id` per handle; a PSP with merchant id + site id + secret needs either a composite `vendor_account_id` (adapter-private encoding, brittle) or non-secret per-tenant provider configuration. Non-secret ids are not credentials and can live in tenant provider configuration; `security` to confirm |

## 12. Platform gaps found (for the architect, `security`, `ledger-finance`; no core change is made here)

Each: scope, what breaks, flag, minimal proposal. None is proposed as a change unless the review in gate G3/G5 confirms the PSP needs it.

| Id | Gap | Scope | Blocking? | Flag | Minimal proposal (only if confirmed necessary) |
|---|---|---|---|---|---|
| G-01 | `VerificationScheme.Extract` is header-only and body-free. A vendor that places signature (and key id) in body fields or the query string cannot register | PW | Potentially (depends on N-U09/N-U12) | security, architect | Amend ADR 0022 s3: allow `Extract` to read a bounded, canonically parsed view of the body, or a per-scheme `Canonicalise` step before extraction, with SC5 fuzz coverage. Keep MAC over raw bytes where the vendor allows |
| G-02 | A non-synthetic scheme **must** sign a timestamp (ADR 0022 s3 pt 10). A vendor checksum with no time component is unregistrable | PW | Potentially (N-U10) | security, architect | Either reject that PSP; or an ADR amendment permitting `ReplayIdempotencyOnly` for a named non-synthetic scheme only with compensating controls (all effects idempotent, statement backstop with `RedeliveryOn401` declared, rate limiting, an IP allow-list at the edge). Owner/security decision (OQ-3) |
| G-03 | Two-reference lifecycle: a hosted-page session/order token known at `Deposit` time may differ from the transaction id in notifications and reports. The platform binds `provider_reference` once; a different one parks `provider_reference_conflict` | PW (likely for any hosted-page PSP) | Potentially | ledger-finance | Prefer an adapter-side choice of one reference that the vendor repeats everywhere (order/client-request id). If none exists: a documented "alias reference" binding rule (ledger-finance design). Do not loosen the conflict park |
| G-04 | No credential purpose for a read-only statement credential (`providercred` purposes: `webhook_verify`, `outbound_api`); the statement source resolves the outbound credential | PW | Yes for the first real statement source (A11 rule) | security, architect | Add purpose `statement_read` (migration CHECK + ADR 0093 amendment) or a distinct-handle rule enforced by the registry |
| G-05 | Dispute lifecycle: only a final whole-deposit reversal is representable; partial refund/chargeback, representment, "won" are P1/rejected | PW | No for sandbox; yes for real money volume | ledger-finance | Keep P1 + manual until volumes justify a canonical dispute-event design; define the unsupported-event runbook first |
| G-06 | Statement line has one amount + asset; settlement-currency reports cannot be matched | PW | Depends on N-U22/N-U28 | ledger-finance | Require the report to show charged currency; else add optional settlement amount/asset to the line |
| G-07 | File-based settlement reports (daily files dated by settlement/value date) do not satisfy record-time `OccurredAt` and provider-side paging | PW | Depends on N-U23..25 | ledger-finance, payments | Use an API report if one exists; else a per-provider settle lag larger than the file cadence and an adapter that stamps the file's availability time (**only with ledger-finance approval**: it weakens LF-A11-2b) |
| G-08 | Credential model has one secret and one `vendor_account_id` | PW (small) | No (workaround exists) | security | Non-secret per-tenant provider configuration for merchant/site ids |
| G-09 | Per-provider settle lag (`PeriodEnd = now - lag`) is NOT IMPLEMENTED | PW | Yes before the first real statement source (LF-A11-2a) | payments + ledger-finance | Build with the adapter; conformance PSP-C-40 |
| G-10 | `QueryStatus` takes only a provider reference; no merchant-reference lookup (A7) | PW | Only if the vendor's status lookup is by merchant/client order id only | payments, architect | Add `QueryStatusByMerchantReference` to the interface (additive), only if N-U19 shows it is the sole lookup |
| G-11 | `OperationManifest` lacks `RedeliveryOn401/5xx`, `ErrorClassMapping`, `StatementSource` fields (deferred, `contract.go:135-148`) | PW | Fields become necessary when a real adapter exists | architect | Add with the adapter, each with its enforcement point |

## 13. What must be true before any of this is real

See `integration-gates.md`. In short: documentation review (G1), adapter build against MOCK-equivalent interfaces (G2), conformance (G3), `security` and `ledger-finance` review (G4, G5), sandbox on a synthetic tenant (G6), operational acceptance (G7), explicit production authorization (G9). No real money until G9, in writing, from the owner.
