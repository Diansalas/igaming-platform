# Sandbox PSP / provider readiness gap analysis (2026-10-10)

Status: **analysis and documentation only. No sandbox PSP, AWS or real-provider work is started or authorised** (ADR 0095 s48
decisions 9 and 10). Built from the repository state at HEAD (after the 2026-10-09 governance cycle), the human-decision-register
and ADR 0111. Every "must be supplied" line below is a prerequisite that only a human, a provider or an AWS authorisation can
satisfy; every "already in place" line is verified in the code or the ADR it names.

## A. What is already in place (provider-independent, MOCK-proven)

| Capability | State | Source |
|---|---|---|
| Payout destination binding, write-once snapshot, gates at request/T1p/phase B/T2/T12 | IMPLEMENTED (MOCK) | ADR 0111 s15, s18 |
| Callbacks/polls/statements are evidence only; echo compared with the platform-authoritative snapshot; mismatch parks `disputed` | IMPLEMENTED (MOCK) | ADR 0111 s2.6, s24 |
| Mandatory destination-echo declaration (Supported / Unsupported / Unset invalid), frozen at startup, conformance suite | IMPLEMENTED (MOCK) | ADR 0111 s24 |
| Instrument state does not gate settlement of an authorised payout | VERIFIED | ADR 0111 s22 |
| M4 evidence standard (nine properties), refusal-only Go gates, DB recount, source contract checker (C1-C11) | IMPLEMENTED (MOCK); non-MOCK BLOCKED | ADR 0111 s25 |
| Governed exits: M4 paid/not-paid (four-eyes, platform_acting), HSEC hold release, `destination_integrity_failure` not-paid exit, durable provider-success record | IMPLEMENTED (MOCK) | ADR 0111 s17, s16, s23 |
| Reconciliation: STANDING-1, BOUND-CLEAR-1, R-1, RR-1 NET recovery, I-2 cap, fail-closed evidence joins | IMPLEMENTED (MOCK) | ADR 0111 s19, s21 |
| Real-time post-M4 signal cells, B12 raise-only alerts with deduplication, audit-once rules | IMPLEMENTED (MOCK) | ADR 0111 s20 |
| Provider abstraction, Synthetic marker, `RefuseSyntheticInProduction`, tiering predicate, embedding scan | IMPLEMENTED | ADR 0111 s15, s18 |

## B. What must be supplied before the FIRST sandbox provider can be connected

Each item names who must supply it and what in the platform consumes it.

