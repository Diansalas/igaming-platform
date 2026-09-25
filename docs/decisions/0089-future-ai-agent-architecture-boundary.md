# ADR 0089 — Future AI Agent Architecture Boundary (Bonus / Gamification / Reward)

- **Status:** **ACCEPTED as a binding future requirement (human-directed
  2026-09-25, Stage 10.1 planning gate) — `NOT IMPLEMENTED`; no code,
  dependency, model provider or infrastructure is authorized by this
  ADR.**
- **Decision type:** future architecture boundary. It records a human
  requirement and constrains any later design. It creates no Stage 10.1
  work item, no permission, no principal type, no route, no schema, and
  reopens no prior decision.
- **Owner:** `architect`. Required co-review before any future
  implementation: `security` (agent identity, tool permissions, prompt
  injection, PII), `ledger-finance` (anything that could reach a
  balance-affecting flow), `identity-compliance` (RG/KYC/jurisdiction),
  `risk`, `bonus-engine`, `qa`.
- **Inputs (verified at HEAD `20c72e4`):** the human directive quoted in
  §1; `CLAUDE.md`; `docs/governance/change-control.md`; docs 10, 17, 21,
  23, 25, 29, 30 under `docs/architecture/`; ADRs 0014, 0017, 0019, 0024,
  0031, 0032, 0033, 0034; `docs/security/security-architecture.md`
  §B1.1, §W15.1 (`SEP-1`), §W15.4.3; `internal/auth/permission.go`,
  `internal/auth/jwt.go`, `internal/audit/audit.go`, `internal/bonus`
  (read only).

Labels (`CLAUDE.md`): every capability named in this ADR is
**`NOT IMPLEMENTED`**. Where this ADR cites an existing surface, its own
status label is stated next to it; nothing here upgrades any label.

## 1. Context — the human requirement (meaning preserved exactly)

At the Stage 10.1 planning gate the human issued the following future
architecture requirement:

> The future platform must support controlled AI agents capable of:
>
> - **READ:** bonus reports; gamification reports; campaign performance;
>   player segmentation; reward history; bonus liability;
>   redemption/expiry; gamification metrics.
> - **PROPOSE:** bonus campaigns; eligibility rules; rewards; wagering
>   requirements; gamification challenges; player journeys; campaign
>   configurations.
> - **SIMULATE:** expected player population; reward cost; bonus
>   liability; exposure; rule outcomes.
> - **EXECUTE:** only through canonical deterministic platform APIs after
>   normal validation and required approval.
>
> **ARCHITECTURAL PRINCIPLE: AI MAY ANALYSE AND PROPOSE. DETERMINISTIC
> CORE SERVICES VALIDATE AND EXECUTE.**
>
> AI agents must **never** become authoritative for: ledger entries;
> wallet balances; financial calculations; jurisdiction; KYC; AML;
> responsible gaming; self-exclusion; risk decisions; payment
> authorization; withdrawal approval; bonus financial liability; tenant
> isolation; RBAC.
>
> Future AI capabilities must use controlled APIs/tools with: agent
> identity; RBAC; tenant scoping; brand scoping; explicit tool
> permissions; read/write separation; approval workflows; audit logging;
> actor/subject separation; idempotency; deterministic validation;
> simulation/dry-run; rate limits; execution limits; model/provider
> version recording; reproducibility; complete action history.
>
> Agents must call the canonical Bonus Engine, Gamification Engine and
> Reward Orchestrator rather than directly modifying their database
> tables.
>
> AI Agent → Agent/Tool API → Read/Propose/Simulate → Validation → Human
> approval where required → Canonical Bonus/Gamification/Reward APIs →
> Ledger/deterministic core.
>
> Keep this as future architecture only. DO NOT implement AI agents
> during Stage 10.1. DO NOT add speculative AI infrastructure. DO NOT add
> model providers. DO NOT add AI dependencies.

**Why an ADR now, with nothing to build.** The requirement constrains the
shape of surfaces that *are* being built (Bonus Engine admin APIs,
suggestions, four-eyes, audit). Recording it now prevents a later stage
from adding an agent "connector" that talks to tables, reuses a staff
token, or treats a model's output as a decision. It also records that the
platform already contains the correct seam for the PROPOSE half (§5.2).

## 2. Decision — the principle, made binding

