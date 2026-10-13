package aml

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
)

// HoldEvalTimeout bounds the hold read (ADR 0116 s5.1: "the caller wraps it in SET LOCAL statement_timeout"). It is an
// ENGINEERING DEFAULT, not a legal value. A read that exceeds it is an evaluator error: the operation is denied.
const HoldEvalTimeout = 2 * time.Second

// HoldVerdict is the AML half of a gate decision (ADR 0116 s5.1).
//
// It is INTERNAL: it must never reach a player, a tenant-visible log or an alert payload. The public rendering of
// Held and EvalFailed is identical to a KYC `pending` (code account_review, outcome review).
type HoldVerdict struct {
	// Held: an active hold gates the operation.
	Held bool
	// EvalFailed: the evaluator errored, timed out or was called with a malformed argument. The operation is denied
	// (fail closed) exactly as if it were held, and ONLY this operation (ADR 0116 s5.1 "Evaluator error").
	EvalFailed bool
	// HoldID is the matched hold when Held and the lookup succeeded; uuid.Nil otherwise.
	HoldID uuid.UUID
}

// Denied reports whether the operation must be refused.
func (v HoldVerdict) Denied() bool { return v.Held || v.EvalFailed }

// holdProbe is the one database read of the enforcement path. It is a package variable ONLY so the integration-tag test
// hook (hold_probe_hook_integration.go, absent from every production build) can inject an evaluator fault; production
// code never assigns it.
var holdProbe = probeHoldActive

// EvaluateHold is the AML enforcement read (ADR 0116 s5.1, L1 / C6): it asks aml_hold_active() whether an active hold
// gates op for the person in the tenant, INSIDE the caller's transaction, with no I/O other than that single database
// round trip. A failure of the read itself is NOT an error return: it is HoldVerdict{EvalFailed: true}, which every caller
// treats as a deny of exactly this operation. The probe runs in a savepoint that is always rolled back, so a failing read
// never aborts the caller's transaction.
//
// A failed evaluation raises the alert Kind aml.hold_eval_failed (operation name only) in the caller's transaction. The
// returned error is non-nil ONLY when that alert statement propagated a hard error (the transaction is unusable: a
// deadlock, a cancelled context); the verdict is still EvalFailed and the caller must treat the whole call as a deny (the
// LF F4 rule: an in-tx raise result is never dropped).
func EvaluateHold(ctx context.Context, tx pgx.Tx, tenantID, personID, accountID uuid.UUID, op Operation) (HoldVerdict, error) {
	if tenantID == uuid.Nil || personID == uuid.Nil || accountID == uuid.Nil || !op.Valid() {
		// A structurally invalid call is a caller bug: deny it. There is no tenant to attribute an alert to.
		return HoldVerdict{EvalFailed: true}, nil
	}
	held, err := holdProbe(ctx, tx, tenantID, personID, accountID, op)
	if err != nil {
		return HoldVerdict{EvalFailed: true}, raiseEvalFailed(ctx, tx, tenantID, op)
	}
	if !held {
		return HoldVerdict{}, nil
	}
	v := HoldVerdict{Held: true}
	// The matched hold id is evidence for the AML-scoped decision log. Its lookup is best effort: the operation is
	// already denied, so a failure here changes nothing but the evidence.
	if id, mErr := matchHold(ctx, tx, tenantID, personID, accountID, op); mErr == nil {
		v.HoldID = id
	}
	return v, nil
}

// probeHoldActive opens a SAVEPOINT (one round trip), then runs SET LOCAL statement_timeout; SELECT aml_hold_active(...);
// ROLLBACK TO SAVEPOINT; RELEASE SAVEPOINT as ONE pipelined batch (a second round trip). The rollback undoes the SET LOCAL,
// so the caller's own timeouts are untouched, and ANY failure inside the savepoint (a missing function, a permission error,
// a timeout) is contained: the caller's transaction stays usable.
func probeHoldActive(ctx context.Context, tx pgx.Tx, tenantID, personID, accountID uuid.UUID, op Operation) (bool, error) {
	const sp = "aml_hold_probe"
	if _, err := tx.Exec(ctx, "SAVEPOINT "+sp); err != nil {
		return false, fmt.Errorf("aml: hold probe: open savepoint: %w", err)
	}
	b := &pgx.Batch{}
	b.Queue(`SELECT set_config('statement_timeout', $1, true)`, fmt.Sprintf("%d", HoldEvalTimeout.Milliseconds()))
	b.Queue(`SELECT aml_hold_active($1, $2, $3, $4)`, tenantID, personID, accountID, string(op))
	b.Queue("ROLLBACK TO SAVEPOINT " + sp)
	b.Queue("RELEASE SAVEPOINT " + sp)
	br := tx.SendBatch(ctx, b)
	var (
		held   bool
		runErr error
	)
	if _, err := br.Exec(); err != nil {
		runErr = err
	}
	if runErr == nil {
		if err := br.QueryRow().Scan(&held); err != nil {
			runErr = err
		}
	}
	if runErr == nil {
		if _, err := br.Exec(); err != nil {
			runErr = err
		}
	}
	if runErr == nil {
		if _, err := br.Exec(); err != nil {
			runErr = err
		}
	}
	closeErr := br.Close()
	if runErr == nil {
		runErr = closeErr
	}
	if runErr != nil {
		// The savepoint is still open (the batch stopped at the failing statement): roll back to it so the caller's
		// transaction is usable, then release it. If even that fails the transaction is unusable and the caller's next
		// statement fails, which is itself fail-closed.
		_, _ = tx.Exec(ctx, "ROLLBACK TO SAVEPOINT "+sp)
		_, _ = tx.Exec(ctx, "RELEASE SAVEPOINT "+sp)
		return false, fmt.Errorf("aml: hold probe: %w", runErr)
	}
	return held, nil
}

func matchHold(ctx context.Context, tx pgx.Tx, tenantID, personID, accountID uuid.UUID, op Operation) (uuid.UUID, error) {
	var id *uuid.UUID
	sp, err := tx.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	qErr := sp.QueryRow(ctx, `SELECT aml_hold_match($1, $2, $3, $4)`, tenantID, personID, accountID, string(op)).Scan(&id)
	_ = sp.Rollback(ctx)
	if qErr != nil {
		return uuid.Nil, qErr
	}
	if id == nil {
		return uuid.Nil, fmt.Errorf("aml: hold vanished between the probe and the match")
	}
	return *id, nil
}

// raiseEvalFailed raises aml.hold_eval_failed in the caller's transaction and RETURNS the alerting layer's result: it
// swallows only the narrow, documented SQLSTATE classes (ADR 0102 s7.2); anything else is a hard error the transaction
// owner must see.
func raiseEvalFailed(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, op Operation) error {
	return alerting.RaiseGuarded(ctx, tx, alerting.Alert{
		Kind:            alerting.KindAMLHoldEvalFailed,
		SubjectTenantID: tenantID,
		Discriminator:   "hold_eval:" + string(op),
		Attributes:      map[string]alerting.AttrValue{"operation": string(op)},
	})
}
