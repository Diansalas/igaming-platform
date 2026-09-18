// Targeting (docs/architecture/10-bonus-engine-architecture.md "doc 10"
// §W5, N2.2-N2.6): single player, selected-list, promo/voucher codes
// (types.go's RedeemCoupon), manual grants, bulk grants - and doc 34's
// EconomicOperationIdentity enforcement (N2.4a) at every one of these
// grant-causing surfaces, closing SEC-W15-02's decomposition vector.
//
// SCOPE, per the human directive's explicit instruction: segment-
// targeted grants implement ONLY the STATIC/PINNED case (a target_kind =
// 'segment'/'segment_set' job references a fixed, already-resolved
// player-ID list SNAPSHOT the caller supplies) - live dynamic segment
// evaluation is NOT implemented (internal/segment does not exist, per
// the Orchestrator's own standing scope decision). A caller that wants
// LIVE segment resolution gets ErrDynamicSegmentTargetingNotSupported,
// never a silent workaround.
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
	"github.com/Diansalas/igaming-platform/internal/economicop"
)

// ErrDynamicSegmentTargetingNotSupported is returned by every targeting
// surface in this file when asked to resolve a segment/segment_set target
// LIVE - this platform's Wave scope deliberately does not build
// internal/segment (known Kleene-logic polarity bug, unpinned member_of,
// per the Orchestrator's own standing decision). A caller must resolve a
// segment to a concrete, already-pinned player-ID list itself (by
// whatever means it has) before calling BulkGrantJob-execution below;
// this package never resolves membership on its own.
var ErrDynamicSegmentTargetingNotSupported = errors.New("bonus: dynamic segment targeting is not supported - internal/segment does not exist; supply a static, pre-resolved player-id list instead")

// MintRootOperationParams is the input every fresh-root mint (doc 34
// §3.1's exactly-two mint points for Bonus Engine: a staff single-Grant
// action, or a BulkGrantJob's own four-eyes approval) shares.
type MintRootOperationParams struct {
	TenantID               uuid.UUID
	BrandID                *uuid.UUID
	OperationType          economicop.OperationType // OperationBonusManualGrant | OperationBonusBulkGrant
	InitiatingActorType    string
	InitiatingActorID      uuid.UUID
	InitiatingPrincipalID  *uuid.UUID
	SubjectScope           economicop.SubjectScope
	SubjectRef             *uuid.UUID // single_subject only
	SubjectSetCount        *int32     // enumerated_set only
	AssetCode              *string
	IntendedAggregateValue *big.Int
	RecipientCeiling       *int32
	IdempotencyKey         string // doc 34 §3.3: deterministic, derived from the authorization, e.g. (tenant_id, bulk_grant_job_id) or (tenant_id, manual_grant_request_id)
	CorrelationID          uuid.UUID
	ExpiresAt              time.Time
	// ApprovalState is set by the CALLER, who has already resolved
	// whether this specific mint requires four-eyes (per CLAUDE.md's
	// threshold rule / doc 10 W5's fail-closed default) and, if so,
	// whether it has already been granted - ApprovalNotRequired is only
	// ever passed with a recorded determination (doc 34 EOI-7), never a
	// silent default.
	ApprovalState economicop.ApprovalState
}

