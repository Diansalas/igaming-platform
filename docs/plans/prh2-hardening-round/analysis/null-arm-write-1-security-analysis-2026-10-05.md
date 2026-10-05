# NULL-ARM-WRITE-1 security analysis (READ-ONLY, 2026-10-05)

Reviewer: security. Source: `git archive HEAD` (66d2968) to scratchpad/sec_na_src; private DB `sec_na`
(migrations up to 0117, TEMP revoked by 0116), probes as the real `igaming_runtime` role.
Raw probe output: scratchpad/na_probe_W0.txt, na_probe_other.txt, na_probe_chain.txt; script na_probe.sh.
Every probe was BEGIN..ROLLBACK. No repo edits, no role or credential changes.

## 1. The finding
These permissive write policies have an arm `tenant_id IS NULL AND NULLIF(app.tenant_id,'') IS NULL`
(or for `persons`, just `app.tenant_id IS NULL`) and no positive guard. Live `pg_policies` catalogue:
- staff_users.dual_scope_isolation (ALL)          [0011]
- audit_log.dual_scope_isolation (ALL)            [0011]
- login_attempts.dual_scope_isolation (ALL)       [0011]
- sessions.session_scoped_insert / _update        [0018]
- risk_rules.tenant_and_platform_write/_update/_delete_visibility
- player_restrictions.staff_insert                [0037]
- persons.persons_platform_scope_update/_delete (app.tenant_id IS NULL); persons_insert_any_scope WITH CHECK (true)
Siblings asset_operation_eligibility / open_bet_self_exclusion_policies require
`app.platform_admin_principal_id IS NOT NULL` (presence only, NOT validated against staff_users).
The sb_* catalogue tables need platform_service_id='sportsbook_catalogue_sync'. Every other NULL arm
needs the platform_admin GUC. The restrictive fences (0112 acting_fence_*, 0114 kyc_worker_fence_*)
cover acting and kyc_worker sessions only.

## 2. Probe matrix as igaming_runtime
Shapes: W0 = no GUC (WithoutTenant / pre-auth login); SB = sportsbook_catalogue_sync; AD = alert_dispatcher;
KW = kyc_submission_worker; TEN = tenant GUC (control); PA = platform_admin GUC set to a random uuid.

| table | INSERT NULL row | UPDATE NULL row | DELETE NULL row | blocked by |
|---|---|---|---|---|
| staff_users | W0/SB/AD/PA: OK (platform_admin minted) | OK (password_hash of real admin) | OK (real admin deleted) | nothing except KW fence and TEN |
| audit_log | OK (forged platform row) | ERROR append-only | ERROR append-only | trigger audit_log_immutable |
| login_attempts | OK | ERROR append-only | ERROR append-only | trigger |
| sessions | OK (refresh session for real admin) | 0 rows; UPDATE 1 with app.session_internal_op_id set | 0 (no DELETE policy) | SELECT-visibility only |
| risk_rules | OK (platform deny rule) | OK (status='disabled' on platform hard limit) | ERROR append-only | immutability trigger (core fields only) |
| player_restrictions | OK (platform-wide self-exclusion of any person) | ERROR append-only | ERROR append-only | trigger |
| persons | OK in every shape incl. TEN (by design) | OK (status of any person) | FK blocks referenced persons | FK only |
Results are the same for W0, SB, AD and PA. KW is denied everywhere (0114 fence). TEN cannot touch NULL rows.
The staff_users CHECK forces `tenant_id IS NULL <=> role='platform_admin'`, so every NULL-row insert mints a platform_admin.

Chain probe (na_probe_chain.txt):
- Mint person + platform_admin in W0, set the platform GUC to it: `financial_actor_session()` returns scope=platform.
  A tenant-visible platform audit row (`subject_tenant_id`) is then accepted by `audit_log_subject_actor_guard`.
- Control: a random uuid is refused (CG001).
- Without any NULL-arm write, the real admin id can be read in W0, and setting the GUC to it impersonates
  the real admin (`financial_actor_session` = platform/ad01). The tenant shape impersonates the real tenant staff the same way.
  Tenant staff and persons can be minted through the tenant arm.
- TEMP: denied ("permission denied to create temporary tables").

