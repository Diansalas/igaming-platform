# Sports-data integration gates

Status: **PROCESS SPECIFICATION. DEFERRED with sportsbook** (owner decision 2, 2026-10-15). Gate D0 cannot be entered
before a sportsbook brand is scheduled and the operating model is decided (`../sportsbook/open-questions.md` OQ-SB-01).
Nothing here is started or authorised.

> **No real money moves, no production credential is requested or used, and no production feed traffic drives a bet
> price, a market state or a settlement until the owner gives EXPLICIT written production authorization (gate D9).** A
> sandbox authorization (D6) is not a production authorization. Technical readiness is separate from legal/licensing
> approval, from data-rights licensing, and from per-brand activation (decision register 2026-10-16).

A gate is passed only when its evidence is filed; a later gate cannot be entered while an earlier one is open. The
orchestrator is the single writer of status documents; specialists produce the evidence.

| Gate | Name | Owner / specialist | Entry criteria | Exit evidence |
|---|---|---|---|---|
| D0 | Seam prerequisites | orchestrator, `architect`, `sportsbook`, `security`, `ledger-finance` | A sportsbook brand is scheduled; OQ-SB-01 decided; owner has named the data provider(s) to evaluate; X-G01 (credential decision) and PSP G-01/G-02 (scheme shape) decided | ADR 0117 Part C slice C1 (MOCK feed: interfaces, ingest with seals, NULL-arm fences, allowlisted identities, feed-namespaced references; no settlement) built, reviewed and merged, with SD-C-10..16, SD-C-40..42 passing against the MOCK feed. `SD-G01..G08` triaged into {not needed, needed, needs ADR}; slices C2 (acceptance rules), C3 (results and settlement; ledger-finance-owned) and C4 (push admission, watchdog) scheduled as needed by the provider's shape |
| D1 | Provider documentation review | `sportsbook` (with `integrations`) | Documentation available to the session (no account, no credential). **None exists today** | The 12 `U-SD-nn` items re-run: each closed with a citation or a vendor written answer, or still UNVERIFIED with the reason; viability verdict; go/no-go (owner). Settle first: U-SD-08 (ordering primitive; "an adapter that supports neither is not admissible"), U-SD-09/10 (ids, duplicates, gaps), U-SD-12 (result shape, finality, corrections), U-SD-03 (push authentication), U-SD-11 (cadence/heartbeat), U-SD-02 (streaming) |
| D2 | Adapter build | `sportsbook` | D1 go; D0 done; owner authorized adapter work **against recorded fixtures only** | Adapter behind `FeedProvider`; canonicalization once inside the adapter; no vendor type outside it; reuses `internal/providers/httpclient`/`config`; capability declaration truthful (push/poll/both, kinds, versioning mode, heartbeat cadence); sequencing primitive declared; labelled `PROVIDER DEPENDENT` / fixture-backed until D6 |
| D3 | Conformance | `sportsbook`, `qa` | D2 | `conformance-plan.md` s1 to s3 pass against recorded fixtures; known-answer vector for the push scheme if any; evidence per s4 |
| D4 | Security review | `security` | D3 | Written review of: the platform-scope credential and its rotation, push admission (mTLS/allowlist, replay window, dedicated bucket), SSRF set on polling, seals (S-B2) and the stolen-credential residual (launch-blocking for a non-MOCK feed), allowlisted identities, tenant-scope derivation (SBF-8), log/audit redaction of licensed data, the C0 `FOR SHARE` grants, the test-support route never registered in production |
| D5 | Ledger-finance and risk review | `ledger-finance`, `risk`, `sportsbook` | D3 | Written review of the result path: sealing, selection-set validation, mandatory hold triggers and the threshold values supplied by the owner/risk, four-eyes confirmation, fan-out idempotency and generations, correction and void rules, payout from frozen odds, dead-heat refusal, new reconciliation checks, `driver` column and the production simulation P1 |
| D6 | Sandbox | `sportsbook` + `qa`, owner supplies credentials | D4 and D5 approved (or approved with conditions closed); **owner written sandbox authorization** naming provider and scope (sports, competitions, pre-match only); vendor sandbox terms in writing; credentials via the secret store (never in chat, repo, docs); synthetic tenant only; data-licence terms for sandbox data in writing | Sandbox run log: per-scenario results (fixture create/update/cancel, price changes, suspension, heartbeat loss and recovery, duplicate/out-of-order/gap, result, held result and confirmation, correction, void, unmapped reference, malformed message), ledger invariants and the settlement stream clean, no real money, sandbox captures filed as fixtures (subject to the data licence), a stop drill (disable feed, in-flight bets, markets suspended) |
| D7 | Operational acceptance | `devops`, `sportsbook`, orchestrator | D6 passed | Runbooks: feed down / stale (markets suspended, bets refused), held-result confirmation procedure, correction and void-correction handling, unmapped-reference provisioning, credential rotation, replay/resync, vendor escalation; alert delivery with a delivery test (watchdog alerts depend on it); monitoring of lag, message rates, rejected/stale counts; allow-lists applied in staging |
| D8 | Pre-production readiness | orchestrator, all reviewers | D7; the "before production" list of the readiness report closed or accepted in writing (ALERT-DELIVERY-1 and others); thresholds configured per tenant and asset; the seal residual closed or accepted in writing | A readiness report update for this provider; open-rows list with owners |
| D9 | **Explicit production authorization** | **owner only** | D8; data-provider contract and data licence signed by the owner/authorised signatory (not by any agent); licensing/legal confirmation owned by the owner and compliance; production credentials provisioned by the owner into the secret store | Owner's written authorization naming provider, sports/competitions, tenants/brands, date; until then `sb_feed_sources.kind` admits only `mock` and the production registry refuses the adapter. **No agent may sign, accept an agreement, create an account or supply a credential** |

