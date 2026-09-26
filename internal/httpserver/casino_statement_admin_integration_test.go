//go:build integration

// Stage 10.3 W3a (CAS-RECON-STMT-1): the read-only casino reconciliation
// admin views surface the casino_statement stream alongside
// casino_consistency - the optional ?stream= filter on runs and
// mismatches, a 400 for an unknown stream, the MOCK label carried in the
// statement mismatch's actual_value, and tenant isolation.
package httpserver

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// ghostLineSource is a TEST-ONLY statement source: the MOCK statement plus
// one line the ledger does not hold.
type ghostLineSource struct{}

func (ghostLineSource) Label() string { return "MOCK test statement with one injected ghost line" }

func (ghostLineSource) Statement(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, ps, pe time.Time) ([]statement.CasinoStatementLine, []statement.CasinoStatementTotal, error) {
	lines, totals, err := casino.MockStatementSource{}.Statement(ctx, tx, tenantID, ps, pe)
	if err != nil {
		return nil, nil, err
	}
	return append(lines, statement.CasinoStatementLine{ProviderID: "mock-casino", ProviderTxID: "ghost-adm",
		Kind: statement.CasinoLineBet, RoundID: "r-ghost", AssetCode: "EUR", Amount: 10}), totals, nil
}

func TestCasinoReconciliationAdmin_SurfacesCasinoStatementStream(t *testing.T) {
	a := newRejEnvHTTP(t)
	b := newRejEnvHTTP(t)
	if s := a.send(t, casino.CallbackEventBet, "adm-b", "", "adm-r", 100, a.sessionID); s != http.StatusOK {
		t.Fatalf("bet: %d", s)
	}
	if s := a.send(t, casino.CallbackEventWin, "adm-orphan", "", "adm-none", 50, uuid.Nil); s != http.StatusBadRequest {
		t.Fatalf("orphan win: %d", s)
	}
	runCasinoConsistencyForTest(t, a.pool, a.tenant.ID)
	for _, src := range []statement.CasinoStatementSource{casino.MockStatementSource{}, ghostLineSource{}} {
		if err := a.pool.WithTenantSnapshot(context.Background(), a.tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
			_, _, _, err := reconciliation.RunCasinoStatement(ctx, tx, a.tenant.ID, time.Now().Add(-time.Hour), time.Now(), src)
			return err
		}); err != nil {
			t.Fatalf("run casino_statement: %v", err)
		}
	}

	adminA := mustCreateStaff(t, a.pool, a.tenant.ID, identity.StaffRoleTenantAdmin, "a-decent-password-1")
	tokA := mustLoginStaff(t, a.srv, a.tenant.Slug, adminA.Email, "a-decent-password-1")

	runsPage := func(query string) pagedResponse[casinoReconciliationRunResponse] {
		t.Helper()
		var p pagedResponse[casinoReconciliationRunResponse]
		resp := getJSON(t, a.srv, "/v1/admin/casino/reconciliation/runs"+query, tokA.AccessToken)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("runs%s: %d", query, resp.StatusCode)
		}
		decodeBody(t, resp, &p)
		return p
	}
	if p := runsPage(""); p.Total != 3 {
		t.Fatalf("unfiltered runs must include both casino streams (1 + 2), got %+v", p)
	}
	if p := runsPage("?stream=casino_consistency"); p.Total != 1 || p.Items[0].Stream != "casino_consistency" {
		t.Fatalf("unexpected casino_consistency runs: %+v", p)
	}
	stmtRuns := runsPage("?stream=casino_statement")
	if stmtRuns.Total != 2 {
		t.Fatalf("expected two casino_statement runs, got %+v", stmtRuns)
	}
	statuses := map[string]int{}
	for _, r := range stmtRuns.Items {
		if r.Stream != "casino_statement" {
			t.Fatalf("filter leaked another stream: %+v", r)
		}
		statuses[r.Status]++
	}
	if statuses["clean"] != 1 || statuses["mismatches_found"] != 1 {
		t.Fatalf("expected one clean MOCK run and one divergent run, got %v", statuses)
	}

	mismatchPage := func(query string) pagedResponse[casinoReconciliationMismatchResponse] {
		t.Helper()
		var p pagedResponse[casinoReconciliationMismatchResponse]
		resp := getJSON(t, a.srv, "/v1/admin/casino/reconciliation/mismatches"+query, tokA.AccessToken)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("mismatches%s: %d", query, resp.StatusCode)
		}
		decodeBody(t, resp, &p)
		return p
	}
	if p := mismatchPage(""); p.Total != 2 {
		t.Fatalf("unfiltered mismatches must include both streams' kinds, got %+v", p)
	}
	if p := mismatchPage("?stream=casino_consistency"); p.Total != 1 || p.Items[0].MismatchKind != "cas_unposted_provider_event" {
		t.Fatalf("unexpected casino_consistency mismatches: %+v", p)
	}
	st := mismatchPage("?stream=casino_statement&status=open")
	if st.Total != 1 || st.Items[0].MismatchKind != "cas_mock_statement_mismatch" ||
		!strings.Contains(st.Items[0].ReconciliationKey, "provider_tx_id=ghost-adm") ||
		!strings.Contains(st.Items[0].ActualValue, "MOCK") {
		t.Fatalf("unexpected casino_statement mismatches: %+v", st)
	}
	for _, p := range []string{"/v1/admin/casino/reconciliation/runs?stream=bogus", "/v1/admin/casino/reconciliation/mismatches?stream=ledger_vs_projection"} {
		if resp := getJSON(t, a.srv, p, tokA.AccessToken); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s must be 400, got %d", p, resp.StatusCode)
		}
	}

	// Tenant B sees none of A's statement evidence.
	adminB := mustCreateStaff(t, b.pool, b.tenant.ID, identity.StaffRoleTenantAdmin, "a-decent-password-1")
	tokB := mustLoginStaff(t, b.srv, b.tenant.Slug, adminB.Email, "a-decent-password-1")
	for _, p := range []string{"/v1/admin/casino/reconciliation/runs?stream=casino_statement", "/v1/admin/casino/reconciliation/mismatches?stream=casino_statement"} {
		var page pagedResponse[map[string]any]
		resp := getJSON(t, b.srv, p, tokB.AccessToken)
		decodeBody(t, resp, &page)
		if page.Total != 0 || len(page.Items) != 0 {
			t.Fatalf("tenant B must see none of A's rows on %s, got %+v", p, page)
		}
	}
}
