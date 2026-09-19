// Enforcement (docs/architecture/34-economic-operation-identity.md
// "doc 34" §5) - the fail-closed entry check (§5.1) and the locking
// budget/approval consume (§5.4), over the whole lineage subtree rooted
// at root_operation_id (§3.4), never a single generation of parentage.
// Deferred by Phase 2 ("no fail-closed entry check, no locking budget-
// consumption function, no canonical lock ordering"), built here.
//
// Package placement per doc 34 §4.5: "mint, resolve, check-and-consume-
// budget... No scheduler, no workflow engine... beyond the status enum."
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

// Entry-check rejection reasons (doc 34 §5.1's table, one sentinel per
// row so a caller/HTTP layer can distinguish them without parsing
// prose).
var (
	ErrParentOperationRequired = errors.New("economicop: no parent_operation_id supplied and no fresh root minted")
	ErrParentOperationNotFound = errors.New("economicop: parent_operation_id does not resolve, or resolves in another tenant")
	ErrParentNotApproved       = errors.New("economicop: resolved EOI's approval_state is not approved (or a recorded not_required)")
	ErrParentNotOpenOrExpired  = errors.New("economicop: resolved EOI is not open, or has expired")
	ErrChildScopeExceedsParent = errors.New("economicop: this execution's scope exceeds the parent's remaining budget or declared bounds")
)

// CheckEntry implements doc 34 §5.1's fail-closed entry rule: non-
// locking, evaluated BEFORE the AssetAuthorization/RG/Risk gate chain
// even starts (§5.3 rule 1). It returns the resolved, still-valid parent
// EconomicOperation, or one of the sentinels above (never a generic
// error) for every named rejection condition. Any error resolving any of
// these is itself a rejection (fail closed) - the caller must treat a
// non-nil error from this function as "reject", never retry-as-if-allowed.
//
// assetCode/subjectRef are optional narrowing checks (empty/zero skips
// them) - doc 34 §3.2's five containment checks in full (recipient
// ceiling, aggregate value, subject-set containment, asset match,
// expires_at) are only fully meaningful for a CHILD EOI being created
// under a parent; for Bonus Engine's first-slice usage (relaying an
// already-approved root directly, never minting a page/item EOI of its
// own - N2.4a's "(a) mints a fresh root" / "(b) relays an already-minted
// root" split has no third, EOI-child-minting path in this slice), the
// asset/expiry checks below are what actually apply; recipient/value
// containment is enforced by ConsumeRootBudget below, at the true
// locking-consume point (§5.3 rule 3), not here.
func CheckEntry(ctx context.Context, tx pgx.Tx, tenantID, parentOperationID uuid.UUID, assetCode string) (EconomicOperation, error) {
	if parentOperationID == uuid.Nil {
		return EconomicOperation{}, ErrParentOperationRequired
	}
	op, err := GetByID(ctx, tx, parentOperationID)
	if errors.Is(err, ErrNotFound) {
		return EconomicOperation{}, ErrParentOperationNotFound
	}
	if err != nil {
		return EconomicOperation{}, fmt.Errorf("%w: %v", ErrParentOperationNotFound, err)
	}
	if op.TenantID != tenantID {
		return EconomicOperation{}, ErrParentOperationNotFound
	}
	if op.ApprovalState != ApprovalApproved && op.ApprovalState != ApprovalNotRequired {
		return EconomicOperation{}, ErrParentNotApproved
	}
	if op.Status != StatusOpen {
		return EconomicOperation{}, ErrParentNotOpenOrExpired
	}
	if !time.Now().UTC().Before(op.ExpiresAt) {
		return EconomicOperation{}, ErrParentNotOpenOrExpired
	}
	if assetCode != "" && op.AssetCode != nil && *op.AssetCode != assetCode {
		return EconomicOperation{}, ErrChildScopeExceedsParent
	}
	return op, nil
}

