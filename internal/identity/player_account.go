package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
	"github.com/Diansalas/igaming-platform/internal/validation"
)

var ErrEmailTaken = errors.New("identity: an account with this email already exists for this brand")

// ErrInvalidInput covers caller-side mistakes in this package's own
// mutating functions (e.g. an actor_type/reason_code combination that
// makes no sense) - mirrors internal/jurisdiction's/internal/risk's own
// package-local ErrInvalidInput sentinel convention. Distinct from
// ErrNotFound (a row genuinely absent or out of RLS scope).
var ErrInvalidInput = errors.New("identity: invalid input")

// ErrEvidenceCollectionInactive is returned by SetPlayerAccountDeclaredResidence
// when jurisdiction_evidence_collection_active is OFF for (tenant,
// declared_residence) - mirrors kyc.ErrEvidenceCollectionInactive's own role
// and reasoning exactly: distinct from ErrInvalidInput so the HTTP layer
// maps it to 403 (a policy refusal), never 400 (a caller input error). When
// this is returned, nothing was written and no audit record exists.
var ErrEvidenceCollectionInactive = errors.New("identity: declared-residence evidence collection is not active for this tenant")

// ErrTransactionScope is returned by SetPlayerAccountDeclaredResidence when
// tx is not tenant-scoped to p.TenantID specifically, or is player-scoped
// (PHASE-B-ARCH-1 security review finding F-4). Deliberately its own
// sentinel, never wrapping ErrInvalidInput: a mis-scoped transaction is a
// SERVER-side caller bug (maps to 500), not a client input error (400) -
// conflating the two would misattribute a server defect to the caller.
var ErrTransactionScope = errors.New("identity: set player account declared residence: requires a tenant-scoped, non-player-scoped transaction for p.TenantID")

// PlayerAccountStatus is the lifecycle state of a player's relationship
// with one brand. See docs/architecture/16-privacy.md and CLAUDE.md's
// "no unnecessary personal information" instruction - this table
// intentionally carries nothing beyond what authentication and the
// brand relationship itself require.
type PlayerAccountStatus string

const (
	PlayerStatusPendingVerification PlayerAccountStatus = "pending_verification"
	PlayerStatusActive              PlayerAccountStatus = "active"
	PlayerStatusSuspended           PlayerAccountStatus = "suspended"
	PlayerStatusSelfExcluded        PlayerAccountStatus = "self_excluded"
	PlayerStatusClosed              PlayerAccountStatus = "closed"
	// PlayerStatusIdentityReviewRequired is Stage 4E's one new status
	// value (migration 0039): a registration whose platform-side identity
	// resolution (internal/identityresolution) returned UNCERTAIN, or
	// whose resolver was itself unavailable, lands here rather than
	// 'active' or silently proceeding - see docs/decisions/0027 §6/§7.
	// internal/rg.EvaluateEligibility already denies any non-'active'
	// status, so this is enforced by the EXISTING RG check with no new
	// code path (docs/decisions/0027 §8's explicit "no duplicated RG
	// logic" requirement) - a review-required account simply cannot
	// gamble until staff clears it (see SetPlayerAccountStatus's own
	// caller in internal/httpserver's identity-review handler).
	PlayerStatusIdentityReviewRequired PlayerAccountStatus = "identity_review_required"
)

// PlayerAccount is a Person's relationship with one specific Brand
// (never the same entity as Person or Tenant - see package doc).
type PlayerAccount struct {
	ID           uuid.UUID
	TenantID     uuid.UUID
	BrandID      uuid.UUID
	PersonID     uuid.UUID
	Email        string
	PasswordHash string
	Status       PlayerAccountStatus
	KYCTier      int
}

