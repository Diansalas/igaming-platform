package bonus

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// GrantStatus is bonus_grants.status's closed set (doc 10 §1.2, plus
// N1.4/N1.9's additive pending_settlement row). NewStakeEligibility
// (doc 10 N1.4) is a pure function of this field: open iff status IN
// {Issued, Activated, InProgress}, closed for every other value
// including PendingSettlement - this package stores the status, Phase 3
// computes NewStakeEligibility from it.
type GrantStatus string

const (
	GrantIssued            GrantStatus = "issued"
	GrantActivated         GrantStatus = "activated"
	GrantInProgress        GrantStatus = "in_progress"
	GrantPendingSettlement GrantStatus = "pending_settlement"
	GrantCompleted         GrantStatus = "completed"
	GrantConverted         GrantStatus = "converted"
	GrantExpired           GrantStatus = "expired"
	GrantCancelled         GrantStatus = "cancelled"
	GrantForfeited         GrantStatus = "forfeited"
	GrantReversed          GrantStatus = "reversed"
)

// TerminalResolution is pending_settlement's own carried field (doc 10
// N1.4): the terminal value a Grant's fate is already decided to reach,
// with the finalization deferred until AOE(G, .) closes to zero.
// Deliberately never 'converted' (Path A's own concern).
type TerminalResolution string

const (
	TerminalResolutionExpired   TerminalResolution = "expired"
	TerminalResolutionCancelled TerminalResolution = "cancelled"
	TerminalResolutionForfeited TerminalResolution = "forfeited"
)

// Grant mirrors one bonus_grants row (doc 10 §1.1/§1.2/T.2) - one
// player's instance of one Offer version, carrying enough immutable data
// to reconstruct itself without a live config read (T.2).
type Grant struct {
	ID       uuid.UUID
	TenantID uuid.UUID
	BrandID  uuid.UUID

	PlayerAccountID uuid.UUID
	WalletID        uuid.UUID

	CampaignID        uuid.UUID
	CampaignVersionID uuid.UUID
	OfferID           uuid.UUID
	OfferVersionID    uuid.UUID

	AssetCode       string
	DecimalExponent int32

	Status GrantStatus

	FundingSource          string // "operator" | "provider:<id>"
	FulfillmentDestination FulfillmentDestination
	FulfillmentOwner       string // "internal" | "external:<provider_id>"

	TriggerReference string

	EligibilitySnapshot []byte // JSONB, opaque to this package
	Segment             SegmentReference

	JurisdictionCode *string
	LicensingMode    *string

	// EconomicOperationIdentity (doc 34, doc 10 N2.4a).
	ParentOperationID *uuid.UUID

	// Bidirectional link with BonusSuggestion Activation (doc 10 §W6/§N3.2).
	OriginatingSuggestionID *uuid.UUID

	CreatedByActorType ActorType
	CreatedByActorID   uuid.UUID
	CreatedAt          time.Time

	ActivatedAt *time.Time
	CompletedAt *time.Time
	ConvertedAt *time.Time
	TerminalAt  *time.Time

	// pending_settlement's carried fields (doc 10 N1.4/N1.9).
	TerminalResolution           *TerminalResolution
	TerminalTriggerReasonCode    *string
	TerminalTriggeredAt          *time.Time
	TerminalTriggerCorrelationID *uuid.UUID

	ReversedAt         *time.Time
	ReversalReasonCode *string

	// ExpiresAt (migration 0070, Stage 4H-B1 Wave 3) is nullable
	// (an Offer that configures no wagering_time_limit legitimately never
	// expires) and write-once at the DB layer once set - see
	// SetGrantExpiryOnce.
	ExpiresAt *time.Time
}

const grantColumns = `
	id, tenant_id, brand_id, player_account_id, wallet_id,
	campaign_id, campaign_version_id, offer_id, offer_version_id,
	asset_code, decimal_exponent, status,
	funding_source, fulfillment_destination, fulfillment_owner, trigger_reference,
	eligibility_snapshot, segment_id, segment_version_id, jurisdiction_code, licensing_mode,
	parent_operation_id, originating_suggestion_id,
	created_by_actor_type, created_by_actor_id, created_at,
	activated_at, completed_at, converted_at, terminal_at,
	terminal_resolution, terminal_trigger_reason_code, terminal_triggered_at, terminal_trigger_correlation_id,
	reversed_at, reversal_reason_code, expires_at`