// consumptionShape declares, per doc 34 §3.4/RK-W15P2-4, exactly which
// child-row table/columns constitute an operation_type's consumption
// record. An operation_type with no entry here is an UNDECLARED shape and
// ConsumeRootBudget refuses it outright (fail closed, never a silent
// zero) - mirroring internal/risk/cumulative.go's cumulativeSpec/
// ErrUnrecognizedCumulativeLeg discipline one level up.
//
// Only the two shapes bonus-engine's first slice actually needs are
// declared. bonus_held_disposition_resolution, crm_engagement_campaign_
// activation, affiliate_commission_settlement/reattribution, and
// manual_balance_adjustment are each a FUTURE consumer's own declaration
// to add when that consumer's Phase lands - not invented here.
var consumptionShapes = map[OperationType]string{
	OperationBonusManualGrant:  "bonus_grants",
	OperationAPIInitiatedGrant: "bonus_grants",
	OperationBonusBulkGrant:    "bulk_grant_job_items",
}

// consumptionRealizedFilter is the per-shape predicate declaring which
// rows of consumptionShapes' table count as an ACTUALLY REALIZED
// consumption (doc 34 §3.4's "recipient reached" test) versus a row that
// merely exists for resumability/audit purposes but never issued
// anything.
//
// bonus_grants DOES need a realized predicate (fixed here, DR-4HB1W2-02):
// a bonus_grants row is inserted by issueIdempotent/IssueGrant at status
// 'issued' BEFORE ConsumeRootBudget ever runs for it - the consume now
// happens inside ActivateGrant's PostGateHook (DR-4HB1W2-01), strictly
// AFTER the T.1 gate chain but strictly BEFORE the 'issued'->'activated'
// UpdateGrantStatus call (lifecycle.go's ActivateGrant, PostGateHook call
// site precedes the status-transition call site). So at the instant THIS
// execution's own count query runs, its own just-inserted row is still
// 'issued', never yet 'activated' - excluding 'issued' rows here is
// exactly what makes ConsumeRootBudget's "!alreadyCounted && currentCount+
// 1 > ceiling" check see the subject player as NOT yet counted, the same
// property bulk_grant_job_items' own 'pending' exclusion already gives
// the bulk surface. Without this, the subject was ALWAYS already counted
// (alreadyCounted=true, since the row already existed pre-fix) and
// recipient_ceiling was a structural no-op on this surface - the finding
// this fix closes. 'cancelled' is excluded for the same reason
// bulk_grant_job_items excludes 'denied': a Grant the T.1 gate chain
// denied (ActivateGrant's own pre-PostGateHook GateCheckpoint call, which
// transitions issued->cancelled and returns BEFORE PostGateHook ever
// runs) was never actually granted and must not occupy a recipient-
// ceiling slot. Every OTHER status (activated and anything reachable only
// from it - in_progress, pending_settlement, completed, converted,
// expired, forfeited, reversed) is a REAL, ledger-posted grant and stays
// counted forever, even if later reversed/forfeited (RK-W15P2-2: "a
// clawed-back recipient has still been reached" - that rule is about a
// REAL grant later reversed, never about an attempt denied before
// issuing). Scoped to exactly this table/column per this dispatch's own
// constraint - no other operation_type's shape is touched.
var consumptionRealizedFilter = map[string]string{
	"bulk_grant_job_items": "AND c.outcome IN ('issued', 'already_granted')",
	"bonus_grants":         "AND c.status NOT IN ('issued', 'cancelled')",
}

// ErrUndeclaredConsumptionShape is RK-W15P2-4's binding fail-closed
// behavior: "a child row bearing a parent_operation_id whose shape is not
// named in that declaration causes the consumption function to raise,
// never to under-count silently."
var ErrUndeclaredConsumptionShape = errors.New("economicop: operation_type has no declared consumption-record shape")

// ErrUnboundedRootAuthorization is returned by
// ValidateRootAuthorizationBounds for a root EOI of a value-creating
// operation_type that declares no finite budget. Distinct from
// ErrBudgetExhausted (a bounded authorization that ran out) - this is an
// authorization that was never bounded in the first place.
var ErrUnboundedRootAuthorization = errors.New("economicop: this operation_type may not be minted as an unbounded root authorization")

