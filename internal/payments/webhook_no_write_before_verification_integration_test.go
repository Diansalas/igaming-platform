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

// sqlRecorder is the shared, mutex-guarded statement log a recordingTx and
// every "child" recordingTx returned by its own Begin (see N4 below) all
// append to - a single ordered log across the whole savepoint tree, not
// one log per nesting level, so the top-level captured.Statements() call
// in the test below sees everything regardless of how deep a savepoint a
// statement ran inside.
type sqlRecorder struct {
	mu         sync.Mutex
	statements []string
}

func (rec *sqlRecorder) record(sql string) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.statements = append(rec.statements, sql)
}

func (rec *sqlRecorder) Statements() []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	out := make([]string, len(rec.statements))
	copy(out, rec.statements)
	return out
}

// recordingTx wraps a real pgx.Tx and records every SQL statement text
// passed to Exec/Query/QueryRow, in order, before forwarding the call
// unchanged to the embedded Tx. Every OTHER pgx.Tx method (Commit,
// Rollback, LargeObjects, Prepare, Conn) is inherited from the embedded
// interface value untouched - this type is a pure observer, never a
// behavior change, so wrapping tx cannot itself be the reason a test
// passes or fails.
//
// N4 (Stage 10.1 code-review re-verification, 2026-09-26): the original
// version of this type wrapped only Exec/Query/QueryRow, so a
// pre-verification write via db.IdempotentInsert (which opens a savepoint
// with Begin, then writes on the returned child Tx) would run entirely
// unobserved - the child Tx returned by the embedded Tx.Begin was a bare
// pgx.Tx, not a recordingTx, so its Exec calls never reached record()
// above. This test would then report a false pass on invariant I1 for
// exactly the kind of future change it exists to catch. Begin is now
// wrapped so a savepoint's own child is ALSO a recording observer sharing
// this same log (via *sqlRecorder), and SendBatch/CopyFrom - which this
// codebase currently never calls on a pre-verification path, but whose
// statement text this technique cannot individually inspect - are recorded
// under an explicit marker and independently asserted never to occur
// before verification, rather than silently falling through unobserved.
type recordingTx struct {
	pgx.Tx
	rec *sqlRecorder
}

func newRecordingTx(tx pgx.Tx) *recordingTx {
	return &recordingTx{Tx: tx, rec: &sqlRecorder{}}
}

func (r *recordingTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	r.rec.record(sql)
	return r.Tx.Exec(ctx, sql, args...)
}

func (r *recordingTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	r.rec.record(sql)
	return r.Tx.Query(ctx, sql, args...)
}

func (r *recordingTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	r.rec.record(sql)
	return r.Tx.QueryRow(ctx, sql, args...)
}

// Begin wraps the real Tx.Begin (a SAVEPOINT under the hood) so the
// returned child transaction is ITSELF a recordingTx sharing this same
// *sqlRecorder - see the N4 doc comment above. Without this, any statement
// run on the savepoint returned by an unwrapped Begin would be invisible
// to Statements() below.
func (r *recordingTx) Begin(ctx context.Context) (pgx.Tx, error) {
	r.rec.record("-- recordingTx.Begin: savepoint opened")
	child, err := r.Tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &recordingTx{Tx: child, rec: r.rec}, nil
}

// sendBatchOrCopyMarker prefixes a recorded entry for SendBatch/CopyFrom -
// calls this technique cannot decompose into individual SQL text, so
// writeOrLockPattern's textual match cannot inspect them. The test below
// therefore treats ANY such marker as an invariant-I1 violation outright,
// never merely pattern-matching its (nonexistent) SQL text.
const sendBatchOrCopyMarker = "-- recordingTx: "

func (r *recordingTx) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults {
	r.rec.record(sendBatchOrCopyMarker + "SendBatch called (opaque batch of statements, not individually recorded)")
	return r.Tx.SendBatch(ctx, b)
}

func (r *recordingTx) CopyFrom(ctx context.Context, tableName pgx.Identifier, columnNames []string, rowSrc pgx.CopyFromSource) (int64, error) {
	r.rec.record(sendBatchOrCopyMarker + "CopyFrom called: " + tableName.Sanitize())
	return r.Tx.CopyFrom(ctx, tableName, columnNames, rowSrc)
}

func (r *recordingTx) Statements() []string {
	return r.rec.Statements()
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
	// Flip the LAST hex digit only, never the "v1=" prefix: the previous
	// "replace the first '0', else the first '1'" hit the prefix whenever
	// the hex had no '0' (~1.6% of runs), producing a malformed header that
	// ParseHeaders rejects before ProviderAcceptsWebhook - zero recorded
	// statements and a spurious failure (TEST-T11A-FLIP-1).
	sig := badHeader.Get(HeaderSignature)
	if !strings.HasPrefix(sig, "v1=") || len(sig) <= len("v1=") {
		t.Fatalf("unexpected signature header shape %q", sig)
	}
	last := sig[len(sig)-1]
	repl := byte('0')
	if last == '0' {
		repl = '1'
	}
	flipped := sig[:len(sig)-1] + string(repl)
	badHeader.Set(HeaderSignature, flipped)
	payload.Header = badHeader

	var captured *recordingTx
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		captured = newRecordingTx(tx)
		_, err := orch.receiveCallbackInTx(ctx, captured, f.tenantID, "mock-psp", payload)
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
		if strings.HasPrefix(sql, sendBatchOrCopyMarker) {
			t.Fatalf("invariant I1 violated: SendBatch/CopyFrom ran before signature verification succeeded (N4): %q\nall statements: %q", sql, statements)
		}
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
