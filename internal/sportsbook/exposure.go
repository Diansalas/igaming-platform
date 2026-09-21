// Part B2 of docs/decisions/0083-sportsbook-jurisdiction-gating-and-
// cumulative-exposure.md (§6.2): the cross-player, per-(scope_kind,
// asset_code) book-exposure gate. This is a DIFFERENT concept from
// internal/risk's player-scoped cumulative cap (§6.0's table) - it
// aggregates POTENTIAL_RETURN (a domain projection, ADR 0038 §2) across
// every player in the tenant on one event/market/selection, never a
// ledger-visible fact, and it therefore lives here, in
// internal/sportsbook, alongside risk.Evaluate rather than inside it
// (§6.2.1's four structural reasons this cannot live in risk_rules).
//
// Stage 9.2 Workstream B / Wave 3 - lands AFTER Wave 2 (jurisdiction.go,
// the Part C jurisdiction gate) per ADR 0083 §9.4's wave split: both waves
// edit PlaceBet's own composed call order, and landing them concurrently
// is explicitly not fine.
package sportsbook

import (
	"context"
	"fmt"
	"math/big"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// exposureParams is evaluateExposureLimits' input. Every id is
// server-derived; IncrementalPotentialReturn is THIS bet's own
// potential_return, computed by computePotentialReturn (orchestrator.go)
// BEFORE this call - ADR 0083 §7.1 step 10 moved that computation earlier
// in PlaceBet precisely so this value would already exist here.
type exposureParams struct {
	TenantID  uuid.UUID
	BrandID   uuid.UUID
	Scope     catalogueScope // Event/Market/Selection, from getSelectionWithContext - no extra query.
	AssetCode string
	// IncrementalPotentialReturn is THIS bet's own potential_return, in
	// AssetCode's minor units.
	IncrementalPotentialReturn int64
}

// ExposureDecision is the gate's output. Deliberately carries NO amount
// and NO threshold (INV-SB-EXP-2): the aggregate and the ceiling are
// trading-book intelligence and must never reach a player-facing
// response. ScopeKind and LimitID exist for the audit record only -
// PlaceBet must not put either into PlaceBetResult.RejectionCode/
// RejectionMessage.
type ExposureDecision struct {
	Breached  bool
	ScopeKind string
	LimitID   uuid.UUID
}

// verifyTenantScopedExposureConnection proves tx is scoped the way
// sb_exposure_limits' RLS policy requires BEFORE any limit is read -
// mirrors internal/risk.verifyConnectionScope byte-for-byte (ADR 0083
// §6.2.4's closing line: "this is asserted, exactly as risk.Evaluate's
// verifyConnectionScope asserts the same precondition, rather than
// assumed"). Duplicated rather than imported: internal/sportsbook must
// not import internal/risk's unexported helpers, and this package already
// duplicates evaluateAndAuditEligibility/sliceContainsString for the
// identical package-boundary reason (see those functions' own doc
// comments).
//
// The aggregate this package computes spans EVERY PLAYER in the tenant
// (§6.2.2) - exactly the shape sportsbook_bets' tenant_staff_scope policy
// permits and its player_self_scope policy does not (migration 0078). A
// player-scoped connection must never reach this query: RLS would silently
// return a partial (single-player) aggregate rather than an error, which
// is precisely the "silently-partial" failure mode this assertion exists
// to convert into a loud, fail-closed one.
func verifyTenantScopedExposureConnection(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error {
	var scopedTenant, scopedPlayer *string
	err := tx.QueryRow(ctx,
		`SELECT NULLIF(current_setting('app.tenant_id', true), ''), NULLIF(current_setting('app.player_account_id', true), '')`,
	).Scan(&scopedTenant, &scopedPlayer)
	if err != nil {
		return fmt.Errorf("sportsbook: read connection scope for exposure evaluation: %w", err)
	}
	if scopedPlayer != nil {
		return fmt.Errorf("%w: app.player_account_id is set", ErrExposurePlayerScopedConnection)
	}
	if scopedTenant == nil {
		return fmt.Errorf("%w: app.tenant_id is not set on this transaction", ErrExposureTenantScopeMismatch)
	}
	parsed, err := uuid.Parse(*scopedTenant)
	if err != nil {
		return fmt.Errorf("%w: app.tenant_id %q is not a uuid", ErrExposureTenantScopeMismatch, *scopedTenant)
	}
	if parsed != tenantID {
		return fmt.Errorf("%w: transaction is scoped to %s, request is for %s", ErrExposureTenantScopeMismatch, parsed, tenantID)
	}
	return nil
}

// exposureLimitRow is one winning row from loadActiveExposureLimits - the
// already brand-precedence-resolved limit for one scope_kind. BrandID is
// the WINNING row's own brand_id (nil for a tenant-wide row) - this is
// the "B" in ADR 0083 §6.2.2's Exposure(T,B,A,S) definition, which the
// aggregate query below uses UNCHANGED from the limit's own scope, not
// from the bet being placed: a brand-specific limit aggregates only that
// brand's open bets, while a tenant-wide limit (brand_id IS NULL)
// aggregates every brand's open bets in the tenant.
type exposureLimitRow struct {
	ID                     uuid.UUID
	ScopeKind              string
	BrandID                *uuid.UUID
	MaxOpenPotentialPayout pgtype.Numeric
}

// loadActiveExposureLimits returns at most one winning row PER scope_kind
// for (tenantID, assetCode) - brand-specific winning over tenant-wide
// (ADR 0083 §6.2.4 step 1), mirroring casino_game_availability's own
// "ORDER BY (brand_id IS NULL) ASC" most-specific-wins precedent
// (internal/casino/catalogue.go's IsGameAvailable). The two partial unique
// indexes on sb_exposure_limits (migration 0088) guarantee at most one
// active row exists at each precedence tier for a given (tenant, brand,
// scope_kind, asset) or (tenant, scope_kind, asset), so DISTINCT ON
// (scope_kind) ordered the same way always picks the single correct
// winner, never an arbitrary one.
func loadActiveExposureLimits(ctx context.Context, tx pgx.Tx, tenantID, brandID uuid.UUID, assetCode string) ([]exposureLimitRow, error) {
	rows, err := tx.Query(ctx,
		`SELECT DISTINCT ON (scope_kind) scope_kind, id, brand_id, max_open_potential_payout
		 FROM sb_exposure_limits
		 WHERE tenant_id = $1 AND asset_code = $2 AND status = 'active' AND (brand_id = $3 OR brand_id IS NULL)
		 ORDER BY scope_kind, (brand_id IS NULL) ASC`,
		tenantID, assetCode, brandID,
	)
	if err != nil {
		return nil, fmt.Errorf("sportsbook: load active exposure limits: %w", err)
	}
	defer rows.Close()
	var out []exposureLimitRow
	for rows.Next() {
		var r exposureLimitRow
		if err := rows.Scan(&r.ScopeKind, &r.ID, &r.BrandID, &r.MaxOpenPotentialPayout); err != nil {
			return nil, fmt.Errorf("sportsbook: scan active exposure limit: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// numericToBigInt converts a scanned NUMERIC(38,0) column to *big.Int,
// never through int64 or float64 - mirrors internal/risk's identical
// helper of the same name byte-for-byte (ADR 0083 §6.2.3's own comment:
// "reusing risk's numericToBigInt discipline"). Duplicated, not imported,
// for the same package-boundary reason as
// verifyTenantScopedExposureConnection above: a SUM of many
// sportsbook_bets.potential_return rows for an 18-exponent asset can
// legitimately exceed int64's range, and a silent overflow here would fail
// OPEN exactly where an exposure ceiling most needs to fail closed. A
// positive Exp is normal (PostgreSQL may return an exact integer total
// normalized to a non-zero scale) and is handled by scaling UP, never
// truncating; a NEGATIVE Exp is refused outright - a fractional count of
// minor units cannot exist in this schema.
func numericToBigInt(n pgtype.Numeric) (*big.Int, error) {
	if !n.Valid || n.Int == nil {
		return big.NewInt(0), nil
	}
	if n.Exp < 0 {
		return nil, fmt.Errorf("sportsbook: refusing a fractional NUMERIC minor-unit value (exponent %d)", n.Exp)
	}
	if n.Exp == 0 {
		return n.Int, nil
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n.Exp)), nil)
	return new(big.Int).Mul(n.Int, scale), nil
}

// aggregateOpenExposure computes ADR 0083 §6.2.2's exact measure -
// Exposure(T, limitBrandID, assetCode, scope) - for exactly one of the
// three scope_kind shapes. selection_id is the aggregate's own indexed
// predicate (idx_sportsbook_bets_open_exposure, migration 0088); market
// and event resolve through sb_selections/sb_markets, exactly as ADR 0083
// §6.2.3's closing paragraph specifies ("the market/event-level lookups
// as a tenant+asset scan hash-joined to the small selection set produced
// by idx_sb_selections_market").
//
// Gross potential payout, not net (§6.2.2: potential_return already
// includes the returned stake - the conservative, larger figure, and a
// plain SUM of a stored column with no arithmetic). status = 'open' only:
// today the only status ever written (no settlement path exists), so this
// predicate is future-proofing, not filtering (test item 29).
func aggregateOpenExposure(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, limitBrandID *uuid.UUID, assetCode, scopeKind string, scopeID uuid.UUID) (*big.Int, error) {
	var query string
	switch scopeKind {
	case "selection":
		query = `SELECT COALESCE(SUM(potential_return), 0) FROM sportsbook_bets
			WHERE tenant_id = $1 AND (brand_id = $2 OR $2 IS NULL) AND asset_code = $3 AND status = 'open' AND selection_id = $4`
	case "market":
		query = `SELECT COALESCE(SUM(b.potential_return), 0) FROM sportsbook_bets b
			JOIN sb_selections sel ON sel.id = b.selection_id
			WHERE b.tenant_id = $1 AND (b.brand_id = $2 OR $2 IS NULL) AND b.asset_code = $3 AND b.status = 'open' AND sel.market_id = $4`
	case "event":
		query = `SELECT COALESCE(SUM(b.potential_return), 0) FROM sportsbook_bets b
			JOIN sb_selections sel ON sel.id = b.selection_id
			JOIN sb_markets mk ON mk.id = sel.market_id
			WHERE b.tenant_id = $1 AND (b.brand_id = $2 OR $2 IS NULL) AND b.asset_code = $3 AND b.status = 'open' AND mk.event_id = $4`
	default:
		return nil, fmt.Errorf("sportsbook: unrecognized exposure scope_kind %q", scopeKind)
	}
	var total pgtype.Numeric
	if err := tx.QueryRow(ctx, query, tenantID, limitBrandID, assetCode, scopeID).Scan(&total); err != nil {
		return nil, fmt.Errorf("sportsbook: aggregate open exposure (scope_kind=%s): %w", scopeKind, err)
	}
	return numericToBigInt(total)
}

// exposureScopeEvaluationOrder is BROADEST FIRST (event -> market ->
// selection), so the reported breach names the broadest true statement
// (ADR 0083 §6.2.4 step 3's own requirement).
var exposureScopeEvaluationOrder = [...]string{"event", "market", "selection"}

func (s catalogueScope) idForScopeKind(kind string) uuid.UUID {
	switch kind {
	case "event":
		return s.EventID
	case "market":
		return s.MarketID
	case "selection":
		return s.SelectionID
	default:
		return uuid.Nil
	}
}

// evaluateExposureLimits is a PRE-POSTING DECISION GATE (ADR 0083 §6.2.4).
// It writes nothing, posts nothing, takes no wallet_balance_projection
// lock, and never reads a balance. Any non-nil error is fail-closed: the
// caller (PlaceBet) propagates it and the whole transaction aborts.
//
// tx MUST be tenant-scoped and MUST NOT be player-scoped - the aggregate
// spans every player in the tenant, which sportsbook_bets' own
// tenant_staff_scope policy permits and its player policy does not. This
// is ASSERTED (verifyTenantScopedExposureConnection), exactly as
// risk.Evaluate's verifyConnectionScope asserts the same precondition,
// rather than assumed.
//
// Algorithm, in order (ADR 0083 §6.2.4):
//  1. Load active limits for (tenant_id, asset_code), brand-specific
//     winning over tenant-wide, per scope_kind. NONE configured => return
//     {Breached: false} immediately, taking NO LOCK - this is the
//     "zero rows configured, zero cost" contract (§6.2.3's closing
//     paragraph) that keeps the gate a no-op on every existing test and on
//     the shipped, unarmed default (HDR-SB-1, §8.2).
//  2. Acquire the class L0.6 advisory lock (ADR 0082 Amendment A2), keyed
//     on the EVENT id - one lock, whatever mix of scope kinds is
//     configured (§6.2.5: any two bets whose aggregates can interact at
//     market or selection level necessarily share the same event, so one
//     event-keyed lock closes every race at all three levels without
//     inventing a within-class multi-lock ordering rule).
//  3. For each configured scope_kind, compute the aggregate (§6.2.2), add
//     IncrementalPotentialReturn, compare against
//     max_open_potential_payout as *big.Int. First breach wins; evaluated
//     broadest first (event -> market -> selection).
//  4. Return.
func evaluateExposureLimits(ctx context.Context, tx pgx.Tx, p exposureParams) (ExposureDecision, error) {
	if p.TenantID == uuid.Nil || p.BrandID == uuid.Nil || p.Scope.EventID == uuid.Nil || p.Scope.MarketID == uuid.Nil ||
		p.Scope.SelectionID == uuid.Nil || p.AssetCode == "" {
		return ExposureDecision{}, fmt.Errorf("%w: exposure evaluation requires a fully-populated tenant/brand/catalogue-scope/asset", ErrInvalidInput)
	}
	if err := verifyTenantScopedExposureConnection(ctx, tx, p.TenantID); err != nil {
		return ExposureDecision{}, err
	}

	// Step 1.
	limits, err := loadActiveExposureLimits(ctx, tx, p.TenantID, p.BrandID, p.AssetCode)
	if err != nil {
		return ExposureDecision{}, err
	}
	if len(limits) == 0 {
		return ExposureDecision{Breached: false}, nil
	}
	byScopeKind := make(map[string]exposureLimitRow, len(limits))
	for _, l := range limits {
		byScopeKind[l.ScopeKind] = l
	}

	// Step 2: class L0.6, keyed on the event id ONLY - never
	// market/selection - regardless of which scope_kind(s) are actually
	// configured (§6.2.5).
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended('sb_exposure:' || $1::text || ':' || $2::text, 0))`,
		p.TenantID, p.Scope.EventID,
	); err != nil {
		return ExposureDecision{}, fmt.Errorf("sportsbook: acquire exposure advisory lock: %w", err)
	}

	// Step 3: broadest first.
	for _, kind := range exposureScopeEvaluationOrder {
		limit, ok := byScopeKind[kind]
		if !ok {
			continue
		}
		scopeID := p.Scope.idForScopeKind(kind)
		existing, err := aggregateOpenExposure(ctx, tx, p.TenantID, limit.BrandID, p.AssetCode, kind, scopeID)
		if err != nil {
			return ExposureDecision{}, err
		}
		threshold, err := numericToBigInt(limit.MaxOpenPotentialPayout)
		if err != nil {
			return ExposureDecision{}, fmt.Errorf("%w: limit %s: %w", ErrExposureUnscannableLimit, limit.ID, err)
		}
		total := new(big.Int).Add(existing, big.NewInt(p.IncrementalPotentialReturn))
		if total.Cmp(threshold) > 0 {
			return ExposureDecision{Breached: true, ScopeKind: kind, LimitID: limit.ID}, nil
		}
	}

	// Step 4.
	return ExposureDecision{Breached: false}, nil
}
