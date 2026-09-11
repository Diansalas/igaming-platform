//go:build integration

package httpserver

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
)

const testJWTSecret = "test-signing-secret-at-least-32-characters"

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

	keys, err := auth.NewKeyRegistry("k1", map[string]string{"k1": testJWTSecret})
	if err != nil {
		t.Fatalf("failed to build key registry: %v", err)
	}
	issuer := auth.NewIssuer(keys, "platform-api-test", "platform-api-test")
	return pool, issuer
}

func newTestServer(t *testing.T, pool *db.Pool, issuer *auth.Issuer) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(New(Deps{
		Logger:          slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		DB:              pool,
		AuthIssuer:      issuer,
		ServiceName:     "platform-api-test",
		AccessTokenTTL:  5 * time.Minute,
		RefreshTokenTTL: time.Hour,
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestHealthz_DoesNotRequireAuth(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)

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
	srv := newTestServer(t, pool, issuer)

	resp, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 with a healthy database, got %d", resp.StatusCode)
	}
}
