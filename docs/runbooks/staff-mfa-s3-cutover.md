# Staff MFA S3 cutover runbook (ADR 0119 slice S3, migration 0138)

Status: written with slice S3 (`IMPLEMENTED` against the local database, NOT MERGED, gate G pending). It describes a
procedure for a development or staging environment. **S3 must not reach any persistent environment (staging or
anything later) without this runbook being followed** (ADR 0119 section 22.8, note under G7). Production has never
run; production launch authorisation is the owner's decision and is not implied by anything here.

Nothing in this runbook is a software control unless it says so. Where a rule is a human control (two operators, a
ticket), it is labelled as such.

## 1. What S3 changes

- Staff login is two-step. `POST /v1/staff/auth/login` (the password) never returns a token pair; it returns a
  single-use `mfa` challenge, or `403 mfa_enrolment_required` when the account has no second factor, or
  `403 mfa_locked`. The token pair comes from `POST /v1/staff/auth/mfa/verify` (TOTP). Contract:
  `docs/plans/adr0119-s3-api-contract.md` and `docs/api/openapi/platform-api.yaml`.
- Staff sessions carry `aal`, `amr`, `auth_time`, `mfa_at`, `absolute_expires_at` and a keyed seal. A staff session
  lasts at most 12 h from the second-factor login; each refresh token lives at most 30 min (configurable down only).
- `cmd/platform-api` and `cmd/seed-admin` refuse to start without `STAFF_MFA_FACTOR_KEYS` / `STAFF_MFA_MAC_KEYS` (and
  their active kids) **in every environment**, APP_ENV unset included. There is no bypass mode.
- Migration 0138 revokes **every** staff session that exists when it runs. Every staff member logs in again, and
  every staff member without a second factor must enrol first (section 4).

## 2. Preconditions (before anything is deployed)

1. **Keys.** Both key families are provisioned in the environment's secret store and injected into
   `platform-api` and into the `cmd/seed-admin` job exactly as `ACTOR_PROOF_KEYS` is
   (`docs/runbooks/staff-mfa-keys.md`). They are distinct from every other platform key family (startup checks every
   pair). The deployment templates under `deploy/aws` do not inject them yet (AWS is OFF): wiring them is part of
   provisioning, `PROVIDER DEPENDENT` on the operator.
2. **Database logging (S3 entry gate G6).** Bind parameters of the authentication statements carry keyed MACs and
   ciphertext, never raw secrets, but they must still never reach the server log or statement statistics:
   - `log_parameter_max_length = 0` and `log_parameter_max_length_on_error = 0` (no bind values in any log line);
   - `log_statement = 'none'` or `'ddl'` (never `'all'` / `'mod'`);
   - `pg_stat_statements`, if installed, keeps its default normalisation (constants replaced by `$n`; parameters of
     the extended protocol are never stored) - do not enable any extension or setting that records parameter values.
   Check: `SHOW log_parameter_max_length; SHOW log_parameter_max_length_on_error; SHOW log_statement;` as a DBA.
3. **An operator who can run the `cmd/seed-admin` job inside the deployment environment** (the same secret-store
   injection as `platform-api`: `DATABASE_URL` for the runtime role and both `STAFF_MFA_*` families). It never runs on
   a workstation (ADR 0119 section 11).
4. **The back office (slice S7a) is deployed together with S3** (one merge unit, ADR 0119 section 16): the old
   single-step login page cannot complete a login after S3.
5. **Back office response headers (S7a security review F2, deploy requirement).** The origin that serves the back
   office must send `Referrer-Policy: no-referrer` (the page also carries a `<meta name="referrer">`, but a header is
   authoritative and covers the document request itself). Enrolment links must be emitted **only** in the fragment
   form `https://<back-office-host>/#/enrol?token=...` (a fragment is never sent to a server or in a Referer); the
   `cmd/seed-admin` bootstrap output and the S6 emails must not use a path or query form (`/enrol?token=`): the
   back office refuses it and shows a fixed invalid-link page. No existing production configuration checklist covers
   response headers; add this item when one is written.

## 3. Deploy order (ADR 0119 section 13.1a item 4)

