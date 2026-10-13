#!/usr/bin/env python3
"""ADR 0119 S2 mutation driver.

Usage: adr0119-s2-mutation-driver.py <worktree> <sandbox-parent> <out-file> [ids,comma,separated] [workers]

Each worker owns a private copy of the worktree (go.mod, go.sum, cmd, internal, migrations, deploy) and, for SQL
mutants, a private database that is REBUILT from the mutated migrations (priv_db.sh) before the integration suite
of ./internal/staffmfa/store runs against it. A mutant is KILLED when the run fails, SURVIVED when it passes,
ERROR when the pattern is not found exactly once or the build/setup fails (ERROR is never evidence).

kinds
  go   mutates Go source; suite = store package (integration) minus the scratch-database migration tests,
       against a stable, unmutated private database (shared by the go workers)
  sql  mutates migration 0137 (up or down); rebuilds a private DB; suite = the whole store package incl. the
       scratch-database migration tests (they build their databases from the SAME mutated migration files)

Database use: only private databases created by this script (names below) through the sanctioned
priv_db.sh / priv_test.sh harness; they are dropped at the end. No role, password or privilege change.
"""
import concurrent.futures as cf
import pathlib
import re
import shutil
import subprocess
import sys
import time

SCRATCH = "/tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad"
PRIV_DB_SH = f"{SCRATCH}/priv_db.sh"
PRIV_TEST_SH = f"{SCRATCH}/priv_test.sh"
STABLE_DB = "adr0119_s2_a1"

wt = pathlib.Path(sys.argv[1])
parent = pathlib.Path(sys.argv[2])
out = pathlib.Path(sys.argv[3])
only = set(sys.argv[4].split(",")) if len(sys.argv) > 4 and sys.argv[4] else None
workers = int(sys.argv[5]) if len(sys.argv) > 5 else 3

F = "internal/staffmfa/store/factor.go"
R = "internal/staffmfa/store/recovery.go"
C = "internal/staffmfa/store/challenge.go"
E = "internal/staffmfa/store/enrolment.go"
D = "internal/db/staff_mfa_lookup.go"
UP = "migrations/0137_staff_mfa.up.sql"
DOWN = "migrations/0137_staff_mfa.down.sql"


def after(anchor, old, new):
    """Replace the first `old` that follows `anchor` (for statements repeated per table)."""
    def fn(src):
        i = src.index(anchor)
        j = src.index(old, i)
        return src[:j] + new + src[j + len(old):]
    return fn


