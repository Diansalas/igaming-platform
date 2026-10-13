//go:build integration

package httpserver

// ADR 0119 S3: the staff MFA flows through the real HTTP stack, as the igaming_runtime role (staff_mfa_world_
// integration_test.go). Every assertion about persisted state reads the database.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/staffmfa"
	"github.com/Diansalas/igaming-platform/internal/staffmfa/store"
	"github.com/Diansalas/igaming-platform/internal/staffmfa/totp"
)

var recoveryCodeShape = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{5}-[0-9A-HJKMNP-TV-Z]{5}$`)

func wantNoStore(t *testing.T, r mfaResp, what string) {
	t.Helper()
	if r.header.Get("Cache-Control") != "no-store" || r.header.Get("Pragma") != "no-cache" {
		t.Fatalf("%s: Cache-Control/Pragma = %q/%q, want no-store/no-cache", what, r.header.Get("Cache-Control"), r.header.Get("Pragma"))
	}
}

// The whole S3 flow for a tenant staff member and a platform admin: enrolment required, bootstrap redeem, enrol,
// the aal2 session and its claims, login + verify, refresh (rotation keeps the assurance), logout, My security.
func TestStaffMFA_FullFlow_TenantAndPlatform(t *testing.T) {
	w := newMFAW(t)
	for name, s := range map[string]mfaStaff{"tenant": w.staff(&w.tenant, "tenant_admin"), "platform": w.staff(nil, "platform_admin")} {
		t.Run(name, func(t *testing.T) {
			w.t = t
			// 1. No factor: the password step answers mfa_enrolment_required and issues no challenge.
			r := w.post("/v1/staff/auth/login", s.loginBody())
			r.want(t, http.StatusForbidden, "mfa_enrolment_required")
			wantNoStore(t, r, "login")
			if n := w.countChallenges(s); n != 0 {
				t.Fatalf("a password alone must not issue a challenge (got %d)", n)
			}
			if w.auditCount(s, "staff.login_denied") != 1 {
				t.Fatal("the enrolment-required answer must be audited")
			}
			// 2. Enrolment through a bootstrap token.
			rd := w.redeem(s, w.bootstrapToken(s))
			rd.want(t, http.StatusOK, "")
			wantNoStore(t, rd, "redeem")
			enrolCh := challengeOf(t, rd, "enrol")
			e := w.enrolWith(s, enrolCh)
			if len(e.codes) != staffmfa.RecoveryCodesPerBatch {
				t.Fatalf("want %d recovery codes, got %d", staffmfa.RecoveryCodesPerBatch, len(e.codes))
			}
			for _, c := range e.codes {
				if !recoveryCodeShape.MatchString(c) {
					t.Fatalf("recovery code %q is not XXXXX-XXXXX Crockford", c)
				}
			}
			if e.start.ExpiresIn != 600 || e.start.OTPAuthURI == "" {
				t.Fatalf("enrol/start body: %+v", e.start)
			}
			if w.activeFactor(s).ID != e.factorID {
				t.Fatal("the confirmed factor must be the active one")
			}
			if w.auditCount(s, "staff.mfa_enrolled") != 1 || w.auditCount(s, "staff.enrolment_token_redeemed") != 1 {
				t.Fatal("redeem and enrolment must be audited")
			}
			// The enrol challenge is consumed by confirm.
			if _, consumed := w.challengeState(s, enrolCh, store.ChallengeEnrol); !consumed {
				t.Fatal("confirm must consume the enrol challenge")
			}
			// 3. The aal2 session behind the confirm token pair.
			claims := w.claims(e.pair.AccessToken)
			sid := uuid.MustParse(claims.SessionID)
			row := w.session(s, sid)
			if !sealOK(row) || row.AAL == nil || *row.AAL != 2 || len(row.AMR) != 2 || row.AMR[0] != "pwd" || row.AMR[1] != "otp" {
				t.Fatalf("the session row must be sealed aal2 pwd+otp: %+v", row)
			}
			if !row.AuthTime.Equal(*row.MFAAt) || !row.AbsExpires.Equal(row.AuthTime.Add(12*time.Hour)) || !row.Expires.Equal(row.AuthTime.Add(30*time.Minute)) {
				t.Fatalf("auth_time / mfa_at / absolute / idle: %+v", row)
			}
			if claims.AAL != 2 || claims.StaffVersion == nil || *claims.StaffVersion != 0 || claims.AuthTime == nil || claims.MFAAt == nil ||
				claims.AuthTime.Unix() != row.AuthTime.Unix() || claims.Role != auth.Role(s.role) || claims.TenantID != s.ref.TenantID {
				t.Fatalf("access token claims: %+v", claims)
			}
			// 4. Login: a challenge, then a TOTP code.
			lr := w.post("/v1/staff/auth/login", s.loginBody())
			lr.want(t, http.StatusOK, "")
			wantNoStore(t, lr, "login challenge")
			var ch staffChallengeWire
			lr.decode(t, &ch)
			if len(ch.Challenge.Methods) != 2 || ch.Challenge.Methods[0] != "totp" || ch.Challenge.Methods[1] != "recovery_code" {
				t.Fatalf("methods: %v", ch.Challenge.Methods)
			}
			if _, ok := mapOf(t, lr.body)["access_token"]; ok {
				t.Fatal("the password step must never return a token")
			}
			vr := w.verify(ch.Challenge.Token, w.code(s))
			vr.want(t, http.StatusOK, "")
			wantNoStore(t, vr, "verify")
			var pair tokenPairResponse
			vr.decode(t, &pair)
			if _, consumed := w.challengeState(s, ch.Challenge.Token, store.ChallengeMFA); !consumed {
				t.Fatal("a successful verify must consume the challenge")
			}
			if w.auditCount(s, "staff.login_password_verified") != 1 || w.auditCount(s, "staff.login_succeeded") != 1 {
				t.Fatal("password step and session issue must each be audited once")
			}
			// 5. Refresh rotates the session and carries the assurance unchanged.
			first := w.claims(pair.AccessToken)
			rf := w.refresh(pair.RefreshToken)
			rf.want(t, http.StatusOK, "")
			wantNoStore(t, rf, "refresh")
			var pair2 tokenPairResponse
			rf.decode(t, &pair2)
			second := w.claims(pair2.AccessToken)
			if second.SessionID == first.SessionID || second.AuthTime.Unix() != first.AuthTime.Unix() || second.MFAAt.Unix() != first.MFAAt.Unix() || second.AAL != 2 {
				t.Fatalf("rotation: %+v -> %+v", first, second)
			}
			old, next := w.session(s, uuid.MustParse(first.SessionID)), w.session(s, uuid.MustParse(second.SessionID))
			if old.ReplacedBy == nil || *old.ReplacedBy != next.ID || !sealOK(next) || !next.AbsExpires.Equal(*old.AbsExpires) || !next.AuthTime.Equal(*old.AuthTime) {
				t.Fatalf("rotated row: old %+v next %+v", old, next)
			}
			if d := time.Until(next.Expires); d < 29*time.Minute || d > 31*time.Minute {
				t.Fatalf("staff idle TTL: the new refresh token expires in %s, want about 30 min", d)
			}
			// 6. My security.
			st := w.do(http.MethodGet, "/v1/staff/me/mfa", pair2.AccessToken, nil)
			st.want(t, http.StatusOK, "")
			var status staffMFAStatusResponse
			st.decode(t, &status)
			if !status.Enrolled || status.FactorType == nil || *status.FactorType != "totp" || status.RecoveryCodesRemaining != 10 || status.HardLocked || status.ActivatedAt == nil {
				t.Fatalf("status: %s", st.body)
			}
			// 7. Logout revokes; the token no longer refreshes.
			w.post("/v1/auth/logout", map[string]string{"refresh_token": pair2.RefreshToken}).want(t, http.StatusNoContent, "")
			w.refresh(pair2.RefreshToken).want(t, http.StatusUnauthorized, "unauthorized")
		})
	}
}

func mapOf(t *testing.T, b []byte) map[string]any {
	t.Helper()
	m := map[string]any{}
	_ = json.Unmarshal(b, &m)
	return m
}

func (w *mfaW) claims(access string) *auth.Claims {
	w.t.Helper()
	c, err := w.issuer.Verify(access)
	if err != nil {
		w.t.Fatalf("verify access token: %v", err)
	}
	return c
}

// S3 entry gate G1: 5 and 6 wrong codes through the REAL routes persist the counters, the soft lock and the
// attempts_left decrement (a refusal commits). A mutant that returns an error from the transaction closure on a
// refusal loses all of them and is killed here.
func TestStaffMFA_G1_RefusalsPersistThroughRealRoutes(t *testing.T) {
	w := newMFAW(t)
	s := w.staff(&w.tenant, "support")
	w.enrol(s)
	before := w.activeFactor(s)
	ch := w.login(s)
	for i := 1; i <= 5; i++ {
		w.verify(ch, w.wrong(s)).want(t, http.StatusUnauthorized, "invalid_mfa_code")
		left, consumed := w.challengeState(s, ch, store.ChallengeMFA)
		if left != 5-i || consumed != (i == 5) {
			t.Fatalf("after wrong code %d: attempts_left=%d consumed=%v, want %d / %v", i, left, consumed, 5-i, i == 5)
		}
		f := w.activeFactor(s)
		if f.Failed24h != i {
			t.Fatalf("after wrong code %d the 24 h count is %d", i, f.Failed24h)
		}
	}
	f := w.activeFactor(s)
	if f.LockedUntil == nil || !f.LockedUntil.After(time.Now().Add(14*time.Minute)) || f.FailedCount != 0 || f.LastUsedStep != before.LastUsedStep {
		t.Fatalf("5 failures must soft-lock the factor (and burn nothing): %+v", f)
	}
	// The 6th attempt on the exhausted challenge: invalid_challenge, nothing counted.
	w.verify(ch, w.code(s)).want(t, http.StatusUnauthorized, "invalid_challenge")
	if w.activeFactor(s).Failed24h != 5 {
		t.Fatal("an exhausted challenge must not reach the factor")
	}
	// A fresh challenge (the soft lock is not a hard lock) and the CORRECT code: mfa_locked, nothing compared.
	ch2 := w.login(s)
	w.verify(ch2, w.code(s)).want(t, http.StatusForbidden, "mfa_locked")
	if left, consumed := w.challengeState(s, ch2, store.ChallengeMFA); left != 5 || consumed {
		t.Fatalf("a locked answer spends no attempt: %d %v", left, consumed)
	}
	if g := w.activeFactor(s); g.LastUsedStep != before.LastUsedStep || g.Failed24h != 5 {
		t.Fatalf("a locked factor must not burn or count: %+v", g)
	}
	if w.auditCount(s, "staff.mfa_verification_failed") != 5 || w.auditCount(s, "staff.mfa_locked") != 1 || w.auditCount(s, "staff.login_succeeded") != 0 {
		t.Fatal("audit: 5 failures, 1 locked, no session")
	}
	if n := len(w.sessions(s)); n != 1 { // only the enrolment session
		t.Fatalf("no session may be issued during the lockout (sessions=%d)", n)
	}
}

// Section 6.5 hard lock: 10 failures in 24 h hard-lock the factor, raise staff.mfa_hard_locked, and from then on
// the password step issues NO challenge (so a recovery code cannot be presented).
func TestStaffMFA_HardLock_NoChallenge_AlertRaised(t *testing.T) {
	w := newMFAW(t)
	for name, s := range map[string]mfaStaff{"tenant": w.staff(&w.tenant, "support"), "platform": w.staff(nil, "platform_admin")} {
		t.Run(name, func(t *testing.T) {
			w.t = t
			w.enrol(s)
			ch := w.login(s)
			for i := 0; i < 5; i++ {
				w.verify(ch, w.wrong(s)).want(t, http.StatusUnauthorized, "invalid_mfa_code")
			}
			// The soft lock's 15 minutes pass (the owner moves locked_until into the past; the 24 h budget is untouched).
			w.inScope(s, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE staff_mfa_factors SET locked_until = now() - interval '1 second' WHERE staff_user_id = $1 AND status = 'active'`, s.ref.StaffID)
				return err
			})
			ch = w.login(s)
			for i := 0; i < 5; i++ {
				w.verify(ch, w.wrong(s)).want(t, http.StatusUnauthorized, "invalid_mfa_code")
			}
			f := w.activeFactor(s)
			if f.HardLockedAt == nil || f.Failed24h != 10 {
				t.Fatalf("10 failures must hard-lock: %+v", f)
			}
			kind := string(alerting.KindStaffMFAHardLocked)
			if s.ref.TenantID == uuid.Nil {
				kind = string(alerting.KindStaffPlatformMFAHardLocked)
			}
			if w.alertCount(kind, s.ref.StaffID) != 1 || w.auditCount(s, "staff.mfa_hard_locked") != 1 {
				t.Fatalf("the hard lock must raise %s and be audited", kind)
			}
			n := w.countChallenges(s)
			w.post("/v1/staff/auth/login", s.loginBody()).want(t, http.StatusForbidden, "mfa_locked")
			if w.countChallenges(s) != n {
				t.Fatal("a hard-locked factor must not get a challenge")
			}
			// The old challenge is exhausted, so recovery is unreachable too.
			w.post("/v1/staff/auth/mfa/recover", map[string]string{"challenge_token": ch, "recovery_code": "AAAAA-AAAAA"}).
				want(t, http.StatusUnauthorized, "invalid_challenge")
		})
	}
}