1. Deploy the S3 code to **every** `platform-api` instance.
   - Until 0138 runs, staff logins fail closed (`503` at the second factor: the session columns do not exist yet);
     player traffic is unaffected. This is the staff-auth maintenance window the ADR accepts. (Tested:
     `TestMigration0138_RollingDeploy_FailsClosed`.)
2. Run migration **0138** (`cmd/migrate up` as the migration role). It:
   - refuses (`SF041`) and changes nothing if the schema is not the one it was written against;
   - adds the session columns, the `SF040` trigger, the `SF008` database-clock rule on the factor lockout counters
     and the ten staff alert Kinds;
   - records its own `now()` in `staff_session_cutover`;
   - revokes every live staff session and prints `NOTICE: migration 0138: revoked N live staff session(s)`. Record N
     in the change ticket.
   - A pre-S3 instance still running after this fails every staff session write with `SF040` (fail closed). (Tested.)
3. Verify:
   - `SELECT at FROM staff_session_cutover;` returns one row;
   - `SELECT count(*) FROM sessions WHERE principal_type = 'staff' AND revoked_at IS NULL AND assurance_seal IS NULL;`
     returns 0 (run as the owner; with FORCE RLS lifted inside a rolled-back transaction, or through the DBA's
     usual audited read path);
   - an old staff refresh token answers `401` and raises **no** `staff.session_integrity` alert (the review C1
     cutover rule: a session created before the cutover with no seal is a plain not-found);
   - a staff member can complete section 4 and log in.

Either order of steps 1 and 2 fails closed for staff and keeps players working, but the order above is the one the
ADR specifies; do not run 0138 first in a shared environment.

## 4. Re-seeding staff after the cutover (S3 to S6)

Until slice S6 there is **no** invitation, MFA-reset or password-reset path over HTTP. Every existing staff account
gets its first factor through a **bootstrap enrolment token** issued by `cmd/seed-admin`, run as a deployment job:

```text
# an existing account with NO active second factor (platform staff: omit -tenant-slug)
seed-admin -email=<staff email> -issue-bootstrap [-tenant-slug=<tenant slug>] \
  -operator=<who runs the job> -reason=<incident or ticket reference>
```

- `-operator` and `-reason` are **required** with `-issue-bootstrap` and are recorded in the audit row
  `staff.bootstrap_token_issued` (security review R4). Neither is a secret; the token is never recorded. Both refuse
  control, Unicode format (bidi override / isolate, zero-width) and line / paragraph separator characters
  (re-review F3).
- `seed-admin` applies the same staff MFA configuration rules as `platform-api` (security review C2): the published
  `.env.example` keys are refused unless `APP_ENV=development` is set explicitly, and a staff MFA key equal to any
  other platform key (JWT, actor proof, payout instrument, credential fingerprint) is refused.

- The token is printed **once** on the job's standard output (its protected output channel). It is single use, valid
  24 h, stored only as a keyed MAC, and audited (`staff.bootstrap_token_issued`) without the token.
- Deliver it to the staff member out of band (a human control: a channel the staff member controls, never a shared
  chat). Any earlier live bootstrap token of that account is revoked by the new one.
- The staff member redeems it in the back office (`/#/enrol?token=...`, which calls
  `POST /v1/staff/auth/enrol/redeem` with their email, tenant slug and **current password**), scans the QR code,
  confirms a code and stores the 10 recovery codes. The token alone is useless without the password, and the
  password alone never opens enrolment.
- `-issue-bootstrap` **refuses** an account that already has an active factor, including a hard-locked one (an MFA
  reset is S6; between S3 and S6 use the owner procedure of section 5). `POST /v1/staff/auth/enrol/redeem` refuses a
  bootstrap token of an account that has an active factor too (defence in depth, security review R3) and raises
  `staff.mfa_integrity` / `staff.platform_mfa_integrity`.
- New platform admins: `seed-admin -email=<email> -create-person` (password from `SEED_ADMIN_PASSWORD`) creates the
  account and prints its bootstrap token in one run.

