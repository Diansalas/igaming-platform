//go:build integration

// ADR 0112 slice 3: integration tests of internal/launchgov against the real database. Every
// assertion about what the application can or cannot do runs as igaming_runtime (non-superuser,
// non-BYPASSRLS, asserted below); the owner pool only seeds fixtures (the LF2 path) and runs
// owner-only probes. Requires TEST_DATABASE_URL and TEST_RUNTIME_DATABASE_URL; a skip is NOT
// evidence.
package launchgov_test

import (
	"context"
	"errors"
	"net/url"
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
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/launchgov"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

type world struct {
	t          *testing.T
	owner, rt  *db.Pool
	svc        *launchgov.Service
	ops        launchfix.OperatorIDs
	req        launchgov.Actor // fixture platform requester
	ap1, ap2   launchgov.Actor // fixture platform approvers (distinct Persons)
	ownerURL   string
	runtimeURL string
}

func connect(t *testing.T, u string) *db.Pool {
	t.Helper()
	p, err := db.Connect(context.Background(), u, 40, 5*time.Second)
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
	w := &world{t: t, ownerURL: ou, runtimeURL: ru, owner: connect(t, ou), rt: connect(t, ru)}
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
	plat := func(id uuid.UUID) launchgov.Actor {
		return launchgov.Actor{StaffID: id, Scope: launchgov.ScopePlatform, Role: auth.RolePlatformAdmin, RequestID: "it-" + id.String()[:8]}
	}
	w.req, w.ap1, w.ap2 = plat(w.ops.Requester), plat(w.ops.Approver1), plat(w.ops.Approver2)
	w.svc = w.service(launchgov.Config{})
	return w
}

// newScratchWorld is newWorld over a PRIVATE, freshly migrated scratch database. It exists for tests that take
// table-wide locks (TRUNCATE ... CASCADE takes ACCESS EXCLUSIVE locks on every table referencing the target): on the
// shared CI database those locks block, or deadlock with, other packages' concurrent withdrawal tests, and the other
// packages' open transactions block the TRUNCATE (hosted CI run #841). The runtime role keeps its default table
// privileges here (no deployed-posture REVOKE), so a runtime DELETE/TRUNCATE reaches the WD012 trigger itself.
func newScratchWorld(t *testing.T) *world {
	t.Helper()
	rtBase := os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if rtBase == "" {
		t.Skip("TEST_RUNTIME_DATABASE_URL is required (a skip is not evidence)")
	}
	ctx := context.Background()
	scratch := scratchdb.New(t, "lgdel_")
	owner := connect(t, scratch)
	su, err := url.Parse(scratch)
	if err != nil {
		t.Fatal(err)
	}
	dbName := strings.TrimPrefix(su.Path, "/")
	for _, stmt := range []string{
		`GRANT CONNECT ON DATABASE ` + pgx.Identifier{dbName}.Sanitize() + ` TO igaming_runtime`,
		`GRANT USAGE ON SCHEMA public TO igaming_runtime`,
		`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO igaming_runtime`,
		`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO igaming_runtime`,
	} {
		if _, err := owner.Raw().Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if _, err := owner.MigrateUp(ctx, "../../migrations"); err != nil {
		t.Fatalf("migrate scratch: %v", err)
	}
	ru, err := url.Parse(rtBase)
	if err != nil {
		t.Fatal(err)
	}
	ru.Path = su.Path
	w := &world{t: t, ownerURL: scratch, runtimeURL: ru.String(), owner: owner, rt: connect(t, ru.String())}
	var who string
	var super, bypass bool
	if err := w.rt.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT current_user::text, rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&who, &super, &bypass)
	}); err != nil {
		t.Fatal(err)
	}
	if who != "igaming_runtime" || super || bypass {
		t.Fatalf("the runtime pool must be igaming_runtime, non-superuser, non-BYPASSRLS; got %q super=%v bypass=%v", who, super, bypass)
	}
	prooftest.InstallVia(t, owner.Raw(), "lgdel-"+dbName)
	launchfix.SeedOperatorsAt(t, scratch)
	w.ops = launchfix.Operators()
	plat := func(id uuid.UUID) launchgov.Actor {
		return launchgov.Actor{StaffID: id, Scope: launchgov.ScopePlatform, Role: auth.RolePlatformAdmin, RequestID: "it-" + id.String()[:8]}
	}
	w.req, w.ap1, w.ap2 = plat(w.ops.Requester), plat(w.ops.Approver1), plat(w.ops.Approver2)
	w.svc = w.service(launchgov.Config{})
	return w
}

