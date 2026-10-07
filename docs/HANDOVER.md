# iGaming Platform — Live Developer Handover

> **Snapshot date: 2026-10-06 (state-corrected after the merges, the environment diagnosis and the Class-B B5-B8 round). Code HEAD: `60f58ed` (code merged through B6+B7; the 0120/0121 merge commit remains `639a2f0`) on branch `claude/focused-wright-jw88w9` (= origin).** This file is the PRIMARY entry point for any engineer taking the project over from the Git repository alone. It is a *live* document: whoever merges a material change updates the affected section in the same change (registry: `HANDOVER-1`, `HANDOVER-LIVE-2026-10-06`). **State correction:** the signed-actor-proof (migration 0120) and stake-return/closure (migration 0121) branches are now MERGED; migrations on HEAD are 0001..0121 (`migrate verify` clean). See [section 25a](#25a-merged-tree-race-verification-state) for the verification state: the merged-tree `-race` sweep is INCOMPLETE because the execution environment repeatedly restarts (environment limitation, no application failure).
>
> Companions: hands-on commands in [`runbooks/developer-handover.md`](runbooks/developer-handover.md); dated snapshot with SHAs, residual register and evidence index in [`plans/prh2-hardening-round/developer-handover-2026-10-06.md`](plans/prh2-hardening-round/developer-handover-2026-10-06.md); human-decision register in [`governance/human-decision-register.md`](governance/human-decision-register.md).
>
> Label vocabulary used everywhere (from `CLAUDE.md`): `IMPLEMENTED`, `PARTIALLY IMPLEMENTED`, `MOCK`, `STUB`, `PROVIDER DEPENDENT`, `NOT IMPLEMENTED`, `BLOCKED`. Nothing in this document is a legal, regulatory or licensing approval, and **PRH-2 is NOT complete.** Evidence is LOCAL unless stated; there is no GitHub CI evidence (CI-BILLING-1).

## READ THIS FIRST

**What the product is.** A multi-tenant iGaming (online casino / sportsbook) platform written in Go on PostgreSQL 16. The first tenant is our own B2C casino brand (Anjouan-licensed). Later tenants are external B2B operators on the same code, some under our licence and some under their own. The platform owns identity, wallets and the ledger, bonuses, tenant configuration, back office, audit and reporting. It does **not** own games, odds, card acquiring, KYC document checks or crypto key custody; those are vendors behind internal interfaces.

**What has been built.** Stages 0 to 10.3 are accepted by the human owner (2026-09-26): identity, tenancy with database-enforced row-level security, an append-only double-entry ledger, multi-wallet, payments and withdrawals, casino and sportsbook integration slices, bonus engine (waves 1-3), KYC/AML/responsible-gaming enforcement, a risk engine, jurisdiction model, back office, B2C frontend, and a staging AWS design. After that, the "PRH" blocks hardened payments without any real provider.

**Where it stands.** Everything external is a **MOCK**: no real payment provider (PSP), KYC vendor, casino aggregator or sportsbook feed, no production credentials, no real money. AWS is **OFF** (nothing deployed). GitHub Actions CI is **blocked by billing**, so all test evidence is local. Migrations `0001..0121` are on HEAD `639a2f0` (the signed-actor-proof `0120` and stake-return/closure `0121` branches are merged); merged-tree `-race` verification is INCOMPLETE because the sandbox restarts (section 25a).

**What is being worked on.** PRH-2 ("payment readiness and provider-independent hardening, round 2") is **not complete**. The two ready branches are merged (0120, 0121). Open: finish merged-tree `-race` verification (INCOMPLETE, environment-limited; see 25a - run it in controlled batches or on a stable CI/self-hosted runner), some remaining "Class-B" payment hardening items, threat-register residuals (T3/T4/T5/T8) and several owner decisions. A sandbox PSP adapter is **not yet authorized**.

**What to do first.** (1) Read the [HOW A NEW DEVELOPER TAKES OVER](#how-a-new-developer-takes-over) section and the [DO-NOT-CHANGE RULES](#48-do-not-change-rules). (2) Verify your local Postgres and run `go run ./cmd/migrate verify` ([runbook](runbooks/developer-handover.md)). (3) Do **not** reconstruct history from any Claude conversation; the repository (this file, `docs/governance/task-registry.md`, ADRs) is the memory. (4) Pick work only from [36. EXACT NEXT ENGINEERING TASKS](#36-exact-next-engineering-tasks), and stop for human authorization at every gate listed in [31](#31-open-human-decisions).

---

# CONTINUING DEVELOPMENT

**The project is ACTIVE. Development continues after this handover.** This document is a baton pass, not a wrap-up. Stage gates still apply (`CLAUDE.md`): at the end of a stage, stop and ask for explicit authorization before starting the next one. The orchestration model (Master Orchestrator plus specialist agents under `.claude/agents/`) is described in `MASTER-BUILD-PROMPT.md`; a human developer takes the orchestrator's role and follows the same loop (requirement, boundary, implement, test, review, document, verify, update progress).

### IMMEDIATE
Can start **without any new human decision** (ordinary reversible engineering inside the already-approved PRH-2 scope):
- ~~Merge the two ready branches~~ DONE at `639a2f0` (signed-actor-proof 0120, stake-return/closure 0121; `migrate verify` 0001..0121 clean). Remaining verification: see [25a](#25a-merged-tree-race-verification-state). Reviews with conditions were met before merge.
- Re-read the `internal/payments` race result (status file `race_results/root_internal_payments.d149a64.status` in the orchestrator scratchpad; if lost, re-run that package per the runbook).
- Docs and registry hygiene (the known documentation conflicts in section 47a).
- Class-B **B5, B6, B7, B8 are now IMPLEMENTED and merged** (2026-10-06, security + ledger-finance reviewed, conditions met); **B11 (PAY-PAYOUT-UNBOUND-HOLD-1, tests + ADR 0095 s35.2 PO-1, MOCK; closes "keeps the hold", NOT "resolvable") and B12 (PAY-PAYOUT-DISPUTE-ALERT-1, raise-only, MOCK; ADR 0095 s42) are IMPLEMENTED and merged** (2026-10-07, security + ledger-finance reviewed, conditions met). **B13 is the next gate and is BLOCKED** (architect + owner); B14-B18 are sandbox-readiness-gate / adapter acceptance items.

### NEXT ENGINEERING ROUND
Next: complete merged-tree `-race` verification in controlled batches (or on a stable CI/self-hosted runner) when the environment allows - it must NOT block normal development; run `golangci-lint` v2.9.0 (built with Go 1.26.x) and the timing lane in its own lane; a PRH-2 gate report; then **stop and ask the owner** to authorize the next stage. Do not start the sandbox adapter, ALERT-DELIVERY-1 real channel, or the identity-store hardening (T3/T4) without the authorizations in [31](#31-open-human-decisions).

### PROVIDER PHASE
`PROVIDER DEPENDENT` and **not authorized**. Sequence already decided: Class-B prerequisites first, then a *sandbox-only* PSP adapter for a synthetic non-real-money tenant (owner said YES to sandbox before real alert delivery; ALERT-DELIVERY-1 stays a production blocker). Vendor choice, contracts and credentials are human-only. See [38](#38-provider-integration-roadmap).

### PRODUCTION PHASE
Not started and **BLOCKED** by the launch blockers in [33](#33-current-launch-blockers): licence/legal, real PSP/KYC, ALERT-DELIVERY-1, backup/DR, AWS deployment verification (PLAT-ROLESPLIT-1), green CI, hosting AUP. See [39](#39-production-roadmap).

### B2C
First brand frontend exists (`b2c/`, MOCK providers). Next steps need product and legal inputs (brand gating, jurisdiction content). See [40](#40-b2c-roadmap).

### B2B
Multi-tenancy, tenant configuration and partner/back-office foundations exist; second-tenant dry run and B2B onboarding are Stage 7+ and need explicit authorization. See [41](#41-b2b--retail-roadmap).

### RETAIL
Retail/POS is architecture-only (`NOT IMPLEMENTED`). No work without owner authorization.

### FUTURE
AI/agent features (ADR 0089, architecture only), crypto custody (ADR 0008, interface only), tenant isolation tightening (schema/database per tenant), analytics (CDC to ClickHouse, deferred).

**Work that can start immediately with no human decision:** the IMMEDIATE list above, test-only hardening that does not change financial semantics, documentation fixes, and read-only analysis. **Everything touching real providers, real money, AWS, Terraform/IAM, billing, legal interpretation, or ADR semantics needs a human decision first.**

---

# HOW A NEW DEVELOPER TAKES OVER

**You must NOT reconstruct history from the Claude conversation.** Everything needed is in Git. If something is not in the repo, treat it as unknown and ask the owner.

### Day 1
1. Clone `https://github.com/Diansalas/igaming-platform` (remote `origin`) and `git fetch --all`.
2. Check out the branch `claude/focused-wright-jw88w9` (or the docs branch `prh2-handover-docs` that contains this handover). The signed-actor-proof and stake-return/closure branches are merged into this branch (HEAD `639a2f0`); the `prh2-*` branches are retained.
3. Read this file **completely**, then `CLAUDE.md`, `MASTER-BUILD-PROMPT.md`, `docs/progress.md`, `docs/active-stage.md` (newest note on top).
4. Read the critical ADRs linked in [49](#49-important-adr--decision-index): 0001, 0002, 0007, 0019/0020, 0082, 0094, 0095 (esp. sections 35-40), 0099-0101, 0108, 0109, and 0110 (merged).
5. Verify local prerequisites (Go 1.26.x per `go.mod`, PostgreSQL 16, Node 22 for frontends, `golangci-lint` v2.9.0 built with Go 1.26.x). Details: [`runbooks/developer-handover.md`](runbooks/developer-handover.md).
6. Verify PostgreSQL is up and reachable as the application roles (never fix access with `ALTER ROLE` or `sudo`; see [47](#47-security--operational-warnings)).
7. Run migration verification: `go run ./cmd/migrate verify` (expect 0001..0119 on HEAD).
8. Run the appropriate tests (unit first; integration against a **private scratch database**, never the shared ones).
9. Inspect Git state: `git status`, `git log --oneline -20`, `git worktree list`, `git branch -vv`.
10. Review [35. CURRENT WORK IN PROGRESS](#35-current-work-in-progress) and [36. EXACT NEXT ENGINEERING TASKS](#36-exact-next-engineering-tasks).

### Day 2
1. Reproduce the dev environment from scratch (fresh DB, roles via the checked-in init scripts run by a human, `make migrate-up`, `make run`, `/healthz`, `/readyz`).
2. Reproduce the relevant tests for the package you will touch (integration tag, `-race`, `-p 1`).
3. Review the open blockers ([29](#29-open-security-items), [30](#30-open-payment-items), [33](#33-current-launch-blockers)).
4. Review the security and financial constraints ([23](#23-security-model), [24](#24-financial-safety-rules), [48](#48-do-not-change-rules)).
5. Begin the next **authorized** task from section 36. If it is not listed there as startable, ask first.

---

## 1. PROJECT PURPOSE

Commercial objective (`MASTER-BUILD-PROMPT.md`): (1) launch our own B2C casino brand as the first tenant; (2) reuse the same core to sell B2B infrastructure to licensed operators without forking. A new brand must need configuration (theme, catalogue, payment methods, currencies, languages, RG defaults, bonus templates, jurisdiction rules, provider credentials, domains), never new application code. The Blueprint (`iGaming-Platform-Blueprint.pdf`) is the primary requirements source; recommendations live in `docs/decisions/` labeled `RECOMMENDATION`. Software capability is never a claim of licensing or regulatory approval.

## 2. BUSINESS / PRODUCT MODEL

- **Tenants and brands.** Tenant = legal/operational customer; brand is a distinct entity (ADR 0012). Hybrid licensing: some tenants run under the platform licence, some bring their own (ADR 0006). Markets: Europe and LATAM, each modelled as distinct jurisdictions with their own rule configuration, never one rule set per region.
- **Money.** Crypto and fiat simultaneously: a player has a distinct wallet per asset (ADR 0007). Crypto key custody is delegated to an institutional custodian (ADR 0008; `NOT IMPLEMENTED`, interface only).
- **Hosting.** A major hyperscale cloud (ADR 0009); AWS staging was designed and exercised once, then torn down. Written gambling-AUP confirmation is required before production (open).
- **First tenant licence.** Anjouan. Any other jurisdiction is a human decision.
- Business decisions history: ADR 0005 (resolved at the Stage 0 to 1 gate).

## 3. CURRENT PRODUCT STATUS

| Area | Status (all external dependencies MOCK) |
|---|---|
| Identity, tenancy, RBAC, audit | `IMPLEMENTED` (open items in 29) |
| Ledger, wallet, multi-asset | `IMPLEMENTED` |
| Deposits, payouts, withdrawals, reconciliation | `IMPLEMENTED` against MOCK; real PSP `PROVIDER DEPENDENT` / `NOT IMPLEMENTED` |
| Manual adjustments (K2), force-resolution (K3), capability grants (K1) | `IMPLEMENTED` (four-eyes DB-enforced); signed actor proof MERGED (migration 0120, `PARTIALLY MITIGATED` threat model) |
| Casino | `IMPLEMENTED` against MOCK vendor (launch-token bootstrap over HTTP, wallet callbacks) |
| Sportsbook | `PARTIALLY IMPLEMENTED`: singles, placement, settlement lifecycle against MOCK |
| Bonus engine | `PARTIALLY IMPLEMENTED` (waves 1-3); Wave 4 not authorized; G-2 configurability and self-exclusion auto-void consumer `NOT IMPLEMENTED` |
| Gamification / reward / CRM / affiliate | architecture only, `NOT IMPLEMENTED` |
| KYC / AML | `PARTIALLY IMPLEMENTED` (KYC-ENFORCE-1) against MOCK |
| Responsible gaming | `PARTIALLY IMPLEMENTED` (WD-RG-1 open) |
| Risk and limits | `IMPLEMENTED` foundation (ADR 0031, 0083) |
| Jurisdiction | `IMPLEMENTED` foundation; legal content is a human/legal input |
| Retail / POS | `NOT IMPLEMENTED` (ADR 0035/0036) |
| Back office, B2C frontend | `IMPLEMENTED` MVP slices (Stages 5, 6, 7) |
| Alert delivery (pages a human) | `NOT IMPLEMENTED` (log sink and MOCK only) |
| AWS / production | OFF / `NOT IMPLEMENTED` |

Stage position: Stages 0-10.3 accepted; PRH (payment readiness) block and PRH-2 hardening round active; the final PRH-2 gate has **not** been passed. Note: `MASTER-BUILD-PROMPT.md` defines Stages 0-7; the repo later used sub-stages up to 10.3 (see conflicts, 47a).

## 4. CURRENT GIT STATE

- Remote: `origin` = `github.com/Diansalas/igaming-platform`. Branch `claude/focused-wright-jw88w9`, HEAD = origin = `d149a64` at snapshot time. Docs-only commits since `6a7b5f5`.
- Migrations on HEAD: `0001..0119`, 119 up/down pairs (238 files), gap-free.
- **MERGED at `639a2f0`** (previously 'ready, merge pending'):
  - `prh2-r5-signed-actor-proof` (final `8863e31`): migration 0120, ADR 0110 (HMAC signed actor proof for governed four-eyes writes). THREAT-MODEL-ARBITRARY-SQL-1 stays PARTIALLY MITIGATED (T3/H1, T4, T5, T8, T6, T7, s9.5).
  - `prh2-r5-stake-return-closure` (final `35cd2fb`): migration 0121 (Q-GP-5 terminal stake returns on non-active tenants; Q-GP-1 closure refused while open sportsbook bets, `GP020`; once-only casino rollback unique index). Casino open rounds are not representable (Q-GP-6 OPEN).
  - Migrations on HEAD `639a2f0`: 0001..0121, gap-free (`migrate verify` on `igaming_orch_local`: 121 applied).
- 52 agent worktrees exist under `.claude/worktrees/` (plus this one). **Preserve them; never auto-delete.** 44 UNKNOWN literal-named scratch databases were intentionally preserved; databases `igaming_orch_local` and `igaming_platform_ci_local` must be kept.
- Many feature branches (`prh2-*`) are merged-and-retained; do not delete without classification.

## 5. REPOSITORY STRUCTURE

| Path | Contents |
|---|---|
| `cmd/` | `platform-api` (the service), `migrate` (`up`/`down`/`status`/`verify`), `seed-admin` |
| `internal/` | One package per capability: `adjustment`, `admission`, `alerting`, `apierror`, `assetregistry`, `audit`, `auth`, `bonus`, `capability`, `casino`, `config`, `db`, `economicop`, `email`, `eventbus`, `geolocation`, `httpserver`, `idempotency`, `identity`, `identityresolution`, `jurisdiction`, `kyc`, `ledger`, `money`, `observability`, `operatingmarket`, `payments`, `providercred`, `providerkind`, `providerref`, `providers`, `reconciliation`, `rg`, `risk`, `secretstore`, `sportsbook`, `tenant`, `testsupport`, `txscope`, `validation`, `wallet`, `webhookauth`, `withdrawal` (the `actorproof` package arrives with 0120 on the unmerged branch) |
| `migrations/` | Numbered SQL up/down pairs, 0001..0119 |
| `deploy/` | `aws/` (Terraform, scripts, IAM, SQL, static tests), `docker/` (Dockerfiles), `docker-compose.dev.yml`, `init-app-role.sql`, `init-test-admin-role.dev.sql` |
| `b2c/`, `backoffice/` | Vite + React frontends (player brand site; staff/partner console) |
| `docs/` | `architecture/`, `decisions/` (ADRs), `governance/` (registry, status), `runbooks/`, `plans/`, `security/`, `testing/`, `api/`, `integrations/` |
| `.github/` | `workflows/ci.yml`, `workflows/ci-selfhosted.yml`, `CODEOWNERS` |
| `.claude/agents/` | Specialist agent definitions |

Topic-to-document map (preserved from the previous handover; some counts inside are historical, see 47a):

| Topic | Authoritative material |
|---|---|
| Repo structure | Top-level directories: `cmd/` (`platform-api`, `migrate`, `seed-admin`), `internal/` (domain packages, one per capability, e.g. `ledger`, `wallet`, `payments`, `kyc`, `rg`, `risk`, `alerting`, `tenant`, `providercred`), `migrations/` (numbered up/down SQL pairs, 0001..0115), `deploy/` (`aws/` Terraform, `docker/`, `docker-compose.dev.yml`, role-init SQL), `b2c/` (player frontend; [`architecture/37-b2c-brand-frontend-architecture.md`](architecture/37-b2c-brand-frontend-architecture.md); no README), `backoffice/` (staff/partner frontend; [`architecture/36-backoffice-and-partner-console-architecture.md`](architecture/36-backoffice-and-partner-console-architecture.md); has a README), `docs/` (architecture, decisions/ADRs, governance, runbooks, plans, testing), `.claude/agents/` (specialist definitions), `.github/` (`workflows/ci.yml`, `CODEOWNERS`) |
| Local development setup | [`runbooks/README.md`](runbooks/README.md) §"Local development setup"; `Makefile` targets `dev-db-up`, `dev-db-init-roles`, `dev-db-init-test-admin`, `test-integration`, `test-integration-runtime-role`; `.env.example`. The runbook's step 2 creates only the `igaming` role: `make dev-db-init-roles` (creates `igaming_runtime`) and `make dev-db-init-test-admin` (creates `igaming_test_admin`) are also needed for the runtime-role and migration tests, with the `TEST_*_DATABASE_URL` variables. Sudo/role steps are for humans only: sub-agents must never create, alter or escalate database roles (CLAUDE.md environment safety; incident record below) |
| Product and requirements | `iGaming-Platform-Blueprint.pdf`; [`architecture/00-system-overview.md`](architecture/00-system-overview.md), [`01-requirements-inventory.md`](architecture/01-requirements-inventory.md), [`14-mvp-scope-and-roadmap.md`](architecture/14-mvp-scope-and-roadmap.md) |
| Services and boundaries | [`architecture/02-domain-and-service-boundaries.md`](architecture/02-domain-and-service-boundaries.md), [`13-dependency-map-and-risk-register.md`](architecture/13-dependency-map-and-risk-register.md) |
| Database, RLS and tenancy | [`architecture/03-database-architecture.md`](architecture/03-database-architecture.md); [`security/runtime-role-separation.md`](security/runtime-role-separation.md); `migrations/` (0001..0117, 117 up/down pairs, gap-free; 0116 revokes TEMPORARY from PUBLIC and the runtime role, 0117 is alert routing readiness). Applied forward-only in deployed environments; `down` is a dev/CI tool and data-bearing downs refuse (03-database-architecture.md §Migrations). `go run ./cmd/migrate verify` checksums applied up-files and checks gaps only; CI reversibility covers only the last 4 steps. Numbers are allocated only by the orchestrator. Remediation: [`runbooks/migration-0101-payment-attempts-remediation.md`](runbooks/migration-0101-payment-attempts-remediation.md) |
| APIs | [`architecture/04-api-architecture.md`](architecture/04-api-architecture.md); [`docs/api/`](api/) |
| Identity, auth and RBAC | [`architecture/05-identity-architecture.md`](architecture/05-identity-architecture.md); [`security/security-architecture.md`](security/security-architecture.md); `internal/auth/permission.go` (role → permission map); open security items: TRIGGER-SEARCH-PATH-1 (HIGH, launch blocker), NULL-ARM-WRITE-1 (Medium), STAFF-LIFECYCLE-1; financial capability grants: ADR [0099](decisions/0099-scoped-financial-capability-grants.md), `internal/capability`, migration 0112 (G-T and G-P2 implemented; G-P1 deferred) |
| Wallet and ledger | [`architecture/06-wallet-ledger-architecture.md`](architecture/06-wallet-ledger-architecture.md), [`ledger-accounting-model.md`](architecture/ledger-accounting-model.md), [`financial-domain-model.md`](architecture/financial-domain-model.md), [`financial-transaction-flows.md`](architecture/financial-transaction-flows.md); governed manual adjustments (four-eyes, DB-enforced): ADR [0100](decisions/0100-governed-manual-adjustments.md), `internal/adjustment`, migration 0113 |
| Payments, deposits, payouts | [`architecture/07-payments-architecture.md`](architecture/07-payments-architecture.md), [`payment-orchestration.md`](architecture/payment-orchestration.md); ADR [0095](decisions/0095-provider-io-transaction-boundary-and-payment-contract.md) (transaction boundary, INV-DEP-1 §28, F-POOL-2 §29, kill switch §10) |
| Withdrawals | [`architecture/withdrawal-state-machine.md`](architecture/withdrawal-state-machine.md), [`withdrawal-policy-configuration.md`](architecture/withdrawal-policy-configuration.md) |
| Risk and limits | ADR [0031](decisions/0031-risk-and-limits-engine.md); `internal/risk`; ADR [0083](decisions/0083-sportsbook-jurisdiction-gating-and-cumulative-exposure.md) (cumulative exposure); CAS-RECON-SCALE-1 (registry) |
| Reconciliation | [`architecture/reconciliation-model.md`](architecture/reconciliation-model.md) |
| Casino, sportsbook | [`architecture/08-casino-integration-architecture.md`](architecture/08-casino-integration-architecture.md), [`09-sportsbook-architecture.md`](architecture/09-sportsbook-architecture.md); [`integrations/`](integrations/); sportsbook ADR [0038](decisions/0038-sportsbook-accounting-and-ledger-integration.md), [0047](decisions/0047-sportsbook-catalogue-jurisdiction-boundary-and-cumulative-risk-deferral.md), [0083](decisions/0083-sportsbook-jurisdiction-gating-and-cumulative-exposure.md); HDR-SB-1 / HDR-J-7 gate sportsbook go-live |
| KYC, AML, RG | [`architecture/11-kyc-aml-rg-architecture.md`](architecture/11-kyc-aml-rg-architecture.md); ADR [0096](decisions/0096-kyc-enforcement-boundary.md) |
| Bonus, gamification, retail | [`architecture/10-bonus-engine-architecture.md`](architecture/10-bonus-engine-architecture.md), `17`–`31` and [`26-retail-operations-architecture.md`](architecture/26-retail-operations-architecture.md); ADR [0032](decisions/0032-bonus-accounting.md)–[0034](decisions/0034-bonus-gamification-rg-kyc-identity-integration.md). Status: Wave 3 report [`governance/wave-3-report.md`](governance/wave-3-report.md); Wave 4 is recorded as unauthorized in `governance/project-status.md` (to confirm it is still current); the ADR 0042 G-2 configurability and the self-exclusion auto-void consumer are NOT IMPLEMENTED; CAS-WIN-IDEMP-1 is needed before bonus-funded casino stakes |
| Jurisdiction and licensing | [`architecture/15-jurisdiction-and-licensing-model.md`](architecture/15-jurisdiction-and-licensing-model.md); ADR [0006](decisions/0006-hybrid-licensing-and-jurisdiction-model.md), [0043](decisions/0043-jurisdiction-evaluation-policy-configuration.md), [0045](decisions/0045-operating-market-and-country-policy-foundation.md); [`governance/stage-4i-exit-register.md`](governance/stage-4i-exit-register.md) (operation-by-gate map; open HDR-J-7/8/9, HDR-M-1/2) |
| Tenant and brand | ADR [0002](decisions/0002-multi-tenancy-isolation-strategy.md), [0012](decisions/0012-brand-distinct-from-tenant.md), [0046](decisions/0046-tenant-licence-registry-rls.md); `internal/tenant`. B2C brand model is build-time (registry Stage 9.1 item 6) |
| Responsible gaming | ADR [0026](decisions/0026-responsible-gaming-player-status-enforcement-foundation.md); `internal/rg`; WD-RG-1 open (no RG/Risk gate on withdrawal) |
| Provider abstraction | ADR [0004](decisions/0004-provider-abstraction-pattern.md), [0080](decisions/0080-provider-integration-readiness-without-external-contracts.md), [0022](decisions/0022-payment-provider-agnosticism-and-capability-model.md), [0033](decisions/0033-provider-interoperability-and-external-bonus-engines.md), [0008](decisions/0008-crypto-custody-provider-abstraction.md); PROV-OUTBOUND-CRED-1 tripwire |
| Audit and reporting | [`architecture/12-audit-reporting-architecture.md`](architecture/12-audit-reporting-architecture.md); tenant-visible audit of platform actions: ADR [0104](decisions/0104-tenant-visible-audit-of-platform-actions.md) (kill switch only today) |
| Privacy | [`architecture/16-privacy.md`](architecture/16-privacy.md) |
| Webhook security | ADR [0097](decisions/0097-webhook-admission-and-rate-limiting.md) and the webhook ADRs it cites |
| Provider credentials and secrets | ADR [0093](decisions/0093-provider-credential-model-and-secret-store.md), [0094](decisions/0094-secret-resolution-resource-isolation.md) |
| Deployment and AWS (currently OFF) | [`architecture/38-deployment-architecture.md`](architecture/38-deployment-architecture.md); `deploy/`; [`runbooks/stage-9-4-staging-lifecycle-runbook.md`](runbooks/stage-9-4-staging-lifecycle-runbook.md); OFF evidence [`governance/staging-teardown-2026-09-26.md`](governance/staging-teardown-2026-09-26.md) (see it for retained resources); ADR [0084](decisions/0084-stage-9-3-staging-aws-architecture.md), [0086](decisions/0086-stage-9-4-staging-hardening-and-cost-optimization.md); [`runbooks/stage-9-3-staging-deployment-runbook.md`](runbooks/stage-9-3-staging-deployment-runbook.md), [`runbooks/stage-9-4-aws-account-verification.md`](runbooks/stage-9-4-aws-account-verification.md); open: ACCESS-ANALYZER-CHECK-1, HD-10.3-2, DEPLOY-FPKEY-1 |
| Operations, monitoring, backups | [`runbooks/`](runbooks/README.md): operational runbooks, observability and alerting, backup and DR, production configuration checklist |
| Testing and CI | [`testing/testing-strategy.md`](testing/testing-strategy.md); `.github/workflows/ci.yml`. GitHub CI is currently blocked by billing (registry CI-BILLING-1); local runs are never reported as CI. CI jobs: `build-test-lint`, `frontend`, `frontend-image`, `infrastructure`. Last green CI evidence: W0 runs #239-#243 (runs from #365 on did not start). BRANCH-PROTECTION-1: `main` is unprotected and CODEOWNERS is unenforced. Timing lane: `ci.yml` step "Timing-sensitive security tests, run alone" (8 tests, `-race`), bounds per ADR 0094 §9, never widened; latest LOCAL result 2026-10-05 is **NOT GREEN** ([`plans/payment-readiness/evidence/prh2-final-timing-lane.md`](plans/payment-readiness/evidence/prh2-final-timing-lane.md); decision needed; TEST-RESISO-RACE-1). Mutation evidence convention and timing-lane notes: `testing/testing-strategy.md` "Mutation evidence convention and timing lane"; files `plans/payment-readiness/evidence/*-mutation-kill.txt`. |
| AI architecture (future) | ADR 0089 (architecture only; nothing implemented) |
| Launch blockers | Section [Launch blockers (current)](#33-current-launch-blockers) below |
| Human decisions | Section [Human decisions awaiting the owner](#31-open-human-decisions) below; registry `HD-*` / `HQ-E1-*` / `HD-CTF-*` rows; ADR [0098](decisions/0098-human-decision-response-force-resolve-and-manual-adjustment.md), [0105](decisions/0105-human-decision-response-closed-tenant-funds-and-e1.md), [0042](decisions/0042-human-decision-response.md); HDR registers ADR [0039](decisions/0039-human-decision-register-stage-4h-b0-r7.md), [0044](decisions/0044-human-decision-register-stage-4i-phase-d.md) |
| Technical debt | Registry "QA Technical Debt Classification (S91-05)"; `testing/testing-strategy.md` "Linter" (integration-tagged files are not linted); `runbooks/observability-and-alerting.md` "Known gaps" |
| Governance and agents | [`governance/`](governance/): change control, ownership, agent registry; `.claude/agents/` (specialist definitions); DB credential incident record `governance/incident-2026-09-27-local-db-credential-mutation.md` |

## 6. ARCHITECTURE OVERVIEW

Single deployable Go service (`cmd/platform-api`, ADR 0010: one service, not the full service map), PostgreSQL 16 as system of record, Redis never authoritative for balances. Shared cluster with RLS, with a documented tightening path (ADR 0002). Domain packages are libraries inside the one binary; provider specifics sit behind interfaces (ADR 0004). Background loops (payments sweeper, alert dispatcher, reconciliation scheduler) run inside `platform-api`; the KYC outbox worker uses its own least-privilege identity (ADR 0106). Read: [`architecture/00-system-overview.md`](architecture/00-system-overview.md), [`02-domain-and-service-boundaries.md`](architecture/02-domain-and-service-boundaries.md), [`13-dependency-map-and-risk-register.md`](architecture/13-dependency-map-and-risk-register.md), [`38-deployment-architecture.md`](architecture/38-deployment-architecture.md).

Key cross-cutting patterns: provider I/O never inside a DB transaction that holds locks (ADR 0095); canonical financial lock ordering (ADR 0082); idempotency by DB unique constraints (ADR 0020); fail closed on ambiguity; every mutating admin/financial action audited.

## 7. CORE SERVICES / DOMAINS

| Domain | Package(s) | Notes |
|---|---|---|
| Ledger and wallet | `ledger`, `wallet`, `money`, `assetregistry`, `idempotency`, `economicop` | append-only double entry; projections |
| Payments | `payments`, `providercred`, `providerref`, `webhookauth`, `admission` | deposits, payouts, sweeper, force-resolution (K3) |
| Withdrawals | `withdrawal` | state machine, policies |
| Reconciliation | `reconciliation` | ledger-vs-projection, payment statement, casino/sportsbook cross-checks |
| Casino / sportsbook | `casino`, `sportsbook` | mock vendors, callbacks, settlement |
| Compliance | `kyc`, `rg`, `risk`, `jurisdiction`, `operatingmarket` | enforcement is our code |
| Identity, tenancy, auth | `identity`, `identityresolution`, `tenant`, `auth`, `capability`, `adjustment` | |
| Platform | `config`, `db`, `httpserver`, `audit`, `alerting`, `observability`, `secretstore`, `eventbus`, `txscope`, `email`, `geolocation` | |

## 8. IDENTITY / TENANCY / RBAC

- `tenant_id` is authoritative from server-side authenticated context only; never from the client (`CLAUDE.md`). Every tenant-owned table carries `tenant_id` and is protected by PostgreSQL RLS bound to a connection-level setting (ADR 0002; `internal/db`).
- Platform-scoped identities may hold a nil-tenant token (ADR 0011). Dual-scope RLS for audit (ADR 0013); persons access (ADR 0015); sessions RLS (ADR 0016).
- RBAC: role to permission map in `internal/auth/permission.go`; enforced server-side. Scoped financial capability grants (K1): ADR 0099, `internal/capability`, migration 0112 (G-T and G-P2 implemented; G-P1 deferred).
- The runtime must connect as `igaming_runtime` (NOSUPERUSER, NOBYPASSRLS, non-owner, TEMP revoked) - [`security/runtime-role-separation.md`](security/runtime-role-separation.md). PLAT-ROLESPLIT-1: code fix done; staging verification prepared, **not executed**.
- Open: NULL-ARM-WRITE-1 (NULL-tenant arms of some RLS policies are not mitigated by RLS), STAFF-LIFECYCLE-1, TENANT-STATUS-AUTHZ-1 (`ChangeStatus` has no authz and no HTTP route exists), H1 identity-store routes (see 29).
- Brand is build-time in the B2C frontend (registry Stage 9.1 item 6).

## 9. WALLET / LEDGER / FINANCIAL SYSTEM

Docs: [`architecture/06-wallet-ledger-architecture.md`](architecture/06-wallet-ledger-architecture.md), [`ledger-accounting-model.md`](architecture/ledger-accounting-model.md), [`financial-domain-model.md`](architecture/financial-domain-model.md), [`financial-transaction-flows.md`](architecture/financial-transaction-flows.md), [`reconciliation-model.md`](architecture/reconciliation-model.md); ADR 0001, 0019, 0020, 0021, 0082.

- Append-only, double-entry; `SUM(debits) = SUM(credits)`. Balances are projections recomputed from entries and diffed hourly (non-zero drift = P1). Integer minor units with per-asset exponent; crypto `NUMERIC(38,0)`. No floats.
- Wallet per asset; cross-asset movement is an explicit `ConversionOperation`. `player_locked` split into cash/bonus origin (migration 0048).
- Idempotency by unique constraint on `(provider_id, provider_tx_id)` (or equivalent), enforced by the database. Rollback of an unseen transaction writes a **tombstone** so a late original is rejected; the ledger refuses a tombstone that carries entries (GP-related, merged).
- Gameplay posting gate (migration 0118, merged): NEW wagering postings on non-active tenants are refused in the posting transaction (advisory-lock pair, status-change trigger, `GP010` ledger backstop for 7 gameplay types); replays are reads; tombstones are always written.
- Manual money movement only through governed paths: K2 manual adjustments (ADR 0100, migration 0113), K3 payment force-resolution M1/M2 (ADR 0101, migration 0115), both DB-enforced four-eyes with reason codes.
- Full rules: [24](#24-financial-safety-rules).

## 10. PAYMENTS

Docs: [`architecture/07-payments-architecture.md`](architecture/07-payments-architecture.md), [`payment-orchestration.md`](architecture/payment-orchestration.md), [`withdrawal-state-machine.md`](architecture/withdrawal-state-machine.md), ADR 0095 (the contract; sections 28 INV-DEP-1, 29 F-POOL-2, 10 kill switch, 35-40 PRH-2 rounds), ADR 0101, 0107, 0109.

- **ALL PROVIDERS MOCK.** `InitiateDepositAttempt` is the only deposit path; no provider call outside the gate (`TestPCG1_NoOutboundProviderCallOutsideTheGate`); outbound-credential tripwire `TestOutboundPrecondition_EveryWiredAdapterIsSynthetic` (PROV-OUTBOUND-CRED-1).
- One logical deposit intent produces at most one authoritative successful ledger posting (INV-DEP-1, migration 0107). A disputed second provider capture stays disputed and unposted (`multiple_success_for_intent` park).
- Payout reference binding guard at every bind site; payout tombstone counts as a conflict (B10, ADR 0109). Callback-mismatch parks bind the provider reference when valid and unconflicted (B3). Panic values are redacted (B1). MA020 preventive control widened (B4, migration 0119).
- Payments sweeper runs in `platform-api` (ADR 0095 section 37, MOCK only). Statements/reconciliation source is MOCK. Closed/suspended tenants are **observed** read-only by reconciliation (H-W1), never auto-dispatched/released/settled/cancelled.
- Real PSP: `PROVIDER DEPENDENT`, **no sandbox started, none authorized**; see 30 and 38.

## 11. CASINO

Docs: [`architecture/08-casino-integration-architecture.md`](architecture/08-casino-integration-architecture.md), [`integrations/`](integrations/), ADR 0025, 0048, 0103. Status: MOCK aggregator; launch-token bootstrap (ADR 0103, migration 0111) over HTTP against the MOCK vendor. Wallet callbacks are idempotent, tombstone-aware, and refused for NEW wagering on non-active tenants. Open: CAS-GAME-KILL-BET-1 (blocks real-money casino), CAS-BET-REQUIRES-BOOTSTRAP-1, CAS-PLAYER-REF-1, CAS-WIN-ANOMALY-1, CAS-STMT-IO-1, CAS-WIN-IDEMP-1 (before bonus-funded stakes), Q-GP-2/3/4/6 (see 28 and 31). Casino has no loss/round-close callback, so "open round" is not representable (Q-GP-6).

## 12. SPORTSBOOK

Docs: [`architecture/09-sportsbook-architecture.md`](architecture/09-sportsbook-architecture.md), ADR 0038, 0047, 0083, 0087, 0088. Singles, placement and settlement lifecycle against a MOCK/in-house mode; catalogue fetch is outside any transaction (SB-CATALOGUE-IO-1 done). Go-live is gated by HDR-SB-1 / HDR-J-7 (ADR 0083). Needs a bounded catalogue-fetch timeout before a real adapter; the real bet-placement contract is unknown. On the unmerged closure branch, tenant closure is refused while open sportsbook bets exist (Q-GP-1).

## 13. BONUS / GAMIFICATION / REWARD

Docs: [`architecture/10-bonus-engine-architecture.md`](architecture/10-bonus-engine-architecture.md), `17`-`25`, `27`-`31`, ADR 0032-0034, 0040, 0042, 0089. Bonus Engine waves 1-3 (migrations 0050-0070) `IMPLEMENTED`; see [`governance/wave-3-report.md`](governance/wave-3-report.md). Wave 4 is recorded as unauthorized in `governance/project-status.md` (confirm still current). Gamification, tournaments, missions, reward marketplace/orchestration, segmentation, CRM, affiliate: architecture only, `NOT IMPLEMENTED`. ADR 0042 G-2 configurability and the self-exclusion auto-void consumer: `NOT IMPLEMENTED`. HR-9 guard rejects postings to bonus accounts until the bonus expense generator exists.

## 14. KYC / AML

Docs: [`architecture/11-kyc-aml-rg-architecture.md`](architecture/11-kyc-aml-rg-architecture.md), ADR 0028, 0029, 0096, 0106. MOCK vendor. Submissions are asynchronous through `kyc_submission_outbox` (migration 0114, dedicated least-privilege `kyc_submission_worker`); terminal failure raises alert kind `kyc.submission_failed_terminal` (durable only, no delivery). A KYC-store outage fails closed (withdrawal returns 503). Enforcement is our code (ADR 0096). Open: KYC-ENFORCE-1 remainder, HD-KYC-1..8 thresholds, HQ-E1-1..4 (legal/compliance), KYC-OUTBOX-REQUEUE-1, KYC-E1-FOLLOWUPS-1, KYC-FX-AGG-1 (no FX source). AML monitoring beyond the above: `NOT IMPLEMENTED` unless named in the registry.

## 15. RESPONSIBLE GAMING

ADR 0026; `internal/rg`. Player-status enforcement foundation is implemented; self-exclusion hardening done in R6. Open: WD-RG-1 (no RG/Risk gate on withdrawal; not scoped, owner to decide), self-exclusion auto-void consumer `NOT IMPLEMENTED`, jurisdiction-specific RG limits depend on legal inputs.

## 16. RISK / LIMITS

ADR 0031; `internal/risk` (fail-closed, exponent-aware). Cumulative sportsbook exposure: ADR 0083. Open: CAS-RECON-SCALE-1 (before real-money scale), cumulative-exposure deferrals recorded in ADR 0047.

## 17. JURISDICTION

Docs: [`architecture/15-jurisdiction-and-licensing-model.md`](architecture/15-jurisdiction-and-licensing-model.md), ADR 0006, 0043, 0045, 0046, [`governance/stage-4i-exit-register.md`](governance/stage-4i-exit-register.md). Jurisdiction is first-class pluggable configuration (KYC thresholds, RG rules, reporting, geo-blocking, data residency). Tenant licence registry is RLS-protected (ADR 0046). Fixed jurisdiction: Anjouan only; anything else is a human decision. Open: HDR-J-7/8/9, HDR-M-1/2 (stage-4i exit register), legal review residuals of HDR-J-1..6.

## 18. RETAIL / POS

`NOT IMPLEMENTED`. Architecture only: [`architecture/26-retail-operations-architecture.md`](architecture/26-retail-operations-architecture.md); ADR 0035 (agent network accounting, Proposed) and ADR 0036 (hierarchy RBAC, `NOT IMPLEMENTED`). There is no retail/POS package under `internal/`. Any work requires explicit owner authorization.

## 19. BACK OFFICE

`backoffice/` (staff/partner console, React + Vite; has its own README). Architecture: [`architecture/36-backoffice-and-partner-console-architecture.md`](architecture/36-backoffice-and-partner-console-architecture.md). Stage 5 MVP complete. Authorization is enforced server-side only. Staff-facing governed flows (K1 grants, K2 adjustments, K3 force-resolution, kill switch) have API routes; the frontend must never be the authority. Tenant-visible audit of platform actions: ADR 0104 (kill switch only today). Brand/partner configuration editing from the partner console is partial; consult the architecture document before assuming a feature exists.

## 20. PROVIDER INTEGRATION ARCHITECTURE

ADR 0004 (every integration is a subsystem: adapter, idempotency/retry semantics, per-tenant credentials, state machine, daily reconciliation), 0080, 0022, 0033, 0008, 0093 (credential model), 0094 (secret resolution isolation), [`plans/next-real-provider-integration-planning-gate.md`](plans/next-real-provider-integration-planning-gate.md). Gates before the first non-MOCK adapter: PROV-OUTBOUND-CRED-1 tripwire, F-POOL-2, ADR 0095 section 35.4. Crypto private keys never enter the platform (`CryptoCustodyProvider`, ADR 0008).

**Mock vs real matrix (preserved from the previous handover; keep current).**

Every external capability is **MOCK** or **NOT IMPLEMENTED** today. No real vendor is selected or integrated, and no
production credential exists.

| Capability | Status | Interface / gate before going real |
|---|---|---|
| Payments (PSP), deposits and payouts | MOCK (`internal/payments` mock provider) | ADR 0095; PROV-OUTBOUND-CRED-1 tripwire `TestOutboundPrecondition_EveryWiredAdapterIsSynthetic`. The legacy deposit chain is deleted (PRH-2 E2); `InitiateDepositAttempt` is the only deposit path, guarded by `TestPCG1_NoOutboundProviderCallOutsideTheGate`. Deposit references are validated on every outcome, the amount echo is required on sync success, and parked attempts never return a redirect (PRH-2 C, ADR 0095 §34). The poll path checks amount, asset and echoed reference and never posts on a missing amount (PRH-2 D1, ADR 0095 §36). The payments sweeper runs in `cmd/platform-api` (PRH-2 H, runbook §12; MOCK only). PAY-KYC-UNAVAIL-1 closed by F-pay (`2dc8d10`). Open: PAY-K3-STATEMENT-SOURCE-WIRING-1, PAY-H-FOLLOWUPS-1, PAYOUT-AMOUNT-DISPUTE-1, WITHDRAWAL-REVERSAL-1; PAY-DEPOSIT-ESCALATION-1 and PAY-RECEIPT-T4-DRAIN-TEST-1 before any real PSP; before any real PSP or real-money tenant: PAY-RECON-PARKED-CAPTURE-STANDING-1 and MA020-SYNC-MISMATCH-1; PAY-PSP-CONTRACT-INVDEP1; PAY-SEC-LAUNCH-1 |
| Payment statements / reconciliation source | MOCK source (PRH-I5) | Real PSP statement format (provider dependent) |
| Casino aggregator | MOCK (`internal/casino` mock). The launch-token bootstrap endpoint (ADR 0103, migration 0111) is IMPLEMENTED against the MOCK vendor over HTTP. | CAS-REVOKE-CONSUMED-1 done (PRH-2 A, 0108). Before a real vendor: CAS-PLAYER-REF-1, a vendor-side bootstrap parse hook (ADR 0103 §13), CAS-GAME-KILL-BET-1 and CAS-BET-REQUIRES-BOOTSTRAP-1. Tripwire. |
| Sportsbook provider | MOCK / in-house mock mode (ADR 0087/0088) | Catalogue fetch is outside any transaction (SB-CATALOGUE-IO-1, done). A bounded fetch timeout is needed before a real adapter. The bet-placement contract is unknown. |
| Payment force resolution (K3) | MOCK. Staff-initiated, four-eyes, DB-enforced M1 (deposit evidence-only) and M2 (payout declared paid / not paid via `withdrawal.Complete`/`Fail`), migration 0115, ADR 0101. Statement source is NOT wired (`m2_declare_not_paid` refused), alert delivery NOT IMPLEMENTED, closed-tenant funds DESIGN ONLY (ADR 0107). Local evidence only. Real-money preconditions: PAY-K3-STATEMENT-SOURCE-WIRING-1, TRIGGER-SEARCH-PATH-1 (HIGH launch blocker), ALERT-DELIVERY-1, PAY-K3-FOLLOWUPS-1. | PAY-K3-STATEMENT-SOURCE-WIRING-1, PAY-K3-MR020-HTTP-MAPPING-1, PAY-K3-FOLLOWUPS-1 |
| KYC vendor | MOCK. Player KYC create/submit is asynchronous through the `kyc_submission_outbox` (migration 0114, ADR 0106; dedicated least-privilege `kyc_submission_worker` identity; terminal failure raises the dedicated alert kind `kyc.submission_failed_terminal`, durable only, no delivery channel; local evidence only; HQ-E1-1..4 open). A KYC-store outage fails closed: the decision and audit are recorded (savepoint), and withdrawal returns 503 (PRH-2 F-kyc). (Sweeper: `PAYMENTS_SWEEP_INTERVAL_SECONDS`, default 15s, ADR 0095 §37, MOCK only, non-active tenants resolution-only; nothing pages a human or a stalled sweeper while ALERT-DELIVERY-1 is OPEN.) Payout and deposit KYC gates treat unavailable as retryable with a recorded decision row, never as deny (PRH-2 F-pay, ADR 0096 §24); payout adapter error-path references are validated before persistence. | KYC-OUTBOX-REQUEUE-1, KYC-E1-FOLLOWUPS-1, HQ-E1-1..4; LF-I3-5 `RecordDecision`-error half; KYC-ENFORCE-1; HD-KYC-1..8 thresholds |
| Crypto custody | NOT IMPLEMENTED (interface only, ADR 0008) | Custodian selection (human) |
| Email | MOCK (`internal/email` mock provider) | Vendor selection |
| Geolocation | MOCK (`internal/geolocation/mock_provider.go`) | Vendor selection |
| FX / rate source | NOT IMPLEMENTED (none exists; KYC-FX-AGG-1) | Human input / design |
| Affiliate platform | NOT IMPLEMENTED | Vendor selection (13-dependency-map) |
| CI self-hosted runner | PREPARED, NOT ACTIVE (CI-BILLING-1 open): [`runbooks/self-hosted-runner.md`](runbooks/self-hosted-runner.md), manual-only `.github/workflows/ci-selfhosted.yml`; awaiting the owner's separate Mac setup authorization | CI-SELFHOSTED-RUNNER-READINESS-2026-10-05 |
| Alert delivery / paging | Durable alerts (`internal/alerting`, migration 0110, ADR 0102): log sink IMPLEMENTED, mock sink MOCK. The dispatcher runs in `cmd/platform-api` with the log sink only (PRH-2 I-wire, merged `dcaa2c6`); every alert is `unrouted`, nothing is delivered to any person. No routes or recipients are configured. ALERT-DELIVERY-1 OPEN (closure conditions: task-registry PRH-2-IWIRE-MERGE-STATE). | HD-PRH2-4-OPS (real recipients); a real channel adapter must dedupe on `<alert_id>:<step>` (ADR 0102 §16.3) | **2026-10-05 (R2-E):** routing readiness smallest cut (migration 0117, ADR 0102 §18) is IMPLEMENTED/MOCK: routes are disabled by default, a DB guard refuses any human-notification route, unrouted deliveries are recorded with a reason, `alert_routing_ready{severity}` and the platform-only status endpoint report NOT READY with the exact missing operator input; the `alertingtest` channel is test-only. NO real channel, NO recipient, NO vendor: log/mock delivery is never a human notification.
| Secret store | `devfile`/memory locally; `awssm` backend exists (AWS OFF) | ADR 0093/0094; production configuration checklist |

## 21. AI / FUTURE AGENT ARCHITECTURE

ADR 0089: architecture boundary only (Bonus/Gamification/Reward). **Nothing is implemented.** No AI agent has any financial or administrative authority in the product; the Claude specialist agents in `.claude/agents/` are development-process roles, not product features. Any product AI feature needs its own decision.

## 22. DATABASE / MIGRATIONS

- PostgreSQL 16. Architecture: [`architecture/03-database-architecture.md`](architecture/03-database-architecture.md). 121 up/down pairs `0001..0121` on HEAD `639a2f0`. Numbers are allocated only by the orchestrator (never pick one yourself): 0120 = signed actor proof and 0121 = stake return/closure are used by the unmerged branches.
- Forward-only in deployed environments. `cmd/migrate down` refuses unless `APP_ENV` is explicitly `development` or `staging` (an unset or `production` value is refused; `cmd/migrate/main.go`). Data-bearing downs refuse.
- `go run ./cmd/migrate verify` checksums applied up-files and checks version gaps only (PLAT-MIGDRIFT-1). CI reversibility covers only the last 4 steps.
- A restore or `CREATE DATABASE` does not carry the TEMP revoke; `schema_migrations` still says 0116 so it will not re-apply: follow `runbooks/operational-runbooks.md` section 7 step 5.
- Remediation precedent: [`runbooks/migration-0101-payment-attempts-remediation.md`](runbooks/migration-0101-payment-attempts-remediation.md).
- Notable recent migrations: 0107 INV-DEP-1; 0108 casino revoke-consumed; 0110 durable alerting; 0111 casino bootstrap; 0112 capability grants (K1); 0113 manual adjustments (K2); 0114 KYC outbox; 0115 force resolution (K3); 0116 REVOKE TEMP; 0117 alert routing readiness; 0118 gameplay posting tenant-status gate; 0119 MA020 sync mismatch. (Task registry rows give the authoritative mapping; ADR numbers and migration numbers are different sequences.)

## 23. SECURITY MODEL

Docs: [`security/security-architecture.md`](security/security-architecture.md), [`security/runtime-role-separation.md`](security/runtime-role-separation.md), ADR 0016, 0017/0018 (MFA and signing, `NOT IMPLEMENTED`), 0093, 0094, 0097 (webhook admission), 0108.

**Accepted threat model (owner decision THREAT-MODEL-ARBITRARY-SQL-1 = YES, 2026-10-05).** A compromised or stolen `igaming_runtime` database credential, or arbitrary SQL executed as that role (SQL injection, compromised application process), is **INSIDE the production threat model** for DATABASE-enforced controls (four-eyes, actor checks, tenant isolation). Consequence: RLS and triggers alone do not suffice where a session can set `app.*` settings; the signed actor proof (below) is the authorized mitigation.

**Signed actor proof (SIGNED-ACTOR-PROOF = AUTHORIZED).** MERGED at `639a2f0` (branch `prh2-r5-signed-actor-proof`, final `8863e31`), migration 0120, ADR 0110. Scope: the application server, after authenticating the principal and authorizing the action, signs a short-lived (30 s issuer, 60 s verifier cap) HMAC-SHA256 proof over actor, scope, tenant, operation, target, payload hash and a nonce. An owner-owned `SECURITY DEFINER` verifier (`actor_proof_require`), fed by an owner-only key table and nonce table, is called from a last-firing trigger `zz_actor_proof_guard` on **nine tables** (K2 adjustment requests/approvals, K3 manual resolutions/approvals, K1 capability grant requests/approvals/grants, financial policy changes and approvals). No valid proof, no write (SQLSTATE `AP001..AP005`). Keys: `ACTOR_PROOF_KEYS`, `ACTOR_PROOF_ACTIVE_KID` via the secret store, never in Git; production startup refuses without an active key.

**THREAT-MODEL-ARBITRARY-SQL-1 is PARTIALLY MITIGATED, never "closed".** Residuals (ADR 0110 section 10a):

| # | Residual | State |
|---|---|---|
| T1 | Impersonate real admins by GUC to approve K2/K3/K1/policy | CLOSED by the proof (on the branch) |
| T2 | Mint/impersonate staff by SQL as actors | CLOSED for K1/K2/K3/policy |
| T3 / H1 | Mint or take over a staff row, or forge a refresh `sessions` row, then authenticate over HTTP and obtain genuine proofs | OPEN, deferred, owner authorization required (keyed refresh-token hashes, keyed staff credential verification, owner-only staff lifecycle) |
| T4 | `player_credential_tokens` use an unkeyed hash; SQL-inserted row redeemable (player account takeover) | OPEN, deferred |
| T5 | Ordinary non-governed posting paths (deposits, withdrawals, casino/sportsbook postings, provider callbacks) are authorized by GUC alone | OPEN, outside authorized scope; limited by ledger invariants and hourly drift reconciliation, not by an actor proof |
| T6 | Application-host compromise holds the signing key | OPEN, inherent |
| T7 | Owner/migration role can read the symmetric key | OPEN, accepted |
| **T8** | Other dual-control flows OUTSIDE the nine tables still take the actor from GUCs/app values with no proof: kill-switch release (0105), provider credential handles (0096), KYC enforcement policy (0103), asset-registry dual control (0047), withdrawal approvals/policies (0026/0034), bonus change governance (0063), casino catalogue dual control (0086/0089). A runtime session impersonating two admins can still approve in those flows | NOT mitigated, deferred, owner authorization required |
| s9.5 | A proof captured on the wire can be used once within its lifetime for exactly the write it names | accepted wire residual (TLS in production) |

Other security rules: never commit secrets; never store raw PAN (hosted fields/redirect only); tenant from server context only; every mutating admin/financial action writes an append-only audit record (actor, tenant, entity, before/after, IP, reason code); security-sensitive work needs `security` review before it is called complete.

**Secret names inventory (preserved from the previous handover; names only, never values).**

Values never go into Git. Locally they come from an untracked environment. For a deployed
environment they come from the secret store per ADR 0093/0094 and
[`runbooks/production-configuration-checklist.md`](runbooks/production-configuration-checklist.md).
The checked-in `.env.example` holds placeholders only.

| Name | Holds | Where it belongs |
|---|---|---|
| `DATABASE_URL` | Runtime DB connection. Locally it includes the dev credential. In staging/ECS (ADR 0086) it is NON-secret and the password arrives separately as `PGPASSWORD` | Local untracked environment; ECS task environment (non-secret form) |
| `PGPASSWORD` | Runtime DB password in deployed environments | ECS secret sourced from Secrets Manager `igaming-staging/db-runtime` (or the RDS-managed master secret) |
| `IGAMING_RUNTIME_PASSWORD` | Password used by the role-init task to create the runtime DB role (`deploy/aws/sql/init-runtime-role.rds.sql`) | Secrets Manager (role-init task) |
| `JWT_SIGNING_SECRET`, `JWT_PREVIOUS_SECRET` | Access-token signing keys and rotation | Deployment secret store; AWS name `igaming-staging/jwt-signing-secret` |
| `PROVIDER_CREDENTIAL_FINGERPRINT_KEY` | Key for provider-credential fingerprints (ADR 0093) | Deployment secret store. **No AWS secret exists for it yet** (DEPLOY-FPKEY-1) |
| `SEED_ADMIN_PASSWORD` | One-off initial admin password (`cmd/seed-admin`) | Supplied at seed time only, never stored locally; AWS name `igaming-staging/seed-admin-password` |
| `devfile` secret store | File-backed store for development only, at `SECRETSTORE_DEVFILE_ROOT` (default `./.secrets/dev`, git-ignored, mode 0700) | Developer machine only |
| Provider credentials (per tenant, per provider) | PSP/casino/KYC API and webhook secrets | Secret store, referenced by `provider_credentials` rows (ADR 0093); never in env or Git |
| `TEST_DATABASE_URL`, `TEST_RUNTIME_DATABASE_URL`, `TEST_ADMIN_DATABASE_URL` | Synthetic local/CI test DBs | Local harness / CI only |

Committed dev/CI placeholder credentials (`Makefile`, `deploy/init-app-role.sql`, `deploy/init-test-admin-role.dev.sql`, `deploy/docker-compose.dev.yml`, `.github/workflows/ci.yml`, `.env.example`) are intentional synthetic values for local and CI databases and tokens, never valid anywhere else. `.env.example` placeholders are refused outside development.

Non-secret configuration (`APP_ENV`, `JWT_ACTIVE_KID`, `JWT_PREVIOUS_KID` (key ids, not secrets), timeouts, intervals, rate limits, `SECRETSTORE_BACKENDS`,
`AWS_REGION`, OTel settings and so on) is read in `internal/config/config.go`.

## 24. FINANCIAL SAFETY RULES

These are rules, not suggestions. Changing any requires an approved decision (ADR) recorded in `docs/decisions/` and `ledger-finance` + `security` review.

1. Ledger append-only, double-entry, `SUM(debits) = SUM(credits)`; never `UPDATE` a balance; balances are projections, recomputed and diffed hourly; drift is P1.
2. No floats for money; integer minor units with per-asset exponent; `NUMERIC(38,0)` for crypto.
3. Distinct wallet per asset; cross-asset movement only as an explicit auditable `ConversionOperation`.
4. Every financial write idempotent via a DB unique constraint (not check-then-insert). Corrections are compensating entries, never edits/deletes. A rollback of an unseen transaction writes a tombstone.
5. **One logical deposit intent produces at most one authoritative successful ledger posting.** A **disputed second provider capture stays disputed and unposted**.
6. Four-eyes for governed money movements, enforced in the database; **actor/subject separation** (the initiator cannot approve; the beneficiary cannot be an approver of their own adjustment); manual adjustments need a reason code and four-eyes above a threshold.
7. Tenant isolation by RLS; **provider reference binding** (a provider reference binds to one attempt/withdrawal, never rebound to a foreign one); **fail closed on ambiguity** (park, never guess).
8. **No arbitrary financial mutation:** money moves only through the governed paths (ledger posting API, K2, K3). Approved manual-resolution paths are M1 (deposit evidence-only) and M2 (payout declared paid / not paid via `withdrawal.Complete`/`Fail`).
9. **Reconciliation never auto-releases or dispatches closed-tenant player funds.** It may observe suspended/closed tenants read-only (H-W1); it posts, dispatches, settles and cancels nothing.
10. Cache (Redis) never holds an authoritative balance and is never read on the bet/settlement path; the authoritative read is in the same DB transaction as the write.
11. Gameplay rules: **NEW WAGERING is refused on non-active tenants** (migration 0118, merged). **RETURN / REVERSAL / SETTLEMENT of existing activity** is treated separately: per owner decision Q-GP-5, terminal stake returns are allowed for valid existing rounds on non-active tenants (casino rollback of a posted bet; sportsbook void of an open bet and void-after-settlement), idempotent, owner-verified, with no new exposure - implemented on the **unmerged** branch `prh2-r5-stake-return-closure` (migration 0121). Per Q-GP-1, **tenant closure is refused while open rounds exist** (sportsbook bets: `GP020`; casino open rounds are not representable, Q-GP-6 open). Reconciliation may observe closed tenants but never auto-releases or settles.
12. Financial work is not done without tests for: normal, duplicate, concurrency, retry, partial failure, rollback, settlement, reconciliation, provider callbacks, idempotency, authorization, auditability. Mutation-kill evidence is recorded per workstream.

## 25. TESTING / QA

Docs: [`testing/testing-strategy.md`](testing/testing-strategy.md) (including "Scratch databases", "Mutation evidence convention and timing lane"). Layers: unit (`go test ./...`), integration (`-tags integration`, needs `TEST_DATABASE_URL`, `TEST_RUNTIME_DATABASE_URL`, `TEST_ADMIN_DATABASE_URL`), `-race`, adversarial runtime-role probes (`internal/db/runtime_role_separation_test.go`), mutation-kill evidence (`docs/plans/payment-readiness/evidence/*-mutation-kill.txt` and `docs/plans/prh2-hardening-round/*-mutation-kill.txt`), frontend vitest, `deploy/aws/tests/run-static-checks.sh`.

**Timing lane.** 8 tests assert wall-clock security bounds (ADR 0094 section 9: no request over 500 ms, at most 4 store calls, no transaction held over 400 ms): `TestStoreOutage_DoesNotPinPool`, `..._ProductionPoolSize` (`./internal/providercred/`) and `TestResolutionIsolation_NormalOperation`, `_OneTenantStoreOutage`, `_MultipleTenantsOutage`, `_SimultaneousOnset_Bounded`, `_ConnectionExhaustion`, `_FinancialDuringOutage` (`./internal/httpserver/`). They are skipped by name in the main sweep (`-skip`) and run alone in their own lane, never retried, bound **unchanged at 500 ms**. Status: environment-dependent; characterized 40/40 on one host, fails on a slower host (NOT GREEN there). The criterion decision (environment-calibrated relative assertion vs controlled runner) is open for owner/architect/security; the bound is never loosened without a recorded decision. Evidence: [`plans/payment-readiness/evidence/prh2-final-timing-lane.md`](plans/payment-readiness/evidence/prh2-final-timing-lane.md).

**Race verification (LOCAL, not GitHub CI):** pre-merge tree `d149a64`: 54/54 packages PASS under `-race -tags integration -count=1 -p 1` (timing lane skipped; `internal/payments` 1341 s). **Merged tree `639a2f0`: INCOMPLETE** - 34 packages carry PASS (13 carried from `d149a64` because neither they nor their dependencies changed, 21 run at `639a2f0`), `internal/httpserver` and `internal/kyc` were interrupted, the rest were not run on the merged tree; no FAIL result exists. See [25a](#25a-merged-tree-race-verification-state).


## 25a. MERGED-TREE RACE VERIFICATION STATE

**Status: INCOMPLETE - environment limitation, NOT an identified application failure. Do not read this as a PASS and do not claim production readiness from it.**

- Tree: `639a2f011421bf5905fb69ae0091f76249dae299` (branch `claude/focused-wright-jw88w9`, = origin, clean). Migrations 0001..0121; `migrate verify` clean ("all applied migrations verified clean, no version gaps"); `igaming_orch_local` has 121 applied.
- Pre-merge tree `d149a64`: 54/54 packages PASS (`-race -tags integration -count=1 -p 1`, timing lane skipped).
- Merged tree: 34 packages PASS (13 carried from `d149a64` because the package and all its dependencies are unchanged by the merges, determined by `go list -deps`; 21 run at `639a2f0`), **0 FAIL**, `internal/httpserver` and `internal/kyc` interrupted, remaining ~19 packages (including `internal/payments`, `casino`, `sportsbook`, `reconciliation`, `adjustment`) not run on the merged tree.
- Per-batch evidence: orchestrator scratchpad `race_results/*.639a2f0.status|out` (not in git; if lost, rerun). Do not delete it.
- **Why incomplete (evidence, 2026-10-06):** the sandbox container was reset at least five times while a sweep was running or between turns. Each reset: uptime restarts at ~0 min, PostgreSQL is down, all background processes are gone. `dmesg` after the last reset shows `random: crng reseeded due to virtual machine fork`, `virtio_blk ... detected capacity change`, `EXT4-fs (vda): mounting unchecked fs ... without journal`, and PID 1 is `/process_api --firecracker-init` started at the boot time: i.e. the Firecracker microVM is snapshotted/forked and restored by the runtime, which discards running processes. A PostgreSQL checkpoint at 18:01:56 UTC and the last test-output write at 17:55 UTC bracket the last freeze; the next boot was 20:22 UTC. No OOM evidence (15 GiB RAM, ~15 GiB available, no swap use), no inode pressure (6%), disk at 14 GB free after the authorized `go clean -cache` (it was 5.8 GB free before; the session disk allowance, not the 252 GB device size, is the real limit). Classification: **external container/runtime restart (VM snapshot/restore)**; the exact trigger is outside the accessible environment and is not proven beyond that. Background jobs survive only while a Claude turn is actively running.
- **Consequences / rules:** (1) this limitation must not block normal development; (2) do not repeatedly restart a full sweep in this sandbox; (3) run the remaining packages in controlled batches (`internal/httpserver`, `internal/kyc`, `internal/payments` first), each as a foreground step within an active turn, or run the whole suite on a stable CI/self-hosted runner (`docs/runbooks/self-hosted-runner.md`) once available; (4) never fabricate a PASS; carried-over PASS markers are valid only while the package and its dependencies are unchanged; (5) recovery after a reset: `service postgresql start`, wait for `pg_isready`, confirm 121 migrations, resume only the missing packages.
- Not affected by this limitation: unit tests, targeted integration suites per branch (each merged branch passed its own targeted `-race` suites and mutation evidence before merge; see the registry rows), `migrate verify`, build, `go vet`, `gofmt`, `golangci-lint` v2.9.0 (0 issues at merge time).

## 26. AWS / INFRASTRUCTURE

AWS is **OFF**. Nothing is deployed; no deployment is authorized. Staging architecture exists (Terraform under `deploy/aws/`, ADR 0084, 0086, [`architecture/38-deployment-architecture.md`](architecture/38-deployment-architecture.md)) and was torn down 2026-09-26 ([`governance/staging-teardown-2026-09-26.md`](governance/staging-teardown-2026-09-26.md), lists retained resources). Last AWS-deployed commit: `9190d5d01da076141a1f70d7e5897a573d3b18f4`; current code (migrations to 0119+) has never run on AWS. Account `765578795051`, region `eu-central-1` (from the runbooks).

**PLAT-ROLESPLIT-1:** code fix done (runtime role `igaming_runtime`, non-owner; dev `make run`/`.env.example` still use the owner role). The existing approved staging architecture was assessed SUFFICIENT (no Terraform/IAM change needed). The operator-run staging verification procedure is [`runbooks/plat-rolesplit-staging-verification.md`](runbooks/plat-rolesplit-staging-verification.md): **prepared, NOT executed.** Open before the next deployment: ACCESS-ANALYZER-CHECK-1, DEPLOY-FPKEY-1, HD-10.3-2 (SECRETSTORE-AWS-1 IAM part). Production runbooks: [`runbooks/production-configuration-checklist.md`](runbooks/production-configuration-checklist.md), [`backup-and-disaster-recovery.md`](runbooks/backup-and-disaster-recovery.md) (NOT MET).

## 27. CI/CD

- `.github/workflows/ci.yml` (GitHub-hosted, jobs `build-test-lint`, `frontend`, `frontend-image`, `infrastructure`): gofmt, vet, golangci-lint **v2.9.0** via `golangci-lint-action@v9` (pinned; the binary must be built with Go >= the `go.mod` Go 1.26), dependency drift, `govulncheck`, build, migrations up and `verify`, unit `-race`, integration `-race` with the timing lane skipped by name, then the timing lane alone, migration reversibility.
- **GitHub Actions is BLOCKED by billing (CI-BILLING-1).** Runs from #365 on never started; last green evidence W0 runs #239-#243. **Do not modify billing or the plan** (owner decision). Local evidence only; never describe a local run as CI. BRANCH-PROTECTION-1: `main` unprotected, CODEOWNERS unenforced.
- Self-hosted Mac runner: **PREPARED, NOT installed/registered** - [`runbooks/self-hosted-runner.md`](runbooks/self-hosted-runner.md) and `.github/workflows/ci-selfhosted.yml` (`workflow_dispatch` only). Setup needs separate owner authorization. Its output would be labeled SELF-HOSTED CI EVIDENCE, distinct from LOCAL and GITHUB-HOSTED CI; whether it satisfies F-POOL-1 K1 / PRH-FPOOL1 is a ruling for architect + security + owner.
- Deployment: `deploy/aws/scripts/deploy.sh` (interactive, never `-auto-approve`), lifecycle in [`runbooks/stage-9-4-staging-lifecycle-runbook.md`](runbooks/stage-9-4-staging-lifecycle-runbook.md).

## 28. CURRENT PRH-2 STATUS

**PRH-2 is NOT COMPLETE.** The final gate has not been passed and is stopped for owner authorization; nothing here approves anything. Plans and reports: [`plans/prh2-hardening-round/plan.md`](plans/prh2-hardening-round/plan.md), [`final-gate-report.md`](plans/prh2-hardening-round/final-gate-report.md), [`final-gate-report-round2.md`](plans/prh2-hardening-round/final-gate-report-round2.md), [`blocker-clearing-report.md`](plans/prh2-hardening-round/blocker-clearing-report.md). Snapshot detail: [`plans/prh2-hardening-round/developer-handover-2026-10-06.md`](plans/prh2-hardening-round/developer-handover-2026-10-06.md).

| Status | Items |
|---|---|
| **COMPLETED (merged in HEAD, LOCAL evidence, MOCK)** | Workstreams A, B, C, D1, D2, E1, E2, E3, F-kyc, F-pay, G1, H, I-core, I-wire, J, K1, K2, K3; R2-A (0116 REVOKE TEMP, ADR 0108), R2-E (0117 alert routing readiness), R2-F; R3 H-W1 (closed-tenant observation); R3 gameplay gate 0118; Class-B B1 (panic redaction), B2 (T4 drain test), B3 (callback mismatch bind), B4 MA020 (0119), B9, B10 (ADR 0109) |
| **IN PROGRESS / INCOMPLETE** | Merged-tree (`639a2f0`) `-race` verification: 34 PASS, 0 FAIL, remainder unverified because the execution environment restarts (see 25a) |
| **COMPLETED (merged `639a2f0`)** | `prh2-r5-signed-actor-proof@8863e31` (0120, ADR 0110, PARTIALLY MITIGATED threat model); `prh2-r5-stake-return-closure@35cd2fb` (0121). Reviews done, conditions met. Merged-tree `-race` verification INCOMPLETE (25a) |
| **PARTIALLY MITIGATED** | THREAT-MODEL-ARBITRARY-SQL-1 (T1/T2 closed on the branch; T3/H1, T4, T5, T8, T6, T7, s9.5 residual); TRIGGER-SEARCH-PATH-1 (exploit path closed in code by 0116; stays a launch blocker until deployed and verified); NULL-ARM-WRITE-1 (hardening item, not mitigated by RLS) |
| **BLOCKED** | B13 payout destination binding (architect + owner); sandbox PSP adapter (not authorized); AWS verification (no deployment authorized); green CI (billing) |
| **DEFERRED** | B14-B18 (adapter acceptance criteria / sandbox readiness gate items); H1 identity-store hardening; T8 flows; nonce pruning automation; asymmetric proof |
| **REQUIRES HUMAN DECISION** | Q-GP-6, Q-GP-2, Q-GP-3, Q-GP-4, T3/T4/T8 authorization, H1, TENANT-STATUS-AUTHZ-1, ADR 0094 timing-lane criterion, ALERT-DELIVERY-1 recipients, HQ-E1-1/3/4, HD-KYC-1..8 (see 31) |

**Class-B items (payment hardening before a sandbox PSP adapter).** B1, B2, B3, B4, B5, B6, B7, B8, B9, B10: COMPLETED (B5/B6/B7/B8 merged at `60f58ed`; residual follow-ups: PAY-PAYOUT-ASSET-ECHO-TEST-1, deposit T12 callers must land with `TestB6_T6Drain_AfterT12Resubmit_*`, A2/A4/A5/A6 in ADR 0095 s41). Remaining (from the orchestrator facts; the registry classification `PAY-FOLLOWUPS-CLASSIFICATION-2026-10-05` has the underlying IDs): B5 poll echo hardening (PAY-POLL-ECHO-HARDENING-1); B6 deposit escalation (PAY-DEPOSIT-ESCALATION-1); B7 no-reference deposit never polled (H(11)); B8 tenant-status read inside the claim transaction (H(1)); B11 payout unbound-hold tests plus ADR (PAY-PAYOUT-UNBOUND-HOLD-1) DONE; B12 payout dispute alert (PAY-PAYOUT-DISPUTE-ALERT-1) DONE; B13 payout destination binding (PAY-SEC-LAUNCH-1) **BLOCKED**; B14-B18 adapter acceptance criteria / sandbox readiness gate. The exact definitions of B14-B18 are not spelled out in the repository files I read (see "items not verified" in the snapshot).

**Q-GP questions (gameplay on non-active tenants).** Q-GP-1 DECIDED (closure refused with open rounds; casino half not representable). Q-GP-5 DECIDED (terminal stake returns allowed). Q-GP-2 OPEN (a `tenant_not_active` class in `casino_callback_rejections`). Q-GP-3 OPEN (public casino webhook answers 401 before verification, so no durable provider-claim evidence). Q-GP-4 / R3-RECON-NONACTIVE-STREAMS-1 OPEN (`casino_consistency` / `sportsbook_settlement` not run for non-active tenants). Q-GP-6 OPEN (what signal closes a casino round). Source: ADR 0095 section 40.5-40.6.

## 29. OPEN SECURITY ITEMS

- THREAT-MODEL-ARBITRARY-SQL-1 residuals T3/H1, T4, T5, T8, T6, T7 (section 23). Deferred items need owner authorization before work.
- TRIGGER-SEARCH-PATH-1 (+ `-UPGRADE`, `-0116`): **HIGH, launch blocker until deployed and verified** (0116 applied in every real-money environment, verification SQL passing, service connecting as `igaming_runtime`, no other TEMP/CREATE holder). Pinning the 0026..0113 functions is `NOT IMPLEMENTED` (defence in depth).
- NULL-ARM-WRITE-1 (Medium; hardening before the second tenant/platform admin). STAFF-LIFECYCLE-1. MANUAL-ADJ-LINK-1 (blocks first real-money tenant). TENANT-STATUS-AUTHZ-1.
- MA020-K2-VISIBILITY-1 (grant K2 session SELECT on `payment_attempt_reference_evidence`, F-VIS stranding needs runbook note and decision), STMT-TABLE-INSERT-RLS-1, MA020-RECLINE-CHECK-1 (reversal-line part done on B4).
- PANIC-LOG-VALUE-1, REDACTEDREASON-PASSTHROUGH-1 (before real-provider go-live). PROV-OUTBOUND-CRED-1 and F-POOL-2 per domain before the first non-MOCK adapter. WEBHOOK-EDGE-1, WEBHOOK-PATH-TOKEN-1 (gated by HD-PRH-1).
- No `idle_in_transaction_session_timeout` on the runtime role (a stuck session can block a tenant status change; deploy decision open).
- AWS: ACCESS-ANALYZER-CHECK-1, DEPLOY-FPKEY-1, HD-10.3-2. BRANCH-PROTECTION-1. Hosting AUP.

## 30. OPEN PAYMENT ITEMS

- Class-B B5-B8, B11, B12 DONE; B13 (blocked, human/architect decision), B14-B18 (sandbox gate items); NEW required-before-any-non-MOCK-payout follow-ups: PAY-PAYOUT-UNBOUND-STANDING-1 (unbound payout holds have no standing reconciliation coverage; deposit-shaped signals can falsely clear payout findings; finding text suggests allocation), PAY-PAYOUT-UNBOUND-RESOLVE-1 (R-K3-8 resolution path; payments + LF), LEDGER-SUSPENSE-B-1 must refuse payout attempts, PAY-PAYOUT-CALLBACK-AUDIT-1 (callback T14/T15/mismatch/tombstone cells write no audit row), B12 residual R-5 (mismatched-amount success on declined payout does not page), ALERT-DELIVERY-1 dispatcher must not hold alerts row locks across network I/O, PAY-PAYOUT-ASSET-ECHO-TEST-1; PAY-PARK-BIND-RACE-1 (`drive.go parkDepositAttempt` bind lacks savepoint/audit scoping); PAY-PAYOUT-CONTRADICTION-HOLD-1 (no operator resolution path for a refused payout park; resolve before live payouts).
- Before the first real PSP / real-money tenant: PAY-K3-STATEMENT-SOURCE-WIRING-1 (real PSP statement source; H-W1), PAY-K3-FOLLOWUPS-1, PAYOUT-AMOUNT-DISPUTE-1, WITHDRAWAL-REVERSAL-1, PAY-SEC-LAUNCH-1, PAY-H-FOLLOWUPS-1, PAY-RECON-POLL-REF-CLEAR-1 + PAY-RECON-PARKED-CAPTURE-STANDING-1, PRH-I1-MANIFEST-2/3/4, PAY-PSP-CONTRACT-INVDEP1 (vendor-selection criterion).
- R3 launch conditions: R3-OBS-BOUND-1 (unbounded observation work), R3-FIRST-REAL-SOURCE-READONLY-1 (read/reporting-scoped credential, own read-only review), R3-LISTING-FAILURE-LOG-ONLY-1 (listing failure is log-only).
- ALERT-DELIVERY-1 OPEN: no channel, no recipient; a stalled sweeper pages nobody. ALERT-RETENTION-1 before production. HD-CTF-10 DECIDED (observe closed tenants; read-only credential for a real PSP).
- Real PSP: `PROVIDER DEPENDENT`; sandbox adapter NOT authorized.

## 31. OPEN HUMAN DECISIONS

Full table (ID | Question | Current Decision | Status | Blocks | Owner), grouped DECIDED / NEEDS USER / NEEDS LEGAL-COMPLIANCE / DEFERRED / INFORMATIONAL, is in [`governance/human-decision-register.md`](governance/human-decision-register.md) and mirrored below.

### DECIDED

| ID | Question | Current Decision | Status | Blocks | Owner |
|---|---|---|---|---|---|
| THREAT-MODEL-ARBITRARY-SQL-1 | Is a stolen/compromised `igaming_runtime` credential (arbitrary SQL) inside the production threat model for DB-enforced controls? | YES (2026-10-05); TEMP fix alone insufficient | DECIDED; mitigation PARTIALLY MITIGATED | Closing it fully needs T3/H1, T4, T5, T8 decisions | owner + security |
| SIGNED-ACTOR-PROOF | Implement a DB-verified signed actor proof? | AUTHORIZED, smallest mechanism only (2026-10-06) | DECIDED; IMPLEMENTED and merged `639a2f0` (PARTIALLY MITIGATED: T3/T4/T5/T8 open) | Identity-store (T3/T4) and T8 authorizations | owner |
| H-W1 | Observe suspended/closed tenants in reconciliation? | YES, read-only evidence; never auto dispatch/release/cancel/settle (2026-10-05) | DECIDED; merged `741569e` | - | owner |
| HD-CTF-10 | Credential treatment when observing a closed tenant | Closed tenants stay observable while unresolved player funds exist; real PSP uses a read-only/reconciliation-scoped credential where supported (2026-10-05) | DECIDED | Real-PSP architecture detail | owner |
| R3-GAME-POSTINGS-NONACTIVE-1 | Gameplay postings on non-active tenants | FAIL CLOSED for NEW gameplay postings (2026-10-05); migration 0118 merged | DECIDED; merged | - | owner |
| Q-GP-5 | Terminal stake returns on non-active tenants | ALLOWED for valid existing rounds; new wagering still refused (2026-10-06) | DECIDED; IMPLEMENTED and merged `639a2f0` | - | owner |
| Q-GP-1 | Tenant closure with open rounds | REFUSED while open rounds exist (2026-10-06); casino half not representable (Q-GP-6) | DECIDED; IMPLEMENTED (sportsbook only; casino Q-GP-6 open), merged `639a2f0` | Merge | owner |
| SANDBOX-BEFORE-ALERT-DELIVERY-1 | Sandbox PSP adapter before real alert delivery? | YES, sandbox only, synthetic non-real-money tenant; Class-B prerequisites first; ALERT-DELIVERY-1 remains a production launch blocker | DECIDED; adapter NOT authorized yet | Sandbox adapter start | owner |
| ADR-0094-500MS | Keep the 500 ms timing bound | KEPT unchanged | DECIDED | - | owner/security |
| HQ-E1-2 | E1 item (p2) | Confirmed | DECIDED | - | owner |
| CLEANUP-2026-10-05 | Scratch DB/worktree cleanup | Accepted; 44 UNKNOWN DBs preserved | DECIDED | - | owner |
| TRIGGER-SEARCH-PATH-1 route | How to close the TEMP shadow path | REVOKE TEMP (0116, ADR 0108) | DECIDED; implemented | Deployment verification per environment | owner + security |
| HD-0095-1 / LEDGER-MANUAL-ADJ-4EYES-1 | Force-resolve authority; manual adjustments | Recorded in ADR 0098 (2026-09-28) | DECIDED | - | owner |
| HD-PRH2-9, HD-PRH2-10 | Closed-tenant funds, E1 alert kind | Recorded in ADR 0105 (2026-10-04) | DECIDED | - | owner |

### NEEDS USER (owner / architect)

| ID | Question | Current Decision | Status | Blocks | Owner |
|---|---|---|---|---|---|
| Q-GP-6 | What signal closes a casino round (open round not representable)? | none | OPEN | Casino half of closure guard; win entitlement on closure | owner + architect |
| Q-GP-2 | Add a `tenant_not_active` class to `casino_callback_rejections`? | none | OPEN | casino_consistency finding for refused win/rollback | owner + ledger-finance |
| Q-GP-3 | Durable provider-claim evidence for the public casino webhook 401 before verification | none | OPEN | Evidence of refused provider claims | owner + architect |
| Q-GP-4 / R3-RECON-NONACTIVE-STREAMS-1 | Run `casino_consistency`/`sportsbook_settlement` for non-active tenants? | none | OPEN | Cross-check coverage | owner + architect |
| H1 / T3 | Authorize identity-store hardening (keyed refresh-token hashes, keyed staff credential check, owner-only staff lifecycle) | none | OPEN | Full closure of THREAT-MODEL-ARBITRARY-SQL-1 | owner + security |
| T4 | Keyed hash for `player_credential_tokens` | none | OPEN | Player account takeover residual | owner + security |
| T8 | Extend signed proof to other dual-control flows (0105, 0096, 0103, 0047, 0026/0034, 0063, 0086/0089) | none | OPEN | Closure of the threat model | owner + security |
| TENANT-STATUS-AUTHZ-1 | `ChangeStatus` has no authz and no HTTP route | none | OPEN | Any tenant-closure flow | owner + security |
| MA020-K2-VISIBILITY-1, STMT-TABLE-INSERT-RLS-1 | K2-session visibility on reference evidence; statement-table insert RLS | none | OPEN | Before a live PSP with status polling | security + architect |
| PAY-PAYOUT-CONTRADICTION-HOLD-1 | Operator resolution path for refused payout parks | none | OPEN | Live payouts | owner + architect |
| B13 | Payout destination binding | none | BLOCKED | Real payouts | architect + owner |
| ALERT-DELIVERY-1 / HD-PRH2-4-OPS | Real recipients, on-call, channel | none | OPEN | Production; first human-notification kind (ADR 0102 s18.4) | owner |
| ADR 0094 timing-lane criterion | Environment-calibrated relative assertion / p95 + p100 ceiling / controlled runner | bound unchanged | OPEN | Reliable green timing lane | owner + architect + security |
| HD-PRH2-8 | Below-threshold four-eyes semantics | interim enforced | OPEN (non-blocking) | - | owner |
| HD-PRH-1 | Are tenant slugs confidential? | none | OPEN | WEBHOOK-PATH-TOKEN-1 | owner |
| HD-PRH2 H(8) | Gate suspended/closed BRAND like a non-active tenant in resolution-only sweep? | none | OPEN | - | security |
| WD-RG-1 | Add RG/Risk gate on withdrawal to scope? | none | OPEN | Withdrawal compliance | owner |
| NULL-ARM-WRITE-1 | Launch-blocking or not | hardening item (security verdict) | OPEN | Second tenant / second platform admin | owner + security |
| CI-BILLING-1 | GitHub billing | do NOT modify; local evidence only | OPEN (owner/account) | Green GitHub CI | owner |
| MAC-RUNNER | Authorize Mac self-hosted runner setup | not authorized | OPEN | Self-hosted CI evidence | owner |
| BRANCH-PROTECTION-1 | Protect `main`, enforce CODEOWNERS | none | OPEN | - | owner |
| AWS-STAGING | Authorize next staging deployment (naming the commit) | not authorized; AWS OFF | OPEN | PLAT-ROLESPLIT-1 verification | owner |
| ACCESS-ANALYZER-CHECK-1, HD-10.3-2, DEPLOY-FPKEY-1 | AWS account/IAM/key-delivery actions | none | OPEN | Next staging deployment | owner / AWS admin |
| Vendor/PSP/custodian selection, contracts, production credentials, production launch | Standing human-only items | none | OPEN | Provider and production phases | owner |
| PRH-GATE | Authorize final PRH-2 gate / next stage | stopped, not authorized | OPEN | Next stage | owner |

### NEEDS LEGAL-COMPLIANCE

| ID | Question | Current Decision | Status | Blocks | Owner |
|---|---|---|---|---|---|
| HQ-E1-1 | KYC submissions for suspended/closed tenants | none | OPEN | E1 on a real tenant | legal/compliance |
| HQ-E1-3 | Jurisdiction deadlines for KYC | none | OPEN | E1 on a real tenant | legal/compliance |
| HQ-E1-4 | KYC outbox retention | none | OPEN | Production | legal/compliance |
| HD-KYC-1..8 | KYC thresholds (ADR 0096 s4) | none | OPEN | KYC-ENFORCE-1 completion | legal/compliance + owner |
| HD-CTF-1..9 | Closed-tenant player-funds policy (ADR 0107 s12) | none; mechanism DESIGN ONLY | OPEN | Any closed-tenant funds mechanism | legal/compliance + owner |
| HDR-J-7/8/9, HDR-M-1/2 | Jurisdiction items (ADR 0044, stage-4i-exit-register) | none | OPEN | Jurisdiction go-live | legal/compliance |
| HDR-SB-1 | Sportsbook go-live gate (ADR 0083 s8.2) | none | OPEN | Sportsbook go-live | legal/compliance + owner |
| HDR-J-1..6 residuals | Legal-review residuals | answered in ADR 0042 with residuals | OPEN (residual) | - | legal |
| K3 note retention | PII retention for K3 resolution notes | none | OPEN | Production | legal + security |
| Licence / jurisdictions beyond Anjouan | Gambling licence decisions | Anjouan only | OPEN | Production launch | owner + legal |
| Hosting AUP | Written gambling-AUP confirmation from the hosting provider | none | OPEN | Production deployment | owner + legal |

### DEFERRED

| ID | Question | Current Decision | Status | Blocks | Owner |
|---|---|---|---|---|---|
| Wave 4 bonus engine | Start Wave 4? | not authorized | DEFERRED | Bonus roadmap | owner |
| ADR 0042 G-2 / self-exclusion auto-void | Implement configurability and consumer | decided in ADR 0042, not implemented | DEFERRED | CAS-WIN-IDEMP-1 before bonus-funded stakes | owner |
| Retail/POS | Build retail | architecture only | DEFERRED | Retail roadmap | owner |
| AI agents (ADR 0089) | Product AI | architecture only | DEFERRED | - | owner |
| Crypto custodian (ADR 0008) | Select custodian | interface only | DEFERRED | Crypto wallets | owner |
| Asymmetric actor proof; nonce pruning automation | Improvements to ADR 0110 | recorded | DEFERRED | - | security |
| Schema/database-per-tenant isolation | Tighten isolation | only when a partner/jurisdiction requires | DEFERRED | - | architect |
| KYC-FX-AGG-1 | FX source | none exists | DEFERRED | Aggregated KYC thresholds | owner |

### INFORMATIONAL

| ID | Question | Current Decision | Status | Blocks | Owner |
|---|---|---|---|---|---|
| PRH-2 | Is PRH-2 complete? | NO | INFORMATIONAL | - | orchestrator |
| Providers | Any real provider? | ALL MOCK; no production credentials, no real money | INFORMATIONAL | - | - |
| AWS | State | OFF; nothing deployed; PLAT-ROLESPLIT-1 verification prepared, not executed | INFORMATIONAL | - | - |
| CI | State | GitHub blocked by billing; local evidence only; Mac runner prepared, not installed | INFORMATIONAL | - | - |
| Preserved artefacts | Scratch DBs / worktrees | 44 UNKNOWN DBs, 52 worktrees preserved; keep `igaming_orch_local`, `igaming_platform_ci_local` | INFORMATIONAL | - | - |

Previous list (snapshot 2026-10-05, preserved):

Snapshot 2026-10-05 (latest additions in registry row HUMAN-DECISIONS-ROUND3: threat model for arbitrary SQL as the runtime role, HD-CTF-10, game/sportsbook postings on non-active tenants, sandbox vs ALERT-DELIVERY-1, brand gating, K3 note retention, staging deployment authorization, Mac runner setup, the 44 UNKNOWN scratch DBs); each has a registry row (the `HQ-E1-*` and `HD-CTF-*` rows were appended 2026-10-05).
- **Infrastructure / accounts:** CI-BILLING-1, BRANCH-PROTECTION-1, ACCESS-ANALYZER-CHECK-1, HD-10.3-2 (AWS IAM / awssm deploy), DEPLOY-FPKEY-1.
- **Security:** the TRIGGER-SEARCH-PATH-1 fix route is DECIDED (REVOKE TEMP, 0116); remaining owner/security actions: deployment verification per environment (and whether the production gate should also cover staging), PLAT-ROLESPLIT-1 (runtime binary never connects as the database owner); NULL-ARM-WRITE-1 launch-blocking or not.
- **Testing:** the ADR 0094 timing-lane decision. 2026-10-05 idle characterization (evidence/prh2-timing-lane-characterization.md): `TestResolutionIsolation_NormalOperation` 500 ms p100 over 90 callbacks under `-race` on 4 vCPU fails at HEAD (0/10 interleaved, 0/13 earlier), also on ecd2b74 (1/10) and the pre-PRH-2 baseline 94b4ae5 (2/10); the bound is NOT changed and ADR 0094 is NOT edited. Options for the owner/architect/security (proposal text in the characterization): environment-calibrated relative assertion (recommended target) with p95 + hard p100 ceiling interim; raising the bound needs a recorded security decision. The lane is reported FAILING, never skipped.
- **Reconciliation / payments (2026-10-05):** H-W1 DECIDED (owner): suspended/closed tenants are OBSERVED by `ledger_vs_projection` and `payment_statement` reconciliation (read-only evidence, IMPLEMENTED against MOCK, ADR 0095 §40.4, merged `741569e`); no auto dispatch/release/cancel/settle; staff four-eyes path unchanged. OPEN owner decisions from it: HD-CTF-10 (credential treatment when observing a closed tenant), R3-GAME-POSTINGS-NONACTIVE-1 (game/sportsbook postings can still land on non-active tenants), THREAT-MODEL-ARBITRARY-SQL-1, plus the sandbox-before-ALERT-DELIVERY-1 question (ADR 0095 §35.4 item 2).
- **Alerting:** HD-PRH2-4-OPS (real recipients, on-call). HD-PRH2-8 (below-threshold four-eyes semantics; non-blocking, interim enforced). HD-PRH-1 (are tenant slugs confidential? gates WEBHOOK-PATH-TOKEN-1).
- **KYC:** HD-KYC-1..8 thresholds (ADR 0096 §4); HQ-E1-1..4 (ADR 0106 §12: suspended/closed-tenant submissions, severity of `kyc.submission_failed_terminal` before 0114 reaches a real tenant, jurisdiction deadlines, outbox retention); KYC-FX-AGG-1 (no FX source).
- **Closed-tenant funds:** HD-CTF-1..9 (ADR 0107 §12). ADR 0107 is DESIGN ONLY until these are answered and the human authorizes a workstream. (HD-PRH2-9 is decided, ADR 0105 §1.)
- **Jurisdiction / sportsbook:** HDR-J-7/8/9 (ADR 0044), HDR-M-1/2 (stage-4i-exit-register), HDR-SB-1 (ADR 0083 §8.2); HDR-J-1..6 answered (ADR 0042) with legal-review residuals. WD-RG-1: add to scope?
- **Standing human-only items:** vendor/PSP/custodian selection, contracts, production credentials, jurisdictions beyond Anjouan, hosting AUP (13-dependency-map R6), production launch, and authorization of the final PRH-2 gate / next stage (PRH-GATE, "Not started").

## 32. OPEN LEGAL / COMPLIANCE ITEMS

Software capability is not legal approval. None of the following is resolved by code: gambling licence decisions and jurisdictions beyond Anjouan; HQ-E1-1 (suspended/closed-tenant KYC submissions), HQ-E1-3 (jurisdiction deadlines), HQ-E1-4 (outbox retention) - HQ-E1-2 p2 is confirmed; HD-CTF-1..9 (closed-tenant player funds, ADR 0107, design only); HD-KYC-1..8 thresholds; HDR-J-7/8/9, HDR-M-1/2, HDR-SB-1; legal review residuals of HDR-J-1..6; K3 note retention (PII); data residency; the hosting provider's gambling AUP; continued use of an offboarded operator's merchant account for observation (HD-CTF-10 residual legal/contract question). Owner for all: human (legal/compliance counsel), not engineering.

## 33. CURRENT LAUNCH BLOCKERS

Real-money launch is **BLOCKED**. Summary (authoritative IDs in the task registry; see also the previous detailed snapshot below):
1. No licence/legal approval and no jurisdiction decisions beyond Anjouan (32).
2. Every provider is MOCK; no PSP/KYC/casino/sportsbook vendor selected or contracted; no production credentials.
3. ALERT-DELIVERY-1: no human is paged (no channel, no recipient).
4. TRIGGER-SEARCH-PATH-1 deployed-and-verified, PLAT-ROLESPLIT-1 staging verification not executed; AWS off.
5. Backup/DR NOT MET; no tested restore path.
6. CI-BILLING-1: no green GitHub CI; timing lane environment-dependent; F-POOL-1 K1 / PRH-FPOOL1.
7. THREAT-MODEL-ARBITRARY-SQL-1 residuals (T3/H1, T4, T5, T8) per owner risk acceptance; the 0120 merge is DONE (`639a2f0`).
8. Class-B payment items, R3 launch conditions, K3 follow-ups, CAS-GAME-KILL-BET-1, KYC-ENFORCE-1, WD-RG-1 undecided.
9. Hosting AUP, production launch authorization.

Previous detailed snapshot (2026-10-05, preserved; items since resolved are noted in 28 and 47a):

Snapshot 2026-10-05, derived from [`governance/task-registry.md`](governance/task-registry.md) (the registry stays authoritative; IDs below are registry IDs; see also the row `REGISTRY-HYGIENE-2026-10-05`). It **supersedes** `payment-readiness-completion-report.md` §11. Everything is MOCK, AWS is OFF, and no GitHub CI evidence exists (CI-BILLING-1). Nothing here is a statement that any item is approved or complete.

**Platform-wide / security**
- **TRIGGER-SEARCH-PATH-1 (+ `-UPGRADE`, `-0116`): HIGH, LAUNCH BLOCKER until DEPLOYED and VERIFIED.** The exploitation path (TEMP-table shadowing of the unpinned 0026..0113 functions bypassed the K2 four-eyes check) is CLOSED in the codebase for the runtime role and other non-owner roles by migration 0116 / ADR 0108 (REVOKE TEMP, owner-authorized): IMPLEMENTED, LOCAL EVIDENCE ONLY. Still required before it stops blocking (only `security` may lower it): 0116 applied in every environment that serves real money and the runbook verification SQL passing (runbooks/operational-runbooks.md §7 step 5); the service connecting as `igaming_runtime`, never the database owner (PLAT-ROLESPLIT-1: dev `make run`/`.env.example` use the owner); 0116/role-init applied BEFORE runtime traffic or runtime backends recycled; no other principal that can write K2 rows holding TEMP/CREATE. NOT IMPLEMENTED (defence in depth): pinning the 0026..0113 functions.
- NULL-ARM-WRITE-1 (reviewed 2026-10-05: production-HARDENING item, not a launch blocker under the current threat model, but NOT mitigated by RLS - only Go discipline; recommended hardening A+B before the second tenant / second platform admin; the larger question is THREAT-MODEL-ARBITRARY-SQL-1, an owner decision; registry NULL-ARM-WRITE-1-REVIEW-2026-10-05).
- STAFF-LIFECYCLE-1: audited staff suspend/role change before real-money K operations. MANUAL-ADJ-LINK-1 (= LEDGER-MANUAL-ADJ-LINK-1): preventive trigger, blocks the first real-money tenant.
- ALERT-DELIVERY-1 (OPEN; routing readiness smallest cut IMPLEMENTED/MOCK in 0117 / ADR 0102 §18, alert_routing_ready is always 0, NO real channel and NO recipient exist), HD-PRH2-4-OPS, PAY-P1-MULTISUCCESS-ALERT-1: no one is paged. Binding before the first human-notification kind: ADR 0102 §18.4. ALERT-RETENTION-1 before production.
- CI-BILLING-1 with F-POOL-1 K1, TEST-RESISO-RACE-1 (timing lane NOT GREEN locally) and PRH-FPOOL1: green GitHub CI is required for the final gate. BRANCH-PROTECTION-1.
- Backup/DR: NOT MET (`runbooks/backup-and-disaster-recovery.md`; no registry ID). DEVOPS-0107-INDEX-WINDOW-1, CAS-RECON-SCALE-1, WEBHOOK-EDGE-1 before real-money scale.

**Payments / payouts**
- PROV-OUTBOUND-CRED-1 precondition and F-POOL-2: per domain, before the first non-MOCK adapter.
- PAY-K3-STATEMENT-SOURCE-WIRING-1 (engineering item IMPLEMENTED against MOCK for mock-payments only; the REAL-MONEY precondition is NOT satisfied: H-W1 and a real PSP statement source remain), PAY-K3-FOLLOWUPS-1, PAYOUT-AMOUNT-DISPUTE-1, WITHDRAWAL-REVERSAL-1, PAY-SEC-LAUNCH-1, PAY-H-FOLLOWUPS-1.
- Before the first real PSP or real-money tenant: PAY-DEPOSIT-ESCALATION-1, PAY-RECEIPT-T4-DRAIN-TEST-1, PAY-POLL-DECLINED-ALERT-RECON-1, PAY-POLL-ECHO-HARDENING-1, PAY-PAYOUT-REFBIND-1, PAY-FPAY-HARDENING-1, PAY-CALLBACK-MISMATCH-BIND-1, PAY-PAYOUT-UNBOUND-HOLD-1, PAY-RECON-POLL-REF-CLEAR-1 + PAY-RECON-PARKED-CAPTURE-STANDING-1 (hard prerequisite), MA020-SYNC-MISMATCH-1, PRH-I1-MANIFEST-2/3/4. PAY-PSP-CONTRACT-INVDEP1 is a vendor-selection criterion.

- R3 launch conditions (real PSP / tenant-closure): R3-OBS-BOUND-1 (unbounded observation work), R3-FIRST-REAL-SOURCE-READONLY-1, R3-LISTING-FAILURE-LOG-ONLY-1; MA020-SYNC-MISMATCH-1 is class B (preventive, needs one migration, NOT built; no money-safety gap today against MOCK). Payment follow-up classification (class A empty; class B list before a sandbox adapter): registry PAY-FOLLOWUPS-CLASSIFICATION-2026-10-05.

**Casino / sportsbook**
- CAS-GAME-KILL-BET-1 (blocks real-money casino); CAS-BET-REQUIRES-BOOTSTRAP-1, CAS-PLAYER-REF-1, CAS-WIN-ANOMALY-1, PRH-REF-C2, CAS-STMT-IO-1, PAY-K3-MR020-HTTP-MAPPING-1 before a real casino/sportsbook provider; CAS-WIN-IDEMP-1 before bonus-funded stakes.
- Sportsbook go-live is gated by HDR-SB-1 / HDR-J-7 (ADR 0083); a bounded catalogue-fetch timeout is needed before a real adapter.

**KYC / compliance**
- KYC-ENFORCE-1 (PARTIALLY IMPLEMENTED; remaining B7, F4/F5, LF-I3-4/5, HD-KYC-1..8 thresholds). KYC-E1-FOLLOWUPS-1 (content-read seam and real-adapter intake before a real KYC vendor; KYC-OUTBOX-REQUEUE-1 is a priority). WD-RG-1 (no RG/Risk gate on withdrawal; not scoped).

**Staging / AWS (before the next deployment)**
- ACCESS-ANALYZER-CHECK-1, DEPLOY-FPKEY-1, SECRETSTORE-AWS-1 IAM part (HD-10.3-2).

## 34. DEFERRED ITEMS

Recorded, NOT built (need owner authorization where noted): H1 identity-store hardening (keyed refresh-token hashes, keyed staff credential check, owner-only staff lifecycle), T4 keyed credential-token hashes, T8 proof coverage for other dual-control flows; ADR 0107 closed-tenant funds mechanism (DESIGN ONLY, disabled); asymmetric proof; nonce pruning automation; G-P1 capability grants; pinning the 0026..0113 functions; ADR 0042 G-2 configurability; self-exclusion auto-void consumer; Wave 4 bonus; retail/POS; AI agents; crypto custody; ClickHouse/CDC analytics; schema-/database-per-tenant isolation; real alert channels; KYC-FX-AGG-1. Anything not required by the Blueprint, the stage's B2C MVP path, future B2B architecture, security/compliance or to avoid technical debt is recorded in `docs/decisions/` rather than built (`CLAUDE.md`, no uncontrolled scope expansion).

## 35. CURRENT WORK IN PROGRESS

1. Merged-tree `-race` verification at `639a2f0` is INCOMPLETE (environment-limited; section 25a). Do not run it as one unattended sweep in the Claude cloud sandbox; use controlled batches or a stable CI/self-hosted runner.
2. Two branches ready to merge (see 4 and 28). No new migration numbers may be allocated by anyone but the orchestrator.
3. Class-B classification and remaining items (28/30). Sandbox PSP adapter NOT started.
4. This handover (`HANDOVER-LIVE-2026-10-06`).
5. Owner decisions pending (31).

## 36. EXACT NEXT ENGINEERING TASKS

Ordered. "Startable" means no new human decision is needed. Every task: branch off the verified HEAD, tests first for financial work, mutation-kill evidence, reviews per `CLAUDE.md`, registry row, then stop at the gate.

| # | Task | Startable? | Notes |
|---|---|---|---|
| 1 | Complete merged-tree `-race` verification for the remaining packages (httpserver, kyc, payments first), in controlled batches or on a stable runner | Yes (not a development blocker) | Do not run two heavy sweeps at once |
| 2 | Merge `prh2-r5-signed-actor-proof@8863e31` and `prh2-r5-stake-return-closure@35cd2fb` (only after task 1 is green); `go run ./cmd/migrate verify` must show 0001..0121 gap-free; rerun the affected packages (`adjustment`, `payments`, `capability`, `db`, `httpserver`, `casino`, `sportsbook`, `tenant`, `reconciliation`) | Yes (reviews done) | Security delta review of the amended 0120 migration is called out as required before merge in ADR 0110 header; confirm it is recorded in the registry first |
| 3 | Full-tree verification: build, vet (both tag sets), gofmt, golangci-lint v2.9.0 built with Go 1.26.x (default and integration tag), `migrate verify`, timing lane separately | Yes | LOCAL evidence only |
| 4 | ~~Class-B B11, B12~~ **DONE 2026-10-07** (B5-B8, B11, B12 merged; see 28/30). Next: B13 gate | Yes (inside approved PRH-2 scope) | Each: ledger-finance + security review; no migration expected; new alert code in a NEW file (`payout_alerts.go`) | B12 must not edit `alerts.go` |
| 5 | Registry/docs hygiene: reconcile the known documentation conflicts (47a); refresh `docs/active-stage.md` and `docs/governance/project-status.md` after the merges | Yes | Append-only for the registry |
| 6 | PRH-2 final gate report, then STOP and ask the owner for the next stage | Yes | Never start the next stage unprompted |
| 7 | B13 payout destination binding | **No** | BLOCKED: architect + owner |
| 8 | T3/H1, T4, T8 identity-store and dual-control proof coverage | **No** | Owner authorization required |
| 9 | Q-GP-2/3/4/6 gameplay questions | **No** | Owner/architect/legal |
| 10 | Sandbox PSP adapter; real alert channel; AWS staging run | **No** | Separate authorizations; see 38, 45 |

### 36a. B13 decision package and independent work (2026-10-07, HEAD 484cbc5 + this docs commit)

B11 and B12 are complete and merged. **B13 is awaiting the architect/owner decision.** The decision brief is prepared: `docs/governance/b13-decision-brief.md` (documented requirement = destination from a player-bound, verified instrument resolved server-side; D1-D4 open; no third option is documented). B13 is NOT implemented and no option has been chosen.

Classification of the named items (A = implement now, no human decision; B = needs human/architect/security decision; C = sandbox-gated; D = depends on B13; E = covered/no action):

| Item | Class | Why |
|---|---|---|
| PAY-PAYOUT-UNBOUND-STANDING-1 | **A** (priority 1) | Direction already ruled in ADR 0095 s35.2 PO-1 (LF): per-operation standing rule, deposit-shaped signals never clear a payout finding, finding text never suggests allocation; flips the B11 pins deliberately. Reconciliation code, no migration expected (a `payout_return` line kind is out of scope). Required before any non-MOCK payout. |
| PAY-PAYOUT-ASSET-ECHO-TEST-1 | **A** | Tests only (payout asset-echo site lacks a site-level test). |
| PAY-PAYOUT-CALLBACK-AUDIT-1 | **A** | Audit rows for existing callback T14/T15/mismatch/tombstone cells; no state/policy change (CLAUDE.md audit rule). |
| R-5 (B12) | **A** | Raise-only alert for mismatched-amount success on a declined payout; LF recorded raise-only as acceptable. |
| PAY-PAYOUT-UNBOUND-RESOLVE-1 | **B** | R-K3-8 resolution semantics for unbound payout holds need owner/architect + LF design before code. Required before any non-MOCK payout. |
| H-SEC-5 (tenant-status gate on HTTP deposit/payout initiation) | **B** (quick confirm) | Mechanism is clear; the policy for non-active tenants on NEW HTTP money initiation is not recorded as a decision (HD-CTF-10 covers closed-tenant outbound). Recommend a one-line security/owner confirmation, then it is A. |
| H-SEC-11 (brand suspended/closed status gate) | **B** | Registry: "security to decide" (H(8)). |
| ALERT-DELIVERY-1 | **B + C** | Owner recipients/channel; user decided sandbox PSP precedes delivery; production blocker. |
| B13 | **B / D** | Gate. B14-B18 and the sandbox adapter depend on it. |
| E | none of the eight is fully covered | PAY-PAYOUT-CONTRADICTION-HOLD-1 is related to RESOLVE-1. |

Recommended path: (1) PAY-PAYOUT-UNBOUND-STANDING-1; (2) one bundle: PAY-PAYOUT-ASSET-ECHO-TEST-1 + PAY-PAYOUT-CALLBACK-AUDIT-1 + R-5. Remain blocked: B13, RESOLVE-1, H-SEC-5/11 (pending one-line rulings), ALERT-DELIVERY-1, B14-B18, sandbox PSP, AWS.

## 37. CONTINUING DEVELOPMENT ROADMAP

1. Close PRH-2: merges, sweep, Class-B startable items, gate report, owner authorization.
2. Provider-independent platform maturity: ALERT-DELIVERY-1 design implementation choices, idempotent timeouts, backup/DR mechanism and restore test, observability alert rules (`runbooks/observability-and-alerting.md`).
3. Sandbox provider phase (after authorization) - see 38.
4. Production readiness - see 39.
5. Product tracks B2C (40) and B2B/retail (41).
Roadmap source documents: [`architecture/14-mvp-scope-and-roadmap.md`](architecture/14-mvp-scope-and-roadmap.md), [`architecture/35-product-surfaces-roadmap.md`](architecture/35-product-surfaces-roadmap.md), [`governance/project-status.md`](governance/project-status.md), [`plans/next-real-provider-integration-planning-gate.md`](plans/next-real-provider-integration-planning-gate.md). Where these disagree with this file, the task registry wins.

## 38. PROVIDER-INTEGRATION ROADMAP

Everything `PROVIDER DEPENDENT`, nothing started. Order: (1) finish Class-B prerequisites; (2) owner authorizes a **sandbox-only** PSP adapter for a synthetic non-real-money tenant (owner YES in principle; ALERT-DELIVERY-1 stays a production blocker; ADR 0095 section 35.4 item 2 amendment to be drafted by ledger-finance if needed); (3) adapter acceptance criteria B14-B18, PROV-OUTBOUND-CRED-1 tripwire relaxation scoped to that adapter, F-POOL-2 LF-C1, guard-list test extension, a sandbox statement source, read-only/reconciliation-scoped credential where the PSP supports one (HD-CTF-10, R3-FIRST-REAL-SOURCE-READONLY-1); (4) then KYC vendor (KYC-E1-FOLLOWUPS-1, content-read seam), casino aggregator (CAS-* prerequisites), sportsbook feed (bounded catalogue timeout, bet contract), crypto custodian (ADR 0008), email, geolocation, FX source. Vendor selection, contracts, commercial terms and production credentials are **human-only**. Do not introduce real provider credentials casually: no credentials in Git, environment or this repo; per-tenant secrets via the secret store (ADR 0093).

## 39. PRODUCTION ROADMAP

Blocked until: licence and legal inputs; real vendors; ALERT-DELIVERY-1 with on-call recipients (HD-PRH2-4-OPS); backup and DR with a tested restore (RPO/RTO evidence); staging deployment from the final approved commit with PLAT-ROLESPLIT-1 verification and the AWS human actions (ACCESS-ANALYZER-CHECK-1, DEPLOY-FPKEY-1, HD-10.3-2); green CI evidence (GitHub or an owner-ruled self-hosted equivalent); WEBHOOK-EDGE-1, CAS-RECON-SCALE-1, DEVOPS-0107-INDEX-WINDOW-1; production configuration checklist; hosting AUP; security sign-off; explicit production launch authorization by the human.

## 40. B2C ROADMAP

Own-brand casino (first tenant, Anjouan). Done: frontend slice (Stages 6, 6.1, 7) against MOCK. Next (all need authorization): brand/jurisdiction content, KYC and RG UX tied to HD-KYC thresholds, real PSP cashier flows (hosted fields/redirect only, no PAN), real casino lobby, sportsbook go-live gate HDR-SB-1, bonus Wave 4. See [`architecture/37-b2c-brand-frontend-architecture.md`](architecture/37-b2c-brand-frontend-architecture.md).

## 41. B2B / RETAIL ROADMAP

B2B: second-tenant dry run and multi-tenancy hardening are Stage 7+ items; partner console configuration; per-tenant provider credentials; tightening of isolation only when a partner/jurisdiction requires it (ADR 0002). Retail/POS: architecture only (26, ADR 0035/0036), `NOT IMPLEMENTED`; needs owner authorization and a plan. Closed-tenant funds mechanism ADR 0107 is DESIGN ONLY until HD-CTF answers and authorization.

## 42. HOW TO RUN THE PROJECT

Summary (verified commands with sources: [`runbooks/developer-handover.md`](runbooks/developer-handover.md)): start Postgres (`make dev-db-up`), roles and database via the checked-in init scripts run **by a human** (`make dev-db-init-roles`, `make dev-db-init-test-admin`), `make migrate-up`, copy `.env.example`, `make run` (defaults `APP_ENV=development`), `curl localhost:8080/healthz` and `/readyz`. Frontends: `npm ci && npm run build && npm test` inside `b2c/` or `backoffice/` (Node 22). Dev `make run` currently connects as the database owner (PLAT-ROLESPLIT-1 dev gap); never use that target for anything but local development.

## 43. HOW TO RUN TESTS

Unit: `go test ./...`. Integration: `-tags=integration` with `TEST_DATABASE_URL`, `TEST_RUNTIME_DATABASE_URL`, `TEST_ADMIN_DATABASE_URL` set to a **private scratch database**, `-race -count=1 -p 1`, long packages need `-timeout 60m`; the timing lane is skipped by name in the sweep and run alone. Exact commands, the `-skip` string and env variables are in the runbook with source file references. Static checks: `gofmt -l .`, `go vet ./...` (also with `-tags integration`), `golangci-lint` v2.9.0 built with Go 1.26.x. Never present local results as GitHub CI.

## 44. HOW TO RUN DATABASE / MIGRATIONS

`go run ./cmd/migrate up|status|verify`; `down` only with `APP_ENV=development` or `staging`, `-steps=N`. Use a private scratch database for experiments (`scratchdb.New` in tests creates and drops its own). **Never** `ALTER ROLE`, `sudo`, `CREATE ROLE` or password changes to fix access: STOP and report (`CLAUDE.md`; incident [`governance/incident-2026-09-27-local-db-credential-mutation.md`](governance/incident-2026-09-27-local-db-credential-mutation.md)). Container restarts kill Postgres: recovery is `service postgresql start` (background it), then wait for `pg_isready`. Do not delete unknown databases; classify first.

## 45. AWS STAGING PROCEDURE

**Do not deploy AWS without explicit owner authorization.** Nothing is deployed. If and when authorized: follow [`runbooks/plat-rolesplit-staging-verification.md`](runbooks/plat-rolesplit-staging-verification.md) (operator-run; sections 2 preconditions, 3 order, 4 checklist, 6 authorizations, 7 closure) together with [`runbooks/stage-9-4-staging-lifecycle-runbook.md`](runbooks/stage-9-4-staging-lifecycle-runbook.md) (`deploy/aws/scripts/deploy.sh up|migrate|down`, `verify-teardown.sh`, interactive applies, never `-auto-approve`) and [`runbooks/stage-9-4-aws-account-verification.md`](runbooks/stage-9-4-aws-account-verification.md). Required: written owner go-ahead naming the commit, clean tree, `aws sts get-caller-identity` check, ACCESS-ANALYZER-CHECK-1, DEPLOY-FPKEY-1 and HD-10.3-2 decided or explicitly deferred, cost acknowledgement, teardown plus verification afterwards. Do not change Terraform or IAM casually; any change goes through devops + security review and Access Analyzer / `simulate-deployer-policies.py`.

## 46. CI / SELF-HOSTED RUNNER

GitHub Actions is blocked by billing (CI-BILLING-1); billing is not to be touched. Run the equivalents locally (see runbook) and label results LOCAL. The Mac self-hosted runner is prepared, not installed: [`runbooks/self-hosted-runner.md`](runbooks/self-hosted-runner.md) (labels `self-hosted, macOS, ARM64, igaming-ci`, assumptions A1-A5 to confirm, owner-run setup commands, teardown) and `.github/workflows/ci-selfhosted.yml` (manual dispatch only, throwaway Postgres cluster on 127.0.0.1:54329). Nothing is registered until the owner separately authorizes it.

## 47. SECURITY / OPERATIONAL WARNINGS

- Container restarts kill Postgres and background jobs; do not run two heavy sweeps at once (disk pressure; each scratch DB is large). Check `df -h` before long sweeps; a go test that hits its default 10-minute timeout leaks scratch databases (use `-timeout 60m`).
- Test runs create scratch databases; use private ones; **never delete unknown databases or worktrees without classification** (44 UNKNOWN DBs and 52 worktrees are preserved by decision).
- Never use `ALTER ROLE`/`sudo`/`CREATE ROLE`/password changes on shared roles; if DB access fails, STOP and report.
- Dev placeholder credentials in the repo are synthetic and invalid elsewhere; `.env.example` placeholders are refused outside `APP_ENV=development`.
- The 500 ms timing-lane bound is a security bound; do not widen it.
- A local green run is not CI; mock green is not provider green; software capability is not legal approval.
- Alert delivery does not exist: a stalled sweeper or P1 pages nobody.
- Production refuses to start if the runtime role holds TEMP, is a member of other roles, or (after 0120) lacks an active actor-proof key.

### 47a. Known documentation conflicts (not silently rewritten)

1. **Migration counts.** The previous HANDOVER said `0001..0115` in the structure row and `0001..0117` in the database row; `docs/active-stage.md`/`project-status.md` newest notes say 0001..0117; the repository HEAD has `0001..0119`; the unmerged branches add 0120/0121. Newest authoritative evidence: `ls migrations` at `d149a64` (0119).
2. **Test status notes.** `active-stage.md` round-2 note says "54/54 packages pass under -race at `f9dc8a3`"; the current sweep at `d149a64` is 53/54 with `internal/payments` pending. Older note is historical.
3. **Migration 0118.** Registry row `DECISIONS-PRH2-CLEARANCE-2026-10-05` reserves 0118 for the arbitrary-SQL mitigation; 0118 was actually used by the gameplay posting gate, and the signed actor proof is 0120. Later rows are authoritative.
4. **THREAT-MODEL-ARBITRARY-SQL-1, R3-GAME-POSTINGS-NONACTIVE-1, H-W1, HD-CTF-10** appear as OPEN in the previous HANDOVER, in `active-stage.md` notes and in earlier registry rows; they are DECIDED (and, for the gameplay gate, merged). The registry rows `DECISIONS-PRH2-*` are newer.
5. **Q-GP-1 and Q-GP-5** are listed open in ADR 0095 section 40 text on HEAD; they were decided 2026-10-06; the ADR 0095 section 40.6 update is now merged (`639a2f0`).
6. **Stage numbering.** `MASTER-BUILD-PROMPT.md` defines Stages 0-7; the repo progressed through sub-stages up to 10.3 and the PRH/PRH-2 blocks. The governance files treat the later labels as authoritative.
7. **Timing lane.** Older notes say NOT GREEN (0/5, 0/13); the orchestrator reports 40/40 on one host and failure on a slower host. Recorded as environment-dependent; no bound was changed.
8. **Payments statement-source label.** Old matrix: PAY-K3-STATEMENT-SOURCE-WIRING-1 "IMPLEMENTED against MOCK" while the real-money precondition is not satisfied. Both are true; read them together.
9. **Makefile vs agent rules.** `make dev-db-init-roles` uses `sudo -u postgres`; `CLAUDE.md` forbids sub-agents from using sudo/role changes. Those targets are human-only.
10. **ADR 0110 review state.** The ADR 0110 header (on the unmerged branch) says a further review of the amended migration is REQUIRED before merge; the orchestrator facts say security, ledger-finance and code review approved with conditions that are met (commit `8863e31` is titled "security delta conditions C1-C4"). Treat the orchestrator facts as newer, but confirm the delta-review record exists before merging.
11. **ADR 0095 header** says "IMPLEMENTED (code + tests); not yet gate-reviewed" though later reviews exist; ADR status headers lag in several ADRs (e.g. 0096 pending-review wording).

## 48. DO-NOT-CHANGE RULES

> ### DO-NOT-CHANGE RULES (highly visible; violating any is a stop-and-ask event)
>
> 1. **Do not change financial semantics without an approved decision** (ADR + `ledger-finance` + `security` review).
> 2. **Do not weaken security controls** (RLS, triggers, guards, admission, redaction, startup refusals).
> 3. **Do not bypass four-eyes** (K1, K2, K3, kill switch, withdrawals) in code, SQL, scripts or tests that run against shared data.
> 4. **Do not weaken tenant isolation** (tenant id from server context only; RLS bound to the connection setting).
> 5. **Do not weaken ledger invariants** (append-only, double entry, idempotency constraints, tombstones, no balance UPDATE).
> 6. **Do not loosen the 500 ms performance/timing requirement** (ADR 0094) without an explicit recorded decision.
> 7. **Do not introduce real provider credentials casually**: no real keys in Git, env files, tests or docs; everything is MOCK until authorized.
> 8. **Do not deploy AWS without explicit authorization.** Nothing is deployed today.
> 9. **Do not change Terraform/IAM casually** (devops + security review, Access Analyzer).
> 10. **Do not alter shared DB passwords** or shared role state.
> 11. **Do not use `ALTER ROLE` (or `sudo`/`CREATE ROLE`) to solve agent access problems**: STOP and report.
> 12. **Do not delete unknown databases or worktrees without classification** (44 UNKNOWN DBs and the agent worktrees are preserved by decision; keep `igaming_orch_local`, `igaming_platform_ci_local`).
> 13. **Do not mark unresolved findings closed without evidence** (tests, mutation-kill evidence, review, registry row).
> 14. **Do not silently change an ADR**: amend through a new ADR or an explicit amendment section; keep history.
> 15. **Do not bypass the Master Orchestrator/governance process** (stage gates, migration number allocation, registry updates, specialist reviews).
>
> Also: do not modify GitHub billing; do not start the next stage, the sandbox adapter or any real-provider work unprompted; do not claim PRH-2 complete or any legal/regulatory approval.

## 49. IMPORTANT ADR / DECISION INDEX

All ADRs: [`decisions/`](decisions/). Key ones (title and status as in the file headers; status lines can lag):

| ADR | Subject | Status / note |
|---|---|---|
| [0001](decisions/0001-ledger-design.md) | Append-only double-entry ledger | Accepted |
| [0002](decisions/0002-multi-tenancy-isolation-strategy.md) | Shared cluster + RLS with tightening path | Accepted |
| [0004](decisions/0004-provider-abstraction-pattern.md) | Integrations are subsystems | Accepted |
| [0006](decisions/0006-hybrid-licensing-and-jurisdiction-model.md) | Hybrid licensing, jurisdiction as config | Accepted (human) |
| [0007](decisions/0007-multi-wallet-per-player-model.md) | Multi-wallet per player | Accepted (human) |
| [0008](decisions/0008-crypto-custody-provider-abstraction.md) | Institutional custody abstraction | Accepted; NOT IMPLEMENTED |
| [0009](decisions/0009-hosting-hyperscale-cloud.md) | Hyperscale cloud hosting | Accepted (human) |
| [0019](decisions/0019-authoritative-ledger-and-balance-projection-architecture.md), [0020](decisions/0020-financial-idempotency-and-concurrency-control.md) | Ledger projection, idempotency | Accepted / implemented |
| [0031](decisions/0031-risk-and-limits-engine.md) | Risk & limits | Accepted |
| [0042](decisions/0042-human-decision-response.md) | Human decisions (G-2 etc.) | Recorded; implementation outstanding |
| [0082](decisions/0082-canonical-financial-lock-ordering.md) | Lock ordering | Accepted |
| [0083](decisions/0083-sportsbook-jurisdiction-gating-and-cumulative-exposure.md) | Sportsbook gating/exposure | Accepted |
| [0089](decisions/0089-future-ai-agent-architecture-boundary.md) | Future AI boundary | architecture only |
| [0093](decisions/0093-provider-credential-model-and-secret-store.md), [0094](decisions/0094-secret-resolution-resource-isolation.md) | Credentials; resource isolation, 500 ms bound | F-POOL-1 closed with conditions; K1 CI outstanding |
| [0095](decisions/0095-provider-io-transaction-boundary-and-payment-contract.md) | Provider I/O boundary, payment contract; sections 28, 29, 34-40 (s35-s40: PRH-2 rounds, kill switch, sweeper, closed-tenant observation 40.4, gameplay gate 40.5) | Implemented against MOCK; long, section-indexed |
| [0096](decisions/0096-kyc-enforcement-boundary.md) | KYC enforcement | Accepted, partially implemented |
| [0098](decisions/0098-human-decision-response-force-resolve-and-manual-adjustment.md) | Human decisions: force-resolve, manual adjustment | Recorded 2026-09-28 |
| [0099](decisions/0099-scoped-financial-capability-grants.md) | K1 capability grants | Partially implemented |
| [0100](decisions/0100-governed-manual-adjustments.md) | K2 manual adjustments | Implemented |
| [0101](decisions/0101-payment-force-resolution-m1-m2.md) | K3 force resolution | Implemented (MOCK) |
| [0102](decisions/0102-durable-alerting-and-provider-neutral-delivery.md) | Alerting; section 18 routing readiness | Implemented with MOCK channel; delivery open |
| [0103](decisions/0103-casino-launch-token-bootstrap-contract.md) | Casino bootstrap | MOCK |
| [0104](decisions/0104-tenant-visible-audit-of-platform-actions.md) | Tenant-visible audit | Implemented (kill switch only) |
| [0105](decisions/0105-human-decision-response-closed-tenant-funds-and-e1.md) | Human decisions: closed-tenant funds, E1 | Recorded 2026-10-04 |
| [0106](decisions/0106-kyc-submit-outbox-worker-identity-and-alert-kind.md) | KYC outbox | Implemented (MOCK) |
| [0107](decisions/0107-closed-tenant-player-funds-staff-resolution.md) | Closed-tenant funds resolution | DESIGN ONLY |
| [0108](decisions/0108-revoke-temp-from-runtime-role.md) | Revoke TEMP (0116) | Implemented; verify per environment |
| [0109](decisions/0109-prh2-round4-classb-amendments.md) | Class-B amendments (B9/B10, B4) | Accepted 2026-10-06 |
| 0110 `docs/decisions/0110-signed-actor-proof.md` | Signed actor proof | IMPLEMENTED, merged `639a2f0` |

Decision registers: ADR 0005, 0039, 0041, 0044. The ADR numbering skips 0049-0079 (no files). Human-decision register: [`governance/human-decision-register.md`](governance/human-decision-register.md).

## 50. HANDOVER HISTORY

| Date | Change |
|---|---|
| (earlier) | `HANDOVER-1`: index-style handover created; kept current per workstream |
| 2026-10-05 | Handover audit and launch-blocker/human-decision sections added; round-2 and blocker-clearing updates (`REGISTRY-HYGIENE-2026-10-05`) |
| 2026-10-06 | `HANDOVER-LIVE-2026-10-06`: rewritten as the live developer handover (this file), plus [`runbooks/developer-handover.md`](runbooks/developer-handover.md), the dated snapshot [`plans/prh2-hardening-round/developer-handover-2026-10-06.md`](plans/prh2-hardening-round/developer-handover-2026-10-06.md) and [`governance/human-decision-register.md`](governance/human-decision-register.md). The previous index content (topic map, launch blockers, human decisions, mock-vs-real matrix, secret names) is preserved inside sections 5, 20, 23, 31, 33. |


### 36b. Round 8 (2026-10-07): unblocked payout bundle merged

Merged (MOCK, security + ledger-finance reviewed, local evidence only): PAY-PAYOUT-UNBOUND-STANDING-1, PAY-PAYOUT-ASSET-ECHO-TEST-1, PAY-PAYOUT-CALLBACK-AUDIT-1, R-5. See registry rows `CLASSB-R8-*`. New required-before-non-MOCK-payout items: **PAY-PAYOUT-BOUND-CLEAR-1 (HIGH)**, **R-6** (succeeded-payout mismatched success does not page), PAY-PAYOUT-UNBOUND-RESOLVE-1 (still NOT IMPLEMENTED, owner/architect + LF), plus B13 (gate, brief `docs/governance/b13-decision-brief.md`). Follow-up: PAY-PAYOUT-CALLBACK-AUDIT-2. Still blocked: B13, RESOLVE-1, H-SEC-5/11 (rulings), ALERT-DELIVERY-1, B14-B18, sandbox PSP, AWS. Next unblocked engineering: PAY-PAYOUT-BOUND-CLEAR-1 (bound-class per-operation clearing; ruled direction in ADR 0095 s35.6) then R-6 (raise-only).

### 36c. Round 9 (2026-10-07): BOUND-CLEAR-1 and R-6 merged

Merged (MOCK, security + ledger-finance reviewed, local evidence only): PAY-PAYOUT-BOUND-CLEAR-1 (`8bd6e26`) and R-6 (`eee7fd6`); registry rows `CLASSB-R9-*`. Required before ANY non-MOCK payout (sandbox included): B13 (gate, brief `docs/governance/b13-decision-brief.md`), PAY-PAYOUT-UNBOUND-RESOLVE-1 (NOT IMPLEMENTED; until then every unbound or bound payout park is a permanent standing P1; must keep positive attribution, compare completion amount/asset, bound the persisted-lines query), PAY-PAYOUT-SUCCEEDED-REF-MISMATCH-1, ALERT-DELIVERY-1. Required before any real-provider DEPOSIT: PAY-DEPOSIT-MISMATCH-ALERT-1. Follow-up: PAY-PAYOUT-CALLBACK-AUDIT-2. R-5, R-6, STANDING-1, BOUND-CLEAR-1, CALLBACK-AUDIT-1 are all done. **Next unblocked engineering:** PAY-PAYOUT-SUCCEEDED-REF-MISMATCH-1 (raise-only + audit, R-5/R-6 pattern; ruled in LF M-1 review), then PAY-PAYOUT-CALLBACK-AUDIT-2. Still blocked: B13, RESOLVE-1 (owner/architect + LF), H-SEC-5/11 rulings, ALERT-DELIVERY-1, B14-B18, sandbox PSP, AWS.

### 36d. Round 10-11 (2026-10-07): payout/receipt hardening bundle merged

Merged (MOCK; security + ledger-finance reviewed; LOCAL evidence only): PAY-PAYOUT-SUCCEEDED-REF-MISMATCH-1, PAY-PAYOUT-CALLBACK-AUDIT-2 (incl. the S-1/C-1 orphan-receipt fix), PAY-RECEIPT-ORPHAN-RESOLVE-1 + C-1b, R3-TEST-GAPS-1 (all items), PAY-FPAY-HARDENING-1 F-L2 verified already implemented by B9 (F-L1 stays OPEN: ALERT-DELIVERY-1). See registry `CLASSB-R10-*`, `R3-TEST-GAPS-1-PARTIAL-*`, `CLASSB-R11-*`. Open engineering follow-ups: PAY-RECEIPT-DRAIN-MERCHANT-REF-1, PAY-RECEIPT-ANOMALY-APPLIED-1 (needs LF ruling), I-1 predates-submission on the live path (security policy), S-4 (M-1 may page on benign PSP behaviour; settle with the sandbox adapter acceptance list), PAY-DEPOSIT-MISMATCH-ALERT-1 (hard gate before real-provider deposit). Gates unchanged: B13, RESOLVE-1, H-SEC-5/11 rulings, ALERT-DELIVERY-1, B14-B18, sandbox PSP, AWS.

### 36e. Round 12 (2026-10-07): drain merchant-reference check merged

PAY-RECEIPT-DRAIN-MERCHANT-REF-1 merged at `bd3c144` (registry `CLASSB-R12-*`); merged-tree targeted `-race` 697 PASS / 0 SKIP / 0 FAIL, LOCAL evidence. **The MOCK-doable, decision-free payment/payout hardening backlog is now exhausted.** Remaining engineering items need a ruling or authorization: PAY-RECEIPT-ANOMALY-APPLIED-1 (LF re-attribution ruling), I-1 predates-submission on the live path (security policy), S-4 (benign-PSP paging, sandbox adapter acceptance), `GetAttemptByMerchantReference` tenant predicate (small defence-in-depth, optional), H-SEC-9 statement/lock timeouts and SWEEPER-CONCURRENCY-CAPS-1 (launch-scale design choices). **Gates (unchanged):** B13 (brief `docs/governance/b13-decision-brief.md`), PAY-PAYOUT-UNBOUND-RESOLVE-1 (permanent standing P1 until built), H-SEC-5/H-SEC-11 rulings, ALERT-DELIVERY-1 (recipients/channel), PAY-DEPOSIT-MISMATCH-ALERT-1 (before real-provider deposit), B14-B18, sandbox PSP, AWS. Environment note: scratch exports consume the per-session disk allowance (hit 94-96%); delete review exports under the scratchpad when finished.
