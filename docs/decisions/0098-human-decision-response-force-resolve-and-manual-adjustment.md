# 0098 — Human Decision Response: force-resolve authority (HD-0095-1) and manual adjustments (LEDGER-MANUAL-ADJ-4EYES-1)

**Status: HUMAN DECISIONS RECORDED (2026-09-28).** Source: the human owner's instruction "MASTER
ORCHESTRATOR — NEXT PROVIDER-INDEPENDENT HARDENING ROUND AND CONTINUOUS HANDOVER READINESS",
sections 3–5. This record reproduces the decisions faithfully. It does not add a threshold, a role name
or a jurisdiction rule the human did not give.

Recording a decision is not by itself an implementation. The design is produced and reviewed at the
PRH-2 planning gate (`docs/plans/prh2-hardening-round/plan.md`), and implementation needs that gate's
authorization.

---

## 1. HD-0095-1 — force-resolution of payouts and disputed payments

**Original question (ADR 0095, registry HD-0095-1):** who may force-resolve an unresolvable payout or
a disputed payment attempt, and above what threshold (manual transitions M1/M2)? M1/M2 were BLOCKED
until decided.

**HUMAN ANSWER (decided):**
- Force-resolution of payouts and disputed payments is supported as a **configurable authorization
  capability**. It is not hard-coded to one fixed role or one employee.
- Appropriately authorized **platform/company administrators** can grant or revoke the capability for
  users within their authorized platform scope.
- Appropriately authorized **tenant administrators** can grant or revoke the capability for users within
  their authorized tenant/company scope.
- The platform must support:
  - capability assignment and revocation;
  - user-level authorization;
  - platform-level and tenant-level scope;
  - complete auditability;
  - actor/subject separation;
  - deterministic financial validation;
  - tenant isolation;
  - idempotency;
  - separation of duties where configured or required.
- The capability does **not** let an administrator bypass the deterministic financial engine. The
  financial layer still enforces:
  - valid transaction state;
  - financial invariants;
  - tenant isolation;
  - authorization;
  - idempotency;
  - audit;
  - that the operation is permitted;
  - required approvals;
  - separation of duties where configured.
- Scope limits:
  - No user may grant themselves a capability they do not already have authority to administer.
  - A tenant administrator may not grant permissions outside their tenant scope.
  - A tenant administrator may not grant platform-level authority.
- **No fixed monetary threshold is to be invented.** If an amount threshold is technically necessary or
  legally required, it is a separate human decision. The platform should support configurable
  thresholds and policies where jurisdictions or operators require them, not one hard-coded universal
  value.

## 2. LEDGER-MANUAL-ADJ-4EYES-1 — manual financial adjustments

**Original item (registry):** a four-eyes manual adjustment / mismatch resolution API. It blocks
real-money go-live, and the threshold was not decided.

**HUMAN ANSWER (decided):**
- Manual financial adjustments are supported as a **configurable capability**, not hard-coded to one
  fixed employee or one fixed role.
- Appropriately authorized platform/company administrators can grant or revoke the manual-adjustment
  capability within their authorized platform scope. Appropriately authorized tenant administrators can
  grant or revoke it within their authorized tenant scope.
- **Four-eyes approval is configurable.** The platform must support:
  - enabling and disabling the capability;
  - a configurable four-eyes requirement;
  - an independent second approval;
  - actor/approver separation;
  - complete auditability;
  - tenant isolation;
  - deterministic ledger validation;
  - idempotency;
  - **no direct balance mutation.**
- When four-eyes is enabled:
  - the initiator cannot approve their own adjustment;
  - the second approver must be an independent authorized actor;
  - the approval is auditable;
  - the approval state is deterministic;
  - the ledger executes only the permitted final state.
- **No universal monetary threshold is to be invented.** Where a jurisdiction or operator requires
  thresholds, the architecture supports configurable thresholds and policies. If a specific threshold is
  required before implementation, it is a separate human decision.

## 3. Interpretation that binds both decisions (human's section 5)

- The goal is **not** "give every administrator unlimited financial power".
- The goal is a configurable authorization and governance framework: authorized platform and tenant
  administrators decide which users hold which capabilities. The deterministic financial engine keeps
  enforcing the immutable financial, security, audit, tenant-isolation and approval invariants.
- The existing RBAC/permission architecture is the foundation. No uncontrolled secondary authorization
  system is created where the existing one can be extended safely. If an extension is necessary, it is
  documented as an architecture decision.

## 4. Consequences recorded

- ADR 0095 manual transitions M1/M2 (force-resolve) move from **BLOCKED on a human decision** to
  **DECIDED; design pending** (PRH-2 plan).
- The ledger manual-adjustment capability moves from **BLOCKED on a human decision** to
  **DECIDED; design pending** (PRH-2 plan). The CLAUDE.md rule still applies: "Manual balance
  adjustments require a reason code and four-eyes approval above a configurable threshold." The
  decision makes that threshold configuration data, not a platform constant.
