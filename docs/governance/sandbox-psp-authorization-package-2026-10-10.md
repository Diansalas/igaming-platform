# Sandbox PSP authorisation package (2026-10-10)

Purpose: the exact package the human would need in hand before authorising the FIRST sandbox PSP (B14-B18, sandbox adapter).
**Nothing in this document is authorised, requested or started.** The human has NOT authorised a sandbox PSP (ADR 0095 s48
decision 9) and has NOT authorised AWS (decision 10). No credential is requested, no provider call is made, no sandbox financial
movement is enabled. It complements `sandbox-provider-readiness-gap-analysis-2026-10-10.md` (what is missing) by stating, per
section A-J, the acceptance condition the authorisation should name, the evidence that already exists, and the external input
still missing. Scope of the authorisation being prepared: **one sandbox-only PSP adapter, a synthetic non-real-money tenant,
payout and deposit capabilities as stated, no production credentials, no real customer data** (owner YES in principle on
2026-10-06 per the decision register `SANDBOX-BEFORE-ALERT-DELIVERY-1`; ALERT-DELIVERY-1 stays a production blocker).

The exact minimum request to the human (items A-Q) is `sandbox-psp-authorization-request-2026-10-10.md`; the questions are in `decision-ballot-2026-10-10.md`.
Additional prerequisite for sandbox PAYOUTS: a non-Synthetic payout-instrument verifier (tiering refuses `synthetic` for real adapters; only the MOCK verifier exists). Not needed for a deposits-only sandbox.

Legend: IN PLACE = implemented and tested against MOCK; GAP = missing; EXTERNAL = needs an input from outside the repository;
DECISION = needs a human/security/ledger-finance ruling.

## A. Provider prerequisites

| Requirement | Status | Missing external input |
|---|---|---|
| Provider chosen; sandbox account; written terms permitting sandbox testing | GAP | EXTERNAL (provider, owner commercial) |
| Payout API documentation: request/response, idempotency, error taxonomy, rate limits, status query | GAP | EXTERNAL (provider documentation) |
| Deposit API documentation (if deposits are in scope of the first adapter) incl. hosted fields/redirect only (no PAN) | GAP | EXTERNAL |
| Provider status vocabulary and unknown-status policy | GAP (platform side IN PLACE: unknown never succeeded; M4 contract C3/C4) | EXTERNAL |
| Provider reference semantics: PSP-issued, stable, never echoes the merchant reference | GAP (platform check IN PLACE: contract C9, G-REF) | EXTERNAL |
| Provider tenant context on every call and callback; how the adapter learns the tenant (B13B-8) | GAP | DECISION (architect interface change) + EXTERNAL |
| Destination-echo semantics declared per adapter (Supported / Unsupported) | IN PLACE (mandatory declaration, conformance suite) | EXTERNAL (what the provider can echo); DECISION (acceptability of `Unsupported`) |

## B. Security prerequisites

| Requirement | Status | Missing |
|---|---|---|
| Per-tenant provider credentials via the secret store; sandbox credentials only; read-only/reconciliation-scoped credential where the PSP supports one (HD-CTF-10) | IN PLACE (`providercred`, `secretstore`); PROV-OUTBOUND-CRED-1 tripwire to be relaxed for that adapter only | EXTERNAL credentials; DECISION (authorisation) |
| Outbound allow-list / egress policy for the provider host | GAP | DECISION + AWS/ops (no deployment yet) |
| Callback authentication (S-3): signing scheme, rotation, replay window, timestamp tolerance | GAP (verifier framework IN PLACE: `webhookauth`) | EXTERNAL (provider scheme); security review |
| Statement-import seal (T10) security review of the implementation | IN PLACE (code); review pending | security owner |
| Registration of instruments through a PSP-hosted/tokenised flow only; no raw credential collection (decision 8) | IN PLACE (no UI); flow GAP | EXTERNAL; security/PCI review; D-REG-1 |
| Production-startup refusal of an unacknowledged `Unsupported` adapter (security recommendation) | GAP (recorded, not built) | DECISION (owner defines the acknowledgement) |
| Security sign-off for the adapter diff (secrets scan, SSRF, TLS, logging) | GAP | security owner |

## C. Financial / ledger prerequisites

| Requirement | Status | Missing |
|---|---|---|
| Payout state machine, holds, completion/failure legs, idempotency keys `(provider_id, provider_tx_id)` | IN PLACE (MOCK) | provider-specific mapping |
| Settlement not gated by instrument state; snapshot authoritative | IN PLACE (decision 3) | none |
| Asset/amount equality (I-1), caps (I-2), NET recovery (RR-1) | IN PLACE | none |
| Stranded `approved` withdrawal disposition (HD-R15-5) | GAP | DECISION (owner + ledger-finance); launch-blocking for non-MOCK payout |
| Verification max-age cadence (HD-R15-1) and what counts as verified (HD-R15-2) | GAP | DECISION; launch-blocking for any sandbox/real payout |
| Fingerprint-ownership claim policy (M-3) | GAP | DECISION; launch-blocking for real customer data |
| WITHDRAWAL-REVERSAL-1 path (money back after a provider return) | GAP | DECISION + ledger-finance design; launch-blocking before non-MOCK M4 paid |

## D. Callback prerequisites

| Requirement | Status | Missing |
|---|---|---|
| Callbacks are evidence only; cannot set/replace/change a destination | IN PLACE (static pins) | none |
| Replay/redelivery idempotency, audit-once, receipt attribution (R-0..), orphan/anomaly handling | IN PLACE | provider ordering/retry semantics (EXTERNAL) |
| Echo-bearing receipts never deferred; echo cells on sync/poll/callback | IN PLACE | none |
| Return/reversal/chargeback-of-payout statuses mapped to the contradiction cell | GAP | EXTERNAL; ledger-finance review (ADR 0111 s20.4) |