// MintRootOperation implements doc 34 §3.1/§3.3: mint-once, via a
// DB-unique idempotency key derived from the authorization (never the
// attempt) - a retry resolves to the SAME EOI rather than creating a
// second one.
func MintRootOperation(ctx context.Context, tx pgx.Tx, p MintRootOperationParams) (economicop.EconomicOperation, error) {
	existing, err := economicop.GetByIdempotencyKey(ctx, tx, p.TenantID, p.IdempotencyKey)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, economicop.ErrNotFound) {
		return economicop.EconomicOperation{}, err
	}

	op, err := economicop.Create(ctx, tx, economicop.EconomicOperation{
		TenantID: p.TenantID, BrandID: p.BrandID, OperationType: p.OperationType,
		InitiatingActorType: p.InitiatingActorType, InitiatingActorID: p.InitiatingActorID, InitiatingPrincipalID: p.InitiatingPrincipalID,
		SubjectScope: p.SubjectScope, SubjectRef: p.SubjectRef, SubjectSetCount: p.SubjectSetCount,
		EconomicOwner: "tenant", AssetCode: p.AssetCode, IntendedAggregateValue: p.IntendedAggregateValue, RecipientCeiling: p.RecipientCeiling,
		LineageKind: economicop.LineageRoot, ApprovalState: p.ApprovalState,
		RequiredApprovals: 2, IdempotencyKey: p.IdempotencyKey, CorrelationID: p.CorrelationID,
		Status: economicop.StatusOpen, ExpiresAt: p.ExpiresAt,
	})
	if err != nil {
		return economicop.EconomicOperation{}, err
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: p.TenantID, ActorType: audit.ActorType(p.InitiatingActorType), ActorID: p.InitiatingActorID,
		Action: "economic_operation.minted", TargetType: "economic_operation", TargetID: op.OperationID.String(), Outcome: audit.OutcomeSuccess,
		Metadata: map[string]any{"operation_type": string(p.OperationType)},
	}); err != nil {
		return economicop.EconomicOperation{}, err
	}
	return op, nil
}

// IssueSingleManualGrant is doc 10 N2.4a's single-Grant staff-action-
// equivalent surface, EOI-gated per §5.1/§5.3: the non-locking entry
// check runs first (before AssetAuthorization even starts), the ordinary
// T.1 gate chain runs unchanged, and the locking EOI consume happens
// immediately before/around the effecting write, atomically inside the
// SAME transaction - so an over-budget attempt never leaves a committed
// Grant behind, even though (for implementation-simplicity reasons
// documented on ConsumeRootBudget's own call below) this function
// performs the consume check immediately AFTER inserting the Grant row
// rather than in a separate pre-check; both orderings are equivalent
// under this transaction's atomicity (an over-budget attempt's
// transaction is never committed either way).
//
// parentOperationID MUST already be a resolvable, approved, open,
// unexpired EOI (either freshly minted via MintRootOperation for THIS
// staff action, or relayed from an upstream authorization, doc 10
// N2.4a's exactly-two paths) - there is no third path, and no fallback
// to "no parent required" for this surface.
func IssueSingleManualGrant(ctx context.Context, tx pgx.Tx, g Grant, parentOperationID uuid.UUID, jurisdictionCode string, actorID uuid.UUID, amount *big.Int) (Grant, GateOutcome, error) {
	if _, err := economicop.CheckEntry(ctx, tx, g.TenantID, parentOperationID, g.AssetCode); err != nil {
		return Grant{}, GateOutcome{}, err
	}
	g.ParentOperationID = &parentOperationID
	g.CreatedByActorType = ActorStaff
	g.CreatedByActorID = actorID

	created, issueOutcome, err := issueIdempotent(ctx, tx, IssueGrantParams{Grant: g, JurisdictionCode: jurisdictionCode})
	if err != nil && !errors.Is(err, ErrAlreadyGranted) {
		return Grant{}, GateOutcome{}, err
	}
	if !issueOutcome.Allowed {
		return created, issueOutcome, err
	}
	if !errors.Is(err, ErrAlreadyGranted) {
		op, getErr := economicop.GetByID(ctx, tx, parentOperationID)
		if getErr != nil {
			return Grant{}, GateOutcome{}, getErr
		}
		if consumeErr := economicop.ConsumeRootBudget(ctx, tx, g.TenantID, op.RootOperationID, economicop.OperationBonusManualGrant, created.PlayerAccountID, amount); consumeErr != nil {
			return Grant{}, GateOutcome{}, consumeErr
		}
	}

	activated, activateOutcome, err := ActivateGrant(ctx, tx, created.TenantID, created.ID, ActivateGrantParams{
		JurisdictionCode: jurisdictionCode, ActorType: ActorStaff, ActorID: actorID, Amount: amount,
	})
	if err != nil {
		return Grant{}, GateOutcome{}, err
	}
	return activated, activateOutcome, nil
}

