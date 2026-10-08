//go:build integration

// PAY-R13 defence in depth (security r12 L-1 / LF L-1, ADR 0095 section 42.8): GetAttemptByMerchantReference
// carries an explicit tenant_id predicate in addition to RLS. The merchant reference is unique per
// (tenant_id, merchant_reference), so the same value can legitimately exist in two tenants.
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

// tpSetup seeds two tenants in ONE database whose attempts share the same merchant reference value.
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
	// Force tenant B's attempt to carry the SAME merchant reference value as A's (allowed: unique per tenant).
	ref := a.attempt.MerchantReference
	if err := b.pool.WithTenant(context.Background(), b.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE payment_attempts SET merchant_reference = $2 WHERE id = $1`, b.attempt.ID, ref)
		return err
	}); err != nil {
		t.Fatalf("setup: share merchant reference across tenants: %v", err)
	}
	return tpEnv{envA: a, envB: b, ref: ref}
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

func TestR13_GetAttemptByMerchantReference_SameRefTwoTenants_ReturnsOwn(t *testing.T) {
	e := tpSetup(t)
	gotA, err := tpLookup(t, e.envA, e.envA.f.tenantID, e.envA.f.tenantID, e.ref)
	if err != nil || gotA.ID != e.envA.attempt.ID || gotA.TenantID != e.envA.f.tenantID {
		t.Fatalf("tenant A lookup = %+v, %v; want A's attempt %s", gotA.ID, err, e.envA.attempt.ID)
	}
	gotB, err := tpLookup(t, e.envB, e.envB.f.tenantID, e.envB.f.tenantID, e.ref)
	if err != nil || gotB.ID != e.envB.attempt.ID || gotB.TenantID != e.envB.f.tenantID {
		t.Fatalf("tenant B lookup = %+v, %v; want B's attempt %s", gotB.ID, err, e.envB.attempt.ID)
	}
}

// A mismatched explicit tenant id is not-found even though the (RLS-scoped) session tenant owns a row with
// that reference: the explicit predicate, not RLS, is what rejects it.
func TestR13_GetAttemptByMerchantReference_MismatchedExplicitTenant_NotFound(t *testing.T) {
	e := tpSetup(t)
	if _, err := tpLookup(t, e.envA, e.envA.f.tenantID, e.envB.f.tenantID, e.ref); !errors.Is(err, ErrAttemptNotFound) {
		t.Fatalf("session A + explicit B: err = %v, want ErrAttemptNotFound", err)
	}
	if _, err := tpLookup(t, e.envB, e.envB.f.tenantID, e.envA.f.tenantID, e.ref); !errors.Is(err, ErrAttemptNotFound) {
		t.Fatalf("session B + explicit A: err = %v, want ErrAttemptNotFound", err)
	}
	if _, err := tpLookup(t, e.envA, e.envA.f.tenantID, uuid.New(), e.ref); !errors.Is(err, ErrAttemptNotFound) {
		t.Fatalf("unknown explicit tenant: err = %v, want ErrAttemptNotFound", err)
	}
}

// A multi-tenant-visible session (platform-admin scope, no role change) sees BOTH rows for the reference;
// the explicit predicate must still select exactly the requested tenant's attempt.
func TestR13_GetAttemptByMerchantReference_MultiTenantVisibleSession(t *testing.T) {
	e := tpSetup(t)
	err := e.envA.pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM payment_attempts WHERE merchant_reference = $1`, e.ref).Scan(&n); err != nil {
			return err
		}
		if n != 2 {
			t.Skipf("platform-admin session sees %d rows for the reference (RLS does not widen); mismatch test covers the predicate", n)
		}
		a, err := GetAttemptByMerchantReference(ctx, tx, e.envA.f.tenantID, e.ref)
		if err != nil || a.ID != e.envA.attempt.ID {
			t.Fatalf("A: %v %v", a.ID, err)
		}
		b, err := GetAttemptByMerchantReference(ctx, tx, e.envB.f.tenantID, e.ref)
		if err != nil || b.ID != e.envB.attempt.ID {
			t.Fatalf("B: %v %v", b.ID, err)
		}
		if _, err := GetAttemptByMerchantReference(ctx, tx, uuid.New(), e.ref); !errors.Is(err, ErrAttemptNotFound) {
			t.Fatalf("unknown tenant: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("platform admin tx: %v", err)
	}
}

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
