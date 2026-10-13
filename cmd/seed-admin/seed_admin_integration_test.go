//go:build integration

// ADR 0119 S3: cmd/seed-admin as the deployment job of section 11 - it refuses to run without the staff MFA keys
// (staffmfa.LoadKeys, before touching the database), creates a platform_admin and prints a single-use bootstrap
// enrolment token exactly once (stored only as a keyed MAC), re-issues one for an existing admin WITHOUT an active
// factor (the S3 cutover re-seed / break-glass path), and refuses to re-issue for an admin that HAS one (an MFA reset
// is S6). Runs the real compiled binary against TEST_DATABASE_URL (table owner).
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/staffmfa"
	"github.com/Diansalas/igaming-platform/internal/staffmfa/store"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

func buildSeedAdmin(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "seed-admin")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

func runSeedAdmin(t *testing.T, bin string, env []string, args ...string) (string, string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env
	var o, e strings.Builder
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return o.String(), e.String(), code
}

// seedTenantStaff provisions a tenant (as a seeded platform operator) and an active tenant_admin; returns the slug
// and the email.
func seedTenantStaff(t *testing.T, pool *db.Pool) (string, string) {
	t.Helper()
	launchfix.SeedOperators(t)
	var tn identity.Tenant
	if err := pool.WithPlatformAdmin(context.Background(), launchfix.Operators().Requester, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		tn, err = launchfix.CreateTenant(ctx, tx, "Seed", "seed-"+uuid.NewString()[:12], "under_platform_licence")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	email := "seed-ta-" + uuid.NewString() + "@fixture.invalid"
	if err := pool.WithTenant(context.Background(), tn.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (tenant_id, email, password_hash, role) VALUES ($1, $2, 'x', 'tenant_admin')`, tn.ID, email)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return tn.Slug, email
}

func randKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// readEnvExampleKeys returns the STAFF_MFA_* key lines published in the repository's .env.example.
func readEnvExampleKeys(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "#"))
		for _, k := range []string{"STAFF_MFA_FACTOR_KEYS=", "STAFF_MFA_FACTOR_ACTIVE_KID=", "STAFF_MFA_MAC_KEYS=", "STAFF_MFA_MAC_ACTIVE_KID="} {
			if strings.HasPrefix(line, k) {
				out = append(out, line)
			}
		}
	}
	if len(out) != 4 {
		t.Fatalf(".env.example publishes %d STAFF_MFA_* key lines, want 4", len(out))
	}
	return out
}

var tokenLine = regexp.MustCompile(`(?m)^([A-Za-z0-9_-]{64})$`)

func TestSeedAdmin_BootstrapToken_Subprocess(t *testing.T) {
	ownerURL := os.Getenv("TEST_DATABASE_URL")
	if ownerURL == "" {
		t.Fatal("TEST_DATABASE_URL must be set")
	}
	bin := buildSeedAdmin(t)
	fk, mk := randKey(t), randKey(t)
	keysEnv := []string{"STAFF_MFA_FACTOR_KEYS=f1:" + fk, "STAFF_MFA_FACTOR_ACTIVE_KID=f1", "STAFF_MFA_MAC_KEYS=m1:" + mk, "STAFF_MFA_MAC_ACTIVE_KID=m1"}
	keys, err := staffmfa.LoadKeys("f1", "f1:"+fk, "m1", "m1:"+mk)
	if err != nil {
		t.Fatal(err)
	}
	email := "seed-" + uuid.NewString() + "@fixture.invalid"
	base := []string{"PATH=" + os.Getenv("PATH"), "DATABASE_URL=" + ownerURL, "SEED_ADMIN_PASSWORD=a-seed-admin-password-1"}

	// 1. Without keys: refused before the database (the account is not created).
	_, stderr, code := runSeedAdmin(t, bin, base, "-email="+email)
	if code == 0 || !strings.Contains(stderr, "staff MFA keys") {
		t.Fatalf("seed-admin without keys must refuse: %d %s", code, stderr)
	}
	pool, err := db.Connect(context.Background(), ownerURL, 2, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	staffByEmail := func() (identity.StaffUser, error) {
		var s identity.StaffUser
		err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			var err error
			s, err = identity.GetStaffUserByEmail(ctx, tx, email)
			return err
		})
		return s, err
	}
	if _, err := staffByEmail(); err == nil {
		t.Fatal("a refused run must not create the account")
	}
	// 1b. (security review C2) The published .env.example keys are refused exactly as platform-api's config.Load
	// refuses them: APP_ENV=production and APP_ENV unset (production by default); nothing is created or printed.
	example := readEnvExampleKeys(t)
	for _, appEnv := range [][]string{{"APP_ENV=production"}, nil} {
		stdout, stderr, code := runSeedAdmin(t, bin, append(append(append([]string{}, base...), example...), appEnv...), "-email="+email)
		if code == 0 || !strings.Contains(stderr, "placeholder") || len(tokenLine.FindAllString(stdout, -1)) != 0 {
			t.Fatalf("seed-admin with the published placeholder keys (%v) must refuse: %d %q %s", appEnv, code, stdout, stderr)
		}
		if _, err := staffByEmail(); err == nil {
			t.Fatal("a refused run must not create the account")
		}
	}
	// A staff MFA key equal to another platform key family is refused too (pairwise distinctness).
	_, stderr, code = runSeedAdmin(t, bin, append(append(append([]string{}, base...), keysEnv...), "JWT_SIGNING_SECRET="+mk), "-email="+email)
	if code == 0 || !strings.Contains(stderr, "JWT_SIGNING_SECRET") {
		t.Fatalf("a staff MFA key shared with JWT_SIGNING_SECRET must be refused: %d %s", code, stderr)
	}

	// 2. Create: a platform_admin and ONE printed bootstrap token that resolves (keyed MAC) to it.
	stdout, stderr, code := runSeedAdmin(t, bin, append(append([]string{}, base...), keysEnv...), "-email="+email, "-create-person")
	if code != 0 {
		t.Fatalf("seed-admin create: %d %s", code, stderr)
	}
	toks := tokenLine.FindAllString(stdout, -1)
	if len(toks) != 1 || strings.Contains(stderr, toks[0]) {
		t.Fatalf("exactly one token on stdout, none on stderr: %q", stdout)
	}
	staff, err := staffByEmail()
	if err != nil || staff.Role != identity.StaffRolePlatformAdmin {
		t.Fatalf("account: %+v %v", staff, err)
	}
	st, err := store.New(keys)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := st.ResolveEnrolmentToken(context.Background(), pool, []store.EnrolmentPurpose{store.EnrolBootstrap}, toks[0])
	if err != nil || ref.Staff.StaffID != staff.ID || ref.Purpose != store.EnrolBootstrap {
		t.Fatalf("the printed token must resolve to the new admin's bootstrap token: %+v %v", ref, err)
	}
	var audits int
	_ = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE target_id = $1 AND action IN ('staff.platform_admin_bootstrapped', 'staff.bootstrap_token_issued')
			AND metadata::text NOT LIKE '%'||$2||'%'`, staff.ID.String(), toks[0]).Scan(&audits)
	})
	if audits != 2 {
		t.Fatalf("creation and token issue must be audited (without the token): %d", audits)
	}

	// 3. -issue-bootstrap for the same admin (no factor yet): a fresh token; the first one is revoked. The break-glass
	// path requires -operator and -reason (security review R4) and records both.
	for _, missing := range [][]string{{"-reason=INC-1"}, {"-operator=ops-alice"}, {"-operator= ops", "-reason=INC-1"}} {
		stdout, stderr, code = runSeedAdmin(t, bin, append([]string{"PATH=" + os.Getenv("PATH"), "DATABASE_URL=" + ownerURL}, keysEnv...),
			append([]string{"-email=" + email, "-issue-bootstrap"}, missing...)...)
		if code == 0 || !strings.Contains(stderr, "-operator") && !strings.Contains(stderr, "-reason") || len(tokenLine.FindAllString(stdout, -1)) != 0 {
			t.Fatalf("-issue-bootstrap %v must refuse: %d %q %s", missing, code, stdout, stderr)
		}
	}
	stdout, stderr, code = runSeedAdmin(t, bin, append([]string{"PATH=" + os.Getenv("PATH"), "DATABASE_URL=" + ownerURL}, keysEnv...), "-email="+email, "-issue-bootstrap", "-operator=ops-alice", "-reason=INC-1 lost device")
	if code != 0 {
		t.Fatalf("-issue-bootstrap: %d %s", code, stderr)
	}
	toks2 := tokenLine.FindAllString(stdout, -1)
	if len(toks2) != 1 || toks2[0] == toks[0] {
		t.Fatalf("-issue-bootstrap must print one new token: %q", stdout)
	}
	if _, err := st.ResolveEnrolmentToken(context.Background(), pool, []store.EnrolmentPurpose{store.EnrolBootstrap}, toks2[0]); err != nil {
		t.Fatalf("the re-issued token must resolve: %v", err)
	}
	var live int
	_ = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM staff_enrolment_tokens WHERE staff_user_id = $1 AND consumed_at IS NULL AND revoked_at IS NULL`, staff.ID).Scan(&live)
	})
	if live != 1 {
		t.Fatalf("exactly one live bootstrap token after a re-issue, got %d", live)
	}
	var who, why string
	_ = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT metadata->>'operator', metadata->>'reason' FROM audit_log WHERE target_id = $1
			AND action = 'staff.bootstrap_token_issued' AND metadata->>'mode' = 'reissue' AND metadata::text NOT LIKE '%'||$2||'%'`,
			staff.ID.String(), toks2[0]).Scan(&who, &why)
	})
	if who != "ops-alice" || why != "INC-1 lost device" {
		t.Fatalf("the re-issue audit row must record the operator and the reason (and never the token): %q %q", who, why)
	}

	// 3b. A TENANT staff member (the S3 re-seed: no invite / reset path before S6): -issue-bootstrap -tenant-slug.
	tenantSlug, tenantStaffEmail := seedTenantStaff(t, pool)
	stdout, stderr, code = runSeedAdmin(t, bin, append([]string{"PATH=" + os.Getenv("PATH"), "DATABASE_URL=" + ownerURL}, keysEnv...),
		"-email="+tenantStaffEmail, "-issue-bootstrap", "-tenant-slug="+tenantSlug, "-operator=ops-alice", "-reason=INC-2")
	if code != 0 || len(tokenLine.FindAllString(stdout, -1)) != 1 {
		t.Fatalf("-issue-bootstrap for tenant staff: %d %q %s", code, stdout, stderr)
	}
	tref, err := st.ResolveEnrolmentToken(context.Background(), pool, []store.EnrolmentPurpose{store.EnrolBootstrap}, tokenLine.FindString(stdout))
	if err != nil || tref.Staff.TenantID == uuid.Nil {
		t.Fatalf("the tenant staff token must resolve to the tenant scope: %+v %v", tref, err)
	}
	// Without the slug the tenant staff member is not found in platform scope.
	_, _, code = runSeedAdmin(t, bin, append([]string{"PATH=" + os.Getenv("PATH"), "DATABASE_URL=" + ownerURL}, keysEnv...), "-email="+tenantStaffEmail, "-issue-bootstrap", "-operator=ops-alice", "-reason=INC-2")
	if code == 0 {
		t.Fatal("a tenant staff member must not be found without its tenant slug")
	}

	// 4. With an ACTIVE factor the re-issue is refused (an MFA reset is S6), and nothing is issued.
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_mfa_factors (staff_user_id, factor_type, status, secret_kid, secret_nonce, secret_ct, activated_at)
			VALUES ($1, 'totp', 'active', 'f1', '\x000000000000000000000000', '\x01', now())`, staff.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code = runSeedAdmin(t, bin, append([]string{"PATH=" + os.Getenv("PATH"), "DATABASE_URL=" + ownerURL}, keysEnv...), "-email="+email, "-issue-bootstrap", "-operator=ops-alice", "-reason=INC-3")
	if code == 0 || !strings.Contains(stderr, "already has an active second factor") || len(tokenLine.FindAllString(stdout, -1)) != 0 {
		t.Fatalf("-issue-bootstrap with an active factor must refuse and print nothing: %d %q %s", code, stdout, stderr)
	}
}
