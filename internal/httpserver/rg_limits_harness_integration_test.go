//go:build integration

// ADR 0115 slice S2: shared harness of the RG limit write API integration tests. Every assertion about
// what the application can do runs through the real HTTP stack as the igaming_runtime role
// (non-superuser, non-BYPASSRLS, asserted) with app.environment=test on every connection; the owner
// pool only seeds fixtures and reads for assertions. Requires TEST_DATABASE_URL and
// TEST_RUNTIME_DATABASE_URL (a skip is NOT evidence). Each test builds its own jurisdiction, tenants,
// brands, players and staff, so tests share a database without sharing rows.
package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/admission"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/auth/authtest"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/rg"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/rgfix"
)

type rgW struct {
	t      *testing.T
	owner  *db.Pool
	rt     *db.Pool
	ops    launchfix.OperatorIDs
	issuer *auth.Issuer
	srv    *httptest.Server
	// samePersonPlatform is a second platform staff account of the floor requester's Person.
	samePersonPlatform uuid.UUID
}

type rgWOpt struct {
	limits  *rg.Limits
	limiter *admission.GCRALimiter
}

func newRGW(t *testing.T, opt rgWOpt) *rgW {
	t.Helper()
	ou, ru := os.Getenv("TEST_DATABASE_URL"), os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if ou == "" || ru == "" {
		t.Skip("TEST_DATABASE_URL and TEST_RUNTIME_DATABASE_URL are required (a skip is not evidence)")
	}
	ctx := context.Background()
	owner, err := db.Connect(ctx, ou, 10, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	rt, err := db.ConnectWithRuntimeParams(ctx, ru, 40, 5*time.Second, map[string]string{"app.environment": "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Close)
	var who string
	var super, bypass bool
	if err := rt.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT current_user::text, rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&who, &super, &bypass)
	}); err != nil {
		t.Fatal(err)
	}
	if who != "igaming_runtime" || super || bypass {
		t.Fatalf("the HTTP server must run as igaming_runtime, non-superuser, non-BYPASSRLS; got %q super=%v bypass=%v", who, super, bypass)
	}
	rgfix.EnsureTestEnvironment(t, ou)
	prooftest.Install(t, ou)
	launchfix.SeedOperators(t)
	keys, err := auth.NewKeyRegistry("k1", map[string]string{"k1": strings.Repeat("k", 32)})
	if err != nil {
		t.Fatal(err)
	}
	w := &rgW{t: t, owner: owner, rt: rt, ops: launchfix.Operators(), issuer: auth.NewIssuer(keys, "rg-test", "rg-test"), samePersonPlatform: uuid.New()}
	limiter := opt.limiter
	if limiter == nil {
		limiter = admission.NewGCRALimiter(1000, 1000, 1000, time.Hour, "overflow", admission.RealClock())
	}
	w.srv = httptest.NewServer(New(Deps{
		Logger:          slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		DB:              rt,
		AuthIssuer:      w.issuer,
		StaffMFA:        testStaffMFA(), // ADR 0119 S3: staff login is two-step (shared test keys)
		ServiceName:     "rg-test",
		AccessTokenTTL:  time.Hour,
		RefreshTokenTTL: time.Hour,
		PersonResolver:  identityresolution.NewMockPersonResolver(),
		RGLimits:        opt.limits,
		RGWriteLimiter:  limiter,
	}))
	t.Cleanup(w.srv.Close)
	// A second platform staff account of the requester's Person (four-eyes is by Person, not staff id).
	if err := owner.WithPlatformAdmin(ctx, w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id)
			VALUES ($1, NULL, $2, '!x', 'platform_admin', $3)`, w.samePersonPlatform, "rgs2-same-"+w.samePersonPlatform.String()+"@fixture.invalid", w.ops.Persons[0])
		return err
	}); err != nil {
		t.Fatalf("seed same-person platform staff: %v", err)
	}
	return w
}

// ---- HTTP -------------------------------------------------------------------------------------------

type rgResp struct {
	status int
	body   []byte
	header http.Header
}

func (w *rgW) call(method, path, token, key string, body any) rgResp {
	w.t.Helper()
	var rdr io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rdr = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			w.t.Fatal(err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, w.srv.URL+path, rdr)
	if err != nil {
		w.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		w.t.Fatal(err)
	}
	return rgResp{status: resp.StatusCode, body: b, header: resp.Header}
}

func (r rgResp) code(t *testing.T) string {
	t.Helper()
	var e struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(r.body, &e); err != nil {
		t.Fatalf("decode error body %q: %v", r.body, err)
	}
	return e.Code
}

func (r rgResp) obj(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.body, &m); err != nil {
		t.Fatalf("decode %q: %v", r.body, err)
	}
	return m
}

func (r rgResp) want(t *testing.T, status int, code string) {
	t.Helper()
	if r.status != status || (code != "" && r.code(t) != code) {
		t.Fatalf("want %d %s, got %d %s", status, code, r.status, r.body)
	}
}

func (w *rgW) tok(subject, tenantID uuid.UUID, role auth.Role, pt auth.PrincipalType) string {
	w.t.Helper()
	return authtest.Token(w.t, w.issuer, subject, tenantID, role, pt) // ADR 0119 S3: staff tokens carry aal2 assurance
}

// ---- fixture ------------------------------------------------------------------------------------------

// rgFix is one isolated jurisdiction with two tenants licensed in it.
type rgFix struct {
	jur                      string
	tenantA, tenantB         uuid.UUID
	brandA1, brandA2, brandB uuid.UUID
	personA, personX         uuid.UUID
	acctA1, acctA2           uuid.UUID // personA at brand A1 / A2 (tenant A)
	acctX                    uuid.UUID // personX at brand A1
	acctB                    uuid.UUID // personA at tenant B
	compA, adminA            uuid.UUID // tenant A compliance / tenant_admin staff
	compSelf                 uuid.UUID // tenant A compliance staff of personA (self-target)
	compB                    uuid.UUID // tenant B compliance staff
	supportA                 uuid.UUID // tenant A support staff
}

func (w *rgW) fixture() rgFix {
	w.t.Helper()
	ctx := context.Background()
	f := rgFix{jur: "RGS2-" + strings.ToUpper(uuid.NewString()[:8]), personA: uuid.New(), personX: uuid.New(),
		acctA1: uuid.New(), acctA2: uuid.New(), acctX: uuid.New(), acctB: uuid.New(),
		compA: uuid.New(), adminA: uuid.New(), compSelf: uuid.New(), compB: uuid.New(), supportA: uuid.New()}
	persons := map[string]uuid.UUID{"compA": uuid.New(), "adminA": uuid.New(), "compB": uuid.New(), "supportA": uuid.New()}
	if err := w.owner.WithPlatformAdmin(ctx, w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		var jid, lic uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO jurisdictions (code, name) VALUES ($1, 'rgs2 fixture') RETURNING id`, f.jur).Scan(&jid); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO licences (jurisdiction_id, licensee, licence_number) VALUES ($1, 'platform', $2) RETURNING id`, jid, "RGS2-"+f.jur).Scan(&lic); err != nil {
			return err
		}
		all := []uuid.UUID{f.personA, f.personX}
		for _, p := range persons {
			all = append(all, p)
		}
		for _, p := range all {
			if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, p); err != nil {
				return err
			}
		}
		for i, tp := range []*uuid.UUID{&f.tenantA, &f.tenantB} {
			tn, err := launchfix.CreateTenant(ctx, tx, "rgs2", "rgs2-"+uuid.NewString()[:12], "under_platform_licence")
			if err != nil {
				return err
			}
			_ = i
			*tp = tn.ID
			if _, err := tx.Exec(ctx, `UPDATE tenants SET licence_id = $1 WHERE id = $2`, lic, tn.ID); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		w.t.Fatalf("seed jurisdiction / tenants: %v", err)
	}
	for _, s := range []struct {
		id, tenant, person uuid.UUID
		role               string
	}{
		{f.compA, f.tenantA, persons["compA"], "compliance"},
		{f.adminA, f.tenantA, persons["adminA"], "tenant_admin"},
		{f.supportA, f.tenantA, persons["supportA"], "support"},
		{f.compSelf, f.tenantA, f.personA, "compliance"},
		{f.compB, f.tenantB, persons["compB"], "compliance"},
	} {
		if err := w.owner.WithTenant(ctx, s.tenant, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id)
				VALUES ($1, $2, $3, '!x', $4, $5)`, s.id, s.tenant, "rgs2-"+s.id.String()+"@fixture.invalid", s.role, s.person)
			return err
		}); err != nil {
			w.t.Fatalf("seed tenant staff: %v", err)
		}
	}
	for _, b := range []struct {
		tenant uuid.UUID
		brand  *uuid.UUID
		accts  [][2]uuid.UUID
	}{
		{f.tenantA, &f.brandA1, [][2]uuid.UUID{{f.acctA1, f.personA}, {f.acctX, f.personX}}},
		{f.tenantA, &f.brandA2, [][2]uuid.UUID{{f.acctA2, f.personA}}},
		{f.tenantB, &f.brandB, [][2]uuid.UUID{{f.acctB, f.personA}}},
	} {
		if err := w.owner.WithTenant(ctx, b.tenant, func(ctx context.Context, tx pgx.Tx) error {
			br, err := launchfix.CreateBrand(ctx, tx, b.tenant, "rgs2", "rgs2-b-"+uuid.NewString()[:12])
			if err != nil {
				return err
			}
			*b.brand = br.ID
			for _, a := range b.accts {
				if _, err := tx.Exec(ctx, `INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
					VALUES ($1, $2, $3, $4, $5, 'x', 'active')`, a[0], b.tenant, br.ID, a[1], a[0].String()+"@rgs2.invalid"); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			w.t.Fatalf("seed brands / accounts: %v", err)
		}
	}
	return f
}

func (w *rgW) player(f rgFix, acct uuid.UUID) string {
	tenant := f.tenantA
	if acct == f.acctB {
		tenant = f.tenantB
	}
	return w.tok(acct, tenant, auth.RolePlayer, auth.PrincipalPlayer)
}

func (w *rgW) staff(tenant, id uuid.UUID, role auth.Role) string {
	return w.tok(id, tenant, role, auth.PrincipalStaff)
}

func (w *rgW) platform(id uuid.UUID) string {
	return w.tok(id, uuid.Nil, auth.RolePlatformAdmin, auth.PrincipalStaff)
}

// ---- configuration fixtures ----------------------------------------------------------------------------------

// strictFloor is a SYNTHETIC-NOT-LEGAL fixture floor for the test (enabled, a 5000 EUR ceiling for
// amount kinds, a 2-second cooldown so tests can wait it out). Not a legal value.
func strictFloor(jur, kind string, over rgfix.Spec) rgfix.Spec {
	s := rgfix.PermissiveFloor(jur, kind).Clone(rgfix.Spec{"enabled": true})
	switch kind {
	case "deposit", "loss", "wager":
		s = s.Clone(rgfix.Spec{"asset_code": "EUR", "ceiling_none": false, "ceiling": "5000", "increase_cooldown": "00:00:02",
			"allowed_periods": []string{"rolling_24h", "calendar_day"}})
	case "session_duration":
		s = s.Clone(rgfix.Spec{"ceiling_none": false, "ceiling": "36000", "increase_cooldown": "00:00:02"})
	}
	return s.Clone(over)
}

func (w *rgW) floor(jur, kind string, over rgfix.Spec) uuid.UUID {
	w.t.Helper()
	var id uuid.UUID
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		if err := rgfix.SetEnvironmentGUC(ctx, tx, "test"); err != nil {
			return err
		}
		var err error
		id, err = rgfix.InsertFloor(ctx, tx, strictFloor(jur, kind, over), rgfix.DefaultSteps())
		return err
	}); err != nil {
		w.t.Fatalf("seed floor %s/%s: %v", jur, kind, err)
	}
	return id
}

