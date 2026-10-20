package aml

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/db"
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
	// ScreeningOnly (lic18, ledger-finance H-1 e): the operation is held ONLY by the automatic-screening gate (an outstanding,
	// stale or failed required screening, or a system-found match no officer has resolved) and NO officer-placed hold exists. It
	// is a waiting state, not a compliance finding: a payout claim must PARK (the approved request and its funds hold stay, the
	// claim is retried) instead of rejecting the withdrawal. It is set only when the hold lookup SUCCEEDED and found none.
	ScreeningOnly bool
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
	return evaluateHold(ctx, tx, tenantID, personID, accountID, op, true)
}

// EvaluateHoldDeferringAlert is EvaluateHold for a caller whose OWN transaction will roll back when the verdict denies (a refusal
// returned as an error): the aml.hold_eval_failed alert is NOT raised in that transaction (it would be rolled back with it and
// lost; ledger-finance C3). The caller raises it after the rollback with RaiseHoldEvalFailedDetached when the returned verdict has
// EvalFailed set.
func EvaluateHoldDeferringAlert(ctx context.Context, tx pgx.Tx, tenantID, personID, accountID uuid.UUID, op Operation) (HoldVerdict, error) {
	return evaluateHold(ctx, tx, tenantID, personID, accountID, op, false)
}

func evaluateHold(ctx context.Context, tx pgx.Tx, tenantID, personID, accountID uuid.UUID, op Operation, alertInTx bool) (HoldVerdict, error) {
	if tenantID == uuid.Nil || personID == uuid.Nil || accountID == uuid.Nil || !op.Valid() {
		// A structurally invalid call is a caller bug: deny it. There is no tenant to attribute an alert to.
		return HoldVerdict{EvalFailed: true}, nil
	}
	held, err := holdProbe(ctx, tx, tenantID, personID, accountID, op)
	if err != nil {
		if !alertInTx {
			return HoldVerdict{EvalFailed: true}, nil
		}
		return HoldVerdict{EvalFailed: true}, raiseEvalFailed(ctx, tx, tenantID, op)
	}
	if !held {
		return HoldVerdict{}, nil
	}
	v := HoldVerdict{Held: true}
	// The matched hold id is evidence for the AML-scoped decision log. Its lookup is best effort: the operation is
	// already denied, so a failure here changes nothing but the evidence.
	if id, found, mErr := matchHold(ctx, tx, tenantID, personID, accountID, op); mErr == nil {
		if found {
			v.HoldID = id
		} else {
			v.ScreeningOnly = true
		}
	}
	return v, nil
}

// RaiseHoldEvalFailedDetached raises aml.hold_eval_failed in its own transaction after the caller's transaction ended (see
// EvaluateHoldDeferringAlert). Detached and retried by the alerting layer; the returned error is for logging only.
func RaiseHoldEvalFailedDetached(ctx context.Context, pool *db.Pool, tenantID uuid.UUID, op Operation) error {
	return alerting.RaiseDetached(ctx, alerting.NewTenantRunner(pool, tenantID), alerting.Alert{
		Kind:            alerting.KindAMLHoldEvalFailed,
		SubjectTenantID: tenantID,
		Discriminator:   "hold_eval:" + string(op),
		Attributes:      map[string]alerting.AttrValue{"operation": string(op)},
	})
}

