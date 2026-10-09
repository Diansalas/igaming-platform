//go:build integration

package httpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
)

// B13-B usability: the staff withdrawal DETAIL exposes the bound instrument's id, rail and display_mask (and
// nothing else), tenant-scoped, under the existing PermWithdrawalReview; null for a legacy NULL-binding row; the
// player view is unchanged.

func rawBody(t *testing.T, resp *http.Response) (int, string) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

func TestAdminGetWithdrawal_ExposesOnlyTheBoundInstrumentIDRailAndMask(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	finance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleFinance, "finance-inst-pw-1")
	token := mustLoginStaff(t, srv, tenant.Slug, finance.Email, "finance-inst-pw-1")
	player := mustRegisterPlayer(t, srv, brand.Slug)
	walletID := fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 50000).ID
	wr := mustCreateWithdrawalRequest(t, pool, tenant.ID, brand.ID, player.ID, walletID, "EUR", 1500)

	var instID uuid.UUID
	var rail, mask, kind string
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT i.id, i.rail, i.display_mask, i.kind FROM withdrawal_requests w
			JOIN payout_instruments i ON i.id = w.payout_instrument_id WHERE w.id = $1`, wr.ID).Scan(&instID, &rail, &mask, &kind)
	}); err != nil {
		t.Fatal(err)
	}

	status, body := rawBody(t, getJSON(t, srv, "/v1/admin/withdrawals/"+wr.ID.String(), token.AccessToken))
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &top); err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal(top["payout_instrument"], &got); err != nil {
		t.Fatalf("payout_instrument: %v (%s)", err, body)
	}
	if len(got) != 3 || got["id"] != instID.String() || got["rail"] != rail || got["display_mask"] != mask || rail != "bank_transfer" {
		t.Fatalf("payout_instrument = %v, want exactly id/rail/display_mask of %s", got, instID)
	}
	// Nothing secret or PII-ish anywhere in the response.
	lower := strings.ToLower(body)
	for _, leak := range []string{"fingerprint", "ciphertext", "seal", "detail", "label", "\"kind\"", "state_changed", "verification", "synthetic_test", "payout_instrument_fingerprint"} {
		if strings.Contains(lower, leak) {
			t.Fatalf("the staff detail leaks %q: %s", leak, body)
		}
	}
	// A pure read: it did not change the request.
	if n := countRows(t, pool, tenant.ID, `SELECT count(*) FROM withdrawal_requests WHERE id = $1 AND state = 'requested'`, wr.ID); n != 1 {
		t.Fatal("the detail view must not change the withdrawal")
	}

	// The history list is unchanged (no instrument key).
	_, list := rawBody(t, getJSON(t, srv, "/v1/admin/withdrawals/history", token.AccessToken))
	if strings.Contains(list, "payout_instrument") {
		t.Fatalf("the list view must not carry the instrument: %s", list)
	}
	// The player view is unchanged.
	status, mine := rawBody(t, getJSON(t, srv, "/v1/me/withdrawals/"+wr.ID.String(), player.Tokens.AccessToken))
	if status != http.StatusOK || strings.Contains(strings.ToLower(mine), "payout_instrument") || strings.Contains(mine, instID.String()) || strings.Contains(mine, mask) {
		t.Fatalf("the player view must be unchanged (status %d): %s", status, mine)
	}
}

func TestAdminGetWithdrawal_InstrumentNotVisibleAcrossTenants(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)
	tenantA := mustCreateTenant(t, pool)
	tenantB := mustCreateTenant(t, pool)
	brandB := mustCreateBrand(t, pool, tenantB)
	financeA := mustCreateStaff(t, pool, tenantA.ID, identity.StaffRoleFinance, "finance-xa-pw-1")
	tokenA := mustLoginStaff(t, srv, tenantA.Slug, financeA.Email, "finance-xa-pw-1")

	var playerB uuid.UUID
	if err := pool.WithTenant(context.Background(), tenantB.ID, func(ctx context.Context, tx pgx.Tx) error {
		account, err := identity.RegisterPlayer(ctx, tx, brandB, "xtenant-inst@example.com", "hash")
		playerB = account.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	walletB := fundWallet(t, pool, tenantB.ID, brandB.ID, playerB, "EUR", 10000)
	wrB := mustCreateWithdrawalRequest(t, pool, tenantB.ID, brandB.ID, playerB, walletB.ID, "EUR", 2500)
	var instB uuid.UUID
	var maskB string
	if err := pool.WithTenant(context.Background(), tenantB.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT i.id, i.display_mask FROM withdrawal_requests w JOIN payout_instruments i ON i.id = w.payout_instrument_id
			WHERE w.id = $1`, wrB.ID).Scan(&instB, &maskB)
	}); err != nil {
		t.Fatal(err)
	}

	status, body := rawBody(t, getJSON(t, srv, "/v1/admin/withdrawals/"+wrB.ID.String(), tokenA.AccessToken))
	if status != http.StatusNotFound {
		t.Fatalf("tenant A staff reading tenant B's withdrawal: status %d, want 404", status)
	}
	if strings.Contains(body, instB.String()) || (maskB != "" && strings.Contains(body, maskB)) || strings.Contains(body, "payout_instrument") {
		t.Fatalf("the 404 leaks the other tenant's instrument: %s", body)
	}

	// Tenant B's own staff do see it (not a broken route).
	financeB := mustCreateStaff(t, pool, tenantB.ID, identity.StaffRoleFinance, "finance-xb-pw-1")
	tokenB := mustLoginStaff(t, srv, tenantB.Slug, financeB.Email, "finance-xb-pw-1")
	status, body = rawBody(t, getJSON(t, srv, "/v1/admin/withdrawals/"+wrB.ID.String(), tokenB.AccessToken))
	if status != http.StatusOK || !strings.Contains(body, instB.String()) {
		t.Fatalf("tenant B staff: status %d body %s", status, body)
	}
	// A wrong-permission role of the owning tenant is still refused (no new permission).
	support := mustCreateStaff(t, pool, tenantB.ID, identity.StaffRoleSupport, "support-xb-pw-1")
	tokenS := mustLoginStaff(t, srv, tenantB.Slug, support.Email, "support-xb-pw-1")
	if status, _ = rawBody(t, getJSON(t, srv, "/v1/admin/withdrawals/"+wrB.ID.String(), tokenS.AccessToken)); status != http.StatusForbidden {
		t.Fatalf("support: status %d, want 403", status)
	}
}

