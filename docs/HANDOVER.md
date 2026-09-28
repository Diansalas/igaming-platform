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
   - [`docs/active-stage.md`](active-stage.md): the status notes at the top are current;
   - [`docs/governance/project-status.md`](governance/project-status.md): roadmap and stages;
   - [`docs/progress.md`](progress.md): per-stage narrative.
4. **Open work and decisions:**
   - [`docs/governance/task-registry.md`](governance/task-registry.md): every open item, blocker and
     `HD-*` human decision;
   - [`docs/decisions/`](decisions/): ADRs and human-decision records, for example
     [`0098`](decisions/0098-human-decision-response-force-resolve-and-manual-adjustment.md).
5. **The latest completed block:**
   [`docs/governance/payment-readiness-completion-report.md`](governance/payment-readiness-completion-report.md).
   The current round's plan is [`docs/plans/prh2-hardening-round/plan.md`](plans/prh2-hardening-round/plan.md).

## Where each topic lives

| Topic | Authoritative material |
|---|---|
| Product and requirements | `iGaming-Platform-Blueprint.pdf`; [`architecture/00-system-overview.md`](architecture/00-system-overview.md), [`01-requirements-inventory.md`](architecture/01-requirements-inventory.md), [`14-mvp-scope-and-roadmap.md`](architecture/14-mvp-scope-and-roadmap.md) |
| Services and boundaries | [`architecture/02-domain-and-service-boundaries.md`](architecture/02-domain-and-service-boundaries.md), [`13-dependency-map-and-risk-register.md`](architecture/13-dependency-map-and-risk-register.md) |
| Database, RLS and tenancy | [`architecture/03-database-architecture.md`](architecture/03-database-architecture.md); [`security/runtime-role-separation.md`](security/runtime-role-separation.md); `migrations/` (checksummed, gap-free: `go run ./cmd/migrate verify`) |
| APIs | [`architecture/04-api-architecture.md`](architecture/04-api-architecture.md); [`docs/api/`](api/) |
| Identity, auth and RBAC | [`architecture/05-identity-architecture.md`](architecture/05-identity-architecture.md); [`security/security-architecture.md`](security/security-architecture.md); `internal/auth/permission.go` (role → permission map) |
| Wallet and ledger | [`architecture/06-wallet-ledger-architecture.md`](architecture/06-wallet-ledger-architecture.md), [`ledger-accounting-model.md`](architecture/ledger-accounting-model.md), [`financial-domain-model.md`](architecture/financial-domain-model.md), [`financial-transaction-flows.md`](architecture/financial-transaction-flows.md) |
| Payments, deposits, payouts | [`architecture/07-payments-architecture.md`](architecture/07-payments-architecture.md), [`payment-orchestration.md`](architecture/payment-orchestration.md); ADR [0095](decisions/0095-provider-io-transaction-boundary-and-payment-contract.md) (transaction boundary, INV-DEP-1 §28, F-POOL-2 §29, kill switch §10) |
| Withdrawals | [`architecture/withdrawal-state-machine.md`](architecture/withdrawal-state-machine.md), [`withdrawal-policy-configuration.md`](architecture/withdrawal-policy-configuration.md) |
| Reconciliation | [`architecture/reconciliation-model.md`](architecture/reconciliation-model.md) |
| Casino, sportsbook | [`architecture/08-casino-integration-architecture.md`](architecture/08-casino-integration-architecture.md), [`09-sportsbook-architecture.md`](architecture/09-sportsbook-architecture.md); [`integrations/`](integrations/) |
| KYC, AML, RG | [`architecture/11-kyc-aml-rg-architecture.md`](architecture/11-kyc-aml-rg-architecture.md); ADR [0096](decisions/0096-kyc-enforcement-boundary.md) |
| Bonus, gamification, retail | [`architecture/10-bonus-engine-architecture.md`](architecture/10-bonus-engine-architecture.md), `17`–`31` and [`26-retail-operations-architecture.md`](architecture/26-retail-operations-architecture.md) |
| Jurisdiction and licensing | [`architecture/15-jurisdiction-and-licensing-model.md`](architecture/15-jurisdiction-and-licensing-model.md) |
| Audit and reporting | [`architecture/12-audit-reporting-architecture.md`](architecture/12-audit-reporting-architecture.md) |
| Privacy | [`architecture/16-privacy.md`](architecture/16-privacy.md) |
| Webhook security | ADR [0097](decisions/0097-webhook-admission-and-rate-limiting.md) and the webhook ADRs it cites |
| Provider credentials and secrets | ADR [0093](decisions/0093-provider-credential-model-and-secret-store.md), [0094](decisions/0094-secret-resolution-resource-isolation.md) |
| Deployment and AWS (currently OFF) | [`architecture/38-deployment-architecture.md`](architecture/38-deployment-architecture.md); `deploy/`; [`runbooks/stage-9-4-staging-lifecycle-runbook.md`](runbooks/stage-9-4-staging-lifecycle-runbook.md) |
| Operations, monitoring, backups | [`runbooks/`](runbooks/README.md): operational runbooks, observability and alerting, backup and DR, production configuration checklist |
| Testing and CI | [`testing/testing-strategy.md`](testing/testing-strategy.md); `.github/workflows/ci.yml`. GitHub CI is currently blocked by billing (registry CI-BILLING-1); local runs are never reported as CI. |
| AI architecture (future) | ADR 0089 (architecture only; nothing implemented) |
| Governance and agents | [`governance/`](governance/): change control, ownership, agent registry; `.claude/agents/` (specialist definitions); DB credential incident record `governance/incident-2026-09-27-local-db-credential-mutation.md` |

