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
func RegisterPlayer(ctx context.Context, tx pgx.Tx, brand Brand, email, passwordHash string) (PlayerAccount, error) {
	email = strings.ToLower(strings.TrimSpace(email))

	person, err := CreatePerson(ctx, tx)
	if err != nil {
		return PlayerAccount{}, err
	}

	account := PlayerAccount{
		ID: uuid.New(), TenantID: brand.TenantID, BrandID: brand.ID, PersonID: person.ID,
		Email: email, PasswordHash: passwordHash, Status: PlayerStatusPendingVerification,
	}
	_, err = tx.Exec(ctx,
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
