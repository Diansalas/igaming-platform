//go:build integration

package httpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/identity"
)

// SA-5: PUT /v1/admin/providers/{id}/capability against the real runtime role. The acceptance suite reported that a `sepa`
// method sent to the mock was dropped with a 200. At this commit that is not reproducible: a request outside the adapter's
// declared manifest is a 400 and writes nothing. These tests pin that, and that the 200 states what was stored.

type capabilityPutResult struct {
	status int
	raw    []byte
	body   struct {
		ID        string                      `json:"id"`
		Effective capabilityEffectiveResponse `json:"effective"`
		Narrowed  bool                        `json:"narrowed"`
	}
}

func putCapability(t *testing.T, srvURL, token string, req map[string]any) capabilityPutResult {
	t.Helper()
	httpReq, err := http.NewRequest(http.MethodPut, srvURL+"/v1/admin/providers/mock/capability", jsonBody(t, req))
	if err != nil {
		t.Fatal(err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out capabilityPutResult
	out.status = resp.StatusCode
	out.raw, _ = io.ReadAll(resp.Body)
	_ = json.Unmarshal(out.raw, &out.body)
	return out
}

func TestProviderCapabilityPut_ReportsTheEffectiveStoredConfigurationAndRefusesWidening(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	ta := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-sa5-pw-1")
	tok := mustLoginStaff(t, srv, tenant.Slug, ta.Email, "ta-sa5-pw-1").AccessToken

	stored := func() (methods, fiat []string, limits int, audits int) {
		t.Helper()
		if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `SELECT supported_payment_methods, supported_fiat_currencies FROM provider_capabilities WHERE provider_id = 'mock'`).Scan(&methods, &fiat); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM provider_capability_amount_limits`).Scan(&limits); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'provider_capability.configured'`).Scan(&audits)
		}); err != nil {
			t.Fatal(err)
		}
		return methods, fiat, limits, audits
	}

	good := map[string]any{
		"supported_fiat_currencies": []string{"EUR", "USD"},
		"supported_payment_methods": []string{"card", "bank_transfer"},
		"supports_deposit":          true, "supports_withdrawal": true,
		"amount_limits": []map[string]any{{"asset_code": "EUR", "min_amount": 100, "max_amount": 500000}},
		"priority":      1, "status": "active",
	}
	res := putCapability(t, srv.URL, tok, good)
	if res.status != http.StatusOK {
		t.Fatalf("valid configuration: %d %s", res.status, res.raw)
	}
	methods, fiat, limits, audits := stored()
	eff := res.body.Effective
	if !reflect.DeepEqual(eff.SupportedPaymentMethods, methods) || !reflect.DeepEqual(eff.SupportedFiatCurrencies, fiat) ||
		len(eff.AmountLimits) != limits || limits != 1 || eff.AmountLimits[0].AssetCode != "EUR" || eff.AmountLimits[0].MaxAmount != 500000 {
		t.Fatalf("the response's effective configuration %+v must equal the stored row (methods %v, fiat %v, %d limits)", eff, methods, fiat, limits)
	}
	if res.body.Narrowed || res.body.ID == "" || !eff.SupportsDeposit || !eff.SupportsWithdrawal || eff.Status != "active" || eff.Priority != 1 {
		t.Fatalf("unexpected response %s", res.raw)
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(res.raw, &raw)
	if len(raw) != 3 {
		t.Fatalf("the response is exactly id, effective, narrowed: %s", res.raw)
	}

	// The reported trap: a method outside the adapter's manifest. It is refused, never silently dropped, and the
	// stored row and the audit log are untouched.
	for _, methodsAsked := range [][]string{{"card", "sepa"}, {"sepa"}} {
		bad := map[string]any{}
		for k, v := range good {
			bad[k] = v
		}
		bad["supported_payment_methods"] = methodsAsked
		res := putCapability(t, srv.URL, tok, bad)
		if res.status != http.StatusBadRequest {
			t.Fatalf("methods %v: got %d %s, want 400 (never a 200 that drops a value)", methodsAsked, res.status, res.raw)
		}
		m2, f2, l2, a2 := stored()
		if !reflect.DeepEqual(m2, methods) || !reflect.DeepEqual(f2, fiat) || l2 != limits || a2 != audits {
			t.Fatalf("a refused request changed state: methods %v fiat %v limits %d audits %d", m2, f2, l2, a2)
		}
	}
	// An amount limit outside the adapter's range is refused too, and an unknown field is refused (no silent drop).
	badLimit := map[string]any{}
	for k, v := range good {
		badLimit[k] = v
	}
	badLimit["amount_limits"] = []map[string]any{{"asset_code": "EUR", "min_amount": 1, "max_amount": 500000}}
	if res := putCapability(t, srv.URL, tok, badLimit); res.status != http.StatusBadRequest {
		t.Fatalf("limit below the adapter range: %d %s", res.status, res.raw)
	}
	unknown := map[string]any{}
	for k, v := range good {
		unknown[k] = v
	}
	unknown["payment_methods"] = []string{"sepa"}
	if res := putCapability(t, srv.URL, tok, unknown); res.status != http.StatusBadRequest {
		t.Fatalf("unknown field: %d %s", res.status, res.raw)
	}

	// Replacement narrows to a subset: the 200 reports the new, smaller stored configuration.
	narrower := map[string]any{}
	for k, v := range good {
		narrower[k] = v
	}
	narrower["supported_payment_methods"] = []string{"card"}
	narrower["amount_limits"] = []map[string]any{}
	res = putCapability(t, srv.URL, tok, narrower)
	if res.status != http.StatusOK || !reflect.DeepEqual(res.body.Effective.SupportedPaymentMethods, []string{"card"}) ||
		len(res.body.Effective.AmountLimits) != 0 || res.body.Narrowed {
		t.Fatalf("narrowing replacement: %d %s", res.status, res.raw)
	}
	if m3, _, l3, _ := stored(); !reflect.DeepEqual(m3, []string{"card"}) || l3 != 0 {
		t.Fatalf("stored after replacement: %v, %d limits", m3, l3)
	}
}
