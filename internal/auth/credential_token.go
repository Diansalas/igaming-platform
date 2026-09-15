// Stage 4F: email-verification and password-reset one-time tokens.
// Mirrors session.go's refresh-token conventions exactly - a raw,
// high-entropy random token whose SHA-256 hash (never the raw value) is
// the only thing persisted, single-use, short-lived, and looked up via
// the same two-phase "hash lookup with no tenant scope, then act within
// the resolved scope" pattern sessions established for tokens presented
// before any authentication exists.
package auth

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

	"github.com/Diansalas/igaming-platform/internal/db"
)

const credentialTokenBytes = 32

// TokenPurpose discriminates the two credential-recovery flows this
// stage implements. Never conflated with each other - a password-reset
// token must never confirm an email, and vice versa; every lookup/consume
// call is scoped to one exact purpose.
type TokenPurpose string

const (
	PurposeEmailVerification TokenPurpose = "email_verification"
	PurposePasswordReset     TokenPurpose = "password_reset"
)

// ErrCredentialTokenNotFound covers "no such token", "expired", and
// "already consumed" without distinguishing which to the caller -
// identical reasoning to ErrSessionNotFound: a client must not be able to
// use the error to enumerate which raw tokens once existed or learn
// anything about their state via timing/response differences.
var ErrCredentialTokenNotFound = errors.New("auth: credential token not found, expired, or already used")

func hashCredentialToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func generateCredentialToken() (string, error) {
	b := make([]byte, credentialTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: generate credential token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// CredentialToken is one issued token, with the raw value attached only
// at issuance - it is never read back from the database (only its hash
// is stored), and it is the caller's responsibility to hand it to the
// player exactly once (in the confirmation email) and never log it.
type CredentialToken struct {
	ID              uuid.UUID
	PlayerAccountID uuid.UUID
	TenantID        uuid.UUID
	Purpose         TokenPurpose
	RawToken        string
	ExpiresAt       time.Time
}

// IssueCredentialToken creates a new token for playerAccountID, first
// superseding (consuming, without ever having been "used") every prior
// outstanding token of the SAME purpose for that account - never more
// than one live token per purpose at a time. This bounds the replay
// surface (an old, still-valid link becomes inert the moment a new one is
// requested) and gives "resend" a safe, well-defined meaning rather than
// silently accumulating live tokens. tx must already be tenant-scoped
// (db.WithTenant) - migration 0040's tenant_isolation policy is what
// actually enforces this is the caller's own tenant's account.
func IssueCredentialToken(ctx context.Context, tx pgx.Tx, tenantID, playerAccountID uuid.UUID, purpose TokenPurpose, ttl time.Duration, requestedIP string) (CredentialToken, error) {
	if _, err := tx.Exec(ctx,
		`UPDATE player_credential_tokens SET consumed_at = now()
		 WHERE player_account_id = $1 AND purpose = $2 AND consumed_at IS NULL`,
		playerAccountID, purpose,
	); err != nil {
		return CredentialToken{}, fmt.Errorf("auth: supersede prior credential tokens: %w", err)
	}

	rawToken, err := generateCredentialToken()
	if err != nil {
		return CredentialToken{}, err
	}
	id := uuid.New()
	expiresAt := time.Now().Add(ttl)

	_, err = tx.Exec(ctx,
		`INSERT INTO player_credential_tokens (id, tenant_id, player_account_id, purpose, token_hash, expires_at, requested_ip)
		 VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, '')::inet)`,
		id, tenantID, playerAccountID, purpose, hashCredentialToken(rawToken), expiresAt, requestedIP,
	)
	if err != nil {
		return CredentialToken{}, fmt.Errorf("auth: insert credential token: %w", err)
	}

	return CredentialToken{
		ID: id, PlayerAccountID: playerAccountID, TenantID: tenantID,
		Purpose: purpose, RawToken: rawToken, ExpiresAt: expiresAt,
	}, nil
}

// CountRecentCredentialTokens reports how many tokens of purpose have
// been issued for playerAccountID within the last window - the
// rate-limiting signal a caller uses to refuse a request (e.g. "at most 3
// resends per hour") BEFORE calling IssueCredentialToken at all. Counts
// every issuance (superseded, consumed, or still-live), not just live
// ones - a caller cannot bypass the limit by exhausting/consuming tokens
// faster.
func CountRecentCredentialTokens(ctx context.Context, tx pgx.Tx, playerAccountID uuid.UUID, purpose TokenPurpose, window time.Duration) (int, error) {
	var count int
	err := tx.QueryRow(ctx,
		`SELECT count(*) FROM player_credential_tokens
		 WHERE player_account_id = $1 AND purpose = $2 AND created_at > $3`,
		playerAccountID, purpose, time.Now().Add(-window),
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("auth: count recent credential tokens: %w", err)
	}
	return count, nil
}

// credentialTokenRow is the two-phase lookup's first result - see
// lookupCredentialTokenByHash's own doc comment.
type credentialTokenRow struct {
	id              uuid.UUID
	tenantID        uuid.UUID
	playerAccountID uuid.UUID
	purpose         TokenPurpose
	expiresAt       time.Time
	consumedAt      *time.Time
}

// lookupCredentialTokenByHash resolves rawToken to its row with NO tenant
// scope assumed yet (db.WithCredentialTokenLookup, mirroring
// lookupSessionByToken exactly) - the confirm request carries no tenant
// hint, so there is nothing else to scope the query by. Returns
// ErrCredentialTokenNotFound (never distinguishing why) for a genuinely
// missing hash; expiry/consumption/purpose-mismatch are checked
// separately by the caller AFTER this returns, so every rejection reason
// funnels through the exact same error and the exact same response, never
// leaking via timing which specific reason applied.
func lookupCredentialTokenByHash(ctx context.Context, pool *db.Pool, rawToken string) (credentialTokenRow, error) {
	hash := hashCredentialToken(rawToken)
	var row credentialTokenRow
	err := pool.WithCredentialTokenLookup(ctx, hash, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT id, tenant_id, player_account_id, purpose, expires_at, consumed_at
			 FROM player_credential_tokens WHERE token_hash = $1`,
			hash,
		).Scan(&row.id, &row.tenantID, &row.playerAccountID, &row.purpose, &row.expiresAt, &row.consumedAt)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return credentialTokenRow{}, ErrCredentialTokenNotFound
	}
	if err != nil {
		return credentialTokenRow{}, fmt.Errorf("auth: look up credential token: %w", err)
	}
	return row, nil
}

// ValidateAndConsumeCredentialToken is the ONE entry point every confirm
// handler (email-verification-confirm, password-reset-confirm) uses. It
// looks rawToken up (phase 1, no tenant scope), verifies it matches
// wantPurpose, is unexpired, and unconsumed, then marks it consumed
// within the caller-supplied fn - fn runs inside a tenant-scoped
// transaction (db.WithTenant, using the tenantID this function itself
// resolved and returns) so the caller can perform its OWN side effect
// (flip player_accounts.status, update password_hash, revoke sessions,
// write audit) atomically with the token's consumption: either both
// happen or neither does, never a consumed token whose intended effect
// silently failed.
//
// Marking consumed happens FIRST inside fn's own transaction (via a
// conditional UPDATE requiring consumed_at IS NULL, closing the identical
// TOCTOU class Stage 4E's identity-review-clear handler was found to have
// - see docs/decisions/0027 - a raced double-confirm must never apply the
// side effect twice), so fn's own work only proceeds if this call
// actually won the race to consume it.
func ValidateAndConsumeCredentialToken(ctx context.Context, pool *db.Pool, rawToken string, wantPurpose TokenPurpose, fn func(ctx context.Context, tx pgx.Tx, tenantID, playerAccountID uuid.UUID) error) error {
	row, err := lookupCredentialTokenByHash(ctx, pool, rawToken)
	if err != nil {
		return err
	}
	if row.purpose != wantPurpose {
		// Not a secret-comparison timing concern (purpose was already
		// resolved via the hash-matched row, not attacker-guessed) - a
		// plain mismatch check is correct: a password-reset token
		// presented to the email-confirm endpoint (or vice versa) must be
		// rejected with the exact same generic error as "not found".
		return ErrCredentialTokenNotFound
	}
	if row.consumedAt != nil || time.Now().After(row.expiresAt) {
		return ErrCredentialTokenNotFound
	}

	return pool.WithTenant(ctx, row.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE player_credential_tokens SET consumed_at = now() WHERE id = $1 AND consumed_at IS NULL AND expires_at > now()`,
			row.id,
		)
		if err != nil {
			return fmt.Errorf("auth: consume credential token: %w", err)
		}
		if tag.RowsAffected() == 0 {
			// Lost a race (another request consumed/expired it between
			// phase 1 and here) - report the same generic error, apply no
			// side effect.
			return ErrCredentialTokenNotFound
		}
		return fn(ctx, tx, row.tenantID, row.playerAccountID)
	})
}
