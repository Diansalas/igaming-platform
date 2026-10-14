//go:build integration

package httpserver

// Ledger-finance review LF-3: the TENANT-scope K1 revoke (revoked_by_scope 'tenant', migration 0112; the handler runs
// in db.WithPrincipalScope) is re-asserted the same way as the platform path, including a ROLE change of the revoker
// (the token's role no longer matches the row), in both orders. Uses the k1World of adr0119_k1_reassert_*.

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
)

// changeTenant runs one statement on a tenant staff row (a bare tenant session: no platform GUC).
func (w *k1World) changeTenant(sql string, id uuid.UUID, lockTimeout string) error {
	return w.a.pool.WithTenant(context.Background(), w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		if lockTimeout != "" {
			if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, lockTimeout); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, sql, id)
		return err
	})
}

func TestK1Reassertion_Revoke_TenantScope_ChangeFirst_Refused(t *testing.T) {
	w := newK1World(t)
	for _, c := range []struct{ name, sql string }{
		{"suspended", `UPDATE staff_users SET status = 'suspended' WHERE id = $1`},
		{"staff_version_only", `UPDATE staff_users SET password_hash = 'y' WHERE id = $1`},
		{"role_changed", `UPDATE staff_users SET role = 'support' WHERE id = $1`},
	} {
		t.Run(c.name, func(t *testing.T) {
			grantID := w.grant(t)
			revoker := w.a.staff(w.tenant, "tenant_admin")
			tok := w.a.token(revoker, w.tenant, auth.RoleTenantAdmin)
			unblock := w.blockRow(t, "staff_capability_grants", grantID)
			var resp cgResp
			var wg sync.WaitGroup
			wg.Add(1)
			go func() { defer wg.Done(); resp = w.revoke(grantID, tok) }()
			w.waitForLockWaiter(t)
			if err := w.changeTenant(c.sql, revoker, ""); err != nil {
				unblock()
				t.Fatalf("change: %v", err)
			}
			unblock()
			wg.Wait()
			wantSessionRevoked(t, "tenant revoke after the revoker changed", resp)
			if n := w.count(t, `SELECT count(*) FROM staff_capability_grants WHERE id = $1 AND revoked_at IS NOT NULL`, grantID); n != 0 {
				t.Fatal("a refused revocation revoked the grant")
			}
		})
	}
	// Control: an unchanged tenant admin revokes (the refusals above are the re-assertion, not a scope refusal).
	grantID := w.grant(t)
	revoker := w.a.staff(w.tenant, "tenant_admin")
	if r := w.revoke(grantID, w.a.token(revoker, w.tenant, auth.RoleTenantAdmin)); r.status != http.StatusNoContent {
		t.Fatalf("tenant-scope revoke (control): %d %s", r.status, r.body)
	}
	if n := w.count(t, `SELECT count(*) FROM staff_capability_grants WHERE id = $1 AND revoked_at IS NOT NULL AND revoked_by_scope = 'tenant'`, grantID); n != 1 {
		t.Fatal("the control revocation must be tenant-scoped")
	}
}

func TestK1Reassertion_Revoke_TenantScope_RevokeFirst_RoleChangeWaits(t *testing.T) {
	w := newK1World(t)
	grantID := w.grant(t)
	revoker := w.a.staff(w.tenant, "tenant_admin")
	tok := w.a.token(revoker, w.tenant, auth.RoleTenantAdmin)
	release := w.pause(t)
	defer release()
	var resp cgResp
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); resp = w.revoke(grantID, tok) }()
	w.waitForLockWaiter(t)
	waitErr := w.changeTenant(`UPDATE staff_users SET role = 'support' WHERE id = $1`, revoker, "300ms")
	release()
	wg.Wait()
	if pgCode(waitErr) != "55P03" {
		t.Fatalf("the role change must wait on the revocation's FOR SHARE (55P03), got %v", waitErr)
	}
	if resp.status != http.StatusNoContent {
		t.Fatalf("the revocation holding the row first must stand: %d %s", resp.status, resp.body)
	}
	if err := w.changeTenant(`UPDATE staff_users SET role = 'support' WHERE id = $1`, revoker, "2s"); err != nil {
		t.Fatalf("the role change after the revocation commits: %v", err)
	}
}