// RegisterPlayer creates a Person and a PlayerAccount together within tx.
// The caller opens tx via db.WithTenant(ctx, brand.TenantID, ...) and is
// expected to also issue the initial session and write the audit record
// in the SAME transaction (see the register handler in internal/
// httpserver), so registration, session issuance, and its audit record
// commit or roll back as one unit - never a player account that exists
// with no corresponding audit trail, or a session issued for a
// registration that didn't actually commit.
//
// Returns ErrEmailTaken if the brand already has an account with this
// email - checked via the database's own UNIQUE(brand_id, email)
// constraint (a race-free guarantee; a check-then-insert in application
// code would not be), not a SELECT-then-INSERT.
//
// Stage 4E note: this function is now ONE of three registration entry
// points internal/identityresolution.RegisterPlayerWithResolution chooses
// between based on its own resolver's outcome - this one is the
// NO_MATCH path (a brand-new, unlinked Person, exactly Stage 2's original
// behavior). See RegisterPlayerLinkedToPerson (the MATCH path) and
// RegisterPlayerPendingReview (the UNCERTAIN/resolver-unavailable path).
// No caller outside internal/identityresolution should call this
// directly anymore for a resolution-aware registration flow - it remains
// exported for any lower-level test/tooling need that genuinely wants
// Stage 2's original, resolution-blind behavior.
func RegisterPlayer(ctx context.Context, tx pgx.Tx, brand Brand, email, passwordHash string) (PlayerAccount, error) {
	person, err := CreatePerson(ctx, tx)
	if err != nil {
		return PlayerAccount{}, err
	}
	return insertPlayerAccount(ctx, tx, brand, person.ID, email, passwordHash, PlayerStatusPendingVerification)
}

// RegisterPlayerLinkedToPerson creates a PlayerAccount for an EXISTING
// Person - internal/identityresolution's MATCH path (ADR 0027 §6/§7): a
// registration whose verified identity evidence resolved to a Person who
// already has one or more accounts elsewhere. Never calls CreatePerson -
// this is precisely what closes the Stage 4D-RG P0 finding (a Person
// self-excluded via one brand keeps that SAME person_id when they
// register at another, so internal/rg.EvaluateEligibility's existing
// person_id-keyed restriction lookup sees them correctly).
//
// personID MUST already be a real, resolved Person id - this function
// does not verify it exists beyond the database's own FK constraint,
// exactly like RegisterPlayer never re-verifies a freshly-created one.
func RegisterPlayerLinkedToPerson(ctx context.Context, tx pgx.Tx, brand Brand, email, passwordHash string, personID uuid.UUID) (PlayerAccount, error) {
	if personID == uuid.Nil {
		return PlayerAccount{}, fmt.Errorf("identity: register player linked to person: person id is required")
	}
	return insertPlayerAccount(ctx, tx, brand, personID, email, passwordHash, PlayerStatusPendingVerification)
}

// RegisterPlayerPendingReview creates a NEW Person (there is no candidate
// to link to) and a PlayerAccount in the 'identity_review_required'
// status - internal/identityresolution's UNCERTAIN and resolver-
// unavailable path (ADR 0027 §6/§7). The account exists, but CANNOT log in
// again (the login handler denies any status other than active/
// pending_verification, exactly like suspended/self_excluded/closed -
// this doc comment previously and incorrectly said "the account exists
// and can log in," corrected during Stage 4E's own security review) until
// staff clears the review (internal/httpserver's identity-review admin
// handler), at which point internal/rg.EvaluateEligibility's existing
// account-status check also stops denying every gambling action on it -
// see PlayerStatusIdentityReviewRequired's own doc comment for why no new
// RG code path was needed for either check.
func RegisterPlayerPendingReview(ctx context.Context, tx pgx.Tx, brand Brand, email, passwordHash string) (PlayerAccount, error) {
	person, err := CreatePerson(ctx, tx)
	if err != nil {
		return PlayerAccount{}, err
	}
	return insertPlayerAccount(ctx, tx, brand, person.ID, email, passwordHash, PlayerStatusIdentityReviewRequired)
}

// insertPlayerAccount is the shared insert behind all three registration
// entry points above - identical row shape, differing only in personID
// (existing vs. freshly-created) and initial status.
func insertPlayerAccount(ctx context.Context, tx pgx.Tx, brand Brand, personID uuid.UUID, email, passwordHash string, status PlayerAccountStatus) (PlayerAccount, error) {
	email = strings.ToLower(strings.TrimSpace(email))

	account := PlayerAccount{
		ID: uuid.New(), TenantID: brand.TenantID, BrandID: brand.ID, PersonID: personID,
		Email: email, PasswordHash: passwordHash, Status: status,
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		account.ID, account.TenantID, account.BrandID, account.PersonID,
		account.Email, account.PasswordHash, account.Status,
	)
	if err != nil {
		if db.IsUniqueViolation(err) {
			return PlayerAccount{}, ErrEmailTaken
		}
		return PlayerAccount{}, fmt.Errorf("identity: register player: %w", err)
	}
	return account, nil
}

