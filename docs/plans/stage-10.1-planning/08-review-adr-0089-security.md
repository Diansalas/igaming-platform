> Stage 10.1 planning gate — specialist working paper (verbatim, recorded 2026-09-25 against 56f5135). Where it differs from the Orchestrator rulings in `docs/plans/stage-10.1-planning-gate-proposal.md` §O, the rulings govern.

# Security review: ADR 0089 (future AI agent boundary)

Reviewer: `security`. Review only. The ADR was not edited. Checked at the working tree against
`internal/auth/{permission,jwt,middleware}.go`, `internal/audit/audit.go`,
`internal/bonus/{suggestion,suggestion_lifecycle,four_eyes_ops}.go`, `internal/httpserver/ratelimit.go`,
migrations 0056 and 0063, `docs/security/security-architecture.md` §W15.1 and §W15.4.3, and doc 29 §5.4.
Scope: design-level review only. No implementation exists to test. This review does not declare any future agent design secure.

## Verdict: ACCEPT WITH CHANGES

The core principle is sound: agents only propose, deterministic services decide, and no agent ever holds execute rights.
The ADR reuses the correct seam (`BonusSuggestion`) and correctly identifies that `audit.Entry` has a single actor.
But four statements rely on mechanisms that either do not exist or work differently from what the ADR says.
If those statements are left as written, a future stage could treat them as already solved.

## Findings

**S-1 (HIGH): the RBAC model cannot express "agent grant ∩ delegating staff".**
- Permissions come only from roles. `RequirePermission` checks `RoleHasPermission(Role(tc.Role), perm)`, and `Claims` holds exactly one `Role`.
- There is no per-principal grant and no way to intersect two principals' permissions.
- `bonus_suggestion:create` is deliberately held by no role (§W15.4.3, constraint 1). So today no token of any kind can actually hold it.
- The §3 intersection rule also contradicts itself. The delegating staff member (for example `promotions_manager`) never holds `bonus_suggestion:create`. The intersection is therefore empty for PROPOSE, which pushes a future implementer toward a union.
- Failure scenario: an implementer "fixes" the empty intersection by adding `bonus_suggestion:create` to a staff role. That breaks the §W15.4.3 constraint-1 test, or quietly lets a human create model-origin suggestions.
- §3 "RBAC" row, replace the Binding rule with:
  > Agents get permissions only from a dedicated agent grant set that is disjoint from every staff role. It never comes from a staff role, a wildcard or inheritance. Today RBAC is single-role-per-token (`RoleHasPermission`), so no mechanism exists yet for per-agent grants or for intersecting with a delegating principal. This is a **future prerequisite** that needs `security` design.
- §3 "Actor/subject separation" row, replace "The delegating staff member's permissions bound the agent (intersection, never union)" with:
  > For READ tools, the effective grant is the agent grant intersected with the delegating principal's read permissions. For PROPOSE, `bonus_suggestion:create` is agent-only and never granted to a human role, and the delegating principal must hold `bonus_suggestion:review` in the same tenant. Delegation never outlives, and is revoked together with, the delegating staff session or account.

**S-2 (HIGH): §2.3 is an incomplete deny-list and should be an allow-list.**
- The list leaves out several existing permissions that would let an agent write authoritative state directly, skipping the inert-proposal seam:
  - `bonus_suggestion:review`: an agent could approve its own or another agent's suggestions. It is also the permission that allows manual-origin creation.
  - `bonus_grant:review`, `bonus_grant:cancel`.
  - `bonus_campaign:create`, `bonus_campaign:update`, `bonus_campaign:suspend`.
  - `bonus_offer:manage`, `bonus_segment:manage`.
  - `withdrawal_policy:write`, which is not covered by the `withdrawal:*` prefix.
  - `rg_restriction:read`, which contradicts §5.1's ban on exposing self-exclusion status.
  - `risk_config:manage`, `provider_config:write`, `asset_registry:manage`, `asset_authorization:write`.
  - `casino_*`, `jurisdiction_*`, `operating_market_*`, `tenant_licence:assign`, `sportsbook_*:manage`.
  - `player:suspend`, `tenant:write`, `brand:write`, `identity_review:manage`, `verification:review`.
- A deny-list also fails open for every permission added later.
- §2.3, append:
  > Grants are **allow-list only**. An agent may hold only permissions on an explicit, `security`-reviewed agent-grantable list. Initial candidate list: `bonus:read`, `bonus_config:read`, `bonus_report:read`, `bonus_suggestion:create`. Every other existing or future permission is non-grantable by default. `bonus_suggestion:review` and every campaign/offer/segment/grant mutation permission are never grantable, because agents propose only through inert proposal objects (§5.2).
- §9 I-1, replace with:
  > The agent grant set is a subset of the agent-grantable allow-list, and a test asserts that it is disjoint from the §2.3 deny-list. A new permission is non-grantable unless it is explicitly added to the allow-list.

**S-3 (HIGH): §5.3 "delegating principal cannot approve" is presented as existing, but it is not enforced for suggestions.**
- Change requests do enforce approver ≠ requester (migration 0063, around line 436).
- `DecideSuggestion` has no such check. It never compares `decidedBy` with the claimant, with `originating_staff_actor_id`, or with anyone else.
- `bonus_suggestions` (0056) has no column for a delegating principal on `model`-origin rows.
- §W15.4.3 also states that review is not separately four-eyes-gated.
- Failure scenario: staff member S runs an agent, the agent files a suggestion, and S claims and approves it. The trail looks reviewed, which is exactly the "worse than no control" case in §W15.4.3.
- §5.3 first bullet, append:
  > **Not enforced today:** `DecideSuggestion` has no maker ≠ checker check, and `bonus_suggestions` has no delegating-principal column. Future prerequisite: a `delegating_principal_id` on agent-origin proposals, plus a database-enforced `decided_by <> delegating_principal_id` rule (the pattern from migration 0063), with an adversarial test.
