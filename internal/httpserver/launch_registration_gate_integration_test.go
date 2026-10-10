//go:build integration

// ADR 0112 section 7.3 (slice 1): player registration refuses unless the brand AND its tenant
// are 'active' (409 BRAND_NOT_ACCEPTING_REGISTRATIONS), login refuses on a pending_launch brand
// (answers like an unknown brand) and stays allowed on suspended / closed brands, and the
// registration read is a plain NON-LOCKING read (S4): a status change that is in flight and
// holds its locks does not make a registration wait.
package httpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

// mustCreatePendingBrand creates a pending_launch brand (the default) through the owner pool.
func mustCreatePendingBrand(t *testing.T, pool *db.Pool, tenantID uuid.UUID) identity.Brand {
	t.Helper()
	b := identity.Brand{ID: uuid.New(), TenantID: tenantID, Name: "Pending Brand", Slug: "pb-" + uuid.NewString(), Status: identity.StatusPendingLaunch}
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, slug, name) VALUES ($1, $2, $3, $4)`, b.ID, b.TenantID, b.Slug, b.Name)
		return err
	}); err != nil {
		t.Fatalf("create pending brand: %v", err)
	}
	return b
}

// registerStatus registers a fresh player on the brand slug and returns the HTTP status and, for
// a non-2xx answer, the decoded error envelope.
func registerStatus(t *testing.T, srv *httptest.Server, slug string) (int, apierror.Error) {
	t.Helper()
	resp := postJSON(t, srv, "/v1/auth/register", "", map[string]string{
		"brand_slug": slug, "email": "gate-" + uuid.NewString() + "@example.com", "password": "a-decent-password-1",
	})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp.StatusCode, apierror.Error{}
	}
	return resp.StatusCode, decodeAPIError(t, resp)
}

func countPlayers(t *testing.T, pool *db.Pool, tenantID, brandID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM player_accounts WHERE brand_id = $1`, brandID).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestLaunchGate_Registration_RefusedUnlessBrandAndTenantActive(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)

	tenant := mustCreateTenant(t, pool)
	active := mustCreateBrand(t, pool, tenant)
	if code, _ := registerStatus(t, srv, active.Slug); code != http.StatusCreated {
		t.Fatalf("control: an active brand of an active tenant must accept registration, got %d", code)
	}

	// pending_launch brand of an ACTIVE tenant: security S-5, answers EXACTLY like an unknown brand.
	pending := mustCreatePendingBrand(t, pool, tenant.ID)
	pCode, pErr := registerStatus(t, srv, pending.Slug)
	uCode, uErr := registerStatus(t, srv, "no-such-"+uuid.NewString())
	if pCode != http.StatusNotFound || uCode != http.StatusNotFound || pErr.Code != uErr.Code || pErr.Message != uErr.Message {
		t.Fatalf("a pending_launch brand must be indistinguishable from an unknown one: %d %+v vs %d %+v", pCode, pErr, uCode, uErr)
	}
	if n := countPlayers(t, pool, tenant.ID, pending.ID); n != 0 {
		t.Fatalf("a refused registration created %d player rows", n)
	}
	// suspended and closed brands keep the explicit 409.
	suspended := mustCreateBrand(t, pool, tenant)
	launchfix.SetBrandStatus(t, tenant.ID, suspended.ID, "suspended")
	closed := mustCreateBrand(t, pool, tenant)
	launchfix.SetBrandStatus(t, tenant.ID, closed.ID, "closed")
	for name, b := range map[string]identity.Brand{"suspended": suspended, "closed": closed} {
		code, e := registerStatus(t, srv, b.Slug)
		if code != http.StatusConflict || e.Code != apierror.CodeBrandNotAcceptingRegistrations {
			t.Fatalf("%s brand: want 409 %s, got %d %+v", name, apierror.CodeBrandNotAcceptingRegistrations, code, e)
		}
		if n := countPlayers(t, pool, tenant.ID, b.ID); n != 0 {
			t.Fatalf("%s brand: a refused registration created %d player rows", name, n)
		}
	}
	// Other brands of the same tenant are unaffected.
	if code, _ := registerStatus(t, srv, active.Slug); code != http.StatusCreated {
		t.Fatalf("another brand of the tenant must stay open, got %d", code)
	}

	// A suspended TENANT with an active brand, and a pending_launch tenant with an active brand.
	susTenant := mustCreateTenant(t, pool)
	susTenantBrand := mustCreateBrand(t, pool, susTenant)
	launchfix.SetTenantStatus(t, susTenant.ID, "suspended")
	if code, e := registerStatus(t, srv, susTenantBrand.Slug); code != http.StatusConflict || e.Code != apierror.CodeBrandNotAcceptingRegistrations {
		t.Fatalf("suspended tenant: want 409, got %d %+v", code, e)
	}
	pendTenantID := uuid.New()
	if err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, name, slug, licensing_model) VALUES ($1, 'pending tenant', $2, 'under_platform_licence')`, pendTenantID, "pt-"+pendTenantID.String())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	pendTenantBrand := identity.Brand{ID: uuid.New(), TenantID: pendTenantID, Slug: "ptb-" + uuid.NewString()}
	if err := pool.WithTenant(context.Background(), pendTenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, slug, name, status) VALUES ($1, $2, $3, 'b', 'active')`, pendTenantBrand.ID, pendTenantID, pendTenantBrand.Slug)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// security S-5/S-6: a pending_launch TENANT hides its (active) brand the same way, for register and login.
	if code, e := registerStatus(t, srv, pendTenantBrand.Slug); code != http.StatusNotFound || e.Code != apierror.CodeNotFound {
		t.Fatalf("pending_launch tenant: want 404 like an unknown brand, got %d %+v", code, e)
	}
	if resp := postJSON(t, srv, "/v1/auth/login", "", map[string]string{"brand_slug": pendTenantBrand.Slug, "email": "x@example.com", "password": "a-decent-password-1"}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("login on a brand of a pending_launch tenant: want 404, got %d", resp.StatusCode)
	} else {
		_ = resp.Body.Close()
	}
}

