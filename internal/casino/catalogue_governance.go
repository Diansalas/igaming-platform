// Stage 9.2, Workstream A: closes ARCH-DB-2 Phase 2 (docs/decisions/0081
// §5, §5.2) - the Go-level surface over migration 0086's four-eyes
// governance tables (casino_catalogue_change_requests/_approvals) and the
// casino_games_dual_control trigger they feed.
//
// Scope, per ADR 0081 §5 (unchanged, restated so it is visible next to the
// code that enforces it): ADDING a jurisdiction_blocklist code and
// disabling a game (status 'active'->'disabled') are the fail-closed
// direction and remain single-actor, unaffected by this file -
// newUpsertCasinoGameHandler's existing PUT path still performs both
// directly. REMOVING a code, and re-enabling a disabled game
// ('disabled'->'active'), are the fail-open/compliance-widening direction
// and require an approved request filed and decided through the two
// functions here before UpsertGame's own UPDATE will be accepted -
// enforced authoritatively by the database trigger, not by this file's own
// discipline.
//
// Every function here requires a transaction opened by
// db.Pool.WithPlatformAdmin, exactly like UpsertGame - asserted in-function
// via assertPlatformScope AND enforced independently by migration 0086's
// RLS policies.
package casino

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ChangeOperation is one of the two dual-controlled catalogue-governance
// operations (ADR 0081 §5.2). Adding a jurisdiction_blocklist code and
// disabling a game have deliberately no representation here - migration
// 0086's operation CHECK constraint has no value for either, so neither
// can ever be made to look like it needed (or received) an approval it
// didn't.
type ChangeOperation string

const (
	ChangeJurisdictionUnblock ChangeOperation = "jurisdiction_unblock"
	ChangeStatusActivate      ChangeOperation = "status_activate"
)

func validChangeOperation(op ChangeOperation) bool {
	switch op {
	case ChangeJurisdictionUnblock, ChangeStatusActivate:
		return true
	}
	return false
}

// ChangeOperations is the canonical list, for HTTP-layer validation so the
// route's allowlist cannot drift from this file.
func ChangeOperations() []ChangeOperation {
	return []ChangeOperation{ChangeJurisdictionUnblock, ChangeStatusActivate}
}

// ChangeRequest is a pending (or decided) four-eyes catalogue-governance
// request - mirrors a casino_catalogue_change_requests row.
type ChangeRequest struct {
	ID                     uuid.UUID
	Operation              ChangeOperation
	GameID                 uuid.UUID
	Payload                map[string]any
	ReasonCode             string
	RequestedByPrincipalID uuid.UUID
	RequestedAt            time.Time
	State                  string
	AppliedByPrincipalID   *uuid.UUID
	AppliedAt              *time.Time
}

// ChangeApproval is one four-eyes decision on a ChangeRequest - mirrors a
// casino_catalogue_change_approvals row.
type ChangeApproval struct {
	ID                  uuid.UUID
	RequestID           uuid.UUID
	ApproverPrincipalID uuid.UUID
	Decision            string
	ReasonCode          string
	DecidedAt           time.Time
}

// classifyChangeRequestTriggerError turns migration 0086's raised
// exceptions into this package's own sentinels, so the HTTP layer can
// answer 409/403 instead of a generic 500 - mirrors
// internal/assetregistry's classifyTriggerError exactly, adapted to this
// table's own trigger messages. The trigger remains the authoritative
// refusal; this only classifies what it said.
func classifyChangeRequestTriggerError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	msg := pgErr.Message
	switch {
	// Migration 0089's principal-eligibility conditions (SEC-S92-1:
	// mandatory Person linkage + active status, mirroring migration
	// 0047's asset-registry precedent) are classified FIRST, ahead of both
	// the self-approval and four-eyes arms, exactly like
	// assetregistry.classifyTriggerError does. Their messages legitimately
	// mention four-eyes/self-approval (that is what the refusal is
	// protecting), so a later arm would never be reached - and the
	// distinction matters operationally: an operator hitting this needs
	// to know the fix is "link this staff account to a Person" or
	// "reactivate this account", not "obtain an approval" or "ask someone
	// else to decide".
	case strings.Contains(msg, "no confirmed Person linkage"),
		strings.Contains(msg, "is not active"),
		strings.Contains(msg, "not a platform-scoped staff principal"),
		strings.Contains(msg, "cannot be resolved"):
		return fmt.Errorf("%w: %s", ErrPrincipalNotEligible, msg)
	case strings.Contains(msg, "self-approval"):
		return fmt.Errorf("%w: %s", ErrSelfApproval, msg)
	case strings.Contains(msg, "four-eyes"):
		return fmt.Errorf("%w: %s", ErrDualControlRequired, msg)
	case strings.Contains(msg, "does not match the codes actually being removed"),
		strings.Contains(msg, "is not visible in this scope"):
		return fmt.Errorf("%w: %s", ErrInvalidInput, msg)
	case strings.Contains(msg, "already") && strings.Contains(msg, "cannot be re-decided"):
		return fmt.Errorf("%w: %s", ErrChangeRequestNotPending, msg)
	}
	return err
}

