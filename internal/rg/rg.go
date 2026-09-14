// Package rg implements the Stage 4D-RG Responsible Gaming / player-
// status enforcement foundation: the player_restrictions record (self-
// exclusion, first-class and cross-brand via the existing Person
// identity) and the single authoritative EvaluateEligibility policy
// boundary every gambling-action entry point (today: casino game launch
// and casino bet) must consult before proceeding.
//
// This package does NOT introduce a second identity/status model. It
// composes the existing ones - internal/identity's Person/PlayerAccount
// and internal/wallet's Wallet - with exactly one new signal
// (player_restrictions) into one Decision. See
// docs/decisions/0026-responsible-gaming-player-status-enforcement-
// foundation.md for the full design and its residual/deferred scope
// (KYC/AML restriction, jurisdiction-at-player-level, and every RG
// control besides self-exclusion - deposit/loss/wagering/session limits,
// reality checks, time-outs/cooling-off - remain documented extension
// points, not implemented here).
package rg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/wallet"
)

// RestrictionType mirrors player_restrictions.restriction_type. Only
// self-exclusion is implemented this stage - see the migration's own
// comment for why the CHECK constraint (and this enum) deliberately
// admits no other value yet.
type RestrictionType string

const RestrictionSelfExclusion RestrictionType = "self_exclusion"

// Source mirrors player_restrictions.source.
type Source string

const (
	SourcePlayerSelfService Source = "player_self_service"
	SourceStaff             Source = "staff"
)

// Scope is the administrative reach of a staff-created restriction -
// derived from, and always consistent with, which of TenantID/BrandID a
// Restriction carries (never stored as a separate column - see the
// migration's own rationale for keeping nullability as the single source
// of truth). Player self-service restrictions are always ScopePlatform.
type Scope string

const (
	ScopePlatform Scope = "platform"
	ScopeTenant   Scope = "tenant"
	ScopeBrand    Scope = "brand"
)

// Restriction mirrors a player_restrictions row.
type Restriction struct {
	ID                 uuid.UUID
	PersonID           uuid.UUID
	TenantID           *uuid.UUID
	BrandID            *uuid.UUID
	PlayerAccountID    *uuid.UUID
	RestrictionType    RestrictionType
	StartsAt           time.Time
	EndsAt             *time.Time
	Source             Source
	ReasonCode         string
	CreatedByActorType audit.ActorType
	CreatedByActorID   uuid.UUID
	CreatedAt          time.Time
}

// Scope reports r's administrative scope from its nullable columns -
// never a separately stored, independently-mutable field.
func (r Restriction) Scope() Scope {
	switch {
	case r.TenantID == nil:
		return ScopePlatform
	case r.BrandID == nil:
		return ScopeTenant
	default:
		return ScopeBrand
	}
}

// IsActiveAt reports whether r is in force at instant t - the same
// "starts_at <= t < ends_at-or-indefinite" test EvaluateEligibility runs
// in SQL, exposed here too for the read-side status endpoint so it never
// has to duplicate the logic.
func (r Restriction) IsActiveAt(t time.Time) bool {
	if t.Before(r.StartsAt) {
		return false
	}
	return r.EndsAt == nil || t.Before(*r.EndsAt)
}

// ErrInvalidInput is returned for a structurally invalid call caught
// before ever reaching the database.
var ErrInvalidInput = errors.New("rg: invalid input")