// boundedRootOperationTypes declares which operation_types may NEVER be
// minted as an unbounded root - the structural half of the fix
// security-architecture.md §W3P6.5 item 1 named and routed to `architect`
// (Stage 4H-B1 Wave 3 Phase 6), recorded as a binding rule in doc 34
// §5.6.
//
// The finding, restated: ConsumeRootBudget treats a nil recipient_ceiling
// as "no recipient check at all" and a nil intended_aggregate_value (on a
// non-null asset_code root) as "no value check at all". Wave 3's minting
// HTTP handler (newMintEconomicOperationHandler) requires all three bounds,
// but bonus.MintRootOperation - the mechanism's OWN entry point, and doc
// 34 §3.1's declared mint point for both Bonus operation types - accepted
// nil for all of them, so any future non-HTTP minting path (a job runner,
// a CRM adapter, a fixture promoted to production code) silently reopens
// the SEC-W15-02 decomposition vector one layer below the control. A
// ceiling that is optional to declare is not a ceiling; enforcing it only
// in one transport is enforcing it nowhere durable.
//
// This map is deliberately NOT "every operation_type". Doc 34 §2.2 states
// plainly that a null asset_code is legal and meaningful (a pure send
// campaign has no asset) and that such a root has no enforceable value
// budget at all (RK-W15P2-5) - so a blanket rule would contradict the
// document it enforces. The rule is therefore per-type and permit-by-
// enumeration: exactly the types that CREATE PLAYER-REDEEMABLE VALUE and
// already carry a declared consumption shape (consumptionShapes above)
// must be bounded. A future value-creating type that omits itself here
// gets no bound - which is why adding a consumptionShapes entry and a
// boundedRootOperationTypes entry belong in the same change, and why doc
// 34 §5.6 states that as a rule for whoever lands the next consumer.
var boundedRootOperationTypes = map[OperationType]bool{
	OperationBonusManualGrant:  true,
	OperationBonusBulkGrant:    true,
	OperationAPIInitiatedGrant: true,
}

// ValidateRootAuthorizationBounds is the mint-time half of doc 34 §3.2's
// containment checks: it refuses to let a root authorization for a
// value-creating operation_type come into existence without the finite
// bounds ConsumeRootBudget later enforces against. It is a pure function
// of the candidate row (no DB access), called by every mint point BEFORE
// the row is inserted.
//
// It is a no-op for a non-root lineage_kind (a child inherits its root's
// bounds by construction - §3.4's budget is a subtree aggregate keyed on
// root_operation_id, so a child carries no independent budget to bound)
// and for any operation_type not declared in boundedRootOperationTypes.
func ValidateRootAuthorizationBounds(op EconomicOperation) error {
	if op.LineageKind != LineageRoot {
		return nil
	}
	if !boundedRootOperationTypes[op.OperationType] {
		return nil
	}
	if op.AssetCode == nil || *op.AssetCode == "" {
		return fmt.Errorf("%w: %q requires an asset_code (a value budget is unenforceable without one, RK-W15P2-5)", ErrUnboundedRootAuthorization, op.OperationType)
	}
	if op.IntendedAggregateValue == nil || op.IntendedAggregateValue.Sign() <= 0 {
		return fmt.Errorf("%w: %q requires a positive intended_aggregate_value", ErrUnboundedRootAuthorization, op.OperationType)
	}
	if op.RecipientCeiling == nil || *op.RecipientCeiling <= 0 {
		return fmt.Errorf("%w: %q requires a positive recipient_ceiling", ErrUnboundedRootAuthorization, op.OperationType)
	}
	if op.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: %q requires expires_at (doc 34 §2.2: an authorization that can be executed forever is not an authorization)", ErrUnboundedRootAuthorization, op.OperationType)
	}
	switch op.SubjectScope {
	case SubjectScopeSingle:
		// Mirrors ConsumeRootBudget's own single_subject containment check
		// (above): a single_subject root with no subject_ref is a malformed
		// authorization, refused at mint rather than left to be refused at
		// every later consume. The ceiling must be exactly 1 - a
		// single_subject root authorizing more than one recipient is a
		// contradiction in terms, and is the shape an actor would use to
		// launder a set-shaped authorization through the narrower scope's
		// weaker declaration requirements.
		if op.SubjectRef == nil {
			return fmt.Errorf("%w: single_subject scope requires a subject_ref", ErrUnboundedRootAuthorization)
		}
		if *op.RecipientCeiling != 1 {
			return fmt.Errorf("%w: single_subject scope requires recipient_ceiling = 1, got %d", ErrUnboundedRootAuthorization, *op.RecipientCeiling)
		}
	case SubjectScopeNone:
		return fmt.Errorf("%w: %q creates player-redeemable value and cannot declare subject_scope = none", ErrUnboundedRootAuthorization, op.OperationType)
	}
	return nil
}

