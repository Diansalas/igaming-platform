package casino

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/marketgate"
)

// SR8-2: exposure that is already running when an operating market closes.
//
// A casino bet arrives as a vendor server-to-server callback, so there is no
// player request and no country evidence to gate it with. Two controls bound
// NEW exposure instead (settlement of existing exposure - postWin,
// postRollback, stake returns, replays, tombstones - is NEVER touched by
// either: they resolve from the ledger's own prior entries, and refusing a
// settlement would strand player funds):
//
//  1. an absolute maximum lifetime for a CONSUMED launch session, measured from
//     the session's creation by the DATABASE clock. Past it a NEW bet is DECLINED (audited, SR8R-7)
//     and the player must pass the evidence-gated
//     launch again - this is what bounds "the player travelled after launch";
//  2. a brand-level "market still open" check on every NEW bet and on the
//     launch-token bootstrap: when the operating-market configuration currently
//     permits NO country for casino wagering at this brand, a new bet is
//     DECLINED (audited) and a not-yet-consumed token is refused and revoked.
//
// Residual (PROVIDER DEPENDENT, recorded in docs/plans/a8-market-blocking-
// record.md): a player who moves from an allowed country to an excluded one
// INSIDE a market that stays open is not detected after launch; that needs a
// per-bet vendor geolocation signal. The maximum lifetime is the bound.

// DefaultSessionMaxLifetime is the maximum age of a launch session that may
// still take NEW bets (config CASINO_SESSION_MAX_LIFETIME overrides).
const DefaultSessionMaxLifetime = 4 * time.Hour

// DeclineReasonMarketUnavailable is the provider-visible decline reason for a
// NEW bet refused because the brand's market is closed. One generic reason,
// like DeclineReasonGameUnavailable; the audit row keeps the cause.
const DeclineReasonMarketUnavailable = "market_unavailable"

// DeclineReasonSessionExpired is the provider-visible decline reason for a NEW bet
// refused because the consumed session passed its maximum lifetime.
const DeclineReasonSessionExpired = "session_expired"

// SetSessionMaxLifetime sets the maximum lifetime of a consumed session for NEW
// bets. Call at construction (not concurrency-safe against in-flight bets). A
// non-positive value keeps DefaultSessionMaxLifetime.
func (o *Orchestrator) SetSessionMaxLifetime(d time.Duration) {
	if d > 0 {
		o.sessionMaxLifetime = d
	}
}

func (o *Orchestrator) maxSessionLifetime() time.Duration {
	if o.sessionMaxLifetime > 0 {
		return o.sessionMaxLifetime
	}
	return DefaultSessionMaxLifetime
}

// WithUngatedMarketOpenForTests turns OFF the brand-level market-open check
// (control 2 above) for tests and tooling that exercise bets without market
// configuration. The default - including every production construction - is
// ENFORCED. No production code may call it
// (TestProductionCodeNeverUsesTheUngatedSentinels scans the module).
func (o *Orchestrator) WithUngatedMarketOpenForTests() *Orchestrator {
	o.marketOpenUngated = true
	return o
}

// brandMarketOpen reports whether the brand's market currently permits casino
// wagering in at least one country. An error means "could not decide": callers
// fail closed.
func (o *Orchestrator) brandMarketOpen(ctx context.Context, tx pgx.Tx, tenantID, brandID uuid.UUID) (bool, error) {
	if o.marketOpenUngated {
		return true, nil
	}
	return marketgate.BrandOpen(ctx, tx, tenantID, brandID, marketgate.OpCasinoBet)
}

// sessionPastMaxLifetime reports whether the session is older than max by the
// database clock.
func sessionPastMaxLifetime(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID, max time.Duration) (bool, error) {
	var past bool
	err := tx.QueryRow(ctx,
		`SELECT clock_timestamp() - created_at > make_interval(secs => $2) FROM casino_launch_sessions WHERE id = $1`,
		sessionID, max.Seconds()).Scan(&past)
	if err != nil {
		return false, fmt.Errorf("casino: check launch session lifetime: %w", err)
	}
	return past, nil
}
