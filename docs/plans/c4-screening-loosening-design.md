# c4 design note: a governed path to LOOSEN the AML screening policy

Branch `c4-screening-loosening` (from 4974b0d, migration head 0189, new 0190). Software capability only: no regulatory approval is claimed, the only
screening provider is the MOCK, and nothing here enables a withdrawal screening gate. Every number below that is not quoted from existing code is a
TEST FIXTURE or an OWNER DECISION. Statements are labelled `RECOMMENDATION` where they are not facts of the code.

## 1. What counts as loosening in the actual schema (read from the code, not guessed)

The only machine classifier of "less strict" is the one in `aml_screening_policy_author` (migration 0189, tightened in lic30). Against the LATEST
version (`created_at DESC, id DESC`) of the same tenant and jurisdiction, and also against the version IN FORCE now, a new `aml_screening_policies`
row is refused (`policy_loosening_refused`, 409) when any of these holds:

| Dimension | Loosening | Column |
|---|---|---|
| Lists | a list of the previous version is not in the new `lists` (`NOT (p_lists @> prev.lists)`) | `lists` (subset of `sanctions`, `pep`, one or two) |
| Re-screen interval | `p_rescreen_seconds > prev.rescreen_interval_seconds` | `rescreen_interval_seconds` |
| Backfill grace | `p_grace_seconds > prev.backfill_grace_seconds` | `backfill_grace_seconds` |
| Effective time | earlier than the version in force (or the latest when none is in force) | `effective_from` (bounded to [now - 2 min, now + 30 d]) |

Independently of direction, 0189 refuses an interval below 3600 s (`rescreen_interval_below_floor`, an engineering floor, not a legal value), below twice
the `gate_alert_after_seconds` of the latest GATING trigger version (`rescreen_interval_below_alert_threshold`) and the route caps interval and grace at
31536000 s. These floors are not strictness dimensions; they bound every version.

What is NOT covered by any classifier today (a finding, not a design choice of c4):

- **The trigger policy** (`aml_screening_trigger_policies`: which events screen, `gate_withdrawal`, `gate_alert_after_seconds`). `POST .../screening/trigger-policy`
  is NOT tighten-only. A version with fewer events, or with `gate_withdrawal=false`, is a loosening in effect ("disable a trigger / disable a gate"), and it is
  governed only by: `aml:hold:manage`, step-up `critical`, a platform_acting officer, the owner-scope officer quorum, a signed proof (ADR 0110) and a legal
  review reference. That is a single officer chain, not the two-Person rule that licensing widening uses. Whether it must also become two-Person is OWNER DECISION D4.
- **The unrouted setter** `aml_screening_policy_set` (0154) still loosens with an officer quorum and a proof, and is `EXECUTE`-granted to the runtime role (LIC30 L-3).
- **Disabling the withdrawal gate** is therefore possible today through the trigger-policy route; it is a loosening of enforcement but it can only reduce what is
  held, never trap funds. The dangerous direction (ENABLING the gate) is guarded separately and is not touched by c4 (section 5).

## 2. Options