## Gate D1 checklist (what the documentation review must answer)

In priority order for a go/no-go: the ordering primitive and its scope (U-SD-08); message ids and duplicate/gap
behaviour (U-SD-09/10); result shape, finality and correction policy (U-SD-12); authentication and transport
(U-SD-01..04); cadence and heartbeat (U-SD-11); identity and lifecycle (U-SD-05..07). Closure is by citation or vendor
written answer; an unverifiable item stays UNVERIFIED and is carried into the D2 risk list.

## Exact inputs needed (minimal, no questionnaire)

From the **owner** (decisions; each with a recommendation in `open-questions.md`):

1. The decision to schedule a sportsbook brand and the operating model (`../sportsbook/` OQ-SB-01/02), then the data
   provider(s) to evaluate (OQ-SD-01).
2. Pre-match only for the first feed (OQ-SD-02); the mapping policy (OQ-SD-05); the hold thresholds and second-source
   policy for results (OQ-SD-04, owner/risk values, none invented).
3. Provide or authorize reading the documentation; authorize the adapter build on recorded fixtures only after D1.
4. Later, only at D6: written sandbox authorization. Later, only at D9: written production authorization.

From the **vendor** (via the owner; this package contacts no vendor):

1. Documentation: data model and identifiers, ordering and duplicate semantics, transport (poll/push/stream) and
   authentication with test vectors, result and correction policy, cadence and heartbeat, SLAs.
2. Sandbox terms in writing and sandbox access; sandbox credentials supplied by the owner through the secret store (a
   platform-scope credential, not per-tenant; X-G01), never in chat, source, docs or logs; allowed use of sandbox data in
   fixtures.
3. Source IP ranges or mTLS options; escalation contacts.
4. Written answers to the `U-SD-nn` items the documentation leaves open.
5. The data licence terms (display, redistribution, official-data flags) for the owner and legal.

Nothing else is requested. No production credential, no real money, no agreement or licence signature is requested at any
gate before D9.