## 3. Reachability
Production WithoutTenant callers (HEAD):
- Read-only: identity/tenant.go:70, :98; identity/brand.go:58; rg/enumeration_sweep.go:59; reconciliation/scheduler.go:198;
  bonus/schedulers.go:45; payments/sweeper_loop.go:162; httpserver/sportsbook_handlers.go:103, :206;
  sportsbook_jurisdiction_admin_handlers.go:258; admin_routes.go:168; payments_kill_switch_handlers.go:164;
  audit_platform_actions_handlers.go:263; auth_routes.go:351 (method value, refresh re-check).
- WRITES NULL rows:
  - staff_routes.go:77 (method value; PRE-AUTH platform staff login): login_attempts, sessions, audit_log.
  - auth/session.go:167 (platform session rotate/revoke): sessions, audit.
  - admin_routes.go:753 (platform admin links a person): staff_users UPDATE person_id, audit_log.
  - cmd/seed-admin/main.go:88 (bootstrap): persons, staff_users platform_admin, audit.
- Platform display_name writes already use WithPlatformAdmin (admin_routes.go:1447-1457).
- No production writer of NULL-tenant risk_rules or player_restrictions: risk_handlers.go:243/292 and
  rg_handlers.go:216 use WithTenant.
- All SQL in these paths is static and parameterized (pgx). The only fmt.Sprintf calls build `$n`
  placeholder indices (identity/tenant.go:176-185, player_account.go:234-243, admin_routes.go:1171).
  No injection surface was found.
- Service identities SB/AD run fixed upsert/raise SQL. KW is fenced.
Result: no current application path writes an attacker-chosen NULL row. The exposure needs
(a) arbitrary SQL as igaming_runtime, or (b) a future Go bug that puts a write in a no-tenant scope.

## 4. Existing mitigations (verified)
- FORCE RLS on all seven tables. The owner is not BYPASSRLS.
- Append-only triggers on audit_log, login_attempts, player_restrictions, and the risk_rules core fields.
- Production gate VerifyRuntimeRoleInProduction refuses an owner role and a TEMP-holding role (production_safety.go:112).
- db.Connect refuses superuser and BYPASSRLS roles (db.go:66).
- 0116 TEMP revoke. The 0112 acting fence and the 0114 kyc_worker fence. A-18 static replay for new NULL arms.
- There is NO staff_users insert guard trigger. The only trigger is person_id_append_only.

## 5. What the NULL arm adds beyond arbitrary SQL as runtime
An attacker with arbitrary SQL can already set app.tenant_id/app.principal_id/app.platform_admin_principal_id.
That lets them impersonate any existing tenant staff or platform admin to every GUC-based DB validator (financial_actor_session,
four-eyes guards), mint tenant staff and persons, and read or write every tenant's data.
The NULL arm adds:
1. Minting NEW platform_admin principals. This only matters for platform four-eyes and two-person checks while fewer than
   two real platform admins with distinct person_ids exist (likely true at the B2C launch: seed-admin creates one).
   With two or more, impersonating the real admins does the same.
2. Takeover or deletion of real platform admin rows: password_hash, status, email, DELETE. That is persistence that
   survives patching the SQL hole, and DoS of the back office.
3. Forging a NULL-tenant refresh session for a real platform admin. /refresh then issues a platform-admin access JWT.
   This turns DB access into an HTTP-layer platform-admin token.
4. Disabling a platform-wide risk hard limit, or injecting a platform-wide deny rule.
5. An irreversible platform-wide self-exclusion of any person (append-only).
6. Forged platform audit rows. Existing rows cannot be edited.
7. persons.status/person_key_hash edits. Nothing in Go reads persons.status today, so this is LOW.

The registry's proposed fix ("require validated platform-admin GUC") does NOT remove items 1-6 for an
arbitrary-SQL attacker. The PA probe shows presence of the GUC passes, and the real admin id passes any
EXISTS-style validation. It also cannot apply to login_attempts, sessions or audit_log, because the pre-auth
platform login (staff_routes.go:77) has no principal yet. Item 3 stays open after any RLS change, because
session issuance is indistinguishable in SQL. The fix's real value is defence in depth against Go-discipline errors.