**(a) Tighten-only forever, plus a documented break-glass owner path.** Nothing is built. A wrongly short value or a regulator-instructed loosening is fixed by a
database operator through the unrouted setter or a direct owner write. Pros: smallest surface, nothing new to attack. Cons: loosening stays an ungoverned operator
action with no two-person control, no cooling period, no structured evidence and an audit trail that depends on the operator; the lic30 backlog item ("a value that
is valid but too SHORT cannot be reverted over HTTP") stays open.

**(b) A governed licensing-change-style request kind (built).** Two new kinds on the 0177 primitive (it already supplies two distinct platform Persons, requester
never decides, 24 h approval freshness, 72 h expiry, a signed proof on request / approve / execute, row locks, audit rows, FORCE RLS):

- `aml_screening_policy_loosen`: always widening; two approvals; executes by writing the next policy version in the transaction of the final approval.
- `aml_screening_loosening_configure`: a per-tenant capability (enabled + cooling period) as append-only versions. Enabling or shortening the cooling period widens
  (two Persons); disabling or lengthening narrows (single platform_admin, at once). So the capability itself cannot be switched on by one person.

The mechanism is INERT until configured: no capability row ships, so every loosening request is refused (`loosening_not_configured`), exactly as loosening was
impossible before. All rules are enforced by the database (a Go bug cannot skip them): configured and enabled, tenant configured for the jurisdiction, a previous
version exists, the request IS a loosening, the 0189 floors and caps, the effective-time window (execution time + an offset of 0..30 days), the cooling period
counted from filing, the production environment guard (section 5). The new row is written by `screening_policy_loosen_apply` and admitted by a table guard only if an
executed request of the same transaction, actor and payload exists (LC040 otherwise). Tenant staff never file, approve or see these requests.

**(c) Other options considered.**
- *c1. Loosen through the existing officer route with a second officer approving.* Reuses the AML four-eyes (`aml_case_propose/approve`), but that machine is bound to
  a case and a hold, not to a configuration row; it would need a new table and state machine, duplicating the licensing-change primitive. Not chosen.
- *c2. Loosening by a new version on the same route plus a `loosening_approval_id`.* Needs the same approval store and puts the policy-authoring route in the middle of
  a maker-checker flow it was deliberately kept out of (SR7-3). Not chosen.
- *c3. A per-jurisdiction rule table of which dimensions may be loosened.* Would require business values (which lists may go) that the owner has not chosen. Recorded
  as OWNER DECISION D3; not built.

## 3. RECOMMENDATION (safe default)

RECOMMENDATION: ship (b) as an INERT mechanism and keep (a)'s break-glass documented as the fallback until the owner decides. Concretely:

1. Loosening is REFUSED unless the governed workflow is configured for that tenant AND the request is fully approved AND the cooling period has elapsed. With no capability row
   (the shipped state) the answer is `loosening_not_configured`.
2. The withdrawal screening gate cannot be enabled by this path regardless; and in a database whose environment anchor is production or unrecorded a loosening is
   refused wherever the jurisdiction's latest trigger policy gates withdrawals. The production-environment guard of the gate (non-production anchor + worker heartbeat)
   is untouched.
3. Do not enable the capability for any tenant until the owner has decided D1 to D3 and `security` has reviewed migration 0190 (BLOCKING per `CLAUDE.md`), and
   `ledger-finance` has reviewed the effect on the payout gate (a longer re-screen interval or grace changes when clears expire and who is held).
4. The unrouted `aml_screening_policy_set` should lose the runtime role's `EXECUTE` once the governed path is accepted (D7), so that loosening has one route (needs a
   migration; not done here: it would change a released function's grants and the owner has not decided).

Approval decisions required (each is a human decision; this record asks, it does not decide):

| Id | Decision | Decided by (RECOMMENDATION) |
|---|---|---|
| D1 | Is governed loosening wanted at all, and for which tenants (option (a) forever, or (b) per tenant)? | owner + compliance |
| D2 | The cooling period value (or none) and what it is counted from | owner + compliance, security consulted |
| D3 | Which dimensions may ever be loosened (list removal, longer interval, longer grace); in particular whether the sanctions list may ever be removed | owner + compliance |
| D4 | Should loosening of the TRIGGER policy (fewer events, gate off) also need two Persons? | owner + security |
| D5 | Approver population (today `platform_admin` only, two approvals: the interim HDR-M-1 answer) | owner |
| D6 | Whether jurisdiction-specific floors replace the engineering floor of 3600 s | owner + legal |
| D7 | Fate of the unrouted `aml_screening_policy_set` (keep reachable by the runtime role, or revoke) | owner + security |
| D8 | Should tenants be told of a loosening of their policy (today: no tenant projection) | owner + compliance |
| D9 | Back Office FILING form (today API only) | owner (product) |

## 4. What was built, precisely

Migration `0190_c4_screening_policy_governed_loosening` (up and down; down refused with `LC095` while any capability row, platform-authored policy version or request of
the two kinds exists):

- Table `aml_screening_loosening_settings`: append-only, owner-only, ENABLE + FORCE RLS with the single `aml_owner_session` policy, no privilege for the runtime role,
  insert guard (`LC040` without an executed configure request of the same transaction and payload). `cooling_seconds` has a structural check 0..255600 (a request lives 72 h).
- `aml_screening_policies`: new nullable `change_request_id` (FK to the request, unique), `created_by_scope` may also be `platform` (checked to coincide with a request id),
  and `aml_screening_policies_guard` admits a `platform` row only with an executed `aml_screening_policy_loosen` request of the same transaction whose payload equals the row.
  The earlier `platform_acting` path of the guard is byte for byte the 0154 check.
- Kind registry rows (2 approvals, `platform_admin`), and `licensing_change_classify` renamed-and-wrapped (the 0180 body is kept untouched as
  `licensing_change_classify_base`; the wrapper adds the two kinds). A later migration that replaces `licensing_change_classify` must keep the wrapper (the tests fail
  closed otherwise).
- `screening_policy_loosen_assert` (validation, shared by request and execution), `screening_loosening_state` (platform sessions only), `screening_loosening_settings_apply`
  and `screening_policy_loosen_apply` (the executors; `LC040` without an executed request of this transaction and actor).
- Go: `internal/licensingchange/screening_loosening_kinds.go` (strict payload decode, `register()` of both kinds, audit rows with before and after), closed refusal codes
  (`aml.RefusalCodes` +5, OpenAPI `AMLScreeningRefusalCode`, `backoffice/src/features/aml/screeningErrors.ts`, the drift pin), `check_codes` on the licensing change guard
  refusal.
- Back Office: the two kinds are labelled and filterable and their refusals shown in fixed wording on the generic licensing change list and detail pages, so APPROVE,
  REJECT and CANCEL work with no new screen. FILING a request has no form: NOT IMPLEMENTED (API only, `POST /v1/admin/licensing-changes`).

## 5. The production guard

`aml_screening_trigger_policy_set` (the only code that can write `gate_withdrawal = true`), `screening_worker_available()` and `aml_screening_gate_environment_ok()` are not
redefined by 0190 (a static test reads the migration and fails if it mentions them). A loosening writes no trigger policy. Additionally the loosening validator refuses
(`gate_environment_refused`) where the latest trigger policy of the jurisdiction gates withdrawals and `aml_screening_gate_environment_ok()` is false, so the executor is
not a way to move a policy in a gated tenant of a production-anchored database. Tested on throwaway databases anchored `production`, unrecorded and `staging`.

## 6. Residual limits (also in the record)

- The primitive's constants apply: approvals count only when at most 24 h old at execution; a request lives 72 h. A cooling period longer than 71 h cannot be satisfied.
- The cooling period is counted from FILING; approvals made inside it are allowed but the executing approval is refused and rolled back (approve again afterwards).
- "Loosening" is measured against the LATEST version only (the in-force comparison of the tighten-only route is not repeated for the governed path).
- The trigger policy is not covered (D4). The break-glass setter is not removed (D7).

## 7. Review fixes (security and ledger-finance reviews, applied in place to 0190)

1. Officer controls (SEC HIGH-1): requester, every counted approver and the executor hold an in-force platform `aml_officer` grant on the tenant and the owner-scope quorum
   holds; non-officer approvals are refused when recorded (a trigger on `licensing_change_approvals` for the two kinds) and not counted at execution. Narrowing configure stays open.
2. Sanctions list never removed (LF H-1, safe default). 3. Every dimension looser or equal (LF M-2). 4. Effective time is the later of the offset and the cooling period (SEC MEDIUM-1).
5. Capability advisory lock in validation and execution (SEC LOW-1). 6. The policy guard bounds the effective time to [executed_at, executed_at + 30 d + 1 min] and fixes the reason code (SEC LOW-2).
7. A longer backfill grace is refused once the previous grace has run out while a gate exists (LF M-1 guard); the retro-release by a longer interval stays D10 (runbook section "Retroactive effect").
Updated decisions: D2 (the effective time now also waits the cooling period), D3 (the sanctions list is closed by default; relaxing it is owner + compliance and needs per-clear
screened-lists recording and a gate subset check first), D10 (retroactive release by a longer interval).

## 8. Second review round (ledger-finance N-1, N-2, L-1; security LOW-A, info)

- N-1: the grace guard is evaluated at the version's EFFECTIVE time (execution + the later of the offset and the cooling period) in the validator, and repeated in the platform branch of the policy guard.
- N-2: `screening_loosening_reconcile()` (platform_admin sessions only, ids and kinds only) replaces the owner-only query.
- LOW-A: own-licence tenants (owner scope `tenant`) are OUT OF SCOPE: `loosening_not_supported_for_tenant_licensing_model` is raised before the quorum read. Supporting them needs the approver model (D5) and a quorum read that counts the platform grants designed first; nothing was widened.
- L-1: the audit outcome carries both `previous_*` (the latest created version) and `in_force_*` (the version in force at execution).
- Info: `screening_loosening_officer_ok` and `_officers_assert` are runtime-executable SECURITY DEFINER helpers that answer whether a staff id holds an officer grant on a tenant: a possible oracle for any caller. They now require a platform session (otherwise `LC001`); a platform admin could already read grants, so nothing new is disclosed to them.
