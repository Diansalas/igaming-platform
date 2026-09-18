package bonus

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SuggestionStatus is bonus_suggestions.status's closed set (doc 10
// §W6/§N3) - a read-model pointer, not the authoritative history (the
// authoritative history is SuggestionReviewEvent, append-only).
type SuggestionStatus string

const (
	SuggestionGenerated   SuggestionStatus = "generated"
	SuggestionUnderReview SuggestionStatus = "under_review"
	SuggestionApproved    SuggestionStatus = "approved"
	SuggestionRejected    SuggestionStatus = "rejected"
	SuggestionEdited      SuggestionStatus = "edited"
	SuggestionActivated   SuggestionStatus = "activated"
	SuggestionDiscarded   SuggestionStatus = "discarded"
)

// OriginatingKind is bonus_suggestions.originating_kind's closed set.
type OriginatingKind string

const (
	OriginatingRule   OriginatingKind = "rule"
	OriginatingModel  OriginatingKind = "model"
	OriginatingManual OriginatingKind = "manual"
)

// ResultingReferenceKind names which of the four possible pipelines a
// Suggestion's Activation produced (doc 10 §N3.2).
type ResultingReferenceKind string

const (
	ResultingCampaign     ResultingReferenceKind = "campaign"
	ResultingOffer        ResultingReferenceKind = "offer"
	ResultingGrant        ResultingReferenceKind = "grant"
	ResultingBulkGrantJob ResultingReferenceKind = "bulk_grant_job"
)

// Suggestion mirrors one bonus_suggestions row (doc 10 §W6/§N3).
// "BonusSuggestion never creates a Grant, never moves money, and never
// calls RG/Risk/AssetAuthorization's value-moving checkpoints" - true by
// construction: nothing in this package's Suggestion-related functions
// writes to bonus_grants/economic_operations/ledger tables.
type Suggestion struct {
	ID       uuid.UUID
	TenantID uuid.UUID
	BrandID  *uuid.UUID

	Status SuggestionStatus

	OriginatingKind         OriginatingKind
	OriginatingRuleID       *uuid.UUID
	OriginatingModelVersion *string
	OriginatingStaffActorID *uuid.UUID

	GeneratedAt    time.Time
	ProposedConfig []byte // JSONB, opaque to this package (§N3.1)
	Reason         *string

	ReviewerID      *uuid.UUID
	ReviewClaimedAt *time.Time
	Decision        *string // "approved" | "rejected" | "edited"
	DecidedAt       *time.Time
	DecidedBy       *uuid.UUID
	Modifications   []byte // JSONB
	RejectionReason *string

	ActivatedAt            *time.Time
	ResultingReferenceKind *ResultingReferenceKind
	ResultingReferenceID   *uuid.UUID

	DiscardedAt   *time.Time
	DiscardReason *string
}

const suggestionColumns = `
	id, tenant_id, brand_id, status,
	originating_kind, originating_rule_id, originating_model_version, originating_staff_actor_id,
	generated_at, proposed_config, reason,
	reviewer_id, review_claimed_at, decision, decided_at, decided_by, modifications, rejection_reason,
	activated_at, resulting_reference_kind, resulting_reference_id,
	discarded_at, discard_reason`

func scanSuggestion(row rowScanner) (Suggestion, error) {
	var (
		s                      Suggestion
		status                 string
		originatingKind        string
		resultingReferenceKind *string
	)
	err := row.Scan(
		&s.ID, &s.TenantID, &s.BrandID, &status,
		&originatingKind, &s.OriginatingRuleID, &s.OriginatingModelVersion, &s.OriginatingStaffActorID,
		&s.GeneratedAt, &s.ProposedConfig, &s.Reason,
		&s.ReviewerID, &s.ReviewClaimedAt, &s.Decision, &s.DecidedAt, &s.DecidedBy, &s.Modifications, &s.RejectionReason,
		&s.ActivatedAt, &resultingReferenceKind, &s.ResultingReferenceID,
		&s.DiscardedAt, &s.DiscardReason,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Suggestion{}, ErrNotFound
	}
	if err != nil {
		return Suggestion{}, fmt.Errorf("bonus: scan suggestion: %w", err)
	}
	s.Status = SuggestionStatus(status)
	s.OriginatingKind = OriginatingKind(originatingKind)
	if resultingReferenceKind != nil {
		rk := ResultingReferenceKind(*resultingReferenceKind)
		s.ResultingReferenceKind = &rk
	}
	return s, nil
}

