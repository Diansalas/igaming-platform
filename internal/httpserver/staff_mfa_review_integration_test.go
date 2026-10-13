//go:build integration

package httpserver

import (
	"context"
	"encoding/base32"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/staffmfa/store"
)

// ADR 0119 S3 review conditions (security R1, R3; code review F1, F2) at the HTTP level.

// Security review R1 (executed by the reviewer under M5): un-revoking a logged-out staff session, or clearing the
// chain link of a rotated one, revived the old refresh token (200, aal2). Both columns are now write-once on staff
// rows (SF040): the SQL edit is refused for the runtime role and the owner, and the old refresh token stays dead.
func TestStaffMFA_RevocationAndChainLinkWriteOnce(t *testing.T) {
	w := newMFAW(t)
	s := w.staff(&w.tenant, "support")
	w.enrol(s)
	execs := map[string]func(mfaStaff, uuid.UUID, string, ...any) error{"runtime": w.runtimeSessionExec, "owner": w.ownerSessionExec}

	// Logout, then try to un-revoke.
	p := w.fullLogin(s)
	w.post("/v1/auth/logout", map[string]string{"refresh_token": p.RefreshToken}).want(t, http.StatusNoContent, "")
	rows := w.sessions(s)
	loggedOut := rows[len(rows)-1]
	if loggedOut.Revoked == nil {
		t.Fatal("logout must revoke the session")
	}
	for name, exec := range execs {
		if err := exec(s, loggedOut.ID, `UPDATE sessions SET revoked_at = NULL WHERE id = $1`); pgCode(err) != "SF040" {
			t.Fatalf("%s un-revoke: want SF040, got %v", name, err)
		}
	}
	w.refresh(p.RefreshToken).want(t, http.StatusUnauthorized, "unauthorized")

	// Rotate, then try to clear the old row's chain link so the old token rotates again.
	p = w.fullLogin(s)
	w.refresh(p.RefreshToken).want(t, http.StatusOK, "")
	var rotated mfaSessionRow
	for _, r := range w.sessions(s) {
		if r.ReplacedBy != nil && r.Revoked == nil {
			rotated = r
		}
	}
	if rotated.ID == uuid.Nil {
		t.Fatal("the rotated session must carry its chain link")
	}
	for name, exec := range execs {
		if err := exec(s, rotated.ID, `UPDATE sessions SET replaced_by_session_id = NULL WHERE id = $1`); pgCode(err) != "SF040" {
			t.Fatalf("%s chain-link reset: want SF040, got %v", name, err)
		}
	}
	// The old refresh token is a reuse: refused, and the chain (the rotated row and its successor) is revoked.
	w.refresh(p.RefreshToken).want(t, http.StatusUnauthorized, "unauthorized")
	if w.session(s, rotated.ID).Revoked == nil || w.session(s, *rotated.ReplacedBy).Revoked == nil {
		t.Fatal("reuse of a rotated token must revoke the chain")
	}
}