// Section 17.3: replay. A burned step is refused, an older step after a newer one is refused, a challenge is single
// use, purposes do not cross, and two concurrent verifies of one code give exactly one session.
func TestStaffMFA_Replay(t *testing.T) {
	w := newMFAW(t)
	s := w.staff(&w.tenant, "support")
	w.enrol(s)
	w.mu.Lock()
	confirmStep := w.steps[s.ref.StaffID]
	secret := w.secrets[s.ref.StaffID]
	w.mu.Unlock()
	// The confirm code itself is burned.
	burned, _ := totpCode(secret, confirmStep)
	ch := w.login(s)
	w.verify(ch, burned).want(t, http.StatusUnauthorized, "invalid_mfa_code")
	good := w.code(s)
	w.verify(ch, good).want(t, http.StatusOK, "")
	// The same code again on a new challenge: refused. The used challenge again: invalid_challenge.
	ch2 := w.login(s)
	w.verify(ch2, good).want(t, http.StatusUnauthorized, "invalid_mfa_code")
	w.verify(ch, w.wrong(s)).want(t, http.StatusUnauthorized, "invalid_challenge")
	// Purposes do not cross: an enrol challenge at mfa/verify and an mfa challenge at enrol/start. (The enrol
	// challenge belongs to a staff member WITHOUT a factor: a bootstrap token of an enrolled one is refused at redeem
	// since the S3 review, R3.)
	fresh := w.staff(&w.tenant, "support")
	rd := w.redeem(fresh, w.bootstrapToken(fresh))
	enrolCh := challengeOf(t, rd, "enrol")
	w.verify(enrolCh, w.wrong(s)).want(t, http.StatusUnauthorized, "invalid_challenge")
	w.post("/v1/staff/auth/mfa/recover", map[string]string{"challenge_token": enrolCh, "recovery_code": "AAAAA-AAAAA"}).want(t, http.StatusUnauthorized, "invalid_challenge")
	w.post("/v1/staff/auth/enrol/start", map[string]string{"challenge_token": ch2}).want(t, http.StatusUnauthorized, "invalid_challenge")
	// A garbage token and a token with another staff member's id embedded: the same single answer.
	w.verify("not-a-token", "123456").want(t, http.StatusUnauthorized, "invalid_challenge")
	other := w.staff(&w.tenant, "support")
	forged, _ := staffmfa.NewToken(other.ref.StaffID)
	w.verify(forged.RevealForDelivery(), "123456").want(t, http.StatusUnauthorized, "invalid_challenge")
	// Concurrency: two challenges, one valid code, verified at once: exactly one session.
	c1, c2 := w.login(s), w.login(s)
	code := w.code(s)
	var wg sync.WaitGroup
	results := make([]int, 2)
	for i, c := range []string{c1, c2} {
		wg.Add(1)
		go func(i int, c string) {
			defer wg.Done()
			results[i] = w.verify(c, code).status
		}(i, c)
	}
	wg.Wait()
	ok := 0
	for _, st := range results {
		if st == http.StatusOK {
			ok++
		} else if st != http.StatusUnauthorized {
			t.Fatalf("concurrent verify answered %d", st)
		}
	}
	if ok != 1 {
		t.Fatalf("two concurrent verifies of one code: %d successes, want exactly 1 (%v)", ok, results)
	}
}

