package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

var ErrEmailTaken = errors.New("identity: an account with this email already exists for this brand")

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