func TestLaunchGate_Login_PendingBrandLooksUnknown_SuspendedAndClosedStayAllowed(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	b := mustCreateBrand(t, pool, tenant)
	p := mustRegisterPlayer(t, srv, b.Slug)

	login := func(slug string) int {
		resp := postJSON(t, srv, "/v1/auth/login", "", map[string]string{"brand_slug": slug, "email": p.Email, "password": "a-decent-password-1"})
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}
	if c := login(b.Slug); c != http.StatusOK {
		t.Fatalf("control login: %d", c)
	}
	// Suspended and closed: players can still log in to see their balances (ADR 0112 7.3).
	launchfix.SetBrandStatus(t, tenant.ID, b.ID, "suspended")
	if c := login(b.Slug); c != http.StatusOK {
		t.Fatalf("login on a suspended brand must stay allowed, got %d", c)
	}
	launchfix.SetBrandStatus(t, tenant.ID, b.ID, "closed")
	if c := login(b.Slug); c != http.StatusOK {
		t.Fatalf("login on a closed brand must stay allowed, got %d", c)
	}
	// pending_launch: answers exactly like an unknown brand (404).
	pending := mustCreatePendingBrand(t, pool, tenant.ID)
	pendingResp := postJSON(t, srv, "/v1/auth/login", "", map[string]string{"brand_slug": pending.Slug, "email": p.Email, "password": "a-decent-password-1"})
	unknownResp := postJSON(t, srv, "/v1/auth/login", "", map[string]string{"brand_slug": "no-such-" + uuid.NewString(), "email": p.Email, "password": "a-decent-password-1"})
	defer func() { _ = pendingResp.Body.Close() }()
	defer func() { _ = unknownResp.Body.Close() }()
	if pendingResp.StatusCode != http.StatusNotFound || unknownResp.StatusCode != http.StatusNotFound {
		t.Fatalf("pending brand login = %d, unknown brand login = %d, want both 404", pendingResp.StatusCode, unknownResp.StatusCode)
	}
	pe, ue := decodeAPIError(t, pendingResp), decodeAPIError(t, unknownResp)
	if pe.Code != ue.Code || pe.Message != ue.Message {
		t.Fatalf("a pending_launch brand must be indistinguishable from an unknown one: %+v vs %+v", pe, ue)
	}
}

// S4: the registration gate takes no lock. A brand status change that is in flight (its UPDATE
// has run and holds the row lock; the transaction is not committed) does not delay a
// registration, which reads the last committed status.
func TestLaunchGate_Registration_ReadIsNonLocking_S4(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	b := mustCreateBrand(t, pool, tenant)

	held, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- launchfix.TrySetBrandStatusHolding(context.Background(), tenant.ID, b.ID, "suspended", func() {
			close(held)
			<-release
		})
	}()
	select {
	case <-held:
	case err := <-done:
		t.Fatalf("status change ended before the hold: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("status change never reached the hold")
	}
	finished := make(chan int, 1)
	go func() {
		code, _ := registerStatus(t, srv, b.Slug)
		finished <- code
	}()
	select {
	case code := <-finished:
		if code != http.StatusCreated {
			t.Fatalf("registration during an uncommitted suspension reads the committed (active) status, got %d", code)
		}
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("registration blocked behind an in-flight status change: the gate must be a plain non-locking read (S4)")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("status change: %v", err)
	}
	if code, _ := registerStatus(t, srv, b.Slug); code != http.StatusConflict {
		t.Fatalf("after the suspension commits registration must be refused, got %d", code)
	}
}