## 6. Verdict per table (threat: Go bug | arbitrary SQL as runtime)
| table | verdict | severity |
|---|---|---|
| staff_users | production-hardening, do before 2nd tenant / before >1 platform admin is relied on for DB four-eyes | MEDIUM |
| risk_rules | production-hardening; arm is unused by app, so closing is zero-impact | MEDIUM |
| player_restrictions.staff_insert | production-hardening; unused arm, zero-impact close | MEDIUM-LOW |
| sessions | accept/residual: arm required by pre-auth platform login; mitigation is outside RLS | LOW (RLS cannot help) |
| login_attempts | accept: append-only; arm required by pre-auth login | LOW |
| audit_log | accept for now: append-only; arm required by pre-auth login and platform writes | LOW |
| persons | hardening (narrow update to columns via trigger) | LOW |
Overall: NOT a launch blocker, under the current threat model and with no app path reaching it.
It is NOT "already mitigated" either: RLS does not stop it, only Go discipline does.

## 7. Hardening (when): smallest exact changes, migration number assigned by the orchestrator
A (zero app impact, before launch recommended): restrictive fences that close the unused arms
```sql
CREATE POLICY null_arm_platform_admin_insert ON risk_rules AS RESTRICTIVE FOR INSERT
  WITH CHECK (tenant_id IS NOT NULL OR NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL);
CREATE POLICY null_arm_platform_admin_update ON risk_rules AS RESTRICTIVE FOR UPDATE
  USING (tenant_id IS NOT NULL OR NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL)
  WITH CHECK (tenant_id IS NOT NULL OR NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL);
CREATE POLICY null_arm_platform_admin_insert ON player_restrictions AS RESTRICTIVE FOR INSERT
  WITH CHECK (tenant_id IS NOT NULL OR source = 'player_self_service'
              OR NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL);
```
(player_self_insert rows have tenant_id NULL and must keep working, hence the source exemption. That arm is positively bound to app.player_account_id.)
B (needs one Go change): staff_users restrictive INSERT/UPDATE/DELETE fence with the same predicate.
Plus convert admin_routes.go:753 from WithoutTenant to WithPlatformAdmin(subjectID). seed-admin needs a bootstrap
path; it runs before any principal exists, so either it sets the GUC to the staff id it is creating, or the policy
exempts the table owner. Decide in review.
C (the only change that helps against arbitrary SQL; design item, post-launch unless the owner extends the threat model):
a BEFORE INSERT/UPDATE/DELETE trigger on staff_users that refuses NULL-tenant row creation/deletion and changes to
tenant_id/email/password_hash/role/status of NULL-tenant rows unless current_user owns the table (owner-only platform-admin
lifecycle). Session forging (item 3) needs session issuance behind a SECURITY DEFINER credential check. That is a redesign.
Tests for A/B: as igaming_runtime in W0/SB/AD shapes, a NULL-tenant INSERT into risk_rules/player_restrictions(staff)/staff_users
gets 42501. UPDATE of a NULL risk_rules row affects 0 rows or errors. The PA shape still succeeds (positive control). The tenant arm and
player self-exclusion still succeed. admin_routes person-link integration still passes. Mutants: drop each fence, which the test must
catch; swap the predicate to `IS NULL`, which must be caught; remove the source exemption, which the self-exclusion test must catch.

## 8. Owner decisions
1. Is "arbitrary SQL as igaming_runtime / stolen runtime credential" in the threat model for DB-enforced controls?
   If YES, NULL-ARM-WRITE-1 (item C), the TEMP item, AND the K2/four-eyes GUC-impersonation exposure (§2 chain, independent of
   NULL arms and TEMP) all become launch blockers, and the fix is architectural (per-principal DB auth or SECURITY DEFINER issuance),
   not a policy tweak. If NO, NULL-ARM-WRITE-1 is a pre-launch hardening item (A+B).
2. Whether to apply A+B before launch (recommended; small, low risk) or immediately after.
Not in scope: no end-to-end K2 adjustment execution via impersonation was run. Only actor resolution was proven. No HTTP /refresh
replay with a forged session was run; the DB insert and the code path auth/session.go:123-167 were verified.

## 9. Hardening A validated on scratch DB (applied as owner to sec_na only, then dropped)
W0/SB: NULL risk_rules INSERT -> 42501 "null_arm_platform_admin_insert"; UPDATE of platform rule -> 0 rows;
NULL staff player_restrictions INSERT -> 42501. PA shape still succeeds (positive control; shows the fence is
Go-discipline defence only). Tenant-arm risk_rules and staff restriction inserts still succeed.
NOT run: player self-exclusion positive control (needs a player_account fixture); full go test suite with the fence.
