//go:build integration

package httpserver

// ADR 0119 slice S4b, against a real database as igaming_runtime: step-up enforcement on every classified staff route
// (section 17.2), the step-up endpoint (6.6), the recovery-code regeneration, replay and the lockout shared with login
// (17.3, 17.4), forged and stale mfa_at claims (S3-R13 / S4a-R10), the session seal v2 binding (S3-R10), step-up
// racing a revocation (17.5), the four classifiers, the signer backstop over HTTP (9.1) and secret hygiene (17.7).

import (
	"bytes"
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/auth/authtest"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/providercred"
	"github.com/Diansalas/igaming-platform/internal/staffmfa"
	"github.com/Diansalas/igaming-platform/internal/staffmfa/store"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

var placeholderRE = regexp.MustCompile(`\{[A-Za-z]+\}`)

// fillPath replaces every path placeholder: {tenantID} with tenantID, anything else with a fresh uuid.
func fillPath(path string, tenantID uuid.UUID) string {
	return placeholderRE.ReplaceAllStringFunc(path, func(p string) string {
		if p == "{tenantID}" {
			return tenantID.String()
		}
		return uuid.NewString()
	})
}

// classifierCriticalBody is a body that makes each classifier route critical (section 8.2), for a platform_admin.
var classifierCriticalBody = map[string]any{
	"classifier:launchActionClass":          map[string]any{"action": "activate", "reason_code": "x"},
	"classifier:financialPolicyChangeClass": map[string]any{},
	"classifier:alertResolveClass":          map[string]any{"reason_code": "x"},
	"classifier:alertRouteManageClass":      map[string]any{},
	"classifier:rgLimitChangeClass":         map[string]any{"kind": "deposit", "value": nil},
	"classifier:amlHoldPlaceClass":          map[string]any{"reason_class": "confirmed_sanctions"},
	"classifier:amlTransitionClass":         map[string]any{"to": "closed_no_action"},
	"classifier:kycResidenceReviewClass":    map[string]any{"status": "approved", "verified_residence_country": "MT"},
}

// stepUpRefused asserts r is the step-up refusal for maxAge seconds (body code and max_age; RFC 9470 header).
func stepUpRefused(t *testing.T, r mfaResp, maxAge int, what string) {
	t.Helper()
	var e struct {
		Code   string `json:"code"`
		MaxAge *int   `json:"max_age"`
	}
	_ = json.Unmarshal(r.body, &e)
	if r.status != http.StatusForbidden || e.Code != "step_up_required" || e.MaxAge == nil || *e.MaxAge != maxAge {
		t.Fatalf("%s: want 403 step_up_required max_age=%d, got %d %s", what, maxAge, r.status, r.body)
	}
	h := r.header.Get("WWW-Authenticate")
	if !strings.HasPrefix(h, "Bearer ") || !strings.Contains(h, `error="insufficient_user_authentication"`) ||
		!strings.Contains(h, `max_age="`+strconv.Itoa(maxAge)+`"`) {
		t.Fatalf("%s: RFC 9470 header missing or wrong: %q", what, h)
	}
}

func notStepUpRefused(t *testing.T, r mfaResp, what string) {
	t.Helper()
	if r.code() == "step_up_required" {
		t.Fatalf("%s: unexpected step_up_required: %d %s", what, r.status, r.body)
	}
	if r.status == http.StatusUnauthorized {
		t.Fatalf("%s: the token must pass the chain: %d %s", what, r.status, r.body)
	}
}

// Section 17.2 golden table: EVERY classified route. A platform_admin whose last second factor is 11 minutes old gets
// 403 step_up_required with the class max-age and the RFC 9470 header on every financial / critical route (and on
// every classifier route given a critical-inducing body); the same staff member with a fresh session passes the gate
// on each of them; a route classified none never asks, even with the stale token.
func TestStepUp_GoldenTable_EveryClassifiedRoute(t *testing.T) {
	// The stale pass mounts every route (zero-value subsystems, as TestStaffLiveness_EveryGoldenRoute: the gate refuses
	// before any handler runs). The fresh pass reaches the handlers, so it uses real subsystems only (a mock payment
	// orchestrator, sportsbook on); a route not mounted there (Go's own 404) is skipped and must be a provider
	// credential route (pinned below).
	w := newMFAW(t, func(d *Deps) {
		d.PaymentOrchestrator, d.ProviderCredentials = &payments.Orchestrator{}, &providercred.Subsystem{}
		d.SportsbookEnabled, d.SportsbookSettlementSimulationEnabled = true, true
	})
	admin := w.staff(nil, "platform_admin")
	stale := authtest.LiveStaffTokenAged(t, w.rt, testStaffMFAKeys(), w.issuer, admin.ref.StaffID, uuid.Nil, auth.RolePlatformAdmin, 11*time.Minute)
	w2 := newMFAW(t, func(d *Deps) {
		d.PaymentOrchestrator, _ = newMockOrchestrator()
		d.SportsbookEnabled, d.SportsbookSettlementSimulationEnabled = true, true
	})
	admin2 := w2.staff(nil, "platform_admin")
	fresh := authtest.LiveStaffToken(t, w2.rt, testStaffMFAKeys(), w2.issuer, admin2.ref.StaffID, uuid.Nil, auth.RolePlatformAdmin)
	var skipped []string
	routes := make([]string, 0, len(staffRouteGolden))
	for r := range staffRouteGolden {
		routes = append(routes, r)
	}
	sort.Strings(routes)
	counts := map[string]int{}
	for _, route := range routes {
		class := staffRouteGolden[route]
		method, path, _ := strings.Cut(route, " ")
		mutating := method != http.MethodGet
		var body any
		if mutating {
			body = map[string]any{}
		}
		if b, ok := classifierCriticalBody[class]; ok {
			body = b
		}
		p := fillPath(path, w.tenant.ID)
		switch {
		case class == "critical" || class == "financial" || strings.HasPrefix(class, "classifier:"):
			want := 300
			if class == "financial" {
				want = 600
			}
			stepUpRefused(t, w.do(method, p, stale, body), want, route+" (stale)")
			fr := w2.do(method, fillPath(path, w2.tenant.ID), fresh, body)
			if fr.status == http.StatusNotFound && !strings.Contains(string(fr.body), `"code"`) {
				skipped = append(skipped, route)
			} else {
				notStepUpRefused(t, fr, route+" (fresh)")
			}
			counts[class]++
		case mutating:
			notStepUpRefused(t, w.do(method, p, stale, body), route+" (none, stale)")
			counts["none-mutating"]++
		default:
			counts["read"]++
		}
	}
	// Non-vacuity: the classified set is the golden table's (S4a + S4b + S5/S6).
	// 125 mutating routes: 59 critical, 22 financial, 5 classifier routes, 39 none; 79 reads (S1b adds GET /v1/admin/alerts/{alertID}). ADR 0119 S5 + S6: +14
	// critical lifecycle routes, -1 (the legacy critical create route), +2 none (four-eyes reject / cancel), +8 reads.
	// ADR 0116 A7: +11 AML routes: +3 critical (lift, propose, approve), +3 reads; place hold and transition are the classifiers amlHoldPlaceClass / amlTransitionClass (SR7-1, SR7-5), so 3 static none-mutating (open case, note, SAR flag) + 2 classifiers.
	// none-mutating 39 -> 42. A6 (ADR 0115 S4-min): +1 critical (POST /v1/admin/rg/enforcement, SR6-1) and +1 read (GET): critical 63, none-mutating 42, read 83.
	// A5 (WITHDRAWAL-REVERSAL-1): +1 critical (POST /v1/admin/withdrawals/{id}/payout-reversal): critical 64.
	// Owner decision 3 (SRME-1): POST /v1/admin/kyc/verifications/{id}/review became the classifier kycResidenceReviewClass: none-mutating 42 -> 41.
	// A10c: +4 reads (attempt events, reconciliation single run, single mismatch, hold candidates): read 87; nothing mutating changes.
	// F6 / SA-3: +1 read (GET /v1/admin/platform/status): read 88; nothing mutating changes.
	if counts["critical"] != 64 || counts["classifier:alertRouteManageClass"] != 1 || counts["financial"] != 22 || counts["classifier:launchActionClass"] != 1 ||
		counts["classifier:financialPolicyChangeClass"] != 1 || counts["classifier:alertResolveClass"] != 1 ||
		counts["classifier:rgLimitChangeClass"] != 1 || counts["classifier:amlHoldPlaceClass"] != 1 || counts["classifier:amlTransitionClass"] != 1 || counts["classifier:kycResidenceReviewClass"] != 1 || counts["none-mutating"] != 41 || counts["read"] != 88 {
		t.Fatalf("unexpected coverage: %v", counts)
	}
	for _, r := range skipped {
		if !strings.Contains(r, "provider-credential") {
			t.Errorf("%s was not mounted in the fresh world: only provider-credential routes may be skipped", r)
		}
	}
	t.Logf("golden coverage: %v; fresh pass skipped %d provider-credential routes", counts, len(skipped))
}

// Section 17.2 boundaries: mfa_at = now - (maxAge - 3 s) passes, now - (maxAge + 1 s) is refused, for each class (the
// claim is whole seconds, so the effective instant may be up to 1 s older than the row's).
func TestStepUp_Boundaries_CriticalAndFinancial(t *testing.T) {
	w := newMFAW(t)
	fin := w.staff(&w.tenant, "finance")
	admin := w.staff(nil, "platform_admin")
	for _, c := range []struct {
		name, method, path string
		s                  mfaStaff
		role               auth.Role
		maxAge             time.Duration
	}{
		{"critical", http.MethodPost, "/v1/staff/mfa/recovery-codes/regenerate", admin, auth.RolePlatformAdmin, 5 * time.Minute},
		{"financial", http.MethodPost, "/v1/admin/withdrawals/" + uuid.NewString() + "/approve", fin, auth.RoleFinance, 10 * time.Minute},
	} {
		t.Run(c.name, func(t *testing.T) {
			ok := authtest.LiveStaffTokenAged(t, w.rt, testStaffMFAKeys(), w.issuer, c.s.ref.StaffID, c.s.ref.TenantID, c.role, c.maxAge-3*time.Second)
			notStepUpRefused(t, w.do(c.method, c.path, ok, map[string]any{}), "inside the window")
			old := authtest.LiveStaffTokenAged(t, w.rt, testStaffMFAKeys(), w.issuer, c.s.ref.StaffID, c.s.ref.TenantID, c.role, c.maxAge+time.Second)
			stepUpRefused(t, w.do(c.method, c.path, old, map[string]any{}), int(c.maxAge/time.Second), "just outside the window")
		})
	}
}

// agedSession plants a sealed session for an ENROLLED staff member whose second factor was verified age ago, and
// returns a genuine access token for it (claims = the row) and its refresh token.
func (w *mfaW) agedSession(s mfaStaff, age time.Duration) (access, refresh string, sid uuid.UUID) {
	w.t.Helper()
	at := staffmfa.SessionTime(time.Now().Add(-age))
	refresh, sid = w.plantSession(s, at, at.Add(12*time.Hour), time.Now().Add(30*time.Minute), nil)
	access = w.liveTok(s, sid, func(p *auth.StaffTokenParams) { p.AuthTime, p.MFAAt = at, at })
	return access, refresh, sid
}

func (w *mfaW) stepUp(access, code string) mfaResp {
	return w.do(http.MethodPost, "/v1/staff/auth/step-up", access, map[string]string{"code": code})
}

const regeneratePath = "/v1/staff/mfa/recovery-codes/regenerate"

func (w *mfaW) recoveryCodesLeft(s mfaStaff) int {
	w.t.Helper()
	st, err := store.New(testStaffMFAKeys())
	if err != nil {
		w.t.Fatal(err)
	}
	var n int
	w.inScope(s, func(ctx context.Context, tx pgx.Tx) error {
		n, err = st.RecoveryCodesRemaining(ctx, tx, s.ref)
		return err
	})
	return n
}

// Section 6.6 end to end (tenant and platform staff): a stale session is refused on a critical route; the step-up
// verifies a TOTP code, writes mfa_at and a VALID v2 seal to the chain-head row only (auth_time untouched), returns a
// new access token and no refresh token (no-store); the OLD access token stays un-stepped-up (the older of claim and
// row wins); the new one passes; regeneration replaces the codes; a refresh carries the fresh mfa_at forward; the same
// code replayed is refused; a step-up with an access token that names a replaced row is session_revoked.
func TestStepUp_EndToEnd_TenantAndPlatform(t *testing.T) {
	w := newMFAW(t)
	for _, tc := range []struct {
		name   string
		tenant bool
	}{{"tenant", true}, {"platform", false}} {
		t.Run(tc.name, func(t *testing.T) {
			w.t = t
			var s mfaStaff
			if tc.tenant {
				s = w.staff(&w.tenant, "tenant_admin")
			} else {
				s = w.staff(nil, "platform_admin")
			}
			enrolled := w.enrol(s)
			access, refresh, sid := w.agedSession(s, 11*time.Minute)
			before := w.session(s, sid)
			stepUpRefused(t, w.do(http.MethodPost, regeneratePath, access, nil), 300, "stale regenerate")

			code := w.code(s)
			r := w.stepUp(access, code)
			r.want(t, http.StatusOK, "")
			if r.header.Get("Cache-Control") != "no-store" || r.header.Get("Pragma") != "no-cache" {
				t.Fatalf("step-up response must be no-store: %v", r.header)
			}
			var su stepUpWire
			r.decode(t, &su)
			if strings.Contains(string(r.body), "refresh_token") || su.AccessToken == "" || su.TokenType != "Bearer" || su.ExpiresIn != 900 {
				t.Fatalf("step-up body: %s", r.body)
			}
			mfaAt, err := time.Parse(time.RFC3339, su.MFAAt)
			if err != nil || time.Since(mfaAt) > 10*time.Second || time.Until(mfaAt) > 5*time.Second {
				t.Fatalf("mfa_at %q", su.MFAAt)
			}
			fexp, _ := time.Parse(time.RFC3339, su.StepUpExpiresAt.Financial)
			cexp, _ := time.Parse(time.RFC3339, su.StepUpExpiresAt.Critical)
			if fexp.Sub(mfaAt) != 10*time.Minute || cexp.Sub(mfaAt) != 5*time.Minute {
				t.Fatalf("step_up_expires_at: %+v (mfa_at %s)", su.StepUpExpiresAt, su.MFAAt)
			}
			after := w.session(s, sid)
			if !after.MFAAt.After(*before.MFAAt) || !after.AuthTime.Equal(*before.AuthTime) || !sealOK(after) ||
				after.Revoked != nil || after.ReplacedBy != nil || after.RefreshHash != before.RefreshHash || !after.Expires.Equal(before.Expires) {
				t.Fatalf("session row after step-up: before %+v after %+v (seal ok %v)", before, after, sealOK(after))
			}
			if c, err := w.issuer.Verify(su.AccessToken); err != nil || c.SessionID != sid.String() || c.MFAAt.Unix() != after.MFAAt.Unix() ||
				c.AuthTime.Unix() != before.AuthTime.Unix() {
				t.Fatalf("new access token claims: %+v %v", c, err)
			}
			if w.auditCount(s, "staff.step_up_verified") != 1 {
				t.Fatal("staff.step_up_verified not audited once")
			}
			// The old access token is still stale: min(claim, row).
			stepUpRefused(t, w.do(http.MethodPost, regeneratePath, access, nil), 300, "old token after step-up")
			// The new one passes; the codes are replaced (10 new, every old one revoked).
			if w.recoveryCodesLeft(s) != 10 {
				t.Fatal("precondition: 10 codes")
			}
			rg := w.do(http.MethodPost, regeneratePath, su.AccessToken, nil)
			rg.want(t, http.StatusOK, "")
			var codes recoveryCodesWire
			rg.decode(t, &codes)
			if len(codes.RecoveryCodes) != 10 || rg.header.Get("Cache-Control") != "no-store" {
				t.Fatalf("regenerate: %s", rg.body)
			}
			for _, old := range enrolled.codes {
				for _, n := range codes.RecoveryCodes {
					if old == n {
						t.Fatal("a regenerated code equals an old one")
					}
				}
			}
			if w.recoveryCodesLeft(s) != 10 || w.auditCount(s, "staff.mfa_recovery_codes_regenerated") != 1 {
				t.Fatal("regeneration must leave exactly the 10 new codes and audit once")
			}
			// Replay of the same code: refused (monotonic step).
			w.stepUp(su.AccessToken, code).want(t, http.StatusUnauthorized, "invalid_mfa_code")
			// A refresh copies the fresh mfa_at; the token it mints passes a critical route.
			rr := w.refresh(refresh)
			rr.want(t, http.StatusOK, "")
			var pair tokenPairResponse
			rr.decode(t, &pair)
			notStepUpRefused(t, w.do(http.MethodPost, regeneratePath, pair.AccessToken, nil), "refreshed token")
			// The step-up access token names the now-replaced row: a step-up through it is refused (refresh first).
			w.stepUp(su.AccessToken, w.code(s)).want(t, http.StatusUnauthorized, "session_revoked")
			// The new head can step up.
			w.stepUp(pair.AccessToken, w.code(s)).want(t, http.StatusOK, "")
		})
	}
}

// Sections 6.5 / 17.4: step-up failures share the factor budget with login. Four wrong step-ups are invalid_mfa_code;
// the fifth soft-locks the factor and revokes the calling session's chain (session_revoked; the token is dead); the
// next login's correct code then meets mfa_locked. Mixed: four wrong login codes, then one wrong step-up, does the same.
// A recovery code is never accepted at step-up (it counts as a failure).
func TestStepUp_LockoutSharedWithLogin(t *testing.T) {
	w := newMFAW(t)
	t.Run("five_step_ups", func(t *testing.T) {
		w.t = t
		s := w.staff(&w.tenant, "support")
		e := w.enrol(s)
		access := e.pair.AccessToken
		// A recovery code at step-up: refused as a code (counts).
		w.stepUp(access, e.codes[0]).want(t, http.StatusUnauthorized, "invalid_mfa_code")
		for i := 0; i < 3; i++ {
			w.stepUp(access, w.wrong(s)).want(t, http.StatusUnauthorized, "invalid_mfa_code")
		}
		if f := w.activeFactor(s); f.FailedCount != 4 {
			t.Fatalf("failed_count %d, want 4", f.FailedCount)
		}
		w.stepUp(access, w.wrong(s)).want(t, http.StatusUnauthorized, "session_revoked")
		if w.auditCount(s, "staff.step_up_failed") != 5 {
			t.Fatalf("step_up_failed audited %d times, want 5", w.auditCount(s, "staff.step_up_failed"))
		}
		w.do(http.MethodGet, "/v1/staff/me/mfa", access, nil).want(t, http.StatusUnauthorized, "session_revoked")
		w.refresh(e.pair.RefreshToken).want(t, http.StatusUnauthorized, "")
		w.verify(w.login(s), w.code(s)).want(t, http.StatusForbidden, "mfa_locked")
		for _, row := range w.sessions(s) {
			if row.Revoked == nil {
				t.Fatalf("session %s still live after the step-up lockout", row.ID)
			}
		}
	})
	t.Run("four_logins_then_step_up", func(t *testing.T) {
		w.t = t
		s := w.staff(&w.tenant, "support")
		e := w.enrol(s)
		ch := w.login(s)
		for i := 0; i < 4; i++ {
			w.verify(ch, w.wrong(s)).want(t, http.StatusUnauthorized, "invalid_mfa_code")
		}
		w.stepUp(e.pair.AccessToken, w.wrong(s)).want(t, http.StatusUnauthorized, "session_revoked")
		w.do(http.MethodGet, "/v1/staff/me/mfa", e.pair.AccessToken, nil).want(t, http.StatusUnauthorized, "session_revoked")
	})
	t.Run("locked_factor_answers_before_comparison", func(t *testing.T) {
		w.t = t
		s := w.staff(&w.tenant, "support")
		e := w.enrol(s)
		other := w.fullLogin(s) // a second, live session of the same staff member
		ch := w.login(s)
		for i := 0; i < 5; i++ {
			w.verify(ch, w.wrong(s)).want(t, http.StatusUnauthorized, "")
		}
		// Soft-locked through login: the step-up with the CORRECT code is mfa_locked, nothing burned or revoked.
		w.stepUp(other.AccessToken, w.code(s)).want(t, http.StatusForbidden, "mfa_locked")
		w.do(http.MethodGet, "/v1/staff/me/mfa", other.AccessToken, nil).want(t, http.StatusOK, "")
		_ = e
	})
}

// S3-R13 / S4a-R10: the step-up gate trusts the SEALED ROW, never the claims alone; and the token verifier refuses
// incoherent assurance claims. Hand-signed tokens with the server's own key (an attacker holding the key is outside
// the threat model; the point is the rule, not the key).
func TestStepUp_ForgedAndIncoherentClaims(t *testing.T) {
	w := newMFAW(t)
	s := w.staff(&w.tenant, "tenant_admin")
	w.enrol(s)
	_, _, sid := w.agedSession(s, 11*time.Minute)
	now := time.Now()
	sign := func(mut func(*auth.Claims)) string {
		sv := authtest.StaffVersion(t, w.owner, s.ref.StaffID, s.ref.TenantID) // ADR 0119 S5 + S6: the row's staff_version
		c := auth.Claims{
			RegisteredClaims: jwt.RegisteredClaims{Subject: s.ref.StaffID.String(), Issuer: "platform-api-test",
				Audience: jwt.ClaimStrings{"platform-api-test"}, IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(10 * time.Minute)),
				ID: uuid.NewString()},
			TenantID: s.ref.TenantID, Role: auth.RoleTenantAdmin, PrincipalType: auth.PrincipalStaff,
			SessionID: sid.String(), AAL: 2, AMR: []string{"pwd", "otp"},
			AuthTime: jwt.NewNumericDate(now.Add(-11 * time.Minute)), MFAAt: jwt.NewNumericDate(now), StaffVersion: &sv,
		}
		if mut != nil {
			mut(&c)
		}
		tok := jwt.NewWithClaims(jwt.SigningMethodHS256, c)
		tok.Header["kid"] = "k1"
		out, err := tok.SignedString([]byte(testJWTSecret))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	// A fresh mfa_at claim over an 11-minute-old sealed row: the row wins.
	stepUpRefused(t, w.do(http.MethodPost, regeneratePath, sign(nil), nil), 300, "fresh claim, stale row")
	for name, mut := range map[string]func(*auth.Claims){
		"mfa_at in the future":   func(c *auth.Claims) { c.MFAAt = jwt.NewNumericDate(now.Add(time.Minute)) },
		"mfa_at before auth":     func(c *auth.Claims) { c.MFAAt = jwt.NewNumericDate(now.Add(-20 * time.Minute)) },
		"amr without 2nd factor": func(c *auth.Claims) { c.AMR = []string{"pwd"} },
		"auth_time older 12h":    func(c *auth.Claims) { c.AuthTime = jwt.NewNumericDate(now.Add(-13 * time.Hour)) },
		"auth_time future":       func(c *auth.Claims) { c.AuthTime = jwt.NewNumericDate(now.Add(time.Hour)); c.MFAAt = c.AuthTime },
	} {
		w.do(http.MethodPost, regeneratePath, sign(mut), nil).want(t, http.StatusUnauthorized, "unauthorized")
		_ = name
	}
}

// S3-R10: the seal binds refresh_token_hash and expires_at. A party that can disable the SF040 trigger (the table
// owner here) rewrites either column: the refresh is refused and audited as tampering (with the P1 alert), and the
// session's access token is refused by liveness (seal_invalid) - so a forged step-up state can never be inherited.
func TestStepUp_SealBindsRefreshHashAndIdleExpiry(t *testing.T) {
	w := newMFAW(t)
	for _, col := range []string{"refresh_token_hash", "expires_at"} {
		t.Run(col, func(t *testing.T) {
			w.t = t
			s := w.staff(&w.tenant, "support")
			e := w.enrol(s)
			sid := w.sidOf(e.pair.AccessToken)
			attacker := "attacker-known-refresh-token-" + uuid.NewString()
			sum := sha256.Sum256([]byte(attacker))
			if err := store.WithScope(context.Background(), w.owner, s.ref, func(ctx context.Context, tx pgx.Tx) error {
				if err := db.SetSessionInternalOpID(ctx, tx, sid); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `ALTER TABLE sessions DISABLE TRIGGER sessions_staff_assurance_guard`); err != nil {
					return err
				}
				var err error
				if col == "refresh_token_hash" {
					_, err = tx.Exec(ctx, `UPDATE sessions SET refresh_token_hash = $2 WHERE id = $1`, sid, hex.EncodeToString(sum[:]))
				} else {
					_, err = tx.Exec(ctx, `UPDATE sessions SET expires_at = expires_at - interval '1 minute' WHERE id = $1`, sid)
				}
				if err != nil {
					return err
				}
				_, err = tx.Exec(ctx, `ALTER TABLE sessions ENABLE TRIGGER sessions_staff_assurance_guard`)
				return err
			}); err != nil {
				t.Fatalf("owner rewrite with the trigger disabled: %v", err)
			}
			before := w.reasonCount(observability.StaffLivenessReasonSealInvalid)
			w.refusedFor(w.do(http.MethodGet, "/v1/staff/me/mfa", e.pair.AccessToken, nil), observability.StaffLivenessReasonSealInvalid, before)
			tok := e.pair.RefreshToken
			if col == "refresh_token_hash" {
				tok = attacker
			}
			w.refresh(tok).want(t, http.StatusUnauthorized, "")
			if w.auditCount(s, "auth.session_assurance_invalid") != 1 || w.alertCount("staff.session_integrity", s.ref.StaffID) != 1 {
				t.Fatalf("tampering not audited / alerted: audit %d alert %d", w.auditCount(s, "auth.session_assurance_invalid"),
					w.alertCount("staff.session_integrity", s.ref.StaffID))
			}
		})
	}
}