func totpCode(secret []byte, step int64) (string, error) {
	return totp.CodeAtStep(secret, step)
}

// Section 6.1.3: a recovery code buys a re-enrolment, never a session; it is single use; it raises
// staff.mfa_recovery_used; the old factor stays active until the new one is confirmed; confirmation revokes the old
// factor, every other session and the old recovery codes.
func TestStaffMFA_RecoveryFlow(t *testing.T) {
	w := newMFAW(t)
	for name, s := range map[string]mfaStaff{"tenant": w.staff(&w.tenant, "compliance"), "platform": w.staff(nil, "platform_admin")} {
		t.Run(name, func(t *testing.T) {
			w.t = t
			e := w.enrol(s)
			firstSID := uuid.MustParse(w.claims(e.pair.AccessToken).SessionID)
			oldFactor := w.activeFactor(s).ID
			r := w.post("/v1/staff/auth/mfa/recover", map[string]string{"challenge_token": w.login(s), "recovery_code": " " + lower(e.codes[0]) + " "})
			r.want(t, http.StatusOK, "")
			wantNoStore(t, r, "recover")
			if _, ok := mapOf(t, r.body)["access_token"]; ok {
				t.Fatal("a recovery code must never return a session")
			}
			enrolCh := challengeOf(t, r, "enrol")
			if w.activeFactor(s).ID != oldFactor {
				t.Fatal("the old factor stays active until the new one is confirmed")
			}
			kind := string(alerting.KindStaffMFARecoveryUsed)
			if s.ref.TenantID == uuid.Nil {
				kind = string(alerting.KindStaffPlatformMFARecoveryUsed)
			}
			if w.alertCount(kind, s.ref.StaffID) != 1 || w.auditCount(s, "staff.mfa_recovery_used") != 1 {
				t.Fatalf("recovery must raise %s and be audited", kind)
			}
			// The code is single use.
			w.post("/v1/staff/auth/mfa/recover", map[string]string{"challenge_token": w.login(s), "recovery_code": e.codes[0]}).
				want(t, http.StatusUnauthorized, "invalid_mfa_code")
			// Re-enrol.
			e2 := w.enrolWith(s, enrolCh)
			fs := w.factors(s)
			var replaced bool
			for _, f := range fs {
				if f.ID == oldFactor && f.Status == "revoked" && f.RevokedReason != nil && *f.RevokedReason == "replaced" {
					replaced = true
				}
			}
			if !replaced || w.activeFactor(s).ID != e2.factorID {
				t.Fatalf("confirm must replace the old factor: %+v", fs)
			}
			if w.session(s, firstSID).Revoked == nil {
				t.Fatal("confirm must revoke every other session")
			}
			// The old codes are dead, the new ones live.
			w.post("/v1/staff/auth/mfa/recover", map[string]string{"challenge_token": w.login(s), "recovery_code": e.codes[1]}).
				want(t, http.StatusUnauthorized, "invalid_mfa_code")
			var live int
			w.inScope(s, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT count(*) FROM staff_mfa_recovery_codes WHERE staff_user_id = $1 AND consumed_at IS NULL AND revoked_at IS NULL`, s.ref.StaffID).Scan(&live)
			})
			if live != 10 {
				t.Fatalf("after re-enrolment exactly the 10 new codes are live, got %d", live)
			}
		})
	}
}

func lower(s string) string {
	out := []byte(s)
	for i, c := range out {
		if c >= 'A' && c <= 'Z' {
			out[i] = c + 32
		}
	}
	return string(out)
}

// S3 entry gate (d): /enrol/redeem resolves tenant and staff from the TOKEN ROW; the body's tenant_slug, email and
// password must name that same staff member, with one answer for every mismatch.
func TestStaffMFA_Redeem_GateD_TokenRowIsAuthoritative(t *testing.T) {
	w := newMFAW(t)
	a := w.staff(&w.tenant, "support")
	b := w.staff(&w.tenant, "support")
	otherTenantStaff := w.staff(&w.other, "support")
	plat := w.staff(nil, "platform_admin")
	tokA := w.bootstrapToken(a)
	try := func(name string, body map[string]string) {
		t.Helper()
		r := w.post("/v1/staff/auth/enrol/redeem", body)
		if r.status != http.StatusUnauthorized || r.code() != "invalid_enrolment_token" {
			t.Fatalf("%s: want 401 invalid_enrolment_token, got %d %s", name, r.status, r.body)
		}
	}
	try("B's email and password with A's token", map[string]string{"token": tokA, "tenant_slug": b.slug, "email": b.email, "password": b.password})
	try("A's email, wrong password", map[string]string{"token": tokA, "tenant_slug": a.slug, "email": a.email, "password": "wrong-password"})
	try("A's credentials, another tenant's slug", map[string]string{"token": tokA, "tenant_slug": w.other.Slug, "email": a.email, "password": a.password})
	try("A's credentials, no tenant slug", map[string]string{"token": tokA, "email": a.email, "password": a.password})
	try("another tenant's staff with A's token and slug", map[string]string{"token": tokA, "tenant_slug": a.slug, "email": otherTenantStaff.email, "password": otherTenantStaff.password})
	try("an unknown token", map[string]string{"token": "x" + tokA[1:], "tenant_slug": a.slug, "email": a.email, "password": a.password})
	try("platform token with a tenant slug", map[string]string{"token": w.bootstrapToken(plat), "tenant_slug": a.slug, "email": plat.email, "password": plat.password})
	if w.auditCount(a, "staff.enrolment_token_rejected") < 2 {
		t.Fatal("token-bound refusals must be audited")
	}
	// The wrong password counted against the same password lockout as /login.
	var failures int
	w.inScope(a, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM login_attempts WHERE identifier = $1 AND NOT succeeded`, a.ref.TenantID.String()+":"+a.email).Scan(&failures)
	})
	if failures < 1 {
		t.Fatal("a wrong password at redeem must count against the password lockout")
	}
	// A suspended staff member's own token and correct password are refused the same way (and the token survives).
	c := w.staff(&w.tenant, "support")
	tokC := w.bootstrapToken(c)
	w.inScope(c, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE staff_users SET status = 'suspended' WHERE id = $1`, c.ref.StaffID)
		return err
	})
	try("a suspended staff member's own token and password", map[string]string{"token": tokC, "tenant_slug": c.slug, "email": c.email, "password": c.password})
	if w.auditCount(c, "staff.enrolment_token_rejected") != 1 {
		t.Fatal("the account-status refusal must be audited")
	}
	// The right credentials redeem once; the token is then spent.
	good := map[string]string{"token": tokA, "tenant_slug": a.slug, "email": a.email, "password": a.password}
	challengeOf(t, w.post("/v1/staff/auth/enrol/redeem", good), "enrol")
	try("the spent token", good)
}

// Redemption honours the same per-identifier password lockout as /login: once locked, even the right token and
// password are refused (429) and the token is not spent.
func TestStaffMFA_Redeem_Lockout(t *testing.T) {
	w := newMFAW(t)
	s := w.staff(&w.tenant, "support")
	tok := w.bootstrapToken(s)
	for i := 0; i < 5; i++ {
		w.post("/v1/staff/auth/login", map[string]string{"tenant_slug": s.slug, "email": s.email, "password": "wrong"}).want(t, http.StatusUnauthorized, "unauthorized")
	}
	w.redeem(s, tok).want(t, http.StatusTooManyRequests, "rate_limited")
	var live int
	w.inScope(s, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM staff_enrolment_tokens WHERE staff_user_id = $1 AND consumed_at IS NULL AND revoked_at IS NULL`, s.ref.StaffID).Scan(&live)
	})
	if live != 1 {
		t.Fatal("a refused redemption must not spend the token")
	}
}