# (id, kind, file, old|callable, new, description)
M = [
    # ---- Go: factor.go ----
    ("G01", "go", F, "AND factor_type = 'totp' AND status = 'active'\n\t\tFOR UPDATE`, staff.StaffID", "AND factor_type = 'totp' AND status = 'active'`, staff.StaffID", "lockActive: FOR UPDATE dropped"),
    ("G02", "go", F, "AND factor_type = 'totp' AND status = 'pending'\n\t\tFOR UPDATE`, id,", "AND factor_type = 'totp' AND status = 'pending'`, id,", "lockPending: FOR UPDATE dropped"),
    ("G03", "go", F, "WHERE id = $1 AND status = 'active' AND last_used_step < $2`,\n\t\tid, step,", "WHERE id = $1 AND status = 'active'`,\n\t\tid, step,", "burnStep: the conditional (last_used_step < step) dropped"),
    ("G03b", "go", F, "WHERE id = $1 AND status = 'pending' AND last_used_step < $2`, row.id, step, now.UTC())", "WHERE id = $1 AND status = 'pending'`, row.id, step, now.UTC())", "activate: the conditional dropped"),
    ("G04", "go", F, "return hard, persistCounters(ctx, tx, row.id, next)", "_ = next\n\t\treturn hard, nil", "recordOutcome: failure counters not persisted"),
    ("G05", "go", F, "\t\tif burned {\n", "\t\tif burned || true {\n", "VerifyDetailed: the burn result ignored"),
    ("G06", "go", F, "AND factor_type = 'totp' AND status = 'active'\n\t\tFOR UPDATE`, staff.StaffID", "AND factor_type = 'totp'\n\t\tFOR UPDATE`, staff.StaffID", "lockActive: status = 'active' dropped"),
    ("G07", "go", F, "WHERE staff_user_id = $1 AND tenant_id IS NOT DISTINCT FROM $2::uuid\n\t\t  AND factor_type = 'totp' AND status = 'active'\n\t\tFOR UPDATE", "WHERE staff_user_id = $1 AND ($2::uuid IS NULL OR $2::uuid IS NOT NULL)\n\t\t  AND factor_type = 'totp' AND status = 'active'\n\t\tFOR UPDATE", "lockActive: tenant predicate dropped"),
    ("G08", "go", F, "SELECT email, status FROM staff_users WHERE id = $1 AND tenant_id IS NOT DISTINCT FROM $2::uuid", "SELECT email, status FROM staff_users WHERE id = $1 AND ($2::uuid IS NULL OR $2::uuid IS NOT NULL)", "Begin: staff tenant predicate dropped"),
    ("G09", "go", F, "AND factor_type = 'totp' AND status = 'pending'`,\n\t\tstaff.StaffID, tenantArg(staff.TenantID), now.UTC()); err != nil {", "AND factor_type = 'totp' AND status = 'nonexistent'`,\n\t\tstaff.StaffID, tenantArg(staff.TenantID), now.UTC()); err != nil {", "Begin: earlier pending factor not superseded"),
    ("G10", "go", F, "THEN 'pending_expired' ELSE 'pending_superseded' END", "THEN 'pending_superseded' ELSE 'pending_expired' END", "Begin: revocation reasons swapped"),
    ("G11", "go", F, "revoked_reason = 'replaced'\n\t\tWHERE staff_user_id = $1 AND tenant_id IS NOT DISTINCT FROM $2::uuid AND factor_type = 'totp' AND status = 'active'", "revoked_reason = 'replaced'\n\t\tWHERE staff_user_id = $1 AND tenant_id IS NOT DISTINCT FROM $2::uuid AND factor_type = 'totp' AND status = 'nonexistent'", "Confirm: previous active factor not revoked"),
    ("G13", "go", F, "if !now.Before(row.pendingExpires) {", "if now.After(row.pendingExpires) {", "Confirm: pending expiry boundary (>= to >)"),
    ("G14", "go", F, "case staffmfa.OutcomeRejected, staffmfa.OutcomeIntegrity:", "case staffmfa.OutcomeRejected:", "recordOutcome: an integrity failure does not count"),
    ("G15", "go", F, "IntegrityFailure: res.Outcome == staffmfa.OutcomeIntegrity", "IntegrityFailure: false", "VerifyDetailed: integrity flag never set"),
    ("G16", "go", F, "\tcase staffmfa.OutcomeLocked:\n\t\treturn ErrLocked", "\tcase staffmfa.OutcomeLocked:\n\t\treturn ErrRejected", "refusalFor: locked reported as rejected"),
    ("G16b", "go", F, "\tcase staffmfa.OutcomeHardLocked:\n\t\treturn ErrHardLocked", "\tcase staffmfa.OutcomeHardLocked:\n\t\treturn ErrLocked", "refusalFor: hard lock reported as soft"),
    ("G16c", "go", F, "\tcase staffmfa.OutcomeIntegrity:\n\t\treturn ErrIntegrity", "\tcase staffmfa.OutcomeIntegrity:\n\t\treturn ErrRejected", "refusalFor: integrity reported as rejected"),
    ("G17", "go", F, "\tif tenant != nil {\n\t\tf.tenant = *tenant\n\t}", "\t_ = tenant", "scanFactor: tenant dropped from the AAD (platform and tenant ciphertext interchangeable)"),
    # ---- Go: recovery.go ----
    ("R01", "go", R, "switch row.counters.LockState(now) {", "switch staffmfa.LockOpen {", "ConsumeRecoveryCode: lock state not checked before the lookup"),
    ("R02", "go", R, "WHERE id = $1 AND consumed_at IS NULL AND revoked_at IS NULL`, id, now.UTC())", "WHERE id = $1`, id, now.UTC())", "consumeCode: single-use condition dropped"),
    ("R03", "go", R, "AND consumed_at IS NULL AND revoked_at IS NULL`, staff.StaffID, tenantArg(staff.TenantID), macs)", "`, staff.StaffID, tenantArg(staff.TenantID), macs)", "consumeMatching: live-code filter dropped (second layer behind consumeCode)"),
    ("R04", "go", R, "UPDATE staff_mfa_recovery_codes SET revoked_at = $2\n\t\tWHERE staff_user_id = $1 AND consumed_at IS NULL", "UPDATE staff_mfa_recovery_codes SET revoked_at = $2\n\t\tWHERE staff_user_id = $1 AND consumed_at IS NOT NULL", "IssueRecoveryCodes: the old batch is not revoked"),
    ("R05", "go", R, "\tif _, err := lockActive(ctx, tx, staff); err != nil {\n\t\treturn staffmfa.RecoveryCodeBatch{}, err\n\t}\n", "", "IssueRecoveryCodes: the factor lock / active-factor requirement dropped"),
    ("R06", "go", R, "persistCounters(ctx, tx, row.id, row.counters.RecordSuccess())", "persistCounters(ctx, tx, row.id, row.counters)", "ConsumeRecoveryCode: a success does not reset the short window"),
    ("R07", "go", R, "AND code_mac = ANY($3)", "AND (code_mac = ANY($3) OR true)", "consumeMatching: MAC match dropped from the lookup (verify layer must hold)"),
    # ---- Go: challenge.go ----
    ("C01", "go", C, "cons != nil || c.AttemptsLeft <= 0 || !now.Before(c.ExpiresAt)", "c.AttemptsLeft <= 0 || !now.Before(c.ExpiresAt)", "LockChallenge: consumed check dropped"),
    ("C02", "go", C, "cons != nil || c.AttemptsLeft <= 0 || !now.Before(c.ExpiresAt)", "cons != nil || c.AttemptsLeft <= 0 || now.After(c.ExpiresAt)", "LockChallenge: expiry boundary (>= to >)"),
    ("C03", "go", C, "cons != nil || c.AttemptsLeft <= 0 || !now.Before(c.ExpiresAt)", "cons != nil || !now.Before(c.ExpiresAt)", "LockChallenge: attempts-left check dropped"),
    ("C04", "go", C, "consumed_at = CASE WHEN attempts_left - 1 = 0 THEN $2::timestamptz ELSE NULL END", "consumed_at = NULL::timestamptz", "DecrementChallengeAttempts: exhaustion does not consume"),
    ("C05", "go", C, "UPDATE staff_auth_challenges SET consumed_at = $2 WHERE id = $1 AND consumed_at IS NULL`", "UPDATE staff_auth_challenges SET consumed_at = $2 WHERE id = $1`", "ConsumeChallenge: single-use condition dropped"),
    ("C06", "go", C, "WHERE id = $1 AND consumed_at IS NULL AND attempts_left > 0\n\t\tRETURNING", "WHERE id = $1\n\t\tRETURNING", "DecrementChallengeAttempts: conditions dropped"),
    ("C07", "go", C, "staffmfa.ChallengeAttempts, now.Add(staffmfa.ChallengeTTL).UTC()", "3, now.Add(staffmfa.ChallengeTTL).UTC()", "IssueChallenge: attempts"),
    ("C08", "go", C, "staffmfa.ChallengeAttempts, now.Add(staffmfa.ChallengeTTL).UTC()", "staffmfa.ChallengeAttempts, now.Add(2*staffmfa.ChallengeTTL).UTC()", "IssueChallenge: TTL"),
    ("C09", "go", C, "!s.keys.VerifyTokenHash(kind, tok, ref.Staff.StaffID, staffmfa.KeyedHash{KID: rowKID, MAC: rowMAC})", "false", "ResolveChallenge: token verification dropped (MAC equality of the lookup is the first layer)"),
    ("C10", "go", C, "rowPurpose != string(purpose) ||", "", "ResolveChallenge: purpose column compare dropped (the purpose is also inside the MAC)"),
    ("C11", "go", C, "\tcase ChallengeEnrol:\n\t\treturn staffmfa.MACChallengeEnrol, true", "\tcase ChallengeEnrol:\n\t\treturn staffmfa.MACChallengeMFA, true", "challenge kinds: enrol uses the mfa MAC kind (the purpose column must not be the only separation)"),
    ("C12", "go", C, "!s.keys.VerifyTokenHash(kind, tok, c.Staff.StaffID, staffmfa.KeyedHash{KID: kid, MAC: mac}) ||\n\t\tcons != nil", "cons != nil", "LockChallenge: token verification dropped"),
    ("C13", "go", C, "WHERE staff_user_id = $1 AND purpose = $2 AND token_mac = ANY($3)\n\t\tFOR UPDATE", "WHERE staff_user_id = $1 AND purpose = $2 AND token_mac = ANY($3)", "LockChallenge: FOR UPDATE dropped"),
    # ---- Go: enrolment.go ----
    ("E01", "go", E, "consumed != nil || rv != nil || !now.Before(t.ExpiresAt)", "consumed != nil || !now.Before(t.ExpiresAt)", "LockEnrolmentToken: revoked check dropped"),
    ("E02", "go", E, "WHERE id = $1 AND consumed_at IS NULL AND revoked_at IS NULL`, id, now.UTC())", "WHERE id = $1 AND consumed_at IS NULL`, id, now.UTC())", "ConsumeEnrolmentToken: revoked condition dropped"),
    ("E03", "go", E, "purpose = $2 AND consumed_at IS NULL AND revoked_at IS NULL`,\n\t\tstaff.StaffID, string(purpose), now.UTC())", "purpose = $2 AND false`,\n\t\tstaff.StaffID, string(purpose), now.UTC())", "IssueEnrolmentToken: the live token is not revoked"),
    ("E04", "go", E, "consumed != nil || rv != nil || !now.Before(t.ExpiresAt)", "consumed != nil || rv != nil || now.After(t.ExpiresAt)", "LockEnrolmentToken: expiry boundary"),
    ("E05", "go", E, "WHERE id = $1 AND consumed_at IS NULL AND revoked_at IS NULL`, id, now.UTC())", "WHERE id = $1 AND revoked_at IS NULL`, id, now.UTC())", "ConsumeEnrolmentToken: single-use condition dropped"),
    ("E06", "go", E, "\tcase EnrolBootstrap:\n\t\treturn staffmfa.MACEnrolBootstrap, true", "\tcase EnrolBootstrap:\n\t\treturn staffmfa.MACEnrolInvite, true", "enrolment kinds: bootstrap uses the invite MAC kind"),
    ("E08", "go", E, "WHERE staff_user_id = $1 AND purpose = $2 AND token_mac = ANY($3)\n\t\tFOR UPDATE", "WHERE staff_user_id = $1 AND purpose = $2 AND token_mac = ANY($3)", "LockEnrolmentToken: FOR UPDATE dropped"),
    ("E07", "go", E, "!s.keys.VerifyTokenHash(kind, tok, t.Staff.StaffID, staffmfa.KeyedHash{KID: kid, MAC: mac}) ||\n\t\tconsumed != nil", "consumed != nil", "LockEnrolmentToken: token verification dropped"),
    # ---- Go: db helper ----
    ("D01", "go", D, "\tif len(s) != staffLookupMACHexLen {\n\t\treturn false\n\t}", "", "lookup helper: length check dropped"),
    ("D02", "go", D, "(c < 'a' || c > 'f')", "(c < 'a' || c > 'g')", "lookup helper: hex alphabet widened"),
    # ---- SQL: guard functions ----
    ("Q01", "sql", UP, "    IF NEW.tenant_id IS NOT NULL AND NEW.tenant_id IS DISTINCT FROM v_tenant THEN", "    IF false THEN", "SF001: a differing supplied tenant admitted"),
    ("Q02", "sql", UP, "    IF NOT FOUND THEN\n        RAISE EXCEPTION '%: the staff member is not visible", "    IF false THEN\n        RAISE EXCEPTION '%: the staff member is not visible", "SF001: an invisible staff member admitted"),
    ("Q03", "sql", UP, "    NEW.tenant_id := v_tenant;\n    RETURN NEW;", "    RETURN NEW;", "SF001: tenant not forced"),
    ("Q04", "sql", UP, "RAISE EXCEPTION '%: rows are never deleted (ADR 0119, SF099)', TG_TABLE_NAME USING ERRCODE = 'SF099';", "RETURN OLD;", "SF099: DELETE allowed"),
    ("Q05", "sql", UP, "RAISE EXCEPTION '%: TRUNCATE is refused (ADR 0119, SF099)', TG_TABLE_NAME USING ERRCODE = 'SF099';", "RETURN NULL;", "SF099: TRUNCATE allowed"),
    ("Q06", "sql", UP, "    IF NEW.id IS DISTINCT FROM OLD.id THEN v_changed := v_changed || 'id'::text; END IF;\n", "", "SF002: factor id mutable"),
    ("Q07", "sql", UP, "    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id THEN v_changed := v_changed || 'tenant_id'::text; END IF;\n", "", "SF002: factor tenant_id mutable"),
    ("Q08", "sql", UP, "    IF NEW.staff_user_id IS DISTINCT FROM OLD.staff_user_id THEN v_changed := v_changed || 'staff_user_id'::text; END IF;\n", "", "SF002: factor staff_user_id mutable"),
    ("Q09", "sql", UP, "    IF NEW.factor_type IS DISTINCT FROM OLD.factor_type THEN v_changed := v_changed || 'factor_type'::text; END IF;\n", "", "SF002: factor_type mutable"),
    ("Q10", "sql", UP, "    IF NEW.secret_kid IS DISTINCT FROM OLD.secret_kid THEN v_changed := v_changed || 'secret_kid'::text; END IF;\n", "", "SF002: secret_kid mutable"),
    ("Q11", "sql", UP, "    IF NEW.secret_nonce IS DISTINCT FROM OLD.secret_nonce THEN v_changed := v_changed || 'secret_nonce'::text; END IF;\n", "", "SF002: secret_nonce mutable"),
    ("Q12", "sql", UP, "    IF NEW.secret_ct IS DISTINCT FROM OLD.secret_ct THEN v_changed := v_changed || 'secret_ct'::text; END IF;\n", "", "SF002: secret_ct mutable"),
    ("Q13", "sql", UP, "    IF NEW.created_at IS DISTINCT FROM OLD.created_at THEN v_changed := v_changed || 'created_at'::text; END IF;\n", "", "SF002: factor created_at mutable"),
    ("Q14", "sql", UP, "    IF NEW.last_used_step < OLD.last_used_step THEN", "    IF false THEN", "SF003: last_used_step may decrease"),
    ("Q15", "sql", UP, "OR (OLD.status = 'active' AND NEW.status = 'revoked')) THEN", "OR (OLD.status = 'active' AND NEW.status IN ('revoked', 'pending'))) THEN", "SF002: active -> pending admitted"),
    ("Q16", "sql", UP, "    ELSIF OLD.status = 'revoked' AND ROW(NEW.*) IS DISTINCT FROM ROW(OLD.*) THEN", "    ELSIF false THEN", "SF002: a revoked factor is not frozen"),
    ("Q17", "sql", UP, "    IF OLD.activated_at IS NOT NULL AND NEW.activated_at IS DISTINCT FROM OLD.activated_at THEN", "    IF false THEN", "SF002: activated_at not write-once"),
    ("Q18", "sql", UP, "    IF OLD.hard_locked_at IS NOT NULL AND NEW.hard_locked_at IS DISTINCT FROM OLD.hard_locked_at THEN", "    IF false THEN", "SF002: hard_locked_at not write-once"),
    ("Q19", "sql", UP, "    IF OLD.revoked_at IS NOT NULL AND (NEW.revoked_at IS DISTINCT FROM OLD.revoked_at OR NEW.revoked_reason IS DISTINCT FROM OLD.revoked_reason) THEN", "    IF false THEN", "SF002: revocation not write-once (second layer behind the frozen-revoked branch)"),
    ("Q20", "sql", UP, "    IF NEW.pending_expires_at IS NOT NULL AND NEW.pending_expires_at IS DISTINCT FROM OLD.pending_expires_at THEN", "    IF false THEN", "SF002: pending expiry extendable"),
    ("Q21", "sql", UP, "OR NEW.code_kid IS DISTINCT FROM OLD.code_kid OR NEW.code_mac IS DISTINCT FROM OLD.code_mac", "OR NEW.code_kid IS DISTINCT FROM OLD.code_kid", "SF004: code_mac mutable"),
    ("Q22", "sql", UP, "RAISE EXCEPTION 'staff_mfa_recovery_codes: consumed_at and revoked_at are write-once (SF004)' USING ERRCODE = 'SF004';", "NULL;", "SF004: consumed_at / revoked_at not write-once"),
    ("Q23", "sql", UP, "RAISE EXCEPTION 'staff_mfa_recovery_codes: a code is consumed or revoked, never both (SF004)' USING ERRCODE = 'SF004';", "NULL;", "SF004: both consumed and revoked (the CHECK raises a different code)"),
    ("Q24", "sql", UP, "    IF NEW.attempts_left > OLD.attempts_left THEN", "    IF false THEN", "SF005: attempts may increase"),
    ("Q25", "sql", UP, "RAISE EXCEPTION 'staff_auth_challenges: a consumed challenge is frozen (SF005)' USING ERRCODE = 'SF005';", "NULL;", "SF005: consumed challenge not frozen"),
    ("Q26", "sql", UP, "OR NEW.token_kid IS DISTINCT FROM OLD.token_kid OR NEW.token_mac IS DISTINCT FROM OLD.token_mac\n       OR NEW.staff_version_at_issue", "OR NEW.token_kid IS DISTINCT FROM OLD.token_kid\n       OR NEW.staff_version_at_issue", "SF005: challenge token_mac mutable"),
    ("Q27", "sql", UP, "OR NEW.expires_at IS DISTINCT FROM OLD.expires_at OR NEW.issued_by IS DISTINCT FROM OLD.issued_by", "OR NEW.expires_at IS DISTINCT FROM OLD.expires_at", "SF006: issued_by mutable"),
    ("Q28", "sql", UP, "RAISE EXCEPTION 'staff_enrolment_tokens: a token is consumed or revoked, never both (SF006)' USING ERRCODE = 'SF006';", "NULL;", "SF006: both consumed and revoked (the CHECK raises a different code)"),
    ("Q29", "sql", UP, "    CONSTRAINT staff_mfa_factors_hard_lock_consistent CHECK (hard_locked_at IS NOT NULL OR failed_24h_count < 10),\n", "", "C4 CHECK dropped"),
    ("Q30", "sql", UP, "CHECK (failed_count >= 0 AND failed_count < 5)", "CHECK (failed_count >= 0)", "failed_count upper bound dropped"),
    ("Q31", "sql", UP, "    CONSTRAINT staff_mfa_factors_pending_expiry CHECK ((status = 'pending') = (pending_expires_at IS NOT NULL)),\n", "", "pending-expiry CHECK dropped"),
    ("Q32", "sql", UP, "CREATE UNIQUE INDEX staff_mfa_factors_one_active", "CREATE INDEX staff_mfa_factors_one_active", "one-active unique index made non-unique"),
    ("Q33", "sql", UP, "CREATE UNIQUE INDEX staff_mfa_factors_one_pending", "CREATE INDEX staff_mfa_factors_one_pending", "one-pending unique index made non-unique"),
    ("Q34", "sql", UP, "CREATE UNIQUE INDEX staff_enrolment_tokens_one_live", "CREATE INDEX staff_enrolment_tokens_one_live", "one-live-token unique index made non-unique"),
    ("Q35", "sql", UP, "code_mac      BYTEA NOT NULL UNIQUE CHECK", "code_mac      BYTEA NOT NULL CHECK", "recovery code_mac UNIQUE dropped"),
    ("Q36", "sql", UP, "token_mac             BYTEA NOT NULL UNIQUE CHECK (octet_length(token_mac) = 32),\n    staff_version_at_issue", "token_mac             BYTEA NOT NULL CHECK (octet_length(token_mac) = 32),\n    staff_version_at_issue", "challenge token_mac UNIQUE dropped"),
    # ---- SQL: RLS ----
    ("Q37", "sql", UP, after("CREATE POLICY dual_scope_isolation ON staff_mfa_factors FOR ALL", "OR (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)", "OR (tenant_id IS NULL)"), None, "factors USING: platform arm visible from tenant sessions"),
    ("Q38", "sql", UP, after("CREATE POLICY dual_scope_isolation ON staff_auth_challenges FOR ALL", "OR (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)", "OR (tenant_id IS NULL)"), None, "challenges USING: platform arm visible from tenant sessions"),
    ("Q39", "sql", UP, after("CREATE POLICY dual_scope_isolation ON staff_mfa_recovery_codes FOR ALL\n    USING (", "    WITH CHECK (", "    WITH CHECK (true OR "), None, "recovery codes WITH CHECK weakened"),
    ("Q40", "sql", UP, "ALTER TABLE staff_mfa_recovery_codes FORCE ROW LEVEL SECURITY;", "", "FORCE ROW LEVEL SECURITY dropped on recovery codes"),
    ("Q41", "sql", UP, "        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL\n    );\nCREATE POLICY staff_enrolment_select_by_mac", "    );\nCREATE POLICY staff_enrolment_select_by_mac", "challenge lookup policy: the no-tenant conjunct dropped"),
    ("Q42", "sql", UP, "token_mac = decode(NULLIF(current_setting('app.staff_enrolment_lookup_mac', true), ''), 'hex')\n        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL", "token_mac = decode(NULLIF(current_setting('app.staff_enrolment_lookup_mac', true), ''), 'hex')", "enrolment lookup policy: the no-tenant conjunct dropped"),
    ("Q43", "sql", UP, "CREATE POLICY acting_fence_select ON %I AS RESTRICTIVE FOR SELECT USING (NOT financial_acting_gucs_present())", "CREATE POLICY acting_fence_select ON %I AS RESTRICTIVE FOR SELECT USING (true)", "acting fence (select) open"),
    ("Q44", "sql", UP, "CREATE POLICY acting_fence_insert ON %I AS RESTRICTIVE FOR INSERT WITH CHECK (NOT financial_acting_gucs_present())", "CREATE POLICY acting_fence_insert ON %I AS RESTRICTIVE FOR INSERT WITH CHECK (true)", "acting fence (insert) open"),
    ("Q45", "sql", UP, "FOR UPDATE USING (NOT financial_acting_gucs_present()) WITH CHECK (NOT financial_acting_gucs_present())", "FOR UPDATE USING (true) WITH CHECK (true)", "acting fence (update) open"),
    ("Q46", "sql", UP, "CREATE POLICY kyc_worker_fence_select ON %I AS RESTRICTIVE FOR SELECT USING (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker')", "CREATE POLICY kyc_worker_fence_select ON %I AS RESTRICTIVE FOR SELECT USING (true)", "KYC worker fence (select) open"),
    ("Q47", "sql", UP, "        EXECUTE format($f$CREATE POLICY kyc_worker_fence_delete ON %I AS RESTRICTIVE FOR DELETE USING (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker')$f$, t);\n", "", "KYC worker fence (delete) missing"),
    ("Q48", "sql", UP, "CREATE POLICY acting_fence_delete ON %I AS RESTRICTIVE FOR DELETE USING (NOT financial_acting_gucs_present())", "CREATE POLICY acting_fence_delete ON %I AS RESTRICTIVE FOR DELETE USING (true)", "acting fence (delete) open"),
    # ---- SQL: grants ----
    ("Q49", "sql", UP, "EXECUTE 'GRANT SELECT, INSERT ON staff_mfa_factors TO igaming_runtime';", "EXECUTE 'GRANT SELECT, INSERT, DELETE ON staff_mfa_factors TO igaming_runtime';", "runtime may DELETE factors"),
    ("Q50", "sql", UP, "GRANT UPDATE (status, last_used_step,", "GRANT UPDATE (secret_ct, status, last_used_step,", "runtime may UPDATE secret_ct"),
    ("Q51", "sql", UP, "        EXECUTE 'REVOKE ALL ON staff_mfa_recovery_codes FROM igaming_runtime';\n", "", "default privileges left on recovery codes (table-level UPDATE / DELETE)"),
    ("Q52", "sql", UP, "EXECUTE 'GRANT UPDATE (consumed_at, revoked_at) ON staff_enrolment_tokens TO igaming_runtime';", "EXECUTE 'GRANT UPDATE (consumed_at, revoked_at, token_mac) ON staff_enrolment_tokens TO igaming_runtime';", "runtime may UPDATE token_mac"),
    # ---- SQL: hygiene ----
    ("Q53", "sql", UP, "LANGUAGE plpgsql\n    SET search_path = pg_catalog, public, pg_temp\nAS $$\nBEGIN\n    RAISE EXCEPTION '%: rows are never deleted", "LANGUAGE plpgsql\nAS $$\nBEGIN\n    RAISE EXCEPTION '%: rows are never deleted", "sf_deny_delete: search_path not pinned"),
    ("Q54", "sql", UP, "CREATE TRIGGER sf_no_truncate BEFORE TRUNCATE ON staff_mfa_factors\n    FOR EACH STATEMENT EXECUTE FUNCTION sf_deny_truncate();\n", "", "sf_no_truncate trigger missing on factors"),
    ("Q55", "sql", UP, "CREATE TRIGGER sf_factor_guard BEFORE UPDATE ON staff_mfa_factors", "CREATE TRIGGER sf_factor_guard AFTER UPDATE ON staff_mfa_factors", "factor guard is AFTER, not BEFORE"),
    ("Q56", "sql", UP, "CREATE TRIGGER sf_challenge_tenant_forced BEFORE INSERT ON staff_auth_challenges\n    FOR EACH ROW EXECUTE FUNCTION sf_force_tenant_from_staff();\n", "", "SF001 trigger missing on challenges"),
    ("Q57", "sql", UP, "CREATE FUNCTION sf_force_tenant_from_staff() RETURNS trigger\n    LANGUAGE plpgsql", "CREATE FUNCTION sf_force_tenant_from_staff() RETURNS trigger\n    LANGUAGE plpgsql SECURITY DEFINER", "SF001 function SECURITY DEFINER"),
    # ---- SQL: pre-flight and down ----
    ("Q66", "sql", UP, "FOR ALL USING (NULLIF(current_setting('app.player_account_id', true), '') IS NULL) WITH CHECK (NULLIF(current_setting('app.player_account_id', true), '') IS NULL)", "FOR ALL USING (true) WITH CHECK (true)", "player scope fence open"),
    ("Q67", "sql", UP, "FOR ALL USING (NULLIF(current_setting('app.platform_service_id', true), '') IS NULL) WITH CHECK (NULLIF(current_setting('app.platform_service_id', true), '') IS NULL)", "FOR ALL USING (true) WITH CHECK (true)", "platform service fence open"),
    ("Q68", "sql", UP, "        EXECUTE format($f$CREATE POLICY player_scope_fence", "        PERFORM 1; -- EXECUTE format($f$CREATE POLICY player_scope_fence", "player scope fence missing"),
    ("Q69", "sql", UP, "FOR ALL USING (NULLIF(current_setting('app.platform_service_id', true), '') IS NULL) WITH CHECK (NULLIF(current_setting('app.platform_service_id', true), '') IS NULL)", "FOR ALL USING (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker') WITH CHECK (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker')", "platform service fence narrowed to the KYC worker only (the F1 gap)"),
    ("Q58", "sql", UP, "IF v_forced IS DISTINCT FROM true OR v_dual <> 1 OR", "IF v_dual <> 1 OR", "pre-flight: FORCE RLS check dropped"),
    ("Q59", "sql", UP, "OR pg_catalog.cardinality(v_extra) > 0 OR v_fn <> 1 THEN", "OR v_fn <> 1 THEN", "pre-flight: unexpected-fence check dropped"),
    ("Q60", "sql", UP, "OR pg_catalog.cardinality(v_missing) > 0", "OR false", "pre-flight: missing-fence check dropped"),
    ("Q61", "sql", DOWN, "    SELECT count(*) INTO v_tokens FROM staff_enrolment_tokens;", "    v_tokens := 0;", "down: enrolment tokens not counted"),
    ("Q62", "sql", DOWN, "    SELECT count(*) INTO v_factors FROM staff_mfa_factors;", "    v_factors := 0;", "down: factors not counted"),
    ("Q63", "sql", DOWN, "ALTER TABLE staff_mfa_factors NO FORCE ROW LEVEL SECURITY;\n", "", "down: FORCE RLS not lifted for the count (RLS-blind count)"),
    ("Q63b", "sql", DOWN, "ALTER TABLE staff_mfa_recovery_codes NO FORCE ROW LEVEL SECURITY;\n", "", "down: FORCE RLS not lifted for the recovery-code count"),
    ("Q63c", "sql", DOWN, "ALTER TABLE staff_auth_challenges NO FORCE ROW LEVEL SECURITY;\n", "", "down: FORCE RLS not lifted for the challenge count"),
    ("Q63d", "sql", DOWN, "ALTER TABLE staff_enrolment_tokens NO FORCE ROW LEVEL SECURITY;\n", "", "down: FORCE RLS not lifted for the token count"),
    ("Q64", "sql", DOWN, "    IF v_factors + v_codes + v_challenges + v_tokens > 0 THEN", "    IF false THEN", "down: never refuses"),
    ("Q65", "sql", UP, "    IF v_forced IS DISTINCT FROM true OR v_dual <> 1 OR pg_catalog.cardinality(v_missing) > 0", "    IF false OR v_dual <> 1 OR pg_catalog.cardinality(v_missing) > 0", "pre-flight: FORCE RLS check made vacuous"),
]


