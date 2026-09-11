// Session/refresh-token management. Access tokens (JWTs) are short-lived
// and never stored; a session row holds only the SHA-256 hash of the
// long-lived refresh token, following the same never-store-the-secret
// principle as password hashing (a stolen database row can't be replayed
// as a credential). Refresh is single-use: presenting a refresh token
// rotates it to a new one and marks the session it came from as
// superseded, forming a chain. Presenting an already-superseded token is
// treated as a stolen/replayed credential and revokes the entire chain -
// this is the "compromised credential handling" the Stage 2 instructions
// ask for.
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

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/db"
)

const refreshTokenBytes = 32

var (
	// ErrSessionNotFound covers both "no such token" and "expired" -
	// deliberately not distinguished to a caller, so a client can't use
	// the error to enumerate whether a guessed token once existed.
	ErrSessionNotFound = errors.New("auth: session not found or expired")
	// ErrSessionReused signals that a refresh token which was already
	// rotated forward has been presented again - a strong signal of
	// token theft. The caller (the HTTP handler) is expected to treat
	// this as a security event: it has already revoked the full chain by
	// the time this is returned.
	ErrSessionReused = errors.New("auth: refresh token reuse detected, session chain revoked")
)

// Session mirrors one row of the sessions table, with the raw (not
// hashed) refresh token attached only at issuance/rotation time - it is
// never read back from the database.
type Session struct {
	ID            uuid.UUID
	PrincipalType PrincipalType
	PrincipalID   uuid.UUID
	TenantID      uuid.UUID // uuid.Nil for a platform-scoped principal
	RefreshToken  string    // raw token, populated only on issue/rotate
	ExpiresAt     time.Time
}

func hashRefreshToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func generateRefreshToken() (string, error) {
	b := make([]byte, refreshTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: generate refresh token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// IssueSession creates a brand-new session (not a rotation) and returns
// it with the raw refresh token set. tx must already be scoped correctly
// for tenantID (db.WithTenant for a tenant-scoped principal,
// db.WithoutTenant for a platform-scoped one) - the caller already knows
// the tenant at this point (registration/login resolve it from the
// brand/tenant slug first), unlike rotation below.
func IssueSession(ctx context.Context, tx pgx.Tx, principalType PrincipalType, principalID, tenantID uuid.UUID, ttl time.Duration, userAgent, ipAddress string) (Session, error) {
	token, err := generateRefreshToken()
	if err != nil {
		return Session{}, err
	}
	id := uuid.New()
	expiresAt := time.Now().Add(ttl)

	var tenantIDArg *uuid.UUID
	if tenantID != uuid.Nil {
		tenantIDArg = &tenantID
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO sessions (id, principal_type, principal_id, tenant_id, refresh_token_hash, user_agent, ip_address, expires_at)
		 VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), NULLIF($7, '')::inet, $8)`,
		id, principalType, principalID, tenantIDArg, hashRefreshToken(token), userAgent, ipAddress, expiresAt,
	)
	if err != nil {
		return Session{}, fmt.Errorf("auth: insert session: %w", err)
	}

	return Session{
		ID: id, PrincipalType: principalType, PrincipalID: principalID,
		TenantID: tenantID, RefreshToken: token, ExpiresAt: expiresAt,
	}, nil
}

// sessionRow is the raw, unscoped read of a session by token hash - see
// the package-level note on why SELECT on sessions has a public RLS
// policy (migration 0012): a refresh token carries no tenant hint, so
// there is no way to know which tenant scope to query with before
// reading the row that would tell us. Finding a specific row still
// requires already possessing the exact, unguessable token.
type sessionRow struct {
	id                  uuid.UUID
	principalType       PrincipalType
	principalID         uuid.UUID
	tenantID            *uuid.UUID
	expiresAt           time.Time
	revokedAt           *time.Time
	replacedBySessionID *uuid.UUID
}

func lookupSessionByToken(ctx context.Context, pool *db.Pool, rawToken string) (sessionRow, error) {
	hash := hashRefreshToken(rawToken)
	var row sessionRow
	err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT id, principal_type, principal_id, tenant_id, expires_at, revoked_at, replaced_by_session_id
			 FROM sessions WHERE refresh_token_hash = $1`,
			hash,
		).Scan(&row.id, &row.principalType, &row.principalID, &row.tenantID, &row.expiresAt, &row.revokedAt, &row.replacedBySessionID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return sessionRow{}, ErrSessionNotFound
	}
	if err != nil {
		return sessionRow{}, fmt.Errorf("auth: look up session: %w", err)
	}
	return row, nil
}

