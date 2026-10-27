//go:build integration

// Migration 0177 integration tests (MKT-DUAL-1). Every assertion about what the application can do runs as igaming_runtime
// (non-superuser, non-BYPASSRLS, asserted); the owner pool only provisions fixtures (the documented owner path) and reads
// for assertions. Requires TEST_DATABASE_URL and TEST_RUNTIME_DATABASE_URL (a skip is NOT evidence).
package licensingchange

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

type world struct {
	t        *testing.T
	ownerURL string
	owner    *db.Pool
	rt       *db.Pool
	svc      *Service
	ops      launchfix.OperatorIDs
	// platform actors: requester, two approvers (three Persons), a third approver (a fourth Person) and a second
	// account of approver 1's Person.
	req, ap1, ap2, ap3, ap1Twin, reqTwin Actor
}

func connect(t *testing.T, u string) *db.Pool {
	t.Helper()
	p, err := db.Connect(context.Background(), u, 20, 5*time.Second)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

func newWorld(t *testing.T) *world {
	t.Helper()
	ou, ru := os.Getenv("TEST_DATABASE_URL"), os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if ou == "" || ru == "" {
		t.Skip("TEST_DATABASE_URL and TEST_RUNTIME_DATABASE_URL are required (a skip is not evidence)")
	}
	w := &world{t: t, ownerURL: ou, owner: connect(t, ou), rt: connect(t, ru)}
	var who string
	var super, bypass bool
	if err := w.rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT current_user::text, rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&who, &super, &bypass)
	}); err != nil {
		t.Fatal(err)
	}
	if who != "igaming_runtime" || super || bypass {
		t.Fatalf("the runtime pool must be igaming_runtime, non-superuser, non-BYPASSRLS; got %q super=%v bypass=%v", who, super, bypass)
	}
	prooftest.Install(t, ou)
	launchfix.SeedOperators(t)
	w.ops = launchfix.Operators()
	plat := func(id uuid.UUID) Actor {
		return Actor{StaffID: id, Scope: ScopePlatform, Role: string(auth.RolePlatformAdmin), RequestID: "lc-" + id.String()[:8]}
	}
	w.req, w.ap1, w.ap2 = plat(w.ops.Requester), plat(w.ops.Approver1), plat(w.ops.Approver2)
	w.ap3 = plat(w.platformStaff(uuid.Nil))
	w.ap1Twin = plat(w.platformStaff(w.ops.Persons[1]))
	w.reqTwin = plat(w.platformStaff(w.ops.Persons[0]))
	w.svc = w.service(Config{})
	return w
}

func (w *world) service(cfg Config) *Service {
	w.t.Helper()
	cfg.Pool = w.rt
	if cfg.LockTimeout == "" {
		cfg.LockTimeout = "3s"
	}
	s, err := New(cfg)
	if err != nil {
		w.t.Fatal(err)
	}
	return s
}

// platformStaff provisions a platform_admin account (owner path) for person (a fresh Person when uuid.Nil).
func (w *world) platformStaff(person uuid.UUID) uuid.UUID {
	w.t.Helper()
	id := uuid.New()
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		if person == uuid.Nil {
			person = uuid.New()
			if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, person); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id)
			VALUES ($1, NULL, $2, '!lc-fixture-not-a-login', 'platform_admin', $3)`, id, "lc-"+id.String()+"@fixture.invalid", person)
		return err
	}); err != nil {
		w.t.Fatalf("seed platform staff: %v", err)
	}
	return id
}

// tenantStaff provisions a tenant staff account with its own Person (owner path).
func (w *world) tenantStaff(tenantID uuid.UUID, role string) Actor {
	w.t.Helper()
	id, person := uuid.New(), uuid.New()
	if err := w.owner.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, person); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id) VALUES ($1, $2, $3, '!x', $4, $5)`,
			id, tenantID, "lc-"+id.String()+"@fixture.invalid", role, person)
		return err
	}); err != nil {
		w.t.Fatalf("seed tenant staff: %v", err)
	}
	return Actor{StaffID: id, Scope: ScopeTenant, TenantID: tenantID, Role: role, RequestID: "lc-t-" + id.String()[:8]}
}

