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

	"github.com/Diansalas/igaming-platform/internal/db"
)

// BulkGrantJobTargetKind is bulk_grant_jobs.target_kind's closed set
// (doc 10 §W5, widened by §N2.2's segment_set mode).
type BulkGrantJobTargetKind string

const (
	TargetSinglePlayer BulkGrantJobTargetKind = "single_player"
	TargetPlayerList   BulkGrantJobTargetKind = "player_list"
	TargetSegment      BulkGrantJobTargetKind = "segment"
	TargetSegmentSet   BulkGrantJobTargetKind = "segment_set"
)

// BulkGrantJobApprovalState is bulk_grant_jobs.approval_state's closed
// set (doc 10 §W5's four-eyes gate).
type BulkGrantJobApprovalState string

const (
	BulkApprovalPendingFourEyes BulkGrantJobApprovalState = "pending_four_eyes"
	BulkApprovalApproved        BulkGrantJobApprovalState = "approved"
	BulkApprovalRejected        BulkGrantJobApprovalState = "rejected"
)

// BulkGrantJobStatus is bulk_grant_jobs.status's closed set.
type BulkGrantJobStatus string

const (
	BulkJobQueued             BulkGrantJobStatus = "queued"
	BulkJobRunning            BulkGrantJobStatus = "running"
	BulkJobCompleted          BulkGrantJobStatus = "completed"
	BulkJobPartiallyCompleted BulkGrantJobStatus = "partially_completed"
	BulkJobFailed             BulkGrantJobStatus = "failed"
)

// SegmentSetEntry is one (segment_id, segment_version_id) pair in a
// segment_set target (doc 10 §N2.2). Resolution semantics (union, live
// at run time) are Phase 3's job - this package only stores the pinned
// reference list.
type SegmentSetEntry struct {
	SegmentID        uuid.UUID
	SegmentVersionID uuid.UUID
}

// BulkGrantJob mirrors one bulk_grant_jobs row (doc 10 §W5, widened by
// §N2.2/§N2.4a).
type BulkGrantJob struct {
	ID             uuid.UUID
	TenantID       uuid.UUID
	BrandID        uuid.UUID
	CampaignID     uuid.UUID
	OfferVersionID uuid.UUID

	TargetKind            BulkGrantJobTargetKind
	TargetPlayerAccountID *uuid.UUID
	TargetPlayerList      []uuid.UUID
	TargetSegment         SegmentReference
	TargetSegmentSet      []byte // JSONB array of SegmentSetEntry, opaque to this package

	RequestedByPrincipalID uuid.UUID
	RequestedAt            time.Time
	ApprovalState          BulkGrantJobApprovalState
	Status                 BulkGrantJobStatus
	IdempotencyKey         string

	ParentOperationID       *uuid.UUID
	OriginatingSuggestionID *uuid.UUID

	CreatedAt time.Time
}

const bulkGrantJobColumns = `
	id, tenant_id, brand_id, campaign_id, offer_version_id,
	target_kind, target_player_account_id, target_player_list, target_segment_id, target_segment_version_id, target_segment_set,
	requested_by_principal_id, requested_at, approval_state, status, idempotency_key,
	parent_operation_id, originating_suggestion_id, created_at`

func scanBulkGrantJob(row rowScanner) (BulkGrantJob, error) {
	var (
		j             BulkGrantJob
		targetKind    string
		approvalState string
		status        string
	)
	err := row.Scan(
		&j.ID, &j.TenantID, &j.BrandID, &j.CampaignID, &j.OfferVersionID,
		&targetKind, &j.TargetPlayerAccountID, &j.TargetPlayerList, &j.TargetSegment.SegmentID, &j.TargetSegment.SegmentVersionID, &j.TargetSegmentSet,
		&j.RequestedByPrincipalID, &j.RequestedAt, &approvalState, &status, &j.IdempotencyKey,
		&j.ParentOperationID, &j.OriginatingSuggestionID, &j.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return BulkGrantJob{}, ErrNotFound
	}
	if err != nil {
		return BulkGrantJob{}, fmt.Errorf("bonus: scan bulk grant job: %w", err)
	}
	j.TargetKind = BulkGrantJobTargetKind(targetKind)
	j.ApprovalState = BulkGrantJobApprovalState(approvalState)
	j.Status = BulkGrantJobStatus(status)
	return j, nil
}

