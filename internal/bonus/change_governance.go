// Four-eyes governance infrastructure (docs/security/security-
// architecture.md "security doc" §B1.2/§B1.3), backed by migration
// 0063's bonus_change_requests/bonus_change_approvals/
// bonus_approval_policies tables. Deferred by Phase 2 ("bonus_change_
// requests/bonus_change_approvals - four-eyes governance
// infrastructure"), built here.
//
// This file is deliberately thin: the actual dual-control enforcement
// (governance trigger, SEP-1 trigger, the single atomic consume function)
// lives in the database (migration 0063), per the security doc's own
// "trigger-based, mirroring 0044/0047" instruction and CLAUDE.md's "not
// by discipline in application code" rule. This file is the Go-side
// repository for filing/approving requests and invoking the consume
// function - it enforces NOTHING itself beyond what the database already
// guarantees.
package bonus

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

// ChangeOperation is bonus_change_requests.operation's closed set
// (security doc §B1.2's seven dual-controlled operations plus doc 34
// §3.1's held_disposition_resolve).
type ChangeOperation string

const (
	ChangeOpManualGrantIssue       ChangeOperation = "manual_grant_issue"
	ChangeOpBulkJobExecute         ChangeOperation = "bulk_job_execute"
	ChangeOpBonusAdjustmentWrite   ChangeOperation = "bonus_adjustment_write"
	ChangeOpGrantForcedConversion  ChangeOperation = "grant_forced_conversion"
	ChangeOpCampaignActivate       ChangeOperation = "campaign_activate"
	ChangeOpOfferPublish           ChangeOperation = "offer_publish"
	ChangeOpGrantCancelCompleted   ChangeOperation = "grant_cancel_completed"
	ChangeOpHeldDispositionResolve ChangeOperation = "held_disposition_resolve"
)

// ChangeRequestState is bonus_change_requests.state's closed set.
type ChangeRequestState string

const (
	ChangeRequestPending   ChangeRequestState = "pending"
	ChangeRequestApplied   ChangeRequestState = "applied"
	ChangeRequestRejected  ChangeRequestState = "rejected"
	ChangeRequestCancelled ChangeRequestState = "cancelled"
)

// ChangeRequest mirrors one bonus_change_requests row.
type ChangeRequest struct {
	ID                     uuid.UUID
	TenantID               uuid.UUID
	BrandID                *uuid.UUID
	Operation              ChangeOperation
	TargetType             string
	TargetID               uuid.UUID
	Payload                []byte
	AmountAtRequest        *big.Int
	AssetCode              *string
	ReasonCode             string
	RequestedByPrincipalID uuid.UUID
	RequestedAt            time.Time
	State                  ChangeRequestState
	AppliedByPrincipalID   *uuid.UUID
	AppliedAt              *time.Time
}

const changeRequestColumns = `
	id, tenant_id, brand_id, operation, target_type, target_id, payload,
	amount_at_request, asset_code, reason_code, requested_by_principal_id, requested_at,
	state, applied_by_principal_id, applied_at`

func scanChangeRequest(row rowScanner) (ChangeRequest, error) {
	var (
		r         ChangeRequest
		operation string
		state     string
		// FIX (Stage 4H-B1 Wave 3 Phase 6, `security`): amount_at_request
		// is NUMERIC(38,0) and MUST be scanned through pgtype.Numeric,
		// exactly like every other monetary column in this package
		// (numeric.go, offer.go, bulk_grant.go, progress.go). Scanning it
		// straight into a **big.Int - as this function previously did -
		// works only while the column is NULL: pgx short-circuits NULL
		// before reaching the numeric codec, but any non-NULL value fails
		// with "cannot scan numeric (OID 1700) in binary format into
		// **big.Int". Every existing caller happened to file amount-less
		// requests, so this never fired in a test; the consequence was
		// that filing a change request WITH an amount - i.e. precisely the
		// above-threshold requests whose amount is the load-bearing input
		// to the approval threshold and to the append-only
		// bonus_change_approvals forensic record - failed outright with a
		// 500 at the RETURNING scan, and no request could ever carry a
		// non-NULL amount_at_request.
		amountAtRequest pgtype.Numeric
	)
	err := row.Scan(
		&r.ID, &r.TenantID, &r.BrandID, &operation, &r.TargetType, &r.TargetID, &r.Payload,
		&amountAtRequest, &r.AssetCode, &r.ReasonCode, &r.RequestedByPrincipalID, &r.RequestedAt,
		&state, &r.AppliedByPrincipalID, &r.AppliedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ChangeRequest{}, ErrNotFound
	}
	if err != nil {
		return ChangeRequest{}, fmt.Errorf("bonus: scan change request: %w", err)
	}
	amount, err := numericToBigInt(amountAtRequest)
	if err != nil {
		return ChangeRequest{}, err
	}
	r.AmountAtRequest = amount
	r.Operation = ChangeOperation(operation)
	r.State = ChangeRequestState(state)
	return r, nil
}