// service builds a Service over the RUNTIME pool with fast test timings.
func (w *world) service(cfg launchgov.Config) *launchgov.Service {
	w.t.Helper()
	cfg.Pool = w.rt
	if cfg.Environment == "" {
		cfg.Environment = "test"
	}
	if cfg.LockTimeout == "" {
		cfg.LockTimeout = "2s"
	}
	if cfg.Backoff == nil {
		cfg.Backoff = func(n int) time.Duration { return 20 * time.Millisecond * time.Duration(n) }
	}
	s, err := launchgov.New(cfg)
	if err != nil {
		w.t.Fatal(err)
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

// ---- fixtures ---------------------------------------------------------------------------------

// activeTenant provisions an ACTIVE tenant through the LF2 owner path.
func (w *world) activeTenant() uuid.UUID {
	w.t.Helper()
	var tenantID uuid.UUID
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		tn, err := launchfix.CreateTenant(ctx, tx, "lgov", "lgov-"+uuid.NewString(), "under_platform_licence")
		tenantID = tn.ID
		return err
	}); err != nil {
		w.t.Fatalf("seed active tenant: %v", err)
	}
	return tenantID
}

// pendingTenant creates a pending_launch tenant through the RUNTIME role (identity.CreateTenant).
func (w *world) pendingTenant() uuid.UUID {
	w.t.Helper()
	var tenantID uuid.UUID
	if err := w.rt.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		tn, err := identity.CreateTenant(ctx, tx, "lgov pending", "lgov-p-"+uuid.NewString(), "under_platform_licence")
		tenantID = tn.ID
		return err
	}); err != nil {
		w.t.Fatalf("create pending tenant: %v", err)
	}
	return tenantID
}

// activeBrand provisions an ACTIVE brand (owner path); pendingBrand creates pending_launch (runtime).
func (w *world) activeBrand(tenantID uuid.UUID) uuid.UUID {
	w.t.Helper()
	var id uuid.UUID
	if err := w.owner.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		b, err := launchfix.CreateBrand(ctx, tx, tenantID, "lgov brand", "lgov-b-"+uuid.NewString())
		id = b.ID
		return err
	}); err != nil {
		w.t.Fatalf("seed active brand: %v", err)
	}
	return id
}

func (w *world) pendingBrand(tenantID uuid.UUID) uuid.UUID {
	w.t.Helper()
	var id uuid.UUID
	if err := w.rt.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		b, err := identity.CreateBrand(ctx, tx, tenantID, "lgov pending brand", "lgov-pb-"+uuid.NewString())
		id = b.ID
		return err
	}); err != nil {
		w.t.Fatalf("create pending brand: %v", err)
	}
	return id
}

// jurisdiction creates a jurisdiction and an in-effect tenant configuration for the tenant with
// all three rulesets, EUR and one payment method (the readiness checks that depend only on
// configuration then pass; R-PAY-METHODS still lacks an approved credential handle).
func (w *world) jurisdiction(tenantID uuid.UUID) uuid.UUID {
	w.t.Helper()
	ctx := context.Background()
	jid := uuid.New()
	if err := w.owner.WithPlatformAdmin(ctx, w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'lgov fixture')`, jid, "LG-"+jid.String()[:12])
		return err
	}); err != nil {
		w.t.Fatalf("seed jurisdiction: %v", err)
	}
	if err := w.owner.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenant_jurisdiction_configs
			(tenant_id, jurisdiction_id, kyc_ruleset_id, aml_ruleset_id, rg_ruleset_id, allowed_currencies, allowed_payment_methods, effective_from)
			VALUES ($1, $2, 'kyc1', 'aml1', 'rg1', '["EUR"]', '["card"]', now() - interval '1 hour')`, tenantID, jid)
		return err
	}); err != nil {
		w.t.Fatalf("seed jurisdiction config: %v", err)
	}
	return jid
}

// licence creates a licence of the given licensee and binds it to the tenant (whose
// licensing_model must agree).
func (w *world) licence(tenantID uuid.UUID, jid uuid.UUID, licensee, status string) uuid.UUID {
	w.t.Helper()
	ctx := context.Background()
	lid := uuid.New()
	if err := w.owner.WithPlatformAdmin(ctx, w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO licences (id, jurisdiction_id, licensee, licence_number, status) VALUES ($1, $2, $3, $4, $5)`,
			lid, jid, licensee, "LN-"+lid.String()[:8], status); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE tenants SET licence_id = $1 WHERE id = $2`, lid, tenantID)
		return err
	}); err != nil {
		w.t.Fatalf("seed licence: %v", err)
	}
	return lid
}

