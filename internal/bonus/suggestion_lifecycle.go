// BonusSuggestion lifecycle enforcement (docs/architecture/10-bonus-
// engine-architecture.md "doc 10" §W6/§N3): Generated -> UnderReview ->
// Edit/Approve/Reject -> Activate. A suggestion NEVER itself posts money
// or creates a Grant (§N3.3's structural guarantee) - Activate here does
// nothing but call the EXACT SAME grant-causing surface (targeting.go's
// IssueSingleManualGrant) any other staff action would call, with an
// extra originating_suggestion_id back-reference. There is no
// suggestion-specific issuance code path anywhere in this file.
package bonus

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
)

// ErrIllegalSuggestionTransition mirrors ErrIllegalTransition one level
// up, for the read-model status column's own legality rules (doc 10
// §N3.2).
var ErrIllegalSuggestionTransition = errors.New("bonus: illegal bonus suggestion state transition")

// ClaimSuggestionForReview performs Generated -> UnderReview.
func ClaimSuggestionForReview(ctx context.Context, tx pgx.Tx, tenantID, suggestionID, reviewerID uuid.UUID) (Suggestion, error) {
	s, err := GetSuggestionByID(ctx, tx, suggestionID)
	if err != nil {
		return Suggestion{}, err
	}
	if s.Status != SuggestionGenerated {
		return Suggestion{}, fmt.Errorf("%w: claim requires status generated, got %s", ErrIllegalSuggestionTransition, s.Status)
	}
	updated, err := UpdateSuggestionStatus(ctx, tx, tenantID, suggestionID, SuggestionUnderReview, &reviewerID)
	if err != nil {
		return Suggestion{}, err
	}
	if _, err := RecordSuggestionReviewEvent(ctx, tx, SuggestionReviewEvent{
		TenantID: tenantID, SuggestionID: suggestionID, EventType: SuggestionUnderReview, ActorType: ActorStaff, ActorID: reviewerID,
	}); err != nil {
		return Suggestion{}, err
	}
	return updated, nil
}

// DecideSuggestion performs UnderReview -> {Approved | Rejected | Edited}
// (doc 10 §N3.2). For Edited, modifications is the field-by-field diff
// against proposed_config (never a silent overwrite); an Edited
// suggestion requires its own subsequent Approve/Reject round - it is
// never itself a terminal approval.
func DecideSuggestion(ctx context.Context, tx pgx.Tx, tenantID, suggestionID uuid.UUID, decision string, decidedBy uuid.UUID, modifications, rejectionReason *string) (Suggestion, error) {
	s, err := GetSuggestionByID(ctx, tx, suggestionID)
	if err != nil {
		return Suggestion{}, err
	}
	if s.Status != SuggestionUnderReview {
		return Suggestion{}, fmt.Errorf("%w: decide requires status under_review, got %s", ErrIllegalSuggestionTransition, s.Status)
	}
	var newStatus SuggestionStatus
	switch decision {
	case "approved":
		newStatus = SuggestionApproved
	case "rejected":
		if rejectionReason == nil || *rejectionReason == "" {
			return Suggestion{}, fmt.Errorf("bonus: rejection requires a reason (doc 10 §N3.1)")
		}
		newStatus = SuggestionRejected
	case "edited":
		if modifications == nil {
			return Suggestion{}, fmt.Errorf("bonus: an edit requires a modifications diff")
		}
		newStatus = SuggestionEdited
	default:
		return Suggestion{}, fmt.Errorf("bonus: unrecognized suggestion decision %q", decision)
	}

	now := time.Now().UTC()
	updated, err := RecordSuggestionDecision(ctx, tx, tenantID, suggestionID, newStatus, decision, decidedBy, now, modifications, rejectionReason)
	if err != nil {
		return Suggestion{}, err
	}
	if _, err := RecordSuggestionReviewEvent(ctx, tx, SuggestionReviewEvent{
		TenantID: tenantID, SuggestionID: suggestionID, EventType: newStatus, ActorType: ActorStaff, ActorID: decidedBy, ReasonCode: rejectionReason,
	}); err != nil {
		return Suggestion{}, err
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: tenantID, ActorType: audit.ActorStaff, ActorID: decidedBy,
		Action: "bonus_suggestion." + decision, TargetType: "bonus_suggestion", TargetID: suggestionID.String(), Outcome: audit.OutcomeSuccess,
	}); err != nil {
		return Suggestion{}, err
	}
	return updated, nil
}

