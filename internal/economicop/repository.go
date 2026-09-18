package economicop

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// ErrNotFound is returned when an EconomicOperation does not exist (or
// is not visible under the caller's current RLS scope).
var ErrNotFound = errors.New("economicop: operation not found")

// numericToBigInt mirrors internal/risk/cumulative.go's own helper of
// the same name - this codebase's established, per-package convention
// for crossing the pgtype.Numeric -> *big.Int boundary (NUMERIC(38,0)
// columns are never scanned into int64/float64, CLAUDE.md).
//
// A POSITIVE Exp is normal and must be handled, NOT rejected: PostgreSQL
// returns a NUMERIC value in whatever scale it likes, so an exact integer
// of 1000 can arrive as Int=1, Exp=3 (internal/risk/cumulative.go's own
// doc comment names this exactly, having once gotten it backwards the
// same way and turned it into a fail-closed availability defect on any
// round number). A NEGATIVE Exp IS refused: a fractional count of minor
// units cannot exist in a NUMERIC(38,0) schema.
func numericToBigInt(n pgtype.Numeric) (*big.Int, error) {
	if !n.Valid || n.Int == nil {
		return nil, nil
	}
	if n.NaN || n.InfinityModifier != pgtype.Finite {
		return nil, fmt.Errorf("economicop: NUMERIC value is not a finite number")
	}
	if n.Exp < 0 {
		return nil, fmt.Errorf("economicop: refusing a fractional NUMERIC minor-unit value (exponent %d)", n.Exp)
	}
	if n.Exp == 0 {
		return n.Int, nil
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n.Exp)), nil)
	return new(big.Int).Mul(n.Int, scale), nil
}

func bigIntToNumeric(v *big.Int) pgtype.Numeric {
	if v == nil {
		return pgtype.Numeric{Valid: false}
	}
	return pgtype.Numeric{Int: new(big.Int).Set(v), Exp: 0, Valid: true}
}