**AI MAY ANALYSE AND PROPOSE. DETERMINISTIC CORE SERVICES VALIDATE AND
EXECUTE.** Concretely, for any future agent capability:

1. **An agent's output is always a proposal or a read result, never a
   decision.** Every state change it leads to is produced by the same
   deterministic, canonical command a human or service caller would use.
   The agent never gets a shortcut, a bypass flag, or a private code path.
2. **Agents hold no authority in the never-authoritative domains**
   (ledger entries, wallet balances, financial calculations,
   jurisdiction, KYC, AML, RG, self-exclusion, risk decisions, payment
   authorization, withdrawal approval, bonus financial liability, tenant
   isolation, RBAC). In those domains an agent may at most *read* a
   permitted projection. It may never write, override, suppress, or
   pre-decide a result. The authoritative source stays where it is today:
   `internal/ledger`, ADR 0019; `rg.EvaluateEligibility`, ADR 0034 §1;
   `risk.Evaluate`, ADR 0031 §2; ADR 0032 for liability; RLS for tenancy;
   `permission.go` for RBAC.
3. **No agent is ever granted any permission that moves value or decides
   compliance.** This covers, at minimum, `bonus_grant:issue`,
   `bonus_bulk:execute`, `bonus_adjustment:write`,
   `bonus_held_disposition:resolve`, `bonus_campaign:activate`,
   `bonus_approval_policy:write`, every `withdrawal:*` permission,
   `rg_restriction:write`, `staff:manage`, and any permission that changes
   RBAC, jurisdiction or licence registries. The EXECUTE stage is always
   performed by a principal that is not the agent (§5.3).
4. **Agents never touch storage.** No agent credential is a database
   credential, and no agent tool issues SQL. Agents call the canonical
   Bonus Engine, Gamification Engine and Reward Orchestrator APIs (§4),
   which keep their own validation, RLS, idempotency and audit.
5. **Fail closed.** An unknown tool, an unresolvable scope, a missing
   approval, an unrecorded model version or a failed validation denies
   the request (`change-control.md` "Fail closed"; ADR 0031 §6).

## 3. Mandatory controls (binding on any future agent capability)

Each control lists the existing mechanism it must reuse. A future design
may not replace any of these with an agent-specific parallel mechanism.

