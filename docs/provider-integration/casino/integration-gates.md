# Casino aggregator integration gates

Status: **PROCESS SPECIFICATION.** Nothing here is started or authorised beyond reading public documentation once an
aggregator is named.

> **No real money moves, no production credential is requested or used, and no production traffic reaches an aggregator
> until the owner gives EXPLICIT written production authorization (gate C9).** A sandbox authorization (C6) is not a
> production authorization. Technical readiness is separate from legal/licensing approval and from per-brand activation
> (owner directive 2026-10-11; decision register 2026-10-16: "Legal/licensing stays a manual per-brand activation
> decision"): passing a gate says nothing about a gambling licence, a game's legality in a territory, or a vendor agreement.

Per provider: documentation -> adapter -> mapping -> conformance -> security -> ledger-finance -> sandbox -> operational
acceptance -> pre-production -> final approval. A gate is passed only when its evidence is filed; a later gate cannot be
entered while an earlier one is open. The orchestrator is the single writer of status documents; specialists produce the
evidence. Casino has no payout, so the T8 payout-reversal gate does not apply; the PSP package's G0 owns T8.

| Gate | Name | Owner / specialist | Entry criteria | Exit evidence |
|---|---|---|---|---|
| C0 | Platform prerequisites | orchestrator, `casino`, `security`, `ledger-finance`, `architect` | Owner has named the aggregator (or a short list) for evaluation. Staging validation done and the platform declared ready for provider integration (owner objective 2026-10-16). The binding rows of `conformance-plan.md` s2 re-verified at HEAD | List of `CAS-G01..G10` and the shared PSP G-01/G-02/G-04/G-08 triaged by `architect`/`security`/`ledger-finance` into {not needed, needed, needs ADR} for the chosen vendor; CAS-RECON-SCALE-1 sizing decision scheduled |
| C1 | Provider documentation review | `casino` (with `integrations`) | Public/vendor-supplied documentation available to the session (no account, no sign-up, no credential). **No documentation exists today** | The 34 `U-CAS-nn` items re-run: each closed with a citation (URL + retrieval date) or a vendor written answer, or still UNVERIFIED with the reason; mapping tables updated; viability verdict; go/no-go for this vendor (owner). The review must settle first: U-CAS-24 (auth scheme registrable under ADR 0022 s3 without amendment), U-CAS-08 (token redemption), U-CAS-13/14/17 (balance, declines, round close), U-CAS-19/20 (free rounds, multi-bet), U-CAS-32 (statement source), U-CAS-25 (per-operator callback and secret) |
| C2 | Adapter build | `casino` | C1 go; C0 triage says no unresolved blocking platform gap (CAS-G01/G02 decided if applicable); owner authorized adapter work **against recorded fixtures only** (no sandbox call) | Adapter in `internal/casino/<provider>` behind `CasinoProvider`; no vendor type outside it; reuses `internal/providers/httpclient` (timeouts, bounded retry only for idempotent requests, no redirects) and `internal/providers/config`, not a re-implementation; `WebhookScheme` + `WebhookRetrySemantics` declared; capability declaration truthful (M-CAS-1); bundle/registration changes in `cmd/platform-api/registrations.go`; labelled `PROVIDER DEPENDENT` / `MOCK`-fixture-backed until C6 |
| C3 | Conformance | `casino`, `qa` | C2 | `conformance-plan.md` s1 to s3 pass against recorded fixtures; known-answer vector for the webhook scheme; scheme added to `conformanceManifest` in the same change; mock-typed sub-tests converted (CAS-C-01) and the skip-to-fail proof still green; evidence per s4 |
| C4 | Security review | `security` | C3 | Written review of: credential storage and the purposes/handles used (PSP G-04, G-08), the inbound scheme (replay, tenant binding, constant time, body bounds; PSP G-01/G-02 outcome), egress allow-list and SSRF, TLS, log/audit redaction (no player data, no secret, no raw vendor body), the opaque player reference (unlinkability, no PII to the vendor), any CAS-G01 hook (a vendor-shaped response must not leak what the generic body withholds), the PROV-OUTBOUND-CRED-1 tripwire relaxation scoped to this adapter, revocation immediacy (CAS-C-15), emergency-stop consequences (stranded exposure) |
| C5 | Ledger-finance review | `ledger-finance` | C3 | Written review of every path that posts for this vendor's events: bet/win/rollback mapping, amount and asset conversion (CAS-C-07), `provider_tx_id` derivation (CAS-C-08), free-round/jackpot/multi-bet handling (CAS-G03/G04), tombstone and out-of-order behaviour (CAS-C-21/22), bonus-funded settlement, statement reconciliation (CAS-G07: source registry, stored copy, windowing, real mismatch kind, unpaired tombstone and duplicate-rollback flags), CAS-RECON-SCALE-1 decision, LEDGER-MANUAL-ADJ-4EYES-1 status, abandoned-round policy (CAS-G08) |
| C6 | Sandbox | `casino` + `qa`, owner supplies credentials | C4 and C5 approved (or approved with conditions closed); **owner written sandbox authorization** naming the vendor and scope (which games, which currencies); vendor sandbox terms in writing; credentials provisioned by the owner through the secret store (never in chat, repo, docs); synthetic tenant and synthetic players only; tripwire relaxation authorized for this adapter only | Sandbox run log: per-scenario results (launch, bootstrap, bet, win, loss, rollback, rollback-before-bet, duplicate and late notifications, decline paths, free-round/jackpot titles disabled or refused, session expiry, kill switch mid-round, credential revocation mid-round), ledger invariants at the end of every run (SUM(D)=SUM(C), projection parity, `casino_consistency` clean), `casino_statement` reconciliation clean against the vendor's own report (CAS-C-41..43), no real money, no real customer data, sandbox captures filed as the vendor fixtures for CAS-C tests, a stop/rollback drill |
| C7 | Operational acceptance | `devops`, `casino`, orchestrator | C6 passed | Runbooks: credential rotation and revocation (with the mandatory consistency/statement run before a replacement key), webhook secret rotation, in-flight rounds at disable time, integrity-alert handling (`bet_not_found`, `payload_mismatch`, `original_tombstoned`, `win_origin`), abandoned-round handling, vendor outage; alert delivery to owner-supplied recipients with a delivery test; per-tenant monitoring of provider latency/error rates; vendor contact/escalation path; egress and inbound IP allow-lists applied in staging |
| C8 | Pre-production readiness | orchestrator, all reviewers | C7; the "before production" list of the readiness report closed or accepted in writing (ALERT-DELIVERY-1 production blocker and others as listed there), CAS-WIN-ANOMALY-1 built, CAS-STMT-STORE-1 and CAS-RECON-SCALE-1 resolved or accepted, HD-10.3-4 consequence accepted by the owner | A readiness report update for this vendor; open-rows list with owners |
| C9 | **Explicit production authorization** | **owner only** | C8; vendor contract signed by the owner/authorised signatory (not by any agent); licensing/legal confirmation owned by the owner and compliance, per brand and per game/territory; per-brand activation decision; production credentials provisioned by the owner into the secret store | Owner's written authorization naming vendor, tenants/brands, games, territories, limits, date; until then the production registry refuses the adapter (`MarkProductionEligible` not set; startup guards refuse). **No agent may sign, accept an agreement, create an account or supply a credential** |

## Gate C1 checklist (what the documentation review must answer)

In priority order for a go/no-go: the inbound authentication scheme (U-CAS-24, U-CAS-25: registrable and per-tenant);
how the player reaches the game and how the vendor identifies the player to the wallet (U-CAS-08..10); the wallet
callback set and the exact semantics of decline, balance, round close, rollback (U-CAS-13..18); free rounds, jackpots and
multi-bet (U-CAS-19, U-CAS-20); retry and ordering (U-CAS-26, U-CAS-29..31); the statement source (U-CAS-32); the
catalogue and territory data (U-CAS-01..03). Closure is by citation or vendor written answer; an unverifiable item stays
UNVERIFIED and is carried into the gate C2 risk list.

## Exact inputs needed (minimal, no questionnaire)

From the **owner** (decisions; each with a recommendation in `open-questions.md`):

1. Name the aggregator(s) to evaluate and confirm the evaluation is documentation-only (OQ-CAS-01).
2. Provide, or authorize the session to read, the vendor documentation (OQ-CAS-02).
3. Written authorization scope for the adapter build, recorded fixtures only, after C1 (OQ-CAS-02).
4. Decide the product scope for the first brand: which game categories, currencies and territories; whether bonus-funded
   play is in scope; whether free rounds/jackpot titles are excluded (OQ-CAS-04, OQ-CAS-05).
5. Later, only at C6: written sandbox authorization. Later, only at C9: written production authorization.

From the **vendor** (via the owner; this package contacts no vendor):

1. Documentation: wallet/seamless integration contract, signature scheme with test vectors, error codes, retry policy,
   statement/report specification, catalogue and territory data format, free-round/jackpot/multi-bet behaviour.
2. Sandbox terms in writing and sandbox access. Sandbox credentials: outbound API credential, an **inbound callback signing
   secret**, and a read-only report credential if scoping exists, supplied by the owner **through the platform's per-tenant
   credential model** (`providercred`, domain `casino`; purposes `webhook_verify` and `outbound_api`; ADR 0093), tenant-
   namespaced `secret_ref` in the approved secret store. Never in chat, source, docs or logs.
3. One operator/site account **per tenant**, with the callback URL form `POST /v1/webhooks/casino/{tenantSlug}/{providerID}`
   configurable per account (the tenant is bound by route and per-tenant key, never by payload). Non-secret operator/site
   identifiers go to per-tenant provider configuration (PSP G-08: this needs a home).
4. Written answers to the `U-CAS-nn` items the documentation leaves open.
5. Published source IP ranges, if any, and the vendor's incident/escalation contacts.

Nothing else is requested. No production credential, no real customer data, no real money, no agreement signature is
requested at any gate before C9.
