//go:build integration

package db

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/txscope"
)

// TestWithScopes_MarkTxscope is security condition C6 (ADR 0094 §4.1):
// EVERY db.Pool method whose name starts with "With" - found by
// reflection, so a new scope function is covered automatically - must
// hand fn a txscope-marked context, so the INV-POOL guard (no secret-store
// call while a pooled transaction is held) sees it.
func TestWithScopes_MarkTxscope(t *testing.T) {
	pool := testPool(t)
	errDone := errors.New("done: roll back")
	pt := reflect.TypeOf(pool)
	txFuncType := reflect.TypeOf(TxFunc(nil))
	found := 0
	for i := 0; i < pt.NumMethod(); i++ {
		m := pt.Method(i)
		if !strings.HasPrefix(m.Name, "With") {
			continue
		}
		found++
		t.Run(m.Name, func(t *testing.T) {
			var marked, called bool
			fn := TxFunc(func(ctx context.Context, _ pgx.Tx) error {
				called, marked = true, txscope.Held(ctx)
				return errDone
			})

			// WithPlatformActingInTenant (ADR 0099 §6.1, PRH-2 K1) is
			// deliberately NOT like every other With* setter: it validates
			// the acting session against the database (an in-force grant
			// for principalID/targetTenantID) BEFORE ever calling fn, and
			// raises (SQLSTATE CG020) rather than proceeding on a random
			// uuid.New() pair - that fail-closed behaviour is the whole
			// point of the ADR. The generic "any random UUID makes fn run"
			// assumption below does not hold for it, so this builds a real,
			// valid grant fixture first and calls it directly.
			if m.Name == "WithPlatformActingInTenant" {
				principalID, tenantID := mustBuildValidActingGrantFixture(t, pool)
				if err := pool.WithPlatformActingInTenant(context.Background(), principalID, tenantID, fn); !errors.Is(err, errDone) {
					t.Fatalf("%s did not run fn: %v", m.Name, err)
				}
				if !called || !marked {
					t.Fatalf("%s must pass a txscope-marked context to fn (ADR 0094 INV-POOL guard)", m.Name)
				}
				return
			}

			args := []reflect.Value{reflect.ValueOf(pool)}
			for j := 1; j < m.Type.NumIn(); j++ {
				in := m.Type.In(j)
				switch {
				case in == reflect.TypeOf((*context.Context)(nil)).Elem():
					args = append(args, reflect.ValueOf(context.Background()))
				case in == reflect.TypeOf(uuid.UUID{}):
					args = append(args, reflect.ValueOf(uuid.New()))
				case in == reflect.TypeOf(PlatformService("")):
					args = append(args, reflect.ValueOf(ServiceSportsbookCatalogueSync))
				case in == txFuncType:
					args = append(args, reflect.ValueOf(fn))
				case in.Kind() == reflect.String:
					args = append(args, reflect.ValueOf("0000000000000000000000000000000000000000000000000000000000000000").Convert(in))
				default:
					t.Fatalf("%s: no test value for parameter type %s - extend this test", m.Name, in)
				}
			}
			out := m.Func.Call(args)
			if err, _ := out[len(out)-1].Interface().(error); !errors.Is(err, errDone) {
				t.Fatalf("%s did not run fn: %v", m.Name, err)
			}
			if !called || !marked {
				t.Fatalf("%s must pass a txscope-marked context to fn (ADR 0094 INV-POOL guard)", m.Name)
			}
		})
	}
	if found < 10 {
		t.Fatalf("found only %d With* scope functions; the reflection walk is broken", found)
	}
}

// TestWithTenantReadOnly_RefusesWrites (ADR 0094 §4.1): the
// pre-verification transaction is READ ONLY, so PostgreSQL itself refuses
// a write (SQLSTATE 25006) - defence in depth under the statement-capture
// tests, which stay the primary I1 control (READ ONLY does not stop
// advisory locks).
func TestWithTenantReadOnly_RefusesWrites(t *testing.T) {
	pool := testPool(t)
	tenant := uuid.New()
	err := pool.WithTenantReadOnly(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		var ro, scoped string
		if err := tx.QueryRow(ctx, `SELECT current_setting('transaction_read_only'), current_setting('app.tenant_id', true)`).Scan(&ro, &scoped); err != nil {
			return err
		}
		if ro != "on" || scoped != tenant.String() {
			t.Fatalf("read_only=%s app.tenant_id=%s, want on/%s", ro, scoped, tenant)
		}
		_, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, slug, name) VALUES ($1, $2, 'ro-test', 'x')`, uuid.New(), tenant)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "25006") && !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("a write in WithTenantReadOnly must be refused by PostgreSQL, got %v", err)
	}
}