// CreateSuggestion inserts a new bonus_suggestions row in status
// 'generated'. Per security-architecture.md §W15.4.3,
// bonus_suggestion:create is granted to no human role (service identity
// only) - this function performs no permission check itself, callers
// (Phase 3's HTTP layer) are responsible for that.
func CreateSuggestion(ctx context.Context, tx pgx.Tx, s Suggestion) (Suggestion, error) {
	if s.ID == uuid.Nil {
		s.ID = uuid.New()
	}
	if s.Status == "" {
		s.Status = SuggestionGenerated
	}
	row := tx.QueryRow(ctx, `
		INSERT INTO bonus_suggestions (
			id, tenant_id, brand_id, status,
			originating_kind, originating_rule_id, originating_model_version, originating_staff_actor_id,
			proposed_config, reason
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		RETURNING `+suggestionColumns,
		s.ID, s.TenantID, s.BrandID, string(s.Status),
		string(s.OriginatingKind), s.OriginatingRuleID, s.OriginatingModelVersion, s.OriginatingStaffActorID,
		nonNilJSON(s.ProposedConfig), s.Reason,
	)
	return scanSuggestion(row)
}

// GetSuggestionByID looks up a Suggestion by id.
func GetSuggestionByID(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Suggestion, error) {
	row := tx.QueryRow(ctx, `SELECT `+suggestionColumns+` FROM bonus_suggestions WHERE id = $1`, id)
	return scanSuggestion(row)
}

