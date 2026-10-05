# Handover index

This is the entry point for an engineering team taking over the platform from this Git repository
alone. It **only links** to authoritative material and holds two small tables that exist nowhere
else: the mock-vs-real matrix and the secret-names inventory. Every workstream updates the rows it
materially changes as part of its Definition of Done (registry: HANDOVER-1). It is not a separate
documentation phase.

## Start here, in order

1. **[`CLAUDE.md`](../CLAUDE.md)**: the permanent project rules (tenancy, ledger, provider abstraction,
   security, compliance, no fake completion, environment safety).
2. **[`MASTER-BUILD-PROMPT.md`](../MASTER-BUILD-PROMPT.md)**: the stage model and how work is governed.
3. **Current state:**
   - [`docs/active-stage.md`](active-stage.md): the newest status note at the top is dated 2026-10-05 (PRH-2 implementation state incl. round 2 R2-A/E/F; the final PRH-2 gate is pending owner authorization, nothing is approved by it; gate report: [`plans/prh2-hardening-round/final-gate-report.md`](plans/prh2-hardening-round/final-gate-report.md)); per-merge detail is in the task-registry `PRH-2-*-MERGE-STATE` rows;
   - [`docs/governance/project-status.md`](governance/project-status.md): roadmap and stages;
   - [`docs/progress.md`](progress.md): per-stage narrative.
4. **Open work and decisions:**
   - [`docs/governance/task-registry.md`](governance/task-registry.md): every open item, blocker and
     `HD-*` human decision;
   - [`docs/decisions/`](decisions/): ADRs and human-decision records, for example
     [`0098`](decisions/0098-human-decision-response-force-resolve-and-manual-adjustment.md).
5. **The latest completed block:**
   [`docs/governance/payment-readiness-completion-report.md`](governance/payment-readiness-completion-report.md).
   Its §11 (launch blockers) is a 2026-09-28 snapshot and is **superseded as of 2026-10-05** by the "Launch blockers (current)" section below.
   The current round's plan is [`docs/plans/prh2-hardening-round/plan.md`](plans/prh2-hardening-round/plan.md).

