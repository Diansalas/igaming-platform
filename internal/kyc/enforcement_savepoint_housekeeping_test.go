// FK-5 (code review f-kyc-code-review.md, 2026-09-28, optional): mutant X5
// survived - changing EvaluateEnforcement's savepoint-housekeeping-failure
// branch from `return unavailableDecision("savepoint_failed"), nil` to
// `return decision, nil` fails OPEN (or at least fails in a way that is
// not the deterministic `unavailable` outcome ADR 0096 promises), and no
// fault-injection test can reach this branch - a real Postgres connection
// essentially never fails to open OR roll back a SAVEPOINT once a query
// has already succeeded against it. This is a pure Go unit test (no
// database, no `integration` build tag): a fake pgx.Tx whose nested
// (savepoint) Tx answers the read successfully but fails on Rollback,
// proving EvaluateEnforcement discards whatever decision the read
// computed and returns `unavailable` instead, exactly as promised.
package kyc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakeNoRowsRow is a pgx.Row whose Scan always reports pgx.ErrNoRows -
// enough for readLatestVerificationByPlayerAccount's single QueryRow call
// to complete normally (found=false), so the read itself succeeds; the
// housekeeping failure this test is about happens strictly AFTER that,
// in the savepoint's own Rollback.
type fakeNoRowsRow struct{}

func (fakeNoRowsRow) Scan(dest ...any) error { return pgx.ErrNoRows }

// fakeSavepointTx is the nested (pseudo-savepoint) pgx.Tx returned by
// fakeOuterTx.Begin. Every method this test's code path does not reach
// panics loudly rather than silently returning a zero value, so a future
// change that makes EvaluateEnforcement call something new here fails
// this test clearly instead of passing for the wrong reason.
type fakeSavepointTx struct {
	rollbackErr error
}

func (f *fakeSavepointTx) Begin(ctx context.Context) (pgx.Tx, error) {
	panic("fakeSavepointTx: Begin not expected on the nested tx")
}
func (f *fakeSavepointTx) Commit(ctx context.Context) error {
	panic("fakeSavepointTx: Commit not expected - RunReadOnlyInSavepoint always rolls back")
}
func (f *fakeSavepointTx) Rollback(ctx context.Context) error { return f.rollbackErr }
func (f *fakeSavepointTx) CopyFrom(ctx context.Context, tableName pgx.Identifier, columnNames []string, rowSrc pgx.CopyFromSource) (int64, error) {
	panic("fakeSavepointTx: CopyFrom not expected")
}
func (f *fakeSavepointTx) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults {
	panic("fakeSavepointTx: SendBatch not expected")
}
func (f *fakeSavepointTx) LargeObjects() pgx.LargeObjects {
	panic("fakeSavepointTx: LargeObjects not expected")
}
func (f *fakeSavepointTx) Prepare(ctx context.Context, name, sql string) (*pgconn.StatementDescription, error) {
	panic("fakeSavepointTx: Prepare not expected")
}
func (f *fakeSavepointTx) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	panic("fakeSavepointTx: Exec not expected - EvaluateEnforcement is read-only")
}
func (f *fakeSavepointTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	panic("fakeSavepointTx: Query not expected")
}
func (f *fakeSavepointTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	// The one read the withdrawal structural rule's evaluateWithdrawalStructuralRule
	// issues (readLatestVerificationByPlayerAccount) - answered successfully
	// (no rows), so the read itself is not what fails here.
	return fakeNoRowsRow{}
}
func (f *fakeSavepointTx) Conn() *pgx.Conn { return nil }

// fakeOuterTx is the caller's own transaction: its only job is to hand
// back fakeSavepointTx from Begin, exactly like a real pgx.Tx opening a
// SAVEPOINT (db/idempotency.go's own documented mechanism).
type fakeOuterTx struct {
	nested *fakeSavepointTx
}

func (f *fakeOuterTx) Begin(ctx context.Context) (pgx.Tx, error) { return f.nested, nil }
func (f *fakeOuterTx) Commit(ctx context.Context) error {
	panic("fakeOuterTx: Commit not expected - this test never commits the outer tx")
}
func (f *fakeOuterTx) Rollback(ctx context.Context) error {
	panic("fakeOuterTx: Rollback not expected - this test never rolls back the outer tx")
}
func (f *fakeOuterTx) CopyFrom(ctx context.Context, tableName pgx.Identifier, columnNames []string, rowSrc pgx.CopyFromSource) (int64, error) {
	panic("fakeOuterTx: CopyFrom not expected")
}
func (f *fakeOuterTx) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults {
	panic("fakeOuterTx: SendBatch not expected")
}
func (f *fakeOuterTx) LargeObjects() pgx.LargeObjects {
	panic("fakeOuterTx: LargeObjects not expected")
}
func (f *fakeOuterTx) Prepare(ctx context.Context, name, sql string) (*pgconn.StatementDescription, error) {
	panic("fakeOuterTx: Prepare not expected")
}
func (f *fakeOuterTx) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	panic("fakeOuterTx: Exec not expected - EvaluateEnforcement never writes on the outer tx either")
}
func (f *fakeOuterTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	panic("fakeOuterTx: Query not expected")
}
func (f *fakeOuterTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	panic("fakeOuterTx: QueryRow not expected - every read goes through the savepoint-scoped tx")
}
func (f *fakeOuterTx) Conn() *pgx.Conn { return nil }

var _ pgx.Tx = (*fakeOuterTx)(nil)
var _ pgx.Tx = (*fakeSavepointTx)(nil)

// TestEvaluateEnforcement_SavepointRollbackFailure_FailsClosedToUnavailable
// is FK-5: the read behind the savepoint succeeds (no rows -> would
// ordinarily compute Outcome=failed for the withdrawal structural rule),
// but the savepoint's own Rollback fails - a housekeeping failure, not a
// read failure. EvaluateEnforcement must discard whatever the read
// computed and return Outcome=unavailable, Allowed=false, with a nil Go
// error (never surfacing the raw rollback error to the caller, and never
// the read's own Outcome=failed result, which would be a fail-OPEN-
// relative-to-the-real-cause bug: the evaluator genuinely could not
// complete this evaluation, and must not report a decision as if it had).
func TestEvaluateEnforcement_SavepointRollbackFailure_FailsClosedToUnavailable(t *testing.T) {
	outer := &fakeOuterTx{nested: &fakeSavepointTx{rollbackErr: errors.New("simulated: ROLLBACK TO SAVEPOINT failed")}}

	decision, err := EvaluateEnforcement(context.Background(), outer, EnforcementParams{
		TenantID: uuid.New(), BrandID: uuid.New(), PlayerAccountID: uuid.New(), PersonID: uuid.New(),
		Operation: EnforcementWithdrawalHold, AssetCode: "EUR", Amount: 500, CorrelationID: uuid.New(),
	})
	if err != nil {
		t.Fatalf("expected a nil Go error (per §2.6(c): a query/housekeeping failure is a typed unavailable decision, never a raw error), got: %v", err)
	}
	if decision.Outcome != OutcomeUnavailable {
		t.Fatalf("expected Outcome=%q on a savepoint-housekeeping failure, got %+v", OutcomeUnavailable, decision)
	}
	if decision.Allowed {
		t.Fatalf("expected Allowed=false, got %+v", decision)
	}
}