// ErrBudgetExhausted is returned by ConsumeRootBudget when the requested
// consumption (this one more recipient, this one more unit of value)
// would exceed the root's remaining recipient_ceiling or
// intended_aggregate_value, computed over the WHOLE lineage subtree
// (doc 34 §3.4) - never a single generation of parentage.
var ErrBudgetExhausted = errors.New("economicop: this execution would exceed the root operation's remaining budget")

// ConsumeRootBudget implements doc 34 §5.3 rule 3 / §5.4: SELECT ... FOR
// UPDATE on the ROOT row (never an intermediate page/item row), checked
// against the subtree-wide aggregate consumption AS IT STANDS BEFORE this
// execution, immediately before the caller's own effecting write (the
// caller MUST call this strictly after its own AssetAuthorization/RG/Risk
// gate chain has already passed, and MUST perform its effecting write
// (the Grant/BulkGrantJobItem insert) in the SAME transaction,
// immediately after this call returns nil - never before it, and never
// in a separate transaction).
//
// subjectPlayerAccountID/valueAmount are THIS execution's own
// contribution to the two budgets - recipient (a COUNT(DISTINCT), so a
// player already counted elsewhere in the subtree contributes 0 to the
// remaining check) and value (a SUM, always additive here - this
// function is never called for a compensation/clawback, which nets the
// value budget by a separate, not-yet-built path, doc 34 §3.2's
// 'compensation' row).
//
// valueAmount is nil for an operation_type/root with no enforceable value
// budget (a null asset_code root, doc 34 §2.2 RK-W15P2-5) - never
// silently treated as zero exposure.
func ConsumeRootBudget(ctx context.Context, tx pgx.Tx, tenantID, rootOperationID uuid.UUID, operationType OperationType, subjectPlayerAccountID uuid.UUID, valueAmount *big.Int) error {
	consumptionTable, declared := consumptionShapes[operationType]
	if !declared {
		return fmt.Errorf("%w: %q", ErrUndeclaredConsumptionShape, operationType)
	}

	var recipientCeiling *int32
	var intendedAggregate pgtype.Numeric
	var assetCode *string
	var status, approvalState, subjectScope string
	var subjectRef *uuid.UUID
	var expiresAt time.Time
	err := tx.QueryRow(ctx, `
		SELECT recipient_ceiling, intended_aggregate_value, asset_code, status, approval_state, expires_at,
		       subject_scope, subject_ref
		  FROM economic_operations
		 WHERE tenant_id = $1 AND operation_id = $2 AND lineage_kind = 'root'
		 FOR UPDATE`,
		tenantID, rootOperationID,
	).Scan(&recipientCeiling, &intendedAggregate, &assetCode, &status, &approvalState, &expiresAt, &subjectScope, &subjectRef)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrParentOperationNotFound
	}
	if err != nil {
		return fmt.Errorf("economicop: lock root operation for consume: %w", err)
	}
	if status != string(StatusOpen) {
		return ErrParentNotOpenOrExpired
	}
	if approvalState != string(ApprovalApproved) && approvalState != string(ApprovalNotRequired) {
		return ErrParentNotApproved
	}
	if !time.Now().UTC().Before(expiresAt) {
		return ErrParentNotOpenOrExpired
	}

	// Doc 34 §3.2's SUBJECT-SET CONTAINMENT check, for the one scope shape
	// this platform can evaluate today.
	//
	// FIX (Stage 4H-B1 Wave 3 Phase 6, `security`): doc 34 §2.2 defines
	// subject_ref as "For single_subject: the beneficiary's
	// player_account_id", and §3.2 names subject-set containment as one of
	// the five containment checks a child execution must satisfy against
	// its authorization. Nothing enforced it: CheckEntry takes no subject
	// argument at all (its own doc comment says so), and the code below
	// only ever compared a COUNT(DISTINCT) against recipient_ceiling -
	// never the identity of the subject. A single_subject root minted and
	// approved to grant to player A therefore authorized a grant to player
	// B exactly as well, which is SEC-W15-02's actor/subject-laundering
	// shape ("laundering an operation through a different actor/subject
	// pairing than the one actually authorized") reachable through the
	// manual-grant surface. A single_subject root with a NULL subject_ref
	// is a malformed authorization - refused rather than silently treated
	// as "any subject", mirroring RK-W15P2-5's own "never silently treated
	// as unlimited" posture for the value budget below.
	//
	// enumerated_set/criteria_defined are deliberately NOT narrowed here:
	// their containment is subject_set_hash/subject_definition_hash-shaped
	// (doc 34 §2.2), which no consumer in this repository materializes on
	// the EOI row yet. They remain bounded by recipient_ceiling, exactly as
	// before this fix - no control is loosened by this change.
	if subjectScope == string(SubjectScopeSingle) {
		if subjectRef == nil {
			return fmt.Errorf("%w: root %s declares single_subject scope with no subject_ref", ErrChildScopeExceedsParent, rootOperationID)
		}
		if subjectPlayerAccountID != uuid.Nil && *subjectRef != subjectPlayerAccountID {
			return fmt.Errorf("%w: root %s authorizes single subject %s, not %s", ErrChildScopeExceedsParent, rootOperationID, *subjectRef, subjectPlayerAccountID)
		}
	}

	realizedFilter := consumptionRealizedFilter[consumptionTable] // empty string if none declared - a no-op AND clause

	if recipientCeiling != nil {
		var alreadyCounted bool
		var currentCount int32
		countQuery := fmt.Sprintf(`
			SELECT COUNT(DISTINCT player_account_id),
			       COALESCE(bool_or(player_account_id = $3), false)
			  FROM %s c
			  JOIN economic_operations eoi ON eoi.operation_id = c.parent_operation_id
			 WHERE eoi.tenant_id = $1 AND eoi.root_operation_id = $2 %s`, consumptionTable, realizedFilter)
		if err := tx.QueryRow(ctx, countQuery, tenantID, rootOperationID, subjectPlayerAccountID).Scan(&currentCount, &alreadyCounted); err != nil {
			return fmt.Errorf("economicop: compute subtree recipient consumption: %w", err)
		}
		if !alreadyCounted && currentCount+1 > *recipientCeiling {
			return fmt.Errorf("%w: recipient_ceiling %d already reached %d", ErrBudgetExhausted, *recipientCeiling, currentCount)
		}
	}

	if intendedAggregate.Valid && valueAmount != nil {
		var currentValue pgtype.Numeric
		valueColumn := "granted_amount"
		valueQuery := fmt.Sprintf(`
			SELECT COALESCE(SUM(c.%s), 0)
			  FROM %s c
			  JOIN economic_operations eoi ON eoi.operation_id = c.parent_operation_id
			 WHERE eoi.tenant_id = $1 AND eoi.root_operation_id = $2 %s`, valueColumn, consumptionTable, realizedFilter)
		if err := tx.QueryRow(ctx, valueQuery, tenantID, rootOperationID).Scan(&currentValue); err != nil {
			return fmt.Errorf("economicop: compute subtree value consumption: %w", err)
		}
		currentBig, err := numericToBigInt(currentValue)
		if err != nil {
			return err
		}
		if currentBig == nil {
			currentBig = big.NewInt(0)
		}
		ceilingBig, err := numericToBigInt(intendedAggregate)
		if err != nil {
			return err
		}
		remaining := new(big.Int).Sub(ceilingBig, currentBig)
		if valueAmount.Cmp(remaining) > 0 {
			return fmt.Errorf("%w: intended_aggregate_value remaining %s, requested %s", ErrBudgetExhausted, remaining.String(), valueAmount.String())
		}
	} else if valueAmount != nil && assetCode == nil {
		// RK-W15P2-5: a null asset_code root has NO enforceable value
		// budget at all - an attempt to enforce one against it is a
		// configuration error, never silently treated as unlimited.
		return fmt.Errorf("economicop: root %s has no asset_code and therefore no enforceable value budget", rootOperationID)
	}

	return nil
}