// CreateBulkGrantJob inserts a new bulk_grant_jobs row in status
// 'queued'/'pending_four_eyes'. The (tenant_id, idempotency_key) unique
// constraint is the actual "a resubmission of the identical job spec is
// a no-op" enforcement (doc 10 §W5) - callers should look up by
// idempotency key first (GetBulkGrantJobByIdempotencyKey) rather than
// relying on this insert to fail gracefully.
func CreateBulkGrantJob(ctx context.Context, tx pgx.Tx, j BulkGrantJob) (BulkGrantJob, error) {
	if j.ID == uuid.Nil {
		j.ID = uuid.New()
	}
	if j.ApprovalState == "" {
		j.ApprovalState = BulkApprovalPendingFourEyes
	}
	if j.Status == "" {
		j.Status = BulkJobQueued
	}
	row := tx.QueryRow(ctx, `
		INSERT INTO bulk_grant_jobs (
			id, tenant_id, brand_id, campaign_id, offer_version_id,
			target_kind, target_player_account_id, target_player_list, target_segment_id, target_segment_version_id, target_segment_set,
			requested_by_principal_id, approval_state, status, idempotency_key,
			parent_operation_id, originating_suggestion_id
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
		RETURNING `+bulkGrantJobColumns,
		j.ID, j.TenantID, j.BrandID, j.CampaignID, j.OfferVersionID,
		string(j.TargetKind), j.TargetPlayerAccountID, j.TargetPlayerList, j.TargetSegment.SegmentID, j.TargetSegment.SegmentVersionID, j.TargetSegmentSet,
		j.RequestedByPrincipalID, string(j.ApprovalState), string(j.Status), j.IdempotencyKey,
		j.ParentOperationID, j.OriginatingSuggestionID,
	)
	return scanBulkGrantJob(row)
}

// GetBulkGrantJobByID looks up a BulkGrantJob by id.
func GetBulkGrantJobByID(ctx context.Context, tx pgx.Tx, id uuid.UUID) (BulkGrantJob, error) {
	row := tx.QueryRow(ctx, `SELECT `+bulkGrantJobColumns+` FROM bulk_grant_jobs WHERE id = $1`, id)
	return scanBulkGrantJob(row)
}

// GetBulkGrantJobByIdempotencyKey resolves a resubmission to its
// existing job (doc 10 §W5: "a resubmission of the identical job spec is
// a no-op against the same key, never a second job").
func GetBulkGrantJobByIdempotencyKey(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, idempotencyKey string) (BulkGrantJob, error) {
	row := tx.QueryRow(ctx, `SELECT `+bulkGrantJobColumns+` FROM bulk_grant_jobs WHERE tenant_id = $1 AND idempotency_key = $2`, tenantID, idempotencyKey)
	return scanBulkGrantJob(row)
}