| Control | Binding rule | Reuse / current state |
|---|---|---|
| **Agent identity** | An agent is a distinct, non-human principal. It is never a staff token, never a player token, and never a shared service credential. | `PrincipalType` today is `player`/`staff`/`service` (`internal/auth/jwt.go`). `audit.ActorType` is `player`/`staff`/`service`/`system`. **RECOMMENDATION (not implemented):** a future `PrincipalAgent` (and a matching `audit.ActorAgent`), or at minimum a service-principal subtype with a distinguishable `agent_id`. An agent must never be indistinguishable in audit from a deterministic rule runner. The choice belongs to the future stage and is `security`-reviewed. |
| **RBAC** | Agents get permissions only through the existing `Permission` model, with no role wildcard and no inheritance from a staff role. | `internal/auth/permission.go`. Adding a principal type or a permission falls under `change-control.md` ("New StaffRole / new RBAC permission": `security` review plus over-/under-grant tests). |
| **Tenant scoping** | `tenant_id` comes from the agent's authenticated credential, never from the prompt, a tool argument or model output. It is enforced by RLS on every canonical call. | `CLAUDE.md` Multi-tenancy; `auth.RequireTenantScope`; ADR 0011 (platform-scoped tokens). A platform-scoped (nil-tenant) agent is **not permitted** for tenant data. |
| **Brand scoping** | An agent credential may be further restricted to a brand set. A brand never widens tenant scope. | Brand-scoped RLS/predicates already used by bonus tables (doc 29 §5.4). |
| **Explicit tool permissions** | Each tool is an allow-listed, typed operation that maps 1:1 onto a canonical API and its existing permission. There is no generic "call any endpoint" or free-form query tool. | New. Registry-as-configuration, versioned (`CLAUDE.md` configuration-row rule). |
| **Read/write separation** | Read tools and propose tools are separate grants. A read-only agent cannot propose, and no agent holds an execute grant (§2.3). | Mirrors `PermBonusRead`/`PermBonusReportRead` vs. mutation permissions. |
| **Approval workflows** | A proposal becomes an action only through the existing approval primitives, and agent origin never counts as an approval. | `BonusSuggestion` review (doc 10 §W6/§N3; security §W15.4.3); bonus four-eyes change requests (`internal/bonus/change_governance.go`, `four_eyes_ops.go`; routes `POST /v1/admin/bonus/change-requests[/{id}/decide]`); withdrawal distinct-approver (ADR 0024); catalogue dual control (ADR 0081 §5); `SEP-1` actor ≠ beneficiary (security §W15.1). Step-up for approvers (ADR 0017): `RequireStepUp` is `NOT IMPLEMENTED`. |
| **Audit logging** | Every agent tool call, including reads of player-level data, writes an audit record in the same transaction as any effect. | `internal/audit.Record` (same-tx). Doc 12 §Audit. |
| **Actor/subject separation** | When an agent acts for a staff principal, **both** are recorded: the agent as acting principal and the staff member as delegating principal. The affected player or population is recorded separately as the subject/beneficiary. The delegating staff member's permissions bound the agent (intersection, never union), and `SEP-1` is evaluated against the delegating human. | `audit.Entry` today carries one `ActorType`/`ActorID`. **Gap (future prerequisite):** a first-class delegating-principal field. `Metadata` is not an acceptable substitute for an authorization-relevant fact. |
| **Idempotency** | Every propose/execute call carries a caller idempotency key. Execution inherits the canonical API's own database-enforced idempotency. | ADR 0020; `EconomicOperationIdentity` (doc 10 §N2.4a); `(provider_id, provider_tx_id)` pattern. |
| **Deterministic validation** | All validation that decides anything is performed by the canonical service. Agent-side checks are advisory only. | T.1 gate order (doc 10 §T.1): `AssetAuthorization` → `rg.EvaluateEligibility` → `risk.Evaluate`. |
| **Simulation / dry-run** | Every SIMULATE result is produced by a deterministic, side-effect-free canonical endpoint, never computed by the model. Results are labelled non-binding. | See §4 (mostly missing). |
| **Rate limits** | Enforced per agent identity, per tenant and per tool, server-side. | No general API rate limiter exists (only auth failed-attempt limiting). **Future prerequisite.** |
| **Execution limits** | Hard caps per agent: proposals per window, population size per proposal, and monetary ceiling per proposed campaign. These are configuration rows, and exceeding them fails closed. | Existing threshold resolution (`ResolveApprovalPolicy`) is the model to follow. |
| **Model/provider version recording** | Every agent-originated artifact records the model identifier, provider and version, plus the agent/tool-registry version. | `bonus_suggestions.originating_model_version` exists (`OriginatingModel` kind, migration 0056): a precedent, not a full solution. |
| **Reproducibility** | Store hashes of the inputs (the read-set, pinned to snapshot identifiers), the prompt/template, the tool-call log and the outputs, with the configuration versions used. | New. Raw prompts containing PII follow the §6 minimisation rules. |
| **Complete action history** | Each agent session can be reconstructed end to end: reads → proposals → simulations → reviews → resulting canonical operations. Linkage is bidirectional. | `SuggestionReviewEvent` (append-only) plus `originating_suggestion_id` on `Grant`/`BulkGrantJob` is the existing pattern to extend. |

## 4. Flow and canonical-surface mapping

```
AI Agent
  → Agent/Tool API        (agent identity, tool allow-list, tenant/brand scope,
                           rate/execution limits, audit, hashing)
  → Read | Propose | Simulate
  → Validation            (canonical deterministic services only)
  → Human approval where required   (existing review / four-eyes / SEP-1)
  → Canonical Bonus / Gamification / Reward Orchestrator APIs
  → Ledger / deterministic core     (ADR 0019 originator matrix unchanged)
```

The Agent/Tool API is a thin, policy-enforcing façade over canonical
APIs. It owns no business rule, no eligibility logic and no calculation.
Under ADR 0019's originator matrix an agent is **not** a new actor class
for ledger purposes: it can never originate a posting of any
`transaction_type`. Postings stay with the existing "internal service"
and "staff principal" rows, reached only via EXECUTE by a non-agent
principal.

### 4.1 Capability → canonical surface

"Exists" means an implemented route or function at HEAD `20c72e4`. Every
"Missing" item is a **future prerequisite**, not a Stage 10.1 work item.

