//go:build integration

package httpserver

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/testsupport/rgfix"
)

// SR6R-1: the activation guard must check the jurisdiction ENFORCEMENT reads (tenants.licence_id -> licences.jurisdiction_id),
// not only the market configuration.
func TestRGEnforcement_HTTP_ActivationGuard_UsesTheLicenceJurisdiction(t *testing.T) {
	w := newRGW(t, rgWOpt{})
	f := w.fixture()
	body := map[string]any{"reason_code": "enable_rg_limits", "attestation_ref": "compliance officer, test"}

	// Mismatch: tenant A is LICENSED in f.jur (no floors); its configuration names J2, which HAS floors for EUR.
	j2 := "RGS2J2-" + strings.ToUpper(uuid.NewString()[:8])
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO jurisdictions (code, name) VALUES ($1, 'rgs2 market jurisdiction')`, j2)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"deposit", "loss", "wager"} {
		w.floor(j2, kind, rgfix.Spec{"attestation_ref": "sr6r-1-test", "asset_code": "EUR"})
	}
	w.tenantConfig(f.tenantA, j2)
	compA := w.staff(f.tenantA, f.compA, auth.RoleCompliance)
	r := w.call("POST", "/v1/admin/rg/enforcement", compA, "", body)
	r.want(t, http.StatusConflict, "conflict")
	if !strings.Contains(string(r.body), "floor_missing_deposit") {
		t.Fatalf("a licence jurisdiction without floors must refuse activation although the configured jurisdiction has them: %s", r.body)
	}
	if m := w.call("GET", "/v1/admin/rg/enforcement", compA, "", nil).obj(t); m["enforcement_enabled"] != false {
		t.Fatalf("a refused activation must write nothing: %v", m)
	}
	// Once the licence jurisdiction has floors too, activation passes.
	for _, kind := range []string{"deposit", "loss", "wager"} {
		w.floor(f.jur, kind, rgfix.Spec{"attestation_ref": "sr6r-1-test", "asset_code": "EUR"})
	}
	w.call("POST", "/v1/admin/rg/enforcement", compA, "", body).want(t, http.StatusCreated, "")

	// No licence: tenant B has a configuration and floors but NO licence jurisdiction: every action would be rg_config_missing.
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE tenants SET licence_id = NULL WHERE id = $1`, f.tenantB)
		return err
	}); err != nil {
		t.Fatalf("clear the licence of tenant B: %v", err)
	}
	w.tenantConfig(f.tenantB, f.jur)
	compB := w.staff(f.tenantB, f.compB, auth.RoleCompliance)
	r = w.call("POST", "/v1/admin/rg/enforcement", compB, "", body)
	r.want(t, http.StatusConflict, "conflict")
	if !strings.Contains(string(r.body), "no_licence_jurisdiction") {
		t.Fatalf("a tenant with no licence jurisdiction must be refused: %s", r.body)
	}
}