// S3-R10 migration of the format: a genuine S3 (v1) seal is retired QUIETLY - liveness refuses it (seal_legacy) and the
// refresh is a plain 401 - with no tampering audit row and no alert.
func TestStepUp_LegacyV1SealRetiredQuietly(t *testing.T) {
	w := newMFAW(t)
	s := w.withPlaceholderFactor(w.staff(&w.tenant, "support"))
	at := staffmfa.SessionTime(time.Now().Add(-time.Minute))
	var sidSeen uuid.UUID
	master := sha256.Sum256([]byte("adr0119-s3-httpserver-test|mac-1"))
	sub, err := hkdf.Key(sha256.New, master[:], nil, "staff-session-assurance-v1|m1", 32)
	if err != nil {
		t.Fatal(err)
	}
	us := func(t time.Time) string { return strconv.FormatInt(t.UTC().UnixMicro(), 10) }
	refresh, sid := w.plantSessionWith(s, at, at.Add(12*time.Hour), time.Now().Add(30*time.Minute), func(id uuid.UUID, _ []byte) []byte {
		sidSeen = id
		m := hmac.New(sha256.New, sub)
		m.Write([]byte("ss-v1|" + id.String() + "|staff|" + s.ref.StaffID.String() + "|" + s.ref.TenantID.String() + "|2|pwd,otp|" +
			us(at) + "|" + us(at) + "|" + us(at.Add(12*time.Hour))))
		return m.Sum(nil)
	})
	if sid != sidSeen {
		t.Fatal("plant id mismatch")
	}
	access := w.liveTok(s, sid, func(p *auth.StaffTokenParams) { p.AuthTime, p.MFAAt = at, at })
	before := w.reasonCount(observability.StaffLivenessReasonSealLegacy)
	w.refusedFor(w.do(http.MethodGet, "/v1/staff/me/mfa", access, nil), observability.StaffLivenessReasonSealLegacy, before)
	w.refresh(refresh).want(t, http.StatusUnauthorized, "")
	if w.auditCount(s, "auth.session_assurance_invalid") != 0 || w.alertCount("staff.session_integrity", s.ref.StaffID) != 0 {
		t.Fatal("a genuine legacy seal must not be reported as tampering")
	}
}

