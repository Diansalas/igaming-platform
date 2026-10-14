//go:build integration

package stafflifecycle

// ADR 0119 S5 + S6 database tests (section 17.5 / 17.6, as igaming_runtime where the rule is the runtime role's):
// the SF010-SF014 trigger matrix, SF020 / SF021 last-admin protection (single and concurrent, with the advisory lock),
// the two-phase suspension (busy -> sessions already revoked), the grants, the catalog pins and the pinned R1 residual.

import (
	"context"
	"errors"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/auth/authtest"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/email"
	"github.com/Diansalas/igaming-platform/internal/staffmfa"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

var (
	rtOnce sync.Once
	rtPool *db.Pool
	rtErr  error
)

func runtimePool(t testing.TB) *db.Pool {
	t.Helper()
	u := os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if u == "" {
		t.Skip("TEST_RUNTIME_DATABASE_URL not set")
	}
	rtOnce.Do(func() { rtPool, rtErr = db.Connect(context.Background(), u, 12, 10*time.Second) })
	if rtErr != nil {
		t.Fatalf("connect runtime pool: %v", rtErr)
	}
	prooftest.Install(t, os.Getenv("TEST_DATABASE_URL"))
	return rtPool
}

func testKeys(t testing.TB) *staffmfa.Keys {
	t.Helper()
	f := make([]byte, 32)
	m := make([]byte, 32)
	for i := range f {
		f[i], m[i] = byte(i+1), byte(200-i)
	}
	k, err := staffmfa.NewKeys("f1", map[string][]byte{"f1": f}, "m1", map[string][]byte{"m1": m})
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func code(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	return ""
}

type lworld struct {
	t      *testing.T
	owner  *db.Pool
	rt     *db.Pool
	tenant uuid.UUID
	svc    *Service
	mail   *email.MockProvider
}

func newLWorld(t *testing.T) *lworld {
	t.Helper()
	w := &lworld{t: t, rt: runtimePool(t), owner: launchfix.OwnerPool(t), mail: email.NewMockProvider()}
	launchfix.SeedOperators(t)
	if err := w.owner.WithPlatformAdmin(context.Background(), launchfix.Operators().Requester, func(ctx context.Context, tx pgx.Tx) error {
		tn, err := launchfix.CreateTenant(ctx, tx, "sl", "sl-"+uuid.NewString()[:12], "under_platform_licence")
		w.tenant = tn.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	svc, err := New(w.rt, testKeys(t), w.mail, "")
	if err != nil {
		t.Fatal(err)
	}
	w.svc = svc
	return w
}

// staff inserts an active staff member with a Person (and a usable placeholder factor when enrolled).
func (w *lworld) staff(tenant uuid.UUID, role string, enrolled bool) uuid.UUID {
	w.t.Helper()
	id := uuid.New()
	ins := func(ctx context.Context, tx pgx.Tx) error {
		var tid any
		if tenant != uuid.Nil {
			tid = tenant
		}
		if _, err := tx.Exec(ctx, `WITH p AS (INSERT INTO persons (id) VALUES ($1))
			INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id) VALUES ($1, $2, $3, 'x', $4, $1)`,
			id, tid, "sl-"+id.String()+"@fixture.invalid", role); err != nil {
			return err
		}
		if enrolled {
			return authtest.EnsurePlaceholderFactor(ctx, tx, id)
		}
		return nil
	}
	if err := w.scope(w.owner, tenant, ins); err != nil {
		w.t.Fatalf("seed staff %s: %v", role, err)
	}
	return id
}

func (w *lworld) scope(pool *db.Pool, tenant uuid.UUID, fn db.TxFunc) error {
	if tenant != uuid.Nil {
		return pool.WithTenant(context.Background(), tenant, fn)
	}
	return pool.WithoutTenant(context.Background(), fn)
}

func (w *lworld) exec(pool *db.Pool, tenant uuid.UUID, q string, args ...any) error {
	return w.scope(pool, tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, q, args...)
		return err
	})
}

func (w *lworld) actor(id, tenant uuid.UUID, role string) Actor {
	var sv int64
	_ = w.scope(w.owner, tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT staff_version FROM staff_users WHERE id = $1`, id).Scan(&sv)
	})
	return Actor{StaffID: id, TenantID: tenant, Role: role, StaffVersion: sv, Meta: Meta{RequestID: "t-" + uuid.NewString()}}
}

// SF010-SF014 as the runtime role (direct SQL: the database is the backstop of every Go check).
func TestTriggers_SF010_SF014_Matrix(t *testing.T) {
	w := newLWorld(t)
	id := w.staff(w.tenant, RoleSupport, false)
	cases := []struct {
		name, q, want string
	}{
		{"insert suspended", `INSERT INTO staff_users (tenant_id, email, password_hash, role, status) VALUES ($1, 'x-` + uuid.NewString() + `@f.invalid', 'x', 'support', 'suspended')`, "SF010"},
		{"insert offboarded", `INSERT INTO staff_users (tenant_id, email, password_hash, role, status) VALUES ($1, 'x-` + uuid.NewString() + `@f.invalid', NULL, 'support', 'offboarded')`, "SF010"},
		{"insert invited with a password", `INSERT INTO staff_users (tenant_id, email, password_hash, role, status) VALUES ($1, 'x-` + uuid.NewString() + `@f.invalid', 'x', 'support', 'invited')`, "SF010"},
		{"insert staff_version 5", `INSERT INTO staff_users (tenant_id, email, password_hash, role, staff_version) VALUES ($1, 'x-` + uuid.NewString() + `@f.invalid', 'x', 'support', 5)`, "SF012"},
		{"insert approve-capable without Person", `INSERT INTO staff_users (tenant_id, email, password_hash, role) VALUES ($1, 'x-` + uuid.NewString() + `@f.invalid', 'x', 'finance')`, "SF014"},
		{"insert active without password", `INSERT INTO staff_users (tenant_id, email, password_hash, role) VALUES ($1, 'x-` + uuid.NewString() + `@f.invalid', NULL, 'support')`, "23514"},
	}
	for _, c := range cases {
		if got := code(w.exec(w.owner, w.tenant, c.q, w.tenant)); got != c.want {
			t.Errorf("%s: got %q, want %s", c.name, got, c.want)
		}
	}
	upd := []struct {
		name, q, want string
	}{
		{"active -> invited", `UPDATE staff_users SET status = 'invited', password_hash = NULL WHERE id = $1`, "SF010"},
		{"staff_version edited", `UPDATE staff_users SET staff_version = staff_version + 1 WHERE id = $1`, "42501"},
		{"tenant_id edited", `UPDATE staff_users SET tenant_id = NULL WHERE id = $1`, "42501"},
		{"email edited", `UPDATE staff_users SET email = 'y@f.invalid' WHERE id = $1`, "42501"},
		{"role into finance without Person is fine here (support has a Person)", `UPDATE staff_users SET role = 'finance' WHERE id = $1`, ""},
	}
	for _, c := range upd {
		if got := code(w.exec(w.rt, w.tenant, c.q, id)); got != c.want {
			t.Errorf("runtime %s: got %q, want %q", c.name, got, c.want)
		}
	}
	// As the owner (no column grant gates it) the trigger is the refusal.
	for _, c := range []struct{ name, q, want string }{
		{"owner staff_version edited", `UPDATE staff_users SET staff_version = 99 WHERE id = $1`, "SF012"},
		{"owner email edited", `UPDATE staff_users SET email = 'y-` + uuid.NewString() + `@f.invalid' WHERE id = $1`, "SF011"},
		{"owner status_changed_at forged", `UPDATE staff_users SET status_changed_at = now() WHERE id = $1`, "SF010"},
	} {
		if got := code(w.exec(w.owner, w.tenant, c.q, id)); got != c.want {
			t.Errorf("%s: got %q, want %s", c.name, got, c.want)
		}
	}
	// staff_version moves by exactly one per change of role / status / password / person.
	var sv0, sv1 int64
	_ = w.scope(w.owner, w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT staff_version FROM staff_users WHERE id = $1`, id).Scan(&sv0)
	})
	if err := w.exec(w.rt, w.tenant, `UPDATE staff_users SET status = 'suspended' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if err := w.exec(w.rt, w.tenant, `UPDATE staff_users SET display_name = 'Name' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	_ = w.scope(w.owner, w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT staff_version FROM staff_users WHERE id = $1`, id).Scan(&sv1)
	})
	if sv1 != sv0+1 {
		t.Fatalf("staff_version %d -> %d, want +1 (status) and +0 (display name)", sv0, sv1)
	}
	// Offboarded is terminal.
	if err := w.exec(w.rt, w.tenant, `UPDATE staff_users SET status = 'offboarded', password_hash = NULL WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`UPDATE staff_users SET status = 'active', password_hash = 'x' WHERE id = $1`, `UPDATE staff_users SET status = 'suspended' WHERE id = $1`} {
		if got := code(w.exec(w.rt, w.tenant, q, id)); got != "SF010" {
			t.Errorf("leaving offboarded (%s): %q", q, got)
		}
	}
}

// SF020 / SF021 single-actor cases: the platform last admin (counting only admins with a usable factor), and the
// tenant last tenant_admin removed by a tenant-scoped actor (refused) or a platform actor (allowed).
func TestLastAdmin_TenantScopedRefused_PlatformAllowed(t *testing.T) {
	w := newLWorld(t)
	ta := w.staff(w.tenant, RoleTenantAdmin, true)
	ta2 := w.staff(w.tenant, RoleTenantAdmin, false) // no usable factor: does not count
	// Tenant-scoped (no platform GUC): ta2 suspending ta (the only enrolled tenant admin) -> SF021.
	err := w.rt.WithPrincipalScope(context.Background(), w.tenant, ta2, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE staff_users SET status = 'suspended' WHERE id = $1`, ta)
		return err
	})
	if code(err) != "SF021" {
		t.Fatalf("tenant-scoped removal of the last tenant admin: %v", err)
	}
	// A platform actor (app.actor_staff_id names a platform row, visible through staff_actor_self_select) may.
	pa := w.staff(uuid.Nil, RolePlatformAdmin, true)
	err = w.rt.WithTenant(context.Background(), w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		if err := db.SetActorStaffIDForCurrentTx(ctx, tx, pa); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE staff_users SET status = 'suspended' WHERE id = $1`, ta)
		return err
	})
	if err != nil {
		t.Fatalf("platform removal of the last tenant admin must be allowed: %v", err)
	}
}