// GetPlayerAccountByEmail looks up a player account within brand's
// tenant. tenantID must already be resolved (from the brand) by the
// caller - this is always called via db.WithTenant.
func GetPlayerAccountByEmail(ctx context.Context, tx pgx.Tx, brandID uuid.UUID, email string) (PlayerAccount, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	var a PlayerAccount
	err := tx.QueryRow(ctx,
		`SELECT id, tenant_id, brand_id, person_id, email, password_hash, status, kyc_tier
		 FROM player_accounts WHERE brand_id = $1 AND email = $2`,
		brandID, email,
	).Scan(&a.ID, &a.TenantID, &a.BrandID, &a.PersonID, &a.Email, &a.PasswordHash, &a.Status, &a.KYCTier)
	if errors.Is(err, pgx.ErrNoRows) {
		return PlayerAccount{}, ErrNotFound
	}
	if err != nil {
		return PlayerAccount{}, fmt.Errorf("identity: get player account: %w", err)
	}
	return a, nil
}

// GetPlayerAccountByID looks up a player account by id within the
// current tenant context.
func GetPlayerAccountByID(ctx context.Context, tx pgx.Tx, id uuid.UUID) (PlayerAccount, error) {
	var a PlayerAccount
	err := tx.QueryRow(ctx,
		`SELECT id, tenant_id, brand_id, person_id, email, password_hash, status, kyc_tier
		 FROM player_accounts WHERE id = $1`,
		id,
	).Scan(&a.ID, &a.TenantID, &a.BrandID, &a.PersonID, &a.Email, &a.PasswordHash, &a.Status, &a.KYCTier)
	if errors.Is(err, pgx.ErrNoRows) {
		return PlayerAccount{}, ErrNotFound
	}
	if err != nil {
		return PlayerAccount{}, fmt.Errorf("identity: get player account by id: %w", err)
	}
	return a, nil
}