// tenant provisions an ACTIVE tenant (owner path) with the licensing model.
func (w *world) tenant(model string) uuid.UUID {
	w.t.Helper()
	var id uuid.UUID
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		tn, err := launchfix.CreateTenant(ctx, tx, "lc", "lc-"+uuid.NewString(), model)
		id = tn.ID
		return err
	}); err != nil {
		w.t.Fatalf("seed tenant: %v", err)
	}
	return id
}

func (w *world) brand(tenantID uuid.UUID) uuid.UUID {
	w.t.Helper()
	var id uuid.UUID
	if err := w.owner.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		b, err := launchfix.CreateBrand(ctx, tx, tenantID, "lc brand", "lcb-"+uuid.NewString())
		id = b.ID
		return err
	}); err != nil {
		w.t.Fatalf("seed brand: %v", err)
	}
	return id
}

// jurisdiction provisions a jurisdiction (owner path).
func (w *world) jurisdiction() uuid.UUID {
	w.t.Helper()
	id := uuid.New()
	w.ownerExec(`INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'lc fixture')`, id, "LC-"+strings.ToUpper(id.String()[:10]))
	return id
}

// licence provisions an active licence and binds it to tenantID when not Nil (owner path).
func (w *world) licence(jid uuid.UUID, licensee string, tenantID uuid.UUID) uuid.UUID {
	w.t.Helper()
	id := uuid.New()
	w.ownerExec(`INSERT INTO licences (id, jurisdiction_id, licensee, licence_number) VALUES ($1, $2, $3, $4)`, id, jid, licensee, "LC-"+id.String()[:8])
	if tenantID != uuid.Nil {
		w.ownerExec(`UPDATE tenants SET licence_id = $1 WHERE id = $2`, id, tenantID)
	}
	return id
}

func (w *world) ownerExec(sql string, args ...any) {
	w.t.Helper()
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, sql, args...)
		return err
	}); err != nil {
		w.t.Fatalf("owner exec %q: %v", sql, err)
	}
}

func (w *world) ownerInt(sql string, args ...any) int {
	w.t.Helper()
	var n int
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(&n)
	}); err != nil {
		w.t.Fatalf("owner query %q: %v", sql, err)
	}
	return n
}

// ownerTenantInt reads under a tenant-scoped owner session (tenant-RLS tables).
func (w *world) ownerTenantInt(tenantID uuid.UUID, sql string, args ...any) int {
	w.t.Helper()
	var n int
	if err := w.owner.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(&n)
	}); err != nil {
		w.t.Fatalf("owner tenant query %q: %v", sql, err)
	}
	return n
}

func payload(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sp(s string) *string { return &s }

func (w *world) mustRequest(a Actor, kind Kind, p any, reason string) *Result {
	w.t.Helper()
	res, err := w.svc.Request(context.Background(), a, RequestInput{Kind: kind, Payload: payload(w.t, p), ReasonCode: reason})
	if err != nil {
		w.t.Fatalf("request %s: %v", kind, err)
	}
	return res
}

func (w *world) approve(a Actor, id uuid.UUID) (*Result, error) {
	return w.svc.Approve(context.Background(), a, id, "reviewed_ok")
}

func (w *world) mustApprove(a Actor, id uuid.UUID) *Result {
	w.t.Helper()
	res, err := w.approve(a, id)
	if err != nil {
		w.t.Fatalf("approve: %v", err)
	}
	return res
}

func (w *world) status(id uuid.UUID) string {
	w.t.Helper()
	var s string
	if err := w.rt.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM licensing_change_requests WHERE id = $1`, id).Scan(&s)
	}); err != nil {
		w.t.Fatalf("read status: %v", err)
	}
	return s
}

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func sqlstateOf(err error) string {
	if c := pgCode(err); c != "" {
		return c
	}
	return GuardCode(err)
}

func countryCode() string {
	// A real ISO-3166-1 alpha-2 code distinct per test run is not needed: every ceiling / policy row is keyed by a fresh
	// licence / tenant, so a fixed code never collides.
	return "MT"
}

func mustNotErr(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

func expectErr(t *testing.T, err error, want error, what string) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: got %v, want %v", what, err, want)
	}
}

var _ = fmt.Sprintf
