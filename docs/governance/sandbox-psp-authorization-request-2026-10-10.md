# Sandbox PSP authorisation request (items A-Q) - 2026-10-10

Status: **REQUEST FOR HUMAN DECISION. NOT AUTHORISED. Nothing here is started.** No provider is chosen, no credential is
requested, no AWS resource is created, no provider call is made, no non-MOCK money movement is enabled. Each line states what the
human would be authorising, what the code already enforces (verified against HEAD, MOCK only), and what is still missing.
Companion documents: `decision-ballot-2026-10-10.md` (questions), `sandbox-psp-authorization-package-2026-10-10.md`
(per-area acceptance conditions), `sandbox-provider-readiness-gap-analysis-2026-10-10.md` (gaps).

A sandbox authorisation is **not** a non-MOCK or production authorisation. Granting it does not decide Q-HSEC-1/2/3, O-1, O-2,
M-3, HD-R15-3/6, `evidence_ref_hash`, GAP-AAM, D-OPS-1/2, ALERT-DELIVERY-1, legal/licensing, or AWS.

## A. Provider
**TO BE CHOSEN BY THE HUMAN.** Not selected, not recommended here. The human supplies: provider name, sandbox account, and written
terms permitting sandbox testing. Provider-specific facts (below) cannot be filled until then.

## B. Suitability criteria (the provider must meet, else it is not suitable)
1. A sandbox environment with synthetic funds only and a written sandbox-terms statement.
2. Payout API with idempotent submission keyed by a merchant reference, a status query, and documented error taxonomy.
3. PSP-issued stable payout reference that does NOT echo the merchant reference (M4 contract C9).
4. Authenticated callbacks with documented signing, rotation and replay window (S-3).
5. A complete, authenticated, paginated payout statement/report source (M4 contract C1-C11) - needed only for M4/reconciliation evidence, not for the sandbox happy path.
6. Declared destination-echo capability (Supported or Unsupported) - either is admissible for a sandbox (see L).
7. Tenant/merchant context identifiable on every callback and poll.
8. Hosted-fields/redirect/tokenised instrument registration (no raw PAN to us).

## C. Sandbox scope (choose ONE)
- **Option 1 - deposits only**: sandbox deposits and reconciliation; payouts stay MOCK. HD-R15-1, HD-R15-5, D-REG-1 and the echo items then drop to NON-MOCK (not required).
- **Option 2 - deposits + payouts (recommended only if the owner wants payout evidence)**: also requires HD-R15-1, HD-R15-5, D-REG-1, a non-Synthetic payout-instrument verifier, and (if the provider cannot echo) the `Unsupported` acknowledgement.
Either way: ONE provider, ONE synthetic non-real-money tenant, synthetic players, sandbox endpoints only.

## D. Credentials needed (supplied by the human via the secret store; never in the repo or chat)
Sandbox API credential(s); sandbox webhook signing secret; a read-only/reconciliation-scoped credential where the provider offers one (HD-CTF-10). **No production credential. Not requested by this document.** The PROV-OUTBOUND-CRED-1 tripwire is relaxed for that adapter only and only on explicit authorisation.

## E. APIs and callbacks (to be fixed from provider documentation)
Deposit initiation (hosted/redirect), deposit callback, payout submit, payout status query, payout callback, statement/report fetch. Exact endpoints, auth and field names come from the provider docs (EXTERNAL; none exist in the repo). The adapter implements `PaymentProvider` and a manifest; the platform owns idempotency, retries, the state machine and reconciliation.

## F. Financial operations permitted in the sandbox
Synthetic deposit credit; synthetic payout request -> approval -> dispatch -> settle/decline; reconciliation of those operations; compensating entries under existing K2 rules. All against a synthetic tenant with no real money. Ledger invariants (SUM(D)=SUM(C), projection parity) must hold at the end of every run.

## G. Operations that stay MOCK-only (not enabled by this request)
M4 manual resolution (paid/not-paid) on non-MOCK evidence (Go gate G-NONMOCK stays refusing); WITHDRAWAL-REVERSAL-1 (money back after a provider return); `release_hold_to_player` against real funds; return-to-source; crypto rails/custody; any second provider.

## H. Data allowed in
Synthetic players, synthetic KYC outcomes, synthetic destinations/tokens issued by the sandbox, sandbox test card/bank numbers published by the provider.

