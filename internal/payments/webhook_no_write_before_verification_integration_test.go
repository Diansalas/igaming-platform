//go:build integration

// PAY-WH-TENANT-1 (ADR 0090; docs/decisions/0022 §3 amendment 2026-09-26).
// QA binding test plan T11a (docs/plans/stage-10.1-planning/
// 16-pay-wh-review-qa-test-plan.md): "a statement-capturing transaction
// proving ZERO reads of ledger/intent/wallet/projection tables, zero
// locks, zero writes before signature verification (invariant I1)".
//
// Technique: wrap the real pgx.Tx ReceiveCallback is handed with a
// recordingTx that records the literal SQL text of every Exec/Query/
// QueryRow call made on it, without changing behavior (every call is
// still forwarded to the real, embedded pgx.Tx). This is deterministic -
// no reliance on timing, pg_stat_statements, or any extension - and
// directly answers "what SQL ran", which is a strictly stronger and more
// direct proof than inferring it from lock/wait state.
package payments

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// recordingTx wraps a real pgx.Tx and records every SQL statement text
// passed to Exec/Query/QueryRow, in order, before forwarding the call
// unchanged to the embedded Tx. Every OTHER pgx.Tx method (Begin, Commit,
// Rollback, CopyFrom, SendBatch, LargeObjects, Prepare, Conn) is inherited
// from the embedded interface value untouched - this type is a pure
// observer, never a behavior change, so wrapping tx cannot itself be the
// reason a test passes or fails.
type recordingTx struct {
	pgx.Tx
	mu         sync.Mutex
	statements []string
}

func (r *recordingTx) record(sql string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statements = append(r.statements, sql)
}

func (r *recordingTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	r.record(sql)
	return r.Tx.Exec(ctx, sql, args...)
}

func (r *recordingTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	r.record(sql)
	return r.Tx.Query(ctx, sql, args...)
}

func (r *recordingTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	r.record(sql)
	return r.Tx.QueryRow(ctx, sql, args...)
}

func (r *recordingTx) Statements() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.statements))
	copy(out, r.statements)
	return out
}

// writeOrLockPattern matches any SQL statement this suite treats as a
// write or a row lock: INSERT/UPDATE/DELETE as leading verbs (word
// boundary, case-insensitive - so it also catches a leading-whitespace or
// newline-prefixed statement, which this codebase's multi-line SQL
// literals commonly are) or a "FOR UPDATE"/"FOR NO KEY UPDATE" row-lock
// clause anywhere in the text.
var writeOrLockPattern = regexp.MustCompile(`(?is)^\s*(INSERT|UPDATE|DELETE)\b|\bFOR\s+(NO\s+KEY\s+)?UPDATE\b`)

// TestWebhook_BadSignature_NoWriteBeforeVerification is QA plan T11a.
func TestWebhook_BadSignature_NoWriteBeforeVerification(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp": provider}, MultiWebhookCredentialResolver{"mock-psp": NewMockWebhookCredentials(provider)})

	// A genuinely tenant-bound, well-formed callback (valid tenant, valid
	// provider, valid header format, valid key id) whose SIGNATURE itself
	// is wrong - the exact case the plan names: "valid tenant, bad
	// signature". Steps (a)-(d) all succeed; only (e) HMAC verification
	// fails.
	payload := provider.CallbackPayload(f.tenantID, CallbackEventDeposit, "t11a-ref", "", OutcomeSucceeded, 1000, "EUR", "", false)
	badHeader := payload.Header.Clone()
	sig := badHeader.Get(HeaderSignature)
	flipped := strings.Replace(sig, "0", "f", 1)
	if flipped == sig {
		flipped = strings.Replace(sig, "1", "e", 1)
	}
	badHeader.Set(HeaderSignature, flipped)
	payload.Header = badHeader

	var captured *recordingTx
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		captured = &recordingTx{Tx: tx}
		_, err := orch.ReceiveCallback(ctx, captured, f.tenantID, "mock-psp", payload)
		return err
	})
	var authErr *CallbackAuthError
	if err == nil {
		t.Fatalf("expected the bad-signature callback to be rejected")
	}
	if !errors.As(err, &authErr) || authErr.Reason != ReasonSignatureInvalid {
		t.Fatalf("expected *CallbackAuthError{Reason: signature_invalid}, got %v", err)
	}

	statements := captured.Statements()
	if len(statements) == 0 {
		t.Fatalf("expected at least one statement (ProviderAcceptsWebhook's own read-only EXISTS), got none - the harness itself may not be wired correctly")
	}
	for _, sql := range statements {
		if writeOrLockPattern.MatchString(sql) {
			t.Fatalf("invariant I1 violated: a write or row-lock statement ran before signature verification succeeded: %q\nall statements: %q", sql, statements)
		}
	}
	// Positive check: the one permitted statement is exactly
	// ProviderAcceptsWebhook's read-only EXISTS over provider_capabilities -
	// asserted by name so this test fails loudly (not silently) if a future
	// change adds a second pre-verification read this plan did not
	// anticipate, rather than only catching a write/lock.
	if len(statements) != 1 || !strings.Contains(statements[0], "provider_capabilities") || !strings.Contains(statements[0], "EXISTS") {
		t.Fatalf("expected exactly one statement, ProviderAcceptsWebhook's own EXISTS over provider_capabilities, got %q", statements)
	}
}