// StaticPlayerListTarget is the ONLY segment-shaped target this Wave
// implements: a fixed, already-resolved list of player_account_ids -
// "pinned" in the sense that it is exactly what the caller supplies,
// never re-derived from a live segment definition.
type StaticPlayerListTarget struct {
	PlayerAccountIDs []uuid.UUID
}

// RunStaticBulkGrantJob executes an already-created, already-approved
// BulkGrantJob (doc 10 §W5/N2.4a) whose target is target_kind IN
// ('single_player', 'player_list') OR a STATIC, CALLER-SUPPLIED player-id
// list standing in for a 'segment'/'segment_set' target (see this file's
// own top-level doc comment - live segment resolution is out of scope
// and this function never attempts it: a caller passing
// target.PlayerAccountIDs for a segment-shaped job is supplying the
// static snapshot itself, not asking this function to resolve one).
//
// Resumable and idempotent by construction (doc 10 §W5): re-walks
// target.PlayerAccountIDs and skips every player who already has a
// BulkGrantJobItem row of any outcome, via the item table's own UNIQUE
// (tenant_id, bulk_grant_job_id, player_account_id) constraint.
//
// EOI enforcement per doc 10 N2.4a/doc 34 §5.3-§5.4: the job's own
// parent_operation_id (its bonus_bulk_grant root, minted at four-eyes
// approval) bounds every item's issuance, over the WHOLE subtree, via
// ConsumeRootBudget - "the 5,001st actual grant execution across the
// whole subtree... finds remaining_recipient_budget = 0... and is
// rejected."
//
// COMPENSATING SEP-1 CHECK, named explicitly (not a silent workaround):
// migration 0063's bonus_change_approvals_enforce_separation() DB
// trigger can only check SEP-1 for target_kind IN ('single_player',
// 'player_list') at APPROVAL time, because only those two shapes are
// materialized on the bulk_grant_jobs row itself before any item exists.
// For a static-list-standing-in-for-segment target, this function
// performs the IDENTICAL comparison itself, per item, at the true
// value-moving instant (immediately before that item's own Grant would
// issue) - comparing the job's OWN requester and every recorded approver
// against the item's own player, via requesterAndApproverPersons (below).
// This is a live, per-item, un-bypassable gate, even though it is
// Go-level rather than the DB-trigger shape migration 0063 uses for the
// two materializable target kinds.
func RunStaticBulkGrantJob(ctx context.Context, tx pgx.Tx, job BulkGrantJob, target StaticPlayerListTarget, grantTemplate Grant, jurisdictionCode string, systemActorID uuid.UUID, amount *big.Int) error {
	if job.ParentOperationID == nil {
		return fmt.Errorf("bonus: bulk grant job %s has no parent_operation_id", job.ID)
	}
	if _, err := economicop.CheckEntry(ctx, tx, job.TenantID, *job.ParentOperationID, grantTemplate.AssetCode); err != nil {
		return err
	}
	op, err := economicop.GetByID(ctx, tx, *job.ParentOperationID)
	if err != nil {
		return err
	}

	requesterPerson, approverPersons, err := requesterAndApproverPersons(ctx, tx, job)
	if err != nil {
		return err
	}

	for _, playerAccountID := range target.PlayerAccountIDs {
		if err := runBulkGrantJobItem(ctx, tx, job, op.RootOperationID, playerAccountID, grantTemplate, jurisdictionCode, systemActorID, amount, requesterPerson, approverPersons); err != nil {
			return err
		}
	}
	return nil
}