## I. Data NEVER sent or accepted
Real customer data of any kind; real PAN/bank credentials; production credentials; production tenant or brand identifiers; real money; secrets in logs/audit; platform-internal fingerprints/HMAC keys or snapshot contents to the provider; any destination change via callback.

## J. Reconciliation
Daily reconciliation of sandbox deposits/payouts against the ledger via the existing statement framework (STANDING-1, BOUND-CLEAR-1, R-1, RR-1, MA020 fail-closed joins). Statement import is sealed (T10). Clean reconciliation is a required exit criterion; any drift is a P1 and stops the sandbox run.

## K. Failures, returns, reversals
Every provider status maps to pending/succeeded/declined/ambiguous; unknown status is never succeeded (reviewed by ledger-finance). Declines, timeouts and ambiguity use the existing park/hold behaviour. Provider returns/reversals/chargebacks of payouts map to the contradiction cell and raise an alert; **no automatic money movement** results. Their governed resolution is G (MOCK-only / deferred).

## L. Destination echo
The adapter must declare `Supported` or `Unsupported` explicitly (Unset is invalid; frozen at startup; conformance suite must pass). Callbacks are evidence only; the platform snapshot stays authoritative; any mismatch/malformed/unexpected echo fails closed (park `destination_mismatch`). If `Unsupported`: the human gives a per-provider acknowledgement (still undefined; the sandbox may use an explicit sandbox-only acknowledgement). If `Supported`: an architect decision on tenant visibility in adapter calls (B13B-8) is needed first.

## M. M4 evidence
Not used in the sandbox (non-MOCK M4 stays blocked). Sandbox statements must still satisfy the M4 source contract checker C1-C11 so the evidence path can be assessed; passing it is necessary, not sufficient (D-7: no single untrusted provider field suffices; ambiguity parks, alerts, investigates).

## N. Alerts
B12 alerts raise as implemented. **ALERT-DELIVERY-1 remains OPEN: no recipients or channel are invented.** The sandbox proceeds without delivery on a synthetic tenant (owner YES in principle); sandbox run reviewers read alerts from logs/audit. Production blocker.

## O. Four-eyes
All governed financial/administrative actions keep four-eyes with ADR 0110 signed actor proof; platform-acting approver floor unchanged; no unilateral override; no weakening for the sandbox.

## P. Rollback / kill switch
Payment kill switches, tenant/brand non-active gates and per-tenant capability disable remain the stop controls. A runbook for in-flight attempts at disable time is a deliverable of the adapter work (GAP, doc). The human may revoke authorisation at any time; the adapter is then disabled and the sandbox credential rotated by the human.

## Q. Tests required before the provider is accepted
Echo conformance suite; M4 source contract checker C1-C11 against the statement source; provider status-mapping tests incl. unknown status; callback signature/replay/ordering tests; idempotency/duplicate/retry/partial-failure/rollback/concurrency/authorisation/auditability tests for the adapter; reference-format and amount/asset tests; tenant-isolation test with the synthetic tenant; full payments integration, unit, lint, frontends and targeted -race (local evidence only; GitHub CI is blocked by billing, CI-BILLING-1); security and ledger-finance written sign-off; a sandbox run log showing invariants and clean reconciliation.

## Minimum human authorisation (verbatim form to sign)
1. Provider named: ______ (human). 2. Scope: Option 1 or 2. 3. Answers: HD-R15-1, HD-R15-5, D-REG-1 (Option 2 only); `Unsupported` acknowledgement (only if the provider cannot echo). 4. Confirm ALERT-DELIVERY-1 stays a production blocker. 5. Authorise the PROV-OUTBOUND-CRED-1 relaxation for that adapter only. 6. Confirm: no AWS, no real money, no real customer data, no production credentials.

## Stays blocked (unchanged by this request)
Real provider in production; non-MOCK money movement; AWS; production deployment; production alert delivery (ALERT-DELIVERY-1); legal/licensing launch authorisation.

## Deferred until a real adapter exists (provider-dependent)
Durable echo-absent success record; standing R-1 post-execution success predicate; credit-after-stop monitoring; status/return mapping; real echo computation; S-3 per source; T10 seal review; WITHDRAWAL-REVERSAL-1; G-DEST migration (statement destination clause).