## Mock vs real (keep current)

Every external capability is **MOCK** today. No real vendor is selected or integrated, and no
production credential exists.

| Capability | Status | Interface / gate before going real |
|---|---|---|
| Payments (PSP), deposits and payouts | MOCK (`internal/payments` mock provider) | ADR 0095; PROV-OUTBOUND-CRED-1 tripwire `TestOutboundPrecondition_EveryWiredAdapterIsSynthetic`; PRH-2 C/D/E2/H; PAY-PSP-CONTRACT-INVDEP1; PAY-SEC-LAUNCH-1 |
| Payment statements / reconciliation source | MOCK source (PRH-I5) | Real PSP statement format (provider dependent) |
| Casino aggregator | MOCK (`internal/casino` mock) | CAS-REVOKE-CONSUMED-1 and CAS-PLAY-BOOTSTRAP-1 (PRH-2 A/B); tripwire |
| Sportsbook provider | MOCK / in-house mock mode (ADR 0087/0088) | SB-CATALOGUE-IO-1 (PRH-2 E3); bet-placement contract unknown |
| KYC vendor | MOCK | KYC-SUBMIT-OUTBOX-1 (PRH-2 E1); KYC-ENFORCE-1; HD-KYC-1..8 thresholds |
| Crypto custody | NOT IMPLEMENTED (interface only, ADR 0008) | Custodian selection (human) |
| Email | MOCK (`internal/email` mock provider) | Vendor selection |
| Alert delivery / paging | Log lines only today; PRH-2 I adds durable alerts and a mock sink | HD-PRH2-4-OPS (real recipients) |
| Secret store | `devfile`/memory locally; `awssm` backend exists (AWS OFF) | ADR 0093/0094; production configuration checklist |

## Secret names inventory (names only, never values)

Values never go into Git. Locally they come from an untracked environment. For a deployed
environment they come from the secret store per ADR 0093/0094 and
[`runbooks/production-configuration-checklist.md`](runbooks/production-configuration-checklist.md).
The checked-in `.env.example` holds placeholders only.

| Name | Holds | Where it belongs |
|---|---|---|
| `DATABASE_URL` | Runtime DB connection, including its credential | Deployment secret store / task environment |
| `JWT_SIGNING_SECRET`, `JWT_PREVIOUS_SECRET` (+ `JWT_ACTIVE_KID`, `JWT_PREVIOUS_KID`) | Access-token signing keys and rotation | Deployment secret store |
| `PROVIDER_CREDENTIAL_FINGERPRINT_KEY` | Key for provider-credential fingerprints (ADR 0093) | Deployment secret store |
| `SEED_ADMIN_PASSWORD` | One-off initial admin password (`cmd/seed-admin`) | Supplied at seed time only; never stored |
| Provider credentials (per tenant, per provider) | PSP/casino/KYC API and webhook secrets | Secret store, referenced by `provider_credentials` rows (ADR 0093); never in env or Git |
| `TEST_DATABASE_URL`, `TEST_RUNTIME_DATABASE_URL`, `TEST_ADMIN_DATABASE_URL` | Synthetic local/CI test DBs | Local harness / CI only |

Non-secret configuration (`APP_ENV`, timeouts, intervals, rate limits, `SECRETSTORE_BACKENDS`,
`AWS_REGION`, OTel settings and so on) is read in `internal/config/config.go`.