// ListPlayerAccounts returns up to limit player accounts for the current
// tenant context, ordered newest first, for the admin listing endpoint.
func ListPlayerAccounts(ctx context.Context, tx pgx.Tx, limit int) ([]PlayerAccount, error) {
	rows, err := tx.Query(ctx,
		`SELECT id, tenant_id, brand_id, person_id, email, password_hash, status, kyc_tier
		 FROM player_accounts ORDER BY id DESC LIMIT $1`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("identity: list player accounts: %w", err)
	}
	defer rows.Close()

	var accounts []PlayerAccount
	for rows.Next() {
		var a PlayerAccount
		if err := rows.Scan(&a.ID, &a.TenantID, &a.BrandID, &a.PersonID, &a.Email, &a.PasswordHash, &a.Status, &a.KYCTier); err != nil {
			return nil, fmt.Errorf("identity: scan player account: %w", err)
		}
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
}

// SetPlayerAccountStatus updates a player account's status - the hook
// behind the admin "suspend" action. Deliberately narrow (status only,
// not a general update) since it is the one mutation Stage 2 exposes.
func SetPlayerAccountStatus(ctx context.Context, tx pgx.Tx, id uuid.UUID, status PlayerAccountStatus) error {
	tag, err := tx.Exec(ctx, `UPDATE player_accounts SET status = $1, updated_at = now() WHERE id = $2`, status, id)
	if err != nil {
		return fmt.Errorf("identity: set player account status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetPlayerAccountPasswordHash overwrites a player account's password
// hash - the mutation behind Stage 4F's password-reset-confirm flow.
// Deliberately narrow (password hash only), mirroring
// SetPlayerAccountStatus's own "one field, one purpose" shape.
func SetPlayerAccountPasswordHash(ctx context.Context, tx pgx.Tx, id uuid.UUID, passwordHash string) error {
	tag, err := tx.Exec(ctx, `UPDATE player_accounts SET password_hash = $1, updated_at = now() WHERE id = $2`, passwordHash, id)
	if err != nil {
		return fmt.Errorf("identity: set player account password hash: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetPlayerAccountEmailVerified stamps verified_at (Stage 4F: this
// specific brand relationship's email was confirmed - see migration
// 0040's column comment for why this is distinct from platform-wide KYC
// identity verification, which never touches player_accounts at all).
// Idempotent - confirming an already-verified email simply re-stamps the
// time rather than erroring, since there is no harm in it and no
// meaningful "already done" error a caller needs to react to differently.
func SetPlayerAccountEmailVerified(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	tag, err := tx.Exec(ctx, `UPDATE player_accounts SET verified_at = now(), updated_at = now() WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("identity: set player account email verified: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetPlayerAccountStatusIfCurrent atomically transitions id from fromStatus
// to toStatus - the WHERE clause's own status predicate is what makes this
// safe against a concurrent status change, unlike a plain "read current
// status, then call SetPlayerAccountStatus" sequence (security specialist
// review finding, Stage 4E): a separate read-then-write has a TOCTOU
// window in which a DIFFERENT transaction (e.g. a concurrent suspend) can
// change the row between the read and the write, and the write would then
// silently clobber that intervening change. This function closes that
// window by making the CURRENT-status check and the transition one atomic
// statement - if the row's status is no longer fromStatus by the time this
// runs, RowsAffected is 0 and applied is false, and NOTHING is written.
//
// Used by the identity-review-clear admin action
// (internal/httpserver/admin_routes.go), whose entire safety argument
// depends on only ever transitioning an account that is GENUINELY still
// identity_review_required at the moment of the write, not merely at the
// moment it was read.
func SetPlayerAccountStatusIfCurrent(ctx context.Context, tx pgx.Tx, id uuid.UUID, fromStatus, toStatus PlayerAccountStatus) (applied bool, err error) {
	tag, err := tx.Exec(ctx,
		`UPDATE player_accounts SET status = $1, updated_at = now() WHERE id = $2 AND status = $3`,
		toStatus, id, fromStatus,
	)
	if err != nil {
		return false, fmt.Errorf("identity: set player account status if current: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// --- Stage 4I Phase B: declared residence (self-service, unverified) ---
//
// This section deliberately does NOT extend PlayerAccount or
// GetPlayerAccountByID's own SELECT column list - declared_residence_*
// is read ONLY through GetDeclaredResidence below, a narrow, purpose-built
// accessor, per the architect ruling for this phase.
//
// PHASE-B-ARCH-1 (activation-gate hardening): SetPlayerAccountDeclaredResidence
// itself checks jurisdiction.IsEvidenceCollectionActive, inside the same
// transaction as the write, mirroring internal/kyc.ReviewVerification's own
// gate exactly - the earlier design (the caller alone responsible for
// checking the gate first) left the gate enforced only in
// internal/httpserver, not in this exported function, which any future
// internal caller could bypass by construction. internal/jurisdiction
// depends on neither internal/identity nor internal/kyc (verified: its own
// only non-test dependency is internal/audit), so this package importing
// internal/jurisdiction creates no cycle - the accepted dependency
// direction is identity/kyc -> jurisdiction, never the reverse.

// RowQuerier is the minimal read handle GetDeclaredResidence needs -
// satisfied by pgx.Tx today. Declared locally (not imported from
// internal/jurisdiction's own identical ReadOnlyQuerier) so this package
// gains no dependency on internal/jurisdiction.
type RowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// SetDeclaredResidenceParams is SetPlayerAccountDeclaredResidence's input.
type SetDeclaredResidenceParams struct {
	PlayerAccountID uuid.UUID
	TenantID        uuid.UUID
	CountryCode     string          // validated in-function (validation.IsISO3166Alpha2) - PHASE-B-ARCH-1 F-1: this is a backstop, not a substitute for the caller's own upstream validation
	ActorType       audit.ActorType // ActorPlayer (self-service) or ActorStaff (a future correction path, not built this phase)
	ActorID         uuid.UUID
	ReasonCode      string // REQUIRED when ActorType == audit.ActorStaff; must be empty when ActorType == audit.ActorPlayer
	IPAddress       string
	UserAgent       string
	RequestID       string
}

// provenanceFromActorType maps the acting principal to the audit
// metadata's own "provenance" vocabulary. The switch above this function
// (in SetPlayerAccountDeclaredResidence) already rejects any ActorType
// other than player/staff before this is ever called, so the default
// branch below is unreachable today - it exists only so this function
// still compiles and returns something sane if that guard is ever
// loosened without updating this switch too.
func provenanceFromActorType(a audit.ActorType) string {
	switch a {
	case audit.ActorPlayer:
		return "player_self_declared"
	case audit.ActorStaff:
		return "staff_correction"
	default:
		return string(a)
	}
}

// SetPlayerAccountDeclaredResidence records a player's self-declared
// residence. Returns ErrNotFound if the player_account_id doesn't exist
// (or isn't visible under the current RLS scope), and
// ErrEvidenceCollectionInactive if jurisdiction_evidence_collection_active
// is OFF for (p.TenantID, declared_residence) - checked inside this same
// transaction, before the write, for EVERY actor type (including the
// currently-unused staff-correction branch: the gate answers a lawful-basis
// question about the DATA, not about who is writing it). tx must be
// tenant-scoped via db.Pool.WithTenant (or an equivalent connection with
// app.tenant_id set to p.TenantID and app.player_account_id unset) - a
// mis-scoped transaction returns a distinct, non-sentinel error rather than
// being silently misreported as a closed gate (see the scope assertion
// below; jurisdiction_evidence_collection_active's own RLS policies are
// invisible to a player-scoped connection, unlike player_accounts' own
// policy, so this assertion is load-bearing, not defensive boilerplate).
//
// The before/after values are computed in ONE atomic UPDATE (a CTE reading
// the prior value correlated into the same statement's RETURNING), never
// a separate SELECT-then-UPDATE, so a concurrent write cannot produce a
// stale "before" read.
func SetPlayerAccountDeclaredResidence(ctx context.Context, tx pgx.Tx, p SetDeclaredResidenceParams) (hadPrevious, changed bool, err error) {
	switch p.ActorType {
	case audit.ActorStaff:
		if strings.TrimSpace(p.ReasonCode) == "" {
			return false, false, fmt.Errorf("%w: reason_code is required when actor_type is staff", ErrInvalidInput)
		}
	case audit.ActorPlayer:
		if p.ReasonCode != "" {
			return false, false, fmt.Errorf("%w: reason_code must be empty when actor_type is player", ErrInvalidInput)
		}
	default:
		return false, false, fmt.Errorf("%w: actor_type must be player or staff", ErrInvalidInput)
	}
	// PHASE-B-ARCH-1 security review finding F-1: this domain function is
	// the backstop, not just the HTTP handler - the handler's own
	// validation is caller-side only, and a future internal caller (the
	// still-deferred staff-correction endpoint, or a bulk/import path)
	// must not be able to store an unassigned two-letter code (e.g. "ZZ")
	// that happens to satisfy the DB's shape-only CHECK constraint.
	if !validation.IsISO3166Alpha2(p.CountryCode) {
		return false, false, fmt.Errorf("%w: country_code must be a valid ISO-3166-1 alpha-2 code", ErrInvalidInput)
	}

	var scopedTenant *uuid.UUID
	var scopedPlayer *uuid.UUID
	if err := tx.QueryRow(ctx,
		`SELECT NULLIF(current_setting('app.tenant_id', true), '')::uuid,
		        NULLIF(current_setting('app.player_account_id', true), '')::uuid`,
	).Scan(&scopedTenant, &scopedPlayer); err != nil {
		return false, false, fmt.Errorf("identity: set player account declared residence: read connection scope: %w", err)
	}
	if scopedTenant == nil || *scopedTenant != p.TenantID || scopedPlayer != nil {
		return false, false, fmt.Errorf("%w (use db.Pool.WithTenant)", ErrTransactionScope)
	}

	active, err := jurisdiction.IsEvidenceCollectionActive(ctx, tx, p.TenantID, jurisdiction.EvidenceDeclaredResidence)
	if err != nil {
		return false, false, fmt.Errorf("identity: check declared-residence evidence collection active: %w", err)
	}
	if !active {
		return false, false, ErrEvidenceCollectionInactive
	}

	var capturedAt time.Time
	err = tx.QueryRow(ctx, `
		WITH prev AS (SELECT declared_residence_country AS before_value FROM player_accounts WHERE id = $1 AND tenant_id = $2)
		UPDATE player_accounts pa
		   SET declared_residence_country = $3,
		       declared_residence_captured_at = now(),
		       updated_at = now()
		  FROM prev
		 WHERE pa.id = $1 AND pa.tenant_id = $2
		RETURNING (prev.before_value IS NOT NULL) AS had_previous,
		          (prev.before_value IS DISTINCT FROM $3) AS changed,
		          pa.declared_residence_captured_at`,
		p.PlayerAccountID, p.TenantID, p.CountryCode,
	).Scan(&hadPrevious, &changed, &capturedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, ErrNotFound
	}
	if err != nil {
		return false, false, fmt.Errorf("identity: set player account declared residence: %w", err)
	}

	// CRITICAL: the country VALUE (or any hash of it - a two-letter code
	// space is trivially brute-forced) must never appear in this metadata.
	// captured_at is the database's own timestamp (from RETURNING), not a
	// separate Go-side time.Now() call, so this audit record and
	// player_accounts.declared_residence_captured_at can never skew.
	metadata := map[string]any{
		"provenance":         provenanceFromActorType(p.ActorType),
		"had_previous_value": hadPrevious,
		"value_changed":      changed,
		"captured_at":        capturedAt.UTC().Format(time.RFC3339),
	}
	if p.ActorType == audit.ActorStaff {
		metadata["reason_code"] = p.ReasonCode
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: p.TenantID, ActorType: p.ActorType, ActorID: p.ActorID,
		Action: "player.declared_residence_set", TargetType: "player_account", TargetID: p.PlayerAccountID.String(),
		Outcome: audit.OutcomeSuccess, IPAddress: p.IPAddress, UserAgent: p.UserAgent, RequestID: p.RequestID,
		Metadata: metadata,
	}); err != nil {
		return false, false, fmt.Errorf("identity: audit declared residence set: %w", err)
	}
	return hadPrevious, changed, nil
}

// GetDeclaredResidence is the resolver-facing read accessor for a player's
// self-declared residence - NOT called by anything in this phase, built
// and tested standalone for a future phase to wire in. ok=false means "no
// fact on file", structurally distinct from an empty-string country -
// never conflate the two.
//
// Explicitly distinguishes "no tenant scope at all on this connection"
// (an error - a caller mistake, never silently reported as ok=false) from
// "no row visible under this tenant's RLS scope" (ok=false, the ordinary
// cross-tenant/not-found case) - mirrors internal/jurisdiction.Resolve's
// own assertTenantScope discipline: a resolver-facing accessor that
// silently returned "no fact on file" for a caller that forgot to scope
// its own connection would be indistinguishable from a genuine absence of
// data, which is exactly the silent-wrong-answer hazard this phase's
// architect ruling calls out.
func GetDeclaredResidence(ctx context.Context, q RowQuerier, playerAccountID uuid.UUID) (country string, capturedAt time.Time, ok bool, err error) {
	var scoped *uuid.UUID
	if err := q.QueryRow(ctx,
		`SELECT NULLIF(current_setting('app.tenant_id', true), '')::uuid`).Scan(&scoped); err != nil {
		return "", time.Time{}, false, fmt.Errorf("identity: get declared residence: read tenant scope: %w", err)
	}
	if scoped == nil {
		return "", time.Time{}, false, fmt.Errorf("identity: get declared residence: transaction has no tenant scope (use db.Pool.WithTenant)")
	}

	var countryPtr *string
	var capturedAtPtr *time.Time
	err = q.QueryRow(ctx,
		`SELECT declared_residence_country, declared_residence_captured_at FROM player_accounts WHERE id = $1`,
		playerAccountID,
	).Scan(&countryPtr, &capturedAtPtr)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", time.Time{}, false, nil
	}
	if err != nil {
		return "", time.Time{}, false, fmt.Errorf("identity: get declared residence: %w", err)
	}
	if countryPtr == nil {
		return "", time.Time{}, false, nil
	}
	return *countryPtr, *capturedAtPtr, true, nil
}