// Security review R3: a bootstrap token that meets an ACTIVE factor (planted under M5, or raced past cmd/seed-admin's
// check) is refused with the single invalid_enrolment_token answer, is not spent, and is audited and alerted (P1).
func TestStaffMFA_Redeem_BootstrapWithActiveFactorRefused(t *testing.T) {
	w := newMFAW(t)
	for name, s := range map[string]mfaStaff{"tenant": w.staff(&w.tenant, "support"), "platform": w.staff(nil, "platform_admin")} {
		t.Run(name, func(t *testing.T) {
			w.t = t
			kind := string(alerting.KindStaffMFAIntegrity)
			if s.ref.TenantID == uuid.Nil {
				kind = string(alerting.KindStaffPlatformMFAIntegrity)
			}
			w.enrol(s)
			before := w.activeFactor(s)
			tok := w.bootstrapToken(s)
			w.redeem(s, tok).want(t, http.StatusUnauthorized, "invalid_enrolment_token")
			var live int
			w.inScope(s, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT count(*) FROM staff_enrolment_tokens WHERE staff_user_id = $1 AND consumed_at IS NULL AND revoked_at IS NULL`, s.ref.StaffID).Scan(&live)
			})
			if live != 1 {
				t.Fatalf("the refused bootstrap token must not be spent: %d live", live)
			}
			if w.activeFactor(s).ID != before.ID {
				t.Fatal("the active factor must be untouched")
			}
			if w.alertCount(kind, s.ref.StaffID) != 1 || w.alertAttrs(kind, s.ref.StaffID)["cause"] != "bootstrap_with_active_factor" {
				t.Fatalf("%s must be raised with cause bootstrap_with_active_factor", kind)
			}
			found := false
			for _, m := range w.auditMetadata(s) {
				if strings.Contains(m, `"active_factor"`) {
					found = true
				}
			}
			if !found {
				t.Fatal("the refusal must be audited with reason active_factor")
			}
		})
	}
}

// startEnrol runs /enrol/start on an enrol challenge and records the pending factor's secret for w.code.
func (w *mfaW) startEnrol(s mfaStaff, enrolChallenge string) enrolStartWire {
	w.t.Helper()
	sr := w.post("/v1/staff/auth/enrol/start", map[string]string{"challenge_token": enrolChallenge})
	sr.want(w.t, http.StatusOK, "")
	var start enrolStartWire
	sr.decode(w.t, &start)
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(start.SecretBase32)
	if err != nil {
		w.t.Fatal(err)
	}
	w.mu.Lock()
	w.secrets[s.ref.StaffID] = secret
	w.steps[s.ref.StaffID] = 0
	w.mu.Unlock()
	return start
}

func (w *mfaW) suspend(s mfaStaff) {
	w.t.Helper()
	w.inScope(s, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE staff_users SET status = 'suspended' WHERE id = $1`, s.ref.StaffID)
		return err
	})
}

func (w *mfaW) liveRecoveryCodes(s mfaStaff) int {
	w.t.Helper()
	var n int
	w.inScope(s, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM staff_mfa_recovery_codes WHERE staff_user_id = $1 AND consumed_at IS NULL AND revoked_at IS NULL`, s.ref.StaffID).Scan(&n)
	})
	return n
}

// Code review F1: a staff member suspended between the first and the second step is refused (403 forbidden) at
// EVERY second-step endpoint - recover, enrol/start, enrol/confirm - with the challenge consumed and nothing written
// (no session, no factor, no recovery code consumed, no new challenge).
func TestStaffMFA_SuspendedBetweenSteps_EverySecondStep(t *testing.T) {
	w := newMFAW(t)
	t.Run("recover", func(t *testing.T) {
		w.t = t
		s := w.staff(&w.tenant, "support")
		e := w.enrol(s)
		ch := w.login(s)
		sessions, codes, challenges := len(w.sessions(s)), w.liveRecoveryCodes(s), w.countChallenges(s)
		w.suspend(s)
		w.post("/v1/staff/auth/mfa/recover", map[string]string{"challenge_token": ch, "recovery_code": e.codes[0]}).want(t, http.StatusForbidden, "forbidden")
		if _, consumed := w.challengeState(s, ch, store.ChallengeMFA); !consumed {
			t.Fatal("the challenge must be consumed")
		}
		if len(w.sessions(s)) != sessions || w.liveRecoveryCodes(s) != codes || w.countChallenges(s) != challenges {
			t.Fatal("nothing may be written for a suspended staff member")
		}
	})
	t.Run("enrol_start", func(t *testing.T) {
		w.t = t
		s := w.staff(&w.tenant, "support")
		ch := challengeOf(t, w.redeem(s, w.bootstrapToken(s)), "enrol")
		w.suspend(s)
		w.post("/v1/staff/auth/enrol/start", map[string]string{"challenge_token": ch}).want(t, http.StatusForbidden, "forbidden")
		if _, consumed := w.challengeState(s, ch, store.ChallengeEnrol); !consumed {
			t.Fatal("the challenge must be consumed")
		}
		if len(w.factors(s)) != 0 || len(w.sessions(s)) != 0 {
			t.Fatal("no factor and no session may be written")
		}
	})
	t.Run("enrol_confirm", func(t *testing.T) {
		w.t = t
		s := w.staff(&w.tenant, "support")
		ch := challengeOf(t, w.redeem(s, w.bootstrapToken(s)), "enrol")
		start := w.startEnrol(s, ch)
		w.suspend(s)
		w.post("/v1/staff/auth/enrol/confirm", map[string]string{"challenge_token": ch, "factor_id": start.FactorID, "code": w.code(s)}).
			want(t, http.StatusForbidden, "forbidden")
		if _, consumed := w.challengeState(s, ch, store.ChallengeEnrol); !consumed {
			t.Fatal("the challenge must be consumed")
		}
		for _, f := range w.factors(s) {
			if f.Status == "active" {
				t.Fatal("no factor may be activated")
			}
		}
		if len(w.sessions(s)) != 0 || w.liveRecoveryCodes(s) != 0 {
			t.Fatal("no session and no recovery codes may be written")
		}
	})
}

// Code review F2: gate G4 for /enrol/confirm - a 40001 at commit answers the same 503 for the right and a wrong code,
// and nothing persists (no factor activated, no session, no recovery codes, the challenge neither spent nor consumed,
// the pending factor's counters unchanged).
func TestStaffMFA_G4_EnrolConfirm(t *testing.T) {
	w := newMFAW(t)
	s := w.staff(&w.tenant, "support")
	ch := challengeOf(t, w.redeem(s, w.bootstrapToken(s)), "enrol")
	start := w.startEnrol(s, ch)
	pendingBefore := w.factors(s)
	w.m.commitFault = func(context.Context, pgx.Tx) error {
		return &pgconn.PgError{Code: "40001", Message: "could not serialize access due to concurrent update"}
	}
	t.Cleanup(func() { w.m.commitFault = nil })
	read := func(r mfaResp) [3]any {
		m := mapOf(t, r.body)
		return [3]any{r.status, m["code"], m["message"]}
	}
	right := read(w.post("/v1/staff/auth/enrol/confirm", map[string]string{"challenge_token": ch, "factor_id": start.FactorID, "code": w.code(s)}))
	wrong := read(w.post("/v1/staff/auth/enrol/confirm", map[string]string{"challenge_token": ch, "factor_id": start.FactorID, "code": w.wrong(s)}))
	if right != wrong || right[0] != http.StatusServiceUnavailable || right[1] != "service_unavailable" {
		t.Fatalf("fault answers must be identical 503s: right=%v wrong=%v", right, wrong)
	}
	if left, consumed := w.challengeState(s, ch, store.ChallengeEnrol); left != 5 || consumed {
		t.Fatalf("nothing persists on a failed commit: attempts_left=%d consumed=%v", left, consumed)
	}
	after := w.factors(s)
	if len(after) != len(pendingBefore) || after[0].Status != "pending" || after[0].FailedCount != pendingBefore[0].FailedCount ||
		after[0].LastUsedStep != pendingBefore[0].LastUsedStep {
		t.Fatalf("the pending factor must be unchanged: %+v -> %+v", pendingBefore, after)
	}
	if len(w.sessions(s)) != 0 || w.liveRecoveryCodes(s) != 0 {
		t.Fatal("no session and no recovery codes may persist")
	}
	// Control: without the fault the same confirm succeeds (the 503 was the fault, not the code).
	w.m.commitFault = nil
	w.post("/v1/staff/auth/enrol/confirm", map[string]string{"challenge_token": ch, "factor_id": start.FactorID, "code": w.code(s)}).want(t, http.StatusOK, "")
}

// The owner procedure of docs/runbooks/staff-mfa-s3-cutover.md section 5 (code review F4): between S3 and S6 a
// hard-locked (or device-and-codes-lost) staff member is restored by the owner revoking the factor and its recovery
// codes by SQL in the staff member's scope, then cmd/seed-admin -issue-bootstrap. The next password step answers
// 403 mfa_enrolment_required and raises the expected P1 staff.mfa_integrity; the staff member then re-enrols.
func TestStaffMFA_OwnerResetProcedure_S3toS6(t *testing.T) {
	w := newMFAW(t)
	for name, s := range map[string]mfaStaff{"tenant": w.staff(&w.tenant, "support"), "platform": w.staff(nil, "platform_admin")} {
		t.Run(name, func(t *testing.T) {
			w.t = t
			kind := string(alerting.KindStaffMFAIntegrity)
			if s.ref.TenantID == uuid.Nil {
				kind = string(alerting.KindStaffPlatformMFAIntegrity)
			}
			login := map[string]string{"email": s.email, "password": s.password}
			if s.slug != "" {
				login["tenant_slug"] = s.slug
			}
			w.enrol(s)
			w.inScope(s, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE staff_mfa_factors SET hard_locked_at = now() WHERE staff_user_id = $1 AND status = 'active'`, s.ref.StaffID)
				return err
			})
			w.post("/v1/staff/auth/login", login).want(t, http.StatusForbidden, "mfa_locked")
			// The runbook SQL (as the owner, in the staff member's scope: tenant GUC for tenant staff, none for platform).
			w.inScope(s, func(ctx context.Context, tx pgx.Tx) error {
				tag, err := tx.Exec(ctx, `UPDATE staff_mfa_factors SET status = 'revoked', revoked_at = now(), revoked_reason = 'reset'
					WHERE staff_user_id = $1 AND status = 'active'`, s.ref.StaffID)
				if err != nil {
					return err
				}
				if tag.RowsAffected() != 1 {
					t.Fatalf("the runbook UPDATE must revoke exactly one factor, got %d", tag.RowsAffected())
				}
				_, err = tx.Exec(ctx, `UPDATE staff_mfa_recovery_codes SET revoked_at = now()
					WHERE staff_user_id = $1 AND consumed_at IS NULL AND revoked_at IS NULL`, s.ref.StaffID)
				return err
			})
			if w.liveRecoveryCodes(s) != 0 {
				t.Fatal("the recovery codes must be revoked")
			}
			tok := w.bootstrapToken(s) // what cmd/seed-admin -issue-bootstrap issues
			w.post("/v1/staff/auth/login", login).want(t, http.StatusForbidden, "mfa_enrolment_required")
			if w.alertCount(kind, s.ref.StaffID) != 1 {
				t.Fatalf("the next password step must raise the expected P1 %s (a factor existed and is gone)", kind)
			}
			w.enrolWith(s, challengeOf(t, w.redeem(s, tok), "enrol"))
			w.fullLogin(s)
		})
	}
}