// sortedUniqueCodes de-duplicates and sorts a jurisdiction-code list, so a
// caller's ordering/repetition never leaks into the approved payload that
// migration 0086's trigger compares against the codes actually removed.
func sortedUniqueCodes(codes []string) []string {
	seen := make(map[string]struct{}, len(codes))
	out := make([]string, 0, len(codes))
	for _, c := range codes {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if _, ok := seen[c]; ok {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// FileChangeRequestParams is FileChangeRequest's input.
type FileChangeRequestParams struct {
	Operation ChangeOperation
	GameID    uuid.UUID
	// RemovedCodes is required, and each code must currently appear in the
	// game's own jurisdiction_blocklist, for ChangeJurisdictionUnblock -
	// ignored for ChangeStatusActivate. Recorded in the request's payload
	// (sorted, de-duplicated) so the approver approves the EXACT codes
	// being unblocked, not "some removal" - migration 0086's trigger
	// verifies the eventual UPDATE matches this set exactly.
	RemovedCodes           []string
	ReasonCode             string
	RequestedByPrincipalID uuid.UUID
}

// FileChangeRequest records a pending request for one of the two
// dual-controlled operations. It never performs the operation itself -
// only casino_catalogue_change_consume_approved_request (invoked from
// inside casino_games_dual_control, itself invoked from UpsertGame's own
// UPDATE) ever applies one, and only once.
func FileChangeRequest(ctx context.Context, tx pgx.Tx, p FileChangeRequestParams) (ChangeRequest, error) {
	if err := assertPlatformScope(ctx, tx); err != nil {
		return ChangeRequest{}, err
	}
	if p.RequestedByPrincipalID == uuid.Nil {
		return ChangeRequest{}, fmt.Errorf("%w: requested_by_principal_id is required", ErrInvalidInput)
	}
	if strings.TrimSpace(p.ReasonCode) == "" {
		return ChangeRequest{}, fmt.Errorf("%w: reason_code is required", ErrInvalidInput)
	}
	if !validChangeOperation(p.Operation) {
		return ChangeRequest{}, fmt.Errorf("%w: operation must be one of jurisdiction_unblock/status_activate (adding a blocklist code and disabling a game are deliberately single-actor)", ErrInvalidInput)
	}
	if p.GameID == uuid.Nil {
		return ChangeRequest{}, fmt.Errorf("%w: game_id is required", ErrInvalidInput)
	}

	game, err := GetGameByID(ctx, tx, p.GameID)
	if err != nil {
		return ChangeRequest{}, err
	}

	payload := map[string]any{}
	switch p.Operation {
	case ChangeJurisdictionUnblock:
		codes := sortedUniqueCodes(p.RemovedCodes)
		if len(codes) == 0 {
			return ChangeRequest{}, fmt.Errorf("%w: removed_codes is required and must be non-empty for jurisdiction_unblock", ErrInvalidInput)
		}
		current := make(map[string]struct{}, len(game.JurisdictionBlocklist))
		for _, c := range game.JurisdictionBlocklist {
			current[c] = struct{}{}
		}
		for _, c := range codes {
			if _, ok := current[c]; !ok {
				return ChangeRequest{}, fmt.Errorf("%w: jurisdiction code %q is not currently in game %s's blocklist", ErrInvalidInput, c, p.GameID)
			}
		}
		payload["removed_codes"] = codes
	case ChangeStatusActivate:
		if game.Status != GameStatusDisabled {
			return ChangeRequest{}, fmt.Errorf("%w: game %s is not currently disabled", ErrInvalidInput, p.GameID)
		}
	}

	req := ChangeRequest{
		ID: uuid.New(), Operation: p.Operation, GameID: p.GameID, Payload: payload,
		ReasonCode: p.ReasonCode, RequestedByPrincipalID: p.RequestedByPrincipalID, State: "pending",
	}
	if err := tx.QueryRow(ctx,
		`INSERT INTO casino_catalogue_change_requests (id, operation, game_id, payload, reason_code, requested_by_principal_id)
		 VALUES ($1, $2, $3, $4, $5, $6) RETURNING requested_at`,
		req.ID, req.Operation, req.GameID, payload, req.ReasonCode, req.RequestedByPrincipalID,
	).Scan(&req.RequestedAt); err != nil {
		return ChangeRequest{}, classifyChangeRequestTriggerError(fmt.Errorf("casino: insert catalogue change request: %w", err))
	}
	return req, nil
}

// DecideChangeRequestParams is DecideChangeRequest's input.
type DecideChangeRequestParams struct {
	RequestID           uuid.UUID
	Approve             bool
	ReasonCode          string
	ApproverPrincipalID uuid.UUID
}

// DecideChangeRequest records one approve/reject decision. The database
// enforces, authoritatively: one decision per principal per request
// (UNIQUE), the requester may not decide its own request, the approver
// must be a platform-scoped staff principal, and a decision can never be
// edited afterwards (migration 0086). This function additionally locks
// the request row and refuses a decision on one that has already left
// 'pending' (applied/rejected/cancelled) - mirrors
// internal/withdrawal.Approve's lockRequestForUpdate/ErrStateConflict
// pattern exactly, and is what makes a concurrent decide-then-apply race
// serialize correctly against casino_catalogue_change_consume_approved_
// request's own FOR UPDATE on the identical row.
func DecideChangeRequest(ctx context.Context, tx pgx.Tx, p DecideChangeRequestParams) (ChangeApproval, error) {
	if err := assertPlatformScope(ctx, tx); err != nil {
		return ChangeApproval{}, err
	}
	if p.RequestID == uuid.Nil {
		return ChangeApproval{}, fmt.Errorf("%w: request_id is required", ErrInvalidInput)
	}
	if p.ApproverPrincipalID == uuid.Nil {
		return ChangeApproval{}, fmt.Errorf("%w: approver_principal_id is required", ErrInvalidInput)
	}
	if strings.TrimSpace(p.ReasonCode) == "" {
		return ChangeApproval{}, fmt.Errorf("%w: reason_code is required", ErrInvalidInput)
	}

	var state string
	err := tx.QueryRow(ctx,
		`SELECT state FROM casino_catalogue_change_requests WHERE id = $1 FOR UPDATE`, p.RequestID,
	).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return ChangeApproval{}, ErrChangeRequestNotFound
	}
	if err != nil {
		return ChangeApproval{}, fmt.Errorf("casino: lock catalogue change request: %w", err)
	}
	if state != "pending" {
		return ChangeApproval{}, fmt.Errorf("%w: request %s is %q", ErrChangeRequestNotPending, p.RequestID, state)
	}

	decision := "reject"
	if p.Approve {
		decision = "approve"
	}
	ap := ChangeApproval{ID: uuid.New(), RequestID: p.RequestID, ApproverPrincipalID: p.ApproverPrincipalID,
		Decision: decision, ReasonCode: p.ReasonCode}

	if err := tx.QueryRow(ctx,
		`INSERT INTO casino_catalogue_change_approvals (id, request_id, approver_principal_id, decision, reason_code)
		 VALUES ($1, $2, $3, $4, $5) RETURNING decided_at`,
		ap.ID, ap.RequestID, ap.ApproverPrincipalID, ap.Decision, ap.ReasonCode,
	).Scan(&ap.DecidedAt); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			// The ONLY unique constraint reachable here is
			// UNIQUE (request_id, approver_principal_id) - this principal
			// already decided this request. A retry/double-click, not
			// self-dealing (assetregistry.ErrDuplicateDecision's identical
			// rationale - code-reviewer finding F9 there).
			return ChangeApproval{}, fmt.Errorf("%w: request %s", ErrDuplicateDecision, p.RequestID)
		}
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return ChangeApproval{}, ErrChangeRequestNotFound
		}
		return ChangeApproval{}, classifyChangeRequestTriggerError(fmt.Errorf("casino: insert catalogue change approval: %w", err))
	}
	return ap, nil
}