GOCACHE = pathlib.Path(SCRATCH) / "s2" / "gocache-mut"
INFRA = ("recovery mode", "no such file or directory", "the database system is starting up", "connection refused",
         "too many clients", "could not connect", "permission denied to terminate process", "sorry, too many")


def sh(cmd, env_extra, cwd, timeout):
    import os
    env = dict(os.environ)
    env["GOCACHE"] = str(GOCACHE)  # private build cache: a restarted container cannot leave a torn shared one in play
    env.update(env_extra)
    p = subprocess.run(cmd, cwd=cwd, env=env, capture_output=True, text=True, timeout=timeout)
    return p.returncode, p.stdout + p.stderr


def make_sandbox(path):
    if path.exists():
        shutil.rmtree(path)
    path.mkdir(parents=True)
    for name in ("go.mod", "go.sum"):
        shutil.copy2(wt / name, path / name)
    for d in ("cmd", "internal", "migrations", "deploy"):
        shutil.copytree(wt / d, path / d)


def run_suite(sandbox, db, skip_migration):
    args = ["bash", PRIV_TEST_SH, "-tags", "integration", "-count=1", "-p", "1", "./internal/staffmfa/store/"]
    if skip_migration:
        args += ["-skip", "Migration0137"]
    return sh(args, {"PRIV_DB": db, "PRIV_SRC": str(sandbox)}, str(sandbox), 1500)