| Capability | Canonical surface | State |
|---|---|---|
| READ campaigns, grants, change requests | `GET /v1/admin/bonus/{campaigns,change-requests,grants}` (`PermBonusRead`) | Exists (Stage 5) |
| READ bonus reports / campaign performance | `PermBonusReportRead` is defined, but no route uses it. Doc 12 §Reporting: never against the transactional ledger; via CDC → ClickHouse | **Missing**: reporting read models |
| READ bonus liability | ADR 0032 §2 `promo_liability` + mirror invariant; ledger projections | **Missing**: a liability read model. The agent may read it and never compute it |
| READ redemption / expiry | Grant states (doc 10 §1.2); expiry sweep (`internal/bonus/expiry_sweep.go`) | Partial: grant list only. **Missing**: aggregated view |
| READ player segmentation | Doc 30 (`Segment`, `SegmentEvaluation`, `Resolve`) | **Missing**: doc 30 is `NOT IMPLEMENTED` |
| READ reward history | Doc 21 (`RewardDecision`), doc 23 | **Missing**: Reward Orchestrator is `NOT IMPLEMENTED` |
| READ gamification reports / metrics | Doc 17 | **Missing**: Gamification is `NOT IMPLEMENTED` |
| PROPOSE campaigns / rewards / wagering / eligibility / configurations | `BonusSuggestion` (`internal/bonus/suggestion.go`, migration 0056): `originating_kind = model`, `proposed_config` inert, `bonus_suggestion:create` held by no human role (security §W15.4.3) | Object and review lifecycle exist; review routes `POST /v1/admin/bonus/suggestions/{id}/{claim,decide}`. **Missing**: an authenticated non-human route for `CreateSuggestion`, and agent identity |
| PROPOSE gamification challenges / player journeys | Doc 17 (missions/challenges); doc 31 §6 (CRM journeys, triggers, steps) and §7 (CRM → Bonus/Gamification/Reward via the canonical grant surface) | **Missing**: both are `NOT IMPLEMENTED`. An agent-proposed journey would be an inert CRM journey draft, reviewed and activated through CRM's own gates (doc 31 §8.2/§8.3), never a new path |
| SIMULATE expected population | Doc 30 `Resolve`/`SegmentEvaluation`; `CheckOfferEligibility` (doc 10 §N2.4, a read-only preview running the T.1 gate, non-binding) | **Missing**: both are design-only |
| SIMULATE reward cost / bonus liability / exposure | ADR 0032 posting map; AOE (doc 10 §N1.3); ADR 0031 limits | **Missing**: no dry-run costing or exposure endpoint exists or is designed. Must be deterministic, integer-minor-unit, per asset, and `ledger-finance`-owned |
| SIMULATE rule outcomes | Doc 30 §5.2 determinism rules; T.1 gate | **Missing**: needs a side-effect-free evaluation endpoint |
| EXECUTE | Suggestion activation → ordinary Campaign/Offer/Grant/`BulkGrantJob` pipeline with its own four-eyes (`ActivateSuggestionAsSingleGrant`; security §W15.4.3 constraint 3) | Exists for Bonus, performed by a **staff** principal. Gamification and Reward Orchestrator execute paths: **Missing** |

## 5. Interaction with existing controls

### 5.1 RG, KYC, jurisdiction

- **RG stays the sole authority** (ADR 0034 §1). An agent never reads
  `player_restrictions` to pre-filter, and never asserts a player is
  eligible. Segmentation and targeting output is always a *proposal*
  evaluated later by deterministic eligibility (doc 10 §T.1/§T.4;
  doc 30 §1.1). A proposal that names a self-excluded player is not an
  error in the agent. It is denied at the gate like any other attempt.
  However, agents **must not be designed to target self-excluded
  players**, and read tools must not expose self-exclusion status as a
  targeting input. RG-restricted players are removed from any population
  the agent sees, by the canonical service rather than by the agent.
- **Mid-lifecycle self-exclusion** follows ADR 0034 §2 unchanged. The
  fact that a grant was originally proposed by an agent does not affect
  how it is handled.
- **KYC gates** (ADR 0034 §4/§6) cannot be bypassed, waived or
  pre-satisfied by an agent proposal.
- **Jurisdiction and licensing mode** are consumed, never invented
  (ADR 0034 §8; ADR 0031 §9/§10). An agent cannot choose or override a
  jurisdiction. A proposal that is invalid for the resolved jurisdiction
  fails validation.
