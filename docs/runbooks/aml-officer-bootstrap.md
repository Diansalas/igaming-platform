# Runbook: AML officer bootstrap and quorum (ADR 0116 S3, migration 0140)

Status: the database and Go capability is IMPLEMENTED. There is NO HTTP route, Back Office screen or auth permission
constant for it (ADR 0116 S6, NOT IMPLEMENTED), so the steps below are performed by calling the existing capability
service (`internal/capability`) from an operator tool or a reviewed script. This runbook describes mechanism only. It
sets no legal value and is not a statement that any licence condition is met.

## What the role is

`aml_officer` is a K1 capability (ADR 0099 / 0112 catalogue), not an auth role and not a permission a tenant admin can
set. It is created only through the proof-bound four-eyes capability grant flow. Every AML function (holds, cases, notes,
proposals, approvals) checks in SQL that the acting staff member holds an in-force grant, and then consumes a signed
actor proof bound to the operation and target. Forging session settings grants nothing.

## Owner scope decides who may hold the role

`tenants.expected_licensee` defines the owner scope of a tenant:

| expected_licensee | owner scope | eligible grantee | who requests | who approves |
|---|---|---|---|---|
| `platform` (our licence) | platform | platform staff with role `platform_admin` (acting session in the tenant) | a platform principal | a distinct platform principal |
| `tenant` (BYO licence) | tenant | tenant staff with role `compliance` | a tenant admin of that tenant | a distinct PLATFORM principal (a tenant admin can never self-approve) |

Requester, approver and grantee must be three different Persons (ADR 0112 R-4). A platform grant is bounded by
`acting_grant_max_lifetime` (default 4 hours) and must be re-issued; plan rotation as an operations task.

## Quorum

Only hold PLACEMENT checks the quorum: a new hold cannot be placed unless at least two distinct Persons hold an in-force
`aml_officer` grant of the matching scope on that tenant whose grantee is LIVE-eligible (`aml_officer_quorum`). Lifting a manual
hold, reading, and approving a four-eyes proposal do not check the quorum (they need a live officer; an approval also needs a
second Person). The quorum is counted PER TENANT per owner scope (deviation from the ADR text "per owner scope", disclosed in
migration 0140). Fewer than two live officers means every hold placement is refused with SQLSTATE `AM003`: this is intentional
(fail closed). A grantee is live only while the staff row is active, still in the eligible role and the same Person: demoting or
suspending a grantee removes them from the quorum immediately, without revoking the grant.

## Bootstrap steps (per tenant)

1. Confirm `tenants.expected_licensee` for the tenant and therefore the owner scope.
2. Create (or identify) at least two staff members who are different Persons: tenant scope needs staff with role
   `compliance`; platform scope needs platform staff.
3. For each: `capability.CreateRequest` with capability `aml_officer`, a `valid_until`, and a reason code, as the
   requester in the table above.
4. A distinct approver calls `capability.DecideAndGrant(..., "approve", reason)` with a signed actor proof. Decisions
   are written to the audit log by the existing capability flow.
5. Verify with the officer-side call `aml_hold_list` (returns an empty set on a fresh tenant) and confirm two active
   grants through the capability grant listing. Do not rely on a hold placement as the test.
6. Record the bootstrap in the change record: names of the Persons, request and grant ids, expiry.

## Revocation

`capability.RevokeGrant` takes effect immediately for the next AML call (the SQL re-check reads the grant at call time).
If revocation drops the quorum below two, new holds and approvals are refused with `AM003`; existing holds stay in force
(a lift also needs the quorum).

## Known limits (do not paper over)

- A suspended grantee does not count toward the quorum, but only when the staff row is visible to the calling session's RLS
  (security to probe for a platform grantee seen from a tenant session).
- No alert routing to officers exists yet (ALERT-DELIVERY open). `aml.hold_eval_failed` alert rows are readable by the SUBJECT
  TENANT's staff under the alert read policy (they infer an evaluator fault, not a hold): see ADR 0116 12.4.
- An officer never acts on a record about their own Person (AM011); the queue omits their own subject.
- Platform `aml_officer` grants expire after at most 4 hours; schedule re-grants or the platform quorum lapses.
- Case notes are plaintext; the lint is an accident guard only.
- Erasure of a person with AML records fails closed (`aml_person_has_records`); the erasure job itself (HD-RET-1) is NOT
  IMPLEMENTED.
- Screening, monitoring rules and SAR export are NOT IMPLEMENTED (S2, S4, S5).
