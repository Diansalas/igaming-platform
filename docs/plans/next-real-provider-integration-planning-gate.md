# Planning gate — First Real Provider Integration + Production Provider Readiness

- **Type:** PLANNING ONLY. Docs only. No code, no migration, no AWS action, no vendor contact, no commit.
- **Author:** `architect`, for the orchestrator. Date 2026-09-26.
- **Baseline:** branch `claude/focused-wright-jw88w9`; code baseline `103b033` (CI #349 green, all jobs);
  docs baseline `2876fa5`. Staging is OFF (torn down 2026-09-26).
- **Sources read:** `CLAUDE.md`, `MASTER-BUILD-PROMPT.md`, `docs/progress.md` (Stage 10.3),
  `docs/active-stage.md`, `docs/governance/stage-10.3-completion-report.md`,
  `docs/plans/stage-10.3-planning/00-roadmap-reconciliation.md`, `docs/governance/task-registry.md`
  ("Stage 10.3"), ADRs 0006, 0007, 0008, 0009, 0019, 0022, 0025, 0028, 0085, 0089, 0092, 0093,
  `docs/architecture/reconciliation-model.md`, `docs/architecture/crypto-custody-boundary.md`, and the
  Blueprint (`iGaming-Platform-Blueprint.pdf`, 20 pages).
- **Rules for this paper:**
  - No vendor is selected.
  - No vendor-specific fact is stated. Every vendor property is an intake question.
  - `RECOMMENDATION` marks the architect's advice. Advice is never presented as a Blueprint
    requirement. `BLUEPRINT §n` marks text the Blueprint actually contains.
  - Labels follow `CLAUDE.md`: `IMPLEMENTED`, `PARTIALLY IMPLEMENTED`, `MOCK`, `STUB`,
    `PROVIDER DEPENDENT`, `NOT IMPLEMENTED`, `BLOCKED`.
- **Nothing here authorizes work.** Every wave below needs explicit human authorization (the
  stage-gate rule in `CLAUDE.md`).

---

## 1. Current product state

| Area | State at `103b033` | Label |
|---|---|---|
| Ledger / wallet | Append-only double-entry ledger, per-asset wallets (ADR 0007), projections, hourly `ledger_vs_projection` reconciliation | `IMPLEMENTED` |
| Payments | Deposit and withdrawal orchestration, capability and routing rows (migrations 0024/0030), reversal (PAY-REV-1), tenant-bound verify-first webhooks | `IMPLEMENTED` for the MOCK PSP only |
| Casino | Launch, catalogue, bet/win/rollback callbacks, capability contract (0094), multi-bet G-1 fix, `casino_consistency` C1–C7, `casino_statement` | Contract `IMPLEMENTED — MOCK provider only`; statement `MOCK` |
| Sportsbook | Settlement lifecycle (Stage 10), `sportsbook_settlement` reconciliation, test-support settlement route | `MOCK`; exposure gate unarmed (HDR-SB-1) |
| KYC | Verification state machine, bounded staff-only reason, provider selected per tenant from its outbound handle | `IMPLEMENTED` for the MOCK only. Production self-service KYC returns 503 |
| Provider trust | Per-adapter `VerificationScheme`, SC1–SC13 suite, synthetic/production guard, credential handles + resolver + four-eyes activation | `IMPLEMENTED` on `memory`/`devfile`. `awssm` is `PARTIALLY IMPLEMENTED` |
| Outbound credentials | Per-call resolver and authenticator; outbound calls still inside domain DB transactions | `PARTIALLY IMPLEMENTED` (launch-blocking) |
| Production binary | Refuses to start with any synthetic component; every bundled adapter is synthetic | Fail-closed by design |
| Real providers | None | `NOT IMPLEMENTED` (`PROVIDER DEPENDENT`) |
| Staging / AWS | OFF. No AWS action since the teardown | — |

No production readiness, provider readiness, licensing or regulatory approval is claimed.

## 2. Stage 10.3 completion

| Item | Record |
|---|---|
| Human acceptance | Stage 10.3 accepted as **COMPLETE** by the human on 2026-09-26, with the carried open findings |
| Code baseline | `103b033` (CI #349 green, all jobs; local 3× race-integration replay passed) |
| Docs baseline | `2876fa5` (completion report, gate 10.3-W2/W3 PASSED, progress, active-stage) |
| CI | Green. `TestStoreOutage_DoesNotPinPool` runs alone in its own blocking step (ruling B) |
| Staging | OFF |
| Delivered | W1a–W1d, W2a, W2b, W3a, W3b; labels in the completion report §2 |
| Not met | PROV-OUTBOUND-CRED-1 target `IMPLEMENTED` (reached `PARTIALLY IMPLEMENTED`) |
| Carried finding | F-POOL-1 (see §3) |

## 3. F-POOL-1 resolution

[PENDING: F-POOL-1 implementation + review]

## 4. Remaining launch blockers

Sources: completion report §8/§11, registry "Stage 10.3", roadmap reconciliation §7. "Vendor?" says
whether the item needs a selected vendor.

| ID | Blocker | Owner | Vendor? | Blocks |
|---|---|---|---|---|
| F-POOL-1 | Store outage pins the pool at production pool size | architect + security | No | Any real provider go-live (§3 pending) |
| PROV-OUTBOUND-CRED-1 | Outbound calls (`Deposit`, `Withdraw`, `QueryStatus`, `Launch`, `CreateVerification`) run inside a domain DB tx; tenant + credential not in adapter request types. Tripwire `TestOutboundPrecondition_EveryWiredAdapterIsSynthetic` | architect + ledger-finance + payments/casino/identity-compliance | No | Registering **any** non-synthetic payments, casino or KYC adapter |
| PROVIDER-REF-BOUND-1 | No length bound on provider reference columns | ledger-finance + casino + payments | No | Real-provider go-live |
| CODE-HYGIENE-10.3-1 item 2 | `DerivedTokenCache` unbounded | architect + security | No | Its first production caller |
| WH-VENDOR-SCHEME-1 (residual) | Domain callback-fixture hook `NOT IMPLEMENTED`; any non-mock adapter fails the domain conformance suites | architect | Partly | The first real adapter in each domain |
| DEPLOY-FPKEY-1 | Fingerprint HMAC key delivery to AWS | devops + security; **human** | No | Staging with the real resolver |
| HD-10.3-2 follow-ups | IAM/KMS for `awssm`, VPC endpoint vs `HTTPS_PROXY`, IRSA, account pinning | **human** (infrastructure decision) | No | `awssm` on AWS |
| CAS-RECON-SCALE-1 | Sweep is O(tenants × full history), serial | ledger-finance + architect | No | Multi-tenant real-money load |
| CAS-WIN-ANOMALY-1 | Detection-only large-win alert | casino + risk | No | Real-money casino go-live |
| CAS-WIN-IDEMP-1 | `postWin` has no already-posted short-circuit | casino + ledger-finance | No | G-6 (bonus-funded casino stakes) |
| Free-round / jackpot conformance case | `NOT IMPLEMENTED` (ADR 0025 amendment item 8) | casino | Yes | First real casino adapter |
| LEDGER-MANUAL-ADJ-4EYES-1 | No four-eyes manual adjustment / mismatch resolution | ledger-finance + security; own stage; **human** | No | Real-money go-live; casino key-compromise recovery |
| Payments statement reconciliation | Wallet ↔ PSP stream is `BLUEPRINT` design only (`reconciliation-model.md` §2.2); no stream in `internal/reconciliation` | payments + ledger-finance | Yes (file/API shape) | Real PSP go-live (`CLAUDE.md`: daily reconciliation per integration) |
| PAYWH-TS-1, PAYWH-RL-1, PAYWH-BRAND-1 | Deferred (ADR 0092) | payments + security | — | RL-1 pre-launch; BRAND-1 when a tenant has per-brand merchant accounts |
| PAY-SB-REPLAY-AUDIT-1 | Audit row ungated on `AlreadyPosted` | payments; sportsbook | No | Confirm or gate before real PSP |
| Earlier-stage blockers | Roadmap reconciliation §7: PLAT-ROLESPLIT-1 production step, KMS signing (ADR 0018), staff MFA (ADR 0017), audit client IP, backup/DR "NOT MET", no metrics backend, sportsbook items | various | No | Production launch |
| Human/legal | §15 of this paper | human | — | Launch |

## 5. A — Recommended provider-integration sequence

### 5.1 What each category's first real integration would exercise

| Dimension | Payments (PSP) | KYC / AML | Casino aggregator | Sportsbook |
|---|---|---|---|---|
| Launch-path position | Deposits come before any real-money play | Gates withdrawals and per-jurisdiction thresholds (BLUEPRINT §4.7 "tiered KYC … first withdrawal") | The B2C product; nothing to play without it | Widget on the shared wallet; BLUEPRINT §9 phase P3 |
| BLUEPRINT §9 phase | P1 "1 PSP" | P2 (lead time starts during P1) | P1 "1 aggregator" | P3 |
| Money at risk per defect | High (deposits, payouts) | None directly (no ledger writes) | High (inbound wallet path, p99 < 150 ms) | High (long-open liability) |
| Existing platform contract | Most mature: capability rows with amount limits, routing, reversal, withdrawal workflow; ADR 0022 §3 is the reference contract | State machine, normalized outcomes, reason bound, provider select | Capability contract, C1–C7, statement plumbing, rejection record | Settlement lifecycle, recon stream |
| Inbound webhook trust stack | Yes | Yes | Yes (every spin) | Not in the handle model (`domain` CHECK is payments/kyc/casino) |
| Outbound calls to restructure (PROV-OUTBOUND-CRED-1) | `Deposit`, `Withdraw`, `QueryStatus`; withdrawal submit holds a row lock across the call (hardest case) | `CreateVerification` | `Launch` | Not assessed |
| Reconciliation vs provider | **Missing** (no PSP stream) | Not a ledger stream | `casino_statement` exists; real source `PROVIDER DEPENDENT` | Stream exists; real source `PROVIDER DEPENDENT` |
| Open human blockers specific to it | Q1 fiat/crypto rail choice; a crypto gateway is blocked on the open ledger decision `crypto-custody-boundary.md` §4.1 | HDR-J-6 (markets → thresholds), legal RG/KYC/AML interpretation, cross-tenant KYC reuse | Aggregator "sub-operators under our credentials" question (BLUEPRINT §5) → credential model (§10) | HDR-SB-1, HDR-J-7, OB-1 |
| Category-specific prerequisites still open | PSP statement stream; decline taxonomy; state-machine mapping (ADR 0022 §5 items 5–6) | KYC-SANCTIONS-IF-1, KYC-HOSTED-SESSION-1 (if the vendor needs them); F-5 audit dedupe | Free-round case, CAS-WIN-ANOMALY-1, CAS-RECON-SCALE-1, latency SLO | Widen handle `domain`; exposure limits; test-route removal |

### 5.2 Recommended order — `RECOMMENDATION`

| Order | Category | Why |
|---|---|---|
| **1** | **Payments: one fiat PSP (or one non-custodial crypto payment gateway only after `crypto-custody-boundary.md` §4.1 is decided), deposits first, then payouts** | (a) Deposits are the first step of every real-money journey. (b) BLUEPRINT §1 calls payments "the hard part, from day one" and puts payment orchestration in phase one; §9 P1 names "1 PSP". (c) Payments has the most mature platform contract (ADR 0022 §2/§3/§5). The first real adapter therefore tests the trust stack against the contract everything else was modelled on. (d) The hardest part of PROV-OUTBOUND-CRED-1 (withdrawal submit holding a row lock across the outbound call) lives in payments. Designing it next to a real payout state machine avoids designing it twice. (e) It forces the missing wallet ↔ PSP reconciliation stream, a `CLAUDE.md` requirement for every integration. (f) High-risk acquirer onboarding has a long commercial lead time (BLUEPRINT §1, §9 "vendor contracting … run on other people's calendars"). |
| **2** | **KYC / AML** (intake starts in parallel with 1, at W0) | It gates withdrawals, and production self-service KYC is 503 today. It carries no ledger risk, so it is a good second run of the stack. Its per-jurisdiction thresholds depend on HDR-J-6 and legal review, and those have lead time. |
| **3** | **Casino aggregator** | It is the product, and 10.3 prepared its financial core. But it has the most category-specific prerequisites (free rounds, anomaly alert, recon scale, p99 latency), and it is only meaningful for real money once deposits exist. The aggregator credential question (§10) should be answered first. |
| **4** | **Sportsbook** | BLUEPRINT §9 places it in P3. It needs a new handle domain and HDR-SB-1 / HDR-J-7 / OB-1 answered. |
| Out | Crypto custodian (ADR 0008) | Separate `CryptoCustodyProvider` boundary. It is blocked on `crypto-custody-boundary.md` §4.1 and a human vendor choice. Not part of this sequence. |

**Alternative (`RECOMMENDATION`, second choice): KYC first.** Choose it if the PSP choice is held up
by the Q1 fiat/crypto question or acquirer onboarding, or if the human wants the lowest possible
financial blast radius for the first run of the new trust stack. What it proves: inbound scheme,
handle resolver, outbound credential, restructured `CreateVerification`, conformance fixture hook,
kill switch. What it does not prove: ledger posting, reconciliation against a provider, or the
payout restructuring. Payments would then follow as the second integration.

**Not recommended first: casino.** It is the strongest product argument. But the first real
integration would then combine the highest-traffic inbound money path with the first production
use of the trust stack and three unshipped casino-only prerequisites.

Whatever the order, **P-W1 (§17, vendor-independent prerequisites; list in §14) is the same.** It can start as soon
as it is authorized.

## 6. B — Vendor information requirements (intake checklist)

Every row is an intake question for the selected vendor. Nothing is assumed. "Blocks" means the
platform cannot integrate without an answer, or without a further ADR.

| # | Item | What we need from the vendor | Why it matters to our code | Blocks? |
|---|---|---|---|---|
| 1 | Vendor identity / contract | Legal entity, contract scope, which of our tenants and jurisdictions it covers | ADR 0006 licensing model; `provider_id` registration is a platform action (ADR 0022 §2.1) | Yes |
| 2 | API documentation | Versioned spec, changelog policy, deprecation notice period | Adapter mapping onto canonical request types (ADR 0022 §1, ADR 0025 §1, ADR 0028 §4) | Yes |
| 3 | Sandbox | Access terms, reset, test data, parity with production, whether it can call a non-public endpoint | Local-vs-staging split (§13); recorded fixtures for SC1 known-answer vectors | Yes |
| 4 | Outbound auth | Scheme (API key, OAuth client credentials, mTLS, request signing), token lifetime, separate scopes per credential | `httpclient.Authenticator`, `DerivedTokenCache`; mTLS is out of scope today (ADR 0093 A4); least privilege (ADR 0022 §4.2) | Yes |
| 5 | Webhook signature scheme | Algorithm, exact signed bytes, header names, key-id header or implicit key, encoding | `VerificationScheme.Extract/Verify`, `KeySelection` (`KeyFromHeader`/`KeyImplicit`) | Yes |
| 6 | Signed timestamp / replay | Is a timestamp inside the signed input? Documented tolerance? | ADR 0022 §3 point 10: `SignedTimestamp = true`, `0 < MaxSkew ≤ 10 min`, or a further ADR; SC7 fails for real schemes | Yes |
| 7 | Tenant / merchant model | Per-merchant keys? Is a merchant/account id signed? Sub-accounts per brand? "Sub-operators under our credentials?" (BLUEPRINT §5) | ADR 0022 §3 point 3: per-merchant keys and `BoundAccountID` match, or not integrable without an ADR; SC8; PAYWH-BRAND-1 trigger; §10 credential model | Yes |
| 8 | Key rotation | Overlap support, number of concurrent keys, rotation notice | `verify_only` overlap ≤ 7 days, at most one predecessor (ADR 0093 §1); SC9 | Yes |
| 9 | Currencies / assets | Supported assets, minor-unit exponent, amount encoding (integer, decimal string, float) | `Asset` registry exponent (ADR 0007); a float on the wire must be converted exactly at the adapter, never carried inward | Yes |
| 10 | Transaction model | Sync vs async, states, terminal states, partial amounts | State-machine mapping pending → settled → reversed (ADR 0022 §5 item 6) | Yes |
| 11 | Idempotency | Does the vendor accept our idempotency key on outbound calls? Is the inbound `provider_tx_id` unique and stable across retries? Maximum length? | `(provider_id, provider_tx_id)` unique constraint; PROVIDER-REF-BOUND-1 bound; `httpclient` retries only when marked idempotent | Yes |
| 12 | Retry semantics | Vendor retry schedule, what counts as acknowledgement, ordering guarantees | Duplicate/reordered/late callbacks; tombstone for unseen-original rollback | Yes |
| 13 | Settlement | Timing, batching, fees, reserves (BLUEPRINT §4.6 rolling reserve) | `settlement_behavior` capability; `psp_reserve`/`psp_clearing` postings; reconciliation cadence | Yes (payments) |
| 14 | Rollback / void / refund | Which reversals exist, who initiates, can they arrive before the original? | Casino rollback + tombstone; PAY-REV-1 reversal; capability flags | Yes |
| 15 | Reconciliation | Statement file or API, format, cadence, period boundaries, keys per line, totals | Provider-neutral statement source (`statement.CasinoStatementSource`; PSP equivalent missing) | Yes |
| 16 | Status query | A `QueryStatus`-style lookup, rate, consistency | Backstop when a callback is lost or refused (ADR 0093 consequences) | Yes (payments) |
| 17 | Limits | Amount min/max per asset/method/country | `amount_limits` child rows, `NUMERIC(38,0)` (ADR 0022 §2) | No |
| 18 | Rate limits | Outbound quotas, callback burst rates | Breaker/timeout tuning; PAYWH-RL-1 | No |
| 19 | SLAs | Availability, latency, the timeout the vendor applies to our wallet endpoint | BLUEPRINT §6: wallet callback p99 < 150 ms; availability ≥ 99.95% | Yes (casino) |
| 20 | Error model | Error codes, retriable vs final, decline taxonomy | Error mapping; cascade on decline (ADR 0022 §5 item 5) | Yes |
| 21 | Health endpoint | Status API, maintenance notices | `HealthStatus` in every provider interface; routing failover | No |
| 22 | Network | Source IPs of callbacks, whether our egress IP must be allowlisted, TLS requirements | Edge allowlist; static egress is an AWS/infrastructure decision (§13) | Staging |
| 23 | Data / privacy | PII sent and received, data residency, retention of raw payloads | HDR-J-3e/3f, O7; KYC reason bound; no raw evidence in `Reason` | Yes (KYC) |
| 24 | Compliance | Vendor licences/certificates per jurisdiction, AML screening scope, sanctions/PEP coverage | Legal review; KYC-SANCTIONS-IF-1; not an engineering sign-off | Human |
| 25 | Certification | Whether the vendor certifies our integration before go-live; GLI evidence it supplies (BLUEPRINT §8) | Release discipline; evidence pack | Human |
| 26 | Crypto specifics (if any) | Confirmations, reorgs, memo/tag, wrong-network deposits (BLUEPRINT §4.6) | Crypto rail blocked on `crypto-custody-boundary.md` §4.1 | Yes (crypto) |
| 27 | Shared-vendor privilege | If the vendor also does custody: separately scoped credentials? | ADR 0022 §4.2: a vendor that cannot issue them is a `security` vendor-selection finding | Yes (if applicable) |

## 7. C — Adapter boundary

The deterministic financial core is authoritative. A provider may report, request or confirm. Only
the core decides and posts.

| Layer | Owns | Existing packages / interfaces | Never |
|---|---|---|---|
| **CORE** (platform, provider-neutral) | Ledger postings and invariants, balances, idempotency constraint, tombstones, state machines, capability gating, reconciliation streams, audit, RLS tenant context, credential resolution, verify-before-parse orchestration | `internal/ledger`, `internal/wallet`, `internal/withdrawal`, `internal/payments` `Orchestrator`, `internal/casino` `Orchestrator`, `internal/kyc` `Orchestrator`, `internal/reconciliation`, `internal/webhookauth` (preamble, `Reason`, `Resolver`), `internal/providercred`, `internal/secretstore`, `internal/providerkind`, `internal/audit` | Branch on a provider id (ADR 0022 §1); trust a payload tenant; read Redis on the money path |
| **ADAPTER** (per vendor, our code) | Mapping vendor ↔ canonical types, vendor scheme `Extract/Verify`, vendor error → closed error set, amount/exponent conversion, outbound HTTP via `httpclient`, declared capabilities | `payments.PaymentProvider`, `casino.CasinoProvider`, `kyc.KYCProvider`, `webhookauth.VerificationScheme`, `internal/providers/httpclient` (`Authenticator`, bounded retry), `providerkind.ProductionEligible` marker | Post to the ledger; open DB transactions; cache credentials; hold SDK types or free-form passthrough fields |
| **PROVIDER-SPECIFIC VERIFICATION** (evidence per adapter) | Known-answer vectors, SC1–SC13 run, domain conformance with a callback fixture, sandbox recordings, capability-truthfulness tests, decline taxonomy | `internal/webhookauth/webhookauthtest` (`RunSchemeConformance`, `ConformanceManifest`), domain `RunProviderConformanceSuite`, `internal/providers/httpclient/conformance` | Relax a conformance case to let an adapter pass (ADR 0022 §3, 10.3 amendment) |

Rules carried from ADRs 0022/0025/0028 and binding on the first adapter:
- Verification is called by the orchestrator, not the adapter.
- The adapter parses only verified bytes.
- Pre-verification failures return the uniform 401.
- Exactly one credential binding is tried.
- `MarkProductionEligible()` is added to the scheme type and the adapter type only after SC1–SC13
  pass with a vendor vector.

## 8. D — First-provider integration elements

"Exists" cites the file or ADR. "Missing" is what the first real adapter needs.

| Element | Exists today | Missing |
|---|---|---|
| Configuration | `provider_capabilities` (0024/0030), casino capabilities (0035/0094), `internal/providers/config.go` | Real capability rows for the vendor; KYC capability is minimal (`SupportedDocumentTypes`, `SupportsCallback`); direct `tenant_id` FK on `provider_capabilities` (ADR 0022 §3 known gap) |
| Credentials | `provider_credential_handles` (0096), four-eyes activation, admin API (ADR 0093) | Real handle rows (human-provisioned); see §10 for the model gaps |
| Secret lookup | Resolver + `Fetcher` (breaker, singleflight, caches); `devfile`; `awssm` code | `awssm` on AWS (HD-10.3-2, DEPLOY-FPKEY-1); F-POOL-1 (§3) |
| Tenant/provider binding | Per-tenant route, `vendor_account_id` → `BoundAccountID`, SC3/SC4/SC8 | The vendor's real account binding (intake #7) |
| Health checks | `HealthStatus` on every provider interface; `/readyz` | A real implementation; routing failover on health is not verified here |
| Timeout / retry | `httpclient`: per-attempt timeout (default 10 s), bounded retry only when idempotent, `Sent` flag on timeout | Per-vendor values; outbound calls moved out of the DB tx (PROV-OUTBOUND-CRED-1) |
| Webhook verification | `VerificationScheme`, orchestrator-enforced `Verify`, SC1–SC13, synthetic/production split | The vendor scheme, its known-answer vector, and the manifest entry |
| Replay protection | Idempotency constraint; `timestamp_out_of_window`; SC7 mandatory for real schemes | The vendor's signed timestamp (intake #6); PAYWH-TS-1 stays open |
| Idempotency | DB-enforced `(provider_id, provider_tx_id)`; casino `postBet` short-circuit | CAS-WIN-IDEMP-1 (before G-6); outbound idempotency key (intake #11); PAY-SB-REPLAY-AUDIT-1 |
| Provider reference mapping | Opaque `provider_tx_id`; casino rounds (0080) | **PROVIDER-REF-BOUND-1**: one platform maximum, validated at the adapter after verification, plus a later CHECK migration |
| Error mapping | Closed `Reason` enum; point-7 adapter error contract; `httpclient` sentinel categories | The vendor error → closed-set mapping and decline taxonomy |
| Audit | Audit on every posting/transition; credential audit (fingerprint only); `webhook_key_verified` log | F-5 KYC `error` audit dedupe on replay (first real KYC adapter) |
| Reconciliation | `ledger_vs_projection`, `casino_consistency`, `casino_statement` (`MOCK` source), `sportsbook_settlement` | Real statement ingestion; **payments stream absent**; CAS-RECON-SCALE-1 |
| Observability | Structured logs with allow-listed fields; OTel SDK with `stdout` exporter | No metrics backend; no per-provider latency/error instruments (BLUEPRINT §7 "per-provider latency SLOs"); alarms `STAGING REQUIRED` |
| Kill switch | `provider_capabilities.status`; casino capability disable (new bets only); credential revocation, single actor (ADR 0093 §3) | PROV-REVOKE-ALL-1 (cross-tenant, per provider); an operator runbook per category |
| Fail-closed | Synthetic guard; nil resolver → 401 `no_resolver`; absent fingerprint key → no real subsystem; KYC select fails closed | Tripwire lifts only after PROV-OUTBOUND-CRED-1 |
| Testing | See §12 | Domain callback-fixture hook; vendor vectors; sandbox recordings |

## 9. E — Capability matrix model

Existing capability surfaces are separate per domain: payments `ProviderCapability` (ADR 0022 §2),
casino `supports_*` (ADR 0025 §4, 0094), KYC `Capabilities`, and scheme `Properties()`.

`RECOMMENDATION`: add one adapter-declared **integration capability manifest** per adapter. It is
code-declared and never tenant-editable (ADR 0022 §2.1). It sits alongside the existing
tenant-narrowing rows and does not replace them. Not registered; proposed ID `PROV-CAP-MATRIX-1`
(architect).

| Capability | Where it lives today | Fail-safe when the vendor does not support it (`RECOMMENDATION`) |
|---|---|---|
| Rollback / void | Casino `supports_rollback` + 0094 CHECK (bet requires settlement) | Refuse to enable new activity that the platform could not unwind; never stop settlement of existing exposure (CAS-CAP-ROLLBACK-1 rule) |
| Partial settlement | Not modelled | Treat as unsupported. Reject partial amounts at the adapter as a verified-malformed 4xx, and record them |
| Refunds / reversal | Payments `supports_refund_reversal`; PAY-REV-1 | Reversal only through a compensating posting; if unsupported, manual path (LEDGER-MANUAL-ADJ-4EYES-1, not built) |
| Multi-currency / asset | Payments asset lists + `amount_limits`; casino `supported_assets` | Route only assets the adapter declares; never convert implicitly (ADR 0007 `ConversionOperation`) |
| Async settlement | `settlement_behavior`, `callback_capabilities` | Pending state + `QueryStatus` polling + reconciliation; never assume a push |
| Webhook replay by vendor | Not modelled | Idempotency makes replay harmless; the absence of replay makes `QueryStatus` mandatory |
| Balance query (casino `Balance`) | Casino interface | Answer from the authoritative DB read only |
| Statement / reconciliation feed | Casino statement source (`MOCK`) | No go-live without one (`CLAUDE.md`: daily reconciliation per integration) |
| Status query | `QueryStatus` (payments), `GetVerification` (KYC) | Mandatory where callbacks are the only signal |
| Outbound idempotency key | `httpclient` idempotent marking | If absent, no automatic retry of money-moving calls; resolve by status query |
| Signed timestamp | Scheme `Properties().SignedTimestamp` | Registration refuses (ADR 0022 §3 point 10) |
| Key rotation overlap | `KeyImplicit` + `verify_only` | Rotation with a cut-over only; documented operator procedure |
| Free rounds / jackpots | Not supported | Must never map to a win (ADR 0025 amendment item 8); reject |

**General rule (`RECOMMENDATION`):** an unsupported capability is resolved in this order:
1. at startup (registration refuses a configuration that needs it); then
2. at new-activity time (gate new bets, deposits or verifications); and
3. never at settlement time.

The platform never emulates a missing capability silently.

## 10. F — Multi-tenant credential model

| Scope | Needed for | ADR 0093 today | Gap |
|---|---|---|---|
| Platform-wide (our contract, shared by tenants on our licence) | ADR 0006 platform-licence tenants whose vendor permits sub-operators under our credentials | **Not representable.** Every handle is tenant-owned (`tenant_id NOT NULL`, no platform policy). The **global** `UNIQUE (domain, provider_id, purpose, fingerprint)` forbids binding the same secret to two tenants | Needs an architecture decision (ADR): either per-tenant sub-accounts at the vendor (preferred by ADR 0022 §3 point 3) or a governed platform-scoped handle. Depends on the vendor's answer to BLUEPRINT §5's question |
| Tenant | Default case | `IMPLEMENTED` | — |
| Brand | Per-brand merchant accounts | Not modelled (no `brand_id` on handles) | PAYWH-BRAND-1 (deferred; trigger defined) |
| Jurisdiction | A tenant with separate vendor accounts per licence/jurisdiction | Not modelled; only distinct `key_id`s with one `active` per (tenant, domain, provider, purpose) | Needs an ADR if a vendor issues per-jurisdiction accounts |
| Provider account | Vendor merchant id | `vendor_account_id` (≤ 128 B), one per handle | Several accounts per tenant and provider need one `provider_id` per account or a model change |
| Purpose | Inbound vs outbound | `webhook_verify` / `outbound_api`, never shared | — |
| Isolation of secret material | B2B / BYOL | One task role reads every tenant's secrets; isolation by RLS + namespaced refs + fingerprints | Per-tenant IAM ABAC / KMS: trigger is the first B2B/BYOL tenant whose contract or regulator requires it (ADR 0093 consequences) |
| Cross-tenant revoke | Compromise at a shared vendor | Per-handle revoke only | PROV-REVOKE-ALL-1 (trigger: a second tenant on one provider) |

## 11. G — Commercial / B2B

| Model (ADR 0006) | Who holds the vendor contract | Credential consequence | Isolation consequence | Onboarding |
|---|---|---|---|---|
| Platform licence (our Anjouan licence) | Us | Platform-wide or per-tenant sub-account; §10 gap | Shared cluster + RLS | Configuration rows + handles through four-eyes; no code |
| Tenant licence (tenant is licensee, we operate) | To be decided per contract (human) | Per-tenant handles | May need per-tenant IAM/KMS | As above plus a licence row (`15-jurisdiction-and-licensing-model.md`) |
| BYOL (tenant brings licence and vendor contracts) | Tenant | Per-tenant handles, tenant-supplied secrets; partner-console secret writing is not built (R13) | Per-tenant IAM/KMS or deeper isolation likely | Tenant-supplied vendor accounts must be registered by platform admins today |

- BLUEPRINT §4.7: partners may arrive with their own KYC contract. The architecture already allows
  several adapters per domain, selected per tenant.
- The first real integration is for our own B2C tenant only. B2B onboarding remains unauthorized
  (ADR 0092 out of scope).
- Commercial terms, revenue share, cost attribution and pricing are **human decisions** and are not
  modelled here.

## 12. H — Testing plan for the first real provider

| Test class | What it proves | Local? |
|---|---|---|
| Scheme conformance SC1–SC13 + vendor known-answer vector | Signature, binding, replay window, rotation, error hygiene | Local |
| Domain conformance with a callback fixture | The adapter passes the payments/KYC/casino suites without MOCK type assertions | Local (the fixture hook must be built) |
| Contract tests against recorded sandbox exchanges | Mapping, error model, amount exponent | Local (recordings made by a human-authorized sandbox session) |
| Live sandbox integration | Real protocol behaviour | Needs sandbox credentials; inbound needs a reachable endpoint (§13) |
| Failure injection | Timeout before/after send (`Sent`), 5xx, malformed-after-verify, store outage | Local |
| Duplicate / reordered / delayed callbacks | Idempotent no-op with the same answer; tombstone; late original rejected | Local |
| Invalid, cross-tenant and cross-domain signatures | Uniform 401, no write, no audit | Local |
| Credential rotation / revocation | Overlap ≤ 7 days; revoke → next call fails closed | Local; drill on AWS is `STAGING REQUIRED` |
| Provider outage | Breaker, fail-closed, `QueryStatus` recovery | Local |
| Reconciliation mismatch | Divergent statement → P1 mismatch rows, no money write | Local (test-only divergent source) |
| Financial invariants | Σ debits = Σ credits; projection = recompute; no float | Local |
| Concurrency | Parallel callbacks on the same round/payment; lock order | Local (race + integration) |
| Recovery | Restart mid-flow; payout resolved by status query; no double pay | Local |
| Outbound precondition | Tripwire replaced by a test that no outbound call runs inside a DB tx | Local |
| Mutation evidence | Red-before-green per wave (as in 10.3 `evidence/`) | Local |
| Latency | Wallet callback p99 < 150 ms (BLUEPRINT §6) | Meaningful only on staging-like infrastructure |

## 13. I — AWS: what genuinely needs staging (no deployment proposed)

| Needs staging | Why it cannot be local | Human decision |
|---|---|---|
| `awssm` real use: IAM, KMS, network path, latency, cold cache, rotation and outage drills | Real AWS services | HD-10.3-2 follow-ups; DEPLOY-FPKEY-1 |
| Vendor sandbox callbacks reaching us | Needs a public HTTPS endpoint the vendor can call; edge allowlist of vendor IPs (if the vendor publishes them) | Staging authorization; alternatively a human-approved tunnel (not recommended without `security` review) |
| Allowlisting of our egress by the vendor (if required) | Needs stable egress addresses | Infrastructure decision |
| Latency / SLO under realistic network | p99 is infrastructure-dependent | Staging authorization |
| Multi-replica behaviour of the credential caches and breaker | Per-process state | Staging authorization |
| Alarms on `credential_store_unavailable`, `credential_integrity`, reconciliation P1 | Real alarm pipeline | Staging authorization; observability backend decision |
| Migrations 0094–0098 via `role-init` + `migrate` on AWS | Real RDS | Staging authorization |
| All items in completion report §9 | — | Staging authorization |

Everything else in this plan is local: synthetic PostgreSQL, the `devfile` backend, recorded
fixtures and MOCK adapters.

## 14. Work that can continue WITHOUT a vendor

Checked against the registry at `2876fa5`. "Authorization" column:
- **ARCH-OK**: fits already-approved architecture (the ADR exists); needs only stage authorization.
- **NEW-ADR**: needs a new architecture decision (architect + named owners), then stage authorization.
- **HUMAN**: needs a human product, legal or infrastructure decision.

**Every row needs the human's stage authorization before it starts.**

| ID (registry) | Work | Owner | Authorization |
|---|---|---|---|
| F-POOL-1 | See §3 (being fixed now) | architect + security | Per §3 |
| PROV-OUTBOUND-CRED-1 | Move `Deposit`, `Withdraw`, `QueryStatus`, `Launch`, `CreateVerification` outside the domain DB tx; tenant + credential through adapter request types. Payout needs an intent-then-call-then-record design | architect + ledger-finance + payments/casino/identity-compliance | **NEW-ADR** (financial flow restructuring; ADR 0093 §5 records it as a design decision) |
| CODE-HYGIENE-10.3-1 (item 2) | Bound/sweep `DerivedTokenCache` | architect + security | ARCH-OK (retention value to `security`) |
| CODE-HYGIENE-10.3-1 (items 1, 3–7) | Low hygiene | architect + security + ledger-finance | ARCH-OK |
| PROVIDER-REF-BOUND-1 | Platform maximum, adapter validation, later CHECK migration with pre-flight | ledger-finance + casino + payments (+ security) | ARCH-OK (the value needs `ledger-finance` + `security` agreement) |
| CAS-RECON-SCALE-1 | Incremental/watermarked checks, bounded parallelism, row retention | ledger-finance + architect (+ devops) | **NEW-ADR** (registry says "design + ADR"; touches ADR 0023 observability rule) |
| CAS-WIN-IDEMP-1 | `postWin` already-posted short-circuit | casino + ledger-finance | ARCH-OK (required before G-6; G-6 itself stays unauthorized) |
| CAS-WIN-ANOMALY-1 | Detection-only large-win alert | casino + risk | ARCH-OK (threshold is a product/risk value) |
| PAY-SB-REPLAY-AUDIT-1 | Confirm or gate audit on `AlreadyPosted` | payments; sportsbook | ARCH-OK |
| PROV-REVOKE-ALL-1 | Cross-tenant revoke per provider | security + architect | ARCH-OK (trigger not reached; optional) |
| WH-VENDOR-SCHEME-1 residual (no own ID) | Domain callback-fixture hook: optional fixture interface signing with the adapter's own `Sign` | architect | ARCH-OK, but ADR 0022 says it is reviewed together with the first adapter that needs it. `RECOMMENDATION`: build the interface now against the MOCK and review it again with the first adapter |
| Not registered — proposed `PROV-CAP-MATRIX-1` | Integration capability manifest + fail-safe rule (§9) | architect | **NEW-ADR** |
| Not registered — proposed `PAY-RECON-STMT-1` | Provider-neutral PSP statement source + `payments_statement` stream with a MOCK source (mirrors CAS-RECON-STMT-1) | payments + ledger-finance | ARCH-OK (`reconciliation-model.md` §2.2 is `BLUEPRINT` design) |
| Not registered — proposed `PROV-CONTRACT-HARNESS-1` | Recorded-exchange contract-test harness (fixture format, provenance, no credentials) | qa + architect | ARCH-OK |
| Not registered — proposed `PROV-OBS-1` | Per-provider latency/error metric instruments (OTel) | devops + architect | ARCH-OK for instruments; the metrics **backend** is **HUMAN** (roadmap §7 "OTel exporter PROVIDER DEPENDENT") |
| Not registered — proposed `PROV-KILLSWITCH-RUNBOOK-1` | One operator runbook per category: capability disable, revoke, expected stranded exposure | architect + security + casino/payments | ARCH-OK (docs) |
| LEDGER-MANUAL-ADJ-4EYES-1 | Four-eyes manual adjustment | ledger-finance + security | **HUMAN** (registered as its own later stage) |
| KYC-SANCTIONS-IF-1, KYC-HOSTED-SESSION-1 | Vendor-agnostic interfaces | identity-compliance | ARCH-OK, but only useful once the KYC vendor shape is known. `RECOMMENDATION`: defer to vendor intake |
| KYC-DOC-REJECTION-BOUND-1 | Bound + player visibility | identity-compliance | **HUMAN** for player visibility (HD-10.3-3 precedent) |
| PAYWH-TS-1 / PAYWH-RL-1 / PAYWH-BRAND-1 | Deferred per ADR 0092 | payments + security | Stay deferred unless the human reopens them |
| DEPLOY-FPKEY-1, HD-10.3-2 follow-ups | Infrastructure | devops + security | **HUMAN** |
| CR-CHECKLIST-HMAC-1 | Agent-configuration edit | human | **HUMAN** |

Proposed IDs above are **not** in the registry. They must be registered by the orchestrator
before any work starts.

## 15. Human decisions (carried forward, unchanged)

| Decision | State |
|---|---|
| ADR 0009 residual: gambling AUP, contractual permission, data residency, final production cloud provider | OPEN |
| HDR-J-6 (permitted markets for the first B2C brand) | OPEN |
| HDR-J-7 (OperationClass → Purpose) | OPEN |
| HDR-J-8 (location signal required/advisory) | OPEN |
| HDR-J-9 (location-signal staleness) | OPEN |
| HDR-M-1 (tenant country-enablement governance) | OPEN |
| HDR-M-2 (existing players after a country is disabled) | OPEN |
| HDR-SB-1 (trading-book liability; exposure ceiling) | OPEN |
| OB-1 (receivable / negative `player_cash` after a rolled-back won settlement) | OPEN |
| Licensing (beyond Anjouan) | OPEN |
| Legal (RG/KYC/AML interpretation per jurisdiction; HDR-J-3e/3f) | OPEN |
| Retail items 1–14 | OPEN |
| LEDGER-MANUAL-ADJ-4EYES-1 (own later stage) | OPEN |
| HD-10.3-2 infrastructure follow-ups (IAM/KMS for `awssm`, VPC endpoint vs `HTTPS_PROXY`, IRSA, account pinning), recorded as **future infrastructure decisions** | OPEN |
| Vendor selection (PSP, KYC/AML, casino aggregator, sportsbook, crypto custodian); production credentials; commercial pricing | OPEN |
| Bonus Engine Wave 4 | **Not authorized** |
| AI agents | **Not authorized**; ADR 0089 is architecture only |
| Also open, relevant to this plan | Q1-derived rail choice (fiat vs crypto payment gateway); `crypto-custody-boundary.md` §4.1 (ledger-finance decision); ADR 0022 §4.2 one vendor in two roles (business/risk); DEPLOY-FPKEY-1; staging deployment authorization; production launch authorization |

## 16. Dependencies

| Item | Depends on |
|---|---|
| Any non-synthetic adapter registered | PROV-OUTBOUND-CRED-1 complete; F-POOL-1 resolved (§3) |
| Real scheme production-eligible | SC1–SC13 with a vendor vector; manifest entry; callback-fixture hook |
| Real PSP go-live | The above + PSP statement stream + PROVIDER-REF-BOUND-1 + LEDGER-MANUAL-ADJ-4EYES-1 + staging acceptance + earlier-stage blockers (§4) |
| Crypto payment gateway | `crypto-custody-boundary.md` §4.1 decided |
| Real KYC go-live | HDR-J-6 + legal thresholds; KYC-SANCTIONS-IF-1 if screening is in the vendor scope |
| Real casino go-live | Free-round case, CAS-WIN-ANOMALY-1, CAS-RECON-SCALE-1, latency on staging, aggregator credential model (§10) |
| Sportsbook | HDR-SB-1, HDR-J-7, OB-1, handle `domain` widening |
| `awssm` on AWS | HD-10.3-2 follow-ups, DEPLOY-FPKEY-1 |
| B2B tenant | §10/§11 decisions; per-tenant isolation trigger |

## 17. Implementation waves (proposed; none authorized)

| Wave | Content | Needs vendor? | Exit gate |
|---|---|---|---|
| **P-W0 Vendor intake** | Human selects the first vendor (category per §5). Complete the §6 checklist from documentation and contract only. An architect paper maps the vendor onto §7–§10. An ADR is written wherever intake hits a "not integrable without an ADR" clause (ADR 0022 §3 points 3/10). `security` vendor-selection review (ADR 0022 §4.2) | Yes | Intake complete; no unanswered "Blocks = Yes" row; ADRs accepted |
| **P-W1 Vendor-independent prerequisites** (may run before or during P-W0) | F-POOL-1 (if not closed by §3); PROV-OUTBOUND-CRED-1 restructuring (new ADR first); `DerivedTokenCache` bound; PROVIDER-REF-BOUND-1; callback-fixture hook; PROV-CAP-MATRIX-1 ADR; PAY-RECON-STMT-1 (MOCK source) if payments is first; contract-test harness; per-provider metric instruments; kill-switch runbooks | No | CI green; mutation evidence; `security` + `ledger-finance` + `code-reviewer` + `qa` reviews; tripwire replaced by the real precondition test |
| **P-W2 Adapter against recorded fixtures** | Vendor scheme + known-answer vector; adapter mapping; error/decline taxonomy; state-machine mapping; capability manifest; conformance with fixture hook. Still unregistered in production | Documentation + recordings | SC1–SC13 and domain suite green; `MarkProductionEligible()` **not** yet added |
| **P-W3 Sandbox integration** | Human-authorized sandbox credentials (non-production) through four-eyes handles on `devfile`; outbound calls to the sandbox; inbound via the staging endpoint (P-W4) | Sandbox | Recorded evidence; no production credential anywhere |
| **P-W4 Governed staging deployment** | `awssm` IAM (HD-10.3-2), DEPLOY-FPKEY-1, vendor callbacks to staging, drills, alarms, latency | Sandbox | Human-authorized; completion report §9 items closed |
| **P-W5 Production-readiness review** | Real statement reconciliation over a sandbox period; remaining §4 blockers; certification evidence; `MarkProductionEligible()` added in a reviewed commit | Yes | **Stop.** Production launch authorization is a separate human decision |

Order: P-W0 ∥ P-W1 → P-W2 → P-W3 ↔ P-W4 → P-W5. Controls from P-W1 are never traded away to reach P-W3.

## 18. Explicit next gate

**GATE: NEXT-PROVIDER-PLANNING — STOP for human authorization.** Nothing in this paper has started.

The human is asked to:
1. Acknowledge §3 once the F-POOL-1 fix and review land (or accept the gap explicitly).
2. Choose the first category (§5: `RECOMMENDATION` payments; alternative KYC).
3. Authorize P-W1 (vendor-independent prerequisites), in full or by row of §14. The
   PROV-OUTBOUND-CRED-1 ADR is its first item.
4. Start P-W0 by supplying the information in §19.
5. Decide separately, not implied by 2–4: staging authorization, HD-10.3-2 follow-ups and
   DEPLOY-FPKEY-1.

## 19. Exact information needed from the human to select the first real provider

1. **Category:** confirm payments first, or choose KYC first.
2. **Rail (if payments):** fiat PSP or crypto payment gateway for the first integration. Crypto also
   needs the `crypto-custody-boundary.md` §4.1 ledger decision.
3. **Markets for the first B2C brand** (HDR-J-6), at least the first jurisdiction(s), since they
   decide vendor coverage, currencies and KYC thresholds.
4. **Currencies/assets** the first brand will accept.
5. **Shortlist** of 1–3 candidate vendors in the chosen category that you have, or can get, a
   commercial conversation with. The platform will not pick one.
6. **Per candidate:** API documentation and sandbox access terms (whether a contract or NDA is
   needed first), and who on your side owns the relationship.
7. **Licensing shape** for this vendor: our Anjouan licence only, or also future tenant/BYOL use,
   and the vendor's answer to "sub-operators under our credentials?" (BLUEPRINT §5).
8. **Permission** to hold vendor **sandbox** (non-production) credentials in the development
   `devfile` store under four-eyes activation. Production credentials stay out of scope.
9. **Whether** vendor sandbox callbacks may reach us before a staging deployment is authorized
   (default: no; recorded fixtures only).