// Section 17.5: step-up racing a revocation. (a) The revocation holds the row lock and commits: the queued step-up
// then sees a revoked row - 401, mfa_at untouched. (b) The step-up commits first: the revocation then ends the
// session and the stepped-up token is refused by liveness. No deadlock in either order.
func TestStepUp_RacesRevocation_BothOrders(t *testing.T) {
	w := newMFAW(t)
	t.Run("revocation_first", func(t *testing.T) {
		w.t = t
		s := w.staff(&w.tenant, "support")
		e := w.enrol(s)
		sid := w.sidOf(e.pair.AccessToken)
		before := w.session(s, sid)
		ctx := context.Background()
		conn, err := w.owner.Raw().Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Rollback(ctx) }()
		for _, q := range []struct {
			sql  string
			args []any
		}{
			{`SELECT set_config('app.tenant_id', $1, true)`, []any{s.ref.TenantID.String()}},
			{`SELECT set_config('app.session_internal_op_id', $1, true)`, []any{sid.String()}},
			{`UPDATE sessions SET revoked_at = now() WHERE id = $1`, []any{sid}},
		} {
			if _, err := conn.Exec(ctx, q.sql, q.args...); err != nil {
				t.Fatal(err)
			}
		}
		code := w.code(s)
		var wg sync.WaitGroup
		var r mfaResp
		wg.Add(1)
		go func() { defer wg.Done(); r = w.stepUp(e.pair.AccessToken, code) }()
		w.waitLockWaiters(1)
		if err := conn.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		r.want(t, http.StatusUnauthorized, "session_revoked")
		if after := w.session(s, sid); !after.MFAAt.Equal(*before.MFAAt) {
			t.Fatal("a refused step-up wrote mfa_at")
		}
	})
	t.Run("step_up_first", func(t *testing.T) {
		w.t = t
		s := w.staff(&w.tenant, "support")
		e := w.enrol(s)
		r := w.stepUp(e.pair.AccessToken, w.code(s))
		r.want(t, http.StatusOK, "")
		var su stepUpWire
		r.decode(t, &su)
		w.post("/v1/auth/logout", map[string]string{"refresh_token": e.pair.RefreshToken}).want(t, http.StatusNoContent, "")
		w.do(http.MethodPost, regeneratePath, su.AccessToken, nil).want(t, http.StatusUnauthorized, "session_revoked")
	})
}