func scanGrant(row rowScanner) (Grant, error) {
	var (
		g                  Grant
		status             string
		fulfillmentDest    string
		createdByActorType string
		terminalResolution *string
	)
	err := row.Scan(
		&g.ID, &g.TenantID, &g.BrandID, &g.PlayerAccountID, &g.WalletID,
		&g.CampaignID, &g.CampaignVersionID, &g.OfferID, &g.OfferVersionID,
		&g.AssetCode, &g.DecimalExponent, &status,
		&g.FundingSource, &fulfillmentDest, &g.FulfillmentOwner, &g.TriggerReference,
		&g.EligibilitySnapshot, &g.Segment.SegmentID, &g.Segment.SegmentVersionID, &g.JurisdictionCode, &g.LicensingMode,
		&g.ParentOperationID, &g.OriginatingSuggestionID,
		&createdByActorType, &g.CreatedByActorID, &g.CreatedAt,
		&g.ActivatedAt, &g.CompletedAt, &g.ConvertedAt, &g.TerminalAt,
		&terminalResolution, &g.TerminalTriggerReasonCode, &g.TerminalTriggeredAt, &g.TerminalTriggerCorrelationID,
		&g.ReversedAt, &g.ReversalReasonCode, &g.ExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Grant{}, ErrNotFound
	}
	if err != nil {
		return Grant{}, fmt.Errorf("bonus: scan grant: %w", err)
	}
	g.Status = GrantStatus(status)
	g.FulfillmentDestination = FulfillmentDestination(fulfillmentDest)
	g.CreatedByActorType = ActorType(createdByActorType)
	if terminalResolution != nil {
		tr := TerminalResolution(*terminalResolution)
		g.TerminalResolution = &tr
	}
	return g, nil
}

// CreateGrant inserts a new bonus_grants row in status 'issued'. The
// caller is responsible for every eligibility/idempotency/EOI check
// doc 10 T.1-T.13/N2.4a require before calling this - this function
// performs a plain insert and relies entirely on the schema's own
// constraints (the (tenant_id, campaign_id, offer_version_id,
// player_account_id, trigger_reference) unique index, every FK) for
// its own safety net.
func CreateGrant(ctx context.Context, tx pgx.Tx, g Grant) (Grant, error) {
	if g.ID == uuid.Nil {
		g.ID = uuid.New()
	}
	if g.Status == "" {
		g.Status = GrantIssued
	}
	row := tx.QueryRow(ctx, `
		INSERT INTO bonus_grants (
			id, tenant_id, brand_id, player_account_id, wallet_id,
			campaign_id, campaign_version_id, offer_id, offer_version_id,
			asset_code, decimal_exponent, status,
			funding_source, fulfillment_destination, fulfillment_owner, trigger_reference,
			eligibility_snapshot, segment_id, segment_version_id, jurisdiction_code, licensing_mode,
			parent_operation_id, originating_suggestion_id,
			created_by_actor_type, created_by_actor_id
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25)
		RETURNING `+grantColumns,
		g.ID, g.TenantID, g.BrandID, g.PlayerAccountID, g.WalletID,
		g.CampaignID, g.CampaignVersionID, g.OfferID, g.OfferVersionID,
		g.AssetCode, g.DecimalExponent, string(g.Status),
		g.FundingSource, string(g.FulfillmentDestination), g.FulfillmentOwner, g.TriggerReference,
		nonNilJSON(g.EligibilitySnapshot), g.Segment.SegmentID, g.Segment.SegmentVersionID, g.JurisdictionCode, g.LicensingMode,
		g.ParentOperationID, g.OriginatingSuggestionID,
		string(g.CreatedByActorType), g.CreatedByActorID,
	)
	return scanGrant(row)
}

// GetGrantByID looks up a Grant by id within the caller's current RLS
// scope (tenant staff scope for back-office/system callers; player
// self-scope, read-only, for a player reading their own Grant).
func GetGrantByID(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Grant, error) {
	row := tx.QueryRow(ctx, `SELECT `+grantColumns+` FROM bonus_grants WHERE id = $1`, id)
	return scanGrant(row)
}

// LockGrantForUpdate reads a Grant AND takes a row lock (SELECT ... FOR
// UPDATE), mirroring internal/withdrawal.lockRequestForUpdate's own
// documented rationale: every transition that performs more than one
// statement should take this lock first so two concurrent transitions
// against the SAME Grant serialize on Postgres's own row lock. This
// package does not itself acquire the (tenant_id, grant_id) advisory
// lock doc 10 §9 additionally specifies - that composition is Phase 3's
// job.
func LockGrantForUpdate(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Grant, error) {
	row := tx.QueryRow(ctx, `SELECT `+grantColumns+` FROM bonus_grants WHERE id = $1 FOR UPDATE`, id)
	return scanGrant(row)
}

