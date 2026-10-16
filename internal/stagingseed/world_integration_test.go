//go:build integration

package stagingseed

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/staffmfa"
	"github.com/Diansalas/igaming-platform/internal/staffmfa/store"
)

func testPool(t *testing.T) *db.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	pool, err := db.Connect(context.Background(), url, 5, 5*time.Second)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Raw().Exec(context.Background(), `INSERT INTO database_environment (environment) VALUES ('test') ON CONFLICT (singleton) DO NOTHING`); err != nil {
		t.Fatalf("record the test anchor: %v", err)
	}
	return pool
}

func mfaStore(t *testing.T) *store.Store {
	t.Helper()
	key := func() string {
		b := make([]byte, 48)
		_, _ = rand.Read(b)
		return base64.StdEncoding.EncodeToString(b)
	}
	keys, err := staffmfa.LoadKeys("f1", "f1:"+key(), "m1", "m1:"+key())
	if err != nil {
		t.Fatalf("keys: %v", err)
	}
	st, err := store.New(keys)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func platformAdmin(t *testing.T, pool *db.Pool) uuid.UUID {
	t.Helper()
	id, person := uuid.New(), uuid.New()
	err := pool.WithPlatformAdmin(context.Background(), id, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, person); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id, status) VALUES ($1, NULL, $2, 'x', 'platform_admin', $3, 'active')`, id, id.String()+"@example.com", person)
		return err
	})
	if err != nil {
		t.Fatalf("seed platform admin: %v", err)
	}
	return id
}

func count(t *testing.T, pool *db.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error { return tx.QueryRow(ctx, q, args...).Scan(&n) })
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// countT runs a count query inside the tenant scope of the slug; the query uses $1 for the tenant id.
func countT(t *testing.T, pool *db.Pool, slug, q string) int {
	t.Helper()
	var id uuid.UUID
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM tenants WHERE slug = $1`, slug).Scan(&id)
	}); err != nil {
		t.Fatalf("tenant %s: %v", slug, err)
	}
	var n int
	if err := pool.WithTenant(context.Background(), id, func(ctx context.Context, tx pgx.Tx) error { return tx.QueryRow(ctx, q, id).Scan(&n) }); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func params(actor uuid.UUID) WorldParams {
	s := strings.ToLower(uuid.NewString()[:8])
	return WorldParams{TenantSlug: "sw-" + s, BrandSlug: "sw-" + s + "-brand", SecondTenantSlug: "sw2-" + s, SecondBrandSlug: "sw2-" + s + "-brand",
		Country: "SV", AuthRef: "TICKET-SW-1", Operator: "ops-test", Reason: "seed-world-test", ActorID: actor}
}