// Section 7.2 / 17.6: a forged or edited staff session never refreshes; it is audited and raises the P1 alert. The
// runtime role cannot touch the immutable assurance columns at all (column grants), and SF040 stops every role.
func TestStaffMFA_SessionSealTamper(t *testing.T) {
	w := newMFAW(t)
	for name, s := range map[string]mfaStaff{"tenant": w.staff(&w.tenant, "support"), "platform": w.staff(nil, "platform_admin")} {
		t.Run(name, func(t *testing.T) {
			w.t = t
			kind := string(alerting.KindStaffSessionIntegrity)
			if s.ref.TenantID == uuid.Nil {
				kind = string(alerting.KindStaffPlatformSessionIntegrity)
			}
			now := time.Now()
			// (a) a valid planted session refreshes (control).
			tok, _ := w.plantSession(s, now, now.Add(12*time.Hour), now.Add(30*time.Minute), nil)
			w.refresh(tok).want(t, http.StatusOK, "")
			// (b) a forged INSERT with a random seal (runtime role): refused, audited, P1.
			tok, id := w.plantSession(s, now, now.Add(12*time.Hour), now.Add(30*time.Minute), func(b []byte) []byte { b[0] ^= 1; return b })
			w.refresh(tok).want(t, http.StatusUnauthorized, "unauthorized")
			if w.alertCount(kind, s.ref.StaffID) != 1 || w.auditCount(s, "auth.session_assurance_invalid") != 1 {
				t.Fatalf("a bad seal must be audited and raise %s", kind)
			}
			if attrs := w.alertAttrs(kind, s.ref.StaffID); attrs["cause"] != auth.IntegrityCauseBadSeal || attrs["session_id"] != id.String() {
				t.Fatalf("alert attributes: %v", attrs)
			}
			// (c) a valid session whose mfa_at is moved forward without re-sealing (SF040 admits the move; since review F9
			// only the owner can write mfa_at, the runtime role gets 42501 - see (d)).
			tok, id = w.plantSession(s, now, now.Add(12*time.Hour), now.Add(30*time.Minute), nil)
			if err := w.ownerSessionExec(s, id, `UPDATE sessions SET mfa_at = mfa_at + interval '1 minute' WHERE id = $1`); err != nil {
				t.Fatal(err)
			}
			w.refresh(tok).want(t, http.StatusUnauthorized, "unauthorized")
			if w.auditCount(s, "auth.session_assurance_invalid") != 2 {
				t.Fatal("an edited mfa_at must fail the seal")
			}
			// (d) the runtime role cannot UPDATE an immutable assurance column (column-level grant: 42501) ...
			_, id = w.plantSession(s, now, now.Add(12*time.Hour), now.Add(30*time.Minute), nil)
			for _, col := range []string{"aal = 2", "amr = ARRAY['pwd']", "auth_time = now()", "absolute_expires_at = now() + interval '1 hour'", "expires_at = now()", "principal_id = gen_random_uuid()",
				"mfa_at = now()", "assurance_seal = NULL, assurance_kid = NULL", "refresh_token_hash = 'x'"} {
				if err := w.runtimeSessionExec(s, id, `UPDATE sessions SET `+col+` WHERE id = $1`); pgCode(err) != "42501" {
					t.Fatalf("runtime UPDATE %s: want 42501, got %v", col, err)
				}
			}
			// ... and even the owner cannot null the seal or move mfa_at backwards (SF040).
			for _, set := range []string{"assurance_seal = NULL, assurance_kid = NULL", "mfa_at = mfa_at - interval '1 hour'"} {
				if err := w.ownerSessionExec(s, id, `UPDATE sessions SET `+set+` WHERE id = $1`); pgCode(err) != "SF040" {
					t.Fatalf("owner UPDATE %s: want SF040, got %v", set, err)
				}
			}
		})
	}
}

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// Section 7.2: staff refresh bounds. The successor keeps the absolute expiry (never extended by rotation) and its
// idle expiry is capped by it; past the absolute expiry nothing refreshes.
func TestStaffMFA_RefreshBounds(t *testing.T) {
	w := newMFAW(t)
	s := w.staff(&w.tenant, "support")
	now := time.Now()
	abs := now.Add(10 * time.Minute)
	tok, _ := w.plantSession(s, now.Add(-11*time.Hour-50*time.Minute), abs, abs, nil)
	r := w.refresh(tok)
	r.want(t, http.StatusOK, "")
	var p tokenPairResponse
	r.decode(t, &p)
	next := w.session(s, uuid.MustParse(w.claims(p.AccessToken).SessionID))
	if !next.AbsExpires.Equal(staffmfa.SessionTime(abs)) || !next.Expires.Equal(*next.AbsExpires) || !sealOK(next) {
		t.Fatalf("the successor keeps the absolute expiry and is capped by it: %+v", next)
	}
	// A second rotation keeps it again (the absolute expiry is copied, never recomputed).
	r = w.refresh(p.RefreshToken)
	r.want(t, http.StatusOK, "")
	r.decode(t, &p)
	third := w.session(s, uuid.MustParse(w.claims(p.AccessToken).SessionID))
	if !third.AbsExpires.Equal(*next.AbsExpires) || !third.AuthTime.Equal(*next.AuthTime) {
		t.Fatalf("absolute expiry / auth_time must be copied on every rotation: %+v", third)
	}
	// Past the absolute expiry: refused.
	soon := time.Now().Add(1500 * time.Millisecond)
	tok, _ = w.plantSession(s, time.Now().Add(-time.Hour), soon, soon, nil)
	time.Sleep(2 * time.Second)
	w.refresh(tok).want(t, http.StatusUnauthorized, "unauthorized")
	if w.auditCount(s, "auth.session_assurance_invalid") != 0 {
		t.Fatal("an expired session is not an integrity event")
	}
}