- **Risk** (ADR 0031 §14/§15) evaluates every executed operation exactly
  as it evaluates a human-originated one.

### 5.2 Why the existing suggestion seam is the PROPOSE boundary

Doc 10 §N3.3 already states the structural guarantee this ADR relies on:
a suggestion generator's credential is scoped to `bonus_suggestion:create`
only, so "a **future** AI/CRM/recommendation system ... can only ever
populate `Generated` rows; it structurally cannot move money." This ADR
adopts that seam as the required PROPOSE path for bonus proposals. Future
Gamification and Reward proposals must use an equivalent inert-proposal
object with the same three properties:

1. It has no write path to ledger or wallet.
2. Activation happens only through the ordinary pipeline.
3. The credential cannot hold any execute permission.

### 5.3 Separation of duties

- Agent origin never counts toward any approval count. A proposal from
  an agent acting for staff member S may not be reviewed or approved by
  S (maker ≠ checker, extending security §W15.4.3 constraint 2 and the
  four-eyes self-approval refusal already enforced for change requests).
- `SEP-1` (security §W15.1) is evaluated against every human in the
  chain, including the delegating principal.
- An agent may never mutate the policy that gates its own proposals.
  This is the same rule that keeps `withdrawal_policy:write` and
  `bonus_approval_policy:write` separate from the authorities they gate
  (ADR 0024 §5; `permission.go`).

## 6. Security considerations

- **Prompt injection through player-controlled data.** Nicknames, chat,
  support notes, referral codes and similar values can appear in reports
  an agent reads. Such data must be treated as untrusted content and
  never as instructions. The design must assume injection will succeed
  at the model layer, which is why §2's "no authority" plus tool
  allow-listing plus downstream deterministic validation is the actual
  control. A tool call must never be authorized because the model asked
  for it; it is authorized only because the agent's credential already
  holds the permission.
- **Data minimisation / PII.** Agents receive aggregated or
  pseudonymised data by default. Player-level PII (name, email, document
  data, IP, payment details) is excluded unless a specific tool is
  justified, `security`-reviewed and audited per read. PAN never exists
  in the platform. KYC document content is out of scope. Data residency
  and transfer rules per jurisdiction apply to any model provider (§8).
- **Cross-tenant leakage.** One agent session has exactly one tenant
  scope. There is no shared context, cache, embedding store or
  fine-tuning corpus across tenants unless a future human decision
  explicitly permits it. RLS enforces this on every canonical call, but
  any agent-side memory is outside RLS and must be tenant-partitioned by
  design.
- **Tool-permission escalation.** The tool registry is configuration
  under RBAC. An agent can never modify its own tools, limits, prompts or
  permissions (§5.3). Adding a tool is a `security`-reviewed change.
- **Secrets never in prompts.** Credentials, tokens, provider keys and
  signing material never enter model context, logs or reproducibility
  hashes' pre-images. The same rule already applies to `audit.Entry.Metadata`.
- **Denial of wallet/cost.** Rate and execution limits (§3) also bound
  model spend and approver fatigue: a flood of proposals is itself an
  attack on four-eyes review.

## 7. Non-goals (explicit)

- No AI agent, agent runtime, Agent/Tool API, model client, prompt
  store, vector store or evaluation harness is authorized. None is built
  during Stage 10.1.
- No new dependency, SDK, model provider, or infrastructure (Terraform,
  IAM, network egress) is authorized.
- No `PrincipalAgent`, `ActorAgent`, permission, role, route, migration
  or OpenAPI change is made by this ADR.
- The missing prerequisites in §4.1 (reporting read models, liability
  read model, simulation endpoints, suggestion-create route, delegating
  principal in audit, rate limiter) are **not** scheduled by this ADR.
  Each needs its own justification under `CLAUDE.md`'s scope test.
- No agent authority in any never-authoritative domain, ever. This is
  not a "later phase" item.

## 8. What would trigger implementation

1. **A future, human-authorized stage** whose directive explicitly
   includes agent capabilities, with its own planning gate. Implementing
   it would be an architectural change and a security-relevant change
   under `change-control.md`.
2. **FUTURE HUMAN DECISION (not taken now):** model/provider selection,
   hosting (vendor API vs. self-hosted), data-processing terms,
   cross-border transfer and per-jurisdiction residency, and cost. This
   is commercial/legal, not engineering. It is surfaced to the human at
   that stage's gate and is not decided or pre-selected here.