// The four classifiers over HTTP (section 8.2, 17.2): launch suspend none / activate critical; a forged action that
// differs between two JSON keys cannot split the classifier from the handler (both decode last-wins with the same
// decoder and type); financial policy none for a tighten-only tenant caller, critical for an author; alert resolve
// critical for staff.* and p1 kinds and an unknown id, none for a p3 non-staff kind and for a tenant caller; RG limit
// none for a value and critical for a removal (an increase stays refused by the RG domain).
func TestStepUp_Classifiers(t *testing.T) {
	w := newMFAW(t)
	admin := w.staff(nil, "platform_admin")
	tadmin := w.staff(&w.tenant, "tenant_admin")
	staleAdmin := authtest.LiveStaffTokenAged(t, w.rt, testStaffMFAKeys(), w.issuer, admin.ref.StaffID, uuid.Nil, auth.RolePlatformAdmin, 11*time.Minute)
	freshAdmin := authtest.LiveStaffToken(t, w.rt, testStaffMFAKeys(), w.issuer, admin.ref.StaffID, uuid.Nil, auth.RolePlatformAdmin)
	staleTenant := authtest.LiveStaffTokenAged(t, w.rt, testStaffMFAKeys(), w.issuer, tadmin.ref.StaffID, w.tenant.ID, auth.RoleTenantAdmin, 11*time.Minute)

	launch := "/v1/admin/tenants/" + w.tenant.ID.String() + "/launch-authorisations"
	notStepUpRefused(t, w.do(http.MethodPost, launch, staleAdmin, map[string]any{"action": "suspend", "reason_code": "x"}), "launch suspend")
	stepUpRefused(t, w.do(http.MethodPost, launch, staleAdmin, map[string]any{"action": "activate", "reason_code": "x"}), 300, "launch activate")
	dup := []byte(`{"action":"suspend","action":"activate","reason_code":"x"}`)
	stepUpRefused(t, w.doRaw(http.MethodPost, launch, staleAdmin, dup), 300, "duplicate action key (last wins)")
	fr := w.doRaw(http.MethodPost, launch, freshAdmin, dup)
	if strings.Contains(string(fr.body), "use the suspend routes") {
		t.Fatalf("the handler saw a different action than the classifier: %s", fr.body)
	}
	caseDup := []byte(`{"action":"suspend","Action":"activate","reason_code":"x"}`)
	stepUpRefused(t, w.doRaw(http.MethodPost, launch, staleAdmin, caseDup), 300, "case-folded duplicate key")

	policy := "/v1/admin/tenants/" + w.tenant.ID.String() + "/financial-policy-changes"
	notStepUpRefused(t, w.do(http.MethodPost, policy, staleTenant, map[string]any{}), "policy tighten-only caller")
	stepUpRefused(t, w.do(http.MethodPost, policy, staleAdmin, map[string]any{}), 300, "policy author")

	alertOf := func(kind string, attrs map[string]alerting.AttrValue, subject uuid.UUID) string {
		disc := "s4b-" + uuid.NewString()
		var id uuid.UUID
		if err := w.rt.WithPlatformAdmin(context.Background(), admin.ref.StaffID, func(ctx context.Context, tx pgx.Tx) error {
			if err := alerting.Raise(ctx, tx, alerting.Alert{Kind: alerting.Kind(kind), SubjectTenantID: subject, Discriminator: disc, Attributes: attrs}); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT id FROM alerts WHERE kind = $1 AND discriminator = $2`, kind, disc).Scan(&id)
		}); err != nil {
			t.Fatalf("seed alert %s: %v", kind, err)
		}
		return "/v1/admin/alerts/" + id.String() + "/resolve"
	}
	staffAttr := map[string]alerting.AttrValue{"staff_id": admin.ref.StaffID.String()}
	body := map[string]any{"reason_code": "s4b_test"}
	stepUpRefused(t, w.do(http.MethodPost, alertOf("staff.platform_mfa_recovery_used", staffAttr, uuid.Nil), staleAdmin, body), 300, "staff.* p3 alert")
	stepUpRefused(t, w.do(http.MethodPost, alertOf("reconciliation.run_failed", map[string]alerting.AttrValue{"stream": "ledger", "phase": "x", "sqlstate_class": "42"}, w.tenant.ID), staleAdmin, body), 300, "p1 alert")
	stepUpRefused(t, w.do(http.MethodPost, "/v1/admin/alerts/"+uuid.NewString()+"/resolve", staleAdmin, body), 300, "unknown alert")
	p2 := alertOf("alerting.unrouted", nil, uuid.Nil)
	r := w.do(http.MethodPost, p2, staleAdmin, body)
	notStepUpRefused(t, r, "p2 non-staff alert")
	if r.status != http.StatusOK {
		t.Fatalf("a non-security alert resolves without step-up: %d %s", r.status, r.body)
	}
	tr := w.do(http.MethodPost, alertOf("staff.platform_mfa_integrity", staffAttr, uuid.Nil), staleTenant, body)
	if tr.status != http.StatusForbidden || tr.code() != "forbidden" {
		t.Fatalf("a tenant caller keeps the handler's audited 403 (no step-up prompt): %d %s", tr.status, tr.body)
	}

	rg := "/v1/admin/players/" + uuid.NewString() + "/rg/limits"
	stepUpRefused(t, w.do(http.MethodPost, rg, staleTenant, map[string]any{"kind": "deposit", "value": nil}), 300, "RG removal")
	notStepUpRefused(t, w.do(http.MethodPost, rg, staleTenant, map[string]any{"kind": "deposit", "value": "100", "asset_code": "EUR", "period": "day"}), "RG value")
}

func (w *mfaW) doRaw(method, path, bearer string, body []byte) mfaResp {
	w.t.Helper()
	req, err := http.NewRequest(method, w.srv.URL+path, bytes.NewReader(body))
	if err != nil {
		w.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return mfaResp{status: resp.StatusCode, body: b, header: resp.Header}
}

// plantSessionWith is plantSession whose seal is replaced by sealFor(id, validV2MAC).
func (w *mfaW) plantSessionWith(s mfaStaff, authTime, absolute, expires time.Time, sealFor func(id uuid.UUID, mac []byte) []byte) (string, uuid.UUID) {
	w.t.Helper()
	var planted uuid.UUID
	var seal []byte
	// plantSession calls tamper before the INSERT; the id is known only from its result, so the replacement seal is
	// computed in two steps: plant with a recognisable seal, then rewrite the seal as the owner (SF040 lets the
	// seal change, never to NULL).
	tok, id := w.plantSession(s, authTime, absolute, expires, func(m []byte) []byte { seal = m; return m })
	planted = id
	if err := w.ownerSessionExec(s, id, `UPDATE sessions SET assurance_seal = $2 WHERE id = $1`, sealFor(planted, seal)); err != nil {
		w.t.Fatalf("replace seal: %v", err)
	}
	return tok, planted
}

// Section 9.1 / 17.2: the signer backstop over HTTP. A test route registered through staffRoute WITHOUT a step-up class
// that signs capability_grant:approve answers 500 (ErrStepUpMissing) even for a freshly logged-in staff member; the
// same route registered critical signs; an assertion for a different subject is refused; a route misclassified
// financial with a 7-minute-old step-up cannot sign a critical operation.
func TestStepUp_SignerBackstopOverHTTP(t *testing.T) {
	ownerURL := os.Getenv("TEST_DATABASE_URL")
	prooftest.Install(t, ownerURL)
	w := newMFAW(t)
	admin := w.staff(nil, "platform_admin")
	deps := Deps{DB: w.rt, AuthIssuer: w.issuer, StaffMFA: w.m, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	signer := func(otherSubject bool) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			tc, _ := tenant.FromContext(r.Context())
			actor := uuid.MustParse(tc.Subject)
			if otherSubject {
				actor = uuid.New()
			}
			err := w.rt.WithoutTenant(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
				return actorproof.Default().Attach(ctx, tx, actorproof.Claims{Actor: actor, Scope: actorproof.ScopePlatform,
					Operation: actorproof.OpGrantApprove, Target: uuid.NewString(), PayloadHash: strings.Repeat("a", 64)})
			})
			switch {
			case errors.Is(err, actorproof.ErrStepUpMissing):
				rw.WriteHeader(http.StatusInternalServerError)
			case err != nil:
				rw.WriteHeader(http.StatusTeapot)
			default:
				rw.WriteHeader(http.StatusNoContent)
			}
		})
	}
	mux := http.NewServeMux()
	mux.Handle("POST /s4b-test/none", staffRoute(deps, stepUpNone, signer(false)))
	mux.Handle("POST /s4b-test/critical", staffRoute(deps, stepUpCritical, signer(false)))
	mux.Handle("POST /s4b-test/critical-other", staffRoute(deps, stepUpCritical, signer(true)))
	mux.Handle("POST /s4b-test/financial", staffRoute(deps, stepUpFinancial, signer(false)))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	call := func(path, tok string) int {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	fresh := authtest.LiveStaffToken(t, w.rt, testStaffMFAKeys(), w.issuer, admin.ref.StaffID, uuid.Nil, auth.RolePlatformAdmin)
	seven := authtest.LiveStaffTokenAged(t, w.rt, testStaffMFAKeys(), w.issuer, admin.ref.StaffID, uuid.Nil, auth.RolePlatformAdmin, 7*time.Minute)
	for path, want := range map[string]int{"/s4b-test/none": 500, "/s4b-test/critical": 204, "/s4b-test/critical-other": 500} {
		if got := call(path, fresh); got != want {
			t.Errorf("%s with a fresh session: %d, want %d", path, got, want)
		}
	}
	if got := call("/s4b-test/financial", seven); got != 500 {
		t.Errorf("a financial route's 7-minute-old step-up must not sign a critical operation: %d", got)
	}
}

// Section 17.7: the step-up and regeneration never log, audit or echo a code, a token or a recovery code.
func TestStepUp_SecretHygiene(t *testing.T) {
	w := newMFAW(t)
	s := w.staff(&w.tenant, "support")
	e := w.enrol(s)
	_, _, _ = w.agedSession(s, 11*time.Minute)
	code := w.code(s)
	r := w.stepUp(e.pair.AccessToken, code)
	r.want(t, http.StatusOK, "")
	var su stepUpWire
	r.decode(t, &su)
	wrong := w.wrong(s)
	w.stepUp(su.AccessToken, wrong).want(t, http.StatusUnauthorized, "invalid_mfa_code")
	rg := w.do(http.MethodPost, regeneratePath, su.AccessToken, nil)
	rg.want(t, http.StatusOK, "")
	var codes recoveryCodesWire
	rg.decode(t, &codes)
	secrets := append([]string{code, wrong, su.AccessToken, e.pair.AccessToken, e.pair.RefreshToken}, codes.RecoveryCodes...)
	if found := containsAny(w.logs.String(), secrets); len(found) > 0 {
		t.Fatalf("secrets in the server log: %v", found)
	}
	if found := containsAny(strings.Join(w.auditMetadata(s), "\n"), secrets); len(found) > 0 {
		t.Fatalf("secrets in audit metadata: %v", found)
	}
	if strings.Contains(w.logs.String(), "max_age_seconds") && strings.Contains(w.logs.String(), code) {
		t.Fatal("a refusal log carries the code")
	}
}

// Security review of S4b, finding 1: POST /v1/admin/alerting/routes checks alert:route_manage INSIDE its handler so a
// refusal is audited. alertRouteManageClass keeps that: a stale tenant_admin (no permission, tenant session) and a
// stale support caller get the handler's audited 403 forbidden (alert_route.create denied, one row each), never a
// step-up prompt; a stale platform_admin is asked to step up (critical, 300 s) and nothing is audited for it.
func TestStepUp_AlertRouteCreate_UnauthorisedProbingKeepsTheAuditedRefusal(t *testing.T) {
	w := newMFAW(t)
	admin := w.staff(nil, "platform_admin")
	staleAdmin := authtest.LiveStaffTokenAged(t, w.rt, testStaffMFAKeys(), w.issuer, admin.ref.StaffID, uuid.Nil, auth.RolePlatformAdmin, 11*time.Minute)
	const path = "/v1/admin/alerting/routes"
	body := map[string]any{"severity": "p1", "channel_kind": "log", "enabled": true, "reason_code": "initial_setup"}
	for _, role := range []struct {
		name string
		r    auth.Role
	}{{"tenant_admin", auth.RoleTenantAdmin}, {"support", auth.RoleSupport}} {
		s := w.staff(&w.tenant, role.name)
		stale := authtest.LiveStaffTokenAged(t, w.rt, testStaffMFAKeys(), w.issuer, s.ref.StaffID, w.tenant.ID, role.r, 11*time.Minute)
		r := w.do(http.MethodPost, path, stale, body)
		if r.status != http.StatusForbidden || r.code() != "forbidden" {
			t.Fatalf("%s: want the handler's 403 forbidden (no step-up prompt), got %d %s", role.name, r.status, r.body)
		}
		if n := w.auditCount(s, "alert_route.create"); n != 1 {
			t.Fatalf("%s: the refusal must be audited exactly once, got %d", role.name, n)
		}
	}
	stepUpRefused(t, w.do(http.MethodPost, path, staleAdmin, body), 300, "alert route create, platform admin")
	if n := w.auditCount(admin, "alert_route.create"); n != 0 {
		t.Fatalf("the step-up refusal runs before the handler: no audit row, got %d", n)
	}
}
