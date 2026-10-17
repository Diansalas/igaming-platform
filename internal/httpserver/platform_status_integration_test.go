//go:build integration

package httpserver

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/buildinfo"
)

// F6 / SA-3: GET /v1/admin/platform/status through the real runtime role against a throwaway database.

const platformStatusPath = "/v1/admin/platform/status"

func (a *arAPI) migrationFacts() (head, count int64) {
	a.t.Helper()
	if err := a.w.Owner.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT max(version), count(*) FROM schema_migrations`).Scan(&head, &count)
	}); err != nil {
		a.t.Fatal(err)
	}
	return head, count
}

func TestPlatformStatus_PlatformAdminReadsBuildMigrationHeadAndAnchor(t *testing.T) {
	a := newARAPI(t, false)
	res := a.do("GET", platformStatusPath, a.adminToken(), nil)
	if res.status != http.StatusOK {
		t.Fatalf("platform admin: %d %s", res.status, res.body)
	}
	m := res.json()
	if len(m) != 3 {
		t.Fatalf("the response is exactly build, migrations and database_environment, got %s", res.body)
	}
	build, _ := m["build"].(map[string]any)
	if len(build) != 1 || build["commit"] != buildinfo.CommitID() {
		t.Fatalf("build = %v, want only the validated commit %q", build, buildinfo.CommitID())
	}
	if c, _ := build["commit"].(string); !regexp.MustCompile(`^([0-9a-f]{7,64}|unknown)$`).MatchString(c) {
		t.Fatalf("commit %q is neither a hex sha nor unknown", c)
	}
	head, count := a.migrationFacts()
	mig, _ := m["migrations"].(map[string]any)
	if len(mig) != 2 || int64(mig["head_version"].(float64)) != head || int64(mig["applied_count"].(float64)) != count {
		t.Fatalf("migrations = %v, want head %d and count %d read from schema_migrations", mig, head, count)
	}
	if head < 170 {
		t.Fatalf("migration head %d is below 170 (this feature's own migration)", head)
	}
	env, _ := m["database_environment"].(map[string]any)
	if len(env) != 2 || env["anchor"] != "unrecorded" || env["recorded"] != false {
		t.Fatalf("a fresh scratch database has no anchor: %v", env)
	}
	for _, leak := range []string{"postgres://", "password", "host=", "secret", "checksum", "igaming_runtime"} {
		if strings.Contains(strings.ToLower(string(res.body)), leak) {
			t.Fatalf("the response must not carry %q: %s", leak, res.body)
		}
	}
}

func TestPlatformStatus_ReportsEveryRecordedAnchorAsAClosedToken(t *testing.T) {
	for _, anchor := range []string{"production", "staging", "development", "test"} {
		t.Run(anchor, func(t *testing.T) {
			a := newARAPI(t, false)
			if err := a.w.Owner.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `INSERT INTO database_environment (environment) VALUES ($1)`, anchor)
				return err
			}); err != nil {
				t.Fatalf("record anchor: %v", err)
			}
			res := a.do("GET", platformStatusPath, a.adminToken(), nil)
			if res.status != http.StatusOK {
				t.Fatalf("%d %s", res.status, res.body)
			}
			env, _ := res.json()["database_environment"].(map[string]any)
			if env["anchor"] != anchor || env["recorded"] != true {
				t.Fatalf("database_environment = %v, want anchor %q recorded", env, anchor)
			}
		})
	}
}

func TestPlatformStatusAnchor_ClosedTokenMapping(t *testing.T) {
	cases := map[string]platformStatusEnvironment{
		"":             {Anchor: "unrecorded", Recorded: false},
		"production":   {Anchor: "production", Recorded: true},
		"staging":      {Anchor: "staging", Recorded: true},
		"development":  {Anchor: "development", Recorded: true},
		"test":         {Anchor: "test", Recorded: true},
		"prod":         {Anchor: "unrecognised", Recorded: true},
		"Production":   {Anchor: "unrecognised", Recorded: true},
		"x\nINFO fake": {Anchor: "unrecognised", Recorded: true},
	}
	for in, want := range cases {
		if got := platformStatusAnchor(in); got != want {
			t.Errorf("platformStatusAnchor(%q) = %+v, want %+v", in, got, want)
		}
	}
}

// Authorisation: only a platform-scope token holding platform_status:read (platform_admin alone). Every refusal writes
// no audit row and returns no status data.
func TestPlatformStatus_OnlyPlatformAdminAndEveryRefusalIsDataFreeAndUnaudited(t *testing.T) {
	a := newARAPI(t, false)
	tenantID := a.w.SeedTenant(a.admin)
	before := len(a.platformAudits(AuditActionPlatformStatusRead))

	if res := a.do("GET", platformStatusPath, "", nil); res.status != http.StatusUnauthorized {
		t.Fatalf("no token: %d", res.status)
	}
	if res := a.do("GET", platformStatusPath, "not-a-token", nil); res.status != http.StatusUnauthorized {
		t.Fatalf("garbage token: %d", res.status)
	}
	for _, role := range []auth.Role{auth.RoleTenantAdmin, auth.RoleCompliance, auth.RoleFinance, auth.RoleSupport, auth.RoleRiskManager} {
		staff := a.tenantStaff(tenantID, string(role))
		res := a.do("GET", platformStatusPath, a.token(staff, tenantID, role), nil)
		if res.status != http.StatusForbidden {
			t.Errorf("tenant %s: %d, want 403", role, res.status)
		}
		for _, leak := range []string{"head_version", "commit", "anchor"} {
			if strings.Contains(string(res.body), leak) {
				t.Errorf("tenant %s: the refusal carried %q: %s", role, leak, res.body)
			}
		}
	}
	// A tenant-scoped token claiming the platform_admin role is refused (the staff session is bound to the tenant
	// staff row, so the chain refuses it; either way it is never a 200).
	staff := a.tenantStaff(tenantID, string(auth.RoleTenantAdmin))
	if res := a.do("GET", platformStatusPath, a.token(staff, tenantID, auth.RolePlatformAdmin), nil); res.status == http.StatusOK {
		t.Fatalf("a tenant-scoped token must never read the platform status: %s", res.body)
	}
	if after := len(a.platformAudits(AuditActionPlatformStatusRead)); after != before {
		t.Fatalf("refused requests wrote %d audit rows", after-before)
	}
}

// Auditing: a successful read writes ONE platform-scope row (tenant_id NULL, no subject tenant), actor = the admin,
// with no response data in it; a read without a body-less POST or other method is not routed.
func TestPlatformStatus_ReadIsAuditedAsAPlatformAction(t *testing.T) {
	a := newARAPI(t, false)
	before := len(a.platformAudits(AuditActionPlatformStatusRead))
	for i := 0; i < 2; i++ {
		if res := a.do("GET", platformStatusPath, a.adminToken(), nil); res.status != http.StatusOK {
			t.Fatalf("read %d: %d", i, res.status)
		}
	}
	rows := a.platformAudits(AuditActionPlatformStatusRead)
	if len(rows) != before+2 {
		t.Fatalf("want %d audit rows, got %d", before+2, len(rows))
	}
	row := rows[len(rows)-1]
	if row["_actor"] != a.admin.String() || row["_outcome"] != "success" {
		t.Fatalf("audit row = %v", row)
	}
	var subjectRows int
	if err := a.w.Pool.WithPlatformAdmin(context.Background(), a.admin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = $1 AND (subject_tenant_id IS NOT NULL OR tenant_id IS NOT NULL OR target_type IS DISTINCT FROM 'platform_status')`,
			AuditActionPlatformStatusRead).Scan(&subjectRows)
	}); err != nil || subjectRows != 0 {
		t.Fatalf("the status audit row must have no tenant, no subject tenant and target_type platform_status (rows breaking that: %d, err %v)", subjectRows, err)
	}
	for k := range row {
		if !strings.HasPrefix(k, "_") {
			t.Fatalf("the audit metadata must be empty (no status data), found key %q", k)
		}
	}
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		if res := a.do(method, platformStatusPath, a.adminToken(), map[string]any{}); res.status != http.StatusMethodNotAllowed && res.status != http.StatusNotFound {
			t.Errorf("%s: %d, want the route not to exist for this method", method, res.status)
		}
	}
}

// Fail closed: a platform read that cannot be audited returns nothing. The failure is injected on the throwaway
// database only.
func TestPlatformStatus_FailsClosedWhenTheAuditRowCannotBeWritten(t *testing.T) {
	a := newARAPI(t, false)
	if err := a.w.Owner.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `CREATE FUNCTION ps_block_status_audit() RETURNS trigger AS $$
			BEGIN
				IF NEW.action = 'platform.status_read' THEN
					RAISE EXCEPTION 'injected audit failure';
				END IF;
				RETURN NEW;
			END $$ LANGUAGE plpgsql`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `CREATE TRIGGER ps_block_status_audit BEFORE INSERT ON audit_log FOR EACH ROW EXECUTE FUNCTION ps_block_status_audit()`)
		return err
	}); err != nil {
		t.Skipf("cannot inject an audit failure on this scratch database: %v", err)
	}
	res := a.do("GET", platformStatusPath, a.adminToken(), nil)
	if res.status != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500 when the audit row cannot be written", res.status)
	}
	for _, leak := range []string{"head_version", "commit", "anchor", "injected"} {
		if strings.Contains(string(res.body), leak) {
			t.Fatalf("the failed read returned %q: %s", leak, res.body)
		}
	}
}