// ListBulkGrantJobsByStatus returns every BulkGrantJob in a given
// status, oldest first - the job-runner's own resumability read pattern.
func ListBulkGrantJobsByStatus(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, status BulkGrantJobStatus) ([]BulkGrantJob, error) {
	rows, err := tx.Query(ctx, `SELECT `+bulkGrantJobColumns+` FROM bulk_grant_jobs WHERE tenant_id = $1 AND status = $2 ORDER BY requested_at ASC`, tenantID, string(status))
	if err != nil {
		return nil, fmt.Errorf("bonus: list bulk grant jobs: %w", err)
	}
	defer rows.Close()
	var out []BulkGrantJob
	for rows.Next() {
		j, err := scanBulkGrantJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// UpdateBulkGrantJobApproval records the four-eyes approve/reject
// decision (doc 10 §W5) - no threshold/four-eyes enforcement itself
// (Phase 3's job; security-architecture.md §B1.2 item 2's "always,
// regardless of per-player value" rule).
func UpdateBulkGrantJobApproval(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, state BulkGrantJobApprovalState) (BulkGrantJob, error) {
	row := tx.QueryRow(ctx, `UPDATE bulk_grant_jobs SET approval_state = $3 WHERE tenant_id = $1 AND id = $2 RETURNING `+bulkGrantJobColumns, tenantID, id, string(state))
	return scanBulkGrantJob(row)
}

// UpdateBulkGrantJobStatus performs an unconditional status write - the
// job-runner's own progress tracking (queued -> running -> completed/
// partially_completed/failed). No resumability logic (Phase 3's job).
func UpdateBulkGrantJobStatus(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, status BulkGrantJobStatus) (BulkGrantJob, error) {
	row := tx.QueryRow(ctx, `UPDATE bulk_grant_jobs SET status = $3 WHERE tenant_id = $1 AND id = $2 RETURNING `+bulkGrantJobColumns, tenantID, id, string(status))
	return scanBulkGrantJob(row)
}

// BulkGrantJobItemOutcome is bulk_grant_job_items.outcome's closed set
// (doc 10 §W5).
type BulkGrantJobItemOutcome string

const (
	ItemPending        BulkGrantJobItemOutcome = "pending"
	ItemIssued         BulkGrantJobItemOutcome = "issued"
	ItemDenied         BulkGrantJobItemOutcome = "denied"
	ItemAlreadyGranted BulkGrantJobItemOutcome = "already_granted"
	ItemError          BulkGrantJobItemOutcome = "error"
)

// BulkGrantJobItem mirrors one bulk_grant_job_items row - the
// per-player isolation/resumability/consumption-record primitive (doc 10
// §W5/§N2.4a).
type BulkGrantJobItem struct {
	ID              uuid.UUID
	TenantID        uuid.UUID
	BulkGrantJobID  uuid.UUID
	PlayerAccountID uuid.UUID
	Outcome         BulkGrantJobItemOutcome
	ReasonCode      *string
	GrantID         *uuid.UUID
	GrantedAmount   *big.Int // NUMERIC(38,0), the EOI recipient/value-budget consumption record (§N2.4a)
	ErrorDetail     *string
	CreatedAt       time.Time
	ProcessedAt     *time.Time
}

const bulkGrantJobItemColumns = `id, tenant_id, bulk_grant_job_id, player_account_id, outcome, reason_code, grant_id, granted_amount, error_detail, created_at, processed_at`

func scanBulkGrantJobItem(row rowScanner) (BulkGrantJobItem, error) {
	var (
		it            BulkGrantJobItem
		outcome       string
		grantedAmount pgtype.Numeric
	)
	err := row.Scan(&it.ID, &it.TenantID, &it.BulkGrantJobID, &it.PlayerAccountID, &outcome, &it.ReasonCode, &it.GrantID, &grantedAmount, &it.ErrorDetail, &it.CreatedAt, &it.ProcessedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return BulkGrantJobItem{}, ErrNotFound
	}
	if err != nil {
		return BulkGrantJobItem{}, fmt.Errorf("bonus: scan bulk grant job item: %w", err)
	}
	it.Outcome = BulkGrantJobItemOutcome(outcome)
	if grantedAmount.Valid {
		amt, err := numericToBigInt(grantedAmount)
		if err != nil {
			return BulkGrantJobItem{}, err
		}
		it.GrantedAmount = amt
	}
	return it, nil
}

// CreateBulkGrantJobItem inserts a new, pending item row for one
// targeted player. The UNIQUE (tenant_id, bulk_grant_job_id,
// player_account_id) constraint is the actual per-player-isolation/
// resumability enforcement (doc 10 §W5) - a crashed/resumed job re-walks
// its target list and should call this expecting ErrDuplicateItem for
// every player already recorded, skipping them, never re-processing.
var ErrDuplicateItem = errors.New("bonus: this player already has an item row for this job")

func CreateBulkGrantJobItem(ctx context.Context, tx pgx.Tx, it BulkGrantJobItem) (BulkGrantJobItem, error) {
	if it.ID == uuid.Nil {
		it.ID = uuid.New()
	}
	if it.Outcome == "" {
		it.Outcome = ItemPending
	}
	row := tx.QueryRow(ctx, `
		INSERT INTO bulk_grant_job_items (id, tenant_id, bulk_grant_job_id, player_account_id, outcome)
		VALUES ($1,$2,$3,$4,$5)
		RETURNING `+bulkGrantJobItemColumns,
		it.ID, it.TenantID, it.BulkGrantJobID, it.PlayerAccountID, string(it.Outcome),
	)
	item, err := scanBulkGrantJobItem(row)
	if err != nil && db.IsUniqueViolation(err) {
		return BulkGrantJobItem{}, ErrDuplicateItem
	}
	return item, err
}

// RecordBulkGrantJobItemOutcome finalizes a pending item with its real
// outcome (issued/denied/already_granted/error). Compare-and-swap on
// outcome = 'pending', mirroring UpdateGrantStatus's own guard - a
// second finalization attempt against an already-finalized item is a
// conflict, never a silent overwrite.
var ErrItemAlreadyProcessed = errors.New("bonus: this item has already been processed")

func RecordBulkGrantJobItemOutcome(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, outcome BulkGrantJobItemOutcome, reasonCode *string, grantID *uuid.UUID, grantedAmount *big.Int, errorDetail *string, at time.Time) (BulkGrantJobItem, error) {
	row := tx.QueryRow(ctx, `
		UPDATE bulk_grant_job_items
		SET outcome = $3, reason_code = $4, grant_id = $5, granted_amount = $6, error_detail = $7, processed_at = $8
		WHERE tenant_id = $1 AND id = $2 AND outcome = $9
		RETURNING `+bulkGrantJobItemColumns,
		tenantID, id, string(outcome), reasonCode, grantID, bigIntToNumeric(grantedAmount), errorDetail, at, string(ItemPending),
	)
	item, err := scanBulkGrantJobItem(row)
	if errors.Is(err, ErrNotFound) {
		return BulkGrantJobItem{}, ErrItemAlreadyProcessed
	}
	return item, err
}

// GetBulkGrantJobItem looks up the single item row for (job, player) -
// the resumability check ("skip every player who already has a
// BulkGrantJobItem row of any outcome", doc 10 §W5).
func GetBulkGrantJobItem(ctx context.Context, tx pgx.Tx, tenantID, bulkGrantJobID, playerAccountID uuid.UUID) (BulkGrantJobItem, error) {
	row := tx.QueryRow(ctx,
		`SELECT `+bulkGrantJobItemColumns+` FROM bulk_grant_job_items WHERE tenant_id = $1 AND bulk_grant_job_id = $2 AND player_account_id = $3`,
		tenantID, bulkGrantJobID, playerAccountID,
	)
	return scanBulkGrantJobItem(row)
}

// ListBulkGrantJobItems returns every item for a job - "who was
// targeted, under what authority, and what happened to each" (doc 10
// §W5's auditability requirement).
func ListBulkGrantJobItems(ctx context.Context, tx pgx.Tx, tenantID, bulkGrantJobID uuid.UUID) ([]BulkGrantJobItem, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+bulkGrantJobItemColumns+` FROM bulk_grant_job_items WHERE tenant_id = $1 AND bulk_grant_job_id = $2 ORDER BY created_at ASC`,
		tenantID, bulkGrantJobID,
	)
	if err != nil {
		return nil, fmt.Errorf("bonus: list bulk grant job items: %w", err)
	}
	defer rows.Close()
	var out []BulkGrantJobItem
	for rows.Next() {
		it, err := scanBulkGrantJobItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}