// probeHoldActive runs SAVEPOINT; set_config('statement_timeout', ..., true); SELECT aml_hold_active(...); ROLLBACK TO
// SAVEPOINT; RELEASE SAVEPOINT as ONE pipelined round trip. The rollback undoes the set_config, so the caller's own timeouts
// are untouched, and ANY failure inside the savepoint (a missing function, a permission error, a timeout) is contained: the
// caller's transaction stays usable.
//
// The pipeline is a pgconn batch of UNNAMED statements (Parse/Bind/Describe/Execute per statement, one Sync at the end): the
// server processes it strictly in order, so the SAVEPOINT has EXECUTED before the hold read is even parsed, and a failure at
// any later stage (Parse, Bind or execution) lands inside the savepoint. It must NOT be a pgx.Batch: in pgx's default
// cache-statement mode a pgx.Batch first prepares every statement that is not yet in the connection's statement cache
// (a fresh connection, an evicted or invalidated entry) in a SEPARATE Parse/Describe + Sync exchange before anything
// executes, so a Parse failure there (aml_hold_active not resolvable) would abort the caller's transaction with no savepoint
// to roll back to (ISO-LATENCY-1 security review; pinned by TestHoldProbe_ParseFailure_*).
func probeHoldActive(ctx context.Context, tx pgx.Tx, tenantID, personID, accountID uuid.UUID, op Operation) (bool, error) {
	const sp = "aml_hold_probe"
	conn := tx.Conn()
	if conn == nil || conn.PgConn() == nil {
		return false, fmt.Errorf("aml: hold probe: transaction has no connection")
	}
	b := &pgconn.Batch{}
	b.ExecParams("SAVEPOINT "+sp, nil, nil, nil, nil)
	b.ExecParams(`SELECT set_config('statement_timeout', $1, true)`,
		[][]byte{[]byte(strconv.FormatInt(HoldEvalTimeout.Milliseconds(), 10))}, []uint32{pgtype.TextOID}, nil, nil)
	b.ExecParams(`SELECT aml_hold_active($1, $2, $3, $4)`,
		[][]byte{[]byte(tenantID.String()), []byte(personID.String()), []byte(accountID.String()), []byte(string(op))},
		[]uint32{pgtype.UUIDOID, pgtype.UUIDOID, pgtype.UUIDOID, pgtype.TextOID}, nil, nil)
	b.ExecParams("ROLLBACK TO SAVEPOINT "+sp, nil, nil, nil, nil)
	b.ExecParams("RELEASE SAVEPOINT "+sp, nil, nil, nil, nil)
	results, runErr := conn.PgConn().ExecBatch(ctx, b).ReadAll()
	if runErr == nil {
		// Every statement ran, so the savepoint is already rolled back and released: a decode failure must NOT attempt the
		// recovery below (ROLLBACK TO a savepoint that no longer exists would itself abort the caller's transaction).
		var held bool
		if err := holdProbeResult(results, &held); err != nil {
			return false, fmt.Errorf("aml: hold probe: %w", err)
		}
		return held, nil
	}
	// Either the savepoint is still open (the batch stopped at the failing statement) or the batch failed before the
	// savepoint existed (the caller's transaction was already aborted, or the connection is gone): roll back to it so the
	// caller's transaction is usable, then release it. If even that fails the transaction is unusable and the caller's next
	// statement fails, which is itself fail-closed.
	_, _ = tx.Exec(ctx, "ROLLBACK TO SAVEPOINT "+sp)
	_, _ = tx.Exec(ctx, "RELEASE SAVEPOINT "+sp)
	return false, fmt.Errorf("aml: hold probe: %w", runErr)
}

// holdProbeResult checks the five results of the probe pipeline and decodes the hold read (text format boolean). Anything
// unexpected is an error, which the caller turns into a deny.
func holdProbeResult(results []*pgconn.Result, held *bool) error {
	if len(results) != 5 {
		return fmt.Errorf("probe pipeline returned %d results, want 5", len(results))
	}
	for _, r := range results {
		if r.Err != nil {
			return r.Err
		}
	}
	rows := results[2].Rows
	if len(rows) != 1 || len(rows[0]) != 1 || rows[0][0] == nil {
		return fmt.Errorf("aml_hold_active returned no single non-NULL value")
	}
	switch string(rows[0][0]) {
	case "t":
		*held = true
	case "f":
		*held = false
	default:
		return fmt.Errorf("aml_hold_active returned an unexpected value")
	}
	return nil
}

func matchHold(ctx context.Context, tx pgx.Tx, tenantID, personID, accountID uuid.UUID, op Operation) (uuid.UUID, bool, error) {
	var id *uuid.UUID
	sp, err := tx.Begin(ctx)
	if err != nil {
		return uuid.Nil, false, err
	}
	qErr := sp.QueryRow(ctx, `SELECT aml_hold_match($1, $2, $3, $4)`, tenantID, personID, accountID, string(op)).Scan(&id)
	_ = sp.Rollback(ctx)
	if qErr != nil {
		return uuid.Nil, false, qErr
	}
	if id == nil {
		// The probe said held, the match found no hold row: the screening gate (or a hold lifted in between).
		return uuid.Nil, false, nil
	}
	return *id, true, nil
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