## E. Reconciliation prerequisites

| Requirement | Status | Missing |
|---|---|---|
| Authenticated, complete, sealed payout statement source; declares merchant-reference carriage; coverage/pagination completeness | GAP (framework + M4 contract checklist IN PLACE) | EXTERNAL source; T10 review; S-3 |
| STANDING-1, BOUND-CLEAR-1, R-1, RR-1, MA020, fail-closed joins | IN PLACE | none |
| Statement lines carrying a destination (positive destination evidence for M4 paid) | GAP (design recorded, ADR 0111 s25.7) | DECISION + migration |
| `evidence_ref_hash` binding (digest vs blind entry) | GAP | DECISION (owner + security) |
| Per-source clock-skew tolerance (<= 5 min, lower bound only) | GAP (contract item C11 IN PLACE, default 0) | ledger-finance review per source |

## F. Test / conformance prerequisites

| Requirement | Status | Missing |
|---|---|---|
| Echo conformance suite run against the adapter | IN PLACE (reusable) | adapter |
| M4 source contract checker (C1-C11) run against the source | IN PLACE (reusable; necessary, not sufficient) | source |
| Provider-specific tests: status mapping, callback replay/ordering, reference format, amount/asset fields, error taxonomy | GAP | EXTERNAL docs |
| Local evidence policy: GitHub CI is blocked by billing (CI-BILLING-1); only local evidence exists; unit + full payments integration + targeted -race + lint + frontend as in the final verification | IN PLACE | DECISION (owner-ruled CI equivalent for production) |

## G. Alerting prerequisites

| Requirement | Status | Missing |
|---|---|---|
| B12 raise-only alerts with dedup, raise-last, audit-once | IN PLACE | none |
| Real recipients, on-call, channel, routing (ALERT-DELIVERY-1 / HD-PRH2-4-OPS) | GAP | DECISION (owner); AWS-hosted channels. Sandbox may proceed before it (owner YES in principle); PRODUCTION blocker |
| Drift/untrusted-declaration and invisible-evidence signals visible to ops | IN PLACE (log + P1) | delivery |

## H. Rollback / reversal prerequisites

| Requirement | Status | Missing |
|---|---|---|
| Kill switch (payment kill switches, tenant/brand non-active gates H-SEC-5/11, H(8)) | IN PLACE | none |
| Governed exits for parked payouts: M4 (paid/not-paid), HSEC release, integrity not-paid exit | IN PLACE (MOCK); non-MOCK M4 BLOCKED | D-7 provider parts, T10, S-3 |
| Provider return/reversal handling and WITHDRAWAL-REVERSAL-1 | GAP | see C |
| Rollback of the adapter itself (disable capability per tenant, drain in-flight attempts) | PARTIAL (capability write + kill switch) | runbook for in-flight attempts at disable time (GAP, doc) |

## I. Monitoring requirements

| Requirement | Status | Missing |
|---|---|---|
| Alerts kinds for destination mismatch, post-M4 contradictions, receipt anomalies, reconciliation findings, evidence overflow/invisible | IN PLACE | delivery (G) |
| Credit-after-RR-1-stop monitoring | GAP (recorded follow-up) | needs a real recovery flow to tune |
| Standing R-1 predicate for a post-execution recorded success | GAP (deferred) | DECISION (must be stoppable by net recovery alone) |
| Operator read views of park/attempt state | GAP | DECISION (D-OPS-1/2, operator console brief) |
| Metrics for provider latency/error rates per tenant | GAP | adapter-specific |

## J. Evidence and audit requirements

| Requirement | Status | Missing |
|---|---|---|
| Audit rows for every mutating administrative/financial action; append-only; signed-actor proof for governed flows | IN PLACE | none |
| Park evidence (`payout_park_evidence`), snapshot write-once, statement seals | IN PLACE | none |
| Sandbox test-run evidence: per-scenario logs, ledger invariants (SUM(D)=SUM(C), projection parity), reconciliation clean, no real money, synthetic tenant only | GAP | the sandbox run itself (after authorisation) |
| Written ledger-finance and security sign-off on the adapter and mapping | GAP | reviewers, after the adapter exists |

## What the human is being asked to authorise (when ready)

1. Start ONE sandbox-only PSP adapter (B14-B18) for a synthetic non-real-money tenant, with the provider named.
2. Name the decisions that must precede the first sandbox payout: HD-R15-1, HD-R15-2, HD-R15-5, M-3 (or explicitly scope the sandbox to deposits + MOCK-equivalent payout until they are made), `Unsupported`-echo acknowledgement, `evidence_ref_hash` binding, D-REG-1.
3. Confirm that ALERT-DELIVERY-1 remains a production blocker only.
4. Authorise the PROV-OUTBOUND-CRED-1 tripwire relaxation scoped to that adapter.
Nothing else (AWS, real money, real customer data, production credentials) is part of this authorisation.

## AWS gate (not authorised; prerequisites only)

Human authorisation to deploy/enable AWS; staging deployment from the final approved commit with PLAT-ROLESPLIT-1 verification;
AWS human actions (ACCESS-ANALYZER-CHECK-1, DEPLOY-FPKEY-1, HD-10.3-2); hosting AUP; backup/DR with a tested restore (RPO/RTO
evidence); real alert recipients/on-call channels (ALERT-DELIVERY-1); secret-store wiring with human-supplied credentials;
WEBHOOK-EDGE-1, CAS-RECON-SCALE-1, DEVOPS-0107-INDEX-WINDOW-1; green CI evidence or an owner-ruled equivalent. No Terraform or
deployment change is made by this document.
