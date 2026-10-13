//go:build integration

package httpserver

import (
	"context"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/testsupport/amlfix"
)

// Security F4, option (b) (a reversible orchestrator default, not owner-approved): a manual-adjustment refusal caused by an AML hold
// is INDISTINGUISHABLE, to a tenant staff member, from an ordinary conflict: same status, same headers, same body, and the same audit
// shape and denied_class. The real reason exists only in the owner-only AML decision log.

type coverObs struct {
	status    int
	headers   string
	body      string
	auditKeys string
	class     string
	outcome   string
	target    string
}

var reqIDRE = regexp.MustCompile(`"request_id"\s*:\s*"[^"]*"`)

func (w *maWorld) observe(res cgResp, action string, actor uuid.UUID) coverObs {
	w.a.t.Helper()
	var hs []string
	for k, v := range res.header {
		switch http.CanonicalHeaderKey(k) {
		case "Date", "X-Request-Id", "Content-Length":
			hs = append(hs, k+"=*")
		default:
			hs = append(hs, k+"="+strings.Join(v, ","))
		}
	}
	sort.Strings(hs)
	o := coverObs{status: res.status, headers: strings.Join(hs, ";"), body: reqIDRE.ReplaceAllString(string(res.body), `"request_id":"*"`)}
	if err := w.a.pool.WithTenant(context.Background(), w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		var keys []string
		rows, err := tx.Query(ctx, `SELECT k FROM audit_log a, LATERAL jsonb_object_keys(a.metadata) k
			WHERE a.id = (SELECT id FROM audit_log WHERE action = $1 AND outcome = 'denied' AND actor_id = $2 ORDER BY created_at DESC, id DESC LIMIT 1) ORDER BY k`, action, actor)
		if err != nil {
			return err
		}
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				rows.Close()
				return err
			}
			keys = append(keys, k)
		}
		rows.Close()
		o.auditKeys = strings.Join(keys, ",")
		return tx.QueryRow(ctx, `SELECT metadata->>'denied_class', outcome, target_type FROM audit_log WHERE action = $1 AND outcome = 'denied' AND actor_id = $2
			ORDER BY created_at DESC, id DESC LIMIT 1`, action, actor).Scan(&o.class, &o.outcome, &o.target)
	}); err != nil {
		w.a.t.Fatalf("observe audit: %v", err)
	}
	return o
}

func TestManualAdjustmentAPI_F4_AMLRefusalIsIndistinguishableFromAnOrdinaryConflict(t *testing.T) {
	w := newMAWorld(t)
	base := "/v1/admin/tenants/" + w.tenant.String() + "/manual-adjustments"
	var person uuid.UUID
	if err := w.a.pool.WithTenant(context.Background(), w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT person_id FROM player_accounts WHERE id = $1`, w.player).Scan(&person)
	}); err != nil {
		t.Fatal(err)
	}
	approve := func(id, hash string) cgResp {
		return w.a.do("POST", base+"/"+id+"/approve", w.tok(w.f2), map[string]any{"payload_hash": hash, "reason_code": "ok"})
	}

	// (A) the ordinary conflict: a decision on a request that is no longer pending.
	_, d1 := w.submit(w.f1)
	if res := approve(d1.ID, d1.PayloadHash); res.status != http.StatusOK {
		t.Fatalf("setup approve: %d %s", res.status, res.body)
	}
	ordinary := w.observe(approve(d1.ID, d1.PayloadHash), "ledger_adjustment.approve", w.f2)
	if ordinary.status != http.StatusConflict || !strings.Contains(ordinary.body, "conflict") {
		t.Fatalf("the reference must be the ordinary 409 conflict: %+v", ordinary)
	}

	// (B) AML at EXECUTION: submit first, hold afterwards, then the final approval.
	st, d2 := w.submit(w.f1)
	if st != http.StatusCreated {
		t.Fatalf("submit before the hold: %d", st)
	}
	amlfix.HoldFor(t, w.a.pool, w.a.pool, w.tenant, person, nil, aml.ScopeAllValueMovement)
	atExec := w.observe(approve(d2.ID, d2.PayloadHash), "ledger_adjustment.approve", w.f2)
	if atExec != ordinary {
		t.Fatalf("an AML refusal at execution must be byte-identical to the ordinary conflict:\n aml:      %+v\n ordinary: %+v", atExec, ordinary)
	}
	// Nothing AML-specific is stored on the request: it stays pending, no refusal code, no approval row.
	var state string
	var refusal *string
	var approvals int
	if err := w.a.pool.WithPrincipalScope(context.Background(), w.tenant, w.f1, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT state, refusal_code FROM ledger_adjustment_requests WHERE id = $1`, d2.ID).Scan(&state, &refusal); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_adjustment_approvals WHERE request_id = $1`, d2.ID).Scan(&approvals)
	}); err != nil {
		t.Fatal(err)
	}
	if state != "pending" || refusal != nil || approvals != 0 {
		t.Fatalf("the refused request must stay untouched: state=%s refusal=%v approvals=%d", state, refusal, approvals)
	}

	// (C) AML at SUBMISSION (the hold is in place).
	res := w.a.do("POST", base, w.tok(w.f1), map[string]any{
		"wallet_id": w.wallet.String(), "asset_code": "USD", "direction": "credit_player", "amount_minor_units": "250",
		"reason_code": "operational_error_correction", "note": "f4 cover",
	})
	atSubmit := w.observe(res, "ledger_adjustment.submit", w.f1)
	if atSubmit.status != ordinary.status || atSubmit.headers != ordinary.headers || atSubmit.body != ordinary.body ||
		atSubmit.auditKeys != ordinary.auditKeys || atSubmit.class != ordinary.class || atSubmit.outcome != ordinary.outcome || atSubmit.target != ordinary.target {
		t.Fatalf("an AML refusal at submission must look like the ordinary conflict:\n aml:      %+v\n ordinary: %+v", atSubmit, ordinary)
	}
	for _, o := range []coverObs{atExec, atSubmit} {
		for _, bad := range []string{"account_review", "review", "hold", "aml", "AM0"} {
			if strings.Contains(o.body, bad) || strings.Contains(o.class, bad) || strings.Contains(o.auditKeys, bad) {
				t.Fatalf("AML wording %q leaked into %+v", bad, o)
			}
		}
	}

	// The real reason is recorded ONLY in the owner-only AML decision log, once per refusal.
	var n int
	if err := w.a.pool.WithTenant(context.Background(), w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM aml_enforcement_decisions WHERE tenant_id = $1 AND person_id = $2 AND operation = 'manual_adjustment'
			AND hold_outcome = 'held' AND public_outcome = 'review' AND NOT allowed AND hold_id IS NOT NULL`, w.tenant, person).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("the AML evidence rows for the two refusals: want 2, got %d", n)
	}
}