func TestWorld_PlanApplyCreatesTheSyntheticWorldAndNeverPrintsACredential(t *testing.T) {
	pool := testPool(t)
	mfa := mfaStore(t)
	p := params(platformAdmin(t, pool))
	ctx := context.Background()

	var out bytes.Buffer
	plan, err := PlanWorld(ctx, pool, p, "staging", &out)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Actors) != len(WorldStaff) {
		t.Fatalf("a fresh world plans %d accounts, got %d", len(WorldStaff), len(plan.Actors))
	}
	if n := count(t, pool, `SELECT count(*) FROM tenants WHERE slug = $1`, p.TenantSlug); n != 0 {
		t.Fatalf("plan is read-only, but the tenant exists")
	}

	res, err := ApplyWorld(ctx, pool, mfa, p, "staging", plan, &out)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, out.String())
	}
	if len(res.Actors) != len(WorldStaff) {
		t.Fatalf("expected %d created accounts, got %d", len(WorldStaff), len(res.Actors))
	}
	for k, a := range res.Actors {
		if a.Password == "" || len(a.BootstrapToken) != 64 || a.Email == "" {
			t.Errorf("%s: incomplete credential", k)
		}
		if strings.Contains(out.String(), a.Password) || strings.Contains(out.String(), a.BootstrapToken) {
			t.Errorf("%s: a credential reached the output", k)
		}
	}
	// distinct Persons for the two finance accounts (four-eyes)
	if n := countT(t, pool, p.TenantSlug, `SELECT count(DISTINCT person_id) FROM staff_users WHERE tenant_id = $1 AND role = 'finance'`); n != 2 {
		t.Errorf("two finance accounts need two distinct persons, got %d", n)
	}
	for _, slug := range []string{p.TenantSlug, p.SecondTenantSlug} {
		if n := count(t, pool, `SELECT count(*) FROM tenants WHERE slug = $1 AND status = 'active'`, slug); n != 1 {
			t.Errorf("tenant %s must be active, got %d", slug, n)
		}
		if n := count(t, pool, `SELECT count(*) FROM launch_status_transitions lt JOIN tenants t ON t.id = lt.tenant_id WHERE t.slug = $1 AND lt.kind = 'owner_provisioned'`, slug); n < 1 {
			t.Errorf("tenant %s: the platform must have recorded an owner_provisioned transition", slug)
		}
	}
	if n := countT(t, pool, p.SecondTenantSlug, `SELECT count(*) FROM staff_users WHERE tenant_id = $1`); n != 0 {
		t.Errorf("the second tenant has no staff, got %d", n)
	}
	if n := countT(t, pool, p.TenantSlug, `SELECT count(*) FROM staff_enrolment_tokens WHERE tenant_id = $1 AND purpose = 'bootstrap' AND consumed_at IS NULL AND revoked_at IS NULL`); n != len(WorldStaff) {
		t.Errorf("each account needs one live bootstrap token, got %d", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM audit_log WHERE action = 'staging_seed.tenant_owner_provisioned' AND metadata->>'slug' = $1 AND metadata->>'operator' = 'ops-test'`, p.TenantSlug); n != 1 {
		t.Errorf("expected one tenant provisioning audit row, got %d", n)
	}
	if n := countT(t, pool, p.TenantSlug, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'staging_seed.staff_created'`); n != len(WorldStaff) {
		t.Errorf("expected %d staff audit rows, got %d", len(WorldStaff), n)
	}
	if !strings.Contains(out.String(), "market_"+p.TenantSlug+"|resolves_sportsbook_wagering|permitted") {
		t.Errorf("the market rows must open the gate:\n%s", out.String())
	}

	// idempotent: nothing more to plan, nothing to apply, no duplicates
	plan2, err := PlanWorld(ctx, pool, p, "staging", &bytes.Buffer{})
	if err != nil || len(plan2.Actors) != 0 {
		t.Fatalf("a second plan must be empty, got %v %v", plan2.Actors, err)
	}
	res2, err := ApplyWorld(ctx, pool, mfa, p, "staging", plan2, &bytes.Buffer{})
	if err != nil || len(res2.Actors) != 0 {
		t.Fatalf("a second apply must create nothing: %v %v", res2.Actors, err)
	}
	if n := countT(t, pool, p.TenantSlug, `SELECT count(*) FROM staff_users WHERE tenant_id = $1`); n != len(WorldStaff) {
		t.Errorf("no duplicate staff, got %d", n)
	}
}