def rebuild_db(sandbox, db):
    return sh(["bash", PRIV_DB_SH], {"PRIV_DB": db, "PRIV_SRC": str(sandbox)}, str(sandbox), 1500)


def classify(rc, o):
    if rc == 0:
        return "SURVIVED", ""
    if "[build failed]" in o or "[setup failed]" in o:
        return "ERROR", "build failed: " + " ".join(l for l in o.splitlines() if ".go:" in l)[:300]
    lines = o.splitlines()
    fails = []
    for i, l in enumerate(lines):
        if l.strip().startswith("--- FAIL") or l.startswith("panic:"):
            msg = ""
            for nxt in lines[i + 1:i + 4]:  # the assertion text under the FAIL line
                if nxt.startswith("    ") and ".go:" in nxt:
                    msg = " :: " + nxt.strip()[:160]
                    break
            fails.append(l.strip() + msg)
    if not fails and "FAIL" not in o:
        return "ERROR", "no result: " + o[-300:]
    return "KILLED", "; ".join(fails[:3])


def apply(src, old, new):
    if callable(old):
        try:
            return old(src), None
        except ValueError:
            return None, "anchor not found"
    n = src.count(old)
    if n != 1:
        return None, f"pattern found {n} times"
    return src.replace(old, new, 1), None


