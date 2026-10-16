# Fiat PSP integration gates

Status: **PROCESS SPECIFICATION.** Nothing here is started or authorised beyond reading public documentation.

> **No real money moves, no production credential is requested or used, and no production traffic reaches a PSP until
> the owner gives EXPLICIT written production authorization (gate G9).** A sandbox authorization (G6) is not a
> production authorization. Technical readiness is separate from legal/licensing approval (owner directive
> 2026-10-11): passing a gate says nothing about the licence or a vendor agreement.

Per provider: documentation -> adapter -> mapping -> conformance -> security -> ledger-finance -> sandbox -> final
approval (readiness report s6). The gates below make that sequence checkable. A gate is passed only when its evidence is
filed; a later gate cannot be entered while an earlier one is open. The orchestrator is the single writer of status
documents; specialists produce the evidence.

| Gate | Name | Owner / specialist | Entry criteria | Exit evidence |
|---|---|---|---|---|
| G0 | Platform prerequisites | orchestrator, `security`, `ledger-finance`, `architect` | Owner has named the provider for evaluation. T8 (ADR 0110 amendment, payout-reversal approvals proof-bound) implemented and merged (owner decision 2026-10-16: before any real PSP is activated). | T8 merge record and tests; list of the platform gaps G-01..G-11 triaged by `architect`/`security` into {not needed, needed, needs ADR} for the chosen provider |
| G1 | Provider documentation review | `payments` | Public documentation reachable (no account, no sign-up, no credential). For Nuvei: access to `https://docs.nuvei.com/` (blocked in the first session) | `nuvei-evaluation.md` UNVERIFIED registry (34 items) re-run: each N-Unn closed with a citation URL and date, or still UNVERIFIED with the reason; fit/gap table updated; viability verdict stated; go/no-go for continuing with this provider (owner) |
| G2 | Adapter build | `payments` | G1 go; G0 triage says no unresolved blocking platform gap (G-01/G-02 decided if applicable); owner authorized adapter work **against recorded fixtures only** (no sandbox call yet) | Adapter in `internal/payments/<provider>` behind `PaymentProvider`; no vendor type outside it (PSP-C-08); manifest truthful; `WebhookScheme` + retry semantics declared; status table complete; labelled `PROVIDER DEPENDENT` / `MOCK`-fixture-backed until G6 |
| G3 | Conformance | `payments`, `qa` | G2 | `conformance-plan.md` sections 1 to 3 pass against recorded fixtures; known-answer vector for the webhook scheme; scheme added to `conformanceManifest`; evidence per `conformance-plan.md` s4 |
| G4 | Security review | `security` | G3 | Written review of: credential storage and the purposes/handles used (G-04, G-08), PCI-scope boundary (hosted/redirect only, no PAN anywhere: PSP-C-09), SSRF/egress allow-list, TLS, logging redaction, webhook scheme (replay, tenant binding, constant-time, body bounds), read-only statement credential and the first Fetch, the PROV-OUTBOUND-CRED-1 tripwire relaxation scoped to this adapter, SR11-1/SR11-10 closure |
| G5 | Ledger-finance review | `ledger-finance` | G3 | Written review of every path that posts: deposit success posting and amounts, reversal/chargeback mapping (G-05), reference binding (G-03), amount/asset evidence, statement reconciliation (LF-A11-1/2/6, settle lag, G-06, G-07), payout asymmetry if payout is in scope, H-W1 second half decision implemented or accepted by the owner in writing |
| G6 | Sandbox | `payments` + `qa`, owner supplies credentials | G4 and G5 approved (or approved with conditions closed); **owner written sandbox authorization** naming provider and scope (Option 1 deposits only, or Option 2 with payouts); vendor sandbox terms in writing; credentials provisioned by the owner through the secret store (never in chat, repo, docs); synthetic tenant only; tripwire relaxation authorized for this adapter only | Sandbox run log: per-scenario results (PSP-C-02 scenarios, 3DS, decline, ambiguous, duplicate and late notifications, refund, chargeback if the sandbox can simulate it), ledger invariants at the end of every run (SUM(D)=SUM(C), projection parity), daily reconciliation clean (PSP-C-49), no real money, no real customer data, sandbox capture ids filed as the vendor fixtures for PSP-C tests, a stop/rollback drill (kill switch, capability disable, in-flight attempts) |
| G7 | Operational acceptance | `devops`, `payments`, orchestrator | G6 passed | Runbooks: in-flight attempts at disable time, stuck cursor (exists: `docs/runbooks/observability-and-alerting.md`), unsupported-event P1, ambiguous/escalated attempt handling, credential rotation, webhook secret rotation; alert delivery: owner-supplied recipients (email + one webhook per the 2026-10-15 decision) with a delivery test; monitoring of provider latency/error rates per tenant; vendor contact/escalation path; egress allow-list and inbound IP allow-list applied in the environment (staging) |
| G8 | Pre-production readiness | orchestrator, all reviewers | G7; the "before production" list of readiness report s4 closed or accepted in writing (ALERT-DELIVERY-1 production blocker, SRSP-3, A12 IAM delta, incident rota, backup/restore tested) | A readiness report update for this provider; open-rows list with owners |
| G9 | **Explicit production authorization** | **owner only** | G8; vendor contract signed by the owner/authorised signatory (not by any agent); licensing/legal confirmation owned by the owner and compliance; production credentials provisioned by the owner into the secret store | Owner's written authorization naming provider, tenants/brands, limits, date; until then the production registry refuses the adapter (`MarkProductionEligible` is not set, startup guards refuse). **No agent may sign, accept an agreement, create an account or supply a credential** |

