# ADR 0119 S5 + S6 (staff lifecycle): independent live security review

- Reviewer: `security` specialist. Date: 2026-10-14.
- Branch `worktree-agent-a031d51e36f08b4f0`, reviewed code at head `131f0d8` (base `ff23a5c`). The later commit `b7f01c3`
  adds only the code-reviewer document, so the code is the same.
- Scope: migration 0139 (up), `internal/stafflifecycle`, `internal/httpserver/staff_lifecycle_routes.go`,
  `internal/httpserver/staff_invitation_accept.go`, the person-link hook in `admin_routes.go`, `internal/db/actor_staff.go`,
  the liveness `sv` comparison, `cmd/seed-admin`, and the `deploy/init-app-role.sql` mirror.
- Method: I did not rely on the implementer's report. I probed live on two PRIVATE databases (`priv_secrev_s5s6`, `priv_secrev_s5s6_t`)
  built from the worktree's migrations with the scratchpad `priv_db.sh` / `priv_test.sh` harness. All probes ran as
  `igaming_runtime` with forged GUCs. For actor proofs I used a review-only key that I inserted into the private database's
  `actor_proof_keys` as the database owner. No shared role, password or test infrastructure was changed. I did not use sudo.
- I also ran a temporary HTTP probe test (`TestSecRev_Probes`) against `httptest` and the private DB, then deleted it. It
  is not committed. The existing `-tags integration` suites `./internal/stafflifecycle/` and the lifecycle / person-link /
  golden / ADR0119 subsets of `./internal/httpserver/` pass on the private DB.
- Not in scope: penetration testing, the back-office UI (S7/S8), and S3 MFA internals beyond their use here (token MAC,
  TOTP). This was a single review: it does not mean the subsystem is "secure".

## Verdict

**NOT APPROVED to be marked complete.** There are four Medium findings (SR-1 to SR-4). Each one needs a fix or an explicit owner
acceptance recorded in `docs/decisions/` before S5 + S6 is labelled `IMPLEMENTED`. **Launch-blocking: SR-1, SR-2 and SR-3**
must not reach production unresolved. The DB guard layer (SF010-SF014, SF020/SF021, SF031-SF033, the 0139 actor-proof
extension, the column-level grant) behaved as specified under every adversarial probe. The weaknesses are in the
application authority around it and in the bootstrap tool.

SR-1 was found independently by the code-reviewer (`adr0119-s5s6-code-review.md` F1, rated High there). I reproduced it.

## Findings

### SR-1 (Medium; launch-blocking): the default mode of `cmd/seed-admin` bypasses every break-glass control

The ADR 0119 section 11 rules are enforced only when `-break-glass` is passed: the refusal while a usable platform admin exists,
the required `-operator` / `-reason`, the `owner_provisioned` event and the P1 `staff.platform_break_glass_used` alert. The
default ("first admin") mode has no precondition. At any time it creates an **active** `platform_admin` and a 24 h bootstrap
token. `op.validate(required=false)` means no operator or reason is needed. It writes no lifecycle event and raises no alert.

Repro (private DB, runtime `DATABASE_URL`, dev `STAFF_MFA_*`, three usable platform admins present):
- `seed-admin -email bg@x.invalid -create-person -break-glass -operator op1 -reason ticket-1` was refused (correct).
- `seed-admin -email sneaky@x.invalid -create-person` printed `created platform_admin sneaky@x.invalid ...` and a bootstrap
  token. The row is `active`. There are 0 lifecycle events and 0 break-glass alerts.

Impact: anyone who can run the deployment job can mint a platform admin silently by leaving out one flag. The ADR states the
four-eyes `platform_invite` and the alerted break-glass as the only creation paths, and this makes that statement false.

Fix: the default mode must refuse unless `staff_users` holds no `platform_admin` row in any status, or at least none that is
active. All later creations go through `-break-glass`. Require `-operator` / `-reason` in every mode. Add a test.

### SR-2 (Medium; launch-blocking): invitations and reset tokens issued by an actor survive the actor's suspension or offboarding

`revokeAccess` revokes only the TARGET's sessions, challenges and enrolment tokens. The accept handler validates the token
and the invitee row, but it never checks whether the issuer (`staff_enrolment_tokens.issued_by` / `staff_users.invited_by`)
is still live.

Repro (live HTTP probe): tenant admin TA1 invites `accomplice@...` as `tenant_admin`, with a Person of TA1's choosing. TA2
then suspends TA1 with `reason_code=security_compromise`, which answers 200 and makes TA1's old token 401 `session_revoked`.
`POST /v1/staff/invitations/accept` with the outstanding invite token then answers **200 with an `enrol` challenge**. The
accomplice tenant_admin account is activated after the compromised admin has been contained.

Fix: on suspend or offboard (at least for `security_compromise`, preferably always), revoke every live enrolment token with
`issued_by = target` and offboard (`invite_revoked`) every `invited` row with `invited_by = target`, in T2, with
events. As a backstop, also refuse acceptance when the issuer is no longer active. Test both orders.