// The s9.5(b) pair: app.actor_staff_id makes the actor's own platform row visible (and FOR SHARE lockable) inside a
// tenant transaction, but never updatable (WITH CHECK false).
func TestActorSelfPolicyPair_VisibleLockableNeverUpdatable(t *testing.T) {
	w := newLWorld(t)
	pa := w.staff(uuid.Nil, RolePlatformAdmin, true)
	var seen int
	err := w.rt.WithTenant(context.Background(), w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM staff_users WHERE id = $1`, pa).Scan(&seen); err != nil {
			return err
		}
		if seen != 0 {
			return errors.New("visible without the GUC")
		}
		if err := db.SetActorStaffIDForCurrentTx(ctx, tx, pa); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM (SELECT id FROM staff_users WHERE id = $1 FOR SHARE) s`, pa).Scan(&seen); err != nil {
			return err
		}
		return nil
	})
	if err != nil || seen != 1 {
		t.Fatalf("visibility through the GUC: %v seen=%d", err, seen)
	}
	err = w.rt.WithTenant(context.Background(), w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		if err := db.SetActorStaffIDForCurrentTx(ctx, tx, pa); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE staff_users SET display_name = 'pwned' WHERE id = $1`, pa)
		return err
	})
	if code(err) != "42501" {
		t.Fatalf("an UPDATE through staff_actor_self_lock must be refused (WITH CHECK false): %v", err)
	}
}

// neutralisePlatformAdmins revokes the factor of every OTHER usable platform admin (TEST DATABASE ONLY - the
// private harness database): last-admin protection counts the whole platform, so a hermetic last-admin test needs
// the rest of the platform to be unusable. Revocation is permanent (SF002); later fixtures re-plant placeholder
// factors (authtest.EnsurePlaceholderFactor) for the admins they use.
func neutralisePlatformAdmins(t *testing.T, owner *db.Pool, keep ...uuid.UUID) {
	t.Helper()
	ids := make([]string, len(keep))
	for i, k := range keep {
		ids[i] = k.String()
	}
	if err := owner.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE staff_mfa_factors SET status = 'revoked', revoked_at = now(), revoked_reason = 'reset', pending_expires_at = NULL
			WHERE tenant_id IS NULL AND status IN ('active', 'pending') AND NOT (staff_user_id::text = ANY ($1))`, ids)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// SF020: two platform admins remove each other at the same moment: exactly one succeeds, the other is SF020 (the