def work(idx, mutants, results):
    sandbox = parent / f"w{idx}"
    make_sandbox(sandbox)
    db = f"adr0119_s2_mut{idx}"
    sql_ready = False
    for mid, kind, f, old, new, desc in mutants:
        path = sandbox / f
        src = path.read_text()
        mutated, err = apply(src, old, new)
        if err:
            results[mid] = (f"{mid} | ERROR | {desc} | {err}")
            continue
        path.write_text(mutated)
        status, detail = "ERROR", ""
        try:
            for attempt in range(1, 5):  # an infrastructure fault (database restart, torn cache, lingering sessions) is retried, never counted
                if attempt > 1:
                    time.sleep(8)
                if kind == "sql":
                    rc, o = rebuild_db(sandbox, db)
                    if rc != 0:
                        status, detail = "ERROR", "migration of the mutant failed: " + " ".join(o.splitlines()[-3:])[:300]
                        if any(s in o for s in INFRA):
                            continue
                        break
                    rc, o = run_suite(sandbox, db, skip_migration=False)
                else:
                    rc, o = run_suite(sandbox, STABLE_DB, skip_migration=True)
                status, detail = classify(rc, o)
                if status == "ERROR" or (status == "KILLED" and any(s in o for s in INFRA)):
                    continue
                break
        finally:
            path.write_text(src)
        results[mid] = f"{mid} | {status} | {desc}" + (f"\n      {detail}" if detail else "")
    return idx