func runBulkGrantJobItem(ctx context.Context, tx pgx.Tx, job BulkGrantJob, rootOperationID, playerAccountID uuid.UUID, grantTemplate Grant, jurisdictionCode string, systemActorID uuid.UUID, amount *big.Int, requesterPerson uuid.UUID, approverPersons []uuid.UUID) error {
	if _, err := GetBulkGrantJobItem(ctx, tx, job.TenantID, job.ID, playerAccountID); err == nil {
		return nil // already processed (any outcome) - resumability (§W5)
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}

	item, err := CreateBulkGrantJobItem(ctx, tx, BulkGrantJobItem{TenantID: job.TenantID, BulkGrantJobID: job.ID, PlayerAccountID: playerAccountID, ParentOperationID: &rootOperationID})
	if err != nil {
		if errors.Is(err, ErrDuplicateItem) {
			return nil
		}
		return err
	}

	// Compensating per-item SEP-1 check (this file's own top-level doc
	// comment) - a bounded, deterministic person lookup, always run for
	// every item regardless of target_kind, so a job whose SEP-1 could
	// not be checked at approval time (segment-shaped) is never silently
	// under-protected.
	subjectPerson, err := playerPersonID(ctx, tx, playerAccountID)
	if err != nil {
		return err
	}
	if subjectPerson == requesterPerson || containsUUID(approverPersons, subjectPerson) {
		reason := "SEP-1: requester or an approver of this bulk job resolves to the same person as this targeted player"
		_, err := RecordBulkGrantJobItemOutcome(ctx, tx, job.TenantID, item.ID, ItemDenied, &reason, nil, nil, nil, time.Now().UTC())
		return err
	}

	walletID, err := resolvePlayerWallet(ctx, tx, job.TenantID, playerAccountID, grantTemplate.AssetCode)
	if err != nil {
		return err
	}

	grant := grantTemplate
	grant.PlayerAccountID = playerAccountID
	grant.WalletID = walletID
	grant.TriggerReference = fmt.Sprintf("bulk_grant_job:%s:%s", job.ID, playerAccountID)
	grant.ParentOperationID = &rootOperationID
	grant.CreatedByActorType = ActorSystem
	grant.CreatedByActorID = uuid.Nil

	created, issueOutcome, err := issueIdempotent(ctx, tx, IssueGrantParams{Grant: grant, JurisdictionCode: jurisdictionCode})
	if err != nil && !errors.Is(err, ErrAlreadyGranted) {
		return err
	}
	if !issueOutcome.Allowed {
		reason := denialReasonCode(issueOutcome)
		_, err := RecordBulkGrantJobItemOutcome(ctx, tx, job.TenantID, item.ID, ItemDenied, &reason, nil, nil, nil, time.Now().UTC())
		return err
	}
	if errors.Is(err, ErrAlreadyGranted) {
		_, err := RecordBulkGrantJobItemOutcome(ctx, tx, job.TenantID, item.ID, ItemAlreadyGranted, nil, &created.ID, nil, nil, time.Now().UTC())
		return err
	}

	if err := economicop.ConsumeRootBudget(ctx, tx, job.TenantID, rootOperationID, economicop.OperationBonusBulkGrant, playerAccountID, amount); err != nil {
		// NAMED FIX (Stage 4H-B1 Wave 2 Phase 6, security - found via
		// genuinely concurrent multi-worker adversarial testing, not by
		// inspection): ConsumeRootBudget's error return conflates an
		// ORDINARY, expected business-level denial (ErrBudgetExhausted -
		// the ceiling is genuinely reached, correctly recordable as
		// ItemDenied on THIS same transaction) with any OTHER error,
		// including a transient Postgres-level failure (a deadlock
		// between concurrent workers contending for the SAME EOI root's
		// FOR UPDATE lock, a dropped connection, a context
		// cancellation). Treating the latter as if it were an ordinary
		// denial and attempting a FURTHER write on the transaction was a
		// real bug: once Postgres aborts a transaction for a genuine
		// error, EVERY subsequent statement on it fails with "current
		// transaction is aborted" (SQLSTATE 25P02) - which is exactly
		// what happened, masking the true underlying error (SQLSTATE
		// 40P01, deadlock_detected) behind a confusing, generic one and
		// leaving the caller unable to distinguish "this item was
		// legitimately denied" from "this whole attempt must be
		// retried". Only ErrBudgetExhausted is recorded as ItemDenied
		// here; every other error (transient or not) is returned
		// immediately, exactly like every other non-budget error path in
		// this function, so the caller's own transaction-retry handling
		// (WithTenant returning the error, unattempted further writes)
		// applies uniformly.
		if !errors.Is(err, economicop.ErrBudgetExhausted) {
			return err
		}
		reason := err.Error()
		_, recErr := RecordBulkGrantJobItemOutcome(ctx, tx, job.TenantID, item.ID, ItemDenied, &reason, nil, nil, nil, time.Now().UTC())
		if recErr != nil {
			return recErr
		}
		return nil // budget exhaustion stops THIS item, not the whole job (doc 34 §5.5: later items may still be under budget if this one is skipped for another reason - though in practice once exhausted, every remaining item also denies)
	}

	activated, activateOutcome, err := ActivateGrant(ctx, tx, created.TenantID, created.ID, ActivateGrantParams{
		JurisdictionCode: jurisdictionCode, ActorType: ActorSystem, ActorID: systemActorID, Amount: amount,
	})
	if err != nil {
		return err
	}
	if !activateOutcome.Allowed {
		reason := denialReasonCode(activateOutcome)
		_, err := RecordBulkGrantJobItemOutcome(ctx, tx, job.TenantID, item.ID, ItemDenied, &reason, &activated.ID, nil, nil, time.Now().UTC())
		return err
	}
	amountCopy := new(big.Int).Set(amount)
	_, err = RecordBulkGrantJobItemOutcome(ctx, tx, job.TenantID, item.ID, ItemIssued, nil, &activated.ID, amountCopy, nil, time.Now().UTC())
	return err
}

