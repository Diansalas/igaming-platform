// Stage 4H-B0-R6, Workstream E: implements ADR 0034 §14's
// OpenBetSelfExclusionPolicy as a general, standalone configuration/
// resolution primitive - see migrations/0043_open_bet_self_exclusion_
// policy.up.sql for the schema this file operates on, and
// docs/security/security-architecture.md's Stage 4H-B0-R5 section for
// findings S-8 (as-of tampering window) and S-9 (enumeration
// completeness), which this file closes.
//
// This file deliberately has NO sportsbook/open-bet concept: no
// sportsbook implementation exists yet in this codebase (ADR 0034 §14
// Consequences addendum), so nothing here enumerates, voids, or settles
// a bet. What it provides a future sportsbook implementation:
//
//   - SetOpenBetSelfExclusionPolicy / ResolveOpenBetSelfExclusionPolicy:
//     the configuration write/read primitive - a policy VALUE plus its
//     provenance and audit trail, never a decision about what to DO with
//     that value (that is a future caller's job).
//   - RecordEnumerationRun / StartEnumerationRun / CompleteEnumerationRun /
//     FailEnumerationRun: the per-self-exclusion-event completion record
//     security finding S-9 requires, so a future listener's own
//     enumeration/dispatch step (ADR 0034 §14.5) can prove it ran to
//     completion instead of leaving "no record" indistinguishable from
//     "nothing was in scope."
//   - FindMissingEnumerationRuns: a reconciliation query for the
//     narrower, honestly-scoped completeness check this stage can
//     actually deliver without sportsbook data - see its own doc comment
//     for exactly what it does and does not detect.
//
// Nothing here selects the platform-wide DEFAULT value between
// SETTLE_NORMALLY and VOID_ON_SELF_EXCLUSION for a jurisdiction that has
// configured neither - ADR 0034 §14.9 leaves that an explicit human/legal
// decision. Where no jurisdiction floor is configured, Resolve reports
// Configured=false and every real caller MUST treat that as fail-closed
// (deny the operation the policy would otherwise decide, or refuse to
// enable sportsbook for that jurisdiction) - this package never
// substitutes an implicit default.
package rg

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
)

// OpenBetSelfExclusionPolicy is ADR 0034 §14.1's exactly-two-valued
// configuration domain. The names match the ADR's own §14.1 text
// verbatim (VOID_ON_SELF_EXCLUSION, not this stage directive's alternate
// "VOID_OPEN_BETS" spelling - reconciled to the ADR's existing name per
// this stage's own instruction not to introduce a third name).
type OpenBetSelfExclusionPolicy string

const (
	PolicySettleNormally      OpenBetSelfExclusionPolicy = "SETTLE_NORMALLY"
	PolicyVoidOnSelfExclusion OpenBetSelfExclusionPolicy = "VOID_ON_SELF_EXCLUSION"
)

// openBetSelfExclusionPolicyStrictness is the Go-side mirror of
// migration 0043's open_bet_self_exclusion_policy_strictness() SQL
// function - ONE canonical ordering (ADR 0034 §14.2:
// VOID_ON_SELF_EXCLUSION >= SETTLE_NORMALLY), expressed twice (SQL for
// the write-time trigger, Go for the read-time aggregation) because the
// two layers run in different processes. See
// TestOpenBetSelfExclusionPolicyStrictness_MatchesDatabase for the
// parity check that keeps them from silently drifting apart.
func openBetSelfExclusionPolicyStrictness(v OpenBetSelfExclusionPolicy) (int, error) {
	switch v {
	case PolicySettleNormally:
		return 0, nil
	case PolicyVoidOnSelfExclusion:
		return 1, nil
	default:
		return 0, fmt.Errorf("%w: unknown OpenBetSelfExclusionPolicy value %q", ErrInvalidInput, v)
	}
}

