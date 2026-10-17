//go:build integration

package payments

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/payoutinstrument"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
)

// F2 fix round: SRF2-2 / LFF2-2 (copied run rows are never M2 evidence: run seal, migration 0168), LFF2-3 (a
// future-dated failure row cannot block M2), SRF2-5 / LFF2-7 (the evidence is read before the M2 transaction).

func f2eKeys(t *testing.T) *payoutinstrument.Keys {
	t.Helper()
	m, fp := make([]byte, 32), make([]byte, 32)
	for i := range m {
		m[i], fp[i] = byte(i+7), byte(250-i)
	}
	k, err := payoutinstrument.NewKeys("f2e", map[string][]byte{"f2e": m}, "f2e-fp", map[string][]byte{"f2e-fp": fp})
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// f2eCopyNewestRun copies the newest sealed run row of the provider as a NEW row with run_at = now() (and,
// optionally, window_truncated = false), keeping its seal, kid and import: the SRF2-2 / LFF2-2 forgery.
func f2eCopyNewestRun(w *k3World, untruncate bool) {
	w.t.Helper()
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO reconciliation_runs (tenant_id, stream, period_start, period_end, status, coverage_end, provider_id, window_truncated,
			run_import_id, run_seal, run_seal_kid)
			SELECT tenant_id, stream, period_start, period_end, status, coverage_end, provider_id, CASE WHEN $3 THEN false ELSE window_truncated END,
			       run_import_id, run_seal, run_seal_kid
			  FROM reconciliation_runs WHERE tenant_id = $1 AND provider_id = $2 AND run_seal IS NOT NULL
			 ORDER BY run_at DESC LIMIT 1`, w.f.tenantID, w.provider, untruncate)
		return err
	})
}

// THE SRF2-2 / LFF2-2 PROBES, with keys: a genuine sealed run, then a failure / staleness / truncation, then ONE run
// row copied from the newest sealed run with run_at = now(). Before 0168 the copy made the M2 request ADMITTED; now
// the copy's run seal does not verify (new id, new run_at) and the M2 stays refused.
func TestF2e_SRF2_2_CopiedRunRowNeverMakesTheEvidenceCurrent(t *testing.T) {
	for _, variant := range []string{"failed", "stale", "truncated"} {
		t.Run(variant, func(t *testing.T) {
			w := newK3World(t, k3Opts{base: 1, noStatementRun: true})
			keys := f2eKeys(t)
			w.svc = w.svc.WithImportSealKeys(keys)
			_, a := w.ambiguousPayout(500)
			switch variant {
			case "failed":
				k3StatementRun(t, w.pool, w.f.tenantID, w.mock, keys)
				time.Sleep(5 * time.Millisecond)
				f2eRecordFailure(t, w.pool, w.f.tenantID, w.provider)
			case "stale":
				k3StatementRun(t, w.pool, w.f.tenantID, w.mock, keys)
				w.svc = w.svc.WithStatementEvidenceMaxAge(50 * time.Millisecond)
				time.Sleep(100 * time.Millisecond)
			case "truncated":
				w.ambiguousPayout(500)
				src := NewMockStatementSource(w.mock, MockCredentialResolver{})
				out := reconciliation.ReconcilePaymentStatementForTenant(context.Background(), w.pool, nil, w.f.tenantID, time.Now().Add(-time.Hour), time.Now(), src,
					reconciliation.PaymentStatementOptions{Continuation: true, RunLineBound: 1, ImportKeys: keys})
				if out.Err != nil || !out.Truncated {
					t.Fatalf("setup: a sealed truncated run: %+v", out)
				}
			}
			if _, err := f2eRequestM2(w, a.ID, ResolutionM2DeclareNotPaid); !errors.Is(err, ErrResolutionReconciliationNotCurrent) {
				t.Fatalf("control: refused before the forgery, got %v", err)
			}
			f2eCopyNewestRun(w, variant == "truncated")
			if _, err := f2eRequestM2(w, a.ID, ResolutionM2DeclareNotPaid); !errors.Is(err, ErrResolutionReconciliationNotCurrent) {
				t.Fatalf("a copied run row must not make the evidence current: %v", err)
			}
		})
	}
}

// LFF2-3: an audit row dated ten years ahead (created_at is caller-settable) does not block M2 once a genuine run
// follows; a failure inside the 5-minute skew tolerance after the run still refuses.
func TestF2e_LFF2_3_FutureDatedFailureRowDoesNotBlockForever(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	_, a := w.ambiguousPayout(500)
	w.ensureStatementRun()
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO audit_log (tenant_id, actor_type, action, target_type, target_id, outcome, metadata, created_at)
			VALUES ($1, 'system', 'reconciliation.sweep_run_failed', 'tenant', $3, 'failure',
			        jsonb_build_object('stream', 'payment_statement', 'provider_id', $2::text), now() + interval '10 years')`, w.f.tenantID, w.provider, w.f.tenantID.String())
		return err
	})
	if _, err := f2eRequestM2(w, a.ID, ResolutionM2DeclareNotPaid); err != nil {
		t.Fatalf("a future-dated failure row must not block M2: %v", err)
	}
	w2 := newK3World(t, k3Opts{base: 1})
	_, a2 := w2.ambiguousPayout(500)
	w2.ensureStatementRun()
	w2.tx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO audit_log (tenant_id, actor_type, action, target_type, target_id, outcome, metadata, created_at)
			VALUES ($1, 'system', 'reconciliation.sweep_run_failed', 'tenant', $3, 'failure',
			        jsonb_build_object('stream', 'payment_statement', 'provider_id', $2::text), now() + interval '1 minute')`, w2.f.tenantID, w2.provider, w2.f.tenantID.String())
		return err
	})
	if _, err := f2eRequestM2(w2, a2.ID, ResolutionM2DeclareNotPaid); !errors.Is(err, ErrResolutionReconciliationNotCurrent) {
		t.Fatalf("a failure inside the skew tolerance still refuses: %v", err)
	}
}

// SRF2-5 / LFF2-7 (static): the transaction bodies never read the evidence themselves (no second pooled connection
// inside the M2 transaction); Request and Decide pre-read it before runSession.
func TestF2e_SRF2_5_EvidenceIsReadBeforeTheTransaction(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "manual_resolution.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string]map[string]bool{}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		set := map[string]bool{}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				switch fun := c.Fun.(type) {
				case *ast.SelectorExpr:
					set[fun.Sel.Name] = true
				case *ast.Ident:
					set[fun.Name] = true
				}
			}
			return true
		})
		calls[fn.Name.Name] = set
	}
	for _, in := range []string{"requestInTx", "decideInTx"} {
		for _, banned := range []string{"preReadStatementEvidence", "WithTenantReadOnly", "WithTenant", "statementEvidenceRefusalInTx"} {
			if calls[in][banned] {
				t.Errorf("%s calls %s: the evidence must be read before the transaction", in, banned)
			}
		}
		if !calls[in]["statementEvidenceRefusal"] {
			t.Errorf("%s no longer applies the pre-read evidence", in)
		}
	}
	for _, outer := range []string{"Request", "Decide"} {
		if !calls[outer]["preReadStatementEvidence"] || !calls[outer]["withReconEvidence"] {
			t.Errorf("%s must pre-read the evidence before runSession", outer)
		}
	}
	// No pre-read on ctx (an entry point that skipped it) refuses, it never falls back to an in-transaction read.
	if reason, err := statementEvidenceRefusal(context.Background(), nil); err != nil || reason != reconEvidenceUnread {
		t.Fatalf("no evidence on ctx: %q %v", reason, err)
	}
	ev := &reconEvidence{reasons: map[string]string{"p": ""}}
	p := "p"
	if reason, _ := statementEvidenceRefusal(withReconEvidence(context.Background(), ev), &p); reason != "" {
		t.Fatalf("current provider: %q", reason)
	}
	q := "q"
	if reason, _ := statementEvidenceRefusal(withReconEvidence(context.Background(), ev), &q); reason != reconEvidenceNoRun {
		t.Fatalf("unknown provider: %q", reason)
	}
	ev.err = errors.New("boom")
	if _, err := statementEvidenceRefusal(withReconEvidence(context.Background(), ev), &p); err == nil {
		t.Fatal("a pre-read error must fail closed")
	}
	_ = uuid.Nil
}
