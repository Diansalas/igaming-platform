//go:build integration

package httpserver

// ADR 0119 slice S4a, against a real database as igaming_runtime: the per-request liveness check of the staff route
// chain (section 7.3), its refusal matrix, its fail-closed fault handling, its connection discipline, the staff refresh
// re-check (S3-R11), the query plan and its latency.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/auth/authtest"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/providercred"
	"github.com/Diansalas/igaming-platform/internal/staffmfa/store"
)

// liveTok mints an aal2 access token for s over the session sid, with optional claim mutations (a test-only
// issuer of the server's own key: the token is genuine, only the session behind it varies).
func (w *mfaW) liveTok(s mfaStaff, sid uuid.UUID, mut func(*auth.StaffTokenParams)) string {
	w.t.Helper()
	p := unitStaffParams(s.ref.StaffID, s.ref.TenantID, auth.Role(s.role))
	p.SessionID = sid
	if mut != nil {
		mut(&p)
	}
	tok, err := w.issuer.IssueStaff(p, 10*time.Minute)
	if err != nil {
		w.t.Fatal(err)
	}
	return tok
}

// sidOf returns the sid claim of an access token issued by the server.
func (w *mfaW) sidOf(access string) uuid.UUID {
	w.t.Helper()
	c, err := w.issuer.Verify(access)
	if err != nil {
		w.t.Fatal(err)
	}
	return uuid.MustParse(c.SessionID)
}

// withPlaceholderFactor gives s an active (placeholder, never verifiable) second factor as the runtime role, for tests
// that plant sessions directly: since S4a a staff refresh and every staff request need a usable factor (S3-R11).
func (w *mfaW) withPlaceholderFactor(s mfaStaff) mfaStaff {
	w.t.Helper()
	if err := store.WithScope(context.Background(), w.rt, s.ref, func(ctx context.Context, tx pgx.Tx) error {
		return authtest.EnsurePlaceholderFactor(ctx, tx, s.ref.StaffID)
	}); err != nil {
		w.t.Fatalf("placeholder factor: %v", err)
	}
	return s
}

func (w *mfaW) ownerExec(s mfaStaff, sql string, args ...any) {
	w.t.Helper()
	w.inScope(s, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, sql, args...)
		return err
	})
}

// refusedFor asserts r is the single liveness refusal and that the log recorded reason for it.
func (w *mfaW) refusedFor(r mfaResp, reason observability.StaffLivenessReason, before int) {
	w.t.Helper()
	r.want(w.t, http.StatusUnauthorized, "session_revoked")
	if !strings.Contains(r.header.Get("WWW-Authenticate"), `error="invalid_token"`) {
		w.t.Errorf("a liveness refusal must carry WWW-Authenticate invalid_token, got %q", r.header.Get("WWW-Authenticate"))
	}
	if got := strings.Count(w.logs.String(), `"reason":"`+string(reason)+`"`); got != before+1 {
		w.t.Errorf("want one more %q refusal logged (%d -> %d)", reason, before, got)
	}
}

func (w *mfaW) reasonCount(reason observability.StaffLivenessReason) int {
	return strings.Count(w.logs.String(), `"reason":"`+string(reason)+`"`)
}