3. **FUTURE HUMAN / COMPLIANCE DECISION:** whether any jurisdiction or
   licence (Anjouan; later BYOL tenants, ADR 0006) restricts automated
   marketing/targeting decisions or requires disclosure. Legal
   interpretation is escalated, not assumed.
4. **Prerequisites in place first.** The canonical READ/SIMULATE
   surfaces named in §4.1 must exist as deterministic, non-AI endpoints
   with their own value to human staff before any agent may call them.
   Agents consume platform capabilities; they never justify building
   those capabilities agent-first.

## 9. Architectural invariants (for `qa` / `code-reviewer`, at implementation time)

- I-1: No agent credential holds any permission listed in §2.3. A test
  enumerates the agent grant set against a deny-list.
- I-2: No code path reachable from the Agent/Tool API calls
  `internal/ledger`, `internal/wallet`, `rg` write functions, or issues
  SQL against Bonus/Gamification/Reward tables directly.
- I-3: Tenant scope is derived from the credential only. Tool arguments
  carrying `tenant_id`/`brand_id` are ignored or rejected.
- I-4: Every agent tool call produces an audit row that records the
  acting agent, the delegating principal (if any), the model version
  and the input hash.
- I-5: Agent origin never satisfies or counts toward an approval. The
  delegating principal cannot approve their agent's proposal.
- I-6: Every SIMULATE response comes from a side-effect-free canonical
  endpoint and is labelled non-binding. The real execution re-runs the
  full gate.

## 10. Consequences

- **Positive:** the future AI path is fixed to existing seams
  (`BonusSuggestion`, four-eyes, `SEP-1`, T.1 gate, same-transaction
  audit), so no parallel authority can emerge. Current Bonus work needs
  no change to stay compatible. The human's "no speculative AI
  infrastructure" instruction is honoured.
- **Negative / cost:** the future stage inherits real prerequisites
  (§4.1 "Missing"), several of which are substantial (reporting
  pipeline, simulation/costing endpoints, Gamification and Reward
  Orchestrator themselves). Agent value is gated on them.
- **Constraint on current work:** any new Bonus, Gamification or Reward
  mutation must stay reachable through a canonical, permission-gated,
  audited API. No "internal-only" mutation shortcut may be introduced
  that a future agent façade would have to bypass validation to use.
- **Audit model pressure:** single-actor `audit.Entry` is sufficient
  today but not for delegated agent actions. This is recorded as a known
  future prerequisite, not changed now.

## 11. Cross-references

- `CLAUDE.md` (Multi-tenancy, Financial/ledger, Security, Compliance, No
  uncontrolled scope expansion, When to stop and ask)
- `docs/governance/change-control.md`
- `docs/architecture/10-bonus-engine-architecture.md` §T.1, §T.4, §W6,
  §N1.3, §N2.4, §N2.4a, §N3
- `docs/architecture/12-audit-reporting-architecture.md` (Audit;
  Reporting and BI)
- `docs/architecture/17-gamification-engine-architecture.md`
- `docs/architecture/21-reward-orchestration-architecture.md`
- `docs/architecture/23-external-reward-provider-contract.md`
- `docs/architecture/25-bonus-gamification-api-architecture.md` §2
- `docs/architecture/29-bonus-implementation-contract.md` §5, §6
- `docs/architecture/30-segmentation-engine-architecture.md` §1.1, §5
- `docs/architecture/31-crm-engine-architecture.md` §1.1, §6, §7, §8
- `docs/security/security-architecture.md` §B1.1, §W15.1 (`SEP-1`),
  §W15.4.3
- ADR 0006 (hybrid licensing), 0011 (platform-scoped tokens), 0014
  (service identity), 0017 (MFA/step-up), 0019 (ledger originator
  matrix), 0020 (idempotency), 0024 (withdrawal separation of duties),
  0031 (risk engine), 0032 (bonus accounting), 0033 (external bonus
  engines), 0034 (bonus/gamification RG/KYC), 0081 (catalogue dual
  control)
- `internal/auth/permission.go`, `internal/auth/jwt.go`,
  `internal/audit/audit.go`, `internal/bonus/suggestion.go`,
  `internal/bonus/suggestion_lifecycle.go`,
  `internal/bonus/change_governance.go`, `internal/bonus/four_eyes_ops.go`