// ActivateSuggestionAsSingleGrant is Activation for a single-player-
// population suggestion (doc 10 §N3.2): reachable ONLY from Approved
// (directly, or via one or more Edited rounds each themselves Approved -
// callers must have already re-run DecideSuggestion to Approved before
// calling this; this function itself only checks the CURRENT status is
// Approved). It calls the IDENTICAL, unmodified IssueSingleManualGrant
// surface - SEP-1, T.1's gate, and doc 34's EOI enforcement all apply
// exactly as they would for a staff member acting with no suggestion
// involved at all.
func ActivateSuggestionAsSingleGrant(ctx context.Context, tx pgx.Tx, tenantID, suggestionID uuid.UUID, g Grant, parentOperationID uuid.UUID, actorID uuid.UUID, amount *big.Int) (Grant, GateOutcome, error) {
	s, err := GetSuggestionByID(ctx, tx, suggestionID)
	if err != nil {
		return Grant{}, GateOutcome{}, err
	}
	if s.Status != SuggestionApproved {
		return Grant{}, GateOutcome{}, fmt.Errorf("%w: activation requires status approved, got %s", ErrIllegalSuggestionTransition, s.Status)
	}

	g.OriginatingSuggestionID = &suggestionID
	activated, outcome, err := IssueSingleManualGrant(ctx, tx, g, parentOperationID, actorID, amount)
	if err != nil {
		return Grant{}, GateOutcome{}, err
	}
	if !outcome.Allowed {
		return activated, outcome, nil
	}

	now := time.Now().UTC()
	if _, err := ActivateSuggestion(ctx, tx, tenantID, suggestionID, ResultingGrant, activated.ID, now); err != nil {
		return Grant{}, GateOutcome{}, err
	}
	if _, err := AppendGrantProgress(ctx, tx, GrantProgressEntry{
		TenantID: activated.TenantID, BrandID: activated.BrandID, PlayerAccountID: activated.PlayerAccountID, GrantID: activated.ID,
		TransitionType: TransitionSuggestionActivationLinked, TriggerType: TriggerStaffAction, ActorType: ActorStaff, ActorID: &actorID,
		Detail: []byte(fmt.Sprintf(`{"originating_suggestion_id":%q}`, suggestionID)),
	}); err != nil {
		return Grant{}, GateOutcome{}, err
	}
	if _, err := RecordSuggestionReviewEvent(ctx, tx, SuggestionReviewEvent{
		TenantID: tenantID, SuggestionID: suggestionID, EventType: SuggestionActivated, ActorType: ActorStaff, ActorID: actorID,
	}); err != nil {
		return Grant{}, GateOutcome{}, err
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: tenantID, ActorType: audit.ActorStaff, ActorID: actorID,
		Action: "bonus_suggestion.activated", TargetType: "bonus_suggestion", TargetID: suggestionID.String(), Outcome: audit.OutcomeSuccess,
		Metadata: map[string]any{"resulting_grant_id": activated.ID.String()},
	}); err != nil {
		return Grant{}, GateOutcome{}, err
	}
	return activated, outcome, nil
}

// DiscardSuggestionWithAudit is the terminal Discarded transition,
// audited (doc 10 §N3.2).
func DiscardSuggestionWithAudit(ctx context.Context, tx pgx.Tx, tenantID, suggestionID, actorID uuid.UUID, reason *string) (Suggestion, error) {
	now := time.Now().UTC()
	updated, err := DiscardSuggestion(ctx, tx, tenantID, suggestionID, now, reason)
	if err != nil {
		return Suggestion{}, err
	}
	if _, err := RecordSuggestionReviewEvent(ctx, tx, SuggestionReviewEvent{
		TenantID: tenantID, SuggestionID: suggestionID, EventType: SuggestionDiscarded, ActorType: ActorStaff, ActorID: actorID, ReasonCode: reason,
	}); err != nil {
		return Suggestion{}, err
	}
	return updated, audit.Record(ctx, tx, audit.Entry{
		TenantID: tenantID, ActorType: audit.ActorStaff, ActorID: actorID,
		Action: "bonus_suggestion.discarded", TargetType: "bonus_suggestion", TargetID: suggestionID.String(), Outcome: audit.OutcomeSuccess,
	})
}