- Add to I-5:
  > enforced by the database, not by application code.

**S-4 (MEDIUM): agent identity should not be satisfiable by a "service subtype", and the name "agent" collides with an existing concept.**
- §W15.4.3 binds `bonus_suggestion:create` to `PrincipalService`/`ActorService`. Rule runners use the same pair. The "at minimum a service-principal subtype with a distinguishable `agent_id`" fallback would leave model-driven and deterministic actors distinguishable only by `Metadata`, which §3 itself rejects.
- "agent" already means the retail agent network (`internal/agentnetwork`, the SEP-1 ancestor-closure resolver in §W15.1.9).
- §3 "Agent identity" row, replace the RECOMMENDATION sentence with:
  > **RECOMMENDATION (not implemented):** a distinct `PrincipalType` and `audit.ActorType` (working name `ai_agent`, not `agent`, to avoid collision with the retail agent network). The distinction must be carried in the token and in the audit actor type, never only in `Metadata`. Amending §W15.4.3 so that `bonus_suggestion:create` names this principal type is part of that future `security` review.

**S-5 (MEDIUM): brand scoping is overstated.**
- RLS is bound to tenant only. There is no brand session setting, and `Claims` has no brand claim.
- Doc 29 §5.4 describes brand as a column that is narrowed in the application, not brand-level RLS.
- §3 "Brand scoping" row, replace the Reuse text with:
  > Brand is a column and query predicate (doc 29 §5.4). RLS enforces tenant only. There is no brand claim in `Claims` and no brand RLS setting. Agent brand restriction is a **future prerequisite** and must be enforced server-side on every tool. Until brand-level RLS exists, it is an application-layer control and must be tested as such.
- I-3: change "ignored or rejected" to "**rejected** (fail closed)".

**S-6 (MEDIUM): the rate-limit statement is factually wrong, and the existing limiter is the wrong model.**
- `internal/httpserver/ratelimit.go` exists. It is an in-process, per-replica, per-IP fixed-window limiter covering unauthenticated credential endpoints only.
- §3 "Rate limits" row, replace the Reuse text with:
  > Only a per-IP, per-replica, in-memory limiter exists, on unauthenticated auth endpoints (`internal/httpserver/ratelimit.go`, Stage 9), plus per-identifier login lockout. It is not reusable for agents. Agent rate limits must be keyed on principal, tenant and tool and must not multiply with replica count. Execution limits (proposal count, population size, monetary ceiling) must be enforced inside the canonical transaction, not in memory. **Future prerequisite.**

**S-7 (MEDIUM): prompt injection also flows outward to reviewers and to the model provider.**
- §6 only covers inbound injection from player data. Two outbound channels are missing:
  - (a) The agent's free text (`reason`, proposed-config labels) is shown to human approvers. Injected text can socially engineer a reviewer, or carry PII the agent read into a record other staff can see.
  - (b) Everything placed in model context is a transfer to an external processor.
- §6, add a bullet:
  > **Outbound injection / exfiltration.** Agent-authored free text is untrusted. It is length-bounded, rendered escaped, and labelled model-generated. The reviewer decides on the structured `proposed_config` and deterministic SIMULATE output, never on the model's narrative. Agent free text must not contain player-level PII. Anything sent to model context is a cross-border data transfer (§8.2). Pseudonymisation keys and mapping tables never leave the platform.

**S-8 (MEDIUM): credential lifecycle and kill switch are missing.**
- §6 "Secrets", append:
  > Agent credentials are short-lived, issued per ADR 0014 from Vault/KMS, per tenant, and individually revocable. A per-tenant and a platform-wide kill switch (configuration row, audited, `security`-owned) disables all agent tool calls fail-closed without a deploy. Model-provider API keys are platform secrets and are never visible to the agent.

**S-9 (LOW): suggestion review events hard-code `ActorStaff`.**
- `ClaimSuggestionForReview` and `DecideSuggestion` hard-code `ActorStaff`. This is fine today. The ADR's I-4 must also cover `SuggestionReviewEvent`, not only `audit_log`.
- I-4, append:
  > This applies equally to `suggestion_review_events` and any domain history table, not only `audit_log`.

## Verified accurate (no change)

- `PrincipalType` is player/staff/service.
- `audit.ActorType` is player/staff/service/system, and `Entry` has a single `ActorType`/`ActorID`.
- `bonus_suggestion:create` is in no role.
- `originating_model_version` is required by a CHECK when `originating_kind = 'model'`.
- `ActivateSuggestionAsSingleGrant` goes through the unmodified four-eyes and SEP-1 path.
- `RequireTenantScope` rejects nil-tenant tokens.
- Change-request approver ≠ requester is enforced by the database.

## Launch-blocking note

Nothing in ADR 0089 is implemented, so nothing blocks launch today. S-1, S-2 and S-3 must be resolved in the ADR text before it is used as a design input. They must be implemented and tested before any agent credential is issued.
