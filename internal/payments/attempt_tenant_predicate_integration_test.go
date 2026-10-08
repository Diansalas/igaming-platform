//go:build integration

// PAY-R13 defence in depth (security r12 L-1 / LF L-1, ADR 0095 section 42.8): GetAttemptByMerchantReference
// carries an explicit tenant_id predicate in addition to RLS. The merchant reference is unique per
// (tenant_id, merchant_reference) but the reference is derived from the attempt id (a global primary key,
// MerchantReferenceFor), and payment_attempts' immutability trigger forbids rewriting it, so a genuine
// same-value collision across tenants cannot be constructed; the tests instead present tenant A's reference
// to tenant B's scope and to a multi-tenant-visible session.
package payments

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type tpEnv struct {
	envA, envB t4DrainEnv
	ref        string
}

// tpSetup seeds two tenants in ONE database, each with an attempt; ref is tenant A's merchant reference.
func tpSetup(t *testing.T) tpEnv {
	t.Helper()
	a := t4DrainSetup(t, "mock-tp-a", "tp-a")
	f := seedOrchFixture(t, a.pool)
	mp := NewMockProvider("mock-tp-b", "EUR")
	registerCapability(t, a.pool, f, mp, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-tp-b": &refLessAmbiguousProvider{mp}},
		MultiWebhookCredentialResolver{"mock-tp-b": NewMockWebhookCredentials(mp)})
	res := rvInit(t, a.pool, orch, f, MockAmountAmbiguous, "tp-b")
	b := t4DrainEnv{pool: a.pool, orch: orch, f: f, attempt: res.Attempt, intent: res.Intent.ID, provider: "mock-tp-b"}
	if a.f.tenantID == b.f.tenantID {
		t.Fatal("setup: tenants must differ")
	}
	return tpEnv{envA: a, envB: b, ref: a.attempt.MerchantReference}
}

func tpLookup(t *testing.T, e t4DrainEnv, sessionTenant, explicit uuid.UUID, ref string) (PaymentAttempt, error) {
	t.Helper()
	var got PaymentAttempt
	var lerr error
	if err := e.pool.WithTenant(context.Background(), sessionTenant, func(ctx context.Context, tx pgx.Tx) error {
		got, lerr = GetAttemptByMerchantReference(ctx, tx, explicit, ref)
		return nil
	}); err != nil {
		t.Fatalf("tx: %v", err)
	}
	return got, lerr
}

func TestR13_GetAttemptByMerchantReference_TwoTenants_ReturnsOwn(t *testing.T) {
	e := tpSetup(t)
	gotA, err := tpLookup(t, e.envA, e.envA.f.tenantID, e.envA.f.tenantID, e.ref)
	if err != nil || gotA.ID != e.envA.attempt.ID || gotA.TenantID != e.envA.f.tenantID {
		t.Fatalf("tenant A lookup = %+v, %v; want A's attempt %s", gotA.ID, err, e.envA.attempt.ID)
	}
	refB := e.envB.attempt.MerchantReference
	gotB, err := tpLookup(t, e.envB, e.envB.f.tenantID, e.envB.f.tenantID, refB)
	if err != nil || gotB.ID != e.envB.attempt.ID || gotB.TenantID != e.envB.f.tenantID {
		t.Fatalf("tenant B lookup = %+v, %v; want B's attempt %s", gotB.ID, err, e.envB.attempt.ID)
	}
	if _, err := tpLookup(t, e.envB, e.envB.f.tenantID, e.envB.f.tenantID, e.ref); !errors.Is(err, ErrAttemptNotFound) {
		t.Fatalf("tenant B must never see A's attempt: err = %v", err)
	}
}

// A mismatched explicit tenant id is not-found even though the RLS-scoped session tenant owns the row:
// the explicit predicate, not RLS, is what rejects it (session A, explicit B, A's own reference).
func TestR13_GetAttemptByMerchantReference_MismatchedExplicitTenant_NotFound(t *testing.T) {
	e := tpSetup(t)
	if _, err := tpLookup(t, e.envA, e.envA.f.tenantID, e.envB.f.tenantID, e.ref); !errors.Is(err, ErrAttemptNotFound) {
		t.Fatalf("session A + explicit B: err = %v, want ErrAttemptNotFound", err)
	}
	if _, err := tpLookup(t, e.envB, e.envB.f.tenantID, e.envA.f.tenantID, e.envB.attempt.MerchantReference); !errors.Is(err, ErrAttemptNotFound) {
		t.Fatalf("session B + explicit A: err = %v, want ErrAttemptNotFound", err)
	}
	if _, err := tpLookup(t, e.envA, e.envA.f.tenantID, uuid.New(), e.ref); !errors.Is(err, ErrAttemptNotFound) {
		t.Fatalf("unknown explicit tenant: err = %v, want ErrAttemptNotFound", err)
	}
}

// NOTE: a multi-tenant-visible session cannot be built without altering roles (platform-admin scope does
// not widen payment_attempts RLS - verified: it sees 0 rows), so the mismatched-explicit-tenant test above
// is the proof of the predicate.

// Static guard: no non-test caller may pass a tenant value that is not an identifier named tenantID or the
// attempt's own TenantID, and the function must keep its tenant parameter.
func TestR13_GetAttemptByMerchantReference_StaticGuard(t *testing.T) {
	fset := token.NewFileSet()
	files, _ := filepath.Glob("*.go")
	calls := 0
	for _, fn := range files {
		if strings.HasSuffix(fn, "_test.go") {
			continue
		}
		src, err := os.ReadFile(fn)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, fn, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := c.Fun.(*ast.Ident)
			if !ok || id.Name != "GetAttemptByMerchantReference" {
				return true
			}
			calls++
			if len(c.Args) != 4 {
				t.Errorf("%s: GetAttemptByMerchantReference needs (ctx, tx, tenantID, ref)", fset.Position(c.Pos()))
				return true
			}
			switch a := c.Args[2].(type) {
			case *ast.Ident:
				if a.Name != "tenantID" {
					t.Errorf("%s: tenant arg %q not the scoped tenantID", fset.Position(c.Pos()), a.Name)
				}
			case *ast.SelectorExpr:
				if a.Sel.Name != "TenantID" {
					t.Errorf("%s: tenant arg is not a .TenantID selector", fset.Position(c.Pos()))
				}
			default:
				t.Errorf("%s: unexpected tenant argument expression", fset.Position(c.Pos()))
			}
			return true
		})
	}
	if calls < 2 {
		t.Fatalf("expected the live-path and drain call sites, found %d", calls)
	}
}
