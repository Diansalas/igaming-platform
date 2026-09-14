# ADR 0024 — Stage 3D Withdrawal Governance Final Gate

Status: `IMPLEMENTED` (mechanisms below), business rule stated verbatim
in §1. Issued at the close of Stage 3D, a tightly-scoped governance-
hardening pass explicitly authorized as "NOT a new financial architecture
stage" - it closes Stage 3C's own documented residual gap (self-approval
enforcement was `PARTIALLY IMPLEMENTED` because `staff_users.person_id`
linkage was optional and unenforced) per an explicit human business
decision, and separates withdrawal-approval authority from staff-
management authority at the RBAC level.

## Context

Stage 3C (ADR 0023 §1) made self-approval enforcement authoritative at
the database layer for any staff/player pair where `staff_users.
person_id` linkage happened to exist - but that linkage was optional,
admin-asserted at staff creation, unverified, and had no path to add it
later. A staff account created without one (the default for every
account, before and after migration `0029`) was invisible to the check
entirely. Stage 3C's own specialist review additionally found that
`RoleTenantAdmin` - a broad administrative role holding `PermStaffManage`
- also held `PermWithdrawalApprove`, meaning a tenant admin could mint an
unlinked "finance" staff account via their own staff-management authority
and approve through it, with no linkage check ever firing. Stage 3D
closes both gaps under an explicit, human-approved business decision.

## The approved business decision (verbatim)

> Withdrawal approval requires attributable Person identity and
> approver/beneficiary separation.

Specifically:

1. Any staff identity that can approve, reject, or submit withdrawals
   MUST be linked to a Person identity.
2. Withdrawal approval must always be attributable to a real Person.
3. A withdrawal approver must never be the same Person as the
   beneficiary/player requesting the withdrawal.
4. Staff-management authority and withdrawal-approval authority must be
   permission-separated.
5. A broad administrative role must NOT implicitly grant withdrawal
   approval authority unless the explicit withdrawal-approval permission
   is present.
6. Staff accounts without a verified Person linkage must NOT be eligible
   for withdrawal approval.
7. No second identity model - reuse the existing Person/StaffUser/
   PlayerAccount identity architecture.

## Decisions

### 1. Mandatory Person linkage, enforced fail-closed at both layers

Migration `0034` replaces the narrower `withdrawal_approvals_deny_self_
approval` trigger (Stage 3C, self-approval only) with `withdrawal_
approvals_enforce_governance`, which fires on every `INSERT` into
`withdrawal_approvals` for a non-automated decision (`is_automated_
approval = false`) and requires the approver to resolve to a
`staff_users` row with a non-`NULL` `person_id` AND `status = 'active'`
- raising an exception otherwise. This covers both `approve` and `reject`
decisions (the two transitions that insert into `withdrawal_approvals`);
the self-approval equality check itself remains scoped to `approve` only
(rejecting your own withdrawal is not a self-dealing bypass the way
approving is). Automated/service decisions (ADR `0014`) are exempt
entirely - a service identity is never a Person, and requiring one would
incorrectly block a legitimate below-threshold automated approval.

`internal/withdrawal.Approve`/`Reject` additionally take a mandatory
`ApproverEligibility` closure - Go-level defense-in-depth ahead of the
trigger, deliberately stricter than the existing `BeneficiaryCheck`
pattern it mirrors: a `nil` value for a non-automated call is itself a
fail-closed `ErrInvalidInput`, never a silent skip. `newSubmitWithdrawal
Handler`/`newResolveWithdrawalHandler` carry an identical explicit check
directly in their own handler bodies, since neither transition inserts
into `withdrawal_approvals` and so has no database trigger backstop -
this Go-level check is their only enforcement point.

### 2. Append-only `person_id` - remediable, never launderable

`staff_users.person_id` gained a `BEFORE UPDATE` trigger (`staff_users_
person_id_append_only`, migration `0034`): a `NULL` → a value transition
is allowed (the sanctioned remediation path for a legacy unlinked
account, via the new `POST /v1/admin/tenants/{tenantID}/staff/{staffID}/
person-link` endpoint, `PermStaffManage`-gated), but a value → a
DIFFERENT value is permanently refused, at the database layer, regardless
of what application code attempts. This directly satisfies business
decision item 7 (no second identity model - a plain append-only column on
the existing `staff_users` table) while closing the "approver relinks
their own account to bypass the rule" attack (adversarial test item G).

### 3. RBAC separation: four withdrawal permissions, one grantee role