## Gate G1 checklist (what the documentation review must answer)

The 34 N-Unn questions in `nuvei-evaluation.md` s3, in priority order for a go/no-go: N-U01 PCI-minimal hosted/redirect
option; N-U09/N-U10/N-U12 notification signature scheme, timestamp coverage and delivery shape (can it be registered
under ADR 0022 s3 without an amendment); N-U20 stable reference; N-U19 status lookup returns amount+asset; N-U13 retry
contract; N-U23..N-U28 reporting source and read-only credential; N-U07 chargeback lifecycle; N-U30 markets.
Closure is by citation (URL + retrieval date) or by a vendor written answer or a sandbox capture; an unverifiable item
stays UNVERIFIED and is carried into the gate G2 risk list.

## Exact inputs needed (minimal, no questionnaire)

From the **owner** (decisions; each with a recommendation in `open-questions.md`):

1. Allow the documentation host `docs.nuvei.com` for the documentation-review session (or nominate someone to supply the
   documentation pages), and confirm Nuvei remains a candidate rather than the choice (OQ-1, OQ-2).
2. Written authorization scope for the adapter build (recorded fixtures only) after G1 (OQ-2).
3. Later, only at G6: written sandbox authorization (provider named; deposits-only recommended; synthetic tenant; tripwire
   relaxation for this adapter only; ALERT-DELIVERY-1 stays a production blocker only).
4. Later, only at G9: written production authorization.

From the **vendor** (via the owner; this package contacts no vendor):

1. Sandbox terms in writing and sandbox access. Sandbox credentials: API credential(s) and a **notification signing
   secret**, supplied by the owner **through the platform's per-tenant credential model** (`providercred` handles:
   `outbound_api` purpose for the API credential, `webhook_verify` for the notification secret; ADR 0093), tenant-
   namespaced `secret_ref` in AWS Secrets Manager / the approved secret store. Never in chat, source, docs or logs.
2. Merchant/site identifiers for the synthetic tenant: non-secret identifiers go in per-tenant provider configuration;
   the secret goes in the handle; the single `vendor_account_id` field carries the account id the vendor signs when the
   scheme binds one (G-08 explains the multi-field limitation). One PSP merchant account/site **per tenant** (shared
   account cannot bind a tenant).
3. A read-only/reporting-scoped credential (separate handle; G-04) if the vendor offers scoping.
4. Written answers to the N-Unn items the public docs leave open (signature/timestamp, retry contract, report API record
   time/ordering/lag, chargeback stages, limits, IP ranges).
5. The per-tenant notification URL form: `POST /v1/webhooks/payments/{tenantSlug}/{providerID}` must be configurable per
   merchant/site at the vendor (the tenant is bound by route + per-tenant key, never by payload).
6. Webhook secret handling: the vendor generates or accepts a secret; rotation procedure and overlap window; the platform
   stores it only as a handle (`secret_ref`), never in the database.

Nothing else is requested. No production credential, no real customer data, no real PAN or bank detail, no agreement
signature is requested at any gate before G9.
