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