- Separately, the human decided that jurisdiction-specific requirements are represented as explicit,
  versioned, auditable configuration, never as global invariants (instruction section 2). Both
  capabilities follow that principle.

**LEGAL / COMPLIANCE REVIEW REQUIRED:** yes, for any jurisdiction- or operator-specific threshold or
approval rule when configured. No such value is set by this decision.

---

## 5. PRH-2 planning-gate decisions (human, 2026-09-28)

Source: the human's instruction "MASTER ORCHESTRATOR — PRH-2 PLANNING GATE AUTHORIZATION", reviewing
`docs/plans/prh2-hardening-round/plan.md` at `0ea367d`. The questions are defined in plan §7.

| ID | Decision (faithful summary) |
|---|---|
| **HD-PRH2-2** | **Option (c), platform co-approval.** Tenant administrators may manage and request financial capabilities within their tenant scope, but **granting a financial capability requires platform-level co-approval**. Covered: force-resolve, manual adjustment, and future financial capabilities where appropriate. The purpose is to prevent one human from controlling several accounts as supposedly independent financial actors. Required: user-level assignment and revocation; tenant and platform scope; auditability; actor/subject separation; independent approval; real-person identity linkage where available. Prohibited: self-grant, a tenant admin granting platform-level authority, a tenant admin granting outside its tenant, and hard-coding the capability to one role or employee. The model stays extensible for future jurisdictions and operators. |
| **HD-PRH2-7** | **Only platform-level administrators may write or loosen the platform financial approval policy.** Tenant administrators may tighten it for their tenant, and brand-level configuration may tighten tenant/platform policy; neither may ever weaken a platform-mandated control. Every policy change is authorized, audited, versioned/effective-dated where applicable, tenant-scoped, and historically reproducible. |
| **HD-PRH2-1** | **No unrestricted switch that disables mandatory four-eyes controls, and no CLAUDE.md amendment creating a general bypass.** Four-eyes stays mandatory for operations the platform's standing financial-control policy classifies as requiring independent approval. Configurable thresholds and policy profiles are allowed; threshold values are not invented. An operation that genuinely does not belong to the mandatory class may be represented explicitly as such. Jurisdiction- and operator-specific requirements are configurable and may tighten the platform baseline, never silently weaken a mandatory platform financial invariant. |
| **HD-PRH2-6** | **Yes, platform staff may be granted tenant-scoped financial capabilities for tenants under their own licence, but never by virtue of being platform staff alone.** Required: an explicit tenant-scoped permission, authorization, the applicable tenant/platform financial policy, auditability, actor/subject separation, the required four-eyes controls, and tenant isolation. No implicit access to every tenant ledger. The same framework applies to tenants under the platform's licence and under their own. Employment by the platform is not financial authority over a tenant. |
| **HD-PRH2-3** | **Confirmed:** no invented monetary thresholds this round. Build a configurable threshold/policy mechanism able to vary by operation, tenant, jurisdiction, policy profile, and currency/asset where appropriate. No universal hard-coded value. Any unavoidable threshold becomes a separate human decision. |
| **HD-PRH2-4** | **No invented people, emails, phone numbers or on-call personnel.** Build the provider-neutral alerting architecture: severity, durable alert state, tenant/platform scope, recipient/routing configuration, delivery state, retry, escalation state, auditability, idempotency/deduplication. Routing is configurable, with no fictional recipients populated. Real recipients and on-call arrangements for production are a future human/operational decision. The platform must be technically ready to route once contacts are configured. |
| **HD-PRH2-5** | **Tenant users see the identifiable platform staff actor** for auditable platform actions within their tenant, subject to applicable privacy/legal requirements; the actor is not pseudonymized by default. The underlying record preserves actor, subject, tenant, operation, timestamp, approval chain, reason/context and relevant before/after state. A different presentation or privacy treatment required by a jurisdiction is supported through policy/presentation configuration, without destroying the underlying accountability record. |

**Orchestrator interpretation notes** (engineering, reversible; recorded so they can be challenged):
- **HD-PRH2-5 vs security S-10(6).** The actor's identity (staff id and display name) is shown to the tenant. Network metadata (IP, user agent) and free-form metadata are not part of "the actor". They stay out of the tenant **presentation** by default, as a privacy-by-default presentation setting that jurisdiction/tenant presentation policy can change. They remain in the underlying record. This keeps accountability without exposing platform staff's network details.
- **HD-PRH2-1 and the threshold ceiling.** The human did not set a ceiling. Combined with HD-PRH2-7, only platform administrators can set or loosen thresholds, so an effectively unbounded threshold would be a deliberate platform-level act that is audited and effective-dated. No value is invented.
- **HD-PRH2-6** requires the "platform principal acting in tenant X" RLS family (security S-3). It is designed in ADR 0099/0100 and reviewed by security before any K code.