### SR-3 (Medium; launch-blocking): the four-eyes approver cannot see the invitee Person, and Person reuse is not refused

`platform_invite` binds `invite_person_id`, which is the attribute that decides Person-distinctness in every later four-eyes
check. `platformStaffLifecycleRequestDTO` (GET list / one) does not return it, and the approver submits a `payload_hash`
they cannot recompute or relate to a Person. Neither the Go code nor SF031 refuses a Person that is already bound to another
active platform staff account.

Scenario (DB probe confirms each step is accepted): PA1 (Person a1) requests a `platform_invite` for a mailbox they control,
bound to **Person a2, which is PA2's Person**. SF032 refuses PA2 as approver, so PA3 approves without seeing the Person. The
new account (Person a2) can approve every later request of PA1, because a2 != a1. A single human now holds two Persons'
worth of approvals. This extends ADR R14 to the platform and bypasses the platform four-eyes.

Fix: return `invite_person_id` in the request read, plus the staff accounts already linked to that Person. Refuse
(SF031 + Go) an invite Person that is already linked to any non-offboarded staff row unless an explicit, audited dual-role
flag is set. Recommended: apply the same rule to tenant invites of approve-capable roles.

### SR-4 (Medium; carry-forward that S6 touched): the person-link routes have no self-target refusal and no role authority

`newLinkStaffPersonHandler` / `newLinkPlatformStaffPersonHandler` were changed in this slice (`PersonLinkedInTx`). The ADR
classes them `critical (b)` because the Person binding decides four-eyes distinctness. They still check only `staff:manage`
plus the tenant. They:
- do not call `refuseSelf`. An admin with a legacy unlinked account can link **their own** account to any Person UUID.
  A human holding two legacy unlinked accounts (the pre-S6 seed-admin default) links them to two different Persons and can
  then self-approve;
- do not call `authorizeTarget` / `Grantable`. A tenant_admin can bind a Person on a `finance` or `risk_manager` account in
  its tenant, even though it may not act on that role by any other lifecycle route. The live probe linked an unlinked
  support account with 204. The finance case follows from the same code path.
- take no in-transaction actor re-assertion (FOR SHARE + role/sv); they rely only on request-start liveness.

Fix: apply `refuseSelf`, `authorizeTarget` and `reassertActorOnly` (or `lockActorAndTarget`) in both handlers. Consider
four-eyes for linking a Person to an approve-capable account.

### SR-5 (Low): a pending platform request from a requester who has since been suspended or offboarded stays approvable

`lockPending` and SF032 check only the decider. This repeats the ADR's R6 pattern in a brand-new flow. Combined with SR-3,
a compromised admin's pending `platform_invite` stays approvable after containment. Fix: the decider path (Go and SF032)
refuses unless the requester is still `active`; suspension or offboarding cancels the target's pending requests.

### SR-6 (Low): the runtime role keeps `DELETE` on `staff_users`

0139 narrowed UPDATE to columns but left DELETE. No application code deletes staff rows. Probe: a runtime session created
and then `DELETE`d an active `platform_admin` with no dependent rows (OK). DELETE bypasses SF010's "offboarded is
terminal", SF020 and all lifecycle audit. Rows with factors or events are protected only incidentally, by FKs. Fix: revoke
DELETE from runtime (migration + init-app-role) and add a deny-delete trigger.

### SR-7 (Low, defence in depth): the GUC-keyed bypasses accept forged values that are not validated

- SF021's platform bypass accepts any non-empty `app.platform_admin_principal_id` / `app.acting_platform_principal_id`.
  Probe: a nonexistent UUID bypassed SF021 (OK). `app.actor_staff_id` is validated (a tenant staff id did not bypass:
  SF021 fired). Fix: require the named row to be an active `tenant_id IS NULL` staff member.
- `staff_actor_self_select` is a cross-tenant read primitive when the GUC is forged. Probe: with `app.tenant_id=T1` and
  `app.actor_staff_id=<T2 staff id>`, the T2 row was returned. Only platform actors need the pair, so add
  `AND tenant_id IS NULL` to both `staff_actor_self_select` and `staff_actor_self_lock`.

### SR-8 (Low): reject / cancel

Class `none` matches ADR S56-D14, and I accept it as protective. Two notes. (a) `Reject` / `Cancel` take no in-transaction
actor FOR SHARE re-assertion (role / sv). SF032 / `sf_lifecycle_actor_session` only check that the actor is active. (b) A
hijacked aal2 session without step-up can veto a `platform_reactivate` / `platform_mfa_reset` that is recovering a
locked-out admin (a governance DoS). Recommend `reassertActorOnly` in both, and a P2 alert on rejects of recovery kinds.

### SR-9 (Low): the password floor at invitation acceptance is 8 for every role, including platform_admin

`cmd/seed-admin` requires 12 for a platform_admin. A platform_admin created by `platform_invite` can choose 8.
Align the floor to at least 12 for approve-capable roles, or for all staff.

### Info / accepted residuals confirmed live