// S3 entry gate G4: a serialization failure / failed COMMIT is answered identically whatever the verification
// outcome would have been, and nothing persists.
func TestStaffMFA_G4_FaultAnswerIndependentOfOutcome(t *testing.T) {
	w := newMFAW(t)
	s := w.staff(&w.tenant, "support")
	w.enrol(s)
	before := w.activeFactor(s)
	sessionsBefore := len(w.sessions(s))
	w.m.commitFault = func(context.Context, pgx.Tx) error {
		return &pgconn.PgError{Code: "40001", Message: "could not serialize access due to concurrent update"}
	}
	t.Cleanup(func() { w.m.commitFault = nil })
	type answer struct {
		status int
		code   string
		msg    string
	}
	read := func(r mfaResp) answer {
		m := mapOf(t, r.body)
		code, _ := m["code"].(string)
		msg, _ := m["message"].(string)
		return answer{r.status, code, msg}
	}
	ch := w.login(s)
	right := read(w.verify(ch, w.code(s)))
	wrong := read(w.verify(ch, w.wrong(s)))
	recov := read(w.post("/v1/staff/auth/mfa/recover", map[string]string{"challenge_token": ch, "recovery_code": "AAAAA-AAAAA"}))
	if right != wrong || right != recov || right.status != http.StatusServiceUnavailable || right.code != "service_unavailable" {
		t.Fatalf("fault answers must be identical 503s: right=%+v wrong=%+v recover=%+v", right, wrong, recov)
	}
	if left, consumed := w.challengeState(s, ch, store.ChallengeMFA); left != 5 || consumed {
		t.Fatalf("nothing persists on a failed commit: attempts_left=%d consumed=%v", left, consumed)
	}
	after := w.activeFactor(s)
	if after.LastUsedStep != before.LastUsedStep || after.Failed24h != before.Failed24h || len(w.sessions(s)) != sessionsBefore {
		t.Fatalf("nothing persists on a failed commit: %+v -> %+v", before, after)
	}
}

