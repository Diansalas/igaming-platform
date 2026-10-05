//go:build integration

package alertingtest

import (
	"context"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

// World is a throwaway, fully migrated database plus a pool that connects as
// the REAL RUNTIME ROLE (TEST_RUNTIME_DATABASE_URL's user), the role the
// application uses in production, so RLS, FORCE RLS and grants are what the
// application actually experiences. NewWorld asserts that role is neither
// superuser nor BYPASSRLS; a test that silently ran as a privileged role would
// pass for the wrong reason.
//
// The owner pool exists only to migrate the scratch database and to run
// owner-only probes (for example the TEMP-shadow probe in a rolled-back
// transaction). Grants given to the runtime role are confined to this
// throwaway database (the same default privileges deploy/init-app-role.sql
// gives it); no role, password or shared database is touched.
type World struct {
	T     *testing.T
	Owner *db.Pool
	Pool  *db.Pool // runtime role
	URL   string   // owner URL of the scratch database
}

// NewWorld builds a World or skips when the required env is unset.
func NewWorld(t *testing.T, prefix string) *World {
	t.Helper()
	rtBase := os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if rtBase == "" {
		t.Skip("TEST_RUNTIME_DATABASE_URL not set; skipping runtime-role integration test")
	}
	ownerURL := scratchdb.New(t, prefix)
	rtu, err := url.Parse(rtBase)
	if err != nil {
		t.Fatalf("parse TEST_RUNTIME_DATABASE_URL: %v", err)
	}
	runtimeUser := rtu.User.Username()
	ou, _ := url.Parse(ownerURL)
	rtu.Path = ou.Path

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, ownerURL)
	if err != nil {
		t.Fatalf("connect owner: %v", err)
	}
	owner := ou.User.Username()
	for _, stmt := range []string{
		"GRANT CONNECT ON DATABASE " + pgx.Identifier{ou.Path[1:]}.Sanitize() + " TO " + pgx.Identifier{runtimeUser}.Sanitize(),
		"GRANT USAGE ON SCHEMA public TO " + pgx.Identifier{runtimeUser}.Sanitize(),
		"ALTER DEFAULT PRIVILEGES FOR ROLE " + pgx.Identifier{owner}.Sanitize() + " IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO " + pgx.Identifier{runtimeUser}.Sanitize(),
		"ALTER DEFAULT PRIVILEGES FOR ROLE " + pgx.Identifier{owner}.Sanitize() + " IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO " + pgx.Identifier{runtimeUser}.Sanitize(),
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			_ = conn.Close(ctx)
			t.Fatalf("scratch grant %q: %v", stmt, err)
		}
	}
	_ = conn.Close(ctx)

	ownerPool, err := db.Connect(ctx, ownerURL, 10, 5*time.Second)
	if err != nil {
		t.Fatalf("connect owner pool: %v", err)
	}
	t.Cleanup(ownerPool.Close)
	if _, err := ownerPool.MigrateUp(ctx, "../../migrations"); err != nil {
		t.Fatalf("migrate scratch: %v", err)
	}
	rt, err := db.Connect(ctx, rtu.String(), 10, 5*time.Second)
	if err != nil {
		t.Fatalf("connect runtime pool: %v", err)
	}
	t.Cleanup(rt.Close)
	w := &World{T: t, Owner: ownerPool, Pool: rt, URL: ownerURL}
	w.AssertUnprivileged()
	return w
}

// AssertUnprivileged fails unless the runtime pool's role is neither
// superuser nor BYPASSRLS.
func (w *World) AssertUnprivileged() {
	w.T.Helper()
	var super, bypass bool
	if err := w.Pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass)
	}); err != nil {
		w.T.Fatalf("inspect runtime role: %v", err)
	}
	if super || bypass {
		w.T.Fatalf("runtime pool role has rolsuper=%v rolbypassrls=%v; RLS assertions would be meaningless", super, bypass)
	}
}

// SeedAdmin creates a platform-scoped staff user.
func (w *World) SeedAdmin() uuid.UUID {
	w.T.Helper()
	id := uuid.New()
	if err := w.Pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role) VALUES ($1, NULL, $2, 'x', 'platform_admin')`,
			id, "w-admin-"+id.String()+"@platform.test")
		return err
	}); err != nil {
		w.T.Fatalf("seed admin: %v", err)
	}
	return id
}

// SeedTenant creates a tenant.
func (w *World) SeedTenant(admin uuid.UUID) uuid.UUID {
	w.T.Helper()
	id := uuid.New()
	if err := w.Pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, name, slug, licensing_model) VALUES ($1, $2, $3, 'own_licence')`,
			id, "w-"+id.String()[:8], "w-"+id.String()[:8])
		return err
	}); err != nil {
		w.T.Fatalf("seed tenant: %v", err)
	}
	return id
}

// SeedAlert inserts one open platform-owned alert whose subject is tenant.
func (w *World) SeedAlert(tenant uuid.UUID, kind alerting.Kind, discriminator string) uuid.UUID {
	w.T.Helper()
	var id uuid.UUID
	if err := w.Pool.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO alerts (tenant_id, subject_tenant_id, kind, discriminator, attributes) VALUES (NULL, $1, $2, $3, '{}'::jsonb) RETURNING id`,
			tenant, string(kind), discriminator).Scan(&id)
	}); err != nil {
		w.T.Fatalf("seed alert: %v", err)
	}
	return id
}

// AddRoute inserts one route version as the platform admin.
func (w *World) AddRoute(admin uuid.UUID, sev alerting.Severity, step int, kind alerting.ChannelKind, recipient string, enabled bool) uuid.UUID {
	w.T.Helper()
	var id uuid.UUID
	var rec *string
	if recipient != "" {
		rec = &recipient
	}
	if err := w.Pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO alert_routes (scope, severity, escalation_step, channel_kind, recipient_ref, enabled, reason_code)
			VALUES ('platform', $1, $2, $3, $4, $5, 'initial_setup') RETURNING id`,
			string(sev), step, string(kind), rec, enabled).Scan(&id)
	}); err != nil {
		w.T.Fatalf("add route: %v", err)
	}
	return id
}

// LatestEvent returns the latest delivery event of an alert ("" if none).
func (w *World) LatestEvent(admin, alertID uuid.UUID) (event, unroutedReason string) {
	w.T.Helper()
	var reason *string
	err := w.Pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT event, unrouted_reason FROM alert_deliveries WHERE alert_id = $1 ORDER BY recorded_at DESC, id DESC LIMIT 1`, alertID).Scan(&event, &reason)
	})
	if err == pgx.ErrNoRows {
		return "", ""
	}
	if err != nil {
		w.T.Fatalf("latest event: %v", err)
	}
	if reason != nil {
		unroutedReason = *reason
	}
	return event, unroutedReason
}

// Events returns every delivery event of an alert in order as "step:attempt:event".
func (w *World) Events(admin, alertID uuid.UUID) []string {
	w.T.Helper()
	var out []string
	if err := w.Pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT escalation_step::text || ':' || attempt_no::text || ':' || event FROM alert_deliveries WHERE alert_id = $1 ORDER BY recorded_at, id`, alertID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return err
			}
			out = append(out, s)
		}
		return rows.Err()
	}); err != nil {
		w.T.Fatalf("events: %v", err)
	}
	return out
}