`PermWithdrawalApprove` (Stage 3B, one permission gating review/approve/
reject/submit indiscriminately) is split into `PermWithdrawalReview`,
`PermWithdrawalApprove`, `PermWithdrawalReject`, `PermWithdrawalSubmit`.
`RoleTenantAdmin` - which holds `PermStaffManage` - loses ALL FOUR
withdrawal permissions entirely (previously held `PermWithdrawalApprove`
alongside `PermStaffManage`, the exact privilege-escalation vector Stage
3C's specialist review identified). `RoleFinance` is now the sole
grantee of all four, and does not hold `PermStaffManage`. This satisfies
business decision items 4 and 5 structurally, at the role-definition
level - not merely via the person-linkage check, which is a second,
independent layer closing the same class of risk from a different angle
(even if a future role accidentally regained a withdrawal permission
alongside `PermStaffManage`, the linkage/self-approval checks would still
hold).

A new `PermWithdrawalPolicyWrite` permission gates the withdrawal-policy
admin API (§4) and is granted ONLY to `RoleTenantAdmin` - deliberately
disjoint from the four withdrawal-decision permissions, so `RoleFinance`
(which approves withdrawals) can never also loosen the policy gating its
own approvals. This resolves `withdrawal-state-machine.md` §5 bypass #3
and `withdrawal-policy-configuration.md` §5 item 3, both previously open.

### 4. Minimal withdrawal-policy admin API

`docs/architecture/withdrawal-policy-configuration.md` §5 item 4 ("no
admin API exists yet") is closed by `internal/httpserver/withdrawal_
policy_handlers.go`: `GET`/`POST`/`DELETE /v1/admin/withdrawal-policies`,
gated by `PermWithdrawalPolicyWrite` and tenant-scoped via RLS. `POST`
is insert-only (a new versioned row, never an `UPDATE`) and never accepts
a client-supplied `jurisdiction_code` or `required_approver_roles` -
migration `0033`'s own `CHECK` constraints require both `NULL` today,
since neither is actually resolved/enforced yet, and this API must not
let an admin write a value that would silently never take effect.
`DELETE` removes a misconfigured row; this is safe because `threshold_
amount_at_decision`/`request_amount_at_decision` already snapshot the
policy onto each `withdrawal_approvals` row at decision time, so removing
a `withdrawal_policies` row can only change what a FUTURE decision
resolves to, never rewrite a past one. This is deliberately the minimal
boundary the Stage 3D directive required - explicitly not a Back Office
UI or Partner Console.

### 5. Staff-management vs. withdrawal-approval authority - separation is structural, not just a linkage check

Business decision item 4's "permission-separated" requirement is
satisfied on two independent axes: (a) the RBAC role/permission split
(§3) means no role can both manage staff and decide withdrawals, and (b)
even if it could, the mandatory-linkage/active-status/self-approval
checks (§1) would still independently prevent that role from laundering
approval authority through a newly created or modified staff account -
`LinkStaffPersonID`'s `WHERE person_id IS NULL` guard plus the append-only
trigger (§2) mean `PermStaffManage` alone can never grant, revoke, or
redirect an existing withdrawal-eligible identity.

## Specialist review findings and fixes

Seven specialists ran in parallel against the full Stage 3D diff:
`security`, `ledger-finance`, `payments`, `architect`, `backend`, `qa`,
`code-reviewer`. Two independently-converged findings blocked sign-off
and were fixed before this stage was considered complete:

1. **P0 (code-reviewer, confirmed against `security`'s adjacent finding) -
   migration `0034`'s first draft exempted EVERY `is_automated_approval =
   true` row from every check, including the self-approval equality
   check** - reopening the exact bypass migration `0033` deliberately
   closed (0033's own doc comment: "a future caller that mislabels a
   staff principal as automated would have disabled the DB-level backstop
   by that label alone"). A real staff member could have self-approved
   their own withdrawal simply by setting that one client-supplied
   boolean. **Fixed**: the exemption is now decided by whether a
   `staff_users` row actually resolves for the principal, never by the
   flag alone - a genuine service identity (ADR `0014`) has no
   `staff_users` row at all, so "no row resolved" is what correctly
   identifies it; a REAL staff row is held to every rule (linkage,
   active status, self-approval) regardless of what
   `is_automated_approval` claims. Regression tests:
   `TestWithdrawalApprovalsGovernance_AutomatedFlagCannotBypassSelfApproval`,
   `..._AutomatedFlagCannotBypassLinkageCheck`.
2. **P1 (security, ledger-finance, architect, independently) - removing
   withdrawal permissions from `RoleTenantAdmin`'s own role definition
   does not, by itself, achieve business decision #4/#5.** A tenant_admin
   retaining `PermStaffManage` could mint a BRAND NEW `finance`-role
   staff account - choosing its password and an existing `person_id` of
   their choice - and immediately log in as it, self-escalating from
   "can manage staff" to "can approve withdrawals." This is the exact
   attack directive item 2 names: "staff admin must not be able to
   silently grant withdrawal approval via account creation." **Fixed**:
   `newCreateStaffHandler` (`internal/httpserver/admin_routes.go`) now
   refuses `role = "finance"` from any tenant-scoped caller outright;
   only a platform-scoped caller (`platform_admin`) may provision a
   tenant's finance staff. This closes the path at the one point that
   actually matters (who can mint the credential), not merely at the
   role-definition level - the role-definition change (§3) remains
   necessary (it is what stops a tenant_admin from acting AS tenant_admin
   on a withdrawal) but was not, on its own, sufficient. Regression
   tests: `TestCreateStaff_TenantAdminCannotCreateFinanceRole` (and its
   positive control, `TestCreateStaff_PlatformAdminCanCreateFinanceRole`,
   proving the restriction doesn't also break legitimate provisioning).

Additional P2 findings, also fixed:

3. **"Verified Person linkage" overclaimed KYC-style verification that
   doesn't exist** (security, architect). The business decision's own
   wording uses "verified"; this ADR's interpretation (see the boxed note
   in migration `0034`'s own header comment) is that "linked"/"confirmed"
   - a `staff_users.person_id` association established by an admin action
   - is what the mechanism actually delivers, not identity/KYC
   verification of the Person, which this platform does not perform for
   staff. Every operational error message and doc comment was reworded
   from "verified" to "confirmed" to avoid the overclaim (CLAUDE.md's "no
   fake completion" rule); the business-decision quote in §1 above is left
   verbatim since it is a quote, not this codebase's own claim.
4. **Policy deletion's audit record carried no before-image or reason
   code** (ledger-finance, security) - a deleted `require_step_up = true`
   row's actual control impact was unreconstructable from the audit trail
   alone. **Fixed**: `DELETE` now requires a `?reason_code=` query
   parameter and captures the row's own values via `DELETE ... RETURNING`
   into the audit entry's `Metadata` (a before-image).
5. **Backdated `effective_from` accepted on policy creation**
   (ledger-finance) - an admin could assert, after the fact, that a more
   permissive or more restrictive policy was in force at a past instant,
   undermining the table's own "insert-only history" design. **Fixed**:
   the admin API now rejects any `effective_from` before the request's
   own time; a scheduled FUTURE `effective_from` remains accepted (that
   is the intended use of the field).
6. **No database-level guard against `UPDATE` on `withdrawal_policies`**
   (ledger-finance, security) - the "insert-only" design was documentation
   and admin-API convention only, not enforced the way `audit_log` and
   `withdrawal_approvals` are. **Fixed**: migration `0034` adds a
   `BEFORE UPDATE` deny-mutation trigger (`withdrawal_policies_deny_
   update`), matching the existing pattern; `DELETE` remains allowed
   (the admin API's own removal endpoint depends on it, and it is safe -
   see §4).
7. **Adversarial test item G ("approver attempts to modify own linkage")
   was only tested from a third party's perspective**, never the linked
   staff member's own token against their own record (qa). **Fixed**:
   added `TestStaffPersonLink_ApproverCannotRelinkOwnAccountViaAPI`.
8. **No test for a not-yet-effective ("inactive") policy row, or for
   policy-deletion audit-trail integrity after a real decision was
   recorded under it** (qa, directive item 5's own listed scenarios).
   **Fixed**: `TestResolveApprovalPolicy_NotYetEffectivePolicyIsIgnored`,
   `TestWithdrawalPolicyAdmin_DeletionDoesNotRetroactivelyAlterPastDecision`.

## Known limitations / open items (not closed by this stage)

1. **No real production withdrawal approval threshold, required-
   approver-role rule, or step-up requirement is decided** - unchanged
   from Stage 3C; the admin API (§4) is a configuration boundary, not a
   business decision.
2. **A TOCTOU window exists between the Go-level eligibility check and
   the side effect it gates, for submit and resolve specifically**
   (identified by this stage's own `payments` specialist review): `
   approverEligibilityCheck`'s `SELECT` takes no row lock, and a
   concurrent staff-suspension committing between that read and the
   transaction's commit is not guaranteed to be observed. Approve/Reject
   are unaffected (the database trigger re-verifies authoritatively at
   `INSERT` time, inside the same transaction, immediately before commit);
   submit/resolve have no such backstop. **Not fixed this stage** -
   closing it requires either a `SELECT ... FOR SHARE` on the resolved
   `staff_users` row held through the transaction, or a second eligibility
   check immediately before `MarkSubmitted`/`Complete`/`Fail` commit;
   left as a defect to close in a future pass rather than expanding this
   stage's scope further, since no HTTP caller today can trigger the
   specific race window (it requires a concurrent, independent admin
   action against the SAME staff account mid-request) and the blast
   radius is a single already-approved payout, not a new approval.
3. **`is_automated_approval` remains a self-asserted boolean** with no
   registry of valid service-principal IDs and no FK from
   `approver_principal_id` to anything (security) - unreachable via HTTP
   today (`internal/withdrawal.Approve`'s only caller,
   `newApproveWithdrawalHandler`, always passes `isAutomated=false`), but
   a future automated caller must be introduced carefully. Not expanded
   in this stage per CLAUDE.md's scope-creep rule.
4. **MFA/step-up remains `NOT IMPLEMENTED`** (ADR `0017` unchanged) -
   this stage did not touch `ApprovalPolicy.RequireStepUp`/
   `ErrStepUpRequired`.
5. **Jurisdiction-scoped and approver-role-scoped policy rows remain
   schema-ready but functionally inert** - unchanged from Stage 3C.
6. **A rolling per-player structuring check (splitting one large payout
   into several sub-threshold requests) remains unimplemented** -
   unchanged from Stage 3B/3C, still an `OPEN DECISION` owned by
   `identity-compliance`.