def main():
    muts = [m for m in M if (only is None or m[0] in only)]
    go = [m for m in muts if m[1] == "go"]
    sq = [m for m in muts if m[1] == "sql"]
    parent.mkdir(parents=True, exist_ok=True)
    # baseline (unmutated) first, so a red baseline cannot read as "killed"
    base = parent / "baseline"
    make_sandbox(base)
    rc, o = rebuild_db(base, "adr0119_s2_mutbase")
    lines = []
    if rc != 0:
        lines.append("BASELINE | ERROR | database rebuild failed\n" + o[-1500:])
    else:
        rc, o = run_suite(base, "adr0119_s2_mutbase", skip_migration=False)
        lines.append(f"BASELINE | {'PASS' if rc == 0 else 'FAIL'}")
        if rc != 0:
            lines.append(o[-2500:])
    out.write_text("\n".join(lines) + "\n")
    if rc != 0:
        print("\n".join(lines))
        return
    results = {}
    buckets = [[] for _ in range(workers)]
    # worker 0 takes the go mutants (stable DB, no rebuild); the sql mutants spread over the other workers
    buckets[0].extend(go)
    others = list(range(1, workers)) if workers > 1 else [0]
    for i, m in enumerate(sq):
        buckets[others[i % len(others)]].append(m)
    with cf.ThreadPoolExecutor(max_workers=workers) as ex:
        futs = [ex.submit(work, i, b, results) for i, b in enumerate(buckets) if b]
        for f in futs:
            f.result()
    ordered = [results[m[0]] for m in muts if m[0] in results]
    killed = sum(1 for l in ordered if "| KILLED |" in l)
    survived = sum(1 for l in ordered if "| SURVIVED |" in l)
    errors = sum(1 for l in ordered if "| ERROR |" in l)
    lines += ordered
    lines.append(f"SUMMARY | total={len(ordered)} killed={killed} survived={survived} error={errors}")
    out.write_text("\n".join(lines) + "\n")
    print("\n".join(lines))


main()
