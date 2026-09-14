package casino

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// launchTokenBytes matches auth.refreshTokenBytes' own entropy choice -
// 256 bits of crypto/rand, base64url-encoded. Reusing the SAME generation
// mechanism as internal/auth's refresh tokens (never the same KEY
// MATERIAL, never the same TABLE) is deliberate: it is a proven, already-
// reviewed pattern for "opaque, unguessable, single-use credential,
// verified by comparing a stored hash" - ADR 0025 §3 chose this over a
// signed JWT specifically so a casino provider is never handed anything
// verifiable against the platform's own internal signing keys.
const launchTokenBytes = 32

// DefaultLaunchTokenTTL bounds how long an un-consumed launch token
// remains resolvable - short, per ADR 0025 §3 ("short TTL").
const DefaultLaunchTokenTTL = 2 * time.Minute

func generateLaunchToken() (string, error) {
	b := make([]byte, launchTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("casino: generate launch token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hashLaunchToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// LaunchSession mirrors a casino_launch_sessions row.
type LaunchSession struct {
	ID              uuid.UUID
	TenantID        uuid.UUID
	BrandID         uuid.UUID
	PlayerAccountID uuid.UUID
	WalletID        uuid.UUID
	GameID          uuid.UUID
	ProviderID      string
	ProviderGameID  string
	AssetCode       string
	Mode            GameMode
	Status          LaunchSessionStatus
	ExpiresAt       time.Time
}

// CreateLaunchSessionParams is CreateLaunchSession's input. All identity
// fields (TenantID, BrandID, PlayerAccountID, WalletID) MUST be resolved
// server-side from the authenticated player session - never from a
// request body - mirroring payment-orchestration.md §3's identical rule
// for DepositScope. GameID/ProviderID/ProviderGameID/AssetCode come from
// the already-resolved, eligibility-checked Game (ResolveLaunchEligibility),
// never re-derived from client input at this point.
type CreateLaunchSessionParams struct {
	TenantID        uuid.UUID
	BrandID         uuid.UUID
	PlayerAccountID uuid.UUID
	WalletID        uuid.UUID
	GameID          uuid.UUID
	ProviderID      string
	ProviderGameID  string
	AssetCode       string
	Mode            GameMode
	TTL             time.Duration
}

// CreateLaunchSession mints a brand-new, single-use, opaque launch
// token and its backing casino_launch_sessions row (ADR 0025 §3). Returns
// the RAW token exactly once - like auth.IssueSession, only the SHA-256
// hash is ever persisted, so the raw value cannot be recovered from the
// database even by a caller with direct SQL access.
func CreateLaunchSession(ctx context.Context, tx pgx.Tx, params CreateLaunchSessionParams) (LaunchSession, string, error) {
	if params.TenantID == uuid.Nil || params.BrandID == uuid.Nil || params.PlayerAccountID == uuid.Nil || params.WalletID == uuid.Nil || params.GameID == uuid.Nil {
		return LaunchSession{}, "", fmt.Errorf("%w: launch session requires fully-populated, server-derived identity fields", ErrInvalidInput)
	}
	if params.ProviderID == "" || params.ProviderGameID == "" || params.AssetCode == "" {
		return LaunchSession{}, "", fmt.Errorf("%w: provider_id, provider_game_id, and asset_code are required", ErrInvalidInput)
	}
	if params.Mode != ModeReal && params.Mode != ModeDemo {
		return LaunchSession{}, "", fmt.Errorf("%w: mode must be 'real' or 'demo'", ErrInvalidInput)
	}
	ttl := params.TTL
	if ttl <= 0 {
		ttl = DefaultLaunchTokenTTL
	}

	token, err := generateLaunchToken()
	if err != nil {
		return LaunchSession{}, "", err
	}
	id := uuid.New()
	expiresAt := time.Now().UTC().Add(ttl)
	_, err = tx.Exec(ctx,
		`INSERT INTO casino_launch_sessions
			(id, tenant_id, brand_id, player_account_id, wallet_id, game_id, provider_id, provider_game_id,
			 asset_code, mode, token_hash, status, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 'active', $12)`,
		id, params.TenantID, params.BrandID, params.PlayerAccountID, params.WalletID, params.GameID,
		params.ProviderID, params.ProviderGameID, params.AssetCode, params.Mode, hashLaunchToken(token), expiresAt,
	)
	if err != nil {
		return LaunchSession{}, "", fmt.Errorf("casino: create launch session: %w", err)
	}

	return LaunchSession{
		ID: id, TenantID: params.TenantID, BrandID: params.BrandID, PlayerAccountID: params.PlayerAccountID,
		WalletID: params.WalletID, GameID: params.GameID, ProviderID: params.ProviderID, ProviderGameID: params.ProviderGameID,
		AssetCode: params.AssetCode, Mode: params.Mode, Status: LaunchSessionActive, ExpiresAt: expiresAt,
	}, token, nil
}

// ResolveLaunchToken looks up the casino_launch_sessions row for a raw
// token by its hash and, if it is genuinely active and not expired,
// atomically marks it 'consumed' - single-use enforcement (ADR 0025 §3).
// Two concurrent resolution attempts for the SAME token can never both
// succeed: the UPDATE ... WHERE status = 'active' below is the atomic
// compare-and-swap, not a separate read-then-write (the same race the
// withdrawal/payments packages close via a row lock, applied here via a
// conditional UPDATE instead, since there is no multi-statement
// transition to protect beyond this single flip).
//
// tx must already be tenant-scoped - the caller resolves which tenant a
// callback/provider request belongs to BEFORE calling this (the token
// itself carries no tenant claim; RLS is what actually prevents a token
// hash collision, astronomically unlikely at 256 bits of entropy, from
// ever resolving across a tenant boundary).
func ResolveLaunchToken(ctx context.Context, tx pgx.Tx, rawToken string) (LaunchSession, error) {
	if rawToken == "" {
		return LaunchSession{}, fmt.Errorf("%w: launch token is required", ErrInvalidInput)
	}
	hash := hashLaunchToken(rawToken)

	var s LaunchSession
	var status LaunchSessionStatus
	err := tx.QueryRow(ctx,
		`SELECT id, tenant_id, brand_id, player_account_id, wallet_id, game_id, provider_id, provider_game_id,
			asset_code, mode, status, expires_at
		 FROM casino_launch_sessions WHERE token_hash = $1`,
		hash,
	).Scan(&s.ID, &s.TenantID, &s.BrandID, &s.PlayerAccountID, &s.WalletID, &s.GameID, &s.ProviderID, &s.ProviderGameID,
		&s.AssetCode, &s.Mode, &status, &s.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return LaunchSession{}, ErrLaunchSessionNotFound
	}
	if err != nil {
		return LaunchSession{}, fmt.Errorf("casino: resolve launch token: %w", err)
	}

	if status != LaunchSessionActive {
		return LaunchSession{}, ErrLaunchSessionNotActive
	}
	if time.Now().UTC().After(s.ExpiresAt) {
		// Lazily transition to 'expired' on first post-expiry access,
		// rather than requiring a background sweep - the row is already
		// unusable via the status check above regardless of whether this
		// write succeeds, so its own error is not fatal to the caller's
		// actual question ("is this token usable" - no).
		_, _ = tx.Exec(ctx, `UPDATE casino_launch_sessions SET status = 'expired' WHERE id = $1 AND status = 'active'`, s.ID)
		return LaunchSession{}, ErrLaunchSessionNotActive
	}

	tag, err := tx.Exec(ctx,
		`UPDATE casino_launch_sessions SET status = 'consumed', consumed_at = now() WHERE id = $1 AND status = 'active'`,
		s.ID,
	)
	if err != nil {
		return LaunchSession{}, fmt.Errorf("casino: consume launch session: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Lost the race to a concurrent resolution of the same token
		// between the SELECT above and this UPDATE.
		return LaunchSession{}, ErrLaunchSessionNotActive
	}

	s.Status = LaunchSessionConsumed
	return s, nil
}

// RevokeLaunchSession marks an active session unusable without consuming
// it (e.g. the launch attempt itself failed after the token was minted,
// or a staff-initiated safety revocation) - never a DELETE, since the
// row is the audit-visible record of a launch attempt having occurred at
// all.
func RevokeLaunchSession(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE casino_launch_sessions SET status = 'revoked' WHERE id = $1 AND status = 'active'`, id)
	if err != nil {
		return fmt.Errorf("casino: revoke launch session: %w", err)
	}
	return nil
}
