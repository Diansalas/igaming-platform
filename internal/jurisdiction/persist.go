package jurisdiction

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Persist writes ONE jurisdiction_resolutions row for r (canonical-model
// §5.1: "one row per resolution attempt, INCLUDING FAILURES") and returns
// a new Resolution carrying that row's id as RecordID(). It is the ONLY
// function in this package that writes anything - Resolve is read-only
// by construction (ReadOnlyQuerier), and persistence is a deliberately
// SEPARATE act (canonical-model §6.4): it happens either entirely outside
// the guarded transaction, or strictly AFTER the whole gate chain
// completes, immediately before or with the effecting write. Calling
// Persist while a gate chain is still running would reopen exactly the
// hazard §6.4 exists to prevent.
//
// tx must be a genuinely writable, tenant-scoped transaction (typically
// db.Pool.WithTenant - migration 0071's INSERT policy requires
// app.tenant_id to be set and app.player_account_id to be UNSET, so a
// player-scoped connection cannot use this path directly; a
// player-initiated operation that needs a persisted resolution must
// attribute the write to the service/system actor that performs it on
// the player's behalf, never call Persist from inside WithPlayerScope).
func Persist(ctx context.Context, tx pgx.Tx, r Resolution) (Resolution, error) {
	if r.tenantID == uuid.Nil {
		return Resolution{}, fmt.Errorf("%w: resolution has no tenant scope", ErrInvalidInput)
	}
	if r.outcome == "" {
		return Resolution{}, fmt.Errorf("%w: resolution has no outcome (was it produced by Resolve?)", ErrInvalidInput)
	}
	if r.requestedByActorType == "" {
		return Resolution{}, fmt.Errorf("%w: requested_by_actor_type is required", ErrInvalidInput)
	}

	considered := r.consideredBases
	if considered == nil {
		considered = []ConsideredBasis{}
	}
	consideredJSON, err := json.Marshal(considered)
	if err != nil {
		return Resolution{}, fmt.Errorf("jurisdiction: marshal considered_bases: %w", err)
	}

	var jurisdictionCode *string
	if r.outcome == Resolved {
		c := r.code
		jurisdictionCode = &c
	}
	var selectedBasis *string
	if r.selectedBasis != "" {
		b := string(r.selectedBasis)
		selectedBasis = &b
	}
	var confidenceClass *string
	if r.confidenceClass != "" {
		c := string(r.confidenceClass)
		confidenceClass = &c
	}
	var registryVersion *string
	if r.registryVersion != "" {
		v := r.registryVersion
		registryVersion = &v
	}

	var id uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO jurisdiction_resolutions (
			tenant_id, brand_id, player_account_id, operation_class,
			requested_by_actor_type, requested_by_actor_id,
			outcome, reason, jurisdiction_code, selected_basis,
			considered_bases, confidence_class, resolver_policy_version,
			registry_version, config_effective_from, as_of
		) VALUES (
			$1, $2, $3, $4,
			$5, $6,
			$7, $8, $9, $10,
			$11, $12, $13,
			$14, $15, $16
		) RETURNING id`,
		r.tenantID, r.brandID, r.playerAcctID, string(r.operationClass),
		string(r.requestedByActorType), r.requestedByActorID,
		string(r.outcome), string(r.reason), jurisdictionCode, selectedBasis,
		consideredJSON, confidenceClass, r.policyVersion,
		registryVersion, r.configEffectiveFrom, r.asOf,
	).Scan(&id)
	if err != nil {
		return Resolution{}, fmt.Errorf("jurisdiction: insert jurisdiction_resolutions: %w", err)
	}

	out := r
	out.recordID = id
	return out, nil
}