// withResolvedScope runs fn in a transaction scoped to row's tenant
// (WithTenant if the session belongs to a specific tenant, WithoutTenant
// if it's a platform-scoped principal's session) - the second phase of
// the two-phase pattern every session-mutating function below uses.
func withResolvedScope(ctx context.Context, pool *db.Pool, row sessionRow, fn db.TxFunc) error {
	if row.tenantID != nil {
		return pool.WithTenant(ctx, *row.tenantID, fn)
	}
	return pool.WithoutTenant(ctx, fn)
}

// RotateSession validates rawToken against the sessions table (a
// two-phase operation: look up the row's scope first, then mutate within
// that scope - see lookupSessionByToken/withResolvedScope), and:
//   - if it names a live, unrevoked, unreplaced, unexpired session: mints
//     a new session, links the old one to it via replaced_by_session_id,
//     and returns the new session.
//   - if it names a session that was already rotated forward
//     (replaced_by_session_id already set): revokes the ENTIRE chain
//     (this session and everything already descended from it) and
//     returns ErrSessionReused. The caller must treat this as a security
//     event - the true holder of a valid, unstolen refresh token would
//     never present an already-rotated one.
//   - otherwise (not found, expired, or already revoked): returns
//     ErrSessionNotFound.
func RotateSession(ctx context.Context, pool *db.Pool, rawToken string, ttl time.Duration, userAgent, ipAddress string) (Session, error) {
	row, err := lookupSessionByToken(ctx, pool, rawToken)
	if err != nil {
		return Session{}, err
	}

	if row.replacedBySessionID != nil {
		// Refresh-token reuse is the platform's strongest credential-
		// theft signal - it must not land only in a mutable application
		// log (Warn-level in the HTTP handler), since ADR 0013's whole
		// premise is that security-relevant history must not depend on
		// mutable logs. Recorded atomically with the chain revocation.
		if err := withResolvedScope(ctx, pool, row, func(ctx context.Context, tx pgx.Tx) error {
			if err := revokeChainFrom(ctx, tx, row.id); err != nil {
				return err
			}
			tenantID := uuid.Nil
			if row.tenantID != nil {
				tenantID = *row.tenantID
			}
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: tenantID, ActorType: audit.ActorType(row.principalType), ActorID: row.principalID,
				Action: "auth.session_reuse_detected", TargetType: "session", TargetID: row.id.String(),
				Outcome: audit.OutcomeDenied, IPAddress: ipAddress, UserAgent: userAgent,
			})
		}); err != nil {
			return Session{}, fmt.Errorf("auth: revoke reused session chain: %w", err)
		}
		return Session{}, ErrSessionReused
	}
	if row.revokedAt != nil || time.Now().After(row.expiresAt) {
		return Session{}, ErrSessionNotFound
	}

	tenantID := uuid.Nil
	if row.tenantID != nil {
		tenantID = *row.tenantID
	}

	var next Session
	err = withResolvedScope(ctx, pool, row, func(ctx context.Context, tx pgx.Tx) error {
		// Re-check under the properly-scoped transaction: the row could
		// have been revoked or rotated by a concurrent request between
		// the lookup above and this point. FOR UPDATE is load-bearing,
		// not defensive dressing: without it, two concurrent refreshes of
		// the same token both read "not yet replaced" here, both mint a
		// new session, and the second UPDATE below silently overwrites
		// the first's replaced_by_session_id - producing two live chains
		// from one token with reuse detection never triggering (a
		// confirmed race, not a theoretical one - see Stage 2 code
		// review). The row lock makes the second concurrent caller block
		// here until the first transaction commits, then observe
		// stillReplaced set and correctly return ErrSessionReused.
		var stillReplaced *uuid.UUID
		var stillRevoked *time.Time
		if err := tx.QueryRow(ctx,
			`SELECT replaced_by_session_id, revoked_at FROM sessions WHERE id = $1 FOR UPDATE`, row.id,
		).Scan(&stillReplaced, &stillRevoked); err != nil {
			return err
		}
		if stillReplaced != nil || stillRevoked != nil {
			return ErrSessionReused
		}

		var err error
		next, err = IssueSession(ctx, tx, row.principalType, row.principalID, tenantID, ttl, userAgent, ipAddress)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx,
			`UPDATE sessions SET replaced_by_session_id = $1, last_used_at = now() WHERE id = $2`,
			next.ID, row.id,
		)
		return err
	})
	if errors.Is(err, ErrSessionReused) {
		return Session{}, ErrSessionReused
	}
	if err != nil {
		return Session{}, fmt.Errorf("auth: rotate session: %w", err)
	}
	return next, nil
}

