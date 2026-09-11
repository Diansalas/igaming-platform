// Login rate-limiting/lockout backing store. See migration
// 0013_create_login_attempts and docs/decisions/0013 for why this is a
// Postgres table rather than Redis/in-memory at this stage.
package identity

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	// lockoutThreshold failed attempts for the same identifier within
	// lockoutWindow trigger a lockout; the lockout itself has no separate
	// "duration" beyond the window, since it naturally lifts as old
	// failures age out of the window on the next check. Chosen as a
	// reasonable default for a foundation stage - not derived from any
	// specific compliance requirement, and expected to become
	// tenant/jurisdiction-configurable once there's a real operator
	// asking for a different threshold.
	lockoutThreshold = 5
	lockoutWindow    = 15 * time.Minute
)

// RecordLoginAttempt logs one login attempt (success or failure). Always
// called within the same tx as the login check itself succeeds/fails, so
// the record can't be lost independently of the attempt it describes.
func RecordLoginAttempt(ctx context.Context, tx pgx.Tx, tenantID *uuid.UUID, principalType, identifier, ipAddress string, succeeded bool) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO login_attempts (tenant_id, principal_type, identifier, ip_address, succeeded)
		 VALUES ($1, $2, $3, NULLIF($4, '')::inet, $5)`,
		tenantID, principalType, identifier, ipAddress, succeeded,
	)
	if err != nil {
		return fmt.Errorf("identity: record login attempt: %w", err)
	}
	return nil
}

// IsLockedOut reports whether identifier has accumulated
// lockoutThreshold or more failed attempts within lockoutWindow, with no
// intervening success - i.e. whether login should be refused regardless
// of whether the presented password is actually correct. Checking
// "regardless of correctness" matters: without it, an attacker could
// distinguish a locked account from a wrong password by timing/response
// differences elsewhere; here, a locked-out request is refused before
// any password comparison happens at all.
func IsLockedOut(ctx context.Context, tx pgx.Tx, identifier string) (bool, error) {
	windowStart := time.Now().Add(-lockoutWindow)
	var failureCount int
	err := tx.QueryRow(ctx,
		`SELECT count(*) FROM login_attempts
		 WHERE identifier = $1 AND succeeded = false
		   AND attempted_at > $2
		   AND attempted_at > COALESCE(
		       (SELECT max(attempted_at) FROM login_attempts WHERE identifier = $1 AND succeeded = true),
		       '-infinity'::timestamptz
		   )`,
		identifier, windowStart,
	).Scan(&failureCount)
	if err != nil {
		return false, fmt.Errorf("identity: check lockout: %w", err)
	}
	return failureCount >= lockoutThreshold, nil
}