## 5. Break-glass between S3 and S6 (the owner MFA-reset gap)

What S3 cannot do (stated, not hidden):

- **There is no MFA reset over HTTP until S6** (ADR 0119 sections 10.1, 10.5). `hard_locked_at` is write-once
  (`SF002`), so a hard lock is never cleared in place: the factor is revoked and the staff member enrols a new one.

### 5.1 Owner procedure: hard-locked factor, or lost device with no recovery codes left (S3 to S6)

This is the ONLY procedure for these two cases between S3 and S6 (`TestStaffMFA_OwnerResetProcedure_S3toS6` runs it
end to end). It needs the database OWNER (the migration role), never the runtime role, and the human controls below.

1. Open the incident ticket with the reason. A hard lock is a compromise signal (the password was right and ten codes
   failed): first rotate the account's password through the owner's procedure, or suspend the account and stop here.
2. As the database owner, in ONE transaction, revoke the factor and its recovery codes in the staff member's scope:

   ```sql
   BEGIN;
   -- tenant staff only (omit for platform staff, whose rows carry no tenant):
   SELECT set_config('app.tenant_id', '<tenant uuid>', true);
   UPDATE staff_mfa_factors SET status = 'revoked', revoked_at = now(), revoked_reason = 'reset'
    WHERE staff_user_id = '<staff uuid>' AND status = 'active';            -- must report UPDATE 1
   UPDATE staff_mfa_recovery_codes SET revoked_at = now()
    WHERE staff_user_id = '<staff uuid>' AND consumed_at IS NULL AND revoked_at IS NULL;
   COMMIT;
   ```

   Anything other than `UPDATE 1` on the first statement: `ROLLBACK` and stop (wrong scope or wrong id).

   Then, still as the owner, revoke the account's live sessions (the refresh path does not re-check the factor
   before S4a, ADR 0119 S3-R11, so without this an existing session keeps refreshing until its expiry):

   ```sql
   BEGIN;
   -- tenant staff only (omit for platform staff):
   SELECT set_config('app.tenant_id', '<tenant uuid>', true);
   SELECT set_config('app.principal_id', '<staff uuid>', true);   -- makes the rows visible under FORCE RLS
   UPDATE sessions SET revoked_at = now()
    WHERE principal_type = 'staff' AND principal_id = '<staff uuid>' AND revoked_at IS NULL;
   COMMIT;
   ```

   Keep `AND revoked_at IS NULL`: `revoked_at` is write-once on staff sessions (`SF040`), so the unguarded form fails
   the whole statement as soon as one of the account's sessions is already revoked (a logout, a rotation).
3. Issue a bootstrap token: `seed-admin -email=<email> -issue-bootstrap [-tenant-slug=<slug>] -operator=<who>
   -reason=<ticket>` and deliver it out of band (section 4).
4. **Expected alert:** the staff member's next password step answers `403 mfa_enrolment_required` and raises the
   P1 `staff.mfa_integrity` (tenant staff) / `staff.platform_mfa_integrity` (platform staff), because an account
   that had a factor now has none. Acknowledge it against the ticket; an UNEXPECTED one of these alerts is an
   incident (a factor deleted or revoked by someone else).
5. The staff member redeems the token and enrols (section 4); the confirm revokes every earlier session of the
   account again. If the session revocation of step 2 was skipped, an existing session keeps refreshing until then
   (no liveness re-check before S4a, ADR 0119 S3-R11). Review the audit rows
   `staff.bootstrap_token_issued`, `staff.enrolment_token_redeemed` and `staff.mfa_enrolled` against the ticket.

The owner SQL writes no audit row of its own; the ticket and the `seed-admin` audit row (with `-operator` and
`-reason`) are the record. S6 replaces this procedure with the audited, four-eyes MFA reset.

### 5.2 No platform admin can log in

If every platform admin is locked out, either run 5.1 for one of them (including its session revocation), or use the
existing `cmd/seed-admin` job (ADR 0119 section 11, OQ-6 default) to create an **additional** platform admin. If a
locked-out admin is suspected compromised, revoke that admin's sessions with the guarded statement of 5.1 step 2
first:

  ```text
  SEED_ADMIN_PASSWORD=<strong password> seed-admin -email=<new break-glass admin email> -create-person
  ```

  then section 4 for that account.