// displayName reads the staff member's display_name as the owner (a probe that a refused write wrote nothing).
func (w *mfaW) displayName(s mfaStaff) *string {
	w.t.Helper()
	var v *string
	w.inScope(s, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT display_name FROM staff_users WHERE id = $1`, s.ref.StaffID).Scan(&v)
	})
	return v
}

// The refusal matrix (sections 7.3, 17.1, 17.5): every cause is one 401 session_revoked, the handler is never reached
// (a refused display-name PATCH writes nothing), and the cause is recorded. Tenant and platform staff.
func TestStaffLiveness_RefusalMatrix(t *testing.T) {
	w := newMFAW(t)
	for _, scope := range []struct {
		name   string
		tenant *identity.Tenant
		role   string
	}{{"tenant", &w.tenant, "tenant_admin"}, {"platform", nil, "platform_admin"}} {
		t.Run(scope.name, func(t *testing.T) {
			w.t = t
			enrolled := func() (mfaStaff, tokenPairResponse) {
				s := w.staff(scope.tenant, scope.role)
				return s, w.enrol(s).pair
			}
			me := func(tok string) mfaResp { return w.do(http.MethodGet, "/v1/staff/me/mfa", tok, nil) }
			rename := func(tok string) mfaResp {
				return w.do(http.MethodPatch, "/v1/admin/staff/me/display-name", tok, map[string]string{"display_name": "Renamed Probe"})
			}

			// Control: a live session passes on a read and a write.
			s, pair := enrolled()
			me(pair.AccessToken).want(t, http.StatusOK, "")
			if r := rename(pair.AccessToken); r.status >= 300 {
				t.Fatalf("control rename: %d %s", r.status, r.body)
			}

			// Logout revokes the session: the still-unexpired access token is refused on its next request.
			s, pair = enrolled()
			n := w.reasonCount(observability.StaffLivenessReasonRevoked)
			w.post("/v1/auth/logout", map[string]string{"refresh_token": pair.RefreshToken}).want(t, http.StatusNoContent, "")
			w.refusedFor(me(pair.AccessToken), observability.StaffLivenessReasonRevoked, n)
			// The refusal line identifies the request (security review R3(a), code review CR 4).
			m, raw := livenessLogLine(t, w.logs.String(), "staff_liveness_refused")
			wantScope := "platform"
			if scope.tenant != nil {
				wantScope = scope.tenant.ID.String()
			}
			for k, want := range map[string]string{"staff_id": s.ref.StaffID.String(), "token_tenant": wantScope,
				"sid": w.sidOf(pair.AccessToken).String(), "method": "GET", "route": "GET /v1/staff/me/mfa", "client_ip": "127.0.0.1"} {
				if m[k] != want {
					t.Errorf("staff_liveness_refused %s = %v, want %s", k, m[k], want)
				}
			}
			if strings.Count(raw, `"request_id"`) != 1 || strings.Contains(raw, pair.AccessToken) {
				t.Errorf("refusal line: one request_id, never the token: %s", raw)
			}
			before := w.displayName(s)
			w.refusedFor(rename(pair.AccessToken), observability.StaffLivenessReasonRevoked, n+1)
			if after := w.displayName(s); (before == nil) != (after == nil) || (before != nil && *before != *after) {
				t.Fatal("a refused write reached the handler")
			}

			// Suspended staff member (status changed by the owner: there is no lifecycle route before S6).
			s, pair = enrolled()
			n = w.reasonCount(observability.StaffLivenessReasonStaffNotActive)
			w.ownerExec(s, `UPDATE staff_users SET status = 'suspended' WHERE id = $1`, s.ref.StaffID)
			w.refusedFor(me(pair.AccessToken), observability.StaffLivenessReasonStaffNotActive, n)
			// ... and the refresh is refused too (pre-existing status re-check).
			w.refresh(pair.RefreshToken).want(t, http.StatusForbidden, "forbidden")

			// Role changed mid-session: the old token is refused; a refresh issues the new role, which passes.
			if scope.tenant != nil {
				s, pair = enrolled()
				n = w.reasonCount(observability.StaffLivenessReasonRoleMismatch)
				w.ownerExec(s, `UPDATE staff_users SET role = 'compliance' WHERE id = $1`, s.ref.StaffID)
				w.refusedFor(me(pair.AccessToken), observability.StaffLivenessReasonRoleMismatch, n)
				r := w.refresh(pair.RefreshToken)
				r.want(t, http.StatusOK, "")
				var next tokenPairResponse
				r.decode(t, &next)
				if c, err := w.issuer.Verify(next.AccessToken); err != nil || c.Role != auth.RoleCompliance {
					t.Fatalf("refresh after a role change must carry the new role: %v %+v", err, c)
				}
				me(next.AccessToken).want(t, http.StatusOK, "")
			}

			// Staff version mismatch (sv ahead of the row; the column arrives with 0139, until then every row is 0).
			s, pair = enrolled()
			n = w.reasonCount(observability.StaffLivenessReasonVersionMismatch)
			w.refusedFor(me(w.liveTok(s, w.sidOf(pair.AccessToken), func(p *auth.StaffTokenParams) { p.StaffVersion = 1 })),
				observability.StaffLivenessReasonVersionMismatch, n)
			me(w.liveTok(s, w.sidOf(pair.AccessToken), nil)).want(t, http.StatusOK, "") // the same sid at sv 0 passes

			// Hard-locked factor (S3-R11): the session ends on the next request, and the refresh is refused.
			s, pair = enrolled()
			n = w.reasonCount(observability.StaffLivenessReasonFactorUnusable)
			// As the RUNTIME role (hard_locked_at is a granted column): what the lockout code itself writes.
			if err := store.WithScope(context.Background(), w.rt, s.ref, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE staff_mfa_factors SET hard_locked_at = now() WHERE staff_user_id = $1 AND status = 'active'`, s.ref.StaffID)
				return err
			}); err != nil {
				t.Fatalf("hard-lock: %v", err)
			}
			w.refusedFor(me(pair.AccessToken), observability.StaffLivenessReasonFactorUnusable, n)
			w.refresh(pair.RefreshToken).want(t, http.StatusForbidden, "forbidden")

			// Revoked factor (the S3-R1 owner reset procedure revokes the factor): same.
			s, pair = enrolled()
			n = w.reasonCount(observability.StaffLivenessReasonFactorUnusable)
			w.ownerExec(s, `UPDATE staff_mfa_factors SET status = 'revoked', revoked_at = now(), revoked_reason = 'reset'
				WHERE staff_user_id = $1 AND status = 'active'`, s.ref.StaffID)
			w.refusedFor(me(pair.AccessToken), observability.StaffLivenessReasonFactorUnusable, n)
			w.refresh(pair.RefreshToken).want(t, http.StatusForbidden, "forbidden")

			// Unknown sid, another staff member's sid, the right sid under another tenant claim: no row.
			s, pair = enrolled()
			other, otherPair := enrolled()
			n = w.reasonCount(observability.StaffLivenessReasonNoSession)
			w.refusedFor(me(w.liveTok(s, uuid.New(), nil)), observability.StaffLivenessReasonNoSession, n)
			w.refusedFor(me(w.liveTok(s, w.sidOf(otherPair.AccessToken), nil)), observability.StaffLivenessReasonNoSession, n+1)
			w.refusedFor(me(w.liveTok(other, w.sidOf(pair.AccessToken), nil)), observability.StaffLivenessReasonNoSession, n+2)
			crossTenant := w.other.ID
			if scope.tenant == nil {
				crossTenant = w.tenant.ID
			}
			w.refusedFor(me(w.liveTok(s, w.sidOf(pair.AccessToken), func(p *auth.StaffTokenParams) { p.TenantID = crossTenant })),
				observability.StaffLivenessReasonNoSession, n+3)
			if scope.tenant != nil { // a tenant staff member's sid presented as a platform token
				w.refusedFor(me(w.liveTok(s, w.sidOf(pair.AccessToken), func(p *auth.StaffTokenParams) { p.TenantID = uuid.Nil })),
					observability.StaffLivenessReasonNoSession, n+4)
			}

			// Past the absolute lifetime (database clock): a sealed session planted with an absolute expiry in the past.
			s, _ = enrolled()
			n = w.reasonCount(observability.StaffLivenessReasonAbsoluteExpired)
			past := time.Now().Add(-13 * time.Hour)
			_, sid := w.plantSession(s, past, past.Add(12*time.Hour), past.Add(30*time.Minute), nil)
			w.refusedFor(me(w.liveTok(s, sid, func(p *auth.StaffTokenParams) { p.AuthTime, p.MFAAt = past, past })),
				observability.StaffLivenessReasonAbsoluteExpired, n)
		})
	}
}