// FileChangeRequest inserts a new bonus_change_requests row in state
// 'pending' (security doc §B1.2). ReasonCode is mandatory (CLAUDE.md).
func FileChangeRequest(ctx context.Context, tx pgx.Tx, r ChangeRequest) (ChangeRequest, error) {
	if r.ID == uuid.Nil {
		r.ID = uuid.New()
	}
	if r.ReasonCode == "" {
		return ChangeRequest{}, fmt.Errorf("bonus: a change request requires a reason code")
	}
	row := tx.QueryRow(ctx, `
		INSERT INTO bonus_change_requests (
			id, tenant_id, brand_id, operation, target_type, target_id, payload,
			amount_at_request, asset_code, reason_code, requested_by_principal_id
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		RETURNING `+changeRequestColumns,
		r.ID, r.TenantID, r.BrandID, string(r.Operation), r.TargetType, r.TargetID, nonNilJSON(r.Payload),
		bigIntToNumericPtr(r.AmountAtRequest), r.AssetCode, r.ReasonCode, r.RequestedByPrincipalID,
	)
	return scanChangeRequest(row)
}

func bigIntToNumericPtr(v *big.Int) any {
	if v == nil {
		return nil
	}
	return bigIntToNumeric(v)
}

// GetChangeRequestByID looks up a ChangeRequest by id.
func GetChangeRequestByID(ctx context.Context, tx pgx.Tx, id uuid.UUID) (ChangeRequest, error) {
	row := tx.QueryRow(ctx, `SELECT `+changeRequestColumns+` FROM bonus_change_requests WHERE id = $1`, id)
	return scanChangeRequest(row)
}