- **R1 (owner-deferred S-H1) confirmed.** In a platform scope, the runtime role can reactivate a platform admin with no
  request, can INSERT an `active` platform_admin directly, and can perform an unrelated `staff_users` change inside an
  approved request's `executing` window. Execution is not bound to the request. Accepted until S-H1, as stated in ADR 0119
  section 12.
- A single platform admin may suspend or offboard every other platform admin (ADR: single-actor, protective). SF020
  leaves the actor as the only usable admin, and recovery then needs break-glass. Recommend a P1 alert on any
  platform_admin suspension or offboarding.
- A tenant_admin can reactivate a staff member that a platform admin suspended, including for `security_compromise`, so
  platform-imposed suspensions are not sticky. Owner decision.
- Factor revocation or hard-lock is not covered by SF020/SF021. A tenant-scoped SQL session can revoke every tenant
  admin's factor (ADR R3: DoS, not bypass).
- `staff_lifecycle_events.actor_staff_id` and its content are not bound to the session (SQL-forgeable evidence). R1 class.
- T1 of the two-phase suspension does not re-assert the actor in the transaction (by design: no staff lock in T1). A
  just-demoted actor can still log a colleague out in that window.
- Legacy unlinked rows: SF014 fires only at INSERT and role change. Unlinked legacy platform admins remain active, can
  suspend or offboard other platform admins and manage tenant staff, but cannot request or approve four-eyes (verified in
  code and SF031/SF032). See SR-4 for the self-link path.

## Verified as specified (live)

| Control | Probe result |
|---|---|
| SF010 graph | active->invited, INSERT suspended, offboarded->active, forged `status_changed_at`, and a role change on a suspended row were all refused SF010 |
| SF011 / SF012 / column grant | email, tenant_id and staff_version UPDATE got 42501 (not granted). INSERT with staff_version 7 got SF012. A person link, password change or status change bumps sv; display_name and same-value writes do not |
| SF013 | self status/role was refused under `app.principal_id`, `app.actor_staff_id`, `app.platform_admin_principal_id` and `app.acting_platform_principal_id`. A forged principal is accepted (stated defence in depth) |
| SF014 / 0034 | role->compliance without a Person, and INSERT of tenant_admin without a Person, were refused. Unlink and relink were refused (append-only) |
| SF020 | the last usable platform admin was refused for suspend, offboard and a multi-row UPDATE |
| SF021 | refused under tenant scope (with or without principal) and under a forged tenant `actor_staff_id`. It stands aside for a platform actor (by design) |
| self pair | `WITH CHECK (false)`: an UPDATE of the actor row got 42501. FOR UPDATE lock only |
| SF031 | wrong session (tenant / acting / none), inactive requester, tenant target, invitee Person = requester Person, forced columns (requested_by / state / expiry overridden), content immutable during execution, illegal moves (executing without approval, reject without decision, early expire, cancel by non-requester) |
| SF032 | requester by id and by Person (a second account of the same Person), target by id and by Person, invitee Person, payload mismatch, tenant / acting session; forged `decided_by` overridden |
| SF033 | `executing` left at commit was refused |
| actor proofs | no proof AP001; proof for another actor, for tenant scope, or for another op AP004 |
| request RLS | invisible to tenant and no-GUC sessions |
| HTTP tenant isolation | a T2 admin got 404 on T1 list / get / events via the T1 path and the T2 path, 404 on suspend, and 403 on platform-staff and four-eyes lists; no email leaked |
| sessions | the suspended actor's token got 401 `session_revoked` (liveness `sv` / status) |
| tokens | the invite response does not contain the mailed token. The link uses a URL fragment. Accept replay answers 401 `invalid_enrolment_token` (code + existing test). Responses are `no-store`. `InvitationAcceptRequest` is `x-sensitive`. The email package and `deliver` never log the body |
| staffRoute chain | every lifecycle route is a `staffRoute` (bearer, staff principal, liveness, aal2, step-up). Mutations are critical except reject / cancel (none, per ADR). Accept is unauthenticated and rate-limited on the credential bucket |
| break-glass flag | `-break-glass` without `-force-additional` was refused while usable admins existed (but see SR-1) |

## Required authorization / isolation tests for `qa` (to add with the fixes)

1. SR-1: seed-admin default mode with an existing platform_admin is refused, and no row is written.
2. SR-2: invite (and mfa-reset) tokens issued by X are refused after X is suspended or offboarded, in both orders (suspend
   before and after the invitee's accept attempt).
3. SR-3: the GET request shows `invite_person_id`. A platform_invite naming a Person already linked to active staff is refused
   (Go and SF031 direct SQL).
4. SR-4: person-link self-target gives 403 `self_target_forbidden`. A tenant_admin linking on a finance / risk_manager account
   gives 403 `role_not_grantable`. Cover both routes.
5. SR-5: approving a request whose requester is now suspended is refused.
6. SR-6: `DELETE FROM staff_users` as runtime gives 42501.
7. SR-7: SF021 with a forged nonexistent platform GUC still refuses. A forged `actor_staff_id` naming a tenant row reveals nothing
   cross-tenant.