// ---- assertions on the owner side ------------------------------------------------------------------------

func (w *rgW) ownerTenant(tenant uuid.UUID, fn func(ctx context.Context, tx pgx.Tx) error) {
	w.t.Helper()
	if err := w.owner.WithTenant(context.Background(), tenant, fn); err != nil {
		w.t.Fatal(err)
	}
}

func (w *rgW) changeCount(tenant, acct uuid.UUID) int {
	w.t.Helper()
	var n int
	w.ownerTenant(tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM player_rg_limit_changes WHERE tenant_id = $1 AND player_account_id = $2`, tenant, acct).Scan(&n)
	})
	return n
}

func (w *rgW) auditRows(tenant uuid.UUID, action string) []map[string]any {
	w.t.Helper()
	var out []map[string]any
	read := func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT metadata, outcome, COALESCE(host(ip_address), ''), COALESCE(user_agent, ''), COALESCE(request_id, ''), COALESCE(target_id, ''), COALESCE(actor_id::text, '')
			FROM audit_log WHERE action = $1 AND ($2::uuid IS NULL OR tenant_id = $2) ORDER BY created_at`, action, nilUUID(tenant))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var md map[string]any
			var outcome, ip, ua, rid, target, actor string
			if err := rows.Scan(&md, &outcome, &ip, &ua, &rid, &target, &actor); err != nil {
				return err
			}
			md["_outcome"], md["_ip"], md["_ua"], md["_request_id"], md["_target"], md["_actor"] = outcome, ip, ua, rid, target, actor
			out = append(out, md)
		}
		return rows.Err()
	}
	if tenant == uuid.Nil {
		if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, read); err != nil {
			w.t.Fatal(err)
		}
	} else {
		w.ownerTenant(tenant, read)
	}
	return out
}

func nilUUID(u uuid.UUID) any {
	if u == uuid.Nil {
		return nil
	}
	return u
}

// sleepPast waits until the database clock passes t (plus a margin).
func sleepPast(t time.Time) {
	if d := time.Until(t) + 300*time.Millisecond; d > 0 {
		time.Sleep(d)
	}
}

func parseTime(s any) time.Time {
	tm, err := time.Parse(time.RFC3339Nano, s.(string))
	if err != nil {
		panic(err)
	}
	return tm
}

type rgBody = map[string]any

type (
	ctxT       = context.Context
	txT        = pgx.Tx
	auditEntry = audit.Entry
)

func newFakeClockForRG() *admission.FakeClock {
	return admission.NewFakeClock(time.Unix(1_800_000_000, 0))
}

// newRGLimiterForTest is a per-player limiter with a frozen clock (no refill unless the test advances it).
func newRGLimiterForTest(clock admission.Clock, rate float64, burst int) *admission.GCRALimiter {
	return admission.NewGCRALimiter(rate, burst, 1000, time.Hour, "overflow", clock)
}

func ctxBackground() context.Context { return context.Background() }
