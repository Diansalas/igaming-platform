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
| R5 | Bonus abuse (multi-accounting, bonus hunting) erodes margin | Financial/Fraud | High | Medium–High | Cross-brand `person` resolution, device/payment fingerprinting, velocity caps, manual review queue owned by `bonus-engine`. **Partially mitigated as of Stage 4H-B1 Wave 3**: `detectMultiAccountFirstDepositSignal` (`internal/bonus/deposit_sweep.go`, REQ-SEP-BONUS-3) resolves the depositing account's `person_id` on a `FirstDepositOnly` Offer match and audits `bonus_deposit_sweep.multi_account_signal_detected` when that Person already holds a Grant against the same Offer via a different `player_account_id`. It is a **signal only** — never a denial — as `security-architecture.md` §W15.1.5 requires and explicitly forbids turning into a block. Device/payment-fingerprint correlation and velocity caps remain `NOT IMPLEMENTED` and need a fraud/device-signal source this platform does not have |
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
| R16 | **A lock-acquisition order that has no cycle today acquires one when a new participant is added** — two correct, independently-reviewed code paths take the same two blocking locks in opposite orders, and neither review sees the other's order | Availability/Operational | Medium (structural; already occurred once) | Medium (a Postgres-detected deadlock aborts one transaction — no financial-integrity breach, but a failed bet or a failed staff action) | The pinned, jointly-stated order (`34-economic-operation-identity.md` §5.6, HR-21 extended: `correlation_id` → `grant_id` → **any** `wallet_balance_projection` row) plus invariant EOI-20 as a code-review gate. Found by `architect` in Stage 4H-B1 Wave 3 Phase 10 as `DR-4HB1W3-ARCH-01`, exactly where `ledger-accounting-model.md` §6.6.16 predicted it ("'no cycle today' is not a property that survives a fourth participant") |

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

## Scheduled cross-domain items — Stage 4H-B1 Wave 3 Phase 10 (`architect`)

Items this Wave's composition review routed to another owner rather than
resolving. **None authorizes work**; each exists so the item survives the
end of the Wave and is not rediscovered from scratch. Full reasoning for
every row: `docs/decisions/0040-wave-3-cross-domain-composition-rulings.md`.

| ID | Item | Owner | Gate |
|---|---|---|---|
| `DR-4HB1W3-ARCH-01` | `postBet` takes the `player_cash` projection row lock **before** the Grant advisory lock, the reverse of HR-21 extended (doc 34 §5.6). Latent today — no Bonus path posting to `player_cash`/`house_gaming` is reachable | `casino` + `bonus-engine` | **Gated**: before any conversion trigger, before `postBet`'s bonus-funded leg, before anything else making `ConvertGrant`/`ACTION_ROUTE_TO_CASH`/`postWinLockedBonus` concurrently reachable with a bet |
| `DEP-EOI-8` | `bonus_campaign_activation` is a declared EOI type with no mint point and no consumption/bounds declaration, while Wave 3 made its effecting writes (the deposit sweep, the cashback scheduler) live | `bonus-engine` + `security` | **Gated**: before campaign-driven automatic issuance can actually succeed, i.e. as part of whatever closes the sweeps' jurisdiction-resolution gap |
| KYC-tier taxonomy | `bonus_offer_versions.kyc_rg_level_required` is stored and never read; `player_accounts.kyc_tier` has the right name but no operative meaning (always 0, never written anywhere). Defining what a tier means, what promotes a player between them, and who owns writing it **is** the missing contract — it cannot be reused around | `identity-compliance` (design) → `architect` (contract sign-off) → `bonus-engine` (enforcement) | Not urgent; needs its own dispatch. Confirmed NOT NEEDED for Wave 3 by `identity-compliance` Phase 5 and by `architect` |
| `contribution_weight_table` authoring validation | Nothing validates the table at any layer: the HTTP handler binds the raw JSON string through and Postgres only checks it is valid JSON. Must be **rejected** at create-version time if it does not parse or carries a weight outside `[0, 10000]` | `bonus-engine` | ADR 0040 D3 ruling item 1. The bet-time clamp (D3 item 2) is already `IMPLEMENTED` and is the fail-safe underneath this, not a substitute |
| Unparseable-table audit signal | `ResolveContributionWeightBP` must distinguish **absent** (no signal; 100% is the intended default) from **present but unparseable** (a platform data defect that must emit an audited signal naming the `offer_version_id`) | `bonus-engine` | ADR 0040 D3 ruling item 3. Needs `ctx`/`tx` threaded into a currently-pure function — a signature change in their code |
| `ConsumeRootBudget` nil-subject refusal | Under `single_subject` scope, a caller passing `uuid.Nil` silently bypasses the containment check. Latent (no caller does) | `bonus-engine` | `security` W3P6.5 item 3. One-line fail-closed addition when that function is next touched |
| EOI-mint audit metadata | `MintRootOperation` audits with only `operation_type` — no IP, user-agent, request id, or the authorization bounds themselves. "Who authorized this budget, from where, for what" is unanswerable from the audit trail alone | `bonus-engine` | `security` W3P6.5 item 4 |
| `StakedBonusAmount` ledger-read hardening | §6.6.4's "read from the posted ledger entry, never from the caller" is now satisfied by call-site construction across two packages, with no test asserting it. Recommended: have `RecordWageringContribution` verify the amount against the actual posted debit on `LockLedgerTransactionID` | `bonus-engine` + `ledger-finance` | ADR 0040 RC-4. Not blocking — the value is provably correct today |
| `bonus_adjustment_write` / `grant_forced_conversion` / `grant_cancel_completed` | The three un-wired `ChangeOperation`s. Design constraints (Risk `Operation` minting, gate-asymmetry split by direction, what "forced" may and may not override, posting shapes) are recorded so they are not re-litigated or guessed | `ledger-finance` (posting shapes) → `architect` (scope, already ruled) → `bonus-engine` | ADR 0040 RC-1/RC-2/RC-3. Out of Wave 3 scope by the dispatch's own instruction |
| LF-10 | Rollback of an already-resolved financial event. **Unchanged by Wave 3** (re-verified: `postRollbackHeldWin` is untouched in `9d857dc..8c6ab7a`). Fails closed today; doubly inert until `postBet`'s bonus-funded locking side exists | `ledger-finance` | Future dedicated dispatch. Orthogonal to every Wave 3 scope item |