func TestWorld_StalePlanDoesNotOverwriteAnExistingAccount(t *testing.T) {
	pool := testPool(t)
	mfa := mfaStore(t)
	p := params(platformAdmin(t, pool))
	ctx := context.Background()
	plan, _ := PlanWorld(ctx, pool, p, "staging", &bytes.Buffer{})
	if _, err := ApplyWorld(ctx, pool, mfa, p, "staging", plan, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	// replaying the same plan: every account exists, so none is re-created and no token is issued
	res, err := ApplyWorld(ctx, pool, mfa, p, "staging", plan, &bytes.Buffer{})
	if err != nil || len(res.Actors) != 0 {
		t.Fatalf("a replayed plan must create nothing: %v %v", res.Actors, err)
	}
}

func TestWorld_Gates(t *testing.T) {
	pool := testPool(t)
	mfa := mfaStore(t)
	ctx := context.Background()
	p := params(platformAdmin(t, pool))
	if _, err := PlanWorld(ctx, pool, p, "production", &bytes.Buffer{}); err == nil {
		t.Error("APP_ENV=production must be refused")
	}
	q := p
	q.ActorID = uuid.New()
	if _, err := PlanWorld(ctx, pool, q, "staging", &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "not an active platform_admin") {
		t.Errorf("the plan (a dry run) must already refuse an unknown actor, got %v", err)
	}
	plan, _ := PlanWorld(ctx, pool, p, "staging", &bytes.Buffer{})
	if _, err := ApplyWorld(ctx, pool, mfa, q, "staging", plan, &bytes.Buffer{}); err == nil {
		t.Error("apply with an unknown actor must be refused")
	}
	other := p
	other.TenantSlug = "different-" + p.TenantSlug
	if _, err := ApplyWorld(ctx, pool, mfa, other, "staging", plan, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "different tenant") {
		t.Errorf("a credentials file of another tenant must be refused, got %v", err)
	}
	bad := p
	bad.Country = "xx"
	if _, err := PlanWorld(ctx, pool, bad, "staging", &bytes.Buffer{}); err == nil {
		t.Error("a malformed country must be refused")
	}
	if n := count(t, pool, `SELECT count(*) FROM tenants WHERE slug IN ($1, $2)`, p.TenantSlug, q.TenantSlug); n != 0 {
		t.Errorf("a refused run must create no tenant, got %d", n)
	}
}

func TestWorld_RefusesToTouchAPendingTenant(t *testing.T) {
	pool := testPool(t)
	mfa := mfaStore(t)
	ctx := context.Background()
	admin := platformAdmin(t, pool)
	p := params(admin)
	if err := pool.WithPlatformAdmin(ctx, admin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (name, slug, licensing_model, status) VALUES ('x', $1, 'under_platform_licence', 'pending_launch')`, p.TenantSlug)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	plan, _ := PlanWorld(ctx, pool, p, "staging", &bytes.Buffer{})
	if _, err := ApplyWorld(ctx, pool, mfa, p, "staging", plan, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "governed launch decision") {
		t.Fatalf("an existing pending tenant must not be activated by this tool, got %v", err)
	}
}

// setAnchor replaces the write-once anchor of a SCRATCH test database ("" = no row): the table owner disables the guard trigger inside one
// transaction and re-enables it before committing. Test-only.
func setAnchor(t *testing.T, pool *db.Pool, value string) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Raw().Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, q := range []string{`ALTER TABLE database_environment DISABLE TRIGGER database_environment_guard`, `DELETE FROM database_environment`} {
		if _, err := tx.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if value != "" {
		if _, err := tx.Exec(ctx, `INSERT INTO database_environment (environment) VALUES ($1)`, value); err != nil {
			t.Fatalf("insert anchor: %v", err)
		}
	}
	if _, err := tx.Exec(ctx, `ALTER TABLE database_environment ENABLE TRIGGER database_environment_guard`); err != nil {
		t.Fatalf("re-enable the guard: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// SRP3-1: with no anchor row (= production) or a production anchor, both phases refuse before reading or writing anything.
func TestWorld_RefusesADatabaseWhoseAnchorIsMissingOrProduction(t *testing.T) {
	pool := testPool(t)
	mfa := mfaStore(t)
	ctx := context.Background()
	p := params(platformAdmin(t, pool))
	t.Cleanup(func() { setAnchor(t, pool, "test") })
	for _, anchor := range []string{"", "production"} {
		setAnchor(t, pool, anchor)
		if _, err := PlanWorld(ctx, pool, p, "staging", &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "anchor") {
			t.Fatalf("anchor %q: plan must be refused on the anchor, got %v", anchor, err)
		}
		plan := Creds{TenantSlug: p.TenantSlug, BrandSlug: p.BrandSlug, Country: p.Country, Actors: map[string]*ActorCred{}}
		if _, err := ApplyWorld(ctx, pool, mfa, p, "staging", plan, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "anchor") {
			t.Fatalf("anchor %q: apply must be refused on the anchor, got %v", anchor, err)
		}
		if n := count(t, pool, `SELECT count(*) FROM tenants WHERE slug = $1`, p.TenantSlug); n != 0 {
			t.Fatalf("anchor %q: a refused run created %d tenant(s)", anchor, n)
		}
	}
}
