//go:build integration

// KYC-WH-1 (Stage 10.2, ADR 0091). Architect ruling R4/J6 requires the
// callback-dispatch reference lookup (getVerificationByProviderReference,
// verification_service.go) to carry an EXPLICIT tenant_id predicate ON TOP
// OF - never instead of - kyc_verifications' own tenant_isolation RLS.
//
// The implementer reported this could not be shown red with a behavioural
// (RLS-mediated) test: RLS itself already scopes every read to the current
// connection's tenant, so a test that only asserts "the wrong tenant's row
// is never returned" cannot distinguish "the code has an explicit
// predicate" from "the code has no predicate at all and RLS alone is doing
// 100% of the work" - exactly the gap R4/J6 exists to close in the first
// place (defence in depth: RLS misconfigured for this connection must
// still not leak a row).
//
// TestGetVerificationByProviderReference_CarriesExplicitTenantIDPredicate
// closes that gap directly: it captures the ACTUAL SQL statement text and
// its ACTUAL bound argument via a statement-and-argument-capturing pgx.Tx
// wrapper, and asserts the tenant id both appears in the WHERE clause text
// and is bound as $1 with the fixture's own tenant id - a property of the
// CODE, inspectable independently of whatever RLS would or would not have
// enforced behaviourally.
//
// Mutation-kill demonstration (recorded here, not committed): with the
// predicate removed from verification_service.go's query text AND its
// placeholders renumbered (`WHERE provider_id = $1 AND provider_reference =
// $2`, dropping `tenant_id = $1 AND` and passing only providerID/
// providerReference at the call site, never tenantID), this test fails on
// this test's OWN tenant-predicate assertion ("no explicit tenant_id = $1
// predicate"), not on a SQL error - see docs/plans/stage-10.2-planning/
// 08-webhook-test-traceability.md for the recorded pre-revert failure
// output (Stage 10.2 final review, K2/M2: an earlier version of this test
// matched the lookup statement by the literal substring "provider_id =
// $2", which a renumbering alone defeats before the mutation ever reaches
// this assertion - fixed by matching structurally, on "FROM
// kyc_verifications" + "provider_reference", instead).
package kyc

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// argCapture is one recorded (sql, args) pair.
type argCapture struct {
	sql  string
	args []any
}

// argRecordingTx wraps a pgx.Tx and records every QueryRow's exact SQL
// text AND its bound arguments (recordingTx, in recording_tx_integration_
// test.go, records SQL text only - insufficient here, since the whole
// point is to inspect what value is BOUND to the predicate, not merely
// that some statement mentions "tenant_id").
type argRecordingTx struct {
	pgx.Tx
	mu    sync.Mutex
	calls []argCapture
}

func newArgRecordingTx(tx pgx.Tx) *argRecordingTx {
	return &argRecordingTx{Tx: tx}
}

func (r *argRecordingTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	r.mu.Lock()
	r.calls = append(r.calls, argCapture{sql: sql, args: args})
	r.mu.Unlock()
	return r.Tx.QueryRow(ctx, sql, args...)
}

func (r *argRecordingTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	r.mu.Lock()
	r.calls = append(r.calls, argCapture{sql: sql, args: args})
	r.mu.Unlock()
	return r.Tx.Exec(ctx, sql, args...)
}

func (r *argRecordingTx) Calls() []argCapture {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]argCapture, len(r.calls))
	copy(out, r.calls)
	return out
}

// TestGetVerificationByProviderReference_CarriesExplicitTenantIDPredicate
// is R4/J6's own direct test: independent of RLS, the reference lookup's
// SQL text names tenant_id in its WHERE clause and binds the caller's
// tenantID to it - not merely "happens to return the right row because RLS
// filtered it".
func TestGetVerificationByProviderReference_CarriesExplicitTenantIDPredicate(t *testing.T) {
	pool := testPool(t)
	f, provider, orch, verificationID := newWebhookFixture(t, pool)
	var ref string
	_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT provider_reference FROM kyc_verifications WHERE id = $1`, verificationID).Scan(&ref)
	})

	in := provider.CallbackPayload(f.tenantID, ref, ProviderApproved, "auto_approved")

	var captured *argRecordingTx
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		captured = newArgRecordingTx(tx)
		_, _, err := orch.receiveCallbackInTx(ctx, captured, f.tenantID, "mock", in)
		return err
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var found bool
	for _, call := range captured.Calls() {
		// This is the reference-lookup statement (as opposed to the later
		// GetVerificationByID/UPDATE statements the same callback also
		// issues) - the one getVerificationByProviderReference issues. It
		// is matched STRUCTURALLY, by "FROM kyc_verifications" (a SELECT,
		// unlike the UPDATE...kyc_verifications statement, which never
		// contains "FROM kyc_verifications" as a substring) plus a WHERE
		// clause EQUALITY on provider_reference ("provider_reference =
		// $") - never by a literal placeholder number like "provider_id =
		// $2", which a harmless renumbering of the query's OWN
		// placeholders would silently stop matching (Stage 10.2 final
		// review, K2/M2: the earlier version of this test was killed by a
		// mutation for the wrong reason - a SQL type-inference error, not
		// this predicate assertion). Matching on the bare substring
		// "provider_reference" alone is NOT enough - verificationColumns
		// selects that column by name in every one of this package's
		// queries (GetVerificationByID included), so that alone would also
		// match the wrong statement; "provider_reference = $" only ever
		// appears in a WHERE-clause equality, and survives any
		// renumbering of which placeholder index it binds to.
		if !strings.Contains(call.sql, "FROM kyc_verifications") || !strings.Contains(call.sql, "provider_reference = $") {
			continue
		}
		found = true
		if !strings.Contains(call.sql, "tenant_id = $1") {
			t.Fatalf("R4/J6: the reference-lookup statement text has no explicit tenant_id = $1 predicate: %q", call.sql)
		}
		if len(call.args) < 1 {
			t.Fatalf("R4/J6: expected at least 1 bound argument, got %d", len(call.args))
		}
		gotTenant, ok := call.args[0].(uuid.UUID)
		if !ok {
			t.Fatalf("R4/J6: expected the FIRST bound argument to be a uuid.UUID (tenant_id), got %T", call.args[0])
		}
		if gotTenant != f.tenantID {
			t.Fatalf("R4/J6: expected the bound tenant_id argument to be the fixture's own tenant %s, got %s", f.tenantID, gotTenant)
		}
	}
	if !found {
		t.Fatal("R4/J6: never observed the reference-lookup statement at all - test infrastructure problem, not proof of anything")
	}
}