func TestAdminGetWithdrawal_LegacyNullBindingIsNull(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	finance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleFinance, "finance-leg-pw-1")
	token := mustLoginStaff(t, srv, tenant.Slug, finance.Email, "finance-leg-pw-1")
	player := mustRegisterPlayer(t, srv, brand.Slug)
	walletID := fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 50000).ID

	legacyID := uuid.New()
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return pitest.WithoutBindingGuard(ctx, tx, func() error {
			_, err := tx.Exec(ctx, `INSERT INTO withdrawal_requests
				(id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, idempotency_key)
				VALUES ($1, $2, $3, $4, $5, 'EUR', 700, 'legacy-null-binding')`,
				legacyID, tenant.ID, brand.ID, player.ID, walletID)
			return err
		})
	}); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	status, body := rawBody(t, getJSON(t, srv, "/v1/admin/withdrawals/"+legacyID.String(), token.AccessToken))
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &top); err != nil {
		t.Fatal(err)
	}
	raw, present := top["payout_instrument"]
	if !present || string(raw) != "null" {
		t.Fatalf("a legacy NULL binding must serialize payout_instrument as null, got present=%v %s", present, raw)
	}
}

// L-1: a withdrawal that names a payout instrument which cannot be read is an integrity anomaly. It must be a
// generic 500, never `payout_instrument: null` (which the back office renders as an editable "legacy" withdrawal).
// Simulated with a RESTRICTIVE select policy hiding exactly that instrument id (the composite FK makes a missing
// row impossible); the policy is dropped on cleanup and affects no other row.
func TestAdminGetWithdrawal_UnreadableBoundInstrumentIsA500NotNull(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	finance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleFinance, "finance-l1-pw-1")
	token := mustLoginStaff(t, srv, tenant.Slug, finance.Email, "finance-l1-pw-1")
	player := mustRegisterPlayer(t, srv, brand.Slug)
	walletID := fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 50000).ID
	wr := mustCreateWithdrawalRequest(t, pool, tenant.ID, brand.ID, player.ID, walletID, "EUR", 1500)

	var instID uuid.UUID
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT payout_instrument_id FROM withdrawal_requests WHERE id = $1`, wr.ID).Scan(&instID)
	}); err != nil {
		t.Fatal(err)
	}
	policy := "l1_hide_" + strings.ReplaceAll(instID.String(), "-", "")[:12]
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `CREATE POLICY `+policy+` ON payout_instruments AS RESTRICTIVE FOR SELECT USING (id <> '`+instID.String()+`'::uuid)`)
		return err
	}); err != nil {
		t.Fatalf("create the simulation policy: %v", err)
	}
	t.Cleanup(func() {
		_ = pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DROP POLICY IF EXISTS `+policy+` ON payout_instruments`)
			return err
		})
	})

	status, body := rawBody(t, getJSON(t, srv, "/v1/admin/withdrawals/"+wr.ID.String(), token.AccessToken))
	if status != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500: %s", status, body)
	}
	if strings.Contains(body, "payout_instrument") || strings.Contains(body, instID.String()) || strings.Contains(body, "null") {
		t.Fatalf("the 500 must be generic (no instrument data, no null view): %s", body)
	}
}
