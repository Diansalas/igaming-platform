//go:build integration

// PAY-K3-STATEMENT-SOURCE-WIRING-1 over real HTTP (security F-2/F-3/F-6, code
// review F-6): M2 is refused with the closed token and a denied audit row when
// no statement source is registered for the attempt's provider, accepted when
// the (MOCK) provider is registered, and a foreign tenant's attempt never yields
// data. Uses an ambiguous MOCK payout (the M2-admissible shape).
package httpserver

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// newFRWorldWithSources builds the FR world whose server has Deps.StatementSources = reg.
func newFRWorldWithSources(t *testing.T, reg *payments.StatementSourceRegistry) *frWorld {
	t.Helper()
	cgAPIStatementSources = reg
	t.Cleanup(func() { cgAPIStatementSources = nil })
	return newFRWorld(t, true)
}

// ambiguousPayout drives a real USD payout (mock adapter) to `ambiguous` through
// the production claim / apply path and returns the attempt id.
func (w *frWorld) ambiguousPayout() uuid.UUID {
	t := w.a.t
	t.Helper()
	ctx := context.Background()
	pool := w.a.pool
	fundWallet(t, pool, w.tenant, w.brand, w.player, "USD", 100000)
	if err := pool.WithTenant(ctx, w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO withdrawal_policies (tenant_id, asset_code, approval_threshold_minor_units, required_approvals, effective_from)
			VALUES ($1, 'USD', 1000000, 2, now() - interval '1 hour') ON CONFLICT DO NOTHING`, w.tenant)
		return err
	}); err != nil {
		t.Fatalf("policy: %v", err)
	}
	wr := mustCreateWithdrawalRequest(t, pool, w.tenant, w.brand, w.player, walletIDFor(t, pool, w.tenant, w.player, "USD"), "USD", 5000)
	do := func(fn func(ctx context.Context, tx pgx.Tx) error) {
		t.Helper()
		if err := pool.WithTenant(ctx, w.tenant, fn); err != nil {
			t.Fatalf("withdrawal setup: %v", err)
		}
	}
	do(func(ctx context.Context, tx pgx.Tx) error { return withdrawal.MoveToPendingReview(ctx, tx, wr.ID) })
	do(func(ctx context.Context, tx pgx.Tx) error {
		ok, err := withdrawal.Approve(ctx, tx, wr.ID, uuid.New(), true, nil, nil)
		if err == nil && !ok {
			err = errors.New("automated approval did not approve")
		}
		return err
	})
	claim, err := w.orch.ClaimForDispatch(ctx, pool, payments.KYCEnforcementPayoutGate{}, w.tenant, wr.ID, "bank_transfer",
		payments.SubmitActor{StaffID: uuid.New(), IPAddress: "127.0.0.1", UserAgent: "t", RequestID: uuid.NewString()})
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	if err := payments.ApplyPayoutResult(ctx, pool, w.tenant, wr.ID, claim.Attempt,
		payments.GateResult[payments.WithdrawResult]{Class: payments.ErrorClassAmbiguous, Err: errors.New("http m2 simulated timeout")}, payments.EvidenceSync); err != nil {
		t.Fatalf("ApplyPayoutResult(ambiguous): %v", err)
	}
	return claim.Attempt.ID
}

func (w *frWorld) m2Body(attempt uuid.UUID, kind string) map[string]any {
	return map[string]any{
		"attempt_id": attempt.String(), "kind": kind, "basis_code": payments.BasisProviderConfirmedOutOfBand,
		"evidence_ref_hash": strings.Repeat("ab", 32), "reason_code": "http-m2", "note": "http m2 test",
	}
}

func TestForceResolutionAPI_M2_SourceRegistry(t *testing.T) {
	// No source registered: BOTH kinds are refused with the closed token and an audit row.
	t.Run("refused_when_no_source_registered", func(t *testing.T) {
		w := newFRWorldWithSources(t, &payments.StatementSourceRegistry{})
		attempt := w.ambiguousPayout()
		for _, kind := range []string{"m2_declare_not_paid", "m2_declare_paid"} {
			before := w.deniedAudits()
			res := w.a.do("POST", w.base(), w.tok(w.f1), w.m2Body(attempt, kind))
			if res.status != http.StatusConflict || !strings.Contains(string(res.body), `"`+payments.TokenForceResolvePreconditionFail+`"`) {
				t.Fatalf("%s: want 409 %s, got %d %s", kind, payments.TokenForceResolvePreconditionFail, res.status, res.body)
			}
			if got := w.deniedAudits(); got != before+1 {
				t.Fatalf("%s: want exactly one new denied audit row, %d -> %d", kind, before, got)
			}
			w.note(res.status, res.body)
		}
		if n := w.countRows(`SELECT count(*) FROM payment_manual_resolutions WHERE tenant_id = $1`, w.tenant); n != 0 {
			t.Fatalf("a refused submission left %d resolution rows", n)
		}
		w.noLeakage(t, 2)
	})

	// Security F-3: authorization first. A requester with the route permission but
	// no in-force grant gets the audited 403 from the database guard, NOT the
	// 409 source-registry refusal (no attempt-existence / monitoring signal).
	t.Run("no_grant_gets_403_before_any_source_signal", func(t *testing.T) {
		w := newFRWorldWithSources(t, &payments.StatementSourceRegistry{})
		attempt := w.ambiguousPayout()
		before := w.deniedAudits()
		res := w.a.do("POST", w.base(), w.tok(w.fNoGrant), w.m2Body(attempt, "m2_declare_not_paid"))
		if res.status != http.StatusForbidden || !frHasClosedToken(res.body) {
			t.Fatalf("want an audited closed-token 403 for a requester without a grant, got %d %s", res.status, res.body)
		}
		if got := w.deniedAudits(); got != before+1 {
			t.Fatalf("want +1 denied audit row, %d -> %d", before, got)
		}
	})

	// A nil registry in Deps (never wired) refuses too.
	t.Run("refused_when_deps_registry_nil", func(t *testing.T) {
		w := newFRWorldWithSources(t, nil)
		attempt := w.ambiguousPayout()
		res := w.a.do("POST", w.base(), w.tok(w.f1), w.m2Body(attempt, "m2_declare_not_paid"))
		if res.status != http.StatusConflict || !frHasClosedToken(res.body) {
			t.Fatalf("want a closed-token 409, got %d %s", res.status, res.body)
		}
	})

	// The MOCK provider registered: accepted (kills "route passes nil instead of
	// deps.StatementSources").
	t.Run("accepted_when_provider_registered", func(t *testing.T) {
		reg := &payments.StatementSourceRegistry{}
		reg.Register("mock")
		w := newFRWorldWithSources(t, reg)
		attempt := w.ambiguousPayout()
		res := w.a.do("POST", w.base(), w.tok(w.f1), w.m2Body(attempt, "m2_declare_not_paid"))
		if res.status != http.StatusCreated {
			t.Fatalf("want 201 for a registered provider, got %d %s", res.status, res.body)
		}
		// Per provider: a registry holding only another id still refuses.
		reg2 := &payments.StatementSourceRegistry{}
		reg2.Register("some-other-psp")
		w2 := newFRWorldWithSources(t, reg2)
		attempt2 := w2.ambiguousPayout()
		res = w2.a.do("POST", w2.base(), w2.tok(w2.f1), w2.m2Body(attempt2, "m2_declare_not_paid"))
		if res.status != http.StatusConflict {
			t.Fatalf("only another provider registered: want 409, got %d %s", res.status, res.body)
		}
	})

	// Unknown attempt and a foreign tenant's attempt: no data, closed refusal, and
	// a denied audit row exactly like M1 (security F-2).
	t.Run("unknown_and_foreign_attempt_refused_and_audited", func(t *testing.T) {
		reg := &payments.StatementSourceRegistry{}
		reg.Register("mock")
		w := newFRWorldWithSources(t, reg)
		attempt := w.ambiguousPayout()

		before := w.deniedAudits()
		res := w.a.do("POST", w.base(), w.tok(w.f1), w.m2Body(uuid.New(), "m2_declare_not_paid"))
		if res.status != http.StatusForbidden && res.status != http.StatusConflict {
			t.Fatalf("unknown attempt: want an audited 403/409, got %d %s", res.status, res.body)
		}
		if !frHasClosedToken(res.body) {
			t.Fatalf("unknown attempt: want a closed token, got %s", res.body)
		}
		if got := w.deniedAudits(); got != before+1 {
			t.Fatalf("unknown attempt: want +1 denied audit row, %d -> %d", before, got)
		}

		// The other tenant's finance token names ITS OWN tenant path with this
		// tenant's attempt id: never data, and an audit row in its own tenant.
		otherBase := "/v1/admin/tenants/" + w.other.String() + "/payment-force-resolutions"
		beforeOther := len(w.deniedRows(w.other))
		res = w.a.do("POST", otherBase, w.tok(w.otherFin), w.m2Body(attempt, "m2_declare_not_paid"))
		if res.status != http.StatusForbidden && res.status != http.StatusNotFound && res.status != http.StatusConflict {
			t.Fatalf("foreign attempt: want 403/404/409, got %d %s", res.status, res.body)
		}
		if strings.Contains(string(res.body), attempt.String()) || strings.Contains(string(res.body), w.tenant.String()) {
			t.Fatalf("foreign attempt: the refusal leaked data: %s", res.body)
		}
		if after := len(w.deniedRows(w.other)); after != beforeOther+1 {
			t.Fatalf("foreign attempt: want +1 denied audit row in the caller's tenant, %d -> %d", beforeOther, after)
		}
		// And nothing was created anywhere.
		if n := w.countRows(`SELECT count(*) FROM payment_manual_resolutions WHERE tenant_id = $1`, w.tenant); n != 0 {
			t.Fatalf("foreign attempt created %d resolution rows in the victim tenant", n)
		}
	})
}

func (w *frWorld) countRows(sql string, args ...any) int {
	var n int
	if err := w.a.pool.WithTenant(context.Background(), w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(&n)
	}); err != nil {
		w.a.t.Fatal(err)
	}
	return n
}