const operationColumns = `
	operation_id, tenant_id, brand_id, operation_type,
	initiating_actor_type, initiating_actor_id, initiating_principal_id,
	subject_scope, subject_ref, subject_set_hash, subject_definition_hash, subject_set_count, beneficiary_class,
	economic_owner, asset_code, intended_aggregate_value, recipient_ceiling, per_window_ceiling, ceiling_window, value_measure_basis,
	parent_operation_id, root_operation_id, lineage_kind, batch_ordinal, batch_total,
	approval_state, required_approvals, approvals_received, threshold_at_decision, required_approvals_at_decision, approval_refs, pinned_payload,
	idempotency_key, correlation_id, audit_record_id, created_at, created_by,
	status, expires_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanOperation(row rowScanner) (EconomicOperation, error) {
	var (
		op                  EconomicOperation
		operationType       string
		subjectScope        string
		beneficiaryClass    *string
		intendedAggregate   pgtype.Numeric
		ceilingWindow       pgtype.Interval
		lineageKind         string
		approvalState       string
		thresholdAtDecision pgtype.Numeric
		approvalRefs        []uuid.UUID
		pinnedPayload       []byte
		valueMeasureBasis   []byte
		status              string
	)
	err := row.Scan(
		&op.OperationID, &op.TenantID, &op.BrandID, &operationType,
		&op.InitiatingActorType, &op.InitiatingActorID, &op.InitiatingPrincipalID,
		&subjectScope, &op.SubjectRef, &op.SubjectSetHash, &op.SubjectDefinitionHash, &op.SubjectSetCount, &beneficiaryClass,
		&op.EconomicOwner, &op.AssetCode, &intendedAggregate, &op.RecipientCeiling, &op.PerWindowCeiling, &ceilingWindow, &valueMeasureBasis,
		&op.ParentOperationID, &op.RootOperationID, &lineageKind, &op.BatchOrdinal, &op.BatchTotal,
		&approvalState, &op.RequiredApprovals, &op.ApprovalsReceived, &thresholdAtDecision, &op.RequiredApprovalsAtDecision, &approvalRefs, &pinnedPayload,
		&op.IdempotencyKey, &op.CorrelationID, &op.AuditRecordID, &op.CreatedAt, &op.CreatedBy,
		&status, &op.ExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return EconomicOperation{}, ErrNotFound
	}
	if err != nil {
		return EconomicOperation{}, fmt.Errorf("economicop: scan operation: %w", err)
	}

	op.OperationType = OperationType(operationType)
	op.SubjectScope = SubjectScope(subjectScope)
	if beneficiaryClass != nil {
		bc := BeneficiaryClass(*beneficiaryClass)
		op.BeneficiaryClass = &bc
	}
	op.LineageKind = LineageKind(lineageKind)
	op.ApprovalState = ApprovalState(approvalState)
	op.Status = Status(status)
	op.ApprovalRefs = approvalRefs
	op.PinnedPayload = pinnedPayload
	op.ValueMeasureBasis = valueMeasureBasis

	if intendedAggregate.Valid {
		v, err := numericToBigInt(intendedAggregate)
		if err != nil {
			return EconomicOperation{}, err
		}
		op.IntendedAggregateValue = v
	}
	if thresholdAtDecision.Valid {
		v, err := numericToBigInt(thresholdAtDecision)
		if err != nil {
			return EconomicOperation{}, err
		}
		op.ThresholdAtDecision = v
	}
	// per_window_ceiling's paired window (doc 34 §2.2). Only a pure
	// microsecond-denominated interval (no months/days component) is
	// representable as a time.Duration - this package does not need
	// calendar-aware windows for Phase 2's schema/plumbing scope, and
	// refusing to silently misinterpret Days/Months is safer than
	// guessing 24h/30d. Not populated (left nil) rather than erroring: a
	// scan-time hard failure over a field no Phase 2 caller yet writes
	// would be disproportionate.
	if ceilingWindow.Valid && ceilingWindow.Days == 0 && ceilingWindow.Months == 0 {
		d := time.Duration(ceilingWindow.Microseconds) * time.Microsecond
		op.CeilingWindow = &d
	}

	return op, nil
}

// Create inserts a new EconomicOperation row. Callers are responsible
// for: resolving TenantID from authenticated server-side context only
// (CLAUDE.md, never client-supplied); for a root operation, setting
// LineageKind = LineageRoot, ParentOperationID = nil, and
// RootOperationID = OperationID (the schema's own CHECK enforces this
// pairing, doc 34 §2.2); for a non-root operation, resolving
// RootOperationID from the parent it inherits from (doc 34 §2.2: "the
// field the whole enforcement mechanism turns on"). This function
// performs no minting/enforcement logic of its own (doc 34 §5 is
// explicitly Phase 3) - it is a plain insert.
func Create(ctx context.Context, tx pgx.Tx, op EconomicOperation) (EconomicOperation, error) {
	if op.OperationID == uuid.Nil {
		op.OperationID = uuid.New()
	}
	if op.LineageKind == LineageRoot && op.RootOperationID == uuid.Nil {
		op.RootOperationID = op.OperationID
	}

	row := tx.QueryRow(ctx, `
		INSERT INTO economic_operations (
			operation_id, tenant_id, brand_id, operation_type,
			initiating_actor_type, initiating_actor_id, initiating_principal_id,
			subject_scope, subject_ref, subject_set_hash, subject_definition_hash, subject_set_count, beneficiary_class,
			economic_owner, asset_code, intended_aggregate_value, recipient_ceiling, per_window_ceiling, ceiling_window, value_measure_basis,
			parent_operation_id, root_operation_id, lineage_kind, batch_ordinal, batch_total,
			approval_state, required_approvals, approvals_received, threshold_at_decision, required_approvals_at_decision, approval_refs, pinned_payload,
			idempotency_key, correlation_id, audit_record_id, created_by,
			status, expires_at
		) VALUES (
			$1, $2, $3, $4,
			$5, $6, $7,
			$8, $9, $10, $11, $12, $13,
			$14, $15, $16, $17, $18, $19, $20,
			$21, $22, $23, $24, $25,
			$26, $27, $28, $29, $30, $31, $32,
			$33, $34, $35, $36,
			$37, $38
		) RETURNING `+operationColumns,
		op.OperationID, op.TenantID, op.BrandID, string(op.OperationType),
		op.InitiatingActorType, op.InitiatingActorID, op.InitiatingPrincipalID,
		string(op.SubjectScope), op.SubjectRef, op.SubjectSetHash, op.SubjectDefinitionHash, op.SubjectSetCount, beneficiaryClassPtr(op.BeneficiaryClass),
		op.EconomicOwner, op.AssetCode, bigIntToNumeric(op.IntendedAggregateValue), op.RecipientCeiling, op.PerWindowCeiling, durationToInterval(op.CeilingWindow), nonNilJSON(op.ValueMeasureBasis),
		op.ParentOperationID, op.RootOperationID, string(op.LineageKind), op.BatchOrdinal, op.BatchTotal,
		string(op.ApprovalState), op.RequiredApprovals, op.ApprovalsReceived, bigIntToNumeric(op.ThresholdAtDecision), op.RequiredApprovalsAtDecision, nonNilUUIDs(op.ApprovalRefs), nonNilJSON(op.PinnedPayload),
		op.IdempotencyKey, op.CorrelationID, op.AuditRecordID, op.CreatedBy,
		string(op.Status), op.ExpiresAt,
	)
	return scanOperation(row)
}

func beneficiaryClassPtr(bc *BeneficiaryClass) *string {
	if bc == nil {
		return nil
	}
	s := string(*bc)
	return &s
}

func nonNilJSON(b []byte) []byte {
	if b == nil {
		return []byte("{}")
	}
	return b
}

// nonNilUUIDs coerces a nil []uuid.UUID to an empty, non-nil slice. A nil
// slice bound as a UUID[] parameter is sent as SQL NULL (not the
// column's own '{}' DEFAULT, which only applies when the column is
// omitted from the INSERT's column list entirely) - approval_refs is
// NOT NULL, so a caller-supplied nil Go slice must be normalized before
// binding.
func nonNilUUIDs(u []uuid.UUID) []uuid.UUID {
	if u == nil {
		return []uuid.UUID{}
	}
	return u
}

func durationToInterval(d *time.Duration) *pgtype.Interval {
	if d == nil {
		return nil
	}
	return &pgtype.Interval{Microseconds: d.Microseconds(), Valid: true}
}

// GetByID looks up an EconomicOperation by id within the caller's
// current RLS scope.
func GetByID(ctx context.Context, tx pgx.Tx, operationID uuid.UUID) (EconomicOperation, error) {
	row := tx.QueryRow(ctx, `SELECT `+operationColumns+` FROM economic_operations WHERE operation_id = $1`, operationID)
	return scanOperation(row)
}

// GetByIdempotencyKey resolves the mint-once key (doc 34 §3.3): a
// retried minting call looks this up first and, on a hit, returns the
// SAME EOI rather than creating a second one. Phase 3's job to call this
// before Create; this package does not itself enforce mint-once beyond
// the DB-level UNIQUE (tenant_id, idempotency_key) constraint.
func GetByIdempotencyKey(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, idempotencyKey string) (EconomicOperation, error) {
	row := tx.QueryRow(ctx,
		`SELECT `+operationColumns+` FROM economic_operations WHERE tenant_id = $1 AND idempotency_key = $2`,
		tenantID, idempotencyKey,
	)
	return scanOperation(row)
}

// ListByRoot returns every operation in a root's lineage subtree
// (itself plus every descendant), ordered by creation - the read every
// budget-consumption join and reconciliation sweep performs (doc 34
// §3.4: "every budget is a subtree-wide aggregate keyed on
// root_operation_id").
func ListByRoot(ctx context.Context, tx pgx.Tx, tenantID, rootOperationID uuid.UUID) ([]EconomicOperation, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+operationColumns+` FROM economic_operations WHERE tenant_id = $1 AND root_operation_id = $2 ORDER BY created_at ASC`,
		tenantID, rootOperationID,
	)
	if err != nil {
		return nil, fmt.Errorf("economicop: list by root: %w", err)
	}
	defer rows.Close()

	var out []EconomicOperation
	for rows.Next() {
		op, err := scanOperation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

// ListChildren returns the direct children of parentOperationID (doc 34
// §3.2's lineage_kind rows minted under it) - narrower than ListByRoot,
// for a caller that only needs one lineage level.
func ListChildren(ctx context.Context, tx pgx.Tx, tenantID, parentOperationID uuid.UUID) ([]EconomicOperation, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+operationColumns+` FROM economic_operations WHERE tenant_id = $1 AND parent_operation_id = $2 ORDER BY created_at ASC`,
		tenantID, parentOperationID,
	)
	if err != nil {
		return nil, fmt.Errorf("economicop: list children: %w", err)
	}
	defer rows.Close()

	var out []EconomicOperation
	for rows.Next() {
		op, err := scanOperation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

// UpdateApprovalState performs the one allowed-transition update this
// package provides: moving approval_state/approvals_received (and,
// optionally, status) forward. It is a plain, unconditional UPDATE - no
// four-eyes/threshold/SEP-1 enforcement, no locking consume (doc 34
// §5.4's atomic budget/approval consumption is explicitly Phase 3's job,
// requiring the canonical lock ordering §5.3 specifies). Callers that
// need the locking consume must implement it themselves against this
// row, e.g. via a SELECT ... FOR UPDATE performed before calling this
// function within the same transaction.
func UpdateApprovalState(ctx context.Context, tx pgx.Tx, tenantID, operationID uuid.UUID, newState ApprovalState, approvalsReceived int32, newStatus Status) (EconomicOperation, error) {
	row := tx.QueryRow(ctx, `
		UPDATE economic_operations
		SET approval_state = $3, approvals_received = $4, status = $5
		WHERE tenant_id = $1 AND operation_id = $2
		RETURNING `+operationColumns,
		tenantID, operationID, string(newState), approvalsReceived, string(newStatus),
	)
	return scanOperation(row)
}