// tenantStaff creates an active tenant_admin linked to a Person and returns its actor.
func (w *world) tenantStaff(tenantID uuid.UUID, role auth.Role) launchgov.Actor {
	w.t.Helper()
	var staff identity.StaffUser
	if err := w.owner.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		p, err := identity.CreatePerson(ctx, tx)
		if err != nil {
			return err
		}
		staff, err = identity.CreateStaffUser(ctx, tx, tenantID, "lgov-"+uuid.NewString()+"@fixture.invalid", "!x", identity.StaffRoleTenantAdmin, &p.ID)
		return err
	}); err != nil {
		w.t.Fatalf("seed tenant staff: %v", err)
	}
	return launchgov.Actor{StaffID: staff.ID, Scope: launchgov.ScopeTenant, TenantID: tenantID, Role: role, RequestID: "it-tenant"}
}

// extraPlatformStaff creates one more platform admin with its own Person (for the x50 races).
func (w *world) extraPlatformStaff() launchgov.Actor {
	w.t.Helper()
	id, person := uuid.New(), uuid.New()
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, person); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id)
			VALUES ($1, NULL, $2, '!lgov-not-a-login', 'platform_admin', $3)`, id, "lgov-extra-"+id.String()+"@fixture.invalid", person)
		return err
	}); err != nil {
		w.t.Fatalf("seed extra platform staff: %v", err)
	}
	return launchgov.Actor{StaffID: id, Scope: launchgov.ScopePlatform, Role: auth.RolePlatformAdmin, RequestID: "it-extra"}
}

func (w *world) status(sub launchgov.Subject) string {
	w.t.Helper()
	var s string
	err := w.owner.WithTenant(context.Background(), sub.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if sub.BrandID != nil {
			return tx.QueryRow(ctx, `SELECT status FROM brands WHERE id = $1`, *sub.BrandID).Scan(&s)
		}
		return tx.QueryRow(ctx, `SELECT status FROM tenants WHERE id = $1`, sub.TenantID).Scan(&s)
	})
	if err != nil {
		w.t.Fatalf("read status: %v", err)
	}
	return s
}

// activation builds a valid activation input with a recorded basis (other_manually_approved) and the
// acknowledgements the readiness requires.
func (w *world) activation(sub launchgov.Subject, jid uuid.UUID) launchgov.RequestInput {
	w.t.Helper()
	in := launchgov.RequestInput{
		Subject: sub, Action: launchgov.ActionActivate, ReasonCode: "initial_launch",
		Licensing: &launchgov.LicensingRecord{
			Model: launchgov.LicModelOther, Status: launchgov.LicStatusConditional,
			DeterminationReference: "ext-ref-001", DeterminationByRole: "external counsel",
		},
		ResponsibleOperatorName: "Operator Ltd", ResponsibleOperatorRegistration: "REG-4711",
		JurisdictionIDs: []uuid.UUID{jid}, ConditionsNote: "review again after the first quarter",
	}
	in.AcknowledgedCheckCodes = w.nonPassing(in)
	return in
}

func (w *world) nonPassing(in launchgov.RequestInput) []string {
	w.t.Helper()
	r, err := w.svc.EvaluateReadiness(context.Background(), launchgov.EvalInput{Subject: in.Subject, Action: in.Action, Licensing: in.Licensing, JurisdictionIDs: in.JurisdictionIDs})
	if err != nil {
		w.t.Fatalf("readiness: %v", err)
	}
	return r.NonPassing()
}

// seen is the readiness hash an approver would be shown for a request right now.
func (w *world) seen(r launchgov.Request) string {
	w.t.Helper()
	got, _, err := w.svc.GetRequest(context.Background(), w.ap1, r.TenantID, r.ID)
	if err != nil {
		w.t.Fatal(err)
	}
	rd, err := w.svc.EvaluateReadiness(context.Background(), launchgov.EvalInputOf(*got))
	if err != nil {
		w.t.Fatal(err)
	}
	return rd.Hash()
}

func (w *world) approve(who launchgov.Actor, r launchgov.Request) (*launchgov.DecisionResult, error) {
	w.t.Helper()
	return w.svc.Decide(context.Background(), who, r.TenantID, r.ID, launchgov.DecideInput{
		Decision: launchgov.DecisionApprove, ReasonCode: "reviewed_ok", SeenReadinessHash: w.seen(r),
	})
}

func (w *world) mustRequest(who launchgov.Actor, in launchgov.RequestInput) launchgov.Request {
	w.t.Helper()
	res, err := w.svc.Request(context.Background(), who, in)
	if err != nil {
		w.t.Fatalf("request %s: %v", in.Action, err)
	}
	return res.Request
}

// audit returns the audit rows of an action whose subject tenant (platform scope) or own tenant
// (tenant scope) is the tenant.
func (w *world) audit(tenantID uuid.UUID, action string) []map[string]any {
	w.t.Helper()
	var out []map[string]any
	read := func(tx pgx.Tx, ctx context.Context, tenantOnly bool) error {
		rows, err := tx.Query(ctx, `SELECT outcome, metadata, tenant_id IS NULL AS platform_scope FROM audit_log
			WHERE action = $1 AND (subject_tenant_id = $2 OR tenant_id = $2) AND (NOT $3 OR tenant_id IS NOT NULL) ORDER BY created_at, id`, action, tenantID, tenantOnly)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var outcome string
			var md map[string]any
			var plat bool
			if err := rows.Scan(&outcome, &md, &plat); err != nil {
				return err
			}
			md["_outcome"], md["_platform_scope"] = outcome, plat
			out = append(out, md)
		}
		return rows.Err()
	}
	// Platform-scope rows (subject_tenant_id) are visible with no tenant GUC; tenant-scope rows
	// (a tenant session's own) only under that tenant: read both, platform rows first.
	if err := w.owner.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error { return read(tx, ctx, false) }); err != nil {
		w.t.Fatalf("read platform audit: %v", err)
	}
	if err := w.owner.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error { return read(tx, ctx, true) }); err != nil {
		w.t.Fatalf("read tenant audit: %v", err)
	}
	return out
}

func (w *world) transitions(sub launchgov.Subject) []string {
	w.t.Helper()
	var kinds []string
	err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT kind || ':' || COALESCE(from_status, '-') || '>' || to_status FROM launch_status_transitions
			WHERE tenant_id = $1 AND brand_id IS NOT DISTINCT FROM $2 ORDER BY created_at, id`, sub.TenantID, sub.BrandID)
		if err != nil {
			return err
		}
		kinds, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		w.t.Fatalf("read transitions: %v", err)
	}
	return kinds
}