// Fail closed: a database fault is 503 service_unavailable (never a pass, never a 401), counted and logged; the
// 500 ms statement timeout bounds a blocked query.
func TestStaffLiveness_FaultIs503FailClosed(t *testing.T) {
	w := newMFAW(t)
	s := w.staff(&w.tenant, "support")
	pair := w.enrol(s).pair

	// (a) A dead pool.
	dead, err := db.Connect(context.Background(), os.Getenv("TEST_RUNTIME_DATABASE_URL"), 2, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	dead.Close()
	logs := &syncBuffer{}
	srv := httptest.NewServer(New(Deps{Logger: slog.New(slog.NewJSONHandler(logs, nil)), DB: dead, AuthIssuer: w.issuer,
		ServiceName: "s4a", AccessTokenTTL: time.Minute, RefreshTokenTTL: time.Hour, StaffMFA: w.m,
		PersonResolver: identityresolution.NewMockPersonResolver(), AuthRateLimitPerMinute: -1}))
	defer srv.Close()
	for _, path := range []string{"/v1/staff/me/mfa", "/v1/admin/audit-log"} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+pair.AccessToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(b), `"code":"service_unavailable"`) {
			t.Fatalf("%s with a dead pool: %d %s", path, resp.StatusCode, b)
		}
	}
	if strings.Count(logs.String(), `"msg":"staff_liveness_failed"`) != 2 {
		t.Fatalf("the fault must be logged once per request: %s", logs.String())
	}

	// (b) A blocked query: the owner holds an ACCESS EXCLUSIVE lock on staff_mfa_factors; the liveness SELECT waits
	// and is cancelled by its own 500 ms statement timeout (SQLSTATE 57014), well before the 2 s overall deadline. Both
	// a COLD connection (the statement is parsed - and its locks taken - in this request) and a WARM one (the statement
	// is cached) are checked: a timeout set in the same pipeline as the query does not bound the cold case.
	for _, warm := range []bool{false, true} {
		fresh, err := db.Connect(context.Background(), os.Getenv("TEST_RUNTIME_DATABASE_URL"), 1, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		flogs := &syncBuffer{}
		fsrv := httptest.NewServer(New(Deps{Logger: slog.New(slog.NewJSONHandler(flogs, nil)), DB: fresh, AuthIssuer: w.issuer,
			ServiceName: "s4a", AccessTokenTTL: time.Minute, RefreshTokenTTL: time.Hour, StaffMFA: w.m,
			PersonResolver: identityresolution.NewMockPersonResolver(), AuthRateLimitPerMinute: -1}))
		get := func() (int, string, time.Duration) {
			req, _ := http.NewRequest(http.MethodGet, fsrv.URL+"/v1/staff/me/mfa", nil)
			req.Header.Set("Authorization", "Bearer "+pair.AccessToken)
			start := time.Now()
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			return resp.StatusCode, string(b), time.Since(start)
		}
		if warm {
			if code, body, _ := get(); code != http.StatusOK {
				t.Fatalf("warm-up: %d %s", code, body)
			}
		}
		lockTx, err := w.owner.Raw().Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := lockTx.Exec(context.Background(), `LOCK TABLE staff_mfa_factors IN ACCESS EXCLUSIVE MODE`); err != nil {
			t.Fatal(err)
		}
		code, body, elapsed := get()
		_ = lockTx.Rollback(context.Background())
		if code != http.StatusServiceUnavailable || !strings.Contains(body, `"code":"service_unavailable"`) {
			t.Fatalf("warm=%v: blocked liveness check answered %d %s", warm, code, body)
		}
		if elapsed < 450*time.Millisecond || elapsed > 1800*time.Millisecond {
			t.Fatalf("warm=%v: the blocked liveness check answered after %s; the 500 ms statement timeout should cut it", warm, elapsed)
		}
		if !strings.Contains(flogs.String(), "57014") {
			t.Fatalf("warm=%v: the refusal must come from the statement timeout (57014): %s", warm, flogs.String())
		}
		t.Logf("warm=%v: blocked liveness check answered 503 after %s", warm, elapsed)
		// Released: the same token passes again.
		if code, body, _ := get(); code != http.StatusOK {
			t.Fatalf("warm=%v: after release: %d %s", warm, code, body)
		}
		fsrv.Close()
		fresh.Close()
	}
}