Human controls for the break-glass (S3; the software controls of section 11 - `-break-glass -reason`, the refusal
while a usable admin exists, the lifecycle event and the P1 alert `staff_break_glass_used` - are **NOT IMPLEMENTED**
until S6):

1. Two operators present; an incident ticket with the reason before the job runs.
2. The job runs inside the deployment environment only.
3. Afterwards: review the audit rows `staff.platform_admin_bootstrapped` and `staff.bootstrap_token_issued`, and
   every `staff.*` alert raised for the locked accounts (`staff.mfa_hard_locked` / `staff.platform_mfa_hard_locked`).
4. A hard-locked account is treated as possibly compromised (its password was correct and ten codes failed): rotate
   its password through the owner's procedure and leave it locked until S6 can reset it or offboard it.

## 6. Alerts raised by S3 (notification routing is NOT IMPLEMENTED, ALERT-DELIVERY-1)

| Kind (tenant staff / platform staff) | Severity | Meaning |
|---|---|---|
| `staff.mfa_integrity` / `staff.platform_mfa_integrity` | p1 | a stored factor failed its AEAD check, or an account that had a factor has none |
| `staff.session_integrity` / `staff.platform_session_integrity` | p1 | a staff session's seal is missing (after the cutover) or wrong: a forged or edited row |
| `staff.mfa_hard_locked` / `staff.platform_mfa_hard_locked` | p2 | 10 second-factor failures in 24 h after a correct password |
| `staff.mfa_recovery_used` / `staff.platform_mfa_recovery_used` | p3 | a recovery code was used |
| `staff.mfa_failure_pattern` / `staff.platform_mfa_failure_pattern` | p2 | second-factor failures on 3 or more days within 7 days |

The alert rows exist; nobody is paged until routes and recipients are configured.

## 7. Rollback (development and staging only)

`cmd/migrate down` (with `APP_ENV=development` or `staging`) reverses 0138, but **refuses** (`SF042`) while any live
MFA-assured staff session or any `staff.*` alert exists, and it never un-revokes the sessions 0138 revoked. Rolling
back the code without the migration leaves staff logins failing closed (`SF040`). In practice: fix forward.

## 8. Residuals (see ADR 0119 sections 12, 21.5, 22.6 and 23)

- Until S4a there is no per-request liveness check: a staff access token stays valid for its 15 min lifetime after a
  suspension or a logout (unchanged from before S3).
- Since the S3 security review (R1), `sessions.revoked_at` and `replaced_by_session_id` are write-once on staff rows
  (`SF040`, every role including the owner): a logged-out or rotated staff session cannot be revived by SQL.
  `refresh_token_hash` and `expires_at` are immutable on staff rows (`SF040`) but are not inside the keyed seal
  (S4b condition, security review C3): the owner or a superuser (who can `ALTER TABLE ... DISABLE TRIGGER`) could
  still rewrite them.
- The per-IP limiter is per process (R9); the per-factor counters in the database are the control, and since 0138
  the hard-lock budget cannot be reset by the runtime role (`SF008`).

## 9. S4b deploy note (ADR 0119 S4b-R8, S4b-R12)

- S4b changes the staff session seal format to v2 (it binds the refresh token hash and the idle expiry, condition
  S3-R10). A session sealed by S3 (v1) is never accepted again: at the S4b deploy **every live staff session ends**.
  Staff see `401 session_revoked` (refresh: `401`) and must log in again. This is expected and raises **no** alert and
  no `auth.session_assurance_invalid` audit row (the liveness reason `seal_legacy` is counted). Announce the re-login
  in the change ticket.
- 12 hours after the deploy (the absolute-TTL ceiling) no genuine v1 session can still exist. The quiet legacy path
  has no sunset in S4b (S4b-R12): until S9 turns it into an alerting `bad_seal`, treat any `seal_legacy` count after
  that point as a possible forgery and investigate.
- Carry both items into the S9 deploy runbook list.
