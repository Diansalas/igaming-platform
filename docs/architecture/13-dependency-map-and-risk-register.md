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
| Secrets/KMS | Vault or cloud KMS provider | Any environment holding real credentials — **and, added in Stage 4H-B1 Wave 1.5 Fix Wave, the affiliate `tracking_token` key material specifically**: per-tenant derivation, `key_version` on the row, rotation with overlapping validity, and immediate per-tenant revocation (doc 32 §5.1.2, DEP-AFF-8). This is the first concrete, design-level consumer of this row rather than a general environment concern |
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
| R12 | **A mass-action control is present, correct, and bypassed** — an authorized operation is executed as N individually-sub-threshold operations (a CRM journey issuing N single grants instead of one bulk job), or a retry/resume/pagination mints a fresh authorization nobody approved | Security/Financial | **High without a mechanism** (this is a supported execution pattern, not an attack) | Severe (mass unauthorized grant / double settlement) | `EconomicOperationIdentity` (`34-economic-operation-identity.md`): an authorization mints exactly one identity; every execution it causes inherits it; the domain that **creates the value** enforces the ceiling, never the requester. Found as `security` SEC-W15-02 (P0) and `ledger-finance` LF-6 in Stage 4H-B1 Wave 1.5 |
| R13 | **A four-eyes control is satisfied by the counterparty it is meant to constrain** — an external commercial principal (an affiliate) authenticating in the staff principal space approves a decision benefiting its own organisation | Security/Financial | Medium–High (structural, once affiliates exist) | Severe (self-dealing on the affiliate money path) | A distinct principal class with deny-by-enumeration (doc 32 §3.2); the approver-is-INTERNAL test as an **explicit conjunct** of the governance trigger, plus subtree-closure containment and a declared/audited beneficial-interest attestation (doc 32 §6.5.1). Found as `security` SEC-W15-01 (P0) in Stage 4H-B1 Wave 1.5 |
| R14 | **A fail-closed control ships inert** — a config-absent default resolves to "no threshold configured, therefore no approval," so a four-eyes control that reads as correct never fires | Security | Medium (recurring; already found twice) | High | Every threshold defaults to `0` with `required_approvals = 2`, mirroring `internal/withdrawal/policy.go`'s `defaultApprovalPolicy`; every such control carries a zero-config test asserting approval **is** required. Found as `security` SEC-W15-07, and previously as the Stage 4H-B0-R6 inert-self-approval P1 |
| R15 | **A protective signal (RG/self-exclusion) is laundered into a marketing inclusion audience** through composition — a nested segment reference, or a lifecycle-state label whose value set silently includes an RG-derived state | Compliance/Player-harm | Medium (no attacker required; ordinary operator configuration) | Severe (licence risk; targeting vulnerable players) | `InclusionSafety` as a **computed property of a criteria tree** propagating through `member_of` and declared protective inputs, with per-state `derivation_source` declarations that are permit-by-enumeration (doc 30 §8.4.1/§8.4.3, doc 31 §3); plus the independent live `rg.EvaluateEligibility` at the send gate. Found as `code-reviewer` P1-2 in Stage 4H-B1 Wave 1.5 |

## Dependency-driven sequencing implication

R3, R6, R10 all argue for starting vendor/legal/hosting conversations in
parallel with Stage 0–1 engineering, not after — this is a human/business
track that runs alongside the engineering stage gates, not inside them.

## Internal (domain-to-domain) dependency graph — Stage 4H-B1 Wave 1.5

This document's external-vendor map above is unchanged (with the one
Secrets/KMS narrowing noted in its row). The **internal** build-order
graph — which domain can be built before which, and the cross-cutting
items that sit upstream of the new commercial domains — is maintained in
`33-cross-domain-commercial-flow-map.md` §4 rather than duplicated here.
It authorizes no sequencing; it exists so the human's stage-sequencing
decisions are made with the dependencies visible.

**Updated in the Stage 4H-B1 Wave 1.5 Fix Wave**: the upstream
cross-cutting list grew from two to five. The original two (an event
transport decision; a consent model) are unchanged and still block CRM.
Three more were identified by Phase 2 review and are recorded in doc 33
§4.1:

| Upstream item | Owner | Blocks |
|---|---|---|
| Event transport (outbox vs. broker) | Orchestrator → human | CRM journeys |
| A consent model | identity-compliance (DEP-CRM-1) | Every CRM send |
| **`EconomicOperationIdentity`** (`34-economic-operation-identity.md`) | architect | Every grant-causing CRM path; affiliate settlement and re-attribution. Risk R12 |
| **A shared four-eyes *consumption* function** (a generalization of, or sibling to, the implemented `asset_change_consume_approved_request`) | security + ledger-finance (DEP-EOI-2 / DEP-AFF-6 / DEP-CRM-5) | Every four-eyes control in docs 31/32/34 — until it exists they are data fields, not controls |
| **An affiliate principal class** with deny-by-enumeration | security (DEP-AFF-1) + identity-compliance (DEP-AFF-9) | Affiliate four-eyes. Risk R13 |