// The liveness transaction releases its connection before the handler acquires its own: with a ONE-connection pool a
// staff read and a staff write still complete (a nested acquisition would block forever).
func TestStaffLiveness_ReleasesConnectionBeforeHandler(t *testing.T) {
	w := newMFAW(t)
	s := w.staff(&w.tenant, "tenant_admin")
	pair := w.enrol(s).pair
	one, err := db.Connect(context.Background(), os.Getenv("TEST_RUNTIME_DATABASE_URL"), 1, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	srv := httptest.NewServer(New(Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: one, AuthIssuer: w.issuer,
		ServiceName: "s4a", AccessTokenTTL: time.Minute, RefreshTokenTTL: time.Hour, StaffMFA: w.m,
		PersonResolver: identityresolution.NewMockPersonResolver(), AuthRateLimitPerMinute: -1}))
	defer srv.Close()
	client := &http.Client{Timeout: 10 * time.Second}
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/staff/me/mfa", ""},
		{http.MethodPatch, "/v1/admin/staff/me/display-name", `{"display_name":"One Conn"}`},
		{http.MethodGet, "/v1/admin/audit-log", ""},
	} {
		req, _ := http.NewRequest(tc.method, srv.URL+tc.path, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer "+pair.AccessToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s with a one-connection pool: %v (nested pool acquisition?)", tc.method, tc.path, err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode >= 300 {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, resp.StatusCode, b)
		}
	}
}

// Every golden route refuses a revoked session (401 session_revoked) with a real database: the chain is on every
// route, not only on the probes above. A live platform admin's reads are never refused by liveness.
func TestStaffLiveness_EveryGoldenRoute(t *testing.T) {
	w := newMFAW(t, func(d *Deps) {
		d.PaymentOrchestrator, d.ProviderCredentials = &payments.Orchestrator{}, &providercred.Subsystem{}
		d.SportsbookEnabled, d.SportsbookSettlementSimulationEnabled = true, true
	})
	admin := w.staff(nil, "platform_admin")
	revoked := w.enrol(admin).pair
	w.post("/v1/auth/logout", map[string]string{"refresh_token": revoked.RefreshToken}).want(t, http.StatusNoContent, "")
	patterns := make([]string, 0, len(staffRouteGolden))
	for p := range staffRouteGolden {
		patterns = append(patterns, p)
	}
	sort.Strings(patterns)
	for _, p := range patterns {
		method, path := concretePath(p)
		r := w.do(method, path, revoked.AccessToken, map[string]string{})
		if r.status != http.StatusUnauthorized || r.code() != "session_revoked" {
			t.Errorf("%s with a revoked session: %d %s", p, r.status, r.body)
		}
	}

	// Live reads (a fresh world without the zero-value subsystems, so every mounted handler is real).
	w2 := newMFAW(t)
	admin2 := w2.staff(nil, "platform_admin")
	live := w2.enrol(admin2).pair
	checked := 0
	for _, p := range patterns {
		method, path := concretePath(p)
		if method != http.MethodGet {
			continue
		}
		r := w2.do(method, path, live.AccessToken, nil)
		if r.status == http.StatusNotFound && !strings.Contains(string(r.body), `"code"`) {
			continue // not mounted in this world (Go's own 404)
		}
		checked++
		if r.code() == "session_revoked" {
			t.Errorf("%s with a live session was refused by the chain: %d %s", p, r.status, r.body)
		}
	}
	if checked < 50 {
		t.Errorf("only %d live GET routes checked", checked)
	}
	// The chain itself never refused or failed: every handler-level answer above (400, 403, 503 "not enabled", ...)
	// came from the route, not from liveness.
	if n := strings.Count(w2.logs.String(), `"msg":"staff_liveness_refused"`) + strings.Count(w2.logs.String(), `"msg":"staff_liveness_failed"`); n != 0 {
		t.Errorf("liveness refused or failed %d live requests", n)
	}
}