// resolvePlayerWallet looks up playerAccountID's own wallet for assetCode
// - a bulk job's grant template carries only the ASSET, never a single
// fixed wallet id, since each targeted player has their own wallet.
func resolvePlayerWallet(ctx context.Context, tx pgx.Tx, tenantID, playerAccountID uuid.UUID, assetCode string) (uuid.UUID, error) {
	var walletID uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM wallets WHERE tenant_id = $1 AND player_account_id = $2 AND asset_code = $3`,
		tenantID, playerAccountID, assetCode,
	).Scan(&walletID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("bonus: resolve player wallet: %w", err)
	}
	return walletID, nil
}

func playerPersonID(ctx context.Context, tx pgx.Tx, playerAccountID uuid.UUID) (uuid.UUID, error) {
	var personID uuid.UUID
	err := tx.QueryRow(ctx, `SELECT person_id FROM player_accounts WHERE id = $1`, playerAccountID).Scan(&personID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("bonus: resolve player person id: %w", err)
	}
	return personID, nil
}

func requesterAndApproverPersons(ctx context.Context, tx pgx.Tx, job BulkGrantJob) (uuid.UUID, []uuid.UUID, error) {
	var requesterPerson uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT person_id FROM staff_users WHERE id = $1`, job.RequestedByPrincipalID).Scan(&requesterPerson); err != nil {
		return uuid.Nil, nil, fmt.Errorf("bonus: resolve requester person id: %w", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT su.person_id FROM bonus_change_approvals a
		JOIN staff_users su ON su.id = a.approver_principal_id
		JOIN bonus_change_requests r ON r.id = a.request_id
		WHERE r.tenant_id = $1 AND r.operation = 'bulk_job_execute' AND r.target_id = $2 AND a.decision = 'approve'`,
		job.TenantID, job.ID,
	)
	if err != nil {
		return uuid.Nil, nil, fmt.Errorf("bonus: resolve bulk job approver persons: %w", err)
	}
	defer rows.Close()
	var approvers []uuid.UUID
	for rows.Next() {
		var p uuid.UUID
		if err := rows.Scan(&p); err != nil {
			return uuid.Nil, nil, err
		}
		approvers = append(approvers, p)
	}
	return requesterPerson, approvers, rows.Err()
}

func containsUUID(list []uuid.UUID, v uuid.UUID) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