// Sentinel errors for the policy write/read boundary - mirrors this
// package's existing ErrInvalidInput/ErrNotFound-style convention
// (rg.go, and internal/risk's identical pattern) rather than an opaque
// wrapped string a caller cannot distinguish programmatically.
var (
	// ErrNoJurisdictionFloor is returned when a caller attempts to write
	// a tenant/brand-scoped override for a jurisdiction that has no
	// currently-effective floor row at the requested effective_from -
	// there is nothing for the override to legitimately tighten.
	ErrNoJurisdictionFloor = errors.New("rg: no jurisdiction floor configured for this scope/instant")
	// ErrPolicyWouldLoosenFloor is returned when a tenant/brand override
	// would be strictly less strict than its applicable floor.
	ErrPolicyWouldLoosenFloor = errors.New("rg: policy value would loosen the applicable jurisdiction/tenant floor")
	// ErrNonMonotonicWrite is returned when a new version's effective_from
	// does not strictly follow the currently-open version for the exact
	// same scope.
	ErrNonMonotonicWrite = errors.New("rg: effective_from must be strictly after the current version's effective_from for this exact scope")
	// ErrJurisdictionFloorBackdated is returned when a jurisdiction-floor
	// write (TenantID == nil) supplies an EffectiveFrom before the
	// current instant. Stage 4H-B0-R6 fix 4: code-reviewer traced a
	// concrete exploit the floor's deliberate tighten-only exemption
	// otherwise allows - an operator backdates a new, permissive floor
	// row to an instant BEFORE a real self-exclusion already occurred
	// under the old, stricter floor, so a later Resolve(asOf=<the
	// self-exclusion instant>) call would retroactively return the
	// permissive value for a player who was excluded under the strict
	// one. The write IS audited either way (detectable after the fact),
	// but this closes it at write time: a floor change may only take
	// effect now or later, never retroactively. This is a temporal-
	// integrity rule, not a compliance-value choice - it does not select
	// SETTLE_NORMALLY vs VOID_ON_SELF_EXCLUSION, which ADR 0034 §14.9
	// still leaves to the jurisdiction. Migration 0049's identical
	// database-level trigger is the authoritative backstop; this is the
	// fast, cleanly-audited Go-layer rejection, matching the tighten-only
	// check's own two-layer pattern below.
	ErrJurisdictionFloorBackdated = errors.New("rg: a jurisdiction-floor write may not be backdated - effective_from must be now or later")
)

// floorBackdatingTolerance is the Go-side mirror of migration 0049's
// identical INTERVAL '5 seconds' tolerance in
// open_bet_self_exclusion_policies_enforce_floor_no_backdating() - see
// TestFloorBackdatingTolerance_MatchesDatabase for the parity check, and
// that trigger's own doc comment for why a zero-tolerance comparison
// against clock_timestamp() would reject every legitimate default "now"
// write (clock_timestamp() strictly advances between the two separate
// statements this function issues - the SELECT that resolves "now" and
// the later INSERT - so a bare "before now" check always fires from
// ordinary round-trip latency alone, never just from a genuine backdating
// attempt). Not a security weakening: the exploit this fix closes
// backdates by the gap between an operator's later action and an
// earlier self-exclusion instant, which is never a matter of
// milliseconds.
const floorBackdatingTolerance = 5 * time.Second

// OpenBetSelfExclusionPolicyRow mirrors one open_bet_self_exclusion_
// policies row. Its own ID doubles as ADR 0034 §14.8's "stable version
// identifier."
type OpenBetSelfExclusionPolicyRow struct {
	ID                 uuid.UUID
	JurisdictionCode   string
	TenantID           *uuid.UUID
	BrandID            *uuid.UUID
	PolicyValue        OpenBetSelfExclusionPolicy
	EffectiveFrom      time.Time
	EffectiveTo        *time.Time
	ReasonCode         string
	CreatedByActorType audit.ActorType
	CreatedByActorID   uuid.UUID
	CreatedAt          time.Time
}

const openBetPolicyColumns = `id, jurisdiction_code, tenant_id, brand_id, policy_value,
	effective_from, effective_to, reason_code, created_by_actor_type, created_by_actor_id, created_at`