// revokeChainFrom marks the named session, and every session reachable by
// following replaced_by_session_id forward from it, as revoked. Used
// when reuse of an already-rotated token indicates the whole chain may be
// in an attacker's hands. Assumes tx is already scoped to every session
// in the chain's tenant - true here because a chain never crosses
// tenants (each rotation reuses the same principal/tenant).
func revokeChainFrom(ctx context.Context, tx pgx.Tx, startID uuid.UUID) error {
	currentID := startID
	for {
		var nextID *uuid.UUID
		err := tx.QueryRow(ctx,
			`UPDATE sessions SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL RETURNING replaced_by_session_id`,
			currentID,
		).Scan(&nextID)
		if errors.Is(err, pgx.ErrNoRows) {
			// Already revoked (or doesn't exist) - nothing more to do on
			// this branch.
			return nil
		}
		if err != nil {
			return err
		}
		if nextID == nil {
			return nil
		}
		currentID = *nextID
	}
}

// RevokeSession revokes one session by id, within an already-scoped tx
// (the caller has an authenticated, tenant-resolved context - e.g.
// DELETE /v1/me/sessions/{id}). It does not walk the chain forward,
// since logging out one device should not be treated as a compromise
// signal.
//
// ownerPrincipalID MUST be the caller's own principal id (from their
// verified token, never a path/body parameter). The sessions table's RLS
// policy only bounds this to the caller's tenant, not to "a session that
// actually belongs to the caller" - tenant isolation and per-principal
// ownership are different guarantees, and without this check a caller
// could revoke another principal's session within the same tenant simply
// by guessing/observing its id. Returns ErrSessionNotFound if
// sessionID doesn't exist, isn't owned by ownerPrincipalID, or is
// already revoked - deliberately the same error as "doesn't exist" so a
// caller can't use the response to enumerate other principals' session
// ids.
func RevokeSession(ctx context.Context, tx pgx.Tx, sessionID, ownerPrincipalID uuid.UUID) error {
	tag, err := tx.Exec(ctx,
		`UPDATE sessions SET revoked_at = now() WHERE id = $1 AND principal_id = $2 AND revoked_at IS NULL`,
		sessionID, ownerPrincipalID,
	)
	if err != nil {
		return fmt.Errorf("auth: revoke session: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSessionNotFound
	}
	return nil
}

// RevokeSessionByToken revokes the session matching rawToken - used for
// POST /v1/auth/logout, which (like refresh) is presented only a refresh
// token and may run with an already-expired access token, so it uses the
// same two-phase lookup-then-scoped-mutate pattern as RotateSession.
func RevokeSessionByToken(ctx context.Context, pool *db.Pool, rawToken string) error {
	row, err := lookupSessionByToken(ctx, pool, rawToken)
	if errors.Is(err, ErrSessionNotFound) {
		// Logging out with an already-invalid token is a no-op, not an
		// error - the end state (no live session for this token) is what
		// the caller wanted.
		return nil
	}
	if err != nil {
		return err
	}
	return withResolvedScope(ctx, pool, row, func(ctx context.Context, tx pgx.Tx) error {
		return RevokeSession(ctx, tx, row.id, row.principalID)
	})
}

// ListActiveSessions returns the caller's own live sessions (not
// revoked, not expired) - the "which devices am I logged in on" view
// behind GET /v1/me/sessions. Called within the caller's own
// already-scoped tx (they are authenticated, so their tenant scope, if
// any, is already established).
func ListActiveSessions(ctx context.Context, tx pgx.Tx, principalType PrincipalType, principalID uuid.UUID) ([]Session, error) {
	rows, err := tx.Query(ctx,
		`SELECT id, tenant_id, expires_at FROM sessions
		 WHERE principal_type = $1 AND principal_id = $2
		   AND revoked_at IS NULL AND expires_at > now()
		 ORDER BY created_at DESC`,
		principalType, principalID,
	)
	if err != nil {
		return nil, fmt.Errorf("auth: list sessions: %w", err)
	}
	defer rows.Close()

	var sessions []Session
	for rows.Next() {
		var s Session
		var tenantID *uuid.UUID
		if err := rows.Scan(&s.ID, &tenantID, &s.ExpiresAt); err != nil {
			return nil, fmt.Errorf("auth: scan session: %w", err)
		}
		if tenantID != nil {
			s.TenantID = *tenantID
		}
		s.PrincipalType = principalType
		s.PrincipalID = principalID
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}