// The staff refresh re-checks the second factor (S3-R11): a healthy refresh passes; once the factor is hard-locked
// or revoked, the refresh is refused like a suspended account.
func TestStaffRefresh_RechecksFactor(t *testing.T) {
	w := newMFAW(t)
	for _, scope := range []*identity.Tenant{&w.tenant, nil} {
		role := "support"
		if scope == nil {
			role = "platform_admin"
		}
		s := w.staff(scope, role)
		pair := w.enrol(s).pair
		r := w.refresh(pair.RefreshToken)
		r.want(t, http.StatusOK, "")
		r.decode(t, &pair)
		w.ownerExec(s, `UPDATE staff_mfa_factors SET hard_locked_at = now() WHERE staff_user_id = $1 AND status = 'active'`, s.ref.StaffID)
		w.refresh(pair.RefreshToken).want(t, http.StatusForbidden, "forbidden")
	}
}

// The liveness query uses indexes (sessions primary key, staff_users primary key, a staff_mfa_factors index on
// staff_user_id), proven with sequential scans disabled; the default plan and the p95 latency over 300 checks on
// this database are logged as evidence.
func TestStaffLiveness_QueryPlanAndLatency(t *testing.T) {
	w := newMFAW(t)
	for _, scope := range []*identity.Tenant{&w.tenant, nil} {
		role := "tenant_admin"
		if scope == nil {
			role = "platform_admin"
		}
		s := w.staff(scope, role)
		pair := w.enrol(s).pair
		sid := w.sidOf(pair.AccessToken)
		var tenantArg any
		if scope != nil {
			tenantArg = scope.ID
		}
		explain := func(seqscanOff bool) string {
			var lines []string
			if err := w.rt.WithPrincipalScope(context.Background(), s.ref.TenantID, s.ref.StaffID, func(ctx context.Context, tx pgx.Tx) error {
				if seqscanOff {
					if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
						return err
					}
				}
				rows, err := tx.Query(ctx, "EXPLAIN "+staffLivenessSQL, sid, s.ref.StaffID, tenantArg)
				if err != nil {
					return err
				}
				defer rows.Close()
				for rows.Next() {
					var l string
					if err := rows.Scan(&l); err != nil {
						return err
					}
					lines = append(lines, l)
				}
				return rows.Err()
			}); err != nil {
				t.Fatal(err)
			}
			return strings.Join(lines, "\n")
		}
		plan := explain(true)
		// Each of the three tables is reached through an index (which one is the planner's choice: on a small table it
		// may prefer idx_sessions_principal over sessions_pkey, both of which bound the scan to the caller's rows).
		for table, indexes := range map[string][]string{
			"sessions s":          {"sessions_pkey", "idx_sessions_principal"},
			"staff_users su":      {"staff_users_pkey", "staff_users_id_tenant_unique"},
			"staff_mfa_factors f": {"staff_mfa_factors_one_active", "idx_staff_mfa_factors_staff"},
		} {
			found := false
			for _, ix := range indexes {
				if strings.Contains(plan, "Index Scan using "+ix+" on "+table) || strings.Contains(plan, "Index Only Scan using "+ix+" on "+table) {
					found = true
				}
			}
			if !found {
				t.Errorf("%s plan does not reach %s through one of %v:\n%s", role, table, indexes, plan)
			}
		}
		if strings.Contains(plan, "Seq Scan") {
			t.Errorf("%s plan has a sequential scan with seqscan disabled (no usable index):\n%s", role, plan)
		}
		t.Logf("%s liveness plan (enable_seqscan=off):\n%s\ndefault plan:\n%s", role, plan, explain(false))

		sess := auth.StaffSession{SessionID: sid, AAL: auth.StaffAAL, StaffVersion: 0}
		var d []time.Duration
		for i := 0; i < 300; i++ {
			start := time.Now()
			reason, _, err := checkStaffLiveness(context.Background(), w.rt, testStaffMFA(), s.ref.TenantID, s.ref.StaffID, sess, auth.Role(role))
			d = append(d, time.Since(start))
			if err != nil || reason != observability.StaffLivenessReasonOK {
				t.Fatalf("check %d: %v %s", i, err, reason)
			}
		}
		sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
		p50, p95, p99 := d[len(d)/2], d[len(d)*95/100], d[len(d)*99/100]
		t.Logf("%s liveness latency over %d checks: p50=%s p95=%s p99=%s max=%s", role, len(d), p50, p95, p99, d[len(d)-1])
		if p95 > 250*time.Millisecond {
			t.Errorf("p95 %s is too close to the 500 ms statement timeout", p95)
		}
	}
}