func scanOpenBetPolicyRow(row pgx.Row) (OpenBetSelfExclusionPolicyRow, error) {
	var r OpenBetSelfExclusionPolicyRow
	var policyValue, createdByActorType string
	if err := row.Scan(
		&r.ID, &r.JurisdictionCode, &r.TenantID, &r.BrandID, &policyValue,
		&r.EffectiveFrom, &r.EffectiveTo, &r.ReasonCode, &createdByActorType, &r.CreatedByActorID, &r.CreatedAt,
	); err != nil {
		return OpenBetSelfExclusionPolicyRow{}, err
	}
	r.PolicyValue = OpenBetSelfExclusionPolicy(policyValue)
	r.CreatedByActorType = audit.ActorType(createdByActorType)
	return r, nil
}

func scanOpenBetPolicyRows(rows pgx.Rows) ([]OpenBetSelfExclusionPolicyRow, error) {
	defer rows.Close()
	var out []OpenBetSelfExclusionPolicyRow
	for rows.Next() {
		r, err := scanOpenBetPolicyRow(rows)
		if err != nil {
			return nil, fmt.Errorf("rg: scan open_bet_self_exclusion_policies row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// lockOpenBetSelfExclusionPolicyScope takes a transaction-scoped advisory
// lock keyed on the exact (jurisdiction, tenant, brand) scope being
// written, mirroring lockPerson's own rationale (rg.go): there is no
// natural row to SELECT ... FOR UPDATE for a scope that may not have any
// row yet, so writers serialize on the FACT of "who is currently writing
// this scope" instead. Every writer of this scope takes this lock before
// reading or writing anything else for it, closing the identical
// TOCTOU race lockPerson closes for player_restrictions.
func lockOpenBetSelfExclusionPolicyScope(ctx context.Context, tx pgx.Tx, jurisdictionCode string, tenantID, brandID *uuid.UUID) error {
	key := scopeLockKey(jurisdictionCode, tenantID, brandID)
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext('open_bet_self_exclusion_policies'), $1)`,
		int32(key),
	); err != nil {
		return fmt.Errorf("rg: lock open-bet self-exclusion policy scope: %w", err)
	}
	return nil
}

// scopeLockKey derives a deterministic 32-bit lock key from the scope
// triple. A collision between two different scopes only ever costs
// unnecessary serialization (both scopes briefly wait on the same
// advisory lock), never incorrect behavior - the lock is a
// synchronization aid, not itself the source of correctness (the
// tighten-only trigger and the max-strictness read are).
func scopeLockKey(jurisdictionCode string, tenantID, brandID *uuid.UUID) int32 {
	h := sha256.New()
	h.Write([]byte(jurisdictionCode))
	if tenantID != nil {
		h.Write(tenantID[:])
	}
	if brandID != nil {
		h.Write(brandID[:])
	}
	sum := h.Sum(nil)
	return int32(binary.BigEndian.Uint32(sum[:4]))
}

// resolvePolicyRows returns every open_bet_self_exclusion_policies row
// applicable to (jurisdictionCode, tenantID, brandID) - jurisdiction-only
// rows always match; a tenant-level row matches when tenantID equals it;
// a brand-level row matches only when BOTH tenantID and brandID equal it
// - that is currently effective AT asOf. tenantID == nil restricts the
// result to jurisdiction-only rows (used both by Resolve for a pure
// jurisdiction-floor query and internally by the write-time floor check
// for a tenant-level write); brandID == nil similarly excludes
// brand-scoped rows (used by the write-time floor check for a
// brand-level write, which must see the jurisdiction AND tenant rows but
// not sibling brand rows).
func resolvePolicyRows(ctx context.Context, tx pgx.Tx, jurisdictionCode string, tenantID, brandID *uuid.UUID, asOf time.Time) ([]OpenBetSelfExclusionPolicyRow, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+openBetPolicyColumns+` FROM open_bet_self_exclusion_policies
		 WHERE jurisdiction_code = $1
		   AND (tenant_id IS NULL OR tenant_id = $2)
		   AND (brand_id IS NULL OR brand_id = $3)
		   AND effective_from <= $4
		   AND (effective_to IS NULL OR effective_to > $4)
		 ORDER BY effective_from`,
		jurisdictionCode, tenantID, brandID, asOf,
	)
	if err != nil {
		return nil, fmt.Errorf("rg: resolve open-bet self-exclusion policy rows: %w", err)
	}
	return scanOpenBetPolicyRows(rows)
}

// aggregation is the pure (no I/O) result of reducing a set of already-
// fetched rows to a single resolved decision - split out from
// resolvePolicyRows deliberately so this reduction logic (as-of
// filtering already applied by the caller's SQL, tighten-only-safe
// max(), fail-closed-when-no-floor) can be unit tested directly against
// fabricated row slices, with no database involved.
type aggregation struct {
	HasJurisdictionFloor bool
	MaxValue             OpenBetSelfExclusionPolicy
	MaxStrictness        int
}

func aggregateStrictness(rows []OpenBetSelfExclusionPolicyRow) (aggregation, error) {
	var agg aggregation
	best := -1
	for _, r := range rows {
		if r.TenantID == nil && r.BrandID == nil {
			agg.HasJurisdictionFloor = true
		}
		s, err := openBetSelfExclusionPolicyStrictness(r.PolicyValue)
		if err != nil {
			return aggregation{}, err
		}
		if s > best {
			best = s
			agg.MaxValue = r.PolicyValue
		}
	}
	agg.MaxStrictness = best
	return agg, nil
}

// currentRowForExactScope returns the currently-open (effective_to IS
// NULL) row for the EXACT scope (jurisdictionCode, tenantID, brandID) -
// used only to find the version a new write should close out, never for
// resolution (resolution always uses resolvePolicyRows' hierarchical,
// as-of query above).
func currentRowForExactScope(ctx context.Context, tx pgx.Tx, jurisdictionCode string, tenantID, brandID *uuid.UUID) (*OpenBetSelfExclusionPolicyRow, error) {
	row, err := scanOpenBetPolicyRow(tx.QueryRow(ctx,
		`SELECT `+openBetPolicyColumns+` FROM open_bet_self_exclusion_policies
		 WHERE jurisdiction_code = $1
		   AND tenant_id IS NOT DISTINCT FROM $2
		   AND brand_id IS NOT DISTINCT FROM $3
		   AND effective_to IS NULL
		 ORDER BY effective_from DESC
		 LIMIT 1`,
		jurisdictionCode, tenantID, brandID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("rg: find current open-bet self-exclusion policy row: %w", err)
	}
	return &row, nil
}

// SetOpenBetSelfExclusionPolicyParams is SetOpenBetSelfExclusionPolicy's
// input. TenantID == nil writes/replaces the jurisdiction FLOOR itself
// (no tighten-only check applies - a jurisdiction's own regulatory value
// may change in either direction, ADR 0034 §14.8); TenantID != nil,
// BrandID == nil writes a tenant-level override; both set writes a
// brand-level override. tx MUST already be scoped consistently with
// TenantID exactly like risk_rules.CreateRule (db.WithoutTenant for a
// platform/jurisdiction-floor write, db.WithTenant(*TenantID, ...)
// otherwise) - migration 0043's tenant_and_platform_write RLS policy
// requires it.
type SetOpenBetSelfExclusionPolicyParams struct {
	JurisdictionCode string
	TenantID         *uuid.UUID
	BrandID          *uuid.UUID
	PolicyValue      OpenBetSelfExclusionPolicy
	// EffectiveFrom == nil means "effective now" - resolved via
	// clock_timestamp() inside this same transaction (directive item 4:
	// authoritative DB time, re-evaluated per call so a write queued
	// behind lockOpenBetSelfExclusionPolicyScope is stamped with the
	// instant it actually proceeds, not an application-server wall-clock
	// value computed before entering the transaction). A future-dated
	// value schedules a change to take effect later - always permitted.
	// Stage 4H-B0-R6 fix 4: for a JURISDICTION-FLOOR write (TenantID ==
	// nil) specifically, a PAST value is now rejected outright
	// (ErrJurisdictionFloorBackdated) - see that error's own doc comment
	// for why. A tenant/brand override may still supply a past
	// EffectiveFrom (its own per-scope monotonicity check below, plus the
	// tighten-only trigger, are what keep that safe for an override -
	// this restriction applies only to the floor's tighten-only
	// exemption).
	EffectiveFrom *time.Time
	ReasonCode    string
	ActorType     audit.ActorType
	ActorID       uuid.UUID
	IPAddress     string
	UserAgent     string
	RequestID     string
}

// SetOpenBetSelfExclusionPolicy inserts a new versioned policy row,
// enforcing (in Go, fast-fail, cleanly audited) the identical
// tighten-only/floor-required rule migration 0043's database trigger
// enforces independently as the authoritative backstop (directive item
// 2: BOTH layers, not either/or). Every call - success or rejection - is
// audited (directive item 7).
func SetOpenBetSelfExclusionPolicy(ctx context.Context, tx pgx.Tx, params SetOpenBetSelfExclusionPolicyParams) (OpenBetSelfExclusionPolicyRow, error) {
	if params.JurisdictionCode == "" {
		return OpenBetSelfExclusionPolicyRow{}, fmt.Errorf("%w: jurisdiction_code is required", ErrInvalidInput)
	}
	if _, err := openBetSelfExclusionPolicyStrictness(params.PolicyValue); err != nil {
		return OpenBetSelfExclusionPolicyRow{}, err
	}
	if params.BrandID != nil && params.TenantID == nil {
		return OpenBetSelfExclusionPolicyRow{}, fmt.Errorf("%w: brand_id requires tenant_id", ErrInvalidInput)
	}
	if params.ReasonCode == "" {
		return OpenBetSelfExclusionPolicyRow{}, fmt.Errorf("%w: reason_code is required", ErrInvalidInput)
	}
	if params.ActorType != audit.ActorSystem && params.ActorID == uuid.Nil {
		return OpenBetSelfExclusionPolicyRow{}, fmt.Errorf("%w: actor_id is required unless actor_type is system", ErrInvalidInput)
	}

	if err := lockOpenBetSelfExclusionPolicyScope(ctx, tx, params.JurisdictionCode, params.TenantID, params.BrandID); err != nil {
		return OpenBetSelfExclusionPolicyRow{}, err
	}

	auditTenantID := uuid.Nil
	if params.TenantID != nil {
		auditTenantID = *params.TenantID
	}

	var effectiveFrom time.Time
	if params.EffectiveFrom != nil {
		effectiveFrom = *params.EffectiveFrom
		// Fix 4: a jurisdiction-floor write (TenantID == nil) may never
		// be backdated - see ErrJurisdictionFloorBackdated's own doc
		// comment for the exploit this closes. Only the floor is checked
		// here: a tenant/brand override's own tighten-only trigger
		// already prevents the equivalent exploit shape for it.
		if params.TenantID == nil {
			var dbNow time.Time
			if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&dbNow); err != nil {
				return OpenBetSelfExclusionPolicyRow{}, fmt.Errorf("rg: resolve current db time: %w", err)
			}
			if effectiveFrom.Before(dbNow.Add(-floorBackdatingTolerance)) {
				if auditErr := audit.Record(ctx, tx, audit.Entry{
					TenantID: auditTenantID, ActorType: params.ActorType, ActorID: params.ActorID,
					Action: "rg.open_bet_self_exclusion_policy.write_denied", TargetType: "open_bet_self_exclusion_policy",
					TargetID: "", Outcome: audit.OutcomeDenied, IPAddress: params.IPAddress, UserAgent: params.UserAgent, RequestID: params.RequestID,
					Metadata: map[string]any{
						"reason": "jurisdiction_floor_backdated", "jurisdiction_code": params.JurisdictionCode,
						"attempted_effective_from": effectiveFrom, "db_now": dbNow,
						"attempted_policy_value": string(params.PolicyValue), "reason_code": params.ReasonCode,
					},
				}); auditErr != nil {
					return OpenBetSelfExclusionPolicyRow{}, fmt.Errorf("rg: audit denied open-bet self-exclusion policy write: %w", auditErr)
				}
				return OpenBetSelfExclusionPolicyRow{}, ErrJurisdictionFloorBackdated
			}
		}
	} else {
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&effectiveFrom); err != nil {
			return OpenBetSelfExclusionPolicyRow{}, fmt.Errorf("rg: resolve effective_from: %w", err)
		}
	}

	newStrictness, _ := openBetSelfExclusionPolicyStrictness(params.PolicyValue)

	// Tighten-only pre-check (Go layer) - only applies to tenant/brand
	// overrides; a jurisdiction-floor write (TenantID == nil) has no
	// floor of its own to violate.
	if params.TenantID != nil {
		floorTenantID := params.TenantID
		if params.BrandID == nil {
			// A tenant-level write's floor is the jurisdiction row only.
			floorTenantID = nil
		}
		floorRows, err := resolvePolicyRows(ctx, tx, params.JurisdictionCode, floorTenantID, nil, effectiveFrom)
		if err != nil {
			return OpenBetSelfExclusionPolicyRow{}, err
		}
		agg, err := aggregateStrictness(floorRows)
		if err != nil {
			return OpenBetSelfExclusionPolicyRow{}, err
		}
		if len(floorRows) == 0 {
			if auditErr := audit.Record(ctx, tx, audit.Entry{
				TenantID: auditTenantID, ActorType: params.ActorType, ActorID: params.ActorID,
				Action: "rg.open_bet_self_exclusion_policy.write_denied", TargetType: "open_bet_self_exclusion_policy",
				TargetID: "", Outcome: audit.OutcomeDenied, IPAddress: params.IPAddress, UserAgent: params.UserAgent, RequestID: params.RequestID,
				Metadata: map[string]any{
					"reason": "no_jurisdiction_floor", "jurisdiction_code": params.JurisdictionCode,
					"tenant_id": uuidPtrString(params.TenantID), "brand_id": uuidPtrString(params.BrandID),
					"attempted_policy_value": string(params.PolicyValue), "reason_code": params.ReasonCode,
				},
			}); auditErr != nil {
				return OpenBetSelfExclusionPolicyRow{}, fmt.Errorf("rg: audit denied open-bet self-exclusion policy write: %w", auditErr)
			}
			return OpenBetSelfExclusionPolicyRow{}, ErrNoJurisdictionFloor
		}
		if newStrictness < agg.MaxStrictness {
			if auditErr := audit.Record(ctx, tx, audit.Entry{
				TenantID: auditTenantID, ActorType: params.ActorType, ActorID: params.ActorID,
				Action: "rg.open_bet_self_exclusion_policy.write_denied", TargetType: "open_bet_self_exclusion_policy",
				TargetID: "", Outcome: audit.OutcomeDenied, IPAddress: params.IPAddress, UserAgent: params.UserAgent, RequestID: params.RequestID,
				Metadata: map[string]any{
					"reason": "would_loosen_floor", "jurisdiction_code": params.JurisdictionCode,
					"tenant_id": uuidPtrString(params.TenantID), "brand_id": uuidPtrString(params.BrandID),
					"attempted_policy_value": string(params.PolicyValue), "required_floor_value": string(agg.MaxValue),
					"reason_code": params.ReasonCode,
				},
			}); auditErr != nil {
				return OpenBetSelfExclusionPolicyRow{}, fmt.Errorf("rg: audit denied open-bet self-exclusion policy write: %w", auditErr)
			}
			return OpenBetSelfExclusionPolicyRow{}, ErrPolicyWouldLoosenFloor
		}
	}

	// Close out the currently-open version for this EXACT scope, if any -
	// keeps the table's "current" view legible; not load-bearing for the
	// tighten-only safety property itself (resolution's own max() over
	// all applicable rows stays safe even if this step is skipped by a
	// bug, since a not-yet-closed stricter row can only ever push the
	// resolved value UP, never down - directive item 2's own reasoning
	// for why read-time max() is a real backstop, applied here too).
	existing, err := currentRowForExactScope(ctx, tx, params.JurisdictionCode, params.TenantID, params.BrandID)
	if err != nil {
		return OpenBetSelfExclusionPolicyRow{}, err
	}
	if existing != nil {
		if !effectiveFrom.After(existing.EffectiveFrom) {
			return OpenBetSelfExclusionPolicyRow{}, fmt.Errorf("%w: existing version effective_from=%s, new effective_from=%s",
				ErrNonMonotonicWrite, existing.EffectiveFrom, effectiveFrom)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE open_bet_self_exclusion_policies SET effective_to = $1 WHERE id = $2`,
			effectiveFrom, existing.ID,
		); err != nil {
			return OpenBetSelfExclusionPolicyRow{}, fmt.Errorf("rg: close prior open-bet self-exclusion policy version: %w", err)
		}
	}

	r := OpenBetSelfExclusionPolicyRow{
		ID: uuid.New(), JurisdictionCode: params.JurisdictionCode, TenantID: params.TenantID, BrandID: params.BrandID,
		PolicyValue: params.PolicyValue, EffectiveFrom: effectiveFrom, ReasonCode: params.ReasonCode,
		CreatedByActorType: params.ActorType, CreatedByActorID: params.ActorID,
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO open_bet_self_exclusion_policies
			(id, jurisdiction_code, tenant_id, brand_id, policy_value, effective_from, reason_code, created_by_actor_type, created_by_actor_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		r.ID, r.JurisdictionCode, r.TenantID, r.BrandID, string(r.PolicyValue), r.EffectiveFrom, r.ReasonCode,
		string(r.CreatedByActorType), r.CreatedByActorID,
	); err != nil {
		return OpenBetSelfExclusionPolicyRow{}, fmt.Errorf("rg: insert open-bet self-exclusion policy: %w", err)
	}
	row, err := scanOpenBetPolicyRow(tx.QueryRow(ctx, `SELECT `+openBetPolicyColumns+` FROM open_bet_self_exclusion_policies WHERE id = $1`, r.ID))
	if err != nil {
		return OpenBetSelfExclusionPolicyRow{}, fmt.Errorf("rg: reload open-bet self-exclusion policy: %w", err)
	}

	beforeValue := "none"
	if existing != nil {
		beforeValue = string(existing.PolicyValue)
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: auditTenantID, ActorType: params.ActorType, ActorID: params.ActorID,
		Action: "rg.open_bet_self_exclusion_policy.set", TargetType: "open_bet_self_exclusion_policy", TargetID: row.ID.String(),
		Outcome: audit.OutcomeSuccess, IPAddress: params.IPAddress, UserAgent: params.UserAgent, RequestID: params.RequestID,
		Metadata: map[string]any{
			"jurisdiction_code": row.JurisdictionCode, "tenant_id": uuidPtrString(row.TenantID), "brand_id": uuidPtrString(row.BrandID),
			"before_policy_value": beforeValue, "after_policy_value": string(row.PolicyValue),
			"effective_from": row.EffectiveFrom, "reason_code": row.ReasonCode, "policy_version": row.ID.String(),
		},
	}); err != nil {
		return OpenBetSelfExclusionPolicyRow{}, fmt.Errorf("rg: audit open-bet self-exclusion policy write: %w", err)
	}
	return row, nil
}

// ResolveOpenBetSelfExclusionPolicyParams is
// ResolveOpenBetSelfExclusionPolicy's input.
type ResolveOpenBetSelfExclusionPolicyParams struct {
	JurisdictionCode string
	TenantID         *uuid.UUID
	BrandID          *uuid.UUID
	// AsOf MUST be the self-exclusion's own effective timestamp (ADR 0034
	// §14.3; security finding S-8) - the instant the triggering
	// restriction became effective (player_restrictions.starts_at for a
	// self-exclusion), never a bare "now" read at whatever later instant
	// a listener happens to run. A zero value is rejected outright: this
	// package will not silently substitute clock_timestamp() here,
	// specifically because doing so is the exact tampering window S-8
	// found (an operator racing a permissive config change into effect
	// between the self-exclusion instant and whenever enforcement runs).
	AsOf time.Time
	// Audit context - every resolution is itself audited (directive item
	// 7). AuditTenantID is uuid.Nil for a platform-level resolution (no
	// TenantID supplied).
	AuditTenantID uuid.UUID
	ActorType     audit.ActorType
	ActorID       uuid.UUID
	// RestrictionID, if supplied, is recorded in the audit metadata only,
	// as provenance linking this resolution back to the self-exclusion
	// event that triggered it - Resolve itself has no restriction/bet
	// concept.
	RestrictionID *uuid.UUID
	IPAddress     string
	UserAgent     string
	RequestID     string
}

// ResolvedOpenBetSelfExclusionPolicy is ResolveOpenBetSelfExclusionPolicy's
// output. Configured == false means NO jurisdiction floor is configured
// for this scope at AsOf - the fail-closed state every real caller MUST
// treat as "deny the operation this policy would otherwise decide" or
// "refuse to enable sportsbook for this jurisdiction." Value is only
// meaningful when Configured is true.
type ResolvedOpenBetSelfExclusionPolicy struct {
	Configured       bool
	Value            OpenBetSelfExclusionPolicy
	JurisdictionCode string
	AsOf             time.Time
	// ContributingRows is every row that fed into the max() computation
	// (or, when Configured is false, every row found that still failed
	// to include a jurisdiction floor) - provenance for audit/debugging.
	ContributingRows []OpenBetSelfExclusionPolicyRow
}

// ResolveOpenBetSelfExclusionPolicy is the general resolution primitive:
// a policy VALUE plus its provenance, resolved as-of a specific instant,
// tighten-only-safe by construction (max(strictness) across every
// applicable scope - directive item 2's read-time backstop), fail-closed
// when no jurisdiction floor is configured. It does not enumerate,
// void, or settle anything - see this file's own package doc comment.
func ResolveOpenBetSelfExclusionPolicy(ctx context.Context, tx pgx.Tx, params ResolveOpenBetSelfExclusionPolicyParams) (ResolvedOpenBetSelfExclusionPolicy, error) {
	if params.JurisdictionCode == "" {
		return ResolvedOpenBetSelfExclusionPolicy{}, fmt.Errorf("%w: jurisdiction_code is required", ErrInvalidInput)
	}
	if params.AsOf.IsZero() {
		return ResolvedOpenBetSelfExclusionPolicy{}, fmt.Errorf("%w: as_of is required and must be the self-exclusion's own effective timestamp, never a zero value implying \"now\"", ErrInvalidInput)
	}
	if params.ActorType != audit.ActorSystem && params.ActorID == uuid.Nil {
		return ResolvedOpenBetSelfExclusionPolicy{}, fmt.Errorf("%w: actor_id is required unless actor_type is system", ErrInvalidInput)
	}

	rows, err := resolvePolicyRows(ctx, tx, params.JurisdictionCode, params.TenantID, params.BrandID, params.AsOf)
	if err != nil {
		return ResolvedOpenBetSelfExclusionPolicy{}, err
	}
	agg, err := aggregateStrictness(rows)
	if err != nil {
		return ResolvedOpenBetSelfExclusionPolicy{}, err
	}

	resolved := ResolvedOpenBetSelfExclusionPolicy{
		Configured: agg.HasJurisdictionFloor, JurisdictionCode: params.JurisdictionCode, AsOf: params.AsOf,
		ContributingRows: rows,
	}
	if resolved.Configured {
		resolved.Value = agg.MaxValue
	}

	metadata := map[string]any{
		"jurisdiction_code": params.JurisdictionCode, "tenant_id": uuidPtrString(params.TenantID),
		"brand_id": uuidPtrString(params.BrandID), "as_of": params.AsOf, "configured": resolved.Configured,
	}
	if resolved.Configured {
		metadata["resolved_value"] = string(resolved.Value)
	}
	if params.RestrictionID != nil {
		metadata["restriction_id"] = params.RestrictionID.String()
	}
	rowIDs := make([]string, len(rows))
	for i, r := range rows {
		rowIDs[i] = r.ID.String()
	}
	metadata["contributing_policy_version_ids"] = rowIDs

	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: params.AuditTenantID, ActorType: params.ActorType, ActorID: params.ActorID,
		Action: "rg.open_bet_self_exclusion_policy.resolved", TargetType: "open_bet_self_exclusion_policy",
		TargetID: params.JurisdictionCode, Outcome: audit.OutcomeSuccess,
		IPAddress: params.IPAddress, UserAgent: params.UserAgent, RequestID: params.RequestID, Metadata: metadata,
	}); err != nil {
		return ResolvedOpenBetSelfExclusionPolicy{}, fmt.Errorf("rg: audit open-bet self-exclusion policy resolution: %w", err)
	}
	return resolved, nil
}

func uuidPtrString(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}