## Where each topic lives

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
| Launch blockers | Section [Launch blockers (current)](#launch-blockers-current) below |
| Human decisions | Section [Human decisions awaiting the owner](#human-decisions-awaiting-the-owner) below; registry `HD-*` / `HQ-E1-*` / `HD-CTF-*` rows; ADR [0098](decisions/0098-human-decision-response-force-resolve-and-manual-adjustment.md), [0105](decisions/0105-human-decision-response-closed-tenant-funds-and-e1.md), [0042](decisions/0042-human-decision-response.md); HDR registers ADR [0039](decisions/0039-human-decision-register-stage-4h-b0-r7.md), [0044](decisions/0044-human-decision-register-stage-4i-phase-d.md) |
| Technical debt | Registry "QA Technical Debt Classification (S91-05)"; `testing/testing-strategy.md` "Linter" (integration-tagged files are not linted); `runbooks/observability-and-alerting.md` "Known gaps" |
| Governance and agents | [`governance/`](governance/): change control, ownership, agent registry; `.claude/agents/` (specialist definitions); DB credential incident record `governance/incident-2026-09-27-local-db-credential-mutation.md` |

## Launch blockers (current)

Snapshot 2026-10-05, derived from [`governance/task-registry.md`](governance/task-registry.md) (the registry stays authoritative; IDs below are registry IDs; see also the row `REGISTRY-HYGIENE-2026-10-05`). It **supersedes** `payment-readiness-completion-report.md` §11. Everything is MOCK, AWS is OFF, and no GitHub CI evidence exists (CI-BILLING-1). Nothing here is a statement that any item is approved or complete.

**Platform-wide / security**
- **TRIGGER-SEARCH-PATH-1 (+ `-UPGRADE`, `-0116`): HIGH, LAUNCH BLOCKER until DEPLOYED and VERIFIED.** The exploitation path (TEMP-table shadowing of the unpinned 0026..0113 functions bypassed the K2 four-eyes check) is CLOSED in the codebase for the runtime role and other non-owner roles by migration 0116 / ADR 0108 (REVOKE TEMP, owner-authorized): IMPLEMENTED, LOCAL EVIDENCE ONLY. Still required before it stops blocking (only `security` may lower it): 0116 applied in every environment that serves real money and the runbook verification SQL passing (runbooks/operational-runbooks.md §7 step 5); the service connecting as `igaming_runtime`, never the database owner (PLAT-ROLESPLIT-1: dev `make run`/`.env.example` use the owner); 0116/role-init applied BEFORE runtime traffic or runtime backends recycled; no other principal that can write K2 rows holding TEMP/CREATE. NOT IMPLEMENTED (defence in depth): pinning the 0026..0113 functions.
- NULL-ARM-WRITE-1 (Medium): NULL-tenant RLS write arms admit sessions without a tenant GUC; the human decides whether it blocks launch.
- STAFF-LIFECYCLE-1: audited staff suspend/role change before real-money K operations. MANUAL-ADJ-LINK-1 (= LEDGER-MANUAL-ADJ-LINK-1): preventive trigger, blocks the first real-money tenant.
- ALERT-DELIVERY-1 (OPEN; routing readiness smallest cut IMPLEMENTED/MOCK in 0117 / ADR 0102 §18, alert_routing_ready is always 0, NO real channel and NO recipient exist), HD-PRH2-4-OPS, PAY-P1-MULTISUCCESS-ALERT-1: no one is paged. Binding before the first human-notification kind: ADR 0102 §18.4. ALERT-RETENTION-1 before production.
- CI-BILLING-1 with F-POOL-1 K1, TEST-RESISO-RACE-1 (timing lane NOT GREEN locally) and PRH-FPOOL1: green GitHub CI is required for the final gate. BRANCH-PROTECTION-1.
- Backup/DR: NOT MET (`runbooks/backup-and-disaster-recovery.md`; no registry ID). DEVOPS-0107-INDEX-WINDOW-1, CAS-RECON-SCALE-1, WEBHOOK-EDGE-1 before real-money scale.

**Payments / payouts**
- PROV-OUTBOUND-CRED-1 precondition and F-POOL-2: per domain, before the first non-MOCK adapter.
- PAY-K3-STATEMENT-SOURCE-WIRING-1 (engineering item IMPLEMENTED against MOCK for mock-payments only; the REAL-MONEY precondition is NOT satisfied: H-W1 and a real PSP statement source remain), PAY-K3-FOLLOWUPS-1, PAYOUT-AMOUNT-DISPUTE-1, WITHDRAWAL-REVERSAL-1, PAY-SEC-LAUNCH-1, PAY-H-FOLLOWUPS-1.
- Before the first real PSP or real-money tenant: PAY-DEPOSIT-ESCALATION-1, PAY-RECEIPT-T4-DRAIN-TEST-1, PAY-POLL-DECLINED-ALERT-RECON-1, PAY-POLL-ECHO-HARDENING-1, PAY-PAYOUT-REFBIND-1, PAY-FPAY-HARDENING-1, PAY-CALLBACK-MISMATCH-BIND-1, PAY-PAYOUT-UNBOUND-HOLD-1, PAY-RECON-POLL-REF-CLEAR-1 + PAY-RECON-PARKED-CAPTURE-STANDING-1 (hard prerequisite), MA020-SYNC-MISMATCH-1, PRH-I1-MANIFEST-2/3/4. PAY-PSP-CONTRACT-INVDEP1 is a vendor-selection criterion.

**Casino / sportsbook**
- CAS-GAME-KILL-BET-1 (blocks real-money casino); CAS-BET-REQUIRES-BOOTSTRAP-1, CAS-PLAYER-REF-1, CAS-WIN-ANOMALY-1, PRH-REF-C2, CAS-STMT-IO-1, PAY-K3-MR020-HTTP-MAPPING-1 before a real casino/sportsbook provider; CAS-WIN-IDEMP-1 before bonus-funded stakes.
- Sportsbook go-live is gated by HDR-SB-1 / HDR-J-7 (ADR 0083); a bounded catalogue-fetch timeout is needed before a real adapter.

**KYC / compliance**
- KYC-ENFORCE-1 (PARTIALLY IMPLEMENTED; remaining B7, F4/F5, LF-I3-4/5, HD-KYC-1..8 thresholds). KYC-E1-FOLLOWUPS-1 (content-read seam and real-adapter intake before a real KYC vendor; KYC-OUTBOX-REQUEUE-1 is a priority). WD-RG-1 (no RG/Risk gate on withdrawal; not scoped).

**Staging / AWS (before the next deployment)**
- ACCESS-ANALYZER-CHECK-1, DEPLOY-FPKEY-1, SECRETSTORE-AWS-1 IAM part (HD-10.3-2).

## Human decisions awaiting the owner

Snapshot 2026-10-05; each has a registry row (the `HQ-E1-*` and `HD-CTF-*` rows were appended 2026-10-05).
- **Infrastructure / accounts:** CI-BILLING-1, BRANCH-PROTECTION-1, ACCESS-ANALYZER-CHECK-1, HD-10.3-2 (AWS IAM / awssm deploy), DEPLOY-FPKEY-1.
- **Security:** the TRIGGER-SEARCH-PATH-1 fix route is DECIDED (REVOKE TEMP, 0116); remaining owner/security actions: deployment verification per environment (and whether the production gate should also cover staging), PLAT-ROLESPLIT-1 (runtime binary never connects as the database owner); NULL-ARM-WRITE-1 launch-blocking or not.
- **Testing:** the ADR 0094 timing-lane decision. 2026-10-05 idle characterization (evidence/prh2-timing-lane-characterization.md): `TestResolutionIsolation_NormalOperation` 500 ms p100 over 90 callbacks under `-race` on 4 vCPU fails at HEAD (0/10 interleaved, 0/13 earlier), also on ecd2b74 (1/10) and the pre-PRH-2 baseline 94b4ae5 (2/10); the bound is NOT changed and ADR 0094 is NOT edited. Options for the owner/architect/security (proposal text in the characterization): environment-calibrated relative assertion (recommended target) with p95 + hard p100 ceiling interim; raising the bound needs a recorded security decision. The lane is reported FAILING, never skipped.
- **Reconciliation / payments (new 2026-10-05):** H-W1: the statement sweep covers active tenants only while ADR 0101 R-5 permits M2 on a closed tenant (refuse M2 on unswept tenants, or sweep non-active tenants). LAUNCH-BLOCKING for a real-money tenant on a real PSP; engineering default: closed-tenant mechanism stays disabled.
- **Alerting:** HD-PRH2-4-OPS (real recipients, on-call). HD-PRH2-8 (below-threshold four-eyes semantics; non-blocking, interim enforced). HD-PRH-1 (are tenant slugs confidential? gates WEBHOOK-PATH-TOKEN-1).
- **KYC:** HD-KYC-1..8 thresholds (ADR 0096 §4); HQ-E1-1..4 (ADR 0106 §12: suspended/closed-tenant submissions, severity of `kyc.submission_failed_terminal` before 0114 reaches a real tenant, jurisdiction deadlines, outbox retention); KYC-FX-AGG-1 (no FX source).
- **Closed-tenant funds:** HD-CTF-1..9 (ADR 0107 §12). ADR 0107 is DESIGN ONLY until these are answered and the human authorizes a workstream. (HD-PRH2-9 is decided, ADR 0105 §1.)
- **Jurisdiction / sportsbook:** HDR-J-7/8/9 (ADR 0044), HDR-M-1/2 (stage-4i-exit-register), HDR-SB-1 (ADR 0083 §8.2); HDR-J-1..6 answered (ADR 0042) with legal-review residuals. WD-RG-1: add to scope?
- **Standing human-only items:** vendor/PSP/custodian selection, contracts, production credentials, jurisdictions beyond Anjouan, hosting AUP (13-dependency-map R6), production launch, and authorization of the final PRH-2 gate / next stage (PRH-GATE, "Not started").

## Mock vs real (keep current)

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
| Alert delivery / paging | Durable alerts (`internal/alerting`, migration 0110, ADR 0102): log sink IMPLEMENTED, mock sink MOCK. The dispatcher runs in `cmd/platform-api` with the log sink only (PRH-2 I-wire, merged `dcaa2c6`); every alert is `unrouted`, nothing is delivered to any person. No routes or recipients are configured. ALERT-DELIVERY-1 OPEN (closure conditions: task-registry PRH-2-IWIRE-MERGE-STATE). | HD-PRH2-4-OPS (real recipients); a real channel adapter must dedupe on `<alert_id>:<step>` (ADR 0102 §16.3) | **2026-10-05 (R2-E):** routing readiness smallest cut (migration 0117, ADR 0102 §18) is IMPLEMENTED/MOCK: routes are disabled by default, a DB guard refuses any human-notification route, unrouted deliveries are recorded with a reason, `alert_routing_ready{severity}` and the platform-only status endpoint report NOT READY with the exact missing operator input; the `alertingtest` channel is test-only. NO real channel, NO recipient, NO vendor: log/mock delivery is never a human notification.
| Secret store | `devfile`/memory locally; `awssm` backend exists (AWS OFF) | ADR 0093/0094; production configuration checklist |

## Secret names inventory (names only, never values)

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