| # | Prerequisite | Who supplies it | Platform consumer / acceptance check |
|---|---|---|---|
| S1 | Written authorisation to start sandbox PSP work (B14-B18 are NOT authorised) | owner | task registry gate; PROV-OUTBOUND-CRED-1 tripwire relaxation scoped to that adapter |
| S2 | Provider API documentation: payout (withdraw) request/response, idempotency semantics, error taxonomy, status query, callbacks | provider | `PaymentProvider` adapter (payout capability), `OperationManifest` (IdempotentSubmission etc.) |
| S3 | Sandbox credentials (non-production, read-only/reconciliation-scoped credential where the PSP supports one: HD-CTF-10, R3-FIRST-REAL-SOURCE-READONLY-1) | provider via owner; stored through the secret store, never in the repo | `providercred` per-tenant credentials |
| S4 | Callback signing: scheme, key rotation, replay window, timestamp semantics, IP allow-list if any (S-3) | provider | `webhookauth` verifier for that provider; per-tenant context resolution |
| S5 | Provider tenant context: how the provider identifies the merchant/tenant on every callback and poll, and how the platform maps it to a tenant (the adapter does not receive the tenant in `Withdraw`/`QueryStatus` today: B13B-8) | provider + architect | an architect interface decision on tenant visibility in adapter calls |
| S6 | Destination-echo semantics: does the provider return a destination identifier on success/poll/callback, in what shape, and can the adapter compute a tenant-bound fingerprint (decision 5 declaration `Supported`) or must it declare `Unsupported` | provider + owner (per-provider acknowledgement for `Unsupported` is still undefined) | `DestinationEchoSemantics` declaration; echo conformance suite |
| S7 | Status mapping: every provider status mapped to pending / succeeded / declined / ambiguous, including unknown-status policy (never succeeded) | provider docs + ledger-finance review | adapter status mapping tests; M4 source contract C3, C4, C7 |
| S8 | Return / reversal / chargeback-of-payout mapping and reversal window; the governed WITHDRAWAL-REVERSAL-1 path must exist before money moves back | provider + ledger-finance | ADR 0111 s20.4 launch conditions; M4 contract C3, C7 |
| S9 | Statement / reconciliation source: a complete, authenticated, sealed payout statement (declares `payout_lines_carry_merchant_reference`, PSP-issued stable references that never echo the merchant reference, coverage and pagination completeness) | provider | `statement` ingest, import seal (T10 security review), M4 contract C2, C5, C8-C10 |
| S10 | M4 evidence readiness: a real source passing the M4 contract checklist (necessary, not sufficient), S-3 per source, T10 seal review, a destination clause for M4 paid (statement lines cannot carry a destination yet), `evidence_ref_hash` binding decision (ledger-finance digest vs security blind entry: OPEN), per-source clock-skew tolerance (<= 5 min, lower bound only, ledger-finance reviewed) | provider + security + ledger-finance + owner | ADR 0111 s25.7 |
| S11 | Alert delivery: recipients, channel, on-call, routing (ALERT-DELIVERY-1 / HD-PRH2-4-OPS, OPEN). A sandbox on a synthetic non-real-money tenant is allowed to proceed before it (owner YES in principle) but it remains a PRODUCTION blocker | owner / ops | alerting kinds already raise; delivery is the missing half |
| S12 | Provider-specific conformance tests: the echo conformance suite, the M4 source contract checker, callback replay/ordering, status mapping, per-provider reference format | engineering, after S2-S9 | CI-equivalent local evidence (GitHub CI blocked by billing) |
| S13 | Verification max-age / re-verification cadence (HD-R15-1), what counts as verified (HD-R15-2), and the stranded-withdrawal disposition (HD-R15-5) before ANY non-MOCK payout | owner + ledger-finance | ADR 0111 s10.3 launch flags |
| S14 | Payout-instrument launch flags: M-3 (fingerprint ownership), L-1 (client-supplied card_token fields must come from the PSP), L-2 (several fingerprints per destination) | owner/architect (M-3); provider (L-1, L-2) | `docs/governance/open-owner-questions-2026-10-09.md`, ADR 0111 s15.5 |
| S15 | Player instrument registration through a PSP-hosted / tokenised flow (no raw credential collection by the platform frontend) and its PCI/PII security review | provider + security | `docs/governance/player-instrument-registration-brief.md` |
| S16 | Echo-absent success from a `Supported` adapter recorded durably, and the standing R-1 predicate for a post-execution recorded success (deferred until a real `Supported` adapter exists) | engineering, after S6 | ADR 0111 s23.6, s24.6 |

Items the sandbox may proceed without (owner YES in principle, synthetic non-real-money tenant only): S11 (alert delivery) only.
Everything else above is required.

## C. What must be supplied/authorised for AWS (not started)

| # | Prerequisite | Source |
|---|---|---|
| A1 | Explicit human authorisation to deploy/enable AWS (decision 10: NOT authorised) | owner |
| A2 | Staging deployment from the final approved commit with PLAT-ROLESPLIT-1 verification | HANDOVER s39 |
| A3 | AWS human actions: ACCESS-ANALYZER-CHECK-1, DEPLOY-FPKEY-1, HD-10.3-2 | HANDOVER s39 |
| A4 | Hosting AUP, backup/DR with a tested restore (RPO/RTO evidence) | HANDOVER s39 |
| A5 | Real alert recipients/on-call through AWS-hosted channels (ALERT-DELIVERY-1) | HANDOVER s39 |
| A6 | Secret store wiring for provider credentials (secretstore/awssm exists; credentials are human-supplied) | internal/secretstore |

## D. Production blockers (summary, not exhaustive)

Licence and legal inputs; real vendors; ALERT-DELIVERY-1 with on-call recipients; backup/DR with a tested restore; staging
deployment and AWS human actions; green CI evidence (GitHub CI is blocked by billing, CI-BILLING-1; or an owner-ruled
self-hosted equivalent); WEBHOOK-EDGE-1, CAS-RECON-SCALE-1, DEVOPS-0107-INDEX-WINDOW-1; every open owner question that is
marked "blocks production" in `open-owner-questions-2026-10-09.md`; the provider-dependent M4/echo/status items above;
production configuration checklist; security sign-off; explicit production launch authorisation by the human.