// S3 entry gate G3: the TOTP step burn, the challenge consumption and the session insert happen in ONE transaction:
// all three are visible inside it just before COMMIT, and a failed COMMIT leaves none of them.
func TestStaffMFA_G3_BurnConsumeAndSessionInOneTransaction(t *testing.T) {
	w := newMFAW(t)
	s := w.staff(&w.tenant, "support")
	w.enrol(s)
	before := w.activeFactor(s)
	ch := w.login(s)
	code := w.code(s)
	var sawBurn, sawConsumed bool
	var sawSessions int
	w.m.commitFault = func(ctx context.Context, tx pgx.Tx) error {
		var step int64
		if err := tx.QueryRow(ctx, `SELECT last_used_step FROM staff_mfa_factors WHERE staff_user_id = $1 AND status = 'active'`, s.ref.StaffID).Scan(&step); err != nil {
			return err
		}
		sawBurn = step > before.LastUsedStep
		var consumed *time.Time
		if err := tx.QueryRow(ctx, `SELECT consumed_at FROM staff_auth_challenges WHERE staff_user_id = $1 AND purpose = 'mfa' ORDER BY created_at DESC LIMIT 1`, s.ref.StaffID).Scan(&consumed); err != nil {
			return err
		}
		sawConsumed = consumed != nil
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE principal_id = $1`, s.ref.StaffID).Scan(&sawSessions); err != nil {
			return err
		}
		return errors.New("injected commit failure")
	}
	t.Cleanup(func() { w.m.commitFault = nil })
	sessionsBefore := len(w.sessions(s))
	w.verify(ch, code).want(t, http.StatusServiceUnavailable, "service_unavailable")
	if !sawBurn || !sawConsumed {
		t.Fatalf("inside the transaction the burn and the consumption must both be visible: burn=%v consumed=%v", sawBurn, sawConsumed)
	}
	if f := w.activeFactor(s); f.LastUsedStep != before.LastUsedStep {
		t.Fatal("the burn must roll back with the session")
	}
	if _, consumed := w.challengeState(s, ch, store.ChallengeMFA); consumed {
		t.Fatal("the consumption must roll back with the session")
	}
	if len(w.sessions(s)) != sessionsBefore {
		t.Fatal("the session must roll back")
	}
	// Retry after the fault: the same challenge and code now succeed (nothing was half-applied).
	w.m.commitFault = nil
	w.verify(ch, code).want(t, http.StatusOK, "")
}

// S3 entry gate G7 / S2-R4 / residual R3: "no factor" is one answer. At the second-factor step a vanished factor
// answers exactly like a wrong code (attempt spent) and raises staff.mfa_integrity; at the password step an account
// that once had a factor gets the same mfa_enrolment_required as a never-enrolled one, plus the alert. A planted
// (AEAD-invalid) factor is a failure and an integrity alert, never "absent".
func TestStaffMFA_G7_NoFactorIsOneAnswer_AndIntegrityAlerts(t *testing.T) {
	w := newMFAW(t)
	s := w.staff(&w.tenant, "support")
	w.enrol(s)
	ch := w.login(s)
	revokeActive := func() {
		w.inScope(s, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE staff_mfa_factors SET status = 'revoked', revoked_at = now(), revoked_reason = 'reset' WHERE staff_user_id = $1 AND status = 'active'`, s.ref.StaffID)
			return err
		})
	}
	revokeActive()
	r := w.verify(ch, w.code(s))
	r.want(t, http.StatusUnauthorized, "invalid_mfa_code")
	if left, _ := w.challengeState(s, ch, store.ChallengeMFA); left != 4 {
		t.Fatalf("a vanished factor spends an attempt like a wrong code: %d", left)
	}
	if w.alertCount(string(alerting.KindStaffMFAIntegrity), s.ref.StaffID) != 1 {
		t.Fatal("a vanished factor must raise staff.mfa_integrity")
	}
	if a := w.alertAttrs(string(alerting.KindStaffMFAIntegrity), s.ref.StaffID); a["cause"] != "factor_missing" {
		t.Fatalf("cause: %v", a)
	}
	// The password step: the same 403 for a once-enrolled account (alert, deduplicated while open) and a fresh one (no alert).
	w.post("/v1/staff/auth/login", s.loginBody()).want(t, http.StatusForbidden, "mfa_enrolment_required")
	fresh := w.staff(&w.tenant, "support")
	w.post("/v1/staff/auth/login", fresh.loginBody()).want(t, http.StatusForbidden, "mfa_enrolment_required")
	if w.alertCount(string(alerting.KindStaffMFAIntegrity), fresh.ref.StaffID) != 0 {
		t.Fatal("a never-enrolled account raises nothing")
	}
	// Residual R3 at the password step alone: an account whose (once active) factor was revoked out of band gets the
	// same 403 and raises staff.mfa_integrity there, without any second-factor attempt.
	r3 := w.staff(&w.tenant, "support")
	w.enrol(r3)
	w.inScope(r3, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE staff_mfa_factors SET status = 'revoked', revoked_at = now(), revoked_reason = 'reset' WHERE staff_user_id = $1 AND status = 'active'`, r3.ref.StaffID)
		return err
	})
	w.post("/v1/staff/auth/login", r3.loginBody()).want(t, http.StatusForbidden, "mfa_enrolment_required")
	if w.alertCount(string(alerting.KindStaffMFAIntegrity), r3.ref.StaffID) != 1 {
		t.Fatal("a correct password for an account whose factor vanished must raise staff.mfa_integrity (R3)")
	}
	if w.auditCount(r3, "staff.login_denied") != 1 {
		t.Fatal("the enrolment-required answer must be audited")
	}
	// A planted active factor with attacker ciphertext: a challenge, then a failure and an integrity alert.
	p := w.staff(&w.tenant, "support")
	w.inScope(p, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_mfa_factors (tenant_id, staff_user_id, factor_type, status, secret_kid, secret_nonce, secret_ct, activated_at)
			VALUES ($1, $2, 'totp', 'active', 'f1', '\x000000000000000000000000', '\x0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f2021222324', now())`, p.ref.TenantID, p.ref.StaffID)
		return err
	})
	pc := w.login(p)
	w.verify(pc, "123456").want(t, http.StatusUnauthorized, "invalid_mfa_code")
	if w.alertCount(string(alerting.KindStaffMFAIntegrity), p.ref.StaffID) != 1 {
		t.Fatal("an AEAD failure must raise staff.mfa_integrity")
	}
	if a := w.alertAttrs(string(alerting.KindStaffMFAIntegrity), p.ref.StaffID); a["cause"] != "aead_open_failed" {
		t.Fatalf("cause: %v", a)
	}
}