// lockPerson takes a transaction-scoped Postgres advisory lock keyed on
// personID, released automatically at COMMIT or ROLLBACK. Every writer of
// a player_restrictions row, and EvaluateEligibility itself, take this
// SAME lock before reading or writing anything else for that person -
// this is what actually closes the TOCTOU race the Stage 4D-RG directive
// (§8) requires be resolved deterministically: a restriction being
// created for a person concurrently with a launch/bet eligibility check
// for that SAME person can never interleave (whichever transaction
// acquires the lock first runs to completion - commit or rollback -
// before the other's own lock acquisition returns), rather than both
// observing a stale "not yet restricted" snapshot. There is no natural
// row to SELECT ... FOR UPDATE here (the restriction may not exist yet at
// all, and the "no restriction" case is exactly the one that needs
// locking) - an advisory lock keyed on the person id is the standard
// Postgres idiom for serializing on a fact rather than a row, applied
// here the same way internal/casino.lockCashBalance's row lock serializes
// concurrent bets against one wallet.
//
// LOCK ORDERING RULE (financial correctness specialist review): every
// caller in this codebase takes this lock BEFORE any wallet-balance lock
// (internal/casino.lockCashBalance's FOR UPDATE) within the same
// transaction, never after. A future money path that calls
// EvaluateEligibility (which calls this) while already holding a balance
// lock would create a lock-ordering cycle with any other transaction that
// takes the two in the opposite order - always resolve eligibility FIRST,
// exactly as internal/casino.postBet already does.
func lockPerson(ctx context.Context, tx pgx.Tx, personID uuid.UUID) error {
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext('player_restrictions'), hashtext($1::text))`,
		personID.String(),
	); err != nil {
		return fmt.Errorf("rg: lock person: %w", err)
	}
	return nil
}

func insertRestriction(ctx context.Context, tx pgx.Tx, r Restriction) (Restriction, error) {
	r.ID = uuid.New()
	if r.StartsAt.IsZero() {
		r.StartsAt = time.Now().UTC()
	}
	r.CreatedAt = time.Now().UTC()
	_, err := tx.Exec(ctx,
		`INSERT INTO player_restrictions
			(id, person_id, tenant_id, brand_id, player_account_id, restriction_type,
			 starts_at, ends_at, source, reason_code, created_by_actor_type, created_by_actor_id, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NULLIF($10, ''), $11, $12, $13)`,
		r.ID, r.PersonID, r.TenantID, r.BrandID, r.PlayerAccountID, r.RestrictionType,
		r.StartsAt, r.EndsAt, r.Source, r.ReasonCode, string(r.CreatedByActorType), r.CreatedByActorID, r.CreatedAt,
	)
	if err != nil {
		return Restriction{}, fmt.Errorf("rg: insert restriction: %w", err)
	}
	return r, nil
}

// CreateSelfExclusionParams is CreateSelfExclusion's input.
// TenantID/PlayerAccountID MUST be resolved server-side from the
// authenticated player session, never a request body - mirroring every
// other player-self-service write in this codebase. DurationDays == nil
// means indefinite (ADR 0026 §4).
type CreateSelfExclusionParams struct {
	TenantID        uuid.UUID
	PlayerAccountID uuid.UUID
	DurationDays    *int
}

// CreateSelfExclusion records a player's own, platform-wide self-
// exclusion. tx MUST come from db.WithPlayerScope(ctx, tenantID,
// playerAccountID, ...) - the player_self_insert RLS policy (migration
// 0037) requires the app.player_account_id GUC that only that call sets,
// and independently re-derives person_id/player_account_id from it rather
// than trusting anything on params beyond identity already established by
// the transaction scope itself.
func CreateSelfExclusion(ctx context.Context, tx pgx.Tx, params CreateSelfExclusionParams) (Restriction, error) {
	if params.TenantID == uuid.Nil || params.PlayerAccountID == uuid.Nil {
		return Restriction{}, fmt.Errorf("%w: tenant_id and player_account_id are required", ErrInvalidInput)
	}
	if params.DurationDays != nil && *params.DurationDays <= 0 {
		return Restriction{}, fmt.Errorf("%w: duration_days must be positive when set", ErrInvalidInput)
	}

	account, err := identity.GetPlayerAccountByID(ctx, tx, params.PlayerAccountID)
	if err != nil {
		return Restriction{}, fmt.Errorf("rg: resolve player account: %w", err)
	}

	if err := lockPerson(ctx, tx, account.PersonID); err != nil {
		return Restriction{}, err
	}

	now := time.Now().UTC()
	var endsAt *time.Time
	if params.DurationDays != nil {
		t := now.AddDate(0, 0, *params.DurationDays)
		endsAt = &t
	}

	restriction, err := insertRestriction(ctx, tx, Restriction{
		PersonID: account.PersonID, PlayerAccountID: &params.PlayerAccountID,
		RestrictionType: RestrictionSelfExclusion, StartsAt: now, EndsAt: endsAt,
		Source: SourcePlayerSelfService, CreatedByActorType: audit.ActorPlayer, CreatedByActorID: params.PlayerAccountID,
	})
	if err != nil {
		return Restriction{}, err
	}

	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: params.TenantID, ActorType: audit.ActorPlayer, ActorID: params.PlayerAccountID,
		Action: "rg.self_exclusion.created", TargetType: "player_restriction", TargetID: restriction.ID.String(),
		Outcome: audit.OutcomeSuccess,
		Metadata: map[string]any{
			"person_id": account.PersonID.String(), "scope": string(ScopePlatform),
			"indefinite": endsAt == nil, "source": string(SourcePlayerSelfService),
		},
	}); err != nil {
		return Restriction{}, fmt.Errorf("rg: audit self-exclusion: %w", err)
	}

	return restriction, nil
}

// CreateStaffRestrictionParams is CreateStaffRestriction's input.
// TargetPlayerAccountID names the account the acting staff principal is
// looking at; Scope decides how far the resulting restriction reaches
// (ScopePlatform is only accepted when tx is genuinely platform-scoped -
// see CreateStaffRestriction's doc comment). ReasonCode is mandatory: an
// RG restriction is exactly the kind of security-sensitive administrative
// action CLAUDE.md requires a reason code for.
type CreateStaffRestrictionParams struct {
	ActorStaffID          uuid.UUID
	TargetPlayerAccountID uuid.UUID
	Scope                 Scope
	DurationDays          *int
	ReasonCode            string
}

// CreateStaffRestriction records a staff-initiated self-exclusion, always
// tenant- or brand-scoped (never ScopePlatform).
//
// tx MUST come from db.WithTenant(callerTenantID, ...) - the resulting
// restriction is pinned to THAT tenant (RoleCompliance's own, from the
// caller's own authenticated context), never a client/staff-asserted one;
// ScopeBrand additionally requires target.BrandID, so a tenant's
// compliance staff can only ever narrow a restriction to a brand their
// OWN tenant owns (enforced twice over: the brand/tenant composite FK,
// and the fact that target itself is only resolvable at all under the
// caller's own RLS-scoped read - player_accounts carries only a single
// tenant_isolation policy, no platform-wide read path, so there is today
// no tenant-scoped staff action that could even resolve a DIFFERENT
// tenant's account to begin with).
//
// ScopePlatform is deliberately NOT accepted here - see ADR 0026 §4/§12's
// own "OPEN DECISION" section: a genuinely platform-wide, staff-initiated
// restriction would need a platform-scoped principal able to resolve an
// arbitrary tenant's player_account first, which no role/permission in
// this codebase grants today (player_accounts has no platform-wide read
// policy - RoleTenantAdmin/RoleCompliance's own player-management
// permissions are themselves RequireTenantScope-gated). Building that
// lookup capability only to support this one caller would be exactly the
// kind of speculative, untested-by-any-real-path code CLAUDE.md's "no
// fake completion" rule warns against. The one path that DOES produce a
// platform-wide (cross-tenant) restriction this stage is player
// self-service (CreateSelfExclusion), which needs no such lookup - it
// always acts on the caller's own, already-authenticated identity.
func CreateStaffRestriction(ctx context.Context, tx pgx.Tx, params CreateStaffRestrictionParams) (Restriction, error) {
	if params.ActorStaffID == uuid.Nil || params.TargetPlayerAccountID == uuid.Nil {
		return Restriction{}, fmt.Errorf("%w: actor_staff_id and target_player_account_id are required", ErrInvalidInput)
	}
	if params.ReasonCode == "" {
		return Restriction{}, fmt.Errorf("%w: reason_code is required for a staff-initiated restriction", ErrInvalidInput)
	}
	if params.Scope != ScopeTenant && params.Scope != ScopeBrand {
		return Restriction{}, fmt.Errorf("%w: scope must be tenant or brand", ErrInvalidInput)
	}
	if params.DurationDays != nil && *params.DurationDays <= 0 {
		return Restriction{}, fmt.Errorf("%w: duration_days must be positive when set", ErrInvalidInput)
	}

	// Resolvable at all only because tx is tenant-scoped and this account
	// belongs to that same tenant - player_accounts' own RLS (migration
	// 0010) has no platform-wide or cross-tenant read path.
	account, err := identity.GetPlayerAccountByID(ctx, tx, params.TargetPlayerAccountID)
	if err != nil {
		return Restriction{}, fmt.Errorf("rg: resolve target player account: %w", err)
	}

	if err := lockPerson(ctx, tx, account.PersonID); err != nil {
		return Restriction{}, err
	}

	now := time.Now().UTC()
	var endsAt *time.Time
	if params.DurationDays != nil {
		t := now.AddDate(0, 0, *params.DurationDays)
		endsAt = &t
	}

	r := Restriction{
		PersonID: account.PersonID, PlayerAccountID: &params.TargetPlayerAccountID,
		RestrictionType: RestrictionSelfExclusion, StartsAt: now, EndsAt: endsAt,
		Source: SourceStaff, ReasonCode: params.ReasonCode,
		CreatedByActorType: audit.ActorStaff, CreatedByActorID: params.ActorStaffID,
	}
	switch params.Scope {
	case ScopeTenant:
		r.TenantID = &account.TenantID
	case ScopeBrand:
		r.TenantID = &account.TenantID
		r.BrandID = &account.BrandID
	}

	restriction, err := insertRestriction(ctx, tx, r)
	if err != nil {
		return Restriction{}, err
	}

	// audit.Record's own tenant scoping: a platform-wide restriction is a
	// platform-level audit event (TenantID uuid.Nil), matching audit_log's
	// own dual-scope convention exactly, since the acting connection here
	// genuinely has no tenant context for ScopePlatform.
	auditTenantID := uuid.Nil
	if r.TenantID != nil {
		auditTenantID = *r.TenantID
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: auditTenantID, ActorType: audit.ActorStaff, ActorID: params.ActorStaffID,
		Action: "rg.self_exclusion.created", TargetType: "player_restriction", TargetID: restriction.ID.String(),
		Outcome: audit.OutcomeSuccess,
		Metadata: map[string]any{
			"person_id": account.PersonID.String(), "target_player_account_id": params.TargetPlayerAccountID.String(),
			"scope": string(params.Scope), "indefinite": endsAt == nil, "reason_code": params.ReasonCode,
		},
	}); err != nil {
		return Restriction{}, fmt.Errorf("rg: audit staff restriction: %w", err)
	}

	return restriction, nil
}

func scanRestrictions(rows pgx.Rows) ([]Restriction, error) {
	defer rows.Close()
	var out []Restriction
	for rows.Next() {
		var r Restriction
		var createdByActorType string
		var reasonCode *string
		if err := rows.Scan(
			&r.ID, &r.PersonID, &r.TenantID, &r.BrandID, &r.PlayerAccountID, &r.RestrictionType,
			&r.StartsAt, &r.EndsAt, &r.Source, &reasonCode, &createdByActorType, &r.CreatedByActorID, &r.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("rg: scan restriction: %w", err)
		}
		if reasonCode != nil {
			r.ReasonCode = *reasonCode
		}
		r.CreatedByActorType = audit.ActorType(createdByActorType)
		out = append(out, r)
	}
	return out, rows.Err()
}

const restrictionColumns = `id, person_id, tenant_id, brand_id, player_account_id, restriction_type,
	starts_at, ends_at, source, reason_code, created_by_actor_type, created_by_actor_id, created_at`

// ListRestrictionsForAccount resolves accountID to its Person and returns
// every player_restrictions row visible under the current RLS scope for
// that person, newest first - used by both the player self-service status
// endpoint (called under db.WithPlayerScope, restricted by RLS to the
// caller's own person) and the staff read endpoint (called under
// db.WithTenant/WithoutTenant, restricted by RLS to the caller's own
// tenant plus every platform-wide row).
func ListRestrictionsForAccount(ctx context.Context, tx pgx.Tx, accountID uuid.UUID) ([]Restriction, error) {
	account, err := identity.GetPlayerAccountByID(ctx, tx, accountID)
	if err != nil {
		return nil, fmt.Errorf("rg: resolve player account: %w", err)
	}
	rows, err := tx.Query(ctx,
		`SELECT `+restrictionColumns+` FROM player_restrictions WHERE person_id = $1 ORDER BY created_at DESC`,
		account.PersonID,
	)
	if err != nil {
		return nil, fmt.Errorf("rg: list restrictions: %w", err)
	}
	return scanRestrictions(rows)
}

// Decision is EvaluateEligibility's deterministic, auditable output.
// Code is a stable, machine-readable reason - never a free-form message
// alone - so callers (and their own callers, e.g. an HTTP handler mapping
// it to a response) can distinguish denial reasons without parsing
// prose, mirroring the rest of this codebase's "specific, distinguishable
// sentinel" philosophy (internal/casino.ErrGameDisabled vs.
// ErrGameNotAvailable, restated here as one Decision rather than N
// sentinel errors since this is a single yes/no boundary with several
// possible reasons, not N independently-callable operations).
type Decision struct {
	Allowed  bool
	Code     string
	Message  string
	PersonID uuid.UUID
}

const (
	CodeAllowed                = "allowed"
	CodePlayerAccountNotActive = "player_account_not_active"
	CodeSelfExcluded           = "self_excluded"
	CodeWalletNotActive        = "wallet_not_active"
)

// EligibilityParams is EvaluateEligibility's input. TenantID/BrandID/
// PlayerAccountID MUST be resolved server-side, exactly like every other
// identity field this codebase ever hands to a financial/gambling
// boundary. WalletID is optional (uuid.Nil skips the wallet-status check)
// - a caller that has not yet resolved a wallet (e.g. before one exists)
// can still evaluate the account/restriction checks.
type EligibilityParams struct {
	TenantID        uuid.UUID
	BrandID         uuid.UUID
	PlayerAccountID uuid.UUID
	WalletID        uuid.UUID
}

// EvaluateEligibility is the single authoritative "may this player
// perform a gambling action right now" policy boundary (ADR 0026 §5) -
// the only place internal/casino's LaunchGame and postBet (and any future
// caller: a sportsbook bet, a future casino financial operation) consult
// this decision, so a new enforcement point never needs its own
// independent copy of these checks.
//
// It composes, in this exact order, the EXISTING status signals this
// stage does not redefine (player-account status, wallet status) with the
// ONE new signal this stage adds (an active player_restrictions row) -
// see docs/decisions/0026 §3 for why these are kept as distinct concepts
// rather than collapsed into one boolean, and §14 for the documented,
// not-yet-implemented KYC/AML extension point this function's own shape
// is designed to grow into without a rewrite (a future KYCAMLChecker
// parameter/call slotting in as one more Decision-producing step here,
// after the checks below, before returning Allowed).
//
// tx must already be tenant-scoped (db.WithTenant) for params.TenantID.
// The restriction query below deliberately does NOT rely solely on RLS to
// keep a different tenant's own tenant-scoped rows out - it filters
// `tenant_id IS NULL OR tenant_id = params.TenantID` explicitly, matching
// params.TenantID rather than the ambient GUC. This is defense in depth
// against a hypothetical future caller running this function under
// db.WithPlayerScope (which sets app.player_account_id, not app.tenant_id
// alone) - migration 0037's player_self_read RLS policy exposes a
// player's OWN restrictions across EVERY tenant, so without this explicit
// predicate a WithPlayerScope caller could over-deny (or, for a
// differently-shaped future query, over-allow) using a different tenant's
// tenant-scoped row. Every current caller already runs under db.WithTenant,
// so this predicate is currently redundant with RLS - it is added so the
// query's own correctness never depends on which scope helper a future
// caller happens to choose.
func EvaluateEligibility(ctx context.Context, tx pgx.Tx, params EligibilityParams) (Decision, error) {
	// BrandID is required, not merely recommended: the self-exclusion
	// query below treats brand_id IS NULL in a STORED restriction row as
	// "applies platform/tenant-wide", but if params.BrandID itself were
	// silently uuid.Nil, the (brand_id IS NULL OR brand_id = $3) clause
	// would still correctly find a platform-wide restriction - the real
	// risk is a brand-SCOPED restriction becoming un-checkable for a
	// caller that forgot to resolve its own brand id, a silent gap a
	// future caller (e.g. a sportsbook bet) could introduce without any
	// visible error. Security specialist review finding.
	if params.TenantID == uuid.Nil || params.PlayerAccountID == uuid.Nil || params.BrandID == uuid.Nil {
		return Decision{}, fmt.Errorf("%w: tenant_id, brand_id, and player_account_id are required", ErrInvalidInput)
	}

	account, err := identity.GetPlayerAccountByID(ctx, tx, params.PlayerAccountID)
	if err != nil {
		return Decision{}, fmt.Errorf("rg: resolve player account: %w", err)
	}

	// Locked BEFORE the restriction read below, for the full remainder of
	// the caller's transaction - see lockPerson's own doc comment for why
	// this is what actually closes the concurrent-self-exclusion race
	// (ADR 0026 §8/directive §8), not merely a best-effort snapshot read.
	if err := lockPerson(ctx, tx, account.PersonID); err != nil {
		return Decision{}, err
	}

	if account.Status != identity.PlayerStatusActive {
		return Decision{
			Allowed: false, Code: CodePlayerAccountNotActive, PersonID: account.PersonID,
			Message: fmt.Sprintf("player account status is %q, not active", account.Status),
		}, nil
	}

	var excluded bool
	err = tx.QueryRow(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM player_restrictions
			WHERE person_id = $1
			  AND restriction_type = $2
			  AND starts_at <= now()
			  AND (ends_at IS NULL OR ends_at > now())
			  AND (brand_id IS NULL OR brand_id = $3)
			  AND (tenant_id IS NULL OR tenant_id = $4)
		)`,
		account.PersonID, RestrictionSelfExclusion, params.BrandID, params.TenantID,
	).Scan(&excluded)
	if err != nil {
		return Decision{}, fmt.Errorf("rg: check self-exclusion: %w", err)
	}
	if excluded {
		return Decision{
			Allowed: false, Code: CodeSelfExcluded, PersonID: account.PersonID,
			Message: "person is currently self-excluded",
		}, nil
	}

	if params.WalletID != uuid.Nil {
		wl, err := wallet.GetByID(ctx, tx, params.WalletID)
		if err != nil {
			return Decision{}, fmt.Errorf("rg: resolve wallet: %w", err)
		}
		if wl.Status != wallet.StatusActive {
			return Decision{
				Allowed: false, Code: CodeWalletNotActive, PersonID: account.PersonID,
				Message: fmt.Sprintf("wallet status is %q, not active", wl.Status),
			}, nil
		}
	}

	return Decision{Allowed: true, Code: CodeAllowed, PersonID: account.PersonID}, nil
}
