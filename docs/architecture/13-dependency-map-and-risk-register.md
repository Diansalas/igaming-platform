# 13 — Dependency Map and Risk Register

Status: Stage 0 draft.

## External dependencies (vendor relationships required)

| Category | Example vendors (Blueprint §3, §7) | Blocks |
|---|---|---|
| Game aggregator | Hub88, SoftSwiss, Pariplay | `casino` real integration; catalogue |
| Sportsbook provider | Altenar, BetBy, Digitain | `sportsbook` real integration |
| PSP / high-risk acquirer | Specialist Anjouan-friendly acquirers | `payments` real integration; go-live with real money |
| Crypto rails/custodian | Self-custody infra, or Fireblocks/BitGo | `payments` crypto flows |
| KYC/AML vendor | SumSub, Veriff, Jumio | `identity-compliance` real verification |
| Affiliate platform | Income Access, MyAffiliates | Affiliate attribution (buy first, per Blueprint §1) |
| Hosting | Hetzner/OVH/Leaseweb (AWS/GCP need written gambling-AUP confirmation) | Every deployed environment |
| Secrets/KMS | Vault or cloud KMS provider | Any environment holding real credentials |
| Certification body | GLI (GLI-19, GLI-33) | B2B sale readiness, not MVP |
| Gaming lawyer | Jurisdiction-specific (Blueprint recommends Malta-based) | Licensing structure, contracts, all Q1–Q6 decisions |

None of these are blocked from *engineering* progress today — Stage 0–3
work proceeds against mocks/sandboxes per `CLAUDE.md`. They block **real-
money go-live** and the B2B sale, not the build.

## Risk register

| # | Risk | Category | Likelihood | Impact | Mitigation |
|---|---|---|---|---|---|
| R1 | Ledger correctness bug causes balance drift or double-credit | Financial | Low (with discipline) | Severe | Append-only double-entry design, mandatory `ledger-finance` review, full financial test matrix, hourly reconciliation (see `06-wallet-ledger-architecture.md`) |
| R2 | Tenant-isolation bug leaks data across tenants/brands | Security/Compliance | Low (with RLS) | Severe | RLS enforced at DB layer, mandatory tenant-isolation tests on every endpoint, `security` review |
| R3 | Payment provider onboarding takes longer than expected (Anjouan operators rejected by tier-1 PSPs) | Commercial | High | High — delays real-money launch | Start PSP/acquirer conversations in parallel with engineering (Blueprint §9 "start in month zero"); design orchestration layer so a new PSP is a routing config, not a rewrite |
| R4 | Provider integration (game/sportsbook) API drift breaks production silently | Operational | Medium (recurring) | Medium | Internal provider interface isolates drift to one adapter; reconciliation catches financial drift; budget ~1 engineer per 15–20 live integrations for maintenance (Blueprint §1) |
| R5 | Bonus abuse (multi-accounting, bonus hunting) erodes margin | Financial/Fraud | High | Medium–High | Cross-brand `person` resolution, device/payment fingerprinting, velocity caps, manual review queue owned by `bonus-engine` |
| R6 | Hosting provider terminates service over gambling AUP mid-build | Operational | Low if verified early, High if not | Severe (forced migration) | Get written confirmation before building (Blueprint §10 Q6) — tracked as an open decision |
| R7 | Regulatory/compliance requirement misunderstood or under-built, found at audit | Compliance | Medium | Severe (licence risk) | Jurisdiction-pluggable design, compliance officer involvement (human, not automatable), explicit "software capability ≠ legal approval" labeling |
| R8 | Team underestimates that integrations "never finish" (ongoing maintenance load) | Delivery/Cost | High | Medium | Budgeted explicitly in `14-mvp-scope-and-roadmap.md` effort assessment; roadmap plans for steady-state maintenance headcount |
| R9 | Multi-tenancy retrofitted too late, forcing a rewrite for the first B2B partner | Architecture | Low (if Stage 2 done right) | Severe | `tenant_id` + RLS from Stage 2, isolation-tightening path designed in from the start (see `03-database-architecture.md`) |
| R10 | Custody model chosen without adequate insurance/legal review (crypto) | Financial/Legal | Medium | Severe | Explicit open decision (Q4), routed to human before crypto rail implementation begins |
| R11 | Certification (GLI-19) requirements retrofitted onto a team that ships without change control | Delivery | Medium | Medium–High | Start certification-compatible discipline (reproducible builds, change control) from Stage 1, per Blueprint §8 |

## Dependency-driven sequencing implication

R3, R6, R10 all argue for starting vendor/legal/hosting conversations in
parallel with Stage 0–1 engineering, not after — this is a human/business
track that runs alongside the engineering stage gates, not inside them.