// Section 6.5: the per-IP `mfa` bucket - the 31st second-factor request in a minute is 429, across the endpoints of
// the bucket.
func TestStaffMFA_RateLimit_MFABucket(t *testing.T) {
	w := newMFAW(t, func(d *Deps) { d.AuthRateLimitPerMinute = 0 })
	for i := 0; i < 30; i++ {
		path := "/v1/staff/auth/mfa/verify"
		if i%2 == 1 {
			path = "/v1/staff/auth/enrol/start"
		}
		r := w.post(path, map[string]string{"challenge_token": "garbage", "code": "123456"})
		if r.status == http.StatusTooManyRequests {
			t.Fatalf("request %d limited early", i+1)
		}
	}
	r := w.post("/v1/staff/auth/mfa/recover", map[string]string{"challenge_token": "garbage", "recovery_code": "x"})
	r.want(t, http.StatusTooManyRequests, "rate_limited")
	if r.header.Get("Retry-After") == "" {
		t.Fatal("429 must carry Retry-After")
	}
}

// S3 entry gate (e): second-factor failures after a correct password on 3 or more distinct days within 7 days raise
// staff.mfa_failure_pattern.
func TestStaffMFA_GateE_SlowRateFailurePattern(t *testing.T) {
	w := newMFAW(t)
	for name, s := range map[string]mfaStaff{"tenant": w.staff(&w.tenant, "support"), "platform": w.staff(nil, "platform_admin")} {
		t.Run(name, func(t *testing.T) {
			w.t = t
			kind := string(alerting.KindStaffMFAFailurePattern)
			var tenantArg any
			if s.ref.TenantID == uuid.Nil {
				kind = string(alerting.KindStaffPlatformMFAFailurePattern)
			} else {
				tenantArg = s.ref.TenantID
			}
			w.enrol(s)
			ch := w.login(s)
			w.verify(ch, w.wrong(s)).want(t, http.StatusUnauthorized, "invalid_mfa_code")
			if w.alertCount(kind, s.ref.StaffID) != 0 {
				t.Fatal("one day of failures is not a pattern")
			}
			// Failures audited on earlier days (the table owner backdates audit rows; the 9-day-old one is outside).
			w.inScope(s, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `INSERT INTO audit_log (tenant_id, actor_type, actor_id, action, target_type, target_id, outcome, created_at)
					VALUES ($1, 'staff', $2, 'staff.mfa_verification_failed', 'staff_user', $3, 'failure', now() - interval '2 days'),
					       ($1, 'staff', $2, 'staff.mfa_verification_failed', 'staff_user', $3, 'failure', now() - interval '4 days'),
					       ($1, 'staff', $2, 'staff.mfa_verification_failed', 'staff_user', $3, 'failure', now() - interval '9 days')`,
					tenantArg, s.ref.StaffID, s.ref.StaffID.String())
				return err
			})
			w.verify(ch, w.wrong(s)).want(t, http.StatusUnauthorized, "invalid_mfa_code")
			if w.alertCount(kind, s.ref.StaffID) != 1 {
				t.Fatalf("failures on 3 distinct days within 7 days must raise %s", kind)
			}
			if a := w.alertAttrs(kind, s.ref.StaffID); a["distinct_days"] != float64(3) {
				t.Fatalf("distinct_days: %v (the 9-day-old failure is outside the window)", a)
			}
		})
	}
}