// ListSuggestionsByStatus returns every Suggestion in a given status,
// oldest first (the review queue's own natural order).
func ListSuggestionsByStatus(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, status SuggestionStatus) ([]Suggestion, error) {
	rows, err := tx.Query(ctx, `SELECT `+suggestionColumns+` FROM bonus_suggestions WHERE tenant_id = $1 AND status = $2 ORDER BY generated_at ASC`, tenantID, string(status))
	if err != nil {
		return nil, fmt.Errorf("bonus: list suggestions: %w", err)
	}
	defer rows.Close()
	var out []Suggestion
	for rows.Next() {
		s, err := scanSuggestion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// UpdateSuggestionStatus performs an unconditional status write plus
// whichever review-decision fields the caller supplies - no lifecycle-
// legality enforcement (Generated -> UnderReview -> ... , doc 10 §N3.2)
// beyond what the CHECK constraints already pin. Phase 3's state-machine
// layer is expected to call this only with an already-validated
// transition, and to write the corresponding SuggestionReviewEvent
// (RecordSuggestionReviewEvent, below) in the SAME transaction - this
// function does not do that itself, since the two are independent writes
// this package does not want to silently couple.
func UpdateSuggestionStatus(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, status SuggestionStatus, reviewerID *uuid.UUID) (Suggestion, error) {
	row := tx.QueryRow(ctx, `
		UPDATE bonus_suggestions SET status = $3, reviewer_id = COALESCE($4, reviewer_id)
		WHERE tenant_id = $1 AND id = $2
		RETURNING `+suggestionColumns,
		tenantID, id, string(status), reviewerID,
	)
	return scanSuggestion(row)
}

// RecordSuggestionDecision records an Approved/Rejected/Edited decision
// round (doc 10 §N3.2).
func RecordSuggestionDecision(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, status SuggestionStatus, decision string, decidedBy uuid.UUID, at time.Time, modifications, rejectionReason *string) (Suggestion, error) {
	var modBytes []byte
	if modifications != nil {
		modBytes = []byte(*modifications)
	}
	row := tx.QueryRow(ctx, `
		UPDATE bonus_suggestions
		SET status = $3, decision = $4, decided_at = $5, decided_by = $6, modifications = COALESCE($7, modifications), rejection_reason = $8
		WHERE tenant_id = $1 AND id = $2
		RETURNING `+suggestionColumns,
		tenantID, id, string(status), decision, at, decidedBy, modBytes, rejectionReason,
	)
	return scanSuggestion(row)
}

// ActivateSuggestion records Activation's bidirectional back-reference
// (doc 10 §W6/§N3.2). Callers are responsible for having already
// submitted proposed_config through the ORDINARY, unmodified Grant/
// BulkGrantJob/Campaign/Offer pipeline (Activation "is"
// BulkGrantJob.Create()/Grant.Issue() called with an extra
// originating_suggestion_id parameter, not a second code path, N3.2) -
// this function only records the forward pointer on the Suggestion
// itself; the corresponding backward pointer
// (Grant.OriginatingSuggestionID / BulkGrantJob.OriginatingSuggestionID)
// is set on the target row's own creation, not here.
func ActivateSuggestion(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, kind ResultingReferenceKind, resultingID uuid.UUID, at time.Time) (Suggestion, error) {
	row := tx.QueryRow(ctx, `
		UPDATE bonus_suggestions
		SET status = $3, activated_at = $4, resulting_reference_kind = $5, resulting_reference_id = $6
		WHERE tenant_id = $1 AND id = $2
		RETURNING `+suggestionColumns,
		tenantID, id, string(SuggestionActivated), at, string(kind), resultingID,
	)
	return scanSuggestion(row)
}

// DiscardSuggestion applies the terminal Discarded transition.
func DiscardSuggestion(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, at time.Time, reason *string) (Suggestion, error) {
	row := tx.QueryRow(ctx, `
		UPDATE bonus_suggestions SET status = $3, discarded_at = $4, discard_reason = $5
		WHERE tenant_id = $1 AND id = $2
		RETURNING `+suggestionColumns,
		tenantID, id, string(SuggestionDiscarded), at, reason,
	)
	return scanSuggestion(row)
}

// SuggestionReviewEvent mirrors one bonus_suggestion_review_events row -
// the authoritative, append-only review trail (doc 10 §N3.2).
type SuggestionReviewEvent struct {
	ID           uuid.UUID
	TenantID     uuid.UUID
	SuggestionID uuid.UUID
	EventType    SuggestionStatus // reuses the same closed vocabulary
	ActorType    ActorType
	ActorID      uuid.UUID
	ReasonCode   *string
	Detail       []byte // JSONB
	OccurredAt   time.Time
}

const suggestionReviewEventColumns = `id, tenant_id, suggestion_id, event_type, actor_type, actor_id, reason_code, detail, occurred_at`

func scanSuggestionReviewEvent(row rowScanner) (SuggestionReviewEvent, error) {
	var (
		e         SuggestionReviewEvent
		eventType string
		actorType string
	)
	err := row.Scan(&e.ID, &e.TenantID, &e.SuggestionID, &eventType, &actorType, &e.ActorID, &e.ReasonCode, &e.Detail, &e.OccurredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return SuggestionReviewEvent{}, ErrNotFound
	}
	if err != nil {
		return SuggestionReviewEvent{}, fmt.Errorf("bonus: scan suggestion review event: %w", err)
	}
	e.EventType = SuggestionStatus(eventType)
	e.ActorType = ActorType(actorType)
	return e, nil
}

// RecordSuggestionReviewEvent appends one row to the authoritative,
// append-only review trail (doc 10 §N3.2) - the database's own
// immutability trigger (migration 0056) enforces append-only, not this
// function.
func RecordSuggestionReviewEvent(ctx context.Context, tx pgx.Tx, e SuggestionReviewEvent) (SuggestionReviewEvent, error) {
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	row := tx.QueryRow(ctx, `
		INSERT INTO bonus_suggestion_review_events (id, tenant_id, suggestion_id, event_type, actor_type, actor_id, reason_code, detail)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING `+suggestionReviewEventColumns,
		e.ID, e.TenantID, e.SuggestionID, string(e.EventType), string(e.ActorType), e.ActorID, e.ReasonCode, nonNilJSON(e.Detail),
	)
	return scanSuggestionReviewEvent(row)
}

// ListSuggestionReviewEvents returns a Suggestion's full review trail,
// in the order the events occurred.
func ListSuggestionReviewEvents(ctx context.Context, tx pgx.Tx, tenantID, suggestionID uuid.UUID) ([]SuggestionReviewEvent, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+suggestionReviewEventColumns+` FROM bonus_suggestion_review_events WHERE tenant_id = $1 AND suggestion_id = $2 ORDER BY occurred_at ASC`,
		tenantID, suggestionID,
	)
	if err != nil {
		return nil, fmt.Errorf("bonus: list suggestion review events: %w", err)
	}
	defer rows.Close()
	var out []SuggestionReviewEvent
	for rows.Next() {
		e, err := scanSuggestionReviewEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