// The liveness transaction is READ ONLY: a write slipped into it would fail (defence in depth for the helper).
func TestQueryRowPrincipalScopeReadOnly_IsReadOnlyAndScoped(t *testing.T) {
	w := newMFAW(t)
	s := w.staff(&w.tenant, "support")
	err := w.rt.QueryRowPrincipalScopeReadOnly(context.Background(), s.ref.TenantID, s.ref.StaffID, 500*time.Millisecond,
		`UPDATE staff_users SET status = status WHERE id = $1 RETURNING id`, []any{s.ref.StaffID}, func(r pgx.Row) error {
			var id uuid.UUID
			return r.Scan(&id)
		})
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("a write in the liveness transaction must fail as read-only, got %v", err)
	}
	var gucs [3]string
	if err := w.rt.QueryRowPrincipalScopeReadOnly(context.Background(), s.ref.TenantID, s.ref.StaffID, 500*time.Millisecond,
		`SELECT current_setting('app.tenant_id', true), current_setting('app.principal_id', true), current_setting('statement_timeout')`,
		nil, func(r pgx.Row) error { return r.Scan(&gucs[0], &gucs[1], &gucs[2]) }); err != nil {
		t.Fatal(err)
	}
	if gucs != [3]string{s.ref.TenantID.String(), s.ref.StaffID.String(), "500ms"} {
		t.Fatalf("scope GUCs %v", gucs)
	}
	if err := w.rt.QueryRowPrincipalScopeReadOnly(context.Background(), uuid.Nil, s.ref.StaffID, 500*time.Millisecond,
		`SELECT current_setting('app.tenant_id', true), current_setting('app.principal_id', true), current_setting('statement_timeout')`,
		nil, func(r pgx.Row) error { return r.Scan(&gucs[0], &gucs[1], &gucs[2]) }); err != nil {
		t.Fatal(err)
	}
	if gucs[0] != "" || gucs[1] != s.ref.StaffID.String() {
		t.Fatalf("platform scope GUCs %v", gucs)
	}
	// The timeout is transaction-local: the pooled connection's own setting is untouched afterwards.
	var after string
	if err := w.rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT current_setting('statement_timeout')`).Scan(&after)
	}); err != nil {
		t.Fatal(err)
	}
	if after == "500ms" {
		t.Fatal("the statement timeout leaked out of the liveness transaction")
	}
	if err := w.rt.QueryRowPrincipalScopeReadOnly(context.Background(), s.ref.TenantID, s.ref.StaffID, 500*time.Millisecond,
		`SELECT 1 WHERE false`, nil, func(r pgx.Row) error { var x int; return r.Scan(&x) }); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("no row must be pgx.ErrNoRows exactly, got %v", err)
	}
	for _, bad := range []time.Duration{0, time.Microsecond, 11 * time.Second} {
		if err := w.rt.QueryRowPrincipalScopeReadOnly(context.Background(), s.ref.TenantID, s.ref.StaffID, bad, `SELECT 1`, nil,
			func(pgx.Row) error { return nil }); err == nil {
			t.Errorf("timeout %s must be refused", bad)
		}
	}
}

// Security review C1 (executed live against the first S4a cut): an access token minted BEFORE a refresh names the
// replaced predecessor session; ending the session (logout with the current refresh token, DELETE /v1/me/sessions/{sid}
// of the current session, or refresh-token reuse) must revoke the whole chain so that token is refused too. The
// active-session list shows only the chain head. Tenant and platform staff.
func TestStaffLiveness_SessionEndRevokesWholeChain(t *testing.T) {
	w := newMFAW(t)
	for _, scope := range []struct {
		name   string
		tenant *identity.Tenant
		role   string
	}{{"tenant", &w.tenant, "support"}, {"platform", nil, "platform_admin"}} {
		t.Run(scope.name, func(t *testing.T) {
			w.t = t
			me := func(tok string) mfaResp { return w.do(http.MethodGet, "/v1/staff/me/mfa", tok, nil) }
			rename := func(tok string) mfaResp {
				return w.do(http.MethodPatch, "/v1/admin/staff/me/display-name", tok, map[string]string{"display_name": "Chain Probe"})
			}
			refresh := func(p tokenPairResponse) tokenPairResponse {
				r := w.refresh(p.RefreshToken)
				r.want(t, http.StatusOK, "")
				var next tokenPairResponse
				r.decode(t, &next)
				return next
			}
			for _, end := range []string{"logout", "delete", "delete a middle link", "reuse", "logout after the head was revoked"} {
				s := w.staff(scope.tenant, scope.role)
				p0 := w.enrol(s).pair
				p1 := refresh(p0)
				p2 := refresh(p1)
				for _, tok := range []string{p0.AccessToken, p1.AccessToken, p2.AccessToken} {
					me(tok).want(t, http.StatusOK, "") // every token of a live chain passes
				}
				// The list shows one session (the head), never the replaced links.
				lr := w.do(http.MethodGet, "/v1/me/sessions", p2.AccessToken, nil)
				lr.want(t, http.StatusOK, "")
				var list []sessionResponse
				lr.decode(t, &list)
				if len(list) != 1 || list[0].ID != w.sidOf(p2.AccessToken).String() {
					t.Fatalf("%s: the active-session list must be the chain head only, got %s", end, lr.body)
				}
				switch end {
				case "logout":
					w.post("/v1/auth/logout", map[string]string{"refresh_token": p2.RefreshToken}).want(t, http.StatusNoContent, "")
				case "delete":
					w.do(http.MethodDelete, "/v1/me/sessions/"+w.sidOf(p2.AccessToken).String(), p2.AccessToken, nil).want(t, http.StatusNoContent, "")
				case "delete a middle link":
					// A replaced link is the caller's own session too; deleting it ends the chain both ways.
					w.do(http.MethodDelete, "/v1/me/sessions/"+w.sidOf(p1.AccessToken).String(), p2.AccessToken, nil).want(t, http.StatusNoContent, "")
				case "reuse":
					w.refresh(p1.RefreshToken).want(t, http.StatusUnauthorized, "unauthorized") // a replaced token: reuse
				case "logout after the head was revoked":
					// The head is revoked on its own (the owner procedure's guarded revocation); its predecessors are
					// still live until the logout of that head revokes them.
					if err := w.ownerSessionExec(s, w.sidOf(p2.AccessToken), `UPDATE sessions SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`); err != nil {
						t.Fatal(err)
					}
					me(p0.AccessToken).want(t, http.StatusOK, "")
					w.post("/v1/auth/logout", map[string]string{"refresh_token": p2.RefreshToken}).want(t, http.StatusNoContent, "")
				}
				for i, tok := range []string{p0.AccessToken, p1.AccessToken, p2.AccessToken} {
					me(tok).want(t, http.StatusUnauthorized, "session_revoked")
					rename(tok).want(t, http.StatusUnauthorized, "session_revoked")
					if t.Failed() {
						t.Fatalf("%s: token %d still honoured", end, i)
					}
				}
				for _, row := range w.sessions(s) {
					if row.Revoked == nil {
						t.Fatalf("%s: session %s of the chain is not revoked", end, row.ID)
					}
				}
			}
		})
	}
}

// waitLockWaiters blocks until at least n backends of this database wait for a lock (the race tests sequence a
// refresh and a session end behind a held row lock without fixed sleeps).
func (w *mfaW) waitLockWaiters(n int) {
	w.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := w.owner.Raw().QueryRow(context.Background(),
			`SELECT count(*) FROM pg_locks l WHERE NOT l.granted
			   AND l.pid IN (SELECT pid FROM pg_stat_activity WHERE datname = current_database())`).Scan(&waiting); err != nil {
			w.t.Fatal(err)
		}
		if waiting >= n {
			return
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("only %d lock waiters after 10 s, want %d", waiting, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Security re-review item 1 (reproduced live against the first chain revocation): ending an OLDER link of a staff
// chain while a refresh of the head is in flight must not leave the head's successor live. A test transaction holds
// the head row's lock; the refresh of the head queues first, the session end second; on release the refresh commits a
// new head N, and the end's re-snapshot loop must still revoke N (access AND refresh token refused). Variants: delete
// the predecessor, log out with the predecessor's (old) refresh token; controls: log out / delete the head, reuse the
// old refresh token.
func TestStaffLiveness_ChainRevocationRacesRefresh(t *testing.T) {
	w := newMFAW(t)
	for _, end := range []string{"delete-predecessor", "logout-old-refresh", "logout-head", "delete-head", "reuse-old"} {
		t.Run(end, func(t *testing.T) {
			w.t = t
			s := w.staff(&w.tenant, "support")
			p0 := w.enrol(s).pair
			r := w.refresh(p0.RefreshToken)
			r.want(t, http.StatusOK, "")
			var p1 tokenPairResponse
			r.decode(t, &p1)
			s0, s1 := w.sidOf(p0.AccessToken), w.sidOf(p1.AccessToken)

			locked, release, held := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			var releaseOnce sync.Once
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) }) // never leave the lock held on a failure
			go func() {
				held <- store.WithScope(context.Background(), w.owner, s.ref, func(ctx context.Context, tx pgx.Tx) error {
					if err := db.SetSessionInternalOpID(ctx, tx, s1); err != nil {
						return err
					}
					var id uuid.UUID
					if err := tx.QueryRow(ctx, `SELECT id FROM sessions WHERE id = $1 FOR UPDATE`, s1).Scan(&id); err != nil {
						return err
					}
					close(locked)
					<-release
					return nil
				})
			}()
			<-locked
			var wg sync.WaitGroup
			var rr, er mfaResp
			wg.Add(1)
			go func() { defer wg.Done(); rr = w.refresh(p1.RefreshToken) }() // the refresh of the head, queued first
			w.waitLockWaiters(1)
			wg.Add(1)
			go func() {
				defer wg.Done()
				switch end {
				case "delete-predecessor":
					er = w.do(http.MethodDelete, "/v1/me/sessions/"+s0.String(), p1.AccessToken, nil)
				case "logout-old-refresh":
					er = w.post("/v1/auth/logout", map[string]string{"refresh_token": p0.RefreshToken})
				case "logout-head":
					er = w.post("/v1/auth/logout", map[string]string{"refresh_token": p1.RefreshToken})
				case "delete-head":
					er = w.do(http.MethodDelete, "/v1/me/sessions/"+s1.String(), p1.AccessToken, nil)
				case "reuse-old":
					er = w.refresh(p0.RefreshToken)
				}
			}()
			w.waitLockWaiters(2)
			releaseOnce.Do(func() { close(release) })
			if err := <-held; err != nil {
				t.Fatal(err)
			}
			wg.Wait()
			wantEnd := http.StatusNoContent
			if end == "reuse-old" {
				wantEnd = http.StatusUnauthorized
			}
			if er.status != wantEnd {
				t.Fatalf("session end answered %d %s, want %d", er.status, er.body, wantEnd)
			}
			if rr.status == http.StatusOK {
				var n tokenPairResponse
				rr.decode(t, &n)
				w.do(http.MethodGet, "/v1/staff/me/mfa", n.AccessToken, nil).want(t, http.StatusUnauthorized, "session_revoked")
				if again := w.refresh(n.RefreshToken); again.status == http.StatusOK {
					t.Fatalf("the new head's refresh token still works: %s", again.body)
				}
			}
			for _, tok := range []string{p0.AccessToken, p1.AccessToken} {
				w.do(http.MethodGet, "/v1/staff/me/mfa", tok, nil).want(t, http.StatusUnauthorized, "session_revoked")
			}
			for _, row := range w.sessions(s) {
				if row.Revoked == nil {
					t.Fatalf("session %s of the chain is live after the race (refresh answered %d)", row.ID, rr.status)
				}
			}
		})
	}
}

// Security re-review item 2: DELETE of one's own already-revoked head still ends the live older links (as logout
// does) and answers 204; once nothing is left to revoke it answers 404.
func TestStaffLiveness_DeleteRevokedHeadEndsOlderLinks(t *testing.T) {
	w := newMFAW(t)
	s := w.staff(nil, "platform_admin")
	p0 := w.enrol(s).pair
	r := w.refresh(p0.RefreshToken)
	r.want(t, http.StatusOK, "")
	var p1 tokenPairResponse
	r.decode(t, &p1)
	head := w.sidOf(p1.AccessToken)
	if err := w.ownerSessionExec(s, head, `UPDATE sessions SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`); err != nil {
		t.Fatal(err)
	}
	w.do(http.MethodGet, "/v1/staff/me/mfa", p0.AccessToken, nil).want(t, http.StatusOK, "") // the older link is live
	w.do(http.MethodDelete, "/v1/me/sessions/"+head.String(), p0.AccessToken, nil).want(t, http.StatusNoContent, "")
	w.do(http.MethodGet, "/v1/staff/me/mfa", p0.AccessToken, nil).want(t, http.StatusUnauthorized, "session_revoked")
	// Nothing left to revoke: 404 (a fresh live session of the same staff member makes the call).
	p2 := w.fullLogin(s)
	w.do(http.MethodDelete, "/v1/me/sessions/"+head.String(), p2.AccessToken, nil).want(t, http.StatusNotFound, "not_found")
	w.do(http.MethodGet, "/v1/staff/me/mfa", p2.AccessToken, nil).want(t, http.StatusOK, "") // an unrelated chain is untouched
}
