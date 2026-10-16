# Sportsbook integration gates

Status: **PROCESS SPECIFICATION. Sportsbook is DEFERRED** until a sportsbook brand is scheduled (owner decision 2,
2026-10-15). Gate S0 cannot be entered before that scheduling decision. Nothing here is started or authorised.

> **No real money moves, no production credential is requested or used, and no production traffic reaches a sportsbook
> provider until the owner gives EXPLICIT written production authorization (gate S9).** A sandbox authorization (S6) is
> not a production authorization. Technical readiness is separate from legal/licensing approval and from per-brand
> activation (decision register 2026-10-16): passing a gate says nothing about a betting licence, an event/market's
> legality in a territory, or a vendor agreement.

A gate is passed only when its evidence is filed; a later gate cannot be entered while an earlier one is open. The
orchestrator is the single writer of status documents; specialists produce the evidence.

| Gate | Name | Owner / specialist | Entry criteria | Exit evidence |
|---|---|---|---|---|
| S0 | Scheduling and operating-model decision | orchestrator, `architect`, `sportsbook`, `ledger-finance` | **Owner has scheduled a sportsbook brand** (the deferral is lifted in the decision register) and named the provider(s) to evaluate. Staging validation done | OQ-SB-01 decided (platform books against a data feed vs vendor books); `SB-G01..G10` triaged by `architect`/`security`/`ledger-finance` into {not needed, needed, needs ADR}; ADR drafted for the chosen interface; the binding rows of `conformance-plan.md` s2 re-verified at HEAD |
| S1 | Provider documentation review | `sportsbook` (with `integrations`) | Documentation available to the session (no account, no credential). **None exists today** | The 13 `U-SB-nn` items re-run: each closed with a citation or a vendor written answer, or still UNVERIFIED with the reason; viability verdict; go/no-go (owner). Settle first: U-SB-01 (acceptance), U-SB-07 (settlement event shape and ordinal), U-SB-11 (authentication), U-SB-13 (settlement report), U-SB-05 (market taxonomy fit), U-SB-04/09 (free bets, cashout) |
| S2 | Adapter build | `sportsbook` | S1 go; S0 triage has no unresolved blocking gap (SB-G01/G02/G04 decided as applicable); owner authorized adapter work **against recorded fixtures only** | Adapter behind the interface fixed by the S0 ADR; no vendor type outside it; reuses `internal/providers/httpclient` and `internal/providers/config`; `WebhookScheme` + retry semantics declared if push; settlement driven only through the existing sole-writer entry point (no second writer); labelled `PROVIDER DEPENDENT` / fixture-backed until S6 |
| S3 | Conformance | `sportsbook`, `qa` | S2 | `conformance-plan.md` s1 to s3 pass against recorded fixtures; known-answer vector for the scheme; evidence per s4 |
| S4 | Security review | `security` | S3 | Written review of: credential model (X-G01 outcome), inbound scheme and tenant mapping of bet-keyed events (SB-G04), market-wide fan-out identity and fence, egress/SSRF, log and audit redaction, the PROV-OUTBOUND-CRED-1 tripwire relaxation scoped to this adapter, the test-support settlement route never registered in production |
| S5 | Ledger-finance review | `ledger-finance` | S3 | Written review of every path that posts: placement, settlement, void, rollback, tombstone, re-settlement by generation, payout derivation and any adjusted-payout kind (SB-G03), provider-reference idempotency (SB-G02), XMIN/SAVEPOINT batch behaviour, statement reconciliation (SB-G07: fetcher shape, key mapping, coverage anchor, mismatch kind, scale), open-liability reporting (SB-G08), OB-1 status |
| S6 | Sandbox | `sportsbook` + `qa`, owner supplies credentials | S4 and S5 approved (or approved with conditions closed); **owner written sandbox authorization** naming provider and scope (sports, pre-match singles, currencies); vendor sandbox terms in writing; credentials via the secret store (never in chat, repo, docs); synthetic tenant and players only; tripwire relaxation authorized for this adapter only | Sandbox run log: per-scenario results (acceptance accepted/rejected/pending, odds change, market suspension mid-bet, settle won/lost, void, dead-heat refusal, correction, duplicate and late events, out-of-order, tenant suspension mid-event, kill switch), ledger invariants at the end of every run and the `sportsbook_settlement` stream clean against the vendor's report, no real money, sandbox captures filed as fixtures, a stop drill |
| S7 | Operational acceptance | `devops`, `sportsbook`, orchestrator | S6 passed | Runbooks: stuck pending bets, integrity alerts (`PAYOUT_INVALID`, `SETTLEMENT_PAYLOAD_MISMATCH`, `GENERATION_OUT_OF_SEQUENCE`, tombstoned), void-correction manual handling, credential rotation, vendor outage and betting posture, event cancellation mass-void procedure; alert delivery with a delivery test; monitoring per tenant; vendor escalation; allow-lists applied in staging |
| S8 | Pre-production readiness | orchestrator, all reviewers | S7; the "before production" list of the readiness report closed or accepted in writing (ALERT-DELIVERY-1 and others); open-liability reporting built; exposure arming decision recorded; HDR-SB-1 answered | A readiness report update for this provider; open-rows list with owners |
| S9 | **Explicit production authorization** | **owner only** | S8; vendor contract signed by the owner/authorised signatory (not by any agent); licensing/legal confirmation owned by the owner and compliance per brand and territory; per-brand activation decision; production credentials provisioned by the owner into the secret store | Owner's written authorization naming provider, tenants/brands, sports/markets, limits, date; until then the production registry refuses the adapter. **No agent may sign, accept an agreement, create an account or supply a credential** |

## Gate S1 checklist (what the documentation review must answer)

In priority order for a go/no-go: who accepts the bet and carries the risk (U-SB-01); the settlement event shape,
identifiers and ordering, and how a correction is told from a resend (U-SB-07, U-SB-08); how the vendor's payout relates
to stake x odds (U-SB-02, U-SB-04, U-SB-09); the authentication scheme (U-SB-11); the settlement report (U-SB-13); the
market taxonomy fit (U-SB-05, U-SB-06). Closure is by citation or vendor written answer; an unverifiable item stays
UNVERIFIED and is carried into the S2 risk list.

## Exact inputs needed (minimal, no questionnaire)

From the **owner** (decisions; each with a recommendation in `open-questions.md`):

1. The decision to schedule a sportsbook brand (lifts the deferral) and the first-brand scope (OQ-SB-02, OQ-SB-03).
2. The operating model (OQ-SB-01) and the provider(s) to evaluate.
3. Provide or authorize reading the documentation; authorize the adapter build on recorded fixtures only after S1.
4. Later, only at S6: written sandbox authorization. Later, only at S9: written production authorization.

From the **vendor** (via the owner; this package contacts no vendor):

1. Documentation: bet and settlement contract, market and odds model, signature scheme with test vectors, retry policy,
   settlement report specification.
2. Sandbox terms in writing and sandbox access. Sandbox credentials supplied by the owner through the platform's
   credential model (domain to be decided, X-G01) with `secret_ref` in the approved secret store; an inbound signing
   secret; a read-only report credential if scoping exists. Never in chat, source, docs or logs.
3. One operator account per tenant and a per-account callback URL form (tenant bound by route and key, never payload).
4. Synthetic events, markets and settlement scenarios for the sandbox.
5. Written answers to the `U-SB-nn` items the documentation leaves open; published IP ranges; escalation contacts.

Nothing else is requested. No production credential, no real customer data, no real money, no agreement signature is
requested at any gate before S9.