// advisory lock serialises them and the second re-counts after the first commits). Then the survivor cannot be removed.
func TestLastAdmin_Platform_ConcurrentMutualSuspension(t *testing.T) {
	w := newLWorld(t)
	a := w.staff(uuid.Nil, RolePlatformAdmin, true)
	b := w.staff(uuid.Nil, RolePlatformAdmin, true)
	neutralisePlatformAdmins(t, w.owner, a, b)

	start := make(chan struct{})
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, pair := range [][2]uuid.UUID{{a, b}, {b, a}} {
		wg.Add(1)
		go func(i int, actor, target uuid.UUID) {
			defer wg.Done()
			<-start
			errs[i] = w.rt.WithPlatformAdmin(context.Background(), actor, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE staff_users SET status = 'suspended' WHERE id = $1`, target)
				if err == nil {
					time.Sleep(200 * time.Millisecond) // hold the advisory lock across the other's attempt
				}
				return err
			})
		}(i, pair[0], pair[1])
	}
	close(start)
	wg.Wait()
	var ok, refused int
	for _, err := range errs {
		switch code(err) {
		case "":
			if err == nil {
				ok++
			}
		case "SF020":
			refused++
		default:
			t.Fatalf("unexpected: %v", err)
		}
	}
	if ok != 1 || refused != 1 {
		t.Fatalf("mutual suspension: %d succeeded, %d refused (want exactly 1 and 1): %v", ok, refused, errs)
	}
	// The survivor is now the last admin: removing it is refused, by suspension or by offboarding.
	survivor := a
	if errs[0] == nil {
		survivor = a
	} else {
		survivor = b
	}
	other := w.staff(uuid.Nil, RolePlatformAdmin, false) // not enrolled: does not count, and acts
	for _, q := range []string{`UPDATE staff_users SET status = 'suspended' WHERE id = $1`, `UPDATE staff_users SET status = 'offboarded', password_hash = NULL WHERE id = $1`} {
		err := w.rt.WithPlatformAdmin(context.Background(), other, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, q, survivor)
			return err
		})
		if code(err) != "SF020" {
			t.Fatalf("removing the last enrolled platform admin (%s): %v", q, err)
		}
	}
	// Through the service: 409 last_admin (ErrLastAdmin), and the T1 pre-check leaves its sessions alive.
	_, err := w.svc.Suspend(context.Background(), w.actor(other, uuid.Nil, RolePlatformAdmin), uuid.Nil, survivor, Reason{Code: "investigation"})
	if !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("service suspension of the last admin: %v", err)
	}
	// Re-plant a usable factor for the survivor's peer so later tests keep a usable platform.
	_ = w.scope(w.owner, uuid.Nil, func(ctx context.Context, tx pgx.Tx) error { return authtest.EnsurePlaceholderFactor(ctx, tx, other) })
}

// The two-phase suspension: when T2 cannot take the target's row lock (5 s here shortened), it retries once and
// answers ErrBusy - and the target's sessions are ALREADY revoked (T1 committed): fail-safe.
func TestTwoPhaseSuspension_BusyLeavesSessionsRevoked(t *testing.T) {
	w := newLWorld(t)
	w.svc.lockTimeout = 300 * time.Millisecond
	ta := w.staff(w.tenant, RoleTenantAdmin, true)
	target := w.staff(w.tenant, RoleSupport, true)
	sess := authtest.LiveStaffSession(t, w.owner, testKeys(t), target, w.tenant)
	_ = sess
	// Hold the target's row lock in another transaction for the whole call.
	hold, err := w.owner.Raw().Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Rollback(context.Background()) }()
	if _, err := hold.Exec(context.Background(), `SELECT set_config('app.tenant_id', $1, true)`, w.tenant.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := hold.Exec(context.Background(), `SELECT 1 FROM staff_users WHERE id = $1 FOR UPDATE`, target); err != nil {
		t.Fatal(err)
	}
	_, err = w.svc.Suspend(context.Background(), w.actor(ta, w.tenant, RoleTenantAdmin), w.tenant, target, Reason{Code: "investigation"})
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("a held row lock must answer ErrBusy, got %v", err)
	}
	var live int
	var status string
	_ = w.scope(w.owner, w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE principal_id = $1 AND revoked_at IS NULL`, target).Scan(&live); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT status FROM staff_users WHERE id = $1`, target).Scan(&status)
	})
	if live != 0 || status != "active" {
		t.Fatalf("busy: live sessions %d (want 0, T1 revoked them), status %s (want active, T2 never committed)", live, status)
	}
	_ = hold.Rollback(context.Background())
	if _, err := w.svc.Suspend(context.Background(), w.actor(ta, w.tenant, RoleTenantAdmin), w.tenant, target, Reason{Code: "investigation"}); err != nil {
		t.Fatalf("the retry after the lock is released: %v", err)
	}
}

// The suspension race (section 17.5) for the lifecycle itself: an actor suspended (or role-changed) after its request
// passed liveness is refused by the in-transaction re-assertion (db.ErrActorStaffNotLive) and nothing is written; a
// stale `sv` alone is enough.
func TestLifecycleReassertion_ActorChangedIsRefused(t *testing.T) {
	w := newLWorld(t)
	pa := w.staff(uuid.Nil, RolePlatformAdmin, true)
	target := w.staff(w.tenant, RoleSupport, true)
	stale := w.actor(pa, uuid.Nil, RolePlatformAdmin)
	// A Person link bumps the actor's staff_version (SF012): the token's sv is now stale.
	if err := w.exec(w.owner, uuid.Nil, `UPDATE staff_users SET display_name = 'x' WHERE id = $1`, pa); err != nil {
		t.Fatal(err)
	}
	other := w.staff(uuid.Nil, RolePlatformAdmin, true)
	if err := w.rt.WithPlatformAdmin(context.Background(), other, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE staff_users SET status = 'suspended' WHERE id = $1`, pa)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for name, fn := range map[string]func() error{
		"role change": func() error {
			_, err := w.svc.ChangeRole(context.Background(), stale, w.tenant, target, RoleCompliance, Reason{Code: "role_change_approved"})
			return err
		},
		"reset": func() error {
			_, err := w.svc.ResetMFA(context.Background(), stale, w.tenant, target, Reason{Code: "device_lost"})
			return err
		},
	} {
		if err := fn(); !errors.Is(err, db.ErrActorStaffNotLive) {
			t.Fatalf("%s by a suspended actor: %v", name, err)
		}
	}
	var role string
	_ = w.scope(w.owner, w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT role FROM staff_users WHERE id = $1`, target).Scan(&role)
	})
	if role != RoleSupport {
		t.Fatal("nothing may be written by a refused actor")
	}
}

// Grants and catalog pins (section 13.4 / 13.5).
func TestGrantsAndCatalog_Pins(t *testing.T) {
	w := newLWorld(t)
	q := func(sql string, args ...any) []string {
		var out []string
		err := w.owner.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			rows, err := tx.Query(ctx, sql, args...)
			if err != nil {
				return err
			}
			out, err = pgx.CollectRows(rows, pgx.RowTo[string])
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(out)
		return out
	}
	eq := func(name string, got, want []string) {
		t.Helper()
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: got %v, want %v", name, got, want)
		}
	}
	eq("staff_users runtime UPDATE columns", q(`SELECT column_name::text FROM information_schema.column_privileges
		WHERE table_name = 'staff_users' AND grantee = 'igaming_runtime' AND privilege_type = 'UPDATE'`),
		[]string{"display_name", "offboarded_at", "password_hash", "person_id", "role", "status", "status_changed_at", "updated_at"})
	eq("lifecycle table privileges", q(`SELECT table_name::text || ':' || privilege_type::text FROM information_schema.table_privileges
		WHERE grantee = 'igaming_runtime' AND table_name LIKE 'staff\_lifecycle\_%'`),
		[]string{"staff_lifecycle_approvals:INSERT", "staff_lifecycle_approvals:SELECT", "staff_lifecycle_events:INSERT", "staff_lifecycle_events:SELECT",
			"staff_lifecycle_requests:INSERT", "staff_lifecycle_requests:SELECT"})
	eq("zz_actor_proof_guard is the last BEFORE row trigger", q(`SELECT c.relname::text || ':' || (SELECT max(t2.tgname::text) FROM pg_trigger t2
			WHERE t2.tgrelid = c.oid AND NOT t2.tgisinternal AND (t2.tgtype & 2) = 2 AND (t2.tgtype & 1) = 1)
		FROM pg_class c WHERE c.relname IN ('staff_lifecycle_requests', 'staff_lifecycle_approvals')`),
		[]string{"staff_lifecycle_approvals:zz_actor_proof_guard", "staff_lifecycle_requests:zz_actor_proof_guard"})
	eq("sf_last_admin is VOLATILE", q(`SELECT provolatile::text FROM pg_proc WHERE proname = 'sf_last_admin'`), []string{"v"})
	eq("no SECURITY DEFINER among the 0139 functions", q(`SELECT proname::text FROM pg_proc WHERE pronamespace = 'public'::regnamespace AND prosecdef
		AND (proname LIKE 'sf\_%' OR proname LIKE '%staff\_lifecycle%')`), nil)
	eq("the s9.5(b) policy pair", q(`SELECT policyname::text || ':' || cmd || ':' || coalesce(with_check, '-') FROM pg_policies
		WHERE tablename = 'staff_users' AND policyname LIKE 'staff\_actor\_self\_%'`),
		[]string{"staff_actor_self_lock:UPDATE:false", "staff_actor_self_select:SELECT:-"})
	eq("FORCE RLS on the lifecycle tables", q(`SELECT relname::text FROM pg_class WHERE relname LIKE 'staff\_lifecycle\_%' AND relkind = 'r' AND relforcerowsecurity`),
		[]string{"staff_lifecycle_approvals", "staff_lifecycle_events", "staff_lifecycle_requests"})
	eq("staff_users guard trigger order", q(`SELECT tgname::text FROM pg_trigger WHERE tgrelid = 'staff_users'::regclass AND NOT tgisinternal AND tgname LIKE 'sf\_%'`),
		[]string{"sf_10_staff_transition", "sf_11_staff_immutable", "sf_12_staff_version", "sf_13_staff_self_target", "sf_14_staff_person_required", "sf_20_last_admin"})
	eq("no SF030 / SF007 (S-H1)", q(`SELECT proname::text FROM pg_proc WHERE pronamespace = 'public'::regnamespace AND (prosrc LIKE '%SF030%' OR prosrc LIKE '%SF007%')`), nil)
	eq("restrictive fences on the events table", q(`SELECT policyname::text FROM pg_policies WHERE tablename = 'staff_lifecycle_events' AND permissive = 'RESTRICTIVE'`),
		[]string{"acting_fence_delete", "acting_fence_insert", "acting_fence_select", "acting_fence_update", "kyc_worker_fence_delete", "kyc_worker_fence_insert",
			"kyc_worker_fence_select", "kyc_worker_fence_update", "platform_service_fence", "player_scope_fence"})
	// Events are append-only for every role.
	id := w.staff(w.tenant, RoleSupport, false)
	if err := w.exec(w.owner, w.tenant, `INSERT INTO staff_lifecycle_events (staff_user_id, event, actor_type) VALUES ($1, 'suspended', 'system')`, id); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{`UPDATE staff_lifecycle_events SET event = 'reactivated' WHERE staff_user_id = $1`, `DELETE FROM staff_lifecycle_events WHERE staff_user_id = $1`} {
		if got := code(w.exec(w.owner, w.tenant, sql, id)); got != "SF099" {
			t.Errorf("owner %s: %q", sql, got)
		}
		if got := code(w.exec(w.rt, w.tenant, sql, id)); got != "42501" {
			t.Errorf("runtime %s: %q", sql, got)
		}
	}
}

// Residual R1 (section 17.6, owner-deferred S-H1): under arbitrary SQL as the runtime role, a staff member's own
// tenant row can still move from support to finance (with a Person), and a platform row can be un-suspended. ACCEPTED
// until S-H1 (OQ-2); this test makes the gap visible and turns into an SF030 / authz_seal refusal test when built.
func TestResidual_R1_RuntimeSQLRoleEscalationAccepted(t *testing.T) {
	w := newLWorld(t)
	id := w.staff(w.tenant, RoleSupport, false)
	if err := w.exec(w.rt, w.tenant, `UPDATE staff_users SET role = 'finance' WHERE id = $1`, id); err != nil {
		t.Fatalf("R1 tenant half (accepted until S-H1): %v", err)
	}
}

func TestResidual_R1_Platform_UnsuspendBySQLAccepted(t *testing.T) {
	w := newLWorld(t)
	keep := w.staff(uuid.Nil, RolePlatformAdmin, true)
	id := w.staff(uuid.Nil, RolePlatformAdmin, false)
	if err := w.exec(w.rt, uuid.Nil, `UPDATE staff_users SET status = 'suspended' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if err := w.exec(w.rt, uuid.Nil, `UPDATE staff_users SET status = 'active' WHERE id = $1`, id); err != nil {
		t.Fatalf("R1 platform half (accepted until S-H1 builds SF030): %v", err)
	}
	_ = keep
}

var _ = auth.PrincipalStaff

// Two-phase suspension, T2 retried ONCE on a lock timeout (ADR 0119 section 11): the target's row is held for longer
// than one lock_timeout but less than two; the first T2 attempt times out (55P03), the retry acquires the row and the
// suspension commits. Without the retry the call answers ErrBusy.
func TestTwoPhaseSuspension_T2RetriedOnceSucceedsWhenTheLockClears(t *testing.T) {
	w := newLWorld(t)
	w.svc.lockTimeout = 400 * time.Millisecond
	ta := w.staff(w.tenant, RoleTenantAdmin, true)
	target := w.staff(w.tenant, RoleSupport, true)
	hold, err := w.owner.Raw().Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Rollback(context.Background()) }()
	if _, err := hold.Exec(context.Background(), `SELECT set_config('app.tenant_id', $1, true)`, w.tenant.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := hold.Exec(context.Background(), `SELECT 1 FROM staff_users WHERE id = $1 FOR UPDATE`, target); err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(600 * time.Millisecond) // > one lock_timeout, < two
		_ = hold.Rollback(context.Background())
		close(released)
	}()
	res, err := w.svc.Suspend(context.Background(), w.actor(ta, w.tenant, RoleTenantAdmin), w.tenant, target, Reason{Code: "investigation"})
	<-released
	if err != nil {
		t.Fatalf("the retried T2 must succeed once the lock clears, got %v", err)
	}
	if res.Staff.Status != "suspended" {
		t.Fatalf("status %s", res.Staff.Status)
	}
}
