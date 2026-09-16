# 02 — Domain and Service Boundary Proposal

Status: Stage 0 proposal, owned by `architect`. **This document describes
the target shape for later stages, not what exists after Stage 1.** Per
`docs/decisions/0010-stage1-single-service-foundation.md`, Stage 1 ships
exactly one deployable (`platform-api`) organized internally by package
along these same lines; services below are split out into their own
deployables only as real domain logic is built and a concrete ownership/
scaling reason exists, not preemptively.

## Principle

Service boundaries follow the nine core services in Blueprint §4, grouped
by who is authorized to change them (see `.claude/agents/`). A boundary is
correct if a single specialist can own it without needing another
specialist's sign-off for routine changes, while cross-cutting invariants
(money, tenant isolation, audit) are enforced by shared, review-gated
components rather than duplicated per service.

## Proposed services (Stage 1+ scaffolding, not all built at once)

| Service | Owns | Primary specialist |
|---|---|---|
| `identity` | Person/player model, sessions, cross-brand resolution | identity-compliance |
| `tenant-config` | Brand/tenant configuration, versioning | backend / architect |
| `wallet` | Ledger, account balances, idempotent postings | ledger-finance |
| `game-gateway` | Aggregator/provider adapters, catalogue, launch tokens, wallet-callback endpoint | casino |
| `sportsbook-adapter` | Sportsbook provider integration, open-bet liability | sportsbook |
| `bonus-engine` | Campaign/Offer/Grant/Progress, rule evaluation | bonus-engine |
| `payment-orchestrator` | PSP/crypto routing, reserve accounting, withdrawal workflow | payments |
| `compliance` | KYC/AML orchestration, RG controls, case queue | identity-compliance |
| `backoffice-api` / `partner-console-api` | Admin operations, RBAC-gated | backend / backoffice |
| `audit` | Append-only audit log, shared library used by every mutating service | security (design), backend (implementation) |
| `event-bus` (Kafka/NATS) | Cross-service event distribution | integrations / architect |
| `reporting` | CDC → ClickHouse, report definitions | data-analytics |

`RECOMMENDATION`: start Stage 1 with fewer, coarser services (e.g.
`wallet`, `game-gateway`, `identity+compliance`, `backoffice-api` as one
deployable each) and split further only when a real scaling or
ownership need appears — per Blueprint's own bias toward not
over-building ahead of proven need (§13).

## Contract discipline

- Every cross-service call is a versioned, documented API (internal or
  external) — never a shared database table written by two services.
- Only `wallet` may write ledger tables. Every other service that needs to
  move money calls `wallet`'s API.
- Only `audit`'s shared library appends to the audit store; services call
  it, they don't implement their own audit writer.
- `tenant-config` is the single source of truth for what a brand looks
  like; no service caches tenant config longer than its documented TTL.

## What is explicitly NOT a separate service (yet)

Per `CLAUDE.md`'s scope-expansion test, the following stay inside an
existing service until a real need forces a split: affiliate tracking
in-house rebuild (Blueprint §1 table — third-party first, in-house is a
year-two consideration), CRM/campaign journey builder (buy first), and any
per-jurisdiction reporting engine beyond a pluggable export interface.

## Open question

Whether `identity` and `compliance` should be one service or two is
deferred to Stage 2 design, when the actual KYC vendor contract shape
(Blueprint §10 Q2 dependency) is known.

## Cross-domain boundary verification (Stage 4G-FINAL)

Still one deployable internally organized by package (ADR 0010 - this
document remains a target proposal, not yet the built shape). Stage
4G-FINAL's own directive required verifying that Casino, Payments,
Financial/Ledger, RG, Identity, KYC, and Risk remain separate domains
with explicit contracts, none silently duplicating another's authority.
Verified as of this stage:

| Domain | Package | Sole authority for | Never does |
|---|---|---|---|
| Financial/Ledger | `internal/ledger`, `internal/wallet` | Posting money, balance projections, idempotency | Business/eligibility decisions about WHETHER to post |
| Identity | `internal/identity` | Person/PlayerAccount/StaffUser/Tenant/Brand records | Auth session mechanics (that's `internal/auth`), risk/RG decisions |
| KYC | `internal/kyc` | Verification/document lifecycle and evidence | Blocking play/withdrawal directly (RG/withdrawal policy enforce, KYC only reports verification state) |
| Responsible Gaming | `internal/rg` | Self-exclusion/RG eligibility (`EvaluateEligibility`) | Generic risk/limit policy - has no rule/threshold concept beyond RG's own restrictions |
| Risk Management | `internal/risk` | Configurable risk/limit policy (`Evaluate`) | Self-exclusion - has no player-status concept beyond what a caller passes in as request fields |
| Casino | `internal/casino` | Game gateway, launch, bet/win/rollback callbacks | Ledger schema, RG/Risk rule logic - calls their interfaces, never reimplements them |
| Payments | `internal/payments`, `internal/withdrawal` | PSP orchestration, deposit/withdrawal state machine | Ledger schema (calls `ledger.Post` like every other domain) |

Confirmed by inspection (not merely by convention): `internal/risk` has
zero imports of `internal/rg` and vice versa. `internal/httpserver`
imports both (`rg_handlers.go`, `risk_handlers.go` - separate files, each
its own domain's admin API), but `internal/casino/orchestrator.go` is the
only file that COMPOSES both decisions on a single operation's path
(`rg.EvaluateEligibility` then `risk.Evaluate`, in that fixed order, RG
always short-circuiting Risk on denial - ADR 0031 §1; corrected from an
earlier, imprecise "only caller that imports both" wording per
documentation review).

`internal/casino`'s one new Stage-4G-FINAL dependency,
`internal/identity.GetTenantByID` (resolving `LicensingMode` for a
`RiskRequest`), is a plain read of a platform-registry table Identity
already owns (`tenants`, no RLS) - it does not cross a domain's WRITE
authority, and was filed through `integration-protocol.md`'s
dependency-request procedure (see `task-registry.md`'s Dependency Request
Log, `DR-4GF-03` - corrected from an earlier version of this paragraph
that miscited `DR-4GF-01`/`DR-4GF-02`, which cover the risk/casino
dependencies, not this identity one).

No ambiguous boundary was found this stage. The one PRE-EXISTING
observation worth naming explicitly (not a defect, already noted in ADR
0031 §8): `internal/risk`'s generic `Rule` shape COULD, in principle, be
misused to express an RG-shaped "block this player entirely" rule (e.g.
a player-scoped `HARD_LIMIT` with a zero `min_amount` threshold on every
operation) - RBAC already prevents this in practice (`risk_config:manage`
and RG's own staff permissions are held by different roles), but nothing
in `internal/risk` itself rejects such a rule at the type level. Recorded
here as a documentation note, consistent with ADR 0031's own treatment -
not fixed, since it would require either a policy-level convention (staff
guidance: "risk rules govern limits, not identity-based prohibition") or
a schema-level restriction neither this stage's directive nor any
specialist review this stage flagged as urgent.
