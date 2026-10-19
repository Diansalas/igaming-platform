//go:build integration

package httpserver

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

// G4 (licensing enforcement slice 3): a launch (demo included) on a suspended
// brand or tenant is refused with 409 TENANT_OR_BRAND_NOT_ACTIVE, creates no
// launch session and no wallet, and the denial is audited.
func TestCasinoLaunch_HTTP_SuspendedBrandOrTenantMintsNothing(t *testing.T) {
	for _, scope := range []string{"brand", "tenant"} {
		t.Run(scope, func(t *testing.T) {
			k := newKYCWorld(t)
			k.addKYC(t, "approved", "MT", 3*time.Hour)
			_, _, s0, wl0 := k.counts(t)
			if scope == "brand" {
				if err := launchfix.TrySetBrandStatus(context.Background(), k.tenant.ID, k.brand.ID, "suspended"); err != nil {
					t.Fatal(err)
				}
			} else if err := launchfix.TrySetTenantStatus(context.Background(), k.tenant.ID, "suspended"); err != nil {
				t.Fatal(err)
			}
			resp := k.launch(t)
			status, body := refusalBody(t, resp)
			if status != http.StatusConflict {
				t.Fatalf("want 409, got %d (%s)", status, body)
			}
			if _, _, s1, wl1 := k.counts(t); s1 != s0 || wl1 != wl0 {
				t.Fatalf("a refused launch created rows: sessions %d->%d wallets %d->%d", s0, s1, wl0, wl1)
			}
			if n := countTenantRows(t, k.pool, k.tenant.ID, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'casino.launch_denied_by_status'`, k.tenant.ID); n != 1 {
				t.Fatalf("want 1 status-denial audit row, got %d", n)
			}
		})
	}
}