// ListGrantsByPlayer returns every Grant for a player, most recent first.
func ListGrantsByPlayer(ctx context.Context, tx pgx.Tx, tenantID, playerAccountID uuid.UUID) ([]Grant, error) {
	return queryGrants(ctx, tx, `SELECT `+grantColumns+` FROM bonus_grants WHERE tenant_id = $1 AND player_account_id = $2 ORDER BY created_at DESC`, tenantID, playerAccountID)
}

// ListGrantsByCampaign returns every Grant issued under a Campaign, most
// recent first.
func ListGrantsByCampaign(ctx context.Context, tx pgx.Tx, tenantID, campaignID uuid.UUID) ([]Grant, error) {
	return queryGrants(ctx, tx, `SELECT `+grantColumns+` FROM bonus_grants WHERE tenant_id = $1 AND campaign_id = $2 ORDER BY created_at DESC`, tenantID, campaignID)
}

// ListGrantsByStatus returns every Grant in a given status, oldest first
// (the natural processing order for a sweep/reconciliation job).
func ListGrantsByStatus(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, status GrantStatus) ([]Grant, error) {
	return queryGrants(ctx, tx, `SELECT `+grantColumns+` FROM bonus_grants WHERE tenant_id = $1 AND status = $2 ORDER BY created_at ASC`, tenantID, string(status))
}

// ListExpirableGrants returns every open (issued/activated/in_progress)
// Grant whose expires_at has already passed asOf, oldest expiry first -
// the expiry sweep job's own read pattern (migration 0070's partial
// index on (tenant_id, expires_at) WHERE expires_at IS NOT NULL exists
// for exactly this query). asOf is supplied by the caller (the sweep
// job's own clock_timestamp() read, per doc 10 §2's "clock_timestamp(),
// never now()" rule already binding on every other window-elapsed
// comparison in this package) rather than read here, so this function
// stays a pure, deterministic query.
func ListExpirableGrants(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, asOf time.Time) ([]Grant, error) {
	return queryGrants(ctx, tx,
		`SELECT `+grantColumns+` FROM bonus_grants
		  WHERE tenant_id = $1 AND status IN ('issued','activated','in_progress')
		    AND expires_at IS NOT NULL AND expires_at <= $2
		  ORDER BY expires_at ASC`,
		tenantID, asOf,
	)
}