// Tenant isolation: a staff member's challenge, factor status and sessions are reachable only in its own scope; the
// login scope rule still binds (a tenant row is never authenticated without its slug).
func TestStaffMFA_TenantIsolation(t *testing.T) {
	w := newMFAW(t)
	a := w.staff(&w.tenant, "tenant_admin")
	b := w.staff(&w.other, "tenant_admin")
	pa := w.fullLoginAfterEnrol(a)
	w.fullLoginAfterEnrol(b)
	// GET /v1/staff/me/mfa answers for the token's own subject only.
	var st staffMFAStatusResponse
	r := w.do(http.MethodGet, "/v1/staff/me/mfa", pa.AccessToken, nil)
	r.want(t, http.StatusOK, "")
	r.decode(t, &st)
	if !st.Enrolled {
		t.Fatal("own status")
	}
	// Tenant A's runtime session sees none of tenant B's MFA rows.
	var n int
	if err := w.rt.WithTenant(context.Background(), w.tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM staff_auth_challenges WHERE staff_user_id = $1) + (SELECT count(*) FROM staff_mfa_factors WHERE staff_user_id = $1)
			+ (SELECT count(*) FROM staff_mfa_recovery_codes WHERE staff_user_id = $1)`, b.ref.StaffID).Scan(&n)
	}); err != nil || n != 0 {
		t.Fatalf("cross-tenant MFA rows visible: %d %v", n, err)
	}
	// B's credentials with A's tenant slug: the ordinary 401 (scope).
	w.post("/v1/staff/auth/login", map[string]string{"tenant_slug": w.tenant.Slug, "email": b.email, "password": b.password}).want(t, http.StatusUnauthorized, "unauthorized")
	// A player token never reaches GET /v1/staff/me/mfa.
	pt, err := w.issuer.Issue(uuid.NewString(), w.tenant.ID, auth.RolePlayer, auth.PrincipalPlayer, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	w.do(http.MethodGet, "/v1/staff/me/mfa", pt, nil).want(t, http.StatusForbidden, "forbidden")
}

func (w *mfaW) fullLoginAfterEnrol(s mfaStaff) tokenPairResponse {
	w.t.Helper()
	w.enrol(s)
	return w.fullLogin(s)
}

// A staff member that is no longer active between the password step and the second factor is refused, and the
// challenge is consumed.
func TestStaffMFA_SuspendedBetweenSteps(t *testing.T) {
	w := newMFAW(t)
	s := w.staff(&w.tenant, "support")
	w.enrol(s)
	ch := w.login(s)
	w.inScope(s, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE staff_users SET status = 'suspended' WHERE id = $1`, s.ref.StaffID)
		return err
	})
	w.verify(ch, w.code(s)).want(t, http.StatusForbidden, "forbidden")
	if _, consumed := w.challengeState(s, ch, store.ChallengeMFA); !consumed {
		t.Fatal("the challenge of an inactive staff member must be consumed")
	}
	if w.auditCount(s, "staff.login_succeeded") != 0 {
		t.Fatal("no session")
	}
}

// The enrolment window: a confirm against an unknown or expired pending factor is 409 mfa_enrolment_expired and
// spends nothing; a wrong confirm code spends an attempt and counts on the pending row.
func TestStaffMFA_EnrolConfirm_Expiry_And_WrongCode(t *testing.T) {
	w := newMFAW(t)
	s := w.staff(&w.tenant, "support")
	rd := w.redeem(s, w.bootstrapToken(s))
	ch := challengeOf(t, rd, "enrol")
	w.post("/v1/staff/auth/enrol/confirm", map[string]string{"challenge_token": ch, "factor_id": uuid.NewString(), "code": "123456"}).
		want(t, http.StatusConflict, "mfa_enrolment_expired")
	sr := w.post("/v1/staff/auth/enrol/start", map[string]string{"challenge_token": ch})
	sr.want(t, http.StatusOK, "")
	var start enrolStartWire
	sr.decode(t, &start)
	w.post("/v1/staff/auth/enrol/confirm", map[string]string{"challenge_token": ch, "factor_id": start.FactorID, "code": "000000"}).
		want(t, http.StatusUnauthorized, "invalid_mfa_code")
	if left, _ := w.challengeState(s, ch, store.ChallengeEnrol); left != 4 {
		t.Fatalf("a wrong confirm code spends one attempt: %d", left)
	}
	// Expire the pending factor (owner) and confirm: 409.
	w.inScope(s, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE staff_mfa_factors SET status = 'revoked', revoked_at = now(), revoked_reason = 'pending_expired', pending_expires_at = NULL WHERE id = $1`, uuid.MustParse(start.FactorID))
		return err
	})
	w.post("/v1/staff/auth/enrol/confirm", map[string]string{"challenge_token": ch, "factor_id": start.FactorID, "code": "123456"}).
		want(t, http.StatusConflict, "mfa_enrolment_expired")
	// No session and no factor came of it.
	if len(w.sessions(s)) != 0 || len(w.factors(s)) != 1 {
		t.Fatal("a failed enrolment leaves no session and only the revoked pending factor")
	}
}

// identity.StaffUser compile guard (the harness reads staff by id).
var _ = identity.StaffUser{}