// bareJurisdiction creates a jurisdiction no tenant has configured.
func (w *world) bareJurisdiction() uuid.UUID {
	w.t.Helper()
	jid := uuid.New()
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'lgov bare')`, jid, "LB-"+jid.String()[:12])
		return err
	}); err != nil {
		w.t.Fatalf("seed bare jurisdiction: %v", err)
	}
	return jid
}

// endConfig ends the tenant's configuration for the jurisdiction (effective_to in the past).
func (w *world) endConfig(tenantID, jid uuid.UUID) {
	w.t.Helper()
	if err := w.owner.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE tenant_jurisdiction_configs SET effective_to = now() - interval '1 minute' WHERE tenant_id = $1 AND jurisdiction_id = $2`, tenantID, jid)
		return err
	}); err != nil {
		w.t.Fatalf("end config: %v", err)
	}
}

// unboundLicence creates a licence of the licensee that no tenant is bound to.
func (w *world) unboundLicence(jid uuid.UUID, licensee string) uuid.UUID {
	w.t.Helper()
	lid := uuid.New()
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO licences (id, jurisdiction_id, licensee, licence_number, status) VALUES ($1, $2, $3, $4, 'active')`,
			lid, jid, licensee, "LU-"+lid.String()[:8])
		return err
	}); err != nil {
		w.t.Fatalf("seed licence: %v", err)
	}
	return lid
}

type ownTenant struct{ tenant, juris, licence uuid.UUID }

// ownLicenceTenant provisions an own_licence tenant bound to a tenant-licensee licence, with a
// jurisdiction configuration.
func (w *world) ownLicenceTenant(jid uuid.UUID) ownTenant {
	w.t.Helper()
	var o ownTenant
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		tn, err := launchfix.CreateTenant(ctx, tx, "lgov own", "lgov-own-"+uuid.NewString(), "own_licence")
		o.tenant = tn.ID
		return err
	}); err != nil {
		w.t.Fatalf("seed own_licence tenant: %v", err)
	}
	o.juris = w.jurisdiction(o.tenant)
	o.licence = w.licence(o.tenant, jid, "tenant", "active")
	return o
}