// HasActiveWageringGrant is ledger-accounting-model.md §7.18.3.3's own
// named seam: a cheap, indexed existence check (idx_bonus_grants_player,
// migration 0057 - no new index/migration needed) casino's postBet calls
// on EVERY cash bet, BEFORE any wagering-contribution weighting/
// attribution work, so the added cost to the overwhelming majority of
// bets (no active bonus at all) is exactly one indexed EXISTS.
func HasActiveWageringGrant(ctx context.Context, tx pgx.Tx, tenantID, playerAccountID uuid.UUID) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM bonus_grants
			 WHERE tenant_id = $1 AND player_account_id = $2 AND status IN ('activated','in_progress')
		)`,
		tenantID, playerAccountID,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("bonus: has active wagering grant: %w", err)
	}
	return exists, nil
}

// listActiveWageringGrants returns every Grant currently open for new
// stakes (status activated/in_progress) for playerAccountID IN assetCode -
// the raw input to this Wave's own fail-closed multi-Grant attribution
// posture (ledger-accounting-model.md §7.18.3.3: "until bonus-engine
// specifies a qualifying-wager attribution rule across concurrent Grants,
// at most one Grant should be in a state that receives cash-funded
// wagering contribution at a time").
//
// DR-4HB1W3-RISK-01 (Stage 4H-B1 Wave 3 Phase 4, risk): the assetCode
// predicate is load-bearing, not a convenience filter. A Grant's wagering
// target is derived from its own granted_amount, in ITS OWN asset's minor
// units (wagering_contribution_entry.go WageringTargetScaled), while a
// contribution's QualifyingScaled is in the BET's asset's minor units.
// Without this predicate a cash bet in one asset would be summed into a
// Grant denominated in another and compared against that Grant's target
// as if the two were the same measure - the exact cross-denomination
// comparison ADR 0031 §34/§35 and internal/risk/denomination.go forbid at
// every OTHER value-authorizing boundary on this platform. This platform
// is multi-wallet/multi-asset per player by design (ADR 0007) with
// per-asset decimal exponents of 2, 8 or 18, so the mismatch is not
// hypothetical: one small crypto-denominated bet (8-decimal minor units)
// would otherwise satisfy a fiat-denominated (2-decimal) Grant's entire
// wagering requirement outright.
func listActiveWageringGrants(ctx context.Context, tx pgx.Tx, tenantID, playerAccountID uuid.UUID, assetCode string) ([]Grant, error) {
	return queryGrants(ctx, tx,
		`SELECT `+grantColumns+` FROM bonus_grants
		  WHERE tenant_id = $1 AND player_account_id = $2 AND asset_code = $3
		    AND status IN ('activated','in_progress')
		  ORDER BY created_at ASC`,
		tenantID, playerAccountID, assetCode,
	)
}

// ListGrantsPage returns a page of Grants for tenantID, optionally
// scoped to one player (playerAccountID) and/or filtered by status, most
// recently created first, plus the total count matching the filters -
// Stage 5 (Operator Back Office MVP)'s own read surface: the tenant-wide
// grant list, and (when playerAccountID is supplied) the per-player
// bonus/reward-state view for a Back Office player-detail page. Additive
// to ListGrantsByPlayer/ListGrantsByStatus (both unchanged).
func ListGrantsPage(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, playerAccountID *uuid.UUID, status *GrantStatus, limit, offset int) ([]Grant, int, error) {
	where := "tenant_id = $1"
	args := []any{tenantID}
	if playerAccountID != nil {
		args = append(args, *playerAccountID)
		where += fmt.Sprintf(" AND player_account_id = $%d", len(args))
	}
	if status != nil {
		args = append(args, string(*status))
		where += fmt.Sprintf(" AND status = $%d", len(args))
	}

	var total int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM bonus_grants WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("bonus: count grants: %w", err)
	}

	pageArgs := append(append([]any{}, args...), limit, offset)
	sql := `SELECT ` + grantColumns + ` FROM bonus_grants WHERE ` + where +
		fmt.Sprintf(` ORDER BY created_at DESC LIMIT $%d OFFSET $%d`, len(pageArgs)-1, len(pageArgs))
	grants, err := queryGrants(ctx, tx, sql, pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	return grants, total, nil
}

func queryGrants(ctx context.Context, tx pgx.Tx, sql string, args ...any) ([]Grant, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("bonus: query grants: %w", err)
	}
	defer rows.Close()
	var out []Grant
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ErrGrantStateConflict is returned by UpdateGrantStatus when the
// caller's expected current status does not match the row's actual
// status at update time - either a stale read or a concurrent
// transition won the race. Mirrors internal/withdrawal.ErrStateConflict's
// exact shape and rationale.
var ErrGrantStateConflict = errors.New("bonus: grant is not in the expected status")

// UpdateGrantStatus performs the one allowed-transition primitive this
// package provides: a compare-and-swap status update
// (UPDATE ... WHERE status = expectedStatus), never an unconditional
// write - this is a real DB-enforced optimistic-concurrency guard, not
// merely a convention, mirroring bonus_held_dispositions' own
// `WHERE status = 'held'` pattern (ledger-accounting-model.md §7.7.2.7).
// It enforces NO transition-legality rule (which (from, to) pairs are
// valid per doc 10 §1.3's table) - that is Phase 3's state-machine layer,
// which is expected to call this only with a pair it has already
// validated. Every other Grant field this function does not take a
// parameter for is left untouched.
func UpdateGrantStatus(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, expectedStatus, newStatus GrantStatus, at time.Time) (Grant, error) {
	var timestampColumn string
	switch newStatus {
	case GrantActivated:
		timestampColumn = "activated_at"
	case GrantCompleted:
		timestampColumn = "completed_at"
	case GrantConverted:
		timestampColumn = "converted_at"
	case GrantExpired, GrantCancelled, GrantForfeited:
		timestampColumn = "terminal_at"
	default:
		timestampColumn = ""
	}

	sql := `UPDATE bonus_grants SET status = $4`
	args := []any{tenantID, id, string(expectedStatus), string(newStatus)}
	if timestampColumn != "" {
		sql += fmt.Sprintf(`, %s = $5`, timestampColumn)
		args = append(args, at)
	}
	sql += ` WHERE tenant_id = $1 AND id = $2 AND status = $3 RETURNING ` + grantColumns

	row := tx.QueryRow(ctx, sql, args...)
	g, err := scanGrant(row)
	if errors.Is(err, ErrNotFound) {
		return Grant{}, ErrGrantStateConflict
	}
	return g, err
}

// SetGrantPendingSettlement transitions a Grant into pending_settlement
// (doc 10 N1.4), recording the Path B fields the eventual finalization
// needs: the decided terminal_resolution, the original trigger's reason
// code/timestamp/correlation id. Compare-and-swap on expectedStatus,
// mirroring UpdateGrantStatus's own guard.
func SetGrantPendingSettlement(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, expectedStatus GrantStatus, resolution TerminalResolution, reasonCode string, triggeredAt time.Time, correlationID uuid.UUID) (Grant, error) {
	row := tx.QueryRow(ctx, `
		UPDATE bonus_grants
		SET status = $4, terminal_resolution = $5, terminal_trigger_reason_code = $6, terminal_triggered_at = $7, terminal_trigger_correlation_id = $8
		WHERE tenant_id = $1 AND id = $2 AND status = $3
		RETURNING `+grantColumns,
		tenantID, id, string(expectedStatus), string(GrantPendingSettlement), string(resolution), reasonCode, triggeredAt, correlationID,
	)
	g, err := scanGrant(row)
	if errors.Is(err, ErrNotFound) {
		return Grant{}, ErrGrantStateConflict
	}
	return g, err
}

// FinalizePendingSettlement flips a pending_settlement Grant to its
// already-decided terminal_resolution value once AOE(G, .) has closed to
// zero (doc 10 N1.4 step 3). Compare-and-swap on the row currently being
// GrantPendingSettlement.
func FinalizePendingSettlement(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, at time.Time) (Grant, error) {
	row := tx.QueryRow(ctx, `
		UPDATE bonus_grants
		SET status = terminal_resolution, terminal_at = $3
		WHERE tenant_id = $1 AND id = $2 AND status = $4 AND terminal_resolution IS NOT NULL
		RETURNING `+grantColumns,
		tenantID, id, at, string(GrantPendingSettlement),
	)
	g, err := scanGrant(row)
	if errors.Is(err, ErrNotFound) {
		return Grant{}, ErrGrantStateConflict
	}
	return g, err
}

// SetGrantExpiryOnce writes bonus_grants.expires_at exactly once
// (migration 0070's own trigger additionally enforces this at the DB
// layer - "expires_at is immutable once set" - this WHERE clause is the
// same belt-and-suspenders no-op-on-replay shape ActivateGrant's own
// granted_amount write uses, not the sole enforcement mechanism). A
// caller that computes expiresAt from g.CreatedAt plus a configured
// duration (rather than time.Now()) avoids any risk of the DB's own
// `expires_at > created_at` CHECK failing under Go/Postgres clock skew -
// see lifecycle.go ActivateGrant's own caller of this function. Returns
// (false, nil) - not an error - if expires_at was already set (either by
// a concurrent/earlier call, or because this Grant's Offer configures no
// expiry and the caller never intended to call this at all is the
// caller's own no-op branch, not this function's).
func SetGrantExpiryOnce(ctx context.Context, tx pgx.Tx, tenantID, grantID uuid.UUID, expiresAt time.Time) (bool, error) {
	tag, err := tx.Exec(ctx,
		`UPDATE bonus_grants SET expires_at = $3 WHERE tenant_id = $1 AND id = $2 AND expires_at IS NULL`,
		tenantID, grantID, expiresAt,
	)
	if err != nil {
		return false, fmt.Errorf("bonus: set grant expiry: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// MarkGrantReversed applies the 'reversed' transition (doc 10 §1.3's
// last row) - "an additional Progress entry and a new Grant status",
// applicable from any prior terminal state. No expectedStatus guard
// beyond "not already reversed" (the CHECK constraint on reversed_at
// pairs with status='reversed' anyway); Phase 3 is responsible for
// verifying the prior state was genuinely terminal before calling this.
func MarkGrantReversed(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, reasonCode string, at time.Time) (Grant, error) {
	row := tx.QueryRow(ctx, `
		UPDATE bonus_grants
		SET status = $3, reversed_at = $4, reversal_reason_code = $5
		WHERE tenant_id = $1 AND id = $2 AND status <> $3
		RETURNING `+grantColumns,
		tenantID, id, string(GrantReversed), at, reasonCode,
	)
	g, err := scanGrant(row)
	if errors.Is(err, ErrNotFound) {
		return Grant{}, ErrGrantStateConflict
	}
	return g, err
}