// ListChangeRequestsPage returns a page of ChangeRequests for tenantID,
// optionally filtered by state, most recently requested first, plus the
// total count matching the filter - Stage 5 (Operator Back Office MVP)'s
// own read surface for the four-eyes approval queue. The caller (HTTP
// handler) is responsible for defaulting an absent filter to
// ChangeRequestPending so operators land on their actual queue - this
// function itself applies no default, mirroring every other List*Page
// function in this package (an explicit nil state means "no filter").
func ListChangeRequestsPage(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, state *ChangeRequestState, limit, offset int) ([]ChangeRequest, int, error) {
	where := "tenant_id = $1"
	args := []any{tenantID}
	if state != nil {
		args = append(args, string(*state))
		where += fmt.Sprintf(" AND state = $%d", len(args))
	}

	var total int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM bonus_change_requests WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("bonus: count change requests: %w", err)
	}

	pageArgs := append(append([]any{}, args...), limit, offset)
	sql := `SELECT ` + changeRequestColumns + ` FROM bonus_change_requests WHERE ` + where +
		fmt.Sprintf(` ORDER BY requested_at DESC LIMIT $%d OFFSET $%d`, len(pageArgs)-1, len(pageArgs))
	rows, err := tx.Query(ctx, sql, pageArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("bonus: list change requests page: %w", err)
	}
	defer rows.Close()
	var out []ChangeRequest
	for rows.Next() {
		r, err := scanChangeRequest(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// RecordChangeApproval inserts a bonus_change_approvals row - the write
// that fires migration 0063's governance (four-eyes) AND SEP-1 triggers.
// A trigger-raised exception surfaces here as a plain Postgres error;
// callers should not attempt to interpret it beyond "this approval was
// refused" (the exception message names the specific reason for a human
// reading logs, but this package does not parse it into a typed
// sentinel, mirroring how internal/withdrawal treats the identical
// migration-0034 trigger's errors).
func RecordChangeApproval(ctx context.Context, tx pgx.Tx, tenantID, requestID, approverPrincipalID uuid.UUID, decision string, reasonCode *string, thresholdAtDecision, amountAtDecision *big.Int) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO bonus_change_approvals (id, tenant_id, request_id, approver_principal_id, decision, reason_code, threshold_at_decision, amount_at_decision)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7)`,
		tenantID, requestID, approverPrincipalID, decision, reasonCode, bigIntToNumericPtr(thresholdAtDecision), bigIntToNumericPtr(amountAtDecision),
	)
	if err != nil {
		return fmt.Errorf("bonus: record change approval: %w", err)
	}
	return nil
}

// ErrChangeRequestNotApproved is returned by ConsumeApprovedChangeRequest
// when no pending request with sufficient distinct approvals (and no
// reject) matches - the request is not consumable yet, or was never
// filed, or the payload does not match exactly.
var ErrChangeRequestNotApproved = errors.New("bonus: no pending, sufficiently-approved bonus_change_requests row matches this operation/target/payload")

// ConsumeApprovedChangeRequest invokes migration 0063's single consume
// function, bonus_change_consume_approved_request - the ONE place a
// pending request transitions to 'applied', atomically, exactly once,
// per security doc item 4/5's own binding discipline (AFTER trigger
// semantics do not apply here since this is an explicit function call,
// not an upsert-triggered path, but the same "consume exactly once, in
// the same statement as the state transition" property holds by this
// function's own construction).
func ConsumeApprovedChangeRequest(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, operation ChangeOperation, targetID uuid.UUID, payloadMatch []byte, requiredApprovals int32, appliedBy uuid.UUID) (uuid.UUID, error) {
	var requestID uuid.UUID
	err := tx.QueryRow(ctx,
		`SELECT bonus_change_consume_approved_request($1, $2, $3, $4, $5, $6)`,
		tenantID, string(operation), targetID, nonNilJSON(payloadMatch), requiredApprovals, appliedBy,
	).Scan(&requestID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: %v", ErrChangeRequestNotApproved, err)
	}
	return requestID, nil
}

// ApprovalPolicy mirrors one bonus_approval_policies row.
type ApprovalPolicy struct {
	Operation              ChangeOperation
	ApprovalThresholdMinor *big.Int
	RequiredApprovals      int32
}

// fallbackApprovalPolicy mirrors internal/withdrawal/policy.go's own
// fail-closed default (security doc §B1.2 item 6): threshold 0 (every
// amount requires four-eyes), required_approvals 2.
func fallbackApprovalPolicy(operation ChangeOperation) ApprovalPolicy {
	return ApprovalPolicy{Operation: operation, ApprovalThresholdMinor: big.NewInt(0), RequiredApprovals: 2}
}

// ResolveApprovalPolicy reads the most specific, most recently effective
// bonus_approval_policies row for (tenant, operation, brand, asset),
// falling back to the tenant-wide/asset-agnostic row, then to
// fallbackApprovalPolicy if none resolves at all - "fail closed when no
// row resolves" (security doc item 6).
func ResolveApprovalPolicy(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, operation ChangeOperation, brandID *uuid.UUID, assetCode *string) (ApprovalPolicy, error) {
	row := tx.QueryRow(ctx, `
		SELECT approval_threshold_minor_units, required_approvals
		  FROM bonus_approval_policies
		 WHERE tenant_id = $1 AND operation = $2
		   AND (brand_id IS NULL OR brand_id = $3)
		   AND (asset_code IS NULL OR asset_code = $4)
		   AND effective_from <= clock_timestamp()
		 ORDER BY (brand_id IS NOT NULL) DESC, (asset_code IS NOT NULL) DESC, effective_from DESC
		 LIMIT 1`,
		tenantID, string(operation), brandID, assetCode,
	)
	var threshold pgtype.Numeric
	var required int32
	if err := row.Scan(&threshold, &required); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fallbackApprovalPolicy(operation), nil
		}
		return ApprovalPolicy{}, fmt.Errorf("bonus: resolve approval policy: %w", err)
	}
	amt, err := numericToBigInt(threshold)
	if err != nil {
		return ApprovalPolicy{}, err
	}
	if amt == nil {
		amt = big.NewInt(0)
	}
	return ApprovalPolicy{Operation: operation, ApprovalThresholdMinor: amt, RequiredApprovals: required}, nil
}
