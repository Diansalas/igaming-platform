//go:build integration

package httpserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
)

func testEnv(t *testing.T) (*db.Pool, *auth.Issuer) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	pool, err := db.Connect(context.Background(), url, 5, 5*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, auth.NewIssuer("test-signing-secret-at-least-32-characters", "platform-api-test")
}

func seedTenantWithConfig(t *testing.T, pool *db.Pool, displayName string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	ctx := context.Background()
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO tenants (id, name, slug, licensing_model) VALUES ($1, $2, $3, 'under_platform_licence')`,
			id, "HTTP Test Tenant "+id.String(), "http-test-"+id.String(),
		)
		return err
	}); err != nil {
		t.Fatalf("failed to create tenant: %v", err)
	}
	t.Cleanup(func() {
		_ = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, id)
			return err
		})
	})

	if err := pool.WithTenant(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenant_config (tenant_id, display_name) VALUES ($1, $2)`, id, displayName)
		return err
	}); err != nil {
		t.Fatalf("failed to seed tenant_config: %v", err)
	}
	return id
}

func TestHealthz_DoesNotRequireAuth(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := httptest.NewServer(New(Deps{
		Logger:      slog.New(slog.NewTextHandler(os.Stderr, nil)),
		DB:          pool,
		AuthIssuer:  issuer,
		ServiceName: "platform-api-test",
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestReadyz_ReflectsDatabaseHealth(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := httptest.NewServer(New(Deps{
		Logger:      slog.New(slog.NewTextHandler(os.Stderr, nil)),
		DB:          pool,
		AuthIssuer:  issuer,
		ServiceName: "platform-api-test",
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 with a healthy database, got %d", resp.StatusCode)
	}
}

func TestTenantConfigEndpoint_RequiresAuth(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := httptest.NewServer(New(Deps{
		Logger:      slog.New(slog.NewTextHandler(os.Stderr, nil)),
		DB:          pool,
		AuthIssuer:  issuer,
		ServiceName: "platform-api-test",
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/tenant-config")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 with no token, got %d", resp.StatusCode)
	}
}

// TestTenantConfigEndpoint_EnforcesIsolationEndToEnd is the full-stack
// version of the RLS proof in internal/db: a valid token for tenant A
// must never be able to see tenant B's configuration over HTTP, even
// though both tenants' rows exist in the same table.
func TestTenantConfigEndpoint_EnforcesIsolationEndToEnd(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := httptest.NewServer(New(Deps{
		Logger:      slog.New(slog.NewTextHandler(os.Stderr, nil)),
		DB:          pool,
		AuthIssuer:  issuer,
		ServiceName: "platform-api-test",
	}))
	defer srv.Close()

	tenantA := seedTenantWithConfig(t, pool, "Tenant A Brand")
	_ = seedTenantWithConfig(t, pool, "Tenant B Brand")

	tokenA, err := issuer.Issue("player-a", tenantA, auth.RolePlayer, time.Hour)
	if err != nil {
		t.Fatalf("failed to issue token: %v", err)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/tenant-config", nil)
	req.Header.Set("Authorization", "Bearer "+tokenA)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var body struct {
		TenantID    string `json:"tenant_id"`
		DisplayName string `json:"display_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if body.TenantID != tenantA.String() {
		t.Errorf("expected tenant id %s, got %s", tenantA, body.TenantID)
	}
	if body.DisplayName != "Tenant A Brand" {
		t.Errorf("expected tenant A's own display name, got %q (leaked cross-tenant data if this is Tenant B's)", body.DisplayName)
	}
}
